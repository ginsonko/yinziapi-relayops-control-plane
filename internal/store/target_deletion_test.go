package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSafeTargetDeletionRejectsStalePreviewAndPreservesSharedCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)

	credential := domain.Credential{Name: "shared target credential", Kind: "newapi-management", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted-payload"); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "credential peer", BaseURL: "https://supplier.example", Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target to delete", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true, WriteMode: "auto"}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "target policy", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		TargetGroup: "economy", RuleType: "add", Value: "0.03", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "economy", ExpectedOld: "0.13",
		ProposedValue: "0.16", ExpectedHash: "expected", Status: "pending", Mode: "approval", IdempotencyKey: "target-delete-proposal"}
	if err := st.CreateProposal(ctx, &proposal); err != nil {
		t.Fatal(err)
	}
	ledger := TargetLedgerSyncConfig{TargetSiteID: target.ID, Enabled: true, IntervalSeconds: 300, OverlapSeconds: 600,
		InitialLookbackSeconds: 86400, PageSize: 100, MaxPages: 20, LogPath: "/api/log/", TopupPath: "/api/user/topup", UserPath: "/api/user/"}
	if err := st.UpsertTargetLedgerSyncConfig(ctx, &ledger); err != nil {
		t.Fatal(err)
	}

	preview, err := st.PreviewTargetDeletion(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.PricingPolicies != 1 || preview.ChangeProposals != 1 || preview.LedgerSyncRecords == 0 ||
		!preview.CredentialPreserved || preview.CredentialSupplierRefs != 1 || preview.CredentialTargetRefs != 1 || preview.ImpactHash == "" {
		t.Fatalf("unexpected target deletion preview: %#v", preview)
	}
	if _, err := st.DeleteTargetSafely(ctx, target.ID, "wrong name", preview.ImpactHash); !errors.Is(err, ErrTargetDeletionConflict) {
		t.Fatalf("wrong confirmation error=%v", err)
	}

	leased, err := st.TryAcquireTargetChangeLease(ctx, target.ID, "test-delete-lease", time.Now().UTC(), time.Hour)
	if err != nil || !leased {
		t.Fatalf("acquire lease=%v err=%v", leased, err)
	}
	leasedPreview, err := st.PreviewTargetDeletion(ctx, target.ID)
	if err != nil || leasedPreview.ActiveLeases != 1 {
		t.Fatalf("leased preview=%#v err=%v", leasedPreview, err)
	}
	if _, err := st.DeleteTargetSafely(ctx, target.ID, target.Name, leasedPreview.ImpactHash); !errors.Is(err, ErrTargetDeletionConflict) {
		t.Fatalf("active lease delete error=%v", err)
	}
	if err := st.ReleaseTargetChangeLease(ctx, target.ID, "test-delete-lease"); err != nil {
		t.Fatal(err)
	}

	target.Enabled = false
	if err := st.UpdateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTargetSafely(ctx, target.ID, target.Name, preview.ImpactHash); !errors.Is(err, ErrTargetDeletionConflict) {
		t.Fatalf("stale preview error=%v", err)
	}
	fresh, err := st.PreviewTargetDeletion(ctx, target.ID)
	if err != nil || fresh.ImpactHash == preview.ImpactHash {
		t.Fatalf("fresh preview=%#v err=%v", fresh, err)
	}
	if _, err := st.DeleteTargetSafely(ctx, target.ID, target.Name, fresh.ImpactHash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTarget(ctx, target.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted target still exists: %v", err)
	}
	if storedCredential, _, err := st.GetCredentialEncrypted(ctx, credential.ID); err != nil || storedCredential.ID != credential.ID {
		t.Fatalf("credential was not preserved: %#v err=%v", storedCredential, err)
	}
	if storedSupplier, err := st.GetSupplier(ctx, supplier.ID); err != nil || storedSupplier.CredentialID == nil || *storedSupplier.CredentialID != credential.ID {
		t.Fatalf("supplier sharing credential changed: %#v err=%v", storedSupplier, err)
	}
	for table, query := range map[string]string{
		"policies":  `SELECT COUNT(*) FROM pricing_policies WHERE target_site_id=?`,
		"proposals": `SELECT COUNT(*) FROM change_proposals WHERE target_site_id=?`,
		"ledger":    `SELECT COUNT(*) FROM target_ledger_sync_configs WHERE target_site_id=?`,
	} {
		var count int
		if err := st.db.QueryRowContext(ctx, query, target.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var audit int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE supplier_id IS NULL AND kind='target_deleted'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("target deletion audit=%d err=%v", audit, err)
	}
}
