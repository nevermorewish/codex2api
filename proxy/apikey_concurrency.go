package proxy

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

// apiKeyConcurrencyLimiter tracks per-API-key inflight proxy requests in this
// process only. A request occupies one slot after API Key limits pass and keeps
// it until the handler returns, covering account scheduling, upstream retries,
// streaming, and WebSocket turn processing time.
type apiKeyConcurrencyLimiter struct {
	mu       sync.Mutex
	counters map[int64]*apiKeyConcurrencyCounter
}

type apiKeyConcurrencyCounter struct {
	inflight int64
}

func newAPIKeyConcurrencyLimiter() *apiKeyConcurrencyLimiter {
	return &apiKeyConcurrencyLimiter{counters: make(map[int64]*apiKeyConcurrencyCounter)}
}

func (l *apiKeyConcurrencyLimiter) acquire(apiKeyID int64, limit int) (func(), int64, bool) {
	if l == nil || apiKeyID <= 0 {
		return nil, 0, true
	}
	counter := l.counter(apiKeyID)
	limit64 := int64(limit)
	for {
		current := atomic.LoadInt64(&counter.inflight)
		if limit64 > 0 && current >= limit64 {
			return nil, current, false
		}
		if atomic.CompareAndSwapInt64(&counter.inflight, current, current+1) {
			released := atomic.Bool{}
			return func() {
				if released.CompareAndSwap(false, true) {
					l.release(counter)
				}
			}, current + 1, true
		}
	}
}

func (l *apiKeyConcurrencyLimiter) snapshot() map[int64]int64 {
	out := make(map[int64]int64)
	if l == nil {
		return out
	}
	l.mu.Lock()
	counters := make(map[int64]*apiKeyConcurrencyCounter, len(l.counters))
	for id, counter := range l.counters {
		counters[id] = counter
	}
	l.mu.Unlock()
	for id, counter := range counters {
		if counter == nil {
			continue
		}
		if inflight := atomic.LoadInt64(&counter.inflight); inflight > 0 {
			out[id] = inflight
		}
	}
	return out
}

func (l *apiKeyConcurrencyLimiter) counter(apiKeyID int64) *apiKeyConcurrencyCounter {
	l.mu.Lock()
	defer l.mu.Unlock()
	counter := l.counters[apiKeyID]
	if counter == nil {
		counter = &apiKeyConcurrencyCounter{}
		l.counters[apiKeyID] = counter
	}
	return counter
}

func (l *apiKeyConcurrencyLimiter) release(counter *apiKeyConcurrencyCounter) {
	if l == nil || counter == nil {
		return
	}
	if current := atomic.AddInt64(&counter.inflight, -1); current < 0 {
		atomic.StoreInt64(&counter.inflight, 0)
	}
}

func (h *Handler) apiKeyConcurrencyLimiter() *apiKeyConcurrencyLimiter {
	if h == nil {
		return nil
	}
	h.apiKeyGateMu.Lock()
	defer h.apiKeyGateMu.Unlock()
	if h.apiKeyGate == nil {
		h.apiKeyGate = newAPIKeyConcurrencyLimiter()
	}
	return h.apiKeyGate
}

func (h *Handler) AcquireAPIKeyConcurrency(c *gin.Context) (func(), bool) {
	return h.acquireAPIKeyConcurrency(c)
}

func (h *Handler) acquireAPIKeyConcurrency(c *gin.Context) (func(), bool) {
	if c != nil {
		if inherited, exists := c.Get(contextAPIKeyConcurrencyInherited); exists {
			if value, ok := inherited.(bool); ok && value {
				return nil, true
			}
		}
	}
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 {
		return nil, true
	}
	limiter := h.apiKeyConcurrencyLimiter()
	release, current, ok := limiter.acquire(row.ID, row.Limits.MaxConcurrency)
	if ok {
		return release, true
	}
	msg := fmt.Sprintf("API key concurrency limit exceeded: %d inflight requests (max %d)", current, row.Limits.MaxConcurrency)
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit), http.StatusTooManyRequests)
	return nil, false
}

func (h *Handler) acquireAPIKeyConcurrencyForWebSocket(c *gin.Context) (func(), *api.APIError, bool) {
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 {
		return nil, nil, true
	}
	limiter := h.apiKeyConcurrencyLimiter()
	release, current, ok := limiter.acquire(row.ID, row.Limits.MaxConcurrency)
	if ok {
		return release, nil, true
	}
	msg := fmt.Sprintf("API key concurrency limit exceeded: %d inflight requests (max %d)", current, row.Limits.MaxConcurrency)
	return nil, api.NewAPIError(api.ErrCodeRateLimitReached, msg, api.ErrorTypeRateLimit), false
}

func (h *Handler) APIKeyConcurrencySnapshot() map[int64]int64 {
	if h == nil {
		return map[int64]int64{}
	}
	return h.apiKeyConcurrencyLimiter().snapshot()
}
