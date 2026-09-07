package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// Production bodies stay in the local evidence directory, never in source
// fixtures or test output. Set GPT400_FIXTURE_DIR to replay their conversions.
func TestCodexRecordedResponsesPreserveContentAndTools(t *testing.T) {
	root := os.Getenv("GPT400_FIXTURE_DIR")
	if root == "" {
		t.Skip("set GPT400_FIXTURE_DIR to the local GPT 400 evidence directory")
	}
	files, err := filepath.Glob(filepath.Join(root, "samples", "*", "upstream_*.body.json"))
	if err != nil || len(files) == 0 {
		t.Fatal("no recorded request bodies found")
	}
	responses, carriersWithContent := 0, 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil || !gjson.ValidBytes(raw) {
			t.Fatalf("cannot read valid JSON from %s", filepath.Base(filepath.Dir(file)))
		}
		input := gjson.GetBytes(raw, "input")
		if !input.IsArray() {
			continue // Captured Chat Completions use a separate translation path.
		}
		responses++
		t.Run(filepath.Base(filepath.Dir(file)), func(t *testing.T) {
			before := bytes.Clone(raw)
			out := PrepareOpenAIResponsesBody(raw)
			if !gjson.ValidBytes(out) || !bytes.Equal(raw, before) {
				t.Fatal("invalid converted body or mutated original fixture")
			}
			items := gjson.GetBytes(out, "input").Array()
			cursor := 0
			for _, original := range input.Array() {
				isCarrier := original.Get("type").String() == "additional_tools"
				content := original.Get("content")
				if isCarrier && content.Exists() && content.Type != gjson.Null {
					carriersWithContent++
					if cursor >= len(items) || !reflect.DeepEqual(content.Value(), items[cursor].Get("content").Value()) {
						t.Fatalf("carrier content was lost or altered at input[%d]", cursor)
					}
					role := original.Get("role").String()
					if role == "" {
						role = "developer"
					}
					if items[cursor].Get("role").String() != role {
						t.Fatal("carrier context changed role")
					}
					cursor++
				}
				if cursor >= len(items) {
					t.Fatal("input item was lost")
				}
				converted := items[cursor]
				for _, field := range []string{"type", "role", "tools", "call_id", "arguments", "output", "encrypted_content"} {
					if !reflect.DeepEqual(original.Get(field).Value(), converted.Get(field).Value()) {
						t.Fatalf("input[%d].%s changed", cursor, field)
					}
				}
				if isCarrier {
					if converted.Get("content").Exists() {
						t.Fatal("additional_tools still carries the rejected content field")
					}
				} else if !reflect.DeepEqual(content.Value(), converted.Get("content").Value()) {
					t.Fatalf("ordinary content changed at input[%d]", cursor)
				}
				cursor++
			}
			if cursor != len(items) {
				t.Fatal("unexpected extra input items")
			}
			// Native HTTP/WS support the Lite carrier. Its complete context must
			// survive those routes too, including the largest real request.
			for name, prepare := range map[string]func([]byte) ([]byte, string){
				"native_http": PrepareResponsesBody, "native_websocket": PrepareResponsesWebSocketBody,
			} {
				native, _ := prepare(raw)
				var actual []gjson.Result
				for _, item := range gjson.GetBytes(native, "input").Array() {
					if item.Get("type").String() == "additional_tools" {
						actual = append(actual, item)
					}
				}
				index := 0
				for _, item := range input.Array() {
					if item.Get("type").String() != "additional_tools" {
						continue
					}
					if index >= len(actual) || !reflect.DeepEqual(item.Get("content").Value(), actual[index].Get("content").Value()) {
						t.Fatalf("%s lost native carrier content", name)
					}
					index++
				}
				if index != len(actual) {
					t.Fatalf("%s changed native carrier count", name)
				}
			}
		})
	}
	if responses == 0 || carriersWithContent == 0 {
		t.Fatal("recordings did not exercise Responses with additional_tools.content")
	}
	t.Logf("validated %d recorded Responses bodies and %d carriers with content", responses, carriersWithContent)
}
