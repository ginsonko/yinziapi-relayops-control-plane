package mirrorservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorexec"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type mirrorVisibilityTarget interface {
	targets.Adapter
	ReadMirrorVisibility(context.Context, domain.TargetSite, int64, string) (targets.MirrorVisibilityReceipt, error)
	ReadManagedChannelState(context.Context, domain.TargetSite, int64) (domain.TargetManagedChannelState, error)
	ReadGroupModelPricing(context.Context, domain.TargetSite) (domain.GroupModelPricingContract, error)
}

// VisibilityVerifier binds NewAPI's independent Marketplace, Ability and cache
// receipt to RelayOps' desired model set and independent state reads.
type VisibilityVerifier struct {
	resolve func(domain.TargetSite) (targets.Adapter, error)
}

func NewVisibilityVerifier(factory *targets.Factory) (*VisibilityVerifier, error) {
	if factory == nil {
		return nil, errors.New("mirror visibility target factory is required")
	}
	return &VisibilityVerifier{resolve: factory.ForTarget}, nil
}

func newVisibilityVerifier(resolve func(domain.TargetSite) (targets.Adapter, error)) *VisibilityVerifier {
	return &VisibilityVerifier{resolve: resolve}
}

// ProbeMirrorVisibility proves that the exact target exposes a valid bounded
// receipt before the executor is allowed to make its first remote mutation.
// Existing false visibility flags do not fail this capability probe because
// the pending mirror write may be the operation that makes them current.
func (v *VisibilityVerifier) ProbeMirrorVisibility(ctx context.Context, target domain.TargetSite,
	channelID int64, group string) error {
	_, state, _, err := v.observe(ctx, target, channelID, group)
	if err != nil {
		return err
	}
	if state.ChannelID != channelID {
		return fmt.Errorf("%w: managed channel binding changed", ErrVisibilityUnavailable)
	}
	return nil
}

func (v *VisibilityVerifier) VerifyMirrorVisibility(ctx context.Context, target domain.TargetSite,
	channelID int64, group string, desiredModels []string) (mirrorexec.VisibilityEvidence, error) {
	receipt, state, _, err := v.observe(ctx, target, channelID, group)
	if err != nil {
		return mirrorexec.VisibilityEvidence{}, err
	}
	desiredModels = canonicalVisibilityModels(desiredModels)
	if receipt.ModelCount != len(desiredModels) ||
		receipt.ModelSetHash != targets.HashMirrorModelSet(group, desiredModels) ||
		strings.Join(state.Models, "\x00") != strings.Join(desiredModels, "\x00") {
		return mirrorexec.VisibilityEvidence{}, fmt.Errorf(
			"%w: visible model set differs from the complete-mirror plan", ErrVisibilityUnavailable)
	}
	evidence, err := visibilityEvidence(receipt)
	if err != nil {
		return mirrorexec.VisibilityEvidence{}, err
	}
	if !receipt.MarketplaceVisible || !receipt.AbilityVisible || !receipt.CacheVisible {
		return evidence, fmt.Errorf("%w: marketplace, ability or cache publication is pending",
			ErrVisibilityUnavailable)
	}
	return evidence, nil
}

func (v *VisibilityVerifier) observe(ctx context.Context, target domain.TargetSite, channelID int64,
	group string) (targets.MirrorVisibilityReceipt, domain.TargetManagedChannelState,
	domain.GroupModelPricingContract, error) {
	if v == nil || v.resolve == nil {
		return targets.MirrorVisibilityReceipt{}, domain.TargetManagedChannelState{},
			domain.GroupModelPricingContract{}, ErrVisibilityUnavailable
	}
	adapter, err := v.resolve(target)
	if err != nil {
		return targets.MirrorVisibilityReceipt{}, domain.TargetManagedChannelState{},
			domain.GroupModelPricingContract{}, fmt.Errorf("%w: %v", ErrVisibilityUnavailable, err)
	}
	reader, ok := adapter.(mirrorVisibilityTarget)
	if !ok {
		return targets.MirrorVisibilityReceipt{}, domain.TargetManagedChannelState{},
			domain.GroupModelPricingContract{}, fmt.Errorf(
				"%w: target adapter %q lacks the mirror visibility contract", ErrVisibilityUnavailable, adapter.Name())
	}

	receipt, err := reader.ReadMirrorVisibility(ctx, target, channelID, group)
	if err != nil {
		return targets.MirrorVisibilityReceipt{}, domain.TargetManagedChannelState{},
			domain.GroupModelPricingContract{}, fmt.Errorf("%w: %v", ErrVisibilityUnavailable, err)
	}
	state, err := reader.ReadManagedChannelState(ctx, target, channelID)
	if err != nil {
		return receipt, domain.TargetManagedChannelState{}, domain.GroupModelPricingContract{},
			fmt.Errorf("%w: read independent channel state: %v", ErrVisibilityUnavailable, err)
	}
	contract, err := reader.ReadGroupModelPricing(ctx, target)
	if err != nil {
		return receipt, state, domain.GroupModelPricingContract{},
			fmt.Errorf("%w: read independent pricing contract: %v", ErrVisibilityUnavailable, err)
	}
	state.Models = canonicalVisibilityModels(state.Models)
	if receipt.ChannelID != channelID || receipt.Group != strings.TrimSpace(group) ||
		receipt.ChannelStateHash != strings.TrimSpace(state.Hash) ||
		receipt.ContractRevision != strings.TrimSpace(contract.Revision) ||
		receipt.ModelCount != len(state.Models) ||
		receipt.ModelSetHash != targets.HashMirrorModelSet(group, state.Models) ||
		targets.HashMirrorVisibilityReceipt(receipt) != receipt.EvidenceHash {
		return receipt, state, contract, fmt.Errorf(
			"%w: receipt differs from independent channel or pricing state", ErrVisibilityUnavailable)
	}
	return receipt, state, contract, nil
}

func visibilityEvidence(receipt targets.MirrorVisibilityReceipt) (mirrorexec.VisibilityEvidence, error) {
	details, err := json.Marshal(struct {
		Schema           string `json:"schema"`
		ChannelID        int64  `json:"channel_id"`
		Group            string `json:"group"`
		ModelCount       int    `json:"model_count"`
		ChannelStateHash string `json:"channel_state_hash"`
		ModelSetHash     string `json:"model_set_hash"`
		ContractRevision string `json:"contract_revision"`
		MarketplaceHash  string `json:"marketplace_hash"`
		AbilityHash      string `json:"ability_hash"`
		CacheHash        string `json:"cache_hash"`
		CacheMode        string `json:"cache_mode"`
	}{receipt.Schema, receipt.ChannelID, receipt.Group, receipt.ModelCount, receipt.ChannelStateHash,
		receipt.ModelSetHash, receipt.ContractRevision, receipt.MarketplaceHash, receipt.AbilityHash,
		receipt.CacheHash, receipt.CacheMode})
	if err != nil {
		return mirrorexec.VisibilityEvidence{}, fmt.Errorf("encode mirror visibility evidence: %w", err)
	}
	return mirrorexec.VisibilityEvidence{
		MarketplaceVisible: receipt.MarketplaceVisible,
		AbilityVisible:     receipt.AbilityVisible,
		CacheVisible:       receipt.CacheVisible,
		EvidenceHash:       receipt.EvidenceHash,
		Details:            details,
	}, nil
}

func canonicalVisibilityModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, modelName := range models {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			continue
		}
		if _, exists := seen[modelName]; exists {
			continue
		}
		seen[modelName] = struct{}{}
		result = append(result, modelName)
	}
	sort.Strings(result)
	return result
}
