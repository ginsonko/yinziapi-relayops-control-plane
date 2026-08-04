package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/decimalnorm"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
)

const (
	maximumDecimalInputLength = 128
	maximumDecimalDigits      = 36
	maximumDecimalExponent    = 18
	membershipOnlyBillingMode = "inherit_target"
)

type CanonicalModel struct {
	SourceModelID       string           `json:"source_model_id"`
	SourceExposedName   string           `json:"source_exposed_name"`
	UpstreamRequestName string           `json:"upstream_request_name"`
	BillingMode         string           `json:"billing_mode"`
	Currency            string           `json:"currency"`
	PriceDimensions     []PriceDimension `json:"price_dimensions"`
}

type CanonicalGroup struct {
	SourceGroupID       string           `json:"source_group_id"`
	SourceGroupName     string           `json:"source_group_name"`
	RawGroupRatio       string           `json:"raw_group_ratio"`
	RechargeValueFactor string           `json:"recharge_value_factor"`
	Models              []CanonicalModel `json:"models"`
}

var dimensionUnits = map[string]string{
	"input_per_1m":          "currency_per_1m_tokens",
	"output_per_1m":         "currency_per_1m_tokens",
	"cache_read_per_1m":     "currency_per_1m_tokens",
	"cache_write_per_1m":    "currency_per_1m_tokens",
	"cache_write_5m_per_1m": "currency_per_1m_tokens",
	"cache_write_1h_per_1m": "currency_per_1m_tokens",
	"image_input_per_1m":    "currency_per_1m_tokens",
	"audio_input_per_1m":    "currency_per_1m_tokens",
	"audio_output_per_1m":   "currency_per_1m_tokens",
	"per_request":           "currency_per_request",
	"per_image":             "currency_per_image",
	"per_second":            "currency_per_second",
	"per_frame":             "currency_per_frame",
	"fixed_duration":        "currency_per_fixed_duration",
}

// NormalizeSourceGroup produces the source identity and price vector used by
// semantic hashing and mirror planning. It never consumes EffectiveRatio;
// RawGroupRatio and RechargeValueFactor remain separate inputs.
func NormalizeSourceGroup(snapshot domain.CatalogSnapshot, group domain.GroupOffer) (CanonicalGroup, error) {
	groupID := strings.TrimSpace(group.ExternalID)
	if groupID == "" {
		groupID = strings.TrimSpace(group.Name)
	}
	if groupID == "" || strings.ContainsRune(groupID, '\x00') {
		return CanonicalGroup{}, errors.New("source group has no stable identity")
	}
	rawRatio := firstValue(group.RawRatio, group.Ratio)
	canonicalRatio, err := canonicalNonnegativeDecimal(rawRatio, "raw group ratio")
	if err != nil {
		return CanonicalGroup{}, err
	}
	recharge, err := canonicalPositiveDefault(group.RechargeValueFactor, "1", "recharge value factor")
	if err != nil {
		return CanonicalGroup{}, err
	}
	modelIDs := sortedUniqueStrings(group.ModelIDs)
	if len(modelIDs) == 0 {
		return CanonicalGroup{}, errors.New("source group has no model membership evidence")
	}
	models := make([]CanonicalModel, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model, modelErr := canonicalModelForGroup(snapshot.Models, group, modelID)
		if modelErr != nil {
			return CanonicalGroup{}, fmt.Errorf("source model %q: %w", modelID, modelErr)
		}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].SourceModelID == models[j].SourceModelID {
			return models[i].UpstreamRequestName < models[j].UpstreamRequestName
		}
		return models[i].SourceModelID < models[j].SourceModelID
	})
	return CanonicalGroup{SourceGroupID: groupID, SourceGroupName: firstValue(group.Name, groupID),
		RawGroupRatio: canonicalRatio, RechargeValueFactor: recharge, Models: models}, nil
}

func SourceSemanticHash(group CanonicalGroup) (string, error) {
	normalized, err := normalizeCanonicalGroup(group)
	if err != nil {
		return "", err
	}
	return hashCanonical(normalized)
}

