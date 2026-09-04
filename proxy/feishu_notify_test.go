package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestFeishuFirstTokenWatchSendsOnTimeout(t *testing.T) {
	previous := CurrentRuntimeSettings()
	defer ApplyRuntimeSettings(previous)
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.FeishuConfig = EncodeFeishuAlertConfig(FeishuAlertConfig{
			Enabled: true, AppID: "cli_test", AppSecret: "secret", ChatIDs: "oc_test", FirstTokenTimeoutSeconds: 1,
		})
		return current
	})

	previousTransport := feishuHTTPClient.Transport
	defer func() { feishuHTTPClient.Transport = previousTransport }()
	feishuTokens.Lock()
	feishuTokens.token, feishuTokens.appID, feishuTokens.expiresAt = "", "", time.Time{}
	feishuTokens.Unlock()
	var messages atomic.Int32
	feishuHTTPClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"tenant","expire":3600}`)), Header: make(http.Header)}, nil
		}
		messages.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":0}`)), Header: make(http.Header)}, nil
	})

	watch := newFeishuFirstTokenWatch(context.Background(), database.UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.4", Stream: true}, 10*time.Millisecond)
	if watch == nil {
		t.Fatal("newFeishuFirstTokenWatch returned nil")
	}
	defer watch.Stop()
	deadline := time.Now().Add(time.Second)
	for messages.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := messages.Load(); got != 1 {
		t.Fatalf("message requests = %d, want 1", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFeishuAlertConfigDefaultsAndNormalization(t *testing.T) {
	cfg := ParseFeishuAlertConfig(`{"enabled":true,"app_id":" cli_demo ","error_codes":"503, http_503; SERVICE_UNAVAILABLE,503"}`)
	if cfg.FirstTokenTimeoutSeconds != 30 {
		t.Fatalf("default first token timeout = %d, want 30", cfg.FirstTokenTimeoutSeconds)
	}
	if cfg.AppID != "cli_demo" || cfg.ErrorCodes != "503,http_503,service_unavailable" {
		t.Fatalf("normalized config = %+v", cfg)
	}
}

func TestFeishuErrorCodeMatchesStatusKindAndMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		in   database.UsageLogInput
		want bool
	}{
		{"status", "503", database.UsageLogInput{StatusCode: 503}, true},
		{"status range", "500-599", database.UsageLogInput{StatusCode: 503}, true},
		{"status wildcard", "http_5xx", database.UsageLogInput{StatusCode: 502}, true},
		{"http status", "http_429", database.UsageLogInput{StatusCode: 429}, true},
		{"kind", "service_unavailable", database.UsageLogInput{UpstreamErrorKind: "service-unavailable"}, true},
		{"message", "token_expired", database.UsageLogInput{ErrorMessage: "upstream token_expired"}, true},
		{"no match", "503", database.UsageLogInput{StatusCode: 200}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FeishuErrorCodeMatches(tc.cfg, &tc.in); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}
