package admin

import (
	"net/http"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetFirstTokenTimeoutSettings(c *gin.Context) {
	v, err := h.db.GetFirstTokenTimeoutSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, v)
}
func (h *Handler) UpdateFirstTokenTimeoutSettings(c *gin.Context) {
	var v database.FirstTokenTimeoutSettings
	if err := c.ShouldBindJSON(&v); err != nil {
		writeError(c, 400, "invalid first token timeout settings")
		return
	}
	if err := v.Validate(); err != nil {
		writeError(c, 400, "first token timeout values must be 1..600 seconds")
		return
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	if err := h.db.UpdateFirstTokenTimeoutSettings(c.Request.Context(), v); err != nil {
		writeInternalError(c, err)
		return
	}
	proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
		current.FirstTokenSizeTimeouts = v
		return current
	})
	c.JSON(http.StatusOK, v)
}
