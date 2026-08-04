package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
)

var ErrConfigCASConflict = errors.New("configuration changed after preview")

const configPreviewLifetime = 30 * time.Minute

type configQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type configState struct {
	bundle             domain.PortableConfig
	credentialsByID    map[int64]domain.CredentialReference
	credentialIDs      map[string][]int64
	suppliersByURL     map[string]domain.Supplier
	targetsByURL       map[string]domain.TargetSite
	policiesByKey      map[string]domain.PricingPolicy
	connectorsByName   map[string][]domain.ConnectorSpec
	connectorNamesByID map[int64]string
	settingsByKey      map[string]json.RawMessage
}

type pendingConfigObject struct {
	ObjectType string
	ObjectKey  string
	Payload    any
	Reason     string
}

func (s *Store) ExportPortableConfig(ctx context.Context) (domain.PortableConfig, string, error) {
	state, err := loadConfigState(ctx, s.db)
	if err != nil {
		return domain.PortableConfig{}, "", err
	}
	value := state.bundle
	now := time.Now().UTC()
	value.ExportedAt = &now
	hash, err := portableConfigHash(state.bundle)
	return value, hash, err
}

func (s *Store) PreviewPortableConfig(ctx context.Context, request domain.ConfigImportPreviewRequest) (domain.ConfigImportPreview, error) {
	if err := validateImportMode(request.Mode); err != nil {
		return domain.ConfigImportPreview{}, err
	}
	if err := ValidatePortableConfig(request.Bundle); err != nil {
		return domain.ConfigImportPreview{}, err
	}
	request.Bundle = canonicalPortableConfig(request.Bundle)
	current, err := loadConfigState(ctx, s.db)
	if err != nil {
		return domain.ConfigImportPreview{}, err
	}
	expectedHash, err := portableConfigHash(current.bundle)
	if err != nil {
		return domain.ConfigImportPreview{}, err
	}
	bundleHash, err := portableConfigHash(request.Bundle)
	if err != nil {
		return domain.ConfigImportPreview{}, err
	}
	actions, warnings := buildConfigImportPlan(current, request.Bundle, request.Mode, bundleHash)
	summary := summarizeConfigActions(actions)
	now := time.Now().UTC()
	hashPayload := struct {
		Expected string                      `json:"expected"`
		Bundle   string                      `json:"bundle"`
		Mode     domain.ConfigImportMode     `json:"mode"`
		Actions  []domain.ConfigObjectAction `json:"actions"`
		IssuedAt string                      `json:"issued_at"`
	}{Expected: expectedHash, Bundle: bundleHash, Mode: request.Mode, Actions: actions, IssuedAt: formatTime(now)}
	previewHash := hashJSON(hashPayload)
	preview := domain.ConfigImportPreview{
		SchemaVersion: domain.PortableConfigSchemaVersion,
		Mode:          request.Mode, PreviewHash: previewHash, ExpectedConfigHash: expectedHash, BundleHash: bundleHash,
		Actions: actions, Summary: summary, Warnings: warnings, CreatedAt: now, ExpiresAt: now.Add(configPreviewLifetime),
	}
	bundleJSON, _ := json.Marshal(request.Bundle)
	reportJSON, _ := json.Marshal(preview)
	result, err := s.db.ExecContext(ctx, `INSERT INTO config_import_previews
		(preview_hash,expected_config_hash,bundle_hash,mode,bundle_json,report_json,expires_at,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, preview.PreviewHash, preview.ExpectedConfigHash, preview.BundleHash, preview.Mode,
		string(bundleJSON), string(reportJSON), formatTime(preview.ExpiresAt), formatTime(preview.CreatedAt))
	if err != nil {
		return domain.ConfigImportPreview{}, fmt.Errorf("save configuration preview: %w", err)
	}
	preview.ID, _ = result.LastInsertId()
	return preview, nil
}

func (s *Store) ApplyPortableConfig(ctx context.Context, previewID int64, previewHash string) (domain.ConfigImportRun, error) {
	if previewID <= 0 || strings.TrimSpace(previewHash) == "" {
		return domain.ConfigImportRun{}, errors.New("preview_id and preview_hash are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	defer tx.Rollback()

	var storedHash, expectedHash, bundleHash, modeText, bundleJSON, reportJSON, expires string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT preview_hash,expected_config_hash,bundle_hash,mode,bundle_json,report_json,expires_at,consumed_at
		FROM config_import_previews WHERE id=?`, previewID).
		Scan(&storedHash, &expectedHash, &bundleHash, &modeText, &bundleJSON, &reportJSON, &expires, &consumed)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	if storedHash != previewHash {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: preview hash does not match", ErrConfigCASConflict)
	}
	if consumed.Valid {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: preview has already been consumed", ErrConfigCASConflict)
	}
	if !parseTime(expires).After(time.Now().UTC()) {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: preview has expired", ErrConfigCASConflict)
	}
	var bundle domain.PortableConfig
	if err := json.Unmarshal([]byte(bundleJSON), &bundle); err != nil {
		return domain.ConfigImportRun{}, fmt.Errorf("decode preview bundle: %w", err)
	}
	var preview domain.ConfigImportPreview
	if err := json.Unmarshal([]byte(reportJSON), &preview); err != nil {
		return domain.ConfigImportRun{}, fmt.Errorf("decode preview report: %w", err)
	}
	mode := domain.ConfigImportMode(modeText)
	current, err := loadConfigState(ctx, tx)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	currentHash, err := portableConfigHash(current.bundle)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	if currentHash != expectedHash {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: expected %s, current %s", ErrConfigCASConflict, expectedHash, currentHash)
	}
	actions, warnings := buildConfigImportPlan(current, bundle, mode, bundleHash)
	if hashJSON(struct {
		Expected string                      `json:"expected"`
		Bundle   string                      `json:"bundle"`
		Mode     domain.ConfigImportMode     `json:"mode"`
		Actions  []domain.ConfigObjectAction `json:"actions"`
		IssuedAt string                      `json:"issued_at"`
	}{currentHash, bundleHash, mode, actions, formatTime(preview.CreatedAt)}) != storedHash {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: preview plan no longer matches", ErrConfigCASConflict)
	}
	pending, err := applyPortableBundle(ctx, tx, current, bundle, mode, bundleHash, actions)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	after, err := loadConfigState(ctx, tx)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	afterHash, err := portableConfigHash(after.bundle)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	beforeJSON, _ := json.Marshal(current.bundle)
	afterJSON, _ := json.Marshal(after.bundle)
	beforeVersionID, err := saveConfigVersion(ctx, tx, currentHash, current.bundle.SchemaVersion, string(beforeJSON), "before_import")
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	versionID, err := saveConfigVersion(ctx, tx, afterHash, after.bundle.SchemaVersion, string(afterJSON), "import")
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	preview.ID = previewID
	preview.Actions = actions
	preview.Summary = summarizeConfigActions(actions)
	preview.Warnings = warnings
	finalReport, _ := json.Marshal(preview)
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO config_import_runs
		(preview_id,mode,status,before_hash,after_hash,before_version_id,version_id,before_document_json,report_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, previewID, mode, "applied", currentHash, afterHash, beforeVersionID, versionID,
		string(beforeJSON), string(finalReport), formatTime(now))
	if err != nil {
		return domain.ConfigImportRun{}, fmt.Errorf("save configuration import: %w", err)
	}
	runID, _ := result.LastInsertId()
	for _, item := range pending {
		payload, _ := json.Marshal(item.Payload)
		if _, err := tx.ExecContext(ctx, `INSERT INTO config_import_pending_objects
			(import_id,object_type,object_key,payload_json,reason,created_at) VALUES(?,?,?,?,?,?)`,
			runID, item.ObjectType, item.ObjectKey, string(payload), item.Reason, formatTime(now)); err != nil {
			return domain.ConfigImportRun{}, err
		}
	}
	result, err = tx.ExecContext(ctx, `UPDATE config_import_previews SET consumed_at=? WHERE id=? AND consumed_at IS NULL`, formatTime(now), previewID)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: preview was consumed concurrently", ErrConfigCASConflict)
	}
	if err := tx.Commit(); err != nil {
		return domain.ConfigImportRun{}, err
	}
	return domain.ConfigImportRun{ID: runID, PreviewID: previewID, Mode: mode, Status: "applied", BeforeHash: currentHash,
		AfterHash: afterHash, VersionID: versionID, BeforeVersionID: beforeVersionID, Summary: preview.Summary,
		Actions: actions, PendingObjects: publicPendingObjects(pending), Warnings: warnings, CreatedAt: now}, nil
}

