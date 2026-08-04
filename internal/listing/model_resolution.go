package listing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const listingDisabledChannelStatus = 2

func listingHasModelResolution(change domain.ListingChangeSet) bool {
	receipt := change.GroupModelPricing
	return strings.TrimSpace(receipt.DiscoveryStatus) != "" && len(receipt.RequestedModels) > 0 && len(receipt.EffectiveModels) > 0
}

func resolveListingPlanFromReceipt(plan domain.ListingPlan, change domain.ListingChangeSet) (domain.ListingPlan, error) {
	if !listingHasModelResolution(change) {
		return plan, nil
	}
	requested := sortedUnique(change.GroupModelPricing.RequestedModels)
	effective := sortedUnique(change.GroupModelPricing.EffectiveModels)
	excluded := sortedUnique(change.GroupModelPricing.ExcludedModels)
	current := sortedUnique(plan.Channel.Models)
	if strings.Join(current, "\x00") == strings.Join(effective, "\x00") {
		if len(intersectModels(excluded, plan.PricingRule.MirrorExcludedModels)) != len(excluded) {
			return domain.ListingPlan{}, errors.New("resolved listing plan is missing receipt-owned mirror exceptions")
		}
		return restoreAuthoritativeResolvedPricing(plan, change)
	}
	if strings.Join(requested, "\x00") != strings.Join(current, "\x00") {
		return domain.ListingPlan{}, errors.New("listing model-resolution receipt does not match the frozen request")
	}
	resolved, err := resolveListingPlan(plan, effective, excluded)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	return restoreAuthoritativeResolvedPricing(resolved, change)
}

func restoreAuthoritativeResolvedPricing(plan domain.ListingPlan, change domain.ListingChangeSet) (domain.ListingPlan, error) {
	if plan.GroupModelPricing.Mode != "atomic_contract" {
		return plan, nil
	}
	receipt := change.GroupModelPricing
	if receipt.ExpectedRevision != plan.GroupModelPricing.ExpectedRevision || receipt.Before.Revision != receipt.ExpectedRevision {
		return domain.ListingPlan{}, errors.New("resolved listing receipt has inconsistent expected pricing authority")
	}
	if receipt.DesiredRevision == "" || receipt.After.Revision != receipt.DesiredRevision {
		return domain.ListingPlan{}, errors.New("resolved listing receipt has no authoritative desired pricing revision")
	}
	if !sameGroupModelPricingSemantics(plan.GroupModelPricing.ExpectedContract, receipt.Before) {
		return domain.ListingPlan{}, errors.New("resolved listing receipt changed the expected pricing contract")
	}
	if !sameGroupModelPricingSemantics(plan.GroupModelPricing.DesiredContract, receipt.After) {
		return domain.ListingPlan{}, errors.New("resolved listing receipt changed the filtered pricing contract")
	}
	after, err := cloneGroupModelContract(receipt.After)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	plan.GroupModelPricing.DesiredContract = after
	plan.GroupModelPricing.DesiredRevision = receipt.DesiredRevision
	plan.GroupModelPricing.Diff = append([]domain.TargetFieldDiff(nil), receipt.Diff...)
	return plan, nil
}

func bindAuthoritativeResolvedPricing(ctx context.Context, adapter targets.GroupModelPricingAdapter,
	target domain.TargetSite, plan domain.ListingPlan) (domain.ListingPlan, error) {
	if plan.GroupModelPricing.Mode != "atomic_contract" {
		return plan, nil
	}
	preview, err := adapter.PreviewGroupModelPricing(targets.WithFreshRead(ctx), target,
		plan.GroupModelPricing.ExpectedRevision, plan.GroupModelPricing.DesiredContract)
	if err != nil {
		return domain.ListingPlan{}, fmt.Errorf("preview filtered group-model pricing: %w", err)
	}
	if !preview.ExpectedRevisionMatches || preview.CurrentRevision != plan.GroupModelPricing.ExpectedRevision {
		return domain.ListingPlan{}, staleError{"target group-model pricing changed while unavailable models were filtered"}
	}
	if preview.DesiredRevision == "" || preview.Desired.Revision != preview.DesiredRevision {
		return domain.ListingPlan{}, errors.New("target returned no authoritative revision for filtered group-model pricing")
	}
	if !sameGroupModelPricingSemantics(plan.GroupModelPricing.DesiredContract, preview.Desired) {
		return domain.ListingPlan{}, errors.New("target group-model pricing preview changed content outside the filtered listing")
	}
	desired, err := cloneGroupModelContract(preview.Desired)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	plan.GroupModelPricing.DesiredContract = desired
	plan.GroupModelPricing.DesiredRevision = preview.DesiredRevision
	plan.GroupModelPricing.Diff = append([]domain.TargetFieldDiff(nil), preview.Diff...)
	return plan, nil
}

