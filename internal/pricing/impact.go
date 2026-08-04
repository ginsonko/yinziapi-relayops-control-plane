package pricing

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
)

// PricingImpact is the model- and billing-dimension evidence used to express
// an upstream catalog as one conservative NewAPI group multiplier.
type PricingImpact struct {
	Models               []string          `json:"models"`
	CostRatioVector      map[string]string `json:"cost_ratio_vector"`
	EquivalentCostRatio  string            `json:"equivalent_cost_ratio"`
	ComparableDimensions int               `json:"comparable_dimensions"`
	TotalDimensions      int               `json:"total_dimensions"`
	CoverageRatio        string            `json:"coverage_ratio"`
	Complete             bool              `json:"complete"`
	Confidence           string            `json:"confidence"`
	UnknownDimensions    []string          `json:"unknown_dimensions"`
	Warnings             []string          `json:"warnings"`
}

// ModelPriceHash deliberately excludes group multipliers and balances. Group
// multipliers have their own CAS hash, while model price evidence must remain
// stable across the intended GroupRatio write.
func ModelPriceHash(snapshot domain.CatalogSnapshot) string {
	type canonicalModel struct {
		ExternalID    string               `json:"external_id"`
		Name          string               `json:"name"`
		Group         string               `json:"group,omitempty"`
		BillingMode   string               `json:"billing_mode"`
		PriceFunction domain.PriceFunction `json:"price_function"`
		sortKey       string
	}
	models := make([]canonicalModel, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		canonical := canonicalModel{ExternalID: model.ExternalID, Name: model.Name, Group: model.Group,
			BillingMode: model.BillingMode, PriceFunction: model.PriceFunction}
		encoded, _ := json.Marshal(canonical)
		canonical.sortKey = string(encoded)
		models = append(models, canonical)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].ExternalID != models[j].ExternalID {
			return models[i].ExternalID < models[j].ExternalID
		}
		if models[i].Name != models[j].Name {
			return models[i].Name < models[j].Name
		}
		if models[i].Group != models[j].Group {
			return models[i].Group < models[j].Group
		}
		if models[i].BillingMode != models[j].BillingMode {
			return models[i].BillingMode < models[j].BillingMode
		}
		return models[i].sortKey < models[j].sortKey
	})
	encoded, _ := json.Marshal(models)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ResolvePricingImpact compares the same models and monetary dimensions on an
// upstream catalog and the target catalog. The highest equivalent multiplier
// is used so a single target group ratio cannot silently underprice one model.
func ResolvePricingImpact(source, target domain.CatalogSnapshot, group domain.GroupOffer, models []string) (PricingImpact, error) {
	return ResolvePricingImpactForTargetGroup(source, target, group, "", models)
}

