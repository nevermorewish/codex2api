// Package riskcontrol implements content moderation independently of promptfilter.
package riskcontrol

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

type Config struct {
	Engine          string             `json:"audit_engine"`
	Audit           AuditConfig        `json:"model_audit"`
	Enabled         bool               `json:"enabled"`
	Mode            string             `json:"mode"`
	Strategy        string             `json:"keyword_blocking_mode"`
	Keywords        []string           `json:"blocked_keywords"`
	BaseURL         string             `json:"base_url,omitempty"`
	Model           string             `json:"model,omitempty"`
	APIKeys         []string           `json:"api_keys,omitempty"`
	TimeoutMS       int                `json:"timeout_ms,omitempty"`
	RetryCount      int                `json:"retry_count,omitempty"`
	ProxyURL        string             `json:"proxy_url,omitempty"`
	SampleRate      int                `json:"sample_rate"`
	ModelFilter     string             `json:"model_filter"`
	Models          []string           `json:"models,omitempty"`
	GroupIDs        []int64            `json:"group_ids,omitempty"`
	APIKeyIDs       []int64            `json:"api_key_ids,omitempty"`
	Thresholds      map[string]float64 `json:"thresholds,omitempty"`
	FallbackOnBlock bool               `json:"fallback_on_block_enabled"`
	PreHash         bool               `json:"pre_hash_check_enabled"`
	RecordNonHits   bool               `json:"record_non_hits"`
	BlockStatus     int                `json:"block_status"`
	BlockMessage    string             `json:"block_message"`
	Workers         int                `json:"worker_count"`
	QueueSize       int                `json:"queue_size"`
	// Deprecated: accepted from older clients, ignored and omitted after validation.
	AutoBan      bool   `json:"auto_ban_enabled,omitempty"`
	BanThreshold int    `json:"ban_threshold,omitempty"`
	WindowHours  int    `json:"violation_window_hours,omitempty"`
	HitDays      int    `json:"hit_retention_days"`
	NonHitDays   int    `json:"non_hit_retention_days"`
	EmailOnHit   bool   `json:"email_on_hit,omitempty"`
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty"`
	SMTPUsername string `json:"smtp_username,omitempty"`
	SMTPPassword string `json:"smtp_password,omitempty"`
	EmailFrom    string `json:"email_from,omitempty"`
	EmailTo      string `json:"email_to,omitempty"`
}

func DefaultConfig() Config {
	c := Config{FallbackOnBlock: true, Mode: "pre_block", Strategy: "keyword_and_api", SampleRate: 100, ModelFilter: "all", BlockStatus: 403, BlockMessage: "内容审计命中风险规则，请调整输入后重试", Workers: 4, QueueSize: 32768, HitDays: 180, NonHitDays: 3, Keywords: []string{}, Models: []string{}, GroupIDs: []int64{}, APIKeyIDs: []int64{}, Thresholds: nil}
	c.Engine, c.Audit = "chat", DefaultAuditConfig()
	return c
}

func (c *Config) Validate() error {
	c.NormalizeRetiredSettings()
	if err := c.Audit.Validate(); err != nil {
		return err
	}
	if c.Mode != "off" && c.Mode != "observe" && c.Mode != "pre_block" {
		return fmt.Errorf("invalid mode")
	}
	if c.Strategy != "keyword_only" && c.Strategy != "keyword_and_api" && c.Strategy != "api_only" {
		return fmt.Errorf("invalid keyword strategy")
	}
	if len(c.Keywords) > 10000 {
		return fmt.Errorf("at most 10000 keywords")
	}
	for _, k := range c.Keywords {
		if utf8.RuneCountInString(k) > 200 {
			return fmt.Errorf("keyword exceeds 200 characters")
		}
	}
	c.Keywords = unique(c.Keywords, true)
	if c.SampleRate < 0 || c.SampleRate > 100 {
		return fmt.Errorf("invalid sampling rate")
	}
	if c.Workers < 1 || c.Workers > 32 || c.QueueSize < 1 || c.QueueSize > 100000 {
		return fmt.Errorf("workers must be 1..32; queue size 1..100000")
	}
	if c.BlockStatus < 400 || c.BlockStatus > 499 || strings.TrimSpace(c.BlockMessage) == "" || len(c.BlockMessage) > 2000 {
		return fmt.Errorf("invalid block status or message")
	}
	if c.HitDays < 1 || c.HitDays > 3650 || c.NonHitDays < 1 || c.NonHitDays > 3 {
		return fmt.Errorf("invalid retention settings")
	}

	return nil
}
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("expected HTTP(S) URL without credentials, query or fragment")
	}
	return nil
}
func unique(items []string, fold bool) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		key := item
		if fold {
			key = strings.ToLower(key)
		}
		if item != "" && !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}

// NormalizeRetiredSettings accepts old stored configs and old admin clients,
// but never keeps hidden scope restrictions, caller bans or mail delivery active.
func (c *Config) NormalizeRetiredSettings() {
	c.Engine = "chat"
	c.BaseURL, c.Model, c.ProxyURL = "", "", ""
	c.APIKeys, c.Thresholds = nil, nil
	c.TimeoutMS, c.RetryCount = 0, 0
	c.AutoBan, c.BanThreshold, c.WindowHours = false, 0, 0
	c.ModelFilter, c.Models, c.GroupIDs, c.APIKeyIDs = "all", []string{}, []int64{}, []int64{}
	c.EmailOnHit, c.SMTPPort = false, 0
	c.SMTPHost, c.SMTPUsername, c.SMTPPassword, c.EmailFrom, c.EmailTo = "", "", "", "", ""
}
