package mirrorservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

var adoptionPolicyNamespace = uuid.MustParse("75d27226-17f5-4e66-af35-9136bbde5416")

func (s *Service) PreviewExistingChannelAdoption(ctx context.Context,
	request AdoptionPreviewRequest) (AdoptionPreview, error) {
	entry, err := s.buildAdoptionPreview(ctx, request)
	if err != nil {
		return AdoptionPreview{}, err
	}
	s.rememberAdoption(entry)
	return entry.public, nil
}

func (s *Service) GetAdoptionPreview(id string) (AdoptionPreview, error) {
	entry, err := s.cachedAdoption(id)
	if err != nil {
		return AdoptionPreview{}, err
	}
	return entry.public, nil
}

func (s *Service) ApplyExistingChannelAdoption(ctx context.Context,
	request AdoptionApplyRequest) (AdoptionReceipt, error) {
	entry, err := s.cachedAdoption(request.PreviewID)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	if !request.ApprovalGranted {
		return AdoptionReceipt{}, errors.New("existing-channel adoption requires explicit administrator approval")
	}
	current, err := s.buildAdoptionPreview(targets.WithFreshRead(ctx), AdoptionPreviewRequest{ChannelID: entry.public.Channel.ID,
		Policy: adoptionPolicyDocument(entry.public.Policy)})
	if err != nil {
		return AdoptionReceipt{}, err
	}
	if current.public.AlreadyAdopted {
		return AdoptionReceipt{PreviewID: request.PreviewID, Policy: current.public.Policy,
			ChannelID: current.public.Channel.ID, ContractRevision: current.public.ContractRevision, Replayed: true}, nil
	}
	if current.public.ID != entry.public.ID {
		// Apply always performs a fresh read. Continue with the freshly verified
		// same-bound adoption when cached target state changed after preview.
		entry = current
	}
	if !entry.public.Executable {
		return AdoptionReceipt{}, firstAdoptionBlockingError(entry.public)
	}
	if err := s.remote.Check("existing-channel mirror adoption"); err != nil {
		return AdoptionReceipt{}, err
	}

	policy, err := s.ensureAdoptionDraft(ctx, entry.public.Policy)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	entry = current

	now := s.now()
	lease, err := s.store.AcquireMirrorLease(ctx, policy.TargetSiteID,
		"mirror-adoption:"+policy.PolicyUUID+":"+entry.public.ID, now, 10*time.Minute)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, s.now()) }()

	targetSite, err := s.store.GetTarget(ctx, policy.TargetSiteID)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	target, err := s.target(ctx, targetSite)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	if prober, ok := s.visibility.(interface {
		ProbeMirrorVisibility(context.Context, domain.TargetSite, int64, string) error
	}); !ok {
		return AdoptionReceipt{}, ErrVisibilityUnavailable
	} else if err := prober.ProbeMirrorVisibility(ctx, targetSite, entry.public.Channel.ID,
		policy.TargetGroup); err != nil {
		return AdoptionReceipt{}, fmt.Errorf("adoption visibility capability probe: %w", err)
	}
	guard := func() error {
		if err := s.store.CheckTargetControlLease(ctx, lease, s.now()); err != nil {
			return err
		}
		stored, err := s.store.GetMirrorPolicy(ctx, policy.PolicyUUID)
		if err != nil {
			return err
		}
		if stored.Enabled || stored.PolicyVersion != policy.PolicyVersion ||
			!sameAdoptionPolicyDefinition(stored, policy) {
			return store.ErrMirrorPolicyVersionConflict
		}
		return nil
	}
	mutation, err := target.ApplyGroupModelPricingControlled(ctx, targetSite,
		entry.public.ContractRevision, entry.desiredContract, guard)
	if err != nil {
		return AdoptionReceipt{PreviewID: entry.public.ID, Policy: policy, ChannelID: entry.public.Channel.ID,
			ContractRevision: mutation.ReadbackHash}, err
	}
	contract, managed, channel, err := readBackAdoption(ctx, target, targetSite, entry.public)
	if err != nil {
		return AdoptionReceipt{PreviewID: entry.public.ID, Policy: policy, ChannelID: entry.public.Channel.ID,
			ContractRevision: mutation.ReadbackHash, RemoteStamped: true}, err
	}
	desired, err := adoptionOwnership(policy, managed, channel, contract)
	if err != nil {
		return AdoptionReceipt{}, err
	}
	adopted, replayed, err := s.store.AdoptMirrorOwnershipAndEnable(ctx, desired, policy.PolicyVersion,
		lease, s.now())
	if err != nil {
		return AdoptionReceipt{PreviewID: entry.public.ID, Policy: policy, ChannelID: channel.ID,
			ContractRevision: contract.Revision, RemoteStamped: true}, err
	}
	return AdoptionReceipt{PreviewID: entry.public.ID, Policy: adopted, ChannelID: channel.ID,
		ContractRevision: contract.Revision, RemoteStamped: mutation.ReadbackHash != entry.public.ContractRevision,
		Replayed: replayed}, nil
}

