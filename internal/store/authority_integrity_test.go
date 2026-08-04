package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestR17AuthorityCatalogRowDigestBindsFinalOrdinal(t *testing.T) {
	st, _ := openAuthorityTestStore(t)
	ctx := context.Background()
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	receipt, err := st.AuditCollectionAuthority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertAuthorityCatalogRowDigests(t, receipt.Bytes)
}

func TestR17NonEmptyLegacyReservationWatermark(t *testing.T) {
	st, supplier := openAuthorityTestStore(t)
	ctx := context.Background()
	now := formatTime(time.Now().UTC())
	if _, err := st.db.ExecContext(ctx, `INSERT INTO collection_attempt_reservations
		(request_uuid,attempt_id,supplier_id,state,created_at,resolved_at) VALUES(?,?,?,?,?,?)`,
		"legacy-reservation", 41, supplier.ID, "committed", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	migration, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if migration.LegacyReservationCount != 1 || migration.LegacyReservationMaxID != 41 ||
		migration.LegacyReservationDigest == emptyAuthorityDigest("protocol1-reservations") {
		t.Fatalf("non-empty reservation receipt was not frozen: %+v", migration)
	}
	if _, err := st.AuditCollectionAuthority(ctx); err != nil {
		t.Fatalf("valid reservation watermark: %v", err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "reservation-owner", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("acquire protocol-2 lease: ok=%v err=%v", ok, err)
	}
	var incarnation string
	var generation int64
	if err := st.db.QueryRowContext(ctx, `SELECT supplier_incarnation_uuid,lease_generation
		FROM collection_leases WHERE supplier_id=?`, supplier.ID).Scan(&incarnation, &generation); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collection_attempt_id_sequence SET next_attempt_id=43 WHERE singleton_id=1`); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO collection_attempt_reservations
		(request_uuid,attempt_id,supplier_id,authority_protocol,supplier_incarnation_uuid,lease_generation,
		lease_schema_digest,state,created_at) VALUES(?,?,?,?,?,?,?,'reserved',?)`, "protocol-2-reservation", 42,
		supplier.ID, 2, incarnation, generation, migration.CatalogDigest[:], now); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuditCollectionAuthority(ctx); err != nil {
		t.Fatalf("protocol-2 reservation changed frozen protocol-1 digest: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE collection_attempt_reservations SET resolved_at=? WHERE request_uuid=?`,
		formatTime(time.Now().UTC().Add(time.Second)), "legacy-reservation"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuditCollectionAuthority(ctx); !errors.Is(err, ErrAuthorityCatalogInvalid) {
		t.Fatalf("reservation drift was not rejected: %v", err)
	}
}

func TestR17ReopenAuditsSequenceBeforeMigrationRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relayops.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	supplier := domain.Supplier{Name: "sequence supplier", BaseURL: "https://sequence.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "sequence-owner", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC()
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
	if err := st.RecordCollectionAttempt(ctx, &attempt, domain.CollectionDiagnostics{}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE collection_attempt_id_sequence SET next_attempt_id=1 WHERE singleton_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(path); !errors.Is(err, ErrAuthorityCatalogInvalid) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("reopen repaired sequence before audit: %v", err)
	}
}

func TestR17AutomaticSnapshotRequiresProtocol2ProofCarrier(t *testing.T) {
	st, supplier := openAuthorityTestStore(t)
	ctx := context.Background()
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: "g", Name: "g", Ratio: "1"}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "bound-catalog-hash"}}
	if err := st.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestEffectiveSnapshot(ctx, supplier.ID); !errors.Is(err, ErrAuthorityCatalogInvalid) {
		t.Fatalf("bare snapshot became automatic authority: %v", err)
	}
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "snapshot-owner", now, time.Minute); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
	if err := st.RecordCollectionAttempt(ctx, &attempt, domain.CollectionDiagnostics{}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	proofJSON := `{"response_hash":"bound-catalog-hash","complete":true}`
	if _, err := st.db.ExecContext(ctx, `INSERT INTO catalog_group_collection_proofs
		(attempt_id,supplier_id,source_group,source_semantic_hash,proof_json,observed_at,supplier_incarnation_uuid,authority_protocol)
		SELECT id,supplier_id,'g','semantic',?,?,supplier_incarnation_uuid,authority_protocol
		FROM collection_attempts WHERE id=?`, proofJSON, formatTime(now), attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestEffectiveSnapshot(ctx, supplier.ID); err != nil {
		t.Fatalf("proof-bound snapshot was rejected: %v", err)
	}
}

func TestR17Protocol2SupplierDeletionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relayops.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	supplier := domain.Supplier{Name: "retained supplier", BaseURL: "https://retained.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	legacy := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
	diagnostics := domain.CollectionDiagnostics{Requests: []domain.RequestEvidence{{EndpointType: "catalog", Method: "GET",
		SourceURL: supplier.BaseURL + "/api/models", Status: "failed", ObservedAt: now}}}
	if err := st.RecordCollectionAttempt(ctx, &legacy, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stamp := formatTime(now)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO catalog_snapshots(supplier_id,payload_json,response_hash,observed_at,total_observations,
			consecutive_observations,last_seen) VALUES(?,?,?,?,1,1,?)`, []any{supplier.ID, `{}`, "catalog", stamp, stamp}},
		{`INSERT INTO price_observations(supplier_id,entity_type,entity_key,group_name,model_name,price_json,
			evidence_json,observed_at) VALUES(?,?,?,?,?,?,?,?)`, []any{supplier.ID, "group", "g", "g", "", `{}`, `{}`, stamp}},
		{`INSERT INTO balance_observations(supplier_id,payload_json,observed_at) VALUES(?,?,?)`, []any{supplier.ID, `{}`, stamp}},
		{`INSERT INTO discoveries(supplier_id,kind,external_id,name,ratio,model_count,payload_json,first_seen_at,last_seen_at)
			VALUES(?,?,?,?,?,0,'{}',?,?)`, []any{supplier.ID, "group", "g", "g", "1", stamp, stamp}},
		{`INSERT INTO catalog_group_collection_proofs(attempt_id,supplier_id,source_group,source_semantic_hash,
			proof_json,observed_at) VALUES(?,?,?,?,?,?)`, []any{legacy.ID, supplier.ID, "g", "legacy", `{}`, stamp}},
		{`INSERT INTO collection_attempt_reservations(request_uuid,attempt_id,supplier_id,state,created_at,resolved_at)
			VALUES(?,?,?,?,?,?)`, []any{"retained-legacy-reservation", legacy.ID + 100, supplier.ID, "abandoned", stamp, stamp}},
	}
	for _, statement := range statements {
		if _, err := st.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "retained-owner", now, time.Minute); err != nil || !ok {
		t.Fatalf("acquire protocol-2 lease: ok=%v err=%v", ok, err)
	}
	v2 := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", StartedAt: now.Add(time.Second), FinishedAt: now.Add(2 * time.Second)}
	if err := st.RecordCollectionAttempt(ctx, &v2, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO catalog_group_collection_proofs
		(attempt_id,supplier_id,source_group,source_semantic_hash,proof_json,observed_at,supplier_incarnation_uuid,authority_protocol)
		SELECT id,supplier_id,'g','v2','{}',?,supplier_incarnation_uuid,authority_protocol
		FROM collection_attempts WHERE id=?`, stamp, v2.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseCollectionLease(ctx, supplier.ID, "retained-owner"); err != nil {
		t.Fatal(err)
	}

	projections := authorityDeletionReceipts(t, st)
	preview, err := st.PreviewSupplierDeletion(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteSupplierSafely(ctx, supplier.ID, supplier.Name, preview.ImpactHash); !errors.Is(err, ErrSupplierDeletionAuthorityRetentionRequired) {
		t.Fatalf("store deletion was not blocked: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM suppliers WHERE id=?`, supplier.ID); err == nil {
		t.Fatal("raw protocol-2 supplier deletion was not blocked")
	}
	assertAuthorityDeletionReceipts(t, st, projections)
	if _, err := st.AuditCollectionAuthority(ctx); err != nil {
		t.Fatalf("authority audit after blocked deletion: %v", err)
	}
	rows, err := st.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		_ = rows.Close()
		t.Fatal("blocked deletion left a foreign-key violation")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after blocked deletion: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.AuditCollectionAuthority(ctx); err != nil {
		t.Fatalf("reopened authority audit: %v", err)
	}
}

func authorityDeletionReceipts(t *testing.T, st *Store) map[string][sha256.Size]byte {
	t.Helper()
	projections := map[string][]string{
		"suppliers":                       {"id", "current_incarnation_uuid", "enabled", "updated_at"},
		"supplier_incarnation_registry":   {"incarnation_uuid", "supplier_id", "created_at", "retired_at"},
		"collection_leases":               {"supplier_id", "supplier_incarnation_uuid", "owner_id", "leased_until", "updated_at", "lease_generation", "leased_until_unix_ms", "retired"},
		"collection_attempts":             {"id", "supplier_id", "authority_protocol", "supplier_incarnation_uuid", "lease_generation", "lease_schema_digest", "commit_ordinal", "automation_eligible"},
		"collection_attempt_reservations": {"request_uuid", "attempt_id", "supplier_id", "authority_protocol", "supplier_incarnation_uuid", "lease_generation", "lease_schema_digest", "state", "created_at", "resolved_at"},
		"collection_commit_sequences":     {"supplier_id", "supplier_incarnation_uuid", "next_commit_ordinal", "retired_at"},
		"catalog_group_collection_proofs": {"attempt_id", "supplier_id", "source_group", "source_semantic_hash", "proof_json", "observed_at", "supplier_incarnation_uuid", "authority_protocol"},
		"request_evidence":                {"id", "attempt_id", "supplier_id", "endpoint_type", "status", "observed_at"},
		"catalog_snapshots":               {"id", "supplier_id", "payload_json", "response_hash", "observed_at"},
		"price_observations":              {"id", "supplier_id", "entity_type", "entity_key", "price_json", "observed_at"},
		"balance_observations":            {"id", "supplier_id", "payload_json", "observed_at"},
		"discoveries":                     {"id", "supplier_id", "kind", "external_id", "name", "ratio", "last_seen_at"},
		"activities":                      {"id", "kind", "supplier_id", "metadata_json", "created_at"},
	}
	result := make(map[string][sha256.Size]byte, len(projections))
	for table, columns := range projections {
		result[table] = mustAuthorityTableReceipt(t, st, table, "supplier-delete", columns).AggregateDigest
	}
	return result
}

func assertAuthorityDeletionReceipts(t *testing.T, st *Store, expected map[string][sha256.Size]byte) {
	t.Helper()
	current := authorityDeletionReceipts(t, st)
	for table, digest := range expected {
		if current[table] != digest {
			t.Fatalf("blocked deletion changed %s", table)
		}
	}
}

func assertAuthorityCatalogRowDigests(t *testing.T, payload []byte) {
	t.Helper()
	if len(payload) < len(authorityCatalogMagic)+8+sha256.Size || string(payload[:len(authorityCatalogMagic)]) != authorityCatalogMagic {
		t.Fatal("invalid authority catalog header")
	}
	offset := len(authorityCatalogMagic)
	count := binary.BigEndian.Uint64(payload[offset : offset+8])
	offset += 8
	for ordinal := uint64(0); ordinal < count; ordinal++ {
		start := offset
		if offset+12 > len(payload) || !bytes.Equal(payload[offset:offset+4], []byte{'R', 'O', 'W', 0}) {
			t.Fatalf("invalid catalog row %d", ordinal)
		}
		if got := binary.BigEndian.Uint64(payload[offset+4 : offset+12]); got != ordinal {
			t.Fatalf("catalog row ordinal=%d want=%d", got, ordinal)
		}
		offset += 12
		for field := 0; field < 4; field++ {
			if offset >= len(payload) {
				t.Fatalf("truncated catalog row %d", ordinal)
			}
			tag := payload[offset]
			offset++
			if tag == 0 {
				continue
			}
			if tag != 1 || offset+8 > len(payload) {
				t.Fatalf("invalid catalog field tag in row %d", ordinal)
			}
			length := binary.BigEndian.Uint64(payload[offset : offset+8])
			offset += 8
			if length > uint64(len(payload)-offset) {
				t.Fatalf("truncated catalog field in row %d", ordinal)
			}
			offset += int(length)
		}
		if offset+sha256.Size > len(payload) {
			t.Fatalf("missing catalog row digest %d", ordinal)
		}
		frame := payload[start:offset]
		digestInput := append([]byte("r30-authority-catalog-row-v1\x00"), frame...)
		expected := sha256.Sum256(digestInput)
		if !bytes.Equal(payload[offset:offset+sha256.Size], expected[:]) {
			t.Fatalf("catalog row %d digest does not bind its final ordinal", ordinal)
		}
		offset += sha256.Size
	}
	if offset+sha256.Size != len(payload) {
		t.Fatalf("catalog payload tail=%d", len(payload)-offset)
	}
}
