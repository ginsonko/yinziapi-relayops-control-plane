package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestDefaultRetentionPolicyMatchesCapacityProfile(t *testing.T) {
	policy := DefaultRetentionPolicy()
	if policy.OperatingLedger != 8*24*time.Hour || policy.CatalogSnapshots != 7*24*time.Hour ||
		policy.PriceObservations != 7*24*time.Hour || policy.BalanceObservations != 7*24*time.Hour ||
		policy.Activities != 7*24*time.Hour || policy.PairingTokens != 24*time.Hour ||
		policy.CollectionAttempts != 12*time.Hour || policy.CollectionKeepLatest != 10 || policy.BatchSize != 250 {
		t.Fatalf("unexpected default retention profile: %#v", policy)
	}
}

func TestCleanupRetentionKeepsLatestAttemptsAndCurrentCatalogProof(t *testing.T) {
	ctx := context.Background()
	st := openIntegrationStore(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	supplier := domain.Supplier{Name: "retention proof", BaseURL: "https://retention-proof.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}

	observed := now.Add(-72 * time.Hour)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "source", Name: "source", RawRatio: "1", ModelIDs: []string{"model"}}},
		Models: []domain.ModelOffer{{ExternalID: "model", Name: "model", Group: "source", BillingMode: "fixed_price",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ObservedAt: observed, ResponseHash: "retained-proof-hash"}}
	current := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "partial_success", CatalogUpdated: true,
		StartedAt: observed.Add(-time.Second), FinishedAt: observed}
	diagnostics := completeCatalogDiagnostics(observed, snapshot.Evidence.ResponseHash, 2)
	diagnostics.Requests = []domain.RequestEvidence{{EndpointType: "catalog", AuthCapability: "customer_session", Method: "GET",
		SourceURL: supplier.BaseURL + "/api/pricing", HTTPStatus: 200, Status: "success", ObservedAt: observed}}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &current, diagnostics, observed.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 12; index++ {
		finished := now.Add(time.Duration(-48+index) * time.Hour)
		attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "failed", ErrorClass: "connect_timeout",
			StartedAt: finished.Add(-time.Second), FinishedAt: finished}
		diagnostics := domain.CollectionDiagnostics{Requests: []domain.RequestEvidence{{EndpointType: "catalog",
			AuthCapability: "customer_session", Method: "GET", SourceURL: supplier.BaseURL + "/api/pricing",
			Status: "failed", ErrorClass: "connect_timeout", ObservedAt: finished}}}
		if err := st.RecordCollectionAttempt(ctx, &attempt, diagnostics, finished.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	result, err := st.CleanupRetention(ctx, now, RetentionPolicy{CollectionAttempts: 24 * time.Hour,
		CollectionKeepLatest: 10, BatchSize: 250})
	if err != nil {
		t.Fatal(err)
	}
	if result.CollectionAttempts != 2 {
		t.Fatalf("deleted attempts = %d, want 2", result.CollectionAttempts)
	}
	assertTableCount(t, st, "collection_attempts", 11)
	assertTableCount(t, st, "request_evidence", 11)
	assertTableCount(t, st, "catalog_group_collection_proofs", 1)
	var retainedCurrentProof int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_group_collection_proofs
		WHERE attempt_id=? AND source_group='source'`, current.ID).Scan(&retainedCurrentProof); err != nil || retainedCurrentProof != 1 {
		t.Fatalf("retained current proof = %d, %v", retainedCurrentProof, err)
	}
	attempts, err := st.ListCollectionAttempts(ctx, supplier.ID, 20)
	if err != nil || len(attempts) != 11 || attempts[len(attempts)-1].ID != current.ID {
		t.Fatalf("retained attempts = %#v, %v", attempts, err)
	}
}

func TestCleanupRetentionBatchesLedgerAndKeepsRecentReferences(t *testing.T) {
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "retention target", Kind: "access-token", SiteURL: "https://retention-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "retention target", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	events := []OperatingLedgerEvent{
		{EventID: "old-a", TargetSiteID: target.ID, OccurredAt: now.Add(-75 * 24 * time.Hour), EventKind: "usage", Source: "test"},
		{EventID: "old-b", TargetSiteID: target.ID, OccurredAt: now.Add(-74 * 24 * time.Hour), EventKind: "usage", Source: "test"},
		{EventID: "old-referenced", TargetSiteID: target.ID, OccurredAt: now.Add(-73 * 24 * time.Hour), EventKind: "usage", Source: "test"},
		{EventID: "old-c", TargetSiteID: target.ID, OccurredAt: now.Add(-72 * 24 * time.Hour), EventKind: "usage", Source: "test"},
		{EventID: "old-d", TargetSiteID: target.ID, OccurredAt: now.Add(-71 * 24 * time.Hour), EventKind: "usage", Source: "test"},
		{EventID: "recent-reference", TargetSiteID: target.ID, OccurredAt: now.Add(-24 * time.Hour), EventKind: "other_expense",
			RelatedEventID: "old-referenced", Source: "test"},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	policy := RetentionPolicy{OperatingLedger: 60 * 24 * time.Hour, BatchSize: 2}
	for pass := 0; pass < 2; pass++ {
		result, err := st.CleanupRetention(ctx, now, policy)
		if err != nil {
			t.Fatal(err)
		}
		if result.OperatingLedger != 2 {
			t.Fatalf("pass %d deleted %d ledger rows, want 2", pass+1, result.OperatingLedger)
		}
	}
	assertTableCount(t, st, "operating_ledger_events", 2)
	var references int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operating_ledger_events
		WHERE event_id IN ('old-referenced','recent-reference')`).Scan(&references); err != nil || references != 2 {
		t.Fatalf("referenced ledger rows = %d, %v", references, err)
	}
}

