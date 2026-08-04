package listing

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestGroupModelPricingPlanAIZZZMixedUnitsAndMargin(t *testing.T) {
	perSecond := []string{"1.8", "1.3", "0.3484", "0.2145", "0.1664", "0.3484", "0.2574", "0.4784"}
	fixed15 := []string{"7.6", "5.8", "4.9"}
	models := make([]domain.ModelOffer, 0, len(perSecond)+len(fixed15))
	modelIDs := make([]string, 0, cap(models))
	for index, value := range perSecond {
		name := fmt.Sprintf("jimeng-second-%d", index)
		modelIDs = append(modelIDs, name)
		models = append(models, domain.ModelOffer{ExternalID: name, Name: name, Group: "jimeng", BillingMode: "per_call",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: value}})
	}
	for index, value := range fixed15 {
		name := fmt.Sprintf("jimeng-fixed-%d", index)
		modelIDs = append(modelIDs, name)
		models = append(models, domain.ModelOffer{ExternalID: name, Name: name, Group: "jimeng", BillingMode: "per_call",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: value},
			Metadata:      map[string]string{"billing_unit": "fixed_duration", "fixed_duration_seconds": "15"}})
	}
	observedAt := time.Date(2026, 7, 26, 1, 2, 3, 0, time.UTC)
	expectedPrice := "9"
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID:    7,
		SourceCatalog: domain.CatalogSnapshot{Models: models, Evidence: domain.Evidence{ResponseHash: "aizzz-r1", ObservedAt: observedAt}},
		SourceGroup: domain.GroupOffer{ExternalID: "jimeng", Name: "jimeng", Ratio: "1", RawRatio: "1",
			EffectiveRatio: "1", RechargeValueFactor: "1", ModelIDs: modelIDs},
		Models: modelIDs, TargetGroup: "special-video", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before", Groups: map[string]map[string]domain.GroupModelPricingSetting{
			"other": {"jimeng-second-0": {BillingMode: "fixed_price", ModelPrice: &expectedPrice}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Models) != 11 || plan.MinimumMarginRate != "0.2" || plan.Mode != "atomic_contract" || !plan.Exact {
		t.Fatalf("plan summary = %#v", plan)
	}
	if plan.DesiredContract.Groups["other"]["jimeng-second-0"].ModelPrice == nil ||
		*plan.DesiredContract.Groups["other"]["jimeng-second-0"].ModelPrice != "9" {
		t.Fatal("planner changed an unrelated group/model entry")
	}
	second := plan.DesiredContract.Groups["special-video"]["jimeng-second-0"]
	if second.TaskProfile == nil || second.TaskProfile.Unit != "per_second" || second.ModelPrice == nil || *second.ModelPrice != "2.25" {
		t.Fatalf("per-second setting = %#v", second)
	}
	fixed := plan.DesiredContract.Groups["special-video"]["jimeng-fixed-0"]
	if fixed.TaskProfile == nil || fixed.TaskProfile.Unit != "fixed_duration" || fixed.TaskProfile.FixedDurationSeconds != 15 ||
		fixed.ModelPrice == nil || *fixed.ModelPrice != "9.5" {
		t.Fatalf("fixed-duration setting = %#v", fixed)
	}
	var fixedEvidence domain.ListingModelPricingEvidence
	for _, model := range plan.Models {
		if model.Model == "jimeng-fixed-0" {
			fixedEvidence = model
			break
		}
	}
	if len(fixedEvidence.Dimensions) != 1 {
		t.Fatalf("fixed-duration evidence missing: %#v", fixedEvidence)
	}
	if got := fixedEvidence.Dimensions[0]; got.SourceCashCost != "7.6" || got.EffectiveSale != "9.5" {
		t.Fatalf("fixed-duration evidence was multiplied by seconds: %#v", got)
	}
}