func sameGroupModelPricingSemantics(left, right domain.GroupModelPricingContract) bool {
	leftHash := targets.HashGroupModelPricingContract(left)
	return leftHash != "" && leftHash == targets.HashGroupModelPricingContract(right)
}

func resolveListingPlan(plan domain.ListingPlan, effectiveModels, excludedModels []string) (domain.ListingPlan, error) {
	requested := sortedUnique(plan.Channel.Models)
	effective := sortedUnique(effectiveModels)
	excluded := sortedUnique(excludedModels)
	if len(effective) == 0 {
		return domain.ListingPlan{}, errors.New("listing has no effective models")
	}
	requestedSet := modelSet(requested)
	for _, model := range effective {
		if _, ok := requestedSet[model]; !ok {
			return domain.ListingPlan{}, fmt.Errorf("effective model %q is outside the frozen listing request", model)
		}
	}
	for _, model := range excluded {
		if _, ok := requestedSet[model]; !ok {
			return domain.ListingPlan{}, fmt.Errorf("excluded model %q is outside the frozen listing request", model)
		}
	}
	if overlap := intersectModels(effective, excluded); len(overlap) > 0 {
		return domain.ListingPlan{}, fmt.Errorf("effective and excluded model sets overlap: %s", strings.Join(overlap, ", "))
	}
	if len(effective)+len(excluded) != len(requested) {
		return domain.ListingPlan{}, errors.New("effective and excluded model sets do not cover the frozen listing request")
	}

	plan.Channel.Models = effective
	plan.PricingRule.MirrorExcludedModels = sortedUnique(append(
		append([]string(nil), plan.PricingRule.MirrorExcludedModels...), excluded...))
	if plan.GroupModelPricing.Mode != "atomic_contract" {
		return plan, nil
	}

	expected, err := cloneGroupModelContract(plan.GroupModelPricing.ExpectedContract)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	desired, err := cloneGroupModelContract(expected)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	if desired.Groups == nil {
		desired.Groups = map[string]map[string]domain.GroupModelPricingSetting{}
	}
	if desired.Groups[plan.TargetGroup] == nil {
		desired.Groups[plan.TargetGroup] = map[string]domain.GroupModelPricingSetting{}
	}
	plannedGroup := plan.GroupModelPricing.DesiredContract.Groups[plan.TargetGroup]
	for _, model := range effective {
		setting, exists := plannedGroup[model]
		if !exists {
			return domain.ListingPlan{}, fmt.Errorf("frozen pricing contract is missing effective model %q", model)
		}
		desired.Groups[plan.TargetGroup][model] = setting
	}
	if len(desired.Groups[plan.TargetGroup]) == 0 {
		delete(desired.Groups, plan.TargetGroup)
	}
	desired.Revision = targets.HashGroupModelPricingContract(desired)
	if desired.Revision == "" {
		return domain.ListingPlan{}, errors.New("resolved group-model pricing contract is invalid")
	}

	modelEvidence := make([]domain.ListingModelPricingEvidence, 0, len(effective))
	effectiveSet := modelSet(effective)
	minimum := decimal.Zero
	minimumDefined := false
	for _, item := range plan.GroupModelPricing.Models {
		if _, keep := effectiveSet[item.Model]; !keep {
			continue
		}
		modelEvidence = append(modelEvidence, item)
		if !item.MinimumMarginDefined {
			continue
		}
		value, parseErr := decimal.NewFromString(strings.TrimSpace(item.MinimumMarginRate))
		if parseErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("model %q has invalid frozen margin", item.Model)
		}
		if !minimumDefined || value.LessThan(minimum) {
			minimum = value
			minimumDefined = true
		}
	}
	if len(modelEvidence) != len(effective) {
		return domain.ListingPlan{}, errors.New("resolved listing model evidence is incomplete")
	}
	plan.GroupModelPricing.Models = modelEvidence
	plan.GroupModelPricing.MinimumMarginDefined = minimumDefined
	if minimumDefined {
		plan.GroupModelPricing.MinimumMarginRate = minimum.String()
	}
	plan.GroupModelPricing.DesiredContract = desired
	plan.GroupModelPricing.DesiredRevision = desired.Revision
	plan.GroupModelPricing.Diff = filterListingPricingDiff(plan.GroupModelPricing.Diff, plan.TargetGroup, effectiveSet)

	global := &plan.GroupModelPricing.GlobalModelPrice
	if global.Supported {
		global.Desired = cloneRatios(global.Expected)
		filteredAdded := make(map[string]string)
		for model, value := range global.Added {
			if _, keep := effectiveSet[model]; !keep {
				continue
			}
			global.Desired[model] = value
			filteredAdded[model] = value
		}
		global.Added = filteredAdded
		global.ExpectedHash = targets.HashDecimalMap(global.Expected)
		global.DesiredHash = targets.HashDecimalMap(global.Desired)
		global.Changed = global.ExpectedHash != global.DesiredHash
	}
	return plan, nil
}

