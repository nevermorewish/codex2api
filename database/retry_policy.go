package database

import "fmt"

const (
	RetryModeOff              = "off"
	RetryModeBeforeFirstToken = "before_first_token"
	RetryModeFullResponse     = "full_response"
)

// RequestRetryPolicy is the sole operator-facing budget and delivery policy.
// The legacy columns remain readable for migration, not as additional budgets.
type RequestRetryPolicy struct {
	Mode                string `json:"mode"`
	MaxAttempts         int    `json:"max_attempts"`
	TotalTimeoutSeconds int    `json:"total_timeout_seconds"`
}

func (p RequestRetryPolicy) Validate() error {
	if p.Mode != RetryModeOff && p.Mode != RetryModeBeforeFirstToken && p.Mode != RetryModeFullResponse {
		return fmt.Errorf("retry_policy.mode must be off, before_first_token or full_response")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 20 {
		return fmt.Errorf("retry_policy.max_attempts must be 1..20 (including the first attempt)")
	}
	if p.TotalTimeoutSeconds < 1 || p.TotalTimeoutSeconds > 900 {
		return fmt.Errorf("retry_policy.total_timeout_seconds must be 1..900")
	}
	return nil
}

func (p RequestRetryPolicy) AttemptLimit() int {
	if p.Mode == RetryModeOff {
		return 1
	}
	return p.MaxAttempts
}

// ResolveRequestRetryPolicy performs a deterministic, non-destructive migration.
// In particular, existing buffered delivery remains buffered until changed.
func ResolveRequestRetryPolicy(old ContinuousRetryPolicy, generalRetries, rateLimitRetries int) RequestRetryPolicy {
	if old.RequestPolicy != nil && old.RequestPolicy.Validate() == nil {
		return *old.RequestPolicy
	}
	p := RequestRetryPolicy{Mode: RetryModeBeforeFirstToken, MaxAttempts: max(generalRetries, rateLimitRetries) + 1, TotalTimeoutSeconds: 300}
	if old.Enabled {
		p.Mode = RetryModeFullResponse
		p.TotalTimeoutSeconds = NormalizeContinuousRetryPolicy(old).MaxDurationSeconds
	} else if generalRetries == 0 && rateLimitRetries == 0 {
		p.Mode = RetryModeOff
	}
	p.MaxAttempts = min(20, max(1, p.MaxAttempts))
	return p
}
