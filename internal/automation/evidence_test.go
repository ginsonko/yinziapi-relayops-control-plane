package automation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestFirstPublicObservationCanBeSimulatedAndManuallyApplied(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "public-first", "public_api", base)

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatalf("Simulate first public observation: %v", err)
	}
	if simulation.Evidence.RequiredObservations != 2 || simulation.Evidence.ConsecutiveObservations != 1 ||
		simulation.Evidence.RemainingObservations != 1 || simulation.Evidence.AutoEligible || simulation.Calculation.AutomationEligible {
		t.Fatalf("first public evidence = %#v calculation=%#v", simulation.Evidence, simulation.Calculation)
	}
	if !strings.Contains(simulation.Calculation.AutomationReason, "还需连续 1 次") {
		t.Fatalf("automation reason = %q", simulation.Calculation.AutomationReason)
	}
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || proposal.Mode != "approval" || proposal.Status != "pending" {
		t.Fatalf("first public proposal = %#v, %v", proposal, err)
	}
	applied, err := fixture.service.Apply(context.Background(), proposal.ID)
	if err != nil || applied.Status != "applied" {
		t.Fatalf("manual apply on first observation = %#v, %v", applied, err)
	}
	_, writes := fixture.target.value("economy")
	if writes != 1 {
		t.Fatalf("manual apply writes = %d, want 1", writes)
	}
}

func TestSecondPublicObservationPromotesPendingProposalAndEnablesReconcile(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	setTargetWriteMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "public-stable", "public_api", base)

	first, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || first.Mode != "approval" {
		t.Fatalf("first proposal = %#v, %v", first, err)
	}
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	if _, writes := fixture.target.value("economy"); writes != 0 {
		t.Fatalf("first observation auto-wrote target %d times", writes)
	}

	saveAutomationEvidence(t, fixture, "public-stable", "public_api", base.Add(time.Second))
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	promoted, err := fixture.store.GetProposal(context.Background(), first.ID)
	if err != nil || promoted.Mode != "auto" || promoted.Status != "applied" || !promoted.Calculation.AutomationEligible {
		t.Fatalf("promoted proposal = %#v, %v", promoted, err)
	}
	if _, writes := fixture.target.value("economy"); writes != 1 {
		t.Fatalf("confirmed public price writes = %d, want 1", writes)
	}
}

func TestGlobalAutomationSwitchBlocksUnattendedReconcile(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	setTargetWriteMode(t, fixture, "auto")
	saveAutomationEvidence(t, fixture, "authenticated-stable", "authenticated_api", time.Now().UTC().Add(time.Minute))

	fixture.service.SetAutomaticWritesEnabled(false)
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	if _, writes := fixture.target.value("economy"); writes != 0 {
		t.Fatalf("disabled global automation wrote target %d times", writes)
	}
	proposals, err := fixture.store.ListProposals(context.Background(), "", 100)
	if err != nil || len(proposals) != 1 || proposals[0].Status != "pending" || proposals[0].Mode != "auto" {
		t.Fatalf("disabled global automation did not retain the latest proposal=%#v err=%v", proposals, err)
	}
	if proposals[0].Calculation.SourceEvidenceHash != "authenticated-stable" {
		t.Fatalf("proposal source evidence = %q", proposals[0].Calculation.SourceEvidenceHash)
	}

	fixture.service.SetAutomaticWritesEnabled(true)
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	if _, writes := fixture.target.value("economy"); writes != 1 {
		t.Fatalf("explicitly enabled automation writes=%d, want 1", writes)
	}
}

func TestUpstreamIncreaseRecalculatesWhileWritesAreDisabledThenAppliesOnceEnabled(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.09")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	setTargetWriteMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)

	fixture.service.SetAutomaticWritesEnabled(false)
	saveAutomationEvidenceRatio(t, fixture, "catalog-008", "authenticated_api", "0.08", base)
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	saveAutomationEvidenceRatio(t, fixture, "catalog-010", "authenticated_api", "0.1", base.Add(time.Second))
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)

	proposals, err := fixture.store.ListProposals(context.Background(), "", 100)
	if err != nil || len(proposals) < 2 {
		t.Fatalf("recalculated proposals=%#v err=%v", proposals, err)
	}
	latest := proposals[0]
	if latest.Status != "pending" || latest.ExpectedOld != "0.09" || latest.ProposedValue != "0.110000000000" ||
		latest.Calculation.SourceEvidenceHash != "catalog-010" {
		t.Fatalf("latest disabled-write proposal = %#v", latest)
	}
	if value, writes := fixture.target.value("economy"); value != "0.09" || writes != 0 {
		t.Fatalf("disabled writes changed target value=%q writes=%d", value, writes)
	}

	fixture.service.SetAutomaticWritesEnabled(true)
	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	value, writes := fixture.target.value("economy")
	if !testDecimalEqual(value, "0.11") || writes != 1 {
		t.Fatalf("enabled reconcile value=%q writes=%d", value, writes)
	}
	applied, err := fixture.store.GetProposal(context.Background(), latest.ID)
	if err != nil || applied.Status != "applied" || !testDecimalEqual(applied.ReadbackValue, "0.11") {
		t.Fatalf("applied proposal=%#v err=%v", applied, err)
	}
}

