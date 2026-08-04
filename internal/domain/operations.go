package domain

import "time"

type ConnectorSpec struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	SchemaVersion string    `json:"schema_version"`
	SpecJSON      string    `json:"spec_json,omitempty"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type PairingToken struct {
	Token      string     `json:"token,omitempty"`
	SupplierID int64      `json:"supplier_id"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
}

type TargetSnapshot struct {
	TargetSiteID               int64                        `json:"target_site_id"`
	GroupRatio                 map[string]string            `json:"group_ratio"`
	Hash                       string                       `json:"hash"`
	UserUsableGroups           map[string]string            `json:"user_usable_groups,omitempty"`
	UserUsableGroupsHash       string                       `json:"user_usable_groups_hash,omitempty"`
	UserUsableGroupsSupported  bool                         `json:"user_usable_groups_supported"`
	PricingOptions             map[string]TargetPriceOption `json:"pricing_options,omitempty"`
	GroupModelPricing          *GroupModelPricingContract   `json:"group_model_pricing,omitempty"`
	GroupModelPricingSupported bool                         `json:"group_model_pricing_supported"`
	GroupModelPricingError     string                       `json:"group_model_pricing_error,omitempty"`
	Contract                   string                       `json:"contract"`
	ContractKnown              bool                         `json:"contract_known"`
	ObservedAt                 time.Time                    `json:"observed_at"`
	SourceURL                  string                       `json:"source_url"`
}

// GroupModelPricingContract mirrors NewAPI's additive, atomic group-plus-model
// pricing contract. Decimal fields stay as canonical strings inside RelayOps;
// the target adapter emits them as JSON numbers only at the HTTP boundary.
type GroupModelPricingContract struct {
	SchemaVersion int                                            `json:"schema_version"`
	Revision      string                                         `json:"revision"`
	Groups        map[string]map[string]GroupModelPricingSetting `json:"groups"`
}

type GroupModelPricingSetting struct {
	BillingMode            string                 `json:"billing_mode"`
	ModelPrice             *string                `json:"model_price,omitempty"`
	ModelRatio             *string                `json:"model_ratio,omitempty"`
	CompletionRatio        *string                `json:"completion_ratio,omitempty"`
	CacheRatio             *string                `json:"cache_ratio,omitempty"`
	CacheCreationRatio     *string                `json:"cache_creation_ratio,omitempty"`
	CacheCreation5mRatio   *string                `json:"cache_creation_5m_ratio,omitempty"`
	CacheCreation1hRatio   *string                `json:"cache_creation_1h_ratio,omitempty"`
	ImageRatio             *string                `json:"image_ratio,omitempty"`
	AudioRatio             *string                `json:"audio_ratio,omitempty"`
	AudioCompletionRatio   *string                `json:"audio_completion_ratio,omitempty"`
	BillingExpr            string                 `json:"billing_expr,omitempty"`
	TaskProfile            *GroupModelTaskProfile `json:"task_profile,omitempty"`
	Source                 string                 `json:"source,omitempty"`
	SourceRevision         string                 `json:"source_revision,omitempty"`
	SourceObservedAtUnixMS int64                  `json:"source_observed_at_unix_ms,omitempty"`
}

type GroupModelTaskProfile struct {
	Unit                 string `json:"unit"`
	FixedDurationSeconds int    `json:"fixed_duration_seconds,omitempty"`
}

// TargetPriceOption is one explicitly allowlisted NewAPI pricing map. Hash is
// calculated independently so an administrator can update one option without
// overwriting concurrent changes to another option.
type TargetPriceOption struct {
	Key    string            `json:"key"`
	Values map[string]string `json:"values"`
	Hash   string            `json:"hash"`
}

type CollectionResult struct {
	Supplier    Supplier              `json:"supplier"`
	Snapshot    CatalogSnapshot       `json:"snapshot"`
	Attempt     CollectionAttempt     `json:"attempt"`
	Diagnostics CollectionDiagnostics `json:"diagnostics"`
	Changed     bool                  `json:"changed"`
	Duration    int64                 `json:"duration_ms"`
}

// CollectionAttempt records one complete collector run independently from the
// last trusted catalog snapshot. A failed attempt must never move LastSuccessAt.
type CollectionAttempt struct {
	ID             int64             `json:"id"`
	SupplierID     int64             `json:"supplier_id"`
	Status         string            `json:"status"`
	ErrorClass     string            `json:"error_class,omitempty"`
	ErrorMessage   string            `json:"error_message,omitempty"`
	CatalogUpdated bool              `json:"catalog_updated"`
	BalanceUpdated bool              `json:"balance_updated"`
	StartedAt      time.Time         `json:"started_at"`
	FinishedAt     time.Time         `json:"finished_at"`
	DurationMS     int64             `json:"duration_ms"`
	Requests       []RequestEvidence `json:"requests"`
}

