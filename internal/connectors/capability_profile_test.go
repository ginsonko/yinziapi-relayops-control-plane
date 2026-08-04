package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCapabilityNegotiationStopsAfterCredentialAuthFailure(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	connector := NewNewAPI(server.Client(), nil)
	supplier := domain.Supplier{ID: 300, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{
		"newapi_customer_self_paths": `["/api/user/self","/api/user/balance"]`,
	}}
	session := newDiagnosticSession(supplier.ID)
	_, err := connector.negotiateEnvelope(context.Background(), session, server.Client(), supplier, server.URL,
		"customer_self", "customer_self", authCustomerSession, "", []string{"/api/user/self"},
		domain.CredentialPayload{Authorization: "Bearer expired-token", UserID: 42})
	if ErrorClass(err) != "auth_missing" {
		t.Fatalf("error=%v class=%q", err, ErrorClass(err))
	}
	if requests.Load() != 1 || len(session.diagnostics.Requests) != 1 {
		t.Fatalf("expired credential was replayed across candidates: requests=%d diagnostics=%#v", requests.Load(), session.diagnostics.Requests)
	}
}

func TestNewAPIBalanceNegotiationStopsAfterCredentialAuthFailure(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	credentialID := int64(301)
	supplier := domain.Supplier{ID: 301, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
		Metadata: map[string]string{"newapi_balance_paths": `["/api/user/self","/api/user/balance"]`}}
	connector := NewNewAPI(server.Client(), nil)
	session := newDiagnosticSession(supplier.ID)
	balance, _ := connector.fetchNegotiatedBalance(context.Background(), supplier, server.URL, server.Client(),
		domain.CredentialPayload{Authorization: "Bearer expired-token", UserID: 42}, authCustomerSession,
		newAPIUnitContract{Currency: "USD"}, session)
	if balance != nil {
		t.Fatalf("expired credential returned balance %#v", balance)
	}
	if requests.Load() != 1 || len(session.diagnostics.Requests) != 1 {
		t.Fatalf("expired balance credential was replayed across candidates: requests=%d diagnostics=%#v",
			requests.Load(), session.diagnostics.Requests)
	}
}

func TestNewAPIUsageNegotiationStopsAfterCredentialAuthFailure(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	credentialID := int64(302)
	supplier := domain.Supplier{ID: 302, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
		Metadata: map[string]string{"newapi_usage_paths": `["/api/data/self","/api/user/usage"]`}}
	connector := NewNewAPI(server.Client(), nil)
	session := newDiagnosticSession(supplier.ID)
	_, err := connector.fetchNegotiatedNewAPIUsage24h(context.Background(), supplier, server.Client(), server.URL,
		domain.CredentialPayload{Authorization: "Bearer expired-token", UserID: 42}, authCustomerSession,
		newAPIUnitContract{Currency: "USD"}, session)
	if ErrorClass(err) != "auth_missing" {
		t.Fatalf("usage error=%v class=%q", err, ErrorClass(err))
	}
	if requests.Load() != 1 || len(session.diagnostics.Requests) != 1 {
		t.Fatalf("expired usage credential was replayed across candidates: requests=%d diagnostics=%#v",
			requests.Load(), session.diagnostics.Requests)
	}
}

func TestNewAPICustomerCapabilitiesRemainIndependent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"currency":"USD"}}`)
		case "/api/user/self":
			http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
		case "/api/user/balance":
			fmt.Fprint(response, `{"success":true,"data":{"balance":12,"currency":"USD"}}`)
		case "/api/channel/":
			http.Error(response, `{"success":false,"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 201, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{
			"newapi_balance_path": "/api/user/balance",
		},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=valid", UserID: 42})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_self") != "expired_or_invalid" {
		t.Fatalf("customer_self = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "balance") != "verified" || !diagnostics.BalanceSucceeded {
		t.Fatalf("balance = %#v", diagnostics)
	}
	if capabilityStatus(diagnostics, authSiteAdmin, "admin_read") != "denied" {
		t.Fatalf("admin = %#v", diagnostics.Capabilities)
	}
}