func (s *Service) buildAdoptionPreview(ctx context.Context,
	request AdoptionPreviewRequest) (cachedAdoptionPreview, error) {
	policy, err := buildAdoptionPolicy(request)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	if err := s.validateAdoptionPolicyIdentity(ctx, policy, request.ChannelID); err != nil {
		return cachedAdoptionPreview{}, err
	}
	if err := s.validateBindings(ctx, policy); err != nil {
		return cachedAdoptionPreview{}, err
	}
	snapshot, err := s.store.LatestEffectiveSnapshot(ctx, policy.SupplierID)
	if err != nil {
		return cachedAdoptionPreview{}, fmt.Errorf("read authoritative source snapshot: %w", err)
	}
	source, sourceState, _, err := sourceFromSnapshot(snapshot, policy.SourceGroup)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	switch sourceState {
	case mirror.SourceGroupPresent:
		if len(source.Models) == 0 {
			return cachedAdoptionPreview{}, errors.New("the selected source group has no model membership evidence")
		}
	case mirror.SourceGroupEmpty:
		if len(source.Models) != 0 {
			return cachedAdoptionPreview{}, errors.New("the selected empty source group contains unexpected model evidence")
		}
	default:
		return cachedAdoptionPreview{}, errors.New("the selected source group is missing from the authoritative catalog")
	}
	targetSite, err := s.store.GetTarget(ctx, policy.TargetSiteID)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	target, err := s.target(ctx, targetSite)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	channels, err := target.ListChannels(ctx, targetSite)
	if err != nil {
		return cachedAdoptionPreview{}, fmt.Errorf("read target channels: %w", err)
	}
	channel, err := exactAdoptionChannel(channels, request.ChannelID, policy.TargetGroup)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	managed, err := target.ReadManagedChannelState(ctx, targetSite, channel.ID)
	if err != nil {
		return cachedAdoptionPreview{}, fmt.Errorf("read selected managed channel: %w", err)
	}
	channelModels := canonicalAdoptionModels(channel.Models)
	managedModels := canonicalAdoptionModels(managed.Models)
	if strings.Join(channelModels, "\x00") != strings.Join(managedModels, "\x00") {
		return cachedAdoptionPreview{}, errors.New("selected channel list and managed-state model sets are inconsistent")
	}
	managed.Models = managedModels
	contract, err := target.ReadGroupModelPricing(ctx, targetSite)
	if err != nil {
		return cachedAdoptionPreview{}, fmt.Errorf("read target group-model contract: %w", err)
	}
	groupModels := canonicalContractModels(contract.Groups[policy.TargetGroup])

	desired, err := cloneAdoptionContract(contract)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	marker := "relayops:policy/" + policy.PolicyUUID
	for modelName, setting := range desired.Groups[policy.TargetGroup] {
		if sourceMarker := strings.TrimSpace(setting.Source); strings.HasPrefix(sourceMarker, "relayops:policy/") &&
			sourceMarker != marker {
			return cachedAdoptionPreview{}, fmt.Errorf("target model %q is already stamped by another mirror policy", modelName)
		}
		setting.Source = marker
		desired.Groups[policy.TargetGroup][modelName] = setting
	}
	remotePreview, err := target.PreviewGroupModelPricing(ctx, targetSite, contract.Revision, desired)
	if err != nil {
		return cachedAdoptionPreview{}, fmt.Errorf("preview ownership-only target contract: %w", err)
	}
	if !remotePreview.ExpectedRevisionMatches || remotePreview.CurrentRevision != contract.Revision {
		return cachedAdoptionPreview{}, errors.New("target group-model contract changed while adoption was being previewed")
	}
	beforeHash, err := mirror.TargetFunctionalHash(remotePreview.Current, map[string]string{})
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	afterHash, err := mirror.TargetFunctionalHash(remotePreview.Desired, map[string]string{})
	if err != nil || beforeHash != afterHash {
		return cachedAdoptionPreview{}, errors.New("ownership-only adoption preview changed functional pricing, billing or model membership")
	}
	policy, alreadyAdopted, gates, err := s.adoptionPolicyState(ctx, policy, managed, channel, remotePreview.Desired)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	gates = append(gates, s.adoptionServiceGates(targetSite)...)
	sort.Slice(gates, func(i, j int) bool { return gates[i].Code < gates[j].Code })
	sourceSet := make(map[string]struct{}, len(source.Models))
	for _, model := range source.Models {
		sourceSet[model.UpstreamRequestName] = struct{}{}
	}
	overlap := 0
	for _, modelName := range groupModels {
		if _, ok := sourceSet[modelName]; ok {
			overlap++
		}
	}
	now := s.now()
	preview := AdoptionPreview{Policy: policy, Channel: channel, ManagedState: managed,
		ContractRevision: contract.Revision, DesiredContractRevision: remotePreview.DesiredRevision,
		ContractModels: remotePreview.Current.Groups[policy.TargetGroup], SourceModelCount: len(source.Models),
		SourceTargetOverlap: overlap, ServiceGates: gates, AlreadyAdopted: alreadyAdopted,
		Executable: !alreadyAdopted && !hasBlockingServiceGate(gates), GeneratedAt: now}
	preview.ID, err = hashAdoptionPreview(preview)
	if err != nil {
		return cachedAdoptionPreview{}, err
	}
	return cachedAdoptionPreview{public: preview, currentContract: remotePreview.Current,
		desiredContract: remotePreview.Desired, expiresAt: now.Add(s.options.PreviewTTL)}, nil
}