func TestCatalogEvidenceThresholdsAreImmediateNotDayBased(t *testing.T) {
	testCases := []struct {
		name       string
		sourceType string
		samples    int
		required   int
		eligible   bool
	}{
		{name: "authenticated API first read", sourceType: "authenticated_api", samples: 1, required: 1, eligible: true},
		{name: "authenticated admin first read", sourceType: "authenticated_admin_api", samples: 1, required: 1, eligible: true},
		{name: "authenticated key first read", sourceType: "authenticated_api_key", samples: 1, required: 1, eligible: true},
		{name: "declared JSON first read", sourceType: "declared_json_api", samples: 1, required: 2, eligible: false},
		{name: "declared JSON second read", sourceType: "declared_json_api", samples: 2, required: 2, eligible: true},
		{name: "DOM second read", sourceType: "dom_page", samples: 2, required: 3, eligible: false},
		{name: "DOM third read", sourceType: "dom_page", samples: 3, required: 3, eligible: true},
	}
	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newAutomationFixture(t, "add", "0.01", "0.05")
			defer fixture.cleanup()
			setPolicyExecutionMode(t, fixture, "auto")
			base := time.Now().UTC().Add(time.Minute)
			for sample := 0; sample < testCase.samples; sample++ {
				saveAutomationEvidence(t, fixture, "threshold-hash", testCase.sourceType, base.Add(time.Duration(sample)*time.Second))
			}
			simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if simulation.Evidence.RequiredObservations != testCase.required || simulation.Evidence.AutoEligible != testCase.eligible ||
				simulation.Calculation.AutomationEligible != testCase.eligible {
				t.Fatalf("evidence=%#v calculation=%#v", simulation.Evidence, simulation.Calculation)
			}
			proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantMode := "approval"
			if testCase.eligible {
				wantMode = "auto"
			}
			if proposal.Mode != wantMode {
				t.Fatalf("proposal mode = %q, want %q", proposal.Mode, wantMode)
			}
		})
	}
}

func TestCatalogHashChangeUpdatesConfirmationWithoutReplacingSameRatioProposal(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "catalog-A", "public_api", base)
	saveAutomationEvidence(t, fixture, "catalog-A", "public_api", base.Add(time.Second))
	confirmed, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || confirmed.Mode != "auto" {
		t.Fatalf("confirmed proposal = %#v, %v", confirmed, err)
	}

	saveAutomationEvidence(t, fixture, "catalog-B", "public_api", base.Add(2*time.Second))
	replacement, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || replacement.ID != confirmed.ID || replacement.Mode != "approval" {
		t.Fatalf("replacement after hash change = %#v, %v", replacement, err)
	}
	if replacement.Calculation.AutomationEligible || !strings.Contains(replacement.Calculation.AutomationReason, "还需连续 1 次") {
		t.Fatalf("replacement calculation = %#v", replacement.Calculation)
	}
	if _, err := fixture.service.Apply(context.Background(), confirmed.ID); err != nil {
		t.Fatalf("same-ratio proposal apply error = %v", err)
	}
	if _, writes := fixture.target.value("economy"); writes != 1 {
		t.Fatalf("same-ratio proposal wrote target %d times, want 1", writes)
	}
}

