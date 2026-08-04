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

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

const maxResponseBytes = 12 << 20

var pricingOptionKeys = []string{
	"GroupRatio",
	"ModelRatio",
	"ModelPrice",
	"CompletionRatio",
	"CacheRatio",
	"CreateCacheRatio",
	"ImageRatio",
	"AudioRatio",
	"AudioCompletionRatio",
}

type Adapter interface {
	Name() string
	Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error)
	ApplyGroupRatio(context.Context, domain.TargetSite, string, map[string]string) (domain.TargetSnapshot, error)
	RollbackGroupRatio(context.Context, domain.TargetSite, string, map[string]string) (domain.TargetSnapshot, error)
}

type PricingOptionAdapter interface {
	ApplyPricingOption(context.Context, domain.TargetSite, string, string, map[string]string) (domain.TargetPriceOption, error)
}

type GroupModelPricingAdapter interface {
	ReadGroupModelPricing(context.Context, domain.TargetSite) (domain.GroupModelPricingContract, error)
	PreviewGroupModelPricing(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract) (domain.TargetGroupModelPricingPreview, error)
	ApplyGroupModelPricingControlled(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract, func() error) (domain.TargetMutationResult, error)
	RollbackGroupModelPricing(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error)
	RollbackGroupModelPricingControlled(context.Context, domain.TargetSite, string, domain.GroupModelPricingContract, func() error) (domain.GroupModelPricingContract, error)
}

type ManagedChannelStateAdapter interface {
	ReadManagedChannelState(context.Context, domain.TargetSite, int64) (domain.TargetManagedChannelState, error)
	ApplyManagedChannelState(context.Context, domain.TargetSite, int64, string, []string, *int, string, func() error) (domain.TargetManagedChannelMutation, error)
}

type GroupVisibilityAdapter interface {
	ReadGroupVisibility(context.Context, domain.TargetSite) (domain.TargetPriceOption, error)
	ApplyGroupVisibility(context.Context, domain.TargetSite, string, map[string]string) (domain.TargetPriceOption, error)
	RollbackGroupVisibility(context.Context, domain.TargetSite, string, map[string]string) (domain.TargetPriceOption, error)
}

func AsPricingOptionAdapter(adapter Adapter) (PricingOptionAdapter, error) {
	value, ok := adapter.(PricingOptionAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support pricing option management", adapter.Name())
	}
	return value, nil
}

func AsGroupModelPricingAdapter(adapter Adapter) (GroupModelPricingAdapter, error) {
	value, ok := adapter.(GroupModelPricingAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support group-model pricing", adapter.Name())
	}
	return value, nil
}

func AsManagedChannelStateAdapter(adapter Adapter) (ManagedChannelStateAdapter, error) {
	value, ok := adapter.(ManagedChannelStateAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support managed channel state", adapter.Name())
	}
	return value, nil
}

func AsGroupVisibilityAdapter(adapter Adapter) (GroupVisibilityAdapter, error) {
	value, ok := adapter.(GroupVisibilityAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support user-visible group management", adapter.Name())
	}
	return value, nil
}

func CanonicalPricingOptionKey(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	for _, key := range pricingOptionKeys {
		if strings.EqualFold(raw, key) {
			return key, true
		}
	}
	return "", false
}

type Factory struct {
	client       *http.Client
	credentials  *credentials.Service
	remoteWrites *remotewrite.Gate
	catalogs     *targetCatalogCache
	snapshots    *sharedReadCache[domain.TargetSnapshot]
	contracts    *sharedReadCache[domain.GroupModelPricingContract]
	channels     *sharedReadCache[[]domain.TargetChannel]
	managed      *sharedReadCache[domain.TargetManagedChannelState]
}

func NewFactory(client *http.Client, credentialService *credentials.Service) *Factory {
	return &Factory{client: client, credentials: credentialService, catalogs: newTargetCatalogCache(),
		snapshots: newSharedReadCache[domain.TargetSnapshot](targetSharedReadTTL, targetReadFailureTTL),
		contracts: newSharedReadCache[domain.GroupModelPricingContract](targetSharedReadTTL, targetReadFailureTTL),
		channels:  newSharedReadCache[[]domain.TargetChannel](targetStructuralReadTTL, targetReadFailureTTL),
		managed:   newSharedReadCache[domain.TargetManagedChannelState](targetStructuralReadTTL, targetReadFailureTTL)}
}

