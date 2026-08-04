package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

func TestCollectionAttemptsKeepTrustedSuccessSeparateFromFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "attempts", BaseURL: "https://supplier.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	success := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: base, FinishedAt: base.Add(250 * time.Millisecond), DurationMS: 250}
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, Requests: []domain.RequestEvidence{{
		EndpointType: "catalog", AuthCapability: "public", Method: "GET",
		SourceURL:  "https://supplier.example/api/pricing?access_token=must-not-persist#fragment",
		HTTPStatus: 200, Status: "success", DurationMS: 200, ResponseHash: "catalog-a", ObservedAt: base.Add(200 * time.Millisecond),
	}}, Capabilities: []domain.AuthCapabilityEvidence{{Capability: "public_catalog", Status: "verified",
		EndpointType: "catalog", HTTPStatus: 200, LastAttemptAt: base.Add(200 * time.Millisecond)}}}
	if err := st.RecordCollectionAttempt(ctx, &success, diagnostics, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	failure := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", ErrorClass: "auth_expired",
		ErrorMessage: "Authorization: Bearer secret-token", StartedAt: base.Add(time.Minute),
		FinishedAt: base.Add(time.Minute + 300*time.Millisecond), DurationMS: 300}
	diagnostics = domain.CollectionDiagnostics{Requests: []domain.RequestEvidence{{EndpointType: "balance",
		AuthCapability: "customer_session", Method: "GET", SourceURL: "https://supplier.example/api/user/self?token=secret",
		HTTPStatus: 401, Status: "failed", ErrorClass: "auth_expired", ErrorMessage: "Bearer secret-token",
		DurationMS: 300, ObservedAt: failure.FinishedAt}}, Capabilities: []domain.AuthCapabilityEvidence{{
		Capability: "public_catalog", Status: "rejected", EndpointType: "catalog", HTTPStatus: 401,
		ErrorClass: "auth_expired", LastAttemptAt: failure.FinishedAt,
	}}}
	if err := st.RecordCollectionAttempt(ctx, &failure, diagnostics, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	stored, err := st.GetSupplier(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastAttemptAt == nil || !stored.LastAttemptAt.Equal(failure.FinishedAt) {
		t.Fatalf("last attempt = %v", stored.LastAttemptAt)
	}
	if stored.LastSuccessAt == nil || !stored.LastSuccessAt.Equal(success.FinishedAt) {
		t.Fatalf("last success was overwritten: %v", stored.LastSuccessAt)
	}
	if stored.LastCollectedAt == nil || !stored.LastCollectedAt.Equal(success.FinishedAt) {
		t.Fatalf("legacy trusted collection time was overwritten: %v", stored.LastCollectedAt)
	}
	if stored.LastErrorClass != "auth_expired" || stored.LastFailureDuration == nil || *stored.LastFailureDuration != 300 {
		t.Fatalf("failure summary = %#v", stored)
	}

	attempts, err := st.ListCollectionAttempts(ctx, supplier.ID, 10)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts = %#v, %v", attempts, err)
	}
	if got := attempts[0].Requests[0].SourceURL; got != "https://supplier.example/api/user/self" {
		t.Fatalf("sanitized request URL = %q", got)
	}
	var persistedMessages string
	rows, err := st.db.QueryContext(ctx, `SELECT error_message FROM collection_attempts UNION ALL SELECT error_message FROM request_evidence`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		persistedMessages += value
	}
	_ = rows.Close()
	if strings.Contains(persistedMessages, "secret-token") {
		t.Fatalf("diagnostic persistence leaked a token: %q", persistedMessages)
	}

	capabilities, err := st.ListAuthCapabilities(ctx, supplier.ID)
	if err != nil || len(capabilities) != 1 || capabilities[0].Status != "rejected" || capabilities[0].VerifiedAt == nil {
		t.Fatalf("capabilities = %#v, %v", capabilities, err)
	}
}

