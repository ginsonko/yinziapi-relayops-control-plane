package mappingdraft

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type ManualPreviewRequest struct {
	TargetSiteID int64   `json:"target_site_id"`
	SupplierIDs  []int64 `json:"supplier_ids,omitempty"`
}

type ManualPreview struct {
	TargetSiteID       int64                `json:"target_site_id"`
	TargetName         string               `json:"target_name"`
	TargetEvidenceHash string               `json:"target_evidence_hash"`
	TargetCatalogHash  string               `json:"target_catalog_hash"`
	TargetObservedAt   time.Time            `json:"target_observed_at"`
	Summary            ManualPreviewSummary `json:"summary"`
	Groups             []ManualTargetGroup  `json:"groups"`
	SupplierIssues     []SupplierIssue      `json:"supplier_issues"`
	GeneratedAt        time.Time            `json:"generated_at"`
}

type ManualPreviewSummary struct {
	TargetGroups       int `json:"target_groups"`
	TargetChannels     int `json:"target_channels"`
	Mapped             int `json:"mapped"`
	Unmapped           int `json:"unmapped"`
	Conflicts          int `json:"conflicts"`
	SuppliersRequested int `json:"suppliers_requested"`
	SuppliersUsable    int `json:"suppliers_usable"`
}

type ManualTargetGroup struct {
	TargetGroup       string                `json:"target_group"`
	TargetRatio       string                `json:"target_ratio,omitempty"`
	Models            []string              `json:"models"`
	Channels          []ManualTargetChannel `json:"channels"`
	Status            string                `json:"status"`
	ExistingPolicy    *domain.PricingPolicy `json:"existing_policy,omitempty"`
	ConflictPolicyIDs []int64               `json:"conflict_policy_ids,omitempty"`
	RecommendedOption string                `json:"recommended_option_id,omitempty"`
	Options           []ManualSourceOption  `json:"options"`
}

type ManualTargetChannel struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Status     int      `json:"status"`
	BaseOrigin string   `json:"base_origin,omitempty"`
	Models     []string `json:"models"`
	Group      string   `json:"group"`
	Tag        string   `json:"tag,omitempty"`
	Remark     string   `json:"remark,omitempty"`
	ConfigHash string   `json:"config_hash"`
}

type ManualSourceOption struct {
	CandidateID          string    `json:"candidate_id"`
	SupplierID           int64     `json:"supplier_id"`
	SupplierName         string    `json:"supplier_name"`
	SourceGroup          string    `json:"source_group"`
	SourceGroupName      string    `json:"source_group_name"`
	SourceRatio          string    `json:"source_ratio,omitempty"`
	RawSourceRatio       string    `json:"raw_source_ratio,omitempty"`
	RechargeValueFactor  string    `json:"recharge_value_factor,omitempty"`
	SourceModels         int       `json:"source_models"`
	SourceSnapshotHash   string    `json:"source_snapshot_hash"`
	SourceObservedAt     time.Time `json:"source_observed_at"`
	SourceFresh          bool      `json:"source_fresh"`
	Score                string    `json:"score"`
	ModelJaccard         string    `json:"model_jaccard"`
	FamilyJaccard        string    `json:"family_jaccard"`
	NameSimilarity       string    `json:"name_similarity"`
	RatioSimilarity      string    `json:"ratio_similarity,omitempty"`
	OriginMatch          bool      `json:"origin_match"`
	MatchedChannelIDs    []int64   `json:"matched_channel_ids,omitempty"`
	OverlappingModels    []string  `json:"overlapping_models"`
	EquivalentCostRatio  string    `json:"equivalent_cost_ratio,omitempty"`
	PricingBasis         string    `json:"pricing_basis,omitempty"`
	PricingCoverageRatio string    `json:"pricing_coverage_ratio,omitempty"`
	SourceModelPattern   string    `json:"source_model_pattern"`
	RecommendedRuleType  string    `json:"recommended_rule_type,omitempty"`
	RecommendedValue     string    `json:"recommended_value,omitempty"`
	ExistingPolicyIDs    []int64   `json:"existing_policy_ids,omitempty"`
	Reasons              []string  `json:"reasons"`
}

