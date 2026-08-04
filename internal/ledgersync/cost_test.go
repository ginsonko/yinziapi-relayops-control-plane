package ledgersync

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestCostResolverDerivesRechargeAdjustedDirectCostExactlyOnce(t *testing.T) {
	resolver := testCostResolver()
	event := store.OperatingLedgerEvent{EventID: "event-1", EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro",
		Requests: 1, CashRevenue: "10", Currency: "CNY", Metadata: map[string]string{"channel_id": "7"}}
	derived, failure := resolver.derive(event, map[string]string{
		"channel_id": "7", "prompt_tokens": "1000000", "completion_tokens": "500000",
	})
	if failure != nil {
		t.Fatalf("derive failure = %#v", failure)
	}
	if derived.amount != "4.8" || derived.currency != "CNY" || derived.evidence != "estimated" {
		t.Fatalf("derived cost = %#v", derived)
	}
	if derived.metadata["procurement_cost_recharge_factor"] != "0.4" ||
		!strings.Contains(derived.metadata["procurement_cost_formula"], "source_effective_ratio(1.2)") {
		t.Fatalf("derived metadata = %#v", derived.metadata)
	}
}

func TestCostResolverPrefersExactChannelSupplierOrigin(t *testing.T) {
	resolver := testCostResolver()
	resolver.policies = append(resolver.policies, domain.PricingPolicy{ID: 2, SupplierID: 10, SourceGroup: "other-pro",
		SourceModelPattern: "gpt-test", SourceBasis: "group_ratio", TargetSiteID: 1, TargetGroup: "local-pro", Enabled: true})
	resolver.suppliers[10] = costSupplier{supplier: domain.Supplier{ID: 10, Name: "other", BaseURL: "https://other.example"},
		canonical: "https://other.example", snapshotState: "fresh", snapshot: domain.CatalogSnapshot{
			Groups: []domain.GroupOffer{{ExternalID: "other-pro", Name: "other-pro", EffectiveRatio: "0.1", ModelIDs: []string{"gpt-test"}}},
		}}
	binding, failure := resolver.resolveChannelBinding(resolver.channels[7], "local-pro", "gpt-test")
	if failure != nil {
		t.Fatalf("resolve binding failure = %#v", failure)
	}
	if binding.supplier.supplier.ID != 9 || binding.method != "exact_channel_supplier_origin" {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestCostResolverRejectsConflictingMultiRouteChannel(t *testing.T) {
	resolver := testCostResolver()
	channel := resolver.channels[7]
	channel.BaseURL = "https://gateway.example"
	resolver.channels[7] = channel
	resolver.policies = append(resolver.policies, domain.PricingPolicy{ID: 2, SupplierID: 10, SourceGroup: "other-pro",
		SourceModelPattern: "gpt-test", SourceBasis: "group_ratio", TargetSiteID: 1, TargetGroup: "local-pro", Enabled: true})
	resolver.suppliers[10] = costSupplier{supplier: domain.Supplier{ID: 10, Name: "other", BaseURL: "https://other.example"},
		canonical: "https://other.example", snapshotState: "fresh"}
	_, failure := resolver.resolveChannelBinding(channel, "local-pro", "gpt-test")
	if failure == nil || failure.Code != "channel_policy_binding_conflict" {
		t.Fatalf("conflict failure = %#v", failure)
	}
}

func TestCostResolverMatchesSourceGroupExternalID(t *testing.T) {
	resolver := testCostResolver()
	supplier := resolver.suppliers[9]
	supplier.snapshot.Groups[0].Name = "Upstream display name"
	supplier.snapshot.Groups[0].ExternalID = "upstream-pro"
	resolver.suppliers[9] = supplier
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", CashRevenue: "10",
		Currency: "CNY", Metadata: map[string]string{"channel_id": "7"}}
	derived, failure := resolver.derive(event, event.Metadata)
	if failure != nil || derived.metadata["procurement_cost_supplier_group_id"] != "upstream-pro" {
		t.Fatalf("derive = %#v failure = %#v", derived, failure)
	}
}

func TestCostResolverUsesApprovedEquivalentCostCoverage(t *testing.T) {
	resolver := testCostResolver()
	policy := resolver.policies[0]
	policy.SourceBasis = "equivalent_cost"
	policy.CoverageMode = "strict"
	resolver.policies[0] = policy
	resolver.targetCatalog.Models = []domain.ModelOffer{{ExternalID: "gpt-test", Name: "gpt-test", Group: "local-pro",
		PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "4", OutputPer1M: "8"}}}
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", CashRevenue: "10",
		Currency: "CNY", Metadata: map[string]string{"channel_id": "7"}}
	derived, failure := resolver.derive(event, event.Metadata)
	if failure != nil {
		t.Fatalf("derive failure = %#v", failure)
	}
	if derived.amount != "6" || derived.metadata["procurement_cost_policy_cost_ratio"] != "0.6" ||
		derived.metadata["procurement_cost_policy_coverage"] != "1" {
		t.Fatalf("equivalent cost = %#v", derived)
	}
}