func (s *Store) GetConfigImportRun(ctx context.Context, id int64) (domain.ConfigImportRun, error) {
	item, err := scanConfigImportRun(s.db.QueryRowContext(ctx, `SELECT id,preview_id,mode,status,before_hash,after_hash,
		before_version_id,version_id,report_json,created_at,rolled_back_at FROM config_import_runs WHERE id=?`, id))
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	item.PendingObjects, err = s.listPendingConfigObjects(ctx, id)
	return item, err
}

func (s *Store) RollbackPortableConfig(ctx context.Context, id int64) (domain.ConfigImportRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	defer tx.Rollback()
	var run domain.ConfigImportRun
	var modeText, reportJSON, beforeJSON, created string
	var rolledBack sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,preview_id,mode,status,before_hash,after_hash,before_version_id,version_id,
		before_document_json,report_json,created_at,rolled_back_at FROM config_import_runs WHERE id=?`, id).
		Scan(&run.ID, &run.PreviewID, &modeText, &run.Status, &run.BeforeHash, &run.AfterHash, &run.BeforeVersionID,
			&run.VersionID, &beforeJSON, &reportJSON, &created, &rolledBack)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	run.Mode = domain.ConfigImportMode(modeText)
	run.CreatedAt = parseTime(created)
	if run.Status != "applied" || rolledBack.Valid {
		return domain.ConfigImportRun{}, fmt.Errorf("configuration import %d is not rollback eligible", id)
	}
	current, err := loadConfigState(ctx, tx)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	currentHash, err := portableConfigHash(current.bundle)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	if currentHash != run.AfterHash {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: rollback expected %s, current %s", ErrConfigCASConflict, run.AfterHash, currentHash)
	}
	var before domain.PortableConfig
	if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
		return domain.ConfigImportRun{}, fmt.Errorf("decode rollback snapshot: %w", err)
	}
	if err := restorePortableBundle(ctx, tx, current, before); err != nil {
		return domain.ConfigImportRun{}, err
	}
	restored, err := loadConfigState(ctx, tx)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	restoredHash, err := portableConfigHash(restored.bundle)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	if restoredHash != run.BeforeHash {
		return domain.ConfigImportRun{}, fmt.Errorf("rollback readback mismatch: expected %s, got %s", run.BeforeHash, restoredHash)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE config_import_runs SET status='rolled_back',rolled_back_at=?
		WHERE id=? AND status='applied' AND rolled_back_at IS NULL`, formatTime(now), id)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return domain.ConfigImportRun{}, fmt.Errorf("%w: import changed concurrently", ErrConfigCASConflict)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM config_import_pending_objects WHERE import_id=?`, id); err != nil {
		return domain.ConfigImportRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConfigImportRun{}, err
	}
	run.Status = "rolled_back"
	run.RolledBackAt = &now
	var preview domain.ConfigImportPreview
	if json.Unmarshal([]byte(reportJSON), &preview) == nil {
		run.Summary = preview.Summary
		run.Actions = preview.Actions
		run.Warnings = preview.Warnings
	}
	return run, nil
}

func scanConfigImportRun(row scanner) (domain.ConfigImportRun, error) {
	var item domain.ConfigImportRun
	var mode, reportJSON, created string
	var rolledBack sql.NullString
	err := row.Scan(&item.ID, &item.PreviewID, &mode, &item.Status, &item.BeforeHash, &item.AfterHash,
		&item.BeforeVersionID, &item.VersionID, &reportJSON, &created, &rolledBack)
	if err != nil {
		return domain.ConfigImportRun{}, err
	}
	item.Mode = domain.ConfigImportMode(mode)
	item.CreatedAt = parseTime(created)
	if rolledBack.Valid {
		value := parseTime(rolledBack.String)
		item.RolledBackAt = &value
	}
	var preview domain.ConfigImportPreview
	if err := json.Unmarshal([]byte(reportJSON), &preview); err != nil {
		return domain.ConfigImportRun{}, err
	}
	item.Summary = preview.Summary
	item.Actions = preview.Actions
	item.Warnings = preview.Warnings
	return item, nil
}

func (s *Store) listPendingConfigObjects(ctx context.Context, importID int64) ([]domain.ConfigPendingObject, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_type,object_key,reason FROM config_import_pending_objects WHERE import_id=? ORDER BY id`, importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ConfigPendingObject, 0)
	for rows.Next() {
		var item domain.ConfigPendingObject
		if err := rows.Scan(&item.ObjectType, &item.ObjectKey, &item.Reason); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func publicPendingObjects(items []pendingConfigObject) []domain.ConfigPendingObject {
	result := make([]domain.ConfigPendingObject, 0, len(items))
	for _, item := range items {
		result = append(result, domain.ConfigPendingObject{ObjectType: item.ObjectType, ObjectKey: item.ObjectKey, Reason: item.Reason})
	}
	return result
}

func loadConfigState(ctx context.Context, query configQuerier) (configState, error) {
	state := configState{
		bundle:          domain.PortableConfig{SchemaVersion: domain.PortableConfigSchemaVersion},
		credentialsByID: make(map[int64]domain.CredentialReference), credentialIDs: make(map[string][]int64),
		suppliersByURL: make(map[string]domain.Supplier), targetsByURL: make(map[string]domain.TargetSite),
		policiesByKey: make(map[string]domain.PricingPolicy), connectorsByName: make(map[string][]domain.ConnectorSpec),
		connectorNamesByID: make(map[int64]string), settingsByKey: make(map[string]json.RawMessage),
	}
	rows, err := query.QueryContext(ctx, `SELECT id,name,kind,site_url FROM credentials ORDER BY id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var id int64
		var ref domain.CredentialReference
		if err := rows.Scan(&id, &ref.Name, &ref.Kind, &ref.SiteURL); err != nil {
			rows.Close()
			return state, err
		}
		ref.SiteURL = portableURL(ref.SiteURL)
		state.credentialsByID[id] = ref
		key := credentialReferenceKey(ref)
		state.credentialIDs[key] = append(state.credentialIDs[key], id)
	}
	if err := rows.Close(); err != nil {
		return state, err
	}

	rows, err = query.QueryContext(ctx, `SELECT id,name,schema_version,spec_json,enabled,created_at,updated_at FROM connector_specs ORDER BY name,id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var item domain.ConnectorSpec
		var enabled int
		var created, updated string
		if err := rows.Scan(&item.ID, &item.Name, &item.SchemaVersion, &item.SpecJSON, &enabled, &created, &updated); err != nil {
			rows.Close()
			return state, err
		}
		item.Enabled = enabled != 0
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		state.connectorsByName[normalizedName(item.Name)] = append(state.connectorsByName[normalizedName(item.Name)], item)
		state.connectorNamesByID[item.ID] = item.Name
		safeSpec, reasons := sanitizeConnectorSpec([]byte(item.SpecJSON))
		state.bundle.ConnectorSpecs = append(state.bundle.ConnectorSpecs, domain.PortableConnectorSpec{
			Name: item.Name, SchemaVersion: item.SchemaVersion, Spec: safeSpec,
			Enabled: item.Enabled && len(reasons) == 0, DisabledReasons: reasons,
		})
	}
	if err := rows.Close(); err != nil {
		return state, err
	}

	rows, err = query.QueryContext(ctx, supplierSelect+` ORDER BY name,id`)
	if err != nil {
		return state, err
	}
	usedCredentials := make(map[string]domain.CredentialReference)
	for rows.Next() {
		item, scanErr := scanSupplier(rows)
		if scanErr != nil {
			rows.Close()
			return state, scanErr
		}
		item.BaseURL = portableURL(item.BaseURL)
		state.suppliersByURL[normalizedURL(item.BaseURL)] = item
		portable := domain.PortableSupplier{Name: item.Name, BaseURL: item.BaseURL, Adapter: item.Adapter,
			PricingPath: safeRelativePath(item.PricingPath), BalancePath: safeRelativePath(item.BalancePath), Enabled: item.Enabled,
			PollIntervalSeconds: item.PollIntervalSeconds, RechargeValueFactor: item.RechargeValueFactor,
			FocusGroups: nonNilStrings(item.FocusGroups),
			FocusModels: nonNilStrings(item.FocusModels), Metadata: safeSupplierMetadata(item.Metadata)}
		if item.CredentialID != nil {
			if ref, ok := state.credentialsByID[*item.CredentialID]; ok {
				portable.Credential = &ref
				usedCredentials[credentialReferenceKey(ref)] = ref
			}
		}
		if rawID := strings.TrimSpace(item.Metadata["connector_spec_id"]); rawID != "" {
			if id, parseErr := strconv.ParseInt(rawID, 10, 64); parseErr == nil {
				portable.ConnectorSpec = state.connectorNamesByID[id]
			}
		}
		state.bundle.Suppliers = append(state.bundle.Suppliers, portable)
	}
	if err := rows.Close(); err != nil {
		return state, err
	}

	rows, err = query.QueryContext(ctx, targetSelect+` ORDER BY name,id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		item, scanErr := scanTarget(rows)
		if scanErr != nil {
			rows.Close()
			return state, scanErr
		}
		item.BaseURL = portableURL(item.BaseURL)
		state.targetsByURL[normalizedURL(item.BaseURL)] = item
		ref, ok := state.credentialsByID[item.CredentialID]
		if !ok {
			rows.Close()
			return state, fmt.Errorf("target %d references missing credential %d", item.ID, item.CredentialID)
		}
		usedCredentials[credentialReferenceKey(ref)] = ref
		state.bundle.Targets = append(state.bundle.Targets, domain.PortableTarget{Name: item.Name, BaseURL: item.BaseURL,
			Adapter: item.Adapter, Credential: ref, Enabled: item.Enabled, WriteMode: item.WriteMode})
	}
	if err := rows.Close(); err != nil {
		return state, err
	}

	rows, err = query.QueryContext(ctx, policySelect+` ORDER BY id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		item, scanErr := scanPolicy(rows)
		if scanErr != nil {
			rows.Close()
			return state, scanErr
		}
		supplier, supplierOK := supplierByID(state.suppliersByURL, item.SupplierID)
		target, targetOK := targetByID(state.targetsByURL, item.TargetSiteID)
		if !supplierOK || !targetOK {
			rows.Close()
			return state, fmt.Errorf("policy %d has unresolved supplier or target", item.ID)
		}
		key := policyNaturalKey(target.BaseURL, item.TargetGroup)
		state.policiesByKey[key] = item
		state.bundle.PricingPolicies = append(state.bundle.PricingPolicies, portablePolicy(item, supplier, target))
	}
	if err := rows.Close(); err != nil {
		return state, err
	}

	rows, err = query.QueryContext(ctx, `SELECT key,value_json FROM settings ORDER BY key`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return state, err
		}
		if !portableSettingKey(key) || !json.Valid([]byte(raw)) {
			continue
		}
		value, safe := sanitizeGenericJSON([]byte(raw))
		if !safe {
			continue
		}
		state.settingsByKey[key] = value
		state.bundle.Settings = append(state.bundle.Settings, domain.PortableSetting{Key: key, Value: value})
	}
	if err := rows.Close(); err != nil {
		return state, err
	}
	for _, ref := range usedCredentials {
		state.bundle.CredentialRefs = append(state.bundle.CredentialRefs, ref)
	}
	state.bundle = canonicalPortableConfig(state.bundle)
	return state, nil
}

