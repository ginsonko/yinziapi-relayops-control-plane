package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestAddActivityCoalescedFoldsSameStructuredFailureWithChangingMessage(t *testing.T) {
	t.Parallel()
	st, err := Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	supplierID := supplier.ID
	base := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	activity := domain.Activity{Kind: "collection_failed", Severity: "warning", Title: "upstream failed",
		Message: "credential expired", SupplierID: &supplierID, Metadata: map[string]string{"error_class": "auth_expired"}, CreatedAt: base}
	if err := st.AddActivityCoalesced(ctx, activity, 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	activity.Message = "credential expired on a different transient port"
	activity.CreatedAt = base.Add(time.Minute)
	if err := st.AddActivityCoalesced(ctx, activity, 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	activities, err := st.RecentActivity(ctx, 10)
	if err != nil || len(activities) != 1 {
		t.Fatalf("activities=%#v error=%v", activities, err)
	}
	if activities[0].Metadata["repeat_count"] != "2" || activities[0].Metadata["first_seen_at"] == "" ||
		activities[0].Metadata["last_seen_at"] == "" || !activities[0].CreatedAt.Equal(activity.CreatedAt) ||
		activities[0].Message != activity.Message {
		t.Fatalf("coalesced metadata=%#v created=%v", activities[0].Metadata, activities[0].CreatedAt)
	}

	activity.Message = "permission denied"
	activity.Metadata["error_class"] = "permission_denied"
	activity.CreatedAt = base.Add(2 * time.Minute)
	if err := st.AddActivityCoalesced(ctx, activity, 6*time.Hour); err != nil {
		t.Fatal(err)
	}
	activities, err = st.RecentActivity(ctx, 10)
	if err != nil || len(activities) != 2 || activities[0].Message != "permission denied" {
		t.Fatalf("distinct failure was not retained: %#v error=%v", activities, err)
	}
}

func TestAddActivityCoalescedSeparatesDifferentCatalogDiffs(t *testing.T) {
	t.Parallel()
	st, err := Open(filepath.Join(t.TempDir(), "pricing-activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	supplierID := supplier.ID
	base := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	activity := domain.Activity{Kind: "pricing_changed", Severity: "info", Title: "目录变化", Message: "0.1 -> 0.11", SupplierID: &supplierID,
		Metadata: map[string]string{"catalog_diff_key": "diff-a"}, CreatedAt: base}
	if err := st.AddActivityCoalesced(ctx, activity, time.Hour); err != nil {
		t.Fatal(err)
	}
	activity.CreatedAt = base.Add(time.Minute)
	activity.Message = "同一差异的最新说明"
	if err := st.AddActivityCoalesced(ctx, activity, time.Hour); err != nil {
		t.Fatal(err)
	}
	activity.CreatedAt = base.Add(2 * time.Minute)
	activity.Metadata = map[string]string{"catalog_diff_key": "diff-b"}
	activity.Message = "0.11 -> 0.12"
	if err := st.AddActivityCoalesced(ctx, activity, time.Hour); err != nil {
		t.Fatal(err)
	}
	activities, err := st.RecentActivity(ctx, 10)
	if err != nil || len(activities) != 2 {
		t.Fatalf("activities=%#v error=%v", activities, err)
	}
	if activities[1].Metadata["repeat_count"] != "2" || activities[0].Metadata["catalog_diff_key"] != "diff-b" {
		t.Fatalf("coalesced pricing activities = %#v", activities)
	}
}

func TestQueryAndDeleteActivitiesUsesServerSideFiltersAndBounds(t *testing.T) {
	t.Parallel()
	st, err := Open(filepath.Join(t.TempDir(), "activity-query.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	base := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	fixtures := []domain.Activity{
		{Kind: "collection_failed", Severity: "warning", Title: "collect", Message: "c", CreatedAt: base},
		{Kind: "credential_refresh_recovered", Severity: "success", Title: "auth", Message: "a", CreatedAt: base.Add(time.Minute)},
		{Kind: "proposal_applied", Severity: "info", Title: "price", Message: "p", CreatedAt: base.Add(2 * time.Minute)},
		{Kind: "mirror_apply_failed", Severity: "error", Title: "mirror", Message: "m", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, activity := range fixtures {
		if err := st.AddActivity(ctx, activity); err != nil {
			t.Fatal(err)
		}
	}

	page, err := st.QueryActivities(ctx, ActivityQuery{Page: 1, Limit: 2})
	if err != nil || page.Total != 4 || len(page.Items) != 2 || page.Items[0].Kind != "mirror_apply_failed" {
		t.Fatalf("first page=%#v error=%v", page, err)
	}
	filtered, err := st.QueryActivities(ctx, ActivityQuery{Category: "price", Status: "healthy"})
	if err != nil || filtered.Total != 1 || filtered.Items[0].Kind != "proposal_applied" {
		t.Fatalf("filtered page=%#v error=%v", filtered, err)
	}
	incremental, err := st.QueryActivities(ctx, ActivityQuery{AfterID: 2, Limit: 50, SkipTotal: true})
	if err != nil || incremental.Total != 0 || len(incremental.Items) != 2 ||
		incremental.Items[0].ID != 3 || incremental.Items[1].ID != 4 {
		t.Fatalf("incremental page=%#v error=%v", incremental, err)
	}
	from := base.Add(time.Minute)
	withoutAuth, err := st.QueryActivities(ctx, ActivityQuery{From: &from, ExcludeCategory: "auth"})
	if err != nil || withoutAuth.Total != 2 {
		t.Fatalf("excluded page=%#v error=%v", withoutAuth, err)
	}

	deleted, err := st.DeleteActivities(ctx, ActivityDeleteRequest{IDs: []int64{page.Items[0].ID}, Limit: 500})
	if err != nil || deleted != 1 {
		t.Fatalf("delete selected=%d error=%v", deleted, err)
	}
	deleted, err = st.DeleteActivities(ctx, ActivityDeleteRequest{Query: ActivityQuery{Category: "auth"}, Limit: 500})
	if err != nil || deleted != 1 {
		t.Fatalf("delete filtered=%d error=%v", deleted, err)
	}
	if _, err := st.DeleteActivities(ctx, ActivityDeleteRequest{}); err == nil {
		t.Fatal("unscoped deletion was accepted without all=true")
	}
	deleted, err = st.DeleteActivities(ctx, ActivityDeleteRequest{All: true, Limit: 1})
	if err != nil || deleted != 1 {
		t.Fatalf("bounded clear=%d error=%v", deleted, err)
	}
	remaining, err := st.QueryActivities(ctx, ActivityQuery{})
	if err != nil || remaining.Total != 1 {
		t.Fatalf("remaining=%#v error=%v", remaining, err)
	}
}
