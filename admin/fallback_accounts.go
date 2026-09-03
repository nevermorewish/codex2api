package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type fallbackAccountResponse struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Protocol    string    `json:"protocol"`
	BaseURL     string    `json:"base_url"`
	Model       string    `json:"model"`
	ProxyURL    string    `json:"proxy_url"`
	Concurrency int       `json:"concurrency"`
	Enabled     bool      `json:"enabled"`
	HasAPIKey   bool      `json:"has_api_key"`
	APIKeyMask  string    `json:"api_key_masked"`
	Active      int64     `json:"active"`
	Occupied    int64     `json:"occupied"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type createFallbackAccountRequest struct {
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
	ProxyURL    string `json:"proxy_url"`
	Concurrency int    `json:"concurrency"`
	Enabled     *bool  `json:"enabled"`
}

type updateFallbackAccountRequest struct {
	Name        *string `json:"name"`
	Protocol    *string `json:"protocol"`
	BaseURL     *string `json:"base_url"`
	APIKey      *string `json:"api_key"`
	Model       *string `json:"model"`
	ProxyURL    *string `json:"proxy_url"`
	Concurrency *int    `json:"concurrency"`
	Enabled     *bool   `json:"enabled"`
}

func fallbackConfigs(rows []*database.FallbackAccountRow) []auth.FallbackAccountConfig {
	configs := make([]auth.FallbackAccountConfig, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		configs = append(configs, auth.FallbackAccountConfig{
			ID: row.ID, Name: row.Name, BaseURL: row.BaseURL, APIKey: row.APIKey,
			Model: row.Model, ProxyURL: row.ProxyURL, Concurrency: row.Concurrency, Enabled: row.Enabled,
		})
	}
	return configs
}

func (h *Handler) ReloadFallbackPool(ctx context.Context) error {
	if h == nil || h.db == nil || h.fallbackPool == nil {
		return nil
	}
	rows, err := h.db.ListFallbackAccounts(ctx)
	if err != nil {
		return err
	}
	policy, err := h.db.GetFallbackPolicy(ctx)
	if err != nil {
		return err
	}
	h.fallbackPool.Replace(fallbackConfigs(rows))
	h.fallbackPool.SetPolicy(auth.FallbackPolicy{
		Enabled:                               policy.Enabled,
		RelayCount:                            policy.RelayCount,
		QueueDirectFallbackThreshold:          policy.QueueDirectFallbackThreshold,
		OversizedRequestDirectFallbackEnabled: policy.OversizedRequestDirectFallbackEnabled,
	})
	return nil
}

func validateFallbackProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if err := security.ValidateProxyURL(raw); err != nil || raw == "" {
		return err
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("proxy_url must be a complete proxy URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("proxy_url only supports http, https, socks5, and socks5h")
	}
}

func normalizeFallbackAccount(row *database.FallbackAccountRow, requireAPIKey bool) error {
	if row == nil {
		return errors.New("fallback account is required")
	}
	row.Name = strings.TrimSpace(row.Name)
	if row.Name == "" || utf8.RuneCountInString(row.Name) > 120 {
		return errors.New("name is required and must be at most 120 characters")
	}
	row.Protocol = strings.ToLower(strings.TrimSpace(row.Protocol))
	if row.Protocol == "" {
		row.Protocol = database.FallbackProtocolOpenAIResponses
	}
	if row.Protocol != database.FallbackProtocolOpenAIResponses {
		return errors.New("only the openai_responses protocol is currently supported")
	}
	baseURL, err := auth.NormalizeOpenAIResponsesBaseURL(row.BaseURL)
	if err != nil {
		return err
	}
	row.BaseURL = baseURL
	row.APIKey = strings.TrimSpace(row.APIKey)
	if requireAPIKey && row.APIKey == "" {
		return errors.New("api_key is required")
	}
	row.Model = strings.TrimSpace(row.Model)
	if row.Model == "" || len(row.Model) > 200 {
		return errors.New("model is required and must be at most 200 characters")
	}
	row.ProxyURL = strings.TrimSpace(row.ProxyURL)
	if err := validateFallbackProxyURL(row.ProxyURL); err != nil {
		return err
	}
	if row.Concurrency < 1 || row.Concurrency > 1000 {
		return errors.New("concurrency must be between 1 and 1000")
	}
	return nil
}

func (h *Handler) fallbackRuntimeByID(id int64) *auth.Account {
	if h == nil || h.fallbackPool == nil {
		return nil
	}
	for _, account := range h.fallbackPool.Accounts() {
		if account != nil && account.ID() == -id {
			return account
		}
	}
	return nil
}

func (h *Handler) fallbackResponse(row *database.FallbackAccountRow) fallbackAccountResponse {
	result := fallbackAccountResponse{
		ID: row.ID, Name: row.Name, Protocol: row.Protocol, BaseURL: row.BaseURL,
		Model: row.Model, ProxyURL: row.ProxyURL, Concurrency: row.Concurrency,
		Enabled: row.Enabled, HasAPIKey: strings.TrimSpace(row.APIKey) != "",
		APIKeyMask: security.MaskAPIKey(row.APIKey), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if account := h.fallbackRuntimeByID(row.ID); account != nil {
		snapshot := account.GetAccountListRuntimeSnapshot()
		result.Active = snapshot.ActiveRequests
		result.Occupied = snapshot.OccupiedRequests
		result.Status = snapshot.Status
	}
	return result
}

func (h *Handler) ListFallbackAccounts(c *gin.Context) {
	rows, err := h.db.ListFallbackAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list fallback accounts"})
		return
	}
	accounts := make([]fallbackAccountResponse, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, h.fallbackResponse(row))
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

func (h *Handler) CreateFallbackAccount(c *gin.Context) {
	var req createFallbackAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row := &database.FallbackAccountRow{
		Name: req.Name, Protocol: req.Protocol, BaseURL: req.BaseURL, APIKey: req.APIKey,
		Model: req.Model, ProxyURL: req.ProxyURL, Concurrency: req.Concurrency, Enabled: enabled,
	}
	if err := normalizeFallbackAccount(row, true); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := h.db.CreateFallbackAccount(c.Request.Context(), row)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create fallback account"})
		return
	}
	if err := h.ReloadFallbackPool(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account was saved but runtime pool refresh failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"account": h.fallbackResponse(created)})
}

func parseFallbackAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid fallback account id"})
		return 0, false
	}
	return id, true
}

func (h *Handler) UpdateFallbackAccount(c *gin.Context) {
	id, ok := parseFallbackAccountID(c)
	if !ok {
		return
	}
	row, err := h.db.GetFallbackAccount(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fallback account not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load fallback account"})
		return
	}
	var req updateFallbackAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Name != nil {
		row.Name = *req.Name
	}
	if req.Protocol != nil {
		row.Protocol = *req.Protocol
	}
	if req.BaseURL != nil {
		row.BaseURL = *req.BaseURL
	}
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		row.APIKey = *req.APIKey
	}
	if req.Model != nil {
		row.Model = *req.Model
	}
	if req.ProxyURL != nil {
		row.ProxyURL = *req.ProxyURL
	}
	if req.Concurrency != nil {
		row.Concurrency = *req.Concurrency
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if err := normalizeFallbackAccount(row, true); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := h.db.UpdateFallbackAccount(c.Request.Context(), row)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fallback account not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update fallback account"})
		return
	}
	if err := h.ReloadFallbackPool(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account was saved but runtime pool refresh failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"account": h.fallbackResponse(updated)})
}

func (h *Handler) DeleteFallbackAccount(c *gin.Context) {
	id, ok := parseFallbackAccountID(c)
	if !ok {
		return
	}
	err := h.db.DeleteFallbackAccount(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fallback account not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete fallback account"})
		return
	}
	if err := h.ReloadFallbackPool(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account was deleted but runtime pool refresh failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "fallback account deleted"})
}

func sanitizeFallbackTestText(value, apiKey string) string {
	value = security.MaskSensitiveData(value)
	if apiKey != "" {
		value = strings.ReplaceAll(value, apiKey, "****")
	}
	return strings.TrimSpace(value)
}

func (h *Handler) TestFallbackAccount(c *gin.Context) {
	id, ok := parseFallbackAccountID(c)
	if !ok {
		return
	}
	row, err := h.db.GetFallbackAccount(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "fallback account not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load fallback account"})
		return
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model": row.Model, "input": "Reply with OK.", "max_output_tokens": 32, "stream": false,
	})
	account := &auth.Account{
		DBID: -row.ID, Name: row.Name, ExternalFallback: true,
		UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: row.BaseURL, APIKey: row.APIKey,
		Models: []string{row.Model}, ProxyURL: row.ProxyURL,
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	resp, requestErr := proxy.ExecuteOpenAIResponsesRequest(ctx, account, body, row.ProxyURL, nil)
	latency := time.Since(started).Milliseconds()
	if requestErr != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "latency_ms": latency, "error": sanitizeFallbackTestText(requestErr.Error(), row.APIKey)})
		return
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	success := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	result := gin.H{"success": success, "status_code": resp.StatusCode, "latency_ms": latency}
	if !success {
		message := sanitizeFallbackTestText(string(responseBody), row.APIKey)
		if message == "" {
			message = fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
		}
		result["error"] = message
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetFallbackSettings(c *gin.Context) {
	policy, err := h.db.GetFallbackPolicy(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load fallback settings"})
		return
	}
	c.JSON(http.StatusOK, policy)
}

func (h *Handler) UpdateFallbackSettings(c *gin.Context) {
	var policy database.FallbackPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.db.UpdateFallbackPolicy(c.Request.Context(), policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.ReloadFallbackPool(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings were saved but runtime pool refresh failed"})
		return
	}
	c.JSON(http.StatusOK, policy)
}
