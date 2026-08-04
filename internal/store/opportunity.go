package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// OpportunityDiscovery retains the full immutable discovery evidence needed by
// the opportunity evaluator. DiscoveryItem remains the compatibility summary.
type OpportunityDiscovery struct {
	domain.DiscoveryItem
	Offer             domain.GroupOffer `json:"offer"`
	EntityFingerprint string            `json:"entity_fingerprint"`
}

// OpportunityUsage is a normalized model-level demand observation. Empty
// monetary values are unknown; an explicit "0" remains a real zero.
type OpportunityUsage struct {
	TargetSiteID     int64     `json:"target_site_id"`
	ModelID          string    `json:"model_id"`
	WindowDays       int       `json:"window_days"`
	DemandWeight     string    `json:"demand_weight"`
	DemandUnit       string    `json:"demand_unit"`
	CashRevenue      string    `json:"cash_revenue,omitempty"`
	RoutedCost       string    `json:"routed_cost,omitempty"`
	Currency         string    `json:"currency,omitempty"`
	Source           string    `json:"source"`
	Confidence       string    `json:"confidence"`
	ObservedAt       time.Time `json:"observed_at"`
	EvidenceRevision string    `json:"evidence_revision,omitempty"`
}

type OpportunityTargetCatalog struct {
	TargetSiteID int64                  `json:"target_site_id"`
	Snapshot     domain.CatalogSnapshot `json:"snapshot"`
	ObservedAt   time.Time              `json:"observed_at"`
}

type OpportunityEvaluationRecord struct {
	CandidateID    string
	InputHash      string
	Classification string
	Payload        any
}

