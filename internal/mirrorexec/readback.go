package mirrorexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func (e *Executor) executeReadback(ctx context.Context, input ApplyInput, changeSet store.MirrorChangeSet,
	lease domain.TargetControlLease) (finalReadback, error) {
	step, err := readbackStep(changeSet.Steps)
	if err != nil {
		return finalReadback{}, err
	}
	if step.State == "pending" {
		intent := stepIntent{RequestID: step.RequestID, ResourceKind: step.ResourceKind, ResourceID: step.ResourceID,
			CASExpectedHash: step.ExpectedHash, Before: json.RawMessage(step.BeforeJSON), Desired: json.RawMessage(step.DesiredJSON)}
		if err := e.store.RecordMirrorStepIntent(ctx, changeSet.ID, step.StepKey,
			string(mustMarshalJSON(intent)), lease, e.now()); err != nil {
			return finalReadback{}, err
		}
		if err := e.boundary("intent_recorded:" + step.StepKey); err != nil {
			return finalReadback{}, err
		}
		changeSet, err = e.store.GetMirrorChangeSet(ctx, changeSet.ID)
		if err != nil {
			return finalReadback{}, err
		}
		step, _ = findStep(changeSet.Steps, step.StepKey)
	}
	if step.State == "succeeded" {
		readback, readErr := e.performFinalReadback(ctx, input, changeSet)
		if readErr == nil {
			return readback, nil
		}
		var failure *remoteStepFailure
		if errors.As(readErr, &failure) && failure.conflict {
			return finalReadback{}, e.markThirdState(ctx, changeSet.ID, step,
				mustMarshalJSON(readback), lease, readErr.Error())
		}
		return finalReadback{}, fmt.Errorf("%w: %v", ErrVisibilityPending, readErr)
	}
	if step.State == "needs_reconciliation" {
		return finalReadback{}, ErrNeedsReconciliation
	}
	if step.State != "intent_recorded" && step.State != "readback_pending" {
		return finalReadback{}, fmt.Errorf("unsupported readback step state %q", step.State)
	}

	readback, readErr := e.performFinalReadback(ctx, input, changeSet)
	result := stepResult{ResultState: "verified_success", ReadbackHash: step.DesiredHash,
		Result: mustMarshalJSON(readback)}
	if readErr != nil {
		result.ResultState = "readback_pending"
		result.ReadbackHash = ""
		result.Error = readErr.Error()
	}
	if step.State == "intent_recorded" {
		if err := e.store.RecordMirrorStepResult(ctx, changeSet.ID, step.StepKey,
			string(mustMarshalJSON(result)), result.Error, lease, e.now()); err != nil {
			return finalReadback{}, err
		}
		if err := e.boundary("result_recorded:" + step.StepKey); err != nil {
			return finalReadback{}, err
		}
	}
	if readErr != nil {
		var failure *remoteStepFailure
		if errors.As(readErr, &failure) && failure.conflict {
			payload := mustMarshalJSON(readback)
			return finalReadback{}, e.markThirdState(ctx, changeSet.ID, step, payload, lease, readErr.Error())
		}
		return finalReadback{}, fmt.Errorf("%w: %v", ErrVisibilityPending, readErr)
	}
	if _, err := e.store.ClassifyMirrorStepReadback(ctx, changeSet.ID, step.StepKey, step.DesiredHash,
		string(mustMarshalJSON(readback)), lease, e.now()); err != nil {
		return finalReadback{}, err
	}
	if err := e.appendVerifiedEvent(ctx, changeSet.ID, step, mustMarshalJSON(readback), lease); err != nil {
		return finalReadback{}, err
	}
	if err := e.boundary("step_verified:" + step.StepKey); err != nil {
		return finalReadback{}, err
	}
	return readback, nil
}

func (e *Executor) performFinalReadback(ctx context.Context, input ApplyInput,
	changeSet store.MirrorChangeSet) (finalReadback, error) {
	result := finalReadback{}
	channel, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
	if err != nil {
		return result, err
	}
	result.Channel = channel
	if !equalStrings(channel.Models, input.Plan.DesiredChannelModels) ||
		channelEnabled(channel.Status) != input.Plan.DesiredChannelEnabled {
		return result, &remoteStepFailure{err: errors.New("final channel readback differs from desired membership or status"), conflict: true}
	}
	contract, err := e.target.ReadGroupModelPricing(ctx, input.Target)
	if err != nil {
		return result, err
	}
	result.Contract = contract
	if contract.Revision != changeSet.DesiredContractRevision {
		return result, &remoteStepFailure{err: errors.New("final group-model contract revision differs from desired"), conflict: true}
	}
	snapshot, err := e.target.Snapshot(ctx, input.Target)
	if err != nil {
		return result, err
	}
	ratio, exists := snapshot.PricingOptions["GroupRatio"]
	if !exists {
		return result, &remoteStepFailure{err: errors.New("final GroupRatio option is absent"), conflict: true}
	}
	result.GroupRatio = ratio
	if !equalDecimal(ratio.Values[input.Policy.TargetGroup], input.Plan.DesiredGroupRatio) {
		return result, &remoteStepFailure{err: errors.New("final target group ratio differs from desired"), conflict: true}
	}
	functionalHash, err := mirror.TargetFunctionalHash(contract,
		map[string]string{input.Policy.TargetGroup: ratio.Values[input.Policy.TargetGroup]})
	if err != nil {
		return result, err
	}
	result.FunctionalHash = functionalHash
	if functionalHash != input.Plan.DesiredFunctionalHash {
		return result, &remoteStepFailure{err: errors.New("final target functional hash differs from desired"), conflict: true}
	}
	if e.visibility != nil {
		visibility, visibilityErr := e.visibility.VerifyMirrorVisibility(ctx, input.Target, input.ChannelID,
			input.Policy.TargetGroup, normalizeModels(input.Plan.DesiredChannelModels))
		if visibilityErr == nil && visibility.MarketplaceVisible && visibility.AbilityVisible && visibility.CacheVisible &&
			strings.TrimSpace(visibility.EvidenceHash) != "" &&
			(len(visibility.Details) == 0 || json.Valid(visibility.Details)) {
			result.Visibility = visibility
		}
	}
	return result, nil
}

