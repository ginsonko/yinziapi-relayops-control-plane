package mappingdraft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type Service struct {
	store  *store.Store
	reader targetReader
	now    func() time.Time
}

type targetReader interface {
	Read(context.Context, domain.TargetSite) (domain.CatalogSnapshot, []domain.TargetChannel, error)
}

type liveTargetReader struct {
	factory *targets.Factory
}

func NewService(st *store.Store, factory *targets.Factory) *Service {
	return &Service{store: st, reader: liveTargetReader{factory: factory}, now: func() time.Time { return time.Now().UTC() }}
}

func (r liveTargetReader) Read(ctx context.Context, target domain.TargetSite) (domain.CatalogSnapshot, []domain.TargetChannel, error) {
	if r.factory == nil {
		return domain.CatalogSnapshot{}, nil, errors.New("target adapter factory is not configured")
	}
	adapter, err := r.factory.ForTarget(target)
	if err != nil {
		return domain.CatalogSnapshot{}, nil, err
	}
	channelAdapter, err := targets.AsChannelAdapter(adapter)
	if err != nil {
		return domain.CatalogSnapshot{}, nil, err
	}
	catalog, err := channelAdapter.PricingCatalog(ctx, target)
	if err != nil {
		return domain.CatalogSnapshot{}, nil, err
	}
	channels, err := channelAdapter.ListChannels(ctx, target)
	if err != nil {
		return domain.CatalogSnapshot{}, nil, err
	}
	return catalog, channels, nil
}

type normalizedRequest struct {
	minimumScore        int64
	minimumModelJaccard int64
	minimumScoreMargin  int64
	maximumOptions      int
}

type sourceGroup struct {
	supplier     domain.Supplier
	group        domain.GroupOffer
	snapshot     domain.CatalogSnapshot
	models       []string
	modelSet     map[string]string
	families     []string
	familySet    map[string]string
	snapshotHash string
	observedAt   time.Time
	fresh        bool
	focused      bool
}

type targetGroup struct {
	name      string
	ratio     string
	models    []string
	modelSet  map[string]string
	families  []string
	familySet map[string]string
	channels  []domain.TargetChannel
}

type candidateWork struct {
	candidate    Candidate
	targetSiteID int64
	topScore     int64
	topKey       string
	ready        bool
}

func (s *Service) Preview(ctx context.Context, request PreviewRequest) (Preview, error) {
	config, err := normalizePreviewRequest(request, false)
	if err != nil {
		return Preview{}, err
	}
	return s.preview(ctx, request, config)
}

func (s *Service) preview(ctx context.Context, request PreviewRequest, config normalizedRequest) (Preview, error) {
	if s.store == nil || s.reader == nil {
		return Preview{}, errors.New("mapping draft service is not configured")
	}
	target, err := s.store.GetTarget(ctx, request.TargetSiteID)
	if err != nil {
		return Preview{}, fmt.Errorf("load target site: %w", err)
	}
	if !target.Enabled {
		return Preview{}, errors.New("target site is disabled")
	}
	targetCatalog, channels, err := s.reader.Read(ctx, target)
	if err != nil {
		return Preview{}, fmt.Errorf("read target catalog and channels: %w", err)
	}
	targetGroups := buildTargetGroups(targetCatalog, channels)
	if len(targetGroups) == 0 {
		return Preview{}, errors.New("target catalog and channels contain no groups")
	}
	targetEvidenceHash := hashTargetEvidence(targetCatalog, channels)
	targetCatalogHash := catalogHash(targetCatalog)
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return Preview{}, fmt.Errorf("list existing pricing policies: %w", err)
	}
	owners := policyOwners(policies, target.ID)

	suppliers, err := s.requestedSuppliers(ctx, request.SupplierIDs)
	if err != nil {
		return Preview{}, err
	}
	now := s.now().UTC()
	result := Preview{
		TargetSiteID: target.ID, TargetName: target.Name, TargetEvidenceHash: targetEvidenceHash,
		TargetCatalogHash: targetCatalogHash, TargetObservedAt: targetCatalog.Evidence.ObservedAt,
		Thresholds: Thresholds{MinimumScore: formatBasisPoints(config.minimumScore),
			MinimumModelJaccard: formatBasisPoints(config.minimumModelJaccard), MinimumScoreMargin: formatBasisPoints(config.minimumScoreMargin)},
		Candidates: []Candidate{}, SupplierIssues: []SupplierIssue{}, GeneratedAt: now,
	}
	result.Summary.SuppliersRequested = len(suppliers)
	works := make([]candidateWork, 0)
	for _, supplier := range suppliers {
		snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID)
		if snapshotErr != nil {
			kind := "catalog_unavailable"
			message := "no trusted catalog snapshot is available"
			if !store.IsNotFound(snapshotErr) {
				kind = "catalog_read_failed"
				message = cleanError(snapshotErr)
			}
			result.SupplierIssues = append(result.SupplierIssues, SupplierIssue{SupplierID: supplier.ID, SupplierName: supplier.Name, Kind: kind, Message: message})
			continue
		}
		result.Summary.SuppliersUsable++
		for _, source := range buildSourceGroups(supplier, snapshot, now) {
			work := buildCandidate(source, target.ID, targetEvidenceHash, targetCatalog, targetGroups, owners, config)
			works = append(works, work)
		}
	}
	resolveTargetCompetition(works, config.minimumScoreMargin)
	sort.Slice(works, func(i, j int) bool {
		if works[i].candidate.SupplierName != works[j].candidate.SupplierName {
			return works[i].candidate.SupplierName < works[j].candidate.SupplierName
		}
		return works[i].candidate.SourceGroup < works[j].candidate.SourceGroup
	})
	for index := range works {
		candidate := works[index].candidate
		result.Candidates = append(result.Candidates, candidate)
		switch candidate.Status {
		case "prefilled":
			result.Summary.Prefilled++
		case "already_mapped":
			result.Summary.AlreadyMapped++
		case "conflict":
			result.Summary.Conflicts++
		default:
			result.Summary.NeedsConfirmation++
		}
	}
	result.Summary.SourceGroups = len(result.Candidates)
	return result, nil
}