func buildAdoptionPolicy(request AdoptionPreviewRequest) (mirror.Policy, error) {
	document := request.Policy
	if request.ChannelID <= 0 || document.SupplierID <= 0 || document.TargetSiteID <= 0 ||
		strings.TrimSpace(document.SourceGroup) == "" || strings.TrimSpace(document.TargetGroup) == "" {
		return mirror.Policy{}, errors.New("supplier, source group, target, target group and channel are required")
	}
	policyUUID := deterministicAdoptionPolicyUUID(document.SupplierID, document.SourceGroup,
		document.TargetSiteID, document.TargetGroup, request.ChannelID)
	if value := strings.TrimSpace(document.PolicyUUID); value != "" {
		parsed, parseErr := uuid.Parse(value)
		if parseErr != nil || parsed.String() != value {
			return mirror.Policy{}, errors.New("policy_uuid must be a valid UUID")
		}
		policyUUID = value
	}
	if document.PolicyVersion != 0 && document.PolicyVersion != 1 {
		return mirror.Policy{}, errors.New("a new adoption policy must start at version 1")
	}
	if document.SchemaVersion != 0 && document.SchemaVersion != mirror.PolicySchemaVersion {
		return mirror.Policy{}, errors.New("unsupported adoption policy schema")
	}
	if document.Enabled != nil && *document.Enabled {
		return mirror.Policy{}, errors.New("adoption preview cannot directly enable a policy")
	}
	if document.Kind != "" && document.Kind != mirror.PolicyKindCompleteMirror {
		return mirror.Policy{}, errors.New("existing-channel adoption requires a complete-mirror policy")
	}
	defaults := mirror.NewCompleteMirrorDocument(policyUUID)
	defaults.Enabled = boolValue(false)
	defaults.SupplierID, defaults.SourceGroup = document.SupplierID, document.SourceGroup
	defaults.TargetSiteID, defaults.TargetGroup = document.TargetSiteID, document.TargetGroup
	copyAdoptionPolicyOptions(&defaults, document)
	policy, err := mirror.DecodePolicy(defaults)
	if err != nil {
		return mirror.Policy{}, err
	}
	if err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		return mirror.Policy{}, err
	}
	return policy, nil
}

