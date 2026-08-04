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

var ErrCredentialDeletionConflict = errors.New("凭据删除预览已失效")

func (s *Store) PreviewCredentialDeletion(ctx context.Context, id int64) (domain.CredentialDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readCredentialDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeleteCredentialSafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.CredentialDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE credentials SET status=status WHERE id=?`, id)
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.CredentialDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readCredentialDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.CredentialName) {
		return domain.CredentialDeletionImpact{}, fmt.Errorf("%w：确认名称与当前凭据名称不一致", ErrCredentialDeletionConflict)
	}
	if strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.CredentialDeletionImpact{}, fmt.Errorf("%w：凭据或引用关系已变化，请重新查看删除影响", ErrCredentialDeletionConflict)
	}
	if len(impact.SupplierRefs) > 0 || len(impact.TargetRefs) > 0 || impact.ListingPlanRefs > 0 {
		return domain.CredentialDeletionImpact{}, fmt.Errorf("%w：凭据仍被上游、目标站或上架计划引用，请先更换授权或删除对应配置", ErrCredentialDeletionConflict)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id=?`, id); err != nil {
		return domain.CredentialDeletionImpact{}, fmt.Errorf("delete credential: %w", err)
	}
	metadata := map[string]any{"deleted_credential_id": impact.CredentialID, "deleted_credential_name": impact.CredentialName,
		"site_url": impact.SiteURL, "kind": impact.Kind, "renewal_removed": impact.RenewalConfigured}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('credential_deleted','warning',?,?,NULL,?,?)`, "孤立凭据已删除",
		fmt.Sprintf("%s（%s）已删除；删除前没有任何站点或计划引用。", impact.CredentialName, impact.SiteURL),
		mustJSON(metadata), formatTime(time.Now().UTC())); err != nil {
		return domain.CredentialDeletionImpact{}, fmt.Errorf("write credential deletion audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	return impact, nil
}

func readCredentialDeletionImpact(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (domain.CredentialDeletionImpact, error) {
	impact := domain.CredentialDeletionImpact{
		SupplierRefs: []domain.ConfigurationReference{},
		TargetRefs:   []domain.ConfigurationReference{},
	}
	var createdAt, updatedAt string
	if err := tx.QueryRowContext(ctx, `SELECT id,name,kind,site_url,created_at,updated_at FROM credentials WHERE id=?`, id).
		Scan(&impact.CredentialID, &impact.CredentialName, &impact.Kind, &impact.SiteURL, &createdAt, &updatedAt); err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	for _, source := range []struct {
		destination *[]domain.ConfigurationReference
		query       string
	}{
		{&impact.SupplierRefs, `SELECT id,name FROM suppliers WHERE credential_id=? ORDER BY name,id`},
		{&impact.TargetRefs, `SELECT id,name FROM target_sites WHERE credential_id=? ORDER BY name,id`},
	} {
		rows, err := tx.QueryContext(ctx, source.query, id)
		if err != nil {
			return domain.CredentialDeletionImpact{}, err
		}
		for rows.Next() {
			var item domain.ConfigurationReference
			if err := rows.Scan(&item.ID, &item.Name); err != nil {
				_ = rows.Close()
				return domain.CredentialDeletionImpact{}, err
			}
			*source.destination = append(*source.destination, item)
		}
		if err := rows.Close(); err != nil {
			return domain.CredentialDeletionImpact{}, err
		}
		if err := rows.Err(); err != nil {
			return domain.CredentialDeletionImpact{}, err
		}
	}
	var renewal int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM listing_plans WHERE upstream_credential_id=?`, id).Scan(&impact.ListingPlanRefs); err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM credential_renewal_state WHERE credential_id=? AND configured=1`, id).Scan(&renewal); err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	impact.RenewalConfigured = renewal > 0
	impact.GeneratedAt = now
	encoded, err := json.Marshal(struct {
		Impact    domain.CredentialDeletionImpact `json:"impact"`
		CreatedAt string                          `json:"created_at"`
		UpdatedAt string                          `json:"updated_at"`
	}{Impact: func() domain.CredentialDeletionImpact {
		copy := impact
		copy.ImpactHash = ""
		copy.GeneratedAt = time.Time{}
		return copy
	}(), CreatedAt: createdAt, UpdatedAt: updatedAt})
	if err != nil {
		return domain.CredentialDeletionImpact{}, err
	}
	sum := sha256.Sum256(encoded)
	impact.ImpactHash = hex.EncodeToString(sum[:])
	return impact, nil
}
