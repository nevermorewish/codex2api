package admin

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// liveStreamsRecentGraceWindow bounds ListRecentlyEndedParentRequestIDs. A
// stream leaves proxy.LiveAttemptsSnapshot the instant its final attempt is
// recorded, so without a short backward-looking window a poll could never
// observe why it just ended — the entry would already be gone. Keeping the
// window tens of seconds wide means the underlying query only ever scans a
// tiny, recent slice of usage_logs (idx_usage_logs_created_at), never the
// whole table.
const liveStreamsRecentGraceWindow = 30 * time.Second

type liveStreamResponse struct {
	SchemaVersion    int                    `json:"schema_version"`
	StreamID         string                 `json:"stream_id"`
	RequestID        string                 `json:"request_id"`
	Model            string                 `json:"model"`
	Protocol         string                 `json:"protocol"`
	APIKeyID         int64                  `json:"api_key_id"`
	APIKeyName       string                 `json:"api_key_name"`
	StartedAt        time.Time              `json:"started_at"`
	Attempts         []relayAttemptResponse `json:"attempts"`
	Status           string                 `json:"status"`
	Disconnected     bool                   `json:"disconnected"`
	DisconnectReason string                 `json:"disconnect_reason,omitempty"`
	TotalMs          int64                  `json:"total_ms"`
	AttemptCount     int                    `json:"attempt_count"`
	SwitchCount      int                    `json:"switch_count"`
	RequestCount     int                    `json:"request_count"`
	EndedAt          *time.Time             `json:"ended_at,omitempty"`
	ElapsedMs        int64                  `json:"elapsed_ms"`
	EndReason        string                 `json:"end_reason,omitempty"`
	EndSource        string                 `json:"end_source,omitempty"`
	DataComplete     bool                   `json:"data_complete"`
}

