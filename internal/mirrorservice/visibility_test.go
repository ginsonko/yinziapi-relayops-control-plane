package mirrorservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type visibilityTarget struct {
	receipt     targets.MirrorVisibilityReceipt
	state       domain.TargetManagedChannelState
	contract    domain.GroupModelPricingContract
	receiptErr  error
	stateErr    error
	contractErr error
}

func (target *visibilityTarget) Name() string { return "visibility-test" }

func (target *visibilityTarget) Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, nil
}

func (target *visibilityTarget) ApplyGroupRatio(context.Context, domain.TargetSite, string,
	map[string]string) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, errors.New("unexpected write")
}

func (target *visibilityTarget) RollbackGroupRatio(context.Context, domain.TargetSite, string,
	map[string]string) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, errors.New("unexpected write")
}

func (target *visibilityTarget) ReadMirrorVisibility(context.Context, domain.TargetSite, int64,
	string) (targets.MirrorVisibilityReceipt, error) {
	return target.receipt, target.receiptErr
}

func (target *visibilityTarget) ReadManagedChannelState(context.Context, domain.TargetSite,
	int64) (domain.TargetManagedChannelState, error) {
	return target.state, target.stateErr
}

func (target *visibilityTarget) ReadGroupModelPricing(context.Context,
	domain.TargetSite) (domain.GroupModelPricingContract, error) {
	return target.contract, target.contractErr
}

func TestVisibilityVerifierBindsReceiptToIndependentReadsAndDesiredModels(t *testing.T) {
	target := newVisibilityTarget([]string{"model-b", "model-a"})
	verifier := newVisibilityVerifier(func(domain.TargetSite) (targets.Adapter, error) { return target, nil })

	evidence, err := verifier.VerifyMirrorVisibility(context.Background(), domain.TargetSite{ID: 9},
		41, "sale", []string{"model-a", "model-b", "model-a"})
	if err != nil {
		t.Fatalf("VerifyMirrorVisibility: %v", err)
	}
	if !evidence.MarketplaceVisible || !evidence.AbilityVisible || !evidence.CacheVisible ||
		evidence.EvidenceHash != target.receipt.EvidenceHash || !strings.Contains(string(evidence.Details), `"model_count":2`) {
		t.Fatalf("evidence=%#v", evidence)
	}
}

func TestVisibilityVerifierRejectsEveryIndependentBindingMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*visibilityTarget)
	}{
		{name: "channel state", mutate: func(value *visibilityTarget) { value.receipt.ChannelStateHash = strings.Repeat("f", 64) }},
		{name: "contract", mutate: func(value *visibilityTarget) { value.receipt.ContractRevision = strings.Repeat("f", 64) }},
		{name: "observed models", mutate: func(value *visibilityTarget) { value.state.Models = []string{"other"} }},
		{name: "receipt model hash", mutate: func(value *visibilityTarget) { value.receipt.ModelSetHash = strings.Repeat("f", 64) }},
		{name: "receipt evidence hash", mutate: func(value *visibilityTarget) { value.receipt.EvidenceHash = strings.Repeat("f", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := newVisibilityTarget([]string{"model-a"})
			test.mutate(target)
			verifier := newVisibilityVerifier(func(domain.TargetSite) (targets.Adapter, error) { return target, nil })
			_, err := verifier.VerifyMirrorVisibility(context.Background(), domain.TargetSite{ID: 9},
				41, "sale", []string{"model-a"})
			if !errors.Is(err, ErrVisibilityUnavailable) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestVisibilityVerifierReturnsBoundedEvidenceWhenPublicationIsPending(t *testing.T) {
	target := newVisibilityTarget([]string{"model-a"})
	target.receipt.CacheVisible = false
	target.receipt.EvidenceHash = targets.HashMirrorVisibilityReceipt(target.receipt)
	verifier := newVisibilityVerifier(func(domain.TargetSite) (targets.Adapter, error) { return target, nil })

	evidence, err := verifier.VerifyMirrorVisibility(context.Background(), domain.TargetSite{ID: 9},
		41, "sale", []string{"model-a"})
	if !errors.Is(err, ErrVisibilityUnavailable) || evidence.CacheVisible {
		t.Fatalf("evidence=%#v error=%v", evidence, err)
	}
}

func TestVisibilityCapabilityProbeFailsClosedForUnsupportedAdapter(t *testing.T) {
	verifier := newVisibilityVerifier(func(domain.TargetSite) (targets.Adapter, error) {
		return &adapterWithoutVisibility{}, nil
	})
	if err := verifier.ProbeMirrorVisibility(context.Background(), domain.TargetSite{ID: 9}, 41, "sale"); !errors.Is(err, ErrVisibilityUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

type adapterWithoutVisibility struct{}

func (*adapterWithoutVisibility) Name() string { return "legacy" }
func (*adapterWithoutVisibility) Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, nil
}
func (*adapterWithoutVisibility) ApplyGroupRatio(context.Context, domain.TargetSite, string,
	map[string]string) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, errors.New("unexpected write")
}
func (*adapterWithoutVisibility) RollbackGroupRatio(context.Context, domain.TargetSite, string,
	map[string]string) (domain.TargetSnapshot, error) {
	return domain.TargetSnapshot{}, errors.New("unexpected write")
}

func newVisibilityTarget(models []string) *visibilityTarget {
	stateHash := strings.Repeat("a", 64)
	contractRevision := strings.Repeat("b", 64)
	receipt := targets.MirrorVisibilityReceipt{
		Schema: "newapi-mirror-visibility-v1", ChannelID: 41, Group: "sale",
		ChannelStateHash: stateHash, ModelSetHash: targets.HashMirrorModelSet("sale", models), ModelCount: len(models),
		ContractRevision: contractRevision, MarketplaceHash: strings.Repeat("c", 64),
		AbilityHash: strings.Repeat("d", 64), CacheHash: strings.Repeat("e", 64),
		MarketplaceVisible: true, AbilityVisible: true, CacheVisible: true, CacheMode: "memory",
	}
	receipt.EvidenceHash = targets.HashMirrorVisibilityReceipt(receipt)
	return &visibilityTarget{receipt: receipt,
		state:    domain.TargetManagedChannelState{ChannelID: 41, Models: models, Status: 1, Hash: stateHash},
		contract: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: contractRevision}}
}
