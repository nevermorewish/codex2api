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
	"github.com/gin-gonic/gin"
)

func TestFeishuSettingsPersistAcrossRestartAndPartialUpdates(t *testing.T) {
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
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	update := func(body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("update status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	want := proxy.FeishuAlertConfig{Enabled: true, AppID: "cli_test", AppSecret: "test-secret", ChatIDs: "oc_test", ErrorCodes: "400-599", FirstTokenTimeoutSeconds: 45}
	update(`{"feishu_alert_enabled":true,"feishu_app_id":"cli_test","feishu_app_secret":"test-secret","feishu_chat_ids":"oc_test","feishu_alert_error_codes":"","feishu_first_token_timeout_seconds":45}`)
	if got := proxy.ParseFeishuAlertConfig(proxy.CurrentRuntimeSettings().FeishuConfig); got != want {
		t.Fatal("saved Feishu configuration was not published to runtime")
	}

	// A stale instance saving unrelated settings must not erase the database value.
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	update(`{"site_name":"Updated site"}`)
	update(`{"feishu_app_secret":"","feishu_first_token_timeout_seconds":46}`)
	want.FirstTokenTimeoutSeconds = 46
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil || persisted == nil {
		t.Fatalf("load persisted settings: %v", err)
	}
	if got := proxy.ParseFeishuAlertConfig(persisted.FeishuConfig); got != want {
		t.Fatal("partial update did not preserve the saved Feishu configuration and secret")
	}
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	proxy.ApplyRuntimeSettingsFromSystem(persisted)
	if got := proxy.ParseFeishuAlertConfig(proxy.CurrentRuntimeSettings().FeishuConfig); got != want {
		t.Fatal("restart did not restore Feishu configuration")
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	handler.GetSettings(ctx)
	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || !response.FeishuAlertEnabled || response.FeishuAppID != want.AppID || response.FeishuAppSecret != want.AppSecret || !response.FeishuAppSecretConfigured || response.FeishuChatIDs != want.ChatIDs || response.FeishuAlertErrorCodes != want.ErrorCodes || response.FeishuFirstTokenTimeoutSeconds != want.FirstTokenTimeoutSeconds {
		t.Fatal("GET settings did not return the saved bot monitor configuration")
	}
}

func TestFeishuSettingsPersistenceFailureDoesNotPublish(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	before := proxy.CurrentRuntimeSettings().FeishuConfig
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"feishu_alert_enabled":true,"feishu_app_id":"not-persisted"}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("failed persistence returned status=%d", recorder.Code)
	}
	if proxy.CurrentRuntimeSettings().FeishuConfig != before {
		t.Fatal("failed persistence changed the runtime Feishu configuration")
	}
}