func TestCostResolverRejectsZeroRevenueAndStaleSnapshot(t *testing.T) {
	resolver := testCostResolver()
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", Currency: "CNY",
		Metadata: map[string]string{"channel_id": "7"}}
	_, failure := resolver.derive(event, event.Metadata)
	if failure == nil || failure.Code != "positive_request_revenue_missing" {
		t.Fatalf("zero revenue failure = %#v", failure)
	}
	supplier := resolver.suppliers[9]
	supplier.snapshotState = "stale"
	resolver.suppliers[9] = supplier
	event.CashRevenue = "10"
	_, failure = resolver.derive(event, event.Metadata)
	if failure == nil || failure.Code != "supplier_snapshot_stale" {
		t.Fatalf("stale snapshot failure = %#v", failure)
	}
}

func TestExplicitUnroutedZeroCostRequiresCompleteZeroEvidence(t *testing.T) {
	event := store.OperatingLedgerEvent{EventKind: "usage", Requests: 1, UsageWeight: "0", CashRevenue: "0",
		Currency: "CNY", Metadata: map[string]string{"channel_id": "0", "prompt_tokens": "0", "completion_tokens": "0"}}
	derived, ok := explicitUnroutedZeroCost(event, "CNY")
	if !ok || derived.amount != "0" || derived.evidence != "observed" || derived.metadata["procurement_cost_method"] != "explicit_unrouted_zero_usage" {
		t.Fatalf("explicit zero = %#v ok=%v", derived, ok)
	}
	for name, mutate := range map[string]func(*store.OperatingLedgerEvent){
		"model":         func(value *store.OperatingLedgerEvent) { value.ModelID = "gpt-test" },
		"group":         func(value *store.OperatingLedgerEvent) { value.GroupName = "paid" },
		"channel":       func(value *store.OperatingLedgerEvent) { value.Metadata["channel_id"] = "7" },
		"revenue":       func(value *store.OperatingLedgerEvent) { value.CashRevenue = "0.01" },
		"usage":         func(value *store.OperatingLedgerEvent) { value.UsageWeight = "1" },
		"token":         func(value *store.OperatingLedgerEvent) { value.Metadata["prompt_tokens"] = "1" },
		"invalid_token": func(value *store.OperatingLedgerEvent) { value.Metadata["prompt_tokens"] = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := event
			candidate.Metadata = map[string]string{"channel_id": "0", "prompt_tokens": "0", "completion_tokens": "0"}
			mutate(&candidate)
			if value, accepted := explicitUnroutedZeroCost(candidate, "CNY"); accepted {
				t.Fatalf("unsafe zero accepted: %#v", value)
			}
		})
	}
}

func TestCostResolverUsesMirrorOnlyBindingForCurrentCatalogCost(t *testing.T) {
	resolver := testCostResolver()
	resolver.policies = []domain.PricingPolicy{{ID: 1 << 62, Name: "mirror cost binding", SupplierID: 9,
		SourceGroup: "upstream-pro", SourceModelPattern: "*", SourceBasis: "group_ratio", TargetSiteID: 1,
		TargetGroup: "local-pro", Enabled: true, UpdatedAt: time.Now().UTC()}}
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", Requests: 1,
		CashRevenue: "10", Currency: "CNY", Metadata: map[string]string{"channel_id": "7"}}
	derived, failure := resolver.derive(event, map[string]string{"channel_id": "7", "prompt_tokens": "1000000", "completion_tokens": "500000"})
	if failure != nil || derived.amount != "4.8" || derived.metadata["procurement_cost_policy_name"] != "mirror cost binding" {
		t.Fatalf("mirror-only cost = %#v failure=%#v", derived, failure)
	}
}