func EmptySourceSemanticHash(group domain.GroupOffer) (string, error) {
	groupID := strings.TrimSpace(group.ExternalID)
	if groupID == "" {
		groupID = strings.TrimSpace(group.Name)
	}
	return EmptyCanonicalSourceSemanticHash(CanonicalGroup{SourceGroupID: groupID,
		SourceGroupName: firstValue(group.Name, groupID), RawGroupRatio: firstValue(group.RawRatio, group.Ratio),
		RechargeValueFactor: group.RechargeValueFactor})
}

// EmptyCanonicalSourceSemanticHash keeps planner and stored-proof identity
// identical for a real, authoritatively observed empty group. It is distinct
// from a missing group and therefore includes the empty state marker.
func EmptyCanonicalSourceSemanticHash(group CanonicalGroup) (string, error) {
	normalized, err := normalizeCanonicalGroup(group)
	if err != nil {
		return "", err
	}
	if len(normalized.Models) != 0 {
		return "", errors.New("empty source group semantic hash cannot contain models")
	}
	return hashCanonical(struct {
		State               string   `json:"state"`
		SourceGroupID       string   `json:"source_group_id"`
		SourceGroupName     string   `json:"source_group_name"`
		RawGroupRatio       string   `json:"raw_group_ratio"`
		RechargeValueFactor string   `json:"recharge_value_factor"`
		Models              []string `json:"models"`
	}{State: SourceStateEmpty, SourceGroupID: normalized.SourceGroupID,
		SourceGroupName: normalized.SourceGroupName, RawGroupRatio: normalized.RawGroupRatio,
		RechargeValueFactor: normalized.RechargeValueFactor, Models: []string{}})
}

func MissingSourceSemanticHash(sourceGroup string) (string, error) {
	sourceGroup = strings.TrimSpace(sourceGroup)
	if sourceGroup == "" || strings.ContainsRune(sourceGroup, '\x00') {
		return "", errors.New("missing source group requires a bounded stable identity")
	}
	return hashCanonical(struct {
		State         string `json:"state"`
		SourceGroupID string `json:"source_group_id"`
	}{State: SourceStateMissing, SourceGroupID: sourceGroup})
}

// TargetFunctionalHash excludes all source provenance. A timestamp-only
// observation or formatting-only decimal change therefore remains a no-op.
func TargetFunctionalHash(contract domain.GroupModelPricingContract, groupRatios map[string]string) (string, error) {
	type functionalSetting struct {
		BillingMode string                        `json:"billing_mode"`
		Dimensions  map[string]string             `json:"dimensions"`
		BillingExpr string                        `json:"billing_expr,omitempty"`
		TaskProfile *domain.GroupModelTaskProfile `json:"task_profile,omitempty"`
	}
	functional := struct {
		SchemaVersion int                                     `json:"schema_version"`
		GroupRatios   map[string]string                       `json:"group_ratios"`
		Groups        map[string]map[string]functionalSetting `json:"groups"`
	}{SchemaVersion: contract.SchemaVersion, GroupRatios: map[string]string{}, Groups: map[string]map[string]functionalSetting{}}
	for group, ratio := range groupRatios {
		canonical, err := canonicalPositiveDecimal(ratio, "target group ratio")
		if err != nil {
			return "", fmt.Errorf("group %q: %w", group, err)
		}
		functional.GroupRatios[strings.TrimSpace(group)], _ = decimalnorm.String(canonical)
	}
	for group, models := range contract.Groups {
		groupKey := strings.TrimSpace(group)
		if groupKey == "" {
			return "", errors.New("target contract contains an empty group")
		}
		functional.Groups[groupKey] = map[string]functionalSetting{}
		for model, setting := range models {
			modelKey := strings.TrimSpace(model)
			if modelKey == "" {
				return "", fmt.Errorf("target group %q contains an empty model identity", groupKey)
			}
			dimensions, err := canonicalSettingDimensions(setting)
			if err != nil {
				return "", fmt.Errorf("target %s/%s: %w", groupKey, modelKey, err)
			}
			profile := cloneTaskProfile(setting.TaskProfile)
			if err := validateTaskProfile(profile); err != nil {
				return "", fmt.Errorf("target %s/%s: %w", groupKey, modelKey, err)
			}
			functional.Groups[groupKey][modelKey] = functionalSetting{BillingMode: strings.TrimSpace(setting.BillingMode),
				Dimensions: dimensions, BillingExpr: strings.TrimSpace(setting.BillingExpr), TaskProfile: profile}
		}
	}
	return hashCanonical(functional)
}

