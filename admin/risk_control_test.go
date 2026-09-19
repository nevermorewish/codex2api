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
	if rec.Code != 200 || len(s.Config().APIKeys) != 1 || s.Config().SMTPPassword != "smtp-private-secret" {
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
}