// ResolvePricingImpactForTargetGroup resolves target prices inside one NewAPI
// group. Older target catalogs without group-scoped records keep the legacy
// unscoped fallback.
func ResolvePricingImpactForTargetGroup(source, target domain.CatalogSnapshot, group domain.GroupOffer,
	targetGroup string, models []string) (PricingImpact, error) {
	groupRatio, err := nonnegativeDecimal(ratios.DecisionRatio(group), "source group ratio")
	if err != nil {
		return PricingImpact{}, err
	}
	models = sortedUnique(models)
	if len(models) == 0 {
		return PricingImpact{}, errors.New("pricing impact requires at least one model")
	}
	if err := validateGroupMembership(group, models); err != nil {
		return PricingImpact{}, err
	}

	vector := make(map[string]string)
	maximum := decimal.Zero
	unknown := make([]string, 0)
	warnings := make([]string, 0)
	comparable := 0
	total := 0
	for _, modelName := range models {
		sourceModel, err := uniqueModel(source.Models, modelName)
		if err != nil {
			total++
			unknown = append(unknown, modelName+"/source_model")
			warnings = append(warnings, fmt.Sprintf("source model %q: %v", modelName, err))
			continue
		}
		targetModel, err := uniqueModelInGroup(target.Models, modelName, targetGroup)
		if err != nil {
			total++
			unknown = append(unknown, modelName+"/target_model")
			warnings = append(warnings, fmt.Sprintf("target model %q: %v", modelName, err))
			continue
		}
		if err := requireComparableCurrency(sourceModel.PriceFunction.Currency, targetModel.PriceFunction.Currency); err != nil {
			total++
			unknown = append(unknown, modelName+"/currency")
			warnings = append(warnings, fmt.Sprintf("model %q: %v", modelName, err))
			continue
		}
		sourceDimensions, err := monetaryDimensions(sourceModel)
		if err != nil {
			total++
			unknown = append(unknown, modelName+"/source_price_function")
			warnings = append(warnings, fmt.Sprintf("source model %q: %v", modelName, err))
			continue
		}
		targetDimensions, err := monetaryDimensions(targetModel)
		if err != nil {
			total++
			unknown = append(unknown, modelName+"/target_price_function")
			warnings = append(warnings, fmt.Sprintf("target model %q: %v", modelName, err))
			continue
		}
		keys := unionKeys(sourceDimensions, targetDimensions)
		if len(keys) == 0 {
			total++
			unknown = append(unknown, modelName+"/price_function")
			warnings = append(warnings, fmt.Sprintf("model %q has no comparable monetary dimensions", modelName))
			continue
		}
		for _, dimension := range keys {
			total++
			sourceRaw, sourceKnown := sourceDimensions[dimension]
			targetRaw, targetKnown := targetDimensions[dimension]
			if !sourceKnown || !targetKnown {
				label := modelName + "/" + dimension
				unknown = append(unknown, label)
				warnings = append(warnings, fmt.Sprintf("model %q dimension %q is unknown on %s", modelName, dimension, unknownSide(sourceKnown, targetKnown)))
				continue
			}
			sourceValue, err := nonnegativeDecimal(sourceRaw, "source model price")
			if err != nil {
				return PricingImpact{}, err
			}
			targetValue, err := nonnegativeDecimal(targetRaw, "target model price")
			if err != nil {
				return PricingImpact{}, err
			}
			if targetValue.IsZero() {
				if sourceValue.IsZero() {
					comparable++
					continue
				}
				label := modelName + "/" + dimension
				unknown = append(unknown, label)
				warnings = append(warnings, fmt.Sprintf("model %q dimension %q has zero target base price but non-zero upstream cost", modelName, dimension))
				continue
			}
			comparable++
			equivalent := sourceValue.Mul(groupRatio).Div(targetValue)
			vector[modelName+"/"+dimension] = equivalent.String()
			if equivalent.GreaterThan(maximum) {
				maximum = equivalent
			}
		}
	}
	coverage := decimal.Zero
	if total > 0 {
		coverage = decimal.NewFromInt(int64(comparable)).Div(decimal.NewFromInt(int64(total)))
	}
	if len(vector) == 0 {
		detail := strings.Join(warnings, "; ")
		if detail == "" {
			detail = "all selected model dimensions were zero on both sides"
		}
		return PricingImpact{Models: models, CostRatioVector: vector, ComparableDimensions: comparable,
				TotalDimensions: total, CoverageRatio: coverage.String(), Complete: false, Confidence: "unavailable",
				UnknownDimensions: sortedUnique(unknown), Warnings: warnings},
			fmt.Errorf("pricing impact has no comparable non-zero dimension: %s; profit cannot be verified", detail)
	}
	confidence := "high"
	if len(unknown) > 0 {
		confidence = "partial"
		if coverage.LessThan(decimal.RequireFromString("0.8")) {
			confidence = "low"
		}
	}
	return PricingImpact{Models: models, CostRatioVector: vector, EquivalentCostRatio: maximum.String(),
		ComparableDimensions: comparable, TotalDimensions: total, CoverageRatio: coverage.String(),
		Complete: len(unknown) == 0, Confidence: confidence, UnknownDimensions: sortedUnique(unknown), Warnings: warnings}, nil
}

