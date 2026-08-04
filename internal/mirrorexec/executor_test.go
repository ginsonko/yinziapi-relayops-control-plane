package mirrorexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

var _ Target = (*targets.NewAPI)(nil)

func TestRemovalFirstExecutorAppliesExactStagesCommitsOwnershipAndReplays(t *testing.T) {
	fixture := newExecutorFixture(t)
	executor := fixture.executor(t, Options{WorkerID: "success", Now: fixture.clock})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ChangeSet.Status != "applied" {
		t.Fatalf("status=%s", receipt.ChangeSet.Status)
	}
	if got, want := fixture.target.writes(), []string{"channel", "contract", "channel"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("write order=%v want=%v", got, want)
	}
	assertFinalOwnership(t, fixture)
	events, err := fixture.store.ListMirrorChangeEvents(context.Background(), receipt.ChangeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, event := range events {
		if event.EventType == "remote_change_verified" {
			verified++
			if !strings.Contains(event.PayloadJSON, `"before"`) || !strings.Contains(event.PayloadJSON, `"after"`) ||
				!strings.Contains(event.PayloadJSON, `"readback"`) {
				t.Fatalf("incomplete exact event payload: %s", event.PayloadJSON)
			}
		}
	}
	if verified != 4 {
		t.Fatalf("verified event count=%d, want 4", verified)
	}
	writes := len(fixture.target.writes())
	replay, err := executor.Apply(context.Background(), fixture.input)
	if err != nil || !replay.Replayed || replay.ChangeSet.ID != receipt.ChangeSet.ID {
		t.Fatalf("replay=%#v error=%v", replay, err)
	}
	if len(fixture.target.writes()) != writes {
		t.Fatal("idempotent replay performed a remote write")
	}
}

func TestExecutorAppliesMembershipOnlyPlanWithoutPhantomEmptyContract(t *testing.T) {
	fixture := newExecutorFixture(t)
	emptyContract := domain.GroupModelPricingContract{SchemaVersion: 1,
		Groups: map[string]map[string]domain.GroupModelPricingSetting{}}
	emptyContract.Revision = contractRevision(emptyContract)
	fixture.target.mu.Lock()
	fixture.target.contract = cloneContractForTest(emptyContract)
	fixture.target.mu.Unlock()
	plannerInput := mirror.CompleteMirrorPlannerInput{
		Policy: fixture.policy, SourceState: mirror.SourceGroupPresent,
		SourceGroup: mirror.CanonicalGroup{SourceGroupID: "source", SourceGroupName: "Source", RawGroupRatio: "1",
			Models: []mirror.CanonicalModel{
				{SourceModelID: "keep", SourceExposedName: "keep", UpstreamRequestName: "keep",
					BillingMode: "inherit_target", Currency: "USD"},
				{SourceModelID: "new", SourceExposedName: "new", UpstreamRequestName: "new",
					BillingMode: "inherit_target", Currency: "USD"},
			}},
		SourceRevision: "membership-only-source", SourceObservedAtUnixMS: 101,
		CurrentGroupRatio: "1", CurrentContract: emptyContract,
		CurrentChannelModels: []string{"keep", "remove"}, CurrentChannelEnabled: true,
		ChannelStatusAuthority: "disable_only", OwnedModels: []mirror.OwnedModelState{
			{TargetModel: "keep", ContractOwned: true, MembershipOwned: true},
			{TargetModel: "remove", ContractOwned: true, MembershipOwned: true},
		},
		InitialApprovedListing: true,
		Limits: mirror.DisasterLimits{MaximumAdditions: 10, MaximumAdditionPercentage: "1",
			MaximumRemovals: 10, MaximumRemovalPercentage: "1", MaximumPriceIncrease: "10"},
	}
	plan, err := mirror.PlanCompleteMirror(plannerInput)
	if err != nil || plan.Decision != mirror.DecisionAutomatic {
		t.Fatalf("membership-only plan=%#v error=%v", plan, err)
	}
	if _, exists := plan.DesiredContract.Groups["sale"]; exists ||
		plan.ExpectedFunctionalHash != plan.DesiredFunctionalHash {
		t.Fatalf("membership-only plan materialized a phantom contract: %#v", plan.DesiredContract)
	}
	fixture.input.Plan = plan
	fixture.input.OperationKey = "membership-only-without-empty-contract"
	receipt, err := fixture.executor(t, Options{WorkerID: "membership-only", Now: fixture.clock}).
		Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("membership-only apply=%#v error=%v", receipt, err)
	}
	if got, want := fixture.target.writes(), []string{"channel", "channel"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("membership-only writes=%v want=%v", got, want)
	}
	fixture.target.mu.Lock()
	_, phantom := fixture.target.contract.Groups["sale"]
	fixture.target.mu.Unlock()
	if phantom {
		t.Fatal("membership-only execution wrote an empty target contract group")
	}
}

