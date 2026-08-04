package listing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

var listingMirrorPolicyNamespace = uuid.MustParse("b229330b-3045-44cb-96be-a93f31fc4675")

func listingUsesCompleteMirror(rule domain.ListingPricingRule) bool {
	if rule.CreateGroupMirror != nil {
		return *rule.CreateGroupMirror
	}
	return rule.SyncGroupRatio != nil || rule.SyncModelPrices != nil || rule.SyncBillingMode != nil ||
		rule.SyncModelAdditions != nil || rule.SyncModelRemovals != nil || strings.TrimSpace(rule.ModelScopeMode) != "" ||
		strings.TrimSpace(rule.EmptyGroupAction) != ""
}

func listingUsesPricingMapping(rule domain.ListingPricingRule) bool {
	return rule.CreatePricingMapping == nil || *rule.CreatePricingMapping
}

func listingMirrorPolicyUUID(request domain.ListingPlanRequest) string {
	if !listingUsesCompleteMirror(request.PricingRule) {
		return ""
	}
	identity := fmt.Sprintf("v1\x00%s\x00%d\x00%s\x00%d\x00%s", request.IdempotencyKey,
		request.SupplierID, request.DiscoveryExternalID, request.TargetSiteID, request.TargetGroup)
	return uuid.NewSHA1(listingMirrorPolicyNamespace, []byte(identity)).String()
}

func listingMirrorOwnershipMarker(rule domain.ListingPricingRule) string {
	policyUUID := strings.TrimSpace(rule.MirrorPolicyUUID)
	if policyUUID == "" {
		return ""
	}
	if _, err := uuid.Parse(policyUUID); err != nil {
		return ""
	}
	return "relayops:policy/" + policyUUID
}

func nativeMirrorPolicyForPlan(plan domain.ListingPlan) (mirror.Policy, bool, error) {
	rule := plan.PricingRule
	if !listingUsesCompleteMirror(rule) {
		return mirror.Policy{}, false, nil
	}
	if err := validateListingMirrorOptions(rule); err != nil {
		return mirror.Policy{}, true, err
	}
	if strings.TrimSpace(rule.MirrorPolicyUUID) == "" {
		return mirror.Policy{}, true, errors.New("complete mirror plan has no server-owned policy UUID")
	}
	modelRule, err := listingModelPriceRule(rule, rule.PriceAnchor)
	if err != nil {
		return mirror.Policy{}, true, err
	}
	policy := mirror.Policy{
		PolicyUUID: rule.MirrorPolicyUUID, PolicyVersion: 1, SchemaVersion: mirror.PolicySchemaVersion,
		SupplierID: plan.SupplierID, SourceGroup: plan.SourceGroup,
		SourceModelPattern: "*", ExcludedModels: append([]string(nil), rule.MirrorExcludedModels...),
		TargetSiteID: plan.TargetSiteID, TargetGroup: plan.TargetGroup,
		Enabled: false, Kind: mirror.PolicyKindCompleteMirror, PriceAnchor: rule.PriceAnchor,
		SyncGroupRatio: false, GroupRatioRule: mirror.Rule{Type: "same"},
		SyncModelPrices: *rule.SyncModelPrices, ModelPriceRule: modelRule,
		SyncBillingMode: *rule.SyncBillingMode, SyncModelAdditions: *rule.SyncModelAdditions,
		SyncModelRemovals: *rule.SyncModelRemovals, ModelScopeMode: rule.ModelScopeMode,
		EmptyGroupAction: rule.EmptyGroupAction, ExecutionMode: defaultString(rule.MirrorExecutionMode, defaultString(rule.ExecutionMode, "approval")),
		MinimumRatio: defaultString(rule.MinimumRatio, "0"), MaximumRatio: defaultString(rule.MaximumRatio, "0"),
		RoundingStep: defaultString(rule.RoundingStep, "0.001"), OnlyFollowIncrease: rule.OnlyFollowIncrease,
		OnlyFollowDecrease: rule.OnlyFollowDecrease,
	}
	capabilities := mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}
	if err := mirror.ValidatePolicy(policy, capabilities); err != nil {
		return mirror.Policy{}, true, err
	}
	return mirror.NormalizeOwnership(policy), true, nil
}
