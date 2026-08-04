package listing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/decimalnorm"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type Service struct {
	store        *store.Store
	targets      *targets.Factory
	credentials  *credentials.Service
	remoteWrites *remotewrite.Gate
	locksMu      sync.Mutex
	targetLocks  map[int64]*sync.Mutex
}

type ApplyInput struct {
	IdempotencyKey string `json:"idempotency_key"`
	ConfirmEnable  bool   `json:"confirm_enable"`
}

type RollbackInput struct {
	ConfirmRollback bool `json:"confirm_rollback"`
}

var ErrRollbackConflict = errors.New("listing rollback conflict")

const (
	catalogListingFreshnessWindow = 10 * time.Minute
	catalogFutureClockTolerance   = 5 * time.Minute
)

type staleError struct{ message string }

func (err staleError) Error() string { return err.message }

func NewService(st *store.Store, targetFactory *targets.Factory, credentialService *credentials.Service) *Service {
	return &Service{store: st, targets: targetFactory, credentials: credentialService, targetLocks: make(map[int64]*sync.Mutex)}
}

func (s *Service) SetRemoteWriteGate(gate *remotewrite.Gate) {
	s.remoteWrites = gate
}

func (s *Service) Preview(ctx context.Context, request domain.ListingPlanRequest) (domain.ListingPlan, error) {
	request = normalizeRequest(request)
	if err := validateRequest(request); err != nil {
		return domain.ListingPlan{}, err
	}
	request.PricingRule.MirrorPolicyUUID = listingMirrorPolicyUUID(request)
	supplier, err := s.store.GetSupplier(ctx, request.SupplierID)
	if err != nil {
		return domain.ListingPlan{}, fmt.Errorf("supplier: %w", err)
	}
	snapshot, group, err := s.resolvePreviewSource(ctx, supplier, request)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	if listingUsesCompleteMirror(request.PricingRule) &&
		((*request.PricingRule.SyncModelAdditions) || (*request.PricingRule.SyncModelRemovals)) {
		groupModels := sortedUnique(group.ModelIDs)
		if strings.Join(groupModels, "\x00") != strings.Join(request.Channel.Models, "\x00") {
			return domain.ListingPlan{}, errors.New("complete model-set synchronization requires selecting every model in the upstream group; disable add/remove synchronization to manage a fixed subset")
		}
	}
	sourceRatio, err := nonnegativeDecimal(ratios.DecisionRatio(group), "source group ratio")
	if err != nil {
		return domain.ListingPlan{}, err
	}
	if err := validateSupplierChannelOrigin(supplier, request.Channel.BaseURL); err != nil {
		return domain.ListingPlan{}, err
	}
	if _, _, err := s.loadUpstreamKey(ctx, supplier, request.UpstreamCredentialID, request.Channel.BaseURL); err != nil {
		return domain.ListingPlan{}, err
	}

	target, adapter, channelAdapter, targetSnapshot, err := s.loadTarget(ctx, request.TargetSiteID)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	current, existed := targetSnapshot.GroupRatio[request.TargetGroup]
	visibilityPlan := domain.ListingGroupVisibilityPlan{}
	switch request.TargetGroupMode {
	case "new":
		if existed {
			return domain.ListingPlan{}, fmt.Errorf("target group %q already exists; choose existing mode", request.TargetGroup)
		}
		if listingUsesCompleteMirror(request.PricingRule) {
			visibilityPlan.Required = true
			visibilityAdapter, visibilityErr := targets.AsGroupVisibilityAdapter(adapter)
			if visibilityErr != nil || !targetSnapshot.UserUsableGroupsSupported {
				return domain.ListingPlan{}, errors.New("new complete-mirror groups require readable and writable NewAPI UserUsableGroups support")
			}
			if _, visibilityErr = visibilityAdapter.ReadGroupVisibility(ctx, target); visibilityErr != nil {
				return domain.ListingPlan{}, fmt.Errorf("read target user-visible groups: %w", visibilityErr)
			}
			visibilityPlan.Supported = true
			visibilityPlan.Expected = cloneRatios(targetSnapshot.UserUsableGroups)
			visibilityPlan.Desired = cloneRatios(targetSnapshot.UserUsableGroups)
			if _, alreadyVisible := visibilityPlan.Desired[request.TargetGroup]; !alreadyVisible {
				visibilityPlan.Desired[request.TargetGroup] = request.TargetGroup
			}
			visibilityPlan.ExpectedHash = targets.HashStringMap(visibilityPlan.Expected)
			visibilityPlan.DesiredHash = targets.HashStringMap(visibilityPlan.Desired)
			visibilityPlan.Changed = visibilityPlan.ExpectedHash != visibilityPlan.DesiredHash
		}
	case "existing":
		if !existed {
			return domain.ListingPlan{}, fmt.Errorf("target group %q does not exist; choose new mode", request.TargetGroup)
		}
	}
	targetCatalog, err := channelAdapter.PricingCatalog(ctx, target)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	targetCatalogHash := pricing.ModelPriceHash(targetCatalog)
	plan := domain.ListingPlan{
		SupplierID: request.SupplierID, DiscoveryKind: request.DiscoveryKind, DiscoveryExternalID: request.DiscoveryExternalID,
		SourceSnapshotHash: snapshot.Evidence.ResponseHash, SourceObservedAt: snapshot.Evidence.ObservedAt,
		SourceGroup: group.ExternalID, SourceRatio: sourceRatio.String(), TargetCatalogHash: targetCatalogHash,
		TargetSiteID: request.TargetSiteID, TargetGroupMode: request.TargetGroupMode, TargetGroup: request.TargetGroup,
		ExpectedTargetHash: targetSnapshot.Hash, ExpectedGroupRatios: cloneRatios(targetSnapshot.GroupRatio),
		ExpectedGroupExisted: existed, ExpectedGroupRatio: current, UpstreamCredentialID: request.UpstreamCredentialID,
		Channel: request.Channel, PricingRule: request.PricingRule,
		Status: "ready", IdempotencyKey: request.IdempotencyKey,
	}
	if targetSnapshot.GroupModelPricingSupported {
		if targetSnapshot.GroupModelPricing == nil {
			return domain.ListingPlan{}, errors.New("target reported group-model pricing support without a readable contract")
		}
		targetGroupRatio := current
		if request.TargetGroupMode == "new" {
			targetGroupRatio = "1"
		}
		contractPlan, planErr := PlanGroupModelPricing(GroupModelPricingPlanInput{
			SupplierID: request.SupplierID, SourceCatalog: snapshot, TargetCatalog: targetCatalog,
			SourceGroup: group, Models: request.Channel.Models,
			TargetGroup: request.TargetGroup, TargetGroupRatio: targetGroupRatio, PricingRule: request.PricingRule,
			Expected: *targetSnapshot.GroupModelPricing, OwnershipSource: listingMirrorOwnershipMarker(request.PricingRule),
		})
		if planErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("group-model pricing preview: %w", planErr)
		}
		if len(contractPlan.ExcludedModels) > 0 {
			plan.Channel.Models = subtractModels(plan.Channel.Models, contractPlan.ExcludedModels)
			plan.PricingRule.MirrorExcludedModels = sortedUnique(append(
				append([]string(nil), plan.PricingRule.MirrorExcludedModels...), contractPlan.ExcludedModels...))
		}
		targetGroupRatio = contractPlan.TargetGroupRatio
		groupModelAdapter, adapterErr := targets.AsGroupModelPricingAdapter(adapter)
		if adapterErr != nil {
			return domain.ListingPlan{}, adapterErr
		}
		remotePreview, previewErr := groupModelAdapter.PreviewGroupModelPricing(ctx, target,
			targetSnapshot.GroupModelPricing.Revision, contractPlan.DesiredContract)
		if previewErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("preview target group-model pricing: %w", previewErr)
		}
		if !remotePreview.ExpectedRevisionMatches || remotePreview.CurrentRevision != targetSnapshot.GroupModelPricing.Revision {
			return domain.ListingPlan{}, errors.New("target group-model pricing changed while the listing preview was being prepared")
		}
		contractPlan.ExpectedRevision = remotePreview.CurrentRevision
		contractPlan.DesiredRevision = remotePreview.DesiredRevision
		contractPlan.ExpectedContract = remotePreview.Current
		contractPlan.DesiredContract = remotePreview.Desired
		contractPlan.Diff = remotePreview.Diff
		contractPlan.GroupVisibility = visibilityPlan
		globalModelPrice, globalPriceErr := planMissingGlobalModelPrices(contractPlan, targetSnapshot)
		if globalPriceErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("plan missing global model prices: %w", globalPriceErr)
		}
		contractPlan.GlobalModelPrice = globalModelPrice
		desiredRatios := cloneRatios(targetSnapshot.GroupRatio)
		desiredRatios[request.TargetGroup] = targetGroupRatio
		calculationPolicy := listingModelPricePolicy(request.PricingRule,
			mirror.Rule{Type: contractPlan.ModelPriceRuleType, Value: contractPlan.ModelPriceRuleValue})
		if contractPlan.PriceAnchor == mirror.PriceAnchorSourceBase {
			calculationPolicy = listingGroupRatioPolicy(request.PricingRule,
				mirror.Rule{Type: contractPlan.GroupRatioRuleType, Value: contractPlan.GroupRatioRuleValue})
		}
		calculation, calculationErr := pricing.Calculate(pricing.Input{SourceRatio: sourceRatio.String(), CostRatio: sourceRatio.String(),
			CurrentRatio: current, Policy: calculationPolicy})
		if calculationErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("calculate listing summary: %w", calculationErr)
		}
		calculation.TargetRatio = targetGroupRatio
		if contractPlan.MinimumMarginDefined {
			calculation.MarginRate = stringPointer(contractPlan.MinimumMarginRate)
		} else {
			calculation.MarginRate = nil
		}
		if contractPlan.PriceAnchor == mirror.PriceAnchorSourceBase {
			calculation.Formula = "target model base prices follow upstream base prices; GroupRatio is calculated independently from the upstream effective group ratio"
			calculation.Warnings = append(calculation.Warnings, "Model base prices and billing units follow the upstream group; GroupRatio is the administrator profit-control multiplier")
		} else {
			calculation.Formula = "desired effective sale is divided by the current GroupRatio to derive each target model base price"
			calculation.Warnings = append(calculation.Warnings, "The requested final sale or margin is authoritative; later GroupRatio edits may be offset by the next model-price synchronization")
		}
		plan.EquivalentCostRatio = sourceRatio.String()
		plan.CostRatioVector = listingCostVector(contractPlan.Models)
		plan.ProfitVerified = true
		plan.ProfitVerification = "every selected model and monetary dimension has a frozen source cash cost, target base price, billing unit and post-rounding margin"
		plan.ProposedGroupRatio = targetGroupRatio
		plan.DesiredTargetHash = targets.HashRatios(desiredRatios)
		plan.Calculation = calculation
		plan.GroupModelPricing = contractPlan
	} else {
		if listingUsesCompleteMirror(request.PricingRule) {
			return domain.ListingPlan{}, errors.New("complete group mirroring requires target group-model pricing and managed-channel CAS; this target only supports the legacy price representation")
		}
		impactTargetGroup := request.TargetGroup
		if request.TargetGroupMode == "new" {
			impactTargetGroup = ""
		}
		impact, impactErr := pricing.ResolvePricingImpactForTargetGroup(snapshot, targetCatalog, group,
			impactTargetGroup, request.Channel.Models)
		if impactErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("profit verification failed: %w", impactErr)
		}
		if !impact.Complete || !uniformCostVector(impact.CostRatioVector) {
			reason := "the target does not support atomic group-model pricing and the selected model dimensions are not exactly representable by one isolated GroupRatio"
			if targetSnapshot.GroupModelPricingError != "" {
				reason += ": " + targetSnapshot.GroupModelPricingError
			}
			return domain.ListingPlan{}, errors.New(reason)
		}
		calculation, calculationErr := pricing.Calculate(pricing.Input{SourceRatio: impact.EquivalentCostRatio, CurrentRatio: current,
			Policy: listingRulePolicy(request.PricingRule)})
		if calculationErr != nil {
			return domain.ListingPlan{}, fmt.Errorf("calculate listing ratio: %w", calculationErr)
		}
		desiredRatios := cloneRatios(targetSnapshot.GroupRatio)
		desiredRatios[request.TargetGroup] = calculation.TargetRatio
		plan.EquivalentCostRatio = impact.EquivalentCostRatio
		plan.CostRatioVector = impact.CostRatioVector
		plan.ProfitVerified = true
		plan.ProfitVerification = "legacy target verified as an exact proportional model-price vector"
		plan.ProposedGroupRatio = calculation.TargetRatio
		plan.DesiredTargetHash = targets.HashRatios(desiredRatios)
		plan.Calculation = calculation
		plan.GroupModelPricing = domain.ListingGroupModelPricingPlan{Mode: "legacy_group_ratio", Supported: false, Exact: true,
			TargetGroupRatio: calculation.TargetRatio, LegacyFallbackReason: "target has no atomic group-model pricing endpoint; exact proportional fallback is safe"}
		plan.GroupModelPricing.GroupVisibility = visibilityPlan
	}
	if listingUsesPricingMapping(plan.PricingRule) {
		if existingPolicy, policyErr := s.store.GetPolicyByTarget(ctx, request.TargetSiteID, request.TargetGroup); policyErr == nil {
			if !equivalentPolicyDefinition(existingPolicy, listingPolicyForPlan(plan)) {
				create := false
				plan.PricingRule.CreatePricingMapping = &create
				plan.Calculation.Warnings = append(plan.Calculation.Warnings,
					fmt.Sprintf("已保留现有价格映射“%s”；本次只创建分组镜像，不会重复创建或覆盖分组倍率规则。", existingPolicy.Name))
			}
		} else if !store.IsNotFound(policyErr) {
			return domain.ListingPlan{}, policyErr
		}
	}
	if existing, getErr := s.store.GetListingPlanByIdempotency(ctx, request.IdempotencyKey); getErr == nil {
		if !equivalentPlans(existing, plan) {
			return domain.ListingPlan{}, errors.New("idempotency key already belongs to a different listing preview")
		}
		return existing, nil
	} else if !store.IsNotFound(getErr) {
		return domain.ListingPlan{}, getErr
	}
	if err := s.store.CreateListingPlan(ctx, &plan); err != nil {
		if existing, getErr := s.store.GetListingPlanByIdempotency(ctx, request.IdempotencyKey); getErr == nil && equivalentPlans(existing, plan) {
			return existing, nil
		}
		return domain.ListingPlan{}, err
	}
	s.activity(ctx, plan, nil, "listing.previewed", "info", "Listing preview ready",
		fmt.Sprintf("%s -> %s at group ratio %s (%s)", group.Name, request.TargetGroup, plan.ProposedGroupRatio, plan.GroupModelPricing.Mode))
	_ = adapter
	return plan, nil
}