func TestGroupModelPricingPlanAppliesRechargeOnceAndDividesTargetGroupRatio(t *testing.T) {
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 9,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "video", Group: "ccmax",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
			Evidence: domain.Evidence{ResponseHash: "kuaipao-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "ccmax", Name: "ccmax", Ratio: "3", RawRatio: "3",
			EffectiveRatio: "1.2", RechargeValueFactor: "0.4", ModelIDs: []string{"video"}},
		Models: []string{"video"}, TargetGroup: "video-local", TargetGroupRatio: "2",
		PricingRule: domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2", ModelPriceRoundingStep: "0.0001"},
		Expected:    domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before", Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dimension := plan.Models[0].Dimensions[0]
	setting := plan.Models[0].Setting
	if dimension.SourceCashCost != "1.2" || dimension.DesiredEffectiveSale != "1.5" || dimension.TargetBasePrice != "0.75" ||
		dimension.EffectiveSale != "1.5" || dimension.MarginRate != "0.2" || setting.ModelPrice == nil || *setting.ModelPrice != "0.75" {
		t.Fatalf("recharge/group math = evidence=%#v setting=%#v", dimension, setting)
	}
}

func TestGroupModelPricingPlanSourceBaseAppliesRechargeOnceAndAddsGroupMargin(t *testing.T) {
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 9,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "video", Group: "ccmax",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
			Evidence: domain.Evidence{ResponseHash: "kuaipao-source-base-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "ccmax", Name: "ccmax", Ratio: "3", RawRatio: "3",
			EffectiveRatio: "1.2", RechargeValueFactor: "0.4", ModelIDs: []string{"video"}},
		Models: []string{"video"}, TargetGroup: "video-local", TargetGroupRatio: "0.9",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", RuleType: "add", Value: "0.1",
			GroupRatioRuleType: "add", GroupRatioRuleValue: "0.1", ModelPriceRuleType: "same",
			RoundingStep: "0.001", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before",
			Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.PriceAnchor != "source_base" || plan.TargetGroupRatio != "1.3" || plan.GroupRatioRuleType != "add" ||
		plan.ModelPriceRuleType != "same" {
		t.Fatalf("source-base plan summary = %#v", plan)
	}
	dimension := plan.Models[0].Dimensions[0]
	setting := plan.Models[0].Setting
	if dimension.SourceUnitPrice != "1" || dimension.SourceCashCost != "1.2" || dimension.TargetBasePrice != "1" ||
		dimension.EffectiveSale != "1.3" || dimension.MarginRate != "0.0769230769230769" ||
		setting.ModelPrice == nil || *setting.ModelPrice != "1" {
		t.Fatalf("source-base recharge/group math = evidence=%#v setting=%#v", dimension, setting)
	}
}

func TestGroupModelPricingPlanPreservesProvenanceForTimestampOnlyRefresh(t *testing.T) {
	firstObservedAt := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	input := GroupModelPricingPlanInput{
		SupplierID: 3,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "model-a", Group: "source",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1.25"}}},
			Evidence: domain.Evidence{ResponseHash: "source-r1", ObservedAt: firstObservedAt}},
		SourceGroup: domain.GroupOffer{ExternalID: "source", Name: "source", Ratio: "0.4", RawRatio: "0.4",
			EffectiveRatio: "0.4", RechargeValueFactor: "1", ModelIDs: []string{"model-a"}},
		Models: []string{"model-a"}, TargetGroup: "target", TargetGroupRatio: "0.4",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", RoundingStep: "0.001", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1,
			Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
		OwnershipSource: "relayops:policy/test",
	}
	first, err := PlanGroupModelPricing(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Expected = first.DesiredContract
	input.SourceCatalog.Evidence = domain.Evidence{ResponseHash: "source-r2", ObservedAt: firstObservedAt.Add(time.Hour)}
	second, err := PlanGroupModelPricing(input)
	if err != nil {
		t.Fatal(err)
	}
	if second.DesiredRevision != first.DesiredRevision {
		t.Fatalf("timestamp-only refresh changed revision: %s -> %s", first.DesiredRevision, second.DesiredRevision)
	}
	unchanged := second.DesiredContract.Groups["target"]["model-a"]
	if unchanged.SourceRevision != "source-r1" || unchanged.SourceObservedAtUnixMS != firstObservedAt.UnixMilli() {
		t.Fatalf("timestamp-only refresh changed provenance: %#v", unchanged)
	}

	input.SourceCatalog.Models[0].PriceFunction.PerRequest = "1.5"
	input.SourceCatalog.Evidence = domain.Evidence{ResponseHash: "source-r3", ObservedAt: firstObservedAt.Add(2 * time.Hour)}
	changed, err := PlanGroupModelPricing(input)
	if err != nil {
		t.Fatal(err)
	}
	updated := changed.DesiredContract.Groups["target"]["model-a"]
	if changed.DesiredRevision == first.DesiredRevision || updated.SourceRevision != "source-r3" ||
		updated.SourceObservedAtUnixMS != firstObservedAt.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("real price change did not refresh provenance: revision=%s setting=%#v", changed.DesiredRevision, updated)
	}
}

