package proxy

import (
	"net/http"
	"sync"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func beginRequestLifecycle(c *gin.Context) func() {
	if c == nil || c.Request == nil || auth.HasRequestLifecycle(c.Request.Context()) {
		return func() {}
	}
	parent := c.Request
	id := resolveParentRequestID(c)
	if turn := c.GetString(liveStreamRequestContextKey); turn != "" {
		id = turn
	}
	ctx, finish := auth.BeginRequest(parent.Context(), id, parent.URL.Path, requestAPIKeyID(c))
	c.Request = parent.WithContext(ctx)
	var once sync.Once
	return func() {
		once.Do(func() {
			status := 0
			if c.Writer != nil && c.Writer.Status() >= 400 {
				status = c.Writer.Status()
			}
			if v := c.GetInt(AccessLogStatusContextKey); v >= 400 {
				status = v
			}
			if ctx.Err() != nil && status == 0 {
				status = 499
			}
			finish(status)
			c.Request = parent
		})
	}
}

func startRequestAttempt(c *gin.Context, account *auth.Account, attempt int) {
	if c == nil || c.Request == nil || account == nil {
		return
	}
	auth.RequestIdentity(c.Request.Context(), resolveParentRequestID(c), requestAPIKeyID(c))
	model := ""
	if raw, ok := c.Get("raw_body"); ok {
		if b, ok := raw.([]byte); ok {
			model = gjson.GetBytes(b, "model").String()
		}
	}
	auth.StartRequestAttempt(c.Request.Context(), account.ID(), account.IsExternalFallback(), attempt, model)
}

// Run before auth for POST generation requests. WebSocket connections are
// deliberately excluded: beginRelayRequest tracks each response.create turn.
func RequestLifecycleMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodPost {
			switch c.Request.URL.Path {
			case "/v1/responses", "/responses", "/backend-api/codex/responses",
				"/v1/chat/completions", "/chat/completions", "/v1/messages", "/messages",
				"/v1/responses/compact", "/responses/compact", "/backend-api/codex/responses/compact",
				"/v1/images/generations", "/images/generations", "/v1/images/edits", "/images/edits":
				defer beginRequestLifecycle(c)()
			}
		}
		c.Next()
	}
}
