package store

import (
	"context"
	"testing"
	"time"
)

func TestListMirrorChangeSetsFiltersCompletedAndActiveAuditHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 29, 2, 30, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatalf("create mirror policy: %v", err)
	}
	lease, err := st.AcquireMirrorLease(ctx, target.ID, "audit-list", now, time.Minute)
	if err != nil {
		t.Fatalf("acquire mirror lease: %v", err)
	}

	first := newMirrorTestChangeSet(policy, target.ID, "audit-operation-1", "audit-request-1", "audit-step-1")
	replayed, err := st.CreateMirrorChangeSet(ctx, &first, lease, now.Add(time.Second))
	if err != nil {
		t.Fatalf("create first change set: %v", err)
	}
	if replayed {
		t.Fatal("first change set was unexpectedly replayed")
	}
	second := newMirrorTestChangeSet(policy, target.ID, "audit-operation-2", "audit-request-2", "audit-step-2")
	replayed, err = st.CreateMirrorChangeSet(ctx, &second, lease, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("create second change set: %v", err)
	}
	if replayed {
		t.Fatal("second change set was unexpectedly replayed")
	}

	items, err := st.ListMirrorChangeSets(ctx, policy.PolicyUUID, "pending", 100)
	if err != nil {
		t.Fatalf("list pending change sets: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("pending change set count = %d, want 2", len(items))
	}
	if items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("change set order = [%d %d], want [%d %d]", items[0].ID, items[1].ID, second.ID, first.ID)
	}
	if len(items[0].Steps) != 0 {
		t.Fatalf("list response included %d steps; detail must load steps separately", len(items[0].Steps))
	}

	items, err = st.ListMirrorChangeSets(ctx, policy.PolicyUUID, "applied", 100)
	if err != nil {
		t.Fatalf("list applied change sets: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("applied change set count = %d, want 0", len(items))
	}
	_, err = st.ListMirrorChangeSets(ctx, "not-a-uuid", "", 100)
	if err == nil {
		t.Fatal("invalid policy UUID was accepted")
	}
}
