package domain

import "time"

type Supplier struct {
	ID                  int64             `json:"id"`
	Name                string            `json:"name"`
	BaseURL             string            `json:"base_url"`
	Adapter             string            `json:"adapter"`
	PricingPath         string            `json:"pricing_path"`
	BalancePath         string            `json:"balance_path"`
	CredentialID        *int64            `json:"credential_id,omitempty"`
	Enabled             bool              `json:"enabled"`
	PollIntervalSeconds int               `json:"poll_interval_seconds"`
	RechargeValueFactor string            `json:"recharge_value_factor"`
	FocusGroups         []string          `json:"focus_groups"`
	FocusModels         []string          `json:"focus_models"`
	Metadata            map[string]string `json:"metadata"`
	LastStatus          string            `json:"last_status"`
	LastError           string            `json:"last_error,omitempty"`
	LastErrorClass      string            `json:"last_error_class,omitempty"`
	LastAttemptAt       *time.Time        `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time        `json:"last_success_at,omitempty"`
	LastSuccessDuration *int64            `json:"last_success_duration_ms,omitempty"`
	LastFailureDuration *int64            `json:"last_failure_duration_ms,omitempty"`
	LastCollectedAt     *time.Time        `json:"last_collected_at,omitempty"`
	NextCollectionAt    *time.Time        `json:"next_collection_at,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type SupplierDeletionImpact struct {
	SupplierID          int64     `json:"supplier_id"`
	SupplierName        string    `json:"supplier_name"`
	BaseURL             string    `json:"base_url"`
	ImpactHash          string    `json:"impact_hash"`
	CredentialID        *int64    `json:"credential_id,omitempty"`
	CredentialPreserved bool      `json:"credential_preserved"`
	PricingPolicies     int64     `json:"pricing_policies"`
	ChangeProposals     int64     `json:"change_proposals"`
	ListingPlans        int64     `json:"listing_plans"`
	ListingChangeSets   int64     `json:"listing_change_sets"`
	CatalogSnapshots    int64     `json:"catalog_snapshots"`
	PriceObservations   int64     `json:"price_observations"`
	BalanceObservations int64     `json:"balance_observations"`
	Discoveries         int64     `json:"discoveries"`
	CollectionAttempts  int64     `json:"collection_attempts"`
	RequestEvidence     int64     `json:"request_evidence"`
	AuthCapabilities    int64     `json:"auth_capabilities"`
	PairingSessions     int64     `json:"pairing_sessions"`
	ManifestBindings    int64     `json:"manifest_bindings"`
	CollectionLeases    int64     `json:"collection_leases"`
	ActivitiesDetached  int64     `json:"activities_detached"`
	GeneratedAt         time.Time `json:"generated_at"`
}

type AutomationControl struct {
	Enabled                  bool      `json:"enabled"`
	BackgroundWorkersEnabled bool      `json:"background_workers_enabled"`
	RemoteWritesEnabled      bool      `json:"remote_writes_enabled"`
	UpdatedAt                time.Time `json:"updated_at"`
}

type TargetDeletionImpact struct {
	TargetSiteID           int64     `json:"target_site_id"`
	TargetName             string    `json:"target_name"`
	BaseURL                string    `json:"base_url"`
	ImpactHash             string    `json:"impact_hash"`
	CredentialID           int64     `json:"credential_id"`
	CredentialPreserved    bool      `json:"credential_preserved"`
	CredentialSupplierRefs int64     `json:"credential_supplier_refs"`
	CredentialTargetRefs   int64     `json:"credential_target_refs"`
	PricingPolicies        int64     `json:"pricing_policies"`
	ChangeProposals        int64     `json:"change_proposals"`
	DecisionRecords        int64     `json:"decision_records"`
	ListingPlans           int64     `json:"listing_plans"`
	ListingChangeSets      int64     `json:"listing_change_sets"`
	ControlOperations      int64     `json:"control_operations"`
	ActiveLeases           int64     `json:"active_leases"`
	LedgerSyncRecords      int64     `json:"ledger_sync_records"`
	LedgerEvents           int64     `json:"ledger_events"`
	OpportunityRecords     int64     `json:"opportunity_records"`
	GeneratedAt            time.Time `json:"generated_at"`
}