func (f *Factory) SetRemoteWriteGate(gate *remotewrite.Gate) {
	f.remoteWrites = gate
}

func (f *Factory) RemoteWriteGate() *remotewrite.Gate {
	if f == nil {
		return nil
	}
	return f.remoteWrites
}

// InvalidateTarget drops all shared reads for one target. Services call this
// after an observed external change; adapter writes invalidate automatically.
func (f *Factory) InvalidateTarget(targetID int64) {
	if f == nil || targetID <= 0 {
		return
	}
	prefix := strconv.FormatInt(targetID, 10) + "|"
	f.catalogs.invalidatePrefix(prefix)
	f.snapshots.invalidatePrefix(prefix)
	f.contracts.invalidatePrefix(prefix)
	f.channels.invalidatePrefix(prefix)
	f.managed.invalidatePrefix(prefix)
}

func (f *Factory) ForTarget(target domain.TargetSite) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(target.Adapter)) {
	case "", "auto", "newapi", "newapi-native", "newapi_legacy":
		return &NewAPI{client: f.client, credentials: f.credentials, remoteWrites: f.remoteWrites, catalogs: f.catalogs,
			snapshots: f.snapshots, contracts: f.contracts, channels: f.channels, managed: f.managed}, nil
	default:
		return nil, fmt.Errorf("target adapter %q is not supported", target.Adapter)
	}
}

type NewAPI struct {
	client             *http.Client
	credentials        *credentials.Service
	credentialOverride *domain.CredentialPayload
	remoteWrites       *remotewrite.Gate
	catalogs           *targetCatalogCache
	snapshots          *sharedReadCache[domain.TargetSnapshot]
	contracts          *sharedReadCache[domain.GroupModelPricingContract]
	channels           *sharedReadCache[[]domain.TargetChannel]
	managed            *sharedReadCache[domain.TargetManagedChannelState]
}

func (a *NewAPI) Name() string { return "newapi" }

func (a *NewAPI) Snapshot(ctx context.Context, target domain.TargetSite) (domain.TargetSnapshot, error) {
	load := func(loadCtx context.Context) (domain.TargetSnapshot, error) {
		return a.snapshotUncached(loadCtx, target)
	}
	if a.snapshots == nil {
		return load(ctx)
	}
	return a.snapshots.read(ctx, targetCatalogCacheKey(target), load)
}

func (a *NewAPI) snapshotUncached(ctx context.Context, target domain.TargetSite) (domain.TargetSnapshot, error) {
	base := strings.TrimRight(target.BaseURL, "/")
	if base == "" {
		return domain.TargetSnapshot{}, errors.New("target base URL is empty")
	}
	client, err := netguard.SameOriginClient(a.client, base)
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	body, err := a.readTargetOptions(reqCtx, ctx, client, target, base+"/api/option/")
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	options, err := parsePricingOptions(body)
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	groupRatio, exists := options["GroupRatio"]
	if !exists {
		return domain.TargetSnapshot{}, errors.New("target options did not contain GroupRatio")
	}
	contract, known := detectOptionContract(body)
	snapshot := domain.TargetSnapshot{TargetSiteID: target.ID, GroupRatio: cloneStringMap(groupRatio.Values), Hash: groupRatio.Hash,
		PricingOptions: options, Contract: contract, ContractKnown: known,
		ObservedAt: time.Now().UTC(), SourceURL: base + "/api/option/"}
	usableGroups, usableGroupsFound, usableGroupsErr := parseStringMapOption(body, "UserUsableGroups")
	if usableGroupsErr != nil {
		return domain.TargetSnapshot{}, usableGroupsErr
	}
	if usableGroupsFound {
		snapshot.UserUsableGroups = cloneStringMap(usableGroups)
		snapshot.UserUsableGroupsHash = HashStringMap(usableGroups)
		snapshot.UserUsableGroupsSupported = true
	}
	groupModelPricing, groupModelErr := a.ReadGroupModelPricing(ctx, target)
	if groupModelErr == nil {
		snapshot.GroupModelPricingSupported = true
		snapshot.GroupModelPricing = &groupModelPricing
	} else if !IsGroupModelPricingUnsupported(groupModelErr) {
		snapshot.GroupModelPricingError = sanitizeTargetError(groupModelErr)
	}
	return snapshot, nil
}

