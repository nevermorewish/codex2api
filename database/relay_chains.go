package database

import "context"

// Filter auxiliary rounds before counting attempts or applying pagination.
// Partition legacy rows by their own ID so unrelated requests stay separate.
const relayChainCTE = `WITH candidate_rows AS (
 SELECT id, COALESCE(NULLIF(TRIM(parent_request_id), ''), 'usage-' || CAST(id AS TEXT)) AS chain_id,
        COALESCE(attempt_index, 0) AS attempt_index, status_code,
        COALESCE(is_retry_attempt, false) AS is_retry_attempt,
        MAX(COALESCE(attempt_index, 0)) OVER (
          PARTITION BY COALESCE(NULLIF(TRIM(parent_request_id), ''), 'usage-' || CAST(id AS TEXT))
        ) AS max_attempt
 FROM usage_logs
 WHERE status_code <> 499 AND COALESCE(TRIM(internal_reason), '') = ''
), attempt_rows AS (
 SELECT * FROM candidate_rows WHERE max_attempt = 0 OR attempt_index > 0
), chains AS (
 SELECT chain_id, MAX(id) AS latest_id FROM attempt_rows
 GROUP BY chain_id
 HAVING COUNT(*) > 1 OR MAX(CASE WHEN status_code >= 200 AND status_code < 300
   AND NOT is_retry_attempt THEN 0 ELSE 1 END) > 0
)
`

// ListRelayChainLogs pages complete chains, not individual usage rows. Successful
// single attempts never consume a page slot, even under heavy successful traffic.
func (db *DB) ListRelayChainLogs(ctx context.Context, page, pageSize int) ([]*UsageLog, int, error) {
	var total int
	if err := db.conn.QueryRowContext(ctx, relayChainCTE+`SELECT COUNT(*) FROM chains`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx, relayChainCTE+`, page_chains AS (
 SELECT chain_id, latest_id FROM chains ORDER BY latest_id DESC LIMIT $1 OFFSET $2
)
 SELECT u.id, COALESCE(u.account_id, 0), COALESCE(a.name, ''),
        COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(u.fallback_account_name, ''),
        COALESCE(u.endpoint, ''), COALESCE(u.inbound_endpoint, ''), COALESCE(u.channel, ''),
        COALESCE(u.model, ''), COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''),
        u.status_code, u.duration_ms, r.is_retry_attempt, r.attempt_index,
        COALESCE(u.error_message, ''), COALESCE(u.upstream_error_kind, ''), r.chain_id, u.created_at
 FROM page_chains p
 JOIN attempt_rows r ON r.chain_id = p.chain_id
 JOIN usage_logs u ON u.id = r.id
 LEFT JOIN accounts a ON a.id = u.account_id
 ORDER BY p.latest_id DESC, u.id ASC`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	logs := make([]*UsageLog, 0)
	for rows.Next() {
		row := &UsageLog{}
		var credentials, createdAt any
		if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountName, &credentials,
			&row.FallbackAccountName, &row.Endpoint, &row.InboundEndpoint, &row.Channel,
			&row.Model, &row.APIKeyID, &row.APIKeyName, &row.StatusCode, &row.DurationMs,
			&row.IsRetryAttempt, &row.AttemptIndex, &row.ErrorMessage, &row.UpstreamErrorKind,
			&row.ParentRequestID, &createdAt); err != nil {
			return nil, 0, err
		}
		row.AccountEmail = accountEmailFromRawCredentials(credentials)
		row.CreatedAt, err = parseDBTimeValue(createdAt)
		if err != nil {
			return nil, 0, err
		}
		logs = append(logs, row)
	}
	return logs, total, rows.Err()
}
