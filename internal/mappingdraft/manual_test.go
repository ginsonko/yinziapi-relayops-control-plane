package mappingdraft

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestManualPreviewLargeMatrixMatchesReferenceBucketing(t *testing.T) {
	const targetCount, sourceCount = 50, 192
	target := domain.CatalogSnapshot{Evidence: domain.Evidence{ResponseHash: "target-large-v1", SourceType: "authenticated_api",
		ObservedAt: time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)}}
	channels := make([]domain.TargetChannel, 0, targetCount)
	models := make([]string, 0, targetCount)
	for index := 0; index < targetCount; index++ {
		group, model := fmt.Sprintf("target-%02d", index), fmt.Sprintf("model-%02d", index)
		models = append(models, model)
		target.Groups = append(target.Groups, domain.GroupOffer{ExternalID: group, Name: group, Ratio: "0.08", ModelIDs: []string{model}})
		channels = append(channels, domain.TargetChannel{ID: int64(index + 1), Name: group, Status: 1,
			BaseURL: "https://upstream.example/v1", Group: group, Models: []string{model}, ConfigHash: fmt.Sprintf("channel-%02d", index)})
	}
	fixture := newMappingFixture(t, staticTargetReader{catalog: target, channels: channels})
	source := domain.CatalogSnapshot{SupplierID: fixture.supplier.ID,
		Evidence: domain.Evidence{ResponseHash: "source-large-v1", SourceType: "authenticated_api", ObservedAt: fixture.now}}
	for index := 0; index < sourceCount; index++ {
		group := fmt.Sprintf("source-%03d", index)
		source.Groups = append(source.Groups, domain.GroupOffer{ExternalID: group, Name: group, Ratio: "0.05", ModelIDs: models})
	}
	if err := fixture.store.SaveSnapshot(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	preview, err := fixture.service.ManualPreview(context.Background(), ManualPreviewRequest{TargetSiteID: fixture.target.ID})
	optimizedDuration := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("optimized 50x192 manual preview took %s", optimizedDuration)
	if len(preview.Groups) != targetCount || preview.Summary.TargetGroups != targetCount || preview.Summary.SuppliersUsable != 1 {
		t.Fatalf("large preview summary=%#v groups=%d", preview.Summary, len(preview.Groups))
	}

	targetGroups := buildTargetGroups(target, channels)
	config, err := normalizePreviewRequest(PreviewRequest{MaximumOptions: 1000}, true)
	if err != nil {
		t.Fatal(err)
	}
	reference := make(map[string][]ManualSourceOption, targetCount)
	for _, sourceGroup := range buildSourceGroups(fixture.supplier, source, fixture.now) {
		candidate := buildCandidate(sourceGroup, fixture.target.ID, preview.TargetEvidenceHash, target, targetGroups,
			map[string][]domain.PricingPolicy{}, config).candidate
		for _, targetGroup := range targetGroups {
			option, found := findOption(candidate.Options, targetGroup.name)
			if !found {
				t.Fatalf("candidate %s has no option for %s", candidate.ID, targetGroup.name)
			}
			key := strings.ToLower(strings.TrimSpace(targetGroup.name))
			reference[key] = append(reference[key], manualSourceOption(candidate, option, targetGroup.ratio))
		}
	}
	for index := range preview.Groups {
		group := &preview.Groups[index]
		expected := reference[strings.ToLower(strings.TrimSpace(group.TargetGroup))]
		sort.SliceStable(expected, func(i, j int) bool {
			left, right := expected[i], expected[j]
			if left.OriginMatch != right.OriginMatch {
				return left.OriginMatch
			}
			if basisPoints(left.Score) != basisPoints(right.Score) {
				return basisPoints(left.Score) > basisPoints(right.Score)
			}
			if left.SupplierName != right.SupplierName {
				return strings.ToLower(left.SupplierName) < strings.ToLower(right.SupplierName)
			}
			return strings.ToLower(left.SourceGroup) < strings.ToLower(right.SourceGroup)
		})
		if len(group.Options) != sourceCount || !reflect.DeepEqual(group.Options, expected) {
			t.Fatalf("group %s optimized bucket differs from reference: got=%d want=%d", group.TargetGroup, len(group.Options), len(expected))
		}
	}
}