func (s *Store) migrateOpportunity(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS opportunity_target_catalogs (
			target_site_id INTEGER PRIMARY KEY REFERENCES target_sites(id) ON DELETE CASCADE,
			payload_json TEXT NOT NULL,
			response_hash TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS opportunity_model_usage (
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			model_id TEXT NOT NULL,
			window_days INTEGER NOT NULL,
			demand_weight TEXT NOT NULL,
			demand_unit TEXT NOT NULL DEFAULT 'normalized_demand',
			cash_revenue TEXT,
			routed_cost TEXT,
			currency TEXT,
			source TEXT NOT NULL,
			confidence TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			evidence_revision TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(target_site_id,model_id,window_days,source)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_opportunity_usage_window
			ON opportunity_model_usage(target_site_id,window_days,observed_at DESC,model_id)`,
		`CREATE TABLE IF NOT EXISTS opportunity_evaluations (
			candidate_id TEXT PRIMARY KEY,
			input_hash TEXT NOT NULL,
			classification TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			evaluated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_opportunity_evaluations_class
			ON opportunity_evaluations(classification,evaluated_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply opportunity schema: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "opportunity_model_usage", "demand_unit", "TEXT NOT NULL DEFAULT 'normalized_demand'"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListOpportunityDiscoveries(ctx context.Context, includeBaseline bool, limit int) ([]OpportunityDiscovery, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	query := `SELECT d.supplier_id,s.name,d.kind,d.external_id,d.name,d.ratio,d.model_count,d.payload_json,
		d.first_seen_at,d.last_seen_at,d.acknowledged_at,d.is_baseline
		FROM discoveries d JOIN suppliers s ON s.id=d.supplier_id`
	if !includeBaseline {
		query += ` WHERE d.is_baseline=0`
	}
	query += ` ORDER BY d.first_seen_at DESC,d.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OpportunityDiscovery, 0)
	for rows.Next() {
		var item OpportunityDiscovery
		var payload, first, last string
		var acknowledged sql.NullString
		var baseline int
		if err := rows.Scan(&item.SupplierID, &item.SupplierName, &item.Kind, &item.ExternalID, &item.Name, &item.Ratio,
			&item.ModelCount, &payload, &first, &last, &acknowledged, &baseline); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &item.Offer); err != nil {
			return nil, fmt.Errorf("decode discovery %d/%s/%s: %w", item.SupplierID, item.Kind, item.ExternalID, err)
		}
		item.FirstSeenAt = parseTime(first)
		item.LastSeenAt = parseTime(last)
		item.Baseline = baseline != 0
		if acknowledged.Valid {
			value := parseTime(acknowledged.String)
			item.AcknowledgedAt = &value
		}
		item.EntityFingerprint = fmt.Sprintf("%d:%s:%s", item.SupplierID, strings.ToLower(item.Kind), strings.ToLower(item.ExternalID))
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SaveOpportunityTargetCatalog(ctx context.Context, targetSiteID int64, snapshot domain.CatalogSnapshot) error {
	if targetSiteID <= 0 {
		return errors.New("target site id is required")
	}
	observedAt := snapshot.Evidence.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO opportunity_target_catalogs(target_site_id,payload_json,response_hash,observed_at)
		VALUES(?,?,?,?) ON CONFLICT(target_site_id) DO UPDATE SET
		payload_json=excluded.payload_json,response_hash=excluded.response_hash,observed_at=excluded.observed_at`,
		targetSiteID, string(payload), snapshot.Evidence.ResponseHash, formatTime(observedAt))
	return err
}

func (s *Store) LatestOpportunityTargetCatalog(ctx context.Context, targetSiteID int64) (OpportunityTargetCatalog, error) {
	var payload, observed string
	if err := s.db.QueryRowContext(ctx, `SELECT payload_json,observed_at FROM opportunity_target_catalogs WHERE target_site_id=?`, targetSiteID).
		Scan(&payload, &observed); err != nil {
		return OpportunityTargetCatalog{}, err
	}
	result := OpportunityTargetCatalog{TargetSiteID: targetSiteID, ObservedAt: parseTime(observed)}
	if err := json.Unmarshal([]byte(payload), &result.Snapshot); err != nil {
		return OpportunityTargetCatalog{}, fmt.Errorf("decode target opportunity catalog: %w", err)
	}
	return result, nil
}

func (s *Store) UpsertOpportunityUsage(ctx context.Context, observations []OpportunityUsage) error {
	if len(observations) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range observations {
		if item.TargetSiteID <= 0 || strings.TrimSpace(item.ModelID) == "" || (item.WindowDays != 7 && item.WindowDays != 30) || strings.TrimSpace(item.DemandWeight) == "" {
			return errors.New("opportunity usage requires a target site, model, a 7/30-day window, and demand weight")
		}
		if item.ObservedAt.IsZero() {
			item.ObservedAt = time.Now().UTC()
		}
		if strings.TrimSpace(item.Source) == "" {
			item.Source = "import"
		}
		item.DemandUnit = normalizeDemandUnit(item.DemandUnit)
		if _, err := tx.ExecContext(ctx, `INSERT INTO opportunity_model_usage
			(target_site_id,model_id,window_days,demand_weight,demand_unit,cash_revenue,routed_cost,currency,source,confidence,observed_at,evidence_revision)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(target_site_id,model_id,window_days,source) DO UPDATE SET
			demand_weight=excluded.demand_weight,demand_unit=excluded.demand_unit,cash_revenue=excluded.cash_revenue,routed_cost=excluded.routed_cost,
			currency=excluded.currency,confidence=excluded.confidence,observed_at=excluded.observed_at,evidence_revision=excluded.evidence_revision`,
			item.TargetSiteID, strings.TrimSpace(item.ModelID), item.WindowDays, item.DemandWeight, item.DemandUnit, nullableText(item.CashRevenue), nullableText(item.RoutedCost),
			nullableText(item.Currency), item.Source, item.Confidence, formatTime(item.ObservedAt), item.EvidenceRevision); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListOpportunityUsage(ctx context.Context, targetSiteID int64, maxAge time.Duration) ([]OpportunityUsage, error) {
	query := `SELECT target_site_id,model_id,window_days,demand_weight,demand_unit,cash_revenue,routed_cost,currency,source,confidence,observed_at,evidence_revision
		FROM opportunity_model_usage WHERE target_site_id=?`
	args := []any{targetSiteID}
	if maxAge > 0 {
		query += ` AND observed_at>=?`
		args = append(args, formatTime(time.Now().UTC().Add(-maxAge)))
	}
	query += ` ORDER BY model_id,window_days,observed_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OpportunityUsage, 0)
	for rows.Next() {
		var item OpportunityUsage
		var revenue, cost, currency sql.NullString
		var observed string
		if err := rows.Scan(&item.TargetSiteID, &item.ModelID, &item.WindowDays, &item.DemandWeight, &item.DemandUnit, &revenue, &cost, &currency,
			&item.Source, &item.Confidence, &observed, &item.EvidenceRevision); err != nil {
			return nil, err
		}
		if revenue.Valid {
			item.CashRevenue = revenue.String
		}
		if cost.Valid {
			item.RoutedCost = cost.String
		}
		if currency.Valid {
			item.Currency = currency.String
		}
		item.ObservedAt = parseTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func normalizeDemandUnit(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "normalized_demand"
	}
	return value
}

func (s *Store) SaveOpportunityEvaluations(ctx context.Context, rows []OpportunityEvaluationRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	for _, row := range rows {
		payload, marshalErr := json.Marshal(row.Payload)
		if marshalErr != nil {
			return marshalErr
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO opportunity_evaluations(candidate_id,input_hash,classification,payload_json,evaluated_at)
			VALUES(?,?,?,?,?) ON CONFLICT(candidate_id) DO UPDATE SET input_hash=excluded.input_hash,
			classification=excluded.classification,payload_json=excluded.payload_json,evaluated_at=excluded.evaluated_at`,
			row.CandidateID, row.InputHash, row.Classification, string(payload), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
