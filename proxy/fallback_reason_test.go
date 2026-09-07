package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestFallbackReasonAttributionPersistsOnlyForFallback(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fallback-attribution.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	store := newFallbackQueueTestStore()
	defer store.Stop()
	h := NewHandler(store, db, nil, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextFallbackReason, fallbackReasonQueueThreshold)
	for _, id := range []int64{1, -9} {
		h.logUsageForRequest(c, &database.UsageLogInput{AccountID: id, StatusCode: 503, AttemptIndex: 1})
	}
	db.FlushUsageLogs()
	logs, _, err := db.ListRelayChainLogs(context.Background(), 1, 20)
	if err != nil || len(logs) != 2 {
		t.Fatalf("logs=%d err=%v", len(logs), err)
	}
	for _, row := range logs {
		want := ""
		if row.AccountID < 0 {
			want = fallbackReasonQueueThreshold
		}
		if row.FallbackReason != want {
			t.Fatalf("account=%d reason=%q want=%q", row.AccountID, row.FallbackReason, want)
		}
	}
}

func TestFallbackReasonsForWaitAndOversize(t *testing.T) {
	store := newFallbackQueueTestStore()
	defer store.Stop()
	primary := store.NextExcluding(0, nil)
	if primary == nil {
		t.Fatal("could not occupy primary")
	}
	defer store.Release(primary)
	pool := newFallbackQueueTestPool(store, 0)
	h := &Handler{store: store, fallbackPool: pool}
	state := h.newFallbackRouteState(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	account, _, _ := h.nextFallbackAwareAccountWithGuard(ctx, state, "unbound", 0, newRetryAccountExclusions(), nil, auth.DispatchPolicyStandard)
	if account != nil {
		store.Release(account)
		t.Fatal("expected wait to end without an account")
	}
	if !state.activateAfterPrimaryExhausted() || state.reason != fallbackReasonWaitEnded {
		t.Fatalf("wait reason=%q", state.reason)
	}
	policy := pool.Policy()
	policy.OversizedRequestDirectFallbackEnabled = true
	pool.SetPolicy(policy)
	state = h.newFallbackRouteState(nil, oversizedDirectFallbackBytes+1)
	if !state.active || state.reason != fallbackReasonOversizedRequest {
		t.Fatalf("oversize reason=%q", state.reason)
	}
	// Later budget checks must not replace the original handoff reason.
	state.retryBudgets(0, 0)
	state.activateAfterRetryBudget(10, 10)
	if state.reason != fallbackReasonOversizedRequest {
		t.Fatalf("handoff reason changed to %q", state.reason)
	}
}
