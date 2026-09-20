package riskcontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/http"
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
	ViolationCount int  `json:"violation_count,omitempty"`
	AutoBanned     bool `json:"auto_banned,omitempty"`
	EmailSent      bool `json:"email_sent,omitempty"`
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
}
type Store interface {
	LoadRiskConfig(context.Context) (Config, error)
	SaveRiskConfig(context.Context, Config) error
	HasRiskHash(context.Context, string) (bool, error)
	DeleteRiskHash(context.Context, string) error
	RecordRiskEvent(context.Context, *Event, Config) error
	RiskLogs(context.Context, LogFilter) (LogPage, error)
	RiskStats(context.Context) (Stats, error)
	CleanupRiskLogs(context.Context, Config) (int64, error)
}
type snapshot struct {
	config  Config
	matcher *matcher
}
type task struct {
	request Request
	config  Config
	bytes   int64
}
type Runtime struct {
	QueueBytes int64 `json:"queue_bytes"`
	Stats
	QueueLength int   `json:"queue_length"`
	Active      int64 `json:"active"`
	Checked     int64 `json:"checked"`
	Dropped     int64 `json:"dropped"`
	Errors      int64 `json:"errors"`
}
type Service struct {
	queueMu                            sync.Mutex
	queueBytes                         atomic.Int64
	store                              Store
	current                            atomic.Pointer[snapshot]
	mu                                 sync.Mutex
	queue                              chan *task
	ctx                                context.Context
	cancel                             context.CancelFunc
	wg                                 sync.WaitGroup
	active, checked, dropped, failures atomic.Int64
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
	s := &Service{store: store, queue: make(chan *task, 100000), ctx: ctx, cancel: cancel}
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

// FallbackOnBlockEnabled reads the routing option without cloning credentials.
func (s *Service) FallbackOnBlockEnabled() bool {
	c := s.current.Load().config
	return c.Mode == "pre_block" && c.FallbackOnBlock
}
func (s *Service) Cleanup(ctx context.Context) (int64, error) {
	return s.store.CleanupRiskLogs(ctx, s.Config())
}
func (s *Service) Status(ctx context.Context) (Runtime, error) {
	st, err := s.store.RiskStats(ctx)
	if err != nil {
		return Runtime{}, err
	}
	return Runtime{QueueBytes: s.queueBytes.Load(), Stats: st, QueueLength: len(s.queue), Active: s.active.Load(), Checked: s.checked.Load(), Dropped: s.dropped.Load(), Errors: s.failures.Load()}, nil
}
func (s *Service) Check(ctx context.Context, r Request) Decision {
	snap := s.current.Load()
	c := snap.config
	if !c.Enabled || c.Mode == "off" || (r.Input.Text == "" && len(r.Input.Images) == 0) {
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
			// Disabling the feature also stops outstanding observation calls.
			if !s.Enabled() {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, 125*time.Second)
			d := s.evaluate(ctx, t.request.Input, t.config)
			d.Blocked = false
			s.record(ctx, t.request, t.config, &d)
			cancel()
		}
	}
}
func (s *Service) evaluate(ctx context.Context, input Input, c Config) Decision {
	return s.evaluateAudit(ctx, input, c)
}
func (s *Service) Test(ctx context.Context, input Input) (Decision, error) {
	c := s.Config()
	return s.evaluate(ctx, input, c), nil
}
func (s *Service) TestKeyword(text string) string { return s.current.Load().matcher.Match(text) }
func (s *Service) record(ctx context.Context, r Request, c Config, d *Decision) {
	// Review failures remain visible even when routine pass logging is off.
	if !d.Flagged && d.Action != "error" && !c.RecordNonHits {
		return
	}
	e := Event{ID: newEventID(), CreatedAt: time.Now().UnixMilli(), APIKeyID: r.APIKeyID, APIKeyName: r.APIKeyName, Endpoint: r.Endpoint, Model: r.Model, Mode: c.Mode, InputHash: inputPolicyHash(c, r.Input), Excerpt: Redact(r.Input.Text), Decision: *d}
	// Persist the audit decision atomically before returning a blocking request.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.store.RecordRiskEvent(recordCtx, &e, c); err != nil {
		s.failures.Add(1)
		return
	}

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
