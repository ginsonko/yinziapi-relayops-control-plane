package intelligence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/forecast"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	ModelVersion                   = "phase-a-v5"
	MaximumForecastHorizonHours    = 7 * 24
	demandUnit                     = "归一化需求单位"
	equivalentContributionUnit     = "倍率加权等价贡献单位"
	equivalentContributionBasis    = "ratio_weighted_equivalent_v2"
	cashContributionBasis          = "ledger_derived_cash_contribution_v1"
	contractCashContributionBasis  = "contract_planned_cash_contribution_v1"
	standardNormalNinetiethPercent = 1.2815515655446004
	ConfigSettingKey               = "intelligence.config.v1"
	dashboardCacheTTL              = 5 * time.Minute
	dashboardBackgroundTimeout     = 4 * time.Minute
)

type Config struct {
	PriceElasticity       string                       `json:"price_elasticity"`
	PriorUncertainty      string                       `json:"prior_uncertainty"`
	ObservedUncertainty   string                       `json:"observed_uncertainty"`
	MinimumCalibrationN   int                          `json:"minimum_calibration_samples"`
	ForecastCurrency      string                       `json:"forecast_currency,omitempty"`
	MinimumLedgerCoverage string                       `json:"minimum_ledger_coverage"`
	RelativeErrorFloor    string                       `json:"relative_error_floor"`
	CashLookbackDays      int                          `json:"cash_lookback_days"`
	FinancialLookbackDays int                          `json:"financial_lookback_days"`
	DisplayCurrency       string                       `json:"display_currency"`
	CurrencyRates         map[string]string            `json:"currency_rates"`
	BusinessTimeZone      string                       `json:"business_time_zone"`
	LedgerExclusions      *store.LedgerExclusionPolicy `json:"ledger_exclusions,omitempty"`
}

func DefaultConfig() Config {
	return Config{PriceElasticity: "-0.35", PriorUncertainty: "0.55", ObservedUncertainty: "0.3", MinimumCalibrationN: 10,
		MinimumLedgerCoverage: "0.95", RelativeErrorFloor: "0.01", CashLookbackDays: 7, FinancialLookbackDays: 7,
		DisplayCurrency: "CNY", CurrencyRates: map[string]string{"CNY": "1", "USD": "1"}, BusinessTimeZone: "Asia/Hong_Kong",
		LedgerExclusions: &store.LedgerExclusionPolicy{ExcludeAdministrators: true, ExcludeTestUsers: true, ExcludeInternalTokens: true}}
}

func NormalizeConfig(config Config) Config {
	defaults := DefaultConfig()
	if strings.TrimSpace(config.PriceElasticity) == "" {
		config.PriceElasticity = defaults.PriceElasticity
	}
	if strings.TrimSpace(config.PriorUncertainty) == "" {
		config.PriorUncertainty = defaults.PriorUncertainty
	}
	if strings.TrimSpace(config.ObservedUncertainty) == "" {
		config.ObservedUncertainty = defaults.ObservedUncertainty
	}
	if config.MinimumCalibrationN <= 0 {
		config.MinimumCalibrationN = defaults.MinimumCalibrationN
	}
	if strings.TrimSpace(config.MinimumLedgerCoverage) == "" {
		config.MinimumLedgerCoverage = defaults.MinimumLedgerCoverage
	}
	if strings.TrimSpace(config.RelativeErrorFloor) == "" {
		config.RelativeErrorFloor = defaults.RelativeErrorFloor
	}
	if config.CashLookbackDays <= 0 {
		config.CashLookbackDays = defaults.CashLookbackDays
	}
	if config.FinancialLookbackDays <= 0 {
		config.FinancialLookbackDays = defaults.FinancialLookbackDays
	}
	config.DisplayCurrency = strings.ToUpper(strings.TrimSpace(config.DisplayCurrency))
	if config.DisplayCurrency == "" {
		config.DisplayCurrency = defaults.DisplayCurrency
	}
	rates := make(map[string]string, len(defaults.CurrencyRates)+len(config.CurrencyRates))
	for currency, rate := range defaults.CurrencyRates {
		rates[currency] = rate
	}
	for currency, rate := range config.CurrencyRates {
		currency = strings.ToUpper(strings.TrimSpace(currency))
		if currency != "" {
			rates[currency] = strings.TrimSpace(rate)
		}
	}
	config.CurrencyRates = rates
	config.BusinessTimeZone = strings.TrimSpace(config.BusinessTimeZone)
	if config.BusinessTimeZone == "" {
		config.BusinessTimeZone = defaults.BusinessTimeZone
	}
	if config.LedgerExclusions == nil {
		copy := *defaults.LedgerExclusions
		config.LedgerExclusions = &copy
	} else {
		copy := *config.LedgerExclusions
		copy.ExcludedUserSegments = normalizeStringSet(copy.ExcludedUserSegments)
		copy.ExcludedUserIDs = normalizeStringSet(copy.ExcludedUserIDs)
		copy.ExcludedTokenIDs = normalizeStringSet(copy.ExcludedTokenIDs)
		config.LedgerExclusions = &copy
	}
	config.ForecastCurrency = strings.ToUpper(strings.TrimSpace(config.ForecastCurrency))
	return config
}

func ValidateConfig(config Config) error {
	config = NormalizeConfig(config)
	elasticity, err := decimal.NewFromString(config.PriceElasticity)
	if err != nil || elasticity.IsPositive() || elasticity.LessThan(decimal.NewFromInt(-5)) {
		return errors.New("price_elasticity must be between -5 and 0")
	}
	for name, raw := range map[string]string{"prior_uncertainty": config.PriorUncertainty, "observed_uncertainty": config.ObservedUncertainty} {
		value, err := decimal.NewFromString(raw)
		if err != nil || value.IsNegative() || value.GreaterThan(decimal.NewFromInt(2)) {
			return fmt.Errorf("%s must be between 0 and 2", name)
		}
	}
	if config.MinimumCalibrationN < 3 || config.MinimumCalibrationN > 1000 {
		return errors.New("minimum_calibration_samples must be between 3 and 1000")
	}
	if config.ForecastCurrency != "" && !validCurrencyCode(config.ForecastCurrency) {
		return errors.New("forecast_currency must be an empty value or a three-letter currency code")
	}
	coverage, err := decimal.NewFromString(config.MinimumLedgerCoverage)
	if err != nil || coverage.IsNegative() || coverage.GreaterThan(decimal.NewFromInt(1)) {
		return errors.New("minimum_ledger_coverage must be between 0 and 1")
	}
	errorFloor, err := decimal.NewFromString(config.RelativeErrorFloor)
	if err != nil || errorFloor.IsNegative() {
		return errors.New("relative_error_floor must be zero or positive")
	}
	if config.CashLookbackDays < 7 || config.CashLookbackDays > 365 {
		return errors.New("cash_lookback_days must be between 7 and 365")
	}
	if config.FinancialLookbackDays < 1 || config.FinancialLookbackDays > 3660 {
		return errors.New("financial_lookback_days must be between 1 and 3660")
	}
	if !validCurrencyCode(config.DisplayCurrency) {
		return errors.New("display_currency must be a three-letter currency code")
	}
	if len(config.CurrencyRates) == 0 || len(config.CurrencyRates) > 64 {
		return errors.New("currency_rates must contain between 1 and 64 entries")
	}
	for currency, raw := range config.CurrencyRates {
		if !validCurrencyCode(currency) {
			return fmt.Errorf("currency_rates contains invalid currency %q", currency)
		}
		rate, parseErr := decimal.NewFromString(strings.TrimSpace(raw))
		if parseErr != nil || !rate.IsPositive() {
			return fmt.Errorf("currency rate for %s must be a positive decimal", currency)
		}
	}
	if _, err := time.LoadLocation(config.BusinessTimeZone); err != nil {
		return fmt.Errorf("business_time_zone is invalid: %w", err)
	}
	for name, values := range map[string][]string{"excluded_user_segments": config.LedgerExclusions.ExcludedUserSegments,
		"excluded_user_ids": config.LedgerExclusions.ExcludedUserIDs, "excluded_token_ids": config.LedgerExclusions.ExcludedTokenIDs} {
		if len(values) > 1000 {
			return fmt.Errorf("%s cannot contain more than 1000 entries", name)
		}
		for _, value := range values {
			if len(value) > 160 {
				return fmt.Errorf("%s entries cannot exceed 160 characters", name)
			}
		}
	}
	return nil
}

func normalizeStringSet(values []string) []string {
	seen := make(map[string]string, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[strings.ToLower(value)] = value
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, seen[key])
	}
	return result
}

type EvidenceItem struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	Level      string    `json:"level"`
	Value      string    `json:"value,omitempty"`
	Unit       string    `json:"unit,omitempty"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Note       string    `json:"note"`
}

type Interval struct {
	Low  string `json:"low"`
	Base string `json:"base"`
	High string `json:"high"`
	Unit string `json:"unit"`
	P10  string `json:"p10,omitempty"`
	P50  string `json:"p50,omitempty"`
	P90  string `json:"p90,omitempty"`
}

type ScenarioEstimate struct {
	Demand       Interval `json:"demand"`
	Revenue      Interval `json:"revenue"`
	Cost         Interval `json:"cost"`
	Contribution Interval `json:"contribution"`
}

type HorizonForecast struct {
	Hours                int                          `json:"hours"`
	Label                string                       `json:"label"`
	LearningScope        string                       `json:"learning_scope,omitempty"`
	Approve              ScenarioEstimate             `json:"approve"`
	Reject               ScenarioEstimate             `json:"reject"`
	AdaptiveContribution *Interval                    `json:"adaptive_contribution,omitempty"`
	Learning             *LearningAdjustment          `json:"learning,omitempty"`
	DeltaContribution    Interval                     `json:"delta_contribution"`
	PositiveScore        string                       `json:"positive_score"`
	IntervalMethod       string                       `json:"interval_method"`
	Calibrated           bool                         `json:"calibrated"`
	MetricBasis          string                       `json:"metric_basis"`
	DemandUnits          map[string]string            `json:"demand_units,omitempty"`
	MinimumCoverage      string                       `json:"minimum_coverage,omitempty"`
	RelativeErrorFloor   string                       `json:"relative_error_floor,omitempty"`
	LedgerExclusions     *store.LedgerExclusionPolicy `json:"ledger_exclusions,omitempty"`
	DueAt                *time.Time                   `json:"due_at,omitempty"`
	OutcomeStatus        string                       `json:"outcome_status"`
	OutcomeNote          string                       `json:"outcome_note"`
	Observation          *ObservedOutcome             `json:"observation,omitempty"`
}

// LearningAdjustment calibrates the observable approved-outcome projection.
// It never rewrites price elasticity or claims that the observed residual was
// caused by the price decision.
type LearningAdjustment struct {
	Active                    bool   `json:"active"`
	DistinctProposals         int    `json:"distinct_proposals"`
	IndependentWindows        int    `json:"independent_windows"`
	MinimumWindows            int    `json:"minimum_windows"`
	BiasCorrection            string `json:"bias_correction,omitempty"`
	MeanAbsoluteRelativeError string `json:"mean_absolute_relative_error,omitempty"`
	RelativeResidualP80       string `json:"relative_residual_p80,omitempty"`
	ProjectionScale           string `json:"projection_scale,omitempty"`
	ShrinkageWeight           string `json:"shrinkage_weight,omitempty"`
	Method                    string `json:"method"`
	Causal                    bool   `json:"causal"`
	Note                      string `json:"note"`
}

type ObservedOutcome struct {
	Contribution     string            `json:"contribution"`
	Revenue          string            `json:"revenue"`
	Cost             string            `json:"cost"`
	Currency         string            `json:"currency,omitempty"`
	Unit             string            `json:"unit"`
	EventCount       int               `json:"event_count"`
	Evidence         string            `json:"evidence"`
	EligibleQuantity string            `json:"eligible_quantity"`
	IncludedQuantity string            `json:"included_quantity"`
	CoverageRatio    string            `json:"coverage_ratio"`
	UsageUnits       map[string]string `json:"usage_units,omitempty"`
	ExcludedByReason map[string]int    `json:"excluded_by_reason,omitempty"`
}

type Recommendation struct {
	ProposalID            int64             `json:"proposal_id"`
	PolicyID              int64             `json:"policy_id"`
	TargetSiteID          int64             `json:"target_site_id"`
	PolicyName            string            `json:"policy_name"`
	Supplier              string            `json:"supplier"`
	Target                string            `json:"target"`
	TargetGroup           string            `json:"target_group"`
	ModelPattern          string            `json:"model_pattern,omitempty"`
	Status                string            `json:"status"`
	LifecycleStatus       string            `json:"lifecycle_status"`
	CurrentRatio          string            `json:"current_ratio,omitempty"`
	ProposedRatio         string            `json:"proposed_ratio"`
	PricingMode           string            `json:"pricing_mode,omitempty"`
	CurrentRevision       string            `json:"current_revision,omitempty"`
	ProposedRevision      string            `json:"proposed_revision,omitempty"`
	ForecastCoveredModels int               `json:"forecast_covered_models,omitempty"`
	ForecastTotalModels   int               `json:"forecast_total_models,omitempty"`
	ForecastCoverage      string            `json:"forecast_coverage,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	DecidedAt             *time.Time        `json:"decided_at,omitempty"`
	EffectiveAt           *time.Time        `json:"effective_at,omitempty"`
	DecisionKind          string            `json:"decision_kind,omitempty"`
	ForecastReady         bool              `json:"forecast_ready"`
	ForecastReason        string            `json:"forecast_reason,omitempty"`
	Forecasts             []HorizonForecast `json:"forecasts"`
	Evidence              []EvidenceItem    `json:"evidence"`
	Warnings              []string          `json:"warnings"`
	ModelVersion          string            `json:"model_version"`
	InputHash             string            `json:"input_hash"`
}

type Calibration struct {
	Ready                bool                 `json:"ready"`
	TotalOutcomes        int                  `json:"total_outcomes"`
	SampleCount          int                  `json:"sample_count"`
	DecisionCount        int                  `json:"decision_count"`
	MAPESampleCount      int                  `json:"mape_sample_count"`
	IntervalSamples      int                  `json:"band_sample_count"`
	MinimumSamples       int                  `json:"minimum_samples"`
	MAPE                 string               `json:"mape,omitempty"`
	MAE                  string               `json:"mae,omitempty"`
	WAPE                 string               `json:"wape,omitempty"`
	SMAPE                string               `json:"smape,omitempty"`
	SignedBias           string               `json:"signed_bias,omitempty"`
	IntervalCoverage     string               `json:"band_coverage,omitempty"`
	ZeroActualCount      int                  `json:"zero_actual_count"`
	NearZeroExcluded     int                  `json:"near_zero_excluded"`
	PerfectZeroCount     int                  `json:"perfect_zero_count"`
	CoverageExcluded     int                  `json:"coverage_excluded"`
	CensoredCount        int                  `json:"censored_count"`
	IncompatibleExcluded int                  `json:"incompatible_excluded"`
	Horizons             []HorizonCalibration `json:"horizons"`
	Note                 string               `json:"note"`
}

type HorizonCalibration struct {
	HorizonHours  int    `json:"horizon_hours"`
	Ready         bool   `json:"ready"`
	SampleCount   int    `json:"sample_count"`
	DecisionCount int    `json:"decision_count"`
	ModelVersion  string `json:"model_version"`
	MetricBasis   string `json:"metric_basis"`
	MetricUnit    string `json:"metric_unit"`
	MAPE          string `json:"mape,omitempty"`
	MAE           string `json:"mae,omitempty"`
	WAPE          string `json:"wape,omitempty"`
	SMAPE         string `json:"smape,omitempty"`
	SignedBias    string `json:"signed_bias,omitempty"`
	BandCoverage  string `json:"band_coverage,omitempty"`
}

type Summary struct {
	Recommendations    int `json:"recommendations"`
	Pending            int `json:"pending"`
	Applied            int `json:"applied"`
	ForecastReady      int `json:"forecast_ready"`
	LedgerEvents       int `json:"ledger_events"`
	ExactRevenueEvents int `json:"exact_revenue_events"`
	ExactCostEvents    int `json:"exact_cost_events"`
}

type UnifiedMoney struct {
	Amount       string `json:"amount,omitempty"`
	Currency     string `json:"currency"`
	Available    bool   `json:"available"`
	KnownItems   int    `json:"known_items"`
	UnknownItems int    `json:"unknown_items"`
	Evidence     string `json:"evidence"`
	Note         string `json:"note,omitempty"`
}

type UnifiedRatio struct {
	Value        string `json:"value,omitempty"`
	Available    bool   `json:"available"`
	Evidence     string `json:"evidence"`
	Numerator    string `json:"numerator,omitempty"`
	Denominator  string `json:"denominator,omitempty"`
	MatchedItems int    `json:"matched_items"`
	TotalItems   int    `json:"total_items"`
	Coverage     string `json:"coverage,omitempty"`
	Formula      string `json:"formula,omitempty"`
	Note         string `json:"note,omitempty"`
}

type SupplierCashPosition struct {
	SupplierID        int64        `json:"supplier_id"`
	SupplierName      string       `json:"supplier_name"`
	Balance           UnifiedMoney `json:"balance"`
	DailySpend        UnifiedMoney `json:"daily_spend"`
	RawBalance        string       `json:"raw_balance,omitempty"`
	RawDailySpend     string       `json:"raw_daily_spend,omitempty"`
	RawCurrency       string       `json:"raw_currency,omitempty"`
	RechargeFactor    string       `json:"recharge_factor"`
	ConversionFormula string       `json:"conversion_formula,omitempty"`
	RunwayHours       string       `json:"runway_hours,omitempty"`
	ObservedAt        time.Time    `json:"observed_at,omitempty"`
}

type GroupOperatingSummary struct {
	Group           string       `json:"group"`
	EventCount      int          `json:"event_count"`
	Requests        int64        `json:"requests"`
	Revenue         UnifiedMoney `json:"revenue"`
	Cost            UnifiedMoney `json:"cost"`
	GrossProfit     UnifiedMoney `json:"gross_profit"`
	GrossMargin     UnifiedRatio `json:"gross_margin"`
	RevenueCoverage string       `json:"revenue_coverage"`
	CostCoverage    string       `json:"cost_coverage"`
}

type ProfitDataQuality struct {
	WindowStart                    time.Time              `json:"window_start"`
	WindowEnd                      time.Time              `json:"window_end"`
	IncludedUsageEvents            int                    `json:"included_usage_events"`
	RevenueKnownEvents             int                    `json:"revenue_known_events"`
	RevenueUnknownEvents           int                    `json:"revenue_unknown_events"`
	CostKnownEvents                int                    `json:"cost_known_events"`
	CostUnknownEvents              int                    `json:"cost_unknown_events"`
	UnknownCurrencyEvents          int                    `json:"unknown_currency_events"`
	RevenueCoverage                string                 `json:"revenue_coverage"`
	CostCoverage                   string                 `json:"cost_coverage"`
	LedgerCoverageStatus           string                 `json:"ledger_coverage_status"`
	StrictProfitAvailable          bool                   `json:"strict_profit_available"`
	OperatingEstimateReady         bool                   `json:"operating_estimate_ready"`
	RequiredLedgerFields           []string               `json:"required_ledger_fields"`
	Blockers                       []string               `json:"blockers"`
	NextAction                     string                 `json:"next_action"`
	OperatingEstimateBasis         string                 `json:"operating_estimate_basis"`
	CostResolutionWindowStart      *time.Time             `json:"cost_resolution_window_start,omitempty"`
	CostResolutionWindowEnd        *time.Time             `json:"cost_resolution_window_end,omitempty"`
	CostResolutionObservedAt       *time.Time             `json:"cost_resolution_observed_at,omitempty"`
	CostResolutionScannedEvents    int                    `json:"cost_resolution_scanned_events"`
	CostResolutionResolvedEvents   int                    `json:"cost_resolution_resolved_events"`
	CostResolutionUnresolvedEvents int                    `json:"cost_resolution_unresolved_events"`
	CostResolutionTruncated        bool                   `json:"cost_resolution_truncated"`
	CostResolutionReasons          []CostResolutionReason `json:"cost_resolution_reasons"`
}

