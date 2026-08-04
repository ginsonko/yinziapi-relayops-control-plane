package mirrorservice

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

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorexec"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type adoptionFixture struct {
	service  *Service
	store    *store.Store
	target   *adoptionTarget
	supplier domain.Supplier
	site     domain.TargetSite
	request  AdoptionPreviewRequest
}

type adoptionTarget struct {
	mu               sync.Mutex
	channel          domain.TargetChannel
	managed          domain.TargetManagedChannelState
	contract         domain.GroupModelPricingContract
	key              string
	reads            int
	contractWrites   int
	pricingWrites    int
	channelWrites    int
	loseNextResponse bool
}

func (target *adoptionTarget) Name() string { return "adoption-memory" }

func (target *adoptionTarget) Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.reads++
	return domain.TargetSnapshot{GroupModelPricing: ptrAdoptionContract(cloneAdoptionContractForTest(target.contract)),
		GroupModelPricingSupported: true, PricingOptions: map[string]domain.TargetPriceOption{
			"GroupRatio": {Values: map[string]string{"retail": "1"}},
		}}, nil
}

func (target *adoptionTarget) ApplyPricingOptionControlled(context.Context, domain.TargetSite, string,
	string, map[string]string, func() error) (domain.TargetMutationResult, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.pricingWrites++
	return domain.TargetMutationResult{}, errors.New("unexpected pricing-option write")
}

func (target *adoptionTarget) ReadGroupModelPricing(context.Context,
	domain.TargetSite) (domain.GroupModelPricingContract, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.reads++
	return cloneAdoptionContractForTest(target.contract), nil
}

func (target *adoptionTarget) PreviewGroupModelPricing(_ context.Context, site domain.TargetSite,
	expectedRevision string, desired domain.GroupModelPricingContract) (domain.TargetGroupModelPricingPreview, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.reads++
	current := cloneAdoptionContractForTest(target.contract)
	desired = cloneAdoptionContractForTest(desired)
	desired.Revision = targets.HashGroupModelPricingContract(desired)
	return domain.TargetGroupModelPricingPreview{TargetSiteID: site.ID, ExpectedRevision: expectedRevision,
		CurrentRevision: current.Revision, DesiredRevision: desired.Revision,
		ExpectedRevisionMatches: current.Revision == expectedRevision, NoChange: current.Revision == desired.Revision,
		Current: current, Desired: desired}, nil
}

func (target *adoptionTarget) ApplyGroupModelPricingControlled(_ context.Context, _ domain.TargetSite,
	expectedRevision string, desired domain.GroupModelPricingContract,
	guard func() error) (domain.TargetMutationResult, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	desired = cloneAdoptionContractForTest(desired)
	desired.Revision = targets.HashGroupModelPricingContract(desired)
	result := domain.TargetMutationResult{ExpectedHash: expectedRevision, DesiredHash: desired.Revision,
		ReadbackHash: target.contract.Revision}
	if target.contract.Revision == desired.Revision {
		result.ResultState = "verified_success"
		result.GroupModelPricing = ptrAdoptionContract(cloneAdoptionContractForTest(target.contract))
		return result, nil
	}
	if target.contract.Revision != expectedRevision {
		result.ResultState = "conflict_before_write"
		return result, errors.New("contract changed before adoption write")
	}
	if guard == nil {
		return result, errors.New("missing adoption write guard")
	}
	if err := guard(); err != nil {
		return result, err
	}
	target.contract = desired
	target.contractWrites++
	result.ResultState = "verified_success"
	result.ReadbackHash = desired.Revision
	result.GroupModelPricing = ptrAdoptionContract(cloneAdoptionContractForTest(desired))
	if target.loseNextResponse {
		target.loseNextResponse = false
		result.ResultState = "ambiguous_after_write"
		return result, errors.New("simulated response loss after durable target write")
	}
	return result, nil
}

