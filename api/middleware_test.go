package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCORSMiddlewareAllowsCodex2APIAffinityHeaderPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handlerCalled := false
	router := gin.New()
	router.Use(CORSMiddleware())
	router.OPTIONS("/v1/responses", func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/v1/responses", nil)
	request.Header.Set("Origin", "https://client.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "authorization, x-codex2api-affinity-key")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if handlerCalled {
		t.Fatal("OPTIONS preflight reached the downstream handler")
	}

	allowedHeaders := strings.ToLower(recorder.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(allowedHeaders, strings.ToLower("X-Codex2API-Affinity-Key")) {
		t.Fatalf("Access-Control-Allow-Headers = %q, missing X-Codex2API-Affinity-Key", recorder.Header().Get("Access-Control-Allow-Headers"))
	}
}

// A bare c.AbortWithStatus(503)/c.Status(503) — anywhere downstream, present or
// future — must never reach the client as an empty Content-Length: 0 body;
// EnsureErrorBodyMiddleware backfills the standard JSON error envelope.
func TestEnsureErrorBodyMiddlewareBackfillsBareStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(EnsureErrorBodyMiddleware())
	router.GET("/bare", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusServiceUnavailable)
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/bare", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("body is empty, want backfilled JSON error")
	}
	var resp ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not valid JSON: %v, body=%q", err, recorder.Body.String())
	}
	if resp.Error.Code != ErrCodeServiceUnavailable {
		t.Fatalf("error.code = %q, want %q", resp.Error.Code, ErrCodeServiceUnavailable)
	}
}

// A handler that already sent a proper JSON error body must not be double-written.
func TestEnsureErrorBodyMiddlewareLeavesExistingBodyAlone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(EnsureErrorBodyMiddleware())
	router.GET("/handled", func(c *gin.Context) {
		SendError(c, NewAPIError(ErrCodeServiceUnavailable, "custom message", ErrorTypeServer))
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/handled", nil))

	var resp ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not valid JSON: %v, body=%q", err, recorder.Body.String())
	}
	if resp.Error.Message != "custom message" {
		t.Fatalf("error.message = %q, want %q (middleware must not overwrite an existing body)", resp.Error.Message, "custom message")
	}
	if strings.Count(recorder.Body.String(), "custom message") != 1 {
		t.Fatalf("body written more than once: %q", recorder.Body.String())
	}
}

// 2xx/4xx responses, including ones with no body (e.g. 204), must pass through untouched.
func TestEnsureErrorBodyMiddlewareIgnoresNon5xx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(EnsureErrorBodyMiddleware())
	router.GET("/nocontent", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/nocontent", nil))

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty for 204", recorder.Body.String())
	}
}

