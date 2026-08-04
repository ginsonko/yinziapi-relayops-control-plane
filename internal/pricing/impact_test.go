package pricing

import (
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestResolvePricingImpactUsesWorstModelDimension(t *testing.T) {
	group := domain.GroupOffer{Name: "promo", Ratio: "0.1", ModelIDs: []string{"text", "image"}}
	source := domain.CatalogSnapshot{Models: []domain.ModelOffer{
		{ExternalID: "text", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "8"}},
		{ExternalID: "image", PriceFunction: domain.PriceFunction{Currency: "USD", PerImage: "0.4"}},
	}}
	target := domain.CatalogSnapshot{Models: []domain.ModelOffer{
		{ExternalID: "text", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "4"}},
		{ExternalID: "image", PriceFunction: domain.PriceFunction{Currency: "USD", PerImage: "0.2"}},
	}}
	impact, err := ResolvePricingImpact(source, target, group, []string{"image", "text"})
	if err != nil {
		t.Fatal(err)
	}
	if impact.EquivalentCostRatio != "0.2" || impact.CostRatioVector["text/input_per_1m"] != "0.1" ||
		impact.CostRatioVector["text/output_per_1m"] != "0.2" || impact.CostRatioVector["image/per_image"] != "0.2" {
		t.Fatalf("impact = %#v", impact)
	}
}

func TestResolvePricingImpactRejectsUnknownOrMismatchedEvidence(t *testing.T) {
	group := domain.GroupOffer{Name: "promo", Ratio: "0.1", ModelIDs: []string{"m"}}
	source := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "1"}}}}
	target := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{RequiresModelBasePrice: true}}}}
	if _, err := ResolvePricingImpact(source, target, group, []string{"m"}); err == nil || !strings.Contains(err.Error(), "currencies") {
		t.Fatalf("unknown target evidence error = %v", err)
	}
	target.Models[0].PriceFunction = domain.PriceFunction{Currency: "CNY", InputPer1M: "1"}
	if _, err := ResolvePricingImpact(source, target, group, []string{"m"}); err == nil || !strings.Contains(err.Error(), "currency conversion") {
		t.Fatalf("currency mismatch error = %v", err)
	}
	target.Models[0].PriceFunction = domain.PriceFunction{Currency: "USD", InputPer1M: "1"}
	if _, err := ResolvePricingImpact(source, target, group, []string{"other"}); err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("group membership error = %v", err)
	}
}

func TestResolvePricingImpactKeepsPartialPreviewForUnknownDimension(t *testing.T) {
	group := domain.GroupOffer{Name: "promo", Ratio: "0.1", ModelIDs: []string{"m"}}
	source := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{
		Currency: "USD", InputPer1M: "2", OutputPer1M: "8", CacheWritePer1M: "4",
	}}}}
	target := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{
		Currency: "USD", InputPer1M: "2", OutputPer1M: "4",
	}}}}
	impact, err := ResolvePricingImpact(source, target, group, []string{"m"})
	if err != nil {
		t.Fatal(err)
	}
	if impact.Complete || impact.EquivalentCostRatio != "0.2" || impact.CoverageRatio != "0.6666666666666667" {
		t.Fatalf("partial impact = %#v", impact)
	}
	if len(impact.UnknownDimensions) != 1 || impact.UnknownDimensions[0] != "m/cache_write_per_1m" {
		t.Fatalf("unknown dimensions = %#v", impact.UnknownDimensions)
	}
}

func TestModelPriceHashIgnoresGroupRatioButDetectsModelPriceChange(t *testing.T) {
	left := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{Name: "g", Ratio: "0.1"}}, Models: []domain.ModelOffer{{
		ExternalID: "m", BillingMode: "ratio", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2"},
	}}}
	right := left
	right.Groups = []domain.GroupOffer{{Name: "g", Ratio: "9"}}
	if ModelPriceHash(left) != ModelPriceHash(right) {
		t.Fatal("group multiplier changed the model price hash")
	}
	right.Models = append([]domain.ModelOffer(nil), left.Models...)
	right.Models[0].PriceFunction.InputPer1M = "3"
	if ModelPriceHash(left) == ModelPriceHash(right) {
		t.Fatal("model price change was not detected")
	}
}

func TestModelPriceHashIsDeterministicForSameModelAcrossGroups(t *testing.T) {
	cheap := domain.ModelOffer{ExternalID: "same-model", Name: "same-model", Group: "cheap", BillingMode: "fixed_price",
		PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "0.02"}}
	premium := domain.ModelOffer{ExternalID: "same-model", Name: "same-model", Group: "premium", BillingMode: "fixed_price",
		PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "0.04"}}
	forward := domain.CatalogSnapshot{Models: []domain.ModelOffer{cheap, premium}}
	reversed := domain.CatalogSnapshot{Models: []domain.ModelOffer{premium, cheap}}
	if ModelPriceHash(forward) != ModelPriceHash(reversed) {
		t.Fatal("same group-scoped catalog changed hash when response order changed")
	}

	changed := domain.CatalogSnapshot{Models: []domain.ModelOffer{cheap, premium}}
	changed.Models[1].PriceFunction.PerSecond = "0.05"
	if ModelPriceHash(forward) == ModelPriceHash(changed) {
		t.Fatal("group-scoped price change was not detected")
	}

	moved := domain.CatalogSnapshot{Models: []domain.ModelOffer{cheap, premium}}
	moved.Models[1].Group = "other"
	if ModelPriceHash(forward) == ModelPriceHash(moved) {
		t.Fatal("group-scoped model identity change was not detected")
	}
}

func TestResolvePricingImpactUsesOnlyBoundTargetGroup(t *testing.T) {
	group := domain.GroupOffer{Name: "upstream", Ratio: "0.3", ModelIDs: []string{"shared"}}
	source := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "shared",
		PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}}}}
	target := domain.CatalogSnapshot{Models: []domain.ModelOffer{
		{ExternalID: "shared", Group: "cheap", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}},
		{ExternalID: "shared", Group: "premium", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "4"}},
	}}
	impact, err := ResolvePricingImpactForTargetGroup(source, target, group, "cheap", []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if impact.EquivalentCostRatio != "0.3" {
		t.Fatalf("cheap group cost ratio = %s, want 0.3", impact.EquivalentCostRatio)
	}
	impact, err = ResolvePricingImpactForTargetGroup(source, target, group, "premium", []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if impact.EquivalentCostRatio != "0.15" {
		t.Fatalf("premium group cost ratio = %s, want 0.15", impact.EquivalentCostRatio)
	}
}

func TestResolvePricingImpactFallsBackToLegacyUngroupedTarget(t *testing.T) {
	group := domain.GroupOffer{Name: "upstream", Ratio: "0.3", ModelIDs: []string{"shared"}}
	source := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "shared",
		PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}}}}
	target := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "shared",
		PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}}}}
	impact, err := ResolvePricingImpactForTargetGroup(source, target, group, "new-group", []string{"shared"})
	if err != nil || impact.EquivalentCostRatio != "0.3" {
		t.Fatalf("legacy target impact = %#v, %v", impact, err)
	}
}
