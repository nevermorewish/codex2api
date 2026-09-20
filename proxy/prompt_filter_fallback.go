package proxy

import (
	"net/http"

	"github.com/codex2api/api"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const contextPromptFilterFallback = "promptFilterFallback"

// Reserve a terminal fallback route before selecting any primary account. The
// verdict remains a block for rule evidence; the audit writer records the route.
func (h *Handler) preparePromptFilterFallback(c *gin.Context, cfg promptfilter.Config, body []byte, endpoint string, evaluation promptGuardEvaluation) bool {
	if c == nil || cfg.Mode != promptfilter.ModeFallback || evaluation.Verdict.Action != promptfilter.ActionBlock {
		return false
	}
	// An unavailable reviewer is not a positive violation. Its existing failure
	// policy still applies, without converting an outage into fallback traffic.
	if evaluation.Verdict.ReviewError != "" && !evaluation.Verdict.SensitiveIntent &&
		!evaluation.Verdict.TerminalStrictHit && !evaluation.Verdict.TerminalCategoryHit {
		return false
	}
	switch endpoint {
	case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages":
	default:
		return false
	}
	if h == nil || h.fallbackPool == nil || !h.fallbackPool.Policy().Enabled || gjson.GetBytes(body, "previous_response_id").String() != "" {
		return false
	}
	// A normalization recheck after account selection cannot authorize the
	// selected primary account. Keep its block instead of silently continuing.
	if state, exists := c.Get(contextFallbackDeadlineState); exists && state != nil {
		if route, ok := state.(*fallbackRouteState); ok && route != nil {
			return false
		}
	}
	c.Set(contextPromptFilterFallback, true)
	return true
}

func requirePromptFilterFallback(c *gin.Context, state *fallbackRouteState, movable bool) string {
	if !c.GetBool(contextPromptFilterFallback) {
		return ""
	}
	if !movable {
		return "Prompt policy violation: this continuation is bound to its upstream and cannot use the fallback pool"
	}
	if state == nil || state.pool == nil || !state.policy.Enabled {
		return "Prompt policy violation: the fallback pool is unavailable"
	}
	state.active = true
	state.required = true
	state.reason = fallbackReasonContentReview
	return ""
}

func requirePromptFilterFallbackHTTP(c *gin.Context, state *fallbackRouteState, movable bool) bool {
	if message := requirePromptFilterFallback(c, state, movable); message != "" {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode("prompt_blocked"), message, api.ErrorTypeInvalidRequest), http.StatusBadRequest)
		return false
	}
	return true
}
