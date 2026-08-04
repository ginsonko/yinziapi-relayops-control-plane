package targets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (a *NewAPI) ReadManagedChannelState(
	ctx context.Context,
	target domain.TargetSite,
	channelID int64,
) (domain.TargetManagedChannelState, error) {
	if channelID <= 0 {
		return domain.TargetManagedChannelState{}, errors.New("managed channel id must be positive")
	}
	load := func(loadCtx context.Context) (domain.TargetManagedChannelState, error) {
		return a.readManagedChannelStateUncached(loadCtx, target, channelID)
	}
	if a.managed == nil {
		return load(ctx)
	}
	key := targetCatalogCacheKey(target) + "|" + strconv.FormatInt(channelID, 10)
	return a.managed.read(ctx, key, load)
}

func (a *NewAPI) readManagedChannelStateUncached(
	ctx context.Context,
	target domain.TargetSite,
	channelID int64,
) (domain.TargetManagedChannelState, error) {
	body, err := a.managementRequest(ctx, target, http.MethodGet,
		"/api/channel/"+strconv.FormatInt(channelID, 10)+"/managed_state", nil, nil)
	if err != nil {
		return domain.TargetManagedChannelState{}, fmt.Errorf("read managed channel state: %w", err)
	}
	var state domain.TargetManagedChannelState
	if err := decodeManagementData(body, &state); err != nil {
		return domain.TargetManagedChannelState{}, fmt.Errorf("decode managed channel state: %w", err)
	}
	return normalizeManagedChannelState(state, channelID)
}

func (a *NewAPI) ApplyManagedChannelState(
	ctx context.Context,
	target domain.TargetSite,
	channelID int64,
	expectedHash string,
	desiredModels []string,
	desiredStatus *int,
	requestID string,
	beforeWrite func() error,
) (domain.TargetManagedChannelMutation, error) {
	result := domain.TargetManagedChannelMutation{ExpectedHash: strings.TrimSpace(expectedHash)}
	if err := a.remoteWrites.Check("managed channel state apply"); err != nil {
		result.ResultState = "rejected_before_write"
		result.Error = err.Error()
		return result, err
	}
	if channelID <= 0 {
		return rejectedManagedChannelMutation(result, errors.New("managed channel id must be positive"))
	}
	if result.ExpectedHash == "" {
		return rejectedManagedChannelMutation(result, errors.New("managed channel expected hash is required"))
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 {
		return rejectedManagedChannelMutation(result, errors.New("managed channel request id must contain 1 to 128 bytes"))
	}
	desiredModels = normalizeModels(desiredModels)
	if desiredStatus != nil && *desiredStatus != channelStatusEnabled && *desiredStatus != channelStatusManuallyDisabled {
		return rejectedManagedChannelMutation(result, fmt.Errorf("managed channel status %d is not supported", *desiredStatus))
	}

	current, err := a.ReadManagedChannelState(WithFreshRead(ctx), target, channelID)
	if err != nil {
		return rejectedManagedChannelMutation(result, err)
	}
	result.ReadbackHash = current.Hash
	result.State = current
	if current.Hash != result.ExpectedHash {
		result.ResultState = "conflict_before_write"
		result.Error = "managed channel state changed since preview"
		return result, errors.New(result.Error)
	}
	wantedStatus := current.Status
	if desiredStatus != nil {
		wantedStatus = *desiredStatus
	}
	if len(desiredModels) == 0 && wantedStatus == channelStatusEnabled {
		return rejectedManagedChannelMutation(result, errors.New("an empty managed model set cannot remain enabled"))
	}
	if managedChannelStateMatches(current, desiredModels, wantedStatus) {
		result.ResultState = "verified_success"
		return result, nil
	}
	if beforeWrite == nil {
		return rejectedManagedChannelMutation(result, errors.New("a fencing guard is required before managed channel mutation"))
	}
	if err := beforeWrite(); err != nil {
		return rejectedManagedChannelMutation(result, err)
	}

	payload := map[string]any{
		"expected_hash":  result.ExpectedHash,
		"desired_models": desiredModels,
		"request_id":     requestID,
	}
	if desiredStatus != nil {
		payload["desired_status"] = *desiredStatus
	}
	path := "/api/channel/" + strconv.FormatInt(channelID, 10) + "/managed_state"
	body, writeErr := a.managementRequest(ctx, target, http.MethodPut, path, nil, payload)
	if writeErr == nil {
		var response struct {
			Changed bool                             `json:"changed"`
			State   domain.TargetManagedChannelState `json:"state"`
		}
		if err := decodeManagementData(body, &response); err != nil {
			writeErr = fmt.Errorf("decode managed channel mutation: %w", err)
		} else if _, err := normalizeManagedChannelState(response.State, channelID); err != nil {
			writeErr = err
		}
	}

	readback, readErr := a.ReadManagedChannelState(ctx, target, channelID)
	if readErr == nil {
		result.ReadbackHash = readback.Hash
		result.State = readback
		if managedChannelStateMatches(readback, desiredModels, wantedStatus) {
			result.ResultState = "verified_success"
			result.Changed = true
			return result, nil
		}
		if readback.Hash == result.ExpectedHash {
			result.ResultState = "rejected_before_write"
			if writeErr == nil {
				writeErr = errors.New("managed channel mutation was not reflected by readback")
			}
			result.Error = writeErr.Error()
			return result, writeErr
		}
		result.ResultState = "conflict_after_write"
		result.Error = "managed channel state differs from both expected and desired state"
		if writeErr != nil {
			result.Error += ": " + writeErr.Error()
		}
		return result, errors.New(result.Error)
	}

	result.ResultState = "ambiguous_after_write"
	result.Error = "managed channel mutation readback failed: " + readErr.Error()
	if writeErr != nil {
		result.Error = writeErr.Error() + "; " + result.Error
	}
	return result, errors.New(result.Error)
}

func normalizeManagedChannelState(
	state domain.TargetManagedChannelState,
	expectedChannelID int64,
) (domain.TargetManagedChannelState, error) {
	if state.ChannelID != expectedChannelID {
		return domain.TargetManagedChannelState{}, fmt.Errorf(
			"managed channel response id %d does not match requested channel %d", state.ChannelID, expectedChannelID)
	}
	state.Hash = strings.TrimSpace(state.Hash)
	if state.Hash == "" || len(state.Hash) > 128 {
		return domain.TargetManagedChannelState{}, errors.New("managed channel response omitted a bounded state hash")
	}
	if state.Status != channelStatusEnabled && state.Status != channelStatusManuallyDisabled {
		return domain.TargetManagedChannelState{}, fmt.Errorf("managed channel response status %d is not supported", state.Status)
	}
	state.Models = normalizeModels(state.Models)
	return state, nil
}

func managedChannelStateMatches(state domain.TargetManagedChannelState, models []string, status int) bool {
	return state.Status == status && strings.Join(state.Models, "\x00") == strings.Join(models, "\x00")
}

func rejectedManagedChannelMutation(
	result domain.TargetManagedChannelMutation,
	err error,
) (domain.TargetManagedChannelMutation, error) {
	result.ResultState = "rejected_before_write"
	result.Error = err.Error()
	return result, err
}