func (s *Service) Materialize(ctx context.Context, request MaterializeRequest) (MaterializeResult, error) {
	if request.TargetSiteID <= 0 || strings.TrimSpace(request.TargetEvidenceHash) == "" {
		return MaterializeResult{}, errors.New("target_site_id and target_evidence_hash are required")
	}
	if len(request.Selections) == 0 || len(request.Selections) > 500 {
		return MaterializeResult{}, errors.New("selections must contain between 1 and 500 items")
	}
	supplierIDs := make([]int64, 0, len(request.Selections))
	for _, selection := range request.Selections {
		if selection.SupplierID <= 0 || strings.TrimSpace(selection.CandidateID) == "" || strings.TrimSpace(selection.SourceGroup) == "" ||
			strings.TrimSpace(selection.SourceSnapshotHash) == "" || strings.TrimSpace(selection.TargetGroup) == "" {
			return MaterializeResult{}, errors.New("every selection requires candidate_id, supplier_id, source_group, source_snapshot_hash and target_group")
		}
		supplierIDs = append(supplierIDs, selection.SupplierID)
	}
	config, _ := normalizePreviewRequest(PreviewRequest{}, true)
	preview, err := s.preview(ctx, PreviewRequest{TargetSiteID: request.TargetSiteID, SupplierIDs: supplierIDs}, config)
	if err != nil {
		return MaterializeResult{}, err
	}
	if preview.TargetEvidenceHash != strings.TrimSpace(request.TargetEvidenceHash) {
		return MaterializeResult{}, &ConflictError{Message: "target catalog or channels changed since preview; generate a new mapping draft preview"}
	}
	candidates := make(map[string]Candidate, len(preview.Candidates))
	for _, candidate := range preview.Candidates {
		candidates[candidate.ID] = candidate
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return MaterializeResult{}, err
	}
	response := MaterializeResult{TargetSiteID: request.TargetSiteID, TargetEvidenceHash: preview.TargetEvidenceHash,
		Policies: make([]MaterializedPolicyResult, len(request.Selections))}
	pending := make([]domain.PricingPolicy, 0, len(request.Selections))
	pendingIndexes := make([]int, 0, len(request.Selections))
	selectedTargets := make(map[string]int, len(request.Selections))
	for index, selection := range request.Selections {
		candidate, exists := candidates[selection.CandidateID]
		if !exists || candidate.SupplierID != selection.SupplierID || !strings.EqualFold(candidate.SourceGroup, strings.TrimSpace(selection.SourceGroup)) {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("selection %d no longer matches the current candidate", index)}
		}
		if candidate.SourceSnapshotHash != strings.TrimSpace(selection.SourceSnapshotHash) {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("source catalog for selection %d changed since preview", index)}
		}
		targetKey := ownerKey(request.TargetSiteID, selection.TargetGroup)
		if previous, duplicate := selectedTargets[targetKey]; duplicate {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("selections %d and %d both claim target group %q", previous, index, selection.TargetGroup)}
		}
		selectedTargets[targetKey] = index

		if len(candidate.ExistingPolicyIDs) == 1 {
			existing, loadErr := s.store.GetPolicy(ctx, candidate.ExistingPolicyIDs[0])
			if loadErr != nil {
				return MaterializeResult{}, &ConflictError{Message: "an existing mapping owner changed; refresh the preview"}
			}
			if strings.EqualFold(strings.TrimSpace(existing.TargetGroup), strings.TrimSpace(selection.TargetGroup)) &&
				existing.SupplierID == candidate.SupplierID && strings.EqualFold(existing.SourceGroup, candidate.SourceGroup) {
				response.Policies[index] = MaterializedPolicyResult{CandidateID: candidate.ID, Created: false, Idempotent: true, Policy: existing}
				continue
			}
		}
		if candidate.Status == "conflict" {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("candidate %s has conflicting existing owners", candidate.ID)}
		}
		option, found := findOption(candidate.Options, selection.TargetGroup)
		if !found {
			return MaterializeResult{}, errors.New("selected target group is not a current match option; preview again with a wider option limit")
		}
		if option.ExistingOwner != nil {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("target group %q is already owned by policy %d", option.TargetGroup, option.ExistingOwner.PolicyID)}
		}
		if candidate.ProposedTargetGroup == "" || !strings.EqualFold(candidate.ProposedTargetGroup, option.TargetGroup) {
			if !selection.ConfirmLowConfidence {
				return MaterializeResult{}, errors.New("a non-prefilled mapping requires confirm_low_confidence=true")
			}
		}
		policy, policyErr := policyFromSelection(candidate, option, request.TargetSiteID)
		if policyErr != nil {
			return MaterializeResult{}, policyErr
		}
		if owner := findPolicyOwner(policies, request.TargetSiteID, option.TargetGroup); owner != nil && !sameOwner(*owner, policy) {
			return MaterializeResult{}, &ConflictError{Message: fmt.Sprintf("target group %q is already owned by policy %d", option.TargetGroup, owner.ID)}
		}
		pending = append(pending, policy)
		pendingIndexes = append(pendingIndexes, index)
	}
	if len(pending) > 0 {
		created, createErr := s.store.CreateMappingDraftPolicies(ctx, pending)
		if createErr != nil {
			var ownership *store.MappingOwnershipConflictError
			if errors.As(createErr, &ownership) {
				return MaterializeResult{}, &ConflictError{Message: ownership.Error()}
			}
			return MaterializeResult{}, createErr
		}
		for resultIndex, item := range created {
			selectionIndex := pendingIndexes[resultIndex]
			response.Policies[selectionIndex] = MaterializedPolicyResult{CandidateID: request.Selections[selectionIndex].CandidateID,
				Created: item.Created, Idempotent: !item.Created, Policy: item.Policy}
		}
	}
	return response, nil
}

