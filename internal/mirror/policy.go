package mirror

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const PolicySchemaVersion = 3

const (
	maximumPolicyBindingBytes      = 512
	maximumPolicyPatternBytes      = 8_192
	maximumPolicyPatternComponents = 1_024
	maximumPolicyPatternComponent  = 512
	maximumPolicyExceptionsBytes   = 8_192
	maximumPolicyEnumBytes         = 64
	maximumPolicyDecimalTextBytes  = 128
)

const (
	PriceAnchorSourceBase    = "source_base"
	PriceAnchorEffectiveSale = "effective_sale"

	PolicyKindCompleteMirror = "complete_mirror"
	PolicyKindLegacyGroup    = "legacy_group_ratio"
	PolicyKindLegacyContract = "legacy_group_model_contract"
)

// Rule is an exact decimal transform. Empty values are allowed only for the
// same transform.
type Rule struct {
	Type  string `json:"type"`
	Value string `json:"value,omitempty"`
}

// Policy is the normalized schema-v3 policy consumed by mirror planning. The
// booleans are concrete only after DecodePolicy has resolved schema defaults.
type Policy struct {
	PolicyUUID         string   `json:"policy_uuid"`
	PolicyVersion      int64    `json:"policy_version"`
	SchemaVersion      int      `json:"schema_version"`
	LegacyPolicyID     int64    `json:"legacy_policy_id,omitempty"`
	SupplierID         int64    `json:"supplier_id"`
	SourceGroup        string   `json:"source_group"`
	SourceModelPattern string   `json:"source_model_pattern"`
	ExcludedModels     []string `json:"excluded_models,omitempty"`
	TargetSiteID       int64    `json:"target_site_id"`
	TargetGroup        string   `json:"target_group"`
	Enabled            bool     `json:"enabled"`
	Kind               string   `json:"kind"`
	PriceAnchor        string   `json:"price_anchor"`
	SyncGroupRatio     bool     `json:"sync_group_ratio"`
	GroupRatioRule     Rule     `json:"group_ratio_rule"`
	SyncModelPrices    bool     `json:"sync_model_prices"`
	ModelPriceRule     Rule     `json:"model_price_rule"`
	SyncBillingMode    bool     `json:"sync_billing_mode"`
	SyncModelAdditions bool     `json:"sync_model_additions"`
	SyncModelRemovals  bool     `json:"sync_model_removals"`
	ModelScopeMode     string   `json:"model_scope_mode"`
	EmptyGroupAction   string   `json:"empty_group_action"`
	ExecutionMode      string   `json:"execution_mode"`
	LegacyRuleTarget   string   `json:"legacy_rule_target,omitempty"`
	MinimumRatio       string   `json:"minimum_ratio,omitempty"`
	MaximumRatio       string   `json:"maximum_ratio,omitempty"`
	RoundingStep       string   `json:"rounding_step,omitempty"`
	OnlyFollowIncrease bool     `json:"only_follow_increase"`
	OnlyFollowDecrease bool     `json:"only_follow_decrease"`
}

// PolicyDocument preserves presence information while decoding schema-v3.
// Missing booleans therefore cannot silently become false.
type PolicyDocument struct {
	PolicyUUID         string   `json:"policy_uuid"`
	PolicyVersion      int64    `json:"policy_version"`
	SchemaVersion      int      `json:"schema_version"`
	SupplierID         int64    `json:"supplier_id"`
	SourceGroup        string   `json:"source_group"`
	SourceModelPattern string   `json:"source_model_pattern"`
	ExcludedModels     []string `json:"excluded_models,omitempty"`
	TargetSiteID       int64    `json:"target_site_id"`
	TargetGroup        string   `json:"target_group"`
	Enabled            *bool    `json:"enabled"`
	Kind               string   `json:"kind"`
	PriceAnchor        string   `json:"price_anchor"`
	SyncGroupRatio     *bool    `json:"sync_group_ratio"`
	GroupRatioRule     Rule     `json:"group_ratio_rule"`
	SyncModelPrices    *bool    `json:"sync_model_prices"`
	ModelPriceRule     Rule     `json:"model_price_rule"`
	SyncBillingMode    *bool    `json:"sync_billing_mode"`
	SyncModelAdditions *bool    `json:"sync_model_additions"`
	SyncModelRemovals  *bool    `json:"sync_model_removals"`
	ModelScopeMode     string   `json:"model_scope_mode"`
	EmptyGroupAction   string   `json:"empty_group_action"`
	ExecutionMode      string   `json:"execution_mode"`
	LegacyRuleTarget   string   `json:"legacy_rule_target,omitempty"`
	MinimumRatio       string   `json:"minimum_ratio,omitempty"`
	MaximumRatio       string   `json:"maximum_ratio,omitempty"`
	RoundingStep       string   `json:"rounding_step,omitempty"`
	OnlyFollowIncrease bool     `json:"only_follow_increase"`
	OnlyFollowDecrease bool     `json:"only_follow_decrease"`
}

