package ledgersync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type staticCredentialLoader struct {
	credential domain.Credential
	payload    domain.CredentialPayload
}

func (s staticCredentialLoader) Load(context.Context, int64) (domain.Credential, domain.CredentialPayload, error) {
	return s.credential, s.payload, nil
}

func TestNewAPILedgerSyncImportsIncrementallyAndEnrichesExclusionsFromUsers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var failLogs atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management" || r.Header.Get("New-Api-User") != "99" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/":
			fmt.Fprint(w, `{"success":true,"data":{"items":[{"id":1,"username":"owner","role":100,"group":"staff","quota":250}],"total":1,"page":0,"page_size":100}}`)
		case "/api/log/":
			if failLogs.Load() {
				http.Error(w, "temporary", http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `{"success":true,"data":{"items":[
				{"id":101,"request_id":"req-owner","upstream_request_id":"up-owner","created_at":%d,"user_id":1,"model_name":"gpt-5.6-sol","group":"pro","channel":31,"quota":100},
				{"id":102,"upstream_request_id":"up-user","created_at":%d,"user_id":2,"model_name":"gpt-5.6-sol","group":"pro","channel":32,"quota":50}],
				"total":2,"page":0,"page_size":100}}`, now.Add(-time.Minute).Unix(), now.Add(-30*time.Second).Unix())
		case "/api/user/topup":
			fmt.Fprintf(w, `{"success":true,"data":{"items":[
				{"id":201,"trade_no":"trade-201","create_time":%d,"complete_time":%d,"user_id":2,"status":"success","money":10},
				{"id":202,"trade_no":"trade-202","create_time":%d,"complete_time":%d,"user_id":2,"status":"pending","money":99}],
				"total":2,"page":0,"page_size":100}}`, now.Add(-48*time.Hour).Unix(), now.Add(-20*time.Second).Unix(),
				now.Add(-48*time.Hour).Unix(), now.Add(-10*time.Second).Unix())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, target := ledgerSyncTestStore(t, server.URL)
	loader := staticCredentialLoader{credential: domain.Credential{ID: target.CredentialID, SiteURL: server.URL, Status: "active"},
		payload: domain.CredentialPayload{Authorization: "Bearer management", UserID: 99}}
	service := newService(st, loader, server.Client())
	config := DefaultConfig(target.ID)
	config.Currency, config.QuotaCashFactor = "CNY", "0.01"
	if _, err := service.UpdateConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	first, err := service.SyncTarget(context.Background(), target.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	assertStreamsHealthy(t, first.Status.Streams)
	profiles, err := st.ListTargetLedgerUserProfiles(context.Background())
	if err != nil || len(profiles) != 1 || profiles[0].AvailableQuota != "250" || profiles[0].CashBalance != "2.5" ||
		profiles[0].Currency != "CNY" || profiles[0].QuotaCashFactor != "0.01" {
		t.Fatalf("user cash profile=%#v err=%v", profiles, err)
	}

	events, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(events) != 3 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	byID := map[string]store.OperatingLedgerEvent{}
	for _, event := range events {
		byID[event.EventID] = event
	}
	if event := byID[fmt.Sprintf("newapi:%d:log:req-owner", target.ID)]; event.UsageWeight != "100" || event.CashRevenue != "1" ||
		event.Currency != "CNY" || event.RevenueEvidence != "observed" || event.Metadata["channel_id"] != "31" {
		t.Fatalf("converted log = %#v", event)
	}
	if event := byID[fmt.Sprintf("newapi:%d:log:up-user", target.ID)]; event.UsageWeight != "50" || event.Metadata["channel_id"] != "32" {
		t.Fatalf("upstream request id log = %#v", event)
	}
	if event := byID[fmt.Sprintf("newapi:%d:topup:trade-201", target.ID)]; event.EventKind != "recharge" || event.Amount != "10" ||
		!event.OccurredAt.Equal(now.Add(-20*time.Second)) {
		t.Fatalf("topup = %#v", event)
	}

	report, err := st.OperatingFinancialSummary(context.Background(), store.LedgerFinancialReportOptions{Start: now.Add(-time.Hour), End: now.Add(time.Hour),
		Exclusions: store.LedgerExclusionPolicy{ExcludeAdministrators: true}})
	if err != nil {
		t.Fatal(err)
	}
	if report.ExcludedByReason["administrator"] != 1 || report.IncludedEventCount != 2 {
		t.Fatalf("profile-enriched exclusions = %#v", report)
	}
	cny := report.ByCurrency[0]
	if cny.UsageRevenue != "0.500000" || cny.RechargeGross != "10.000000" {
		t.Fatalf("included financials = %#v", cny)
	}

	second, err := service.SyncTarget(context.Background(), target.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range second.Status.Streams {
		if (stream.Stream == streamLogs || stream.Stream == streamTopups) && stream.LastImportedEvents != 0 {
			t.Fatalf("overlap replay inserted duplicates: %#v", stream)
		}
	}
	beforeFailure, _ := st.GetTargetLedgerSyncState(context.Background(), target.ID, streamLogs)
	failLogs.Store(true)
	failed, err := service.SyncTarget(context.Background(), target.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	afterFailure, _ := st.GetTargetLedgerSyncState(context.Background(), target.ID, streamLogs)
	cursorPreserved := beforeFailure.CursorTime != nil && afterFailure.CursorTime != nil &&
		beforeFailure.CursorTime.Equal(*afterFailure.CursorTime) && beforeFailure.CursorID == afterFailure.CursorID
	if afterFailure.Status != "failed" || afterFailure.LastSuccessAt == nil || !cursorPreserved {
		t.Fatalf("failed sync did not preserve LKG: before=%#v after=%#v", beforeFailure, afterFailure)
	}
	for _, stream := range failed.Status.Streams {
		if stream.Stream == streamLogs && (!stream.HasLastKnownGood || stream.NextAction == "") {
			t.Fatalf("log stream status lacks LKG guidance: %#v", stream)
		}
	}
}

func TestNewAPILedgerSyncDoesNotGuessQuotaCashAndUsesLease(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/":
			fmt.Fprint(w, `{"success":true,"data":{"items":[],"total":0}}`)
		case "/api/log/":
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blocked
			fmt.Fprintf(w, `{"success":true,"data":{"items":[{"id":1,"created_at":%d,"quota":500}],"total":1}}`, now.Unix())
		case "/api/user/topup":
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	st, target := ledgerSyncTestStore(t, server.URL)
	service := newService(st, staticCredentialLoader{credential: domain.Credential{SiteURL: server.URL},
		payload: domain.CredentialPayload{Authorization: "Bearer x"}}, server.Client())
	done := make(chan error, 1)
	go func() {
		_, err := service.SyncTarget(context.Background(), target.ID, true)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first sync did not enter log endpoint")
	}
	if _, err := service.SyncTarget(context.Background(), target.ID, true); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("concurrent sync err=%v, want ErrSyncBusy", err)
	}
	close(blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	events, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	if events[0].UsageWeight != "500" || events[0].UsageUnit != "quota" || events[0].CashRevenue != "" || events[0].Currency != "" {
		t.Fatalf("quota was guessed as cash: %#v", events[0])
	}
	topupState, err := st.GetTargetLedgerSyncState(context.Background(), target.ID, streamTopups)
	if err != nil || topupState.Status != "unsupported" {
		t.Fatalf("unsupported topup status=%#v err=%v", topupState, err)
	}
}

func TestNewAPILedgerSyncAllowsTopupCurrencyWithoutQuotaCashConversion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/":
			fmt.Fprint(w, `{"success":true,"data":{"items":[],"total":0}}`)
		case "/api/log/":
			fmt.Fprintf(w, `{"success":true,"data":{"items":[{"id":1,"request_id":"request-1","created_at":%d,"quota":500}],"total":1}}`, now.Add(-time.Minute).Unix())
		case "/api/user/topup":
			fmt.Fprintf(w, `{"success":true,"data":{"items":[{"id":2,"trade_no":"trade-2","complete_time":%d,"status":"success","money":12.5}],"total":1}}`, now.Add(-30*time.Second).Unix())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, target := ledgerSyncTestStore(t, server.URL)
	service := newService(st, staticCredentialLoader{credential: domain.Credential{SiteURL: server.URL},
		payload: domain.CredentialPayload{Authorization: "Bearer x"}}, server.Client())
	config := DefaultConfig(target.ID)
	config.Currency = "CNY"
	if _, err := service.UpdateConfig(context.Background(), config); err != nil {
		t.Fatalf("currency-only config: %v", err)
	}
	if _, err := service.SyncTarget(context.Background(), target.ID, true); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	byKind := map[string]store.OperatingLedgerEvent{}
	for _, event := range events {
		byKind[event.EventKind] = event
	}
	if usage := byKind["usage"]; usage.UsageWeight != "500" || usage.CashRevenue != "" || usage.Currency != "" {
		t.Fatalf("quota was guessed as cash: %#v", usage)
	}
	if topup := byKind["recharge"]; topup.Amount != "12.5" || topup.Currency != "CNY" || topup.AmountEvidence != "exact" {
		t.Fatalf("topup did not use declared currency: %#v", topup)
	}

	invalid := DefaultConfig(target.ID)
	invalid.QuotaCashFactor = "0.01"
	if err := ValidateConfig(invalid); err == nil {
		t.Fatal("quota_cash_factor without currency was accepted")
	}
}

func TestNewAPILedgerSyncAutoDiscoversAuditableCashUnits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":1}}`)
		case "/api/user/":
			fmt.Fprint(w, `{"success":true,"data":{"items":[],"total":0}}`)
		case "/api/log/":
			fmt.Fprintf(w, `{"success":true,"data":{"items":[{"request_id":"auto-unit-log","created_at":%d,"quota":500000}],"total":1}}`, now.Add(-time.Minute).Unix())
		case "/api/user/topup":
			fmt.Fprintf(w, `{"success":true,"data":{"items":[{"trade_no":"auto-unit-topup","complete_time":%d,"status":"success","money":2.5}],"total":1}}`, now.Add(-30*time.Second).Unix())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	st, target := ledgerSyncTestStore(t, server.URL)
	service := newService(st, staticCredentialLoader{credential: domain.Credential{SiteURL: server.URL},
		payload: domain.CredentialPayload{Authorization: "Bearer x"}}, server.Client())
	result, err := service.SyncTarget(context.Background(), target.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.UnitMode != "auto" || result.Status.EffectiveCurrency != "CNY" ||
		result.Status.EffectiveQuotaCashFactor != "0.000002" || result.Status.UnitSource != "/api/status" {
		t.Fatalf("unit status = %#v", result.Status)
	}
	events, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	byKind := map[string]store.OperatingLedgerEvent{}
	for _, event := range events {
		byKind[event.EventKind] = event
	}
	if usage := byKind["usage"]; usage.CashRevenue != "1" || usage.Currency != "CNY" {
		t.Fatalf("auto converted usage = %#v", usage)
	}
	if topup := byKind["recharge"]; topup.Amount != "2.5" || topup.Currency != "CNY" {
		t.Fatalf("auto converted topup = %#v", topup)
	}
	second, err := service.SyncTarget(context.Background(), target.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	assertStreamsHealthy(t, second.Status.Streams)
	for _, stream := range second.Status.Streams {
		if (stream.Stream == streamLogs || stream.Stream == streamTopups) && stream.LastImportedEvents != 0 {
			t.Fatalf("auto-unit overlap replay inserted duplicates: %#v", stream)
		}
	}
	replayed, err := st.ListOperatingLedgerEvents(context.Background(), target.ID, "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(replayed) != 2 {
		t.Fatalf("auto-unit replay events=%#v err=%v", replayed, err)
	}
}

func TestFetchPagesUsesNewAPIOneBasedPagination(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("p"))
		page := r.URL.Query().Get("p")
		items := make([]map[string]any, 0, 100)
		if page == "1" {
			for id := 1; id <= 100; id++ {
				items = append(items, map[string]any{"id": id})
			}
		} else if page == "2" {
			items = append(items, map[string]any{"id": 101})
		} else {
			t.Fatalf("unexpected page %q", page)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"items": items, "total": 101, "page": page, "page_size": 100,
		}})
	}))
	defer server.Close()

	service := newService(nil, staticCredentialLoader{}, server.Client())
	config := DefaultConfig(1)
	rows, err := service.fetchPages(context.Background(), domain.TargetSite{BaseURL: server.URL},
		domain.CredentialPayload{Authorization: "Bearer x"}, "/api/log/", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 101 || len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("rows=%d pages=%v", len(rows), pages)
	}
}