func TestManualPreviewUsesTargetChannelsAsPrimaryRows(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{
		catalog: targetCatalog("target-v1", "yinzi-plus", "0.08", []string{"gpt-test", "gpt-fast"}),
		channels: []domain.TargetChannel{{ID: 17, Name: "mdkj plus 渠道", Status: 1, BaseURL: "https://upstream.example/v1",
			Group: "yinzi-plus", Models: []string{"gpt-test", "gpt-fast"}, ConfigHash: "channel-v1"}},
	})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "plus", "0.05", []string{"gpt-test", "gpt-fast"})

	preview, err := fixture.service.ManualPreview(context.Background(), ManualPreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.TargetGroups != 1 || preview.Summary.TargetChannels != 1 || preview.Summary.Unmapped != 1 || len(preview.Groups) != 1 {
		t.Fatalf("manual summary=%#v groups=%#v", preview.Summary, preview.Groups)
	}
	group := preview.Groups[0]
	if group.TargetGroup != "yinzi-plus" || group.TargetRatio != "0.08" || len(group.Channels) != 1 ||
		group.Channels[0].Name != "mdkj plus 渠道" || group.Channels[0].BaseOrigin != "https://upstream.example" {
		t.Fatalf("target channel row=%#v", group)
	}
	if len(group.Options) != 1 || group.RecommendedOption != group.Options[0].CandidateID || !group.Options[0].OriginMatch ||
		group.Options[0].SupplierName != fixture.supplier.Name || group.Options[0].SourceGroup != "plus" ||
		group.Options[0].SourceRatio != "0.05" || group.Options[0].RawSourceRatio != "0.05" || group.Options[0].RechargeValueFactor != "1" ||
		group.Options[0].RecommendedRuleType != "add" || group.Options[0].RecommendedValue != "0.03" ||
		!strings.Contains(group.Options[0].SourceModelPattern, "gpt-test") {
		t.Fatalf("manual source option=%#v", group.Options)
	}
}

func TestManualSourceOptionKeepsRealGroupRatioSeparateFromEquivalentCost(t *testing.T) {
	candidate := Candidate{ID: "candidate", SupplierID: 9, SupplierName: "CodexAPIs", SourceGroup: "gptplus",
		SourceGroupName: "gptplus", SourceRatio: "0.12", SourceModels: 1, SourceSnapshotHash: "source-v1"}
	option := MatchOption{EquivalentCostRatio: "9", PricingBasis: "model_dimension_equivalent",
		PricingCoverageRatio: "1", OverlappingModels: []string{"gpt-5.6-sol"}, allOverlappingModels: []string{"gpt-5.6-sol"}}

	result := manualSourceOption(candidate, option, "0.15")
	if result.SourceRatio != "0.12" || result.EquivalentCostRatio != "9" {
		t.Fatalf("manual option ratio semantics were merged: %#v", result)
	}
	if result.RecommendedRuleType != "add" || result.RecommendedValue != "0.03" {
		t.Fatalf("manual recommendation used equivalent cost instead of real group ratio: %#v", result)
	}
}

