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
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Reproduce a structural event followed by silence, with a healthy second account.
// The production threshold of 90 seconds is shortened to one second for the test.
func TestFirstTokenTimeoutSwitchesAccountAfterStructuralFrames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, stream), func(t *testing.T) {
				previousExec, previousSettings := WebsocketExecuteFunc, CurrentRuntimeSettings()
				t.Cleanup(func() { WebsocketExecuteFunc = previousExec; ApplyRuntimeSettings(previousSettings) })
				settings := DefaultRuntimeSettings()
				settings.FirstTokenTimeoutSec = 1
				settings.FirstTokenMode = FirstTokenModeLoose // Statistics must not disable the watchdog.
				settings.CodexForceWebsocket = true
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
					Enabled: true, Categories: []string{database.ContinuousRetryCategoryTransport}, MaxDurationSeconds: 10,
				}
				ApplyRuntimeSettings(settings)
				var attempts []int64
				var canceled atomic.Bool
				WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxy, key string, cfg *DeviceProfileConfig, headers http.Header, route string) (*http.Response, error) {
					attempts = append(attempts, account.ID())
					if len(attempts) == 1 {
						pr, pw := io.Pipe()
						go func() {
							defer pw.Close()
							_, _ = io.WriteString(pw, "data: {\"type\":\"response.created\",\"response\":{}}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n")
							<-ctx.Done()
							canceled.Store(true)
							_ = pw.CloseWithError(ctx.Err())
						}()
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
					}
					sse := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"retried\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"retried\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
				}
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1, TestConcurrency: 1, TestModel: "gpt-5.4", RetryIntervalMS: 5000})
				t.Cleanup(store.Stop)
				accounts := []*auth.Account{
					{DBID: 1, AccessToken: "ttft-1", PlanType: "pro", AccountID: "acct-1"},
					{DBID: 2, AccessToken: "ttft-2", PlanType: "pro", AccountID: "acct-2"},
				}
				for _, account := range accounts {
					store.AddAccount(account)
				}
				handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
				router := gin.New()
				handler.RegisterRoutes(router)
				body := fmt.Sprintf(`{"model":"gpt-5.4","stream":%v,"input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`, stream)
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body)).WithContext(ctx)
				req.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				started := time.Now()
				router.ServeHTTP(recorder, req)
				elapsed := time.Since(started)
				if !canceled.Load() || len(attempts) != 2 || attempts[0] == attempts[1] {
					t.Fatalf("expected canceled first attempt and another account: canceled=%v attempts=%v body=%s", canceled.Load(), attempts, recorder.Body.String())
				}
				if elapsed < time.Second || elapsed > 2500*time.Millisecond {
					t.Fatalf("failover elapsed=%s; expected timeout without retry delay", elapsed)
				}
				if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "retried") {
					t.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
				}
				for _, account := range accounts {
					if atomic.LoadInt64(&account.ActiveRequests) != 0 {
						t.Fatal("attempt leaked an account concurrency slot")
					}
				}
			})
		}
	}
}