type ManualRuleInput struct {
	Name                string `json:"name,omitempty"`
	SourceBasis         string `json:"source_basis,omitempty"`
	RuleType            string `json:"rule_type"`
	Value               string `json:"value"`
	ProfitBasis         string `json:"profit_basis,omitempty"`
	RevenueCashFactor   string `json:"revenue_cash_factor,omitempty"`
	CostCashFactor      string `json:"cost_cash_factor,omitempty"`
	RiskBufferRate      string `json:"risk_buffer_rate,omitempty"`
	AllowNegativeProfit bool   `json:"allow_negative_profit,omitempty"`
	SubsidyBudgetDaily  string `json:"subsidy_budget_daily,omitempty"`
	MinimumRatio        string `json:"minimum_ratio,omitempty"`
	MaximumRatio        string `json:"maximum_ratio,omitempty"`
	RoundingStep        string `json:"rounding_step,omitempty"`
	OnlyFollowIncrease  bool   `json:"only_follow_increase,omitempty"`
	OnlyFollowDecrease  bool   `json:"only_follow_decrease,omitempty"`
	Enabled             bool   `json:"enabled"`
	ExecutionMode       string `json:"execution_mode"`
	CoverageMode        string `json:"coverage_mode,omitempty"`
	MinimumCoverage     string `json:"minimum_coverage_ratio,omitempty"`
}

type ManualMaterializeRequest struct {
	TargetSiteID       int64                        `json:"target_site_id"`
	TargetEvidenceHash string                       `json:"target_evidence_hash"`
	Selections         []ManualMaterializeSelection `json:"selections"`
}

type ManualMaterializeSelection struct {
	CandidateID             string          `json:"candidate_id"`
	SupplierID              int64           `json:"supplier_id"`
	SourceGroup             string          `json:"source_group"`
	SourceSnapshotHash      string          `json:"source_snapshot_hash"`
	TargetGroup             string          `json:"target_group"`
	ExpectedPolicyID        int64           `json:"expected_policy_id,omitempty"`
	ExpectedPolicyUpdatedAt *time.Time      `json:"expected_policy_updated_at,omitempty"`
	ConfirmManual           bool            `json:"confirm_manual"`
	Rule                    ManualRuleInput `json:"rule"`
}

type ManualMaterializeResult struct {
	TargetSiteID       int64                            `json:"target_site_id"`
	TargetEvidenceHash string                           `json:"target_evidence_hash"`
	Policies           []ManualMaterializedPolicyResult `json:"policies"`
}

type ManualMaterializedPolicyResult struct {
	TargetGroup string               `json:"target_group"`
	Created     bool                 `json:"created"`
	Updated     bool                 `json:"updated"`
	Idempotent  bool                 `json:"idempotent"`
	Policy      domain.PricingPolicy `json:"policy"`
}

