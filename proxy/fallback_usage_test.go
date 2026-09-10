package proxy

import (
	"context"
	"fmt"
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
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Exercise actual handlers, transport, translation and usage persistence.
// A helper-only router misses checks absent from the relay branch and terminals
// that were already published before the usage check.
func TestFallbackUsageRequiredAcrossHTTPProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexForceWebsocket = false
	settings.CodexPreflightSSEPassthrough = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, content := range []bool{false, true} {
				for _, usageMode := range []string{"missing", "zero", "valid"} {
					t.Run(fmt.Sprintf("%s/stream=%t/content=%t/%s", endpoint, stream, content, usageMode), func(t *testing.T) {
						var calls atomic.Int32
						response := `{"id":"resp_usage","object":"response","status":"completed","output":[]`
						if usageMode == "zero" {
							response += `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`
						}
						if usageMode == "valid" {
							response += `,"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}`
						}
						response += `}`
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							if !stream && endpoint == "/v1/responses" {
								w.Header().Set("Content-Type", "application/json")
								body := response
								if content {
									body = strings.Replace(body, `"output":[]`, `"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible"}]}]`, 1)
								}
								_, _ = io.WriteString(w, body)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_usage\"}}\n\n")
							if content {
								_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\",\"output_index\":0,\"content_index\":0}\n\n")
								w.(http.Flusher).Flush()
							}
							_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
						}))
						defer upstream.Close()
						db, err := database.New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
						if err != nil {
							t.Fatal(err)
						}
						defer db.Close()
						db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
						store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
						defer store.Stop()
						pool := auth.NewFallbackPool(store)
						pool.Replace([]auth.FallbackAccountConfig{{ID: 9, Name: "usage-test", BaseURL: upstream.URL, APIKey: "test", Enabled: true}})
						pool.SetPolicy(auth.FallbackPolicy{Enabled: true, RelayCount: 3})
						h := NewHandler(store, db, nil, nil)
						h.SetFallbackPool(pool)
						body := fmt.Sprintf(`{"model":"gpt-5.4","input":"hello","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":%t}`, stream)
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
						switch endpoint {
						case "/v1/responses":
							h.Responses(c)
						case "/v1/chat/completions":
							h.ChatCompletions(c)
						case "/v1/messages":
							h.Messages(c)
						}
						wantStatus := http.StatusBadGateway
						if usageMode == "valid" || (stream && content) {
							wantStatus = http.StatusOK
						}
						if recorder.Code != wantStatus {
							t.Fatalf("HTTP status=%d want=%d body=%s", recorder.Code, wantStatus, recorder.Body.String())
						}
						if usageMode != "valid" {
							if !stream && (!gjson.ValidBytes(recorder.Body.Bytes()) || !gjson.GetBytes(recorder.Body.Bytes(), "error").IsObject()) {
								t.Fatalf("non-stream failure must be a JSON error: %s", recorder.Body.String())
							}
							if !strings.Contains(recorder.Body.String(), ErrorCodeUpstreamUsageMissing) {
								t.Fatalf("missing explicit failure: %s", recorder.Body.String())
							}
							for _, sentinel := range []string{`"type":"response.completed"`, "[DONE]", `"type":"message_stop"`} {
								if strings.Contains(recorder.Body.String(), sentinel) {
									t.Fatalf("success terminal leaked: %s", recorder.Body.String())
								}
							}
							if stream && content && !strings.Contains(recorder.Body.String(), "visible") {
								t.Fatal("partial output lost")
							}
						}
						db.FlushUsageLogs()
						logs, err := db.ListRecentUsageLogs(context.Background(), 10)
						if err != nil || len(logs) != 1 {
							t.Fatalf("logs=%+v err=%v", logs, err)
						}
						wantLogStatus, wantTokens := http.StatusBadGateway, 0
						if usageMode == "valid" {
							wantLogStatus, wantTokens = http.StatusOK, 10
						}
						if logs[0].StatusCode != wantLogStatus || logs[0].TotalTokens != wantTokens {
							t.Fatalf("log status/tokens=%d/%d want=%d/%d", logs[0].StatusCode, logs[0].TotalTokens, wantLogStatus, wantTokens)
						}
						if usageMode != "valid" && logs[0].UpstreamErrorKind != "usage_missing" {
							t.Fatalf("error kind=%q", logs[0].UpstreamErrorKind)
						}
						if calls.Load() != 1 {
							t.Fatalf("unexpected fallback retry: %d", calls.Load())
						}
						if pool.Accounts()[0].GetOccupiedRequests() != 0 {
							t.Fatal("fallback lease leaked")
						}
					})
				}
			}
		}
	}
}

