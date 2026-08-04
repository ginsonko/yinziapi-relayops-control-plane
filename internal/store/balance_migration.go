package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const legacyQuotaEvidenceMigrationVersion = 2026072201

type legacyJSONRow struct {
	id      int64
	payload string
}

// migrateLegacyQuotaEvidence removes the false monetary meaning assigned by
// older builds to raw NewAPI quota values. It never looks up today's status or
// quota_per_unit: normalization is valid only when those facts were captured
// in the same observation.
func (s *Store) migrateLegacyQuotaEvidence(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy quota evidence migration: %w", err)
	}
	defer tx.Rollback()

	var applied int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, legacyQuotaEvidenceMigrationVersion).Scan(&applied)
	if err != nil {
		return fmt.Errorf("read legacy quota migration state: %w", err)
	}
	if applied > 0 {
		return tx.Commit()
	}

	if err := migrateQuotaJSONTable(ctx, tx, "catalog_snapshots", true); err != nil {
		return err
	}
	if err := migrateQuotaJSONTable(ctx, tx, "balance_observations", false); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`,
		legacyQuotaEvidenceMigrationVersion, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record legacy quota evidence migration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy quota evidence migration: %w", err)
	}
	return nil
}

func migrateQuotaJSONTable(ctx context.Context, tx *sql.Tx, table string, nestedBalance bool) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,payload_json FROM `+table+` ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read %s for quota evidence migration: %w", table, err)
	}
	items := make([]legacyJSONRow, 0)
	for rows.Next() {
		var item legacyJSONRow
		if err := rows.Scan(&item.id, &item.payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s quota evidence: %w", table, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate %s quota evidence: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s quota evidence rows: %w", table, err)
	}

	for _, item := range items {
		migrated, changed, err := migrateQuotaPayload([]byte(item.payload), nestedBalance)
		if err != nil {
			return fmt.Errorf("migrate %s row %d quota evidence: %w", table, item.id, err)
		}
		if !changed {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET payload_json=? WHERE id=?`, string(migrated), item.id); err != nil {
			return fmt.Errorf("update %s row %d quota evidence: %w", table, item.id, err)
		}
	}
	return nil
}

func migrateQuotaPayload(payload []byte, nestedBalance bool) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, false, err
	}
	if !nestedBalance {
		return migrateQuotaBalanceObject(root, payload)
	}
	balanceRaw, exists := root["balance"]
	if !exists || len(balanceRaw) == 0 || string(balanceRaw) == "null" {
		return payload, false, nil
	}
	var balance map[string]json.RawMessage
	if err := json.Unmarshal(balanceRaw, &balance); err != nil {
		return nil, false, err
	}
	migratedBalance, changed, err := migrateQuotaBalanceObject(balance, balanceRaw)
	if err != nil || !changed {
		return payload, changed, err
	}
	root["balance"] = migratedBalance
	migrated, err := json.Marshal(root)
	return migrated, true, err
}

func migrateQuotaBalanceObject(balance map[string]json.RawMessage, original []byte) ([]byte, bool, error) {
	currency := rawJSONString(balance["currency"])
	if !strings.EqualFold(strings.TrimSpace(currency), "quota") {
		return original, false, nil
	}

	rawValue := rawJSONString(balance["raw_value"])
	if strings.TrimSpace(rawValue) == "" {
		rawValue = rawScalarText(balance["amount"])
		if strings.TrimSpace(rawValue) == "" {
			rawValue = rawScalarText(balance["available"])
		}
	}
	setJSONString(balance, "raw_value", rawValue)
	setJSONString(balance, "raw_unit", "quota")

	// Amount, available, daily spend and runway were all interpreted in the
	// invalid currency. Keep the raw quota, but remove values that downstream
	// code could otherwise treat as normalized money.
	setJSONString(balance, "amount", "")
	setJSONString(balance, "available", "")
	setJSONString(balance, "daily_spend", "")
	setJSONString(balance, "currency", "")
	balance["runway_hours"] = json.RawMessage("null")
	setJSONString(balance, "confidence", "invalid_unit")
	if strings.TrimSpace(rawJSONString(balance["raw_display_text"])) == "" && rawValue != "" {
		setJSONString(balance, "raw_display_text", "raw quota="+rawValue)
	}
	if rawValue != "" {
		divisor := strings.TrimSpace(rawJSONString(balance["normalization_divisor"]))
		if divisor == "" {
			divisor = "unknown"
		}
		setJSONString(balance, "normalization_formula", "raw quota="+rawValue+" / quota_per_unit="+divisor+" = unknown (target currency unavailable)")
	}

	migrated, err := json.Marshal(balance)
	if err != nil {
		return nil, false, err
	}
	return migrated, true, nil
}

func hasValidQuotaNormalizationEvidence(balance map[string]json.RawMessage) bool {
	if strings.TrimSpace(rawJSONString(balance["raw_value"])) == "" ||
		!strings.EqualFold(strings.TrimSpace(rawJSONString(balance["raw_unit"])), "quota") ||
		strings.TrimSpace(rawJSONString(balance["normalization_formula"])) == "" ||
		!validMoneyCurrency(rawJSONString(balance["currency"])) {
		return false
	}
	divisor, err := decimal.NewFromString(strings.TrimSpace(rawJSONString(balance["normalization_divisor"])))
	return err == nil && divisor.IsPositive()
}

func validMoneyCurrency(value string) bool {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 3 || value == "QUOTA" {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func rawJSONString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawScalarText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if value := rawJSONString(raw); value != "" {
		return value
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func setJSONString(object map[string]json.RawMessage, key, value string) {
	encoded, _ := json.Marshal(value)
	object[key] = encoded
}
