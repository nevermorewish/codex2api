package proxy

import (
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
	"github.com/tidwall/gjson"
)

func TestFallbackTakesOverExpiredPrimaryAcrossHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexForceWebsocket = false
	settings.CodexPreflightSSEPassthrough = false
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled: true, Categories: []string{"http_5xx", "response_failed", "transport"}, MaxDurationSeconds: 1,
	}
	ApplyRuntimeSettings(settings)
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			if stream && endpoint == "/v1/responses/compact" {
				continue
			}
			for _, phase := range []string{"headers", "body", "backoff"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", endpoint, stream, phase), func(t *testing.T) {
					var primaryCalls, fallbackCalls atomic.Int32
					primaryCanceled := make(chan struct{}, 1)
					releasePrimary := make(chan struct{})
					primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if primaryCalls.Add(1) == 1 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, `{"error":{"message":"primary overloaded","code":"server_is_overloaded"}}`)
							return
						}
						if phase == "body" {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"primary\"}}\n\n")
							w.(http.Flusher).Flush()
						}
						select {
						case <-r.Context().Done():
							primaryCanceled <- struct{}{}
						case <-releasePrimary:
						}
					}))
					defer primary.Close()
					defer close(releasePrimary)
					fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						fallbackCalls.Add(1)
						requestBody, _ := io.ReadAll(r.Body)
						if r.URL.Path == "/v1/responses/compact" {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"id":"fallback-compact","object":"response.compaction","output":[]}`)
							return
						}
						if !gjson.GetBytes(requestBody, "stream").Bool() {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"id":"resp_fallback","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+wsFallbackSuccess+"\n\n")
					}))
					defer fallback.Close()
					store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 10, MaxRateLimitRetries: 10})
					defer store.Stop()
					if phase == "backoff" {
						store.SetRetryIntervalMS(5000)
					}
					for id := int64(1); id <= 3; id++ {
						store.AddAccount(&auth.Account{DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: primary.URL,
							APIKey: "primary-key", Models: []string{"gpt-5.4"}, PlanType: "api"})
					}
					pool := auth.NewFallbackPool(store)
					pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: fallback.URL, APIKey: "fallback-key", Concurrency: 1, Enabled: true}})
					pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 10})
					occupied := pool.Acquire(nil, nil)
					if occupied == nil {
						t.Fatal("could not occupy fallback account")
					}
					defer store.Release(occupied)
					h := NewHandler(store, nil, nil, nil)
					h.SetFallbackPool(pool)
					body := fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","stream":%t}`, stream)
					if endpoint == "/v1/chat/completions" || endpoint == "/v1/messages" {
						body = fmt.Sprintf(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":%t}`, stream)
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					c.Request = httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)).WithContext(requestCtx)
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
					if fallbackCalls.Load() != 1 || recorder.Code != http.StatusOK {
						t.Fatalf("primary=%d fallback=%d status=%d body=%s", primaryCalls.Load(), fallbackCalls.Load(), recorder.Code, recorder.Body.String())
					}
					if got := c.GetString(contextFallbackReason); got != fallbackReasonRetryDeadline {
						t.Fatalf("fallback reason = %q", got)
					}
					if c.GetInt(AccessLogStatusContextKey) >= 400 {
						t.Fatal("primary timeout hides the successful fallback in the access log")
					}
					if strings.Contains(recorder.Body.String(), "primary overloaded") || strings.Contains(recorder.Body.String(), continuousRetryTimeoutMessage) {
						t.Fatalf("primary error was appended to fallback response: %s", recorder.Body.String())
					}
					if phase != "backoff" {
						select {
						case <-primaryCanceled:
						case <-time.After(time.Second):
							t.Fatal("expired primary request was not canceled")
						}
					}
					for _, account := range store.Accounts() {
						if account.GetOccupiedRequests() != 0 {
							t.Fatalf("primary account %d leaked a lease", account.ID())
						}
					}
					if occupied.GetOccupiedRequests() != 1 {
						t.Fatalf("fallback leases = %d, want the original occupied lease only", occupied.GetOccupiedRequests())
					}
				})
			}
		}
	}
}

