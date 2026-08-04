package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSafeSupplierDeletionRejectsStalePreviewAndPreservesCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)

	credential := domain.Credential{Name: "shared upstream credential", Kind: "access-token", SiteURL: "https://supplier.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted-payload"); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "supplier to delete", BaseURL: "https://supplier.example", Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "shared credential target", BaseURL: "https://supplier.example", Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "supplier policy", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		TargetGroup: "economy", RuleType: "add", Value: "0.03", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "economy", ExpectedOld: "0.13",
		ProposedValue: "0.16", ExpectedHash: "expected", Status: "pending", Mode: "approval", IdempotencyKey: "supplier-delete-proposal"}
	if err := st.CreateProposal(ctx, &proposal); err != nil {
		t.Fatal(err)
	}

	now := formatTime(time.Now().UTC())
	if _, err := st.db.ExecContext(ctx, `INSERT INTO catalog_snapshots
		(supplier_id,payload_json,response_hash,observed_at,total_observations,consecutive_observations,last_seen)
		VALUES(?,?,?,?,1,1,?)`, supplier.ID, `{}`, "catalog-1", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO price_observations
		(supplier_id,entity_type,entity_key,group_name,model_name,price_json,evidence_json,observed_at)
		VALUES(?,?,?,?,?,?,?,?)`, supplier.ID, "group", "economy", "economy", "", `{}`, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO balance_observations(supplier_id,payload_json,observed_at) VALUES(?,?,?)`, supplier.ID, `{}`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at)
		VALUES('collect','info','collected','ok',?,'{}',?)`, supplier.ID, now); err != nil {
		t.Fatal(err)
	}

	preview, err := st.PreviewSupplierDeletion(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.PricingPolicies != 1 || preview.ChangeProposals != 1 || preview.CatalogSnapshots != 1 || preview.PriceObservations != 1 ||
		preview.BalanceObservations != 1 || preview.ActivitiesDetached != 1 || !preview.CredentialPreserved || preview.ImpactHash == "" {
		t.Fatalf("unexpected deletion preview: %#v", preview)
	}
	if _, err := st.DeleteSupplierSafely(ctx, supplier.ID, "wrong name", preview.ImpactHash); !errors.Is(err, ErrSupplierDeletionConflict) {
		t.Fatalf("wrong confirmation error = %v", err)
	}
	if _, err := st.GetSupplier(ctx, supplier.ID); err != nil {
		t.Fatalf("wrong confirmation changed supplier: %v", err)
	}

	if _, err := st.db.ExecContext(ctx, `INSERT INTO catalog_snapshots
		(supplier_id,payload_json,response_hash,observed_at,total_observations,consecutive_observations,last_seen)
		VALUES(?,?,?,?,1,1,?)`, supplier.ID, `{}`, "catalog-2", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteSupplierSafely(ctx, supplier.ID, supplier.Name, preview.ImpactHash); !errors.Is(err, ErrSupplierDeletionConflict) {
		t.Fatalf("stale preview error = %v", err)
	}
	fresh, err := st.PreviewSupplierDeletion(ctx, supplier.ID)
	if err != nil || fresh.CatalogSnapshots != 2 || fresh.ImpactHash == preview.ImpactHash {
		t.Fatalf("fresh preview = %#v, err=%v", fresh, err)
	}
	deleted, err := st.DeleteSupplierSafely(ctx, supplier.ID, supplier.Name, fresh.ImpactHash)
	if err != nil || deleted.SupplierID != supplier.ID {
		t.Fatalf("delete = %#v, err=%v", deleted, err)
	}
	if _, err := st.GetSupplier(ctx, supplier.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted supplier still exists: %v", err)
	}
	if storedCredential, _, err := st.GetCredentialEncrypted(ctx, credential.ID); err != nil || storedCredential.ID != credential.ID {
		t.Fatalf("shared credential was not preserved: %#v, %v", storedCredential, err)
	}
	if storedTarget, err := st.GetTarget(ctx, target.ID); err != nil || storedTarget.CredentialID != credential.ID {
		t.Fatalf("target using shared credential changed: %#v, %v", storedTarget, err)
	}
	for table, query := range map[string]string{
		"policies":  `SELECT COUNT(*) FROM pricing_policies WHERE supplier_id=?`,
		"proposals": `SELECT COUNT(*) FROM change_proposals WHERE policy_id=?`,
		"snapshots": `SELECT COUNT(*) FROM catalog_snapshots WHERE supplier_id=?`,
	} {
		argument := supplier.ID
		if table == "proposals" {
			argument = policy.ID
		}
		var count int
		if err := st.db.QueryRowContext(ctx, query, argument).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	var detached, audit int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE supplier_id IS NULL AND title='collected'`).Scan(&detached); err != nil || detached != 1 {
		t.Fatalf("detached activity count=%d err=%v", detached, err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE supplier_id IS NULL AND kind='supplier_deleted'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("deletion audit count=%d err=%v", audit, err)
	}
}
