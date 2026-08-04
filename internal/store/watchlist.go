package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const WatchSettingsKey = "watchlist.v2"

type WatchSettings struct {
	AutoEnabled    bool `json:"auto_enabled"`
	RetentionDays  int  `json:"retention_days"`
	AutomaticLimit int  `json:"automatic_limit"`
	OverviewLimit  int  `json:"overview_limit"`
}

func DefaultWatchSettings() WatchSettings {
	return WatchSettings{AutoEnabled: true, RetentionDays: 7, AutomaticLimit: 30, OverviewLimit: 5}
}

func NormalizeWatchSettings(value WatchSettings) WatchSettings {
	if value.RetentionDays < 1 || value.RetentionDays > 90 {
		value.RetentionDays = 7
	}
	if value.AutomaticLimit < 1 || value.AutomaticLimit > 200 {
		value.AutomaticLimit = 30
	}
	if value.OverviewLimit < 1 || value.OverviewLimit > 20 {
		value.OverviewLimit = 5
	}
	return value
}

type WatchRecord struct {
	ID             int64     `json:"id"`
	SupplierID     int64     `json:"supplier_id"`
	ScopeType      string    `json:"scope_type"`
	GroupKey       string    `json:"group_key"`
	ModelKey       string    `json:"model_key"`
	ManualPinned   bool      `json:"manual_pinned"`
	ManualRank     int       `json:"manual_rank"`
	DisplayName    string    `json:"display_name"`
	Note           string    `json:"note"`
	SuppressedHash string    `json:"suppressed_hash"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type WatchAutomaticSource struct {
	SupplierID   int64
	GroupKey     string
	Pricing      bool
	Mirror       bool
	Changed      bool
	ChangedAt    time.Time
	PricingAt    time.Time
	MirrorAt     time.Time
	SemanticHash string
}

type WatchGroupRatioSample struct {
	SupplierID int64
	GroupKey   string
	Ratio      string
	ObservedAt time.Time
}

type WatchGroupRatioKey struct {
	SupplierID int64
	GroupKey   string
}

func (s *Store) WatchSettings(ctx context.Context) (WatchSettings, error) {
	settings := DefaultWatchSettings()
	var stored WatchSettings
	if err := s.GetSetting(ctx, WatchSettingsKey, &stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return settings, nil
		}
		return WatchSettings{}, err
	}
	stored = NormalizeWatchSettings(stored)
	return stored, nil
}

func (s *Store) SaveWatchSettings(ctx context.Context, settings WatchSettings) (WatchSettings, error) {
	settings = NormalizeWatchSettings(settings)
	if err := s.SetSetting(ctx, WatchSettingsKey, settings); err != nil {
		return WatchSettings{}, err
	}
	return settings, nil
}

func normalizeWatchIdentity(scopeType, groupKey, modelKey string) (string, string, string, error) {
	scopeType = strings.ToLower(strings.TrimSpace(scopeType))
	groupKey = strings.TrimSpace(groupKey)
	modelKey = strings.TrimSpace(modelKey)
	if scopeType != "group" && scopeType != "model" {
		return "", "", "", fmt.Errorf("关注类型必须是分组或模型")
	}
	if scopeType == "group" && groupKey == "" {
		return "", "", "", fmt.Errorf("关注分组不能为空")
	}
	if scopeType == "model" && modelKey == "" {
		return "", "", "", fmt.Errorf("关注模型不能为空")
	}
	if len(groupKey) > 512 || len(modelKey) > 512 {
		return "", "", "", fmt.Errorf("关注对象名称过长")
	}
	return scopeType, groupKey, modelKey, nil
}

func (s *Store) ListWatchRecords(ctx context.Context) ([]WatchRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,supplier_id,scope_type,group_key,model_key,manual_pinned,
		manual_rank,display_name,note,suppressed_hash,created_at,updated_at
		FROM watch_items ORDER BY manual_pinned DESC,manual_rank,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]WatchRecord, 0)
	for rows.Next() {
		item, scanErr := scanWatchRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanWatchRecord(row scanner) (WatchRecord, error) {
	var item WatchRecord
	var manual int
	var created, updated string
	err := row.Scan(&item.ID, &item.SupplierID, &item.ScopeType, &item.GroupKey, &item.ModelKey, &manual,
		&item.ManualRank, &item.DisplayName, &item.Note, &item.SuppressedHash, &created, &updated)
	item.ManualPinned = manual != 0
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, err
}

func (s *Store) CreateManualWatch(ctx context.Context, item *WatchRecord) error {
	if item == nil {
		return fmt.Errorf("关注对象不能为空")
	}
	scope, group, model, err := normalizeWatchIdentity(item.ScopeType, item.GroupKey, item.ModelKey)
	if err != nil {
		return err
	}
	if _, err := s.GetSupplier(ctx, item.SupplierID); err != nil {
		return err
	}
	var nextRank int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(manual_rank),-1)+1 FROM watch_items WHERE manual_pinned=1`).Scan(&nextRank); err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO watch_items
		(supplier_id,scope_type,group_key,model_key,manual_pinned,manual_rank,display_name,note,suppressed_hash,created_at,updated_at)
		VALUES(?,?,?,?,1,?,?,?,'',?,?)
		ON CONFLICT(supplier_id,scope_type,group_key,model_key) DO UPDATE SET
		manual_pinned=1,manual_rank=CASE WHEN watch_items.manual_pinned=1 THEN watch_items.manual_rank ELSE excluded.manual_rank END,
		display_name=excluded.display_name,note=excluded.note,suppressed_hash='',updated_at=excluded.updated_at`,
		item.SupplierID, scope, group, model, nextRank, strings.TrimSpace(item.DisplayName), strings.TrimSpace(item.Note),
		formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	// SQLite may retain a previous LastInsertId when an UPSERT takes the conflict path.
	// Read the complete persisted identity back so updates and ordering use database truth.
	var created, updated string
	if err := s.db.QueryRowContext(ctx, `SELECT id,manual_rank,created_at,updated_at FROM watch_items
		WHERE supplier_id=? AND scope_type=? AND group_key=? AND model_key=?`,
		item.SupplierID, scope, group, model).Scan(&item.ID, &item.ManualRank, &created, &updated); err != nil {
		return err
	}
	item.ScopeType, item.GroupKey, item.ModelKey = scope, group, model
	item.ManualPinned = true
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	return nil
}

func (s *Store) UpdateManualWatch(ctx context.Context, item WatchRecord) error {
	scope, group, model, err := normalizeWatchIdentity(item.ScopeType, item.GroupKey, item.ModelKey)
	if err != nil {
		return err
	}
	if _, err := s.GetSupplier(ctx, item.SupplierID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE watch_items SET supplier_id=?,scope_type=?,group_key=?,model_key=?,
		manual_pinned=1,display_name=?,note=?,suppressed_hash='',updated_at=? WHERE id=? AND manual_pinned=1`,
		item.SupplierID, scope, group, model, strings.TrimSpace(item.DisplayName), strings.TrimSpace(item.Note),
		formatTime(time.Now().UTC()), item.ID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteManualWatch(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE watch_items SET manual_pinned=0,display_name='',note='',updated_at=?
		WHERE id=? AND manual_pinned=1`, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM watch_items WHERE id=? AND manual_pinned=0 AND suppressed_hash=''`, id)
	return nil
}