func (s *Service) Apply(ctx context.Context, planID int64, input ApplyInput) (domain.ListingChangeSet, error) {
	ctx = targets.WithFreshRead(ctx)
	if err := s.remoteWrites.Check("listing apply"); err != nil {
		return domain.ListingChangeSet{}, err
	}
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if !input.ConfirmEnable {
		return domain.ListingChangeSet{}, errors.New("confirm_enable=true is required before a tested channel may be enabled")
	}
	if input.IdempotencyKey == "" || len(input.IdempotencyKey) > 160 {
		return domain.ListingChangeSet{}, errors.New("a bounded idempotency_key is required")
	}
	plan, err := s.store.GetListingPlan(ctx, planID)
	if err != nil {
		return domain.ListingChangeSet{}, err
	}
	marker := ownershipMarker(planID, input.IdempotencyKey)
	change := domain.ListingChangeSet{PlanID: planID, TargetSiteID: plan.TargetSiteID, Status: "pending", Step: "created",
		IdempotencyKey: input.IdempotencyKey, OwnershipMarker: marker, ExpectedTargetHash: plan.ExpectedTargetHash,
		GroupModelPricing: domain.ListingGroupModelPricingReceipt{
			Mode: plan.GroupModelPricing.Mode, ExpectedRevision: plan.GroupModelPricing.ExpectedRevision,
			DesiredRevision: plan.GroupModelPricing.DesiredRevision, Before: plan.GroupModelPricing.ExpectedContract,
			After: plan.GroupModelPricing.DesiredContract, Diff: append([]domain.TargetFieldDiff(nil), plan.GroupModelPricing.Diff...),
			GroupVisibility: domain.ListingGroupVisibilityReceipt{
				ExpectedHash: plan.GroupModelPricing.GroupVisibility.ExpectedHash,
				DesiredHash:  plan.GroupModelPricing.GroupVisibility.DesiredHash,
				Before:       cloneRatios(plan.GroupModelPricing.GroupVisibility.Expected),
				After:        cloneRatios(plan.GroupModelPricing.GroupVisibility.Desired),
				Changed:      plan.GroupModelPricing.GroupVisibility.Changed,
			},
			GlobalModelPrice: domain.ListingGlobalModelPriceReceipt{
				ExpectedHash: plan.GroupModelPricing.GlobalModelPrice.ExpectedHash,
				DesiredHash:  plan.GroupModelPricing.GlobalModelPrice.DesiredHash,
				Before:       cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Expected),
				After:        cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Desired),
				Added:        cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Added),
				Changed:      plan.GroupModelPricing.GlobalModelPrice.Changed,
			},
			RequestedModels: append([]string(nil), plan.Channel.Models...),
		}}
	created, err := s.store.CreateOrGetListingChangeSet(ctx, &change)
	if err != nil {
		return domain.ListingChangeSet{}, err
	}
	if !created {
		switch change.Status {
		case "applied":
			return change, nil
		case "rolled_back":
			return change, errors.New("listing change set was rolled back and cannot be applied again; create a new preview")
		case "failed", "stale":
			return change, fmt.Errorf("listing change set is terminal: %s", change.Error)
		}
	}

	lock := s.targetLock(plan.TargetSiteID)
	lock.Lock()
	defer lock.Unlock()
	leaseNow := time.Now().UTC()
	leased, err := s.store.TryAcquireTargetChangeLease(ctx, plan.TargetSiteID, marker, leaseNow, 10*time.Minute)
	if err != nil {
		return change, err
	}
	if !leased {
		return change, errors.New("another target change is currently in progress")
	}
	defer s.store.ReleaseTargetChangeLease(context.Background(), plan.TargetSiteID, marker)

	plan, err = s.store.GetListingPlan(ctx, planID)
	if err != nil {
		return change, err
	}
	plan, err = resolveListingPlanFromReceipt(plan, change)
	if err != nil {
		return change, err
	}
	if change.Status == "pending" {
		now := time.Now().UTC()
		change.Status = "applying"
		change.StartedAt = &now
		if err := s.store.UpdateListingChangeSet(ctx, change, []string{"pending"}); err != nil {
			return change, err
		}
	} else if change.Status != "applying" {
		return change, fmt.Errorf("listing change set cannot resume from %q", change.Status)
	}
	if plan.Status == "ready" || plan.Status == "failed" || plan.Status == "stale" {
		if err := s.store.TransitionListingPlan(ctx, plan.ID, []string{plan.Status}, "applying", ""); err != nil {
			return change, err
		}
		plan.Status = "applying"
	} else if plan.Status != "applying" && !(plan.Status == "applied" && change.ChannelEnabled) {
		return change, fmt.Errorf("listing plan cannot apply from %q", plan.Status)
	}

	s.activity(ctx, plan, &change, "listing.applying", "info", "Listing apply started", "Target mutation lease acquired")
	err = s.execute(ctx, plan, &change)
	if err == nil {
		return s.store.GetListingChangeSet(ctx, change.ID)
	}
	if change.ChannelEnabled {
		change.Status = "applying"
		change.Step = "enabled"
		change.Error = "enabled channel preserved; retry the same idempotency key to reconcile state: " + err.Error()
		_ = s.store.UpdateListingChangeSet(ctx, change, []string{"applying"})
		s.activity(ctx, plan, &change, "listing.reconcile_required", "warning", "Enabled listing needs state reconciliation", change.Error)
		return change, err
	}
	// Compensation must use the durable effective/excluded model receipt. The
	// immutable preview may still contain models filtered during discovery.
	compensationPlan, resolutionErr := resolveListingPlanFromReceipt(plan, change)
	if resolutionErr != nil {
		compensationPlan = plan
	}
	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), 90*time.Second)
	compensation := s.compensate(recoveryCtx, compensationPlan, &change)
	cancelRecovery()
	message := err.Error()
	if resolutionErr != nil {
		message += "; runtime model receipt could not be re-resolved: " + resolutionErr.Error()
	}
	if compensation != "" {
		message += "; compensation: " + compensation
	}
	change.Status = "failed"
	if _, ok := err.(staleError); ok {
		change.Status = "stale"
	}
	change.Error = message
	change.Step = "compensated"
	now := time.Now().UTC()
	change.FinishedAt = &now
	_ = s.store.UpdateListingChangeSet(ctx, change, []string{"applying"})
	_ = s.store.TransitionListingPlan(ctx, plan.ID, []string{"applying"}, change.Status, message)
	s.activity(ctx, plan, &change, "listing.failed", "error", "Listing apply failed", message)
	return change, err
}

// Rollback conditionally reverses a successfully applied listing. It never
// overwrites target state that no longer matches the frozen apply receipt.
func (s *Service) Rollback(ctx context.Context, changeID int64, input RollbackInput) (domain.ListingChangeSet, error) {
	ctx = targets.WithFreshRead(ctx)
	if err := s.remoteWrites.Check("listing rollback"); err != nil {
		return domain.ListingChangeSet{}, err
	}
	if !input.ConfirmRollback {
		return domain.ListingChangeSet{}, errors.New("confirm_rollback=true is required")
	}
	change, err := s.store.GetListingChangeSet(ctx, changeID)
	if err != nil {
		return domain.ListingChangeSet{}, err
	}
	if change.Status == "rolled_back" {
		return change, nil
	}
	if change.Status != "applied" && change.Status != "rolling_back" {
		return change, fmt.Errorf("listing change set in state %q cannot be rolled back", change.Status)
	}
	plan, err := s.store.GetListingPlan(ctx, change.PlanID)
	if err != nil {
		return change, err
	}
	plan, err = resolveListingPlanFromReceipt(plan, change)
	if err != nil {
		return change, err
	}
	if change.Status == "applied" {
		if err := s.preflightRollback(ctx, plan, change); err != nil {
			return change, err
		}
	}

	lock := s.targetLock(change.TargetSiteID)
	lock.Lock()
	defer lock.Unlock()
	leaseOwner := fmt.Sprintf("listing:rollback:%d", change.ID)
	leased, err := s.store.TryAcquireTargetChangeLease(ctx, change.TargetSiteID, leaseOwner, time.Now().UTC(), 10*time.Minute)
	if err != nil {
		return change, err
	}
	if !leased {
		return change, errors.New("another target change is currently in progress")
	}
	defer s.store.ReleaseTargetChangeLease(context.Background(), change.TargetSiteID, leaseOwner)

	change, err = s.store.GetListingChangeSet(ctx, changeID)
	if err != nil {
		return change, err
	}
	if change.Status == "rolled_back" {
		return change, nil
	}
	plan, err = s.store.GetListingPlan(ctx, change.PlanID)
	if err != nil {
		return change, err
	}
	plan, err = resolveListingPlanFromReceipt(plan, change)
	if err != nil {
		return change, err
	}
	if change.Status == "applied" {
		if err := s.preflightRollback(ctx, plan, change); err != nil {
			return change, err
		}
		change.Status = "rolling_back"
		change.Step = "rollback_started"
		change.Error = ""
		change.RollbackStatus = "preflight verified; rollback started"
		if err := s.store.UpdateListingChangeSet(ctx, change, []string{"applied"}); err != nil {
			return change, err
		}
		s.activity(ctx, plan, &change, "listing.rollback_started", "warning", "Listing rollback started",
			"Target revision, GroupRatio, owned channel and continuing policy passed the read-only preflight")
	} else if change.Status != "rolling_back" {
		return change, fmt.Errorf("listing change set cannot resume rollback from %q", change.Status)
	}

	if err := s.executeRollback(ctx, plan, &change); err != nil {
		change.Status = "rolling_back"
		change.Error = err.Error()
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "paused: "+err.Error())
		_ = s.store.UpdateListingChangeSet(ctx, change, []string{"rolling_back"})
		s.activity(ctx, plan, &change, "listing.rollback_paused", "error", "Listing rollback paused", err.Error())
		stored, getErr := s.store.GetListingChangeSet(ctx, change.ID)
		if getErr == nil {
			change = stored
		}
		return change, err
	}
	return s.store.GetListingChangeSet(ctx, change.ID)
}