// GetLiveStreams reports client streams from the independent lifecycle
// observer and enriches them with the currently active or recently persisted
// upstream attempts. The bounded usage-log lookup is only a compatibility
// fallback for streams created before the observer was enabled.
func (h *Handler) GetLiveStreams(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	inFlight := proxy.LiveAttemptsSnapshot()
	observedStreams := proxy.LiveStreamsSnapshot()
	recentIDs, err := h.db.ListRecentlyEndedParentRequestIDs(ctx, time.Now().Add(-liveStreamsRecentGraceWindow))
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// Union of "currently in flight" and "ended recently" parent request IDs.
	// A stream present in both (its last attempt just finished but the
	// registry has not yet drained under concurrent access) is deduplicated
	// here and resolved as in_progress below, since LiveAttemptsSnapshot is
	// the authoritative "is it still running" signal.
	parentIDs := make([]string, 0, len(inFlight)+len(recentIDs))
	seen := make(map[string]struct{}, len(inFlight)+len(recentIDs))
	inFlightByParent := make(map[string][]proxy.LiveAttemptInfo, len(inFlight))
	for _, attempt := range inFlight {
		if !attempt.IsStream {
			continue
		}
		inFlightByParent[attempt.ParentRequestID] = append(inFlightByParent[attempt.ParentRequestID], attempt)
		if _, ok := seen[attempt.ParentRequestID]; !ok {
			seen[attempt.ParentRequestID] = struct{}{}
			parentIDs = append(parentIDs, attempt.ParentRequestID)
		}
	}
	for _, observed := range observedStreams {
		if _, ok := seen[observed.ID]; !ok {
			seen[observed.ID] = struct{}{}
			parentIDs = append(parentIDs, observed.ID)
		}
	}
	for _, id := range recentIDs {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			parentIDs = append(parentIDs, id)
		}
	}

	if len(parentIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"schema_version": 2, "collected_at": time.Now(), "streams": []liveStreamResponse{}, "total": 0, "truncated": proxy.LiveAttemptsTruncated(), "data_complete": true})
		return
	}

	logs, err := h.db.ListUsageLogsByParentRequestIDs(ctx, parentIDs)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	historyByParent := make(map[string][]*database.UsageLog, len(parentIDs))
	for _, row := range logs {
		if row == nil {
			continue
		}
		if !row.Stream && !row.ViaWebsocket && (strings.TrimSpace(row.Endpoint) != "" || strings.TrimSpace(row.InboundEndpoint) != "") {
			continue
		}
		historyByParent[row.ParentRequestID] = append(historyByParent[row.ParentRequestID], row)
	}
	observedByID := make(map[string]proxy.LiveStreamInfo, len(observedStreams))
	for _, observed := range observedStreams {
		observedByID[observed.ID] = observed
	}

	fallbackNames := make(map[int64]string)
	if h.fallbackPool != nil {
		for _, account := range h.fallbackPool.Accounts() {
			if account == nil {
				continue
			}
			account.Mu().RLock()
			name := strings.TrimSpace(account.Name)
			account.Mu().RUnlock()
			fallbackNames[account.ID()] = name
		}
	}

	streams := make([]liveStreamResponse, 0, len(parentIDs))
	for _, requestID := range parentIDs {
		attempts := inFlightByParent[requestID]
		history := sortAndFilterChainLogs(historyByParent[requestID])
		observed, hasObserved := observedByID[requestID]
		if len(attempts) == 0 && len(history) == 0 && !hasObserved {
			// The recent-ended lookup raced with the history batch (e.g. flush
			// lag) and produced nothing usable; skip rather than show an empty row.
			continue
		}

		stream := liveStreamResponse{
			SchemaVersion: 2, StreamID: requestID, RequestID: requestID,
			Attempts:     make([]relayAttemptResponse, 0, len(history)+len(attempts)),
			DataComplete: true,
		}
		if hasObserved {
			stream.Model, stream.Protocol = observed.Model, observed.Protocol
			stream.StartedAt = observed.OpenedAt
			stream.RequestCount = len(observed.Requests)
			stream.EndReason, stream.EndSource = observed.EndReason, observed.EndSource
			if !observed.EndedAt.IsZero() {
				ended := observed.EndedAt
				stream.EndedAt = &ended
			}
			end := observed.EndedAt
			if end.IsZero() {
				end = time.Now()
			}
			if !observed.OpenedAt.IsZero() {
				stream.ElapsedMs = end.Sub(observed.OpenedAt).Milliseconds()
			}
			if observed.State == proxy.LiveStreamOpen {
				stream.Status = "in_progress"
			} else {
				switch {
				case strings.Contains(observed.EndReason, "cancel"):
					stream.Status = "canceled"
				case strings.Contains(observed.EndReason, "normal"), observed.EndReason == "request_completed":
					stream.Status = "success"
				case observed.EndReason != "":
					stream.Status = "failed"
				}
			}
		}
		if len(history) > 0 {
			first := history[0]
			stream.Model = first.Model
			stream.Protocol = protocolForUsageLog(first)
			stream.APIKeyID = first.APIKeyID
			stream.APIKeyName = first.APIKeyName
			stream.StartedAt = first.CreatedAt.Add(-time.Duration(first.DurationMs) * time.Millisecond)
		}

		var previousAccountKey string
		seq := 0
		for _, row := range history {
			seq++
			accountName := relayAccountName(row, fallbackNames)
			accountKey := relayAccountKey(row, accountName)
			decision := "failed"
			if row.StatusCode == 499 {
				decision = "canceled"
			} else if row.StatusCode >= 200 && row.StatusCode < 300 && !row.IsRetryAttempt {
				decision = "success"
			} else if seq > 1 || row.IsRetryAttempt {
				switch {
				case accountKey != "" && previousAccountKey != "" && accountKey != previousAccountKey:
					decision = "switch"
				case accountKey != "" && accountKey == previousAccountKey:
					decision = "retry_same"
				default:
					decision = "retry"
				}
			}
			message := strings.TrimSpace(row.ErrorMessage)
			if message == "" {
				message = strings.TrimSpace(row.UpstreamErrorKind)
			}
			stream.Attempts = append(stream.Attempts, relayAttemptResponse{
				Seq: seq, AccountID: row.AccountID, AccountName: accountName,
				Fallback:       isFallbackRelayAttempt(row),
				FallbackReason: strings.TrimSpace(row.FallbackReason),
				StatusCode:     row.StatusCode, Error: message, DurationMs: int64(row.DurationMs), Decision: decision,
			})
			stream.TotalMs += int64(row.DurationMs)
			if seq > 1 && accountKey != "" && previousAccountKey != "" && accountKey != previousAccountKey {
				stream.SwitchCount++
			}
			if accountKey != "" {
				previousAccountKey = accountKey
			}
			if row.StatusCode == 499 {
				stream.Status = "canceled"
				stream.DisconnectReason = ""
			} else if row.StatusCode >= 200 && row.StatusCode < 300 && !row.IsRetryAttempt {
				stream.Status = "success"
				stream.DisconnectReason = ""
			} else {
				stream.Status = "failed"
				stream.DisconnectReason = strings.TrimSpace(row.UpstreamErrorKind)
			}
		}
		if stream.RequestCount == 0 && len(history) > 0 {
			stream.RequestCount = 1
		}
		if !hasObserved && len(history) > 0 {
			ended := history[len(history)-1].CreatedAt
			stream.EndedAt = &ended
			stream.ElapsedMs = ended.Sub(stream.StartedAt).Milliseconds()
		}

		// Sort in-flight attempts by their local attempt index so multiple
		// concurrently-registered attempts (a brief begin/end overlap during
		// rotation) still render in a stable, chronological order.
		sort.SliceStable(attempts, func(i, j int) bool { return attempts[i].AttemptIndex < attempts[j].AttemptIndex })
		historyAttemptIndexes := make(map[int]struct{}, len(history))
		for _, row := range history {
			if row.AttemptIndex > 0 {
				historyAttemptIndexes[row.AttemptIndex] = struct{}{}
			}
		}
		for _, attempt := range attempts {
			if attempt.AttemptIndex > 0 {
				if _, exists := historyAttemptIndexes[attempt.AttemptIndex]; exists {
					continue
				}
			}
			seq++
			if stream.Model == "" {
				stream.Model = attempt.Model
			}
			if stream.Protocol == "" {
				stream.Protocol = "openai"
			}
			if stream.APIKeyID == 0 {
				stream.APIKeyID = attempt.APIKeyID
			}
			if stream.APIKeyName == "" {
				stream.APIKeyName = attempt.APIKeyName
			}
			if stream.StartedAt.IsZero() || attempt.StartedAt.Before(stream.StartedAt) {
				// An in-flight attempt's own StartedAt is a real recorded
				// timestamp, more accurate than the history's back-computed
				// (CreatedAt - DurationMs) estimate. Prefer whichever is earliest
				// so the stream's started_at always reflects its first hop.
				stream.StartedAt = attempt.StartedAt
			}
			accountKey := "id:" + strconv.FormatInt(attempt.AccountID, 10)
			decision := "in_progress"
			if seq > 1 && previousAccountKey != "" && accountKey != previousAccountKey {
				decision = "switch"
				stream.SwitchCount++
			}
			previousAccountKey = accountKey
			stream.Attempts = append(stream.Attempts, relayAttemptResponse{
				Seq: seq, AccountID: attempt.AccountID, AccountName: attempt.AccountName,
				Fallback: attempt.Fallback, Decision: decision,
			})
		}

		if len(attempts) > 0 {
			stream.Status = "in_progress"
			stream.DisconnectReason = ""
		}
		if stream.Status == "" {
			stream.Status = "incomplete"
		}
		if stream.ElapsedMs == 0 && !stream.StartedAt.IsZero() {
			end := time.Now()
			if stream.EndedAt != nil {
				end = *stream.EndedAt
			}
			stream.ElapsedMs = end.Sub(stream.StartedAt).Milliseconds()
		}
		if stream.EndReason == "" && stream.Status != "in_progress" {
			switch stream.Status {
			case "success":
				stream.EndReason = "request_completed"
			case "canceled":
				stream.EndReason = "client_canceled"
			case "failed":
				stream.EndReason = stream.DisconnectReason
			default:
				stream.EndReason = "unknown"
			}
		}
		if hasObserved && (stream.EndReason == "request_completed" || stream.EndReason == "") && len(history) > 0 {
			last := history[len(history)-1]
			if last.StatusCode == 499 {
				stream.EndReason = "client_canceled"
				stream.EndSource = "usage_log"
			} else if last.StatusCode < 200 || last.StatusCode >= 300 || last.IsRetryAttempt {
				stream.EndReason = firstNonEmptyLiveReason(last.UpstreamErrorKind, last.ErrorMessage, "request_failed")
				stream.EndSource = "usage_log"
			}
		}
		stream.Disconnected = stream.Status != "in_progress" && stream.Status != "success"
		stream.AttemptCount = len(stream.Attempts)
		streams = append(streams, stream)
	}

	sort.Slice(streams, func(i, j int) bool { return streams[i].StartedAt.After(streams[j].StartedAt) })
	c.JSON(http.StatusOK, gin.H{"schema_version": 2, "collected_at": time.Now(), "streams": streams, "total": len(streams), "truncated": proxy.LiveAttemptsTruncated(), "data_complete": true})
}

func firstNonEmptyLiveReason(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}
