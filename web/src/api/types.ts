export type HealthStatus = 'healthy' | 'warning' | 'error' | 'unknown';

export interface RelayOpsHealth {
  status: string;
  service: string;
  api_contract_version?: string;
	remote_writes_enabled?: boolean;
  time: string;
}

export type MirrorRuleType = 'same' | 'add' | 'multiply' | 'fixed' | 'target_margin' | 'markup' | 'subsidy_rate';

export interface MirrorRule {
	type: MirrorRuleType;
	value?: string;
}

export interface MirrorPolicy {
	policy_uuid: string;
	policy_version: number;
	schema_version: 3;
	legacy_policy_id?: number;
	supplier_id: number;
	source_group: string;
	source_model_pattern: string;
	excluded_models: string[];
	target_site_id: number;
	target_group: string;
	enabled: boolean;
	kind: 'complete_mirror' | 'legacy_group_ratio' | 'legacy_group_model_contract';
	price_anchor: 'source_base' | 'effective_sale';
	sync_group_ratio: boolean;
	group_ratio_rule: MirrorRule;
	sync_model_prices: boolean;
	model_price_rule: MirrorRule;
	sync_billing_mode: boolean;
	sync_model_additions: boolean;
	sync_model_removals: boolean;
	model_scope_mode: 'mirror_group' | 'fixed_allowlist';
	empty_group_action: 'preserve' | 'disable_managed_channel' | 'remove_managed_models';
	execution_mode: 'auto' | 'approval';
	legacy_rule_target?: string;
	minimum_ratio?: string;
	maximum_ratio?: string;
	rounding_step?: string;
	only_follow_increase: boolean;
	only_follow_decrease: boolean;
}

export type MirrorPolicyDocument = Omit<MirrorPolicy, 'legacy_policy_id'>;

export interface MirrorServiceGate {
	code: string;
	message: string;
	blocks_apply: boolean;
}

export interface MirrorPreview {
	id: string;
	policy: MirrorPolicy;
	channel_id: number;
	source_state: 'present' | 'empty' | 'missing';
	source_revision: string;
	source_semantic_hash: string;
	source_observed_at: string;
	snapshot_response_hash: string;
	confirmations: Array<{ kind: string; result: Record<string, unknown> }>;
	evidence_ready: boolean;
	plan: Record<string, unknown>;
	service_gates?: MirrorServiceGate[];
	executable: boolean;
	generated_at: string;
}

export interface MirrorChangeStep {
	id: number;
	change_set_id: number;
	step_key: string;
	phase: string;
	resource_kind: string;
	resource_id: string;
	action: string;
	request_id: string;
	expected_hash: string;
	desired_hash: string;
	readback_hash?: string;
	state: string;
	attempts: number;
	fencing_token: number;
	error?: string;
	compensation_state?: string;
	intent_at?: string;
	result_at?: string;
	created_at: string;
	updated_at: string;
}

export interface MirrorChangeSet {
	id: number;
	operation_key: string;
	request_hash: string;
	policy_uuid: string;
	policy_version: number;
	target_site_id: number;
	source_semantic_hash: string;
	expected_contract_revision: string;
	desired_contract_revision: string;
	status: string;
	lease_owner_id: string;
	fencing_token: number;
	error?: string;
	created_at: string;
	updated_at: string;
	finished_at?: string;
	steps?: MirrorChangeStep[];
}

export interface MirrorChangeEvent {
	id: number;
	change_set_id: number;
	step_id?: number;
	event_type: string;
	phase: string;
	fencing_token: number;
	created_at: string;
}

export interface MirrorApplyReceipt {
	preview_id: string;
	change_set: MirrorChangeSet;
	replayed: boolean;
}

export interface MirrorAdoptionPreviewRequest {
	channel_id: number;
	policy: MirrorPolicyDocument;
}

export interface TargetManagedChannelState {
	channel_id: number;
	models: string[];
	status: number;
	hash: string;
}

export interface MirrorAdoptionPreview {
	id: string;
	policy: MirrorPolicy;
	channel: TargetChannel;
	managed_state: TargetManagedChannelState;
	contract_revision: string;
	desired_contract_revision: string;
	contract_models: Record<string, GroupModelPricingSetting>;
	source_model_count: number;
	source_target_overlap: number;
	service_gates?: MirrorServiceGate[];
	already_adopted: boolean;
	executable: boolean;
	generated_at: string;
}

export interface MirrorAdoptionReceipt {
	preview_id: string;
	policy: MirrorPolicy;
	channel_id: number;
	contract_revision: string;
	remote_stamped: boolean;
	replayed: boolean;
}
export type AuthStatus = 'valid' | 'expiring' | 'missing' | 'pending' | 'invalid';

export interface DashboardMetrics {
  supplierCount: number;
  healthySuppliers: number;
  activeAlerts: number;
  todayCost: number | null;
  todayRevenue: number | null;
  grossMargin: number | null;
  automatedPolicies: number;
  pendingChanges: number;
}

export interface Supplier {
  id: string;
	credentialId: string | null;
  name: string;
  baseUrl: string;
  kind: 'NewAPI' | 'Sub2API' | 'JSON Connector';
	enabled: boolean;
  status: HealthStatus;
  authStatus: AuthStatus;
	authAttention: boolean;
	authAttentionReasons?: string[];
  collectInterval: number;
	rechargeValueFactor: string;
  lastCollectedAt: string;
  lastAttemptAt: string;
  lastSuccessAt: string;
  nextCollectedAt: string;
  latencyMs: number | null;
  lastFailureDurationMs: number | null;
  lastErrorClass?: string;
  groupCount: number;
  modelCount: number;
  balance: number | null;
  balanceCurrency?: string;
  priceChangeCount: number;
  focused: boolean;
  note?: string;
}

export interface WatchItem {
  id: string;
	supplierId: number;
  supplier: string;
	scopeType: 'group' | 'model';
	groupKey: string;
	modelKey: string;
  group: string;
  model: string;
	displayName: string;
	note?: string;
	manual: boolean;
	sourcePricing: boolean;
	sourceMirror: boolean;
	sourceChange: boolean;
	evidenceHash?: string;
  upstreamRate: number | null;
  localRate: number | null;
  grossMargin: number | null;
  delta24h: number | null;
  status: HealthStatus;
  updatedAt: string;
}

export interface WatchSettings {
	auto_enabled: boolean;
	retention_days: number;
	automatic_limit: number;
	overview_limit: number;
}

export interface WatchlistResponse {
	items: WatchItem[];
	settings: WatchSettings;
	automatic_candidates: number;
}

export interface WatchItemInput {
	supplier_id: number;
	scope_type: 'group' | 'model';
	group_key: string;
	model_key: string;
	display_name: string;
	note: string;
	evidence_hash?: string;
}

export type PolicyMode = 'add' | 'multiply' | 'target_margin' | 'markup' | 'subsidy_rate' | 'fixed';

export interface PricingPolicy {
  id: string;
  name: string;
  supplier: string;
  upstreamGroup: string;
  targetGroup: string;
  mode: PolicyMode;
  parameter: number;
  upstreamRate: number;
  suggestedRate: number | null;
  currentRate: number | null;
	currentRateStatus: 'live' | 'trusted_readback' | 'proposal_expectation' | 'missing_group' | 'unavailable' | string;
	sourceBasis: 'group_ratio' | 'equivalent_cost' | string;
	ruleBasisRate: number | null;
	equivalentCostRate: number | null;
	ruleMargin: number | null;
	catalogRiskMargin: number | null;
	cashMargin: number | null;
  enabled: boolean;
  allowNegativeMargin: boolean;
  executionMode: 'approval' | 'auto';
  coverageMode: 'strict' | 'threshold' | 'owner_override';
  minimumCoverage: number;
  targetWriteMode: 'approval' | 'auto' | string;
  proposalMode?: 'approval' | 'auto' | string;
  proposalStatus?: string;
  proposalError?: string;
  automationEligible?: boolean;
  automationReason?: string;
  lastRunAt: string;
}

export type PricingPolicyBulkStrategy = 'atomic' | 'best_effort';

export interface PricingPolicyBulkMutationInput {
  policy_ids: number[];
  execution_mode?: 'approval' | 'auto';
  enabled?: boolean;
  strategy?: PricingPolicyBulkStrategy;
  preview_hash?: string;
}

export interface PricingPolicyBulkItem {
  policy_id: number;
  policy_name?: string;
  target_site_id?: number;
  target_site_name?: string;
  target_enabled?: boolean;
  target_write_mode?: string;
  before_execution_mode?: string;
  after_execution_mode?: string;
  before_enabled?: boolean;
  after_enabled?: boolean;
  status: 'ready' | 'unchanged' | 'blocked' | 'applied' | string;
  code?: string;
  message: string;
  policy_updated_at?: string;
  target_updated_at?: string;
}

