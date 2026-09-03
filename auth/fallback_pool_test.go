package auth

import (
	"testing"

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
	if third := pool.Acquire(nil, nil); third != nil {
		t.Fatalf("full fallback pool selected unexpected account %d", third.ID())
	}
	store.Release(first)
	store.Release(second)
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
