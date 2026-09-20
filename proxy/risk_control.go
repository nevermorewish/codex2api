package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// HTTP adapters can inspect the same body more than once during conversion.
// Cache only an identical body/model/protocol in this request. WebSocket turns
// intentionally call checkRiskControl directly and never reuse this result.
func (h *Handler) checkRiskControlHTTP(c *gin.Context, body []byte, endpoint, model string) riskcontrol.Decision {
	if c == nil || h == nil || h.riskControl == nil {
		return riskcontrol.Decision{}
	}
	if !h.riskControl.Enabled() {
		return h.checkRiskControl(c, nil, endpoint, model)
	}
	key := fmt.Sprintf("risk_control.http.%x", sha256.Sum256(append([]byte(endpoint+"\x00"+model+"\x00"), body...)))
	if value, ok := c.Get(key); ok {
		if d, ok := value.(riskcontrol.Decision); ok {
			return d
		}
	}
	d := h.checkRiskControl(c, body, endpoint, model)
	c.Set(key, d)
	return d
}

func (h *Handler) SetRiskControl(s *riskcontrol.Service) { h.riskControl = s }
func (h *Handler) checkRiskControl(c *gin.Context, body []byte, endpoint, model string) riskcontrol.Decision {
	if h == nil || h.riskControl == nil || c == nil {
		return riskcontrol.Decision{}
	}
	r := riskcontrol.Request{APIKeyID: c.GetInt64(contextAPIKeyID), APIKeyName: c.GetString(contextAPIKeyName), Endpoint: endpoint, Model: model}
	if h.riskControl.Enabled() {
		r.Input = h.riskControl.Extract(body, endpoint)
	}
	if v, ok := c.Get(contextAPIKeyRow); ok {
		if row, ok := v.(*database.APIKeyRow); ok {
			r.GroupIDs = row.AllowedGroupIDs
		}
	}
	d := h.riskControl.Check(c.Request.Context(), r)
	// Only a positive blocking verdict is eligible. Review service failures and
	// observation mode keep their configured semantics, not a forced handoff.
	if d.Blocked && d.Flagged && h.riskControl.FallbackOnBlockEnabled() &&
		(d.Action == "keyword_block" || d.Action == "hash_block" || d.Action == "block") &&
		h.fallbackPool != nil && h.fallbackPool.Policy().Enabled && !c.GetBool(contextRiskPrimarySelected) {
		switch endpoint {
		case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages":
			// A provider-owned response ID must never be replayed on another provider.
			if gjson.GetBytes(body, "previous_response_id").String() == "" {
				c.Set(contextRiskControlFallback, d)
				d.Blocked = false // The stored audit verdict remains unchanged.
			}
		}
	}
	return d
}
func (h *Handler) inspectRiskControl(c *gin.Context, body []byte, endpoint, model string) bool {
	d := h.checkRiskControlHTTP(c, body, endpoint, model)
	if !d.Blocked {
		return false
	}
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode("content_policy_violation"), d.Message, api.ErrorTypeInvalidRequest), d.Status)
	c.Abort()
	return true
}
func (h *Handler) inspectRiskControlText(c *gin.Context, text, endpoint, model string) bool {
	body, _ := json.Marshal(map[string]string{"prompt": text})
	return h.inspectRiskControl(c, body, endpoint, model)
}

const contextRiskControlFallback = "riskControlFallback"
const contextRiskPrimarySelected = "riskControlPrimarySelected"

// Called after all account/model/scope filters have been applied, before any
// primary selection. A review handoff is terminal and cannot return to primary.
func requireRiskControlFallback(c *gin.Context, state *fallbackRouteState, movable bool) *riskcontrol.Decision {
	value, _ := c.Get(contextRiskControlFallback)
	d, required := value.(riskcontrol.Decision)
	if !required {
		return nil
	}
	if !movable || !state.configured() {
		return &d
	}
	state.active, state.required = true, true
	state.reason = fallbackReasonContentReview
	return nil
}
func requireRiskControlFallbackHTTP(c *gin.Context, state *fallbackRouteState, movable bool) bool {
	if d := requireRiskControlFallback(c, state, movable); d != nil {
		if c.Request.URL.Path == "/v1/messages" {
			sendAnthropicError(c, d.Status, "permission_error", d.Message)
		} else {
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode("content_policy_violation"), d.Message, api.ErrorTypeInvalidRequest), d.Status)
		}
		c.Abort()
		return false
	}
	return true
}