func TestFetchLogWindowsBisectsDenseRangesWithoutDroppingRows(t *testing.T) {
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := start.Add(time.Hour)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		lower, _ := strconv.ParseInt(r.URL.Query().Get("start_timestamp"), 10, 64)
		upper, _ := strconv.ParseInt(r.URL.Query().Get("end_timestamp"), 10, 64)
		items := []map[string]any{{"id": fmt.Sprintf("row-%d", lower), "created_at": lower}}
		total := 1
		if upper-lower > int64((30 * time.Minute).Seconds()) {
			total = 11
			for id := 1; id < 10; id++ {
				items = append(items, map[string]any{"id": fmt.Sprintf("dense-%d", id), "created_at": lower})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"items": items, "total": total, "page": 1, "page_size": 10,
		}})
	}))
	defer server.Close()

	service := newService(nil, staticCredentialLoader{}, server.Client())
	config := DefaultConfig(1)
	config.PageSize, config.MaxPages = 10, 1
	rows, err := service.fetchLogWindows(context.Background(), domain.TargetSite{BaseURL: server.URL},
		domain.CredentialPayload{Authorization: "Bearer x"}, "/api/log/", config, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || len(rows) != 2 {
		t.Fatalf("requests=%d rows=%d, want 3/2", requests, len(rows))
	}
}

func ledgerSyncTestStore(t *testing.T, baseURL string) (*store.Store, domain.TargetSite) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "target", Kind: "management_token", SiteURL: baseURL}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: baseURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	return st, target
}

func assertStreamsHealthy(t *testing.T, streams []StreamStatus) {
	t.Helper()
	if len(streams) != 4 {
		t.Fatalf("streams = %#v", streams)
	}
	seenCommission := false
	for _, stream := range streams {
		if stream.Stream == streamCommissions {
			seenCommission = true
			if stream.Status != "healthy" && stream.Status != "unsupported" {
				t.Fatalf("commission stream did not degrade independently: %#v", stream)
			}
			continue
		}
		if stream.Status != "healthy" || stream.LastSuccessAt == nil {
			t.Fatalf("stream not healthy: %#v", stream)
		}
	}
	if !seenCommission {
		t.Fatal("commission stream is missing")
	}
}