export interface PricingPolicyBulkTargetCheck {
  target_site_id: number;
  target_site_name: string;
  policy_count: number;
  enabled: boolean;
  write_mode: string;
  requires_auto_write: boolean;
  auto_write_ready: boolean;
  message: string;
}

export interface PricingPolicyBulkSummary {
  selected: number;
  found: number;
  changed: number;
  unchanged: number;
  blocked: number;
  targets_affected: number;
  execution_mode_changes: number;
  enable_changes: number;
  disable_changes: number;
  auto_write_ready: number;
  auto_write_blocked: number;
}

export interface PricingPolicyBulkPreview {
  execution_mode?: 'approval' | 'auto';
  enabled?: boolean;
  strategy: PricingPolicyBulkStrategy;
  summary: PricingPolicyBulkSummary;
  items: PricingPolicyBulkItem[];
  targets: PricingPolicyBulkTargetCheck[];
  preview_hash: string;
  generated_at: string;
}

export interface PricingPolicyBulkResult {
  status: 'applied' | 'partial' | 'no_change' | 'blocked' | string;
  strategy: PricingPolicyBulkStrategy;
  summary: PricingPolicyBulkSummary;
  items: PricingPolicyBulkItem[];
  targets: PricingPolicyBulkTargetCheck[];
  preview_hash: string;
  applied_at?: string;
}

export interface BalanceForecast {
  id: string;
  supplier: string;
	balance: number | null;
	currency?: string;
	spentToday: number | null;
  dailyAverage: number | null;
  runwayHours: number | null;
  thresholdHours: number;
  forecastAt: string;
  status: HealthStatus;
  samples: number[];
	rawValue?: string;
	rawUnit?: string;
	sourceField?: string;
	divisor?: string;
	formula?: string;
	observedAt?: string;
	historicalSpend?: number | null;
	requestCount?: number;
	dailyRequestCount?: number;
	usageSource?: string;
	usageObservedAt?: string;
	rawBalance?: string;
	rawDailySpend?: string;
	rawCurrency?: string;
	cashFactor?: string;
	conversionFormula?: string;
}

export interface CollectionJobResult {
	supplier_id: number;
	supplier_name: string;
	status: 'succeeded' | 'failed' | string;
	changed: boolean;
	duration_ms: number;
	error?: string;
}

export interface CollectionJob {
	id: string;
	status: 'running' | 'completed' | 'partial_failed' | 'failed' | string;
	total: number;
	completed: number;
	succeeded: number;
	failed: number;
	results: CollectionJobResult[];
	started_at: string;
	finished_at?: string;
}

export interface Discovery {
  id: string;
  supplier: string;
  upstreamGroup: string;
  upstreamRate: number | null;
  modelCount: number;
  featuredModels: string[];
  foundAt: string;
  recommendation: string;
  status: 'new' | 'reviewing' | 'ignored' | 'listed';
	classification?: 'baseline' | 'new';
}

export type OpportunityClassification = 'recommended' | 'watch' | 'no_advantage' | 'insufficient_data';

export interface OpportunityModelResult {
	model_id: string;
	family?: string;
	daily_demand: string;
	expected_daily_profit: string;
	expected_daily_saving: string;
	conservative_lower_bound: string;
	comparable_coverage: string;
	demand_evidence: 'observed' | 'prior' | 'unknown';
	demand_unit?: string;
	price_unit?: string;
}

export interface OpportunityResult {
	id: string;
	supplier_id: number;
	supplier: string;
	group: string;
	classification: OpportunityClassification;
	eligible: boolean;
	expected_daily_profit?: string;
	expected_monthly_profit?: string;
	expected_daily_saving?: string;
	expected_monthly_saving?: string;
	gross_margin?: string;
	recommended_rule_type?: PolicyMode;
	recommended_rule_value?: string;
	recommended_rounding_step?: string;
	recommendation_basis?: string;
	recommendation_note?: string;
	conservative_lower_bound?: string;
	model_coverage?: string;
	availability?: string;
	confidence?: string;
	currency?: string;
	model_ids: string[];
	model_families: string[];
	models: OpportunityModelResult[];
	reason: string;
	warnings: string[];
	hard_filter_reasons: string[];
	uses_prior: boolean;
	subsidy_required_daily?: string;
	subsidy_budget_daily?: string;
	subsidy_within_budget?: boolean;
	observed_at: string;
	evaluated_at: string;
	input_hash: string;
}

export interface OpportunityResponse {
	items: OpportunityResult[];
	summary: Record<OpportunityClassification, number>;
	target?: {
		id: number;
		name: string;
		source: 'live' | 'cache' | 'stale_cache' | 'missing' | 'unavailable';
		observed_at?: string;
		stale: boolean;
	};
	warnings: string[];
	has_observed_usage: boolean;
	generated_at: string;
}

export type IntelligenceEvidenceLevel = 'exact' | 'observed' | 'estimated' | 'prior' | 'missing';

export interface IntelligenceEvidenceItem {
	key: string;
	label: string;
	level: IntelligenceEvidenceLevel;
	value?: string;
	unit?: string;
	source: string;
	observed_at?: string;
	note: string;
}

export interface IntelligenceInterval {
	low: string;
	base: string;
	high: string;
	p10?: string;
	p50?: string;
	p90?: string;
	unit: string;
}

export interface IntelligenceScenarioEstimate {
	demand: IntelligenceInterval;
	revenue: IntelligenceInterval;
	cost: IntelligenceInterval;
	contribution: IntelligenceInterval;
}

export interface IntelligenceObservedOutcome {
	contribution: string;
	revenue: string;
	cost: string;
	currency?: string;
	unit: string;
	event_count: number;
	evidence: string;
	eligible_quantity: string;
	included_quantity: string;
	coverage_ratio: string;
	usage_units?: Record<string, string>;
	excluded_by_reason?: Record<string, number>;
}

export interface IntelligenceLearningAdjustment {
	active: boolean;
	distinct_proposals: number;
	independent_windows: number;
	minimum_windows: number;
	bias_correction?: string;
	mean_absolute_relative_error?: string;
	relative_residual_p80?: string;
	projection_scale?: string;
	shrinkage_weight?: string;
	method: string;
	causal: boolean;
	note: string;
}

export interface IntelligenceHorizonForecast {
	hours: number;
	label: string;
	learning_scope?: string;
	approve: IntelligenceScenarioEstimate;
	reject: IntelligenceScenarioEstimate;
	adaptive_contribution?: IntelligenceInterval;
	learning?: IntelligenceLearningAdjustment;
	delta_contribution: IntelligenceInterval;
	positive_score: string;
	interval_method: string;
	calibrated: boolean;
	metric_basis: 'ratio_weighted_equivalent_v2' | 'ledger_derived_cash_contribution_v1' | 'contract_planned_cash_contribution_v1' | 'ratio_weighted_equivalent_v1' | 'cash_contribution_v1';
	demand_units?: Record<string, string>;
	minimum_coverage?: string;
	relative_error_floor?: string;
	due_at?: string;
	outcome_status: string;
	outcome_note: string;
	observation?: IntelligenceObservedOutcome;
}

export interface IntelligenceRecommendation {
	proposal_id: number;
	policy_id: number;
	target_site_id: number;
	policy_name: string;
	supplier: string;
	target: string;
	target_group: string;
	model_pattern?: string;
	status: string;
	lifecycle_status: string;
	current_ratio?: string;
	proposed_ratio: string;
	pricing_mode?: 'group_model_contract' | string;
	current_revision?: string;
	proposed_revision?: string;
	forecast_covered_models?: number;
	forecast_total_models?: number;
	forecast_coverage?: string;
	created_at: string;
	decided_at?: string;
	effective_at?: string;
	decision_kind?: string;
	forecast_ready: boolean;
	forecast_reason?: string;
	forecasts: IntelligenceHorizonForecast[];
	evidence: IntelligenceEvidenceItem[];
	warnings: string[];
	model_version: string;
	input_hash: string;
}

export interface IntelligenceConfig {
	price_elasticity: string;
	prior_uncertainty: string;
	observed_uncertainty: string;
	minimum_calibration_samples: number;
	forecast_currency?: string;
	minimum_ledger_coverage: string;
	relative_error_floor: string;
	cash_lookback_days: number;
	financial_lookback_days: number;
	display_currency: string;
	currency_rates: Record<string, string>;
	business_time_zone: string;
	ledger_exclusions: LedgerExclusionPolicy;
}