func TestCalculateModelCostPreservesExplicitFreePrice(t *testing.T) {
	amount, _, ok, reason := calculateModelCost(domain.PriceFunction{InputPer1M: "0"}, store.OperatingLedgerEvent{Requests: 1},
		map[string]string{"prompt_tokens": "1000"})
	if !ok || !amount.IsZero() {
		t.Fatalf("explicit free price became unknown: amount=%s ok=%v reason=%s", amount, ok, reason)
	}
}

func TestCalculateModelCostRejectsIncompleteTieredExpression(t *testing.T) {
	function := domain.PriceFunction{InputPer1M: "75", OutputPer1M: "450",
		Expression: `tier("short", p * 1 + c * 6 + cr * 0.1 + cc * 1.25)`}
	_, _, ok, reason := calculateModelCost(function, store.OperatingLedgerEvent{Requests: 1}, map[string]string{
		"prompt_tokens": "1000000", "completion_tokens": "1000",
	})
	if ok || reason != "billing_expression_requires_unavailable_dimensions" {
		t.Fatalf("tiered expression was treated as directly billable: ok=%v reason=%s", ok, reason)
	}
}

func TestCalculateModelCostRejectsCacheDoubleCountingRisk(t *testing.T) {
	function := domain.PriceFunction{InputPer1M: "2", OutputPer1M: "8", CacheReadPer1M: "0.2"}
	_, _, ok, reason := calculateModelCost(function, store.OperatingLedgerEvent{Requests: 1}, map[string]string{
		"prompt_tokens": "1000000", "completion_tokens": "1000", "cache_read_tokens": "900000",
	})
	if ok || reason != "cache_token_inclusion_contract_missing" {
		t.Fatalf("cache-inclusive prompt total was treated as directly billable: ok=%v reason=%s", ok, reason)
	}
}

func TestCalculateModelCostRejectsMixedPerRequestAndTokenPricing(t *testing.T) {
	_, _, ok, reason := calculateModelCost(domain.PriceFunction{PerRequest: "0.1", InputPer1M: "2"},
		store.OperatingLedgerEvent{Requests: 1}, map[string]string{"prompt_tokens": "1000"})
	if ok || reason != "mixed_per_request_and_dimension_pricing" {
		t.Fatalf("mixed price function was treated as directly billable: ok=%v reason=%s", ok, reason)
	}
}

func TestCalculateModelCostSupportsSimpleTokenAndPerRequestFunctions(t *testing.T) {
	amount, _, ok, reason := calculateModelCost(domain.PriceFunction{InputPer1M: "2", OutputPer1M: "8"},
		store.OperatingLedgerEvent{Requests: 1}, map[string]string{"prompt_tokens": "1000000", "completion_tokens": "500000"})
	if !ok || amount.String() != "6" || reason != "" {
		t.Fatalf("simple token price = %s ok=%v reason=%s", amount, ok, reason)
	}
	amount, _, ok, reason = calculateModelCost(domain.PriceFunction{PerRequest: "0.4"},
		store.OperatingLedgerEvent{Requests: 3}, nil)
	if !ok || amount.String() != "1.2" || reason != "" {
		t.Fatalf("per-request price = %s ok=%v reason=%s", amount, ok, reason)
	}
}

