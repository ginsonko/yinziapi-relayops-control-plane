package ledgersync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type remotePage struct {
	items      []map[string]json.RawMessage
	hasMore    bool
	total      int
	totalKnown bool
	pageSize   int
}

type paginationLimitError struct {
	path, start, end string
	total, capacity  int
}

func (e *paginationLimitError) Error() string {
	return fmt.Sprintf("GET %s has %d rows in window %s..%s, above safe page capacity %d", e.path, e.total, e.start, e.end, e.capacity)
}

type remoteHTTPError struct {
	status int
	path   string
}

func (e *remoteHTTPError) Error() string {
	return fmt.Sprintf("GET %s returned HTTP %d", e.path, e.status)
}

func (s *Service) syncUsers(ctx context.Context, target domain.TargetSite, config store.TargetLedgerSyncConfig,
	payload domain.CredentialPayload, previous store.TargetLedgerSyncState, attemptedAt time.Time) {
	rows, err := s.fetchPages(ctx, target, payload, config.UserPath, config, nil)
	if err != nil {
		status := classifyRemoteFailure(err)
		_ = s.recordFailure(ctx, config, target.ID, streamUsers, status, err, attemptedAt, 0, 0)
		return
	}
	profiles := make([]store.TargetLedgerUserProfile, 0, len(rows))
	skipped := 0
	cashFactor, cashFactorErr := decimal.NewFromString(strings.TrimSpace(config.QuotaCashFactor))
	cashConversionReady := cashFactorErr == nil && cashFactor.IsPositive() && strings.TrimSpace(config.Currency) != ""
	for _, row := range rows {
		userID := rawIdentifier(row, "id", "user_id")
		if userID == "" {
			skipped++
			continue
		}
		profile := store.TargetLedgerUserProfile{TargetSiteID: target.ID, UserID: userID,
			Username: rawString(row, "username", "name"), Role: normalizeRole(rawString(row, "role", "user_role")),
			UserSegment: rawString(row, "group", "group_name", "user_group"), IsTest: rawBool(row, "is_test", "test_user"),
			IsInternal: rawBool(row, "is_internal", "internal"), Unlimited: rawBool(row, "unlimited_quota", "is_unlimited", "unlimited"),
			Currency: strings.ToUpper(strings.TrimSpace(config.Currency)), QuotaCashFactor: strings.TrimSpace(config.QuotaCashFactor),
			SourceHash: rawRowHash(row), ObservedAt: attemptedAt}
		if quota, ok := rawDecimal(row, "quota", "available_quota", "remaining_quota"); ok && !quota.IsNegative() && !profile.Unlimited {
			profile.AvailableQuota = quota.String()
			if cashConversionReady {
				profile.CashBalance = quota.Mul(cashFactor).String()
			}
		}
		profiles = append(profiles, profile)
	}
	if err := s.store.ReplaceTargetLedgerUserProfiles(ctx, target.ID, profiles); err != nil {
		_ = s.recordFailure(ctx, config, target.ID, streamUsers, "failed", err, attemptedAt, len(rows), skipped)
		return
	}
	cursor := attemptedAt
	state := store.TargetLedgerSyncState{TargetSiteID: target.ID, Stream: streamUsers, CursorTime: &cursor, Status: "healthy",
		LastAttemptAt: &attemptedAt, LastSuccessAt: &attemptedAt, NextAttemptAt: attemptedAt.Add(time.Duration(config.IntervalSeconds) * time.Second),
		LastScannedRows: len(rows), LastImportedEvents: len(profiles), LastSkippedRows: skipped}
	_ = s.store.RecordTargetLedgerSyncSuccess(ctx, state)
}

