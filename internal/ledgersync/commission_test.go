package ledgersync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestCommissionOperatingEventMapsAwardAndReversalWithoutConflatingProcurement(t *testing.T) {
	now := time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC)
	config := store.TargetLedgerSyncConfig{Currency: "CNY", QuotaCashFactor: "0.01"}
	base := commissionCostEvent{SchemaVersion: commissionCostSchema, ID: 7, EventUUID: "event-7",
		BeneficiaryUserID: 20, ConsumerUserID: 30, OperationPublicID: "operation-1",
		OperationSource: "http", SourceStableID: "request-1", CreatedAt: now.Add(-time.Minute).UnixMilli()}

	award := base
	award.EventType, award.AmountQuota = "commission_awarded", 125
	got, ok := commissionOperatingEvent(9, config, award, decimal.RequireFromString("0.01"), now)
	if !ok || got.EventKind != "other_expense" || got.Amount != "1.25" || got.Currency != "CNY" ||
		got.ProcurementCost != "" || got.SubjectUserID != "30" ||
		got.Metadata["beneficiary_user_id"] != "20" || got.Metadata["cost_kind"] != "consumption_commission" {
		t.Fatalf("award event = %#v, ok=%v", got, ok)
	}

	reversal := base
	reversal.ID, reversal.EventUUID = 8, "event-8"
	reversal.EventType, reversal.AmountQuota, reversal.RelatedEventUUID = "commission_award_reversed", -50, "event-7"
	got, ok = commissionOperatingEvent(9, config, reversal, decimal.RequireFromString("0.01"), now)
	if !ok || got.EventKind != "other_income" || got.Amount != "0.5" ||
		got.RelatedEventID != "newapi:9:commission:event-7" || got.Metadata["cost_direction"] != "reversal" {
		t.Fatalf("reversal event = %#v, ok=%v", got, ok)
	}
}

func TestCommissionOperatingEventRejectsWrongSignOrFutureEvidence(t *testing.T) {
	now := time.Now().UTC()
	config := store.TargetLedgerSyncConfig{Currency: "CNY", QuotaCashFactor: "0.01"}
	item := commissionCostEvent{SchemaVersion: commissionCostSchema, ID: 1, EventUUID: "event-1",
		EventType: "commission_awarded", AmountQuota: -1, BeneficiaryUserID: 2, ConsumerUserID: 3,
		CreatedAt: now.UnixMilli()}
	if _, ok := commissionOperatingEvent(1, config, item, decimal.RequireFromString("0.01"), now); ok {
		t.Fatal("negative award was accepted")
	}
	item.EventType, item.AmountQuota, item.CreatedAt = "commission_awarded", 1, now.Add(6*time.Minute).UnixMilli()
	if _, ok := commissionOperatingEvent(1, config, item, decimal.RequireFromString("0.01"), now); ok {
		t.Fatal("future commission evidence was accepted")
	}
}

func TestCommissionSyncPaginatesAdvancesCursorAndReplaysAsNoOp(t *testing.T) {
	now := time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC)
	requestedCursors := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedCursors = append(requestedCursors, r.URL.Query().Get("after_id"))
		items := []commissionCostEvent{}
		switch r.URL.Query().Get("after_id") {
		case "0":
			items = []commissionCostEvent{
				{SchemaVersion: commissionCostSchema, ID: 1, EventUUID: "event-1", EventType: "commission_awarded", AmountQuota: 100, BeneficiaryUserID: 2, ConsumerUserID: 3, CreatedAt: now.Add(-3 * time.Minute).UnixMilli()},
				{SchemaVersion: commissionCostSchema, ID: 2, EventUUID: "event-2", EventType: "commission_awarded", AmountQuota: 50, BeneficiaryUserID: 2, ConsumerUserID: 4, CreatedAt: now.Add(-2 * time.Minute).UnixMilli()},
			}
		case "2":
			items = []commissionCostEvent{
				{SchemaVersion: commissionCostSchema, ID: 3, EventUUID: "event-3", EventType: "commission_award_reversed", AmountQuota: -25, BeneficiaryUserID: 2, ConsumerUserID: 3, RelatedEventUUID: "event-1", CreatedAt: now.Add(-time.Minute).UnixMilli()},
			}
		case "3":
		default:
			t.Fatalf("unexpected commission cursor %q", r.URL.Query().Get("after_id"))
		}
		nextCursor := int64(3)
		if len(items) > 0 {
			nextCursor = items[len(items)-1].ID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": commissionCostPage{
			SchemaVersion: commissionCostSchema, Items: items, NextCursor: nextCursor,
		}})
	}))
	defer server.Close()

	st, target := ledgerSyncTestStore(t, server.URL)
	service := newService(st, staticCredentialLoader{}, server.Client())
	config := DefaultConfig(target.ID)
	config.Currency, config.QuotaCashFactor, config.PageSize, config.MaxPages = "CNY", "0.01", 2, 5
	service.syncCommissionEvents(context.Background(), target, config, domain.CredentialPayload{Authorization: "Bearer x"}, store.TargetLedgerSyncState{}, now)
	state, err := st.GetTargetLedgerSyncState(context.Background(), target.ID, streamCommissions)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "healthy" || state.CursorID != "3" || state.LastScannedRows != 3 || state.LastImportedEvents != 3 {
		t.Fatalf("first commission sync state = %#v", state)
	}
	events, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(events) != 3 {
		t.Fatalf("first commission sync events = %#v, err=%v", events, err)
	}

	service.syncCommissionEvents(context.Background(), target, config, domain.CredentialPayload{Authorization: "Bearer x"}, state, now.Add(time.Minute))
	replayed, err := st.GetTargetLedgerSyncState(context.Background(), target.ID, streamCommissions)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.CursorID != "3" || replayed.LastImportedEvents != 0 || len(requestedCursors) != 3 ||
		requestedCursors[0] != "0" || requestedCursors[1] != "2" || requestedCursors[2] != "3" {
		t.Fatalf("commission replay state=%#v cursors=%v", replayed, requestedCursors)
	}
}
