package intelligence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestDashboardCachesOnlyMatchingSuccessfulConfiguration(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := NewService(st)
	config := DefaultConfig()
	first, err := service.Dashboard(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	credential := domain.Credential{Name: "cache target", Kind: "access-token", SiteURL: "https://cache-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "cache target", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertOperatingLedgerEvents(ctx, []store.OperatingLedgerEvent{{EventID: "cache-event",
		TargetSiteID: target.ID, OccurredAt: now, EventKind: "usage", Requests: 1, Source: "test"}}); err != nil {
		t.Fatal(err)
	}

	cached, err := service.Dashboard(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if cached.GeneratedAt != first.GeneratedAt || cached.Ledger.EventCount != first.Ledger.EventCount {
		t.Fatalf("matching configuration did not reuse the successful dashboard: first=%#v cached=%#v", first.Ledger, cached.Ledger)
	}
	different := config
	different.CashLookbackDays++
	refreshed, err := service.Dashboard(ctx, different)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Ledger.EventCount != 1 || refreshed.GeneratedAt.Before(first.GeneratedAt) {
		t.Fatalf("different configuration reused stale dashboard: %#v", refreshed)
	}
}

func TestEstimateDailyDemandBlendsObservedWindows(t *testing.T) {
	now := time.Now().UTC()
	items := []store.OpportunityUsage{
		{ModelID: "gpt-5.6-sol", WindowDays: 7, DemandWeight: "70", Confidence: "0.95", ObservedAt: now},
		{ModelID: "gpt-5.6-sol", WindowDays: 30, DemandWeight: "150", Confidence: "0.95", ObservedAt: now},
		{ModelID: "claude-other", WindowDays: 7, DemandWeight: "7000", Confidence: "0.95", ObservedAt: now},
	}
	demand, level, _, models := estimateDailyDemand(items, "gpt-*")
	if level != "observed" || models != 1 {
		t.Fatalf("evidence = %s, models = %d", level, models)
	}
	want := decimal.RequireFromString("8.5") // 70/7*0.7 + 150/30*0.3
	if !demand.Equal(want) {
		t.Fatalf("daily demand = %s, want %s", demand, want)
	}
}

func TestUnifiedMoneyMergesCNYAndUSDWithoutHidingUnknownItems(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(Config{})
	var accumulator unifiedMoneyAccumulator
	accumulator.add("12.5", "USD", 1, config)
	accumulator.add("7.5", "CNY", 1, config)
	metric := accumulator.metric(config, "observed", "")
	if !metric.Available || metric.Amount != "20" || metric.Currency != "CNY" || metric.KnownItems != 2 || metric.UnknownItems != 0 {
		t.Fatalf("unified money = %#v", metric)
	}
	accumulator.unknown(1)
	partial := accumulator.metric(config, "observed", "")
	if partial.Available || partial.Amount != "20" || partial.Evidence != "partial" || partial.UnknownItems != 1 {
		t.Fatalf("partial unified money = %#v", partial)
	}
}

func TestUnifiedGroupSummaryNeverTreatsMissingCostAsZero(t *testing.T) {
	t.Parallel()
	items := []store.LedgerDimensionSummary{{Label: "gpt-plus", Currency: "USD", EventCount: 2, Requests: 7,
		RevenueKnownEvents: 2, CostKnownEvents: 1, KnownRevenue: "10", KnownCost: "3"}}
	result := unifiedGroupSummaries(items, NormalizeConfig(Config{}))
	if len(result) != 1 || result[0].Revenue.Amount != "10" || result[0].RevenueCoverage != "1" ||
		result[0].CostCoverage != "0.5" || result[0].GrossProfit.Available || result[0].GrossMargin.Available {
		t.Fatalf("group with partial cost evidence = %#v", result)
	}
}

func TestProfitQualitySeparatesStrictProfitFromOperatingEstimate(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	report := store.LedgerFinancialSummary{WindowStart: now.Add(-time.Hour), WindowEnd: now, IncludedUsageEventCount: 4,
		Coverage: store.LedgerCoverageSummary{Status: "partial"}, ByCurrency: []store.LedgerCurrencySummary{{
			Currency: "USD", UsageEventCount: 4, UnknownUsageRevenueEvents: 0, UnknownUsageCostEvents: 3,
		}}}
	quality := buildProfitDataQuality(report, false)
	if quality.StrictProfitAvailable || quality.RevenueKnownEvents != 4 || quality.RevenueUnknownEvents != 0 ||
		quality.CostKnownEvents != 1 || quality.CostUnknownEvents != 3 || quality.RevenueCoverage != "1" ||
		quality.CostCoverage != "0.25" || len(quality.RequiredLedgerFields) != 1 || quality.RequiredLedgerFields[0] != "procurement_cost" ||
		!strings.Contains(quality.NextAction, "无需录入充值") {
		t.Fatalf("profit quality = %#v", quality)
	}

	config := NormalizeConfig(Config{})
	var revenue, upstreamSpend unifiedMoneyAccumulator
	revenue.add("100", "USD", 4, config)
	upstreamSpend.add("60", "USD", 2, config)
	gap, margin := operatingGapEstimate(revenue, upstreamSpend, config)
	if !gap.Available || gap.Amount != "40" || gap.Evidence != "estimated" || !margin.Available || margin.Value != "0.4" ||
		!strings.Contains(gap.Note, "不算充值") || !strings.Contains(gap.Note, "包含管理员") || !strings.Contains(gap.Note, "不参与自动调价") ||
		!strings.Contains(margin.Formula, "full_site_cash_revenue_including_administrators") {
		t.Fatalf("operating estimate gap=%#v margin=%#v", gap, margin)
	}
	upstreamSpend.unknown(1)
	partial, partialMargin := operatingGapEstimate(revenue, upstreamSpend, config)
	if partial.Available || partial.Amount != "40" || partial.Evidence != "partial" || partial.UnknownItems != 1 || partialMargin.Available {
		t.Fatalf("partial operating estimate gap=%#v margin=%#v", partial, partialMargin)
	}
}

