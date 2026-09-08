package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func (h *Handler) GetLiveStreamDetail(c *gin.Context) {
	id := strings.TrimSpace(c.Param("stream_id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stream_id is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	logs, err := h.db.ListUsageLogsByParentRequestIDs(ctx, []string{id})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	observed, exists := proxy.LiveStreamSnapshot(id)
	if len(logs) == 0 && !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "live stream not found"})
		return
	}
	rows := sortAndFilterChainLogs(logs)
	requests := make([]gin.H, 0, 1)
	if exists {
		for _, request := range observed.Requests {
			requests = append(requests, gin.H{"request_id": request.ID, "request_seq": request.Seq, "started_at": request.StartedAt, "ended_at": nullableTime(request.EndedAt), "outcome": request.Outcome})
		}
	}
	if len(requests) == 0 && len(rows) > 0 {
		requests = append(requests, gin.H{"request_id": id, "request_seq": 1, "started_at": rows[0].CreatedAt.Add(-time.Duration(rows[0].DurationMs) * time.Millisecond), "ended_at": nullableTime(rows[len(rows)-1].CreatedAt), "outcome": relayChainStatus(rows[len(rows)-1], false)})
	}
	end, opened, state, endReason := time.Time{}, time.Time{}, "closed", ""
	if exists {
		end, opened, state, endReason = observed.EndedAt, observed.OpenedAt, string(observed.State), observed.EndReason
	}
	if len(rows) > 0 {
		if opened.IsZero() {
			opened = rows[0].CreatedAt.Add(-time.Duration(rows[0].DurationMs) * time.Millisecond)
		}
		if end.IsZero() {
			end = rows[len(rows)-1].CreatedAt
		}
	}
	c.JSON(http.StatusOK, gin.H{"schema_version": 2, "stream_id": id, "state": state, "opened_at": opened, "last_activity_at": end, "ended_at": nullableTime(end), "elapsed_ms": elapsedMillis(opened, end), "end_reason": endReason, "data_complete": true, "requests": requests})
}

func (h *Handler) GetLiveStreamRequests(c *gin.Context) {
	id := strings.TrimSpace(c.Param("stream_id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stream_id is required"})
		return
	}
	if observed, ok := proxy.LiveStreamSnapshot(id); ok {
		items := make([]gin.H, 0, len(observed.Requests))
		for _, request := range observed.Requests {
			items = append(items, gin.H{"request_id": request.ID, "request_seq": request.Seq, "started_at": request.StartedAt, "ended_at": nullableTime(request.EndedAt), "outcome": request.Outcome})
		}
		c.JSON(http.StatusOK, gin.H{"schema_version": 2, "stream_id": id, "requests": items, "next_cursor": ""})
		return
	}
	c.JSON(http.StatusOK, gin.H{"schema_version": 2, "stream_id": id, "requests": []gin.H{}, "next_cursor": ""})
}

func (h *Handler) GetLiveStreamRequestAttempts(c *gin.Context) {
	id := strings.TrimSpace(c.Param("request_id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request_id is required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	logs, err := h.db.ListUsageLogsByParentRequestIDs(ctx, []string{id})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	rows := sortAndFilterChainLogs(logs)
	items := make([]gin.H, 0, len(rows))
	for index, row := range rows {
		items = append(items, gin.H{"attempt_id": attemptID(id, row.AttemptIndex, row.ID), "attempt_seq": index + 1, "account_id": row.AccountID, "account_name": row.AccountName, "channel": row.Channel, "transport": row.UpstreamEndpoint, "fallback": isFallbackRelayAttempt(row), "fallback_reason": row.FallbackReason, "status_code": row.StatusCode, "started_at": row.CreatedAt.Add(-time.Duration(row.DurationMs) * time.Millisecond), "ended_at": row.CreatedAt, "duration_ms": row.DurationMs, "error": liveStreamFirstNonEmpty(row.ErrorMessage, row.UpstreamErrorKind)})
	}
	c.JSON(http.StatusOK, gin.H{"schema_version": 2, "request_id": id, "attempts": items, "next_cursor": ""})
}

func attemptID(requestID string, seq int, rowID int64) string {
	if seq > 0 {
		return requestID + ":attempt-" + fmt.Sprintf("%d", seq)
	}
	return requestID + ":log-" + fmt.Sprintf("%d", rowID)
}
func liveStreamFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
func elapsedMillis(start, end time.Time) int64 {
	if start.IsZero() {
		return 0
	}
	if end.IsZero() {
		end = time.Now()
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}