func (s *Service) validateAdoptionPolicyIdentity(ctx context.Context, desired mirror.Policy,
	channelID int64) error {
	deterministic := deterministicAdoptionPolicyUUID(desired.SupplierID, desired.SourceGroup,
		desired.TargetSiteID, desired.TargetGroup, channelID)
	if desired.PolicyUUID == deterministic {
		return nil
	}
	existing, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if store.IsNotFound(err) {
		return errors.New("policy_uuid must identify an existing native complete-mirror policy")
	}
	if err != nil {
		return fmt.Errorf("read existing adoption policy identity: %w", err)
	}
	if existing.LegacyPolicyID != 0 || existing.Kind != mirror.PolicyKindCompleteMirror {
		return errors.New("policy_uuid must identify an existing native complete-mirror policy")
	}
	if !sameAdoptionPolicyDefinition(existing, desired) {
		return fmt.Errorf("existing policy_uuid is bound to different mirror settings: %s",
			adoptionPolicyDefinitionMismatch(existing, desired))
	}
	return nil
}

func copyAdoptionPolicyOptions(target *mirror.PolicyDocument, source mirror.PolicyDocument) {
	target.ExcludedModels = append([]string(nil), source.ExcludedModels...)
	if strings.TrimSpace(source.SourceModelPattern) != "" {
		target.SourceModelPattern = source.SourceModelPattern
	}
	if strings.TrimSpace(source.PriceAnchor) != "" {
		target.PriceAnchor = source.PriceAnchor
	}
	if source.SyncGroupRatio != nil {
		target.SyncGroupRatio = source.SyncGroupRatio
	}
	if source.GroupRatioRule.Type != "" {
		target.GroupRatioRule = source.GroupRatioRule
	}
	if source.SyncModelPrices != nil {
		target.SyncModelPrices = source.SyncModelPrices
	}
	if source.ModelPriceRule.Type != "" {
		target.ModelPriceRule = source.ModelPriceRule
	}
	if source.SyncBillingMode != nil {
		target.SyncBillingMode = source.SyncBillingMode
	}
	if source.SyncModelAdditions != nil {
		target.SyncModelAdditions = source.SyncModelAdditions
	}
	if source.SyncModelRemovals != nil {
		target.SyncModelRemovals = source.SyncModelRemovals
	}
	copyAdoptionString(&target.ModelScopeMode, source.ModelScopeMode)
	copyAdoptionString(&target.EmptyGroupAction, source.EmptyGroupAction)
	copyAdoptionString(&target.ExecutionMode, source.ExecutionMode)
	copyAdoptionString(&target.MinimumRatio, source.MinimumRatio)
	copyAdoptionString(&target.MaximumRatio, source.MaximumRatio)
	copyAdoptionString(&target.RoundingStep, source.RoundingStep)
	target.OnlyFollowIncrease = source.OnlyFollowIncrease
	target.OnlyFollowDecrease = source.OnlyFollowDecrease
}

func copyAdoptionString(target *string, source string) {
	if strings.TrimSpace(source) != "" {
		*target = source
	}
}

func deterministicAdoptionPolicyUUID(supplierID int64, sourceGroup string, targetID int64,
	targetGroup string, channelID int64) string {
	identity := strings.Join([]string{"v1", strconv.FormatInt(supplierID, 10), strings.TrimSpace(sourceGroup),
		strconv.FormatInt(targetID, 10), strings.TrimSpace(targetGroup), strconv.FormatInt(channelID, 10)}, "\x00")
	return uuid.NewSHA1(adoptionPolicyNamespace, []byte(identity)).String()
}

