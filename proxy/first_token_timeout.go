package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type firstTokenTimeoutGuard struct {
	timeout    time.Duration
	cancel     context.CancelFunc
	timer      *time.Timer
	fired      atomic.Bool
	once       sync.Once
	onProgress func()
	onStop     func()
}

func newFirstTokenTimeoutGuard(timeout time.Duration, cancel context.CancelFunc) *firstTokenTimeoutGuard {
	return newFirstTokenTimeoutGuardWithHooks(timeout, cancel, nil, nil)
}

// newFirstTokenTimeoutGuardWithHooks is the lifecycle-aware variant used by
// auxiliary monitors (such as Feishu first-token alerts). The existing
// constructor intentionally remains unchanged for callers that only need the
// upstream cancellation guard.
func newFirstTokenTimeoutGuardWithHooks(timeout time.Duration, cancel context.CancelFunc, onProgress, onStop func()) *firstTokenTimeoutGuard {
	if cancel == nil || (timeout <= 0 && onProgress == nil && onStop == nil) {
		return nil
	}
	guard := &firstTokenTimeoutGuard{
		timeout:    timeout,
		cancel:     cancel,
		onProgress: onProgress,
		onStop:     onStop,
	}
	if timeout > 0 {
		guard.timer = time.AfterFunc(timeout, func() {
			guard.fired.Store(true)
			cancel()
		})
	}
	return guard
}

func (g *firstTokenTimeoutGuard) Stop() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		if g.timer != nil {
			g.timer.Stop()
		}
		if g.onStop != nil {
			g.onStop()
		}
	})
}

func (g *firstTokenTimeoutGuard) MarkEvent(eventType string) {
	if g == nil || !isFirstTokenEvent(eventType) {
		return
	}
	if g.onProgress != nil {
		g.onProgress()
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkPayload(data []byte) {
	if g == nil || !isFirstTokenPayload(data) {
		return
	}
	if g.onProgress != nil {
		g.onProgress()
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) MarkFirstToken() {
	if g == nil {
		return
	}
	if g.onProgress != nil {
		g.onProgress()
	}
	g.Stop()
}

func (g *firstTokenTimeoutGuard) TimedOut() bool {
	return g != nil && g.fired.Load()
}

func firstTokenTimeoutOutcome(timeout time.Duration) streamOutcome {
	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "timeout",
		failureMessage: fmt.Sprintf("上游首字超时：%s 内未收到首个内容", timeout.Round(time.Millisecond)),
		penalize:       true,
	}
}

func firstTokenTimeoutError(timeout time.Duration) error {
	return ErrUpstreamTimeout(fmt.Errorf("first token timeout after %s", timeout.Round(time.Millisecond)))
}

// recordStreamOutcomeAsLastFailure 把一次「首内容前流式失败」记成请求级最终失败，
// 供账号池候选耗尽时的收尾分支透传。
//
// lastStatusCode/lastBody 过去只在拿到非 200 的 HTTP 响应时赋值，而流式失败走的是
// 200 + error/response.failed 帧，两个变量一直是零值。候选耗尽时收尾分支因此跳过
// 「透传真实上游错误」的那条路，把「上游 500 Bad Gateway」这类具体原因换成含糊的
// 「无可用账号，请稍后重试」503。重复覆盖是安全的：留下的就是最后一次失败。
func recordStreamOutcomeAsLastFailure(lastStatusCode *int, lastBody *[]byte, outcome streamOutcome) {
	if lastStatusCode == nil || lastBody == nil {
		return
	}
	message := outcome.failureMessage
	if message == "" {
		message = "Upstream stream failed before delivering any content"
	}
	status := outcome.logStatusCode
	if status < 400 || status > 599 || status == logStatusUpstreamStreamBreak || status == logStatusClientClosed {
		status = http.StatusBadGateway
	}
	code := ErrorCodeUpstreamStreamBreak
	if outcome.failureKind == "timeout" {
		code = ErrorCodeUpstreamTimeout
	}
	payload, err := json.Marshal(gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code},
	})
	if err != nil {
		return
	}
	*lastStatusCode = status
	*lastBody = payload
}

