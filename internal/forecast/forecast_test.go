package forecast

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCalculateDoesNotSubtractBalancesAcrossCurrencies(t *testing.T) {
	now := time.Now().UTC()
	history := []domain.BalanceState{
		{Available: "100", Currency: "USD", ObservedAt: now},
		{Available: "1000", Currency: "CNY", ObservedAt: now.Add(-24 * time.Hour)},
	}
	result := Calculate(domain.Supplier{ID: 1, Name: "mixed"}, history, decimal.NewFromInt(48))
	if result.RunwayHours != nil || result.DailySpend != "" || result.Confidence != "insufficient_data" {
		t.Fatalf("mixed-currency forecast = %#v", result)
	}
}

func TestCalculateKeepsUnknownBalanceUnknown(t *testing.T) {
	now := time.Now().UTC()
	result := Calculate(domain.Supplier{ID: 1, Name: "unknown"}, []domain.BalanceState{{
		RawValue: "30965000", RawUnit: "quota", Currency: "USD", ObservedAt: now,
	}}, decimal.NewFromInt(48))
	if result.BalanceAmount != "" || result.RunwayHours != nil || result.NeedsAttention || result.Confidence != "insufficient_data" {
		t.Fatalf("unknown balance forecast = %#v", result)
	}
}

func TestCalculateEstimatesSpendWithinOneCurrency(t *testing.T) {
	now := time.Now().UTC()
	history := []domain.BalanceState{
		{Available: "90", Currency: "usd", ObservedAt: now},
		{Available: "100", Currency: "USD", ObservedAt: now.Add(-24 * time.Hour)},
	}
	result := Calculate(domain.Supplier{ID: 1, Name: "same"}, history, decimal.NewFromInt(48))
	if result.RunwayHours == nil || *result.RunwayHours != "216.0" || result.DailySpend != "10" || result.Confidence != "estimated" {
		t.Fatalf("same-currency forecast = %#v", result)
	}
}

func TestCalculateAppliesRechargeFactorOnceAndPreservesRunway(t *testing.T) {
	now := time.Now().UTC()
	supplier := domain.Supplier{ID: 7, Name: "Kuaipao", RechargeValueFactor: "0.4"}
	result := Calculate(supplier, []domain.BalanceState{{
		Available: "100", DailySpend: "20", Currency: "USD", Confidence: "observed", ObservedAt: now,
	}}, decimal.NewFromInt(48))
	if result.RawBalanceAmount != "100" || result.BalanceAmount != "40" || result.RawDailySpend != "20" ||
		result.DailySpend != "8" || result.CashFactor != "0.4" || result.Currency != "USD" {
		t.Fatalf("discounted cash projection = %#v", result)
	}
	if result.RunwayHours == nil || *result.RunwayHours != "120.0" {
		t.Fatalf("runway changed under a common cash factor: %#v", result.RunwayHours)
	}
	converted, ok := ConvertCash("15", "0.4")
	if !ok || converted != "6" {
		t.Fatalf("ConvertCash = %q, %v", converted, ok)
	}
}

func TestCalculateDefaultsInvalidRechargeFactorToOne(t *testing.T) {
	result := Calculate(domain.Supplier{Name: "default", RechargeValueFactor: ""}, []domain.BalanceState{{
		Available: "12.5", Currency: "CNY", ObservedAt: time.Now().UTC(),
	}}, decimal.Zero)
	if result.BalanceAmount != "12.5" || result.CashFactor != "1" || result.RawBalanceAmount != "12.5" {
		t.Fatalf("default cash factor projection = %#v", result)
	}
}