func (s *Service) adoptionPolicyState(ctx context.Context, desired mirror.Policy,
	managed domain.TargetManagedChannelState, channel domain.TargetChannel,
	contract domain.GroupModelPricingContract) (mirror.Policy, bool, []ServiceGate, error) {
	existing, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if err == nil {
		if !sameAdoptionPolicyDefinition(existing, desired) {
			return desired, false, nil, errors.New("the deterministic adoption policy is already bound to different mirror settings")
		}
		desired = existing
	} else if !store.IsNotFound(err) {
		return desired, false, nil, err
	}
	wanted, err := adoptionOwnership(desired, managed, channel, contract)
	if err != nil {
		return desired, false, nil, err
	}
	gates := make([]ServiceGate, 0)
	conflict := func(code, message string) {
		gates = append(gates, ServiceGate{Code: code, Message: message, BlocksApply: true})
	}
	if owner, err := s.store.GetActiveMirrorChannelOwnership(ctx, desired.TargetSiteID, channel.ID); err == nil {
		if !sameAdoptionChannelOwner(owner, wanted.Channel) {
			conflict("channel_owned_elsewhere", "The selected target channel is owned by another mirror policy.")
		}
	} else if !store.IsNotFound(err) {
		return desired, false, nil, err
	}
	for _, item := range wanted.Models {
		owner, ownerErr := s.store.GetActiveMirrorModelOwnership(ctx, item.TargetSiteID, item.TargetGroup, item.TargetModel)
		if ownerErr == nil && !sameAdoptionModelOwner(owner, item) {
			conflict("model_owned_elsewhere:"+item.TargetModel, "A target group-model contract item is owned by another mirror policy.")
		} else if ownerErr != nil && !store.IsNotFound(ownerErr) {
			return desired, false, nil, ownerErr
		}
	}
	for _, item := range wanted.Memberships {
		member, memberErr := s.store.GetActiveMirrorChannelModelOwnership(ctx, item.TargetSiteID, channel.ID, item.TargetModel)
		if memberErr == nil && !sameAdoptionMemberOwner(member, item) {
			conflict("membership_owned_elsewhere:"+item.TargetModel, "A selected channel model membership is owned by another mirror policy.")
		} else if memberErr != nil && !store.IsNotFound(memberErr) {
			return desired, false, nil, memberErr
		}
	}
	already := false
	if desired.Enabled && len(gates) == 0 {
		already = exactAdoptionOwnership(ctx, s.store, wanted)
		if !already {
			conflict("enabled_policy_without_exact_ownership", "The mirror policy is enabled without the exact adoption ownership projection.")
		}
	}
	return desired, already, gates, nil
}

func (s *Service) adoptionServiceGates(target domain.TargetSite) []ServiceGate {
	gates := make([]ServiceGate, 0, 4)
	add := func(code, message string) {
		gates = append(gates, ServiceGate{Code: code, Message: message, BlocksApply: true})
	}
	if !target.Enabled {
		add("target_disabled", "The target site is disabled.")
	}
	mode := strings.ToLower(strings.TrimSpace(target.WriteMode))
	if mode == "read_only" || mode == "readonly" {
		add("target_read_only", "The target site is configured read-only.")
	}
	if !s.remote.Enabled() {
		add("remote_writes_disabled", "Remote writes are disabled for this process.")
	}
	if s.visibility == nil {
		add("visibility_verifier_unavailable", "The target does not expose complete-mirror visibility receipts.")
	}
	return gates
}

func (s *Service) ensureAdoptionDraft(ctx context.Context, desired mirror.Policy) (mirror.Policy, error) {
	existing, err := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
	if err == nil {
		if existing.Enabled || !sameAdoptionPolicyDefinition(existing, desired) {
			return mirror.Policy{}, errors.New("existing adoption policy is enabled or has different settings")
		}
		return existing, nil
	}
	if !store.IsNotFound(err) {
		return mirror.Policy{}, err
	}
	if err := s.store.CreateMirrorPolicy(ctx, desired, s.now()); err != nil {
		existing, getErr := s.store.GetMirrorPolicy(ctx, desired.PolicyUUID)
		if getErr == nil && !existing.Enabled && sameAdoptionPolicyDefinition(existing, desired) {
			return existing, nil
		}
		return mirror.Policy{}, err
	}
	return desired, nil
}

