package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// The verbatim path skips the Responses translators, so it must apply the same
// missing-billable-usage guard: a fallback account answering 200 without usage
// must not be delivered downstream as a silent, unbillable success.
func TestVerbatimFallbackWithoutUsageIsNotASilentSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexForceWebsocket = false
	settings.CodexPreflightSSEPassthrough = false
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Content but no usage field at all.
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-6-astra",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`)
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

	if recorder.Code == http.StatusOK {
		t.Fatalf("usage-less fallback answer was delivered as a 200 success: %s", recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "upstream_usage_missing") {
		t.Fatalf("expected the usage-missing error code, got status=%d body=%s", recorder.Code, body)
	}
}