type CostResolutionReason struct {
	TargetSiteID int64                                    `json:"target_site_id"`
	TargetName   string                                   `json:"target_name"`
	Code         string                                   `json:"code"`
	Message      string                                   `json:"message"`
	Count        int                                      `json:"count"`
	Samples      []store.TargetLedgerCostResolutionSample `json:"samples"`
}

type OperatingSnapshot struct {
	DisplayCurrency                    string                  `json:"display_currency"`
	CurrencyRates                      map[string]string       `json:"currency_rates"`
	BusinessTimeZone                   string                  `json:"business_time_zone"`
	DayStartedAt                       time.Time               `json:"day_started_at"`
	AdminExcluded                      bool                    `json:"admin_excluded"`
	UserBalance                        UnifiedMoney            `json:"user_balance"`
	TodayUserSpend                     UnifiedMoney            `json:"today_user_spend"`
	UpstreamBalance                    UnifiedMoney            `json:"upstream_balance"`
	UpstreamDailySpend                 UnifiedMoney            `json:"upstream_daily_spend"`
	TodayRevenue                       UnifiedMoney            `json:"today_revenue"`
	TodayCost                          UnifiedMoney            `json:"today_cost"`
	TodayGrossProfit                   UnifiedMoney            `json:"today_gross_profit"`
	TodayGrossMargin                   UnifiedRatio            `json:"today_gross_margin"`
	TodayCommissionCost                UnifiedMoney            `json:"today_commission_cost"`
	TodayNetProfit                     UnifiedMoney            `json:"today_net_profit"`
	TodayNetMargin                     UnifiedRatio            `json:"today_net_margin"`
	OperatingGapRevenue                UnifiedMoney            `json:"operating_gap_revenue"`
	OperatingGapIncludesAdministrators bool                    `json:"operating_gap_includes_administrators"`
	OperatingGapEstimate               UnifiedMoney            `json:"operating_gap_estimate"`
	OperatingGapMargin                 UnifiedRatio            `json:"operating_gap_margin"`
	ProfitDataQuality                  ProfitDataQuality       `json:"profit_data_quality"`
	SupplierPositions                  []SupplierCashPosition  `json:"supplier_positions"`
	Groups                             []GroupOperatingSummary `json:"groups"`
	Warnings                           []string                `json:"warnings"`
	GeneratedAt                        time.Time               `json:"generated_at"`
}

type Dashboard struct {
	Summary          Summary                      `json:"summary"`
	Recommendations  []Recommendation             `json:"recommendations"`
	Ledger           store.LedgerSummary          `json:"ledger"`
	Financials       store.LedgerFinancialSummary `json:"financials"`
	Operations       OperatingSnapshot            `json:"operations"`
	Calibration      Calibration                  `json:"calibration"`
	EvidenceLegend   []EvidenceItem               `json:"evidence_legend"`
	Warnings         []string                     `json:"warnings"`
	Config           Config                       `json:"config"`
	GeneratedAt      time.Time                    `json:"generated_at"`
	CacheState       string                       `json:"cache_state"`
	CacheGeneratedAt time.Time                    `json:"cache_generated_at"`
	CacheRevision    int64                        `json:"cache_revision"`
	SourceRevision   int64                        `json:"source_revision"`
	Refreshing       bool                         `json:"refreshing"`
}

type OverviewSnapshot struct {
	Operations     OperatingSnapshot `json:"operations"`
	RawEventCount  int64             `json:"raw_event_count"`
	CacheState     string            `json:"cache_state"`
	CacheGenerated time.Time         `json:"cache_generated_at"`
	CacheRevision  int64             `json:"cache_revision"`
	SourceRevision int64             `json:"source_revision"`
	GeneratedAt    time.Time         `json:"generated_at"`
}

type Service struct {
	store *store.Store

	dashboardMu     sync.Mutex
	dashboardCache  dashboardCacheEntry
	dashboardFlight *dashboardFlight
	ledgerMu        sync.Mutex
	ledgerCache     ledgerCacheState
}

type dashboardCacheEntry struct {
	key             string
	expiresAt       time.Time
	result          Dashboard
	recommendations recommendationCacheEntry
}

type recommendationCacheEntry struct {
	expiresAt   time.Time
	items       []Recommendation
	calibration Calibration
	warnings    []string
}

type dashboardFlight struct {
	key    string
	done   chan struct{}
	result Dashboard
	err    error
}

func NewService(st *store.Store) *Service { return &Service{store: st} }

type unifiedMoneyAccumulator struct {
	total                    decimal.Decimal
	knownItems, unknownItems int
}

func (a *unifiedMoneyAccumulator) add(raw, currency string, items int, config Config) {
	if items <= 0 {
		return
	}
	value, valueErr := decimal.NewFromString(strings.TrimSpace(raw))
	rate, rateErr := decimal.NewFromString(strings.TrimSpace(config.CurrencyRates[strings.ToUpper(strings.TrimSpace(currency))]))
	if valueErr != nil || rateErr != nil || rate.IsNegative() || rate.IsZero() {
		a.unknownItems += items
		return
	}
	a.total = a.total.Add(value.Mul(rate))
	a.knownItems += items
}

func (a *unifiedMoneyAccumulator) unknown(items int) {
	if items > 0 {
		a.unknownItems += items
	}
}

func (a unifiedMoneyAccumulator) metric(config Config, evidence, note string) UnifiedMoney {
	result := UnifiedMoney{Currency: config.DisplayCurrency, KnownItems: a.knownItems, UnknownItems: a.unknownItems,
		Available: a.knownItems > 0 && a.unknownItems == 0, Evidence: evidence, Note: note}
	if a.knownItems > 0 {
		result.Amount = a.total.Round(6).String()
	}
	if a.knownItems == 0 {
		result.Evidence = "missing"
	} else if a.unknownItems > 0 {
		result.Evidence = "partial"
	}
	return result
}

func (s *Service) operatingSnapshot(ctx context.Context, config Config, now time.Time) (OperatingSnapshot, error) {
	cache, err := s.ledgerData(ctx)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	return s.operatingSnapshotCached(ctx, config, now, cache.Events, cache.Profiles)
}

func (s *Service) operatingSnapshotCached(ctx context.Context, config Config, now time.Time, events []store.OperatingLedgerEvent,
	profiles []store.TargetLedgerUserProfile) (OperatingSnapshot, error) {
	location, err := time.LoadLocation(config.BusinessTimeZone)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	localNow := now.In(location)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location).UTC()
	today, err := store.SummarizeOperatingFinancialEvents(events, store.LedgerFinancialReportOptions{
		Start: dayStart, End: now, Exclusions: *config.LedgerExclusions,
	})
	if err != nil {
		return OperatingSnapshot{}, err
	}
	fullSiteExclusions := *config.LedgerExclusions
	fullSiteExclusions.ExcludeAdministrators = false
	fullSiteToday, err := store.SummarizeOperatingFinancialEvents(events, store.LedgerFinancialReportOptions{
		Start: dayStart, End: now, Exclusions: fullSiteExclusions,
	})
	if err != nil {
		return OperatingSnapshot{}, err
	}
	result := OperatingSnapshot{DisplayCurrency: config.DisplayCurrency, CurrencyRates: cloneStringMap(config.CurrencyRates),
		BusinessTimeZone: config.BusinessTimeZone, DayStartedAt: dayStart, AdminExcluded: config.LedgerExclusions.ExcludeAdministrators,
		OperatingGapIncludesAdministrators: true, SupplierPositions: []SupplierCashPosition{}, Groups: []GroupOperatingSummary{}, Warnings: []string{}, GeneratedAt: now}

	var revenue, cost, matchedRevenue, matchedCost, commissionExpense, commissionReversal unifiedMoneyAccumulator
	for _, item := range today.ByCurrency {
		revenueKnown := item.UsageEventCount - item.UnknownUsageRevenueEvents
		costKnown := item.UsageEventCount - item.UnknownUsageCostEvents
		revenue.add(item.UsageRevenue, item.Currency, revenueKnown, config)
		cost.add(item.UsageProcurementCost, item.Currency, costKnown, config)
		matchedRevenue.add(item.MatchedUsageRevenue, item.Currency, item.MatchedUsageEventCount, config)
		matchedCost.add(item.MatchedUsageProcurementCost, item.Currency, item.MatchedUsageEventCount, config)
		commissionExpense.add(item.CommissionExpense, item.Currency, item.CommissionAwardEvents, config)
		commissionReversal.add(item.CommissionReversal, item.Currency, item.CommissionReversalEvents, config)
		revenue.unknown(item.UnknownUsageRevenueEvents)
		cost.unknown(item.UnknownUsageCostEvents)
	}
	revenue.unknown(today.UnknownUsageCurrencyEvents)
	cost.unknown(today.UnknownUsageCurrencyEvents)
	result.TodayRevenue = revenue.metric(config, "ledger_observed", "本站调用日志按 quota 现金因子换算；默认排除管理员、测试和内部调用")
	result.TodayUserSpend = result.TodayRevenue
	result.TodayUserSpend.Note = "今日用户实际扣费金额；与充值流水、用户剩余额度分开统计"
	result.TodayCost = cost.metric(config, "ledger_observed", fmt.Sprintf("请求级采购成本；当前 %d/%d 条调用已关联，充值与上游余额不计入",
		knownUsageCostEvents(today), today.IncludedUsageEventCount))
	strictGrossProfitAvailable := result.TodayRevenue.Available && result.TodayCost.Available
	if strictGrossProfitAvailable {
		profit := revenue.total.Sub(cost.total)
		result.TodayGrossProfit = UnifiedMoney{Amount: profit.Round(6).String(), Currency: config.DisplayCurrency,
			Available: true, KnownItems: revenue.knownItems, Evidence: "ledger_matched", Note: "同一今日窗口、同一排除口径下：用户扣费 - 请求级采购成本；不含充值"}
	} else if estimate, ok := estimatedGrossProfitMetric(revenue, matchedRevenue, matchedCost, today.IncludedUsageEventCount, config); ok {
		result.TodayGrossProfit = estimate
	} else {
		result.TodayGrossProfit = UnifiedMoney{Currency: config.DisplayCurrency, Available: false, Evidence: "missing",
			UnknownItems: maxInt(result.TodayRevenue.UnknownItems, result.TodayCost.UnknownItems),
			Note:         strictProfitMissingNote(today)}
	}
	// The primary ratio is weighted by matched cash revenue, never an unweighted
	// average of per-request percentages. It remains useful with partial cost
	// coverage, while the strict gross-profit amount above remains unavailable.
	result.TodayGrossMargin = weightedMarginMetric(matchedRevenue, matchedCost, today.IncludedUsageEventCount, result.AdminExcluded)
	commissionEvents := commissionExpense.knownItems + commissionReversal.knownItems
	if commissionEvents > 0 && commissionExpense.unknownItems+commissionReversal.unknownItems == 0 {
		netCommissionCost := commissionExpense.total.Sub(commissionReversal.total)
		result.TodayCommissionCost = UnifiedMoney{Amount: netCommissionCost.Round(6).String(), Currency: config.DisplayCurrency,
			Available: true, KnownItems: commissionEvents, Evidence: "ledger_exact",
			Note: "消费返佣支出 - 返佣冲正；来自 NewAPI 佣金事件账本，不含提现、充值或采购成本"}
	} else {
		result.TodayCommissionCost = UnifiedMoney{Currency: config.DisplayCurrency, Available: false, Evidence: "missing",
			UnknownItems: commissionExpense.unknownItems + commissionReversal.unknownItems,
			Note:         "当前窗口尚无可验证的消费返佣事件；不会把未知佣金成本按 0 计入净利润"}
	}
	if result.TodayGrossProfit.Available && result.TodayCommissionCost.Available {
		grossProfit, grossErr := decimal.NewFromString(result.TodayGrossProfit.Amount)
		commissionCost, commissionErr := decimal.NewFromString(result.TodayCommissionCost.Amount)
		if grossErr == nil && commissionErr == nil {
			netProfit := grossProfit.Sub(commissionCost)
			netEvidence := "ledger_matched"
			netNote := "今日现金毛利 - 消费佣金净成本；提现只结算既有佣金，不会重复计成本"
			if result.TodayGrossProfit.Evidence == "estimated" {
				netEvidence = "estimated"
				netNote = "估算值：今日毛利估算 - 已验证消费佣金净成本；严格成本覆盖率继续单独显示，不参与自动调价"
			}
			result.TodayNetProfit = UnifiedMoney{Amount: netProfit.Round(6).String(), Currency: config.DisplayCurrency,
				Available: true, KnownItems: result.TodayGrossProfit.KnownItems + commissionEvents,
				UnknownItems: result.TodayGrossProfit.UnknownItems, Evidence: netEvidence, Note: netNote}
			if revenue.total.IsPositive() {
				result.TodayNetMargin = UnifiedRatio{Value: netProfit.Div(revenue.total).Round(6).String(), Available: true,
					Evidence: netEvidence, Numerator: netProfit.Round(6).String(), Denominator: revenue.total.Round(6).String(),
					MatchedItems: result.TodayGrossProfit.KnownItems, TotalItems: today.IncludedUsageEventCount,
					Coverage: integerCoverage(result.TodayGrossProfit.KnownItems, today.IncludedUsageEventCount),
					Formula:  "(cash_revenue - procurement_cost - commission_awards + commission_reversals) / cash_revenue",
					Note:     netNote + "；佣金消费不会递归产生新佣金"}
			}
		}
	}
	if !result.TodayNetProfit.Available {
		result.TodayNetProfit = UnifiedMoney{Currency: config.DisplayCurrency, Available: false, Evidence: "missing",
			Note: "需要严格现金毛利和可验证的消费佣金成本后计算"}
		result.TodayNetMargin = UnifiedRatio{Available: false, Evidence: "missing",
			Formula: "(cash_revenue - procurement_cost - commission_awards + commission_reversals) / cash_revenue",
			Note:    "需要严格现金毛利、消费佣金成本和正数用户扣费后计算"}
	}
	result.ProfitDataQuality = buildProfitDataQuality(today, strictGrossProfitAvailable)
	var fullSiteRevenue unifiedMoneyAccumulator
	for _, item := range fullSiteToday.ByCurrency {
		known := item.UsageEventCount - item.UnknownUsageRevenueEvents
		fullSiteRevenue.add(item.UsageRevenue, item.Currency, known, config)
		fullSiteRevenue.unknown(item.UnknownUsageRevenueEvents)
	}
	fullSiteRevenue.unknown(fullSiteToday.UnknownUsageCurrencyEvents)
	result.OperatingGapRevenue = fullSiteRevenue.metric(config, "ledger_observed_full_site",
		"运营差额专用本站收入；包含管理员调用，仍遵守测试账号、内部令牌和管理员自定义排除项")

	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	enabledTargets := make(map[int64]bool, len(targets))
	for _, target := range targets {
		enabledTargets[target.ID] = target.Enabled
	}
	costStatuses, err := s.store.ListTargetLedgerCostResolutionStatuses(ctx)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	attachCostResolutionDiagnostics(&result.ProfitDataQuality, costStatuses, targets)
	var userBalance unifiedMoneyAccumulator
	unlimitedUsers := 0
	for _, profile := range profiles {
		if !enabledTargets[profile.TargetSiteID] {
			continue
		}
		event := store.OperatingLedgerEvent{SubjectUserID: profile.UserID, SubjectRole: profile.Role, UserSegment: profile.UserSegment,
			IsTest: profile.IsTest, IsInternal: profile.IsInternal}
		if len(store.LedgerEventExclusionReasons(event, *config.LedgerExclusions)) > 0 {
			continue
		}
		if profile.Unlimited {
			unlimitedUsers++
			userBalance.unknown(1)
			continue
		}
		if strings.TrimSpace(profile.CashBalance) == "" {
			userBalance.unknown(1)
			continue
		}
		userBalance.add(profile.CashBalance, profile.Currency, 1, config)
	}
	userBalanceNote := "来自本站 NewAPI 用户列表的剩余额度，按目标站 quota 现金因子换算"
	if len(profiles) == 0 {
		userBalanceNote = "尚未同步本站用户列表或上游未提供用户剩余额度字段"
	}
	if unlimitedUsers > 0 {
		userBalanceNote += fmt.Sprintf("；%d 个无限额度账号不计入有限余额合计", unlimitedUsers)
	}
	result.UserBalance = userBalance.metric(config, "target_user_snapshot", userBalanceNote)

	suppliers, err := s.store.ListSuppliers(ctx)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	forecasts, err := forecast.All(ctx, s.store, decimal.Zero)
	if err != nil {
		return OperatingSnapshot{}, err
	}
	forecastBySupplier := make(map[int64]domain.RunwayForecast, len(forecasts))
	for _, item := range forecasts {
		forecastBySupplier[item.SupplierID] = item
	}
	var upstreamBalance, upstreamSpend unifiedMoneyAccumulator
	for _, supplier := range suppliers {
		if !supplier.Enabled {
			continue
		}
		item, found := forecastBySupplier[supplier.ID]
		position := SupplierCashPosition{SupplierID: supplier.ID, SupplierName: supplier.Name, RechargeFactor: normalizedCashFactor(supplier.RechargeValueFactor)}
		if !found {
			upstreamBalance.unknown(1)
			upstreamSpend.unknown(1)
			position.Balance = UnifiedMoney{Currency: config.DisplayCurrency, Available: false, UnknownItems: 1, Evidence: "missing", Note: "尚无可信余额"}
			position.DailySpend = UnifiedMoney{Currency: config.DisplayCurrency, Available: false, UnknownItems: 1, Evidence: "missing", Note: "尚无近 24 小时消耗"}
			result.SupplierPositions = append(result.SupplierPositions, position)
			continue
		}
		position.RawBalance, position.RawDailySpend, position.RawCurrency = item.RawBalanceAmount, item.RawDailySpend, item.RawCurrency
		position.RechargeFactor, position.ConversionFormula, position.ObservedAt = item.CashFactor, item.ConversionFormula, item.ObservedAt
		if item.RunwayHours != nil {
			position.RunwayHours = *item.RunwayHours
		}
		var balance, spend unifiedMoneyAccumulator
		if strings.TrimSpace(item.BalanceAmount) == "" {
			balance.unknown(1)
			upstreamBalance.unknown(1)
		} else {
			balance.add(item.BalanceAmount, item.Currency, 1, config)
			upstreamBalance.add(item.BalanceAmount, item.Currency, 1, config)
		}
		if strings.TrimSpace(item.DailySpend) == "" {
			spend.unknown(1)
			upstreamSpend.unknown(1)
		} else {
			spend.add(item.DailySpend, item.Currency, 1, config)
			upstreamSpend.add(item.DailySpend, item.Currency, 1, config)
		}
		position.Balance = balance.metric(config, "supplier_balance", "上游余额已按充值折算系数换算")
		position.DailySpend = spend.metric(config, "supplier_usage", "上游最近 24 小时消耗或余额历史估算")
		result.SupplierPositions = append(result.SupplierPositions, position)
	}
	result.UpstreamBalance = upstreamBalance.metric(config, "supplier_balance", "所有启用上游的已知余额；快跑等折扣额度只折算一次")
	result.UpstreamDailySpend = upstreamSpend.metric(config, "supplier_usage", "所有启用上游的近 24 小时消耗或余额历史估算")
	result.OperatingGapEstimate, result.OperatingGapMargin = operatingGapEstimate(fullSiteRevenue, upstreamSpend, config)
	result.ProfitDataQuality.OperatingEstimateReady = strings.TrimSpace(result.OperatingGapEstimate.Amount) != ""
	sort.Slice(result.SupplierPositions, func(i, j int) bool {
		left, _ := decimal.NewFromString(result.SupplierPositions[i].DailySpend.Amount)
		right, _ := decimal.NewFromString(result.SupplierPositions[j].DailySpend.Amount)
		return left.GreaterThan(right)
	})
	result.Groups = unifiedGroupSummaries(today.Groups, config)
	if !result.TodayCost.Available {
		result.Warnings = append(result.Warnings, fmt.Sprintf("请求级采购成本尚未完整关联：%d/%d 条调用已具备成本。严格毛利保持未知；下方运营差额仅作估算，不会参与自动调价。",
			result.ProfitDataQuality.CostKnownEvents, result.ProfitDataQuality.IncludedUsageEvents))
	}
	if !result.UserBalance.Available {
		result.Warnings = append(result.Warnings, "本站用户剩余额度存在缺失或无限额度账号；页面显示已知部分并明确缺口。")
	}
	return result, nil
}

