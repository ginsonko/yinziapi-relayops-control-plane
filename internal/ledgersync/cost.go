package ledgersync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type costResolver struct {
	target             domain.TargetSite
	currency           string
	cashRate           decimal.Decimal
	maxAge             time.Duration
	channels           map[int64]domain.TargetChannel
	targetCatalog      domain.CatalogSnapshot
	policies           []domain.PricingPolicy
	suppliers          map[int64]costSupplier
	policyCost         map[int64]policyCostBasis
	policyCostDone     map[int64]bool
	targetContract     *domain.GroupModelPricingContract
	contractByRevision map[string]map[string]contractCostEvidence
	contractBySource   map[string]contractCostEvidence
}

type contractCostEvidence struct {
	policyID        int64
	targetGroup     string
	appliedRevision string
	model           domain.ListingModelPricingEvidence
}

type costSupplier struct {
	supplier      domain.Supplier
	snapshot      domain.CatalogSnapshot
	canonical     string
	snapshotState string
}

type costBinding struct {
	policy   domain.PricingPolicy
	supplier costSupplier
	method   string
}

type policyCostBasis struct {
	ratio      decimal.Decimal
	method     string
	coverage   string
	confidence string
	warning    string
}

type derivedCost struct {
	amount   string
	currency string
	evidence string
	metadata map[string]string
}

type costResolutionFailure struct {
	Code     string
	Message  string
	Metadata map[string]string
}

type costResolutionAccumulator struct {
	reasons map[string]*store.TargetLedgerCostResolutionReason
}

func (f *costResolutionFailure) Error() string {
	if f == nil {
		return ""
	}
	return f.Message
}

func newCostFailure(code, message string, metadata map[string]string) *costResolutionFailure {
	return &costResolutionFailure{Code: code, Message: message, Metadata: metadata}
}

func newCostResolutionAccumulator() *costResolutionAccumulator {
	return &costResolutionAccumulator{reasons: map[string]*store.TargetLedgerCostResolutionReason{}}
}

func (a *costResolutionAccumulator) add(failure *costResolutionFailure, event store.OperatingLedgerEvent) {
	if failure == nil {
		return
	}
	code := strings.TrimSpace(failure.Code)
	if code == "" {
		code = "cost_resolution_failed"
	}
	item := a.reasons[code]
	if item == nil {
		item = &store.TargetLedgerCostResolutionReason{Code: code, Message: safeCostDiagnostic(failure.Message, 500),
			Samples: []store.TargetLedgerCostResolutionSample{}}
		a.reasons[code] = item
	}
	item.Count++
	if len(item.Samples) >= 3 {
		return
	}
	metadata := failure.Metadata
	sample := store.TargetLedgerCostResolutionSample{
		TargetGroup: safeCostDiagnostic(firstCostDiagnostic(metadata["target_group"], event.GroupName), 160),
		Model:       safeCostDiagnostic(firstCostDiagnostic(metadata["model"], event.ModelID), 160),
		ChannelID:   safeCostDiagnostic(firstCostDiagnostic(metadata["channel_id"], event.Metadata["channel_id"]), 80), ChannelName: safeCostDiagnostic(metadata["channel_name"], 160),
		PolicyID: safeCostDiagnostic(metadata["policy_id"], 80), PolicyName: safeCostDiagnostic(metadata["policy_name"], 160),
		SupplierID: safeCostDiagnostic(metadata["supplier_id"], 80), SupplierName: safeCostDiagnostic(metadata["supplier_name"], 160),
		SourceGroup: safeCostDiagnostic(metadata["source_group"], 160),
	}
	item.Samples = append(item.Samples, sample)
}

func (a *costResolutionAccumulator) list() []store.TargetLedgerCostResolutionReason {
	result := make([]store.TargetLedgerCostResolutionReason, 0, len(a.reasons))
	for _, item := range a.reasons {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Code < result[j].Code
	})
	return result
}

