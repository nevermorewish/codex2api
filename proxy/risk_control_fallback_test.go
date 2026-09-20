package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func keywordRiskService(t *testing.T) *riskcontrol.Service {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := riskcontrol.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	cfg := s.Config()
	cfg.Enabled, cfg.Strategy, cfg.Keywords = true, "keyword_only", []string{"blocked-fixture"}
	if err := s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRiskControlDirectFallbackRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	service := keywordRiskService(t)
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, scenario := range []string{"hit", "streaming", "clean", "switch_off", "disabled", "empty", "model_mismatch", "fallback_failure"} {
			t.Run(endpoint+"/"+scenario, func(t *testing.T) {
				cfg := service.Config()
				cfg.FallbackOnBlock = scenario != "switch_off"
				if err := service.Update(context.Background(), cfg); err != nil {
					t.Fatal(err)
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
				h.SetRiskControl(service)
				input := "blocked-fixture"
				if scenario == "clean" {
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
				case "clean":
					wantPrimary, wantFallback = 1, 0
				case "switch_off", "disabled", "empty", "model_mismatch":
					wantFallback, wantStatus = 0, 403
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

func TestRiskControlFallbackGuards(t *testing.T) {
	s := keywordRiskService(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: "http://localhost:9999", APIKey: "test", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	h := &Handler{riskControl: s, fallbackPool: pool}
	for _, endpoint := range []string{"/v1/images/generations", "/v1/videos/generations", "/v1/realtime"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", endpoint, nil)
		d := h.checkRiskControl(c, []byte(`{"input":"blocked-fixture","prompt":"blocked-fixture"}`), endpoint, "gpt-5.5")
		if !d.Blocked {
			t.Fatalf("unsupported endpoint escaped review: %s %+v", endpoint, d)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	if d := h.checkRiskControl(c, []byte(`{"previous_response_id":"resp_owned","input":"blocked-fixture"}`), "/v1/responses", "gpt-5.5"); !d.Blocked {
		t.Fatal("provider-owned continuation escaped")
	}
	c, _ = gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	if d := h.checkRiskControl(c, []byte(`{"input":"blocked-fixture"}`), "/v1/responses", "gpt-5.5"); d.Blocked {
		t.Fatal("eligible review was not deferred")
	}
	state := h.newFallbackRouteState(nil)
	if requireRiskControlFallback(c, state, false) == nil || state.active {
		t.Fatal("pinned compaction escaped")
	}
	state = h.newFallbackRouteState(func(*auth.Account) bool { return false })
	if requireRiskControlFallback(c, state, true) == nil {
		t.Fatal("account scope filter ignored")
	}
	logs, err := s.Store().RiskLogs(context.Background(), riskcontrol.LogFilter{})
	if err != nil || logs.Total == 0 {
		t.Fatalf("audit log lost: %v", err)
	}
	for _, e := range logs.Items {
		if !e.Blocked || e.AutoBanned {
			t.Fatalf("audit verdict changed: %+v", e)
		}
	}
}

func TestRiskControlFallbackWebSocketTurnIsolation(t *testing.T) {
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
	h.SetRiskControl(keywordRiskService(t))
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

func TestRiskControlFallbackReviewEngines(t *testing.T) {
	service := keywordRiskService(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: "http://localhost:9999", APIKey: "test", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	h := &Handler{riskControl: service, fallbackPool: pool}
	for _, engine := range []string{"chat"} {
		t.Run(engine, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if engine == "moderations" {
					io.WriteString(w, `{"results":[{"category_scores":{"violence":0.99}}]}`)
				} else {
					io.WriteString(w, `{"choices":[{"message":{"content":"{\"risk\":\"unsafe\",\"confidence\":0.99,\"reason\":\"fixture\"}"}}]}`)
				}
			}))
			defer server.Close()
			cfg := service.Config()
			cfg.Engine = engine
			cfg.Strategy = "api_only"
			cfg.PreHash = false
			cfg.BaseURL = server.URL
			cfg.APIKeys = []string{"test"}
			cfg.Audit.Nodes = []riskcontrol.AuditNode{{ID: "custom", Name: "custom", Enabled: true, BaseURL: server.URL, Model: "local/custom-model", TimeoutMS: 3000, MaxInputChars: 400000}}
			if err := service.Update(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			check := func() riskcontrol.Decision {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				d := h.checkRiskControl(c, []byte(`{"input":"engine fixture"}`), "/v1/responses", "gpt-5.5")
				if d.Blocked || !d.Flagged {
					t.Fatalf("expected review handoff: %+v", d)
				}
				state := h.newFallbackRouteState(nil)
				if requireRiskControlFallback(c, state, true) != nil || !state.active || state.reason != fallbackReasonContentReview {
					t.Fatal("missing direct fallback")
				}
				return d
			}
			if d := check(); d.Action != "block" {
				t.Fatalf("action=%s", d.Action)
			}
			cfg.PreHash = true
			if err := service.Update(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if d := check(); d.Action != "hash_block" {
				t.Fatalf("hash action=%s", d.Action)
			}
		})
	}
}

func TestRiskControlNonBlockingAndErrorsDoNotForceFallback(t *testing.T) {
	service := keywordRiskService(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: "http://localhost:9999", APIKey: "test", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 1})
	h := &Handler{riskControl: service, fallbackPool: pool}
	failedAudit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer failedAudit.Close()
	for _, scenario := range []string{"off", "disabled", "observe", "fail_open", "fail_closed", "selected_primary"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := riskcontrol.DefaultConfig()
			cfg.Enabled = true
			cfg.Strategy = "keyword_only"
			cfg.Keywords = []string{"blocked-fixture"}
			switch scenario {
			case "off":
				cfg.Mode = "off"
			case "disabled":
				cfg.Enabled = false
			case "observe":
				cfg.Mode = "observe"
			case "fail_open", "fail_closed":
				cfg.Engine = "chat"
				cfg.Strategy = "api_only"
				cfg.Audit.FailOpen = scenario == "fail_open"
				cfg.Audit.Nodes = []riskcontrol.AuditNode{{ID: "broken", Name: "broken", Enabled: true, BaseURL: failedAudit.URL, Model: "custom", TimeoutMS: 1000, MaxInputChars: 400000}}
			}
			if err := service.Update(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set(contextRiskPrimarySelected, scenario == "selected_primary")
			d := h.checkRiskControl(c, []byte(`{"input":"blocked-fixture"}`), "/v1/responses", "gpt-5.5")
			if _, ok := c.Get(contextRiskControlFallback); ok {
				t.Fatalf("%s incorrectly forced fallback: %+v", scenario, d)
			}
			wantBlocked := scenario == "fail_closed" || scenario == "selected_primary"
			if d.Blocked != wantBlocked {
				t.Fatalf("decision=%+v", d)
			}
			if scenario == "fail_closed" && d.Status != 503 {
				t.Fatalf("lost review failure status: %+v", d)
			}
		})
	}
}
