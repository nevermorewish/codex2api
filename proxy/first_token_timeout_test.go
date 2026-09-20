package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestFirstTokenTimeoutGuardCancelsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	guard := newFirstTokenTimeoutGuard(20*time.Millisecond, cancel)
	defer guard.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first token timeout guard did not cancel upstream context")
	}
	if !guard.TimedOut() {
		t.Fatal("guard TimedOut() = false, want true")
	}
}

func TestFirstTokenTimeoutGuardStopsOnFirstTokenEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
	defer guard.Stop()

	guard.MarkPayload([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))

	select {
	case <-ctx.Done():
		t.Fatal("first token timeout guard canceled after first token event")
	case <-time.After(80 * time.Millisecond):
	}
	if guard.TimedOut() {
		t.Fatal("guard TimedOut() = true, want false")
	}
}

func TestFirstTokenTimeoutGuardHooksRunWithoutUpstreamTimeout(t *testing.T) {
	var progress, stopped atomic.Bool
	guard := newFirstTokenTimeoutGuardWithHooks(0, func() {}, func() { progress.Store(true) }, func() { stopped.Store(true) })
	if guard == nil {
		t.Fatal("hook-only guard is nil")
	}
	guard.MarkPayload([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	if !progress.Load() || !stopped.Load() {
		t.Fatalf("hooks progress=%v stopped=%v, want both true", progress.Load(), stopped.Load())
	}
}

func TestFirstTokenTimeoutGuardIgnoresLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
	defer guard.Stop()

	// created / in_progress 不应解除看门狗：上游只发生命周期帧仍视为未开始响应。
	guard.MarkPayload([]byte(`{"type":"response.created"}`))
	guard.MarkPayload([]byte(`{"type":"response.in_progress"}`))

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("guard did not fire when only lifecycle frames arrived")
	}
	if !guard.TimedOut() {
		t.Fatal("guard TimedOut() = false, want true")
	}
}

func TestFirstTokenTimeoutGuardStructuralAndEmptyFramesStillTimeout(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","delta":""}`,
		`{"type":"response.reasoning_summary_text.delta","delta":""}`,
		`{"type":"response.function_call_arguments.delta","delta":""}`,
	} {
		t.Run(payload, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
			defer guard.Stop()
			guard.MarkPayload([]byte(payload))
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
				t.Fatal("content-free frame disabled first-token timeout")
			}
			if !guard.TimedOut() {
				t.Fatal("expected first-token timeout")
			}
		})
	}
}

func TestNormalizeRuntimeSettingsFirstTokenTimeout(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{FirstTokenTimeoutSec: -1})
	if settings.FirstTokenTimeoutSec != 0 {
		t.Fatalf("negative first token timeout normalized to %d, want 0", settings.FirstTokenTimeoutSec)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{FirstTokenTimeoutSec: 601})
	if settings.FirstTokenTimeoutSec != 600 {
		t.Fatalf("oversized first token timeout normalized to %d, want 600", settings.FirstTokenTimeoutSec)
	}
}

func TestNormalizeRuntimeSettingsFirstTokenMode(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{FirstTokenMode: "loose"})
	if settings.FirstTokenMode != FirstTokenModeLoose {
		t.Fatalf("FirstTokenMode = %q, want loose", settings.FirstTokenMode)
	}

	// 严格首字开关已取消：strict / 非法值 / 空值都归一化为 loose。
	for _, mode := range []string{"strict", "invalid", ""} {
		settings = NormalizeRuntimeSettings(RuntimeSettings{FirstTokenMode: mode})
		if settings.FirstTokenMode != FirstTokenModeLoose {
			t.Fatalf("FirstTokenMode %q normalized to %q, want loose", mode, settings.FirstTokenMode)
		}
	}
}

func TestNormalizeRuntimeSettingsCodexWSSilentRetries(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{CodexWSSilentRetries: -1})
	if settings.CodexWSSilentRetries != 0 {
		t.Fatalf("negative CodexWSSilentRetries normalized to %d, want 0", settings.CodexWSSilentRetries)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{CodexWSSilentRetries: -2})
	if settings.CodexWSSilentRetries != 0 {
		t.Fatalf("below-range CodexWSSilentRetries normalized to %d, want 0", settings.CodexWSSilentRetries)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{CodexWSSilentRetries: 99})
	if settings.CodexWSSilentRetries != 10 {
		t.Fatalf("oversized CodexWSSilentRetries normalized to %d, want 10", settings.CodexWSSilentRetries)
	}
}