func (s *Service) preflightRollback(ctx context.Context, plan domain.ListingPlan, change domain.ListingChangeSet) error {
	if plan.Status != "applied" || change.Status != "applied" {
		return rollbackConflict("the listing is no longer in the applied state")
	}
	if change.CreatedChannelID == nil || !change.ChannelEnabled {
		return rollbackConflict("the applied receipt is missing its enabled channel")
	}
	if listingUsesPricingMapping(plan.PricingRule) && (change.PricingPolicyID == nil || !change.PolicyActivated) {
		return rollbackConflict("the applied receipt is missing its continuing pricing mapping")
	}
	if !listingUsesPricingMapping(plan.PricingRule) && change.PricingPolicyID != nil {
		return rollbackConflict("the applied receipt unexpectedly contains a continuing pricing mapping")
	}
	if change.ExpectedTargetHash != plan.ExpectedTargetHash || change.AppliedTargetHash == "" ||
		change.AppliedTargetHash != plan.DesiredTargetHash {
		return rollbackConflict("the stored GroupRatio receipt is internally inconsistent")
	}
	target, _, channelAdapter, snapshot, err := s.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		return rollbackConflict("target inspection failed: %v", err)
	}
	if snapshot.Hash != change.AppliedTargetHash {
		return rollbackConflict("target GroupRatio changed after apply: expected %s, current %s", change.AppliedTargetHash, snapshot.Hash)
	}
	if plan.GroupModelPricing.Mode == "atomic_contract" {
		if snapshot.GroupModelPricing == nil || change.GroupModelPricing.AppliedRevision == "" ||
			change.GroupModelPricing.AppliedRevision != plan.GroupModelPricing.DesiredRevision ||
			change.GroupModelPricing.Before.Revision != plan.GroupModelPricing.ExpectedRevision ||
			change.GroupModelPricing.After.Revision != plan.GroupModelPricing.DesiredRevision {
			return rollbackConflict("the stored group-model pricing receipt is incomplete")
		}
		if snapshot.GroupModelPricing.Revision != change.GroupModelPricing.AppliedRevision {
			return rollbackConflict("group-model pricing changed after apply: expected %s, current %s",
				change.GroupModelPricing.AppliedRevision, snapshot.GroupModelPricing.Revision)
		}
		global := plan.GroupModelPricing.GlobalModelPrice
		if global.Supported {
			receipt := change.GroupModelPricing.GlobalModelPrice
			option, exists := snapshot.PricingOptions["ModelPrice"]
			if !exists || receipt.AppliedHash == "" || receipt.ExpectedHash != global.ExpectedHash ||
				receipt.DesiredHash != global.DesiredHash || targets.HashDecimalMap(receipt.Before) != global.ExpectedHash ||
				targets.HashDecimalMap(receipt.After) != global.DesiredHash {
				return rollbackConflict("the stored global ModelPrice receipt is incomplete")
			}
			if option.Hash != receipt.AppliedHash || receipt.AppliedHash != global.DesiredHash {
				return rollbackConflict("global ModelPrice changed after apply: expected %s, current %s",
					receipt.AppliedHash, option.Hash)
			}
		}
	}
	visibility := plan.GroupModelPricing.GroupVisibility
	if visibility.Required {
		receipt := change.GroupModelPricing.GroupVisibility
		if !snapshot.UserUsableGroupsSupported || receipt.AppliedHash == "" ||
			receipt.ExpectedHash != visibility.ExpectedHash || receipt.DesiredHash != visibility.DesiredHash ||
			targets.HashStringMap(receipt.Before) != visibility.ExpectedHash ||
			targets.HashStringMap(receipt.After) != visibility.DesiredHash {
			return rollbackConflict("the stored UserUsableGroups receipt is incomplete")
		}
		if snapshot.UserUsableGroupsHash != receipt.AppliedHash || receipt.AppliedHash != visibility.DesiredHash {
			return rollbackConflict("UserUsableGroups changed after apply: expected %s, current %s",
				receipt.AppliedHash, snapshot.UserUsableGroupsHash)
		}
	}
	channel, err := channelAdapter.GetChannel(ctx, target, *change.CreatedChannelID)
	if err != nil {
		return rollbackConflict("owned channel inspection failed: %v", err)
	}
	if err := verifyListingChannel(channel, plan, change.OwnershipMarker); err != nil {
		return rollbackConflict("owned channel changed after apply: %v", err)
	}
	if channel.Status != 1 {
		return rollbackConflict("owned channel status changed after apply: expected enabled, current status %d", channel.Status)
	}
	if change.PricingPolicyID != nil {
		policy, err := s.store.GetPolicy(ctx, *change.PricingPolicyID)
		if err != nil {
			return rollbackConflict("continuing pricing policy inspection failed: %v", err)
		}
		if !equivalentPolicyDefinition(policy, listingPolicyForPlan(plan)) {
			return rollbackConflict("continuing pricing policy definition changed after apply")
		}
		if !policy.Enabled {
			return rollbackConflict("continuing pricing policy was disabled after apply")
		}
	}
	if err := s.preflightNativeMirrorRollback(ctx, plan, change); err != nil {
		return err
	}
	return nil
}

func (s *Service) executeRollback(ctx context.Context, plan domain.ListingPlan, change *domain.ListingChangeSet) error {
	if err := s.remoteWrites.Check("listing rollback execution"); err != nil {
		return err
	}
	if change.Status != "rolling_back" {
		return errors.New("rollback execution requires the rolling_back state")
	}
	if change.CreatedChannelID == nil {
		return rollbackConflict("the applied receipt has no owned channel id")
	}
	if err := s.prepareNativeMirrorRollback(ctx, plan, change); err != nil {
		return err
	}
	target, adapter, channelAdapter, _, err := s.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		return err
	}
	channel, channelFound, channelErr := channelAdapter.FindOwnedChannel(ctx, target, change.OwnershipMarker)
	if channelErr != nil {
		return fmt.Errorf("inspect owned channel before rollback: %w", channelErr)
	}
	if channelFound {
		if channel.ID != *change.CreatedChannelID {
			return rollbackConflict("ownership marker moved from channel #%d to channel #%d", *change.CreatedChannelID, channel.ID)
		}
		if err := verifyListingChannel(channel, plan, change.OwnershipMarker); err != nil {
			return rollbackConflict("owned channel changed during rollback: %v", err)
		}
		switch channel.Status {
		case 1:
			if _, err := channelAdapter.DisableOwnedChannel(ctx, target, channel.ID, change.OwnershipMarker); err != nil {
				return fmt.Errorf("disable owned channel before rollback: %w", err)
			}
		case 2:
		default:
			return rollbackConflict("owned channel is in unsupported status %d", channel.Status)
		}
	} else if !rollbackStepAtLeast(change.Step, "rollback_ratio_restored") {
		return rollbackConflict("owned channel disappeared before its rollback checkpoint")
	}
	change.ChannelEnabled = false
	if !rollbackStepAtLeast(change.Step, "rollback_channel_disabled") {
		change.Step = "rollback_channel_disabled"
	}
	change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "owned channel disabled")
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
		return err
	}

	visibility := plan.GroupModelPricing.GroupVisibility
	if visibility.Required {
		visibilityAdapter, visibilityErr := targets.AsGroupVisibilityAdapter(adapter)
		if visibilityErr != nil {
			return visibilityErr
		}
		currentVisibility, visibilityErr := visibilityAdapter.ReadGroupVisibility(ctx, target)
		if visibilityErr != nil {
			return fmt.Errorf("read UserUsableGroups before rollback: %w", visibilityErr)
		}
		switch currentVisibility.Hash {
		case visibility.ExpectedHash:
			change.GroupModelPricing.GroupVisibility.RollbackStatus = "already restored hash " + currentVisibility.Hash
		case change.GroupModelPricing.GroupVisibility.AppliedHash:
			rolledBack, rollbackErr := visibilityAdapter.RollbackGroupVisibility(ctx, target,
				change.GroupModelPricing.GroupVisibility.AppliedHash, visibility.Expected)
			if rollbackErr != nil {
				return fmt.Errorf("conditionally restore UserUsableGroups: %w", rollbackErr)
			}
			if rolledBack.Hash != visibility.ExpectedHash {
				return errors.New("UserUsableGroups rollback readback did not match the preview hash")
			}
			change.GroupModelPricing.GroupVisibility.RollbackStatus = "restored hash " + rolledBack.Hash
		default:
			return rollbackConflict("UserUsableGroups changed during rollback: current hash %s", currentVisibility.Hash)
		}
		if !rollbackStepAtLeast(change.Step, "rollback_visibility_restored") {
			change.Step = "rollback_visibility_restored"
		}
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus,
			"UserUsableGroups "+change.GroupModelPricing.GroupVisibility.RollbackStatus)
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
			return err
		}
	}

	global := plan.GroupModelPricing.GlobalModelPrice
	if global.Supported && change.GroupModelPricing.GlobalModelPrice.AppliedHash != "" {
		status, rollbackErr := rollbackListingGlobalModelPrices(ctx, adapter, target, global,
			&change.GroupModelPricing.GlobalModelPrice)
		if rollbackErr != nil {
			return fmt.Errorf("conditionally restore global ModelPrice: %w", rollbackErr)
		}
		if !rollbackStepAtLeast(change.Step, "rollback_global_model_price_restored") {
			change.Step = "rollback_global_model_price_restored"
		}
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, status)
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
			return err
		}
	}

	if plan.GroupModelPricing.Mode == "atomic_contract" {
		groupModelAdapter, err := targets.AsGroupModelPricingAdapter(adapter)
		if err != nil {
			return err
		}
		current, err := groupModelAdapter.ReadGroupModelPricing(ctx, target)
		if err != nil {
			return fmt.Errorf("read group-model pricing before rollback: %w", err)
		}
		switch current.Revision {
		case plan.GroupModelPricing.ExpectedRevision:
			change.GroupModelPricing.RollbackStatus = "already restored revision " + current.Revision
		case change.GroupModelPricing.AppliedRevision:
			rolledBack, err := groupModelAdapter.RollbackGroupModelPricing(ctx, target,
				change.GroupModelPricing.AppliedRevision, change.GroupModelPricing.Before)
			if err != nil {
				return fmt.Errorf("conditionally restore group-model pricing: %w", err)
			}
			if rolledBack.Revision != plan.GroupModelPricing.ExpectedRevision {
				return errors.New("group-model pricing rollback readback did not match the preview revision")
			}
			change.GroupModelPricing.RollbackStatus = "restored revision " + rolledBack.Revision
		default:
			return rollbackConflict("group-model pricing changed during rollback: current revision %s", current.Revision)
		}
		if !rollbackStepAtLeast(change.Step, "rollback_contract_restored") {
			change.Step = "rollback_contract_restored"
		}
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, change.GroupModelPricing.RollbackStatus)
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
			return err
		}
	}

	current, err := adapter.Snapshot(ctx, target)
	if err != nil {
		return fmt.Errorf("read GroupRatio before rollback: %w", err)
	}
	switch current.Hash {
	case plan.ExpectedTargetHash:
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "GroupRatio already restored")
	case change.AppliedTargetHash:
		if change.RatioChanged {
			rolledBack, err := adapter.RollbackGroupRatio(ctx, target, change.AppliedTargetHash, plan.ExpectedGroupRatios)
			if err != nil {
				return fmt.Errorf("conditionally restore GroupRatio: %w", err)
			}
			if rolledBack.Hash != plan.ExpectedTargetHash {
				return errors.New("GroupRatio rollback readback did not match the preview hash")
			}
		}
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "GroupRatio restored to preview hash "+plan.ExpectedTargetHash)
	default:
		return rollbackConflict("GroupRatio changed during rollback: current hash %s", current.Hash)
	}
	if !rollbackStepAtLeast(change.Step, "rollback_ratio_restored") {
		change.Step = "rollback_ratio_restored"
	}
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
		return err
	}

	channel, channelFound, channelErr = channelAdapter.FindOwnedChannel(ctx, target, change.OwnershipMarker)
	if channelErr != nil {
		return fmt.Errorf("inspect owned channel before deletion: %w", channelErr)
	}
	if channelFound {
		if channel.ID != *change.CreatedChannelID {
			return rollbackConflict("ownership marker moved before deletion from channel #%d to channel #%d", *change.CreatedChannelID, channel.ID)
		}
		if err := verifyListingChannel(channel, plan, change.OwnershipMarker); err != nil {
			return rollbackConflict("owned channel changed before deletion: %v", err)
		}
		if channel.Status == 1 {
			if _, err := channelAdapter.DisableOwnedChannel(ctx, target, channel.ID, change.OwnershipMarker); err != nil {
				return fmt.Errorf("disable re-enabled owned channel: %w", err)
			}
		} else if channel.Status != 2 {
			return rollbackConflict("owned channel is in unsupported status %d before deletion", channel.Status)
		}
		if err := channelAdapter.DeleteOwnedDisabledChannel(ctx, target, channel.ID, change.OwnershipMarker); err != nil {
			return fmt.Errorf("delete owned disabled channel: %w", err)
		}
	}
	if !rollbackStepAtLeast(change.Step, "rollback_channel_deleted") {
		change.Step = "rollback_channel_deleted"
	}
	change.RollbackStatus = appendRollbackStatus(change.RollbackStatus,
		fmt.Sprintf("owned channel #%d deleted", *change.CreatedChannelID))
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
		return err
	}

	if change.PricingPolicyID != nil {
		policyID := *change.PricingPolicyID
		policy, err := s.store.GetPolicy(ctx, policyID)
		if err != nil {
			return rollbackConflict("continuing pricing policy disappeared during rollback: %v", err)
		}
		if !equivalentPolicyDefinition(policy, listingPolicyForPlan(plan)) {
			return rollbackConflict("continuing pricing policy changed during rollback")
		}
		policyResult, err := s.store.RestoreListingPolicy(ctx, change.ID)
		if err != nil {
			return fmt.Errorf("restore continuing pricing policy: %w", err)
		}
		stored, err := s.store.GetListingChangeSet(ctx, change.ID)
		if err != nil {
			return err
		}
		*change = stored
		change.RollbackStatus = appendRollbackStatus(change.RollbackStatus,
			fmt.Sprintf("policy #%d: %s", policyID, policyResult))
	}
	if !rollbackStepAtLeast(change.Step, "rollback_policy_restored") {
		change.Step = "rollback_policy_restored"
	}
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
		return err
	}

	storedPlan, err := s.store.GetListingPlan(ctx, plan.ID)
	if err != nil {
		return err
	}
	if storedPlan.Status == "applied" {
		if err := s.store.TransitionListingPlan(ctx, plan.ID, []string{"applied"}, "rolled_back", ""); err != nil {
			return err
		}
	} else if storedPlan.Status != "rolled_back" {
		return fmt.Errorf("listing plan cannot finish rollback from %q", storedPlan.Status)
	}
	change.Status = "rolled_back"
	change.Step = "rolled_back"
	change.ChannelEnabled = false
	change.Error = ""
	change.RollbackStatus = appendRollbackStatus(change.RollbackStatus, "rollback verified and complete")
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"rolling_back"}); err != nil {
		return err
	}
	s.activity(ctx, plan, change, "listing.rolled_back", "success", "Listing rolled back", change.RollbackStatus)
	return nil
}

