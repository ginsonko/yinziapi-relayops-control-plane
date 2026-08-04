package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const (
	groupModelPricingSchemaVersion = 1
	maxGroupModelPricingGroups     = 2048
	maxGroupModelPricingEntries    = 50000
	maxGroupModelPricingNameBytes  = 512
	maxGroupModelPricingSource     = 2048
	maxGroupModelPricingRevision   = 512
	maxFixedDurationSeconds        = 3600
	groupModelPricingWriteAttempts = 3
	groupModelPricingRetryDelay    = 50 * time.Millisecond
)

type groupModelPricingWireContract struct {
	SchemaVersion int                                                `json:"schema_version"`
	Revision      string                                             `json:"revision,omitempty"`
	Groups        map[string]map[string]groupModelPricingWireSetting `json:"groups"`
}

type groupModelPricingWireSetting struct {
	BillingMode            string                        `json:"billing_mode"`
	ModelPrice             *json.Number                  `json:"model_price,omitempty"`
	ModelRatio             *json.Number                  `json:"model_ratio,omitempty"`
	CompletionRatio        *json.Number                  `json:"completion_ratio,omitempty"`
	CacheRatio             *json.Number                  `json:"cache_ratio,omitempty"`
	CacheCreationRatio     *json.Number                  `json:"cache_creation_ratio,omitempty"`
	CacheCreation5mRatio   *json.Number                  `json:"cache_creation_5m_ratio,omitempty"`
	CacheCreation1hRatio   *json.Number                  `json:"cache_creation_1h_ratio,omitempty"`
	ImageRatio             *json.Number                  `json:"image_ratio,omitempty"`
	AudioRatio             *json.Number                  `json:"audio_ratio,omitempty"`
	AudioCompletionRatio   *json.Number                  `json:"audio_completion_ratio,omitempty"`
	BillingExpr            string                        `json:"billing_expr,omitempty"`
	TaskProfile            *domain.GroupModelTaskProfile `json:"task_profile,omitempty"`
	Source                 string                        `json:"source,omitempty"`
	SourceRevision         string                        `json:"source_revision,omitempty"`
	SourceObservedAtUnixMS int64                         `json:"source_observed_at_unix_ms,omitempty"`
}

type groupModelPricingEnvelope struct {
	Success bool                          `json:"success"`
	Code    string                        `json:"code"`
	Message string                        `json:"message"`
	Data    groupModelPricingResponseData `json:"data"`
}

type groupModelPricingResponseData struct {
	Supported               bool                          `json:"supported"`
	SchemaVersion           int                           `json:"schema_version"`
	Revision                string                        `json:"revision"`
	CurrentRevision         string                        `json:"current_revision"`
	ExpectedRevisionMatches bool                          `json:"expected_revision_matches"`
	Changed                 bool                          `json:"changed"`
	Contract                groupModelPricingWireContract `json:"contract"`
}

type groupModelPricingWriteGuardError struct {
	err error
}

func (e *groupModelPricingWriteGuardError) Error() string { return e.err.Error() }
func (e *groupModelPricingWriteGuardError) Unwrap() error { return e.err }

func IsGroupModelPricingUnsupported(err error) bool {
	status := RemoteHTTPStatus(err)
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed
}

func (a *NewAPI) ReadGroupModelPricing(ctx context.Context, target domain.TargetSite) (domain.GroupModelPricingContract, error) {
	load := func(loadCtx context.Context) (domain.GroupModelPricingContract, error) {
		return a.readGroupModelPricingUncached(loadCtx, target)
	}
	if a.contracts == nil {
		return load(ctx)
	}
	return a.contracts.read(ctx, targetCatalogCacheKey(target), load)
}

