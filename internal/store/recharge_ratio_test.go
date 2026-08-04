package store

import (
	"context"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestLatestEffectiveSnapshotPreservesRawRatioAndAppliesCurrentFactorOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "discounted upstream", BaseURL: "https://upstream.example", Adapter: "sub2api", Enabled: true,
		RechargeValueFactor: "0.4"}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: "ccmax", Name: "ccmax", Ratio: "3", ModelIDs: []string{"gpt-test"}}},
		Evidence: domain.Evidence{ObservedAt: time.Now().UTC(), ResponseHash: "raw-catalog"}}
	if err := st.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	raw, err := st.LatestSnapshot(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Groups[0].Ratio != "3" || raw.Groups[0].RawRatio != "" || raw.Groups[0].EffectiveRatio != "" {
		t.Fatalf("persisted evidence was rewritten: %#v", raw.Groups[0])
	}
	effective, err := st.LatestEffectiveSnapshot(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	group := effective.Groups[0]
	if group.Ratio != "3" || group.RawRatio != "3" || group.RechargeValueFactor != "0.4" || group.EffectiveRatio != "1.2" {
		t.Fatalf("effective snapshot=%#v", group)
	}

	supplier.RechargeValueFactor = "0.5"
	if err := st.UpdateSupplier(ctx, supplier); err != nil {
		t.Fatal(err)
	}
	recalculated, err := st.LatestEffectiveSnapshot(ctx, supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recalculated.Groups[0].EffectiveRatio != "1.5" || recalculated.Groups[0].RawRatio != "3" {
		t.Fatalf("factor update did not re-evaluate raw evidence exactly once: %#v", recalculated.Groups[0])
	}
}