func adoptionPolicyDocument(policy mirror.Policy) mirror.PolicyDocument {
	enabled := false
	groupRatio, modelPrices, billing := policy.SyncGroupRatio, policy.SyncModelPrices, policy.SyncBillingMode
	additions, removals := policy.SyncModelAdditions, policy.SyncModelRemovals
	return mirror.PolicyDocument{PolicyUUID: policy.PolicyUUID, PolicyVersion: 1,
		SchemaVersion: policy.SchemaVersion, SupplierID: policy.SupplierID, SourceGroup: policy.SourceGroup,
		SourceModelPattern: policy.SourceModelPattern, TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup,
		ExcludedModels: append([]string(nil), policy.ExcludedModels...),
		Enabled:        &enabled, Kind: policy.Kind, PriceAnchor: policy.PriceAnchor, SyncGroupRatio: &groupRatio,
		GroupRatioRule: policy.GroupRatioRule, SyncModelPrices: &modelPrices, ModelPriceRule: policy.ModelPriceRule,
		SyncBillingMode: &billing, SyncModelAdditions: &additions, SyncModelRemovals: &removals,
		ModelScopeMode: policy.ModelScopeMode, EmptyGroupAction: policy.EmptyGroupAction,
		ExecutionMode: policy.ExecutionMode, MinimumRatio: policy.MinimumRatio, MaximumRatio: policy.MaximumRatio,
		RoundingStep: policy.RoundingStep, OnlyFollowIncrease: policy.OnlyFollowIncrease,
		OnlyFollowDecrease: policy.OnlyFollowDecrease}
}

func adoptionOwnership(policy mirror.Policy, managed domain.TargetManagedChannelState, channel domain.TargetChannel,
	contract domain.GroupModelPricingContract) (store.MirrorOwnershipState, error) {
	membershipModels := canonicalAdoptionModels(managed.Models)
	channelModels := canonicalAdoptionModels(channel.Models)
	if strings.Join(membershipModels, "\x00") != strings.Join(channelModels, "\x00") {
		return store.MirrorOwnershipState{}, errors.New("adoption readback channel model sets differ")
	}
	group := contract.Groups[policy.TargetGroup]
	contractModels := canonicalContractModels(group)
	marker := "relayops:policy/" + policy.PolicyUUID
	source := fmt.Sprintf("supplier/%d/group/%s", policy.SupplierID, policy.SourceGroup)
	result := store.MirrorOwnershipState{Channel: store.MirrorChannelOwnership{PolicyUUID: policy.PolicyUUID,
		TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, ChannelID: channel.ID,
		OwnershipMarker: marker, SourceIdentity: source, ModelSetAuthority: "exclusive",
		StatusAuthority: "disable_only", LastAppliedFunctionalHash: managed.Hash}}
	for _, modelName := range contractModels {
		setting := group[modelName]
		if strings.TrimSpace(setting.Source) != marker {
			return store.MirrorOwnershipState{}, fmt.Errorf("adoption readback model %q has no exact policy marker", modelName)
		}
		modelHash, err := mirror.TargetModelFunctionalHash(policy.TargetGroup, modelName, setting)
		if err != nil {
			return store.MirrorOwnershipState{}, err
		}
		identity := source + "/model/" + modelName
		result.Models = append(result.Models, store.MirrorModelOwnership{PolicyUUID: policy.PolicyUUID,
			TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, TargetModel: modelName,
			OwnershipMarker: marker, SourceIdentity: identity, LastAppliedFunctionalHash: modelHash})
	}
	for _, modelName := range membershipModels {
		memberHash, err := mirror.TargetChannelModelFunctionalHash(channel.ID, modelName, true)
		if err != nil {
			return store.MirrorOwnershipState{}, err
		}
		identity := source + "/model/" + modelName
		result.Memberships = append(result.Memberships, store.MirrorChannelModelOwnership{PolicyUUID: policy.PolicyUUID,
			TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, ChannelID: channel.ID,
			TargetModel: modelName, OwnershipMarker: marker, SourceIdentity: identity,
			LastAppliedFunctionalHash: memberHash})
	}
	return result, nil
}