func filterListingPricingDiff(diff []domain.TargetFieldDiff, targetGroup string, effective map[string]struct{}) []domain.TargetFieldDiff {
	prefix := targetGroup + "/"
	result := make([]domain.TargetFieldDiff, 0, len(diff))
	for _, item := range diff {
		if !strings.HasPrefix(item.Field, prefix) {
			result = append(result, item)
			continue
		}
		model := strings.TrimPrefix(item.Field, prefix)
		if _, keep := effective[model]; keep {
			result = append(result, item)
		}
	}
	return result
}

func recordListingModelResolution(change *domain.ListingChangeSet, plan domain.ListingPlan,
	requested, effective, excluded []string, status string) {
	previous := change.GroupModelPricing
	change.GroupModelPricing = domain.ListingGroupModelPricingReceipt{
		Mode: plan.GroupModelPricing.Mode, ExpectedRevision: plan.GroupModelPricing.ExpectedRevision,
		DesiredRevision: plan.GroupModelPricing.DesiredRevision, AppliedRevision: previous.AppliedRevision,
		Before: plan.GroupModelPricing.ExpectedContract, After: plan.GroupModelPricing.DesiredContract,
		Diff: append([]domain.TargetFieldDiff(nil), plan.GroupModelPricing.Diff...), Changed: previous.Changed,
		RollbackStatus:  previous.RollbackStatus,
		GroupVisibility: previous.GroupVisibility, GlobalModelPrice: previous.GlobalModelPrice,
		RequestedModels: sortedUnique(requested), EffectiveModels: sortedUnique(effective),
		ExcludedModels: sortedUnique(excluded), DiscoveryStatus: strings.TrimSpace(status),
	}
	global := plan.GroupModelPricing.GlobalModelPrice
	change.GroupModelPricing.GlobalModelPrice = domain.ListingGlobalModelPriceReceipt{
		ExpectedHash: global.ExpectedHash, DesiredHash: global.DesiredHash,
		Before: cloneRatios(global.Expected), After: cloneRatios(global.Desired), Added: cloneRatios(global.Added),
		Changed: global.Changed,
	}
}