func (s *Service) requestedSuppliers(ctx context.Context, ids []int64) ([]domain.Supplier, error) {
	if len(ids) == 0 {
		items, err := s.store.ListSuppliers(ctx)
		if err != nil {
			return nil, err
		}
		result := make([]domain.Supplier, 0, len(items))
		for _, item := range items {
			if item.Enabled {
				result = append(result, item)
			}
		}
		return result, nil
	}
	if len(ids) > 1000 {
		return nil, errors.New("supplier_ids exceeds the safety limit")
	}
	unique := make(map[int64]struct{}, len(ids))
	result := make([]domain.Supplier, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("supplier_ids must contain only positive IDs")
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		item, err := s.store.GetSupplier(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load supplier %d: %w", id, err)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func normalizePreviewRequest(request PreviewRequest, materialize bool) (normalizedRequest, error) {
	minimumScore, err := parseThreshold(request.MinimumScore, DefaultMinimumScore, DefaultMinimumScore)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("minimum_score: %w", err)
	}
	minimumModels, err := parseThreshold(request.MinimumModelJaccard, DefaultMinimumModelJaccard, DefaultMinimumModelJaccard)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("minimum_model_jaccard: %w", err)
	}
	minimumMargin, err := parseThreshold(request.MinimumScoreMargin, DefaultMinimumScoreMargin, DefaultMinimumScoreMargin)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("minimum_score_margin: %w", err)
	}
	maximumOptions := request.MaximumOptions
	if materialize {
		maximumOptions = 1000
	} else if maximumOptions == 0 {
		maximumOptions = DefaultMaximumOptions
	}
	if maximumOptions < 1 || maximumOptions > 1000 {
		return normalizedRequest{}, errors.New("maximum_options must be between 1 and 1000")
	}
	return normalizedRequest{minimumScore: minimumScore, minimumModelJaccard: minimumModels,
		minimumScoreMargin: minimumMargin, maximumOptions: maximumOptions}, nil
}

func parseThreshold(raw, fallback, floor string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		raw = fallback
	}
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || value.IsNegative() || value.GreaterThan(decimal.NewFromInt(1)) {
		return 0, errors.New("must be a decimal between 0 and 1")
	}
	minimum, _ := decimal.NewFromString(floor)
	if value.LessThan(minimum) {
		return 0, fmt.Errorf("cannot be lower than the production floor %s", floor)
	}
	return value.Mul(decimal.NewFromInt(10000)).Round(0).IntPart(), nil
}

func buildSourceGroups(supplier domain.Supplier, snapshot domain.CatalogSnapshot, now time.Time) []sourceGroup {
	groups := append([]domain.GroupOffer(nil), snapshot.Groups...)
	sort.Slice(groups, func(i, j int) bool { return groupID(groups[i]) < groupID(groups[j]) })
	result := make([]sourceGroup, 0, len(groups))
	focus := stringSet(supplier.FocusGroups)
	snapshotHash := catalogHash(snapshot)
	maxAge := 3 * time.Duration(maxInt(supplier.PollIntervalSeconds, 300)) * time.Second
	if maxAge < 15*time.Minute {
		maxAge = 15 * time.Minute
	}
	fresh := !snapshot.Evidence.ObservedAt.IsZero() && !snapshot.Evidence.ObservedAt.After(now.Add(5*time.Minute)) && now.Sub(snapshot.Evidence.ObservedAt) <= maxAge
	for _, group := range groups {
		id := groupID(group)
		if id == "" {
			continue
		}
		models := sortedUnique(group.ModelIDs)
		families := modelFamilies(models)
		result = append(result, sourceGroup{supplier: supplier, group: group, snapshot: snapshot, models: models,
			modelSet: stringSet(models), families: families, familySet: stringSet(families),
			snapshotHash: snapshotHash, observedAt: snapshot.Evidence.ObservedAt, fresh: fresh,
			focused: len(focus) == 0 || containsFold(focus, id) || containsFold(focus, group.Name)})
	}
	return result
}