func safeCostDiagnostic(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func firstCostDiagnostic(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func rawRowStrings(row map[string]json.RawMessage) map[string]string {
	result := make(map[string]string, len(row))
	for key, value := range row {
		result[key] = strings.TrimSpace(rawScalar(value))
	}
	for key, value := range rawNestedScalars(row, "other") {
		if strings.TrimSpace(result[key]) == "" {
			result[key] = value
		}
	}
	return result
}

func (s *Service) buildCostResolver(ctx context.Context, target domain.TargetSite, config store.TargetLedgerSyncConfig, now time.Time) (*costResolver, error) {
	if s.targetFactory == nil {
		return nil, errors.New("target channel adapter is not configured")
	}
	adapter, err := s.targetFactory.ForTarget(target)
	if err != nil {
		return nil, err
	}
	channelAdapter, err := targets.AsChannelAdapter(adapter)
	if err != nil {
		return nil, err
	}
	channels, err := channelAdapter.ListChannels(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("read target channels for procurement cost: %w", err)
	}
	targetCatalog, err := channelAdapter.PricingCatalog(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("read target pricing catalog for procurement cost: %w", err)
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(config.SupplierCostCashRate))
	if err != nil || !rate.IsPositive() {
		return nil, errors.New("supplier cost cash rate is not a positive decimal")
	}
	resolver := &costResolver{
		target: target, currency: strings.ToUpper(strings.TrimSpace(config.Currency)), cashRate: rate,
		maxAge: time.Duration(config.CostEvidenceMaxAgeSeconds) * time.Second, channels: make(map[int64]domain.TargetChannel, len(channels)),
		targetCatalog: targetCatalog, suppliers: map[int64]costSupplier{}, policyCost: map[int64]policyCostBasis{}, policyCostDone: map[int64]bool{},
		contractByRevision: map[string]map[string]contractCostEvidence{}, contractBySource: map[string]contractCostEvidence{},
	}
	if groupModelAdapter, groupModelErr := targets.AsGroupModelPricingAdapter(adapter); groupModelErr == nil {
		if contract, readErr := groupModelAdapter.ReadGroupModelPricing(ctx, target); readErr == nil {
			resolver.targetContract = &contract
		}
	}
	for _, channel := range channels {
		resolver.channels[channel.ID] = channel
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for _, policy := range policies {
		if policy.Enabled && policy.TargetSiteID == target.ID {
			resolver.policies = append(resolver.policies, policy)
		}
	}
	// A complete group mirror owns the upstream group/model contract even when
	// the administrator intentionally has no separate pricing-mapping rule.
	// Use it as a cost-binding fallback, while keeping explicit pricing rules
	// ahead of it when both describe the same supplier/source group.
	mirrorPolicies, err := s.store.ListMirrorPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for index, policy := range mirrorPolicies {
		if !policy.Enabled || policy.TargetSiteID != target.ID || policy.Kind != mirror.PolicyKindCompleteMirror {
			continue
		}
		pattern := strings.TrimSpace(policy.SourceModelPattern)
		if pattern == "" {
			pattern = "*"
		}
		resolver.policies = append(resolver.policies, domain.PricingPolicy{
			ID: int64(1<<62) + int64(index), Name: "镜像成本绑定: " + policy.SourceGroup + " -> " + policy.TargetGroup,
			SupplierID: policy.SupplierID, SourceGroup: policy.SourceGroup, SourceModelPattern: pattern,
			SourceBasis: "group_ratio", TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup,
			RuleType: "same", Enabled: true, ExecutionMode: "auto", UpdatedAt: now,
		})
	}
	sort.Slice(resolver.policies, func(i, j int) bool { return resolver.policies[i].ID < resolver.policies[j].ID })
	proposals, err := s.store.ListProposals(ctx, "", 5000)
	if err != nil {
		return nil, fmt.Errorf("read pricing proposals for procurement cost: %w", err)
	}
	for _, proposal := range proposals {
		if proposal.TargetSiteID != target.ID || proposal.GroupModelPricing.Mode != "atomic_contract" || proposal.Status != "applied" {
			continue
		}
		revision := firstCostDiagnostic(proposal.GroupModelPricingReceipt.AppliedRevision, proposal.GroupModelPricing.DesiredRevision)
		if revision == "" {
			continue
		}
		if resolver.contractByRevision[revision] == nil {
			resolver.contractByRevision[revision] = map[string]contractCostEvidence{}
		}
		for _, model := range proposal.GroupModelPricing.Models {
			evidence := contractCostEvidence{policyID: proposal.PolicyID, targetGroup: proposal.TargetGroup, appliedRevision: revision, model: model}
			key := contractCostKey(proposal.PolicyID, proposal.TargetGroup, model.Model)
			resolver.contractByRevision[revision][key] = evidence
			if sourceRevision := strings.TrimSpace(model.SourceEvidenceHash); sourceRevision != "" {
				resolver.contractBySource[sourceRevision+"\x00"+key] = evidence
			}
		}
	}
	suppliers, err := s.store.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	for _, supplier := range suppliers {
		if !supplier.Enabled {
			continue
		}
		entry := costSupplier{supplier: supplier, snapshotState: "missing"}
		entry.canonical, _ = canonicalOrigin(supplier.BaseURL)
		snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID)
		switch {
		case snapshotErr != nil:
			entry.snapshotState = "missing"
		case snapshot.Evidence.ObservedAt.IsZero():
			entry.snapshot, entry.snapshotState = snapshot, "missing_observed_at"
		case now.Sub(snapshot.Evidence.ObservedAt) > resolver.maxAge:
			entry.snapshot, entry.snapshotState = snapshot, "stale"
		default:
			entry.snapshot, entry.snapshotState = snapshot, "fresh"
		}
		resolver.suppliers[supplier.ID] = entry
	}
	return resolver, nil
}

func (r *costResolver) derive(event store.OperatingLedgerEvent, row map[string]string) (derivedCost, *costResolutionFailure) {
	channelID := strings.TrimSpace(row["channel_id"])
	if channelID == "" {
		channelID = strings.TrimSpace(event.Metadata["channel_id"])
	}
	baseMetadata := map[string]string{"target_group": strings.TrimSpace(event.GroupName), "model": strings.TrimSpace(event.ModelID), "channel_id": channelID}
	modelName := strings.TrimSpace(event.ModelID)
	if modelName == "" {
		return derivedCost{}, newCostFailure("model_missing", "target log has no model name", baseMetadata)
	}
	if channelID == "" || parseID(channelID) == 0 {
		binding, failure := r.resolveTargetGroupBinding(strings.TrimSpace(event.GroupName), modelName)
		if failure != nil {
			return derivedCost{}, newCostFailure("channel_readback_missing", "target channel was not read back and its target group has no unique mirror cost binding", baseMetadata)
		}
		channel := domain.TargetChannel{ID: 0, Name: "未回读渠道（按目标分组镜像）", BaseURL: binding.supplier.supplier.BaseURL, Group: event.GroupName}
		return r.deriveFromBinding(event, row, "0", channel, binding)
	}
	channel, ok := r.channels[parseID(channelID)]
	if !ok {
		return derivedCost{}, newCostFailure("channel_readback_missing", "target channel was not found during readback", baseMetadata)
	}
	binding, failure := r.resolveChannelBinding(channel, strings.TrimSpace(event.GroupName), modelName)
	if failure != nil {
		for key, value := range baseMetadata {
			if failure.Metadata == nil {
				failure.Metadata = map[string]string{}
			}
			failure.Metadata[key] = value
		}
		return derivedCost{}, failure
	}
	return r.deriveFromBinding(event, row, channelID, channel, binding)
}

var zeroCostUsageDimensions = []string{
	"prompt_tokens", "input_tokens", "completion_tokens", "output_tokens", "cache_tokens", "cache_read_tokens",
	"cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h", "seconds", "second", "duration",
	"duration_seconds", "image_count", "images", "frame_count", "frames", "image_tokens", "image_input_tokens",
	"audio_tokens", "audio_input_tokens", "audio_output_tokens",
}

func explicitUnroutedZeroCost(event store.OperatingLedgerEvent, currency string) (derivedCost, bool) {
	channelID := strings.TrimSpace(event.Metadata["channel_id"])
	if channelID != "" && channelID != "0" || strings.TrimSpace(event.ModelID) != "" || strings.TrimSpace(event.GroupName) != "" {
		return derivedCost{}, false
	}
	revenue, revenueErr := decimal.NewFromString(strings.TrimSpace(event.CashRevenue))
	usage, usageErr := decimal.NewFromString(strings.TrimSpace(event.UsageWeight))
	if revenueErr != nil || usageErr != nil || !revenue.IsZero() || !usage.IsZero() {
		return derivedCost{}, false
	}
	for _, key := range zeroCostUsageDimensions {
		raw := strings.TrimSpace(event.Metadata[key])
		if raw == "" {
			continue
		}
		value, err := decimal.NewFromString(raw)
		if err != nil || !value.IsZero() {
			return derivedCost{}, false
		}
	}
	currency = strings.ToUpper(strings.TrimSpace(firstCostDiagnostic(event.Currency, currency)))
	if currency == "" {
		return derivedCost{}, false
	}
	return derivedCost{amount: "0", currency: currency, evidence: "observed", metadata: map[string]string{
		"procurement_cost_method":  "explicit_unrouted_zero_usage",
		"procurement_cost_formula": "channel_id=0 AND model/group empty AND cash_revenue=0 AND all observed usage dimensions=0",
		"procurement_cost_note":    "request did not reach an upstream route; zero cost is evidence-based, not a default for unknown costs",
	}}, true
}

func (r *costResolver) deriveFromBinding(event store.OperatingLedgerEvent, row map[string]string, channelID string,
	channel domain.TargetChannel, binding costBinding,
) (derivedCost, *costResolutionFailure) {
	baseMetadata := map[string]string{"target_group": strings.TrimSpace(event.GroupName), "model": strings.TrimSpace(event.ModelID), "channel_id": channelID, "channel_name": channel.Name,
		"policy_id": strconv.FormatInt(binding.policy.ID, 10), "policy_name": binding.policy.Name,
		"supplier_id": strconv.FormatInt(binding.supplier.supplier.ID, 10), "supplier_name": binding.supplier.supplier.Name,
		"source_group": binding.policy.SourceGroup}
	if contractCost, contractFailure, matched := r.deriveFrozenContractCost(event, row, channelID, channel, binding); matched {
		return contractCost, contractFailure
	}
	if binding.supplier.snapshotState != "fresh" {
		return derivedCost{}, newCostFailure("supplier_snapshot_"+binding.supplier.snapshotState,
			"supplier catalog snapshot is not fresh enough for procurement cost", baseMetadata)
	}
	sourceGroup, ok := findCatalogGroup(binding.supplier.snapshot, binding.policy.SourceGroup)
	if !ok {
		return derivedCost{}, newCostFailure("source_group_missing", "pricing policy source group is absent from the supplier catalog", baseMetadata)
	}
	baseMetadata["source_group_name"] = sourceGroup.Name
	baseMetadata["source_group_id"] = sourceGroup.ExternalID
	modelName := strings.TrimSpace(event.ModelID)
	if !containsFold(sourceGroup.ModelIDs, modelName) {
		return derivedCost{}, newCostFailure("model_not_in_source_group", "request model is not present in the policy source group", baseMetadata)
	}
	if !matchesModelPattern(modelName, binding.policy.SourceModelPattern) {
		return derivedCost{}, newCostFailure("model_outside_policy_pattern", "request model is outside the pricing policy model pattern", baseMetadata)
	}
	sourceModel, sourceModelFound := findCatalogModel(binding.supplier.snapshot, modelName, sourceGroup)
	directFallbackReason := "source_model_price_missing"
	if sourceModelFound {
		if amount, formula, direct, reason := calculateModelCost(sourceModel.PriceFunction, event, row); direct {
			groupRatio, ratioErr := nonnegativeCostDecimal(ratios.DecisionRatio(sourceGroup))
			if ratioErr != nil {
				return derivedCost{}, newCostFailure("source_group_ratio_invalid", "source group effective ratio is invalid", baseMetadata)
			}
			cost := amount.Mul(groupRatio).Mul(r.cashRate)
			return r.finishDerivedCost(cost, channelID, channel, binding, sourceGroup, "policy_channel_direct_catalog", "estimated",
				formula+fmt.Sprintf(" * source_effective_ratio(%s) * supplier_cost_cash_rate(%s)", groupRatio.String(), r.cashRate.String()), map[string]string{}), nil
		} else {
			directFallbackReason = reason
		}
	}

	revenue, revenueErr := decimal.NewFromString(strings.TrimSpace(event.CashRevenue))
	if revenueErr != nil || !revenue.IsPositive() {
		return derivedCost{}, newCostFailure("positive_request_revenue_missing",
			"ratio-based procurement cost requires positive observed request revenue; zero revenue does not prove zero upstream cost", baseMetadata)
	}
	if event.Currency != "" && r.currency != "" && !strings.EqualFold(strings.TrimSpace(event.Currency), r.currency) {
		baseMetadata["event_currency"] = event.Currency
		baseMetadata["cost_currency"] = r.currency
		return derivedCost{}, newCostFailure("request_currency_conflict", "request revenue currency conflicts with procurement cost currency", baseMetadata)
	}
	targetGroup, ok := findCatalogGroup(r.targetCatalog, event.GroupName)
	if !ok {
		return derivedCost{}, newCostFailure("target_group_price_missing", "request target group is absent from the current target pricing catalog", baseMetadata)
	}
	targetRatio, ratioErr := positiveCostDecimal(ratios.DecisionRatio(targetGroup))
	if ratioErr != nil {
		return derivedCost{}, newCostFailure("target_group_ratio_invalid", "request target group ratio is not a positive decimal", baseMetadata)
	}
	basis, basisFailure := r.resolvePolicyCostBasis(binding, sourceGroup)
	if basisFailure != nil {
		for key, value := range baseMetadata {
			if basisFailure.Metadata == nil {
				basisFailure.Metadata = map[string]string{}
			}
			basisFailure.Metadata[key] = value
		}
		return derivedCost{}, basisFailure
	}
	cost := revenue.Mul(basis.ratio).Div(targetRatio).Mul(r.cashRate)
	extra := map[string]string{
		"procurement_cost_target_group":                   targetGroup.Name,
		"procurement_cost_target_group_ratio":             targetRatio.String(),
		"procurement_cost_target_snapshot_hash":           r.targetCatalog.Evidence.ResponseHash,
		"procurement_cost_target_observed_at":             r.targetCatalog.Evidence.ObservedAt.UTC().Format(time.RFC3339Nano),
		"procurement_cost_policy_basis":                   binding.policy.SourceBasis,
		"procurement_cost_policy_cost_ratio":              basis.ratio.String(),
		"procurement_cost_policy_coverage":                basis.coverage,
		"procurement_cost_policy_confidence":              basis.confidence,
		"procurement_cost_policy_cost_ratio_derivation":   basis.method,
		"procurement_cost_direct_catalog_fallback_reason": directFallbackReason,
	}
	if basis.warning != "" {
		extra["procurement_cost_policy_warning"] = basis.warning
	}
	formula := fmt.Sprintf("observed_request_revenue(%s) * policy_cost_ratio(%s) / target_group_ratio(%s) * supplier_cost_cash_rate(%s)",
		revenue.String(), basis.ratio.String(), targetRatio.String(), r.cashRate.String())
	return r.finishDerivedCost(cost, channelID, channel, binding, sourceGroup, "policy_channel_revenue_ratio", "estimated", formula, extra), nil
}

func contractCostKey(policyID int64, targetGroup, model string) string {
	return fmt.Sprintf("%d\x00%s\x00%s", policyID, strings.ToLower(strings.TrimSpace(targetGroup)), strings.ToLower(strings.TrimSpace(model)))
}

func (r *costResolver) deriveFrozenContractCost(event store.OperatingLedgerEvent, row map[string]string, channelID string,
	channel domain.TargetChannel, binding costBinding) (derivedCost, *costResolutionFailure, bool) {
	if !strings.EqualFold(strings.TrimSpace(event.Metadata["pricing_scope"]), "group_model") {
		return derivedCost{}, nil, false
	}
	revision := strings.TrimSpace(event.Metadata["pricing_revision"])
	base := map[string]string{"target_group": event.GroupName, "model": event.ModelID, "channel_id": channelID,
		"channel_name": channel.Name, "policy_id": strconv.FormatInt(binding.policy.ID, 10), "policy_name": binding.policy.Name,
		"supplier_id": strconv.FormatInt(binding.supplier.supplier.ID, 10), "supplier_name": binding.supplier.supplier.Name,
		"source_group": binding.policy.SourceGroup}
	if revision == "" {
		return derivedCost{}, nil, false
	}
	key := contractCostKey(binding.policy.ID, event.GroupName, event.ModelID)
	var evidence contractCostEvidence
	found := false
	if byKey := r.contractByRevision[revision]; byKey != nil {
		evidence, found = byKey[key]
	}
	if !found && r.targetContract != nil && r.targetContract.Revision == revision {
		if setting, ok := contractSettingFor(r.targetContract, event.GroupName, event.ModelID); ok && strings.TrimSpace(setting.SourceRevision) != "" {
			evidence, found = r.contractBySource[strings.TrimSpace(setting.SourceRevision)+"\x00"+key]
		}
	}
	if !found {
		return derivedCost{}, nil, false
	}
	amount, formula, reason := calculateFrozenModelCost(evidence.model, event, row)
	if reason != "" {
		base["pricing_revision"] = revision
		base["billing_unit"] = evidence.model.BillingUnit
		return derivedCost{}, newCostFailure("target_contract_"+reason,
			"the frozen model cost vector cannot be applied to this request: "+reason, base), true
	}
	cost := amount.Mul(r.cashRate)
	sourceGroup := domain.GroupOffer{Name: binding.policy.SourceGroup, ExternalID: binding.policy.SourceGroup,
		RechargeValueFactor: evidence.model.RechargeValueFactor, EffectiveRatio: evidence.model.SourceCashGroupRatio}
	extra := map[string]string{
		"procurement_cost_target_pricing_scope":    "group_model",
		"procurement_cost_target_pricing_revision": revision,
		"procurement_cost_target_billing_unit":     evidence.model.BillingUnit,
		"procurement_cost_source_currency":         evidence.model.Currency,
		"procurement_cost_source_evidence_hash":    evidence.model.SourceEvidenceHash,
		"procurement_cost_source_observed_at":      evidence.model.SourceObservedAt.UTC().Format(time.RFC3339Nano),
		"procurement_cost_supplier_snapshot_hash":  evidence.model.SourceEvidenceHash,
		"procurement_cost_supplier_observed_at":    evidence.model.SourceObservedAt.UTC().Format(time.RFC3339Nano),
	}
	fullFormula := formula + fmt.Sprintf(" * supplier_cost_cash_rate(%s)", r.cashRate.String())
	return r.finishDerivedCost(cost, channelID, channel, binding, sourceGroup, "group_model_contract_frozen", "observed", fullFormula, extra), nil, true
}

func contractSettingFor(contract *domain.GroupModelPricingContract, group, model string) (domain.GroupModelPricingSetting, bool) {
	if contract == nil {
		return domain.GroupModelPricingSetting{}, false
	}
	for groupName, models := range contract.Groups {
		if !strings.EqualFold(strings.TrimSpace(groupName), strings.TrimSpace(group)) {
			continue
		}
		for modelName, setting := range models {
			if strings.EqualFold(strings.TrimSpace(modelName), strings.TrimSpace(model)) {
				return setting, true
			}
		}
	}
	return domain.GroupModelPricingSetting{}, false
}

func calculateFrozenModelCost(model domain.ListingModelPricingEvidence, event store.OperatingLedgerEvent,
	row map[string]string) (decimal.Decimal, string, string) {
	prices := make(map[string]decimal.Decimal, len(model.Dimensions))
	for _, dimension := range model.Dimensions {
		value, err := nonnegativeCostDecimal(dimension.SourceCashCost)
		if err != nil {
			return decimal.Zero, "", "source_price_invalid:" + dimension.Dimension
		}
		prices[dimension.Dimension] = value
	}
	requests := decimal.NewFromInt(event.Requests)
	if requests.IsZero() {
		requests = decimal.NewFromInt(1)
	}
	if price, ok := prices["per_request"]; ok {
		if len(prices) != 1 {
			return decimal.Zero, "", "mixed_fixed_dimensions"
		}
		return price.Mul(requests), fmt.Sprintf("frozen_per_request(%s) * requests(%s)", price, requests), ""
	}
	if price, ok := prices["per_second"]; ok {
		if len(prices) != 1 {
			return decimal.Zero, "", "mixed_fixed_dimensions"
		}
		seconds, found := decimalValue(row, "seconds", "second", "duration_seconds", "duration")
		if !found || seconds.IsNegative() {
			return decimal.Zero, "", "request_seconds_missing"
		}
		return price.Mul(seconds), fmt.Sprintf("frozen_per_second(%s) * seconds(%s)", price, seconds), ""
	}
	if _, audioInput := prices["audio_input_per_1m"]; audioInput {
		return decimal.Zero, "", "audio_token_semantics_unavailable"
	}
	if _, audioOutput := prices["audio_output_per_1m"]; audioOutput {
		return decimal.Zero, "", "audio_token_semantics_unavailable"
	}
	inputPrice, inputOK := prices["input_per_1m"]
	outputPrice, outputOK := prices["output_per_1m"]
	if !inputOK || !outputOK {
		return decimal.Zero, "", "token_price_vector_incomplete"
	}
	promptTokens, promptFound := decimalValue(row, "prompt_tokens", "input_tokens")
	completionTokens, completionFound := decimalValue(row, "completion_tokens", "output_tokens")
	if !promptFound || !completionFound || promptTokens.IsNegative() || completionTokens.IsNegative() {
		return decimal.Zero, "", "request_token_dimensions_missing"
	}
	cacheRead := decimalOrZero(row, "cache_tokens", "cache_read_tokens")
	cacheCreate := decimalOrZero(row, "cache_creation_tokens")
	cacheCreate5m := decimalOrZero(row, "cache_creation_tokens_5m")
	cacheCreate1h := decimalOrZero(row, "cache_creation_tokens_1h")
	imageInput := decimalOrZero(row, "image_tokens", "image_input_tokens")
	baseInput := promptTokens.Sub(imageInput)
	if !strings.EqualFold(strings.TrimSpace(row["claude"]), "true") {
		baseInput = baseInput.Sub(cacheRead).Sub(cacheCreate)
	}
	if baseInput.IsNegative() {
		return decimal.Zero, "", "request_token_semantics_inconsistent"
	}
	total := inputPrice.Mul(baseInput).Add(outputPrice.Mul(completionTokens))
	parts := []string{fmt.Sprintf("input(%s)*%s", inputPrice, baseInput), fmt.Sprintf("output(%s)*%s", outputPrice, completionTokens)}
	optional := []struct {
		name  string
		value decimal.Decimal
	}{
		{"cache_read_per_1m", cacheRead},
		{"cache_write_per_1m", decimal.Max(cacheCreate.Sub(cacheCreate5m).Sub(cacheCreate1h), decimal.Zero)},
		{"cache_write_5m_per_1m", cacheCreate5m},
		{"cache_write_1h_per_1m", cacheCreate1h},
		{"image_input_per_1m", imageInput},
	}
	for _, dimension := range optional {
		if price, ok := prices[dimension.name]; ok {
			total = total.Add(price.Mul(dimension.value))
			parts = append(parts, fmt.Sprintf("%s(%s)*%s", dimension.name, price, dimension.value))
		}
	}
	return total.Div(decimal.NewFromInt(1_000_000)), strings.Join(parts, " + ") + " / 1000000", ""
}

func decimalOrZero(row map[string]string, keys ...string) decimal.Decimal {
	if value, ok := decimalValue(row, keys...); ok && !value.IsNegative() {
		return value
	}
	return decimal.Zero
}

func (r *costResolver) finishDerivedCost(cost decimal.Decimal, channelID string, channel domain.TargetChannel, binding costBinding,
	group domain.GroupOffer, method, evidence, formula string, extra map[string]string) derivedCost {
	metadata := map[string]string{
		"procurement_cost_method":                 method,
		"procurement_cost_target_channel_id":      channelID,
		"procurement_cost_target_channel_name":    channel.Name,
		"procurement_cost_route_binding_method":   binding.method,
		"procurement_cost_policy_id":              strconv.FormatInt(binding.policy.ID, 10),
		"procurement_cost_policy_name":            binding.policy.Name,
		"procurement_cost_policy_updated_at":      binding.policy.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"procurement_cost_policy_fingerprint":     pricing.PolicyFingerprint(binding.policy),
		"procurement_cost_supplier_id":            strconv.FormatInt(binding.supplier.supplier.ID, 10),
		"procurement_cost_supplier_name":          binding.supplier.supplier.Name,
		"procurement_cost_supplier_group":         group.Name,
		"procurement_cost_supplier_group_id":      group.ExternalID,
		"procurement_cost_supplier_snapshot_hash": binding.supplier.snapshot.Evidence.ResponseHash,
		"procurement_cost_supplier_observed_at":   binding.supplier.snapshot.Evidence.ObservedAt.UTC().Format(time.RFC3339Nano),
		"procurement_cost_recharge_factor":        group.RechargeValueFactor,
		"procurement_cost_cash_rate":              r.cashRate.String(),
		"procurement_cost_formula":                formula,
	}
	for key, value := range extra {
		metadata[key] = value
	}
	return derivedCost{amount: cost.String(), currency: r.currency, evidence: evidence, metadata: metadata}
}

func (r *costResolver) resolveTargetGroupBinding(eventGroup, modelName string) (costBinding, *costResolutionFailure) {
	candidates := make([]costBinding, 0)
	for _, policy := range r.policies {
		if !strings.EqualFold(strings.TrimSpace(policy.TargetGroup), strings.TrimSpace(eventGroup)) || !matchesModelPattern(modelName, policy.SourceModelPattern) {
			continue
		}
		supplier, ok := r.suppliers[policy.SupplierID]
		if ok {
			candidates = append(candidates, costBinding{policy: policy, supplier: supplier})
		}
	}
	candidates = uniqueCostBindings(candidates, eventGroup)
	if len(candidates) == 0 {
		return costBinding{}, newCostFailure("channel_policy_binding_missing",
			"target channel was not read back and its target group has no enabled pricing or mirror policy", nil)
	}
	if len(candidates) != 1 {
		return costBinding{}, newCostFailure("channel_policy_binding_conflict",
			"target channel was not read back and its target group has conflicting supplier bindings",
			map[string]string{"candidate_count": strconv.Itoa(len(candidates))})
	}
	candidates[0].method = "target_group_mirror_without_channel"
	return candidates[0], nil
}

func (r *costResolver) resolveChannelBinding(channel domain.TargetChannel, eventGroup, modelName string) (costBinding, *costResolutionFailure) {
	channelGroups := splitTargetGroups(channel.Group)
	candidates := make([]costBinding, 0)
	for _, policy := range r.policies {
		if !containsFold(channelGroups, policy.TargetGroup) || !matchesModelPattern(modelName, policy.SourceModelPattern) {
			continue
		}
		supplier, ok := r.suppliers[policy.SupplierID]
		if ok {
			candidates = append(candidates, costBinding{policy: policy, supplier: supplier})
		}
	}
	if len(candidates) == 0 {
		return costBinding{}, newCostFailure("channel_policy_binding_missing",
			"target channel has no enabled pricing policy that identifies its supplier source group for this model", nil)
	}
	exact := make([]costBinding, 0, len(candidates))
	channelOrigin, channelOriginErr := canonicalOrigin(channel.BaseURL)
	if channelOriginErr == nil {
		for _, candidate := range candidates {
			if candidate.supplier.canonical != "" && candidate.supplier.canonical == channelOrigin {
				candidate.method = "exact_channel_supplier_origin"
				exact = append(exact, candidate)
			}
		}
	}
	if len(exact) > 0 {
		candidates = exact
	}
	candidates = uniqueCostBindings(candidates, eventGroup)
	if len(candidates) == 1 {
		if candidates[0].method == "" {
			candidates[0].method = "unique_channel_policy_binding"
		}
		return candidates[0], nil
	}
	eventMatches := make([]costBinding, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.policy.TargetGroup), strings.TrimSpace(eventGroup)) {
			candidate.method = "event_group_policy_disambiguation"
			eventMatches = append(eventMatches, candidate)
		}
	}
	eventMatches = uniqueCostBindings(eventMatches, eventGroup)
	if len(eventMatches) == 1 {
		return eventMatches[0], nil
	}
	return costBinding{}, newCostFailure("channel_policy_binding_conflict",
		"target channel is associated with conflicting supplier source-group policies", map[string]string{"candidate_count": strconv.Itoa(len(candidates))})
}