func (target *adoptionTarget) RollbackGroupModelPricingControlled(context.Context, domain.TargetSite,
	string, domain.GroupModelPricingContract, func() error) (domain.GroupModelPricingContract, error) {
	return domain.GroupModelPricingContract{}, errors.New("unexpected adoption rollback")
}

func (target *adoptionTarget) ReadManagedChannelState(context.Context, domain.TargetSite,
	int64) (domain.TargetManagedChannelState, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.reads++
	result := target.managed
	result.Models = append([]string(nil), result.Models...)
	return result, nil
}

func (target *adoptionTarget) ApplyManagedChannelState(context.Context, domain.TargetSite, int64,
	string, []string, *int, string, func() error) (domain.TargetManagedChannelMutation, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.channelWrites++
	return domain.TargetManagedChannelMutation{}, errors.New("unexpected managed-channel write")
}

func (target *adoptionTarget) ListChannels(context.Context,
	domain.TargetSite) ([]domain.TargetChannel, error) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.reads++
	result := target.channel
	result.Models = append([]string(nil), result.Models...)
	return []domain.TargetChannel{result}, nil
}

func (target *adoptionTarget) writeCounts() (int, int, int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.contractWrites, target.pricingWrites, target.channelWrites
}

func (target *adoptionTarget) readCount() int {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.reads
}

type adoptionVisibility struct{}

func (adoptionVisibility) ProbeMirrorVisibility(context.Context, domain.TargetSite, int64, string) error {
	return nil
}

func (adoptionVisibility) VerifyMirrorVisibility(context.Context, domain.TargetSite, int64, string,
	[]string) (mirrorexec.VisibilityEvidence, error) {
	return mirrorexec.VisibilityEvidence{MarketplaceVisible: true, AbilityVisible: true, CacheVisible: true,
		EvidenceHash: strings.Repeat("a", 64)}, nil
}

func TestExistingChannelAdoptionIsReadOnlyAtomicAndReplayable(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	beforeContract := cloneAdoptionContractForTest(fixture.target.contract)
	beforeChannel := fixture.target.channel
	beforeKey := fixture.target.key

	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || !preview.Executable || preview.AlreadyAdopted || preview.SourceModelCount != 2 {
		t.Fatalf("preview=%#v error=%v", preview, err)
	}
	if contract, pricing, channel := fixture.target.writeCounts(); contract != 0 || pricing != 0 || channel != 0 {
		t.Fatalf("preview performed writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
	}
	if _, err := fixture.store.GetMirrorPolicy(ctx, preview.Policy.PolicyUUID); !store.IsNotFound(err) {
		t.Fatalf("preview persisted a policy: %v", err)
	}

	receipt, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
	if err != nil || receipt.Replayed || !receipt.Policy.Enabled {
		t.Fatalf("apply=%#v error=%v previewRevisions=%s/%s target=%#v", receipt, err,
			preview.ContractRevision, preview.DesiredContractRevision, fixture.target.contract)
	}
	if contract, pricing, channel := fixture.target.writeCounts(); contract != 1 || pricing != 0 || channel != 0 {
		t.Fatalf("apply write boundary: contract=%d pricing=%d channel=%d", contract, pricing, channel)
	}
	assertAdoptionFunctionalStateUnchanged(t, fixture.target, beforeContract, beforeChannel, beforeKey)
	assertAdoptionAuthorityCommitted(t, fixture, receipt.Policy.PolicyUUID, 2)

	replayed, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
	if err != nil || !replayed.Replayed {
		t.Fatalf("same-preview replay=%#v error=%v", replayed, err)
	}
	fixture.service.remote = remotewrite.New(false)
	replayed, err = fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
	if err != nil || !replayed.Replayed {
		t.Fatalf("read-only-gate replay=%#v error=%v", replayed, err)
	}
	if contract, pricing, channel := fixture.target.writeCounts(); contract != 1 || pricing != 0 || channel != 0 {
		t.Fatalf("replay repeated writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
	}
}

func TestExistingChannelAdoptionRecoversResponseLossFromFreshPreview(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.target.loseNextResponse = true
	if _, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true}); err == nil {
		t.Fatal("response loss was not surfaced")
	}
	stored, err := fixture.store.GetMirrorPolicy(ctx, preview.Policy.PolicyUUID)
	if err != nil || stored.Enabled {
		t.Fatalf("ambiguous write did not leave an inert draft: %#v error=%v", stored, err)
	}
	if channels, _ := fixture.store.ListActiveMirrorChannelOwnerships(ctx, preview.Policy.PolicyUUID); len(channels) != 0 {
		t.Fatalf("ambiguous write committed ownership: %#v", channels)
	}

	fresh, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || !fresh.Executable || fresh.ID == preview.ID {
		t.Fatalf("fresh preview=%#v error=%v", fresh, err)
	}
	receipt, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: fresh.ID, ApprovalGranted: true})
	if err != nil || !receipt.Policy.Enabled {
		t.Fatalf("response-loss recovery=%#v error=%v", receipt, err)
	}
	if contract, pricing, channel := fixture.target.writeCounts(); contract != 1 || pricing != 0 || channel != 0 {
		t.Fatalf("recovery repeated remote writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
	}
	assertAdoptionAuthorityCommitted(t, fixture, receipt.Policy.PolicyUUID, 2)
}