func monetaryDimensions(model domain.ModelOffer) (map[string]string, error) {
	function := model.PriceFunction
	if function.RequiresModelBasePrice {
		return nil, errors.New("price function requires an unresolved model base price")
	}
	values := map[string]string{
		"input_per_1m": function.InputPer1M, "output_per_1m": function.OutputPer1M,
		"cache_read_per_1m": function.CacheReadPer1M, "cache_write_per_1m": function.CacheWritePer1M,
		"cache_write_5m_per_1m": function.CacheWrite5MPer1M, "cache_write_1h_per_1m": function.CacheWrite1HPer1M,
		"image_input_per_1m": function.ImageInputPer1M, "audio_input_per_1m": function.AudioInputPer1M,
		"audio_output_per_1m": function.AudioOutputPer1M, "per_request": function.PerRequest,
		"per_image": function.PerImage, "per_second": function.PerSecond, "per_frame": function.PerFrame,
		"fixed_fee": function.FixedFee,
	}
	for key, value := range function.Dimensions {
		values["custom:"+key] = value
	}
	result := make(map[string]string)
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := nonnegativeDecimal(value, key); err != nil {
			return nil, err
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil, errors.New("price function contains no explicit monetary dimension")
	}
	return result, nil
}

// MonetaryDimensions exposes the same normalized monetary-vector contract to
// the listing planner. Callers receive a new map and cannot mutate a catalog.
func MonetaryDimensions(model domain.ModelOffer) (map[string]string, error) {
	return monetaryDimensions(model)
}

func uniqueModel(models []domain.ModelOffer, name string) (domain.ModelOffer, error) {
	return uniqueModelInGroup(models, name, "")
}

func uniqueModelInGroup(models []domain.ModelOffer, name, group string) (domain.ModelOffer, error) {
	group = strings.TrimSpace(group)
	if group != "" {
		exact := make([]domain.ModelOffer, 0, 1)
		unscoped := make([]domain.ModelOffer, 0, 1)
		for _, model := range models {
			if model.ExternalID != name && model.Name != name {
				continue
			}
			modelGroup := strings.TrimSpace(model.Group)
			if modelGroup == group {
				exact = append(exact, model)
			} else if modelGroup == "" {
				unscoped = append(unscoped, model)
			}
		}
		if len(exact) > 0 {
			return uniqueModel(exact, name)
		}
		if len(unscoped) > 0 {
			return uniqueModel(unscoped, name)
		}
		return domain.ModelOffer{}, sql.ErrNoRows
	}
	var found *domain.ModelOffer
	for index := range models {
		if models[index].ExternalID != name && models[index].Name != name {
			continue
		}
		if found != nil && !reflect.DeepEqual(found.PriceFunction, models[index].PriceFunction) {
			return domain.ModelOffer{}, errors.New("multiple conflicting model price records were found")
		}
		copy := models[index]
		found = &copy
	}
	if found == nil {
		return domain.ModelOffer{}, sql.ErrNoRows
	}
	return *found, nil
}

func validateGroupMembership(group domain.GroupOffer, selected []string) error {
	allowed := make(map[string]struct{}, len(group.ModelIDs))
	for _, model := range group.ModelIDs {
		allowed[strings.TrimSpace(model)] = struct{}{}
	}
	if len(allowed) == 0 {
		return errors.New("source group has no model membership evidence")
	}
	for _, model := range selected {
		if _, ok := allowed[model]; !ok {
			return fmt.Errorf("model %q is not present in source group %q", model, group.Name)
		}
	}
	return nil
}

func requireComparableCurrency(source, target string) error {
	source = strings.ToUpper(strings.TrimSpace(source))
	target = strings.ToUpper(strings.TrimSpace(target))
	if source == "" || target == "" {
		return errors.New("source and target model currencies must both be known")
	}
	if source != target {
		return fmt.Errorf("currency conversion is not configured (%s -> %s)", source, target)
	}
	return nil
}

func nonnegativeDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a non-negative decimal", name)
	}
	return value, nil
}

func unionKeys(left, right map[string]string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
	}
	for key := range right {
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func unknownSide(sourceKnown, targetKnown bool) string {
	switch {
	case !sourceKnown && !targetKnown:
		return "both catalogs"
	case !sourceKnown:
		return "the upstream catalog"
	default:
		return "the target catalog"
	}
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