func uniqueCostBindings(values []costBinding, eventGroup string) []costBinding {
	byKey := map[string][]costBinding{}
	keys := make([]string, 0)
	for _, value := range values {
		key := fmt.Sprintf("%d\x00%s", value.supplier.supplier.ID, strings.ToLower(strings.TrimSpace(value.policy.SourceGroup)))
		if _, exists := byKey[key]; !exists {
			keys = append(keys, key)
		}
		byKey[key] = append(byKey[key], value)
	}
	sort.Strings(keys)
	result := make([]costBinding, 0, len(keys))
	for _, key := range keys {
		group := byKey[key]
		sort.Slice(group, func(i, j int) bool {
			leftEvent := strings.EqualFold(strings.TrimSpace(group[i].policy.TargetGroup), strings.TrimSpace(eventGroup))
			rightEvent := strings.EqualFold(strings.TrimSpace(group[j].policy.TargetGroup), strings.TrimSpace(eventGroup))
			if leftEvent != rightEvent {
				return leftEvent
			}
			leftEquivalent := group[i].policy.SourceBasis == "equivalent_cost"
			rightEquivalent := group[j].policy.SourceBasis == "equivalent_cost"
			if leftEquivalent != rightEquivalent {
				return leftEquivalent
			}
			return group[i].policy.ID < group[j].policy.ID
		})
		result = append(result, group[0])
	}
	return result
}

