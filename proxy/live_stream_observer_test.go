package proxy

import (
	"testing"
	"time"
)

func TestLiveStreamObserverKeepsConnectionAcrossRequests(t *testing.T) {
	id := "observer-test-" + time.Now().UTC().Format("150405.000000000")
	finish := BeginLiveStream(id, "websocket", "gpt-5.4")
	requestOne := BeginLiveStreamRequest(id, id+":responses:0", 1)
	requestOne("success")
	requestTwo := BeginLiveStreamRequest(id, id+":responses:1", 2)
	if got, ok := LiveStreamSnapshot(id); !ok || got.State != LiveStreamOpen || len(got.Requests) != 2 {
		t.Fatalf("open stream snapshot = (%+v, %v), want two requests", got, ok)
	}
	requestTwo("canceled")
	finish("normal_close", "websocket")
	got, ok := LiveStreamSnapshot(id)
	if !ok || got.State != LiveStreamClosed || got.EndReason != "normal_close" || got.EndSource != "websocket" {
		t.Fatalf("closed stream snapshot = (%+v, %v)", got, ok)
	}
	if got.EndedAt.IsZero() || got.Requests[1].EndedAt.IsZero() || got.Requests[1].Outcome != "canceled" {
		t.Fatalf("close did not finish request: %+v", got)
	}
	// Closing is idempotent and must not overwrite the first terminal reason.
	finish("proxy_error", "gateway")
	got, _ = LiveStreamSnapshot(id)
	if got.EndReason != "normal_close" {
		t.Fatalf("terminal reason was overwritten: %+v", got)
	}
}

func TestLiveStreamObserverIgnoresUnknownRequestAfterClose(t *testing.T) {
	id := "observer-closed-" + time.Now().UTC().Format("150405.000000000")
	finish := BeginLiveStream(id, "sse", "model")
	finish("client_canceled", "downstream")
	end := BeginLiveStreamRequest(id, id+":late", 1)
	end("success")
	got, _ := LiveStreamSnapshot(id)
	if len(got.Requests) != 0 {
		t.Fatalf("request was added after close: %+v", got.Requests)
	}
}