// writeExhaustedFirstTokenTimeout 在首字超时预算耗尽、请求不再续跑时写出终态。
//
// 重试等待返回 false 有两种来源：重试预算确实耗尽，或请求上下文已被取消。
// 前者此前是静默 return——不写响应体、不认领终态，下游只拿到一个空 body 的 503，
// 真实原因（兜底账号首字超时）被外层网关的「无可用账号，请稍后重试」盖掉。后者
// 已经不需要新响应（连接已断，或保活/取消路径写过终态），交回既有写出口按上下文
// 状态自行跳过。已提交的 SSE 流必须继续写协议帧，不能追加一段 JSON。
func writeExhaustedFirstTokenTimeout(c *gin.Context, err error, isStream bool, protocol continuousRetryHTTPProtocol) bool {
	if c == nil || err == nil {
		return false
	}
	message := firstTokenTimeoutClientMessage(err)
	if c.Request != nil && c.Request.Context().Err() != nil {
		if protocol == continuousRetryProtocolResponses {
			return writeCommittedResponsesRetryError(c, message)
		}
		return writeCommittedChatRetryError(c, message)
	}
	code := ErrorCodeUpstreamTimeout
	var structured *Error
	if errors.As(err, &structured) && structured != nil && structured.Code != "" {
		code = structured.Code
	}
	if isStream && retryKeepaliveCommitted(c) {
		if protocol == continuousRetryProtocolResponses {
			return writeCommittedResponsesRetryError(c, message)
		}
		return writeCommittedChatRetryError(c, message)
	}
	if !claimContinuousRetryTerminal(c, protocol) {
		return true
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(clientFacingHTTPStatus(logStatusUpstreamStreamBreak), gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code},
	})
	return true
}

// writeExhaustedStreamOutcomeTerminal 与 writeExhaustedFirstTokenTimeout 同义，
// 但输入是已经归一化过的流结果：failureMessage 才是下游该看到的原因，
// 无终态可写时才退回到兜底文案。
func writeExhaustedStreamOutcomeTerminal(c *gin.Context, outcome streamOutcome, isStream bool, protocol continuousRetryHTTPProtocol) bool {
	if c == nil {
		return false
	}
	message := outcome.failureMessage
	if message == "" {
		message = "Upstream stream failed before delivering any content"
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		if protocol == continuousRetryProtocolResponses {
			return writeCommittedResponsesRetryError(c, message)
		}
		return writeCommittedChatRetryError(c, message)
	}
	code := ErrorCodeUpstreamTimeout
	if outcome.failureKind != "timeout" && outcome.logStatusCode >= 400 && outcome.logStatusCode <= 599 {
		code = ErrorCodeUpstreamStreamBreak
	}
	if isStream && retryKeepaliveCommitted(c) {
		if protocol == continuousRetryProtocolResponses {
			return writeCommittedResponsesRetryError(c, message)
		}
		return writeCommittedChatRetryError(c, message)
	}
	if !claimContinuousRetryTerminal(c, protocol) {
		return true
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(clientFacingHTTPStatus(outcome.logStatusCode), gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code},
	})
	return true
}

// firstTokenTimeoutClientMessage 保留下游可读的失败原因：结构化错误用其 Message，
// 并附上 Cause 里的具体超时事实（如 "first token timeout after 20s"），否则只回
// 「Upstream request timeout」会让排查重新落回猜测。
func firstTokenTimeoutClientMessage(err error) string {
	if err == nil {
		return ""
	}
	var structured *Error
	if !errors.As(err, &structured) || structured == nil {
		return err.Error()
	}
	if structured.Cause == nil {
		return structured.Message
	}
	if structured.Message == "" {
		return structured.Cause.Error()
	}
	return structured.Message + ": " + structured.Cause.Error()
}

