package proxy

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestFallbackMetricsAttemptOutcomesAndDeduplication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := newFallbackMetrics()
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	pool.Replace([]auth.FallbackAccountConfig{{ID: 1, Name: "backup", BaseURL: "https://provider.invalid", APIKey: "must-not-leak", Enabled: true}})
	a := pool.Accounts()[0]
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	state := &fallbackRouteState{}
	for i, status := range []int{503, 200, 499} {
		m.selected(c, state, a)
		// Hidden continuation and unrelated usage rows are not outer outcomes.
		observeFallbackMetricUsage(c, &database.UsageLogInput{AccountID: a.ID(), StatusCode: 200})
		observeFallbackMetricUsage(c, &database.UsageLogInput{AccountID: 55, StatusCode: 200, AttemptIndex: i + 1})
		input := &database.UsageLogInput{AccountID: a.ID(), StatusCode: status, AttemptIndex: i + 1}
		observeFallbackMetricUsage(c, input)
		observeFallbackMetricUsage(c, input)
	}
	s := m.snapshot()
	if s.FallbackHandoffCount != 1 || s.FallbackAttemptCount != 3 || s.FallbackSuccessCount != 1 || s.FallbackFailureCount != 1 || s.FallbackCanceledCount != 1 {
		t.Fatalf("incorrect attempt/request counters: %+v", s)
	}
	if len(s.Accounts) != 1 || s.Accounts[0].Name != "backup" || s.Accounts[0].Attempts != 3 || s.Accounts[0].Success != 1 || s.Accounts[0].Failure != 1 || s.Accounts[0].Canceled != 1 {
		t.Fatalf("bad provider grouping: %+v", s)
	}
	s.Accounts[0].Name = "mutated"
	if m.snapshot().Accounts[0].Name != "backup" {
		t.Fatal("snapshot aliases mutable collector state")
	}
}

func TestFallbackMetricsFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, kind, message   string
		status                int
		over, timeout, stream uint64
	}{
		{"overload", "server", "server_is_overloaded · service_unavailable_error", 503, 1, 0, 0},
		{"first_event_timeout", "timeout", "上游首字超时：30s 内未收到首个响应事件", 598, 0, 1, 0},
		{"request_watchdog", "timeout", "first token timeout after 30s", 598, 0, 1, 0},
		{"generic_timeout", "timeout", "context deadline exceeded", 598, 0, 0, 1},
		{"eof", "network", "unexpected EOF", 598, 0, 0, 1},
		{"canceled", "network", "client canceled", 499, 0, 0, 0},
		{"business_500", "server", "server_error", 500, 0, 0, 0},
		{"success_text_not_error", "", "server_is_overloaded", 200, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFallbackMetrics()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			m.selected(c, &fallbackRouteState{}, &auth.Account{DBID: 1})
			observeFallbackMetricUsage(c, &database.UsageLogInput{AccountID: 1, AttemptIndex: 1, StatusCode: tc.status, UpstreamErrorKind: tc.kind, ErrorMessage: tc.message})
			s := m.snapshot()
			if s.UpstreamOverloadedCount != tc.over || s.FirstTokenTimeoutCount != tc.timeout || s.UpstreamStreamBreakCount != tc.stream {
				t.Fatalf("bad classification: %+v", s)
			}
		})
	}
}

func TestFallbackMetricsConcurrentAndBoundedAccounts(t *testing.T) {
	m := newFallbackMetrics()
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	pool := auth.NewFallbackPool(store)
	var configs []auth.FallbackAccountConfig
	for id := 1; id <= maxFallbackMetricAccounts+20; id++ {
		configs = append(configs, auth.FallbackAccountConfig{ID: int64(id), Name: fmt.Sprint(id), BaseURL: "https://provider.invalid", APIKey: "test", Enabled: true})
	}
	pool.Replace(configs)
	var wg sync.WaitGroup
	for _, a := range pool.Accounts() {
		wg.Add(1)
		go func(a *auth.Account) {
			defer wg.Done()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			m.selected(c, &fallbackRouteState{}, a)
			observeFallbackMetricUsage(c, &database.UsageLogInput{AccountID: a.ID(), AttemptIndex: 1, StatusCode: 200})
			_ = m.snapshot()
		}(a)
	}
	wg.Wait()
	s := m.snapshot()
	if s.FallbackHandoffCount != uint64(len(configs)) || s.FallbackSuccessCount != uint64(len(configs)) || len(s.Accounts) != maxFallbackMetricAccounts+1 {
		t.Fatalf("lost concurrent events or unbounded labels: %+v", s)
	}
	var attempts, successes uint64
	for _, a := range s.Accounts {
		attempts += a.Attempts
		successes += a.Success
	}
	if attempts != s.FallbackAttemptCount || successes != s.FallbackSuccessCount {
		t.Fatal("provider totals do not reconcile")
	}
}