func buildTargetGroups(catalog domain.CatalogSnapshot, channels []domain.TargetChannel) []targetGroup {
	groups := make(map[string]*targetGroup)
	for _, item := range catalog.Groups {
		name := groupID(item)
		if name == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		groups[key] = &targetGroup{name: name, ratio: strings.TrimSpace(item.Ratio), models: sortedUnique(item.ModelIDs)}
	}
	for _, channel := range channels {
		for _, name := range splitGroups(channel.Group) {
			key := strings.ToLower(name)
			group := groups[key]
			if group == nil {
				group = &targetGroup{name: name}
				groups[key] = group
			}
			group.models = sortedUnique(append(group.models, channel.Models...))
			group.channels = append(group.channels, channel)
		}
	}
	result := make([]targetGroup, 0, len(groups))
	for _, item := range groups {
		sort.Slice(item.channels, func(i, j int) bool { return item.channels[i].ID < item.channels[j].ID })
		item.modelSet = stringSet(item.models)
		item.families = modelFamilies(item.models)
		item.familySet = stringSet(item.families)
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].name) < strings.ToLower(result[j].name) })
	return result
}

func buildCandidate(source sourceGroup, targetID int64, targetEvidenceHash string, targetCatalog domain.CatalogSnapshot, targetGroups []targetGroup,
	owners map[string][]domain.PricingPolicy, config normalizedRequest) candidateWork {
	rawRatio := strings.TrimSpace(source.group.RawRatio)
	if rawRatio == "" {
		rawRatio = strings.TrimSpace(source.group.Ratio)
	}
	factor := strings.TrimSpace(source.group.RechargeValueFactor)
	if factor == "" {
		factor = ratios.DefaultRechargeValueFactor
	}
	candidate := Candidate{
		SupplierID: source.supplier.ID, SupplierName: source.supplier.Name, SourceGroup: groupID(source.group),
		SourceGroupName: defaultString(source.group.Name, groupID(source.group)), SourceRatio: ratios.DecisionRatio(source.group),
		RawSourceRatio: rawRatio, RechargeValueFactor: factor,
		SourceModels: len(source.models), SourceSnapshotHash: source.snapshotHash, SourceObservedAt: source.observedAt,
		SourceFresh: source.fresh, Focused: source.focused, Status: "needs_confirmation", ConfidenceLevel: "low",
		Options: []MatchOption{}, Reasons: []string{}, TargetEvidenceHash: targetEvidenceHash,
	}
	candidate.ID = candidateID(targetID, candidate.SupplierID, candidate.SourceGroup, candidate.SourceSnapshotHash, candidate.SourceRatio, targetEvidenceHash)
	for _, target := range targetGroups {
		option := scoreOption(source, targetCatalog, target, owners[ownerKey(targetID, target.name)], config)
		candidate.Options = append(candidate.Options, option)
	}
	sort.Slice(candidate.Options, func(i, j int) bool {
		left, right := basisPoints(candidate.Options[i].Score), basisPoints(candidate.Options[j].Score)
		if left != right {
			return left > right
		}
		return strings.ToLower(candidate.Options[i].TargetGroup) < strings.ToLower(candidate.Options[j].TargetGroup)
	})
	selectedOption := 0
	setCandidateScoreSummary(&candidate, selectedOption)

	existing := existingMappingsForSource(owners, source.supplier.ID, candidate.SourceGroup)
	for _, policy := range existing {
		candidate.ExistingPolicyIDs = append(candidate.ExistingPolicyIDs, policy.ID)
	}
	if len(existing) == 1 {
		candidate.Status = "already_mapped"
		candidate.ConfidenceLevel = "explicit"
		candidate.Reasons = append(candidate.Reasons, "existing_mapping_is_authoritative")
		for index := range candidate.Options {
			if strings.EqualFold(candidate.Options[index].TargetGroup, existing[0].TargetGroup) {
				candidate.ProposedTargetGroup = candidate.Options[index].TargetGroup
				if index > 0 {
					selected := candidate.Options[index]
					copy(candidate.Options[1:index+1], candidate.Options[0:index])
					candidate.Options[0] = selected
				}
				selectedOption = 0
				setCandidateScoreSummary(&candidate, selectedOption)
				break
			}
		}
		trimOptions(&candidate, config.maximumOptions)
		return candidateWork{candidate: candidate, targetSiteID: targetID}
	}
	if len(existing) > 1 {
		candidate.Status = "conflict"
		candidate.Reasons = append(candidate.Reasons, "source_group_has_multiple_existing_mappings")
		trimOptions(&candidate, config.maximumOptions)
		return candidateWork{candidate: candidate, targetSiteID: targetID}
	}
	if len(candidate.Options) == 0 {
		candidate.Reasons = append(candidate.Reasons, "no_target_group_options")
		return candidateWork{candidate: candidate, targetSiteID: targetID}
	}
	topScore := basisPoints(candidate.Options[0].Score)
	margin := basisPoints(candidate.SourceScoreMargin)
	if evidenceHigh(candidate.Options[0], source, config) {
		candidate.ConfidenceLevel = "high"
	} else if topScore >= config.minimumScore || basisPoints(candidate.Options[0].ModelJaccard) >= config.minimumModelJaccard {
		candidate.ConfidenceLevel = "medium"
	}
	ready := candidate.Options[0].EligibleForPrefill
	if margin < config.minimumScoreMargin {
		candidate.Options[0].EligibleForPrefill = false
		candidate.Options[0].IneligibleReasons = appendUnique(candidate.Options[0].IneligibleReasons, "source_match_is_ambiguous")
		candidate.Reasons = append(candidate.Reasons, "source_match_is_ambiguous")
		ready = false
	}
	for index := 1; index < len(candidate.Options); index++ {
		if candidate.Options[index].EligibleForPrefill {
			candidate.Options[index].EligibleForPrefill = false
			candidate.Options[index].IneligibleReasons = appendUnique(candidate.Options[index].IneligibleReasons, "not_best_match_for_source")
		}
	}
	if !ready {
		candidate.Reasons = appendUnique(candidate.Reasons, candidate.Options[0].IneligibleReasons...)
	}
	topKey := ownerKey(targetID, candidate.Options[0].TargetGroup)
	trimOptions(&candidate, config.maximumOptions)
	return candidateWork{candidate: candidate, targetSiteID: targetID, topScore: topScore, topKey: topKey, ready: ready}
}

