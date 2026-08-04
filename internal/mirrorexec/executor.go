package mirrorexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type remoteStepFailure struct {
	err      error
	conflict bool
	unknown  bool
}

func (failure *remoteStepFailure) Error() string { return failure.err.Error() }
func (failure *remoteStepFailure) Unwrap() error { return failure.err }

func (e *Executor) Apply(ctx context.Context, input ApplyInput) (Receipt, error) {
	operationKey, requestHash, err := e.requestIdentity(input)
	if err != nil {
		return Receipt{}, err
	}
	now := e.now()
	lease, err := e.store.AcquireMirrorLease(ctx, input.Target.ID,
		e.options.WorkerID+":"+operationKey, now, e.options.LeaseTTL)
	if err != nil {
		return Receipt{}, err
	}
	defer func() {
		_ = e.store.ReleaseTargetControlLease(context.Background(), lease, e.now())
	}()

	changeSet, replayed, err := e.loadOrCreateChangeSet(ctx, input, operationKey, requestHash, lease)
	if err != nil {
		return Receipt{}, err
	}
	if changeSet.Status == "applied" {
		return Receipt{ChangeSet: changeSet, Replayed: true}, nil
	}
	if changeSet.Status == "compensated" {
		return Receipt{ChangeSet: changeSet, Replayed: true}, errors.New("mirror operation was previously compensated")
	}
	if changeSet.Status == "needs_reconciliation" {
		return Receipt{ChangeSet: changeSet, Replayed: replayed}, ErrNeedsReconciliation
	}

	for {
		changeSet, err = e.store.GetMirrorChangeSet(ctx, changeSet.ID)
		if err != nil {
			return Receipt{}, err
		}
		switch changeSet.Status {
		case "pending":
			err = e.verifyPreflight(ctx, input, changeSet, lease)
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "pending", "preflight_verified", lease)
			}
		case "preflight_verified":
			err = e.executeRemotePhase(ctx, input, changeSet, mirror.PhaseRemovalQuiesce, lease)
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "preflight_verified", "removals_quiesced", lease)
			}
		case "removals_quiesced":
			err = e.executeRemotePhase(ctx, input, changeSet, mirror.PhaseContractCAS, lease)
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "removals_quiesced", "contract_applied", lease)
			}
		case "contract_applied":
			err = e.executeRemotePhase(ctx, input, changeSet, mirror.PhaseAdditionsExpose, lease)
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "contract_applied", "additions_exposed", lease)
			}
		case "additions_exposed":
			var readback finalReadback
			err = e.ensureCurrentPolicy(ctx, changeSet)
			if err == nil {
				readback, err = e.executeReadback(ctx, input, changeSet, lease)
			}
			if err == nil {
				err = e.ensureCurrentPolicy(ctx, changeSet)
			}
			if err == nil {
				err = e.commitOwnership(ctx, input, changeSet, readback, lease)
			}
			if err == nil {
				err = e.ensureCurrentPolicy(ctx, changeSet)
			}
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "additions_exposed", "readback_verified", lease)
			}
		case "readback_verified":
			err = e.ensureCurrentPolicy(ctx, changeSet)
			if err == nil {
				err = e.transition(ctx, changeSet.ID, "readback_verified", "applied", lease)
			}
		case "applied":
			return Receipt{ChangeSet: changeSet, Replayed: replayed}, nil
		case "compensated":
			return Receipt{ChangeSet: changeSet, Replayed: replayed}, errors.New("mirror operation was compensated")
		case "needs_reconciliation":
			return Receipt{ChangeSet: changeSet, Replayed: replayed}, ErrNeedsReconciliation
		default:
			return Receipt{ChangeSet: changeSet, Replayed: replayed},
				fmt.Errorf("unknown mirror change-set status %q", changeSet.Status)
		}
		if err != nil {
			return e.handleExecutionFailure(ctx, input, changeSet.ID, lease, replayed, err)
		}
	}
}

