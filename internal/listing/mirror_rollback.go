package listing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func (s *Service) preflightNativeMirrorRollback(ctx context.Context, plan domain.ListingPlan,
	change domain.ListingChangeSet) error {
	desired, configured, err := nativeMirrorPolicyForPlan(plan)
	if err != nil || !configured {
		return err
	}
	current, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if err != nil {
		return rollbackConflict("complete mirror policy inspection failed: %v", err)
	}
	if !current.Enabled || current.PolicyVersion != 2 || !sameNativeMirrorDefinition(current, desired) {
		return rollbackConflict("complete mirror policy changed or was disabled after apply")
	}
	if err := s.verifyCompleteMirrorOwnership(ctx, plan, change, current.PolicyUUID); err != nil {
		return rollbackConflict("complete mirror ownership changed after apply: %v", err)
	}
	return nil
}

func (s *Service) prepareNativeMirrorRollback(ctx context.Context, plan domain.ListingPlan,
	change *domain.ListingChangeSet) error {
	desired, configured, err := nativeMirrorPolicyForPlan(plan)
	if err != nil || !configured {
		return err
	}
	current, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if err != nil {
		return rollbackConflict("complete mirror policy inspection failed: %v", err)
	}
	if !sameNativeMirrorDefinition(current, desired) {
		return rollbackConflict("complete mirror policy definition changed during rollback")
	}
	if current.Enabled {
		if current.PolicyVersion != 2 {
			return rollbackConflict("complete mirror policy version changed before rollback")
		}
		current.Enabled = false
		if _, err := s.store.UpdateMirrorPolicy(ctx, current, current.PolicyVersion, time.Now().UTC()); err != nil {
			return rollbackConflict("disable complete mirror policy: %v", err)
		}
	} else if current.PolicyVersion != 3 {
		return rollbackConflict("disabled complete mirror policy has an unexpected version")
	}
	if !rollbackStepAtLeast(change.Step, "rollback_native_policy_disabled") {
		change.Step = "rollback_native_policy_disabled"
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "complete mirror policy disabled")
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
			return err
		}
	}
	if rollbackStepAtLeast(change.Step, "rollback_native_ownership_released") {
		return nil
	}
	if err := s.releaseCompleteMirrorOwnership(ctx, plan, *change, desired.PolicyUUID); err != nil {
		return err
	}
	change.Step = "rollback_native_ownership_released"
	change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "complete mirror item ownership released")
	return s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"})
}

func (s *Service) verifyCompleteMirrorOwnership(ctx context.Context, plan domain.ListingPlan,
	change domain.ListingChangeSet, policyUUID string) error {
	if change.CreatedChannelID == nil {
		return errors.New("listing receipt has no managed channel")
	}
	channels, err := s.store.ListActiveMirrorChannelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	models, err := s.store.ListActiveMirrorModelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	memberships, err := s.store.ListActiveMirrorChannelModelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	expectedModels := sortedUnique(plan.Channel.Models)
	if len(channels) != 1 || len(models) != len(expectedModels) || len(memberships) != len(expectedModels) {
		return errors.New("complete mirror ownership cardinality differs from the frozen listing")
	}
	return s.verifyActiveMirrorOwnershipRecords(ctx, plan, *change.CreatedChannelID, policyUUID,
		channels, models, memberships, true)
}