func setCandidateScoreSummary(candidate *Candidate, selectedIndex int) {
	if candidate == nil || selectedIndex < 0 || selectedIndex >= len(candidate.Options) {
		return
	}
	selected := basisPoints(candidate.Options[selectedIndex].Score)
	bestAlternative := int64(0)
	for index := range candidate.Options {
		if index == selectedIndex {
			continue
		}
		if score := basisPoints(candidate.Options[index].Score); score > bestAlternative {
			bestAlternative = score
		}
	}
	candidate.Score = formatBasisPoints(selected)
	candidate.SourceRunnerUpScore = formatBasisPoints(bestAlternative)
	delta := selected - bestAlternative
	candidate.ScoreIsBest = delta >= 0
	if delta < 0 {
		delta = -delta
	}
	candidate.SourceScoreMargin = formatBasisPoints(delta)
}

func resolveTargetCompetition(works []candidateWork, minimumMargin int64) {
	byTarget := make(map[string][]int)
	for index := range works {
		if works[index].ready {
			byTarget[works[index].topKey] = append(byTarget[works[index].topKey], index)
		}
	}
	for _, indexes := range byTarget {
		sort.Slice(indexes, func(i, j int) bool {
			left, right := works[indexes[i]], works[indexes[j]]
			if left.topScore != right.topScore {
				return left.topScore > right.topScore
			}
			return left.candidate.ID < right.candidate.ID
		})
		winnerAllowed := true
		nextScore := int64(0)
		margin := works[indexes[0]].topScore
		if len(indexes) > 1 {
			nextScore = works[indexes[1]].topScore
			margin = works[indexes[0]].topScore - nextScore
			winnerAllowed = margin >= minimumMargin
		}
		for position, index := range indexes {
			work := &works[index]
			work.candidate.TargetCompetition = Competition{Contenders: len(indexes), NextScore: formatBasisPoints(nextScore), Margin: formatBasisPoints(margin)}
			if position == 0 && winnerAllowed {
				option := work.candidate.Options[0]
				policy, err := policyFromSelection(work.candidate, option, work.targetSiteID)
				if err == nil {
					work.candidate.Status = "prefilled"
					work.candidate.ProposedTargetGroup = option.TargetGroup
					work.candidate.Policy = policyDraft(policy)
					continue
				}
				work.candidate.Reasons = appendUnique(work.candidate.Reasons, "policy_draft_could_not_be_derived")
			}
			work.candidate.Options[0].EligibleForPrefill = false
			if !winnerAllowed {
				work.candidate.Reasons = appendUnique(work.candidate.Reasons, "target_group_has_ambiguous_source_competition")
			} else {
				work.candidate.Reasons = appendUnique(work.candidate.Reasons, "target_group_reserved_by_stronger_candidate")
			}
		}
	}
}