func TestProfitQualityIncludesActionableCostResolutionDiagnostics(t *testing.T) {
	now := time.Now().UTC()
	quality := ProfitDataQuality{Blockers: []string{}, NextAction: "generic"}
	attachCostResolutionDiagnostics(&quality, []store.TargetLedgerCostResolutionStatus{{TargetSiteID: 7,
		WindowStart: now.Add(-time.Hour), WindowEnd: now, ScannedEvents: 5, ResolvedEvents: 2, UnresolvedEvents: 3, ObservedAt: now,
		Reasons: []store.TargetLedgerCostResolutionReason{{Code: "channel_policy_binding_missing", Message: "mapping missing", Count: 3,
			Samples: []store.TargetLedgerCostResolutionSample{{ChannelName: "route-12", TargetGroup: "plus", SupplierName: "BiuAPI", SourceGroup: "17"}}}}}},
		[]domain.TargetSite{{ID: 7, Name: "yinziapi", Enabled: true}})
	if quality.CostResolutionUnresolvedEvents != 3 || quality.CostResolutionResolvedEvents != 2 || len(quality.CostResolutionReasons) != 1 ||
		!strings.Contains(quality.Blockers[len(quality.Blockers)-1], "route-12") || !strings.Contains(quality.NextAction, "自动重试") {
		t.Fatalf("cost resolution quality = %#v", quality)
	}
}

func TestWeightedMarginUsesRevenueWeightAndReportsPartialCoverage(t *testing.T) {
	config := NormalizeConfig(Config{})
	var revenue, cost unifiedMoneyAccumulator
	revenue.add("90", "CNY", 1, config)
	revenue.add("10", "CNY", 1, config)
	cost.add("9", "CNY", 1, config)
	cost.add("1", "CNY", 1, config)
	metric := weightedMarginMetric(revenue, cost, 3, true)
	if !metric.Available || metric.Value != "0.9" || metric.Numerator != "90" || metric.Denominator != "100" ||
		metric.MatchedItems != 2 || metric.TotalItems != 3 || metric.Coverage != "0.666667" || metric.Evidence != "ledger_matched_partial" ||
		!strings.Contains(metric.Note, "排除管理员") {
		t.Fatalf("weighted margin = %#v", metric)
	}
	var unknown, missingCost unifiedMoneyAccumulator
	unknown.add("1", "CNY", 1, config)
	missingCost.unknown(1)
	missing := weightedMarginMetric(unknown, missingCost, 1, false)
	if missing.Available || missing.Evidence != "missing" {
		t.Fatalf("missing weighted margin = %#v", missing)
	}
}

func TestEstimatedGrossProfitUsesMatchedRevenueWeightedCostRate(t *testing.T) {
	config := NormalizeConfig(Config{})
	var revenue, matchedRevenue, matchedCost unifiedMoneyAccumulator
	revenue.add("200", "CNY", 4, config)
	matchedRevenue.add("100", "CNY", 3, config)
	matchedCost.add("75", "CNY", 3, config)
	metric, ok := estimatedGrossProfitMetric(revenue, matchedRevenue, matchedCost, 4, config)
	if !ok || !metric.Available || metric.Amount != "50" || metric.Evidence != "estimated" || metric.UnknownItems != 1 ||
		!strings.Contains(metric.Note, "3/4") || !strings.Contains(metric.Note, "不参与自动调价") {
		t.Fatalf("estimated gross profit = %#v ok=%v", metric, ok)
	}
	matchedRevenue.unknown(1)
	if value, accepted := estimatedGrossProfitMetric(revenue, matchedRevenue, matchedCost, 4, config); accepted {
		t.Fatalf("incomplete matched basis accepted: %#v", value)
	}
}

