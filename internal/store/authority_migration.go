package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAuthorityAlreadyActive  = errors.New("collection authority protocol 2 is already active")
	ErrAuthorityDrainRequired  = errors.New("collection authority activation requires a completed drain")
	ErrAuthorityMigrationFault = errors.New("injected authority migration failure")
)

const finalCollectionLeasesDDL = `CREATE TABLE collection_leases (
	supplier_id INTEGER PRIMARY KEY,
	supplier_incarnation_uuid TEXT,
	owner_id TEXT NOT NULL DEFAULT '',
	leased_until TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	lease_generation INTEGER NOT NULL DEFAULT 0 CHECK(lease_generation>=0),
	leased_until_unix_ms INTEGER NOT NULL DEFAULT 0,
	retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0,1))
)`

const authorityStateUpdateTriggerSQL = `CREATE TRIGGER trg_collection_authority_state_update
BEFORE UPDATE ON collection_authority_state
WHEN OLD.authority_protocol=2
BEGIN
	SELECT RAISE(ABORT,'collection authority activation row is immutable');
END`

const authorityStateDeleteTriggerSQL = `CREATE TRIGGER trg_collection_authority_state_delete
BEFORE DELETE ON collection_authority_state
BEGIN
	SELECT RAISE(ABORT,'collection authority activation row is immutable');
END`

const authorityAttemptInsertTriggerSQL = `CREATE TRIGGER trg_collection_attempt_authority_insert
BEFORE INSERT ON collection_attempts
WHEN (SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1)=2
	AND (NEW.authority_protocol<>2 OR NEW.supplier_incarnation_uuid IS NULL OR
		NEW.lease_generation IS NULL OR NEW.lease_generation<=0 OR
		NEW.lease_schema_digest IS NULL OR length(NEW.lease_schema_digest)<>32 OR
		NEW.commit_ordinal IS NULL OR NEW.commit_ordinal<=0 OR NEW.automation_eligible<>1)
BEGIN
	SELECT RAISE(ABORT,'collection attempt lacks protocol-2 authority');
END`

const authorityReservationInsertTriggerSQL = `CREATE TRIGGER trg_collection_reservation_authority_insert
BEFORE INSERT ON collection_attempt_reservations
WHEN (SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1)=2
	AND (NEW.authority_protocol<>2 OR NEW.supplier_incarnation_uuid IS NULL OR
		NEW.lease_generation IS NULL OR NEW.lease_generation<=0 OR
		NEW.lease_schema_digest IS NULL OR length(NEW.lease_schema_digest)<>32)
BEGIN
	SELECT RAISE(ABORT,'collection reservation lacks protocol-2 authority');
END`

const authoritySupplierDeleteTriggerSQL = `CREATE TRIGGER trg_supplier_delete_authority_fence
BEFORE DELETE ON suppliers
WHEN (SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1)=2
BEGIN
	SELECT RAISE(ABORT,'protocol-2 supplier deletion requires retained-history migration');
END`

type AuthorityMigrationOptions struct {
	FailureStage string
}

type AuthorityMigrationReceipt struct {
	ActivationUUID          string
	CatalogDigest           [sha256.Size]byte
	LegacyLeaseDigest       [sha256.Size]byte
	StableLeaseDigest       [sha256.Size]byte
	FinalLeaseDigest        [sha256.Size]byte
	LegacyAttemptDigest     [sha256.Size]byte
	LegacyAttemptCount      int64
	LegacyAttemptMaxID      int64
	LegacyReservationDigest [sha256.Size]byte
	LegacyReservationCount  int64
	LegacyReservationMaxID  int64
	LegacyChildDigest       [sha256.Size]byte
	LegacyChildCount        int64
	NextAttemptIDFloor      int64
	AuthorityObjectNames    []string
	AuthorityTableClosure   []string
}