func unifiedGroupSummaries(items []store.LedgerDimensionSummary, config Config) []GroupOperatingSummary {
	type accumulator struct {
		events, revenueUnknown, costUnknown int
		requests                            int64
		revenue, cost                       unifiedMoneyAccumulator
	}
	groups := make(map[string]*accumulator)
	for _, item := range items {
		entry := groups[item.Label]
		if entry == nil {
			entry = &accumulator{}
			groups[item.Label] = entry
		}
		entry.events += item.EventCount
		entry.requests += item.Requests
		revenueKnown := item.RevenueKnownEvents
		costKnown := item.CostKnownEvents
		entry.revenue.add(item.KnownRevenue, item.Currency, revenueKnown, config)
		entry.cost.add(item.KnownCost, item.Currency, costKnown, config)
		entry.revenue.unknown(item.EventCount - revenueKnown)
		entry.cost.unknown(item.EventCount - costKnown)
	}
	result := make([]GroupOperatingSummary, 0, len(groups))
	for name, item := range groups {
		revenueMetric := item.revenue.metric(config, "ledger_observed", "今日该分组用户扣费")
		costMetric := item.cost.metric(config, "ledger_observed", "今日该分组请求级采购成本")
		profitMetric := UnifiedMoney{Currency: config.DisplayCurrency, Available: false, Evidence: "missing", Note: "收入或采购成本不完整"}
		marginMetric := UnifiedRatio{Available: false, Evidence: "missing", Note: "收入与采购成本完整后计算"}
		if revenueMetric.Available && costMetric.Available {
			profit := item.revenue.total.Sub(item.cost.total)
			profitMetric = UnifiedMoney{Amount: profit.Round(6).String(), Currency: config.DisplayCurrency, Available: true,
				KnownItems: item.events, Evidence: "ledger_matched"}
			if item.revenue.total.IsPositive() {
				marginMetric = UnifiedRatio{Value: profit.Div(item.revenue.total).Round(6).String(), Available: true, Evidence: "ledger_matched"}
			}
		}
		result = append(result, GroupOperatingSummary{Group: name, EventCount: item.events, Requests: item.requests,
			Revenue: revenueMetric, Cost: costMetric, GrossProfit: profitMetric, GrossMargin: marginMetric,
			RevenueCoverage: decimal.NewFromInt(int64(item.events - item.revenue.unknownItems)).Div(decimal.NewFromInt(int64(maxInt(item.events, 1)))).Round(6).String(),
			CostCoverage:    decimal.NewFromInt(int64(item.events - item.cost.unknownItems)).Div(decimal.NewFromInt(int64(maxInt(item.events, 1)))).Round(6).String()})
	}
	sort.Slice(result, func(i, j int) bool {
		left, _ := decimal.NewFromString(result[i].Revenue.Amount)
		right, _ := decimal.NewFromString(result[j].Revenue.Amount)
		if !left.Equal(right) {
			return left.GreaterThan(right)
		}
		return result[i].Requests > result[j].Requests
	})
	return result
}

func buildProfitDataQuality(today store.LedgerFinancialSummary, strictAvailable bool) ProfitDataQuality {
	total := today.IncludedUsageEventCount
	revenueKnown := knownUsageRevenueEvents(today)
	costKnown := knownUsageCostEvents(today)
	result := ProfitDataQuality{WindowStart: today.WindowStart, WindowEnd: today.WindowEnd, IncludedUsageEvents: total,
		RevenueKnownEvents: revenueKnown, RevenueUnknownEvents: maxInt(total-revenueKnown, 0),
		CostKnownEvents: costKnown, CostUnknownEvents: maxInt(total-costKnown, 0),
		UnknownCurrencyEvents: today.UnknownUsageCurrencyEvents, RevenueCoverage: integerCoverage(revenueKnown, total),
		CostCoverage: integerCoverage(costKnown, total), LedgerCoverageStatus: today.Coverage.Status,
		StrictProfitAvailable: strictAvailable, RequiredLedgerFields: []string{}, Blockers: []string{},
		OperatingEstimateBasis: "本站今日全站扣费（包含管理员）- 所有启用上游近 24 小时消耗；上游消耗已应用充值折算系数且不含充值流水",
	}
	if total == 0 {
		result.Blockers = append(result.Blockers, "当前业务日没有纳入经营口径的 usage 调用事件")
	}
	if result.RevenueUnknownEvents > 0 {
		result.RequiredLedgerFields = append(result.RequiredLedgerFields, "cash_revenue")
		result.Blockers = append(result.Blockers, fmt.Sprintf("%d 条调用缺少可换算的用户扣费", result.RevenueUnknownEvents))
	}
	if result.CostUnknownEvents > 0 {
		result.RequiredLedgerFields = append(result.RequiredLedgerFields, "procurement_cost")
		result.Blockers = append(result.Blockers, fmt.Sprintf("%d 条调用缺少请求级采购成本", result.CostUnknownEvents))
	}
	if result.UnknownCurrencyEvents > 0 {
		result.RequiredLedgerFields = append(result.RequiredLedgerFields, "currency")
		result.Blockers = append(result.Blockers, fmt.Sprintf("%d 条调用缺少币种，不能进入统一金额", result.UnknownCurrencyEvents))
	}
	result.RequiredLedgerFields = normalizeStringSet(result.RequiredLedgerFields)
	if today.Coverage.Status != "verified" {
		result.Blockers = append(result.Blockers, "账本来源窗口尚未提供完整性水位；当前数值只代表已导入事件")
	}
	switch {
	case total == 0:
		result.NextAction = "在站点接入中启用本站经营账本同步并执行一次立即同步；充值数据不会进入毛利公式。"
	case result.CostUnknownEvents > 0:
		result.NextAction = "无需录入充值，也无需管理员逐条填写采购成本。系统会优先按分组镜像和上游模型目录自动回填；仍缺历史证据时，今日毛利用已匹配请求的加权成本率估算并明确标注，且不参与自动调价。"
	case result.RevenueUnknownEvents > 0 || result.UnknownCurrencyEvents > 0:
		result.NextAction = "检查本站 quota 现金因子、币种和调用日志同步状态，然后执行一次立即同步。"
	case today.Coverage.Status != "verified":
		result.NextAction = "金额字段已齐，但源站未提供完整性水位；可先作为已导入账本结果使用，不能宣称覆盖全部调用。"
	default:
		result.NextAction = "严格毛利字段已齐，无需管理员处理。"
	}
	return result
}

func attachCostResolutionDiagnostics(quality *ProfitDataQuality, statuses []store.TargetLedgerCostResolutionStatus, targets []domain.TargetSite) {
	if quality == nil {
		return
	}
	targetNames := make(map[int64]string, len(targets))
	enabledTargets := make(map[int64]bool, len(targets))
	for _, target := range targets {
		targetNames[target.ID] = target.Name
		enabledTargets[target.ID] = target.Enabled
	}
	for _, status := range statuses {
		if !enabledTargets[status.TargetSiteID] {
			continue
		}
		quality.CostResolutionScannedEvents += status.ScannedEvents
		quality.CostResolutionResolvedEvents += status.ResolvedEvents
		quality.CostResolutionUnresolvedEvents += status.UnresolvedEvents
		quality.CostResolutionTruncated = quality.CostResolutionTruncated || status.Truncated
		if quality.CostResolutionWindowStart == nil || status.WindowStart.Before(*quality.CostResolutionWindowStart) {
			value := status.WindowStart
			quality.CostResolutionWindowStart = &value
		}
		if quality.CostResolutionWindowEnd == nil || status.WindowEnd.After(*quality.CostResolutionWindowEnd) {
			value := status.WindowEnd
			quality.CostResolutionWindowEnd = &value
		}
		if quality.CostResolutionObservedAt == nil || status.ObservedAt.After(*quality.CostResolutionObservedAt) {
			value := status.ObservedAt
			quality.CostResolutionObservedAt = &value
		}
		for _, reason := range status.Reasons {
			quality.CostResolutionReasons = append(quality.CostResolutionReasons, CostResolutionReason{TargetSiteID: status.TargetSiteID,
				TargetName: targetNames[status.TargetSiteID], Code: reason.Code, Message: reason.Message, Count: reason.Count, Samples: reason.Samples})
		}
	}
	sort.Slice(quality.CostResolutionReasons, func(i, j int) bool {
		if quality.CostResolutionReasons[i].Count != quality.CostResolutionReasons[j].Count {
			return quality.CostResolutionReasons[i].Count > quality.CostResolutionReasons[j].Count
		}
		return quality.CostResolutionReasons[i].Code < quality.CostResolutionReasons[j].Code
	})
	for index, reason := range quality.CostResolutionReasons {
		if index >= 3 {
			break
		}
		detail := costResolutionReasonLabel(reason.Code)
		if sample := costResolutionSampleLabel(reason.Samples); sample != "" {
			detail += "（例如 " + sample + "）"
		}
		quality.Blockers = append(quality.Blockers, fmt.Sprintf("%s：%d 条，%s", firstNonEmptyString(reason.TargetName, "本站"), reason.Count, detail))
	}
	if quality.CostResolutionTruncated {
		quality.Blockers = append(quality.Blockers, "成本回填待处理事件超过单轮安全上限，系统会在后续同步中继续处理")
	}
	if quality.CostResolutionUnresolvedEvents > 0 {
		quality.NextAction = "系统会在上游快照和本站账本更新后自动重试；下方已列出最大的渠道/规则阻塞样本。无需录入上游充值，只有映射冲突或缺失时才需要调整规则。"
	}
}

func costResolutionReasonLabel(code string) string {
	labels := map[string]string{
		"channel_id_missing": "调用日志缺少实际渠道 ID", "channel_readback_missing": "实际渠道已不存在或尚未读回",
		"channel_policy_binding_missing": "渠道没有唯一可证明的上游分组映射", "channel_policy_binding_conflict": "渠道关联了互相冲突的上游映射",
		"supplier_snapshot_missing": "上游价格快照缺失", "supplier_snapshot_missing_observed_at": "上游价格快照缺少采集时间",
		"supplier_snapshot_stale": "上游价格快照已过期", "source_group_missing": "规则中的上游分组不在最新快照中",
		"model_not_in_source_group": "调用模型不属于规则指定的上游分组", "model_outside_policy_pattern": "调用模型不符合规则模型范围",
		"positive_request_revenue_missing": "调用缺少正数用户扣费，不能按倍率反推成本", "request_currency_conflict": "用户扣费与采购成本币种冲突",
		"target_group_price_missing": "本站分组不在最新定价目录中", "target_group_ratio_invalid": "本站分组倍率缺失或无效",
		"source_group_ratio_invalid": "上游真实倍率缺失或无效", "policy_equivalent_cost_unavailable": "等价成本缺少可比模型价格",
		"policy_equivalent_cost_coverage_insufficient": "等价成本价格覆盖未达到规则门槛", "cost_resolver_unavailable": "暂时无法读取渠道或定价证据",
		"target_cost_adapter_missing": "本站只读渠道适配器未启用", "cost_persistence_failed": "成本已算出但原子写入失败",
	}
	if label := labels[code]; label != "" {
		return label
	}
	return firstNonEmptyString(code, "采购成本解析失败")
}

func costResolutionSampleLabel(samples []store.TargetLedgerCostResolutionSample) string {
	if len(samples) == 0 {
		return ""
	}
	sample := samples[0]
	parts := make([]string, 0, 4)
	if value := firstNonEmptyString(sample.ChannelName, sample.ChannelID); value != "" {
		parts = append(parts, "渠道 "+value)
	}
	if sample.TargetGroup != "" {
		parts = append(parts, "本站分组 "+sample.TargetGroup)
	}
	if value := firstNonEmptyString(sample.SupplierName, sample.SupplierID); value != "" {
		parts = append(parts, "上游 "+value)
	}
	if sample.SourceGroup != "" {
		parts = append(parts, "上游分组 "+sample.SourceGroup)
	}
	return strings.Join(parts, " / ")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func operatingGapEstimate(revenue, upstreamSpend unifiedMoneyAccumulator, config Config) (UnifiedMoney, UnifiedRatio) {
	result := UnifiedMoney{Currency: config.DisplayCurrency, Evidence: "missing",
		Note: "需要同时取得包含管理员的本站今日全站扣费和至少一个上游的近 24 小时消耗"}
	margin := UnifiedRatio{Evidence: "missing", Note: result.Note}
	if revenue.knownItems == 0 || upstreamSpend.knownItems == 0 {
		return result, margin
	}
	gap := revenue.total.Sub(upstreamSpend.total)
	unknown := revenue.unknownItems + upstreamSpend.unknownItems
	result = UnifiedMoney{Amount: gap.Round(6).String(), Currency: config.DisplayCurrency,
		Available: unknown == 0, KnownItems: revenue.knownItems + upstreamSpend.knownItems, UnknownItems: unknown,
		Evidence: "estimated", Note: "估算口径：本站今日全站扣费（包含管理员）- 启用上游近 24 小时消耗；只算消耗、不算充值，不参与自动调价"}
	if unknown > 0 {
		result.Evidence = "partial"
		result.Note += fmt.Sprintf("；当前另有 %d 项金额缺失，因此仅显示已知部分", unknown)
	}
	if revenue.total.IsPositive() {
		margin = UnifiedRatio{Value: gap.Div(revenue.total).Round(6).String(), Available: unknown == 0,
			Evidence: result.Evidence, Numerator: gap.Round(6).String(), Denominator: revenue.total.Round(6).String(),
			MatchedItems: revenue.knownItems, TotalItems: revenue.knownItems + revenue.unknownItems,
			Coverage: integerCoverage(revenue.knownItems, revenue.knownItems+revenue.unknownItems),
			Formula:  "(full_site_cash_revenue_including_administrators - upstream_24h_spend) / full_site_cash_revenue_including_administrators",
			Note:     "运营差额估算 / 包含管理员的已知本站全站扣费；时间窗和上游归属可能不完全一致"}
	}
	return result, margin
}

func knownUsageRevenueEvents(summary store.LedgerFinancialSummary) int {
	known := 0
	for _, item := range summary.ByCurrency {
		known += maxInt(item.UsageEventCount-item.UnknownUsageRevenueEvents, 0)
	}
	return known
}

func knownUsageCostEvents(summary store.LedgerFinancialSummary) int {
	known := 0
	for _, item := range summary.ByCurrency {
		known += maxInt(item.UsageEventCount-item.UnknownUsageCostEvents, 0)
	}
	return known
}

func strictProfitMissingNote(today store.LedgerFinancialSummary) string {
	if today.IncludedUsageEventCount == 0 {
		return "当前业务日没有纳入经营口径的调用，无法计算严格毛利"
	}
	return fmt.Sprintf("严格口径需要同窗用户扣费和请求级采购成本；当前收入覆盖 %s，成本覆盖 %s，缺失值不会按 0",
		integerCoverage(knownUsageRevenueEvents(today), today.IncludedUsageEventCount),
		integerCoverage(knownUsageCostEvents(today), today.IncludedUsageEventCount))
}

func integerCoverage(known, total int) string {
	if total <= 0 {
		return "0"
	}
	return decimal.NewFromInt(int64(known)).Div(decimal.NewFromInt(int64(total))).Round(6).String()
}

func adminScopeNote(excluded bool) string {
	if excluded {
		return "当前口径排除管理员、测试和内部调用，分子与分母使用同一口径"
	}
	return "当前口径包含管理员、测试和内部调用，分子与分母使用同一口径"
}

func weightedMarginMetric(revenue, cost unifiedMoneyAccumulator, totalItems int, adminExcluded bool) UnifiedRatio {
	formula := "(matched_cash_revenue - matched_procurement_cost) / matched_cash_revenue"
	coverage := integerCoverage(revenue.knownItems, totalItems)
	if revenue.knownItems == 0 || !revenue.total.IsPositive() || cost.knownItems != revenue.knownItems {
		return UnifiedRatio{Available: false, Evidence: "missing", MatchedItems: revenue.knownItems, TotalItems: totalItems,
			Coverage: coverage, Formula: formula, Note: "没有可验证的正数用户扣费和同一请求采购成本，无法计算加权平均利润率"}
	}
	profit := revenue.total.Sub(cost.total)
	evidence := "ledger_matched"
	if revenue.knownItems < totalItems {
		evidence = "ledger_matched_partial"
	}
	return UnifiedRatio{Value: profit.Div(revenue.total).Round(6).String(), Available: true, Evidence: evidence,
		Numerator: profit.Round(6).String(), Denominator: revenue.total.Round(6).String(), MatchedItems: revenue.knownItems,
		TotalItems: totalItems, Coverage: coverage, Formula: formula,
		Note: fmt.Sprintf("加权平均利润率；匹配 %d/%d 条调用，分子 %s - %s = %s，分母为已匹配用户扣费 %s；%s",
			revenue.knownItems, totalItems, revenue.total.Round(6).String(), cost.total.Round(6).String(), profit.Round(6).String(),
			revenue.total.Round(6).String(), adminScopeNote(adminExcluded))}
}

func estimatedGrossProfitMetric(revenue, matchedRevenue, matchedCost unifiedMoneyAccumulator, totalItems int,
	config Config,
) (UnifiedMoney, bool) {
	if revenue.unknownItems != 0 || !revenue.total.IsPositive() || matchedRevenue.knownItems == 0 ||
		matchedRevenue.knownItems != matchedCost.knownItems || matchedRevenue.unknownItems != 0 || matchedCost.unknownItems != 0 ||
		!matchedRevenue.total.IsPositive() {
		return UnifiedMoney{}, false
	}
	costRate := matchedCost.total.Div(matchedRevenue.total)
	estimatedCost := revenue.total.Mul(costRate)
	profit := revenue.total.Sub(estimatedCost)
	return UnifiedMoney{Amount: profit.Round(6).String(), Currency: config.DisplayCurrency, Available: true,
		KnownItems: matchedRevenue.knownItems, UnknownItems: maxInt(totalItems-matchedRevenue.knownItems, 0), Evidence: "estimated",
		Note: fmt.Sprintf("估算值：今日全部已知用户扣费按同窗已匹配 %d/%d 条调用的加权成本率外推；严格成本覆盖仍单独显示，不参与自动调价",
			matchedRevenue.knownItems, totalItems)}, true
}

func normalizedCashFactor(raw string) string {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return "1"
	}
	return value.String()
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func (s *Service) Dashboard(ctx context.Context, config Config) (Dashboard, error) {
	config = NormalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return Dashboard{}, err
	}
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return Dashboard{}, fmt.Errorf("encode intelligence config: %w", err)
	}
	configHash := sha256.Sum256(encodedConfig)
	key := hex.EncodeToString(configHash[:])

	for {
		sourceRevision, revisionErr := s.store.CurrentOperatingLedgerRevision(ctx)
		if revisionErr != nil {
			return Dashboard{}, revisionErr
		}
		now := time.Now()
		s.dashboardMu.Lock()
		matchingCache := s.dashboardCache.key == key && !s.dashboardCache.result.GeneratedAt.IsZero()
		if matchingCache && s.dashboardCache.result.CacheRevision == sourceRevision && now.Before(s.dashboardCache.expiresAt) {
			result := s.dashboardCache.result
			result.Refreshing = false
			result.SourceRevision = sourceRevision
			s.dashboardMu.Unlock()
			return result, nil
		}
		if matchingCache {
			result := s.dashboardCache.result
			result.Refreshing = true
			result.SourceRevision = sourceRevision
			result.CacheState = "stale"
			if s.dashboardFlight == nil {
				flight := &dashboardFlight{key: key, done: make(chan struct{})}
				s.dashboardFlight = flight
				go func() {
					background, cancel := context.WithTimeout(context.Background(), dashboardBackgroundTimeout)
					defer cancel()
					s.executeDashboardFlight(background, flight, config)
				}()
			}
			s.dashboardMu.Unlock()
			return result, nil
		}
		if flight := s.dashboardFlight; flight != nil {
			s.dashboardMu.Unlock()
			select {
			case <-ctx.Done():
				return Dashboard{}, ctx.Err()
			case <-flight.done:
				if flight.key == key {
					return flight.result, flight.err
				}
				continue
			}
		}
		flight := &dashboardFlight{key: key, done: make(chan struct{})}
		s.dashboardFlight = flight
		s.dashboardMu.Unlock()
		s.executeDashboardFlight(ctx, flight, config)
		return flight.result, flight.err
	}
}

