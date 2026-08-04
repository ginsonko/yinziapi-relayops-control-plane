package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestOpportunityEndpointUsesCachedTargetAndObservedDemand(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "target", Kind: "newapi-management", SiteURL: "https://target.example.test"}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer test", UserID: 1}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "production", BaseURL: "https://target.example.test", Adapter: "newapi",
		CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	targetCatalog := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "default", Name: "default", Ratio: "1", ModelIDs: []string{"gpt-test"}}},
		Models:   []domain.ModelOffer{{ExternalID: "gpt-test", Name: "gpt-test", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2"}}},
		Evidence: domain.Evidence{Confidence: 95, ObservedAt: now, ResponseHash: "target-catalog"}}
	if err := st.SaveOpportunityTargetCatalog(context.Background(), target.ID, targetCatalog); err != nil {
		t.Fatal(err)
	}

	current := domain.Supplier{Name: "current", BaseURL: "https://current.example.test", Adapter: "newapi", Enabled: true, LastStatus: "healthy"}
	candidate := domain.Supplier{Name: "candidate", BaseURL: "https://candidate.example.test", Adapter: "newapi", Enabled: true, LastStatus: "healthy"}
	baselineCandidate := domain.Supplier{Name: "baseline candidate", BaseURL: "https://baseline.example.test", Adapter: "newapi", Enabled: true, LastStatus: "healthy"}
	if err := st.CreateSupplier(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSupplier(context.Background(), &candidate); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSupplier(context.Background(), &baselineCandidate); err != nil {
		t.Fatal(err)
	}
	currentSnapshot := opportunityFixtureSnapshot(current.ID, "standard", "0.5", "current", now)
	candidateSnapshot := opportunityFixtureSnapshot(candidate.ID, "promo", "0.2", "candidate", now)
	baselineCandidateSnapshot := opportunityFixtureSnapshot(baselineCandidate.ID, "baseline-promo", "0.1", "baseline-candidate", now)
	if err := st.SaveSnapshot(context.Background(), currentSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSnapshot(context.Background(), candidateSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSnapshot(context.Background(), baselineCandidateSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDiscoveries(context.Background(), currentSnapshot, true); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDiscoveries(context.Background(), candidateSnapshot, false); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDiscoveries(context.Background(), baselineCandidateSnapshot, true); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "current route", SupplierID: current.ID, SourceGroup: "standard", SourceModelPattern: "*",
		TargetSiteID: target.ID, TargetGroup: "default", RuleType: "multiply", Value: "1", Enabled: true, ExecutionMode: "approval"}
	if err := st.CreatePolicy(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOpportunityUsage(context.Background(), []store.OpportunityUsage{
		{TargetSiteID: target.ID, ModelID: "gpt-test", WindowDays: 7, DemandWeight: "70", DemandUnit: "million_tokens", Source: "test", Confidence: "0.95", ObservedAt: now},
		{TargetSiteID: target.ID, ModelID: "gpt-test", WindowDays: 30, DemandWeight: "300", DemandUnit: "million_tokens", Source: "test", Confidence: "0.95", ObservedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	handler := New(Dependencies{Store: st, Credentials: credentialService, StaticDir: t.TempDir(), AdminToken: "admin"})
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/opportunities?target_id=%d", target.ID), nil)
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Summary opportunitySummary `json:"summary"`
			Items   []struct {
				SupplierID     int64  `json:"supplier_id"`
				Classification string `json:"classification"`
				UsesPrior      bool   `json:"uses_prior"`
				DailyProfit    string `json:"expected_daily_profit"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Summary.Recommended != 1 || len(envelope.Data.Items) != 1 || envelope.Data.Items[0].SupplierID != candidate.ID ||
		envelope.Data.Items[0].Classification != "recommended" || envelope.Data.Items[0].UsesPrior || envelope.Data.Items[0].DailyProfit == "" {
		t.Fatalf("response=%s", response.Body.String())
	}
}

func opportunityFixtureSnapshot(supplierID int64, group, ratio, hash string, observedAt time.Time) domain.CatalogSnapshot {
	return domain.CatalogSnapshot{SupplierID: supplierID,
		Groups:   []domain.GroupOffer{{ExternalID: group, Name: group, Ratio: ratio, ModelIDs: []string{"gpt-test"}}},
		Models:   []domain.ModelOffer{{ExternalID: "gpt-test", Name: "gpt-test", PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "1"}}},
		Evidence: domain.Evidence{Confidence: 95, ObservedAt: observedAt, ResponseHash: hash}}
}