func TestExistingChannelAdoptionAcceptsConfirmedEmptySourceGroup(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	commitEmptyAdoptionSource(t, fixture.store, fixture.supplier.ID)

	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || !preview.Executable || preview.SourceModelCount != 0 || preview.SourceTargetOverlap != 0 {
		t.Fatalf("empty-source preview=%#v error=%v", preview, err)
	}
	if contract, pricing, channel := fixture.target.writeCounts(); contract != 0 || pricing != 0 || channel != 0 {
		t.Fatalf("empty-source preview performed writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
	}

	receipt, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
	if err != nil || !receipt.Policy.Enabled {
		t.Fatalf("empty-source adoption=%#v error=%v", receipt, err)
	}
	models, modelErr := fixture.store.ListActiveMirrorModelOwnerships(ctx, receipt.Policy.PolicyUUID)
	members, memberErr := fixture.store.ListActiveMirrorChannelModelOwnerships(ctx, receipt.Policy.PolicyUUID)
	if modelErr != nil || memberErr != nil || len(models) != 2 || len(members) != 2 {
		t.Fatalf("empty-source adoption did not retain the current models for first-sync removal: models=%#v members=%#v errors=%v/%v",
			models, members, modelErr, memberErr)
	}
}

func TestExistingChannelAdoptionRefreshesStaleBindingsBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*adoptionTarget)
	}{
		{name: "channel config", mutate: func(target *adoptionTarget) {
			target.channel.Name = "externally-renamed"
			target.channel.ConfigHash = targets.HashTargetChannel(target.channel)
		}},
		{name: "managed state", mutate: func(target *adoptionTarget) {
			target.managed.Hash = strings.Repeat("9", 64)
		}},
		{name: "contract revision", mutate: func(target *adoptionTarget) {
			price := "0.99"
			setting := target.contract.Groups["retail"]["model-a"]
			setting.ModelPrice = &price
			target.contract.Groups["retail"]["model-a"] = setting
			target.contract.Revision = targets.HashGroupModelPricingContract(target.contract)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdoptionFixture(t)
			preview, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			fixture.target.mu.Lock()
			test.mutate(fixture.target)
			fixture.target.mu.Unlock()
			receipt, err := fixture.service.ApplyExistingChannelAdoption(context.Background(),
				AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
			if err != nil || !receipt.Policy.Enabled {
				t.Fatalf("refreshed adoption receipt=%#v error=%v", receipt, err)
			}
			if contract, pricing, channel := fixture.target.writeCounts(); contract != 1 || pricing != 0 || channel != 0 {
				t.Fatalf("refreshed apply writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
			}
			stored, err := fixture.store.GetMirrorPolicy(context.Background(), preview.Policy.PolicyUUID)
			if err != nil || !stored.Enabled {
				t.Fatalf("refreshed apply policy=%#v error=%v", stored, err)
			}
		})
	}
}

func TestExistingChannelAdoptionRejectsForeignMarkerAndManagedChannelMismatch(t *testing.T) {
	t.Run("foreign marker", func(t *testing.T) {
		fixture := newAdoptionFixture(t)
		fixture.target.mu.Lock()
		setting := fixture.target.contract.Groups["retail"]["model-a"]
		setting.Source = "relayops:policy/00000000-0000-0000-0000-000000000001"
		fixture.target.contract.Groups["retail"]["model-a"] = setting
		fixture.target.contract.Revision = targets.HashGroupModelPricingContract(fixture.target.contract)
		fixture.target.mu.Unlock()
		if _, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request); err == nil ||
			!strings.Contains(err.Error(), "another mirror policy") {
			t.Fatalf("foreign marker error=%v", err)
		}
	})
	t.Run("managed channel mismatch", func(t *testing.T) {
		fixture := newAdoptionFixture(t)
		fixture.target.mu.Lock()
		fixture.target.channel.Models = []string{"model-a"}
		fixture.target.channel.ConfigHash = targets.HashTargetChannel(fixture.target.channel)
		fixture.target.mu.Unlock()
		if _, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request); err == nil ||
			!strings.Contains(err.Error(), "managed-state model sets are inconsistent") {
			t.Fatalf("model-set error=%v", err)
		}
	})
}