// Overview returns only the current operating snapshot. It reuses the
// incremental ledger cache and deliberately avoids recommendation generation
// and the longer financial lookback used by the full intelligence dashboard.
func (s *Service) Overview(ctx context.Context, config Config) (OverviewSnapshot, error) {
	config = NormalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return OverviewSnapshot{}, err
	}
	cache, err := s.ledgerData(ctx)
	if err != nil {
		return OverviewSnapshot{}, err
	}
	now := time.Now().UTC()
	location, err := time.LoadLocation(config.BusinessTimeZone)
	if err != nil {
		return OverviewSnapshot{}, err
	}
	localNow := now.In(location)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location).UTC()
	operations, err := s.operatingSnapshotCached(ctx, config, now,
		ledgerEventsInWindow(cache.Events, dayStart, now), cache.Profiles)
	if err != nil {
		return OverviewSnapshot{}, err
	}
	return OverviewSnapshot{Operations: operations, RawEventCount: cache.Ledger.EventCount,
		CacheState: cache.State, CacheGenerated: cache.GeneratedAt, CacheRevision: cache.Revision,
		SourceRevision: cache.SourceRevision, GeneratedAt: now}, nil
}

func (s *Service) executeDashboardFlight(ctx context.Context, flight *dashboardFlight, config Config) {
	s.dashboardMu.Lock()
	var cachedRecommendations *recommendationCacheEntry
	if s.dashboardCache.key == flight.key && time.Now().Before(s.dashboardCache.recommendations.expiresAt) {
		copy := s.dashboardCache.recommendations
		cachedRecommendations = &copy
	}
	s.dashboardMu.Unlock()
	view, err := s.ledgerData(ctx)
	var recommendations recommendationCacheEntry
	if err == nil {
		flight.result, recommendations, err = s.dashboardUncached(ctx, config, view, cachedRecommendations)
	}
	flight.err = err
	s.dashboardMu.Lock()
	if flight.err == nil {
		s.dashboardCache = dashboardCacheEntry{key: flight.key, expiresAt: time.Now().Add(dashboardCacheTTL),
			result: flight.result, recommendations: recommendations}
	}
	if s.dashboardFlight == flight {
		s.dashboardFlight = nil
	}
	close(flight.done)
	s.dashboardMu.Unlock()
}

func (s *Service) dashboardUncached(ctx context.Context, config Config, cache ledgerCacheView,
	cachedRecommendations *recommendationCacheEntry) (Dashboard, recommendationCacheEntry, error) {
	ledger := cache.Ledger
	now := time.Now().UTC()
	financialEvents := ledgerEventsInWindow(cache.Events, now.Add(-time.Duration(config.FinancialLookbackDays)*24*time.Hour), now)
	financials, err := store.SummarizeOperatingFinancialEvents(financialEvents, store.LedgerFinancialReportOptions{
		Start: now.Add(-time.Duration(config.FinancialLookbackDays) * 24 * time.Hour), End: now, Exclusions: *config.LedgerExclusions})
	if err != nil {
		return Dashboard{}, recommendationCacheEntry{}, fmt.Errorf("summarize operating financials: %w", err)
	}
	location, err := time.LoadLocation(config.BusinessTimeZone)
	if err != nil {
		return Dashboard{}, recommendationCacheEntry{}, err
	}
	localNow := now.In(location)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location).UTC()
	operations, err := s.operatingSnapshotCached(ctx, config, now, ledgerEventsInWindow(cache.Events, dayStart, now), cache.Profiles)
	if err != nil {
		return Dashboard{}, recommendationCacheEntry{}, fmt.Errorf("build operating cash snapshot: %w", err)
	}
	var recommendationState recommendationCacheEntry
	if cachedRecommendations != nil {
		recommendationState = *cachedRecommendations
	} else {
		recommendationState, err = s.buildDashboardRecommendations(ctx, config, now)
		if err != nil {
			return Dashboard{}, recommendationCacheEntry{}, err
		}
	}
	items, calibration, dashboardWarnings := recommendationState.items, recommendationState.calibration, recommendationState.warnings
	result := Dashboard{Recommendations: items, Ledger: ledger, Financials: financials, Operations: operations, Calibration: calibration, Config: config, GeneratedAt: now,
		EvidenceLegend: evidenceLegend(), Warnings: dashboardWarnings, CacheState: cache.State, CacheGeneratedAt: cache.GeneratedAt,
		CacheRevision: cache.Revision, SourceRevision: cache.SourceRevision, Refreshing: false}
	result.Summary.Recommendations = len(items)
	result.Summary.LedgerEvents = int(ledger.EventCount)
	result.Summary.ExactRevenueEvents = int(ledger.ExactRevenueEvents)
	result.Summary.ExactCostEvents = int(ledger.ExactCostEvents)
	for _, item := range items {
		if item.Status == "pending" {
			result.Summary.Pending++
		}
		if item.Status == "applied" {
			result.Summary.Applied++
		}
		if item.ForecastReady {
			result.Summary.ForecastReady++
		}
	}
	if ledger.EventCount == 0 {
		result.Warnings = append(result.Warnings, "尚未接入请求级经营账本；当前情景使用观测用量和目录成本推算，不会冒充精确财务结果。")
	} else {
		result.Warnings = append(result.Warnings, "当前覆盖率只表示已导入账本行中的联合现金字段覆盖；尚未接入源站 completeness watermark，不能据此证明源站没有漏行。")
	}
	if !calibration.Ready {
		result.Warnings = append(result.Warnings, calibration.Note)
	}
	result.Warnings = append(result.Warnings, financials.Warnings...)
	return result, recommendationState, nil
}

func ledgerEventsInWindow(events []store.OperatingLedgerEvent, start, end time.Time) []store.OperatingLedgerEvent {
	first := sort.Search(len(events), func(index int) bool { return !events[index].OccurredAt.Before(start) })
	last := sort.Search(len(events), func(index int) bool { return !events[index].OccurredAt.Before(end) })
	return events[first:last]
}

func (s *Service) buildDashboardRecommendations(ctx context.Context, config Config, asOf time.Time) (recommendationCacheEntry, error) {
	proposals, err := s.store.ListProposals(ctx, "", 5000)
	if err != nil {
		return recommendationCacheEntry{}, err
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return recommendationCacheEntry{}, err
	}
	policyByID := make(map[int64]domain.PricingPolicy, len(policies))
	for _, policy := range policies {
		policyByID[policy.ID] = policy
	}
	items := make([]Recommendation, 0, len(proposals))
	dashboardWarnings := make([]string, 0)
	supplierByID := make(map[int64]domain.Supplier)
	targetByID := make(map[int64]domain.TargetSite)
	type targetUsage struct {
		items []store.OpportunityUsage
		err   error
	}
	usageByTarget := make(map[int64]targetUsage)
	for _, proposal := range proposals {
		policy, ok := policyByID[proposal.PolicyID]
		if !ok {
			continue
		}
		supplier, ok := supplierByID[policy.SupplierID]
		if !ok {
			supplier, _ = s.store.GetSupplier(ctx, policy.SupplierID)
			supplierByID[policy.SupplierID] = supplier
		}
		target, ok := targetByID[policy.TargetSiteID]
		if !ok {
			target, _ = s.store.GetTarget(ctx, policy.TargetSiteID)
			targetByID[policy.TargetSiteID] = target
		}
		usage, ok := usageByTarget[policy.TargetSiteID]
		if !ok {
			usage.items, usage.err = s.store.ListOpportunityUsage(ctx, policy.TargetSiteID, 45*24*time.Hour)
			if errors.Is(usage.err, sql.ErrNoRows) {
				usage.err = nil
			}
			usageByTarget[policy.TargetSiteID] = usage
		}
		var item Recommendation
		buildErr := usage.err
		if buildErr == nil {
			item, buildErr = s.buildRecommendationWithInputs(ctx, proposal, policy, config, asOf, supplier, target, usage.items)
		}
		if buildErr != nil {
			message := fmt.Sprintf("历史建议 #%d 的冻结证据无法完整读取：%v", proposal.ID, buildErr)
			item = Recommendation{ProposalID: proposal.ID, PolicyID: proposal.PolicyID, TargetSiteID: proposal.TargetSiteID,
				PolicyName: policy.Name, Supplier: supplier.Name, Target: target.Name, TargetGroup: proposal.TargetGroup,
				ModelPattern: policy.SourceModelPattern, Status: proposal.Status, LifecycleStatus: lifecycleStatus(proposal.Status),
				CurrentRatio: proposal.ExpectedOld, ProposedRatio: proposal.ProposedValue, CreatedAt: proposal.CreatedAt,
				ForecastReady: false, ForecastReason: "该历史记录的数据契约不完整，已隔离为只读审计记录。",
				Forecasts: []HorizonForecast{}, Evidence: []EvidenceItem{}, Warnings: []string{message},
				ModelVersion: ModelVersion, InputHash: "unavailable"}
			dashboardWarnings = append(dashboardWarnings, message)
		}
		normalizeRecommendation(&item)
		items = append(items, item)
	}
	calibration, err := s.calibration(ctx, config.MinimumCalibrationN)
	if err != nil {
		return recommendationCacheEntry{}, err
	}
	return recommendationCacheEntry{expiresAt: asOf.Add(dashboardCacheTTL), items: items, calibration: calibration, warnings: dashboardWarnings}, nil
}

// ReconcileDueOutcomes is the scheduler-facing path for result collection. It
// reconstructs only immutable decision-time forecasts and remote effect
// receipts, so outcome evidence no longer depends on an administrator opening
// the dashboard.
func (s *Service) ReconcileDueOutcomes(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	candidates, err := s.store.ListOutcomeReconcileCandidates(ctx, time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	processed := 0
	var reconcileErrors []error
	for _, proposal := range candidates {
		reconcileErr := s.reconcileProposalOutcomes(ctx, proposal)
		if attemptErr := s.store.RecordOutcomeReconcileAttempt(ctx, proposal.ID, time.Now().UTC(), reconcileErr); attemptErr != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("proposal %d record reconcile attempt: %w", proposal.ID, attemptErr))
		}
		if reconcileErr != nil {
			reconcileErrors = append(reconcileErrors, reconcileErr)
			continue
		}
		processed++
	}
	return processed, errors.Join(reconcileErrors...)
}

func (s *Service) reconcileProposalOutcomes(ctx context.Context, proposal domain.ChangeProposal) error {
	snapshot, err := s.store.GetDecisionSnapshot(ctx, proposal.ID)
	if err != nil {
		return fmt.Errorf("proposal %d snapshot: %w", proposal.ID, err)
	}
	item := Recommendation{ProposalID: proposal.ID, TargetSiteID: proposal.TargetSiteID, TargetGroup: proposal.TargetGroup,
		Status: proposal.Status, ModelVersion: snapshot.ModelVersion}
	if err := restoreFrozenRecommendation(&item, snapshot); err != nil {
		return fmt.Errorf("proposal %d frozen forecast: %w", proposal.ID, err)
	}
	if !item.ForecastReady || strings.TrimSpace(item.ModelPattern) == "" {
		return fmt.Errorf("proposal %d immutable forecast is not reconcileable", proposal.ID)
	}
	effect, err := s.store.GetDecisionEffect(ctx, proposal.ID)
	if err != nil {
		return fmt.Errorf("proposal %d effect: %w", proposal.ID, err)
	}
	item.EffectiveAt = timePointer(effect.EffectiveAt)
	if err := s.reconcileOutcomes(ctx, &item, item.ModelPattern); err != nil {
		return fmt.Errorf("proposal %d outcome: %w", proposal.ID, err)
	}
	return nil
}

func (s *Service) LoadConfig(ctx context.Context) (Config, error) {
	config := DefaultConfig()
	var stored Config
	if err := s.store.GetSetting(ctx, ConfigSettingKey, &stored); err == nil {
		config = NormalizeConfig(stored)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Config{}, err
	}
	return config, ValidateConfig(config)
}

func (s *Service) PrepareDecisionSnapshot(ctx context.Context, proposalID int64, decisionKind string, decidedAt time.Time) (store.DecisionSnapshot, error) {
	decisionKind = strings.ToLower(strings.TrimSpace(decisionKind))
	if decisionKind != "approve" && decisionKind != "reject" {
		return store.DecisionSnapshot{}, errors.New("decision kind must be approve or reject")
	}
	if existing, err := s.store.GetDecisionSnapshot(ctx, proposalID); err == nil {
		if existing.DecisionKind != decisionKind {
			return store.DecisionSnapshot{}, fmt.Errorf("proposal %d already has an immutable %s decision snapshot", proposalID, existing.DecisionKind)
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return store.DecisionSnapshot{}, err
	}
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return store.DecisionSnapshot{}, err
	}
	policy, err := s.store.GetPolicy(ctx, proposal.PolicyID)
	if err != nil {
		return store.DecisionSnapshot{}, err
	}
	config, err := s.LoadConfig(ctx)
	if err != nil {
		return store.DecisionSnapshot{}, err
	}
	working := proposal
	working.Status = "pending"
	item, err := s.buildRecommendation(ctx, working, policy, config, decidedAt.UTC())
	if err != nil {
		return store.DecisionSnapshot{}, err
	}
	item.Status = proposal.Status
	item.DecisionKind = decisionKind
	item.DecidedAt = timePointer(decidedAt.UTC())
	payload, err := json.Marshal(item)
	if err != nil {
		return store.DecisionSnapshot{}, err
	}
	lifecycle := "tracking"
	if decisionKind == "reject" {
		lifecycle = "rejected"
	}
	return store.DecisionSnapshot{ProposalID: proposalID, ModelVersion: item.ModelVersion, InputHash: item.InputHash,
		LifecycleStatus: lifecycle, DecisionKind: decisionKind, DecidedAt: decidedAt.UTC(), Payload: payload}, nil
}

func (s *Service) buildRecommendation(ctx context.Context, proposal domain.ChangeProposal, policy domain.PricingPolicy, config Config, asOf time.Time) (Recommendation, error) {
	supplier, _ := s.store.GetSupplier(ctx, policy.SupplierID)
	target, _ := s.store.GetTarget(ctx, policy.TargetSiteID)
	usage, usageErr := s.store.ListOpportunityUsage(ctx, policy.TargetSiteID, 45*24*time.Hour)
	if usageErr != nil && !errors.Is(usageErr, sql.ErrNoRows) {
		return Recommendation{}, usageErr
	}
	return s.buildRecommendationWithInputs(ctx, proposal, policy, config, asOf, supplier, target, usage)
}

