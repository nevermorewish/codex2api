package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// An external fallback account configured with the chat_completions protocol
// must receive the downstream /v1/chat/completions request verbatim: the
// provider's own endpoint, the caller's message shape (not a Responses body),
// and the provider's raw answer back downstream.
func TestChatCompletionsFallbackProtocolForwardsVerbatim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexForceWebsocket = false
	settings.CodexPreflightSSEPassthrough = false
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)

	var calls atomic.Int32
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-6-astra",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 1, BaseURL: upstream.URL, APIKey: "fallback-key", Enabled: true,
		Protocol: auth.FallbackProtocolChatCompletions,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, NonStreamingDirectFallbackEnabled: true})
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"ping"}],"stream":false}`))
	h.ChatCompletions(c)

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	// Verbatim means the caller's messages survive; a Responses translation
	// would have replaced them with an input[] array and added instructions.
	if !gjson.GetBytes(gotBody, "messages.0.content").Exists() {
		t.Fatalf("request was translated, not forwarded verbatim: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "instructions").Exists() {
		t.Fatalf("Responses-only field leaked into the verbatim request: %s", gotBody)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "pong") {
		t.Fatalf("provider answer was not passed through: %s", recorder.Body.String())
	}
}

// The default (unconfigured) fallback account must keep the historical
// Responses projection: this is what every existing row and every generic
// relay depends on.
func TestFallbackWithoutProtocolKeepsResponsesProjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexForceWebsocket = false
	settings.CodexPreflightSSEPassthrough = false
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)

	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		// The Responses projection always asks the upstream for a stream; a
		// non-streaming downstream request is aggregated by the gateway.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\","+
			"\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: upstream.URL, APIKey: "fallback-key", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, NonStreamingDirectFallbackEnabled: true})
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"ping"}],"stream":false}`))
	h.ChatCompletions(c)

	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses (projection must be unchanged)", gotPath)
	}
	// A translated body carries input[] instead of the caller's messages[].
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("chat body was forwarded verbatim despite no protocol opt-in: %s", gotBody)
	}
	if !gjson.GetBytes(gotBody, "input.0.content").Exists() {
		t.Fatalf("Responses projection is missing: %s", gotBody)
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("projection no longer requests an upstream stream: %s", gotBody)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "pong") {
		t.Fatalf("translated answer missing: %s", recorder.Body.String())
	}
}