func listingModelResolution(plan domain.ListingPlan, discovered []string, discoveryErr error) (
	effective, excluded []string, status string, err error,
) {
	requested := sortedUnique(plan.Channel.Models)
	manualExceptions := intersectModels(requested, plan.PricingRule.MirrorExcludedModels)
	candidates := subtractModels(requested, manualExceptions)
	if len(candidates) == 0 {
		return nil, requested, "confirmed", errors.New("所选模型全部位于例外清单，当前分组没有可上架模型")
	}
	if discoveryErr != nil {
		return candidates, manualExceptions, "frozen_catalog_fallback", nil
	}
	effective = intersectModels(candidates, sortedUnique(discovered))
	excluded = subtractModels(requested, effective)
	if len(effective) == 0 {
		return nil, excluded, "confirmed", errors.New("上游当前未返回任何所选模型，已停止上架且不会产生收费测试")
	}
	return effective, excluded, "confirmed", nil
}

func ensureListingChannelModels(ctx context.Context, adapter targets.Adapter, channelAdapter targets.ChannelAdapter,
	target domain.TargetSite, plan domain.ListingPlan, change *domain.ListingChangeSet) (domain.TargetChannel, error) {
	if change.CreatedChannelID == nil {
		return domain.TargetChannel{}, errors.New("listing model resolution has no disabled channel")
	}
	channel, err := channelAdapter.GetChannel(ctx, target, *change.CreatedChannelID)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	if err := verifyListingChannelIdentity(channel, plan, change.OwnershipMarker); err != nil {
		return domain.TargetChannel{}, err
	}
	if channel.Status != listingDisabledChannelStatus {
		return domain.TargetChannel{}, fmt.Errorf("resolved listing channel is in unsupported status %d", channel.Status)
	}
	desired := sortedUnique(plan.Channel.Models)
	if reflect.DeepEqual(sortedUnique(channel.Models), desired) {
		return channel, nil
	}
	allowedBefore := modelSet(change.GroupModelPricing.RequestedModels)
	for _, model := range channel.Models {
		if _, ok := allowedBefore[model]; !ok {
			return domain.TargetChannel{}, fmt.Errorf("disabled channel contains unexpected model %q", model)
		}
	}
	managed, err := targets.AsManagedChannelStateAdapter(adapter)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	state, err := managed.ReadManagedChannelState(ctx, target, channel.ID)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	if state.Status != listingDisabledChannelStatus || !reflect.DeepEqual(sortedUnique(state.Models), sortedUnique(channel.Models)) {
		return domain.TargetChannel{}, errors.New("disabled channel managed-state readback is inconsistent")
	}
	requestID := fmt.Sprintf("listing-%d-model-filter", change.ID)
	result, err := managed.ApplyManagedChannelState(ctx, target, channel.ID, state.Hash, desired, nil, requestID, func() error {
		guard, guardErr := channelAdapter.GetChannel(ctx, target, channel.ID)
		if guardErr != nil {
			return guardErr
		}
		if guardErr = verifyListingChannelIdentity(guard, plan, change.OwnershipMarker); guardErr != nil {
			return guardErr
		}
		if guard.Status != listingDisabledChannelStatus {
			return errors.New("disabled channel status changed before model filtering")
		}
		return nil
	})
	if err != nil {
		return domain.TargetChannel{}, fmt.Errorf("filter unavailable listing models: %w", err)
	}
	if result.ResultState != "verified_success" || !reflect.DeepEqual(sortedUnique(result.State.Models), desired) {
		return domain.TargetChannel{}, errors.New("filtered channel model readback did not match the effective model set")
	}
	channel, err = channelAdapter.GetChannel(ctx, target, channel.ID)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	if err := verifyListingChannel(channel, plan, change.OwnershipMarker); err != nil {
		return domain.TargetChannel{}, err
	}
	return channel, nil
}

func modelSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[strings.TrimSpace(value)] = struct{}{}
	}
	return result
}

func intersectModels(left, right []string) []string {
	rightSet := modelSet(right)
	result := make([]string, 0)
	for _, model := range sortedUnique(left) {
		if _, ok := rightSet[model]; ok {
			result = append(result, model)
		}
	}
	return result
}

func subtractModels(left, right []string) []string {
	rightSet := modelSet(right)
	result := make([]string, 0)
	for _, model := range sortedUnique(left) {
		if _, skip := rightSet[model]; !skip {
			result = append(result, model)
		}
	}
	return result
}