func TestFallbackTerminalValidationBoundaries(t *testing.T) {
	for _, event := range []string{"response.completed", "response.incomplete"} {
		data := []byte(fmt.Sprintf(`{"type":%q,"response":{"id":"keep-id","status":"completed","usage":null}}`, event))
		typ, failed := validateFallbackTerminalEvent(&auth.Account{ExternalFallback: true}, event, data)
		if typ != "response.failed" || classifyResponseFailedOutcome(failed).logStatusCode != 502 || gjson.GetBytes(failed, "response.id").String() != "keep-id" {
			t.Fatalf("invalid failure: %s", failed)
		}
		againType, again := validateFallbackTerminalEvent(&auth.Account{ExternalFallback: true}, typ, failed)
		if againType != typ || string(again) != string(failed) {
			t.Fatal("validating an existing failure must not rewrite it")
		}
		_, unchanged := validateFallbackTerminalEvent(&auth.Account{DBID: 1}, event, data)
		if string(unchanged) != string(data) {
			t.Fatal("primary behavior changed")
		}
	}
}

func TestFallbackUsageRequiredOverWebSocket(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexWSHideErrors = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	for _, content := range []bool{false, true} {
		t.Run(fmt.Sprint(content), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if content {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n")
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ws_usage\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			defer upstream.Close()
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ws.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			defer store.Stop()
			pool := auth.NewFallbackPool(store)
			pool.Replace([]auth.FallbackAccountConfig{{ID: 9, BaseURL: upstream.URL, APIKey: "test", Enabled: true}})
			pool.SetPolicy(auth.FallbackPolicy{Enabled: true})
			h := NewHandler(store, db, nil, nil)
			h.SetFallbackPool(pool)
			done := make(chan struct{})
			router := gin.New()
			router.GET("/v1/responses", func(c *gin.Context) {
				defer close(done)
				conn, upgradeErr := responsesWSUpgrader.Upgrade(c.Writer, c.Request, nil)
				if upgradeErr != nil {
					return
				}
				defer conn.Close()
				_, body, readErr := conn.ReadMessage()
				if readErr == nil {
					_ = h.forwardResponsesWebSocketTurn(c, conn, body, "usage-ws", nil)
				}
			})
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var output strings.Builder
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					break
				}
				output.Write(data)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("WS handler did not finish")
			}
			if strings.Contains(output.String(), `"type":"response.completed"`) || !strings.Contains(output.String(), ErrorCodeUpstreamUsageMissing) {
				t.Fatalf("invalid WS failure: %s", output.String())
			}
			db.FlushUsageLogs()
			logs, err := db.ListRecentUsageLogs(context.Background(), 10)
			if err != nil || len(logs) != 1 {
				t.Fatalf("logs=%+v err=%v", logs, err)
			}
			if logs[0].StatusCode != 502 || logs[0].TotalTokens != 0 || logs[0].UpstreamErrorKind != "usage_missing" {
				t.Fatalf("log=%+v", logs[0])
			}
			if pool.Accounts()[0].GetOccupiedRequests() != 0 {
				t.Fatal("WS lease leaked")
			}
		})
	}
}
