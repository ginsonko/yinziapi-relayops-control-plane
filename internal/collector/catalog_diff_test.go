package collector

import (
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCatalogDiffIgnoresDecimalFormattingAndOrdering(t *testing.T) {
	previous := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "g1", Name: "economy", Ratio: "0.100000000000", ModelIDs: []string{"m2", "m1"}}}, Models: []domain.ModelOffer{{ExternalID: "m1", Name: "gpt", Group: "economy", ModelPrice: "0.050000"}}}
	current := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "g1", Name: "economy", Ratio: "0.1", ModelIDs: []string{"m1", "m2"}}}, Models: []domain.ModelOffer{{ExternalID: "m1", Name: "gpt", Group: "economy", ModelPrice: "0.05"}}}

	diff := summarizeCatalogDiff(&previous, current)
	if diff.semanticChanged() || diff.key() != "format_only" {
		t.Fatalf("format-only catalog was treated as semantic change: %#v", diff)
	}
	activity := diff.activity(domain.Supplier{ID: 7, Name: "upstream"}, "old", "new", 0)
	if !strings.Contains(activity.Title, "价格未变") || !strings.Contains(activity.Message, "未发现需要改价") {
		t.Fatalf("format-only activity is misleading: %#v", activity)
	}
}

func TestCatalogDiffReportsConcreteGroupAndModelChanges(t *testing.T) {
	previous := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "g1", Name: "economy", Ratio: "0.1", ModelIDs: []string{"m1"}}}, Models: []domain.ModelOffer{{ExternalID: "m1", Name: "gpt", Group: "economy", ModelPrice: "0.05"}}}
	current := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "g1", Name: "economy", Ratio: "0.11", ModelIDs: []string{"m1", "m2"}}}, Models: []domain.ModelOffer{{ExternalID: "m1", Name: "gpt", Group: "economy", ModelPrice: "0.06"}, {ExternalID: "m2", Name: "claude", Group: "economy", ModelPrice: "0.2"}}}

	diff := summarizeCatalogDiff(&previous, current)
	if !diff.semanticChanged() || len(diff.ChangedGroups) != 1 || len(diff.AddedModels) != 1 || len(diff.ChangedModels) != 1 {
		t.Fatalf("unexpected semantic diff: %#v", diff)
	}
	activity := diff.activity(domain.Supplier{ID: 7, Name: "upstream"}, "old", "new", 2)
	for _, fragment := range []string{"economy", "0.1 -> 0.11", "gpt", "0.05 -> 0.06", "关联规则 2 条"} {
		if !strings.Contains(activity.Message, fragment) {
			t.Fatalf("activity message %q does not contain %q", activity.Message, fragment)
		}
	}
	if activity.Metadata["diff_kind"] != "semantic" || activity.Metadata["affected_policy_count"] != "2" {
		t.Fatalf("activity metadata = %#v", activity.Metadata)
	}
}
