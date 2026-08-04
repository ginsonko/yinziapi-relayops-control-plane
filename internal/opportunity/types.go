package opportunity

import "time"

const (
	ClassificationRecommended  = "recommended"
	ClassificationWatch        = "watch"
	ClassificationNoAdvantage  = "no_advantage"
	ClassificationInsufficient = "insufficient_data"
)

type Config struct {
	RecentWeight7D          string   `json:"recent_weight_7d"`
	RecentWeight30D         string   `json:"recent_weight_30d"`
	PriorDailyDemand        string   `json:"prior_daily_demand"`
	PriorConfidence         string   `json:"prior_confidence"`
	MinimumConfidence       string   `json:"minimum_confidence"`
	MinimumAvailability     string   `json:"minimum_availability"`
	MinimumCoverage         string   `json:"minimum_coverage"`
	DemandLowerFactor       string   `json:"demand_lower_factor"`
	PriorLowerFactor        string   `json:"prior_lower_factor"`
	CostUpperRiskRate       string   `json:"cost_upper_risk_rate"`
	MonthlyDays             string   `json:"monthly_days"`
	FeeRate                 string   `json:"fee_rate"`
	RetryLossRate           string   `json:"retry_loss_rate"`
	AllowedModelFamilies    []string `json:"allowed_model_families"`
	BlacklistedEntities     []string `json:"blacklisted_entities"`
	TargetCatalogTTLSeconds int      `json:"target_catalog_ttl_seconds"`
	UsageTTLSeconds         int      `json:"usage_ttl_seconds"`
}

func DefaultConfig() Config {
	return Config{
		RecentWeight7D: "0.7", RecentWeight30D: "0.3", PriorDailyDemand: "1", PriorConfidence: "0.25",
		MinimumConfidence: "0.65", MinimumAvailability: "0.7", MinimumCoverage: "0.5",
		DemandLowerFactor: "0.65", PriorLowerFactor: "0.1", CostUpperRiskRate: "0.15", MonthlyDays: "30",
		FeeRate: "0", RetryLossRate: "0.03", AllowedModelFamilies: []string{}, BlacklistedEntities: []string{},
		TargetCatalogTTLSeconds: 900, UsageTTLSeconds: 900,
	}
}

// ModelInput is one comparable model-level economic row. Monetary values are
// decimal strings in Currency per normalized demand unit. Empty means unknown;
// an explicit "0" is a real zero or free price.
type ModelInput struct {
	ModelID              string `json:"model_id"`
	Family               string `json:"family,omitempty"`
	Currency             string `json:"currency,omitempty"`
	Recognized           bool   `json:"recognized"`
	Demand7D             string `json:"demand_7d,omitempty"`
	Demand30D            string `json:"demand_30d,omitempty"`
	DemandConfidence     string `json:"demand_confidence,omitempty"`
	DemandUnit           string `json:"demand_unit,omitempty"`
	PriorDailyDemand     string `json:"prior_daily_demand,omitempty"`
	SalesUnitPrice       string `json:"sales_unit_price,omitempty"`
	TargetSalesRatio     string `json:"target_sales_ratio,omitempty"`
	CandidateUnitCost    string `json:"candidate_unit_cost,omitempty"`
	CandidateCostUpper   string `json:"candidate_cost_upper,omitempty"`
	CurrentUnitCost      string `json:"current_unit_cost,omitempty"`
	FeesUnit             string `json:"fees_unit,omitempty"`
	RetryLossUnit        string `json:"retry_loss_unit,omitempty"`
	PriceUnit            string `json:"price_unit,omitempty"`
	ComparableDimensions int    `json:"comparable_dimensions"`
	TotalDimensions      int    `json:"total_dimensions"`
}

type Candidate struct {
	ID                   string       `json:"id"`
	SupplierID           int64        `json:"supplier_id"`
	Supplier             string       `json:"supplier"`
	Group                string       `json:"group"`
	EntityFingerprint    string       `json:"entity_fingerprint"`
	Models               []ModelInput `json:"models"`
	TargetIntersection   bool         `json:"target_intersection"`
	CreateEligible       bool         `json:"create_eligible"`
	DuplicateFingerprint bool         `json:"duplicate_fingerprint"`
	Blacklisted          bool         `json:"blacklisted"`
	Unavailable          bool         `json:"unavailable"`
	Availability         string       `json:"availability,omitempty"`
	AvailabilityLower    string       `json:"availability_lower,omitempty"`
	EvidenceConfidence   string       `json:"evidence_confidence,omitempty"`
	AllowSubsidy         bool         `json:"allow_subsidy"`
	SubsidyBudgetDaily   string       `json:"subsidy_budget_daily,omitempty"`
	ObservedAt           time.Time    `json:"observed_at"`
}

type ModelResult struct {
	ModelID                string `json:"model_id"`
	Family                 string `json:"family,omitempty"`
	DailyDemand            string `json:"daily_demand"`
	ExpectedDailyProfit    string `json:"expected_daily_profit"`
	ExpectedDailySaving    string `json:"expected_daily_saving"`
	ConservativeLowerBound string `json:"conservative_lower_bound"`
	ComparableCoverage     string `json:"comparable_coverage"`
	DemandEvidence         string `json:"demand_evidence"`
	DemandUnit             string `json:"demand_unit,omitempty"`
	PriceUnit              string `json:"price_unit,omitempty"`
}

type Result struct {
	ID                     string        `json:"id"`
	SupplierID             int64         `json:"supplier_id"`
	Supplier               string        `json:"supplier"`
	Group                  string        `json:"group"`
	Classification         string        `json:"classification"`
	Eligible               bool          `json:"eligible"`
	ExpectedDailyProfit    string        `json:"expected_daily_profit,omitempty"`
	ExpectedMonthlyProfit  string        `json:"expected_monthly_profit,omitempty"`
	ExpectedDailySaving    string        `json:"expected_daily_saving,omitempty"`
	ExpectedMonthlySaving  string        `json:"expected_monthly_saving,omitempty"`
	GrossMargin            string        `json:"gross_margin,omitempty"`
	RecommendedRuleType    string        `json:"recommended_rule_type,omitempty"`
	RecommendedRuleValue   string        `json:"recommended_rule_value,omitempty"`
	RecommendedRounding    string        `json:"recommended_rounding_step,omitempty"`
	RecommendationBasis    string        `json:"recommendation_basis,omitempty"`
	RecommendationNote     string        `json:"recommendation_note,omitempty"`
	ConservativeLowerBound string        `json:"conservative_lower_bound,omitempty"`
	ModelCoverage          string        `json:"model_coverage,omitempty"`
	Availability           string        `json:"availability,omitempty"`
	Confidence             string        `json:"confidence,omitempty"`
	Currency               string        `json:"currency,omitempty"`
	ModelIDs               []string      `json:"model_ids"`
	ModelFamilies          []string      `json:"model_families"`
	Models                 []ModelResult `json:"models"`
	Reason                 string        `json:"reason"`
	Warnings               []string      `json:"warnings"`
	HardFilterReasons      []string      `json:"hard_filter_reasons"`
	UsesPrior              bool          `json:"uses_prior"`
	SubsidyRequiredDaily   string        `json:"subsidy_required_daily,omitempty"`
	SubsidyBudgetDaily     string        `json:"subsidy_budget_daily,omitempty"`
	SubsidyWithinBudget    bool          `json:"subsidy_within_budget,omitempty"`
	ObservedAt             time.Time     `json:"observed_at"`
	EvaluatedAt            time.Time     `json:"evaluated_at"`
	InputHash              string        `json:"input_hash"`
}
