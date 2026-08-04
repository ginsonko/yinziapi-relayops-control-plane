package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const authorityCatalogMagic = "R30-AUTHORITY-SQLITE-CATALOG-V1\n"

var ErrAuthorityCatalogInvalid = errors.New("collection authority catalog is invalid")

type authoritySchemaRow struct {
	Type  string
	Name  string
	Table string
	SQL   sql.NullString
	frame []byte
}

type AuthorityCatalogReceipt struct {
	Tables          []string
	ObjectNames     []string
	Bytes           []byte
	AggregateDigest [sha256.Size]byte
}

func (s *Store) AuditCollectionAuthority(ctx context.Context) (AuthorityCatalogReceipt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var protocol int
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT authority_protocol,authority_catalog_digest
		FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol, &stored); err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	if protocol != authorityProtocolV2 || len(stored) != sha256.Size {
		return AuthorityCatalogReceipt{}, fmt.Errorf("%w: protocol 2 is not active", ErrAuthorityCatalogInvalid)
	}
	receipt, err := buildAuthorityCatalogTx(ctx, tx, true)
	if err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	if !bytes.Equal(stored, receipt.AggregateDigest[:]) {
		return AuthorityCatalogReceipt{}, fmt.Errorf("%w: aggregate digest changed", ErrAuthorityCatalogInvalid)
	}
	if err := auditCollectionAuthorityDataTx(ctx, tx, receipt.AggregateDigest, true); err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) auditActiveAuthorityOnOpen(ctx context.Context) error {
	var protocol int
	if err := s.db.QueryRowContext(ctx, `SELECT authority_protocol FROM collection_authority_state
		WHERE singleton_id=1`).Scan(&protocol); err != nil {
		return err
	}
	if protocol != authorityProtocolV2 {
		return nil
	}
	_, err := s.AuditCollectionAuthority(ctx)
	return err
}