func buildConfigImportPlan(current configState, bundle domain.PortableConfig, mode domain.ConfigImportMode, bundleHash string) ([]domain.ConfigObjectAction, []string) {
	actions := make([]domain.ConfigObjectAction, 0, len(bundle.CredentialRefs)+len(bundle.ConnectorSpecs)+len(bundle.Suppliers)+len(bundle.Targets)+len(bundle.PricingPolicies)+len(bundle.Settings))
	warnings := make([]string, 0)
	credentialResolved := make(map[string]bool)
	for _, ref := range bundle.CredentialRefs {
		key := credentialReferenceKey(ref)
		ids := current.credentialIDs[key]
		action := domain.ConfigObjectAction{ObjectType: "credential", ObjectKey: key, Action: "resolve", Enabled: len(ids) == 1}
		switch len(ids) {
		case 0:
			action.Action = "unresolved"
			action.Reasons = []string{"matching credential placeholder is not configured; secrets are never imported"}
		case 1:
			credentialResolved[key] = true
			action.ResolvedObject = strconv.FormatInt(ids[0], 10)
		default:
			action.Action = "unresolved"
			action.Conflict = true
			action.Reasons = []string{"credential placeholder matches multiple local credentials"}
		}
		actions = append(actions, action)
	}

	connectorAvailable := make(map[string]bool)
	for name, values := range current.connectorsByName {
		connectorAvailable[name] = len(values) == 1 && values[0].Enabled
	}
	copySuffix := shortHash(bundleHash)
	for _, item := range bundle.ConnectorSpecs {
		key := normalizedName(item.Name)
		existing := current.connectorsByName[key]
		action := domain.ConfigObjectAction{ObjectType: "connector_spec", ObjectKey: item.Name, Enabled: item.Enabled && len(item.DisabledReasons) == 0}
		if len(item.DisabledReasons) > 0 {
			action.Enabled = false
			action.Reasons = append(action.Reasons, item.DisabledReasons...)
		}
		switch {
		case len(existing) > 1:
			action.Action = "unresolved"
			action.Conflict = true
			action.Enabled = false
			action.Reasons = append(action.Reasons, "multiple local connector specs share this name")
		case len(existing) == 0:
			action.Action = "create"
			connectorAvailable[key] = action.Enabled
		case mode == domain.ConfigImportOverwrite:
			action.Action = "overwrite"
			action.Conflict = true
			connectorAvailable[key] = action.Enabled
		case mode == domain.ConfigImportCopy:
			action.Action = "copy"
			action.Conflict = true
			action.ResolvedObject = uniqueConnectorCopyName(current, item.Name, copySuffix)
			connectorAvailable[key] = action.Enabled
		default:
			action.Action = "reuse"
			action.Conflict = true
			action.Enabled = existing[0].Enabled
			connectorAvailable[key] = action.Enabled
			action.Reasons = append(action.Reasons, "existing connector spec is preserved in add mode")
		}
		actions = append(actions, action)
	}
	supplierAvailable := make(map[string]bool)
	for key, item := range current.suppliersByURL {
		supplierAvailable[key] = item.Enabled
	}
	for _, item := range bundle.Suppliers {
		key := normalizedURL(item.BaseURL)
		_, exists := current.suppliersByURL[key]
		action := domain.ConfigObjectAction{ObjectType: "supplier", ObjectKey: item.BaseURL, Enabled: item.Enabled}
		if exists {
			action.Conflict = true
			switch mode {
			case domain.ConfigImportOverwrite:
				action.Action = "overwrite"
			case domain.ConfigImportCopy:
				action.Action = "reuse"
				action.Enabled = current.suppliersByURL[key].Enabled
				action.Reasons = append(action.Reasons, "supplier endpoints are unique; copy mode safely reuses the existing endpoint")
			default:
				action.Action = "reuse"
				action.Enabled = current.suppliersByURL[key].Enabled
				action.Reasons = append(action.Reasons, "existing supplier is preserved in add mode")
			}
		} else {
			action.Action = "create"
		}
		if action.Action == "create" || action.Action == "overwrite" {
			if item.Credential != nil && !credentialResolved[credentialReferenceKey(*item.Credential)] {
				action.Enabled = false
				action.Reasons = append(action.Reasons, "credential reference is unresolved; supplier will remain disabled")
			}
			if item.ConnectorSpec != "" && !connectorAvailable[normalizedName(item.ConnectorSpec)] {
				action.Enabled = false
				action.Reasons = append(action.Reasons, "connector spec reference is unresolved; supplier will remain disabled")
			}
		}
		supplierAvailable[key] = action.Enabled
		actions = append(actions, action)
	}

	targetAvailable := make(map[string]bool)
	for key, item := range current.targetsByURL {
		targetAvailable[key] = item.Enabled
	}
	for _, item := range bundle.Targets {
		key := normalizedURL(item.BaseURL)
		_, exists := current.targetsByURL[key]
		credentialOK := credentialResolved[credentialReferenceKey(item.Credential)]
		action := domain.ConfigObjectAction{ObjectType: "target", ObjectKey: item.BaseURL, Enabled: item.Enabled}
		if exists {
			action.Conflict = true
			switch mode {
			case domain.ConfigImportOverwrite:
				action.Action = "overwrite"
				if !credentialOK {
					action.Enabled = false
					action.Reasons = append(action.Reasons, "credential reference is unresolved; target will remain disabled")
				}
			case domain.ConfigImportCopy:
				action.Action = "reuse"
				action.Enabled = current.targetsByURL[key].Enabled
				action.Reasons = append(action.Reasons, "target endpoints are unique; copy mode safely reuses the existing endpoint")
			default:
				action.Action = "reuse"
				action.Enabled = current.targetsByURL[key].Enabled
				action.Reasons = append(action.Reasons, "existing target is preserved in add mode")
			}
		} else if !credentialOK {
			action.Action = "pending_disabled"
			action.Enabled = false
			action.Reasons = append(action.Reasons, "credential reference is unresolved; target is saved in the pending configuration area")
		} else {
			action.Action = "create"
		}
		targetAvailable[key] = action.Enabled
		actions = append(actions, action)
	}

	for _, item := range bundle.PricingPolicies {
		targetKey := normalizedURL(item.Target.BaseURL)
		key := policyNaturalKey(item.Target.BaseURL, item.TargetGroup)
		_, exists := current.policiesByKey[key]
		action := domain.ConfigObjectAction{ObjectType: "pricing_policy", ObjectKey: key, Enabled: item.Enabled}
		if !supplierAvailable[normalizedURL(item.Supplier.BaseURL)] || !targetAvailable[targetKey] {
			action.Action = "pending_disabled"
			action.Enabled = false
			action.Reasons = []string{"supplier or target reference is unresolved; policy is saved in the pending configuration area"}
		} else if !exists {
			action.Action = "create"
		} else {
			action.Conflict = true
			switch mode {
			case domain.ConfigImportOverwrite:
				action.Action = "overwrite"
			case domain.ConfigImportCopy:
				action.Action = "copy"
				action.Enabled = false
				action.ResolvedObject = copiedPolicyGroup(item.TargetGroup, copySuffix)
				action.Reasons = []string{"copied policy is disabled until its generated target group is reviewed"}
			default:
				action.Action = "reuse"
				action.Enabled = current.policiesByKey[key].Enabled
				action.Reasons = []string{"existing pricing policy is preserved in add mode"}
			}
		}
		actions = append(actions, action)
	}

	for _, item := range bundle.Settings {
		_, exists := current.settingsByKey[item.Key]
		action := domain.ConfigObjectAction{ObjectType: "setting", ObjectKey: item.Key, Enabled: true}
		switch {
		case !portableSettingKey(item.Key):
			action.Action = "skipped"
			action.Enabled = false
			action.Reasons = []string{"setting is not on the portable non-sensitive allowlist"}
		case !exists:
			action.Action = "create"
		case mode == domain.ConfigImportOverwrite:
			action.Action = "overwrite"
			action.Conflict = true
		default:
			action.Action = "reuse"
			action.Conflict = true
			action.Reasons = []string{"existing setting is preserved outside overwrite mode"}
		}
		actions = append(actions, action)
	}
	if len(bundle.CredentialRefs) > 0 && len(credentialResolved) < len(bundle.CredentialRefs) {
		warnings = append(warnings, "Some credential placeholders are unresolved. Related objects will not be enabled.")
	}
	return actions, warnings
}

