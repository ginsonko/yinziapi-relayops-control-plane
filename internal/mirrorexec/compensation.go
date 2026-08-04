package mirrorexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func (e *Executor) handleExecutionFailure(ctx context.Context, input ApplyInput, changeSetID int64,
	lease domain.TargetControlLease, replayed bool, executionErr error) (Receipt, error) {
	changeSet, getErr := e.store.GetMirrorChangeSet(ctx, changeSetID)
	if getErr != nil {
		return Receipt{}, errors.Join(executionErr, getErr)
	}
	receipt := Receipt{ChangeSet: changeSet, Replayed: replayed}
	if errors.Is(executionErr, ErrInjectedCrash) || errors.Is(executionErr, ErrVisibilityPending) ||
		errors.Is(executionErr, store.ErrTargetControlFenceLost) {
		return receipt, executionErr
	}
	var failure *remoteStepFailure
	if errors.As(executionErr, &failure) {
		if failure.unknown {
			return receipt, executionErr
		}
		if failure.conflict {
			if err := e.ensureNeedsReconciliation(ctx, changeSet, lease, executionErr.Error()); err != nil {
				return receipt, errors.Join(executionErr, err)
			}
			changeSet, _ = e.store.GetMirrorChangeSet(ctx, changeSetID)
			return Receipt{ChangeSet: changeSet, Replayed: replayed}, ErrNeedsReconciliation
		}
	}
	if errors.Is(executionErr, ErrNeedsReconciliation) {
		changeSet, _ = e.store.GetMirrorChangeSet(ctx, changeSetID)
		return Receipt{ChangeSet: changeSet, Replayed: replayed}, executionErr
	}
	if !hasSucceededRemoteStep(changeSet.Steps) && changeSet.Status == "pending" {
		return receipt, executionErr
	}
	if err := e.ensureNeedsReconciliation(ctx, changeSet, lease, executionErr.Error()); err != nil {
		return receipt, errors.Join(executionErr, err)
	}
	if err := e.compensate(ctx, input, changeSetID, lease); err != nil {
		changeSet, _ = e.store.GetMirrorChangeSet(ctx, changeSetID)
		return Receipt{ChangeSet: changeSet, Replayed: replayed}, errors.Join(executionErr, err)
	}
	changeSet, _ = e.store.GetMirrorChangeSet(ctx, changeSetID)
	return Receipt{ChangeSet: changeSet, Replayed: replayed},
		fmt.Errorf("mirror apply failed and was conditionally compensated: %w", executionErr)
}

func (e *Executor) ensureNeedsReconciliation(ctx context.Context, changeSet store.MirrorChangeSet,
	lease domain.TargetControlLease, reason string) error {
	if changeSet.Status == "needs_reconciliation" {
		return nil
	}
	if changeSet.Status == "applied" || changeSet.Status == "compensated" {
		return store.ErrMirrorInvalidTransition
	}
	if err := e.store.TransitionMirrorChangeSet(ctx, changeSet.ID, changeSet.Status,
		"needs_reconciliation", lease, e.now()); err != nil {
		return err
	}
	payload := struct {
		Reason string `json:"reason"`
	}{reason}
	return e.store.AppendMirrorChangeEvent(ctx, changeSet.ID, nil, "compensation_started",
		"compensation", string(mustMarshalJSON(payload)), lease, e.now())
}

func (e *Executor) compensate(ctx context.Context, input ApplyInput, changeSetID int64,
	lease domain.TargetControlLease) error {
	changeSet, err := e.store.GetMirrorChangeSet(ctx, changeSetID)
	if err != nil {
		return err
	}
	steps := append([]store.MirrorChangeStep(nil), changeSet.Steps...)
	sort.Slice(steps, func(i, j int) bool { return steps[i].ID > steps[j].ID })
	for _, step := range steps {
		if step.State != "succeeded" || step.CompensationState == "compensated" {
			continue
		}
		payload, compensationErr := e.compensateStep(ctx, input, step, lease)
		state := "compensated"
		if compensationErr != nil {
			state = "compensation_failed"
			if len(payload) == 0 {
				payload = mustMarshalJSON(struct {
					Error string `json:"error"`
				}{compensationErr.Error()})
			}
		}
		if err := e.store.RecordMirrorStepCompensation(ctx, changeSetID, step.StepKey, state,
			string(payload), lease, e.now()); err != nil {
			return errors.Join(compensationErr, err)
		}
		if compensationErr != nil {
			return fmt.Errorf("%w: conditional compensation for %s failed: %v",
				ErrNeedsReconciliation, step.StepKey, compensationErr)
		}
		if err := e.boundary("compensated:" + step.StepKey); err != nil {
			return err
		}
	}
	changeSet, err = e.store.GetMirrorChangeSet(ctx, changeSetID)
	if err != nil {
		return err
	}
	if err := e.store.TransitionMirrorChangeSet(ctx, changeSetID, "needs_reconciliation",
		"compensated", lease, e.now()); err != nil {
		return err
	}
	return e.boundary("phase:compensated")
}

