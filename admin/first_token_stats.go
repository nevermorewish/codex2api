package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetFirstTokenStats(c *gin.Context) {
	now := time.Now()
	start := database.StartOfDay(now)
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			start = t
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	stats, err := h.db.GetAccountFirstTokenStats(ctx, start, now)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"start": start, "end": now, "stats": stats})
}
