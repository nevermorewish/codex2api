package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

// FeishuAlertConfig controls the optional Feishu alert channel. The config is
// persisted as JSON in system_settings so older databases can be upgraded
// without adding several independent columns.
type FeishuAlertConfig struct {
	Enabled                  bool   `json:"enabled"`
	AppID                    string `json:"app_id"`
	AppSecret                string `json:"app_secret"`
	ChatIDs                  string `json:"chat_ids"`
	ErrorCodes               string `json:"error_codes"`
	FirstTokenTimeoutSeconds int    `json:"first_token_timeout_seconds"`
}

const defaultFeishuFirstTokenTimeoutSeconds = 30

func NormalizeFeishuAlertConfig(cfg FeishuAlertConfig) FeishuAlertConfig {
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	cfg.AppSecret = strings.TrimSpace(cfg.AppSecret)
	cfg.ChatIDs = strings.TrimSpace(cfg.ChatIDs)
	cfg.ErrorCodes = normalizeFeishuErrorCodes(cfg.ErrorCodes)
	if cfg.FirstTokenTimeoutSeconds <= 0 {
		cfg.FirstTokenTimeoutSeconds = defaultFeishuFirstTokenTimeoutSeconds
	}
	if cfg.FirstTokenTimeoutSeconds > 86400 {
		cfg.FirstTokenTimeoutSeconds = 86400
	}
	return cfg
}

func ParseFeishuAlertConfig(raw string) FeishuAlertConfig {
	cfg := FeishuAlertConfig{FirstTokenTimeoutSeconds: defaultFeishuFirstTokenTimeoutSeconds}
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	return NormalizeFeishuAlertConfig(cfg)
}

func EncodeFeishuAlertConfig(cfg FeishuAlertConfig) string {
	cfg = NormalizeFeishuAlertConfig(cfg)
	b, err := json.Marshal(cfg)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func normalizeFeishuErrorCodes(raw string) string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', '，', '；':
			return true
		default:
			return false
		}
	})
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, ",")
}

func feishuChatIDs(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ', '，', '；':
			return true
		default:
			return false
		}
	})
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

// FeishuErrorCodeMatches reports whether a usage log matches one of the
// configured error codes. Both HTTP status codes (503/http_503) and internal
// upstream kinds (for example service_unavailable) are accepted.
func FeishuErrorCodeMatches(configured string, input *database.UsageLogInput) bool {
	if input == nil {
		return false
	}
	codes := strings.Split(normalizeFeishuErrorCodes(configured), ",")
	if len(codes) == 0 || (len(codes) == 1 && codes[0] == "") {
		return false
	}
	candidates := []string{}
	if input.StatusCode > 0 {
		status := strconv.Itoa(input.StatusCode)
		candidates = append(candidates, status, "http_"+status)
	}
	if kind := strings.ToLower(strings.TrimSpace(input.UpstreamErrorKind)); kind != "" {
		candidates = append(candidates, kind, strings.ReplaceAll(kind, "-", "_"))
	}
	message := strings.ToLower(strings.TrimSpace(input.ErrorMessage))
	for _, code := range codes {
		code = strings.ToLower(strings.TrimSpace(code))
		if code == "" {
			continue
		}
		if input.StatusCode > 0 && feishuStatusCodeTokenMatches(code, input.StatusCode) {
			return true
		}
		for _, candidate := range candidates {
			if code == candidate {
				return true
			}
		}
		if message != "" && strings.Contains(message, code) {
			return true
		}
		// Make human-readable forms such as "service unavailable" match the
		// conventional service_unavailable error code.
		if message != "" && strings.Contains(strings.ReplaceAll(message, " ", "_"), code) {
			return true
		}
	}
	return false
}

// feishuStatusCodeTokenMatches accepts exact HTTP codes plus the convenient
// range forms used by the Huanxing monitor (for example 500-599, 5xx, and
// http_5xx). Invalid tokens simply do not match and can coexist with valid
// error kinds in the same configuration string.
func feishuStatusCodeTokenMatches(token string, status int) bool {
	token = strings.TrimSpace(strings.TrimPrefix(token, "http_"))
	if status < 100 || status > 599 {
		return false
	}
	if len(token) == 3 && token[1:] == "xx" && token[0] >= '1' && token[0] <= '5' {
		return status/100 == int(token[0]-'0')
	}
	if strings.Contains(token, "-") {
		parts := strings.SplitN(token, "-", 2)
		if len(parts) != 2 {
			return false
		}
		lo, errLo := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, errHi := strconv.Atoi(strings.TrimSpace(parts[1]))
		return errLo == nil && errHi == nil && lo <= hi && status >= lo && status <= hi
	}
	value, err := strconv.Atoi(token)
	return err == nil && value == status
}