func TestRemovalFirstExecutorConvergesWhenOnlyOwnershipReceiptHashesAreOld(t *testing.T) {
	fixture := newExecutorFixture(t)
	ctx := context.Background()
	lease, err := fixture.store.AcquireMirrorLease(ctx, fixture.site.ID, "stale-receipt-fixture",
		fixture.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + fixture.policy.PolicyUUID
	channels, _ := fixture.store.ListActiveMirrorChannelOwnerships(ctx, fixture.policy.PolicyUUID)
	models, _ := fixture.store.ListActiveMirrorModelOwnerships(ctx, fixture.policy.PolicyUUID)
	members, _ := fixture.store.ListActiveMirrorChannelModelOwnerships(ctx, fixture.policy.PolicyUUID)
	if err := fixture.store.UpdateMirrorChannelOwnershipHash(ctx, fixture.site.ID, 77,
		fixture.policy.PolicyUUID, marker, channels[0].LastAppliedFunctionalHash, "old-channel-receipt",
		lease, fixture.now); err != nil {
		t.Fatal(err)
	}
	for _, item := range models {
		if err := fixture.store.UpdateMirrorModelOwnershipHash(ctx, fixture.site.ID, "sale", item.TargetModel,
			fixture.policy.PolicyUUID, marker, item.LastAppliedFunctionalHash, "old-model-"+item.TargetModel,
			lease, fixture.now); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range members {
		if err := fixture.store.UpdateMirrorChannelModelOwnershipHash(ctx, fixture.site.ID, 77, item.TargetModel,
			fixture.policy.PolicyUUID, marker, item.LastAppliedFunctionalHash, "old-member-"+item.TargetModel,
			lease, fixture.now); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.store.ReleaseTargetControlLease(ctx, lease, fixture.now); err != nil {
		t.Fatal(err)
	}

	receipt, err := fixture.executor(t, Options{WorkerID: "stale-receipt", Now: fixture.clock}).
		Apply(ctx, fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	assertFinalOwnership(t, fixture)
}

func TestRemovalFirstExecutorRecoversLostContractResponseFromDesiredReadback(t *testing.T) {
	fixture := newExecutorFixture(t)
	fixture.target.failAfterWrite["contract"] = 1
	receipt, err := fixture.executor(t, Options{WorkerID: "lost-response", Now: fixture.clock}).
		Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	if fixture.target.writeCount("contract") != 1 {
		t.Fatalf("contract writes=%d", fixture.target.writeCount("contract"))
	}
}

func TestRemovalFirstExecutorRecoversLostChannelResponseFromDesiredReadback(t *testing.T) {
	fixture := newExecutorFixture(t)
	fixture.target.failAfterWrite["channel"] = 1
	receipt, err := fixture.executor(t, Options{WorkerID: "lost-channel-response", Now: fixture.clock}).
		Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	if fixture.target.writeCount("channel") != 2 {
		t.Fatalf("channel writes=%d, want one removal and one addition", fixture.target.writeCount("channel"))
	}
}

func TestRemovalFirstExecutorRecoversCrashAfterChannelTransportWithoutResult(t *testing.T) {
	fixture := newExecutorFixture(t)
	crashed := false
	first := fixture.executor(t, Options{WorkerID: "crash-after-channel-send", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if !crashed && boundary == "transport_returned:removal-channel" {
				crashed = true
				return errors.New("stop before durable result")
			}
			return nil
		}})
	if _, err := first.Apply(context.Background(), fixture.input); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("first apply error=%v", err)
	}
	if fixture.target.writeCount("channel") != 1 {
		t.Fatalf("channel writes before recovery=%d", fixture.target.writeCount("channel"))
	}
	receipt, err := fixture.executor(t, Options{WorkerID: "resume-channel-send", Now: fixture.clock}).
		Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("resume receipt=%#v error=%v", receipt, err)
	}
	if fixture.target.writeCount("channel") != 2 {
		t.Fatalf("lost channel response caused a duplicate write; count=%d", fixture.target.writeCount("channel"))
	}
}

func TestRemovalFirstExecutorResumesEveryDurableBoundaryWithoutDuplicateWrites(t *testing.T) {
	fixture := newExecutorFixture(t)
	boundaries := []struct {
		name   string
		writes []string
	}{
		{"change_set_created", nil},
		{"phase:preflight_verified", nil},
		{"intent_recorded:removal-channel", nil},
		{"result_recorded:removal-channel", []string{"channel"}},
		{"step_verified:removal-channel", []string{"channel"}},
		{"phase:removals_quiesced", []string{"channel"}},
		{"intent_recorded:contract", []string{"channel"}},
		{"result_recorded:contract", []string{"channel", "contract"}},
		{"step_verified:contract", []string{"channel", "contract"}},
		{"phase:contract_applied", []string{"channel", "contract"}},
		{"intent_recorded:addition-channel", []string{"channel", "contract"}},
		{"result_recorded:addition-channel", []string{"channel", "contract", "channel"}},
		{"step_verified:addition-channel", []string{"channel", "contract", "channel"}},
		{"phase:additions_exposed", []string{"channel", "contract", "channel"}},
		{"intent_recorded:readback", []string{"channel", "contract", "channel"}},
		{"result_recorded:readback", []string{"channel", "contract", "channel"}},
		{"step_verified:readback", []string{"channel", "contract", "channel"}},
		{"ownership_committed", []string{"channel", "contract", "channel"}},
		{"phase:readback_verified", []string{"channel", "contract", "channel"}},
		{"phase:applied", []string{"channel", "contract", "channel"}},
	}
	next := 0
	var boundaryMismatch error
	executor := fixture.executor(t, Options{WorkerID: "all-boundaries", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if next >= len(boundaries) || boundary != boundaries[next].name {
				return nil
			}
			if got := fixture.target.writes(); !reflect.DeepEqual(got, boundaries[next].writes) {
				boundaryMismatch = fmt.Errorf("boundary %s writes=%v want=%v", boundary, got, boundaries[next].writes)
			}
			next++
			return errors.New("injected durable-boundary crash")
		}})
	for next < len(boundaries) {
		before := next
		if _, err := executor.Apply(context.Background(), fixture.input); !errors.Is(err, ErrInjectedCrash) {
			t.Fatalf("boundary %s apply error=%v", boundaries[before].name, err)
		}
		if boundaryMismatch != nil {
			t.Fatal(boundaryMismatch)
		}
		if next != before+1 {
			t.Fatalf("expected crash at %s, progressed from %d to %d", boundaries[before].name, before, next)
		}
	}
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("final resume receipt=%#v error=%v", receipt, err)
	}
	if got, want := fixture.target.writes(), []string{"channel", "contract", "channel"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("final write order=%v want=%v", got, want)
	}
	assertFinalOwnership(t, fixture)
}