type PricingPolicyDeletionImpact struct {
	PolicyID          int64     `json:"policy_id"`
	PolicyName        string    `json:"policy_name"`
	TargetSiteID      int64     `json:"target_site_id"`
	TargetGroup       string    `json:"target_group"`
	ImpactHash        string    `json:"impact_hash"`
	ChangeProposals   int64     `json:"change_proposals"`
	DecisionRecords   int64     `json:"decision_records"`
	ListingReferences int64     `json:"listing_references"`
	ActiveLeases      int64     `json:"active_leases"`
	GeneratedAt       time.Time `json:"generated_at"`
}

type ConfigurationReference struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CredentialDeletionImpact struct {
	CredentialID      int64                    `json:"credential_id"`
	CredentialName    string                   `json:"credential_name"`
	Kind              string                   `json:"kind"`
	SiteURL           string                   `json:"site_url"`
	ImpactHash        string                   `json:"impact_hash"`
	SupplierRefs      []ConfigurationReference `json:"supplier_refs"`
	TargetRefs        []ConfigurationReference `json:"target_refs"`
	ListingPlanRefs   int64                    `json:"listing_plan_refs"`
	RenewalConfigured bool                     `json:"renewal_configured"`
	GeneratedAt       time.Time                `json:"generated_at"`
}

type ConnectorSpecDeletionImpact struct {
	ConnectorSpecID int64                    `json:"connector_spec_id"`
	Name            string                   `json:"name"`
	ImpactHash      string                   `json:"impact_hash"`
	SupplierRefs    []ConfigurationReference `json:"supplier_refs"`
	GeneratedAt     time.Time                `json:"generated_at"`
}

type ConnectorManifestDeletionImpact struct {
	ManifestID   int64                    `json:"manifest_id"`
	Name         string                   `json:"name"`
	ImpactHash   string                   `json:"impact_hash"`
	SupplierRefs []ConfigurationReference `json:"supplier_refs"`
	GeneratedAt  time.Time                `json:"generated_at"`
}