// RequestEvidence is deliberately metadata-only. It stores no response body,
// request headers, cookies, tokens, passwords, or query strings.
type RequestEvidence struct {
	ID             int64     `json:"id"`
	AttemptID      int64     `json:"attempt_id"`
	SupplierID     int64     `json:"supplier_id"`
	EndpointType   string    `json:"endpoint_type"`
	AuthCapability string    `json:"auth_capability"`
	Method         string    `json:"method"`
	SourceURL      string    `json:"source_url"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	Status         string    `json:"status"`
	ErrorClass     string    `json:"error_class,omitempty"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	SchemaVersion  string    `json:"schema_version,omitempty"`
	ResponseHash   string    `json:"response_hash,omitempty"`
	ObservedAt     time.Time `json:"observed_at"`
}

// AuthCapabilityEvidence keeps the four authorization layers independent.
// Capturing a credential does not mark any capability as verified.
type AuthCapabilityEvidence struct {
	SupplierID    int64      `json:"supplier_id"`
	Capability    string     `json:"capability"`
	Status        string     `json:"status"`
	CredentialID  *int64     `json:"credential_id,omitempty"`
	EndpointType  string     `json:"endpoint_type,omitempty"`
	HTTPStatus    int        `json:"http_status,omitempty"`
	DurationMS    int64      `json:"duration_ms,omitempty"`
	ErrorClass    string     `json:"error_class,omitempty"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	LastAttemptAt time.Time  `json:"last_attempt_at"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
}

type CollectionDiagnostics struct {
	Requests         []RequestEvidence            `json:"requests"`
	Capabilities     []AuthCapabilityEvidence     `json:"capabilities"`
	CatalogSucceeded bool                         `json:"catalog_succeeded"`
	BalanceSucceeded bool                         `json:"balance_succeeded"`
	Warnings         []string                     `json:"warnings,omitempty"`
	CatalogProof     *CatalogCollectionProofBasis `json:"catalog_proof,omitempty"`
}

// CatalogCollectionProofBasis is connector-supplied structural evidence for
// one authoritative catalog traversal. The store assigns the durable attempt
// identity and computes each source-group semantic hash itself.
type CatalogCollectionProofBasis struct {
	AuthoritativeEndpoint  string                  `json:"authoritative_endpoint"`
	AuthenticationRequired bool                    `json:"authentication_required"`
	Authenticated          bool                    `json:"authenticated"`
	SourceRevision         string                  `json:"source_revision"`
	Pages                  []CatalogCollectionPage `json:"pages"`
	DeclaredPageCount      *int                    `json:"declared_page_count,omitempty"`
	DeclaredTotal          *int                    `json:"declared_total,omitempty"`
	RawItemCount           int                     `json:"raw_item_count"`
	ParsedItemCount        int                     `json:"parsed_item_count"`
	UniqueItemCount        int                     `json:"unique_item_count"`
	DroppedItemCount       int                     `json:"dropped_item_count"`
	UnitErrorCount         int                     `json:"unit_error_count"`
	AmbiguousIdentityCount int                     `json:"ambiguous_identity_count"`
	ObservedAt             time.Time               `json:"observed_at"`
}

