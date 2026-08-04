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

var ErrConnectorManifestConflict = errors.New("Connector Manifest 版本或引用已变化")

func (s *Store) CreateConnectorManifest(ctx context.Context, record *domain.ConnectorManifestRecord) error {
	if record == nil {
		return fmt.Errorf("connector manifest is required")
	}
	encoded, err := json.Marshal(record.Manifest)
	if err != nil {
		return fmt.Errorf("encode connector manifest: %w", err)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO connector_manifests(
		name,schema_version,adapter_family,manifest_json,source,validation_status,validation_message,created_at,updated_at
	) VALUES(?,?,?,?,?,?,?,?,?)`, record.Name, record.SchemaVersion, record.AdapterFamily, string(encoded), record.Source,
		record.ValidationStatus, record.ValidationMessage, formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	record.ID, err = result.LastInsertId()
	if err != nil {
		return err
	}
	record.CreatedAt = now
	record.UpdatedAt = now
	return nil
}

func (s *Store) ListConnectorManifests(ctx context.Context) ([]domain.ConnectorManifestRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,schema_version,adapter_family,manifest_json,source,
		validation_status,validation_message,created_at,updated_at FROM connector_manifests ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ConnectorManifestRecord, 0)
	for rows.Next() {
		item, err := scanConnectorManifest(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetConnectorManifest(ctx context.Context, id int64) (domain.ConnectorManifestRecord, error) {
	return scanConnectorManifest(s.db.QueryRowContext(ctx, `SELECT id,name,schema_version,adapter_family,manifest_json,source,
		validation_status,validation_message,created_at,updated_at FROM connector_manifests WHERE id=?`, id))
}

func (s *Store) UpdateConnectorManifest(ctx context.Context, record *domain.ConnectorManifestRecord, expectedUpdatedAt time.Time) error {
	return s.UpdateConnectorManifestSafely(ctx, record, expectedUpdatedAt, nil)
}

func (s *Store) UpdateConnectorManifestSafely(ctx context.Context, record *domain.ConnectorManifestRecord, expectedUpdatedAt time.Time, validateBoundSupplier func(domain.Supplier) error) error {
	if record == nil || record.ID <= 0 || expectedUpdatedAt.IsZero() {
		return ErrConnectorManifestConflict
	}
	encoded, err := json.Marshal(record.Manifest)
	if err != nil {
		return fmt.Errorf("encode connector manifest: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	lockResult, err := tx.ExecContext(ctx, `UPDATE connector_manifests SET validation_status=validation_status WHERE id=? AND updated_at=?`,
		record.ID, formatTime(expectedUpdatedAt))
	if err != nil {
		return err
	}
	if affected, _ := lockResult.RowsAffected(); affected != 1 {
		return ErrConnectorManifestConflict
	}
	if validateBoundSupplier != nil {
		rows, err := tx.QueryContext(ctx, `SELECT supplier_id FROM supplier_manifest_bindings WHERE manifest_id=? ORDER BY supplier_id`, record.ID)
		if err != nil {
			return err
		}
		var supplierIDs []int64
		for rows.Next() {
			var supplierID int64
			if err := rows.Scan(&supplierID); err != nil {
				_ = rows.Close()
				return err
			}
			supplierIDs = append(supplierIDs, supplierID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, supplierID := range supplierIDs {
			supplier, err := scanSupplier(tx.QueryRowContext(ctx, supplierSelect+` WHERE id=?`, supplierID))
			if err != nil {
				return err
			}
			if err := validateBoundSupplier(supplier); err != nil {
				return fmt.Errorf("%w：已绑定站点 %q 与新版 Manifest 不兼容：%v", ErrConnectorManifestConflict, supplier.Name, err)
			}
		}
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE connector_manifests SET name=?,schema_version=?,adapter_family=?,manifest_json=?,
		source=?,validation_status=?,validation_message=?,updated_at=? WHERE id=?`, record.Name, record.SchemaVersion,
		record.AdapterFamily, string(encoded), record.Source, record.ValidationStatus, record.ValidationMessage, formatTime(now), record.ID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrConnectorManifestConflict
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	record.UpdatedAt = now
	return nil
}

func (s *Store) PreviewConnectorManifestDeletion(ctx context.Context, id int64) (domain.ConnectorManifestDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	impact, err := readConnectorManifestDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	return impact, nil
}

func (s *Store) DeleteConnectorManifestSafely(ctx context.Context, id int64, confirmName, expectedHash string) (domain.ConnectorManifestDeletionImpact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE connector_manifests SET validation_status=validation_status WHERE id=?`, id)
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return domain.ConnectorManifestDeletionImpact{}, sql.ErrNoRows
	}
	impact, err := readConnectorManifestDeletionImpact(ctx, tx, id, time.Now().UTC())
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if strings.TrimSpace(confirmName) != strings.TrimSpace(impact.Name) || strings.TrimSpace(expectedHash) == "" || expectedHash != impact.ImpactHash {
		return domain.ConnectorManifestDeletionImpact{}, ErrConnectorManifestConflict
	}
	if len(impact.SupplierRefs) > 0 {
		return domain.ConnectorManifestDeletionImpact{}, fmt.Errorf("%w：仍有 %d 个上游绑定此 Manifest", ErrConnectorManifestConflict, len(impact.SupplierRefs))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connector_manifests WHERE id=?`, id); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('connector_manifest_deleted','warning',?,?,NULL,?,?)`, "Connector Manifest 已删除", impact.Name+" 已删除；删除前没有站点绑定。",
		mustJSON(map[string]any{"deleted_manifest_id": id, "deleted_manifest_name": impact.Name}), formatTime(time.Now().UTC())); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	return impact, nil
}

func readConnectorManifestDeletionImpact(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (domain.ConnectorManifestDeletionImpact, error) {
	record, err := scanConnectorManifest(tx.QueryRowContext(ctx, `SELECT id,name,schema_version,adapter_family,manifest_json,source,
		validation_status,validation_message,created_at,updated_at FROM connector_manifests WHERE id=?`, id))
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	impact := domain.ConnectorManifestDeletionImpact{
		ManifestID:   id,
		Name:         record.Name,
		SupplierRefs: []domain.ConfigurationReference{},
		GeneratedAt:  now,
	}
	rows, err := tx.QueryContext(ctx, `SELECT suppliers.id,suppliers.name FROM supplier_manifest_bindings
		JOIN suppliers ON suppliers.id=supplier_manifest_bindings.supplier_id WHERE manifest_id=? ORDER BY suppliers.name,suppliers.id`, id)
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	for rows.Next() {
		var item domain.ConfigurationReference
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			_ = rows.Close()
			return domain.ConnectorManifestDeletionImpact{}, err
		}
		impact.SupplierRefs = append(impact.SupplierRefs, item)
	}
	if err := rows.Close(); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	impact.ImpactHash, err = connectorObjectHash(struct {
		Record domain.ConnectorManifestRecord  `json:"record"`
		Refs   []domain.ConfigurationReference `json:"refs"`
	}{Record: record, Refs: impact.SupplierRefs})
	if err != nil {
		return domain.ConnectorManifestDeletionImpact{}, err
	}
	return impact, nil
}

type manifestScanner interface {
	Scan(...any) error
}

func scanConnectorManifest(scanner manifestScanner) (domain.ConnectorManifestRecord, error) {
	var item domain.ConnectorManifestRecord
	var manifestJSON, created, updated string
	if err := scanner.Scan(&item.ID, &item.Name, &item.SchemaVersion, &item.AdapterFamily, &manifestJSON, &item.Source,
		&item.ValidationStatus, &item.ValidationMessage, &created, &updated); err != nil {
		return domain.ConnectorManifestRecord{}, err
	}
	if err := json.Unmarshal([]byte(manifestJSON), &item.Manifest); err != nil {
		return domain.ConnectorManifestRecord{}, fmt.Errorf("decode stored connector manifest %d: %w", item.ID, err)
	}
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) BindSupplierConnectorManifest(ctx context.Context, supplierID, manifestID int64, boundOrigin string, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO supplier_manifest_bindings(supplier_id,manifest_id,bound_origin,bound_at)
		VALUES(?,?,?,?) ON CONFLICT(supplier_id) DO UPDATE SET manifest_id=excluded.manifest_id,
		bound_origin=excluded.bound_origin,bound_at=excluded.bound_at`, supplierID, manifestID, boundOrigin, formatTime(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UnbindSupplierConnectorManifest(ctx context.Context, supplierID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM supplier_manifest_bindings WHERE supplier_id=?`, supplierID)
	return err
}

func (s *Store) GetSupplierManifestBinding(ctx context.Context, supplierID int64) (domain.SupplierManifestBinding, error) {
	var binding domain.SupplierManifestBinding
	var boundAt string
	if err := s.db.QueryRowContext(ctx, `SELECT supplier_id,manifest_id,bound_origin,bound_at
		FROM supplier_manifest_bindings WHERE supplier_id=?`, supplierID).
		Scan(&binding.SupplierID, &binding.ManifestID, &binding.BoundOrigin, &boundAt); err != nil {
		return domain.SupplierManifestBinding{}, err
	}
	binding.BoundAt = parseTime(boundAt)
	manifest, err := s.GetConnectorManifest(ctx, binding.ManifestID)
	if err != nil {
		return domain.SupplierManifestBinding{}, err
	}
	binding.Manifest = &manifest
	return binding, nil
}

// BoundConnectorManifest implements connectors.ConnectorManifestLoader without
// making the connector package depend on the concrete Store type.
func (s *Store) BoundConnectorManifest(ctx context.Context, supplierID int64) (domain.ConnectorManifestRecord, bool, error) {
	binding, err := s.GetSupplierManifestBinding(ctx, supplierID)
	if err != nil {
		if IsNotFound(err) {
			return domain.ConnectorManifestRecord{}, false, nil
		}
		return domain.ConnectorManifestRecord{}, false, err
	}
	if binding.Manifest == nil {
		return domain.ConnectorManifestRecord{}, false, nil
	}
	return *binding.Manifest, true, nil
}
