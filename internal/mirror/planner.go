package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const (
	SourceGroupPresent SourceGroupState = "present"
	SourceGroupEmpty   SourceGroupState = "empty"
	SourceGroupMissing SourceGroupState = "missing"

	PhaseRemovalQuiesce  MirrorPlanPhase = "removal_quiesce"
	PhaseContractCAS     MirrorPlanPhase = "contract_cas"
	PhaseAdditionsExpose MirrorPlanPhase = "additions_expose"
	PhaseReadback        MirrorPlanPhase = "readback"

	DecisionNoChange         MirrorPlanDecision = "no_change"
	DecisionAutomatic        MirrorPlanDecision = "automatic"
	DecisionApprovalRequired MirrorPlanDecision = "approval_required"
	DecisionDisabled         MirrorPlanDecision = "policy_disabled"
	DecisionBlocked          MirrorPlanDecision = "blocked"

	DiffGroupRatio   MirrorDiffKind = "group_ratio"
	DiffModelPrice   MirrorDiffKind = "model_price"
	DiffBilling      MirrorDiffKind = "billing"
	DiffAddition     MirrorDiffKind = "addition"
	DiffRemoval      MirrorDiffKind = "removal"
	DiffChannelState MirrorDiffKind = "channel_state"

	defaultMaximumAdditions          = 100
	defaultMaximumAdditionPercentage = "0.5"
	defaultMaximumRemovals           = 25
	defaultMaximumRemovalPercentage  = "0.25"
	defaultMaximumPriceIncrease      = "10"
	hardMaximumContractEntries       = 50000
	hardMaximumContractBytes         = 8 * 1024 * 1024
)

type SourceGroupState string

type MirrorPlanPhase string

type MirrorPlanDecision string

type MirrorDiffKind string

// DisasterLimits are continuing-automation limits. Zero values select the
// Rev3 defaults; contract limits are always clamped to the hard ceilings.
type DisasterLimits struct {
	MaximumAdditions          int    `json:"maximum_additions"`
	MaximumAdditionPercentage string `json:"maximum_addition_percentage"`
	MaximumRemovals           int    `json:"maximum_removals"`
	MaximumRemovalPercentage  string `json:"maximum_removal_percentage"`
	MaximumPriceIncrease      string `json:"maximum_price_increase"`
	MaximumContractEntries    int    `json:"maximum_contract_entries"`
	MaximumContractBytes      int    `json:"maximum_contract_bytes"`
}

func DefaultDisasterLimits() DisasterLimits {
	return DisasterLimits{
		MaximumAdditions: defaultMaximumAdditions, MaximumAdditionPercentage: defaultMaximumAdditionPercentage,
		MaximumRemovals: defaultMaximumRemovals, MaximumRemovalPercentage: defaultMaximumRemovalPercentage,
		MaximumPriceIncrease: defaultMaximumPriceIncrease, MaximumContractEntries: hardMaximumContractEntries,
		MaximumContractBytes: hardMaximumContractBytes,
	}
}

// OwnedModelState is the complete set of deletion candidates. A target model
// absent from this input is foreign and is never removed, even on an empty
// upstream group. Contract deletion is also suppressed while another channel
// is known to expose the model.
type OwnedModelState struct {
	TargetModel              string `json:"target_model"`
	ContractOwned            bool   `json:"contract_owned"`
	MembershipOwned          bool   `json:"membership_owned"`
	ForeignChannelReferences int    `json:"foreign_channel_references"`
}

type CompleteMirrorPlannerInput struct {
	Policy                  Policy
	SourceState             SourceGroupState
	SourceGroup             CanonicalGroup
	SourceRevision          string
	SourceObservedAtUnixMS  int64
	Factors                 CashFactors
	CurrentGroupRatio       string
	CurrentContract         domain.GroupModelPricingContract
	CurrentChannelModels    []string
	CurrentChannelEnabled   bool
	ChannelDisabledByPolicy bool
	ChannelStatusAuthority  string
	OwnedModels             []OwnedModelState
	Limits                  DisasterLimits
	InitialApprovedListing  bool
}

type MirrorFunctionalDiff struct {
	Phase  MirrorPlanPhase `json:"phase"`
	Kind   MirrorDiffKind  `json:"kind"`
	Model  string          `json:"model,omitempty"`
	Field  string          `json:"field"`
	Before string          `json:"before,omitempty"`
	After  string          `json:"after,omitempty"`
}

type MirrorPlanGate struct {
	Code    string `json:"code"`
	Scope   string `json:"scope,omitempty"`
	Actual  string `json:"actual"`
	Limit   string `json:"limit"`
	Message string `json:"message"`
}

type MirrorPlanOperation struct {
	Kind   string   `json:"kind"`
	Models []string `json:"models,omitempty"`
}

type MirrorPlanStage struct {
	Phase      MirrorPlanPhase       `json:"phase"`
	Operations []MirrorPlanOperation `json:"operations"`
}

type CompleteMirrorPlan struct {
	PolicyUUID               string                           `json:"policy_uuid"`
	PolicyVersion            int64                            `json:"policy_version"`
	Decision                 MirrorPlanDecision               `json:"decision"`
	NoChange                 bool                             `json:"no_change"`
	ApprovalRequired         bool                             `json:"approval_required"`
	Blocked                  bool                             `json:"blocked"`
	ExpectedGroupRatio       string                           `json:"expected_group_ratio,omitempty"`
	DesiredGroupRatio        string                           `json:"desired_group_ratio,omitempty"`
	ExpectedContractRevision string                           `json:"expected_contract_revision,omitempty"`
	ExpectedContract         domain.GroupModelPricingContract `json:"expected_contract"`
	DesiredContract          domain.GroupModelPricingContract `json:"desired_contract"`
	ExpectedChannelModels    []string                         `json:"expected_channel_models"`
	DesiredChannelModels     []string                         `json:"desired_channel_models"`
	ExpectedChannelEnabled   bool                             `json:"expected_channel_enabled"`
	DesiredChannelEnabled    bool                             `json:"desired_channel_enabled"`
	RemovedContractModels    []string                         `json:"removed_contract_models,omitempty"`
	ExpectedFunctionalHash   string                           `json:"expected_functional_hash"`
	DesiredFunctionalHash    string                           `json:"desired_functional_hash"`
	SourceSemanticHash       string                           `json:"source_semantic_hash,omitempty"`
	ContractEntries          int                              `json:"contract_entries"`
	ContractBytes            int                              `json:"contract_bytes"`
	Diffs                    []MirrorFunctionalDiff           `json:"diffs"`
	ApprovalGates            []MirrorPlanGate                 `json:"approval_gates,omitempty"`
	BlockingGates            []MirrorPlanGate                 `json:"blocking_gates,omitempty"`
	Stages                   []MirrorPlanStage                `json:"stages,omitempty"`
}

type preparedMirrorModel struct {
	model       CanonicalModel
	targetModel string
	source      map[string]decimal.Decimal
	base        map[string]decimal.Decimal
}