func (s *Service) syncEvents(ctx context.Context, target domain.TargetSite, config store.TargetLedgerSyncConfig,
	payload domain.CredentialPayload, previous store.TargetLedgerSyncState, stream string, attemptedAt time.Time) {
	start := attemptedAt.Add(-time.Duration(config.InitialLookbackSeconds) * time.Second)
	if previous.CursorTime != nil {
		start = previous.CursorTime.Add(-time.Duration(config.OverlapSeconds) * time.Second)
	}
	path := config.LogPath
	if stream == streamTopups {
		path = config.TopupPath
	}
	var rows []map[string]json.RawMessage
	var err error
	if stream == streamLogs {
		rows, err = s.fetchLogWindows(ctx, target, payload, path, config, start, attemptedAt)
	} else {
		rows, err = s.fetchPages(ctx, target, payload, path, config, &start)
	}
	if err != nil {
		_ = s.recordFailure(ctx, config, target.ID, stream, classifyRemoteFailure(err), err, attemptedAt, 0, 0)
		return
	}
	events := make([]store.OperatingLedgerEvent, 0, len(rows))
	var costResolver *costResolver
	var resolverFailure *costResolutionFailure
	if stream == streamLogs {
		if s.targetFactory == nil {
			resolverFailure = newCostFailure("target_cost_adapter_missing", "target channel read adapter is not configured", nil)
		} else {
			var resolverErr error
			costResolver, resolverErr = s.buildCostResolver(ctx, target, config, attemptedAt)
			if resolverErr != nil {
				resolverFailure = newCostFailure("cost_resolver_unavailable", resolverErr.Error(), nil)
			}
		}
	}
	skipped := 0
	maxTime := time.Time{}
	maxID := ""
	if previous.CursorTime != nil {
		maxTime, maxID = *previous.CursorTime, previous.CursorID
	}
	for _, row := range rows {
		var occurredAt time.Time
		var remoteID string
		var ok bool
		if stream == streamLogs {
			occurredAt, ok = rawTimestamp(row, "created_at", "created_time", "timestamp", "time")
			remoteID = rawIdentifier(row, "request_id", "upstream_request_id", "id", "log_id")
		} else {
			// NewAPI TopUp uses complete_time/create_time. Prefer completion so the
			// cash event is recognized when payment actually settled.
			occurredAt, ok = rawTimestamp(row, "complete_time", "completed_at", "paid_at", "create_time", "created_at", "created_time", "timestamp", "time")
			remoteID = rawIdentifier(row, "trade_no", "id", "topup_id", "order_id")
		}
		if ok && (occurredAt.After(maxTime) || occurredAt.Equal(maxTime) && remoteID > maxID) && !occurredAt.After(attemptedAt.Add(5*time.Minute)) {
			maxTime, maxID = occurredAt, remoteID
		}
		if !ok || occurredAt.Before(start) || occurredAt.After(attemptedAt.Add(5*time.Minute)) {
			skipped++
			continue
		}
		var event store.OperatingLedgerEvent
		var eventOK bool
		if stream == streamLogs {
			event, eventOK = logEvent(target.ID, config, row, occurredAt, remoteID)
		} else {
			event, eventOK = topupEvent(target.ID, config, row, occurredAt, remoteID)
		}
		if !eventOK {
			skipped++
			continue
		}
		if derived, zeroCost := explicitUnroutedZeroCost(event, config.Currency); zeroCost {
			event.ProcurementCost, event.Currency, event.CostEvidence = derived.amount, derived.currency, derived.evidence
			for key, value := range derived.metadata {
				event.Metadata[key] = value
			}
		} else if costResolver != nil {
			if derived, failure := costResolver.derive(event, rawRowStrings(row)); failure == nil {
				event.ProcurementCost, event.Currency, event.CostEvidence = derived.amount, derived.currency, derived.evidence
				for key, value := range derived.metadata {
					if event.Metadata == nil {
						event.Metadata = map[string]string{}
					}
					event.Metadata[key] = value
				}
			}
		}
		events = append(events, event)
	}
	inserted, err := s.store.UpsertOperatingLedgerEventsCount(ctx, events)
	if err != nil {
		_ = s.recordFailure(ctx, config, target.ID, stream, "failed", err, attemptedAt, len(rows), skipped)
		return
	}
	// Existing immutable request rows may predate the resolver. Enrich only
	// those still missing cost; Upsert above remains the authoritative insert.
	if stream == streamLogs {
		for _, event := range events {
			if costResolver == nil || event.ProcurementCost == "" {
				continue
			}
			_, _ = s.store.FillMissingUsageCost(ctx, event.EventID, event.ProcurementCost, event.Currency, event.CostEvidence, event.Metadata)
		}
		if err := s.backfillMissingUsageCosts(ctx, target, config, costResolver, resolverFailure, attemptedAt); err != nil {
			_ = s.recordFailure(ctx, config, target.ID, streamLogs, "failed", err, attemptedAt, len(rows), skipped)
			return
		}
	}
	if maxTime.IsZero() {
		maxTime = attemptedAt
	}
	state := store.TargetLedgerSyncState{TargetSiteID: target.ID, Stream: stream, CursorTime: &maxTime, CursorID: maxID,
		Status: "healthy", LastAttemptAt: &attemptedAt, LastSuccessAt: &attemptedAt,
		NextAttemptAt: attemptedAt.Add(time.Duration(config.IntervalSeconds) * time.Second), LastScannedRows: len(rows),
		LastImportedEvents: inserted, LastSkippedRows: skipped}
	_ = s.store.RecordTargetLedgerSyncSuccess(ctx, state)
}