func TestCalculateModelCostSupportsPerSecondAndFixedFeeWithoutGuessingQuantity(t *testing.T) {
	amount, _, ok, reason := calculateModelCost(domain.PriceFunction{PerSecond: "0.08"},
		store.OperatingLedgerEvent{Requests: 1}, map[string]string{"seconds": "15"})
	if !ok || amount.String() != "1.2" || reason != "" {
		t.Fatalf("per-second price = %s ok=%v reason=%s", amount, ok, reason)
	}
	_, _, ok, reason = calculateModelCost(domain.PriceFunction{PerSecond: "0.08"},
		store.OperatingLedgerEvent{Requests: 1}, map[string]string{})
	if ok || reason != "request_dimension_missing:per_second" {
		t.Fatalf("missing duration was guessed: ok=%v reason=%s", ok, reason)
	}
	amount, _, ok, reason = calculateModelCost(domain.PriceFunction{FixedFee: "2.5"},
		store.OperatingLedgerEvent{Requests: 2}, nil)
	if !ok || amount.String() != "5" || reason != "" {
		t.Fatalf("fixed fee = %s ok=%v reason=%s", amount, ok, reason)
	}
}

func TestCostResolverUsesFrozenContractRevisionAndObservedRequestDimension(t *testing.T) {
	resolver := testCostResolver()
	model := domain.ListingModelPricingEvidence{Model: "gpt-test", BillingMode: "fixed_price", BillingUnit: "per_second",
		Currency: "CNY", RechargeValueFactor: "0.4", SourceCashGroupRatio: "1.2", SourceEvidenceHash: "source-frozen",
		SourceObservedAt: time.Now().UTC(), Dimensions: []domain.ListingPriceDimensionEvidence{{Dimension: "per_second", SourceCashCost: "0.08"}}}
	key := contractCostKey(1, "local-pro", "gpt-test")
	resolver.contractByRevision = map[string]map[string]contractCostEvidence{"revision-request": {key: {
		policyID: 1, targetGroup: "local-pro", appliedRevision: "revision-request", model: model,
	}}}
	resolver.contractBySource = map[string]contractCostEvidence{}
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", Currency: "CNY",
		Metadata: map[string]string{"channel_id": "7", "pricing_scope": "group_model", "pricing_revision": "revision-request", "seconds": "15"}}
	derived, failure := resolver.derive(event, event.Metadata)
	if failure != nil || derived.amount != "1.2" || derived.evidence != "observed" {
		t.Fatalf("frozen contract cost = %#v failure=%#v", derived, failure)
	}
	if derived.metadata["procurement_cost_target_pricing_revision"] != "revision-request" ||
		derived.metadata["procurement_cost_source_evidence_hash"] != "source-frozen" ||
		derived.metadata["procurement_cost_method"] != "group_model_contract_frozen" {
		t.Fatalf("frozen contract metadata = %#v", derived.metadata)
	}
	delete(event.Metadata, "seconds")
	_, failure = resolver.derive(event, event.Metadata)
	if failure == nil || failure.Code != "target_contract_request_seconds_missing" {
		t.Fatalf("missing seconds failure = %#v", failure)
	}
}

func TestCostResolverFindsUnchangedModelEvidenceThroughCurrentContractSourceRevision(t *testing.T) {
	resolver := testCostResolver()
	model := domain.ListingModelPricingEvidence{Model: "gpt-test", BillingMode: "fixed_price", BillingUnit: "per_request",
		Currency: "CNY", RechargeValueFactor: "0.4", SourceCashGroupRatio: "1.2", SourceEvidenceHash: "source-model-v1",
		SourceObservedAt: time.Now().UTC(), Dimensions: []domain.ListingPriceDimensionEvidence{{Dimension: "per_request", SourceCashCost: "0.6"}}}
	key := contractCostKey(1, "local-pro", "gpt-test")
	resolver.contractByRevision = map[string]map[string]contractCostEvidence{}
	resolver.contractBySource = map[string]contractCostEvidence{"source-model-v1\x00" + key: {policyID: 1, targetGroup: "local-pro", model: model}}
	resolver.targetContract = &domain.GroupModelPricingContract{Revision: "current-global-revision", Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"local-pro": {"gpt-test": {BillingMode: "fixed_price", SourceRevision: "source-model-v1"}},
	}}
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro", Requests: 2, Currency: "CNY",
		Metadata: map[string]string{"channel_id": "7", "pricing_scope": "group_model", "pricing_revision": "current-global-revision"}}
	derived, failure := resolver.derive(event, event.Metadata)
	if failure != nil || derived.amount != "1.2" {
		t.Fatalf("current contract source lookup = %#v failure=%#v", derived, failure)
	}
}