func (s *Service) execute(ctx context.Context, plan domain.ListingPlan, change *domain.ListingChangeSet) error {
	if err := s.remoteWrites.Check("listing execution"); err != nil {
		return err
	}
	var err error
	plan, err = resolveListingPlanFromReceipt(plan, *change)
	if err != nil {
		return err
	}
	if !plan.ProfitVerified || strings.TrimSpace(plan.EquivalentCostRatio) == "" {
		return errors.New("listing plan has no verified model-level profit basis")
	}
	supplier, _, _, err := s.validateFrozenListingSource(ctx, plan, "after preview")
	if err != nil {
		return err
	}
	_, upstreamKey, err := s.loadUpstreamKey(ctx, supplier, plan.UpstreamCredentialID, plan.Channel.BaseURL)
	if err != nil {
		return err
	}
	target, adapter, channelAdapter, current, err := s.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		return err
	}
	var groupModelAdapter targets.GroupModelPricingAdapter
	var visibilityAdapter targets.GroupVisibilityAdapter
	var pricingOptionAdapter targets.PricingOptionAdapter
	if err := validateListingVisibilityPlan(plan); err != nil {
		return err
	}
	if plan.GroupModelPricing.GroupVisibility.Required {
		visibilityAdapter, err = targets.AsGroupVisibilityAdapter(adapter)
		if err != nil || !current.UserUsableGroupsSupported {
			return staleError{"target no longer exposes user-visible group management"}
		}
	}
	if plan.GroupModelPricing.Mode == "atomic_contract" {
		if err := validateListingContractPlan(plan); err != nil {
			return err
		}
		groupModelAdapter, err = targets.AsGroupModelPricingAdapter(adapter)
		if err != nil {
			return err
		}
		if !current.GroupModelPricingSupported || current.GroupModelPricing == nil {
			return staleError{"target no longer exposes the group-model pricing contract"}
		}
		if plan.GroupModelPricing.GlobalModelPrice.Supported {
			pricingOptionAdapter, err = targets.AsPricingOptionAdapter(adapter)
			if err != nil {
				return err
			}
			if _, exists := current.PricingOptions["ModelPrice"]; !exists {
				return staleError{"target no longer exposes the ModelPrice option"}
			}
		}
	}
	targetCatalog, err := channelAdapter.PricingCatalog(ctx, target)
	if err != nil {
		return err
	}
	if pricing.ModelPriceHash(targetCatalog) != plan.TargetCatalogHash {
		return staleError{"target model pricing catalog changed after preview"}
	}
	if targets.HashRatios(plan.ExpectedGroupRatios) != plan.ExpectedTargetHash {
		return errors.New("listing plan expected GroupRatio snapshot is internally inconsistent")
	}
	desired := cloneRatios(plan.ExpectedGroupRatios)
	desired[plan.TargetGroup] = plan.ProposedGroupRatio
	if targets.HashRatios(desired) != plan.DesiredTargetHash {
		return errors.New("listing plan desired GroupRatio snapshot is internally inconsistent")
	}
	if _, _, _, err := s.validateFrozenListingSource(ctx, plan, "during apply preflight"); err != nil {
		return err
	}
	boundPolicy := domain.PricingPolicy{}

	// Read the owned remote object on every resume. This closes the crash window
	// after NewAPI accepted an enable request but before the local checkpoint.
	if owned, found, findErr := channelAdapter.FindOwnedChannel(ctx, target, change.OwnershipMarker); findErr != nil {
		return findErr
	} else if found {
		if err := verifyListingChannelIdentity(owned, plan, change.OwnershipMarker); err != nil {
			return err
		}
		if listingHasModelResolution(*change) {
			models := sortedUnique(owned.Models)
			effective := sortedUnique(plan.Channel.Models)
			requested := sortedUnique(change.GroupModelPricing.RequestedModels)
			if strings.Join(models, "\x00") != strings.Join(effective, "\x00") &&
				strings.Join(models, "\x00") != strings.Join(requested, "\x00") {
				return errors.New("owned channel model scope differs from both the requested and effective listing sets")
			}
		} else if err := verifyListingChannel(owned, plan, change.OwnershipMarker); err != nil {
			return err
		}
		change.CreatedChannelID = &owned.ID
		switch owned.Status {
		case 1:
			change.ChannelEnabled = true
		case 2:
			change.ChannelEnabled = false
		default:
			return fmt.Errorf("owned channel is in unsupported status %d", owned.Status)
		}
	}
	if change.CreatedChannelID == nil && current.Hash != plan.ExpectedTargetHash {
		return staleError{fmt.Sprintf("target GroupRatio changed after preview: expected %s, current %s", plan.ExpectedTargetHash, current.Hash)}
	}
	if change.CreatedChannelID == nil && groupModelAdapter != nil && current.GroupModelPricing.Revision != plan.GroupModelPricing.ExpectedRevision {
		return staleError{fmt.Sprintf("target group-model pricing changed after preview: expected %s, current %s",
			plan.GroupModelPricing.ExpectedRevision, current.GroupModelPricing.Revision)}
	}
	if change.CreatedChannelID == nil && pricingOptionAdapter != nil &&
		current.PricingOptions["ModelPrice"].Hash != plan.GroupModelPricing.GlobalModelPrice.ExpectedHash {
		return staleError{"target ModelPrice changed after preview"}
	}
	if change.CreatedChannelID == nil && visibilityAdapter != nil &&
		current.UserUsableGroupsHash != plan.GroupModelPricing.GroupVisibility.ExpectedHash {
		return staleError{"target UserUsableGroups changed after preview"}
	}

	if change.ChannelEnabled {
		if current.Hash != plan.DesiredTargetHash {
			return staleError{"enabled channel is preserved, but target GroupRatio changed externally"}
		}
		if groupModelAdapter != nil && current.GroupModelPricing.Revision != plan.GroupModelPricing.DesiredRevision {
			return staleError{"enabled channel is preserved, but target group-model pricing changed externally"}
		}
		if pricingOptionAdapter != nil &&
			current.PricingOptions["ModelPrice"].Hash != plan.GroupModelPricing.GlobalModelPrice.DesiredHash {
			return staleError{"enabled channel is preserved, but target ModelPrice changed externally"}
		}
		if visibilityAdapter != nil && current.UserUsableGroupsHash != plan.GroupModelPricing.GroupVisibility.DesiredHash {
			return staleError{"enabled channel is preserved, but target UserUsableGroups changed externally"}
		}
		if listingUsesPricingMapping(plan.PricingRule) && !change.PolicyActivated {
			if change.PricingPolicyID == nil {
				return errors.New("enabled listing is missing its continuing pricing policy receipt")
			}
			boundPolicy, err = s.store.GetPolicy(ctx, *change.PricingPolicyID)
			if err != nil {
				return err
			}
			if !equivalentPolicyDefinition(boundPolicy, listingPolicyForPlan(plan)) {
				return errors.New("continuing pricing policy differs from the resolved listing")
			}
			if err := s.store.ActivateListingPolicy(ctx, change.ID, boundPolicy.ID); err != nil {
				return err
			}
			change.PolicyActivated = true
		}
		if err := s.ensureNativeMirrorOwnership(ctx, plan, change); err != nil {
			return fmt.Errorf("adopt complete mirror ownership: %w", err)
		}
		return s.finalizeApplied(plan, change)
	}

	var configuredChannel domain.TargetChannel
	if change.CreatedChannelID == nil {
		configuredChannel, err = channelAdapter.CreateDisabledChannel(ctx, target, plan.Channel, plan.TargetGroup, upstreamKey, change.OwnershipMarker)
		if err != nil {
			return err
		}
		change.CreatedChannelID = &configuredChannel.ID
		change.Step = "channel_created_disabled"
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
			return err
		}
		s.activity(ctx, plan, change, "listing.channel_created", "info", "Disabled channel created", fmt.Sprintf("channel_id=%d", configuredChannel.ID))
	} else {
		configuredChannel, err = channelAdapter.GetChannel(ctx, target, *change.CreatedChannelID)
		if err != nil {
			return fmt.Errorf("resume disabled channel readback: %w", err)
		}
		if err := verifyListingChannelIdentity(configuredChannel, plan, change.OwnershipMarker); err != nil {
			return fmt.Errorf("resume disabled channel: %w", err)
		}
	}

	if !listingHasModelResolution(*change) {
		requested := append([]string(nil), plan.Channel.Models...)
		discovered, discoveryErr := channelAdapter.FetchChannelModels(ctx, target, configuredChannel.ID)
		effective, excluded, status, resolutionErr := listingModelResolution(plan, discovered, discoveryErr)
		if resolutionErr != nil {
			return resolutionErr
		}
		resolvedPlan, resolveErr := resolveListingPlan(plan, effective, excluded)
		if resolveErr != nil {
			return resolveErr
		}
		resolvedPlan, resolveErr = bindAuthoritativeResolvedPricing(ctx, groupModelAdapter, target, resolvedPlan)
		if resolveErr != nil {
			return resolveErr
		}
		recordListingModelResolution(change, resolvedPlan, requested, effective, excluded, status)
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
			return err
		}
		plan = resolvedPlan
		if len(excluded) > 0 {
			s.activity(ctx, plan, change, "listing.models_excluded", "info", "已自动排除不可用模型",
				fmt.Sprintf("已保留 %d 个可用模型，排除：%s", len(effective), strings.Join(excluded, "、")))
		}
	}
	configuredChannel, err = ensureListingChannelModels(ctx, adapter, channelAdapter, target, plan, change)
	if err != nil {
		return err
	}

	current, err = adapter.Snapshot(ctx, target)
	if err != nil {
		return err
	}
	if current.Hash != plan.ExpectedTargetHash && current.Hash != plan.DesiredTargetHash {
		return staleError{"target GroupRatio changed while the disabled channel was being verified"}
	}
	if groupModelAdapter != nil && current.GroupModelPricing != nil &&
		current.GroupModelPricing.Revision != plan.GroupModelPricing.ExpectedRevision &&
		current.GroupModelPricing.Revision != plan.GroupModelPricing.DesiredRevision {
		return staleError{"target group-model pricing changed while the disabled channel was being verified"}
	}
	if pricingOptionAdapter != nil {
		modelPriceHash := current.PricingOptions["ModelPrice"].Hash
		if modelPriceHash != plan.GroupModelPricing.GlobalModelPrice.ExpectedHash &&
			modelPriceHash != plan.GroupModelPricing.GlobalModelPrice.DesiredHash {
			return staleError{"target ModelPrice changed while the disabled channel was being verified"}
		}
	}
	if visibilityAdapter != nil && current.UserUsableGroupsHash != plan.GroupModelPricing.GroupVisibility.ExpectedHash &&
		current.UserUsableGroupsHash != plan.GroupModelPricing.GroupVisibility.DesiredHash {
		return staleError{"target UserUsableGroups changed while the disabled channel was being verified"}
	}
	if !stepAtLeast(change.Step, "models_verified") {
		if err := verifyListingChannel(configuredChannel, plan, change.OwnershipMarker); err != nil {
			return fmt.Errorf("configured channel readback mismatch: %w", err)
		}
		change.Step = "models_verified"
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
			return err
		}
		s.activity(ctx, plan, change, "listing.models_verified", "info", "上架模型范围已确认", fmt.Sprintf("%d 个模型", len(plan.Channel.Models)))
	}
	if _, _, err := s.ensureNativeMirrorPolicyDraft(ctx, plan); err != nil {
		return fmt.Errorf("prepare complete mirror policy: %w", err)
	}
	if listingUsesPricingMapping(plan.PricingRule) {
		desiredPolicy := listingPolicyForPlan(plan)
		if change.PricingPolicyID == nil {
			var policyCreated, policyInitiallyOn, policyActivated bool
			boundPolicy, policyCreated, policyInitiallyOn, policyActivated, err = s.store.BindListingPolicy(ctx, change.ID, desiredPolicy)
			if err != nil {
				return err
			}
			change.PricingPolicyID = &boundPolicy.ID
			change.PolicyCreated = policyCreated
			change.PolicyInitiallyOn = policyInitiallyOn
			change.PolicyActivated = policyActivated
			change.Step = "policy_bound"
			if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
				return err
			}
			s.activity(ctx, plan, change, "listing.policy_bound", "info", "Continuing pricing policy bound",
				fmt.Sprintf("policy_id=%d enabled=%t", boundPolicy.ID, policyActivated))
		} else {
			boundPolicy, err = s.store.GetPolicy(ctx, *change.PricingPolicyID)
			if err != nil {
				return err
			}
			if !equivalentPolicyDefinition(boundPolicy, desiredPolicy) {
				return errors.New("continuing pricing policy differs from the resolved listing")
			}
		}
	}

	if _, _, _, err := s.validateFrozenListingSource(ctx, plan, "before the target pricing write"); err != nil {
		return err
	}

	// Only expose the new price after the disabled channel has passed model and
	// protocol checks. The mutable GroupRatio window is now intentionally short.
	current, err = adapter.Snapshot(ctx, target)
	if err != nil {
		return err
	}
	if change.AppliedTargetHash == "" {
		switch current.Hash {
		case plan.ExpectedTargetHash:
			change.RatioChanged = plan.ExpectedTargetHash != plan.DesiredTargetHash
			if change.RatioChanged {
				current, err = adapter.ApplyGroupRatio(ctx, target, plan.ExpectedTargetHash, desired)
				if err != nil {
					return fmt.Errorf("apply target GroupRatio: %w", err)
				}
			}
		case plan.DesiredTargetHash:
			change.RatioChanged = plan.ExpectedTargetHash != plan.DesiredTargetHash
		default:
			return staleError{fmt.Sprintf("target GroupRatio changed after preview: expected %s, current %s", plan.ExpectedTargetHash, current.Hash)}
		}
		change.AppliedTargetHash = plan.DesiredTargetHash
		change.Step = "ratio_confirmed"
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
			return err
		}
		s.activity(ctx, plan, change, "listing.ratio_confirmed", "info", "Group ratio confirmed", plan.ProposedGroupRatio)
	} else if current.Hash != plan.DesiredTargetHash {
		return staleError{"target GroupRatio changed before channel enable"}
	}

	if groupModelAdapter != nil {
		current, err = adapter.Snapshot(ctx, target)
		if err != nil {
			return err
		}
		if current.GroupModelPricing == nil {
			return staleError{"target group-model pricing became unreadable before apply"}
		}
		if change.GroupModelPricing.AppliedRevision == "" {
			switch current.GroupModelPricing.Revision {
			case plan.GroupModelPricing.ExpectedRevision:
				result, applyErr := groupModelAdapter.ApplyGroupModelPricingControlled(ctx, target,
					plan.GroupModelPricing.ExpectedRevision, plan.GroupModelPricing.DesiredContract, func() error {
						if _, _, _, sourceErr := s.validateFrozenListingSource(ctx, plan, "before the group-model pricing write"); sourceErr != nil {
							return sourceErr
						}
						guard, guardErr := adapter.Snapshot(ctx, target)
						if guardErr != nil {
							return guardErr
						}
						if guard.Hash != plan.DesiredTargetHash {
							return errors.New("target GroupRatio changed before the group-model pricing write")
						}
						return nil
					})
				if applyErr != nil {
					return fmt.Errorf("apply target group-model pricing: %w", applyErr)
				}
				if result.ResultState != "verified_success" || result.ReadbackHash != plan.GroupModelPricing.DesiredRevision {
					return errors.New("target group-model pricing write did not produce a verified desired revision")
				}
				change.GroupModelPricing.AppliedRevision = result.ReadbackHash
			case plan.GroupModelPricing.DesiredRevision:
				// Recovery after a successful remote write and before the local
				// checkpoint. The immutable plan proves the desired content.
				change.GroupModelPricing.AppliedRevision = current.GroupModelPricing.Revision
			default:
				return staleError{"target group-model pricing changed before apply"}
			}
			change.GroupModelPricing.Changed = plan.GroupModelPricing.ExpectedRevision != plan.GroupModelPricing.DesiredRevision
			change.Step = "contract_confirmed"
			if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
				return err
			}
			s.activity(ctx, plan, change, "listing.group_model_pricing_confirmed", "info", "Per-model pricing contract confirmed",
				fmt.Sprintf("revision=%s models=%d minimum_margin=%s", change.GroupModelPricing.AppliedRevision,
					len(plan.GroupModelPricing.Models), plan.GroupModelPricing.MinimumMarginRate))
		} else if current.GroupModelPricing.Revision != plan.GroupModelPricing.DesiredRevision {
			return staleError{"target group-model pricing changed before channel enable"}
		}
	}

	globalModelPrice := plan.GroupModelPricing.GlobalModelPrice
	if pricingOptionAdapter != nil {
		current, err = adapter.Snapshot(ctx, target)
		if err != nil {
			return err
		}
		currentOption, exists := current.PricingOptions["ModelPrice"]
		if !exists {
			return staleError{"target ModelPrice became unreadable before apply"}
		}
		if change.GroupModelPricing.GlobalModelPrice.AppliedHash == "" {
			switch currentOption.Hash {
			case globalModelPrice.ExpectedHash:
				if globalModelPrice.Changed {
					readback, applyErr := pricingOptionAdapter.ApplyPricingOption(ctx, target, "ModelPrice",
						globalModelPrice.ExpectedHash, globalModelPrice.Desired)
					if applyErr != nil {
						return fmt.Errorf("add missing target ModelPrice entries: %w", applyErr)
					}
					if readback.Hash != globalModelPrice.DesiredHash {
						return errors.New("target ModelPrice write did not produce the desired hash")
					}
				}
				change.GroupModelPricing.GlobalModelPrice.AppliedHash = globalModelPrice.DesiredHash
			case globalModelPrice.DesiredHash:
				change.GroupModelPricing.GlobalModelPrice.AppliedHash = currentOption.Hash
			default:
				return staleError{"target ModelPrice changed before apply"}
			}
			change.Step = "global_model_price_confirmed"
			if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
				return err
			}
			if len(globalModelPrice.Added) > 0 {
				s.activity(ctx, plan, change, "listing.global_model_price_confirmed", "info",
					"Missing global model prices added", fmt.Sprintf("models=%d; existing prices preserved", len(globalModelPrice.Added)))
			}
		} else if currentOption.Hash != globalModelPrice.DesiredHash {
			return staleError{"target ModelPrice changed before channel enable"}
		}
	}

	if visibilityAdapter != nil {
		current, err = adapter.Snapshot(ctx, target)
		if err != nil {
			return err
		}
		visibility := plan.GroupModelPricing.GroupVisibility
		if change.GroupModelPricing.GroupVisibility.AppliedHash == "" {
			switch current.UserUsableGroupsHash {
			case visibility.ExpectedHash:
				readback, applyErr := visibilityAdapter.ApplyGroupVisibility(ctx, target,
					visibility.ExpectedHash, visibility.Desired)
				if applyErr != nil {
					return fmt.Errorf("apply target UserUsableGroups: %w", applyErr)
				}
				if readback.Hash != visibility.DesiredHash {
					return errors.New("target UserUsableGroups write did not produce the desired hash")
				}
				change.GroupModelPricing.GroupVisibility.AppliedHash = readback.Hash
			case visibility.DesiredHash:
				change.GroupModelPricing.GroupVisibility.AppliedHash = current.UserUsableGroupsHash
			default:
				return staleError{"target UserUsableGroups changed before apply"}
			}
			change.GroupModelPricing.GroupVisibility.Changed = visibility.Changed
			change.Step = "visibility_confirmed"
			if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
				return err
			}
			s.activity(ctx, plan, change, "listing.group_visibility_confirmed", "info", "User-visible group confirmed",
				fmt.Sprintf("group=%s hash=%s", plan.TargetGroup, visibility.DesiredHash))
		} else if current.UserUsableGroupsHash != visibility.DesiredHash {
			return staleError{"target UserUsableGroups changed before channel enable"}
		}
	}

	// Listing never sends a billable model request. Exact target readback proves
	// the configuration before the owned channel is enabled; runtime failures
	// remain visible through normal collection and channel logs.
	if !stepAtLeast(change.Step, "configuration_verified_after_pricing") {
		verified, err := adapter.Snapshot(ctx, target)
		if err != nil {
			return err
		}
		if verified.Hash != plan.DesiredTargetHash {
			return staleError{"target GroupRatio changed during final configuration readback"}
		}
		if groupModelAdapter != nil && (verified.GroupModelPricing == nil ||
			verified.GroupModelPricing.Revision != plan.GroupModelPricing.DesiredRevision) {
			return staleError{"target group-model pricing changed during final configuration readback"}
		}
		if pricingOptionAdapter != nil &&
			verified.PricingOptions["ModelPrice"].Hash != globalModelPrice.DesiredHash {
			return staleError{"target ModelPrice changed during final configuration readback"}
		}
		if visibilityAdapter != nil && verified.UserUsableGroupsHash != plan.GroupModelPricing.GroupVisibility.DesiredHash {
			return staleError{"target UserUsableGroups changed during final configuration readback"}
		}
		change.Step = "configuration_verified_after_pricing"
		if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
			return err
		}
		s.activity(ctx, plan, change, "listing.configuration_verified", "info",
			"Channel configuration verified without a billable model request", fmt.Sprintf("models=%d", len(plan.Channel.Models)))
	}

	if _, _, _, err := s.validateFrozenListingSource(ctx, plan, "before channel enable"); err != nil {
		return err
	}
	if listingUsesPricingMapping(plan.PricingRule) && !change.PolicyActivated {
		if err := s.store.ActivateListingPolicy(ctx, change.ID, boundPolicy.ID); err != nil {
			return fmt.Errorf("activate continuing pricing policy: %w", err)
		}
		change.PolicyActivated = true
		change.Step = "policy_activated"
		s.activity(ctx, plan, change, "listing.policy_activated", "info", "Continuing pricing policy activated",
			fmt.Sprintf("policy_id=%d", boundPolicy.ID))
	}
	if _, err := channelAdapter.EnableOwnedChannel(ctx, target, *change.CreatedChannelID, change.OwnershipMarker); err != nil {
		return err
	}
	change.ChannelEnabled = true
	change.Step = "channel_enabled_readback"
	if err := s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"}); err != nil {
		return err
	}
	if err := s.ensureNativeMirrorOwnership(ctx, plan, change); err != nil {
		if _, disableErr := channelAdapter.DisableOwnedChannel(ctx, target, *change.CreatedChannelID,
			change.OwnershipMarker); disableErr == nil {
			change.ChannelEnabled = false
			change.Step = "mirror_adoption_failed_channel_disabled"
			_ = s.store.UpdateListingChangeSet(ctx, *change, []string{"applying"})
		} else {
			err = fmt.Errorf("%w; owned channel could not be disabled after adoption failure: %v", err, disableErr)
		}
		return fmt.Errorf("adopt complete mirror ownership: %w", err)
	}
	return s.finalizeApplied(plan, change)
}