func TestFallbackDeadlineHandoffRespectsRequestBoundaries(t *testing.T) {
	for _, scenario := range []string{"eligible", "client_canceled", "output_committed", "disabled", "no_eligible_model", "fallback_attempted", "pinned_domain"} {
		t.Run(scenario, func(t *testing.T) {
			store := newFallbackQueueTestStore()
			defer store.Stop()
			pool := newFallbackQueueTestPool(store, 0)
			state := (&Handler{store: store, fallbackPool: pool}).newFallbackRouteState(nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(parent)
			stop := installContinuousRetryHTTPDeadline(c, database.ContinuousRetryPolicy{Enabled: true}, continuousRetryProtocolResponses)
			defer stop()
			continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
			switch scenario {
			case "client_canceled":
				cancel()
			case "output_committed":
				_, _ = c.Writer.WriteString("data: visible output\n\n")
			case "disabled":
				state.policy.Enabled = false
			case "no_eligible_model":
				state.filter = func(*auth.Account) bool { return false }
			case "fallback_attempted":
				state.fallbackAttempted = true
			case "pinned_domain":
				state = &fallbackRouteState{}
			}
			c.Set(contextFallbackDeadlineState, state)
			usage := &database.UsageLogInput{AccountID: 1, StatusCode: http.StatusGatewayTimeout, AttemptIndex: 2}
			(&Handler{store: store}).logUsageForRequest(c, usage)
			if usage.IsRetryAttempt != (scenario == "eligible") {
				t.Fatalf("primary timeout retry marker = %t", usage.IsRetryAttempt)
			}
			got := handoffPrimaryDeadlineToFallback(c, state)
			if got != (scenario == "eligible") {
				t.Fatalf("handoff = %t", got)
			}
			if got {
				if c.Request.Context().Err() != nil || !state.active {
					t.Fatal("fallback inherited the canceled primary context")
				}
				cancel()
				if c.Request.Context().Err() != context.Canceled {
					t.Fatal("fallback lost client cancellation")
				}
			}
		})
	}
}

func TestFallbackSelectedAfterDeadlineUsesLiveClientContext(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	policy := database.ContinuousRetryPolicy{Enabled: true}
	stop := installContinuousRetryHTTPDeadline(c, policy, continuousRetryProtocolResponses)
	defer stop()
	continuousRetryDeadlineForContext(c.Request.Context()).cancel(errContinuousRetryDeadlineExceeded)
	prepareFallbackAttempt(c, &auth.Account{DBID: -1, ExternalFallback: true}, 1, 1, policy)
	if c.Request.Context().Err() != nil {
		t.Fatal("fallback selected at the end of the primary availability wait inherited its timeout")
	}
}

func TestFallbackTakesOverExpiredCodexWebsocketUpstream(t *testing.T) {
	previous, previousExecute := CurrentRuntimeSettings(), WebsocketExecuteFunc
	t.Cleanup(func() {
		ApplyRuntimeSettings(previous)
		WebsocketExecuteFunc = previousExecute
	})
	settings := previous
	settings.CodexForceWebsocket = true
	settings.CodexPreflightSSEPassthrough = false
	settings.CodexOverloadPauseEnabled = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled: true, Categories: []string{"http_5xx", "response_failed"}, MaxDurationSeconds: 1,
	}
	ApplyRuntimeSettings(settings)
	var primaryCalls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, _ *auth.Account, _ []byte, _ string, _ string, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		var body io.ReadCloser
		if primaryCalls.Add(1) == 1 {
			body = io.NopCloser(strings.NewReader("data: " + wsFallbackOverload + "\n\n"))
		} else {
			body = &continuousRetryDeadlineBlockingBody{ctx: ctx, started: make(chan struct{}, 1)}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}
	var path, authorization string
	var body []byte
	upstream := newOpenAIResponsesSSEUpstream(&path, &authorization, &body)
	defer upstream.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 10, MaxRateLimitRetries: 10})
	defer store.Stop()
	for id := int64(1); id <= 3; id++ {
		store.AddAccount(&auth.Account{DBID: id, AccessToken: "primary-token", AccountID: fmt.Sprint(id), PlanType: "pro"})
	}
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: upstream.URL, APIKey: "fallback-key", Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 10})
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(ctx)
	h.Responses(c)
	if primaryCalls.Load() != 2 || recorder.Code != http.StatusOK || authorization != "Bearer fallback-key" || !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("primary=%d status=%d fallback=%q body=%s", primaryCalls.Load(), recorder.Code, path, recorder.Body.String())
	}
	for _, account := range append(store.Accounts(), pool.Accounts()...) {
		if account.GetOccupiedRequests() != 0 {
			t.Fatalf("account %d leaked a lease", account.ID())
		}
	}
}

func TestFallbackServesWhenPrimaryAndFallbackConcurrencyAreFull(t *testing.T) {
	var path, authorization string
	var body []byte
	upstream := newOpenAIResponsesSSEUpstream(&path, &authorization, &body)
	defer upstream.Close()
	store := newFallbackQueueTestStore()
	defer store.Stop()
	primary := store.NextExcluding(0, nil)
	if primary == nil {
		t.Fatal("could not occupy primary account")
	}
	defer store.Release(primary)
	const session = "full-primary-and-fallback"
	store.BindSessionAffinity(sessionAffinityKey(session, 0), primary, "")
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, BaseURL: upstream.URL, APIKey: "fallback-key", Concurrency: 1, Enabled: true}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3})
	busyFallback := pool.Acquire(nil, nil)
	if busyFallback == nil {
		t.Fatal("could not occupy fallback account")
	}
	defer store.Release(busyFallback)
	h := NewHandler(store, nil, nil, nil)
	h.SetFallbackPool(pool)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	c.Request.Header.Set("Session_id", session)
	h.Responses(c)
	if recorder.Code != http.StatusOK || authorization != "Bearer fallback-key" || !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("status=%d upstream=%q body=%s", recorder.Code, path, recorder.Body.String())
	}
	if primary.GetOccupiedRequests() != 1 || busyFallback.GetOccupiedRequests() != 1 {
		t.Fatal("request leaked a lease or released another request's lease")
	}
}