func (e *Executor) loadOrCreateChangeSet(ctx context.Context, input ApplyInput, operationKey, requestHash string,
	lease domain.TargetControlLease) (store.MirrorChangeSet, bool, error) {
	existing, err := e.store.GetMirrorChangeSetByOperationKey(ctx, operationKey)
	if err == nil {
		if existing.RequestHash != requestHash || existing.PolicyUUID != input.Policy.PolicyUUID ||
			existing.PolicyVersion != input.Policy.PolicyVersion || existing.TargetSiteID != input.Target.ID {
			return store.MirrorChangeSet{}, false, store.ErrMirrorIdempotencyConflict
		}
		if existing.Status != "applied" && existing.Status != "compensated" {
			if err := e.store.ClaimMirrorChangeSet(ctx, existing.ID, lease, e.now()); err != nil {
				return store.MirrorChangeSet{}, false, err
			}
			existing, err = e.store.GetMirrorChangeSet(ctx, existing.ID)
		}
		return existing, true, err
	}
	if !isNotFound(err) {
		return store.MirrorChangeSet{}, false, err
	}
	materialized, err := e.materialize(ctx, input, operationKey, requestHash)
	if err != nil {
		return store.MirrorChangeSet{}, false, err
	}
	changeSet := materialized.ChangeSet
	replayed, err := e.store.CreateMirrorChangeSet(ctx, &changeSet, lease, e.now())
	if err != nil {
		return store.MirrorChangeSet{}, false, err
	}
	if err := e.boundary("change_set_created"); err != nil {
		return changeSet, replayed, err
	}
	return changeSet, replayed, nil
}

func (e *Executor) verifyPreflight(ctx context.Context, input ApplyInput, changeSet store.MirrorChangeSet,
	lease domain.TargetControlLease) error {
	policy, err := e.store.GetMirrorPolicy(ctx, changeSet.PolicyUUID)
	if err != nil || policy.PolicyVersion != changeSet.PolicyVersion || !policy.Enabled {
		return &remoteStepFailure{err: errors.New("mirror policy changed or was disabled before apply"), conflict: true}
	}
	seenChannel := false
	for _, step := range changeSet.Steps {
		switch step.ResourceKind {
		case "managed_channel_state":
			if seenChannel {
				continue
			}
			seenChannel = true
			var before channelState
			if err := json.Unmarshal([]byte(step.BeforeJSON), &before); err != nil {
				return err
			}
			live, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
			if err != nil {
				return err
			}
			if (before.CASHash != "" && live.Hash != before.CASHash) ||
				mustChannelSemanticHash(channelFromDomain(live)) != step.ExpectedHash {
				return &remoteStepFailure{err: errors.New("managed channel changed after preview"), conflict: true}
			}
		case "group_model_contract":
			live, err := e.target.ReadGroupModelPricing(ctx, input.Target)
			if err != nil {
				return err
			}
			if live.Revision != step.ExpectedHash {
				return &remoteStepFailure{err: errors.New("group-model contract changed after preview"), conflict: true}
			}
		case "pricing_option":
			live, err := e.target.Snapshot(ctx, input.Target)
			if err != nil {
				return err
			}
			if live.PricingOptions["GroupRatio"].Hash != step.ExpectedHash {
				return &remoteStepFailure{err: errors.New("GroupRatio changed after preview"), conflict: true}
			}
		}
	}
	if err := e.store.CheckTargetControlLease(ctx, lease, e.now()); err != nil {
		return err
	}
	return e.boundary("preflight_verified")
}