func TestNewAPIMetadataCustomPathAndHeadersAreAuthoritative(t *testing.T) {
	t.Parallel()
	var defaultModels atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{}}`)
		case "/custom/models":
			if request.Header.Get("X-API-Key") != "custom-secret" || request.Header.Get("X-User-ID") != "88" || request.Header.Get("Authorization") != "" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(response, `{"object":"list","data":[{"id":"model-a"}]}`)
		case "/v1/models", "/api/models":
			defaultModels.Add(1)
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 202, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{
			"newapi_catalog_path":        "/custom/models",
			"newapi_catalog_auth_header": "X-API-Key",
			"newapi_catalog_auth_scheme": "raw",
			"newapi_catalog_user_header": "X-User-ID",
		},
	}, "api-key", domain.CredentialPayload{Authorization: "Bearer custom-secret", UserID: 88})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if !diagnostics.CatalogSucceeded || defaultModels.Load() != 0 {
		t.Fatalf("custom catalog was not authoritative: defaults=%d diagnostics=%#v", defaultModels.Load(), diagnostics)
	}
}

func TestNewAPICapabilityProfileIsReusedWithoutSecrets(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests[request.URL.Path]++
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{}}`)
		case "/api/models":
			fmt.Fprint(response, `{"object":"list","data":[{"id":"cached-model"}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	connector := NewNewAPI(server.Client(), nil)
	supplier := domain.Supplier{ID: 203, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{}}
	payload := domain.CredentialPayload{Authorization: "Bearer cache-secret"}
	if _, err := connector.ValidateCredential(context.Background(), supplier, "api-key", payload); err != nil {
		t.Fatalf("first validation: %v", err)
	}
	mu.Lock()
	firstDefault := requests["/v1/models"]
	firstCached := requests["/api/models"]
	mu.Unlock()
	if _, err := connector.ValidateCredential(context.Background(), supplier, "api-key", payload); err != nil {
		t.Fatalf("second validation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests["/v1/models"] != firstDefault || requests["/api/models"] != firstCached+1 {
		t.Fatalf("cached selection was not reused first: requests=%v", requests)
	}
	formatted := fmt.Sprintf("%#v", connector.profiles)
	if strings.Contains(formatted, "cache-secret") {
		t.Fatalf("capability profile leaked credential material: %s", formatted)
	}
}

func TestSub2BusinessCatalogSurvivesUsageAuthorizationFailure(t *testing.T) {
	t.Parallel()
	var defaultModels atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/health":
			fmt.Fprint(response, `{"status":"ok"}`)
		case "/custom/models":
			if request.Header.Get("X-API-Key") != "sub2-secret" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(response, `{"object":"list","data":[{"id":"model-b"}]}`)
		case "/custom/usage":
			http.Error(response, `{}`, http.StatusUnauthorized)
		case "/v1/models", "/api/v1/models":
			defaultModels.Add(1)
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 204, BaseURL: server.URL, Adapter: "sub2api_upstream", Metadata: map[string]string{
			"sub2api_mode":                "upstream",
			"sub2api_catalog_path":        "/custom/models",
			"sub2api_catalog_auth_header": "X-API-Key",
			"sub2api_catalog_auth_scheme": "raw",
			"sub2api_usage_path":          "/custom/usage",
		},
	}, "api-key", domain.CredentialPayload{Authorization: "sub2-secret"})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if !diagnostics.CatalogSucceeded || capabilityStatus(diagnostics, authBusinessAPIKey, "catalog") != "verified" {
		t.Fatalf("catalog = %#v", diagnostics)
	}
	if capabilityStatus(diagnostics, authBusinessAPIKey, "usage") != "expired_or_invalid" || defaultModels.Load() != 0 {
		t.Fatalf("usage isolation/default path = %#v defaults=%d", diagnostics.Capabilities, defaultModels.Load())
	}
}

func TestNewAPICollectionUsesNegotiatedPathsAndAuthHeaders(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	var defaultCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/custom/pricing":
			_, _ = response.Write(pricing)
		case "/custom/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/custom/balance":
			if request.Header.Get("X-API-Key") != "collection-secret" || request.Header.Get("Authorization") != "" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(response, `{"success":true,"data":{"quota":5000000,"currency":"quota"}}`)
		case "/custom/usage":
			if request.Header.Get("X-API-Key") != "collection-secret" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(response, `{"success":true,"data":[{"quota":500000,"count":2}]}`)
		case "/api/pricing", "/api/status", "/api/user/self", "/api/data/self":
			defaultCalls.Add(1)
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing",
		domain.CredentialPayload{Authorization: "Bearer collection-secret"})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 206, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
		Metadata: map[string]string{
			"newapi_pricing_path":        "/custom/pricing",
			"newapi_status_path":         "/custom/status",
			"newapi_balance_path":        "/custom/balance",
			"newapi_usage_path":          "/custom/usage",
			"newapi_auth_header":         "X-API-Key",
			"newapi_auth_scheme":         "raw",
			"newapi_balance_auth_scheme": "raw",
		},
	})
	if err != nil {
		t.Fatalf("CollectDetailed: %v", err)
	}
	if defaultCalls.Load() != 0 || !diagnostics.CatalogSucceeded || !diagnostics.BalanceSucceeded {
		t.Fatalf("defaults/diagnostics: defaults=%d diagnostics=%#v", defaultCalls.Load(), diagnostics)
	}
	if snapshot.Evidence.SourceURL != server.URL+"/custom/pricing" {
		t.Fatalf("source URL = %q", snapshot.Evidence.SourceURL)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "10" || snapshot.Balance.DailySpend != "1" || snapshot.Balance.UsageSource != "/custom/usage" {
		t.Fatalf("balance = %#v", snapshot.Balance)
	}
}

func TestAnonymousSub2HealthProbeFallsBackAfterProtectedPath(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/health":
			http.Error(response, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		case "/api/health":
			fmt.Fprint(response, `{"status":"ok"}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	connector := NewSub2API(server.Client(), nil)
	session := newDiagnosticSession(206)
	err := connector.probeHealthCandidates(context.Background(), session, server.Client(), domain.Supplier{
		ID: 206, BaseURL: server.URL, Adapter: "sub2api_upstream", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, server.URL)
	if err != nil || requests.Load() != 2 {
		t.Fatalf("anonymous health fallback requests=%d err=%v diagnostics=%#v", requests.Load(), err, session.result())
	}
}

func TestSub2CustomerSessionCollectsCatalogRatesBalanceAndUsage(t *testing.T) {
	t.Parallel()
	var businessCatalogCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer customer-token" {
			http.Error(response, `{"code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/channels/available":
			fmt.Fprint(response, `{"code":0,"data":[{"name":"Primary","description":"","platforms":[{"platform":"anthropic","groups":[{"id":7,"name":"Economy","platform":"anthropic","subscription_type":"standard","rate_multiplier":0.08}],"supported_models":[{"name":"claude-test","platform":"anthropic","pricing":{"billing_mode":"token","input_price":0.000001,"output_price":0.000005,"cache_read_price":0.0000001,"per_request_price":null,"intervals":[]}}]}]}]}`)
		case "/api/v1/groups/available":
			fmt.Fprint(response, `{"code":0,"data":[{"id":7,"name":"Economy","description":"low cost","platform":"anthropic","subscription_type":"standard","rate_multiplier":0.08,"peak_rate_enabled":false}]}`)
		case "/api/v1/groups/rates":
			fmt.Fprint(response, `{"code":0,"data":{"7":0.05}}`)
		case "/api/v1/user/profile":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":20,"frozen_balance":2,"status":"active"}}`)
		case "/api/v1/usage/dashboard/stats":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":1.25,"total_actual_cost":8.5,"today_requests":3,"total_requests":40,"today_tokens":1000}}`)
		case "/v1/models", "/api/v1/models":
			businessCatalogCalls.Add(1)
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing",
		domain.CredentialPayload{Authorization: "Bearer customer-token"})
	snapshot, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 205, BaseURL: server.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err != nil {
		t.Fatalf("CollectDetailed: %v", err)
	}
	if businessCatalogCalls.Load() != 0 || !diagnostics.CatalogSucceeded || !diagnostics.BalanceSucceeded {
		t.Fatalf("role isolation/diagnostics: business=%d diagnostics=%#v", businessCatalogCalls.Load(), diagnostics)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].Ratio != "0.05" || len(snapshot.Models) != 1 {
		t.Fatalf("catalog = groups:%#v models:%#v", snapshot.Groups, snapshot.Models)
	}
	if snapshot.Models[0].PriceFunction.InputPer1M != "1" || snapshot.Models[0].PriceFunction.OutputPer1M != "5" {
		t.Fatalf("price function = %#v", snapshot.Models[0].PriceFunction)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "20" || snapshot.Balance.Available != "18" || snapshot.Balance.DailySpend != "1.25" {
		t.Fatalf("balance = %#v", snapshot.Balance)
	}
	if snapshot.Evidence.SourceURL != server.URL+"/api/v1/channels/available" || snapshot.Evidence.SourceType != "authenticated_customer_session" {
		t.Fatalf("evidence = %#v", snapshot.Evidence)
	}
}

func TestSub2CustomerSessionFallsBackToActiveKeyModelCatalog(t *testing.T) {
	t.Parallel()
	const groupAKey = "sub2-secret-group-a"
	const groupBKey = "sub2-secret-group-b"
	const disabledKey = "sub2-secret-disabled"
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/models" {
			modelCalls.Add(1)
			switch request.Header.Get("Authorization") {
			case "Bearer " + groupAKey:
				fmt.Fprint(response, `{"object":"list","data":[{"id":"claude-a"},{"id":"claude-shared"}]}`)
			case "Bearer " + groupBKey:
				fmt.Fprint(response, `{"object":"list","data":[{"id":"gpt-b"}]}`)
			default:
				http.Error(response, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			}
			return
		}
		if request.Header.Get("Authorization") != "Bearer customer-token" {
			http.Error(response, `{"code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/channels/available":
			fmt.Fprint(response, `{"code":0,"data":[]}`)
		case "/api/v1/groups/available":
			fmt.Fprint(response, `{"code":0,"data":[{"id":7,"name":"Economy","platform":"anthropic","rate_multiplier":0.05},{"id":8,"name":"Plus","platform":"openai","rate_multiplier":0.08},{"id":9,"name":"No active key","platform":"openai","rate_multiplier":0.1}]}`)
		case "/api/v1/groups/rates":
			fmt.Fprint(response, `{"code":0,"data":{}}`)
		case "/api/v1/keys":
			fmt.Fprintf(response, `{"code":0,"data":{"items":[{"key":%q,"status":"active","group_id":7},{"key":%q,"status":"active","group_id":8},{"key":%q,"status":"disabled","group_id":9}],"total":3,"page":1,"page_size":100,"pages":1}}`, groupAKey, groupBKey, disabledKey)
		case "/api/v1/user/profile":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":36.69,"frozen_balance":0,"status":"active"}}`)
		case "/api/v1/usage/dashboard/stats":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":0.25,"today_requests":2}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing",
		domain.CredentialPayload{Authorization: "Bearer customer-token"})
	snapshot, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 209, BaseURL: server.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err != nil {
		t.Fatalf("CollectDetailed: %v diagnostics=%#v", err, diagnostics)
	}
	if !diagnostics.CatalogSucceeded || !diagnostics.BalanceSucceeded || modelCalls.Load() != 2 {
		t.Fatalf("fallback diagnostics=%#v model_calls=%d", diagnostics, modelCalls.Load())
	}
	if len(snapshot.Groups) != 3 || len(snapshot.Models) != 3 || snapshot.Balance == nil || snapshot.Balance.Amount != "36.69" {
		t.Fatalf("fallback snapshot groups=%#v models=%#v balance=%#v", snapshot.Groups, snapshot.Models, snapshot.Balance)
	}
	groupModels := make(map[string][]string)
	for _, group := range snapshot.Groups {
		groupModels[group.Name] = group.ModelIDs
	}
	if len(groupModels["Economy"]) != 2 || len(groupModels["Plus"]) != 1 || len(groupModels["No active key"]) != 0 {
		t.Fatalf("group model coverage=%#v", groupModels)
	}
	encoded, err := json.Marshal(struct {
		Snapshot    domain.CatalogSnapshot
		Diagnostics domain.CollectionDiagnostics
	}{snapshot, diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{groupAKey, groupBKey, disabledKey} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("customer API key leaked into result: %s", encoded)
		}
	}
}

func TestSub2CustomerSessionCollectsWhenOptionalChannelsCapabilityIsDisabled(t *testing.T) {
	t.Parallel()
	const customerToken = "current-customer-session"
	const groupKey = "current-business-key"
	var channelCalls atomic.Int64
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/models" {
			modelCalls.Add(1)
			if request.Header.Get("Authorization") != "Bearer "+groupKey {
				http.Error(response, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(response, `{"object":"list","data":[{"id":"gpt-current"}]}`)
			return
		}
		if request.URL.Path == "/health" {
			fmt.Fprint(response, `{"status":"ok"}`)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+customerToken {
			http.Error(response, `{"code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/channels/available":
			channelCalls.Add(1)
			http.Error(response, `{"code":403,"message":"available channels disabled"}`, http.StatusForbidden)
		case "/api/v1/groups/available":
			fmt.Fprint(response, `{"code":0,"data":[{"id":7,"name":"Economy","platform":"openai","rate_multiplier":0.08}]}`)
		case "/api/v1/groups/rates":
			fmt.Fprint(response, `{"code":0,"data":{"7":0.05}}`)
		case "/api/v1/auth/me":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":61.93,"frozen_balance":0,"status":"active"}}`)
		case "/api/v1/usage":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":0.31,"today_requests":4}}`)
		case "/api/v1/keys":
			fmt.Fprintf(response, `{"code":0,"data":{"items":[{"key":%q,"status":"active","group_id":7}],"total":1,"page":1,"page_size":100,"pages":1}}`, groupKey)
		default:
			// Some Sub2API forks authenticate before routing removed legacy
			// endpoints. A 401 here must not hide the current endpoint.
			http.Error(response, `{"code":401,"message":"legacy endpoint unavailable"}`, http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "password_login",
		domain.CredentialPayload{Authorization: "Bearer " + customerToken})
	snapshot, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 210, BaseURL: server.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err != nil {
		t.Fatalf("CollectDetailed with disabled optional channels: %v diagnostics=%#v", err, diagnostics)
	}
	if channelCalls.Load() != 1 || modelCalls.Load() != 1 {
		t.Fatalf("bounded fallback calls: channels=%d models=%d", channelCalls.Load(), modelCalls.Load())
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].Ratio != "0.05" || len(snapshot.Models) != 1 || snapshot.Models[0].Name != "gpt-current" {
		t.Fatalf("degraded catalog = groups:%#v models:%#v", snapshot.Groups, snapshot.Models)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "61.93" || snapshot.Balance.DailySpend != "0.31" {
		t.Fatalf("current balance/usage = %#v", snapshot.Balance)
	}
	if !diagnostics.CatalogSucceeded || !diagnostics.BalanceSucceeded {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if !strings.Contains(strings.Join(snapshot.Evidence.Warnings, " | "), "channels") {
		t.Fatalf("optional capability degradation was not disclosed: %#v", snapshot.Evidence.Warnings)
	}
}

func TestSub2CustomerBalanceRemainsAvailableWhenCatalogFails(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer customer-token" {
			http.Error(response, `{"code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/user/profile":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":20,"frozen_balance":2,"status":"active"}}`)
		case "/api/v1/usage/dashboard/stats":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":1.25,"today_requests":3}}`)
		case "/api/v1/channels/available":
			http.Error(response, `{"code":503,"message":"catalog unavailable"}`, http.StatusServiceUnavailable)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing",
		domain.CredentialPayload{Authorization: "Bearer customer-token"})
	snapshot, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 207, BaseURL: server.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err == nil {
		t.Fatal("catalog failure unexpectedly succeeded")
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "20" || snapshot.Balance.Available != "18" ||
		snapshot.Balance.DailySpend != "1.25" || !diagnostics.BalanceSucceeded {
		t.Fatalf("independent balance was lost: snapshot=%#v diagnostics=%#v err=%v", snapshot, diagnostics, err)
	}
	if diagnostics.CatalogSucceeded {
		t.Fatalf("failed catalog was reported as successful: %#v", diagnostics)
	}
}

func TestSub2AutoFallbackPreservesCustomerBalanceWhenCatalogFails(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/admin/groups/all":
			http.Error(response, `{"code":403,"message":"Forbidden"}`, http.StatusForbidden)
		case "/api/v1/user/profile":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":20,"frozen_balance":2,"status":"active"}}`)
		case "/api/v1/usage/dashboard/stats":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":1.25,"today_requests":3}}`)
		case "/api/v1/channels/available":
			http.Error(response, `{"code":503,"message":"catalog unavailable"}`, http.StatusServiceUnavailable)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing",
		domain.CredentialPayload{Authorization: "Bearer customer-token"})
	snapshot, diagnostics, err := NewSub2API(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 208, BaseURL: server.URL, Adapter: "sub2api", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "auto"},
	})
	if err == nil {
		t.Fatal("auto mode catalog failure unexpectedly succeeded")
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "20" || snapshot.Balance.DailySpend != "1.25" ||
		!diagnostics.BalanceSucceeded {
		t.Fatalf("auto fallback lost independent balance: snapshot=%#v diagnostics=%#v err=%v", snapshot, diagnostics, err)
	}
}

func TestCapabilityMetadataRejectsCrossOriginAndStaysBounded(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"https://evil.example/models", "//evil.example/models", "\r\nX-Evil: 1"} {
		if _, ok := safeCapabilityPath(value); ok {
			t.Fatalf("unsafe path was accepted: %q", value)
		}
	}
	supplier := domain.Supplier{Metadata: map[string]string{
		"newapi_catalog_paths": `["/a","/b","/c","/d","/e","/f","/g"]`,
	}}
	selections := capabilitySelections(newCapabilityProfileCache(), supplier, "newapi", "https://safe.example",
		"catalog", authBusinessAPIKey, "", nil, domain.CredentialPayload{Authorization: "Bearer bounded-secret"})
	if len(selections) == 0 || len(selections) > maxCapabilityAttempts {
		t.Fatalf("selection count = %d", len(selections))
	}
	encoded, err := json.Marshal(selections)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "bounded-secret") {
		t.Fatalf("selection metadata leaked a secret: %s", encoded)
	}
}
