package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const wsFallbackOverload = `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"primary overloaded"}}}`
const wsFallbackSuccess = `{"type":"response.completed","response":{"id":"resp_fallback","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`

// Exercise the real per-turn forwarder over one downstream socket. The wrapper
// snapshots Gin metadata after each completed turn, before accepting the next.
func TestNativeWSFallbackMultiTurnIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	previous, previousExec := CurrentRuntimeSettings(), WebsocketExecuteFunc
	previousMetrics := globalFallbackMetrics
	globalFallbackMetrics = newFallbackMetrics()
	t.Cleanup(func() {
		ApplyRuntimeSettings(previous)
		WebsocketExecuteFunc = previousExec
		globalFallbackMetrics = previousMetrics
	})
	settings := previous
	settings.CodexWSSilentRetry, settings.CodexWSHideErrors = true, false
	settings.CodexWSSilentRetries = 1
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.SetMaxRetries(1)
	store.SetRetryIntervalMS(0)
	store.SetTransportRetryPolicy("rotate")
	var accounts []*auth.Account
	for id := int64(1); id <= 8; id++ {
		a := &auth.Account{DBID: id, Name: fmt.Sprintf("primary-%d", id), AccountID: fmt.Sprint(id), AccessToken: "test-only", PlanType: "pro"}
		store.AddAccount(a)
		accounts = append(accounts, a)
	}
	var mu sync.Mutex
	primaryCalls, fallbackCalls := 0, 0
	WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, cfg *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
		mu.Lock()
		primaryCalls++
		mu.Unlock()
		payload := wsFallbackOverload
		if strings.Contains(string(body), "primary-success") {
			payload = strings.ReplaceAll(wsFallbackSuccess, "resp_fallback", "resp_primary")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}, nil
	}
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		fallbackCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+wsFallbackSuccess+"\n\n")
	}))
	defer fallback.Close()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "multi-turn-backup", BaseURL: fallback.URL, APIKey: "backup-only", Model: "fallback-model", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 10})
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	h.SetFallbackPool(pool)
	type turnResult struct {
		err                      error
		fallbackName, sourceName string
		fallbackReason           string
		sourceID                 int64
		occupied                 int64
		metrics                  FallbackMetricsSnapshot
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
		for turn := 0; turn < 3; turn++ {
			_, body, err := conn.ReadMessage()
			if err != nil {
				results <- turnResult{err: err}
				return
			}
			err = h.forwardResponsesWebSocketTurn(c, conn, body, fmt.Sprintf("turn-%d", turn), nil)
			result := turnResult{err: err, fallbackName: c.GetString(contextFallbackAccountName), sourceName: c.GetString(contextSourceAccountName), sourceID: c.GetInt64(contextSourceAccountID), metrics: GetFallbackMetricsSnapshot()}
			result.fallbackReason = c.GetString(contextFallbackReason)
			for _, a := range append(accounts, pool.Accounts()...) {
				result.occupied += a.GetOccupiedRequests() + a.GetActiveRequests()
			}
			results <- result
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
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("WS handler did not stop")
		}
	}()
	for turn, input := range []string{"fallback-first", "primary-success", "fallback-third"} {
		request := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","store":%t,"input":[{"type":"message","role":"user","content":%q}],"prompt_cache_key":"same-multi-turn-session"}`, turn != 0, input)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
			kind := gjson.GetBytes(body, "type").String()
			if kind == "error" || kind == "response.failed" {
				t.Fatalf("turn %d leaked failed primary: %s", turn, body)
			}
			if kind == "response.completed" {
				wantID := "resp_fallback"
				if turn == 1 {
					wantID = "resp_primary"
				}
				if gjson.GetBytes(body, "response.id").String() != wantID {
					t.Fatalf("turn %d: wrong terminal %s", turn, body)
				}
				break
			}
		}
		var result turnResult
		select {
		case result = <-results:
		case <-time.After(time.Second):
			t.Fatal("missing turn result")
		}
		if result.err != nil || result.occupied != 0 {
			t.Fatalf("turn %d err=%v occupied=%d", turn, result.err, result.occupied)
		}
		// The forwarder has returned, so its cache commit is complete. The
		// fallback adapter must obey store:false and retain the current turn
		// when storage is enabled under the upstream replay-source API.
		cachedFallback := getResponseCache("anon", "resp_fallback")
		if turn == 0 && len(cachedFallback) != 0 {
			t.Fatal("store:false fallback turn was cached")
		}
		if turn == 2 && (len(cachedFallback) != 1 || gjson.GetBytes(cachedFallback[0], "content").String() != input) {
			t.Fatalf("fallback replay snapshot lost the current input: %s", cachedFallback)
		}
		if turn == 1 {
			if result.fallbackName != "" || result.sourceName != "" || result.sourceID != 0 || result.fallbackReason != "" {
				t.Fatalf("fallback attribution leaked into primary turn: %+v", result)
			}
		} else if result.fallbackName != "multi-turn-backup" || result.sourceID <= 0 || result.sourceName == "" || result.fallbackReason != fallbackReasonRetryBudget {
			t.Fatalf("missing fallback attribution: %+v", result)
		}
		wantPrimary, wantFallback := []int{2, 3, 5}[turn], []int{1, 1, 2}[turn]
		mu.Lock()
		primary, backup := primaryCalls, fallbackCalls
		mu.Unlock()
		if primary != wantPrimary || backup != wantFallback {
			t.Fatalf("turn %d budgets leaked: primary=%d fallback=%d", turn, primary, backup)
		}
		m := result.metrics
		if m.WSPrimaryAttempts != uint64(wantPrimary) || m.FallbackHandoffCount != uint64(wantFallback) || m.FallbackAttemptCount != uint64(wantFallback) || m.FallbackSuccessCount != uint64(wantFallback) || m.UpstreamOverloadedCount != []uint64{2, 2, 4}[turn] {
			t.Fatalf("turn %d bad metrics: %+v", turn, m)
		}
		if m.FallbackFailureCount != 0 || len(m.Accounts) != 1 || m.Accounts[0].Success != uint64(wantFallback) {
			t.Fatalf("bad fallback outcome counters: %+v", m)
		}
	}
}

func TestNativeWSFallbackHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                                             string
		wsRetries, maxRetries, relayCount                                int
		failure                                                          string
		fallbackFailure                                                  string
		wantPrimary, wantFallback                                        int
		sticky, silentOff, fallbackOff, emptyPrimary, fallbackFails      bool
		incompatible, emptyFallback, continuation, preflight, continuous bool
	}{
		{name: "default_budget", wsRetries: 2, maxRetries: 2, relayCount: 3, failure: "overload", wantPrimary: 3, wantFallback: 1},
		{name: "relay_count_cannot_extend", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, wantFallback: 1},
		{name: "ws_zero", wsRetries: 0, maxRetries: 3, relayCount: 10, failure: "overload", wantPrimary: 1, wantFallback: 1},
		{name: "global_zero", wsRetries: 5, maxRetries: 0, relayCount: 10, failure: "overload", wantPrimary: 1, wantFallback: 1},
		{name: "global_cap", wsRetries: 5, maxRetries: 1, relayCount: 10, failure: "overload", wantPrimary: 2, wantFallback: 1},
		{name: "silent_off", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 1, wantFallback: 1, silentOff: true},
		{name: "early_relay", wsRetries: 5, maxRetries: 5, relayCount: 1, failure: "overload", wantPrimary: 1, wantFallback: 1},
		{name: "sticky_capacity", wsRetries: 2, maxRetries: 2, relayCount: 3, failure: "overload", wantPrimary: 3, wantFallback: 1, sticky: true},
		{name: "eof", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "eof", wantPrimary: 3, wantFallback: 1},
		{name: "transport", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "transport", wantPrimary: 3, wantFallback: 1},
		{name: "http_500", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "http500", wantPrimary: 3, wantFallback: 1},
		{name: "http_429", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "http429", wantPrimary: 3, wantFallback: 1},
		{name: "fallback_503", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, wantFallback: 1, fallbackFails: true},
		{name: "pool_disabled", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, fallbackOff: true},
		{name: "empty_primary", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantFallback: 1, emptyPrimary: true},
		{name: "invalid_request", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "invalid", wantPrimary: 1},
		{name: "after_content", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "after_content", wantPrimary: 1},
		{name: "incompatible_model", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, incompatible: true},
		{name: "empty_fallback", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, emptyFallback: true},
		{name: "provider_continuation", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, continuation: true, sticky: true},
		{name: "preflight_metadata", wsRetries: 2, maxRetries: 2, relayCount: 10, failure: "overload", wantPrimary: 3, wantFallback: 1, preflight: true},
		{name: "continuous_primary_cap", wsRetries: 2, maxRetries: 0, relayCount: 10, failure: "overload", wantPrimary: 1, wantFallback: 1, continuous: true},
		{name: "early_fallback_failure", wsRetries: 10, maxRetries: 10, relayCount: 1, failure: "overload", wantPrimary: 1, wantFallback: 1, fallbackFails: true},
		{name: "direct_fallback_failure", wsRetries: 10, maxRetries: 10, relayCount: 3, wantFallback: 1, emptyPrimary: true, fallbackFails: true, continuous: true},
		{name: "continuous_fallback_http_error", wsRetries: 10, maxRetries: 10, relayCount: 3, failure: "overload", wantPrimary: 3, wantFallback: 1, fallbackFails: true, continuous: true},
		{name: "continuous_fallback_stream_error", wsRetries: 10, maxRetries: 10, relayCount: 3, failure: "overload", wantPrimary: 3, wantFallback: 1, fallbackFails: true, fallbackFailure: "failed", continuous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := CurrentRuntimeSettings()
			previousExec := WebsocketExecuteFunc
			previousMetrics := globalFallbackMetrics
			globalFallbackMetrics = newFallbackMetrics()
			t.Cleanup(func() {
				ApplyRuntimeSettings(previous)
				WebsocketExecuteFunc = previousExec
				globalFallbackMetrics = previousMetrics
			})
			settings := previous
			settings.CodexWSSilentRetry = !tc.silentOff
			settings.CodexWSSilentRetries = tc.wsRetries
			settings.CodexWSHideErrors = tc.fallbackFails
			settings.CodexPreflightSSEPassthrough = tc.preflight
			settings.CodexOverloadPauseEnabled = false
			settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
			if tc.continuous {
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
			}
			ApplyRuntimeSettings(settings)
			var mu sync.Mutex
			var primaryIDs []int64
			fallbackCalls := 0
			var fallbackBody []byte
			var fallbackAuth, fallbackPath string
			WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, sessionID, proxyURL, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
				mu.Lock()
				primaryIDs = append(primaryIDs, a.ID())
				mu.Unlock()
				if a.IsExternalFallback() {
					return nil, errors.New("fallback credential entered Codex WS executor")
				}
				if tc.failure == "transport" {
					return nil, errors.New("connection reset by peer")
				}
				status := http.StatusOK
				sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"primary\"}}\n\n"
				if tc.preflight {
					sse += "data: {\"type\":\"codex.rate_limits\",\"marker\":\"failed-preflight\"}\n\n"
				}
				switch tc.failure {
				case "eof":
				case "invalid":
					status = 400
					sse = `{"error":{"code":"invalid_request","message":"invalid input"}}`
				case "http500":
					status = 500
					sse = `{"error":{"code":"server_error","message":"primary failed"}}`
				case "http429":
					status = 429
					sse = `{"error":{"code":"rate_limit_exceeded","message":"limited"}}`
				default:
					if tc.failure == "after_content" {
						sse += "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already-visible\"}\n\n"
					}
					sse += "data: " + wsFallbackOverload + "\n\n"
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
			}
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				fallbackCalls++
				fallbackBody = body
				fallbackAuth = r.Header.Get("Authorization")
				fallbackPath = r.URL.Path
				mu.Unlock()
				if tc.fallbackFails {
					if tc.fallbackFailure == "failed" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fallback unavailable\"}}}\n\n")
						return
					}
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":{"code":"server_error","message":"fallback unavailable"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback-recovered\"}\n\ndata: "+wsFallbackSuccess+"\n\n")
			}))
			defer fallback.Close()
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			defer store.Stop()
			store.SetMaxRetries(tc.maxRetries)
			store.SetTransportRetryPolicy("rotate")
			if tc.sticky {
				store.SetTransportRetryPolicy("sticky")
			}
			var accounts []*auth.Account
			if !tc.emptyPrimary {
				for i := 1; i <= 8; i++ {
					a := &auth.Account{DBID: int64(i), AccessToken: fmt.Sprintf("at-%d", i), AccountID: fmt.Sprintf("acct-%d", i), PlanType: "pro"}
					store.AddAccount(a)
					accounts = append(accounts, a)
				}
				store.BindSessionAffinity("native-ws-fallback", accounts[0], "")
			}
			pool := auth.NewFallbackPool(store)
			pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "ws-backup", BaseURL: fallback.URL, APIKey: "sk-backup-only", Model: "fallback-model", Enabled: true}})
			if tc.incompatible {
				pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "ws-backup", BaseURL: fallback.URL, APIKey: "sk-backup-only", Models: []string{"unrelated-model"}, Enabled: true}})
			}
			if tc.emptyFallback {
				pool.Replace(nil)
			}
			pool.SetPolicy(auth.FallbackPolicy{Enabled: !tc.fallbackOff, RelayCount: tc.relayCount})
			h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			h.SetFallbackPool(pool)
			router := gin.New()
			h.RegisterRoutes(router)
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			request := `{"type":"response.create","model":"gpt-5.4","input":"hello","prompt_cache_key":"native-ws-fallback"}`
			if tc.continuation {
				request = `{"type":"response.create","model":"gpt-5.4","input":"continue","prompt_cache_key":"native-ws-fallback","previous_response_id":"resp_remote_only","client_metadata":{"x-codex-turn-state":"pinned-turn"}}`
			}
			if err = conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
			var output strings.Builder
			var terminal string
			for i := 0; i < 12; i++ {
				_, frame, readErr := conn.ReadMessage()
				if readErr != nil {
					t.Fatalf("read WS: %v; output=%s", readErr, output.String())
				}
				output.Write(frame)
				kind := gjson.GetBytes(frame, "type").String()
				if kind == "response.completed" || kind == "error" || kind == "response.failed" {
					terminal = kind
					break
				}
			}
			// The terminal frame can arrive just before the server releases leases.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				occupied := int64(0)
				for _, a := range append(accounts, pool.Accounts()...) {
					occupied += a.GetOccupiedRequests()
				}
				if occupied == 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(primaryIDs) != tc.wantPrimary || fallbackCalls != tc.wantFallback {
				t.Fatalf("primary=%v fallback=%d want=%d/%d output=%s", primaryIDs, fallbackCalls, tc.wantPrimary, tc.wantFallback, output.String())
			}
			metrics := GetFallbackMetricsSnapshot()
			if metrics.WSPrimaryAttempts != uint64(tc.wantPrimary) || metrics.FallbackHandoffCount != uint64(tc.wantFallback) || metrics.FallbackAttemptCount != uint64(tc.wantFallback) {
				t.Fatalf("routing metrics disagree with real calls: %+v", metrics)
			}
			if tc.wantFallback > 0 {
				wantSuccess, wantFailure := uint64(1), uint64(0)
				if tc.fallbackFails {
					wantSuccess, wantFailure = 0, 1
				}
				if metrics.FallbackSuccessCount != wantSuccess || metrics.FallbackFailureCount != wantFailure {
					t.Fatalf("fallback outcomes disagree with actual response: %+v", metrics)
				}
			}
			if tc.sticky {
				for _, id := range primaryIDs {
					if id != primaryIDs[0] {
						t.Fatalf("sticky rotated: %v", primaryIDs)
					}
				}
			} else {
				seen := map[int64]bool{}
				for _, id := range primaryIDs {
					if seen[id] {
						t.Fatalf("rotate reused primary: %v", primaryIDs)
					}
					seen[id] = true
				}
			}
			if tc.wantFallback > 0 && !tc.fallbackFails {
				if terminal != "response.completed" || !strings.Contains(output.String(), "fallback-recovered") || strings.Contains(output.String(), "primary overloaded") || strings.Contains(output.String(), "failed-preflight") {
					t.Fatalf("bad WS fallback output: %s", output.String())
				}
				if fallbackAuth != "Bearer sk-backup-only" || fallbackPath != "/v1/responses" {
					t.Fatalf("wrong fallback endpoint/auth: %s %s", fallbackPath, fallbackAuth)
				}
				if gjson.GetBytes(fallbackBody, "type").Exists() || !gjson.GetBytes(fallbackBody, "stream").Bool() || gjson.GetBytes(fallbackBody, "model").String() != "fallback-model" {
					t.Fatalf("invalid HTTP fallback request: %s", fallbackBody)
				}
			} else if terminal == "response.completed" {
				t.Fatalf("failure reported as success: %s", output.String())
			}
			if tc.fallbackFails && !strings.Contains(output.String(), "fallback unavailable") {
				t.Fatalf("fallback error was hidden: %s", output.String())
			}
			for _, a := range append(accounts, pool.Accounts()...) {
				if a.GetActiveRequests() != 0 || a.GetOccupiedRequests() != 0 {
					t.Fatalf("lease leaked account=%d", a.ID())
				}
			}
		})
	}
}