func TestConsecutiveCollectionBusyFailuresCountsOnlyBusyTail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "busy tail", BaseURL: "https://busy-tail.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	record := func(status, errorClass string) {
		now := time.Now().UTC()
		attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: status, ErrorClass: errorClass,
			StartedAt: now.Add(-time.Millisecond), FinishedAt: now, DurationMS: 1}
		if err := st.RecordCollectionAttempt(ctx, &attempt, domain.CollectionDiagnostics{}, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	record("failed", "database_busy")
	record("failed", "database_busy")
	if count, err := st.ConsecutiveCollectionBusyFailures(ctx, supplier.ID, 3); err != nil || count != 2 {
		t.Fatalf("busy tail count=%d err=%v, want 2", count, err)
	}
	record("failed", "connect_timeout")
	if count, err := st.ConsecutiveCollectionBusyFailures(ctx, supplier.ID, 3); err != nil || count != 0 {
		t.Fatalf("network failure did not reset busy tail: count=%d err=%v", count, err)
	}
	record("failed", "database_busy")
	if count, err := st.ConsecutiveCollectionBusyFailures(ctx, supplier.ID, 3); err != nil || count != 1 {
		t.Fatalf("new busy tail count=%d err=%v, want 1", count, err)
	}
}

func TestUnknownBalanceFieldsRemainUnknown(t *testing.T) {
	state := domain.BalanceState{Amount: "", Currency: "", RawValue: "30965000", RawUnit: "quota"}
	if state.Amount != "" || state.NormalizationDivisor != "" || state.NormalizationFormula != "" {
		t.Fatalf("unknown normalized balance was fabricated: %#v", state)
	}
}

func TestCommitCollectionSuccessPersistsPerGroupCompletenessProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "proof supplier", BaseURL: "https://proof.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "video", Name: "video", RawRatio: "0.4",
			RechargeValueFactor: "1", ModelIDs: []string{"jimeng"}}},
		Models: []domain.ModelOffer{{ExternalID: "jimeng", Name: "jimeng", Group: "video",
			BillingMode: "fixed_price", Currency: "USD",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "0.12"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "catalog-proof-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, CatalogProof: &domain.CatalogCollectionProofBasis{
		AuthoritativeEndpoint: "newapi://pricing", AuthenticationRequired: false, Authenticated: true,
		SourceRevision: "catalog-proof-hash", Pages: []domain.CatalogCollectionPage{{PageNumber: 1,
			ItemCount: 2, Terminal: true}}, RawItemCount: 2, ParsedItemCount: 2, UniqueItemCount: 2,
		ObservedAt: now,
	}}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatalf("CommitCollectionSuccess: %v", err)
	}
	proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "video", 10)
	if err != nil || len(proofs) != 1 || !proofs[0].Complete || proofs[0].AttemptID != strconv.FormatInt(attempt.ID, 10) ||
		proofs[0].SourceSemanticHash == "" || proofs[0].RawItemCount != 2 {
		t.Fatalf("stored proofs = %#v, %v", proofs, err)
	}
}

func TestFindCatalogGroupCollectionProofSurvivesFailedAttemptWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "proof fallback", BaseURL: "https://proof-fallback.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "64", Name: "source", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"model"}}},
		Models: []domain.ModelOffer{{ExternalID: "model", Name: "model", Group: "64",
			BillingMode: "fixed_price", Currency: "USD",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "trusted-snapshot-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
		completeCatalogDiagnostics(now, snapshot.Evidence.ResponseHash, 2), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "64", 12)
	if err != nil || len(proofs) != 1 || !proofs[0].Complete {
		t.Fatalf("initial proofs = %#v, %v", proofs, err)
	}
	semanticHash := proofs[0].SourceSemanticHash
	for index := 0; index < 20; index++ {
		failedAt := now.Add(time.Duration(index+1) * time.Minute)
		failure := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", ErrorClass: "connect_timeout",
			StartedAt: failedAt.Add(-time.Second), FinishedAt: failedAt}
		if err := st.RecordCollectionAttempt(ctx, &failure, domain.CollectionDiagnostics{}, failedAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	bounded, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "64", 12)
	if err != nil || len(bounded) != 12 {
		t.Fatalf("bounded proofs = %#v, %v", bounded, err)
	}
	for _, proof := range bounded {
		if proof.ResponseHash == snapshot.Evidence.ResponseHash {
			t.Fatal("trusted proof unexpectedly remained in the bounded failed-attempt window")
		}
	}
	found, err := st.FindCatalogGroupCollectionProof(ctx, supplier.ID, "64", semanticHash,
		snapshot.Evidence.ResponseHash)
	if err != nil || !found.Complete || found.AttemptID != strconv.FormatInt(attempt.ID, 10) {
		t.Fatalf("exact proof = %#v, %v", found, err)
	}
	if _, err := st.FindCatalogGroupCollectionProof(ctx, supplier.ID, "64", semanticHash, "other-hash"); !IsNotFound(err) {
		t.Fatalf("mismatched response hash error = %v", err)
	}
}

