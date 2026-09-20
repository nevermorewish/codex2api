package admin

import (
	"encoding/hex"
	"encoding/json"
	"github.com/codex2api/security/riskcontrol"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) SetRiskControl(s *riskcontrol.Service) { h.riskControl = s }
func (h *Handler) registerRiskControlRoutes(api *gin.RouterGroup) {
	r := api.Group("/risk-control")
	r.Use(func(c *gin.Context) {
		if h.riskControl == nil {
			c.AbortWithStatusJSON(503, gin.H{"error": "风控服务尚未初始化"})
			return
		}
		c.Next()
	})
	r.GET("/config", h.getRiskConfig)
	r.PUT("/config", h.updateRiskConfig)
	r.GET("/status", h.riskStatus)
	r.GET("/logs", h.riskLogs)
	r.POST("/test", h.riskTest)
	r.POST("/model-audit/test", h.riskTestModelAudit)
	r.POST("/cleanup", h.riskCleanup)
	r.DELETE("/hashes/:hash", h.riskDeleteHash)
	r.DELETE("/hashes", h.riskDeleteHash)
}

func (h *Handler) riskConfigView(c *gin.Context) {
	cfg := h.riskControl.PublicConfig()
	c.JSON(200, gin.H{"config": cfg, "audit_categories": riskcontrol.AuditCategories, "audit_default_prompt": riskcontrol.DefaultAuditPrompt, "audit_category_prompt": riskcontrol.CategorizedAuditPrompt})
}

func (h *Handler) riskTestModelAudit(c *gin.Context) {
	var in struct {
		Policy riskcontrol.AuditConfig `json:"policy"`
		Text   string                  `json:"text"`
		NodeID string                  `json:"node_id"`
	}
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || strings.TrimSpace(in.Text) == "" || len([]rune(in.Text)) > 400000 {
		c.JSON(400, gin.H{"error": "无效试审请求或文本超过 400000 字符"})
		return
	}
	d, err := h.riskControl.TestAudit(c.Request.Context(), in.Policy, in.Text, in.NodeID)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, d)
}
func (h *Handler) getRiskConfig(c *gin.Context) { h.riskConfigView(c) }
func (h *Handler) updateRiskConfig(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		c.JSON(400, gin.H{"error": "读取配置失败"})
		return
	}
	if err = h.riskControl.Patch(c.Request.Context(), raw); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	h.riskConfigView(c)
}
func (h *Handler) riskStatus(c *gin.Context) {
	v, err := h.riskControl.Status(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "读取风控状态失败"})
		return
	}
	c.JSON(200, v)
}
func (h *Handler) riskLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	size, _ := strconv.Atoi(c.Query("page_size"))
	id, _ := strconv.ParseInt(c.Query("api_key_id"), 10, 64)
	v, err := h.riskControl.Store().RiskLogs(c.Request.Context(), riskcontrol.LogFilter{Page: page, PageSize: size, Action: c.Query("action"), APIKeyID: id, Query: c.Query("q")})
	if err != nil {
		c.JSON(500, gin.H{"error": "读取审核记录失败"})
		return
	}
	c.JSON(200, v)
}
func (h *Handler) riskDeleteHash(c *gin.Context) {
	hash := c.Param("hash")
	if hash != "" {
		if _, err := hex.DecodeString(hash); err != nil || len(hash) != 64 {
			c.JSON(400, gin.H{"error": "Hash 必须为 64 位十六进制"})
			return
		}
	} else if c.Query("confirm") != "true" {
		c.JSON(400, gin.H{"error": "清空 Hash 需要确认"})
		return
	}
	if err := h.riskControl.Store().DeleteRiskHash(c.Request.Context(), strings.ToLower(hash)); err != nil {
		c.JSON(500, gin.H{"error": "删除 Hash 失败"})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (h *Handler) riskCleanup(c *gin.Context) {
	n, err := h.riskControl.Cleanup(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "清理失败"})
		return
	}
	c.JSON(200, gin.H{"deleted": n})
}
func (h *Handler) riskTest(c *gin.Context) {
	var in struct {
		Text string `json:"text"`
		Kind string `json:"kind"`
	}
	if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Text) == "" || len(in.Text) > 100000 {
		c.JSON(400, gin.H{"error": "请输入测试文本（最多 100 KB）"})
		return
	}
	if in.Kind == "keyword" {
		c.JSON(200, gin.H{"matched_keyword": h.riskControl.TestKeyword(in.Text)})
		return
	}
	if in.Kind != "api" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "测试类型应为 keyword 或 api"})
		return
	}
	d, _ := h.riskControl.Test(c.Request.Context(), riskcontrol.Input{Text: in.Text})
	c.JSON(200, d)
}