func (s *Service) buildRecommendationWithInputs(ctx context.Context, proposal domain.ChangeProposal, policy domain.PricingPolicy,
	config Config, asOf time.Time, supplier domain.Supplier, target domain.TargetSite, usage []store.OpportunityUsage,
) (Recommendation, error) {
	usage = opportunityUsageAsOf(usage, asOf)
	profiles, demandLevel, demandAt := estimateDemandProfiles(usage, policy.SourceModelPattern)
	demand, _, _, matched := estimateDailyDemand(usage, policy.SourceModelPattern)
	item := Recommendation{ProposalID: proposal.ID, PolicyID: proposal.PolicyID, TargetSiteID: proposal.TargetSiteID, PolicyName: policy.Name,
		Supplier: supplier.Name, Target: target.Name, TargetGroup: proposal.TargetGroup, Status: proposal.Status,
		ModelPattern:    policy.SourceModelPattern,
		LifecycleStatus: lifecycleStatus(proposal.Status), CurrentRatio: proposal.ExpectedOld, ProposedRatio: proposal.ProposedValue,
		CreatedAt: proposal.CreatedAt, Forecasts: []HorizonForecast{}, Evidence: []EvidenceItem{}, Warnings: []string{}, ModelVersion: ModelVersion}
	if proposal.GroupModelPricing.Mode == "atomic_contract" {
		item.PricingMode = "group_model_contract"
		item.CurrentRevision = proposal.GroupModelPricing.ExpectedRevision
		item.ProposedRevision = proposal.GroupModelPricing.DesiredRevision
		item.ForecastTotalModels = len(proposal.GroupModelPricing.Models)
	}
	upstreamCost := proposal.Calculation.CostRatio
	if strings.TrimSpace(upstreamCost) == "" {
		upstreamCost = proposal.Calculation.SourceRatio
	}
	item.Evidence = append(item.Evidence,
		EvidenceItem{Key: "upstream_cost", Label: "上游成本", Level: "observed", Value: upstreamCost,
			Unit: "等效倍率", Source: "上游目录与模型价格函数", ObservedAt: proposal.UpdatedAt, Note: "来自当前目录证据，不等同于已出账现金成本。"},
		EvidenceItem{Key: "target_ratio", Label: "本站倍率", Level: "exact", Value: proposal.ExpectedOld,
			Unit: "倍率", Source: "目标站写前读回", ObservedAt: proposal.CreatedAt, Note: "目标站配置读数；实际用户收入仍需请求级扣费账本。"})
	if demandLevel == "missing" {
		item.Evidence = append(item.Evidence, EvidenceItem{Key: "demand", Label: "模型需求", Level: "missing", Source: "NewAPI 模型用量",
			Note: "没有匹配该规则模型范围的 7/30 天用量，系统拒绝生成伪精确收益数字。"})
		item.ForecastReason = "缺少匹配模型的 7/30 天真实用量"
	} else {
		item.Evidence = append(item.Evidence, EvidenceItem{Key: "demand", Label: "模型需求", Level: demandLevel, Value: demand.String(),
			Unit: "归一化需求单位/日", Source: "NewAPI 7/30 天模型用量", ObservedAt: demandAt,
			Note: fmt.Sprintf("匹配 %d 个模型；这是需求权重，不是现金金额。", matched)})
	}
	item.Evidence = append(item.Evidence,
		EvidenceItem{Key: "cost", Label: "采购成本", Level: "estimated", Value: proposal.Calculation.RiskAdjustedCost,
			Unit: "风险调整单位成本", Source: "目录成本模型", ObservedAt: proposal.UpdatedAt,
			Note: "按本建议对应的上游目录、现金折算因子与风险缓冲推算；其它模型或分组的精确成本事件不会提升本项证据等级。"},
		EvidenceItem{Key: "elasticity", Label: "价格弹性", Level: "prior", Value: config.PriceElasticity,
			Unit: "弹性系数", Source: "可配置保守先验", Note: "有足够自然价格变更样本并通过校准前，不把它宣称为本站已学习参数。"})

	learningDigest := ""
	parsed, parseErr := parseForecastInputs(proposal, policy, config)
	if parseErr == nil && proposal.GroupModelPricing.Mode == "atomic_contract" {
		var rates map[string]cashUnitRate
		var currency, coverage string
		var covered, total int
		rates, currency, covered, total, coverage, parseErr = groupModelForecastRates(proposal, policy, profiles)
		item.ForecastCoveredModels = covered
		item.ForecastTotalModels = total
		item.ForecastCoverage = coverage
		if parseErr == nil {
			parsed.metricBasis = contractCashContributionBasis
			parsed.metricUnit = currency
			parsed.cashRates = rates
			item.Evidence = append(item.Evidence, EvidenceItem{Key: "group_model_contract", Label: "Per-model price contract",
				Level: "exact", Value: coverage, Unit: "forecast model coverage", Source: "revision-checked target contract and supplier cost plan",
				ObservedAt: proposal.UpdatedAt, Note: "Forecasts use each model's current sale, proposed sale, procurement cost, and billing unit; revisions are never treated as numeric prices."})
		}
	}
	if parseErr != nil {
		item.ForecastReason = parseErr.Error()
		item.Warnings = append(item.Warnings, parseErr.Error())
	} else if demandLevel != "missing" {
		if config.ForecastCurrency != "" && !isCashContributionBasis(parsed.metricBasis) {
			rates, coverage, cashErr := s.resolveCashRates(ctx, proposal.TargetSiteID, proposal.TargetGroup, profiles, config, asOf)
			if cashErr == nil {
				parsed.metricBasis, parsed.metricUnit, parsed.cashRates = cashContributionBasis, config.ForecastCurrency, rates
				item.Evidence = append(item.Evidence, EvidenceItem{Key: "cash_conversion", Label: "现金换算", Level: "observed",
					Value: canonical(coverage), Unit: "已导入账本内覆盖率", Source: "同模型同计量单位经营账本", ObservedAt: asOf,
					Note: "收入和采购成本由已导入的同币种、同模型、同计量单位账本推导；未接入源站完整性水位时不证明无漏行。"})
			} else {
				item.Warnings = append(item.Warnings, "现金预测未启用："+cashErr.Error()+"；当前继续显示无币种的倍率加权等价贡献。")
			}
		}
		item.Forecasts, parseErr = forecastsForProfiles(proposal, profiles, demandLevel, parsed, config)
		if parseErr != nil {
			item.ForecastReason = parseErr.Error()
			item.Warnings = append(item.Warnings, parseErr.Error())
		} else {
			item.ForecastReady = true
			for index := range item.Forecasts {
				item.Forecasts[index].LearningScope = learningScopeKey(item.TargetSiteID, item.TargetGroup, item.ModelPattern,
					item.Forecasts[index].DemandUnits)
			}
			if isCashContributionBasis(parsed.metricBasis) {
				learningDigest, parseErr = s.applyOutcomeLearning(ctx, item.Forecasts, config.MinimumCalibrationN, asOf)
				if parseErr != nil {
					item.Warnings = append(item.Warnings, "历史结果自适应层暂不可用："+parseErr.Error()+"；当前保留结构预测和先验区间。")
				}
			}
		}
	}
	item.InputHash = recommendationHash(proposal, policy, config, usage)
	if learningDigest != "" {
		item.InputHash = combineHashes(item.InputHash, learningDigest)
	}
	if isDecisionTerminal(proposal.Status) {
		frozen, snapshotErr := s.store.GetDecisionSnapshot(ctx, proposal.ID)
		if snapshotErr == nil {
			if err := restoreFrozenRecommendation(&item, frozen); err != nil {
				return Recommendation{}, fmt.Errorf("restore decision snapshot for proposal %d: %w", proposal.ID, err)
			}
			item.DecisionKind = frozen.DecisionKind
			item.DecidedAt = timePointer(frozen.DecidedAt)
		} else if !errors.Is(snapshotErr, sql.ErrNoRows) {
			return Recommendation{}, snapshotErr
		} else {
			item.ForecastReady = false
			item.ForecastReason = "该历史变更没有决策时点不可变快照，系统不会用事后数据重建预测。"
			item.Forecasts = []HorizonForecast{}
			item.Warnings = append(item.Warnings, item.ForecastReason)
		}
		if effect, effectErr := s.store.GetDecisionEffect(ctx, proposal.ID); effectErr == nil {
			item.EffectiveAt = timePointer(effect.EffectiveAt)
		} else if !errors.Is(effectErr, sql.ErrNoRows) {
			return Recommendation{}, effectErr
		}
		for index := range item.Forecasts {
			if item.Forecasts[index].OutcomeStatus != "observed" {
				item.Forecasts[index].OutcomeStatus = outcomeStatus(proposal.Status, item.Forecasts[index].Hours)
				item.Forecasts[index].OutcomeNote = outcomeNote(proposal.Status, item.Forecasts[index].Hours)
			}
		}
	}
	if (proposal.Status == "applied" || proposal.Status == "rolled_back") && item.ForecastReady && item.EffectiveAt != nil {
		if strings.TrimSpace(item.ModelPattern) == "" {
			item.Warnings = append(item.Warnings, "决策快照没有冻结模型范围，系统不会借用当前可变策略核对历史结果。")
		}
		if err := s.hydrateOutcomeViews(ctx, &item); err != nil {
			item.Warnings = append(item.Warnings, "结果窗口状态暂不可读："+err.Error())
		}
	}
	normalizeRecommendation(&item)
	return item, nil
}

func normalizeRecommendation(item *Recommendation) {
	if item.Forecasts == nil {
		item.Forecasts = []HorizonForecast{}
	}
	if item.Evidence == nil {
		item.Evidence = []EvidenceItem{}
	}
	if item.Warnings == nil {
		item.Warnings = []string{}
	}
	if strings.TrimSpace(item.ModelVersion) == "" {
		item.ModelVersion = ModelVersion
	}
	if strings.TrimSpace(item.InputHash) == "" {
		item.InputHash = "unavailable"
	}
}

func restoreFrozenRecommendation(item *Recommendation, snapshot store.DecisionSnapshot) error {
	var frozen Recommendation
	if err := json.Unmarshal(snapshot.Payload, &frozen); err != nil {
		return err
	}
	if frozen.ProposalID != item.ProposalID || strings.TrimSpace(snapshot.InputHash) == "" || strings.TrimSpace(snapshot.ModelVersion) == "" {
		return errors.New("decision snapshot identity is incomplete")
	}
	if frozen.InputHash != "" && frozen.InputHash != snapshot.InputHash {
		return errors.New("decision snapshot payload input hash conflicts with its immutable envelope")
	}
	if frozen.ModelVersion != "" && frozen.ModelVersion != snapshot.ModelVersion {
		return errors.New("decision snapshot payload model version conflicts with its immutable envelope")
	}
	if frozen.DecisionKind != "" && snapshot.DecisionKind != "" && frozen.DecisionKind != snapshot.DecisionKind {
		return errors.New("decision snapshot payload decision kind conflicts with its immutable envelope")
	}
	item.ForecastReady = frozen.ForecastReady
	item.ForecastReason = frozen.ForecastReason
	item.Forecasts = frozen.Forecasts
	item.Evidence = frozen.Evidence
	item.Warnings = frozen.Warnings
	item.ModelPattern = frozen.ModelPattern
	item.ModelVersion = snapshot.ModelVersion
	item.InputHash = snapshot.InputHash
	item.DecidedAt = timePointer(snapshot.DecidedAt)
	for index := range item.Forecasts {
		normalizeLegacyForecast(&item.Forecasts[index], snapshot.ModelVersion != ModelVersion)
		if item.Forecasts[index].MetricBasis == "" {
			item.Forecasts[index].MetricBasis = equivalentContributionBasis
		}
		if snapshot.ModelVersion != ModelVersion {
			item.Forecasts[index].Observation = nil
			if item.Forecasts[index].OutcomeStatus == "observed" {
				item.Forecasts[index].OutcomeStatus = "not_due"
			}
		}
	}
	return nil
}

func normalizeLegacyForecast(forecast *HorizonForecast, incompatible bool) {
	for _, interval := range []*Interval{&forecast.Approve.Demand, &forecast.Approve.Revenue, &forecast.Approve.Cost,
		&forecast.Approve.Contribution, &forecast.Reject.Demand, &forecast.Reject.Revenue, &forecast.Reject.Cost,
		&forecast.Reject.Contribution, &forecast.DeltaContribution} {
		if interval.Low == "" {
			interval.Low = interval.P10
		}
		if interval.Base == "" {
			interval.Base = interval.P50
		}
		if interval.High == "" {
			interval.High = interval.P90
		}
	}
	if incompatible {
		forecast.AdaptiveContribution = nil
		forecast.Learning = nil
		forecast.Calibrated = false
		forecast.IntervalMethod = "legacy_relative_sensitivity_band"
	} else if forecast.IntervalMethod == "" {
		forecast.IntervalMethod = "legacy_relative_sensitivity_band"
	}
}

func timePointer(value time.Time) *time.Time { return &value }

type forecastInputs struct {
	currentRatio, proposedRatio, revenueFactor, riskCost, elasticity decimal.Decimal
	priorUncertainty, observedUncertainty                            decimal.Decimal
	metricBasis, metricUnit                                          string
	cashRates                                                        map[string]cashUnitRate
}

type cashUnitRate struct {
	revenue          decimal.Decimal
	proposedRevenue  decimal.Decimal
	cost             decimal.Decimal
	priceScale       decimal.Decimal
	explicitProposal bool
}

type demandProfile struct {
	modelID    string
	unit       string
	daily      decimal.Decimal
	level      string
	observedAt time.Time
}

func parseForecastInputs(proposal domain.ChangeProposal, policy domain.PricingPolicy, config Config) (forecastInputs, error) {
	parse := func(raw, name string) (decimal.Decimal, error) {
		value, err := decimal.NewFromString(strings.TrimSpace(raw))
		if err != nil {
			return decimal.Zero, fmt.Errorf("%s 不是有效数字", name)
		}
		return value, nil
	}
	current, err := parse(proposal.ExpectedOld, "当前倍率")
	if err != nil || !current.IsPositive() {
		return forecastInputs{}, errors.New("目标分组没有可比较的当前倍率；可执行上架预览，但暂不生成价格弹性预测")
	}
	proposed, err := parse(proposal.ProposedValue, "建议倍率")
	if err != nil || !proposed.IsPositive() {
		return forecastInputs{}, errors.New("建议倍率必须大于零；零倍率下常弹性模型无有限解，暂不生成场景数字")
	}
	revenueFactor, err := parse(defaultValue(policy.RevenueCashFactor, "1"), "收入现金因子")
	if err != nil || !revenueFactor.IsPositive() {
		return forecastInputs{}, errors.New("收入现金因子必须大于零")
	}
	riskCost, err := parse(proposal.Calculation.RiskAdjustedCost, "风险调整成本")
	if err != nil || riskCost.IsNegative() {
		return forecastInputs{}, errors.New("风险调整成本缺失或无效")
	}
	elasticity, _ := decimal.NewFromString(config.PriceElasticity)
	priorUncertainty, _ := decimal.NewFromString(config.PriorUncertainty)
	observedUncertainty, _ := decimal.NewFromString(config.ObservedUncertainty)
	return forecastInputs{currentRatio: current, proposedRatio: proposed, revenueFactor: revenueFactor, riskCost: riskCost,
		elasticity: elasticity, priorUncertainty: priorUncertainty, observedUncertainty: observedUncertainty,
		metricBasis: equivalentContributionBasis, metricUnit: equivalentContributionUnit}, nil
}

func groupModelForecastRates(proposal domain.ChangeProposal, policy domain.PricingPolicy,
	profiles []demandProfile) (map[string]cashUnitRate, string, int, int, string, error) {
	plan := proposal.GroupModelPricing
	total := len(plan.Models)
	coverage := func(covered int) string {
		if total == 0 {
			return "0/0"
		}
		return fmt.Sprintf("%d/%d", covered, total)
	}
	if plan.Mode != "atomic_contract" || !plan.Exact || total == 0 {
		return nil, "", 0, total, coverage(0), errors.New("group-model forecast has no complete atomic price plan")
	}
	profileByModel := make(map[string]demandProfile, len(profiles))
	for _, profile := range profiles {
		profileByModel[strings.ToLower(strings.TrimSpace(profile.modelID))] = profile
	}
	revenueFactor, err := decimal.NewFromString(defaultValue(policy.RevenueCashFactor, "1"))
	if err != nil || !revenueFactor.IsPositive() {
		return nil, "", 0, total, coverage(0), errors.New("revenue cash factor must be a positive decimal")
	}
	rates := make(map[string]cashUnitRate, total)
	currency := ""
	covered := 0
	problems := make([]string, 0)
	for _, model := range plan.Models {
		key := strings.ToLower(strings.TrimSpace(model.Model))
		profile, ok := profileByModel[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("model %s has no 7/30-day demand observation", model.Model))
			continue
		}
		if len(model.Dimensions) != 1 {
			problems = append(problems, fmt.Sprintf("model %s has %d monetary dimensions; dimension-level usage is required for an exact forecast",
				model.Model, len(model.Dimensions)))
			continue
		}
		dimension := model.Dimensions[0]
		dimensionName := dimension.Dimension
		if dimensionName == "fixed_fee" {
			dimensionName = "per_request"
		}
		if !groupModelDemandUnitMatches(profile.unit, model.BillingUnit, dimensionName) {
			problems = append(problems, fmt.Sprintf("model %s demand unit %s does not match billing unit %s",
				model.Model, profile.unit, model.BillingUnit))
			continue
		}
		currentSetting, ok := groupModelSetting(plan.ExpectedContract, plan.TargetGroup, model.Model)
		if !ok {
			problems = append(problems, fmt.Sprintf("model %s has no previous group-model price for the reject scenario", model.Model))
			continue
		}
		currentDimensions, dimensionErr := listing.EffectiveContractDimensions(currentSetting, plan.TargetGroupRatio)
		if dimensionErr != nil {
			problems = append(problems, fmt.Sprintf("model %s previous price is not comparable: %v", model.Model, dimensionErr))
			continue
		}
		currentRaw, ok := currentDimensions[dimensionName]
		if !ok || len(currentDimensions) != 1 {
			problems = append(problems, fmt.Sprintf("model %s previous price vector does not match dimension %s", model.Model, dimensionName))
			continue
		}
		currentSale, currentErr := decimal.NewFromString(currentRaw)
		proposedSale, proposedErr := decimal.NewFromString(dimension.EffectiveSale)
		proposedRevenue, revenueErr := decimal.NewFromString(dimension.RevenueCashValue)
		cost, costErr := decimal.NewFromString(dimension.RiskAdjustedCashCost)
		if currentErr != nil || proposedErr != nil || revenueErr != nil || costErr != nil ||
			currentSale.IsNegative() || proposedSale.IsNegative() || proposedRevenue.IsNegative() || cost.IsNegative() {
			problems = append(problems, fmt.Sprintf("model %s has an invalid current, proposed, or procurement price", model.Model))
			continue
		}
		priceScale := decimal.NewFromInt(1)
		if currentSale.IsZero() {
			if !proposedSale.IsZero() {
				problems = append(problems, fmt.Sprintf("model %s changes from a zero price; elasticity has no finite price ratio", model.Model))
				continue
			}
		} else {
			priceScale = proposedSale.Div(currentSale)
		}
		modelCurrency := strings.ToUpper(strings.TrimSpace(model.Currency))
		if modelCurrency == "" {
			problems = append(problems, fmt.Sprintf("model %s has no declared forecast currency", model.Model))
			continue
		}
		if currency == "" {
			currency = modelCurrency
		} else if currency != modelCurrency {
			problems = append(problems, fmt.Sprintf("model %s uses currency %s while the plan uses %s", model.Model, modelCurrency, currency))
			continue
		}
		rates[key] = cashUnitRate{revenue: currentSale.Mul(revenueFactor), proposedRevenue: proposedRevenue,
			cost: cost, priceScale: priceScale, explicitProposal: true}
		covered++
	}
	if covered != total || len(problems) > 0 {
		return nil, currency, covered, total, coverage(covered), fmt.Errorf("group-model forecast coverage %s is insufficient: %s",
			coverage(covered), strings.Join(problems, "; "))
	}
	return rates, currency, covered, total, coverage(covered), nil
}

func groupModelSetting(contract domain.GroupModelPricingContract, group, model string) (domain.GroupModelPricingSetting, bool) {
	models, ok := contract.Groups[group]
	if !ok {
		for name, candidate := range contract.Groups {
			if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(group)) {
				models, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		return domain.GroupModelPricingSetting{}, false
	}
	if setting, exists := models[model]; exists {
		return setting, true
	}
	for name, setting := range models {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(model)) {
			return setting, true
		}
	}
	return domain.GroupModelPricingSetting{}, false
}

func groupModelDemandUnitMatches(demandUnit, billingUnit, dimension string) bool {
	demandUnit = strings.ToLower(strings.TrimSpace(demandUnit))
	billingUnit = strings.ToLower(strings.TrimSpace(billingUnit))
	switch billingUnit {
	case "per_request", "fixed_duration":
		return (demandUnit == "request" || demandUnit == "requests" || demandUnit == "per_request") && dimension == "per_request"
	case "per_second":
		return (demandUnit == "second" || demandUnit == "seconds" || demandUnit == "per_second") && dimension == "per_second"
	case "token":
		return demandUnit == "million_tokens" && strings.HasSuffix(dimension, "_per_1m")
	default:
		return false
	}
}

func isCashContributionBasis(value string) bool {
	return value == cashContributionBasis || value == contractCashContributionBasis
}

func forecasts(proposal domain.ChangeProposal, dailyDemand decimal.Decimal, demandLevel string, input forecastInputs) ([]HorizonForecast, error) {
	config := DefaultConfig()
	return forecastsForProfiles(proposal, []demandProfile{{modelID: "aggregate", unit: "normalized_demand", daily: dailyDemand,
		level: demandLevel}}, demandLevel, input, config)
}

