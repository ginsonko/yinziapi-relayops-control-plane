package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type watchlistResponse struct {
	Items               []consoleWatch      `json:"items"`
	Settings            store.WatchSettings `json:"settings"`
	AutomaticCandidates int                 `json:"automatic_candidates"`
}

type watchItemInput struct {
	SupplierID   int64  `json:"supplier_id"`
	ScopeType    string `json:"scope_type"`
	GroupKey     string `json:"group_key"`
	ModelKey     string `json:"model_key"`
	DisplayName  string `json:"display_name"`
	Note         string `json:"note"`
	EvidenceHash string `json:"evidence_hash"`
}

type watchOrderInput struct {
	IDs []int64 `json:"ids"`
}

type watchCandidate struct {
	supplier      domain.Supplier
	snapshot      domain.CatalogSnapshot
	record        *store.WatchRecord
	scopeType     string
	groupKey      string
	modelKey      string
	manual        bool
	manualRank    int
	sourcePricing bool
	sourceMirror  bool
	sourceChange  bool
	changeAt      time.Time
	pricingAt     time.Time
	mirrorAt      time.Time
	semanticHash  string
}

func watchIdentity(supplierID int64, scopeType, groupKey, modelKey string) string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s", supplierID, scopeType, strings.TrimSpace(groupKey), strings.TrimSpace(modelKey))
}

func canonicalWatchGroup(snapshot domain.CatalogSnapshot, value string) string {
	value = strings.TrimSpace(value)
	for _, group := range snapshot.Groups {
		if group.ExternalID == value || group.Name == value {
			if strings.TrimSpace(group.ExternalID) != "" {
				return group.ExternalID
			}
			return group.Name
		}
	}
	return value
}

func findWatchGroup(snapshot domain.CatalogSnapshot, value string) (domain.GroupOffer, bool) {
	for _, group := range snapshot.Groups {
		if group.ExternalID == value || group.Name == value {
			return group, true
		}
	}
	return domain.GroupOffer{}, false
}

func findWatchModel(snapshot domain.CatalogSnapshot, value string) (domain.ModelOffer, bool) {
	for _, model := range snapshot.Models {
		if model.ExternalID == value || model.Name == value {
			return model, true
		}
	}
	return domain.ModelOffer{}, false
}