func summarizeConfigActions(actions []domain.ConfigObjectAction) domain.ConfigImportSummary {
	var result domain.ConfigImportSummary
	for _, action := range actions {
		switch action.Action {
		case "create":
			result.Create++
		case "overwrite":
			result.Overwrite++
		case "reuse", "resolve":
			result.Reuse++
		case "copy":
			result.Copy++
		case "unresolved", "pending_disabled":
			result.Unresolved++
		case "skipped":
			result.Skipped++
		}
		if !action.Enabled && action.ObjectType != "credential" {
			result.Disabled++
		}
	}
	return result
}

func applyPortableBundle(ctx context.Context, tx *sql.Tx, current configState, bundle domain.PortableConfig, mode domain.ConfigImportMode, bundleHash string, actions []domain.ConfigObjectAction) ([]pendingConfigObject, error) {
	actionMap := make(map[string]domain.ConfigObjectAction, len(actions))
	for _, action := range actions {
		actionMap[action.ObjectType+"\x00"+action.ObjectKey] = action
	}
	now := formatTime(time.Now().UTC())
	connectorIDs := make(map[string]int64)
	for name, specs := range current.connectorsByName {
		if len(specs) == 1 {
			connectorIDs[name] = specs[0].ID
		}
	}
	for _, item := range bundle.ConnectorSpecs {
		action := actionMap["connector_spec\x00"+item.Name]
		if action.Action == "reuse" {
			continue
		}
		name := item.Name
		if action.Action == "copy" {
			name = action.ResolvedObject
		}
		enabled := item.Enabled && len(item.DisabledReasons) == 0
		if action.Action == "unresolved" || action.Action == "skipped" {
			enabled = false
		}
		var id int64
		if action.Action == "overwrite" {
			existing := current.connectorsByName[normalizedName(item.Name)]
			if len(existing) != 1 {
				return nil, fmt.Errorf("connector spec %q cannot be overwritten: local identity is ambiguous", item.Name)
			}
			id = existing[0].ID
			if _, err := tx.ExecContext(ctx, `UPDATE connector_specs SET name=?,schema_version=?,spec_json=?,enabled=?,updated_at=? WHERE id=?`,
				name, item.SchemaVersion, string(item.Spec), boolInt(enabled), now, id); err != nil {
				return nil, err
			}
		} else {
			result, err := tx.ExecContext(ctx, `INSERT INTO connector_specs(name,schema_version,spec_json,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
				name, item.SchemaVersion, string(item.Spec), boolInt(enabled), now, now)
			if err != nil {
				return nil, fmt.Errorf("create connector spec %q: %w", name, err)
			}
			id, _ = result.LastInsertId()
		}
		connectorIDs[normalizedName(item.Name)] = id
	}

	credentialIDs := current.credentialIDs
	supplierIDs := make(map[string]int64)
	for key, item := range current.suppliersByURL {
		supplierIDs[key] = item.ID
	}
	for _, item := range bundle.Suppliers {
		key := normalizedURL(item.BaseURL)
		action := actionMap["supplier\x00"+item.BaseURL]
		if action.Action == "reuse" {
			continue
		}
		var credentialID any
		credentialOK := true
		if item.Credential != nil {
			ids := credentialIDs[credentialReferenceKey(*item.Credential)]
			if len(ids) == 1 {
				credentialID = ids[0]
			} else {
				credentialOK = false
			}
		}
		metadata := cloneStringMap(item.Metadata)
		if item.ConnectorSpec != "" {
			if id, ok := connectorIDs[normalizedName(item.ConnectorSpec)]; ok {
				metadata["connector_spec_id"] = strconv.FormatInt(id, 10)
			} else {
				credentialOK = false
			}
		}
		enabled := item.Enabled && credentialOK && action.Enabled
		factor, factorErr := ratios.NormalizeRechargeValueFactor(item.RechargeValueFactor)
		if factorErr != nil {
			return nil, fmt.Errorf("supplier %q: %w", item.Name, factorErr)
		}
		if action.Action == "overwrite" {
			existing, ok := current.suppliersByURL[key]
			if !ok {
				return nil, fmt.Errorf("supplier %q disappeared during import", item.BaseURL)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE suppliers SET name=?,base_url=?,adapter=?,pricing_path=?,balance_path=?,credential_id=?,enabled=?,poll_interval_seconds=?,recharge_value_factor=?,focus_groups_json=?,focus_models_json=?,metadata_json=?,updated_at=? WHERE id=?`,
				item.Name, portableURL(item.BaseURL), item.Adapter, safeRelativePath(item.PricingPath), safeRelativePath(item.BalancePath), credentialID,
				boolInt(enabled), positivePoll(item.PollIntervalSeconds), factor, mustJSON(nonNilStrings(item.FocusGroups)), mustJSON(nonNilStrings(item.FocusModels)), mustJSON(metadata), now, existing.ID); err != nil {
				return nil, err
			}
			supplierIDs[key] = existing.ID
		} else {
			result, err := tx.ExecContext(ctx, `INSERT INTO suppliers(name,base_url,adapter,pricing_path,balance_path,credential_id,enabled,poll_interval_seconds,recharge_value_factor,focus_groups_json,focus_models_json,metadata_json,last_status,last_error,next_collection_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				item.Name, portableURL(item.BaseURL), defaultString(item.Adapter, "auto"), safeRelativePath(item.PricingPath), safeRelativePath(item.BalancePath), credentialID,
				boolInt(enabled), positivePoll(item.PollIntervalSeconds), factor, mustJSON(nonNilStrings(item.FocusGroups)), mustJSON(nonNilStrings(item.FocusModels)), mustJSON(metadata), "pending", "", now, now, now)
			if err != nil {
				return nil, fmt.Errorf("create supplier %q: %w", item.Name, err)
			}
			id, _ := result.LastInsertId()
			supplierIDs[key] = id
		}
	}

	targetIDs := make(map[string]int64)
	for key, item := range current.targetsByURL {
		targetIDs[key] = item.ID
	}
	pending := make([]pendingConfigObject, 0)
	for _, item := range bundle.Targets {
		key := normalizedURL(item.BaseURL)
		action := actionMap["target\x00"+item.BaseURL]
		ids := credentialIDs[credentialReferenceKey(item.Credential)]
		credentialOK := len(ids) == 1
		if action.Action == "reuse" {
			continue
		}
		if action.Action == "pending_disabled" {
			pending = append(pending, pendingConfigObject{ObjectType: "target", ObjectKey: item.BaseURL, Payload: item, Reason: strings.Join(action.Reasons, "; ")})
			continue
		}
		existing, exists := current.targetsByURL[key]
		if action.Action == "overwrite" && !exists {
			return nil, fmt.Errorf("target %q disappeared during import", item.BaseURL)
		}
		credentialID := int64(0)
		if credentialOK {
			credentialID = ids[0]
		}
		enabled := item.Enabled && credentialOK && action.Enabled
		if action.Action == "overwrite" {
			// Keep the old foreign key when a disabled overwrite is unresolved. This
			// preserves referential integrity while making the target unusable.
			if credentialID == 0 {
				credentialID = existing.CredentialID
			}
			if _, err := tx.ExecContext(ctx, `UPDATE target_sites SET name=?,base_url=?,adapter=?,credential_id=?,enabled=?,write_mode=?,updated_at=? WHERE id=?`,
				item.Name, portableURL(item.BaseURL), defaultString(item.Adapter, "newapi"), credentialID, boolInt(enabled), defaultString(item.WriteMode, "approval"), now, existing.ID); err != nil {
				return nil, err
			}
			targetIDs[key] = existing.ID
		} else {
			result, err := tx.ExecContext(ctx, `INSERT INTO target_sites(name,base_url,adapter,credential_id,enabled,write_mode,last_status,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				item.Name, portableURL(item.BaseURL), defaultString(item.Adapter, "newapi"), credentialID, boolInt(enabled), defaultString(item.WriteMode, "approval"), "pending", "", now, now)
			if err != nil {
				return nil, fmt.Errorf("create target %q: %w", item.Name, err)
			}
			targetIDs[key], _ = result.LastInsertId()
		}
	}

	for _, item := range bundle.PricingPolicies {
		action := actionMap["pricing_policy\x00"+policyNaturalKey(item.Target.BaseURL, item.TargetGroup)]
		targetID := targetIDs[normalizedURL(item.Target.BaseURL)]
		supplierID := supplierIDs[normalizedURL(item.Supplier.BaseURL)]
		if action.Action == "pending_disabled" || targetID == 0 || supplierID == 0 {
			pending = append(pending, pendingConfigObject{ObjectType: "pricing_policy", ObjectKey: policyNaturalKey(item.Target.BaseURL, item.TargetGroup), Payload: item, Reason: strings.Join(action.Reasons, "; ")})
			continue
		}
		group := item.TargetGroup
		name := item.Name
		enabled := item.Enabled && action.Enabled
		if action.Action == "copy" {
			group = action.ResolvedObject
			name = name + " (import copy " + shortHash(bundleHash) + ")"
			enabled = false
		}
		policy := domain.PricingPolicy{Name: name, SupplierID: supplierID, SourceGroup: item.SourceGroup, SourceModelPattern: item.SourceModelPattern,
			SourceBasis:  "group_ratio",
			TargetSiteID: targetID, TargetGroup: group, RuleType: item.RuleType, Value: item.Value, ProfitBasis: item.ProfitBasis,
			RevenueCashFactor: item.RevenueCashFactor, CostCashFactor: item.CostCashFactor, RiskBufferRate: item.RiskBufferRate,
			AllowNegativeProfit: item.AllowNegativeProfit, SubsidyBudgetDaily: item.SubsidyBudgetDaily, MinimumRatio: item.MinimumRatio,
			MaximumRatio: item.MaximumRatio, RoundingStep: item.RoundingStep, OnlyFollowIncrease: item.OnlyFollowIncrease,
			OnlyFollowDecrease: item.OnlyFollowDecrease, Enabled: enabled, ExecutionMode: item.ExecutionMode,
			CoverageMode: item.CoverageMode, MinimumCoverage: item.MinimumCoverage}
		applyPolicyDefaults(&policy)
		if err := validatePolicyCoverage(policy); err != nil {
			return nil, fmt.Errorf("pricing policy %q: %w", item.Name, err)
		}
		if action.Action == "reuse" {
			continue
		}
		if action.Action == "overwrite" {
			existing, ok := current.policiesByKey[policyNaturalKey(item.Target.BaseURL, item.TargetGroup)]
			if !ok {
				return nil, fmt.Errorf("pricing policy %q disappeared during import", item.Name)
			}
			policy.ID = existing.ID
			if err := updatePolicyTx(ctx, tx, policy, now); err != nil {
				return nil, err
			}
		} else if err := insertPolicyTx(ctx, tx, policy, now); err != nil {
			return nil, err
		}
	}

	for _, item := range bundle.Settings {
		action := actionMap["setting\x00"+item.Key]
		if action.Action == "reuse" || action.Action == "skipped" {
			continue
		}
		if !portableSettingKey(item.Key) || !json.Valid(item.Value) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, item.Key, string(item.Value), now); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

func insertPolicyTx(ctx context.Context, tx *sql.Tx, policy domain.PricingPolicy, now string) error {
	applyPolicyDefaults(&policy)
	if err := validatePolicyCoverage(policy); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO pricing_policies(name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,profit_basis,revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,coverage_mode,minimum_coverage_ratio,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		policy.Name, policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID, policy.TargetGroup, policy.RuleType, policy.Value,
		policy.ProfitBasis, policy.RevenueCashFactor, policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily,
		policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep, boolInt(policy.OnlyFollowIncrease), boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled), policy.ExecutionMode,
		policy.CoverageMode, policy.MinimumCoverage, now, now)
	return err
}

func updatePolicyTx(ctx context.Context, tx *sql.Tx, policy domain.PricingPolicy, now string) error {
	applyPolicyDefaults(&policy)
	if err := validatePolicyCoverage(policy); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE pricing_policies SET name=?,supplier_id=?,source_group=?,source_model_pattern=?,source_basis=?,target_site_id=?,target_group=?,rule_type=?,value=?,profit_basis=?,revenue_cash_factor=?,cost_cash_factor=?,risk_buffer_rate=?,allow_negative_profit=?,subsidy_budget_daily=?,minimum_ratio=?,maximum_ratio=?,rounding_step=?,only_follow_increase=?,only_follow_decrease=?,enabled=?,execution_mode=?,coverage_mode=?,minimum_coverage_ratio=?,updated_at=? WHERE id=?`,
		policy.Name, policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID, policy.TargetGroup, policy.RuleType, policy.Value,
		policy.ProfitBasis, policy.RevenueCashFactor, policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily,
		policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep, boolInt(policy.OnlyFollowIncrease), boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled), policy.ExecutionMode,
		policy.CoverageMode, policy.MinimumCoverage, now, policy.ID)
	return err
}

func restorePortableBundle(ctx context.Context, tx *sql.Tx, current configState, before domain.PortableConfig) error {
	wantedPolicies := make(map[string]bool)
	for _, item := range before.PricingPolicies {
		wantedPolicies[policyNaturalKey(item.Target.BaseURL, item.TargetGroup)] = true
	}
	for key, item := range current.policiesByKey {
		if !wantedPolicies[key] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_policies WHERE id=?`, item.ID); err != nil {
				return fmt.Errorf("remove imported pricing policy %d during rollback: %w", item.ID, err)
			}
		}
	}
	wantedTargets := make(map[string]bool)
	for _, item := range before.Targets {
		wantedTargets[normalizedURL(item.BaseURL)] = true
	}
	for key, item := range current.targetsByURL {
		if !wantedTargets[key] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM target_sites WHERE id=?`, item.ID); err != nil {
				return fmt.Errorf("remove imported target %d during rollback: %w", item.ID, err)
			}
		}
	}
	wantedSuppliers := make(map[string]bool)
	for _, item := range before.Suppliers {
		wantedSuppliers[normalizedURL(item.BaseURL)] = true
	}
	for key, item := range current.suppliersByURL {
		if !wantedSuppliers[key] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM suppliers WHERE id=?`, item.ID); err != nil {
				return fmt.Errorf("remove imported supplier %d during rollback: %w", item.ID, err)
			}
		}
	}
	wantedConnectors := make(map[string]int)
	for _, item := range before.ConnectorSpecs {
		wantedConnectors[normalizedName(item.Name)]++
	}
	for name, items := range current.connectorsByName {
		keep := wantedConnectors[name]
		for index, item := range items {
			if index >= keep {
				if _, err := tx.ExecContext(ctx, `DELETE FROM connector_specs WHERE id=?`, item.ID); err != nil {
					return fmt.Errorf("remove imported connector spec %d during rollback: %w", item.ID, err)
				}
			}
		}
	}
	wantedSettings := make(map[string]bool)
	for _, item := range before.Settings {
		wantedSettings[item.Key] = true
	}
	for key := range current.settingsByKey {
		if !wantedSettings[key] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key); err != nil {
				return err
			}
		}
	}
	trimmed, err := loadConfigState(ctx, tx)
	if err != nil {
		return err
	}
	bundleHash, err := portableConfigHash(before)
	if err != nil {
		return err
	}
	actions, _ := buildConfigImportPlan(trimmed, before, domain.ConfigImportOverwrite, bundleHash)
	for _, action := range actions {
		if action.Action == "unresolved" || action.Action == "pending_disabled" || action.Action == "skipped" {
			return fmt.Errorf("rollback reference %s %q is unresolved: %s", action.ObjectType, action.ObjectKey, strings.Join(action.Reasons, "; "))
		}
	}
	pending, err := applyPortableBundle(ctx, tx, trimmed, before, domain.ConfigImportOverwrite, bundleHash, actions)
	if err != nil {
		return err
	}
	if len(pending) != 0 {
		return errors.New("rollback produced pending configuration objects")
	}
	return nil
}

