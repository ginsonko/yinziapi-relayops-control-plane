package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/collector"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func TestConsoleBootstrapOnEmptyDatabaseReturnsStableEmptyCollections(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK || !bytes.Contains(health.Body.Bytes(), []byte(`"api_contract_version":"`+apiContractVersion+`"`)) {
		t.Fatalf("health contract status=%d body=%s", health.Code, health.Body.String())
	}
	if health.Header().Get(apiContractHeader) != apiContractVersion {
		t.Fatalf("health contract header = %q", health.Header().Get(apiContractHeader))
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/console/bootstrap", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/console/bootstrap", nil)
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	request.Header.Set(apiContractHeader, apiContractVersion)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("security headers = %#v", response.Header())
	}

	staleRequest := httptest.NewRequest(http.MethodPost, "/api/v1/suppliers", bytes.NewBufferString(`{"name":"must-not-write","base_url":"https://example.com","adapter":"newapi","enabled":true}`))
	staleRequest.Header.Set("Content-Type", "application/json")
	staleRequest.Header.Set("X-RelayOps-Token", "admin-secret")
	staleRequest.Header.Set(apiContractHeader, "relayops-v3-stale")
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusConflict || !bytes.Contains(staleResponse.Body.Bytes(), []byte("请重新加载")) {
		t.Fatalf("stale console status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	suppliers, err := st.ListSuppliers(context.Background())
	if err != nil || len(suppliers) != 0 {
		t.Fatalf("stale console mutated suppliers: %#v, error=%v", suppliers, err)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Metrics struct {
				SupplierCount  int `json:"supplierCount"`
				PendingChanges int `json:"pendingChanges"`
			} `json:"metrics"`
			Suppliers   []json.RawMessage `json:"suppliers"`
			WatchItems  []json.RawMessage `json:"watchItems"`
			Policies    []json.RawMessage `json:"policies"`
			Balances    []json.RawMessage `json:"balances"`
			Discoveries []json.RawMessage `json:"discoveries"`
			Proposals   []json.RawMessage `json:"proposals"`
			Connectors  []json.RawMessage `json:"connectors"`
			Credentials []json.RawMessage `json:"credentials"`
			Activities  []json.RawMessage `json:"activities"`
			Runtime     consoleRuntime    `json:"runtime"`
			GeneratedAt string            `json:"generatedAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode bootstrap: %v; body=%s", err, response.Body.String())
	}
	if !envelope.Success || envelope.Data.Metrics.SupplierCount != 0 || envelope.Data.Metrics.PendingChanges != 0 {
		t.Fatalf("empty metrics = %#v", envelope)
	}
	if envelope.Data.Suppliers == nil || envelope.Data.WatchItems == nil || envelope.Data.Policies == nil || envelope.Data.Balances == nil ||
		envelope.Data.Discoveries == nil || envelope.Data.Proposals == nil || envelope.Data.Credentials == nil || envelope.Data.Activities == nil {
		t.Fatalf("bootstrap emitted null instead of []: %s", response.Body.String())
	}
	if len(envelope.Data.Connectors) != 2 {
		t.Fatalf("built-in connectors = %d, want 2; body=%s", len(envelope.Data.Connectors), response.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, envelope.Data.GeneratedAt); err != nil {
		t.Fatalf("generatedAt = %q: %v", envelope.Data.GeneratedAt, err)
	}
	if envelope.Data.Runtime.StartedAt == "" || envelope.Data.Runtime.UptimeSeconds < 0 ||
		envelope.Data.Runtime.DatabaseBytes <= 0 || envelope.Data.Runtime.ProcessMemoryBytes <= 0 ||
		envelope.Data.Runtime.Targets == nil {
		t.Fatalf("runtime projection = %#v", envelope.Data.Runtime)
	}
}

func TestConsoleBootstrapRejectsUnknownScopeInsteadOfRunningFullProjection(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/console/bootstrap?scope=everything", nil)
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "不支持的控制台数据范围") {
		t.Fatalf("unknown scope status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConsoleBootstrapProjectsExactSupplierCredentialID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	credential := domain.Credential{Name: "bound-browser", Kind: "browser-pairing", SiteURL: "https://same-origin.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "AIZZZ", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}

	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/console/bootstrap", nil)
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Suppliers []struct {
				ID           string  `json:"id"`
				CredentialID *string `json:"credentialId"`
			} `json:"suppliers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode bootstrap: %v; body=%s", err, response.Body.String())
	}
	if len(envelope.Data.Suppliers) != 1 || envelope.Data.Suppliers[0].CredentialID == nil ||
		*envelope.Data.Suppliers[0].CredentialID != fmt.Sprint(credential.ID) {
		t.Fatalf("supplier credential projection = %#v, want exact credential %d", envelope.Data.Suppliers, credential.ID)
	}
}

