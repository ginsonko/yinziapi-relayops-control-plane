package ratios

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const DefaultRechargeValueFactor = "1"

// NormalizeRechargeValueFactor validates the amount of usable upstream credit
// received for one unit of cash. Empty legacy values are equivalent to 1.
func NormalizeRechargeValueFactor(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DefaultRechargeValueFactor, nil
	}
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return "", errors.New("recharge value factor must be a decimal number")
	}
	if !parsed.IsPositive() {
		return "", errors.New("recharge value factor must be greater than zero")
	}
	if parsed.Exponent() < -12 {
		return "", errors.New("recharge value factor supports at most 12 decimal places")
	}
	return parsed.String(), nil
}

// EffectiveGroupRatio converts an upstream catalog multiplier to its cash-cost
// multiplier. Decimal arithmetic is exact and the factor is applied once.
func EffectiveGroupRatio(rawRatio, rechargeValueFactor string) (string, error) {
	raw, err := decimal.NewFromString(strings.TrimSpace(rawRatio))
	if err != nil {
		return "", errors.New("group ratio must be a decimal number")
	}
	if raw.IsNegative() {
		return "", errors.New("group ratio cannot be negative")
	}
	factor, err := NormalizeRechargeValueFactor(rechargeValueFactor)
	if err != nil {
		return "", err
	}
	parsedFactor, _ := decimal.NewFromString(factor)
	return raw.Mul(parsedFactor).String(), nil
}

// EnrichSnapshot preserves the observed ratio while attaching the cash-cost
// ratio used by pricing decisions. The stored upstream evidence is unchanged.
func EnrichSnapshot(snapshot domain.CatalogSnapshot, rechargeValueFactor string) (domain.CatalogSnapshot, error) {
	factor, err := NormalizeRechargeValueFactor(rechargeValueFactor)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	snapshot.Groups = append([]domain.GroupOffer(nil), snapshot.Groups...)
	for index := range snapshot.Groups {
		group := &snapshot.Groups[index]
		group.RawRatio = strings.TrimSpace(group.Ratio)
		group.RechargeValueFactor = factor
		if group.RawRatio == "" {
			group.EffectiveRatio = ""
			continue
		}
		effective, effectiveErr := EffectiveGroupRatio(group.RawRatio, factor)
		if effectiveErr != nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("group %q: %w", firstNonEmpty(group.Name, group.ExternalID), effectiveErr)
		}
		group.EffectiveRatio = effective
	}
	return snapshot, nil
}

// DecisionRatio returns the explicitly enriched cash-cost ratio, falling back
// to the legacy observed ratio for target catalogs and older test fixtures.
func DecisionRatio(group domain.GroupOffer) string {
	if value := strings.TrimSpace(group.EffectiveRatio); value != "" {
		return value
	}
	return strings.TrimSpace(group.Ratio)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
}
