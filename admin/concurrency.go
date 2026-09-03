package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

type concurrencyAccountRow struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Channel     string   `json:"channel"`
	Status      string   `json:"status"`
	HealthTier  string   `json:"health_tier"`
	GroupIDs    []int64  `json:"group_ids"`
	GroupNames  []string `json:"group_names"`
	Active      int64    `json:"active"`
	Occupied    int64    `json:"occupied"`
	Buffered    int64    `json:"buffered"`
	Limit       int64    `json:"limit"`
	Utilization float64  `json:"utilization"`
	Available   bool     `json:"available"`
	Fallback    bool     `json:"fallback"`
}

type concurrencyGroupRow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	AccountCount int    `json:"account_count"`
	Active       int64  `json:"active"`
	Occupied     int64  `json:"occupied"`
	Buffered     int64  `json:"buffered"`
	Capacity     int64  `json:"capacity"`
}

type concurrencyAPIKeyRow struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Active  int64  `json:"active"`
	Limit   int    `json:"limit"`
	Enabled bool   `json:"enabled"`
	Expired bool   `json:"expired"`
}

func concurrencyChannel(account *auth.Account) string {
	switch {
	case account == nil:
		return "unknown"
	case account.IsExternalFallback():
		return "fallback"
	case account.IsGrokAPI():
		return "grok"
	case account.IsAntigravityAPI():
		return "antigravity"
	case account.IsClaudeOAuth():
		return "claude"
	case account.IsOpenAIResponsesAPI():
		return "openai_responses"
	default:
		return "codex"
	}
}

func concurrencyAccount(account *auth.Account, groupNames map[int64]string) concurrencyAccountRow {
	snapshot := account.GetAccountListRuntimeSnapshot()
	account.Mu().RLock()
	name := strings.TrimSpace(account.Name)
	if name == "" {
		name = strings.TrimSpace(account.Email)
	}
	account.Mu().RUnlock()
	if name == "" {
		name = "#" + strconv.FormatInt(account.ID(), 10)
	}
	buffered := snapshot.OccupiedRequests - snapshot.ActiveRequests
	if buffered < 0 {
		buffered = 0
	}
	utilization := float64(0)
	if snapshot.DynamicConcurrencyLimit > 0 {
		utilization = float64(snapshot.OccupiedRequests) / float64(snapshot.DynamicConcurrencyLimit) * 100
	}
	names := make([]string, 0, len(snapshot.GroupIDs))
	for _, id := range snapshot.GroupIDs {
		if groupNames[id] != "" {
			names = append(names, groupNames[id])
		}
	}
	id := account.ID()
	if account.IsExternalFallback() {
		id = -id
	}
	return concurrencyAccountRow{
		ID: id, Name: name, Channel: concurrencyChannel(account), Status: snapshot.Status,
		HealthTier: snapshot.HealthTier, GroupIDs: snapshot.GroupIDs, GroupNames: names,
		Active: snapshot.ActiveRequests, Occupied: snapshot.OccupiedRequests, Buffered: buffered,
		Limit: snapshot.DynamicConcurrencyLimit, Utilization: utilization,
		Available: account.IsAvailable(), Fallback: account.IsExternalFallback(),
	}
}

func (h *Handler) GetConcurrencySnapshot(c *gin.Context) {
	ctx := c.Request.Context()
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account groups"})
		return
	}
	keys, err := h.db.ListAPIKeys(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load API keys"})
		return
	}
	groupNames := make(map[int64]string, len(groups))
	groupRows := make(map[int64]*concurrencyGroupRow, len(groups))
	for _, group := range groups {
		groupNames[group.ID] = group.Name
		groupRows[group.ID] = &concurrencyGroupRow{ID: group.ID, Name: group.Name, Color: group.Color}
	}

	accounts := h.store.Accounts()
	fallbackEnabled := false
	if h.fallbackPool != nil {
		fallbackEnabled = h.fallbackPool.Policy().Enabled
		accounts = append(accounts, h.fallbackPool.Accounts()...)
	}
	accountRows := make([]concurrencyAccountRow, 0, len(accounts))
	var totalActive, totalOccupied, capacity int64
	for _, account := range accounts {
		if account == nil {
			continue
		}
		row := concurrencyAccount(account, groupNames)
		if row.Fallback && !fallbackEnabled {
			row.Available = false
		}
		accountRows = append(accountRows, row)
		totalActive += row.Active
		totalOccupied += row.Occupied
		if row.Available && row.Limit > 0 {
			capacity += row.Limit
		}
		if row.Fallback {
			continue
		}
		for _, groupID := range row.GroupIDs {
			group := groupRows[groupID]
			if group == nil {
				continue
			}
			group.AccountCount++
			group.Active += row.Active
			group.Occupied += row.Occupied
			group.Buffered += row.Buffered
			if row.Available && row.Limit > 0 {
				group.Capacity += row.Limit
			}
		}
	}
	sort.Slice(accountRows, func(i, j int) bool {
		if accountRows[i].Fallback != accountRows[j].Fallback {
			return !accountRows[i].Fallback
		}
		if accountRows[i].Occupied != accountRows[j].Occupied {
			return accountRows[i].Occupied > accountRows[j].Occupied
		}
		return accountRows[i].ID < accountRows[j].ID
	})
	groupList := make([]concurrencyGroupRow, 0, len(groupRows))
	for _, group := range groups {
		if row := groupRows[group.ID]; row != nil {
			groupList = append(groupList, *row)
		}
	}

	keyActive := map[int64]int64{}
	if h.apiKeyConcurrencySnapshot != nil {
		keyActive = h.apiKeyConcurrencySnapshot()
	}
	now := time.Now()
	keyRows := make([]concurrencyAPIKeyRow, 0, len(keys))
	for _, key := range keys {
		if key == nil {
			continue
		}
		keyRows = append(keyRows, concurrencyAPIKeyRow{
			ID: key.ID, Name: key.Name, Active: keyActive[key.ID], Limit: key.Limits.MaxConcurrency,
			Enabled: key.Enabled, Expired: key.IsExpired(now),
		})
	}
	sort.Slice(keyRows, func(i, j int) bool {
		if keyRows[i].Active != keyRows[j].Active {
			return keyRows[i].Active > keyRows[j].Active
		}
		return keyRows[i].ID < keyRows[j].ID
	})

	globalActive := int64(0)
	if h.rateLimiter != nil {
		globalActive = h.rateLimiter.GetActiveRequests()
	}
	queueDepth := int64(0)
	if h.store != nil {
		queueDepth = h.store.GetSchedulerMetrics().Waiters
	}
	c.JSON(http.StatusOK, gin.H{
		"collected_at": now.UTC(), "global_active": globalActive, "queue_depth": queueDepth,
		"total_active": totalActive, "total_occupied": totalOccupied, "capacity": capacity,
		"accounts": accountRows, "groups": groupList, "api_keys": keyRows,
	})
}
