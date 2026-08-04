package mirrorexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func (e *Executor) requestIdentity(input ApplyInput) (string, string, error) {
	if err := validateApplyInput(input); err != nil {
		return "", "", err
	}
	proof, err := compactJSON(input.SourceProof)
	if err != nil {
		return "", "", fmt.Errorf("source proof: %w", err)
	}
	document := struct {
		PolicyUUID      string                    `json:"policy_uuid"`
		PolicyVersion   int64                     `json:"policy_version"`
		TargetSiteID    int64                     `json:"target_site_id"`
		ChannelID       int64                     `json:"channel_id"`
		SourceIdentity  string                    `json:"source_identity"`
		SourceProof     json.RawMessage           `json:"source_proof"`
		Plan            mirror.CompleteMirrorPlan `json:"plan"`
		ApprovalGranted bool                      `json:"approval_granted"`
	}{
		PolicyUUID: input.Policy.PolicyUUID, PolicyVersion: input.Policy.PolicyVersion,
		TargetSiteID: input.Target.ID, ChannelID: input.ChannelID,
		SourceIdentity: strings.TrimSpace(input.SourceIdentity), SourceProof: proof,
		Plan: input.Plan, ApprovalGranted: input.ApprovalGranted,
	}
	requestHash, err := hashJSON(document)
	if err != nil {
		return "", "", err
	}
	operationKey := strings.TrimSpace(input.OperationKey)
	if operationKey == "" {
		operationKey = "mirror:" + input.Policy.PolicyUUID + ":" + requestHash[:32]
	}
	if len(operationKey) > 256 {
		return "", "", errors.New("mirror operation key exceeds 256 bytes")
	}
	return operationKey, requestHash, nil
}

func validateApplyInput(input ApplyInput) error {
	if input.Target.ID <= 0 || input.ChannelID <= 0 || strings.TrimSpace(input.SourceIdentity) == "" {
		return fmt.Errorf("%w: target, channel and source identity are required", ErrPlanNotExecutable)
	}
	if input.Policy.PolicyUUID == "" || input.Policy.PolicyUUID != input.Plan.PolicyUUID ||
		input.Policy.PolicyVersion != input.Plan.PolicyVersion || input.Policy.TargetSiteID != input.Target.ID {
		return fmt.Errorf("%w: policy, plan and target identities differ", ErrPlanNotExecutable)
	}
	if !input.Policy.Enabled || input.Plan.NoChange || input.Plan.Blocked || input.Plan.Decision == mirror.DecisionBlocked ||
		input.Plan.Decision == mirror.DecisionDisabled || len(input.Plan.Stages) == 0 {
		return fmt.Errorf("%w: plan is disabled, blocked or a no-op", ErrPlanNotExecutable)
	}
	if input.Plan.Decision == mirror.DecisionApprovalRequired && !input.ApprovalGranted {
		return fmt.Errorf("%w: plan requires recorded administrator approval", ErrPlanNotExecutable)
	}
	if input.Plan.Decision != mirror.DecisionAutomatic && input.Plan.Decision != mirror.DecisionApprovalRequired {
		return fmt.Errorf("%w: unsupported decision %q", ErrPlanNotExecutable, input.Plan.Decision)
	}
	if len(input.SourceProof) == 0 || !json.Valid(input.SourceProof) || string(input.SourceProof) == "null" {
		return fmt.Errorf("%w: durable source proof is required", ErrPlanNotExecutable)
	}
	return validateStageOrder(input.Plan.Stages)
}

func validateStageOrder(stages []mirror.MirrorPlanStage) error {
	order := map[mirror.MirrorPlanPhase]int{
		mirror.PhaseRemovalQuiesce:  0,
		mirror.PhaseContractCAS:     1,
		mirror.PhaseAdditionsExpose: 2,
		mirror.PhaseReadback:        3,
	}
	last := -1
	readback := false
	for _, stage := range stages {
		current, exists := order[stage.Phase]
		if !exists || current < last {
			return fmt.Errorf("%w: planner stages are not removal-first", ErrPlanNotExecutable)
		}
		last = current
		if stage.Phase == mirror.PhaseReadback {
			readback = true
		}
	}
	if !readback {
		return fmt.Errorf("%w: final readback stage is missing", ErrPlanNotExecutable)
	}
	return nil
}