func TestNewAPILogNestedOtherPreservesOnlyPricingAndBillingEvidence(t *testing.T) {
	other, _ := json.Marshal(map[string]any{"pricing_scope": "group_model", "pricing_revision": "revision-1",
		"task_billing_unit": "per_second", "seconds": 15, "password": "must-not-copy"})
	encodedOther, _ := json.Marshal(string(other))
	row := map[string]json.RawMessage{"other": encodedOther, "channel_id": json.RawMessage(`7`)}
	values := rawRowStrings(row)
	if values["pricing_revision"] != "revision-1" || values["seconds"] != "15" || values["password"] != "" {
		t.Fatalf("nested other evidence = %#v", values)
	}
	event := baseRemoteEvent(1, streamLogs, "r1", time.Now().UTC(), row, "row-hash")
	if event.Metadata["pricing_scope"] != "group_model" || event.Metadata["task_billing_unit"] != "per_second" || event.Metadata["password"] != "" {
		t.Fatalf("ledger metadata = %#v", event.Metadata)
	}
}

func TestCostResolverFallsBackFromTieredExpressionToApprovedRatio(t *testing.T) {
	resolver := testCostResolver()
	supplier := resolver.suppliers[9]
	supplier.snapshot.Models[0].PriceFunction = domain.PriceFunction{InputPer1M: "75", OutputPer1M: "450",
		Expression: `tier("short", p + c * 6 + cr * 0.1 + cc * 1.25)`}
	resolver.suppliers[9] = supplier
	event := store.OperatingLedgerEvent{EventKind: "usage", ModelID: "gpt-test", GroupName: "local-pro",
		CashRevenue: "10", Currency: "CNY", Metadata: map[string]string{"channel_id": "7"}}
	derived, failure := resolver.derive(event, map[string]string{
		"channel_id": "7", "prompt_tokens": "1000000", "completion_tokens": "500000",
	})
	if failure != nil {
		t.Fatalf("ratio fallback failed: %#v", failure)
	}
	if derived.amount != "12" || derived.metadata["procurement_cost_method"] != "policy_channel_revenue_ratio" ||
		derived.metadata["procurement_cost_direct_catalog_fallback_reason"] != "billing_expression_requires_unavailable_dimensions" {
		t.Fatalf("ratio fallback = %#v", derived)
	}
}

func testCostResolver() costResolver {
	now := time.Now().UTC()
	sourceSnapshot := domain.CatalogSnapshot{Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "source-hash"},
		Groups: []domain.GroupOffer{{ExternalID: "upstream-pro", Name: "upstream-pro", Ratio: "3", RawRatio: "3",
			EffectiveRatio: "1.2", RechargeValueFactor: "0.4", ModelIDs: []string{"gpt-test"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-test", Name: "gpt-test", Group: "upstream-pro",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "4"}}}}
	return costResolver{target: domain.TargetSite{ID: 1, Name: "target"}, currency: "CNY", cashRate: decimal.NewFromInt(1),
		maxAge: time.Hour, channels: map[int64]domain.TargetChannel{7: {ID: 7, Name: "route-7", BaseURL: "https://upstream.example", Group: "local-pro"}},
		targetCatalog: domain.CatalogSnapshot{Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "target-hash"},
			Groups: []domain.GroupOffer{{ExternalID: "local-pro", Name: "local-pro", Ratio: "1", ModelIDs: []string{"gpt-test"}}}},
		policies: []domain.PricingPolicy{{ID: 1, Name: "mapping", SupplierID: 9, SourceGroup: "upstream-pro", SourceModelPattern: "gpt-test",
			SourceBasis: "group_ratio", TargetSiteID: 1, TargetGroup: "local-pro", Enabled: true, UpdatedAt: now}},
		suppliers: map[int64]costSupplier{9: {supplier: domain.Supplier{ID: 9, Name: "upstream", BaseURL: "https://upstream.example"},
			snapshot: sourceSnapshot, canonical: "https://upstream.example", snapshotState: "fresh"}},
		policyCost: map[int64]policyCostBasis{}, policyCostDone: map[int64]bool{}}
}