export interface LedgerExclusionPolicy {
	exclude_administrators: boolean;
	exclude_test_users: boolean;
	exclude_internal_tokens: boolean;
	excluded_user_segments?: string[];
	excluded_user_ids?: string[];
	excluded_token_ids?: string[];
}

export interface LedgerCoverageSummary {
	status: string;
	window_start: string;
	window_end: string;
	cohorts: number;
	verified_cohorts: number;
	conflicting_watermarks: number;
	missing_watermark_cohorts: number;
	note: string;
}

export interface LedgerCurrencySummary {
	currency: string;
	usage_event_count: number;
	matched_usage_event_count: number;
	cash_flow_event_count: number;
	usage_revenue: string;
	usage_procurement_cost: string;
	matched_usage_revenue: string;
	matched_usage_procurement_cost: string;
	usage_gross_profit?: string;
	usage_gross_margin?: string;
	usage_profit_available: boolean;
	recharge_gross: string;
	refunds: string;
	payment_fees: string;
	procurement_payments: string;
	gift_credit_issued: string;
	other_income: string;
	other_expense: string;
	commission_award_events: number;
	commission_reversal_events: number;
	commission_expense: string;
	commission_reversal: string;
	cash_net_flow?: string;
	cash_net_flow_available: boolean;
	unknown_usage_revenue_events: number;
	unknown_usage_cost_events: number;
	unknown_cash_flow_amount_events: number;
	evidence_quality: string;
}

export interface LedgerSegmentSummary {
	user_segment: string;
	currency: string;
	included: boolean;
	exclusion_reasons?: string[];
	event_count: number;
	requests: number;
	revenue_known_events: number;
	cost_known_events: number;
	known_revenue?: string;
	known_cost?: string;
	revenue_coverage: string;
	cost_coverage: string;
	contribution?: string;
	contribution_available: boolean;
	requires_ledger_fields?: string[];
}

export interface LedgerFinancialSummary {
	window_start: string;
	window_end: string;
	raw_event_count: number;
	included_event_count: number;
	included_usage_event_count: number;
	matched_usage_event_count: number;
	excluded_event_count: number;
	excluded_by_reason: Record<string, number>;
	excluded_reason_counts_overlap: boolean;
	unclassified_subject_events: number;
	unknown_currency_events: number;
	unknown_usage_currency_events: number;
	unknown_amount_events: number;
	unknown_usage_revenue_events: number;
	unknown_usage_cost_events: number;
	unknown_cash_flow_amount_events: number;
	by_currency: LedgerCurrencySummary[];
	excluded_by_currency?: LedgerCurrencySummary[];
	user_segments: LedgerSegmentSummary[];
	groups: LedgerDimensionSummary[];
	models?: LedgerDimensionSummary[];
	pricing_contracts?: LedgerPricingContractSummary[];
	cross_currency_aggregate_available: boolean;
	coverage: LedgerCoverageSummary;
	warnings: string[];
}

export interface LedgerDimensionSummary {
	key: string;
	label: string;
	currency: string;
	event_count: number;
	requests: number;
	revenue_known_events: number;
	cost_known_events: number;
	known_revenue?: string;
	known_cost?: string;
	revenue_coverage: string;
	cost_coverage: string;
	gross_profit?: string;
	gross_margin?: string;
	profit_available: boolean;
	requires_ledger_fields?: string[];
}

export interface LedgerPricingContractSummary {
	key: string;
	group_name: string;
	model_id: string;
	pricing_scope: string;
	pricing_revision?: string;
	billing_mode: string;
	billing_unit: string;
	fixed_duration_seconds?: string;
	currency: string;
	event_count: number;
	requests: number;
	revenue_known_events: number;
	cost_known_events: number;
	contract_evidence_known_events: number;
	known_revenue?: string;
	known_cost?: string;
	revenue_coverage: string;
	cost_coverage: string;
	contract_evidence_coverage: string;
	gross_profit?: string;
	gross_margin?: string;
	profit_available: boolean;
	requires_ledger_fields?: string[];
}

export interface UnifiedMoney {
	amount?: string;
	currency: string;
	available: boolean;
	known_items: number;
	unknown_items: number;
	evidence: string;
	note?: string;
}

export interface UnifiedRatio {
	value?: string;
	available: boolean;
	evidence: string;
	numerator?: string;
	denominator?: string;
	matched_items: number;
	total_items: number;
	coverage?: string;
	formula?: string;
	note?: string;
}

export interface SupplierCashPosition {
	supplier_id: number;
	supplier_name: string;
	balance: UnifiedMoney;
	daily_spend: UnifiedMoney;
	raw_balance?: string;
	raw_daily_spend?: string;
	raw_currency?: string;
	recharge_factor: string;
	conversion_formula?: string;
	runway_hours?: string;
	observed_at?: string;
}

export interface GroupOperatingSummary {
	group: string;
	event_count: number;
	requests: number;
	revenue: UnifiedMoney;
	cost: UnifiedMoney;
	gross_profit: UnifiedMoney;
	gross_margin: UnifiedRatio;
	revenue_coverage: string;
	cost_coverage: string;
}

export interface ProfitDataQuality {
	window_start: string;
	window_end: string;
	included_usage_events: number;
	revenue_known_events: number;
	revenue_unknown_events: number;
	cost_known_events: number;
	cost_unknown_events: number;
	unknown_currency_events: number;
	revenue_coverage: string;
	cost_coverage: string;
	ledger_coverage_status: string;
	strict_profit_available: boolean;
	operating_estimate_ready: boolean;
	required_ledger_fields: string[];
	blockers: string[];
	next_action: string;
	operating_estimate_basis: string;
	cost_resolution_window_start?: string;
	cost_resolution_window_end?: string;
	cost_resolution_observed_at?: string;
	cost_resolution_scanned_events: number;
	cost_resolution_resolved_events: number;
	cost_resolution_unresolved_events: number;
	cost_resolution_truncated: boolean;
	cost_resolution_reasons: CostResolutionReason[];
}

export interface CostResolutionSample {
	target_group?: string;
	model?: string;
	channel_id?: string;
	channel_name?: string;
	policy_id?: string;
	policy_name?: string;
	supplier_id?: string;
	supplier_name?: string;
	source_group?: string;
}

export interface CostResolutionReason {
	target_site_id?: number;
	target_name?: string;
	code: string;
	message: string;
	count: number;
	samples: CostResolutionSample[];
}

export interface CostResolutionStatus {
	target_site_id: number;
	window_start: string;
	window_end: string;
	scanned_events: number;
	resolved_events: number;
	unresolved_events: number;
	truncated: boolean;
	reasons: CostResolutionReason[];
	observed_at: string;
}

export interface OperatingSnapshot {
	display_currency: string;
	currency_rates: Record<string, string>;
	business_time_zone: string;
	day_started_at: string;
	admin_excluded: boolean;
	user_balance: UnifiedMoney;
	today_user_spend: UnifiedMoney;
	upstream_balance: UnifiedMoney;
	upstream_daily_spend: UnifiedMoney;
	today_revenue: UnifiedMoney;
	today_cost: UnifiedMoney;
	today_gross_profit: UnifiedMoney;
	today_gross_margin: UnifiedRatio;
	today_commission_cost: UnifiedMoney;
	today_net_profit: UnifiedMoney;
	today_net_margin: UnifiedRatio;
	operating_gap_revenue: UnifiedMoney;
	operating_gap_includes_administrators: boolean;
	operating_gap_estimate: UnifiedMoney;
	operating_gap_margin: UnifiedRatio;
	profit_data_quality: ProfitDataQuality;
	supplier_positions: SupplierCashPosition[];
	groups: GroupOperatingSummary[];
	warnings: string[];
	generated_at: string;
}

export interface IntelligenceDashboard {
	summary: {
		recommendations: number;
		pending: number;
		applied: number;
		forecast_ready: number;
		ledger_events: number;
		exact_revenue_events: number;
		exact_cost_events: number;
	};
	recommendations: IntelligenceRecommendation[];
	ledger: {
		event_count: number;
		exact_revenue_events: number;
		exact_cost_events: number;
		estimated_revenue_events: number;
		estimated_cost_events: number;
		earliest_at?: string;
		latest_at?: string;
	};
	financials: LedgerFinancialSummary;
	operations: OperatingSnapshot;
	calibration: {
		ready: boolean;
		total_outcomes: number;
		sample_count: number;
		decision_count: number;
		mape_sample_count: number;
		band_sample_count: number;
		minimum_samples: number;
		mape?: string;
		mae?: string;
		wape?: string;
		smape?: string;
		signed_bias?: string;
		band_coverage?: string;
		zero_actual_count: number;
		near_zero_excluded: number;
		perfect_zero_count: number;
		coverage_excluded: number;
		censored_count: number;
		incompatible_excluded: number;
		horizons: Array<{
			horizon_hours: number;
			ready: boolean;
			sample_count: number;
			decision_count: number;
			model_version: string;
			metric_basis: string;
			metric_unit: string;
			mape?: string;
			mae?: string;
			wape?: string;
			smape?: string;
			signed_bias?: string;
			band_coverage?: string;
		}>;
		note: string;
	};
	evidence_legend: IntelligenceEvidenceItem[];
	warnings: string[];
	config: IntelligenceConfig;
	generated_at: string;
	cache_state: 'ready' | 'catching_up' | 'stale';
	cache_generated_at: string;
	cache_revision: number;
	source_revision: number;
	refreshing: boolean;
}