func TestGroupModelPricingPlanSourceBaseMirrorsMixedJimengBillingAndSolvesMargin(t *testing.T) {
	models := []domain.ModelOffer{
		{ExternalID: "jimeng-second", Group: "jimeng", PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "1.8"}},
		{ExternalID: "jimeng-fixed", Group: "jimeng", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "7.6"},
			Metadata: map[string]string{"billing_unit": "fixed_duration", "fixed_duration_seconds": "15"}},
	}
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 7, SourceCatalog: domain.CatalogSnapshot{Models: models,
			Evidence: domain.Evidence{ResponseHash: "jimeng-source-base-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "jimeng", Name: "jimeng", Ratio: "1", RawRatio: "1",
			EffectiveRatio: "1", RechargeValueFactor: "1", ModelIDs: []string{"jimeng-second", "jimeng-fixed"}},
		Models: []string{"jimeng-second", "jimeng-fixed"}, TargetGroup: "special-video", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", RuleType: "target_margin", Value: "0.2",
			GroupRatioRuleType: "target_margin", GroupRatioRuleValue: "0.2", ModelPriceRuleType: "same",
			RoundingStep: "0.001", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before",
			Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetGroupRatio != "1.25" || plan.MinimumMarginRate != "0.2" || !plan.MinimumMarginDefined {
		t.Fatalf("mixed source-base summary = %#v", plan)
	}
	second := plan.DesiredContract.Groups["special-video"]["jimeng-second"]
	fixed := plan.DesiredContract.Groups["special-video"]["jimeng-fixed"]
	if second.ModelPrice == nil || *second.ModelPrice != "1.8" || second.TaskProfile == nil || second.TaskProfile.Unit != "per_second" {
		t.Fatalf("per-second source-base setting = %#v", second)
	}
	if fixed.ModelPrice == nil || *fixed.ModelPrice != "7.6" || fixed.TaskProfile == nil ||
		fixed.TaskProfile.Unit != "fixed_duration" || fixed.TaskProfile.FixedDurationSeconds != 15 {
		t.Fatalf("fixed-duration source-base setting = %#v", fixed)
	}
}

func TestGroupModelPricingPlanBuildsTokenVector(t *testing.T) {
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 11,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "gpt", Group: "cheap", BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "8", CacheReadPer1M: "1"}}},
			Evidence: domain.Evidence{ResponseHash: "token-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "cheap", Name: "cheap", Ratio: "0.5", EffectiveRatio: "0.5",
			RechargeValueFactor: "1", ModelIDs: []string{"gpt"}},
		Models: []string{"gpt"}, TargetGroup: "gpt-local", TargetGroupRatio: "0.25",
		PricingRule: domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2", ModelPriceRoundingStep: "0.000000000001"},
		Expected:    domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before", Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setting := plan.Models[0].Setting
	if setting.BillingMode != "ratio" || setting.ModelRatio == nil || *setting.ModelRatio != "2.5" ||
		setting.CompletionRatio == nil || *setting.CompletionRatio != "4" || setting.CacheRatio == nil || *setting.CacheRatio != "0.5" {
		t.Fatalf("token setting = %#v", setting)
	}
	for _, dimension := range plan.Models[0].Dimensions {
		margin, err := decimal.NewFromString(dimension.MarginRate)
		if err != nil || margin.LessThan(decimal.RequireFromString("0.2")) {
			t.Fatalf("dimension under margin floor: %#v", dimension)
		}
	}
}

