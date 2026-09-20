package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func promptFallbackTestConfig() promptfilter.Config {
	cfg := promptGuardTestConfig()
	cfg.Mode = promptfilter.ModeFallback
	cfg.Review.Enabled = false
	cfg.Advanced.Guard.Mode = promptfilter.GuardModeShadow
	cfg.CustomPatterns = []promptfilter.PatternConfig{{Name: "fallback_fixture", Pattern: "blocked-fixture", Weight: 100, Category: "credential_theft", Strict: true}}
	return promptfilter.NormalizeConfig(cfg)
}

func TestPromptFilterFallbackGuardsAndAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "prompt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := promptFallbackTestConfig()
	h := newPromptGuardTestHandler(cfg)
	defer h.store.Stop()
	h.db = db
	pool := auth.NewFallbackPool(h.store)
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true})
	h.SetFallbackPool(pool)
	body := []byte(`{"model":"gpt-5.5","input":"blocked-fixture"}`)
	for _, tc := range []struct {
		name, endpoint, body string
		selected             bool
		want                 bool
	}{
		{"ordinary", "/v1/responses", string(body), false, true},
		{"previous response", "/v1/responses", `{"input":"blocked-fixture","previous_response_id":"resp_owned"}`, false, false},
		{"image", "/v1/images/generations", string(body), false, false},
		{"selected primary", "/v1/messages", string(body), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", tc.endpoint, strings.NewReader(tc.body))
			if tc.selected {
				c.Set(contextFallbackDeadlineState, h.newFallbackRouteState(nil))
			}
			evaluation := h.evaluatePromptGuardWithConfig(c, cfg, body, body, "/v1/responses", "gpt-5.5", promptfilter.TransportHTTP)
			if evaluation.Verdict.Action != promptfilter.ActionBlock {
				t.Fatalf("expected violation: %+v", evaluation.Verdict)
			}
			if got := h.preparePromptFilterFallback(c, cfg, []byte(tc.body), tc.endpoint, evaluation); got != tc.want {
				t.Fatalf("fallback=%v want %v", got, tc.want)
			}
			if tc.want {
				h.logPromptGuardEvaluation(c, tc.endpoint, "gpt-5.5", "local_filter", "", evaluation)
				state := h.newFallbackRouteState(nil)
				if requirePromptFilterFallback(c, state, false) == "" || state.active {
					t.Fatal("bound continuation was not rejected")
				}
				if message := requirePromptFilterFallback(c, state, true); message != "" || !state.active || !state.required {
					t.Fatalf("route not required: %s", message)
				}
			}
		})
	}
	waitPromptFilterAuditIdle(t, db)
	logs, err := db.ListPromptFilterLogs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Action != promptfilter.ActionFallback || logs[0].Mode != promptfilter.ModeFallback || logs[0].StrikeEligible || logs[0].FullText == "" {
		t.Fatalf("unexpected audit: %+v", logs)
	}
}
func TestPromptFilterDirectFallbackRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)

	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, scenario := range []string{"hit", "streaming", "clean", "switch_off", "disabled", "empty", "model_mismatch", "fallback_failure", "audit_failure", "audit_fail_closed", "model_violation", "model_clean", "monitor", "warn", "filter_off", "local_hit_review_outage"} {
			t.Run(endpoint+"/"+scenario, func(t *testing.T) {
				cfg := promptFallbackTestConfig()
				if scenario == "switch_off" {
					cfg.Mode = promptfilter.ModeBlock
					cfg.Advanced.Guard.Mode = promptfilter.GuardModeInherit
				}
				if scenario == "monitor" || scenario == "warn" {
					cfg.Mode = scenario
					cfg.Advanced.Guard.Mode = promptfilter.GuardModeInherit
				}
				if scenario == "filter_off" {
					cfg.Enabled = false
				}
				if scenario == "audit_failure" || scenario == "audit_fail_closed" || scenario == "model_violation" || scenario == "model_clean" || scenario == "local_hit_review_outage" {
					reviewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasPrefix(scenario, "audit_") || scenario == "local_hit_review_outage" {
							w.WriteHeader(503)
							return
						}
						confidence := 0.01
						if scenario == "model_violation" {
							confidence = 0.99
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": fmt.Sprintf("{\"confidence\":%g,\"reason\":\"test review\"}", confidence)}}}})
					}))
					defer reviewer.Close()
					cfg.Review.Enabled = true
					cfg.Review.APIKey = "review-only"
					cfg.Review.BaseURL = reviewer.URL
					cfg.Review.FailClosed = scenario == "audit_fail_closed"
					cfg.Review.Adapter.RequestMode = promptfilter.ReviewRequestModeChatCompletions
				}
				var primaryCalls, fallbackCalls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") == "Bearer primary" {
						primaryCalls.Add(1)
					} else {
						fallbackCalls.Add(1)
					}
					if scenario == "fallback_failure" {
						w.WriteHeader(502)
						io.WriteString(w, `{"error":{"message":"fallback failed"}}`)
						return
					}
					body := readUpstreamRequestBody(r)
					const response = `{"id":"resp_review","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
					if r.URL.Path == "/v1/responses/compact" || !gjson.GetBytes(body, "stream").Bool() {
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
				defer store.Stop()
				store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "primary", Models: []string{"gpt-5.5"}, PlanType: "api"})
				pool := auth.NewFallbackPool(store)
				models := []string{"gpt-5.5"}
				if scenario == "model_mismatch" {
					models = []string{"other-model"}
				}
				if scenario != "empty" {
					pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "backup", BaseURL: upstream.URL, APIKey: "fallback", Models: models, Enabled: true}})
				}
				pool.SetPolicy(auth.FallbackPolicy{Enabled: scenario != "disabled", RelayCount: 3})
				h := NewHandler(store, nil, nil, nil)
				h.SetFallbackPool(pool)
				store.SetPromptFilterConfig(cfg)
				input := "blocked-fixture"
				if scenario == "clean" || strings.HasPrefix(scenario, "audit_") || (scenario == "model_violation" || scenario == "model_clean") {
					input = "hello"
				}
				body := map[string]any{"model": "gpt-5.5", "stream": scenario == "streaming"}
				if endpoint == "/v1/messages" || endpoint == "/v1/chat/completions" {
					body["messages"] = []map[string]string{{"role": "user", "content": input}}
					body["max_tokens"] = 64
				} else {
					body["input"] = input
				}
				raw, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", endpoint, strings.NewReader(string(raw)))
				c.Request.Header.Set("Content-Type", "application/json")
				switch endpoint {
				case "/v1/responses":
					h.Responses(c)
				case "/v1/responses/compact":
					h.ResponsesCompact(c)
				case "/v1/chat/completions":
					h.ChatCompletions(c)
				case "/v1/messages":
					h.Messages(c)
				}
				wantPrimary, wantFallback, wantStatus := int32(0), int32(1), 200
				switch scenario {
				case "clean", "audit_failure", "model_clean", "monitor", "warn", "filter_off":
					wantPrimary, wantFallback = 1, 0
				case "switch_off", "disabled", "audit_fail_closed":
					wantFallback, wantStatus = 0, 400
				case "empty", "model_mismatch":
					wantFallback, wantStatus = 0, 503
				case "fallback_failure":
					wantStatus = 502
				}
				if rec.Code != wantStatus || primaryCalls.Load() != wantPrimary || fallbackCalls.Load() != wantFallback {
					t.Fatalf("status/primary/fallback=%d/%d/%d want %d/%d/%d body=%s", rec.Code, primaryCalls.Load(), fallbackCalls.Load(), wantStatus, wantPrimary, wantFallback, rec.Body.String())
				}
				if wantFallback > 0 && c.GetString(contextFallbackReason) != fallbackReasonContentReview {
					t.Fatal("missing review handoff reason")
				}
				for _, a := range pool.Accounts() {
					if a.GetOccupiedRequests() != 0 || a.GetActiveRequests() != 0 {
						t.Fatal("leaked fallback lease")
					}
				}
			})
		}
	}
}

func TestPromptFilterFallbackWebSocketTurnIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	previous, previousExec := CurrentRuntimeSettings(), WebsocketExecuteFunc
	t.Cleanup(func() { ApplyRuntimeSettings(previous); WebsocketExecuteFunc = previousExec })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccountID: "primary", AccessToken: "test-only", PlanType: "pro"})
	var primaryCalls, fallbackCalls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, cfg *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
		primaryCalls.Add(1)
		payload := strings.ReplaceAll(wsFallbackSuccess, "resp_fallback", "resp_primary")
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}, nil
	}
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+wsFallbackSuccess+"\n\n")
	}))
	defer fallback.Close()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 9, BaseURL: fallback.URL, APIKey: "test", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3})
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)
	store.SetPromptFilterConfig(promptFallbackTestConfig())
	type turnResult struct {
		err    error
		reason string
	}
	results := make(chan turnResult, 3)
	done := make(chan struct{})
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(done)
		conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			results <- turnResult{err: err}
			return
		}
		defer conn.Close()
		for i := 0; i < 3; i++ {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			err = h.forwardResponsesWebSocketTurn(c, conn, body, fmt.Sprint(i), nil)
			results <- turnResult{err: err, reason: c.GetString(contextFallbackReason)}
			if err != nil {
				return
			}
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		conn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("WS handler did not stop")
		}
	}()
	for i, input := range []string{"blocked-fixture", "clean", "blocked-fixture"} {
		body := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.5","input":[{"role":"user","content":%q}],"store":false}`, input)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			kind := gjson.GetBytes(body, "type").String()
			if kind == "error" || kind == "response.failed" {
				t.Fatalf("turn %d failed: %s", i, body)
			}
			if kind == "response.completed" {
				break
			}
		}
		result := <-results
		wantReason := fallbackReasonContentReview
		if i == 1 {
			wantReason = ""
		}
		if result.err != nil || result.reason != wantReason {
			t.Fatalf("turn %d: %+v", i, result)
		}
		if primaryCalls.Load() != []int32{0, 1, 1}[i] || fallbackCalls.Load() != []int32{1, 1, 2}[i] {
			t.Fatalf("turn %d misrouted: primary=%d fallback=%d", i, primaryCalls.Load(), fallbackCalls.Load())
		}
	}
}