func TestCatalogProofCountMismatchFailsClosedWithoutRejectingCatalog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "incomplete proof", BaseURL: "https://incomplete.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 5, 5, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "g", Name: "g", RawRatio: "1", ModelIDs: []string{"m"}}},
		Models: []domain.ModelOffer{{ExternalID: "m", Name: "m", Group: "g", BillingMode: "fixed_price",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "incomplete-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, CatalogProof: &domain.CatalogCollectionProofBasis{
		AuthoritativeEndpoint: "newapi://pricing", Authenticated: true, SourceRevision: "incomplete-hash",
		Pages:        []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: 1, Terminal: true}},
		RawItemCount: 1, ParsedItemCount: 1, UniqueItemCount: 1, ObservedAt: now,
	}}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatalf("incomplete proof rejected otherwise valid catalog: %v", err)
	}
	proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "g", 10)
	if err != nil || len(proofs) != 1 || proofs[0].Complete ||
		!strings.Contains(strings.Join(proofs[0].Reasons, ","), "snapshot_parsed_count_mismatch") {
		t.Fatalf("incomplete proof = %#v, %v", proofs, err)
	}
	if _, err := st.FindCatalogGroupCollectionProof(ctx, supplier.ID, "g", proofs[0].SourceSemanticHash,
		"incomplete-hash"); !errors.Is(err, ErrCatalogGroupProofIncomplete) {
		t.Fatalf("incomplete exact proof error = %v", err)
	}
}