func TestGroupModelPricingPlanAllowsExplicitZeroTokenVector(t *testing.T) {
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 52,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "grok-imagine-image", Group: "grok",
			BillingMode: "ratio", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "0", OutputPer1M: "0"}}},
			Evidence: domain.Evidence{ResponseHash: "grok-zero-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "grok", Name: "grok", Ratio: "1", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"grok-imagine-image"}},
		Models: []string{"grok-imagine-image"}, TargetGroup: "grok-local", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setting := plan.DesiredContract.Groups["grok-local"]["grok-imagine-image"]
	if setting.BillingMode != "ratio" || setting.ModelRatio == nil || *setting.ModelRatio != "0" ||
		setting.CompletionRatio == nil || *setting.CompletionRatio != "1" {
		t.Fatalf("explicit zero token vector was not preserved: %#v", setting)
	}
}

func TestGroupModelPricingPlanRejectsNonZeroTokenDimensionWithZeroInput(t *testing.T) {
	_, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 52,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "grok-imagine-image", Group: "grok",
			BillingMode: "ratio", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "0", OutputPer1M: "1"}}},
			Evidence: domain.Evidence{ResponseHash: "grok-invalid-r1", ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "grok", Name: "grok", Ratio: "1", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"grok-imagine-image"}},
		Models: []string{"grok-imagine-image"}, TargetGroup: "grok-local", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be represented") {
		t.Fatalf("zero input/non-zero output was accepted: %v", err)
	}
}

func TestGroupModelPricingPlanRejectsUnrepresentableUnitsAndAmbiguousScope(t *testing.T) {
	base := GroupModelPricingPlanInput{
		SupplierID: 1,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", Group: "g",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerImage: "1"}}}, Evidence: domain.Evidence{ObservedAt: time.Now().UTC()}},
		SourceGroup: domain.GroupOffer{ExternalID: "g", Name: "g", Ratio: "1", ModelIDs: []string{"m"}},
		Models:      []string{"m"}, TargetGroup: "local", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2"},
		Expected:    domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	}
	if _, err := PlanGroupModelPricing(base); err == nil {
		t.Fatal("per-image price was accepted without an exact target unit")
	}
	base.SourceCatalog.Models = []domain.ModelOffer{
		{ExternalID: "m", Group: "g", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}},
		{ExternalID: "m", Group: "g", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}},
	}
	if _, err := PlanGroupModelPricing(base); err == nil {
		t.Fatal("conflicting same-group model records were accepted")
	}
}

