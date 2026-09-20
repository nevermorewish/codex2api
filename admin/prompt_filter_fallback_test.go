package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestPromptFilterFallbackSettingsPersist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, body := range []string{`{"prompt_filter_enabled":true,"prompt_filter_mode":"fallback"}`, `{"prompt_filter_log_matches":true}`} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
		}
		var response settingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.PromptFilterMode != promptfilter.ModeFallback {
			t.Fatalf("response mode=%s", response.PromptFilterMode)
		}
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.PromptFilterMode != promptfilter.ModeFallback || store.GetPromptFilterConfig().Mode != promptfilter.ModeFallback {
		t.Fatal("fallback mode not persisted/applied")
	}
	reloaded := auth.NewStore(db, tc, persisted)
	defer reloaded.Stop()
	if reloaded.GetPromptFilterConfig().Mode != promptfilter.ModeFallback {
		t.Fatal("fallback mode lost on restart")
	}
}