func readBackAdoption(ctx context.Context, target mirrorTarget, targetSite domain.TargetSite,
	preview AdoptionPreview) (domain.GroupModelPricingContract, domain.TargetManagedChannelState,
	domain.TargetChannel, error) {
	contract, err := target.ReadGroupModelPricing(ctx, targetSite)
	if err != nil {
		return contract, domain.TargetManagedChannelState{}, domain.TargetChannel{},
			fmt.Errorf("read adoption contract: %w", err)
	}
	if contract.Revision != preview.DesiredContractRevision {
		return contract, domain.TargetManagedChannelState{}, domain.TargetChannel{},
			errors.New("adoption contract readback differs from the preview")
	}
	managed, err := target.ReadManagedChannelState(ctx, targetSite, preview.Channel.ID)
	if err != nil {
		return contract, managed, domain.TargetChannel{}, fmt.Errorf("read adoption managed state: %w", err)
	}
	if managed.Hash != preview.ManagedState.Hash {
		return contract, managed, domain.TargetChannel{}, errors.New("adoption managed-state readback differs from the preview")
	}
	channels, err := target.ListChannels(ctx, targetSite)
	if err != nil {
		return contract, managed, domain.TargetChannel{}, err
	}
	channel, err := exactAdoptionChannel(channels, preview.Channel.ID, preview.Policy.TargetGroup)
	if err != nil {
		return contract, managed, channel, err
	}
	if channel.ConfigHash != preview.Channel.ConfigHash {
		return contract, managed, channel, errors.New("adoption channel configuration changed after preview")
	}
	return contract, managed, channel, nil
}

func exactAdoptionChannel(channels []domain.TargetChannel, channelID int64, targetGroup string) (domain.TargetChannel, error) {
	var result *domain.TargetChannel
	for index := range channels {
		if channels[index].ID == channelID {
			if result != nil {
				return domain.TargetChannel{}, errors.New("selected channel identity is ambiguous")
			}
			copy := channels[index]
			result = &copy
		}
	}
	if result == nil || strings.TrimSpace(result.ConfigHash) == "" {
		return domain.TargetChannel{}, errors.New("selected channel is absent or has no configuration hash")
	}
	matchedGroup := false
	for _, group := range strings.Split(result.Group, ",") {
		if strings.EqualFold(strings.TrimSpace(group), strings.TrimSpace(targetGroup)) {
			matchedGroup = true
			break
		}
	}
	if !matchedGroup {
		return domain.TargetChannel{}, errors.New("selected channel is not assigned to the target group")
	}
	result.Models = canonicalAdoptionModels(result.Models)
	return *result, nil
}

func cloneAdoptionContract(source domain.GroupModelPricingContract) (domain.GroupModelPricingContract, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	var result domain.GroupModelPricingContract
	if err := json.Unmarshal(payload, &result); err != nil {
		return domain.GroupModelPricingContract{}, err
	}
	return result, nil
}