// TargetAuthorityHash tracks ownership/provenance independently from values.
func TargetAuthorityHash(contract domain.GroupModelPricingContract) (string, error) {
	type authority struct {
		Group          string `json:"group"`
		Model          string `json:"model"`
		Source         string `json:"source"`
		SourceRevision string `json:"source_revision"`
	}
	items := make([]authority, 0)
	for group, models := range contract.Groups {
		for model, setting := range models {
			items = append(items, authority{Group: strings.TrimSpace(group), Model: strings.TrimSpace(model),
				Source: strings.TrimSpace(setting.Source), SourceRevision: strings.TrimSpace(setting.SourceRevision)})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Group != items[j].Group {
			return items[i].Group < items[j].Group
		}
		return items[i].Model < items[j].Model
	})
	return hashCanonical(items)
}

// TargetModelFunctionalHash is the ownership comparison hash for one target
// group-model entry. Source provenance is checked separately so a timestamp or
// source revision update cannot masquerade as a price change.
func TargetModelFunctionalHash(group, model string, setting domain.GroupModelPricingSetting) (string, error) {
	group = strings.TrimSpace(group)
	model = strings.TrimSpace(model)
	if group == "" || model == "" {
		return "", errors.New("target group and model identity are required")
	}
	dimensions, err := canonicalSettingDimensions(setting)
	if err != nil {
		return "", err
	}
	profile := cloneTaskProfile(setting.TaskProfile)
	if err := validateTaskProfile(profile); err != nil {
		return "", err
	}
	functional := struct {
		Group       string                        `json:"group"`
		Model       string                        `json:"model"`
		BillingMode string                        `json:"billing_mode"`
		Dimensions  map[string]string             `json:"dimensions"`
		BillingExpr string                        `json:"billing_expr,omitempty"`
		TaskProfile *domain.GroupModelTaskProfile `json:"task_profile,omitempty"`
	}{Group: group, Model: model, BillingMode: strings.TrimSpace(setting.BillingMode), Dimensions: dimensions,
		BillingExpr: strings.TrimSpace(setting.BillingExpr), TaskProfile: profile}
	return hashCanonical(functional)
}

// TargetChannelModelFunctionalHash tracks one membership independently from
// unrelated models in the same channel. Whole-channel concurrency remains
// protected by the target managed-state CAS hash.
func TargetChannelModelFunctionalHash(channelID int64, model string, present bool) (string, error) {
	model = strings.TrimSpace(model)
	if channelID <= 0 || model == "" {
		return "", errors.New("channel and model identity are required")
	}
	return hashCanonical(struct {
		ChannelID int64  `json:"channel_id"`
		Model     string `json:"model"`
		Present   bool   `json:"present"`
	}{ChannelID: channelID, Model: model, Present: present})
}

