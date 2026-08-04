package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/decimalnorm"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

const (
	optionContractV1  = "newapi-option-array-v1"
	channelContractV1 = "newapi-channel-page-v1"
	maxOptionEntries  = 100000
	maxOptionKeyBytes = 512
)

type ControlledAdapter interface {
	Capabilities(context.Context, domain.TargetSite) domain.TargetCapabilityMatrix
	PreviewPricingOption(context.Context, domain.TargetSite, string, map[string]string) (domain.TargetPricingOptionPreview, error)
	ApplyPricingOptionControlled(context.Context, domain.TargetSite, string, string, map[string]string, func() error) (domain.TargetMutationResult, error)
	PreviewChannelRouting(context.Context, domain.TargetSite, int64, domain.TargetChannelRoutingUpdate) (domain.TargetChannelPreview, error)
	ApplyChannelRoutingControlled(context.Context, domain.TargetSite, int64, string, domain.TargetChannelRoutingUpdate, func() error) (domain.TargetMutationResult, error)
	PreviewChannelStatus(context.Context, domain.TargetSite, int64, int) (domain.TargetChannelPreview, error)
	ApplyChannelStatusControlled(context.Context, domain.TargetSite, int64, string, int, func() error) (domain.TargetMutationResult, error)
}

func AsControlledAdapter(adapter Adapter) (ControlledAdapter, error) {
	value, ok := adapter.(ControlledAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support controlled management", adapter.Name())
	}
	return value, nil
}

func (f *Factory) ValidateNewAPIManagementCredential(ctx context.Context, target domain.TargetSite, payload domain.CredentialPayload) domain.TargetCapabilityMatrix {
	adapter := &NewAPI{client: f.client, credentials: f.credentials, credentialOverride: &payload, remoteWrites: f.remoteWrites}
	return adapter.Capabilities(ctx, target)
}

func (a *NewAPI) Capabilities(ctx context.Context, target domain.TargetSite) domain.TargetCapabilityMatrix {
	matrix := domain.TargetCapabilityMatrix{
		TargetSiteID:   target.ID,
		Adapter:        a.Name(),
		ReadOnly:       true,
		PricingOptions: make(map[string]domain.TargetOptionCapability, len(pricingOptionKeys)),
		GroupModelPricing: domain.TargetGroupModelPricingCapability{
			Read:    unavailableCapability("group-model pricing is not exposed by the target"),
			Preview: unavailableCapability("group-model pricing is not exposed by the target"),
			Write:   unavailableCapability("group-model pricing is not exposed by the target"),
		},
		ObservedAt: time.Now().UTC(),
	}
	for _, key := range pricingOptionKeys {
		matrix.PricingOptions[key] = domain.TargetOptionCapability{Key: key,
			Read:  unavailableCapability("option was not exposed by the target"),
			Write: unavailableCapability("option was not exposed by the target")}
	}

	snapshot, optionErr := a.Snapshot(ctx, target)
	if optionErr != nil {
		capability := capabilityFromError(optionErr)
		for _, key := range pricingOptionKeys {
			entry := matrix.PricingOptions[key]
			entry.Read = capability
			entry.Write = capability
			matrix.PricingOptions[key] = entry
		}
	} else {
		matrix.OptionContract = snapshot.Contract
		for _, key := range pricingOptionKeys {
			entry := matrix.PricingOptions[key]
			if option, exists := snapshot.PricingOptions[key]; exists {
				entry.Read = availableCapability()
				entry.Hash = option.Hash
				entry.Count = len(option.Values)
				entry.Write = writeCapability(target, snapshot.ContractKnown,
					"the option response uses an unknown write contract")
			}
			matrix.PricingOptions[key] = entry
		}
		if snapshot.GroupModelPricingSupported && snapshot.GroupModelPricing != nil {
			matrix.GroupModelPricing.Read = availableCapability()
			matrix.GroupModelPricing.Preview = availableCapability()
			matrix.GroupModelPricing.Write = writeCapability(target, true, "")
			matrix.GroupModelPricing.SchemaVersion = snapshot.GroupModelPricing.SchemaVersion
			matrix.GroupModelPricing.Revision = snapshot.GroupModelPricing.Revision
		} else if snapshot.GroupModelPricingError != "" {
			capability := unavailableCapability(snapshot.GroupModelPricingError)
			matrix.GroupModelPricing.Read = capability
			matrix.GroupModelPricing.Preview = capability
			matrix.GroupModelPricing.Write = capability
		}
	}

	if _, err := a.ListChannels(ctx, target); err != nil {
		matrix.Channels.Read = capabilityFromError(err)
		matrix.Channels.RoutingWrite = capabilityFromError(err)
		matrix.Channels.StatusWrite = capabilityFromError(err)
		matrix.Channels.BillableTest = capabilityFromError(err)
	} else {
		matrix.ChannelContract = channelContractV1
		matrix.Channels.Read = availableCapability()
		matrix.Channels.RoutingWrite = writeCapability(target, true, "")
		matrix.Channels.StatusWrite = writeCapability(target, true, "")
		matrix.Channels.BillableTest = writeCapability(target, true, "")
	}
	if !a.remoteWrites.Enabled() {
		blocked := domain.TargetCapability{Status: "read_only", Reason: remotewrite.DisabledMessage}
		for key, entry := range matrix.PricingOptions {
			entry.Write = blocked
			matrix.PricingOptions[key] = entry
		}
		matrix.GroupModelPricing.Write = blocked
		matrix.Channels.RoutingWrite = blocked
		matrix.Channels.StatusWrite = blocked
		matrix.Channels.BillableTest = blocked
		matrix.ReadOnly = true
		matrix.ReadOnlyReason = remotewrite.DisabledMessage
		return matrix
	}

	for _, entry := range matrix.PricingOptions {
		if entry.Write.Available {
			matrix.ReadOnly = false
			break
		}
	}
	if matrix.GroupModelPricing.Write.Available {
		matrix.ReadOnly = false
	}
	if matrix.Channels.RoutingWrite.Available || matrix.Channels.StatusWrite.Available {
		matrix.ReadOnly = false
	}
	if matrix.ReadOnly {
		switch {
		case !target.Enabled:
			matrix.ReadOnlyReason = "target is disabled; reauthorization does not override an administrator disable"
		case strings.EqualFold(strings.TrimSpace(target.WriteMode), "read_only") || strings.EqualFold(strings.TrimSpace(target.WriteMode), "readonly"):
			matrix.ReadOnlyReason = "target write mode is read-only"
		case optionErr == nil && !snapshot.ContractKnown:
			matrix.ReadOnlyReason = "target option contract is readable but not a recognized write contract"
		default:
			matrix.ReadOnlyReason = "no verified write capability is available"
		}
	}
	return matrix
}