// preflightActiveAuthorityBeforeMigration prevents an idempotent-looking
// schema migration from repairing or masking a corrupted protocol-2 carrier
// before the immutable activation watermark has been checked.
func (s *Store) preflightActiveAuthorityBeforeMigration(ctx context.Context) error {
	var tableCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.sqlite_schema
		WHERE type='table' AND name='collection_authority_state'`).Scan(&tableCount); err != nil {
		return err
	}
	if tableCount == 0 {
		return nil
	}
	var protocol int
	err := s.db.QueryRowContext(ctx, `SELECT authority_protocol FROM collection_authority_state
		WHERE singleton_id=1`).Scan(&protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if protocol != authorityProtocolV2 {
		return nil
	}
	_, err = s.AuditCollectionAuthority(ctx)
	return err
}

func checkCollectionAuthorityTx(ctx context.Context, tx *sql.Tx) error {
	var protocol int
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT authority_protocol,authority_catalog_digest
		FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol, &stored); err != nil {
		return err
	}
	if protocol != authorityProtocolV2 {
		return nil
	}
	if len(stored) != sha256.Size {
		return fmt.Errorf("%w: activation digest missing", ErrAuthorityCatalogInvalid)
	}
	receipt, err := buildAuthorityCatalogTx(ctx, tx, true)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, receipt.AggregateDigest[:]) {
		return fmt.Errorf("%w: aggregate digest changed", ErrAuthorityCatalogInvalid)
	}
	// Until a connection-scoped PRAGMA data_version cache is introduced, use
	// the complete frozen protocol-1 receipt on every authority boundary. This
	// keeps an external legacy-row mutation from being hidden by an indexed-only
	// check and is preferable to accepting a stale pricing/collection basis.
	return auditCollectionAuthorityDataTx(ctx, tx, receipt.AggregateDigest, true)
}

var authoritySeedTables = []string{
	"suppliers",
	"supplier_incarnation_registry",
	"collection_authority_state",
	"collection_leases",
	"collection_attempt_id_sequence",
	"collection_attempt_reservations",
	"collection_commit_sequences",
	"collection_attempts",
	"catalog_snapshots",
	"discoveries",
	"price_observations",
	"balance_observations",
	"catalog_group_collection_proofs",
	"request_evidence",
	"auth_capabilities",
}

type authorityInboundObject struct {
	ObjectType        string
	Name              string
	AttachedTable     string
	RawSQL            string
	DirectTableInputs []string
}

var authorityInboundObjects = []authorityInboundObject{
	{ObjectType: "trigger", Name: "trg_collection_authority_state_update", AttachedTable: "collection_authority_state", RawSQL: authorityStateUpdateTriggerSQL, DirectTableInputs: []string{"collection_authority_state"}},
	{ObjectType: "trigger", Name: "trg_collection_authority_state_delete", AttachedTable: "collection_authority_state", RawSQL: authorityStateDeleteTriggerSQL, DirectTableInputs: []string{"collection_authority_state"}},
	{ObjectType: "trigger", Name: "trg_collection_attempt_authority_insert", AttachedTable: "collection_attempts", RawSQL: authorityAttemptInsertTriggerSQL, DirectTableInputs: []string{"collection_attempts", "collection_authority_state"}},
	{ObjectType: "trigger", Name: "trg_collection_reservation_authority_insert", AttachedTable: "collection_attempt_reservations", RawSQL: authorityReservationInsertTriggerSQL, DirectTableInputs: []string{"collection_attempt_reservations", "collection_authority_state"}},
	{ObjectType: "trigger", Name: "trg_supplier_delete_authority_fence", AttachedTable: "suppliers", RawSQL: authoritySupplierDeleteTriggerSQL, DirectTableInputs: []string{"suppliers", "collection_authority_state"}},
}

func buildAuthorityCatalogTx(ctx context.Context, tx *sql.Tx, requireInboundInventory bool) (AuthorityCatalogReceipt, error) {
	closure := make(map[string]struct{}, len(authoritySeedTables)+4)
	queue := append([]string(nil), authoritySeedTables...)
	for len(queue) > 0 {
		table := queue[0]
		queue = queue[1:]
		if _, seen := closure[table]; seen {
			continue
		}
		closure[table] = struct{}{}
		rows, err := tx.QueryContext(ctx, `PRAGMA main.foreign_key_list(`+quoteSQLiteIdentifier(table)+`)`)
		if err != nil {
			return AuthorityCatalogReceipt{}, fmt.Errorf("inspect authority foreign keys for %s: %w", table, err)
		}
		for rows.Next() {
			var id, seq int
			var target, from, to, onUpdate, onDelete, match string
			if err := rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				_ = rows.Close()
				return AuthorityCatalogReceipt{}, err
			}
			if _, seen := closure[target]; !seen {
				queue = append(queue, target)
			}
		}
		if err := rows.Close(); err != nil {
			return AuthorityCatalogReceipt{}, err
		}
	}

	if requireInboundInventory {
		if err := verifyAuthorityInboundInventoryTx(ctx, tx, closure); err != nil {
			return AuthorityCatalogReceipt{}, err
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM main.sqlite_schema
		ORDER BY type,name,tbl_name`)
	if err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	defer rows.Close()
	schemaRows := make([]authoritySchemaRow, 0, 64)
	for rows.Next() {
		var row authoritySchemaRow
		if err := rows.Scan(&row.Type, &row.Name, &row.Table, &row.SQL); err != nil {
			return AuthorityCatalogReceipt{}, err
		}
		if _, include := closure[row.Table]; !include {
			if _, include = closure[row.Name]; !include {
				continue
			}
		}
		frame, err := encodeAuthorityCatalogRow(row)
		if err != nil {
			return AuthorityCatalogReceipt{}, err
		}
		row.frame = frame
		schemaRows = append(schemaRows, row)
	}
	if err := rows.Err(); err != nil {
		return AuthorityCatalogReceipt{}, err
	}
	for table := range closure {
		found := false
		for _, row := range schemaRows {
			if row.Type == "table" && row.Name == table {
				found = true
				break
			}
		}
		if !found {
			return AuthorityCatalogReceipt{}, fmt.Errorf("%w: required table %s is missing", ErrAuthorityCatalogInvalid, table)
		}
	}
	sort.Slice(schemaRows, func(i, j int) bool { return bytes.Compare(schemaRows[i].frame, schemaRows[j].frame) < 0 })
	payload := make([]byte, 0, len(authorityCatalogMagic)+8+len(schemaRows)*128)
	payload = append(payload, authorityCatalogMagic...)
	payload = appendAuthorityUint64(payload, uint64(len(schemaRows)))
	objectNames := make([]string, 0, len(schemaRows))
	for index, row := range schemaRows {
		frame := bytes.Clone(row.frame)
		binary.BigEndian.PutUint64(frame[4:12], uint64(index))
		digestInput := make([]byte, 0, len("r30-authority-catalog-row-v1\x00")+len(frame))
		digestInput = append(digestInput, "r30-authority-catalog-row-v1\x00"...)
		digestInput = append(digestInput, frame...)
		digest := sha256.Sum256(digestInput)
		payload = append(payload, frame...)
		payload = append(payload, digest[:]...)
		objectNames = append(objectNames, row.Type+":"+row.Name)
	}
	aggregateInput := append([]byte("r30-authority-catalog-v1\x00"), payload...)
	aggregate := sha256.Sum256(aggregateInput)
	payload = append(payload, aggregate[:]...)
	tables := make([]string, 0, len(closure))
	for table := range closure {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return AuthorityCatalogReceipt{Tables: tables, ObjectNames: objectNames, Bytes: payload, AggregateDigest: aggregate}, nil
}

