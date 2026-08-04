package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// OperatingLedgerEvent is the immutable financial evidence accepted by the
// decision engine. Empty revenue or cost values are unknown, while "0" is an
// observed zero. The source system is expected to provide a stable EventID.
type OperatingLedgerEvent struct {
	EventID          string            `json:"event_id"`
	TargetSiteID     int64             `json:"target_site_id"`
	OccurredAt       time.Time         `json:"occurred_at"`
	EventKind        string            `json:"event_kind,omitempty"`
	Amount           string            `json:"amount,omitempty"`
	AmountEvidence   string            `json:"amount_evidence,omitempty"`
	ModelID          string            `json:"model_id,omitempty"`
	GroupName        string            `json:"group_name,omitempty"`
	UserSegment      string            `json:"user_segment,omitempty"`
	SubjectUserID    string            `json:"subject_user_id,omitempty"`
	SubjectTokenID   string            `json:"subject_token_id,omitempty"`
	SubjectRole      string            `json:"subject_role,omitempty"`
	IsTest           bool              `json:"is_test,omitempty"`
	IsInternal       bool              `json:"is_internal,omitempty"`
	RelatedEventID   string            `json:"related_event_id,omitempty"`
	Requests         int64             `json:"requests"`
	UsageWeight      string            `json:"usage_weight,omitempty"`
	UsageUnit        string            `json:"usage_unit,omitempty"`
	UsageEvidence    string            `json:"usage_evidence"`
	CashRevenue      string            `json:"cash_revenue,omitempty"`
	ProcurementCost  string            `json:"procurement_cost,omitempty"`
	Currency         string            `json:"currency,omitempty"`
	RevenueEvidence  string            `json:"revenue_evidence"`
	CostEvidence     string            `json:"cost_evidence"`
	Source           string            `json:"source"`
	EvidenceRevision string            `json:"evidence_revision,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	PayloadHash      string            `json:"payload_hash,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

// FillMissingUsageCost adds a derived route-cost observation to an existing
// immutable usage row. The source request evidence remains unchanged; a
// second calculation can never overwrite an already accepted cost.
func (s *Store) FillMissingUsageCost(ctx context.Context, eventID, cost, currency, evidence string, metadata map[string]string) (bool, error) {
	eventID, cost, currency, evidence = strings.TrimSpace(eventID), strings.TrimSpace(cost), strings.ToUpper(strings.TrimSpace(currency)), strings.ToLower(strings.TrimSpace(evidence))
	if eventID == "" || cost == "" || currency == "" || evidence == "" {
		return false, errors.New("cost enrichment requires event_id, cost, currency, and evidence")
	}
	if _, ok := parseNonNegativeDecimal(cost); !ok {
		return false, errors.New("derived procurement cost must be a non-negative decimal")
	}
	if evidence != "exact" && evidence != "observed" && evidence != "estimated" && evidence != "prior" {
		return false, errors.New("derived cost evidence is invalid")
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	return serializedWriteResult(ctx, s, func() (bool, error) {
		return s.fillMissingUsageCostOnce(ctx, eventID, cost, currency, evidence, metadata)
	})
}

func (s *Store) fillMissingUsageCostOnce(ctx context.Context, eventID, cost, currency, evidence string, metadata map[string]string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var targetSiteID int64
	var kind, currentCost, currentCurrency, existingMetadata string
	if err := tx.QueryRowContext(ctx, `SELECT target_site_id,event_kind,COALESCE(procurement_cost,''),COALESCE(currency,''),metadata_json FROM operating_ledger_events WHERE event_id=?`, eventID).
		Scan(&targetSiteID, &kind, &currentCost, &currentCurrency, &existingMetadata); err != nil {
		return false, err
	}
	if kind != "usage" {
		return false, errors.New("only usage events can receive procurement cost")
	}
	if strings.TrimSpace(currentCost) != "" {
		return false, nil
	}
	if strings.TrimSpace(currentCurrency) != "" && !strings.EqualFold(strings.TrimSpace(currentCurrency), currency) {
		return false, fmt.Errorf("derived procurement cost currency %s conflicts with existing event currency %s", currency, currentCurrency)
	}
	merged := map[string]string{}
	_ = json.Unmarshal([]byte(existingMetadata), &merged)
	for key, value := range metadata {
		if strings.TrimSpace(key) != "" && len(key) <= 120 && len(value) <= 1000 {
			merged[key] = value
		}
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE operating_ledger_events SET procurement_cost=?,currency=CASE WHEN currency IS NULL OR currency='' THEN ? ELSE currency END,cost_evidence=?,metadata_json=? WHERE event_id=? AND (procurement_cost IS NULL OR procurement_cost='')`, cost, currency, evidence, string(encoded), eventID)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed == 1 {
		if err := recordOperatingLedgerChangeTx(ctx, tx, eventID, targetSiteID, "cost_update", time.Now().UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed == 1, nil
}

type DecisionSnapshot struct {
	ProposalID      int64           `json:"proposal_id"`
	ModelVersion    string          `json:"model_version"`
	InputHash       string          `json:"input_hash"`
	LifecycleStatus string          `json:"lifecycle_status"`
	DecisionKind    string          `json:"decision_kind"`
	DecidedAt       time.Time       `json:"decided_at"`
	Payload         json.RawMessage `json:"payload"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type DecisionEffect struct {
	ProposalID    int64     `json:"proposal_id"`
	EffectiveAt   time.Time `json:"effective_at"`
	ReadbackHash  string    `json:"readback_hash"`
	ReadbackValue string    `json:"readback_value"`
	CreatedAt     time.Time `json:"created_at"`
}

type DecisionExposure struct {
	ProposalID   int64      `json:"proposal_id"`
	TargetSiteID int64      `json:"target_site_id"`
	TargetGroup  string     `json:"target_group"`
	EffectiveAt  time.Time  `json:"effective_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EndReason    string     `json:"end_reason,omitempty"`
	StartHash    string     `json:"start_hash"`
	EndHash      string     `json:"end_hash,omitempty"`
}

type DecisionOutcome struct {
	ProposalID        int64           `json:"proposal_id"`
	HorizonHours      int             `json:"horizon_hours"`
	DueAt             time.Time       `json:"due_at"`
	Status            string          `json:"status"`
	Prediction        json.RawMessage `json:"prediction"`
	Observation       json.RawMessage `json:"observation,omitempty"`
	AbsoluteError     string          `json:"absolute_error,omitempty"`
	AbsolutePctError  string          `json:"absolute_percentage_error,omitempty"`
	SignedError       string          `json:"signed_error,omitempty"`
	SymmetricPctError string          `json:"symmetric_percentage_error,omitempty"`
	ActualAbsolute    string          `json:"actual_absolute,omitempty"`
	BandHit           *bool           `json:"band_hit,omitempty"`
	IntervalHit       *bool           `json:"-"`
	MetricBasis       string          `json:"metric_basis,omitempty"`
	MetricUnit        string          `json:"metric_unit,omitempty"`
	ModelVersion      string          `json:"model_version,omitempty"`
	CoverageRatio     string          `json:"coverage_ratio,omitempty"`
	ExclusionReason   string          `json:"exclusion_reason,omitempty"`
	ObservedAt        *time.Time      `json:"observed_at,omitempty"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type LedgerSummary struct {
	EventCount             int64      `json:"event_count"`
	ExactRevenueEvents     int64      `json:"exact_revenue_events"`
	ExactCostEvents        int64      `json:"exact_cost_events"`
	EstimatedRevenueEvents int64      `json:"estimated_revenue_events"`
	EstimatedCostEvents    int64      `json:"estimated_cost_events"`
	EarliestAt             *time.Time `json:"earliest_at,omitempty"`
	LatestAt               *time.Time `json:"latest_at,omitempty"`
}

// LedgerExclusionPolicy changes reporting and learning views only. Raw ledger
// evidence remains append-only and is never deleted when this policy changes.
type LedgerExclusionPolicy struct {
	ExcludeAdministrators bool     `json:"exclude_administrators"`
	ExcludeTestUsers      bool     `json:"exclude_test_users"`
	ExcludeInternalTokens bool     `json:"exclude_internal_tokens"`
	ExcludedUserSegments  []string `json:"excluded_user_segments,omitempty"`
	ExcludedUserIDs       []string `json:"excluded_user_ids,omitempty"`
	ExcludedTokenIDs      []string `json:"excluded_token_ids,omitempty"`
}

type LedgerFinancialReportOptions struct {
	Start      time.Time
	End        time.Time
	Exclusions LedgerExclusionPolicy
}

type LedgerCoverageSummary struct {
	Status                  string    `json:"status"`
	WindowStart             time.Time `json:"window_start"`
	WindowEnd               time.Time `json:"window_end"`
	Cohorts                 int       `json:"cohorts"`
	VerifiedCohorts         int       `json:"verified_cohorts"`
	ConflictingWatermarks   int       `json:"conflicting_watermarks"`
	MissingWatermarkCohorts int       `json:"missing_watermark_cohorts"`
	Note                    string    `json:"note"`
}

type LedgerCurrencySummary struct {
	Currency                    string `json:"currency"`
	UsageEventCount             int    `json:"usage_event_count"`
	MatchedUsageEventCount      int    `json:"matched_usage_event_count"`
	CashFlowEventCount          int    `json:"cash_flow_event_count"`
	UsageRevenue                string `json:"usage_revenue"`
	UsageProcurementCost        string `json:"usage_procurement_cost"`
	MatchedUsageRevenue         string `json:"matched_usage_revenue"`
	MatchedUsageProcurementCost string `json:"matched_usage_procurement_cost"`
	UsageGrossProfit            string `json:"usage_gross_profit,omitempty"`
	UsageGrossMargin            string `json:"usage_gross_margin,omitempty"`
	UsageProfitAvailable        bool   `json:"usage_profit_available"`
	RechargeGross               string `json:"recharge_gross"`
	Refunds                     string `json:"refunds"`
	PaymentFees                 string `json:"payment_fees"`
	ProcurementPayments         string `json:"procurement_payments"`
	GiftCreditIssued            string `json:"gift_credit_issued"`
	OtherIncome                 string `json:"other_income"`
	OtherExpense                string `json:"other_expense"`
	CommissionAwardEvents       int    `json:"commission_award_events"`
	CommissionReversalEvents    int    `json:"commission_reversal_events"`
	CommissionExpense           string `json:"commission_expense"`
	CommissionReversal          string `json:"commission_reversal"`
	CashNetFlow                 string `json:"cash_net_flow,omitempty"`
	CashNetFlowAvailable        bool   `json:"cash_net_flow_available"`
	UnknownUsageRevenueEvents   int    `json:"unknown_usage_revenue_events"`
	UnknownUsageCostEvents      int    `json:"unknown_usage_cost_events"`
	UnknownCashFlowAmountEvents int    `json:"unknown_cash_flow_amount_events"`
	EvidenceQuality             string `json:"evidence_quality"`
}

type LedgerSegmentSummary struct {
	UserSegment           string   `json:"user_segment"`
	Currency              string   `json:"currency"`
	Included              bool     `json:"included"`
	ExclusionReasons      []string `json:"exclusion_reasons,omitempty"`
	EventCount            int      `json:"event_count"`
	Requests              int64    `json:"requests"`
	RevenueKnownEvents    int      `json:"revenue_known_events"`
	CostKnownEvents       int      `json:"cost_known_events"`
	KnownRevenue          string   `json:"known_revenue,omitempty"`
	KnownCost             string   `json:"known_cost,omitempty"`
	RevenueCoverage       string   `json:"revenue_coverage"`
	CostCoverage          string   `json:"cost_coverage"`
	Contribution          string   `json:"contribution,omitempty"`
	ContributionAvailable bool     `json:"contribution_available"`
	RequiresLedgerFields  []string `json:"requires_ledger_fields,omitempty"`
}

type LedgerDimensionSummary struct {
	Key                  string   `json:"key"`
	Label                string   `json:"label"`
	Currency             string   `json:"currency"`
	EventCount           int      `json:"event_count"`
	Requests             int64    `json:"requests"`
	RevenueKnownEvents   int      `json:"revenue_known_events"`
	CostKnownEvents      int      `json:"cost_known_events"`
	KnownRevenue         string   `json:"known_revenue,omitempty"`
	KnownCost            string   `json:"known_cost,omitempty"`
	RevenueCoverage      string   `json:"revenue_coverage"`
	CostCoverage         string   `json:"cost_coverage"`
	GrossProfit          string   `json:"gross_profit,omitempty"`
	GrossMargin          string   `json:"gross_margin,omitempty"`
	ProfitAvailable      bool     `json:"profit_available"`
	RequiresLedgerFields []string `json:"requires_ledger_fields,omitempty"`
}

type LedgerPricingContractSummary struct {
	Key                         string   `json:"key"`
	GroupName                   string   `json:"group_name"`
	ModelID                     string   `json:"model_id"`
	PricingScope                string   `json:"pricing_scope"`
	PricingRevision             string   `json:"pricing_revision,omitempty"`
	BillingMode                 string   `json:"billing_mode"`
	BillingUnit                 string   `json:"billing_unit"`
	FixedDurationSeconds        string   `json:"fixed_duration_seconds,omitempty"`
	Currency                    string   `json:"currency"`
	EventCount                  int      `json:"event_count"`
	Requests                    int64    `json:"requests"`
	RevenueKnownEvents          int      `json:"revenue_known_events"`
	CostKnownEvents             int      `json:"cost_known_events"`
	ContractEvidenceKnownEvents int      `json:"contract_evidence_known_events"`
	KnownRevenue                string   `json:"known_revenue,omitempty"`
	KnownCost                   string   `json:"known_cost,omitempty"`
	RevenueCoverage             string   `json:"revenue_coverage"`
	CostCoverage                string   `json:"cost_coverage"`
	ContractEvidenceCoverage    string   `json:"contract_evidence_coverage"`
	GrossProfit                 string   `json:"gross_profit,omitempty"`
	GrossMargin                 string   `json:"gross_margin,omitempty"`
	ProfitAvailable             bool     `json:"profit_available"`
	RequiresLedgerFields        []string `json:"requires_ledger_fields,omitempty"`
}

type LedgerFinancialSummary struct {
	WindowStart                 time.Time                      `json:"window_start"`
	WindowEnd                   time.Time                      `json:"window_end"`
	RawEventCount               int                            `json:"raw_event_count"`
	IncludedEventCount          int                            `json:"included_event_count"`
	IncludedUsageEventCount     int                            `json:"included_usage_event_count"`
	MatchedUsageEventCount      int                            `json:"matched_usage_event_count"`
	ExcludedEventCount          int                            `json:"excluded_event_count"`
	ExcludedByReason            map[string]int                 `json:"excluded_by_reason"`
	ExcludedReasonCountsOverlap bool                           `json:"excluded_reason_counts_overlap"`
	UnclassifiedSubjectEvents   int                            `json:"unclassified_subject_events"`
	UnknownCurrencyEvents       int                            `json:"unknown_currency_events"`
	UnknownUsageCurrencyEvents  int                            `json:"unknown_usage_currency_events"`
	UnknownAmountEvents         int                            `json:"unknown_amount_events"`
	UnknownUsageRevenueEvents   int                            `json:"unknown_usage_revenue_events"`
	UnknownUsageCostEvents      int                            `json:"unknown_usage_cost_events"`
	UnknownCashFlowAmountEvents int                            `json:"unknown_cash_flow_amount_events"`
	ByCurrency                  []LedgerCurrencySummary        `json:"by_currency"`
	ExcludedByCurrency          []LedgerCurrencySummary        `json:"excluded_by_currency,omitempty"`
	UserSegments                []LedgerSegmentSummary         `json:"user_segments"`
	Groups                      []LedgerDimensionSummary       `json:"groups"`
	Models                      []LedgerDimensionSummary       `json:"models"`
	PricingContracts            []LedgerPricingContractSummary `json:"pricing_contracts"`
	CrossCurrencyAggregate      bool                           `json:"cross_currency_aggregate_available"`
	Coverage                    LedgerCoverageSummary          `json:"coverage"`
	Warnings                    []string                       `json:"warnings"`
}

func (s *Store) migrateIntelligence(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS operating_ledger_events (
			event_id TEXT PRIMARY KEY,
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			occurred_at TEXT NOT NULL,
			event_kind TEXT NOT NULL DEFAULT 'usage',
			amount TEXT,
			amount_evidence TEXT NOT NULL DEFAULT 'missing',
			model_id TEXT NOT NULL DEFAULT '',
			group_name TEXT NOT NULL DEFAULT '',
			user_segment TEXT NOT NULL DEFAULT '',
			subject_user_id TEXT NOT NULL DEFAULT '',
			subject_token_id TEXT NOT NULL DEFAULT '',
			subject_role TEXT NOT NULL DEFAULT '',
			is_test INTEGER NOT NULL DEFAULT 0,
			is_internal INTEGER NOT NULL DEFAULT 0,
			related_event_id TEXT NOT NULL DEFAULT '',
			requests INTEGER NOT NULL DEFAULT 0,
			usage_weight TEXT,
			usage_unit TEXT,
			usage_evidence TEXT NOT NULL DEFAULT 'missing',
			cash_revenue TEXT,
			procurement_cost TEXT,
			currency TEXT,
			revenue_evidence TEXT NOT NULL,
			cost_evidence TEXT NOT NULL,
			source TEXT NOT NULL,
			evidence_revision TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			payload_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_operating_ledger_target_time
			ON operating_ledger_events(target_site_id,occurred_at,group_name,model_id)`,
		`CREATE INDEX IF NOT EXISTS idx_operating_ledger_time
			ON operating_ledger_events(occurred_at,event_id)`,
		`CREATE TABLE IF NOT EXISTS operating_ledger_changes (
			revision INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL DEFAULT '',
			target_site_id INTEGER NOT NULL DEFAULT 0,
			change_kind TEXT NOT NULL,
			changed_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_operating_ledger_changes_target
			ON operating_ledger_changes(target_site_id,revision)`,
		`CREATE TABLE IF NOT EXISTS operating_ledger_cache_identity (
			id INTEGER PRIMARY KEY CHECK(id=1),
			instance_id TEXT NOT NULL
		)`,
		`INSERT OR IGNORE INTO operating_ledger_cache_identity(id,instance_id) VALUES(1,lower(hex(randomblob(16))))`,
		`CREATE TABLE IF NOT EXISTS decision_snapshots (
			proposal_id INTEGER PRIMARY KEY REFERENCES change_proposals(id) ON DELETE CASCADE,
			model_version TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			lifecycle_status TEXT NOT NULL,
			decision_kind TEXT NOT NULL DEFAULT 'legacy',
			decided_at TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_snapshots_status
			ON decision_snapshots(lifecycle_status,updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS decision_outcomes (
			proposal_id INTEGER NOT NULL REFERENCES change_proposals(id) ON DELETE CASCADE,
			horizon_hours INTEGER NOT NULL,
			due_at TEXT NOT NULL,
			status TEXT NOT NULL,
			prediction_json TEXT NOT NULL,
			observation_json TEXT,
			absolute_error TEXT,
			absolute_percentage_error TEXT,
			signed_error TEXT,
			symmetric_percentage_error TEXT,
			actual_absolute TEXT,
			interval_hit INTEGER,
			metric_basis TEXT,
			metric_unit TEXT,
			model_version TEXT,
			coverage_ratio TEXT,
			exclusion_reason TEXT,
			observed_at TEXT,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(proposal_id,horizon_hours)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_outcomes_status
			ON decision_outcomes(status,due_at)`,
		`CREATE TABLE IF NOT EXISTS decision_reconcile_attempts (
			proposal_id INTEGER PRIMARY KEY REFERENCES change_proposals(id) ON DELETE CASCADE,
			last_attempt_at TEXT NOT NULL,
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_reconcile_attempts_time
			ON decision_reconcile_attempts(last_attempt_at,proposal_id)`,
		`CREATE TABLE IF NOT EXISTS decision_effects (
			proposal_id INTEGER PRIMARY KEY REFERENCES change_proposals(id) ON DELETE CASCADE,
			effective_at TEXT NOT NULL,
			readback_hash TEXT NOT NULL,
			readback_value TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS decision_exposures (
			proposal_id INTEGER PRIMARY KEY REFERENCES change_proposals(id) ON DELETE CASCADE,
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			target_group TEXT NOT NULL,
			effective_at TEXT NOT NULL,
			ended_at TEXT,
			end_reason TEXT NOT NULL DEFAULT '',
			start_hash TEXT NOT NULL,
			end_hash TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_decision_exposures_scope
			ON decision_exposures(target_site_id,target_group,effective_at,ended_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply intelligence schema: %w", err)
		}
	}
	for _, column := range []struct{ table, name, definition string }{
		{"operating_ledger_events", "event_kind", "TEXT NOT NULL DEFAULT 'usage'"},
		{"operating_ledger_events", "amount", "TEXT"},
		{"operating_ledger_events", "amount_evidence", "TEXT NOT NULL DEFAULT 'missing'"},
		{"operating_ledger_events", "subject_user_id", "TEXT NOT NULL DEFAULT ''"},
		{"operating_ledger_events", "subject_token_id", "TEXT NOT NULL DEFAULT ''"},
		{"operating_ledger_events", "subject_role", "TEXT NOT NULL DEFAULT ''"},
		{"operating_ledger_events", "is_test", "INTEGER NOT NULL DEFAULT 0"},
		{"operating_ledger_events", "is_internal", "INTEGER NOT NULL DEFAULT 0"},
		{"operating_ledger_events", "related_event_id", "TEXT NOT NULL DEFAULT ''"},
		{"operating_ledger_events", "usage_unit", "TEXT"},
		{"operating_ledger_events", "usage_evidence", "TEXT NOT NULL DEFAULT 'missing'"},
		{"decision_snapshots", "decision_kind", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"decision_snapshots", "decided_at", "TEXT NOT NULL DEFAULT ''"},
		{"decision_outcomes", "signed_error", "TEXT"},
		{"decision_outcomes", "symmetric_percentage_error", "TEXT"},
		{"decision_outcomes", "actual_absolute", "TEXT"},
		{"decision_outcomes", "metric_basis", "TEXT"},
		{"decision_outcomes", "metric_unit", "TEXT"},
		{"decision_outcomes", "model_version", "TEXT"},
		{"decision_outcomes", "coverage_ratio", "TEXT"},
		{"decision_outcomes", "exclusion_reason", "TEXT"},
	} {
		if err := s.ensureColumn(ctx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpsertOperatingLedgerEvents(ctx context.Context, events []OperatingLedgerEvent) error {
	_, err := s.UpsertOperatingLedgerEventsCount(ctx, events)
	return err
}

func (s *Store) UpsertOperatingLedgerEventsCount(ctx context.Context, events []OperatingLedgerEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	return serializedWriteResult(ctx, s, func() (int, error) {
		return s.upsertOperatingLedgerEventsCountOnce(ctx, events)
	})
}

func (s *Store) upsertOperatingLedgerEventsCountOnce(ctx context.Context, events []OperatingLedgerEvent) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inserted := 0
	for index := range events {
		event := &events[index]
		if err := normalizeLedgerEvent(event); err != nil {
			return 0, fmt.Errorf("ledger event %d: %w", index, err)
		}
		metadata, err := json.Marshal(event.Metadata)
		if err != nil {
			return 0, fmt.Errorf("ledger event %d metadata: %w", index, err)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO operating_ledger_events
			(event_id,target_site_id,occurred_at,event_kind,amount,amount_evidence,model_id,group_name,user_segment,subject_user_id,
			subject_token_id,subject_role,is_test,is_internal,related_event_id,requests,usage_weight,usage_unit,usage_evidence,cash_revenue,
			procurement_cost,currency,revenue_evidence,cost_evidence,source,evidence_revision,metadata_json,payload_hash,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event.EventID, event.TargetSiteID,
			formatTime(event.OccurredAt), event.EventKind, nullableText(event.Amount), event.AmountEvidence, event.ModelID, event.GroupName,
			event.UserSegment, event.SubjectUserID, event.SubjectTokenID, event.SubjectRole, boolInt(event.IsTest), boolInt(event.IsInternal),
			event.RelatedEventID, event.Requests,
			nullableText(event.UsageWeight), nullableText(event.UsageUnit), event.UsageEvidence, nullableText(event.CashRevenue), nullableText(event.ProcurementCost),
			nullableText(event.Currency), event.RevenueEvidence, event.CostEvidence, event.Source,
			event.EvidenceRevision, string(metadata), event.PayloadHash, formatTime(event.CreatedAt))
		if err != nil {
			return 0, err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			var existingHash, existingSource, existingRevision, existingMetadata string
			if err := tx.QueryRowContext(ctx, `SELECT payload_hash,source,evidence_revision,metadata_json
				FROM operating_ledger_events WHERE event_id=?`, event.EventID).
				Scan(&existingHash, &existingSource, &existingRevision, &existingMetadata); err != nil {
				return 0, err
			}
			if existingHash != event.PayloadHash && !sameImmutableSourceRow(existingSource, existingRevision, existingMetadata, event) {
				return 0, fmt.Errorf("event_id %q was already recorded with different evidence", event.EventID)
			}
		} else {
			inserted++
			if err := recordOperatingLedgerChangeTx(ctx, tx, event.EventID, event.TargetSiteID, "insert", time.Now().UTC()); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func sameImmutableSourceRow(existingSource, existingRevision, existingMetadata string, incoming *OperatingLedgerEvent) bool {
	if strings.TrimSpace(existingSource) != strings.TrimSpace(incoming.Source) ||
		strings.TrimSpace(existingRevision) != strings.TrimSpace(incoming.EvidenceRevision) {
		return false
	}
	incomingRowHash := strings.TrimSpace(incoming.Metadata["source_row_hash"])
	if incomingRowHash == "" {
		return false
	}
	var metadata map[string]string
	if json.Unmarshal([]byte(existingMetadata), &metadata) != nil {
		return false
	}
	return strings.TrimSpace(metadata["source_row_hash"]) == incomingRowHash
}

func normalizeLedgerEvent(event *OperatingLedgerEvent) error {
	event.EventID = strings.TrimSpace(event.EventID)
	if event.EventID == "" || event.TargetSiteID <= 0 || event.OccurredAt.IsZero() {
		return errors.New("event_id, target_site_id, and occurred_at are required")
	}
	if event.Requests < 0 {
		return errors.New("requests cannot be negative")
	}
	event.EventKind = strings.ToLower(strings.TrimSpace(event.EventKind))
	if event.EventKind == "" {
		event.EventKind = "usage"
	}
	switch event.EventKind {
	case "usage", "recharge", "gift_credit", "refund", "payment_fee", "procurement_cost", "other_income", "other_expense":
	default:
		return errors.New("event_kind must be usage, recharge, gift_credit, refund, payment_fee, procurement_cost, other_income, or other_expense")
	}
	event.Amount = strings.TrimSpace(event.Amount)
	event.AmountEvidence = strings.ToLower(strings.TrimSpace(event.AmountEvidence))
	if event.AmountEvidence == "" {
		event.AmountEvidence = "missing"
	}
	event.SubjectUserID = strings.TrimSpace(event.SubjectUserID)
	event.SubjectTokenID = strings.TrimSpace(event.SubjectTokenID)
	event.SubjectRole = strings.ToLower(strings.TrimSpace(event.SubjectRole))
	event.RelatedEventID = strings.TrimSpace(event.RelatedEventID)
	if event.RelatedEventID == event.EventID {
		return errors.New("related_event_id cannot refer to the event itself")
	}
	for name, value := range map[string]string{"subject_user_id": event.SubjectUserID, "subject_token_id": event.SubjectTokenID,
		"subject_role": event.SubjectRole, "related_event_id": event.RelatedEventID} {
		if len(value) > 160 {
			return fmt.Errorf("%s exceeds 160 characters", name)
		}
	}
	event.UsageUnit = strings.ToLower(strings.TrimSpace(event.UsageUnit))
	event.UsageEvidence = strings.ToLower(strings.TrimSpace(event.UsageEvidence))
	event.RevenueEvidence = strings.ToLower(strings.TrimSpace(event.RevenueEvidence))
	event.CostEvidence = strings.ToLower(strings.TrimSpace(event.CostEvidence))
	if event.RevenueEvidence == "" {
		event.RevenueEvidence = "missing"
	}
	if event.CostEvidence == "" {
		event.CostEvidence = "missing"
	}
	if event.UsageEvidence == "" {
		event.UsageEvidence = "missing"
	}
	if strings.TrimSpace(event.UsageWeight) != "" && event.UsageUnit == "" {
		return errors.New("usage_unit is required when usage_weight is present")
	}
	for name, value := range map[string]string{
		"amount": event.Amount, "usage_weight": event.UsageWeight, "cash_revenue": event.CashRevenue, "procurement_cost": event.ProcurementCost,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := parseNonNegativeDecimal(value); !ok {
			return fmt.Errorf("%s must be a non-negative decimal", name)
		}
	}
	validEvidence := func(value string) bool {
		switch strings.TrimSpace(value) {
		case "exact", "observed", "estimated", "prior", "missing":
			return true
		default:
			return false
		}
	}
	if !validEvidence(event.AmountEvidence) || !validEvidence(event.UsageEvidence) || !validEvidence(event.RevenueEvidence) || !validEvidence(event.CostEvidence) {
		return errors.New("amount, usage, revenue, and cost evidence must be exact, observed, estimated, prior, or missing")
	}
	if event.EventKind == "usage" {
		if event.Amount != "" || event.AmountEvidence != "missing" {
			return errors.New("usage events use cash_revenue and procurement_cost; amount must be empty")
		}
	} else {
		if strings.TrimSpace(event.CashRevenue) != "" || strings.TrimSpace(event.ProcurementCost) != "" || strings.TrimSpace(event.UsageWeight) != "" {
			return errors.New("non-usage events use amount and cannot also carry usage revenue, procurement cost, or usage weight")
		}
		if event.Amount == "" && event.AmountEvidence != "missing" {
			return errors.New("amount_evidence must be missing when amount is unknown")
		}
		if event.Amount != "" && event.AmountEvidence == "missing" {
			return errors.New("known amount requires non-missing amount_evidence")
		}
	}
	if (event.Amount != "" || strings.TrimSpace(event.CashRevenue) != "" || strings.TrimSpace(event.ProcurementCost) != "") && strings.TrimSpace(event.Currency) == "" {
		return errors.New("currency is required for monetary amounts")
	}
	if strings.TrimSpace(event.Source) == "" {
		return errors.New("source is required")
	}
	if event.Metadata == nil {
		event.Metadata = map[string]string{}
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	payload, _ := json.Marshal(struct {
		TargetSiteID     int64             `json:"target_site_id"`
		OccurredAt       time.Time         `json:"occurred_at"`
		ModelID          string            `json:"model_id"`
		GroupName        string            `json:"group_name"`
		UserSegment      string            `json:"user_segment"`
		Requests         int64             `json:"requests"`
		UsageWeight      string            `json:"usage_weight"`
		UsageUnit        string            `json:"usage_unit"`
		UsageEvidence    string            `json:"usage_evidence"`
		CashRevenue      string            `json:"cash_revenue"`
		ProcurementCost  string            `json:"procurement_cost"`
		Currency         string            `json:"currency"`
		RevenueEvidence  string            `json:"revenue_evidence"`
		CostEvidence     string            `json:"cost_evidence"`
		Source           string            `json:"source"`
		EvidenceRevision string            `json:"evidence_revision"`
		Metadata         map[string]string `json:"metadata"`
	}{event.TargetSiteID, event.OccurredAt.UTC(), strings.TrimSpace(event.ModelID), strings.TrimSpace(event.GroupName),
		strings.TrimSpace(event.UserSegment), event.Requests, strings.TrimSpace(event.UsageWeight), event.UsageUnit, event.UsageEvidence, strings.TrimSpace(event.CashRevenue),
		strings.TrimSpace(event.ProcurementCost), strings.ToUpper(strings.TrimSpace(event.Currency)), event.RevenueEvidence,
		event.CostEvidence, strings.TrimSpace(event.Source), strings.TrimSpace(event.EvidenceRevision), event.Metadata})
	if event.EventKind != "usage" || event.Amount != "" || event.AmountEvidence != "missing" || event.SubjectUserID != "" ||
		event.SubjectTokenID != "" || event.SubjectRole != "" || event.IsTest || event.IsInternal || event.RelatedEventID != "" {
		payload, _ = json.Marshal(struct {
			Legacy         json.RawMessage `json:"legacy"`
			EventKind      string          `json:"event_kind"`
			Amount         string          `json:"amount"`
			AmountEvidence string          `json:"amount_evidence"`
			SubjectUserID  string          `json:"subject_user_id"`
			SubjectTokenID string          `json:"subject_token_id"`
			SubjectRole    string          `json:"subject_role"`
			IsTest         bool            `json:"is_test"`
			IsInternal     bool            `json:"is_internal"`
			RelatedEventID string          `json:"related_event_id"`
		}{payload, event.EventKind, event.Amount, event.AmountEvidence, event.SubjectUserID, event.SubjectTokenID,
			event.SubjectRole, event.IsTest, event.IsInternal, event.RelatedEventID})
	}
	sum := sha256.Sum256(payload)
	event.PayloadHash = hex.EncodeToString(sum[:])
	event.OccurredAt = event.OccurredAt.UTC()
	event.Currency = strings.ToUpper(strings.TrimSpace(event.Currency))
	return nil
}

func parseNonNegativeDecimal(value string) (string, bool) {
	// Keep the store independent from forecast arithmetic while still rejecting
	// malformed or negative evidence.
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") {
		return "", false
	}
	var number json.Number = json.Number(value)
	if _, err := number.Float64(); err != nil {
		return "", false
	}
	return value, true
}

func (s *Store) ListOperatingLedgerEvents(ctx context.Context, targetSiteID int64, group string, start, end time.Time) ([]OperatingLedgerEvent, error) {
	query := operatingLedgerEventSelect + ` WHERE target_site_id=? AND occurred_at>=? AND occurred_at<?`
	args := []any{targetSiteID, formatTime(start.UTC()), formatTime(end.UTC())}
	if strings.TrimSpace(group) != "" {
		query += ` AND group_name=?`
		args = append(args, strings.TrimSpace(group))
	}
	query += ` ORDER BY occurred_at,event_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OperatingLedgerEvent, 0)
	for rows.Next() {
		item, err := scanOperatingLedgerEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

const operatingLedgerEventSelect = `SELECT event_id,target_site_id,occurred_at,event_kind,amount,amount_evidence,model_id,group_name,user_segment,subject_user_id,
	subject_token_id,subject_role,is_test,is_internal,related_event_id,requests,usage_weight,usage_unit,usage_evidence,cash_revenue,
	procurement_cost,currency,revenue_evidence,cost_evidence,source,evidence_revision,metadata_json,payload_hash,created_at
	FROM operating_ledger_events`

func (s *Store) ListUsageEventsMissingCost(ctx context.Context, targetSiteID int64, start, end time.Time, limit int) ([]OperatingLedgerEvent, error) {
	if limit <= 0 || limit > 50000 {
		limit = 5000
	}
	rows, err := s.db.QueryContext(ctx, operatingLedgerEventSelect+` WHERE target_site_id=? AND event_kind='usage'
		AND (procurement_cost IS NULL OR TRIM(procurement_cost)='') AND occurred_at>=? AND occurred_at<?
		ORDER BY occurred_at DESC,event_id LIMIT ?`, targetSiteID, formatTime(start.UTC()), formatTime(end.UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OperatingLedgerEvent, 0)
	for rows.Next() {
		item, scanErr := scanOperatingLedgerEvent(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func scanOperatingLedgerEvent(row scanner) (OperatingLedgerEvent, error) {
	var item OperatingLedgerEvent
	var occurred, created, metadata string
	var amount, usage, usageUnit, revenue, cost, currency sql.NullString
	var isTest, isInternal int
	err := row.Scan(&item.EventID, &item.TargetSiteID, &occurred, &item.EventKind, &amount, &item.AmountEvidence,
		&item.ModelID, &item.GroupName, &item.UserSegment, &item.SubjectUserID, &item.SubjectTokenID, &item.SubjectRole,
		&isTest, &isInternal, &item.RelatedEventID, &item.Requests, &usage, &usageUnit, &item.UsageEvidence, &revenue, &cost,
		&currency, &item.RevenueEvidence, &item.CostEvidence, &item.Source, &item.EvidenceRevision, &metadata, &item.PayloadHash, &created)
	if err != nil {
		return OperatingLedgerEvent{}, err
	}
	item.Amount, item.UsageWeight, item.UsageUnit = nullString(amount), nullString(usage), nullString(usageUnit)
	item.CashRevenue, item.ProcurementCost, item.Currency = nullString(revenue), nullString(cost), nullString(currency)
	item.IsTest, item.IsInternal = isTest != 0, isInternal != 0
	item.OccurredAt, item.CreatedAt = parseTime(occurred), parseTime(created)
	_ = json.Unmarshal([]byte(metadata), &item.Metadata)
	return item, nil
}

func (s *Store) OperatingLedgerSummary(ctx context.Context) (LedgerSummary, error) {
	var result LedgerSummary
	var earliest, latest sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN revenue_evidence='exact' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cost_evidence='exact' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN revenue_evidence='estimated' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cost_evidence='estimated' THEN 1 ELSE 0 END),0),MIN(occurred_at),MAX(occurred_at)
		FROM operating_ledger_events`).Scan(&result.EventCount, &result.ExactRevenueEvents, &result.ExactCostEvents,
		&result.EstimatedRevenueEvents, &result.EstimatedCostEvents, &earliest, &latest)
	if err != nil {
		return LedgerSummary{}, err
	}
	result.EarliestAt = nullableParsedTime(earliest)
	result.LatestAt = nullableParsedTime(latest)
	return result, nil
}

func SummarizeOperatingLedgerEvents(events []OperatingLedgerEvent) LedgerSummary {
	var result LedgerSummary
	for _, event := range events {
		result.EventCount++
		switch strings.ToLower(strings.TrimSpace(event.RevenueEvidence)) {
		case "exact":
			result.ExactRevenueEvents++
		case "estimated":
			result.EstimatedRevenueEvents++
		}
		switch strings.ToLower(strings.TrimSpace(event.CostEvidence)) {
		case "exact":
			result.ExactCostEvents++
		case "estimated":
			result.EstimatedCostEvents++
		}
		occurred := event.OccurredAt.UTC()
		if result.EarliestAt == nil || occurred.Before(*result.EarliestAt) {
			copy := occurred
			result.EarliestAt = &copy
		}
		if result.LatestAt == nil || occurred.After(*result.LatestAt) {
			copy := occurred
			result.LatestAt = &copy
		}
	}
	return result
}

type decisionSnapshotExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// UpsertDecisionSnapshot keeps its compatibility name, but snapshots are
// insert-only. An exact replay is idempotent; a different decision-time input
// for the same proposal is rejected instead of rewriting history.
func (s *Store) UpsertDecisionSnapshot(ctx context.Context, snapshot DecisionSnapshot) error {
	return insertDecisionSnapshot(ctx, s.db, snapshot)
}

func insertDecisionSnapshot(ctx context.Context, exec decisionSnapshotExecutor, snapshot DecisionSnapshot) error {
	if snapshot.ProposalID <= 0 || strings.TrimSpace(snapshot.ModelVersion) == "" || strings.TrimSpace(snapshot.InputHash) == "" ||
		strings.TrimSpace(snapshot.DecisionKind) == "" || snapshot.DecidedAt.IsZero() || len(snapshot.Payload) == 0 {
		return errors.New("decision snapshot requires proposal, model version, input hash, decision kind, decided_at, and payload")
	}
	now := time.Now().UTC()
	result, err := exec.ExecContext(ctx, `INSERT INTO decision_snapshots
		(proposal_id,model_version,input_hash,lifecycle_status,decision_kind,decided_at,payload_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(proposal_id) DO NOTHING`, snapshot.ProposalID, snapshot.ModelVersion,
		snapshot.InputHash, snapshot.LifecycleStatus, snapshot.DecisionKind, formatTime(snapshot.DecidedAt.UTC()),
		string(snapshot.Payload), formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	var modelVersion, inputHash, decisionKind, decidedAt, payload string
	if err := exec.QueryRowContext(ctx, `SELECT model_version,input_hash,decision_kind,decided_at,payload_json
		FROM decision_snapshots WHERE proposal_id=?`, snapshot.ProposalID).
		Scan(&modelVersion, &inputHash, &decisionKind, &decidedAt, &payload); err != nil {
		return err
	}
	if modelVersion != snapshot.ModelVersion || inputHash != snapshot.InputHash || decisionKind != snapshot.DecisionKind ||
		decidedAt != formatTime(snapshot.DecidedAt.UTC()) || payload != string(snapshot.Payload) {
		return fmt.Errorf("proposal %d already has a different immutable decision snapshot", snapshot.ProposalID)
	}
	return nil
}

func (s *Store) BeginProposalApplication(ctx context.Context, id int64, from []string, snapshot DecisionSnapshot) error {
	return s.transitionProposalWithSnapshot(ctx, id, from, "applying", "", "", snapshot)
}

func (s *Store) RejectProposalWithDecisionSnapshot(ctx context.Context, id int64, from []string, snapshot DecisionSnapshot, message string) error {
	return s.transitionProposalWithSnapshot(ctx, id, from, "rejected", "", message, snapshot)
}

func (s *Store) transitionProposalWithSnapshot(ctx context.Context, id int64, from []string, status, readback, message string, snapshot DecisionSnapshot) error {
	if len(from) == 0 || snapshot.ProposalID != id {
		return errors.New("proposal snapshot transition requires matching proposal and source states")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertDecisionSnapshot(ctx, tx, snapshot); err != nil {
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	query := `UPDATE change_proposals SET status=?,readback_value=?,error=?,updated_at=? WHERE id=? AND status IN (` + placeholders + `)`
	args := []any{status, readback, message, formatTime(time.Now().UTC()), id}
	for _, state := range from {
		args = append(args, state)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("proposal %d state changed concurrently", id)
	}
	return tx.Commit()
}

func (s *Store) GetDecisionSnapshot(ctx context.Context, proposalID int64) (DecisionSnapshot, error) {
	var item DecisionSnapshot
	var payload, decided, created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT proposal_id,model_version,input_hash,lifecycle_status,decision_kind,decided_at,payload_json,created_at,updated_at
		FROM decision_snapshots WHERE proposal_id=?`, proposalID).Scan(&item.ProposalID, &item.ModelVersion, &item.InputHash,
		&item.LifecycleStatus, &item.DecisionKind, &decided, &payload, &created, &updated)
	if err != nil {
		return DecisionSnapshot{}, err
	}
	item.Payload = json.RawMessage(payload)
	item.DecidedAt = parseTime(decided)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) CompleteProposalApplication(ctx context.Context, id int64, readbackValue, readbackHash string, effectiveAt time.Time) error {
	if id <= 0 || strings.TrimSpace(readbackHash) == "" || effectiveAt.IsZero() {
		return errors.New("application receipt requires proposal, readback hash, and effective_at")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var targetSiteID int64
	var targetGroup, status string
	if err := tx.QueryRowContext(ctx, `SELECT target_site_id,target_group,status FROM change_proposals WHERE id=?`, id).
		Scan(&targetSiteID, &targetGroup, &status); err != nil {
		return err
	}
	if status == "applied" {
		var existingHash, existingValue, existingAt string
		if err := tx.QueryRowContext(ctx, `SELECT readback_hash,readback_value,effective_at FROM decision_effects WHERE proposal_id=?`, id).
			Scan(&existingHash, &existingValue, &existingAt); err != nil {
			return err
		}
		if existingHash != readbackHash || existingValue != readbackValue || existingAt != formatTime(effectiveAt.UTC()) {
			return fmt.Errorf("proposal %d already has a different effect receipt", id)
		}
		return nil
	}
	if status != "applying" {
		return fmt.Errorf("proposal %d is %s, not applying", id, status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE decision_exposures SET ended_at=?,end_reason='superseded',end_hash=?
		WHERE target_site_id=? AND target_group=? AND proposal_id<>? AND ended_at IS NULL`, formatTime(effectiveAt.UTC()),
		readbackHash, targetSiteID, targetGroup, id); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO decision_effects(proposal_id,effective_at,readback_hash,readback_value,created_at)
		VALUES(?,?,?,?,?)`, id, formatTime(effectiveAt.UTC()), readbackHash, readbackValue, formatTime(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO decision_exposures
		(proposal_id,target_site_id,target_group,effective_at,ended_at,end_reason,start_hash,end_hash) VALUES(?,?,?,?,NULL,'',?,'')`,
		id, targetSiteID, targetGroup, formatTime(effectiveAt.UTC()), readbackHash); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE change_proposals SET status='applied',readback_value=?,error='',updated_at=?
		WHERE id=? AND status='applying'`, readbackValue, formatTime(effectiveAt.UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("proposal %d state changed concurrently", id)
	}
	return tx.Commit()
}

func (s *Store) CompleteProposalRollback(ctx context.Context, id int64, readbackValue, readbackHash string, endedAt time.Time) error {
	if id <= 0 || strings.TrimSpace(readbackHash) == "" || endedAt.IsZero() {
		return errors.New("rollback receipt requires proposal, readback hash, and ended_at")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE change_proposals SET status='rolled_back',readback_value=?,error='',updated_at=?
		WHERE id=? AND status='rolling_back'`, readbackValue, formatTime(endedAt.UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("proposal %d state changed concurrently", id)
	}
	result, err = tx.ExecContext(ctx, `UPDATE decision_exposures SET ended_at=?,end_reason='rolled_back',end_hash=?
		WHERE proposal_id=? AND ended_at IS NULL`, formatTime(endedAt.UTC()), readbackHash, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("proposal %d has no active exposure to roll back", id)
	}
	return tx.Commit()
}

func (s *Store) GetDecisionEffect(ctx context.Context, proposalID int64) (DecisionEffect, error) {
	var item DecisionEffect
	var effective, created string
	err := s.db.QueryRowContext(ctx, `SELECT proposal_id,effective_at,readback_hash,readback_value,created_at
		FROM decision_effects WHERE proposal_id=?`, proposalID).Scan(&item.ProposalID, &effective, &item.ReadbackHash, &item.ReadbackValue, &created)
	if err != nil {
		return DecisionEffect{}, err
	}
	item.EffectiveAt, item.CreatedAt = parseTime(effective), parseTime(created)
	return item, nil
}

func (s *Store) GetDecisionExposure(ctx context.Context, proposalID int64) (DecisionExposure, error) {
	var item DecisionExposure
	var effective string
	var ended sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT proposal_id,target_site_id,target_group,effective_at,ended_at,end_reason,start_hash,end_hash
		FROM decision_exposures WHERE proposal_id=?`, proposalID).Scan(&item.ProposalID, &item.TargetSiteID, &item.TargetGroup,
		&effective, &ended, &item.EndReason, &item.StartHash, &item.EndHash)
	if err != nil {
		return DecisionExposure{}, err
	}
	item.EffectiveAt, item.EndedAt = parseTime(effective), nullableParsedTime(ended)
	return item, nil
}

func (s *Store) UpsertDecisionOutcome(ctx context.Context, outcome DecisionOutcome) error {
	if outcome.ProposalID <= 0 || outcome.HorizonHours <= 0 || outcome.DueAt.IsZero() || len(outcome.Prediction) == 0 {
		return errors.New("decision outcome requires proposal, horizon, due time, and prediction")
	}
	band := outcome.BandHit
	if band == nil {
		band = outcome.IntervalHit
	}
	var bandHit any
	if band != nil {
		bandHit = boolInt(*band)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO decision_outcomes
		(proposal_id,horizon_hours,due_at,status,prediction_json,observation_json,absolute_error,absolute_percentage_error,
		signed_error,symmetric_percentage_error,actual_absolute,interval_hit,metric_basis,metric_unit,model_version,coverage_ratio,
		exclusion_reason,observed_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(proposal_id,horizon_hours) DO UPDATE SET
		due_at=excluded.due_at,status=excluded.status,prediction_json=excluded.prediction_json,
		observation_json=excluded.observation_json,absolute_error=excluded.absolute_error,
		absolute_percentage_error=excluded.absolute_percentage_error,signed_error=excluded.signed_error,
		symmetric_percentage_error=excluded.symmetric_percentage_error,actual_absolute=excluded.actual_absolute,
		interval_hit=excluded.interval_hit,metric_basis=excluded.metric_basis,metric_unit=excluded.metric_unit,
		model_version=excluded.model_version,coverage_ratio=excluded.coverage_ratio,exclusion_reason=excluded.exclusion_reason,
		observed_at=excluded.observed_at,updated_at=excluded.updated_at`,
		outcome.ProposalID, outcome.HorizonHours, formatTime(outcome.DueAt.UTC()), outcome.Status, string(outcome.Prediction),
		nullableRawJSON(outcome.Observation), nullableText(outcome.AbsoluteError), nullableText(outcome.AbsolutePctError),
		nullableText(outcome.SignedError), nullableText(outcome.SymmetricPctError), nullableText(outcome.ActualAbsolute), bandHit,
		nullableText(outcome.MetricBasis), nullableText(outcome.MetricUnit), nullableText(outcome.ModelVersion),
		nullableText(outcome.CoverageRatio), nullableText(outcome.ExclusionReason), nullableTime(outcome.ObservedAt), formatTime(time.Now().UTC()))
	return err
}

func (s *Store) ListDecisionOutcomes(ctx context.Context, proposalID int64) ([]DecisionOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT proposal_id,horizon_hours,due_at,status,prediction_json,observation_json,
		absolute_error,absolute_percentage_error,signed_error,symmetric_percentage_error,actual_absolute,interval_hit,
		metric_basis,metric_unit,model_version,coverage_ratio,exclusion_reason,observed_at,updated_at FROM decision_outcomes
		WHERE proposal_id=? ORDER BY horizon_hours`, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]DecisionOutcome, 0)
	for rows.Next() {
		var item DecisionOutcome
		var due, prediction, updated string
		var observation, absolute, pct, signed, symmetric, actual, basis, unit, modelVersion, coverage, exclusion, observed sql.NullString
		var hit sql.NullInt64
		if err := rows.Scan(&item.ProposalID, &item.HorizonHours, &due, &item.Status, &prediction, &observation,
			&absolute, &pct, &signed, &symmetric, &actual, &hit, &basis, &unit, &modelVersion, &coverage, &exclusion, &observed, &updated); err != nil {
			return nil, err
		}
		item.DueAt = parseTime(due)
		item.Prediction = json.RawMessage(prediction)
		if observation.Valid {
			item.Observation = json.RawMessage(observation.String)
		}
		item.AbsoluteError = nullString(absolute)
		item.AbsolutePctError = nullString(pct)
		item.SignedError, item.SymmetricPctError, item.ActualAbsolute = nullString(signed), nullString(symmetric), nullString(actual)
		item.MetricBasis, item.MetricUnit, item.ModelVersion = nullString(basis), nullString(unit), nullString(modelVersion)
		item.CoverageRatio, item.ExclusionReason = nullString(coverage), nullString(exclusion)
		if hit.Valid {
			value := hit.Int64 != 0
			item.BandHit = &value
			item.IntervalHit = &value
		}
		item.ObservedAt = nullableParsedTime(observed)
		item.UpdatedAt = parseTime(updated)
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListOutcomeReconcileCandidates performs the due-work filtering in SQLite so
// the scheduler does not repeatedly scan and truncate the newest proposals.
// Successfully or unsuccessfully attempted candidates move to the back of the
// queue through decision_reconcile_attempts.
func (s *Store) ListOutcomeReconcileCandidates(ctx context.Context, now time.Time, limit int) ([]domain.ChangeProposal, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	query := `SELECT p.id,p.policy_id,p.target_site_id,p.target_group,p.expected_old,p.proposed_value,p.expected_hash,p.calculation_json,
		p.group_model_pricing_plan_json,p.group_model_pricing_receipt_json,
		p.status,p.mode,p.idempotency_key,p.readback_value,p.error,p.created_at,p.updated_at
		FROM change_proposals p
		JOIN decision_snapshots s ON s.proposal_id=p.id
		JOIN decision_effects f ON f.proposal_id=p.id
		JOIN decision_exposures e ON e.proposal_id=p.id
		LEFT JOIN decision_outcomes o ON o.proposal_id=p.id
		LEFT JOIN decision_reconcile_attempts a ON a.proposal_id=p.id
		WHERE p.status IN ('applied','rolled_back')
		AND COALESCE(json_extract(s.payload_json,'$.forecast_ready'),0)=1
		AND TRIM(COALESCE(json_extract(s.payload_json,'$.model_pattern'),''))<>''
		GROUP BY p.id
		HAVING COUNT(o.proposal_id)=0 OR MAX(CASE
			WHEN o.status NOT IN ('observed','censored','no_exposure','incompatible')
			AND NOT (o.status='awaiting_ledger' AND COALESCE(o.exclusion_reason,'')='non_cash_basis')
			AND (o.due_at<=? OR (e.ended_at IS NOT NULL AND o.due_at>e.ended_at)) THEN 1 ELSE 0 END)=1
		ORDER BY CASE WHEN COALESCE(a.last_attempt_at,MAX(o.updated_at),'')='' THEN 0 ELSE 1 END,
			COALESCE(a.last_attempt_at,MAX(o.updated_at),'') ASC,
			COALESCE(MIN(CASE WHEN o.status NOT IN ('observed','censored','no_exposure','incompatible') THEN o.due_at END),f.effective_at) ASC,
			p.id ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, formatTime(now.UTC()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ChangeProposal, 0, limit)
	for rows.Next() {
		item, scanErr := scanProposal(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) RecordOutcomeReconcileAttempt(ctx context.Context, proposalID int64, attemptedAt time.Time, reconcileErr error) error {
	if proposalID <= 0 || attemptedAt.IsZero() {
		return errors.New("outcome reconcile attempt requires proposal and timestamp")
	}
	message := ""
	if reconcileErr != nil {
		message = reconcileErr.Error()
		if len(message) > 2000 {
			message = message[:2000]
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO decision_reconcile_attempts(proposal_id,last_attempt_at,last_error) VALUES(?,?,?)
		ON CONFLICT(proposal_id) DO UPDATE SET last_attempt_at=excluded.last_attempt_at,last_error=excluded.last_error`,
		proposalID, formatTime(attemptedAt.UTC()), message)
	return err
}

func (s *Store) ListCompletedDecisionOutcomes(ctx context.Context, limit int) ([]DecisionOutcome, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.proposal_id,o.horizon_hours,o.due_at,o.status,o.prediction_json,o.observation_json,
		o.absolute_error,o.absolute_percentage_error,o.signed_error,o.symmetric_percentage_error,o.actual_absolute,o.interval_hit,
		o.metric_basis,o.metric_unit,o.model_version,o.coverage_ratio,o.exclusion_reason,o.observed_at,o.updated_at
		FROM decision_outcomes o JOIN change_proposals p ON p.id=o.proposal_id
		JOIN decision_snapshots s ON s.proposal_id=o.proposal_id
		JOIN decision_exposures e ON e.proposal_id=o.proposal_id
		WHERE o.status IN ('observed','censored','coverage_unknown','insufficient_coverage','no_exposure','incompatible')
		AND p.status IN ('applied','rolled_back')
		AND (o.status<>'observed' OR e.ended_at IS NULL OR o.due_at<=e.ended_at)
		ORDER BY COALESCE(o.observed_at,o.updated_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]DecisionOutcome, 0)
	for rows.Next() {
		var item DecisionOutcome
		var due, prediction, updated string
		var observation, absolute, pct, signed, symmetric, actual, basis, unit, modelVersion, coverage, exclusion, observed sql.NullString
		var hit sql.NullInt64
		if err := rows.Scan(&item.ProposalID, &item.HorizonHours, &due, &item.Status, &prediction, &observation,
			&absolute, &pct, &signed, &symmetric, &actual, &hit, &basis, &unit, &modelVersion, &coverage, &exclusion, &observed, &updated); err != nil {
			return nil, err
		}
		item.DueAt, item.Prediction, item.UpdatedAt = parseTime(due), json.RawMessage(prediction), parseTime(updated)
		if observation.Valid {
			item.Observation = json.RawMessage(observation.String)
		}
		item.AbsoluteError, item.AbsolutePctError = nullString(absolute), nullString(pct)
		item.SignedError, item.SymmetricPctError, item.ActualAbsolute = nullString(signed), nullString(symmetric), nullString(actual)
		item.MetricBasis, item.MetricUnit, item.ModelVersion = nullString(basis), nullString(unit), nullString(modelVersion)
		item.CoverageRatio, item.ExclusionReason = nullString(coverage), nullString(exclusion)
		if hit.Valid {
			value := hit.Int64 != 0
			item.BandHit = &value
			item.IntervalHit = &value
		}
		item.ObservedAt = nullableParsedTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func nullString(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullableRawJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}
