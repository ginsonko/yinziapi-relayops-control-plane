package mirror

import (
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNormalizeSourceGroupKeepsGroupScopedPricesAndBillingUnits(t *testing.T) {
	group := domain.GroupOffer{ExternalID: "cheap", Name: "Cheap", RawRatio: "0.12", RechargeValueFactor: "0.4",
		ModelIDs: []string{"same", "video", "fixed"}}
	snapshot := domain.CatalogSnapshot{Models: []domain.ModelOffer{
		{ExternalID: "same", Name: "Same", Group: "cheap", Metadata: map[string]string{"upstream_request_name": "same-v2"},
			PriceFunction: domain.PriceFunction{Currency: "usd", InputPer1M: "0.10", OutputPer1M: "0.20"}},
		{ExternalID: "same", Name: "Same", Group: "premium",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "9", OutputPer1M: "18"}},
		{ExternalID: "video", Group: "cheap", PriceFunction: domain.PriceFunction{Currency: "USD", PerSecond: "0.03"}},
		{ExternalID: "fixed", Group: "cheap", Metadata: map[string]string{"fixed_duration_seconds": "8"},
			PriceFunction: domain.PriceFunction{Currency: "USD", FixedFee: "2"}},
	}}
	result, err := NormalizeSourceGroup(snapshot, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Models) != 3 || result.RawGroupRatio != "0.12" || result.RechargeValueFactor != "0.4" {
		t.Fatalf("canonical group = %#v", result)
	}
	byID := map[string]CanonicalModel{}
	for _, model := range result.Models {
		byID[model.SourceModelID] = model
	}
	if byID["same"].UpstreamRequestName != "same-v2" || byID["same"].BillingMode != "token_vector" ||
		len(byID["same"].PriceDimensions) != 2 || byID["same"].PriceDimensions[0].DecimalValue != "0.1" {
		t.Fatalf("token model = %#v", byID["same"])
	}
	if byID["video"].PriceDimensions[0].Unit != "currency_per_second" ||
		byID["fixed"].PriceDimensions[0].Kind != "fixed_duration" || byID["fixed"].PriceDimensions[0].DurationSeconds != 8 {
		t.Fatalf("task models = video:%#v fixed:%#v", byID["video"], byID["fixed"])
	}
}

func TestSourceSemanticHashCollapsesDecimalFormatting(t *testing.T) {
	group := CanonicalGroup{SourceGroupID: "g", SourceGroupName: "G", RawGroupRatio: "0.10",
		RechargeValueFactor: "0.400", Models: []CanonicalModel{{SourceModelID: "m", SourceExposedName: "M",
			UpstreamRequestName: "m", BillingMode: "fixed_price", Currency: "USD",
			PriceDimensions: []PriceDimension{{Kind: "per_request", Unit: "currency_per_request", DecimalValue: "1.00"}}}}}
	left, err := SourceSemanticHash(group)
	if err != nil {
		t.Fatal(err)
	}
	group.RawGroupRatio = "0.1"
	group.RechargeValueFactor = "0.4"
	group.Models[0].PriceDimensions[0].DecimalValue = "1"
	right, err := SourceSemanticHash(group)
	if err != nil || left != right {
		t.Fatalf("formatting-only hash changed: %s %s %v", left, right, err)
	}
	group.Models[0].PriceDimensions[0].DecimalValue = "1.01"
	changed, _ := SourceSemanticHash(group)
	if changed == left {
		t.Fatal("functional price change did not change source hash")
	}
}

func TestSourceGroupStateHashesAreDistinctAndRejectInvalidIdentity(t *testing.T) {
	empty, err := EmptySourceSemanticHash(domain.GroupOffer{ExternalID: "video", RawRatio: "1", RechargeValueFactor: "1"})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := MissingSourceSemanticHash("video")
	if err != nil {
		t.Fatal(err)
	}
	if empty == missing {
		t.Fatal("empty and missing source states produced the same semantic hash")
	}
	invalid := domain.GroupOffer{ExternalID: "video\x00shadow", RawRatio: "1", RechargeValueFactor: "1", ModelIDs: []string{"m"}}
	snapshot := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", Group: invalid.ExternalID,
		PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}}}
	if _, err := NormalizeSourceGroup(snapshot, invalid); err == nil {
		t.Fatal("present source group accepted a NUL-containing identity")
	}
	if _, err := EmptySourceSemanticHash(invalid); err == nil {
		t.Fatal("empty source group accepted a NUL-containing identity")
	}
	if _, err := MissingSourceSemanticHash(invalid.ExternalID); err == nil {
		t.Fatal("missing source group accepted a NUL-containing identity")
	}
}

