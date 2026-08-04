package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSafePricingPolicyDeletionBlocksActiveWriteAndDoesNotTouchTargetPrice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "policy source", BaseURL: "https://policy-source.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "policy target admin", Kind: "newapi-management", SiteURL: "https://policy-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "policy target", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true, WriteMode: "auto"}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "delete this policy", SupplierID: supplier.ID, SourceGroup: "upstream", TargetSiteID: target.ID,
		TargetGroup: "local", RuleType: "add", Value: "0.03", Enabled: true, ExecutionMode: "auto"}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: policy.TargetGroup, ExpectedOld: "0.13",
		ProposedValue: "0.16", ExpectedHash: "target-hash", Status: "pending", Mode: "auto", IdempotencyKey: "policy-delete-proposal"}
	if err := st.CreateProposal(ctx, &proposal); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewPricingPolicyDeletion(ctx, policy.ID)
	if err != nil || preview.ChangeProposals != 1 || preview.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	if _, err := st.DeletePricingPolicySafely(ctx, policy.ID, "wrong", preview.ImpactHash); !errors.Is(err, ErrPricingPolicyDeletionConflict) {
		t.Fatalf("wrong name error=%v", err)
	}
	leased, err := st.TryAcquireTargetChangeLease(ctx, target.ID, "policy-delete-test", time.Now().UTC(), time.Hour)
	if err != nil || !leased {
		t.Fatalf("lease=%v err=%v", leased, err)
	}
	leasedPreview, err := st.PreviewPricingPolicyDeletion(ctx, policy.ID)
	if err != nil || leasedPreview.ActiveLeases != 1 {
		t.Fatalf("leased preview=%#v err=%v", leasedPreview, err)
	}
	if _, err := st.DeletePricingPolicySafely(ctx, policy.ID, policy.Name, leasedPreview.ImpactHash); !errors.Is(err, ErrPricingPolicyDeletionConflict) {
		t.Fatalf("delete while leased error=%v", err)
	}
	if err := st.ReleaseTargetChangeLease(ctx, target.ID, "policy-delete-test"); err != nil {
		t.Fatal(err)
	}
	policy.Value = "0.04"
	if err := st.UpdatePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeletePricingPolicySafely(ctx, policy.ID, policy.Name, preview.ImpactHash); !errors.Is(err, ErrPricingPolicyDeletionConflict) {
		t.Fatalf("stale preview error=%v", err)
	}
	fresh, err := st.PreviewPricingPolicyDeletion(ctx, policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeletePricingPolicySafely(ctx, policy.ID, policy.Name, fresh.ImpactHash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPolicy(ctx, policy.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("policy still exists: %v", err)
	}
	if storedTarget, err := st.GetTarget(ctx, target.ID); err != nil || storedTarget.ID != target.ID || storedTarget.WriteMode != "auto" {
		t.Fatalf("target changed during policy deletion: %#v err=%v", storedTarget, err)
	}
	if _, err := st.GetProposal(ctx, proposal.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("proposal still exists: %v", err)
	}
	var audit int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activities WHERE kind='pricing_policy_deleted'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("audit=%d err=%v", audit, err)
	}
}