func TestApplyRuntimeSettingsFromSystemFirstTokenTimeout(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		FirstTokenTimeoutSeconds: 42,
		FirstTokenMode:           FirstTokenModeLoose,
	})

	if settings.FirstTokenMode != FirstTokenModeLoose {
		t.Fatalf("FirstTokenMode = %q, want loose", settings.FirstTokenMode)
	}
	if settings.FirstTokenTimeoutSec != 42 {
		t.Fatalf("FirstTokenTimeoutSec = %d, want 42", settings.FirstTokenTimeoutSec)
	}
	if got := currentFirstTokenTimeout(); got != 42*time.Second {
		t.Fatalf("currentFirstTokenTimeout() = %s, want 42s", got)
	}
}

func TestApplyRuntimeSettingsFromSystemCodexWebSocketRetrySettings(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexWSHideUpstreamErrors: true,
		CodexWSSilentRetryEnabled: true,
		CodexWSSilentMaxRetries:   42,
		CodexWSWeakNetworkMode:    true,
	})

	if !settings.CodexWSHideErrors {
		t.Fatal("CodexWSHideErrors = false, want true")
	}
	if !settings.CodexWSSilentRetry {
		t.Fatal("CodexWSSilentRetry = false, want true")
	}
	if settings.CodexWSSilentRetries != 10 {
		t.Fatalf("CodexWSSilentRetries = %d, want 10", settings.CodexWSSilentRetries)
	}
	if !settings.CodexWSWeakNetworkMode {
		t.Fatal("CodexWSWeakNetworkMode = false, want true")
	}
}

func TestFirstTokenTimeoutForRequestExemptsCompaction(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())
	base := 30 * time.Second

	// 普通请求在 request_size 模式下按体积档位取值（默认档位表）。
	if got := firstTokenTimeoutForRequest(base, false, firstTokenTimeoutInput{BodySize: 10 * 1024}); got != 10*time.Second {
		t.Fatalf("non-compaction timeout = %s, want 10s", got)
	}
	// 压缩轮豁免看门狗（返回 0），与体积档位无关。
	if got := firstTokenTimeoutForRequest(base, true, firstTokenTimeoutInput{BodySize: 10 * 1024}); got != 0 {
		t.Fatalf("compaction timeout = %s, want 0", got)
	}
	// 看门狗本身遇到 0 阈值不启动，保证豁免生效。
	if guard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(base, true, firstTokenTimeoutInput{}), func() {}); guard != nil {
		t.Fatal("compaction round should not create a first token timeout guard")
	}
}

// 固定首 Token 模式忽略体积，直接使用 base——此时模型逐档表若命中仍然优先。
func TestFirstTokenTimeoutForRequestFixedMode(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.FirstTokenTimeoutMode = "first_token"
	ApplyRuntimeSettings(settings)
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	base := 77 * time.Second
	if got := firstTokenTimeoutForRequest(base, false, firstTokenTimeoutInput{BodySize: 900 * 1024}); got != base {
		t.Fatalf("first_token mode timeout = %s, want %s", got, base)
	}
}

// 关闭超时的模式对任何体积、任何模型都不设首字看门狗。
func TestFirstTokenTimeoutForRequestDisabledMode(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.FirstTokenTimeoutMode = "disabled"
	settings.FirstTokenSizeTimeouts.ModelTimeouts = map[string]database.FirstTokenModelTimeouts{
		"gpt-6-astra": {database.FirstTokenSizeOver500KB: 300},
	}
	ApplyRuntimeSettings(settings)
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	if got := firstTokenTimeoutForRequest(time.Minute, false, firstTokenTimeoutInput{Model: "gpt-6-astra", BodySize: 900 * 1024}); got != 0 {
		t.Fatalf("disabled mode timeout = %s, want 0", got)
	}
}