func scoreOption(source sourceGroup, targetCatalog domain.CatalogSnapshot, target targetGroup, owners []domain.PricingPolicy, config normalizedRequest) MatchOption {
	sourceNames := []string{source.group.ExternalID, source.group.Name}
	nameBP, exactName := bestNameSimilarity(sourceNames, target.name)
	modelBP, overlap, modelUnionCount := jaccardSetStats(source.modelSet, target.modelSet)
	familyBP, _, _ := jaccardSetStats(source.familySet, target.familySet)
	ratioBP, ratioKnown := ratioSimilarity(ratios.DecisionRatio(source.group), target.ratio)
	matchedChannels := matchingOriginChannels(source.supplier, target.channels)
	originBP := int64(0)
	if len(matchedChannels) > 0 {
		originBP = 10000
	}
	components := []struct {
		key, weight, evidence string
		weightBP, similarity  int64
	}{
		{"origin", "0.2000", fmt.Sprintf("same_origin_channel_count=%d", len(matchedChannels)), 2000, originBP},
		{"name", "0.2500", fmt.Sprintf("normalized_exact=%t", exactName), 2500, nameBP},
		{"models", "0.4000", fmt.Sprintf("intersection=%d,union=%d", len(overlap), modelUnionCount), 4000, modelBP},
		{"families", "0.1000", fmt.Sprintf("source=%d,target=%d", len(source.families), len(target.families)), 1000, familyBP},
		{"ratio", "0.0500", fmt.Sprintf("both_known=%t", ratioKnown), 500, ratioBP},
	}
	option := MatchOption{TargetGroup: target.name, TargetRatio: strings.TrimSpace(target.ratio), ModelJaccard: formatBasisPoints(modelBP),
		FamilyJaccard: formatBasisPoints(familyBP), NameSimilarity: formatBasisPoints(nameBP), OriginMatch: originBP == 10000,
		NameExact: exactName, MatchedChannelIDs: matchedChannels, SourceModelCount: len(source.models), TargetModelCount: len(target.models),
		OverlappingModels: overlap, allOverlappingModels: append([]string(nil), overlap...), IneligibleReasons: []string{}, Factors: []ScoreFactor{},
		EquivalentCostRatio: ratios.DecisionRatio(source.group), PricingBasis: "group_ratio_fallback"}
	if len(overlap) > 0 {
		hasExplicitPricing := catalogHasExplicitModelPricing(source.snapshot, overlap) ||
			catalogHasExplicitModelPricing(targetCatalog, overlap)
		if hasExplicitPricing {
			impact, impactErr := pricing.ResolvePricingImpactForTargetGroup(source.snapshot, targetCatalog, source.group,
				target.name, overlap)
			if impactErr == nil {
				option.EquivalentCostRatio = impact.EquivalentCostRatio
				option.PricingBasis = "model_dimension_equivalent"
				option.PricingCoverageRatio = impact.CoverageRatio
				option.PricingImpactComplete = impact.Complete
				option.PricingImpactConfidence = impact.Confidence
				option.PricingUnknownDimensions = append([]string(nil), impact.UnknownDimensions...)
				option.PricingWarnings = append([]string(nil), impact.Warnings...)
			} else {
				option.EquivalentCostRatio = ""
				option.PricingBasis = "model_dimension_unresolved"
				option.PricingImpactError = cleanError(impactErr)
			}
		}
	}
	if ratioKnown {
		option.RatioSimilarity = formatBasisPoints(ratioBP)
	}
	if len(option.OverlappingModels) > 50 {
		option.OverlappingModelsOmitted = len(option.OverlappingModels) - 50
		option.OverlappingModels = option.OverlappingModels[:50]
	}
	total := int64(0)
	for _, component := range components {
		value := component.weightBP * component.similarity / 10000
		total += value
		option.Factors = append(option.Factors, ScoreFactor{Key: component.key, Weight: component.weight,
			Evidence: component.evidence, Component: formatBasisPoints(value)})
	}
	option.Score = formatBasisPoints(total)
	if len(owners) > 0 {
		owner := owners[0]
		option.ExistingOwner = &PolicyOwner{PolicyID: owner.ID, SupplierID: owner.SupplierID, SourceGroup: owner.SourceGroup,
			Enabled: owner.Enabled, ExecutionMode: owner.ExecutionMode}
	}
	if total < config.minimumScore {
		option.IneligibleReasons = append(option.IneligibleReasons, "score_below_threshold")
	}
	if modelBP < config.minimumModelJaccard {
		option.IneligibleReasons = append(option.IneligibleReasons, "model_overlap_below_threshold")
	}
	if originBP == 0 && !exactName {
		option.IneligibleReasons = append(option.IneligibleReasons, "no_strong_identity_evidence")
	}
	if !ratioKnown {
		option.IneligibleReasons = append(option.IneligibleReasons, "source_or_target_ratio_unknown")
	}
	if !source.fresh {
		option.IneligibleReasons = append(option.IneligibleReasons, "source_snapshot_stale")
	}
	if option.EquivalentCostRatio == "" {
		option.IneligibleReasons = append(option.IneligibleReasons, "equivalent_cost_unresolved")
	} else if option.PricingCoverageRatio != "" && basisPoints(option.PricingCoverageRatio) < 8000 {
		option.IneligibleReasons = append(option.IneligibleReasons, "equivalent_cost_coverage_below_threshold")
	}
	if option.ExistingOwner != nil {
		option.IneligibleReasons = append(option.IneligibleReasons, "target_group_already_owned")
	}
	option.EligibleForPrefill = len(option.IneligibleReasons) == 0
	return option
}

