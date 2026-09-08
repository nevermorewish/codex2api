package proxy

import (
	"strings"
	"sync"
	"time"
)

const liveStreamRequestContextKey = "liveStreamRequestID"

// LiveStreamState is deliberately independent from retry/account state.  A
// stream may stay open while there is no upstream attempt in flight (for
// example between two Responses WebSocket turns).
type LiveStreamState string

const (
	LiveStreamOpen    LiveStreamState = "open"
	LiveStreamClosed  LiveStreamState = "closed"
	LiveStreamUnknown LiveStreamState = "unknown"
)

type LiveStreamRequestInfo struct {
	ID        string
	Seq       int
	StartedAt time.Time
	EndedAt   time.Time
	Outcome   string
}

type LiveStreamInfo struct {
	ID             string
	Protocol       string
	Model          string
	OpenedAt       time.Time
	LastActivityAt time.Time
	EndedAt        time.Time
	State          LiveStreamState
	EndReason      string
	EndSource      string
	Requests       []LiveStreamRequestInfo
	DataComplete   bool
}

type liveStreamRequest struct {
	id, outcome string
	seq         int
	startedAt   time.Time
	endedAt     time.Time
}

type liveStreamSession struct {
	id, protocol, model             string
	openedAt, lastActivity, endedAt time.Time
	state                           LiveStreamState
	endReason, endSource            string
	requests                        []*liveStreamRequest
	requestByID                     map[string]*liveStreamRequest
}

var liveStreamObserver = struct {
	sync.RWMutex
	byID map[string]*liveStreamSession
}{byID: make(map[string]*liveStreamSession)}

const maxObservedLiveStreams = 4096

func pruneObservedLiveStreamsLocked() {
	if len(liveStreamObserver.byID) <= maxObservedLiveStreams {
		return
	}
	for len(liveStreamObserver.byID) > maxObservedLiveStreams {
		var oldestID string
		var oldest time.Time
		for id, stream := range liveStreamObserver.byID {
			if stream.state == LiveStreamOpen {
				continue
			}
			if oldestID == "" || stream.endedAt.Before(oldest) {
				oldestID, oldest = id, stream.endedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(liveStreamObserver.byID, oldestID)
	}
}

// BeginLiveStream records a client connection/stream.  It is safe to call
// more than once for the same ID; the first open event owns the start time.
// The returned function closes the stream with an explicit reason.
func BeginLiveStream(id, protocol, model string) func(reason, source string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return func(string, string) {}
	}
	now := time.Now()
	liveStreamObserver.Lock()
	s := liveStreamObserver.byID[id]
	if s == nil || s.state != LiveStreamOpen {
		s = &liveStreamSession{id: id, protocol: strings.TrimSpace(protocol), model: strings.TrimSpace(model), openedAt: now, lastActivity: now, state: LiveStreamOpen, requestByID: make(map[string]*liveStreamRequest)}
		liveStreamObserver.byID[id] = s
		pruneObservedLiveStreamsLocked()
	} else {
		if s.protocol == "" {
			s.protocol = strings.TrimSpace(protocol)
		}
		if s.model == "" {
			s.model = strings.TrimSpace(model)
		}
	}
	liveStreamObserver.Unlock()
	var once sync.Once
	return func(reason, source string) {
		once.Do(func() { FinishLiveStream(id, reason, source) })
	}
}

// BeginLiveStreamRequest marks one logical request within an open stream.
// It is idempotent for requestID, which prevents a retry from inflating the
// request count.
func BeginLiveStreamRequest(streamID, requestID string, seq int) func(outcome string) {
	streamID, requestID = strings.TrimSpace(streamID), strings.TrimSpace(requestID)
	if streamID == "" {
		return func(string) {}
	}
	now := time.Now()
	liveStreamObserver.Lock()
	s := liveStreamObserver.byID[streamID]
	if s == nil || s.state != LiveStreamOpen {
		liveStreamObserver.Unlock()
		return func(string) {}
	}
	if requestID == "" {
		requestID = streamID + ":request-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	r := s.requestByID[requestID]
	if r == nil {
		if seq <= 0 {
			seq = len(s.requests) + 1
		}
		r = &liveStreamRequest{id: requestID, seq: seq, startedAt: now}
		s.requests = append(s.requests, r)
		s.requestByID[requestID] = r
	}
	s.lastActivity = now
	liveStreamObserver.Unlock()
	var once sync.Once
	return func(outcome string) {
		once.Do(func() {
			liveStreamObserver.Lock()
			if r.endedAt.IsZero() {
				r.endedAt = time.Now()
				r.outcome = strings.TrimSpace(outcome)
			}
			if s.state == LiveStreamOpen {
				s.lastActivity = time.Now()
			}
			liveStreamObserver.Unlock()
		})
	}
}

func FinishLiveStream(id, reason, source string) {
	liveStreamObserver.Lock()
	defer liveStreamObserver.Unlock()
	s := liveStreamObserver.byID[strings.TrimSpace(id)]
	if s == nil || s.state != LiveStreamOpen {
		return
	}
	now := time.Now()
	s.state, s.endedAt, s.lastActivity = LiveStreamClosed, now, now
	s.endReason, s.endSource = strings.TrimSpace(reason), strings.TrimSpace(source)
	for _, r := range s.requests {
		if r.endedAt.IsZero() {
			r.endedAt, r.outcome = now, "canceled"
		}
	}
}

func LiveStreamsSnapshot() []LiveStreamInfo {
	liveStreamObserver.RLock()
	defer liveStreamObserver.RUnlock()
	out := make([]LiveStreamInfo, 0, len(liveStreamObserver.byID))
	for _, s := range liveStreamObserver.byID {
		item := LiveStreamInfo{ID: s.id, Protocol: s.protocol, Model: s.model, OpenedAt: s.openedAt, LastActivityAt: s.lastActivity, EndedAt: s.endedAt, State: s.state, EndReason: s.endReason, EndSource: s.endSource, DataComplete: true}
		item.Requests = make([]LiveStreamRequestInfo, 0, len(s.requests))
		for _, r := range s.requests {
			item.Requests = append(item.Requests, LiveStreamRequestInfo{ID: r.id, Seq: r.seq, StartedAt: r.startedAt, EndedAt: r.endedAt, Outcome: r.outcome})
		}
		out = append(out, item)
	}
	return out
}

func LiveStreamSnapshot(id string) (LiveStreamInfo, bool) {
	for _, item := range LiveStreamsSnapshot() {
		if item.ID == strings.TrimSpace(id) {
			return item, true
		}
	}
	return LiveStreamInfo{}, false
}