// PlanCompleteMirror is deterministic and side-effect free. Its caller owns
// evidence confirmation, target CAS and durable intent journaling; this
// function only turns already trusted snapshots into an exact guarded plan.
func PlanCompleteMirror(input CompleteMirrorPlannerInput) (CompleteMirrorPlan, error) {
	input.Policy = NormalizeOwnership(input.Policy)
	if err := validatePlannerPolicy(input.Policy); err != nil {
		return CompleteMirrorPlan{}, err
	}
	limits, err := normalizeDisasterLimits(input.Limits)
	if err != nil {
		return CompleteMirrorPlan{}, err
	}
	currentModels, err := normalizeModelNames(input.CurrentChannelModels)
	if err != nil {
		return CompleteMirrorPlan{}, fmt.Errorf("current channel models: %w", err)
	}
	owned, err := normalizeOwnedModels(input.OwnedModels)
	if err != nil {
		return CompleteMirrorPlan{}, err
	}
	currentContract, err := cloneContract(input.CurrentContract)
	if err != nil {
		return CompleteMirrorPlan{}, err
	}
	desiredContract, err := cloneContract(input.CurrentContract)
	if err != nil {
		return CompleteMirrorPlan{}, err
	}
	if desiredContract.Groups == nil {
		desiredContract.Groups = map[string]map[string]domain.GroupModelPricingSetting{}
	}

	plan := CompleteMirrorPlan{
		PolicyUUID: input.Policy.PolicyUUID, PolicyVersion: input.Policy.PolicyVersion,
		ExpectedContractRevision: currentContract.Revision, ExpectedContract: currentContract,
		ExpectedChannelModels: append([]string(nil), currentModels...), ExpectedChannelEnabled: input.CurrentChannelEnabled,
		ExpectedGroupRatio: strings.TrimSpace(input.CurrentGroupRatio), DesiredGroupRatio: strings.TrimSpace(input.CurrentGroupRatio),
		DesiredChannelModels: append([]string(nil), currentModels...), DesiredChannelEnabled: input.CurrentChannelEnabled,
	}

	state := input.SourceState
	if state == "" {
		state = SourceGroupPresent
	}
	if state != SourceGroupPresent && state != SourceGroupEmpty && state != SourceGroupMissing {
		return CompleteMirrorPlan{}, fmt.Errorf("unsupported source group state %q", state)
	}

	var source CanonicalGroup
	var prepared []preparedMirrorModel
	if state != SourceGroupMissing {
		source, err = normalizeCanonicalGroup(input.SourceGroup)
		if err != nil {
			return CompleteMirrorPlan{}, fmt.Errorf("source group: %w", err)
		}
		if !sourceMatchesPolicy(source, input.Policy.SourceGroup) {
			return CompleteMirrorPlan{}, errors.New("source group identity does not match the policy binding")
		}
		if state == SourceGroupPresent && len(source.Models) == 0 {
			return CompleteMirrorPlan{}, errors.New("present source group must contain at least one model")
		}
		if state == SourceGroupEmpty && len(source.Models) != 0 {
			return CompleteMirrorPlan{}, errors.New("empty source group cannot contain models")
		}
		if state == SourceGroupEmpty {
			plan.SourceSemanticHash, err = EmptyCanonicalSourceSemanticHash(source)
		} else {
			plan.SourceSemanticHash, err = SourceSemanticHash(source)
		}
		if err != nil {
			return CompleteMirrorPlan{}, err
		}
	} else {
		plan.SourceSemanticHash, err = MissingSourceSemanticHash(input.Policy.SourceGroup)
		if err != nil {
			return CompleteMirrorPlan{}, err
		}
	}

	removalDiffs := make([]MirrorFunctionalDiff, 0)
	contractDiffs := make([]MirrorFunctionalDiff, 0)
	additionDiffs := make([]MirrorFunctionalDiff, 0)
	removedMemberships := make([]string, 0)
	removedContracts := make([]string, 0)
	addedMemberships := make([]string, 0)
	contractChanged := false
	groupRatioChanged := false
	disableChannel := false
	enableChannel := false
	authorityGates := make([]MirrorPlanGate, 0, 1)

	if state == SourceGroupPresent {
		prepared, err = prepareMirrorModels(source, input.Policy, input.Factors, input.CurrentGroupRatio)
		if err != nil {
			return CompleteMirrorPlan{}, err
		}
		if len(prepared) == 0 {
			plan.DesiredGroupRatio, err = canonicalPositiveDecimal(input.CurrentGroupRatio, "current group ratio")
		} else {
			plan.DesiredGroupRatio, err = desiredPlannerGroupRatio(source, prepared, input)
		}
		if err != nil {
			return CompleteMirrorPlan{}, err
		}
		if input.Policy.SyncGroupRatio {
			before, beforeErr := canonicalOptionalPositive(input.CurrentGroupRatio, "current group ratio")
			if beforeErr != nil {
				return CompleteMirrorPlan{}, beforeErr
			}
			plan.ExpectedGroupRatio = before
			if before != plan.DesiredGroupRatio {
				groupRatioChanged = true
				contractDiffs = append(contractDiffs, MirrorFunctionalDiff{Phase: PhaseContractCAS, Kind: DiffGroupRatio,
					Field: "group_ratio", Before: before, After: plan.DesiredGroupRatio})
			}
		} else {
			current, currentErr := canonicalPositiveDecimal(input.CurrentGroupRatio, "current group ratio")
			if currentErr != nil {
				return CompleteMirrorPlan{}, currentErr
			}
			plan.ExpectedGroupRatio, plan.DesiredGroupRatio = current, current
		}

		targetSettings, targetGroupWasPresent := desiredContract.Groups[input.Policy.TargetGroup]
		if targetSettings == nil {
			targetSettings = map[string]domain.GroupModelPricingSetting{}
		}
		currentSettings := currentContract.Groups[input.Policy.TargetGroup]
		currentSet := stringSet(currentModels)
		marker := "relayops:policy/" + input.Policy.PolicyUUID
		sourceRevision := strings.TrimSpace(input.SourceRevision)
		if sourceRevision == "" {
			sourceRevision = plan.SourceSemanticHash
		}
		for _, item := range prepared {
			before, existed := currentSettings[item.targetModel]
			_, currentlyExposed := currentSet[item.targetModel]
			if !existed && !currentlyExposed && !input.Policy.SyncModelAdditions {
				continue
			}
			if item.model.BillingMode == membershipOnlyBillingMode {
				if existed {
					targetSettings[item.targetModel] = cloneSetting(before)
				}
				if !currentlyExposed && input.Policy.SyncModelAdditions {
					addedMemberships = append(addedMemberships, item.targetModel)
					additionDiffs = append(additionDiffs, MirrorFunctionalDiff{Phase: PhaseAdditionsExpose,
						Kind: DiffAddition, Model: item.targetModel, Field: "channel_membership", Before: "absent", After: "present"})
				}
				continue
			}
			generated, buildErr := buildPlannerSetting(item, input.Policy, plan.DesiredGroupRatio, input.Factors)
			if buildErr != nil {
				return CompleteMirrorPlan{}, fmt.Errorf("model %q: %w", item.targetModel, buildErr)
			}
			desired, mergeErr := mergePlannerSetting(before, existed, generated, input.Policy)
			if mergeErr != nil {
				return CompleteMirrorPlan{}, fmt.Errorf("model %q: %w", item.targetModel, mergeErr)
			}
			desired.Source = marker
			desired.SourceRevision = sourceRevision
			desired.SourceObservedAtUnixMS = input.SourceObservedAtUnixMS
			if existed {
				equal, equalErr := targetSettingsFunctionallyEqual(input.Policy.TargetGroup, item.targetModel, before, desired)
				if equalErr != nil {
					return CompleteMirrorPlan{}, fmt.Errorf("model %q: %w", item.targetModel, equalErr)
				}
				if equal {
					desired = cloneSetting(before)
				}
			}
			targetSettings[item.targetModel] = desired
			settingDiffs, diffErr := diffPlannerSettings(input.Policy.TargetGroup, item.targetModel, before, existed, desired, true)
			if diffErr != nil {
				return CompleteMirrorPlan{}, diffErr
			}
			if len(settingDiffs) > 0 {
				contractChanged = true
				contractDiffs = append(contractDiffs, settingDiffs...)
				if ownership, found := owned[item.targetModel]; existed && (!found || !ownership.ContractOwned) {
					authorityGates = append(authorityGates, MirrorPlanGate{Code: "contract_takeover_required",
						Scope: item.targetModel, Actual: "foreign", Limit: "owned",
						Message: "changing an existing foreign contract item requires an explicit takeover approval"})
				}
			}
			if !currentlyExposed && input.Policy.SyncModelAdditions {
				addedMemberships = append(addedMemberships, item.targetModel)
				additionDiffs = append(additionDiffs, MirrorFunctionalDiff{Phase: PhaseAdditionsExpose, Kind: DiffAddition,
					Model: item.targetModel, Field: "channel_membership", Before: "absent", After: "present"})
			}
		}

		sourceSet := make(map[string]struct{}, len(prepared))
		for _, item := range prepared {
			sourceSet[item.targetModel] = struct{}{}
		}
		if input.Policy.SyncModelRemovals {
			removedMemberships, removedContracts, removalDiffs, contractChanged, err = applyOwnedRemovals(
				input.Policy.TargetGroup, sourceSet, currentSet, currentSettings, desiredContract, owned,
				excludedModelSet(input.Policy), contractChanged)
			if err != nil {
				return CompleteMirrorPlan{}, err
			}
		}
		if targetGroupWasPresent || len(targetSettings) > 0 {
			desiredContract.Groups[input.Policy.TargetGroup] = targetSettings
		} else {
			delete(desiredContract.Groups, input.Policy.TargetGroup)
		}
		if input.ChannelDisabledByPolicy && !input.CurrentChannelEnabled {
			enableChannel = true
			plan.DesiredChannelEnabled = true
			additionDiffs = append(additionDiffs, MirrorFunctionalDiff{Phase: PhaseAdditionsExpose, Kind: DiffChannelState,
				Field: "channel_enabled", Before: "false", After: "true"})
		}
	} else {
		switch input.Policy.EmptyGroupAction {
		case "preserve":
		case "disable_managed_channel":
			if input.CurrentChannelEnabled {
				disableChannel = true
				plan.DesiredChannelEnabled = false
				if strings.TrimSpace(input.ChannelStatusAuthority) != "disable_only" {
					authorityGates = append(authorityGates, MirrorPlanGate{Code: "status_authority_required",
						Scope: input.Policy.TargetGroup, Actual: firstValue(input.ChannelStatusAuthority, "none"),
						Limit: "disable_only", Message: "managed-channel disable requires explicit policy status authority"})
				}
				removalDiffs = append(removalDiffs, MirrorFunctionalDiff{Phase: PhaseRemovalQuiesce, Kind: DiffChannelState,
					Field: "channel_enabled", Before: "true", After: "false"})
			}
		case "remove_managed_models":
			if input.Policy.SyncModelRemovals {
				removedMemberships, removedContracts, removalDiffs, contractChanged, err = applyOwnedRemovals(
					input.Policy.TargetGroup, map[string]struct{}{}, stringSet(currentModels),
					currentContract.Groups[input.Policy.TargetGroup], desiredContract, owned,
					excludedModelSet(input.Policy), contractChanged)
				if err != nil {
					return CompleteMirrorPlan{}, err
				}
			}
		}
	}

	plan.DesiredChannelModels = applyMembershipDelta(currentModels, removedMemberships, addedMemberships)
	plan.RemovedContractModels = append([]string(nil), removedContracts...)
	plan.DesiredContract = desiredContract
	plan.Diffs = append(plan.Diffs, removalDiffs...)
	plan.Diffs = append(plan.Diffs, contractDiffs...)
	plan.Diffs = append(plan.Diffs, additionDiffs...)
	if err := validateDiffOrder(plan.Diffs); err != nil {
		return CompleteMirrorPlan{}, err
	}

	currentRatios := map[string]string{}
	desiredRatios := map[string]string{}
	if strings.TrimSpace(plan.ExpectedGroupRatio) != "" {
		currentRatios[input.Policy.TargetGroup] = plan.ExpectedGroupRatio
	}
	if strings.TrimSpace(plan.DesiredGroupRatio) != "" {
		desiredRatios[input.Policy.TargetGroup] = plan.DesiredGroupRatio
	}
	plan.ExpectedFunctionalHash, err = TargetFunctionalHash(currentContract, currentRatios)
	if err != nil {
		return CompleteMirrorPlan{}, fmt.Errorf("current target functional hash: %w", err)
	}
	plan.DesiredFunctionalHash, err = TargetFunctionalHash(desiredContract, desiredRatios)
	if err != nil {
		return CompleteMirrorPlan{}, fmt.Errorf("desired target functional hash: %w", err)
	}
	plan.ContractEntries = countContractEntries(desiredContract)
	encoded, err := json.Marshal(desiredContract)
	if err != nil {
		return CompleteMirrorPlan{}, fmt.Errorf("serialize desired contract: %w", err)
	}
	plan.ContractBytes = len(encoded)

	changed := len(plan.Diffs) > 0
	if changed {
		removalGateModels := sortedStringUnion(removedMemberships, removedContracts)
		plan.ApprovalGates, err = disasterApprovalGates(input, limits, currentModels, removalGateModels,
			addedMemberships, currentContract, desiredContract)
		if err != nil {
			return CompleteMirrorPlan{}, err
		}
		plan.ApprovalGates = append(plan.ApprovalGates, authorityGates...)
		sortPlanGates(plan.ApprovalGates)
		plan.BlockingGates = hardContractGates(limits, plan.ContractEntries, plan.ContractBytes, len(plan.DesiredChannelModels))
		plan.Stages = buildPlannerStages(groupRatioChanged, contractChanged, removedMemberships, addedMemberships,
			disableChannel, enableChannel)
	}
	plan.NoChange = !changed
	plan.Blocked = len(plan.BlockingGates) > 0
	plan.ApprovalRequired = changed && !plan.Blocked && (input.Policy.ExecutionMode != "auto" || len(plan.ApprovalGates) > 0)
	switch {
	case !changed:
		plan.Decision = DecisionNoChange
	case plan.Blocked:
		plan.Decision = DecisionBlocked
	case !input.Policy.Enabled:
		plan.Decision = DecisionDisabled
	case plan.ApprovalRequired:
		plan.Decision = DecisionApprovalRequired
	default:
		plan.Decision = DecisionAutomatic
	}
	return plan, nil
}

