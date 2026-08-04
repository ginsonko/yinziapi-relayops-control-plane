package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestR17LeaseNoLossAndInboundInventory(t *testing.T) {
	st, supplier := openAuthorityTestStore(t)
	ctx := context.Background()
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "legacy-owner", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("acquire legacy lease: ok=%v err=%v", ok, err)
	}
	if err := st.ReleaseCollectionLease(ctx, supplier.ID, "legacy-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	receipt, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.LegacyLeaseDigest == ([32]byte{}) || receipt.FinalLeaseDigest == ([32]byte{}) ||
		receipt.StableLeaseDigest == ([32]byte{}) {
		t.Fatal("lease migration receipts must be non-zero")
	}
	if len(receipt.AuthorityObjectNames) == 0 || len(receipt.AuthorityTableClosure) < len(authoritySeedTables) {
		t.Fatalf("incomplete catalog receipt: objects=%d tables=%d", len(receipt.AuthorityObjectNames), len(receipt.AuthorityTableClosure))
	}
	if _, err := st.AuditCollectionAuthority(ctx); err != nil {
		t.Fatalf("post-activation audit: %v", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM collection_leases WHERE supplier_id=? AND lease_generation>=1`, supplier.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retained lease tombstone: count=%d err=%v", count, err)
	}
}

func TestR17InboundUnknownAndIndirectChainBlock(t *testing.T) {
	for _, testCase := range []struct {
		name string
		ddl  []string
	}{
		{name: "unrelated trigger", ddl: []string{
			`CREATE TABLE unrelated_probe(id INTEGER PRIMARY KEY, value INTEGER NOT NULL DEFAULT 0)`,
			`CREATE TRIGGER unknown_authority_reacher AFTER UPDATE ON unrelated_probe BEGIN UPDATE collection_authority_state SET next_attempt_id_floor=next_attempt_id_floor; END`,
		}},
		{name: "indirect view chain", ddl: []string{
			`CREATE VIEW unknown_authority_view AS SELECT singleton_id,authority_protocol FROM collection_authority_state`,
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			st, _ := openAuthorityTestStore(t)
			for _, ddl := range testCase.ddl {
				if _, err := st.db.Exec(ddl); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.SetReleaseAdmissionDraining(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			_, err := st.ActivateCollectionAuthorityV2(context.Background(), AuthorityMigrationOptions{})
			if !errors.Is(err, ErrAuthorityCatalogInvalid) {
				t.Fatalf("expected catalog rejection, got %v", err)
			}
			var protocol int
			if scanErr := st.db.QueryRow(`SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol); scanErr != nil || protocol != 1 {
				t.Fatalf("failed activation changed protocol: protocol=%d err=%v", protocol, scanErr)
			}
		})
	}
}