func availableCapability() domain.TargetCapability {
	return domain.TargetCapability{Status: "available", Available: true}
}

func unavailableCapability(reason string) domain.TargetCapability {
	return domain.TargetCapability{Status: "unavailable", Reason: reason}
}

func writeCapability(target domain.TargetSite, known bool, unknownReason string) domain.TargetCapability {
	if !target.Enabled {
		return domain.TargetCapability{Status: "read_only", Reason: "target is disabled"}
	}
	mode := strings.ToLower(strings.TrimSpace(target.WriteMode))
	if mode == "read_only" || mode == "readonly" {
		return domain.TargetCapability{Status: "read_only", Reason: "target write mode is read-only"}
	}
	if !known {
		return domain.TargetCapability{Status: "unknown_contract", Reason: unknownReason}
	}
	return availableCapability()
}

func capabilityFromError(err error) domain.TargetCapability {
	status := RemoteHTTPStatus(err)
	switch status {
	case http.StatusUnauthorized:
		return domain.TargetCapability{Status: "unauthorized", HTTPStatus: status, Reason: "management credential was not accepted"}
	case http.StatusForbidden:
		return domain.TargetCapability{Status: "forbidden", HTTPStatus: status, Reason: "management credential lacks this permission"}
	default:
		return domain.TargetCapability{Status: "unavailable", HTTPStatus: status, Reason: sanitizeTargetError(err)}
	}
}

func ValidatePricingOptionValues(values map[string]string) error {
	if len(values) == 0 {
		return errors.New("pricing option map cannot be empty")
	}
	_, err := normalizePricingOptionMutation(nil, values)
	return err
}

