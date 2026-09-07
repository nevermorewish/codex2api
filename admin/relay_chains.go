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

// relayAttemptResponse is intentionally a small, UI-facing projection of a
// usage log row. Keeping the projection here means the usage log schema can
// continue to evolve without coupling the realtime page to every column.
type relayAttemptResponse struct {
	Seq            int    `json:"seq"`
	AccountID      int64  `json:"account_id"`
	AccountName    string `json:"account_name"`
	Fallback       bool   `json:"fallback"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	StatusCode     int    `json:"status_code"`
	Error          string `json:"error,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
	Decision       string `json:"decision"`
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
	Status      string                 `json:"status"`
	TotalMs     int64                  `json:"total_ms"`
	SwitchCount int                    `json:"switch_count"`
}

// GetRelayChains reconstructs request-level failover chains from usage logs.
// Each upstream attempt is already logged with the request correlation ID and
// attempt index, so this works for both PostgreSQL and SQLite and survives a
// process restart.
func (h *Handler) GetRelayChains(c *gin.Context) {
	page := 1
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}
	const pageSize = 20
	if page > int(^uint(0)>>1)/pageSize {
		writeError(c, http.StatusBadRequest, "page is too large")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	logs, total, err := h.db.ListRelayChainLogs(ctx, page, pageSize)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	// External fallback accounts use negative runtime IDs and do not exist in
	// the primary accounts table. Resolve their current labels from the pool,
	// including disabled accounts, so older usage rows also display names.
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

	type chainRows struct {
		requestID string
		logs      []*database.UsageLog
	}
	groups := make(map[string]*chainRows)
	order := make(map[string]int)
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
			order[requestID] = len(order)
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
			accountName := relayAccountName(row, fallbackNames)
			accountKey := relayAccountKey(row, accountName)
			decision := "failed"
			if row.StatusCode == 499 {
				decision = "canceled"
			} else if row.StatusCode >= 200 && row.StatusCode < 300 && !row.IsRetryAttempt {
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
				Fallback:       isFallbackRelayAttempt(row),
				FallbackReason: strings.TrimSpace(row.FallbackReason),
				StatusCode:     row.StatusCode, Error: message, DurationMs: int64(row.DurationMs), Decision: decision,
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
		chain.Status = relayChainStatus(group.logs[len(group.logs)-1], proxy.RelayRequestInProgress(chain.RequestID))
		chain.StartedAt = first.CreatedAt.Add(-time.Duration(first.DurationMs) * time.Millisecond)
		chains = append(chains, chain)
	}
	sort.Slice(chains, func(i, j int) bool { return order[chains[i].RequestID] < order[chains[j].RequestID] })
	c.JSON(http.StatusOK, gin.H{"chains": chains, "total": total, "page": page, "page_size": pageSize})
}

func relayChainStatus(last *database.UsageLog, active bool) string {
	if last == nil {
		return "incomplete"
	}
	if last.StatusCode == 499 {
		return "canceled"
	}
	if last.IsRetryAttempt || last.StatusCode < 200 {
		if active {
			return "in_progress"
		}
		// The request ended (or the process restarted), but its final attempt
		// has not been persisted. Do not invent a success or failure outcome.
		return "incomplete"
	}
	if last.StatusCode < 300 {
		return "success"
	}
	return "failed"
}

func isFallbackRelayAttempt(row *database.UsageLog) bool {
	return row != nil && (row.AccountID < 0 || strings.EqualFold(strings.TrimSpace(row.Channel), "fallback"))
}

func relayAccountName(row *database.UsageLog, fallbackNames map[int64]string) string {
	if row == nil {
		return ""
	}
	if isFallbackRelayAttempt(row) {
		if name := strings.TrimSpace(fallbackNames[row.AccountID]); name != "" {
			return name
		}
		if name := strings.TrimSpace(row.FallbackAccountName); name != "" {
			return name
		}
		if row.AccountID < 0 {
			// Trim the sign rather than negate: this also handles MinInt64.
			return "兜底账号 #" + strings.TrimPrefix(strconv.FormatInt(row.AccountID, 10), "-")
		}
	}
	for _, name := range []string{row.AccountName, row.AccountEmail, row.FallbackAccountName} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	if row.AccountID != 0 {
		return "账号 #" + strconv.FormatInt(row.AccountID, 10)
	}
	return ""
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