func (a *NewAPI) invalidateTargetReads(target domain.TargetSite) {
	key := targetCatalogCacheKey(target)
	a.catalogs.invalidate(key)
	a.snapshots.invalidate(key)
	a.contracts.invalidate(key)
	a.channels.invalidate(key)
	a.managed.invalidatePrefix(key + "|")
}

func (a *NewAPI) readTargetOptions(requestContext, credentialContext context.Context, client *http.Client,
	target domain.TargetSite, endpoint string) ([]byte, error) {
	const maximumAttempts = 3
	for attempt := 0; attempt < maximumAttempts; attempt++ {
		req, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if err := a.authorize(credentialContext, target, req); err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &RemoteError{Operation: "read target options", Err: err}
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == http.StatusOK {
			return body, nil
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt == maximumAttempts-1 {
			return nil, remoteHTTPError("read target options", resp.StatusCode, body)
		}
		if err := waitTargetReadRetry(requestContext, resp.Header.Get("Retry-After"), attempt); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("read target options retry exhausted")
}

func waitTargetReadRetry(ctx context.Context, retryAfter string, attempt int) error {
	delay := time.Duration(attempt+1) * 500 * time.Millisecond
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if observedAt, err := http.ParseTime(strings.TrimSpace(retryAfter)); err == nil {
		delay = time.Until(observedAt)
		if delay < 0 {
			delay = 0
		}
	}
	if delay > 3*time.Second {
		delay = 3 * time.Second
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *NewAPI) ApplyPricingOption(ctx context.Context, target domain.TargetSite, key, expectedHash string, values map[string]string) (domain.TargetPriceOption, error) {
	if err := a.remoteWrites.Check("target pricing option apply"); err != nil {
		return domain.TargetPriceOption{}, err
	}
	canonical, ok := CanonicalPricingOptionKey(key)
	if !ok {
		return domain.TargetPriceOption{}, fmt.Errorf("pricing option %q is not allowlisted", key)
	}
	if strings.TrimSpace(expectedHash) == "" {
		return domain.TargetPriceOption{}, errors.New("expected pricing option hash is required")
	}
	if values == nil {
		return domain.TargetPriceOption{}, errors.New("pricing option values are required")
	}
	if canonical == "GroupRatio" && len(values) == 0 {
		return domain.TargetPriceOption{}, errors.New("GroupRatio cannot be empty")
	}
	current, err := a.Snapshot(WithFreshRead(ctx), target)
	if err != nil {
		return domain.TargetPriceOption{}, err
	}
	currentOption, exists := current.PricingOptions[canonical]
	if !exists {
		return domain.TargetPriceOption{}, fmt.Errorf("target does not expose pricing option %s", canonical)
	}
	if currentOption.Hash != expectedHash {
		return domain.TargetPriceOption{}, fmt.Errorf("pricing option changed since preview; expected %s, current %s", expectedHash, currentOption.Hash)
	}
	desiredHash := HashDecimalMap(values)
	if desiredHash == currentOption.Hash {
		return currentOption, nil
	}
	if err := a.putOption(ctx, target, canonical, currentOption.Values, values); err != nil {
		return domain.TargetPriceOption{}, err
	}
	readback, err := a.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetPriceOption{}, a.rollbackPriceOptionAfterVerificationFailure(ctx, target, canonical, values,
			currentOption.Values, fmt.Errorf("read back %s: %w", canonical, err))
	}
	readbackOption, exists := readback.PricingOptions[canonical]
	if !exists || readbackOption.Hash != desiredHash {
		return domain.TargetPriceOption{}, a.rollbackPriceOptionAfterVerificationFailure(ctx, target, canonical, values,
			currentOption.Values, fmt.Errorf("target readback differs from requested %s", canonical))
	}
	return readbackOption, nil
}

func (a *NewAPI) ApplyGroupRatio(ctx context.Context, target domain.TargetSite, expectedHash string, ratio map[string]string) (domain.TargetSnapshot, error) {
	if err := a.remoteWrites.Check("target GroupRatio apply"); err != nil {
		return domain.TargetSnapshot{}, err
	}
	current, err := a.Snapshot(WithFreshRead(ctx), target)
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	if expectedHash != "" && current.Hash != expectedHash {
		return domain.TargetSnapshot{}, fmt.Errorf("target changed since preview; expected %s, current %s", expectedHash, current.Hash)
	}
	if err := a.putOption(ctx, target, "GroupRatio", current.GroupRatio, ratio); err != nil {
		return domain.TargetSnapshot{}, err
	}
	readback, err := a.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetSnapshot{}, a.rollbackAfterVerificationFailure(ctx, target, ratio, current.GroupRatio,
			fmt.Errorf("read back GroupRatio: %w", err))
	}
	if readback.Hash != HashRatios(ratio) {
		return domain.TargetSnapshot{}, a.rollbackAfterVerificationFailure(ctx, target, ratio, current.GroupRatio,
			errors.New("target readback differs from requested GroupRatio"))
	}
	return readback, nil
}

func (a *NewAPI) RollbackGroupRatio(ctx context.Context, target domain.TargetSite, expectedHash string, ratio map[string]string) (domain.TargetSnapshot, error) {
	if err := a.remoteWrites.Check("target GroupRatio rollback"); err != nil {
		return domain.TargetSnapshot{}, err
	}
	current, err := a.Snapshot(WithFreshRead(ctx), target)
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	if expectedHash != "" && current.Hash != expectedHash {
		return domain.TargetSnapshot{}, errors.New("cannot rollback because the target changed after apply")
	}
	if err := a.putOption(ctx, target, "GroupRatio", current.GroupRatio, ratio); err != nil {
		return domain.TargetSnapshot{}, err
	}
	readback, err := a.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetSnapshot{}, err
	}
	if readback.Hash != HashRatios(ratio) {
		return domain.TargetSnapshot{}, errors.New("rollback readback differs from the requested snapshot")
	}
	return readback, nil
}