func (s *Service) ManualPreview(ctx context.Context, request ManualPreviewRequest) (ManualPreview, error) {
	if request.TargetSiteID <= 0 {
		return ManualPreview{}, errors.New("target_site_id is required")
	}
	if s.store == nil || s.reader == nil {
		return ManualPreview{}, errors.New("mapping draft service is not configured")
	}
	target, err := s.store.GetTarget(ctx, request.TargetSiteID)
	if err != nil {
		return ManualPreview{}, fmt.Errorf("load target site: %w", err)
	}
	if !target.Enabled {
		return ManualPreview{}, errors.New("target site is disabled")
	}
	targetCatalog, channels, err := s.reader.Read(ctx, target)
	if err != nil {
		return ManualPreview{}, fmt.Errorf("read target catalog and channels: %w", err)
	}
	targetGroups := buildTargetGroups(targetCatalog, channels)
	if len(targetGroups) == 0 {
		return ManualPreview{}, errors.New("target catalog and channels contain no groups")
	}
	targetEvidenceHash := hashTargetEvidence(targetCatalog, channels)
	targetCatalogHash := catalogHash(targetCatalog)
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return ManualPreview{}, fmt.Errorf("list existing pricing policies: %w", err)
	}
	owners := policyOwners(policies, target.ID)
	suppliers, err := s.requestedSuppliers(ctx, request.SupplierIDs)
	if err != nil {
		return ManualPreview{}, err
	}
	config, _ := normalizePreviewRequest(PreviewRequest{MaximumOptions: 1000}, true)
	now := s.now().UTC()
	result := ManualPreview{TargetSiteID: target.ID, TargetName: target.Name,
		TargetEvidenceHash: targetEvidenceHash, TargetCatalogHash: targetCatalogHash,
		TargetObservedAt: targetCatalog.Evidence.ObservedAt, Groups: make([]ManualTargetGroup, 0, len(targetGroups)),
		SupplierIssues: make([]SupplierIssue, 0), GeneratedAt: now}
	result.Summary.SuppliersRequested = len(suppliers)
	groupIndexes := make(map[string]int, len(targetGroups))
	for _, targetGroup := range targetGroups {
		group := ManualTargetGroup{TargetGroup: targetGroup.name, TargetRatio: strings.TrimSpace(targetGroup.ratio),
			Models: sortedUnique(targetGroup.models), Channels: make([]ManualTargetChannel, 0, len(targetGroup.channels)),
			Status: "unmapped", Options: make([]ManualSourceOption, 0)}
		for _, channel := range targetGroup.channels {
			group.Channels = append(group.Channels, ManualTargetChannel{ID: channel.ID, Name: channel.Name, Status: channel.Status,
				BaseOrigin: origin(channel.BaseURL), Models: sortedUnique(channel.Models), Group: channel.Group,
				Tag: channel.Tag, Remark: channel.Remark, ConfigHash: channel.ConfigHash})
		}
		ownerRows := owners[ownerKey(target.ID, targetGroup.name)]
		if len(ownerRows) == 1 {
			owner := ownerRows[0]
			group.ExistingPolicy = &owner
			group.Status = "mapped"
			result.Summary.Mapped++
		} else if len(ownerRows) > 1 {
			group.Status = "conflict"
			for _, owner := range ownerRows {
				group.ConflictPolicyIDs = append(group.ConflictPolicyIDs, owner.ID)
			}
			sort.Slice(group.ConflictPolicyIDs, func(i, j int) bool { return group.ConflictPolicyIDs[i] < group.ConflictPolicyIDs[j] })
			result.Summary.Conflicts++
		} else {
			result.Summary.Unmapped++
		}
		result.Summary.TargetChannels += len(group.Channels)
		groupIndexes[strings.ToLower(strings.TrimSpace(group.TargetGroup))] = len(result.Groups)
		result.Groups = append(result.Groups, group)
	}
	result.Summary.TargetGroups = len(result.Groups)

	for _, supplier := range suppliers {
		snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID)
		if snapshotErr != nil {
			kind := "catalog_unavailable"
			message := "no trusted catalog snapshot is available"
			if !store.IsNotFound(snapshotErr) {
				kind, message = "catalog_read_failed", cleanError(snapshotErr)
			}
			result.SupplierIssues = append(result.SupplierIssues, SupplierIssue{SupplierID: supplier.ID, SupplierName: supplier.Name, Kind: kind, Message: message})
			continue
		}
		result.Summary.SuppliersUsable++
		for _, source := range buildSourceGroups(supplier, snapshot, now) {
			candidate := buildCandidate(source, target.ID, targetEvidenceHash, targetCatalog, targetGroups, owners, config).candidate
			for _, option := range candidate.Options {
				index, exists := groupIndexes[strings.ToLower(strings.TrimSpace(option.TargetGroup))]
				if !exists {
					continue
				}
				group := &result.Groups[index]
				group.Options = append(group.Options, manualSourceOption(candidate, option, group.TargetRatio))
			}
		}
	}

	for index := range result.Groups {
		group := &result.Groups[index]
		sort.SliceStable(group.Options, func(i, j int) bool {
			left, right := group.Options[i], group.Options[j]
			if left.OriginMatch != right.OriginMatch {
				return left.OriginMatch
			}
			if basisPoints(left.Score) != basisPoints(right.Score) {
				return basisPoints(left.Score) > basisPoints(right.Score)
			}
			if left.SupplierName != right.SupplierName {
				return strings.ToLower(left.SupplierName) < strings.ToLower(right.SupplierName)
			}
			return strings.ToLower(left.SourceGroup) < strings.ToLower(right.SourceGroup)
		})
		if len(group.Options) > 0 {
			group.RecommendedOption = group.Options[0].CandidateID
			if group.ExistingPolicy != nil {
				for _, option := range group.Options {
					if option.SupplierID == group.ExistingPolicy.SupplierID && strings.EqualFold(option.SourceGroup, group.ExistingPolicy.SourceGroup) {
						group.RecommendedOption = option.CandidateID
						break
					}
				}
			}
		}
	}
	return result, nil
}