type CatalogCollectionPage struct {
	PageNumber int    `json:"page_number"`
	ItemCount  int    `json:"item_count"`
	Terminal   bool   `json:"terminal"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type CatalogEvidenceStatus struct {
	SupplierID              int64     `json:"supplier_id"`
	ResponseHash            string    `json:"response_hash"`
	SourceType              string    `json:"source_type"`
	TotalObservations       int       `json:"total_observations"`
	ConsecutiveObservations int       `json:"consecutive_observations"`
	RequiredObservations    int       `json:"required_observations"`
	RemainingObservations   int       `json:"remaining_observations"`
	AutoEligible            bool      `json:"auto_eligible"`
	LastSeen                time.Time `json:"last_seen"`
}

type PolicySimulation struct {
	Policy               PricingPolicy         `json:"policy"`
	SourceGroup          GroupOffer            `json:"source_group"`
	MatchedModels        []string              `json:"matched_models"`
	EquivalentCostRatio  string                `json:"equivalent_cost_ratio"`
	CostRatioVector      map[string]string     `json:"cost_ratio_vector"`
	ComparableDimensions int                   `json:"comparable_dimensions"`
	TotalDimensions      int                   `json:"total_dimensions"`
	CoverageRatio        string                `json:"coverage_ratio"`
	ImpactComplete       bool                  `json:"impact_complete"`
	ImpactConfidence     string                `json:"impact_confidence"`
	UnknownDimensions    []string              `json:"unknown_dimensions"`
	CurrentRatio         string                `json:"current_ratio"`
	Calculation          PriceCalculation      `json:"calculation"`
	Evidence             CatalogEvidenceStatus `json:"evidence"`
	// EvidenceOnlyBlocked keeps the write gate strict while allowing a bounded
	// targeted confirmation pass after a real price change.
	EvidenceOnlyBlocked bool                         `json:"evidence_only_blocked"`
	TargetHash          string                       `json:"target_hash"`
	TargetCatalogHash   string                       `json:"target_catalog_hash"`
	Warnings            []string                     `json:"warnings"`
	GroupModelPricing   ListingGroupModelPricingPlan `json:"group_model_pricing"`
}

type RunwayForecast struct {
	SupplierID        int64     `json:"supplier_id"`
	SupplierName      string    `json:"supplier_name"`
	BalanceAmount     string    `json:"balance_amount"`
	Currency          string    `json:"currency"`
	DailySpend        string    `json:"daily_spend"`
	RunwayHours       *string   `json:"runway_hours,omitempty"`
	Confidence        string    `json:"confidence"`
	ObservedAt        time.Time `json:"observed_at"`
	NeedsAttention    bool      `json:"needs_attention"`
	AttentionReason   string    `json:"attention_reason,omitempty"`
	RawBalanceAmount  string    `json:"raw_balance_amount,omitempty"`
	RawDailySpend     string    `json:"raw_daily_spend,omitempty"`
	RawCurrency       string    `json:"raw_currency,omitempty"`
	CashFactor        string    `json:"cash_factor,omitempty"`
	ConversionFormula string    `json:"conversion_formula,omitempty"`
}

type DiscoveryItem struct {
	SupplierID     int64      `json:"supplier_id"`
	SupplierName   string     `json:"supplier_name"`
	Kind           string     `json:"kind"`
	ExternalID     string     `json:"external_id"`
	Name           string     `json:"name"`
	Ratio          string     `json:"ratio,omitempty"`
	ModelCount     int        `json:"model_count"`
	FirstSeenAt    time.Time  `json:"first_seen_at"`
	LastSeenAt     time.Time  `json:"last_seen_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	Baseline       bool       `json:"baseline"`
}

type SavingsSummary struct {
	CollectionsToday  int    `json:"collections_today"`
	ChangesFound      int    `json:"changes_found"`
	ChangesApplied    int    `json:"changes_applied"`
	ManualChecksSaved int    `json:"manual_checks_saved"`
	AvoidedLoss       string `json:"avoided_loss"`
}

// ListingPricingRule is the immutable pricing input captured by a listing
// preview. The service converts it to PricingPolicy before using the shared
// pricing engine, so clients cannot submit a trusted "calculated" ratio.
type ListingPricingRule struct {
	CreatePricingMapping   *bool    `json:"create_pricing_mapping,omitempty"`
	CreateGroupMirror      *bool    `json:"create_group_mirror,omitempty"`
	MirrorExecutionMode    string   `json:"mirror_execution_mode,omitempty"`
	MirrorPolicyUUID       string   `json:"mirror_policy_uuid,omitempty"`
	PriceAnchor            string   `json:"price_anchor,omitempty"`
	GroupRatioRuleType     string   `json:"group_ratio_rule_type,omitempty"`
	GroupRatioRuleValue    string   `json:"group_ratio_rule_value,omitempty"`
	ModelPriceRuleType     string   `json:"model_price_rule_type,omitempty"`
	ModelPriceRuleValue    string   `json:"model_price_rule_value,omitempty"`
	RuleType               string   `json:"rule_type"`
	Value                  string   `json:"value"`
	ExecutionMode          string   `json:"execution_mode,omitempty"`
	ProfitBasis            string   `json:"profit_basis,omitempty"`
	RevenueCashFactor      string   `json:"revenue_cash_factor,omitempty"`
	CostCashFactor         string   `json:"cost_cash_factor,omitempty"`
	RiskBufferRate         string   `json:"risk_buffer_rate,omitempty"`
	AllowNegativeProfit    bool     `json:"allow_negative_profit,omitempty"`
	SubsidyBudgetDaily     string   `json:"subsidy_budget_daily,omitempty"`
	MinimumRatio           string   `json:"minimum_ratio,omitempty"`
	MaximumRatio           string   `json:"maximum_ratio,omitempty"`
	RoundingStep           string   `json:"rounding_step,omitempty"`
	ModelPriceRoundingStep string   `json:"model_price_rounding_step,omitempty"`
	OnlyFollowIncrease     bool     `json:"only_follow_increase,omitempty"`
	OnlyFollowDecrease     bool     `json:"only_follow_decrease,omitempty"`
	SyncGroupRatio         *bool    `json:"sync_group_ratio,omitempty"`
	SyncModelPrices        *bool    `json:"sync_model_prices,omitempty"`
	SyncBillingMode        *bool    `json:"sync_billing_mode,omitempty"`
	SyncModelAdditions     *bool    `json:"sync_model_additions,omitempty"`
	SyncModelRemovals      *bool    `json:"sync_model_removals,omitempty"`
	ModelScopeMode         string   `json:"model_scope_mode,omitempty"`
	EmptyGroupAction       string   `json:"empty_group_action,omitempty"`
	MirrorExcludedModels   []string `json:"mirror_excluded_models,omitempty"`
}

