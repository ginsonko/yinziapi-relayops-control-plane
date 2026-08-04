package mappingdraft

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type staticTargetReader struct {
	catalog  domain.CatalogSnapshot
	channels []domain.TargetChannel
	err      error
}

func (r staticTargetReader) Read(context.Context, domain.TargetSite) (domain.CatalogSnapshot, []domain.TargetChannel, error) {
	return r.catalog, append([]domain.TargetChannel(nil), r.channels...), r.err
}

type mappingFixture struct {
	store    *store.Store
	service  *Service
	target   domain.TargetSite
	supplier domain.Supplier
	now      time.Time
}

func TestPreviewAndMaterializeHighConfidenceDraft(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{
		catalog: targetCatalog("target-v1", "economy", "0.05", []string{"gpt-test", "claude-test"}),
		channels: []domain.TargetChannel{{ID: 7, BaseURL: "https://upstream.example/v1", Group: "economy",
			Models: []string{"gpt-test", "claude-test"}, ConfigHash: "channel-v1"}},
	})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "economy", "0.04", []string{"gpt-test", "claude-test"})

	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != 1 {
		t.Fatalf("candidates=%d", len(preview.Candidates))
	}
	candidate := preview.Candidates[0]
	if candidate.Status != "prefilled" || candidate.ProposedTargetGroup != "economy" || candidate.Policy == nil {
		t.Fatalf("candidate=%#v", candidate)
	}
	if candidate.Policy.RuleType != "add" || candidate.Policy.Value != "0.01" || candidate.Policy.ExecutionMode != "approval" || candidate.Policy.Enabled {
		t.Fatalf("policy draft=%#v", candidate.Policy)
	}
	if candidate.Policy.TargetSiteID != fixture.target.ID || candidate.Policy.SourceModelPattern != "*" {
		t.Fatalf("policy binding=%#v", candidate.Policy)
	}
	if policies, listErr := fixture.store.ListPolicies(context.Background()); listErr != nil || len(policies) != 0 {
		t.Fatalf("preview persisted policies=%d err=%v", len(policies), listErr)
	}

	request := MaterializeRequest{TargetSiteID: fixture.target.ID, TargetEvidenceHash: preview.TargetEvidenceHash,
		Selections: []MaterializeSelection{{CandidateID: candidate.ID, SupplierID: candidate.SupplierID,
			SourceGroup: candidate.SourceGroup, SourceSnapshotHash: candidate.SourceSnapshotHash, TargetGroup: candidate.ProposedTargetGroup}}}
	materialized, err := fixture.service.Materialize(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized.Policies) != 1 || !materialized.Policies[0].Created || materialized.Policies[0].Policy.Enabled ||
		materialized.Policies[0].Policy.ExecutionMode != "approval" {
		t.Fatalf("materialized=%#v", materialized)
	}

	replayed, err := fixture.service.Materialize(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Policies) != 1 || replayed.Policies[0].Created || !replayed.Policies[0].Idempotent ||
		replayed.Policies[0].Policy.ID != materialized.Policies[0].Policy.ID {
		t.Fatalf("replayed=%#v", replayed)
	}

	existingPreview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	existing := existingPreview.Candidates[0]
	if existing.Status != "already_mapped" || existing.ProposedTargetGroup != "economy" || existing.Score != existing.Options[0].Score || existing.Score == "0" {
		t.Fatalf("existing mapping score summary=%#v", existing)
	}
}

func TestHighConfidenceDraftKeepsModelEvidenceSeparateFromGroupRatioRule(t *testing.T) {
	target := targetCatalog("target-v1", "economy", "0.3", []string{"gpt-test"})
	target.Models = []domain.ModelOffer{{ExternalID: "gpt-test", PriceFunction: domain.PriceFunction{
		Currency: "USD", InputPer1M: "2", OutputPer1M: "4",
	}}}
	fixture := newMappingFixture(t, staticTargetReader{catalog: target, channels: []domain.TargetChannel{{
		ID: 7, BaseURL: "https://upstream.example/v1", Group: "economy", Models: []string{"gpt-test"},
	}}})
	source := domain.CatalogSnapshot{SupplierID: fixture.supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "0.1", ModelIDs: []string{"gpt-test"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-test", PriceFunction: domain.PriceFunction{
			Currency: "USD", InputPer1M: "2", OutputPer1M: "8",
		}}},
		Evidence: domain.Evidence{ResponseHash: "source-priced-v1", SourceType: "authenticated_api", ObservedAt: fixture.now}}
	if err := fixture.store.SaveSnapshot(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != 1 || preview.Candidates[0].Status != "prefilled" || preview.Candidates[0].Policy == nil {
		t.Fatalf("priced candidate=%#v", preview.Candidates)
	}
	option := preview.Candidates[0].Options[0]
	if option.EquivalentCostRatio != "0.2" || option.PricingBasis != "model_dimension_equivalent" ||
		!option.PricingImpactComplete || option.PricingCoverageRatio != "1" || preview.Candidates[0].Policy.Value != "0.2" {
		t.Fatalf("equivalent price option=%#v policy=%#v", option, preview.Candidates[0].Policy)
	}
	candidate := preview.Candidates[0]
	result, err := fixture.service.Materialize(context.Background(), MaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []MaterializeSelection{{CandidateID: candidate.ID,
			SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup, SourceSnapshotHash: candidate.SourceSnapshotHash,
			TargetGroup: candidate.ProposedTargetGroup}}})
	if err != nil || len(result.Policies) != 1 || !result.Policies[0].Created {
		t.Fatalf("priced materialization=%#v err=%v", result, err)
	}
	policy := result.Policies[0].Policy
	if policy.Value != "0.2" || policy.SourceBasis != "group_ratio" || policy.RuleType != "add" || policy.Enabled || policy.ExecutionMode != "approval" {
		t.Fatalf("priced materialized policy=%#v", policy)
	}
}