func manualSourceOption(candidate Candidate, option MatchOption, targetRatio string) ManualSourceOption {
	models := option.allOverlappingModels
	if len(models) == 0 {
		models = option.OverlappingModels
	}
	patterns := make([]string, 0, len(models))
	for _, model := range models {
		if !strings.Contains(model, ",") {
			patterns = append(patterns, literalGlob(strings.ToLower(model)))
		}
	}
	pattern := "*"
	if len(patterns) > 0 {
		pattern = strings.Join(sortedUnique(patterns), ",")
	}
	cost := strings.TrimSpace(candidate.SourceRatio)
	recommended := ""
	if source, sourceErr := decimal.NewFromString(cost); sourceErr == nil && !source.IsNegative() {
		if target, targetErr := decimal.NewFromString(strings.TrimSpace(targetRatio)); targetErr == nil && !target.IsNegative() {
			recommended = target.Sub(source).String()
		}
	}
	reasons := append([]string(nil), candidate.Reasons...)
	reasons = appendUnique(reasons, option.IneligibleReasons...)
	return ManualSourceOption{CandidateID: candidate.ID, SupplierID: candidate.SupplierID, SupplierName: candidate.SupplierName,
		SourceGroup: candidate.SourceGroup, SourceGroupName: candidate.SourceGroupName, SourceRatio: candidate.SourceRatio,
		RawSourceRatio: candidate.RawSourceRatio, RechargeValueFactor: candidate.RechargeValueFactor,
		SourceModels: candidate.SourceModels, SourceSnapshotHash: candidate.SourceSnapshotHash,
		SourceObservedAt: candidate.SourceObservedAt, SourceFresh: candidate.SourceFresh, Score: option.Score,
		ModelJaccard: option.ModelJaccard, FamilyJaccard: option.FamilyJaccard, NameSimilarity: option.NameSimilarity,
		RatioSimilarity: option.RatioSimilarity, OriginMatch: option.OriginMatch,
		MatchedChannelIDs: append([]int64(nil), option.MatchedChannelIDs...), OverlappingModels: append([]string(nil), option.OverlappingModels...),
		EquivalentCostRatio: option.EquivalentCostRatio, PricingBasis: option.PricingBasis,
		PricingCoverageRatio: option.PricingCoverageRatio, SourceModelPattern: pattern,
		RecommendedRuleType: "add", RecommendedValue: recommended,
		ExistingPolicyIDs: append([]int64(nil), candidate.ExistingPolicyIDs...), Reasons: reasons}
}