type PolicyCapabilities struct {
	CompleteModelSet  bool
	ManagedChannelCAS bool
}

func boolPointer(value bool) *bool { return &value }

// NewCompleteMirrorDocument returns the explicit, user-visible defaults for a
// newly created complete mirror. Callers still assign the immutable UUID.
func NewCompleteMirrorDocument(policyUUID string) PolicyDocument {
	return PolicyDocument{
		PolicyUUID: policyUUID, PolicyVersion: 1, SchemaVersion: PolicySchemaVersion,
		Enabled: boolPointer(true), SourceModelPattern: "*", RoundingStep: defaultRoundingStep,
		ExcludedModels: []string{},
		Kind:           PolicyKindCompleteMirror, PriceAnchor: PriceAnchorSourceBase,
		SyncGroupRatio: boolPointer(false), GroupRatioRule: Rule{Type: "same"},
		SyncModelPrices: boolPointer(true), ModelPriceRule: Rule{Type: "same"},
		SyncBillingMode: boolPointer(true), SyncModelAdditions: boolPointer(true),
		SyncModelRemovals: boolPointer(true), ModelScopeMode: "mirror_group",
		EmptyGroupAction: "remove_managed_models", ExecutionMode: "auto",
	}
}

// DecodePolicy normalizes an explicit schema-v3 document. It deliberately
// rejects older documents: legacy rows must pass through MigrateLegacyPolicy
// so their missing fields use the frozen migration matrix.
func DecodePolicy(document PolicyDocument) (Policy, error) {
	if document.SchemaVersion != PolicySchemaVersion {
		return Policy{}, fmt.Errorf("policy schema_version must be %d", PolicySchemaVersion)
	}
	missing := make([]string, 0)
	for name, value := range map[string]*bool{
		"enabled":              document.Enabled,
		"sync_group_ratio":     document.SyncGroupRatio,
		"sync_model_prices":    document.SyncModelPrices,
		"sync_billing_mode":    document.SyncBillingMode,
		"sync_model_additions": document.SyncModelAdditions,
		"sync_model_removals":  document.SyncModelRemovals,
	} {
		if value == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Policy{}, fmt.Errorf("schema-v3 policy is missing explicit fields: %s", strings.Join(missing, ", "))
	}
	excludedModels, err := normalizeExcludedModels(document.ExcludedModels)
	if err != nil {
		return Policy{}, err
	}
	policy := Policy{
		PolicyUUID: strings.TrimSpace(document.PolicyUUID), PolicyVersion: document.PolicyVersion,
		SchemaVersion: PolicySchemaVersion, SupplierID: document.SupplierID, SourceGroup: strings.TrimSpace(document.SourceGroup),
		SourceModelPattern: strings.TrimSpace(document.SourceModelPattern), ExcludedModels: excludedModels,
		TargetSiteID: document.TargetSiteID,
		TargetGroup:  strings.TrimSpace(document.TargetGroup), Enabled: *document.Enabled, Kind: strings.TrimSpace(document.Kind),
		PriceAnchor: strings.TrimSpace(document.PriceAnchor), SyncGroupRatio: *document.SyncGroupRatio,
		GroupRatioRule: normalizeRule(document.GroupRatioRule), SyncModelPrices: *document.SyncModelPrices,
		ModelPriceRule: normalizeRule(document.ModelPriceRule), SyncBillingMode: *document.SyncBillingMode,
		SyncModelAdditions: *document.SyncModelAdditions, SyncModelRemovals: *document.SyncModelRemovals,
		ModelScopeMode: strings.TrimSpace(document.ModelScopeMode), EmptyGroupAction: strings.TrimSpace(document.EmptyGroupAction),
		ExecutionMode: strings.TrimSpace(document.ExecutionMode), LegacyRuleTarget: strings.TrimSpace(document.LegacyRuleTarget),
		MinimumRatio: strings.TrimSpace(document.MinimumRatio), MaximumRatio: strings.TrimSpace(document.MaximumRatio),
		RoundingStep: strings.TrimSpace(document.RoundingStep), OnlyFollowIncrease: document.OnlyFollowIncrease,
		OnlyFollowDecrease: document.OnlyFollowDecrease,
	}
	policy = NormalizeOwnership(policy)
	if err := ValidatePolicy(policy, PolicyCapabilities{}); err != nil && !errors.Is(err, ErrCapabilityRequired) {
		return Policy{}, err
	}
	return policy, nil
}