// normalizePricingOptionMutation applies current validation only to entries
// introduced or changed by this mutation. Exact historical entries are kept
// compatible so replacing a whole NewAPI option does not make unrelated legacy
// data an administrator blocker.
func normalizePricingOptionMutation(current, desired map[string]string) (map[string]string, error) {
	if desired == nil {
		return nil, errors.New("pricing option values are required")
	}
	if len(desired) > maxOptionEntries && len(desired) > len(current) {
		return nil, fmt.Errorf("pricing option has %d entries; limit is %d", len(desired), maxOptionEntries)
	}
	normalized := make(map[string]string, len(desired))
	for key, raw := range desired {
		if previous, exists := current[key]; exists && equalDecimal(previous, raw) {
			normalized[key] = previous
			continue
		}
		if strings.TrimSpace(key) != key || key == "" || !utf8.ValidString(key) || len(key) > maxOptionKeyBytes ||
			strings.ContainsAny(key, "\x00\r\n") {
			return nil, fmt.Errorf("pricing option key %q is invalid or too long", key)
		}
		value := strings.TrimSpace(raw)
		if value == "" || len(value) > 128 {
			return nil, fmt.Errorf("pricing option value for %q is empty or too long", key)
		}
		parsed, err := decimal.NewFromString(value)
		if err != nil || parsed.IsNegative() {
			return nil, fmt.Errorf("pricing option value for %q must be a finite non-negative decimal", key)
		}
		normalized[key] = decimalnorm.Functional(parsed).String()
	}
	return normalized, nil
}

func (a *NewAPI) PreviewPricingOption(ctx context.Context, target domain.TargetSite, key string, desired map[string]string) (domain.TargetPricingOptionPreview, error) {
	canonical, ok := CanonicalPricingOptionKey(key)
	if !ok {
		return domain.TargetPricingOptionPreview{}, fmt.Errorf("pricing option %q is not allowlisted", key)
	}
	if len(desired) == 0 {
		return domain.TargetPricingOptionPreview{}, errors.New("pricing option map cannot be empty")
	}
	snapshot, err := a.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetPricingOptionPreview{}, err
	}
	current, exists := snapshot.PricingOptions[canonical]
	if !exists {
		return domain.TargetPricingOptionPreview{}, fmt.Errorf("target does not expose pricing option %s", canonical)
	}
	desired, err = normalizePricingOptionMutation(current.Values, desired)
	if err != nil {
		return domain.TargetPricingOptionPreview{}, err
	}
	diff, deletions := diffStringMaps(current.Values, desired)
	preview := domain.TargetPricingOptionPreview{
		TargetSiteID: target.ID, Option: canonical, ExpectedHash: current.Hash, DesiredHash: HashDecimalMap(desired),
		Current: cloneStringMap(current.Values), Desired: cloneStringMap(desired), Diff: diff, Deletions: deletions,
		NoChange: len(diff) == 0, ObservedAt: snapshot.ObservedAt,
	}
	preview.PreviewHash = hashControlPreview("pricing_option", target.ID, 0, canonical, preview.ExpectedHash, preview.DesiredHash)
	return preview, nil
}