func TestExistingChannelAdoptionClaimsContractAndMembershipSetsIndependently(t *testing.T) {
	tests := []struct {
		name            string
		channelModels   []string
		contractModels  []string
		wantModels      int
		wantMemberships int
	}{
		{name: "contract has extra model", channelModels: []string{"model-a"},
			contractModels: []string{"model-a", "model-b"}, wantModels: 2, wantMemberships: 1},
		{name: "channel has extra model", channelModels: []string{"model-a", "model-b"},
			contractModels: []string{"model-a"}, wantModels: 1, wantMemberships: 2},
		{name: "both sets empty", channelModels: []string{}, contractModels: []string{}, wantModels: 0, wantMemberships: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdoptionFixture(t)
			fixture.target.mu.Lock()
			fixture.target.channel.Models = append([]string(nil), test.channelModels...)
			fixture.target.channel.ConfigHash = targets.HashTargetChannel(fixture.target.channel)
			fixture.target.managed.Models = append([]string(nil), test.channelModels...)
			group := make(map[string]domain.GroupModelPricingSetting, len(test.contractModels))
			for _, modelName := range test.contractModels {
				if modelName == "model-b" {
					ratio, completion := "3", "1.5"
					group[modelName] = domain.GroupModelPricingSetting{BillingMode: "ratio", ModelRatio: &ratio,
						CompletionRatio: &completion}
				} else {
					price := "0.12"
					group[modelName] = domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &price,
						TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"}}
				}
			}
			fixture.target.contract.Groups["retail"] = group
			fixture.target.contract.Revision = targets.HashGroupModelPricingContract(fixture.target.contract)
			fixture.target.mu.Unlock()

			preview, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request)
			if err != nil || !preview.Executable {
				t.Fatalf("preview=%#v error=%v", preview, err)
			}
			if contract, pricing, channel := fixture.target.writeCounts(); contract != 0 || pricing != 0 || channel != 0 {
				t.Fatalf("preview performed writes: contract=%d pricing=%d channel=%d", contract, pricing, channel)
			}
			receipt, err := fixture.service.ApplyExistingChannelAdoption(context.Background(),
				AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
			if err != nil || !receipt.Policy.Enabled {
				t.Fatalf("apply=%#v error=%v", receipt, err)
			}
			models, modelErr := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), receipt.Policy.PolicyUUID)
			members, memberErr := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), receipt.Policy.PolicyUUID)
			if modelErr != nil || memberErr != nil || len(models) != test.wantModels || len(members) != test.wantMemberships {
				t.Fatalf("ownership models=%#v memberships=%#v errors=%v/%v", models, members, modelErr, memberErr)
			}
			firstSync, syncErr := fixture.service.PreviewPolicy(context.Background(),
				PreviewRequest{PolicyUUID: receipt.Policy.PolicyUUID})
			if syncErr != nil || len(firstSync.Plan.Diffs) == 0 {
				t.Fatalf("first mirror sync did not plan convergence: preview=%#v error=%v", firstSync, syncErr)
			}
		})
	}
}