func canonicalAdoptionModels(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
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

func canonicalContractModels(values map[string]domain.GroupModelPricingSetting) []string {
	result := make([]string, 0, len(values))
	for modelName := range values {
		if modelName = strings.TrimSpace(modelName); modelName != "" {
			result = append(result, modelName)
		}
	}
	sort.Strings(result)
	return result
}

func sameAdoptionPolicyDefinition(left, right mirror.Policy) bool {
	left, right = normalizedAdoptionPolicyDefinition(left), normalizedAdoptionPolicyDefinition(right)
	return reflect.DeepEqual(left, right)
}

func normalizedAdoptionPolicyDefinition(policy mirror.Policy) mirror.Policy {
	policy.PolicyVersion = 1
	policy.Enabled = false
	policy.LegacyPolicyID = 0
	policy.ExcludedModels = canonicalAdoptionModels(policy.ExcludedModels)
	return policy
}

func adoptionPolicyDefinitionMismatch(left, right mirror.Policy) string {
	left, right = normalizedAdoptionPolicyDefinition(left), normalizedAdoptionPolicyDefinition(right)
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	policyType := leftValue.Type()
	fields := make([]string, 0)
	for index := 0; index < leftValue.NumField(); index++ {
		if !reflect.DeepEqual(leftValue.Field(index).Interface(), rightValue.Field(index).Interface()) {
			fields = append(fields, policyType.Field(index).Name)
		}
	}
	if len(fields) == 0 {
		return "unknown"
	}
	return strings.Join(fields, ",")
}

func sameAdoptionChannelOwner(left, right store.MirrorChannelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.ChannelID == right.ChannelID &&
		left.OwnershipMarker == right.OwnershipMarker && left.SourceIdentity == right.SourceIdentity &&
		left.ModelSetAuthority == right.ModelSetAuthority && left.StatusAuthority == right.StatusAuthority &&
		left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func sameAdoptionModelOwner(left, right store.MirrorModelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.TargetModel == right.TargetModel &&
		left.OwnershipMarker == right.OwnershipMarker && left.SourceIdentity == right.SourceIdentity &&
		left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func sameAdoptionMemberOwner(left, right store.MirrorChannelModelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.ChannelID == right.ChannelID &&
		left.TargetModel == right.TargetModel && left.OwnershipMarker == right.OwnershipMarker &&
		left.SourceIdentity == right.SourceIdentity && left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func exactAdoptionOwnership(ctx context.Context, st *store.Store, wanted store.MirrorOwnershipState) bool {
	channels, err := st.ListActiveMirrorChannelOwnerships(ctx, wanted.Channel.PolicyUUID)
	if err != nil || len(channels) != 1 || !sameAdoptionChannelOwner(channels[0], wanted.Channel) {
		return false
	}
	models, err := st.ListActiveMirrorModelOwnerships(ctx, wanted.Channel.PolicyUUID)
	if err != nil || len(models) != len(wanted.Models) {
		return false
	}
	members, err := st.ListActiveMirrorChannelModelOwnerships(ctx, wanted.Channel.PolicyUUID)
	if err != nil || len(members) != len(wanted.Memberships) {
		return false
	}
	modelByName := make(map[string]store.MirrorModelOwnership, len(models))
	for _, item := range models {
		if _, duplicate := modelByName[item.TargetModel]; duplicate {
			return false
		}
		modelByName[item.TargetModel] = item
	}
	memberByName := make(map[string]store.MirrorChannelModelOwnership, len(members))
	for _, item := range members {
		if _, duplicate := memberByName[item.TargetModel]; duplicate {
			return false
		}
		memberByName[item.TargetModel] = item
	}
	for _, item := range wanted.Models {
		if !sameAdoptionModelOwner(modelByName[item.TargetModel], item) {
			return false
		}
	}
	for _, item := range wanted.Memberships {
		if !sameAdoptionMemberOwner(memberByName[item.TargetModel], item) {
			return false
		}
	}
	return true
}

func boolValue(value bool) *bool { return &value }

func hashAdoptionPreview(preview AdoptionPreview) (string, error) {
	preview.ID = ""
	preview.GeneratedAt = time.Time{}
	payload, err := json.Marshal(preview)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func firstAdoptionBlockingError(preview AdoptionPreview) error {
	for _, gate := range preview.ServiceGates {
		if gate.BlocksApply {
			return errors.New(gate.Message)
		}
	}
	return errors.New("existing-channel adoption preview is not executable")
}

func (s *Service) rememberAdoption(entry cachedAdoptionPreview) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	now := s.now()
	for id, candidate := range s.adoptionPreviews {
		if !candidate.expiresAt.After(now) {
			delete(s.adoptionPreviews, id)
		}
	}
	if len(s.adoptionPreviews) >= s.options.PreviewCapacity {
		oldestID := ""
		var oldest time.Time
		for id, candidate := range s.adoptionPreviews {
			if oldestID == "" || candidate.expiresAt.Before(oldest) {
				oldestID, oldest = id, candidate.expiresAt
			}
		}
		delete(s.adoptionPreviews, oldestID)
	}
	s.adoptionPreviews[entry.public.ID] = entry
}

func (s *Service) cachedAdoption(id string) (cachedAdoptionPreview, error) {
	id = strings.TrimSpace(id)
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	entry, ok := s.adoptionPreviews[id]
	if !ok || !entry.expiresAt.After(s.now()) {
		delete(s.adoptionPreviews, id)
		return cachedAdoptionPreview{}, ErrPreviewNotFound
	}
	return entry, nil
}