func saveConfigVersion(ctx context.Context, tx *sql.Tx, hash, schemaVersion, document, source string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO config_versions(version_hash,schema_version,document_json,source,created_at)
		VALUES(?,?,?,?,?) ON CONFLICT(version_hash) DO NOTHING`, hash, schemaVersion, document, source, formatTime(time.Now().UTC())); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM config_versions WHERE version_hash=?`, hash).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func portableConfigHash(bundle domain.PortableConfig) (string, error) {
	canonical := canonicalPortableConfig(bundle)
	canonical.ExportedAt = nil
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalPortableConfig(bundle domain.PortableConfig) domain.PortableConfig {
	bundle.ExportedAt = nil
	if bundle.CredentialRefs == nil {
		bundle.CredentialRefs = []domain.CredentialReference{}
	}
	if bundle.Suppliers == nil {
		bundle.Suppliers = []domain.PortableSupplier{}
	}
	if bundle.Targets == nil {
		bundle.Targets = []domain.PortableTarget{}
	}
	if bundle.PricingPolicies == nil {
		bundle.PricingPolicies = []domain.PortablePricingPolicy{}
	}
	if bundle.ConnectorSpecs == nil {
		bundle.ConnectorSpecs = []domain.PortableConnectorSpec{}
	}
	if bundle.Settings == nil {
		bundle.Settings = []domain.PortableSetting{}
	}
	for index := range bundle.CredentialRefs {
		bundle.CredentialRefs[index].Name = strings.TrimSpace(bundle.CredentialRefs[index].Name)
		bundle.CredentialRefs[index].Kind = strings.TrimSpace(bundle.CredentialRefs[index].Kind)
		bundle.CredentialRefs[index].SiteURL = portableURL(bundle.CredentialRefs[index].SiteURL)
	}
	for index := range bundle.Suppliers {
		item := &bundle.Suppliers[index]
		item.Name = strings.TrimSpace(item.Name)
		item.BaseURL = portableURL(item.BaseURL)
		item.RechargeValueFactor, _ = ratios.NormalizeRechargeValueFactor(item.RechargeValueFactor)
		item.FocusGroups = sortedUnique(item.FocusGroups)
		item.FocusModels = sortedUnique(item.FocusModels)
		if item.Metadata == nil {
			item.Metadata = map[string]string{}
		}
		if item.Credential != nil {
			item.Credential.SiteURL = portableURL(item.Credential.SiteURL)
		}
	}
	for index := range bundle.Targets {
		bundle.Targets[index].Name = strings.TrimSpace(bundle.Targets[index].Name)
		bundle.Targets[index].BaseURL = portableURL(bundle.Targets[index].BaseURL)
		bundle.Targets[index].Credential.SiteURL = portableURL(bundle.Targets[index].Credential.SiteURL)
	}
	for index := range bundle.PricingPolicies {
		item := &bundle.PricingPolicies[index]
		item.Supplier.BaseURL = portableURL(item.Supplier.BaseURL)
		item.Target.BaseURL = portableURL(item.Target.BaseURL)
	}
	for index := range bundle.ConnectorSpecs {
		item := &bundle.ConnectorSpecs[index]
		item.Name = strings.TrimSpace(item.Name)
		item.DisabledReasons = sortedUnique(item.DisabledReasons)
		item.Spec = canonicalJSON(item.Spec)
	}
	for index := range bundle.Settings {
		bundle.Settings[index].Key = strings.TrimSpace(bundle.Settings[index].Key)
		bundle.Settings[index].Value = canonicalJSON(bundle.Settings[index].Value)
	}
	sort.Slice(bundle.CredentialRefs, func(i, j int) bool {
		return credentialReferenceKey(bundle.CredentialRefs[i]) < credentialReferenceKey(bundle.CredentialRefs[j])
	})
	sort.Slice(bundle.Suppliers, func(i, j int) bool {
		return normalizedURL(bundle.Suppliers[i].BaseURL) < normalizedURL(bundle.Suppliers[j].BaseURL)
	})
	sort.Slice(bundle.Targets, func(i, j int) bool {
		return normalizedURL(bundle.Targets[i].BaseURL) < normalizedURL(bundle.Targets[j].BaseURL)
	})
	sort.Slice(bundle.PricingPolicies, func(i, j int) bool {
		return policyNaturalKey(bundle.PricingPolicies[i].Target.BaseURL, bundle.PricingPolicies[i].TargetGroup) < policyNaturalKey(bundle.PricingPolicies[j].Target.BaseURL, bundle.PricingPolicies[j].TargetGroup)
	})
	sort.Slice(bundle.ConnectorSpecs, func(i, j int) bool {
		return normalizedName(bundle.ConnectorSpecs[i].Name) < normalizedName(bundle.ConnectorSpecs[j].Name)
	})
	sort.Slice(bundle.Settings, func(i, j int) bool { return bundle.Settings[i].Key < bundle.Settings[j].Key })
	return bundle
}

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return encoded
}

