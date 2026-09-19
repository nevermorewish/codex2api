package riskcontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
func hasV1Suffix(s string) bool  { return strings.HasSuffix(s, "/v1") }
func newEventID() string         { return uuid.NewString() }

type Request struct {
	APIKeyID   int64   `json:"api_key_id"`
	APIKeyName string  `json:"api_key_name"`
	GroupIDs   []int64 `json:"group_ids,omitempty"`
	Endpoint   string  `json:"endpoint"`
	Model      string  `json:"model"`
	Input      Input   `json:"input"`
}
type Decision struct {
	Audit          *AuditResult       `json:"audit,omitempty"`
	Blocked        bool               `json:"blocked"`
	Flagged        bool               `json:"flagged"`
	Action         string             `json:"action"`
	MatchedKeyword string             `json:"matched_keyword,omitempty"`
	Scores         map[string]float64 `json:"scores,omitempty"`
	Status         int                `json:"status"`
	Message        string             `json:"message"`
	Error          string             `json:"error,omitempty"`
	LatencyMS      int64              `json:"latency_ms"`
}
type Event struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"created_at"`
	APIKeyID   int64  `json:"api_key_id"`
	APIKeyName string `json:"api_key_name"`
	Endpoint   string `json:"endpoint"`
	Model      string `json:"model"`
	Mode       string `json:"mode"`
	InputHash  string `json:"input_hash"`
	Excerpt    string `json:"input_excerpt"`
	Decision
	ViolationCount int  `json:"violation_count"`
	AutoBanned     bool `json:"auto_banned"`
	EmailSent      bool `json:"email_sent"`
}
type Ban struct {
	APIKeyID  int64  `json:"api_key_id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	ResetAt   int64  `json:"reset_at"`
	Blocked   bool   `json:"blocked"`
}
type LogFilter struct {
	Page     int
	PageSize int
	Action   string
	APIKeyID int64
	Query    string
}
type LogPage struct {
	Items    []Event `json:"items"`
	Total    int     `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
}
type Stats struct {
	Total   int64 `json:"total"`
	Hits    int64 `json:"hits"`
	Blocked int64 `json:"blocked"`
	Hashes  int64 `json:"hashes"`
	Bans    int64 `json:"bans"`
}
type Store interface {
	LoadRiskConfig(context.Context) (Config, error)
	SaveRiskConfig(context.Context, Config) error
	HasRiskHash(context.Context, string) (bool, error)
	DeleteRiskHash(context.Context, string) error
	RiskBan(context.Context, int64) (bool, error)
	UnbanRiskKey(context.Context, int64) error
	RiskBans(context.Context) ([]Ban, error)
	RecordRiskEvent(context.Context, *Event, Config) error
	RiskLogs(context.Context, LogFilter) (LogPage, error)
	RiskStats(context.Context) (Stats, error)
	CleanupRiskLogs(context.Context, Config) (int64, error)
	MarkRiskEmailSent(context.Context, string) error
}
type snapshot struct {
	config  Config
	matcher *matcher
}
type task struct {
	request Request
	config  Config
	notice  *Event
	bytes   int64
}
type KeyHealth struct {
	ID          string `json:"id"`
	Hint        string `json:"hint"`
	Status      string `json:"status"`
	Calls       int64  `json:"calls"`
	Errors      int64  `json:"errors"`
	LatencyMS   int64  `json:"latency_ms"`
	HTTPStatus  int    `json:"http_status"`
	FrozenUntil int64  `json:"frozen_until"`
}
type Runtime struct {
	QueueBytes int64 `json:"queue_bytes"`
	Stats
	QueueLength        int         `json:"queue_length"`
	Active             int64       `json:"active"`
	Checked            int64       `json:"checked"`
	Dropped            int64       `json:"dropped"`
	Errors             int64       `json:"errors"`
	NotificationErrors int64       `json:"notification_errors"`
	Keys               []KeyHealth `json:"keys"`
}
type Service struct {
	queueMu                                                          sync.Mutex
	queueBytes                                                       atomic.Int64
	store                                                            Store
	current                                                          atomic.Pointer[snapshot]
	mu                                                               sync.Mutex
	healthMu                                                         sync.Mutex
	health                                                           map[string]KeyHealth
	queue                                                            chan *task
	ctx                                                              context.Context
	cancel                                                           context.CancelFunc
	wg                                                               sync.WaitGroup
	active, checked, dropped, failures, notificationErrors, sequence atomic.Int64
	// Client is injectable in tests. Production transports never follow redirects.
	Client *http.Client
}

func New(parent context.Context, store Store) (*Service, error) {
	c, err := store.LoadRiskConfig(parent)
	if err != nil {
		return nil, err
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Service{store: store, queue: make(chan *task, 100000), ctx: ctx, cancel: cancel, health: map[string]KeyHealth{}}
	s.current.Store(&snapshot{config: c, matcher: newMatcher(c.Keywords)})
	for i := 0; i < 32; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTicker(time.Hour)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if _, err := s.Cleanup(ctx); err != nil {
					s.failures.Add(1)
				}
			}
		}
	}()
	return s, nil
}
func (s *Service) Close() { s.cancel(); s.wg.Wait() }
func (s *Service) Config() Config {
	b, _ := json.Marshal(s.current.Load().config)
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}

// Update atomically replaces a full configuration.
func (s *Service) Update(ctx context.Context, c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(ctx, c)
}

// Patch merges partial admin updates under the same lock as publication.
// Omitted secrets are retained; explicit empty values erase them.
func (s *Service) Patch(ctx context.Context, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("expected one config object")
	}
	c := s.Config()
	oldAudit := c.Audit
	// encoding/json reuses struct elements in slices. Reset a supplied node list
	// before decoding so omitted keys cannot migrate to another node by index.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		var auditFields map[string]json.RawMessage
		if json.Unmarshal(fields["model_audit"], &auditFields) == nil {
			if _, supplied := auditFields["nodes"]; supplied {
				c.Audit.Nodes = nil
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return fmt.Errorf("invalid config JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one config object")
	}
	mergeAuditSecrets(&c.Audit, oldAudit)
	return s.saveLocked(ctx, c)
}

func (s *Service) saveLocked(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := s.store.SaveRiskConfig(ctx, c); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	var copy Config
	_ = json.Unmarshal(b, &copy)
	s.current.Store(&snapshot{config: copy, matcher: newMatcher(copy.Keywords)})
	return nil
}
func (s *Service) Store() Store  { return s.store }
func (s *Service) Enabled() bool { c := s.current.Load().config; return c.Enabled && c.Mode != "off" }
func (s *Service) IsBanned(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	return s.store.RiskBan(ctx, id)
}
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	return s.store.CleanupRiskLogs(ctx, s.Config())
}
func (s *Service) Status(ctx context.Context) (Runtime, error) {
	st, err := s.store.RiskStats(ctx)
	if err != nil {
		return Runtime{}, err
	}
	r := Runtime{QueueBytes: s.queueBytes.Load(), Stats: st, QueueLength: len(s.queue), Active: s.active.Load(), Checked: s.checked.Load(), Dropped: s.dropped.Load(), Errors: s.failures.Load(), NotificationErrors: s.notificationErrors.Load(), Keys: []KeyHealth{}}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for _, k := range s.current.Load().config.APIKeys {
		r.Keys = append(r.Keys, s.keyHealth(k))
	}
	return r, nil
}
func keyID(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *Service) keyHealth(k string) KeyHealth {
	id := keyID(k)
	v, ok := s.health[id]
	if !ok {
		v = KeyHealth{ID: id, Hint: "••••", Status: "unknown"}
		if len(k) > 4 {
			v.Hint += k[len(k)-4:]
		}
	}
	return v
}
func (s *Service) updateHealth(k string, status int, ms int64, err error) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	v := s.keyHealth(k)
	v.Calls++
	v.LatencyMS = ms
	v.HTTPStatus = status
	v.Status = "ok"
	v.FrozenUntil = 0
	if err != nil {
		v.Errors++
		v.Status = "error"
		cooldown := 10 * time.Second
		if status == 401 || status == 403 {
			cooldown = 10 * time.Minute
		}
		if status == 429 {
			cooldown = time.Minute
		}
		v.FrozenUntil = time.Now().Add(cooldown).Unix()
		v.Status = "frozen"
	}
	s.health[v.ID] = v
}
func inIDs(scope, actual []int64) bool {
	if len(scope) == 0 {
		return true
	}
	for _, a := range actual {
		for _, b := range scope {
			if a == b {
				return true
			}
		}
	}
	return false
}
func inScope(c Config, r Request) bool {
	if !inIDs(c.APIKeyIDs, []int64{r.APIKeyID}) || !inIDs(c.GroupIDs, r.GroupIDs) {
		return false
	}
	found := false
	for _, m := range c.Models {
		if equalFold(m, r.Model) {
			found = true
			break
		}
	}
	return c.ModelFilter == "all" || c.ModelFilter == "include" && found || c.ModelFilter == "exclude" && !found
}
func (s *Service) Check(ctx context.Context, r Request) Decision {
	snap := s.current.Load()
	c := snap.config
	if blocked, err := s.IsBanned(ctx, r.APIKeyID); err != nil {
		s.failures.Add(1)
		return Decision{Blocked: true, Action: "error", Status: 503, Message: "风控状态暂时不可用"}
	} else if blocked {
		return Decision{Blocked: true, Action: "key_banned", Status: 403, Message: "该 API Key 已被风控封禁，请联系管理员"}
	}
	if !c.Enabled || c.Mode == "off" || !inScope(c, r) || (r.Input.Text == "" && len(r.Input.Images) == 0) {
		return Decision{Action: "allow"}
	}
	s.checked.Add(1)
	d, terminal := s.local(ctx, r, c, snap.matcher)
	if terminal {
		s.record(ctx, r, c, &d)
		return d
	}
	if c.Mode == "observe" {
		if s.enqueue(&task{request: r, config: c}) {
			return Decision{Action: "queued"}
		}
		return Decision{Action: "queue_full"}
	}

	d = s.evaluate(ctx, r.Input, c)
	s.record(ctx, r, c, &d)
	return d
}
func (s *Service) local(ctx context.Context, r Request, c Config, m *matcher) (Decision, bool) {
	if c.Mode == "pre_block" && c.Strategy != "api_only" {
		if kw := m.Match(r.Input.Text); kw != "" {
			return Decision{Blocked: true, Flagged: true, Action: "keyword_block", MatchedKeyword: kw, Status: c.BlockStatus, Message: c.BlockMessage}, true
		}
		if c.Strategy == "keyword_only" {
			return Decision{Action: "allow"}, true
		}
	}
	if c.PreHash {
		hit, err := s.store.HasRiskHash(ctx, inputPolicyHash(c, r.Input))
		if err != nil {
			s.failures.Add(1)
		}
		if hit {
			return Decision{Blocked: true, Flagged: true, Action: "hash_block", Status: c.BlockStatus, Message: c.BlockMessage}, true
		}
	}
	hash := sha256.Sum256([]byte(r.Input.Hash()))
	if c.SampleRate <= 0 || c.SampleRate < 100 && int(uint16(hash[0])<<8|uint16(hash[1]))%100 >= c.SampleRate {
		return Decision{Action: "sample_skipped"}, true
	}
	return Decision{}, false
}
func (s *Service) worker(id int) {
	defer s.wg.Done()
	for {
		if id >= s.current.Load().config.Workers {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
				continue
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case t := <-s.queue:
			s.queueBytes.Add(-t.bytes)
			if t.notice != nil {
				ctx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
				if err := sendNotice(ctx, t.config, *t.notice); err != nil {
					s.notificationErrors.Add(1)
				} else if err := s.store.MarkRiskEmailSent(ctx, t.notice.ID); err != nil {
					s.failures.Add(1)
				}
				cancel()
				continue
			}
			// Disabling the feature also stops outstanding observation calls.
			if !s.Enabled() {
				continue
			}
			timeout := time.Duration(t.config.TimeoutMS*(t.config.RetryCount+1)+5000) * time.Millisecond
			if t.config.Engine == "chat" {
				timeout = 125 * time.Second
			}
			ctx, cancel := context.WithTimeout(s.ctx, timeout)
			d := s.evaluate(ctx, t.request.Input, t.config)
			d.Blocked = false
			s.record(ctx, t.request, t.config, &d)
			cancel()
		}
	}
}
func (s *Service) evaluate(ctx context.Context, input Input, c Config) Decision {
	if c.Engine == "chat" {
		return s.evaluateAudit(ctx, input, c)
	}
	s.active.Add(1)
	defer s.active.Add(-1)
	start := time.Now()
	scores, err := s.call(ctx, input, c)
	d := Decision{Action: "allow", Scores: scores, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		s.failures.Add(1)
		d.Action = "error"
		d.Error = err.Error()
		return d
	}
	for _, cat := range Categories {
		if score, exists := scores[cat]; exists && score >= c.Thresholds[cat] {
			d.Flagged = true
		}
	}
	if d.Flagged {
		d.Action = "flag"
		if c.Mode == "pre_block" {
			d.Blocked = true
			d.Action = "block"
			d.Status = c.BlockStatus
			d.Message = c.BlockMessage
		}
	}
	return d
}
func (s *Service) call(ctx context.Context, input Input, c Config) (map[string]float64, error) {
	if len(c.APIKeys) == 0 {
		return nil, fmt.Errorf("no moderation API keys configured")
	}
	start := int(s.sequence.Add(1)-1) % len(c.APIKeys)
	var last error = fmt.Errorf("all moderation keys are cooling down")
	for n := 0; n <= c.RetryCount; n++ {
		key := ""
		for j := 0; j < len(c.APIKeys); j++ {
			candidate := c.APIKeys[(start+n+j)%len(c.APIKeys)]
			s.healthMu.Lock()
			h := s.keyHealth(candidate)
			s.healthMu.Unlock()
			if h.FrozenUntil <= time.Now().Unix() {
				key = candidate
				break
			}
		}
		if key == "" {
			break
		}
		begin := time.Now()
		scores, status, err := s.callOnce(ctx, input, c, key)
		s.updateHealth(key, status, time.Since(begin).Milliseconds(), err)
		if err == nil {
			return scores, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}
func (s *Service) callOnce(ctx context.Context, input Input, c Config, key string) (map[string]float64, int, error) {
	var value any = input.Text
	if len(input.Images) > 0 {
		parts := []map[string]any{}
		if input.Text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": input.Text})
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": input.Images[0]}})
		value = parts
	}
	body, _ := json.Marshal(map[string]any{"model": c.Model, "input": value})
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMS)*time.Millisecond)
	defer cancel()
	endpoint := c.BaseURL
	if !hasV1Suffix(endpoint) {
		endpoint += "/v1"
	}
	endpoint += "/moderations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("invalid moderation URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		if c.ProxyURL != "" {
			u, _ := url.Parse(c.ProxyURL)
			transport.Proxy = http.ProxyURL(u)
		}
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("moderation connection or timeout error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode, fmt.Errorf("moderation HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 200, fmt.Errorf("moderation response read error")
	}
	var out struct {
		Results []struct {
			Scores map[string]float64 `json:"category_scores"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Results) == 0 || len(out.Results[0].Scores) == 0 {
		return nil, 200, fmt.Errorf("invalid moderation response")
	}
	scores := out.Results[0].Scores
	known := false
	for _, cat := range Categories {
		if v, ok := scores[cat]; ok {
			known = true
			if v < 0 || v > 1 {
				return nil, 200, fmt.Errorf("invalid moderation score")
			}
		}
	}
	if !known {
		return nil, 200, fmt.Errorf("missing moderation categories")
	}
	return scores, 200, nil
}
func (s *Service) Test(ctx context.Context, input Input) (Decision, error) {
	c := s.Config()
	return s.evaluate(ctx, input, c), nil
}
func (s *Service) TestKeyword(text string) string { return s.current.Load().matcher.Match(text) }
func (s *Service) record(ctx context.Context, r Request, c Config, d *Decision) {
	if !d.Flagged && !c.RecordNonHits {
		return
	}
	e := Event{ID: newEventID(), CreatedAt: time.Now().UnixMilli(), APIKeyID: r.APIKeyID, APIKeyName: r.APIKeyName, Endpoint: r.Endpoint, Model: r.Model, Mode: c.Mode, InputHash: inputPolicyHash(c, r.Input), Excerpt: Redact(r.Input.Text), Decision: *d}
	// Persist the decision and ban atomically before returning a blocking request.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.store.RecordRiskEvent(recordCtx, &e, c); err != nil {
		s.failures.Add(1)
		return
	}
	if c.EmailOnHit && d.Flagged && d.Action != "hash_block" {
		if !s.enqueue(&task{config: c, notice: &e}) {
			s.notificationErrors.Add(1)
		}
	}
}

