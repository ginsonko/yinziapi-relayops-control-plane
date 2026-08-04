package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// LedgerEventExclusionReasons applies explicit subject evidence only. Missing
// subject metadata is never guessed to be an ordinary user or administrator.
func LedgerEventExclusionReasons(event OperatingLedgerEvent, policy LedgerExclusionPolicy) []string {
	reasons := make([]string, 0, 4)
	role := strings.ToLower(strings.TrimSpace(event.SubjectRole))
	if policy.ExcludeAdministrators && (role == "admin" || role == "administrator" || role == "owner" || role == "site_owner") {
		reasons = append(reasons, "administrator")
	}
	if policy.ExcludeTestUsers && event.IsTest {
		reasons = append(reasons, "test_user")
	}
	if policy.ExcludeInternalTokens && event.IsInternal {
		reasons = append(reasons, "internal_token")
	}
	if containsFold(policy.ExcludedUserSegments, event.UserSegment) {
		reasons = append(reasons, "user_segment")
	}
	if containsFold(policy.ExcludedUserIDs, event.SubjectUserID) {
		reasons = append(reasons, "user_id")
	}
	if containsFold(policy.ExcludedTokenIDs, event.SubjectTokenID) {
		reasons = append(reasons, "token_id")
	}
	sort.Strings(reasons)
	return reasons
}

func containsFold(values []string, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), candidate) {
			return true
		}
	}
	return false
}

func FilterOperatingLedgerEvents(events []OperatingLedgerEvent, policy LedgerExclusionPolicy) []OperatingLedgerEvent {
	result := make([]OperatingLedgerEvent, 0, len(events))
	for _, event := range events {
		if len(LedgerEventExclusionReasons(event, policy)) == 0 {
			result = append(result, event)
		}
	}
	return result
}

type ledgerCurrencyAccumulator struct {
	usageRevenue, usageCost, matchedRevenue, matchedCost decimal.Decimal
	recharge, refunds, fees, procurement                 decimal.Decimal
	gifts, otherIncome, otherExpense                     decimal.Decimal
	commissionExpense, commissionReversal                decimal.Decimal
	usageEvents, matchedUsageEvents, cashEvents          int
	commissionAwardEvents, commissionReversalEvents      int
	unknownUsageRevenue, unknownUsageCost, unknownCash   int
	evidenceRank                                         int
}

type ledgerSegmentAccumulator struct {
	segment, currency string
	included          bool
	reasons           []string
	events            int
	requests          int64
	revenueKnown      int
	costKnown         int
	revenue, cost     decimal.Decimal
}

type ledgerDimensionAccumulator struct {
	key, label, currency string
	events               int
	requests             int64
	revenueKnown         int
	costKnown            int
	revenue, cost        decimal.Decimal
}

type ledgerPricingContractAccumulator struct {
	key, group, model, scope, revision, mode, unit, fixedDuration, currency string
	events, revenueKnown, costKnown, contractEvidenceKnown                  int
	requests                                                                int64
	revenue, cost                                                           decimal.Decimal
	requires                                                                map[string]struct{}
}

type coverageEvent struct {
	targetID   int64
	source     string
	occurredAt time.Time
}

type financialWatermark struct {
	id, source    string
	targetID      int64
	start, end    time.Time
	authoritative int
	conflict      bool
}

