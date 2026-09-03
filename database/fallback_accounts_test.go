package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFallbackAccountsSQLiteCRUDAndPolicy(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	policy, err := db.GetFallbackPolicy(ctx)
	if err != nil {
		t.Fatalf("GetFallbackPolicy: %v", err)
	}
	if policy.Enabled || policy.RelayCount != 3 || policy.QueueDirectFallbackThreshold != 5 || policy.OversizedRequestDirectFallbackEnabled {
		t.Fatalf("default policy = %+v, want disabled relay_count=3 queue_threshold=5 oversized=false", policy)
	}

	created, err := db.CreateFallbackAccount(ctx, &FallbackAccountRow{
		Name: "backup", Protocol: FallbackProtocolOpenAIResponses,
		BaseURL: "https://example.com/v1", APIKey: "sk-secret", Model: "gpt-backup",
		ProxyURL: "socks5://127.0.0.1:1080", Concurrency: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateFallbackAccount: %v", err)
	}
	if created.ID <= 0 || created.APIKey != "sk-secret" || created.Model != "gpt-backup" {
		t.Fatalf("created row = %+v", created)
	}

	created.Name = "backup-updated"
	created.APIKey = "sk-replaced"
	created.Concurrency = 11
	created.Enabled = false
	updated, err := db.UpdateFallbackAccount(ctx, created)
	if err != nil {
		t.Fatalf("UpdateFallbackAccount: %v", err)
	}
	if updated.Name != "backup-updated" || updated.APIKey != "sk-replaced" || updated.Concurrency != 11 || updated.Enabled {
		t.Fatalf("updated row = %+v", updated)
	}

	rows, err := db.ListFallbackAccounts(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListFallbackAccounts = %d rows, err=%v", len(rows), err)
	}
	if err := db.UpdateFallbackPolicy(ctx, FallbackPolicy{
		Enabled: true, RelayCount: 5, QueueDirectFallbackThreshold: 9,
		OversizedRequestDirectFallbackEnabled: true,
	}); err != nil {
		t.Fatalf("UpdateFallbackPolicy: %v", err)
	}
	policy, err = db.GetFallbackPolicy(ctx)
	if err != nil || !policy.Enabled || policy.RelayCount != 5 || policy.QueueDirectFallbackThreshold != 9 || !policy.OversizedRequestDirectFallbackEnabled {
		t.Fatalf("updated policy = %+v, err=%v", policy, err)
	}
	if err := db.UpdateFallbackPolicy(ctx, FallbackPolicy{Enabled: true, RelayCount: 0}); err == nil {
		t.Fatal("invalid relay_count should be rejected")
	}
	if err := db.UpdateFallbackPolicy(ctx, FallbackPolicy{Enabled: true, RelayCount: 1001}); err == nil {
		t.Fatal("relay_count above 1000 should be rejected")
	}
	if err := db.UpdateFallbackPolicy(ctx, FallbackPolicy{Enabled: true, RelayCount: 3, QueueDirectFallbackThreshold: -1}); err == nil {
		t.Fatal("negative queue threshold should be rejected")
	}
	if err := db.UpdateFallbackPolicy(ctx, FallbackPolicy{Enabled: true, RelayCount: 3, QueueDirectFallbackThreshold: 1001}); err == nil {
		t.Fatal("queue threshold above 1000 should be rejected")
	}

	if err := db.DeleteFallbackAccount(ctx, created.ID); err != nil {
		t.Fatalf("DeleteFallbackAccount: %v", err)
	}
	rows, err = db.ListFallbackAccounts(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after delete = %d, err=%v", len(rows), err)
	}
}
