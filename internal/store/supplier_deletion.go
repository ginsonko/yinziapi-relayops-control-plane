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

var ErrSupplierDeletionConflict = errors.New("上游删除预览已失效")
var ErrSupplierDeletionAuthorityRetentionRequired = errors.New("protocol-2 supplier deletion requires retained-history migration")

func (s *Store) PreviewSupplierDeletion(ctx context.Context, id int64) (domain.SupplierDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readSupplierDeletionImpact(ctx, tx, id)
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeleteSupplierSafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.SupplierDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Acquire SQLite's write lock before re-reading the preview inputs.
	result, err := tx.ExecContext(ctx, `UPDATE suppliers SET enabled=enabled WHERE id=?`, id)
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.SupplierDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readSupplierDeletionImpact(ctx, tx, id)
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.SupplierName) {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("%w：确认名称与当前站点名称不一致", ErrSupplierDeletionConflict)
	}
	if strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("%w：关联数据已变化，请重新查看删除影响", ErrSupplierDeletionConflict)
	}
	var authorityProtocol int
	if err := tx.QueryRowContext(ctx, `SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1`).Scan(&authorityProtocol); err != nil {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("read collection authority before supplier deletion: %w", err)
	}
	if authorityProtocol == authorityProtocolV2 {
		return domain.SupplierDeletionImpact{}, ErrSupplierDeletionAuthorityRetentionRequired
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM listing_change_sets WHERE plan_id IN (SELECT id FROM listing_plans WHERE supplier_id=?)
			OR pricing_policy_id IN (SELECT id FROM pricing_policies WHERE supplier_id=?)`, []any{id, id}},
		{`DELETE FROM listing_plans WHERE supplier_id=?`, []any{id}},
		{`DELETE FROM change_proposals WHERE policy_id IN (SELECT id FROM pricing_policies WHERE supplier_id=?)`, []any{id}},
		{`DELETE FROM pricing_policies WHERE supplier_id=?`, []any{id}},
		{`UPDATE activities SET supplier_id=NULL WHERE supplier_id=?`, []any{id}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return domain.SupplierDeletionImpact{}, fmt.Errorf("prepare supplier deletion: %w", err)
		}
	}

	metadata := map[string]any{
		"deleted_supplier_id":     id,
		"deleted_supplier_name":   impact.SupplierName,
		"deleted_supplier_origin": impact.BaseURL,
		"credential_preserved":    impact.CredentialPreserved,
		"pricing_policies":        impact.PricingPolicies,
		"change_proposals":        impact.ChangeProposals,
		"listing_plans":           impact.ListingPlans,
		"catalog_snapshots":       impact.CatalogSnapshots,
		"price_observations":      impact.PriceObservations,
		"balance_observations":    impact.BalanceObservations,
		"collection_attempts":     impact.CollectionAttempts,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('supplier_deleted','warning',?,?,NULL,?,?)`, "上游站点已删除", fmt.Sprintf("%s（%s）已由管理员确认删除；独立凭据未删除。", impact.SupplierName, impact.BaseURL), mustJSON(metadata), formatTime(time.Now().UTC())); err != nil {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("write supplier deletion audit: %w", err)
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM suppliers WHERE id=?`, id)
	if err != nil {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("delete supplier: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return domain.SupplierDeletionImpact{}, fmt.Errorf("%w：站点在删除过程中发生变化", ErrSupplierDeletionConflict)
	}
	if err := tx.Commit(); err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	return impact, nil
}

func readSupplierDeletionImpact(ctx context.Context, tx *sql.Tx, id int64) (domain.SupplierDeletionImpact, error) {
	supplier, err := scanSupplier(tx.QueryRowContext(ctx, supplierSelect+` WHERE id=?`, id))
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	impact := domain.SupplierDeletionImpact{
		SupplierID: supplier.ID, SupplierName: supplier.Name, BaseURL: supplier.BaseURL,
		CredentialID: supplier.CredentialID, CredentialPreserved: supplier.CredentialID != nil,
	}
	counts := []struct {
		destination *int64
		query       string
		args        []any
	}{
		{&impact.PricingPolicies, `SELECT COUNT(*) FROM pricing_policies WHERE supplier_id=?`, []any{id}},
		{&impact.ChangeProposals, `SELECT COUNT(*) FROM change_proposals WHERE policy_id IN (SELECT id FROM pricing_policies WHERE supplier_id=?)`, []any{id}},
		{&impact.ListingPlans, `SELECT COUNT(*) FROM listing_plans WHERE supplier_id=?`, []any{id}},
		{&impact.ListingChangeSets, `SELECT COUNT(*) FROM listing_change_sets WHERE plan_id IN (SELECT id FROM listing_plans WHERE supplier_id=?) OR pricing_policy_id IN (SELECT id FROM pricing_policies WHERE supplier_id=?)`, []any{id, id}},
		{&impact.CatalogSnapshots, `SELECT COUNT(*) FROM catalog_snapshots WHERE supplier_id=?`, []any{id}},
		{&impact.PriceObservations, `SELECT COUNT(*) FROM price_observations WHERE supplier_id=?`, []any{id}},
		{&impact.BalanceObservations, `SELECT COUNT(*) FROM balance_observations WHERE supplier_id=?`, []any{id}},
		{&impact.Discoveries, `SELECT COUNT(*) FROM discoveries WHERE supplier_id=?`, []any{id}},
		{&impact.CollectionAttempts, `SELECT COUNT(*) FROM collection_attempts WHERE supplier_id=?`, []any{id}},
		{&impact.RequestEvidence, `SELECT COUNT(*) FROM request_evidence WHERE supplier_id=?`, []any{id}},
		{&impact.AuthCapabilities, `SELECT COUNT(*) FROM auth_capabilities WHERE supplier_id=?`, []any{id}},
		{&impact.PairingSessions, `SELECT COUNT(*) FROM pairing_tokens WHERE supplier_id=?`, []any{id}},
		{&impact.ManifestBindings, `SELECT COUNT(*) FROM supplier_manifest_bindings WHERE supplier_id=?`, []any{id}},
		{&impact.CollectionLeases, `SELECT COUNT(*) FROM collection_leases WHERE supplier_id=?`, []any{id}},
		{&impact.ActivitiesDetached, `SELECT COUNT(*) FROM activities WHERE supplier_id=?`, []any{id}},
	}
	for _, count := range counts {
		if err := tx.QueryRowContext(ctx, count.query, count.args...).Scan(count.destination); err != nil {
			return domain.SupplierDeletionImpact{}, fmt.Errorf("count supplier deletion impact: %w", err)
		}
	}
	impact.ImpactHash, err = supplierDeletionImpactHash(impact, supplier.UpdatedAt)
	if err != nil {
		return domain.SupplierDeletionImpact{}, err
	}
	impact.GeneratedAt = time.Now().UTC()
	return impact, nil
}

func supplierDeletionImpactHash(impact domain.SupplierDeletionImpact, supplierUpdatedAt time.Time) (string, error) {
	impact.ImpactHash = ""
	impact.GeneratedAt = time.Time{}
	payload := struct {
		Impact            domain.SupplierDeletionImpact `json:"impact"`
		SupplierUpdatedAt time.Time                     `json:"supplier_updated_at"`
	}{Impact: impact, SupplierUpdatedAt: supplierUpdatedAt}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
