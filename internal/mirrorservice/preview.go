package mirrorservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorexec"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func (s *Service) PreviewPolicy(ctx context.Context, request PreviewRequest) (Preview, error) {
	policy, err := s.store.GetMirrorPolicy(ctx, strings.TrimSpace(request.PolicyUUID))
	if err != nil {
		return Preview{}, err
	}
	entry, err := s.buildPreview(ctx, policy)
	if err != nil {
		return Preview{}, err
	}
	s.remember(entry)
	return entry.public, nil
}

func (s *Service) GetPreview(id string) (Preview, error) {
	entry, err := s.cached(id)
	if err != nil {
		return Preview{}, err
	}
	return entry.public, nil
}

func (s *Service) ApplyPreview(ctx context.Context, request ApplyRequest) (ApplyReceipt, error) {
	entry, err := s.cached(request.PreviewID)
	if err != nil {
		return ApplyReceipt{}, err
	}
	if err := s.remote.Check("complete mirror apply"); err != nil {
		return ApplyReceipt{}, err
	}
	currentPolicy, err := s.store.GetMirrorPolicy(ctx, entry.public.Policy.PolicyUUID)
	if err != nil {
		return ApplyReceipt{}, err
	}
	if currentPolicy.PolicyVersion != entry.public.Policy.PolicyVersion {
		return ApplyReceipt{}, ErrPreviewStale
	}
	// The preview page may reuse shared target reads, but the final plan bound to
	// a mutation must be rebuilt from fresh target state.
	current, err := s.buildPreview(targets.WithFreshRead(ctx), currentPolicy)
	if err != nil {
		return ApplyReceipt{}, err
	}
	if current.public.Policy.PolicyVersion != entry.public.Policy.PolicyVersion {
		return ApplyReceipt{}, ErrPreviewStale
	}
	if current.public.Plan.NoChange {
		return ApplyReceipt{PreviewID: current.public.ID, Replayed: true}, nil
	}
	if !current.public.Executable {
		return ApplyReceipt{}, firstBlockingError(current.public)
	}
	if err := requireEvidenceApproval(current.public, request.ApprovalGranted); err != nil {
		return ApplyReceipt{}, err
	}
	if current.public.Plan.Decision == mirror.DecisionApprovalRequired && !request.ApprovalGranted {
		return ApplyReceipt{}, errors.New("mirror preview requires explicit administrator approval")
	}
	targetSite, err := s.store.GetTarget(ctx, current.public.Policy.TargetSiteID)
	if err != nil {
		return ApplyReceipt{}, err
	}
	target, err := s.target(ctx, targetSite)
	if err != nil {
		return ApplyReceipt{}, err
	}
	executor, err := mirrorexec.New(s.store, target, s.visibility, mirrorexec.Options{
		WorkerID: "mirrorservice", Now: s.options.Now,
	})
	if err != nil {
		return ApplyReceipt{}, err
	}
	receipt, err := executor.Apply(ctx, mirrorexec.ApplyInput{
		Policy: current.public.Policy, Plan: current.public.Plan, Target: targetSite,
		ChannelID: current.public.ChannelID, SourceProof: current.sourceProof,
		SourceIdentity:  current.sourceIdentity,
		OperationKey:    "mirror:" + current.public.Policy.PolicyUUID + ":" + current.public.ID,
		ApprovalGranted: request.ApprovalGranted,
	})
	if err != nil {
		return ApplyReceipt{PreviewID: current.public.ID, Receipt: receipt.ChangeSet, Replayed: receipt.Replayed}, err
	}
	return ApplyReceipt{PreviewID: current.public.ID, Receipt: receipt.ChangeSet, Replayed: receipt.Replayed}, nil
}

