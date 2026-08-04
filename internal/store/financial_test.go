package store

import (
	"context"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOperatingFinancialSummarySeparatesBasesAndAppliesExclusions(t *testing.T) {
	ctx := context.Background()
	st, targetID := financialTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	base := func(id, kind string) OperatingLedgerEvent {
		return OperatingLedgerEvent{EventID: id, TargetSiteID: targetID, OccurredAt: now, EventKind: kind,
			AmountEvidence: "missing", UsageEvidence: "missing", RevenueEvidence: "missing", CostEvidence: "missing",
			Source: "newapi-ledger", SubjectUserID: "user-1", SubjectRole: "user"}
	}
	usage := func(id, segment, currency, revenue, cost string) OperatingLedgerEvent {
		event := base(id, "usage")
		event.UserSegment, event.Currency, event.CashRevenue, event.ProcurementCost, event.Requests = segment, currency, revenue, cost, 1
		event.RevenueEvidence = "exact"
		if cost != "" {
			event.CostEvidence = "exact"
		}
		return event
	}
	amount := func(id, kind, currency, value string) OperatingLedgerEvent {
		event := base(id, kind)
		event.Currency, event.Amount = currency, value
		if value != "" {
			event.AmountEvidence = "exact"
		}
		return event
	}
	events := []OperatingLedgerEvent{
		usage("usage-complete", "retail", "CNY", "50", "20"),
		usage("usage-cost-unknown", "retail", "CNY", "10", ""),
		amount("recharge", "recharge", "CNY", "100"),
		amount("refund", "refund", "CNY", "10"),
		amount("fee-unknown", "payment_fee", "CNY", ""),
		amount("gift", "gift_credit", "CNY", "5"),
		amount("usd-recharge", "recharge", "USD", "20"),
	}
	admin := amount("admin-recharge", "recharge", "CNY", "999")
	admin.SubjectRole = "administrator"
	events = append(events, admin)
	testUsage := usage("test-usage", "tester", "USD", "100", "1")
	testUsage.IsTest = true
	events = append(events, testUsage)
	internal := usage("internal-usage", "ops", "CNY", "100", "1")
	internal.IsInternal = true
	events = append(events, internal)
	bot := usage("bot-usage", "automation", "CNY", "100", "1")
	events = append(events, bot)
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	report, err := st.OperatingFinancialSummary(ctx, LedgerFinancialReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour),
		Exclusions: LedgerExclusionPolicy{ExcludeAdministrators: true, ExcludeTestUsers: true, ExcludeInternalTokens: true,
			ExcludedUserSegments: []string{"automation"}}})
	if err != nil {
		t.Fatal(err)
	}
	if report.RawEventCount != 11 || report.IncludedEventCount != 7 || report.IncludedUsageEventCount != 2 ||
		report.MatchedUsageEventCount != 1 || report.ExcludedEventCount != 4 {
		t.Fatalf("event accounting = %#v", report)
	}
	for _, reason := range []string{"administrator", "test_user", "internal_token", "user_segment"} {
		if report.ExcludedByReason[reason] != 1 {
			t.Fatalf("excluded reasons = %#v", report.ExcludedByReason)
		}
	}
	if report.CrossCurrencyAggregate || report.UnknownAmountEvents != 1 || report.Coverage.Status != "unknown" ||
		report.UnknownUsageRevenueEvents != 0 || report.UnknownUsageCostEvents != 1 || report.UnknownCashFlowAmountEvents != 1 {
		t.Fatalf("honesty flags = %#v", report)
	}
	cny := findCurrencySummary(t, report.ByCurrency, "CNY")
	if cny.UsageRevenue != "60.000000" || cny.UsageProcurementCost != "20.000000" || cny.UsageProfitAvailable ||
		cny.MatchedUsageEventCount != 1 || cny.MatchedUsageRevenue != "50.000000" || cny.MatchedUsageProcurementCost != "20.000000" ||
		cny.RechargeGross != "100.000000" || cny.Refunds != "10.000000" || cny.GiftCreditIssued != "5.000000" ||
		cny.CashNetFlowAvailable || cny.UnknownCashFlowAmountEvents != 1 {
		t.Fatalf("CNY summary = %#v", cny)
	}
	usd := findCurrencySummary(t, report.ByCurrency, "USD")
	if !usd.CashNetFlowAvailable || usd.CashNetFlow != "20.000000" {
		t.Fatalf("USD summary = %#v", usd)
	}
	segment := findSegmentSummary(t, report.UserSegments, "retail", "CNY", true)
	if segment.EventCount != 2 || segment.Requests != 2 || segment.RevenueCoverage != "1.000000" || segment.CostCoverage != "0.500000" ||
		segment.ContributionAvailable || len(segment.RequiresLedgerFields) != 1 || segment.RequiresLedgerFields[0] != "procurement_cost" {
		t.Fatalf("retail segment = %#v", segment)
	}
}