func TestExplicitButUnresolvedModelPricingDoesNotBlockGroupRatioDraft(t *testing.T) {
	target := targetCatalog("target-v1", "economy", "0.3", []string{"gpt-test"})
	target.Models = []domain.ModelOffer{{ExternalID: "gpt-test", PriceFunction: domain.PriceFunction{
		Currency: "USD", InputPer1M: "2",
	}}}
	fixture := newMappingFixture(t, staticTargetReader{catalog: target, channels: []domain.TargetChannel{{
		ID: 7, BaseURL: "https://upstream.example/v1", Group: "economy", Models: []string{"gpt-test"},
	}}})
	source := domain.CatalogSnapshot{SupplierID: fixture.supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "0.1", ModelIDs: []string{"gpt-test"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-test", PriceFunction: domain.PriceFunction{
			Currency: "USD", RequiresModelBasePrice: true,
		}}},
		Evidence: domain.Evidence{ResponseHash: "source-unresolved-v1", SourceType: "authenticated_api", ObservedAt: fixture.now}}
	if err := fixture.store.SaveSnapshot(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidate := preview.Candidates[0]
	if candidate.Status != "needs_confirmation" || candidate.Policy != nil || candidate.Options[0].EquivalentCostRatio != "" ||
		candidate.Options[0].PricingBasis != "model_dimension_unresolved" || !containsString(candidate.Reasons, "equivalent_cost_unresolved") {
		t.Fatalf("unresolved pricing candidate=%#v", candidate)
	}
	selection := MaterializeSelection{CandidateID: candidate.ID, SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup,
		SourceSnapshotHash: candidate.SourceSnapshotHash, TargetGroup: candidate.Options[0].TargetGroup, ConfirmLowConfidence: true}
	result, err := fixture.service.Materialize(context.Background(), MaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []MaterializeSelection{selection}})
	if err != nil || len(result.Policies) != 1 || result.Policies[0].Policy.SourceBasis != "group_ratio" {
		t.Fatalf("group-ratio draft was blocked by model pricing: %#v, %v", result, err)
	}
}

func TestLowConfidenceRequiresExplicitConfirmation(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{
		catalog:  targetCatalog("target-v1", "premium", "0.08", []string{"gpt-test"}),
		channels: []domain.TargetChannel{{ID: 8, BaseURL: "https://different.example/v1", Group: "premium", Models: []string{"gpt-test"}}},
	})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "economy", "0.04", []string{"gpt-test"})
	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidate := preview.Candidates[0]
	if candidate.Status != "needs_confirmation" || candidate.ProposedTargetGroup != "" || candidate.Policy != nil {
		t.Fatalf("low-confidence candidate=%#v", candidate)
	}
	selection := MaterializeSelection{CandidateID: candidate.ID, SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup,
		SourceSnapshotHash: candidate.SourceSnapshotHash, TargetGroup: "premium"}
	_, err = fixture.service.Materialize(context.Background(), MaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []MaterializeSelection{selection}})
	if err == nil || !strings.Contains(err.Error(), "confirm_low_confidence") {
		t.Fatalf("unconfirmed error=%v", err)
	}
	selection.ConfirmLowConfidence = true
	result, err := fixture.service.Materialize(context.Background(), MaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []MaterializeSelection{selection}})
	if err != nil || len(result.Policies) != 1 || !result.Policies[0].Created {
		t.Fatalf("confirmed result=%#v err=%v", result, err)
	}
}