export interface OverviewIntelligenceSnapshot {
	operations: OperatingSnapshot;
	raw_event_count: number;
	cache_state: string;
	cache_generated_at: string;
	cache_revision: number;
	source_revision: number;
	generated_at: string;
}

export interface TargetLedgerSyncConfig {
	target_site_id: number;
	enabled: boolean;
	interval_seconds: number;
	overlap_seconds: number;
	initial_lookback_seconds: number;
	page_size: number;
	max_pages: number;
	currency?: string;
	quota_cash_factor?: string;
	cost_evidence_max_age_seconds: number;
	supplier_cost_cash_rate: string;
	log_path: string;
	topup_path: string;
	user_path: string;
	created_at?: string;
	updated_at?: string;
}

export interface TargetLedgerSyncStream {
	target_site_id: number;
	stream: 'users' | 'logs' | 'topups' | string;
	cursor_time?: string;
	cursor_id?: string;
	status: string;
	last_error?: string;
	last_attempt_at?: string;
	last_success_at?: string;
	next_attempt_at?: string;
	last_scanned_rows: number;
	last_imported_events: number;
	last_skipped_rows: number;
	has_last_known_good: boolean;
	unsupported: boolean;
	auth_required: boolean;
	next_action: string;
}

export interface TargetLedgerSyncStatus {
	config: TargetLedgerSyncConfig;
	streams: TargetLedgerSyncStream[];
	last_run_at?: string;
	next_run_at?: string;
	running: boolean;
	unit_mode: 'auto' | 'manual' | string;
	effective_currency?: string;
	effective_quota_cash_factor?: string;
	unit_source?: string;
	unit_observed_at?: string;
	unit_warning?: string;
	cost_resolution?: CostResolutionStatus;
	generated_at: string;
}

export interface TargetLedgerSyncResult {
	target_site_id: number;
	status: TargetLedgerSyncStatus;
	started_at: string;
	finished_at: string;
}

export interface ChangeProposal {
  id: string;
  title: string;
  target: string;
  type: 'price' | 'listing' | 'credential' | 'config';
  status: 'pending' | 'applying' | 'applied' | 'no_change' | 'stale' | 'rejected' | 'failed' | 'rolling_back' | 'rolled_back';
  createdAt: string;
  createdBy: string;
  reason: string;
  before: string;
  after: string;
  impact: string;
  readback?: string;
  error?: string;
	pricingMode?: 'group_model_contract' | 'legacy_group_ratio' | string;
	currentRevision?: string;
	proposedRevision?: string;
	changedModels?: number;
	minimumMargin?: string;
	modelChanges?: ConsoleModelPriceChange[];
}

export interface ConsoleModelPriceDimensionChange {
	dimension: string;
	currency?: string;
	sourceCashCost?: string;
	currentSale?: string;
	proposedSale?: string;
	marginRate?: string;
	currentInherited?: boolean;
}

export interface ConsoleModelPriceChange {
	model: string;
	billingMode: string;
	billingUnit: string;
	currency?: string;
	minimumMargin?: string;
	dimensions: ConsoleModelPriceDimensionChange[];
}

export interface Connector {
  id: string;
  name: string;
  kind: 'builtin' | 'json';
  protocol: 'NewAPI' | 'Sub2API' | 'Custom';
  version: string;
  supplierCount: number;
  status: HealthStatus;
  capabilities: string[];
  updatedAt: string;
}

export interface Credential {
  id: string;
  supplier: string;
  method: 'Access Token' | 'Cookie' | 'API Key' | 'Browser Pairing' | 'Unconfigured';
  status: AuthStatus;
  expiresAt: string | null;
  lastRefreshedAt: string;
  autoRefresh: boolean;
	renewal?: CredentialRenewalStatus;
	recollectionPending?: boolean;
	browserCustody?: BrowserCustodyStatus;
}

export interface BrowserCustodyStatus {
	enabled: boolean;
	supplier_id: number;
	credential_id: number;
	expected_origin: string;
	expires_at?: string;
	revoked_at?: string;
	last_check_at?: string;
	last_submission_at?: string;
	last_success_at?: string;
	submission_count: number;
	last_error_class?: string;
	last_error_message?: string;
	helper_version?: string;
	minimum_helper_version?: string;
	helper_state?: 'binding_pending' | 'online' | 'offline' | 'upgrade_required' | 'disabled';
	helper_online: boolean;
	created_at?: string;
	updated_at?: string;
	needs_capture: boolean;
	need_reason?: string;
	need_since?: string;
	recovery_pending: boolean;
	recovery_overdue: boolean;
}

export interface CredentialRenewalStatus {
	configured: boolean;
	enabled: boolean;
	state: string;
	consecutive_failures: number;
	last_attempt_at?: string;
	last_success_at?: string;
	next_attempt_at?: string;
	human_action?: string;
	last_error_class?: string;
	updated_at?: string;
}

export interface CredentialLoginRuleView {
	url: string;
	method: string;
	content_type: string;
	username_field: string;
	password_field: string;
	token_json_path?: string;
	user_id_json_path?: string;
	expires_in_json_path?: string;
	expires_at_json_path?: string;
	success_json_path?: string;
	success_value?: string;
	message_json_path?: string;
	login_refresh_token_json_path?: string;
	refresh_url?: string;
	refresh_method?: string;
	refresh_token_field?: string;
	refresh_response_token_json_path?: string;
	refresh_response_refresh_token_json_path?: string;
	refresh_response_expires_in_json_path?: string;
	refresh_response_expires_at_json_path?: string;
	captcha_json_path?: string;
	two_factor_json_path?: string;
	cookie_response: boolean;
	minimum_interval_seconds: number;
	initial_backoff_seconds: number;
	maximum_backoff_seconds: number;
	maximum_consecutive_failures: number;
	human_action_cooldown_seconds: number;
	allow_replace_browser_pairing: boolean;
}

export interface CredentialRenewalPreset {
	supplier_id: number;
	adapter: string;
	adapter_family?: string;
	label?: string;
	supported: boolean;
	rule?: CredentialLoginRuleView;
}

export interface CredentialRenewalDetail {
	credential_id: number;
	status: CredentialRenewalStatus;
	username_configured: boolean;
	password_configured: boolean;
	rule?: CredentialLoginRuleView;
}

export interface ConfigureCredentialRenewalInput {
	enabled: boolean;
	username?: string;
	password?: string;
	rule: CredentialLoginRuleView;
}

export interface PairingStatus {
	status: 'pending' | 'completed' | 'failed' | 'expired';
	supplier_id: number;
	completed_at?: string;
	failed_at?: string;
	last_error_class?: string;
	last_error_message?: string;
	last_attempt_at?: string;
	attempt_count: number;
	retryable: boolean;
	attempts_remaining: number;
	retry_after_seconds?: number;
	attempt_consumed?: boolean;
	validation_stage?: 'input_validation' | 'upstream_readonly';
	old_credential_preserved?: boolean;
}

export interface SupplierPasswordLoginInput {
	username: string;
	password: string;
	enable_renewal: boolean;
	rule?: CredentialLoginRuleView;
}

export interface SupplierPasswordLoginResult {
	credential: RawCredential;
	supplier_id: number;
	preset: string;
	validation: { capabilities?: AuthCapabilityEvidence[] };
	renewal_enabled: boolean;
	collection_started: boolean;
}

export interface ConnectorManifestAuth {
	header?: string;
	scheme?: string;
	user_header?: string;
}

export interface ConnectorManifestCapability {
	paths: string[];
	contract?: 'native' | 'json';
	auth?: ConnectorManifestAuth;
	json_paths?: Record<string, string>;
}

export interface ConnectorManifestDocument {
	schema_version: 'relayops.connector-manifest/v1';
	name: string;
	adapter_family: 'newapi' | 'sub2api';
	origins: { schemes: string[]; hosts: string[] };
	auth?: ConnectorManifestAuth;
	capabilities: Record<string, ConnectorManifestCapability>;
	login?: CredentialLoginRuleView;
}