func TestGroupModelPricingPlanExcludesUnsupportedDimensionAndKeepsCompatibleModels(t *testing.T) {
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 1,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{
			{ExternalID: "chat-ok", Group: "mixed", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}},
			{ExternalID: "gpt-5.4", Group: "mixed", PriceFunction: domain.PriceFunction{Currency: "USD",
				Dimensions: map[string]string{"custom:image_output_per_1m": "6"}}},
		}, Evidence: domain.Evidence{ObservedAt: time.Now().UTC(), ResponseHash: "mixed-r1"}},
		SourceGroup: domain.GroupOffer{ExternalID: "mixed", Name: "mixed", Ratio: "1", RawRatio: "1",
			RechargeValueFactor: "1", ModelIDs: []string{"chat-ok", "gpt-5.4"}},
		Models: []string{"chat-ok", "gpt-5.4"}, TargetGroup: "local", TargetGroupRatio: "1",
		PricingRule: domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2"},
		Expected:    domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Models) != 1 || plan.Models[0].Model != "chat-ok" || strings.Join(plan.ExcludedModels, ",") != "gpt-5.4" ||
		!strings.Contains(plan.ExclusionReasons["gpt-5.4"], "custom:image_output_per_1m") {
		t.Fatalf("partial compatibility plan=%#v", plan)
	}
	if _, exists := plan.DesiredContract.Groups["local"]["gpt-5.4"]; exists {
		t.Fatalf("unsupported model reached desired contract: %#v", plan.DesiredContract.Groups["local"])
	}
}

func TestGroupModelPricingPlanFillsUnresolvedSourceBaseFromExistingTargetPrice(t *testing.T) {
	existing := "3"
	plan, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 1,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", Group: "source",
			PriceFunction: domain.PriceFunction{Currency: "USD", GroupMultiplier: "0.4", RequiresModelBasePrice: true}}}},
		TargetCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{
			{ExternalID: "m", Group: "target", BillingMode: "fixed_price", ModelPrice: "3",
				PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "3"}},
			{ExternalID: "m", Group: "other", BillingMode: "fixed_price", ModelPrice: "9",
				PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "9"}},
		}},
		SourceGroup: domain.GroupOffer{ExternalID: "source", Name: "source", Ratio: "0.4", RawRatio: "0.4",
			RechargeValueFactor: "1", ModelIDs: []string{"m"}},
		Models: []string{"m"}, TargetGroup: "target", TargetGroupRatio: "0.4",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", RoundingStep: "0.001", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before",
			Groups: map[string]map[string]domain.GroupModelPricingSetting{"target": {
				"m": {BillingMode: "fixed_price", ModelPrice: &existing,
					TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"}},
			}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setting := plan.DesiredContract.Groups["target"]["m"]
	if setting.ModelPrice == nil || *setting.ModelPrice != "3" || setting.TaskProfile == nil ||
		setting.TaskProfile.Unit != "per_request" {
		t.Fatalf("fallback changed the existing target price: %#v", setting)
	}
	if plan.Models[0].Dimensions[0].SourceUnitPrice != "3" {
		t.Fatalf("fallback evidence = %#v", plan.Models[0])
	}
}

func TestGroupModelPricingPlanRejectsConflictingTargetFallbackPrices(t *testing.T) {
	_, err := PlanGroupModelPricing(GroupModelPricingPlanInput{
		SupplierID: 1,
		SourceCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", Group: "source",
			PriceFunction: domain.PriceFunction{Currency: "USD", GroupMultiplier: "0.4", RequiresModelBasePrice: true}}}},
		TargetCatalog: domain.CatalogSnapshot{Models: []domain.ModelOffer{
			{ExternalID: "m", Group: "a", BillingMode: "fixed_price", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "3"}},
			{ExternalID: "m", Group: "b", BillingMode: "fixed_price", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "4"}},
		}},
		SourceGroup: domain.GroupOffer{ExternalID: "source", Name: "source", Ratio: "0.4", RawRatio: "0.4",
			RechargeValueFactor: "1", ModelIDs: []string{"m"}},
		Models: []string{"m"}, TargetGroup: "new-target", TargetGroupRatio: "0.4",
		PricingRule: domain.ListingPricingRule{PriceAnchor: "source_base", GroupRatioRuleType: "same",
			ModelPriceRuleType: "same", RoundingStep: "0.001", ModelPriceRoundingStep: "0.0001"},
		Expected: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "before",
			Groups: map[string]map[string]domain.GroupModelPricingSetting{}},
	})
	if err == nil || !strings.Contains(err.Error(), "multiple conflicting target base prices") {
		t.Fatalf("conflicting fallback error = %v", err)
	}
}
