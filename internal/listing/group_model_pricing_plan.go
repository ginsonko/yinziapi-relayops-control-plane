package listing

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const defaultModelPriceRoundingStep = "0.000000000001"

type GroupModelPricingPlanInput struct {
	SupplierID       int64
	SourceCatalog    domain.CatalogSnapshot
	TargetCatalog    domain.CatalogSnapshot
	SourceGroup      domain.GroupOffer
	Models           []string
	TargetGroup      string
	TargetGroupRatio string
	PricingRule      domain.ListingPricingRule
	Expected         domain.GroupModelPricingContract
	OwnershipSource  string
}

var tokenDimensionOrder = []string{
	"input_per_1m", "output_per_1m", "cache_read_per_1m", "cache_write_per_1m",
	"cache_write_5m_per_1m", "cache_write_1h_per_1m", "image_input_per_1m",
	"audio_input_per_1m", "audio_output_per_1m",
}

func PlanGroupModelPricing(input GroupModelPricingPlanInput) (domain.ListingGroupModelPricingPlan, error) {
	requestedModels := sortedUnique(input.Models)
	if len(requestedModels) == 0 {
		return domain.ListingGroupModelPricingPlan{}, errors.New("group-model pricing requires at least one selected model")
	}
	if strings.TrimSpace(input.TargetGroup) == "" {
		return domain.ListingGroupModelPricingPlan{}, errors.New("target group is required")
	}
	if err := validateGroupModels(input.SourceGroup, requestedModels); err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	manualExcluded := intersectModels(requestedModels, input.PricingRule.MirrorExcludedModels)
	models := subtractModels(requestedModels, manualExcluded)
	if len(models) == 0 {
		return domain.ListingGroupModelPricingPlan{}, errors.New("所选模型全部位于例外清单，当前分组没有可上架模型")
	}
	resolvedCatalog, err := resolveMissingSourceBasePrices(input, models)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	input.SourceCatalog = resolvedCatalog
	models, incompatibleModels, exclusionReasons := filterTargetContractModels(input, models)
	excludedModels := sortedUnique(append(append([]string(nil), manualExcluded...), incompatibleModels...))
	for _, model := range manualExcluded {
		exclusionReasons[model] = "管理员例外模型"
	}
	if len(models) == 0 {
		return domain.ListingGroupModelPricingPlan{}, fmt.Errorf("所选模型均无法由本站计费合同安全表示，已全部排除：%s", strings.Join(excludedModels, "、"))
	}
	priceAnchor, err := listingPriceAnchor(input.PricingRule)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	groupRule, err := listingGroupRatioRule(input.PricingRule, priceAnchor)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	modelRule, err := listingModelPriceRule(input.PricingRule, priceAnchor)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	targetGroupRatio, err := positivePlanDecimal(input.TargetGroupRatio, "target group ratio")
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	sourceCashGroupRatio, err := nonnegativePlanDecimal(ratios.DecisionRatio(input.SourceGroup), "source cash group ratio")
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	rawSourceRatio := firstPlanValue(input.SourceGroup.RawRatio, input.SourceGroup.Ratio, ratios.DecisionRatio(input.SourceGroup))
	rechargeFactor := firstPlanValue(input.SourceGroup.RechargeValueFactor, ratios.DefaultRechargeValueFactor)
	if _, err := positivePlanDecimal(rechargeFactor, "recharge value factor"); err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	roundingStepRaw := firstPlanValue(input.PricingRule.ModelPriceRoundingStep, defaultModelPriceRoundingStep)
	roundingStep, err := positivePlanDecimal(roundingStepRaw, "model price rounding step")
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	if roundingStep.Exponent() < -12 {
		return domain.ListingGroupModelPricingPlan{}, errors.New("model price rounding step supports at most 12 decimal places")
	}
	if input.PricingRule.OnlyFollowIncrease || input.PricingRule.OnlyFollowDecrease {
		return domain.ListingGroupModelPricingPlan{}, errors.New("one-click group-model listing cannot use follow-direction constraints without a complete current per-model sale vector")
	}

	expected, err := cloneGroupModelContract(input.Expected)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	if expected.SchemaVersion == 0 {
		expected.SchemaVersion = 1
	}
	if expected.Groups == nil {
		expected.Groups = map[string]map[string]domain.GroupModelPricingSetting{}
	}
	desired, err := cloneGroupModelContract(expected)
	if err != nil {
		return domain.ListingGroupModelPricingPlan{}, err
	}
	if desired.Groups[input.TargetGroup] == nil {
		desired.Groups[input.TargetGroup] = map[string]domain.GroupModelPricingSetting{}
	}

	policy := listingModelPricePolicy(input.PricingRule, modelRule)
	policy.RoundingStep = defaultModelPriceRoundingStep
	if priceAnchor == mirror.PriceAnchorSourceBase {
		targetGroupRatio, err = sourceBaseTargetGroupRatio(input, models, groupRule, modelRule, roundingStep)
		if err != nil {
			return domain.ListingGroupModelPricingPlan{}, err
		}
	}
	evidence := make([]domain.ListingModelPricingEvidence, 0, len(models))
	minimumMargin := decimal.NewFromInt(1)
	minimumMarginDefined := false
	currency := ""
	for _, modelName := range models {
		model, modelErr := sourceModelForGroup(input.SourceCatalog.Models, input.SourceGroup, modelName)
		if modelErr != nil {
			return domain.ListingGroupModelPricingPlan{}, fmt.Errorf("source model %q: %w", modelName, modelErr)
		}
		modelCurrency := strings.ToUpper(strings.TrimSpace(model.PriceFunction.Currency))
		if modelCurrency == "" {
			return domain.ListingGroupModelPricingPlan{}, fmt.Errorf("source model %q has no declared currency", modelName)
		}
		if currency == "" {
			currency = modelCurrency
		} else if currency != modelCurrency {
			return domain.ListingGroupModelPricingPlan{}, fmt.Errorf("selected models mix currencies %s and %s without a conversion rule", currency, modelCurrency)
		}
		modelEvidence, modelErr := planOneModel(model, modelName, input, priceAnchor, modelRule, policy,
			sourceCashGroupRatio, targetGroupRatio, roundingStep, rawSourceRatio, rechargeFactor)
		if modelErr != nil {
			return domain.ListingGroupModelPricingPlan{}, fmt.Errorf("model %q: %w", modelName, modelErr)
		}
		if current, exists := expected.Groups[input.TargetGroup][modelName]; exists {
			modelEvidence.Setting = preserveUnchangedModelProvenance(input.TargetGroup, modelName, current, modelEvidence.Setting)
		}
		desired.Groups[input.TargetGroup][modelName] = modelEvidence.Setting
		evidence = append(evidence, modelEvidence)
		if modelEvidence.MinimumMarginDefined {
			margin, _ := decimal.NewFromString(modelEvidence.MinimumMarginRate)
			if !minimumMarginDefined || margin.LessThan(minimumMargin) {
				minimumMargin = margin
			}
			minimumMarginDefined = true
		}
	}
	desired.Revision = targets.HashGroupModelPricingContract(desired)
	minimumMarginRaw := ""
	if minimumMarginDefined {
		minimumMarginRaw = minimumMargin.String()
	}
	return domain.ListingGroupModelPricingPlan{
		Mode: "atomic_contract", Supported: true, Exact: true, RoundingStep: roundingStep.String(), PriceAnchor: priceAnchor,
		GroupRatioRuleType: groupRule.Type, GroupRatioRuleValue: groupRule.Value,
		ModelPriceRuleType: modelRule.Type, ModelPriceRuleValue: modelRule.Value,
		TargetGroup: input.TargetGroup, TargetGroupRatio: targetGroupRatio.String(), ExpectedRevision: expected.Revision,
		DesiredRevision: desired.Revision, ExpectedContract: expected, DesiredContract: desired,
		Models: evidence, ExcludedModels: excludedModels, ExclusionReasons: exclusionReasons,
		MinimumMarginRate: minimumMarginRaw, MinimumMarginDefined: minimumMarginDefined,
	}, nil
}