func TestCommitCollectionSuccessRetriesShortSQLiteBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy-retry.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "busy retry", BaseURL: "https://busy.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	st.db.SetMaxOpenConns(1)
	st.db.SetMaxIdleConns(1)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(45 * time.Millisecond)
		_, releaseErr := locker.Exec(`COMMIT`)
		released <- releaseErr
	}()
	now := time.Now().UTC()
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "g", Name: "g", RawRatio: "1", ModelIDs: []string{"m"}}},
		Models: []domain.ModelOffer{{ExternalID: "m", Name: "m", Group: "g", BillingMode: "fixed_price",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "busy-retry-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
		domain.CollectionDiagnostics{CatalogSucceeded: true}, now.Add(time.Minute)); err != nil {
		t.Fatalf("bounded busy retry failed: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func TestRecordCollectionAttemptRetriesShortSQLiteBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt-busy-retry.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "attempt busy retry", BaseURL: "https://attempt-busy.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	st.db.SetMaxOpenConns(1)
	st.db.SetMaxIdleConns(1)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(45 * time.Millisecond)
		_, releaseErr := locker.Exec(`COMMIT`)
		released <- releaseErr
	}()
	now := time.Now().UTC()
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", ErrorClass: "network_timeout",
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	if err := st.RecordCollectionAttempt(ctx, &attempt, domain.CollectionDiagnostics{}, now.Add(time.Minute)); err != nil {
		t.Fatalf("bounded attempt busy retry failed: %v", err)
	}
	if attempt.ID == 0 {
		t.Fatal("retried collection attempt did not receive an identity")
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

func TestCatalogGroupStateProofsUseOnlyEnabledNativeCompleteMirrorBindings(t *testing.T) {
	ctx := context.Background()
	st, supplier, target := openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "group-state.db"))
	createPolicy := func(sourceGroup, targetGroup string, enabled bool) mirror.Policy {
		policy := newMirrorTestPolicy(t, supplier.ID, target.ID, targetGroup)
		policy.SourceGroup = sourceGroup
		policy.Enabled = enabled
		if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
			t.Fatalf("create mirror policy %s/%s: %v", sourceGroup, targetGroup, err)
		}
		return policy
	}
	createPolicy("missing", "retail-a", true)
	createPolicy("missing", "retail-b", true)
	createPolicy("disabled", "retail-disabled", false)
	legacy, err := mirror.MigrateLegacyPolicy(newTestUUID(t), domain.PricingPolicy{ID: 999, SupplierID: supplier.ID,
		SourceGroup: "legacy", SourceModelPattern: "*", SourceBasis: "group_ratio", TargetSiteID: target.ID,
		TargetGroup: "retail-legacy", RuleType: "same", Enabled: true, ExecutionMode: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := formatTime(time.Now().UTC())
	if _, err := st.db.ExecContext(ctx, `INSERT INTO mirror_policies
		(policy_uuid,policy_version,schema_version,legacy_policy_id,origin,supplier_id,source_group,target_site_id,
		target_group,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, legacy.PolicyUUID,
		legacy.PolicyVersion, legacy.SchemaVersion, legacy.LegacyPolicyID, "legacy_compat", legacy.SupplierID,
		legacy.SourceGroup, legacy.TargetSiteID, legacy.TargetGroup, string(legacyPayload), createdAt, createdAt); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{
			{ExternalID: "present", Name: "present", RawRatio: "1", RechargeValueFactor: "1", ModelIDs: []string{"m"}},
			{ExternalID: "empty", Name: "empty", RawRatio: "1", RechargeValueFactor: "1"},
		},
		Models: []domain.ModelOffer{{ExternalID: "m", Name: "m", Group: "present",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "group-state-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	diagnostics := completeCatalogDiagnostics(now, "group-state-hash", 3)
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	rows, err := st.db.QueryContext(ctx, `SELECT source_group,COUNT(*) FROM catalog_group_collection_proofs GROUP BY source_group`)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for rows.Next() {
		var sourceGroup string
		var count int
		if err := rows.Scan(&sourceGroup, &count); err != nil {
			t.Fatal(err)
		}
		counts[sourceGroup] = count
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(counts) != 3 || counts["present"] != 1 || counts["empty"] != 1 || counts["missing"] != 1 ||
		counts["disabled"] != 0 || counts["legacy"] != 0 {
		t.Fatalf("group-state proof counts = %#v", counts)
	}
	missing, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "missing", 10)
	if err != nil || len(missing) != 1 || !missing[0].Complete || missing[0].SourceState != mirror.SourceStateMissing {
		t.Fatalf("missing proofs = %#v, %v", missing, err)
	}
	empty, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "empty", 10)
	if err != nil || len(empty) != 1 || !empty[0].Complete || empty[0].SourceState != mirror.SourceStateEmpty ||
		empty[0].SourceSemanticHash == missing[0].SourceSemanticHash {
		t.Fatalf("empty proofs = %#v, %v", empty, err)
	}
	for _, sourceGroup := range []string{"disabled", "legacy", "unbound"} {
		proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, sourceGroup, 10)
		if err != nil || len(proofs) != 1 || proofs[0].Complete || proofs[0].SourceState != "" ||
			!strings.Contains(strings.Join(proofs[0].Reasons, ","), "collection_attempt_without_group_proof") {
			t.Fatalf("%s unexpectedly received synthetic proof: %#v, %v", sourceGroup, proofs, err)
		}
	}
}

func TestFailedCollectionAttemptBreaksStoredMissingProofSequence(t *testing.T) {
	ctx := context.Background()
	st, supplier, target := openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "missing-sequence.db"))
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	policy.SourceGroup = "missing"
	if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	commitMissing := func(index int) {
		now := base.Add(time.Duration(index) * time.Minute)
		snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
			Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "missing-" + strconv.Itoa(index)}}
		attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
			StartedAt: now.Add(-time.Second), FinishedAt: now}
		if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
			completeCatalogDiagnostics(now, snapshot.Evidence.ResponseHash, 0), now.Add(time.Minute)); err != nil {
			t.Fatalf("commit missing attempt %d: %v", index, err)
		}
	}
	commitMissing(1)
	commitMissing(2)
	failureAt := base.Add(3 * time.Minute)
	failure := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", ErrorClass: "auth_expired",
		StartedAt: failureAt.Add(-time.Second), FinishedAt: failureAt}
	if err := st.RecordCollectionAttempt(ctx, &failure, domain.CollectionDiagnostics{}, failureAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	commitMissing(4)

	proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "missing", 10)
	if err != nil || len(proofs) != 4 || proofs[0].SourceState != mirror.SourceStateMissing || proofs[1].Complete {
		t.Fatalf("stored sequence = %#v, %v", proofs, err)
	}
	result, err := mirror.ConfirmEquivalentChange(mirror.ChangeGroupDisappearance, mirror.SourceStateMissing,
		proofs[0].SourceSemanticHash, proofs, base.Add(5*time.Minute), time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 1 {
		t.Fatalf("failed attempt did not break stored missing sequence = %#v, %v", result, err)
	}
}

func TestCatalogGroupProofFailureRollsBackSnapshotAttemptAndEarlierProof(t *testing.T) {
	ctx := context.Background()
	st, supplier, target := openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "group-proof-rollback.db"))
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	policy.SourceGroup = "z-missing"
	if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_second_group_proof BEFORE INSERT ON catalog_group_collection_proofs
		WHEN NEW.source_group='z-missing' BEGIN SELECT RAISE(ABORT, 'injected group proof failure'); END`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "a-present", RawRatio: "1", ModelIDs: []string{"m"}}},
		Models: []domain.ModelOffer{{ExternalID: "m", Group: "a-present",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "rollback-hash"}}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
		completeCatalogDiagnostics(now, "rollback-hash", 2), now.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "injected group proof failure") {
		t.Fatalf("injected proof failure = %v", err)
	}
	if attempt.ID != 0 {
		t.Fatalf("rolled-back attempt leaked ID %d", attempt.ID)
	}
	for _, table := range []string{"catalog_snapshots", "collection_attempts", "catalog_group_collection_proofs"} {
		assertTableCount(t, st, table, 0)
	}
}