func (s *Service) buildPreview(ctx context.Context, policy mirror.Policy) (cachedPreview, error) {
	policy = mirror.NormalizeOwnership(policy)
	if err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		return cachedPreview{}, err
	}
	if err := s.validateBindings(ctx, policy); err != nil {
		return cachedPreview{}, err
	}
	ownership, err := s.channelOwnership(ctx, policy)
	if err != nil {
		return cachedPreview{}, err
	}
	supplier, err := s.store.GetSupplier(ctx, policy.SupplierID)
	if err != nil {
		return cachedPreview{}, err
	}
	targetSite, err := s.store.GetTarget(ctx, policy.TargetSiteID)
	if err != nil {
		return cachedPreview{}, err
	}
	target, err := s.target(ctx, targetSite)
	if err != nil {
		return cachedPreview{}, err
	}

	snapshot, err := s.store.LatestEffectiveSnapshot(ctx, policy.SupplierID)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read authoritative source snapshot: %w", err)
	}
	source, state, semanticHash, err := sourceFromSnapshot(snapshot, policy.SourceGroup)
	if err != nil {
		return cachedPreview{}, err
	}
	proofs, err := s.store.ListCatalogGroupCollectionProofs(ctx, policy.SupplierID, policy.SourceGroup,
		s.options.ProofLimit)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read source group proofs: %w", err)
	}
	boundProof, err := proofBoundToSnapshot(proofs, state, semanticHash, snapshot.Evidence.ResponseHash)
	if err != nil {
		boundProof, err = s.store.FindCatalogGroupCollectionProof(ctx, policy.SupplierID, policy.SourceGroup,
			semanticHash, snapshot.Evidence.ResponseHash)
		if err != nil {
			if store.IsNotFound(err) {
				return cachedPreview{}, sourceEvidencePending("source group proof is not yet bound to the effective snapshot")
			}
			if errors.Is(err, store.ErrCatalogGroupProofIncomplete) {
				return cachedPreview{}, sourceEvidencePending("the latest catalog collection is incomplete; the previous target state is retained")
			}
			return cachedPreview{}, fmt.Errorf("read exact source group proof: %w", err)
		}
	}

	targetSnapshot, err := target.Snapshot(ctx, targetSite)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read target GroupRatio: %w", err)
	}
	groupRatioOption, ok := targetSnapshot.PricingOptions["GroupRatio"]
	if !ok {
		return cachedPreview{}, errors.New("target does not expose GroupRatio")
	}
	currentGroupRatio, ok := groupRatioOption.Values[policy.TargetGroup]
	if !ok || strings.TrimSpace(currentGroupRatio) == "" {
		return cachedPreview{}, fmt.Errorf("target GroupRatio has no exact group %q", policy.TargetGroup)
	}
	contract, err := target.ReadGroupModelPricing(ctx, targetSite)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read target group-model contract: %w", err)
	}
	channel, err := target.ReadManagedChannelState(ctx, targetSite, ownership.ChannelID)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read managed channel: %w", err)
	}
	// A complete mirror owns convergence, not the administrator's last edit.
	// The fresh channel state below is frozen into the preview and checked again
	// by the executor, so an older ownership receipt must not block reconciliation.
	channels, err := target.ListChannels(ctx, targetSite)
	if err != nil {
		return cachedPreview{}, fmt.Errorf("read conservative target channel references: %w", err)
	}
	owned, err := s.ownedModelState(ctx, policy, ownership, channel, contract, channels)
	if err != nil {
		return cachedPreview{}, err
	}

	plan, err := mirror.PlanCompleteMirror(mirror.CompleteMirrorPlannerInput{
		Policy: policy, SourceState: state, SourceGroup: source,
		SourceRevision: boundProof.SourceRevision, SourceObservedAtUnixMS: boundProof.ObservedAt.UnixMilli(),
		Factors: mirror.CashFactors{RawGroupRatio: source.RawGroupRatio,
			RechargeValueFactor: source.RechargeValueFactor, SourceCashFactor: "1",
			TargetRealizationFactor: "1", RiskBufferRate: "0"},
		CurrentGroupRatio: currentGroupRatio, CurrentContract: contract,
		CurrentChannelModels: channel.Models, CurrentChannelEnabled: channel.Status == 1,
		ChannelDisabledByPolicy: channel.Status == 2 && ownership.StatusAuthority == "disable_only",
		ChannelStatusAuthority:  ownership.StatusAuthority, OwnedModels: owned,
	})
	if err != nil {
		return cachedPreview{}, err
	}
	if plan.SourceSemanticHash != semanticHash {
		return cachedPreview{}, errors.New("planner source semantic hash differs from the authoritative group proof")
	}

	confirmations, evidenceReady, err := s.confirmPlan(plan, state, semanticHash, proofs)
	if err != nil {
		return cachedPreview{}, err
	}
	now := s.now()
	preview := Preview{Policy: policy, ChannelID: ownership.ChannelID, SourceState: state,
		SourceRevision: boundProof.SourceRevision, SourceSemanticHash: semanticHash,
		SourceObservedAt: boundProof.ObservedAt, SnapshotResponseHash: snapshot.Evidence.ResponseHash,
		Confirmations: confirmations, EvidenceReady: evidenceReady, Plan: plan, GeneratedAt: now}
	preview.ServiceGates = s.serviceGates(policy, targetSite, plan, evidenceReady)
	preview.Executable = !plan.NoChange && !hasBlockingServiceGate(preview.ServiceGates)
	preview.ID, err = hashPreview(preview)
	if err != nil {
		return cachedPreview{}, err
	}
	proofJSON, err := json.Marshal(sourceEvidence{SupplierID: supplier.ID, SourceGroup: policy.SourceGroup,
		SourceState: state, SourceSemanticHash: semanticHash, SnapshotResponseHash: snapshot.Evidence.ResponseHash,
		Proofs: proofs})
	if err != nil {
		return cachedPreview{}, err
	}
	return cachedPreview{public: preview, sourceProof: proofJSON,
		sourceIdentity: fmt.Sprintf("supplier/%d/group/%s", policy.SupplierID, policy.SourceGroup),
		expiresAt:      now.Add(s.options.PreviewTTL)}, nil
}

