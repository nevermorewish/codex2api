package database

import "context"

func (db *DB) GetOverloadConditionSettings(ctx context.Context) (bool, bool, error) {
	var code, message bool
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(codex_overload_code_enabled,false),COALESCE(codex_overload_message_enabled,false) FROM system_settings WHERE id=1`).Scan(&code, &message)
	return code, message, err
}
func (db *DB) UpdateOverloadConditionSettings(ctx context.Context, code, message bool) error {
	query := `INSERT INTO system_settings(id,codex_overload_code_enabled,codex_overload_message_enabled) VALUES(1,$1,$2) ON CONFLICT(id) DO UPDATE SET codex_overload_code_enabled=EXCLUDED.codex_overload_code_enabled,codex_overload_message_enabled=EXCLUDED.codex_overload_message_enabled`
	if db.isSQLite() {
		query = `INSERT INTO system_settings(id,codex_overload_code_enabled,codex_overload_message_enabled) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET codex_overload_code_enabled=excluded.codex_overload_code_enabled,codex_overload_message_enabled=excluded.codex_overload_message_enabled`
	}
	_, err := db.conn.ExecContext(ctx, query, code, message)
	return err
}
