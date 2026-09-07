package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