func (s *Service) backfillMissingUsageCosts(ctx context.Context, target domain.TargetSite, config store.TargetLedgerSyncConfig,
	resolver *costResolver, resolverFailure *costResolutionFailure, attemptedAt time.Time) error {
	limit := config.PageSize * config.MaxPages
	if limit <= 0 || limit > 50000 {
		limit = 5000
	}
	start := attemptedAt.Add(-time.Duration(config.InitialLookbackSeconds) * time.Second)
	events, err := s.store.ListUsageEventsMissingCost(ctx, target.ID, start, attemptedAt.Add(time.Second), limit)
	if err != nil {
		return fmt.Errorf("list usage events missing procurement cost: %w", err)
	}
	diagnostics := newCostResolutionAccumulator()
	resolved := 0
	for _, event := range events {
		if derived, zeroCost := explicitUnroutedZeroCost(event, config.Currency); zeroCost {
			if _, err := s.store.FillMissingUsageCost(ctx, event.EventID, derived.amount, derived.currency, derived.evidence, derived.metadata); err != nil {
				diagnostics.add(newCostFailure("cost_persistence_failed", err.Error(), nil), event)
				continue
			}
			resolved++
			continue
		}
		if resolverFailure != nil {
			diagnostics.add(resolverFailure, event)
			continue
		}
		if resolver == nil {
			diagnostics.add(newCostFailure("cost_resolver_unavailable", "procurement cost resolver is unavailable", nil), event)
			continue
		}
		derived, failure := resolver.derive(event, event.Metadata)
		if failure != nil {
			diagnostics.add(failure, event)
			continue
		}
		if _, err := s.store.FillMissingUsageCost(ctx, event.EventID, derived.amount, derived.currency, derived.evidence, derived.metadata); err != nil {
			diagnostics.add(newCostFailure("cost_persistence_failed", err.Error(), map[string]string{
				"target_group": event.GroupName, "model": event.ModelID, "channel_id": event.Metadata["channel_id"],
			}), event)
			continue
		}
		resolved++
	}
	unresolved := len(events) - resolved
	status := store.TargetLedgerCostResolutionStatus{TargetSiteID: target.ID, WindowStart: start, WindowEnd: attemptedAt.Add(time.Second),
		ScannedEvents: len(events), ResolvedEvents: resolved, UnresolvedEvents: unresolved, Truncated: len(events) == limit,
		Reasons: diagnostics.list(), ObservedAt: attemptedAt}
	if err := s.store.ReplaceTargetLedgerCostResolutionStatus(ctx, status); err != nil {
		return fmt.Errorf("persist procurement cost resolution diagnostics: %w", err)
	}
	return nil
}

func (s *Service) fetchPages(ctx context.Context, target domain.TargetSite, payload domain.CredentialPayload, path string,
	config store.TargetLedgerSyncConfig, start *time.Time) ([]map[string]json.RawMessage, error) {
	return s.fetchPagesInWindow(ctx, target, payload, path, config, start, nil)
}