func validatePlannerPolicy(policy Policy) error {
	if policy.Kind != PolicyKindCompleteMirror {
		return fmt.Errorf("complete-mirror planner does not accept policy kind %q", policy.Kind)
	}
	err := ValidatePolicy(policy, PolicyCapabilities{})
	if err != nil && !errors.Is(err, ErrCapabilityRequired) {
		return err
	}
	if (policy.SyncModelAdditions || policy.SyncModelRemovals) && policy.SourceModelPattern != "*" {
		return errors.New("complete model-set synchronization requires source_model_pattern '*'")
	}
	if policy.SyncModelAdditions && (!policy.SyncModelPrices || !policy.SyncBillingMode) {
		return errors.New("model additions require model-price and billing synchronization")
	}
	if policy.SyncBillingMode && !policy.SyncModelPrices {
		return errors.New("billing synchronization requires model-price synchronization")
	}
	return nil
}

func normalizeDisasterLimits(raw DisasterLimits) (DisasterLimits, error) {
	defaults := DefaultDisasterLimits()
	if raw.MaximumAdditions == 0 {
		raw.MaximumAdditions = defaults.MaximumAdditions
	}
	if raw.MaximumAdditionPercentage == "" {
		raw.MaximumAdditionPercentage = defaults.MaximumAdditionPercentage
	}
	if raw.MaximumRemovals == 0 {
		raw.MaximumRemovals = defaults.MaximumRemovals
	}
	if raw.MaximumRemovalPercentage == "" {
		raw.MaximumRemovalPercentage = defaults.MaximumRemovalPercentage
	}
	if raw.MaximumPriceIncrease == "" {
		raw.MaximumPriceIncrease = defaults.MaximumPriceIncrease
	}
	if raw.MaximumContractEntries == 0 || raw.MaximumContractEntries > hardMaximumContractEntries {
		raw.MaximumContractEntries = hardMaximumContractEntries
	}
	if raw.MaximumContractBytes == 0 || raw.MaximumContractBytes > hardMaximumContractBytes {
		raw.MaximumContractBytes = hardMaximumContractBytes
	}
	if raw.MaximumAdditions < 0 || raw.MaximumRemovals < 0 || raw.MaximumContractEntries < 1 || raw.MaximumContractBytes < 1 {
		return DisasterLimits{}, errors.New("disaster limits must be non-negative and contract caps must be positive")
	}
	for name, value := range map[string]string{
		"maximum addition percentage": raw.MaximumAdditionPercentage,
		"maximum removal percentage":  raw.MaximumRemovalPercentage,
		"maximum price increase":      raw.MaximumPriceIncrease,
	} {
		parsed, err := parseNonnegativeDecimal(value, name)
		if err != nil || (name == "maximum price increase" && !parsed.IsPositive()) {
			return DisasterLimits{}, fmt.Errorf("%s must be a non-negative decimal", name)
		}
	}
	return raw, nil
}