func filterTargetContractModels(input GroupModelPricingPlanInput, models []string) ([]string, []string, map[string]string) {
	compatible := make([]string, 0, len(models))
	excluded := make([]string, 0)
	reasons := make(map[string]string)
	for _, modelName := range models {
		model, err := sourceModelForGroup(input.SourceCatalog.Models, input.SourceGroup, modelName)
		if err == nil && strings.TrimSpace(model.PriceFunction.Currency) == "" {
			err = errors.New("上游没有声明价格币种")
		}
		if err == nil {
			if reason := strings.TrimSpace(model.Metadata["pricing_ambiguity"]); reason != "" {
				err = fmt.Errorf("上游价格存在歧义：%s", reason)
			}
		}
		if err == nil && (strings.EqualFold(strings.TrimSpace(model.BillingMode), "tiered_expr") ||
			strings.TrimSpace(model.PriceFunction.Expression) != "") {
			err = errors.New("阶梯或表达式价格无法写入本站固定计费合同")
		}
		if err == nil {
			var dimensions map[string]string
			dimensions, err = pricing.MonetaryDimensions(model)
			if err == nil {
				_, _, _, _, err = classifyPlanDimensions(model, dimensions)
			}
		}
		if err != nil {
			excluded = append(excluded, modelName)
			reasons[modelName] = err.Error()
			continue
		}
		compatible = append(compatible, modelName)
	}
	return compatible, excluded, reasons
}

func preserveUnchangedModelProvenance(group, model string, current,
	desired domain.GroupModelPricingSetting) domain.GroupModelPricingSetting {
	if strings.TrimSpace(current.Source) != strings.TrimSpace(desired.Source) ||
		(strings.TrimSpace(current.SourceRevision) == "" && current.SourceObservedAtUnixMS == 0) {
		return desired
	}
	currentHash, currentErr := mirror.TargetModelFunctionalHash(group, model, current)
	desiredHash, desiredErr := mirror.TargetModelFunctionalHash(group, model, desired)
	if currentErr != nil || desiredErr != nil || currentHash != desiredHash {
		return desired
	}
	desired.SourceRevision = current.SourceRevision
	desired.SourceObservedAtUnixMS = current.SourceObservedAtUnixMS
	return desired
}