func hashJSON(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func ValidatePortableConfig(bundle domain.PortableConfig) error {
	if bundle.SchemaVersion != domain.PortableConfigSchemaVersion {
		return fmt.Errorf("unsupported schema_version %q; expected %q", bundle.SchemaVersion, domain.PortableConfigSchemaVersion)
	}
	if len(bundle.Suppliers) > 1000 || len(bundle.Targets) > 100 || len(bundle.PricingPolicies) > 5000 || len(bundle.ConnectorSpecs) > 500 || len(bundle.Settings) > 200 {
		return errors.New("configuration document exceeds the supported object limits")
	}
	credentialKeys := make(map[string]bool)
	for index, ref := range bundle.CredentialRefs {
		if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Kind) == "" || strings.TrimSpace(ref.SiteURL) == "" {
			return fmt.Errorf("credential_refs[%d] requires name, kind, and site_url", index)
		}
		if err := validatePortableURL(ref.SiteURL); err != nil {
			return fmt.Errorf("credential_refs[%d].site_url: %w", index, err)
		}
		key := credentialReferenceKey(ref)
		if credentialKeys[key] {
			return fmt.Errorf("credential reference %q is duplicated", key)
		}
		credentialKeys[key] = true
	}
	connectorNames := make(map[string]bool)
	for index, item := range bundle.ConnectorSpecs {
		name := normalizedName(item.Name)
		if name == "" || item.SchemaVersion == "" || !json.Valid(item.Spec) {
			return fmt.Errorf("connector_specs[%d] requires name, schema_version, and valid spec JSON", index)
		}
		if connectorNames[name] {
			return fmt.Errorf("connector spec name %q is duplicated", item.Name)
		}
		connectorNames[name] = true
		_, reasons := sanitizeConnectorSpec(item.Spec)
		if len(reasons) > 0 {
			return fmt.Errorf("connector spec %q may contain sensitive static data; replace it with credential_ref before import", item.Name)
		}
		for _, reason := range item.DisabledReasons {
			if secretLikeValue(reason) {
				return fmt.Errorf("connector spec %q disabled reason contains sensitive-looking data", item.Name)
			}
		}
	}
	supplierURLs := make(map[string]bool)
	for index, item := range bundle.Suppliers {
		if strings.TrimSpace(item.Name) == "" {
			return fmt.Errorf("suppliers[%d].name is required", index)
		}
		if err := validatePortableURL(item.BaseURL); err != nil {
			return fmt.Errorf("suppliers[%d].base_url: %w", index, err)
		}
		key := normalizedURL(item.BaseURL)
		if supplierURLs[key] {
			return fmt.Errorf("supplier endpoint %q is duplicated", item.BaseURL)
		}
		supplierURLs[key] = true
		if item.Credential != nil && !credentialKeys[credentialReferenceKey(*item.Credential)] {
			return fmt.Errorf("supplier %q credential is missing from credential_refs", item.Name)
		}
		if item.Credential != nil && !portableSameOrigin(item.Credential.SiteURL, item.BaseURL) {
			return fmt.Errorf("supplier %q credential origin does not match base_url", item.Name)
		}
		if item.PollIntervalSeconds < 0 {
			return fmt.Errorf("supplier %q poll_interval_seconds cannot be negative", item.Name)
		}
		if _, err := ratios.NormalizeRechargeValueFactor(item.RechargeValueFactor); err != nil {
			return fmt.Errorf("supplier %q: %w", item.Name, err)
		}
		if err := validatePortablePath(item.PricingPath); err != nil {
			return fmt.Errorf("supplier %q pricing_path: %w", item.Name, err)
		}
		if err := validatePortablePath(item.BalancePath); err != nil {
			return fmt.Errorf("supplier %q balance_path: %w", item.Name, err)
		}
		if !equalStringMaps(item.Metadata, safeSupplierMetadata(item.Metadata)) {
			return fmt.Errorf("supplier %q metadata contains non-portable or sensitive keys", item.Name)
		}
	}
	targetURLs := make(map[string]bool)
	for index, item := range bundle.Targets {
		if strings.TrimSpace(item.Name) == "" {
			return fmt.Errorf("targets[%d].name is required", index)
		}
		if err := validatePortableURL(item.BaseURL); err != nil {
			return fmt.Errorf("targets[%d].base_url: %w", index, err)
		}
		key := normalizedURL(item.BaseURL)
		if targetURLs[key] {
			return fmt.Errorf("target endpoint %q is duplicated", item.BaseURL)
		}
		targetURLs[key] = true
		if !credentialKeys[credentialReferenceKey(item.Credential)] {
			return fmt.Errorf("target %q credential is missing from credential_refs", item.Name)
		}
		if !portableSameOrigin(item.Credential.SiteURL, item.BaseURL) {
			return fmt.Errorf("target %q credential origin does not match base_url", item.Name)
		}
	}
	policyKeys := make(map[string]bool)
	for index, item := range bundle.PricingPolicies {
		if strings.TrimSpace(item.Name) == "" || strings.TrimSpace(item.TargetGroup) == "" {
			return fmt.Errorf("pricing_policies[%d] requires name and target_group", index)
		}
		if err := validatePortableURL(item.Supplier.BaseURL); err != nil {
			return fmt.Errorf("pricing_policies[%d].supplier: %w", index, err)
		}
		if err := validatePortableURL(item.Target.BaseURL); err != nil {
			return fmt.Errorf("pricing_policies[%d].target: %w", index, err)
		}
		key := policyNaturalKey(item.Target.BaseURL, item.TargetGroup)
		if policyKeys[key] {
			return fmt.Errorf("pricing policy target %q is duplicated", key)
		}
		policyKeys[key] = true
	}
	settingKeys := make(map[string]bool)
	for index, item := range bundle.Settings {
		if !portableSettingKey(item.Key) {
			return fmt.Errorf("settings[%d] key %q is not portable", index, item.Key)
		}
		if settingKeys[item.Key] || !json.Valid(item.Value) {
			return fmt.Errorf("settings[%d] is duplicated or invalid", index)
		}
		if _, safe := sanitizeGenericJSON(item.Value); !safe {
			return fmt.Errorf("settings[%d] contains sensitive-looking data", index)
		}
		settingKeys[item.Key] = true
	}
	return nil
}

