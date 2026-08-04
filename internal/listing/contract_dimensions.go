package listing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// EffectiveContractDimensions resolves one group-model setting into the
// monetary dimensions charged after GroupRatio. It is shared by automation
// safety checks and operating forecasts so neither path reconstructs a
// different interpretation of the same target contract.
func EffectiveContractDimensions(setting domain.GroupModelPricingSetting, groupRatioRaw string) (map[string]string, error) {
	groupRatio, err := positiveContractDecimal(groupRatioRaw, "target group ratio")
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	switch strings.TrimSpace(setting.BillingMode) {
	case "fixed_price":
		if setting.ModelPrice == nil || setting.TaskProfile == nil {
			return nil, errors.New("fixed price or task unit is missing")
		}
		price, err := nonnegativeContractDecimal(*setting.ModelPrice, "model price")
		if err != nil {
			return nil, err
		}
		dimension := "per_request"
		if setting.TaskProfile.Unit == "per_second" {
			dimension = "per_second"
		}
		result[dimension] = price.Mul(groupRatio).String()
	case "ratio":
		if setting.ModelRatio == nil || setting.CompletionRatio == nil {
			return nil, errors.New("model or completion ratio is missing")
		}
		modelRatio, err := nonnegativeContractDecimal(*setting.ModelRatio, "model ratio")
		if err != nil {
			return nil, err
		}
		input := modelRatio.Mul(decimal.NewFromInt(2)).Mul(groupRatio)
		result["input_per_1m"] = input.String()
		completion, err := nonnegativeContractDecimal(*setting.CompletionRatio, "completion ratio")
		if err != nil {
			return nil, err
		}
		result["output_per_1m"] = input.Mul(completion).String()
		for name, pointer := range map[string]*string{
			"cache_read_per_1m": setting.CacheRatio, "cache_write_per_1m": setting.CacheCreationRatio,
			"cache_write_5m_per_1m": setting.CacheCreation5mRatio, "cache_write_1h_per_1m": setting.CacheCreation1hRatio,
			"image_input_per_1m": setting.ImageRatio, "audio_input_per_1m": setting.AudioRatio,
			"audio_output_per_1m": setting.AudioCompletionRatio,
		} {
			if pointer == nil {
				continue
			}
			ratio, parseErr := nonnegativeContractDecimal(*pointer, name)
			if parseErr != nil {
				return nil, parseErr
			}
			result[name] = input.Mul(ratio).String()
		}
	default:
		return nil, fmt.Errorf("billing mode %s is not comparable", setting.BillingMode)
	}
	return result, nil
}

func positiveContractDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.Zero, fmt.Errorf("%s must be a positive decimal", name)
	}
	return value, nil
}

func nonnegativeContractDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a non-negative decimal", name)
	}
	return value, nil
}