func TestRemovalFirstExecutorPreflightAcceptsDynamicChannelCASForAdditionOnlyStep(t *testing.T) {
	fixture := newExecutorFixture(t)
	ctx := context.Background()
	lease, err := fixture.store.AcquireMirrorLease(ctx, fixture.site.ID, "dynamic-cas", fixture.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.store.ReleaseTargetControlLease(context.Background(), lease, fixture.now) })
	live := fixture.target.channelState()
	before := channelFromDomain(live)
	before.CASHash = ""
	changeSet := store.MirrorChangeSet{PolicyUUID: fixture.policy.PolicyUUID,
		PolicyVersion: fixture.policy.PolicyVersion, Steps: []store.MirrorChangeStep{{
			ResourceKind: "managed_channel_state", ExpectedHash: mustChannelSemanticHash(before),
			BeforeJSON: string(mustMarshalJSON(before)),
		}}}
	if err := fixture.executor(t, Options{WorkerID: "dynamic-cas", Now: fixture.clock}).
		verifyPreflight(ctx, fixture.input, changeSet, lease); err != nil {
		t.Fatalf("dynamic channel CAS preflight failed: %v", err)
	}
}

func TestRemovalFirstExecutorResumesCrashAfterDurableResultWithoutDuplicateWrite(t *testing.T) {
	fixture := newExecutorFixture(t)
	crashed := false
	first := fixture.executor(t, Options{WorkerID: "crash-result", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if !crashed && boundary == "result_recorded:contract" {
				crashed = true
				return errors.New("stop")
			}
			return nil
		}})
	if _, err := first.Apply(context.Background(), fixture.input); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("first apply error=%v", err)
	}
	if fixture.target.writeCount("contract") != 1 {
		t.Fatalf("contract writes before resume=%d", fixture.target.writeCount("contract"))
	}
	second := fixture.executor(t, Options{WorkerID: "resume-result", Now: fixture.clock})
	receipt, err := second.Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("resume receipt=%#v error=%v", receipt, err)
	}
	if fixture.target.writeCount("contract") != 1 {
		t.Fatalf("contract write was duplicated after crash; count=%d", fixture.target.writeCount("contract"))
	}
}

func TestRemovalFirstExecutorThirdStateStopsWithoutRemoteOverwrite(t *testing.T) {
	fixture := newExecutorFixture(t)
	crashed := false
	first := fixture.executor(t, Options{WorkerID: "third-first", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if !crashed && boundary == "intent_recorded:removal-channel" {
				crashed = true
				fixture.target.externalChannelEdit([]string{"keep", "remove", "manual"}, managedChannelEnabled)
				return errors.New("stop")
			}
			return nil
		}})
	if _, err := first.Apply(context.Background(), fixture.input); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("first apply error=%v", err)
	}
	second := fixture.executor(t, Options{WorkerID: "third-resume", Now: fixture.clock})
	receipt, err := second.Apply(context.Background(), fixture.input)
	if !errors.Is(err, ErrNeedsReconciliation) || receipt.ChangeSet.Status != "needs_reconciliation" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	if len(fixture.target.writes()) != 0 {
		t.Fatalf("third-state recovery wrote target: %v", fixture.target.writes())
	}
}

