package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOutcomeReconcileCandidatesFilterAndRotateFairly(t *testing.T) {
	ctx := context.Background()
	st, policyID, targetID := outcomeQueueTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	newApplied := func(key string) domain.ChangeProposal {
		proposal := domain.ChangeProposal{PolicyID: policyID, TargetSiteID: targetID, TargetGroup: "group-" + key, ExpectedOld: "0.1",
			ProposedValue: "0.2", ExpectedHash: "hash", Status: "pending", Mode: "approval", IdempotencyKey: key,
			Calculation: domain.PriceCalculation{TargetRatio: "0.2", Formula: "test"}}
		if err := st.CreateProposal(ctx, &proposal); err != nil {
			t.Fatal(err)
		}
		snapshot := DecisionSnapshot{ProposalID: proposal.ID, ModelVersion: "phase-a-v3", InputHash: "input-" + key,
			LifecycleStatus: "tracking", DecisionKind: "approve", DecidedAt: now,
			Payload: json.RawMessage(`{"forecast_ready":true,"model_pattern":"gpt-*","forecasts":[]}`)}
		if err := st.BeginProposalApplication(ctx, proposal.ID, []string{"pending"}, snapshot); err != nil {
			t.Fatal(err)
		}
		if err := st.CompleteProposalApplication(ctx, proposal.ID, "0.2", "receipt-"+key, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		proposal.Status = "applied"
		return proposal
	}
	noOutcome := newApplied("no-outcome")
	notDue := newApplied("not-due")
	due := newApplied("due")
	terminal := newApplied("terminal")
	ended := newApplied("ended")
	incompatible := newApplied("incompatible")
	upsert := func(proposalID int64, status string, dueAt time.Time, exclusion string) {
		if err := st.UpsertDecisionOutcome(ctx, DecisionOutcome{ProposalID: proposalID, HorizonHours: 24, DueAt: dueAt,
			Status: status, Prediction: json.RawMessage(`{"p50":"1"}`), ExclusionReason: exclusion}); err != nil {
			t.Fatal(err)
		}
	}
	upsert(notDue.ID, "not_due", now.Add(time.Hour), "")
	upsert(due.ID, "awaiting_ledger", now.Add(-time.Minute), "")
	upsert(terminal.ID, "observed", now.Add(-time.Minute), "")
	upsert(ended.ID, "not_due", now.Add(time.Hour), "")
	upsert(incompatible.ID, "incompatible", now.Add(-time.Minute), "non_cash_basis")
	if _, err := st.db.ExecContext(ctx, `UPDATE decision_exposures SET ended_at=?,end_reason='rolled_back' WHERE proposal_id=?`,
		formatTime(now.Add(-time.Minute)), ended.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE change_proposals SET status='rolled_back' WHERE id=?`, ended.ID); err != nil {
		t.Fatal(err)
	}

	first, err := st.ListOutcomeReconcileCandidates(ctx, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != noOutcome.ID {
		t.Fatalf("first page = %#v", first)
	}
	if err := st.RecordOutcomeReconcileAttempt(ctx, noOutcome.ID, now.Add(time.Second), nil); err != nil {
		t.Fatal(err)
	}
	second, err := st.ListOutcomeReconcileCandidates(ctx, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != due.ID {
		t.Fatalf("second page did not rotate to the oldest due candidate: %#v", second)
	}
	if err := st.RecordOutcomeReconcileAttempt(ctx, due.ID, now.Add(2*time.Second), context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	third, err := st.ListOutcomeReconcileCandidates(ctx, now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || third[0].ID != ended.ID {
		t.Fatalf("early-ended candidate was starved: %#v", third)
	}
	all, err := st.ListOutcomeReconcileCandidates(ctx, now, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range all {
		if item.ID == notDue.ID || item.ID == terminal.ID || item.ID == incompatible.ID {
			t.Fatalf("ineligible proposal %d entered queue: %#v", item.ID, all)
		}
	}
}

func outcomeQueueTestStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st, targetID := financialTestStore(t)
	ctx := context.Background()
	supplier := domain.Supplier{Name: "queue-supplier", BaseURL: "https://queue-supplier.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "queue-policy", SupplierID: supplier.ID, SourceGroup: "source", SourceModelPattern: "gpt-*",
		TargetSiteID: targetID, TargetGroup: "group", RuleType: "fixed", Value: "0.2", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	return st, policy.ID, targetID
}
