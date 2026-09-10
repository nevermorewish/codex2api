package auth

import (
	"testing"
	"time"
)

// TestTransportFailureNeverDropsTier 验证传输层断流不再削账号并发。
//
// 断流来自上游边缘重置或链路抖动，与账号自身健康无关；即便连续多次，也只留痕
// 供运营查看，不参与档位与并发判定（#491 之后的进一步收紧）。
func TestTransportFailureNeverDropsTier(t *testing.T) {
	s := &Store{}
	acc := &Account{HealthTier: HealthTierHealthy}

	for i := 1; i <= transportFailureTierDropStreak+2; i++ {
		s.ReportRequestFailure(acc, transportFailureKind, 10*time.Millisecond)
		if acc.HealthTier != HealthTierHealthy {
			t.Fatalf("transport failure #%d dropped tier to %s", i, acc.HealthTier)
		}
	}
	if !acc.LastFailureAt.After(acc.LastSuccessAt) || acc.LastFailureKind != transportFailureKind {
		t.Fatalf("failure attribution not recorded: kind=%q", acc.LastFailureKind)
	}

	// 成功清零连击，豁免判据也随之恢复。
	s.ReportRequestSuccess(acc, 10*time.Millisecond)
	if acc.FailureStreak != 0 {
		t.Fatalf("success must reset failure streak, got %d", acc.FailureStreak)
	}
	s.ReportRequestFailure(acc, transportFailureKind, 10*time.Millisecond)
	if !acc.isolatedTransportFailureLocked() {
		t.Fatal("post-success single transport failure must be treated as isolated again")
	}
}

// TestUpstreamFailuresDoNotDropTier 验证 server/timeout 这类上游故障不再降档，
// 只有 unauthorized 这种凭证维度的硬状态才会封禁账号。
func TestUpstreamFailuresDoNotDropTier(t *testing.T) {
	s := &Store{}
	for _, kind := range []string{"server", "timeout"} {
		acc := &Account{HealthTier: HealthTierHealthy}
		s.ReportRequestFailure(acc, kind, 10*time.Millisecond)
		if acc.HealthTier != HealthTierHealthy {
			t.Fatalf("%s failure must not drop tier, got %s", kind, acc.HealthTier)
		}
		if acc.isolatedTransportFailureLocked() {
			t.Fatalf("%s failure must not qualify for the transport exemption", kind)
		}
	}

	// 时间戳仍然留痕，运营页要能看出最近发生过什么。
	timedOut := &Account{HealthTier: HealthTierHealthy}
	s.ReportRequestFailure(timedOut, "timeout", 10*time.Millisecond)
	if timedOut.LastTimeoutAt.IsZero() {
		t.Fatal("timeout failure must still stamp LastTimeoutAt for operator visibility")
	}
	serverErr := &Account{HealthTier: HealthTierHealthy}
	s.ReportRequestFailure(serverErr, "server", 10*time.Millisecond)
	if serverErr.LastServerErrorAt.IsZero() {
		t.Fatal("server failure must still stamp LastServerErrorAt for operator visibility")
	}

	banned := &Account{HealthTier: HealthTierHealthy}
	s.ReportRequestFailure(banned, "unauthorized", 10*time.Millisecond)
	if banned.HealthTier != HealthTierBanned {
		t.Fatalf("unauthorized must still ban, got %s", banned.HealthTier)
	}
}