export interface ConnectorManifestRecord {
	id: number;
	name: string;
	schema_version: string;
	adapter_family: string;
	manifest: ConnectorManifestDocument;
	source: string;
	validation_status: string;
	validation_message?: string;
	created_at: string;
	updated_at: string;
}

export interface SupplierManifestBinding {
	supplier_id: number;
	manifest_id: number;
	bound_origin: string;
	bound_at: string;
	manifest?: ConnectorManifestRecord;
}

export interface Activity {
  id: string;
	kind: 'collect' | 'price' | 'balance' | 'auth' | 'listing' | 'mirror' | 'system';
	rawKind: string;
  title: string;
  detail: string;
  at: string;
  status: HealthStatus;
	severity: string;
	metadata?: Record<string, string>;
}

export interface ActivityPage {
	items: Activity[];
	page: number;
	limit: number;
	total: number;
}

export interface ActivityQuery {
	page?: number;
	limit?: number;
	after_id?: number;
	include_total?: boolean;
	from?: string;
	to?: string;
	category?: Activity['kind'] | '';
	status?: 'healthy' | 'warning' | 'error' | 'unknown' | '';
	exclude_category?: Activity['kind'] | '';
}

export interface DeleteActivitiesInput extends Omit<ActivityQuery, 'page' | 'limit'> {
	ids?: number[];
	all?: boolean;
	limit?: number;
}

export interface MirrorDraftBatchResult {
	target_group: string;
	supplier: string;
	source_group: string;
	channel: string;
	status: 'created' | 'updated' | 'unchanged' | 'failed';
	reason?: string;
}

export interface MirrorDraftBatchInput {
	target_site_id: number;
	created: number;
	updated: number;
	unchanged: number;
	failed: number;
	results: MirrorDraftBatchResult[];
}

export interface BootstrapData {
	automationEnabled: boolean;
	backgroundWorkersEnabled: boolean;
	remoteWritesEnabled: boolean;
  metrics: DashboardMetrics;
  suppliers: Supplier[];
  watchItems: WatchItem[];
	watchSettings: WatchSettings;
	watchCandidateCount: number;
  policies: PricingPolicy[];
  balances: BalanceForecast[];
  discoveries: Discovery[];
  proposals: ChangeProposal[];
  connectors: Connector[];
  credentials: Credential[];
  activities: Activity[];
	runtime?: RuntimeSnapshot;
  generatedAt: string;
}

export interface RuntimeSnapshot {
	startedAt: string;
	uptimeSeconds: number;
	databaseBytes: number;
	processMemoryBytes: number;
	goroutines: number;
	targets: RuntimeTargetStatus[];
	generatedAt: string;
}

export interface RuntimeTargetStatus {
	id: string;
	name: string;
	enabled: boolean;
	status: HealthStatus;
	updatedAt?: string;
}

export interface AutomationControl {
	enabled: boolean;
	background_workers_enabled: boolean;
	remote_writes_enabled: boolean;
	updated_at: string;
}

export interface AutomationConfirmationConfig {
	enabled: boolean;
	interval_seconds: number;
	max_attempts: number;
}

export interface AutomationSafetyConfig {
	target_read_cache_milliseconds: number;
	equivalent_cost_multiple_limit: string;
	minimum_comparable_dimensions: number;
	maximum_automatic_increase_rate: string;
	maximum_automatic_decrease_rate: string;
}

export interface ApiResult<T> {
  data: T;
  source: 'live';
  error?: string;
}

export interface RawSupplier {
  id: number;
  name: string;
  base_url: string;
  adapter: string;
  enabled: boolean;
  poll_interval_seconds: number;
	recharge_value_factor: string;
  focus_groups: string[];
  focus_models: string[];
	pricing_path?: string;
	balance_path?: string;
	credential_id?: number | null;
	metadata?: Record<string, string>;
	last_status?: string;
	last_error?: string;
	last_error_class?: string;
	last_attempt_at?: string;
	last_success_at?: string;
	last_success_duration_ms?: number;
	last_failure_duration_ms?: number;
}

export interface SupplierDeletionImpact {
	supplier_id: number;
	supplier_name: string;
	base_url: string;
	impact_hash: string;
	credential_id?: number;
	credential_preserved: boolean;
	pricing_policies: number;
	change_proposals: number;
	listing_plans: number;
	listing_change_sets: number;
	catalog_snapshots: number;
	price_observations: number;
	balance_observations: number;
	discoveries: number;
	collection_attempts: number;
	request_evidence: number;
	auth_capabilities: number;
	pairing_sessions: number;
	manifest_bindings: number;
	collection_leases: number;
	activities_detached: number;
	generated_at: string;
}

export interface DeleteSupplierInput {
	confirm_name: string;
	impact_hash: string;
}

export interface RequestEvidence {
	id: number;
	attempt_id: number;
	supplier_id: number;
	endpoint_type: string;
	auth_capability: string;
	method: string;
	source_url: string;
	http_status?: number;
	status: string;
	error_class?: string;
	error_message?: string;
	duration_ms: number;
	schema_version?: string;
	response_hash?: string;
	observed_at: string;
}

export interface CollectionAttempt {
	id: number;
	supplier_id: number;
	status: 'success' | 'partial_success' | 'failed' | 'cancelled';
	error_class?: string;
	error_message?: string;
	catalog_updated: boolean;
	balance_updated: boolean;
	started_at: string;
	finished_at: string;
	duration_ms: number;
	requests: RequestEvidence[];
}

export interface AuthCapabilityEvidence {
	supplier_id: number;
	capability: string;
	status: string;
	credential_id?: number;
	endpoint_type?: string;
	http_status?: number;
	duration_ms?: number;
	error_class?: string;
	error_message?: string;
	last_attempt_at: string;
	verified_at?: string;
}

export interface UpdateSupplierInput {
	name: string;
	base_url: string;
	adapter: string;
	pricing_path: string;
	balance_path: string;
	credential_id: number | null;
	enabled: boolean;
	poll_interval_seconds: number;
	recharge_value_factor: string;
	focus_groups: string[];
	focus_models: string[];
	metadata: Record<string, string>;
}

export interface RawCredential {
	id: number;
	name: string;
	kind: string;
	site_url: string;
	expires_at?: string;
	status?: string;
	created_at?: string;
	updated_at?: string;
}

export interface CreateCredentialInput {
	name: string;
	kind: 'api-key' | 'access-token' | 'site-cookie';
	site_url: string;
	payload: {
		authorization?: string;
		authorization_mode?: 'bearer' | 'raw' | 'header';
		authorization_header?: string;
		cookie?: string;
		headers?: Record<string, string>;
		expires_at?: string;
	};
}

export interface PriceCalculation {
	source_ratio: string;
	cost_ratio: string;
	risk_adjusted_cost: string;
	target_ratio: string;
	profit_basis: string;
	basis_profit: string;
	nominal_profit: string;
	cash_profit: string;
	contribution_profit: string;
	markup_rate?: string;
	margin_rate?: string;
	subsidy_rate?: string;
	automation_eligible: boolean;
	automation_reason?: string;
	warnings: string[];
	formula: string;
}

export interface ListingChannelSpec {
	name: string;
	type: number;
	base_url: string;
	models: string[];
	test_model: string;
	priority?: number;
	weight?: number;
	auto_ban: boolean;
	model_mapping?: string;
	tag?: string;
}

export interface ListingPricingRule {
	create_pricing_mapping?: boolean;
	create_group_mirror?: boolean;
	mirror_execution_mode?: 'auto' | 'approval';
	mirror_policy_uuid?: string;
	price_anchor?: 'source_base' | 'effective_sale';
	group_ratio_rule_type?: PolicyMode | 'same';
	group_ratio_rule_value?: string;
	model_price_rule_type?: 'same' | 'add' | 'multiply' | 'fixed' | PolicyMode;
	model_price_rule_value?: string;
	rule_type: PolicyMode;
	value: string;
	execution_mode?: string;
	profit_basis?: 'contribution' | 'cash' | 'nominal';
	revenue_cash_factor?: string;
	cost_cash_factor?: string;
	risk_buffer_rate?: string;
	allow_negative_profit?: boolean;
	subsidy_budget_daily?: string;
	minimum_ratio?: string;
	maximum_ratio?: string;
	rounding_step?: string;
	model_price_rounding_step?: string;
	only_follow_increase?: boolean;
	only_follow_decrease?: boolean;
	sync_group_ratio?: boolean;
	sync_model_prices?: boolean;
	sync_billing_mode?: boolean;
	sync_model_additions?: boolean;
	sync_model_removals?: boolean;
	model_scope_mode?: 'mirror_group' | 'fixed_allowlist';
	empty_group_action?: 'preserve' | 'disable_managed_channel' | 'remove_managed_models';
	mirror_excluded_models?: string[];
}