func verifyAuthorityInboundInventoryTx(ctx context.Context, tx *sql.Tx, closure map[string]struct{}) error {
	expected := make(map[string]authorityInboundObject, len(authorityInboundObjects))
	for _, item := range authorityInboundObjects {
		expected[item.ObjectType+"\x00"+item.Name] = item
		for _, dependency := range item.DirectTableInputs {
			if _, ok := closure[dependency]; !ok {
				return fmt.Errorf("%w: inventory %s reaches unlisted table %s", ErrAuthorityCatalogInvalid, item.Name, dependency)
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM main.sqlite_schema
		WHERE type IN ('trigger','view') ORDER BY type,name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[string]struct{}, len(expected))
	for rows.Next() {
		var objectType, name, table string
		var rawSQL sql.NullString
		if err := rows.Scan(&objectType, &name, &table, &rawSQL); err != nil {
			return err
		}
		key := objectType + "\x00" + name
		item, ok := expected[key]
		if !ok {
			return fmt.Errorf("%w: unknown product schema object %s %s", ErrAuthorityCatalogInvalid, objectType, name)
		}
		if !rawSQL.Valid || table != item.AttachedTable || rawSQL.String != item.RawSQL {
			return fmt.Errorf("%w: product schema object %s bytes differ", ErrAuthorityCatalogInvalid, name)
		}
		seen[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for key, item := range expected {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("%w: product schema object %s is missing", ErrAuthorityCatalogInvalid, item.Name)
		}
	}
	return nil
}

func encodeAuthorityCatalogRow(row authoritySchemaRow) ([]byte, error) {
	frame := make([]byte, 12)
	copy(frame, []byte{'R', 'O', 'W', 0})
	values := []sql.NullString{{String: row.Type, Valid: true}, {String: row.Name, Valid: true}, {String: row.Table, Valid: true}, row.SQL}
	for _, value := range values {
		if !value.Valid {
			frame = append(frame, 0)
			continue
		}
		if !utf8.ValidString(value.String) {
			return nil, fmt.Errorf("%w: schema object contains invalid UTF-8", ErrAuthorityCatalogInvalid)
		}
		frame = append(frame, 1)
		frame = appendAuthorityUint64(frame, uint64(len(value.String)))
		frame = append(frame, value.String...)
	}
	return frame, nil
}

func appendAuthorityUint64(dst []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(dst, encoded[:]...)
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
