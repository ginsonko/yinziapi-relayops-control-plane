package domain

import "time"

const (
	PolicyBulkStrategyAtomic     = "atomic"
	PolicyBulkStrategyBestEffort = "best_effort"
)

// PricingPolicyBulkMutation describes a local policy-state change. It never
// proposes or applies a target price by itself.
type PricingPolicyBulkMutation struct {
	PolicyIDs     []int64 `json:"policy_ids"`
	ExecutionMode *string `json:"execution_mode,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
	Strategy      string  `json:"strategy,omitempty"`
	PreviewHash   string  `json:"preview_hash,omitempty"`
}

type PricingPolicyBulkItem struct {
	PolicyID            int64      `json:"policy_id"`
	PolicyName          string     `json:"policy_name,omitempty"`
	TargetSiteID        int64      `json:"target_site_id,omitempty"`
	TargetSiteName      string     `json:"target_site_name,omitempty"`
	TargetEnabled       bool       `json:"target_enabled,omitempty"`
	TargetWriteMode     string     `json:"target_write_mode,omitempty"`
	BeforeExecutionMode string     `json:"before_execution_mode,omitempty"`
	AfterExecutionMode  string     `json:"after_execution_mode,omitempty"`
	BeforeEnabled       bool       `json:"before_enabled,omitempty"`
	AfterEnabled        bool       `json:"after_enabled,omitempty"`
	Status              string     `json:"status"`
	Code                string     `json:"code,omitempty"`
	Message             string     `json:"message"`
	PolicyUpdatedAt     *time.Time `json:"policy_updated_at,omitempty"`
	TargetUpdatedAt     *time.Time `json:"target_updated_at,omitempty"`
}

type PricingPolicyBulkTargetCheck struct {
	TargetSiteID      int64  `json:"target_site_id"`
	TargetSiteName    string `json:"target_site_name"`
	PolicyCount       int    `json:"policy_count"`
	Enabled           bool   `json:"enabled"`
	WriteMode         string `json:"write_mode"`
	RequiresAutoWrite bool   `json:"requires_auto_write"`
	AutoWriteReady    bool   `json:"auto_write_ready"`
	Message           string `json:"message"`
}

type PricingPolicyBulkSummary struct {
	Selected             int `json:"selected"`
	Found                int `json:"found"`
	Changed              int `json:"changed"`
	Unchanged            int `json:"unchanged"`
	Blocked              int `json:"blocked"`
	TargetsAffected      int `json:"targets_affected"`
	ExecutionModeChanges int `json:"execution_mode_changes"`
	EnableChanges        int `json:"enable_changes"`
	DisableChanges       int `json:"disable_changes"`
	AutoWriteReady       int `json:"auto_write_ready"`
	AutoWriteBlocked     int `json:"auto_write_blocked"`
}

type PricingPolicyBulkPreview struct {
	ExecutionMode *string                        `json:"execution_mode,omitempty"`
	Enabled       *bool                          `json:"enabled,omitempty"`
	Strategy      string                         `json:"strategy"`
	Summary       PricingPolicyBulkSummary       `json:"summary"`
	Items         []PricingPolicyBulkItem        `json:"items"`
	Targets       []PricingPolicyBulkTargetCheck `json:"targets"`
	PreviewHash   string                         `json:"preview_hash"`
	GeneratedAt   time.Time                      `json:"generated_at"`
}

type PricingPolicyBulkResult struct {
	Status      string                         `json:"status"`
	Strategy    string                         `json:"strategy"`
	Summary     PricingPolicyBulkSummary       `json:"summary"`
	Items       []PricingPolicyBulkItem        `json:"items"`
	Targets     []PricingPolicyBulkTargetCheck `json:"targets"`
	PreviewHash string                         `json:"preview_hash"`
	AppliedAt   *time.Time                     `json:"applied_at,omitempty"`
}
