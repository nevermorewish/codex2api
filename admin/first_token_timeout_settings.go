package admin

import (
	"net/http"
	"strings"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// FirstTokenTimeoutSettingsResponse 是首字超时配置的读写形状：全局档位表
// （未单独配置的模型都跟随它）加按模型逐档覆盖表。模型表的每个模型都是
// 完整五行——store 端已把缺档补成全局值，前端表格因此不需要自己算回落。
type FirstTokenTimeoutSettingsResponse struct {
	Under50KB     int                                         `json:"under_50kb"`
	Under100KB    int                                         `json:"under_100kb"`
	Under200KB    int                                         `json:"under_200kb"`
	Under500KB    int                                         `json:"under_500kb"`
	Over500KB     int                                         `json:"over_500kb"`
	ModelTimeouts map[string]database.FirstTokenModelTimeouts `json:"model_timeouts"`
}

func firstTokenTimeoutSettingsResponse(settings database.FirstTokenTimeoutSettings) FirstTokenTimeoutSettingsResponse {
	normalized := database.NormalizeFirstTokenTimeoutSettings(settings)
	models := make(map[string]database.FirstTokenModelTimeouts, len(normalized.ModelTimeouts))
	for model, timeouts := range normalized.ModelTimeouts {
		models[model] = timeouts
	}
	return FirstTokenTimeoutSettingsResponse{
		Under50KB:     normalized.Under50KB,
		Under100KB:    normalized.Under100KB,
		Under200KB:    normalized.Under200KB,
		Under500KB:    normalized.Under500KB,
		Over500KB:     normalized.Over500KB,
		ModelTimeouts: models,
	}
}

func (h *Handler) GetFirstTokenTimeoutSettings(c *gin.Context) {
	v, err := h.db.GetFirstTokenTimeoutSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, firstTokenTimeoutSettingsResponse(v))
}

// UpdateFirstTokenTimeoutSettings 落库后回显的是归一化后的结果而不是请求原样：
// 请求里被丢弃的档位/模型（越界值、未知档位键、空白模型名）必须在响应里如实消失，
// 否则后台显示"已保存"，实际库里没有这条配置。
func (h *Handler) UpdateFirstTokenTimeoutSettings(c *gin.Context) {
	var req database.FirstTokenTimeoutSettingsInput
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid first token timeout settings")
		return
	}
	settings := database.FirstTokenTimeoutSettings{
		Under50KB:     req.Under50KB,
		Under100KB:    req.Under100KB,
		Under200KB:    req.Under200KB,
		Under500KB:    req.Under500KB,
		Over500KB:     req.Over500KB,
		ModelTimeouts: req.ModelTimeouts,
	}
	if err := settings.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, firstTokenTimeoutValidationMessage(err))
		return
	}
	settings = database.NormalizeFirstTokenTimeoutSettings(settings)
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	if err := h.db.UpdateFirstTokenTimeoutSettings(c.Request.Context(), settings); err != nil {
		writeInternalError(c, err)
		return
	}
	proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
		current.FirstTokenSizeTimeouts = settings
		return current
	})
	c.JSON(http.StatusOK, firstTokenTimeoutSettingsResponse(settings))
}

// firstTokenTimeoutValidationMessage 把存储层的校验错误翻译成能指到具体配置的中文提示：
// 笼统的 400 会让"哪个模型哪一档填错了"重新回到猜测。
func firstTokenTimeoutValidationMessage(err error) string {
	if err == nil {
		return "首 token 超时配置无效"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "must not be blank"):
		return "模型名不能为空"
	case strings.Contains(message, "unknown first token size bracket"):
		return "存在未知的请求体体积档位，请刷新页面后重试"
	case strings.Contains(message, "1..600"):
		return "超时时间必须是 1～600 秒，请检查标红的模型档位"
	}
	return message
}
