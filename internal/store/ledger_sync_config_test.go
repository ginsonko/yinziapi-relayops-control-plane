package store

import (
	"context"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestTargetLedgerSyncConfigRoundTripKeepsCostAndPathFieldsAligned(t *testing.T) {
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "ledger-config", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "ledger-config", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	config := TargetLedgerSyncConfig{TargetSiteID: target.ID, Enabled: true, IntervalSeconds: 321, OverlapSeconds: 654,
		InitialLookbackSeconds: 9876, PageSize: 123, MaxPages: 45, Currency: "CNY", QuotaCashFactor: "0.000002",
		CostEvidenceMaxAgeSeconds: 43210, SupplierCostCashRate: "1.25", LogPath: "/custom/log", TopupPath: "/custom/topup", UserPath: "/custom/users"}
	if err := st.UpsertTargetLedgerSyncConfig(ctx, &config); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTargetLedgerSyncConfig(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CostEvidenceMaxAgeSeconds != 43210 || stored.SupplierCostCashRate != "1.25" || stored.LogPath != "/custom/log" ||
		stored.TopupPath != "/custom/topup" || stored.UserPath != "/custom/users" {
		t.Fatalf("misaligned ledger config = %#v", stored)
	}
}

func TestTargetLedgerCostResolutionStatusReplacesReasonsAtomically(t *testing.T) {
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "ledger-cost-diagnostics", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "ledger-cost-diagnostics", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	status := TargetLedgerCostResolutionStatus{TargetSiteID: target.ID, WindowStart: now.Add(-time.Hour), WindowEnd: now,
		ScannedEvents: 3, ResolvedEvents: 1, UnresolvedEvents: 2, ObservedAt: now,
		Reasons: []TargetLedgerCostResolutionReason{{Code: "channel_policy_binding_missing", Message: "mapping missing", Count: 2,
			Samples: []TargetLedgerCostResolutionSample{{ChannelID: "12", TargetGroup: "plus", Model: "gpt-test"}}}}}
	if err := st.ReplaceTargetLedgerCostResolutionStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTargetLedgerCostResolutionStatus(ctx, target.ID)
	if err != nil || stored.UnresolvedEvents != 2 || len(stored.Reasons) != 1 || stored.Reasons[0].Samples[0].ChannelID != "12" {
		t.Fatalf("stored diagnostics = %#v err=%v", stored, err)
	}
	status.ScannedEvents, status.ResolvedEvents, status.UnresolvedEvents, status.Reasons = 0, 0, 0, nil
	status.ObservedAt = now.Add(time.Minute)
	if err := st.ReplaceTargetLedgerCostResolutionStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	stored, err = st.GetTargetLedgerCostResolutionStatus(ctx, target.ID)
	if err != nil || stored.UnresolvedEvents != 0 || len(stored.Reasons) != 0 {
		t.Fatalf("replaced diagnostics retained stale reasons: %#v err=%v", stored, err)
	}
}
