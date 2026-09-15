package auth

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Lifecycle state belongs to a logical request, not an idle connection.
// Prompts, API keys and upstream bodies are never retained.
const (
	LifecyclePreparing = "preparing"
	LifecycleScheduler = "scheduler_wait"
	LifecycleRetry     = "retry_wait"
	LifecycleUpstream  = "upstream"
	LifecycleFallback  = "fallback"
	LifecycleFinishing = "finishing"
)

type requestLifecycleKey struct{}
type RequestLifecycle struct {
	ID                string     `json:"id"`
	RequestID         string     `json:"request_id"`
	Endpoint          string     `json:"endpoint"`
	Model             string     `json:"model"`
	APIKeyID          int64      `json:"api_key_id"`
	State             string     `json:"state"`
	StartedAt         time.Time  `json:"started_at"`
	StateStartedAt    time.Time  `json:"state_started_at"`
	AttemptStartedAt  *time.Time `json:"attempt_started_at,omitempty"`
	Attempt           int        `json:"attempt"`
	Retries           int        `json:"retries"`
	AccountID         int64      `json:"account_id"`
	Fallback          bool       `json:"fallback"`
	FirstTokenMS      *int64     `json:"first_token_ms"`
	FirstTokenWaitMS  int64      `json:"first_token_wait_ms"`
	WaitingFirstToken bool       `json:"waiting_first_token"`
	ElapsedMS         int64      `json:"elapsed_ms"`
	StateElapsedMS    int64      `json:"state_elapsed_ms"`
	SchedulerWaitMS   int64      `json:"scheduler_wait_ms"`
	RetryWaitMS       int64      `json:"retry_wait_ms"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	StatusCode        int        `json:"status_code,omitempty"`
}
type LifecycleSnapshot struct {
	Preparing, SchedulerWaiters, RetryWaiters, UpstreamActive, FallbackActive, Finishing, TotalInflight int64
	WaitStarted, RetryStarted                                                                           uint64
	Requests, Recent                                                                                    []RequestLifecycle
	Truncated                                                                                           bool
}
type requestLifecycleEntry struct {
	row      RequestLifecycle
	revision uint64
	finished bool
}
type lifecycleTracker struct {
	mu                        sync.Mutex
	next                      atomic.Uint64
	items                     map[string]*requestLifecycleEntry
	recent                    []RequestLifecycle
	waitStarted, retryStarted uint64
}

var requestTracker = &lifecycleTracker{items: make(map[string]*requestLifecycleEntry)}

func HasRequestLifecycle(ctx context.Context) bool { return requestEntry(ctx) != nil }
func requestEntry(ctx context.Context) *requestLifecycleEntry {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(requestLifecycleKey{}).(*requestLifecycleEntry)
	return e
}
func BeginRequest(ctx context.Context, requestID, endpoint string, keyID int64) (context.Context, func(int)) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now().UTC()
	e := &requestLifecycleEntry{row: RequestLifecycle{ID: strconv.FormatUint(requestTracker.next.Add(1), 10), RequestID: requestID, Endpoint: endpoint, APIKeyID: keyID, State: LifecyclePreparing, StartedAt: now, StateStartedAt: now}}
	requestTracker.mu.Lock()
	requestTracker.items[e.row.ID] = e
	requestTracker.mu.Unlock()
	return context.WithValue(ctx, requestLifecycleKey{}, e), func(status int) {
		requestTracker.mu.Lock()
		defer requestTracker.mu.Unlock()
		if e.finished {
			return
		}
		now := time.Now().UTC()
		accrueLifecycle(e, now)
		e.finished = true
		r := projectLifecycle(e, now)
		if status == 0 {
			status = r.StatusCode
		}
		r.StatusCode = status
		r.EndedAt = &now
		r.WaitingFirstToken = false
		r.State = "completed"
		if status >= 400 {
			r.State = "failed"
		}
		if status == 499 {
			r.State = "canceled"
		}
		delete(requestTracker.items, r.ID)
		requestTracker.recent = append(requestTracker.recent, r)
		if len(requestTracker.recent) > 100 {
			requestTracker.recent = append([]RequestLifecycle(nil), requestTracker.recent[len(requestTracker.recent)-100:]...)
		}
	}
}
func accrueLifecycle(e *requestLifecycleEntry, now time.Time) {
	ms := now.Sub(e.row.StateStartedAt).Milliseconds()
	switch e.row.State {
	case LifecycleScheduler:
		e.row.SchedulerWaitMS += ms
	case LifecycleRetry:
		e.row.RetryWaitMS += ms
	}
	e.row.StateStartedAt = now
}
func setLifecycleState(e *requestLifecycleEntry, state string) {
	if e.row.State == state {
		return
	}
	accrueLifecycle(e, time.Now().UTC())
	e.row.State = state
	e.revision++
	if state == LifecycleScheduler {
		requestTracker.waitStarted++
	}
	if state == LifecycleRetry {
		requestTracker.retryStarted++
	}
}

// Restore only our own transition; never overwrite a newer attempt or revive a completed request.
func WaitRequest(ctx context.Context, state string) func() {
	e := requestEntry(ctx)
	if e == nil {
		return func() {}
	}
	requestTracker.mu.Lock()
	if e.finished {
		requestTracker.mu.Unlock()
		return func() {}
	}
	previous := e.row.State
	if previous == state {
		requestTracker.mu.Unlock()
		return func() {}
	}
	setLifecycleState(e, state)
	revision := e.revision
	requestTracker.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			requestTracker.mu.Lock()
			defer requestTracker.mu.Unlock()
			if !e.finished && e.revision == revision {
				setLifecycleState(e, previous)
			}
		})
	}
}
func RequestModel(ctx context.Context, model string) {
	e := requestEntry(ctx)
	if e == nil {
		return
	}
	requestTracker.mu.Lock()
	defer requestTracker.mu.Unlock()
	if !e.finished && model != "" {
		e.row.Model = model
	}
}

func RequestIdentity(ctx context.Context, requestID string, keyID int64) {
	e := requestEntry(ctx)
	if e == nil {
		return
	}
	requestTracker.mu.Lock()
	defer requestTracker.mu.Unlock()
	if !e.finished {
		if requestID != "" {
			e.row.RequestID = requestID
		}
		if keyID > 0 {
			e.row.APIKeyID = keyID
		}
	}
}
func StartRequestAttempt(ctx context.Context, accountID int64, fallback bool, attempt int, model string) {
	e := requestEntry(ctx)
	if e == nil {
		return
	}
	requestTracker.mu.Lock()
	defer requestTracker.mu.Unlock()
	if e.finished {
		return
	}
	now := time.Now().UTC()
	state := LifecycleUpstream
	if fallback {
		state = LifecycleFallback
	}
	setLifecycleState(e, state)
	e.revision++
	e.row.StateStartedAt = now
	e.row.Attempt = max(attempt, 1)
	e.row.Retries = max(e.row.Attempt-1, 0)
	e.row.AccountID = accountID
	e.row.Fallback = fallback
	e.row.AttemptStartedAt = &now
	e.row.FirstTokenMS = nil
	e.row.FirstTokenWaitMS = 0
	if model != "" {
		e.row.Model = model
	}
}
func RequestFirstToken(ctx context.Context, ms int64) {
	e := requestEntry(ctx)
	if e == nil {
		return
	}
	requestTracker.mu.Lock()
	defer requestTracker.mu.Unlock()
	if !e.finished && e.row.AttemptStartedAt != nil && e.row.FirstTokenMS == nil {
		value := max(ms, 0)
		e.row.FirstTokenMS = &value
	}
}
func EndRequestAttempt(ctx context.Context, status int) {
	e := requestEntry(ctx)
	if e == nil {
		return
	}
	requestTracker.mu.Lock()
	defer requestTracker.mu.Unlock()
	if e.finished {
		return
	}
	if e.row.AttemptStartedAt != nil {
		e.row.FirstTokenWaitMS = time.Since(*e.row.AttemptStartedAt).Milliseconds()
	}
	e.row.StatusCode = status
	setLifecycleState(e, LifecycleFinishing)
}
func projectLifecycle(e *requestLifecycleEntry, now time.Time) RequestLifecycle {
	r := e.row
	r.ElapsedMS = now.Sub(r.StartedAt).Milliseconds()
	r.StateElapsedMS = now.Sub(r.StateStartedAt).Milliseconds()
	switch r.State {
	case LifecycleScheduler:
		r.SchedulerWaitMS += r.StateElapsedMS
	case LifecycleRetry:
		r.RetryWaitMS += r.StateElapsedMS
	}
	r.WaitingFirstToken = (r.State == LifecycleUpstream || r.State == LifecycleFallback) && r.FirstTokenMS == nil && r.AttemptStartedAt != nil
	if r.WaitingFirstToken {
		r.FirstTokenWaitMS = now.Sub(*r.AttemptStartedAt).Milliseconds()
	}
	if r.FirstTokenMS != nil {
		v := *r.FirstTokenMS
		r.FirstTokenMS = &v
		r.FirstTokenWaitMS = v
	}
	if r.AttemptStartedAt != nil {
		v := *r.AttemptStartedAt
		r.AttemptStartedAt = &v
	}
	return r
}
func SnapshotLifecycle() LifecycleSnapshot {
	requestTracker.mu.Lock()
	now := time.Now().UTC()
	s := LifecycleSnapshot{TotalInflight: int64(len(requestTracker.items)), WaitStarted: requestTracker.waitStarted, RetryStarted: requestTracker.retryStarted, Requests: make([]RequestLifecycle, 0, len(requestTracker.items)), Recent: append([]RequestLifecycle{}, requestTracker.recent...)}
	for _, e := range requestTracker.items {
		r := projectLifecycle(e, now)
		s.Requests = append(s.Requests, r)
		switch r.State {
		case LifecyclePreparing:
			s.Preparing++
		case LifecycleScheduler:
			s.SchedulerWaiters++
		case LifecycleRetry:
			s.RetryWaiters++
		case LifecycleUpstream:
			s.UpstreamActive++
		case LifecycleFallback:
			s.FallbackActive++
		case LifecycleFinishing:
			s.Finishing++
		}
	}
	requestTracker.mu.Unlock()
	sort.Slice(s.Requests, func(i, j int) bool { return s.Requests[i].StartedAt.Before(s.Requests[j].StartedAt) })
	if len(s.Requests) > 200 {
		s.Truncated = true
		s.Requests = s.Requests[:200]
	}
	sort.Slice(s.Recent, func(i, j int) bool { return s.Recent[i].StartedAt.After(s.Recent[j].StartedAt) })
	return s
}