// ListingPriceDimensionEvidence freezes one comparable monetary dimension.
// SourceUnitPrice is the supplier catalog value before the supplier group and
// recharge factors. SourceCashCost and EffectiveSale are in the declared
// currency and are the values used to verify the realized margin.
type ListingPriceDimensionEvidence struct {
	Dimension            string `json:"dimension"`
	SourceUnitPrice      string `json:"source_unit_price"`
	SourceCashCost       string `json:"source_cash_cost"`
	RiskAdjustedCashCost string `json:"risk_adjusted_cash_cost"`
	DesiredEffectiveSale string `json:"desired_effective_sale"`
	TargetBasePrice      string `json:"target_base_price"`
	EffectiveSale        string `json:"effective_sale"`
	RevenueCashValue     string `json:"revenue_cash_value"`
	MarginRate           string `json:"margin_rate"`
	MarginDefined        bool   `json:"margin_defined"`
}

// ListingModelPricingEvidence is the immutable explanation for one generated
// group-model contract entry. It is intentionally verbose so preview, audit,
// rollback and operating intelligence do not need to reconstruct old prices
// from mutable catalogs.
type ListingModelPricingEvidence struct {
	Model                string                          `json:"model"`
	PriceAnchor          string                          `json:"price_anchor"`
	BillingMode          string                          `json:"billing_mode"`
	BillingUnit          string                          `json:"billing_unit"`
	FixedDurationSeconds int                             `json:"fixed_duration_seconds,omitempty"`
	Currency             string                          `json:"currency"`
	RawSourceGroupRatio  string                          `json:"raw_source_group_ratio"`
	SourceCashGroupRatio string                          `json:"source_cash_group_ratio"`
	RechargeValueFactor  string                          `json:"recharge_value_factor"`
	TargetGroupRatio     string                          `json:"target_group_ratio"`
	SourceEvidenceHash   string                          `json:"source_evidence_hash"`
	SourceObservedAt     time.Time                       `json:"source_observed_at"`
	Setting              GroupModelPricingSetting        `json:"setting"`
	Dimensions           []ListingPriceDimensionEvidence `json:"dimensions"`
	MinimumMarginRate    string                          `json:"minimum_margin_rate"`
	MinimumMarginDefined bool                            `json:"minimum_margin_defined"`
	Warnings             []string                        `json:"warnings,omitempty"`
}

// ListingGroupModelPricingPlan owns the complete before/after contract. Mode
// is atomic_contract for a capable NewAPI target and legacy_group_ratio only
// when the old representation is proven exact and isolated.
type ListingGroupModelPricingPlan struct {
	Mode                 string                        `json:"mode,omitempty"`
	PriceAnchor          string                        `json:"price_anchor,omitempty"`
	GroupRatioRuleType   string                        `json:"group_ratio_rule_type,omitempty"`
	GroupRatioRuleValue  string                        `json:"group_ratio_rule_value,omitempty"`
	ModelPriceRuleType   string                        `json:"model_price_rule_type,omitempty"`
	ModelPriceRuleValue  string                        `json:"model_price_rule_value,omitempty"`
	Supported            bool                          `json:"supported"`
	Exact                bool                          `json:"exact"`
	RoundingStep         string                        `json:"rounding_step,omitempty"`
	TargetGroup          string                        `json:"target_group,omitempty"`
	TargetGroupRatio     string                        `json:"target_group_ratio,omitempty"`
	ExpectedRevision     string                        `json:"expected_revision,omitempty"`
	DesiredRevision      string                        `json:"desired_revision,omitempty"`
	ExpectedContract     GroupModelPricingContract     `json:"expected_contract,omitempty"`
	DesiredContract      GroupModelPricingContract     `json:"desired_contract,omitempty"`
	Diff                 []TargetFieldDiff             `json:"diff,omitempty"`
	Models               []ListingModelPricingEvidence `json:"models,omitempty"`
	ExcludedModels       []string                      `json:"excluded_models,omitempty"`
	ExclusionReasons     map[string]string             `json:"exclusion_reasons,omitempty"`
	MinimumMarginRate    string                        `json:"minimum_margin_rate,omitempty"`
	MinimumMarginDefined bool                          `json:"minimum_margin_defined"`
	BlockingReasons      []string                      `json:"blocking_reasons,omitempty"`
	LegacyFallbackReason string                        `json:"legacy_fallback_reason,omitempty"`
	GroupVisibility      ListingGroupVisibilityPlan    `json:"group_visibility,omitempty"`
	GlobalModelPrice     ListingGlobalModelPricePlan   `json:"global_model_price,omitempty"`
}

