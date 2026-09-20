package riskcontrol

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRiskControlSampling(t *testing.T) {
	c := DefaultConfig()
	r := Request{Model: "Model-A"}
	c.SampleRate = 0
	c.Keywords = []string{"blocked"}
	r.Input.Text = "BLOCKED"
	s := &Service{}
	if d, done := s.local(context.Background(), r, c, newMatcher(c.Keywords)); !done || !d.Blocked {
		t.Fatal("sampling bypassed local keyword check")
	}
	r.Input.Text = "clean"
	if d, done := s.local(context.Background(), r, c, newMatcher(c.Keywords)); !done || d.Action != "sample_skipped" {
		t.Fatal("zero sampling invoked API")
	}
}

func TestRiskControlQueueBudgets(t *testing.T) {
	c := DefaultConfig()
	c.QueueSize = 1
	s := &Service{ctx: context.Background(), queue: make(chan *task, 10)}
	if !s.enqueue(&task{config: c, request: Request{Input: Input{Text: "small"}}}) {
		t.Fatal("small task dropped")
	}
	if s.enqueue(&task{config: c}) {
		t.Fatal("configured capacity ignored")
	}
	entry := <-s.queue
	s.queueBytes.Add(-entry.bytes)
	if s.enqueue(&task{config: c, request: Request{Input: Input{Images: []string{strings.Repeat("x", 256*1024)}}}}) {
		t.Fatal("oversized payload retained")
	}
	if s.queueBytes.Load() != 0 || s.dropped.Load() != 2 {
		t.Fatal("queue accounting")
	}
}

func TestRiskControlAuditRedirectAndInvalidResponse(t *testing.T) {
	var leaked atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	c := DefaultConfig()
	node := auditTestNode("redirect", redirect.URL)
	s := &Service{}
	if _, err := s.callAuditNode(context.Background(), c, node, "test"); err == nil {
		t.Fatalf("redirect accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("followed redirect with sensitive input")
	}
	for _, body := range []string{`{}`, `{"results":[]}`, `{"results":[{"category_scores":{"unknown":1}}]}`, `{"results":[{"category_scores":{"violence":2}}]}`} {
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		node.BaseURL = remote.URL
		if _, err := s.callAuditNode(context.Background(), c, node, "test"); err == nil {
			t.Errorf("accepted invalid response %s", body)
		}
		remote.Close()
	}
}

func TestRiskControlMatcherParity(t *testing.T) {
	for _, tc := range []struct {
		words      []string
		text, want string
	}{
		{[]string{"later", "early"}, "early then LATER", "later"}, {[]string{"bc", "abc"}, "abc", "bc"}, {[]string{"世界", "测试"}, "测试一下世界", "世界"}, {[]string{"secret"}, "topSECRETvalue", "secret"}, {[]string{"x"}, "clean", ""},
	} {
		if got := newMatcher(tc.words).Match(tc.text); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.text, got, tc.want)
		}
	}
	rng := rand.New(rand.NewSource(42))
	for n := 0; n < 500; n++ {
		word := func(size int) string {
			var b strings.Builder
			for i := 0; i < size; i++ {
				b.WriteByte("abcXYZ"[rng.Intn(6)])
			}
			return b.String()
		}
		words := []string{}
		for i := 0; i < 30; i++ {
			words = append(words, word(1+rng.Intn(8)))
		}
		text := word(120)
		want := ""
		for _, w := range words {
			if strings.Contains(strings.ToLower(text), strings.ToLower(w)) {
				want = w
				break
			}
		}
		if got := newMatcher(words).Match(text); got != want {
			t.Fatalf("iteration %d got %q want %q", n, got, want)
		}
	}
}
func TestRiskControlExtraction(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"messages":[{"role":"user","content":"old"},{"role":"user","content":"new\n  line"}]}`, "new line"},
		{`{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"ignore"}]}`, ""},
		{`{"input":"hello"}`, "hello"},
		{`{"input":[{"role":"user","content":[{"type":"input_text","text":"question"}]}]}`, "question"},
		{`{"response":{"input":"websocket"}}`, "websocket"},
		{`{"contents":[{"role":"user","parts":[{"text":"gemini"}]}]}`, "gemini"},
		{`{"prompt":"draw"}`, "draw"},
		{`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"unrelated"},{"type":"text","text":"actual"}]}]}`, "actual"},
	} {
		if got := Extract([]byte(tc.body), "").Text; got != tc.want {
			t.Errorf("%s: got %q want %q", tc.body, got, tc.want)
		}
	}
	in := Extract([]byte(`{"input":"`+strings.Repeat("字", 12001)+`"}`), "")
	if len([]rune(in.Text)) != 12000 {
		t.Fatal("input limit")
	}
	in = Extract([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/image"}}]}]}`), "")
	if len(in.Images) != 1 {
		t.Fatalf("image extraction: %+v", in)
	}
}
func TestRiskControlConfigAndRedaction(t *testing.T) {
	c := DefaultConfig()
	c.Keywords = []string{" A ", "a", "", "中文"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Keywords) != 2 {
		t.Fatal(c.Keywords)
	}
	c.Workers = 0
	if c.Validate() == nil {
		t.Fatal("worker bounds")
	}
	c = DefaultConfig()
	c.Audit.BlockThreshold = 2
	if c.Validate() == nil {
		t.Fatal("threshold bounds")
	}
	c = DefaultConfig()
	c.Audit.Nodes = []AuditNode{auditTestNode("invalid", "https://user:secret@example.test")}
	if c.Validate() == nil {
		t.Fatal("URL credentials")
	}
	for _, secret := range []string{"https://example.test?token=secret", "Bearer abcdef123456789", "password=secret123"} {
		if strings.Contains(Redact(secret), "secret") || strings.Contains(Redact(secret), "abcdef") {
			t.Fatalf("redaction failed: %s", Redact(secret))
		}
	}
}