func planMissingGlobalModelPrices(contract domain.ListingGroupModelPricingPlan,
	target domain.TargetSnapshot) (domain.ListingGlobalModelPricePlan, error) {
	option, supported := target.PricingOptions["ModelPrice"]
	if !supported {
		return domain.ListingGlobalModelPricePlan{}, nil
	}
	expected := clonePlanDecimalMap(option.Values)
	desired := clonePlanDecimalMap(option.Values)
	added := make(map[string]string)
	for _, model := range contract.Models {
		setting := model.Setting
		if setting.BillingMode != "fixed_price" || setting.ModelPrice == nil || setting.TaskProfile == nil {
			continue
		}
		unit := strings.TrimSpace(setting.TaskProfile.Unit)
		if unit != "per_request" && unit != "fixed_duration" {
			// A global ModelPrice has no duration context. Per-second prices stay
			// exclusively in the per-group task contract.
			continue
		}
		if _, exists := desired[model.Model]; exists {
			continue
		}
		value, err := nonnegativePlanDecimal(*setting.ModelPrice, "global model price")
		if err != nil {
			return domain.ListingGlobalModelPricePlan{}, fmt.Errorf("model %q: %w", model.Model, err)
		}
		desired[model.Model] = value.String()
		added[model.Model] = value.String()
	}
	expectedHash := targets.HashDecimalMap(expected)
	desiredHash := targets.HashDecimalMap(desired)
	if option.Hash != "" && option.Hash != expectedHash {
		return domain.ListingGlobalModelPricePlan{}, errors.New("target ModelPrice option hash is internally inconsistent")
	}
	return domain.ListingGlobalModelPricePlan{Supported: true, Changed: desiredHash != expectedHash,
		ExpectedHash: expectedHash, DesiredHash: desiredHash, Expected: expected, Desired: desired, Added: added}, nil
}

func clonePlanDecimalMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func resolveMissingSourceBasePrices(input GroupModelPricingPlanInput, selected []string) (domain.CatalogSnapshot, error) {
	result := input.SourceCatalog
	result.Models = append([]domain.ModelOffer(nil), input.SourceCatalog.Models...)
	wanted := make(map[string]struct{}, len(selected))
	for _, model := range selected {
		wanted[strings.TrimSpace(model)] = struct{}{}
	}
	groupIDs := map[string]struct{}{
		strings.TrimSpace(input.SourceGroup.ExternalID): {},
		strings.TrimSpace(input.SourceGroup.Name):       {},
	}
	for index := range result.Models {
		model := result.Models[index]
		name := strings.TrimSpace(firstPlanValue(model.ExternalID, model.Name))
		if _, ok := wanted[name]; !ok || !model.PriceFunction.RequiresModelBasePrice {
			continue
		}
		scope := strings.TrimSpace(model.Group)
		if scope != "" {
			if _, ok := groupIDs[scope]; !ok {
				continue
			}
		}
		fallback, found, fallbackErr := uniqueTargetBasePrice(input.TargetCatalog, input.TargetGroup, name)
		if fallbackErr != nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("source model %q has no explicit base price: %w", name, fallbackErr)
		}
		if !found {
			continue
		}
		result.Models[index] = mergeTargetBasePrice(model, fallback)
	}
	return result, nil
}

func uniqueTargetBasePrice(catalog domain.CatalogSnapshot, targetGroup, modelName string) (domain.ModelOffer, bool, error) {
	exactGroup := make([]domain.ModelOffer, 0, 1)
	all := make([]domain.ModelOffer, 0, 2)
	for _, candidate := range catalog.Models {
		if !sameModelName(candidate, modelName) || candidate.PriceFunction.RequiresModelBasePrice {
			continue
		}
		if _, err := pricing.MonetaryDimensions(candidate); err != nil {
			continue
		}
		all = append(all, candidate)
		if strings.EqualFold(strings.TrimSpace(candidate.Group), strings.TrimSpace(targetGroup)) {
			exactGroup = append(exactGroup, candidate)
		}
	}
	candidates := exactGroup
	if len(candidates) == 0 {
		candidates = all
	}
	if len(candidates) == 0 {
		return domain.ModelOffer{}, false, nil
	}
	firstSignature, err := targetBasePriceSignature(candidates[0])
	if err != nil {
		return domain.ModelOffer{}, false, err
	}
	for _, candidate := range candidates[1:] {
		signature, signatureErr := targetBasePriceSignature(candidate)
		if signatureErr != nil {
			return domain.ModelOffer{}, false, signatureErr
		}
		if signature != firstSignature {
			return domain.ModelOffer{}, false, errors.New("multiple conflicting target base prices exist; an upstream model price is required")
		}
	}
	return candidates[0], true, nil
}

func targetBasePriceSignature(model domain.ModelOffer) (string, error) {
	dimensions, err := pricing.MonetaryDimensions(model)
	if err != nil {
		return "", err
	}
	_, unit, duration, ordered, err := classifyPlanDimensions(model, dimensions)
	if err != nil {
		return "", err
	}
	values := make([]string, 0, len(ordered))
	for _, dimension := range ordered {
		value, valueErr := nonnegativePlanDecimal(dimensions[dimension], dimension)
		if valueErr != nil {
			return "", valueErr
		}
		values = append(values, dimension+"="+value.String())
	}
	return strings.Join([]string{strings.ToUpper(strings.TrimSpace(model.PriceFunction.Currency)), unit,
		strconv.Itoa(duration), strings.Join(values, "|")}, "\x1f"), nil
}

func sameModelName(model domain.ModelOffer, name string) bool {
	name = strings.TrimSpace(name)
	return strings.EqualFold(strings.TrimSpace(model.ExternalID), name) || strings.EqualFold(strings.TrimSpace(model.Name), name)
}

