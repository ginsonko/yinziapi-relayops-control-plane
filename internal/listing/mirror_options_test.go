package listing

import (
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestListingMirrorOptionsRequireExplicitClosedConfiguration(t *testing.T) {
	if err := validateListingMirrorOptions(domain.ListingPricingRule{}); err != nil {
		t.Fatalf("legacy omitted mirror options: %v", err)
	}

	yes, no := true, false
	complete := domain.ListingPricingRule{
		SyncGroupRatio: &yes, SyncModelPrices: &yes, SyncBillingMode: &yes,
		SyncModelAdditions: &yes, SyncModelRemovals: &yes,
		ModelScopeMode: "mirror_group", EmptyGroupAction: "disable_managed_channel",
	}
	if err := validateListingMirrorOptions(complete); err != nil {
		t.Fatalf("complete mirror options: %v", err)
	}

	missing := complete
	missing.SyncModelRemovals = nil
	if err := validateListingMirrorOptions(missing); err == nil || !strings.Contains(err.Error(), "sync_model_removals") {
		t.Fatalf("missing explicit removal switch error = %v", err)
	}

	unsafeAddition := complete
	unsafeAddition.SyncModelPrices = &no
	if err := validateListingMirrorOptions(unsafeAddition); err == nil || !strings.Contains(err.Error(), "billing-mode") {
		t.Fatalf("unsafe addition error = %v", err)
	}

	unsafeEmptyDisable := complete
	unsafeEmptyDisable.SyncModelRemovals = &no
	if err := validateListingMirrorOptions(unsafeEmptyDisable); err == nil || !strings.Contains(err.Error(), "managed-channel disable") {
		t.Fatalf("unsafe empty-group action error = %v", err)
	}
}

func TestListingMirrorIdentityIsStableAndOwnsGeneratedContractEntries(t *testing.T) {
	yes := true
	request := domain.ListingPlanRequest{SupplierID: 9, DiscoveryExternalID: "jimeng", TargetSiteID: 4,
		TargetGroup: "special-video", IdempotencyKey: "preview-identity-1",
		PricingRule: domain.ListingPricingRule{SyncGroupRatio: &yes, SyncModelPrices: &yes,
			SyncBillingMode: &yes, SyncModelAdditions: &yes, SyncModelRemovals: &yes,
			ModelScopeMode: "mirror_group", EmptyGroupAction: "disable_managed_channel"}}
	first := listingMirrorPolicyUUID(request)
	second := listingMirrorPolicyUUID(request)
	if first == "" || first != second {
		t.Fatalf("mirror policy identity is not stable: %q %q", first, second)
	}
	request.TargetGroup = "another-group"
	if other := listingMirrorPolicyUUID(request); other == first {
		t.Fatalf("different target binding reused policy UUID %q", first)
	}
	request.TargetGroup = "special-video"
	request.PricingRule.MirrorPolicyUUID = first
	marker := listingMirrorOwnershipMarker(request.PricingRule)
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 9,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "jimeng-video", Group: "jimeng",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "0.12"}}},
			Evidence: domain.Evidence{ResponseHash: "source-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "jimeng", Name: "jimeng", RawRatio: "1", Ratio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"jimeng-video"}},
		Models: []string{"jimeng-video"}, TargetGroup: "special-video", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", RuleType: "multiply", Value: "1"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1,
			Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
		OwnershipSource: marker,
	})
	if err != nil {
		t.Fatalf("plan owned contract: %v", err)
	}
	if got := plan.DesiredContract.Groups["special-video"]["jimeng-video"].Source; got != marker {
		t.Fatalf("contract ownership source = %q, want %q", got, marker)
	}

	listingPlan := domain.ListingPlan{SupplierID: request.SupplierID, SourceGroup: "jimeng", TargetSiteID: request.TargetSiteID,
		TargetGroup: request.TargetGroup, PricingRule: request.PricingRule}
	listingPlan.PricingRule.PriceAnchor = "source_base"
	listingPlan.PricingRule.GroupRatioRuleType = "same"
	listingPlan.PricingRule.GroupRatioRuleValue = ""
	listingPlan.PricingRule.ModelPriceRuleType = "same"
	listingPlan.PricingRule.ModelPriceRuleValue = ""
	listingPlan.PricingRule.ExecutionMode = "auto"
	policy, configured, err := nativeMirrorPolicyForPlan(listingPlan)
	if err != nil || !configured {
		t.Fatalf("native mirror policy configured=%t error=%v", configured, err)
	}
	if policy.PolicyUUID != first || policy.LegacyPolicyID != 0 || policy.Enabled || !policy.SyncModelRemovals ||
		policy.SourceModelPattern != "*" || policy.ExecutionMode != "auto" {
		t.Fatalf("native mirror policy = %#v", policy)
	}
}
