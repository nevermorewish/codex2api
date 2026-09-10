package proxy

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const oversizedDirectFallbackBytes = 3 << 20
const fallbackTerminalAttemptContextKey = "fallback_terminal_attempt"
const contextFallbackReason = "fallbackReason"
const contextFallbackDeadlineState = "fallbackDeadlineState"

const (
	fallbackReasonRelayLimit       = "relay_limit"
	fallbackReasonRetryDeadline    = "retry_deadline"
	fallbackReasonAffinityFull     = "affinity_capacity_full"
	fallbackReasonQueueThreshold   = "queue_threshold"
	fallbackReasonNoEligible       = "no_eligible_primary"
	fallbackReasonWaitEnded        = "primary_wait_ended"
	fallbackReasonUnavailable      = "primary_unavailable"
	fallbackReasonOversizedRequest = "oversized_request"
)

type fallbackRouteState struct {
	pool                  *auth.FallbackPool
	filter                auth.AccountFilter
	policy                auth.FallbackPolicy
	primaryAttempts       int
	active                bool
	required              bool
	sourceAccount         *auth.Account
	metricHandoffRecorded bool
	fallbackAttempted     bool
	reason                string
	retryHandoffEnabled   bool
}

func (h *Handler) newFallbackRouteState(filter auth.AccountFilter, requestBodySize ...int) *fallbackRouteState {
	state := &fallbackRouteState{filter: filter}
	if h == nil || h.fallbackPool == nil {
		return state
	}
	state.pool = h.fallbackPool
	state.policy = h.fallbackPool.Policy()
	if state.policy.Enabled && state.policy.OversizedRequestDirectFallbackEnabled &&
		len(requestBodySize) > 0 && requestBodySize[0] > oversizedDirectFallbackBytes {
		state.active = true
		state.reason = fallbackReasonOversizedRequest
		state.required = true
	}
	return state
}

func (s *fallbackRouteState) account(exclude map[int64]bool) *auth.Account {
	if s == nil || !s.active || s.pool == nil || !s.policy.Enabled || s.fallbackAttempted {
		return nil
	}
	return s.pool.Acquire(exclude, s.filter)
}

func (s *fallbackRouteState) noteSelected(account *auth.Account) {
	if s == nil || account == nil {
		return
	}
	if account.IsExternalFallback() {
		s.fallbackAttempted = true
		return
	}
	s.primaryAttempts++
	s.sourceAccount = account
	if s.configured() && s.primaryAttempts >= s.policy.RelayCount {
		s.active = true
		s.reason = fallbackReasonRelayLimit
	}
}

// The external pool is the final attempt, including requests that bypassed the
// primary pool. Stop the primary retry timer before dispatch so it cannot
// replace the fallback's response with an earlier primary error.
func prepareFallbackAttempt(c *gin.Context, account *auth.Account, generalLimit, rateLimit int, policy database.ContinuousRetryPolicy) (int, int, database.ContinuousRetryPolicy) {
	if account == nil || !account.IsExternalFallback() {
		return generalLimit, rateLimit, policy
	}
	policy = database.ContinuousRetryPolicy{}
	rememberContinuousRetryPolicyForRequest(c, policy)
	if c != nil {
		c.Set(fallbackTerminalAttemptContextKey, true)
		if c.Request != nil {
			if deadline := continuousRetryDeadlineForContext(c.Request.Context()); deadline != nil {
				deadline.Stop()
				// Selection itself may finish just after the primary timer fires
				// (for example while waiting for a concurrency slot).
				if continuousRetryDeadlineExceeded(c.Request.Context()) && deadline.parentContext != nil &&
					deadline.parentContext.Err() == nil && c.Writer != nil && !c.Writer.Written() {
					c.Request = c.Request.WithContext(deadline.parentContext)
				}
			}
		}
	}
	return 0, 0, policy
}

// annotateFallbackRequest carries the primary account that led to a fallback
// attempt into the request log. Fallback accounts are runtime-only and use a
// negative ID, so the normal accounts table cannot provide this relationship.
func (h *Handler) annotateFallbackRequest(c *gin.Context, state *fallbackRouteState, account *auth.Account) {
	globalFallbackMetrics.selected(c, state, account)
	if c == nil || state == nil || account == nil || !account.IsExternalFallback() {
		return
	}
	account.Mu().RLock()
	fallbackName := strings.TrimSpace(account.Name)
	account.Mu().RUnlock()
	c.Set(contextFallbackAccountName, fallbackName)
	c.Set(contextFallbackReason, state.reason)
	log.Printf("使用兜底号池账号 (account=%d, primary_attempts=%d, reason=%s)", account.ID(), state.primaryAttempts, state.reason)
	if state.sourceAccount != nil {
		source := state.sourceAccount
		source.Mu().RLock()
		sourceName := strings.TrimSpace(source.Name)
		if sourceName == "" {
			sourceName = strings.TrimSpace(source.Email)
		}
		source.Mu().RUnlock()
		c.Set(contextSourceAccountID, source.ID())
		c.Set(contextSourceAccountName, sourceName)
	}
}

func (s *fallbackRouteState) activateAfterPrimaryExhausted() bool {
	if s == nil || s.pool == nil || !s.policy.Enabled || s.active {
		return false
	}
	s.active = true
	if s.reason == "" {
		s.reason = fallbackReasonUnavailable
	}
	return true
}

func (s *fallbackRouteState) usingFallback() bool {
	return s != nil && s.active
}

func (s *fallbackRouteState) configured() bool {
	return s != nil && s.pool != nil && s.policy.Enabled && s.pool.HasEligibleAccount(s.filter)
}