func (a *NewAPI) readGroupModelPricingUncached(ctx context.Context, target domain.TargetSite) (domain.GroupModelPricingContract, error) {
	body, err := a.rawAuthorizedRequest(ctx, target, http.MethodGet, "/api/option/group-model-pricing", nil)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	data, err := decodeGroupModelPricingResponse(body, "read group-model pricing")
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if !data.Supported || data.SchemaVersion != groupModelPricingSchemaVersion {
		return domain.GroupModelPricingContract{}, fmt.Errorf("target group-model pricing schema %d is not supported", data.SchemaVersion)
	}
	contract, err := contractFromWire(data.Contract)
	if err != nil {
		return domain.GroupModelPricingContract{}, fmt.Errorf("decode target group-model pricing contract: %w", err)
	}
	if contract.Revision == "" {
		contract.Revision = strings.TrimSpace(data.Revision)
	}
	if contract.Revision == "" || data.Revision != "" && contract.Revision != data.Revision {
		return domain.GroupModelPricingContract{}, errors.New("target group-model pricing response has inconsistent revision evidence")
	}
	return contract, nil
}

func (a *NewAPI) PreviewGroupModelPricing(ctx context.Context, target domain.TargetSite, expectedRevision string,
	desired domain.GroupModelPricingContract) (domain.TargetGroupModelPricingPreview, error) {
	wire, normalized, err := contractToWire(desired)
	if err != nil {
		return domain.TargetGroupModelPricingPreview{}, err
	}
	current, err := a.ReadGroupModelPricing(ctx, target)
	if err != nil {
		return domain.TargetGroupModelPricingPreview{}, err
	}
	body, err := a.rawAuthorizedSemanticReadRequest(ctx, target, http.MethodPost, "/api/option/group-model-pricing/preview", map[string]any{
		"expected_revision": strings.TrimSpace(expectedRevision),
		"contract":          wire,
	})
	if err != nil {
		return domain.TargetGroupModelPricingPreview{}, err
	}
	data, err := decodeGroupModelPricingResponse(body, "preview group-model pricing")
	if err != nil {
		return domain.TargetGroupModelPricingPreview{}, err
	}
	serverDesired, err := contractFromWire(data.Contract)
	if err != nil {
		return domain.TargetGroupModelPricingPreview{}, fmt.Errorf("decode previewed group-model pricing contract: %w", err)
	}
	if serverDesired.Revision == "" {
		serverDesired.Revision = data.Revision
	}
	if serverDesired.Revision == "" {
		return domain.TargetGroupModelPricingPreview{}, errors.New("group-model pricing preview returned no semantic revision")
	}
	// The server is the revision authority. Local normalization still proves the
	// outgoing contract contains no unsupported or non-decimal values.
	_ = normalized
	diff := diffGroupModelPricingContracts(current, serverDesired)
	preview := domain.TargetGroupModelPricingPreview{
		TargetSiteID: target.ID, ExpectedRevision: strings.TrimSpace(expectedRevision), CurrentRevision: current.Revision,
		DesiredRevision: serverDesired.Revision, ExpectedRevisionMatches: data.ExpectedRevisionMatches,
		NoChange: len(diff) == 0, Current: current, Desired: serverDesired, Diff: diff, ObservedAt: time.Now().UTC(),
	}
	preview.PreviewHash = hashControlPreview("group_model_pricing", target.ID, 0, "contract",
		preview.CurrentRevision, preview.DesiredRevision)
	return preview, nil
}