type feishuTokenCache struct {
	sync.Mutex
	token     string
	expiresAt time.Time
	appID     string
}

var feishuTokens feishuTokenCache

var feishuHTTPClient = &http.Client{Timeout: 15 * time.Second}

// feishuFirstTokenWatch mirrors the monitor used by Huanxing: the timer runs
// while the upstream request is still in flight and is disarmed as soon as a
// real response-progress event arrives. It is deliberately independent from
// the request's upstream timeout guard, so enabling alerts never changes
// routing, retry, or cancellation behavior.
type feishuFirstTokenWatch struct {
	timer      *time.Timer
	stopOnce   sync.Once
	done       chan struct{}
	progressed atomic.Bool
	fired      atomic.Bool
	config     FeishuAlertConfig
	input      database.UsageLogInput
	startedAt  time.Time
}

func newFeishuFirstTokenWatch(ctx context.Context, input database.UsageLogInput, timeout time.Duration) *feishuFirstTokenWatch {
	if !input.Stream && !input.ViaWebsocket {
		return nil
	}
	cfg := ParseFeishuAlertConfig(CurrentRuntimeSettings().FeishuConfig)
	if !cfg.Enabled || cfg.AppID == "" || cfg.AppSecret == "" || len(feishuChatIDs(cfg.ChatIDs)) == 0 || timeout <= 0 {
		return nil
	}
	// timeout can be shorter than the configured threshold when account
	// selection/connection setup already consumed part of the request. Preserve
	// that elapsed portion so the alert reports the real first-token wait.
	startedAt := time.Now().Add(-(time.Duration(cfg.FirstTokenTimeoutSeconds)*time.Second - timeout))
	watch := &feishuFirstTokenWatch{config: cfg, input: input, done: make(chan struct{}), startedAt: startedAt}
	watch.timer = time.AfterFunc(timeout, func() {
		if watch.progressed.Load() || !watch.fired.CompareAndSwap(false, true) {
			return
		}
		input := watch.input
		input.FirstTokenMs = int(time.Since(watch.startedAt).Milliseconds())
		input.DurationMs = input.FirstTokenMs
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := sendFeishuUsageAlert(ctx, watch.config, &input, false, true); err != nil {
				log.Printf("飞书首 token 告警发送失败: %v", err)
			}
		}()
	})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				watch.Stop()
			case <-watch.done:
			}
		}()
	}
	return watch
}

func (w *feishuFirstTokenWatch) MarkProgress() {
	if w == nil || w.progressed.Swap(true) {
		return
	}
	w.Stop()
}

func (w *feishuFirstTokenWatch) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() {
		close(w.done)
		if w.timer != nil {
			w.timer.Stop()
		}
	})
}

func feishuFirstTokenTimeoutForAttempt(start time.Time) time.Duration {
	cfg := ParseFeishuAlertConfig(CurrentRuntimeSettings().FeishuConfig)
	if !cfg.Enabled || cfg.FirstTokenTimeoutSeconds <= 0 {
		return 0
	}
	remaining := time.Duration(cfg.FirstTokenTimeoutSeconds) * time.Second
	if elapsed := time.Since(start); elapsed > 0 {
		remaining -= elapsed
	}
	if remaining <= 0 {
		return time.Millisecond
	}
	return remaining
}

// NotifyFeishuForUsage evaluates a completed request and asynchronously sends
// a card when an enabled error-code or first-token rule matches. It is safe to
// call from the request logging path; all network work is detached.
func NotifyFeishuForUsage(input *database.UsageLogInput) {
	if input == nil || input.IsRetryAttempt {
		return
	}
	cfg := ParseFeishuAlertConfig(CurrentRuntimeSettings().FeishuConfig)
	if !cfg.Enabled || cfg.AppID == "" || cfg.AppSecret == "" || len(feishuChatIDs(cfg.ChatIDs)) == 0 {
		return
	}
	errorAlert := FeishuErrorCodeMatches(cfg.ErrorCodes, input)
	firstTokenMs := input.FirstTokenMs
	// Streaming requests are monitored by newFeishuFirstTokenWatch while they
	// are in flight. Do not evaluate their measured value again here: when the
	// first token arrives after the threshold, the timer has already emitted an
	// alert and a completion-time check would send a duplicate. Keep the
	// completion check for non-stream records that carry an explicit measured
	// first-token value (for example a future buffered protocol).
	firstTokenAlert := !input.Stream && !input.ViaWebsocket &&
		cfg.FirstTokenTimeoutSeconds > 0 && firstTokenMs >= cfg.FirstTokenTimeoutSeconds*1000
	if !errorAlert && !firstTokenAlert {
		return
	}
	// Copy the small value before handing it to a goroutine. Callers commonly
	// reuse the request log object after this function returns.
	copyInput := *input
	copyInput.FirstTokenMs = firstTokenMs
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := sendFeishuUsageAlert(ctx, cfg, &copyInput, errorAlert, firstTokenAlert); err != nil {
			log.Printf("飞书告警发送失败: %v", err)
		}
	}()
}