func (s *Service) releaseCompleteMirrorOwnership(ctx context.Context, plan domain.ListingPlan,
	change domain.ListingChangeSet, policyUUID string) error {
	if change.CreatedChannelID == nil {
		return rollbackConflict("listing receipt has no managed channel")
	}
	channels, err := s.store.ListActiveMirrorChannelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	models, err := s.store.ListActiveMirrorModelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	memberships, err := s.store.ListActiveMirrorChannelModelOwnerships(ctx, policyUUID)
	if err != nil {
		return err
	}
	if len(channels) == 0 && len(models) == 0 && len(memberships) == 0 {
		return nil
	}
	if err := s.verifyActiveMirrorOwnershipRecords(ctx, plan, *change.CreatedChannelID, policyUUID,
		channels, models, memberships, false); err != nil {
		return rollbackConflict("complete mirror ownership release preflight failed: %v", err)
	}
	now := time.Now().UTC()
	lease, err := s.store.AcquireMirrorLease(ctx, plan.TargetSiteID,
		fmt.Sprintf("listing:%d:mirror-rollback", change.ID), now, 10*time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	for _, item := range memberships {
		if err := s.store.ReleaseMirrorChannelModelOwnership(ctx, item.TargetSiteID, item.ChannelID,
			item.TargetModel, item.PolicyUUID, item.OwnershipMarker, item.LastAppliedFunctionalHash, lease, now); err != nil {
			return err
		}
	}
	for _, item := range models {
		if err := s.store.ReleaseMirrorModelOwnership(ctx, item.TargetSiteID, item.TargetGroup,
			item.TargetModel, item.PolicyUUID, item.OwnershipMarker, item.LastAppliedFunctionalHash, lease, now); err != nil {
			return err
		}
	}
	for _, item := range channels {
		if err := s.store.ReleaseMirrorChannelOwnership(ctx, item.TargetSiteID, item.ChannelID,
			item.PolicyUUID, item.OwnershipMarker, item.LastAppliedFunctionalHash, lease, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) verifyActiveMirrorOwnershipRecords(ctx context.Context, plan domain.ListingPlan,
	channelID int64, policyUUID string, channels []store.MirrorChannelOwnership,
	models []store.MirrorModelOwnership, memberships []store.MirrorChannelModelOwnership, requireComplete bool) error {
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
	channelState, err := managed.ReadManagedChannelState(ctx, target, channelID)
	if err != nil {
		return err
	}
	contract, err := contractAdapter.ReadGroupModelPricing(ctx, target)
	if err != nil {
		return err
	}
	group, exists := contract.Groups[plan.TargetGroup]
	if !exists {
		return errors.New("target group-model contract no longer contains the managed group")
	}
	marker := "relayops:policy/" + policyUUID
	if requireComplete {
		expectedModels := sortedUnique(plan.Channel.Models)
		if strings.Join(channelState.Models, "\x00") != strings.Join(expectedModels, "\x00") {
			return errors.New("managed channel model set differs from the frozen listing")
		}
	}
	available := make(map[string]struct{}, len(channelState.Models))
	for _, modelName := range channelState.Models {
		available[modelName] = struct{}{}
	}
	for _, item := range channels {
		if item.PolicyUUID != policyUUID || item.TargetSiteID != plan.TargetSiteID || item.ChannelID != channelID ||
			item.TargetGroup != plan.TargetGroup || item.OwnershipMarker != marker ||
			item.LastAppliedFunctionalHash != channelState.Hash {
			return errors.New("managed channel ownership marker or functional hash changed")
		}
	}
	seenModels := make(map[string]struct{}, len(models))
	for _, item := range models {
		setting, exists := group[item.TargetModel]
		if !exists || strings.TrimSpace(setting.Source) != marker {
			return fmt.Errorf("managed model %q is missing or has a foreign marker", item.TargetModel)
		}
		hash, err := mirror.TargetModelFunctionalHash(plan.TargetGroup, item.TargetModel, setting)
		if err != nil || hash != item.LastAppliedFunctionalHash || item.PolicyUUID != policyUUID ||
			item.TargetSiteID != plan.TargetSiteID || item.TargetGroup != plan.TargetGroup || item.OwnershipMarker != marker {
			return fmt.Errorf("managed model %q functional ownership changed", item.TargetModel)
		}
		seenModels[item.TargetModel] = struct{}{}
	}
	seenMemberships := make(map[string]struct{}, len(memberships))
	for _, item := range memberships {
		if _, exists := available[item.TargetModel]; !exists {
			return fmt.Errorf("managed channel no longer exposes owned model %q", item.TargetModel)
		}
		hash, err := mirror.TargetChannelModelFunctionalHash(channelID, item.TargetModel, true)
		if err != nil || hash != item.LastAppliedFunctionalHash || item.PolicyUUID != policyUUID ||
			item.TargetSiteID != plan.TargetSiteID || item.TargetGroup != plan.TargetGroup || item.ChannelID != channelID ||
			item.OwnershipMarker != marker {
			return fmt.Errorf("managed channel membership %q functional ownership changed", item.TargetModel)
		}
		seenMemberships[item.TargetModel] = struct{}{}
	}
	if requireComplete {
		for _, modelName := range sortedUnique(plan.Channel.Models) {
			if _, exists := seenModels[modelName]; !exists {
				return fmt.Errorf("managed model ownership %q is missing", modelName)
			}
			if _, exists := seenMemberships[modelName]; !exists {
				return fmt.Errorf("managed channel membership ownership %q is missing", modelName)
			}
		}
	}
	return nil
}