// NormalizeOwnership keeps native complete mirrors and pricing mappings as
// independent writers. Complete mirrors own only model membership, billing
// mode and per-group model prices; GroupRatio remains owned by pricing rules.
// Legacy migrated policies keep their frozen behavior.
func NormalizeOwnership(policy Policy) Policy {
	if excludedModels, err := normalizeExcludedModels(policy.ExcludedModels); err == nil {
		policy.ExcludedModels = excludedModels
	}
	if policy.Kind == PolicyKindCompleteMirror {
		policy.SyncGroupRatio = false
		policy.GroupRatioRule = Rule{Type: "same"}
		policy.EmptyGroupAction = "remove_managed_models"
	}
	return policy
}

// MigrateLegacyPolicy applies the Rev3 matrix without activating model-set
// ownership. The stable UUID is supplied by the caller because legacy rows did
// not contain one.
func MigrateLegacyPolicy(policyUUID string, legacy domain.PricingPolicy) (Policy, error) {
	if _, err := uuid.Parse(strings.TrimSpace(policyUUID)); err != nil {
		return Policy{}, errors.New("legacy policy migration requires a valid immutable UUID")
	}
	sourceBasis := strings.ToLower(strings.TrimSpace(legacy.SourceBasis))
	if sourceBasis == "" {
		sourceBasis = "group_ratio"
	}
	result := Policy{
		PolicyUUID: strings.TrimSpace(policyUUID), PolicyVersion: 1, SchemaVersion: PolicySchemaVersion,
		LegacyPolicyID: legacy.ID, SupplierID: legacy.SupplierID, SourceGroup: strings.TrimSpace(legacy.SourceGroup),
		SourceModelPattern: strings.TrimSpace(legacy.SourceModelPattern), TargetSiteID: legacy.TargetSiteID,
		TargetGroup: strings.TrimSpace(legacy.TargetGroup), Enabled: legacy.Enabled,
		ExecutionMode: defaultExecutionMode(legacy.ExecutionMode), EmptyGroupAction: "preserve",
		SyncModelAdditions: false, SyncModelRemovals: false,
		MinimumRatio: strings.TrimSpace(legacy.MinimumRatio), MaximumRatio: strings.TrimSpace(legacy.MaximumRatio),
		RoundingStep: strings.TrimSpace(legacy.RoundingStep), OnlyFollowIncrease: legacy.OnlyFollowIncrease,
		OnlyFollowDecrease: legacy.OnlyFollowDecrease,
	}
	switch sourceBasis {
	case "group_ratio", "equivalent_cost":
		result.Kind = PolicyKindLegacyGroup
		result.PriceAnchor = PriceAnchorSourceBase
		result.SyncGroupRatio = true
		result.GroupRatioRule = normalizeRule(Rule{Type: legacy.RuleType, Value: legacy.Value})
		result.SyncModelPrices = false
		result.ModelPriceRule = Rule{Type: "same"}
		result.SyncBillingMode = false
		result.ModelScopeMode = "fixed_allowlist"
		result.LegacyRuleTarget = "group_ratio"
	case "group_model_contract":
		result.Kind = PolicyKindLegacyContract
		result.PriceAnchor = PriceAnchorEffectiveSale
		result.SyncGroupRatio = false
		result.GroupRatioRule = Rule{Type: "same"}
		result.SyncModelPrices = true
		result.ModelPriceRule = normalizeRule(Rule{Type: legacy.RuleType, Value: legacy.Value})
		result.SyncBillingMode = true
		result.ModelScopeMode = "fixed_allowlist"
		result.LegacyRuleTarget = "model_price"
	default:
		return Policy{}, fmt.Errorf("legacy source_basis %q has no schema-v3 migration", legacy.SourceBasis)
	}
	if err := ValidatePolicy(result, PolicyCapabilities{}); err != nil && !errors.Is(err, ErrCapabilityRequired) {
		return Policy{}, err
	}
	return result, nil
}

