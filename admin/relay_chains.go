package admin

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// relayAttemptResponse is intentionally a small, UI-facing projection of a
// usage log row. Keeping the projection here means the usage log schema can
// continue to evolve without coupling the realtime page to every column.
type relayAttemptResponse struct {
	Seq         int    `json:"seq"`
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	StatusCode  int    `json:"status_code"`
	Error       string `json:"error,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	Decision    string `json:"decision"`
}

type relayChainResponse struct {
	RequestID   string                 `json:"request_id"`
	Model       string                 `json:"model"`
	Protocol    string                 `json:"protocol"`
	APIKeyID    int64                  `json:"api_key_id"`
	APIKeyName  string                 `json:"api_key_name"`
	StartedAt   time.Time              `json:"started_at"`
	Attempts    []relayAttemptResponse `json:"attempts"`
	FinalOK     bool                   `json:"final_ok"`
	TotalMs     int64                  `json:"total_ms"`
	SwitchCount int                    `json:"switch_count"`
}

// GetRelayChains reconstructs request-level failover chains from usage logs.
// Each upstream attempt is already logged with the request correlation ID and
// attempt index, so this works for both PostgreSQL and SQLite and survives a
// process restart.
func (h *Handler) GetRelayChains(c *gin.Context) {
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	// A request can produce several attempt rows. Fetch a bounded multiple so
	// the requested number of chains remains useful under failover-heavy load.
	logs, err := h.db.ListRecentUsageLogs(ctx, minInt(limit*8, 5000))
	if err != nil {
		writeInternalError(c, err)
		return
	}

	type chainRows struct {
		requestID string
		logs      []*database.UsageLog
	}
	groups := make(map[string]*chainRows)
	for _, logRow := range logs {
		if logRow == nil {
			continue
		}
		// Internal maintenance requests (for example overflow compaction) reuse
		// the parent request ID for attribution, but they are not relay hops and
		// should not inflate the account switch/attempt count shown here.
		if strings.TrimSpace(logRow.InternalReason) != "" {
			continue
		}
		requestID := strings.TrimSpace(logRow.ParentRequestID)
		if requestID == "" {
			// Legacy rows predate request correlation. Preserve them as single-hop
			// chains rather than dropping otherwise useful realtime history.
			requestID = "usage-" + strconv.FormatInt(logRow.ID, 10)
		}
		group := groups[requestID]
		if group == nil {
			group = &chainRows{requestID: requestID}
			groups[requestID] = group
		}
		group.logs = append(group.logs, logRow)
	}

	chains := make([]relayChainResponse, 0, len(groups))
	for _, group := range groups {
		if len(group.logs) == 0 {
			continue
		}
		// Continuation/折叠 rounds are persisted with attempt_index=0 and share
		// the parent request ID, while normal upstream failover attempts use the
		// one-based attempt index. When a chain has indexed attempts, omit those
		// auxiliary zero-index rows so they do not appear as account hops.
		hasIndexedAttempt := false
		for _, row := range group.logs {
			if row.AttemptIndex > 0 {
				hasIndexedAttempt = true
				break
			}
		}
		if hasIndexedAttempt {
			filtered := group.logs[:0]
			for _, row := range group.logs {
				if row.AttemptIndex > 0 {
					filtered = append(filtered, row)
				}
			}
			group.logs = filtered
		}
		if len(group.logs) == 0 {
			continue
		}
		sort.SliceStable(group.logs, func(i, j int) bool {
			a, b := group.logs[i], group.logs[j]
			if a.AttemptIndex != b.AttemptIndex {
				// Some legacy rows have no attempt index; keep them in timestamp order.
				if a.AttemptIndex == 0 || b.AttemptIndex == 0 {
					return a.CreatedAt.Before(b.CreatedAt)
				}
				return a.AttemptIndex < b.AttemptIndex
			}
			return a.CreatedAt.Before(b.CreatedAt)
		})
		first := group.logs[0]
		chain := relayChainResponse{
			RequestID:  group.requestID,
			Model:      first.Model,
			Protocol:   protocolForUsageLog(first),
			APIKeyID:   first.APIKeyID,
			APIKeyName: first.APIKeyName,
			Attempts:   make([]relayAttemptResponse, 0, len(group.logs)),
		}
		var previousAccountKey string
		for index, row := range group.logs {
			accountName := strings.TrimSpace(row.AccountName)
			if accountName == "" {
				accountName = strings.TrimSpace(row.AccountEmail)
			}
			if accountName == "" {
				accountName = strings.TrimSpace(row.FallbackAccountName)
			}
			if accountName == "" && row.AccountID != 0 {
				accountName = "账号 #" + strconv.FormatInt(row.AccountID, 10)
			}
			accountKey := relayAccountKey(row, accountName)
			decision := "failed"
			if row.StatusCode >= 200 && row.StatusCode < 300 && !row.IsRetryAttempt {
				decision = "success"
			} else if index > 0 || row.IsRetryAttempt {
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
			chain.Attempts = append(chain.Attempts, relayAttemptResponse{
				Seq: index + 1, AccountID: row.AccountID, AccountName: accountName,
				StatusCode: row.StatusCode, Error: message, DurationMs: int64(row.DurationMs), Decision: decision,
			})
			chain.TotalMs += int64(row.DurationMs)
			if index > 0 && accountKey != "" && previousAccountKey != "" && accountKey != previousAccountKey {
				chain.SwitchCount++
			}
			if accountKey != "" {
				previousAccountKey = accountKey
			}
			chain.FinalOK = row.StatusCode >= 200 && row.StatusCode < 300 && !row.IsRetryAttempt
		}
		chain.StartedAt = first.CreatedAt.Add(-time.Duration(first.DurationMs) * time.Millisecond)
		chains = append(chains, chain)
	}
	sort.Slice(chains, func(i, j int) bool { return chains[i].StartedAt.After(chains[j].StartedAt) })
	if len(chains) > limit {
		chains = chains[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"chains": chains})
}

func relayAccountKey(row *database.UsageLog, accountName string) string {
	if row == nil {
		return ""
	}
	if row.AccountID != 0 {
		return "id:" + strconv.FormatInt(row.AccountID, 10)
	}
	if name := strings.TrimSpace(accountName); name != "" {
		return "name:" + name
	}
	return ""
}

func protocolForUsageLog(row *database.UsageLog) string {
	if row == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(row.Channel), database.UpstreamChannelClaude) {
		return "anthropic"
	}
	endpoint := row.InboundEndpoint
	if endpoint == "" {
		endpoint = row.Endpoint
	}
	if strings.Contains(endpoint, "/messages") {
		return "anthropic"
	}
	return "openai"
}