type Credential struct {
	ID        int64                   `json:"id"`
	Name      string                  `json:"name"`
	Kind      string                  `json:"kind"`
	SiteURL   string                  `json:"site_url"`
	ExpiresAt *time.Time              `json:"expires_at,omitempty"`
	Status    string                  `json:"status"`
	Renewal   CredentialRenewalStatus `json:"renewal"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

type CredentialPayload struct {
	UserID              int64             `json:"user_id,omitempty"`
	Authorization       string            `json:"authorization,omitempty"`
	AuthorizationMode   string            `json:"authorization_mode,omitempty"`
	AuthorizationHeader string            `json:"authorization_header,omitempty"`
	Cookie              string            `json:"cookie,omitempty"`
	CapturedOrigin      string            `json:"captured_origin,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	LocalStorage        map[string]string `json:"local_storage,omitempty"`
	SessionStorage      map[string]string `json:"session_storage,omitempty"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	ExpiresIn           int64             `json:"expires_in,omitempty"`
	Refresh             *RefreshRule      `json:"refresh,omitempty"`
	Login               *LoginRule        `json:"login,omitempty"`
}

type RefreshRule struct {
	URL                  string            `json:"url"`
	Method               string            `json:"method"`
	Headers              map[string]string `json:"headers,omitempty"`
	Body                 string            `json:"body,omitempty"`
	TokenJSONPath        string            `json:"token_json_path,omitempty"`
	RefreshTokenJSONPath string            `json:"refresh_token_json_path,omitempty"`
	ExpiresInJSONPath    string            `json:"expires_in_json_path,omitempty"`
	ExpiresAtJSONPath    string            `json:"expires_at_json_path,omitempty"`
	CookieResponse       bool              `json:"cookie_response,omitempty"`
}

// LoginRule is encrypted as part of CredentialPayload. It is deliberately
// declarative: no script, template language, SQL, or arbitrary response code
// can be stored or executed by the credential lifecycle worker.
type LoginRule struct {
	Enabled                    bool   `json:"enabled"`
	Username                   string `json:"username"`
	Password                   string `json:"password"`
	URL                        string `json:"url"`
	Method                     string `json:"method"`
	ContentType                string `json:"content_type"`
	UsernameField              string `json:"username_field"`
	PasswordField              string `json:"password_field"`
	TokenJSONPath              string `json:"token_json_path,omitempty"`
	UserIDJSONPath             string `json:"user_id_json_path,omitempty"`
	ExpiresInJSONPath          string `json:"expires_in_json_path,omitempty"`
	ExpiresAtJSONPath          string `json:"expires_at_json_path,omitempty"`
	SuccessJSONPath            string `json:"success_json_path,omitempty"`
	SuccessValue               string `json:"success_value,omitempty"`
	MessageJSONPath            string `json:"message_json_path,omitempty"`
	LoginRefreshTokenJSONPath  string `json:"login_refresh_token_json_path,omitempty"`
	RefreshURL                 string `json:"refresh_url,omitempty"`
	RefreshMethod              string `json:"refresh_method,omitempty"`
	RefreshTokenField          string `json:"refresh_token_field,omitempty"`
	RefreshResponseTokenPath   string `json:"refresh_response_token_json_path,omitempty"`
	RefreshResponseRefreshPath string `json:"refresh_response_refresh_token_json_path,omitempty"`
	RefreshResponseExpiresPath string `json:"refresh_response_expires_in_json_path,omitempty"`
	RefreshResponseExpiryPath  string `json:"refresh_response_expires_at_json_path,omitempty"`
	CaptchaJSONPath            string `json:"captcha_json_path,omitempty"`
	TwoFactorJSONPath          string `json:"two_factor_json_path,omitempty"`
	CookieResponse             bool   `json:"cookie_response,omitempty"`
	MinimumIntervalSeconds     int64  `json:"minimum_interval_seconds,omitempty"`
	InitialBackoffSeconds      int64  `json:"initial_backoff_seconds,omitempty"`
	MaximumBackoffSeconds      int64  `json:"maximum_backoff_seconds,omitempty"`
	MaximumConsecutiveFailures int    `json:"maximum_consecutive_failures,omitempty"`
	HumanActionCooldownSeconds int64  `json:"human_action_cooldown_seconds,omitempty"`
	AllowReplaceBrowserPairing bool   `json:"allow_replace_browser_pairing,omitempty"`
}

// CredentialRenewalStatus is the only renewal material exposed by list and
// status APIs. Login identifiers, passwords, cookies, and tokens never enter
// this structure.
type CredentialRenewalStatus struct {
	Configured          bool       `json:"configured"`
	Enabled             bool       `json:"enabled"`
	State               string     `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	NextAttemptAt       *time.Time `json:"next_attempt_at,omitempty"`
	HumanAction         string     `json:"human_action,omitempty"`
	LastErrorClass      string     `json:"last_error_class,omitempty"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
}

// BrowserCustodyStatus is secret-free lifecycle metadata for a browser-paired
// session. The raw custody token is held only by the browser extension; APIs,
// logs and configuration exports expose this status object instead.
type BrowserCustodyStatus struct {
	Enabled          bool       `json:"enabled"`
	SupplierID       int64      `json:"supplier_id"`
	CredentialID     int64      `json:"credential_id"`
	ExpectedOrigin   string     `json:"expected_origin"`
	ExpiresAt        time.Time  `json:"expires_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	LastCheckAt      *time.Time `json:"last_check_at,omitempty"`
	LastSubmissionAt *time.Time `json:"last_submission_at,omitempty"`
	LastSuccessAt    *time.Time `json:"last_success_at,omitempty"`
	SubmissionCount  int        `json:"submission_count"`
	LastErrorClass   string     `json:"last_error_class,omitempty"`
	LastErrorMessage string     `json:"last_error_message,omitempty"`
	HelperVersion    string     `json:"helper_version,omitempty"`
	MinimumVersion   string     `json:"minimum_helper_version,omitempty"`
	HelperState      string     `json:"helper_state,omitempty"`
	HelperOnline     bool       `json:"helper_online"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	NeedsCapture     bool       `json:"needs_capture"`
	NeedReason       string     `json:"need_reason,omitempty"`
	NeedSince        *time.Time `json:"need_since,omitempty"`
	RecoveryPending  bool       `json:"recovery_pending"`
	RecoveryOverdue  bool       `json:"recovery_overdue"`
}

type CredentialLoginRuleView struct {
	URL                        string `json:"url"`
	Method                     string `json:"method"`
	ContentType                string `json:"content_type"`
	UsernameField              string `json:"username_field"`
	PasswordField              string `json:"password_field"`
	TokenJSONPath              string `json:"token_json_path,omitempty"`
	UserIDJSONPath             string `json:"user_id_json_path,omitempty"`
	ExpiresInJSONPath          string `json:"expires_in_json_path,omitempty"`
	ExpiresAtJSONPath          string `json:"expires_at_json_path,omitempty"`
	SuccessJSONPath            string `json:"success_json_path,omitempty"`
	SuccessValue               string `json:"success_value,omitempty"`
	MessageJSONPath            string `json:"message_json_path,omitempty"`
	LoginRefreshTokenJSONPath  string `json:"login_refresh_token_json_path,omitempty"`
	RefreshURL                 string `json:"refresh_url,omitempty"`
	RefreshMethod              string `json:"refresh_method,omitempty"`
	RefreshTokenField          string `json:"refresh_token_field,omitempty"`
	RefreshResponseTokenPath   string `json:"refresh_response_token_json_path,omitempty"`
	RefreshResponseRefreshPath string `json:"refresh_response_refresh_token_json_path,omitempty"`
	RefreshResponseExpiresPath string `json:"refresh_response_expires_in_json_path,omitempty"`
	RefreshResponseExpiryPath  string `json:"refresh_response_expires_at_json_path,omitempty"`
	CaptchaJSONPath            string `json:"captcha_json_path,omitempty"`
	TwoFactorJSONPath          string `json:"two_factor_json_path,omitempty"`
	CookieResponse             bool   `json:"cookie_response"`
	MinimumIntervalSeconds     int64  `json:"minimum_interval_seconds"`
	InitialBackoffSeconds      int64  `json:"initial_backoff_seconds"`
	MaximumBackoffSeconds      int64  `json:"maximum_backoff_seconds"`
	MaximumConsecutiveFailures int    `json:"maximum_consecutive_failures"`
	HumanActionCooldownSeconds int64  `json:"human_action_cooldown_seconds"`
	AllowReplaceBrowserPairing bool   `json:"allow_replace_browser_pairing"`
}

type CredentialRenewalDetail struct {
	CredentialID       int64                    `json:"credential_id"`
	Status             CredentialRenewalStatus  `json:"status"`
	UsernameConfigured bool                     `json:"username_configured"`
	PasswordConfigured bool                     `json:"password_configured"`
	Rule               *CredentialLoginRuleView `json:"rule,omitempty"`
}

// CredentialRenewalPreset is a public, secret-free adapter recommendation.
// It contains only declarative request and response mappings; account names,
// passwords, tokens, cookies, and request headers are never part of a preset.
type CredentialRenewalPreset struct {
	SupplierID    int64                    `json:"supplier_id"`
	Adapter       string                   `json:"adapter"`
	AdapterFamily string                   `json:"adapter_family,omitempty"`
	Label         string                   `json:"label,omitempty"`
	Supported     bool                     `json:"supported"`
	Rule          *CredentialLoginRuleView `json:"rule,omitempty"`
}

type CatalogSnapshot struct {
	SupplierID int64         `json:"supplier_id"`
	Groups     []GroupOffer  `json:"groups"`
	Models     []ModelOffer  `json:"models"`
	Balance    *BalanceState `json:"balance,omitempty"`
	Evidence   Evidence      `json:"evidence"`
}

type GroupOffer struct {
	ExternalID          string            `json:"external_id"`
	Name                string            `json:"name"`
	Ratio               string            `json:"ratio,omitempty"`
	RawRatio            string            `json:"raw_ratio,omitempty"`
	EffectiveRatio      string            `json:"effective_ratio,omitempty"`
	RechargeValueFactor string            `json:"recharge_value_factor,omitempty"`
	ModelIDs            []string          `json:"model_ids"`
	Metadata            map[string]string `json:"metadata,omitempty"`
}

type ModelOffer struct {
	ExternalID      string            `json:"external_id"`
	Name            string            `json:"name"`
	Group           string            `json:"group,omitempty"`
	BillingMode     string            `json:"billing_mode"`
	ModelRatio      string            `json:"model_ratio,omitempty"`
	CompletionRatio string            `json:"completion_ratio,omitempty"`
	CacheRatio      string            `json:"cache_ratio,omitempty"`
	ModelPrice      string            `json:"model_price,omitempty"`
	BillingExpr     string            `json:"billing_expr,omitempty"`
	Currency        string            `json:"currency,omitempty"`
	PriceFunction   PriceFunction     `json:"price_function,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// PriceFunction keeps normalized monetary dimensions separate from source
// multipliers. Empty values mean unknown, while an explicit "0" means free.
type PriceFunction struct {
	Currency               string            `json:"currency,omitempty"`
	Basis                  string            `json:"basis,omitempty"`
	InputPer1M             string            `json:"input_per_1m,omitempty"`
	OutputPer1M            string            `json:"output_per_1m,omitempty"`
	CacheReadPer1M         string            `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M        string            `json:"cache_write_per_1m,omitempty"`
	CacheWrite5MPer1M      string            `json:"cache_write_5m_per_1m,omitempty"`
	CacheWrite1HPer1M      string            `json:"cache_write_1h_per_1m,omitempty"`
	ImageInputPer1M        string            `json:"image_input_per_1m,omitempty"`
	AudioInputPer1M        string            `json:"audio_input_per_1m,omitempty"`
	AudioOutputPer1M       string            `json:"audio_output_per_1m,omitempty"`
	PerRequest             string            `json:"per_request,omitempty"`
	PerImage               string            `json:"per_image,omitempty"`
	PerSecond              string            `json:"per_second,omitempty"`
	PerFrame               string            `json:"per_frame,omitempty"`
	FixedFee               string            `json:"fixed_fee,omitempty"`
	GroupMultiplier        string            `json:"group_multiplier,omitempty"`
	Expression             string            `json:"expression,omitempty"`
	Dimensions             map[string]string `json:"dimensions,omitempty"`
	Derivation             string            `json:"derivation,omitempty"`
	RequiresModelBasePrice bool              `json:"requires_model_base_price,omitempty"`
}

type BalanceState struct {
	Amount               string     `json:"amount"`
	Currency             string     `json:"currency"`
	Available            string     `json:"available,omitempty"`
	DailySpend           string     `json:"daily_spend,omitempty"`
	HistoricalSpend      string     `json:"historical_spend,omitempty"`
	RequestCount         *int64     `json:"request_count,omitempty"`
	DailyRequestCount    *int64     `json:"daily_request_count,omitempty"`
	UsageSource          string     `json:"usage_source,omitempty"`
	UsageObservedAt      *time.Time `json:"usage_observed_at,omitempty"`
	RunwayHours          *string    `json:"runway_hours,omitempty"`
	Confidence           string     `json:"confidence"`
	ObservedAt           time.Time  `json:"observed_at"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	SourceField          string     `json:"source_field,omitempty"`
	RawValue             string     `json:"raw_value,omitempty"`
	RawUnit              string     `json:"raw_unit,omitempty"`
	NormalizationDivisor string     `json:"normalization_divisor,omitempty"`
	NormalizationFormula string     `json:"normalization_formula,omitempty"`
	RawDisplayText       string     `json:"raw_display_text,omitempty"`
}

type Evidence struct {
	SourceType   string    `json:"source_type"`
	SourceURL    string    `json:"source_url"`
	Adapter      string    `json:"adapter"`
	Confidence   int       `json:"confidence"`
	ObservedAt   time.Time `json:"observed_at"`
	ResponseHash string    `json:"response_hash"`
	Warnings     []string  `json:"warnings"`
}

type PriceObservation struct {
	ID           int64      `json:"id"`
	SupplierID   int64      `json:"supplier_id"`
	EntityType   string     `json:"entity_type"`
	EntityKey    string     `json:"entity_key"`
	GroupName    string     `json:"group_name,omitempty"`
	ModelName    string     `json:"model_name,omitempty"`
	Price        ModelOffer `json:"price"`
	Evidence     Evidence   `json:"evidence"`
	ObservedAt   time.Time  `json:"observed_at"`
	PreviousHash string     `json:"previous_hash,omitempty"`
}

type TargetSite struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	BaseURL      string    `json:"base_url"`
	Adapter      string    `json:"adapter"`
	CredentialID int64     `json:"credential_id"`
	Enabled      bool      `json:"enabled"`
	WriteMode    string    `json:"write_mode"`
	LastStatus   string    `json:"last_status"`
	LastError    string    `json:"last_error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type PricingPolicy struct {
	ID                  int64     `json:"id"`
	Name                string    `json:"name"`
	SupplierID          int64     `json:"supplier_id"`
	SourceGroup         string    `json:"source_group"`
	SourceModelPattern  string    `json:"source_model_pattern"`
	SourceBasis         string    `json:"source_basis"`
	TargetSiteID        int64     `json:"target_site_id"`
	TargetGroup         string    `json:"target_group"`
	RuleType            string    `json:"rule_type"`
	Value               string    `json:"value"`
	ProfitBasis         string    `json:"profit_basis"`
	RevenueCashFactor   string    `json:"revenue_cash_factor"`
	CostCashFactor      string    `json:"cost_cash_factor"`
	RiskBufferRate      string    `json:"risk_buffer_rate"`
	AllowNegativeProfit bool      `json:"allow_negative_profit"`
	SubsidyBudgetDaily  string    `json:"subsidy_budget_daily"`
	MinimumRatio        string    `json:"minimum_ratio"`
	MaximumRatio        string    `json:"maximum_ratio"`
	RoundingStep        string    `json:"rounding_step"`
	OnlyFollowIncrease  bool      `json:"only_follow_increase"`
	OnlyFollowDecrease  bool      `json:"only_follow_decrease"`
	Enabled             bool      `json:"enabled"`
	ExecutionMode       string    `json:"execution_mode"`
	CoverageMode        string    `json:"coverage_mode"`
	MinimumCoverage     string    `json:"minimum_coverage_ratio"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type PriceCalculation struct {
	SourceRatio         string   `json:"source_ratio"`
	RawSourceRatio      string   `json:"raw_source_ratio,omitempty"`
	RechargeValueFactor string   `json:"recharge_value_factor,omitempty"`
	CostRatio           string   `json:"cost_ratio"`
	EquivalentCostRatio string   `json:"equivalent_cost_ratio,omitempty"`
	SourceEvidenceHash  string   `json:"source_evidence_hash,omitempty"`
	EvidenceHash        string   `json:"evidence_hash,omitempty"`
	PolicyFingerprint   string   `json:"policy_fingerprint,omitempty"`
	InputFingerprint    string   `json:"input_fingerprint,omitempty"`
	RiskAdjustedCost    string   `json:"risk_adjusted_cost"`
	TargetRatio         string   `json:"target_ratio"`
	ProfitBasis         string   `json:"profit_basis"`
	BasisProfit         string   `json:"basis_profit"`
	NominalProfit       string   `json:"nominal_profit"`
	CashProfit          string   `json:"cash_profit"`
	ContributionProfit  string   `json:"contribution_profit"`
	MarkupRate          *string  `json:"markup_rate,omitempty"`
	MarginRate          *string  `json:"margin_rate,omitempty"`
	SubsidyRate         *string  `json:"subsidy_rate,omitempty"`
	AutomationEligible  bool     `json:"automation_eligible"`
	AutomationReason    string   `json:"automation_reason,omitempty"`
	Warnings            []string `json:"warnings"`
	Formula             string   `json:"formula"`
}

type ChangeProposal struct {
	ID                       int64                           `json:"id"`
	PolicyID                 int64                           `json:"policy_id"`
	TargetSiteID             int64                           `json:"target_site_id"`
	TargetGroup              string                          `json:"target_group"`
	ExpectedOld              string                          `json:"expected_old"`
	ProposedValue            string                          `json:"proposed_value"`
	ExpectedHash             string                          `json:"expected_hash"`
	Calculation              PriceCalculation                `json:"calculation"`
	GroupModelPricing        ListingGroupModelPricingPlan    `json:"group_model_pricing"`
	GroupModelPricingReceipt ListingGroupModelPricingReceipt `json:"group_model_pricing_receipt"`
	Status                   string                          `json:"status"`
	Mode                     string                          `json:"mode"`
	IdempotencyKey           string                          `json:"idempotency_key"`
	ReadbackValue            string                          `json:"readback_value,omitempty"`
	Error                    string                          `json:"error,omitempty"`
	CreatedAt                time.Time                       `json:"created_at"`
	UpdatedAt                time.Time                       `json:"updated_at"`
}

type Activity struct {
	ID         int64             `json:"id"`
	Kind       string            `json:"kind"`
	Severity   string            `json:"severity"`
	Title      string            `json:"title"`
	Message    string            `json:"message"`
	SupplierID *int64            `json:"supplier_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

type Dashboard struct {
	Suppliers          []Supplier         `json:"suppliers"`
	FocusedPrices      []PriceObservation `json:"focused_prices"`
	Balances           []BalanceState     `json:"balances"`
	PendingProposals   []ChangeProposal   `json:"pending_proposals"`
	RecentActivity     []Activity         `json:"recent_activity"`
	HealthySuppliers   int                `json:"healthy_suppliers"`
	AttentionSuppliers int                `json:"attention_suppliers"`
	PendingCount       int                `json:"pending_count"`
	Runway             []RunwayForecast   `json:"runway"`
	Discoveries        []DiscoveryItem    `json:"discoveries"`
	Savings            SavingsSummary     `json:"savings"`
}