// ListingGlobalModelPricePlan fills only absent legacy ModelPrice entries so
// NewAPI's default-group channel test can resolve newly listed fixed-price
// models. Existing global prices are never overwritten; per-group pricing
// remains the authoritative billing contract for the listed group.
type ListingGlobalModelPricePlan struct {
	Supported    bool              `json:"supported"`
	Changed      bool              `json:"changed"`
	ExpectedHash string            `json:"expected_hash,omitempty"`
	DesiredHash  string            `json:"desired_hash,omitempty"`
	Expected     map[string]string `json:"expected,omitempty"`
	Desired      map[string]string `json:"desired,omitempty"`
	Added        map[string]string `json:"added,omitempty"`
}

// ListingGroupVisibilityPlan freezes the exact NewAPI UserUsableGroups map
// needed to make a newly listed group selectable. It is embedded in the
// existing listing JSON so older databases require no schema migration.
type ListingGroupVisibilityPlan struct {
	Required     bool              `json:"required"`
	Supported    bool              `json:"supported"`
	Changed      bool              `json:"changed"`
	ExpectedHash string            `json:"expected_hash,omitempty"`
	DesiredHash  string            `json:"desired_hash,omitempty"`
	Expected     map[string]string `json:"expected,omitempty"`
	Desired      map[string]string `json:"desired,omitempty"`
}

// ListingGroupModelPricingReceipt records the independently read-back target
// state. Rollback is allowed only while AppliedRevision is still current.
type ListingGroupModelPricingReceipt struct {
	Mode             string                         `json:"mode,omitempty"`
	ExpectedRevision string                         `json:"expected_revision,omitempty"`
	DesiredRevision  string                         `json:"desired_revision,omitempty"`
	AppliedRevision  string                         `json:"applied_revision,omitempty"`
	Before           GroupModelPricingContract      `json:"before,omitempty"`
	After            GroupModelPricingContract      `json:"after,omitempty"`
	Diff             []TargetFieldDiff              `json:"diff,omitempty"`
	Changed          bool                           `json:"changed"`
	RollbackStatus   string                         `json:"rollback_status,omitempty"`
	GroupVisibility  ListingGroupVisibilityReceipt  `json:"group_visibility,omitempty"`
	GlobalModelPrice ListingGlobalModelPriceReceipt `json:"global_model_price,omitempty"`
	RequestedModels  []string                       `json:"requested_models,omitempty"`
	EffectiveModels  []string                       `json:"effective_models,omitempty"`
	ExcludedModels   []string                       `json:"excluded_models,omitempty"`
	DiscoveryStatus  string                         `json:"discovery_status,omitempty"`
}

type ListingGlobalModelPriceReceipt struct {
	ExpectedHash   string            `json:"expected_hash,omitempty"`
	DesiredHash    string            `json:"desired_hash,omitempty"`
	AppliedHash    string            `json:"applied_hash,omitempty"`
	Before         map[string]string `json:"before,omitempty"`
	After          map[string]string `json:"after,omitempty"`
	Added          map[string]string `json:"added,omitempty"`
	Changed        bool              `json:"changed"`
	RollbackStatus string            `json:"rollback_status,omitempty"`
}

type ListingGroupVisibilityReceipt struct {
	ExpectedHash   string            `json:"expected_hash,omitempty"`
	DesiredHash    string            `json:"desired_hash,omitempty"`
	AppliedHash    string            `json:"applied_hash,omitempty"`
	Before         map[string]string `json:"before,omitempty"`
	After          map[string]string `json:"after,omitempty"`
	Changed        bool              `json:"changed"`
	RollbackStatus string            `json:"rollback_status,omitempty"`
}

