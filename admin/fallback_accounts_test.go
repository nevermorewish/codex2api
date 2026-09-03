package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestFallbackAccountAPIKeepsSecretsAndReloadsPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fallback-admin.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	pool := auth.NewFallbackPool(store)
	handler := &Handler{store: store, db: db, fallbackPool: pool}

	secret := "sk-fallback-secret-value"
	createBody := []byte(`{"name":"backup","protocol":"openai_responses","base_url":"https://api.example.com/v1","api_key":"` + secret + `","model":"gpt-backup","proxy_url":"","concurrency":7,"enabled":true}`)
	createRecorder, createContext := fallbackAdminTestContext(http.MethodPost, "/api/admin/fallback/accounts", createBody)
	handler.CreateFallbackAccount(createContext)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	if strings.Contains(createRecorder.Body.String(), secret) {
		t.Fatal("create response leaked the fallback API key")
	}
	var created struct {
		Account fallbackAccountResponse `json:"account"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Account.ID <= 0 || !created.Account.HasAPIKey || created.Account.APIKeyMask == secret {
		t.Fatalf("created account response = %+v", created.Account)
	}
	if len(pool.Accounts()) != 1 {
		t.Fatalf("runtime fallback accounts = %d, want 1", len(pool.Accounts()))
	}

	updateBody := []byte(`{"name":"backup-renamed","api_key":""}`)
	updateRecorder, updateContext := fallbackAdminTestContext(http.MethodPut, "/api/admin/fallback/accounts/1", updateBody)
	updateContext.Params = gin.Params{{Key: "id", Value: "1"}}
	handler.UpdateFallbackAccount(updateContext)
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	row, err := db.GetFallbackAccount(context.Background(), created.Account.ID)
	if err != nil {
		t.Fatalf("GetFallbackAccount: %v", err)
	}
	if row.Name != "backup-renamed" || row.APIKey != secret {
		t.Fatalf("updated stored account = name:%q key:%q", row.Name, row.APIKey)
	}

	policyBody := []byte(`{"enabled":true,"relay_count":8,"queue_direct_fallback_threshold":12,"oversized_request_direct_fallback_enabled":true}`)
	policyRecorder, policyContext := fallbackAdminTestContext(http.MethodPut, "/api/admin/fallback/settings", policyBody)
	handler.UpdateFallbackSettings(policyContext)
	if policyRecorder.Code != http.StatusOK {
		t.Fatalf("policy status = %d, body=%s", policyRecorder.Code, policyRecorder.Body.String())
	}
	policy := pool.Policy()
	if !policy.Enabled || policy.RelayCount != 8 || policy.QueueDirectFallbackThreshold != 12 || !policy.OversizedRequestDirectFallbackEnabled {
		t.Fatalf("runtime fallback policy = %+v", policy)
	}
}

func TestCreateFallbackAccountRequiresAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "fallback-required.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	handler := &Handler{store: store, db: db, fallbackPool: auth.NewFallbackPool(store)}
	body := []byte(`{"name":"backup","protocol":"openai_responses","base_url":"https://api.example.com","model":"gpt-backup","proxy_url":"","concurrency":1}`)
	recorder, testContext := fallbackAdminTestContext(http.MethodPost, "/api/admin/fallback/accounts", body)
	handler.CreateFallbackAccount(testContext)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "api_key is required") {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func fallbackAdminTestContext(method, target string, body []byte) (*httptest.ResponseRecorder, *gin.Context) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return recorder, ctx
}
