package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestUpdateFirstTokenTimeoutSettingsPersistsPerModelBrackets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	// 这条路径会直接改写进程级 runtime settings，用例结束必须还原。
	previousSettings := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousSettings) })
	h := &Handler{db: db}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/api/admin/settings/first-token-timeouts", strings.NewReader(`{
		"under_50kb": 11, "under_100kb": 22, "under_200kb": 33, "under_500kb": 44, "over_500kb": 55,
		"model_timeouts": {"gpt-6-astra": {"under_50kb": 90, "over_500kb": 300}}
	}`))
	h.UpdateFirstTokenTimeoutSettings(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "model_timeouts.gpt-6-astra.over_500kb").Int(); got != 300 {
		t.Fatalf("response over_500kb = %d body=%s", got, body)
	}
	// 未覆盖的档位在响应里就是补齐后的全局值，前端表格不需要自己算回落。
	if got := gjson.GetBytes(body, "model_timeouts.gpt-6-astra.under_100kb").Int(); got != 22 {
		t.Fatalf("response under_100kb = %d, want 22 (global)", got)
	}

	// 落库的与回显的一致。
	stored, err := db.GetFirstTokenTimeoutSettings(c.Request.Context())
	if err != nil {
		t.Fatalf("GetFirstTokenTimeoutSettings: %v", err)
	}
	if stored.ModelTimeouts["gpt-6-astra"][database.FirstTokenSizeOver500KB] != 300 {
		t.Fatalf("stored row = %#v", stored.ModelTimeouts)
	}

	// 立即生效：runtime settings 换成同一份。
	effective := proxy.CurrentRuntimeSettings().FirstTokenSizeTimeouts
	if effective.ModelTimeouts["gpt-6-astra"][database.FirstTokenSizeOver500KB] != 300 {
		t.Fatalf("runtime settings not updated: %#v", effective.ModelTimeouts)
	}
}

// 越界值与空白模型名必须在响应里如实消失，而不是回显"已保存"却在库里查无此配置。
func TestUpdateFirstTokenTimeoutSettingsRejectsInvalidBrackets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}

	cases := []struct {
		name string
		body string
	}{
		{"global out of range", `{"under_50kb":0,"under_100kb":22,"under_200kb":33,"under_500kb":44,"over_500kb":55,"model_timeouts":{}}`},
		{"model out of range", `{"under_50kb":11,"under_100kb":22,"under_200kb":33,"under_500kb":44,"over_500kb":55,"model_timeouts":{"gpt-6-astra":{"under_50kb":601}}}`},
		{"blank model", `{"under_50kb":11,"under_100kb":22,"under_200kb":33,"under_500kb":44,"over_500kb":55,"model_timeouts":{"  ":{"under_50kb":30}}}`},
		{"unknown bracket", `{"under_50kb":11,"under_100kb":22,"under_200kb":33,"under_500kb":44,"over_500kb":55,"model_timeouts":{"gpt-6-astra":{"nope":30}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("PUT", "/api/admin/settings/first-token-timeouts", strings.NewReader(tc.body))
			h.UpdateFirstTokenTimeoutSettings(c)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// 全部被拒后，库里仍是默认档位表。
	stored, err := db.GetFirstTokenTimeoutSettings(t.Context())
	if err != nil {
		t.Fatalf("GetFirstTokenTimeoutSettings: %v", err)
	}
	if stored.Under50KB != 10 || len(stored.ModelTimeouts) != 0 {
		t.Fatalf("rejected writes must not change stored settings: %#v", stored)
	}
}

func TestGetFirstTokenTimeoutSettingsReturnsFullModelRows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}

	settings := database.DefaultFirstTokenTimeoutSettings()
	settings.Under100KB = 22
	settings.ModelTimeouts = map[string]database.FirstTokenModelTimeouts{
		"gpt-6-astra": {database.FirstTokenSizeOver500KB: 300},
	}
	if err := db.UpdateFirstTokenTimeoutSettings(t.Context(), settings); err != nil {
		t.Fatalf("seed: %v", err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/api/admin/settings/first-token-timeouts", nil)
	h.GetFirstTokenTimeoutSettings(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	for _, bracket := range []string{"under_50kb", "under_100kb", "under_200kb", "under_500kb", "over_500kb"} {
		if !gjson.GetBytes(body, "model_timeouts.gpt-6-astra."+bracket).Exists() {
			t.Fatalf("bracket %s missing from response: %s", bracket, body)
		}
	}
	if got := gjson.GetBytes(body, "model_timeouts.gpt-6-astra.under_100kb").Int(); got != 22 {
		t.Fatalf("unfilled bracket = %d, want global 22", got)
	}
}
