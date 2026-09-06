package proxy

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const contextFallbackMetricAttempt = "fallbackMetricAttempt"
const maxFallbackMetricAccounts = 128

// Process-local cumulative counters, independent of usage-log persistence.
// Account names are operator labels; no provider URL or credential is exposed.
type FallbackAccountMetrics struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Attempts  uint64 `json:"attempts"`
	Success   uint64 `json:"success"`
	Failure   uint64 `json:"failure"`
	Canceled  uint64 `json:"canceled"`
}

type FallbackMetricsSnapshot struct {
	Since                    string                   `json:"since"`
	WSPrimaryAttempts        uint64                   `json:"ws_primary_attempts"`
	FallbackHandoffCount     uint64                   `json:"fallback_handoff_count"`
	FallbackAttemptCount     uint64                   `json:"fallback_attempt_count"`
	FallbackSuccessCount     uint64                   `json:"fallback_success_count"`
	FallbackFailureCount     uint64                   `json:"fallback_failure_count"`
	FallbackCanceledCount    uint64                   `json:"fallback_canceled_count"`
	UpstreamOverloadedCount  uint64                   `json:"upstream_overloaded_count"`
	FirstTokenTimeoutCount   uint64                   `json:"first_token_timeout_count"`
	UpstreamStreamBreakCount uint64                   `json:"upstream_stream_break_count"`
	Accounts                 []FallbackAccountMetrics `json:"accounts"`
}

type fallbackMetrics struct {
	mu       sync.Mutex
	totals   FallbackMetricsSnapshot
	accounts map[int64]*FallbackAccountMetrics
}

func newFallbackMetrics() *fallbackMetrics {
	return &fallbackMetrics{totals: FallbackMetricsSnapshot{Since: time.Now().UTC().Format(time.RFC3339)}, accounts: make(map[int64]*FallbackAccountMetrics)}
}

var globalFallbackMetrics = newFallbackMetrics()

func GetFallbackMetricsSnapshot() FallbackMetricsSnapshot { return globalFallbackMetrics.snapshot() }

func (m *fallbackMetrics) snapshot() FallbackMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.totals
	s.Accounts = make([]FallbackAccountMetrics, 0, len(m.accounts))
	for _, account := range m.accounts {
		s.Accounts = append(s.Accounts, *account)
	}
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].AccountID < s.Accounts[j].AccountID })
	return s
}

type fallbackMetricAttempt struct {
	collector *fallbackMetrics
	accountID int64
	bucketID  int64
	fallback  bool
	completed bool
}

func (m *fallbackMetrics) selected(c *gin.Context, state *fallbackRouteState, account *auth.Account) {
	if c == nil || state == nil || account == nil {
		return
	}
	a := &fallbackMetricAttempt{collector: m, accountID: account.ID(), fallback: account.IsExternalFallback()}
	c.Set(contextFallbackMetricAttempt, a)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !a.fallback {
		if c.Request != nil && isResponsesWebSocketUpgradeRequest(c.Request) {
			m.totals.WSPrimaryAttempts++
		}
		return
	}
	if !state.metricHandoffRecorded {
		m.totals.FallbackHandoffCount++
		state.metricHandoffRecorded = true
	}
	m.totals.FallbackAttemptCount++
	a.bucketID = a.accountID
	if m.accounts[a.bucketID] == nil && len(m.accounts) >= maxFallbackMetricAccounts {
		a.bucketID = 0
	}
	b := m.accounts[a.bucketID]
	if b == nil {
		b = &FallbackAccountMetrics{AccountID: a.bucketID}
		m.accounts[a.bucketID] = b
	}
	if a.bucketID == 0 {
		b.Name = "other"
	} else {
		account.Mu().RLock()
		b.Name = strings.TrimSpace(account.Name)
		account.Mu().RUnlock()
	}
	b.Attempts++
}

// Observe only the selected outer attempt. Hidden continuation usage (index 0)
// and duplicate audit writes must not inflate success/failure counters.
func observeFallbackMetricUsage(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.AttemptIndex <= 0 || input.StatusCode < 200 {
		return
	}
	value, _ := c.Get(contextFallbackMetricAttempt)
	a, _ := value.(*fallbackMetricAttempt)
	if a == nil || a.accountID != input.AccountID {
		return
	}
	m := a.collector
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.completed {
		return
	}
	a.completed = true
	if a.fallback {
		b := m.accounts[a.bucketID]
		switch {
		case input.StatusCode == logStatusClientClosed:
			m.totals.FallbackCanceledCount++
			b.Canceled++
		case input.StatusCode < 300 && input.ErrorMessage == "":
			m.totals.FallbackSuccessCount++
			b.Success++
		default:
			m.totals.FallbackFailureCount++
			b.Failure++
		}
	}
	if input.StatusCode < http.StatusBadRequest || input.StatusCode == logStatusClientClosed {
		return
	}
	message := strings.ToLower(input.ErrorMessage)
	if strings.Contains(message, overloadErrorCode) {
		m.totals.UpstreamOverloadedCount++
	}
	// The first-token watchdog has explicit messages; a generic timeout may
	// instead occur mid-stream and must not be called a first-token timeout.
	if input.UpstreamErrorKind == "timeout" && (strings.Contains(message, "first token timeout") || strings.Contains(message, "上游首字超时")) {
		m.totals.FirstTokenTimeoutCount++
	} else if input.StatusCode == logStatusUpstreamStreamBreak {
		m.totals.UpstreamStreamBreakCount++
	}
}
