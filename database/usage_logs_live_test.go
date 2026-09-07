package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestListUsageLogsByParentRequestIDs(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage-logs-live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(UsageLogModeFull, 1000, 3600)

	seed := []*UsageLogInput{
		{AccountID: 1, ParentRequestID: "live-a", AttemptIndex: 1, StatusCode: 503, IsRetryAttempt: true, DurationMs: 10},
		{AccountID: 2, ParentRequestID: "live-a", AttemptIndex: 2, StatusCode: 200, DurationMs: 20},
		{AccountID: 3, ParentRequestID: "live-b", AttemptIndex: 1, StatusCode: 429, IsRetryAttempt: true, DurationMs: 5},
		{AccountID: 4, ParentRequestID: "live-c", AttemptIndex: 1, StatusCode: 200, DurationMs: 8},
		// Internal auxiliary rounds must never surface as relay hops.
		{AccountID: 1, ParentRequestID: "live-a", InternalReason: "overflow_compact_summary", StatusCode: 200, DurationMs: 3},
	}
	for _, input := range seed {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()

	t.Run("empty input returns nothing without querying", func(t *testing.T) {
		logs, err := db.ListUsageLogsByParentRequestIDs(ctx, nil)
		if err != nil || len(logs) != 0 {
			t.Fatalf("logs=%d err=%v", len(logs), err)
		}
	})

	t.Run("matches only requested parents and excludes internal rounds", func(t *testing.T) {
		logs, err := db.ListUsageLogsByParentRequestIDs(ctx, []string{"live-a", "live-b", "does-not-exist"})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 3 {
			t.Fatalf("expected 3 rows (2 for live-a + 1 for live-b), got %d", len(logs))
		}
		byParent := map[string]int{}
		for _, row := range logs {
			if row.InternalReason != "" {
				t.Fatalf("internal round leaked into result: %+v", row)
			}
			byParent[row.ParentRequestID]++
		}
		if byParent["live-a"] != 2 || byParent["live-b"] != 1 || byParent["live-c"] != 0 {
			t.Fatalf("unexpected distribution: %+v", byParent)
		}
	})

	t.Run("batches beyond usageLogsLiveBatchSize", func(t *testing.T) {
		ids := make([]string, 0, usageLogsLiveBatchSize+50)
		for i := 0; i < usageLogsLiveBatchSize+50; i++ {
			ids = append(ids, "no-such-parent")
		}
		ids = append(ids, "live-c")
		logs, err := db.ListUsageLogsByParentRequestIDs(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 || logs[0].ParentRequestID != "live-c" {
			t.Fatalf("expected exactly the live-c row, got %+v", logs)
		}
	})
}

func TestListRecentlyEndedParentRequestIDs(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage-logs-recent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(UsageLogModeFull, 1000, 3600)

	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: 1, ParentRequestID: "recent-a", AttemptIndex: 1, StatusCode: 200, DurationMs: 5}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: 2, ParentRequestID: "recent-b", AttemptIndex: 1, StatusCode: 503, IsRetryAttempt: true, DurationMs: 5}); err != nil {
		t.Fatal(err)
	}
	// Internal auxiliary rounds are not relay hops and must not appear as a
	// distinct "recently ended" parent request.
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: 1, ParentRequestID: "recent-a", InternalReason: "overflow_compact_summary", StatusCode: 200, DurationMs: 2}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()

	since := time.Now().Add(-time.Minute)
	ids, err := db.ListRecentlyEndedParentRequestIDs(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["recent-a"] || !seen["recent-b"] {
		t.Fatalf("missing recent parents: %v", ids)
	}

	future := time.Now().Add(time.Minute)
	ids, err = db.ListRecentlyEndedParentRequestIDs(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no rows from a future window, got %v", ids)
	}
}