func evidenceHigh(option MatchOption, source sourceGroup, config normalizedRequest) bool {
	return basisPoints(option.Score) >= config.minimumScore && basisPoints(option.ModelJaccard) >= config.minimumModelJaccard &&
		(option.OriginMatch || option.NameExact) && option.RatioSimilarity != "" && source.fresh
}

func policyFromSelection(candidate Candidate, option MatchOption, targetSiteID int64) (domain.PricingPolicy, error) {
	if targetSiteID <= 0 {
		return domain.PricingPolicy{}, errors.New("target site ID is missing")
	}
	sourceRatio, err := decimal.NewFromString(strings.TrimSpace(candidate.SourceRatio))
	if err != nil || sourceRatio.IsNegative() {
		return domain.PricingPolicy{}, errors.New("upstream real group ratio is unknown or invalid")
	}
	target, err := decimal.NewFromString(strings.TrimSpace(option.TargetRatio))
	if err != nil || target.IsNegative() {
		return domain.PricingPolicy{}, errors.New("target ratio is unknown or invalid")
	}
	name := fmt.Sprintf("Draft: %s / %s -> %s", candidate.SupplierName, candidate.SourceGroup, option.TargetGroup)
	if len(name) > 200 {
		name = name[:200]
	}
	return domain.PricingPolicy{Name: name, SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup,
		SourceModelPattern: "*", SourceBasis: "group_ratio",
		TargetSiteID: targetSiteID, TargetGroup: option.TargetGroup,
		RuleType: "add", Value: target.Sub(sourceRatio).String(), ProfitBasis: "contribution", RevenueCashFactor: "1",
		CostCashFactor: "1", RiskBufferRate: "0", AllowNegativeProfit: false, SubsidyBudgetDaily: "0",
		MinimumRatio: "0", MaximumRatio: "0", RoundingStep: "0.001", Enabled: false, ExecutionMode: "approval",
		CoverageMode: "threshold", MinimumCoverage: "0.8"}, nil
}

func catalogHasExplicitModelPricing(snapshot domain.CatalogSnapshot, models []string) bool {
	wanted := stringSet(models)
	for _, model := range snapshot.Models {
		if !containsFold(wanted, model.ExternalID) && !containsFold(wanted, model.Name) {
			continue
		}
		if !reflect.DeepEqual(model.PriceFunction, domain.PriceFunction{}) || strings.TrimSpace(model.ModelPrice) != "" ||
			strings.TrimSpace(model.ModelRatio) != "" || strings.TrimSpace(model.BillingExpr) != "" {
			return true
		}
	}
	return false
}

func policyDraft(policy domain.PricingPolicy) *PolicyDraft {
	return &PolicyDraft{Name: policy.Name, SupplierID: policy.SupplierID, SourceGroup: policy.SourceGroup,
		SourceModelPattern: policy.SourceModelPattern, TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup,
		RuleType: policy.RuleType, Value: policy.Value, ExecutionMode: policy.ExecutionMode, Enabled: policy.Enabled}
}

func findOption(options []MatchOption, group string) (MatchOption, bool) {
	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option.TargetGroup), strings.TrimSpace(group)) {
			return option, true
		}
	}
	return MatchOption{}, false
}

func trimOptions(candidate *Candidate, maximum int) {
	if maximum > 0 && len(candidate.Options) > maximum {
		candidate.Options = candidate.Options[:maximum]
	}
}

func policyOwners(policies []domain.PricingPolicy, targetSiteID int64) map[string][]domain.PricingPolicy {
	result := make(map[string][]domain.PricingPolicy)
	for _, policy := range policies {
		if policy.TargetSiteID == targetSiteID {
			key := ownerKey(targetSiteID, policy.TargetGroup)
			result[key] = append(result[key], policy)
		}
	}
	return result
}

