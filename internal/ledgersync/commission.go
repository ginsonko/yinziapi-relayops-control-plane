package ledgersync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

const (
	commissionCostPath    = "/api/commission/admin/cost-events"
	commissionCostSchema  = "r29-commission-cost-v1"
	commissionMaximumPage = 50
	commissionMaximumSize = 200
)

type commissionCostEvent struct {
	SchemaVersion     string `json:"schema_version"`
	ID                int64  `json:"id"`
	EventUUID         string `json:"event_uuid"`
	EventType         string `json:"event_type"`
	AmountQuota       int64  `json:"amount_quota"`
	BeneficiaryUserID int    `json:"beneficiary_user_id"`
	ConsumerUserID    int    `json:"consumer_user_id"`
	OperationPublicID string `json:"operation_public_id"`
	OperationSource   string `json:"operation_source"`
	SourceStableID    string `json:"source_stable_id"`
	RelatedEventUUID  string `json:"related_event_uuid"`
	CreatedAt         int64  `json:"created_at"`
}

type commissionCostPage struct {
	SchemaVersion string                `json:"schema_version"`
	Items         []commissionCostEvent `json:"items"`
	NextCursor    int64                 `json:"next_cursor"`
}

func (s *Service) syncCommissionEvents(ctx context.Context, target domain.TargetSite,
	config store.TargetLedgerSyncConfig, payload domain.CredentialPayload,
	previous store.TargetLedgerSyncState, attemptedAt time.Time) {
	afterID := int64(0)
	if cursor := strings.TrimSpace(previous.CursorID); cursor != "" {
		parsed, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || parsed < 0 {
			_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed",
				errors.New("commission cursor is invalid"), attemptedAt, 0, 0)
			return
		}
		afterID = parsed
	}
	factor, err := decimal.NewFromString(strings.TrimSpace(config.QuotaCashFactor))
	if err != nil || !factor.IsPositive() || strings.TrimSpace(config.Currency) == "" {
		_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed",
			errors.New("commission cost sync requires the target quota cash factor and currency"), attemptedAt, 0, 0)
		return
	}
	pageSize := config.PageSize
	if pageSize <= 0 || pageSize > commissionMaximumSize {
		pageSize = commissionMaximumSize
	}
	pageLimit := config.MaxPages
	if pageLimit <= 0 || pageLimit > commissionMaximumPage {
		pageLimit = commissionMaximumPage
	}

	events := make([]store.OperatingLedgerEvent, 0, pageSize)
	lastID, maxTime, scanned, skipped := afterID, time.Time{}, 0, 0
	truncated := false
	for pageNumber := 0; pageNumber < pageLimit; pageNumber++ {
		page, fetchErr := s.fetchCommissionCostPage(ctx, target, payload, lastID, pageSize)
		if fetchErr != nil {
			_ = s.recordFailure(ctx, config, target.ID, streamCommissions,
				classifyRemoteFailure(fetchErr), fetchErr, attemptedAt, scanned, skipped)
			return
		}
		if page.SchemaVersion != commissionCostSchema {
			_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed",
				fmt.Errorf("unsupported commission cost schema %q", page.SchemaVersion), attemptedAt, scanned, skipped)
			return
		}
		if len(page.Items) == 0 {
			break
		}
		previousID := lastID
		for _, item := range page.Items {
			scanned++
			if item.ID <= previousID || strings.TrimSpace(item.EventUUID) == "" ||
				item.SchemaVersion != commissionCostSchema {
				_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed",
					errors.New("commission cost page is not strictly ordered or has an invalid schema"),
					attemptedAt, scanned, skipped)
				return
			}
			event, ok := commissionOperatingEvent(target.ID, config, item, factor, attemptedAt)
			if !ok {
				skipped++
			} else {
				events = append(events, event)
				if event.OccurredAt.After(maxTime) {
					maxTime = event.OccurredAt
				}
			}
			previousID = item.ID
		}
		if page.NextCursor != previousID {
			_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed",
				errors.New("commission cost cursor does not match the last event"), attemptedAt, scanned, skipped)
			return
		}
		lastID = previousID
		if len(page.Items) < pageSize {
			break
		}
		truncated = pageNumber == pageLimit-1
	}

	inserted, err := s.store.UpsertOperatingLedgerEventsCount(ctx, events)
	if err != nil {
		_ = s.recordFailure(ctx, config, target.ID, streamCommissions, "failed", err,
			attemptedAt, scanned, skipped)
		return
	}
	if maxTime.IsZero() {
		maxTime = attemptedAt
	}
	nextAttempt := attemptedAt.Add(time.Duration(config.IntervalSeconds) * time.Second)
	if truncated {
		nextAttempt = attemptedAt.Add(30 * time.Second)
	}
	state := store.TargetLedgerSyncState{TargetSiteID: target.ID, Stream: streamCommissions,
		CursorTime: &maxTime, CursorID: strconv.FormatInt(lastID, 10), Status: "healthy",
		LastAttemptAt: &attemptedAt, LastSuccessAt: &attemptedAt, NextAttemptAt: nextAttempt,
		LastScannedRows: scanned, LastImportedEvents: inserted, LastSkippedRows: skipped}
	_ = s.store.RecordTargetLedgerSyncSuccess(ctx, state)
}

