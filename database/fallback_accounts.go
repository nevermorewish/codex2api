package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const FallbackProtocolOpenAIResponses = "openai_responses"

type FallbackAccountRow struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Protocol    string    `json:"protocol"`
	BaseURL     string    `json:"base_url"`
	APIKey      string    `json:"-"`
	Model       string    `json:"model"`
	ProxyURL    string    `json:"proxy_url"`
	Concurrency int       `json:"concurrency"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type FallbackPolicy struct {
	Enabled                               bool `json:"enabled"`
	RelayCount                            int  `json:"relay_count"`
	QueueDirectFallbackThreshold          int  `json:"queue_direct_fallback_threshold"`
	OversizedRequestDirectFallbackEnabled bool `json:"oversized_request_direct_fallback_enabled"`
}

func (db *DB) ensureFallbackAccountsSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return errors.New("database is not initialized")
	}
	query := `
		CREATE TABLE IF NOT EXISTS fallback_accounts (
			id BIGSERIAL PRIMARY KEY,
			name VARCHAR(120) NOT NULL,
			protocol VARCHAR(32) NOT NULL DEFAULT 'openai_responses',
			credentials JSONB NOT NULL DEFAULT '{}'::jsonb,
			proxy_url VARCHAR(500) NOT NULL DEFAULT '',
			concurrency INT NOT NULL DEFAULT 10,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_fallback_accounts_enabled ON fallback_accounts(enabled, id);
		CREATE TABLE IF NOT EXISTS fallback_settings (
			id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			relay_count INT NOT NULL DEFAULT 3,
			queue_direct_fallback_threshold INT NOT NULL DEFAULT 5,
			oversized_request_direct_fallback_enabled BOOLEAN NOT NULL DEFAULT FALSE
		);
		INSERT INTO fallback_settings(id, enabled, relay_count)
		VALUES (1, FALSE, 3) ON CONFLICT (id) DO NOTHING;
	`
	if db.isSQLite() {
		query = `
			CREATE TABLE IF NOT EXISTS fallback_accounts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				protocol TEXT NOT NULL DEFAULT 'openai_responses',
				credentials TEXT NOT NULL DEFAULT '{}',
				proxy_url TEXT NOT NULL DEFAULT '',
				concurrency INTEGER NOT NULL DEFAULT 10,
				enabled INTEGER NOT NULL DEFAULT 1,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			);
			CREATE INDEX IF NOT EXISTS idx_fallback_accounts_enabled ON fallback_accounts(enabled, id);
			CREATE TABLE IF NOT EXISTS fallback_settings (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				enabled INTEGER NOT NULL DEFAULT 0,
				relay_count INTEGER NOT NULL DEFAULT 3,
				queue_direct_fallback_threshold INTEGER NOT NULL DEFAULT 5,
				oversized_request_direct_fallback_enabled INTEGER NOT NULL DEFAULT 0
			);
			INSERT OR IGNORE INTO fallback_settings(id, enabled, relay_count) VALUES (1, 0, 3);
		`
	}
	if _, err := db.conn.ExecContext(ctx, query); err != nil {
		return err
	}
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "fallback_settings", "queue_direct_fallback_threshold", "INTEGER NOT NULL DEFAULT 5"); err != nil {
			return err
		}
		return db.ensureSQLiteColumn(ctx, "fallback_settings", "oversized_request_direct_fallback_enabled", "INTEGER NOT NULL DEFAULT 0")
	}
	_, err := db.conn.ExecContext(ctx, `
		ALTER TABLE fallback_settings ADD COLUMN IF NOT EXISTS queue_direct_fallback_threshold INT NOT NULL DEFAULT 5;
		ALTER TABLE fallback_settings ADD COLUMN IF NOT EXISTS oversized_request_direct_fallback_enabled BOOLEAN NOT NULL DEFAULT FALSE;
	`)
	return err
}

func encodeFallbackCredentials(baseURL, apiKey, model string) ([]byte, error) {
	return json.Marshal(encryptSensitiveCredentials(map[string]interface{}{
		"base_url": strings.TrimSpace(baseURL),
		"api_key":  strings.TrimSpace(apiKey),
		"model":    strings.TrimSpace(model),
	}))
}

func scanFallbackAccount(scanner interface{ Scan(...interface{}) error }) (*FallbackAccountRow, error) {
	var row FallbackAccountRow
	var credentialsRaw, createdRaw, updatedRaw interface{}
	if err := scanner.Scan(
		&row.ID, &row.Name, &row.Protocol, &credentialsRaw, &row.ProxyURL,
		&row.Concurrency, &row.Enabled, &createdRaw, &updatedRaw,
	); err != nil {
		return nil, err
	}
	credentials := decodeCredentials(credentialsRaw)
	row.BaseURL, _ = credentials["base_url"].(string)
	row.APIKey, _ = credentials["api_key"].(string)
	row.Model, _ = credentials["model"].(string)
	var err error
	row.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, err
	}
	row.UpdatedAt, err = parseDBTimeValue(updatedRaw)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

const fallbackAccountColumns = `id, name, protocol, credentials, proxy_url, concurrency, enabled, created_at, updated_at`

func (db *DB) ListFallbackAccounts(ctx context.Context) ([]*FallbackAccountRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+fallbackAccountColumns+` FROM fallback_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*FallbackAccountRow, 0)
	for rows.Next() {
		row, err := scanFallbackAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (db *DB) GetFallbackAccount(ctx context.Context, id int64) (*FallbackAccountRow, error) {
	row, err := scanFallbackAccount(db.conn.QueryRowContext(ctx, `SELECT `+fallbackAccountColumns+` FROM fallback_accounts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return row, err
}

func (db *DB) CreateFallbackAccount(ctx context.Context, row *FallbackAccountRow) (*FallbackAccountRow, error) {
	if row == nil {
		return nil, errors.New("fallback account is required")
	}
	credentials, err := encodeFallbackCredentials(row.BaseURL, row.APIKey, row.Model)
	if err != nil {
		return nil, err
	}
	var id int64
	if db.isSQLite() {
		result, err := db.conn.ExecContext(ctx, `
			INSERT INTO fallback_accounts(name, protocol, credentials, proxy_url, concurrency, enabled)
			VALUES (?, ?, ?, ?, ?, ?)`, row.Name, row.Protocol, string(credentials), row.ProxyURL, row.Concurrency, row.Enabled)
		if err != nil {
			return nil, err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return nil, err
		}
	} else {
		err = db.conn.QueryRowContext(ctx, `
			INSERT INTO fallback_accounts(name, protocol, credentials, proxy_url, concurrency, enabled)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6) RETURNING id`,
			row.Name, row.Protocol, credentials, row.ProxyURL, row.Concurrency, row.Enabled,
		).Scan(&id)
		if err != nil {
			return nil, err
		}
	}
	return db.GetFallbackAccount(ctx, id)
}

func (db *DB) UpdateFallbackAccount(ctx context.Context, row *FallbackAccountRow) (*FallbackAccountRow, error) {
	if row == nil || row.ID <= 0 {
		return nil, errors.New("valid fallback account is required")
	}
	credentials, err := encodeFallbackCredentials(row.BaseURL, row.APIKey, row.Model)
	if err != nil {
		return nil, err
	}
	query := `UPDATE fallback_accounts SET name=$1, protocol=$2, credentials=$3::jsonb, proxy_url=$4, concurrency=$5, enabled=$6, updated_at=NOW() WHERE id=$7`
	args := []interface{}{row.Name, row.Protocol, credentials, row.ProxyURL, row.Concurrency, row.Enabled, row.ID}
	if db.isSQLite() {
		query = `UPDATE fallback_accounts SET name=?, protocol=?, credentials=?, proxy_url=?, concurrency=?, enabled=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`
		args[2] = string(credentials)
	}
	result, err := db.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil, sql.ErrNoRows
	}
	return db.GetFallbackAccount(ctx, row.ID)
}

func (db *DB) DeleteFallbackAccount(ctx context.Context, id int64) error {
	result, err := db.conn.ExecContext(ctx, `DELETE FROM fallback_accounts WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) GetFallbackPolicy(ctx context.Context) (FallbackPolicy, error) {
	var policy FallbackPolicy
	err := db.conn.QueryRowContext(ctx, `
		SELECT enabled, relay_count, queue_direct_fallback_threshold, oversized_request_direct_fallback_enabled
		FROM fallback_settings WHERE id=1
	`).Scan(
		&policy.Enabled,
		&policy.RelayCount,
		&policy.QueueDirectFallbackThreshold,
		&policy.OversizedRequestDirectFallbackEnabled,
	)
	return policy, err
}

func (db *DB) UpdateFallbackPolicy(ctx context.Context, policy FallbackPolicy) error {
	if policy.RelayCount < 1 || policy.RelayCount > 1000 {
		return fmt.Errorf("relay_count must be between 1 and 1000")
	}
	if policy.QueueDirectFallbackThreshold < 0 || policy.QueueDirectFallbackThreshold > 1000 {
		return fmt.Errorf("queue_direct_fallback_threshold must be between 0 and 1000")
	}
	_, err := db.conn.ExecContext(ctx, `
		UPDATE fallback_settings
		SET enabled=$1, relay_count=$2, queue_direct_fallback_threshold=$3, oversized_request_direct_fallback_enabled=$4
		WHERE id=1
	`, policy.Enabled, policy.RelayCount, policy.QueueDirectFallbackThreshold, policy.OversizedRequestDirectFallbackEnabled)
	return err
}
