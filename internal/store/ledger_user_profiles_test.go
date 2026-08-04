package store

import (
	"context"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestReplaceTargetLedgerUserProfilesRemovesStaleRowsAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "target admin", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "managed NewAPI", BaseURL: "https://target.example", Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	initial := []TargetLedgerUserProfile{
		{TargetSiteID: target.ID, UserID: "admin", Role: "administrator", CashBalance: "100", Currency: "CNY", SourceHash: "a", ObservedAt: now},
		{TargetSiteID: target.ID, UserID: "user-1", Role: "user", CashBalance: "20", Currency: "CNY", SourceHash: "b", ObservedAt: now},
	}
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, initial); err != nil {
		t.Fatal(err)
	}
	replacement := []TargetLedgerUserProfile{{TargetSiteID: target.ID, UserID: "user-1", Role: "user", CashBalance: "15", Currency: "CNY", SourceHash: "c", ObservedAt: now.Add(time.Minute)}}
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, replacement); err != nil {
		t.Fatal(err)
	}
	profiles, err := st.ListTargetLedgerUserProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].UserID != "user-1" || profiles[0].CashBalance != "15" {
		t.Fatalf("replacement profiles = %#v, %v", profiles, err)
	}
	invalid := []TargetLedgerUserProfile{{TargetSiteID: target.ID, UserID: "", SourceHash: "invalid", ObservedAt: now}}
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, invalid); err == nil {
		t.Fatal("invalid replacement snapshot was accepted")
	}
	profiles, err = st.ListTargetLedgerUserProfiles(ctx)
	if err != nil || len(profiles) != 1 || profiles[0].UserID != "user-1" {
		t.Fatalf("failed replacement was not atomic: %#v, %v", profiles, err)
	}
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, nil); err != nil {
		t.Fatal(err)
	}
	profiles, err = st.ListTargetLedgerUserProfiles(ctx)
	if err != nil || len(profiles) != 0 {
		t.Fatalf("successful empty snapshot retained stale users: %#v, %v", profiles, err)
	}
}
