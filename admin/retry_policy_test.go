package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestSettingsUnifiedRetryPolicy(t *testing.T) {
	h, db, _ := newResponseCacheSettingsAdminHandler(t)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	want := database.RequestRetryPolicy{Mode: database.RetryModeBeforeFirstToken, MaxAttempts: 4, TotalTimeoutSeconds: 123}
	put := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"retry_policy": want})
	if put.Code != 200 {
		t.Fatalf("PUT=%d %s", put.Code, put.Body.String())
	}
	if got := decodeResponseCacheSettingsResponse(t, put).RetryPolicy; got != want {
		t.Fatalf("PUT policy=%+v", got)
	}
	if got := proxy.CurrentRuntimeSettings().ContinuousRetryPolicy.RequestPolicy; got == nil || *got != want {
		t.Fatalf("runtime=%+v", got)
	}
	put = invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, map[string]any{"continuous_retry_catch_all": true})
	if put.Code != 200 {
		t.Fatalf("selector PUT=%d %s", put.Code, put.Body.String())
	}
	get := invokeResponseCacheSettingsAdmin(t, h, http.MethodGet, nil)
	if got := decodeResponseCacheSettingsResponse(t, get); got.RetryPolicy != want || !got.ContinuousRetryCatchAll {
		t.Fatalf("GET policy=%+v", got.RetryPolicy)
	}
	saved, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := database.ParseContinuousRetryPolicy(saved.ContinuousRetryPolicy).RequestPolicy; got == nil || *got != want {
		t.Fatalf("saved=%+v", got)
	}
}

func TestSettingsUnifiedRetryRejectsInvalidAndDeprecatedWrites(t *testing.T) {
	h, _, _ := newResponseCacheSettingsAdminHandler(t)
	for _, patch := range []map[string]any{
		{"max_retries": 3}, {"max_rate_limit_retries": 3}, {"continuous_retry_enabled": true}, {"continuous_retry_max_duration_seconds": 30},
		{"retry_policy": database.RequestRetryPolicy{Mode: "unknown", MaxAttempts: 3, TotalTimeoutSeconds: 300}},
		{"retry_policy": database.RequestRetryPolicy{Mode: "off", MaxAttempts: 0, TotalTimeoutSeconds: 300}},
		{"retry_policy": database.RequestRetryPolicy{Mode: "off", MaxAttempts: 21, TotalTimeoutSeconds: 300}},
		{"retry_policy": database.RequestRetryPolicy{Mode: "off", MaxAttempts: 1, TotalTimeoutSeconds: 0}},
		{"retry_policy": database.RequestRetryPolicy{Mode: "off", MaxAttempts: 1, TotalTimeoutSeconds: 901}},
	} {
		patch["site_name"] = "must not change"
		put := invokeResponseCacheSettingsAdmin(t, h, http.MethodPut, patch)
		if put.Code != 400 {
			t.Fatalf("patch=%v status=%d body=%s", patch, put.Code, put.Body.String())
		}
	}
}