func (r *costResolver) resolvePolicyCostBasis(binding costBinding, sourceGroup domain.GroupOffer) (policyCostBasis, *costResolutionFailure) {
	if r.policyCostDone[binding.policy.ID] {
		basis, ok := r.policyCost[binding.policy.ID]
		if !ok {
			return policyCostBasis{}, newCostFailure("policy_cost_basis_unavailable",
				"pricing policy does not have enough catalog evidence for a ratio-based procurement cost", nil)
		}
		return basis, nil
	}
	r.policyCostDone[binding.policy.ID] = true
	if binding.policy.SourceBasis == "group_ratio" {
		ratio, err := nonnegativeCostDecimal(ratios.DecisionRatio(sourceGroup))
		if err != nil {
			return policyCostBasis{}, newCostFailure("source_group_ratio_invalid", "source group effective ratio is invalid", nil)
		}
		basis := policyCostBasis{ratio: ratio, method: "effective_source_group_ratio", coverage: "1", confidence: "high"}
		r.policyCost[binding.policy.ID] = basis
		return basis, nil
	}
	if binding.policy.SourceBasis != "equivalent_cost" {
		return policyCostBasis{}, newCostFailure("policy_source_basis_unsupported", "pricing policy source basis is unsupported for procurement cost", nil)
	}
	models, err := matchPolicyModels(sourceGroup, binding.policy.SourceModelPattern)
	if err != nil {
		return policyCostBasis{}, newCostFailure("policy_model_pattern_unresolved", err.Error(), nil)
	}
	impact, err := pricing.ResolvePricingImpactForTargetGroup(binding.supplier.snapshot, r.targetCatalog, sourceGroup,
		binding.policy.TargetGroup, models)
	if err != nil {
		return policyCostBasis{}, newCostFailure("policy_equivalent_cost_unavailable", err.Error(), nil)
	}
	if !policyImpactAllowed(binding.policy, impact) {
		return policyCostBasis{}, newCostFailure("policy_equivalent_cost_coverage_insufficient",
			fmt.Sprintf("pricing impact coverage %s does not satisfy policy mode %s", impact.CoverageRatio, binding.policy.CoverageMode), nil)
	}
	ratio, err := nonnegativeCostDecimal(impact.EquivalentCostRatio)
	if err != nil {
		return policyCostBasis{}, newCostFailure("policy_equivalent_cost_invalid", "policy equivalent cost ratio is invalid", nil)
	}
	basis := policyCostBasis{ratio: ratio, method: "policy_equivalent_cost_upper_bound", coverage: impact.CoverageRatio,
		confidence: impact.Confidence, warning: strings.Join(impact.Warnings, "; ")}
	r.policyCost[binding.policy.ID] = basis
	return basis, nil
}

