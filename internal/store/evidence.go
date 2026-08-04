package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (s *Store) CatalogEvidenceStatus(ctx context.Context, supplierID int64, responseHash string) (domain.CatalogEvidenceStatus, error) {
	query := `SELECT payload_json,response_hash,total_observations,consecutive_observations,
		COALESCE(NULLIF(last_seen,''),observed_at)
		FROM catalog_snapshots WHERE supplier_id=?`
	args := []any{supplierID}
	if strings.TrimSpace(responseHash) != "" {
		query += ` AND response_hash=?`
		args = append(args, responseHash)
	}
	query += ` ORDER BY julianday(COALESCE(NULLIF(last_seen,''),observed_at)) DESC,id DESC LIMIT 1`

	var payload, hash, lastSeen string
	var status domain.CatalogEvidenceStatus
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&payload, &hash, &status.TotalObservations,
		&status.ConsecutiveObservations, &lastSeen)
	if err != nil {
		return domain.CatalogEvidenceStatus{}, err
	}
	var snapshot domain.CatalogSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return domain.CatalogEvidenceStatus{}, fmt.Errorf("decode catalog evidence snapshot: %w", err)
	}
	status.SupplierID = supplierID
	status.ResponseHash = hash
	status.SourceType = snapshot.Evidence.SourceType
	status.LastSeen = parseTime(lastSeen)
	return status, nil
}

func (s *Store) UpdatePendingProposalMode(ctx context.Context, id int64, mode string, calculation domain.PriceCalculation) (domain.ChangeProposal, error) {
	if mode != "auto" && mode != "approval" {
		return domain.ChangeProposal{}, fmt.Errorf("unsupported proposal mode %q", mode)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE change_proposals SET mode=?,calculation_json=?,updated_at=?
		WHERE id=? AND status='pending'`, mode, mustJSON(calculation), formatTime(time.Now().UTC()), id)
	if err != nil {
		return domain.ChangeProposal{}, fmt.Errorf("update pending proposal mode: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	if count == 0 {
		current, getErr := s.GetProposal(ctx, id)
		if getErr != nil {
			return domain.ChangeProposal{}, getErr
		}
		if current.Status == "pending" && current.Mode == mode {
			return current, nil
		}
		return domain.ChangeProposal{}, sql.ErrNoRows
	}
	return s.GetProposal(ctx, id)
}

// SupersedeActionableProposals removes obsolete suggestions from the operator's
// action queue while retaining them as immutable audit history.
func (s *Store) SupersedeActionableProposals(ctx context.Context, policyID int64, message string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE change_proposals
		SET status='stale',error=?,updated_at=?
		WHERE policy_id=? AND status IN ('pending','failed')`,
		message, formatTime(time.Now().UTC()), policyID)
	if err != nil {
		return 0, fmt.Errorf("supersede actionable proposals: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, nil
}
