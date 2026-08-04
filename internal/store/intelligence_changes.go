package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const MaxOperatingLedgerCacheEvents = 250000

type OperatingLedgerChange struct {
	Revision     int64
	EventID      string
	TargetSiteID int64
	ChangeKind   string
	ChangedAt    time.Time
}

type OperatingLedgerCacheSnapshot struct {
	Revision int64
	Events   []OperatingLedgerEvent
	Profiles []TargetLedgerUserProfile
}

func recordOperatingLedgerChangeTx(ctx context.Context, tx *sql.Tx, eventID string, targetSiteID int64, kind string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO operating_ledger_changes(event_id,target_site_id,change_kind,changed_at)
		VALUES(?,?,?,?)`, strings.TrimSpace(eventID), targetSiteID, strings.TrimSpace(kind), formatTime(now.UTC()))
	return err
}

func (s *Store) CurrentOperatingLedgerRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM operating_ledger_changes`).Scan(&revision)
	return revision, err
}

func (s *Store) OperatingLedgerCacheIdentity(ctx context.Context) (string, error) {
	var identity string
	err := s.db.QueryRowContext(ctx, `SELECT instance_id FROM operating_ledger_cache_identity WHERE id=1`).Scan(&identity)
	return identity, err
}

func (s *Store) ListOperatingLedgerChanges(ctx context.Context, afterRevision int64, limit int) ([]OperatingLedgerChange, error) {
	if afterRevision < 0 {
		afterRevision = 0
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision,event_id,target_site_id,change_kind,changed_at
		FROM operating_ledger_changes WHERE revision>? ORDER BY revision LIMIT ?`, afterRevision, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OperatingLedgerChange, 0, limit)
	for rows.Next() {
		var item OperatingLedgerChange
		var changedAt string
		if err := rows.Scan(&item.Revision, &item.EventID, &item.TargetSiteID, &item.ChangeKind, &changedAt); err != nil {
			return nil, err
		}
		item.ChangedAt = parseTime(changedAt)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) PruneOperatingLedgerChanges(ctx context.Context, throughRevision int64, keepTail, limit int) (int, error) {
	if throughRevision <= 0 {
		return 0, nil
	}
	if keepTail < 1 {
		keepTail = 1
	}
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	threshold := throughRevision - int64(keepTail)
	if threshold <= 0 {
		return 0, nil
	}
	return serializedWriteResult(ctx, s, func() (int, error) {
		result, err := s.db.ExecContext(ctx, `DELETE FROM operating_ledger_changes WHERE revision IN (
			SELECT revision FROM operating_ledger_changes WHERE revision<=? ORDER BY revision LIMIT ?
		)`, threshold, limit)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		return int(count), err
	})
}

func (s *Store) ListOperatingLedgerEventsByIDs(ctx context.Context, ids []string) ([]OperatingLedgerEvent, error) {
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			unique[id] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return []OperatingLedgerEvent{}, nil
	}
	if len(unique) > 2000 {
		return nil, fmt.Errorf("operating ledger change batch exceeds 2000 event IDs")
	}
	ordered := make([]string, 0, len(unique))
	for id := range unique {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ordered)), ",")
	args := make([]any, len(ordered))
	for index := range ordered {
		args[index] = ordered[index]
	}
	rows, err := s.db.QueryContext(ctx, operatingLedgerEventSelect+` WHERE event_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]OperatingLedgerEvent, 0, len(ordered))
	for rows.Next() {
		item, err := scanOperatingLedgerEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) LoadOperatingLedgerCacheSnapshot(ctx context.Context) (OperatingLedgerCacheSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	defer tx.Rollback()
	var result OperatingLedgerCacheSnapshot
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM operating_ledger_changes`).Scan(&result.Revision); err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, operatingLedgerEventSelect+` ORDER BY occurred_at,event_id LIMIT ?`, MaxOperatingLedgerCacheEvents+1)
	if err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	for rows.Next() {
		item, scanErr := scanOperatingLedgerEvent(rows)
		if scanErr != nil {
			rows.Close()
			return OperatingLedgerCacheSnapshot{}, scanErr
		}
		result.Events = append(result.Events, item)
	}
	if err := rows.Close(); err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	if len(result.Events) > MaxOperatingLedgerCacheEvents {
		return OperatingLedgerCacheSnapshot{}, fmt.Errorf("operating ledger cache exceeds %d events", MaxOperatingLedgerCacheEvents)
	}
	result.Profiles, err = listTargetLedgerUserProfilesQuery(ctx, tx, 0)
	if err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatingLedgerCacheSnapshot{}, err
	}
	return result, nil
}

func (s *Store) ListTargetLedgerUserProfilesForTarget(ctx context.Context, targetID int64) ([]TargetLedgerUserProfile, error) {
	if targetID <= 0 {
		return nil, fmt.Errorf("target ledger profile query requires target_site_id")
	}
	return listTargetLedgerUserProfilesQuery(ctx, s.db, targetID)
}

func ApplyTargetLedgerUserProfiles(events []OperatingLedgerEvent, profiles []TargetLedgerUserProfile) []OperatingLedgerEvent {
	profileByUser := make(map[string]TargetLedgerUserProfile, len(profiles))
	for _, profile := range profiles {
		profileByUser[strconv.FormatInt(profile.TargetSiteID, 10)+"\x00"+profile.UserID] = profile
	}
	result := make([]OperatingLedgerEvent, len(events))
	for index, event := range events {
		result[index] = event
		profile, ok := profileByUser[strconv.FormatInt(event.TargetSiteID, 10)+"\x00"+event.SubjectUserID]
		if !ok {
			continue
		}
		if strings.TrimSpace(result[index].UserSegment) == "" {
			result[index].UserSegment = profile.UserSegment
		}
		if strings.TrimSpace(result[index].SubjectRole) == "" {
			result[index].SubjectRole = profile.Role
		}
		result[index].IsTest = result[index].IsTest || profile.IsTest
		result[index].IsInternal = result[index].IsInternal || profile.IsInternal
	}
	return result
}

type profileQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listTargetLedgerUserProfilesQuery(ctx context.Context, queryer profileQueryer, targetID int64) ([]TargetLedgerUserProfile, error) {
	query := `SELECT target_site_id,user_id,username,role,user_segment,is_test,is_internal,unlimited,
		available_quota,cash_balance,currency,quota_cash_factor,source_hash,observed_at FROM target_ledger_user_profiles`
	args := []any{}
	if targetID > 0 {
		query += ` WHERE target_site_id=?`
		args = append(args, targetID)
	}
	query += ` ORDER BY target_site_id,user_id`
	rows, err := queryer.QueryContext(ctx, query, args...)
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
		item.IsTest, item.IsInternal, item.Unlimited = isTest != 0, isInternal != 0, unlimited != 0
		item.ObservedAt = parseTime(observed)
		result = append(result, item)
	}
	return result, rows.Err()
}