func (s *Service) fetchPagesInWindow(ctx context.Context, target domain.TargetSite, payload domain.CredentialPayload, path string,
	config store.TargetLedgerSyncConfig, start, end *time.Time) ([]map[string]json.RawMessage, error) {
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	client, err := netguard.SameOriginClient(s.client, base)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]json.RawMessage, 0, config.PageSize)
	seenPages := map[string]struct{}{}
	for pageNumber := 1; pageNumber <= config.MaxPages; pageNumber++ {
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, err
		}
		query := parsed.Query()
		// NewAPI's common.PageInfo is one-based and aliases p=0 to p=1.
		// Starting at zero therefore repeats the first page once the result grows
		// beyond page_size and prevents a complete ledger sync.
		query.Set("p", strconv.Itoa(pageNumber))
		query.Set("page_size", strconv.Itoa(config.PageSize))
		if start != nil {
			query.Set("start_timestamp", strconv.FormatInt(start.UTC().Unix(), 10))
		}
		if end != nil {
			query.Set("end_timestamp", strconv.FormatInt(end.UTC().Unix(), 10))
		}
		parsed.RawQuery = query.Encode()
		requestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet, base+parsed.String(), nil)
		if requestErr != nil {
			cancel()
			return nil, requestErr
		}
		request.Header.Set("Accept", "application/json")
		if err := authorize(request, payload); err != nil {
			cancel()
			return nil, err
		}
		response, requestErr := client.Do(request)
		if requestErr != nil {
			cancel()
			return nil, requestErr
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 12<<20))
		response.Body.Close()
		cancel()
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, &remoteHTTPError{status: response.StatusCode, path: parsed.Path}
		}
		page, decodeErr := decodeRemotePage(body, config.PageSize)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode GET %s page %d: %w", parsed.Path, pageNumber, decodeErr)
		}
		hash := sha256.Sum256(body)
		pageHash := hex.EncodeToString(hash[:])
		if _, duplicate := seenPages[pageHash]; duplicate && len(page.items) > 0 {
			return nil, fmt.Errorf("GET %s ignored pagination and repeated a non-empty page", parsed.Path)
		}
		seenPages[pageHash] = struct{}{}
		pageSize := page.pageSize
		if pageSize <= 0 {
			pageSize = config.PageSize
		}
		capacity := config.MaxPages * pageSize
		if page.totalKnown && page.total > capacity {
			startLabel, endLabel := "unbounded", "unbounded"
			if start != nil {
				startLabel = start.UTC().Format(time.RFC3339)
			}
			if end != nil {
				endLabel = end.UTC().Format(time.RFC3339)
			}
			return nil, &paginationLimitError{path: parsed.Path, start: startLabel, end: endLabel, total: page.total, capacity: capacity}
		}
		result = append(result, page.items...)
		if !page.hasMore {
			return result, nil
		}
	}
	return nil, fmt.Errorf("GET %s exceeded max_pages=%d before a complete pagination boundary", path, config.MaxPages)
}

