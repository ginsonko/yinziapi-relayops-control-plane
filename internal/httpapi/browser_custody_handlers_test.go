package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/collector"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestBrowserCustodyRequestsPreemptiveRenewalAndBoundsAutomaticRecovery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	far := now.Add(browserCustodyRefreshAhead + time.Second)
	if _, needed := browserCustodyExpiryDemand(&far, now); needed {
		t.Fatal("credential outside the refresh-ahead window requested custody")
	}
	near := now.Add(time.Minute)
	since, needed := browserCustodyExpiryDemand(&near, now)
	if !needed || !since.Equal(near.Add(-browserCustodyRefreshAhead)) {
		t.Fatalf("preemptive demand since=%s needed=%v", since, needed)
	}

	status := domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online"}
	markBrowserCustodyRecoveryUntil(&status, "credential_expiring", since, near.Add(browserCustodyRecoveryGrace), now)
	if !status.NeedsCapture || !status.RecoveryPending || status.RecoveryOverdue || status.NeedSince == nil {
		t.Fatalf("bounded recovery status=%#v", status)
	}

	overdue := domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online"}
	markBrowserCustodyRecoveryUntil(&overdue, "credential_expiring", since, now.Add(-time.Second), now)
	if overdue.RecoveryPending || !overdue.RecoveryOverdue {
		t.Fatalf("overdue recovery status=%#v", overdue)
	}

	rejectedAt := now.Add(-time.Minute)
	rejected := domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online",
		LastSubmissionAt: &rejectedAt, LastErrorClass: "auth_expired"}
	markBrowserCustodyRecovery(&rejected, "catalog_auth_expired", now.Add(-2*time.Minute), now)
	if rejected.RecoveryPending || !rejected.RecoveryOverdue {
		t.Fatalf("rejected recovery status=%#v", rejected)
	}
}

