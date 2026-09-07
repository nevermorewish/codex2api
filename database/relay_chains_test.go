package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRelayFallbackReasonPersistenceAndMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fallback-reasons.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a database written by the version before fallback reasons.
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: -3, StatusCode: 503, AttemptIndex: 1, ParentRequestID: "legacy"}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_logs DROP COLUMN fallback_reason`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: -3, StatusCode: 503, AttemptIndex: 1, ParentRequestID: "sqlite", FallbackReason: "affinity_capacity_full"}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	// Exercise the PostgreSQL multi-row VALUES builder against SQLite as well:
	// it supports the same numbered bind parameters and insert column list.
	if err := db.batchInsertLogsChunk(ctx, db.conn, []usageLogEntry{
		{AccountID: -4, StatusCode: 503, AttemptIndex: 1, ParentRequestID: "pg-builder-1", FallbackReason: "queue_threshold"},
		{AccountID: -4, StatusCode: 503, AttemptIndex: 1, ParentRequestID: "pg-builder-2", FallbackReason: "relay_limit"},
	}); err != nil {
		t.Fatal(err)
	}
	logs, total, err := db.ListRelayChainLogs(ctx, 1, 20)
	if err != nil || total != 4 || len(logs) != 4 {
		t.Fatalf("logs=%d total=%d err=%v", len(logs), total, err)
	}
	want := map[string]string{"legacy": "", "sqlite": "affinity_capacity_full", "pg-builder-1": "queue_threshold", "pg-builder-2": "relay_limit"}
	for _, row := range logs {
		if reason, ok := want[row.ParentRequestID]; !ok || row.FallbackReason != reason {
			t.Fatalf("unexpected reason for %s: %q", row.ParentRequestID, row.FallbackReason)
		}
		delete(want, row.ParentRequestID)
	}
	if len(want) != 0 {
		t.Fatalf("missing rows: %v", want)
	}
}
