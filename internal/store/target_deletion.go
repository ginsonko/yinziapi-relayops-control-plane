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

var ErrTargetDeletionConflict = errors.New("目标站删除预览已失效")

func (s *Store) PreviewTargetDeletion(ctx context.Context, id int64) (domain.TargetDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readTargetDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeleteTargetSafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.TargetDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `UPDATE target_sites SET enabled=enabled WHERE id=?`, id)
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.TargetDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readTargetDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.TargetName) {
		return domain.TargetDeletionImpact{}, fmt.Errorf("%w：确认名称与当前目标站名称不一致", ErrTargetDeletionConflict)
	}
	if strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.TargetDeletionImpact{}, fmt.Errorf("%w：关联数据已变化，请重新查看删除影响", ErrTargetDeletionConflict)
	}
	if impact.ActiveLeases > 0 {
		return domain.TargetDeletionImpact{}, fmt.Errorf("%w：目标站仍有正在执行的受控操作，请稍后重新预览", ErrTargetDeletionConflict)
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM listing_change_sets WHERE target_site_id=?`, []any{id}},
		{`DELETE FROM listing_plans WHERE target_site_id=?`, []any{id}},
		{`DELETE FROM change_proposals WHERE target_site_id=? OR policy_id IN (SELECT id FROM pricing_policies WHERE target_site_id=?)`, []any{id, id}},
		{`DELETE FROM pricing_policies WHERE target_site_id=?`, []any{id}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return domain.TargetDeletionImpact{}, fmt.Errorf("prepare target deletion: %w", err)
		}
	}

	metadata := map[string]any{
		"deleted_target_id": impact.TargetSiteID, "deleted_target_name": impact.TargetName,
		"deleted_target_origin": impact.BaseURL, "credential_id": impact.CredentialID,
		"credential_preserved": true, "pricing_policies": impact.PricingPolicies,
		"change_proposals": impact.ChangeProposals, "listing_plans": impact.ListingPlans,
		"ledger_events": impact.LedgerEvents, "control_operations": impact.ControlOperations,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('target_deleted','warning',?,?,NULL,?,?)`, "目标站已删除",
		fmt.Sprintf("%s（%s）已由管理员确认从 RelayOps 删除；目标站和独立凭据均未收到删除请求。", impact.TargetName, impact.BaseURL),
		mustJSON(metadata), formatTime(time.Now().UTC())); err != nil {
		return domain.TargetDeletionImpact{}, fmt.Errorf("write target deletion audit: %w", err)
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM target_sites WHERE id=?`, id)
	if err != nil {
		return domain.TargetDeletionImpact{}, fmt.Errorf("delete target: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return domain.TargetDeletionImpact{}, fmt.Errorf("%w：目标站在删除过程中发生变化", ErrTargetDeletionConflict)
	}
	if err := tx.Commit(); err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	return impact, nil
}

func readTargetDeletionImpact(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (domain.TargetDeletionImpact, error) {
	target, err := scanTarget(tx.QueryRowContext(ctx, targetSelect+` WHERE id=?`, id))
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	impact := domain.TargetDeletionImpact{TargetSiteID: target.ID, TargetName: target.Name, BaseURL: target.BaseURL,
		CredentialID: target.CredentialID, CredentialPreserved: true}
	counts := []struct {
		destination *int64
		query       string
		args        []any
	}{
		{&impact.CredentialSupplierRefs, `SELECT COUNT(*) FROM suppliers WHERE credential_id=?`, []any{target.CredentialID}},
		{&impact.CredentialTargetRefs, `SELECT COUNT(*) FROM target_sites WHERE credential_id=?`, []any{target.CredentialID}},
		{&impact.PricingPolicies, `SELECT COUNT(*) FROM pricing_policies WHERE target_site_id=?`, []any{id}},
		{&impact.ChangeProposals, `SELECT COUNT(*) FROM change_proposals WHERE target_site_id=?`, []any{id}},
		{&impact.DecisionRecords, `SELECT (SELECT COUNT(*) FROM decision_snapshots WHERE proposal_id IN (SELECT id FROM change_proposals WHERE target_site_id=?))+
			(SELECT COUNT(*) FROM decision_outcomes WHERE proposal_id IN (SELECT id FROM change_proposals WHERE target_site_id=?))+
			(SELECT COUNT(*) FROM decision_reconcile_attempts WHERE proposal_id IN (SELECT id FROM change_proposals WHERE target_site_id=?))+
			(SELECT COUNT(*) FROM decision_effects WHERE proposal_id IN (SELECT id FROM change_proposals WHERE target_site_id=?))+
			(SELECT COUNT(*) FROM decision_exposures WHERE target_site_id=?)`, []any{id, id, id, id, id}},
		{&impact.ListingPlans, `SELECT COUNT(*) FROM listing_plans WHERE target_site_id=?`, []any{id}},
		{&impact.ListingChangeSets, `SELECT COUNT(*) FROM listing_change_sets WHERE target_site_id=?`, []any{id}},
		{&impact.ControlOperations, `SELECT COUNT(*) FROM target_control_operations WHERE target_site_id=?`, []any{id}},
		{&impact.ActiveLeases, `SELECT (SELECT COUNT(*) FROM target_change_leases WHERE target_site_id=? AND leased_until>?)+
			(SELECT COUNT(*) FROM target_control_leases WHERE target_site_id=? AND leased_until>?)+
			(SELECT COUNT(*) FROM target_control_operations WHERE target_site_id=? AND result_state='in_progress')`,
			[]any{id, formatTime(now), id, formatTime(now), id}},
		{&impact.LedgerSyncRecords, `SELECT (SELECT COUNT(*) FROM target_ledger_sync_configs WHERE target_site_id=?)+
			(SELECT COUNT(*) FROM target_ledger_sync_states WHERE target_site_id=?)+
			(SELECT COUNT(*) FROM target_ledger_user_profiles WHERE target_site_id=?)`, []any{id, id, id}},
		{&impact.LedgerEvents, `SELECT COUNT(*) FROM operating_ledger_events WHERE target_site_id=?`, []any{id}},
		{&impact.OpportunityRecords, `SELECT (SELECT COUNT(*) FROM opportunity_target_catalogs WHERE target_site_id=?)+
			(SELECT COUNT(*) FROM opportunity_model_usage WHERE target_site_id=?)`, []any{id, id}},
	}
	for _, count := range counts {
		if err := tx.QueryRowContext(ctx, count.query, count.args...).Scan(count.destination); err != nil {
			return domain.TargetDeletionImpact{}, fmt.Errorf("count target deletion impact: %w", err)
		}
	}
	impact.ImpactHash, err = targetDeletionImpactHash(impact, target)
	if err != nil {
		return domain.TargetDeletionImpact{}, err
	}
	impact.GeneratedAt = now
	return impact, nil
}

func targetDeletionImpactHash(impact domain.TargetDeletionImpact, target domain.TargetSite) (string, error) {
	impact.ImpactHash = ""
	impact.GeneratedAt = time.Time{}
	payload := struct {
		Impact    domain.TargetDeletionImpact `json:"impact"`
		Enabled   bool                        `json:"enabled"`
		WriteMode string                      `json:"write_mode"`
	}{Impact: impact, Enabled: target.Enabled, WriteMode: target.WriteMode}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
