package admin

import (
	"context"
	"encoding/json"
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