func (a *NewAPI) ReadGroupVisibility(ctx context.Context, target domain.TargetSite) (domain.TargetPriceOption, error) {
	snapshot, err := a.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetPriceOption{}, err
	}
	if !snapshot.UserUsableGroupsSupported {
		return domain.TargetPriceOption{}, errors.New("target options did not contain UserUsableGroups")
	}
	return domain.TargetPriceOption{Key: "UserUsableGroups", Values: cloneStringMap(snapshot.UserUsableGroups),
		Hash: snapshot.UserUsableGroupsHash}, nil
}

func (a *NewAPI) ApplyGroupVisibility(ctx context.Context, target domain.TargetSite, expectedHash string,
	values map[string]string) (domain.TargetPriceOption, error) {
	if err := a.remoteWrites.Check("target user-visible groups apply"); err != nil {
		return domain.TargetPriceOption{}, err
	}
	if strings.TrimSpace(expectedHash) == "" || values == nil {
		return domain.TargetPriceOption{}, errors.New("expected UserUsableGroups hash and values are required")
	}
	current, err := a.ReadGroupVisibility(WithFreshRead(ctx), target)
	if err != nil {
		return domain.TargetPriceOption{}, err
	}
	if current.Hash != expectedHash {
		return domain.TargetPriceOption{}, fmt.Errorf("UserUsableGroups changed since preview; expected %s, current %s", expectedHash, current.Hash)
	}
	desiredHash := HashStringMap(values)
	if desiredHash == current.Hash {
		return current, nil
	}
	if err := a.putStringMapOption(ctx, target, "UserUsableGroups", current.Values, values); err != nil {
		return domain.TargetPriceOption{}, err
	}
	readback, err := a.ReadGroupVisibility(ctx, target)
	if err != nil || readback.Hash != desiredHash {
		cause := errors.New("target readback differs from requested UserUsableGroups")
		if err != nil {
			cause = fmt.Errorf("read back UserUsableGroups: %w", err)
		}
		return domain.TargetPriceOption{}, a.rollbackStringMapAfterVerificationFailure(ctx, target, "UserUsableGroups",
			values, current.Values, cause)
	}
	return readback, nil
}