func TestBrowserCustodyRecoversOnlyAfterExplicitAuthFailureAndCanBeRevoked(t *testing.T) {
	var accepted atomic.Value
	accepted.Store("old-token")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/pricing":
			if r.Header.Get("Authorization") != "Bearer "+accepted.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"success":false,"message":"session expired"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"success":true,"data":[{"model_name":"gpt-5.6-sol","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["economy"]}],"group_ratio":{"economy":0.05}}`)
		case "/api/user/self":
			if r.Header.Get("Authorization") != "Bearer "+accepted.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"success":false,"message":"session expired"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"id":42,"balance":12.5,"currency":"USD"}}`)
		default:
			http.NotFound(w, r)
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
	collectorService := collector.NewService(st, connectors.NewFactory(upstream.Client(), credentialService))
	handler := New(Dependencies{Store: st, Credentials: credentialService, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "aishop", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true,
		PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	start := authenticatedRequest(handler, http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), "")
	var startEnvelope struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &startEnvelope); err != nil || start.Code != http.StatusCreated || startEnvelope.Data.Code == "" {
		t.Fatalf("pair start status=%d body=%s err=%v", start.Code, start.Body.String(), err)
	}
	code := startEnvelope.Data.Code
	pairBody := fmt.Sprintf(`{"payload":{"captured_origin":%q,"local_storage":{"access_token":"old-token","user_id":"42"}}}`, upstream.URL)
	pair := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+code, bytes.NewBufferString(pairBody))
	pair.Header.Set("Content-Type", "application/json")
	pairResponse := httptest.NewRecorder()
	handler.ServeHTTP(pairResponse, pair)
	if pairResponse.Code != http.StatusCreated || !bytes.Contains(pairResponse.Body.Bytes(), []byte(`"browser_custody"`)) {
		t.Fatalf("pair complete status=%d body=%s", pairResponse.Code, pairResponse.Body.String())
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID == nil {
		t.Fatalf("paired supplier=%#v err=%v", updated, err)
	}
	credentialID := *updated.CredentialID
	deadline := time.Now().Add(2 * time.Second)
	for {
		collected, _ := st.GetSupplier(context.Background(), supplier.ID)
		if collected.LastStatus == "healthy" && collected.LastSuccessAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("initial post-pair collection did not finish: %#v", collected)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pending := authenticatedRequest(handler, http.MethodGet,
		fmt.Sprintf("/api/v1/suppliers/%d/browser-custody", supplier.ID), "")
	if pending.Code != http.StatusOK || !bytes.Contains(pending.Body.Bytes(), []byte(`"helper_state":"binding_pending"`)) {
		t.Fatalf("new binding should wait for helper heartbeat: status=%d body=%s", pending.Code, pending.Body.String())
	}
	if err := st.RecordBrowserCustodyCheck(context.Background(), tokenHash(code), browserCustodyMinimumHelperVersion,
		time.Now().UTC().Add(-browserCustodyHeartbeatTTL+time.Minute)); err != nil {
		t.Fatal(err)
	}
	throttled := authenticatedRequest(handler, http.MethodGet,
		fmt.Sprintf("/api/v1/suppliers/%d/browser-custody", supplier.ID), "")
	if throttled.Code != http.StatusOK || !bytes.Contains(throttled.Body.Bytes(), []byte(`"helper_state":"online"`)) {
		t.Fatalf("throttled helper heartbeat should remain online: status=%d body=%s", throttled.Code, throttled.Body.String())
	}
	if err := st.RecordBrowserCustodyCheck(context.Background(), tokenHash(code), browserCustodyMinimumHelperVersion,
		time.Now().UTC().Add(-browserCustodyHeartbeatTTL-time.Minute)); err != nil {
		t.Fatal(err)
	}
	offline := authenticatedRequest(handler, http.MethodGet,
		fmt.Sprintf("/api/v1/suppliers/%d/browser-custody", supplier.ID), "")
	if offline.Code != http.StatusOK || !bytes.Contains(offline.Body.Bytes(), []byte(`"helper_state":"offline"`)) {
		t.Fatalf("stale helper heartbeat should be offline: status=%d body=%s", offline.Code, offline.Body.String())
	}

	status := custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusOK || bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		t.Fatalf("healthy custody status=%d body=%s", status.Code, status.Body.String())
	}
	if !bytes.Contains(status.Body.Bytes(), []byte(`"helper_state":"online"`)) ||
		!bytes.Contains(status.Body.Bytes(), []byte(`"helper_version":"1.3.0"`)) {
		t.Fatalf("compatible helper heartbeat not observable: %s", status.Body.String())
	}
	if status.Header().Get("Access-Control-Allow-Origin") != "*" || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("custody CORS/cache headers=%#v", status.Header())
	}

	accepted.Store("new-token")
	if _, err := collectorService.Collect(context.Background(), supplier.ID); err == nil {
		t.Fatal("expired old token unexpectedly collected successfully")
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		failedSupplier, _ := st.GetSupplier(context.Background(), supplier.ID)
		capabilities, _ := st.ListAuthCapabilities(context.Background(), supplier.ID)
		ids := make([]int64, 0, len(capabilities))
		for _, capability := range capabilities {
			if capability.CredentialID != nil {
				ids = append(ids, *capability.CredentialID)
			}
		}
		t.Fatalf("expired custody status=%d body=%s credential=%d capability_ids=%v supplier=%#v capabilities=%#v", status.Code, status.Body.String(), credentialID, ids, failedSupplier, capabilities)
	}
	rejectedBody := fmt.Sprintf(`{"payload":{"captured_origin":%q,"local_storage":{"access_token":"old-token","user_id":"42"}}}`, upstream.URL)
	rejected := custodyRequest(handler, http.MethodPost, "/api/v1/browser-custody/capture", code, rejectedBody)
	if rejected.Code != http.StatusUnprocessableEntity ||
		!bytes.Contains(rejected.Body.Bytes(), []byte(`"old_credential_preserved":true`)) {
		t.Fatalf("stale page session must preserve custody binding: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		t.Fatalf("rejected candidate deleted custody binding: status=%d body=%s", status.Code, status.Body.String())
	}

	wrongOrigin := `{"payload":{"captured_origin":"https://wrong.example","local_storage":{"access_token":"new-token"}}}`
	wrong := custodyRequest(handler, http.MethodPost, "/api/v1/browser-custody/capture", code, wrongOrigin)
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("wrong-origin capture status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	_, before, err := credentialService.Load(context.Background(), credentialID)
	if err != nil || before.Authorization != "Bearer old-token" {
		t.Fatalf("wrong-origin capture changed credential: auth=%q err=%v", before.Authorization, err)
	}

	captureBody := fmt.Sprintf(`{"payload":{"captured_origin":%q,"local_storage":{"access_token":"new-token","user_id":"42"}}}`, upstream.URL)
	captured := custodyRequest(handler, http.MethodPost, "/api/v1/browser-custody/capture", code, captureBody)
	if captured.Code != http.StatusOK || !bytes.Contains(captured.Body.Bytes(), []byte(`"captured":true`)) {
		t.Fatalf("custody capture status=%d body=%s", captured.Code, captured.Body.String())
	}
	accepted.Store("newer-token")
	if _, err := collectorService.Collect(context.Background(), supplier.ID); err == nil {
		t.Fatal("second expired token unexpectedly collected successfully")
	}
	repeatedBody := fmt.Sprintf(`{"payload":{"captured_origin":%q,"local_storage":{"access_token":"newer-token","user_id":"42"}}}`, upstream.URL)
	repeated := custodyRequest(handler, http.MethodPost, "/api/v1/browser-custody/capture", code, repeatedBody)
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeated custody capture status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	activityPage, err := st.QueryActivities(context.Background(), store.ActivityQuery{Page: 1, Limit: 50, SkipTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	recoveryRows := 0
	recoveryMetadata := make([]map[string]string, 0)
	for _, activity := range activityPage.Items {
		if activity.Kind == "browser_custody_recovered" {
			recoveryRows++
			recoveryMetadata = append(recoveryMetadata, activity.Metadata)
		}
	}
	if recoveryRows != 1 || len(recoveryMetadata) != 1 || recoveryMetadata[0]["repeat_count"] != "2" {
		t.Fatalf("browser custody recovery rows = %d metadata=%#v, want one row with repeat_count=2", recoveryRows, recoveryMetadata)
	}
	currentSupplier, _ := st.GetSupplier(context.Background(), supplier.ID)
	if currentSupplier.CredentialID == nil || *currentSupplier.CredentialID != credentialID {
		t.Fatalf("custody replaced credential identity: %#v", currentSupplier.CredentialID)
	}
	_, after, err := credentialService.Load(context.Background(), credentialID)
	if err != nil || after.Authorization != "Bearer newer-token" {
		t.Fatalf("custody did not commit new session: auth=%q err=%v", after.Authorization, err)
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusOK || bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) ||
		!bytes.Contains(status.Body.Bytes(), []byte(`"last_success_at"`)) {
		t.Fatalf("recovered custody status=%d body=%s", status.Code, status.Body.String())
	}
	optionalFailureAt := time.Now().UTC().Add(time.Second)
	partial := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "partial_success", ErrorClass: "auth_expired",
		ErrorMessage: "optional usage endpoint rejected", StartedAt: optionalFailureAt, FinishedAt: optionalFailureAt}
	diagnostics := domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{
		{SupplierID: supplier.ID, CredentialID: &credentialID, Capability: "customer_session", EndpointType: "catalog", Status: "verified", LastAttemptAt: optionalFailureAt, VerifiedAt: &optionalFailureAt},
		{SupplierID: supplier.ID, CredentialID: &credentialID, Capability: "customer_session", EndpointType: "balance", Status: "verified", LastAttemptAt: optionalFailureAt, VerifiedAt: &optionalFailureAt},
		{SupplierID: supplier.ID, CredentialID: &credentialID, Capability: "customer_session", EndpointType: "usage", Status: "expired_or_invalid", ErrorClass: "auth_expired", LastAttemptAt: optionalFailureAt},
	}}
	if err := st.RecordCollectionAttempt(context.Background(), &partial, diagnostics, optionalFailureAt.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusOK || bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		t.Fatalf("optional usage failure triggered custody capture: status=%d body=%s", status.Code, status.Body.String())
	}

	revoked := authenticatedRequest(handler, http.MethodDelete,
		fmt.Sprintf("/api/v1/suppliers/%d/browser-custody", supplier.ID), "")
	if revoked.Code != http.StatusOK {
		t.Fatalf("admin revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", code, "")
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("revoked custody token status=%d body=%s", status.Code, status.Body.String())
	}
}

func TestBrowserCustodyVersionOrdering(t *testing.T) {
	for _, testCase := range []struct {
		current string
		want    bool
	}{
		{"1.3.0", true}, {"1.3.1", true}, {"1.10.0", true}, {"2.0.0", true},
		{"1.2.99", false}, {"0.0.0", false}, {"1.3", false}, {"1.3.0-beta", false}, {"", false},
	} {
		if got := browserCustodyVersionAtLeast(testCase.current, browserCustodyMinimumHelperVersion); got != testCase.want {
			t.Errorf("version %q compatible=%t want %t", testCase.current, got, testCase.want)
		}
	}
}

func TestBrowserCustodyRemoteWriteGatePreservesOldCredentialAndFailureState(t *testing.T) {
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/refresh" {
			refreshCalls.Add(1)
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"must-not-be-issued"}}`)
			return
		}
		if r.URL.Path == "/health" || r.URL.Path == "/api/health" {
			_, _ = fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"code":401,"message":"token expired"}`)
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
	service := credentials.NewService(st, vault)
	supplier := domain.Supplier{Name: "Sub2 custody candidate", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	token := "candidate-custody-token"
	tokenDigest := tokenHash(token)
	if err := st.SavePairingToken(context.Background(), tokenDigest, supplier.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "preserved browser session", Kind: "browser_pairing", SiteURL: upstream.URL}
	preserved := domain.CredentialPayload{Authorization: "Bearer preserved-access", CapturedOrigin: upstream.URL,
		LocalStorage: map[string]string{"auth_token": "preserved-access", "refresh_token": "preserved-refresh"}}
	if err := service.CreateFromPairingWithCustody(context.Background(), tokenDigest, now, supplier.ID, &credential,
		preserved, upstream.URL, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	failureAt := now.Add(time.Second)
	if err := st.RecordCollectionAttempt(context.Background(), &domain.CollectionAttempt{SupplierID: supplier.ID,
		Status: "failed", ErrorClass: "auth_expired", ErrorMessage: "fixture expired", StartedAt: failureAt, FinishedAt: failureAt},
		domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{{SupplierID: supplier.ID,
			CredentialID: &credential.ID, Capability: "customer_session", EndpointType: "catalog",
			Status: "expired_or_invalid", ErrorClass: "auth_expired", LastAttemptAt: failureAt}}}, failureAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	collectorService := collector.NewService(st, connectors.NewFactory(upstream.Client(), service))
	refresher := credentials.NewRefresher(st, service, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: service, Collector: collectorService, Refresher: refresher,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RemoteWriteGate: remotewrite.New(false)})

	status := custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", token, "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		t.Fatalf("custody precondition status=%d body=%s", status.Code, status.Body.String())
	}
	beforeBinding, err := st.GetBrowserCustodyForSupplier(context.Background(), supplier.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	captureBody := fmt.Sprintf(`{"payload":{"captured_origin":%q,"local_storage":{"auth_token":"expired-access","refresh_token":"captured-refresh"}}}`, upstream.URL)
	captured := custodyRequest(handler, http.MethodPost, "/api/v1/browser-custody/capture", token, captureBody)
	if captured.Code != http.StatusServiceUnavailable ||
		!bytes.Contains(captured.Body.Bytes(), []byte(remotewrite.DisabledMessage)) ||
		!bytes.Contains(captured.Body.Bytes(), []byte(`"old_credential_preserved":true`)) {
		t.Fatalf("blocked custody refresh status=%d body=%s", captured.Code, captured.Body.String())
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("blocked custody refresh reached upstream %d times", refreshCalls.Load())
	}
	_, afterPayload, err := service.Load(context.Background(), credential.ID)
	if err != nil || afterPayload.Authorization != preserved.Authorization ||
		afterPayload.LocalStorage["refresh_token"] != preserved.LocalStorage["refresh_token"] {
		t.Fatalf("blocked custody replaced credential: payload=%#v err=%v", afterPayload, err)
	}
	afterBinding, err := st.GetBrowserCustodyForSupplier(context.Background(), supplier.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if afterBinding.SubmissionCount != beforeBinding.SubmissionCount || afterBinding.LastErrorClass != beforeBinding.LastErrorClass ||
		afterBinding.LastErrorMessage != beforeBinding.LastErrorMessage || !equalOptionalTime(afterBinding.LastSuccessAt, beforeBinding.LastSuccessAt) {
		t.Fatalf("blocked custody changed lifecycle\nbefore=%#v\nafter=%#v", beforeBinding, afterBinding)
	}
	status = custodyRequest(handler, http.MethodGet, "/api/v1/browser-custody/status", token, "")
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"needs_capture":true`)) {
		t.Fatalf("blocked custody lost retry state status=%d body=%s", status.Code, status.Body.String())
	}
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func custodyRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set(browserCustodyHeader, token)
	request.Header.Set(browserCustodyHelperVersionHeader, browserCustodyMinimumHelperVersion)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestBrowserCustodyOldHelperCanReadUpgradeStateButCannotCapture(t *testing.T) {
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
	service := credentials.NewService(st, vault)
	supplier := domain.Supplier{Name: "legacy", BaseURL: "https://legacy.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	legacyDigest := tokenHash("legacy-token")
	if err := st.SavePairingToken(context.Background(), legacyDigest, supplier.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "legacy browser", Kind: "browser_pairing", SiteURL: supplier.BaseURL}
	if err := service.CreateFromPairingWithCustody(context.Background(), legacyDigest, now, supplier.ID, &credential,
		domain.CredentialPayload{Cookie: "session=old", CapturedOrigin: supplier.BaseURL}, supplier.BaseURL, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, Credentials: service, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/browser-custody/status", nil)
	statusRequest.Header.Set(browserCustodyHeader, "legacy-token")
	statusRequest.Header.Set(browserCustodyHelperVersionHeader, "1.1.0")
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"helper_state":"upgrade_required"`)) {
		t.Fatalf("legacy helper status=%d body=%s", status.Code, status.Body.String())
	}

	captureRequest := httptest.NewRequest(http.MethodPost, "/api/v1/browser-custody/capture", bytes.NewBufferString(
		`{"payload":{"captured_origin":"https://legacy.example","cookie":"session=new"}}`))
	captureRequest.Header.Set(browserCustodyHeader, "legacy-token")
	captureRequest.Header.Set(browserCustodyHelperVersionHeader, "1.1.0")
	captureRequest.Header.Set("Content-Type", "application/json")
	captured := httptest.NewRecorder()
	handler.ServeHTTP(captured, captureRequest)
	if captured.Code != http.StatusUpgradeRequired {
		t.Fatalf("legacy helper capture status=%d body=%s", captured.Code, captured.Body.String())
	}
	_, payload, err := service.Load(context.Background(), credential.ID)
	if err != nil || payload.Cookie != "session=old" {
		t.Fatalf("legacy helper changed credential: cookie=%q err=%v", payload.Cookie, err)
	}
}

func TestBrowserCustodyRejectsStaleCASAndPreservesManualCredential(t *testing.T) {
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
	service := credentials.NewService(st, vault)
	supplier := domain.Supplier{Name: "CAS", BaseURL: "https://cas.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := st.SavePairingToken(context.Background(), "token-digest", supplier.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "browser", Kind: "browser_pairing", SiteURL: supplier.BaseURL}
	if err := service.CreateFromPairingWithCustody(context.Background(), "token-digest", now, supplier.ID, &credential,
		domain.CredentialPayload{Cookie: "session=old", CapturedOrigin: supplier.BaseURL}, supplier.BaseURL, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	loaded, existing, err := service.Load(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Update(context.Background(), credential.ID, domain.CredentialPayload{Cookie: "session=manual"}); err != nil {
		t.Fatal(err)
	}
	err = service.CommitBrowserCustodyRenewal(context.Background(), loaded, existing,
		domain.CredentialPayload{Cookie: "session=background", CapturedOrigin: supplier.BaseURL},
		domain.CredentialRenewalStatus{State: "unconfigured"}, "token-digest", supplier.ID)
	if !errors.Is(err, store.ErrCredentialChanged) {
		t.Fatalf("stale browser custody commit error=%v want ErrCredentialChanged", err)
	}
	_, preserved, err := service.Load(context.Background(), credential.ID)
	if err != nil || preserved.Cookie != "session=manual" {
		t.Fatalf("stale custody overwrote manual credential: cookie=%q err=%v", preserved.Cookie, err)
	}
}