func TestOperatingFinancialSummarySeparatesCommissionCostFromProcurement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, targetID := financialTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	event := func(id, kind, amount string, metadata map[string]string) OperatingLedgerEvent {
		return OperatingLedgerEvent{EventID: id, TargetSiteID: targetID, OccurredAt: now, EventKind: kind,
			Amount: amount, AmountEvidence: "exact", Currency: "CNY", UsageEvidence: "missing",
			RevenueEvidence: "missing", CostEvidence: "missing", Source: "newapi", SubjectUserID: "user-1", Metadata: metadata}
	}
	events := []OperatingLedgerEvent{
		event("commission-award", "other_expense", "2", map[string]string{"cost_kind": "consumption_commission"}),
		event("commission-reversal", "other_income", "0.5", map[string]string{"cost_kind": "consumption_commission"}),
		event("unrelated-expense", "other_expense", "7", map[string]string{"cost_kind": "support_credit"}),
		event("procurement-payment", "procurement_cost", "3", nil),
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	report, err := st.OperatingFinancialSummary(ctx, LedgerFinancialReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cny := findCurrencySummary(t, report.ByCurrency, "CNY")
	if cny.CommissionAwardEvents != 1 || cny.CommissionReversalEvents != 1 ||
		cny.CommissionExpense != "2.000000" || cny.CommissionReversal != "0.500000" ||
		cny.OtherExpense != "9.000000" || cny.OtherIncome != "0.500000" ||
		cny.ProcurementPayments != "3.000000" || !cny.CashNetFlowAvailable || cny.CashNetFlow != "-11.500000" {
		t.Fatalf("commission/procurement separation = %#v", cny)
	}
}

func TestOperatingFinancialSummaryVerifiesExactSourceWatermark(t *testing.T) {
	ctx := context.Background()
	st, targetID := financialTestStore(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	metadata := map[string]string{"financial_watermark_id": "newapi-day-20260701", "financial_window_start": start.Format(time.RFC3339Nano),
		"financial_window_end": end.Format(time.RFC3339Nano), "financial_authoritative_event_count": "2"}
	events := []OperatingLedgerEvent{
		{EventID: "wm-1", TargetSiteID: targetID, OccurredAt: start.Add(time.Hour), EventKind: "recharge", Amount: "10", AmountEvidence: "exact",
			Currency: "CNY", RevenueEvidence: "missing", CostEvidence: "missing", UsageEvidence: "missing", Source: "newapi", SubjectUserID: "1", Metadata: metadata},
		{EventID: "wm-2", TargetSiteID: targetID, OccurredAt: start.Add(2 * time.Hour), EventKind: "refund", Amount: "1", AmountEvidence: "exact",
			Currency: "CNY", RevenueEvidence: "missing", CostEvidence: "missing", UsageEvidence: "missing", Source: "newapi", SubjectUserID: "1"},
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	report, err := st.OperatingFinancialSummary(ctx, LedgerFinancialReportOptions{Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if report.Coverage.Status != "verified" || report.Coverage.VerifiedCohorts != 1 || report.Coverage.Cohorts != 1 {
		t.Fatalf("coverage = %#v", report.Coverage)
	}
}

func TestOperatingFinancialSummaryCountsMissingFieldsWithoutCurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, targetID := financialTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	event := OperatingLedgerEvent{EventID: "usage-no-currency", TargetSiteID: targetID, OccurredAt: now,
		EventKind: "usage", AmountEvidence: "missing", UsageEvidence: "observed", RevenueEvidence: "missing",
		CostEvidence: "missing", Source: "newapi-ledger", SubjectUserID: "user-1", SubjectRole: "user",
		UsageWeight: "1", UsageUnit: "request", Requests: 1}
	if err := st.UpsertOperatingLedgerEvents(ctx, []OperatingLedgerEvent{event}); err != nil {
		t.Fatal(err)
	}
	report, err := st.OperatingFinancialSummary(ctx, LedgerFinancialReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if report.RawEventCount != 1 || report.IncludedEventCount != 1 || report.IncludedUsageEventCount != 1 ||
		report.UnknownCurrencyEvents != 1 || report.UnknownUsageCurrencyEvents != 1 ||
		report.UnknownUsageRevenueEvents != 1 || report.UnknownUsageCostEvents != 1 || report.CrossCurrencyAggregate {
		t.Fatalf("currencyless missing usage fields = %#v", report)
	}
}

func TestOperatingFinancialSummarySeparatesModelsAndPricingContractRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, targetID := financialTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	usage := func(id, group, model, revision, revenue, cost string) OperatingLedgerEvent {
		event := OperatingLedgerEvent{EventID: id, TargetSiteID: targetID, OccurredAt: now, EventKind: "usage",
			AmountEvidence: "missing", UsageEvidence: "observed", RevenueEvidence: "observed", CostEvidence: "observed",
			Source: "newapi-ledger", SubjectUserID: "user-1", SubjectRole: "user", GroupName: group, ModelID: model,
			CashRevenue: revenue, ProcurementCost: cost, Currency: "CNY", Requests: 1, Metadata: map[string]string{
				"pricing_scope": "group_model", "pricing_revision": revision, "billing_mode": "fixed_price", "task_billing_unit": "per_second"}}
		if cost == "" {
			event.CostEvidence = "missing"
		}
		return event
	}
	events := []OperatingLedgerEvent{
		usage("contract-a-1", "video", "jimeng", "rev-a", "2", "1"),
		usage("contract-a-2", "video", "jimeng", "rev-a", "4", "2"),
		usage("contract-b", "video", "jimeng", "rev-b", "3", "2"),
		usage("contract-other-group", "video-plus", "jimeng", "rev-a", "1", "0.5"),
		usage("contract-cost-missing", "video", "unknown-cost", "rev-c", "1", ""),
	}
	fixed := usage("contract-fixed-duration", "video", "fixed-video", "rev-fixed", "2", "1")
	fixed.Metadata["task_billing_unit"] = "fixed_duration"
	fixed.Metadata["task_fixed_duration"] = "15"
	events = append(events, fixed)
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	report, err := st.OperatingFinancialSummary(ctx, LedgerFinancialReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	model := findDimensionSummary(t, report.Models, "jimeng", "CNY")
	if model.EventCount != 4 || model.KnownRevenue != "10.000000" || model.KnownCost != "5.500000" ||
		!model.ProfitAvailable || model.GrossProfit != "4.500000" || model.GrossMargin != "0.450000" {
		t.Fatalf("model summary = %#v", model)
	}
	contract := findPricingContractSummary(t, report.PricingContracts, "video", "jimeng", "rev-a", "CNY")
	if contract.EventCount != 2 || contract.Requests != 2 || contract.KnownRevenue != "6.000000" || contract.KnownCost != "3.000000" ||
		contract.GrossProfit != "3.000000" || contract.GrossMargin != "0.500000" || !contract.ProfitAvailable ||
		contract.BillingUnit != "per_second" || contract.ContractEvidenceCoverage != "1.000000" {
		t.Fatalf("contract summary = %#v", contract)
	}
	if other := findPricingContractSummary(t, report.PricingContracts, "video-plus", "jimeng", "rev-a", "CNY"); other.EventCount != 1 {
		t.Fatalf("same revision from another group was merged: %#v", other)
	}
	missing := findPricingContractSummary(t, report.PricingContracts, "video", "unknown-cost", "rev-c", "CNY")
	if missing.ProfitAvailable || missing.CostCoverage != "0.000000" || !containsString(missing.RequiresLedgerFields, "procurement_cost") {
		t.Fatalf("unknown contract cost = %#v", missing)
	}
	if fixedSummary := findPricingContractSummary(t, report.PricingContracts, "video", "fixed-video", "rev-fixed", "CNY"); fixedSummary.BillingUnit != "fixed_duration" || fixedSummary.FixedDurationSeconds != "15" || fixedSummary.ContractEvidenceCoverage != "1.000000" {
		t.Fatalf("fixed-duration contract = %#v", fixedSummary)
	}
}

func financialTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "financial-target", Kind: "access-token", SiteURL: "https://financial.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "financial-target", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	return st, target.ID
}

func findCurrencySummary(t *testing.T, items []LedgerCurrencySummary, currency string) LedgerCurrencySummary {
	t.Helper()
	for _, item := range items {
		if item.Currency == currency {
			return item
		}
	}
	t.Fatalf("currency %s not found in %#v", currency, items)
	return LedgerCurrencySummary{}
}

func findSegmentSummary(t *testing.T, items []LedgerSegmentSummary, segment, currency string, included bool) LedgerSegmentSummary {
	t.Helper()
	for _, item := range items {
		if item.UserSegment == segment && item.Currency == currency && item.Included == included {
			return item
		}
	}
	t.Fatalf("segment %s/%s/%v not found in %#v", segment, currency, included, items)
	return LedgerSegmentSummary{}
}

func findDimensionSummary(t *testing.T, items []LedgerDimensionSummary, label, currency string) LedgerDimensionSummary {
	t.Helper()
	for _, item := range items {
		if item.Label == label && item.Currency == currency {
			return item
		}
	}
	t.Fatalf("dimension %s/%s not found in %#v", label, currency, items)
	return LedgerDimensionSummary{}
}

func findPricingContractSummary(t *testing.T, items []LedgerPricingContractSummary, group, model, revision, currency string) LedgerPricingContractSummary {
	t.Helper()
	for _, item := range items {
		if item.GroupName == group && item.ModelID == model && item.PricingRevision == revision && item.Currency == currency {
			return item
		}
	}
	t.Fatalf("pricing contract %s/%s/%s/%s not found in %#v", group, model, revision, currency, items)
	return LedgerPricingContractSummary{}
}

func containsString(items []string, expected string) bool {
	for _, item := range items {
		if item == expected {
			return true
		}
	}
	return false
}