func TestCatalogGroupProofJoinedIdentityMutationsBecomeNewestBarriers(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*testing.T, context.Context, *Store, domain.Supplier, int64)
		expectedReasons []string
	}{
		{name: "attempt status", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE collection_attempts SET status='failed' WHERE id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}, expectedReasons: []string{"collection_attempt_not_success"}},
		{name: "attempt catalog flag", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE collection_attempts SET catalog_updated=0 WHERE id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}, expectedReasons: []string{"collection_attempt_catalog_not_updated"}},
		{name: "attempt status and catalog flag", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE collection_attempts SET status='failed',catalog_updated=0 WHERE id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}, expectedReasons: []string{"collection_attempt_catalog_not_updated", "collection_attempt_not_success"}},
		{name: "invalid proof json", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET proof_json='{invalid' WHERE attempt_id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "json attempt id", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			mutateStoredProofJSON(t, ctx, st, newestID, func(proof *mirror.CollectionProof) { proof.AttemptID = "999" })
		}},
		{name: "json attempt sequence", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			mutateStoredProofJSON(t, ctx, st, newestID, func(proof *mirror.CollectionProof) { proof.AttemptSequence = 999 })
		}},
		{name: "proof row attempt id", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			olderID := newestID - 1
			if _, err := st.db.ExecContext(ctx, `DELETE FROM catalog_group_collection_proofs WHERE attempt_id=? AND source_group='present'`, olderID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET attempt_id=? WHERE attempt_id=? AND source_group='present'`, olderID, newestID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "proof row supplier", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			other := domain.Supplier{Name: "other proof supplier", BaseURL: "https://other-proof.example", Enabled: true}
			if err := st.CreateSupplier(ctx, &other); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET supplier_id=? WHERE attempt_id=?`, other.ID, newestID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "proof row source group", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET source_group='other' WHERE attempt_id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "proof row semantic hash", mutate: func(t *testing.T, ctx context.Context, st *Store, _ domain.Supplier, newestID int64) {
			if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET source_semantic_hash='mutated' WHERE attempt_id=?`, newestID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			st, supplier, _ := openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "proof-identity.db"))
			base := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
			var newestID int64
			for index := 1; index <= 4; index++ {
				now := base.Add(time.Duration(index) * time.Minute)
				revision := "identity-" + strconv.Itoa(index)
				snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
					Groups: []domain.GroupOffer{{ExternalID: "present", RawRatio: "1", ModelIDs: []string{"m"}}},
					Models: []domain.ModelOffer{{ExternalID: "m", Group: "present",
						PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
					Evidence: domain.Evidence{ObservedAt: now, ResponseHash: revision}}
				attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
					StartedAt: now.Add(-time.Second), FinishedAt: now}
				if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
					completeCatalogDiagnostics(now, revision, 2), now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				newestID = attempt.ID
			}
			testCase.mutate(t, ctx, st, supplier, newestID)
			proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplier.ID, "present", 10)
			if err != nil || len(proofs) != 4 || proofs[0].AttemptSequence != newestID || proofs[0].Complete {
				t.Fatalf("mutated proof projection = %#v, %v", proofs, err)
			}
			for _, reason := range testCase.expectedReasons {
				if !strings.Contains(strings.Join(proofs[0].Reasons, ","), reason) {
					t.Fatalf("mutated proof reasons = %#v, want %q", proofs[0].Reasons, reason)
				}
			}
			semanticHash := ""
			for _, proof := range proofs {
				if proof.Complete {
					semanticHash = proof.SourceSemanticHash
					break
				}
			}
			if semanticHash == "" {
				t.Fatalf("older proof lost semantic hash: %#v", proofs)
			}
			result, err := mirror.ConfirmEquivalentChange(mirror.ChangeRemoval, mirror.SourceStatePresent,
				semanticHash, proofs, base.Add(6*time.Minute), time.Hour)
			if err != nil || result.Eligible || result.Confirmed != 0 {
				t.Fatalf("mutated newest proof did not block removal = %#v, %v", result, err)
			}
		})
	}
}

func TestCollectionSuccessFailsClosedAndRollsBackWhenPolicyBindingWouldDisappearFromSQLFilter(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(context.Context, *Store, mirror.Policy) error
		expectedErr string
	}{
		{name: "supplier identity", expectedErr: "supplier_id", mutate: func(ctx context.Context, st *Store, policy mirror.Policy) error {
			_, err := st.db.ExecContext(ctx, `UPDATE mirror_policies SET supplier_id=supplier_id+1000 WHERE policy_uuid=?`, policy.PolicyUUID)
			return err
		}},
		{name: "origin identity", expectedErr: "origin", mutate: func(ctx context.Context, st *Store, policy mirror.Policy) error {
			_, err := st.db.ExecContext(ctx, `UPDATE mirror_policies SET origin='legacy_compat' WHERE policy_uuid=?`, policy.PolicyUUID)
			return err
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			st, supplier, target := openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "policy-filter-bypass.db"))
			policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
			policy.SourceGroup = "present"
			if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := testCase.mutate(ctx, st, policy); err != nil {
				t.Fatal(err)
			}

			now := time.Date(2026, 7, 28, 10, 30, 0, 0, time.UTC)
			snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
				Groups: []domain.GroupOffer{{ExternalID: "present", RawRatio: "1", ModelIDs: []string{"m"}}},
				Models: []domain.ModelOffer{{ExternalID: "m", Group: "present",
					PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
				Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "policy-filter-bypass"}}
			attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
				StartedAt: now.Add(-time.Second), FinishedAt: now}
			if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt,
				completeCatalogDiagnostics(now, snapshot.Evidence.ResponseHash, 2), now.Add(time.Minute)); err == nil ||
				!strings.Contains(err.Error(), testCase.expectedErr) {
				t.Fatalf("corrupt policy commit error = %v, want %q", err, testCase.expectedErr)
			}
			if attempt.ID != 0 {
				t.Fatalf("rolled-back attempt leaked ID %d", attempt.ID)
			}
			for _, table := range []string{"catalog_snapshots", "collection_attempts", "catalog_group_collection_proofs"} {
				assertTableCount(t, st, table, 0)
			}
		})
	}
}

func mutateStoredProofJSON(t *testing.T, ctx context.Context, st *Store, attemptID int64,
	mutate func(*mirror.CollectionProof)) {
	t.Helper()
	var payload string
	if err := st.db.QueryRowContext(ctx, `SELECT proof_json FROM catalog_group_collection_proofs
		WHERE attempt_id=? AND source_group='present'`, attemptID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var proof mirror.CollectionProof
	if err := json.Unmarshal([]byte(payload), &proof); err != nil {
		t.Fatal(err)
	}
	mutate(&proof)
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE catalog_group_collection_proofs SET proof_json=?
		WHERE attempt_id=? AND source_group='present'`, string(encoded), attemptID); err != nil {
		t.Fatal(err)
	}
}

func completeCatalogDiagnostics(now time.Time, revision string, itemCount int) domain.CollectionDiagnostics {
	return domain.CollectionDiagnostics{CatalogSucceeded: true, CatalogProof: &domain.CatalogCollectionProofBasis{
		AuthoritativeEndpoint: "newapi://pricing", Authenticated: true, SourceRevision: revision,
		Pages:        []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: itemCount, Terminal: true}},
		RawItemCount: itemCount, ParsedItemCount: itemCount, UniqueItemCount: itemCount, ObservedAt: now,
	}}
}

