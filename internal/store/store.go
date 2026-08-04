package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string

	configMu          sync.RWMutex
	collectionLeaseMu sync.Mutex
	writeGate         chan struct{}
	balanceHeartbeat  time.Duration
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(250)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// WAL permits concurrent readers while retaining a single SQLite writer.
	// Keep the pool bounded so a long intelligence report cannot block short
	// pairing and administration requests behind one shared connection.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db, path: path, writeGate: make(chan struct{}, 1), balanceHeartbeat: 6 * time.Hour}
	if err := store.preflightActiveAuthorityBeforeMigration(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.auditActiveAuthorityOnOpen(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DatabasePath() string { return s.path }

func (s *Store) DatabaseStorageBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(s.path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS credentials (
            id INTEGER PRIMARY KEY,
            name TEXT NOT NULL,
            kind TEXT NOT NULL,
            site_url TEXT NOT NULL,
            encrypted_payload TEXT NOT NULL,
            expires_at TEXT,
            status TEXT NOT NULL DEFAULT 'active',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS credential_renewal_state (
			credential_id INTEGER PRIMARY KEY REFERENCES credentials(id) ON DELETE CASCADE,
			configured INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'unconfigured',
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			last_attempt_at TEXT,
			last_success_at TEXT,
			next_attempt_at TEXT,
			human_action TEXT NOT NULL DEFAULT '',
			last_error_class TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_credential_renewal_due
			ON credential_renewal_state(enabled,state,next_attempt_at)`,
		`CREATE TABLE IF NOT EXISTS suppliers (
            id INTEGER PRIMARY KEY,
            name TEXT NOT NULL,
            base_url TEXT NOT NULL UNIQUE,
            adapter TEXT NOT NULL DEFAULT 'auto',
            pricing_path TEXT NOT NULL DEFAULT '',
            balance_path TEXT NOT NULL DEFAULT '',
            credential_id INTEGER REFERENCES credentials(id),
            enabled INTEGER NOT NULL DEFAULT 1,
            poll_interval_seconds INTEGER NOT NULL DEFAULT 300,
			recharge_value_factor TEXT NOT NULL DEFAULT '1',
            focus_groups_json TEXT NOT NULL DEFAULT '[]',
            focus_models_json TEXT NOT NULL DEFAULT '[]',
            metadata_json TEXT NOT NULL DEFAULT '{}',
            last_status TEXT NOT NULL DEFAULT 'pending',
            last_error TEXT NOT NULL DEFAULT '',
			last_error_class TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT,
			last_success_at TEXT,
			last_success_duration_ms INTEGER,
			last_failure_duration_ms INTEGER,
            last_collected_at TEXT,
            next_collection_at TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS catalog_snapshots (
            id INTEGER PRIMARY KEY,
            supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
            payload_json TEXT NOT NULL,
            response_hash TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			total_observations INTEGER NOT NULL DEFAULT 1,
			consecutive_observations INTEGER NOT NULL DEFAULT 1,
			last_seen TEXT NOT NULL DEFAULT ''
        )`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_supplier_time ON catalog_snapshots(supplier_id, observed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS price_observations (
            id INTEGER PRIMARY KEY,
            supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
            entity_type TEXT NOT NULL,
            entity_key TEXT NOT NULL,
            group_name TEXT NOT NULL DEFAULT '',
            model_name TEXT NOT NULL DEFAULT '',
            price_json TEXT NOT NULL,
            evidence_json TEXT NOT NULL,
            observed_at TEXT NOT NULL,
            previous_hash TEXT NOT NULL DEFAULT ''
        )`,
		`CREATE INDEX IF NOT EXISTS idx_price_supplier_entity_time ON price_observations(supplier_id, entity_key, observed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS balance_observations (
            id INTEGER PRIMARY KEY,
            supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
            payload_json TEXT NOT NULL,
            observed_at TEXT NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS idx_balance_supplier_time ON balance_observations(supplier_id, observed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS target_sites (
            id INTEGER PRIMARY KEY,
            name TEXT NOT NULL,
            base_url TEXT NOT NULL UNIQUE,
            adapter TEXT NOT NULL,
            credential_id INTEGER NOT NULL REFERENCES credentials(id),
            enabled INTEGER NOT NULL DEFAULT 1,
			write_mode TEXT NOT NULL DEFAULT 'approval',
			last_status TEXT NOT NULL DEFAULT 'pending',
			last_error TEXT NOT NULL DEFAULT '',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS pricing_policies (
            id INTEGER PRIMARY KEY,
            name TEXT NOT NULL,
            supplier_id INTEGER NOT NULL REFERENCES suppliers(id),
            source_group TEXT NOT NULL DEFAULT '',
            source_model_pattern TEXT NOT NULL DEFAULT '*',
			source_basis TEXT NOT NULL DEFAULT 'group_ratio',
            target_site_id INTEGER NOT NULL REFERENCES target_sites(id),
            target_group TEXT NOT NULL,
            rule_type TEXT NOT NULL,
            value TEXT NOT NULL,
            profit_basis TEXT NOT NULL DEFAULT 'contribution',
            revenue_cash_factor TEXT NOT NULL DEFAULT '1',
            cost_cash_factor TEXT NOT NULL DEFAULT '1',
            risk_buffer_rate TEXT NOT NULL DEFAULT '0',
            allow_negative_profit INTEGER NOT NULL DEFAULT 0,
            subsidy_budget_daily TEXT NOT NULL DEFAULT '0',
            minimum_ratio TEXT NOT NULL DEFAULT '0',
            maximum_ratio TEXT NOT NULL DEFAULT '0',
            rounding_step TEXT NOT NULL DEFAULT '0.001',
            only_follow_increase INTEGER NOT NULL DEFAULT 0,
            only_follow_decrease INTEGER NOT NULL DEFAULT 0,
            enabled INTEGER NOT NULL DEFAULT 1,
            execution_mode TEXT NOT NULL DEFAULT 'approval',
            coverage_mode TEXT NOT NULL DEFAULT 'threshold',
            minimum_coverage_ratio TEXT NOT NULL DEFAULT '0.8',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            UNIQUE(target_site_id, target_group)
        )`,
		`CREATE TABLE IF NOT EXISTS change_proposals (
            id INTEGER PRIMARY KEY,
            policy_id INTEGER NOT NULL REFERENCES pricing_policies(id),
            target_site_id INTEGER NOT NULL REFERENCES target_sites(id),
            target_group TEXT NOT NULL,
            expected_old TEXT NOT NULL,
            proposed_value TEXT NOT NULL,
            expected_hash TEXT NOT NULL,
            calculation_json TEXT NOT NULL,
			group_model_pricing_plan_json TEXT NOT NULL DEFAULT '{}',
			group_model_pricing_receipt_json TEXT NOT NULL DEFAULT '{}',
            status TEXT NOT NULL,
            mode TEXT NOT NULL,
            idempotency_key TEXT NOT NULL UNIQUE,
            readback_value TEXT NOT NULL DEFAULT '',
            error TEXT NOT NULL DEFAULT '',
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS listing_plans (
			id INTEGER PRIMARY KEY,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id),
			discovery_kind TEXT NOT NULL,
			discovery_external_id TEXT NOT NULL,
			source_snapshot_hash TEXT NOT NULL,
			source_observed_at TEXT NOT NULL,
			source_group TEXT NOT NULL,
			source_ratio TEXT NOT NULL,
			equivalent_cost_ratio TEXT NOT NULL,
			cost_ratio_vector_json TEXT NOT NULL DEFAULT '{}',
			target_catalog_hash TEXT NOT NULL,
			profit_verified INTEGER NOT NULL DEFAULT 0,
			profit_verification TEXT NOT NULL DEFAULT '',
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id),
			target_group_mode TEXT NOT NULL,
			target_group TEXT NOT NULL,
			expected_target_hash TEXT NOT NULL,
			expected_group_ratios_json TEXT NOT NULL DEFAULT '{}',
			desired_target_hash TEXT NOT NULL DEFAULT '',
			expected_group_existed INTEGER NOT NULL,
			expected_group_ratio TEXT NOT NULL DEFAULT '',
			proposed_group_ratio TEXT NOT NULL,
			upstream_credential_id INTEGER NOT NULL REFERENCES credentials(id),
			channel_json TEXT NOT NULL,
			pricing_rule_json TEXT NOT NULL,
			calculation_json TEXT NOT NULL,
			group_model_pricing_plan_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_listing_plans_target_status ON listing_plans(target_site_id,status,updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS listing_change_sets (
			id INTEGER PRIMARY KEY,
			plan_id INTEGER NOT NULL REFERENCES listing_plans(id),
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id),
			status TEXT NOT NULL,
			step TEXT NOT NULL,
			idempotency_key TEXT NOT NULL UNIQUE,
			ownership_marker TEXT NOT NULL UNIQUE,
			expected_target_hash TEXT NOT NULL,
			applied_target_hash TEXT NOT NULL DEFAULT '',
			created_channel_id INTEGER,
			pricing_policy_id INTEGER REFERENCES pricing_policies(id),
			policy_created INTEGER NOT NULL DEFAULT 0,
			policy_initially_on INTEGER NOT NULL DEFAULT 0,
			policy_activated INTEGER NOT NULL DEFAULT 0,
			ratio_changed INTEGER NOT NULL DEFAULT 0,
			channel_enabled INTEGER NOT NULL DEFAULT 0,
			rollback_status TEXT NOT NULL DEFAULT '',
			group_model_pricing_receipt_json TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT,
			finished_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_listing_changes_plan ON listing_change_sets(plan_id,created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS target_change_leases (
			target_site_id INTEGER PRIMARY KEY REFERENCES target_sites(id) ON DELETE CASCADE,
			owner_id TEXT NOT NULL,
			leased_until TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_target_change_leases_expiry ON target_change_leases(leased_until)`,
		`CREATE TABLE IF NOT EXISTS target_control_leases (
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			dimension TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			fencing_token INTEGER NOT NULL DEFAULT 0,
			leased_until TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(target_site_id,dimension)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_target_control_leases_expiry ON target_control_leases(leased_until)`,
		`CREATE TABLE IF NOT EXISTS target_control_operations (
			id INTEGER PRIMARY KEY,
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			dimension TEXT NOT NULL,
			action TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			request_hash TEXT NOT NULL,
			expected_hash TEXT NOT NULL DEFAULT '',
			desired_hash TEXT NOT NULL DEFAULT '',
			readback_hash TEXT NOT NULL DEFAULT '',
			result_state TEXT NOT NULL,
			fencing_token INTEGER NOT NULL,
			result_json TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			finished_at TEXT,
			updated_at TEXT NOT NULL,
			UNIQUE(target_site_id,idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_target_control_operations_dimension
			ON target_control_operations(target_site_id,dimension,started_at DESC,id DESC)`,
		`CREATE TABLE IF NOT EXISTS activities (
            id INTEGER PRIMARY KEY,
            kind TEXT NOT NULL,
            severity TEXT NOT NULL,
            title TEXT NOT NULL,
            message TEXT NOT NULL,
            supplier_id INTEGER REFERENCES suppliers(id),
            metadata_json TEXT NOT NULL DEFAULT '{}',
            created_at TEXT NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS idx_activity_time ON activities(created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS collection_leases (
			supplier_id INTEGER PRIMARY KEY REFERENCES suppliers(id) ON DELETE CASCADE,
			owner_id TEXT NOT NULL,
			leased_until TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_collection_leases_expiry ON collection_leases(leased_until)`,
		`CREATE TABLE IF NOT EXISTS collection_attempts (
			id INTEGER PRIMARY KEY,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			error_class TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			catalog_updated INTEGER NOT NULL DEFAULT 0,
			balance_updated INTEGER NOT NULL DEFAULT 0,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_collection_attempts_supplier_time
			ON collection_attempts(supplier_id,started_at DESC,id DESC)`,
		`CREATE TABLE IF NOT EXISTS catalog_group_collection_proofs (
			attempt_id INTEGER NOT NULL REFERENCES collection_attempts(id) ON DELETE CASCADE,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			source_group TEXT NOT NULL,
			source_semantic_hash TEXT NOT NULL DEFAULT '',
			proof_json TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			PRIMARY KEY(attempt_id,source_group)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_group_proofs_lookup
			ON catalog_group_collection_proofs(supplier_id,source_group,attempt_id DESC)`,
		`CREATE TABLE IF NOT EXISTS request_evidence (
			id INTEGER PRIMARY KEY,
			attempt_id INTEGER NOT NULL REFERENCES collection_attempts(id) ON DELETE CASCADE,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			endpoint_type TEXT NOT NULL,
			auth_capability TEXT NOT NULL,
			method TEXT NOT NULL,
			source_url TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			error_class TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			schema_version TEXT NOT NULL DEFAULT '',
			response_hash TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_evidence_attempt ON request_evidence(attempt_id,id)`,
		`CREATE TABLE IF NOT EXISTS auth_capabilities (
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			capability TEXT NOT NULL,
			status TEXT NOT NULL,
			credential_id INTEGER REFERENCES credentials(id) ON DELETE SET NULL,
			endpoint_type TEXT NOT NULL DEFAULT '',
			http_status INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error_class TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT NOT NULL,
			verified_at TEXT,
			PRIMARY KEY(supplier_id,capability,endpoint_type)
		)`,
		`CREATE TABLE IF NOT EXISTS connector_specs (
            id INTEGER PRIMARY KEY,
            name TEXT NOT NULL,
            schema_version TEXT NOT NULL,
            spec_json TEXT NOT NULL,
            enabled INTEGER NOT NULL DEFAULT 0,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS pairing_tokens (
            token_hash TEXT PRIMARY KEY,
            supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
            expires_at TEXT NOT NULL,
			used_at TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			last_error_class TEXT NOT NULL DEFAULT '',
			last_error_message TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT,
			attempt_count INTEGER NOT NULL DEFAULT 0
        )`,
		`CREATE TABLE IF NOT EXISTS browser_custody_tokens (
			token_hash TEXT PRIMARY KEY,
			supplier_id INTEGER NOT NULL UNIQUE REFERENCES suppliers(id) ON DELETE CASCADE,
			credential_id INTEGER NOT NULL REFERENCES credentials(id) ON DELETE CASCADE,
			expected_origin TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			revoked_at TEXT,
			last_check_at TEXT,
			last_submission_at TEXT,
			last_success_at TEXT,
			submission_count INTEGER NOT NULL DEFAULT 0,
			last_error_class TEXT NOT NULL DEFAULT '',
			last_error_message TEXT NOT NULL DEFAULT '',
			helper_version TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_browser_custody_expiry
			ON browser_custody_tokens(revoked_at,expires_at)`,
		`CREATE TABLE IF NOT EXISTS connector_manifests (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			adapter_family TEXT NOT NULL,
			manifest_json TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'import',
			validation_status TEXT NOT NULL DEFAULT 'valid',
			validation_message TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(name,schema_version)
		)`,
		`CREATE TABLE IF NOT EXISTS supplier_manifest_bindings (
			supplier_id INTEGER PRIMARY KEY REFERENCES suppliers(id) ON DELETE CASCADE,
			manifest_id INTEGER NOT NULL REFERENCES connector_manifests(id) ON DELETE RESTRICT,
			bound_origin TEXT NOT NULL,
			bound_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_supplier_manifest_bindings_manifest
			ON supplier_manifest_bindings(manifest_id)`,
		`CREATE TABLE IF NOT EXISTS discoveries (
			id INTEGER PRIMARY KEY,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			external_id TEXT NOT NULL,
			name TEXT NOT NULL,
			ratio TEXT NOT NULL DEFAULT '',
			model_count INTEGER NOT NULL DEFAULT 0,
			payload_json TEXT NOT NULL DEFAULT '{}',
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			acknowledged_at TEXT,
			is_baseline INTEGER NOT NULL DEFAULT 0,
			UNIQUE(supplier_id, kind, external_id)
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
            key TEXT PRIMARY KEY,
            value_json TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS watch_items (
			id INTEGER PRIMARY KEY,
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			scope_type TEXT NOT NULL,
			group_key TEXT NOT NULL DEFAULT '',
			model_key TEXT NOT NULL DEFAULT '',
			manual_pinned INTEGER NOT NULL DEFAULT 1,
			manual_rank INTEGER NOT NULL DEFAULT 0,
			display_name TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			suppressed_hash TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(supplier_id,scope_type,group_key,model_key),
			CHECK(scope_type IN ('group','model'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_watch_items_manual_order
			ON watch_items(manual_pinned DESC,manual_rank,id)`,
		`CREATE TABLE IF NOT EXISTS config_versions (
			id INTEGER PRIMARY KEY,
			version_hash TEXT NOT NULL UNIQUE,
			schema_version TEXT NOT NULL,
			document_json TEXT NOT NULL,
			source TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS config_import_previews (
			id INTEGER PRIMARY KEY,
			preview_hash TEXT NOT NULL UNIQUE,
			expected_config_hash TEXT NOT NULL,
			bundle_hash TEXT NOT NULL,
			mode TEXT NOT NULL,
			bundle_json TEXT NOT NULL,
			report_json TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			consumed_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS config_import_runs (
			id INTEGER PRIMARY KEY,
			preview_id INTEGER NOT NULL UNIQUE REFERENCES config_import_previews(id),
			mode TEXT NOT NULL,
			status TEXT NOT NULL,
			before_hash TEXT NOT NULL,
			after_hash TEXT NOT NULL,
			before_version_id INTEGER NOT NULL REFERENCES config_versions(id),
			version_id INTEGER NOT NULL REFERENCES config_versions(id),
			before_document_json TEXT NOT NULL,
			report_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			rolled_back_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS config_import_pending_objects (
			id INTEGER PRIMARY KEY,
			import_id INTEGER NOT NULL REFERENCES config_import_runs(id) ON DELETE CASCADE,
			object_type TEXT NOT NULL,
			object_key TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			reason TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_config_import_pending_run ON config_import_pending_objects(import_id,id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	if err := s.migrateAuthCapabilityIdentity(ctx); err != nil {
		return err
	}
	// Older builds only coalesced consecutive identical snapshots. Collapse any
	// historical duplicates before enforcing directory-hash identity.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM catalog_snapshots WHERE id IN (
		SELECT id FROM (
			SELECT id,ROW_NUMBER() OVER(
				PARTITION BY supplier_id,response_hash ORDER BY julianday(observed_at) DESC,id DESC
			) AS row_num FROM catalog_snapshots
		) WHERE row_num>1
	)`); err != nil {
		return fmt.Errorf("deduplicate catalog snapshots: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_catalog_supplier_hash
		ON catalog_snapshots(supplier_id,response_hash)`); err != nil {
		return fmt.Errorf("index catalog snapshot hashes: %w", err)
	}
	for _, column := range []struct {
		table, name, definition string
	}{
		{"target_sites", "write_mode", "TEXT NOT NULL DEFAULT 'approval'"},
		{"target_sites", "last_status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"target_sites", "last_error", "TEXT NOT NULL DEFAULT ''"},
		{"suppliers", "last_error_class", "TEXT NOT NULL DEFAULT ''"},
		{"suppliers", "last_attempt_at", "TEXT"},
		{"suppliers", "last_success_at", "TEXT"},
		{"suppliers", "last_success_duration_ms", "INTEGER"},
		{"suppliers", "last_failure_duration_ms", "INTEGER"},
		{"suppliers", "recharge_value_factor", "TEXT NOT NULL DEFAULT '1'"},
		{"discoveries", "is_baseline", "INTEGER NOT NULL DEFAULT 0"},
		{"catalog_snapshots", "total_observations", "INTEGER NOT NULL DEFAULT 1"},
		{"catalog_snapshots", "consecutive_observations", "INTEGER NOT NULL DEFAULT 1"},
		{"catalog_snapshots", "last_seen", "TEXT NOT NULL DEFAULT ''"},
		{"listing_plans", "equivalent_cost_ratio", "TEXT NOT NULL DEFAULT ''"},
		{"listing_plans", "cost_ratio_vector_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"listing_plans", "target_catalog_hash", "TEXT NOT NULL DEFAULT ''"},
		{"listing_plans", "profit_verified", "INTEGER NOT NULL DEFAULT 0"},
		{"listing_plans", "profit_verification", "TEXT NOT NULL DEFAULT ''"},
		{"listing_plans", "expected_group_ratios_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"listing_plans", "desired_target_hash", "TEXT NOT NULL DEFAULT ''"},
		{"listing_plans", "group_model_pricing_plan_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"listing_change_sets", "pricing_policy_id", "INTEGER REFERENCES pricing_policies(id)"},
		{"listing_change_sets", "policy_created", "INTEGER NOT NULL DEFAULT 0"},
		{"listing_change_sets", "policy_initially_on", "INTEGER NOT NULL DEFAULT 0"},
		{"listing_change_sets", "policy_activated", "INTEGER NOT NULL DEFAULT 0"},
		{"listing_change_sets", "group_model_pricing_receipt_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"change_proposals", "group_model_pricing_plan_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"change_proposals", "group_model_pricing_receipt_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"pricing_policies", "coverage_mode", "TEXT NOT NULL DEFAULT 'threshold'"},
		{"pricing_policies", "minimum_coverage_ratio", "TEXT NOT NULL DEFAULT '0.8'"},
		{"pricing_policies", "source_basis", "TEXT NOT NULL DEFAULT 'group_ratio'"},
		{"pairing_tokens", "status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"pairing_tokens", "last_error_class", "TEXT NOT NULL DEFAULT ''"},
		{"pairing_tokens", "last_error_message", "TEXT NOT NULL DEFAULT ''"},
		{"pairing_tokens", "last_attempt_at", "TEXT"},
		{"pairing_tokens", "attempt_count", "INTEGER NOT NULL DEFAULT 0"},
		{"browser_custody_tokens", "helper_version", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	// Legacy pairing_tokens tables predate the status column. The index must be
	// created only after ensureColumn has upgraded those databases.
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_pairing_tokens_status_expiry
		ON pairing_tokens(status,expires_at)`); err != nil {
		return fmt.Errorf("index pairing token status: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE catalog_snapshots SET
		total_observations=CASE WHEN total_observations<1 THEN 1 ELSE total_observations END,
		consecutive_observations=CASE WHEN consecutive_observations<1 THEN 1 ELSE consecutive_observations END,
		last_seen=CASE WHEN last_seen='' THEN observed_at ELSE last_seen END`); err != nil {
		return fmt.Errorf("backfill catalog evidence counters: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE suppliers SET
		last_attempt_at=COALESCE(last_attempt_at,last_collected_at),
		last_success_at=CASE WHEN last_success_at IS NOT NULL THEN last_success_at WHEN last_status='healthy' THEN last_collected_at ELSE NULL END`); err != nil {
		return fmt.Errorf("backfill supplier collection timestamps: %w", err)
	}
	if err := s.migrateLegacyQuotaEvidence(ctx); err != nil {
		return err
	}
	if err := s.migrateOpportunity(ctx); err != nil {
		return err
	}
	if err := s.migrateIntelligence(ctx); err != nil {
		return err
	}
	if err := s.migrateLedgerSync(ctx); err != nil {
		return err
	}
	if err := s.compactHistoricalNoChangeProposals(ctx); err != nil {
		return err
	}
	if err := s.migrateMirrorSchemaV3(ctx); err != nil {
		return err
	}
	if err := s.migrateLegacyWatchItems(ctx); err != nil {
		return err
	}
	if err := s.migrateAuthorityDarkSchema(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if columnName == name {
			_ = rows.Close()
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}

func (s *Store) CreateSupplier(ctx context.Context, supplier *domain.Supplier) error {
	now := time.Now().UTC()
	factor, err := ratios.NormalizeRechargeValueFactor(supplier.RechargeValueFactor)
	if err != nil {
		return err
	}
	supplier.RechargeValueFactor = factor
	if supplier.PollIntervalSeconds <= 0 {
		supplier.PollIntervalSeconds = 300
	}
	if supplier.Adapter == "" {
		supplier.Adapter = "auto"
	}
	if supplier.FocusGroups == nil {
		supplier.FocusGroups = []string{}
	}
	if supplier.FocusModels == nil {
		supplier.FocusModels = []string{}
	}
	if supplier.Metadata == nil {
		supplier.Metadata = map[string]string{}
	}
	if supplier.LastStatus == "" {
		supplier.LastStatus = "pending"
	}
	groups := mustJSON(supplier.FocusGroups)
	models := mustJSON(supplier.FocusModels)
	metadata := mustJSON(supplier.Metadata)
	result, err := s.db.ExecContext(ctx, `INSERT INTO suppliers
        (name, base_url, adapter, pricing_path, balance_path, credential_id, enabled, poll_interval_seconds,
         recharge_value_factor, focus_groups_json, focus_models_json, metadata_json, last_status, last_error, next_collection_at, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, supplier.Name, strings.TrimRight(supplier.BaseURL, "/"),
		supplier.Adapter, supplier.PricingPath, supplier.BalancePath, supplier.CredentialID, boolInt(supplier.Enabled),
		supplier.PollIntervalSeconds, supplier.RechargeValueFactor, groups, models, metadata, supplier.LastStatus, supplier.LastError,
		formatTime(now), formatTime(now), formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create supplier: %w", err)
	}
	supplier.ID, _ = result.LastInsertId()
	supplier.CreatedAt = now
	supplier.UpdatedAt = now
	next := now
	supplier.NextCollectionAt = &next
	return nil
}

func (s *Store) UpdateSupplier(ctx context.Context, supplier domain.Supplier) error {
	now := time.Now().UTC()
	factor, err := ratios.NormalizeRechargeValueFactor(supplier.RechargeValueFactor)
	if err != nil {
		return err
	}
	supplier.RechargeValueFactor = factor
	result, err := s.db.ExecContext(ctx, `UPDATE suppliers SET name=?, base_url=?, adapter=?, pricing_path=?, balance_path=?,
		credential_id=?, enabled=?, poll_interval_seconds=?, recharge_value_factor=?, focus_groups_json=?, focus_models_json=?, metadata_json=?, updated_at=? WHERE id=?`,
		supplier.Name, strings.TrimRight(supplier.BaseURL, "/"), supplier.Adapter, supplier.PricingPath, supplier.BalancePath,
		supplier.CredentialID, boolInt(supplier.Enabled), supplier.PollIntervalSeconds, supplier.RechargeValueFactor, mustJSON(supplier.FocusGroups),
		mustJSON(supplier.FocusModels), mustJSON(supplier.Metadata), formatTime(now), supplier.ID)
	if err != nil {
		return fmt.Errorf("update supplier: %w", err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListSuppliers(ctx context.Context) ([]domain.Supplier, error) {
	rows, err := s.db.QueryContext(ctx, supplierSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list suppliers: %w", err)
	}
	defer rows.Close()
	var suppliers []domain.Supplier
	for rows.Next() {
		supplier, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		suppliers = append(suppliers, supplier)
	}
	return suppliers, rows.Err()
}

func (s *Store) GetSupplier(ctx context.Context, id int64) (domain.Supplier, error) {
	return scanSupplier(s.db.QueryRowContext(ctx, supplierSelect+` WHERE id=?`, id))
}

func (s *Store) DueSuppliers(ctx context.Context, now time.Time, limit int) ([]domain.Supplier, error) {
	rows, err := s.db.QueryContext(ctx, supplierSelect+` WHERE enabled=1 AND (next_collection_at IS NULL OR next_collection_at<=?) ORDER BY next_collection_at LIMIT ?`, formatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list due suppliers: %w", err)
	}
	defer rows.Close()
	var suppliers []domain.Supplier
	for rows.Next() {
		supplier, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		suppliers = append(suppliers, supplier)
	}
	return suppliers, rows.Err()
}

func (s *Store) MarkSupplierResult(ctx context.Context, id int64, status, message string, observedAt, nextAt time.Time) error {
	var err error
	if status == "healthy" {
		_, err = s.db.ExecContext(ctx, `UPDATE suppliers SET last_status=?,last_error='',last_error_class='',last_attempt_at=?,
			last_success_at=?,last_collected_at=?,next_collection_at=?,updated_at=? WHERE id=?`, status, formatTime(observedAt),
			formatTime(observedAt), formatTime(observedAt), formatTime(nextAt), formatTime(time.Now().UTC()), id)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE suppliers SET last_status=?,last_error=?,last_attempt_at=?,next_collection_at=?,updated_at=? WHERE id=?`,
			status, message, formatTime(observedAt), formatTime(nextAt), formatTime(time.Now().UTC()), id)
	}
	return err
}

// TryAcquireCollectionLease atomically claims a supplier across all processes
// sharing the database. Expired claims are reclaimable after a crashed worker.
func (s *Store) TryAcquireCollectionLease(ctx context.Context, supplierID int64, ownerID string, now time.Time, ttl time.Duration) (bool, error) {
	if strings.TrimSpace(ownerID) == "" {
		return false, fmt.Errorf("collection lease owner is required")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("collection lease ttl must be positive")
	}
	s.collectionLeaseMu.Lock()
	defer s.collectionLeaseMu.Unlock()
	for attempt := 0; ; attempt++ {
		claimed, err := s.tryAcquireCollectionLeaseOnce(ctx, supplierID, ownerID, now, ttl)
		if err == nil || !isSQLiteBusy(err) || attempt >= 5 {
			return claimed, err
		}
		delay := time.Duration(10*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) tryAcquireCollectionLeaseOnce(ctx context.Context, supplierID int64, ownerID string, now time.Time, ttl time.Duration) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkReleaseAdmissionOpenTx(ctx, tx); err != nil {
		return false, err
	}
	if err := checkCollectionAuthorityTx(ctx, tx); err != nil {
		return false, err
	}
	incarnation, err := ensureSupplierIncarnationTx(ctx, tx, supplierID)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO collection_leases
		(supplier_id,supplier_incarnation_uuid,owner_id,leased_until,updated_at,lease_generation,leased_until_unix_ms,retired)
		VALUES(?,?,?,?,?,1,?,0) ON CONFLICT(supplier_id) DO UPDATE SET
		supplier_incarnation_uuid=excluded.supplier_incarnation_uuid,
		owner_id=excluded.owner_id,leased_until=excluded.leased_until,updated_at=excluded.updated_at,
		leased_until_unix_ms=excluded.leased_until_unix_ms,retired=0,
		lease_generation=collection_leases.lease_generation+1
		WHERE collection_leases.lease_generation<9223372036854775807 AND
			(collection_leases.leased_until_unix_ms<=? OR
			(collection_leases.leased_until_unix_ms=0 AND collection_leases.leased_until<=?))`, supplierID, incarnation,
		ownerID, formatLeaseTime(now.Add(ttl)), formatLeaseTime(now), unixMillis(now.Add(ttl)), unixMillis(now), formatLeaseTime(now))
	if err != nil {
		return false, fmt.Errorf("acquire collection lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	type sqliteCoder interface{ Code() int }
	var coded sqliteCoder
	if errors.As(err, &coded) {
		code := coded.Code() & 0xff
		if code == 5 || code == 6 {
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func (s *Store) RenewCollectionLease(ctx context.Context, supplierID int64, ownerID string, now time.Time, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("collection lease ttl must be positive")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE collection_leases SET leased_until=?,leased_until_unix_ms=?,updated_at=?
		WHERE supplier_id=? AND owner_id=? AND retired=0 AND
		(CASE WHEN leased_until_unix_ms>0 THEN leased_until_unix_ms ELSE CAST((julianday(leased_until)-2440587.5)*86400000 AS INTEGER) END)>?`,
		formatLeaseTime(now.Add(ttl)), unixMillis(now.Add(ttl)), formatLeaseTime(now), supplierID, ownerID, unixMillis(now))
	if err != nil {
		return false, fmt.Errorf("renew collection lease: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) ReleaseCollectionLease(ctx context.Context, supplierID int64, ownerID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE collection_leases SET owner_id='',leased_until=?,leased_until_unix_ms=?,
		updated_at=? WHERE supplier_id=? AND owner_id=?`, formatLeaseTime(now), unixMillis(now), formatLeaseTime(now),
		supplierID, ownerID)
	return err
}

func (s *Store) SetBalanceHeartbeat(interval time.Duration) {
	s.configMu.Lock()
	s.balanceHeartbeat = interval
	s.configMu.Unlock()
}

func (s *Store) balanceHeartbeatInterval() time.Duration {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.balanceHeartbeat
}

func (s *Store) SaveSnapshot(ctx context.Context, snapshot domain.CatalogSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.saveSnapshotTx(ctx, tx, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveSnapshotTx(ctx context.Context, tx *sql.Tx, snapshot domain.CatalogSnapshot) error {
	if snapshot.Balance != nil && snapshot.Balance.ObservedAt.IsZero() {
		snapshot.Balance.ObservedAt = snapshot.Evidence.ObservedAt
	}
	payload := mustJSON(snapshot)
	var latestHash string
	latestErr := tx.QueryRowContext(ctx, `SELECT response_hash FROM catalog_snapshots WHERE supplier_id=?
		ORDER BY julianday(COALESCE(NULLIF(last_seen,''),observed_at)) DESC,id DESC LIMIT 1`, snapshot.SupplierID).
		Scan(&latestHash)
	if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
		return latestErr
	}
	changed := errors.Is(latestErr, sql.ErrNoRows) || latestHash != snapshot.Evidence.ResponseHash
	var matchingID int64
	var matchingConsecutive int
	matchingErr := tx.QueryRowContext(ctx, `SELECT id,consecutive_observations FROM catalog_snapshots WHERE supplier_id=? AND response_hash=?`,
		snapshot.SupplierID, snapshot.Evidence.ResponseHash).Scan(&matchingID, &matchingConsecutive)
	if matchingErr == nil {
		nextConsecutive := 1
		if latestHash == snapshot.Evidence.ResponseHash {
			nextConsecutive = matchingConsecutive + 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_snapshots SET payload_json=?,observed_at=?,last_seen=?,
			total_observations=total_observations+1,consecutive_observations=? WHERE id=?`, payload,
			formatTime(snapshot.Evidence.ObservedAt), formatTime(snapshot.Evidence.ObservedAt), nextConsecutive, matchingID); err != nil {
			return fmt.Errorf("refresh catalog snapshot: %w", err)
		}
	} else if errors.Is(matchingErr, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_snapshots
			(supplier_id,payload_json,response_hash,observed_at,total_observations,consecutive_observations,last_seen)
			VALUES(?,?,?,?,1,1,?)`, snapshot.SupplierID, payload, snapshot.Evidence.ResponseHash,
			formatTime(snapshot.Evidence.ObservedAt), formatTime(snapshot.Evidence.ObservedAt)); err != nil {
			return fmt.Errorf("save catalog snapshot: %w", err)
		}
	} else {
		return matchingErr
	}
	if changed {
		for _, model := range snapshot.Models {
			if _, err := tx.ExecContext(ctx, `INSERT INTO price_observations
            (supplier_id,entity_type,entity_key,group_name,model_name,price_json,evidence_json,observed_at)
            VALUES(?,?,?,?,?,?,?,?)`, snapshot.SupplierID, "model", model.ExternalID, model.Group, model.Name,
				mustJSON(model), mustJSON(snapshot.Evidence), formatTime(snapshot.Evidence.ObservedAt)); err != nil {
				return fmt.Errorf("save model observation: %w", err)
			}
		}
		for _, group := range snapshot.Groups {
			model := domain.ModelOffer{ExternalID: group.ExternalID, Name: group.Name, Group: group.Name, BillingMode: "group_ratio", ModelRatio: group.Ratio}
			if _, err := tx.ExecContext(ctx, `INSERT INTO price_observations
            (supplier_id,entity_type,entity_key,group_name,model_name,price_json,evidence_json,observed_at)
            VALUES(?,?,?,?,?,?,?,?)`, snapshot.SupplierID, "group", group.ExternalID, group.Name, "",
				mustJSON(model), mustJSON(snapshot.Evidence), formatTime(snapshot.Evidence.ObservedAt)); err != nil {
				return fmt.Errorf("save group observation: %w", err)
			}
		}
	}
	if snapshot.Balance != nil {
		if err := s.saveBalanceObservationTx(ctx, tx, snapshot.SupplierID, *snapshot.Balance); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) saveBalanceObservationTx(ctx context.Context, tx *sql.Tx, supplierID int64, balance domain.BalanceState) error {
	writeBalance, err := s.shouldWriteBalance(ctx, tx, supplierID, balance)
	if err != nil {
		return err
	}
	if !writeBalance {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO balance_observations(supplier_id,payload_json,observed_at) VALUES(?,?,?)`,
		supplierID, mustJSON(balance), formatTime(balance.ObservedAt)); err != nil {
		return fmt.Errorf("save balance observation: %w", err)
	}
	return nil
}

func (s *Store) shouldWriteBalance(ctx context.Context, tx *sql.Tx, supplierID int64, balance domain.BalanceState) (bool, error) {
	var previousPayload, previousObserved string
	err := tx.QueryRowContext(ctx, `SELECT payload_json,observed_at FROM balance_observations WHERE supplier_id=? ORDER BY julianday(observed_at) DESC,id DESC LIMIT 1`, supplierID).
		Scan(&previousPayload, &previousObserved)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read latest balance observation: %w", err)
	}
	var previous domain.BalanceState
	if err := json.Unmarshal([]byte(previousPayload), &previous); err != nil {
		return false, fmt.Errorf("decode latest balance observation: %w", err)
	}
	if balanceFingerprint(previous) != balanceFingerprint(balance) {
		return true, nil
	}
	heartbeat := s.balanceHeartbeatInterval()
	return heartbeat <= 0 || !balance.ObservedAt.Before(parseTime(previousObserved).Add(heartbeat)), nil
}

func balanceFingerprint(balance domain.BalanceState) string {
	balance.ObservedAt = time.Time{}
	// Raw display text is evidence formatting rather than a monetary change.
	balance.RawDisplayText = ""
	return mustJSON(balance)
}

func (s *Store) LatestSnapshot(ctx context.Context, supplierID int64) (domain.CatalogSnapshot, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM catalog_snapshots WHERE supplier_id=?
		ORDER BY julianday(COALESCE(NULLIF(last_seen,''),observed_at)) DESC,id DESC LIMIT 1`, supplierID).Scan(&payload)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	var snapshot domain.CatalogSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("decode catalog snapshot: %w", err)
	}
	return snapshot, nil
}

// LatestEffectiveSnapshot keeps the persisted catalog as upstream evidence and
// applies the supplier's current cash-value factor only at the decision/read
// boundary. Changing the factor therefore takes effect immediately without a
// synthetic collection or a rewritten response hash.
func (s *Store) LatestEffectiveSnapshot(ctx context.Context, supplierID int64) (domain.CatalogSnapshot, error) {
	snapshot, err := s.LatestSnapshot(ctx, supplierID)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	supplier, err := s.GetSupplier(ctx, supplierID)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	if err := s.requireAuthoritativeSnapshot(ctx, supplierID, snapshot); err != nil {
		return domain.CatalogSnapshot{}, err
	}
	return ratios.EnrichSnapshot(snapshot, supplier.RechargeValueFactor)
}

// requireAuthoritativeSnapshot is deliberately a read-side policy boundary.
// Legacy/manual snapshots remain available for diagnostics and history, but
// protocol-2 automatic pricing/listing may consume only a snapshot tied to a
// protocol-2 collection attempt through its immutable response evidence.
func (s *Store) requireAuthoritativeSnapshot(ctx context.Context, supplierID int64, snapshot domain.CatalogSnapshot) error {
	var protocol int
	err := s.db.QueryRowContext(ctx, `SELECT authority_protocol FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol)
	if errors.Is(err, sql.ErrNoRows) || err != nil || protocol != authorityProtocolV2 {
		return err
	}
	if _, err := s.AuditCollectionAuthority(ctx); err != nil {
		return err
	}
	responseHash := strings.TrimSpace(snapshot.Evidence.ResponseHash)
	if responseHash == "" {
		return authorityIntegrityError("snapshot has no response hash carrier for automatic use")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.proof_json FROM catalog_group_collection_proofs p
		JOIN collection_attempts a ON a.id=p.attempt_id AND a.supplier_id=p.supplier_id
		WHERE a.supplier_id=? AND a.authority_protocol=2 AND a.automation_eligible=1 AND
			a.catalog_updated=1 AND a.status IN ('success','partial_success') AND
			p.authority_protocol=2 AND p.supplier_incarnation_uuid=a.supplier_incarnation_uuid`, supplierID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return err
		}
		var proof struct {
			ResponseHash string `json:"response_hash"`
			Complete     bool   `json:"complete"`
		}
		if json.Unmarshal([]byte(payload), &proof) == nil && proof.Complete && proof.ResponseHash == responseHash {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return authorityIntegrityError("snapshot is not bound to a protocol-2 collection proof")
}

func (s *Store) LatestPrices(ctx context.Context, supplierID int64, limit int) ([]domain.PriceObservation, error) {
	query := `SELECT p.id,p.supplier_id,p.entity_type,p.entity_key,p.group_name,p.model_name,p.price_json,p.evidence_json,p.observed_at,p.previous_hash
        FROM price_observations p JOIN (
          SELECT entity_type,entity_key,group_name,MAX(id) AS max_id FROM price_observations WHERE supplier_id=? GROUP BY entity_type,entity_key,group_name
        ) latest ON latest.max_id=p.id ORDER BY p.entity_type,p.group_name,p.model_name LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, supplierID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.PriceObservation
	for rows.Next() {
		var item domain.PriceObservation
		var priceJSON, evidenceJSON, observed string
		if err := rows.Scan(&item.ID, &item.SupplierID, &item.EntityType, &item.EntityKey, &item.GroupName, &item.ModelName,
			&priceJSON, &evidenceJSON, &observed, &item.PreviousHash); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(priceJSON), &item.Price)
		_ = json.Unmarshal([]byte(evidenceJSON), &item.Evidence)
		item.ObservedAt = parseTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CreateCredential(ctx context.Context, credential *domain.Credential, encryptedPayload string) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO credentials(name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?)`, credential.Name, credential.Kind, credential.SiteURL, encryptedPayload, nullableTime(credential.ExpiresAt),
		"active", formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	credential.ID, _ = result.LastInsertId()
	credential.Status = "active"
	credential.CreatedAt = now
	credential.UpdatedAt = now
	return nil
}

func (s *Store) GetCredentialEncrypted(ctx context.Context, id int64) (domain.Credential, string, error) {
	var credential domain.Credential
	var encrypted, created, updated string
	var expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at FROM credentials WHERE id=?`, id).
		Scan(&credential.ID, &credential.Name, &credential.Kind, &credential.SiteURL, &encrypted, &expires, &credential.Status, &created, &updated)
	if err != nil {
		return domain.Credential{}, "", err
	}
	credential.CreatedAt = parseTime(created)
	credential.UpdatedAt = parseTime(updated)
	if expires.Valid {
		value := parseTime(expires.String)
		credential.ExpiresAt = &value
	}
	return credential, encrypted, nil
}

func (s *Store) UpdateCredentialPayload(ctx context.Context, id int64, encryptedPayload string, expiresAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE credentials SET encrypted_payload=?,expires_at=?,status='active',updated_at=? WHERE id=?`,
		encryptedPayload, nullableTime(expiresAt), formatTime(time.Now().UTC()), id)
	return err
}

func (s *Store) AddActivity(ctx context.Context, activity domain.Activity) error {
	if activity.CreatedAt.IsZero() {
		activity.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		activity.Kind, activity.Severity, activity.Title, activity.Message, activity.SupplierID, mustJSON(activity.Metadata), formatTime(activity.CreatedAt))
	return err
}

// AddActivityCoalesced keeps the activity stream concise while collection
// attempts retain every individual failure. Structured dimensions are stable
// even when transport messages contain volatile ports or addresses.
func (s *Store) AddActivityCoalesced(ctx context.Context, activity domain.Activity, window time.Duration) error {
	if window <= 0 {
		return s.AddActivity(ctx, activity)
	}
	if activity.CreatedAt.IsZero() {
		activity.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	var message, metadataJSON, created string
	query := `SELECT id,message,metadata_json,created_at FROM activities WHERE kind=? AND supplier_id IS NULL ORDER BY created_at DESC,id DESC LIMIT 1`
	args := []any{activity.Kind}
	if activity.SupplierID != nil {
		query = `SELECT id,message,metadata_json,created_at FROM activities WHERE kind=? AND supplier_id=? ORDER BY created_at DESC,id DESC LIMIT 1`
		args = append(args, *activity.SupplierID)
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(&id, &message, &metadataJSON, &created)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	previousMetadata := map[string]string{}
	_ = json.Unmarshal([]byte(metadataJSON), &previousMetadata)
	previousCreatedAt := parseTime(created)
	previousDimension := activityCoalesceDimension(previousMetadata)
	currentDimension := activityCoalesceDimension(activity.Metadata)
	dimensionSpecified := activityHasCoalesceDimension(previousMetadata) || activityHasCoalesceDimension(activity.Metadata)
	sameEvent := previousDimension == currentDimension && (dimensionSpecified || message == activity.Message)
	if err == nil && !activity.CreatedAt.Before(previousCreatedAt) && activity.CreatedAt.Sub(previousCreatedAt) <= window && sameEvent {
		metadata := cloneMetadata(activity.Metadata)
		repeatCount, _ := strconv.Atoi(previousMetadata["repeat_count"])
		if repeatCount < 1 {
			repeatCount = 1
		}
		metadata["repeat_count"] = strconv.Itoa(repeatCount + 1)
		metadata["first_seen_at"] = previousMetadata["first_seen_at"]
		if metadata["first_seen_at"] == "" {
			metadata["first_seen_at"] = created
		}
		metadata["last_seen_at"] = formatTime(activity.CreatedAt)
		if _, err := tx.ExecContext(ctx, `UPDATE activities SET severity=?,title=?,message=?,metadata_json=?,created_at=? WHERE id=?`,
			activity.Severity, activity.Title, activity.Message, mustJSON(metadata), formatTime(activity.CreatedAt), id); err != nil {
			return err
		}
		return tx.Commit()
	}

	metadata := cloneMetadata(activity.Metadata)
	metadata["repeat_count"] = "1"
	metadata["first_seen_at"] = formatTime(activity.CreatedAt)
	metadata["last_seen_at"] = formatTime(activity.CreatedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		activity.Kind, activity.Severity, activity.Title, activity.Message, activity.SupplierID, mustJSON(metadata), formatTime(activity.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func activityCoalesceDimension(metadata map[string]string) string {
	return strings.Join([]string{metadata["error_class"], metadata["capability"], metadata["endpoint_type"],
		metadata["catalog_diff_key"], metadata["automation_failure_key"]}, "\x00")
}

func activityHasCoalesceDimension(metadata map[string]string) bool {
	return metadata["error_class"] != "" || metadata["capability"] != "" || metadata["endpoint_type"] != "" ||
		metadata["catalog_diff_key"] != "" || metadata["automation_failure_key"] != ""
}

func cloneMetadata(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+3)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (s *Store) RecentActivity(ctx context.Context, limit int) ([]domain.Activity, error) {
	page, err := s.QueryActivities(ctx, ActivityQuery{Page: 1, Limit: limit})
	return page.Items, err
}

const supplierSelect = `SELECT id,name,base_url,adapter,pricing_path,balance_path,credential_id,enabled,poll_interval_seconds,recharge_value_factor,
    focus_groups_json,focus_models_json,metadata_json,last_status,last_error,last_error_class,last_attempt_at,last_success_at,
	last_success_duration_ms,last_failure_duration_ms,last_collected_at,next_collection_at,created_at,updated_at FROM suppliers`

type scanner interface {
	Scan(dest ...any) error
}

func scanSupplier(row scanner) (domain.Supplier, error) {
	var supplier domain.Supplier
	var credential sql.NullInt64
	var enabled int
	var groupsJSON, modelsJSON, metadataJSON string
	var lastAttempt, lastSuccess, lastCollected, nextCollection sql.NullString
	var lastSuccessDuration, lastFailureDuration sql.NullInt64
	var created, updated string
	err := row.Scan(&supplier.ID, &supplier.Name, &supplier.BaseURL, &supplier.Adapter, &supplier.PricingPath, &supplier.BalancePath,
		&credential, &enabled, &supplier.PollIntervalSeconds, &supplier.RechargeValueFactor, &groupsJSON, &modelsJSON, &metadataJSON, &supplier.LastStatus,
		&supplier.LastError, &supplier.LastErrorClass, &lastAttempt, &lastSuccess, &lastSuccessDuration,
		&lastFailureDuration, &lastCollected, &nextCollection, &created, &updated)
	if err != nil {
		return domain.Supplier{}, err
	}
	if credential.Valid {
		value := credential.Int64
		supplier.CredentialID = &value
	}
	supplier.Enabled = enabled != 0
	if supplier.RechargeValueFactor == "" {
		supplier.RechargeValueFactor = ratios.DefaultRechargeValueFactor
	}
	_ = json.Unmarshal([]byte(groupsJSON), &supplier.FocusGroups)
	_ = json.Unmarshal([]byte(modelsJSON), &supplier.FocusModels)
	_ = json.Unmarshal([]byte(metadataJSON), &supplier.Metadata)
	if lastAttempt.Valid {
		value := parseTime(lastAttempt.String)
		supplier.LastAttemptAt = &value
	}
	if lastSuccess.Valid {
		value := parseTime(lastSuccess.String)
		supplier.LastSuccessAt = &value
	}
	if lastSuccessDuration.Valid {
		value := lastSuccessDuration.Int64
		supplier.LastSuccessDuration = &value
	}
	if lastFailureDuration.Valid {
		value := lastFailureDuration.Int64
		supplier.LastFailureDuration = &value
	}
	if lastCollected.Valid {
		value := parseTime(lastCollected.String)
		supplier.LastCollectedAt = &value
	}
	if nextCollection.Valid {
		value := parseTime(nextCollection.String)
		supplier.NextCollectionAt = &value
	}
	supplier.CreatedAt = parseTime(created)
	supplier.UpdatedAt = parseTime(updated)
	return supplier, nil
}

func mustJSON(value any) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(bytes)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func formatLeaseTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