func (e *Executor) materialize(ctx context.Context, input ApplyInput, operationKey, requestHash string) (materializedChange, error) {
	channel, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
	if err != nil {
		return materializedChange{}, fmt.Errorf("read managed channel preflight: %w", err)
	}
	expectedModels := normalizeModels(input.Plan.ExpectedChannelModels)
	if !equalStrings(channel.Models, expectedModels) || channelEnabled(channel.Status) != input.Plan.ExpectedChannelEnabled {
		return materializedChange{}, errors.New("managed channel changed since the mirror preview")
	}

	operations, err := parsePlanOperations(input.Plan)
	if err != nil {
		return materializedChange{}, err
	}
	contract, desiredContract, err := e.materializeContract(ctx, input, operations.contract)
	if err != nil {
		return materializedChange{}, err
	}
	ratioBefore, ratioAfter, err := e.materializeGroupRatio(ctx, input, operations.groupRatio)
	if err != nil {
		return materializedChange{}, err
	}

	expectedFunctional, err := mirror.TargetFunctionalHash(contract,
		map[string]string{input.Policy.TargetGroup: ratioBefore.Values[input.Policy.TargetGroup]})
	if err != nil || expectedFunctional != input.Plan.ExpectedFunctionalHash {
		return materializedChange{}, errors.New("target functional state changed since the mirror preview")
	}
	desiredFunctional, err := mirror.TargetFunctionalHash(desiredContract,
		map[string]string{input.Policy.TargetGroup: ratioAfter.Values[input.Policy.TargetGroup]})
	if err != nil || desiredFunctional != input.Plan.DesiredFunctionalHash {
		return materializedChange{}, errors.New("materialized target state differs from the frozen mirror plan")
	}

	beforeChannel := channelState{ChannelID: channel.ChannelID, Models: normalizeModels(channel.Models),
		Status: channel.Status, CASHash: channel.Hash}
	removedChannel := beforeChannel
	removedChannel.Models = removeModels(removedChannel.Models, operations.removals)
	if operations.disable {
		removedChannel.Status = managedChannelDisabled
	}
	removedChannel.CASHash = ""
	finalChannel := channelState{ChannelID: channel.ChannelID, Models: normalizeModels(input.Plan.DesiredChannelModels),
		Status: managedChannelDisabled}
	if input.Plan.DesiredChannelEnabled {
		finalChannel.Status = managedChannelEnabled
	}
	if !equalStrings(applyMembershipDeltaForMaterialize(removedChannel.Models, operations.additions), finalChannel.Models) {
		return materializedChange{}, errors.New("planner membership stages do not produce the desired model set")
	}
	if !operations.enable && removedChannel.Status != finalChannel.Status {
		return materializedChange{}, errors.New("planner channel status stages do not produce the desired state")
	}

	expectedOwned, err := e.readAndVerifyOwnership(ctx, input, channel, contract)
	if err != nil {
		return materializedChange{}, err
	}
	beforeOwnedJSON, err := marshalJSON(expectedOwned)
	if err != nil {
		return materializedChange{}, err
	}
	desiredOwnedHint, err := marshalJSON(struct {
		ChannelSemanticHash string                           `json:"channel_semantic_hash"`
		Contract            domain.GroupModelPricingContract `json:"contract"`
		Models              []string                         `json:"models"`
	}{mustChannelSemanticHash(finalChannel), desiredContract, finalChannel.Models})
	if err != nil {
		return materializedChange{}, err
	}

	steps := make([]store.MirrorChangeStep, 0, 5)
	if beforeHash, afterHash := mustChannelSemanticHash(beforeChannel), mustChannelSemanticHash(removedChannel); beforeHash != afterHash {
		steps = append(steps, newMirrorStep(operationKey, "removal-channel", mirror.PhaseRemovalQuiesce,
			"managed_channel_state", strconv.FormatInt(input.ChannelID, 10), "quiesce_removed_models",
			beforeHash, afterHash, beforeChannel, removedChannel))
	}
	if operations.contract {
		steps = append(steps, newMirrorStep(operationKey, "contract", mirror.PhaseContractCAS,
			"group_model_contract", input.Policy.TargetGroup, "cas_group_model_contract",
			contract.Revision, desiredContract.Revision, contract, desiredContract))
	}
	if operations.groupRatio {
		steps = append(steps, newMirrorStep(operationKey, "group-ratio", mirror.PhaseContractCAS,
			"pricing_option", "GroupRatio", "cas_group_ratio", ratioBefore.Hash, ratioAfter.Hash,
			ratioBefore, ratioAfter))
	}
	if beforeHash, afterHash := mustChannelSemanticHash(removedChannel), mustChannelSemanticHash(finalChannel); beforeHash != afterHash {
		steps = append(steps, newMirrorStep(operationKey, "addition-channel", mirror.PhaseAdditionsExpose,
			"managed_channel_state", strconv.FormatInt(input.ChannelID, 10), "expose_added_models",
			beforeHash, afterHash, removedChannel, finalChannel))
	}
	expectedReadback := readbackState{ChannelSemanticHash: mustChannelSemanticHash(beforeChannel),
		ContractRevision: contract.Revision, GroupRatioHash: ratioBefore.Hash,
		TargetFunctionalHash: input.Plan.ExpectedFunctionalHash, VisibilityVerified: false}
	desiredReadback := readbackState{ChannelSemanticHash: mustChannelSemanticHash(finalChannel),
		ContractRevision: desiredContract.Revision, GroupRatioHash: ratioAfter.Hash,
		TargetFunctionalHash: input.Plan.DesiredFunctionalHash, VisibilityVerified: true}
	steps = append(steps, newMirrorStep(operationKey, "readback", mirror.PhaseReadback,
		"complete_mirror", input.Policy.TargetGroup, "verify_complete_mirror",
		mustHashJSON(expectedReadback), mustHashJSON(desiredReadback), expectedReadback, desiredReadback))

	proof, _ := compactJSON(input.SourceProof)
	changeSet := store.MirrorChangeSet{
		OperationKey: operationKey, RequestHash: requestHash, PolicyUUID: input.Policy.PolicyUUID,
		PolicyVersion: input.Policy.PolicyVersion, TargetSiteID: input.Target.ID,
		SourceProofJSON: string(proof), SourceSemanticHash: input.Plan.SourceSemanticHash,
		BeforeOwnedJSON: string(beforeOwnedJSON), DesiredOwnedJSON: string(desiredOwnedHint),
		ExpectedContractRevision: contract.Revision, DesiredContractRevision: desiredContract.Revision,
		Steps: steps,
	}
	return materializedChange{ChangeSet: changeSet, ExpectedOwned: expectedOwned,
		DesiredOwnedHint: desiredOwnedHint}, nil
}

