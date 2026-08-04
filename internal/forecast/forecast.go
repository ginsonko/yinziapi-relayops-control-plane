package forecast

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func All(ctx context.Context, st *store.Store, attentionHours decimal.Decimal) ([]domain.RunwayForecast, error) {
	suppliers, err := st.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.RunwayForecast, 0, len(suppliers))
	for _, supplier := range suppliers {
		history, err := st.BalanceHistory(ctx, supplier.ID, 30)
		if err != nil || len(history) == 0 {
			continue
		}
		item := Calculate(supplier, history, attentionHours)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].NeedsAttention != result[j].NeedsAttention {
			return result[i].NeedsAttention
		}
		if result[i].RunwayHours == nil {
			return false
		}
		if result[j].RunwayHours == nil {
			return true
		}
		left, _ := decimal.NewFromString(*result[i].RunwayHours)
		right, _ := decimal.NewFromString(*result[j].RunwayHours)
		return left.LessThan(right)
	})
	return result, nil
}

func Calculate(supplier domain.Supplier, history []domain.BalanceState, attentionHours decimal.Decimal) domain.RunwayForecast {
	latest := history[0]
	available, availableKnown := knownDecimal(latest.Available, latest.Amount)
	daily, dailyKnown := knownDecimal(latest.DailySpend)
	factor := cashFactor(supplier.RechargeValueFactor)
	confidence := latest.Confidence
	if !dailyKnown || !daily.IsPositive() {
		daily = estimatedDailySpend(history)
		if daily.IsPositive() {
			dailyKnown = true
			confidence = "estimated"
		}
	}
	result := domain.RunwayForecast{SupplierID: supplier.ID, SupplierName: supplier.Name,
		BalanceAmount: "", Currency: strings.ToUpper(strings.TrimSpace(latest.Currency)), DailySpend: "",
		RawCurrency: strings.ToUpper(strings.TrimSpace(latest.Currency)), CashFactor: factor.String(),
		Confidence: confidence, ObservedAt: latest.ObservedAt}
	if !availableKnown {
		result.Confidence = "insufficient_data"
		return result
	}
	result.RawBalanceAmount = available.String()
	result.BalanceAmount = available.Mul(factor).String()
	if dailyKnown {
		result.RawDailySpend = daily.String()
		result.DailySpend = daily.Mul(factor).String()
	}
	result.ConversionFormula = fmt.Sprintf("cash = raw_amount x %s", factor.String())
	if available.IsNegative() || available.IsZero() {
		zero := "0"
		result.RunwayHours = &zero
		result.NeedsAttention = true
		result.AttentionReason = "可用余额已耗尽"
		return result
	}
	if daily.IsPositive() {
		// Both numerator and denominator use the same supplier cash factor, so
		// runway is intentionally calculated from raw values and is invariant.
		hours := available.Div(daily).Mul(decimal.NewFromInt(24))
		value := hours.StringFixed(1)
		result.RunwayHours = &value
		if attentionHours.IsPositive() && hours.LessThan(attentionHours) {
			result.NeedsAttention = true
			result.AttentionReason = "按近期消耗预计不足 " + attentionHours.String() + " 小时"
		}
	} else {
		result.Confidence = "insufficient_data"
	}
	return result
}

// ConvertCash applies the supplier-side recharge value factor exactly once.
// It is shared by reporting projections so raw observations never get
// repeatedly discounted by different presentation layers.
func ConvertCash(raw, factorRaw string) (string, bool) {
	value, ok := knownDecimal(raw)
	if !ok {
		return "", false
	}
	return value.Mul(cashFactor(factorRaw)).String(), true
}

func cashFactor(raw string) decimal.Decimal {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.NewFromInt(1)
	}
	return value
}

func estimatedDailySpend(history []domain.BalanceState) decimal.Decimal {
	if len(history) < 2 {
		return decimal.Zero
	}
	estimates := make([]decimal.Decimal, 0, len(history)-1)
	for i := 0; i+1 < len(history); i++ {
		newer, older := history[i], history[i+1]
		newerCurrency := strings.ToUpper(strings.TrimSpace(newer.Currency))
		olderCurrency := strings.ToUpper(strings.TrimSpace(older.Currency))
		if newerCurrency == "" || olderCurrency == "" || newerCurrency != olderCurrency {
			continue
		}
		newBalance := firstDecimal(newer.Available, newer.Amount)
		oldBalance := firstDecimal(older.Available, older.Amount)
		spent := oldBalance.Sub(newBalance)
		days := newer.ObservedAt.Sub(older.ObservedAt).Hours() / 24
		if !spent.IsPositive() || days <= 0 {
			continue
		}
		estimates = append(estimates, spent.Div(decimal.NewFromFloat(days)))
	}
	if len(estimates) == 0 {
		return decimal.Zero
	}
	sort.Slice(estimates, func(i, j int) bool { return estimates[i].LessThan(estimates[j]) })
	return estimates[len(estimates)/2]
}

func firstDecimal(values ...string) decimal.Decimal {
	value, _ := knownDecimal(values...)
	return value
}

func knownDecimal(values ...string) (decimal.Decimal, bool) {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		parsed, err := decimal.NewFromString(value)
		if err == nil {
			return parsed, true
		}
	}
	return decimal.Zero, false
}

func Fresh(state domain.BalanceState, maxAge time.Duration) bool {
	return !state.ObservedAt.IsZero() && time.Since(state.ObservedAt) <= maxAge
}
