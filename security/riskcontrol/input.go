package riskcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/tidwall/gjson"
	"regexp"
	"strings"
)

type Input struct {
	Text   string   `json:"text"`
	Images []string `json:"images,omitempty"`
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func (i Input) Hash() string {
	b, _ := json.Marshal(i)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Extract(body []byte, endpoint string) Input {
	return extractWithLimit(body, endpoint, 12000)
}
func extractWithLimit(body []byte, endpoint string, limit int) Input {
	var text []string
	images := []string{}
	var walk func(gjson.Result)
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			text = append(text, s)
		}
	}
	walk = func(v gjson.Result) {
		if v.Type == gjson.String {
			add(v.String())
			return
		}
		if v.IsArray() {
			for _, x := range v.Array() {
				walk(x)
			}
			return
		}
		if !v.IsObject() {
			return
		}
		switch v.Get("type").String() {
		case "tool_result", "tool_use", "function_call", "function_call_output":
			return
		}
		for _, p := range []string{"text", "content"} {
			if x := v.Get(p); x.Exists() {
				walk(x)
			}
		}
		for _, p := range []string{"image_url.url", "image_url", "url"} {
			x := v.Get(p)
			if x.Type == gjson.String {
				s := x.String()
				if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "data:image/") {
					images = append(images, s)
				}
			}
		}
		if data := v.Get("source.data").String(); data != "" && strings.HasPrefix(v.Get("source.media_type").String(), "image/") {
			images = append(images, "data:"+v.Get("source.media_type").String()+";base64,"+data)
		}
	}
	last := func(v gjson.Result) gjson.Result {
		a := v.Array()
		if len(a) == 0 {
			return gjson.Result{}
		}
		return a[len(a)-1]
	}
	root := gjson.ParseBytes(body)
	// response.create frames may wrap input in response.
	if root.Get("response").IsObject() {
		root = root.Get("response")
	}
	if v := root.Get("messages"); v.IsArray() {
		m := last(v)
		if m.Get("role").String() == "user" {
			walk(m.Get("content"))
		}
	} else if v := root.Get("input"); v.Exists() {
		if v.IsArray() {
			v = last(v)
		}
		role := v.Get("role").String()
		if role == "" || role == "user" {
			walk(v)
		}
	} else if v := root.Get("contents"); v.IsArray() {
		m := last(v)
		role := m.Get("role").String()
		if role == "" || role == "user" {
			walk(m.Get("parts"))
		}
	} else {
		add(root.Get("prompt").String())
		walk(root.Get("images"))
	}
	return Input{Text: truncate(strings.Join(strings.Fields(strings.Join(text, "\n")), " "), limit), Images: unique(images, false)}
}

// AC automaton: terminal output stores the earliest configured keyword index.
type node struct {
	edges       map[rune]int
	fail, match int
}
type matcher struct {
	nodes []node
	words []string
}

func newMatcher(words []string) *matcher {
	m := &matcher{nodes: []node{{edges: map[rune]int{}, match: -1}}, words: words}
	for idx, w := range words {
		s := 0
		for _, ch := range strings.ToLower(w) {
			n, ok := m.nodes[s].edges[ch]
			if !ok {
				n = len(m.nodes)
				m.nodes = append(m.nodes, node{edges: map[rune]int{}, match: -1})
				m.nodes[s].edges[ch] = n
			}
			s = n
		}
		if w != "" && (m.nodes[s].match < 0 || idx < m.nodes[s].match) {
			m.nodes[s].match = idx
		}
	}
	q := []int{}
	for _, n := range m.nodes[0].edges {
		q = append(q, n)
	}
	for head := 0; head < len(q); head++ {
		s := q[head]
		for ch, n := range m.nodes[s].edges {
			f := m.nodes[s].fail
			for f > 0 && m.nodes[f].edges[ch] == 0 {
				f = m.nodes[f].fail
			}
			m.nodes[n].fail = m.nodes[f].edges[ch]
			other := m.nodes[m.nodes[n].fail].match
			if other >= 0 && (m.nodes[n].match < 0 || other < m.nodes[n].match) {
				m.nodes[n].match = other
			}
			q = append(q, n)
		}
	}
	return m
}
func (m *matcher) Match(text string) string {
	s, best := 0, -1
	for _, ch := range strings.ToLower(text) {
		for s > 0 && m.nodes[s].edges[ch] == 0 {
			s = m.nodes[s].fail
		}
		s = m.nodes[s].edges[ch]
		n := m.nodes[s].match
		if n >= 0 && (best < 0 || n < best) {
			best = n
		}
		if best == 0 {
			break
		}
	}
	if best >= 0 {
		return m.words[best]
	}
	return ""
}

var redactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)https?://[^\s<>"']+`),
	regexp.MustCompile(`(?i)(?:bearer\s+|(?:api[_-]?key|token|password|secret|authorization)\s*[:=]\s*)[^\s,;]+`),
	regexp.MustCompile(`\b(?:sk[-_][a-zA-Z0-9_-]+|eyJ[a-zA-Z0-9_.-]+|[a-zA-Z0-9_+/=-]{40,})\b`),
}

func Redact(s string) string {
	for _, r := range redactors {
		s = r.ReplaceAllString(s, "[已脱敏]")
	}
	return truncate(s, 240)
}
