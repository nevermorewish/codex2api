package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

func lifecycleRow(t *testing.T, ctx context.Context) RequestLifecycle {
	t.Helper()
	id := requestEntry(ctx).row.ID
	for _, r := range SnapshotLifecycle().Requests {
		if r.ID == id {
			return r
		}
	}
	t.Fatal("request missing")
	return RequestLifecycle{}
}

func TestRequestLifecycleWaitAttemptAndCleanup(t *testing.T) {
	ctx, finish := BeginRequest(context.Background(), "test-lifecycle", "/v1/responses", 7)
	defer finish(200)
	if r := lifecycleRow(t, ctx); r.Attempt != 0 || r.State != LifecyclePreparing {
		t.Fatalf("initial: %+v", r)
	}
	release := WaitRequest(ctx, LifecycleScheduler)
	if r := lifecycleRow(t, ctx); r.State != LifecycleScheduler {
		t.Fatal(r)
	}
	// Move the clock through stored start times, avoiding timing-sensitive sleeps.
	requestTracker.mu.Lock()
	requestEntry(ctx).row.StateStartedAt = time.Now().Add(-2 * time.Second)
	requestTracker.mu.Unlock()
	release()
	release()
	StartRequestAttempt(ctx, 1, false, 1, "test-model")
	RequestFirstToken(ctx, 25)
	RequestFirstToken(ctx, 90)
	if r := lifecycleRow(t, ctx); r.SchedulerWaitMS < 2000 || r.FirstTokenMS == nil || *r.FirstTokenMS != 25 || r.State != LifecycleUpstream {
		t.Fatal(r)
	}
	EndRequestAttempt(ctx, 503)
	retryDone := WaitRequest(ctx, LifecycleRetry)
	StartRequestAttempt(ctx, -9, true, 2, "test-model")
	retryDone() // stale cleanup must not revert a newer attempt
	if r := lifecycleRow(t, ctx); r.State != LifecycleFallback || r.FirstTokenMS != nil || !r.WaitingFirstToken || r.Attempt != 2 || r.Retries != 1 {
		t.Fatal(r)
	}
	EndRequestAttempt(ctx, 200)
	finish(0)
	finish(500)
	for _, r := range SnapshotLifecycle().Requests {
		if r.RequestID == "test-lifecycle" {
			t.Fatal("leaked request")
		}
	}
	RequestFirstToken(ctx, 99)
	StartRequestAttempt(ctx, 2, false, 3, "")
	for _, r := range SnapshotLifecycle().Recent {
		if r.RequestID == "test-lifecycle" && (r.StatusCode != 200 || r.Attempt != 2) {
			t.Fatal(r)
		}
	}
}

func TestRequestLifecycleSnapshotsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				ctx, end := BeginRequest(context.Background(), "concurrent", "/v1/responses", 0)
				StartRequestAttempt(ctx, 1, false, 1, "")
				RequestFirstToken(ctx, 1)
				s := SnapshotLifecycle()
				sum := s.Preparing + s.SchedulerWaiters + s.RetryWaiters + s.UpstreamActive + s.FallbackActive + s.Finishing
				if sum != s.TotalInflight {
					t.Errorf("inconsistent state sum: %d vs %d", sum, s.TotalInflight)
				}
				end(499)
			}
		}()
	}
	wg.Wait()
}