func (s *Store) SuppressAutomaticWatch(ctx context.Context, supplierID int64, scopeType, groupKey, modelKey, evidenceHash string) error {
	scope, group, model, err := normalizeWatchIdentity(scopeType, groupKey, modelKey)
	if err != nil {
		return err
	}
	if strings.TrimSpace(evidenceHash) == "" {
		return fmt.Errorf("自动关注证据不能为空")
	}
	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `INSERT INTO watch_items
		(supplier_id,scope_type,group_key,model_key,manual_pinned,manual_rank,display_name,note,suppressed_hash,created_at,updated_at)
		VALUES(?,?,?,?,0,0,'','',?,?,?)
		ON CONFLICT(supplier_id,scope_type,group_key,model_key) DO UPDATE SET
		suppressed_hash=excluded.suppressed_hash,updated_at=excluded.updated_at`,
		supplierID, scope, group, model, strings.TrimSpace(evidenceHash), now, now)
	return err
}

func (s *Store) ReorderManualWatches(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for rank, id := range ids {
		result, execErr := tx.ExecContext(ctx, `UPDATE watch_items SET manual_rank=?,updated_at=? WHERE id=? AND manual_pinned=1`,
			rank, formatTime(time.Now().UTC()), id)
		if execErr != nil {
			return execErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
	}
	return tx.Commit()
}

func (s *Store) ListAutomaticWatchSources(ctx context.Context, cutoff time.Time) ([]WatchAutomaticSource, error) {
	byKey := map[string]*WatchAutomaticSource{}
	key := func(supplierID int64, group string) string {
		return fmt.Sprintf("%d\x00%s", supplierID, strings.TrimSpace(group))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT supplier_id,source_group,updated_at FROM pricing_policies WHERE enabled=1`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var supplierID int64
		var group, updated string
		if err := rows.Scan(&supplierID, &group, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		entry := byKey[key(supplierID, group)]
		if entry == nil {
			entry = &WatchAutomaticSource{SupplierID: supplierID, GroupKey: strings.TrimSpace(group)}
			byKey[key(supplierID, group)] = entry
		}
		entry.Pricing = true
		entry.PricingAt = parseTime(updated)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT supplier_id,source_group,policy_json,updated_at FROM mirror_policies
		WHERE NOT EXISTS (SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var supplierID int64
		var group, payload, updated string
		if err := rows.Scan(&supplierID, &group, &payload, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		var state struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal([]byte(payload), &state) != nil || !state.Enabled {
			continue
		}
		entry := byKey[key(supplierID, group)]
		if entry == nil {
			entry = &WatchAutomaticSource{SupplierID: supplierID, GroupKey: strings.TrimSpace(group)}
			byKey[key(supplierID, group)] = entry
		}
		entry.Mirror = true
		entry.MirrorAt = parseTime(updated)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	supplierRows, err := s.db.QueryContext(ctx, `SELECT id FROM suppliers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var supplierIDs []int64
	for supplierRows.Next() {
		var supplierID int64
		if err := supplierRows.Scan(&supplierID); err != nil {
			supplierRows.Close()
			return nil, err
		}
		supplierIDs = append(supplierIDs, supplierID)
	}
	if err := supplierRows.Close(); err != nil {
		return nil, err
	}
	for _, supplierID := range supplierIDs {
		rows, err = s.db.QueryContext(ctx, `SELECT p.attempt_id,p.source_group,p.source_semantic_hash,p.observed_at
			FROM collection_attempts AS a INDEXED BY idx_collection_attempts_supplier_time
			CROSS JOIN catalog_group_collection_proofs AS p
			WHERE a.supplier_id=? AND a.started_at>=? AND p.attempt_id=a.id AND p.supplier_id=a.supplier_id
			ORDER BY p.source_group,p.attempt_id`, supplierID, formatTime(cutoff.UTC()))
		if err != nil {
			return nil, err
		}
		type recentProof struct {
			attemptID    int64
			group        string
			semanticHash string
			observed     string
		}
		recent := make([]recentProof, 0)
		for rows.Next() {
			var proof recentProof
			if err := rows.Scan(&proof.attemptID, &proof.group, &proof.semanticHash, &proof.observed); err != nil {
				rows.Close()
				return nil, err
			}
			proof.group = strings.TrimSpace(proof.group)
			recent = append(recent, proof)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		currentGroup, previousHash := "", ""
		havePrevious := false
		for _, proof := range recent {
			if proof.group != currentGroup {
				currentGroup, previousHash, havePrevious = proof.group, "", false
				previousErr := s.db.QueryRowContext(ctx, `SELECT source_semantic_hash
					FROM catalog_group_collection_proofs INDEXED BY idx_catalog_group_proofs_lookup
					WHERE supplier_id=? AND source_group=? AND attempt_id<?
					ORDER BY attempt_id DESC LIMIT 1`, supplierID, proof.group, proof.attemptID).Scan(&previousHash)
				if previousErr != nil && !errors.Is(previousErr, sql.ErrNoRows) {
					return nil, previousErr
				}
				havePrevious = previousErr == nil
			}
			if havePrevious && proof.semanticHash != previousHash {
				entry := byKey[key(supplierID, proof.group)]
				if entry == nil {
					entry = &WatchAutomaticSource{SupplierID: supplierID, GroupKey: proof.group}
					byKey[key(supplierID, proof.group)] = entry
				}
				observedAt := parseTime(proof.observed)
				if observedAt.After(entry.ChangedAt) {
					entry.Changed, entry.ChangedAt, entry.SemanticHash = true, observedAt, proof.semanticHash
				}
			}
			previousHash, havePrevious = proof.semanticHash, true
		}
	}
	items := make([]WatchAutomaticSource, 0, len(byKey))
	for _, item := range byKey {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].SupplierID != items[j].SupplierID {
			return items[i].SupplierID < items[j].SupplierID
		}
		return items[i].GroupKey < items[j].GroupKey
	})
	return items, nil
}

func (s *Store) ListWatchGroupRatiosAtOrBefore(ctx context.Context, cutoff time.Time, keys []WatchGroupRatioKey) ([]WatchGroupRatioSample, error) {
	result := make([]WatchGroupRatioSample, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key.GroupKey = strings.TrimSpace(key.GroupKey)
		identity := fmt.Sprintf("%d\x00%s", key.SupplierID, key.GroupKey)
		if key.SupplierID == 0 || key.GroupKey == "" {
			continue
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		var payload, observed string
		err := s.db.QueryRowContext(ctx, `SELECT price_json,observed_at FROM price_observations
			INDEXED BY idx_price_supplier_entity_time
			WHERE supplier_id=? AND entity_type='group' AND entity_key=?
			AND julianday(observed_at)<=julianday(?)
			ORDER BY julianday(observed_at) DESC,id DESC LIMIT 1`, key.SupplierID, key.GroupKey, formatTime(cutoff.UTC())).Scan(&payload, &observed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var price struct {
			ModelRatio string `json:"model_ratio"`
		}
		if err := json.Unmarshal([]byte(payload), &price); err != nil {
			continue
		}
		item := WatchGroupRatioSample{SupplierID: key.SupplierID, GroupKey: key.GroupKey}
		item.Ratio = strings.TrimSpace(price.ModelRatio)
		item.ObservedAt = parseTime(observed)
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) migrateLegacyWatchItems(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,focus_groups_json,focus_models_json FROM suppliers ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read legacy watch configuration: %w", err)
	}
	defer rows.Close()
	type legacy struct {
		supplierID     int64
		groups, models []string
	}
	items := make([]legacy, 0)
	for rows.Next() {
		var supplierID int64
		var groupsJSON, modelsJSON string
		if err := rows.Scan(&supplierID, &groupsJSON, &modelsJSON); err != nil {
			return err
		}
		var groups, models []string
		_ = json.Unmarshal([]byte(groupsJSON), &groups)
		_ = json.Unmarshal([]byte(modelsJSON), &models)
		items = append(items, legacy{supplierID: supplierID, groups: groups, models: models})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	rank := 0
	for _, item := range items {
		for _, group := range item.groups {
			group = strings.TrimSpace(group)
			if group == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO watch_items
				(supplier_id,scope_type,group_key,model_key,manual_pinned,manual_rank,created_at,updated_at)
				VALUES(?,'group',?,'',1,?,?,?)`, item.supplierID, group, rank, now, now); err != nil {
				return err
			}
			rank++
		}
		for _, model := range item.models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO watch_items
				(supplier_id,scope_type,group_key,model_key,manual_pinned,manual_rank,created_at,updated_at)
				VALUES(?,'model','',?,1,?,?,?)`, item.supplierID, model, rank, now, now); err != nil {
				return err
			}
			rank++
		}
	}
	return nil
}