func TestCrossSupplierTieDoesNotClaimTarget(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{
		catalog:  targetCatalog("target-v1", "economy", "0.05", []string{"gpt-test"}),
		channels: []domain.TargetChannel{{ID: 9, BaseURL: "https://unrelated.example/v1", Group: "economy", Models: []string{"gpt-test"}}},
	})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-a", "economy", "0.05", []string{"gpt-test"})
	second := domain.Supplier{Name: "second", BaseURL: "https://second.example", Adapter: "newapi", Enabled: true, PollIntervalSeconds: 300}
	if err := fixture.store.CreateSupplier(context.Background(), &second); err != nil {
		t.Fatal(err)
	}
	saveSupplierCatalog(t, fixture, second, "source-b", "economy", "0.05", []string{"gpt-test"})
	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != 2 || preview.Summary.Prefilled != 0 {
		t.Fatalf("summary=%#v candidates=%#v", preview.Summary, preview.Candidates)
	}
	for _, candidate := range preview.Candidates {
		if candidate.Status != "needs_confirmation" || candidate.TargetCompetition.Contenders != 2 ||
			!containsString(candidate.Reasons, "target_group_has_ambiguous_source_competition") {
			t.Fatalf("candidate=%#v", candidate)
		}
	}
}

func TestMaterializeRejectsStaleTargetEvidenceAndRollsBackOwnershipConflict(t *testing.T) {
	reader := &mutableTargetReader{catalog: targetCatalog("target-v1", "economy", "0.05", []string{"gpt-test"}),
		channels: []domain.TargetChannel{{ID: 7, BaseURL: "https://upstream.example/v1", Group: "economy", Models: []string{"gpt-test"}, ConfigHash: "channel-v1"}}}
	fixture := newMappingFixture(t, reader)
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "economy", "0.04", []string{"gpt-test"})
	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidate := preview.Candidates[0]
	reader.channels[0].ConfigHash = "channel-v2"
	_, err = fixture.service.Materialize(context.Background(), MaterializeRequest{TargetSiteID: fixture.target.ID,
		TargetEvidenceHash: preview.TargetEvidenceHash, Selections: []MaterializeSelection{{CandidateID: candidate.ID,
			SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup, SourceSnapshotHash: candidate.SourceSnapshotHash,
			TargetGroup: "economy"}}})
	if err == nil || !IsConflict(err) {
		t.Fatalf("stale target error=%v", err)
	}
	if policies, _ := fixture.store.ListPolicies(context.Background()); len(policies) != 0 {
		t.Fatalf("stale materialize wrote %d policies", len(policies))
	}
}

func TestMappingDraftPolicyBatchRollsBackOnOwnershipConflict(t *testing.T) {
	fixture := newMappingFixture(t, staticTargetReader{})
	owner := domain.PricingPolicy{Name: "owner", SupplierID: fixture.supplier.ID, SourceGroup: "owned-source",
		SourceModelPattern: "gpt-test", TargetSiteID: fixture.target.ID, TargetGroup: "owned-target", RuleType: "add",
		Value: "0.01", Enabled: false, ExecutionMode: "approval"}
	if err := fixture.store.CreatePolicy(context.Background(), &owner); err != nil {
		t.Fatal(err)
	}
	secondSupplier := domain.Supplier{Name: "other", BaseURL: "https://other.example", Adapter: "newapi", Enabled: true}
	if err := fixture.store.CreateSupplier(context.Background(), &secondSupplier); err != nil {
		t.Fatal(err)
	}
	batch := []domain.PricingPolicy{
		{Name: "would-be-created", SupplierID: fixture.supplier.ID, SourceGroup: "fresh-source", SourceModelPattern: "gpt-test",
			TargetSiteID: fixture.target.ID, TargetGroup: "fresh-target", RuleType: "add", Value: "0.01", ExecutionMode: "approval"},
		{Name: "conflict", SupplierID: secondSupplier.ID, SourceGroup: "other-source", SourceModelPattern: "gpt-test",
			TargetSiteID: fixture.target.ID, TargetGroup: "owned-target", RuleType: "add", Value: "0.01", ExecutionMode: "approval"},
	}
	_, err := fixture.store.CreateMappingDraftPolicies(context.Background(), batch)
	var ownership *store.MappingOwnershipConflictError
	if err == nil || !errors.As(err, &ownership) {
		t.Fatalf("ownership error=%v", err)
	}
	policies, err := fixture.store.ListPolicies(context.Background())
	if err != nil || len(policies) != 1 || policies[0].ID != owner.ID {
		t.Fatalf("batch was not rolled back: policies=%#v err=%v", policies, err)
	}
}