func TestCompleteMirrorPreviewsAdministratorEditsAndStillRejectsForeignPricing(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: preview.ID, ApprovalGranted: true})
	if err != nil {
		t.Fatal(err)
	}

	fixture.target.mu.Lock()
	fixture.target.managed.Models = []string{"manual-model", "model-a"}
	fixture.target.managed.Hash = strings.Repeat("7", 64)
	fixture.target.channel.Models = append([]string(nil), fixture.target.managed.Models...)
	fixture.target.channel.ConfigHash = targets.HashTargetChannel(fixture.target.channel)
	setting := fixture.target.contract.Groups["retail"]["model-a"]
	changedPrice := "0.99"
	setting.ModelPrice = &changedPrice
	setting.TaskProfile = &domain.GroupModelTaskProfile{Unit: "per_request"}
	fixture.target.contract.Groups["retail"]["model-a"] = setting
	fixture.target.contract.Revision = targets.HashGroupModelPricingContract(fixture.target.contract)
	fixture.target.mu.Unlock()

	convergence, err := fixture.service.PreviewPolicy(ctx, PreviewRequest{PolicyUUID: receipt.Policy.PolicyUUID})
	if err != nil || len(convergence.Plan.Diffs) == 0 {
		t.Fatalf("administrator edit was not planned for convergence: preview=%#v error=%v", convergence, err)
	}

	fixture.target.mu.Lock()
	setting = fixture.target.contract.Groups["retail"]["model-a"]
	setting.Source = "relayops:policy/00000000-0000-0000-0000-000000000001"
	fixture.target.contract.Groups["retail"]["model-a"] = setting
	fixture.target.contract.Revision = targets.HashGroupModelPricingContract(fixture.target.contract)
	fixture.target.mu.Unlock()
	if _, err := fixture.service.PreviewPolicy(ctx, PreviewRequest{PolicyUUID: receipt.Policy.PolicyUUID}); err == nil ||
		!strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign pricing marker was not rejected: %v", err)
	}
}

