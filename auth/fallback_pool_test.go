package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func testFallbackConfigs() []FallbackAccountConfig {
	return []FallbackAccountConfig{
		{ID: 1, Name: "first", BaseURL: "https://one.example", APIKey: "sk-one", Model: "model-one", Concurrency: 1, Enabled: true},
		{ID: 2, Name: "second", BaseURL: "https://two.example", APIKey: "sk-two", Model: "model-two", Concurrency: 1, Enabled: true},
		{ID: 3, Name: "disabled", BaseURL: "https://three.example", APIKey: "sk-three", Model: "model-three", Concurrency: 10, Enabled: false},
	}
}

func TestFallbackPoolPolicySelectionAndConcurrency(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 20})
	pool := NewFallbackPool(store)
	pool.Replace(testFallbackConfigs())

	if account := pool.Acquire(nil, nil); account != nil {
		t.Fatal("disabled fallback policy should not select an account")
	}
	pool.SetPolicy(FallbackPolicy{Enabled: true, RelayCount: 2})

	first := pool.Acquire(nil, nil)
	if first == nil || first.ID() != -1 || !first.IsExternalFallback() {
		t.Fatalf("first selection = %#v, want runtime account -1", first)
	}
	if first.GetActiveRequests() != 1 || first.GetDynamicConcurrencyLimit() != 1 {
		t.Fatalf("first runtime = active:%d limit:%d", first.GetActiveRequests(), first.GetDynamicConcurrencyLimit())
	}
	second := pool.Acquire(nil, nil)
	if second == nil || second.ID() != -2 {
		t.Fatalf("second selection = %#v, want least-loaded runtime account -2", second)
	}
	third := pool.Acquire(nil, nil)
	if third == nil || !third.IsExternalFallback() {
		t.Fatal("fallback pool must continue dispatching beyond the configured local concurrency")
	}
	store.Release(first)
	store.Release(second)
	store.Release(third)
}

func TestFallbackPoolWildcardMappingAndReloadPreservesLease(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 20})
	pool := NewFallbackPool(store)
	configs := testFallbackConfigs()[:1]
	pool.Replace(configs)
	pool.SetPolicy(FallbackPolicy{Enabled: true, RelayCount: 3})

	account := pool.Acquire(nil, nil)
	if account == nil {
		t.Fatal("expected fallback account")
	}
	if account.ModelMapping != `{"*":"model-one"}` {
		t.Fatalf("model mapping = %q", account.ModelMapping)
	}
	pool.Replace(configs)
	reloaded := pool.Accounts()[0]
	if reloaded != account {
		t.Fatal("unchanged reload should preserve the runtime account pointer")
	}
	if reloaded.GetActiveRequests() != 1 {
		t.Fatalf("active requests after reload = %d, want 1", reloaded.GetActiveRequests())
	}
	store.Release(reloaded)
}

func TestFallbackPoolHonorsExclusionsAndFilter(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 20})
	pool := NewFallbackPool(store)
	pool.Replace(testFallbackConfigs())
	pool.SetPolicy(FallbackPolicy{Enabled: true, RelayCount: 3})

	account := pool.Acquire(map[int64]bool{-1: true}, func(candidate *Account) bool {
		return candidate.ID() == -2
	})
	if account == nil || account.ID() != -2 {
		t.Fatalf("filtered selection = %#v, want -2", account)
	}
	store.Release(account)
}

func TestFallbackAccountFailureDoesNotChangeLocalHealth(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	pool := NewFallbackPool(store)
	pool.Replace(testFallbackConfigs()[:1])
	account := pool.Accounts()[0]

	store.ReportRequestFailure(account, "server", time.Second)

	if !account.IsAvailable() {
		account.Mu().RLock()
		status := account.Status
		account.Mu().RUnlock()
		t.Fatalf("fallback account became unavailable after failure: status=%v", status)
	}
	account.Mu().RLock()
	streak := account.FailureStreak
	account.Mu().RUnlock()
	if streak != 0 {
		t.Fatalf("fallback failure was recorded in local health: streak=%d", streak)
	}
}

func TestFallbackPoolWhitelistModelsAndUnrestrictedEmptyList(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 20})
	pool := NewFallbackPool(store)
	pool.Replace([]FallbackAccountConfig{
		{ID: 1, Name: "limited", BaseURL: "https://one.example", APIKey: "sk-one", Models: []string{"gpt-a", "gpt-b"}, Concurrency: 2, Enabled: true},
		{ID: 2, Name: "all", BaseURL: "https://two.example", APIKey: "sk-two", Models: []string{}, Concurrency: 2, Enabled: true},
	})
	pool.SetPolicy(FallbackPolicy{Enabled: true, RelayCount: 1})
	accounts := pool.Accounts()
	var limited, unrestricted *Account
	for _, account := range accounts {
		if account.ID() == -1 {
			limited = account
		} else if account.ID() == -2 {
			unrestricted = account
		}
	}
	if limited == nil || unrestricted == nil {
		t.Fatalf("fallback accounts = %#v", accounts)
	}
	if limited.ModelMapping != `{"gpt-a":"gpt-a","gpt-b":"gpt-b"}` {
		t.Fatalf("whitelist mapping = %q", limited.ModelMapping)
	}
	if unrestricted.ModelMapping != `{}` {
		t.Fatalf("empty whitelist mapping = %q", unrestricted.ModelMapping)
	}
	if !fallbackAccountSupportsModelForTest(limited, "gpt-a") || fallbackAccountSupportsModelForTest(limited, "gpt-c") {
		t.Fatal("limited fallback model whitelist was not enforced")
	}
	if !fallbackAccountSupportsModelForTest(unrestricted, "gpt-c") {
		t.Fatal("empty fallback model whitelist should allow the requested model")
	}
}

func fallbackAccountSupportsModelForTest(account *Account, model string) bool {
	if account == nil || strings.TrimSpace(model) == "" {
		return false
	}
	models := account.OpenAIResponsesModels()
	if len(models) == 0 {
		return true
	}
	for _, candidate := range models {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(model)) {
			return true
		}
	}
	return false
}