func TestPolicyPatternKeepsModelsOmittedFromPublicEvidence(t *testing.T) {
	models := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		models = append(models, fmt.Sprintf("gpt-test-%02d", index))
	}
	fixture := newMappingFixture(t, staticTargetReader{catalog: targetCatalog("target-v1", "economy", "0.05", models),
		channels: []domain.TargetChannel{{ID: 7, BaseURL: "https://upstream.example/v1", Group: "economy", Models: models}}})
	saveSupplierCatalog(t, fixture, fixture.supplier, "source-v1", "economy", "0.04", models)
	preview, err := fixture.service.Preview(context.Background(), PreviewRequest{TargetSiteID: fixture.target.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidate := preview.Candidates[0]
	if candidate.Policy == nil || len(candidate.Options[0].OverlappingModels) != 50 || candidate.Options[0].OverlappingModelsOmitted != 10 {
		t.Fatalf("public evidence truncation=%#v", candidate)
	}
	if candidate.Policy.SourceModelPattern != "*" {
		t.Fatalf("policy pattern=%q want wildcard group ownership", candidate.Policy.SourceModelPattern)
	}
}

func TestTargetEvidenceHashIgnoresResponseOrdering(t *testing.T) {
	firstCatalog := domain.CatalogSnapshot{Groups: []domain.GroupOffer{
		{ExternalID: "b", Name: "b", Ratio: "0.2", ModelIDs: []string{"m2", "m1"}},
		{ExternalID: "a", Name: "a", Ratio: "0.1", ModelIDs: []string{"m3"}},
	}}
	secondCatalog := domain.CatalogSnapshot{Groups: []domain.GroupOffer{
		{ExternalID: "a", Name: "a", Ratio: "0.1", ModelIDs: []string{"m3"}},
		{ExternalID: "b", Name: "b", Ratio: "0.2", ModelIDs: []string{"m1", "m2"}},
	}}
	firstChannels := []domain.TargetChannel{
		{ID: 9, BaseURL: "https://b.example/v1", Group: "b", Models: []string{"m2", "m1"}},
		{ID: 3, BaseURL: "https://a.example/v1", Group: "a", Models: []string{"m3"}},
	}
	secondChannels := []domain.TargetChannel{firstChannels[1], firstChannels[0]}
	if left, right := hashTargetEvidence(firstCatalog, firstChannels), hashTargetEvidence(secondCatalog, secondChannels); left != right {
		t.Fatalf("order changed evidence hash: %s != %s", left, right)
	}
}

func TestTargetEvidenceHashIncludesChannelSemantics(t *testing.T) {
	catalog := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "a", Name: "a", Ratio: "0.1", ModelIDs: []string{"m1"}}}}
	base := domain.TargetChannel{ID: 3, Name: "primary", Status: 1, BaseURL: "https://a.example/v1", Group: "a",
		Models: []string{"m1"}, Tag: "gpt", Remark: "preferred", ConfigHash: "config-v1"}
	want := hashTargetEvidence(catalog, []domain.TargetChannel{base})
	tests := map[string]func(*domain.TargetChannel){
		"name":   func(channel *domain.TargetChannel) { channel.Name = "renamed" },
		"status": func(channel *domain.TargetChannel) { channel.Status = 0 },
		"tag":    func(channel *domain.TargetChannel) { channel.Tag = "claude" },
		"remark": func(channel *domain.TargetChannel) { channel.Remark = "disabled upstream" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if got := hashTargetEvidence(catalog, []domain.TargetChannel{changed}); got == want {
				t.Fatalf("changing channel %s did not invalidate target evidence", name)
			}
		})
	}
}

type mutableTargetReader struct {
	catalog  domain.CatalogSnapshot
	channels []domain.TargetChannel
}

func (r *mutableTargetReader) Read(context.Context, domain.TargetSite) (domain.CatalogSnapshot, []domain.TargetChannel, error) {
	return r.catalog, append([]domain.TargetChannel(nil), r.channels...), nil
}

func newMappingFixture(t *testing.T, reader targetReader) mappingFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "target-admin", Kind: "newapi-admin", SiteURL: "https://target.example", Status: "active"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted-test-payload"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", Adapter: "newapi",
		CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi", Enabled: true, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	service := &Service{store: st, reader: reader, now: func() time.Time { return now }}
	return mappingFixture{store: st, service: service, target: target, supplier: supplier, now: now}
}

func saveSupplierCatalog(t *testing.T, fixture mappingFixture, supplier domain.Supplier, hash, group, ratio string, models []string) {
	t.Helper()
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: group, Name: group, Ratio: ratio, ModelIDs: models}},
		Evidence: domain.Evidence{ResponseHash: hash, SourceType: "authenticated_api", ObservedAt: fixture.now}}
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
}

func targetCatalog(hash, group, ratio string, models []string) domain.CatalogSnapshot {
	return domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: group, Name: group, Ratio: ratio, ModelIDs: models}},
		Evidence: domain.Evidence{ResponseHash: hash, SourceType: "authenticated_api", ObservedAt: time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)}}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
