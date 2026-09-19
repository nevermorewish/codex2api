package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
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
		r.Input = riskcontrol.Extract(body, endpoint)
	}
	if v, ok := c.Get(contextAPIKeyRow); ok {
		if row, ok := v.(*database.APIKeyRow); ok {
			r.GroupIDs = row.AllowedGroupIDs
		}
	}
	return h.riskControl.Check(c.Request.Context(), r)
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