func TestFirstTokenTimeoutForRequestByBodySize(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.FirstTokenSizeTimeouts = database.FirstTokenTimeoutSettings{
		Under50KB: 11, Under100KB: 22, Under200KB: 33, Under500KB: 44, Over500KB: 55,
	}
	ApplyRuntimeSettings(settings)
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	cases := []struct {
		size int
		want time.Duration
	}{
		{1, 11 * time.Second}, {50*1024 - 1, 11 * time.Second},
		{50 * 1024, 22 * time.Second}, {100*1024 - 1, 22 * time.Second},
		{100 * 1024, 33 * time.Second}, {200*1024 - 1, 33 * time.Second},
		{200 * 1024, 44 * time.Second}, {500*1024 - 1, 44 * time.Second},
		{500 * 1024, 55 * time.Second}, {1 << 20, 55 * time.Second},
	}
	for _, tc := range cases {
		if got := firstTokenTimeoutForRequest(time.Minute, false, firstTokenTimeoutInput{BodySize: tc.size}); got != tc.want {
			t.Errorf("size %d: got %s, want %s", tc.size, got, tc.want)
		}
	}
}

// 模型逐档表优先于全局档位表，且必须逐档生效：同一个模型在不同体积下
// 拿到不同超时，未被覆盖的档位回落到全局值。
func TestFirstTokenTimeoutForRequestByModelBracket(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.FirstTokenSizeTimeouts = database.FirstTokenTimeoutSettings{
		Under50KB: 11, Under100KB: 22, Under200KB: 33, Under500KB: 44, Over500KB: 55,
		ModelTimeouts: map[string]database.FirstTokenModelTimeouts{
			"gpt-6-astra": {
				database.FirstTokenSizeUnder50KB:  90,
				database.FirstTokenSizeOver500KB:  300,
				database.FirstTokenSizeUnder200KB: 140,
			},
		},
	}
	ApplyRuntimeSettings(settings)
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	cases := []struct {
		size int
		want time.Duration
	}{
		{10 * 1024, 90 * time.Second},   // 模型值
		{60 * 1024, 22 * time.Second},   // 未覆盖，回落全局
		{150 * 1024, 140 * time.Second}, // 模型值
		{300 * 1024, 44 * time.Second},  // 未覆盖，回落全局
		{900 * 1024, 300 * time.Second}, // 模型值
	}
	for _, tc := range cases {
		if got := firstTokenTimeoutForRequest(time.Minute, false, firstTokenTimeoutInput{Model: "gpt-6-astra", BodySize: tc.size}); got != tc.want {
			t.Errorf("gpt-6-astra size %d: got %s, want %s", tc.size, got, tc.want)
		}
	}
	// 其它模型不受影响。
	if got := firstTokenTimeoutForRequest(time.Minute, false, firstTokenTimeoutInput{Model: "gpt-5.6-sol", BodySize: 900 * 1024}); got != 55*time.Second {
		t.Errorf("gpt-5.6-sol = %s, want 55s", got)
	}
}

// 模型查找键与配置键必须走同一套规范化，否则大小写/空白的差异会让配置静默失效。
func TestFirstTokenTimeoutForRequestModelKeyNormalization(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.FirstTokenSizeTimeouts = database.FirstTokenTimeoutSettings{
		Under50KB: 11, Under100KB: 22, Under200KB: 33, Under500KB: 44, Over500KB: 55,
		ModelTimeouts: map[string]database.FirstTokenModelTimeouts{
			"gpt-6-astra": {database.FirstTokenSizeUnder50KB: 90},
		},
	}
	ApplyRuntimeSettings(settings)
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	for _, model := range []string{"gpt-6-astra", "GPT-6-Astra", "  gpt-6-astra  "} {
		if got := firstTokenTimeoutForRequest(time.Minute, false, firstTokenTimeoutInput{Model: model, BodySize: 1024}); got != 90*time.Second {
			t.Errorf("model %q = %s, want 90s", model, got)
		}
	}
}

func TestNormalizeBillingTierPolicy(t *testing.T) {
	if got := NormalizeBillingTierPolicy(""); got != BillingTierPolicyActual {
		t.Fatalf("empty policy = %q, want actual", got)
	}
	if got := NormalizeBillingTierPolicy("requested"); got != BillingTierPolicyRequested {
		t.Fatalf("requested policy = %q, want requested", got)
	}
	if got := NormalizeBillingTierPolicy("invalid"); got != BillingTierPolicyActual {
		t.Fatalf("invalid policy = %q, want actual", got)
	}
}