func policyImpactAllowed(policy domain.PricingPolicy, impact pricing.PricingImpact) bool {
	switch strings.ToLower(strings.TrimSpace(policy.CoverageMode)) {
	case "strict":
		return impact.Complete
	case "owner_override":
		return strings.TrimSpace(impact.EquivalentCostRatio) != ""
	default:
		minimum, err := decimal.NewFromString(strings.TrimSpace(policy.MinimumCoverage))
		coverage, coverageErr := decimal.NewFromString(strings.TrimSpace(impact.CoverageRatio))
		return err == nil && coverageErr == nil && !minimum.IsNegative() && coverage.GreaterThanOrEqual(minimum)
	}
}

func matchPolicyModels(group domain.GroupOffer, rawPattern string) ([]string, error) {
	patterns := strings.Split(rawPattern, ",")
	matched := make([]string, 0, len(group.ModelIDs))
	for _, model := range group.ModelIDs {
		for _, raw := range patterns {
			candidate := strings.ToLower(strings.TrimSpace(raw))
			if candidate == "" {
				continue
			}
			if candidate == "*" {
				matched = append(matched, model)
				break
			}
			ok, err := path.Match(candidate, strings.ToLower(model))
			if err != nil {
				return nil, fmt.Errorf("invalid source model pattern %q: %w", candidate, err)
			}
			if ok {
				matched = append(matched, model)
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("source model pattern %q does not match source group %q", rawPattern, group.Name)
	}
	sort.Strings(matched)
	return matched, nil
}

func matchesModelPattern(model, rawPattern string) bool {
	patterns := strings.Split(rawPattern, ",")
	for _, raw := range patterns {
		candidate := strings.ToLower(strings.TrimSpace(raw))
		if candidate == "" {
			continue
		}
		if candidate == "*" {
			return true
		}
		ok, err := path.Match(candidate, strings.ToLower(strings.TrimSpace(model)))
		if err == nil && ok {
			return true
		}
	}
	return false
}

func splitTargetGroups(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' })
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

func findCatalogGroup(snapshot domain.CatalogSnapshot, identity string) (domain.GroupOffer, bool) {
	for _, group := range snapshot.Groups {
		if strings.EqualFold(strings.TrimSpace(group.ExternalID), strings.TrimSpace(identity)) ||
			strings.EqualFold(strings.TrimSpace(group.Name), strings.TrimSpace(identity)) {
			return group, true
		}
	}
	return domain.GroupOffer{}, false
}

func findCatalogModel(snapshot domain.CatalogSnapshot, modelName string, group domain.GroupOffer) (domain.ModelOffer, bool) {
	matches := make([]domain.ModelOffer, 0)
	for _, candidate := range snapshot.Models {
		if strings.EqualFold(strings.TrimSpace(candidate.ExternalID), strings.TrimSpace(modelName)) ||
			strings.EqualFold(strings.TrimSpace(candidate.Name), strings.TrimSpace(modelName)) {
			matches = append(matches, candidate)
		}
	}
	for _, candidate := range matches {
		if strings.EqualFold(strings.TrimSpace(candidate.Group), strings.TrimSpace(group.ExternalID)) ||
			strings.EqualFold(strings.TrimSpace(candidate.Group), strings.TrimSpace(group.Name)) {
			return candidate, true
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return domain.ModelOffer{}, false
}

func calculateModelCost(function domain.PriceFunction, event store.OperatingLedgerEvent, row map[string]string) (decimal.Decimal, string, bool, string) {
	// NewAPI prompt token totals can include cache reads. Without a source-specific
	// inclusion contract, applying both the input and cache prices would double
	// charge those tokens. Complex or mixed functions therefore use the approved
	// policy-ratio fallback instead of producing false request-level precision.
	switch {
	case function.RequiresModelBasePrice:
		return decimal.Zero, "", false, "price_function_requires_model_base_price"
	case strings.TrimSpace(function.Expression) != "":
		return decimal.Zero, "", false, "billing_expression_requires_unavailable_dimensions"
	case len(function.Dimensions) > 0:
		return decimal.Zero, "", false, "custom_price_dimensions_unsupported"
	case anyPriceDimension(function.CacheReadPer1M, function.CacheWritePer1M,
		function.CacheWrite5MPer1M, function.CacheWrite1HPer1M):
		return decimal.Zero, "", false, "cache_token_inclusion_contract_missing"
	}

	requestCount := decimal.NewFromInt(event.Requests)
	if requestCount.IsZero() {
		requestCount = decimal.NewFromInt(1)
	}
	fixed := []struct {
		name, price string
		keys        []string
	}{
		{"per_request", function.PerRequest, nil}, {"fixed_fee", function.FixedFee, nil},
		{"per_second", function.PerSecond, []string{"seconds", "second", "duration_seconds", "duration"}},
		{"per_image", function.PerImage, []string{"image_count", "images"}},
		{"per_frame", function.PerFrame, []string{"frame_count", "frames"}},
	}
	configuredFixed := make([]struct {
		name, price string
		keys        []string
	}, 0, 1)
	for _, dimension := range fixed {
		if strings.TrimSpace(dimension.price) != "" {
			configuredFixed = append(configuredFixed, dimension)
		}
	}
	if len(configuredFixed) > 1 {
		return decimal.Zero, "", false, "mixed_fixed_price_dimensions"
	}
	if len(configuredFixed) == 1 {
		if anyPriceDimension(function.InputPer1M, function.OutputPer1M, function.ImageInputPer1M,
			function.AudioInputPer1M, function.AudioOutputPer1M) {
			if configuredFixed[0].name == "per_request" {
				return decimal.Zero, "", false, "mixed_per_request_and_dimension_pricing"
			}
			return decimal.Zero, "", false, "mixed_fixed_and_token_pricing"
		}
		dimension := configuredFixed[0]
		value, err := decimal.NewFromString(strings.TrimSpace(dimension.price))
		if err != nil || value.IsNegative() {
			return decimal.Zero, "", false, dimension.name + "_price_invalid"
		}
		quantity := requestCount
		if len(dimension.keys) > 0 {
			var found bool
			quantity, found = decimalValue(row, dimension.keys...)
			if !found || quantity.IsNegative() {
				return decimal.Zero, "", false, "request_dimension_missing:" + dimension.name
			}
		}
		return value.Mul(quantity), fmt.Sprintf("%s(%s) * quantity(%s)", dimension.name, value.String(), quantity.String()), true, ""
	}
	dimensions := []struct {
		name, price string
		keys        []string
	}{
		{"input_per_1m", function.InputPer1M, []string{"prompt_tokens", "input_tokens"}},
		{"output_per_1m", function.OutputPer1M, []string{"completion_tokens", "output_tokens"}},
		{"image_input_per_1m", function.ImageInputPer1M, []string{"image_input_tokens"}},
		{"audio_input_per_1m", function.AudioInputPer1M, []string{"audio_input_tokens"}},
		{"audio_output_per_1m", function.AudioOutputPer1M, []string{"audio_output_tokens"}},
	}
	total := decimal.Zero
	used := 0
	formula := make([]string, 0, len(dimensions))
	for _, dimension := range dimensions {
		if strings.TrimSpace(dimension.price) == "" {
			continue
		}
		priceValue, err := decimal.NewFromString(strings.TrimSpace(dimension.price))
		if err != nil || priceValue.IsNegative() {
			return decimal.Zero, "", false, "price_dimension_invalid:" + dimension.name
		}
		value, found := decimalValue(row, dimension.keys...)
		if !found || value.IsNegative() {
			return decimal.Zero, "", false, "request_dimension_missing:" + dimension.name
		}
		total = total.Add(priceValue.Mul(value).Div(decimal.NewFromInt(1_000_000)))
		formula = append(formula, fmt.Sprintf("%s(%s) * %s / 1000000", dimension.name, priceValue.String(), value.String()))
		used++
	}
	if used == 0 {
		return decimal.Zero, "", false, "no_supported_price_dimension"
	}
	return total, strings.Join(formula, " + "), true, ""
}

func anyPriceDimension(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func decimalValue(row map[string]string, keys ...string) (decimal.Decimal, bool) {
	for _, key := range keys {
		if raw := strings.TrimSpace(row[key]); raw != "" {
			value, err := decimal.NewFromString(raw)
			if err == nil {
				return value, true
			}
		}
	}
	return decimal.Zero, false
}

func positiveCostDecimal(raw string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.Zero, errors.New("value is not a positive decimal")
	}
	return value, nil
}

func nonnegativeCostDecimal(raw string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return decimal.Zero, errors.New("value is not a non-negative decimal")
	}
	return value, nil
}

func canonicalOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid origin")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery, parsed.Fragment = "", ""
	parsed.Path = strings.TrimRight(parsed.EscapedPath(), "/")
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path, nil
}

func parseID(raw string) int64 {
	value, _ := decimal.NewFromString(strings.TrimSpace(raw))
	return value.IntPart()
}
