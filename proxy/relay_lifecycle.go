package proxy

import (
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const relayActivityContextKey = "relayRequestActivity"

// Only retain IDs for requests that have emitted an indexed attempt and are
// still running. Completion removes them; there is no history or background
// goroutine to leak. Old retry-only logs are incomplete, not forever active.
var relayActivities = struct {
	sync.Mutex
	active map[string]int
}{active: make(map[string]int)}

type relayRequestActivity struct {
	ids      map[string]struct{}
	finished bool
}

func beginRelayRequest(c *gin.Context) func() {
	if c == nil {
		return func() {}
	}
	previous, _ := c.Get(relayActivityContextKey)
	activity := &relayRequestActivity{}
	c.Set(relayActivityContextKey, activity)
	var finishStreamRequest func(string)
	if streamID := resolveParentRequestID(c); streamID != "" {
		requestID := streamID
		if value, exists := c.Get(liveStreamRequestContextKey); exists {
			if candidate, ok := value.(string); ok && strings.TrimSpace(candidate) != "" {
				requestID = candidate
			}
		}
		finishStreamRequest = BeginLiveStreamRequest(streamID, requestID, 0)
	}
	return func() {
		if finishStreamRequest != nil {
			finishStreamRequest("")
		}
		relayActivities.Lock()
		if !activity.finished {
			activity.finished = true
			for id := range activity.ids {
				relayActivities.active[id]--
				if relayActivities.active[id] <= 0 {
					delete(relayActivities.active, id)
				}
			}
		}
		relayActivities.Unlock()
		c.Set(relayActivityContextKey, previous)
	}
}

// beginHTTPStream is the HTTP/SSE counterpart of the WebSocket connection
// observer. It is intentionally attached only after the request body has been
// parsed and stream=true is known, so ordinary JSON requests cannot pollute
// the stream list.
func beginHTTPStream(c *gin.Context, protocol, model string, isStream bool) func() {
	if c == nil || !isStream {
		return func() {}
	}
	id := resolveParentRequestID(c)
	if id == "" {
		return func() {}
	}
	finishStream := BeginLiveStream(id, protocol, model)
	finishRequest := BeginLiveStreamRequest(id, id, 1)
	var once sync.Once
	return func() {
		once.Do(func() {
			outcome, reason := "success", "request_completed"
			if c.Request != nil && c.Request.Context().Err() != nil {
				outcome, reason = "canceled", "client_canceled"
			}
			if c.Writer != nil && c.Writer.Status() >= 400 {
				outcome, reason = "failed", "request_failed"
			}
			finishRequest(outcome)
			finishStream(reason, "downstream")
		})
	}
}

func observeRelayRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.AttemptIndex <= 0 || strings.TrimSpace(input.InternalReason) != "" {
		return
	}
	id := strings.TrimSpace(input.ParentRequestID)
	if id == "" {
		return
	}
	value, _ := c.Get(relayActivityContextKey)
	activity, _ := value.(*relayRequestActivity)
	if activity == nil {
		return
	}
	relayActivities.Lock()
	defer relayActivities.Unlock()
	if activity.finished {
		return
	}
	if activity.ids == nil {
		activity.ids = make(map[string]struct{})
	}
	if _, exists := activity.ids[id]; !exists {
		activity.ids[id] = struct{}{}
		relayActivities.active[id]++
	}
}

// RelayRequestInProgress is a process-local lifecycle check, not an inference
// from the last persisted error. It covers waiting, streaming and WS turns.
func RelayRequestInProgress(requestID string) bool {
	relayActivities.Lock()
	defer relayActivities.Unlock()
	return relayActivities.active[requestID] > 0
}

// resolveParentRequestID mirrors populateInternalUsageMetaFromContext's
// resolution order (handler.go) so a live attempt registered before its
// terminal usage log is written resolves to the same correlation ID that
// log will eventually carry. Diverging here would make the live-streams
// merge in admin/live_streams.go unable to match an in-flight attempt with
// its own completed history.
func resolveParentRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get(contextParentRequestID); exists {
		if id, _ := value.(string); strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	if requestContext := api.GetRequestContext(c); requestContext != nil {
		if id := strings.TrimSpace(requestContext.RequestID); id != "" {
			return id
		}
	}
	if c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.GetHeader("X-Request-ID"))
}