func watchEvidenceHash(item *watchCandidate) string {
	raw := fmt.Sprintf("pricing=%t@%s|mirror=%t@%s|change=%t@%s:%s", item.sourcePricing,
		item.pricingAt.UTC().Format(time.RFC3339Nano), item.sourceMirror, item.mirrorAt.UTC().Format(time.RFC3339Nano),
		item.sourceChange, item.changeAt.UTC().Format(time.RFC3339Nano), item.semanticHash)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func laterWatchTime(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result
}

func (s *Server) buildConsoleWatchItems(ctx context.Context, suppliers []domain.Supplier,
	snapshots map[int64]domain.CatalogSnapshot, policies []domain.PricingPolicy,
	proposals map[int64]domain.ChangeProposal) ([]consoleWatch, store.WatchSettings, int, error) {
	settings, err := s.store.WatchSettings(ctx)
	if err != nil {
		return nil, store.WatchSettings{}, 0, err
	}
	records, err := s.store.ListWatchRecords(ctx)
	if err != nil {
		return nil, settings, 0, err
	}
	supplierByID := make(map[int64]domain.Supplier, len(suppliers))
	for _, supplier := range suppliers {
		supplierByID[supplier.ID] = supplier
	}
	candidates := make(map[string]*watchCandidate)
	recordByIdentity := make(map[string]*store.WatchRecord, len(records))
	for index := range records {
		record := &records[index]
		supplier, ok := supplierByID[record.SupplierID]
		if !ok {
			continue
		}
		snapshot := snapshots[record.SupplierID]
		groupKey := canonicalWatchGroup(snapshot, record.GroupKey)
		if record.ScopeType == "model" && groupKey == "" {
			if model, found := findWatchModel(snapshot, record.ModelKey); found {
				groupKey = canonicalWatchGroup(snapshot, model.Group)
			}
		}
		key := watchIdentity(record.SupplierID, record.ScopeType, groupKey, record.ModelKey)
		recordByIdentity[key] = record
		if !record.ManualPinned {
			continue
		}
		candidates[key] = &watchCandidate{supplier: supplier, snapshot: snapshot, record: record,
			scopeType: record.ScopeType, groupKey: groupKey, modelKey: record.ModelKey,
			manual: true, manualRank: record.ManualRank}
	}

	automatic, sourceErr := s.store.ListAutomaticWatchSources(ctx, time.Now().UTC().AddDate(0, 0, -settings.RetentionDays))
	if sourceErr != nil {
		return nil, settings, 0, sourceErr
	}
	for _, source := range automatic {
		supplier, ok := supplierByID[source.SupplierID]
		if !ok {
			continue
		}
		snapshot := snapshots[source.SupplierID]
		groupKey := canonicalWatchGroup(snapshot, source.GroupKey)
		key := watchIdentity(source.SupplierID, "group", groupKey, "")
		item := candidates[key]
		if item == nil {
			item = &watchCandidate{supplier: supplier, snapshot: snapshot, scopeType: "group", groupKey: groupKey}
			candidates[key] = item
		}
		item.sourcePricing = item.sourcePricing || source.Pricing
		item.sourceMirror = item.sourceMirror || source.Mirror
		item.sourceChange = item.sourceChange || source.Changed
		item.changeAt = laterWatchTime(item.changeAt, source.ChangedAt)
		item.pricingAt = laterWatchTime(item.pricingAt, source.PricingAt)
		item.mirrorAt = laterWatchTime(item.mirrorAt, source.MirrorAt)
		if source.ChangedAt.Equal(item.changeAt) {
			item.semanticHash = source.SemanticHash
		}
		if record := recordByIdentity[key]; record != nil {
			item.record = record
		}
	}

	type selectedCandidate struct {
		key          string
		candidate    *watchCandidate
		evidenceHash string
	}
	manualCandidates := make([]selectedCandidate, 0)
	automaticCandidatesList := make([]selectedCandidate, 0)
	automaticCandidates := 0
	for key, candidate := range candidates {
		evidenceHash := watchEvidenceHash(candidate)
		if !candidate.manual {
			automaticCandidates++
			if !settings.AutoEnabled {
				continue
			}
			if candidate.record != nil && candidate.record.SuppressedHash == evidenceHash {
				continue
			}
		}
		selected := selectedCandidate{key: key, candidate: candidate, evidenceHash: evidenceHash}
		if candidate.manual {
			manualCandidates = append(manualCandidates, selected)
		} else {
			automaticCandidatesList = append(automaticCandidatesList, selected)
		}
	}
	sort.SliceStable(manualCandidates, func(i, j int) bool {
		left, right := manualCandidates[i].candidate, manualCandidates[j].candidate
		if left.manualRank != right.manualRank {
			return left.manualRank < right.manualRank
		}
		return left.record.ID < right.record.ID
	})
	sort.SliceStable(automaticCandidatesList, func(i, j int) bool {
		left, right := automaticCandidatesList[i].candidate, automaticCandidatesList[j].candidate
		leftUpdated := laterWatchTime(left.changeAt, left.pricingAt, left.mirrorAt, left.snapshot.Evidence.ObservedAt)
		rightUpdated := laterWatchTime(right.changeAt, right.pricingAt, right.mirrorAt, right.snapshot.Evidence.ObservedAt)
		if !leftUpdated.Equal(rightUpdated) {
			return leftUpdated.After(rightUpdated)
		}
		if left.supplier.Name != right.supplier.Name {
			return left.supplier.Name < right.supplier.Name
		}
		return left.groupKey < right.groupKey
	})
	if len(automaticCandidatesList) > settings.AutomaticLimit {
		automaticCandidatesList = automaticCandidatesList[:settings.AutomaticLimit]
	}
	selected := append(manualCandidates, automaticCandidatesList...)
	baselineKeys := make([]store.WatchGroupRatioKey, 0, len(selected))
	for _, item := range selected {
		if item.candidate.scopeType == "group" {
			baselineKeys = append(baselineKeys, store.WatchGroupRatioKey{
				SupplierID: item.candidate.supplier.ID,
				GroupKey:   item.candidate.groupKey,
			})
		}
	}
	baseline, err := s.store.ListWatchGroupRatiosAtOrBefore(ctx, time.Now().UTC().Add(-24*time.Hour), baselineKeys)
	if err != nil {
		return nil, settings, 0, err
	}
	baselineByGroup := make(map[string]string, len(baseline))
	for _, sample := range baseline {
		groupKey := canonicalWatchGroup(snapshots[sample.SupplierID], sample.GroupKey)
		baselineByGroup[watchIdentity(sample.SupplierID, "group", groupKey, "")] = sample.Ratio
	}
	items := make([]consoleWatch, 0, len(selected))
	for _, item := range selected {
		items = append(items, s.projectConsoleWatch(item.candidate, policies, proposals,
			baselineByGroup[item.key], item.evidenceHash))
	}
	return items, settings, automaticCandidates, nil
}

func (s *Server) projectConsoleWatch(candidate *watchCandidate, policies []domain.PricingPolicy,
	proposals map[int64]domain.ChangeProposal, baselineRatio, evidenceHash string) consoleWatch {
	view := consoleWatch{SupplierID: candidate.supplier.ID, Supplier: candidate.supplier.Name,
		ScopeType: candidate.scopeType, GroupKey: candidate.groupKey, ModelKey: candidate.modelKey,
		Manual: candidate.manual, SourcePricing: candidate.sourcePricing, SourceMirror: candidate.sourceMirror,
		SourceChange: candidate.sourceChange, EvidenceHash: evidenceHash, Status: mapSupplierStatus(candidate.supplier.LastStatus)}
	if candidate.record != nil {
		view.Note = candidate.record.Note
	}
	updated := laterWatchTime(candidate.changeAt, candidate.pricingAt, candidate.mirrorAt, candidate.snapshot.Evidence.ObservedAt)
	if candidate.record != nil {
		updated = laterWatchTime(updated, candidate.record.UpdatedAt)
	}
	view.UpdatedAt = updated.UTC().Format(time.RFC3339)
	if candidate.manual && candidate.record != nil {
		view.ID = fmt.Sprintf("manual:%d", candidate.record.ID)
	} else {
		view.ID = fmt.Sprintf("auto:%d:%s:%s", candidate.supplier.ID, candidate.scopeType, candidate.groupKey)
	}
	if candidate.scopeType == "model" {
		model, found := findWatchModel(candidate.snapshot, candidate.modelKey)
		view.Group = candidate.groupKey
		view.Model = candidate.modelKey
		view.DisplayName = candidate.modelKey
		if found {
			view.Group = model.Group
			view.Model = model.Name
			view.DisplayName = model.Name
			view.UpstreamRate = optionalFloatValue(model.ModelRatio)
		} else {
			view.Status = "warning"
		}
		if candidate.record != nil && strings.TrimSpace(candidate.record.DisplayName) != "" {
			view.DisplayName = candidate.record.DisplayName
		}
		return view
	}
	group, found := findWatchGroup(candidate.snapshot, candidate.groupKey)
	view.Group = candidate.groupKey
	view.Model = "等待可信目录"
	view.DisplayName = candidate.groupKey
	if found {
		view.Group = group.Name
		view.Model = fmt.Sprintf("%d 个模型", len(group.ModelIDs))
		view.DisplayName = group.Name
		currentRatio := ratios.DecisionRatio(group)
		view.UpstreamRate = optionalFloatValue(currentRatio)
		current, currentErr := decimal.NewFromString(currentRatio)
		baseline, baselineErr := decimal.NewFromString(strings.TrimSpace(baselineRatio))
		if currentErr == nil && baselineErr == nil && !baseline.IsZero() {
			value, _ := current.Sub(baseline).Div(baseline).Mul(decimal.NewFromInt(100)).Float64()
			view.Delta24H = &value
		}
	} else if candidate.sourceChange {
		view.Status = "warning"
	}
	if candidate.record != nil && strings.TrimSpace(candidate.record.DisplayName) != "" {
		view.DisplayName = candidate.record.DisplayName
	}
	for _, policy := range policies {
		if !policy.Enabled || policy.SupplierID != candidate.supplier.ID || canonicalWatchGroup(candidate.snapshot, policy.SourceGroup) != candidate.groupKey {
			continue
		}
		if proposal, ok := proposals[policy.ID]; ok {
			view.LocalRate = optionalFloatValue(proposal.ProposedValue)
			view.GrossMargin = ratioMarginPercent(proposal.Calculation.SourceRatio, proposal.Calculation.TargetRatio)
		}
	}
	return view
}

func (s *Server) loadWatchlistView(ctx context.Context) ([]consoleWatch, store.WatchSettings, int, error) {
	suppliers, err := s.store.ListSuppliers(ctx)
	if err != nil {
		return nil, store.WatchSettings{}, 0, err
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return nil, store.WatchSettings{}, 0, err
	}
	proposals, err := s.store.ListProposals(ctx, "", 200)
	if err != nil {
		return nil, store.WatchSettings{}, 0, err
	}
	snapshots := make(map[int64]domain.CatalogSnapshot, len(suppliers))
	for _, supplier := range suppliers {
		if snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID); snapshotErr == nil {
			snapshots[supplier.ID] = snapshot
		}
	}
	views := buildPolicyProposalViews(proposals, policies, snapshots)
	fresh := make(map[int64]domain.ChangeProposal, len(views))
	for policyID, view := range views {
		if view.HasFresh {
			fresh[policyID] = view.Fresh
		}
	}
	return s.buildConsoleWatchItems(ctx, suppliers, snapshots, policies, fresh)
}