func validateImportMode(mode domain.ConfigImportMode) error {
	switch mode {
	case domain.ConfigImportAdd, domain.ConfigImportOverwrite, domain.ConfigImportCopy:
		return nil
	default:
		return fmt.Errorf("unsupported import mode %q", mode)
	}
}

func validatePortableURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("invalid HTTP URL %q", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("query strings and fragments are not allowed in portable site URLs")
	}
	return nil
}

func portableSameOrigin(left, right string) bool {
	leftURL, leftErr := url.Parse(strings.TrimSpace(left))
	rightURL, rightErr := url.Parse(strings.TrimSpace(right))
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftPort, rightPort := leftURL.Port(), rightURL.Port()
	if leftPort == "" {
		if strings.EqualFold(leftURL.Scheme, "https") {
			leftPort = "443"
		} else {
			leftPort = "80"
		}
	}
	if rightPort == "" {
		if strings.EqualFold(rightURL.Scheme, "https") {
			rightPort = "443"
		} else {
			rightPort = "80"
		}
	}
	return strings.EqualFold(leftURL.Scheme, rightURL.Scheme) && strings.EqualFold(leftURL.Hostname(), rightURL.Hostname()) && leftPort == rightPort
}

func validatePortablePath(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "?#") {
		return errors.New("query strings and fragments are not portable")
	}
	if secretLikeValue(value) {
		return errors.New("path contains sensitive-looking data")
	}
	return nil
}

