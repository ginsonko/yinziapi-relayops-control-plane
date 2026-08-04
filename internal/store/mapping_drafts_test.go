package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCreateMappingDraftPoliciesIsAtomicAndIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	credential := domain.Credential{Name: "target", Kind: "test", SiteURL: "https://target.example", Status: "active"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	firstSupplier := domain.Supplier{Name: "first", BaseURL: "https://first.example", Enabled: true}
	secondSupplier := domain.Supplier{Name: "second", BaseURL: "https://second.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &firstSupplier); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSupplier(ctx, &secondSupplier); err != nil {
		t.Fatal(err)
	}
	existing := mappingDraftPolicy(firstSupplier.ID, target.ID, "source-a", "owned")
	initial, err := st.CreateMappingDraftPolicies(ctx, []domain.PricingPolicy{existing})
	if err != nil || len(initial) != 1 || !initial[0].Created {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}
	replayed, err := st.CreateMappingDraftPolicies(ctx, []domain.PricingPolicy{existing})
	if err != nil || len(replayed) != 1 || replayed[0].Created || replayed[0].Policy.ID != initial[0].Policy.ID {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}

	batch := []domain.PricingPolicy{
		mappingDraftPolicy(firstSupplier.ID, target.ID, "source-new", "new-target"),
		mappingDraftPolicy(secondSupplier.ID, target.ID, "source-b", "owned"),
	}
	_, err = st.CreateMappingDraftPolicies(ctx, batch)
	var conflict *MappingOwnershipConflictError
	if err == nil || !errors.As(err, &conflict) || conflict.Owner.ID != initial[0].Policy.ID {
		t.Fatalf("conflict=%#v err=%v", conflict, err)
	}
	policies, err := st.ListPolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].ID != initial[0].Policy.ID {
		t.Fatalf("atomic rollback policies=%#v err=%v", policies, err)
	}
}

func TestSaveManualMappingPoliciesRollsBackWholeBatch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	credential := domain.Credential{Name: "target", Kind: "test", SiteURL: "https://target.example", Status: "active"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	firstSupplier := domain.Supplier{Name: "first", BaseURL: "https://first.example", Enabled: true}
	secondSupplier := domain.Supplier{Name: "second", BaseURL: "https://second.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &firstSupplier); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSupplier(ctx, &secondSupplier); err != nil {
		t.Fatal(err)
	}
	existing, err := st.SaveManualMappingPolicy(ctx, mappingDraftPolicy(secondSupplier.ID, target.ID, "source-existing", "owned"), 0, nil)
	if err != nil || !existing.Created {
		t.Fatalf("existing=%#v err=%v", existing, err)
	}
	changedExisting := mappingDraftPolicy(secondSupplier.ID, target.ID, "source-existing", "owned")
	changedExisting.Value = "0.02"
	stale := existing.Policy.UpdatedAt.Add(-time.Second)
	_, err = st.SaveManualMappingPolicies(ctx, []ManualMappingPolicyMutation{
		{Policy: mappingDraftPolicy(firstSupplier.ID, target.ID, "source-new", "new-target")},
		{Policy: changedExisting, ExpectedPolicyID: existing.Policy.ID, ExpectedPolicyUpdatedAt: &stale},
	})
	if err == nil || !errors.Is(err, ErrManualMappingStale) {
		t.Fatalf("batch stale error=%v", err)
	}
	policies, err := st.ListPolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].ID != existing.Policy.ID || policies[0].Value != existing.Policy.Value {
		t.Fatalf("atomic rollback policies=%#v err=%v", policies, err)
	}
}

func mappingDraftPolicy(supplierID, targetID int64, source, target string) domain.PricingPolicy {
	return domain.PricingPolicy{Name: "draft", SupplierID: supplierID, SourceGroup: source, SourceModelPattern: "gpt-test",
		TargetSiteID: targetID, TargetGroup: target, RuleType: "add", Value: "0.01", Enabled: false, ExecutionMode: "approval"}
}