func (a *NewAPI) RollbackGroupVisibility(ctx context.Context, target domain.TargetSite, expectedHash string,
	values map[string]string) (domain.TargetPriceOption, error) {
	if err := a.remoteWrites.Check("target user-visible groups rollback"); err != nil {
		return domain.TargetPriceOption{}, err
	}
	current, err := a.ReadGroupVisibility(WithFreshRead(ctx), target)
	if err != nil {
		return domain.TargetPriceOption{}, err
	}
	if current.Hash != expectedHash {
		return domain.TargetPriceOption{}, errors.New("cannot rollback because UserUsableGroups changed after apply")
	}
	desiredHash := HashStringMap(values)
	if current.Hash == desiredHash {
		return current, nil
	}
	if err := a.putStringMapOption(ctx, target, "UserUsableGroups", current.Values, values); err != nil {
		return domain.TargetPriceOption{}, err
	}
	readback, err := a.ReadGroupVisibility(ctx, target)
	if err != nil || readback.Hash != desiredHash {
		return domain.TargetPriceOption{}, errors.New("UserUsableGroups rollback readback differs from the requested snapshot")
	}
	return readback, nil
}

func (a *NewAPI) putOption(ctx context.Context, target domain.TargetSite, key string,
	current, value map[string]string) error {
	if err := a.remoteWrites.Check("target option write"); err != nil {
		return err
	}
	canonical, allowed := CanonicalPricingOptionKey(key)
	if !allowed {
		return fmt.Errorf("pricing option %q is not allowlisted", key)
	}
	key = canonical
	if key == "GroupRatio" && len(value) == 0 {
		return errors.New("GroupRatio cannot be empty")
	}
	normalized, err := normalizePricingOptionMutation(current, value)
	if err != nil {
		return fmt.Errorf("%s mutation: %w", key, err)
	}
	numeric := make(map[string]json.Number, len(normalized))
	for name, raw := range normalized {
		parsed, _ := decimal.NewFromString(strings.TrimSpace(raw))
		numeric[name] = json.Number(parsed.String())
	}
	return a.putRawOption(ctx, target, key, numeric)
}

func (a *NewAPI) putStringMapOption(ctx context.Context, target domain.TargetSite, key string,
	current, value map[string]string) error {
	if err := a.remoteWrites.Check("target string-map option write"); err != nil {
		return err
	}
	if key != "UserUsableGroups" || value == nil {
		return errors.New("unsupported target string-map option")
	}
	for group, label := range value {
		if previous, exists := current[group]; exists && previous == label {
			continue
		}
		if strings.TrimSpace(group) != group || group == "" || len(group) > 64 || strings.ContainsAny(group, ",\x00\r\n") {
			return fmt.Errorf("UserUsableGroups contains invalid group %q", group)
		}
		if strings.TrimSpace(label) == "" || len(label) > 256 || strings.ContainsAny(label, "\x00\r\n") {
			return fmt.Errorf("UserUsableGroups label for %q is invalid", group)
		}
	}
	return a.putRawOption(ctx, target, key, value)
}

func (a *NewAPI) putRawOption(ctx context.Context, target domain.TargetSite, key string, value any) error {
	if err := a.remoteWrites.Check("target option write request"); err != nil {
		return err
	}
	client, err := netguard.SameOriginClient(a.client, target.BaseURL)
	if err != nil {
		return err
	}
	encodedValue, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"key": key, "value": string(encodedValue)})
	if err != nil {
		return err
	}
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, strings.TrimRight(target.BaseURL, "/")+"/api/option/", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if err := a.authorize(ctx, target, req); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return &RemoteError{Operation: "write target option", Err: err}
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteHTTPError("write target option", resp.StatusCode, responseBody)
	}
	var envelope struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return &RemoteError{Operation: "decode target option response", Definitive: true, Err: err}
	}
	if !envelope.Success {
		return &RemoteError{Operation: "write target option",
			Message: safeRemoteMessage(envelope.Message, "target rejected the option update"), Definitive: true}
	}
	a.invalidateTargetReads(target)
	return nil
}

func (a *NewAPI) rollbackPriceOptionAfterVerificationFailure(ctx context.Context, target domain.TargetSite, key string,
	desired, original map[string]string, cause error) error {
	current, err := a.Snapshot(ctx, target)
	if err != nil {
		return fmt.Errorf("%w; conditional rollback could not inspect current state: %v", cause, err)
	}
	currentOption, exists := current.PricingOptions[key]
	if !exists {
		return fmt.Errorf("%w; conditional rollback stopped because %s disappeared", cause, key)
	}
	if currentOption.Hash == HashDecimalMap(original) {
		return cause
	}
	if currentOption.Hash != HashDecimalMap(desired) {
		return fmt.Errorf("%w; conditional rollback stopped because another %s change was detected", cause, key)
	}
	if err := a.putOption(ctx, target, key, currentOption.Values, original); err != nil {
		return fmt.Errorf("%w; conditional rollback failed: %v", cause, err)
	}
	rolledBack, err := a.Snapshot(ctx, target)
	if err != nil || rolledBack.PricingOptions[key].Hash != HashDecimalMap(original) {
		return fmt.Errorf("%w; conditional rollback could not be verified", cause)
	}
	return fmt.Errorf("%w; the unverified write was rolled back", cause)
}

