package proxy

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestCodexStreamOptionsPreservedOnAllResponsesPaths(t *testing.T) {
	options := `{"reasoning_summary_delivery":"auto","include_obfuscation":false,"future_delivery":{"enabled":false,"count":0}}`
	raw := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"keep CLI and Desktop content"}],"stream_options":` + options + `}`)
	for name, prepare := range map[string]func([]byte) []byte{
		"native_http":      func(b []byte) []byte { out, _ := PrepareResponsesBody(b); return out },
		"native_websocket": func(b []byte) []byte { out, _ := PrepareResponsesWebSocketBody(b); return out },
		"openai_relay":     PrepareOpenAIResponsesBody,
	} {
		t.Run(name, func(t *testing.T) {
			out := prepare(raw)
			got := gjson.GetBytes(out, "stream_options")
			if !reflect.DeepEqual(got.Value(), gjson.Parse(options).Value()) {
				t.Fatalf("stream options lost: %s", out)
			}
			if gjson.GetBytes(out, "input.0.content").String() != "keep CLI and Desktop content" {
				t.Fatalf("message changed: %s", out)
			}
		})
	}
}

func TestCodexItemReferenceToolOutputsRemainToolOutputs(t *testing.T) {
	input := `[{"type":"item_reference","id":"call_1"},{"type":"function_call_output","call_id":"call_1","output":"original tool output"},{"type":"item_reference","id":"call_2"},{"type":"custom_tool_call_output","call_id":"call_2","output":"original custom output"}]`
	raw := []byte(`{"model":"gpt-6-astra","input":` + input + `}`)
	for name, prepare := range map[string]func([]byte) []byte{
		"native_http":      func(b []byte) []byte { out, _ := PrepareResponsesBody(b); return out },
		"native_websocket": func(b []byte) []byte { out, _ := PrepareResponsesWebSocketBody(b); return out },
		"openai_relay":     PrepareOpenAIResponsesBody,
	} {
		t.Run(name, func(t *testing.T) {
			out := prepare(raw)
			if !reflect.DeepEqual(gjson.GetBytes(out, "input").Value(), gjson.Parse(input).Value()) {
				t.Fatalf("reference ID or output relationship was altered: %s", out)
			}
		})
	}
}

func TestCodexAdditionalToolsSplitPreservesRoleAndRichContent(t *testing.T) {
	for _, role := range []string{"developer", "user", "system"} {
		content := `[{"type":"input_text","text":"full instructions"},{"type":"input_image","image_url":"https://example.com/image.png","detail":"auto"}]`
		raw := []byte(`{"model":"gpt-6-astra","input":[{"type":"additional_tools","role":"` + role + `","id":"at_1","content":` + content + `,"tools":[]}]}`)
		out := PrepareOpenAIResponsesBody(raw)
		if gjson.GetBytes(out, "input.0.role").String() != role || !reflect.DeepEqual(gjson.GetBytes(out, "input.0.content").Value(), gjson.Parse(content).Value()) {
			t.Fatalf("role or rich content changed: %s", out)
		}
		if gjson.GetBytes(out, "input.1.type").String() != "additional_tools" || gjson.GetBytes(out, "input.1.content").Exists() {
			t.Fatalf("carrier was not preserved separately: %s", out)
		}
	}
}

func TestCodexOpenAIRelayPreservesContinuationHeaders(t *testing.T) {
	for _, ua := range []string{"codex_cli_rs/0.153.0", "Codex Desktop/0.153.4 (Windows 10.0.19045; x86_64)"} {
		headers := make(http.Header)
		for k, v := range map[string]string{
			"User-Agent": ua, "OpenAI-Beta": "responses_websockets=2026-02-06", "Originator": "codex_cli_rs",
			"X-Codex-Turn-State": "opaque-state", "X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
			"X-Codex-Beta-Features": "remote_compaction_v2", "X-Client-Request-Id": "thread-1",
			"X-OpenAI-Internal-Codex-Responses-Lite": "true", "Session-Id": "session-1", "Thread-Id": "thread-1",
			"X-Oai-Attestation": "client-proof",
			"Conversation-Id":   "conversation-1", "Session_id": "legacy-session", "Conversation_id": "legacy-conversation",
		} {
			headers.Set(k, v)
		}
		before := headers.Clone()
		req, _ := http.NewRequest(http.MethodPost, "https://relay.example/v1/responses", nil)
		applyOpenAIResponsesRequestHeaders(req, &auth.Account{UpstreamType: auth.UpstreamOpenAIResponses}, "upstream-key", headers)
		for _, name := range []string{"OpenAI-Beta", "Originator", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Beta-Features", "X-Client-Request-Id", codexResponsesLiteHeader, "Session-Id", "Thread-Id", "Conversation-Id", "Session_id", "Conversation_id", "X-Oai-Attestation"} {
			if req.Header.Get(name) != headers.Get(name) {
				t.Errorf("%s: header %s lost", ua, name)
			}
		}
		if req.Header.Get("Authorization") != "Bearer upstream-key" || !reflect.DeepEqual(headers, before) {
			t.Fatal("upstream authentication or input headers changed")
		}
	}
}
