package database

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/security/riskcontrol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRiskControlPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("RISK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set RISK_TEST_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	c := riskcontrol.DefaultConfig()
	c.AutoBan = true
	c.BanThreshold = 4
	if err = db.SaveRiskConfig(ctx, c); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LoadRiskConfig(ctx); err != nil || got.BanThreshold != 0 || got.AutoBan {
		t.Fatalf("config: %+v %v", got, err)
	}
	id := time.Now().UnixNano()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := riskcontrol.Event{ID: fmt.Sprintf("pg-%d-%d", id, i), CreatedAt: time.Now().UnixMilli(), APIKeyID: id, InputHash: fmt.Sprint(id), Decision: riskcontrol.Decision{Action: "block", Flagged: true, Blocked: true}}
			errs <- db.RecordRiskEvent(ctx, &e, c)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var banCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM risk_control_bans WHERE api_key_id=$1`, id).Scan(&banCount); err != nil || banCount != 0 {
		t.Fatalf("legacy ban unexpectedly created: %d %v", banCount, err)
	}
	page, err := db.RiskLogs(ctx, riskcontrol.LogFilter{APIKeyID: id, PageSize: 20})
	if err != nil || page.Total != 8 {
		t.Fatalf("logs: %+v %v", page, err)
	}
	if hit, err := db.HasRiskHash(ctx, fmt.Sprint(id)); err != nil || !hit {
		t.Fatal("missing hash", err)
	}
	if err = db.DeleteRiskHash(ctx, fmt.Sprint(id)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.RiskStats(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CleanupRiskLogs(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func riskTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "risk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func TestRiskControlPersistence(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	c := riskcontrol.DefaultConfig()
	c.Enabled = true
	c.Keywords = []string{"blocked"}
	c.APIKeys = []string{"private-test-key"}
	if err := db.SaveRiskConfig(ctx, c); err != nil {
		t.Fatal(err)
	}
	read, err := db.LoadRiskConfig(ctx)
	if err != nil || len(read.APIKeys) != 0 || read.Engine != "chat" {
		t.Fatalf("load %+v %v", read, err)
	}
	c.AutoBan = true
	c.BanThreshold = 2
	c.WindowHours = 24
	event := func(id, action string) *riskcontrol.Event {
		return &riskcontrol.Event{ID: id, CreatedAt: time.Now().UnixMilli(), APIKeyID: 9, APIKeyName: "tester", InputHash: "hash", Decision: riskcontrol.Decision{Flagged: true, Blocked: true, Action: action}}
	}
	for _, id := range []string{"one", "two"} {
		e := event(id, "keyword_block")
		if err := db.RecordRiskEvent(ctx, e, c); err != nil {
			t.Fatal(err)
		}
	}
	if hit, _ := db.HasRiskHash(ctx, "hash"); hit {
		t.Fatal("keyword wrote hash")
	}
	var banCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM risk_control_bans`).Scan(&banCount); err != nil || banCount != 0 {
		t.Fatalf("legacy ban unexpectedly created: %d %v", banCount, err)
	}
	logs, err := db.RiskLogs(ctx, riskcontrol.LogFilter{Page: 1, PageSize: 1, Action: "keyword_block", APIKeyID: 9})
	if err != nil || logs.Total != 2 || len(logs.Items) != 1 {
		t.Fatalf("logs %+v %v", logs, err)
	}
	e := event("three", "block")
	if err := db.RecordRiskEvent(ctx, e, c); err != nil {
		t.Fatal(err)
	}
	if e.ViolationCount != 0 || e.AutoBanned {
		t.Fatalf("removed violation counter=%d", e.ViolationCount)
	}
	if hit, _ := db.HasRiskHash(ctx, "hash"); !hit {
		t.Fatal("API hit did not persist hash")
	}
	hash := event("four", "hash_block")
	if err := db.RecordRiskEvent(ctx, hash, c); err != nil {
		t.Fatal(err)
	}
	if hash.ViolationCount != 0 {
		t.Fatal("hash counted")
	}
	if err := db.DeleteRiskHash(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	if hit, _ := db.HasRiskHash(ctx, "hash"); hit {
		t.Fatal("delete failed")
	}
	old := event("old", "allow")
	old.Flagged = false
	old.CreatedAt = time.Now().AddDate(0, 0, -4).UnixMilli()
	if err := db.RecordRiskEvent(ctx, old, c); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CleanupRiskLogs(ctx, c); err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
}
func TestRiskControlServicePaths(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	var calls atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing key")
		}
		var b map[string]any
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			t.Error("body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"risk\":\"unsafe\",\"confidence\":0.99}"}}]}`))
	}))
	defer remote.Close()
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := riskcontrol.DefaultConfig()
	c.Enabled = true
	c.Audit.Nodes = []riskcontrol.AuditNode{{ID: "audit", Name: "audit", Enabled: true, BaseURL: remote.URL, Model: "custom", APIKey: "test-key", TimeoutMS: 1000, MaxInputChars: 400000}}
	c.Keywords = []string{"secret"}
	r := riskcontrol.Request{APIKeyID: 1, APIKeyName: "test", Endpoint: "/v1/responses", Model: "model", Input: riskcontrol.Input{Text: "contains SECRET"}}
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	d := s.Check(ctx, r)
	if !d.Blocked || d.Action != "keyword_block" || calls.Load() != 0 {
		t.Fatalf("keyword: %+v calls=%d", d, calls.Load())
	}
	c.Strategy = "keyword_only"
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	r.Input.Text = "clean"
	d = s.Check(ctx, r)
	if d.Blocked || calls.Load() != 0 {
		t.Fatal("keyword only invoked API")
	}
	c.Strategy = "api_only"
	c.PreHash = true
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	d = s.Check(ctx, r)
	if !d.Blocked || d.Action != "block" || calls.Load() != 1 {
		t.Fatalf("API: %+v", d)
	}
	d = s.Check(ctx, r)
	if d.Action != "hash_block" || calls.Load() != 1 {
		t.Fatalf("hash: %+v", d)
	}
	c.Mode = "observe"
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	d = s.Check(ctx, r)
	if !d.Blocked || d.Action != "hash_block" {
		t.Fatal("observe hash semantics")
	}
	c.PreHash = false
	c.Strategy = "keyword_and_api"
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	r.Input.Text = "secret observed"
	d = s.Check(ctx, r)
	if d.Blocked || d.Action != "queued" {
		t.Fatalf("observe: %+v", d)
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatal("observe worker did not run")
	}
	c.Enabled = false
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if d = s.Check(ctx, r); d.Blocked || d.Action != "allow" {
		t.Fatal("disabled")
	}
	c.Enabled = true
	c.Mode = "pre_block"
	c.ModelFilter = "include"
	c.Models = []string{"other"}
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if d = s.Check(ctx, r); !d.Blocked {
		t.Fatal("retired model scope bypassed review")
	}
}
func TestRiskControlFailureAndTestNoSideEffects(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := riskcontrol.DefaultConfig()
	c.Enabled = true
	c.RecordNonHits = true
	c.Audit.FailOpen = true
	c.Strategy = "api_only"
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	d := s.Check(ctx, riskcontrol.Request{APIKeyID: 1, Input: riskcontrol.Input{Text: "test"}})
	if d.Blocked || d.Action != "error" {
		t.Fatalf("fail-open: %+v", d)
	}
	before, _ := db.RiskStats(ctx)
	_, _ = s.Test(ctx, riskcontrol.Input{Text: "test"})
	after, _ := db.RiskStats(ctx)
	if before != after {
		t.Fatal("test wrote persistent side effects")
	}
}

func TestRiskControlModelAuditPersistenceAndDryRun(t *testing.T) {
	ctx := context.Background()
	db := riskTestDB(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"risk":"unsafe","confidence":0.5}`}}}})
	}))
	defer remote.Close()
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Engine, c.Enabled, c.Strategy, c.PreHash = "chat", true, "api_only", true
	c.Audit.Nodes = []riskcontrol.AuditNode{{ID: "local", Name: "Local", Enabled: true, Model: "custom", BaseURL: remote.URL, APIKey: "fixture-private-key", TimeoutMS: 1000, MaxInputChars: 1000}}
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadRiskConfig(ctx)
	if err != nil || loaded.Engine != "chat" || loaded.Audit.Nodes[0].APIKey != "fixture-private-key" {
		t.Fatalf("persisted model config %+v %v", loaded, err)
	}
	before, _ := db.RiskStats(ctx)
	if d, err := s.TestAudit(ctx, c.Audit, "trial", ""); err != nil || d.Blocked || d.Action != "flag" {
		t.Fatalf("trial %+v %v", d, err)
	}
	after, _ := db.RiskStats(ctx)
	if before != after {
		t.Fatal("trial wrote logs, hash or ban")
	}
	r := riskcontrol.Request{APIKeyID: 4, Input: riskcontrol.Input{Text: "same content"}}
	for i := 0; i < 2; i++ {
		if d := s.Check(ctx, r); d.Blocked || d.Action != "flag" {
			t.Fatalf("nonblocking result turned into hash block %+v", d)
		}
	}
	st, _ := db.RiskStats(ctx)
	if st.Hashes != 0 || st.Total != 2 {
		t.Fatalf("flag-only events cached as blocked: %+v", st)
	}
	c.Audit.BlockThreshold = .45
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if d := s.Check(ctx, r); !d.Blocked || d.Action != "block" {
		t.Fatalf("threshold update ignored %+v", d)
	}
	if d := s.Check(ctx, r); d.Action != "hash_block" {
		t.Fatalf("blocking result not cached %+v", d)
	}
	c.Audit.BlockThreshold = .9
	if err = s.Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	if d := s.Check(ctx, r); d.Blocked || d.Action != "flag" {
		t.Fatalf("stale policy hash reused %+v", d)
	}
}