// maxLiveAttempts bounds the in-progress registry so a burst of concurrent
// requests cannot grow it without limit. Exceeding it only drops realtime
// visibility for the overflow attempts; it never blocks or fails the
// underlying request.
const maxLiveAttempts = 4096

// liveAttempt is the minimal, display-only snapshot of one upstream attempt
// that is currently in flight (selected an account, about to call
// ExecuteRequest or already streaming its response).
type liveAttempt struct {
	parentRequestID string
	accountID       int64
	accountName     string
	apiKeyID        int64
	apiKeyName      string
	model           string
	isStream        bool
	useWebsocket    bool
	fallback        bool
	attemptIndex    int // 1-based
	startedAt       time.Time
}

// liveAttempts is the process-local "currently running" registry. There is
// no background goroutine and no TTL: every entry is removed by the end()
// closure returned from beginRelayAttempt, mirroring the design of
// relayActivities above. token identifies one attempt uniquely because the
// same parentRequestID can have multiple attempts registered in sequence
// (account rotation) or, briefly, in overlap (the previous attempt's end()
// racing the next attempt's begin()).
var liveAttempts = struct {
	sync.Mutex
	byToken   map[uint64]*liveAttempt
	byParent  map[string]map[uint64]struct{}
	nextToken uint64
	dropped   uint64 // attempts rejected once byToken hit maxLiveAttempts
}{byToken: make(map[uint64]*liveAttempt), byParent: make(map[string]map[uint64]struct{})}

// beginRelayAttempt registers one in-flight upstream attempt and returns a
// closure that must be called exactly once when that attempt has produced a
// result (success, terminal failure, or a decision to rotate accounts and
// retry). Call sites must call end() before starting the next attempt in
// their retry loop, except responses_ws.go's WS turn where the true end of
// the attempt is after streamResponsesWSUpstream returns, not right after
// ExecuteRequest — see the call site comment there.
//
// account may be nil defensively; a nil account never happens at the real
// call sites (they run right after account selection) but guarding here
// keeps this function safe to call without an extra nil check everywhere.
func beginRelayAttempt(c *gin.Context, account *auth.Account, model string, isStream, useWebsocket bool, attemptIndex int) func() {
	if c == nil || account == nil {
		return func() {}
	}
	parentRequestID := resolveParentRequestID(c)
	if parentRequestID == "" {
		return func() {}
	}
	accountID := account.ID()
	account.Mu().RLock()
	accountName := strings.TrimSpace(account.Name)
	account.Mu().RUnlock()

	liveAttempts.Lock()
	if len(liveAttempts.byToken) >= maxLiveAttempts {
		liveAttempts.dropped++
		liveAttempts.Unlock()
		return func() {}
	}
	liveAttempts.nextToken++
	token := liveAttempts.nextToken
	apiKeyName := ""
	if value, exists := c.Get(contextAPIKeyName); exists {
		apiKeyName, _ = value.(string)
	}
	entry := &liveAttempt{
		parentRequestID: parentRequestID,
		accountID:       accountID,
		accountName:     accountName,
		apiKeyID:        requestAPIKeyID(c),
		apiKeyName:      strings.TrimSpace(apiKeyName),
		model:           strings.TrimSpace(model),
		isStream:        isStream,
		useWebsocket:    useWebsocket,
		fallback:        account.IsExternalFallback(),
		attemptIndex:    attemptIndex,
		startedAt:       time.Now(),
	}
	liveAttempts.byToken[token] = entry
	parentSet := liveAttempts.byParent[parentRequestID]
	if parentSet == nil {
		parentSet = make(map[uint64]struct{})
		liveAttempts.byParent[parentRequestID] = parentSet
	}
	parentSet[token] = struct{}{}
	liveAttempts.Unlock()

	var ended bool
	return func() {
		liveAttempts.Lock()
		if !ended {
			ended = true
			delete(liveAttempts.byToken, token)
			if parentSet := liveAttempts.byParent[parentRequestID]; parentSet != nil {
				delete(parentSet, token)
				if len(parentSet) == 0 {
					delete(liveAttempts.byParent, parentRequestID)
				}
			}
		}
		liveAttempts.Unlock()
	}
}

