package proxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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

// firstTokenTimeoutForRequest 返回本轮请求应使用的首字超时。上下文压缩轮
// （请求体含 compaction_trigger）豁免看门狗：压缩需要先读入整段历史上下文
// 才吐出首个 compaction 输出项，首帧天然可能超过用户配置的较短阈值（如 30s），
// 而这一轮的 encrypted_content 绑定原账号，超时换号重试大概率继续失败并耗尽
// attempt，导致会话彻底废掉（issue #381）。故对压缩轮返回 0（关闭看门狗），
// 让其思考时间不受限；异常挂死仍由客户端自身超时兜底。
// firstTokenTimeoutForRequest accepts an optional model string followed by request body size.
func firstTokenTimeoutForRequest(base time.Duration, isCompactionTrigger bool, args ...interface{}) time.Duration {
	if isCompactionTrigger {
		return 0
	}
	if CurrentRuntimeSettings().FirstTokenTimeoutMode == "disabled" {
		return 0
	}
	model := ""
	bodySize := 0
	for _, arg := range args { switch v := arg.(type) { case string: model = v; case int: bodySize = v } }
	if model != "" {
		if v, ok := CurrentRuntimeSettings().FirstTokenSizeTimeouts.ModelTimeouts[model]; ok { return time.Duration(v)*time.Second }
	}
	if len(args) > 0 {
		if CurrentRuntimeSettings().FirstTokenTimeoutMode == "first_token" {
			return base
		}
		size := bodySize
		settings := CurrentRuntimeSettings().FirstTokenSizeTimeouts
		switch {
		case size < 50*1024:
			return time.Duration(settings.Under50KB) * time.Second
		case size < 100*1024:
			return time.Duration(settings.Under100KB) * time.Second
		case size < 200*1024:
			return time.Duration(settings.Under200KB) * time.Second
		case size < 500*1024:
			return time.Duration(settings.Under500KB) * time.Second
		default:
			return time.Duration(settings.Over500KB) * time.Second
		}
	}
	return base
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