func TestOverviewBootstrapNeverReadsLiveTargetSnapshots(t *testing.T) {
	t.Parallel()
	var targetRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		http.Error(w, "periodic target read is forbidden", http.StatusTeapot)
	}))
	defer upstream.Close()

	ctx := context.Background()
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
	credential := domain.Credential{Name: "target", Kind: "access-token", SiteURL: upstream.URL}
	if err := credentialService.Create(ctx, &credential, domain.CredentialPayload{Authorization: "Bearer test"}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: upstream.URL, CredentialID: credential.ID, Adapter: "newapi", Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "supplier", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "policy", SupplierID: supplier.ID, SourceGroup: "source", SourceModelPattern: "*",
		TargetSiteID: target.ID, TargetGroup: "target", RuleType: "add", Value: "0", Enabled: true, ExecutionMode: "approval"}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, Credentials: credentialService, Targets: targets.NewFactory(upstream.Client(), credentialService),
		StaticDir: directory, AdminToken: "admin", RunwayHours: 48})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/console/bootstrap?scope=overview", nil)
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("overview bootstrap status=%d body=%s", response.Code, response.Body.String())
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("overview bootstrap made %d live target requests", targetRequests.Load())
	}
}

func TestBrowserPairingCreatesCredentialOnceAndStartsImmediateCollection(t *testing.T) {
	t.Parallel()
	var pricingRequest = make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/pricing":
			if request.Header.Get("Authorization") != "" {
				select {
				case pricingRequest <- request.Header.Clone():
				default:
				}
			}
			fmt.Fprint(response, `{"success":true,"data":[{"model_name":"gpt-5.6-sol","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["economy"]}],"group_ratio":{"economy":0.05}}`)
		case "/api/user/self":
			if request.Header.Get("Authorization") != "Bearer paired-token" {
				response.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(response, `{"success":false,"message":"invalid token"}`)
				return
			}
			fmt.Fprint(response, `{"success":true,"data":{"balance":12.5,"currency":"USD"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer upstream.Close()

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
	connectorFactory := connectors.NewFactory(upstream.Client(), credentialService)
	collectorService := collector.NewService(st, connectorFactory)
	handler := New(Dependencies{
		Store: st, Credentials: credentialService, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48,
	})
	supplier := domain.Supplier{Name: "paired upstream", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	startRequest := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), nil)
	startRequest.Header.Set("Authorization", "Bearer admin-secret")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("pair start status = %d, body=%s", startResponse.Code, startResponse.Body.String())
	}
	var startEnvelope struct {
		Data struct {
			Code      string `json:"code"`
			ExpiresIn int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &startEnvelope); err != nil || startEnvelope.Data.Code == "" || startEnvelope.Data.ExpiresIn != 300 {
		t.Fatalf("pair start payload = %#v, err=%v", startEnvelope, err)
	}
	startNewPair := func() string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), nil)
		request.Header.Set("Authorization", "Bearer admin-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("new pair start status = %d, body=%s", response.Code, response.Body.String())
		}
		var envelope struct {
			Data struct {
				Code string `json:"code"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Code == "" {
			t.Fatalf("new pair start payload = %#v, err=%v", envelope, err)
		}
		return envelope.Data.Code
	}
	pendingStatus := httptest.NewRecorder()
	handler.ServeHTTP(pendingStatus, httptest.NewRequest(http.MethodGet, "/api/v1/pair/"+startEnvelope.Data.Code+"/status", nil))
	var pendingEnvelope struct {
		Data struct {
			Status         string `json:"status"`
			ExpectedOrigin string `json:"expected_origin"`
		} `json:"data"`
	}
	if err := json.Unmarshal(pendingStatus.Body.Bytes(), &pendingEnvelope); err != nil || pendingStatus.Code != http.StatusOK ||
		pendingEnvelope.Data.Status != "pending" || pendingEnvelope.Data.ExpectedOrigin != upstream.URL {
		t.Fatalf("pending pair status = %d, body=%s", pendingStatus.Code, pendingStatus.Body.String())
	}

	optionsRequest := httptest.NewRequest(http.MethodOptions, "/api/v1/pair/"+startEnvelope.Data.Code, nil)
	optionsResponse := httptest.NewRecorder()
	handler.ServeHTTP(optionsResponse, optionsRequest)
	if optionsResponse.Code != http.StatusNoContent || optionsResponse.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("pair OPTIONS = %d %#v", optionsResponse.Code, optionsResponse.Header())
	}

	wrongOriginPayload := []byte(`{"name":"Browser login","kind":"browser_pairing","payload":{"captured_origin":"https://wrong.example","cookie":"session=paired"}}`)
	wrongOriginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(wrongOriginPayload))
	wrongOriginRequest.Header.Set("Content-Type", "application/json")
	wrongOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongOriginResponse, wrongOriginRequest)
	if wrongOriginResponse.Code != http.StatusBadRequest {
		t.Fatalf("wrong-origin pair status = %d, body=%s", wrongOriginResponse.Code, wrongOriginResponse.Body.String())
	}
	failedOriginStatus := httptest.NewRecorder()
	handler.ServeHTTP(failedOriginStatus, httptest.NewRequest(http.MethodGet, "/api/v1/pair/"+startEnvelope.Data.Code+"/status", nil))
	if failedOriginStatus.Code != http.StatusOK || !bytes.Contains(failedOriginStatus.Body.Bytes(), []byte(`"status":"failed"`)) ||
		!bytes.Contains(failedOriginStatus.Body.Bytes(), []byte(`"last_error_class":"origin_mismatch"`)) {
		t.Fatalf("wrong-origin terminal status = %d, body=%s", failedOriginStatus.Code, failedOriginStatus.Body.String())
	}
	startEnvelope.Data.Code = startNewPair()

	emptyPayload := []byte(fmt.Sprintf(`{"name":"Browser login","kind":"browser_pairing","payload":{"captured_origin":%q}}`, upstream.URL))
	emptyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(emptyPayload))
	emptyRequest.Header.Set("Content-Type", "application/json")
	emptyResponse := httptest.NewRecorder()
	handler.ServeHTTP(emptyResponse, emptyRequest)
	if emptyResponse.Code != http.StatusBadRequest {
		t.Fatalf("empty pair status = %d, body=%s", emptyResponse.Code, emptyResponse.Body.String())
	}
	startEnvelope.Data.Code = startNewPair()

	invalidPayload := []byte(fmt.Sprintf(`{"name":"Browser login","kind":"browser_pairing","payload":{"captured_origin":%q,"local_storage":{"access_token":"expired-token","user_id":"42"},"cookie":"session=expired"}}`, upstream.URL))
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(invalidPayload))
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusUnauthorized || !bytes.Contains(invalidResponse.Body.Bytes(), []byte(`"retryable":true`)) ||
		!bytes.Contains(invalidResponse.Body.Bytes(), []byte(`"attempts_remaining":2`)) {
		t.Fatalf("invalid credential pair status = %d, body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
	failedValidation := httptest.NewRecorder()
	handler.ServeHTTP(failedValidation, httptest.NewRequest(http.MethodGet, "/api/v1/pair/"+startEnvelope.Data.Code+"/status", nil))
	if failedValidation.Code != http.StatusOK || !bytes.Contains(failedValidation.Body.Bytes(), []byte(`"status":"pending"`)) ||
		!bytes.Contains(failedValidation.Body.Bytes(), []byte(`"attempt_count":1`)) ||
		!bytes.Contains(failedValidation.Body.Bytes(), []byte(`"attempts_remaining":2`)) {
		t.Fatalf("failed validation retry state: status=%d body=%s", failedValidation.Code, failedValidation.Body.String())
	}

	payload := []byte(fmt.Sprintf(`{"name":"Browser login","kind":"browser_pairing","payload":{"captured_origin":%q,"local_storage":{"access_token":"paired-token","user_id":"42"},"cookie":"session=paired"}}`, upstream.URL))
	completeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(payload))
	completeRequest.Header.Set("Content-Type", "application/json")
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, completeRequest)
	if completeResponse.Code != http.StatusCreated {
		t.Fatalf("pair complete status = %d, body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	if completeResponse.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("pair complete CORS = %#v", completeResponse.Header())
	}
	completedStatus := httptest.NewRecorder()
	handler.ServeHTTP(completedStatus, httptest.NewRequest(http.MethodGet, "/api/v1/pair/"+startEnvelope.Data.Code+"/status", nil))
	if completedStatus.Code != http.StatusOK || !bytes.Contains(completedStatus.Body.Bytes(), []byte(`"status":"completed"`)) {
		t.Fatalf("completed pair status = %d, body=%s", completedStatus.Code, completedStatus.Body.String())
	}

	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID == nil {
		t.Fatalf("paired supplier = %#v, %v", updated, err)
	}
	_, loaded, err := credentialService.Load(context.Background(), *updated.CredentialID)
	if err != nil {
		t.Fatalf("load paired credential: %v", err)
	}
	if loaded.Authorization != "Bearer paired-token" || loaded.UserID != 42 || loaded.Cookie != "session=paired" {
		t.Fatalf("normalized paired credential = %#v", loaded)
	}

	select {
	case headers := <-pricingRequest:
		if headers.Get("Authorization") != "Bearer paired-token" || headers.Get("New-Api-User") != "42" || headers.Get("Cookie") != "session=paired" {
			t.Fatalf("collection credential headers = %#v", headers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("immediate collection did not reach upstream")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if snapshot, snapshotErr := st.LatestSnapshot(context.Background(), supplier.ID); snapshotErr == nil {
			if len(snapshot.Models) != 1 || snapshot.Groups[0].Ratio != "0.05" {
				t.Fatalf("collected snapshot = %#v", snapshot)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("paired collection did not persist a catalog snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	replayRequest := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(payload))
	replayRequest.Header.Set("Content-Type", "application/json")
	replayResponse := httptest.NewRecorder()
	handler.ServeHTTP(replayResponse, replayRequest)
	if replayResponse.Code != http.StatusConflict {
		t.Fatalf("pair token replay status = %d, body=%s", replayResponse.Code, replayResponse.Body.String())
	}
}

func TestPairingExpectedOriginDropsPathQueryAndFragment(t *testing.T) {
	origin, err := pairingExpectedOrigin("https://Example.COM:8443/private/path?token=must-not-leak#fragment")
	if err != nil || origin != "https://example.com:8443" {
		t.Fatalf("pairing expected origin = %q, %v", origin, err)
	}
	if _, err := pairingExpectedOrigin("https://user:secret@example.com/private"); err == nil {
		t.Fatal("pairing origin unexpectedly accepted URL credentials")
	}
}

func openHTTPTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