func credentialReferenceKey(ref domain.CredentialReference) string {
	return normalizedName(ref.Name) + "\x00" + normalizedName(ref.Kind) + "\x00" + normalizedURL(ref.SiteURL)
}

func normalizedName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizedURL(raw string) string {
	return strings.ToLower(strings.TrimRight(portableURL(raw), "/"))
}

func portableURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/")
}

func policyNaturalKey(targetURL, targetGroup string) string {
	return normalizedURL(targetURL) + "\x00" + strings.TrimSpace(targetGroup)
}

func supplierByID(items map[string]domain.Supplier, id int64) (domain.Supplier, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return domain.Supplier{}, false
}

func targetByID(items map[string]domain.TargetSite, id int64) (domain.TargetSite, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return domain.TargetSite{}, false
}

func portablePolicy(item domain.PricingPolicy, supplier domain.Supplier, target domain.TargetSite) domain.PortablePricingPolicy {
	return domain.PortablePricingPolicy{Name: item.Name,
		Supplier: domain.PortableObjectReference{Name: supplier.Name, BaseURL: supplier.BaseURL}, SourceGroup: item.SourceGroup,
		SourceModelPattern: item.SourceModelPattern, SourceBasis: item.SourceBasis,
		Target:      domain.PortableObjectReference{Name: target.Name, BaseURL: target.BaseURL},
		TargetGroup: item.TargetGroup, RuleType: item.RuleType, Value: item.Value, ProfitBasis: item.ProfitBasis,
		RevenueCashFactor: item.RevenueCashFactor, CostCashFactor: item.CostCashFactor, RiskBufferRate: item.RiskBufferRate,
		AllowNegativeProfit: item.AllowNegativeProfit, SubsidyBudgetDaily: item.SubsidyBudgetDaily, MinimumRatio: item.MinimumRatio,
		MaximumRatio: item.MaximumRatio, RoundingStep: item.RoundingStep, OnlyFollowIncrease: item.OnlyFollowIncrease,
		OnlyFollowDecrease: item.OnlyFollowDecrease, Enabled: item.Enabled, ExecutionMode: item.ExecutionMode,
		CoverageMode: item.CoverageMode, MinimumCoverage: item.MinimumCoverage}
}

func sanitizeConnectorSpec(raw []byte) (json.RawMessage, []string) {
	var root any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return json.RawMessage("{}"), []string{"connector spec was invalid and was redacted"}
	}
	reasons := make([]string, 0)
	object, _ := root.(map[string]any)
	endpoints, _ := object["endpoints"].([]any)
	for _, endpointRaw := range endpoints {
		endpoint, _ := endpointRaw.(map[string]any)
		headers, _ := endpoint["headers"].(map[string]any)
		for headerName, sourceRaw := range headers {
			source, _ := sourceRaw.(map[string]any)
			staticValue, hasStatic := source["static"].(string)
			if !hasStatic || staticValue == "" || publicStaticHeader(headerName) {
				continue
			}
			delete(source, "static")
			source["credential_ref"] = connectorCredentialReference(headerName)
			reasons = append(reasons, "static request header was replaced with a credential reference")
		}
		if body, exists := endpoint["body"]; exists {
			cleaned, safe := sanitizeGenericValue(body)
			endpoint["body"] = cleaned
			if !safe {
				reasons = append(reasons, "sensitive-looking request body values were redacted")
			}
		}
	}
	// The connector schema rejects unknown fields. Header names are dynamic map
	// keys, so a generic key-name scrubber would incorrectly treat X-API-Key as
	// a secret after its value has already been replaced by credential_ref.
	encoded, _ := json.Marshal(root)
	return encoded, sortedUnique(reasons)
}

func sanitizeGenericJSON(raw []byte) (json.RawMessage, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	cleaned, safe := sanitizeGenericValue(value)
	encoded, err := json.Marshal(cleaned)
	return encoded, safe && err == nil
}

func sanitizeGenericValue(value any) (any, bool) {
	safe := true
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if sensitiveFieldName(key) && normalizedName(key) != "credential_ref" {
				result[key] = "<credential-required>"
				safe = false
				continue
			}
			cleaned, childSafe := sanitizeGenericValue(child)
			result[key] = cleaned
			safe = safe && childSafe
		}
		return result, safe
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			cleaned, childSafe := sanitizeGenericValue(child)
			result[index] = cleaned
			safe = safe && childSafe
		}
		return result, safe
	case string:
		if secretLikeValue(typed) {
			return "<credential-required>", false
		}
	}
	return value, true
}

func sensitiveFieldName(value string) bool {
	normalized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	for _, token := range []string{"authorization", "cookie", "api_key", "apikey", "access_token", "refresh_token", "client_secret", "password", "passwd", "encrypted_payload", "private_key", "session_token"} {
		if normalized == token || strings.HasSuffix(normalized, "_"+token) {
			return true
		}
	}
	return normalized == "secret" || normalized == "token" || normalized == "key"
}

func secretLikeValue(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "bearer ") || strings.HasPrefix(lower, "basic ") || strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "sess-") {
		return true
	}
	if strings.Count(trimmed, ".") == 2 {
		parts := strings.Split(trimmed, ".")
		if len(parts[0]) >= 16 && len(parts[1]) >= 16 && len(parts[2]) >= 8 {
			return true
		}
	}
	return false
}

func publicStaticHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "accept", "content-type", "user-agent", "accept-language", "cache-control":
		return true
	default:
		return false
	}
}

func connectorCredentialReference(header string) string {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "authorization", "proxy-authorization":
		return "authorization"
	case "cookie":
		return "cookie"
	default:
		return "headers." + strings.TrimSpace(header)
	}
}

func safeSupplierMetadata(input map[string]string) map[string]string {
	allowed := map[string]bool{
		"catalog_visibility": true, "pricing_page": true, "credential_method": true, "provider": true,
		"region": true, "currency": true, "mode": true, "sub2_mode": true, "notes": true,
		"api_key_management_path": true,
	}
	result := make(map[string]string)
	for key, value := range input {
		if !allowed[key] || secretLikeValue(value) {
			continue
		}
		if key == "api_key_management_path" {
			normalized, err := netguard.NormalizeSameOriginPath(value)
			if err == nil && normalized != "" {
				result[key] = normalized
			}
			continue
		}
		result[key] = value
	}
	return result
}

func portableSettingKey(key string) bool {
	key = strings.TrimSpace(key)
	allowed := map[string]bool{
		"alerts": true, "retention": true, "collection": true, "collector": true, "forecast": true,
		"runway_alert_hours": true, "credential_refresh_interval": true, "credential_refresh_ahead": true,
	}
	if allowed[key] {
		return true
	}
	for _, prefix := range []string{"alerts.", "retention.", "collection.", "collector.", "forecast.", "credentials.refresh.", "intelligence."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func safeRelativePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		if index := strings.IndexByte(value, '?'); index >= 0 {
			return value[:index]
		}
		return strings.SplitN(value, "#", 2)[0]
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	if parsed.IsAbs() {
		return parsed.EscapedPath()
	}
	return parsed.String()
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func cloneStringMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value)+1)
	for key, item := range value {
		result[key] = item
	}
	return result
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func positivePoll(value int) int {
	if value <= 0 {
		return 300
	}
	return value
}

func shortHash(value string) string {
	if len(value) >= 8 {
		return value[:8]
	}
	return value
}

func uniqueConnectorCopyName(current configState, base, suffix string) string {
	candidate := strings.TrimSpace(base) + " (import copy " + suffix + ")"
	if len(current.connectorsByName[normalizedName(candidate)]) == 0 {
		return candidate
	}
	for index := 2; ; index++ {
		value := fmt.Sprintf("%s (import copy %s-%d)", strings.TrimSpace(base), suffix, index)
		if len(current.connectorsByName[normalizedName(value)]) == 0 {
			return value
		}
	}
}

func copiedPolicyGroup(group, suffix string) string {
	return strings.TrimSpace(group) + "__import_copy_" + suffix
}
