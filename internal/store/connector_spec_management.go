package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

var ErrConnectorSpecConflict = errors.New("Connector 配置版本或引用已变化")

func (s *Store) GetConnectorSpec(ctx context.Context, id int64) (domain.ConnectorSpec, error) {
	return scanConnectorSpec(s.db.QueryRowContext(ctx, `SELECT id,name,schema_version,spec_json,enabled,created_at,updated_at FROM connector_specs WHERE id=?`, id))
}

func (s *Store) UpdateConnectorSpec(ctx context.Context, spec *domain.ConnectorSpec, expectedUpdatedAt time.Time) error {
	if spec == nil || spec.ID <= 0 || expectedUpdatedAt.IsZero() {
		return ErrConnectorSpecConflict
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE connector_specs SET name=?,schema_version=?,spec_json=?,enabled=?,updated_at=? WHERE id=? AND updated_at=?`,
		spec.Name, spec.SchemaVersion, spec.SpecJSON, boolInt(spec.Enabled), formatTime(now), spec.ID, formatTime(expectedUpdatedAt))
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConnectorSpecConflict
	}
	spec.UpdatedAt = now
	return nil
}

func (s *Store) PreviewConnectorSpecDeletion(ctx context.Context, id int64) (domain.ConnectorSpecDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readConnectorSpecDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeleteConnectorSpecSafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.ConnectorSpecDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE connector_specs SET enabled=enabled WHERE id=?`, id)
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.ConnectorSpecDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readConnectorSpecDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.Name) || strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.ConnectorSpecDeletionImpact{}, ErrConnectorSpecConflict
	}
	if len(impact.SupplierRefs) > 0 {
		return domain.ConnectorSpecDeletionImpact{}, fmt.Errorf("%w：仍有 %d 个上游使用此 Connector", ErrConnectorSpecConflict, len(impact.SupplierRefs))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_specs WHERE id=?`, id); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('connector_spec_deleted','warning',?,?,NULL,?,?)`, "自定义 Connector 已删除", impact.Name+" 已删除；删除前没有站点引用。",
		mustJSON(map[string]any{"deleted_connector_spec_id": id, "deleted_connector_spec_name": impact.Name}), formatTime(time.Now().UTC())); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	return impact, nil
}

func readConnectorSpecDeletionImpact(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (domain.ConnectorSpecDeletionImpact, error) {
	spec, err := scanConnectorSpec(tx.QueryRowContext(ctx, `SELECT id,name,schema_version,spec_json,enabled,created_at,updated_at FROM connector_specs WHERE id=?`, id))
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	impact := domain.ConnectorSpecDeletionImpact{
		ConnectorSpecID: spec.ID,
		Name:            spec.Name,
		SupplierRefs:    []domain.ConfigurationReference{},
		GeneratedAt:     now,
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,name,metadata_json FROM suppliers ORDER BY name,id`)
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	for rows.Next() {
		var supplierID int64
		var name, metadataJSON string
		if err := rows.Scan(&supplierID, &name, &metadataJSON); err != nil {
			_ = rows.Close()
			return domain.ConnectorSpecDeletionImpact{}, err
		}
		var metadata map[string]string
		if json.Unmarshal([]byte(metadataJSON), &metadata) == nil && strings.TrimSpace(metadata["connector_spec_id"]) == strconv.FormatInt(id, 10) {
			impact.SupplierRefs = append(impact.SupplierRefs, domain.ConfigurationReference{ID: supplierID, Name: name})
		}
	}
	if err := rows.Close(); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	impact.ImpactHash, err = connectorObjectHash(struct {
		Spec domain.ConnectorSpec            `json:"spec"`
		Refs []domain.ConfigurationReference `json:"refs"`
	}{Spec: spec, Refs: impact.SupplierRefs})
	if err != nil {
		return domain.ConnectorSpecDeletionImpact{}, err
	}
	return impact, nil
}

type connectorSpecScanner interface{ Scan(...any) error }

func scanConnectorSpec(scanner connectorSpecScanner) (domain.ConnectorSpec, error) {
	var item domain.ConnectorSpec
	var enabled int
	var created, updated string
	if err := scanner.Scan(&item.ID, &item.Name, &item.SchemaVersion, &item.SpecJSON, &enabled, &created, &updated); err != nil {
		return domain.ConnectorSpec{}, err
	}
	item.Enabled = enabled != 0
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func connectorObjectHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
