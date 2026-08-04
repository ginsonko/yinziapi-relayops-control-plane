package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type policyDecisionFields struct {
	ID                  int64  `json:"id"`
	SupplierID          int64  `json:"supplier_id"`
	SourceGroup         string `json:"source_group"`
	SourceModelPattern  string `json:"source_model_pattern"`
	SourceBasis         string `json:"source_basis"`
	TargetSiteID        int64  `json:"target_site_id"`
	TargetGroup         string `json:"target_group"`
	RuleType            string `json:"rule_type"`
	Value               string `json:"value"`
	ProfitBasis         string `json:"profit_basis"`
	RevenueCashFactor   string `json:"revenue_cash_factor"`
	CostCashFactor      string `json:"cost_cash_factor"`
	RiskBufferRate      string `json:"risk_buffer_rate"`
	AllowNegativeProfit bool   `json:"allow_negative_profit"`
	SubsidyBudgetDaily  string `json:"subsidy_budget_daily"`
	MinimumRatio        string `json:"minimum_ratio"`
	MaximumRatio        string `json:"maximum_ratio"`
	RoundingStep        string `json:"rounding_step"`
	OnlyFollowIncrease  bool   `json:"only_follow_increase"`
	OnlyFollowDecrease  bool   `json:"only_follow_decrease"`
	Enabled             bool   `json:"enabled"`
	ExecutionMode       string `json:"execution_mode"`
	CoverageMode        string `json:"coverage_mode"`
	MinimumCoverage     string `json:"minimum_coverage_ratio"`
}

func PolicyFingerprint(policy domain.PricingPolicy) string {
	return hashDecisionInput(policyDecisionFields{
		ID: policy.ID, SupplierID: policy.SupplierID, SourceGroup: policy.SourceGroup,
		SourceModelPattern: policy.SourceModelPattern, SourceBasis: policy.SourceBasis,
		TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, RuleType: policy.RuleType,
		Value: policy.Value, ProfitBasis: policy.ProfitBasis, RevenueCashFactor: policy.RevenueCashFactor,
		CostCashFactor: policy.CostCashFactor, RiskBufferRate: policy.RiskBufferRate,
		AllowNegativeProfit: policy.AllowNegativeProfit, SubsidyBudgetDaily: policy.SubsidyBudgetDaily,
		MinimumRatio: policy.MinimumRatio, MaximumRatio: policy.MaximumRatio, RoundingStep: policy.RoundingStep,
		OnlyFollowIncrease: policy.OnlyFollowIncrease, OnlyFollowDecrease: policy.OnlyFollowDecrease,
		Enabled: policy.Enabled, ExecutionMode: policy.ExecutionMode, CoverageMode: policy.CoverageMode,
		MinimumCoverage: policy.MinimumCoverage,
	})
}

type DecisionFingerprintInput struct {
	Policy               domain.PricingPolicy
	SourceEvidenceHash   string
	PricingEvidenceHash  string
	TargetHash           string
	TargetCatalogHash    string
	RawSourceRatio       string
	EffectiveSourceRatio string
	RechargeValueFactor  string
	EquivalentCostRatio  string
	CurrentRatio         string
	CostRatioVector      map[string]string
}

func DecisionInputFingerprint(input DecisionFingerprintInput) string {
	return hashDecisionInput(struct {
		Policy               string            `json:"policy"`
		SourceEvidenceHash   string            `json:"source_evidence_hash"`
		PricingEvidenceHash  string            `json:"pricing_evidence_hash"`
		TargetHash           string            `json:"target_hash"`
		TargetCatalogHash    string            `json:"target_catalog_hash"`
		RawSourceRatio       string            `json:"raw_source_ratio"`
		EffectiveSourceRatio string            `json:"effective_source_ratio"`
		RechargeValueFactor  string            `json:"recharge_value_factor"`
		EquivalentCostRatio  string            `json:"equivalent_cost_ratio"`
		CurrentRatio         string            `json:"current_ratio"`
		CostRatioVector      map[string]string `json:"cost_ratio_vector"`
	}{
		Policy: PolicyFingerprint(input.Policy), SourceEvidenceHash: input.SourceEvidenceHash,
		PricingEvidenceHash: input.PricingEvidenceHash, TargetHash: input.TargetHash,
		TargetCatalogHash: input.TargetCatalogHash, RawSourceRatio: input.RawSourceRatio,
		EffectiveSourceRatio: input.EffectiveSourceRatio, RechargeValueFactor: input.RechargeValueFactor,
		EquivalentCostRatio: input.EquivalentCostRatio, CurrentRatio: input.CurrentRatio,
		CostRatioVector: input.CostRatioVector,
	})
}

func hashDecisionInput(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
