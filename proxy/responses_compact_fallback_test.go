package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ResponsesCompact previously had no fallback-pool wiring at all: an empty
// primary pool went straight to a bare 503 (no_available_account) even with
// the fallback pool enabled and healthy. This is the delegation target for
// non-streaming, body-signal compaction-trigger requests sent to
// /v1/responses, so that gap silently affected regular /v1/responses traffic
// too. Confirm the fallback pool now absorbs it, mirroring
// TestResponsesUsesFallbackWhenPrimaryPoolIsEmpty for Responses().
func TestResponsesCompactUsesFallbackWhenPrimaryPoolIsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath, seenAuth string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenBody = readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_fallback",
			"object":"response",
			"created_at":1710000000,
			"model":"gpt-4.1-direct",
			"output":[],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},
			"service_tier":"default"
		}`))
	}))
	defer upstream.Close()

	// Primary pool has zero accounts, so account selection must fall straight
	// through to the fallback pool.
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: upstream.URL, APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 2, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetFallbackPool(pool)

	body := []byte(`{"model":"gpt-4.1-direct","input":"hello","include":["reasoning.encrypted_content"],"store":true,"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (fallback pool should have absorbed the empty primary pool); body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "no_available_account") {
		t.Fatalf("response still reports no_available_account despite a configured, healthy fallback pool: %s", recorder.Body.String())
	}
	if seenPath != "/v1/responses/compact" || seenAuth != "Bearer sk-fallback" {
		t.Fatalf("fallback upstream = path:%q auth:%q", seenPath, seenAuth)
	}
	if model := gjsonGetString(seenBody, "model"); model != "gpt-4.1-direct" {
		t.Fatalf("fallback fixed model = %q, body=%s", model, seenBody)
	}
	if got := ctx.GetString(contextFallbackReason); got != fallbackReasonNoEligible {
		t.Fatalf("fallback reason = %q, want %q", got, fallbackReasonNoEligible)
	}
	if account := pool.Accounts()[0]; account.GetActiveRequests() != 0 || account.GetOccupiedRequests() != 0 {
		t.Fatalf("fallback lease leaked: active=%d occupied=%d", account.GetActiveRequests(), account.GetOccupiedRequests())
	}
}

// A request pinned to known compaction affinity must never spill to the
// generic fallback pool -- that pool has no knowledge of the encrypted
// reasoning/previous_response_id state tied to the original account's domain,
// so routing it there would silently corrupt the compaction. This must stay
// true even when a healthy fallback pool is configured and enabled.
func TestResponsesCompactDoesNotUseFallbackWhenCompactionAffinityIsKnown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: "https://fallback.example/v1", APIKey: "sk-fallback",
		Model: "gpt-4.1-direct", Concurrency: 2, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	handler := NewHandler(store, nil, nil, nil)
	handler.SetFallbackPool(pool)
	handler.SetRuntimeCache(cache.NewMemory(1))

	// Seed provenance so resolveCompactionAffinity resolves this encrypted
	// content to a known (but now absent from the primary pool) account.
	relay := &auth.Account{DBID: 7, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example/v1", APIKey: "key"}
	if err := handler.recordCompactionProvenance(context.Background(), relay, "known-state"); err != nil {
		t.Fatalf("recordCompactionProvenance() error = %v", err)
	}

	body := []byte(`{"model":"gpt-4.1-direct","input":[{"type":"compaction","encrypted_content":"known-state"}],"include":["reasoning.encrypted_content"],"store":true,"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (compaction_upstream_unavailable); body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "compaction_upstream_unavailable") {
		t.Fatalf("body = %q, want compaction_upstream_unavailable error (not a generic no_available_account or fallback response)", recorder.Body.String())
	}
	if account := pool.Accounts()[0]; account.GetActiveRequests() != 0 || account.GetOccupiedRequests() != 0 {
		t.Fatalf("fallback account was leased despite known compaction affinity: active=%d occupied=%d", account.GetActiveRequests(), account.GetOccupiedRequests())
	}
}
