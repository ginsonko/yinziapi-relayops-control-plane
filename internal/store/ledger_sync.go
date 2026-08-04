package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type TargetLedgerSyncConfig struct {
	TargetSiteID              int64      `json:"target_site_id"`
	Enabled                   bool       `json:"enabled"`
	IntervalSeconds           int        `json:"interval_seconds"`
	OverlapSeconds            int        `json:"overlap_seconds"`
	InitialLookbackSeconds    int        `json:"initial_lookback_seconds"`
	PageSize                  int        `json:"page_size"`
	MaxPages                  int        `json:"max_pages"`
	Currency                  string     `json:"currency,omitempty"`
	QuotaCashFactor           string     `json:"quota_cash_factor,omitempty"`
	CostEvidenceMaxAgeSeconds int        `json:"cost_evidence_max_age_seconds"`
	SupplierCostCashRate      string     `json:"supplier_cost_cash_rate"`
	LogPath                   string     `json:"log_path"`
	TopupPath                 string     `json:"topup_path"`
	UserPath                  string     `json:"user_path"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	UnitEvidenceSource        string     `json:"-"`
	UnitEvidenceObservedAt    *time.Time `json:"-"`
}

type TargetLedgerSyncState struct {
	TargetSiteID       int64      `json:"target_site_id"`
	Stream             string     `json:"stream"`
	CursorTime         *time.Time `json:"cursor_time,omitempty"`
	CursorID           string     `json:"cursor_id,omitempty"`
	Status             string     `json:"status"`
	LastError          string     `json:"last_error,omitempty"`
	LastAttemptAt      *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt      *time.Time `json:"last_success_at,omitempty"`
	NextAttemptAt      time.Time  `json:"next_attempt_at"`
	LastScannedRows    int        `json:"last_scanned_rows"`
	LastImportedEvents int        `json:"last_imported_events"`
	LastSkippedRows    int        `json:"last_skipped_rows"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type TargetLedgerUserProfile struct {
	TargetSiteID    int64     `json:"target_site_id"`
	UserID          string    `json:"user_id"`
	Username        string    `json:"username,omitempty"`
	Role            string    `json:"role,omitempty"`
	UserSegment     string    `json:"user_segment,omitempty"`
	IsTest          bool      `json:"is_test,omitempty"`
	IsInternal      bool      `json:"is_internal,omitempty"`
	Unlimited       bool      `json:"unlimited,omitempty"`
	AvailableQuota  string    `json:"available_quota,omitempty"`
	CashBalance     string    `json:"cash_balance,omitempty"`
	Currency        string    `json:"currency,omitempty"`
	QuotaCashFactor string    `json:"quota_cash_factor,omitempty"`
	SourceHash      string    `json:"source_hash"`
	ObservedAt      time.Time `json:"observed_at"`
}

type TargetLedgerCostResolutionSample struct {
	TargetGroup  string `json:"target_group,omitempty"`
	Model        string `json:"model,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	ChannelName  string `json:"channel_name,omitempty"`
	PolicyID     string `json:"policy_id,omitempty"`
	PolicyName   string `json:"policy_name,omitempty"`
	SupplierID   string `json:"supplier_id,omitempty"`
	SupplierName string `json:"supplier_name,omitempty"`
	SourceGroup  string `json:"source_group,omitempty"`
}

type TargetLedgerCostResolutionReason struct {
	Code    string                             `json:"code"`
	Message string                             `json:"message"`
	Count   int                                `json:"count"`
	Samples []TargetLedgerCostResolutionSample `json:"samples"`
}

type TargetLedgerCostResolutionStatus struct {
	TargetSiteID     int64                              `json:"target_site_id"`
	WindowStart      time.Time                          `json:"window_start"`
	WindowEnd        time.Time                          `json:"window_end"`
	ScannedEvents    int                                `json:"scanned_events"`
	ResolvedEvents   int                                `json:"resolved_events"`
	UnresolvedEvents int                                `json:"unresolved_events"`
	Truncated        bool                               `json:"truncated"`
	Reasons          []TargetLedgerCostResolutionReason `json:"reasons"`
	ObservedAt       time.Time                          `json:"observed_at"`
}

func (s *Store) migrateLedgerSync(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS target_ledger_sync_configs (
			target_site_id INTEGER PRIMARY KEY REFERENCES target_sites(id) ON DELETE CASCADE,
			enabled INTEGER NOT NULL DEFAULT 1,
			interval_seconds INTEGER NOT NULL DEFAULT 300,
			overlap_seconds INTEGER NOT NULL DEFAULT 600,
			initial_lookback_seconds INTEGER NOT NULL DEFAULT 86400,
			page_size INTEGER NOT NULL DEFAULT 100,
			max_pages INTEGER NOT NULL DEFAULT 100,
			currency TEXT NOT NULL DEFAULT '',
			quota_cash_factor TEXT NOT NULL DEFAULT '',
			cost_evidence_max_age_seconds INTEGER NOT NULL DEFAULT 86400,
			supplier_cost_cash_rate TEXT NOT NULL DEFAULT '1',
			log_path TEXT NOT NULL DEFAULT '/api/log/',
			topup_path TEXT NOT NULL DEFAULT '/api/user/topup',
			user_path TEXT NOT NULL DEFAULT '/api/user/',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS target_ledger_sync_states (
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			stream TEXT NOT NULL,
			cursor_time TEXT,
			cursor_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			last_error TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT,
			last_success_at TEXT,
			next_attempt_at TEXT NOT NULL,
			last_scanned_rows INTEGER NOT NULL DEFAULT 0,
			last_imported_events INTEGER NOT NULL DEFAULT 0,
			last_skipped_rows INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(target_site_id,stream)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_target_ledger_sync_due ON target_ledger_sync_states(next_attempt_at,target_site_id,stream)`,
		`CREATE TABLE IF NOT EXISTS target_ledger_user_profiles (
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			user_segment TEXT NOT NULL DEFAULT '',
			is_test INTEGER NOT NULL DEFAULT 0,
			is_internal INTEGER NOT NULL DEFAULT 0,
			unlimited INTEGER NOT NULL DEFAULT 0,
			available_quota TEXT NOT NULL DEFAULT '',
			cash_balance TEXT NOT NULL DEFAULT '',
			currency TEXT NOT NULL DEFAULT '',
			quota_cash_factor TEXT NOT NULL DEFAULT '',
			source_hash TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			PRIMARY KEY(target_site_id,user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS target_ledger_cost_resolution_runs (
			target_site_id INTEGER PRIMARY KEY REFERENCES target_sites(id) ON DELETE CASCADE,
			window_start TEXT NOT NULL,
			window_end TEXT NOT NULL,
			scanned_events INTEGER NOT NULL,
			resolved_events INTEGER NOT NULL,
			unresolved_events INTEGER NOT NULL,
			truncated INTEGER NOT NULL DEFAULT 0,
			observed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS target_ledger_cost_resolution_reasons (
			target_site_id INTEGER NOT NULL REFERENCES target_sites(id) ON DELETE CASCADE,
			reason_code TEXT NOT NULL,
			message TEXT NOT NULL,
			affected_events INTEGER NOT NULL,
			samples_json TEXT NOT NULL DEFAULT '[]',
			observed_at TEXT NOT NULL,
			PRIMARY KEY(target_site_id,reason_code)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply target ledger sync schema: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "target_ledger_sync_configs", "initial_lookback_seconds", "INTEGER NOT NULL DEFAULT 86400"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_ledger_sync_configs", "cost_evidence_max_age_seconds", "INTEGER NOT NULL DEFAULT 86400"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_ledger_sync_configs", "supplier_cost_cash_rate", "TEXT NOT NULL DEFAULT '1'"); err != nil {
		return err
	}
	for name, definition := range map[string]string{
		"unlimited": "INTEGER NOT NULL DEFAULT 0", "available_quota": "TEXT NOT NULL DEFAULT ''",
		"cash_balance": "TEXT NOT NULL DEFAULT ''", "currency": "TEXT NOT NULL DEFAULT ''",
		"quota_cash_factor": "TEXT NOT NULL DEFAULT ''",
	} {
		if err := s.ensureColumn(ctx, "target_ledger_user_profiles", name, definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ReplaceTargetLedgerCostResolutionStatus(ctx context.Context, status TargetLedgerCostResolutionStatus) error {
	if status.TargetSiteID <= 0 || status.WindowStart.IsZero() || status.WindowEnd.IsZero() ||
		!status.WindowStart.Before(status.WindowEnd) || status.ObservedAt.IsZero() {
		return fmt.Errorf("cost resolution status requires target, window, and observed_at")
	}
	if status.ScannedEvents < 0 || status.ResolvedEvents < 0 || status.UnresolvedEvents < 0 ||
		status.ResolvedEvents+status.UnresolvedEvents != status.ScannedEvents {
		return fmt.Errorf("cost resolution counts are inconsistent")
	}
	reasonTotal := 0
	seen := make(map[string]struct{}, len(status.Reasons))
	for _, reason := range status.Reasons {
		if reason.Code == "" || reason.Count <= 0 {
			return fmt.Errorf("cost resolution reason requires code and positive count")
		}
		if _, exists := seen[reason.Code]; exists {
			return fmt.Errorf("duplicate cost resolution reason %q", reason.Code)
		}
		seen[reason.Code] = struct{}{}
		reasonTotal += reason.Count
	}
	if reasonTotal != status.UnresolvedEvents {
		return fmt.Errorf("cost resolution reason counts do not match unresolved events")
	}
	return serializedWrite(ctx, s, func() error {
		return s.replaceTargetLedgerCostResolutionStatusOnce(ctx, status)
	})
}

func (s *Store) replaceTargetLedgerCostResolutionStatusOnce(ctx context.Context, status TargetLedgerCostResolutionStatus) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_ledger_cost_resolution_reasons WHERE target_site_id=?`, status.TargetSiteID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO target_ledger_cost_resolution_runs
		(target_site_id,window_start,window_end,scanned_events,resolved_events,unresolved_events,truncated,observed_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(target_site_id) DO UPDATE SET window_start=excluded.window_start,
		window_end=excluded.window_end,scanned_events=excluded.scanned_events,resolved_events=excluded.resolved_events,
		unresolved_events=excluded.unresolved_events,truncated=excluded.truncated,observed_at=excluded.observed_at`,
		status.TargetSiteID, formatTime(status.WindowStart.UTC()), formatTime(status.WindowEnd.UTC()), status.ScannedEvents,
		status.ResolvedEvents, status.UnresolvedEvents, boolInt(status.Truncated), formatTime(status.ObservedAt.UTC())); err != nil {
		return err
	}
	for _, reason := range status.Reasons {
		samples, err := json.Marshal(reason.Samples)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO target_ledger_cost_resolution_reasons
			(target_site_id,reason_code,message,affected_events,samples_json,observed_at) VALUES(?,?,?,?,?,?)`,
			status.TargetSiteID, reason.Code, reason.Message, reason.Count, string(samples), formatTime(status.ObservedAt.UTC())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetTargetLedgerCostResolutionStatus(ctx context.Context, targetID int64) (TargetLedgerCostResolutionStatus, error) {
	var result TargetLedgerCostResolutionStatus
	var windowStart, windowEnd, observed string
	var truncated int
	err := s.db.QueryRowContext(ctx, `SELECT target_site_id,window_start,window_end,scanned_events,resolved_events,
		unresolved_events,truncated,observed_at FROM target_ledger_cost_resolution_runs WHERE target_site_id=?`, targetID).
		Scan(&result.TargetSiteID, &windowStart, &windowEnd, &result.ScannedEvents, &result.ResolvedEvents,
			&result.UnresolvedEvents, &truncated, &observed)
	if err != nil {
		return TargetLedgerCostResolutionStatus{}, err
	}
	result.WindowStart, result.WindowEnd, result.ObservedAt = parseTime(windowStart), parseTime(windowEnd), parseTime(observed)
	result.Truncated = truncated != 0
	rows, err := s.db.QueryContext(ctx, `SELECT reason_code,message,affected_events,samples_json
		FROM target_ledger_cost_resolution_reasons WHERE target_site_id=? ORDER BY affected_events DESC,reason_code`, targetID)
	if err != nil {
		return TargetLedgerCostResolutionStatus{}, err
	}
	defer rows.Close()
	result.Reasons = make([]TargetLedgerCostResolutionReason, 0)
	for rows.Next() {
		var item TargetLedgerCostResolutionReason
		var samples string
		if err := rows.Scan(&item.Code, &item.Message, &item.Count, &samples); err != nil {
			return TargetLedgerCostResolutionStatus{}, err
		}
		if err := json.Unmarshal([]byte(samples), &item.Samples); err != nil {
			return TargetLedgerCostResolutionStatus{}, fmt.Errorf("decode cost resolution samples: %w", err)
		}
		result.Reasons = append(result.Reasons, item)
	}
	return result, rows.Err()
}

func (s *Store) ListTargetLedgerCostResolutionStatuses(ctx context.Context) ([]TargetLedgerCostResolutionStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_site_id FROM target_ledger_cost_resolution_runs ORDER BY target_site_id`)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]TargetLedgerCostResolutionStatus, 0, len(ids))
	for _, id := range ids {
		item, err := s.GetTargetLedgerCostResolutionStatus(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) UpsertTargetLedgerUserProfiles(ctx context.Context, profiles []TargetLedgerUserProfile) error {
	if len(profiles) == 0 {
		return nil
	}
	return serializedWrite(ctx, s, func() error {
		return s.upsertTargetLedgerUserProfilesOnce(ctx, profiles)
	})
}

func (s *Store) upsertTargetLedgerUserProfilesOnce(ctx context.Context, profiles []TargetLedgerUserProfile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changedTargets := map[int64]struct{}{}
	for _, item := range profiles {
		if item.TargetSiteID <= 0 || item.UserID == "" || item.SourceHash == "" || item.ObservedAt.IsZero() {
			return fmt.Errorf("target ledger user profile requires target, user, source hash, and observed_at")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO target_ledger_user_profiles
			(target_site_id,user_id,username,role,user_segment,is_test,is_internal,unlimited,available_quota,cash_balance,currency,quota_cash_factor,source_hash,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(target_site_id,user_id) DO UPDATE SET username=excluded.username,role=excluded.role,
			user_segment=excluded.user_segment,is_test=excluded.is_test,is_internal=excluded.is_internal,
			unlimited=excluded.unlimited,available_quota=excluded.available_quota,cash_balance=excluded.cash_balance,
			currency=excluded.currency,quota_cash_factor=excluded.quota_cash_factor,
			source_hash=excluded.source_hash,observed_at=excluded.observed_at
			WHERE target_ledger_user_profiles.source_hash<>excluded.source_hash`, item.TargetSiteID, item.UserID, item.Username,
			item.Role, item.UserSegment, boolInt(item.IsTest), boolInt(item.IsInternal), boolInt(item.Unlimited), item.AvailableQuota,
			item.CashBalance, item.Currency, item.QuotaCashFactor, item.SourceHash, formatTime(item.ObservedAt.UTC()))
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count > 0 {
			changedTargets[item.TargetSiteID] = struct{}{}
		}
	}
	for targetID := range changedTargets {
		if err := recordOperatingLedgerChangeTx(ctx, tx, "", targetID, "profile_revision", time.Now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceTargetLedgerUserProfiles commits one complete upstream user snapshot.
// A failed fetch never reaches this method, while a successful empty snapshot
// deliberately removes stale users so totals cannot grow forever.
func (s *Store) ReplaceTargetLedgerUserProfiles(ctx context.Context, targetID int64, profiles []TargetLedgerUserProfile) error {
	if targetID <= 0 {
		return fmt.Errorf("target ledger user profile replacement requires target_site_id")
	}
	for _, item := range profiles {
		if item.TargetSiteID != targetID || item.UserID == "" || item.SourceHash == "" || item.ObservedAt.IsZero() {
			return fmt.Errorf("target ledger user profile snapshot contains an invalid row")
		}
	}
	return serializedWrite(ctx, s, func() error {
		return s.replaceTargetLedgerUserProfilesOnce(ctx, targetID, profiles)
	})
}

func (s *Store) replaceTargetLedgerUserProfilesOnce(ctx context.Context, targetID int64, profiles []TargetLedgerUserProfile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	matches, err := targetLedgerProfileSnapshotMatchesTx(ctx, tx, targetID, profiles)
	if err != nil {
		return err
	}
	if matches {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_ledger_user_profiles WHERE target_site_id=?`, targetID); err != nil {
		return err
	}
	for _, item := range profiles {
		if item.TargetSiteID != targetID || item.UserID == "" || item.SourceHash == "" || item.ObservedAt.IsZero() {
			return fmt.Errorf("target ledger user profile snapshot contains an invalid row")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO target_ledger_user_profiles
			(target_site_id,user_id,username,role,user_segment,is_test,is_internal,unlimited,available_quota,cash_balance,currency,quota_cash_factor,source_hash,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.TargetSiteID, item.UserID, item.Username, item.Role, item.UserSegment,
			boolInt(item.IsTest), boolInt(item.IsInternal), boolInt(item.Unlimited), item.AvailableQuota, item.CashBalance,
			item.Currency, item.QuotaCashFactor, item.SourceHash, formatTime(item.ObservedAt.UTC())); err != nil {
			return err
		}
	}
	if err := recordOperatingLedgerChangeTx(ctx, tx, "", targetID, "profile_revision", time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func targetLedgerProfileSnapshotMatchesTx(ctx context.Context, tx *sql.Tx, targetID int64, profiles []TargetLedgerUserProfile) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_id,source_hash FROM target_ledger_user_profiles WHERE target_site_id=?`, targetID)
	if err != nil {
		return false, err
	}
	existing := make(map[string]string)
	for rows.Next() {
		var userID, sourceHash string
		if err := rows.Scan(&userID, &sourceHash); err != nil {
			rows.Close()
			return false, err
		}
		existing[userID] = sourceHash
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if len(existing) != len(profiles) {
		return false, nil
	}
	for _, profile := range profiles {
		if existing[profile.UserID] != profile.SourceHash {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) ListTargetLedgerUserProfiles(ctx context.Context) ([]TargetLedgerUserProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_site_id,user_id,username,role,user_segment,is_test,is_internal,unlimited,
		available_quota,cash_balance,currency,quota_cash_factor,source_hash,observed_at
		FROM target_ledger_user_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]TargetLedgerUserProfile, 0)
	for rows.Next() {
		var item TargetLedgerUserProfile
		var isTest, isInternal, unlimited int
		var observed string
		if err := rows.Scan(&item.TargetSiteID, &item.UserID, &item.Username, &item.Role, &item.UserSegment, &isTest, &isInternal,
			&unlimited, &item.AvailableQuota, &item.CashBalance, &item.Currency, &item.QuotaCashFactor, &item.SourceHash, &observed); err != nil {
			return nil, err
		}
		item.IsTest, item.IsInternal, item.Unlimited, item.ObservedAt = isTest != 0, isInternal != 0, unlimited != 0, parseTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetTargetLedgerSyncConfig(ctx context.Context, targetID int64) (TargetLedgerSyncConfig, error) {
	var item TargetLedgerSyncConfig
	var enabled int
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT target_site_id,enabled,interval_seconds,overlap_seconds,initial_lookback_seconds,page_size,max_pages,currency,
		quota_cash_factor,cost_evidence_max_age_seconds,supplier_cost_cash_rate,log_path,topup_path,user_path,created_at,updated_at FROM target_ledger_sync_configs WHERE target_site_id=?`, targetID).
		Scan(&item.TargetSiteID, &enabled, &item.IntervalSeconds, &item.OverlapSeconds, &item.InitialLookbackSeconds, &item.PageSize, &item.MaxPages,
			&item.Currency, &item.QuotaCashFactor, &item.CostEvidenceMaxAgeSeconds, &item.SupplierCostCashRate, &item.LogPath, &item.TopupPath, &item.UserPath, &created, &updated)
	if err != nil {
		return TargetLedgerSyncConfig{}, err
	}
	item.Enabled, item.CreatedAt, item.UpdatedAt = enabled != 0, parseTime(created), parseTime(updated)
	return item, nil
}

func (s *Store) UpsertTargetLedgerSyncConfig(ctx context.Context, item *TargetLedgerSyncConfig) error {
	if item == nil || item.TargetSiteID <= 0 {
		return fmt.Errorf("target ledger sync config requires target_site_id")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO target_ledger_sync_configs
		(target_site_id,enabled,interval_seconds,overlap_seconds,initial_lookback_seconds,page_size,max_pages,currency,quota_cash_factor,cost_evidence_max_age_seconds,supplier_cost_cash_rate,log_path,topup_path,user_path,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(target_site_id) DO UPDATE SET enabled=excluded.enabled,
		interval_seconds=excluded.interval_seconds,overlap_seconds=excluded.overlap_seconds,initial_lookback_seconds=excluded.initial_lookback_seconds,
		page_size=excluded.page_size,max_pages=excluded.max_pages,
		currency=excluded.currency,quota_cash_factor=excluded.quota_cash_factor,cost_evidence_max_age_seconds=excluded.cost_evidence_max_age_seconds,supplier_cost_cash_rate=excluded.supplier_cost_cash_rate,log_path=excluded.log_path,topup_path=excluded.topup_path,
		user_path=excluded.user_path,updated_at=excluded.updated_at`, item.TargetSiteID, boolInt(item.Enabled), item.IntervalSeconds,
		item.OverlapSeconds, item.InitialLookbackSeconds, item.PageSize, item.MaxPages, item.Currency, item.QuotaCashFactor,
		item.CostEvidenceMaxAgeSeconds, item.SupplierCostCashRate, item.LogPath, item.TopupPath, item.UserPath,
		formatTime(now), formatTime(now))
	if err == nil {
		stored, readErr := s.GetTargetLedgerSyncConfig(ctx, item.TargetSiteID)
		if readErr == nil {
			*item = stored
		}
	}
	return err
}

func (s *Store) GetTargetLedgerSyncState(ctx context.Context, targetID int64, stream string) (TargetLedgerSyncState, error) {
	return scanTargetLedgerSyncState(s.db.QueryRowContext(ctx, targetLedgerSyncStateSelect+` WHERE target_site_id=? AND stream=?`, targetID, stream))
}

func (s *Store) ListTargetLedgerSyncStates(ctx context.Context, targetID int64) ([]TargetLedgerSyncState, error) {
	rows, err := s.db.QueryContext(ctx, targetLedgerSyncStateSelect+` WHERE target_site_id=? ORDER BY stream`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]TargetLedgerSyncState, 0, 3)
	for rows.Next() {
		item, scanErr := scanTargetLedgerSyncState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) RecordTargetLedgerSyncSuccess(ctx context.Context, item TargetLedgerSyncState) error {
	now := time.Now().UTC()
	if item.LastAttemptAt == nil {
		item.LastAttemptAt = &now
	}
	if item.LastSuccessAt == nil {
		item.LastSuccessAt = item.LastAttemptAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO target_ledger_sync_states
		(target_site_id,stream,cursor_time,cursor_id,status,last_error,last_attempt_at,last_success_at,next_attempt_at,
		last_scanned_rows,last_imported_events,last_skipped_rows,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(target_site_id,stream) DO UPDATE SET cursor_time=excluded.cursor_time,cursor_id=excluded.cursor_id,status=excluded.status,
		last_error='',last_attempt_at=excluded.last_attempt_at,last_success_at=excluded.last_success_at,next_attempt_at=excluded.next_attempt_at,
		last_scanned_rows=excluded.last_scanned_rows,last_imported_events=excluded.last_imported_events,
		last_skipped_rows=excluded.last_skipped_rows,updated_at=excluded.updated_at`, item.TargetSiteID, item.Stream,
		nullableTime(item.CursorTime), item.CursorID, "healthy", "", nullableTime(item.LastAttemptAt), nullableTime(item.LastSuccessAt),
		formatTime(item.NextAttemptAt.UTC()), item.LastScannedRows, item.LastImportedEvents, item.LastSkippedRows, formatTime(now))
	return err
}

// RecordTargetLedgerSyncFailure deliberately leaves the last-known-good cursor
// and last_success_at untouched on conflict.
func (s *Store) RecordTargetLedgerSyncFailure(ctx context.Context, targetID int64, stream, status, message string,
	attemptedAt, nextAttemptAt time.Time, scanned, skipped int) error {
	if len(message) > 2000 {
		message = message[:2000]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO target_ledger_sync_states
		(target_site_id,stream,cursor_time,cursor_id,status,last_error,last_attempt_at,last_success_at,next_attempt_at,
		last_scanned_rows,last_imported_events,last_skipped_rows,updated_at) VALUES(?, ?,NULL,'',?,?,?,NULL,?,?,0,?,?)
		ON CONFLICT(target_site_id,stream) DO UPDATE SET status=excluded.status,last_error=excluded.last_error,
		last_attempt_at=excluded.last_attempt_at,next_attempt_at=excluded.next_attempt_at,last_scanned_rows=excluded.last_scanned_rows,
		last_imported_events=0,last_skipped_rows=excluded.last_skipped_rows,updated_at=excluded.updated_at`, targetID, stream,
		status, message, formatTime(attemptedAt.UTC()), formatTime(nextAttemptAt.UTC()), scanned, skipped, formatTime(time.Now().UTC()))
	return err
}

const targetLedgerSyncStateSelect = `SELECT target_site_id,stream,cursor_time,cursor_id,status,last_error,last_attempt_at,last_success_at,
	next_attempt_at,last_scanned_rows,last_imported_events,last_skipped_rows,updated_at FROM target_ledger_sync_states`

func scanTargetLedgerSyncState(row scanner) (TargetLedgerSyncState, error) {
	var item TargetLedgerSyncState
	var cursor, attempted, succeeded sql.NullString
	var next, updated string
	err := row.Scan(&item.TargetSiteID, &item.Stream, &cursor, &item.CursorID, &item.Status, &item.LastError, &attempted, &succeeded,
		&next, &item.LastScannedRows, &item.LastImportedEvents, &item.LastSkippedRows, &updated)
	if err != nil {
		return TargetLedgerSyncState{}, err
	}
	item.CursorTime, item.LastAttemptAt, item.LastSuccessAt = nullableParsedTime(cursor), nullableParsedTime(attempted), nullableParsedTime(succeeded)
	item.NextAttemptAt, item.UpdatedAt = parseTime(next), parseTime(updated)
	return item, nil
}
