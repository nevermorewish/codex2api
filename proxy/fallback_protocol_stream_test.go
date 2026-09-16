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

// Streaming counterpart of TestChatCompletionsFallbackProtocolForwardsVerbatim:
// a stream:true request must also reach the provider's own endpoint with the
// caller's messages[] intact, and the provider's SSE must reach the client
// unchanged.
func TestChatCompletionsFallbackProtocolForwardsStreamingVerbatim(t *testing.T) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-6-astra","choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":"pong"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-6-astra","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 1, BaseURL: upstream.URL, APIKey: "fallback-key", Enabled: true,
		Protocol: auth.FallbackProtocolChatCompletions,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true})
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	h.ChatCompletions(c)

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if !gjson.GetBytes(gotBody, "messages.0.content").Exists() {
		t.Fatalf("streaming request was translated, not forwarded verbatim: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("Responses-only input[] leaked into the verbatim request: %s", gotBody)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "pong") {
		t.Fatalf("provider stream content missing: %s", body)
	}
	// The provider's own chunk framing must survive; a translation would have
	// rewritten these into Responses SSE events.
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("provider chunk framing was rewritten: %s", body)
	}
}