func forecastsForProfiles(proposal domain.ChangeProposal, profiles []demandProfile, demandLevel string, input forecastInputs, config Config) ([]HorizonForecast, error) {
	if len(profiles) == 0 {
		return nil, errors.New("没有可预测的模型需求")
	}
	uncertainty := input.observedUncertainty
	if demandLevel != "observed" {
		uncertainty = input.priorUncertainty
	}
	defaultPriceScale := input.proposedRatio.Div(input.currentRatio)
	demandUnits := make(map[string]string, len(profiles))
	commonUnit := ""
	for _, profile := range profiles {
		demandUnits[profile.modelID] = profile.unit
		if commonUnit == "" {
			commonUnit = profile.unit
		} else if commonUnit != profile.unit {
			commonUnit = "mixed"
		}
	}
	if !isCashContributionBasis(input.metricBasis) && commonUnit == "mixed" {
		return nil, errors.New("匹配模型同时使用多种需求计量单位，不能把 token、请求或媒体计量直接相加；请缩小模型范围或补齐现金账本换算")
	}
	coverage := config.MinimumLedgerCoverage
	if coverage == "" {
		coverage = DefaultConfig().MinimumLedgerCoverage
	}
	errorFloor := config.RelativeErrorFloor
	if errorFloor == "" {
		errorFloor = DefaultConfig().RelativeErrorFloor
	}
	result := make([]HorizonForecast, 0, 3)
	exclusions := *config.LedgerExclusions
	for _, horizon := range []struct {
		hours int
		label string
	}{{1, "1 小时"}, {24, "24 小时"}, {MaximumForecastHorizonHours, "7 天"}} {
		days := decimal.NewFromInt(int64(horizon.hours)).Div(decimal.NewFromInt(24))
		approveDemand, rejectDemand := decimal.Zero, decimal.Zero
		approveRevenue, rejectRevenue, approveCost, rejectCost := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
		for _, profile := range profiles {
			priceScale := defaultPriceScale
			rate, hasRate := input.cashRates[strings.ToLower(profile.modelID)]
			if hasRate && rate.explicitProposal {
				priceScale = rate.priceScale
			}
			demandFactor, factorErr := elasticityDemandFactor(priceScale, input.elasticity)
			if factorErr != nil {
				return nil, fmt.Errorf("model %s: %w", profile.modelID, factorErr)
			}
			rejectQuantity := profile.daily.Mul(days)
			approveQuantity := profile.daily.Mul(demandFactor).Mul(days)
			approveDemand, rejectDemand = approveDemand.Add(approveQuantity), rejectDemand.Add(rejectQuantity)
			if isCashContributionBasis(input.metricBasis) {
				if !hasRate {
					return nil, fmt.Errorf("模型 %s 缺少同计量单位现金换算", profile.modelID)
				}
				approveUnitRevenue := rate.revenue.Mul(priceScale)
				if rate.explicitProposal {
					approveUnitRevenue = rate.proposedRevenue
				}
				approveRevenue = approveRevenue.Add(approveQuantity.Mul(approveUnitRevenue))
				rejectRevenue = rejectRevenue.Add(rejectQuantity.Mul(rate.revenue))
				approveCost = approveCost.Add(approveQuantity.Mul(rate.cost))
				rejectCost = rejectCost.Add(rejectQuantity.Mul(rate.cost))
			}
		}
		demandMetricUnit := commonUnit
		if demandMetricUnit == "mixed" {
			demandMetricUnit = "分模型需求组合指数"
		}
		var approve, reject ScenarioEstimate
		if isCashContributionBasis(input.metricBasis) {
			approve = scenarioFromTotals(approveDemand, approveRevenue, approveCost, uncertainty, demandMetricUnit, input.metricUnit)
			reject = scenarioFromTotals(rejectDemand, rejectRevenue, rejectCost, uncertainty, demandMetricUnit, input.metricUnit)
			if commonUnit == "mixed" {
				approve.Demand = Interval{Unit: "分模型需求向量（见 demand_units）"}
				reject.Demand = Interval{Unit: "分模型需求向量（见 demand_units）"}
			}
		} else {
			rejectUnitRevenue := input.currentRatio.Mul(input.revenueFactor)
			approveUnitRevenue := input.proposedRatio.Mul(input.revenueFactor)
			approve = scenario(approveDemand, approveUnitRevenue, input.riskCost, uncertainty, input.metricUnit, demandMetricUnit)
			reject = scenario(rejectDemand, rejectUnitRevenue, input.riskCost, uncertainty, input.metricUnit, demandMetricUnit)
		}
		delta := differenceInterval(approve.Contribution, reject.Contribution)
		result = append(result, HorizonForecast{Hours: horizon.hours, Label: horizon.label, Approve: approve, Reject: reject,
			DeltaContribution: delta, PositiveScore: positiveScore(delta), IntervalMethod: "relative_sensitivity_band",
			Calibrated: false, MetricBasis: input.metricBasis, DemandUnits: demandUnits, MinimumCoverage: coverage,
			RelativeErrorFloor: errorFloor, LedgerExclusions: &exclusions,
			OutcomeStatus: outcomeStatus(proposal.Status, horizon.hours),
			OutcomeNote:   outcomeNote(proposal.Status, horizon.hours)})
	}
	return result, nil
}

func elasticityDemandFactor(priceScale, elasticity decimal.Decimal) (decimal.Decimal, error) {
	if !priceScale.IsPositive() {
		return decimal.Zero, errors.New("price ratio must be positive for the elasticity forecast")
	}
	priceRatio, _ := priceScale.Float64()
	elasticityFloat, _ := elasticity.Float64()
	demandFactorFloat := math.Pow(priceRatio, elasticityFloat)
	if math.IsNaN(demandFactorFloat) || math.IsInf(demandFactorFloat, 0) || demandFactorFloat <= 0 {
		return decimal.Zero, errors.New("price elasticity produced a non-finite demand scenario")
	}
	return decimal.NewFromFloat(demandFactorFloat), nil
}

func scenario(demand, unitRevenue, unitCost, uncertainty decimal.Decimal, metricUnit, demandMetricUnit string) ScenarioEstimate {
	revenue := demand.Mul(unitRevenue)
	cost := demand.Mul(unitCost)
	return ScenarioEstimate{Demand: intervalFromValue(demand, uncertainty, demandMetricUnit, true),
		Revenue:      intervalFromValue(revenue, uncertainty, metricUnit, true),
		Cost:         intervalFromValue(cost, uncertainty, metricUnit, true),
		Contribution: intervalFromValue(revenue.Sub(cost), uncertainty, metricUnit, false)}
}

func scenarioFromTotals(demand, revenue, cost, uncertainty decimal.Decimal, demandMetricUnit, metricUnit string) ScenarioEstimate {
	return ScenarioEstimate{Demand: intervalFromValue(demand, uncertainty, demandMetricUnit, true),
		Revenue: intervalFromValue(revenue, uncertainty, metricUnit, true), Cost: intervalFromValue(cost, uncertainty, metricUnit, true),
		Contribution: intervalFromValue(revenue.Sub(cost), uncertainty, metricUnit, false)}
}

func intervalFromValue(value, uncertainty decimal.Decimal, unit string, nonNegative bool) Interval {
	spread := value.Abs().Mul(uncertainty)
	low, high := value.Sub(spread), value.Add(spread)
	if nonNegative && low.IsNegative() {
		low = decimal.Zero
	}
	if low.GreaterThan(high) {
		low, high = high, low
	}
	return Interval{Low: canonical(low), Base: canonical(value), High: canonical(high), Unit: unit}
}

func differenceInterval(approve, reject Interval) Interval {
	approveLow, _ := decimal.NewFromString(approve.Low)
	approveMid, _ := decimal.NewFromString(approve.Base)
	approveHigh, _ := decimal.NewFromString(approve.High)
	rejectLow, _ := decimal.NewFromString(reject.Low)
	rejectMid, _ := decimal.NewFromString(reject.Base)
	rejectHigh, _ := decimal.NewFromString(reject.High)
	return Interval{Low: canonical(approveLow.Sub(rejectHigh)), Base: canonical(approveMid.Sub(rejectMid)),
		High: canonical(approveHigh.Sub(rejectLow)), Unit: approve.Unit}
}

func positiveScore(value Interval) string {
	base, _ := decimal.NewFromString(value.Base)
	low, _ := decimal.NewFromString(value.Low)
	high, _ := decimal.NewFromString(value.High)
	width, _ := high.Sub(low).Float64()
	mean, _ := base.Float64()
	if math.IsNaN(width) || math.IsInf(width, 0) || math.IsNaN(mean) || math.IsInf(mean, 0) {
		return intervalSignScore(low, high)
	}
	if width <= 0 {
		switch {
		case mean > 0:
			return "1.000000"
		case mean < 0:
			return "0.000000"
		default:
			return "0.500000"
		}
	}
	sigma := width / (2 * standardNormalNinetiethPercent)
	probability := 0.5 * (1 + math.Erf(mean/(sigma*math.Sqrt2)))
	if math.IsNaN(probability) || math.IsInf(probability, 0) {
		return intervalSignScore(low, high)
	}
	return canonical(decimal.NewFromFloat(probability))
}

func intervalSignScore(low, high decimal.Decimal) string {
	switch {
	case low.IsPositive():
		return "0.900000"
	case high.IsNegative():
		return "0.100000"
	default:
		return "0.500000"
	}
}

type outcomeLearningSample struct {
	proposalID                       int64
	relativeSigned, relativeAbsolute decimal.Decimal
	startsAt, endsAt, observedAt     time.Time
}

type outcomeLearningKey struct {
	modelVersion, basis, unit, scope string
	horizon                          int
}

func (s *Service) applyOutcomeLearning(ctx context.Context, forecasts []HorizonForecast, minimum int, asOf time.Time) (string, error) {
	outcomes, err := s.store.ListCompletedDecisionOutcomes(ctx, 5000)
	if err != nil {
		return "", err
	}
	return calibrateOutcomeForecasts(forecasts, outcomes, minimum, asOf), nil
}

// calibrateOutcomeForecasts learns only from structural residuals of fully
// observed approved outcomes. Samples are isolated by target/model scope and
// overlapping exposure windows are clustered into one effective vote.
func calibrateOutcomeForecasts(forecasts []HorizonForecast, outcomes []store.DecisionOutcome, minimum int, asOf time.Time) string {
	if minimum < 3 {
		minimum = 3
	}
	required := make(map[outcomeLearningKey]struct{}, len(forecasts))
	for _, forecast := range forecasts {
		if strings.TrimSpace(forecast.LearningScope) == "" {
			continue
		}
		required[outcomeLearningKey{modelVersion: ModelVersion, basis: forecast.MetricBasis,
			unit: strings.ToUpper(strings.TrimSpace(forecast.Approve.Contribution.Unit)), scope: forecast.LearningScope,
			horizon: forecast.Hours}] = struct{}{}
	}
	strata := make(map[outcomeLearningKey]map[int64]outcomeLearningSample)
	type digestRow struct {
		ProposalID int64  `json:"proposal_id"`
		Horizon    int    `json:"horizon"`
		Scope      string `json:"scope"`
		Relative   string `json:"relative_error"`
		Absolute   string `json:"absolute_relative_error"`
		Due        string `json:"due_at"`
		Observed   string `json:"observed_at"`
	}
	digestRows := make([]digestRow, 0)
	for _, outcome := range outcomes {
		if outcome.Status != "observed" || outcome.ObservedAt == nil || outcome.ObservedAt.After(asOf) ||
			outcome.DueAt.IsZero() || outcome.DueAt.After(asOf) || outcome.ObservedAt.Before(outcome.DueAt) ||
			outcome.ModelVersion != ModelVersion || strings.TrimSpace(outcome.MetricBasis) == "" || strings.TrimSpace(outcome.MetricUnit) == "" {
			continue
		}
		var frozen HorizonForecast
		var observation ObservedOutcome
		if json.Unmarshal(outcome.Prediction, &frozen) != nil || json.Unmarshal(outcome.Observation, &observation) != nil ||
			frozen.Hours != outcome.HorizonHours || strings.TrimSpace(frozen.LearningScope) == "" ||
			!strings.EqualFold(strings.TrimSpace(observation.Unit), strings.TrimSpace(outcome.MetricUnit)) ||
			(isCashContributionBasis(outcome.MetricBasis) && !strings.EqualFold(strings.TrimSpace(observation.Currency), strings.TrimSpace(outcome.MetricUnit))) {
			continue
		}
		key := outcomeLearningKey{modelVersion: outcome.ModelVersion, basis: outcome.MetricBasis,
			unit: strings.ToUpper(strings.TrimSpace(outcome.MetricUnit)), scope: frozen.LearningScope, horizon: outcome.HorizonHours}
		if _, ok := required[key]; !ok {
			continue
		}
		structural, structuralErr := decimal.NewFromString(frozen.Approve.Contribution.Base)
		actual, actualErr := decimal.NewFromString(observation.Contribution)
		floor, floorErr := decimal.NewFromString(defaultValue(frozen.RelativeErrorFloor, DefaultConfig().RelativeErrorFloor))
		if structuralErr != nil || actualErr != nil || floorErr != nil || floor.IsNegative() {
			continue
		}
		scale := maxDecimal(structural.Abs(), actual.Abs(), floor)
		if !scale.IsPositive() {
			continue
		}
		relativeSigned := actual.Sub(structural).Div(scale)
		relativeAbsolute := relativeSigned.Abs()
		if strata[key] == nil {
			strata[key] = make(map[int64]outcomeLearningSample)
		}
		candidate := outcomeLearningSample{proposalID: outcome.ProposalID, relativeSigned: relativeSigned,
			relativeAbsolute: relativeAbsolute, startsAt: outcome.DueAt.Add(-time.Duration(outcome.HorizonHours) * time.Hour),
			endsAt: outcome.DueAt, observedAt: *outcome.ObservedAt}
		if current, exists := strata[key][outcome.ProposalID]; !exists || candidate.observedAt.After(current.observedAt) {
			strata[key][outcome.ProposalID] = candidate
		}
		digestRows = append(digestRows, digestRow{ProposalID: outcome.ProposalID, Horizon: outcome.HorizonHours, Scope: frozen.LearningScope,
			Relative: canonical(relativeSigned), Absolute: canonical(relativeAbsolute), Due: outcome.DueAt.UTC().Format(time.RFC3339Nano),
			Observed: outcome.ObservedAt.UTC().Format(time.RFC3339Nano)})
	}
	sort.Slice(digestRows, func(i, j int) bool {
		if digestRows[i].Horizon != digestRows[j].Horizon {
			return digestRows[i].Horizon < digestRows[j].Horizon
		}
		if digestRows[i].Scope != digestRows[j].Scope {
			return digestRows[i].Scope < digestRows[j].Scope
		}
		return digestRows[i].ProposalID < digestRows[j].ProposalID
	})

	for index := range forecasts {
		forecast := &forecasts[index]
		key := outcomeLearningKey{modelVersion: ModelVersion, basis: forecast.MetricBasis,
			unit: strings.ToUpper(strings.TrimSpace(forecast.Approve.Contribution.Unit)), scope: forecast.LearningScope, horizon: forecast.Hours}
		decisions := strata[key]
		if len(decisions) == 0 {
			continue
		}
		independent := clusterOverlappingLearningSamples(decisions)
		learning := &LearningAdjustment{DistinctProposals: len(decisions), IndependentWindows: len(independent), MinimumWindows: minimum,
			Method: "scope_isolated_nonoverlap_relative_residual_v2", Causal: false,
			Note: fmt.Sprintf("已有 %d 个不同决策、%d/%d 个不重叠证据窗口；达到门槛前保持结构预测与先验区间。", len(decisions), len(independent), minimum)}
		forecast.Learning = learning
		if len(independent) < minimum {
			continue
		}
		signedValues := make([]decimal.Decimal, 0, len(independent))
		absoluteValues := make([]decimal.Decimal, 0, len(independent))
		absoluteTotal := decimal.Zero
		for _, sample := range independent {
			signedValues = append(signedValues, sample.relativeSigned)
			absoluteValues = append(absoluteValues, sample.relativeAbsolute)
			absoluteTotal = absoluteTotal.Add(sample.relativeAbsolute)
		}
		sort.Slice(signedValues, func(i, j int) bool { return signedValues[i].LessThan(signedValues[j]) })
		sort.Slice(absoluteValues, func(i, j int) bool { return absoluteValues[i].LessThan(absoluteValues[j]) })
		medianRelative := decimalMedian(signedValues)
		relativeP80 := decimalNearestRank(absoluteValues, 0.80)
		meanAbsoluteRelative := absoluteTotal.Div(decimal.NewFromInt(int64(len(independent))))
		weight := decimal.NewFromInt(int64(len(independent))).Div(decimal.NewFromInt(int64(len(independent) + minimum)))
		relativeBias := medianRelative.Mul(weight)
		if relativeBias.Abs().GreaterThan(relativeP80) {
			relativeBias = relativeP80
			if medianRelative.IsNegative() {
				relativeBias = relativeBias.Neg()
			}
		}
		structuralBase, baseErr := decimal.NewFromString(forecast.Approve.Contribution.Base)
		floor, floorErr := decimal.NewFromString(defaultValue(forecast.RelativeErrorFloor, DefaultConfig().RelativeErrorFloor))
		if baseErr != nil || floorErr != nil || floor.IsNegative() {
			learning.Note = "历史样本已达到门槛，但当前投影尺度无法解析；本次保持先验结果。"
			continue
		}
		projectionScale := maxDecimal(structuralBase.Abs(), floor)
		bias := relativeBias.Mul(projectionScale)
		residualRadius := relativeP80.Mul(projectionScale)
		projection, ok := adaptiveContributionInterval(forecast.Approve.Contribution, bias, residualRadius)
		if !ok {
			learning.Note = "历史样本已达到门槛，但结构预测区间无法解析；本次保持先验结果。"
			continue
		}
		forecast.AdaptiveContribution = &projection
		forecast.Calibrated = true
		forecast.IntervalMethod = "structural_plus_scope_relative_residual_band_v2"
		learning.Active = true
		learning.BiasCorrection = canonical(bias)
		learning.MeanAbsoluteRelativeError = canonical(meanAbsoluteRelative)
		learning.RelativeResidualP80 = canonical(relativeP80)
		learning.ProjectionScale = canonical(projectionScale)
		learning.ShrinkageWeight = canonical(weight)
		learning.Note = "同站点、分组和模型范围内，不重叠窗口的结构预测相对误差中位数经样本量收缩后校正结果投影；P80 相对残差只扩宽不缩窄先验区间，不改写价格弹性，也不作因果归因。"
	}
	if len(digestRows) == 0 {
		return ""
	}
	payload, _ := json.Marshal(struct {
		Minimum int         `json:"minimum"`
		Rows    []digestRow `json:"rows"`
	}{Minimum: minimum, Rows: digestRows})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func clusterOverlappingLearningSamples(decisions map[int64]outcomeLearningSample) []outcomeLearningSample {
	items := make([]outcomeLearningSample, 0, len(decisions))
	for _, sample := range decisions {
		items = append(items, sample)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].startsAt.Equal(items[j].startsAt) {
			return items[i].startsAt.Before(items[j].startsAt)
		}
		return items[i].endsAt.Before(items[j].endsAt)
	})
	result := make([]outcomeLearningSample, 0, len(items))
	for start := 0; start < len(items); {
		end := start + 1
		clusterEnd := items[start].endsAt
		for end < len(items) && items[end].startsAt.Before(clusterEnd) {
			if items[end].endsAt.After(clusterEnd) {
				clusterEnd = items[end].endsAt
			}
			end++
		}
		signedValues := make([]decimal.Decimal, 0, end-start)
		absoluteValues := make([]decimal.Decimal, 0, end-start)
		for _, sample := range items[start:end] {
			signedValues = append(signedValues, sample.relativeSigned)
			absoluteValues = append(absoluteValues, sample.relativeAbsolute)
		}
		sort.Slice(signedValues, func(i, j int) bool { return signedValues[i].LessThan(signedValues[j]) })
		sort.Slice(absoluteValues, func(i, j int) bool { return absoluteValues[i].LessThan(absoluteValues[j]) })
		result = append(result, outcomeLearningSample{relativeSigned: decimalMedian(signedValues), relativeAbsolute: decimalMedian(absoluteValues),
			startsAt: items[start].startsAt, endsAt: clusterEnd})
		start = end
	}
	return result
}

