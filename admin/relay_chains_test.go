package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

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
		{AccountID: 22, ParentRequestID: "req-relay-1", Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: http.StatusOK, DurationMs: 25, AttemptIndex: 2},
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
	for page, want := range []int{20, 20, 2, 0} {
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
		if response.Total != 42 || response.PageSize != 20 || response.Page != page+1 || len(response.Chains) != want {
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
	if len(seen) != 42 {
		t.Fatalf("unique chains = %d, want 42", len(seen))
	}
}
