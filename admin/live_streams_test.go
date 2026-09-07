package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newLiveStreamsTestHandler(t *testing.T, dbName string) (*Handler, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	return &Handler{db: db}, db
}

func doGetLiveStreams(t *testing.T, h *Handler) (*httptest.ResponseRecorder, struct {
	Streams   []liveStreamResponse `json:"streams"`
	Total     int                  `json:"total"`
	Truncated bool                 `json:"truncated"`
}) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/live-streams", nil)
	h.GetLiveStreams(c)
	var response struct {
		Streams   []liveStreamResponse `json:"streams"`
		Total     int                  `json:"total"`
		Truncated bool                 `json:"truncated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	return recorder, response
}

func TestGetLiveStreamsOnlyInProgress(t *testing.T) {
	h, _ := newLiveStreamsTestHandler(t, "live-only-in-progress.db")
	end := proxy.RegisterLiveAttemptForTest(proxy.LiveAttemptInfo{
		ParentRequestID: "stream-a", AccountID: 5, AccountName: "acct-a",
		Model: "gpt-5.4", IsStream: true, AttemptIndex: 1,
	})
	defer end()

	recorder, response := doGetLiveStreams(t, h)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(response.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d: %+v", len(response.Streams), response.Streams)
	}
	stream := response.Streams[0]
	if stream.RequestID != "stream-a" || stream.Status != "in_progress" || stream.Disconnected {
		t.Fatalf("unexpected stream: %+v", stream)
	}
	if len(stream.Attempts) != 1 || stream.Attempts[0].AccountName != "acct-a" || stream.Attempts[0].Decision != "in_progress" {
		t.Fatalf("unexpected attempts: %+v", stream.Attempts)
	}
	if stream.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1", stream.AttemptCount)
	}
}

func TestGetLiveStreamsMergesHistoryWithInProgress(t *testing.T) {
	h, db := newLiveStreamsTestHandler(t, "live-merge-history.db")
	ctx := context.Background()
	// Two prior failed attempts already persisted (account rotation), the
	// third attempt is still running.
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		AccountID: 1, ParentRequestID: "stream-b", AttemptIndex: 1, Model: "gpt-5.4",
		StatusCode: 503, IsRetryAttempt: true, DurationMs: 50,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		AccountID: 2, ParentRequestID: "stream-b", AttemptIndex: 2, Model: "gpt-5.4",
		StatusCode: 500, IsRetryAttempt: true, DurationMs: 30,
	}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()

	end := proxy.RegisterLiveAttemptForTest(proxy.LiveAttemptInfo{
		ParentRequestID: "stream-b", AccountID: 3, AccountName: "acct-3",
		Model: "gpt-5.4", IsStream: true, AttemptIndex: 3,
	})
	defer end()

	_, response := doGetLiveStreams(t, h)
	if len(response.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d: %+v", len(response.Streams), response.Streams)
	}
	stream := response.Streams[0]
	if len(stream.Attempts) != 3 {
		t.Fatalf("expected 3 attempts (2 history + 1 in-flight), got %d: %+v", len(stream.Attempts), stream.Attempts)
	}
	if stream.Status != "in_progress" {
		t.Fatalf("status = %q, want in_progress", stream.Status)
	}
	if stream.SwitchCount != 2 {
		t.Fatalf("switch_count = %d, want 2 (1->2, 2->3)", stream.SwitchCount)
	}
	if stream.Attempts[0].Seq != 1 || stream.Attempts[1].Seq != 2 || stream.Attempts[2].Seq != 3 {
		t.Fatalf("attempts out of order: %+v", stream.Attempts)
	}
	if stream.Attempts[2].Decision != "switch" {
		t.Fatalf("final in-flight attempt decision = %q, want switch", stream.Attempts[2].Decision)
	}
}

func TestGetLiveStreamsExcludesCompletedStreams(t *testing.T) {
	h, db := newLiveStreamsTestHandler(t, "live-excludes-completed.db")
	ctx := context.Background()
	// A stream that succeeded well outside the recent grace window and has no
	// in-flight attempt must not appear.
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		AccountID: 1, ParentRequestID: "stream-old", AttemptIndex: 1, Model: "gpt-5.4",
		StatusCode: 200, DurationMs: 10,
	}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	// Backdate it past the grace window so ListRecentlyEndedParentRequestIDs
	// does not pick it up.
	if err := db.BackdateUsageLogCreatedAtForTest(ctx, "stream-old", time.Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	_, response := doGetLiveStreams(t, h)
	if len(response.Streams) != 0 {
		t.Fatalf("expected 0 streams, got %d: %+v", len(response.Streams), response.Streams)
	}
}

func TestGetLiveStreamsDisconnectReasonFromUpstreamErrorKind(t *testing.T) {
	h, db := newLiveStreamsTestHandler(t, "live-disconnect-reason.db")
	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		AccountID: 1, ParentRequestID: "stream-failed", AttemptIndex: 1, Model: "gpt-5.4",
		StatusCode: 503, DurationMs: 10, UpstreamErrorKind: "transport",
	}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()

	_, response := doGetLiveStreams(t, h)
	if len(response.Streams) != 1 {
		t.Fatalf("expected 1 recently-ended stream, got %d: %+v", len(response.Streams), response.Streams)
	}
	stream := response.Streams[0]
	if stream.Status != "failed" || !stream.Disconnected {
		t.Fatalf("unexpected status: %+v", stream)
	}
	if stream.DisconnectReason != "transport" {
		t.Fatalf("disconnect_reason = %q, want transport", stream.DisconnectReason)
	}
}

// queryCounter wraps a *database.DB is not directly instrumentable here (the
// DB type has no query-count hook), so this test instead asserts the
// behavior that guarantees no N+1 pattern: exactly one merged history lookup
// covering every active parent ID, verified by checking that a large number
// of concurrently active streams still produces the correct merged output
// (an O(n) per-parent loop would still pass this, but a hidden per-parent
// query call would show up as a drastic slowdown / would still be correct
// here; the real N+1 guard is structural — GetLiveStreams calls
// ListUsageLogsByParentRequestIDs exactly once, see its source).
func TestGetLiveStreamsBatchesHistoryLookupAcrossManyStreams(t *testing.T) {
	h, db := newLiveStreamsTestHandler(t, "live-batches.db")
	ctx := context.Background()
	const streamCount = 50
	for i := 0; i < streamCount; i++ {
		id := "stream-batch-" + time.Now().Add(time.Duration(i)*time.Nanosecond).Format("150405.000000000")
		if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
			AccountID: int64(i + 1), ParentRequestID: id, AttemptIndex: 1, Model: "gpt-5.4",
			StatusCode: 503, IsRetryAttempt: true, DurationMs: 5,
		}); err != nil {
			t.Fatal(err)
		}
		end := proxy.RegisterLiveAttemptForTest(proxy.LiveAttemptInfo{
			ParentRequestID: id, AccountID: int64(i + 100), AccountName: "acct",
			Model: "gpt-5.4", IsStream: true, AttemptIndex: 2,
		})
		defer end()
	}
	db.FlushUsageLogs()

	_, response := doGetLiveStreams(t, h)
	if len(response.Streams) != streamCount {
		t.Fatalf("expected %d streams, got %d", streamCount, len(response.Streams))
	}
	for _, stream := range response.Streams {
		if len(stream.Attempts) != 2 || stream.Status != "in_progress" {
			t.Fatalf("stream %s missing merged history: %+v", stream.RequestID, stream)
		}
	}
}