func (a *NewAPI) rollbackStringMapAfterVerificationFailure(ctx context.Context, target domain.TargetSite, key string,
	desired, original map[string]string, cause error) error {
	current, err := a.ReadGroupVisibility(ctx, target)
	if err != nil {
		return fmt.Errorf("%w; conditional rollback could not inspect current state: %v", cause, err)
	}
	if current.Hash == HashStringMap(original) {
		return cause
	}
	if current.Hash != HashStringMap(desired) {
		return fmt.Errorf("%w; conditional rollback stopped because another %s change was detected", cause, key)
	}
	if err := a.putStringMapOption(ctx, target, key, current.Values, original); err != nil {
		return fmt.Errorf("%w; conditional rollback failed: %v", cause, err)
	}
	rolledBack, err := a.ReadGroupVisibility(ctx, target)
	if err != nil || rolledBack.Hash != HashStringMap(original) {
		return fmt.Errorf("%w; conditional rollback could not be verified", cause)
	}
	return fmt.Errorf("%w; the unverified write was rolled back", cause)
}

func (a *NewAPI) rollbackAfterVerificationFailure(ctx context.Context, target domain.TargetSite, desired, original map[string]string, cause error) error {
	current, err := a.Snapshot(ctx, target)
	if err != nil {
		return fmt.Errorf("%w; conditional rollback could not inspect current state: %v", cause, err)
	}
	if current.Hash == HashRatios(original) {
		return cause
	}
	if current.Hash != HashRatios(desired) {
		return fmt.Errorf("%w; conditional rollback stopped because another change was detected", cause)
	}
	if err := a.putOption(ctx, target, "GroupRatio", current.GroupRatio, original); err != nil {
		return fmt.Errorf("%w; conditional rollback failed: %v", cause, err)
	}
	rolledBack, err := a.Snapshot(ctx, target)
	if err != nil || rolledBack.Hash != HashRatios(original) {
		return fmt.Errorf("%w; conditional rollback could not be verified", cause)
	}
	return fmt.Errorf("%w; the unverified write was rolled back", cause)
}

func (a *NewAPI) authorize(ctx context.Context, target domain.TargetSite, req *http.Request) error {
	if a.credentialOverride != nil {
		return authorizePayload(req, *a.credentialOverride)
	}
	if target.CredentialID == 0 {
		return errors.New("target requires an encrypted management credential")
	}
	if a.credentials == nil {
		return errors.New("target credential service is required")
	}
	credential, payload, err := a.credentials.Load(ctx, target.CredentialID)
	if err != nil {
		return fmt.Errorf("load target credential: %w", err)
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, target.BaseURL); err != nil {
		return fmt.Errorf("target credential origin mismatch: %w", err)
	}
	return authorizePayload(req, payload)
}

func authorizePayload(req *http.Request, payload domain.CredentialPayload) error {
	if payload.Authorization != "" {
		req.Header.Set("Authorization", payload.Authorization)
	}
	if payload.Cookie != "" {
		req.Header.Set("Cookie", payload.Cookie)
	}
	if payload.UserID > 0 {
		req.Header.Set("New-Api-User", fmt.Sprintf("%d", payload.UserID))
	}
	for key, value := range payload.Headers {
		if strings.TrimSpace(key) != "" {
			req.Header.Set(key, value)
		}
	}
	if strings.TrimSpace(req.Header.Get("Authorization")) == "" && strings.TrimSpace(req.Header.Get("Cookie")) == "" {
		return errors.New("management credential contains neither authorization nor session cookie")
	}
	return nil
}

func parseGroupRatio(body []byte) (map[string]string, error) {
	options, err := parsePricingOptions(body)
	if err != nil {
		return nil, err
	}
	option, exists := options["GroupRatio"]
	if !exists {
		return nil, errors.New("target options did not contain GroupRatio")
	}
	return cloneStringMap(option.Values), nil
}

