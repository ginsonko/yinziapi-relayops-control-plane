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

var ErrPricingPolicyDeletionConflict = errors.New("定价规则删除预览已失效")

func (s *Store) PreviewPricingPolicyDeletion(ctx context.Context, id int64) (domain.PricingPolicyDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readPricingPolicyDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeletePricingPolicySafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.PricingPolicyDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE pricing_policies SET enabled=enabled WHERE id=?`, id)
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.PricingPolicyDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readPricingPolicyDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.PolicyName) {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("%w：确认名称与当前规则名称不一致", ErrPricingPolicyDeletionConflict)
	}
	if strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("%w：规则或关联数据已变化，请重新查看删除影响", ErrPricingPolicyDeletionConflict)
	}
	if impact.ActiveLeases > 0 {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("%w：目标站仍有正在执行的价格或控制操作，请稍后重试", ErrPricingPolicyDeletionConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE listing_change_sets SET pricing_policy_id=NULL WHERE pricing_policy_id=?`, id); err != nil {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("detach listing history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM change_proposals WHERE policy_id=?`, id); err != nil {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("delete policy proposals: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_policies WHERE id=?`, id); err != nil {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("delete pricing policy: %w", err)
	}
	metadata := map[string]any{"deleted_policy_id": impact.PolicyID, "deleted_policy_name": impact.PolicyName,
		"target_site_id": impact.TargetSiteID, "target_group": impact.TargetGroup,
		"change_proposals": impact.ChangeProposals, "decision_records": impact.DecisionRecords,
		"listing_references_detached": impact.ListingReferences, "remote_price_unchanged": true}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('pricing_policy_deleted','warning',?,?,NULL,?,?)`, "定价规则已删除",
		fmt.Sprintf("%s 已从 RelayOps 删除；本站现有价格未被回滚。", impact.PolicyName), mustJSON(metadata), formatTime(time.Now().UTC())); err != nil {
		return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("write pricing policy deletion audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	return impact, nil
}

func readPricingPolicyDeletionImpact(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (domain.PricingPolicyDeletionImpact, error) {
	policy, err := scanPolicy(tx.QueryRowContext(ctx, policySelect+` WHERE id=?`, id))
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	impact := domain.PricingPolicyDeletionImpact{PolicyID: policy.ID, PolicyName: policy.Name,
		TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, GeneratedAt: now}
	counts := []struct {
		destination *int64
		query       string
		args        []any
	}{
		{&impact.ChangeProposals, `SELECT COUNT(*) FROM change_proposals WHERE policy_id=?`, []any{id}},
		{&impact.DecisionRecords, `SELECT (SELECT COUNT(*) FROM decision_snapshots WHERE proposal_id IN (SELECT id FROM change_proposals WHERE policy_id=?))+
			(SELECT COUNT(*) FROM decision_outcomes WHERE proposal_id IN (SELECT id FROM change_proposals WHERE policy_id=?))+
			(SELECT COUNT(*) FROM decision_reconcile_attempts WHERE proposal_id IN (SELECT id FROM change_proposals WHERE policy_id=?))+
			(SELECT COUNT(*) FROM decision_effects WHERE proposal_id IN (SELECT id FROM change_proposals WHERE policy_id=?))+
			(SELECT COUNT(*) FROM decision_exposures WHERE proposal_id IN (SELECT id FROM change_proposals WHERE policy_id=?))`, []any{id, id, id, id, id}},
		{&impact.ListingReferences, `SELECT COUNT(*) FROM listing_change_sets WHERE pricing_policy_id=?`, []any{id}},
		{&impact.ActiveLeases, `SELECT (SELECT COUNT(*) FROM target_change_leases WHERE target_site_id=? AND leased_until>?)+
			(SELECT COUNT(*) FROM target_control_leases WHERE target_site_id=? AND leased_until>?)+
			(SELECT COUNT(*) FROM target_control_operations WHERE target_site_id=? AND result_state='in_progress')`,
			[]any{policy.TargetSiteID, formatTime(now), policy.TargetSiteID, formatTime(now), policy.TargetSiteID}},
	}
	for _, count := range counts {
		if err := tx.QueryRowContext(ctx, count.query, count.args...).Scan(count.destination); err != nil {
			return domain.PricingPolicyDeletionImpact{}, fmt.Errorf("count pricing policy deletion impact: %w", err)
		}
	}
	impact.ImpactHash, err = pricingPolicyDeletionImpactHash(impact, policy)
	if err != nil {
		return domain.PricingPolicyDeletionImpact{}, err
	}
	return impact, nil
}

func pricingPolicyDeletionImpactHash(impact domain.PricingPolicyDeletionImpact, policy domain.PricingPolicy) (string, error) {
	impact.ImpactHash = ""
	impact.GeneratedAt = time.Time{}
	encoded, err := json.Marshal(struct {
		Impact domain.PricingPolicyDeletionImpact `json:"impact"`
		Policy domain.PricingPolicy               `json:"policy"`
	}{Impact: impact, Policy: policy})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
