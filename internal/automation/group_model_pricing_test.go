package automation

import (
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestContractChangeSafetyAllowsConfirmedIncreaseAndGatesAnomalousDecrease(t *testing.T) {
	currentPrice := "1"
	base := domain.ListingGroupModelPricingPlan{TargetGroup: "economy", TargetGroupRatio: "1",
		ExpectedContract: domain.GroupModelPricingContract{Groups: map[string]map[string]domain.GroupModelPricingSetting{
			"economy": {"video": {BillingMode: "fixed_price", ModelPrice: &currentPrice,
				TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"}}},
		}}, Models: []domain.ListingModelPricingEvidence{{Model: "video", TargetGroupRatio: "1",
			Setting: domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: stringPointer("2"),
				TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"}},
			Dimensions: []domain.ListingPriceDimensionEvidence{{Dimension: "per_request", EffectiveSale: "2"}},
		}}}
	config := DefaultSafetyConfig()
	if eligible, warnings := contractChangeSafety(base, config); !eligible || len(warnings) != 0 {
		t.Fatalf("confirmed supplier increase was blocked: eligible=%t warnings=%v", eligible, warnings)
	}
	base.Models[0].Setting.ModelPrice = stringPointer("0.1")
	base.Models[0].Dimensions[0].EffectiveSale = "0.1"
	eligible, warnings := contractChangeSafety(base, config)
	if eligible || len(warnings) == 0 || !strings.Contains(warnings[0], "above the unattended decrease gate") {
		t.Fatalf("anomalous decrease was not gated: eligible=%t warnings=%v", eligible, warnings)
	}
}

func TestSameFunctionalContractOwnershipIgnoresOnlyProvenance(t *testing.T) {
	currentRatio := "5.472727345454546"
	desiredRatio := "5.472727345454546"
	current := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special": {"kimi-k3": {BillingMode: "ratio", CompletionRatio: &currentRatio,
			Source: "relayops:supplier/3/group/domestic", SourceRevision: "source-r1", SourceObservedAtUnixMS: 100}},
	}}
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special": {"kimi-k3": {BillingMode: "ratio", CompletionRatio: &desiredRatio,
			Source: "relayops:supplier/3/group/domestic", SourceRevision: "source-r2", SourceObservedAtUnixMS: 200}},
	}}
	if !sameFunctionalContractOwnership(current, desired) {
		t.Fatal("provenance-only target-normalized change was treated as functional")
	}

	changedRatio := "5.5"
	desired.Groups["special"]["kimi-k3"] = domain.GroupModelPricingSetting{BillingMode: "ratio", CompletionRatio: &changedRatio,
		Source: "relayops:supplier/3/group/domestic", SourceRevision: "source-r2", SourceObservedAtUnixMS: 200}
	if sameFunctionalContractOwnership(current, desired) {
		t.Fatal("real price change was collapsed")
	}

	desiredRatio = currentRatio
	desired.Groups["special"]["kimi-k3"] = domain.GroupModelPricingSetting{BillingMode: "ratio", CompletionRatio: &desiredRatio,
		Source: "relayops:policy/other", SourceRevision: "source-r2", SourceObservedAtUnixMS: 200}
	if sameFunctionalContractOwnership(current, desired) {
		t.Fatal("ownership change was collapsed")
	}
}

func stringPointer(value string) *string { return &value }