func (a *NewAPI) ApplyPricingOptionControlled(ctx context.Context, target domain.TargetSite, key, expectedHash string,
	desired map[string]string, beforeWrite func() error) (domain.TargetMutationResult, error) {
	if err := a.remoteWrites.Check("controlled target pricing apply"); err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	if capability := writeCapability(target, true, ""); !capability.Available {
		return rejectedMutation(expectedHash, "", errors.New(capability.Reason))
	}
	canonical, ok := CanonicalPricingOptionKey(key)
	if !ok {
		return rejectedMutation(expectedHash, "", fmt.Errorf("pricing option %q is not allowlisted", key))
	}
	if len(desired) == 0 {
		return rejectedMutation(expectedHash, "", errors.New("pricing option map cannot be empty"))
	}
	current, err := a.Snapshot(ctx, target)
	if err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	if !current.ContractKnown || current.Contract != optionContractV1 {
		return rejectedMutation(expectedHash, "", errors.New("target option contract is unknown; writes are disabled"))
	}
	currentOption, exists := current.PricingOptions[canonical]
	if !exists {
		return rejectedMutation(expectedHash, "", fmt.Errorf("target does not expose pricing option %s", canonical))
	}
	desired, err = normalizePricingOptionMutation(currentOption.Values, desired)
	if err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	desiredHash := HashDecimalMap(desired)
	result := domain.TargetMutationResult{ExpectedHash: expectedHash, DesiredHash: desiredHash}
	if currentOption.Hash == desiredHash {
		copy := currentOption
		result.ResultState = "verified_success"
		result.ReadbackHash = currentOption.Hash
		result.PricingOption = &copy
		return result, nil
	}
	if strings.TrimSpace(expectedHash) == "" || currentOption.Hash != expectedHash {
		result.ResultState = "conflict_before_write"
		result.ReadbackHash = currentOption.Hash
		result.Error = "pricing option changed since preview"
		return result, errors.New(result.Error)
	}
	if beforeWrite == nil {
		return rejectedMutation(expectedHash, desiredHash, errors.New("a fencing guard is required before mutation"))
	}
	if err := beforeWrite(); err != nil {
		return rejectedMutation(expectedHash, desiredHash, err)
	}

	writeErr := a.putOption(ctx, target, canonical, currentOption.Values, desired)
	readback, readErr := a.Snapshot(ctx, target)
	if readErr == nil {
		if option, exists := readback.PricingOptions[canonical]; exists {
			result.ReadbackHash = option.Hash
			if option.Hash == desiredHash {
				evidence, evidenceErr := a.verifyEffectivePricing(ctx, target, canonical, currentOption.Values, desired)
				if evidenceErr == nil {
					copy := option
					result.ResultState = "verified_success"
					result.PricingOption = &copy
					result.EffectivePricing = &evidence
					return result, nil
				}
				return a.compensatePricingOption(ctx, target, canonical, currentOption, desired, result,
					fmt.Errorf("effective pricing verification failed: %w", evidenceErr))
			}
			if writeErr != nil && IsDefinitiveRemoteRejection(writeErr) && option.Hash == currentOption.Hash {
				result.ResultState = "rejected_before_write"
				result.Error = sanitizeTargetError(writeErr)
				return result, writeErr
			}
		}
	}
	result.ResultState = "ambiguous_after_write"
	if writeErr != nil {
		result.Error = sanitizeTargetError(writeErr)
	} else if readErr != nil {
		result.Error = "write returned success but readback failed: " + sanitizeTargetError(readErr)
	} else {
		result.Error = "write readback did not match the requested option"
	}
	return result, errors.New(result.Error)
}