func TestCleanupRetentionProtectsUnfinishedHistoricalForecastWindow(t *testing.T) {
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "retention forecast", Kind: "access-token", SiteURL: "https://retention-forecast.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "retention forecast", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "retention forecast", BaseURL: "https://retention-source.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "retention forecast", SupplierID: supplier.ID, SourceGroup: "source",
		TargetSiteID: target.ID, TargetGroup: "protected", RuleType: "fixed", Value: "1", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "protected",
		ExpectedOld: "1", ProposedValue: "2", ExpectedHash: "hash", Status: "pending", Mode: "approval",
		IdempotencyKey: "retention-forecast", Calculation: domain.PriceCalculation{TargetRatio: "2", Formula: "test"}}
	if err := st.CreateProposal(ctx, &proposal); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	effectiveAt := now.Add(-20 * 24 * time.Hour)
	snapshot := DecisionSnapshot{ProposalID: proposal.ID, ModelVersion: "legacy-30-day", InputHash: "frozen",
		LifecycleStatus: "tracking", DecisionKind: "approve", DecidedAt: effectiveAt,
		Payload: json.RawMessage(`{"forecast_ready":true,"model_pattern":"*"}`)}
	if err := st.BeginProposalApplication(ctx, proposal.ID, []string{"pending"}, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteProposalApplication(ctx, proposal.ID, "2", "effect", effectiveAt); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDecisionOutcome(ctx, DecisionOutcome{ProposalID: proposal.ID, HorizonHours: 720,
		DueAt: effectiveAt.Add(30 * 24 * time.Hour), Status: "not_due", Prediction: json.RawMessage(`{"hours":720}`)}); err != nil {
		t.Fatal(err)
	}
	events := []OperatingLedgerEvent{
		{EventID: "protected-window", TargetSiteID: target.ID, GroupName: "protected", OccurredAt: effectiveAt.Add(24 * time.Hour), Source: "test"},
		{EventID: "expired-unrelated", TargetSiteID: target.ID, GroupName: "other", OccurredAt: effectiveAt.Add(24 * time.Hour), Source: "test"},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	result, err := st.CleanupRetention(ctx, now, RetentionPolicy{OperatingLedger: 8 * 24 * time.Hour, BatchSize: 250})
	if err != nil || result.OperatingLedger != 1 {
		t.Fatalf("unfinished cleanup result=%#v err=%v", result, err)
	}
	var remaining int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operating_ledger_events WHERE event_id='protected-window'`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("unfinished forecast ledger remaining=%d err=%v", remaining, err)
	}
	observedAt := now
	if err := st.UpsertDecisionOutcome(ctx, DecisionOutcome{ProposalID: proposal.ID, HorizonHours: 720,
		DueAt: effectiveAt.Add(30 * 24 * time.Hour), Status: "observed", Prediction: json.RawMessage(`{"hours":720}`),
		Observation: json.RawMessage(`{"contribution":"1"}`), ObservedAt: &observedAt}); err != nil {
		t.Fatal(err)
	}
	result, err = st.CleanupRetention(ctx, now, RetentionPolicy{OperatingLedger: 8 * 24 * time.Hour, BatchSize: 250})
	if err != nil || result.OperatingLedger != 1 {
		t.Fatalf("terminal cleanup result=%#v err=%v", result, err)
	}
	assertTableCount(t, st, "operating_ledger_events", 0)
}

func TestCleanupRetentionRejectsNegativeLimits(t *testing.T) {
	st := openIntegrationStore(t)
	for name, policy := range map[string]RetentionPolicy{
		"batch":       {BatchSize: -1},
		"keep latest": {CollectionKeepLatest: -1},
		"duration":    {OperatingLedger: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := st.CleanupRetention(context.Background(), time.Now(), policy); err == nil {
				t.Fatal("negative retention value was accepted")
			}
		})
	}
}