func adaptiveContributionInterval(structural Interval, bias, residualRadius decimal.Decimal) (Interval, bool) {
	low, lowErr := decimal.NewFromString(structural.Low)
	base, baseErr := decimal.NewFromString(structural.Base)
	high, highErr := decimal.NewFromString(structural.High)
	if lowErr != nil || baseErr != nil || highErr != nil || low.GreaterThan(base) || base.GreaterThan(high) || residualRadius.IsNegative() {
		return Interval{}, false
	}
	left, right := base.Sub(low), high.Sub(base)
	if residualRadius.GreaterThan(left) {
		left = residualRadius
	}
	if residualRadius.GreaterThan(right) {
		right = residualRadius
	}
	adjusted := base.Add(bias)
	return Interval{Low: canonical(adjusted.Sub(left)), Base: canonical(adjusted), High: canonical(adjusted.Add(right)), Unit: structural.Unit}, true
}

func decimalMedian(values []decimal.Decimal) decimal.Decimal {
	if len(values) == 0 {
		return decimal.Zero
	}
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return values[middle-1].Add(values[middle]).Div(decimal.NewFromInt(2))
}

func decimalNearestRank(values []decimal.Decimal, quantile float64) decimal.Decimal {
	if len(values) == 0 {
		return decimal.Zero
	}
	index := int(math.Ceil(quantile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func combineHashes(left, right string) string {
	sum := sha256.Sum256([]byte(left + ":" + right))
	return hex.EncodeToString(sum[:])
}

func learningScopeKey(targetSiteID int64, targetGroup, modelPattern string, demandUnits map[string]string) string {
	payload, _ := json.Marshal(struct {
		TargetSiteID int64             `json:"target_site_id"`
		TargetGroup  string            `json:"target_group"`
		ModelPattern string            `json:"model_pattern"`
		DemandUnits  map[string]string `json:"demand_units"`
	}{TargetSiteID: targetSiteID, TargetGroup: strings.TrimSpace(targetGroup), ModelPattern: strings.TrimSpace(modelPattern), DemandUnits: demandUnits})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func maxDecimal(values ...decimal.Decimal) decimal.Decimal {
	result := decimal.Zero
	for _, value := range values {
		if value.GreaterThan(result) {
			result = value
		}
	}
	return result
}

func (s *Service) resolveCashRates(ctx context.Context, targetSiteID int64, group string, profiles []demandProfile, config Config, asOf time.Time) (map[string]cashUnitRate, decimal.Decimal, error) {
	if len(profiles) == 0 {
		return nil, decimal.Zero, errors.New("没有模型需求可用于现金换算")
	}
	events, err := s.store.ListOperatingLedgerEvents(ctx, targetSiteID, group, asOf.Add(-time.Duration(config.CashLookbackDays)*24*time.Hour), asOf)
	if err != nil {
		return nil, decimal.Zero, err
	}
	events = store.FilterOperatingLedgerEvents(events, *config.LedgerExclusions)
	type accumulator struct {
		unit, revenue, cost, included              string
		watermarkID, authoritativeTotal            string
		seen, completenessKnown, watermarkConflict bool
	}
	accumulators := make(map[string]*accumulator, len(profiles))
	for _, profile := range profiles {
		accumulators[strings.ToLower(profile.modelID)] = &accumulator{unit: profile.unit, revenue: "0", cost: "0", included: "0"}
	}
	for _, event := range events {
		accumulator, ok := accumulators[strings.ToLower(strings.TrimSpace(event.ModelID))]
		if !ok {
			continue
		}
		accumulator.seen = true
		if watermarkID, total, ok := authoritativeUsageWatermark(event.Metadata, accumulator.unit); ok {
			if !accumulator.completenessKnown {
				accumulator.watermarkID, accumulator.authoritativeTotal, accumulator.completenessKnown = watermarkID, canonical(total), true
			} else if accumulator.watermarkID != watermarkID || accumulator.authoritativeTotal != canonical(total) {
				accumulator.watermarkConflict = true
			}
		}
		if !observedEvidence(event.UsageEvidence) || strings.TrimSpace(event.UsageWeight) == "" || !strings.EqualFold(event.UsageUnit, accumulator.unit) {
			continue
		}
		quantity, quantityErr := decimal.NewFromString(event.UsageWeight)
		if quantityErr != nil || quantity.IsNegative() {
			continue
		}
		if !strings.EqualFold(event.Currency, config.ForecastCurrency) || !observedEvidence(event.RevenueEvidence) ||
			!observedEvidence(event.CostEvidence) || strings.TrimSpace(event.CashRevenue) == "" || strings.TrimSpace(event.ProcurementCost) == "" {
			continue
		}
		revenue, revenueErr := decimal.NewFromString(event.CashRevenue)
		cost, costErr := decimal.NewFromString(event.ProcurementCost)
		if revenueErr != nil || costErr != nil || revenue.IsNegative() || cost.IsNegative() {
			continue
		}
		accumulator.included = canonical(decimal.RequireFromString(accumulator.included).Add(quantity))
		accumulator.revenue = canonical(decimal.RequireFromString(accumulator.revenue).Add(revenue))
		accumulator.cost = canonical(decimal.RequireFromString(accumulator.cost).Add(cost))
	}
	minimum := decimal.RequireFromString(config.MinimumLedgerCoverage)
	minimumObserved := decimal.NewFromInt(1)
	rates := make(map[string]cashUnitRate, len(accumulators))
	for model, accumulator := range accumulators {
		if !accumulator.seen || !accumulator.completenessKnown || accumulator.watermarkConflict {
			return nil, decimal.Zero, fmt.Errorf("模型 %s 缺少一致的源站完整性水位和权威用量总量", model)
		}
		eligible, included := decimal.RequireFromString(accumulator.authoritativeTotal), decimal.RequireFromString(accumulator.included)
		if !eligible.IsPositive() {
			return nil, decimal.Zero, fmt.Errorf("模型 %s 在现金回看窗口内没有有效暴露量", model)
		}
		if included.GreaterThan(eligible) {
			return nil, decimal.Zero, fmt.Errorf("模型 %s 的已导入用量超过权威窗口总量", model)
		}
		coverage := included.Div(eligible)
		if coverage.LessThan(minimum) || !included.IsPositive() {
			return nil, coverage, fmt.Errorf("模型 %s 的联合现金账本覆盖率 %s 低于门槛 %s", model, canonical(coverage), config.MinimumLedgerCoverage)
		}
		if coverage.LessThan(minimumObserved) {
			minimumObserved = coverage
		}
		rates[model] = cashUnitRate{revenue: decimal.RequireFromString(accumulator.revenue).Div(included),
			cost: decimal.RequireFromString(accumulator.cost).Div(included)}
	}
	return rates, minimumObserved, nil
}

func estimateDailyDemand(items []store.OpportunityUsage, pattern string) (decimal.Decimal, string, time.Time, int) {
	profiles, level, latest := estimateDemandProfiles(items, pattern)
	total := decimal.Zero
	for _, profile := range profiles {
		total = total.Add(profile.daily)
	}
	return total, level, latest, len(profiles)
}

func estimateDemandProfiles(items []store.OpportunityUsage, pattern string) ([]demandProfile, string, time.Time) {
	type windows struct {
		seven, thirty         decimal.Decimal
		hasSeven, hasThirty   bool
		minimumConfidence     int
		unitSeven, unitThirty string
		observedAt            time.Time
	}
	byModel := make(map[string]windows)
	latestAt := time.Time{}
	for _, item := range effectiveForecastUsage(items, pattern) {
		value, err := decimal.NewFromString(item.DemandWeight)
		if err != nil || value.IsNegative() {
			continue
		}
		modelKey := strings.ToLower(strings.TrimSpace(item.ModelID))
		current := byModel[modelKey]
		confidence := confidenceRank(item.Confidence)
		if current.minimumConfidence == 0 || confidence < current.minimumConfidence {
			current.minimumConfidence = confidence
		}
		if item.ObservedAt.After(latestAt) {
			latestAt = item.ObservedAt
		}
		switch item.WindowDays {
		case 7:
			current.seven = value.Div(decimal.NewFromInt(7))
			current.hasSeven = true
			current.unitSeven = normalizedUsageUnit(item.DemandUnit)
		case 30:
			current.thirty = value.Div(decimal.NewFromInt(30))
			current.hasThirty = true
			current.unitThirty = normalizedUsageUnit(item.DemandUnit)
		}
		if item.ObservedAt.After(current.observedAt) {
			current.observedAt = item.ObservedAt
		}
		byModel[modelKey] = current
	}
	if len(byModel) == 0 {
		return nil, "missing", time.Time{}
	}
	level := "observed"
	profiles := make([]demandProfile, 0, len(byModel))
	for model, item := range byModel {
		profile := demandProfile{modelID: model, observedAt: item.observedAt, level: "observed"}
		switch {
		case item.hasSeven && item.hasThirty:
			if item.unitSeven != item.unitThirty {
				profile.unit = "mixed"
			} else {
				profile.unit = item.unitSeven
			}
			profile.daily = item.seven.Mul(decimal.RequireFromString("0.7")).Add(item.thirty.Mul(decimal.RequireFromString("0.3")))
		case item.hasSeven:
			profile.daily, profile.unit = item.seven, item.unitSeven
			level = "estimated"
			profile.level = "estimated"
		case item.hasThirty:
			profile.daily, profile.unit = item.thirty, item.unitThirty
			level = "estimated"
			profile.level = "estimated"
		}
		if item.minimumConfidence < 900 {
			level = "estimated"
			profile.level = "estimated"
		}
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].modelID < profiles[j].modelID })
	return profiles, level, latestAt
}

func normalizedUsageUnit(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "normalized_demand"
	}
	return value
}

func effectiveForecastUsage(items []store.OpportunityUsage, pattern string) []store.OpportunityUsage {
	type key struct {
		model string
		days  int
	}
	latest := make(map[key]store.OpportunityUsage)
	for _, item := range items {
		value, valueErr := decimal.NewFromString(item.DemandWeight)
		if !matchesPattern(item.ModelID, pattern) || (item.WindowDays != 7 && item.WindowDays != 30) || valueErr != nil || value.IsNegative() {
			continue
		}
		candidateKey := key{strings.ToLower(strings.TrimSpace(item.ModelID)), item.WindowDays}
		if current, ok := latest[candidateKey]; !ok || item.ObservedAt.After(current.ObservedAt) ||
			(item.ObservedAt.Equal(current.ObservedAt) && confidenceRank(item.Confidence) > confidenceRank(current.Confidence)) {
			latest[candidateKey] = item
		}
	}
	result := make([]store.OpportunityUsage, 0, len(latest))
	for _, item := range latest {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].ModelID), strings.ToLower(result[j].ModelID)
		if left != right {
			return left < right
		}
		return result[i].WindowDays < result[j].WindowDays
	})
	return result
}