func (a *NewAPI) ApplyGroupModelPricingControlled(ctx context.Context, target domain.TargetSite, expectedRevision string,
	desired domain.GroupModelPricingContract, beforeWrite func() error) (domain.TargetMutationResult, error) {
	if err := a.remoteWrites.Check("controlled target group-model pricing apply"); err != nil {
		return rejectedMutation(expectedRevision, desired.Revision, err)
	}
	if capability := writeCapability(target, true, ""); !capability.Available {
		return rejectedMutation(expectedRevision, desired.Revision, errors.New(capability.Reason))
	}
	preview, err := a.PreviewGroupModelPricing(WithFreshRead(ctx), target, expectedRevision, desired)
	if err != nil {
		return rejectedMutation(expectedRevision, desired.Revision, err)
	}
	result := domain.TargetMutationResult{ExpectedHash: expectedRevision, DesiredHash: preview.DesiredRevision}
	if preview.NoChange {
		contract := preview.Current
		result.ResultState = "verified_success"
		result.ReadbackHash = contract.Revision
		result.GroupModelPricing = &contract
		return result, nil
	}
	if expectedRevision == "" || !preview.ExpectedRevisionMatches || preview.CurrentRevision != expectedRevision {
		result.ResultState = "conflict_before_write"
		result.ReadbackHash = preview.CurrentRevision
		result.Error = "group-model pricing changed since preview"
		return result, errors.New(result.Error)
	}
	if beforeWrite == nil {
		return rejectedMutation(expectedRevision, preview.DesiredRevision, errors.New("a fencing guard is required before mutation"))
	}

	writeContract, writeErr := a.updateGroupModelPricingControlled(ctx, target, expectedRevision, preview.Desired, beforeWrite)
	var guardErr *groupModelPricingWriteGuardError
	if errors.As(writeErr, &guardErr) {
		return rejectedMutation(expectedRevision, preview.DesiredRevision, guardErr.err)
	}
	readback, readErr := a.ReadGroupModelPricing(ctx, target)
	if readErr == nil {
		result.ReadbackHash = readback.Revision
		if readback.Revision == preview.DesiredRevision {
			result.ResultState = "verified_success"
			result.GroupModelPricing = &readback
			return result, nil
		}
		if writeErr != nil && IsDefinitiveRemoteRejection(writeErr) && readback.Revision == preview.CurrentRevision {
			result.ResultState = "rejected_before_write"
			result.Error = sanitizeTargetError(writeErr)
			return result, writeErr
		}
	}
	if writeErr == nil && writeContract.Revision == preview.DesiredRevision && readErr != nil {
		result.ResultState = "ambiguous_after_write"
		result.Error = "group-model pricing write was accepted but independent readback failed: " + sanitizeTargetError(readErr)
		return result, errors.New(result.Error)
	}
	result.ResultState = "ambiguous_after_write"
	result.Error = mutationReadbackError(writeErr, readErr, "group-model pricing")
	return result, errors.New(result.Error)
}

func (a *NewAPI) RollbackGroupModelPricing(ctx context.Context, target domain.TargetSite, expectedRevision string,
	previous domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	return a.rollbackGroupModelPricing(ctx, target, expectedRevision, previous, nil, false)
}

func (a *NewAPI) RollbackGroupModelPricingControlled(ctx context.Context, target domain.TargetSite,
	expectedRevision string, previous domain.GroupModelPricingContract,
	beforeWrite func() error) (domain.GroupModelPricingContract, error) {
	return a.rollbackGroupModelPricing(ctx, target, expectedRevision, previous, beforeWrite, true)
}

func (a *NewAPI) rollbackGroupModelPricing(ctx context.Context, target domain.TargetSite, expectedRevision string,
	previous domain.GroupModelPricingContract, beforeWrite func() error,
	requireGuard bool) (domain.GroupModelPricingContract, error) {
	if err := a.remoteWrites.Check("target group-model pricing rollback"); err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if requireGuard && beforeWrite == nil {
		return domain.GroupModelPricingContract{}, errors.New("a fencing guard is required before rollback mutation")
	}
	current, err := a.ReadGroupModelPricing(WithFreshRead(ctx), target)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if strings.TrimSpace(expectedRevision) == "" || current.Revision != expectedRevision {
		return domain.GroupModelPricingContract{}, errors.New("cannot rollback group-model pricing because the target changed after apply")
	}
	preview, err := a.PreviewGroupModelPricing(ctx, target, current.Revision, previous)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if !preview.ExpectedRevisionMatches {
		return domain.GroupModelPricingContract{}, errors.New("cannot rollback group-model pricing because the target revision is stale")
	}
	if preview.NoChange {
		return current, nil
	}
	if _, err := a.updateGroupModelPricingWithGuard(ctx, target, current.Revision, preview.Desired,
		beforeWrite, requireGuard); err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	readback, err := a.ReadGroupModelPricing(ctx, target)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if readback.Revision != preview.DesiredRevision {
		return domain.GroupModelPricingContract{}, errors.New("group-model pricing rollback readback differs from the requested contract")
	}
	return readback, nil
}