// TestKey explicitly probes a saved credential even while it is cooling down.
func (s *Service) TestKey(ctx context.Context, id string) (KeyHealth, error) {
	c := s.Config()
	for _, key := range c.APIKeys {
		if keyID(key) == id {
			start := time.Now()
			_, status, err := s.callOnce(ctx, Input{Text: "Hello, have a nice day."}, c, key)
			s.updateHealth(key, status, time.Since(start).Milliseconds(), err)
			s.healthMu.Lock()
			defer s.healthMu.Unlock()
			return s.keyHealth(key), nil
		}
	}
	return KeyHealth{}, fmt.Errorf("moderation key not found")
}

func (s *Service) DeleteKey(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Config()
	keys := []string{}
	found := false
	for _, key := range c.APIKeys {
		if keyID(key) == id {
			found = true
		} else {
			keys = append(keys, key)
		}
	}
	if !found {
		return fmt.Errorf("moderation key not found")
	}
	c.APIKeys = keys
	return s.saveLocked(ctx, c)
}

// SMTP requires STARTTLS or implicit TLS; notifications contain metadata, not prompts.
func sendNotice(ctx context.Context, c Config, e Event) error {
	return sendNoticeTLS(ctx, c, e, &tls.Config{ServerName: c.SMTPHost, MinVersion: tls.VersionTLS12})
}