func (e *Executor) executeRemotePhase(ctx context.Context, input ApplyInput, changeSet store.MirrorChangeSet,
	phase mirror.MirrorPlanPhase, lease domain.TargetControlLease) error {
	if err := e.ensureCurrentPolicy(ctx, changeSet); err != nil {
		return err
	}
	steps := append([]store.MirrorChangeStep(nil), changeSet.Steps...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].ID < steps[j].ID })
	for _, step := range steps {
		if step.Phase != string(phase) {
			continue
		}
		if err := e.executeRemoteStep(ctx, input, changeSet.ID, step.StepKey, lease); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) executeRemoteStep(ctx context.Context, input ApplyInput, changeSetID int64, stepKey string,
	lease domain.TargetControlLease) error {
	for {
		changeSet, err := e.store.GetMirrorChangeSet(ctx, changeSetID)
		if err != nil {
			return err
		}
		step, err := findStep(changeSet.Steps, stepKey)
		if err != nil {
			return err
		}
		switch step.State {
		case "succeeded":
			return nil
		case "needs_reconciliation":
			return ErrNeedsReconciliation
		case "intent_recorded", "readback_pending":
			classification, readbackJSON, conflict, err := e.classifyRemoteStep(ctx, input, step)
			if err != nil {
				return &remoteStepFailure{err: err, unknown: true}
			}
			if conflict {
				classification = "third"
			}
			switch classification {
			case "desired":
				if _, err := e.store.ClassifyMirrorStepReadback(ctx, changeSetID, step.StepKey,
					step.DesiredHash, string(readbackJSON), lease, e.now()); err != nil {
					return err
				}
				if err := e.appendVerifiedEvent(ctx, changeSetID, step, readbackJSON, lease); err != nil {
					return err
				}
				return e.boundary("step_verified:" + step.StepKey)
			case "before":
				if _, err := e.store.ClassifyMirrorStepReadback(ctx, changeSetID, step.StepKey,
					step.ExpectedHash, string(readbackJSON), lease, e.now()); err != nil {
					return err
				}
				if conflict {
					return e.markThirdState(ctx, changeSetID, step, readbackJSON, lease,
						"target reported a CAS conflict")
				}
				if step.Attempts >= e.options.MaxAttempts {
					return &remoteStepFailure{err: fmt.Errorf("remote step %s remained at its before state after %d attempts",
						step.StepKey, step.Attempts)}
				}
				continue
			default:
				return e.markThirdState(ctx, changeSetID, step, readbackJSON, lease,
					"remote state matched neither the recorded before nor desired state")
			}
		case "pending":
			if err := e.ensureCurrentPolicy(ctx, changeSet); err != nil {
				return err
			}
			intent, err := e.buildStepIntent(ctx, input, step)
			if err != nil {
				return err
			}
			intentJSON := mustMarshalJSON(intent)
			if err := e.store.RecordMirrorStepIntent(ctx, changeSetID, step.StepKey, string(intentJSON), lease, e.now()); err != nil {
				return err
			}
			if err := e.boundary("intent_recorded:" + step.StepKey); err != nil {
				return err
			}
			result, mutationErr := e.sendRemoteStep(ctx, input, step, intent, lease)
			if err := e.boundary("transport_returned:" + step.StepKey); err != nil {
				return err
			}
			resultJSON := mustMarshalJSON(result)
			message := ""
			if mutationErr != nil {
				message = mutationErr.Error()
			}
			if err := e.store.RecordMirrorStepResult(ctx, changeSetID, step.StepKey, string(resultJSON),
				message, lease, e.now()); err != nil {
				return err
			}
			if err := e.boundary("result_recorded:" + step.StepKey); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("unsupported mirror step state %q", step.State)
		}
	}
}

func (e *Executor) buildStepIntent(ctx context.Context, input ApplyInput,
	step store.MirrorChangeStep) (stepIntent, error) {
	intent := stepIntent{RequestID: step.RequestID, ResourceKind: step.ResourceKind, ResourceID: step.ResourceID,
		Before: json.RawMessage(step.BeforeJSON), Desired: json.RawMessage(step.DesiredJSON)}
	switch step.ResourceKind {
	case "managed_channel_state":
		live, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
		if err != nil {
			return stepIntent{}, err
		}
		if mustChannelSemanticHash(channelFromDomain(live)) != step.ExpectedHash {
			return stepIntent{}, &remoteStepFailure{err: errors.New("managed channel is not at the step before state"), conflict: true}
		}
		var before channelState
		if err := json.Unmarshal([]byte(step.BeforeJSON), &before); err != nil {
			return stepIntent{}, err
		}
		if before.CASHash != "" && before.CASHash != live.Hash {
			return stepIntent{}, &remoteStepFailure{err: errors.New("managed channel CAS hash drifted before send"), conflict: true}
		}
		intent.CASExpectedHash = live.Hash
	case "group_model_contract", "pricing_option":
		intent.CASExpectedHash = step.ExpectedHash
	default:
		return stepIntent{}, fmt.Errorf("unsupported remote resource kind %q", step.ResourceKind)
	}
	return intent, nil
}