func (s *Service) fetchCommissionCostPage(ctx context.Context, target domain.TargetSite,
	payload domain.CredentialPayload, afterID int64, limit int) (commissionCostPage, error) {
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	client, err := netguard.SameOriginClient(s.client, base)
	if err != nil {
		return commissionCostPage{}, err
	}
	parsed, _ := url.Parse(commissionCostPath)
	query := parsed.Query()
	query.Set("after_id", strconv.FormatInt(afterID, 10))
	query.Set("limit", strconv.Itoa(limit))
	parsed.RawQuery = query.Encode()
	requestCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, base+parsed.String(), nil)
	if err != nil {
		return commissionCostPage{}, err
	}
	request.Header.Set("Accept", "application/json")
	if err := authorize(request, payload); err != nil {
		return commissionCostPage{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return commissionCostPage{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return commissionCostPage{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return commissionCostPage{}, &remoteHTTPError{status: response.StatusCode, path: commissionCostPath}
	}
	var envelope struct {
		Success bool               `json:"success"`
		Data    commissionCostPage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.Success {
		return commissionCostPage{}, errors.New("commission cost endpoint did not return the NewAPI envelope")
	}
	return envelope.Data, nil
}

func commissionOperatingEvent(targetID int64, config store.TargetLedgerSyncConfig,
	item commissionCostEvent, factor decimal.Decimal, attemptedAt time.Time) (store.OperatingLedgerEvent, bool) {
	if item.CreatedAt <= 0 || item.CreatedAt > attemptedAt.Add(5*time.Minute).UnixMilli() ||
		item.BeneficiaryUserID <= 0 || item.ConsumerUserID <= 0 ||
		item.AmountQuota == math.MinInt64 {
		return store.OperatingLedgerEvent{}, false
	}
	amount := item.AmountQuota
	kind, costDirection := "other_expense", "award"
	if amount < 0 {
		kind, costDirection, amount = "other_income", "reversal", -amount
	}
	if item.EventType != "commission_awarded" && item.EventType != "commission_award_reversed" ||
		item.EventType == "commission_awarded" && item.AmountQuota < 0 ||
		item.EventType == "commission_award_reversed" && item.AmountQuota > 0 {
		return store.OperatingLedgerEvent{}, false
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return store.OperatingLedgerEvent{}, false
	}
	hash := sha256.Sum256(encoded)
	eventID := fmt.Sprintf("newapi:%d:commission:%s", targetID, item.EventUUID)
	relatedID := ""
	if strings.TrimSpace(item.RelatedEventUUID) != "" {
		relatedID = fmt.Sprintf("newapi:%d:commission:%s", targetID, item.RelatedEventUUID)
	}
	return store.OperatingLedgerEvent{
		EventID: eventID, TargetSiteID: targetID, OccurredAt: time.UnixMilli(item.CreatedAt).UTC(),
		EventKind: kind, Amount: decimal.NewFromInt(amount).Mul(factor).String(), AmountEvidence: "exact",
		SubjectUserID: strconv.Itoa(item.ConsumerUserID), RelatedEventID: relatedID,
		Currency: strings.ToUpper(strings.TrimSpace(config.Currency)), RevenueEvidence: "missing", CostEvidence: "missing",
		Source: "newapi_target_commission", EvidenceRevision: commissionCostSchema,
		PayloadHash: hex.EncodeToString(hash[:]), CreatedAt: attemptedAt,
		Metadata: map[string]string{
			"cost_kind":             "consumption_commission",
			"cost_direction":        costDirection,
			"commission_event_type": item.EventType,
			"commission_event_uuid": item.EventUUID,
			"beneficiary_user_id":   strconv.Itoa(item.BeneficiaryUserID),
			"consumer_user_id":      strconv.Itoa(item.ConsumerUserID),
			"operation_public_id":   item.OperationPublicID,
			"operation_source":      item.OperationSource,
			"source_stable_id":      item.SourceStableID,
			"amount_quota":          strconv.FormatInt(item.AmountQuota, 10),
			"quota_cash_factor":     factor.String(),
			"quota_cash_currency":   strings.ToUpper(strings.TrimSpace(config.Currency)),
			"source_row_hash":       hex.EncodeToString(hash[:]),
		},
	}, true
}