// OperatingFinancialSummary reports the imported ledger without pretending it
// is a complete source-system statement. Verification requires an explicit,
// matching source watermark for every target/source cohort in the window.
func (s *Store) OperatingFinancialSummary(ctx context.Context, options LedgerFinancialReportOptions) (LedgerFinancialSummary, error) {
	start, end := options.Start.UTC(), options.End.UTC()
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return LedgerFinancialSummary{}, fmt.Errorf("financial report requires a valid start and end window")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.event_id,e.target_site_id,e.occurred_at,e.event_kind,e.amount,e.amount_evidence,e.model_id,e.group_name,
		COALESCE(NULLIF(e.user_segment,''),p.user_segment,''),e.subject_user_id,e.subject_token_id,
		COALESCE(NULLIF(e.subject_role,''),p.role,''),CASE WHEN e.is_test=1 OR COALESCE(p.is_test,0)=1 THEN 1 ELSE 0 END,
		CASE WHEN e.is_internal=1 OR COALESCE(p.is_internal,0)=1 THEN 1 ELSE 0 END,e.related_event_id,e.requests,e.usage_weight,e.usage_unit,
		e.usage_evidence,e.cash_revenue,e.procurement_cost,e.currency,e.revenue_evidence,e.cost_evidence,e.source,e.evidence_revision,
		e.metadata_json
		FROM operating_ledger_events e LEFT JOIN target_ledger_user_profiles p
		ON p.target_site_id=e.target_site_id AND p.user_id=e.subject_user_id
		WHERE e.occurred_at>=? AND e.occurred_at<? ORDER BY e.occurred_at,e.event_id`, formatTime(start), formatTime(end))
	if err != nil {
		return LedgerFinancialSummary{}, err
	}
	defer rows.Close()
	events := make([]OperatingLedgerEvent, 0)
	for rows.Next() {
		event, scanErr := scanFinancialLedgerEvent(rows)
		if scanErr != nil {
			return LedgerFinancialSummary{}, scanErr
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return LedgerFinancialSummary{}, err
	}
	return SummarizeOperatingFinancialEvents(events, options)
}

// SummarizeOperatingFinancialEvents is the database-free financial contract
// used by the bounded intelligence cache. Callers must provide profile-enriched
// events; the report still applies the exact time and exclusion semantics.
func SummarizeOperatingFinancialEvents(events []OperatingLedgerEvent, options LedgerFinancialReportOptions) (LedgerFinancialSummary, error) {
	start, end := options.Start.UTC(), options.End.UTC()
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return LedgerFinancialSummary{}, fmt.Errorf("financial report requires a valid start and end window")
	}

	result := LedgerFinancialSummary{WindowStart: start, WindowEnd: end, ExcludedByReason: map[string]int{},
		ByCurrency: []LedgerCurrencySummary{}, UserSegments: []LedgerSegmentSummary{}, Groups: []LedgerDimensionSummary{},
		Models: []LedgerDimensionSummary{}, PricingContracts: []LedgerPricingContractSummary{}, Warnings: []string{}}
	included := map[string]*ledgerCurrencyAccumulator{}
	excluded := map[string]*ledgerCurrencyAccumulator{}
	segments := map[string]*ledgerSegmentAccumulator{}
	groups := map[string]*ledgerDimensionAccumulator{}
	models := map[string]*ledgerDimensionAccumulator{}
	pricingContracts := map[string]*ledgerPricingContractAccumulator{}
	watermarks := map[string]*financialWatermark{}
	cohortEvents := map[string][]coverageEvent{}
	watermarkMetadataSeen := false

	for _, event := range events {
		if event.OccurredAt.Before(start) || !event.OccurredAt.Before(end) {
			continue
		}
		result.RawEventCount++
		cohortKey := financialCohortKey(event.TargetSiteID, event.Source)
		cohortEvents[cohortKey] = append(cohortEvents[cohortKey], coverageEvent{event.TargetSiteID, event.Source, event.OccurredAt})
		if seen, watermark := parseFinancialWatermark(event); seen {
			watermarkMetadataSeen = true
			key := cohortKey + "\x00" + watermark.id
			if existing, ok := watermarks[key]; ok {
				if existing.start != watermark.start || existing.end != watermark.end || existing.authoritative != watermark.authoritative ||
					existing.source != watermark.source || existing.targetID != watermark.targetID {
					existing.conflict = true
				}
			} else {
				watermarks[key] = &watermark
			}
		}

		if event.SubjectUserID == "" && event.SubjectTokenID == "" && event.SubjectRole == "" && !event.IsTest && !event.IsInternal && event.UserSegment == "" {
			result.UnclassifiedSubjectEvents++
		}
		reasons := LedgerEventExclusionReasons(event, options.Exclusions)
		isExcluded := len(reasons) > 0
		if isExcluded {
			result.ExcludedEventCount++
			for _, reason := range reasons {
				result.ExcludedByReason[reason]++
			}
		} else {
			result.IncludedEventCount++
		}
		currencyMap := included
		if isExcluded {
			currencyMap = excluded
		}
		unknownCurrency, unknownAmount := addFinancialEvent(currencyMap, event)
		if !isExcluded {
			if unknownCurrency {
				result.UnknownCurrencyEvents++
			}
			if unknownAmount {
				result.UnknownAmountEvents++
			}
		}
		if event.EventKind == "usage" {
			addSegmentEvent(segments, event, !isExcluded, reasons)
			if !isExcluded {
				groupName, modelID := ledgerPricingLabels(event)
				addDimensionEvent(groups, "group", groupName, event)
				addDimensionEvent(models, "model", modelID, event)
				addPricingContractEvent(pricingContracts, event, groupName, modelID)
			}
		}
		if !isExcluded {
			switch event.EventKind {
			case "usage":
				result.IncludedUsageEventCount++
				if strings.TrimSpace(event.Currency) == "" {
					result.UnknownUsageCurrencyEvents++
				}
				if _, ok := ledgerAmount(event.CashRevenue, event.RevenueEvidence); !ok {
					result.UnknownUsageRevenueEvents++
				}
				if _, ok := ledgerAmount(event.ProcurementCost, event.CostEvidence); !ok {
					result.UnknownUsageCostEvents++
				}
				_, revenueOK := ledgerAmount(event.CashRevenue, event.RevenueEvidence)
				_, costOK := ledgerAmount(event.ProcurementCost, event.CostEvidence)
				if revenueOK && costOK && strings.TrimSpace(event.Currency) != "" {
					result.MatchedUsageEventCount++
				}
			case "gift_credit":
				// Gift credit is non-cash and does not require a cash-flow amount.
			default:
				if _, ok := ledgerAmount(event.Amount, event.AmountEvidence); !ok {
					result.UnknownCashFlowAmountEvents++
				}
			}
		}
	}
	result.ByCurrency = finalizeCurrencySummaries(included)
	result.ExcludedByCurrency = finalizeCurrencySummaries(excluded)
	result.UserSegments = finalizeSegmentSummaries(segments)
	result.Groups = finalizeDimensionSummaries(groups)
	result.Models = finalizeDimensionSummaries(models)
	result.PricingContracts = finalizePricingContractSummaries(pricingContracts)
	result.CrossCurrencyAggregate = len(result.ByCurrency) == 1 && result.UnknownCurrencyEvents == 0
	for _, count := range result.ExcludedByReason {
		if count > 0 {
			result.ExcludedReasonCountsOverlap = sumReasonCounts(result.ExcludedByReason) > result.ExcludedEventCount
			break
		}
	}
	result.Coverage = summarizeFinancialCoverage(start, end, cohortEvents, watermarks, watermarkMetadataSeen)
	appendFinancialWarnings(&result)
	return result, nil
}

func addDimensionEvent(items map[string]*ledgerDimensionAccumulator, dimension, rawLabel string, event OperatingLedgerEvent) {
	label := strings.TrimSpace(rawLabel)
	if label == "" {
		if strings.EqualFold(strings.TrimSpace(dimension), "model") {
			label = "未标记模型"
		} else {
			label = "未标记分组"
		}
	}
	currency := strings.ToUpper(strings.TrimSpace(event.Currency))
	if currency == "" {
		currency = "UNKNOWN"
	}
	key := strings.ToLower(dimension) + "\x00" + strings.ToLower(label) + "\x00" + currency
	item := items[key]
	if item == nil {
		item = &ledgerDimensionAccumulator{key: dimension + ":" + label, label: label, currency: currency}
		items[key] = item
	}
	item.events++
	item.requests += event.Requests
	if value, ok := ledgerAmount(event.CashRevenue, event.RevenueEvidence); ok {
		item.revenueKnown++
		item.revenue = item.revenue.Add(value)
	}
	if value, ok := ledgerAmount(event.ProcurementCost, event.CostEvidence); ok {
		item.costKnown++
		item.cost = item.cost.Add(value)
	}
}

func ledgerPricingLabels(event OperatingLedgerEvent) (group, model string) {
	group = strings.TrimSpace(event.GroupName)
	model = strings.TrimSpace(event.ModelID)
	if group == "" {
		group = strings.TrimSpace(event.Metadata["pricing_group"])
	}
	if model == "" {
		model = strings.TrimSpace(event.Metadata["pricing_model"])
	}
	return group, model
}

func addPricingContractEvent(items map[string]*ledgerPricingContractAccumulator, event OperatingLedgerEvent, rawGroup, rawModel string) {
	group, model := strings.TrimSpace(rawGroup), strings.TrimSpace(rawModel)
	scope := strings.ToLower(strings.TrimSpace(event.Metadata["pricing_scope"]))
	revision := strings.TrimSpace(event.Metadata["pricing_revision"])
	mode := strings.ToLower(strings.TrimSpace(event.Metadata["billing_mode"]))
	unit := strings.ToLower(strings.TrimSpace(event.Metadata["task_billing_unit"]))
	if unit == "" {
		unit = strings.ToLower(strings.TrimSpace(event.Metadata["billing_unit"]))
	}
	if unit == "" && (mode == "ratio" || mode == "tiered") {
		unit = "token_vector"
	}
	fixedDuration := strings.TrimSpace(event.Metadata["task_fixed_duration"])
	identityKnown := group != "" && model != "" && scope != "" && revision != "" && (mode != "" || unit != "") &&
		(unit != "fixed_duration" || fixedDuration != "")
	requires := make([]string, 0, 5)
	if group == "" {
		group, requires = "未标记分组", append(requires, "group_name")
	}
	if model == "" {
		model, requires = "未标记模型", append(requires, "model_id")
	}
	if scope == "" {
		scope, requires = "unknown", append(requires, "pricing_scope")
	}
	if revision == "" {
		requires = append(requires, "pricing_revision")
	}
	if mode == "" {
		mode = "unknown"
	}
	if unit == "" {
		unit, requires = "unknown", append(requires, "billing_unit")
	}
	if unit == "fixed_duration" && fixedDuration == "" {
		requires = append(requires, "fixed_duration_seconds")
	}
	currency := strings.ToUpper(strings.TrimSpace(event.Currency))
	if currency == "" {
		currency, requires = "UNKNOWN", append(requires, "currency")
	}
	mapKey := strings.Join([]string{strings.ToLower(group), strings.ToLower(model), scope, revision, mode, unit, fixedDuration, currency}, "\x00")
	item := items[mapKey]
	if item == nil {
		item = &ledgerPricingContractAccumulator{key: "contract:" + strings.Join([]string{group, model, scope, revision, mode, unit, fixedDuration, currency}, "|"),
			group: group, model: model, scope: scope, revision: revision, mode: mode, unit: unit, fixedDuration: fixedDuration, currency: currency,
			requires: map[string]struct{}{}}
		items[mapKey] = item
	}
	for _, field := range requires {
		item.requires[field] = struct{}{}
	}
	item.events++
	item.requests += event.Requests
	if identityKnown {
		item.contractEvidenceKnown++
	}
	if value, ok := ledgerAmount(event.CashRevenue, event.RevenueEvidence); ok {
		item.revenueKnown++
		item.revenue = item.revenue.Add(value)
	}
	if value, ok := ledgerAmount(event.ProcurementCost, event.CostEvidence); ok {
		item.costKnown++
		item.cost = item.cost.Add(value)
	}
}

func finalizePricingContractSummaries(items map[string]*ledgerPricingContractAccumulator) []LedgerPricingContractSummary {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]LedgerPricingContractSummary, 0, len(keys))
	for _, key := range keys {
		item := items[key]
		summary := LedgerPricingContractSummary{Key: item.key, GroupName: item.group, ModelID: item.model,
			PricingScope: item.scope, PricingRevision: item.revision, BillingMode: item.mode, BillingUnit: item.unit,
			FixedDurationSeconds: item.fixedDuration, Currency: item.currency, EventCount: item.events, Requests: item.requests, RevenueKnownEvents: item.revenueKnown,
			CostKnownEvents: item.costKnown, ContractEvidenceKnownEvents: item.contractEvidenceKnown,
			RevenueCoverage: ledgerRatio(item.revenueKnown, item.events), CostCoverage: ledgerRatio(item.costKnown, item.events),
			ContractEvidenceCoverage: ledgerRatio(item.contractEvidenceKnown, item.events)}
		if item.revenueKnown > 0 {
			summary.KnownRevenue = ledgerCanonical(item.revenue)
		}
		if item.costKnown > 0 {
			summary.KnownCost = ledgerCanonical(item.cost)
		}
		if item.revenueKnown == item.events && item.costKnown == item.events && item.currency != "UNKNOWN" {
			profit := item.revenue.Sub(item.cost)
			summary.ProfitAvailable, summary.GrossProfit = true, ledgerCanonical(profit)
			if item.revenue.IsPositive() {
				summary.GrossMargin = ledgerCanonical(profit.Div(item.revenue))
			}
		} else {
			if item.revenueKnown != item.events {
				item.requires["cash_revenue"] = struct{}{}
			}
			if item.costKnown != item.events {
				item.requires["procurement_cost"] = struct{}{}
			}
		}
		for field := range item.requires {
			summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, field)
		}
		sort.Strings(summary.RequiresLedgerFields)
		result = append(result, summary)
	}
	return result
}

func finalizeDimensionSummaries(items map[string]*ledgerDimensionAccumulator) []LedgerDimensionSummary {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]LedgerDimensionSummary, 0, len(keys))
	for _, key := range keys {
		item := items[key]
		summary := LedgerDimensionSummary{Key: item.key, Label: item.label, Currency: item.currency,
			EventCount: item.events, Requests: item.requests, RevenueKnownEvents: item.revenueKnown, CostKnownEvents: item.costKnown,
			RevenueCoverage: ledgerRatio(item.revenueKnown, item.events), CostCoverage: ledgerRatio(item.costKnown, item.events)}
		if item.revenueKnown > 0 {
			summary.KnownRevenue = ledgerCanonical(item.revenue)
		}
		if item.costKnown > 0 {
			summary.KnownCost = ledgerCanonical(item.cost)
		}
		if item.revenueKnown == item.events && item.costKnown == item.events && item.currency != "UNKNOWN" {
			profit := item.revenue.Sub(item.cost)
			summary.ProfitAvailable, summary.GrossProfit = true, ledgerCanonical(profit)
			if item.revenue.IsPositive() {
				summary.GrossMargin = ledgerCanonical(profit.Div(item.revenue))
			}
		} else {
			if item.revenueKnown != item.events {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "cash_revenue")
			}
			if item.costKnown != item.events {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "procurement_cost")
			}
			if item.currency == "UNKNOWN" {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "currency")
			}
		}
		result = append(result, summary)
	}
	return result
}

func scanFinancialLedgerEvent(rows *sql.Rows) (OperatingLedgerEvent, error) {
	var event OperatingLedgerEvent
	var occurred, metadata string
	var amount, usage, usageUnit, revenue, cost, currency sql.NullString
	var isTest, isInternal int
	err := rows.Scan(&event.EventID, &event.TargetSiteID, &occurred, &event.EventKind, &amount, &event.AmountEvidence,
		&event.ModelID, &event.GroupName, &event.UserSegment, &event.SubjectUserID, &event.SubjectTokenID, &event.SubjectRole,
		&isTest, &isInternal, &event.RelatedEventID, &event.Requests, &usage, &usageUnit, &event.UsageEvidence, &revenue, &cost,
		&currency, &event.RevenueEvidence, &event.CostEvidence, &event.Source, &event.EvidenceRevision, &metadata)
	if err != nil {
		return OperatingLedgerEvent{}, err
	}
	event.Amount, event.UsageWeight, event.UsageUnit = nullString(amount), nullString(usage), nullString(usageUnit)
	event.CashRevenue, event.ProcurementCost, event.Currency = nullString(revenue), nullString(cost), nullString(currency)
	event.IsTest, event.IsInternal = isTest != 0, isInternal != 0
	event.OccurredAt = parseTime(occurred)
	_ = json.Unmarshal([]byte(metadata), &event.Metadata)
	return event, nil
}

func addFinancialEvent(items map[string]*ledgerCurrencyAccumulator, event OperatingLedgerEvent) (unknownCurrency, unknownAmount bool) {
	currency := strings.ToUpper(strings.TrimSpace(event.Currency))
	get := func() *ledgerCurrencyAccumulator {
		item := items[currency]
		if item == nil {
			item = &ledgerCurrencyAccumulator{evidenceRank: 99}
			items[currency] = item
		}
		return item
	}
	if event.EventKind == "usage" {
		revenue, revenueOK := ledgerAmount(event.CashRevenue, event.RevenueEvidence)
		cost, costOK := ledgerAmount(event.ProcurementCost, event.CostEvidence)
		if currency == "" {
			return true, false
		}
		item := get()
		item.usageEvents++
		if revenueOK {
			item.usageRevenue = item.usageRevenue.Add(revenue)
			item.evidenceRank = minEvidenceRank(item.evidenceRank, event.RevenueEvidence)
		} else {
			item.unknownUsageRevenue++
		}
		if costOK {
			item.usageCost = item.usageCost.Add(cost)
			item.evidenceRank = minEvidenceRank(item.evidenceRank, event.CostEvidence)
		} else {
			item.unknownUsageCost++
		}
		if revenueOK && costOK {
			item.matchedUsageEvents++
			item.matchedRevenue = item.matchedRevenue.Add(revenue)
			item.matchedCost = item.matchedCost.Add(cost)
		}
		return false, false
	}
	amount, ok := ledgerAmount(event.Amount, event.AmountEvidence)
	if !ok {
		if currency != "" {
			item := get()
			if event.EventKind != "gift_credit" {
				item.cashEvents++
				item.unknownCash++
			}
		}
		return currency == "", true
	}
	if currency == "" {
		return true, false
	}
	item := get()
	if event.EventKind != "gift_credit" {
		item.cashEvents++
	}
	item.evidenceRank = minEvidenceRank(item.evidenceRank, event.AmountEvidence)
	switch event.EventKind {
	case "recharge":
		item.recharge = item.recharge.Add(amount)
	case "gift_credit":
		item.gifts = item.gifts.Add(amount)
	case "refund":
		item.refunds = item.refunds.Add(amount)
	case "payment_fee":
		item.fees = item.fees.Add(amount)
	case "procurement_cost":
		item.procurement = item.procurement.Add(amount)
	case "other_income":
		item.otherIncome = item.otherIncome.Add(amount)
		if event.Metadata["cost_kind"] == "consumption_commission" {
			item.commissionReversal = item.commissionReversal.Add(amount)
			item.commissionReversalEvents++
		}
	case "other_expense":
		item.otherExpense = item.otherExpense.Add(amount)
		if event.Metadata["cost_kind"] == "consumption_commission" {
			item.commissionExpense = item.commissionExpense.Add(amount)
			item.commissionAwardEvents++
		}
	}
	return false, false
}

func ledgerAmount(raw, evidence string) (decimal.Decimal, bool) {
	if strings.TrimSpace(raw) == "" || strings.EqualFold(strings.TrimSpace(evidence), "missing") {
		return decimal.Zero, false
	}
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	return value, err == nil && !value.IsNegative()
}

func minEvidenceRank(current int, evidence string) int {
	rank := map[string]int{"exact": 4, "observed": 3, "estimated": 2, "prior": 1}[strings.ToLower(strings.TrimSpace(evidence))]
	if rank == 0 {
		return current
	}
	if current == 99 || rank < current {
		return rank
	}
	return current
}

func evidenceName(rank int) string {
	switch rank {
	case 4:
		return "exact"
	case 3:
		return "observed_or_better"
	case 2:
		return "estimated_or_better"
	case 1:
		return "prior_or_better"
	default:
		return "missing"
	}
}

func finalizeCurrencySummaries(items map[string]*ledgerCurrencyAccumulator) []LedgerCurrencySummary {
	keys := make([]string, 0, len(items))
	for currency := range items {
		if currency != "" {
			keys = append(keys, currency)
		}
	}
	sort.Strings(keys)
	result := make([]LedgerCurrencySummary, 0, len(keys))
	for _, currency := range keys {
		item := items[currency]
		summary := LedgerCurrencySummary{Currency: currency, UsageEventCount: item.usageEvents,
			MatchedUsageEventCount: item.matchedUsageEvents, CashFlowEventCount: item.cashEvents,
			UsageRevenue:         ledgerCanonical(item.usageRevenue),
			UsageProcurementCost: ledgerCanonical(item.usageCost),
			MatchedUsageRevenue:  ledgerCanonical(item.matchedRevenue), MatchedUsageProcurementCost: ledgerCanonical(item.matchedCost),
			RechargeGross: ledgerCanonical(item.recharge),
			Refunds:       ledgerCanonical(item.refunds), PaymentFees: ledgerCanonical(item.fees), ProcurementPayments: ledgerCanonical(item.procurement),
			GiftCreditIssued: ledgerCanonical(item.gifts), OtherIncome: ledgerCanonical(item.otherIncome), OtherExpense: ledgerCanonical(item.otherExpense),
			CommissionAwardEvents: item.commissionAwardEvents, CommissionReversalEvents: item.commissionReversalEvents,
			CommissionExpense: ledgerCanonical(item.commissionExpense), CommissionReversal: ledgerCanonical(item.commissionReversal),
			UnknownUsageRevenueEvents: item.unknownUsageRevenue, UnknownUsageCostEvents: item.unknownUsageCost,
			UnknownCashFlowAmountEvents: item.unknownCash, EvidenceQuality: evidenceName(item.evidenceRank)}
		if item.usageEvents > 0 && item.unknownUsageRevenue == 0 && item.unknownUsageCost == 0 {
			profit := item.usageRevenue.Sub(item.usageCost)
			summary.UsageProfitAvailable, summary.UsageGrossProfit = true, ledgerCanonical(profit)
			if item.usageRevenue.IsPositive() {
				summary.UsageGrossMargin = ledgerCanonical(profit.Div(item.usageRevenue))
			}
		}
		if item.cashEvents > 0 && item.unknownCash == 0 {
			net := item.recharge.Add(item.otherIncome).Sub(item.refunds).Sub(item.fees).Sub(item.procurement).Sub(item.otherExpense)
			summary.CashNetFlowAvailable, summary.CashNetFlow = true, ledgerCanonical(net)
		}
		result = append(result, summary)
	}
	return result
}

func addSegmentEvent(items map[string]*ledgerSegmentAccumulator, event OperatingLedgerEvent, included bool, reasons []string) {
	segment := strings.TrimSpace(event.UserSegment)
	if segment == "" {
		segment = "unclassified"
	}
	currency := strings.ToUpper(strings.TrimSpace(event.Currency))
	if currency == "" {
		currency = "UNKNOWN"
	}
	reasonKey := strings.Join(reasons, ",")
	key := strings.ToLower(segment) + "\x00" + currency + "\x00" + strconv.FormatBool(included) + "\x00" + reasonKey
	item := items[key]
	if item == nil {
		item = &ledgerSegmentAccumulator{segment: segment, currency: currency, included: included, reasons: append([]string(nil), reasons...)}
		items[key] = item
	}
	item.events++
	item.requests += event.Requests
	if value, ok := ledgerAmount(event.CashRevenue, event.RevenueEvidence); ok {
		item.revenueKnown++
		item.revenue = item.revenue.Add(value)
	}
	if value, ok := ledgerAmount(event.ProcurementCost, event.CostEvidence); ok {
		item.costKnown++
		item.cost = item.cost.Add(value)
	}
}

func finalizeSegmentSummaries(items map[string]*ledgerSegmentAccumulator) []LedgerSegmentSummary {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]LedgerSegmentSummary, 0, len(keys))
	for _, key := range keys {
		item := items[key]
		summary := LedgerSegmentSummary{UserSegment: item.segment, Currency: item.currency, Included: item.included,
			ExclusionReasons: item.reasons, EventCount: item.events, Requests: item.requests, RevenueKnownEvents: item.revenueKnown,
			CostKnownEvents: item.costKnown, RevenueCoverage: ledgerRatio(item.revenueKnown, item.events), CostCoverage: ledgerRatio(item.costKnown, item.events)}
		if item.revenueKnown > 0 {
			summary.KnownRevenue = ledgerCanonical(item.revenue)
		}
		if item.costKnown > 0 {
			summary.KnownCost = ledgerCanonical(item.cost)
		}
		if item.revenueKnown == item.events && item.costKnown == item.events && item.currency != "UNKNOWN" {
			summary.ContributionAvailable, summary.Contribution = true, ledgerCanonical(item.revenue.Sub(item.cost))
		} else {
			if item.revenueKnown != item.events {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "cash_revenue")
			}
			if item.costKnown != item.events {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "procurement_cost")
			}
			if item.currency == "UNKNOWN" {
				summary.RequiresLedgerFields = append(summary.RequiresLedgerFields, "currency")
			}
		}
		result = append(result, summary)
	}
	return result
}

func ledgerRatio(numerator, denominator int) string {
	if denominator <= 0 {
		return "0.000000"
	}
	return ledgerCanonical(decimal.NewFromInt(int64(numerator)).Div(decimal.NewFromInt(int64(denominator))))
}

func ledgerCanonical(value decimal.Decimal) string { return value.Round(6).StringFixed(6) }

func parseFinancialWatermark(event OperatingLedgerEvent) (bool, financialWatermark) {
	metadata := event.Metadata
	if metadata == nil {
		return false, financialWatermark{}
	}
	id := strings.TrimSpace(metadata["financial_watermark_id"])
	startRaw, endRaw := strings.TrimSpace(metadata["financial_window_start"]), strings.TrimSpace(metadata["financial_window_end"])
	countRaw := strings.TrimSpace(metadata["financial_authoritative_event_count"])
	if id == "" && startRaw == "" && endRaw == "" && countRaw == "" {
		return false, financialWatermark{}
	}
	item := financialWatermark{id: id, source: event.Source, targetID: event.TargetSiteID, authoritative: -1}
	var err error
	item.start, err = time.Parse(time.RFC3339Nano, startRaw)
	if err != nil {
		item.conflict = true
	}
	item.end, err = time.Parse(time.RFC3339Nano, endRaw)
	if err != nil || !item.start.Before(item.end) {
		item.conflict = true
	}
	item.authoritative, err = strconv.Atoi(countRaw)
	if err != nil || item.authoritative < 0 || id == "" {
		item.conflict = true
	}
	return true, item
}

func financialCohortKey(targetID int64, source string) string {
	return strconv.FormatInt(targetID, 10) + "\x00" + strings.ToLower(strings.TrimSpace(source))
}

func summarizeFinancialCoverage(start, end time.Time, cohorts map[string][]coverageEvent, watermarks map[string]*financialWatermark, metadataSeen bool) LedgerCoverageSummary {
	result := LedgerCoverageSummary{Status: "unknown", WindowStart: start, WindowEnd: end, Cohorts: len(cohorts)}
	if len(cohorts) == 0 {
		result.Note = "报表窗口内没有账本事件，无法验证源站完整性。"
		return result
	}
	for cohortKey, events := range cohorts {
		verified := false
		candidateSeen := false
		for key, watermark := range watermarks {
			if !strings.HasPrefix(key, cohortKey+"\x00") {
				continue
			}
			candidateSeen = true
			if watermark.conflict {
				result.ConflictingWatermarks++
				continue
			}
			if !watermark.start.Equal(start) || !watermark.end.Equal(end) {
				continue
			}
			if watermark.authoritative == len(events) {
				verified = true
				break
			}
		}
		if verified {
			result.VerifiedCohorts++
		} else if !candidateSeen {
			result.MissingWatermarkCohorts++
		}
	}
	switch {
	case result.ConflictingWatermarks > 0:
		result.Status = "conflict"
		result.Note = "至少一个权威水位内容冲突或格式无效，不能证明账本窗口完整。"
	case result.VerifiedCohorts == result.Cohorts:
		result.Status = "verified"
		result.Note = "每个目标站点/来源分组均有与报表窗口和事件数一致的权威水位。"
	case metadataSeen || result.VerifiedCohorts > 0:
		result.Status = "partial"
		result.Note = "只有部分来源具备可核验水位，经营数值仅代表已导入事件。"
	default:
		result.Note = "未提供源站权威完整性水位，经营数值仅代表已导入事件。"
	}
	return result
}

func sumReasonCounts(items map[string]int) int {
	total := 0
	for _, count := range items {
		total += count
	}
	return total
}

func appendFinancialWarnings(result *LedgerFinancialSummary) {
	if result.RawEventCount == 0 {
		result.Warnings = append(result.Warnings, "当前窗口没有经营账本事件。")
	}
	if result.Coverage.Status != "verified" {
		result.Warnings = append(result.Warnings, result.Coverage.Note)
	}
	if result.UnclassifiedSubjectEvents > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 条事件缺少主体分类，无法确认其中是否含管理员、测试或内部流量。", result.UnclassifiedSubjectEvents))
	}
	if result.UnknownCurrencyEvents > 0 || result.UnknownAmountEvents > 0 {
		result.Warnings = append(result.Warnings, "存在未知币种或金额；未知值未按 0 参与净额或毛利。")
	}
	if result.UnknownUsageCostEvents > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d 条调用缺少 procurement_cost；严格毛利保持未知，可继续查看单独标注的运营差额估算。", result.UnknownUsageCostEvents))
	}
	if len(result.ByCurrency) > 1 {
		result.Warnings = append(result.Warnings, "存在多个币种且没有汇率证据，系统未生成跨币种合计。")
	}
	if result.ExcludedReasonCountsOverlap {
		result.Warnings = append(result.Warnings, "同一事件可能命中多个排除原因；按原因计数可重叠，排除事件总数已去重。")
	}
}