export interface GroupModelTaskProfile {
	unit: 'per_request' | 'per_second' | 'fixed_duration' | string;
	fixed_duration_seconds?: number;
}

export interface GroupModelPricingSetting {
	billing_mode: 'fixed_price' | 'ratio' | 'tiered_expr' | 'inherit' | string;
	model_price?: string;
	model_ratio?: string;
	completion_ratio?: string;
	cache_ratio?: string;
	cache_creation_ratio?: string;
	cache_creation_5m_ratio?: string;
	cache_creation_1h_ratio?: string;
	image_ratio?: string;
	audio_ratio?: string;
	audio_completion_ratio?: string;
	billing_expr?: string;
	task_profile?: GroupModelTaskProfile;
	source?: string;
	source_revision?: string;
	source_observed_at_unix_ms?: number;
}

export interface GroupModelPricingContract {
	schema_version: number;
	revision: string;
	groups: Record<string, Record<string, GroupModelPricingSetting>>;
}

export interface TargetFieldDiff {
	field: string;
	before?: string;
	after?: string;
	kind: string;
}

export interface ListingPriceDimensionEvidence {
	dimension: string;
	source_unit_price: string;
	source_cash_cost: string;
	risk_adjusted_cash_cost: string;
	desired_effective_sale: string;
	target_base_price: string;
	effective_sale: string;
	revenue_cash_value: string;
	margin_rate: string;
	margin_defined: boolean;
}

export interface ListingModelPricingEvidence {
	model: string;
	price_anchor: 'source_base' | 'effective_sale' | string;
	billing_mode: string;
	billing_unit: string;
	fixed_duration_seconds?: number;
	currency: string;
	raw_source_group_ratio: string;
	source_cash_group_ratio: string;
	recharge_value_factor: string;
	target_group_ratio: string;
	source_evidence_hash: string;
	source_observed_at: string;
	setting: GroupModelPricingSetting;
	dimensions: ListingPriceDimensionEvidence[];
	minimum_margin_rate: string;
	minimum_margin_defined: boolean;
	warnings?: string[];
}

export interface ListingGroupVisibilityPlan {
	required: boolean;
	supported: boolean;
	changed: boolean;
	expected_hash?: string;
	desired_hash?: string;
	expected?: Record<string, string>;
	desired?: Record<string, string>;
}

export interface ListingGroupVisibilityReceipt {
	expected_hash?: string;
	desired_hash?: string;
	applied_hash?: string;
	before?: Record<string, string>;
	after?: Record<string, string>;
	changed: boolean;
	rollback_status?: string;
}

export interface ListingGroupModelPricingPlan {
	mode?: 'atomic_contract' | 'legacy_group_ratio' | string;
	supported: boolean;
	exact: boolean;
	rounding_step?: string;
	price_anchor?: 'source_base' | 'effective_sale' | string;
	group_ratio_rule_type?: string;
	group_ratio_rule_value?: string;
	model_price_rule_type?: string;
	model_price_rule_value?: string;
	target_group?: string;
	target_group_ratio?: string;
	expected_revision?: string;
	desired_revision?: string;
	expected_contract?: GroupModelPricingContract;
	desired_contract?: GroupModelPricingContract;
	diff?: TargetFieldDiff[];
	models?: ListingModelPricingEvidence[];
	excluded_models?: string[];
	exclusion_reasons?: Record<string, string>;
	minimum_margin_rate?: string;
	minimum_margin_defined: boolean;
	blocking_reasons?: string[];
	legacy_fallback_reason?: string;
	group_visibility?: ListingGroupVisibilityPlan;
}

export interface ListingGroupModelPricingReceipt {
	mode?: string;
	expected_revision?: string;
	desired_revision?: string;
	applied_revision?: string;
	before?: GroupModelPricingContract;
	after?: GroupModelPricingContract;
	diff?: TargetFieldDiff[];
	changed: boolean;
	rollback_status?: string;
	group_visibility?: ListingGroupVisibilityReceipt;
	requested_models?: string[];
	effective_models?: string[];
	excluded_models?: string[];
	discovery_status?: string;
}

export interface ListingPlanRequest {
	supplier_id: number;
	discovery_kind: 'group' | 'catalog_group';
	discovery_external_id: string;
	source_snapshot_hash?: string;
	expected_source_ratio?: string;
	confirm_catalog_group?: boolean;
	target_site_id: number;
	target_group_mode: 'new' | 'existing';
	target_group: string;
	upstream_credential_id: number;
	channel: ListingChannelSpec;
	pricing_rule: ListingPricingRule;
	idempotency_key: string;
}

export interface ListingPlan extends ListingPlanRequest {
	id: number;
	source_snapshot_hash: string;
	source_observed_at: string;
	source_group: string;
	source_ratio: string;
	equivalent_cost_ratio: string;
	cost_ratio_vector: Record<string, string>;
	target_catalog_hash: string;
	profit_verified: boolean;
	profit_verification: string;
	expected_target_hash: string;
	expected_group_ratios: Record<string, string>;
	desired_target_hash: string;
	expected_group_existed: boolean;
	expected_group_ratio?: string;
	proposed_group_ratio: string;
	calculation: PriceCalculation;
	group_model_pricing: ListingGroupModelPricingPlan;
	status: string;
	error?: string;
	created_at: string;
	updated_at: string;
}

export interface ListingChangeSet {
	id: number;
	plan_id: number;
	target_site_id: number;
	status: string;
	step: string;
	idempotency_key: string;
	ownership_marker: string;
	expected_target_hash: string;
	applied_target_hash?: string;
	created_channel_id?: number;
	pricing_policy_id?: number;
	policy_created?: boolean;
	policy_initially_on?: boolean;
	policy_activated?: boolean;
	ratio_changed: boolean;
	channel_enabled: boolean;
	rollback_status?: string;
	group_model_pricing: ListingGroupModelPricingReceipt;
	error?: string;
	started_at?: string;
	finished_at?: string;
	created_at: string;
	updated_at: string;
}

export interface ApplyListingPlanInput {
	idempotency_key: string;
	confirm_enable: true;
}

export interface TargetSite {
  id: number;
  name: string;
  base_url: string;
  adapter: string;
	credential_id: number;
  enabled: boolean;
  write_mode: string;
	last_status: string;
	last_error?: string;
	created_at?: string;
	updated_at?: string;
}

export interface TargetDeletionImpact {
	target_site_id: number;
	target_name: string;
	base_url: string;
	impact_hash: string;
	credential_id: number;
	credential_preserved: boolean;
	credential_supplier_refs: number;
	credential_target_refs: number;
	pricing_policies: number;
	change_proposals: number;
	decision_records: number;
	listing_plans: number;
	listing_change_sets: number;
	control_operations: number;
	active_leases: number;
	ledger_sync_records: number;
	ledger_events: number;
	opportunity_records: number;
	generated_at: string;
}

export interface DeleteTargetInput {
	confirm_name: string;
	impact_hash: string;
}

export interface PricingPolicyDeletionImpact {
	policy_id: number;
	policy_name: string;
	target_site_id: number;
	target_group: string;
	impact_hash: string;
	change_proposals: number;
	decision_records: number;
	listing_references: number;
	active_leases: number;
	generated_at: string;
}

export interface DeletePricingPolicyInput {
	confirm_name: string;
	impact_hash: string;
}

export interface ConfigurationReference {
	id: number;
	name: string;
}

export interface CredentialDeletionImpact {
	credential_id: number;
	credential_name: string;
	kind: string;
	site_url: string;
	impact_hash: string;
	supplier_refs: ConfigurationReference[];
	target_refs: ConfigurationReference[];
	listing_plan_refs: number;
	renewal_configured: boolean;
	generated_at: string;
}

export interface DeleteCredentialInput {
	confirm_name: string;
	impact_hash: string;
}

export interface UpsertTargetInput {
	name: string;
	base_url: string;
	adapter: string;
	credential_id: number;
	enabled: boolean;
	write_mode: string;
}

export interface ConnectTargetInput {
	name: string;
	base_url: string;
	adapter: 'newapi';
	write_mode: 'approval' | 'auto';
	auth_method: 'password' | 'token';
	username?: string;
	password?: string;
	token?: string;
	user_id?: number;
}

export interface ConnectTargetResult {
	target: TargetSite;
	credential: RawCredential;
	snapshot?: TargetSnapshot;
	connection_error?: string;
}

export interface TargetChannel {
	id: number;
	name: string;
	type: number;
	status: number;
	base_url: string;
	models: string[];
	group: string;
	priority: number;
	weight: number;
	auto_ban: boolean;
	model_mapping?: string;
	tag?: string;
	remark?: string;
	config_hash: string;
}

