package opportunity

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestZeroDemandHighMarginDoesNotRankAheadOfRealDemand(t *testing.T) {
	t.Parallel()
	zeroDemand := viableCandidate("zero", ModelInput{ModelID: "zero", Recognized: true, Currency: "USD", Demand7D: "0", Demand30D: "0",
		SalesUnitPrice: "100", CandidateUnitCost: "1", CurrentUnitCost: "50", ComparableDimensions: 1, TotalDimensions: 1})
	realDemand := viableCandidate("real", ModelInput{ModelID: "real", Recognized: true, Currency: "USD", Demand7D: "70", Demand30D: "300",
		SalesUnitPrice: "2", CandidateUnitCost: "1", CandidateCostUpper: "1", CurrentUnitCost: "1.5", ComparableDimensions: 1, TotalDimensions: 1})
	results := EvaluateAll([]Candidate{zeroDemand, realDemand}, DefaultConfig())
	if results[0].ID != "real" || results[0].ExpectedDailyProfit == "0" {
		t.Fatalf("ranked results = %#v", results)
	}
}

func TestNegativeConservativeLowerBoundIsNeverRecommended(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("risk", ModelInput{ModelID: "m", Recognized: true, Currency: "USD", Demand7D: "70", Demand30D: "300",
		SalesUnitPrice: "2", CandidateUnitCost: "1", CandidateCostUpper: "2.5", CurrentUnitCost: "1.5", ComparableDimensions: 1, TotalDimensions: 1})
	result := Evaluate(candidate, DefaultConfig())
	if result.Classification == ClassificationRecommended || result.ConservativeLowerBound == "" || result.ConservativeLowerBound[0] != '-' {
		t.Fatalf("result = %#v", result)
	}
}

func TestUnknownInputsRemainInsufficientInsteadOfFakeZero(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("unknown", ModelInput{ModelID: "m", Recognized: true, Currency: "USD", SalesUnitPrice: "2",
		CandidateUnitCost: "", CurrentUnitCost: "1"})
	result := Evaluate(candidate, DefaultConfig())
	if result.Classification != ClassificationInsufficient || result.ExpectedDailyProfit != "" || result.Eligible {
		t.Fatalf("result = %#v", result)
	}
}

func TestEmptyAndNonComparableCandidatesAreHardFiltered(t *testing.T) {
	t.Parallel()
	empty := viableCandidate("empty")
	nonComparable := viableCandidate("currency", ModelInput{ModelID: "m", Recognized: true, Currency: "", SalesUnitPrice: "",
		CandidateUnitCost: "1", CurrentUnitCost: "1"})
	for _, candidate := range []Candidate{empty, nonComparable} {
		result := Evaluate(candidate, DefaultConfig())
		if result.Eligible || result.Classification != ClassificationInsufficient || len(result.HardFilterReasons) == 0 {
			t.Fatalf("candidate %s result = %#v", candidate.ID, result)
		}
	}
}

