package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/codex2api/security/riskcontrol"
	"strings"
	"time"
)

func (db *DB) ensureRiskControlSchema(ctx context.Context) error {
	// TEXT payloads and epoch timestamps deliberately use the same schema on both engines.
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS risk_control_config (id INTEGER PRIMARY KEY, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS risk_control_logs (id TEXT PRIMARY KEY, created_at BIGINT NOT NULL, api_key_id BIGINT NOT NULL, action TEXT NOT NULL, flagged BOOLEAN NOT NULL, counted BOOLEAN NOT NULL, blocked BOOLEAN NOT NULL, payload TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS risk_control_logs_key_time ON risk_control_logs(api_key_id,created_at)`,
		`CREATE INDEX IF NOT EXISTS risk_control_logs_time ON risk_control_logs(created_at)`,
		`CREATE TABLE IF NOT EXISTS risk_control_hashes (hash TEXT PRIMARY KEY, created_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS risk_control_bans (api_key_id BIGINT PRIMARY KEY, name TEXT NOT NULL, blocked BOOLEAN NOT NULL, created_at BIGINT NOT NULL, reset_at BIGINT NOT NULL)`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func (db *DB) LoadRiskConfig(ctx context.Context) (riskcontrol.Config, error) {
	c := riskcontrol.DefaultConfig()
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT payload FROM risk_control_config WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err
}
func (db *DB) SaveRiskConfig(ctx context.Context, c riskcontrol.Config) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO risk_control_config(id,payload) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, string(raw))
	return err
}
func (db *DB) HasRiskHash(ctx context.Context, hash string) (bool, error) {
	var n int
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM risk_control_hashes WHERE hash=$1`, hash).Scan(&n)
	return n > 0, err
}
func (db *DB) DeleteRiskHash(ctx context.Context, hash string) error {
	q := `DELETE FROM risk_control_hashes`
	args := []any{}
	if hash != "" {
		q += ` WHERE hash=$1`
		args = append(args, hash)
	}
	_, err := db.conn.ExecContext(ctx, q, args...)
	return err
}
func (db *DB) RiskBan(ctx context.Context, id int64) (bool, error) {
	var blocked bool
	err := db.conn.QueryRowContext(ctx, `SELECT blocked FROM risk_control_bans WHERE api_key_id=$1`, id).Scan(&blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return blocked, err
}
func (db *DB) UnbanRiskKey(ctx context.Context, id int64) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE risk_control_bans SET blocked=FALSE,reset_at=$1 WHERE api_key_id=$2`, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	// Clear only the counting flag, not historical evidence. This avoids losing
	// a new violation that happens within the same millisecond as an unban.
	if _, err = tx.ExecContext(ctx, `UPDATE risk_control_logs SET counted=FALSE WHERE api_key_id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) RiskBans(ctx context.Context) ([]riskcontrol.Ban, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT api_key_id,name,created_at,reset_at,blocked FROM risk_control_bans WHERE blocked=TRUE ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []riskcontrol.Ban{}
	for rows.Next() {
		var b riskcontrol.Ban
		if err := rows.Scan(&b.APIKeyID, &b.Name, &b.CreatedAt, &b.ResetAt, &b.Blocked); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (db *DB) RecordRiskEvent(ctx context.Context, e *riskcontrol.Event, c riskcontrol.Config) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	counted := e.Flagged && e.Action != "hash_block"
	if counted && e.APIKeyID > 0 {
		// The upsert obtains the per-key row lock before counting (also serializes SQLite writers).
		_, err = tx.ExecContext(ctx, `INSERT INTO risk_control_bans(api_key_id,name,blocked,created_at,reset_at) VALUES($1,$2,FALSE,$3,0) ON CONFLICT(api_key_id) DO UPDATE SET name=excluded.name`, e.APIKeyID, e.APIKeyName, e.CreatedAt)
		if err != nil {
			return err
		}
		var reset int64
		if err = tx.QueryRowContext(ctx, `SELECT reset_at FROM risk_control_bans WHERE api_key_id=$1`, e.APIKeyID).Scan(&reset); err != nil {
			return err
		}
		since := time.Now().Add(-time.Duration(c.WindowHours) * time.Hour).UnixMilli()
		if reset > since {
			since = reset
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM risk_control_logs WHERE api_key_id=$1 AND counted=TRUE AND created_at>=$2`, e.APIKeyID, since).Scan(&e.ViolationCount); err != nil {
			return err
		}
		e.ViolationCount++
		if c.AutoBan && e.ViolationCount >= c.BanThreshold {
			e.AutoBanned = true
			if _, err = tx.ExecContext(ctx, `UPDATE risk_control_bans SET blocked=TRUE,created_at=$1 WHERE api_key_id=$2`, e.CreatedAt, e.APIKeyID); err != nil {
				return err
			}
		}
	}
	if counted && e.Action != "keyword_block" && (e.Audit == nil || e.Audit.WouldBlock) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO risk_control_hashes(hash,created_at) VALUES($1,$2) ON CONFLICT(hash) DO NOTHING`, e.InputHash, e.CreatedAt); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO risk_control_logs(id,created_at,api_key_id,action,flagged,counted,blocked,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, e.ID, e.CreatedAt, e.APIKeyID, e.Action, e.Flagged, counted, e.Blocked, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
func (db *DB) RiskLogs(ctx context.Context, f riskcontrol.LogFilter) (riskcontrol.LogPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	out := riskcontrol.LogPage{Items: []riskcontrol.Event{}, Page: f.Page, PageSize: f.PageSize}
	args := []any{}
	where := []string{"1=1"}
	add := func(expr string, v any) { args = append(args, v); where = append(where, fmt.Sprintf(expr, len(args))) }
	if f.Action != "" {
		add("action=$%d", f.Action)
	}
	if f.APIKeyID > 0 {
		add("api_key_id=$%d", f.APIKeyID)
	}
	if f.Query != "" {
		add("LOWER(payload) LIKE $%d", "%"+strings.ToLower(f.Query)+"%")
	}
	clause := strings.Join(where, " AND ")
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM risk_control_logs WHERE `+clause, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := db.conn.QueryContext(ctx, `SELECT payload FROM risk_control_logs WHERE `+clause+fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var e riskcontrol.Event
		if err := rows.Scan(&raw); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return out, err
		}
		out.Items = append(out.Items, e)
	}
	return out, rows.Err()
}
func (db *DB) RiskStats(ctx context.Context) (riskcontrol.Stats, error) {
	var s riskcontrol.Stats
	for _, q := range []struct {
		query  string
		target *int64
	}{
		{`SELECT COUNT(*) FROM risk_control_logs`, &s.Total}, {`SELECT COUNT(*) FROM risk_control_logs WHERE flagged=TRUE`, &s.Hits}, {`SELECT COUNT(*) FROM risk_control_logs WHERE blocked=TRUE`, &s.Blocked}, {`SELECT COUNT(*) FROM risk_control_hashes`, &s.Hashes}, {`SELECT COUNT(*) FROM risk_control_bans WHERE blocked=TRUE`, &s.Bans},
	} {
		if err := db.conn.QueryRowContext(ctx, q.query).Scan(q.target); err != nil {
			return s, err
		}
	}
	return s, nil
}
func (db *DB) CleanupRiskLogs(ctx context.Context, c riskcontrol.Config) (int64, error) {
	now := time.Now()
	res, err := db.conn.ExecContext(ctx, `DELETE FROM risk_control_logs WHERE (flagged=TRUE AND created_at<$1) OR (flagged=FALSE AND created_at<$2)`, now.AddDate(0, 0, -c.HitDays).UnixMilli(), now.AddDate(0, 0, -c.NonHitDays).UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (db *DB) MarkRiskEmailSent(ctx context.Context, id string) error {
	var raw string
	if err := db.conn.QueryRowContext(ctx, `SELECT payload FROM risk_control_logs WHERE id=$1`, id).Scan(&raw); err != nil {
		return err
	}
	var event riskcontrol.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return err
	}
	event.EmailSent = true
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE risk_control_logs SET payload=$1 WHERE id=$2`, string(encoded), id)
	return err
}