func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	items, settings, candidates, err := s.loadWatchlistView(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, watchlistResponse{Items: items, Settings: settings, AutomaticCandidates: candidates})
}

func (s *Server) handleCreateWatchItem(w http.ResponseWriter, r *http.Request) {
	var input watchItemInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item := store.WatchRecord{SupplierID: input.SupplierID, ScopeType: input.ScopeType, GroupKey: input.GroupKey,
		ModelKey: input.ModelKey, DisplayName: input.DisplayName, Note: input.Note}
	if err := s.store.CreateManualWatch(r.Context(), &item); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func manualWatchID(value string) (int64, error) {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return 0, err
	}
	value = decoded
	if !strings.HasPrefix(value, "manual:") {
		return 0, sql.ErrNoRows
	}
	return strconv.ParseInt(strings.TrimPrefix(value, "manual:"), 10, 64)
}

func (s *Server) handleUpdateWatchItem(w http.ResponseWriter, r *http.Request) {
	id, err := manualWatchID(chiParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "只能修改手动关注项")
		return
	}
	var input watchItemInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item := store.WatchRecord{ID: id, SupplierID: input.SupplierID, ScopeType: input.ScopeType, GroupKey: input.GroupKey,
		ModelKey: input.ModelKey, DisplayName: input.DisplayName, Note: input.Note}
	if err := s.store.UpdateManualWatch(r.Context(), item); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) handleDeleteWatchItem(w http.ResponseWriter, r *http.Request) {
	var input watchItemInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	idValue := chiParam(r, "id")
	if id, err := manualWatchID(idValue); err == nil {
		if err := s.store.DeleteManualWatch(r.Context(), id); err != nil {
			writeError(w, statusCode(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
		return
	}
	if err := s.store.SuppressAutomaticWatch(r.Context(), input.SupplierID, input.ScopeType, input.GroupKey,
		input.ModelKey, input.EvidenceHash); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ignored": true})
}

func (s *Server) handleWatchOrder(w http.ResponseWriter, r *http.Request) {
	var input watchOrderInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.ReorderManualWatches(r.Context(), input.IDs); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(input.IDs)})
}

func (s *Server) handleWatchSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings, err := s.store.WatchSettings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, settings)
		return
	}
	var settings store.WatchSettings
	if err := readJSON(r, &settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if settings.RetentionDays < 1 || settings.RetentionDays > 90 || settings.AutomaticLimit < 1 || settings.AutomaticLimit > 200 || settings.OverviewLimit < 1 || settings.OverviewLimit > 20 {
		writeError(w, http.StatusBadRequest, "自动保留天数、关注上限或总览数量超出允许范围")
		return
	}
	updated, err := s.store.SaveWatchSettings(r.Context(), settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
