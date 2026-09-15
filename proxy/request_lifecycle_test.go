package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func waitLifecycle(t *testing.T, id, state string) auth.RequestLifecycle {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range auth.SnapshotLifecycle().Requests {
			if r.RequestID == id && r.State == state {
				return r
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never entered %s", id, state)
	return auth.RequestLifecycle{}
}
func TestRequestLifecycleSchedulerCancelAndRetryCancel(t *testing.T) {
	store := newFallbackQueueTestStore()
	defer store.Stop()
	lease := store.NextExcluding(0, nil)
	if lease == nil {
		t.Fatal("no account")
	}
	defer store.Release(lease)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, finish := auth.BeginRequest(parent, "cancel-queue", "/v1/responses", 0)
	defer finish(499)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a := store.WaitForAvailable(ctx, time.Minute, 0)
		if a != nil {
			store.Release(a)
		}
	}()
	waitLifecycle(t, "cancel-queue", auth.LifecycleScheduler)
	cancel()
	<-done
	if store.GetSchedulerMetrics().Waiters != 0 {
		t.Fatal("scheduler leaked")
	}
	parent, cancelRetry := context.WithCancel(context.Background())
	defer cancelRetry()
	ctx, finishRetry := auth.BeginRequest(parent, "cancel-retry", "/v1/responses", 0)
	defer finishRetry(499)
	done = make(chan struct{})
	go func() { defer close(done); waitForRetryInterval(ctx, time.Minute) }()
	waitLifecycle(t, "cancel-retry", auth.LifecycleRetry)
	cancelRetry()
	<-done
	waitLifecycle(t, "cancel-retry", auth.LifecyclePreparing)
}

func TestRequestLifecycleMiddlewareScopeAndWebSocketTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestLifecycleMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		end := beginRelayRequest(c)
		defer end()
		if !auth.HasRequestLifecycle(c.Request.Context()) {
			t.Error("not tracked")
		}
		c.Status(401)
	})
	r.GET("/v1/responses", func(c *gin.Context) {
		if auth.HasRequestLifecycle(c.Request.Context()) {
			t.Error("idle WS tracked")
		}
		for i := 0; i < 2; i++ {
			end := beginRelayRequest(c)
			if !auth.HasRequestLifecycle(c.Request.Context()) {
				t.Error("turn missing")
			}
			end()
			if auth.HasRequestLifecycle(c.Request.Context()) {
				t.Error("turn leaked")
			}
		}
	})
	r.GET("/admin/", func(c *gin.Context) {
		if auth.HasRequestLifecycle(c.Request.Context()) {
			t.Error("admin tracked")
		}
	})
	before := auth.SnapshotLifecycle().TotalInflight
	for _, tc := range []struct{ method, path string }{{"POST", "/v1/responses"}, {"GET", "/v1/responses"}, {"GET", "/admin/"}} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
	}
	if after := auth.SnapshotLifecycle().TotalInflight; after != before {
		t.Fatalf("leaked: %d to %d", before, after)
	}
}

func TestRequestLifecycleDirectDispatchAndFirstToken(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Request-ID", "direct-lifecycle")
	finish := beginRelayRequest(c)
	defer finish()
	a := &auth.Account{DBID: 5}
	c.Set("raw_body", []byte(`{"model":"gpt-test"}`))
	startRequestAttempt(c, a, 1)
	row := waitLifecycle(t, "direct-lifecycle", auth.LifecycleUpstream)
	if row.Attempt != 1 || row.Model != "gpt-test" || row.FirstTokenMS != nil || !row.WaitingFirstToken {
		t.Fatal(row)
	}
	auth.RequestFirstToken(c.Request.Context(), 42)
	row = waitLifecycle(t, "direct-lifecycle", auth.LifecycleUpstream)
	if row.WaitingFirstToken || row.FirstTokenMS == nil || *row.FirstTokenMS != 42 {
		t.Fatal(row)
	}
}
