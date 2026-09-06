package proxy

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestStreamFailureRecyclesOnlyBrokenTransport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outcome     streamOutcome
		wantRecycle bool
	}{
		{"overload", classifyResponseFailedOutcome([]byte(`{"response":{"error":{"code":"server_is_overloaded"}}}`)), false},
		{"server_error", classifyResponseFailedOutcome([]byte(`{"response":{"error":{"code":"server_error"}}}`)), false},
		{"eof", classifyStreamOutcome(nil, io.ErrUnexpectedEOF, nil, false), true},
		{"missing_terminal", classifyStreamOutcome(nil, nil, nil, false), true},
		{"downstream_write", classifyStreamOutcome(nil, nil, errors.New("broken pipe"), false), false},
		{"local_replay", streamOutcome{terminalLocal: true, logStatusCode: logStatusUpstreamStreamBreak}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Account{DBID: 99001, AccessToken: "recycle-test-" + tc.name}
			key := clientPoolKey(a, "", codexTransportModeFromEnv())
			entry := &poolEntry{client: &http.Client{Transport: &http.Transport{}}}
			previous, loaded := clientPool.Load(key)
			clientPool.Store(key, entry)
			t.Cleanup(func() {
				clientPool.Delete(key)
				if loaded {
					clientPool.Store(key, previous)
				}
			})
			recycleStreamClientIfBroken(a, "", tc.outcome)
			_, present := clientPool.Load(key)
			if present == tc.wantRecycle {
				t.Fatalf("client present=%v, want recycle=%v", present, tc.wantRecycle)
			}
		})
	}
}

func TestPreContentRetrySeparatesProviderErrorsFromDisconnects(t *testing.T) {
	overloaded := classifyResponseFailedOutcome([]byte(`{"response":{"error":{"code":"server_is_overloaded"}}}`))
	if reason := preContentRetryReason(overloaded); reason != "upstream_overloaded" {
		t.Fatalf("overload mislabeled as disconnect: %s", reason)
	}
	if reason := preContentRetryReason(classifyStreamOutcome(nil, io.EOF, nil, false)); reason != "upstream_stream_break" {
		t.Fatalf("EOF reason=%s", reason)
	}
}
