package intelligence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestR42LargeDatabaseAcceptance(t *testing.T) {
	path := os.Getenv("RELAYOPS_R42_ACCEPTANCE_DB")
	if path == "" {
		t.Skip("set RELAYOPS_R42_ACCEPTANCE_DB to an isolated database copy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	config := NormalizeConfig(DefaultConfig())
	now := time.Now().UTC()
	financialOptions := store.LedgerFinancialReportOptions{Start: now.Add(-time.Duration(config.FinancialLookbackDays) * 24 * time.Hour),
		End: now, Exclusions: *config.LedgerExclusions}
	oldLedger, err := st.OperatingLedgerSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldFinancial, err := st.OperatingFinancialSummary(ctx, financialOptions)
	if err != nil {
		t.Fatal(err)
	}

	service := NewService(st)
	buildStarted := time.Now()
	view, err := service.ledgerData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	buildDuration := time.Since(buildStarted)
	newLedger := store.SummarizeOperatingLedgerEvents(view.Events)
	newFinancial, err := store.SummarizeOperatingFinancialEvents(view.Events, financialOptions)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEquivalent(t, "ledger", oldLedger, newLedger)
	assertJSONEquivalent(t, "financial", oldFinancial, newFinancial)

	firstStarted := time.Now()
	first, err := service.Dashboard(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	firstDuration := time.Since(firstStarted)
	durations := make([]time.Duration, 0, 25)
	for index := 0; index < 25; index++ {
		started := time.Now()
		result, err := service.Dashboard(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		if result.GeneratedAt != first.GeneratedAt || result.CacheRevision != first.CacheRevision {
			t.Fatalf("unchanged request replaced the successful snapshot: first=%s/%d current=%s/%d",
				first.GeneratedAt, first.CacheRevision, result.GeneratedAt, result.CacheRevision)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[(len(durations)*95+99)/100-1]
	if p95 >= 250*time.Millisecond {
		t.Fatalf("unchanged dashboard p95=%s, want <250ms", p95)
	}

	targets, err := st.ListTargets(ctx)
	if err != nil || len(targets) == 0 {
		t.Fatalf("large database has no target for incremental acceptance: targets=%d err=%v", len(targets), err)
	}
	events := make([]store.OperatingLedgerEvent, 100)
	for index := range events {
		occurred := time.Now().UTC()
		events[index] = store.OperatingLedgerEvent{EventID: fmt.Sprintf("r42-acceptance-%d-%03d", occurred.UnixNano(), index),
			TargetSiteID: targets[0].ID, OccurredAt: occurred, EventKind: "usage", ModelID: "r42-acceptance",
			GroupName: "r42-acceptance", Requests: 1, CashRevenue: "0", ProcurementCost: "0", Currency: "CNY",
			RevenueEvidence: "exact", CostEvidence: "exact", Source: "r42-local-acceptance", EvidenceRevision: "r42",
			Metadata: map[string]string{"billing_mode": "per_request"}, PayloadHash: fmt.Sprintf("r42-%d-%03d", occurred.UnixNano(), index), CreatedAt: occurred}
	}
	inserted, err := st.UpsertOperatingLedgerEventsCount(ctx, events)
	if err != nil || inserted != len(events) {
		t.Fatalf("insert incremental acceptance events: inserted=%d err=%v", inserted, err)
	}
	incrementalStarted := time.Now()
	stale, err := service.Dashboard(ctx, config)
	if err != nil || !stale.Refreshing {
		t.Fatalf("changed revision did not start stale-while-revalidate: refreshing=%v err=%v", stale.Refreshing, err)
	}
	var refreshed Dashboard
	for time.Since(incrementalStarted) < 5*time.Second {
		time.Sleep(10 * time.Millisecond)
		refreshed, err = service.Dashboard(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		if !refreshed.Refreshing && refreshed.CacheRevision == refreshed.SourceRevision && refreshed.CacheRevision > first.CacheRevision {
			break
		}
	}
	incrementalDuration := time.Since(incrementalStarted)
	if refreshed.Refreshing || refreshed.CacheRevision != refreshed.SourceRevision {
		t.Fatalf("incremental refresh did not finish: cache=%d source=%d refreshing=%v", refreshed.CacheRevision, refreshed.SourceRevision, refreshed.Refreshing)
	}
	t.Logf("R42 acceptance: events=%d cache_build=%s first_dashboard=%s unchanged_p95=%s incremental_100=%s cache_bytes=%d",
		len(view.Events), buildDuration, firstDuration, p95, incrementalDuration, service.ledgerCache.estimatedBytes)
	if incrementalDuration >= time.Second {
		t.Fatalf("100-event incremental refresh=%s, want <1s", incrementalDuration)
	}
}

func assertJSONEquivalent(t *testing.T, name string, expected, actual any) {
	t.Helper()
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	if string(expectedJSON) != string(actualJSON) {
		t.Fatalf("%s summary differs\nold=%s\nnew=%s", name, expectedJSON, actualJSON)
	}
}