func sourceFromSnapshot(snapshot domain.CatalogSnapshot, binding string) (mirror.CanonicalGroup,
	mirror.SourceGroupState, string, error) {
	binding = strings.TrimSpace(binding)
	var matched *domain.GroupOffer
	for index := range snapshot.Groups {
		group := &snapshot.Groups[index]
		identity := strings.TrimSpace(group.ExternalID)
		if identity == "" {
			identity = strings.TrimSpace(group.Name)
		}
		if identity != binding {
			continue
		}
		if matched != nil {
			return mirror.CanonicalGroup{}, "", "", errors.New("source group identity is ambiguous")
		}
		matched = group
	}
	if matched == nil {
		hash, err := mirror.MissingSourceSemanticHash(binding)
		return mirror.CanonicalGroup{}, mirror.SourceGroupMissing, hash, err
	}
	if len(matched.ModelIDs) == 0 {
		groupID := strings.TrimSpace(matched.ExternalID)
		if groupID == "" {
			groupID = strings.TrimSpace(matched.Name)
		}
		source := mirror.CanonicalGroup{SourceGroupID: groupID, SourceGroupName: strings.TrimSpace(matched.Name),
			RawGroupRatio:       firstNonEmpty(matched.RawRatio, matched.Ratio),
			RechargeValueFactor: firstNonEmpty(matched.RechargeValueFactor, "1")}
		hash, err := mirror.EmptyCanonicalSourceSemanticHash(source)
		return source, mirror.SourceGroupEmpty, hash, err
	}
	source, err := mirror.NormalizeSourceGroup(snapshot, *matched)
	if err != nil {
		return mirror.CanonicalGroup{}, "", "", err
	}
	hash, err := mirror.SourceSemanticHash(source)
	return source, mirror.SourceGroupPresent, hash, err
}

func proofBoundToSnapshot(proofs []mirror.CollectionProof, state mirror.SourceGroupState, semanticHash,
	responseHash string) (mirror.CollectionProof, error) {
	responseHash = strings.TrimSpace(responseHash)
	if responseHash == "" {
		return mirror.CollectionProof{}, errors.New("authoritative snapshot has no response hash")
	}
	for _, proof := range proofs {
		if proof.SourceState == string(state) && proof.SourceSemanticHash == semanticHash &&
			proof.ResponseHash == responseHash {
			return proof, nil
		}
	}
	return mirror.CollectionProof{}, errors.New("source group proof is not bound to the effective snapshot response hash")
}

