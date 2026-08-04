package mappingdraft

import (
	"errors"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const (
	DefaultMinimumScore        = "0.7800"
	DefaultMinimumModelJaccard = "0.5000"
	DefaultMinimumScoreMargin  = "0.0800"
	DefaultMaximumOptions      = 5
)

type PreviewRequest struct {
	TargetSiteID        int64   `json:"target_site_id"`
	SupplierIDs         []int64 `json:"supplier_ids,omitempty"`
	MinimumScore        string  `json:"minimum_score,omitempty"`
	MinimumModelJaccard string  `json:"minimum_model_jaccard,omitempty"`
	MinimumScoreMargin  string  `json:"minimum_score_margin,omitempty"`
	MaximumOptions      int     `json:"maximum_options,omitempty"`
}

type Thresholds struct {
	MinimumScore        string `json:"minimum_score"`
	MinimumModelJaccard string `json:"minimum_model_jaccard"`
	MinimumScoreMargin  string `json:"minimum_score_margin"`
}

type Preview struct {
	TargetSiteID       int64           `json:"target_site_id"`
	TargetName         string          `json:"target_name"`
	TargetEvidenceHash string          `json:"target_evidence_hash"`
	TargetCatalogHash  string          `json:"target_catalog_hash"`
	TargetObservedAt   time.Time       `json:"target_observed_at"`
	Thresholds         Thresholds      `json:"thresholds"`
	Summary            PreviewSummary  `json:"summary"`
	Candidates         []Candidate     `json:"candidates"`
	SupplierIssues     []SupplierIssue `json:"supplier_issues"`
	GeneratedAt        time.Time       `json:"generated_at"`
}

type PreviewSummary struct {
	SuppliersRequested int `json:"suppliers_requested"`
	SuppliersUsable    int `json:"suppliers_usable"`
	SourceGroups       int `json:"source_groups"`
	Prefilled          int `json:"prefilled"`
	NeedsConfirmation  int `json:"needs_confirmation"`
	AlreadyMapped      int `json:"already_mapped"`
	Conflicts          int `json:"conflicts"`
}

type SupplierIssue struct {
	SupplierID   int64  `json:"supplier_id"`
	SupplierName string `json:"supplier_name,omitempty"`
	Kind         string `json:"kind"`
	Message      string `json:"message"`
}

type Candidate struct {
	ID                  string        `json:"id"`
	SupplierID          int64         `json:"supplier_id"`
	SupplierName        string        `json:"supplier_name"`
	SourceGroup         string        `json:"source_group"`
	SourceGroupName     string        `json:"source_group_name"`
	SourceRatio         string        `json:"source_ratio,omitempty"`
	RawSourceRatio      string        `json:"raw_source_ratio,omitempty"`
	RechargeValueFactor string        `json:"recharge_value_factor,omitempty"`
	SourceModels        int           `json:"source_models"`
	SourceSnapshotHash  string        `json:"source_snapshot_hash"`
	SourceObservedAt    time.Time     `json:"source_observed_at"`
	SourceFresh         bool          `json:"source_fresh"`
	Focused             bool          `json:"focused"`
	Status              string        `json:"status"`
	ConfidenceLevel     string        `json:"confidence_level"`
	Score               string        `json:"score"`
	SourceRunnerUpScore string        `json:"source_runner_up_score"`
	SourceScoreMargin   string        `json:"source_score_margin"`
	ScoreIsBest         bool          `json:"score_is_best"`
	ProposedTargetGroup string        `json:"proposed_target_group,omitempty"`
	ExistingPolicyIDs   []int64       `json:"existing_policy_ids,omitempty"`
	Policy              *PolicyDraft  `json:"policy,omitempty"`
	Options             []MatchOption `json:"options"`
	Reasons             []string      `json:"reasons"`
	TargetEvidenceHash  string        `json:"target_evidence_hash"`
	TargetCompetition   Competition   `json:"target_competition"`
}

type Competition struct {
	Contenders int    `json:"contenders"`
	NextScore  string `json:"next_score,omitempty"`
	Margin     string `json:"margin,omitempty"`
}

type MatchOption struct {
	allOverlappingModels     []string
	TargetGroup              string        `json:"target_group"`
	TargetRatio              string        `json:"target_ratio,omitempty"`
	EquivalentCostRatio      string        `json:"equivalent_cost_ratio,omitempty"`
	PricingBasis             string        `json:"pricing_basis,omitempty"`
	PricingCoverageRatio     string        `json:"pricing_coverage_ratio,omitempty"`
	PricingImpactComplete    bool          `json:"pricing_impact_complete"`
	PricingImpactConfidence  string        `json:"pricing_impact_confidence,omitempty"`
	PricingUnknownDimensions []string      `json:"pricing_unknown_dimensions,omitempty"`
	PricingWarnings          []string      `json:"pricing_warnings,omitempty"`
	PricingImpactError       string        `json:"pricing_impact_error,omitempty"`
	Score                    string        `json:"score"`
	ModelJaccard             string        `json:"model_jaccard"`
	FamilyJaccard            string        `json:"family_jaccard"`
	NameSimilarity           string        `json:"name_similarity"`
	RatioSimilarity          string        `json:"ratio_similarity,omitempty"`
	OriginMatch              bool          `json:"origin_match"`
	NameExact                bool          `json:"name_exact"`
	MatchedChannelIDs        []int64       `json:"matched_channel_ids,omitempty"`
	OverlappingModels        []string      `json:"overlapping_models"`
	OverlappingModelsOmitted int           `json:"overlapping_models_omitted,omitempty"`
	SourceModelCount         int           `json:"source_model_count"`
	TargetModelCount         int           `json:"target_model_count"`
	ExistingOwner            *PolicyOwner  `json:"existing_owner,omitempty"`
	Factors                  []ScoreFactor `json:"factors"`
	EligibleForPrefill       bool          `json:"eligible_for_prefill"`
	IneligibleReasons        []string      `json:"ineligible_reasons"`
}

type PolicyOwner struct {
	PolicyID      int64  `json:"policy_id"`
	SupplierID    int64  `json:"supplier_id"`
	SourceGroup   string `json:"source_group"`
	Enabled       bool   `json:"enabled"`
	ExecutionMode string `json:"execution_mode"`
}

type ScoreFactor struct {
	Key       string `json:"key"`
	Weight    string `json:"weight"`
	Evidence  string `json:"evidence"`
	Component string `json:"component"`
}

type PolicyDraft struct {
	Name               string `json:"name"`
	SupplierID         int64  `json:"supplier_id"`
	SourceGroup        string `json:"source_group"`
	SourceModelPattern string `json:"source_model_pattern"`
	TargetSiteID       int64  `json:"target_site_id"`
	TargetGroup        string `json:"target_group"`
	RuleType           string `json:"rule_type"`
	Value              string `json:"value"`
	ExecutionMode      string `json:"execution_mode"`
	Enabled            bool   `json:"enabled"`
}

type MaterializeRequest struct {
	TargetSiteID       int64                  `json:"target_site_id"`
	TargetEvidenceHash string                 `json:"target_evidence_hash"`
	Selections         []MaterializeSelection `json:"selections"`
}

type MaterializeSelection struct {
	CandidateID          string `json:"candidate_id"`
	SupplierID           int64  `json:"supplier_id"`
	SourceGroup          string `json:"source_group"`
	SourceSnapshotHash   string `json:"source_snapshot_hash"`
	TargetGroup          string `json:"target_group"`
	ConfirmLowConfidence bool   `json:"confirm_low_confidence,omitempty"`
}

type MaterializeResult struct {
	TargetSiteID       int64                      `json:"target_site_id"`
	TargetEvidenceHash string                     `json:"target_evidence_hash"`
	Policies           []MaterializedPolicyResult `json:"policies"`
}

type MaterializedPolicyResult struct {
	CandidateID string               `json:"candidate_id"`
	Created     bool                 `json:"created"`
	Idempotent  bool                 `json:"idempotent"`
	Policy      domain.PricingPolicy `json:"policy"`
}

type ConflictError struct {
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

func IsConflict(err error) bool {
	var conflict *ConflictError
	return errors.As(err, &conflict)
}
