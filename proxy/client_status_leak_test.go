package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 回归：内部日志状态码绝不能作为 HTTP 状态码下发给客户端。
//
// 生产现象（2026-09-10）：客户端访问日志里出现 `POST /v1/responses 598`。
// 598 是服务内部的 logStatusUpstreamStreamBreak，不是合法 HTTP 状态码；
// 一旦被 c.JSON 直接写回，客户端会收到标准里不存在的状态码并解析失败。
func TestClientFacingHTTPStatusNeverLeaksInternalCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   int
	}{
		{"upstream stream break 598", logStatusUpstreamStreamBreak, http.StatusBadGateway},
		{"client closed 499", logStatusClientClosed, http.StatusBadGateway},
		{"zero", 0, http.StatusBadGateway},
		{"sub-400", http.StatusContinue, http.StatusBadGateway},
		{"above 599", 600, http.StatusBadGateway},
		{"real 500 passes", http.StatusInternalServerError, http.StatusInternalServerError},
		{"real 502 passes", http.StatusBadGateway, http.StatusBadGateway},
		{"real 429 passes", http.StatusTooManyRequests, http.StatusTooManyRequests},
		{"real 400 passes", http.StatusBadRequest, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientFacingHTTPStatus(tc.status); got != tc.want {
				t.Fatalf("clientFacingHTTPStatus(%d) = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// 回归：内部状态码被记住后经 deadline 的「最后失败」通道下发时，同样必须归一化。
// rememberContinuousRetryStreamFailure 把 598 原样写进 failure.status，这条路径
// 过去只做 400..599 范围判断，会把 598 当成合法状态码发给客户端。
func TestContinuousRetryLastFailureNormalizesInternalStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	writeContinuousRetryLastFailure(c, continuousRetryProtocolOpenAI, continuousRetryFailure{
		status: logStatusUpstreamStreamBreak,
		body:   []byte(`{"error":{"message":"upstream stream ended","type":"upstream_error"}}`),
	})

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (598 must not reach the client)", recorder.Code, http.StatusBadGateway)
	}
	if code := recorder.Code; code == logStatusUpstreamStreamBreak {
		t.Fatal("internal 598 leaked to the client")
	}
}

// 回归：兜底账号自称成功却既无 usage 又无内容时，必须判为失败。
//
// 生产现象（2026-09-10）：主账号池被上游过载打穿后请求切入兜底号池，
// 上游返回 200 + response.completed，但既无 usage 也无 output；客户端拿到
// 200 空响应，下游计费方只能打出「上游没有返回计费信息，无法扣费」。
func TestFallbackSucceededWithoutUsageDetection(t *testing.T) {
	fallback := &auth.Account{ExternalFallback: true}
	primary := &auth.Account{DBID: 7}

	t.Run("fallback, ok status, no usage, no content => pseudo success", func(t *testing.T) {
		if !fallbackSucceededWithoutUsage(fallback, streamOutcome{logStatusCode: http.StatusOK}, nil, false) {
			t.Fatal("want pseudo-success detected for fallback with no usage and no content")
		}
	})
	t.Run("fallback, ok status, zero-valued usage => pseudo success", func(t *testing.T) {
		if !fallbackSucceededWithoutUsage(fallback, streamOutcome{logStatusCode: http.StatusOK}, &UsageInfo{}, false) {
			t.Fatal("want pseudo-success detected for all-zero usage")
		}
	})
	t.Run("fallback with real usage is a success", func(t *testing.T) {
		usage := &UsageInfo{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}
		if fallbackSucceededWithoutUsage(fallback, streamOutcome{logStatusCode: http.StatusOK}, usage, false) {
			t.Fatal("real usage must not be treated as pseudo success")
		}
	})
	t.Run("fallback with content but missing usage stays a success", func(t *testing.T) {
		if fallbackSucceededWithoutUsage(fallback, streamOutcome{logStatusCode: http.StatusOK}, nil, true) {
			t.Fatal("delivered content must not be reclassified: upstream merely omitted usage")
		}
	})
	t.Run("primary account is never reclassified", func(t *testing.T) {
		if fallbackSucceededWithoutUsage(primary, streamOutcome{logStatusCode: http.StatusOK}, nil, false) {
			t.Fatal("primary accounts must keep the existing behaviour")
		}
	})
	t.Run("already-failed outcome is untouched", func(t *testing.T) {
		if fallbackSucceededWithoutUsage(fallback, streamOutcome{logStatusCode: http.StatusInternalServerError}, nil, false) {
			t.Fatal("non-200 outcomes are already failures")
		}
	})
}

func TestEmptyFallbackSuccessOutcomeIsRetryableUpstreamError(t *testing.T) {
	outcome := emptyFallbackSuccessOutcome()
	if outcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadGateway)
	}
	if outcome.failureKind != "usage_missing" {
		t.Fatalf("failureKind = %q, want usage_missing", outcome.failureKind)
	}
	if strings.TrimSpace(outcome.failureMessage) == "" {
		t.Fatal("failure message must be present so downstream can log/retry")
	}
	if outcome.terminalLocal {
		t.Fatal("a broken fallback upstream is not a local failure")
	}
}

func TestResponsesDeliveredContentSignals(t *testing.T) {
	cases := []struct {
		name                           string
		delta, completed, respJSON, im int
		want                           bool
	}{
		{"nothing at all", 0, 0, 0, 0, false},
		{"streamed deltas", 12, 0, 0, 0, true},
		{"completed response kept", 0, 256, 0, 0, true},
		{"buffered non-stream response", 0, 0, 512, 0, true},
		{"image output", 0, 0, 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesDeliveredContent(tc.delta, tc.completed, tc.respJSON, tc.im); got != tc.want {
				t.Fatalf("responsesDeliveredContent(%d,%d,%d,%d) = %v, want %v",
					tc.delta, tc.completed, tc.respJSON, tc.im, got, tc.want)
			}
		})
	}
}

// 端到端：兜底账号交付零内容成功时，/v1/responses 非流式路径不得以 200 结束。
func TestFallbackZeroContentSuccessIsNotDeliveredAs200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		account := &auth.Account{ExternalFallback: true}
		outcome := streamOutcome{logStatusCode: http.StatusOK}
		usage := (*UsageInfo)(nil)
		if fallbackSucceededWithoutUsage(account, outcome, usage, responsesDeliveredContent(0, 0, 0, 0)) {
			outcome = emptyFallbackSuccessOutcome()
		}
		c.JSON(clientFacingHTTPStatus(outcome.logStatusCode), gin.H{
			"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
		})
	})

	server := httptest.NewServer(router)
	defer server.Close()
	resp, err := server.Client().Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %q (%v)", body, err)
	}
	if payload.Error.Message == "" {
		t.Fatalf("expected an explanatory error message, got %q", body)
	}
}

var _ = database.ContinuousRetryPolicy{}
