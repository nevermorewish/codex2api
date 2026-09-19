// Package riskcontrol implements content moderation independently of promptfilter.
package riskcontrol

import (
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"
)

var Categories = []string{"harassment", "harassment/threatening", "hate", "hate/threatening", "illicit", "illicit/violent", "self-harm", "self-harm/intent", "self-harm/instructions", "sexual", "sexual/minors", "violence", "violence/graphic"}

type Config struct {
	Enabled       bool               `json:"enabled"`
	Mode          string             `json:"mode"`
	Strategy      string             `json:"keyword_blocking_mode"`
	Keywords      []string           `json:"blocked_keywords"`
	BaseURL       string             `json:"base_url"`
	Model         string             `json:"model"`
	APIKeys       []string           `json:"api_keys,omitempty"`
	TimeoutMS     int                `json:"timeout_ms"`
	RetryCount    int                `json:"retry_count"`
	ProxyURL      string             `json:"proxy_url"`
	SampleRate    int                `json:"sample_rate"`
	ModelFilter   string             `json:"model_filter"`
	Models        []string           `json:"models"`
	GroupIDs      []int64            `json:"group_ids"`
	APIKeyIDs     []int64            `json:"api_key_ids"`
	Thresholds    map[string]float64 `json:"thresholds"`
	PreHash       bool               `json:"pre_hash_check_enabled"`
	RecordNonHits bool               `json:"record_non_hits"`
	BlockStatus   int                `json:"block_status"`
	BlockMessage  string             `json:"block_message"`
	Workers       int                `json:"worker_count"`
	QueueSize     int                `json:"queue_size"`
	AutoBan       bool               `json:"auto_ban_enabled"`
	BanThreshold  int                `json:"ban_threshold"`
	WindowHours   int                `json:"violation_window_hours"`
	HitDays       int                `json:"hit_retention_days"`
	NonHitDays    int                `json:"non_hit_retention_days"`
	EmailOnHit    bool               `json:"email_on_hit"`
	SMTPHost      string             `json:"smtp_host"`
	SMTPPort      int                `json:"smtp_port"`
	SMTPUsername  string             `json:"smtp_username"`
	SMTPPassword  string             `json:"smtp_password,omitempty"`
	EmailFrom     string             `json:"email_from"`
	EmailTo       string             `json:"email_to"`
}

func DefaultConfig() Config {
	c := Config{Mode: "pre_block", Strategy: "keyword_and_api", BaseURL: "https://api.openai.com", Model: "omni-moderation-latest", TimeoutMS: 3000, RetryCount: 2, SampleRate: 100, ModelFilter: "all", BlockStatus: 403, BlockMessage: "内容审计命中风险规则，请调整输入后重试", Workers: 4, QueueSize: 32768, BanThreshold: 10, WindowHours: 720, HitDays: 180, NonHitDays: 3, SMTPPort: 587, Keywords: []string{}, Models: []string{}, GroupIDs: []int64{}, APIKeyIDs: []int64{}, Thresholds: map[string]float64{}}
	values := []float64{0.98, 0.90, 0.65, 0.65, 0.95, 0.95, 0.65, 0.85, 0.65, 0.65, 0.65, 0.95, 0.95}
	for i, k := range Categories {
		c.Thresholds[k] = values[i]
	}
	return c
}

func (c *Config) Validate() error {
	if c.Mode != "off" && c.Mode != "observe" && c.Mode != "pre_block" {
		return fmt.Errorf("invalid mode")
	}
	if c.Strategy != "keyword_only" && c.Strategy != "keyword_and_api" && c.Strategy != "api_only" {
		return fmt.Errorf("invalid keyword strategy")
	}
	if c.ModelFilter != "all" && c.ModelFilter != "include" && c.ModelFilter != "exclude" {
		return fmt.Errorf("invalid model filter")
	}
	if c.ModelFilter != "all" && len(c.Models) == 0 {
		return fmt.Errorf("model list is required")
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
	c.Models = unique(c.Models, true)
	c.APIKeys = unique(c.APIKeys, false)
	if c.GroupIDs == nil {
		c.GroupIDs = []int64{}
	}
	if c.APIKeyIDs == nil {
		c.APIKeyIDs = []int64{}
	}
	if len(c.Models) > 1000 || len(c.APIKeys) > 100 || len(c.APIKeyIDs) > 1000 || len(c.GroupIDs) > 1000 {
		return fmt.Errorf("scope or key list too large")
	}
	for _, ids := range [][]int64{c.APIKeyIDs, c.GroupIDs} {
		for _, id := range ids {
			if id <= 0 {
				return fmt.Errorf("scope IDs must be positive")
			}
		}
	}
	if c.SampleRate < 0 || c.SampleRate > 100 || c.TimeoutMS < 100 || c.TimeoutMS > 30000 || c.RetryCount < 0 || c.RetryCount > 5 {
		return fmt.Errorf("invalid sampling, timeout or retry count")
	}
	if c.Workers < 1 || c.Workers > 32 || c.QueueSize < 1 || c.QueueSize > 100000 {
		return fmt.Errorf("workers must be 1..32; queue size 1..100000")
	}
	if c.BlockStatus < 400 || c.BlockStatus > 499 || strings.TrimSpace(c.BlockMessage) == "" || len(c.BlockMessage) > 2000 {
		return fmt.Errorf("invalid block status or message")
	}
	if c.BanThreshold < 1 || c.BanThreshold > 100000 || c.WindowHours < 1 || c.WindowHours > 87600 || c.HitDays < 1 || c.HitDays > 3650 || c.NonHitDays < 1 || c.NonHitDays > 3 {
		return fmt.Errorf("invalid ban or retention settings")
	}
	if c.Thresholds == nil {
		c.Thresholds = DefaultConfig().Thresholds
	}
	for _, k := range Categories {
		v, ok := c.Thresholds[k]
		if !ok || v < 0 || v > 1 {
			return fmt.Errorf("invalid threshold: %s", k)
		}
	}
	for k := range c.Thresholds {
		found := false
		for _, known := range Categories {
			if known == k {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unknown category: %s", k)
		}
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if err := validateURL(c.BaseURL); err != nil {
		return err
	}
	if c.ProxyURL != "" {
		if err := validateURL(c.ProxyURL); err != nil {
			return err
		}
	}
	if c.Model == "" || len(c.Model) > 200 {
		return fmt.Errorf("invalid moderation model")
	}
	if c.EmailOnHit {
		if c.SMTPHost == "" || c.SMTPPort < 1 || c.SMTPPort > 65535 {
			return fmt.Errorf("SMTP host and port required")
		}
		for _, a := range []string{c.EmailFrom, c.EmailTo} {
			if strings.ContainsAny(a, "\r\n") {
				return fmt.Errorf("invalid email address")
			}
			if _, err := mail.ParseAddress(a); err != nil {
				return fmt.Errorf("invalid email address")
			}
		}
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
