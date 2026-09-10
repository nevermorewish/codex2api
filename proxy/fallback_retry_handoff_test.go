package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Exercise the real streaming handlers: reserving a fallback transition must
// also keep the last primary error private, otherwise headers/content commit
// before the handler can ever reach the fallback pool.
func TestPreContentFailureHandsOffAtMaxRetries(t *testing.T) {
	t.Run("relay", func(t *testing.T) { testPreContentFailureHandoff(t, false) })
	t.Run("codex", func(t *testing.T) { testPreContentFailureHandoff(t, true) })
	t.Run("codex_ws_executor", func(t *testing.T) { testPreContentFailureHandoff(t, true, true) })
}

func testPreContentFailureHandoff(t *testing.T, codexPrimary bool, websocketUpstream ...bool) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	settings.CodexPreflightSSEPassthrough = false
	useWebsocket := len(websocketUpstream) > 0 && websocketUpstream[0]
	settings.CodexForceWebsocket = useWebsocket
	settings.CodexOverloadPauseEnabled = false
	ApplyRuntimeSettings(settings)

	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		for _, tc := range []struct {
			name            string
			failure         string
			maxRetries      int
			relayCount      int
			wantPrimary     int
			wantFallback    int
			fallbackFails   bool
			disableFallback bool
		}{
			{"relay_one_ignores_high_retry_limit", "overload", 5, 1, 1, 1, false, false},
			{"relay_three_ignores_disabled_retries", "overload", 0, 3, 3, 1, false, false},
			{"relay_three_ignores_high_retry_limit", "overload", 5, 3, 3, 1, false, false},
			{"eof_uses_relay_count", "eof", 0, 2, 2, 1, false, false},
			{"http_500_uses_relay_count", "http500", 0, 2, 2, 1, false, false},
			{"rate_limit_uses_relay_count", "http429", 5, 2, 2, 1, false, false},
			{"fallback_failure_is_finite", "overload", 5, 2, 2, 1, true, false},
			{"deterministic_error", "bad_request", 2, 10, 1, 0, false, false},
			{"already_output", "after_content", 2, 10, 1, 0, false, false},
			{"no_fallback_uses_exact_retries", "overload", 2, 10, 3, 0, false, true},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var primaryKeys []string
				fallbackCalls := 0
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					primaryKeys = append(primaryKeys, r.Header.Get("Authorization"))
					mu.Unlock()
					switch tc.failure {
					case "http500", "http429", "bad_request":
						status := http.StatusInternalServerError
						body := `{"error":{"code":"server_error","message":"primary unavailable"}}`
						if tc.failure == "http429" {
							status = http.StatusTooManyRequests
							body = `{"error":{"code":"rate_limit_exceeded","message":"rate limited"}}`
						} else if tc.failure == "bad_request" {
							status = http.StatusBadRequest
							body = `{"error":{"code":"invalid_request","message":"invalid input"}}`
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, body)
					default:
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"primary\"}}\n\n")
						if tc.failure == "after_content" {
							_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"already-visible\"}\n\n")
							w.(http.Flusher).Flush()
						}
						if tc.failure != "eof" {
							_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"type\":\"service_unavailable_error\",\"message\":\"primary overloaded\"}}}\n\n")
						}
					}
				}))
				defer primary.Close()
				if useWebsocket {
					// WS executors expose their events to these HTTP handlers as an
					// SSE reader. Substitute only that boundary and exercise routing,
					// parsing, budgets and the real HTTP fallback request unchanged.
					previousExecute := WebsocketExecuteFunc
					defer func() { WebsocketExecuteFunc = previousExecute }()
					WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, primary.URL, bytes.NewReader(body))
						if err != nil {
							return nil, err
						}
						req.Header.Set("Authorization", "Bearer "+account.AccessToken)
						return http.DefaultClient.Do(req)
					}
				}
				if codexPrimary {
					previousResin := resinCfg.Load()
					defer resinCfg.Store(previousResin)
					SetResinConfig(&ResinConfig{BaseURL: primary.URL, PlatformName: "handoff-test"})
				}
				fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					fallbackCalls++
					mu.Unlock()
					if tc.fallbackFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"error":{"code":"server_error","message":"fallback unavailable"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback-recovered\"}\n\n"+
						"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fallback\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
				}))
				defer fallback.Close()
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: tc.maxRetries, MaxRateLimitRetries: 0, TransportRetryPolicy: "rotate"})
				store.SetMaxRetries(tc.maxRetries)
				store.SetMaxRateLimitRetries(0)
				defer store.Stop()
				var accounts []*auth.Account
				for i := 1; i <= 8; i++ {
					a := &auth.Account{DBID: int64(i), UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL,
						APIKey: fmt.Sprintf("sk-primary-%d", i), Models: []string{"gpt-5.4"}, PlanType: "api"}
					if codexPrimary {
						a = &auth.Account{DBID: int64(i), AccessToken: fmt.Sprintf("at-primary-%d", i), PlanType: "pro", AccountID: fmt.Sprintf("acct-%d", i)}
					}
					store.AddAccount(a)
					accounts = append(accounts, a)
				}
				pool := auth.NewFallbackPool(store)
				pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "backup", BaseURL: fallback.URL, APIKey: "sk-fallback", Enabled: true}})
				pool.SetPolicy(auth.FallbackPolicy{Enabled: !tc.disableFallback, RelayCount: tc.relayCount})
				h := NewHandler(store, nil, nil, nil)
				h.SetFallbackPool(pool)
				body := `{"model":"gpt-5.4","input":"hello","stream":true}`
				if endpoint != "/v1/responses" {
					body = `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":true}`
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("X-Codex2API-Affinity-Key", "retry-handoff-test")
				beforeMetrics := GetFallbackMetricsSnapshot()
				switch endpoint {
				case "/v1/responses":
					h.Responses(c)
				case "/v1/chat/completions":
					h.ChatCompletions(c)
				case "/v1/messages":
					h.Messages(c)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(primaryKeys) != tc.wantPrimary || fallbackCalls != tc.wantFallback {
					t.Fatalf("primary keys=%v fallback calls=%d; want primary=%d fallback=%d; status=%d body=%s", primaryKeys, fallbackCalls, tc.wantPrimary, tc.wantFallback, recorder.Code, recorder.Body.String())
				}
				afterMetrics := GetFallbackMetricsSnapshot()
				if afterMetrics.WSPrimaryAttempts != beforeMetrics.WSPrimaryAttempts || afterMetrics.FallbackHandoffCount-beforeMetrics.FallbackHandoffCount != uint64(tc.wantFallback) || afterMetrics.FallbackAttemptCount-beforeMetrics.FallbackAttemptCount != uint64(tc.wantFallback) {
					t.Fatalf("HTTP routing metrics mismatch: before=%+v after=%+v", beforeMetrics, afterMetrics)
				}
				if tc.wantFallback > 0 {
					wantSuccess, wantFailure := uint64(1), uint64(0)
					if tc.fallbackFails {
						wantSuccess, wantFailure = 0, 1
					}
					if afterMetrics.FallbackSuccessCount-beforeMetrics.FallbackSuccessCount != wantSuccess || afterMetrics.FallbackFailureCount-beforeMetrics.FallbackFailureCount != wantFailure {
						t.Fatalf("HTTP outcome metrics mismatch: before=%+v after=%+v", beforeMetrics, afterMetrics)
					}
				}
				seen := map[string]bool{}
				for _, key := range primaryKeys {
					if seen[key] {
						t.Fatalf("failed primary selected again despite available alternatives: %v", primaryKeys)
					}
					seen[key] = true
				}
				if tc.wantFallback > 0 && !tc.fallbackFails {
					if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "fallback-recovered") || strings.Contains(recorder.Body.String(), "primary overloaded") {
						t.Fatalf("failed attempt leaked or fallback output missing: status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					if got, _ := c.Get(contextFallbackAccountName); got != "backup" {
						t.Fatalf("fallback attribution=%v", got)
					}
				}
				for _, a := range append(accounts, pool.Accounts()...) {
					if a.GetActiveRequests() != 0 || a.GetOccupiedRequests() != 0 {
						t.Fatalf("account %d lease leaked", a.ID())
					}
				}
			})
		}
	}
}