type planOperations struct {
	removals, additions  []string
	disable, enable      bool
	contract, groupRatio bool
}

func parsePlanOperations(plan mirror.CompleteMirrorPlan) (planOperations, error) {
	result := planOperations{}
	for _, stage := range plan.Stages {
		for _, operation := range stage.Operations {
			switch operation.Kind {
			case "remove_managed_models":
				if stage.Phase != mirror.PhaseRemovalQuiesce {
					return result, errors.New("model removal is outside removal_quiesce")
				}
				result.removals = append(result.removals, operation.Models...)
			case "disable_managed_channel":
				result.disable = true
			case "cas_group_model_contract":
				result.contract = true
			case "cas_group_ratio":
				result.groupRatio = true
			case "add_managed_models":
				result.additions = append(result.additions, operation.Models...)
			case "enable_policy_disabled_channel":
				result.enable = true
			case "verify_channel_membership_and_status", "verify_group_model_contract", "verify_marketplace_ability_and_cache":
			default:
				return result, fmt.Errorf("unsupported mirror planner operation %q", operation.Kind)
			}
		}
	}
	result.removals = normalizeModels(result.removals)
	result.additions = normalizeModels(result.additions)
	return result, nil
}

func (e *Executor) materializeContract(ctx context.Context, input ApplyInput, changed bool) (
	domain.GroupModelPricingContract, domain.GroupModelPricingContract, error,
) {
	if changed {
		preview, err := e.target.PreviewGroupModelPricing(ctx, input.Target,
			input.Plan.ExpectedContractRevision, input.Plan.DesiredContract)
		if err != nil {
			return domain.GroupModelPricingContract{}, domain.GroupModelPricingContract{}, err
		}
		if !preview.ExpectedRevisionMatches || preview.CurrentRevision != input.Plan.ExpectedContractRevision ||
			preview.NoChange || preview.DesiredRevision == preview.CurrentRevision {
			return domain.GroupModelPricingContract{}, domain.GroupModelPricingContract{},
				errors.New("group-model contract changed or became a no-op after preview")
		}
		return preview.Current, preview.Desired, nil
	}
	current, err := e.target.ReadGroupModelPricing(ctx, input.Target)
	if err != nil {
		return domain.GroupModelPricingContract{}, domain.GroupModelPricingContract{}, err
	}
	if current.Revision != input.Plan.ExpectedContractRevision {
		return domain.GroupModelPricingContract{}, domain.GroupModelPricingContract{},
			errors.New("group-model contract revision changed since preview")
	}
	return current, current, nil
}

func (e *Executor) materializeGroupRatio(ctx context.Context, input ApplyInput, changed bool) (ratioState, ratioState, error) {
	snapshot, err := e.target.Snapshot(ctx, input.Target)
	if err != nil {
		return ratioState{}, ratioState{}, err
	}
	option, exists := snapshot.PricingOptions["GroupRatio"]
	if !exists || option.Hash == "" {
		return ratioState{}, ratioState{}, errors.New("target did not expose the GroupRatio CAS option")
	}
	current, exists := option.Values[input.Policy.TargetGroup]
	if !exists || !equalDecimal(current, input.Plan.ExpectedGroupRatio) {
		return ratioState{}, ratioState{}, errors.New("target group ratio changed since preview")
	}
	before := ratioState{Option: "GroupRatio", Values: cloneMap(option.Values), Hash: option.Hash}
	after := ratioState{Option: "GroupRatio", Values: cloneMap(option.Values), Hash: option.Hash}
	if changed {
		after.Values[input.Policy.TargetGroup] = canonicalDecimal(input.Plan.DesiredGroupRatio)
		after.Hash = targets.HashDecimalMap(after.Values)
		if after.Hash == "" || after.Hash == before.Hash {
			return ratioState{}, ratioState{}, errors.New("GroupRatio planner change materialized as a no-op")
		}
	}
	return before, after, nil
}