func mergeTargetBasePrice(source, fallback domain.ModelOffer) domain.ModelOffer {
	groupMultiplier := source.PriceFunction.GroupMultiplier
	metadata := make(map[string]string, len(source.Metadata)+3)
	for key, value := range source.Metadata {
		metadata[key] = value
	}
	for _, key := range []string{"billing_unit", "fixed_duration_seconds", "duration_seconds"} {
		if value := strings.TrimSpace(fallback.Metadata[key]); value != "" {
			metadata[key] = value
		}
	}
	metadata["base_price_fallback"] = "existing_target_model_price"
	source.BillingMode = fallback.BillingMode
	source.ModelRatio = fallback.ModelRatio
	source.CompletionRatio = fallback.CompletionRatio
	source.CacheRatio = fallback.CacheRatio
	source.ModelPrice = fallback.ModelPrice
	source.BillingExpr = fallback.BillingExpr
	source.Currency = fallback.Currency
	source.PriceFunction = fallback.PriceFunction
	source.PriceFunction.GroupMultiplier = groupMultiplier
	source.PriceFunction.RequiresModelBasePrice = false
	source.PriceFunction.Basis = "existing_target_model_base"
	source.PriceFunction.Derivation = "upstream exposed only a group multiplier; preserved the existing unique target model base price"
	source.Metadata = metadata
	return source
}

func listingPriceAnchor(rule domain.ListingPricingRule) (string, error) {
	anchor := strings.ToLower(strings.TrimSpace(rule.PriceAnchor))
	if anchor == "" {
		// Missing means the R27 behavior for stored previews and older clients.
		// New complete-mirror/listing UIs send source_base explicitly.
		anchor = mirror.PriceAnchorEffectiveSale
	}
	if anchor != mirror.PriceAnchorSourceBase && anchor != mirror.PriceAnchorEffectiveSale {
		return "", fmt.Errorf("unsupported price_anchor %q", rule.PriceAnchor)
	}
	return anchor, nil
}

func listingGroupRatioRule(rule domain.ListingPricingRule, anchor string) (mirror.Rule, error) {
	explicitType := strings.ToLower(strings.TrimSpace(rule.GroupRatioRuleType))
	if anchor == mirror.PriceAnchorEffectiveSale && explicitType == "" {
		return mirror.Rule{Type: "same"}, nil
	}
	ruleType := explicitType
	value := strings.TrimSpace(rule.GroupRatioRuleValue)
	if ruleType == "" {
		ruleType = strings.ToLower(strings.TrimSpace(rule.RuleType))
		value = strings.TrimSpace(rule.Value)
	}
	return validateListingMirrorRule(mirror.Rule{Type: ruleType, Value: value}, true)
}

func listingModelPriceRule(rule domain.ListingPricingRule, anchor string) (mirror.Rule, error) {
	ruleType := strings.ToLower(strings.TrimSpace(rule.ModelPriceRuleType))
	value := strings.TrimSpace(rule.ModelPriceRuleValue)
	if ruleType == "" {
		if anchor == mirror.PriceAnchorSourceBase {
			ruleType = "same"
			value = ""
		} else {
			ruleType = strings.ToLower(strings.TrimSpace(rule.RuleType))
			value = strings.TrimSpace(rule.Value)
		}
	}
	return validateListingMirrorRule(mirror.Rule{Type: ruleType, Value: value}, anchor == mirror.PriceAnchorEffectiveSale)
}

func validateListingMirrorRule(rule mirror.Rule, allowEconomic bool) (mirror.Rule, error) {
	rule.Type = strings.ToLower(strings.TrimSpace(rule.Type))
	rule.Value = strings.TrimSpace(rule.Value)
	if rule.Type == "" {
		return mirror.Rule{}, errors.New("pricing transform type is required")
	}
	switch rule.Type {
	case "same":
		if rule.Value != "" {
			return mirror.Rule{}, errors.New("same pricing transform cannot contain a value")
		}
		return rule, nil
	case "add", "multiply", "fixed":
	case "target_margin", "markup", "subsidy_rate":
		if !allowEconomic {
			return mirror.Rule{}, fmt.Errorf("model price transform %q is not valid for source_base", rule.Type)
		}
	default:
		return mirror.Rule{}, fmt.Errorf("unsupported pricing transform %q", rule.Type)
	}
	if rule.Value == "" {
		return mirror.Rule{}, fmt.Errorf("pricing transform %q requires a decimal value", rule.Type)
	}
	if _, err := decimal.NewFromString(rule.Value); err != nil {
		return mirror.Rule{}, fmt.Errorf("pricing transform %q requires a decimal value", rule.Type)
	}
	return rule, nil
}

func listingModelPricePolicy(rule domain.ListingPricingRule, modelRule mirror.Rule) domain.PricingPolicy {
	policy := listingRulePolicy(rule)
	policy.RuleType = modelRule.Type
	policy.Value = modelRule.Value
	return policy
}

func listingGroupRatioPolicy(rule domain.ListingPricingRule, groupRule mirror.Rule) domain.PricingPolicy {
	policy := listingRulePolicy(rule)
	if groupRule.Type == "same" {
		policy.RuleType = "multiply"
		policy.Value = "1"
	} else {
		policy.RuleType = groupRule.Type
		policy.Value = groupRule.Value
	}
	return policy
}

