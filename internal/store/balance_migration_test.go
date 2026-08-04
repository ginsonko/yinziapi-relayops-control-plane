package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOpenMigratesLegacyQuotaEvidenceInSnapshotsAndBalanceHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-quota.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE catalog_snapshots (id INTEGER PRIMARY KEY,supplier_id INTEGER NOT NULL,payload_json TEXT NOT NULL,response_hash TEXT NOT NULL,observed_at TEXT NOT NULL)`,
		`CREATE TABLE balance_observations (id INTEGER PRIMARY KEY,supplier_id INTEGER NOT NULL,payload_json TEXT NOT NULL,observed_at TEXT NOT NULL)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	observed := "2026-07-21T12:00:00Z"
	catalogPayload := `{"supplier_id":7,"legacy_marker":"keep-catalog-field","balance":{"amount":"30965000","available":"30964000","daily_spend":"500000","currency":"quota","confidence":"high","observed_at":"2026-07-21T12:00:00Z","legacy_balance_field":"keep-balance-field"},"evidence":{"source_type":"authenticated_api","observed_at":"2026-07-21T12:00:00Z","response_hash":"catalog-a","warnings":[]}}`
	balancePayload := `{"amount":30965001.25,"available":"30964001.25","daily_spend":"500000","currency":"QUOTA","confidence":"high","observed_at":"2026-07-21T12:00:00Z","normalization_divisor":"500000","legacy_observation_field":"keep-observation-field"}`
	if _, err := raw.Exec(`INSERT INTO catalog_snapshots(id,supplier_id,payload_json,response_hash,observed_at) VALUES(1,7,?,?,?)`, catalogPayload, "catalog-a", observed); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO balance_observations(id,supplier_id,payload_json,observed_at) VALUES(1,7,?,?)`, balancePayload, observed); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy quota database: %v", err)
	}

	snapshot, err := st.LatestSnapshot(context.Background(), 7)
	if err != nil || snapshot.Balance == nil {
		t.Fatalf("LatestSnapshot = %#v, %v", snapshot, err)
	}
	assertMigratedUnknownQuota(t, *snapshot.Balance, "30965000")
	balance, err := st.LatestBalance(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	assertMigratedUnknownQuota(t, balance, "30965001.25")
	if balance.NormalizationDivisor != "500000" {
		t.Fatalf("same-row partial divisor was not preserved: %#v", balance)
	}

	var migratedCatalog, migratedBalance string
	if err := st.db.QueryRow(`SELECT payload_json FROM catalog_snapshots WHERE id=1`).Scan(&migratedCatalog); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT payload_json FROM balance_observations WHERE id=1`).Scan(&migratedBalance); err != nil {
		t.Fatal(err)
	}
	for payload, marker := range map[string]string{migratedCatalog: "keep-catalog-field", migratedBalance: "keep-observation-field"} {
		if !strings.Contains(payload, marker) {
			t.Fatalf("migration dropped unknown field %q from %s", marker, payload)
		}
	}
	if !strings.Contains(migratedCatalog, "keep-balance-field") {
		t.Fatalf("migration dropped nested unknown field: %s", migratedCatalog)
	}
	var applied int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, legacyQuotaEvidenceMigrationVersion).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration marker count=%d error=%v", applied, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// The migration marker makes reopening byte-stable rather than repeatedly
	// rewriting historical evidence.
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reopenedCatalog, reopenedBalance string
	_ = reopened.db.QueryRow(`SELECT payload_json FROM catalog_snapshots WHERE id=1`).Scan(&reopenedCatalog)
	_ = reopened.db.QueryRow(`SELECT payload_json FROM balance_observations WHERE id=1`).Scan(&reopenedBalance)
	if reopenedCatalog != migratedCatalog || reopenedBalance != migratedBalance {
		t.Fatal("legacy quota migration was not idempotent")
	}
}

func TestMigrateQuotaPayloadOnlyReclassifiesUnprovenQuotaCurrency(t *testing.T) {
	t.Parallel()
	normalized := []byte(`{"amount":"61.93","available":"61.93","currency":"USD","raw_value":"30965000","raw_unit":"quota","normalization_divisor":"500000","normalization_formula":"raw quota=30965000 / quota_per_unit=500000 = 61.93 USD"}`)
	got, changed, err := migrateQuotaPayload(normalized, false)
	if err != nil || changed || string(got) != string(normalized) {
		t.Fatalf("valid same-sample normalization changed: changed=%t error=%v payload=%s", changed, err, got)
	}

	legacy := []byte(`{"amount":"30965000","currency":"quota","raw_value":"30965000","raw_unit":"quota","normalization_divisor":"500000","normalization_formula":"partial legacy formula","observed_at":"2026-07-21T12:00:00Z"}`)
	got, changed, err = migrateQuotaPayload(legacy, false)
	if err != nil || !changed {
		t.Fatalf("quota currency was not migrated: changed=%t error=%v", changed, err)
	}
	var migrated domain.BalanceState
	if err := json.Unmarshal(got, &migrated); err != nil {
		t.Fatal(err)
	}
	assertMigratedUnknownQuota(t, migrated, "30965000")
}

func TestValidQuotaNormalizationEvidenceRequiresPositiveDivisorAndMoneyCurrency(t *testing.T) {
	t.Parallel()
	decode := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var result map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	valid := decode(`{"currency":"USD","raw_value":"1000","raw_unit":"quota","normalization_divisor":"500000","normalization_formula":"1000 / 500000"}`)
	if !hasValidQuotaNormalizationEvidence(valid) {
		t.Fatal("complete same-sample quota evidence was rejected")
	}
	for name, raw := range map[string]string{
		"quota is not money": `{"currency":"quota","raw_value":"1000","raw_unit":"quota","normalization_divisor":"500000","normalization_formula":"formula"}`,
		"zero divisor":       `{"currency":"USD","raw_value":"1000","raw_unit":"quota","normalization_divisor":"0","normalization_formula":"formula"}`,
		"missing formula":    `{"currency":"USD","raw_value":"1000","raw_unit":"quota","normalization_divisor":"500000"}`,
	} {
		if hasValidQuotaNormalizationEvidence(decode(raw)) {
			t.Fatalf("%s was accepted as complete evidence", name)
		}
	}
}

func assertMigratedUnknownQuota(t *testing.T, balance domain.BalanceState, rawValue string) {
	t.Helper()
	if balance.RawValue != rawValue || balance.RawUnit != "quota" {
		t.Fatalf("raw quota evidence = %#v, want %s quota", balance, rawValue)
	}
	if balance.Amount != "" || balance.Available != "" || balance.DailySpend != "" || balance.Currency != "" || balance.RunwayHours != nil {
		t.Fatalf("unproven normalized monetary fields were retained: %#v", balance)
	}
	if balance.Confidence != "invalid_unit" || !strings.Contains(balance.NormalizationFormula, "unknown") {
		t.Fatalf("unknown-unit explanation = %#v", balance)
	}
	if balance.ObservedAt.IsZero() || !balance.ObservedAt.Equal(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("observation time changed: %#v", balance.ObservedAt)
	}
}