func (s *Service) compensate(ctx context.Context, plan domain.ListingPlan, change *domain.ListingChangeSet) string {
	target, adapter, channelAdapter, _, err := s.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		change.RollbackStatus = "inspection_failed"
		return err.Error()
	}
	results := make([]string, 0, 2)
	checkpointEnabled := change.ChannelEnabled
	channelID := change.CreatedChannelID
	if channelID == nil {
		if channel, found, findErr := channelAdapter.FindOwnedChannel(ctx, target, change.OwnershipMarker); findErr == nil && found {
			channelID = &channel.ID
			if channel.Status == 1 {
				change.ChannelEnabled = true
			}
		}
	} else if channel, getErr := channelAdapter.GetChannel(ctx, target, *channelID); getErr == nil && channel.Remark == change.OwnershipMarker && channel.Status == 1 {
		change.ChannelEnabled = true
	}
	if change.ChannelEnabled && !checkpointEnabled && channelID != nil {
		if _, disableErr := channelAdapter.DisableOwnedChannel(ctx, target, *channelID, change.OwnershipMarker); disableErr != nil {
			results = append(results, "unexpected enabled channel could not be disabled: "+disableErr.Error())
		} else {
			change.ChannelEnabled = false
			results = append(results, "unexpected enabled channel returned to disabled")
		}
	}
	if channelID != nil && !change.ChannelEnabled {
		if err := channelAdapter.DeleteOwnedDisabledChannel(ctx, target, *channelID, change.OwnershipMarker); err != nil {
			results = append(results, "channel preserved: "+err.Error())
		} else {
			results = append(results, "owned disabled channel deleted")
		}
	}
	visibility := plan.GroupModelPricing.GroupVisibility
	visibilityReceipt := &change.GroupModelPricing.GroupVisibility
	if !change.ChannelEnabled && visibility.Required && visibilityReceipt.AppliedHash != "" {
		visibilityAdapter, adapterErr := targets.AsGroupVisibilityAdapter(adapter)
		if adapterErr != nil {
			results = append(results, "UserUsableGroups rollback unavailable: "+adapterErr.Error())
		} else if currentVisibility, readErr := visibilityAdapter.ReadGroupVisibility(ctx, target); readErr != nil {
			results = append(results, "UserUsableGroups rollback inspection failed: "+readErr.Error())
		} else if currentVisibility.Hash == visibility.ExpectedHash {
			visibilityReceipt.RollbackStatus = "already at the preview hash"
			results = append(results, "UserUsableGroups already at the preview hash")
		} else if currentVisibility.Hash != visibilityReceipt.AppliedHash {
			visibilityReceipt.RollbackStatus = "skipped because an external visibility change was detected"
			results = append(results, "UserUsableGroups rollback skipped because an external change was detected")
		} else if rolledBack, rollbackErr := visibilityAdapter.RollbackGroupVisibility(ctx, target,
			visibilityReceipt.AppliedHash, visibility.Expected); rollbackErr != nil {
			visibilityReceipt.RollbackStatus = "failed: " + rollbackErr.Error()
			results = append(results, "UserUsableGroups conditional rollback failed: "+rollbackErr.Error())
		} else {
			visibilityReceipt.RollbackStatus = "restored hash " + rolledBack.Hash
			results = append(results, "UserUsableGroups conditionally rolled back")
		}
	}
	if change.ChannelEnabled {
		results = append(results, "owned channel is enabled; channel, GroupRatio and per-model pricing were preserved")
	} else if plan.GroupModelPricing.GlobalModelPrice.Supported &&
		change.GroupModelPricing.GlobalModelPrice.AppliedHash != "" {
		status, rollbackErr := rollbackListingGlobalModelPrices(ctx, adapter, target,
			plan.GroupModelPricing.GlobalModelPrice, &change.GroupModelPricing.GlobalModelPrice)
		if rollbackErr != nil {
			results = append(results, "global ModelPrice rollback failed: "+rollbackErr.Error())
		} else if status != "" {
			results = append(results, status)
		}
	}
	if !change.ChannelEnabled && plan.GroupModelPricing.Mode == "atomic_contract" {
		groupModelAdapter, adapterErr := targets.AsGroupModelPricingAdapter(adapter)
		if adapterErr != nil {
			results = append(results, "group-model pricing rollback unavailable: "+adapterErr.Error())
		} else if currentContract, readErr := groupModelAdapter.ReadGroupModelPricing(ctx, target); readErr != nil {
			results = append(results, "group-model pricing rollback inspection failed: "+readErr.Error())
		} else if currentContract.Revision == plan.GroupModelPricing.ExpectedRevision {
			change.GroupModelPricing.RollbackStatus = "already at the preview revision"
			results = append(results, "group-model pricing already at the preview revision")
		} else if currentContract.Revision != plan.GroupModelPricing.DesiredRevision {
			currentGroup := currentContract.Groups[plan.TargetGroup]
			desiredGroup := plan.GroupModelPricing.DesiredContract.Groups[plan.TargetGroup]
			if !reflect.DeepEqual(currentGroup, desiredGroup) {
				change.GroupModelPricing.RollbackStatus = "skipped because the owned target group changed externally"
				results = append(results, "group-model pricing rollback skipped because the owned target group changed externally")
			} else if scoped, cloneErr := cloneGroupModelContract(currentContract); cloneErr != nil {
				change.GroupModelPricing.RollbackStatus = "failed: " + cloneErr.Error()
				results = append(results, "group-model pricing scoped rollback failed: "+cloneErr.Error())
			} else {
				if original, existed := plan.GroupModelPricing.ExpectedContract.Groups[plan.TargetGroup]; existed {
					if scoped.Groups == nil {
						scoped.Groups = map[string]map[string]domain.GroupModelPricingSetting{}
					}
					scoped.Groups[plan.TargetGroup] = original
				} else {
					delete(scoped.Groups, plan.TargetGroup)
				}
				rolledBack, rollbackErr := groupModelAdapter.ApplyGroupModelPricingControlled(ctx, target,
					currentContract.Revision, scoped, func() error { return nil })
				if rollbackErr != nil {
					change.GroupModelPricing.RollbackStatus = "failed: " + rollbackErr.Error()
					results = append(results, "group-model pricing scoped rollback failed: "+rollbackErr.Error())
				} else {
					change.GroupModelPricing.RollbackStatus = "restored target group at revision " + rolledBack.ReadbackHash
					results = append(results, "group-model pricing target group conditionally rolled back")
				}
			}
		} else if rolledBack, rollbackErr := groupModelAdapter.RollbackGroupModelPricing(ctx, target,
			plan.GroupModelPricing.DesiredRevision, plan.GroupModelPricing.ExpectedContract); rollbackErr != nil {
			change.GroupModelPricing.RollbackStatus = "failed: " + rollbackErr.Error()
			results = append(results, "group-model pricing conditional rollback failed: "+rollbackErr.Error())
		} else {
			change.GroupModelPricing.RollbackStatus = "restored revision " + rolledBack.Revision
			results = append(results, "group-model pricing conditionally rolled back")
		}
	}
	if !change.ChannelEnabled && change.RatioChanged && change.AppliedTargetHash != "" {
		current, snapshotErr := adapter.Snapshot(ctx, target)
		if snapshotErr != nil {
			results = append(results, "GroupRatio rollback inspection failed: "+snapshotErr.Error())
		} else if !listingDecimalEqual(current.GroupRatio[plan.TargetGroup], plan.ProposedGroupRatio) {
			results = append(results, "GroupRatio rollback skipped because the owned target group changed externally")
		} else {
			desiredRatios := cloneRatios(current.GroupRatio)
			if original, existed := plan.ExpectedGroupRatios[plan.TargetGroup]; existed {
				desiredRatios[plan.TargetGroup] = original
			} else {
				delete(desiredRatios, plan.TargetGroup)
			}
			if _, rollbackErr := adapter.ApplyGroupRatio(ctx, target, current.Hash, desiredRatios); rollbackErr != nil {
				results = append(results, "GroupRatio conditional rollback failed: "+rollbackErr.Error())
			} else {
				results = append(results, "GroupRatio conditionally rolled back")
			}
		}
	}
	if !change.ChannelEnabled && change.PricingPolicyID != nil {
		if policyResult, policyErr := s.store.RestoreListingPolicy(ctx, change.ID); policyErr != nil {
			results = append(results, "pricing policy restore failed: "+policyErr.Error())
		} else {
			results = append(results, policyResult)
			change.PricingPolicyID = nil
			change.PolicyCreated = false
			change.PolicyActivated = false
		}
	}
	change.RollbackStatus = strings.Join(results, "; ")
	return change.RollbackStatus
}