// Only the primary retry deadline can be replaced by a fallback attempt.
// Preserve the real client's cancellation/deadline and never replay visible
// output, a pinned compaction domain, or an already-attempted external fallback.
func canFallbackAfterPrimaryDeadline(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Writer == nil || c.Writer.Written() ||
		!continuousRetryDeadlineExceeded(c.Request.Context()) {
		return false
	}
	value, _ := c.Get(contextFallbackDeadlineState)
	state, _ := value.(*fallbackRouteState)
	deadline := continuousRetryDeadlineForContext(c.Request.Context())
	return state != nil && !state.fallbackAttempted && state.configured() &&
		deadline != nil && deadline.parentContext != nil && deadline.parentContext.Err() == nil
}

// Called after the primary attempt has returned and released its response,
// replay buffer and account lease. Continue the same loop with a live sibling
// context instead of starting over with a new request or losing attempt IDs.
func handoffPrimaryDeadlineToFallback(c *gin.Context, state *fallbackRouteState) bool {
	if !canFallbackAfterPrimaryDeadline(c) {
		return false
	}
	deadline := continuousRetryDeadlineForContext(c.Request.Context())
	deadline.Stop()
	c.Request = c.Request.WithContext(deadline.parentContext)
	state.active = true
	state.reason = fallbackReasonRetryDeadline
	c.Set(fallbackTerminalAttemptContextKey, true)
	// The primary's synthetic access-log status must not hide fallback success.
	c.Set(AccessLogStatusContextKey, 0)
	c.Status(http.StatusOK)
	c.Header("Retry-After", "")
	log.Printf("主账号池持续重试超时，切入兜底号池 (primary_attempts=%d)", state.primaryAttempts)
	return true
}

func (s *fallbackRouteState) retryBudgets(maxRetries, maxRateLimitRetries int) (int, int) {
	if s == nil || !s.configured() {
		return maxRetries, maxRateLimitRetries
	}
	s.retryHandoffEnabled = true
	// With an eligible fallback account, RelayCount is the complete primary-pool
	// contract: it includes the first attempt and is independent from the normal
	// HTTP and rate-limit retry settings. The internal retry limit needs one
	// additional continuation slot so the attempt after the last primary failure
	// can be dispatched to the terminal fallback account.
	return s.policy.RelayCount, s.policy.RelayCount
}

func (s *fallbackRouteState) retryBudgetForAccount(maxRateLimitRetries int) int {
	if s == nil || !s.retryHandoffEnabled {
		return maxRateLimitRetries
	}
	// Per-account 429 overrides belong to the normal retry mode. Once fallback
	// routing is active, total primary attempts must be governed solely by the
	// operator's Local relay attempts setting.
	return s.policy.RelayCount
}

func (s *fallbackRouteState) primaryRateLimitBudget(defaultLimit int) int {
	if s != nil && s.retryHandoffEnabled {
		return s.policy.RelayCount - 1
	}
	return defaultLimit
}

// Called before selection as a defensive handoff check. The selected-account
// count is authoritative, so mixed general and rate-limit failures cannot
// shorten or extend RelayCount.
func (s *fallbackRouteState) activateAfterRetryBudget(generalRetries, rateLimitRetries int) {
	if s == nil || !s.retryHandoffEnabled || s.active {
		return
	}
	if s.primaryAttempts >= s.policy.RelayCount {
		s.active = true
		s.reason = fallbackReasonRelayLimit
		log.Printf("主账号池已达本地接力次数，切入兜底号池 (primary_attempts=%d, relay_count=%d, general_retries=%d, rate_limit_retries=%d)",
			s.primaryAttempts, s.policy.RelayCount, generalRetries, rateLimitRetries)
	}
}

func (s *fallbackRouteState) queueThresholdReached(store *auth.Store) bool {
	return s != nil && store != nil && s.configured() &&
		s.policy.QueueDirectFallbackThreshold > 0 &&
		store.GetSchedulerMetrics().Waiters >= int64(s.policy.QueueDirectFallbackThreshold)
}

func (h *Handler) nextFallbackAwareAccountWithGuard(
	ctx context.Context,
	state *fallbackRouteState,
	affinityKey string,
	apiKeyID int64,
	exclusions *retryAccountExclusions,
	filter auth.AccountFilter,
	policy auth.DispatchPolicy,
) (*auth.Account, string, auth.SessionAffinityGuard) {
	if h == nil || h.store == nil || state == nil {
		return nil, "", auth.SessionAffinityGuard{}
	}
	exclude := exclusions.ForSelection()
	account, proxyURL, guard := h.nextAccountForSessionWithDispatchGuard(affinityKey, apiKeyID, exclude, filter, policy)
	if account != nil {
		return account, proxyURL, guard
	}
	// A bound session account that is simply at its live concurrency ceiling
	// must not make this request enter the normal availability wait.  Spill it
	// straight to the configured external fallback, while leaving the durable
	// affinity binding intact for the next request that can use it.
	if state.configured() && h.store.SessionAffinityCapacityFull(affinityKey, apiKeyID, exclude, filter, policy) {
		state.active = true
		state.reason = fallbackReasonAffinityFull
		return state.account(exclude), "", auth.SessionAffinityGuard{}
	}
	if state.queueThresholdReached(h.store) {
		state.active = true
		state.reason = fallbackReasonQueueThreshold
		return state.account(exclude), "", auth.SessionAffinityGuard{}
	}
	if !h.store.HasDispatchCandidateWithDispatch(apiKeyID, exclude, filter, policy) {
		state.reason = fallbackReasonNoEligible
		return nil, "", auth.SessionAffinityGuard{}
	}
	account, proxyURL, guard = h.nextRetryAccountForSessionWithDispatchGuard(ctx, affinityKey, apiKeyID, exclusions, filter, policy)
	if account == nil {
		state.reason = fallbackReasonWaitEnded
	}
	return account, proxyURL, guard
}
