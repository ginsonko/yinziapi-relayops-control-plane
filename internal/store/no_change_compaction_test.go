package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOpenCompactsHistoricalNoChangeRowsAndActivities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relayops.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "target", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "supplier", BaseURL: "https://supplier.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "policy", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		TargetGroup: "economy", RuleType: "fixed", Value: "0.05", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	var proposals []domain.ChangeProposal
	for index := 0; index < 3; index++ {
		proposal := domain.ChangeProposal{PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "economy",
			ExpectedOld: "0.05", ProposedValue: "0.05", ExpectedHash: "hash", Status: "no_change", Mode: "auto",
			IdempotencyKey: "no-change-" + fmt.Sprint(index)}
		if err := st.CreateProposal(ctx, &proposal); err != nil {
			t.Fatal(err)
		}
		proposals = append(proposals, proposal)
		if err := st.AddActivity(ctx, domain.Activity{Kind: "proposal_created", Severity: "info", Title: "已生成价格变更预览",
			Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err := reopened.ListProposals(ctx, "no_change", 10)
	if err != nil || len(items) != 1 || items[0].ID != proposals[len(proposals)-1].ID {
		t.Fatalf("compacted proposals = %#v, error=%v", items, err)
	}
	activities, err := reopened.RecentActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	maintenance := false
	for _, activity := range activities {
		if activity.Kind == "proposal_created" {
			t.Fatalf("historical no-change activity survived startup compaction: %#v", activity)
		}
		if activity.Kind == "maintenance" {
			maintenance = true
		}
	}
	if !maintenance {
		t.Fatal("startup compaction did not leave an operator-facing maintenance receipt")
	}
}