func TestRemovalFirstExecutorConditionallyCompensatesPriorRemoval(t *testing.T) {
	fixture := newExecutorFixture(t)
	fixture.target.failBeforeWrite["contract"] = 1
	executor := fixture.executor(t, Options{WorkerID: "compensate", Now: fixture.clock, MaxAttempts: 1})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if err == nil || receipt.ChangeSet.Status != "compensated" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	channel := fixture.target.channelState()
	if !equalStrings(channel.Models, []string{"keep", "remove"}) || channel.Status != managedChannelEnabled {
		t.Fatalf("channel was not restored: %#v", channel)
	}
	if fixture.target.writeCount("channel") != 2 {
		t.Fatalf("channel writes=%d, want removal plus compensation", fixture.target.writeCount("channel"))
	}
	assertInitialOwnership(t, fixture)
}

func TestRemovalFirstExecutorRefusesCompensationAfterExternalChannelDrift(t *testing.T) {
	fixture := newExecutorFixture(t)
	fixture.target.failBeforeWrite["contract"] = 1
	fixture.target.beforeFailure = func(kind string) {
		if kind == "contract" {
			fixture.target.externalChannelEdit([]string{"keep", "manual"}, managedChannelEnabled)
		}
	}
	executor := fixture.executor(t, Options{WorkerID: "compensation-conflict", Now: fixture.clock, MaxAttempts: 1})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if !errors.Is(err, ErrNeedsReconciliation) || receipt.ChangeSet.Status != "needs_reconciliation" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	channel := fixture.target.channelState()
	if !equalStrings(channel.Models, []string{"keep", "manual"}) {
		t.Fatalf("external channel edit was overwritten: %#v", channel)
	}
	assertInitialOwnership(t, fixture)
}

func TestRemovalFirstExecutorDoesNotFailAfterAuthoritativeReadbackWhenDerivedVisibilityIsPending(t *testing.T) {
	fixture := newExecutorFixture(t)
	fixture.visibility.failures = 1
	executor := fixture.executor(t, Options{WorkerID: "visibility-first", Now: fixture.clock})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	assertFinalOwnership(t, fixture)
}

func TestRemovalFirstExecutorResumesAfterReadbackWasDurablyVerifiedWithoutDerivedVisibility(t *testing.T) {
	fixture := newExecutorFixture(t)
	crashed := false
	first := fixture.executor(t, Options{WorkerID: "readback-verified-crash", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if !crashed && boundary == "step_verified:readback" {
				crashed = true
				return errors.New("stop before ownership commit")
			}
			return nil
		}})
	if _, err := first.Apply(context.Background(), fixture.input); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("first apply error=%v", err)
	}
	assertInitialOwnership(t, fixture)
	fixture.visibility.failures = 1
	receipt, err := fixture.executor(t, Options{WorkerID: "readback-visibility-delay", Now: fixture.clock}).
		Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("delayed visibility receipt=%#v error=%v", receipt, err)
	}
	assertFinalOwnership(t, fixture)
}

func TestRemovalFirstExecutorSupportsTargetsWithoutDerivedVisibilityReceipt(t *testing.T) {
	fixture := newExecutorFixture(t)
	executor, err := New(fixture.store, fixture.target, nil,
		Options{WorkerID: "no-derived-visibility", Now: fixture.clock})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if err != nil || receipt.ChangeSet.Status != "applied" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	assertFinalOwnership(t, fixture)
}

func TestRemovalFirstExecutorPolicyChangeBeforeOwnershipCommitFailsClosed(t *testing.T) {
	fixture := newExecutorFixture(t)
	changed := false
	executor := fixture.executor(t, Options{WorkerID: "policy-change-before-ownership", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if changed || boundary != "step_verified:readback" {
				return nil
			}
			changed = true
			policy := fixture.policy
			policy.Enabled = false
			if _, err := fixture.store.UpdateMirrorPolicy(context.Background(), policy,
				fixture.policy.PolicyVersion, fixture.now.Add(time.Second)); err != nil {
				t.Fatalf("disable policy at readback boundary: %v", err)
			}
			return nil
		}})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if !errors.Is(err, ErrNeedsReconciliation) || receipt.ChangeSet.Status != "needs_reconciliation" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	assertInitialOwnership(t, fixture)
}