export interface CatalogGroup {
  external_id: string;
  name: string;
  ratio?: string;
	raw_ratio?: string;
	effective_ratio?: string;
	recharge_value_factor?: string;
  model_ids: string[];
}

export interface CatalogSnapshot {
  supplier_id: number;
  groups: CatalogGroup[];
  models: Array<{ external_id: string; name: string; group?: string }>;
	evidence?: {
		observed_at?: string;
		response_hash?: string;
		source_url?: string;
	};
}

export interface TargetSnapshot {
  target_site_id: number;
  group_ratio: Record<string, string>;
  hash: string;
	pricing_options?: Record<string, TargetPriceOption>;
	contract?: string;
	contract_known?: boolean;
	observed_at?: string;
	source_url?: string;
}

export interface MappingDraftPreviewInput {
	target_site_id: number;
	supplier_ids?: number[];
	minimum_score?: string;
	minimum_model_jaccard?: string;
	minimum_score_margin?: string;
	maximum_options?: number;
}

export interface MappingDraftThresholds {
	minimum_score: string;
	minimum_model_jaccard: string;
	minimum_score_margin: string;
}

export interface MappingDraftScoreFactor {
	key: string;
	weight: string;
	evidence: string;
	component: string;
}

export interface MappingDraftPolicyOwner {
	policy_id: number;
	supplier_id: number;
	source_group: string;
	enabled: boolean;
	execution_mode: string;
}

export interface MappingDraftMatchOption {
	target_group: string;
	target_ratio?: string;
	equivalent_cost_ratio?: string;
	pricing_basis?: 'model_dimension_equivalent' | 'group_ratio_fallback' | 'model_dimension_unresolved' | string;
	pricing_coverage_ratio?: string;
	pricing_impact_complete: boolean;
	pricing_impact_confidence?: string;
	pricing_unknown_dimensions?: string[];
	pricing_warnings?: string[];
	pricing_impact_error?: string;
	score: string;
	model_jaccard: string;
	family_jaccard: string;
	name_similarity: string;
	ratio_similarity?: string;
	origin_match: boolean;
	name_exact: boolean;
	matched_channel_ids?: number[];
	overlapping_models: string[];
	overlapping_models_omitted?: number;
	source_model_count: number;
	target_model_count: number;
	existing_owner?: MappingDraftPolicyOwner;
	factors: MappingDraftScoreFactor[];
	eligible_for_prefill: boolean;
	ineligible_reasons: string[];
}

export interface MappingDraftPolicy {
	name: string;
	supplier_id: number;
	source_group: string;
	source_model_pattern: string;
	target_site_id: number;
	target_group: string;
	rule_type: 'add';
	value: string;
	execution_mode: 'approval';
	enabled: false;
}

export interface MappingDraftCandidate {
	id: string;
	supplier_id: number;
	supplier_name: string;
	source_group: string;
	source_group_name: string;
	source_ratio?: string;
	raw_source_ratio?: string;
	recharge_value_factor?: string;
	source_models: number;
	source_snapshot_hash: string;
	source_observed_at: string;
	source_fresh: boolean;
	focused: boolean;
	status: 'prefilled' | 'needs_confirmation' | 'already_mapped' | 'conflict';
	confidence_level: 'high' | 'medium' | 'low' | 'explicit';
	score: string;
	source_runner_up_score: string;
	source_score_margin: string;
	score_is_best: boolean;
	proposed_target_group?: string;
	existing_policy_ids?: number[];
	policy?: MappingDraftPolicy;
	options: MappingDraftMatchOption[];
	reasons: string[];
	target_evidence_hash: string;
	target_competition: { contenders: number; next_score?: string; margin?: string };
}

export interface MappingDraftPreview {
	target_site_id: number;
	target_name: string;
	target_evidence_hash: string;
	target_catalog_hash: string;
	target_observed_at: string;
	thresholds: MappingDraftThresholds;
	summary: {
		suppliers_requested: number;
		suppliers_usable: number;
		source_groups: number;
		prefilled: number;
		needs_confirmation: number;
		already_mapped: number;
		conflicts: number;
	};
	candidates: MappingDraftCandidate[];
	supplier_issues: Array<{ supplier_id: number; supplier_name?: string; kind: string; message: string }>;
	generated_at: string;
}

export interface MappingDraftMaterializeInput {
	target_site_id: number;
	target_evidence_hash: string;
	selections: Array<{
		candidate_id: string;
		supplier_id: number;
		source_group: string;
		source_snapshot_hash: string;
		target_group: string;
		confirm_low_confidence?: boolean;
	}>;
}

export interface MappingDraftMaterializeResult {
	target_site_id: number;
	target_evidence_hash: string;
	policies: Array<{ candidate_id: string; created: boolean; idempotent: boolean; policy: RawPricingPolicy }>;
}

export interface ManualMappingDraftPreviewInput {
	target_site_id: number;
	supplier_ids?: number[];
}

export interface ManualMappingTargetChannel {
	id: number;
	name: string;
	status: number;
	base_origin?: string;
	models: string[];
	group: string;
	tag?: string;
	remark?: string;
	config_hash: string;
}

export interface ManualMappingSourceOption {
	candidate_id: string;
	supplier_id: number;
	supplier_name: string;
	source_group: string;
	source_group_name: string;
	source_ratio?: string;
	raw_source_ratio?: string;
	recharge_value_factor?: string;
	source_models: number;
	source_snapshot_hash: string;
	source_observed_at: string;
	source_fresh: boolean;
	score: string;
	model_jaccard: string;
	family_jaccard: string;
	name_similarity: string;
	ratio_similarity?: string;
	origin_match: boolean;
	matched_channel_ids?: number[];
	overlapping_models: string[];
	equivalent_cost_ratio?: string;
	pricing_basis?: string;
	pricing_coverage_ratio?: string;
	source_model_pattern: string;
	recommended_rule_type?: PolicyMode;
	recommended_value?: string;
	existing_policy_ids?: number[];
	reasons: string[];
}

export interface ManualMappingTargetGroup {
	target_group: string;
	target_ratio?: string;
	models: string[];
	channels: ManualMappingTargetChannel[];
	status: 'unmapped' | 'mapped' | 'conflict';
	existing_policy?: RawPricingPolicy;
	conflict_policy_ids?: number[];
	recommended_option_id?: string;
	options: ManualMappingSourceOption[];
}

export interface ManualMappingDraftPreview {
	target_site_id: number;
	target_name: string;
	target_evidence_hash: string;
	target_catalog_hash: string;
	target_observed_at: string;
	summary: {
		target_groups: number;
		target_channels: number;
		mapped: number;
		unmapped: number;
		conflicts: number;
		suppliers_requested: number;
		suppliers_usable: number;
	};
	groups: ManualMappingTargetGroup[];
	supplier_issues: Array<{ supplier_id: number; supplier_name?: string; kind: string; message: string }>;
	generated_at: string;
}

export interface ManualMappingRuleInput {
	name?: string;
	source_basis?: 'group_ratio' | 'equivalent_cost';
	rule_type: PolicyMode;
	value: string;
	profit_basis?: 'contribution' | 'cash' | 'nominal';
	revenue_cash_factor?: string;
	cost_cash_factor?: string;
	risk_buffer_rate?: string;
	allow_negative_profit?: boolean;
	subsidy_budget_daily?: string;
	minimum_ratio?: string;
	maximum_ratio?: string;
	rounding_step?: string;
	only_follow_increase?: boolean;
	only_follow_decrease?: boolean;
	enabled: boolean;
	execution_mode: 'approval' | 'auto';
	coverage_mode?: 'strict' | 'threshold' | 'owner_override';
	minimum_coverage_ratio?: string;
}

export interface ManualMappingDraftMaterializeInput {
	target_site_id: number;
	target_evidence_hash: string;
	selections: Array<{
		candidate_id: string;
		supplier_id: number;
		source_group: string;
		source_snapshot_hash: string;
		target_group: string;
		expected_policy_id?: number;
		expected_policy_updated_at?: string;
		confirm_manual: true;
		rule: ManualMappingRuleInput;
	}>;
}

export interface ManualMappingDraftMaterializeResult {
	target_site_id: number;
	target_evidence_hash: string;
	policies: Array<{
		target_group: string;
		created: boolean;
		updated: boolean;
		idempotent: boolean;
		policy: RawPricingPolicy;
	}>;
}

export interface TargetPriceOption {
	key: string;
	values: Record<string, string>;
	hash: string;
}

export interface TargetCapability {
	status: string;
	available: boolean;
	http_status?: number;
	reason?: string;
}