var ErrCapabilityRequired = errors.New("mirror policy capability is required")

// ValidatePolicy validates both static combinations and optional runtime
// capabilities. A zero PolicyCapabilities value performs static validation;
// callers enabling automatic model-set changes must validate again with the
// actual source and target capabilities.
func ValidatePolicy(policy Policy, capabilities PolicyCapabilities) error {
	if err := validatePolicyTextBounds(policy); err != nil {
		return err
	}
	if policy.SchemaVersion != PolicySchemaVersion {
		return fmt.Errorf("unsupported policy schema_version %d", policy.SchemaVersion)
	}
	parsedUUID, uuidErr := uuid.Parse(policy.PolicyUUID)
	if uuidErr != nil || parsedUUID.String() != policy.PolicyUUID {
		return errors.New("policy_uuid must be a valid UUID")
	}
	if policy.PolicyVersion < 1 {
		return errors.New("policy_version must be at least 1")
	}
	if policy.SupplierID < 1 || policy.TargetSiteID < 1 || strings.TrimSpace(policy.SourceGroup) == "" ||
		strings.TrimSpace(policy.TargetGroup) == "" {
		return errors.New("supplier, source group, target site and target group bindings are required")
	}
	if strings.TrimSpace(policy.SourceModelPattern) == "" {
		return errors.New("source_model_pattern is required")
	}
	excludedModels, err := normalizeExcludedModels(policy.ExcludedModels)
	if err != nil || !slices.Equal(excludedModels, policy.ExcludedModels) {
		return errors.New("excluded_models must be a canonical sorted unique model list")
	}
	if policy.OnlyFollowIncrease && policy.OnlyFollowDecrease {
		return errors.New("only-follow-increase and only-follow-decrease cannot both be enabled")
	}
	minimum, err := parseDefaultDecimal(policy.MinimumRatio, "0", "minimum ratio")
	if err != nil || minimum.IsNegative() {
		return errors.New("minimum ratio must be a non-negative decimal")
	}
	maximum, err := parseDefaultDecimal(policy.MaximumRatio, "0", "maximum ratio")
	if err != nil || maximum.IsNegative() || (maximum.IsPositive() && minimum.GreaterThan(maximum)) {
		return errors.New("maximum ratio must be zero or at least the minimum ratio")
	}
	if _, err := parsePositiveDefault(policy.RoundingStep, defaultRoundingStep, "rounding step"); err != nil {
		return err
	}
	if policy.PriceAnchor != PriceAnchorSourceBase && policy.PriceAnchor != PriceAnchorEffectiveSale {
		return fmt.Errorf("unsupported price_anchor %q", policy.PriceAnchor)
	}
	if policy.ExecutionMode != "auto" && policy.ExecutionMode != "approval" {
		return fmt.Errorf("unsupported execution_mode %q", policy.ExecutionMode)
	}
	if policy.ModelScopeMode != "mirror_group" && policy.ModelScopeMode != "fixed_allowlist" {
		return fmt.Errorf("unsupported model_scope_mode %q", policy.ModelScopeMode)
	}
	switch policy.EmptyGroupAction {
	case "preserve", "disable_managed_channel", "remove_managed_models":
	default:
		return fmt.Errorf("unsupported empty_group_action %q", policy.EmptyGroupAction)
	}
	if policy.SyncGroupRatio {
		if err := validateRule(policy.GroupRatioRule); err != nil {
			return fmt.Errorf("group ratio rule: %w", err)
		}
	}
	if policy.SyncModelPrices {
		if err := validateRule(policy.ModelPriceRule); err != nil {
			return fmt.Errorf("model price rule: %w", err)
		}
		if policy.PriceAnchor == PriceAnchorSourceBase && !isSourceBaseModelTransform(policy.ModelPriceRule.Type) {
			return errors.New("source_base model price rule must be same, add, multiply, or fixed; margin rules belong to the group ratio")
		}
		if !policy.SyncBillingMode {
			return errors.New("model price synchronization requires billing-mode synchronization")
		}
	}
	setChanges := policy.SyncModelAdditions || policy.SyncModelRemovals
	if setChanges && policy.ModelScopeMode != "mirror_group" {
		return errors.New("model additions or removals require mirror_group scope")
	}
	if setChanges && (!capabilities.CompleteModelSet || (policy.ExecutionMode == "auto" && !capabilities.ManagedChannelCAS)) {
		return fmt.Errorf("%w: complete source proof and managed-channel CAS are required for model-set changes", ErrCapabilityRequired)
	}
	return nil
}