func (e *Executor) readAndVerifyOwnership(ctx context.Context, input ApplyInput,
	channel domain.TargetManagedChannelState, contract domain.GroupModelPricingContract) (store.MirrorOwnershipState, error) {
	channels, err := e.store.ListActiveMirrorChannelOwnerships(ctx, input.Policy.PolicyUUID)
	if err != nil || len(channels) != 1 {
		return store.MirrorOwnershipState{}, errors.New("complete mirror requires exactly one active channel ownership")
	}
	models, err := e.store.ListActiveMirrorModelOwnerships(ctx, input.Policy.PolicyUUID)
	if err != nil {
		return store.MirrorOwnershipState{}, err
	}
	memberships, err := e.store.ListActiveMirrorChannelModelOwnerships(ctx, input.Policy.PolicyUUID)
	if err != nil {
		return store.MirrorOwnershipState{}, err
	}
	marker := "relayops:policy/" + input.Policy.PolicyUUID
	ownedChannel := channels[0]
	if ownedChannel.TargetSiteID != input.Target.ID || ownedChannel.ChannelID != input.ChannelID ||
		ownedChannel.TargetGroup != input.Policy.TargetGroup || ownedChannel.OwnershipMarker != marker ||
		!ownedChannel.Active {
		return store.MirrorOwnershipState{}, errors.New("managed channel ownership changed")
	}
	group := contract.Groups[input.Policy.TargetGroup]
	// Remote state is already bound to the fresh preview and target CAS checks.
	// Keep the durable receipt as the transaction's expected local state, while
	// allowing this policy to converge administrator edits made since that receipt.
	for _, item := range models {
		setting, exists := group[item.TargetModel]
		if exists && setting.Source != marker {
			return store.MirrorOwnershipState{}, fmt.Errorf("owned contract model %q is foreign", item.TargetModel)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].TargetModel < models[j].TargetModel })
	sort.Slice(memberships, func(i, j int) bool { return memberships[i].TargetModel < memberships[j].TargetModel })
	return store.MirrorOwnershipState{Channel: ownedChannel, Models: models, Memberships: memberships}, nil
}

func newMirrorStep(operationKey, suffix string, phase mirror.MirrorPlanPhase, resourceKind, resourceID,
	action, expectedHash, desiredHash string, before, desired any) store.MirrorChangeStep {
	return store.MirrorChangeStep{
		StepKey: suffix, Phase: string(phase), ResourceKind: resourceKind, ResourceID: resourceID,
		Action: action, RequestID: deterministicRequestID(operationKey, suffix),
		ExpectedHash: expectedHash, DesiredHash: desiredHash,
		BeforeJSON: string(mustMarshalJSON(before)), DesiredJSON: string(mustMarshalJSON(desired)),
	}
}

func deterministicRequestID(operationKey, suffix string) string {
	sum := sha256.Sum256([]byte(operationKey + "\x00" + suffix))
	return "mirror-" + hex.EncodeToString(sum[:])[:48]
}

func compactJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func marshalJSON(value any) (json.RawMessage, error) { return json.Marshal(value) }

func mustMarshalJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func mustHashJSON(value any) string {
	hash, err := hashJSON(value)
	if err != nil {
		panic(err)
	}
	return hash
}

func mustChannelSemanticHash(value channelState) string {
	value.CASHash = ""
	value.Models = normalizeModels(value.Models)
	return mustHashJSON(value)
}

func normalizeModels(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func removeModels(values, removed []string) []string {
	blocked := make(map[string]struct{}, len(removed))
	for _, value := range removed {
		blocked[value] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, remove := blocked[value]; !remove {
			result = append(result, value)
		}
	}
	return normalizeModels(result)
}

func applyMembershipDeltaForMaterialize(values, additions []string) []string {
	return normalizeModels(append(append([]string(nil), values...), additions...))
}

func equalStrings(left, right []string) bool {
	left, right = normalizeModels(left), normalizeModels(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func channelEnabled(status int) bool { return status == managedChannelEnabled }

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func canonicalDecimal(raw string) string {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return value.String()
}

func equalDecimal(left, right string) bool {
	return decimalnorm.Equal(left, right)
}

func isNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) || store.IsNotFound(err) }