func TestR17LeaseFailureRestore(t *testing.T) {
	st, supplier := openAuthorityTestStore(t)
	ctx := context.Background()
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "owner", time.Now().UTC().Add(-2*time.Minute), time.Minute); err != nil || !ok {
		t.Fatalf("acquire expired fixture lease: ok=%v err=%v", ok, err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"before_incarnation_bind", "after_incarnation_bind", "after_lease_rebuild", "after_trigger_install", "before_activation_commit"} {
		t.Run(stage, func(t *testing.T) {
			before := mustAuthorityTableReceipt(t, st, "collection_leases", "failure-restore", []string{
				"supplier_id", "supplier_incarnation_uuid", "owner_id", "leased_until", "updated_at", "lease_generation", "leased_until_unix_ms", "retired",
			})
			_, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{FailureStage: stage})
			if !errors.Is(err, ErrAuthorityMigrationFault) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			after := mustAuthorityTableReceipt(t, st, "collection_leases", "failure-restore", []string{
				"supplier_id", "supplier_incarnation_uuid", "owner_id", "leased_until", "updated_at", "lease_generation", "leased_until_unix_ms", "retired",
			})
			if before.AggregateDigest != after.AggregateDigest {
				t.Fatal("injected migration failure changed lease multiset")
			}
			var protocol int
			if err := st.db.QueryRow(`SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol); err != nil || protocol != 1 {
				t.Fatalf("failure did not restore protocol 1: protocol=%d err=%v", protocol, err)
			}
		})
	}
}

func TestR17LegacyDrainNoNewAdmission(t *testing.T) {
	st, supplier := openAuthorityTestStore(t)
	ctx := context.Background()
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "old", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("initial acquire: ok=%v err=%v", ok, err)
	}
	status, err := st.SetReleaseAdmissionDraining(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Drained() || status.ActiveCollectionLeases != 1 {
		t.Fatalf("expected one draining lease: %+v", status)
	}
	if _, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "new", time.Now().UTC().Add(2*time.Minute), time.Minute); !errors.Is(err, ErrReleaseAdmissionDraining) {
		t.Fatalf("drain admitted new lease: %v", err)
	}
	if err := st.ReleaseCollectionLease(ctx, supplier.ID, "old"); err != nil {
		t.Fatal(err)
	}
	status, err = st.WaitForReleaseDrain(ctx, time.Millisecond)
	if err != nil || !status.Drained() {
		t.Fatalf("wait for drain: status=%+v err=%v", status, err)
	}
}

func TestR17AuthorityCatalogDriftBlocksAfterActivation(t *testing.T) {
	st, _ := openAuthorityTestStore(t)
	ctx := context.Background()
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE VIEW late_authority_view AS SELECT * FROM collection_attempts`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuditCollectionAuthority(ctx); !errors.Is(err, ErrAuthorityCatalogInvalid) {
		t.Fatalf("expected post-activation drift rejection, got %v", err)
	}
}

func TestR17ProtocolV2CarrierAndDriftBeforeArtifact(t *testing.T) {
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
	if ok, err := st.TryAcquireCollectionLease(ctx, supplier.ID, "v2-owner", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("protocol-2 acquire: ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC()
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", StartedAt: now, FinishedAt: now.Add(time.Millisecond)}
	if err := st.RecordCollectionAttempt(ctx, &attempt, domain.CollectionDiagnostics{}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var protocol, eligible int
	var incarnation string
	var generation, ordinal int64
	var digest []byte
	if err := st.db.QueryRow(`SELECT authority_protocol,supplier_incarnation_uuid,lease_generation,
		lease_schema_digest,commit_ordinal,automation_eligible FROM collection_attempts WHERE id=?`, attempt.ID).
		Scan(&protocol, &incarnation, &generation, &digest, &ordinal, &eligible); err != nil {
		t.Fatal(err)
	}
	if protocol != 2 || incarnation == "" || generation < 1 || len(digest) != 32 || ordinal != 1 || eligible != 1 {
		t.Fatalf("invalid protocol-2 carrier: protocol=%d incarnation=%q generation=%d digest=%d ordinal=%d eligible=%d",
			protocol, incarnation, generation, len(digest), ordinal, eligible)
	}
	if _, err := st.db.Exec(`CREATE VIEW late_carrier_drift AS SELECT * FROM collection_attempts`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM collection_attempts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	second := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", StartedAt: now.Add(time.Second), FinishedAt: now.Add(2 * time.Second)}
	if err := st.RecordCollectionAttempt(ctx, &second, domain.CollectionDiagnostics{}, now.Add(time.Minute)); !errors.Is(err, ErrAuthorityCatalogInvalid) {
		t.Fatalf("expected drift barrier, got %v", err)
	}
	var after int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM collection_attempts`).Scan(&after); err != nil || after != before {
		t.Fatalf("drift created artifact: before=%d after=%d err=%v", before, after, err)
	}
}

func openAuthorityTestStore(t *testing.T) (*Store, domain.Supplier) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	supplier := domain.Supplier{Name: "authority supplier", BaseURL: "https://authority.example", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	return st, supplier
}

func mustAuthorityTableReceipt(t *testing.T, st *Store, table, projection string, columns []string) sqliteTableRowsV2Receipt {
	t.Helper()
	tx, err := st.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	receipt, err := tableReceiptTx(context.Background(), tx, table, projection, "1", columns)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return receipt
}