func validatePolicyTextBounds(policy Policy) error {
	if err := canonicalBoundedPolicyText(policy.PolicyUUID, 36, true, true); err != nil || len(policy.PolicyUUID) != 36 {
		return errors.New("policy_uuid must be a canonical 36-byte UUID")
	}
	for name, value := range map[string]string{
		"source_group": policy.SourceGroup,
		"target_group": policy.TargetGroup,
	} {
		if err := canonicalBoundedPolicyText(value, maximumPolicyBindingBytes, true, false); err != nil {
			return fmt.Errorf("%s must be canonical UTF-8 within %d bytes", name, maximumPolicyBindingBytes)
		}
	}
	if err := canonicalBoundedPolicyText(policy.SourceModelPattern, maximumPolicyPatternBytes, true, false); err != nil {
		return fmt.Errorf("source_model_pattern must be canonical UTF-8 within %d bytes", maximumPolicyPatternBytes)
	}
	components := strings.Split(policy.SourceModelPattern, ",")
	if len(components) > maximumPolicyPatternComponents {
		return errors.New("source_model_pattern has too many components")
	}
	for _, component := range components {
		component = strings.TrimSpace(component)
		if err := canonicalBoundedPolicyText(component, maximumPolicyPatternComponent, true, false); err != nil {
			return errors.New("source_model_pattern contains an invalid component")
		}
	}
	totalExceptionBytes := 0
	if len(policy.ExcludedModels) > maximumPolicyPatternComponents {
		return errors.New("excluded_models has too many entries")
	}
	for _, model := range policy.ExcludedModels {
		if err := canonicalBoundedPolicyText(model, maximumPolicyPatternComponent, true, false); err != nil {
			return errors.New("excluded_models contains an invalid model identity")
		}
		totalExceptionBytes += len(model)
	}
	if totalExceptionBytes > maximumPolicyExceptionsBytes {
		return fmt.Errorf("excluded_models exceeds %d bytes", maximumPolicyExceptionsBytes)
	}
	for name, value := range map[string]string{
		"kind": policy.Kind, "price_anchor": policy.PriceAnchor,
		"model_scope_mode": policy.ModelScopeMode, "empty_group_action": policy.EmptyGroupAction,
		"execution_mode": policy.ExecutionMode, "legacy_rule_target": policy.LegacyRuleTarget,
		"group_ratio_rule.type": policy.GroupRatioRule.Type, "model_price_rule.type": policy.ModelPriceRule.Type,
	} {
		if value == "" && (name == "legacy_rule_target") {
			continue
		}
		if err := canonicalBoundedPolicyText(value, maximumPolicyEnumBytes, true, true); err != nil {
			return fmt.Errorf("%s must be canonical bounded ASCII", name)
		}
	}
	for name, value := range map[string]string{
		"group_ratio_rule.value": policy.GroupRatioRule.Value,
		"model_price_rule.value": policy.ModelPriceRule.Value,
		"minimum_ratio":          policy.MinimumRatio, "maximum_ratio": policy.MaximumRatio,
		"rounding_step": policy.RoundingStep,
	} {
		if value == "" {
			continue
		}
		if err := canonicalBoundedPolicyText(value, maximumPolicyDecimalTextBytes, true, true); err != nil || !validBoundedDecimalGrammar(value) {
			return fmt.Errorf("%s must use bounded decimal syntax", name)
		}
	}
	return nil
}

