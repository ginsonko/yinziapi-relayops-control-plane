package store

import (
	"context"
	"fmt"
	"time"
)

const authCapabilityIdentityMigrationVersion = 2026072202

// migrateAuthCapabilityIdentity preserves independent endpoint evidence for a
// single authorization role. Older databases keyed only by role, allowing a
// later optional usage failure to overwrite an earlier verified balance.
func (s *Store) migrateAuthCapabilityIdentity(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin auth capability identity migration: %w", err)
	}
	defer tx.Rollback()
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, authCapabilityIdentityMigrationVersion).Scan(&applied); err != nil {
		return fmt.Errorf("read auth capability migration state: %w", err)
	}
	if applied > 0 {
		return tx.Commit()
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(auth_capabilities)`)
	if err != nil {
		return fmt.Errorf("inspect auth capability schema: %w", err)
	}
	primaryKey := map[string]int{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan auth capability schema: %w", err)
		}
		if pk > 0 {
			primaryKey[name] = pk
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close auth capability schema rows: %w", err)
	}
	if primaryKey["supplier_id"] != 1 || primaryKey["capability"] != 2 || primaryKey["endpoint_type"] != 3 {
		statements := []string{
			`DROP TABLE IF EXISTS auth_capabilities_v2`,
			`CREATE TABLE auth_capabilities_v2 (
				supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
				capability TEXT NOT NULL,
				status TEXT NOT NULL,
				credential_id INTEGER REFERENCES credentials(id) ON DELETE SET NULL,
				endpoint_type TEXT NOT NULL DEFAULT '',
				http_status INTEGER NOT NULL DEFAULT 0,
				duration_ms INTEGER NOT NULL DEFAULT 0,
				error_class TEXT NOT NULL DEFAULT '',
				error_message TEXT NOT NULL DEFAULT '',
				last_attempt_at TEXT NOT NULL,
				verified_at TEXT,
				PRIMARY KEY(supplier_id,capability,endpoint_type)
			)`,
			`INSERT INTO auth_capabilities_v2
				(supplier_id,capability,status,credential_id,endpoint_type,http_status,duration_ms,error_class,error_message,last_attempt_at,verified_at)
				SELECT supplier_id,capability,status,credential_id,endpoint_type,http_status,duration_ms,error_class,error_message,last_attempt_at,verified_at
				FROM auth_capabilities`,
			`DROP TABLE auth_capabilities`,
			`ALTER TABLE auth_capabilities_v2 RENAME TO auth_capabilities`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate auth capability identity: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`,
		authCapabilityIdentityMigrationVersion, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record auth capability identity migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth capability identity migration: %w", err)
	}
	return nil
}
