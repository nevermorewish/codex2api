package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRelayRequestLifecycleIsolationAndCleanup(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	end := beginRelayRequest(c)
	input := &database.UsageLogInput{ParentRequestID: "relay-lifecycle-test", AttemptIndex: 1, IsRetryAttempt: true}
	observeRelayRequest(c, input)
	observeRelayRequest(c, input)
	if !RelayRequestInProgress(input.ParentRequestID) {
		t.Fatal("retrying request was not tracked")
	}
	// Another request with the same correlation ID must retain its own lease.
	other, _ := gin.CreateTestContext(httptest.NewRecorder())
	endOther := beginRelayRequest(other)
	observeRelayRequest(other, input)
	copyOfContext := c.Copy()
	end()
	end()
	if !RelayRequestInProgress(input.ParentRequestID) {
		t.Fatal("one request removed another request's activity")
	}
	endOther()
	observeRelayRequest(copyOfContext, input)
	if RelayRequestInProgress(input.ParentRequestID) {
		t.Fatal("completed request leaked or late audit resurrected it")
	}
	// Reusing a Gin context for the next WS turn must not revive the old ID.
	end = beginRelayRequest(c)
	defer end()
	input.ParentRequestID = "relay-next-turn"
	input.InternalReason = "overflow_compact"
	observeRelayRequest(c, input)
	input.InternalReason, input.AttemptIndex = "", 0
	observeRelayRequest(c, input)
	if RelayRequestInProgress(input.ParentRequestID) {
		t.Fatal("auxiliary rounds were tracked as outer requests")
	}
	input.AttemptIndex = 1
	observeRelayRequest(c, input)
	if !RelayRequestInProgress(input.ParentRequestID) || RelayRequestInProgress("relay-lifecycle-test") {
		t.Fatal("turn lifecycle was not isolated")
	}
}

func TestRelayRequestRemainsActiveWhileFallbackIsPending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"message":"primary unavailable"}}`)
	}))
	defer primary.Close()
	started, release := make(chan struct{}), make(chan struct{})
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+wsFallbackSuccess+"\n\n")
	}))
	defer fallback.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relay-pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 2})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL, APIKey: "primary", Models: []string{"gpt-5.4"}, PlanType: "api"})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "backup", BaseURL: fallback.URL, APIKey: "backup", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	h := NewHandler(store, db, nil, nil)
	h.SetFallbackPool(pool)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	c.Request.Header.Set("X-Request-ID", "relay-pending-test")
	done := make(chan struct{})
	go func() { defer close(done); h.Responses(c) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback did not start")
	}
	db.FlushUsageLogs()
	logs, _, err := db.ListRelayChainLogs(context.Background(), 1, 20)
	if err != nil || len(logs) != 1 || !logs[0].IsRetryAttempt || !RelayRequestInProgress(logs[0].ParentRequestID) {
		t.Fatalf("pending state not visible: logs=%+v err=%v", logs, err)
	}
	requestID := logs[0].ParentRequestID
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	if RelayRequestInProgress(requestID) {
		t.Fatal("request still active after fallback finished")
	}
	db.FlushUsageLogs()
	logs, _, err = db.ListRelayChainLogs(context.Background(), 1, 20)
	if err != nil || len(logs) != 2 || logs[1].AccountID != -9 || logs[1].StatusCode != 200 || logs[1].IsRetryAttempt {
		t.Fatalf("missing terminal fallback: logs=%+v err=%v", logs, err)
	}
}

func newLiveAttemptTestContext(t *testing.T, requestID string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{}`))
	if requestID != "" {
		c.Request.Header.Set("X-Request-ID", requestID)
	}
	return c
}