func TestOperatingSnapshotSubtractsNetCommissionCostFromGrossProfit(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "commission-target", Kind: "access-token", SiteURL: "https://commission.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "commission-target", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	events := []store.OperatingLedgerEvent{
		{EventID: "usage", TargetSiteID: target.ID, OccurredAt: now.Add(-time.Minute), EventKind: "usage",
			Requests: 1, UsageWeight: "1", UsageUnit: "request", UsageEvidence: "exact", CashRevenue: "10",
			ProcurementCost: "6", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact",
			Source: "newapi", SubjectUserID: "consumer", SubjectRole: "user"},
		{EventID: "commission-award", TargetSiteID: target.ID, OccurredAt: now.Add(-50 * time.Second), EventKind: "other_expense",
			Amount: "2", AmountEvidence: "exact", Currency: "CNY", UsageEvidence: "missing", RevenueEvidence: "missing",
			CostEvidence: "missing", Source: "newapi_target_commission", SubjectUserID: "consumer",
			Metadata: map[string]string{"cost_kind": "consumption_commission"}},
		{EventID: "commission-reversal", TargetSiteID: target.ID, OccurredAt: now.Add(-40 * time.Second), EventKind: "other_income",
			Amount: "0.5", AmountEvidence: "exact", Currency: "CNY", UsageEvidence: "missing", RevenueEvidence: "missing",
			CostEvidence: "missing", Source: "newapi_target_commission", SubjectUserID: "consumer",
			Metadata: map[string]string{"cost_kind": "consumption_commission"}},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewService(st).operatingSnapshot(ctx, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TodayGrossProfit.Amount != "4" || !snapshot.TodayGrossProfit.Available ||
		snapshot.TodayCommissionCost.Amount != "1.5" || !snapshot.TodayCommissionCost.Available ||
		snapshot.TodayNetProfit.Amount != "2.5" || !snapshot.TodayNetProfit.Available ||
		snapshot.TodayNetMargin.Value != "0.25" || !snapshot.TodayNetMargin.Available ||
		snapshot.TodayNetMargin.Numerator != "2.5" || snapshot.TodayNetMargin.Denominator != "10" {
		t.Fatalf("commission-aware operating snapshot = %#v", snapshot)
	}
}

func TestOperatingSnapshotCarriesEstimatedGrossProfitThroughCommission(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "estimated-commission-target", Kind: "access-token", SiteURL: "https://estimated-commission.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: credential.Name, BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	events := []store.OperatingLedgerEvent{
		{EventID: "usage-with-cost", TargetSiteID: target.ID, OccurredAt: now.Add(-time.Minute), EventKind: "usage",
			Requests: 1, UsageWeight: "1", UsageUnit: "request", UsageEvidence: "exact", CashRevenue: "10",
			ProcurementCost: "6", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact",
			Source: "newapi", SubjectUserID: "consumer", SubjectRole: "user"},
		{EventID: "usage-estimated-cost", TargetSiteID: target.ID, OccurredAt: now.Add(-50 * time.Second), EventKind: "usage",
			Requests: 1, UsageWeight: "1", UsageUnit: "request", UsageEvidence: "exact", CashRevenue: "10",
			Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "missing",
			Source: "newapi", SubjectUserID: "consumer", SubjectRole: "user"},
		{EventID: "commission-award-estimated", TargetSiteID: target.ID, OccurredAt: now.Add(-40 * time.Second), EventKind: "other_expense",
			Amount: "2", AmountEvidence: "exact", Currency: "CNY", UsageEvidence: "missing", RevenueEvidence: "missing",
			CostEvidence: "missing", Source: "newapi_target_commission", SubjectUserID: "consumer",
			Metadata: map[string]string{"cost_kind": "consumption_commission"}},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewService(st).operatingSnapshot(ctx, DefaultConfig(), now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TodayGrossProfit.Amount != "8" || snapshot.TodayGrossProfit.Evidence != "estimated" ||
		snapshot.TodayNetProfit.Amount != "6" || snapshot.TodayNetProfit.Evidence != "estimated" || !snapshot.TodayNetProfit.Available ||
		snapshot.TodayNetMargin.Value != "0.3" || snapshot.TodayNetMargin.Evidence != "estimated" || !snapshot.TodayNetMargin.Available ||
		snapshot.TodayNetMargin.MatchedItems != 1 || snapshot.TodayNetMargin.TotalItems != 2 || snapshot.TodayNetMargin.Coverage != "0.5" {
		t.Fatalf("estimated commission-aware operating snapshot = %#v", snapshot)
	}
}

func TestEstimateDailyDemandKeepsMissingUnknown(t *testing.T) {
	demand, level, _, models := estimateDailyDemand(nil, "*")
	if !demand.IsZero() || level != "missing" || models != 0 {
		t.Fatalf("missing demand became %#v %q %d", demand, level, models)
	}
}

func TestEstimateDailyDemandMarksSingleWindowAsEstimated(t *testing.T) {
	now := time.Now().UTC()
	demand, level, _, models := estimateDailyDemand([]store.OpportunityUsage{
		{ModelID: "gpt-5.6-sol", WindowDays: 7, DemandWeight: "70", Confidence: "0.95", ObservedAt: now},
	}, "gpt-*")
	if !demand.Equal(decimal.NewFromInt(10)) || level != "estimated" || models != 1 {
		t.Fatalf("single-window demand = %s, %s, %d", demand, level, models)
	}
}

func TestEstimateDailyDemandPrefersFreshEvidenceBeforeConfidence(t *testing.T) {
	now := time.Now().UTC()
	demand, level, _, _ := estimateDailyDemand([]store.OpportunityUsage{
		{ModelID: "gpt-5.6-sol", WindowDays: 7, DemandWeight: "700", Confidence: "0.99", ObservedAt: now.Add(-24 * time.Hour)},
		{ModelID: "gpt-5.6-sol", WindowDays: 7, DemandWeight: "70", Confidence: "0.8", ObservedAt: now},
		{ModelID: "gpt-5.6-sol", WindowDays: 30, DemandWeight: "150", Confidence: "0.95", ObservedAt: now},
	}, "gpt-*")
	if !demand.Equal(decimal.RequireFromString("8.5")) || level != "estimated" {
		t.Fatalf("stale high-confidence evidence displaced fresher evidence: %s, %s", demand, level)
	}
}

func TestPriceIncreaseChangesApproveScenarioWithoutChangingRejectBaseline(t *testing.T) {
	proposal := domain.ChangeProposal{Status: "pending", ExpectedOld: "0.06", ProposedValue: "0.07"}
	input := forecastInputs{currentRatio: decimal.RequireFromString("0.06"), proposedRatio: decimal.RequireFromString("0.07"),
		revenueFactor: decimal.NewFromInt(1), riskCost: decimal.RequireFromString("0.05"),
		elasticity: decimal.RequireFromString("-0.35"), priorUncertainty: decimal.RequireFromString("0.55"),
		observedUncertainty: decimal.RequireFromString("0.3"), metricBasis: equivalentContributionBasis,
		metricUnit: equivalentContributionUnit}
	result, err := forecasts(proposal, decimal.NewFromInt(100), "observed", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 {
		t.Fatalf("forecast windows = %d", len(result))
	}
	for index, want := range []struct {
		hours int
		label string
	}{{1, "1 小时"}, {24, "24 小时"}, {MaximumForecastHorizonHours, "7 天"}} {
		if result[index].Hours != want.hours || result[index].Label != want.label {
			t.Fatalf("forecast window %d = %d/%q, want %d/%q", index, result[index].Hours, result[index].Label, want.hours, want.label)
		}
	}
	approveDemand := decimal.RequireFromString(result[1].Approve.Demand.Base)
	rejectDemand := decimal.RequireFromString(result[1].Reject.Demand.Base)
	if !approveDemand.LessThan(rejectDemand) {
		t.Fatalf("price increase demand: approve=%s reject=%s", approveDemand, rejectDemand)
	}
	if result[1].OutcomeStatus != "awaiting_decision" {
		t.Fatalf("pending proposal outcome = %s", result[1].OutcomeStatus)
	}
}

func TestForecastUsesConfiguredPriorUncertainty(t *testing.T) {
	proposal := domain.ChangeProposal{Status: "pending", ExpectedOld: "1", ProposedValue: "1"}
	input := forecastInputs{currentRatio: decimal.NewFromInt(1), proposedRatio: decimal.NewFromInt(1),
		revenueFactor: decimal.NewFromInt(1), riskCost: decimal.RequireFromString("0.5"), elasticity: decimal.RequireFromString("-0.35"),
		priorUncertainty: decimal.RequireFromString("0.8"), observedUncertainty: decimal.RequireFromString("0.1"),
		metricBasis: equivalentContributionBasis, metricUnit: equivalentContributionUnit}
	result, err := forecasts(proposal, decimal.NewFromInt(100), "estimated", input)
	if err != nil {
		t.Fatal(err)
	}
	if got := result[1].Reject.Demand.Low; got != "20.000000" {
		t.Fatalf("prior uncertainty did not affect the conservative band: low=%s", got)
	}
	if result[1].Reject.Demand.Unit != "normalized_demand" || result[1].Reject.Contribution.Unit != equivalentContributionUnit {
		t.Fatalf("forecast units are inconsistent: %#v", result[1].Reject)
	}
}

func TestForecastDoesNotAggregateMixedDemandUnitsWithoutCashConversion(t *testing.T) {
	proposal := domain.ChangeProposal{Status: "pending", ExpectedOld: "1", ProposedValue: "1"}
	input := forecastInputs{currentRatio: decimal.NewFromInt(1), proposedRatio: decimal.NewFromInt(1),
		revenueFactor: decimal.NewFromInt(1), riskCost: decimal.RequireFromString("0.5"), elasticity: decimal.RequireFromString("-0.35"),
		priorUncertainty: decimal.RequireFromString("0.55"), observedUncertainty: decimal.RequireFromString("0.3"),
		metricBasis: equivalentContributionBasis, metricUnit: equivalentContributionUnit}
	profiles := []demandProfile{
		{modelID: "gpt-5.6-sol", unit: "million_tokens", daily: decimal.NewFromInt(1), level: "observed"},
		{modelID: "image-model", unit: "requests", daily: decimal.NewFromInt(1), level: "observed"},
	}
	if _, err := forecastsForProfiles(proposal, profiles, "observed", input, DefaultConfig()); err == nil ||
		!strings.Contains(err.Error(), "不能把 token、请求或媒体计量直接相加") {
		t.Fatalf("mixed demand units were accepted without conversion: %v", err)
	}

	input.metricBasis, input.metricUnit = cashContributionBasis, "CNY"
	input.cashRates = map[string]cashUnitRate{
		"gpt-5.6-sol": {revenue: decimal.NewFromInt(2), cost: decimal.NewFromInt(1)},
		"image-model": {revenue: decimal.NewFromInt(4), cost: decimal.NewFromInt(3)},
	}
	result, err := forecastsForProfiles(proposal, profiles, "observed", input, DefaultConfig())
	if err != nil || len(result) == 0 || result[0].Approve.Contribution.Unit != "CNY" || result[0].MetricBasis != cashContributionBasis {
		t.Fatalf("valid per-model cash conversion failed: %#v, %v", result, err)
	}
}

func TestParseForecastInputsRejectsZeroPriceElasticitySingularity(t *testing.T) {
	config := DefaultConfig()
	proposal := domain.ChangeProposal{ExpectedOld: "0.1", ProposedValue: "0", Calculation: domain.PriceCalculation{RiskAdjustedCost: "0.05"}}
	policy := domain.PricingPolicy{RevenueCashFactor: "1"}
	_, err := parseForecastInputs(proposal, policy, config)
	if err == nil || !strings.Contains(err.Error(), "必须大于零") {
		t.Fatalf("zero proposed ratio error = %v", err)
	}
}

func TestGroupModelForecastUsesPerModelContractPricesInsteadOfRevisionStrings(t *testing.T) {
	currentPrice := "1"
	proposal := domain.ChangeProposal{ExpectedOld: "1", ProposedValue: "1", Calculation: domain.PriceCalculation{RiskAdjustedCost: "0"},
		GroupModelPricing: domain.ListingGroupModelPricingPlan{Mode: "atomic_contract", Exact: true, TargetGroup: "video",
			TargetGroupRatio: "1", ExpectedRevision: "sha256-current", DesiredRevision: "sha256-desired",
			ExpectedContract: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "sha256-current",
				Groups: map[string]map[string]domain.GroupModelPricingSetting{"video": {"video-model": {
					BillingMode: "fixed_price", ModelPrice: &currentPrice, TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_request"},
				}}}},
			Models: []domain.ListingModelPricingEvidence{{Model: "video-model", BillingMode: "fixed_price", BillingUnit: "per_request",
				Currency: "USD", TargetGroupRatio: "1", Dimensions: []domain.ListingPriceDimensionEvidence{{Dimension: "per_request",
					EffectiveSale: "1.25", RevenueCashValue: "1.25", RiskAdjustedCashCost: "0.8"}}}},
		}}
	policy := domain.PricingPolicy{RevenueCashFactor: "1"}
	profiles := []demandProfile{{modelID: "video-model", unit: "requests", daily: decimal.NewFromInt(100), level: "observed"}}
	rates, currency, covered, total, coverage, err := groupModelForecastRates(proposal, policy, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if currency != "USD" || covered != 1 || total != 1 || coverage != "1/1" {
		t.Fatalf("coverage=%s currency=%s covered=%d total=%d", coverage, currency, covered, total)
	}
	rate := rates["video-model"]
	if !rate.revenue.Equal(decimal.NewFromInt(1)) || !rate.proposedRevenue.Equal(decimal.RequireFromString("1.25")) ||
		!rate.cost.Equal(decimal.RequireFromString("0.8")) || !rate.priceScale.Equal(decimal.RequireFromString("1.25")) {
		t.Fatalf("rate = %#v", rate)
	}
	input, err := parseForecastInputs(proposal, policy, DefaultConfig())
	if err != nil {
		t.Fatalf("revision leaked into decimal forecast fields: %v", err)
	}
	input.metricBasis, input.metricUnit, input.cashRates = contractCashContributionBasis, currency, rates
	result, err := forecastsForProfiles(proposal, profiles, "observed", input, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	approveDemand := decimal.RequireFromString(result[1].Approve.Demand.Base)
	rejectDemand := decimal.RequireFromString(result[1].Reject.Demand.Base)
	if !approveDemand.LessThan(rejectDemand) || result[1].MetricBasis != contractCashContributionBasis ||
		result[1].Approve.Revenue.Unit != "USD" {
		t.Fatalf("group-model forecast = %#v", result[1])
	}
}

func TestGroupModelForecastRefusesAggregateTokenUsageForNonScalarVector(t *testing.T) {
	modelRatio, completion := "0.5", "2"
	proposal := domain.ChangeProposal{GroupModelPricing: domain.ListingGroupModelPricingPlan{Mode: "atomic_contract", Exact: true,
		TargetGroup: "token", TargetGroupRatio: "1", ExpectedContract: domain.GroupModelPricingContract{SchemaVersion: 1,
			Groups: map[string]map[string]domain.GroupModelPricingSetting{"token": {"gpt": {BillingMode: "ratio",
				ModelRatio: &modelRatio, CompletionRatio: &completion}}}}, Models: []domain.ListingModelPricingEvidence{{Model: "gpt",
			BillingMode: "ratio", BillingUnit: "token", Currency: "USD", Dimensions: []domain.ListingPriceDimensionEvidence{
				{Dimension: "input_per_1m", EffectiveSale: "2", RevenueCashValue: "2", RiskAdjustedCashCost: "1"},
				{Dimension: "output_per_1m", EffectiveSale: "5", RevenueCashValue: "5", RiskAdjustedCashCost: "3"},
			}}}}}
	profiles := []demandProfile{{modelID: "gpt", unit: "million_tokens", daily: decimal.NewFromInt(1)}}
	_, _, covered, total, coverage, err := groupModelForecastRates(proposal, domain.PricingPolicy{RevenueCashFactor: "1"}, profiles)
	if err == nil || covered != 0 || total != 1 || coverage != "0/1" || !strings.Contains(err.Error(), "dimension-level usage") {
		t.Fatalf("token vector coverage=%s covered=%d total=%d err=%v", coverage, covered, total, err)
	}
}

func TestSummarizeObservedRequiresMatchingModelCurrencyAndEvidence(t *testing.T) {
	watermark := map[string]string{"usage_window_complete": "true", "usage_window_id": "window-1", "usage_window_unit": "million_tokens", "usage_window_total": "10"}
	events := []store.OperatingLedgerEvent{
		{ModelID: "gpt-5.6-sol", UsageWeight: "8", UsageUnit: "million_tokens", UsageEvidence: "exact", CashRevenue: "0", ProcurementCost: "0", Currency: "cny", RevenueEvidence: "exact", CostEvidence: "exact", Metadata: watermark},
		{ModelID: "claude-opus", UsageWeight: "100", UsageUnit: "million_tokens", UsageEvidence: "exact", CashRevenue: "100", ProcurementCost: "1", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact"},
		{ModelID: "gpt-5.6-sol", UsageWeight: "2", UsageUnit: "million_tokens", UsageEvidence: "exact", CashRevenue: "100", ProcurementCost: "1", Currency: "USD", RevenueEvidence: "exact", CostEvidence: "exact", Metadata: watermark},
	}
	result, status, _ := summarizeObserved(events, "gpt-*", "CNY", map[string]string{"gpt-5.6-sol": "million_tokens"}, "0.75")
	if status != "observed" || result.EventCount != 1 || result.Contribution != "0.000000" || result.Currency != "CNY" ||
		result.CoverageRatio != "0.800000" {
		t.Fatalf("strict observed summary = %#v, %s", result, status)
	}
}

func TestSummarizeObservedDistinguishesUnknownZeroAndPartialCoverage(t *testing.T) {
	units := map[string]string{"gpt-5.6-sol": "million_tokens"}
	unknown := []store.OperatingLedgerEvent{{ModelID: "gpt-5.6-sol", UsageEvidence: "missing",
		CashRevenue: "0", ProcurementCost: "0", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact"}}
	if _, status, _ := summarizeObserved(unknown, "gpt-*", "CNY", units, "0.95"); status != "coverage_unknown" {
		t.Fatalf("unknown usage denominator status = %s", status)
	}
	apparentlyComplete := []store.OperatingLedgerEvent{{ModelID: "gpt-5.6-sol", UsageWeight: "1", UsageUnit: "million_tokens", UsageEvidence: "exact",
		CashRevenue: "2", ProcurementCost: "1", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact"}}
	if _, status, _ := summarizeObserved(apparentlyComplete, "gpt-*", "CNY", units, "0.95"); status != "coverage_unknown" {
		t.Fatalf("complete imported rows without a source completeness watermark became calibratable: %s", status)
	}

	zero := []store.OperatingLedgerEvent{{ModelID: "gpt-5.6-sol", UsageWeight: "0", UsageUnit: "million_tokens", UsageEvidence: "exact",
		CashRevenue: "0", ProcurementCost: "0", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact",
		Metadata: map[string]string{"usage_window_complete": "true", "usage_window_id": "zero-window", "usage_window_unit": "million_tokens", "usage_window_total": "0"}}}
	observation, status, _ := summarizeObserved(zero, "gpt-*", "CNY", units, "0.95")
	if status != "no_exposure" || observation.EligibleQuantity != "0.000000" || observation.CoverageRatio != "0.000000" {
		t.Fatalf("authoritative zero exposure = %#v, %s", observation, status)
	}

	partial := []store.OperatingLedgerEvent{
		{ModelID: "gpt-5.6-sol", UsageWeight: "5", UsageUnit: "million_tokens", UsageEvidence: "exact",
			CashRevenue: "10", ProcurementCost: "4", Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact",
			Metadata: map[string]string{"usage_window_complete": "true", "usage_window_id": "partial-window", "usage_window_unit": "million_tokens", "usage_window_total": "10"}},
		{ModelID: "gpt-5.6-sol", UsageWeight: "5", UsageUnit: "million_tokens", UsageEvidence: "exact",
			Currency: "CNY", RevenueEvidence: "missing", CostEvidence: "missing",
			Metadata: map[string]string{"usage_window_complete": "true", "usage_window_id": "partial-window", "usage_window_unit": "million_tokens", "usage_window_total": "10"}},
	}
	observation, status, _ = summarizeObserved(partial, "gpt-*", "CNY", units, "0.95")
	if status != "insufficient_coverage" || observation.CoverageRatio != "0.500000" || observation.IncludedQuantity != "5.000000" {
		t.Fatalf("partial ledger coverage = %#v, %s", observation, status)
	}
}

func TestCalibrationUsesExplicitMetricDenominatorsAndUniqueDecisions(t *testing.T) {
	hit, miss := true, false
	result := calculateCalibration([]store.DecisionOutcome{
		{ProposalID: 1, HorizonHours: 1, Status: "observed", AbsoluteError: "1", SignedError: "-1", ActualAbsolute: "10", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
		{ProposalID: 2, HorizonHours: 1, Status: "observed", AbsoluteError: "3", SignedError: "3", ActualAbsolute: "10", AbsolutePctError: "0.3", SymmetricPctError: "0.3", BandHit: &miss, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
	}, 2)
	if !result.Ready || result.SampleCount != 2 || result.DecisionCount != 2 || result.MAPESampleCount != 2 || result.IntervalSamples != 2 {
		t.Fatalf("calibration denominators = %#v", result)
	}
	if result.MAPE != "0.200000" || result.IntervalCoverage != "0.500000" {
		t.Fatalf("calibration metrics = %#v", result)
	}
}

func TestCalibrationSeparatesHorizonsAndKeepsZeroActualMetricsIndependent(t *testing.T) {
	hit, miss := true, false
	items := []store.DecisionOutcome{
		{ProposalID: 1, HorizonHours: 1, Status: "observed", AbsoluteError: "0", SignedError: "0", ActualAbsolute: "0", SymmetricPctError: "0", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
		{ProposalID: 2, HorizonHours: 1, Status: "observed", AbsoluteError: "1", SignedError: "1", ActualAbsolute: "10", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &miss, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
		{ProposalID: 1, HorizonHours: 24, Status: "observed", AbsoluteError: "2", SignedError: "-2", ActualAbsolute: "20", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
		{ProposalID: 2, HorizonHours: 24, Status: "observed", AbsoluteError: "3", SignedError: "3", ActualAbsolute: "30", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &miss, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY"},
		{ProposalID: 3, HorizonHours: 1, Status: "censored"},
		{ProposalID: 4, HorizonHours: 1, Status: "insufficient_coverage"},
	}
	result := calculateCalibration(items, 2)
	if !result.Ready || result.DecisionCount != 2 || len(result.Horizons) != 2 || result.CensoredCount != 1 || result.CoverageExcluded != 1 {
		t.Fatalf("stratified calibration = %#v", result)
	}
	if result.ZeroActualCount != 1 || result.PerfectZeroCount != 1 || result.MAPESampleCount != 2 {
		t.Fatalf("zero-relative-error accounting = %#v", result)
	}
	for _, horizon := range result.Horizons {
		if horizon.DecisionCount != 2 || horizon.SampleCount != 2 {
			t.Fatalf("horizon was pooled or undercounted: %#v", horizon)
		}
		if !horizon.Ready {
			t.Fatalf("an unavailable MAPE denominator blocked otherwise valid horizon calibration: %#v", horizon)
		}
	}
}

func TestCalibrationExcludesOldContractsAndDoesNotMixDimensionalMetrics(t *testing.T) {
	hit := true
	now := time.Now().UTC()
	items := []store.DecisionOutcome{
		{ProposalID: 1, HorizonHours: 24, Status: "observed", AbsoluteError: "1", SignedError: "1", ActualAbsolute: "10", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY", ObservedAt: &now},
		{ProposalID: 2, HorizonHours: 24, Status: "observed", AbsoluteError: "2", SignedError: "2", ActualAbsolute: "20", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY", ObservedAt: &now},
		{ProposalID: 3, HorizonHours: 24, Status: "observed", AbsoluteError: "3", SignedError: "3", ActualAbsolute: "30", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "USD", ObservedAt: &now},
		{ProposalID: 4, HorizonHours: 24, Status: "observed", AbsoluteError: "4", SignedError: "4", ActualAbsolute: "40", AbsolutePctError: "0.1", SymmetricPctError: "0.1", BandHit: &hit, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "USD", ObservedAt: &now},
		{ProposalID: 5, HorizonHours: 1, Status: "observed", AbsoluteError: "999", SignedError: "999", ActualAbsolute: "999", AbsolutePctError: "1", SymmetricPctError: "1", BandHit: &hit, ModelVersion: "phase-a-v2", MetricBasis: cashContributionBasis, MetricUnit: "CNY", ObservedAt: &now},
	}
	result := calculateCalibration(items, 2)
	if !result.Ready || result.IncompatibleExcluded != 1 || result.DecisionCount != 4 || result.SampleCount != 4 || result.TotalOutcomes != 5 {
		t.Fatalf("contract filtering = %#v", result)
	}
	if result.MAE != "" || result.WAPE != "" || result.SignedBias != "" {
		t.Fatalf("cross-currency dimensional metrics were published: %#v", result)
	}
	if result.MAPE != "0.100000" || result.SMAPE != "0.100000" || result.IntervalCoverage != "1.000000" {
		t.Fatalf("dimensionless macro metrics were lost: %#v", result)
	}
}

func TestOutcomeLearningUsesIndependentPastDecisionsAndExpandsResidualBand(t *testing.T) {
	asOf := time.Now().UTC().Truncate(time.Second)
	forecast := HorizonForecast{Hours: 24, LearningScope: "scope-a", MetricBasis: cashContributionBasis, RelativeErrorFloor: "0.01",
		Approve: ScenarioEstimate{Contribution: Interval{Low: "8", Base: "10", High: "12", Unit: "CNY"}}}
	makeOutcome := func(proposalID int64, dueOffset, observedOffset time.Duration, status, actual string) store.DecisionOutcome {
		due, observed := asOf.Add(dueOffset), asOf.Add(observedOffset)
		prediction, _ := json.Marshal(forecast)
		observation, _ := json.Marshal(ObservedOutcome{Contribution: actual, Currency: "CNY", Unit: "CNY"})
		return store.DecisionOutcome{ProposalID: proposalID, HorizonHours: 24, DueAt: due, Status: status,
			Prediction: prediction, Observation: observation, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis,
			MetricUnit: "CNY", ObservedAt: &observed}
	}
	outcomes := []store.DecisionOutcome{
		makeOutcome(1, -72*time.Hour, -70*time.Hour, "observed", "100"),
		makeOutcome(1, -72*time.Hour, -69*time.Hour, "observed", "20"),
		makeOutcome(2, -48*time.Hour, -46*time.Hour, "observed", "20"),
		makeOutcome(3, -24*time.Hour, -22*time.Hour, "observed", "20"),
		makeOutcome(4, 0, time.Hour, "observed", "1000"),
		makeOutcome(5, -24*time.Hour, -22*time.Hour, "censored", "1000"),
	}
	forecasts := []HorizonForecast{forecast}
	digest := calibrateOutcomeForecasts(forecasts, outcomes, 3, asOf)
	learned := forecasts[0]
	if digest == "" || learned.Learning == nil || !learned.Learning.Active || learned.Learning.DistinctProposals != 3 ||
		learned.Learning.IndependentWindows != 3 || learned.Learning.Causal {
		t.Fatalf("learning activation = %#v, digest=%q", learned.Learning, digest)
	}
	if learned.Learning.BiasCorrection != "2.500000" || learned.Learning.MeanAbsoluteRelativeError != "0.500000" ||
		learned.Learning.RelativeResidualP80 != "0.500000" || learned.Learning.ProjectionScale != "10.000000" {
		t.Fatalf("robust residual statistics = %#v", learned.Learning)
	}
	if learned.AdaptiveContribution == nil || learned.AdaptiveContribution.Low != "7.500000" ||
		learned.AdaptiveContribution.Base != "12.500000" || learned.AdaptiveContribution.High != "17.500000" {
		t.Fatalf("adaptive projection = %#v", learned.AdaptiveContribution)
	}
	if learned.Approve.Contribution.Base != "10" || learned.DeltaContribution.Base != "" {
		t.Fatalf("learning rewrote structural or causal scenario: %#v", learned)
	}
}

func TestOutcomeLearningKeepsPriorBeforeIndependentDecisionThreshold(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	forecast := HorizonForecast{Hours: 24, LearningScope: "scope-a", MetricBasis: cashContributionBasis, RelativeErrorFloor: "0.01",
		Approve: ScenarioEstimate{Contribution: Interval{Low: "8", Base: "10", High: "12", Unit: "CNY"}}}
	prediction, _ := json.Marshal(forecast)
	observation, _ := json.Marshal(ObservedOutcome{Contribution: "20", Currency: "CNY", Unit: "CNY"})
	outcomes := []store.DecisionOutcome{}
	for proposalID, offset := range []time.Duration{-48 * time.Hour, -24 * time.Hour} {
		due, observed := now.Add(offset), now.Add(offset+time.Hour)
		outcomes = append(outcomes, store.DecisionOutcome{ProposalID: int64(proposalID + 1), HorizonHours: 24, DueAt: due,
			Status: "observed", Prediction: prediction, Observation: observation, ModelVersion: ModelVersion,
			MetricBasis: cashContributionBasis, MetricUnit: "CNY", ObservedAt: &observed})
	}
	forecasts := []HorizonForecast{forecast}
	calibrateOutcomeForecasts(forecasts, outcomes, 3, now)
	if forecasts[0].Learning == nil || forecasts[0].Learning.Active || forecasts[0].AdaptiveContribution != nil || forecasts[0].Calibrated {
		t.Fatalf("underpowered learning changed the forecast: %#v", forecasts[0])
	}
}

func TestOutcomeLearningRejectsPrematureOrUnitMismatchedRows(t *testing.T) {
	now := time.Date(2026, 7, 23, 6, 0, 0, 0, time.UTC)
	forecast := HorizonForecast{Hours: 24, LearningScope: "scope-a", MetricBasis: cashContributionBasis, RelativeErrorFloor: "0.01",
		Approve: ScenarioEstimate{Contribution: Interval{Low: "8", Base: "10", High: "12", Unit: "CNY"}}}
	prediction, _ := json.Marshal(forecast)
	validObservation, _ := json.Marshal(ObservedOutcome{Contribution: "11", Currency: "CNY", Unit: "CNY"})
	wrongUnitObservation, _ := json.Marshal(ObservedOutcome{Contribution: "11", Currency: "USD", Unit: "USD"})
	makeOutcome := func(id int64, due, observed time.Time, observation json.RawMessage) store.DecisionOutcome {
		return store.DecisionOutcome{ProposalID: id, HorizonHours: 24, DueAt: due, Status: "observed", Prediction: prediction,
			Observation: observation, ModelVersion: ModelVersion, MetricBasis: cashContributionBasis, MetricUnit: "CNY", ObservedAt: &observed}
	}
	outcomes := []store.DecisionOutcome{
		makeOutcome(1, now.Add(-72*time.Hour), now.Add(-71*time.Hour), validObservation),
		makeOutcome(2, now.Add(-48*time.Hour), now.Add(-47*time.Hour), validObservation),
		makeOutcome(3, now.Add(-24*time.Hour), now.Add(-25*time.Hour), validObservation),
		makeOutcome(4, now.Add(-24*time.Hour), now.Add(-23*time.Hour), wrongUnitObservation),
		makeOutcome(5, now.Add(time.Hour), now, validObservation),
	}
	forecasts := []HorizonForecast{forecast}
	calibrateOutcomeForecasts(forecasts, outcomes, 3, now)
	if forecasts[0].Learning == nil || forecasts[0].Learning.DistinctProposals != 2 || forecasts[0].Learning.Active || forecasts[0].Calibrated {
		t.Fatalf("premature or incompatible evidence entered learning: %#v", forecasts[0].Learning)
	}
}

func TestRecommendationHashIncludesEveryHotConfigFieldAndModelVersion(t *testing.T) {
	proposal := domain.ChangeProposal{ID: 7, ExpectedOld: "0.1", ProposedValue: "0.2"}
	policy := domain.PricingPolicy{ID: 3, SourceModelPattern: "gpt-*"}
	base := DefaultConfig()
	first := recommendationHash(proposal, policy, base, nil)
	base.PriorUncertainty = "0.9"
	second := recommendationHash(proposal, policy, base, nil)
	base.ForecastCurrency = "CNY"
	third := recommendationHash(proposal, policy, base, nil)
	if first == second || second == third || first == third {
		t.Fatalf("hot-config changes did not invalidate the input hash: %s %s %s", first, second, third)
	}
}

func TestRecommendationHashUsesOnlyCanonicalEffectiveUsage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	proposal := domain.ChangeProposal{ID: 8, ExpectedOld: "0.1", ProposedValue: "0.2"}
	policy := domain.PricingPolicy{ID: 4, SourceModelPattern: "gpt-*"}
	relevant := store.OpportunityUsage{ModelID: "gpt-5.6-sol", WindowDays: 7, DemandWeight: "70", Confidence: "0.95", ObservedAt: now}
	stale := relevant
	stale.DemandWeight = "700"
	stale.ObservedAt = now.Add(-time.Hour)
	irrelevant := store.OpportunityUsage{ModelID: "claude-opus", WindowDays: 7, DemandWeight: "900", Confidence: "0.95", ObservedAt: now}
	withNoise := recommendationHash(proposal, policy, DefaultConfig(), []store.OpportunityUsage{irrelevant, stale, relevant})
	canonical := recommendationHash(proposal, policy, DefaultConfig(), []store.OpportunityUsage{relevant})
	if withNoise != canonical {
		t.Fatalf("irrelevant or stale usage invalidated the forecast hash: %s != %s", withNoise, canonical)
	}
}

func TestRestoreFrozenRecommendationKeepsDecisionTimeInputs(t *testing.T) {
	decidedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	frozen := Recommendation{ProposalID: 9, ForecastReady: true, DecidedAt: &decidedAt, InputHash: "frozen-hash", ModelVersion: ModelVersion,
		ModelPattern: "gpt-*",
		Forecasts: []HorizonForecast{{Hours: 24, MetricBasis: cashContributionBasis,
			Approve: ScenarioEstimate{Contribution: Interval{P50: "1", Unit: "CNY"}}}}}
	payload, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	item := Recommendation{ProposalID: 9, InputHash: "new-hash", ModelVersion: ModelVersion}
	err = restoreFrozenRecommendation(&item, store.DecisionSnapshot{ProposalID: 9, InputHash: "frozen-hash", ModelVersion: ModelVersion, DecidedAt: decidedAt, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if item.InputHash != "frozen-hash" || item.DecidedAt == nil || !item.DecidedAt.Equal(decidedAt) ||
		len(item.Forecasts) != 1 || item.Forecasts[0].Approve.Contribution.Base != "1" {
		t.Fatalf("frozen decision contract was not restored: %#v", item)
	}
}

func TestRestoreCurrentSnapshotPreservesAdaptiveLearningContract(t *testing.T) {
	decidedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	adaptive := Interval{Low: "7", Base: "11", High: "15", Unit: "CNY"}
	frozen := Recommendation{ProposalID: 11, ForecastReady: true, InputHash: "adaptive-hash", ModelVersion: ModelVersion,
		ModelPattern: "gpt-*", DecisionKind: "approve", Forecasts: []HorizonForecast{{Hours: 24, Calibrated: true,
			IntervalMethod: "structural_plus_scope_relative_residual_band_v2", AdaptiveContribution: &adaptive,
			Learning: &LearningAdjustment{Active: true, IndependentWindows: 3, Method: "scope_isolated_nonoverlap_relative_residual_v2"}}}}
	payload, _ := json.Marshal(frozen)
	item := Recommendation{ProposalID: frozen.ProposalID}
	err := restoreFrozenRecommendation(&item, store.DecisionSnapshot{ProposalID: frozen.ProposalID, InputHash: frozen.InputHash,
		ModelVersion: ModelVersion, DecisionKind: "approve", DecidedAt: decidedAt, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Forecasts) != 1 || !item.Forecasts[0].Calibrated || item.Forecasts[0].AdaptiveContribution == nil ||
		item.Forecasts[0].Learning == nil || !item.Forecasts[0].Learning.Active || item.ModelPattern != "gpt-*" {
		t.Fatalf("current adaptive snapshot was downgraded during restore: %#v", item)
	}
}

func TestRestoreLegacySnapshotDropsIncompatibleObservation(t *testing.T) {
	frozen := Recommendation{ProposalID: 10, ForecastReady: true, Forecasts: []HorizonForecast{{Hours: 24,
		OutcomeStatus: "observed", Observation: &ObservedOutcome{Contribution: "1", Currency: "CNY"}}}}
	payload, _ := json.Marshal(frozen)
	item := Recommendation{ProposalID: 10}
	if err := restoreFrozenRecommendation(&item, store.DecisionSnapshot{ProposalID: 10, InputHash: "legacy", ModelVersion: "phase-a-v1", DecidedAt: time.Now().UTC(), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if item.Forecasts[0].MetricBasis != equivalentContributionBasis || item.Forecasts[0].Observation != nil || item.Forecasts[0].OutcomeStatus == "observed" {
		t.Fatalf("legacy mixed-unit observation remained active: %#v", item.Forecasts[0])
	}
}

func TestValidateConfigRejectsOptimisticPositiveElasticity(t *testing.T) {
	config := DefaultConfig()
	config.PriceElasticity = "0.2"
	if err := ValidateConfig(config); err == nil {
		t.Fatal("positive elasticity should require an explicit future model contract")
	}
}

func TestValidateConfigNormalizesAndValidatesForecastCurrency(t *testing.T) {
	config := NormalizeConfig(Config{ForecastCurrency: " cny "})
	if config.ForecastCurrency != "CNY" || ValidateConfig(config) != nil {
		t.Fatalf("currency normalization = %#v", config)
	}
	config.ForecastCurrency = "RMB1"
	if ValidateConfig(config) == nil {
		t.Fatal("invalid currency code was accepted")
	}
}