func rollbackListingGlobalModelPrices(ctx context.Context, adapter targets.Adapter, target domain.TargetSite,
	plan domain.ListingGlobalModelPricePlan, receipt *domain.ListingGlobalModelPriceReceipt) (string, error) {
	if !plan.Supported || len(plan.Added) == 0 {
		receipt.RollbackStatus = "no global ModelPrice additions"
		return receipt.RollbackStatus, nil
	}
	pricingAdapter, err := targets.AsPricingOptionAdapter(adapter)
	if err != nil {
		return "", err
	}
	snapshot, err := adapter.Snapshot(ctx, target)
	if err != nil {
		return "", err
	}
	current, exists := snapshot.PricingOptions["ModelPrice"]
	if !exists {
		return "", errors.New("target ModelPrice option is unavailable")
	}
	desired := cloneRatios(current.Values)
	removed := 0
	for model, addedValue := range plan.Added {
		value, present := desired[model]
		if !present {
			continue
		}
		if !listingDecimalEqual(value, addedValue) {
			receipt.RollbackStatus = "skipped because an added ModelPrice changed externally"
			return receipt.RollbackStatus, nil
		}
		delete(desired, model)
		removed++
	}
	if removed == 0 {
		receipt.RollbackStatus = "global ModelPrice additions already absent"
		return receipt.RollbackStatus, nil
	}
	readback, err := pricingAdapter.ApplyPricingOption(ctx, target, "ModelPrice", current.Hash, desired)
	if err != nil {
		return "", err
	}
	if readback.Hash != targets.HashDecimalMap(desired) {
		return "", errors.New("global ModelPrice rollback readback mismatch")
	}
	receipt.RollbackStatus = fmt.Sprintf("removed %d listing-added global ModelPrice entries", removed)
	return receipt.RollbackStatus, nil
}

func (s *Service) finalizeApplied(plan domain.ListingPlan, change *domain.ListingChangeSet) error {
	checkpointCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	change.Status = "applying"
	change.Step = "enabled"
	change.ChannelEnabled = true
	change.Error = ""
	now := time.Now().UTC()
	change.FinishedAt = &now
	if err := s.store.UpdateListingChangeSet(checkpointCtx, *change, []string{"applying"}); err != nil {
		return err
	}
	if plan.Status != "applied" {
		if err := s.store.TransitionListingPlan(checkpointCtx, plan.ID, []string{"applying"}, "applied", ""); err != nil {
			return err
		}
	}
	change.Status = "applied"
	if err := s.store.UpdateListingChangeSet(checkpointCtx, *change, []string{"applying"}); err != nil {
		return err
	}
	pricingPolicy := "none"
	if change.PricingPolicyID != nil {
		pricingPolicy = fmt.Sprint(*change.PricingPolicyID)
	}
	s.activity(checkpointCtx, plan, change, "listing.applied", "success", "Listing applied",
		fmt.Sprintf("channel_id=%d enabled with pricing_policy_id=%s", *change.CreatedChannelID, pricingPolicy))
	return nil
}

func (s *Service) GetPlan(ctx context.Context, id int64) (domain.ListingPlan, error) {
	return s.store.GetListingPlan(ctx, id)
}

func (s *Service) ListPlans(ctx context.Context, status string, limit int) ([]domain.ListingPlan, error) {
	return s.store.ListListingPlans(ctx, status, limit)
}

func (s *Service) GetChangeSet(ctx context.Context, id int64) (domain.ListingChangeSet, error) {
	return s.store.GetListingChangeSet(ctx, id)
}

func (s *Service) ListChangeSets(ctx context.Context, planID int64, limit int) ([]domain.ListingChangeSet, error) {
	return s.store.ListListingChangeSets(ctx, planID, limit)
}

// RecoverIncomplete resumes only change sets that had already entered the
// listing state machine. It never creates a new listing or bypasses approval.
func (s *Service) RecoverIncomplete(ctx context.Context) error {
	if err := s.remoteWrites.Check("incomplete listing recovery"); err != nil {
		return err
	}
	changes, err := s.store.ListListingChangeSets(ctx, 0, 500)
	if err != nil {
		return err
	}
	errorsFound := make([]error, 0)
	for _, change := range changes {
		switch change.Status {
		case "pending", "applying":
			if _, applyErr := s.Apply(ctx, change.PlanID, ApplyInput{IdempotencyKey: change.IdempotencyKey, ConfirmEnable: true}); applyErr != nil {
				errorsFound = append(errorsFound, fmt.Errorf("recover listing change %d: %w", change.ID, applyErr))
			}
		case "rolling_back":
			if _, rollbackErr := s.Rollback(ctx, change.ID, RollbackInput{ConfirmRollback: true}); rollbackErr != nil {
				errorsFound = append(errorsFound, fmt.Errorf("recover listing rollback %d: %w", change.ID, rollbackErr))
			}
		}
	}
	return errors.Join(errorsFound...)
}

func (s *Service) loadTarget(ctx context.Context, id int64) (domain.TargetSite, targets.Adapter, targets.ChannelAdapter, domain.TargetSnapshot, error) {
	target, err := s.store.GetTarget(ctx, id)
	if err != nil {
		return domain.TargetSite{}, nil, nil, domain.TargetSnapshot{}, fmt.Errorf("target: %w", err)
	}
	if !target.Enabled {
		return domain.TargetSite{}, nil, nil, domain.TargetSnapshot{}, errors.New("target site is disabled")
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		return domain.TargetSite{}, nil, nil, domain.TargetSnapshot{}, err
	}
	channelAdapter, err := targets.AsChannelAdapter(adapter)
	if err != nil {
		return domain.TargetSite{}, nil, nil, domain.TargetSnapshot{}, err
	}
	snapshot, err := adapter.Snapshot(ctx, target)
	if err != nil {
		return domain.TargetSite{}, nil, nil, domain.TargetSnapshot{}, err
	}
	return target, adapter, channelAdapter, snapshot, nil
}

func (s *Service) loadUpstreamKey(ctx context.Context, supplier domain.Supplier, credentialID int64, channelBaseURL string) (domain.Credential, string, error) {
	if supplier.CredentialID != nil && *supplier.CredentialID == credentialID {
		return domain.Credential{}, "", errors.New("the supplier login credential cannot be reused as the channel API key credential")
	}
	credential, payload, err := s.credentials.Load(ctx, credentialID)
	if err != nil {
		return domain.Credential{}, "", fmt.Errorf("load upstream API key credential: %w", err)
	}
	kind := strings.ToLower(strings.TrimSpace(credential.Kind))
	if !strings.Contains(kind, "api") || strings.Contains(kind, "login") || strings.Contains(kind, "browser") ||
		strings.Contains(kind, "cookie") || strings.Contains(kind, "session") {
		return domain.Credential{}, "", fmt.Errorf("credential %d kind %q is not a dedicated API key", credentialID, credential.Kind)
	}
	if credential.Status != "" && credential.Status != "active" {
		return domain.Credential{}, "", fmt.Errorf("upstream API key credential is %s", credential.Status)
	}
	if credential.ExpiresAt != nil && !credential.ExpiresAt.After(time.Now().UTC()) {
		return domain.Credential{}, "", errors.New("upstream API key credential has expired")
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, channelBaseURL); err != nil {
		return domain.Credential{}, "", fmt.Errorf("upstream API key credential origin mismatch: %w", err)
	}
	key := strings.TrimSpace(payload.Authorization)
	if strings.EqualFold(payload.AuthorizationMode, "header") {
		key = strings.TrimSpace(payload.Headers[payload.AuthorizationHeader])
	}
	if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return domain.Credential{}, "", errors.New("dedicated upstream API key credential contains no usable key")
	}
	return credential, key, nil
}

