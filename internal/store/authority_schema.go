package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	authorityProtocolLegacy = 1
	authorityProtocolV2     = 2
)

// migrateAuthorityDarkSchema installs only additive protocol-v2 carriers.
// Activation is a separate, drained operation; opening an existing protocol-1
// database never makes legacy rows authoritative under protocol 2.
func (s *Store) migrateAuthorityDarkSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS supplier_incarnation_registry (
			incarnation_uuid TEXT PRIMARY KEY,
			supplier_id INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			retired_at TEXT,
			CHECK(length(incarnation_uuid)=36),
			UNIQUE(supplier_id,incarnation_uuid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_supplier_incarnation_active
			ON supplier_incarnation_registry(supplier_id,retired_at)`,
		`CREATE TABLE IF NOT EXISTS collection_authority_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
			authority_protocol INTEGER NOT NULL CHECK(authority_protocol IN (1,2)),
			activation_uuid TEXT,
			authority_catalog_digest BLOB,
			protocol1_attempt_max_id INTEGER NOT NULL DEFAULT 0,
			protocol1_attempt_count INTEGER NOT NULL DEFAULT 0,
			protocol1_attempt_digest BLOB,
			protocol1_reservation_max_id INTEGER NOT NULL DEFAULT 0,
			protocol1_reservation_count INTEGER NOT NULL DEFAULT 0,
			protocol1_reservation_digest BLOB,
			protocol1_child_row_count INTEGER NOT NULL DEFAULT 0,
			protocol1_child_digest BLOB,
			next_attempt_id_floor INTEGER NOT NULL DEFAULT 1,
			activated_at TEXT
		)`,
		`INSERT OR IGNORE INTO collection_authority_state(singleton_id,authority_protocol)
			VALUES(1,1)`,
		`CREATE TABLE IF NOT EXISTS collection_attempt_id_sequence (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
			next_attempt_id INTEGER NOT NULL CHECK(next_attempt_id>0)
		)`,
		`CREATE TABLE IF NOT EXISTS collection_attempt_reservations (
			request_uuid TEXT PRIMARY KEY,
			attempt_id INTEGER NOT NULL UNIQUE,
			supplier_id INTEGER NOT NULL,
			authority_protocol INTEGER NOT NULL DEFAULT 1 CHECK(authority_protocol IN (1,2)),
			supplier_incarnation_uuid TEXT,
			lease_generation INTEGER,
			lease_schema_digest BLOB,
			state TEXT NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','committed','abandoned')),
			created_at TEXT NOT NULL,
			resolved_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_collection_reservations_supplier
			ON collection_attempt_reservations(supplier_id,state,attempt_id)`,
		`CREATE TABLE IF NOT EXISTS collection_commit_sequences (
			supplier_id INTEGER NOT NULL,
			supplier_incarnation_uuid TEXT NOT NULL,
			next_commit_ordinal INTEGER NOT NULL CHECK(next_commit_ordinal>0),
			retired_at TEXT,
			PRIMARY KEY(supplier_id,supplier_incarnation_uuid)
		)`,
		`CREATE TABLE IF NOT EXISTS release_admission_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id=1),
			state TEXT NOT NULL CHECK(state IN ('open','draining')),
			generation INTEGER NOT NULL CHECK(generation>0),
			updated_at TEXT NOT NULL
		)`,
		`INSERT OR IGNORE INTO release_admission_state(singleton_id,state,generation,updated_at)
			VALUES(1,'open',1,'1970-01-01T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("install authority dark schema: %w", err)
		}
	}
	for _, column := range []struct {
		table, name, definition string
	}{
		{"suppliers", "current_incarnation_uuid", "TEXT"},
		{"collection_leases", "supplier_incarnation_uuid", "TEXT"},
		{"collection_leases", "lease_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"collection_leases", "leased_until_unix_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"collection_leases", "retired", "INTEGER NOT NULL DEFAULT 0"},
		{"collection_attempts", "authority_protocol", "INTEGER NOT NULL DEFAULT 1"},
		{"collection_attempts", "supplier_incarnation_uuid", "TEXT"},
		{"collection_attempts", "lease_generation", "INTEGER"},
		{"collection_attempts", "lease_schema_digest", "BLOB"},
		{"collection_attempts", "commit_ordinal", "INTEGER"},
		{"collection_attempts", "automation_eligible", "INTEGER NOT NULL DEFAULT 0"},
		{"collection_attempt_reservations", "authority_protocol", "INTEGER NOT NULL DEFAULT 1"},
		{"catalog_group_collection_proofs", "supplier_incarnation_uuid", "TEXT"},
		{"catalog_group_collection_proofs", "authority_protocol", "INTEGER NOT NULL DEFAULT 1"},
	} {
		if err := s.ensureColumn(ctx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO collection_attempt_id_sequence(singleton_id,next_attempt_id)
		SELECT 1,COALESCE(MAX(id),0)+1 FROM collection_attempts`); err != nil {
		return fmt.Errorf("initialize authority attempt sequence: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE collection_attempt_id_sequence SET next_attempt_id=(
		SELECT MAX(collection_attempt_id_sequence.next_attempt_id,COALESCE(MAX(id),0)+1) FROM collection_attempts
	) WHERE singleton_id=1`); err != nil {
		return fmt.Errorf("reconcile authority attempt sequence: %w", err)
	}
	return nil
}

func sqliteUnixMillisTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var now int64
	err := tx.QueryRowContext(ctx,
		`SELECT CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)`).Scan(&now)
	return now, err
}

func unixMillis(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixMilli()
}