func (a *NewAPI) updateGroupModelPricing(ctx context.Context, target domain.TargetSite, expectedRevision string,
	desired domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	return a.updateGroupModelPricingWithGuard(ctx, target, expectedRevision, desired, nil, false)
}

func (a *NewAPI) updateGroupModelPricingControlled(ctx context.Context, target domain.TargetSite,
	expectedRevision string, desired domain.GroupModelPricingContract,
	beforeWrite func() error) (domain.GroupModelPricingContract, error) {
	if beforeWrite == nil {
		return domain.GroupModelPricingContract{}, errors.New("a fencing guard is required before mutation")
	}
	return a.updateGroupModelPricingWithGuard(ctx, target, expectedRevision, desired, beforeWrite, true)
}

func (a *NewAPI) updateGroupModelPricingWithGuard(ctx context.Context, target domain.TargetSite,
	expectedRevision string, desired domain.GroupModelPricingContract, beforeWrite func() error,
	requireGuard bool) (domain.GroupModelPricingContract, error) {
	var lastErr error
	for attempt := 0; attempt < groupModelPricingWriteAttempts; attempt++ {
		if requireGuard {
			if err := beforeWrite(); err != nil {
				return domain.GroupModelPricingContract{}, &groupModelPricingWriteGuardError{err: err}
			}
		}
		updated, err := a.updateGroupModelPricingOnce(ctx, target, expectedRevision, desired)
		if err == nil {
			return updated, nil
		}
		lastErr = err
		if !isRetryableGroupModelPricingWrite(err) {
			return domain.GroupModelPricingContract{}, err
		}

		current, readErr := a.ReadGroupModelPricing(ctx, target)
		if readErr == nil {
			switch {
			case desired.Revision != "" && current.Revision == desired.Revision:
				return current, nil
			case current.Revision != expectedRevision:
				return domain.GroupModelPricingContract{}, fmt.Errorf(
					"group-model pricing changed while recovering a transient write: expected %s, current %s",
					expectedRevision, current.Revision)
			}
		} else if !isRetryableGroupModelPricingWrite(readErr) {
			return domain.GroupModelPricingContract{}, fmt.Errorf("read back group-model pricing after transient write: %w", readErr)
		}
		if attempt+1 == groupModelPricingWriteAttempts {
			break
		}
		if err := waitForGroupModelPricingRetry(ctx, time.Duration(attempt+1)*groupModelPricingRetryDelay); err != nil {
			return domain.GroupModelPricingContract{}, err
		}
	}
	return domain.GroupModelPricingContract{}, fmt.Errorf("update group-model pricing after %d attempts: %w",
		groupModelPricingWriteAttempts, lastErr)
}

func (a *NewAPI) updateGroupModelPricingOnce(ctx context.Context, target domain.TargetSite, expectedRevision string,
	desired domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	wire, _, err := contractToWire(desired)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	body, err := a.rawAuthorizedRequest(ctx, target, http.MethodPut, "/api/option/group-model-pricing", map[string]any{
		"expected_revision": strings.TrimSpace(expectedRevision),
		"contract":          wire,
	})
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	data, err := decodeGroupModelPricingResponse(body, "update group-model pricing")
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	contract, err := contractFromWire(data.Contract)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	if contract.Revision == "" {
		contract.Revision = data.Revision
	}
	if contract.Revision == "" {
		return domain.GroupModelPricingContract{}, errors.New("group-model pricing update returned no revision")
	}
	return contract, nil
}

