package decimalnorm

import (
	"strings"

	"github.com/shopspring/decimal"
)

const (
	maximumNoiseScale = int32(18)
	// NewAPI persists pricing values through float-backed JSON/options. Treat
	// sub-parts-per-trillion tails as representation noise, not price changes.
	relativeNoiseTolerance = "0.000000000002"
)

var noiseTolerance = decimal.RequireFromString(relativeNoiseTolerance)

// Functional returns the shortest decimal at or below the supported scale
// that remains within the float round-trip tolerance of value.
func Functional(value decimal.Decimal) decimal.Decimal {
	if value.IsZero() {
		return decimal.Zero
	}
	tolerance := value.Abs().Mul(noiseTolerance)
	for scale := int32(0); scale <= maximumNoiseScale; scale++ {
		candidate := value.Round(scale)
		if value.Sub(candidate).Abs().LessThanOrEqual(tolerance) {
			return candidate
		}
	}
	return value
}

func String(raw string) (string, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return Functional(value).String(), nil
}

func Equal(left, right string) bool {
	leftValue, leftErr := decimal.NewFromString(strings.TrimSpace(left))
	rightValue, rightErr := decimal.NewFromString(strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil {
		return false
	}
	return Functional(leftValue).Equal(Functional(rightValue))
}