func TestFallbackHandoffPreservesIndependentAndAccountBudgets(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: "http://example.invalid", APIKey: "test", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 20})
	for _, tc := range []struct {
		name                                   string
		relayCount, general, rate, accountRate int
		attempts, generalUsed, rateUsed        int
		wantFallback                           bool
	}{
		{"before_relay_count", 3, 0, 0, 0, 2, 2, 0, false},
		{"at_relay_count", 3, 20, 20, 20, 3, 3, 0, true},
		{"mixed_failures", 3, 0, 0, 0, 3, 1, 2, true},
		{"rate_limit_cannot_handoff_early", 3, 5, 0, 0, 2, 0, 2, false},
		{"unlimited_normal_budget_still_uses_relay_count", 3, -1, -1, -1, 3, 3, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := (&Handler{fallbackPool: pool}).newFallbackRouteState(nil)
			s.policy.RelayCount = tc.relayCount
			general, rate := s.retryBudgets(tc.general, tc.rate)
			if general != tc.relayCount || rate != tc.relayCount {
				t.Fatalf("fallback retry budgets = %d/%d, want relay count %d", general, rate, tc.relayCount)
			}
			if base := s.primaryRateLimitBudget(rate); base != tc.relayCount-1 {
				t.Fatalf("primary rate-limit retries=%d, want %d", base, tc.relayCount-1)
			}
			if accountRate := s.retryBudgetForAccount(tc.accountRate); accountRate != tc.relayCount {
				t.Fatalf("account rate-limit budget=%d, want relay count %d", accountRate, tc.relayCount)
			}
			for i := 0; i < tc.attempts; i++ {
				s.noteSelected(&auth.Account{DBID: int64(i + 1)})
			}
			s.activateAfterRetryBudget(tc.generalUsed, tc.rateUsed)
			if s.usingFallback() != tc.wantFallback {
				t.Fatalf("fallback=%v want=%v", s.usingFallback(), tc.wantFallback)
			}
			wantReason := ""
			if tc.wantFallback {
				wantReason = fallbackReasonRelayLimit
			}
			if s.reason != wantReason {
				t.Fatalf("reason=%q want=%q", s.reason, wantReason)
			}
		})
	}
	for _, mode := range []string{"disabled", "empty", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			p := auth.NewFallbackPool(store)
			p.SetPolicy(auth.FallbackPolicy{Enabled: mode != "disabled", RelayCount: 1})
			if mode != "empty" {
				p.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: "http://example.invalid", APIKey: "test", Enabled: true}})
			}
			s := (&Handler{fallbackPool: p}).newFallbackRouteState(func(*auth.Account) bool { return mode != "filtered" })
			general, rate := s.retryBudgets(5, 2)
			if general != 5 || rate != 2 {
				t.Fatalf("unavailable fallback changed normal budgets: %d/%d", general, rate)
			}
			s.noteSelected(&auth.Account{DBID: 1})
			s.activateAfterRetryBudget(1, 0)
			if s.usingFallback() {
				t.Fatal("unavailable fallback replaced an available primary")
			}
		})
	}
}