func (s *Store) ActivateCollectionAuthorityV2(ctx context.Context, options AuthorityMigrationOptions) (AuthorityMigrationReceipt, error) {
	status, err := s.ReleaseAdmissionStatus(ctx)
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if !status.Drained() {
		return AuthorityMigrationReceipt{}, ErrAuthorityDrainRequired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var protocol int
	if err := tx.QueryRowContext(ctx, `SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if protocol == authorityProtocolV2 {
		return AuthorityMigrationReceipt{}, ErrAuthorityAlreadyActive
	}
	stableBefore, err := tableReceiptTx(ctx, tx, "collection_leases", "legacy-stable", "1",
		[]string{"supplier_id", "owner_id", "leased_until", "updated_at"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	legacyFull, err := tableReceiptTx(ctx, tx, "collection_leases", "legacy-full", "1",
		[]string{"supplier_id", "supplier_incarnation_uuid", "owner_id", "leased_until", "updated_at", "lease_generation", "leased_until_unix_ms", "retired"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := authorityMigrationFault(options, "before_incarnation_bind"); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := bindAllSupplierIncarnationsTx(ctx, tx); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := authorityMigrationFault(options, "after_incarnation_bind"); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := rebuildCollectionLeasesTx(ctx, tx); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := authorityMigrationFault(options, "after_lease_rebuild"); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	stableAfter, err := tableReceiptTx(ctx, tx, "collection_leases", "legacy-stable", "1",
		[]string{"supplier_id", "owner_id", "leased_until", "updated_at"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if stableBefore.AggregateDigest != stableAfter.AggregateDigest {
		return AuthorityMigrationReceipt{}, errors.New("collection lease stable projection changed during migration")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collection_attempts SET authority_protocol=1,automation_eligible=0,
		supplier_incarnation_uuid=NULL,lease_generation=NULL,lease_schema_digest=NULL,commit_ordinal=NULL`); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collection_attempt_reservations SET authority_protocol=1,
		state='abandoned',resolved_at=COALESCE(resolved_at,?)
		WHERE state='reserved'`, formatTime(time.Now().UTC())); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	legacyAttempt, err := tableReceiptTx(ctx, tx, "collection_attempts", "protocol1-attempts", "1",
		[]string{"id", "supplier_id", "status", "error_class", "error_message", "catalog_updated", "balance_updated", "started_at", "finished_at", "duration_ms"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	legacyReservations, err := tableReceiptTx(ctx, tx, "collection_attempt_reservations", "protocol1-reservations", "1",
		[]string{"request_uuid", "attempt_id", "supplier_id", "authority_protocol", "supplier_incarnation_uuid", "lease_generation",
			"lease_schema_digest", "state", "created_at", "resolved_at"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	legacyChildren, legacyChildCount, err := protocolOneChildReceiptTx(ctx, tx)
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	var maxAttemptID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM collection_attempts`).Scan(&maxAttemptID); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	var maxReservationID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_id),0) FROM collection_attempt_reservations`).Scan(&maxReservationID); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	nextFloor := maxAttemptID
	if maxReservationID > nextFloor {
		nextFloor = maxReservationID
	}
	if nextFloor == 9223372036854775807 {
		return AuthorityMigrationReceipt{}, errors.New("collection attempt identity exhausted")
	}
	nextFloor++
	if _, err := tx.ExecContext(ctx, `UPDATE collection_attempt_id_sequence SET next_attempt_id=? WHERE singleton_id=1`, nextFloor); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM collection_commit_sequences`); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS trg_collection_authority_state_update;
		DROP TRIGGER IF EXISTS trg_collection_authority_state_delete;
		DROP TRIGGER IF EXISTS trg_collection_attempt_authority_insert;
		DROP TRIGGER IF EXISTS trg_collection_reservation_authority_insert;
		DROP TRIGGER IF EXISTS trg_supplier_delete_authority_fence;`); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	for _, ddl := range []string{authorityStateUpdateTriggerSQL, authorityStateDeleteTriggerSQL,
		authorityAttemptInsertTriggerSQL, authorityReservationInsertTriggerSQL, authoritySupplierDeleteTriggerSQL} {
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return AuthorityMigrationReceipt{}, err
		}
	}
	if err := authorityMigrationFault(options, "after_trigger_install"); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	catalog, err := buildAuthorityCatalogTx(ctx, tx, true)
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	activationUUID, err := newAuthorityUUID()
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	activatedAt := formatTime(time.Now().UTC())
	reservationDigest := legacyReservations.AggregateDigest
	if len(legacyReservations.Rows) == 0 {
		reservationDigest = emptyAuthorityDigest("protocol1-reservations")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collection_authority_state SET authority_protocol=2,
		activation_uuid=?,authority_catalog_digest=?,protocol1_attempt_max_id=?,protocol1_attempt_count=?,
		protocol1_attempt_digest=?,protocol1_reservation_max_id=?,protocol1_reservation_count=?,
		protocol1_reservation_digest=?,protocol1_child_row_count=?,protocol1_child_digest=?,next_attempt_id_floor=?,activated_at=?
		WHERE singleton_id=1 AND authority_protocol=1`, activationUUID, catalog.AggregateDigest[:], maxAttemptID,
		len(legacyAttempt.Rows), legacyAttempt.AggregateDigest[:], maxReservationID, len(legacyReservations.Rows),
		reservationDigest[:],
		legacyChildCount, legacyChildren[:], nextFloor, activatedAt); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := authorityMigrationFault(options, "before_activation_commit"); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	finalLease, err := tableReceiptTx(ctx, tx, "collection_leases", "authority-final", "2",
		[]string{"supplier_id", "supplier_incarnation_uuid", "owner_id", "leased_until", "updated_at", "lease_generation", "leased_until_unix_ms", "retired"})
	if err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthorityMigrationReceipt{}, err
	}
	return AuthorityMigrationReceipt{ActivationUUID: activationUUID, CatalogDigest: catalog.AggregateDigest,
		LegacyLeaseDigest: legacyFull.AggregateDigest, StableLeaseDigest: stableAfter.AggregateDigest,
		FinalLeaseDigest: finalLease.AggregateDigest, LegacyAttemptDigest: legacyAttempt.AggregateDigest,
		LegacyAttemptCount: int64(len(legacyAttempt.Rows)), LegacyAttemptMaxID: maxAttemptID,
		LegacyReservationDigest: reservationDigest, LegacyReservationCount: int64(len(legacyReservations.Rows)),
		LegacyReservationMaxID: maxReservationID,
		LegacyChildDigest:      legacyChildren, LegacyChildCount: legacyChildCount, NextAttemptIDFloor: nextFloor,
		AuthorityObjectNames: catalog.ObjectNames, AuthorityTableClosure: catalog.Tables}, nil
}

