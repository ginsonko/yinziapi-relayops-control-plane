package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestValidateNewAPIBrowserSessionProvesCustomerWithoutClaimingAdmin(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			_, _ = response.Write([]byte(`{"success":true,"data":{"currency":"USD"}}`))
		case "/api/user/self":
			if request.Header.Get("Cookie") != "session=valid" || request.Header.Get("New-Api-User") != "42" {
				http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write([]byte(`{"success":true,"data":{"id":42,"quota":100}}`))
		case "/api/channel/":
			http.Error(response, `{"success":false,"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 71, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=valid", LocalStorage: map[string]string{"uid": "42"}})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if capabilityStatus(diagnostics, authAnonymous, "status") != "verified" || capabilityStatus(diagnostics, authCustomerSession, "customer_self") != "verified" {
		t.Fatalf("capabilities = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authSiteAdmin, "admin_read") != "denied" {
		t.Fatalf("admin capability was not kept distinct: %#v", diagnostics.Capabilities)
	}
	if !diagnostics.BalanceSucceeded {
		t.Fatalf("customer self capability was not reflected: %#v", diagnostics)
	}
}

func TestValidateNewAPISelfSuccessDoesNotClaimFailedBalance(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			_, _ = response.Write([]byte(`{"success":true,"data":{"currency":"USD"}}`))
		case "/api/user/self":
			_, _ = response.Write([]byte(`{"success":true,"data":{"id":42,"username":"reader"}}`))
		case "/api/user/balance", "/api/user/quota":
			http.Error(response, `{"success":false,"message":"balance unavailable"}`, http.StatusServiceUnavailable)
		case "/api/channel/":
			http.Error(response, `{"success":false,"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 76, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=valid", UserID: 42})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_self") != "verified" {
		t.Fatalf("customer self capability = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "balance") == "verified" {
		t.Fatalf("balance capability was falsely verified: %#v", diagnostics.Capabilities)
	}
	if diagnostics.BalanceSucceeded {
		t.Fatalf("self success was incorrectly reported as balance success: %#v", diagnostics)
	}
}

func TestValidateNewAPIManagementCredentialRequiresAdminRead(t *testing.T) {
	t.Parallel()
	var customerRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			_, _ = response.Write([]byte(`{"success":true,"data":{"quota_display_type":"USD"}}`))
		case "/api/channel/":
			if request.Header.Get("Cookie") != "session=admin" || request.Header.Get("New-Api-User") != "17" {
				http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write([]byte(`{"success":true,"data":{"items":[],"total":0}}`))
		case "/api/user/self":
			customerRequests.Add(1)
			http.Error(response, `{"success":false,"message":"Forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 74, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{},
	}, "newapi-management", domain.CredentialPayload{Cookie: "session=admin", UserID: 17})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if customerRequests.Load() != 0 {
		t.Fatalf("management credential was probed as a customer session")
	}
	if capabilityStatus(diagnostics, authSiteAdmin, "admin_read") != "verified" {
		t.Fatalf("admin capability = %#v", diagnostics.Capabilities)
	}
}

func TestValidateNewAPIBrowserSessionRejectsMissingUIDWithoutConsumingRetries(t *testing.T) {
	t.Parallel()
	var selfRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/status" {
			_, _ = response.Write([]byte(`{"success":true,"data":{}}`))
			return
		}
		if request.URL.Path == "/api/user/self" {
			selfRequests.Add(1)
			http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()

	diagnostics, err := NewNewAPI(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 72, BaseURL: server.URL, Adapter: "newapi", Metadata: map[string]string{},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=stale"})
	if err == nil || !strings.Contains(err.Error(), "New-Api-User") {
		t.Fatalf("missing UID error = %v", err)
	}
	// customer_self and balance are independent capabilities. Both may use the
	// same legacy endpoint, but neither individual probe is retried after 401.
	if selfRequests.Load() != 2 {
		t.Fatalf("401 validation request count = %d, want 2 independent capability probes", selfRequests.Load())
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_self") != "expired_or_invalid" {
		t.Fatalf("capabilities = %#v", diagnostics.Capabilities)
	}
}

func TestValidateSub2BrowserSessionDoesNotBecomeBusinessAPIKey(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/health" {
			_, _ = response.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Error(response, `{"code":"API_KEY_REQUIRED"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 73, BaseURL: server.URL, Adapter: "sub2api_upstream", PricingPath: "/v1/models", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=web-only"})
	if err == nil || !strings.Contains(err.Error(), "customer session") {
		t.Fatalf("browser pairing error = %v", err)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_catalog") != "expired_or_invalid" {
		t.Fatalf("capabilities = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authBusinessAPIKey, "catalog") != "" {
		t.Fatalf("business key was falsely claimed: %#v", diagnostics.Capabilities)
	}
}

func TestValidateSub2BusinessAPIKeyUsesReadOnlyModelList(t *testing.T) {
	t.Parallel()
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requested = append(requested, request.Method+" "+request.URL.Path)
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/health":
			_, _ = response.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			if request.Header.Get("Authorization") != "Bearer upstream-key" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write([]byte(`{"object":"list","data":[{"id":"model-a"}]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 74, BaseURL: server.URL, Adapter: "sub2api_upstream", PricingPath: "/v1/models", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, "api-key", domain.CredentialPayload{Authorization: "upstream-key", AuthorizationMode: "bearer"})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if capabilityStatus(diagnostics, authBusinessAPIKey, "catalog") != "verified" || !diagnostics.CatalogSucceeded {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	for _, request := range requested {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			t.Fatalf("credential validation made a mutating request: %v", requested)
		}
	}
}

func TestValidateSub2CustomerSessionUsesCurrentIdentityAndGroupContract(t *testing.T) {
	t.Parallel()
	const token = "current-customer-token"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/health" {
			fmt.Fprint(response, `{"status":"ok"}`)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(response, `{}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/auth/me":
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"balance":61.93,"status":"active"}}`)
		case "/api/v1/groups/available":
			fmt.Fprint(response, `{"code":0,"data":[{"id":7,"name":"Economy","rate_multiplier":0.08}]}`)
		case "/api/v1/usage":
			fmt.Fprint(response, `{"code":0,"data":{"today_actual_cost":0.1,"today_requests":1}}`)
		case "/api/v1/channels/available":
			http.Error(response, `{"code":403,"message":"feature disabled"}`, http.StatusForbidden)
		default:
			http.Error(response, `{"code":401,"message":"legacy endpoint unavailable"}`, http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 76, BaseURL: server.URL, Adapter: "sub2api_upstream", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, "password_login", domain.CredentialPayload{Authorization: "Bearer " + token})
	if err != nil {
		t.Fatalf("ValidateCredential current Sub2API contract: %v diagnostics=%#v", err, diagnostics)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_identity") != "verified" ||
		capabilityStatus(diagnostics, authCustomerSession, "customer_groups") != "verified" {
		t.Fatalf("current capabilities = %#v", diagnostics.Capabilities)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_channels") == "verified" {
		t.Fatalf("disabled optional channels were falsely verified: %#v", diagnostics.Capabilities)
	}
}

func TestValidateSub2CustomerIdentityDoesNotRequireBalance(t *testing.T) {
	t.Parallel()
	const token = "identity-without-balance"
	var legacyIdentityRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/health" {
			fmt.Fprint(response, `{"status":"ok"}`)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(response, `{}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v1/auth/me":
			fmt.Fprint(response, `{"code":"0","data":{"user":{"id":"9","email":"owner@example.test"},"status":"active"}}`)
		case "/api/v1/groups/available":
			fmt.Fprint(response, `{"code":0,"data":[{"id":7,"name":"Economy","rate_multiplier":"0.08"}]}`)
		case "/api/v1/user/profile", "/api/v1/user":
			legacyIdentityRequests.Add(1)
			http.Error(response, `{"code":401,"message":"legacy endpoint unavailable"}`, http.StatusUnauthorized)
		default:
			http.Error(response, `{"code":403,"message":"optional capability disabled"}`, http.StatusForbidden)
		}
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 77, BaseURL: server.URL, Adapter: "sub2api_upstream", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, "password_login", domain.CredentialPayload{Authorization: "Bearer " + token})
	if err != nil {
		t.Fatalf("identity-only login should remain valid: %v diagnostics=%#v", err, diagnostics)
	}
	if legacyIdentityRequests.Load() != 0 {
		t.Fatalf("successful authoritative identity was followed by %d legacy probes", legacyIdentityRequests.Load())
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_identity") != "verified" ||
		capabilityStatus(diagnostics, authCustomerSession, "customer_balance") == "verified" || diagnostics.BalanceSucceeded {
		t.Fatalf("identity/balance capabilities = %#v", diagnostics)
	}
}

func TestValidateSub2CustomerIdentityAllowsUnavailableOptionalCapabilities(t *testing.T) {
	t.Parallel()
	const token = "identity-only-customer"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/health" {
			fmt.Fprint(response, `{"status":"ok"}`)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(response, `{"code":401,"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if request.URL.Path == "/api/v1/auth/me" {
			fmt.Fprint(response, `{"code":0,"data":{"id":9,"email":"owner@example.test","status":"active"}}`)
			return
		}
		http.Error(response, `{"code":403,"message":"optional capability disabled"}`, http.StatusForbidden)
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 78, BaseURL: server.URL, Adapter: "sub2api_upstream", Metadata: map[string]string{"sub2api_mode": "upstream"},
	}, "password_login", domain.CredentialPayload{Authorization: "Bearer " + token})
	if err != nil {
		t.Fatalf("identity-only login should be accepted: %v diagnostics=%#v", err, diagnostics)
	}
	if capabilityStatus(diagnostics, authCustomerSession, "customer_identity") != "verified" {
		t.Fatalf("identity capability = %#v", diagnostics.Capabilities)
	}
	if diagnostics.CatalogSucceeded || diagnostics.BalanceSucceeded {
		t.Fatalf("optional capabilities were falsely promoted: %#v", diagnostics)
	}
}

func TestParseSub2CustomerProfileAcceptsNestedNumericStrings(t *testing.T) {
	t.Parallel()
	balance, err := parseSub2CustomerProfile([]byte(`{"code":0,"data":{"user":{"id":"9"},"wallet":{"balance":"61.9300","frozen_balance":"1.25","currency":"usd"}}}`), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if balance.Amount != "61.93" || balance.Available != "60.68" || balance.Currency != "USD" || balance.SourceField != "wallet.balance" {
		t.Fatalf("nested balance = %#v", balance)
	}
}

func TestValidateSub2AdminUsesBoundedAdminRead(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/health":
			_, _ = response.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/admin/groups/all":
			if request.Header.Get("Cookie") != "session=admin" {
				http.Error(response, `{}`, http.StatusUnauthorized)
				return
			}
			_, _ = response.Write([]byte(`{"code":0,"data":[{"id":1,"name":"main"}]}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	diagnostics, err := NewSub2API(server.Client(), nil).ValidateCredential(context.Background(), domain.Supplier{
		ID: 75, BaseURL: server.URL, Adapter: "sub2api_admin", Metadata: map[string]string{"sub2api_mode": "admin"},
	}, "browser_pairing", domain.CredentialPayload{Cookie: "session=admin"})
	if err != nil {
		t.Fatalf("ValidateCredential: %v", err)
	}
	if capabilityStatus(diagnostics, authSiteAdmin, "admin_groups") != "verified" {
		t.Fatalf("capabilities = %#v", diagnostics.Capabilities)
	}
}

func TestDiagnosticMessageSanitizerRedactsCredentialAndQueryMaterial(t *testing.T) {
	t.Parallel()
	message := `request https://upstream.example/path?token=query-secret failed Authorization: Bearer top.secret Cookie=session=hidden; API-Key=fixture-api-key-value`
	sanitized := sanitizeDiagnosticMessage(message)
	for _, secret := range []string{"query-secret", "top.secret", "session=hidden", "fixture-api-key-value"} {
		if strings.Contains(sanitized, secret) {
			t.Fatalf("sanitizer leaked %q in %q", secret, sanitized)
		}
	}
	if !strings.Contains(sanitized, "https://upstream.example/path") || strings.Contains(sanitized, "?") {
		t.Fatalf("sanitized URL = %q", sanitized)
	}
}
