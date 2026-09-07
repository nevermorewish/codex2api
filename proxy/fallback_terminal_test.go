package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestFallbackErrorIsTerminalAcrossHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, mode := range []string{"finite", "selected", "catch_all"} {
		settings := previous
		settings.CodexForceWebsocket = false
		settings.CodexPreflightSSEPassthrough = false
		settings.CodexOverloadPauseEnabled = false
		settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
		if mode != "finite" {
			settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
				Enabled: true, CatchAll: mode == "catch_all",
				Categories: []string{"http_429", "http_5xx", "response_failed", "stream_error", "transport"},
			}
		}
		ApplyRuntimeSettings(settings)
		for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, failure := range []string{"http503", "http429", "http401", "metadata403", "encrypted400", "quota429", "failed", "error", "eof", "transport", "handoff"} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", mode, endpoint, stream, failure), func(t *testing.T) {
						var primaryCalls, fallbackCalls atomic.Int32
						primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							primaryCalls.Add(1)
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, `{"error":{"code":"server_error","message":"primary failure"}}`)
						}))
						defer primary.Close()
						wantStatus := 0
						wantError := "fallback failure"
						fallbackBody := ""
						switch failure {
						case "http503", "handoff":
							wantStatus = 503
						case "http429":
							wantStatus = 429
						case "http401":
							wantStatus = 401
						case "metadata403":
							wantStatus = 403
							fallbackBody = `{"error":{"code":"codex_access_restricted","message":"fallback failure: official client required"}}`
						case "encrypted400":
							wantStatus = 400
							fallbackBody = `{"error":{"code":"invalid_encrypted_content","message":"fallback failure: invalid encrypted content"}}`
						case "quota429":
							wantStatus = 429
							fallbackBody = `{"error":{"code":"usage_limit_reached","message":"fallback failure: quota exhausted","resets_in_seconds":60}}`
						case "eof":
							wantError = ""
						case "transport":
							wantError = ""
						}
						if wantStatus != 0 && fallbackBody == "" {
							fallbackBody = `{"error":{"code":"fallback_error","message":"fallback failure","detail":"retain this detail"}}`
						}
						fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if fallbackCalls.Add(1) > 1 {
								// A retry succeeds, making accidental retries observable without
								// keeping a continuous-retry regression running indefinitely.
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: "+wsFallbackSuccess+"\n\n")
								return
							}
							if failure == "transport" {
								conn, _, err := w.(http.Hijacker).Hijack()
								if err == nil {
									_ = conn.Close()
								}
								return
							}
							if wantStatus != 0 {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(wantStatus)
								_, _ = io.WriteString(w, fallbackBody)
								return
							}
							if !stream && endpoint == "/v1/responses" && (failure == "failed" || failure == "error") {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, `{"id":"fallback","status":"failed","error":{"code":"server_error","message":"fallback failure"}}`)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fallback\"}}\n\n")
							if failure == "failed" {
								_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fallback failure\"}}}\n\n")
							} else if failure == "error" {
								_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"server_error\",\"message\":\"fallback failure\"}}\n\n")
							}
						}))
						defer fallback.Close()
						store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
						defer store.Stop()
						store.SetMaxRetries(10)
						store.SetMaxRateLimitRetries(10)
						var accounts []*auth.Account
						if failure == "handoff" {
							for i := 1; i <= 6; i++ {
								a := &auth.Account{DBID: int64(i), UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL, APIKey: fmt.Sprintf("primary-%d", i), Models: []string{"gpt-5.4"}, PlanType: "api"}
								store.AddAccount(a)
								accounts = append(accounts, a)
							}
						}
						pool := auth.NewFallbackPool(store)
						pool.Replace([]auth.FallbackAccountConfig{
							{ID: 101, BaseURL: fallback.URL, APIKey: "backup-one", Enabled: true},
							{ID: 102, BaseURL: fallback.URL, APIKey: "backup-two", Enabled: true},
						})
						pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3})
						h := NewHandler(store, nil, nil, nil)
						h.SetFallbackPool(pool)
						body := fmt.Sprintf(`{"model":"gpt-5.4","input":[{"type":"reasoning","encrypted_content":"invalid-ciphertext"},{"role":"user","content":"hello"}],"stream":%t}`, stream)
						if endpoint != "/v1/responses" {
							body = fmt.Sprintf(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":%t}`, stream)
						}
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
						ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
						defer cancel()
						c.Request = c.Request.WithContext(ctx)
						switch endpoint {
						case "/v1/responses":
							h.Responses(c)
						case "/v1/chat/completions":
							h.ChatCompletions(c)
						case "/v1/messages":
							h.Messages(c)
						}
						wantPrimary := int32(0)
						if failure == "handoff" {
							wantPrimary = 3
						}
						if primaryCalls.Load() != wantPrimary || fallbackCalls.Load() != 1 {
							t.Fatalf("calls primary=%d fallback=%d want=%d/1; status=%d body=%s", primaryCalls.Load(), fallbackCalls.Load(), wantPrimary, recorder.Code, recorder.Body.String())
						}
						wantReason := fallbackReasonNoEligible
						if wantPrimary == 3 {
							wantReason = fallbackReasonRelayLimit
						}
						if got := c.GetString(contextFallbackReason); got != wantReason {
							t.Fatalf("fallback reason = %q, want %q", got, wantReason)
						}
						if wantStatus != 0 && recorder.Code != wantStatus {
							t.Fatalf("status=%d want=%d; body=%s", recorder.Code, wantStatus, recorder.Body.String())
						}
						if wantError != "" && !strings.Contains(recorder.Body.String(), wantError) {
							t.Fatalf("fallback error missing: %s", recorder.Body.String())
						}
						if wantStatus != 0 && endpoint != "/v1/messages" && recorder.Body.String() != fallbackBody {
							t.Fatalf("fallback error body changed: got=%s want=%s", recorder.Body.String(), fallbackBody)
						}
						for _, a := range append(accounts, pool.Accounts()...) {
							if a.GetOccupiedRequests() != 0 || a.GetActiveRequests() != 0 {
								t.Fatalf("account %d lease leaked", a.ID())
							}
						}
					})
				}
			}
		}
	}
}

func TestFallbackStopsPrimaryRetryDeadline(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	stop := installContinuousRetryDeadlineContext(c, policy)
	defer stop()
	deadline := continuousRetryDeadlineForContext(c.Request.Context())
	deadline.duration = 20 * time.Millisecond
	deadline.Activate()
	general, rate, policy := prepareFallbackAttempt(c, &auth.Account{DBID: -1, ExternalFallback: true}, -1, -1, policy)
	if general != 0 || rate != 0 || policy.Enabled || continuousRetryPolicyForRequest(c).Enabled {
		t.Fatalf("fallback inherited retry policy: %d/%d %+v", general, rate, policy)
	}
	time.Sleep(40 * time.Millisecond)
	if c.Request.Context().Err() != nil || continuousRetryDeadlineActive(c.Request.Context()) {
		t.Fatal("primary retry deadline canceled the terminal fallback attempt")
	}
}
