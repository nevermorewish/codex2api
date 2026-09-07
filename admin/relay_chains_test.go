package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRelayChainStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    *database.UsageLog
		active bool
		want   string
	}{
		{"retry running", &database.UsageLog{StatusCode: 500, IsRetryAttempt: true}, true, "in_progress"},
		{"interrupted retry", &database.UsageLog{StatusCode: 500, IsRetryAttempt: true}, false, "incomplete"},
		{"canceled", &database.UsageLog{StatusCode: 499}, false, "canceled"},
		{"cancel before cleanup", &database.UsageLog{StatusCode: 499}, true, "canceled"},
		{"success before cleanup", &database.UsageLog{StatusCode: 200}, true, "success"},
		{"terminal error", &database.UsageLog{StatusCode: 503}, false, "failed"},
		{"missing", nil, false, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayChainStatus(tc.row, tc.active); got != tc.want {
				t.Fatalf("status=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestGetRelayChainsPreservesCanceledFallback(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relay-canceled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	for i, id := range []int64{1, 2, 3, -4} {
		status, reason := 500, ""
		if i == 3 {
			status, reason = 499, "relay_limit"
		}
		if err := db.InsertUsageLog(context.Background(), &database.UsageLogInput{
			AccountID: id, ParentRequestID: "cancel-fallback", AttemptIndex: i + 1,
			StatusCode: status, IsRetryAttempt: i < 3, DurationMs: 10, FallbackReason: reason,
		}); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/relay-chains", nil)
	(&Handler{db: db}).GetRelayChains(c)
	var response struct {
		Chains []relayChainResponse `json:"chains"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || len(response.Chains) != 1 {
		t.Fatalf("response=%s", recorder.Body.String())
	}
	chain := response.Chains[0]
	if len(chain.Attempts) != 4 || chain.SwitchCount != 3 || chain.Status != "canceled" || chain.FinalOK || chain.TotalMs != 40 {
		t.Fatalf("chain lost canceled hop: %+v", chain)
	}
	last := chain.Attempts[3]
	if last.StatusCode != 499 || last.Decision != "canceled" || !last.Fallback || last.FallbackReason != "relay_limit" {
		t.Fatalf("canceled fallback lost attribution: %+v", last)
	}
}

func TestRelayAccountName(t *testing.T) {
	names := map[int64]string{-3: " zwenooo ", -4: "nexaxis.ai"}
	tests := []struct {
		name     string
		row      *database.UsageLog
		want     string
		fallback bool
	}{
		{"nil", nil, "", false},
		{"current fallback name", &database.UsageLog{AccountID: -3, FallbackAccountName: "old label"}, "zwenooo", true},
		{"historical missing label", &database.UsageLog{AccountID: -4}, "nexaxis.ai", true},
		{"deleted with snapshot", &database.UsageLog{AccountID: -5, FallbackAccountName: " archived backup "}, "archived backup", true},
		{"deleted without snapshot", &database.UsageLog{AccountID: -6}, "兜底账号 #6", true},
		{"minimum ID", &database.UsageLog{AccountID: -9223372036854775808}, "兜底账号 #9223372036854775808", true},
		{"legacy fallback channel", &database.UsageLog{Channel: " Fallback ", AccountName: "source", FallbackAccountName: "backup"}, "backup", true},
		{"primary ID collision", &database.UsageLog{AccountID: 3, AccountName: "local"}, "local", false},
		{"primary email", &database.UsageLog{AccountID: 3, AccountEmail: "local@example.test"}, "local@example.test", false},
		{"primary unnamed", &database.UsageLog{AccountID: 3}, "账号 #3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := relayAccountName(tt.row, names); got != tt.want {
				t.Fatalf("relayAccountName = %q, want %q", got, tt.want)
			}
			if got := isFallbackRelayAttempt(tt.row); got != tt.fallback {
				t.Fatalf("fallback = %v, want %v", got, tt.fallback)
			}
		})
	}
}

func TestGetRelayChainsResolvesFallbackNamesForHistoricalRows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relay-fallback-names.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	// Older rows only contain the negative runtime ID, not a name snapshot.
	for i, id := range []int64{3, -3, -4} {
		if err := db.InsertUsageLog(context.Background(), &database.UsageLogInput{
			AccountID: id, ParentRequestID: "historical-fallback", Endpoint: "/v1/responses",
			AttemptIndex: i + 1, StatusCode: 503, IsRetryAttempt: i < 2,
		}); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	pool := auth.NewFallbackPool(nil)
	configs := []auth.FallbackAccountConfig{
		{ID: 3, Name: "zwenooo", BaseURL: "https://fallback.example.test", APIKey: "test-key", Enabled: true},
		{ID: 4, Name: "nexaxis.ai", BaseURL: "https://fallback.example.test", APIKey: "test-key", Enabled: false},
	}
	pool.Replace(configs)
	h := &Handler{db: db, fallbackPool: pool}
	for _, renamed := range []bool{false, true} {
		if renamed {
			configs[0].Name = "renamed backup"
			pool.Replace(configs)
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/relay-chains", nil)
		h.GetRelayChains(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Chains []relayChainResponse `json:"chains"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Chains) != 1 || len(response.Chains[0].Attempts) != 3 {
			t.Fatalf("unexpected chains: %+v", response.Chains)
		}
		chain := response.Chains[0]
		for i, want := range []string{"账号 #3", configs[0].Name, "nexaxis.ai"} {
			attempt := chain.Attempts[i]
			if attempt.AccountName != want || attempt.Fallback != (i > 0) {
				t.Fatalf("attempt %d = %+v, want name=%q fallback=%v", i, attempt, want, i > 0)
			}
		}
		if chain.SwitchCount != 2 || chain.FinalOK {
			t.Fatalf("unexpected summary: %+v", chain)
		}
	}
}

func TestGetRelayChainsAggregatesAttemptsAndSkipsInternalRows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relay-chains.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)

	ctx := context.Background()
	inputs := []*database.UsageLogInput{
		{AccountID: 11, ParentRequestID: "req-relay-1", Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: http.StatusTooManyRequests, DurationMs: 15, IsRetryAttempt: true, AttemptIndex: 1},
		{AccountID: -22, ParentRequestID: "req-relay-1", Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: http.StatusOK, DurationMs: 25, AttemptIndex: 2, FallbackReason: "queue_threshold"},
		{AccountID: 11, ParentRequestID: "req-relay-1", Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: http.StatusOK, DurationMs: 5, InternalReason: "overflow_compact"},
		{AccountID: 22, ParentRequestID: "req-relay-1", Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: http.StatusOK, DurationMs: 5},
	}
	for _, input := range inputs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/relay-chains?limit=10", nil)
	ctxGin, _ := gin.CreateTestContext(recorder)
	ctxGin.Request = request
	(&Handler{db: db}).GetRelayChains(ctxGin)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Chains []relayChainResponse `json:"chains"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Chains) != 1 {
		t.Fatalf("chains = %d, want 1: %+v", len(response.Chains), response.Chains)
	}
	chain := response.Chains[0]
	if chain.RequestID != "req-relay-1" || len(chain.Attempts) != 2 {
		t.Fatalf("chain identity/attempts = (%q, %d), want (req-relay-1, 2)", chain.RequestID, len(chain.Attempts))
	}
	if chain.SwitchCount != 1 || !chain.FinalOK || chain.TotalMs != 40 {
		t.Fatalf("chain summary = switches=%d final_ok=%v total_ms=%d, want 1/true/40", chain.SwitchCount, chain.FinalOK, chain.TotalMs)
	}
	if chain.Attempts[0].FallbackReason != "" || chain.Attempts[1].FallbackReason != "queue_threshold" || !chain.Attempts[1].Fallback {
		t.Fatalf("fallback reason missing or leaked into primary: %+v", chain.Attempts)
	}
}

func TestGetRelayChainsFiltersBeforePagingCompleteRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "relay-pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 2000, 3600)
	insert := func(input *database.UsageLogInput) {
		t.Helper()
		input.Endpoint = "/v1/responses"
		if err := db.InsertUsageLog(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	// Include same-account retries, switched accounts, single failures and legacy failures.
	for i := 0; i < 41; i++ {
		id := fmt.Sprintf("relay-%02d", i)
		insert(&database.UsageLogInput{ParentRequestID: id, AccountID: 11, AttemptIndex: 1, StatusCode: 503, IsRetryAttempt: i%2 == 0})
		if i%2 == 0 {
			insert(&database.UsageLogInput{ParentRequestID: id, AccountID: int64(11 + i%3), AttemptIndex: 2, StatusCode: 200})
		}
	}
	insert(&database.UsageLogInput{StatusCode: 500})
	// These rows must not consume page slots or hide older relay history.
	for i := 0; i < 900; i++ {
		id := fmt.Sprintf("success-%d", i)
		insert(&database.UsageLogInput{ParentRequestID: id, AttemptIndex: 1, StatusCode: 200})
		insert(&database.UsageLogInput{ParentRequestID: id, StatusCode: 200})
	}
	insert(&database.UsageLogInput{StatusCode: 200})
	insert(&database.UsageLogInput{ParentRequestID: "internal", StatusCode: 500, InternalReason: "overflow_compact"})
	insert(&database.UsageLogInput{ParentRequestID: "cancelled", StatusCode: 499})
	db.FlushUsageLogs()
	seen := make(map[string]bool)
	for page, want := range []int{20, 20, 3, 0} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/dashboard/relay-chains?page=%d", page+1), nil)
		(&Handler{db: db}).GetRelayChains(c)
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d: %s", page+1, recorder.Body.String())
		}
		var response struct {
			Chains   []relayChainResponse `json:"chains"`
			Total    int                  `json:"total"`
			Page     int                  `json:"page"`
			PageSize int                  `json:"page_size"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Total != 43 || response.PageSize != 20 || response.Page != page+1 || len(response.Chains) != want {
			t.Fatalf("page %d: total=%d page=%d size=%d chains=%d", page+1, response.Total, response.Page, response.PageSize, len(response.Chains))
		}
		for _, chain := range response.Chains {
			if seen[chain.RequestID] {
				t.Fatalf("duplicate chain across pages: %s", chain.RequestID)
			}
			seen[chain.RequestID] = true
			if chain.FinalOK && len(chain.Attempts) < 2 {
				t.Fatalf("single success or truncated chain: %+v", chain)
			}
		}
	}
	if len(seen) != 43 {
		t.Fatalf("unique chains = %d, want 43", len(seen))
	}
}