func normalizeModelNames(raw []string) ([]string, error) {
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("model identity cannot be empty")
		}
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeOwnedModels(raw []OwnedModelState) (map[string]OwnedModelState, error) {
	result := make(map[string]OwnedModelState, len(raw))
	for _, item := range raw {
		item.TargetModel = strings.TrimSpace(item.TargetModel)
		if item.TargetModel == "" || item.ForeignChannelReferences < 0 {
			return nil, errors.New("owned model identity and non-negative foreign reference count are required")
		}
		if _, duplicate := result[item.TargetModel]; duplicate {
			return nil, fmt.Errorf("duplicate ownership input for model %q", item.TargetModel)
		}
		result[item.TargetModel] = item
	}
	return result, nil
}

func cloneContract(contract domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	encoded, err := json.Marshal(contract)
	if err != nil {
		return domain.GroupModelPricingContract{}, fmt.Errorf("clone target contract: %w", err)
	}
	var result domain.GroupModelPricingContract
	if err := json.Unmarshal(encoded, &result); err != nil {
		return domain.GroupModelPricingContract{}, fmt.Errorf("clone target contract: %w", err)
	}
	return result, nil
}

func sourceMatchesPolicy(group CanonicalGroup, binding string) bool {
	binding = strings.TrimSpace(binding)
	return binding == group.SourceGroupID || binding == group.SourceGroupName
}

func prepareMirrorModels(group CanonicalGroup, policy Policy, factors CashFactors, currentGroupRatio string) ([]preparedMirrorModel, error) {
	models := make([]CanonicalModel, 0, len(group.Models))
	for _, model := range group.Models {
		matched, err := plannerModelMatches(model, policy)
		if err != nil {
			return nil, err
		}
		if matched {
			models = append(models, model)
		}
	}
	modelStep, err := parsePositiveDefault(policy.RoundingStep, defaultRoundingStep, "model rounding step")
	if err != nil {
		return nil, err
	}
	effectiveRatio, sourceCashFactor, targetFactor, risk, err := plannerCashFactors(group, factors)
	if err != nil {
		return nil, err
	}
	var effectiveGroup decimal.Decimal
	if policy.PriceAnchor == PriceAnchorEffectiveSale {
		effectiveGroup, err = plannerSimpleGroupRatio(effectiveRatio, policy, currentGroupRatio)
		if err != nil {
			return nil, err
		}
	}
	result := make([]preparedMirrorModel, 0, len(models))
	seenTarget := make(map[string]struct{}, len(models))
	for _, model := range models {
		target := strings.TrimSpace(model.UpstreamRequestName)
		if target == "" {
			return nil, fmt.Errorf("source model %q has no target request identity", model.SourceModelID)
		}
		if _, duplicate := seenTarget[target]; duplicate {
			return nil, fmt.Errorf("multiple source models map to target model %q", target)
		}
		seenTarget[target] = struct{}{}
		item := preparedMirrorModel{model: model, targetModel: target, source: map[string]decimal.Decimal{}, base: map[string]decimal.Decimal{}}
		for _, dimension := range model.PriceDimensions {
			normalized, normalizeErr := normalizePriceDimension(dimension)
			if normalizeErr != nil {
				return nil, fmt.Errorf("source model %q: %w", model.SourceModelID, normalizeErr)
			}
			identity := dimensionIdentity(normalized)
			if _, duplicate := item.source[identity]; duplicate {
				return nil, fmt.Errorf("source model %q contains duplicate dimension %s", model.SourceModelID, identity)
			}
			sourceValue, _ := decimal.NewFromString(normalized.DecimalValue)
			item.source[identity] = sourceValue
			if !policy.SyncModelPrices {
				continue
			}
			var base decimal.Decimal
			if policy.PriceAnchor == PriceAnchorSourceBase {
				base, err = applyTransform(sourceValue, policy.ModelPriceRule)
			} else {
				cost := sourceValue.Mul(effectiveRatio).Mul(sourceCashFactor).Mul(decimalOne().Add(risk))
				var desiredSale decimal.Decimal
				switch normalizeRule(policy.ModelPriceRule).Type {
				case "target_margin", "markup", "subsidy_rate":
					desiredSale, err = requiredEconomicRevenue(cost, policy.ModelPriceRule)
				default:
					desiredSale, err = applyTransform(cost, policy.ModelPriceRule)
				}
				if err == nil {
					base = desiredSale.Div(effectiveGroup.Mul(targetFactor))
				}
			}
			if err != nil {
				return nil, fmt.Errorf("source model %q dimension %s: %w", model.SourceModelID, identity, err)
			}
			base = ceilToStep(base, modelStep)
			item.base[identity] = base
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].targetModel < result[j].targetModel })
	return result, nil
}

