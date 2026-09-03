package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestResponsesUsesFallbackWhenPrimaryPoolIsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: upstream.URL, APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 2, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetFallbackPool(pool)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != "/v1/responses" || seenAuth != "Bearer sk-fallback" {
		t.Fatalf("fallback upstream = path:%q auth:%q", seenPath, seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("fallback fixed model = %q, body=%s", model, seenBody)
	}
	if account := pool.Accounts()[0]; account.GetActiveRequests() != 0 || account.GetOccupiedRequests() != 0 {
		t.Fatalf("fallback lease leaked: active=%d occupied=%d", account.GetActiveRequests(), account.GetOccupiedRequests())
	}
}

func TestFallbackRouteStateSwitchesAfterPrimaryAttemptThreshold(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, Name: "backup", BaseURL: "https://example.com", APIKey: "sk", Model: "fixed", Concurrency: 1, Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 2})
	state := (&Handler{fallbackPool: pool}).newFallbackRouteState(nil)
	primary := &auth.Account{DBID: 100}

	state.noteSelected(primary)
	if state.usingFallback() {
		t.Fatal("fallback activated before the primary threshold")
	}
	state.noteSelected(primary)
	if !state.usingFallback() {
		t.Fatal("fallback did not activate at the primary threshold")
	}
	account := state.account(nil)
	if account == nil || !account.IsExternalFallback() {
		t.Fatal("active fallback state did not acquire a fallback account")
	}
	store.Release(account)
}

func TestFallbackRouteStateOversizedBoundary(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	pool := auth.NewFallbackPool(store)
	pool.SetPolicy(auth.FallbackPolicy{
		Enabled: true, RelayCount: 3, QueueDirectFallbackThreshold: 5,
		OversizedRequestDirectFallbackEnabled: true,
	})
	handler := &Handler{fallbackPool: pool}

	if state := handler.newFallbackRouteState(nil, oversizedDirectFallbackBytes); state.usingFallback() {
		t.Fatal("a request body exactly 3 MiB must not bypass the primary pool")
	}
	state := handler.newFallbackRouteState(nil, oversizedDirectFallbackBytes+1)
	if !state.usingFallback() || !state.required {
		t.Fatal("a request body larger than 3 MiB must require direct fallback")
	}
}

func TestResponsesOversizedRequestGoesDirectlyToFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var primaryCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()

	var seenPath, seenAuth string
	var seenBody []byte
	fallback := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer fallback.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	store.AddAccount(&auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL,
		APIKey: "sk-primary", Models: []string{"gpt-5.4"}, PlanType: "api",
	})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: fallback.URL, APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 2, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3, OversizedRequestDirectFallbackEnabled: true})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetFallbackPool(pool)

	body := []byte(`{"model":"gpt-5.4","input":"` + strings.Repeat("x", oversizedDirectFallbackBytes) + `","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if calls := atomic.LoadInt32(&primaryCalls); calls != 0 {
		t.Fatalf("oversized request reached primary upstream %d time(s)", calls)
	}
	if seenPath != "/v1/responses" || seenAuth != "Bearer sk-fallback" {
		t.Fatalf("fallback upstream = path:%q auth:%q", seenPath, seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("fallback fixed model = %q", model)
	}
}

func TestResponsesRelayCountExtendsRetryBudgetForFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var primaryCalls int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&primaryCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary failed"}}`))
	}))
	defer primary.Close()

	var seenPath, seenAuth string
	var seenBody []byte
	fallback := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer fallback.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	store.AddAccount(&auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL,
		APIKey: "sk-primary", Models: []string{"gpt-5.4"}, PlanType: "api",
	})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: fallback.URL, APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 2, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetFallbackPool(pool)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if calls := atomic.LoadInt32(&primaryCalls); calls != 1 {
		t.Fatalf("primary calls = %d, want 1", calls)
	}
	if seenAuth != "Bearer sk-fallback" {
		t.Fatalf("fallback authorization = %q", seenAuth)
	}
}

func TestFallbackQueueWaitsBelowThreshold(t *testing.T) {
	store := newFallbackQueueTestStore()
	leased := store.NextExcluding(0, nil)
	if leased == nil {
		t.Fatal("failed to occupy primary account")
	}
	pool := newFallbackQueueTestPool(store, 2)
	state := (&Handler{store: store, fallbackPool: pool}).newFallbackRouteState(nil)
	handler := &Handler{store: store, fallbackPool: pool}
	type selection struct {
		account *auth.Account
	}
	selected := make(chan selection, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		account, _, _ := handler.nextFallbackAwareAccountWithGuard(ctx, state, "queue-test", 0, newRetryAccountExclusions(), nil, auth.DispatchPolicyStandard)
		selected <- selection{account: account}
	}()
	waitForFallbackQueueDepth(t, store, 1)
	select {
	case result := <-selected:
		if result.account != nil {
			store.Release(result.account)
		}
		t.Fatal("request bypassed the local queue below the configured threshold")
	default:
	}
	store.Release(leased)
	result := <-selected
	if result.account == nil || result.account.IsExternalFallback() {
		t.Fatal("queued request did not resume on the primary pool")
	}
	store.Release(result.account)
}

func TestFallbackQueueBypassesAtThreshold(t *testing.T) {
	store := newFallbackQueueTestStore()
	leased := store.NextExcluding(0, nil)
	if leased == nil {
		t.Fatal("failed to occupy primary account")
	}
	defer store.Release(leased)

	waitCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		account := store.WaitForAvailable(waitCtx, time.Minute, 0)
		if account != nil {
			store.Release(account)
		}
	}()
	waitForFallbackQueueDepth(t, store, 1)

	pool := newFallbackQueueTestPool(store, 1)
	handler := &Handler{store: store, fallbackPool: pool}
	state := handler.newFallbackRouteState(nil)
	account, _, _ := handler.nextFallbackAwareAccountWithGuard(context.Background(), state, "queue-bypass", 0, newRetryAccountExclusions(), nil, auth.DispatchPolicyStandard)
	if account == nil || !account.IsExternalFallback() {
		t.Fatal("new request did not bypass the local queue at the configured threshold")
	}
	store.Release(account)
	cancelWaiter()
	<-waiterDone
}

func TestFallbackQueueThresholdZeroDisablesBypass(t *testing.T) {
	store := newFallbackQueueTestStore()
	pool := newFallbackQueueTestPool(store, 0)
	state := (&Handler{store: store, fallbackPool: pool}).newFallbackRouteState(nil)
	if state.queueThresholdReached(store) {
		t.Fatal("queue threshold 0 must disable direct fallback")
	}
}

func newFallbackQueueTestStore() *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://primary.example.com",
		APIKey: "sk-primary", Models: []string{"gpt-5.4"}, PlanType: "api",
	})
	return store
}

func newFallbackQueueTestPool(store *auth.Store, threshold int) *auth.FallbackPool {
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: "https://fallback.example.com", APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 1, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3, QueueDirectFallbackThreshold: threshold})
	return pool
}

func waitForFallbackQueueDepth(t *testing.T, store *auth.Store, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.GetSchedulerMetrics().Waiters == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue depth = %d, want %d", store.GetSchedulerMetrics().Waiters, want)
}