func TestExistingChannelAdoptionUsesCurrentDisabledPolicyVersion(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	policy := preview.Policy
	if err := fixture.store.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for expected := int64(1); expected <= 2; expected++ {
		policy, err = fixture.store.UpdateMirrorPolicy(ctx, policy, expected, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || fresh.Policy.PolicyVersion != 3 {
		t.Fatalf("versioned preview=%#v error=%v", fresh.Policy, err)
	}
	receipt, err := fixture.service.ApplyExistingChannelAdoption(ctx,
		AdoptionApplyRequest{PreviewID: fresh.ID, ApprovalGranted: true})
	if err != nil || receipt.Policy.PolicyVersion != 4 || !receipt.Policy.Enabled {
		t.Fatalf("versioned apply=%#v error=%v", receipt, err)
	}
}

func TestExistingChannelAdoptionResumesExistingNativePolicyUUID(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	const existingUUID = "0ce5e701-9e36-5471-ac95-4f67104ccf5c"
	fixture.request.Policy.PolicyUUID = existingUUID
	policy, err := buildAdoptionPolicy(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	policy, err = fixture.store.UpdateMirrorPolicy(ctx, policy, 1, time.Now().UTC())
	if err != nil || policy.PolicyVersion != 2 {
		t.Fatalf("version existing policy: policy=%#v error=%v", policy, err)
	}

	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || !preview.Executable || preview.Policy.PolicyUUID != existingUUID ||
		preview.Policy.PolicyVersion != 2 {
		t.Fatalf("existing native UUID preview=%#v error=%v", preview, err)
	}
}

func TestExistingChannelAdoptionTreatsHistoricalNilExceptionsAsEmpty(t *testing.T) {
	fixture := newAdoptionFixture(t)
	ctx := context.Background()
	fixture.request.Policy.PolicyUUID = "00000000-0000-0000-0000-000000000097"
	policy, err := buildAdoptionPolicy(fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	policy.ExcludedModels = nil
	if err := fixture.store.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	preview, err := fixture.service.PreviewExistingChannelAdoption(ctx, fixture.request)
	if err != nil || !preview.Executable || preview.Policy.PolicyUUID != policy.PolicyUUID {
		t.Fatalf("historical nil exceptions preview=%#v error=%v", preview, err)
	}
}

func TestExistingChannelAdoptionRejectsUnownedIdentityBeforeTargetRead(t *testing.T) {
	tests := []struct {
		name       string
		policyUUID string
		prepare    func(*testing.T, adoptionFixture)
		want       string
	}{
		{name: "missing UUID", policyUUID: "00000000-0000-0000-0000-000000000099",
			want: "existing native complete-mirror policy"},
		{name: "malformed UUID", policyUUID: "not-a-uuid", want: "valid UUID"},
		{name: "different settings", policyUUID: "00000000-0000-0000-0000-000000000098",
			prepare: func(t *testing.T, fixture adoptionFixture) {
				policy, err := buildAdoptionPolicy(fixture.request)
				if err != nil {
					t.Fatal(err)
				}
				policy.ExcludedModels = []string{"model-z"}
				if err := fixture.store.CreateMirrorPolicy(context.Background(), policy, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}, want: "different mirror settings"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdoptionFixture(t)
			fixture.request.Policy.PolicyUUID = test.policyUUID
			if test.prepare != nil {
				test.prepare(t, fixture)
			}
			_, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("identity error=%v", err)
			}
			if reads := fixture.target.readCount(); reads != 0 {
				t.Fatalf("identity rejection performed %d target reads", reads)
			}
		})
	}
}

func TestExistingChannelAdoptionKeepsDeterministicIdentityPaths(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			fixture := newAdoptionFixture(t)
			if explicit {
				fixture.request.Policy.PolicyUUID = deterministicAdoptionPolicyUUID(fixture.supplier.ID,
					fixture.request.Policy.SourceGroup, fixture.site.ID, fixture.request.Policy.TargetGroup,
					fixture.request.ChannelID)
			}
			preview, err := fixture.service.PreviewExistingChannelAdoption(context.Background(), fixture.request)
			if err != nil || !preview.Executable || preview.Policy.PolicyUUID == "" {
				t.Fatalf("deterministic preview=%#v error=%v", preview, err)
			}
		})
	}
}