func normalizeRequest(request domain.ListingPlanRequest) domain.ListingPlanRequest {
	// Ownership identity is server-derived from the immutable preview request;
	// callers cannot submit another policy's provenance marker.
	request.PricingRule.MirrorPolicyUUID = ""
	request.DiscoveryKind = strings.ToLower(strings.TrimSpace(request.DiscoveryKind))
	request.DiscoveryExternalID = strings.TrimSpace(request.DiscoveryExternalID)
	request.SourceSnapshotHash = strings.TrimSpace(request.SourceSnapshotHash)
	request.ExpectedSourceRatio = strings.TrimSpace(request.ExpectedSourceRatio)
	request.TargetGroupMode = strings.ToLower(strings.TrimSpace(request.TargetGroupMode))
	request.TargetGroup = strings.TrimSpace(request.TargetGroup)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Channel.Name = strings.TrimSpace(request.Channel.Name)
	request.Channel.BaseURL = strings.TrimRight(strings.TrimSpace(request.Channel.BaseURL), "/")
	request.Channel.Models = sortedUnique(request.Channel.Models)
	request.Channel.TestModel = ""
	request.Channel.Tag = strings.TrimSpace(request.Channel.Tag)
	request.PricingRule.PriceAnchor = strings.ToLower(strings.TrimSpace(request.PricingRule.PriceAnchor))
	request.PricingRule.GroupRatioRuleType = strings.ToLower(strings.TrimSpace(request.PricingRule.GroupRatioRuleType))
	request.PricingRule.GroupRatioRuleValue = strings.TrimSpace(request.PricingRule.GroupRatioRuleValue)
	request.PricingRule.ModelPriceRuleType = strings.ToLower(strings.TrimSpace(request.PricingRule.ModelPriceRuleType))
	request.PricingRule.ModelPriceRuleValue = strings.TrimSpace(request.PricingRule.ModelPriceRuleValue)
	request.PricingRule.RuleType = strings.ToLower(strings.TrimSpace(request.PricingRule.RuleType))
	request.PricingRule.Value = strings.TrimSpace(request.PricingRule.Value)
	request.PricingRule.ExecutionMode = strings.ToLower(strings.TrimSpace(request.PricingRule.ExecutionMode))
	request.PricingRule.MirrorExecutionMode = strings.ToLower(strings.TrimSpace(request.PricingRule.MirrorExecutionMode))
	request.PricingRule.ModelPriceRoundingStep = strings.TrimSpace(request.PricingRule.ModelPriceRoundingStep)
	request.PricingRule.ModelScopeMode = strings.ToLower(strings.TrimSpace(request.PricingRule.ModelScopeMode))
	request.PricingRule.EmptyGroupAction = strings.ToLower(strings.TrimSpace(request.PricingRule.EmptyGroupAction))
	if len(request.PricingRule.MirrorExcludedModels) > 0 {
		request.PricingRule.MirrorExcludedModels = sortedUnique(request.PricingRule.MirrorExcludedModels)
	} else {
		request.PricingRule.MirrorExcludedModels = nil
	}
	return request
}

func validateRequest(request domain.ListingPlanRequest) error {
	if request.SupplierID <= 0 || request.TargetSiteID <= 0 || request.UpstreamCredentialID <= 0 {
		return errors.New("supplier_id, target_site_id and upstream_credential_id are required")
	}
	if request.DiscoveryExternalID == "" {
		return errors.New("an upstream group must be selected explicitly")
	}
	switch request.DiscoveryKind {
	case "group":
	case "catalog_group":
		if !request.ConfirmCatalogGroup {
			return errors.New("confirm_catalog_group=true is required for a complete-catalog selection")
		}
		if request.SourceSnapshotHash == "" {
			return errors.New("source_snapshot_hash is required for a complete-catalog selection")
		}
		if request.ExpectedSourceRatio == "" {
			return errors.New("expected_source_ratio is required for a complete-catalog selection")
		}
	default:
		return errors.New("discovery_kind must be group or catalog_group")
	}
	if request.TargetGroupMode != "new" && request.TargetGroupMode != "existing" {
		return errors.New("target_group_mode must be new or existing")
	}
	if request.TargetGroup == "" || strings.Contains(request.TargetGroup, ",") {
		return errors.New("exactly one target group is required")
	}
	if request.Channel.Name == "" || request.Channel.Type <= 0 || request.Channel.BaseURL == "" {
		return errors.New("channel name, positive type and base_url are required")
	}
	if _, err := netguard.ParseOrigin(request.Channel.BaseURL); err != nil {
		return err
	}
	if len(request.Channel.Models) == 0 {
		return errors.New("an explicit non-empty model scope is required")
	}
	anchor, err := listingPriceAnchor(request.PricingRule)
	if err != nil {
		return err
	}
	if _, err := listingGroupRatioRule(request.PricingRule, anchor); err != nil {
		return fmt.Errorf("pricing_rule group ratio: %w", err)
	}
	if _, err := listingModelPriceRule(request.PricingRule, anchor); err != nil {
		return fmt.Errorf("pricing_rule model price: %w", err)
	}
	if request.PricingRule.ExecutionMode != "" && request.PricingRule.ExecutionMode != "approval" && request.PricingRule.ExecutionMode != "auto" {
		return errors.New("pricing_rule.execution_mode must be approval or auto")
	}
	if request.PricingRule.MirrorExecutionMode != "" && request.PricingRule.MirrorExecutionMode != "approval" && request.PricingRule.MirrorExecutionMode != "auto" {
		return errors.New("pricing_rule.mirror_execution_mode must be approval or auto")
	}
	if err := validateListingMirrorOptions(request.PricingRule); err != nil {
		return fmt.Errorf("pricing_rule complete mirror: %w", err)
	}
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 160 {
		return errors.New("a bounded idempotency_key is required")
	}
	return nil
}

func validateListingMirrorOptions(rule domain.ListingPricingRule) error {
	fields := map[string]*bool{
		"sync_group_ratio":     rule.SyncGroupRatio,
		"sync_model_prices":    rule.SyncModelPrices,
		"sync_billing_mode":    rule.SyncBillingMode,
		"sync_model_additions": rule.SyncModelAdditions,
		"sync_model_removals":  rule.SyncModelRemovals,
	}
	configured := strings.TrimSpace(rule.ModelScopeMode) != "" || strings.TrimSpace(rule.EmptyGroupAction) != ""
	for _, value := range fields {
		configured = configured || value != nil
	}
	if !configured {
		// Older clients and persisted previews retain their original price-only
		// authority. Missing fields must never grant model-set deletion rights.
		return nil
	}
	missing := make([]string, 0)
	for name, value := range fields {
		if value == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("all mirror switches must be explicit; missing %s", strings.Join(missing, ", "))
	}
	switch rule.ModelScopeMode {
	case "mirror_group", "fixed_allowlist":
	default:
		return errors.New("model_scope_mode must be mirror_group or fixed_allowlist")
	}
	switch rule.EmptyGroupAction {
	case "preserve", "disable_managed_channel", "remove_managed_models":
	default:
		return errors.New("empty_group_action must be preserve, disable_managed_channel, or remove_managed_models")
	}
	if *rule.SyncBillingMode && !*rule.SyncModelPrices {
		return errors.New("billing-mode synchronization requires model-price synchronization")
	}
	if *rule.SyncModelAdditions && (!*rule.SyncModelPrices || !*rule.SyncBillingMode) {
		return errors.New("model additions require model-price and billing-mode synchronization")
	}
	if (*rule.SyncModelAdditions || *rule.SyncModelRemovals) && rule.ModelScopeMode != "mirror_group" {
		return errors.New("model additions or removals require mirror_group scope")
	}
	if rule.EmptyGroupAction == "disable_managed_channel" && !*rule.SyncModelRemovals {
		return errors.New("managed-channel disable requires model-removal synchronization")
	}
	return nil
}

func (s *Service) resolvePreviewSource(ctx context.Context, supplier domain.Supplier,
	request domain.ListingPlanRequest) (domain.CatalogSnapshot, domain.GroupOffer, error) {
	if request.DiscoveryKind == "group" {
		discovery, err := s.store.GetDiscovery(ctx, request.SupplierID, "group", request.DiscoveryExternalID)
		if err != nil {
			return domain.CatalogSnapshot{}, domain.GroupOffer{}, fmt.Errorf("discovery: %w", err)
		}
		if discovery.Baseline {
			return domain.CatalogSnapshot{}, domain.GroupOffer{}, errors.New("the selected discovery is part of the first catalog baseline, not a newly discovered upstream group")
		}
		snapshot, err := s.effectiveListingSnapshot(ctx, supplier)
		if err != nil {
			return domain.CatalogSnapshot{}, domain.GroupOffer{}, fmt.Errorf("latest supplier snapshot: %w", err)
		}
		group, err := findGroup(snapshot, request.DiscoveryExternalID)
		return snapshot, group, err
	}

	if !supplier.Enabled {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, errors.New("supplier is disabled; enable it before selecting a complete-catalog group")
	}
	snapshot, err := s.effectiveListingSnapshot(ctx, supplier)
	if err != nil {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, fmt.Errorf("latest supplier snapshot: %w", err)
	}
	if strings.TrimSpace(snapshot.Evidence.ResponseHash) == "" || request.SourceSnapshotHash != snapshot.Evidence.ResponseHash {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, errors.New("source_snapshot_hash is stale; refresh the supplier catalog and select the group again")
	}
	if err := validateCatalogEvidenceFreshness(snapshot, time.Now().UTC()); err != nil {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, err
	}
	group, err := findGroup(snapshot, request.DiscoveryExternalID)
	if err != nil {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, err
	}
	expectedRatio, err := nonnegativeDecimal(request.ExpectedSourceRatio, "expected source ratio")
	if err != nil {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, err
	}
	currentRatio, err := nonnegativeDecimal(ratios.DecisionRatio(group), "current source group ratio")
	if err != nil {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, err
	}
	if !expectedRatio.Equal(currentRatio) {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, errors.New("source effective ratio changed after selection; refresh the supplier catalog and confirm the group again")
	}
	groupModels := sortedUnique(group.ModelIDs)
	if len(groupModels) == 0 {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, fmt.Errorf("catalog group %q has no models and cannot be listed", group.ExternalID)
	}
	if outside := missingModels(request.Channel.Models, groupModels); len(outside) > 0 {
		return domain.CatalogSnapshot{}, domain.GroupOffer{}, fmt.Errorf("selected models are outside catalog group %q: %s",
			group.ExternalID, strings.Join(outside, ", "))
	}
	return snapshot, group, nil
}

func (s *Service) effectiveListingSnapshot(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error) {
	effective, err := s.store.LatestEffectiveSnapshot(ctx, supplier.ID)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	return effective, nil
}

func (s *Service) loadEffectiveListingSource(ctx context.Context, supplierID int64) (domain.Supplier, domain.CatalogSnapshot, error) {
	supplier, err := s.store.GetSupplier(ctx, supplierID)
	if err != nil {
		return domain.Supplier{}, domain.CatalogSnapshot{}, err
	}
	snapshot, err := s.effectiveListingSnapshot(ctx, supplier)
	if err != nil {
		return domain.Supplier{}, domain.CatalogSnapshot{}, err
	}
	return supplier, snapshot, nil
}

func (s *Service) validateFrozenListingSource(ctx context.Context, plan domain.ListingPlan, stage string) (
	domain.Supplier, domain.CatalogSnapshot, domain.GroupOffer, error,
) {
	supplier, snapshot, err := s.loadEffectiveListingSource(ctx, plan.SupplierID)
	if err != nil {
		return domain.Supplier{}, domain.CatalogSnapshot{}, domain.GroupOffer{}, err
	}
	if snapshot.Evidence.ResponseHash != plan.SourceSnapshotHash {
		return supplier, snapshot, domain.GroupOffer{}, staleError{fmt.Sprintf("supplier pricing catalog changed %s", stage)}
	}
	if plan.DiscoveryKind == "catalog_group" {
		if !supplier.Enabled {
			return supplier, snapshot, domain.GroupOffer{}, staleError{fmt.Sprintf("supplier was disabled %s", stage)}
		}
		if err := validateFrozenCatalogEvidence(plan, snapshot, time.Now().UTC()); err != nil {
			return supplier, snapshot, domain.GroupOffer{}, staleError{err.Error()}
		}
	}
	group, err := findGroup(snapshot, plan.SourceGroup)
	if err != nil {
		return supplier, snapshot, domain.GroupOffer{}, staleError{fmt.Sprintf("supplier source group changed %s", stage)}
	}
	currentRatio, err := nonnegativeDecimal(ratios.DecisionRatio(group), "current source group ratio")
	previewRatio, previewErr := nonnegativeDecimal(plan.SourceRatio, "preview source group ratio")
	if err != nil || previewErr != nil || !currentRatio.Equal(previewRatio) {
		return supplier, snapshot, group, staleError{fmt.Sprintf("supplier recharge factor or effective source ratio changed %s", stage)}
	}
	if expectedFactor := listingPlanRechargeFactor(plan); expectedFactor != "" && group.RechargeValueFactor != expectedFactor {
		return supplier, snapshot, group, staleError{fmt.Sprintf("supplier recharge factor changed %s", stage)}
	}
	return supplier, snapshot, group, nil
}

func listingPlanRechargeFactor(plan domain.ListingPlan) string {
	for _, model := range plan.GroupModelPricing.Models {
		if factor := strings.TrimSpace(model.RechargeValueFactor); factor != "" {
			return factor
		}
	}
	return ""
}

func validateFrozenCatalogEvidence(plan domain.ListingPlan, snapshot domain.CatalogSnapshot, now time.Time) error {
	if plan.SourceObservedAt.IsZero() {
		return errors.New("listing preview has no frozen source observation time")
	}
	if now.After(plan.SourceObservedAt.Add(catalogListingFreshnessWindow)) {
		return fmt.Errorf("listing preview source evidence expired at %s; generate a new preview before applying",
			plan.SourceObservedAt.Add(catalogListingFreshnessWindow).UTC().Format(time.RFC3339))
	}
	if err := validateCatalogEvidenceFreshness(snapshot, now); err != nil {
		return err
	}
	if snapshot.Evidence.ObservedAt.Before(plan.SourceObservedAt) {
		return errors.New("latest supplier evidence is older than the listing preview source")
	}
	return nil
}

