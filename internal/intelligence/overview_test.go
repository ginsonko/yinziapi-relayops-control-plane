package intelligence

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestOverviewUsesIncrementalLedgerCacheWithoutBuildingFullDashboard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := domain.Credential{Name: "overview", Kind: "access-token", SiteURL: "https://overview.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "overview", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	firstEvent := store.OperatingLedgerEvent{EventID: "overview-1", TargetSiteID: target.ID, OccurredAt: now,
		EventKind: "usage", ModelID: "model", GroupName: "group", Requests: 1, CashRevenue: "2", ProcurementCost: "1",
		Currency: "CNY", RevenueEvidence: "exact", CostEvidence: "exact", Source: "test", EvidenceRevision: "1", PayloadHash: "overview-1"}
	if err := st.UpsertOperatingLedgerEvents(ctx, []store.OperatingLedgerEvent{firstEvent}); err != nil {
		t.Fatal(err)
	}

	service := NewService(st)
	first, err := service.Overview(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if first.RawEventCount != 1 || first.Operations.TodayUserSpend.Amount != "2" || first.CacheRevision != first.SourceRevision {
		t.Fatalf("first overview = %#v", first)
	}
	if !service.dashboardCache.result.GeneratedAt.IsZero() || service.dashboardFlight != nil {
		t.Fatal("overview populated or started the full dashboard cache")
	}

	secondEvent := firstEvent
	secondEvent.EventID = "overview-2"
	secondEvent.EvidenceRevision = "2"
	secondEvent.PayloadHash = "overview-2"
	secondEvent.CashRevenue = "3"
	if err := st.UpsertOperatingLedgerEvents(ctx, []store.OperatingLedgerEvent{secondEvent}); err != nil {
		t.Fatal(err)
	}
	second, err := service.Overview(ctx, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if second.RawEventCount != 2 || second.Operations.TodayUserSpend.Amount != "5" || second.CacheRevision <= first.CacheRevision ||
		second.CacheRevision != second.SourceRevision {
		t.Fatalf("incremental overview = %#v", second)
	}
}

func TestDashboardBackgroundTimeoutRemainsBoundedForLowCPUProduction(t *testing.T) {
	t.Parallel()
	if dashboardBackgroundTimeout != 4*time.Minute {
		t.Fatalf("dashboard background timeout = %s, want 4m", dashboardBackgroundTimeout)
	}
}