func canonicalModelForGroup(models []domain.ModelOffer, group domain.GroupOffer, modelID string) (CanonicalModel, error) {
	groupKeys := map[string]struct{}{}
	for _, value := range []string{group.ExternalID, group.Name} {
		if value = strings.TrimSpace(value); value != "" {
			groupKeys[value] = struct{}{}
		}
	}
	exact := make([]domain.ModelOffer, 0, 1)
	unscoped := make([]domain.ModelOffer, 0, 1)
	for _, model := range models {
		if strings.TrimSpace(model.ExternalID) != modelID && strings.TrimSpace(model.Name) != modelID {
			continue
		}
		scope := strings.TrimSpace(model.Group)
		if _, ok := groupKeys[scope]; ok && scope != "" {
			exact = append(exact, model)
		} else if scope == "" {
			unscoped = append(unscoped, model)
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = unscoped
	}
	if len(candidates) == 0 {
		return CanonicalModel{}, errors.New("model is absent from the selected group")
	}
	var normalized CanonicalModel
	var identity string
	for index, candidate := range candidates {
		current, err := normalizeModelOffer(candidate, modelID)
		if err != nil {
			return CanonicalModel{}, err
		}
		encoded, _ := json.Marshal(current)
		if index == 0 {
			normalized = current
			identity = string(encoded)
			continue
		}
		if identity != string(encoded) {
			return CanonicalModel{}, errors.New("multiple conflicting group-scoped model records were found")
		}
	}
	return normalized, nil
}

func normalizeModelOffer(model domain.ModelOffer, fallbackID string) (CanonicalModel, error) {
	result := CanonicalModel{SourceModelID: firstValue(model.ExternalID, fallbackID),
		SourceExposedName: firstValue(model.Name, model.ExternalID, fallbackID),
		BillingMode:       membershipOnlyBillingMode,
		Currency:          strings.ToUpper(firstValue(model.PriceFunction.Currency, model.Currency))}
	result.UpstreamRequestName = firstValue(model.Metadata["upstream_request_name"], model.Metadata["request_name"],
		model.Metadata["model_mapping_target"], model.ExternalID, fallbackID)
	if strings.TrimSpace(model.PriceFunction.Expression) != "" || strings.TrimSpace(model.BillingExpr) != "" ||
		strings.EqualFold(strings.TrimSpace(model.BillingMode), "tiered_expr") {
		return result, nil
	}
	dimensions, err := pricing.MonetaryDimensions(model)
	if err != nil {
		return result, nil
	}
	currency := strings.ToUpper(firstValue(model.PriceFunction.Currency, model.Currency))
	if currency == "" {
		return result, nil
	}
	result.Currency = currency
	tokenDimensions, taskDimensions := 0, 0
	for kind, raw := range dimensions {
		if strings.HasPrefix(kind, "custom:") {
			return membershipOnlyModel(result), nil
		}
		canonicalKind := kind
		duration := 0
		if kind == "fixed_fee" {
			duration, err = fixedDuration(model.Metadata)
			if err != nil {
				return membershipOnlyModel(result), nil
			}
			if duration > 0 {
				canonicalKind = "fixed_duration"
			} else {
				canonicalKind = "per_request"
			}
		} else if kind == "per_request" {
			duration, err = fixedDuration(model.Metadata)
			if err != nil {
				return membershipOnlyModel(result), nil
			}
			if duration > 0 {
				canonicalKind = "fixed_duration"
			}
		}
		unit, ok := dimensionUnits[canonicalKind]
		if !ok {
			return membershipOnlyModel(result), nil
		}
		canonical, err := canonicalNonnegativeDecimal(raw, canonicalKind)
		if err != nil {
			return membershipOnlyModel(result), nil
		}
		result.PriceDimensions = append(result.PriceDimensions, PriceDimension{Kind: canonicalKind, Unit: unit,
			DurationSeconds: duration, DecimalValue: canonical})
		if strings.HasSuffix(canonicalKind, "_per_1m") {
			tokenDimensions++
		} else {
			taskDimensions++
		}
	}
	if tokenDimensions > 0 && taskDimensions > 0 {
		return membershipOnlyModel(result), nil
	}
	if tokenDimensions > 0 {
		result.BillingMode = "token_vector"
	} else if taskDimensions == 1 {
		result.BillingMode = "fixed_price"
	} else {
		return membershipOnlyModel(result), nil
	}
	sort.Slice(result.PriceDimensions, func(i, j int) bool {
		return dimensionIdentity(result.PriceDimensions[i]) < dimensionIdentity(result.PriceDimensions[j])
	})
	return result, nil
}

func membershipOnlyModel(model CanonicalModel) CanonicalModel {
	model.BillingMode = membershipOnlyBillingMode
	model.PriceDimensions = nil
	return model
}

func normalizeCanonicalGroup(group CanonicalGroup) (CanonicalGroup, error) {
	group.SourceGroupID = strings.TrimSpace(group.SourceGroupID)
	group.SourceGroupName = strings.TrimSpace(group.SourceGroupName)
	if group.SourceGroupID == "" || strings.ContainsRune(group.SourceGroupID, '\x00') {
		return CanonicalGroup{}, errors.New("source group ID is required")
	}
	var err error
	group.RawGroupRatio, err = canonicalNonnegativeDecimal(group.RawGroupRatio, "raw group ratio")
	if err != nil {
		return CanonicalGroup{}, err
	}
	group.RechargeValueFactor, err = canonicalPositiveDefault(group.RechargeValueFactor, "1", "recharge value factor")
	if err != nil {
		return CanonicalGroup{}, err
	}
	for modelIndex := range group.Models {
		model := &group.Models[modelIndex]
		model.SourceModelID = strings.TrimSpace(model.SourceModelID)
		model.SourceExposedName = strings.TrimSpace(model.SourceExposedName)
		model.UpstreamRequestName = strings.TrimSpace(model.UpstreamRequestName)
		model.Currency = strings.ToUpper(strings.TrimSpace(model.Currency))
		if model.SourceModelID == "" || model.UpstreamRequestName == "" {
			return CanonicalGroup{}, errors.New("canonical model identity and request name are required")
		}
		if model.BillingMode == membershipOnlyBillingMode {
			if len(model.PriceDimensions) != 0 {
				return CanonicalGroup{}, errors.New("membership-only source model cannot contain price dimensions")
			}
			continue
		}
		if model.Currency == "" {
			return CanonicalGroup{}, errors.New("priced canonical model currency is required")
		}
		for index := range model.PriceDimensions {
			normalized, normalizeErr := normalizePriceDimension(model.PriceDimensions[index])
			if normalizeErr != nil {
				return CanonicalGroup{}, normalizeErr
			}
			model.PriceDimensions[index] = normalized
		}
		sort.Slice(model.PriceDimensions, func(i, j int) bool {
			return dimensionIdentity(model.PriceDimensions[i]) < dimensionIdentity(model.PriceDimensions[j])
		})
	}
	sort.Slice(group.Models, func(i, j int) bool { return group.Models[i].SourceModelID < group.Models[j].SourceModelID })
	return group, nil
}

func normalizePriceDimension(dimension PriceDimension) (PriceDimension, error) {
	dimension.Kind = strings.TrimSpace(dimension.Kind)
	dimension.Unit = strings.TrimSpace(dimension.Unit)
	expectedUnit, ok := dimensionUnits[dimension.Kind]
	if !ok {
		return PriceDimension{}, fmt.Errorf("unsupported price dimension %q", dimension.Kind)
	}
	if dimension.Unit == "" {
		dimension.Unit = expectedUnit
	}
	if dimension.Unit != expectedUnit {
		return PriceDimension{}, fmt.Errorf("dimension %s requires unit %s", dimension.Kind, expectedUnit)
	}
	if dimension.Kind == "fixed_duration" {
		if dimension.DurationSeconds < 1 || dimension.DurationSeconds > 86400 {
			return PriceDimension{}, errors.New("fixed duration must be between 1 and 86400 seconds")
		}
	} else if dimension.DurationSeconds != 0 {
		return PriceDimension{}, fmt.Errorf("dimension %s cannot contain a duration", dimension.Kind)
	}
	value, err := canonicalNonnegativeDecimal(dimension.DecimalValue, dimension.Kind)
	if err != nil {
		return PriceDimension{}, err
	}
	dimension.DecimalValue = value
	return dimension, nil
}

func canonicalSettingDimensions(setting domain.GroupModelPricingSetting) (map[string]string, error) {
	values := map[string]*string{
		"model_price": setting.ModelPrice, "model_ratio": setting.ModelRatio,
		"completion_ratio": setting.CompletionRatio, "cache_ratio": setting.CacheRatio,
		"cache_creation_ratio": setting.CacheCreationRatio, "cache_creation_5m_ratio": setting.CacheCreation5mRatio,
		"cache_creation_1h_ratio": setting.CacheCreation1hRatio, "image_ratio": setting.ImageRatio,
		"audio_ratio": setting.AudioRatio, "audio_completion_ratio": setting.AudioCompletionRatio,
	}
	result := make(map[string]string)
	for name, pointer := range values {
		if pointer == nil {
			continue
		}
		canonical, err := canonicalNonnegativeDecimal(*pointer, name)
		if err != nil {
			return nil, err
		}
		result[name], _ = decimalnorm.String(canonical)
	}
	return result, nil
}

func validateTaskProfile(profile *domain.GroupModelTaskProfile) error {
	if profile == nil {
		return nil
	}
	switch strings.TrimSpace(profile.Unit) {
	case "per_request", "per_second", "per_image", "per_frame":
		if profile.FixedDurationSeconds != 0 {
			return errors.New("non-duration task profile cannot contain fixed_duration_seconds")
		}
	case "fixed_duration":
		if profile.FixedDurationSeconds < 1 || profile.FixedDurationSeconds > 86400 {
			return errors.New("fixed-duration task profile has an invalid duration")
		}
	default:
		return fmt.Errorf("unsupported task unit %q", profile.Unit)
	}
	return nil
}

func cloneTaskProfile(profile *domain.GroupModelTaskProfile) *domain.GroupModelTaskProfile {
	if profile == nil {
		return nil
	}
	copy := *profile
	copy.Unit = strings.TrimSpace(copy.Unit)
	return &copy
}

func fixedDuration(metadata map[string]string) (int, error) {
	raw := firstValue(metadata["fixed_duration_seconds"], metadata["duration_seconds"], metadata["billing_duration_seconds"])
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 86400 {
		return 0, errors.New("fixed_duration_seconds must be an integer between 1 and 86400")
	}
	return value, nil
}

func canonicalPositiveDefault(raw, fallback, name string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	return canonicalPositiveDecimal(raw, name)
}

func canonicalPositiveDecimal(raw, name string) (string, error) {
	value, err := parsePositiveDecimal(raw, name)
	if err != nil {
		return "", err
	}
	return canonicalDecimalValue(value), nil
}

func canonicalNonnegativeDecimal(raw, name string) (string, error) {
	value, err := parseNonnegativeDecimal(raw, name)
	if err != nil {
		return "", err
	}
	return canonicalDecimalValue(value), nil
}

func parsePositiveDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := parseDecimal(raw, name)
	if err != nil || !value.IsPositive() {
		return decimal.Zero, fmt.Errorf("%s must be a positive decimal", name)
	}
	return value, nil
}

func parseNonnegativeDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := parseDecimal(raw, name)
	if err != nil || value.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a non-negative decimal", name)
	}
	return value, nil
}

func parseDecimal(raw, name string) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maximumDecimalInputLength {
		return decimal.Zero, fmt.Errorf("%s must be a bounded decimal", name)
	}
	value, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s must be a decimal", name)
	}
	digits := strings.TrimPrefix(value.Coefficient().String(), "-")
	if len(digits) > maximumDecimalDigits || value.Exponent() < -maximumDecimalExponent || value.Exponent() > maximumDecimalExponent {
		return decimal.Zero, fmt.Errorf("%s exceeds the supported decimal bounds", name)
	}
	return value, nil
}

func canonicalDecimalValue(value decimal.Decimal) string { return value.String() }

func hashCanonical(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func firstValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sortedUniqueStrings(values []string) []string {
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
