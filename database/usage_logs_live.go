package database

import (
	"context"
	"strings"
	"time"
)

// usageLogsLiveBatchSize bounds each IN/ANY query so a burst of concurrently
// active streams (which drives the parentRequestID list this feeds) never
// produces an unbounded parameter list. Mirrors the batching used elsewhere
// for bulk ID lookups (see data_migrations.go).
const usageLogsLiveBatchSize = 500

// ListUsageLogsByParentRequestIDs returns every usage_logs row whose
// parent_request_id matches one of ids. It is a point lookup keyed on
// parent_request_id (see the idx_usage_logs_parent_request_id index added
// alongside this function) intended for a small, bounded set of currently
// active streams — never a full-table scan, and never paged like
// ListRelayChainLogs. Rows for a given parent request ID are returned in no
// particular cross-batch order; callers that need chronological order
// within one stream should sort by AttemptIndex/CreatedAt themselves.
func (db *DB) ListUsageLogsByParentRequestIDs(ctx context.Context, ids []string) ([]*UsageLog, error) {
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			cleaned = append(cleaned, id)
		}
	}
	if len(cleaned) == 0 {
		return nil, nil
	}

	logs := make([]*UsageLog, 0, len(cleaned))
	for start := 0; start < len(cleaned); start += usageLogsLiveBatchSize {
		end := start + usageLogsLiveBatchSize
		if end > len(cleaned) {
			end = len(cleaned)
		}
		batch, err := db.listUsageLogsByParentRequestIDsBatch(ctx, cleaned[start:end])
		if err != nil {
			return nil, err
		}
		logs = append(logs, batch...)
	}
	return logs, nil
}

func (db *DB) listUsageLogsByParentRequestIDsBatch(ctx context.Context, ids []string) ([]*UsageLog, error) {
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := `
		SELECT u.id, COALESCE(u.account_id, 0), COALESCE(a.name, ''),
		       COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(u.fallback_account_name, ''),
		       COALESCE(u.endpoint, ''), COALESCE(u.inbound_endpoint, ''), COALESCE(u.channel, ''),
		       COALESCE(u.model, ''), COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''),
		       u.status_code, u.duration_ms, COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0),
		       COALESCE(u.error_message, ''), COALESCE(u.upstream_error_kind, ''),
		       COALESCE(NULLIF(TRIM(u.parent_request_id), ''), 'usage-' || CAST(u.id AS TEXT)),
		       u.created_at, COALESCE(u.fallback_reason, '')
		FROM usage_logs u
		LEFT JOIN accounts a ON a.id = u.account_id
		WHERE COALESCE(TRIM(u.internal_reason), '') = ''
		  AND NULLIF(TRIM(u.parent_request_id), '') IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY u.id ASC`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]*UsageLog, 0, len(ids))
	for rows.Next() {
		row := &UsageLog{}
		var credentials, createdAt any
		if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountName, &credentials,
			&row.FallbackAccountName, &row.Endpoint, &row.InboundEndpoint, &row.Channel,
			&row.Model, &row.APIKeyID, &row.APIKeyName, &row.StatusCode, &row.DurationMs,
			&row.IsRetryAttempt, &row.AttemptIndex, &row.ErrorMessage, &row.UpstreamErrorKind,
			&row.ParentRequestID, &createdAt, &row.FallbackReason); err != nil {
			return nil, err
		}
		row.AccountEmail = accountEmailFromRawCredentials(credentials)
		if row.CreatedAt, err = parseDBTimeValue(createdAt); err != nil {
			return nil, err
		}
		logs = append(logs, row)
	}
	return logs, rows.Err()
}

// ListRecentlyEndedParentRequestIDs returns the distinct parent_request_id
// values of every usage_logs row created at or after since. It backs the
// live streams admin endpoint's short "just disconnected" grace window: a
// stream leaves the in-memory in-flight registry (proxy.LiveAttemptsSnapshot)
// the instant its final attempt is recorded, so without this lookup no poll
// would ever observe the terminal failure reason. The query relies on the
// existing idx_usage_logs_created_at / idx_usage_logs_created_status indexes
// and is expected to scan a tiny, recent slice of the table, not the whole
// history — callers should keep the window short (tens of seconds).
func (db *DB) ListRecentlyEndedParentRequestIDs(ctx context.Context, since time.Time) ([]string, error) {
	placeholder := dbPlaceholders(db.isSQLite(), 1, 1)[0]
	rows, err := db.conn.QueryContext(ctx, `
		SELECT DISTINCT parent_request_id
		FROM usage_logs
		WHERE created_at >= `+placeholder+`
		  AND NULLIF(TRIM(parent_request_id), '') IS NOT NULL
		  AND COALESCE(TRIM(internal_reason), '') = ''`, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, 64)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// BackdateUsageLogCreatedAtForTest overwrites created_at for every row
// matching parentRequestID. It exists solely so other packages' tests (for
// example admin/live_streams_test.go) can push a row outside
// ListRecentlyEndedParentRequestIDs's grace window without reaching into
// this package's unexported connection field. Production code must never
// call this.
func (db *DB) BackdateUsageLogCreatedAtForTest(ctx context.Context, parentRequestID string, at time.Time) error {
	placeholders := dbPlaceholders(db.isSQLite(), 1, 2)
	_, err := db.conn.ExecContext(ctx,
		`UPDATE usage_logs SET created_at = `+placeholders[0]+` WHERE parent_request_id = `+placeholders[1],
		db.timeArg(at), parentRequestID)
	return err
}