// Separate the TLS trust configuration for local protocol tests; production
// always uses system roots and the configured SMTP hostname above.
func sendNoticeTLS(ctx context.Context, c Config, e Event, tlsCfg *tls.Config) error {
	addr := net.JoinHostPort(c.SMTPHost, strconv.Itoa(c.SMTPPort))
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if c.SMTPPort == 465 {
		conn = tls.Client(conn, tlsCfg)
	}
	client, err := smtp.NewClient(conn, c.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if c.SMTPPort != 465 {
		if err = client.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if c.SMTPUsername != "" {
		if err = client.Auth(smtp.PlainAuth("", c.SMTPUsername, c.SMTPPassword, c.SMTPHost)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(c.EmailFrom)
	to, _ := mail.ParseAddress(c.EmailTo)
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	if err = client.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: Risk control alert\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nAPI Key ID: %d\r\nAction: %s\r\nViolations: %d\r\nBanned: %t\r\nEvent: %s\r\n", from.Address, to.Address, e.APIKeyID, e.Action, e.ViolationCount, e.AutoBanned, e.ID)
	if err != nil {
		return err
	}
	return w.Close()
}

func (s *Service) enqueue(t *task) bool {
	t.bytes = int64(len(t.request.Input.Text) + 1024)
	for _, image := range t.request.Input.Images {
		t.bytes += int64(len(image))
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.ctx.Err() != nil || len(s.queue) >= t.config.QueueSize || t.bytes > 256*1024 || s.queueBytes.Load()+t.bytes > 32*1024*1024 {
		s.dropped.Add(1)
		return false
	}
	s.queueBytes.Add(t.bytes)
	select {
	case s.queue <- t:
		return true
	default:
		s.queueBytes.Add(-t.bytes)
		s.dropped.Add(1)
		return false
	}
}