func newAdoptionFixture(t *testing.T) adoptionFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	supplier := domain.Supplier{Name: "source", BaseURL: "https://source.example", Adapter: "newapi",
		RechargeValueFactor: "1", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "target", Kind: "newapi-management", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	site := domain.TargetSite{Name: "target", BaseURL: credential.SiteURL, Adapter: "newapi",
		CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(ctx, &site); err != nil {
		t.Fatal(err)
	}
	activateAdoptionAuthority(t, st, supplier.ID)

	fixed := "0.12"
	ratio := "3"
	completion := "1.5"
	contract := domain.GroupModelPricingContract{SchemaVersion: 1,
		Groups: map[string]map[string]domain.GroupModelPricingSetting{"retail": {
			"model-a": {BillingMode: "fixed_price", ModelPrice: &fixed},
			"model-b": {BillingMode: "ratio", ModelRatio: &ratio, CompletionRatio: &completion},
		}}}
	contract.Revision = targets.HashGroupModelPricingContract(contract)
	channel := domain.TargetChannel{ID: 41, Name: "existing", Type: 1, Status: 1,
		BaseURL: "https://source.example/v1", Models: []string{"model-b", "model-a"}, Group: "retail",
		Priority: 7, Weight: 30, AutoBan: true, ModelMapping: `{}`, Tag: "existing", Remark: "keep"}
	channel.ConfigHash = targets.HashTargetChannel(channel)
	managedModels := []string{"model-a", "model-b"}
	target := &adoptionTarget{channel: channel, key: "upstream-secret-must-not-change", contract: contract,
		managed: domain.TargetManagedChannelState{ChannelID: channel.ID, Models: managedModels,
			Status: channel.Status, Hash: strings.Repeat("8", 64)}}
	gate := remotewrite.New(true)
	factory := targets.NewFactory(nil, nil)
	factory.SetRemoteWriteGate(gate)
	service, err := New(st, factory, adoptionVisibility{}, Options{Now: func() time.Time {
		return time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.resolveTarget = func(context.Context, domain.TargetSite) (mirrorTarget, error) { return target, nil }
	document := mirror.NewCompleteMirrorDocument("")
	document.SupplierID = supplier.ID
	document.SourceGroup = "source-group"
	document.TargetSiteID = site.ID
	document.TargetGroup = "retail"
	document.Enabled = boolValue(false)
	return adoptionFixture{service: service, store: st, target: target, supplier: supplier, site: site,
		request: AdoptionPreviewRequest{ChannelID: channel.ID, Policy: document}}
}

func activateAdoptionAuthority(t *testing.T, st *store.Store, supplierID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SetReleaseAdmissionDraining(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateCollectionAuthorityV2(ctx, store.AuthorityMigrationOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetReleaseAdmissionDraining(ctx, false); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 5, 59, 0, 0, time.UTC)
	if ok, err := st.TryAcquireCollectionLease(ctx, supplierID, "adoption-source", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("acquire source lease: ok=%v error=%v", ok, err)
	}
	snapshot := domain.CatalogSnapshot{SupplierID: supplierID,
		Groups: []domain.GroupOffer{{ExternalID: "source-group", Name: "source-group", Ratio: "1", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"model-a", "model-b"}}},
		Models: []domain.ModelOffer{
			{ExternalID: "model-a", Name: "model-a", Group: "source-group", BillingMode: "fixed_price",
				PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "0.12"}},
			{ExternalID: "model-b", Name: "model-b", Group: "source-group", BillingMode: "token_vector",
				PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "1", OutputPer1M: "2"}},
		}, Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "adoption-source-catalog"}}
	attempt := domain.CollectionAttempt{SupplierID: supplierID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true,
		CatalogProof: &domain.CatalogCollectionProofBasis{AuthoritativeEndpoint: "newapi://pricing",
			Authenticated: true, SourceRevision: snapshot.Evidence.ResponseHash,
			Pages:        []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: 3, Terminal: true}},
			RawItemCount: 3, ParsedItemCount: 3, UniqueItemCount: 3, ObservedAt: now}}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatalf("commit authoritative source: %v", err)
	}
	if err := st.ReleaseCollectionLease(ctx, supplierID, "adoption-source"); err != nil {
		t.Fatalf("release source lease: %v", err)
	}
	proofs, err := st.ListCatalogGroupCollectionProofs(ctx, supplierID, "source-group", 10)
	if err != nil || len(proofs) != 1 || !proofs[0].Complete {
		t.Fatalf("authoritative source proof=%#v error=%v", proofs, err)
	}
}