func (e *Executor) compensateStep(ctx context.Context, input ApplyInput, step store.MirrorChangeStep,
	lease domain.TargetControlLease) (json.RawMessage, error) {
	guard := func() error { return e.store.CheckTargetControlLease(ctx, lease, e.now()) }
	if step.ResourceKind == "complete_mirror" {
		return mustMarshalJSON(struct {
			ReadOnly bool `json:"read_only"`
		}{true}), nil
	}
	switch step.ResourceKind {
	case "managed_channel_state":
		var before, desired channelState
		if err := json.Unmarshal([]byte(step.BeforeJSON), &before); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(step.DesiredJSON), &desired); err != nil {
			return nil, err
		}
		live, err := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
		if err != nil {
			return nil, err
		}
		liveSemantic := mustChannelSemanticHash(channelFromDomain(live))
		if liveSemantic == step.ExpectedHash {
			return compensationPayload(step, live, live, true), nil
		}
		var appliedReadback domain.TargetManagedChannelState
		if err := json.Unmarshal([]byte(step.ReadbackJSON), &appliedReadback); err != nil {
			return nil, err
		}
		if liveSemantic != step.DesiredHash || appliedReadback.Hash == "" || live.Hash != appliedReadback.Hash {
			return compensationPayload(step, desired, live, false),
				errors.New("managed channel changed after apply; compensation would overwrite external state")
		}
		status := before.Status
		mutation, mutationErr := e.target.ApplyManagedChannelState(ctx, input.Target, input.ChannelID,
			live.Hash, before.Models, &status, compensationRequestID(step.RequestID), guard)
		readback, readErr := e.target.ReadManagedChannelState(ctx, input.Target, input.ChannelID)
		if readErr == nil && mustChannelSemanticHash(channelFromDomain(readback)) == step.ExpectedHash {
			return compensationPayload(step, live, readback, false), nil
		}
		return compensationPayload(step, live, readback, false), errors.Join(mutationErr, readErr,
			fmt.Errorf("managed channel compensation state=%s", mutation.ResultState))
	case "group_model_contract":
		var before domain.GroupModelPricingContract
		if err := json.Unmarshal([]byte(step.BeforeJSON), &before); err != nil {
			return nil, err
		}
		live, err := e.target.ReadGroupModelPricing(ctx, input.Target)
		if err != nil {
			return nil, err
		}
		if live.Revision == step.ExpectedHash {
			return compensationPayload(step, live, live, true), nil
		}
		if live.Revision != step.DesiredHash {
			return compensationPayload(step, json.RawMessage(step.DesiredJSON), live, false),
				errors.New("group-model contract changed after apply; compensation was refused")
		}
		rolledBack, rollbackErr := e.target.RollbackGroupModelPricingControlled(ctx, input.Target,
			live.Revision, before, guard)
		readback, readErr := e.target.ReadGroupModelPricing(ctx, input.Target)
		if readErr == nil && readback.Revision == step.ExpectedHash {
			return compensationPayload(step, live, readback, false), nil
		}
		return compensationPayload(step, live, rolledBack, false), errors.Join(rollbackErr, readErr)
	case "pricing_option":
		var before ratioState
		if err := json.Unmarshal([]byte(step.BeforeJSON), &before); err != nil {
			return nil, err
		}
		snapshot, err := e.target.Snapshot(ctx, input.Target)
		if err != nil {
			return nil, err
		}
		live, exists := snapshot.PricingOptions["GroupRatio"]
		if !exists {
			return nil, errors.New("GroupRatio disappeared before compensation")
		}
		if live.Hash == step.ExpectedHash {
			return compensationPayload(step, live, live, true), nil
		}
		if live.Hash != step.DesiredHash {
			return compensationPayload(step, json.RawMessage(step.DesiredJSON), live, false),
				errors.New("GroupRatio changed after apply; compensation was refused")
		}
		mutation, mutationErr := e.target.ApplyPricingOptionControlled(ctx, input.Target, "GroupRatio",
			live.Hash, before.Values, guard)
		readbackSnapshot, readErr := e.target.Snapshot(ctx, input.Target)
		readback := readbackSnapshot.PricingOptions["GroupRatio"]
		if readErr == nil && readback.Hash == step.ExpectedHash {
			return compensationPayload(step, live, readback, false), nil
		}
		return compensationPayload(step, live, mutation, false), errors.Join(mutationErr, readErr)
	default:
		return nil, fmt.Errorf("unsupported compensation resource %q", step.ResourceKind)
	}
}

func compensationPayload(step store.MirrorChangeStep, applied, restored any, replayed bool) json.RawMessage {
	return mustMarshalJSON(struct {
		ResourceKind string `json:"resource_kind"`
		ResourceID   string `json:"resource_id"`
		Action       string `json:"action"`
		Applied      any    `json:"applied"`
		Restored     any    `json:"restored"`
		Replayed     bool   `json:"replayed"`
	}{step.ResourceKind, step.ResourceID, step.Action, applied, restored, replayed})
}

func compensationRequestID(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if len(requestID) <= 115 {
		return requestID + "-compensation"
	}
	return deterministicRequestID(requestID, "compensation")
}

func hasSucceededRemoteStep(steps []store.MirrorChangeStep) bool {
	for _, step := range steps {
		if step.State == "succeeded" && step.ResourceKind != "complete_mirror" {
			return true
		}
	}
	return false
}
