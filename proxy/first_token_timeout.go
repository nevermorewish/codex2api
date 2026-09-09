package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
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

// firstTokenTimeoutForRequest 返回本轮请求应使用的首字超时。上下文压缩轮
// （请求体含 compaction_trigger）豁免看门狗：压缩需要先读入整段历史上下文
// 才吐出首个 compaction 输出项，首帧天然可能超过用户配置的较短阈值（如 30s），
// 而这一轮的 encrypted_content 绑定原账号，超时换号重试大概率继续失败并耗尽
// attempt，导致会话彻底废掉（issue #381）。故对压缩轮返回 0（关闭看门狗），
// 让其思考时间不受限；异常挂死仍由客户端自身超时兜底。
func firstTokenTimeoutForRequest(base time.Duration, isCompactionTrigger bool, bodySize ...int) time.Duration {
	if isCompactionTrigger {
		return 0
	}
	if len(bodySize) > 0 {
		size := bodySize[0]
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