func (e *Executor) sendRemoteStep(ctx context.Context, input ApplyInput, step store.MirrorChangeStep,
	intent stepIntent, lease domain.TargetControlLease) (stepResult, error) {
	guard := func() error {
		if err := e.store.CheckTargetControlLease(ctx, lease, e.now()); err != nil {
			return err
		}
		return e.ensureCurrentPolicy(ctx, store.MirrorChangeSet{
			PolicyUUID: input.Policy.PolicyUUID, PolicyVersion: input.Policy.PolicyVersion,
		})
	}
	switch step.ResourceKind {
	case "managed_channel_state":
		var desired channelState
		if err := json.Unmarshal([]byte(step.DesiredJSON), &desired); err != nil {
			return stepResult{}, err
		}
		status := desired.Status
		mutation, err := e.target.ApplyManagedChannelState(ctx, input.Target, input.ChannelID,
			intent.CASExpectedHash, desired.Models, &status, step.RequestID, guard)
		encoded := mustMarshalJSON(mutation)
		return stepResult{ResultState: mutation.ResultState, ReadbackHash: mutation.ReadbackHash,
			Result: encoded, Error: mutation.Error}, err
	case "group_model_contract":
		var desired domain.GroupModelPricingContract
		if err := json.Unmarshal([]byte(step.DesiredJSON), &desired); err != nil {
			return stepResult{}, err
		}
		mutation, err := e.target.ApplyGroupModelPricingControlled(ctx, input.Target,
			intent.CASExpectedHash, desired, guard)
		encoded := mustMarshalJSON(mutation)
		return stepResult{ResultState: mutation.ResultState, ReadbackHash: mutation.ReadbackHash,
			Result: encoded, Error: mutation.Error}, err
	case "pricing_option":
		var desired ratioState
		if err := json.Unmarshal([]byte(step.DesiredJSON), &desired); err != nil {
			return stepResult{}, err
		}
		mutation, err := e.target.ApplyPricingOptionControlled(ctx, input.Target, desired.Option,
			intent.CASExpectedHash, desired.Values, guard)
		encoded := mustMarshalJSON(mutation)
		return stepResult{ResultState: mutation.ResultState, ReadbackHash: mutation.ReadbackHash,
			Result: encoded, Error: mutation.Error}, err
	default:
		return stepResult{}, fmt.Errorf("unsupported remote resource kind %q", step.ResourceKind)
	}
}

func (e *Executor) classifyRemoteStep(ctx context.Context, input ApplyInput,
	step store.MirrorChangeStep) (string, json.RawMessage, bool, error) {
	var result stepResult
	if strings.TrimSpace(step.ResultJSON) != "" {
		if err := json.Unmarshal([]byte(step.ResultJSON), &result); err != nil {
			return "", nil, false, err
		}
	}
	conflict := strings.Contains(result.ResultState, "conflict")
	switch step.ResourceKind {
	case "managed_channel_state":
		live, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
		if err != nil {
			return "", nil, conflict, err
		}
		encoded := mustMarshalJSON(live)
		semantic := mustChannelSemanticHash(channelFromDomain(live))
		var intent stepIntent
		if err := json.Unmarshal([]byte(step.IntentJSON), &intent); err != nil {
			return "", nil, conflict, err
		}
		switch {
		case semantic == step.ExpectedHash && live.Hash == intent.CASExpectedHash:
			return "before", encoded, conflict, nil
		case semantic == step.DesiredHash:
			return "desired", encoded, conflict, nil
		default:
			return "third", encoded, conflict, nil
		}
	case "group_model_contract":
		live, err := e.target.ReadGroupModelPricing(ctx, input.Target)
		if err != nil {
			return "", nil, conflict, err
		}
		encoded := mustMarshalJSON(live)
		if live.Revision == step.ExpectedHash {
			return "before", encoded, conflict, nil
		}
		if live.Revision == step.DesiredHash {
			return "desired", encoded, conflict, nil
		}
		return "third", encoded, conflict, nil
	case "pricing_option":
		live, err := e.target.Snapshot(ctx, input.Target)
		if err != nil {
			return "", nil, conflict, err
		}
		option, exists := live.PricingOptions["GroupRatio"]
		if !exists {
			return "third", mustMarshalJSON(live), conflict, nil
		}
		encoded := mustMarshalJSON(option)
		if option.Hash == step.ExpectedHash {
			return "before", encoded, conflict, nil
		}
		if option.Hash == step.DesiredHash {
			return "desired", encoded, conflict, nil
		}
		return "third", encoded, conflict, nil
	default:
		return "", nil, conflict, fmt.Errorf("unsupported remote resource kind %q", step.ResourceKind)
	}
}