func TestManualMaterializeCreatesEditsAndRejectsStaleEvidence(t *testing.T) {
	reader := &mutableTargetReader{catalog: targetCatalog("target-v1", "yinzi-plus", "0.08", []string{"gpt-test"}),
		channels: []domain.TargetChannel{{ID: 17, Name: "existing target channel", Status: 1, BaseURL: "https://upstream.example/v1",
			Group: "yinzi-plus", Models: []string{"gpt-test"}, ConfigHash: "channel-v1"}}}
	fixture := newMappingFixture(t, reader)
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "plus", "0.05", []string{"gpt-test"})
	preview, err := fixture.service.ManualPreview(context.Background(), ManualPreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	group, option := preview.Groups[0], preview.Groups[0].Options[0]
	selection := ManualMaterializeSelection{CandidateID: option.CandidateID, SupplierID: option.SupplierID,
		SourceGroup: option.SourceGroup, SourceSnapshotHash: option.SourceSnapshotHash, TargetGroup: group.TargetGroup,
		ConfirmManual: true, Rule: ManualRuleInput{RuleType: "add", Value: "0.03", ExecutionMode: "approval", Enabled: false}}
	created, err := fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err != nil || len(created.Policies) != 1 || !created.Policies[0].Created || created.Policies[0].Policy.Value != "0.03" {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if created.Policies[0].Policy.SourceBasis != "group_ratio" {
		t.Fatalf("manual policy source basis=%q, want group_ratio", created.Policies[0].Policy.SourceBasis)
	}

	current, err := fixture.service.ManualPreview(context.Background(), ManualPreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil || current.Groups[0].ExistingPolicy == nil || current.Groups[0].Status != "mapped" {
		t.Fatalf("current preview=%#v err=%v", current, err)
	}
	owner := current.Groups[0].ExistingPolicy
	currentOption := current.Groups[0].Options[0]
	selection = ManualMaterializeSelection{CandidateID: currentOption.CandidateID, SupplierID: currentOption.SupplierID,
		SourceGroup: currentOption.SourceGroup, SourceSnapshotHash: currentOption.SourceSnapshotHash, TargetGroup: current.Groups[0].TargetGroup,
		ExpectedPolicyID: owner.ID, ExpectedPolicyUpdatedAt: &owner.UpdatedAt, ConfirmManual: true,
		Rule: ManualRuleInput{Name: owner.Name, RuleType: "multiply", Value: "1.2", ExecutionMode: "approval", Enabled: false}}
	updated, err := fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: current.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err != nil || len(updated.Policies) != 1 || !updated.Policies[0].Updated || updated.Policies[0].Policy.RuleType != "multiply" {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}

	staleTime := owner.UpdatedAt.Add(-time.Second)
	selection.ExpectedPolicyUpdatedAt = &staleTime
	selection.Rule.Value = "1.3"
	_, err = fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: current.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err == nil || !IsConflict(err) {
		t.Fatalf("stale policy edit error=%v", err)
	}

	reader.channels[0].ConfigHash = "channel-v2"
	_, err = fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: current.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err == nil || !IsConflict(err) || !strings.Contains(err.Error(), "target catalog or channels changed") {
		t.Fatalf("stale target evidence error=%v", err)
	}
}

func TestManualMaterializeRequiresExplicitConfirmationAndAutoWriteAuthorization(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{
		catalog: targetCatalog("target-v1", "economy", "0.08", []string{"gpt-test"}),
		channels: []domain.TargetChannel{{ID: 7, Name: "existing channel", Status: 1, BaseURL: "https://upstream.example/v1",
			Group: "economy", Models: []string{"gpt-test"}, ConfigHash: "channel-v1"}},
	})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "economy", "0.05", []string{"gpt-test"})
	preview, err := fixture.service.ManualPreview(context.Background(), ManualPreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	group, option := preview.Groups[0], preview.Groups[0].Options[0]
	selection := ManualMaterializeSelection{CandidateID: option.CandidateID, SupplierID: option.SupplierID,
		SourceGroup: option.SourceGroup, SourceSnapshotHash: option.SourceSnapshotHash, TargetGroup: group.TargetGroup,
		Rule: ManualRuleInput{RuleType: "add", Value: "0.03", ExecutionMode: "approval"}}
	_, err = fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err == nil || !strings.Contains(err.Error(), "confirm_manual") {
		t.Fatalf("unconfirmed manual mapping error=%v", err)
	}
	selection.ConfirmManual = true
	selection.Rule.ExecutionMode = "auto"
	selection.Rule.Enabled = true
	_, err = fixture.service.ManualMaterialize(context.Background(), ManualMaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []ManualMaterializeSelection{selection}})
	if err == nil || !strings.Contains(err.Error(), "not authorized for automatic writes") {
		t.Fatalf("auto write gate error=%v", err)
	}
}