// ListingChannelSpec contains only channel fields that are deliberately
// supported by the listing workflow. The upstream key is referenced through
// ListingPlan.UpstreamCredentialID and is never serialized into a plan.
type ListingChannelSpec struct {
	Name         string   `json:"name"`
	Type         int      `json:"type"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	TestModel    string   `json:"test_model,omitempty"`
	Priority     int64    `json:"priority,omitempty"`
	Weight       uint     `json:"weight,omitempty"`
	AutoBan      bool     `json:"auto_ban"`
	ModelMapping string   `json:"model_mapping,omitempty"`
	Tag          string   `json:"tag,omitempty"`
}

type ListingPlanRequest struct {
	SupplierID           int64              `json:"supplier_id"`
	DiscoveryKind        string             `json:"discovery_kind"`
	DiscoveryExternalID  string             `json:"discovery_external_id"`
	SourceSnapshotHash   string             `json:"source_snapshot_hash,omitempty"`
	ExpectedSourceRatio  string             `json:"expected_source_ratio,omitempty"`
	ConfirmCatalogGroup  bool               `json:"confirm_catalog_group,omitempty"`
	TargetSiteID         int64              `json:"target_site_id"`
	TargetGroupMode      string             `json:"target_group_mode"`
	TargetGroup          string             `json:"target_group"`
	UpstreamCredentialID int64              `json:"upstream_credential_id"`
	Channel              ListingChannelSpec `json:"channel"`
	PricingRule          ListingPricingRule `json:"pricing_rule"`
	IdempotencyKey       string             `json:"idempotency_key"`
}

// ListingPlan is a read-only preview frozen against both the supplier catalog
// hash and the target GroupRatio hash observed at preview time.
type ListingPlan struct {
	ID                   int64                        `json:"id"`
	SupplierID           int64                        `json:"supplier_id"`
	DiscoveryKind        string                       `json:"discovery_kind"`
	DiscoveryExternalID  string                       `json:"discovery_external_id"`
	SourceSnapshotHash   string                       `json:"source_snapshot_hash"`
	SourceObservedAt     time.Time                    `json:"source_observed_at"`
	SourceGroup          string                       `json:"source_group"`
	SourceRatio          string                       `json:"source_ratio"`
	EquivalentCostRatio  string                       `json:"equivalent_cost_ratio"`
	CostRatioVector      map[string]string            `json:"cost_ratio_vector"`
	TargetCatalogHash    string                       `json:"target_catalog_hash"`
	ProfitVerified       bool                         `json:"profit_verified"`
	ProfitVerification   string                       `json:"profit_verification"`
	TargetSiteID         int64                        `json:"target_site_id"`
	TargetGroupMode      string                       `json:"target_group_mode"`
	TargetGroup          string                       `json:"target_group"`
	ExpectedTargetHash   string                       `json:"expected_target_hash"`
	ExpectedGroupRatios  map[string]string            `json:"expected_group_ratios"`
	DesiredTargetHash    string                       `json:"desired_target_hash"`
	ExpectedGroupExisted bool                         `json:"expected_group_existed"`
	ExpectedGroupRatio   string                       `json:"expected_group_ratio,omitempty"`
	ProposedGroupRatio   string                       `json:"proposed_group_ratio"`
	UpstreamCredentialID int64                        `json:"upstream_credential_id"`
	Channel              ListingChannelSpec           `json:"channel"`
	PricingRule          ListingPricingRule           `json:"pricing_rule"`
	Calculation          PriceCalculation             `json:"calculation"`
	GroupModelPricing    ListingGroupModelPricingPlan `json:"group_model_pricing"`
	Status               string                       `json:"status"`
	Error                string                       `json:"error,omitempty"`
	IdempotencyKey       string                       `json:"idempotency_key"`
	CreatedAt            time.Time                    `json:"created_at"`
	UpdatedAt            time.Time                    `json:"updated_at"`
}

type ListingChangeSet struct {
	ID                 int64                           `json:"id"`
	PlanID             int64                           `json:"plan_id"`
	TargetSiteID       int64                           `json:"target_site_id"`
	Status             string                          `json:"status"`
	Step               string                          `json:"step"`
	IdempotencyKey     string                          `json:"idempotency_key"`
	OwnershipMarker    string                          `json:"ownership_marker"`
	ExpectedTargetHash string                          `json:"expected_target_hash"`
	AppliedTargetHash  string                          `json:"applied_target_hash,omitempty"`
	CreatedChannelID   *int64                          `json:"created_channel_id,omitempty"`
	PricingPolicyID    *int64                          `json:"pricing_policy_id,omitempty"`
	PolicyCreated      bool                            `json:"policy_created"`
	PolicyInitiallyOn  bool                            `json:"policy_initially_on"`
	PolicyActivated    bool                            `json:"policy_activated"`
	RatioChanged       bool                            `json:"ratio_changed"`
	ChannelEnabled     bool                            `json:"channel_enabled"`
	RollbackStatus     string                          `json:"rollback_status,omitempty"`
	GroupModelPricing  ListingGroupModelPricingReceipt `json:"group_model_pricing"`
	Error              string                          `json:"error,omitempty"`
	StartedAt          *time.Time                      `json:"started_at,omitempty"`
	FinishedAt         *time.Time                      `json:"finished_at,omitempty"`
	CreatedAt          time.Time                       `json:"created_at"`
	UpdatedAt          time.Time                       `json:"updated_at"`
}

// TargetChannel is the safe, redacted channel shape used by target adapters.
type TargetChannel struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Type         int      `json:"type"`
	Status       int      `json:"status"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	Group        string   `json:"group"`
	Priority     int64    `json:"priority"`
	Weight       uint     `json:"weight"`
	AutoBan      bool     `json:"auto_ban"`
	ModelMapping string   `json:"model_mapping,omitempty"`
	Tag          string   `json:"tag,omitempty"`
	Remark       string   `json:"remark,omitempty"`
	ConfigHash   string   `json:"config_hash"`
}