func (e *Executor) markThirdState(ctx context.Context, changeSetID int64, step store.MirrorChangeStep,
	readback json.RawMessage, lease domain.TargetControlLease, reason string) error {
	thirdHash := "third:" + mustHashJSON(struct {
		Reason   string          `json:"reason"`
		Readback json.RawMessage `json:"readback"`
	}{reason, readback})
	if _, err := e.store.ClassifyMirrorStepReadback(ctx, changeSetID, step.StepKey, thirdHash,
		string(readback), lease, e.now()); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrNeedsReconciliation, reason)
}

func (e *Executor) appendVerifiedEvent(ctx context.Context, changeSetID int64, step store.MirrorChangeStep,
	readback json.RawMessage, lease domain.TargetControlLease) error {
	payload := struct {
		ResourceKind string          `json:"resource_kind"`
		ResourceID   string          `json:"resource_id"`
		Action       string          `json:"action"`
		RequestID    string          `json:"request_id"`
		Before       json.RawMessage `json:"before"`
		After        json.RawMessage `json:"after"`
		Readback     json.RawMessage `json:"readback"`
	}{step.ResourceKind, step.ResourceID, step.Action, step.RequestID,
		json.RawMessage(step.BeforeJSON), json.RawMessage(step.DesiredJSON), readback}
	stepID := step.ID
	return e.store.AppendMirrorChangeEvent(ctx, changeSetID, &stepID, "remote_change_verified", step.Phase,
		string(mustMarshalJSON(payload)), lease, e.now())
}

func (e *Executor) ensureCurrentPolicy(ctx context.Context, changeSet store.MirrorChangeSet) error {
	policy, err := e.store.GetMirrorPolicy(ctx, changeSet.PolicyUUID)
	if err != nil {
		return err
	}
	if !policy.Enabled || policy.PolicyVersion != changeSet.PolicyVersion {
		return &remoteStepFailure{err: errors.New("mirror policy changed or was disabled during apply"), conflict: true}
	}
	return nil
}

func (e *Executor) transition(ctx context.Context, changeSetID int64, from, to string,
	lease domain.TargetControlLease) error {
	if err := e.store.TransitionMirrorChangeSet(ctx, changeSetID, from, to, lease, e.now()); err != nil {
		return err
	}
	return e.boundary("phase:" + to)
}

func (e *Executor) boundary(name string) error {
	if e.options.CrashHook == nil {
		return nil
	}
	if err := e.options.CrashHook(name); err != nil {
		return fmt.Errorf("%w at %s: %v", ErrInjectedCrash, name, err)
	}
	return nil
}

func (e *Executor) now() time.Time { return e.options.Now().UTC() }

func findStep(steps []store.MirrorChangeStep, key string) (store.MirrorChangeStep, error) {
	for _, step := range steps {
		if step.StepKey == key {
			return step, nil
		}
	}
	return store.MirrorChangeStep{}, fmt.Errorf("mirror step %q was not found", key)
}

func channelFromDomain(value domain.TargetManagedChannelState) channelState {
	return channelState{ChannelID: value.ChannelID, Models: normalizeModels(value.Models), Status: value.Status,
		CASHash: value.Hash}
}
