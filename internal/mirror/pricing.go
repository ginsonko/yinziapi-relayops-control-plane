package mirror

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

const defaultRoundingStep = "0.000000000001"

type PriceDimension struct {
	Kind            string `json:"kind"`
	Unit            string `json:"unit"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	DecimalValue    string `json:"decimal_value"`
}

type CashFactors struct {
	RawGroupRatio           string `json:"raw_group_ratio"`
	RechargeValueFactor     string `json:"recharge_value_factor"`
	SourceCashFactor        string `json:"source_cash_factor"`
	TargetRealizationFactor string `json:"target_realization_factor"`
	RiskBufferRate          string `json:"risk_buffer_rate"`
}

type SourceBaseInput struct {
	Dimensions         []PriceDimension
	Factors            CashFactors
	GroupRatioRule     Rule
	ModelPriceRule     Rule
	CurrentGroupRatio  string
	MinimumGroupRatio  string
	MaximumGroupRatio  string
	OnlyFollowIncrease bool
	OnlyFollowDecrease bool
	GroupRoundingStep  string
	ModelRoundingStep  string
	RoundingStep       string
}

type DimensionEconomics struct {
	PriceDimension
	SourceBasePrice   string `json:"source_base_price"`
	TargetBasePrice   string `json:"target_base_price"`
	CashCost          string `json:"cash_cost"`
	CashRevenue       string `json:"cash_revenue"`
	CashMarginRate    string `json:"cash_margin_rate,omitempty"`
	CashMarginDefined bool   `json:"cash_margin_defined"`
}

type SourceBaseResult struct {
	RawGroupRatio           string               `json:"raw_group_ratio"`
	RechargeValueFactor     string               `json:"recharge_value_factor"`
	EffectiveSourceRatio    string               `json:"effective_source_ratio"`
	TargetGroupRatio        string               `json:"target_group_ratio"`
	SourceCashFactor        string               `json:"source_cash_factor"`
	TargetRealizationFactor string               `json:"target_realization_factor"`
	RiskBufferRate          string               `json:"risk_buffer_rate"`
	Dimensions              []DimensionEconomics `json:"dimensions"`
	MinimumMarginRate       string               `json:"minimum_margin_rate,omitempty"`
	MinimumMarginDefined    bool                 `json:"minimum_margin_defined"`
}

type sourceBasePreparedDimension struct {
	original PriceDimension
	source   decimal.Decimal
	base     decimal.Decimal
	cost     decimal.Decimal
}

// ComputeSourceBase keeps upstream model base prices and the upstream group
// multiplier independent. RechargeValueFactor is multiplied into the raw
// group ratio exactly once and never applied to model base prices.
func ComputeSourceBase(input SourceBaseInput) (SourceBaseResult, error) {
	if len(input.Dimensions) == 0 {
		return SourceBaseResult{}, errors.New("source_base pricing requires at least one dimension")
	}
	rawGroup, err := parseNonnegativeDecimal(input.Factors.RawGroupRatio, "raw group ratio")
	if err != nil {
		return SourceBaseResult{}, err
	}
	recharge, err := parsePositiveDefault(input.Factors.RechargeValueFactor, "1", "recharge value factor")
	if err != nil {
		return SourceBaseResult{}, err
	}
	sourceCashFactor, err := parsePositiveDefault(input.Factors.SourceCashFactor, "1", "source cash factor")
	if err != nil {
		return SourceBaseResult{}, err
	}
	targetFactor, err := parsePositiveDefault(input.Factors.TargetRealizationFactor, "1", "target realization factor")
	if err != nil {
		return SourceBaseResult{}, err
	}
	risk, err := parseDefaultDecimal(input.Factors.RiskBufferRate, "0", "risk buffer rate")
	if err != nil || risk.LessThan(decimal.NewFromInt(-1)) {
		return SourceBaseResult{}, errors.New("risk buffer rate must be at least -1")
	}
	legacyStep := firstValue(input.RoundingStep, defaultRoundingStep)
	modelRoundingStep, err := parsePositiveDefault(input.ModelRoundingStep, legacyStep, "model rounding step")
	if err != nil {
		return SourceBaseResult{}, err
	}
	groupRoundingStep, err := parsePositiveDefault(input.GroupRoundingStep, legacyStep, "group rounding step")
	if err != nil {
		return SourceBaseResult{}, err
	}
	effectiveRatio := rawGroup.Mul(recharge)
	modelRule := normalizeRule(input.ModelPriceRule)
	groupRule := normalizeRule(input.GroupRatioRule)
	if modelRule.Type != "same" && modelRule.Type != "add" && modelRule.Type != "multiply" && modelRule.Type != "fixed" {
		return SourceBaseResult{}, fmt.Errorf("source_base model price transform %q is not supported", modelRule.Type)
	}
	if input.OnlyFollowIncrease && input.OnlyFollowDecrease {
		return SourceBaseResult{}, errors.New("only-follow-increase and only-follow-decrease cannot both be enabled")
	}

	prepared := make([]sourceBasePreparedDimension, 0, len(input.Dimensions))
	seen := make(map[string]struct{}, len(input.Dimensions))
	for _, dimension := range input.Dimensions {
		normalized, normalizeErr := normalizePriceDimension(dimension)
		if normalizeErr != nil {
			return SourceBaseResult{}, normalizeErr
		}
		key := dimensionIdentity(normalized)
		if _, exists := seen[key]; exists {
			return SourceBaseResult{}, fmt.Errorf("duplicate price dimension %s", key)
		}
		seen[key] = struct{}{}
		source, _ := decimal.NewFromString(normalized.DecimalValue)
		base, transformErr := applyTransform(source, modelRule)
		if transformErr != nil {
			return SourceBaseResult{}, fmt.Errorf("model price %s: %w", key, transformErr)
		}
		base = ceilToStep(base, modelRoundingStep)
		cost := source.Mul(effectiveRatio).Mul(sourceCashFactor).Mul(decimal.NewFromInt(1).Add(risk))
		prepared = append(prepared, sourceBasePreparedDimension{original: normalized, source: source, base: base, cost: cost})
	}

	var targetGroup decimal.Decimal
	if groupRule.Type == "target_margin" || groupRule.Type == "markup" || groupRule.Type == "subsidy_rate" {
		targetGroup, err = requiredEconomicGroupRatio(prepared, targetFactor, groupRule, groupRoundingStep)
		if err != nil {
			return SourceBaseResult{}, err
		}
	} else {
		targetGroup, err = applyTransform(effectiveRatio, groupRule)
		if err != nil {
			return SourceBaseResult{}, fmt.Errorf("group ratio: %w", err)
		}
		targetGroup = ceilToStep(targetGroup, groupRoundingStep)
	}
	targetGroup, err = constrainGroupRatio(targetGroup, input, groupRoundingStep)
	if err != nil {
		return SourceBaseResult{}, err
	}
	if !targetGroup.IsPositive() {
		return SourceBaseResult{}, errors.New("target group ratio must be positive")
	}

	result := SourceBaseResult{
		RawGroupRatio: canonicalDecimalValue(rawGroup), RechargeValueFactor: canonicalDecimalValue(recharge),
		EffectiveSourceRatio: canonicalDecimalValue(effectiveRatio), TargetGroupRatio: canonicalDecimalValue(targetGroup),
		SourceCashFactor: canonicalDecimalValue(sourceCashFactor), TargetRealizationFactor: canonicalDecimalValue(targetFactor),
		RiskBufferRate: canonicalDecimalValue(risk), Dimensions: make([]DimensionEconomics, 0, len(prepared)),
	}
	minimumMargin := decimal.NewFromInt(1)
	minimumDefined := false
	for _, item := range prepared {
		revenue := item.base.Mul(targetGroup).Mul(targetFactor)
		// An explicit zero source price is a real, representable free dimension.
		// Keep it in the contract and evidence; only a negative sale is invalid.
		// Missing values and negative values are rejected during normalization.
		if revenue.IsNegative() {
			return SourceBaseResult{}, fmt.Errorf("dimension %s produces a negative cash sale", dimensionIdentity(item.original))
		}
		economics := DimensionEconomics{PriceDimension: item.original,
			SourceBasePrice: canonicalDecimalValue(item.source), TargetBasePrice: canonicalDecimalValue(item.base),
			CashCost: canonicalDecimalValue(item.cost), CashRevenue: canonicalDecimalValue(revenue)}
		if item.cost.IsPositive() {
			margin := revenue.Sub(item.cost).Div(revenue)
			economics.CashMarginDefined = true
			economics.CashMarginRate = canonicalDecimalValue(margin)
			if !minimumDefined || margin.LessThan(minimumMargin) {
				minimumMargin = margin
			}
			minimumDefined = true
		}
		result.Dimensions = append(result.Dimensions, economics)
	}
	if minimumDefined {
		result.MinimumMarginDefined = true
		result.MinimumMarginRate = canonicalDecimalValue(minimumMargin)
	}
	if err := verifyEconomicGroupRule(prepared, targetGroup, targetFactor, groupRule); err != nil {
		return SourceBaseResult{}, err
	}
	sort.Slice(result.Dimensions, func(i, j int) bool {
		return dimensionIdentity(result.Dimensions[i].PriceDimension) < dimensionIdentity(result.Dimensions[j].PriceDimension)
	})
	return result, nil
}

type EffectiveSaleInput struct {
	DesiredCashSales        []PriceDimension
	TargetGroupRatio        string
	TargetRealizationFactor string
	RoundingStep            string
}

// ComputeEffectiveSaleBase derives target base prices from desired cash sales.
// The returned values are rounded up, so the realized sale never drops below
// the requested value.
func ComputeEffectiveSaleBase(input EffectiveSaleInput) ([]PriceDimension, error) {
	groupRatio, err := parsePositiveDecimal(input.TargetGroupRatio, "target group ratio")
	if err != nil {
		return nil, err
	}
	targetFactor, err := parsePositiveDefault(input.TargetRealizationFactor, "1", "target realization factor")
	if err != nil {
		return nil, err
	}
	step, err := parsePositiveDefault(input.RoundingStep, defaultRoundingStep, "rounding step")
	if err != nil {
		return nil, err
	}
	denominator := groupRatio.Mul(targetFactor)
	result := make([]PriceDimension, 0, len(input.DesiredCashSales))
	for _, raw := range input.DesiredCashSales {
		dimension, normalizeErr := normalizePriceDimension(raw)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		sale, _ := decimal.NewFromString(dimension.DecimalValue)
		if !sale.IsPositive() {
			return nil, fmt.Errorf("dimension %s desired cash sale must be positive", dimensionIdentity(dimension))
		}
		dimension.DecimalValue = canonicalDecimalValue(ceilToStep(sale.Div(denominator), step))
		result = append(result, dimension)
	}
	sort.Slice(result, func(i, j int) bool { return dimensionIdentity(result[i]) < dimensionIdentity(result[j]) })
	return result, nil
}

func requiredEconomicGroupRatio(prepared []sourceBasePreparedDimension, targetFactor decimal.Decimal, rule Rule,
	step decimal.Decimal) (decimal.Decimal, error) {
	maximum := decimal.Zero
	for _, item := range prepared {
		requiredRevenue, err := requiredEconomicRevenue(item.cost, rule)
		if err != nil {
			return decimal.Zero, err
		}
		if !item.base.IsPositive() {
			if requiredRevenue.IsPositive() {
				return decimal.Zero, fmt.Errorf("dimension %s cannot meet %s with a zero target base price",
					dimensionIdentity(item.original), rule.Type)
			}
			continue
		}
		required := requiredRevenue.Div(item.base.Mul(targetFactor))
		if required.GreaterThan(maximum) {
			maximum = required
		}
	}
	if !maximum.IsPositive() {
		maximum = step
	}
	return ceilToStep(maximum, step), nil
}

func requiredEconomicRevenue(cost decimal.Decimal, rule Rule) (decimal.Decimal, error) {
	rule = normalizeRule(rule)
	switch rule.Type {
	case "target_margin":
		margin, err := parseMargin(rule.Value)
		if err != nil {
			return decimal.Zero, err
		}
		return cost.Div(decimal.NewFromInt(1).Sub(margin)), nil
	case "markup":
		markup, err := parseDecimal(rule.Value, "markup")
		if err != nil || markup.LessThan(decimal.NewFromInt(-1)) {
			return decimal.Zero, errors.New("markup must be at least -1")
		}
		return cost.Mul(decimal.NewFromInt(1).Add(markup)), nil
	case "subsidy_rate":
		subsidy, err := parseDecimal(rule.Value, "subsidy rate")
		if err != nil || subsidy.IsNegative() || subsidy.GreaterThan(decimal.NewFromInt(1)) {
			return decimal.Zero, errors.New("subsidy rate must be between 0 and 1")
		}
		return cost.Mul(decimal.NewFromInt(1).Sub(subsidy)), nil
	default:
		return decimal.Zero, fmt.Errorf("rule %q is not an economic group rule", rule.Type)
	}
}

func constrainGroupRatio(value decimal.Decimal, input SourceBaseInput, step decimal.Decimal) (decimal.Decimal, error) {
	minimum, err := parseDefaultDecimal(input.MinimumGroupRatio, "0", "minimum group ratio")
	if err != nil || minimum.IsNegative() {
		return decimal.Zero, errors.New("minimum group ratio must be a non-negative decimal")
	}
	maximum, err := parseDefaultDecimal(input.MaximumGroupRatio, "0", "maximum group ratio")
	if err != nil || maximum.IsNegative() || (maximum.IsPositive() && minimum.GreaterThan(maximum)) {
		return decimal.Zero, errors.New("maximum group ratio must be zero or at least the minimum group ratio")
	}
	current, err := parseDefaultDecimal(input.CurrentGroupRatio, "0", "current group ratio")
	if err != nil || current.IsNegative() {
		return decimal.Zero, errors.New("current group ratio must be a non-negative decimal")
	}
	if value.LessThan(minimum) {
		value = minimum
	}
	value = ceilToStep(value, step)
	if maximum.IsPositive() && value.GreaterThan(maximum) {
		value = maximum
	}
	if input.OnlyFollowIncrease && current.IsPositive() && value.LessThan(current) {
		value = current
	}
	if input.OnlyFollowDecrease && current.IsPositive() && value.GreaterThan(current) {
		value = current
	}
	if value.LessThan(minimum) || (maximum.IsPositive() && value.GreaterThan(maximum)) {
		return decimal.Zero, errors.New("follow-direction constraint conflicts with the configured group-ratio bounds")
	}
	return value, nil
}

func verifyEconomicGroupRule(prepared []sourceBasePreparedDimension, targetGroup, targetFactor decimal.Decimal,
	rule Rule) error {
	rule = normalizeRule(rule)
	if rule.Type != "target_margin" && rule.Type != "markup" && rule.Type != "subsidy_rate" {
		return nil
	}
	for _, item := range prepared {
		required, err := requiredEconomicRevenue(item.cost, rule)
		if err != nil {
			return err
		}
		realized := item.base.Mul(targetGroup).Mul(targetFactor)
		if realized.LessThan(required) {
			return fmt.Errorf("dimension %s misses the requested %s after rounding or constraints",
				dimensionIdentity(item.original), rule.Type)
		}
	}
	return nil
}

func recommendedRatio(costs, bases []decimal.Decimal, targetFactor, margin, step decimal.Decimal) (decimal.Decimal, error) {
	if len(costs) != len(bases) || len(costs) == 0 {
		return decimal.Zero, errors.New("margin recommendation requires matching cost and base vectors")
	}
	denominatorFactor := targetFactor.Mul(decimal.NewFromInt(1).Sub(margin))
	maximum := decimal.Zero
	for index := range costs {
		if !bases[index].IsPositive() {
			return decimal.Zero, errors.New("minimum margin cannot be guaranteed with a zero target base price")
		}
		required := costs[index].Div(bases[index].Mul(denominatorFactor))
		if required.GreaterThan(maximum) {
			maximum = required
		}
	}
	return ceilToStep(maximum, step), nil
}

func applyTransform(value decimal.Decimal, rule Rule) (decimal.Decimal, error) {
	rule = normalizeRule(rule)
	var result decimal.Decimal
	switch rule.Type {
	case "same":
		result = value
	case "add":
		adjustment, err := parseDecimal(rule.Value, "add transform value")
		if err != nil {
			return decimal.Zero, err
		}
		result = value.Add(adjustment)
	case "multiply":
		factor, err := parseDecimal(rule.Value, "multiply transform value")
		if err != nil {
			return decimal.Zero, err
		}
		result = value.Mul(factor)
	case "markup":
		markup, err := parseDecimal(rule.Value, "markup transform value")
		if err != nil || markup.LessThan(decimal.NewFromInt(-1)) {
			return decimal.Zero, errors.New("markup transform value must be at least -1")
		}
		result = value.Mul(decimal.NewFromInt(1).Add(markup))
	case "subsidy_rate":
		subsidy, err := parseDecimal(rule.Value, "subsidy transform value")
		if err != nil || subsidy.IsNegative() || subsidy.GreaterThan(decimal.NewFromInt(1)) {
			return decimal.Zero, errors.New("subsidy transform value must be between 0 and 1")
		}
		result = value.Mul(decimal.NewFromInt(1).Sub(subsidy))
	case "fixed":
		fixed, err := parseDecimal(rule.Value, "fixed transform value")
		if err != nil {
			return decimal.Zero, err
		}
		result = fixed
	case "target_margin":
		return decimal.Zero, errors.New("target_margin requires the complete price vector")
	default:
		return decimal.Zero, fmt.Errorf("unsupported transform %q", rule.Type)
	}
	if result.IsNegative() {
		return decimal.Zero, errors.New("transform result cannot be negative")
	}
	return result, nil
}

func parseMargin(raw string) (decimal.Decimal, error) {
	value, err := parseDecimal(raw, "target margin")
	if err != nil || value.IsNegative() || value.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return decimal.Zero, errors.New("target margin must be at least 0 and less than 1")
	}
	return value, nil
}

func ceilToStep(value, step decimal.Decimal) decimal.Decimal {
	return value.Div(step).Ceil().Mul(step)
}

func dimensionIdentity(dimension PriceDimension) string {
	return fmt.Sprintf("%s|%s|%d", dimension.Kind, dimension.Unit, dimension.DurationSeconds)
}

func parsePositiveDefault(raw, fallback, name string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	return parsePositiveDecimal(raw, name)
}

func parseDefaultDecimal(raw, fallback, name string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	return parseDecimal(raw, name)
}
