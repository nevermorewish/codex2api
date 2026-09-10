package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const fallbackUsageMissingMessage = "Fallback upstream completed without billable usage (upstream_usage_missing)"

// Validate before translating or writing the terminal, not after the stream has
// already published response.completed / [DONE]. Existing failure handling then
// returns HTTP 502 before output, or a protocol error after output has started.
func validateFallbackTerminalEvent(account *auth.Account, eventType string, data []byte) (string, []byte) {
	if !isResponsesSuccessTerminalEvent(eventType) ||
		!fallbackSucceededWithoutUsage(account, streamOutcome{logStatusCode: http.StatusOK}, extractUsageFromResult(gjson.GetBytes(data, "response.usage")), false) {
		return eventType, data
	}
	root := gjson.GetBytes(data, "response")
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": root.Get("id").String(), "object": "response",
			"created_at": root.Get("created_at").Int(),
			"status":     "failed", "status_code": http.StatusBadGateway,
			"error": map[string]any{
				"type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamUsageMissing,
				"message": fallbackUsageMissingMessage,
			},
		},
	})
	return "response.failed", payload
}
