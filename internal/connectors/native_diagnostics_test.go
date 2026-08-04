package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestNewAPIDynamicQuotaUnitAndPublicEvidenceWithCredential(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	status := readConnectorFixture(t, "newapi_status_quota.json")
	self := readConnectorFixture(t, "newapi_self_quota.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			_, _ = response.Write(status)
		case "/api/user/self":
			if request.Header.Get("Authorization") != "Bearer paired-token" || request.Header.Get("New-Api-User") != "42" {
				http.Error(response, `{"success":false}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write(self)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{
		Authorization: "Bearer paired-token", UserID: 42, Cookie: "session=paired",
	})
	connector := NewNewAPI(server.Client(), credentialService)
	snapshot, diagnostics, err := connector.CollectDetailed(context.Background(), domain.Supplier{
		ID: 1, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatalf("CollectDetailed: %v", err)
	}
	if snapshot.Evidence.SourceType != "public_api" {
		t.Fatalf("catalog source = %q, want public_api", snapshot.Evidence.SourceType)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "61.93" || snapshot.Balance.Currency != "USD" {
		t.Fatalf("normalized balance = %#v", snapshot.Balance)
	}
	if snapshot.Balance.RawValue != "30965000" || snapshot.Balance.RawUnit != "quota" || snapshot.Balance.NormalizationDivisor != "500000" {
		t.Fatalf("raw balance evidence = %#v", snapshot.Balance)
	}
	if !strings.Contains(snapshot.Balance.NormalizationFormula, "30965000") || !strings.Contains(snapshot.Balance.NormalizationFormula, "61.93 USD") {
		t.Fatalf("normalization formula = %q", snapshot.Balance.NormalizationFormula)
	}
	if !diagnostics.CatalogSucceeded || !diagnostics.BalanceSucceeded {
		t.Fatalf("diagnostics success flags = %#v", diagnostics)
	}
	assertRequestEvidence(t, diagnostics.Requests, "catalog", authAnonymous, "success")
	assertRequestEvidence(t, diagnostics.Requests, "catalog", authCustomerSession, "success")
	assertRequestEvidence(t, diagnostics.Requests, "status", authAnonymous, "success")
	assertRequestEvidence(t, diagnostics.Requests, "balance", authCustomerSession, "success")
}

func TestNewAPIQuotaWithoutUnitRemainsUnknownAndPreservesRawValue(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			_, _ = response.Write([]byte(`{"success":true,"data":{"currency":"USD"}}`))
		case "/api/user/self":
			_, _ = response.Write([]byte(`{"success":true,"data":{"quota":30965000,"currency":"quota"}}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 2, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "" || snapshot.Balance.Currency != "" || snapshot.Balance.RawValue != "30965000" || snapshot.Balance.Confidence != "invalid_unit" {
		t.Fatalf("unknown-unit balance = %#v", snapshot.Balance)
	}
	if diagnostics.BalanceSucceeded {
		t.Fatalf("unknown-unit balance incorrectly marked successful: %#v", diagnostics)
	}
}

func TestNewAPIAuthenticatedCatalogRequiresEvidence(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			if request.Header.Get("Authorization") == "" {
				http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write(pricing)
		case "/api/status":
			_, _ = response.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`))
		case "/api/user/self":
			_, _ = response.Write([]byte(`{"success":true,"data":{"balance":10,"currency":"USD"}}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Authorization: "Bearer paired", UserID: 9})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 3, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Evidence.SourceType != "authenticated_api" {
		t.Fatalf("catalog source = %q", snapshot.Evidence.SourceType)
	}
	if capabilityStatus(diagnostics, authAnonymous, "catalog") != "missing" || capabilityStatus(diagnostics, authCustomerSession, "catalog") != "verified" {
		t.Fatalf("capability evidence = %#v", diagnostics.Capabilities)
	}
}

func TestNewAPIMissingUserIDHasPreciseDKBYGuidance(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	_, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 4, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err == nil || !strings.Contains(err.Error(), "New-Api-User") {
		t.Fatalf("missing uid guidance = %v", err)
	}
	if countRequests(diagnostics.Requests, "catalog", authAnonymous) != 1 || countRequests(diagnostics.Requests, "catalog", authCustomerSession) != 1 {
		t.Fatalf("401 was retried or capability requests missing: %#v", diagnostics.Requests)
	}
}

func TestNewAPIRetriesConnectionResetWithStructuredDiagnostics(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	var pricingAttempts atomic.Int64
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/pricing" && pricingAttempts.Add(1) <= 2 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		body := pricing
		if request.URL.Path == "/api/status" {
			body = []byte(`{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}
	snapshot, diagnostics, err := NewNewAPI(client, nil).CollectDetailed(context.Background(), domain.Supplier{ID: 5, BaseURL: "https://reset.example", Adapter: "newapi"})
	if err != nil || len(snapshot.Models) == 0 {
		t.Fatalf("collection after reset = models:%d err:%v", len(snapshot.Models), err)
	}
	if countRequests(diagnostics.Requests, "catalog", authAnonymous) != 3 {
		t.Fatalf("catalog attempts = %#v", diagnostics.Requests)
	}
	failed := 0
	for _, item := range diagnostics.Requests {
		if item.EndpointType == "catalog" && item.ErrorClass == "connection_reset" {
			failed++
		}
		if strings.Contains(item.SourceURL, "?") || strings.Contains(item.ErrorMessage, "reset.example") {
			t.Fatalf("diagnostic leaked URL details: %#v", item)
		}
	}
	if failed != 2 {
		t.Fatalf("connection reset evidence count = %d", failed)
	}
}

func TestSub2BrowserSessionDoesNotMasqueradeAsBiuBusinessKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		http.Error(response, `{"code":"API_KEY_REQUIRED"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	_, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 6, BaseURL: server.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		PricingPath: "/v1/models", Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err == nil || !strings.Contains(err.Error(), "business API Key") {
		t.Fatalf("browser-session guidance = %v", err)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_catalog") != "expired_or_invalid" {
		t.Fatalf("capabilities = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authBusinessAPIKey, "catalog") != "" {
		t.Fatalf("business API key was falsely verified: %#v", diagnostics.Capabilities)
	}
}

func TestNineSiteCapabilityFixtureIsCompleteAndBounded(t *testing.T) {
	t.Parallel()
	var matrix []struct {
		Name            string `json:"name"`
		Adapter         string `json:"adapter"`
		CatalogPath     string `json:"catalog_path"`
		CatalogRole     string `json:"catalog_role"`
		PublicCatalog   bool   `json:"public_catalog"`
		BalanceRole     string `json:"balance_role"`
		CurrentBoundary string `json:"current_boundary"`
	}
	decodeFixtureJSON(t, "nine_site_capabilities.json", &matrix)
	if len(matrix) != 9 {
		t.Fatalf("site count = %d, want 9", len(matrix))
	}
	seen := make(map[string]bool)
	for _, site := range matrix {
		if site.Name == "" || seen[site.Name] || site.CatalogPath == "" || site.CatalogRole == "" || site.BalanceRole == "" || site.CurrentBoundary == "" {
			t.Fatalf("incomplete or duplicate site capability: %#v", site)
		}
		if site.Adapter != "newapi" && site.Adapter != "sub2api_upstream" {
			t.Fatalf("unbounded adapter in fixture: %#v", site)
		}
		if site.PublicCatalog != (site.CatalogRole == authAnonymous) {
			t.Fatalf("public catalog role mismatch: %#v", site)
		}
		seen[site.Name] = true
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newConnectorCredential(t *testing.T, siteURL, kind string, payload domain.CredentialPayload) (*credentials.Service, int64) {
	t.Helper()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	service := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "connector test", Kind: kind, SiteURL: siteURL}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatal(err)
	}
	return service, credential.ID
}

func readConnectorFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

func decodeFixtureJSON(t *testing.T, name string, target any) {
	t.Helper()
	body := readConnectorFixture(t, name)
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func assertRequestEvidence(t *testing.T, requests []domain.RequestEvidence, endpointType, capability, status string) {
	t.Helper()
	for _, item := range requests {
		if item.EndpointType == endpointType && item.AuthCapability == capability && item.Status == status {
			if item.ObservedAt.IsZero() || item.SourceURL == "" || item.ResponseHash == "" {
				t.Fatalf("incomplete request evidence: %#v", item)
			}
			return
		}
	}
	t.Fatalf("request evidence %s/%s/%s not found: %#v", endpointType, capability, status, requests)
}

func capabilityStatus(diagnostics domain.CollectionDiagnostics, capability, endpointType string) string {
	for _, item := range diagnostics.Capabilities {
		if item.Capability == capability && item.EndpointType == endpointType {
			return item.Status
		}
	}
	return ""
}

func countRequests(requests []domain.RequestEvidence, endpointType, capability string) int {
	count := 0
	for _, item := range requests {
		if item.EndpointType == endpointType && item.AuthCapability == capability {
			count++
		}
	}
	return count
}
