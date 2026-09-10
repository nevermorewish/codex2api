package database

import (
	"context"
	"database/sql"
	"fmt"
)

type FirstTokenTimeoutSettings struct {
	Under50KB  int `json:"under_50kb"`
	Under100KB int `json:"under_100kb"`
	Under200KB int `json:"under_200kb"`
	Under500KB int `json:"under_500kb"`
	Over500KB  int `json:"over_500kb"`
}

func DefaultFirstTokenTimeoutSettings() FirstTokenTimeoutSettings {
	return FirstTokenTimeoutSettings{10, 20, 30, 50, 90}
}

// Validate requires a complete set. Zero is not an instruction to disable a tier.
func (d FirstTokenTimeoutSettings) Validate() error {
	for _, value := range []int{d.Under50KB, d.Under100KB, d.Under200KB, d.Under500KB, d.Over500KB} {
		if value < 1 || value > 600 {
			return fmt.Errorf("first token timeout values must be 1..600 seconds")
		}
	}
	return nil
}

func NormalizeFirstTokenTimeoutSettings(d FirstTokenTimeoutSettings) FirstTokenTimeoutSettings {
	defaults := DefaultFirstTokenTimeoutSettings()
	values := []*int{&d.Under50KB, &d.Under100KB, &d.Under200KB, &d.Under500KB, &d.Over500KB}
	fallbacks := []int{defaults.Under50KB, defaults.Under100KB, defaults.Under200KB, defaults.Under500KB, defaults.Over500KB}
	for i, value := range values {
		if *value < 1 || *value > 600 {
			*value = fallbacks[i]
		}
	}
	return d
}
func (db *DB) GetFirstTokenTimeoutSettings(ctx context.Context) (FirstTokenTimeoutSettings, error) {
	d := DefaultFirstTokenTimeoutSettings()
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(first_token_timeout_under_50kb,10),COALESCE(first_token_timeout_under_100kb,20),COALESCE(first_token_timeout_under_200kb,30),COALESCE(first_token_timeout_under_500kb,50),COALESCE(first_token_timeout_over_500kb,90) FROM system_settings WHERE id=1`).Scan(&d.Under50KB, &d.Under100KB, &d.Under200KB, &d.Under500KB, &d.Over500KB)
	if err == sql.ErrNoRows {
		return d, nil
	}
	return d, err
}
func (db *DB) UpdateFirstTokenTimeoutSettings(ctx context.Context, d FirstTokenTimeoutSettings) error {
	if err := d.Validate(); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_under_50kb,first_token_timeout_under_100kb,first_token_timeout_under_200kb,first_token_timeout_under_500kb,first_token_timeout_over_500kb) VALUES(1,$1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET first_token_timeout_under_50kb=EXCLUDED.first_token_timeout_under_50kb,first_token_timeout_under_100kb=EXCLUDED.first_token_timeout_under_100kb,first_token_timeout_under_200kb=EXCLUDED.first_token_timeout_under_200kb,first_token_timeout_under_500kb=EXCLUDED.first_token_timeout_under_500kb,first_token_timeout_over_500kb=EXCLUDED.first_token_timeout_over_500kb`, d.Under50KB, d.Under100KB, d.Under200KB, d.Under500KB, d.Over500KB)
	return err
}

func (db *DB) UpdateFirstTokenTimeoutMode(ctx context.Context, mode string) error {
	if mode != "request_size" && mode != "first_token" && mode != "disabled" {
		return fmt.Errorf("invalid first token timeout mode")
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_mode) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET first_token_timeout_mode=EXCLUDED.first_token_timeout_mode`, mode)
	return err
}
