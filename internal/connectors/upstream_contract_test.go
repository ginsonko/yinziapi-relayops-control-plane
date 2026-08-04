package connectors

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNewAPIPublicPricingContractAndStableCatalogHash(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("testdata/newapi_public_pricing.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/pricing" {
			http.NotFound(response, request)
			return
		}
		marker := fmt.Sprintf("volatile-%d", calls.Add(1))
		body := bytes.Replace(fixture, []byte("volatile-response-marker"), []byte(marker), 1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(body)
	}))
	defer server.Close()

	connector := NewNewAPI(server.Client(), nil)
	supplier := domain.Supplier{ID: 12, BaseURL: server.URL, Adapter: "newapi", PricingPath: "/api/pricing"}
	first, err := connector.Collect(context.Background(), supplier)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	second, err := connector.Collect(context.Background(), supplier)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if first.Evidence.ResponseHash == "" || first.Evidence.ResponseHash != second.Evidence.ResponseHash {
		t.Fatalf("volatile envelope changed catalog hash: %q != %q", first.Evidence.ResponseHash, second.Evidence.ResponseHash)
	}
	if len(first.Groups) != 3 || len(first.Models) != 2 {
		t.Fatalf("catalog sizes: groups=%d models=%d", len(first.Groups), len(first.Models))
	}
	economy := findGroupByName(t, first.Groups, "economy")
	if economy.Ratio != "0.05" || len(economy.ModelIDs) != 1 || economy.ModelIDs[0] != "gpt-5.6-sol" {
		t.Fatalf("economy group = %#v", economy)
	}
	image := findModelByName(t, first.Models, "gpt-image-2")
	if image.BillingMode != "per_call" || image.ModelPrice != "0.15" || image.PriceFunction.PerRequest != "0.15" {
		t.Fatalf("per-call model = %#v", image)
	}
}

func TestNewAPICatalogHashIgnoresChangingAuthenticatedBalance(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("testdata/newapi_public_pricing.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var balanceCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/status" {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`))
			return
		}
		if request.Header.Get("Authorization") != "Bearer paired-token" {
			http.Error(response, "missing credential", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(fixture)
		case "/api/user/self":
			quota := 500000 - balanceCalls.Add(1)*1000
			_, _ = fmt.Fprintf(response, `{"data":{"quota":%d,"currency":"quota"},"success":true}`, quota)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newSub2TestCredential(t, server.URL, domain.CredentialPayload{Authorization: "Bearer paired-token"})
	connector := NewNewAPI(server.Client(), credentialService)
	supplier := domain.Supplier{ID: 13, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID}
	first, err := connector.Collect(context.Background(), supplier)
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	second, err := connector.Collect(context.Background(), supplier)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if first.Balance == nil || second.Balance == nil || first.Balance.Amount == second.Balance.Amount {
		t.Fatalf("balance did not change between samples: first=%#v second=%#v", first.Balance, second.Balance)
	}
	if first.Evidence.ResponseHash != second.Evidence.ResponseHash {
		t.Fatalf("balance changed catalog hash: %s != %s", first.Evidence.ResponseHash, second.Evidence.ResponseHash)
	}
}

func TestNewAPILoginRestrictedErrorsRequestCredentialInsteadOfReportingHealthy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"success":false,"message":"Unauthorized"}`))
	}))
	defer server.Close()

	_, err := NewNewAPI(server.Client(), nil).Collect(context.Background(), domain.Supplier{BaseURL: server.URL, Adapter: "newapi"})
	if err == nil || !strings.Contains(err.Error(), "browser-paired session or API access token") {
		t.Fatalf("credential guidance error = %v", err)
	}
}

func TestNewAPILoginHTMLIsNotDecodedAsPricing(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = response.Write([]byte("<!doctype html><title>Sign in</title>"))
	}))
	defer server.Close()

	_, err := NewNewAPI(server.Client(), nil).Collect(context.Background(), domain.Supplier{BaseURL: server.URL, Adapter: "newapi"})
	if err == nil || !strings.Contains(err.Error(), "returned a login page") {
		t.Fatalf("login page error = %v", err)
	}
}

func TestSub2APIUpstreamAuthFailureExplainsSupportedCredentialPaths(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"code":"API_KEY_REQUIRED","message":"API key required"}`))
	}))
	defer server.Close()

	_, err := NewSub2API(server.Client(), nil).Collect(context.Background(), domain.Supplier{
		BaseURL: server.URL, Adapter: "sub2api_upstream", PricingPath: "/v1/models",
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err == nil || !strings.Contains(err.Error(), "attach an API Key") {
		t.Fatalf("credential guidance error = %v", err)
	}
}

func TestFactoryDoesNotMistakeHTMLHealthFallbackForSub2API(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health":
			response.Header().Set("Content-Type", "text/html")
			_, _ = response.Write([]byte("<!doctype html><title>SPA fallback</title>"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	_, _, err := NewFactory(server.Client(), nil).ForSupplier(context.Background(), domain.Supplier{BaseURL: server.URL, Adapter: "auto"})
	if err == nil || !strings.Contains(err.Error(), "unable to identify upstream type") {
		t.Fatalf("auto detection error = %v", err)
	}
}

func TestFactoryRecognizesSub2APIJSONHealthContract(t *testing.T) {
	t.Parallel()
	healthFixture, err := os.ReadFile("testdata/sub2_health.json")
	if err != nil {
		t.Fatalf("read health fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(healthFixture)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	connector, adapter, err := NewFactory(server.Client(), nil).ForSupplier(context.Background(), domain.Supplier{BaseURL: server.URL, Adapter: "auto"})
	if err != nil || adapter != "sub2api" || connector.Name() != "sub2api" {
		t.Fatalf("detected connector=%v adapter=%q err=%v", connector, adapter, err)
	}
}

func TestFactoryRecognizesNewAPIStatusContract(t *testing.T) {
	t.Parallel()
	statusFixture, err := os.ReadFile("testdata/newapi_status.json")
	if err != nil {
		t.Fatalf("read status fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/status" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(statusFixture)
	}))
	defer server.Close()

	connector, adapter, err := NewFactory(server.Client(), nil).ForSupplier(context.Background(), domain.Supplier{BaseURL: server.URL, Adapter: "auto"})
	if err != nil || adapter != "newapi" || connector.Name() != "newapi" {
		t.Fatalf("detected connector=%v adapter=%q err=%v", connector, adapter, err)
	}
}

func findGroupByName(t *testing.T, groups []domain.GroupOffer, name string) domain.GroupOffer {
	t.Helper()
	for _, group := range groups {
		if group.Name == name {
			return group
		}
	}
	t.Fatalf("group %q not found", name)
	return domain.GroupOffer{}
}

func findModelByName(t *testing.T, models []domain.ModelOffer, name string) domain.ModelOffer {
	t.Helper()
	for _, model := range models {
		if model.Name == name {
			return model
		}
	}
	t.Fatalf("model %q not found", name)
	return domain.ModelOffer{}
}