func TestBeginRelayAttemptTracksAndCleansUp(t *testing.T) {
	c := newLiveAttemptTestContext(t, "live-attempt-basic")
	account := &auth.Account{DBID: 5, Name: "acct-a"}
	end := beginRelayAttempt(c, account, "gpt-5.4", true, false, 1)
	snapshot := LiveAttemptsSnapshot()
	var found *LiveAttemptInfo
	for i := range snapshot {
		if snapshot[i].ParentRequestID == "live-attempt-basic" {
			found = &snapshot[i]
		}
	}
	if found == nil {
		t.Fatal("in-flight attempt not visible in snapshot")
	}
	if found.AccountID != 5 || found.AccountName != "acct-a" || found.Model != "gpt-5.4" || !found.IsStream || found.UseWebsocket || found.AttemptIndex != 1 {
		t.Fatalf("unexpected snapshot entry: %+v", found)
	}
	if found.Fallback {
		t.Fatal("primary account misreported as fallback")
	}
	end()
	for _, entry := range LiveAttemptsSnapshot() {
		if entry.ParentRequestID == "live-attempt-basic" {
			t.Fatal("entry still present after end()")
		}
	}
	// end() must be safe to call more than once.
	end()
}

func TestBeginRelayAttemptMultipleAttemptsSameParent(t *testing.T) {
	c := newLiveAttemptTestContext(t, "live-attempt-rotation")
	first := &auth.Account{DBID: 1, Name: "acct-1"}
	second := &auth.Account{DBID: 2, Name: "acct-2"}
	endFirst := beginRelayAttempt(c, first, "gpt-5.4", true, false, 1)
	endSecond := beginRelayAttempt(c, second, "gpt-5.4", true, false, 2)

	count := func() int {
		n := 0
		for _, entry := range LiveAttemptsSnapshot() {
			if entry.ParentRequestID == "live-attempt-rotation" {
				n++
			}
		}
		return n
	}
	if count() != 2 {
		t.Fatalf("expected 2 in-flight attempts for the same parent, got %d", count())
	}
	endFirst()
	if count() != 1 {
		t.Fatalf("ending the first attempt must not affect the second, got %d", count())
	}
	endSecond()
	if count() != 0 {
		t.Fatalf("expected 0 in-flight attempts after both ended, got %d", count())
	}
}

func TestLiveAttemptsCapEnforced(t *testing.T) {
	liveAttempts.Lock()
	liveAttempts.byToken = make(map[uint64]*liveAttempt)
	liveAttempts.byParent = make(map[string]map[uint64]struct{})
	liveAttempts.dropped = 0
	liveAttempts.Unlock()

	account := &auth.Account{DBID: 1, Name: "acct-cap"}
	var ends []func()
	for i := 0; i < maxLiveAttempts+5; i++ {
		c := newLiveAttemptTestContext(t, "live-attempt-cap")
		ends = append(ends, beginRelayAttempt(c, account, "gpt-5.4", true, false, i+1))
	}
	if len(liveAttempts.byToken) != maxLiveAttempts {
		t.Fatalf("registry exceeded cap: len=%d cap=%d", len(liveAttempts.byToken), maxLiveAttempts)
	}
	if !LiveAttemptsTruncated() {
		t.Fatal("expected truncation to be reported once the cap was hit")
	}
	for _, end := range ends {
		end()
	}
	if len(liveAttempts.byToken) != 0 {
		t.Fatalf("registry did not drain after all ends: len=%d", len(liveAttempts.byToken))
	}
}

func TestBeginRelayAttemptConcurrentSafety(t *testing.T) {
	account := &auth.Account{DBID: 1, Name: "acct-race"}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := newLiveAttemptTestContext(t, "live-attempt-race")
			end := beginRelayAttempt(c, account, "gpt-5.4", true, false, n+1)
			_ = LiveAttemptsSnapshot()
			end()
		}(i)
	}
	wg.Wait()
	for _, entry := range LiveAttemptsSnapshot() {
		if entry.ParentRequestID == "live-attempt-race" {
			t.Fatal("registry did not fully drain after concurrent begin/end")
		}
	}
}