func TestRemovalFirstExecutorPolicyChangeBetweenContractWritesStopsNextTransport(t *testing.T) {
	fixture := newExecutorFixture(t)
	changed := false
	executor := fixture.executor(t, Options{WorkerID: "policy-change-between-writes", Now: fixture.clock,
		CrashHook: func(boundary string) error {
			if changed || boundary != "step_verified:contract" {
				return nil
			}
			changed = true
			policy := fixture.policy
			policy.Enabled = false
			if _, err := fixture.store.UpdateMirrorPolicy(context.Background(), policy,
				fixture.policy.PolicyVersion, fixture.now.Add(time.Second)); err != nil {
				t.Fatalf("disable policy between contract writes: %v", err)
			}
			return nil
		}})
	receipt, err := executor.Apply(context.Background(), fixture.input)
	if !errors.Is(err, ErrNeedsReconciliation) || receipt.ChangeSet.Status != "needs_reconciliation" {
		t.Fatalf("receipt=%#v error=%v", receipt, err)
	}
	if got, want := fixture.target.writes(), []string{"channel", "contract"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("policy change reached a later transport: writes=%v want=%v", got, want)
	}
	assertInitialOwnership(t, fixture)
}

func TestRemovalFirstExecutorFenceLossBlocksTransportAndOwnership(t *testing.T) {
	fixture := newExecutorFixture(t)
	current := fixture.now
	var competing domain.TargetControlLease
	executor := fixture.executor(t, Options{WorkerID: "stale-fence", LeaseTTL: time.Minute,
		Now: func() time.Time { return current }, CrashHook: func(boundary string) error {
			if boundary == "intent_recorded:removal-channel" && competing.FencingToken == 0 {
				current = current.Add(61 * time.Second)
				var err error
				competing, err = fixture.store.AcquireMirrorLease(context.Background(), fixture.site.ID,
					"competing", current, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}})
	if _, err := executor.Apply(context.Background(), fixture.input); !errors.Is(err, store.ErrTargetControlFenceLost) {
		t.Fatalf("apply error=%v", err)
	}
	if len(fixture.target.writes()) != 0 {
		t.Fatalf("stale fence reached transport: %v", fixture.target.writes())
	}
	assertInitialOwnership(t, fixture)
	_ = fixture.store.ReleaseTargetControlLease(context.Background(), competing, current)
}

type executorFixture struct {
	store      *store.Store
	supplier   domain.Supplier
	site       domain.TargetSite
	policy     mirror.Policy
	plan       mirror.CompleteMirrorPlan
	input      ApplyInput
	target     *fakeMirrorTarget
	visibility *fakeVisibilityVerifier
	now        time.Time
}

func newExecutorFixture(t *testing.T) *executorFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "target-admin", Kind: "newapi-management", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi",
		PollIntervalSeconds: 300, RechargeValueFactor: "1", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	site := domain.TargetSite{Name: "target", BaseURL: credential.SiteURL, Adapter: "newapi",
		CredentialID: credential.ID, Enabled: true, WriteMode: "auto"}
	if err := st.CreateTarget(ctx, &site); err != nil {
		t.Fatal(err)
	}

	document := mirror.NewCompleteMirrorDocument(uuid.NewString())
	document.SupplierID = supplier.ID
	document.SourceGroup = "source"
	document.TargetSiteID = site.ID
	document.TargetGroup = "sale"
	policy, err := mirror.DecodePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMirrorPolicy(ctx, policy, time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + policy.PolicyUUID
	contract := domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "contract-before",
		Groups: map[string]map[string]domain.GroupModelPricingSetting{"sale": {
			"keep": testTaskSetting("1", marker), "remove": testTaskSetting("1", marker),
		}}}
	target := newFakeMirrorTarget(contract, map[string]string{"sale": "1"},
		domain.TargetManagedChannelState{ChannelID: 77, Models: []string{"keep", "remove"},
			Status: managedChannelEnabled, Hash: "channel-before"})
	plannerInput := mirror.CompleteMirrorPlannerInput{
		Policy: policy, SourceState: mirror.SourceGroupPresent,
		SourceGroup: mirror.CanonicalGroup{SourceGroupID: "source", SourceGroupName: "Source",
			RawGroupRatio: "1.2", RechargeValueFactor: "1", Models: []mirror.CanonicalModel{
				testTaskModel("keep", "2"), testTaskModel("new", "1"),
			}},
		SourceRevision: "source-rev", SourceObservedAtUnixMS: 100,
		CurrentGroupRatio: "1", CurrentContract: contract,
		CurrentChannelModels: []string{"keep", "remove"}, CurrentChannelEnabled: true,
		ChannelStatusAuthority: "disable_only", OwnedModels: []mirror.OwnedModelState{
			{TargetModel: "keep", ContractOwned: true, MembershipOwned: true},
			{TargetModel: "remove", ContractOwned: true, MembershipOwned: true},
		},
		Limits: mirror.DisasterLimits{MaximumAdditions: 10, MaximumAdditionPercentage: "1",
			MaximumRemovals: 10, MaximumRemovalPercentage: "1", MaximumPriceIncrease: "10"},
	}
	plan, err := mirror.PlanCompleteMirror(plannerInput)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Decision != mirror.DecisionAutomatic {
		t.Fatalf("fixture plan decision=%s gates=%#v", plan.Decision, plan.ApprovalGates)
	}

	now := time.Date(2026, 7, 28, 6, 5, 0, 0, time.UTC)
	lease, err := st.AcquireMirrorLease(ctx, site.ID, "fixture-ownership", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	channelOwner := store.MirrorChannelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: site.ID,
		TargetGroup: "sale", ChannelID: 77, OwnershipMarker: marker, SourceIdentity: "supplier/source",
		ModelSetAuthority: "exclusive", StatusAuthority: "disable_only", LastAppliedFunctionalHash: "channel-before"}
	if err := st.ClaimMirrorChannelOwnership(ctx, &channelOwner, lease, now); err != nil {
		t.Fatal(err)
	}
	for _, modelName := range []string{"keep", "remove"} {
		modelHash, _ := mirror.TargetModelFunctionalHash("sale", modelName, contract.Groups["sale"][modelName])
		modelOwner := store.MirrorModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: site.ID,
			TargetGroup: "sale", TargetModel: modelName, OwnershipMarker: marker,
			SourceIdentity: "supplier/source/model/" + modelName, LastAppliedFunctionalHash: modelHash}
		if err := st.ClaimMirrorModelOwnership(ctx, &modelOwner, lease, now); err != nil {
			t.Fatal(err)
		}
		memberHash, _ := mirror.TargetChannelModelFunctionalHash(77, modelName, true)
		memberOwner := store.MirrorChannelModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: site.ID,
			TargetGroup: "sale", ChannelID: 77, TargetModel: modelName, OwnershipMarker: marker,
			SourceIdentity: "supplier/source/model/" + modelName, LastAppliedFunctionalHash: memberHash}
		if err := st.ClaimMirrorChannelModelOwnership(ctx, &memberOwner, lease, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReleaseTargetControlLease(ctx, lease, now); err != nil {
		t.Fatal(err)
	}

	visibility := &fakeVisibilityVerifier{}
	input := ApplyInput{Policy: policy, Plan: plan, Target: site, ChannelID: 77,
		SourceProof:    json.RawMessage(`{"attempts":["a","b","c"],"complete":true}`),
		SourceIdentity: "supplier/source"}
	return &executorFixture{store: st, supplier: supplier, site: site, policy: policy,
		plan: plan, input: input, target: target, visibility: visibility, now: now.Add(time.Second)}
}

func (fixture *executorFixture) executor(t *testing.T, options Options) *Executor {
	t.Helper()
	if options.Now == nil {
		options.Now = fixture.clock
	}
	executor, err := New(fixture.store, fixture.target, fixture.visibility, options)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func (fixture *executorFixture) clock() time.Time { return fixture.now }

func assertInitialOwnership(t *testing.T, fixture *executorFixture) {
	t.Helper()
	channels, _ := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	models, _ := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	members, _ := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	if len(channels) != 1 || channels[0].LastAppliedFunctionalHash != "channel-before" ||
		modelNames(models) != "keep,remove" || memberNames(members) != "keep,remove" {
		t.Fatalf("initial ownership changed channels=%#v models=%#v memberships=%#v", channels, models, members)
	}
}

func assertFinalOwnership(t *testing.T, fixture *executorFixture) {
	t.Helper()
	channels, _ := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	models, _ := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	members, _ := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), fixture.policy.PolicyUUID)
	if len(channels) != 1 || channels[0].LastAppliedFunctionalHash != fixture.target.channelState().Hash ||
		modelNames(models) != "keep,new" || memberNames(members) != "keep,new" {
		t.Fatalf("final ownership channels=%#v models=%#v memberships=%#v", channels, models, members)
	}
}

