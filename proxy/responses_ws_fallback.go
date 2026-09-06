package proxy

import "github.com/tidwall/sjson"

// Only self-contained turns reach this adapter. Keep the original Responses
// request options, remove the WS envelope, and require SSE for the WS bridge.
func prepareResponsesWSFallbackBody(rawBody []byte) []byte {
	body := PrepareOpenAIResponsesBody(rawBody)
	body, _ = sjson.DeleteBytes(body, "type")
	body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
	body, _ = sjson.SetBytes(body, "stream", true)
	return body
}