func sourceBaseTargetGroupRatio(input GroupModelPricingPlanInput, models []string, groupRule, modelRule mirror.Rule,
	modelRoundingStep decimal.Decimal) (decimal.Decimal, error) {
	maximum := decimal.Zero
	for _, modelName := range models {
		model, err := sourceModelForGroup(input.SourceCatalog.Models, input.SourceGroup, modelName)
		if err != nil {
			return decimal.Zero, fmt.Errorf("source model %q: %w", modelName, err)
		}
		if reason := strings.TrimSpace(model.Metadata["pricing_ambiguity"]); reason != "" {
			return decimal.Zero, fmt.Errorf("source model %q pricing is ambiguous: %s", modelName, reason)
		}
		if strings.EqualFold(strings.TrimSpace(model.BillingMode), "tiered_expr") || strings.TrimSpace(model.PriceFunction.Expression) != "" {
			return decimal.Zero, fmt.Errorf("source model %q uses an unrepresentable tiered or expression price", modelName)
		}
		dimensions, err := pricing.MonetaryDimensions(model)
		if err != nil {
			return decimal.Zero, fmt.Errorf("source model %q: %w", modelName, err)
		}
		_, unit, duration, ordered, err := classifyPlanDimensions(model, dimensions)
		if err != nil {
			return decimal.Zero, fmt.Errorf("source model %q: %w", modelName, err)
		}
		result, err := computeSourceBase(input, ordered, dimensions, unit, duration, groupRule, modelRule,
			input.TargetGroupRatio, modelRoundingStep)
		if err != nil {
			return decimal.Zero, fmt.Errorf("source model %q: %w", modelName, err)
		}
		ratio, _ := decimal.NewFromString(result.TargetGroupRatio)
		if ratio.GreaterThan(maximum) {
			maximum = ratio
		}
	}
	if !maximum.IsPositive() {
		return decimal.Zero, errors.New("source_base pricing produced no positive target group ratio")
	}
	return maximum, nil
}

func computeSourceBaseModel(input GroupModelPricingPlanInput, ordered []string, dimensions map[string]string,
	unit string, duration int, modelRule mirror.Rule, targetGroupRatio, modelRoundingStep decimal.Decimal) (mirror.SourceBaseResult, error) {
	return computeSourceBase(input, ordered, dimensions, unit, duration,
		mirror.Rule{Type: "fixed", Value: targetGroupRatio.String()}, modelRule, "", modelRoundingStep)
}

func computeSourceBase(input GroupModelPricingPlanInput, ordered []string, dimensions map[string]string, unit string,
	duration int, groupRule, modelRule mirror.Rule, currentGroupRatio string,
	modelRoundingStep decimal.Decimal) (mirror.SourceBaseResult, error) {
	canonical := make([]mirror.PriceDimension, 0, len(ordered))
	originalKinds := make(map[string]string, len(ordered))
	for _, dimension := range ordered {
		canonicalKind, canonicalUnit := listingMirrorDimension(dimension, unit)
		originalKinds[canonicalKind] = dimension
		canonical = append(canonical, mirror.PriceDimension{Kind: canonicalKind, Unit: canonicalUnit,
			DurationSeconds: duration, DecimalValue: dimensions[dimension]})
	}
	profitBasis := strings.ToLower(firstPlanValue(input.PricingRule.ProfitBasis, "contribution"))
	costFactor := firstPlanValue(input.PricingRule.CostCashFactor, "1")
	revenueFactor := firstPlanValue(input.PricingRule.RevenueCashFactor, "1")
	riskRate := firstPlanValue(input.PricingRule.RiskBufferRate, "0")
	switch profitBasis {
	case "catalog", "nominal":
		costFactor, revenueFactor, riskRate = "1", "1", "0"
	case "cash":
		riskRate = "0"
	case "contribution", "risk", "risk_adjusted":
	default:
		return mirror.SourceBaseResult{}, fmt.Errorf("unsupported profit basis %q", input.PricingRule.ProfitBasis)
	}
	rawSourceRatio := firstPlanValue(input.SourceGroup.RawRatio, input.SourceGroup.Ratio)
	rechargeFactor := firstPlanValue(input.SourceGroup.RechargeValueFactor, ratios.DefaultRechargeValueFactor)
	result, err := mirror.ComputeSourceBase(mirror.SourceBaseInput{
		Dimensions: canonical,
		Factors: mirror.CashFactors{RawGroupRatio: rawSourceRatio, RechargeValueFactor: rechargeFactor,
			SourceCashFactor: costFactor, TargetRealizationFactor: revenueFactor, RiskBufferRate: riskRate},
		GroupRatioRule: groupRule, ModelPriceRule: modelRule, CurrentGroupRatio: currentGroupRatio,
		MinimumGroupRatio: input.PricingRule.MinimumRatio, MaximumGroupRatio: input.PricingRule.MaximumRatio,
		OnlyFollowIncrease: input.PricingRule.OnlyFollowIncrease, OnlyFollowDecrease: input.PricingRule.OnlyFollowDecrease,
		GroupRoundingStep: firstPlanValue(input.PricingRule.RoundingStep, "0.001"),
		ModelRoundingStep: modelRoundingStep.String(),
	})
	if err != nil {
		return mirror.SourceBaseResult{}, err
	}
	for index := range result.Dimensions {
		if original := originalKinds[result.Dimensions[index].Kind]; original != "" {
			result.Dimensions[index].Kind = original
		}
	}
	return result, nil
}

func listingMirrorDimension(dimension, billingUnit string) (string, string) {
	if billingUnit == "fixed_duration" {
		return "fixed_duration", "currency_per_fixed_duration"
	}
	switch dimension {
	case "input_per_1m", "output_per_1m", "cache_read_per_1m", "cache_write_per_1m",
		"cache_write_5m_per_1m", "cache_write_1h_per_1m", "image_input_per_1m",
		"audio_input_per_1m", "audio_output_per_1m":
		return dimension, "currency_per_1m_tokens"
	case "per_second":
		return dimension, "currency_per_second"
	case "per_request", "fixed_fee":
		return "per_request", "currency_per_request"
	default:
		return dimension, billingUnit
	}
}