func (a *NewAPI) verifyEffectivePricing(ctx context.Context, target domain.TargetSite, option string,
	previous, desired map[string]string) (domain.TargetEffectivePricingEvidence, error) {
	body, err := a.rawAuthorizedRequest(ctx, target, http.MethodGet, "/api/pricing", nil)
	if err != nil {
		return domain.TargetEffectivePricingEvidence{}, err
	}
	var envelope struct {
		Success    *bool           `json:"success"`
		Message    string          `json:"message"`
		Data       json.RawMessage `json:"data"`
		GroupRatio json.RawMessage `json:"group_ratio"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Success == nil || !*envelope.Success || len(envelope.Data) == 0 {
		return domain.TargetEffectivePricingEvidence{}, errors.New("pricing endpoint did not return a recognized successful contract")
	}
	sum := sha256.Sum256(body)
	evidence := domain.TargetEffectivePricingEvidence{Status: "observed", PricingHash: hex.EncodeToString(sum[:])}
	if option == "GroupRatio" {
		observed, err := parseRatioJSON(envelope.GroupRatio)
		if err != nil {
			return domain.TargetEffectivePricingEvidence{}, errors.New("effective GroupRatio cannot be decoded")
		}
		evidence.ObservedGroups = len(observed)
		for group, actual := range observed {
			expected, exists := desired[group]
			if !exists {
				if _, deleted := previous[group]; deleted {
					return domain.TargetEffectivePricingEvidence{}, fmt.Errorf("effective GroupRatio still contains deleted group %q", group)
				}
				evidence.Warnings = append(evidence.Warnings, fmt.Sprintf("生效价格接口额外返回了原始配置中没有的分组 %q", group))
				continue
			}
			if !equalDecimal(actual, expected) {
				return domain.TargetEffectivePricingEvidence{}, fmt.Errorf("effective GroupRatio for %q is %s, raw option readback is %s", group, actual, expected)
			}
			evidence.MatchedGroups++
		}
		changed := changedMapKeys(previous, desired)
		for _, group := range changed {
			expected, exists := desired[group]
			actual, observedExists := observed[group]
			if !exists {
				if observedExists {
					return domain.TargetEffectivePricingEvidence{}, fmt.Errorf("effective GroupRatio still contains deleted group %q", group)
				}
				evidence.ChangedGroupsVerified++
				continue
			}
			if observedExists {
				if !equalDecimal(actual, expected) {
					return domain.TargetEffectivePricingEvidence{}, fmt.Errorf("effective GroupRatio for changed group %q is %s, want %s", group, actual, expected)
				}
				evidence.ChangedGroupsVerified++
				continue
			}
			evidence.ChangedGroupsRawOnly = append(evidence.ChangedGroupsRawOnly, group)
		}
		if HashDecimalMap(observed) == HashDecimalMap(desired) {
			evidence.Status = "verified_exact"
			evidence.Exact = true
			return evidence, nil
		}
		evidence.Status = "verified_effective_subset"
		if len(evidence.ChangedGroupsRawOnly) > 0 {
			evidence.Warnings = append(evidence.Warnings,
				"部分已修改分组当前没有可用模型，因此生效价格接口未展示；原始配置写后读回已验证")
		} else {
			evidence.Warnings = append(evidence.Warnings,
				"生效价格接口只返回当前可用分组；其返回的分组均与原始配置写后读回一致")
		}
		return evidence, nil
	}
	evidence.Warnings = []string{"the pricing endpoint is healthy; per-model locked or built-in values may override this option and are reported by the target catalog"}
	return evidence, nil
}

func equalDecimal(left, right string) bool {
	return decimalnorm.Equal(left, right)
}

func changedMapKeys(previous, desired map[string]string) []string {
	keys := make([]string, 0)
	seen := make(map[string]struct{}, len(previous)+len(desired))
	for key := range previous {
		seen[key] = struct{}{}
	}
	for key := range desired {
		seen[key] = struct{}{}
	}
	for key := range seen {
		before, beforeExists := previous[key]
		after, afterExists := desired[key]
		if beforeExists != afterExists || (beforeExists && !equalDecimal(before, after)) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func (a *NewAPI) compensatePricingOption(ctx context.Context, target domain.TargetSite, key string,
	original domain.TargetPriceOption, desired map[string]string, result domain.TargetMutationResult, cause error) (domain.TargetMutationResult, error) {
	current, err := a.Snapshot(ctx, target)
	if err != nil {
		result.ResultState = "ambiguous_after_write"
		result.Error = cause.Error() + "; current state could not be inspected for conditional compensation"
		return result, errors.New(result.Error)
	}
	option, exists := current.PricingOptions[key]
	if !exists {
		result.ResultState = "ambiguous_after_write"
		result.Error = cause.Error() + "; option disappeared before conditional compensation"
		return result, errors.New(result.Error)
	}
	result.ReadbackHash = option.Hash
	if option.Hash == original.Hash {
		result.ResultState = "rolled_back_verified"
		result.Error = cause.Error()
		return result, errors.New(result.Error)
	}
	if option.Hash != HashDecimalMap(desired) {
		result.ResultState = "ambiguous_after_write"
		result.Error = cause.Error() + "; another option change was detected, so compensation was not attempted"
		return result, errors.New(result.Error)
	}
	if err := a.putOption(ctx, target, key, option.Values, original.Values); err != nil {
		result.ResultState = "compensation_failed"
		result.Error = cause.Error() + "; conditional compensation failed: " + sanitizeTargetError(err)
		return result, errors.New(result.Error)
	}
	rolledBack, err := a.Snapshot(ctx, target)
	if err != nil || rolledBack.PricingOptions[key].Hash != original.Hash {
		result.ResultState = "compensation_failed"
		result.Error = cause.Error() + "; conditional compensation readback failed"
		return result, errors.New(result.Error)
	}
	result.ResultState = "rolled_back_verified"
	result.ReadbackHash = original.Hash
	result.Error = cause.Error()
	return result, errors.New(result.Error)
}

func (a *NewAPI) PreviewChannelRouting(ctx context.Context, target domain.TargetSite, channelID int64,
	update domain.TargetChannelRoutingUpdate) (domain.TargetChannelPreview, error) {
	if _, err := a.probeChannelContract(ctx, target); err != nil {
		return domain.TargetChannelPreview{}, err
	}
	current, err := a.GetChannel(ctx, target, channelID)
	if err != nil {
		return domain.TargetChannelPreview{}, err
	}
	desired, diff, err := applyRoutingUpdate(current, update)
	if err != nil {
		return domain.TargetChannelPreview{}, err
	}
	preview := domain.TargetChannelPreview{TargetSiteID: target.ID, ChannelID: channelID, Action: "routing",
		ExpectedHash: current.ConfigHash, DesiredHash: desired.ConfigHash, Before: current, After: desired, Diff: diff,
		NoChange: len(diff) == 0, ObservedAt: time.Now().UTC()}
	preview.PreviewHash = hashControlPreview("channel_routing", target.ID, channelID, "", preview.ExpectedHash, preview.DesiredHash)
	return preview, nil
}

func (a *NewAPI) ApplyChannelRoutingControlled(ctx context.Context, target domain.TargetSite, channelID int64,
	expectedHash string, update domain.TargetChannelRoutingUpdate, beforeWrite func() error) (domain.TargetMutationResult, error) {
	if err := a.remoteWrites.Check("controlled target channel routing apply"); err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	if capability := writeCapability(target, true, ""); !capability.Available {
		return rejectedMutation(expectedHash, "", errors.New(capability.Reason))
	}
	if _, err := a.probeChannelContract(ctx, target); err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	current, err := a.GetChannel(ctx, target, channelID)
	if err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	desired, _, err := applyRoutingUpdate(current, update)
	if err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	result := domain.TargetMutationResult{ExpectedHash: expectedHash, DesiredHash: desired.ConfigHash}
	if current.ConfigHash == desired.ConfigHash {
		result.ResultState = "verified_success"
		result.ReadbackHash = current.ConfigHash
		result.Channel = &current
		return result, nil
	}
	if expectedHash == "" || current.ConfigHash != expectedHash {
		result.ResultState = "conflict_before_write"
		result.ReadbackHash = current.ConfigHash
		result.Error = "channel changed since preview"
		return result, errors.New(result.Error)
	}
	if beforeWrite == nil {
		return rejectedMutation(expectedHash, desired.ConfigHash, errors.New("a fencing guard is required before mutation"))
	}
	if err := beforeWrite(); err != nil {
		return rejectedMutation(expectedHash, desired.ConfigHash, err)
	}
	payload := channelRoutingPayload(desired)
	_, writeErr := a.managementRequest(ctx, target, http.MethodPut, "/api/channel/", nil, payload)
	readback, readErr := a.GetChannel(ctx, target, channelID)
	if readErr == nil {
		result.ReadbackHash = readback.ConfigHash
		if readback.ConfigHash == desired.ConfigHash {
			result.ResultState = "verified_success"
			result.Channel = &readback
			return result, nil
		}
		if writeErr != nil && IsDefinitiveRemoteRejection(writeErr) && readback.ConfigHash == current.ConfigHash {
			result.ResultState = "rejected_before_write"
			result.Error = sanitizeTargetError(writeErr)
			return result, writeErr
		}
	}
	result.ResultState = "ambiguous_after_write"
	result.Error = mutationReadbackError(writeErr, readErr, "channel routing")
	return result, errors.New(result.Error)
}

func (a *NewAPI) PreviewChannelStatus(ctx context.Context, target domain.TargetSite, channelID int64,
	status int) (domain.TargetChannelPreview, error) {
	if _, err := a.probeChannelContract(ctx, target); err != nil {
		return domain.TargetChannelPreview{}, err
	}
	if status != channelStatusEnabled && status != channelStatusManuallyDisabled {
		return domain.TargetChannelPreview{}, errors.New("channel status must be enabled (1) or manually disabled (2)")
	}
	current, err := a.GetChannel(ctx, target, channelID)
	if err != nil {
		return domain.TargetChannelPreview{}, err
	}
	desired := current
	desired.Status = status
	desired.ConfigHash = HashTargetChannel(desired)
	diff := make([]domain.TargetFieldDiff, 0, 1)
	if current.Status != status {
		diff = append(diff, domain.TargetFieldDiff{Field: "status", Before: strconv.Itoa(current.Status), After: strconv.Itoa(status), Kind: "update"})
	}
	preview := domain.TargetChannelPreview{TargetSiteID: target.ID, ChannelID: channelID, Action: "status",
		ExpectedHash: current.ConfigHash, DesiredHash: desired.ConfigHash, Before: current, After: desired, Diff: diff,
		NoChange: len(diff) == 0, ObservedAt: time.Now().UTC()}
	preview.PreviewHash = hashControlPreview("channel_status", target.ID, channelID, "", preview.ExpectedHash, preview.DesiredHash)
	return preview, nil
}

func (a *NewAPI) ApplyChannelStatusControlled(ctx context.Context, target domain.TargetSite, channelID int64,
	expectedHash string, status int, beforeWrite func() error) (domain.TargetMutationResult, error) {
	if err := a.remoteWrites.Check("controlled target channel status apply"); err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	if capability := writeCapability(target, true, ""); !capability.Available {
		return rejectedMutation(expectedHash, "", errors.New(capability.Reason))
	}
	if _, err := a.probeChannelContract(ctx, target); err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	if status != channelStatusEnabled && status != channelStatusManuallyDisabled {
		return rejectedMutation(expectedHash, "", errors.New("channel status must be enabled (1) or manually disabled (2)"))
	}
	current, err := a.GetChannel(ctx, target, channelID)
	if err != nil {
		return rejectedMutation(expectedHash, "", err)
	}
	desired := current
	desired.Status = status
	desired.ConfigHash = HashTargetChannel(desired)
	result := domain.TargetMutationResult{ExpectedHash: expectedHash, DesiredHash: desired.ConfigHash}
	if current.ConfigHash == desired.ConfigHash {
		result.ResultState = "verified_success"
		result.ReadbackHash = current.ConfigHash
		result.Channel = &current
		return result, nil
	}
	if expectedHash == "" || current.ConfigHash != expectedHash {
		result.ResultState = "conflict_before_write"
		result.ReadbackHash = current.ConfigHash
		result.Error = "channel changed since preview"
		return result, errors.New(result.Error)
	}
	if beforeWrite == nil {
		return rejectedMutation(expectedHash, desired.ConfigHash, errors.New("a fencing guard is required before mutation"))
	}
	if err := beforeWrite(); err != nil {
		return rejectedMutation(expectedHash, desired.ConfigHash, err)
	}
	path := "/api/channel/" + strconv.FormatInt(channelID, 10) + "/status"
	_, writeErr := a.managementRequest(ctx, target, http.MethodPost, path, nil, map[string]int{"status": status})
	readback, readErr := a.GetChannel(ctx, target, channelID)
	if readErr == nil {
		result.ReadbackHash = readback.ConfigHash
		if readback.ConfigHash == desired.ConfigHash {
			result.ResultState = "verified_success"
			result.Channel = &readback
			return result, nil
		}
		if writeErr != nil && IsDefinitiveRemoteRejection(writeErr) && readback.ConfigHash == current.ConfigHash {
			result.ResultState = "rejected_before_write"
			result.Error = sanitizeTargetError(writeErr)
			return result, writeErr
		}
	}
	result.ResultState = "ambiguous_after_write"
	result.Error = mutationReadbackError(writeErr, readErr, "channel status")
	return result, errors.New(result.Error)
}

func (a *NewAPI) probeChannelContract(ctx context.Context, target domain.TargetSite) (string, error) {
	if _, err := a.ListChannels(ctx, target); err != nil {
		return "", err
	}
	return channelContractV1, nil
}

func applyRoutingUpdate(current domain.TargetChannel, update domain.TargetChannelRoutingUpdate) (domain.TargetChannel, []domain.TargetFieldDiff, error) {
	if update.Group == nil && update.Priority == nil && update.Weight == nil && update.AutoBan == nil {
		return domain.TargetChannel{}, nil, errors.New("at least one routing field is required")
	}
	desired := current
	diff := make([]domain.TargetFieldDiff, 0, 4)
	if update.Group != nil {
		value := strings.TrimSpace(*update.Group)
		if value == "" || len(value) > 64 {
			return domain.TargetChannel{}, nil, errors.New("channel group must be 1 to 64 bytes")
		}
		if value != current.Group {
			diff = append(diff, domain.TargetFieldDiff{Field: "group", Before: current.Group, After: value, Kind: "update"})
			desired.Group = value
		}
	}
	if update.Priority != nil && *update.Priority != current.Priority {
		diff = append(diff, domain.TargetFieldDiff{Field: "priority", Before: strconv.FormatInt(current.Priority, 10), After: strconv.FormatInt(*update.Priority, 10), Kind: "update"})
		desired.Priority = *update.Priority
	}
	if update.Weight != nil && *update.Weight != current.Weight {
		diff = append(diff, domain.TargetFieldDiff{Field: "weight", Before: strconv.FormatUint(uint64(current.Weight), 10), After: strconv.FormatUint(uint64(*update.Weight), 10), Kind: "update"})
		desired.Weight = *update.Weight
	}
	if update.AutoBan != nil && *update.AutoBan != current.AutoBan {
		diff = append(diff, domain.TargetFieldDiff{Field: "auto_ban", Before: strconv.FormatBool(current.AutoBan), After: strconv.FormatBool(*update.AutoBan), Kind: "update"})
		desired.AutoBan = *update.AutoBan
	}
	desired.ConfigHash = HashTargetChannel(desired)
	return desired, diff, nil
}

func channelRoutingPayload(channel domain.TargetChannel) map[string]any {
	autoBan := 0
	if channel.AutoBan {
		autoBan = 1
	}
	return map[string]any{
		"id": channel.ID, "type": channel.Type, "name": channel.Name, "models": strings.Join(normalizeModels(channel.Models), ","),
		"group": channel.Group, "priority": channel.Priority, "weight": channel.Weight, "auto_ban": autoBan,
		"model_mapping": channel.ModelMapping, "tag": channel.Tag, "remark": channel.Remark,
	}
}

func (a *NewAPI) rawAuthorizedRequest(ctx context.Context, target domain.TargetSite, method, path string, payload any) ([]byte, error) {
	return a.rawAuthorizedRequestMode(ctx, target, method, path, payload, false)
}

func (a *NewAPI) rawAuthorizedSemanticReadRequest(ctx context.Context, target domain.TargetSite, method, path string, payload any) ([]byte, error) {
	return a.rawAuthorizedRequestMode(ctx, target, method, path, payload, true)
}

func (a *NewAPI) rawAuthorizedRequestMode(ctx context.Context, target domain.TargetSite, method, path string, payload any, semanticRead bool) ([]byte, error) {
	if !semanticRead {
		if err := a.remoteWrites.CheckRequest(ctx, method, "target authorized mutation"); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	client, err := netguard.SameOriginClient(a.client, base)
	if err != nil {
		return nil, err
	}
	var bodyBytes []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyBytes = encoded
	}
	requestContext, cancel := withTimeout(ctx)
	defer cancel()
	requestURL := base + path
	if semanticRead {
		requestContext, err = remotewrite.WithReadOnlyRequest(requestContext, method, requestURL, bodyBytes)
		if err != nil {
			return nil, err
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var body io.Reader
		if payload != nil {
			body = strings.NewReader(string(bodyBytes))
		}
		req, err := http.NewRequestWithContext(requestContext, method, requestURL, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := a.authorize(ctx, target, req); err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &RemoteError{Operation: method + " " + path, Err: err}
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		retryAfter := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if method != http.MethodGet && !semanticRead {
				a.invalidateTargetReads(target)
			}
			return responseBody, nil
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == attempts-1 {
			return nil, remoteHTTPError(method+" "+path, resp.StatusCode, responseBody)
		}
		if err := waitTargetReadRetry(requestContext, retryAfter, attempt); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("target read retry exhausted")
}

func diffStringMaps(before, after map[string]string) ([]domain.TargetFieldDiff, []string) {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range after {
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	diff := make([]domain.TargetFieldDiff, 0)
	deletions := make([]string, 0)
	for _, key := range keys {
		oldValue, oldExists := before[key]
		newValue, newExists := after[key]
		switch {
		case oldExists && !newExists:
			diff = append(diff, domain.TargetFieldDiff{Field: key, Before: oldValue, Kind: "delete"})
			deletions = append(deletions, key)
		case !oldExists && newExists:
			diff = append(diff, domain.TargetFieldDiff{Field: key, After: newValue, Kind: "add"})
		case oldValue != newValue:
			diff = append(diff, domain.TargetFieldDiff{Field: key, Before: oldValue, After: newValue, Kind: "update"})
		}
	}
	return diff, deletions
}

func hashControlPreview(action string, targetID, channelID int64, key, expected, desired string) string {
	value := strings.Join([]string{action, strconv.FormatInt(targetID, 10), strconv.FormatInt(channelID, 10), key, expected, desired}, "\n")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func rejectedMutation(expected, desired string, err error) (domain.TargetMutationResult, error) {
	result := domain.TargetMutationResult{ResultState: "rejected_before_write", ExpectedHash: expected, DesiredHash: desired,
		Error: sanitizeTargetError(err)}
	return result, err
}

func mutationReadbackError(writeErr, readErr error, kind string) string {
	if writeErr != nil {
		return sanitizeTargetError(writeErr)
	}
	if readErr != nil {
		return fmt.Sprintf("%s write returned success but readback failed: %s", kind, sanitizeTargetError(readErr))
	}
	return kind + " readback differs from the requested state"
}

func sanitizeTargetError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " ")
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
