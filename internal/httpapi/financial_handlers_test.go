package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestIntelligenceLedgerFinancialDashboardAndSecretReferenceGuard(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := domain.Credential{Name: "financial-api", Kind: "access-token", SiteURL: "https://financial-api.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "financial-api", BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin"})

	event := store.OperatingLedgerEvent{EventID: "recharge-1", TargetSiteID: target.ID, OccurredAt: time.Now().UTC(), EventKind: "recharge",
		Amount: "25", AmountEvidence: "exact", Currency: "CNY", Source: "newapi", SubjectUserID: "user-1", SubjectRole: "user"}
	body, _ := json.Marshal(map[string]any{"events": []store.OperatingLedgerEvent{event}})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/intelligence/ledger", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ledger PUT status=%d body=%s", response.Code, response.Body.String())
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/api/v1/intelligence", nil)
	dashboardRequest.Header.Set("X-RelayOps-Token", "admin")
	dashboardResponse := httptest.NewRecorder()
	handler.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", dashboardResponse.Code, dashboardResponse.Body.String())
	}
	var envelope struct {
		Data intelligence.Dashboard `json:"data"`
	}
	if err := json.Unmarshal(dashboardResponse.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Financials.IncludedEventCount != 1 || len(envelope.Data.Financials.ByCurrency) != 1 ||
		envelope.Data.Financials.ByCurrency[0].RechargeGross != "25.000000" {
		t.Fatalf("financial dashboard = %#v", envelope.Data.Financials)
	}

	event.EventID = "secret-ref"
	event.SubjectTokenID = "s" + "k-" + "fixture-secret-shaped-token-id"
	body, _ = json.Marshal(map[string]any{"events": []store.OperatingLedgerEvent{event}})
	secretRequest := httptest.NewRequest(http.MethodPut, "/api/v1/intelligence/ledger", bytes.NewReader(body))
	secretRequest.Header.Set("Content-Type", "application/json")
	secretRequest.Header.Set("X-RelayOps-Token", "admin")
	secretResponse := httptest.NewRecorder()
	handler.ServeHTTP(secretResponse, secretRequest)
	if secretResponse.Code != http.StatusBadRequest {
		t.Fatalf("secret token reference status=%d body=%s", secretResponse.Code, secretResponse.Body.String())
	}
}

func TestIntelligenceConfigPreservesExplicitFalseExclusions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin"})
	body := []byte(`{"financial_lookback_days":60,"ledger_exclusions":{"exclude_administrators":false,"exclude_test_users":false,"exclude_internal_tokens":false,"excluded_user_segments":[" Automation ","automation"]}}`)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/intelligence/config", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("config PUT status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data intelligence.Config `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	policy := envelope.Data.LedgerExclusions
	if policy == nil || policy.ExcludeAdministrators || policy.ExcludeTestUsers || policy.ExcludeInternalTokens ||
		len(policy.ExcludedUserSegments) != 1 || policy.ExcludedUserSegments[0] != "automation" {
		t.Fatalf("explicit exclusions were not preserved: %#v", envelope.Data)
	}
}
