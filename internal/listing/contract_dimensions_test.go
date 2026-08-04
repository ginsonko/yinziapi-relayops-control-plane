package listing

import (
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestEffectiveContractDimensionsKeepsBillingUnitsAndTokenVectorCoherent(t *testing.T) {
	price := "2.5"
	fixed, err := EffectiveContractDimensions(domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &price,
		TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"}}, "0.4")
	if err != nil || fixed["per_second"] != "1" || len(fixed) != 1 {
		t.Fatalf("fixed dimensions = %#v, err=%v", fixed, err)
	}
	modelRatio, completion, cache := "0.5", "3", "0.2"
	token, err := EffectiveContractDimensions(domain.GroupModelPricingSetting{BillingMode: "ratio", ModelRatio: &modelRatio,
		CompletionRatio: &completion, CacheRatio: &cache}, "2")
	if err != nil {
		t.Fatal(err)
	}
	if token["input_per_1m"] != "2" || token["output_per_1m"] != "6" || token["cache_read_per_1m"] != "0.4" {
		t.Fatalf("token dimensions = %#v", token)
	}
}

func TestEffectiveContractDimensionsRejectsIncompleteContracts(t *testing.T) {
	if _, err := EffectiveContractDimensions(domain.GroupModelPricingSetting{BillingMode: "fixed_price"}, "1"); err == nil {
		t.Fatal("incomplete fixed contract was accepted")
	}
	completion := "1"
	if _, err := EffectiveContractDimensions(domain.GroupModelPricingSetting{BillingMode: "ratio", CompletionRatio: &completion}, "1"); err == nil {
		t.Fatal("incomplete ratio contract was accepted")
	}
}