export interface TargetOptionCapability {
	key: string;
	read: TargetCapability;
	write: TargetCapability;
	hash?: string;
	count: number;
}

export interface TargetCapabilityMatrix {
	target_site_id: number;
	adapter: string;
	option_contract: string;
	channel_contract: string;
	read_only: boolean;
	read_only_reason?: string;
	pricing_options: Record<string, TargetOptionCapability>;
	channels: {
		read: TargetCapability;
		routing_write: TargetCapability;
		status_write: TargetCapability;
		billable_test: TargetCapability;
	};
	observed_at: string;
}

export interface TargetFieldDiff {
	field: string;
	before?: string;
	after?: string;
	kind: 'added' | 'changed' | 'deleted' | string;
}

export interface TargetPricingOptionPreview {
	target_site_id: number;
	option: string;
	expected_hash: string;
	desired_hash: string;
	preview_hash: string;
	current: Record<string, string>;
	desired: Record<string, string>;
	diff: TargetFieldDiff[];
	deletions: string[];
	no_change: boolean;
	observed_at: string;
}

export interface TargetChannelRoutingUpdate {
	group?: string;
	priority?: number;
	weight?: number;
	auto_ban?: boolean;
}

export interface TargetChannelPreview {
	target_site_id: number;
	channel_id: number;
	action: string;
	expected_hash: string;
	desired_hash: string;
	preview_hash: string;
	before: TargetChannel;
	after: TargetChannel;
	diff: TargetFieldDiff[];
	no_change: boolean;
	observed_at: string;
}

export interface TargetControlOperation {
	id: number;
	target_site_id: number;
	dimension: string;
	action: string;
	idempotency_key: string;
	expected_hash?: string;
	desired_hash?: string;
	readback_hash?: string;
	result_state: string;
	fencing_token: number;
	error?: string;
	started_at: string;
	finished_at?: string;
	updated_at: string;
	result?: unknown;
}

export interface ReauthorizeTargetInput {
	auth_method: 'password' | 'token';
	token?: string;
	user_id?: number;
	username?: string;
	password?: string;
	expected_target_updated_at: string;
	expected_credential_id: number;
	expected_credential_updated_at: string;
}

export interface CreateSupplierInput {
  name: string;
  base_url: string;
  adapter: 'newapi' | 'sub2api' | 'json';
  poll_interval_seconds: number;
	recharge_value_factor: string;
  collect_now: true;
  enabled: true;
  focus_groups: string[];
  focus_models: string[];
  metadata: Record<string, string>;
}

export interface CreatePricingPolicyInput {
  name: string;
  supplier_id: number;
  source_group: string;
  source_model_pattern: string;
  source_basis?: 'group_ratio' | 'equivalent_cost';
  target_site_id: number;
  target_group: string;
  rule_type: PolicyMode;
  value: string;
  profit_basis: 'contribution' | 'cash' | 'nominal';
  revenue_cash_factor: string;
  cost_cash_factor: string;
  risk_buffer_rate: string;
  allow_negative_profit: boolean;
  subsidy_budget_daily: string;
  minimum_ratio: string;
  maximum_ratio: string;
  rounding_step: string;
  only_follow_increase: boolean;
  only_follow_decrease: boolean;
  enabled: boolean;
  execution_mode: 'approval' | 'auto';
  coverage_mode: 'strict' | 'threshold' | 'owner_override';
  minimum_coverage_ratio: string;
}

export interface RawPricingPolicy extends CreatePricingPolicyInput {
  id: number;
	created_at?: string;
	updated_at?: string;
}

export interface RawChangeProposal {
	id: number;
	policy_id: number;
	target_site_id: number;
	target_group: string;
	expected_old: string;
	proposed_value: string;
	status: string;
	mode: string;
	readback_value?: string;
	error?: string;
	calculation: PolicySimulationResult['calculation'];
}

export interface PolicySimulationResult {
	policy: RawPricingPolicy;
	source_group: CatalogGroup;
	equivalent_cost_ratio: string;
	cost_ratio_vector: Record<string, string>;
	comparable_dimensions: number;
	total_dimensions: number;
	coverage_ratio: string;
	impact_complete: boolean;
	impact_confidence: 'high' | 'partial' | 'low' | 'unavailable';
	unknown_dimensions: string[];
	current_ratio: string;
	warnings: string[];
	calculation: {
		source_ratio: string;
		cost_ratio: string;
		equivalent_cost_ratio?: string;
		target_ratio: string;
		margin_rate?: string;
		automation_eligible: boolean;
		automation_reason?: string;
		warnings: string[];
	};
}

export interface ConnectorSpecInput {
  name: string;
  schema_version: string;
  spec_json: string;
  enabled: boolean;
}

export interface ConnectorSpecRecord extends ConnectorSpecInput {
	id: number;
	created_at: string;
	updated_at: string;
}

export interface UpdateConnectorSpecInput extends ConnectorSpecInput {
	expected_updated_at: string;
}

export interface ConnectorSpecDeletionImpact {
	connector_spec_id: number;
	name: string;
	impact_hash: string;
	supplier_refs: ConfigurationReference[];
	generated_at: string;
}

export interface ConnectorManifestDeletionImpact {
	manifest_id: number;
	name: string;
	impact_hash: string;
	supplier_refs: ConfigurationReference[];
	generated_at: string;
}

export interface DeleteConnectorObjectInput {
	confirm_name: string;
	impact_hash: string;
}

export type ConfigImportMode = 'add' | 'overwrite' | 'copy';

export interface PortableCredentialReference {
	name: string;
	kind: string;
	site_url: string;
}

export interface PortableSupplier {
	name: string;
	base_url: string;
	adapter: string;
	pricing_path?: string;
	balance_path?: string;
	credential?: PortableCredentialReference;
	connector_spec?: string;
	enabled: boolean;
	poll_interval_seconds: number;
	recharge_value_factor?: string;
	focus_groups: string[];
	focus_models: string[];
	metadata?: Record<string, string>;
}

export interface PortableTarget {
	name: string;
	base_url: string;
	adapter: string;
	credential: PortableCredentialReference;
	enabled: boolean;
	write_mode: string;
}

export interface PortablePricingPolicy {
	name: string;
	supplier: { name: string; base_url: string };
	source_group?: string;
	source_model_pattern: string;
	source_basis?: 'group_ratio' | 'equivalent_cost';
	target: { name: string; base_url: string };
	target_group: string;
	rule_type: string;
	value: string;
	profit_basis: string;
	revenue_cash_factor: string;
	cost_cash_factor: string;
	risk_buffer_rate: string;
	allow_negative_profit: boolean;
	subsidy_budget_daily: string;
	minimum_ratio: string;
	maximum_ratio: string;
	rounding_step: string;
	only_follow_increase: boolean;
	only_follow_decrease: boolean;
	enabled: boolean;
	execution_mode: string;
	coverage_mode: string;
	minimum_coverage_ratio: string;
}

export interface PortableConnectorSpec {
	name: string;
	schema_version: string;
	spec: unknown;
	enabled: boolean;
	disabled_reasons?: string[];
}

export interface PortableConfig {
	schema_version: string;
	exported_at?: string;
	credential_refs: PortableCredentialReference[];
	suppliers: PortableSupplier[];
	targets: PortableTarget[];
	pricing_policies: PortablePricingPolicy[];
	connector_specs: PortableConnectorSpec[];
	settings: Array<{ key: string; value: unknown }>;
}

export interface PortableConfigExport {
	config_hash: string;
	bundle: PortableConfig;
}

export interface ConfigObjectAction {
	object_type: string;
	object_key: string;
	action: string;
	conflict: boolean;
	enabled: boolean;
	reasons?: string[];
	resolved_object?: string;
}

export interface ConfigImportSummary {
	create: number;
	overwrite: number;
	reuse: number;
	copy: number;
	disabled: number;
	unresolved: number;
	skipped: number;
}

export interface ConfigImportPreview {
	id: number;
	schema_version: string;
	mode: ConfigImportMode;
	preview_hash: string;
	expected_config_hash: string;
	bundle_hash: string;
	actions: ConfigObjectAction[];
	summary: ConfigImportSummary;
	warnings: string[];
	expires_at: string;
	created_at: string;
}

export interface ConfigPendingObject {
	object_type: string;
	object_key: string;
	reason: string;
}

export interface ConfigImportRun {
	id: number;
	preview_id: number;
	mode: ConfigImportMode;
	status: string;
	before_hash: string;
	after_hash: string;
	version_id: number;
	before_version_id: number;
	summary: ConfigImportSummary;
	actions: ConfigObjectAction[];
	pending_objects: ConfigPendingObject[];
	warnings: string[];
	created_at: string;
	rolled_back_at?: string;
}
