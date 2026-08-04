package pricing

import (
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestTargetMarginIncludesCashAndRiskFactors(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "target_margin"
	policy.Value = "0.2"
	policy.RevenueCashFactor = "0.9"
	policy.CostCashFactor = "0.8"
	policy.RiskBufferRate = "0.1"

	result, err := Calculate(Input{SourceRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetRatio != "0.123000000000" {
		t.Fatalf("unexpected target ratio: %s", result.TargetRatio)
	}
	if result.MarginRate == nil || *result.MarginRate < "0.2" {
		t.Fatalf("target margin was not preserved after rounding: %#v", result.MarginRate)
	}
}

func TestRuleSourceAndEquivalentCostAreCalculatedSeparately(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "add"
	policy.Value = "0.03"

	result, err := Calculate(Input{SourceRatio: "0.12", CostRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceRatio != "0.120000000000" || result.CostRatio != "0.100000000000" {
		t.Fatalf("source and cost ratios were not preserved separately: %#v", result)
	}
	if result.TargetRatio != "0.150000000000" || result.ContributionProfit != "0.050000000000" {
		t.Fatalf("group-ratio rule or cost-based profit is wrong: %#v", result)
	}
}

func TestFixedAddUsesExactCurrentSourceValue(t *testing.T) {
	t.Parallel()
	policy := basePolicy()
	policy.RuleType = "add"
	policy.Value = "0.03"

	result, err := Calculate(Input{SourceRatio: "0.13", CostRatio: "0.13", CurrentRatio: "0.06", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetRatio != "0.160000000000" || result.Formula != "target = source + value" {
		t.Fatalf("0.13 + 0.03 calculation = %#v", result)
	}
}

func TestNegativeProfitRequiresExplicit公益Policy(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "fixed"
	policy.Value = "0.05"
	if _, err := Calculate(Input{SourceRatio: "0.1", Policy: policy}); err == nil {
		t.Fatal("expected negative contribution profit to be rejected")
	}
	policy.AllowNegativeProfit = true
	result, err := Calculate(Input{SourceRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.SubsidyRate == nil || *result.SubsidyRate != "0.500000000000" {
		t.Fatalf("unexpected subsidy rate: %#v", result.SubsidyRate)
	}
}

func TestFollowIncreaseKeepsCurrentValue(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "multiply"
	policy.Value = "1.2"
	policy.OnlyFollowIncrease = true
	result, err := Calculate(Input{SourceRatio: "0.1", CurrentRatio: "0.2", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetRatio != "0.200000000000" {
		t.Fatalf("unexpected target ratio: %s", result.TargetRatio)
	}
}

func TestProfitBasisChangesMarginSolver(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "target_margin"
	policy.Value = "0.2"
	policy.RevenueCashFactor = "0.8"
	policy.CostCashFactor = "0.5"
	policy.RiskBufferRate = "0.2"
	policy.ProfitBasis = "cash"

	result, err := Calculate(Input{SourceRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProfitBasis != "cash" || result.TargetRatio != "0.079000000000" {
		t.Fatalf("cash-basis calculation = %#v", result)
	}
	if result.BasisProfit != result.CashProfit {
		t.Fatalf("basis profit %s does not match cash profit %s", result.BasisProfit, result.CashProfit)
	}
}

func TestHardMaximumWinsAfterCeiling(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "fixed"
	policy.Value = "0.101"
	policy.MaximumRatio = "0.105"
	policy.RoundingStep = "0.01"
	policy.AllowNegativeProfit = true

	result, err := Calculate(Input{SourceRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetRatio != "0.105000000000" {
		t.Fatalf("rounded target exceeded hard maximum: %#v", result)
	}
}

func TestRejectsConflictingConstraints(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "fixed"
	policy.Value = "0.1"
	policy.MinimumRatio = "0.2"
	policy.MaximumRatio = "0.1"
	if _, err := Calculate(Input{SourceRatio: "0.1", Policy: policy}); err == nil {
		t.Fatal("expected minimum greater than maximum to be rejected")
	}

	policy.MinimumRatio = "0"
	policy.MaximumRatio = "0"
	policy.OnlyFollowIncrease = true
	policy.OnlyFollowDecrease = true
	if _, err := Calculate(Input{SourceRatio: "0.1", Policy: policy}); err == nil {
		t.Fatal("expected opposing follow constraints to be rejected")
	}
}

func TestNegativeProfitForcesApprovalUntilBudgetLedgerExists(t *testing.T) {
	policy := basePolicy()
	policy.RuleType = "fixed"
	policy.Value = "0.05"
	policy.AllowNegativeProfit = true
	policy.SubsidyBudgetDaily = "10"
	policy.ExecutionMode = "auto"

	result, err := Calculate(Input{SourceRatio: "0.1", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if result.AutomationEligible || result.AutomationReason == "" {
		t.Fatalf("negative-profit automation was not gated: %#v", result)
	}
}

func basePolicy() domain.PricingPolicy {
	return domain.PricingPolicy{
		RevenueCashFactor: "1",
		CostCashFactor:    "1",
		RiskBufferRate:    "0",
		MinimumRatio:      "0",
		MaximumRatio:      "0",
		RoundingStep:      "0.001",
	}
}
