package connectors

import (
	"encoding/json"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNewAPIPriceFunctionNormalizesTokenDimensions(t *testing.T) {
	modelRatio := json.Number("2.5")
	completionRatio := json.Number("4")
	cacheRatio := json.Number("0.1")
	cacheWriteRatio := json.Number("1.25")
	imageRatio := json.Number("0.5")
	audioRatio := json.Number("0.25")
	audioCompletionRatio := json.Number("2")

	price := newAPIPriceFunction(newAPIPrice{
		ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheRatio: &cacheRatio,
		CreateCacheRatio: &cacheWriteRatio, ImageRatio: &imageRatio, AudioRatio: &audioRatio,
		AudioCompletionRatio: &audioCompletionRatio,
	})

	if price.InputPer1M != "5" || price.OutputPer1M != "20" || price.CacheReadPer1M != "0.5" || price.CacheWritePer1M != "6.25" {
		t.Fatalf("token price vector = %#v", price)
	}
	if price.ImageInputPer1M != "2.5" || price.AudioInputPer1M != "1.25" || price.AudioOutputPer1M != "10" {
		t.Fatalf("multimodal price vector = %#v", price)
	}
}

func TestNewAPIPriceFunctionPreservesExplicitFreeAndPerRequest(t *testing.T) {
	free := json.Number("0")
	freePrice := newAPIPriceFunction(newAPIPrice{ModelRatio: &free})
	if freePrice.InputPer1M != "0" || freePrice.RequiresModelBasePrice {
		t.Fatalf("explicit free price = %#v", freePrice)
	}

	perRequest := json.Number("0.004")
	requestPrice := newAPIPriceFunction(newAPIPrice{QuotaType: 1, ModelPrice: &perRequest})
	if requestPrice.PerRequest != "0.004" || requestPrice.InputPer1M != "" {
		t.Fatalf("per-request price = %#v", requestPrice)
	}
}

func TestNewAPILegacyDescriptionsRefineDurationBillingWithoutModelNameHeuristics(t *testing.T) {
	var upstream newAPIPricingResponse
	err := json.Unmarshal([]byte(`{
		"success":true,"group_ratio":{"即梦":1},"data":[
			{"model_name":"variable-video","description":"自由时长，按秒计费","quota_type":1,"model_price":1.794,"enable_groups":["即梦"]},
			{"model_name":"fixed-video","description":"高并发 固定时长15s固定价格","quota_type":1,"model_price":5.824,"enable_groups":["即梦"]},
			{"model_name":"ordinary-request","description":"一次请求一个结果","quota_type":1,"model_price":0.3,"enable_groups":["即梦"]},
			{"model_name":"conflicting-description","description":"固定时长15s，同时按秒计费","quota_type":1,"model_price":9,"enable_groups":["即梦"]}
		]}`), &upstream)
	if err != nil {
		t.Fatal(err)
	}
	catalog := parseNewAPICatalog(upstream)
	byName := make(map[string]domain.ModelOffer)
	for _, offer := range catalog.Models {
		byName[offer.Name] = offer
	}
	variable := byName["variable-video"]
	if variable.PriceFunction.PerSecond != "1.794" || variable.PriceFunction.PerRequest != "" || variable.Metadata["billing_unit"] != "per_second" {
		t.Fatalf("variable-duration offer = %#v", variable)
	}
	fixed := byName["fixed-video"]
	if fixed.PriceFunction.FixedFee != "5.824" || fixed.PriceFunction.PerRequest != "" ||
		fixed.Metadata["billing_unit"] != "fixed_duration" || fixed.Metadata["fixed_duration_seconds"] != "15" {
		t.Fatalf("fixed-duration offer = %#v", fixed)
	}
	ordinary := byName["ordinary-request"]
	if ordinary.PriceFunction.PerRequest != "0.3" || ordinary.Metadata["billing_unit"] != "per_request" {
		t.Fatalf("ordinary per-request offer = %#v", ordinary)
	}
	conflict := byName["conflicting-description"]
	if !conflict.PriceFunction.RequiresModelBasePrice || conflict.Metadata["pricing_ambiguity"] == "" {
		t.Fatalf("conflicting description must remain ambiguous = %#v", conflict)
	}
}

func TestNewAPIExplicitGroupPricingOverridesLegacyDescription(t *testing.T) {
	var upstream newAPIPricingResponse
	err := json.Unmarshal([]byte(`{
		"success":true,"group_model_pricing_revision":"revision-structured","group_ratio":{"g":1},
		"data":[{"model_name":"structured-video","description":"固定时长15s固定价格","quota_type":1,"model_price":9,
			"group_pricing":{"g":{"group":"g","scope":"group_model","billing_mode":"fixed_price","billing_unit":"per_second","quota_type":1,"model_price":0.25,"base_group_ratio":1}}}]}`), &upstream)
	if err != nil {
		t.Fatal(err)
	}
	catalog := parseNewAPICatalog(upstream)
	if len(catalog.Models) != 1 || catalog.Models[0].PriceFunction.PerSecond != "0.25" || catalog.Models[0].PriceFunction.FixedFee != "" {
		t.Fatalf("structured pricing must override description = %#v", catalog.Models)
	}
}

func TestNewAPIPriceFunctionHonorsTieredExpressionDimensions(t *testing.T) {
	modelRatio := json.Number("1.25")
	completionRatio := json.Number("6")
	cacheRatio := json.Number("0.1")
	cacheWriteRatio := json.Number("1.2")

	withoutCacheWrite := newAPIPriceFunction(newAPIPrice{
		ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheRatio: &cacheRatio,
		CreateCacheRatio: &cacheWriteRatio, BillingMode: "tiered_expr",
		BillingExpr: `len < 272000 ? tier("standard", p * 2.5 + c * 15 + cr * 0.25) : tier("cc is only text", p * 5 + c * 22.5 + cr * 0.5)`,
	})
	if withoutCacheWrite.InputPer1M == "" || withoutCacheWrite.OutputPer1M == "" || withoutCacheWrite.CacheReadPer1M == "" {
		t.Fatalf("referenced dimensions were removed: %#v", withoutCacheWrite)
	}
	if withoutCacheWrite.CacheWritePer1M != "" {
		t.Fatalf("unreferenced cache write was treated as billable: %#v", withoutCacheWrite)
	}

	withCacheWrite := newAPIPriceFunction(newAPIPrice{
		ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheRatio: &cacheRatio,
		CreateCacheRatio: &cacheWriteRatio, BillingMode: "tiered_expr",
		BillingExpr: `tier("base", p * 2.5 + c * 15 + cr * 0.25 + cc * 3)`,
	})
	if withCacheWrite.CacheWritePer1M != "3" {
		t.Fatalf("referenced cache write was not retained: %#v", withCacheWrite)
	}
}

func TestBillingExpressionVariablesFallsBackForInvalidExpression(t *testing.T) {
	if variables, scoped := billingExpressionVariables("tiered_expr", `tier("unterminated, p * 2)`); scoped || variables != nil {
		t.Fatalf("invalid expression must preserve conservative legacy dimensions: %#v %v", variables, scoped)
	}
}

func TestNewAPIGroupPricingPreservesSameModelPerGroupUnitsAndVectors(t *testing.T) {
	var upstream newAPIPricingResponse
	err := json.Unmarshal([]byte(`{
		"success":true,"group_model_pricing_revision":"revision-1","group_ratio":{"cheap":0.4,"premium":1.2},
		"data":[{"model_name":"same-model","quota_type":1,"model_price":9,"enable_groups":["cheap","premium"],
			"group_pricing":{
				"cheap":{"group":"cheap","scope":"group_model","billing_mode":"fixed_price","billing_unit":"per_second","quota_type":1,"model_price":0.05,"base_group_ratio":0.4},
				"premium":{"group":"premium","scope":"group_model","billing_mode":"fixed_price","billing_unit":"fixed_duration","quota_type":1,"model_price":2,"fixed_duration_seconds":8,"base_group_ratio":1.2}
			}}]}`), &upstream)
	if err != nil {
		t.Fatal(err)
	}
	catalog := parseNewAPICatalog(upstream)
	if len(catalog.Models) != 2 {
		t.Fatalf("group-scoped models = %#v", catalog.Models)
	}
	byGroup := make(map[string]domain.ModelOffer)
	for _, model := range catalog.Models {
		byGroup[model.Group] = model
	}
	cheap := byGroup["cheap"]
	if cheap.PriceFunction.PerSecond != "0.05" || cheap.PriceFunction.PerRequest != "" || cheap.Metadata["pricing_revision"] != "revision-1" {
		t.Fatalf("cheap group price = %#v", cheap)
	}
	premium := byGroup["premium"]
	if premium.PriceFunction.FixedFee != "2" || premium.Metadata["fixed_duration_seconds"] != "8" || premium.PriceFunction.PerSecond != "" {
		t.Fatalf("premium group price = %#v", premium)
	}
}

func TestNewAPIGroupPricingTokenVectorUsesRawBaseBeforeGroupMultiplier(t *testing.T) {
	modelRatio := json.Number("1.5")
	completionRatio := json.Number("4")
	cacheRatio := json.Number("0.1")
	baseGroupRatio := json.Number("0.4")
	offer := newAPIGroupPricingOffer("token-model", "source", newAPIGroupPricing{
		Group: "source", Scope: "group_model", BillingMode: "ratio", BillingUnit: "per_million_tokens",
		QuotaType: 0, ModelRatio: &modelRatio, CompletionRatio: &completionRatio, CacheRatio: &cacheRatio,
		BaseGroupRatio: &baseGroupRatio,
	}, "revision-2")
	if offer.PriceFunction.InputPer1M != "3" || offer.PriceFunction.OutputPer1M != "12" || offer.PriceFunction.CacheReadPer1M != "0.3" {
		t.Fatalf("raw token vector = %#v", offer.PriceFunction)
	}
	if offer.PriceFunction.GroupMultiplier != "0.4" || offer.PriceFunction.RequiresModelBasePrice {
		t.Fatalf("group evidence = %#v", offer.PriceFunction)
	}
}

func TestNewAPIGroupPricingMarksUnknownUnitAndRatioConflictAmbiguous(t *testing.T) {
	var upstream newAPIPricingResponse
	err := json.Unmarshal([]byte(`{"success":true,"group_ratio":{"g":0.4},"data":[{"model_name":"m","group_pricing":{"g":{"group":"g","billing_mode":"fixed_price","billing_unit":"mystery","quota_type":1,"model_price":1,"base_group_ratio":0.5}}}]}`), &upstream)
	if err != nil {
		t.Fatal(err)
	}
	catalog := parseNewAPICatalog(upstream)
	if len(catalog.Models) != 1 || !catalog.Models[0].PriceFunction.RequiresModelBasePrice || catalog.Models[0].Metadata["pricing_ambiguity"] == "" {
		t.Fatalf("ambiguous group price = %#v", catalog.Models)
	}
}

func TestNewAPICatalogCompletenessCountersExposeDropsAndDuplicates(t *testing.T) {
	ratio := json.RawMessage("1")
	modelRatio := json.Number("1")
	completionRatio := json.Number("1")
	valid := newAPIPrice{ModelName: "m", ModelRatio: &modelRatio, CompletionRatio: &completionRatio,
		EnableGroups: []string{"g"}}
	catalog := parseNewAPICatalog(newAPIPricingResponse{PricingVersion: "v1",
		GroupRatio: map[string]json.RawMessage{"g": ratio}, Data: []newAPIPrice{valid}})
	if catalog.RawItemCount != 2 || catalog.ParsedItemCount != 2 || catalog.UniqueItemCount != 2 ||
		catalog.DroppedItemCount != 0 || catalog.SourceRevision != "v1" {
		t.Fatalf("complete catalog counters = %#v", catalog)
	}

	withDrop := parseNewAPICatalog(newAPIPricingResponse{GroupRatio: map[string]json.RawMessage{"g": ratio},
		Data: []newAPIPrice{valid, {ModelName: " "}}})
	if withDrop.RawItemCount != 3 || withDrop.ParsedItemCount != 2 || withDrop.DroppedItemCount != 1 {
		t.Fatalf("dropped catalog counters = %#v", withDrop)
	}

	withDuplicate := parseNewAPICatalog(newAPIPricingResponse{GroupRatio: map[string]json.RawMessage{"g": ratio},
		Data: []newAPIPrice{valid, valid}})
	if withDuplicate.RawItemCount != 3 || withDuplicate.ParsedItemCount != 3 || withDuplicate.UniqueItemCount != 2 {
		t.Fatalf("duplicate catalog counters = %#v", withDuplicate)
	}
}