func planOneModel(model domain.ModelOffer, modelName string, input GroupModelPricingPlanInput, priceAnchor string,
	modelRule mirror.Rule, policy domain.PricingPolicy, sourceCashGroupRatio, targetGroupRatio,
	roundingStep decimal.Decimal, rawSourceRatio, rechargeFactor string) (domain.ListingModelPricingEvidence, error) {
	if reason := strings.TrimSpace(model.Metadata["pricing_ambiguity"]); reason != "" {
		return domain.ListingModelPricingEvidence{}, fmt.Errorf("source pricing is ambiguous: %s", reason)
	}
	if strings.EqualFold(strings.TrimSpace(model.BillingMode), "tiered_expr") || strings.TrimSpace(model.PriceFunction.Expression) != "" {
		return domain.ListingModelPricingEvidence{}, errors.New("tiered or expression pricing cannot be transformed into a fixed margin without a request-specific quote")
	}
	dimensions, err := pricing.MonetaryDimensions(model)
	if err != nil {
		return domain.ListingModelPricingEvidence{}, err
	}
	kind, unit, duration, orderedDimensions, err := classifyPlanDimensions(model, dimensions)
	if err != nil {
		return domain.ListingModelPricingEvidence{}, err
	}
	basePrices := make(map[string]decimal.Decimal, len(orderedDimensions))
	dimensionEvidence := make([]domain.ListingPriceDimensionEvidence, 0, len(orderedDimensions))
	minimumMargin := decimal.NewFromInt(1)
	minimumMarginDefined := false
	if priceAnchor == mirror.PriceAnchorSourceBase {
		result, resultErr := computeSourceBaseModel(input, orderedDimensions, dimensions, unit, duration, modelRule,
			targetGroupRatio, roundingStep)
		if resultErr != nil {
			return domain.ListingModelPricingEvidence{}, resultErr
		}
		for _, economics := range result.Dimensions {
			dimension := economics.Kind
			sourceUnit, _ := decimal.NewFromString(economics.SourceBasePrice)
			basePrice, _ := decimal.NewFromString(economics.TargetBasePrice)
			riskCost, _ := decimal.NewFromString(economics.CashCost)
			revenue, _ := decimal.NewFromString(economics.CashRevenue)
			sourceCashCost := sourceUnit.Mul(sourceCashGroupRatio)
			effectiveSale := basePrice.Mul(targetGroupRatio)
			margin := ""
			if economics.CashMarginDefined {
				margin = economics.CashMarginRate
				parsed, _ := decimal.NewFromString(margin)
				if !minimumMarginDefined || parsed.LessThan(minimumMargin) {
					minimumMargin = parsed
				}
				minimumMarginDefined = true
			}
			basePrices[dimension] = basePrice
			dimensionEvidence = append(dimensionEvidence, domain.ListingPriceDimensionEvidence{
				Dimension: dimension, SourceUnitPrice: sourceUnit.String(), SourceCashCost: sourceCashCost.String(),
				RiskAdjustedCashCost: riskCost.String(), DesiredEffectiveSale: effectiveSale.String(), TargetBasePrice: basePrice.String(),
				EffectiveSale: effectiveSale.String(), RevenueCashValue: revenue.String(), MarginRate: margin,
				MarginDefined: economics.CashMarginDefined,
			})
		}
	} else {
		for _, dimension := range orderedDimensions {
			sourceUnit, err := nonnegativePlanDecimal(dimensions[dimension], "source unit price")
			if err != nil {
				return domain.ListingModelPricingEvidence{}, fmt.Errorf("dimension %s: %w", dimension, err)
			}
			sourceCashCost := sourceUnit.Mul(sourceCashGroupRatio)
			calculation, err := pricing.Calculate(pricing.Input{SourceRatio: sourceCashCost.String(), CostRatio: sourceCashCost.String(), Policy: policy})
			if err != nil {
				return domain.ListingModelPricingEvidence{}, fmt.Errorf("dimension %s: %w", dimension, err)
			}
			desiredSale, _ := decimal.NewFromString(calculation.TargetRatio)
			basePrice := ceilPlanToStep(desiredSale.Div(targetGroupRatio), roundingStep)
			effectiveSale := basePrice.Mul(targetGroupRatio)
			riskCost, revenue, margin, marginDefined, err := realizedPlanMargin(sourceCashCost, effectiveSale, policy)
			if err != nil {
				return domain.ListingModelPricingEvidence{}, fmt.Errorf("dimension %s: %w", dimension, err)
			}
			if marginDefined && (!minimumMarginDefined || margin.LessThan(minimumMargin)) {
				minimumMargin = margin
				minimumMarginDefined = true
			}
			basePrices[dimension] = basePrice
			marginRaw := ""
			if marginDefined {
				marginRaw = margin.String()
			}
			dimensionEvidence = append(dimensionEvidence, domain.ListingPriceDimensionEvidence{
				Dimension: dimension, SourceUnitPrice: sourceUnit.String(), SourceCashCost: sourceCashCost.String(),
				RiskAdjustedCashCost: riskCost.String(), DesiredEffectiveSale: desiredSale.String(), TargetBasePrice: basePrice.String(),
				EffectiveSale: effectiveSale.String(), RevenueCashValue: revenue.String(), MarginRate: marginRaw,
				MarginDefined: marginDefined,
			})
		}
	}
	setting, err := buildContractSetting(kind, unit, duration, basePrices)
	if err != nil {
		return domain.ListingModelPricingEvidence{}, err
	}
	setting.Source = strings.TrimSpace(input.OwnershipSource)
	if setting.Source == "" {
		setting.Source = fmt.Sprintf("relayops:supplier/%d/group/%s", input.SupplierID, input.SourceGroup.ExternalID)
	}
	setting.SourceRevision = input.SourceCatalog.Evidence.ResponseHash
	setting.SourceObservedAtUnixMS = input.SourceCatalog.Evidence.ObservedAt.UnixMilli()
	minimumMarginRaw := ""
	if minimumMarginDefined {
		minimumMarginRaw = minimumMargin.String()
	}
	return domain.ListingModelPricingEvidence{
		Model: modelName, PriceAnchor: priceAnchor, BillingMode: setting.BillingMode, BillingUnit: unit, FixedDurationSeconds: duration,
		Currency: strings.ToUpper(strings.TrimSpace(model.PriceFunction.Currency)), RawSourceGroupRatio: rawSourceRatio,
		SourceCashGroupRatio: sourceCashGroupRatio.String(), RechargeValueFactor: rechargeFactor,
		TargetGroupRatio: targetGroupRatio.String(), SourceEvidenceHash: input.SourceCatalog.Evidence.ResponseHash,
		SourceObservedAt: input.SourceCatalog.Evidence.ObservedAt, Setting: setting, Dimensions: dimensionEvidence,
		MinimumMarginRate: minimumMarginRaw, MinimumMarginDefined: minimumMarginDefined,
	}, nil
}