// firstTokenTimeoutInput 是本轮用于选择首字超时的请求特征。
// Model 是模型查找键（任意大小写/空白，内部统一规范化），BodySize 是
// 上层实际发给上游的请求体字节数。
//
// 用结构体而不是变参：此前 `args ...interface{}` 按类型收窄入参，位置无关、
// 数量不定，漏传一个参数就会静默改变模式判定（len(args) 参与分支），
// 且 size 的口径在三个入口各不相同。结构体让"忘了传"变成编译错误。
type firstTokenTimeoutInput struct {
	Model    string
	BodySize int
}

// firstTokenTimeoutForRequest 返回本轮请求应使用的首字超时。优先级：
// 压缩轮豁免 > 全局关闭 > 模型逐档覆盖 > 全局档位表（request_size）/ 固定值（first_token）。
//
// 上下文压缩轮（请求体含 compaction_trigger）豁免看门狗：压缩需要先读入整段
// 历史上下文才吐出首个 compaction 输出项，首帧天然可能超过用户配置的较短阈值
// （如 30s），而这一轮的 encrypted_content 绑定原账号，超时换号重试大概率继续
// 失败并耗尽 attempt，导致会话彻底废掉（issue #381）。故对压缩轮返回 0
// （关闭看门狗），让其思考时间不受限；异常挂死仍由客户端自身超时兜底。
//
// 模型覆盖先于模式判定：模型行是逐档配置（每个模型各有一套按体积划分的超时），
// 命中即为该模型的完整策略。此前模型查询被夹在 request_size 模式内部，导致
// 在默认模式下（线上为 request_size）模型专属超时永远轮不到，被全局档位表覆盖。
func firstTokenTimeoutForRequest(base time.Duration, isCompactionTrigger bool, input firstTokenTimeoutInput) time.Duration {
	if isCompactionTrigger {
		return 0
	}
	settings := CurrentRuntimeSettings()
	if settings.FirstTokenTimeoutMode == "disabled" {
		return 0
	}
	if timeout, ok := firstTokenModelTimeout(settings.FirstTokenSizeTimeouts, input.Model, input.BodySize); ok {
		return timeout
	}
	if settings.FirstTokenTimeoutMode == "first_token" {
		return base
	}
	return firstTokenSizeTimeout(settings.FirstTokenSizeTimeouts, input.BodySize)
}

// firstTokenModelTimeout 在模型逐档表中查找命中档位的超时值。档位由
// BodySize 决定，与全局档位表共用同一张阈值表（database.FirstTokenSizeBracketAt），
// 避免阈值在两处各写一份而漂移。
func firstTokenModelTimeout(settings database.FirstTokenTimeoutSettings, model string, bodySize int) (time.Duration, bool) {
	key := database.NormalizeFirstTokenModelKey(model)
	if key == "" {
		return 0, false
	}
	row, ok := settings.ModelTimeouts[key]
	if !ok {
		return 0, false
	}
	seconds, ok := row[database.FirstTokenSizeBracketAt(bodySize)]
	if !ok || seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// firstTokenSizeTimeout 返回全局档位表中该体积对应的超时。
func firstTokenSizeTimeout(settings database.FirstTokenTimeoutSettings, bodySize int) time.Duration {
	switch database.FirstTokenSizeBracketAt(bodySize) {
	case database.FirstTokenSizeUnder50KB:
		return time.Duration(settings.Under50KB) * time.Second
	case database.FirstTokenSizeUnder100KB:
		return time.Duration(settings.Under100KB) * time.Second
	case database.FirstTokenSizeUnder200KB:
		return time.Duration(settings.Under200KB) * time.Second
	case database.FirstTokenSizeUnder500KB:
		return time.Duration(settings.Under500KB) * time.Second
	default:
		return time.Duration(settings.Over500KB) * time.Second
	}
}

// firstTokenTimeoutAfterTransport subtracts time already spent in a failed
// WebSocket attempt so HTTP fallback shares the same first-token budget.
func firstTokenTimeoutAfterTransport(timeout, transportElapsed time.Duration) time.Duration {
	if timeout <= 0 || transportElapsed <= 0 {
		return timeout
	}
	if remaining := timeout - transportElapsed; remaining > 0 {
		return remaining
	}
	return time.Millisecond
}