// TargetManagedChannelState is the redacted CAS surface used by complete
// mirror automation. It intentionally contains no credential, URL, routing,
// mapping, provider, or ownership-marker fields.
type TargetManagedChannelState struct {
	ChannelID int64    `json:"channel_id"`
	Models    []string `json:"models"`
	Status    int      `json:"status"`
	Hash      string   `json:"hash"`
}

type TargetManagedChannelMutation struct {
	ResultState  string                    `json:"result_state"`
	ExpectedHash string                    `json:"expected_hash"`
	ReadbackHash string                    `json:"readback_hash,omitempty"`
	Changed      bool                      `json:"changed"`
	State        TargetManagedChannelState `json:"state"`
	Error        string                    `json:"error,omitempty"`
}

// TargetChannelRoutingUpdate deliberately excludes credentials, upstream
// URLs, model mappings and other sensitive channel fields.
type TargetChannelRoutingUpdate struct {
	Group    *string `json:"group,omitempty"`
	Priority *int64  `json:"priority,omitempty"`
	Weight   *uint   `json:"weight,omitempty"`
	AutoBan  *bool   `json:"auto_ban,omitempty"`
}

// TargetCapability describes one independently verified management action.
// Status is one of available, read_only, unauthorized, forbidden, unavailable
// or unknown_contract. It never exposes credential material.
type TargetCapability struct {
	Status     string `json:"status"`
	Available  bool   `json:"available"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type TargetOptionCapability struct {
	Key   string           `json:"key"`
	Read  TargetCapability `json:"read"`
	Write TargetCapability `json:"write"`
	Hash  string           `json:"hash,omitempty"`
	Count int              `json:"count"`
}

type TargetChannelCapabilities struct {
	Read         TargetCapability `json:"read"`
	RoutingWrite TargetCapability `json:"routing_write"`
	StatusWrite  TargetCapability `json:"status_write"`
	BillableTest TargetCapability `json:"billable_test"`
}

// TargetCapabilityMatrix is a read-only negotiation result. Write support is
// advertised only for response contracts RelayOps has explicitly tested.
type TargetCapabilityMatrix struct {
	TargetSiteID      int64                             `json:"target_site_id"`
	Adapter           string                            `json:"adapter"`
	OptionContract    string                            `json:"option_contract"`
	ChannelContract   string                            `json:"channel_contract"`
	ReadOnly          bool                              `json:"read_only"`
	ReadOnlyReason    string                            `json:"read_only_reason,omitempty"`
	PricingOptions    map[string]TargetOptionCapability `json:"pricing_options"`
	GroupModelPricing TargetGroupModelPricingCapability `json:"group_model_pricing"`
	Channels          TargetChannelCapabilities         `json:"channels"`
	ObservedAt        time.Time                         `json:"observed_at"`
}

type TargetGroupModelPricingCapability struct {
	Read          TargetCapability `json:"read"`
	Preview       TargetCapability `json:"preview"`
	Write         TargetCapability `json:"write"`
	SchemaVersion int              `json:"schema_version,omitempty"`
	Revision      string           `json:"revision,omitempty"`
}

type TargetFieldDiff struct {
	Field  string `json:"field"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Kind   string `json:"kind"`
}