func plannerModelMatches(model CanonicalModel, policy Policy) (bool, error) {
	if modelExcluded(policy, model.SourceModelID) || modelExcluded(policy, model.UpstreamRequestName) {
		return false, nil
	}
	if policy.ModelScopeMode == "mirror_group" {
		return true, nil
	}
	patterns := strings.Split(policy.SourceModelPattern, ",")
	for _, raw := range patterns {
		candidate := strings.ToLower(strings.TrimSpace(raw))
		if candidate == "*" {
			return true, nil
		}
		if candidate == "" {
			continue
		}
		matched, err := path.Match(candidate, strings.ToLower(model.SourceModelID))
		if err != nil {
			return false, fmt.Errorf("invalid source model pattern %q: %w", candidate, err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func modelExcluded(policy Policy, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, excluded := range policy.ExcludedModels {
		if model == excluded {
			return true
		}
	}
	return false
}

func excludedModelSet(policy Policy) map[string]struct{} {
	result := make(map[string]struct{}, len(policy.ExcludedModels))
	for _, model := range policy.ExcludedModels {
		result[strings.TrimSpace(model)] = struct{}{}
	}
	return result
}

func plannerCashFactors(group CanonicalGroup, factors CashFactors) (effectiveRatio, sourceCashFactor,
	targetFactor, risk decimal.Decimal, err error) {
	raw, err := parseNonnegativeDecimal(group.RawGroupRatio, "raw group ratio")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	recharge, err := parsePositiveDefault(group.RechargeValueFactor, "1", "recharge value factor")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	sourceCashFactor, err = parsePositiveDefault(factors.SourceCashFactor, "1", "source cash factor")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	targetFactor, err = parsePositiveDefault(factors.TargetRealizationFactor, "1", "target realization factor")
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, err
	}
	risk, err = parseDefaultDecimal(factors.RiskBufferRate, "0", "risk buffer rate")
	if err != nil || risk.LessThan(decimalMinusOne()) {
		return decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, errors.New("risk buffer rate must be at least -1")
	}
	return raw.Mul(recharge), sourceCashFactor, targetFactor, risk, nil
}

func desiredPlannerGroupRatio(group CanonicalGroup, prepared []preparedMirrorModel,
	input CompleteMirrorPlannerInput) (string, error) {
	if !input.Policy.SyncGroupRatio {
		return canonicalPositiveDecimal(input.CurrentGroupRatio, "current group ratio")
	}
	effective, sourceCashFactor, targetFactor, risk, err := plannerCashFactors(group, input.Factors)
	if err != nil {
		return "", err
	}
	rule := normalizeRule(input.Policy.GroupRatioRule)
	step, err := parsePositiveDefault(input.Policy.RoundingStep, defaultRoundingStep, "group rounding step")
	if err != nil {
		return "", err
	}
	var desired decimal.Decimal
	if rule.Type == "target_margin" || rule.Type == "markup" || rule.Type == "subsidy_rate" {
		if input.Policy.PriceAnchor != PriceAnchorSourceBase || !input.Policy.SyncModelPrices {
			return "", errors.New("economic group-ratio rules require synchronized source-base model prices")
		}
		for _, item := range prepared {
			for identity, source := range item.source {
				base, ok := item.base[identity]
				if !ok || !base.IsPositive() {
					return "", fmt.Errorf("model %q dimension %s cannot satisfy the economic group rule", item.targetModel, identity)
				}
				cost := source.Mul(effective).Mul(sourceCashFactor).Mul(decimalOne().Add(risk))
				revenue, revenueErr := requiredEconomicRevenue(cost, rule)
				if revenueErr != nil {
					return "", revenueErr
				}
				required := revenue.Div(base.Mul(targetFactor))
				if required.GreaterThan(desired) {
					desired = required
				}
			}
		}
	} else {
		desired, err = applyTransform(effective, rule)
		if err != nil {
			return "", fmt.Errorf("group ratio: %w", err)
		}
	}
	desired = ceilToStep(desired, step)
	desired, err = constrainGroupRatio(desired, SourceBaseInput{
		CurrentGroupRatio: input.CurrentGroupRatio, MinimumGroupRatio: input.Policy.MinimumRatio,
		MaximumGroupRatio: input.Policy.MaximumRatio, OnlyFollowIncrease: input.Policy.OnlyFollowIncrease,
		OnlyFollowDecrease: input.Policy.OnlyFollowDecrease,
	}, step)
	if err != nil {
		return "", err
	}
	if !desired.IsPositive() {
		return "", errors.New("target group ratio must be positive")
	}
	return canonicalDecimalValue(desired), nil
}

func plannerSimpleGroupRatio(effective decimal.Decimal, policy Policy, current string) (decimal.Decimal, error) {
	if !policy.SyncGroupRatio {
		return parsePositiveDecimal(current, "current group ratio")
	}
	rule := normalizeRule(policy.GroupRatioRule)
	if rule.Type == "target_margin" || rule.Type == "markup" || rule.Type == "subsidy_rate" {
		return decimal.Zero, errors.New("effective_sale uses its model-price rule for economic authority; group ratio must use a direct transform")
	}
	value, err := applyTransform(effective, rule)
	if err != nil {
		return decimal.Zero, err
	}
	step, err := parsePositiveDefault(policy.RoundingStep, defaultRoundingStep, "group rounding step")
	if err != nil {
		return decimal.Zero, err
	}
	value, err = constrainGroupRatio(ceilToStep(value, step), SourceBaseInput{
		CurrentGroupRatio: current, MinimumGroupRatio: policy.MinimumRatio, MaximumGroupRatio: policy.MaximumRatio,
		OnlyFollowIncrease: policy.OnlyFollowIncrease, OnlyFollowDecrease: policy.OnlyFollowDecrease,
	}, step)
	if err != nil || !value.IsPositive() {
		return decimal.Zero, errors.New("target group ratio must be positive")
	}
	return value, nil
}

func buildPlannerSetting(item preparedMirrorModel, policy Policy, targetGroupRatio string,
	factors CashFactors) (domain.GroupModelPricingSetting, error) {
	prices := item.base
	if !policy.SyncModelPrices {
		prices = item.source
	}
	setting, err := plannerSettingFromDimensions(item.model, prices)
	if err != nil {
		return domain.GroupModelPricingSetting{}, err
	}
	if policy.SyncModelPrices {
		targetGroup, err := parsePositiveDecimal(targetGroupRatio, "target group ratio")
		if err != nil {
			return domain.GroupModelPricingSetting{}, err
		}
		targetFactor, err := parsePositiveDefault(factors.TargetRealizationFactor, "1", "target realization factor")
		if err != nil {
			return domain.GroupModelPricingSetting{}, err
		}
		for identity, value := range prices {
			if value.Mul(targetGroup).Mul(targetFactor).IsNegative() {
				return domain.GroupModelPricingSetting{}, fmt.Errorf("dimension %s has a negative effective sale", identity)
			}
		}
	}
	return setting, nil
}

func plannerSettingFromDimensions(model CanonicalModel, prices map[string]decimal.Decimal) (domain.GroupModelPricingSetting, error) {
	byKind := make(map[string]decimal.Decimal, len(prices))
	var taskDimension *PriceDimension
	for _, raw := range model.PriceDimensions {
		dimension, err := normalizePriceDimension(raw)
		if err != nil {
			return domain.GroupModelPricingSetting{}, err
		}
		value, ok := prices[dimensionIdentity(dimension)]
		if !ok {
			return domain.GroupModelPricingSetting{}, fmt.Errorf("missing transformed dimension %s", dimensionIdentity(dimension))
		}
		if _, duplicate := byKind[dimension.Kind]; duplicate {
			return domain.GroupModelPricingSetting{}, fmt.Errorf("duplicate target price kind %s", dimension.Kind)
		}
		byKind[dimension.Kind] = value
		if !strings.HasSuffix(dimension.Kind, "_per_1m") {
			copy := dimension
			taskDimension = &copy
		}
	}
	if model.BillingMode == "fixed_price" {
		if len(byKind) != 1 || taskDimension == nil {
			return domain.GroupModelPricingSetting{}, errors.New("fixed-price model requires one task dimension")
		}
		value := canonicalDecimalValue(byKind[taskDimension.Kind])
		unit := strings.TrimPrefix(taskDimension.Unit, "currency_")
		if taskDimension.Kind == "fixed_duration" {
			unit = "fixed_duration"
		}
		return domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: &value,
			TaskProfile: &domain.GroupModelTaskProfile{Unit: unit, FixedDurationSeconds: taskDimension.DurationSeconds}}, nil
	}
	if model.BillingMode != "token_vector" {
		return domain.GroupModelPricingSetting{}, fmt.Errorf("unsupported canonical billing mode %q", model.BillingMode)
	}
	if taskDimension != nil {
		return domain.GroupModelPricingSetting{}, errors.New("token-vector model cannot contain task price dimensions")
	}
	input, inputOK := byKind["input_per_1m"]
	output, outputOK := byKind["output_per_1m"]
	if !inputOK || !outputOK {
		return domain.GroupModelPricingSetting{}, errors.New("token pricing requires input_per_1m and output_per_1m")
	}
	if input.IsZero() {
		for kind, value := range byKind {
			if kind != "input_per_1m" && !value.IsZero() {
				return domain.GroupModelPricingSetting{}, fmt.Errorf("non-zero %s cannot be represented with zero token input price", kind)
			}
		}
	}
	modelRatio := canonicalDecimalValue(input.Div(decimal.NewFromInt(2)))
	completion := "1"
	if !input.IsZero() {
		completion = canonicalDecimalValue(output.Div(input))
	}
	setting := domain.GroupModelPricingSetting{BillingMode: "ratio", ModelRatio: &modelRatio, CompletionRatio: &completion}
	setRatio := func(kind string, destination **string) {
		if value, ok := byKind[kind]; ok {
			ratio := decimal.Zero
			if !input.IsZero() {
				ratio = value.Div(input)
			}
			canonical := canonicalDecimalValue(ratio)
			*destination = &canonical
		}
	}
	setRatio("cache_read_per_1m", &setting.CacheRatio)
	setRatio("cache_write_per_1m", &setting.CacheCreationRatio)
	setRatio("cache_write_5m_per_1m", &setting.CacheCreation5mRatio)
	setRatio("cache_write_1h_per_1m", &setting.CacheCreation1hRatio)
	setRatio("image_input_per_1m", &setting.ImageRatio)
	setRatio("audio_input_per_1m", &setting.AudioRatio)
	setRatio("audio_output_per_1m", &setting.AudioCompletionRatio)
	return setting, nil
}

func mergePlannerSetting(current domain.GroupModelPricingSetting, exists bool, generated domain.GroupModelPricingSetting,
	policy Policy) (domain.GroupModelPricingSetting, error) {
	if !exists {
		if !policy.SyncModelPrices || !policy.SyncBillingMode {
			return domain.GroupModelPricingSetting{}, errors.New("a new contract item requires synchronized price and billing fields")
		}
		return generated, nil
	}
	result := cloneSetting(current)
	if policy.SyncModelPrices {
		result.ModelPrice = cloneStringPointer(generated.ModelPrice)
		result.ModelRatio = cloneStringPointer(generated.ModelRatio)
		result.CompletionRatio = cloneStringPointer(generated.CompletionRatio)
		result.CacheRatio = cloneStringPointer(generated.CacheRatio)
		result.CacheCreationRatio = cloneStringPointer(generated.CacheCreationRatio)
		result.CacheCreation5mRatio = cloneStringPointer(generated.CacheCreation5mRatio)
		result.CacheCreation1hRatio = cloneStringPointer(generated.CacheCreation1hRatio)
		result.ImageRatio = cloneStringPointer(generated.ImageRatio)
		result.AudioRatio = cloneStringPointer(generated.AudioRatio)
		result.AudioCompletionRatio = cloneStringPointer(generated.AudioCompletionRatio)
	}
	if policy.SyncBillingMode {
		result.BillingMode = generated.BillingMode
		result.BillingExpr = generated.BillingExpr
		result.TaskProfile = cloneTaskProfile(generated.TaskProfile)
	}
	return result, nil
}

func cloneSetting(setting domain.GroupModelPricingSetting) domain.GroupModelPricingSetting {
	setting.ModelPrice = cloneStringPointer(setting.ModelPrice)
	setting.ModelRatio = cloneStringPointer(setting.ModelRatio)
	setting.CompletionRatio = cloneStringPointer(setting.CompletionRatio)
	setting.CacheRatio = cloneStringPointer(setting.CacheRatio)
	setting.CacheCreationRatio = cloneStringPointer(setting.CacheCreationRatio)
	setting.CacheCreation5mRatio = cloneStringPointer(setting.CacheCreation5mRatio)
	setting.CacheCreation1hRatio = cloneStringPointer(setting.CacheCreation1hRatio)
	setting.ImageRatio = cloneStringPointer(setting.ImageRatio)
	setting.AudioRatio = cloneStringPointer(setting.AudioRatio)
	setting.AudioCompletionRatio = cloneStringPointer(setting.AudioCompletionRatio)
	setting.TaskProfile = cloneTaskProfile(setting.TaskProfile)
	return setting
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func targetSettingsFunctionallyEqual(group, model string, left, right domain.GroupModelPricingSetting) (bool, error) {
	leftHash, err := TargetModelFunctionalHash(group, model, left)
	if err != nil {
		return false, err
	}
	rightHash, err := TargetModelFunctionalHash(group, model, right)
	if err != nil {
		return false, err
	}
	return leftHash == rightHash, nil
}

func diffPlannerSettings(group, model string, before domain.GroupModelPricingSetting, beforeExists bool,
	after domain.GroupModelPricingSetting, afterExists bool) ([]MirrorFunctionalDiff, error) {
	beforeFields, err := plannerSettingFields(before, beforeExists)
	if err != nil {
		return nil, fmt.Errorf("target %s/%s before: %w", group, model, err)
	}
	afterFields, err := plannerSettingFields(after, afterExists)
	if err != nil {
		return nil, fmt.Errorf("target %s/%s after: %w", group, model, err)
	}
	keys := make(map[string]struct{}, len(beforeFields)+len(afterFields))
	for key := range beforeFields {
		keys[key] = struct{}{}
	}
	for key := range afterFields {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	result := make([]MirrorFunctionalDiff, 0)
	for _, field := range ordered {
		if beforeFields[field] == afterFields[field] {
			continue
		}
		kind := DiffModelPrice
		if field == "billing_mode" || field == "billing_expr" || field == "billing_unit" || field == "fixed_duration_seconds" {
			kind = DiffBilling
		}
		result = append(result, MirrorFunctionalDiff{Phase: PhaseContractCAS, Kind: kind, Model: model,
			Field: field, Before: beforeFields[field], After: afterFields[field]})
	}
	return result, nil
}

func plannerSettingFields(setting domain.GroupModelPricingSetting, exists bool) (map[string]string, error) {
	if !exists {
		return map[string]string{}, nil
	}
	result, err := canonicalSettingDimensions(setting)
	if err != nil {
		return nil, err
	}
	result["billing_mode"] = strings.TrimSpace(setting.BillingMode)
	if expr := strings.TrimSpace(setting.BillingExpr); expr != "" {
		result["billing_expr"] = expr
	}
	if setting.TaskProfile != nil {
		if err := validateTaskProfile(setting.TaskProfile); err != nil {
			return nil, err
		}
		result["billing_unit"] = strings.TrimSpace(setting.TaskProfile.Unit)
		if setting.TaskProfile.FixedDurationSeconds != 0 {
			result["fixed_duration_seconds"] = strconv.Itoa(setting.TaskProfile.FixedDurationSeconds)
		}
	}
	return result, nil
}

func applyOwnedRemovals(targetGroup string, sourceSet, currentSet map[string]struct{},
	currentSettings map[string]domain.GroupModelPricingSetting, desiredContract domain.GroupModelPricingContract,
	owned map[string]OwnedModelState, excluded map[string]struct{}, alreadyChanged bool) (memberships, contracts []string,
	diffs []MirrorFunctionalDiff, contractChanged bool, err error) {
	contractChanged = alreadyChanged
	membershipDiffs := make([]MirrorFunctionalDiff, 0)
	contractDiffs := make([]MirrorFunctionalDiff, 0)
	models := make([]string, 0, len(owned))
	for model := range owned {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if _, skip := excluded[model]; skip {
			continue
		}
		if _, retained := sourceSet[model]; retained {
			continue
		}
		item := owned[model]
		if item.MembershipOwned {
			if _, present := currentSet[model]; present {
				memberships = append(memberships, model)
				membershipDiffs = append(membershipDiffs, MirrorFunctionalDiff{Phase: PhaseRemovalQuiesce, Kind: DiffRemoval,
					Model: model, Field: "channel_membership", Before: "present", After: "absent"})
			}
		}
		before, exists := currentSettings[model]
		if item.ContractOwned && item.ForeignChannelReferences == 0 && exists {
			settingDiffs, diffErr := diffPlannerSettings(targetGroup, model, before, true, domain.GroupModelPricingSetting{}, false)
			if diffErr != nil {
				return nil, nil, nil, alreadyChanged, diffErr
			}
			contracts = append(contracts, model)
			contractDiffs = append(contractDiffs, settingDiffs...)
			delete(desiredContract.Groups[targetGroup], model)
			contractChanged = true
		}
	}
	if len(contracts) > 0 && len(desiredContract.Groups[targetGroup]) == 0 {
		delete(desiredContract.Groups, targetGroup)
	}
	diffs = append(membershipDiffs, contractDiffs...)
	return memberships, contracts, diffs, contractChanged, nil
}

func applyMembershipDelta(current, removals, additions []string) []string {
	result := stringSet(current)
	for _, model := range removals {
		delete(result, model)
	}
	for _, model := range additions {
		result[model] = struct{}{}
	}
	ordered := make([]string, 0, len(result))
	for model := range result {
		ordered = append(ordered, model)
	}
	sort.Strings(ordered)
	return ordered
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedStringUnion(groups ...[]string) []string {
	set := make(map[string]struct{})
	for _, group := range groups {
		for _, value := range group {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func canonicalOptionalPositive(raw, name string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return canonicalPositiveDecimal(raw, name)
}

func countContractEntries(contract domain.GroupModelPricingContract) int {
	count := 0
	for _, models := range contract.Groups {
		count += len(models)
	}
	return count
}

func disasterApprovalGates(input CompleteMirrorPlannerInput, limits DisasterLimits, currentModels,
	removals, additions []string, before, after domain.GroupModelPricingContract) ([]MirrorPlanGate, error) {
	gates := make([]MirrorPlanGate, 0)
	if !input.InitialApprovedListing {
		if len(additions) > limits.MaximumAdditions {
			gates = append(gates, MirrorPlanGate{Code: "addition_count_exceeded", Scope: input.Policy.TargetGroup,
				Actual: strconv.Itoa(len(additions)), Limit: strconv.Itoa(limits.MaximumAdditions),
				Message: "model additions exceed the continuing-automation absolute limit"})
		}
		currentSet := stringSet(currentModels)
		ownedLive := 0
		for _, item := range input.OwnedModels {
			if item.MembershipOwned {
				if _, present := currentSet[strings.TrimSpace(item.TargetModel)]; present {
					ownedLive++
				}
			}
		}
		additionBaseline := ownedLive
		if additionBaseline == 0 {
			additionBaseline = len(currentModels)
		}
		additionRate := changeRate(len(additions), additionBaseline)
		additionLimit, _ := decimal.NewFromString(limits.MaximumAdditionPercentage)
		if additionRate.GreaterThan(additionLimit) {
			gates = append(gates, MirrorPlanGate{Code: "addition_percentage_exceeded", Scope: input.Policy.TargetGroup,
				Actual: canonicalDecimalValue(additionRate), Limit: canonicalDecimalValue(additionLimit),
				Message: "model additions exceed the continuing-automation percentage limit"})
		}
		confirmedEmptySource := input.SourceState == SourceGroupEmpty || input.SourceState == SourceGroupMissing
		if !confirmedEmptySource && len(removals) > limits.MaximumRemovals {
			gates = append(gates, MirrorPlanGate{Code: "removal_count_exceeded", Scope: input.Policy.TargetGroup,
				Actual: strconv.Itoa(len(removals)), Limit: strconv.Itoa(limits.MaximumRemovals),
				Message: "owned model removals exceed the continuing-automation absolute limit"})
		}
		removalRate := changeRate(len(removals), ownedLive)
		removalLimit, _ := decimal.NewFromString(limits.MaximumRemovalPercentage)
		if !confirmedEmptySource && removalRate.GreaterThan(removalLimit) {
			gates = append(gates, MirrorPlanGate{Code: "removal_percentage_exceeded", Scope: input.Policy.TargetGroup,
				Actual: canonicalDecimalValue(removalRate), Limit: canonicalDecimalValue(removalLimit),
				Message: "owned model removals exceed the continuing-automation percentage limit"})
		}
	}
	priceGates, err := priceIncreaseGates(input.Policy.TargetGroup, limits.MaximumPriceIncrease, before, after)
	if err != nil {
		return nil, err
	}
	gates = append(gates, priceGates...)
	sort.SliceStable(gates, func(i, j int) bool {
		if gates[i].Code != gates[j].Code {
			return gates[i].Code < gates[j].Code
		}
		return gates[i].Scope < gates[j].Scope
	})
	return gates, nil
}

func changeRate(delta, baseline int) decimal.Decimal {
	if delta == 0 {
		return decimal.Zero
	}
	if baseline <= 0 {
		return decimalOne()
	}
	return decimal.NewFromInt(int64(delta)).Div(decimal.NewFromInt(int64(baseline)))
}

func priceIncreaseGates(group, rawLimit string, before, after domain.GroupModelPricingContract) ([]MirrorPlanGate, error) {
	limit, _ := decimal.NewFromString(rawLimit)
	models := make([]string, 0)
	for model := range after.Groups[group] {
		if _, existed := before.Groups[group][model]; existed {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	result := make([]MirrorPlanGate, 0)
	for _, model := range models {
		equal, err := targetSettingsFunctionallyEqual(group, model, before.Groups[group][model], after.Groups[group][model])
		if err != nil {
			return nil, fmt.Errorf("target %s/%s price gate: %w", group, model, err)
		}
		if equal {
			continue
		}
		oldDimensions, err := plannerEffectiveDimensions(before.Groups[group][model])
		if err != nil {
			return nil, fmt.Errorf("target %s/%s before price gate: %w", group, model, err)
		}
		newDimensions, err := plannerEffectiveDimensions(after.Groups[group][model])
		if err != nil {
			return nil, fmt.Errorf("target %s/%s after price gate: %w", group, model, err)
		}
		keys := make([]string, 0, len(newDimensions))
		for key := range newDimensions {
			if _, comparable := oldDimensions[key]; comparable {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			oldValue, newValue := oldDimensions[key], newDimensions[key]
			if !newValue.GreaterThan(oldValue) {
				continue
			}
			if oldValue.IsZero() {
				result = append(result, MirrorPlanGate{Code: "price_increase_exceeded", Scope: model + "/" + key,
					Actual: "infinite", Limit: canonicalDecimalValue(limit),
					Message: "a comparable effective model-price dimension rises from zero and requires approval"})
				continue
			}
			multiple := newValue.Div(oldValue)
			if multiple.GreaterThan(limit) {
				result = append(result, MirrorPlanGate{Code: "price_increase_exceeded", Scope: model + "/" + key,
					Actual: canonicalDecimalValue(multiple), Limit: canonicalDecimalValue(limit),
					Message: "a comparable effective model-price dimension exceeds the automatic increase multiple"})
			}
		}
	}
	return result, nil
}

func plannerEffectiveDimensions(setting domain.GroupModelPricingSetting) (map[string]decimal.Decimal, error) {
	mode := strings.TrimSpace(setting.BillingMode)
	if mode == "fixed_price" {
		if setting.ModelPrice == nil || setting.TaskProfile == nil {
			return nil, errors.New("fixed-price setting is missing model_price or task_profile")
		}
		if err := validateTaskProfile(setting.TaskProfile); err != nil {
			return nil, err
		}
		price, err := parseNonnegativeDecimal(*setting.ModelPrice, "model price")
		if err != nil {
			return nil, err
		}
		unit := strings.TrimSpace(setting.TaskProfile.Unit)
		kind := unit
		dimensionUnit := "currency_" + unit
		if unit == "fixed_duration" {
			dimensionUnit = "currency_per_fixed_duration"
		}
		return map[string]decimal.Decimal{
			fmt.Sprintf("%s|%s|%d", kind, dimensionUnit, setting.TaskProfile.FixedDurationSeconds): price,
		}, nil
	}
	if mode != "ratio" || setting.ModelRatio == nil || setting.CompletionRatio == nil {
		return nil, errors.New("ratio setting is missing model_ratio or completion_ratio")
	}
	modelRatio, err := parseNonnegativeDecimal(*setting.ModelRatio, "model ratio")
	if err != nil {
		return nil, err
	}
	completion, err := parseNonnegativeDecimal(*setting.CompletionRatio, "completion ratio")
	if err != nil {
		return nil, err
	}
	input := modelRatio.Mul(decimal.NewFromInt(2))
	result := map[string]decimal.Decimal{
		"input_per_1m|currency_per_1m_tokens|0":  input,
		"output_per_1m|currency_per_1m_tokens|0": input.Mul(completion),
	}
	optional := []struct {
		kind  string
		value *string
	}{
		{"cache_read_per_1m", setting.CacheRatio}, {"cache_write_per_1m", setting.CacheCreationRatio},
		{"cache_write_5m_per_1m", setting.CacheCreation5mRatio}, {"cache_write_1h_per_1m", setting.CacheCreation1hRatio},
		{"image_input_per_1m", setting.ImageRatio}, {"audio_input_per_1m", setting.AudioRatio},
		{"audio_output_per_1m", setting.AudioCompletionRatio},
	}
	for _, item := range optional {
		if item.value == nil {
			continue
		}
		ratio, parseErr := parseNonnegativeDecimal(*item.value, item.kind)
		if parseErr != nil {
			return nil, parseErr
		}
		result[item.kind+"|currency_per_1m_tokens|0"] = input.Mul(ratio)
	}
	return result, nil
}

func hardContractGates(limits DisasterLimits, entries, bytes, channelModels int) []MirrorPlanGate {
	result := make([]MirrorPlanGate, 0, 3)
	if entries > limits.MaximumContractEntries {
		result = append(result, MirrorPlanGate{Code: "contract_entry_hard_cap_exceeded", Actual: strconv.Itoa(entries),
			Limit: strconv.Itoa(limits.MaximumContractEntries), Message: "desired contract exceeds the target or hard entry cap"})
	}
	if channelModels > limits.MaximumContractEntries {
		result = append(result, MirrorPlanGate{Code: "channel_model_hard_cap_exceeded", Actual: strconv.Itoa(channelModels),
			Limit: strconv.Itoa(limits.MaximumContractEntries), Message: "desired managed channel exceeds the target or hard model cap"})
	}
	if bytes > limits.MaximumContractBytes {
		result = append(result, MirrorPlanGate{Code: "contract_size_hard_cap_exceeded", Actual: strconv.Itoa(bytes),
			Limit: strconv.Itoa(limits.MaximumContractBytes), Message: "desired serialized contract exceeds the target or 8 MiB hard cap"})
	}
	return result
}

func sortPlanGates(gates []MirrorPlanGate) {
	sort.SliceStable(gates, func(i, j int) bool {
		if gates[i].Code != gates[j].Code {
			return gates[i].Code < gates[j].Code
		}
		return gates[i].Scope < gates[j].Scope
	})
}

func buildPlannerStages(groupRatioChanged, contractChanged bool, removals, additions []string,
	disableChannel, enableChannel bool) []MirrorPlanStage {
	result := make([]MirrorPlanStage, 0, 4)
	if len(removals) > 0 || disableChannel {
		stage := MirrorPlanStage{Phase: PhaseRemovalQuiesce}
		if len(removals) > 0 {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "remove_managed_models", Models: append([]string(nil), removals...)})
		}
		if disableChannel {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "disable_managed_channel"})
		}
		result = append(result, stage)
	}
	if groupRatioChanged || contractChanged {
		stage := MirrorPlanStage{Phase: PhaseContractCAS}
		if contractChanged {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "cas_group_model_contract"})
		}
		if groupRatioChanged {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "cas_group_ratio"})
		}
		result = append(result, stage)
	}
	if len(additions) > 0 || enableChannel {
		stage := MirrorPlanStage{Phase: PhaseAdditionsExpose}
		if len(additions) > 0 {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "add_managed_models", Models: append([]string(nil), additions...)})
		}
		if enableChannel {
			stage.Operations = append(stage.Operations, MirrorPlanOperation{Kind: "enable_policy_disabled_channel"})
		}
		result = append(result, stage)
	}
	result = append(result, MirrorPlanStage{Phase: PhaseReadback, Operations: []MirrorPlanOperation{
		{Kind: "verify_channel_membership_and_status"}, {Kind: "verify_group_model_contract"},
		{Kind: "verify_marketplace_ability_and_cache"},
	}})
	return result
}

func validateDiffOrder(diffs []MirrorFunctionalDiff) error {
	order := map[MirrorPlanPhase]int{PhaseRemovalQuiesce: 0, PhaseContractCAS: 1, PhaseAdditionsExpose: 2}
	last := -1
	for _, diff := range diffs {
		current, ok := order[diff.Phase]
		if !ok || current < last {
			return errors.New("mirror functional diffs are not in safe phase order")
		}
		last = current
	}
	return nil
}
