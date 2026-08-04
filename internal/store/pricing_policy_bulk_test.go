package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type policyBulkFixture struct {
	store         *Store
	readyPolicy   domain.PricingPolicy
	blockedPolicy domain.PricingPolicy
	pausedPolicy  domain.PricingPolicy
}

func TestPolicyBulkPreviewAndAtomicApplyRespectTargetAutoWriteGate(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	mode := "auto"
	input := domain.PricingPolicyBulkMutation{
		PolicyIDs:     []int64{fixture.blockedPolicy.ID, fixture.readyPolicy.ID, fixture.readyPolicy.ID, 999999},
		ExecutionMode: &mode,
	}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Selected != 3 || preview.Summary.Found != 2 || preview.Summary.Changed != 1 || preview.Summary.Blocked != 2 {
		t.Fatalf("unexpected preview summary: %#v", preview.Summary)
	}
	if preview.Summary.AutoWriteReady != 1 || preview.Summary.AutoWriteBlocked != 1 || len(preview.PreviewHash) != 64 {
		t.Fatalf("unexpected auto gate summary: %#v hash=%q", preview.Summary, preview.PreviewHash)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if !errors.Is(err, ErrPolicyBulkBlocked) || result.Status != "blocked" {
		t.Fatalf("atomic apply result=%#v err=%v", result, err)
	}
	ready, _ := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	if ready.ExecutionMode != "approval" {
		t.Fatalf("atomic blocker allowed partial update: %#v", ready)
	}
}

func TestPolicyBulkBestEffortAppliesEligibleRulesAndReportsBlockedRules(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	mode := "auto"
	input := domain.PricingPolicyBulkMutation{
		PolicyIDs:     []int64{fixture.readyPolicy.ID, fixture.blockedPolicy.ID},
		ExecutionMode: &mode, Strategy: domain.PolicyBulkStrategyBestEffort,
	}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" || result.Summary.Changed != 1 || result.Summary.Blocked != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	ready, _ := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	blocked, _ := fixture.store.GetPolicy(context.Background(), fixture.blockedPolicy.ID)
	if ready.ExecutionMode != "auto" || blocked.ExecutionMode != "approval" {
		t.Fatalf("policies after best effort: ready=%#v blocked=%#v", ready, blocked)
	}
	activities, err := fixture.store.RecentActivity(context.Background(), 20)
	if err != nil || len(activities) == 0 || activities[0].Kind != "pricing_policy_bulk_update" {
		t.Fatalf("bulk audit activity=%#v err=%v", activities, err)
	}
}

func TestPolicyBulkCanEnableAndSwitchDraftToAutoInOneCAS(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	draft, err := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft.Enabled = false
	draft.ExecutionMode = "approval"
	if err := fixture.store.UpdatePolicy(context.Background(), draft); err != nil {
		t.Fatal(err)
	}
	mode, enabled := "auto", true
	input := domain.PricingPolicyBulkMutation{PolicyIDs: []int64{draft.ID}, ExecutionMode: &mode, Enabled: &enabled}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Changed != 1 || preview.Summary.ExecutionModeChanges != 1 || preview.Summary.EnableChanges != 1 ||
		preview.Summary.AutoWriteReady != 1 || preview.Summary.Blocked != 0 {
		t.Fatalf("combined auto preview=%#v", preview)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if err != nil || result.Status != "applied" {
		t.Fatalf("combined auto result=%#v err=%v", result, err)
	}
	stored, err := fixture.store.GetPolicy(context.Background(), draft.ID)
	if err != nil || !stored.Enabled || stored.ExecutionMode != "auto" {
		t.Fatalf("combined auto readback=%#v err=%v", stored, err)
	}
}

func TestPolicyBulkRejectsStalePreview(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	enabled := false
	input := domain.PricingPolicyBulkMutation{PolicyIDs: []int64{fixture.readyPolicy.ID}, Enabled: &enabled}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	policy.Name = "changed after preview"
	if err := fixture.store.UpdatePolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if !errors.Is(err, ErrPolicyBulkStale) || result.Status != "stale" {
		t.Fatalf("stale apply result=%#v err=%v", result, err)
	}
	stored, _ := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	if !stored.Enabled {
		t.Fatalf("stale preview changed policy: %#v", stored)
	}
}

func TestPolicyBulkRejectsPreviewAfterTargetPermissionChanges(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	mode := "auto"
	input := domain.PricingPolicyBulkMutation{PolicyIDs: []int64{fixture.readyPolicy.ID}, ExecutionMode: &mode}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	target, err := fixture.store.GetTarget(context.Background(), fixture.readyPolicy.TargetSiteID)
	if err != nil {
		t.Fatal(err)
	}
	target.WriteMode = "approval"
	if err := fixture.store.UpdateTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if !errors.Is(err, ErrPolicyBulkStale) || result.Status != "stale" || result.Summary.Blocked != 1 {
		t.Fatalf("target-stale result=%#v err=%v", result, err)
	}
	stored, _ := fixture.store.GetPolicy(context.Background(), fixture.readyPolicy.ID)
	if stored.ExecutionMode != "approval" {
		t.Fatalf("stale target permission allowed update: %#v", stored)
	}
}

func TestPolicyBulkEnablePreflightsExistingAutoPolicy(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	enabled := true
	input := domain.PricingPolicyBulkMutation{PolicyIDs: []int64{fixture.pausedPolicy.ID}, Enabled: &enabled}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Blocked != 1 || preview.Items[0].Code != "target_write_mode_not_auto" || !preview.Targets[0].RequiresAutoWrite {
		t.Fatalf("enable preflight=%#v", preview)
	}
}

func TestPolicyBulkAllowsRiskReducingPauseWithoutAutoWritePermission(t *testing.T) {
	t.Parallel()
	fixture := newPolicyBulkFixture(t)
	enabled := false
	input := domain.PricingPolicyBulkMutation{PolicyIDs: []int64{fixture.pausedPolicy.ID}, Enabled: &enabled}
	preview, err := fixture.store.PreviewPolicyBulkMutation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Blocked != 0 || preview.Summary.DisableChanges != 1 || len(preview.Targets) != 1 || preview.Targets[0].RequiresAutoWrite || !preview.Targets[0].AutoWriteReady {
		t.Fatalf("pause preview = %#v", preview)
	}
	input.PreviewHash = preview.PreviewHash
	result, err := fixture.store.ApplyPolicyBulkMutation(context.Background(), input)
	if err != nil || result.Status != "applied" {
		t.Fatalf("pause result=%#v err=%v", result, err)
	}
	stored, _ := fixture.store.GetPolicy(context.Background(), fixture.pausedPolicy.ID)
	if stored.Enabled {
		t.Fatalf("auto policy was not paused: %#v", stored)
	}
}

func newPolicyBulkFixture(t *testing.T) policyBulkFixture {
	t.Helper()
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "bulk admin", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "bulk supplier", BaseURL: "https://upstream.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	readyTarget := domain.TargetSite{Name: "auto target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true, WriteMode: "auto"}
	if err := st.CreateTarget(ctx, &readyTarget); err != nil {
		t.Fatal(err)
	}
	blockedTarget := domain.TargetSite{Name: "approval target", BaseURL: "https://approval-target.example", CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(ctx, &blockedTarget); err != nil {
		t.Fatal(err)
	}
	readyPolicy := domain.PricingPolicy{Name: "ready", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: readyTarget.ID,
		TargetGroup: "economy", RuleType: "add", Value: "0.01", Enabled: true, ExecutionMode: "approval"}
	blockedPolicy := domain.PricingPolicy{Name: "blocked", SupplierID: supplier.ID, SourceGroup: "plus", TargetSiteID: blockedTarget.ID,
		TargetGroup: "plus", RuleType: "add", Value: "0.01", Enabled: true, ExecutionMode: "approval"}
	pausedPolicy := domain.PricingPolicy{Name: "risk reducing", SupplierID: supplier.ID, SourceGroup: "pro", TargetSiteID: blockedTarget.ID,
		TargetGroup: "pro", RuleType: "add", Value: "0.01", Enabled: true, ExecutionMode: "auto"}
	for _, policy := range []*domain.PricingPolicy{&readyPolicy, &blockedPolicy, &pausedPolicy} {
		if err := st.CreatePolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	return policyBulkFixture{store: st, readyPolicy: readyPolicy, blockedPolicy: blockedPolicy, pausedPolicy: pausedPolicy}
}