func TestPerItemOwnershipHashesIgnoreProvenanceButTrackFunctionalState(t *testing.T) {
	price := "0.1200"
	setting := domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &price,
		TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"}, Source: "relayops:policy/a",
		SourceRevision: "source-r1", SourceObservedAtUnixMS: 1}
	first, err := TargetModelFunctionalHash("video", "jimeng", setting)
	if err != nil {
		t.Fatal(err)
	}
	setting.SourceRevision = "source-r2"
	setting.SourceObservedAtUnixMS = 2
	setting.Source = "relayops:policy/b"
	second, err := TargetModelFunctionalHash("video", "jimeng", setting)
	if err != nil || first != second {
		t.Fatalf("provenance changed functional hash: %s %s %v", first, second, err)
	}
	changed := "0.13"
	setting.ModelPrice = &changed
	third, _ := TargetModelFunctionalHash("video", "jimeng", setting)
	if third == first {
		t.Fatal("price change did not change the per-item functional hash")
	}
	present, err := TargetChannelModelFunctionalHash(41, "jimeng", true)
	if err != nil {
		t.Fatal(err)
	}
	absent, _ := TargetChannelModelFunctionalHash(41, "jimeng", false)
	if present == absent {
		t.Fatal("channel membership presence did not change the hash")
	}
}

func TestPerItemOwnershipHashesCollapseFloatRoundTripNoise(t *testing.T) {
	left := "30.100000400000003"
	right := "30.1000004"
	first, err := TargetModelFunctionalHash("video", "seedance", domain.GroupModelPricingSetting{
		BillingMode: "fixed_price", ModelPrice: &left,
		TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := TargetModelFunctionalHash("video", "seedance", domain.GroupModelPricingSetting{
		BillingMode: "fixed_price", ModelPrice: &right,
		TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"},
	})
	if err != nil || first != second {
		t.Fatalf("float round-trip noise changed functional hash: %s %s %v", first, second, err)
	}
}

func TestTargetFunctionalHashExcludesProvenanceButAuthorityHashTracksIt(t *testing.T) {
	priceA, priceB := "0.10", "0.1"
	left := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"sale": {"m": {BillingMode: "fixed_price", ModelPrice: &priceA, Source: "relayops:policy/a",
			SourceRevision: "source-a", SourceObservedAtUnixMS: 100}},
	}}
	right := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"sale": {"m": {BillingMode: "fixed_price", ModelPrice: &priceB, Source: "relayops:policy/a",
			SourceRevision: "source-a", SourceObservedAtUnixMS: 200}},
	}}
	functionalLeft, err := TargetFunctionalHash(left, map[string]string{"sale": "1.30"})
	if err != nil {
		t.Fatal(err)
	}
	functionalRight, err := TargetFunctionalHash(right, map[string]string{"sale": "1.3"})
	if err != nil || functionalLeft != functionalRight {
		t.Fatalf("provenance changed functional hash: %s %s %v", functionalLeft, functionalRight, err)
	}
	authorityLeft, _ := TargetAuthorityHash(left)
	authorityRight, _ := TargetAuthorityHash(right)
	if authorityLeft != authorityRight {
		t.Fatal("observation timestamp changed the stable authority hash")
	}
	right.Groups["sale"]["m"] = domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &priceB,
		Source: "relayops:policy/b", SourceRevision: "source-b", SourceObservedAtUnixMS: 200}
	authorityRight, _ = TargetAuthorityHash(right)
	if authorityLeft == authorityRight {
		t.Fatal("owner or semantic source revision change was not detected")
	}
}

func TestNormalizeSourceGroupRejectsConflictingOrMixedRecords(t *testing.T) {
	group := domain.GroupOffer{ExternalID: "g", Ratio: "1", ModelIDs: []string{"m"}}
	snapshot := domain.CatalogSnapshot{Models: []domain.ModelOffer{
		{ExternalID: "m", Group: "g", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}},
		{ExternalID: "m", Group: "g", PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "2"}},
	}}
	if _, err := NormalizeSourceGroup(snapshot, group); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	snapshot.Models = []domain.ModelOffer{{ExternalID: "m", Group: "g", PriceFunction: domain.PriceFunction{
		Currency: "USD", InputPer1M: "1", OutputPer1M: "2", PerRequest: "1"}}}
	normalized, err := NormalizeSourceGroup(snapshot, group)
	if err != nil || len(normalized.Models) != 1 || normalized.Models[0].BillingMode != membershipOnlyBillingMode ||
		len(normalized.Models[0].PriceDimensions) != 0 {
		t.Fatalf("mixed billing must fall back to membership-only: %#v %v", normalized, err)
	}
}

func TestNormalizeSourceGroupAllowsMembershipWithoutMonetaryDimensions(t *testing.T) {
	group := domain.GroupOffer{ExternalID: "64", Name: "CC-MAX", Ratio: "0.55", ModelIDs: []string{"claude-fable-5"}}
	snapshot := domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "claude-fable-5", Group: "CC-MAX",
		Currency: "USD", BillingMode: "unknown", PriceFunction: domain.PriceFunction{Currency: "USD",
			Basis: "sub2api_customer_key_catalog", GroupMultiplier: "0.55"}}}}
	result, err := NormalizeSourceGroup(snapshot, group)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Models) != 1 || result.Models[0].BillingMode != membershipOnlyBillingMode ||
		len(result.Models[0].PriceDimensions) != 0 || result.Models[0].UpstreamRequestName != "claude-fable-5" {
		t.Fatalf("membership-only model = %#v", result.Models)
	}
}
