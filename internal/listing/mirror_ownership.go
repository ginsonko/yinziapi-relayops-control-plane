package listing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const managedChannelEnabledStatus = 1

func (s *Service) ensureNativeMirrorPolicyDraft(ctx context.Context, plan domain.ListingPlan) (mirror.Policy, bool, error) {
	desired, configured, err := nativeMirrorPolicyForPlan(plan)
	if err != nil || !configured {
		return desired, configured, err
	}
	existing, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if err == nil {
		if !sameNativeMirrorDefinition(existing, desired) {
			return mirror.Policy{}, true, errors.New("mirror policy UUID is already bound to a different definition")
		}
		return existing, true, nil
	}
	if !store.IsNotFound(err) {
		return mirror.Policy{}, true, err
	}
	if err := s.store.CreateMirrorPolicy(ctx, desired, time.Now().UTC()); err != nil {
		if existing, getErr := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID); getErr == nil &&
			sameNativeMirrorDefinition(existing, desired) {
			return existing, true, nil
		}
		return mirror.Policy{}, true, err
	}
	return desired, true, nil
}

func sameNativeMirrorDefinition(left, right mirror.Policy) bool {
	left.PolicyVersion, right.PolicyVersion = 1, 1
	left.Enabled, right.Enabled = false, false
	left.LegacyPolicyID, right.LegacyPolicyID = 0, 0
	return reflect.DeepEqual(left, right)
}

func (s *Service) ensureNativeMirrorOwnership(ctx context.Context, plan domain.ListingPlan,
	change *domain.ListingChangeSet) error {
	policy, configured, err := s.ensureNativeMirrorPolicyDraft(ctx, plan)
	if err != nil || !configured {
		return err
	}
	if plan.GroupModelPricing.Mode != "atomic_contract" || change.CreatedChannelID == nil {
		return errors.New("complete mirror ownership requires an atomic contract and a created channel")
	}
	target, err := s.store.GetTarget(ctx, plan.TargetSiteID)
	if err != nil {
		return err
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		return err
	}
	managed, err := targets.AsManagedChannelStateAdapter(adapter)
	if err != nil {
		return err
	}
	contractAdapter, err := targets.AsGroupModelPricingAdapter(adapter)
	if err != nil {
		return err
	}
	channelState, err := managed.ReadManagedChannelState(ctx, target, *change.CreatedChannelID)
	if err != nil {
		return err
	}
	if channelState.Status != managedChannelEnabledStatus {
		return fmt.Errorf("managed channel %d is not enabled", channelState.ChannelID)
	}
	expectedModels := sortedUnique(plan.Channel.Models)
	if strings.Join(channelState.Models, "\x00") != strings.Join(expectedModels, "\x00") {
		return errors.New("managed channel model readback differs from the frozen listing model set")
	}
	contract, err := contractAdapter.ReadGroupModelPricing(ctx, target)
	if err != nil {
		return err
	}
	if contract.Revision != plan.GroupModelPricing.DesiredRevision {
		return errors.New("group-model contract changed before mirror ownership could be claimed")
	}
	group := contract.Groups[plan.TargetGroup]
	marker := listingMirrorOwnershipMarker(plan.PricingRule)
	if marker == "" {
		return errors.New("complete mirror ownership marker is missing")
	}

	now := time.Now().UTC()
	lease, err := s.store.AcquireMirrorLease(ctx, plan.TargetSiteID,
		fmt.Sprintf("listing:%d:mirror-adoption", change.ID), now, 10*time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	sourceGroupIdentity := fmt.Sprintf("supplier/%d/group/%s", plan.SupplierID, plan.SourceGroup)
	desired := store.MirrorOwnershipState{Channel: store.MirrorChannelOwnership{
		PolicyUUID: policy.PolicyUUID, TargetSiteID: plan.TargetSiteID, TargetGroup: plan.TargetGroup,
		ChannelID: channelState.ChannelID, OwnershipMarker: marker, SourceIdentity: sourceGroupIdentity,
		ModelSetAuthority: "exclusive", StatusAuthority: "disable_only",
		LastAppliedFunctionalHash: channelState.Hash,
	}}
	for _, modelName := range expectedModels {
		setting, exists := group[modelName]
		if !exists {
			return fmt.Errorf("target contract is missing listed model %q", modelName)
		}
		if strings.TrimSpace(setting.Source) != marker {
			return fmt.Errorf("target model %q has foreign ownership marker %q", modelName, setting.Source)
		}
		modelHash, err := mirror.TargetModelFunctionalHash(plan.TargetGroup, modelName, setting)
		if err != nil {
			return err
		}
		desired.Models = append(desired.Models, store.MirrorModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: plan.TargetSiteID,
			TargetGroup: plan.TargetGroup, TargetModel: modelName, OwnershipMarker: marker,
			SourceIdentity: sourceGroupIdentity + "/model/" + modelName, LastAppliedFunctionalHash: modelHash})
		membershipHash, err := mirror.TargetChannelModelFunctionalHash(channelState.ChannelID, modelName, true)
		if err != nil {
			return err
		}
		desired.Memberships = append(desired.Memberships, store.MirrorChannelModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: plan.TargetSiteID,
			TargetGroup: plan.TargetGroup, ChannelID: channelState.ChannelID, TargetModel: modelName,
			OwnershipMarker: marker, SourceIdentity: sourceGroupIdentity + "/model/" + modelName,
			LastAppliedFunctionalHash: membershipHash})
	}
	_, _, err = s.store.AdoptMirrorOwnershipAndEnable(ctx, desired, policy.PolicyVersion, lease, time.Now().UTC())
	return err
}
