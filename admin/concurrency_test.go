package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestConcurrencySnapshotShowsDisabledFallbackWithoutCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "concurrency.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID: 1, Name: "primary", UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: "https://primary.example.com", APIKey: "sk-primary",
		Models: []string{"gpt-5.4"}, PlanType: "api",
	})
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{
		ID: 9, Name: "backup", BaseURL: "https://fallback.example.com", APIKey: "sk-fallback",
		Model: "gpt-backup", Concurrency: 7, Enabled: true,
	}})
	pool.SetPolicy(auth.FallbackPolicy{Enabled: false, RelayCount: 3, QueueDirectFallbackThreshold: 5})
	handler := &Handler{store: store, db: db, fallbackPool: pool}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/concurrency", nil)
	handler.GetConcurrencySnapshot(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Capacity int64                   `json:"capacity"`
		Accounts []concurrencyAccountRow `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Accounts) != 2 {
		t.Fatalf("accounts = %d, want primary and fallback", len(response.Accounts))
	}
	if response.Capacity != 2 {
		t.Fatalf("capacity = %d, want only primary capacity 2", response.Capacity)
	}
	fallback := response.Accounts[1]
	if !fallback.Fallback || fallback.ID != 9 || fallback.Available {
		t.Fatalf("fallback row = %+v", fallback)
	}
}
