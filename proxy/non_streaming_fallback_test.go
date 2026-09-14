package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestNonStreamingDirectFallbackRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	streamTrue, streamFalse := true, false
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages", "/v1/responses/compact"} {
		for _, tc := range []struct {
			name            string
			stream          *bool
			enabled, direct bool
			unavailable     string
		}{
			{name: "explicit_false", stream: &streamFalse, enabled: true, direct: true},
			{name: "omitted", enabled: true, direct: true},
			{name: "streaming", stream: &streamTrue, enabled: true, direct: true},
			{name: "switch_off", stream: &streamFalse, enabled: true},
			{name: "pool_off", direct: true},
			{name: "empty_pool", enabled: true, direct: true, unavailable: "empty"},
			{name: "model_not_supported", enabled: true, direct: true, unavailable: "model"},
			{name: "occupied_fallback_still_dispatches", enabled: true, direct: true, unavailable: "busy"},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var primaryCalls, fallbackCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") == "Bearer sk-primary" {
						primaryCalls.Add(1)
					} else {
						fallbackCalls.Add(1)
					}
					const response = `{"id":"resp_direct_fallback","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
					requestBody := readUpstreamRequestBody(r)
					if r.URL.Path == "/v1/responses/compact" || !gjson.GetBytes(requestBody, "stream").Bool() {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
					io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n")
				}))
				defer upstream.Close()
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
				store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "sk-primary", Models: []string{"gpt-5.4"}, PlanType: "api"})
				pool := auth.NewFallbackPool(store)
				models := []string{"gpt-5.4"}
				if tc.unavailable == "model" {
					models = []string{"unrelated-model"}
				}
				if tc.unavailable != "empty" {
					pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "backup", BaseURL: upstream.URL, APIKey: "sk-fallback", Models: models, Concurrency: 1, Enabled: true}})
				}
				pool.SetPolicy(auth.FallbackPolicy{Enabled: tc.enabled, RelayCount: 3, NonStreamingDirectFallbackEnabled: tc.direct})
				if tc.unavailable == "busy" {
					busy := pool.Acquire(nil, nil)
					if busy == nil {
						t.Fatal("could not fill fallback concurrency")
					}
					defer store.Release(busy)
				}
				h := NewHandler(store, nil, nil, nil)
				h.SetFallbackPool(pool)
				body := map[string]any{"model": "gpt-5.4"}
				if tc.stream != nil {
					body["stream"] = *tc.stream
				}
				if endpoint == "/v1/messages" || endpoint == "/v1/chat/completions" {
					body["messages"] = []map[string]string{{"role": "user", "content": "hello"}}
					body["max_tokens"] = 64
				} else {
					body["input"] = "hello"
				}
				raw, _ := json.Marshal(body)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
				ctx.Request.Header.Set("Content-Type", "application/json")
				switch endpoint {
				case "/v1/responses":
					h.Responses(ctx)
				case "/v1/chat/completions":
					h.ChatCompletions(ctx)
				case "/v1/messages":
					h.Messages(ctx)
				case "/v1/responses/compact":
					h.ResponsesCompact(ctx)
				}
				direct := tc.enabled && tc.direct && (endpoint == "/v1/responses/compact" || tc.stream == nil || !*tc.stream)
				wantStatus := http.StatusOK
				wantPrimary, wantFallback := int32(1), int32(0)
				if direct {
					wantPrimary, wantFallback = 0, 1
					if tc.unavailable != "" && tc.unavailable != "busy" {
						wantStatus, wantFallback = http.StatusServiceUnavailable, 0
					}
				}
				if recorder.Code != wantStatus || primaryCalls.Load() != wantPrimary || fallbackCalls.Load() != wantFallback {
					t.Fatalf("status=%d primary=%d fallback=%d; want %d/%d/%d; body=%s", recorder.Code, primaryCalls.Load(), fallbackCalls.Load(), wantStatus, wantPrimary, wantFallback, recorder.Body.String())
				}
				if direct && wantFallback > 0 && ctx.GetString(contextFallbackReason) != fallbackReasonNonStreaming {
					t.Fatal("missing non-streaming fallback reason")
				}
				if direct && wantFallback > 0 && (strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") || !json.Valid(recorder.Body.Bytes())) {
					t.Fatalf("non-streaming response is not JSON: %s", recorder.Body.String())
				}
			})
		}
	}
}