func validateCatalogEvidenceFreshness(snapshot domain.CatalogSnapshot, now time.Time) error {
	observedAt := snapshot.Evidence.ObservedAt
	if observedAt.IsZero() {
		return errors.New("catalog snapshot has no observation time; collect the supplier again before listing")
	}
	if observedAt.After(now.Add(catalogFutureClockTolerance)) {
		return fmt.Errorf("catalog snapshot observation time %s is in the future; check the system clock and collect the supplier again",
			observedAt.UTC().Format(time.RFC3339))
	}
	if now.Sub(observedAt) > catalogListingFreshnessWindow {
		return fmt.Errorf("catalog snapshot is too old (observed %s; maximum age %s); collect the supplier again before listing",
			observedAt.UTC().Format(time.RFC3339), catalogListingFreshnessWindow)
	}
	return nil
}

func listingRulePolicy(rule domain.ListingPricingRule) domain.PricingPolicy {
	return domain.PricingPolicy{RuleType: rule.RuleType, Value: rule.Value, SourceBasis: "group_ratio",
		ProfitBasis:       defaultString(rule.ProfitBasis, "contribution"),
		RevenueCashFactor: defaultString(rule.RevenueCashFactor, "1"), CostCashFactor: defaultString(rule.CostCashFactor, "1"),
		RiskBufferRate: defaultString(rule.RiskBufferRate, "0"), AllowNegativeProfit: rule.AllowNegativeProfit,
		SubsidyBudgetDaily: defaultString(rule.SubsidyBudgetDaily, "0"), MinimumRatio: defaultString(rule.MinimumRatio, "0"),
		MaximumRatio: defaultString(rule.MaximumRatio, "0"), RoundingStep: defaultString(rule.RoundingStep, "0.001"),
		OnlyFollowIncrease: rule.OnlyFollowIncrease, OnlyFollowDecrease: rule.OnlyFollowDecrease,
		ExecutionMode: defaultString(rule.ExecutionMode, "approval")}
}

func listingPolicyForPlan(plan domain.ListingPlan) domain.PricingPolicy {
	policy := listingRulePolicy(plan.PricingRule)
	policy.Name = fmt.Sprintf("上游跟价 · %s → %s", plan.SourceGroup, plan.TargetGroup)
	policy.SupplierID = plan.SupplierID
	policy.SourceGroup = plan.SourceGroup
	models := sortedUnique(append(append([]string(nil), plan.Channel.Models...), plan.PricingRule.MirrorExcludedModels...))
	for index := range models {
		models[index] = literalGlob(models[index])
	}
	policy.SourceModelPattern = strings.Join(models, ",")
	policy.TargetSiteID = plan.TargetSiteID
	policy.TargetGroup = plan.TargetGroup
	policy.Enabled = false
	return policy
}

func equivalentPolicyDefinition(left, right domain.PricingPolicy) bool {
	leftDefaults := listingRulePolicy(domain.ListingPricingRule{
		RuleType: left.RuleType, Value: left.Value, ExecutionMode: left.ExecutionMode, ProfitBasis: left.ProfitBasis,
		RevenueCashFactor: left.RevenueCashFactor, CostCashFactor: left.CostCashFactor, RiskBufferRate: left.RiskBufferRate,
		AllowNegativeProfit: left.AllowNegativeProfit, SubsidyBudgetDaily: left.SubsidyBudgetDaily,
		MinimumRatio: left.MinimumRatio, MaximumRatio: left.MaximumRatio, RoundingStep: left.RoundingStep,
		OnlyFollowIncrease: left.OnlyFollowIncrease, OnlyFollowDecrease: left.OnlyFollowDecrease,
	})
	rightDefaults := listingRulePolicy(domain.ListingPricingRule{
		RuleType: right.RuleType, Value: right.Value, ExecutionMode: right.ExecutionMode, ProfitBasis: right.ProfitBasis,
		RevenueCashFactor: right.RevenueCashFactor, CostCashFactor: right.CostCashFactor, RiskBufferRate: right.RiskBufferRate,
		AllowNegativeProfit: right.AllowNegativeProfit, SubsidyBudgetDaily: right.SubsidyBudgetDaily,
		MinimumRatio: right.MinimumRatio, MaximumRatio: right.MaximumRatio, RoundingStep: right.RoundingStep,
		OnlyFollowIncrease: right.OnlyFollowIncrease, OnlyFollowDecrease: right.OnlyFollowDecrease,
	})
	return left.SupplierID == right.SupplierID && left.SourceGroup == right.SourceGroup &&
		left.SourceModelPattern == right.SourceModelPattern && left.SourceBasis == right.SourceBasis && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && reflect.DeepEqual(leftDefaults, rightDefaults)
}

func literalGlob(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	for _, marker := range []string{"*", "?", "["} {
		value = strings.ReplaceAll(value, marker, `\`+marker)
	}
	return value
}

func verifyListingChannel(channel domain.TargetChannel, plan domain.ListingPlan, marker string) error {
	if err := verifyListingChannelIdentity(channel, plan, marker); err != nil {
		return err
	}
	if strings.Join(sortedUnique(channel.Models), "\x00") != strings.Join(sortedUnique(plan.Channel.Models), "\x00") {
		return errors.New("owned channel model scope differs from the listing plan")
	}
	return nil
}

func verifyListingChannelIdentity(channel domain.TargetChannel, plan domain.ListingPlan, marker string) error {
	if channel.Remark != marker {
		return errors.New("owned channel marker differs from the listing change set")
	}
	if channel.Name != strings.TrimSpace(plan.Channel.Name) || channel.Type != plan.Channel.Type ||
		strings.TrimRight(channel.BaseURL, "/") != strings.TrimRight(strings.TrimSpace(plan.Channel.BaseURL), "/") ||
		channel.Group != plan.TargetGroup {
		return errors.New("owned channel identity differs from the listing plan")
	}
	return nil
}

func validateSupplierChannelOrigin(supplier domain.Supplier, channelBaseURL string) error {
	allowed := supplier.BaseURL
	if configured := strings.TrimSpace(supplier.Metadata["api_base_url"]); configured != "" {
		allowed = configured
	}
	if err := netguard.RequireSameOrigin(allowed, channelBaseURL); err != nil {
		return fmt.Errorf("channel base_url is not the supplier API origin; configure supplier metadata api_base_url when the API uses another host: %w", err)
	}
	return nil
}

func findGroup(snapshot domain.CatalogSnapshot, id string) (domain.GroupOffer, error) {
	for _, group := range snapshot.Groups {
		if group.ExternalID == id {
			return group, nil
		}
	}
	return domain.GroupOffer{}, fmt.Errorf("group %q is absent from the latest supplier snapshot", id)
}

func nonnegativeDecimal(raw, name string) (decimal.Decimal, error) {
	if strings.TrimSpace(raw) == "" {
		return decimal.Zero, fmt.Errorf("%s is unknown", name)
	}
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() {
		return decimal.Zero, fmt.Errorf("%s must be a non-negative decimal", name)
	}
	return value, nil
}

func missingModels(want, got []string) []string {
	available := make(map[string]struct{}, len(got))
	for _, model := range got {
		available[model] = struct{}{}
	}
	missing := make([]string, 0)
	for _, model := range want {
		if _, ok := available[model]; !ok {
			missing = append(missing, model)
		}
	}
	return missing
}

func listingDecimalEqual(left, right string) bool {
	return decimalnorm.Equal(left, right)
}

func stepAtLeast(current, expected string) bool {
	order := map[string]int{
		"created": 0, "policy_bound": 1, "channel_created_disabled": 2,
		"models_verified": 3, "channel_tested": 4, "ratio_confirmed": 5,
		"contract_confirmed": 6, "global_model_price_confirmed": 7, "visibility_confirmed": 8,
		"channel_tested_after_pricing": 9, "configuration_verified_after_pricing": 9,
		"policy_activated": 10, "channel_enabled_readback": 11, "enabled": 12,
	}
	return order[current] >= order[expected]
}

func rollbackStepAtLeast(current, expected string) bool {
	order := map[string]int{
		"rollback_started": 0, "rollback_native_policy_disabled": 1, "rollback_native_ownership_released": 2,
		"rollback_channel_disabled": 3, "rollback_visibility_restored": 4, "rollback_global_model_price_restored": 5,
		"rollback_contract_restored": 6, "rollback_ratio_restored": 7, "rollback_channel_deleted": 8,
		"rollback_policy_restored": 9, "rolled_back": 10,
	}
	currentOrder, currentOK := order[current]
	expectedOrder, expectedOK := order[expected]
	return currentOK && expectedOK && currentOrder >= expectedOrder
}

func appendRollbackStatus(current, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return current
	}
	for _, existing := range strings.Split(current, "; ") {
		if existing == message {
			return current
		}
	}
	if strings.TrimSpace(current) == "" {
		return message
	}
	return current + "; " + message
}

func rollbackConflict(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrRollbackConflict, fmt.Sprintf(format, values...))
}

func validateListingContractPlan(plan domain.ListingPlan) error {
	contract := plan.GroupModelPricing
	if !contract.Supported || !contract.Exact || contract.ExpectedRevision == "" || contract.DesiredRevision == "" {
		return errors.New("listing plan has no complete atomic group-model pricing contract")
	}
	if contract.ExpectedContract.Revision != contract.ExpectedRevision || contract.DesiredContract.Revision != contract.DesiredRevision {
		return errors.New("listing plan group-model pricing revisions are internally inconsistent")
	}
	if len(contract.Models) == 0 || contract.MinimumMarginRate == "" {
		return errors.New("listing plan has no frozen per-model margin evidence")
	}
	global := contract.GlobalModelPrice
	if global.Supported {
		if global.ExpectedHash == "" || global.DesiredHash == "" {
			return errors.New("listing plan has no complete global ModelPrice fill contract")
		}
		if targets.HashDecimalMap(global.Expected) != global.ExpectedHash ||
			targets.HashDecimalMap(global.Desired) != global.DesiredHash ||
			global.Changed != (global.ExpectedHash != global.DesiredHash) {
			return errors.New("listing plan global ModelPrice hashes are internally inconsistent")
		}
		for model, value := range global.Added {
			if _, existed := global.Expected[model]; existed || !listingDecimalEqual(global.Desired[model], value) {
				return fmt.Errorf("listing plan global ModelPrice addition for %q is not append-only", model)
			}
		}
	}
	return validateListingVisibilityPlan(plan)
}

func validateListingVisibilityPlan(plan domain.ListingPlan) error {
	visibility := plan.GroupModelPricing.GroupVisibility
	if visibility.Required {
		if !visibility.Supported || visibility.Expected == nil || visibility.Desired == nil ||
			visibility.ExpectedHash == "" || visibility.DesiredHash == "" {
			return errors.New("listing plan has no complete user-visible group contract")
		}
		if targets.HashStringMap(visibility.Expected) != visibility.ExpectedHash ||
			targets.HashStringMap(visibility.Desired) != visibility.DesiredHash ||
			visibility.Changed != (visibility.ExpectedHash != visibility.DesiredHash) {
			return errors.New("listing plan user-visible group hashes are internally inconsistent")
		}
		if _, visible := visibility.Desired[plan.TargetGroup]; !visible {
			return errors.New("listing plan does not expose the target group to users")
		}
	}
	return nil
}

func equivalentPlans(left, right domain.ListingPlan) bool {
	left.ID, right.ID = 0, 0
	left.Status, right.Status = "", ""
	left.Error, right.Error = "", ""
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func ownershipMarker(planID int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", planID, key)))
	return fmt.Sprintf("relayops-listing:%d:%s", planID, hex.EncodeToString(sum[:8]))
}

func cloneRatios(values map[string]string) map[string]string {
	result := make(map[string]string, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func listingCostVector(models []domain.ListingModelPricingEvidence) map[string]string {
	result := make(map[string]string)
	for _, model := range models {
		for _, dimension := range model.Dimensions {
			result[model.Model+"/"+dimension.Dimension] = dimension.SourceCashCost
		}
	}
	return result
}

func uniformCostVector(values map[string]string) bool {
	var first decimal.Decimal
	set := false
	for _, raw := range values {
		value, err := decimal.NewFromString(strings.TrimSpace(raw))
		if err != nil {
			return false
		}
		if !set {
			first = value
			set = true
			continue
		}
		if !value.Equal(first) {
			return false
		}
	}
	return set
}

func stringPointer(value string) *string { return &value }

func sortedUnique(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *Service) targetLock(targetID int64) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock := s.targetLocks[targetID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.targetLocks[targetID] = lock
	}
	return lock
}

func (s *Service) activity(ctx context.Context, plan domain.ListingPlan, change *domain.ListingChangeSet, kind, severity, title, message string) {
	metadata := map[string]string{
		"plan_id":              fmt.Sprintf("%d", plan.ID),
		"target_site_id":       fmt.Sprintf("%d", plan.TargetSiteID),
		"discovery_kind":       plan.DiscoveryKind,
		"source_group":         plan.SourceGroup,
		"source_snapshot_hash": plan.SourceSnapshotHash,
	}
	if change != nil {
		metadata["change_set_id"] = fmt.Sprintf("%d", change.ID)
		metadata["step"] = change.Step
	}
	supplierID := plan.SupplierID
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: kind, Severity: severity, Title: title, Message: message,
		SupplierID: &supplierID, Metadata: metadata})
}

// planFingerprint is used by tests and diagnostics without exposing secrets.
func planFingerprint(plan domain.ListingPlan) string {
	encoded, _ := json.Marshal(plan)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