func (s *Service) fetchLogWindows(ctx context.Context, target domain.TargetSite, payload domain.CredentialPayload, path string,
	config store.TargetLedgerSyncConfig, start, end time.Time) ([]map[string]json.RawMessage, error) {
	const initialWindow = 6 * time.Hour
	if !start.Before(end) {
		return []map[string]json.RawMessage{}, nil
	}
	rows := make([]map[string]json.RawMessage, 0, config.PageSize)
	seen := make(map[string]struct{})
	for lower := start; lower.Before(end); {
		upper := lower.Add(initialWindow)
		if upper.After(end) {
			upper = end
		}
		chunk, err := s.fetchLogWindow(ctx, target, payload, path, config, lower, upper, 0)
		if err != nil {
			return nil, err
		}
		for _, row := range chunk {
			key := rawIdentifier(row, "request_id", "upstream_request_id", "id", "log_id")
			if key == "" {
				key = rawRowHash(row)
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			rows = append(rows, row)
		}
		lower = upper
	}
	return rows, nil
}

func (s *Service) fetchLogWindow(ctx context.Context, target domain.TargetSite, payload domain.CredentialPayload, path string,
	config store.TargetLedgerSyncConfig, start, end time.Time, depth int) ([]map[string]json.RawMessage, error) {
	rows, err := s.fetchPagesInWindow(ctx, target, payload, path, config, &start, &end)
	var limit *paginationLimitError
	if !errors.As(err, &limit) {
		return rows, err
	}
	if depth >= 20 || end.Sub(start) <= time.Minute {
		return nil, fmt.Errorf("log density remains above the configured page capacity at the one-minute boundary: %w", err)
	}
	middle := start.Add(end.Sub(start) / 2).Truncate(time.Second)
	if !middle.After(start) || !middle.Before(end) {
		return nil, err
	}
	left, leftErr := s.fetchLogWindow(ctx, target, payload, path, config, start, middle, depth+1)
	if leftErr != nil {
		return nil, leftErr
	}
	right, rightErr := s.fetchLogWindow(ctx, target, payload, path, config, middle, end, depth+1)
	if rightErr != nil {
		return nil, rightErr
	}
	return append(left, right...), nil
}

func authorize(request *http.Request, payload domain.CredentialPayload) error {
	if payload.Authorization != "" {
		request.Header.Set("Authorization", payload.Authorization)
	}
	if payload.Cookie != "" {
		request.Header.Set("Cookie", payload.Cookie)
	}
	if payload.UserID > 0 {
		request.Header.Set("New-Api-User", strconv.FormatInt(payload.UserID, 10))
	}
	for key, value := range payload.Headers {
		if strings.TrimSpace(key) != "" {
			request.Header.Set(key, value)
		}
	}
	if request.Header.Get("Authorization") == "" && request.Header.Get("Cookie") == "" {
		return errors.New("management credential contains neither authorization nor session cookie")
	}
	return nil
}

func (s *Service) withDiscoveredUnitContract(ctx context.Context, target domain.TargetSite, config store.TargetLedgerSyncConfig,
	attemptedAt time.Time) store.TargetLedgerSyncConfig {
	unit, err := s.discoverUnitContract(ctx, target, attemptedAt)
	if err == nil {
		s.unitMu.Lock()
		s.units[target.ID] = unit
		s.unitMu.Unlock()
		config.Currency, config.QuotaCashFactor = unit.Currency, unit.Factor
		config.UnitEvidenceSource, config.UnitEvidenceObservedAt = unit.Source, &unit.ObservedAt
		return config
	}

	s.unitMu.Lock()
	previous, hasPrevious := s.units[target.ID]
	if hasPrevious && previous.Currency != "" && previous.Factor != "" {
		previous.Warning = "自动单位发现本轮失败，继续使用最近可信参数：" + err.Error()
		s.units[target.ID] = previous
		config.Currency, config.QuotaCashFactor = previous.Currency, previous.Factor
		config.UnitEvidenceSource, config.UnitEvidenceObservedAt = previous.Source, &previous.ObservedAt
	} else {
		s.units[target.ID] = effectiveUnitContract{Source: "/api/status", Warning: "自动单位发现失败，现金字段保持未知：" + err.Error()}
	}
	s.unitMu.Unlock()
	return config
}

func (s *Service) discoverUnitContract(ctx context.Context, target domain.TargetSite, attemptedAt time.Time) (effectiveUnitContract, error) {
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	client, err := netguard.SameOriginClient(s.client, base)
	if err != nil {
		return effectiveUnitContract{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, base+"/api/status", nil)
	if err != nil {
		return effectiveUnitContract{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return effectiveUnitContract{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return effectiveUnitContract{}, err
	}
	if response.StatusCode != http.StatusOK {
		return effectiveUnitContract{}, fmt.Errorf("GET /api/status returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Success bool                       `json:"success"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.Success || envelope.Data == nil {
		return effectiveUnitContract{}, errors.New("GET /api/status did not return the NewAPI status contract")
	}
	quotaPerUnit, ok := rawDecimal(envelope.Data, "quota_per_unit", "quotaPerUnit")
	if !ok || !quotaPerUnit.IsPositive() {
		return effectiveUnitContract{}, errors.New("GET /api/status did not provide a positive quota_per_unit")
	}
	displayType := strings.ToUpper(strings.TrimSpace(rawString(envelope.Data, "quota_display_type", "quotaDisplayType",
		"currency", "quota_currency", "billing_currency", "display_currency")))
	currency := normalizeCurrency(displayType)
	rate := decimal.NewFromInt(1)
	switch displayType {
	case "TOKENS":
		return effectiveUnitContract{}, errors.New("target displays quota as tokens rather than monetary units")
	case "CNY", "RMB", "¥", "￥":
		currency = "CNY"
		value, rateOK := rawDecimal(envelope.Data, "usd_exchange_rate", "usdExchangeRate")
		if !rateOK || !value.IsPositive() {
			return effectiveUnitContract{}, errors.New("CNY quota display requires a positive usd_exchange_rate")
		}
		rate = value
	case "USD", "$", "US$":
		currency = "USD"
	case "CUSTOM":
		return effectiveUnitContract{}, errors.New("custom quota display has no auditable three-letter currency code")
	default:
		if currency == "" {
			return effectiveUnitContract{}, fmt.Errorf("quota display type %q is not a recognized monetary unit", displayType)
		}
		if currency != "USD" {
			value, rateOK := rawDecimal(envelope.Data, "custom_currency_exchange_rate", "currency_exchange_rate")
			if !rateOK || !value.IsPositive() {
				return effectiveUnitContract{}, fmt.Errorf("currency %s requires a positive exchange rate", currency)
			}
			rate = value
		}
	}
	factor := rate.Div(quotaPerUnit)
	if !factor.IsPositive() {
		return effectiveUnitContract{}, errors.New("derived quota cash factor is not positive")
	}
	return effectiveUnitContract{Currency: currency, Factor: factor.String(), Source: "/api/status",
		ObservedAt: attemptedAt.UTC()}, nil
}

func decodeRemotePage(body []byte, requestedPageSize int) (remotePage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return remotePage{}, err
	}
	if value, exists := root["success"]; exists {
		var success bool
		if json.Unmarshal(value, &success) == nil && !success {
			return remotePage{}, errors.New("remote envelope reported success=false")
		}
	}
	if value, exists := root["code"]; exists {
		code := strings.TrimSpace(rawScalar(value))
		if code != "" && code != "0" && code != "200" {
			return remotePage{}, fmt.Errorf("remote envelope reported code=%s", code)
		}
	}
	payload := body
	if data, exists := root["data"]; exists && len(data) > 0 && string(data) != "null" {
		payload = data
	}
	items, metadata, err := extractItems(payload)
	if err != nil {
		return remotePage{}, err
	}
	total, totalOK := rawInt(metadata, "total", "total_count", "count")
	pageSize, pageSizeOK := rawInt(metadata, "page_size", "size", "per_page")
	hasMore, explicit := rawOptionalBool(metadata, "has_more", "hasMore", "more")
	if !explicit {
		page, pageOK := rawInt(metadata, "page", "p", "current_page")
		if totalOK && pageOK {
			if !pageSizeOK || pageSize <= 0 {
				pageSize = requestedPageSize
			}
			hasMore = (page+1)*pageSize < total || page > 0 && page*pageSize < total
		} else {
			hasMore = len(items) >= requestedPageSize
		}
	}
	return remotePage{items: items, hasMore: hasMore, total: total, totalKnown: totalOK, pageSize: pageSize}, nil
}

func extractItems(payload []byte) ([]map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &list); err == nil {
		return list, map[string]json.RawMessage{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, nil, errors.New("response data is neither an array nor an object")
	}
	for _, key := range []string{"items", "records", "list", "data"} {
		if raw, exists := object[key]; exists {
			if err := json.Unmarshal(raw, &list); err == nil {
				return list, object, nil
			}
		}
	}
	if rawIdentifier(object, "id", "log_id", "topup_id", "user_id") != "" {
		return []map[string]json.RawMessage{object}, object, nil
	}
	return []map[string]json.RawMessage{}, object, nil
}

func logEvent(targetID int64, config store.TargetLedgerSyncConfig, row map[string]json.RawMessage,
	occurredAt time.Time, remoteID string) (store.OperatingLedgerEvent, bool) {
	rowHash := rawRowHash(row)
	if remoteID == "" {
		remoteID = rowHash
	}
	event := baseRemoteEvent(targetID, "log", remoteID, occurredAt, row, rowHash)
	event.EventKind = "usage"
	event.ModelID = rawString(row, "model_name", "model")
	event.GroupName = rawString(row, "group", "group_name")
	event.Requests = 1
	if quota, ok := rawDecimal(row, "quota", "used_quota"); ok && !quota.IsNegative() {
		event.UsageWeight, event.UsageUnit, event.UsageEvidence = quota.String(), "quota", "observed"
		if config.Currency != "" && config.QuotaCashFactor != "" {
			factor, factorErr := decimal.NewFromString(config.QuotaCashFactor)
			if factorErr == nil && factor.IsPositive() {
				event.CashRevenue = quota.Mul(factor).String()
				event.Currency, event.RevenueEvidence = config.Currency, "observed"
				event.Metadata["quota_cash_factor"] = config.QuotaCashFactor
				event.Metadata["quota_cash_currency"] = config.Currency
				if config.UnitEvidenceSource != "" {
					event.Metadata["unit_evidence_source"] = config.UnitEvidenceSource
				}
				if config.UnitEvidenceObservedAt != nil {
					event.Metadata["unit_evidence_observed_at"] = config.UnitEvidenceObservedAt.UTC().Format(time.RFC3339Nano)
				}
			}
		}
	}
	return event, true
}

func topupEvent(targetID int64, config store.TargetLedgerSyncConfig, row map[string]json.RawMessage,
	occurredAt time.Time, remoteID string) (store.OperatingLedgerEvent, bool) {
	if !explicitSuccessfulTopup(row) {
		return store.OperatingLedgerEvent{}, false
	}
	rowHash := rawRowHash(row)
	if remoteID == "" {
		remoteID = rowHash
	}
	event := baseRemoteEvent(targetID, "topup", remoteID, occurredAt, row, rowHash)
	event.EventKind = "recharge"
	amount, amountOK := rawDecimal(row, "money", "cash_amount", "paid_amount", "payment_amount")
	currency := normalizeCurrency(rawString(row, "currency", "money_currency", "payment_currency"))
	if currency == "" {
		currency = config.Currency
	}
	if amountOK && !amount.IsNegative() && currency != "" {
		event.Amount, event.AmountEvidence, event.Currency = amount.String(), "exact", currency
		if normalizeCurrency(rawString(row, "currency", "money_currency", "payment_currency")) == "" && config.UnitEvidenceSource != "" {
			event.Metadata["unit_evidence_source"] = config.UnitEvidenceSource
			if config.UnitEvidenceObservedAt != nil {
				event.Metadata["unit_evidence_observed_at"] = config.UnitEvidenceObservedAt.UTC().Format(time.RFC3339Nano)
			}
		}
	} else {
		event.AmountEvidence = "missing"
		if amountOK {
			event.Metadata["raw_money"] = amount.String()
		}
	}
	return event, true
}

func baseRemoteEvent(targetID int64, stream, remoteID string, occurredAt time.Time, row map[string]json.RawMessage,
	rowHash string) store.OperatingLedgerEvent {
	event := store.OperatingLedgerEvent{EventID: fmt.Sprintf("newapi:%d:%s:%s", targetID, stream, remoteID), TargetSiteID: targetID,
		OccurredAt: occurredAt.UTC(), AmountEvidence: "missing", UsageEvidence: "missing", RevenueEvidence: "missing", CostEvidence: "missing",
		Source: "newapi_target_" + stream, EvidenceRevision: "newapi-" + stream + "-v1", Metadata: map[string]string{
			"remote_id": remoteID, "source_row_hash": rowHash}}
	event.SubjectUserID = rawIdentifier(row, "user_id", "userid")
	event.SubjectTokenID = rawIdentifier(row, "token_id")
	event.SubjectRole = normalizeRole(rawString(row, "role", "user_role"))
	event.UserSegment = rawString(row, "group", "group_name", "user_group")
	event.IsTest = rawBool(row, "is_test", "test_user")
	event.IsInternal = rawBool(row, "is_internal", "internal")
	for metadataKey, rowKeys := range map[string][]string{"channel_id": {"channel_id", "channel"}, "prompt_tokens": {"prompt_tokens"},
		"completion_tokens": {"completion_tokens"}} {
		if value := rawIdentifier(row, rowKeys...); value != "" {
			event.Metadata[metadataKey] = value
		}
	}
	rowEvidence := rawRowStrings(row)
	for _, key := range newAPILogEvidenceFields {
		if value := strings.TrimSpace(rowEvidence[key]); value != "" {
			event.Metadata[key] = value
		}
	}
	return event
}

var newAPILogEvidenceFields = []string{
	"pricing_scope", "pricing_group", "pricing_model", "billing_mode", "pricing_revision", "task_billing_unit", "task_fixed_duration",
	"seconds", "second", "duration", "duration_seconds", "size", "frames", "frame_count", "image_count",
	"cache_tokens", "cache_creation_tokens", "cache_creation_tokens_5m", "cache_creation_tokens_1h",
	"image_tokens", "image_input_tokens", "audio_tokens", "audio_input_tokens", "audio_output_tokens", "claude",
}

func rawNestedScalars(row map[string]json.RawMessage, key string) map[string]string {
	raw, exists := row[key]
	if !exists || len(raw) == 0 {
		return nil
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		raw = json.RawMessage(encoded)
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for _, field := range newAPILogEvidenceFields {
		if value, ok := values[field]; ok {
			if scalar := strings.TrimSpace(rawScalar(value)); scalar != "" && scalar != "null" {
				result[field] = scalar
			}
		}
	}
	return result
}

func explicitSuccessfulTopup(row map[string]json.RawMessage) bool {
	if success, exists := rawOptionalBool(row, "success", "paid", "completed"); exists {
		return success
	}
	status := strings.ToLower(strings.TrimSpace(rawString(row, "status", "trade_status", "payment_status")))
	switch status {
	case "success", "succeeded", "paid", "completed", "trade_success":
		return true
	default:
		return false
	}
}

func rawString(row map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw, exists := row[key]; exists {
			value := strings.TrimSpace(rawScalar(raw))
			if value != "" && value != "null" {
				return value
			}
		}
	}
	return ""
}

func rawScalar(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	var boolean bool
	if json.Unmarshal(raw, &boolean) == nil {
		return strconv.FormatBool(boolean)
	}
	return ""
}

func rawIdentifier(row map[string]json.RawMessage, keys ...string) string {
	value := rawString(row, keys...)
	if parsed, err := decimal.NewFromString(value); err == nil && parsed.Equal(parsed.Truncate(0)) {
		return parsed.StringFixed(0)
	}
	return value
}

func rawDecimal(row map[string]json.RawMessage, keys ...string) (decimal.Decimal, bool) {
	value := rawString(row, keys...)
	parsed, err := decimal.NewFromString(value)
	return parsed, err == nil
}

func rawBool(row map[string]json.RawMessage, keys ...string) bool {
	value, _ := rawOptionalBool(row, keys...)
	return value
}

func rawOptionalBool(row map[string]json.RawMessage, keys ...string) (bool, bool) {
	for _, key := range keys {
		raw, exists := row[key]
		if !exists {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
		switch strings.ToLower(strings.TrimSpace(rawScalar(raw))) {
		case "1", "true", "yes":
			return true, true
		case "0", "false", "no":
			return false, true
		}
	}
	return false, false
}

func rawInt(row map[string]json.RawMessage, keys ...string) (int, bool) {
	value := rawString(row, keys...)
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

func rawTimestamp(row map[string]json.RawMessage, keys ...string) (time.Time, bool) {
	value := rawString(row, keys...)
	if value == "" {
		return time.Time{}, false
	}
	if numeric, err := strconv.ParseInt(value, 10, 64); err == nil {
		if numeric > 10_000_000_000 {
			numeric /= 1000
		}
		parsed := time.Unix(numeric, 0).UTC()
		return parsed, numeric > 0
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func rawRowHash(row map[string]json.RawMessage) string {
	encoded, _ := json.Marshal(row)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func normalizeRole(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "10", "100", "admin", "administrator", "root", "owner", "site_owner":
		return "administrator"
	case "1", "user", "common_user":
		return "user"
	case "":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 3 {
		return ""
	}
	for _, char := range value {
		if char < 'A' || char > 'Z' {
			return ""
		}
	}
	return value
}

func classifyRemoteFailure(err error) string {
	var remote *remoteHTTPError
	if errors.As(err, &remote) {
		switch remote.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "auth_required"
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return "unsupported"
		}
	}
	return "failed"
}