func existingMappingsForSource(owners map[string][]domain.PricingPolicy, supplierID int64, sourceGroup string) []domain.PricingPolicy {
	result := make([]domain.PricingPolicy, 0)
	for _, policies := range owners {
		for _, policy := range policies {
			if policy.SupplierID == supplierID && strings.EqualFold(strings.TrimSpace(policy.SourceGroup), strings.TrimSpace(sourceGroup)) {
				result = append(result, policy)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func findPolicyOwner(policies []domain.PricingPolicy, targetSiteID int64, targetGroup string) *domain.PricingPolicy {
	for index := range policies {
		if policies[index].TargetSiteID == targetSiteID && strings.EqualFold(strings.TrimSpace(policies[index].TargetGroup), strings.TrimSpace(targetGroup)) {
			return &policies[index]
		}
	}
	return nil
}

func sameOwner(existing, desired domain.PricingPolicy) bool {
	return existing.TargetSiteID == desired.TargetSiteID && existing.SupplierID == desired.SupplierID &&
		strings.EqualFold(strings.TrimSpace(existing.TargetGroup), strings.TrimSpace(desired.TargetGroup)) &&
		strings.EqualFold(strings.TrimSpace(existing.SourceGroup), strings.TrimSpace(desired.SourceGroup))
}

func ownerKey(targetSiteID int64, targetGroup string) string {
	return fmt.Sprintf("%d\x00%s", targetSiteID, strings.ToLower(strings.TrimSpace(targetGroup)))
}

func candidateID(targetSiteID, supplierID int64, sourceGroup, sourceHash, effectiveRatio, targetHash string) string {
	raw := fmt.Sprintf("%d\n%d\n%s\n%s\n%s\n%s", targetSiteID, supplierID, strings.ToLower(strings.TrimSpace(sourceGroup)), sourceHash,
		strings.TrimSpace(effectiveRatio), targetHash)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func hashTargetEvidence(catalog domain.CatalogSnapshot, channels []domain.TargetChannel) string {
	type channelEvidence struct {
		ID      int64    `json:"id"`
		Name    string   `json:"name"`
		Status  int      `json:"status"`
		BaseURL string   `json:"base_url"`
		Group   string   `json:"group"`
		Models  []string `json:"models"`
		Tag     string   `json:"tag"`
		Remark  string   `json:"remark"`
		Hash    string   `json:"hash"`
	}
	type groupEvidence struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Ratio  string   `json:"ratio"`
		Models []string `json:"models"`
	}
	groups := make([]groupEvidence, 0, len(catalog.Groups))
	for _, item := range catalog.Groups {
		groups = append(groups, groupEvidence{ID: item.ExternalID, Name: item.Name, Ratio: item.Ratio, Models: sortedUnique(item.ModelIDs)})
	}
	sort.Slice(groups, func(i, j int) bool {
		left := strings.ToLower(defaultString(groups[i].ID, groups[i].Name))
		right := strings.ToLower(defaultString(groups[j].ID, groups[j].Name))
		return left < right
	})
	channelRows := make([]channelEvidence, 0, len(channels))
	for _, item := range channels {
		channelRows = append(channelRows, channelEvidence{ID: item.ID, Name: item.Name, Status: item.Status,
			BaseURL: strings.TrimRight(item.BaseURL, "/"), Group: item.Group, Models: sortedUnique(item.Models),
			Tag: item.Tag, Remark: item.Remark, Hash: item.ConfigHash})
	}
	sort.Slice(channelRows, func(i, j int) bool { return channelRows[i].ID < channelRows[j].ID })
	encoded, _ := json.Marshal(struct {
		CatalogHash string            `json:"catalog_hash"`
		Groups      []groupEvidence   `json:"groups"`
		Channels    []channelEvidence `json:"channels"`
	}{CatalogHash: catalogHash(catalog), Groups: groups, Channels: channelRows})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func catalogHash(snapshot domain.CatalogSnapshot) string {
	if value := strings.TrimSpace(snapshot.Evidence.ResponseHash); value != "" {
		return value
	}
	groups := append([]domain.GroupOffer(nil), snapshot.Groups...)
	models := append([]domain.ModelOffer(nil), snapshot.Models...)
	for index := range groups {
		groups[index].ModelIDs = sortedUnique(groups[index].ModelIDs)
	}
	sort.Slice(groups, func(i, j int) bool { return groupID(groups[i]) < groupID(groups[j]) })
	sort.Slice(models, func(i, j int) bool {
		left := strings.Join([]string{models[i].ExternalID, models[i].Group, models[i].Name}, "\x00")
		right := strings.Join([]string{models[j].ExternalID, models[j].Group, models[j].Name}, "\x00")
		return left < right
	})
	encoded, _ := json.Marshal(struct {
		Groups []domain.GroupOffer `json:"groups"`
		Models []domain.ModelOffer `json:"models"`
	}{Groups: groups, Models: models})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func matchingOriginChannels(supplier domain.Supplier, channels []domain.TargetChannel) []int64 {
	base := supplier.BaseURL
	if configured := strings.TrimSpace(supplier.Metadata["api_base_url"]); configured != "" {
		base = configured
	}
	want := origin(base)
	if want == "" {
		return nil
	}
	result := make([]int64, 0)
	for _, channel := range channels {
		if origin(channel.BaseURL) == want {
			result = append(result, channel.ID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func origin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return ""
	}
	port := parsed.Port()
	if (strings.EqualFold(parsed.Scheme, "https") && port == "443") || (strings.EqualFold(parsed.Scheme, "http") && port == "80") {
		port = ""
	}
	host := strings.ToLower(parsed.Hostname())
	if port != "" {
		host += ":" + port
	}
	return strings.ToLower(parsed.Scheme) + "://" + host
}

func splitGroups(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return sortedUnique(result)
}

func groupID(group domain.GroupOffer) string {
	return defaultString(strings.TrimSpace(group.ExternalID), strings.TrimSpace(group.Name))
}

func literalGlob(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	for _, marker := range []string{"*", "?", "["} {
		value = strings.ReplaceAll(value, marker, `\`+marker)
	}
	return value
}

func cleanError(err error) string {
	value := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(err.Error()), "\r", " "), "\n", " ")
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