func (s *Service) ownedModelState(ctx context.Context, policy mirror.Policy, ownership store.MirrorChannelOwnership,
	channel domain.TargetManagedChannelState, contract domain.GroupModelPricingContract,
	channels []domain.TargetChannel) ([]mirror.OwnedModelState, error) {
	models, err := s.store.ListActiveMirrorModelOwnerships(ctx, policy.PolicyUUID)
	if err != nil {
		return nil, err
	}
	memberships, err := s.store.ListActiveMirrorChannelModelOwnerships(ctx, policy.PolicyUUID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]mirror.OwnedModelState, len(models)+len(memberships))
	group := contract.Groups[policy.TargetGroup]
	channelModels := stringSet(channel.Models)
	for _, item := range models {
		setting, ok := group[item.TargetModel]
		if ok && setting.Source != ownership.OwnershipMarker {
			return nil, fmt.Errorf("owned target contract model %q is foreign", item.TargetModel)
		}
		state := result[item.TargetModel]
		state.TargetModel, state.ContractOwned = item.TargetModel, ok
		result[item.TargetModel] = state
	}
	for _, item := range memberships {
		_, present := channelModels[item.TargetModel]
		state := result[item.TargetModel]
		state.TargetModel, state.MembershipOwned = item.TargetModel, present
		result[item.TargetModel] = state
	}
	for modelName, state := range result {
		state.ForeignChannelReferences = foreignChannelReferences(channels, ownership.ChannelID, modelName)
		result[modelName] = state
	}
	ordered := make([]mirror.OwnedModelState, 0, len(result))
	for _, state := range result {
		ordered = append(ordered, state)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TargetModel < ordered[j].TargetModel })
	return ordered, nil
}

func foreignChannelReferences(channels []domain.TargetChannel, ownedChannelID int64, model string) int {
	count := 0
	for _, channel := range channels {
		if channel.ID == ownedChannelID {
			continue
		}
		for _, candidate := range channel.Models {
			if strings.TrimSpace(candidate) == model {
				count++
				break
			}
		}
	}
	return count
}

func (s *Service) serviceGates(policy mirror.Policy, target domain.TargetSite,
	plan mirror.CompleteMirrorPlan, evidenceReady bool) []ServiceGate {
	gates := make([]ServiceGate, 0, 8)
	add := func(code, message string, blocks bool) {
		gates = append(gates, ServiceGate{Code: code, Message: message, BlocksApply: blocks})
	}
	if !policy.Enabled {
		add("policy_disabled", "The complete-mirror policy is disabled.", true)
	}
	if !evidenceReady {
		add("source_evidence_unconfirmed", "当前完整采集可由管理员立即确认同步；后台自动同步会等待下一次相同采集。", false)
	}
	if plan.Blocked || plan.Decision == mirror.DecisionBlocked {
		add("planner_blocked", "The deterministic mirror planner reported a hard safety gate.", true)
	}
	if !target.Enabled {
		add("target_disabled", "The target site is disabled.", true)
	}
	writeMode := strings.ToLower(strings.TrimSpace(target.WriteMode))
	if writeMode == "read_only" || writeMode == "readonly" {
		add("target_read_only", "The target site is configured read-only.", true)
	}
	if !s.remote.Enabled() {
		add("remote_writes_disabled", "Remote writes are disabled for this process.", true)
	}
	if !s.automaticWrites.Load() {
		add("automatic_writes_disabled", "Global unattended automation is disabled; an approved manual apply remains possible when other gates pass.", false)
	}
	if writeMode != "auto" {
		add("target_not_automatic", "The target does not permit unattended writes; an approved manual apply remains possible.", false)
	}
	sort.Slice(gates, func(i, j int) bool { return gates[i].Code < gates[j].Code })
	return gates
}

func hasBlockingServiceGate(gates []ServiceGate) bool {
	for _, gate := range gates {
		if gate.BlocksApply {
			return true
		}
	}
	return false
}

func firstBlockingError(preview Preview) error {
	for _, gate := range preview.ServiceGates {
		if gate.BlocksApply {
			switch gate.Code {
			case "source_evidence_unconfirmed":
				return ErrEvidenceUnconfirmed
			case "visibility_verifier_unavailable":
				return ErrVisibilityUnavailable
			default:
				return errors.New(gate.Message)
			}
		}
	}
	return mirrorexec.ErrPlanNotExecutable
}

func requireEvidenceApproval(preview Preview, approvalGranted bool) error {
	if !preview.EvidenceReady && !approvalGranted {
		return ErrEvidenceUnconfirmed
	}
	return nil
}

func hashPreview(preview Preview) (string, error) {
	preview.ID = ""
	preview.GeneratedAt = time.Time{}
	encoded, err := json.Marshal(preview)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