type TargetPricingOptionPreview struct {
	TargetSiteID int64             `json:"target_site_id"`
	Option       string            `json:"option"`
	ExpectedHash string            `json:"expected_hash"`
	DesiredHash  string            `json:"desired_hash"`
	PreviewHash  string            `json:"preview_hash"`
	Current      map[string]string `json:"current"`
	Desired      map[string]string `json:"desired"`
	Diff         []TargetFieldDiff `json:"diff"`
	Deletions    []string          `json:"deletions"`
	NoChange     bool              `json:"no_change"`
	ObservedAt   time.Time         `json:"observed_at"`
}

type TargetGroupModelPricingPreview struct {
	TargetSiteID            int64                     `json:"target_site_id"`
	ExpectedRevision        string                    `json:"expected_revision"`
	CurrentRevision         string                    `json:"current_revision"`
	DesiredRevision         string                    `json:"desired_revision"`
	PreviewHash             string                    `json:"preview_hash"`
	ExpectedRevisionMatches bool                      `json:"expected_revision_matches"`
	NoChange                bool                      `json:"no_change"`
	Current                 GroupModelPricingContract `json:"current"`
	Desired                 GroupModelPricingContract `json:"desired"`
	Diff                    []TargetFieldDiff         `json:"diff"`
	ObservedAt              time.Time                 `json:"observed_at"`
}

type TargetChannelPreview struct {
	TargetSiteID int64             `json:"target_site_id"`
	ChannelID    int64             `json:"channel_id"`
	Action       string            `json:"action"`
	ExpectedHash string            `json:"expected_hash"`
	DesiredHash  string            `json:"desired_hash"`
	PreviewHash  string            `json:"preview_hash"`
	Before       TargetChannel     `json:"before"`
	After        TargetChannel     `json:"after"`
	Diff         []TargetFieldDiff `json:"diff"`
	NoChange     bool              `json:"no_change"`
	ObservedAt   time.Time         `json:"observed_at"`
}

// TargetControlOperation is the durable receipt for a controlled remote
// action. ResultState uses the six V2 terminal states, plus in_progress while a
// valid fencing lease owns the operation.
type TargetControlOperation struct {
	ID             int64      `json:"id"`
	TargetSiteID   int64      `json:"target_site_id"`
	Dimension      string     `json:"dimension"`
	Action         string     `json:"action"`
	IdempotencyKey string     `json:"idempotency_key"`
	RequestHash    string     `json:"request_hash"`
	ExpectedHash   string     `json:"expected_hash,omitempty"`
	DesiredHash    string     `json:"desired_hash,omitempty"`
	ReadbackHash   string     `json:"readback_hash,omitempty"`
	ResultState    string     `json:"result_state"`
	FencingToken   int64      `json:"fencing_token"`
	ResultJSON     string     `json:"-"`
	Error          string     `json:"error,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Result         any        `json:"result,omitempty"`
}

type TargetControlLease struct {
	TargetSiteID int64     `json:"target_site_id"`
	Dimension    string    `json:"dimension"`
	OwnerID      string    `json:"owner_id"`
	FencingToken int64     `json:"fencing_token"`
	LeasedUntil  time.Time `json:"leased_until"`
}

type TargetEffectivePricingEvidence struct {
	Status                string   `json:"status"`
	PricingHash           string   `json:"pricing_hash,omitempty"`
	Exact                 bool     `json:"exact"`
	ObservedGroups        int      `json:"observed_groups,omitempty"`
	MatchedGroups         int      `json:"matched_groups,omitempty"`
	ChangedGroupsVerified int      `json:"changed_groups_verified,omitempty"`
	ChangedGroupsRawOnly  []string `json:"changed_groups_raw_only,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

type TargetMutationResult struct {
	ResultState       string                          `json:"result_state"`
	ExpectedHash      string                          `json:"expected_hash,omitempty"`
	DesiredHash       string                          `json:"desired_hash,omitempty"`
	ReadbackHash      string                          `json:"readback_hash,omitempty"`
	PricingOption     *TargetPriceOption              `json:"pricing_option,omitempty"`
	GroupModelPricing *GroupModelPricingContract      `json:"group_model_pricing,omitempty"`
	Channel           *TargetChannel                  `json:"channel,omitempty"`
	EffectivePricing  *TargetEffectivePricingEvidence `json:"effective_pricing,omitempty"`
	Error             string                          `json:"error,omitempty"`
}