func isRetryableGroupModelPricingWrite(err error) bool {
	var remote *RemoteError
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.StatusCode {
	case 0, http.StatusBadRequest, http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func waitForGroupModelPricingRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeGroupModelPricingResponse(body []byte, operation string) (groupModelPricingResponseData, error) {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var envelope groupModelPricingEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return groupModelPricingResponseData{}, &RemoteError{Operation: operation, Definitive: true, Err: err}
	}
	if !envelope.Success {
		return groupModelPricingResponseData{}, &RemoteError{Operation: operation,
			Message: safeRemoteMessage(envelope.Message, "target rejected the group-model pricing request"), Definitive: true}
	}
	return envelope.Data, nil
}

func contractFromWire(wire groupModelPricingWireContract) (domain.GroupModelPricingContract, error) {
	contract := domain.GroupModelPricingContract{SchemaVersion: wire.SchemaVersion, Revision: strings.TrimSpace(wire.Revision),
		Groups: make(map[string]map[string]domain.GroupModelPricingSetting, len(wire.Groups))}
	for group, models := range wire.Groups {
		contract.Groups[group] = make(map[string]domain.GroupModelPricingSetting, len(models))
		for model, setting := range models {
			converted := domain.GroupModelPricingSetting{
				BillingMode: setting.BillingMode, BillingExpr: setting.BillingExpr, TaskProfile: setting.TaskProfile,
				Source: setting.Source, SourceRevision: setting.SourceRevision,
				SourceObservedAtUnixMS: setting.SourceObservedAtUnixMS,
			}
			fields := []struct {
				source *json.Number
				target **string
			}{
				{setting.ModelPrice, &converted.ModelPrice}, {setting.ModelRatio, &converted.ModelRatio},
				{setting.CompletionRatio, &converted.CompletionRatio}, {setting.CacheRatio, &converted.CacheRatio},
				{setting.CacheCreationRatio, &converted.CacheCreationRatio}, {setting.CacheCreation5mRatio, &converted.CacheCreation5mRatio},
				{setting.CacheCreation1hRatio, &converted.CacheCreation1hRatio}, {setting.ImageRatio, &converted.ImageRatio},
				{setting.AudioRatio, &converted.AudioRatio}, {setting.AudioCompletionRatio, &converted.AudioCompletionRatio},
			}
			for _, field := range fields {
				if field.source == nil {
					continue
				}
				value, err := canonicalNonnegativeDecimal(field.source.String())
				if err != nil {
					return domain.GroupModelPricingContract{}, fmt.Errorf("group %q model %q: %w", group, model, err)
				}
				*field.target = &value
			}
			contract.Groups[group][model] = converted
		}
	}
	_, normalized, err := contractToWire(contract)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	return normalized, nil
}

func contractToWire(contract domain.GroupModelPricingContract) (groupModelPricingWireContract, domain.GroupModelPricingContract, error) {
	if contract.SchemaVersion == 0 {
		contract.SchemaVersion = groupModelPricingSchemaVersion
	}
	if contract.SchemaVersion != groupModelPricingSchemaVersion {
		return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, fmt.Errorf("unsupported group-model pricing schema_version %d", contract.SchemaVersion)
	}
	if contract.Groups == nil {
		contract.Groups = map[string]map[string]domain.GroupModelPricingSetting{}
	}
	if len(contract.Groups) > maxGroupModelPricingGroups {
		return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, fmt.Errorf("group-model pricing has more than %d groups", maxGroupModelPricingGroups)
	}
	wire := groupModelPricingWireContract{SchemaVersion: contract.SchemaVersion, Revision: strings.TrimSpace(contract.Revision),
		Groups: make(map[string]map[string]groupModelPricingWireSetting, len(contract.Groups))}
	normalized := domain.GroupModelPricingContract{SchemaVersion: contract.SchemaVersion, Revision: strings.TrimSpace(contract.Revision),
		Groups: make(map[string]map[string]domain.GroupModelPricingSetting, len(contract.Groups))}
	entries := 0
	for group, models := range contract.Groups {
		if err := validateGroupModelPricingName("group", group); err != nil {
			return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, err
		}
		entries += len(models)
		if entries > maxGroupModelPricingEntries {
			return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, fmt.Errorf("group-model pricing has more than %d entries", maxGroupModelPricingEntries)
		}
		wire.Groups[group] = make(map[string]groupModelPricingWireSetting, len(models))
		normalized.Groups[group] = make(map[string]domain.GroupModelPricingSetting, len(models))
		for model, setting := range models {
			if err := validateGroupModelPricingName("model", model); err != nil {
				return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, fmt.Errorf("group %q: %w", group, err)
			}
			wireSetting, normalizedSetting, err := settingToWire(setting)
			if err != nil {
				return groupModelPricingWireContract{}, domain.GroupModelPricingContract{}, fmt.Errorf("group %q model %q: %w", group, model, err)
			}
			wire.Groups[group][model] = wireSetting
			normalized.Groups[group][model] = normalizedSetting
		}
	}
	return wire, normalized, nil
}

func settingToWire(setting domain.GroupModelPricingSetting) (groupModelPricingWireSetting, domain.GroupModelPricingSetting, error) {
	setting.BillingMode = strings.TrimSpace(setting.BillingMode)
	if setting.BillingMode == "" {
		setting.BillingMode = "inherit"
	}
	setting.Source = strings.TrimSpace(setting.Source)
	setting.SourceRevision = strings.TrimSpace(setting.SourceRevision)
	setting.BillingExpr = strings.TrimSpace(setting.BillingExpr)
	if len(setting.Source) > maxGroupModelPricingSource || len(setting.SourceRevision) > maxGroupModelPricingRevision {
		return groupModelPricingWireSetting{}, domain.GroupModelPricingSetting{}, errors.New("pricing provenance exceeds the supported size")
	}
	if setting.SourceObservedAtUnixMS < 0 {
		return groupModelPricingWireSetting{}, domain.GroupModelPricingSetting{}, errors.New("source_observed_at_unix_ms cannot be negative")
	}
	wire := groupModelPricingWireSetting{BillingMode: setting.BillingMode, BillingExpr: setting.BillingExpr,
		TaskProfile: setting.TaskProfile, Source: setting.Source, SourceRevision: setting.SourceRevision,
		SourceObservedAtUnixMS: setting.SourceObservedAtUnixMS}
	fields := []struct {
		source **string
		target **json.Number
	}{
		{&setting.ModelPrice, &wire.ModelPrice}, {&setting.ModelRatio, &wire.ModelRatio},
		{&setting.CompletionRatio, &wire.CompletionRatio}, {&setting.CacheRatio, &wire.CacheRatio},
		{&setting.CacheCreationRatio, &wire.CacheCreationRatio}, {&setting.CacheCreation5mRatio, &wire.CacheCreation5mRatio},
		{&setting.CacheCreation1hRatio, &wire.CacheCreation1hRatio}, {&setting.ImageRatio, &wire.ImageRatio},
		{&setting.AudioRatio, &wire.AudioRatio}, {&setting.AudioCompletionRatio, &wire.AudioCompletionRatio},
	}
	for _, field := range fields {
		if *field.source == nil {
			continue
		}
		value, err := canonicalNonnegativeDecimal(**field.source)
		if err != nil {
			return groupModelPricingWireSetting{}, domain.GroupModelPricingSetting{}, err
		}
		*field.source = &value
		number := json.Number(value)
		*field.target = &number
	}
	if err := validateGroupModelPricingSetting(setting); err != nil {
		return groupModelPricingWireSetting{}, domain.GroupModelPricingSetting{}, err
	}
	return wire, setting, nil
}

func validateGroupModelPricingSetting(setting domain.GroupModelPricingSetting) error {
	hasRatio := setting.ModelRatio != nil || setting.CompletionRatio != nil || setting.CacheRatio != nil ||
		setting.CacheCreationRatio != nil || setting.CacheCreation5mRatio != nil || setting.CacheCreation1hRatio != nil ||
		setting.ImageRatio != nil || setting.AudioRatio != nil || setting.AudioCompletionRatio != nil
	switch setting.BillingMode {
	case "inherit":
		if setting.ModelPrice != nil || hasRatio || setting.BillingExpr != "" {
			return errors.New("inherit mode may only contain a task profile or provenance")
		}
	case "fixed_price":
		if setting.ModelPrice == nil || hasRatio || setting.BillingExpr != "" {
			return errors.New("fixed_price mode requires only model_price")
		}
	case "ratio":
		if setting.ModelRatio == nil || setting.CompletionRatio == nil || setting.ModelPrice != nil || setting.BillingExpr != "" {
			return errors.New("ratio mode requires model_ratio and completion_ratio and cannot contain model_price or billing_expr")
		}
	case "tiered_expr":
		if setting.BillingExpr == "" || setting.ModelPrice != nil || hasRatio {
			return errors.New("tiered_expr mode requires only billing_expr")
		}
	default:
		return fmt.Errorf("unsupported billing_mode %q", setting.BillingMode)
	}
	if setting.TaskProfile != nil {
		profile := *setting.TaskProfile
		profile.Unit = strings.TrimSpace(profile.Unit)
		switch profile.Unit {
		case "per_request", "per_second":
			if profile.FixedDurationSeconds != 0 {
				return fmt.Errorf("%s cannot define fixed_duration_seconds", profile.Unit)
			}
		case "fixed_duration":
			if profile.FixedDurationSeconds < 1 || profile.FixedDurationSeconds > maxFixedDurationSeconds {
				return fmt.Errorf("fixed_duration_seconds must be between 1 and %d", maxFixedDurationSeconds)
			}
		default:
			return fmt.Errorf("unsupported task billing unit %q", profile.Unit)
		}
		setting.TaskProfile = &profile
	}
	return nil
}

func validateGroupModelPricingName(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s name cannot be empty", kind)
	}
	if !utf8.ValidString(value) || len(value) > maxGroupModelPricingNameBytes {
		return fmt.Errorf("%s name is invalid or exceeds %d bytes", kind, maxGroupModelPricingNameBytes)
	}
	return nil
}