func authorityMigrationFault(options AuthorityMigrationOptions, stage string) error {
	if options.FailureStage == stage {
		return fmt.Errorf("%w at %s", ErrAuthorityMigrationFault, stage)
	}
	return nil
}

func bindAllSupplierIncarnationsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,current_incarnation_uuid FROM suppliers ORDER BY id`)
	if err != nil {
		return err
	}
	type item struct {
		id          int64
		incarnation sql.NullString
	}
	items := make([]item, 0)
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.incarnation); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range items {
		if value.incarnation.Valid && value.incarnation.String != "" {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM supplier_incarnation_registry
				WHERE incarnation_uuid=? AND supplier_id=? AND retired_at IS NULL`, value.incarnation.String, value.id).Scan(&count); err != nil {
				return err
			}
			if count == 1 {
				continue
			}
			return errors.New("supplier incarnation registry mismatch")
		}
		if _, err := ensureSupplierIncarnationTx(ctx, tx, value.id); err != nil {
			return err
		}
	}
	return nil
}

func ensureSupplierIncarnationTx(ctx context.Context, tx *sql.Tx, supplierID int64) (string, error) {
	var incarnation sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT current_incarnation_uuid FROM suppliers WHERE id=?`, supplierID).Scan(&incarnation); err != nil {
		return "", err
	}
	if incarnation.Valid && incarnation.String != "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM supplier_incarnation_registry WHERE
			incarnation_uuid=? AND supplier_id=? AND retired_at IS NULL`, incarnation.String, supplierID).Scan(&count); err != nil {
			return "", err
		}
		if count == 1 {
			return incarnation.String, nil
		}
		return "", errors.New("supplier incarnation registry mismatch")
	}
	generated, err := newAuthorityUUID()
	if err != nil {
		return "", err
	}
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO supplier_incarnation_registry
		(incarnation_uuid,supplier_id,created_at,retired_at) VALUES(?,?,?,NULL)`, generated, supplierID, now); err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE suppliers SET current_incarnation_uuid=?
		WHERE id=? AND current_incarnation_uuid IS NULL`, generated, supplierID)
	if err != nil {
		return "", err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", errors.New("supplier incarnation changed during bind")
	}
	return generated, nil
}

func rebuildCollectionLeasesTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE collection_leases RENAME TO collection_leases_protocol1`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, finalCollectionLeasesDDL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO collection_leases
		(supplier_id,supplier_incarnation_uuid,owner_id,leased_until,updated_at,lease_generation,leased_until_unix_ms,retired)
		SELECT l.supplier_id,s.current_incarnation_uuid,l.owner_id,l.leased_until,l.updated_at,
			CASE WHEN l.lease_generation<1 THEN 1 ELSE l.lease_generation END,l.leased_until_unix_ms,l.retired
		FROM collection_leases_protocol1 l LEFT JOIN suppliers s ON s.id=l.supplier_id`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE collection_leases_protocol1`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX idx_collection_leases_expiry ON collection_leases(leased_until_unix_ms)`)
	return err
}

func tableReceiptTx(ctx context.Context, tx *sql.Tx, table, projectionID, version string, columns []string) (sqliteTableRowsV2Receipt, error) {
	return tableReceiptWhereTx(ctx, tx, table, projectionID, version, columns, "")
}

func tableReceiptWhereTx(ctx context.Context, tx *sql.Tx, table, projectionID, version string, columns []string,
	where string, args ...any) (sqliteTableRowsV2Receipt, error) {
	quoted := make([]string, len(columns))
	for index, column := range columns {
		quoted[index] = quoteSQLiteIdentifier(column)
	}
	query := `SELECT ` + strings.Join(quoted, ",") + ` FROM main.` + quoteSQLiteIdentifier(table)
	if strings.TrimSpace(where) != "" {
		query += ` WHERE ` + where
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return sqliteTableRowsV2Receipt{}, err
	}
	defer rows.Close()
	values := make([][]sqliteTableRowsV2Value, 0)
	for rows.Next() {
		raw := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range raw {
			destinations[index] = &raw[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return sqliteTableRowsV2Receipt{}, err
		}
		framed, err := sqliteTableRowsV2ValuesFromDriver(raw)
		if err != nil {
			return sqliteTableRowsV2Receipt{}, err
		}
		values = append(values, framed)
	}
	if err := rows.Err(); err != nil {
		return sqliteTableRowsV2Receipt{}, err
	}
	return buildSQLiteTableRowsV2Receipt(sqliteTableRowsV2Projection{LogicalDatabaseID: "relayops",
		SchemaName: "main", TableName: table, ProjectionID: projectionID, ProjectionVersion: version,
		Columns: append([]string(nil), columns...)}, values)
}

func protocolOneChildReceiptTx(ctx context.Context, tx *sql.Tx) ([sha256.Size]byte, int64, error) {
	hash := sha256.New()
	var total int64
	projections := []struct {
		table   string
		columns []string
	}{
		{"catalog_group_collection_proofs", []string{"attempt_id", "supplier_id", "source_group", "source_semantic_hash", "proof_json", "observed_at"}},
		{"request_evidence", []string{"id", "attempt_id", "supplier_id", "endpoint_type", "auth_capability", "method", "source_url", "http_status", "status", "error_class", "error_message", "duration_ms", "schema_version", "response_hash", "observed_at"}},
	}
	for _, projection := range projections {
		receipt, err := tableReceiptWhereTx(ctx, tx, projection.table, "protocol1-child", "1", projection.columns,
			`attempt_id IN (SELECT id FROM collection_attempts WHERE authority_protocol=1)`)
		if err != nil {
			return [sha256.Size]byte{}, 0, err
		}
		hash.Write(receipt.AggregateDigest[:])
		total += int64(len(receipt.Rows))
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, total, nil
}

func emptyAuthorityDigest(domain string) [sha256.Size]byte {
	return sha256.Sum256([]byte("r30-empty-set\x00" + domain))
}

func newAuthorityUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