func classifyPlanDimensions(model domain.ModelOffer, dimensions map[string]string) (kind, unit string, duration int, ordered []string, err error) {
	tokenSet := make(map[string]struct{}, len(tokenDimensionOrder))
	for _, name := range tokenDimensionOrder {
		tokenSet[name] = struct{}{}
	}
	fixed := make([]string, 0, 1)
	token := make([]string, 0, len(dimensions))
	for dimension := range dimensions {
		if _, ok := tokenSet[dimension]; ok {
			token = append(token, dimension)
			continue
		}
		switch dimension {
		case "per_request", "per_second", "fixed_fee":
			fixed = append(fixed, dimension)
		case "per_image", "per_frame":
			return "", "", 0, nil, fmt.Errorf("dimension %s is not exactly representable by the target billing contract", dimension)
		default:
			return "", "", 0, nil, fmt.Errorf("dimension %s is not supported by the target billing contract", dimension)
		}
	}
	if len(token) > 0 && len(fixed) > 0 {
		return "", "", 0, nil, errors.New("a model cannot mix token and fixed task dimensions in one target contract entry")
	}
	if len(token) > 0 {
		if _, ok := dimensions["input_per_1m"]; !ok {
			return "", "", 0, nil, errors.New("token pricing requires input_per_1m")
		}
		if _, ok := dimensions["output_per_1m"]; !ok {
			return "", "", 0, nil, errors.New("token pricing requires output_per_1m")
		}
		for _, name := range tokenDimensionOrder {
			if _, ok := dimensions[name]; ok {
				ordered = append(ordered, name)
			}
		}
		return "ratio", "token", 0, ordered, nil
	}
	if len(fixed) != 1 {
		return "", "", 0, nil, errors.New("fixed pricing requires exactly one per_request, per_second or fixed_fee dimension")
	}
	ordered = fixed
	metadataUnit := strings.ToLower(firstPlanValue(model.Metadata["billing_unit"], model.Metadata["task_unit"]))
	duration, err = modelFixedDuration(model.Metadata)
	if err != nil {
		return "", "", 0, nil, err
	}
	switch fixed[0] {
	case "per_second":
		if duration != 0 || metadataUnit == "fixed_duration" {
			return "", "", 0, nil, errors.New("per_second pricing conflicts with fixed-duration metadata")
		}
		unit = "per_second"
	case "per_request":
		if metadataUnit == "per_second" {
			return "", "", 0, nil, errors.New("per_request pricing conflicts with per-second metadata")
		}
		if duration > 0 || metadataUnit == "fixed_duration" {
			if duration <= 0 {
				return "", "", 0, nil, errors.New("fixed-duration pricing requires fixed_duration_seconds")
			}
			unit = "fixed_duration"
		} else {
			unit = "per_request"
		}
	case "fixed_fee":
		if duration > 0 || metadataUnit == "fixed_duration" {
			if duration <= 0 {
				return "", "", 0, nil, errors.New("fixed-duration pricing requires fixed_duration_seconds")
			}
			unit = "fixed_duration"
		} else {
			unit = "per_request"
		}
	}
	return "fixed_price", unit, duration, ordered, nil
}

