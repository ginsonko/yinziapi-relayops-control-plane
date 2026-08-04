package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSafeCredentialDeletionRequiresOrphanAndFreshPreview(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	credential := domain.Credential{Name: "orphan candidate", Kind: "access-token", SiteURL: "https://credential.example"}
	if err := st.CreateCredential(ctx, &credential, "secret-payload"); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "credential owner", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewCredentialDeletion(ctx, credential.ID)
	if err != nil || len(preview.SupplierRefs) != 1 || preview.TargetRefs == nil || preview.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	if _, err := st.DeleteCredentialSafely(ctx, credential.ID, credential.Name, preview.ImpactHash); !errors.Is(err, ErrCredentialDeletionConflict) {
		t.Fatalf("referenced delete error=%v", err)
	}
	supplier.CredentialID = nil
	if err := st.UpdateSupplier(ctx, supplier); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteCredentialSafely(ctx, credential.ID, credential.Name, preview.ImpactHash); !errors.Is(err, ErrCredentialDeletionConflict) {
		t.Fatalf("stale preview error=%v", err)
	}
	fresh, err := st.PreviewCredentialDeletion(ctx, credential.ID)
	if err != nil || len(fresh.SupplierRefs) != 0 || fresh.SupplierRefs == nil || fresh.TargetRefs == nil {
		t.Fatalf("fresh preview=%#v err=%v", fresh, err)
	}
	if _, err := st.DeleteCredentialSafely(ctx, credential.ID, credential.Name, fresh.ImpactHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GetCredentialEncrypted(ctx, credential.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("credential still exists: %v", err)
	}
	var audit int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE kind='credential_deleted'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("audit=%d err=%v", audit, err)
	}
}