func canonicalNonnegativeDecimal(raw string) (string, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return "", fmt.Errorf("pricing value %q must be a finite non-negative decimal", raw)
	}
	return value.String(), nil
}

func diffGroupModelPricingContracts(before, after domain.GroupModelPricingContract) []domain.TargetFieldDiff {
	beforeEntries := flattenGroupModelPricing(before)
	afterEntries := flattenGroupModelPricing(after)
	keys := make([]string, 0, len(beforeEntries)+len(afterEntries))
	seen := make(map[string]struct{}, len(beforeEntries)+len(afterEntries))
	for key := range beforeEntries {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range afterEntries {
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	diff := make([]domain.TargetFieldDiff, 0)
	for _, key := range keys {
		oldValue, oldExists := beforeEntries[key]
		newValue, newExists := afterEntries[key]
		switch {
		case oldExists && !newExists:
			diff = append(diff, domain.TargetFieldDiff{Field: key, Before: oldValue, Kind: "delete"})
		case !oldExists && newExists:
			diff = append(diff, domain.TargetFieldDiff{Field: key, After: newValue, Kind: "add"})
		case oldValue != newValue:
			diff = append(diff, domain.TargetFieldDiff{Field: key, Before: oldValue, After: newValue, Kind: "update"})
		}
	}
	return diff
}

func flattenGroupModelPricing(contract domain.GroupModelPricingContract) map[string]string {
	entries := make(map[string]string)
	for group, models := range contract.Groups {
		for model, setting := range models {
			encoded, _ := json.Marshal(setting)
			entries[group+"/"+model] = string(encoded)
		}
	}
	return entries
}

func HashGroupModelPricingContract(contract domain.GroupModelPricingContract) string {
	_, normalized, err := contractToWire(contract)
	if err != nil {
		return ""
	}
	normalized.Revision = ""
	encoded, _ := json.Marshal(normalized)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
