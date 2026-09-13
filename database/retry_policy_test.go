package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRequestRetryPolicyMigration(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		enabled                 bool
		general, rate, attempts int
		mode                    string
	}{
		{"buffered", true, 2, 5, 6, RetryModeFullResponse},
		{"streaming", false, 2, 5, 6, RetryModeBeforeFirstToken},
		{"off", false, 0, 0, 1, RetryModeOff},
		{"bounded", true, 100, 100, 20, RetryModeFullResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := DefaultContinuousRetryPolicy()
			old.Enabled, old.MaxDurationSeconds = tc.enabled, 45
			got := ResolveRequestRetryPolicy(old, tc.general, tc.rate)
			if got.Mode != tc.mode || got.MaxAttempts != tc.attempts || got.Validate() != nil {
				t.Fatalf("migration = %+v", got)
			}
			if tc.enabled && got.TotalTimeoutSeconds != 45 {
				t.Fatal("lost old deadline")
			}
			old.RequestPolicy = &got
			if next := ResolveRequestRetryPolicy(old, 0, 0); next != got {
				t.Fatal("legacy counts changed unified policy")
			}
		})
	}
}

func TestRequestRetryPolicyPersistsAndSurvivesSelectorUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.sqlite")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	want := RequestRetryPolicy{Mode: RetryModeBeforeFirstToken, MaxAttempts: 4, TotalTimeoutSeconds: 123}
	if _, err := db.UpdateContinuousRetryPolicy(ctx, ContinuousRetryPolicyUpdate{RequestPolicy: &want}); err != nil {
		t.Fatal(err)
	}
	catchAll := true
	got, err := db.UpdateContinuousRetryPolicy(ctx, ContinuousRetryPolicyUpdate{CatchAll: &catchAll})
	if err != nil || got.RequestPolicy == nil || *got.RequestPolicy != want || !got.CatchAll {
		t.Fatalf("merged=%+v err=%v", got, err)
	}
	bad := want
	bad.MaxAttempts = 0
	if _, err := db.UpdateContinuousRetryPolicy(ctx, ContinuousRetryPolicyUpdate{RequestPolicy: &bad}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got = ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	if got.RequestPolicy == nil || *got.RequestPolicy != want || !got.CatchAll {
		t.Fatalf("after restart=%+v", got)
	}
}

func TestRequestRetrySelectorOnlySaveMigratesLegacy(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "retry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	catchAll := true
	got, err := db.UpdateContinuousRetryPolicy(context.Background(), ContinuousRetryPolicyUpdate{CatchAll: &catchAll})
	if err != nil || got.RequestPolicy == nil {
		t.Fatalf("selector save restored legacy runtime: %+v, %v", got, err)
	}
}
