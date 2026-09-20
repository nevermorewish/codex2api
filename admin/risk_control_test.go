package admin

import (
	"context"
	"github.com/codex2api/database"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRiskControlAdminConfigAndAuth(t *testing.T) {
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
	h := &Handler{riskControl: s, adminSecretEnv: "risk-admin-test"}
	r := gin.New()
	group := r.Group("/api/admin")
	group.Use(h.adminAuthMiddleware())
	h.registerRiskControlRoutes(group)
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/admin/risk-control"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if !s.Config().FallbackOnBlock {
		t.Fatal("fallback switch should default on")
	}
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"fallback_on_block_enabled":false}`, false},
		{`{"sample_rate":50}`, false}, // Omitted field must preserve an explicit false.
		{`{"fallback_on_block_enabled":true}`, true},
	} {
		rec := call("PUT", "/config", test.body, "risk-admin-test")
		expected := `"fallback_on_block_enabled":false`
		if test.want {
			expected = `"fallback_on_block_enabled":true`
		}
		if rec.Code != 200 || s.Config().FallbackOnBlock != test.want || !strings.Contains(rec.Body.String(), expected) {
			t.Fatalf("switch patch: %d %s", rec.Code, rec.Body.String())
		}
		stored, err := db.LoadRiskConfig(context.Background())
		if err != nil || stored.FallbackOnBlock != test.want {
			t.Fatalf("switch persistence: %+v %v", stored, err)
		}
	}
	for _, route := range []struct{ method, path string }{{"GET", "/bans"}, {"POST", "/keys/1/unban"}, {"POST", "/api-keys/legacy/test"}, {"DELETE", "/api-keys/legacy"}} {
		if rec := call(route.method, route.path, "", "risk-admin-test"); rec.Code != 404 {
			t.Fatalf("removed route: %d", rec.Code)
		}
	}
	if rec := call("PUT", "/config", `{"auto_ban_enabled":true,"ban_threshold":1,"violation_window_hours":24}`, "risk-admin-test"); rec.Code != 200 || strings.Contains(rec.Body.String(), "auto_ban_enabled") || s.Config().AutoBan {
		t.Fatalf("legacy patch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call("GET", "/config", "", ""); rec.Code != 401 {
		t.Fatalf("auth %d %s", rec.Code, rec.Body.String())
	}
	rec := call("PUT", "/config", `{"enabled":true,"keyword_blocking_mode":"keyword_only","blocked_keywords":["blocked"],"api_keys":["private-test-secret"],"smtp_password":"smtp-private-secret"}`, "risk-admin-test")
	if rec.Code != 200 {
		t.Fatalf("save %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private-test-secret") || strings.Contains(rec.Body.String(), "smtp-private-secret") {
		t.Fatal("secret leaked")
	}
	rec = call("PUT", "/config", `{"sample_rate":30}`, "risk-admin-test")
	if rec.Code != 200 || len(s.Config().APIKeys) != 0 || s.Config().SMTPPassword != "" {
		t.Fatal("partial update lost secrets")
	}
	rec = call("PUT", "/config", `{"worker_count":0}`, "risk-admin-test")
	if rec.Code != 400 || s.Config().Workers == 0 {
		t.Fatal("invalid config changed runtime")
	}
	for _, body := range []string{`null`, `[]`, `{"unknown_field":true}`, `{} {}`} {
		if rec := call("PUT", "/config", body, "risk-admin-test"); rec.Code != 400 {
			t.Fatalf("invalid patch %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec = call("POST", "/test", `{"kind":"keyword","text":"BLOCKED"}`, "risk-admin-test")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"matched_keyword":"blocked"`) {
		t.Fatal(rec.Body.String())
	}
	rec = call("DELETE", "/hashes", "", "risk-admin-test")
	if rec.Code != 400 {
		t.Fatal("missing destructive confirmation")
	}
	rec = call("PUT", "/config", `{"api_keys":[]}`, "risk-admin-test")
	if rec.Code != 200 || len(s.Config().APIKeys) != 0 {
		t.Fatal("explicit clear failed")
	}
	for _, test := range []struct {
		body string
		want string
	}{
		{`{"model_audit":{"nodes":[{"id":"a","name":"A","enabled":true,"base_url":"http://localhost:9999","model":"custom","api_key":"node-private-secret","timeout_ms":40000,"max_input_chars":400000}]}}`, "node-private-secret"},
		{`{"model_audit":{"nodes":[{"id":"a","name":"A","enabled":true,"base_url":"http://localhost:9999","model":"custom","timeout_ms":40000,"max_input_chars":400000}]}}`, "node-private-secret"},
		{`{"model_audit":{"nodes":[{"id":"b","name":"B","enabled":true,"base_url":"http://localhost:9999","model":"custom","timeout_ms":40000,"max_input_chars":400000}]}}`, ""},
		{`{"model_audit":{"nodes":[{"id":"b","name":"B","enabled":true,"base_url":"http://localhost:9999","model":"custom","api_key":"temporary","timeout_ms":40000,"max_input_chars":400000}]}}`, "temporary"},
		{`{"model_audit":{"nodes":[{"id":"b","name":"B","enabled":true,"base_url":"http://localhost:9999","model":"custom","clear_api_key":true,"timeout_ms":40000,"max_input_chars":400000}]}}`, ""},
	} {
		rec = call("PUT", "/config", test.body, "risk-admin-test")
		if rec.Code != 200 || s.Config().Audit.Nodes[0].APIKey != test.want {
			t.Fatalf("node update %d %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "node-private-secret") || strings.Contains(rec.Body.String(), `"api_key":`) {
			t.Fatal("node secret leaked")
		}
	}
}