func (s *Service) ManualMaterialize(ctx context.Context, request ManualMaterializeRequest) (ManualMaterializeResult, error) {
	if request.TargetSiteID <= 0 || strings.TrimSpace(request.TargetEvidenceHash) == "" {
		return ManualMaterializeResult{}, errors.New("target_site_id and target_evidence_hash are required")
	}
	if len(request.Selections) == 0 || len(request.Selections) > 500 {
		return ManualMaterializeResult{}, errors.New("selections must contain between 1 and 500 items")
	}
	supplierIDs := make([]int64, 0, len(request.Selections))
	seenTargets := make(map[string]struct{}, len(request.Selections))
	for _, selection := range request.Selections {
		if !selection.ConfirmManual {
			return ManualMaterializeResult{}, errors.New("every manual selection requires confirm_manual=true")
		}
		if selection.SupplierID <= 0 || strings.TrimSpace(selection.CandidateID) == "" || strings.TrimSpace(selection.SourceGroup) == "" ||
			strings.TrimSpace(selection.SourceSnapshotHash) == "" || strings.TrimSpace(selection.TargetGroup) == "" {
			return ManualMaterializeResult{}, errors.New("every selection requires candidate_id, supplier_id, source_group, source_snapshot_hash and target_group")
		}
		key := ownerKey(request.TargetSiteID, selection.TargetGroup)
		if _, exists := seenTargets[key]; exists {
			return ManualMaterializeResult{}, errors.New("a manual batch cannot contain the same target group more than once")
		}
		seenTargets[key] = struct{}{}
		supplierIDs = append(supplierIDs, selection.SupplierID)
	}
	preview, err := s.ManualPreview(ctx, ManualPreviewRequest{TargetSiteID: request.TargetSiteID, SupplierIDs: supplierIDs})
	if err != nil {
		return ManualMaterializeResult{}, err
	}
	if preview.TargetEvidenceHash != strings.TrimSpace(request.TargetEvidenceHash) {
		return ManualMaterializeResult{}, &ConflictError{Message: "target catalog or channels changed since preview; generate a new manual mapping preview"}
	}
	target, err := s.store.GetTarget(ctx, request.TargetSiteID)
	if err != nil {
		return ManualMaterializeResult{}, err
	}
	groups := make(map[string]ManualTargetGroup, len(preview.Groups))
	for _, group := range preview.Groups {
		groups[strings.ToLower(strings.TrimSpace(group.TargetGroup))] = group
	}
	result := ManualMaterializeResult{TargetSiteID: request.TargetSiteID, TargetEvidenceHash: preview.TargetEvidenceHash,
		Policies: make([]ManualMaterializedPolicyResult, 0, len(request.Selections))}
	mutations := make([]store.ManualMappingPolicyMutation, 0, len(request.Selections))
	selectedGroups := make([]ManualTargetGroup, 0, len(request.Selections))
	for index, selection := range request.Selections {
		group, exists := groups[strings.ToLower(strings.TrimSpace(selection.TargetGroup))]
		if !exists {
			return ManualMaterializeResult{}, &ConflictError{Message: fmt.Sprintf("selection %d target group is no longer present", index)}
		}
		if group.Status == "conflict" {
			return ManualMaterializeResult{}, &ConflictError{Message: fmt.Sprintf("target group %q has multiple existing policy owners", group.TargetGroup)}
		}
		var source *ManualSourceOption
		for optionIndex := range group.Options {
			option := &group.Options[optionIndex]
			if option.CandidateID == selection.CandidateID && option.SupplierID == selection.SupplierID &&
				strings.EqualFold(option.SourceGroup, selection.SourceGroup) {
				source = option
				break
			}
		}
		if source == nil {
			return ManualMaterializeResult{}, &ConflictError{Message: fmt.Sprintf("selection %d no longer matches a current source option", index)}
		}
		if source.SourceSnapshotHash != strings.TrimSpace(selection.SourceSnapshotHash) {
			return ManualMaterializeResult{}, &ConflictError{Message: fmt.Sprintf("source catalog for selection %d changed since preview", index)}
		}
		policy, policyErr := manualPolicy(target, group, *source, selection.Rule)
		if policyErr != nil {
			return ManualMaterializeResult{}, fmt.Errorf("selection %d: %w", index, policyErr)
		}
		mutations = append(mutations, store.ManualMappingPolicyMutation{Policy: policy,
			ExpectedPolicyID: selection.ExpectedPolicyID, ExpectedPolicyUpdatedAt: selection.ExpectedPolicyUpdatedAt})
		selectedGroups = append(selectedGroups, group)
	}
	savedPolicies, saveErr := s.store.SaveManualMappingPolicies(ctx, mutations)
	if saveErr != nil {
		var ownership *store.MappingOwnershipConflictError
		if errors.As(saveErr, &ownership) || errors.Is(saveErr, store.ErrManualMappingStale) {
			return ManualMaterializeResult{}, &ConflictError{Message: saveErr.Error()}
		}
		return ManualMaterializeResult{}, saveErr
	}
	for index, saved := range savedPolicies {
		group := selectedGroups[index]
		result.Policies = append(result.Policies, ManualMaterializedPolicyResult{TargetGroup: group.TargetGroup,
			Created: saved.Created, Updated: saved.Updated, Idempotent: saved.Idempotent, Policy: saved.Policy})
	}
	return result, nil
}

