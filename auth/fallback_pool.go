package auth

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type FallbackAccountConfig struct {
	ID      int64
	Name    string
	BaseURL string
	APIKey  string
	// Models is the model allowlist. An empty list means no model restriction.
	Models []string
	// Model is kept for legacy configurations that used a single fixed model.
	Model       string
	ProxyURL    string
	Concurrency int
	Enabled     bool
}

type FallbackPolicy struct {
	Enabled                               bool `json:"enabled"`
	RelayCount                            int  `json:"relay_count"`
	QueueDirectFallbackThreshold          int  `json:"queue_direct_fallback_threshold"`
	OversizedRequestDirectFallbackEnabled bool `json:"oversized_request_direct_fallback_enabled"`
}

type fallbackPolicyState struct {
	policy FallbackPolicy
}

// FallbackPool owns standalone API-key accounts that never participate in
// normal credential-pool selection.
type FallbackPool struct {
	mu       sync.RWMutex
	store    *Store
	accounts map[int64]*Account
	policy   atomic.Pointer[fallbackPolicyState]
}

func NewFallbackPool(store *Store) *FallbackPool {
	pool := &FallbackPool{store: store, accounts: make(map[int64]*Account)}
	pool.SetPolicy(defaultFallbackPolicy())
	return pool
}

func defaultFallbackPolicy() FallbackPolicy {
	return FallbackPolicy{RelayCount: 3, QueueDirectFallbackThreshold: 5}
}

func (p *FallbackPool) SetPolicy(policy FallbackPolicy) {
	if p == nil {
		return
	}
	if policy.RelayCount < 1 {
		policy.RelayCount = 3
	}
	p.policy.Store(&fallbackPolicyState{policy: policy})
}

func (p *FallbackPool) Policy() FallbackPolicy {
	if p == nil {
		return defaultFallbackPolicy()
	}
	if state := p.policy.Load(); state != nil {
		return state.policy
	}
	return defaultFallbackPolicy()
}

// fallbackModelMapping preserves the legacy fixed-model wildcard mapping.
func fallbackModelMapping(model string) string {
	raw, _ := json.Marshal(map[string]string{"*": strings.TrimSpace(model)})
	return string(raw)
}

func fallbackWhitelistModelMapping(models []string) string {
	mapping := make(map[string]string, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		mapping[model] = model
	}
	raw, _ := json.Marshal(mapping)
	return string(raw)
}

func (p *FallbackPool) Replace(configs []FallbackAccountConfig) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	next := make(map[int64]*Account, len(configs))
	for _, config := range configs {
		if config.ID <= 0 || strings.TrimSpace(config.BaseURL) == "" ||
			strings.TrimSpace(config.APIKey) == "" {
			continue
		}
		models := NormalizeOpenAIResponsesModels(config.Models)
		legacyModel := strings.TrimSpace(config.Model)
		legacyFixed := len(models) == 0 && legacyModel != ""
		if legacyFixed {
			models = []string{legacyModel}
		}
		modelMapping := fallbackWhitelistModelMapping(models)
		if legacyFixed {
			modelMapping = fallbackModelMapping(legacyModel)
		}
		limit := int64(config.Concurrency)
		if limit < 1 {
			limit = 10
		}
		account := p.accounts[config.ID]
		if account == nil {
			account = &Account{
				DBID:                 -config.ID,
				ExternalFallback:     true,
				CredentialGeneration: 1,
				HealthTier:           HealthTierHealthy,
				Status:               StatusReady,
				AddedAt:              time.Now().UnixNano(),
			}
		}
		account.mu.Lock()
		configChanged := account.Name != strings.TrimSpace(config.Name) ||
			account.BaseURL != strings.TrimRight(strings.TrimSpace(config.BaseURL), "/") ||
			account.APIKey != strings.TrimSpace(config.APIKey) ||
			account.ModelMapping != modelMapping ||
			account.ProxyURL != strings.TrimSpace(config.ProxyURL) ||
			account.BaseConcurrencyOverride == nil || *account.BaseConcurrencyOverride != limit
		wasDisabled := atomic.LoadInt32(&account.Disabled) != 0
		account.Name = strings.TrimSpace(config.Name)
		account.UpstreamType = UpstreamOpenAIResponses
		account.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
		account.APIKey = strings.TrimSpace(config.APIKey)
		account.Models = models
		account.ModelMapping = modelMapping
		account.ProxyURL = strings.TrimSpace(config.ProxyURL)
		account.Email = account.BaseURL
		account.PlanType = "api"
		account.BaseConcurrencyOverride = &limit
		if config.Enabled {
			atomic.StoreInt32(&account.Disabled, 0)
			if configChanged || wasDisabled {
				account.Status = StatusReady
				account.ErrorMsg = ""
				account.CooldownUtil = time.Time{}
				account.CooldownReason = ""
				account.HealthTier = HealthTierHealthy
			}
		} else {
			atomic.StoreInt32(&account.Disabled, 1)
		}
		baseLimit := limit
		if p.store != nil && p.store.GetMaxConcurrency() > 0 {
			baseLimit = int64(p.store.GetMaxConcurrency())
		}
		account.recomputeSchedulerLocked(baseLimit)
		account.mu.Unlock()
		next[config.ID] = account
	}
	p.accounts = next
}

func (p *FallbackPool) Accounts() []*Account {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	out := make([]*Account, 0, len(p.accounts))
	for _, account := range p.accounts {
		out = append(out, account)
	}
	p.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].DBID > out[j].DBID })
	return out
}

func (p *FallbackPool) HasEligibleAccount(filter AccountFilter) bool {
	for _, account := range p.Accounts() {
		if account != nil && account.IsAvailable() && (filter == nil || filter(account)) {
			return true
		}
	}
	return false
}

func (p *FallbackPool) Acquire(exclude map[int64]bool, filter AccountFilter) *Account {
	if p == nil || p.store == nil || !p.Policy().Enabled {
		return nil
	}
	accounts := p.Accounts()
	for attempts := 0; attempts < len(accounts); attempts++ {
		var best *Account
		bestLoad := int64(^uint64(0) >> 1)
		for _, account := range accounts {
			if account == nil || (exclude != nil && exclude[account.DBID]) || !account.IsAvailable() {
				continue
			}
			if filter != nil && !filter(account) {
				continue
			}
			load := account.GetOccupiedRequests()
			// External fallback accounts intentionally do not participate in
			// local concurrency admission.  Pick the least-loaded key for
			// observability/fairness, then send immediately regardless of its
			// configured concurrency value.
			if best == nil || load < bestLoad || (load == bestLoad && account.DBID > best.DBID) {
				best, bestLoad = account, load
			}
		}
		if best == nil {
			return nil
		}
		if p.store.tryAcquireExternalFallbackAccount(best) {
			return best
		}
	}
	return nil
}
