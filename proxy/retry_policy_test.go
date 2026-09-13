package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func unifiedPolicy(mode string, attempts, seconds int) database.ContinuousRetryPolicy {
	p := database.DefaultContinuousRetryPolicy()
	p.RequestPolicy = &database.RequestRetryPolicy{Mode: mode, MaxAttempts: attempts, TotalTimeoutSeconds: seconds}
	return database.NormalizeContinuousRetryPolicy(p)
}

func unifiedRouter(t *testing.T, policy database.ContinuousRetryPolicy) (*gin.Engine, *auth.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousExec, previous := WebsocketExecuteFunc, CurrentRuntimeSettings()
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec; ApplyRuntimeSettings(previous) })
	s := DefaultRuntimeSettings()
	s.CodexForceWebsocket = true
	s.FirstTokenTimeoutMode = "first_token"
	s.FirstTokenTimeoutSec = 1
	s.ContinuousRetryPolicy = policy
	ApplyRuntimeSettings(s)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 10, MaxRateLimitRetries: 10, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	for id := int64(1); id <= 4; id++ {
		store.AddAccount(&auth.Account{DBID: id, AccessToken: fmt.Sprint("token-", id), AccountID: fmt.Sprint("account-", id), PlanType: "pro"})
	}
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	r := gin.New()
	h.RegisterRoutes(r)
	return r, store
}

func unifiedRequest(endpoint string) *http.Request {
	req := httptest.NewRequest("POST", endpoint, strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

const recoveredStream = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"recovered\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"

type firstTokenRecorder struct {
	*httptest.ResponseRecorder
	seen chan struct{}
	once sync.Once
}

func (w *firstTokenRecorder) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), "original-content") {
		w.once.Do(func() { close(w.seen) })
	}
}

func TestUnifiedRetryStreamingBoundary(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		for _, mode := range []string{database.RetryModeBeforeFirstToken, database.RetryModeFullResponse, database.RetryModeOff} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				router, _ := unifiedRouter(t, unifiedPolicy(mode, 3, 5))
				writer := &firstTokenRecorder{ResponseRecorder: httptest.NewRecorder(), seen: make(chan struct{})}
				var calls atomic.Int32
				var deliveredBeforeEnd atomic.Bool
				WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxy, key string, cfg *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
					if calls.Add(1) > 1 {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(recoveredStream))}, nil
					}
					pr, pw := io.Pipe()
					go func() {
						_, _ = io.WriteString(pw, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"original-content\"}\n\n")
						select {
						case <-writer.seen:
							deliveredBeforeEnd.Store(true)
						case <-time.After(150 * time.Millisecond):
						case <-ctx.Done():
						}
						_ = pw.CloseWithError(io.ErrUnexpectedEOF)
					}()
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
				}
				router.ServeHTTP(writer, unifiedRequest(endpoint))
				body := writer.Body.String()
				if mode == database.RetryModeFullResponse {
					if calls.Load() != 2 || deliveredBeforeEnd.Load() || strings.Contains(body, "original-content") || !strings.Contains(body, "recovered") {
						t.Fatalf("buffered delivery calls=%d early=%v body=%s", calls.Load(), deliveredBeforeEnd.Load(), body)
					}
				} else {
					if calls.Load() != 1 || !deliveredBeforeEnd.Load() || !strings.Contains(body, "original-content") || strings.Contains(body, "recovered") || !strings.Contains(body, "upstream_stream_break") && !strings.Contains(body, "prematurely") {
						t.Fatalf("stream boundary calls=%d early=%v body=%s", calls.Load(), deliveredBeforeEnd.Load(), body)
					}
				}
			})
		}
	}
}

func TestUnifiedRetryFirstTokenFailover(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			router, _ := unifiedRouter(t, unifiedPolicy(database.RetryModeBeforeFirstToken, 2, 4))
			var accounts []int64
			WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, b []byte, s, p, k string, cfg *DeviceProfileConfig, h http.Header, r string) (*http.Response, error) {
				accounts = append(accounts, a.ID())
				if len(accounts) > 1 {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(recoveredStream))}, nil
				}
				pr, pw := io.Pipe()
				go func() {
					_, _ = io.WriteString(pw, "data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n")
					<-ctx.Done()
					_ = pw.CloseWithError(ctx.Err())
				}()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: pr}, nil
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, unifiedRequest(endpoint))
			if len(accounts) != 2 || accounts[0] == accounts[1] || !strings.Contains(w.Body.String(), "recovered") {
				t.Fatalf("accounts=%v body=%s", accounts, w.Body.String())
			}
		})
	}
}

func TestUnifiedRetryMixedFailuresShareAttemptLimit(t *testing.T) {
	for _, mode := range []string{database.RetryModeOff, database.RetryModeBeforeFirstToken, database.RetryModeFullResponse} {
		t.Run(mode, func(t *testing.T) {
			router, _ := unifiedRouter(t, unifiedPolicy(mode, 3, 4))
			calls := 0
			WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, b []byte, s, p, k string, cfg *DeviceProfileConfig, h http.Header, r string) (*http.Response, error) {
				calls++
				status := 503
				if calls%2 == 1 {
					status = 429
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`))}, nil
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, unifiedRequest("/v1/responses"))
			want := 3
			if mode == database.RetryModeOff {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d body=%s", calls, want, w.Body.String())
			}
		})
	}
}

func TestUnifiedRetryDeadlineIncludesElapsedTimeAndFirstAttempt(t *testing.T) {
	for _, mode := range []string{database.RetryModeOff, database.RetryModeBeforeFirstToken, database.RetryModeFullResponse} {
		t.Run(mode, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set("request_context", &api.RequestContext{StartTime: time.Now().Add(-time.Second)})
			stop := installContinuousRetryDeadlineContext(c, unifiedPolicy(mode, 3, 1))
			defer stop()
			if !continuousRetryDeadlineActive(c.Request.Context()) && !continuousRetryDeadlineExceeded(c.Request.Context()) {
				t.Fatal("deadline not activated on initial request")
			}
			select {
			case <-c.Request.Context().Done():
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("first attempt not bounded")
			}
			if !writeContinuousRetryTimeoutResponse(c, continuousRetryProtocolResponses) || !strings.Contains(w.Body.String(), "request_deadline_exceeded") {
				t.Fatalf("response=%s", w.Body.String())
			}
		})
	}
}
