package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/codex2api/database"
)

var errRetryAttemptsExhausted = errors.New("retry attempts exhausted")

func deferUntilFirstToken(policy database.ContinuousRetryPolicy, contentSeen, terminal bool) bool {
	return policy.RequestPolicy != nil && policy.RequestPolicy.Mode == database.RetryModeBeforeFirstToken && !contentSeen && !terminal
}

func unifiedRetryBudgetAvailable(policy database.ContinuousRetryPolicy, general, rate *int) bool {
	if policy.RequestPolicy == nil {
		return true
	}
	used := 0
	if general != nil {
		used += *general
	}
	if rate != nil {
		used += *rate
	}
	return used < policy.RequestPolicy.AttemptLimit()-1
}

// Dispatch is the final shared gate: even repair/fallback branches must consume
// the same request budget. This gate never resets when the selected account does.
func claimUnifiedRetryAttempt(ctx context.Context) error {
	d := continuousRetryDeadlineForContext(ctx)
	if d == nil || d.maxAttempts == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fired || ctx.Err() != nil {
		return ErrUpstreamTimeout(errContinuousRetryDeadlineExceeded)
	}
	if d.attempts >= d.maxAttempts {
		return &Error{Type: ErrorTypeUpstreamError, Code: "retry_attempts_exhausted", Message: "Maximum upstream attempts reached", HTTPStatus: http.StatusBadGateway, Cause: errRetryAttemptsExhausted}
	}
	d.attempts++
	return nil
}
