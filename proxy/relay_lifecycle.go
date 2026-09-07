package proxy

import (
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const relayActivityContextKey = "relayRequestActivity"

// Only retain IDs for requests that have emitted an indexed attempt and are
// still running. Completion removes them; there is no history or background
// goroutine to leak. Old retry-only logs are incomplete, not forever active.
var relayActivities = struct {
	sync.Mutex
	active map[string]int
}{active: make(map[string]int)}

type relayRequestActivity struct {
	ids      map[string]struct{}
	finished bool
}

func beginRelayRequest(c *gin.Context) func() {
	if c == nil {
		return func() {}
	}
	previous, _ := c.Get(relayActivityContextKey)
	activity := &relayRequestActivity{}
	c.Set(relayActivityContextKey, activity)
	return func() {
		relayActivities.Lock()
		if !activity.finished {
			activity.finished = true
			for id := range activity.ids {
				relayActivities.active[id]--
				if relayActivities.active[id] <= 0 {
					delete(relayActivities.active, id)
				}
			}
		}
		relayActivities.Unlock()
		c.Set(relayActivityContextKey, previous)
	}
}

func observeRelayRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || input.AttemptIndex <= 0 || strings.TrimSpace(input.InternalReason) != "" {
		return
	}
	id := strings.TrimSpace(input.ParentRequestID)
	if id == "" {
		return
	}
	value, _ := c.Get(relayActivityContextKey)
	activity, _ := value.(*relayRequestActivity)
	if activity == nil {
		return
	}
	relayActivities.Lock()
	defer relayActivities.Unlock()
	if activity.finished {
		return
	}
	if activity.ids == nil {
		activity.ids = make(map[string]struct{})
	}
	if _, exists := activity.ids[id]; !exists {
		activity.ids[id] = struct{}{}
		relayActivities.active[id]++
	}
}

// RelayRequestInProgress is a process-local lifecycle check, not an inference
// from the last persisted error. It covers waiting, streaming and WS turns.
func RelayRequestInProgress(requestID string) bool {
	relayActivities.Lock()
	defer relayActivities.Unlock()
	return relayActivities.active[requestID] > 0
}
