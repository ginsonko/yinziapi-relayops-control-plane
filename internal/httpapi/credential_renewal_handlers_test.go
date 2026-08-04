package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
)

func TestCredentialRenewalEndpointsConfigureTriggerAndHideSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"renewed-http-token"}}`)
	}))
	defer upstream.Close()
	st := openHTTPTestStore(t)
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	service := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "HTTP renewal", Kind: "token", SiteURL: upstream.URL}
	if err := service.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer old-http-token"}); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "HTTP upstream", BaseURL: upstream.URL, Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	refresher := credentials.NewRefresher(st, service, upstream.Client(), time.Minute).WithCandidateValidator(
		func(context.Context, domain.Supplier, domain.Credential, domain.CredentialPayload) error { return nil })
	handler := New(Dependencies{Store: st, Credentials: service, Refresher: refresher, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	configureBody := `{"enabled":true,"username":"http-user","password":"http-password","rule":{"url":"/api/user/login","method":"POST","content_type":"application/json","username_field":"username","password_field":"password","token_json_path":"data.token"}}`
	configured := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/credentials/%d/renewal", credential.ID), configureBody)
	if configured.Code != http.StatusOK || !strings.Contains(configured.Body.String(), `"username_configured":true`) {
		t.Fatalf("configure status=%d body=%s", configured.Code, configured.Body.String())
	}
	assertHTTPBodyHidesCredentialSecrets(t, configured.Body.String())

	status := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/credentials/%d/renewal", credential.ID), "")
	list := authenticatedRequest(handler, http.MethodGet, "/api/v1/credentials", "")
	for _, response := range []*httptest.ResponseRecorder{status, list} {
		if response.Code != http.StatusOK {
			t.Fatalf("read status=%d body=%s", response.Code, response.Body.String())
		}
		assertHTTPBodyHidesCredentialSecrets(t, response.Body.String())
	}

	triggered := authenticatedRequest(handler, http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/renewal/trigger", credential.ID), "")
	if triggered.Code != http.StatusOK || !strings.Contains(triggered.Body.String(), `"state":"healthy"`) {
		t.Fatalf("trigger status=%d body=%s", triggered.Code, triggered.Body.String())
	}
	assertHTTPBodyHidesCredentialSecrets(t, triggered.Body.String())
	_, renewed, err := service.Load(context.Background(), credential.ID)
	if err != nil || !strings.Contains(renewed.Authorization, "renewed-http-token") {
		t.Fatalf("renewed payload authorization=%q err=%v", renewed.Authorization, err)
	}

	removed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/credentials/%d/renewal", credential.ID), "")
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"state":"unconfigured"`) {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}
	assertHTTPBodyHidesCredentialSecrets(t, removed.Body.String())
}

func TestCredentialRenewalPresetEndpointUsesSupplierAdapterAndReturnsNoSecrets(t *testing.T) {
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	tests := []struct {
		adapter, family, path, tokenPath, successValue string
	}{
		{adapter: "newapi-native", family: "newapi", path: "/api/user/login"},
		{adapter: "sub2api_admin", family: "sub2api", path: "/api/v1/auth/login", tokenPath: "data.access_token", successValue: "0"},
	}
	for _, test := range tests {
		supplier := domain.Supplier{Name: test.adapter, BaseURL: "https://" + strings.ReplaceAll(test.adapter, "_", "-") + ".example", Adapter: test.adapter, Enabled: true}
		if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
			t.Fatal(err)
		}
		response := authenticatedRequest(handler, http.MethodGet,
			fmt.Sprintf("/api/v1/suppliers/%d/credential-renewal-preset", supplier.ID), "")
		if response.Code != http.StatusOK {
			t.Fatalf("adapter %s status=%d body=%s", test.adapter, response.Code, response.Body.String())
		}
		body := response.Body.String()
		for _, expected := range []string{`"supported":true`, `"adapter_family":"` + test.family + `"`,
			`"url":"` + test.path + `"`} {
			if !strings.Contains(body, expected) {
				t.Fatalf("adapter %s missing %s: %s", test.adapter, expected, body)
			}
		}
		if test.tokenPath != "" && !strings.Contains(body, `"token_json_path":"`+test.tokenPath+`"`) {
			t.Fatalf("adapter %s token mapping: %s", test.adapter, body)
		}
		if test.successValue != "" && !strings.Contains(body, `"success_value":"`+test.successValue+`"`) {
			t.Fatalf("adapter %s success mapping: %s", test.adapter, body)
		}
		for _, secret := range []string{`"username":`, `"password":`, `"authorization":`, `"cookie":`, "admin-secret"} {
			if strings.Contains(strings.ToLower(body), secret) {
				t.Fatalf("preset leaked secret-bearing value %q: %s", secret, body)
			}
		}
	}

	custom := domain.Supplier{Name: "custom", BaseURL: "https://custom.example", Adapter: "json", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &custom); err != nil {
		t.Fatal(err)
	}
	unsupported := authenticatedRequest(handler, http.MethodGet,
		fmt.Sprintf("/api/v1/suppliers/%d/credential-renewal-preset", custom.ID), "")
	if unsupported.Code != http.StatusOK || !strings.Contains(unsupported.Body.String(), `"supported":false`) || strings.Contains(unsupported.Body.String(), `"rule"`) {
		t.Fatalf("unsupported preset status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
}

func authenticatedRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertHTTPBodyHidesCredentialSecrets(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{"http-user", "http-password", "old-http-token", "renewed-http-token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("HTTP response leaked %q: %s", secret, body)
		}
	}
}