func modelNames(items []store.MirrorModelOwnership) string {
	values := make([]string, 0, len(items))
	for _, item := range items {
		values = append(values, item.TargetModel)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func memberNames(items []store.MirrorChannelModelOwnership) string {
	values := make([]string, 0, len(items))
	for _, item := range items {
		values = append(values, item.TargetModel)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func testTaskSetting(price, marker string) domain.GroupModelPricingSetting {
	return domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &price,
		TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"}, Source: marker,
		SourceRevision: "old", SourceObservedAtUnixMS: 1}
}

func testTaskModel(name, price string) mirror.CanonicalModel {
	return mirror.CanonicalModel{SourceModelID: name, SourceExposedName: name, UpstreamRequestName: name,
		BillingMode: "fixed_price", Currency: "USD", PriceDimensions: []mirror.PriceDimension{{
			Kind: "per_request", Unit: "currency_per_request", DecimalValue: price,
		}}}
}

type fakeMirrorTarget struct {
	mu              sync.Mutex
	contract        domain.GroupModelPricingContract
	ratio           domain.TargetPriceOption
	channel         domain.TargetManagedChannelState
	writeLog        []string
	writeCounts     map[string]int
	failAfterWrite  map[string]int
	failBeforeWrite map[string]int
	beforeFailure   func(string)
	channelReplay   map[string]domain.TargetManagedChannelMutation
}

func newFakeMirrorTarget(contract domain.GroupModelPricingContract, ratios map[string]string,
	channel domain.TargetManagedChannelState) *fakeMirrorTarget {
	return &fakeMirrorTarget{contract: cloneContractForTest(contract), ratio: domain.TargetPriceOption{
		Key: "GroupRatio", Values: cloneMap(ratios), Hash: targets.HashDecimalMap(ratios)}, channel: channel,
		writeCounts: map[string]int{}, failAfterWrite: map[string]int{}, failBeforeWrite: map[string]int{},
		channelReplay: map[string]domain.TargetManagedChannelMutation{}}
}

func (target *fakeMirrorTarget) Snapshot(_ context.Context, site domain.TargetSite) (domain.TargetSnapshot, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	ratio := target.ratio
	ratio.Values = cloneMap(target.ratio.Values)
	contract := cloneContractForTest(target.contract)
	return domain.TargetSnapshot{TargetSiteID: site.ID, GroupRatio: cloneMap(ratio.Values), Hash: ratio.Hash,
		PricingOptions: map[string]domain.TargetPriceOption{"GroupRatio": ratio}, GroupModelPricing: &contract,
		GroupModelPricingSupported: true, ContractKnown: true, Contract: "test"}, nil
}

func (target *fakeMirrorTarget) ApplyPricingOptionControlled(_ context.Context, _ domain.TargetSite, key,
	expected string, desired map[string]string, guard func() error) (domain.TargetMutationResult, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if key != "GroupRatio" || target.ratio.Hash != expected {
		return domain.TargetMutationResult{ResultState: "conflict_before_write", ReadbackHash: target.ratio.Hash}, errors.New("ratio conflict")
	}
	if target.shouldFailBefore("ratio") {
		target.invokeBeforeFailureUnlocked("ratio")
		return domain.TargetMutationResult{ResultState: "rejected_before_write", ReadbackHash: target.ratio.Hash}, errors.New("ratio rejected")
	}
	if err := guard(); err != nil {
		return domain.TargetMutationResult{ResultState: "rejected_before_write"}, err
	}
	target.recordWrite("ratio")
	target.ratio.Values = cloneMap(desired)
	target.ratio.Hash = targets.HashDecimalMap(desired)
	result := domain.TargetMutationResult{ResultState: "verified_success", ExpectedHash: expected,
		DesiredHash: target.ratio.Hash, ReadbackHash: target.ratio.Hash}
	if target.shouldFailAfter("ratio") {
		result.ResultState = "ambiguous_after_write"
		return result, errors.New("ratio response lost")
	}
	return result, nil
}

func (target *fakeMirrorTarget) ReadGroupModelPricing(context.Context, domain.TargetSite) (domain.GroupModelPricingContract, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return cloneContractForTest(target.contract), nil
}

func (target *fakeMirrorTarget) PreviewGroupModelPricing(_ context.Context, _ domain.TargetSite, expected string,
	desired domain.GroupModelPricingContract) (domain.TargetGroupModelPricingPreview, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	desired = cloneContractForTest(desired)
	desired.Revision = contractRevision(desired)
	return domain.TargetGroupModelPricingPreview{ExpectedRevision: expected, CurrentRevision: target.contract.Revision,
		DesiredRevision: desired.Revision, ExpectedRevisionMatches: expected == target.contract.Revision,
		NoChange: desired.Revision == target.contract.Revision, Current: cloneContractForTest(target.contract), Desired: desired}, nil
}

func (target *fakeMirrorTarget) ApplyGroupModelPricingControlled(_ context.Context, _ domain.TargetSite,
	expected string, desired domain.GroupModelPricingContract, guard func() error) (domain.TargetMutationResult, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.contract.Revision != expected {
		return domain.TargetMutationResult{ResultState: "conflict_before_write", ReadbackHash: target.contract.Revision}, errors.New("contract conflict")
	}
	if target.shouldFailBefore("contract") {
		target.invokeBeforeFailureUnlocked("contract")
		return domain.TargetMutationResult{ResultState: "rejected_before_write", ReadbackHash: target.contract.Revision}, errors.New("contract rejected")
	}
	if err := guard(); err != nil {
		return domain.TargetMutationResult{ResultState: "rejected_before_write"}, err
	}
	target.recordWrite("contract")
	desired = cloneContractForTest(desired)
	target.contract = desired
	result := domain.TargetMutationResult{ResultState: "verified_success", ExpectedHash: expected,
		DesiredHash: desired.Revision, ReadbackHash: desired.Revision, GroupModelPricing: &desired}
	if target.shouldFailAfter("contract") {
		result.ResultState = "ambiguous_after_write"
		return result, errors.New("contract response lost")
	}
	return result, nil
}

func (target *fakeMirrorTarget) RollbackGroupModelPricingControlled(_ context.Context, _ domain.TargetSite,
	expected string, previous domain.GroupModelPricingContract, guard func() error) (domain.GroupModelPricingContract, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.contract.Revision != expected {
		return domain.GroupModelPricingContract{}, errors.New("rollback conflict")
	}
	if err := guard(); err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	target.recordWrite("contract-rollback")
	target.contract = cloneContractForTest(previous)
	return cloneContractForTest(target.contract), nil
}

func (target *fakeMirrorTarget) ReadManagedChannelState(context.Context, domain.TargetSite,
	int64) (domain.TargetManagedChannelState, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return cloneChannelState(target.channel), nil
}

func (target *fakeMirrorTarget) ApplyManagedChannelState(_ context.Context, _ domain.TargetSite, _ int64,
	expected string, models []string, status *int, requestID string,
	guard func() error) (domain.TargetManagedChannelMutation, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if replay, exists := target.channelReplay[requestID]; exists {
		return replay, nil
	}
	if target.channel.Hash != expected {
		return domain.TargetManagedChannelMutation{ResultState: "conflict_before_write", ReadbackHash: target.channel.Hash,
			State: cloneChannelState(target.channel)}, errors.New("channel conflict")
	}
	if target.shouldFailBefore("channel") {
		target.invokeBeforeFailureUnlocked("channel")
		return domain.TargetManagedChannelMutation{ResultState: "rejected_before_write", ReadbackHash: target.channel.Hash,
			State: cloneChannelState(target.channel)}, errors.New("channel rejected")
	}
	if err := guard(); err != nil {
		return domain.TargetManagedChannelMutation{ResultState: "rejected_before_write"}, err
	}
	target.recordWrite("channel")
	target.channel.Models = normalizeModels(models)
	if status != nil {
		target.channel.Status = *status
	}
	target.channel.Hash = fmt.Sprintf("channel-%d", target.writeCounts["channel"])
	result := domain.TargetManagedChannelMutation{ResultState: "verified_success", ExpectedHash: expected,
		ReadbackHash: target.channel.Hash, Changed: true, State: cloneChannelState(target.channel)}
	target.channelReplay[requestID] = result
	if target.shouldFailAfter("channel") {
		ambiguous := result
		ambiguous.ResultState = "ambiguous_after_write"
		return ambiguous, errors.New("channel response lost")
	}
	return result, nil
}

func (target *fakeMirrorTarget) shouldFailBefore(kind string) bool {
	if target.failBeforeWrite[kind] <= 0 {
		return false
	}
	target.failBeforeWrite[kind]--
	return true
}

func (target *fakeMirrorTarget) invokeBeforeFailureUnlocked(kind string) {
	callback := target.beforeFailure
	target.mu.Unlock()
	defer target.mu.Lock()
	if callback != nil {
		callback(kind)
	}
}

func (target *fakeMirrorTarget) shouldFailAfter(kind string) bool {
	if target.failAfterWrite[kind] <= 0 {
		return false
	}
	target.failAfterWrite[kind]--
	return true
}

func (target *fakeMirrorTarget) recordWrite(kind string) {
	target.writeLog = append(target.writeLog, kind)
	target.writeCounts[kind]++
}

func (target *fakeMirrorTarget) writes() []string {
	target.mu.Lock()
	defer target.mu.Unlock()
	return append([]string(nil), target.writeLog...)
}

func (target *fakeMirrorTarget) writeCount(kind string) int {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.writeCounts[kind]
}

func (target *fakeMirrorTarget) channelState() domain.TargetManagedChannelState {
	target.mu.Lock()
	defer target.mu.Unlock()
	return cloneChannelState(target.channel)
}

func (target *fakeMirrorTarget) externalChannelEdit(models []string, status int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.channel.Models = normalizeModels(models)
	target.channel.Status = status
	target.channel.Hash = "channel-external-" + uuid.NewString()
}

type fakeVisibilityVerifier struct {
	mu       sync.Mutex
	failures int
}

func (verifier *fakeVisibilityVerifier) VerifyMirrorVisibility(_ context.Context, _ domain.TargetSite,
	_ int64, _ string, _ []string) (VisibilityEvidence, error) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if verifier.failures > 0 {
		verifier.failures--
		return VisibilityEvidence{}, errors.New("ability cache has not converged")
	}
	return VisibilityEvidence{MarketplaceVisible: true, AbilityVisible: true, CacheVisible: true,
		EvidenceHash: "visibility-ok", Details: json.RawMessage(`{"verified":true}`)}, nil
}

func cloneContractForTest(value domain.GroupModelPricingContract) domain.GroupModelPricingContract {
	encoded, _ := json.Marshal(value)
	var result domain.GroupModelPricingContract
	_ = json.Unmarshal(encoded, &result)
	return result
}

func cloneChannelState(value domain.TargetManagedChannelState) domain.TargetManagedChannelState {
	value.Models = append([]string(nil), value.Models...)
	return value
}

func contractRevision(contract domain.GroupModelPricingContract) string {
	hash := targets.HashGroupModelPricingContract(contract)
	if hash == "" {
		panic("invalid test contract")
	}
	return "contract-" + hash[:16]
}