func TestExplicitPublicBenefitBudgetPreservesNegativeProfitOpportunity(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("subsidy", ModelInput{ModelID: "m", Recognized: true, Currency: "USD", Demand7D: "7", Demand30D: "30",
		SalesUnitPrice: "0.5", CandidateUnitCost: "1", CandidateCostUpper: "1", CurrentUnitCost: "1.2", ComparableDimensions: 1, TotalDimensions: 1})
	candidate.AllowSubsidy = true
	candidate.SubsidyBudgetDaily = "1"
	result := Evaluate(candidate, DefaultConfig())
	if result.Classification != ClassificationWatch || !result.SubsidyWithinBudget || result.SubsidyRequiredDaily == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestPriorDemandIsVisibleAndCannotBecomeRecommended(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("prior", ModelInput{ModelID: "m", Recognized: true, Currency: "USD",
		SalesUnitPrice: "4", CandidateUnitCost: "1", CandidateCostUpper: "1", CurrentUnitCost: "2", ComparableDimensions: 1, TotalDimensions: 1})
	result := Evaluate(candidate, DefaultConfig())
	if !result.UsesPrior || result.Classification != ClassificationWatch || result.Models[0].DemandEvidence != "prior" {
		t.Fatalf("result = %#v", result)
	}
}

func TestProfitableButMoreExpensiveThanCurrentRouteIsNotRecommended(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("worse-route", ModelInput{ModelID: "m", Recognized: true, Currency: "USD", Demand7D: "70", Demand30D: "300",
		SalesUnitPrice: "5", CandidateUnitCost: "2", CandidateCostUpper: "2", CurrentUnitCost: "1", ComparableDimensions: 1, TotalDimensions: 1})
	result := Evaluate(candidate, DefaultConfig())
	if result.Classification == ClassificationRecommended || result.ExpectedDailySaving != "0" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOpportunityPredictionCarriesExecutableRuleSnapshot(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("rule-snapshot", ModelInput{ModelID: "m", Recognized: true, Currency: "USD",
		Demand7D: "70", Demand30D: "300", SalesUnitPrice: "6", TargetSalesRatio: "0.06",
		CandidateUnitCost: "2", CandidateCostUpper: "2", CurrentUnitCost: "3", ComparableDimensions: 1, TotalDimensions: 1})
	result := Evaluate(candidate, DefaultConfig())
	if result.RecommendedRuleType != "fixed" || result.RecommendedRuleValue != "0.06" ||
		result.RecommendedRounding != "0.000001" || result.RecommendationBasis == "" {
		t.Fatalf("recommendation snapshot = %#v", result)
	}
}

func TestOpportunityPredictionUsesTheSameCeilingStepAsExecutablePricing(t *testing.T) {
	t.Parallel()
	model := ModelInput{ModelID: "m", Recognized: true, Currency: "USD", Demand7D: "70", Demand30D: "300",
		SalesUnitPrice: "6.00004", TargetSalesRatio: "0.0600004", CandidateUnitCost: "2", CandidateCostUpper: "2",
		CurrentUnitCost: "3", ComparableDimensions: 1, TotalDimensions: 1}
	withRule := Evaluate(viableCandidate("quantized-rule", model), DefaultConfig())
	model.TargetSalesRatio = ""
	withoutRule := Evaluate(viableCandidate("unquantized-baseline", model), DefaultConfig())
	if withRule.RecommendedRuleValue != "0.060001" || withRule.RecommendedRounding != "0.000001" {
		t.Fatalf("quantized recommendation = %#v", withRule)
	}
	withProfit, _ := decimal.NewFromString(withRule.ExpectedDailyProfit)
	withoutProfit, _ := decimal.NewFromString(withoutRule.ExpectedDailyProfit)
	if !withProfit.GreaterThan(withoutProfit) {
		t.Fatalf("prediction did not use executable ceiling value: with=%s without=%s", withProfit, withoutProfit)
	}
}

func TestObservedRequestCountCannotMultiplyPerMillionTokenPrices(t *testing.T) {
	t.Parallel()
	candidate := viableCandidate("unit-mismatch", ModelInput{ModelID: "m", Recognized: true, Currency: "USD",
		Demand7D: "700", Demand30D: "3000", DemandUnit: "requests", PriceUnit: "million_tokens",
		SalesUnitPrice: "6", CandidateUnitCost: "2", CandidateCostUpper: "2", CurrentUnitCost: "3",
		ComparableDimensions: 1, TotalDimensions: 1})
	result := Evaluate(candidate, DefaultConfig())
	if result.Eligible || result.ExpectedDailyProfit != "" || result.Classification != ClassificationInsufficient ||
		!strings.Contains(result.Reason, "单位不一致") {
		t.Fatalf("unit-mismatched result = %#v", result)
	}
}

func viableCandidate(id string, models ...ModelInput) Candidate {
	return Candidate{ID: id, SupplierID: 1, Supplier: "supplier", Group: id, EntityFingerprint: id, Models: models,
		TargetIntersection: true, Availability: "0.95", AvailabilityLower: "0.85", EvidenceConfidence: "0.9", ObservedAt: time.Now().UTC()}
}
