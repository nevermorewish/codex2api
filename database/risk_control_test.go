package database

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codex2api/security/riskcontrol"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRiskControlNotificationFailureIsAsync(t *testing.T) {
	// A local SMTP endpoint that immediately closes exercises notification errors
	// without external credentials or outbound email.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	db := riskTestDB(t)
	s, err := riskcontrol.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	c.Enabled, c.EmailOnHit, c.Strategy = true, true, "keyword_only"
	c.Keywords = []string{"blocked"}
	c.SMTPHost, c.EmailFrom, c.EmailTo = "127.0.0.1", "from@example.test", "to@example.test"
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	c.SMTPPort, _ = strconv.Atoi(port)
	if err = s.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	d := s.Check(context.Background(), riskcontrol.Request{APIKeyID: 1, Input: riskcontrol.Input{Text: "blocked"}})
	if !d.Blocked {
		t.Fatal("notification failure changed moderation decision")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := s.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.NotificationErrors == 1 {
			logs, err := db.RiskLogs(context.Background(), riskcontrol.LogFilter{})
			if err != nil || len(logs.Items) != 1 || logs.Items[0].EmailSent {
				t.Fatalf("incorrect notification persistence: %+v %v", logs, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("notification worker did not report failure")
}

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
	if got, err := db.LoadRiskConfig(ctx); err != nil || got.BanThreshold != 4 {
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
	if banned, err := db.RiskBan(ctx, id); err != nil || !banned {
		t.Fatalf("ban: %v %v", banned, err)
	}
	page, err := db.RiskLogs(ctx, riskcontrol.LogFilter{APIKeyID: id, PageSize: 20})
	if err != nil || page.Total != 8 {
		t.Fatalf("logs: %+v %v", page, err)
	}
	if err = db.UnbanRiskKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	if banned, _ := db.RiskBan(ctx, id); banned {
		t.Fatal("unban failed")
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
	if err != nil || len(read.APIKeys) != 1 || read.APIKeys[0] != c.APIKeys[0] {
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
	if banned, _ := db.RiskBan(ctx, 9); !banned {
		t.Fatal("threshold failed to ban")
	}
	logs, err := db.RiskLogs(ctx, riskcontrol.LogFilter{Page: 1, PageSize: 1, Action: "keyword_block", APIKeyID: 9})
	if err != nil || logs.Total != 2 || len(logs.Items) != 1 {
		t.Fatalf("logs %+v %v", logs, err)
	}
	if err := db.UnbanRiskKey(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if banned, _ := db.RiskBan(ctx, 9); banned {
		t.Fatal("unban failed")
	}
	time.Sleep(2 * time.Millisecond)
	e := event("three", "block")
	if err := db.RecordRiskEvent(ctx, e, c); err != nil {
		t.Fatal(err)
	}
	if e.ViolationCount != 1 {
		t.Fatalf("reset count=%d", e.ViolationCount)
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
		if r.URL.Path != "/v1/moderations" {
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
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"violence":0.99}}]}`))
	}))
	defer remote.Close()
	s, err := riskcontrol.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := riskcontrol.DefaultConfig()
	c.Enabled = true
	c.APIKeys = []string{"test-key"}
	c.BaseURL = remote.URL
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
	if d = s.Check(ctx, r); d.Blocked {
		t.Fatal("model scope")
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
