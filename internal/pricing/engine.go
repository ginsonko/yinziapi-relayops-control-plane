package pricing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type Input struct {
	SourceRatio  string
	CostRatio    string
	CurrentRatio string
	Policy       domain.PricingPolicy
}

func Calculate(input Input) (domain.PriceCalculation, error) {
	source, err := positiveOrZero(input.SourceRatio, "source ratio")
	if err != nil {
		return domain.PriceCalculation{}, err
	}
	costRatio := strings.TrimSpace(input.CostRatio)
	if costRatio == "" {
		costRatio = input.SourceRatio
	}
	cost, err := positiveOrZero(costRatio, "cost ratio")
	if err != nil {
		return domain.PriceCalculation{}, err
	}
	current, err := optionalDecimal(input.CurrentRatio, decimal.Zero)
	if err != nil {
		return domain.PriceCalculation{}, fmt.Errorf("current ratio: %w", err)
	}
	value, err := decimal.NewFromString(input.Policy.Value)
	if err != nil {
		return domain.PriceCalculation{}, fmt.Errorf("policy value: %w", err)
	}
	revenueFactor, err := positive(input.Policy.RevenueCashFactor, "revenue cash factor")
	if err != nil {
		return domain.PriceCalculation{}, err
	}
	costFactor, err := positive(input.Policy.CostCashFactor, "cost cash factor")
	if err != nil {
		return domain.PriceCalculation{}, err
	}
	riskRate, err := decimal.NewFromString(defaultString(input.Policy.RiskBufferRate, "0"))
	if err != nil || riskRate.LessThan(decimal.NewFromInt(-1)) {
		return domain.PriceCalculation{}, errors.New("risk buffer rate must be at least -1")
	}
	cashCost := cost.Mul(costFactor)
	riskCost := cashCost.Mul(decimal.NewFromInt(1).Add(riskRate))
	if riskCost.IsNegative() {
		return domain.PriceCalculation{}, errors.New("risk-adjusted cost cannot be negative")
	}
	profitBasis := strings.ToLower(strings.TrimSpace(input.Policy.ProfitBasis))
	if profitBasis == "" {
		profitBasis = "contribution"
	}
	basisCost := riskCost
	basisRevenueFactor := revenueFactor
	switch profitBasis {
	case "catalog", "nominal":
		profitBasis = "catalog"
		basisCost = cost
		basisRevenueFactor = decimal.NewFromInt(1)
	case "cash":
		basisCost = cashCost
	case "contribution", "risk", "risk_adjusted":
		profitBasis = "contribution"
	default:
		return domain.PriceCalculation{}, fmt.Errorf("unsupported profit basis %q", input.Policy.ProfitBasis)
	}

	var target decimal.Decimal
	formula := ""
	switch input.Policy.RuleType {
	case "add":
		target = source.Add(value)
		formula = "target = source + value"
	case "multiply":
		target = source.Mul(value)
		formula = "target = source * value"
	case "target_margin":
		if value.GreaterThanOrEqual(decimal.NewFromInt(1)) {
			return domain.PriceCalculation{}, errors.New("target margin must be less than 1")
		}
		denominator := decimal.NewFromInt(1).Sub(value).Mul(basisRevenueFactor)
		if denominator.LessThanOrEqual(decimal.Zero) {
			return domain.PriceCalculation{}, errors.New("target margin produces an invalid denominator")
		}
		target = basisCost.Div(denominator)
		formula = "target = selected_basis_cost / ((1 - margin) * selected_revenue_factor)"
	case "markup":
		if value.LessThan(decimal.NewFromInt(-1)) {
			return domain.PriceCalculation{}, errors.New("markup must be at least -1")
		}
		target = basisCost.Mul(decimal.NewFromInt(1).Add(value)).Div(basisRevenueFactor)
		formula = "target = selected_basis_cost * (1 + markup) / selected_revenue_factor"
	case "subsidy_rate":
		if value.IsNegative() || value.GreaterThan(decimal.NewFromInt(1)) {
			return domain.PriceCalculation{}, errors.New("subsidy rate must be between 0 and 1")
		}
		target = basisCost.Mul(decimal.NewFromInt(1).Sub(value)).Div(basisRevenueFactor)
		formula = "target = selected_basis_cost * (1 - subsidy) / selected_revenue_factor"
	case "fixed":
		target = value
		formula = "target = fixed value"
	default:
		return domain.PriceCalculation{}, fmt.Errorf("unsupported rule type %q", input.Policy.RuleType)
	}

	if target.IsNegative() {
		return domain.PriceCalculation{}, errors.New("target ratio cannot be negative")
	}
	warnings := make([]string, 0)
	minimum, err := optionalDecimal(input.Policy.MinimumRatio, decimal.Zero)
	if err != nil {
		return domain.PriceCalculation{}, fmt.Errorf("minimum ratio: %w", err)
	}
	maximum, err := optionalDecimal(input.Policy.MaximumRatio, decimal.Zero)
	if err != nil {
		return domain.PriceCalculation{}, fmt.Errorf("maximum ratio: %w", err)
	}
	if minimum.IsNegative() || maximum.IsNegative() {
		return domain.PriceCalculation{}, errors.New("minimum and maximum ratios cannot be negative")
	}
	if maximum.IsPositive() && minimum.GreaterThan(maximum) {
		return domain.PriceCalculation{}, errors.New("minimum ratio cannot exceed maximum ratio")
	}
	if input.Policy.OnlyFollowIncrease && input.Policy.OnlyFollowDecrease {
		return domain.PriceCalculation{}, errors.New("only-follow-increase and only-follow-decrease cannot both be enabled")
	}
	if target.LessThan(minimum) {
		target = minimum
		warnings = append(warnings, "目标倍率已提升到规则下限")
	}
	if maximum.IsPositive() && target.GreaterThan(maximum) {
		target = maximum
		warnings = append(warnings, "目标倍率已限制到规则上限")
	}
	step, err := optionalDecimal(input.Policy.RoundingStep, decimal.RequireFromString("0.001"))
	if err != nil || step.LessThanOrEqual(decimal.Zero) {
		return domain.PriceCalculation{}, errors.New("rounding step must be positive")
	}
	target = ceilToStep(target, step)
	if maximum.IsPositive() && target.GreaterThan(maximum) {
		target = maximum
		warnings = append(warnings, "向上取整会超过硬上限，已以上限为准")
	}

	if input.Policy.OnlyFollowIncrease && current.IsPositive() && target.LessThan(current) {
		if maximum.IsPositive() && current.GreaterThan(maximum) {
			return domain.PriceCalculation{}, errors.New("current ratio exceeds the hard maximum while only-follow-increase is enabled")
		}
		target = current
		warnings = append(warnings, "规则仅跟随涨价，已保持当前倍率")
	}
	if input.Policy.OnlyFollowDecrease && current.IsPositive() && target.GreaterThan(current) {
		if current.LessThan(minimum) {
			return domain.PriceCalculation{}, errors.New("current ratio is below the hard minimum while only-follow-decrease is enabled")
		}
		target = current
		warnings = append(warnings, "规则仅跟随降价，已保持当前倍率")
	}

	nominalProfit := target.Sub(cost)
	cashRevenue := target.Mul(revenueFactor)
	cashProfit := cashRevenue.Sub(cashCost)
	contributionProfit := cashRevenue.Sub(riskCost)
	basisProfit := contributionProfit
	switch profitBasis {
	case "catalog":
		basisProfit = nominalProfit
	case "cash":
		basisProfit = cashProfit
	}
	calculation := domain.PriceCalculation{
		SourceRatio:        canonical(source),
		CostRatio:          canonical(cost),
		RiskAdjustedCost:   canonical(riskCost),
		TargetRatio:        canonical(target),
		ProfitBasis:        profitBasis,
		BasisProfit:        canonical(basisProfit),
		NominalProfit:      canonical(nominalProfit),
		CashProfit:         canonical(cashProfit),
		ContributionProfit: canonical(contributionProfit),
		AutomationEligible: true,
		Warnings:           warnings,
		Formula:            formula,
	}
	if riskCost.IsPositive() {
		markup := contributionProfit.Div(riskCost)
		value := canonical(markup)
		calculation.MarkupRate = &value
		if contributionProfit.IsNegative() {
			subsidy := contributionProfit.Neg().Div(riskCost)
			value := canonical(subsidy)
			calculation.SubsidyRate = &value
		}
	}
	if cashRevenue.IsPositive() {
		margin := contributionProfit.Div(cashRevenue)
		value := canonical(margin)
		calculation.MarginRate = &value
	}
	if (basisProfit.IsNegative() || contributionProfit.IsNegative()) && !input.Policy.AllowNegativeProfit {
		return domain.PriceCalculation{}, errors.New("rule would create a negative profit; enable the public-benefit subsidy option or raise the target price")
	}
	budget, err := optionalDecimal(input.Policy.SubsidyBudgetDaily, decimal.Zero)
	if err != nil || budget.IsNegative() {
		return domain.PriceCalculation{}, errors.New("daily subsidy budget must be zero or positive")
	}
	if contributionProfit.IsNegative() {
		calculation.AutomationEligible = false
		if !budget.IsPositive() {
			calculation.AutomationReason = "负利润规则未设置每日补贴预算，只能人工审批"
		} else {
			calculation.AutomationReason = "负利润规则需要实际用量账本核销每日补贴预算，当前只能人工审批"
		}
		calculation.Warnings = append(calculation.Warnings, calculation.AutomationReason)
	}
	return calculation, nil
}

func positive(value, name string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(defaultString(value, "1"))
	if err != nil || parsed.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("%s must be positive", name)
	}
	return parsed, nil
}

func positiveOrZero(value, name string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || parsed.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be zero or positive", name)
	}
	return parsed, nil
}

func optionalDecimal(value string, fallback decimal.Decimal) (decimal.Decimal, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	return decimal.NewFromString(value)
}

func ceilToStep(value, step decimal.Decimal) decimal.Decimal {
	return value.Div(step).Ceil().Mul(step)
}

func canonical(value decimal.Decimal) string {
	return value.StringFixed(12)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
