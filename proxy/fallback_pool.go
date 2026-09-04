package proxy

import (
	"context"
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
	if s.policy.Enabled && s.primaryAttempts >= s.policy.RelayCount {
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
	if s == nil || !s.configured() || s.policy.RelayCount < 1 {
		return maxRetries, maxRateLimitRetries
	}
	if maxRetries >= 0 && maxRetries < s.policy.RelayCount {
		maxRetries = s.policy.RelayCount
	}
	if maxRateLimitRetries >= 0 && maxRateLimitRetries < s.policy.RelayCount {
		maxRateLimitRetries = s.policy.RelayCount
	}
	return maxRetries, maxRateLimitRetries
}

func (s *fallbackRouteState) retryBudgetForAccount(maxRateLimitRetries int) int {
	if s == nil || !s.configured() || maxRateLimitRetries < 0 || maxRateLimitRetries >= s.policy.RelayCount {
		return maxRateLimitRetries
	}
	return s.policy.RelayCount
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