func opportunityUsageAsOf(items []store.OpportunityUsage, asOf time.Time) []store.OpportunityUsage {
	result := make([]store.OpportunityUsage, 0, len(items))
	for _, item := range items {
		if item.ObservedAt.IsZero() || item.ObservedAt.After(asOf) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func matchesPattern(model, raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "*"
	}
	for _, pattern := range strings.Split(raw, ",") {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if ok, err := path.Match(pattern, strings.ToLower(strings.TrimSpace(model))); err == nil && ok {
			return true
		}
	}
	return false
}

func (s *Service) hydrateOutcomeViews(ctx context.Context, item *Recommendation) error {
	exposure, err := s.store.GetDecisionExposure(ctx, item.ProposalID)
	if err != nil {
		return err
	}
	rows, err := s.store.ListDecisionOutcomes(ctx, item.ProposalID)
	if err != nil {
		return err
	}
	byHorizon := make(map[int]store.DecisionOutcome, len(rows))
	for _, row := range rows {
		byHorizon[row.HorizonHours] = row
	}
	now := time.Now().UTC()
	for index := range item.Forecasts {
		forecast := &item.Forecasts[index]
		due := exposure.EffectiveAt.Add(time.Duration(forecast.Hours) * time.Hour)
		forecast.DueAt = &due
		if row, ok := byHorizon[forecast.Hours]; ok {
			forecast.OutcomeStatus = row.Status
			if row.Status == "observed" {
				var observation ObservedOutcome
				if err := json.Unmarshal(row.Observation, &observation); err != nil {
					return err
				}
				forecast.Observation = &observation
				forecast.OutcomeNote = "该结果窗口已由不可变账本核对完成。"
			} else {
				forecast.Observation = nil
				forecast.OutcomeNote = outcomeNoteForStoredStatus(row.Status, row.ExclusionReason)
			}
			continue
		}
		forecast.Observation = nil
		switch {
		case exposure.EndedAt != nil && due.After(*exposure.EndedAt):
			forecast.OutcomeStatus, forecast.OutcomeNote = "censored", "价格暴露提前结束，等待后台核对器固定删失证据。"
		case now.Before(due):
			forecast.OutcomeStatus, forecast.OutcomeNote = "not_due", fmt.Sprintf("从远程读回生效时点起计算，%s 后核对。", forecast.Label)
		default:
			forecast.OutcomeStatus, forecast.OutcomeNote = "awaiting_ledger", "窗口已到期，等待后台核对器汇总经营账本。"
		}
	}
	return nil
}

func outcomeNoteForStoredStatus(status, reason string) string {
	switch status {
	case "censored":
		return "价格暴露提前结束，该窗口不进入校准。"
	case "coverage_unknown":
		return "已导入账本缺少权威用量分母，暂不进入校准。"
	case "insufficient_coverage":
		return "已导入账本内联合现金字段覆盖不足，暂不进入校准。"
	case "no_exposure":
		return "窗口内有权威零用量证据，不进入校准。"
	case "incompatible":
		return "决策快照不是现金口径，不能与现金账本核对。"
	case "awaiting_ledger":
		return "现金账本尚未形成可核对结果。"
	case "not_due":
		return "结果窗口仍在观察中。"
	default:
		return reason
	}
}

func (s *Service) reconcileOutcomes(ctx context.Context, item *Recommendation, modelPattern string) error {
	exposure, err := s.store.GetDecisionExposure(ctx, item.ProposalID)
	if err != nil {
		return err
	}
	existingRows, err := s.store.ListDecisionOutcomes(ctx, item.ProposalID)
	if err != nil {
		return err
	}
	existingByHorizon := make(map[int]store.DecisionOutcome, len(existingRows))
	for _, existing := range existingRows {
		existingByHorizon[existing.HorizonHours] = existing
	}
	for index := range item.Forecasts {
		forecast := &item.Forecasts[index]
		projection := forecast.Approve.Contribution
		if forecast.AdaptiveContribution != nil {
			projection = *forecast.AdaptiveContribution
		}
		prediction, _ := json.Marshal(forecast)
		outcome := store.DecisionOutcome{ProposalID: item.ProposalID, HorizonHours: forecast.Hours,
			Status: forecast.OutcomeStatus, Prediction: prediction, MetricBasis: forecast.MetricBasis,
			MetricUnit: projection.Unit, ModelVersion: item.ModelVersion}
		due := exposure.EffectiveAt.Add(time.Duration(forecast.Hours) * time.Hour)
		forecast.DueAt, outcome.DueAt = &due, due
		existing, hasExisting := existingByHorizon[forecast.Hours]
		if hasExisting && (existing.Status == "observed" || existing.Status == "censored") {
			forecast.OutcomeStatus = existing.Status
			if existing.Status == "observed" {
				var observation ObservedOutcome
				if err := json.Unmarshal(existing.Observation, &observation); err != nil {
					return fmt.Errorf("decode frozen observed outcome for %dh: %w", forecast.Hours, err)
				}
				forecast.Observation = &observation
				forecast.OutcomeNote = "该结果窗口已由不可变账本核对完成；重复调度不会改写观测时间或学习证据。"
			} else {
				forecast.Observation = nil
				forecast.OutcomeNote = "价格暴露提前结束，该窗口已固定为删失样本，不进入校准。"
			}
			continue
		}
		if exposure.EndedAt != nil && due.After(*exposure.EndedAt) {
			forecast.OutcomeStatus = "censored"
			forecast.OutcomeNote = fmt.Sprintf("该价格暴露在 %s 提前结束，未覆盖完整 %s 窗口，因此不会进入校准。",
				exposure.EndedAt.Format(time.RFC3339), forecast.Label)
			forecast.Observation = nil
			outcome.Status, outcome.ExclusionReason = "censored", "partial_exposure"
		} else if time.Now().UTC().Before(outcome.DueAt) {
			forecast.OutcomeStatus, forecast.OutcomeNote = "not_due", fmt.Sprintf("从远程读回生效时点起计算，%s 后核对。", forecast.Label)
			outcome.Status = forecast.OutcomeStatus
			if hasExisting && existing.Status == "not_due" {
				continue
			}
		} else {
			if !isCashContributionBasis(forecast.MetricBasis) {
				forecast.OutcomeStatus = "incompatible"
				forecast.OutcomeNote = "当前是无币种的倍率加权等价情景，不与现金账本混算，也不进入现金校准。"
				forecast.Observation = nil
				outcome.Status, outcome.ExclusionReason = forecast.OutcomeStatus, "non_cash_basis"
			} else {
				events, err := s.store.ListOperatingLedgerEvents(ctx, item.TargetSiteID, item.TargetGroup, exposure.EffectiveAt, outcome.DueAt)
				if err != nil {
					return err
				}
				if forecast.LedgerExclusions != nil {
					events = store.FilterOperatingLedgerEvents(events, *forecast.LedgerExclusions)
				}
				observation, status, note := summarizeObserved(events, modelPattern, forecast.Approve.Contribution.Unit,
					forecast.DemandUnits, forecast.MinimumCoverage)
				if status == "observed" {
					forecast.OutcomeStatus = "observed"
					forecast.OutcomeNote = "同目标分组、同模型、同计量单位、同币种的联合账本覆盖率已达标；仅核对实际批准场景。"
					forecast.Observation = &observation
					observedJSON, _ := json.Marshal(observation)
					actual := decimal.RequireFromString(observation.Contribution)
					predicted := decimal.RequireFromString(projection.Base)
					signed := actual.Sub(predicted)
					absolute := signed.Abs()
					actualAbsolute := actual.Abs()
					epsilon := decimal.RequireFromString(defaultValue(forecast.RelativeErrorFloor, DefaultConfig().RelativeErrorFloor))
					pct := ""
					if actualAbsolute.GreaterThanOrEqual(epsilon) && actualAbsolute.IsPositive() {
						pct = canonical(absolute.Div(actualAbsolute))
					}
					denominator := actualAbsolute.Add(predicted.Abs())
					symmetric := "0.000000"
					if denominator.IsPositive() {
						symmetric = canonical(absolute.Mul(decimal.NewFromInt(2)).Div(denominator))
					}
					low := decimal.RequireFromString(projection.Low)
					high := decimal.RequireFromString(projection.High)
					hit := !actual.LessThan(low) && !actual.GreaterThan(high)
					now := time.Now().UTC()
					outcome.Status, outcome.Observation, outcome.AbsoluteError, outcome.AbsolutePctError = "observed", observedJSON, canonical(absolute), pct
					outcome.SignedError, outcome.SymmetricPctError, outcome.ActualAbsolute = canonical(signed), symmetric, canonical(actualAbsolute)
					outcome.BandHit, outcome.CoverageRatio, outcome.ObservedAt = &hit, observation.CoverageRatio, &now
				} else {
					forecast.OutcomeStatus = status
					forecast.OutcomeNote = note
					forecast.Observation = nil
					outcome.Status, outcome.CoverageRatio, outcome.ExclusionReason = status, observation.CoverageRatio, status
				}
			}
		}
		prediction, _ = json.Marshal(forecast)
		outcome.Prediction = prediction
		if err := s.store.UpsertDecisionOutcome(ctx, outcome); err != nil {
			return err
		}
	}
	return nil
}

func summarizeObserved(events []store.OperatingLedgerEvent, modelPattern, expectedCurrency string, demandUnits map[string]string, minimumCoverage string) (ObservedOutcome, string, string) {
	type cohort struct {
		unit, included                    string
		watermarkID, authoritativeTotal   string
		seen, completenessKnown, conflict bool
	}
	revenue, cost := decimal.Zero, decimal.Zero
	expectedCurrency = strings.ToUpper(strings.TrimSpace(expectedCurrency))
	validEvents := 0
	evidence := "exact_cash_ledger"
	excluded := map[string]int{}
	cohorts := make(map[string]*cohort, len(demandUnits))
	for model, unit := range demandUnits {
		cohorts[strings.ToLower(strings.TrimSpace(model))] = &cohort{unit: strings.ToLower(strings.TrimSpace(unit)), included: "0"}
	}
	for _, event := range events {
		if !matchesPattern(event.ModelID, modelPattern) {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(event.ModelID))
		item, ok := cohorts[key]
		if !ok {
			excluded["scope_changed"]++
			return ObservedOutcome{ExcludedByReason: excluded}, "coverage_unknown", "窗口内出现决策快照未覆盖的新模型，实际范围已经变化，不能拿它校准旧预测。"
		}
		item.seen = true
		if watermarkID, total, ok := authoritativeUsageWatermark(event.Metadata, item.unit); ok {
			if !item.completenessKnown {
				item.watermarkID, item.authoritativeTotal, item.completenessKnown = watermarkID, canonical(total), true
			} else if item.watermarkID != watermarkID || item.authoritativeTotal != canonical(total) {
				item.conflict = true
			}
		}
		if !observedEvidence(event.UsageEvidence) || strings.TrimSpace(event.UsageWeight) == "" || !strings.EqualFold(event.UsageUnit, item.unit) {
			excluded["usage_denominator_unknown"]++
			continue
		}
		quantity, quantityErr := decimal.NewFromString(event.UsageWeight)
		if quantityErr != nil || quantity.IsNegative() {
			excluded["usage_invalid"]++
			continue
		}
		if expectedCurrency == "" || !strings.EqualFold(event.Currency, expectedCurrency) {
			excluded["currency_mismatch"]++
			continue
		}
		if !observedEvidence(event.RevenueEvidence) || !observedEvidence(event.CostEvidence) ||
			strings.TrimSpace(event.CashRevenue) == "" || strings.TrimSpace(event.ProcurementCost) == "" {
			excluded["joint_cash_missing"]++
			continue
		}
		r, rErr := decimal.NewFromString(event.CashRevenue)
		c, cErr := decimal.NewFromString(event.ProcurementCost)
		if rErr != nil || cErr != nil || r.IsNegative() || c.IsNegative() {
			excluded["cash_invalid"]++
			continue
		}
		revenue, cost = revenue.Add(r), cost.Add(c)
		item.included = canonical(decimal.RequireFromString(item.included).Add(quantity))
		validEvents++
		if event.RevenueEvidence != "exact" || event.CostEvidence != "exact" {
			evidence = "observed_cash_ledger"
		}
	}
	minimum, err := decimal.NewFromString(defaultValue(minimumCoverage, DefaultConfig().MinimumLedgerCoverage))
	if err != nil {
		minimum = decimal.RequireFromString(DefaultConfig().MinimumLedgerCoverage)
	}
	minimumObserved := decimal.NewFromInt(1)
	totalEligible, totalIncluded := decimal.Zero, decimal.Zero
	usageUnits := make(map[string]string, len(cohorts))
	distinctUnits := map[string]struct{}{}
	for model, item := range cohorts {
		usageUnits[model] = item.unit
		distinctUnits[item.unit] = struct{}{}
		if !item.seen || !item.completenessKnown || item.conflict {
			return ObservedOutcome{CoverageRatio: "", UsageUnits: usageUnits, ExcludedByReason: excluded}, "coverage_unknown",
				"至少一个决策范围内模型缺少一致的源站完整性水位与权威窗口总量；系统不会让局部账本冒充完整窗口。"
		}
		eligible, included := decimal.RequireFromString(item.authoritativeTotal), decimal.RequireFromString(item.included)
		if included.GreaterThan(eligible) {
			return ObservedOutcome{CoverageRatio: "", UsageUnits: usageUnits, ExcludedByReason: excluded}, "coverage_unknown",
				"已导入用量超过权威窗口总量，完整性证据存在冲突。"
		}
		totalEligible, totalIncluded = totalEligible.Add(eligible), totalIncluded.Add(included)
		if eligible.IsPositive() {
			coverage := included.Div(eligible)
			if coverage.LessThan(minimumObserved) {
				minimumObserved = coverage
			}
		}
	}
	observation := ObservedOutcome{Contribution: canonical(revenue.Sub(cost)), Revenue: canonical(revenue), Cost: canonical(cost),
		Currency: expectedCurrency, Unit: expectedCurrency, EventCount: validEvents, Evidence: evidence,
		EligibleQuantity: canonical(totalEligible), IncludedQuantity: canonical(totalIncluded), UsageUnits: usageUnits, ExcludedByReason: excluded}
	if len(distinctUnits) > 1 {
		observation.EligibleQuantity, observation.IncludedQuantity = "", ""
	}
	if !totalEligible.IsPositive() {
		observation.CoverageRatio = "0.000000"
		return observation, "no_exposure", "该窗口有权威零用量证据，没有发生可核对的价格暴露，因此不进入校准。"
	}
	observation.CoverageRatio = canonical(minimumObserved)
	if minimumObserved.LessThan(minimum) || validEvents == 0 {
		return observation, "insufficient_coverage", fmt.Sprintf("联合现金账本最低模型覆盖率为 %s，未达到门槛 %s。", observation.CoverageRatio, canonical(minimum))
	}
	return observation, "observed", ""
}

func observedEvidence(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "exact" || value == "observed"
}

// authoritativeUsageWatermark is an explicit completeness contract supplied by
// the ledger importer. Row presence alone can never prove that other rows were
// not omitted, so cash calibration requires all four metadata fields.
func authoritativeUsageWatermark(metadata map[string]string, expectedUnit string) (string, decimal.Decimal, bool) {
	if !strings.EqualFold(strings.TrimSpace(metadata["usage_window_complete"]), "true") {
		return "", decimal.Zero, false
	}
	id := strings.TrimSpace(metadata["usage_window_id"])
	unit := strings.TrimSpace(metadata["usage_window_unit"])
	total, err := decimal.NewFromString(strings.TrimSpace(metadata["usage_window_total"]))
	if id == "" || !strings.EqualFold(unit, expectedUnit) || err != nil || total.IsNegative() {
		return "", decimal.Zero, false
	}
	return id, total, true
}

func (s *Service) calibration(ctx context.Context, minimum int) (Calibration, error) {
	items, err := s.store.ListCompletedDecisionOutcomes(ctx, 5000)
	if err != nil {
		return Calibration{}, err
	}
	return calculateCalibration(items, minimum), nil
}

type calibrationSample struct {
	absolute, pct, symmetric, signed, actual decimal.Decimal
	hasPct, hasBand                          bool
	bandHit                                  bool
}

func calculateCalibration(items []store.DecisionOutcome, minimum int) Calibration {
	result := Calibration{TotalOutcomes: len(items), MinimumSamples: minimum}
	if len(items) == 0 {
		result.Note = fmt.Sprintf("尚无完成结果窗口；至少需要 %d 个可核验决策后才显示校准精度。", minimum)
		return result
	}
	type stratumKey struct {
		modelVersion, basis, unit string
		horizon                   int
	}
	strata := map[stratumKey]map[int64]calibrationSample{}
	decisionSamples := map[int64][]calibrationSample{}
	measurementStrata := map[string]struct{}{}
	for _, item := range items {
		switch item.Status {
		case "censored":
			result.CensoredCount++
			continue
		case "coverage_unknown", "insufficient_coverage", "no_exposure":
			result.CoverageExcluded++
			continue
		case "observed":
		default:
			continue
		}
		if item.ModelVersion != ModelVersion {
			result.IncompatibleExcluded++
			continue
		}
		absolute, absErr := decimal.NewFromString(item.AbsoluteError)
		signed, signedErr := decimal.NewFromString(item.SignedError)
		symmetric, symmetricErr := decimal.NewFromString(item.SymmetricPctError)
		actual, actualErr := decimal.NewFromString(item.ActualAbsolute)
		if absErr != nil || signedErr != nil || symmetricErr != nil || actualErr != nil {
			continue
		}
		result.SampleCount++
		current := calibrationSample{absolute: absolute, signed: signed, symmetric: symmetric, actual: actual}
		if pct, err := decimal.NewFromString(item.AbsolutePctError); err == nil && !pct.IsNegative() {
			current.pct, current.hasPct = pct, true
		} else if actual.IsZero() {
			result.ZeroActualCount++
			if absolute.IsZero() {
				result.PerfectZeroCount++
			}
		} else {
			result.NearZeroExcluded++
		}
		band := item.BandHit
		if band == nil {
			band = item.IntervalHit
		}
		if band != nil {
			current.hasBand, current.bandHit = true, *band
		}
		key := stratumKey{item.ModelVersion, item.MetricBasis, item.MetricUnit, item.HorizonHours}
		measurementStrata[item.MetricBasis+"\x00"+item.MetricUnit] = struct{}{}
		if strata[key] == nil {
			strata[key] = map[int64]calibrationSample{}
		}
		strata[key][item.ProposalID] = current
		decisionSamples[item.ProposalID] = append(decisionSamples[item.ProposalID], current)
	}
	result.DecisionCount = len(decisionSamples)
	for key, decisions := range strata {
		metrics := aggregateSamples(decisions)
		result.Horizons = append(result.Horizons, HorizonCalibration{HorizonHours: key.horizon,
			Ready:       metrics.decisionCount >= minimum,
			SampleCount: metrics.sampleCount, DecisionCount: metrics.decisionCount, MAPE: metrics.mape, MAE: metrics.mae,
			WAPE: metrics.wape, SMAPE: metrics.smape, SignedBias: metrics.signedBias, BandCoverage: metrics.bandCoverage,
			ModelVersion: key.modelVersion, MetricBasis: key.basis, MetricUnit: key.unit})
	}
	sort.Slice(result.Horizons, func(i, j int) bool {
		if result.Horizons[i].HorizonHours != result.Horizons[j].HorizonHours {
			return result.Horizons[i].HorizonHours < result.Horizons[j].HorizonHours
		}
		return result.Horizons[i].MetricUnit < result.Horizons[j].MetricUnit
	})
	if len(decisionSamples) > 0 {
		macro := aggregateDecisionMacro(decisionSamples)
		result.MAPE, result.MAE, result.WAPE, result.SMAPE, result.SignedBias = macro.mape, macro.mae, macro.wape, macro.smape, macro.signedBias
		result.IntervalCoverage = macro.bandCoverage
		result.MAPESampleCount, result.IntervalSamples = macro.mapeCount, macro.bandCount
		if len(measurementStrata) > 1 {
			result.MAE, result.WAPE, result.SignedBias = "", "", ""
		}
	}
	result.Ready = len(result.Horizons) > 0
	for _, horizon := range result.Horizons {
		result.Ready = result.Ready && horizon.Ready
	}
	if result.Ready {
		result.Note = fmt.Sprintf("%d 个实际生效决策已按模型版本、口径、币种和 horizon 分层达到门槛；总览先在决策内等权，再跨决策宏平均。", result.DecisionCount)
	} else {
		result.Note = fmt.Sprintf("已有 %d 个当前模型契约下的可核验决策；各 horizon 必须分别达到 %d 个独立决策才宣称校准。已排除覆盖不足 %d 个、截断窗口 %d 个、不兼容旧契约 %d 个。",
			result.DecisionCount, minimum, result.CoverageExcluded, result.CensoredCount, result.IncompatibleExcluded)
	}
	return result
}

type aggregatedMetrics struct {
	sampleCount, decisionCount, mapeCount, bandCount int
	mape, mae, wape, smape, signedBias, bandCoverage string
}

func aggregateSamples(items map[int64]calibrationSample) aggregatedMetrics {
	result := aggregatedMetrics{sampleCount: len(items), decisionCount: len(items)}
	absoluteTotal, signedTotal, symmetricTotal, actualTotal := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	mapeTotal := decimal.Zero
	bandHits := 0
	for _, item := range items {
		absoluteTotal, signedTotal = absoluteTotal.Add(item.absolute), signedTotal.Add(item.signed)
		symmetricTotal, actualTotal = symmetricTotal.Add(item.symmetric), actualTotal.Add(item.actual)
		if item.hasPct {
			mapeTotal, result.mapeCount = mapeTotal.Add(item.pct), result.mapeCount+1
		}
		if item.hasBand {
			result.bandCount++
			if item.bandHit {
				bandHits++
			}
		}
	}
	if result.sampleCount > 0 {
		denominator := decimal.NewFromInt(int64(result.sampleCount))
		result.mae = canonical(absoluteTotal.Div(denominator))
		result.smape = canonical(symmetricTotal.Div(denominator))
		result.signedBias = canonical(signedTotal.Div(denominator))
	}
	if actualTotal.IsPositive() {
		result.wape = canonical(absoluteTotal.Div(actualTotal))
	}
	if result.mapeCount > 0 {
		result.mape = canonical(mapeTotal.Div(decimal.NewFromInt(int64(result.mapeCount))))
	}
	if result.bandCount > 0 {
		result.bandCoverage = canonical(decimal.NewFromInt(int64(bandHits)).Div(decimal.NewFromInt(int64(result.bandCount))))
	}
	return result
}

func aggregateDecisionMacro(items map[int64][]calibrationSample) aggregatedMetrics {
	result := aggregatedMetrics{decisionCount: len(items)}
	mapeTotal, maeTotal, wapeTotal := decimal.Zero, decimal.Zero, decimal.Zero
	smapeTotal, biasTotal, bandTotal := decimal.Zero, decimal.Zero, decimal.Zero
	maeDecisions, wapeDecisions, smapeDecisions, biasDecisions := 0, 0, 0, 0
	for _, windows := range items {
		if len(windows) == 0 {
			continue
		}
		result.sampleCount += len(windows)
		absolute, actual, signed, symmetric, pct := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
		pctCount, bandCount, bandHits := 0, 0, 0
		for _, window := range windows {
			absolute, actual = absolute.Add(window.absolute), actual.Add(window.actual)
			signed, symmetric = signed.Add(window.signed), symmetric.Add(window.symmetric)
			if window.hasPct {
				pct, pctCount = pct.Add(window.pct), pctCount+1
			}
			if window.hasBand {
				bandCount++
				if window.bandHit {
					bandHits++
				}
			}
		}
		windowCount := decimal.NewFromInt(int64(len(windows)))
		maeTotal, maeDecisions = maeTotal.Add(absolute.Div(windowCount)), maeDecisions+1
		smapeTotal, smapeDecisions = smapeTotal.Add(symmetric.Div(windowCount)), smapeDecisions+1
		biasTotal, biasDecisions = biasTotal.Add(signed.Div(windowCount)), biasDecisions+1
		if actual.IsPositive() {
			wapeTotal, wapeDecisions = wapeTotal.Add(absolute.Div(actual)), wapeDecisions+1
		}
		if pctCount > 0 {
			mapeTotal, result.mapeCount = mapeTotal.Add(pct.Div(decimal.NewFromInt(int64(pctCount)))), result.mapeCount+1
		}
		if bandCount > 0 {
			bandTotal, result.bandCount = bandTotal.Add(decimal.NewFromInt(int64(bandHits)).Div(decimal.NewFromInt(int64(bandCount)))), result.bandCount+1
		}
	}
	average := func(total decimal.Decimal, count int) string {
		if count == 0 {
			return ""
		}
		return canonical(total.Div(decimal.NewFromInt(int64(count))))
	}
	result.mae, result.wape = average(maeTotal, maeDecisions), average(wapeTotal, wapeDecisions)
	result.smape, result.signedBias = average(smapeTotal, smapeDecisions), average(biasTotal, biasDecisions)
	result.mape, result.bandCoverage = average(mapeTotal, result.mapeCount), average(bandTotal, result.bandCount)
	return result
}

func lifecycleStatus(status string) string {
	switch status {
	case "pending", "applying":
		return "awaiting_decision"
	case "applied":
		return "tracking"
	case "rejected":
		return "rejected"
	case "no_change":
		return "no_intervention"
	case "rolled_back", "rolling_back":
		return "rolled_back"
	case "stale":
		return "superseded"
	default:
		return "failed"
	}
}

func isDecisionTerminal(status string) bool {
	return status != "pending" && status != "applying"
}

func outcomeStatus(status string, hours int) string {
	switch status {
	case "applied":
		return "not_due"
	case "pending", "applying":
		return "awaiting_decision"
	default:
		return "not_applicable"
	}
}

func outcomeNote(status string, hours int) string {
	switch status {
	case "applied":
		return fmt.Sprintf("批准后 %d 小时到期时，用请求级账本核对实际批准场景。", hours)
	case "pending", "applying":
		return "管理员尚未作出决定，结果窗口不会提前启动。"
	case "rejected":
		return "拒绝建议只能验证维持现状的结果，不能把未执行的批准场景当作事实。"
	default:
		return "没有发生可归因的价格干预，本窗口不纳入模型校准。"
	}
}

func evidenceLegend() []EvidenceItem {
	return []EvidenceItem{
		{Key: "exact", Label: "精确", Level: "exact", Source: "不可变账本或配置读回", Note: "可逐项复算，不由预测模型改写。"},
		{Key: "observed", Label: "观测", Level: "observed", Source: "站点统计或上游响应", Note: "真实观测，但可能是聚合口径。"},
		{Key: "estimated", Label: "估算", Level: "estimated", Source: "确定性公式", Note: "输入有证据，结果仍依赖换算假设。"},
		{Key: "prior", Label: "先验", Level: "prior", Source: "保守默认参数", Note: "尚未被本站样本充分校准。"},
		{Key: "missing", Label: "缺失", Level: "missing", Source: "未接入", Note: "不以 0 代替未知，也不生成伪精确结论。"},
	}
}

func recommendationHash(proposal domain.ChangeProposal, policy domain.PricingPolicy, config Config, usage []store.OpportunityUsage) string {
	payload, _ := json.Marshal(struct {
		Model    string                   `json:"model"`
		Proposal domain.ChangeProposal    `json:"proposal"`
		Policy   domain.PricingPolicy     `json:"policy"`
		Config   Config                   `json:"config"`
		Usage    []store.OpportunityUsage `json:"usage"`
	}{ModelVersion, proposal, policy, config, effectiveForecastUsage(usage, policy.SourceModelPattern)})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func confidenceRank(raw string) int {
	value, err := decimal.NewFromString(raw)
	if err != nil {
		return 0
	}
	return int(value.Mul(decimal.NewFromInt(1000)).IntPart())
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func validCurrencyCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func canonical(value decimal.Decimal) string { return value.StringFixed(6) }

func sortedRecommendations(items []Recommendation) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
}
