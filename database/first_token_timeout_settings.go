package database

import (
	"context"
	"database/sql"
	"fmt"
	"encoding/json"
)

type FirstTokenTimeoutSettings struct {
	Under50KB  int `json:"under_50kb"`
	Under100KB int `json:"under_100kb"`
	Under200KB int `json:"under_200kb"`
	Under500KB int `json:"under_500kb"`
	Over500KB  int `json:"over_500kb"`
	ModelTimeouts map[string]int `json:"model_timeouts,omitempty"`
}

func DefaultFirstTokenTimeoutSettings() FirstTokenTimeoutSettings {
	return FirstTokenTimeoutSettings{Under50KB:10, Under100KB:20, Under200KB:30, Under500KB:50, Over500KB:90, ModelTimeouts: map[string]int{}}
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
	if d.ModelTimeouts == nil { d.ModelTimeouts = map[string]int{} }
	for model, value := range d.ModelTimeouts { if value < 1 || value > 600 { delete(d.ModelTimeouts, model) } }
	return d
}
func (db *DB) GetFirstTokenTimeoutSettings(ctx context.Context) (FirstTokenTimeoutSettings, error) {
	d := DefaultFirstTokenTimeoutSettings()
	var raw sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(first_token_timeout_under_50kb,10),COALESCE(first_token_timeout_under_100kb,20),COALESCE(first_token_timeout_under_200kb,30),COALESCE(first_token_timeout_under_500kb,50),COALESCE(first_token_timeout_over_500kb,90),COALESCE(first_token_timeout_models,'{}') FROM system_settings WHERE id=1`).Scan(&d.Under50KB, &d.Under100KB, &d.Under200KB, &d.Under500KB, &d.Over500KB, &raw)
	if err == sql.ErrNoRows {
		return d, nil
	}
	if err == nil { _ = json.Unmarshal([]byte(raw.String), &d.ModelTimeouts) }; return d, err
}
func (db *DB) UpdateFirstTokenTimeoutSettings(ctx context.Context, d FirstTokenTimeoutSettings) error {
	if err := d.Validate(); err != nil {
		return err
	}
	raw,_:=json.Marshal(d.ModelTimeouts); _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_under_50kb,first_token_timeout_under_100kb,first_token_timeout_under_200kb,first_token_timeout_under_500kb,first_token_timeout_over_500kb,first_token_timeout_models) VALUES(1,$1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET first_token_timeout_under_50kb=EXCLUDED.first_token_timeout_under_50kb,first_token_timeout_under_100kb=EXCLUDED.first_token_timeout_under_100kb,first_token_timeout_under_200kb=EXCLUDED.first_token_timeout_under_200kb,first_token_timeout_under_500kb=EXCLUDED.first_token_timeout_under_500kb,first_token_timeout_over_500kb=EXCLUDED.first_token_timeout_over_500kb,first_token_timeout_models=EXCLUDED.first_token_timeout_models`, d.Under50KB, d.Under100KB, d.Under200KB, d.Under500KB, d.Over500KB,string(raw))
	return err
}

func (db *DB) UpdateFirstTokenTimeoutMode(ctx context.Context, mode string) error {
	if mode != "request_size" && mode != "first_token" && mode != "disabled" {
		return fmt.Errorf("invalid first token timeout mode")
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id,first_token_timeout_mode) VALUES(1,$1) ON CONFLICT(id) DO UPDATE SET first_token_timeout_mode=EXCLUDED.first_token_timeout_mode`, mode)
	return err
}