func parsePricingOptions(body []byte) (map[string]domain.TargetPriceOption, error) {
	var root struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode target options: %w", err)
	}
	if root.Success == nil || !*root.Success {
		if strings.TrimSpace(root.Message) == "" {
			root.Message = "target options response did not confirm success"
		}
		return nil, errors.New(root.Message)
	}
	rawOptions := make(map[string]json.RawMessage)
	var options []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(root.Data, &options); err == nil {
		for _, option := range options {
			if canonical, ok := CanonicalPricingOptionKey(option.Key); ok {
				rawOptions[canonical] = json.RawMessage(option.Value)
			}
		}
	} else {
		var direct map[string]json.RawMessage
		if err := json.Unmarshal(root.Data, &direct); err != nil {
			return nil, errors.New("target options data has an unsupported shape")
		}
		for key, raw := range direct {
			canonical, ok := CanonicalPricingOptionKey(key)
			if !ok {
				continue
			}
			if len(raw) > 0 && raw[0] == '"' {
				var text string
				if err := json.Unmarshal(raw, &text); err != nil {
					return nil, fmt.Errorf("decode %s option string: %w", canonical, err)
				}
				raw = json.RawMessage(text)
			}
			rawOptions[canonical] = raw
		}
	}
	result := make(map[string]domain.TargetPriceOption, len(rawOptions))
	for key, raw := range rawOptions {
		values, err := parseRatioJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		result[key] = domain.TargetPriceOption{Key: key, Values: values, Hash: HashDecimalMap(values)}
	}
	return result, nil
}

func parseStringMapOption(body []byte, expectedKey string) (map[string]string, bool, error) {
	var root struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false, fmt.Errorf("decode target options: %w", err)
	}
	if root.Success == nil || !*root.Success {
		return nil, false, errors.New("target options response did not confirm success")
	}
	var raw json.RawMessage
	var options []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(root.Data, &options); err == nil {
		for _, option := range options {
			if strings.EqualFold(strings.TrimSpace(option.Key), expectedKey) {
				raw = json.RawMessage(option.Value)
				break
			}
		}
	} else {
		var direct map[string]json.RawMessage
		if err := json.Unmarshal(root.Data, &direct); err != nil {
			return nil, false, errors.New("target options data has an unsupported shape")
		}
		for key, value := range direct {
			if strings.EqualFold(strings.TrimSpace(key), expectedKey) {
				raw = value
				if len(raw) > 0 && raw[0] == '"' {
					var text string
					if err := json.Unmarshal(raw, &text); err != nil {
						return nil, false, fmt.Errorf("decode %s option string: %w", expectedKey, err)
					}
					raw = json.RawMessage(text)
				}
				break
			}
		}
	}
	if len(raw) == 0 {
		return nil, false, nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", expectedKey, err)
	}
	if values == nil {
		return nil, false, fmt.Errorf("decode %s: null map", expectedKey)
	}
	return values, true, nil
}

func detectOptionContract(body []byte) (string, bool) {
	var root struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Data) == 0 {
		return "unknown", false
	}
	var options []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if json.Unmarshal(root.Data, &options) == nil {
		return "newapi-option-array-v1", true
	}
	var direct map[string]json.RawMessage
	if json.Unmarshal(root.Data, &direct) == nil {
		return "newapi-option-map-readonly", false
	}
	return "unknown", false
}

func parseRatioJSON(raw []byte) (map[string]string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("decode GroupRatio: %w", err)
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		var number json.Number
		if err := json.Unmarshal(value, &number); err == nil {
			result[key] = number.String()
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("GroupRatio value for %q is not numeric", key)
		}
		result[key] = text
	}
	return result, nil
}

func HashRatios(ratio map[string]string) string {
	return HashDecimalMap(ratio)
}

func HashDecimalMap(ratio map[string]string) string {
	keys := make([]string, 0, len(ratio))
	for key := range ratio {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		value := strings.TrimSpace(ratio[key])
		if parsed, err := decimal.NewFromString(value); err == nil {
			value = parsed.String()
		}
		builder.WriteString(value)
		builder.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func HashStringMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		writeMirrorHashPart(hash, key)
		writeMirrorHashPart(hash, values[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func compact(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 500 {
		return text[:500] + "..."
	}
	return text
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, exists := ctx.Deadline(); exists {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 25*time.Second)
}