// LiveAttemptInfo is the exported, copy-safe projection of liveAttempt for
// callers outside this package (admin/live_streams.go).
type LiveAttemptInfo struct {
	ParentRequestID string
	AccountID       int64
	AccountName     string
	APIKeyID        int64
	APIKeyName      string
	Model           string
	IsStream        bool
	UseWebsocket    bool
	Fallback        bool
	AttemptIndex    int
	StartedAt       time.Time
}

// LiveAttemptsSnapshot returns every currently in-flight upstream attempt.
// The result is a fresh copy; callers never observe a torn read and never
// hold a reference into this package's internal state.
func LiveAttemptsSnapshot() []LiveAttemptInfo {
	liveAttempts.Lock()
	defer liveAttempts.Unlock()
	out := make([]LiveAttemptInfo, 0, len(liveAttempts.byToken))
	for _, entry := range liveAttempts.byToken {
		out = append(out, LiveAttemptInfo{
			ParentRequestID: entry.parentRequestID,
			AccountID:       entry.accountID,
			AccountName:     entry.accountName,
			APIKeyID:        entry.apiKeyID,
			APIKeyName:      entry.apiKeyName,
			Model:           entry.model,
			IsStream:        entry.isStream,
			UseWebsocket:    entry.useWebsocket,
			Fallback:        entry.fallback,
			AttemptIndex:    entry.attemptIndex,
			StartedAt:       entry.startedAt,
		})
	}
	return out
}

// LiveAttemptsTruncated reports whether the registry has ever rejected a
// registration because it was at capacity (maxLiveAttempts). It never
// resets, which is fine: it exists only so the admin endpoint can surface a
// "some active attempts may be missing" hint once the pool has run hot.
func LiveAttemptsTruncated() bool {
	liveAttempts.Lock()
	defer liveAttempts.Unlock()
	return liveAttempts.dropped > 0
}

// RegisterLiveAttemptForTest directly registers one in-flight attempt using
// caller-supplied fields, bypassing the gin.Context/*auth.Account plumbing
// beginRelayAttempt requires. It exists solely so admin/live_streams_test.go
// (and any other package's tests) can populate LiveAttemptsSnapshot without
// assembling a full request pipeline. Production code must never call this;
// it always goes through beginRelayAttempt at the real call sites.
func RegisterLiveAttemptForTest(info LiveAttemptInfo) func() {
	if strings.TrimSpace(info.ParentRequestID) == "" {
		return func() {}
	}
	if info.StartedAt.IsZero() {
		info.StartedAt = time.Now()
	}
	liveAttempts.Lock()
	if len(liveAttempts.byToken) >= maxLiveAttempts {
		liveAttempts.dropped++
		liveAttempts.Unlock()
		return func() {}
	}
	liveAttempts.nextToken++
	token := liveAttempts.nextToken
	liveAttempts.byToken[token] = &liveAttempt{
		parentRequestID: info.ParentRequestID,
		accountID:       info.AccountID,
		accountName:     info.AccountName,
		apiKeyID:        info.APIKeyID,
		apiKeyName:      info.APIKeyName,
		model:           info.Model,
		isStream:        info.IsStream,
		useWebsocket:    info.UseWebsocket,
		fallback:        info.Fallback,
		attemptIndex:    info.AttemptIndex,
		startedAt:       info.StartedAt,
	}
	parentSet := liveAttempts.byParent[info.ParentRequestID]
	if parentSet == nil {
		parentSet = make(map[uint64]struct{})
		liveAttempts.byParent[info.ParentRequestID] = parentSet
	}
	parentSet[token] = struct{}{}
	liveAttempts.Unlock()

	var ended bool
	return func() {
		liveAttempts.Lock()
		if !ended {
			ended = true
			delete(liveAttempts.byToken, token)
			if parentSet := liveAttempts.byParent[info.ParentRequestID]; parentSet != nil {
				delete(parentSet, token)
				if len(parentSet) == 0 {
					delete(liveAttempts.byParent, info.ParentRequestID)
				}
			}
		}
		liveAttempts.Unlock()
	}
}