func feishuTenantAccessToken(ctx context.Context, cfg FeishuAlertConfig) (string, error) {
	feishuTokens.Lock()
	if feishuTokens.appID == cfg.AppID && feishuTokens.token != "" && time.Until(feishuTokens.expiresAt) > time.Minute {
		token := feishuTokens.token
		feishuTokens.Unlock()
		return token, nil
	}
	feishuTokens.Unlock()

	body, _ := json.Marshal(map[string]string{"app_id": cfg.AppID, "app_secret": cfg.AppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := feishuHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("解析租户 token 响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 || strings.TrimSpace(result.TenantAccessToken) == "" {
		return "", fmt.Errorf("获取租户 token 失败: HTTP %d code=%d msg=%s", resp.StatusCode, result.Code, strings.TrimSpace(result.Msg))
	}
	expires := result.Expire
	if expires <= 0 {
		expires = 3600
	}
	feishuTokens.Lock()
	feishuTokens.appID = cfg.AppID
	feishuTokens.token = result.TenantAccessToken
	feishuTokens.expiresAt = time.Now().Add(time.Duration(expires) * time.Second)
	feishuTokens.Unlock()
	return result.TenantAccessToken, nil
}

func sendFeishuUsageAlert(ctx context.Context, cfg FeishuAlertConfig, input *database.UsageLogInput, errorAlert, firstTokenAlert bool) error {
	token, err := feishuTenantAccessToken(ctx, cfg)
	if err != nil {
		return err
	}
	title := "Codex2API 请求告警"
	if errorAlert && firstTokenAlert {
		title = "Codex2API 错误及首 token 超时"
	} else if errorAlert {
		title = "Codex2API 错误告警"
	} else {
		title = "Codex2API 首 token 超时"
	}
	lines := []string{
		fmt.Sprintf("**状态码**: %d", input.StatusCode),
		fmt.Sprintf("**模型**: %s", strings.TrimSpace(input.Model)),
		fmt.Sprintf("**接口**: %s", strings.TrimSpace(input.Endpoint)),
		fmt.Sprintf("**首 token**: %d ms", input.FirstTokenMs),
		fmt.Sprintf("**耗时**: %d ms", input.DurationMs),
	}
	if input.UpstreamErrorKind != "" {
		lines = append(lines, fmt.Sprintf("**错误类型**: %s", input.UpstreamErrorKind))
	}
	if input.ErrorMessage != "" {
		message := strings.TrimSpace(input.ErrorMessage)
		if len([]rune(message)) > 800 {
			message = string([]rune(message)[:800]) + "…"
		}
		lines = append(lines, fmt.Sprintf("**错误信息**: %s", message))
	}
	content, _ := json.Marshal(map[string]interface{}{
		"config":   map[string]bool{"wide_screen_mode": true},
		"header":   map[string]interface{}{"template": "red", "title": map[string]string{"tag": "plain_text", "content": title}},
		"elements": []map[string]interface{}{{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": strings.Join(lines, "\n")}}},
	})
	for _, chatID := range feishuChatIDs(cfg.ChatIDs) {
		payload, _ := json.Marshal(map[string]string{
			"receive_id": chatID,
			"msg_type":   "interactive",
			"content":    string(content),
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=chat_id", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := feishuHTTPClient.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("发送消息到 %s 失败: HTTP %d: %s", chatID, resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var result struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("解析飞书消息响应失败: %w", err)
		}
		if result.Code != 0 {
			return fmt.Errorf("发送消息到 %s 失败: code=%d msg=%s", chatID, result.Code, strings.TrimSpace(result.Msg))
		}
	}
	return nil
}