func TestTargetedConfirmationRecollectsImmediatelyAndApplies(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	setTargetWriteMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "targeted-confirmation", "public_api", base)
	if _, err := fixture.service.SetConfirmationConfig(context.Background(), ConfirmationConfig{Enabled: true, IntervalSeconds: 1, MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fixture.service.SetSupplierRefresher(func(_ context.Context, supplierID int64) error {
		if supplierID != fixture.policy.SupplierID {
			t.Fatalf("refresh supplier = %d, want %d", supplierID, fixture.policy.SupplierID)
		}
		attempt := calls.Add(1)
		saveAutomationEvidence(t, fixture, "targeted-confirmation", "public_api", base.Add(time.Duration(attempt)*time.Second))
		return nil
	})

	fixture.service.ReconcileSupplier(context.Background(), fixture.policy.SupplierID)
	if calls.Load() != 1 {
		t.Fatalf("targeted refresh calls = %d, want 1", calls.Load())
	}
	value, writes := fixture.target.value("economy")
	if !testDecimalEqual(value, "0.06") || writes != 1 {
		t.Fatalf("confirmed automatic write value=%q writes=%d", value, writes)
	}
	activities, err := fixture.store.RecentActivity(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	started, completed := false, false
	for _, item := range activities {
		started = started || item.Kind == "price_confirmation_started"
		completed = completed || item.Kind == "price_confirmation_completed"
	}
	if !started || !completed {
		t.Fatalf("targeted confirmation activity started=%v completed=%v items=%#v", started, completed, activities)
	}
}

func TestTargetedConfirmationDeduplicatesConcurrentSupplierPasses(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "deduplicated-confirmation", "public_api", base)
	policies, err := fixture.store.ListPolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var calls atomic.Int32
	fixture.service.SetSupplierRefresher(func(_ context.Context, _ int64) error {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		saveAutomationEvidence(t, fixture, "deduplicated-confirmation", "public_api", base.Add(time.Second))
		return nil
	})
	firstDone := make(chan struct{})
	go func() {
		fixture.service.maybeConfirmPriceEvidence(context.Background(), fixture.policy.SupplierID, policies)
		close(firstDone)
	}()
	<-started
	secondDone := make(chan struct{})
	go func() {
		fixture.service.maybeConfirmPriceEvidence(context.Background(), fixture.policy.SupplierID, policies)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("duplicate confirmation pass did not return while the supplier was already in progress")
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("primary confirmation pass did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent confirmation refreshes = %d, want 1", calls.Load())
	}
}

func TestTargetedConfirmationNetworkFailureDoesNotEnterActivityLog(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	setPolicyExecutionMode(t, fixture, "auto")
	base := time.Now().UTC().Add(time.Minute)
	saveAutomationEvidence(t, fixture, "network-confirmation", "public_api", base)
	if _, err := fixture.service.SetConfirmationConfig(context.Background(), ConfirmationConfig{
		Enabled: true, IntervalSeconds: 1, MaxAttempts: 2,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.service.SetSupplierRefresher(func(context.Context, int64) error {
		return errors.New("upstream returned 503 Service Unavailable")
	})

	policies, err := fixture.store.ListPolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.maybeConfirmPriceEvidence(context.Background(), fixture.policy.SupplierID, policies)
	activities, err := fixture.store.RecentActivity(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Kind == "price_confirmation_failed" {
			t.Fatalf("network confirmation failure entered activity log: %#v", activity)
		}
	}
}

func saveAutomationEvidence(t *testing.T, fixture automationFixture, hash, sourceType string, observed time.Time) {
	saveAutomationEvidenceRatio(t, fixture, hash, sourceType, "0.05", observed)
}

func saveAutomationEvidenceRatio(t *testing.T, fixture automationFixture, hash, sourceType, ratio string, observed time.Time) {
	t.Helper()
	if err := fixture.store.SaveSnapshot(context.Background(), domain.CatalogSnapshot{
		SupplierID: fixture.policy.SupplierID,
		Groups: []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: ratio,
			ModelIDs: []string{"gpt-5.6-sol"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-5.6-sol", Name: "gpt-5.6-sol", Group: "economy", BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "2"}}},
		Evidence: domain.Evidence{ObservedAt: observed, ResponseHash: hash, SourceType: sourceType},
	}); err != nil {
		t.Fatalf("SaveSnapshot(%s): %v", hash, err)
	}
}

func setPolicyExecutionMode(t *testing.T, fixture automationFixture, mode string) {
	t.Helper()
	fixture.policy.ExecutionMode = mode
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatalf("UpdatePolicy mode: %v", err)
	}
}

func setTargetWriteMode(t *testing.T, fixture automationFixture, mode string) {
	t.Helper()
	target, err := fixture.store.GetTarget(context.Background(), fixture.policy.TargetSiteID)
	if err != nil {
		t.Fatal(err)
	}
	target.WriteMode = mode
	if err := fixture.store.UpdateTarget(context.Background(), target); err != nil {
		t.Fatalf("UpdateTarget mode: %v", err)
	}
}