func (e *Executor) commitOwnership(ctx context.Context, input ApplyInput, changeSet store.MirrorChangeSet,
	readback finalReadback, lease domain.TargetControlLease) error {
	var expected store.MirrorOwnershipState
	if err := json.Unmarshal([]byte(changeSet.BeforeOwnedJSON), &expected); err != nil {
		return err
	}
	desired, err := desiredOwnershipState(input, expected, readback)
	if err != nil {
		return err
	}
	replayed, err := e.store.CommitMirrorOwnershipAfterReadback(ctx, expected, desired,
		changeSet.PolicyVersion, lease, e.now())
	if err != nil {
		return err
	}
	payload := struct {
		Before   store.MirrorOwnershipState `json:"before"`
		After    store.MirrorOwnershipState `json:"after"`
		Replayed bool                       `json:"replayed"`
	}{expected, desired, replayed}
	if err := e.store.AppendMirrorChangeEvent(ctx, changeSet.ID, nil, "ownership_committed_after_readback",
		"readback", string(mustMarshalJSON(payload)), lease, e.now()); err != nil {
		return err
	}
	return e.boundary("ownership_committed")
}

func desiredOwnershipState(input ApplyInput, expected store.MirrorOwnershipState,
	readback finalReadback) (store.MirrorOwnershipState, error) {
	marker := "relayops:policy/" + input.Policy.PolicyUUID
	desired := store.MirrorOwnershipState{Channel: expected.Channel}
	desired.Channel.LastAppliedFunctionalHash = readback.Channel.Hash
	group, exists := readback.Contract.Groups[input.Policy.TargetGroup]
	if !exists {
		group = map[string]domain.GroupModelPricingSetting{}
	}
	existingModels := make(map[string]store.MirrorModelOwnership, len(expected.Models))
	for _, item := range expected.Models {
		existingModels[item.TargetModel] = item
		if setting, retained := group[item.TargetModel]; retained && strings.TrimSpace(setting.Source) != marker {
			return store.MirrorOwnershipState{}, fmt.Errorf("owned model %q lost its policy marker", item.TargetModel)
		}
	}
	modelNames := make([]string, 0, len(group))
	for modelName, setting := range group {
		if strings.TrimSpace(setting.Source) == marker {
			modelNames = append(modelNames, modelName)
		}
	}
	sort.Strings(modelNames)
	for _, modelName := range modelNames {
		setting := group[modelName]
		hash, err := mirror.TargetModelFunctionalHash(input.Policy.TargetGroup, modelName, setting)
		if err != nil {
			return store.MirrorOwnershipState{}, err
		}
		item, existed := existingModels[modelName]
		if !existed {
			item = store.MirrorModelOwnership{
				PolicyUUID: input.Policy.PolicyUUID, TargetSiteID: input.Target.ID,
				TargetGroup: input.Policy.TargetGroup, TargetModel: modelName, OwnershipMarker: marker,
				SourceIdentity: strings.TrimRight(input.SourceIdentity, "/") + "/model/" + modelName,
			}
		}
		item.LastAppliedFunctionalHash = hash
		desired.Models = append(desired.Models, item)
	}

	existingMemberships := make(map[string]store.MirrorChannelModelOwnership, len(expected.Memberships))
	for _, item := range expected.Memberships {
		existingMemberships[item.TargetModel] = item
	}
	for _, modelName := range normalizeModels(readback.Channel.Models) {
		item, existed := existingMemberships[modelName]
		setting, policyContract := group[modelName]
		claim := existed || expected.Channel.ModelSetAuthority == "exclusive" ||
			(policyContract && strings.TrimSpace(setting.Source) == marker)
		if !claim {
			continue
		}
		if !existed {
			item = store.MirrorChannelModelOwnership{
				PolicyUUID: input.Policy.PolicyUUID, TargetSiteID: input.Target.ID,
				TargetGroup: input.Policy.TargetGroup, ChannelID: input.ChannelID, TargetModel: modelName,
				OwnershipMarker: marker,
				SourceIdentity:  strings.TrimRight(input.SourceIdentity, "/") + "/model/" + modelName,
			}
		}
		hash, err := mirror.TargetChannelModelFunctionalHash(input.ChannelID, modelName, true)
		if err != nil {
			return store.MirrorOwnershipState{}, err
		}
		item.LastAppliedFunctionalHash = hash
		desired.Memberships = append(desired.Memberships, item)
	}
	sort.Slice(desired.Models, func(i, j int) bool { return desired.Models[i].TargetModel < desired.Models[j].TargetModel })
	sort.Slice(desired.Memberships, func(i, j int) bool {
		return desired.Memberships[i].TargetModel < desired.Memberships[j].TargetModel
	})
	return desired, nil
}

func readbackStep(steps []store.MirrorChangeStep) (store.MirrorChangeStep, error) {
	var result store.MirrorChangeStep
	for _, step := range steps {
		if step.ResourceKind != "complete_mirror" {
			continue
		}
		if result.ID != 0 {
			return store.MirrorChangeStep{}, errors.New("multiple complete-mirror readback steps were found")
		}
		result = step
	}
	if result.ID == 0 {
		return store.MirrorChangeStep{}, errors.New("complete-mirror readback step was not found")
	}
	return result, nil
}
