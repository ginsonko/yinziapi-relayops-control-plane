package mirror

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestSourceBaseExampleAppliesRechargeOnce(t *testing.T) {
	result, err := ComputeSourceBase(SourceBaseInput{
		Dimensions: []PriceDimension{{Kind: "per_request", Unit: "currency_per_request", DecimalValue: "1.00"}},
		Factors: CashFactors{RawGroupRatio: "3", RechargeValueFactor: "0.4", SourceCashFactor: "1",
			TargetRealizationFactor: "1", RiskBufferRate: "0"},
		GroupRatioRule: Rule{Type: "add", Value: "0.1"}, ModelPriceRule: Rule{Type: "same"}, RoundingStep: "0.01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveSourceRatio != "1.2" || result.TargetGroupRatio != "1.3" || len(result.Dimensions) != 1 ||
		result.Dimensions[0].SourceBasePrice != "1" || result.Dimensions[0].TargetBasePrice != "1" ||
		result.Dimensions[0].CashCost != "1.2" || result.Dimensions[0].CashRevenue != "1.3" {
		t.Fatalf("source_base result = %#v", result)
	}
}

func TestSourceCashAndRechargeFactorsRemainDistinct(t *testing.T) {
	result, err := ComputeSourceBase(SourceBaseInput{
		Dimensions: []PriceDimension{{Kind: "per_second", Unit: "currency_per_second", DecimalValue: "2"}},
		Factors: CashFactors{RawGroupRatio: "3", RechargeValueFactor: "0.4", SourceCashFactor: "1.1",
			TargetRealizationFactor: "1", RiskBufferRate: "0.05"},
		GroupRatioRule: Rule{Type: "same"}, ModelPriceRule: Rule{Type: "same"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveSourceRatio != "1.2" || result.Dimensions[0].CashCost != "2.772" ||
		result.Dimensions[0].CashRevenue != "2.4" {
		t.Fatalf("distinct factors result = %#v", result)
	}
}

func TestTargetMarginUsesWorstDimensionAndRechecksRoundedValue(t *testing.T) {
	result, err := ComputeSourceBase(SourceBaseInput{
		Dimensions: []PriceDimension{
			{Kind: "input_per_1m", Unit: "currency_per_1m_tokens", DecimalValue: "1"},
			{Kind: "output_per_1m", Unit: "currency_per_1m_tokens", DecimalValue: "2"},
		},
		Factors: CashFactors{RawGroupRatio: "0.5", RechargeValueFactor: "1", SourceCashFactor: "1",
			TargetRealizationFactor: "1", RiskBufferRate: "0"},
		GroupRatioRule: Rule{Type: "target_margin", Value: "0.2"},
		ModelPriceRule: Rule{Type: "fixed", Value: "1"}, RoundingStep: "0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetGroupRatio != "1.3" || !result.MinimumMarginDefined {
		t.Fatalf("margin recommendation = %#v", result)
	}
	margin, err := decimal.NewFromString(result.MinimumMarginRate)
	if err != nil || margin.LessThan(decimal.RequireFromString("0.2")) {
		t.Fatalf("rounded minimum margin = %q", result.MinimumMarginRate)
	}
}

func TestZeroCostMarginIsUndefinedNotDivideByZero(t *testing.T) {
	result, err := ComputeSourceBase(SourceBaseInput{
		Dimensions: []PriceDimension{{Kind: "per_image", Unit: "currency_per_image", DecimalValue: "1"}},
		Factors: CashFactors{RawGroupRatio: "0", RechargeValueFactor: "1", SourceCashFactor: "1",
			TargetRealizationFactor: "1", RiskBufferRate: "0"},
		GroupRatioRule: Rule{Type: "add", Value: "0.1"}, ModelPriceRule: Rule{Type: "same"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MinimumMarginDefined || result.Dimensions[0].CashMarginDefined || result.Dimensions[0].CashCost != "0" {
		t.Fatalf("zero-cost margin = %#v", result)
	}
}

func TestSourceBaseAllowsExplicitZeroPriceDimension(t *testing.T) {
	result, err := ComputeSourceBase(SourceBaseInput{
		Dimensions: []PriceDimension{{Kind: "input_per_1m", Unit: "currency_per_1m_tokens", DecimalValue: "0"}},
		Factors: CashFactors{RawGroupRatio: "1", RechargeValueFactor: "1", SourceCashFactor: "1",
			TargetRealizationFactor: "1", RiskBufferRate: "0"},
		GroupRatioRule: Rule{Type: "same"}, ModelPriceRule: Rule{Type: "same"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Dimensions) != 1 || result.Dimensions[0].SourceBasePrice != "0" ||
		result.Dimensions[0].TargetBasePrice != "0" || result.Dimensions[0].CashRevenue != "0" ||
		result.Dimensions[0].CashMarginDefined {
		t.Fatalf("explicit zero dimension was not preserved: %#v", result)
	}
}

func TestEffectiveSaleDerivesBaseWithoutChangingModelIdentity(t *testing.T) {
	result, err := ComputeEffectiveSaleBase(EffectiveSaleInput{
		DesiredCashSales: []PriceDimension{{Kind: "fixed_duration", Unit: "currency_per_fixed_duration",
			DurationSeconds: 5, DecimalValue: "1.3"}},
		TargetGroupRatio: "1.3", TargetRealizationFactor: "1", RoundingStep: "0.01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].DecimalValue != "1" || result[0].DurationSeconds != 5 {
		t.Fatalf("effective-sale base = %#v", result)
	}
}
