package proxy

import (
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// A response.failed/error frame is a provider verdict, not evidence of a
// broken HTTP transport. Evict only for actual stream read/timeout failures;
// the caller still closes the attempt body and releases its account lease.
func shouldRecycleStreamClient(outcome streamOutcome) bool {
	return !outcome.terminalLocal && !outcome.capacityShed &&
		len(outcome.failurePayload) == 0 && outcome.logStatusCode == logStatusUpstreamStreamBreak
}

func recycleStreamClientIfBroken(account *auth.Account, proxyURL string, outcome streamOutcome) {
	if shouldRecycleStreamClient(outcome) {
		recyclePooledClient(account, proxyURL)
	}
}

func preContentRetryReason(outcome streamOutcome) string {
	switch {
	case outcome.capacityShed:
		return "upstream_overloaded"
	case len(outcome.failurePayload) > 0:
		return "upstream_error_frame"
	case isFirstTokenTimeoutOutcome(outcome):
		return "first_response_timeout"
	default:
		return "upstream_stream_break"
	}
}

// Avoid selecting the same healthy-looking account immediately after a
// pre-content failure. Transient failures only exclude it for this pool pass:
// a single-account installation can still recover within its finite budget.
func (r *retryAccountExclusions) markPreContentStreamFailure(accountID int64, outcome streamOutcome, generalLimit, rateLimit int, policy database.ContinuousRetryPolicy) {
	if retryLimitForStreamOutcome(outcome, generalLimit, rateLimit, policy) != -1 &&
		(outcome.logStatusCode == logStatusUpstreamStreamBreak || isTransientRetryHTTPFailure(outcome.logStatusCode, outcome.failurePayload)) {
		r.MarkSoft(accountID)
		return
	}
	r.MarkStreamFailure(accountID, outcome, generalLimit, rateLimit, policy)
}