func buildContractSetting(kind, unit string, duration int, prices map[string]decimal.Decimal) (domain.GroupModelPricingSetting, error) {
	setting := domain.GroupModelPricingSetting{BillingMode: kind}
	if kind == "fixed_price" {
		if len(prices) != 1 {
			return setting, errors.New("fixed-price contract requires exactly one base price")
		}
		for _, price := range prices {
			value := price.String()
			setting.ModelPrice = &value
		}
		setting.TaskProfile = &domain.GroupModelTaskProfile{Unit: unit, FixedDurationSeconds: duration}
		return setting, nil
	}
	input := prices["input_per_1m"]
	output := prices["output_per_1m"]
	if input.IsZero() {
		for name, value := range prices {
			if name != "input_per_1m" && !value.IsZero() {
				return setting, fmt.Errorf("non-zero %s cannot be represented when input_per_1m is zero", name)
			}
		}
	}
	modelRatio := input.Div(decimal.NewFromInt(2))
	completionRatio := decimal.NewFromInt(1)
	if !input.IsZero() {
		completionRatio = output.Div(input)
	}
	setting.ModelRatio = decimalPointer(modelRatio)
	setting.CompletionRatio = decimalPointer(completionRatio)
	if value, ok := prices["cache_read_per_1m"]; ok {
		setting.CacheRatio = ratioPointer(value, input)
	}
	if value, ok := prices["cache_write_per_1m"]; ok {
		setting.CacheCreationRatio = ratioPointer(value, input)
	}
	if value, ok := prices["cache_write_5m_per_1m"]; ok {
		setting.CacheCreation5mRatio = ratioPointer(value, input)
	}
	if value, ok := prices["cache_write_1h_per_1m"]; ok {
		setting.CacheCreation1hRatio = ratioPointer(value, input)
	}
	if value, ok := prices["image_input_per_1m"]; ok {
		setting.ImageRatio = ratioPointer(value, input)
	}
	if value, ok := prices["audio_input_per_1m"]; ok {
		setting.AudioRatio = ratioPointer(value, input)
	}
	if value, ok := prices["audio_output_per_1m"]; ok {
		setting.AudioCompletionRatio = ratioPointer(value, input)
	}
	return setting, nil
}

func realizedPlanMargin(cost, sale decimal.Decimal, policy domain.PricingPolicy) (decimal.Decimal, decimal.Decimal, decimal.Decimal, bool, error) {
	revenueFactor, err := positivePlanDecimal(firstPlanValue(policy.RevenueCashFactor, "1"), "revenue cash factor")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, err
	}
	costFactor, err := positivePlanDecimal(firstPlanValue(policy.CostCashFactor, "1"), "cost cash factor")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, err
	}
	riskRate, err := decimal.NewFromString(firstPlanValue(policy.RiskBufferRate, "0"))
	if err != nil || riskRate.LessThan(decimal.NewFromInt(-1)) {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, errors.New("risk buffer rate must be at least -1")
	}
	riskCost := cost.Mul(costFactor).Mul(decimal.NewFromInt(1).Add(riskRate))
	revenue := sale.Mul(revenueFactor)
	if !revenue.IsPositive() {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, errors.New("sale revenue must be positive")
	}
	if riskCost.IsZero() {
		return riskCost, revenue, decimal.Zero, false, nil
	}
	return riskCost, revenue, revenue.Sub(riskCost).Div(revenue), true, nil
}

func sourceModelForGroup(models []domain.ModelOffer, group domain.GroupOffer, name string) (domain.ModelOffer, error) {
	groupIDs := map[string]struct{}{strings.TrimSpace(group.ExternalID): {}, strings.TrimSpace(group.Name): {}}
	exact := make([]domain.ModelOffer, 0, 1)
	unscoped := make([]domain.ModelOffer, 0, 1)
	for _, model := range models {
		if model.ExternalID != name && model.Name != name {
			continue
		}
		scope := strings.TrimSpace(model.Group)
		if _, ok := groupIDs[scope]; ok && scope != "" {
			exact = append(exact, model)
		} else if scope == "" {
			unscoped = append(unscoped, model)
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = unscoped
	}
	if len(candidates) == 0 {
		return domain.ModelOffer{}, errors.New("model is absent from the selected source group")
	}
	first := candidates[0]
	for _, candidate := range candidates[1:] {
		left, _ := json.Marshal(first.PriceFunction)
		right, _ := json.Marshal(candidate.PriceFunction)
		if string(left) != string(right) {
			return domain.ModelOffer{}, errors.New("multiple conflicting price records exist in the selected source group")
		}
	}
	return first, nil
}

func validateGroupModels(group domain.GroupOffer, models []string) error {
	allowed := make(map[string]struct{}, len(group.ModelIDs))
	for _, model := range group.ModelIDs {
		allowed[strings.TrimSpace(model)] = struct{}{}
	}
	if len(allowed) == 0 {
		return errors.New("source group has no model membership evidence")
	}
	for _, model := range models {
		if _, ok := allowed[model]; !ok {
			return fmt.Errorf("model %q is not present in source group %q", model, group.Name)
		}
	}
	return nil
}

func modelFixedDuration(metadata map[string]string) (int, error) {
	raw := firstPlanValue(metadata["fixed_duration_seconds"], metadata["duration_seconds"], metadata["billing_duration_seconds"])
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 3600 {
		return 0, errors.New("fixed_duration_seconds must be an integer between 1 and 3600")
	}
	return value, nil
}

func cloneGroupModelContract(contract domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	encoded, err := json.Marshal(contract)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	var clone domain.GroupModelPricingContract
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	return clone, nil
}

func decimalPointer(value decimal.Decimal) *string {
	result := value.String()
	return &result
}

func ratioPointer(value, base decimal.Decimal) *string {
	result := "0"
	if !base.IsZero() {
		result = value.Div(base).String()
	}
	return &result
}

func positivePlanDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.Zero, fmt.Errorf("%s must be a positive decimal", name)
	}
	return value, nil
}

func nonnegativePlanDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a non-negative decimal", name)
	}
	return value, nil
}

func ceilPlanToStep(value, step decimal.Decimal) decimal.Decimal {
	return value.Div(step).Ceil().Mul(step)
}

func firstPlanValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
