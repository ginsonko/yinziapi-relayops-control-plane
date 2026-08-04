package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOperatingLedgerIsImmutableAndDistinguishesUnknownFromZero(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := domain.Credential{Name: "target", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	event := OperatingLedgerEvent{EventID: "evt-1", TargetSiteID: target.ID, OccurredAt: now, Requests: 1,
		CashRevenue: "0", Currency: "cny", RevenueEvidence: "exact", CostEvidence: "missing", Source: "newapi-log"}
	if err := st.UpsertOperatingLedgerEvents(ctx, []OperatingLedgerEvent{event}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, []OperatingLedgerEvent{event}); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	changed := event
	changed.CashRevenue = "1"
	if err := st.UpsertOperatingLedgerEvents(ctx, []OperatingLedgerEvent{changed}); err == nil {
		t.Fatal("same event id with changed evidence was accepted")
	}
	summary, err := st.OperatingLedgerSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.EventCount != 1 || summary.ExactRevenueEvents != 1 || summary.ExactCostEvents != 0 {
		t.Fatalf("summary = %#v", summary)
	}
	items, err := st.ListOperatingLedgerEvents(ctx, target.ID, "", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(items) != 1 || items[0].CashRevenue != "0" || items[0].ProcurementCost != "" || items[0].Currency != "CNY" {
		t.Fatalf("ledger readback = %#v, %v", items, err)
	}
}

func TestOperatingLedgerReplayKeepsFirstDerivationWhenRemoteSourceRowIsUnchanged(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := domain.Credential{Name: "target", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	event := OperatingLedgerEvent{EventID: "newapi:1:log:remote-1", TargetSiteID: target.ID, OccurredAt: now, Requests: 1,
		UsageWeight: "500000", UsageUnit: "quota", UsageEvidence: "observed", CashRevenue: "1", Currency: "CNY",
		RevenueEvidence: "observed", CostEvidence: "missing", Source: "newapi_target_log", EvidenceRevision: "newapi-log-v1",
		Metadata: map[string]string{"source_row_hash": "raw-row-a", "quota_cash_factor": "0.000002", "unit_evidence_observed_at": now.Format(time.RFC3339Nano)}}
	if inserted, err := st.UpsertOperatingLedgerEventsCount(ctx, []OperatingLedgerEvent{event}); err != nil || inserted != 1 {
		t.Fatalf("first insert count=%d err=%v", inserted, err)
	}

	replay := event
	replay.CashRevenue = "2"
	replay.Metadata = map[string]string{"source_row_hash": "raw-row-a", "quota_cash_factor": "0.000004",
		"unit_evidence_observed_at": now.Add(time.Minute).Format(time.RFC3339Nano)}
	if inserted, err := st.UpsertOperatingLedgerEventsCount(ctx, []OperatingLedgerEvent{replay}); err != nil || inserted != 0 {
		t.Fatalf("same source row replay count=%d err=%v", inserted, err)
	}
	items, err := st.ListOperatingLedgerEvents(ctx, target.ID, "", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(items) != 1 || items[0].CashRevenue != "1" || items[0].Metadata["quota_cash_factor"] != "0.000002" {
		t.Fatalf("first immutable derivation was not preserved: %#v err=%v", items, err)
	}

	changedRemoteRow := replay
	changedRemoteRow.Metadata = map[string]string{"source_row_hash": "raw-row-b"}
	if _, err := st.UpsertOperatingLedgerEventsCount(ctx, []OperatingLedgerEvent{changedRemoteRow}); err == nil {
		t.Fatal("same remote id with a changed source row was accepted")
	}
}

func TestFillMissingUsageCostIsIdempotentAndRejectsCurrencyConflict(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := domain.Credential{Name: "cost-target", Kind: "access-token", SiteURL: "https://cost-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "cost-target", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	events := []OperatingLedgerEvent{
		{EventID: "usage-fill", TargetSiteID: target.ID, OccurredAt: now, EventKind: "usage", Requests: 1,
			CashRevenue: "2", Currency: "CNY", RevenueEvidence: "observed", CostEvidence: "missing", Source: "newapi_target_log"},
		{EventID: "usage-conflict", TargetSiteID: target.ID, OccurredAt: now, EventKind: "usage", Requests: 1,
			CashRevenue: "2", Currency: "USD", RevenueEvidence: "observed", CostEvidence: "missing", Source: "newapi_target_log"},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	changed, err := st.FillMissingUsageCost(ctx, "usage-fill", "1.2", "CNY", "estimated", map[string]string{"formula": "evidence"})
	if err != nil || !changed {
		t.Fatalf("first fill changed=%v err=%v", changed, err)
	}
	changed, err = st.FillMissingUsageCost(ctx, "usage-fill", "9.9", "CNY", "estimated", nil)
	if err != nil || changed {
		t.Fatalf("second fill changed=%v err=%v", changed, err)
	}
	if _, err := st.FillMissingUsageCost(ctx, "usage-conflict", "1", "CNY", "estimated", nil); err == nil {
		t.Fatal("currency-conflicting cost was accepted")
	}
	items, err := st.ListOperatingLedgerEvents(ctx, target.ID, "", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.EventID == "usage-fill" && (item.ProcurementCost != "1.2" || item.CostEvidence != "estimated" || item.Metadata["formula"] != "evidence") {
			t.Fatalf("filled event = %#v", item)
		}
	}
}

func TestDecisionSnapshotIsImmutableAndFailedStateTransitionLeavesNoSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	credential := domain.Credential{Name: "snapshot-target", Kind: "access-token", SiteURL: "https://snapshot-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "snapshot-target", BaseURL: "https://snapshot-target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "snapshot-supplier", BaseURL: "https://snapshot-supplier.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "snapshot-policy", SupplierID: supplier.ID, SourceGroup: "source", TargetSiteID: target.ID,
		TargetGroup: "group", RuleType: "fixed", Value: "0.1", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	newProposal := func(key string) domain.ChangeProposal {
		proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "group", ExpectedOld: "0.1",
			ProposedValue: "0.2", ExpectedHash: "hash", Status: "pending", Mode: "approval", IdempotencyKey: key,
			Calculation: domain.PriceCalculation{TargetRatio: "0.2", Formula: "target = fixed value"}}
		if err := st.CreateProposal(ctx, &proposal); err != nil {
			t.Fatal(err)
		}
		return proposal
	}

	now := time.Now().UTC().Truncate(time.Second)
	proposal := newProposal("snapshot-immutable")
	snapshot := DecisionSnapshot{ProposalID: proposal.ID, ModelVersion: "phase-a-v3", InputHash: "input-a", LifecycleStatus: "tracking",
		DecisionKind: "approve", DecidedAt: now, Payload: json.RawMessage(`{"proposal_id":1,"forecast":"frozen"}`)}
	if err := st.UpsertDecisionSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDecisionSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("exact snapshot replay was not idempotent: %v", err)
	}
	changed := snapshot
	changed.InputHash = "input-b"
	if err := st.UpsertDecisionSnapshot(ctx, changed); err == nil {
		t.Fatal("conflicting snapshot rewrite was accepted")
	}
	readback, err := st.GetDecisionSnapshot(ctx, proposal.ID)
	if err != nil || readback.InputHash != "input-a" || string(readback.Payload) != string(snapshot.Payload) {
		t.Fatalf("immutable snapshot changed: %#v, %v", readback, err)
	}

	failed := newProposal("snapshot-cas-rollback")
	failedSnapshot := snapshot
	failedSnapshot.ProposalID = failed.ID
	if err := st.BeginProposalApplication(ctx, failed.ID, []string{"approved"}, failedSnapshot); err == nil {
		t.Fatal("invalid proposal state transition succeeded")
	}
	if _, err := st.GetDecisionSnapshot(ctx, failed.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed proposal transition leaked a decision snapshot: %v", err)
	}
}

func TestDecisionOutcomeRefreshClearsStaleObservationAndExcludesRejectedDecisions(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	credential := domain.Credential{Name: "target-outcome", Kind: "access-token", SiteURL: "https://target-outcome.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target-outcome", BaseURL: "https://target-outcome.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "supplier-outcome", BaseURL: "https://supplier-outcome.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "policy-outcome", SupplierID: supplier.ID, SourceGroup: "source", TargetSiteID: target.ID,
		TargetGroup: "group", RuleType: "fixed", Value: "0.1", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	newProposal := func(status, key string) domain.ChangeProposal {
		proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "group", ExpectedOld: "0.1",
			ProposedValue: "0.2", ExpectedHash: "hash", Status: status, Mode: "approval", IdempotencyKey: key,
			Calculation: domain.PriceCalculation{TargetRatio: "0.2", Formula: "target = fixed value"}}
		if err := st.CreateProposal(ctx, &proposal); err != nil {
			t.Fatal(err)
		}
		return proposal
	}

	applied := newProposal("pending", "outcome-applied")
	now := time.Now().UTC().Truncate(time.Second)
	decision := DecisionSnapshot{ProposalID: applied.ID, ModelVersion: "phase-a-v2", InputHash: "input-hash",
		LifecycleStatus: "tracking", DecisionKind: "approve", DecidedAt: now, Payload: json.RawMessage(`{"proposal_id":1}`)}
	if err := st.BeginProposalApplication(ctx, applied.ID, []string{"pending"}, decision); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteProposalApplication(ctx, applied.ID, "0.2", "effect-hash", now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.GetDecisionSnapshot(ctx, applied.ID)
	if err != nil || snapshot.InputHash != "input-hash" || snapshot.ModelVersion != "phase-a-v2" ||
		snapshot.DecisionKind != "approve" || !snapshot.DecidedAt.Equal(now) || len(snapshot.Payload) == 0 {
		t.Fatalf("decision snapshot readback = %#v, %v", snapshot, err)
	}
	hit := true
	observed := DecisionOutcome{ProposalID: applied.ID, HorizonHours: 24, DueAt: now, Status: "observed",
		Prediction: json.RawMessage(`{"p50":"1"}`), Observation: json.RawMessage(`{"contribution":"1"}`),
		AbsoluteError: "0", AbsolutePctError: "0", IntervalHit: &hit, ObservedAt: &now}
	if err := st.UpsertDecisionOutcome(ctx, observed); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDecisionOutcome(ctx, DecisionOutcome{ProposalID: applied.ID, HorizonHours: 24, DueAt: now,
		Status: "awaiting_ledger", Prediction: json.RawMessage(`{"p50":"2"}`)}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListDecisionOutcomes(ctx, applied.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outcome readback = %#v, %v", rows, err)
	}
	if rows[0].Status != "awaiting_ledger" || len(rows[0].Observation) != 0 || rows[0].AbsoluteError != "" || rows[0].IntervalHit != nil || rows[0].ObservedAt != nil {
		t.Fatalf("stale observation survived prediction refresh: %#v", rows[0])
	}
	if err := st.UpsertDecisionOutcome(ctx, observed); err != nil {
		t.Fatal(err)
	}
	rejected := newProposal("rejected", "outcome-rejected")
	rejectedOutcome := observed
	rejectedOutcome.ProposalID = rejected.ID
	if err := st.UpsertDecisionOutcome(ctx, rejectedOutcome); err != nil {
		t.Fatal(err)
	}
	completed, err := st.ListCompletedDecisionOutcomes(ctx, 10)
	if err != nil || len(completed) != 1 || completed[0].ProposalID != applied.ID {
		t.Fatalf("completed outcomes included a non-executed decision: %#v, %v", completed, err)
	}
}