func TestRiskControlLegacyBansHaveNoEffect(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO risk_control_bans(api_key_id,name,blocked,created_at,reset_at) VALUES(9,'old',TRUE,1,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO risk_control_config(id,payload) VALUES(1,'{"auto_ban_enabled":true,"ban_threshold":1,"violation_window_hours":24}')`); err != nil {
		t.Fatal(err)
	}
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Config().AutoBan || s.Config().BanThreshold != 0 {
		t.Fatal("legacy config remained active")
	}
	for _, enabled := range []bool{false, true} {
		cfg := s.Config()
		cfg.Enabled = enabled
		cfg.Strategy = "keyword_only"
		cfg.Keywords = []string{"blocked"}
		if err := s.Update(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		if d := s.Check(ctx, riskcontrol.Request{APIKeyID: 9, Input: riskcontrol.Input{Text: "clean"}}); d.Blocked {
			t.Fatalf("legacy ban affected request: %+v", d)
		}
	}
	var blocked bool
	if err := db.conn.QueryRowContext(ctx, `SELECT blocked FROM risk_control_bans WHERE api_key_id=9`).Scan(&blocked); err != nil || !blocked {
		t.Fatal("legacy history was destroyed", err)
	}
}

func TestRiskControlFallbackSwitchDefaultsAndPersistence(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	cfg, err := db.LoadRiskConfig(ctx)
	if err != nil || !cfg.FallbackOnBlock {
		t.Fatalf("new config should default on: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO risk_control_config(id,payload) VALUES(1,'{"enabled":true}')`); err != nil {
		t.Fatal(err)
	}
	cfg, err = db.LoadRiskConfig(ctx)
	if err != nil || !cfg.FallbackOnBlock {
		t.Fatalf("legacy config should default on: %v", err)
	}
	for _, enabled := range []bool{false, true} {
		cfg.FallbackOnBlock = enabled
		if err := db.SaveRiskConfig(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		service, err := riskcontrol.New(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		got := service.Config().FallbackOnBlock
		service.Close()
		if got != enabled {
			t.Fatalf("restart lost explicit switch: got %v want %v", got, enabled)
		}
	}
}

func TestRiskControlLegacyScopeAndEmailAreIgnored(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO risk_control_config(id,payload) VALUES(1,'{"enabled":true,"keyword_blocking_mode":"keyword_only","blocked_keywords":["blocked"],"model_filter":"include","models":["old-model"],"api_key_ids":[7],"group_ids":[9],"email_on_hit":true,"smtp_host":"legacy.example","smtp_password":"old-secret"}')`); err != nil {
		t.Fatal(err)
	}
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, model := range []string{"old-model", "new-model", "vendor/custom"} {
		d := s.Check(ctx, riskcontrol.Request{APIKeyID: 123, Model: model, Input: riskcontrol.Input{Text: "blocked"}})
		if !d.Blocked {
			t.Fatalf("review skipped model %s", model)
		}
	}
	cfg := s.Config()
	if cfg.EmailOnHit || cfg.SMTPPassword != "" || cfg.ModelFilter != "all" || len(cfg.Models)+len(cfg.APIKeyIDs)+len(cfg.GroupIDs) != 0 {
		t.Fatalf("legacy settings active")
	}
	logs, err := db.RiskLogs(ctx, riskcontrol.LogFilter{})
	if err != nil || logs.Total != 3 {
		t.Fatalf("missing audit evidence: %+v %v", logs, err)
	}
	for _, event := range logs.Items {
		if event.EmailSent {
			t.Fatal("unexpected email")
		}
	}
}

func TestRiskControlLegacyEngineKeepsCustomPool(t *testing.T) {
	db := riskTestDB(t)
	ctx := context.Background()
	cfg := riskcontrol.DefaultConfig()
	cfg.Engine = "moderations"
	cfg.APIKeys = []string{"retired-credential"}
	cfg.BaseURL = "http://retired.example"
	cfg.Audit.Nodes = []riskcontrol.AuditNode{{ID: "existing", Name: "Existing", Enabled: true, BaseURL: "http://audit.example/v1", Model: "vendor/custom", APIKey: "keep-this-secret", TimeoutMS: 1000, MaxInputChars: 1000}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO risk_control_config(id,payload) VALUES(1,$1)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := s.Config()
	if got.Engine != "chat" || len(got.APIKeys) != 0 || got.BaseURL != "" {
		t.Fatal("legacy credentials still active")
	}
	if len(got.Audit.Nodes) != 1 || got.Audit.Nodes[0].APIKey != "keep-this-secret" || got.Audit.Nodes[0].Model != "vendor/custom" {
		t.Fatal("existing pool lost")
	}
	if err := s.Patch(ctx, []byte(`{"audit_engine":"moderations","api_keys":["old-client-key"],"sample_rate":75}`)); err != nil {
		t.Fatal(err)
	}
	if s.Config().Engine != "chat" || s.Config().Audit.Nodes[0].APIKey != "keep-this-secret" {
		t.Fatal("legacy client changed active pool")
	}
}