func TestAuthCapabilitiesKeepEndpointOutcomesIndependent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "endpoint capabilities", BaseURL: "https://capabilities.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "partial_success", StartedAt: base,
		FinishedAt: base.Add(time.Second), CatalogUpdated: true, BalanceUpdated: true}
	diagnostics := domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{
		{Capability: "customer_session", Status: "verified", EndpointType: "balance", HTTPStatus: 200, LastAttemptAt: base},
		{Capability: "customer_session", Status: "expired_or_invalid", EndpointType: "usage", HTTPStatus: 401,
			ErrorClass: "auth_expired", LastAttemptAt: base.Add(time.Second)},
	}}
	if err := st.RecordCollectionAttempt(ctx, &attempt, diagnostics, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListAuthCapabilities(ctx, supplier.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("capabilities=%#v err=%v", items, err)
	}
	statuses := map[string]string{}
	for _, item := range items {
		statuses[item.EndpointType] = item.Status
	}
	if statuses["balance"] != "verified" || statuses["usage"] != "expired_or_invalid" {
		t.Fatalf("independent endpoint statuses=%#v", statuses)
	}
}

func TestCommitCollectionSuccessRollsBackEveryArtifactAfterLateWriteFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "atomic collection", BaseURL: "https://atomic.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	baseline := historySnapshot(supplier.ID, "catalog-a", "0.05", "10", base)
	baselineAttempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		BalanceUpdated: true, StartedAt: base, FinishedAt: base.Add(time.Second), DurationMS: 1000}
	baselineDiagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, BalanceSucceeded: true,
		Requests: []domain.RequestEvidence{{EndpointType: "catalog", AuthCapability: "public_catalog", Method: "GET",
			SourceURL: "https://atomic.example/api/pricing", HTTPStatus: 200, Status: "success", ObservedAt: base}},
		Capabilities: []domain.AuthCapabilityEvidence{{Capability: "public_catalog", Status: "verified", EndpointType: "catalog", HTTPStatus: 200}}}
	changed, err := st.CommitCollectionSuccess(ctx, baseline, &baselineAttempt, baselineDiagnostics, base.Add(time.Minute))
	if err != nil || changed || baselineAttempt.ID == 0 {
		t.Fatalf("baseline commit = changed:%v attempt:%#v error:%v", changed, baselineAttempt, err)
	}

	if _, err := st.db.Exec(`CREATE TRIGGER fail_atomic_capability BEFORE INSERT ON auth_capabilities
		BEGIN SELECT RAISE(ABORT, 'injected late capability failure'); END`); err != nil {
		t.Fatal(err)
	}
	updated := historySnapshot(supplier.ID, "catalog-b", "0.08", "8", base.Add(2*time.Minute))
	failedAttempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "success", CatalogUpdated: true,
		BalanceUpdated: true, StartedAt: base.Add(2 * time.Minute), FinishedAt: base.Add(2*time.Minute + time.Second), DurationMS: 1000}
	updatedDiagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, BalanceSucceeded: true,
		Requests: []domain.RequestEvidence{{EndpointType: "catalog", AuthCapability: "business_api_key", Method: "GET",
			SourceURL: "https://atomic.example/v1/models", HTTPStatus: 200, Status: "success", ObservedAt: base.Add(2 * time.Minute)}},
		Capabilities: []domain.AuthCapabilityEvidence{{Capability: "business_api_key", Status: "verified", EndpointType: "catalog", HTTPStatus: 200}}}
	if _, err := st.CommitCollectionSuccess(ctx, updated, &failedAttempt, updatedDiagnostics, base.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "injected late capability failure") {
		t.Fatalf("late injected failure = %v", err)
	}
	if failedAttempt.ID != 0 || failedAttempt.Requests != nil {
		t.Fatalf("rolled-back attempt leaked staged identity: %#v", failedAttempt)
	}
	latest, err := st.LatestSnapshot(ctx, supplier.ID)
	if err != nil || latest.Evidence.ResponseHash != "catalog-a" || latest.Groups[0].Ratio != "0.05" {
		t.Fatalf("latest snapshot moved after rollback: %#v error=%v", latest, err)
	}
	balance, err := st.LatestBalance(ctx, supplier.ID)
	if err != nil || balance.Available != "10" {
		t.Fatalf("latest balance moved after rollback: %#v error=%v", balance, err)
	}
	discoveries, err := st.ListDiscoveries(ctx, false, 10)
	if err != nil || len(discoveries) != 1 || discoveries[0].Ratio != "0.05" || !discoveries[0].Baseline {
		t.Fatalf("discoveries moved after rollback: %#v error=%v", discoveries, err)
	}
	attempts, err := st.ListCollectionAttempts(ctx, supplier.ID, 10)
	if err != nil || len(attempts) != 1 || attempts[0].ID != baselineAttempt.ID {
		t.Fatalf("attempt history moved after rollback: %#v error=%v", attempts, err)
	}
	capabilities, err := st.ListAuthCapabilities(ctx, supplier.ID)
	if err != nil || len(capabilities) != 1 || capabilities[0].Capability != "public_catalog" {
		t.Fatalf("capabilities moved after rollback: %#v error=%v", capabilities, err)
	}
	storedSupplier, err := st.GetSupplier(ctx, supplier.ID)
	if err != nil || storedSupplier.LastSuccessAt == nil || !storedSupplier.LastSuccessAt.Equal(baselineAttempt.FinishedAt) {
		t.Fatalf("supplier success state moved after rollback: %#v error=%v", storedSupplier, err)
	}
	assertTableCount(t, st, "catalog_snapshots", 1)
	assertTableCount(t, st, "price_observations", 2)
	assertTableCount(t, st, "balance_observations", 1)
	assertTableCount(t, st, "collection_attempts", 1)
	assertTableCount(t, st, "request_evidence", 1)

	if _, err := st.db.Exec(`DROP TRIGGER fail_atomic_capability`); err != nil {
		t.Fatal(err)
	}
	changed, err = st.CommitCollectionSuccess(ctx, updated, &failedAttempt, updatedDiagnostics, base.Add(3*time.Minute))
	if err != nil || !changed || failedAttempt.ID == 0 || len(failedAttempt.Requests) != 1 {
		t.Fatalf("retry commit = changed:%v attempt:%#v error:%v", changed, failedAttempt, err)
	}
	latest, err = st.LatestSnapshot(ctx, supplier.ID)
	if err != nil || latest.Evidence.ResponseHash != "catalog-b" {
		t.Fatalf("retry did not publish latest snapshot: %#v error=%v", latest, err)
	}
	assertTableCount(t, st, "catalog_snapshots", 2)
	assertTableCount(t, st, "price_observations", 4)
	assertTableCount(t, st, "balance_observations", 2)
	assertTableCount(t, st, "collection_attempts", 2)
	assertTableCount(t, st, "request_evidence", 2)
}