func commitEmptyAdoptionSource(t *testing.T, st *store.Store, supplierID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 29, 6, 1, 0, 0, time.UTC)
	if ok, err := st.TryAcquireCollectionLease(ctx, supplierID, "adoption-source", time.Now().UTC(), time.Minute); err != nil || !ok {
		t.Fatalf("reacquire source lease: ok=%v error=%v", ok, err)
	}
	snapshot := domain.CatalogSnapshot{SupplierID: supplierID,
		Groups: []domain.GroupOffer{{ExternalID: "source-group", Name: "source-group", Ratio: "1", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "adoption-empty-source-catalog"}}
	attempt := domain.CollectionAttempt{SupplierID: supplierID, Status: "success", CatalogUpdated: true,
		StartedAt: now.Add(-time.Second), FinishedAt: now}
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true,
		CatalogProof: &domain.CatalogCollectionProofBasis{AuthoritativeEndpoint: "newapi://pricing",
			Authenticated: true, SourceRevision: snapshot.Evidence.ResponseHash,
			Pages:        []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: 1, Terminal: true}},
			RawItemCount: 1, ParsedItemCount: 1, UniqueItemCount: 1, ObservedAt: now}}
	if _, err := st.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, now.Add(time.Minute)); err != nil {
		t.Fatalf("commit authoritative empty source: %v", err)
	}
	if err := st.ReleaseCollectionLease(ctx, supplierID, "adoption-source"); err != nil {
		t.Fatalf("release empty-source lease: %v", err)
	}
}

func assertAdoptionFunctionalStateUnchanged(t *testing.T, target *adoptionTarget,
	before domain.GroupModelPricingContract, channel domain.TargetChannel, key string) {
	t.Helper()
	target.mu.Lock()
	defer target.mu.Unlock()
	after := cloneAdoptionContractForTest(target.contract)
	marker := ""
	for modelName, setting := range after.Groups["retail"] {
		if !strings.HasPrefix(setting.Source, "relayops:policy/") {
			t.Fatalf("model %s was not stamped: %#v", modelName, setting)
		}
		if marker == "" {
			marker = setting.Source
		} else if marker != setting.Source {
			t.Fatalf("models received different ownership markers")
		}
		setting.Source = ""
		after.Groups["retail"][modelName] = setting
	}
	before.Revision, after.Revision = "", ""
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("adoption changed functional pricing: before=%#v after=%#v", before, after)
	}
	if !reflect.DeepEqual(channel, target.channel) || key != target.key {
		t.Fatalf("adoption changed channel config or key: channel=%#v keyChanged=%v", target.channel, key != target.key)
	}
}

func assertAdoptionAuthorityCommitted(t *testing.T, fixture adoptionFixture, policyUUID string,
	wantVersion int64) {
	t.Helper()
	ctx := context.Background()
	policy, err := fixture.store.GetMirrorPolicy(ctx, policyUUID)
	if err != nil || !policy.Enabled || policy.PolicyVersion != wantVersion {
		t.Fatalf("adopted policy=%#v error=%v", policy, err)
	}
	channels, channelErr := fixture.store.ListActiveMirrorChannelOwnerships(ctx, policyUUID)
	models, modelErr := fixture.store.ListActiveMirrorModelOwnerships(ctx, policyUUID)
	members, memberErr := fixture.store.ListActiveMirrorChannelModelOwnerships(ctx, policyUUID)
	if channelErr != nil || modelErr != nil || memberErr != nil || len(channels) != 1 || len(models) != 2 || len(members) != 2 {
		t.Fatalf("ownership channel=%#v models=%#v members=%#v errors=%v/%v/%v",
			channels, models, members, channelErr, modelErr, memberErr)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].TargetModel > models[j].TargetModel })
	sort.Slice(members, func(i, j int) bool { return members[i].TargetModel < members[j].TargetModel })
	wanted, err := adoptionOwnership(policy, fixture.target.managed, fixture.target.channel, fixture.target.contract)
	if err != nil || !exactAdoptionOwnership(ctx, fixture.store, wanted) {
		t.Fatalf("order-independent ownership readback failed: error=%v", err)
	}
}

func cloneAdoptionContractForTest(source domain.GroupModelPricingContract) domain.GroupModelPricingContract {
	payload, _ := json.Marshal(source)
	var result domain.GroupModelPricingContract
	_ = json.Unmarshal(payload, &result)
	return result
}

func ptrAdoptionContract(value domain.GroupModelPricingContract) *domain.GroupModelPricingContract {
	return &value
}
