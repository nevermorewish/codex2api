package proxy

import (
	"context"
	"github.com/codex2api/database"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRiskControlGatewayBeforeExistingFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := riskcontrol.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := s.Config()
	cfg.Enabled = true
	cfg.Strategy = "keyword_only"
	cfg.Keywords = []string{"blocked-fixture"}
	if err = s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h := &Handler{riskControl: s}
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/images/generations", "/v1/messages", "/v1/videos/generations"} {
		t.Run(endpoint, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", endpoint, nil)
			c.Set(contextAPIKeyID, int64(12))
			var blocked bool
			switch endpoint {
			case "/v1/messages":
				blocked = h.inspectPromptFilterAnthropic(c, []byte(`{"messages":[{"role":"user","content":"blocked-fixture"}]}`), endpoint, "m")
			case "/v1/videos/generations":
				blocked = h.inspectPromptFilterTextOpenAI(c, "blocked-fixture", endpoint, "m")
			default:
				blocked = h.inspectPromptFilterOpenAI(c, []byte(`{"input":"blocked-fixture"}`), endpoint, "m")
			}
			if !blocked || w.Code != 403 {
				t.Fatalf("not intercepted %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestRiskControlWebSocketEachTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := riskcontrol.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := s.Config()
	cfg.Enabled = true
	cfg.Strategy = "keyword_only"
	cfg.Keywords = []string{"blocked-fixture"}
	if err = s.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h := &Handler{riskControl: s}
	r := gin.New()
	r.GET("/ws", func(c *gin.Context) {
		conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			blocked, _ := h.inspectPromptFilterOpenAIForWebSocket(c, conn, body, "/v1/responses", "m", "")
			if !blocked {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"ok":true}`))
			}
		}
	})
	server := httptest.NewServer(r)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i, text := range []string{"clean", "blocked-fixture"} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"response":{"input":"`+text+`"}}`)); err != nil {
			t.Fatal(err)
		}
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && !strings.Contains(string(body), `"ok"`) {
			t.Fatal(string(body))
		}
		if i == 1 && !strings.Contains(string(body), "content_policy_violation") {
			t.Fatal(string(body))
		}
	}
}