func manualPolicy(target domain.TargetSite, group ManualTargetGroup, source ManualSourceOption, input ManualRuleInput) (domain.PricingPolicy, error) {
	ruleType := strings.ToLower(strings.TrimSpace(input.RuleType))
	if ruleType == "" {
		ruleType = defaultString(source.RecommendedRuleType, "add")
	}
	supported := map[string]bool{"add": true, "multiply": true, "target_margin": true, "markup": true, "subsidy_rate": true, "fixed": true}
	if !supported[ruleType] {
		return domain.PricingPolicy{}, fmt.Errorf("unsupported rule type %q", input.RuleType)
	}
	value := strings.TrimSpace(input.Value)
	if value == "" {
		value = strings.TrimSpace(source.RecommendedValue)
	}
	if value == "" {
		return domain.PricingPolicy{}, errors.New("rule value is required because the current price evidence cannot derive a default")
	}
	executionMode := strings.ToLower(strings.TrimSpace(input.ExecutionMode))
	if executionMode == "" {
		executionMode = "approval"
	}
	if executionMode != "approval" && executionMode != "auto" {
		return domain.PricingPolicy{}, errors.New("execution_mode must be approval or auto")
	}
	if input.Enabled && executionMode == "auto" && !strings.EqualFold(strings.TrimSpace(target.WriteMode), "auto") {
		return domain.PricingPolicy{}, errors.New("the target site is not authorized for automatic writes; enable its auto write mode first or save this rule in approval mode")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = fmt.Sprintf("Manual: %s / %s -> %s", source.SupplierName, source.SourceGroup, group.TargetGroup)
	}
	if len(name) > 200 {
		name = name[:200]
	}
	policy := domain.PricingPolicy{Name: name, SupplierID: source.SupplierID, SourceGroup: source.SourceGroup,
		SourceModelPattern: defaultString(source.SourceModelPattern, "*"),
		SourceBasis:        "group_ratio", TargetSiteID: target.ID, TargetGroup: group.TargetGroup,
		RuleType: ruleType, Value: value, ProfitBasis: defaultString(strings.TrimSpace(input.ProfitBasis), "contribution"),
		RevenueCashFactor: defaultString(strings.TrimSpace(input.RevenueCashFactor), "1"),
		CostCashFactor:    defaultString(strings.TrimSpace(input.CostCashFactor), "1"),
		RiskBufferRate:    defaultString(strings.TrimSpace(input.RiskBufferRate), "0"), AllowNegativeProfit: input.AllowNegativeProfit,
		SubsidyBudgetDaily: defaultString(strings.TrimSpace(input.SubsidyBudgetDaily), "0"),
		MinimumRatio:       defaultString(strings.TrimSpace(input.MinimumRatio), "0"), MaximumRatio: defaultString(strings.TrimSpace(input.MaximumRatio), "0"),
		RoundingStep: defaultString(strings.TrimSpace(input.RoundingStep), "0.001"), OnlyFollowIncrease: input.OnlyFollowIncrease,
		OnlyFollowDecrease: input.OnlyFollowDecrease, Enabled: input.Enabled, ExecutionMode: executionMode,
		CoverageMode:    defaultString(strings.TrimSpace(input.CoverageMode), "threshold"),
		MinimumCoverage: defaultString(strings.TrimSpace(input.MinimumCoverage), "0.8")}
	groupRatio := strings.TrimSpace(source.SourceRatio)
	ruleSourceRatio := groupRatio
	if ruleSourceRatio == "" && ruleType == "fixed" {
		ruleSourceRatio = "0"
	}
	if ruleSourceRatio == "" {
		return domain.PricingPolicy{}, errors.New("the selected upstream group has no comparable source ratio; use a fixed rule or refresh its price evidence")
	}
	if _, err := pricing.Calculate(pricing.Input{SourceRatio: ruleSourceRatio, CostRatio: ruleSourceRatio,
		CurrentRatio: defaultString(group.TargetRatio, "0"), Policy: policy}); err != nil {
		return domain.PricingPolicy{}, fmt.Errorf("invalid pricing rule: %w", err)
	}
	return policy, nil
}
