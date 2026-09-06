package proxy

import (
	"context"
	"log"
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const oversizedDirectFallbackBytes = 3 << 20

type fallbackRouteState struct {
	pool            *auth.FallbackPool
	filter          auth.AccountFilter
	policy          auth.FallbackPolicy
	primaryAttempts int
	active          bool
	required        bool
	sourceAccount   *auth.Account
	// Keep the operator's primary budgets separate from the extra transition
	// attempt. RelayCount may switch earlier, but must never extend these limits.
	retryHandoffEnabled        bool
	primaryMaxRetries          int
	primaryMaxRateLimitRetries int
	accountMaxRateLimitRetries int
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
		state.required = true
	}
	return state
}

func (s *fallbackRouteState) account(exclude map[int64]bool) *auth.Account {
	if s == nil || !s.active || s.pool == nil || !s.policy.Enabled {
		return nil
	}
	return s.pool.Acquire(exclude, s.filter)
}

func (s *fallbackRouteState) noteSelected(account *auth.Account) {
	if s == nil || account == nil || account.IsExternalFallback() {
		return
	}
	s.primaryAttempts++
	s.sourceAccount = account
	if s.configured() && s.primaryAttempts >= s.policy.RelayCount {
		s.active = true
	}
}

// annotateFallbackRequest carries the primary account that led to a fallback
// attempt into the request log. Fallback accounts are runtime-only and use a
// negative ID, so the normal accounts table cannot provide this relationship.
func (h *Handler) annotateFallbackRequest(c *gin.Context, state *fallbackRouteState, account *auth.Account) {
	if c == nil || state == nil || account == nil || !account.IsExternalFallback() {
		return
	}
	account.Mu().RLock()
	fallbackName := strings.TrimSpace(account.Name)
	account.Mu().RUnlock()
	c.Set(contextFallbackAccountName, fallbackName)
	log.Printf("使用兜底号池账号 (account=%d, primary_attempts=%d)", account.ID(), state.primaryAttempts)
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
	return true
}

func (s *fallbackRouteState) usingFallback() bool {
	return s != nil && s.active
}

func (s *fallbackRouteState) configured() bool {
	return s != nil && s.pool != nil && s.policy.Enabled && s.pool.HasEligibleAccount(s.filter)
}

func (s *fallbackRouteState) retryBudgets(maxRetries, maxRateLimitRetries int) (int, int) {
	if s == nil || !s.configured() {
		return maxRetries, maxRateLimitRetries
	}
	s.retryHandoffEnabled = true
	s.primaryMaxRetries = maxRetries
	s.primaryMaxRateLimitRetries = maxRateLimitRetries
	s.accountMaxRateLimitRetries = maxRateLimitRetries
	return reserveFallbackTransition(maxRetries), reserveFallbackTransition(maxRateLimitRetries)
}

func (s *fallbackRouteState) retryBudgetForAccount(maxRateLimitRetries int) int {
	if s == nil || !s.retryHandoffEnabled {
		return maxRateLimitRetries
	}
	s.accountMaxRateLimitRetries = maxRateLimitRetries
	return reserveFallbackTransition(maxRateLimitRetries)
}

func (s *fallbackRouteState) primaryRateLimitBudget(defaultLimit int) int {
	if s != nil && s.retryHandoffEnabled {
		return s.primaryMaxRateLimitRetries
	}
	return defaultLimit
}

func reserveFallbackTransition(limit int) int {
	if limit < 0 {
		return limit
	}
	return limit + 1
}

// Called only when a retryable, uncommitted attempt has elected to continue.
// N retries means the initial attempt plus N retries; the next attempt belongs
// to the fallback pool, even if healthy primary accounts are still available.
func (s *fallbackRouteState) activateAfterRetryBudget(generalRetries, rateLimitRetries int) {
	if s == nil || !s.retryHandoffEnabled || s.active {
		return
	}
	if (s.primaryMaxRetries >= 0 && (generalRetries > s.primaryMaxRetries || s.primaryAttempts > s.primaryMaxRetries)) ||
		(s.accountMaxRateLimitRetries >= 0 && rateLimitRetries > s.accountMaxRateLimitRetries) {
		s.active = true
		log.Printf("主账号池重试预算耗尽，切入兜底号池 (primary_attempts=%d, general_retries=%d, max_retries=%d, rate_limit_retries=%d, max_rate_limit_retries=%d)",
			s.primaryAttempts, generalRetries, s.primaryMaxRetries, rateLimitRetries, s.accountMaxRateLimitRetries)
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
		return state.account(exclude), "", auth.SessionAffinityGuard{}
	}
	if state.queueThresholdReached(h.store) {
		state.active = true
		return state.account(exclude), "", auth.SessionAffinityGuard{}
	}
	if !h.store.HasDispatchCandidateWithDispatch(apiKeyID, exclude, filter, policy) {
		return nil, "", auth.SessionAffinityGuard{}
	}
	return h.nextRetryAccountForSessionWithDispatchGuard(ctx, affinityKey, apiKeyID, exclusions, filter, policy)
}