func normalizeExcludedModels(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return nil, errors.New("excluded_models contains an empty or invalid model identity")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func canonicalBoundedPolicyText(value string, maximum int, required, ascii bool) error {
	if (required && value == "") || len(value) > maximum || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || value != strings.TrimSpace(value) {
		return errors.New("invalid bounded text")
	}
	if ascii {
		for index := 0; index < len(value); index++ {
			if value[index] < 0x20 || value[index] > 0x7e {
				return errors.New("text is not printable ASCII")
			}
		}
	}
	return nil
}

func validBoundedDecimalGrammar(value string) bool {
	if value == "" || len(value) > maximumPolicyDecimalTextBytes {
		return false
	}
	index := 0
	if value[0] == '-' || value[0] == '+' {
		index++
	}
	integerDigits := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		integerDigits++
		index++
	}
	if integerDigits == 0 {
		return false
	}
	if index == len(value) {
		return true
	}
	if value[index] != '.' {
		return false
	}
	index++
	fractionDigits := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		fractionDigits++
		index++
	}
	return fractionDigits > 0 && index == len(value)
}

func isSourceBaseModelTransform(ruleType string) bool {
	switch ruleType {
	case "same", "add", "multiply", "fixed":
		return true
	default:
		return false
	}
}

func validateRule(rule Rule) error {
	switch rule.Type {
	case "same":
		if rule.Value != "" {
			return errors.New("same transform cannot contain a value")
		}
	case "add", "multiply", "fixed", "target_margin", "markup", "subsidy_rate":
		if strings.TrimSpace(rule.Value) == "" {
			return fmt.Errorf("%s transform requires a decimal value", rule.Type)
		}
		value, err := parseDecimal(rule.Value, rule.Type+" transform value")
		if err != nil {
			return err
		}
		switch rule.Type {
		case "multiply", "fixed":
			if value.IsNegative() {
				return fmt.Errorf("%s transform value cannot be negative", rule.Type)
			}
		case "target_margin":
			if value.IsNegative() || value.GreaterThanOrEqual(decimalOne()) {
				return errors.New("target_margin must be at least 0 and less than 1")
			}
		case "markup":
			if value.LessThan(decimalMinusOne()) {
				return errors.New("markup must be at least -1")
			}
		case "subsidy_rate":
			if value.IsNegative() || value.GreaterThan(decimalOne()) {
				return errors.New("subsidy_rate must be between 0 and 1")
			}
		}
	default:
		return fmt.Errorf("unsupported transform %q", rule.Type)
	}
	return nil
}

func decimalOne() decimal.Decimal { return decimal.NewFromInt(1) }

func decimalMinusOne() decimal.Decimal { return decimal.NewFromInt(-1) }

func normalizeRule(rule Rule) Rule {
	rule.Type = strings.ToLower(strings.TrimSpace(rule.Type))
	rule.Value = strings.TrimSpace(rule.Value)
	if rule.Type == "" {
		rule.Type = "same"
	}
	return rule
}

func defaultExecutionMode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "auto" {
		return "auto"
	}
	return "approval"
}
