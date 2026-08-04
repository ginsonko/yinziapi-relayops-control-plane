package domain

import (
	"encoding/json"
	"time"
)

const PortableConfigSchemaVersion = "relayops-config/v1"

type CredentialReference struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	SiteURL string `json:"site_url"`
}

type PortableSupplier struct {
	Name                string               `json:"name"`
	BaseURL             string               `json:"base_url"`
	Adapter             string               `json:"adapter"`
	PricingPath         string               `json:"pricing_path,omitempty"`
	BalancePath         string               `json:"balance_path,omitempty"`
	Credential          *CredentialReference `json:"credential,omitempty"`
	ConnectorSpec       string               `json:"connector_spec,omitempty"`
	Enabled             bool                 `json:"enabled"`
	PollIntervalSeconds int                  `json:"poll_interval_seconds"`
	RechargeValueFactor string               `json:"recharge_value_factor,omitempty"`
	FocusGroups         []string             `json:"focus_groups"`
	FocusModels         []string             `json:"focus_models"`
	Metadata            map[string]string    `json:"metadata,omitempty"`
}

type PortableTarget struct {
	Name       string              `json:"name"`
	BaseURL    string              `json:"base_url"`
	Adapter    string              `json:"adapter"`
	Credential CredentialReference `json:"credential"`
	Enabled    bool                `json:"enabled"`
	WriteMode  string              `json:"write_mode"`
}

type PortableObjectReference struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

type PortablePricingPolicy struct {
	Name                string                  `json:"name"`
	Supplier            PortableObjectReference `json:"supplier"`
	SourceGroup         string                  `json:"source_group,omitempty"`
	SourceModelPattern  string                  `json:"source_model_pattern"`
	SourceBasis         string                  `json:"source_basis,omitempty"`
	Target              PortableObjectReference `json:"target"`
	TargetGroup         string                  `json:"target_group"`
	RuleType            string                  `json:"rule_type"`
	Value               string                  `json:"value"`
	ProfitBasis         string                  `json:"profit_basis"`
	RevenueCashFactor   string                  `json:"revenue_cash_factor"`
	CostCashFactor      string                  `json:"cost_cash_factor"`
	RiskBufferRate      string                  `json:"risk_buffer_rate"`
	AllowNegativeProfit bool                    `json:"allow_negative_profit"`
	SubsidyBudgetDaily  string                  `json:"subsidy_budget_daily"`
	MinimumRatio        string                  `json:"minimum_ratio"`
	MaximumRatio        string                  `json:"maximum_ratio"`
	RoundingStep        string                  `json:"rounding_step"`
	OnlyFollowIncrease  bool                    `json:"only_follow_increase"`
	OnlyFollowDecrease  bool                    `json:"only_follow_decrease"`
	Enabled             bool                    `json:"enabled"`
	ExecutionMode       string                  `json:"execution_mode"`
	CoverageMode        string                  `json:"coverage_mode"`
	MinimumCoverage     string                  `json:"minimum_coverage_ratio"`
}

type PortableConnectorSpec struct {
	Name            string          `json:"name"`
	SchemaVersion   string          `json:"schema_version"`
	Spec            json.RawMessage `json:"spec"`
	Enabled         bool            `json:"enabled"`
	DisabledReasons []string        `json:"disabled_reasons,omitempty"`
}

type PortableSetting struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type PortableConfig struct {
	SchemaVersion   string                  `json:"schema_version"`
	ExportedAt      *time.Time              `json:"exported_at,omitempty"`
	CredentialRefs  []CredentialReference   `json:"credential_refs"`
	Suppliers       []PortableSupplier      `json:"suppliers"`
	Targets         []PortableTarget        `json:"targets"`
	PricingPolicies []PortablePricingPolicy `json:"pricing_policies"`
	ConnectorSpecs  []PortableConnectorSpec `json:"connector_specs"`
	Settings        []PortableSetting       `json:"settings"`
}

type ConfigImportMode string

const (
	ConfigImportAdd       ConfigImportMode = "add"
	ConfigImportOverwrite ConfigImportMode = "overwrite"
	ConfigImportCopy      ConfigImportMode = "copy"
)

type ConfigImportPreviewRequest struct {
	Mode   ConfigImportMode `json:"mode"`
	Bundle PortableConfig   `json:"bundle"`
}

type ConfigObjectAction struct {
	ObjectType     string   `json:"object_type"`
	ObjectKey      string   `json:"object_key"`
	Action         string   `json:"action"`
	Conflict       bool     `json:"conflict"`
	Enabled        bool     `json:"enabled"`
	Reasons        []string `json:"reasons,omitempty"`
	ResolvedObject string   `json:"resolved_object,omitempty"`
}

type ConfigImportSummary struct {
	Create     int `json:"create"`
	Overwrite  int `json:"overwrite"`
	Reuse      int `json:"reuse"`
	Copy       int `json:"copy"`
	Disabled   int `json:"disabled"`
	Unresolved int `json:"unresolved"`
	Skipped    int `json:"skipped"`
}

type ConfigImportPreview struct {
	ID                 int64                `json:"id"`
	SchemaVersion      string               `json:"schema_version"`
	Mode               ConfigImportMode     `json:"mode"`
	PreviewHash        string               `json:"preview_hash"`
	ExpectedConfigHash string               `json:"expected_config_hash"`
	BundleHash         string               `json:"bundle_hash"`
	Actions            []ConfigObjectAction `json:"actions"`
	Summary            ConfigImportSummary  `json:"summary"`
	Warnings           []string             `json:"warnings"`
	ExpiresAt          time.Time            `json:"expires_at"`
	CreatedAt          time.Time            `json:"created_at"`
}

type ConfigImportApplyRequest struct {
	PreviewID   int64  `json:"preview_id"`
	PreviewHash string `json:"preview_hash"`
}

type ConfigImportRun struct {
	ID              int64                 `json:"id"`
	PreviewID       int64                 `json:"preview_id"`
	Mode            ConfigImportMode      `json:"mode"`
	Status          string                `json:"status"`
	BeforeHash      string                `json:"before_hash"`
	AfterHash       string                `json:"after_hash"`
	VersionID       int64                 `json:"version_id"`
	BeforeVersionID int64                 `json:"before_version_id"`
	Summary         ConfigImportSummary   `json:"summary"`
	Actions         []ConfigObjectAction  `json:"actions"`
	PendingObjects  []ConfigPendingObject `json:"pending_objects"`
	Warnings        []string              `json:"warnings"`
	CreatedAt       time.Time             `json:"created_at"`
	RolledBackAt    *time.Time            `json:"rolled_back_at,omitempty"`
}

type ConfigPendingObject struct {
	ObjectType string `json:"object_type"`
	ObjectKey  string `json:"object_key"`
	Reason     string `json:"reason"`
}
