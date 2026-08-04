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
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestBrowserPairingRefreshesExpiredSub2SessionBeforePersisting(t *testing.T) {
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health", "/api/health":
			_, _ = fmt.Fprint(w, `{"status":"ok"}`)
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["refresh_token"] != "captured-refresh" {
				http.Error(w, "bad refresh request", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":0,"message":"success","data":{"access_token":"fresh-access","refresh_token":"rotated-refresh","expires_in":3600}}`)
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer fresh-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"code":401,"message":"token expired"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":0,"message":"success","data":{"id":42,"email":"owner@example.test","status":"active"}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/api/v1/") && r.Header.Get("Authorization") != "Bearer fresh-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprint(w, `{"code":401,"message":"token expired"}`)
				return
			}
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
	connectorFactory := connectors.NewFactory(upstream.Client(), credentialService)
	collectorService := collector.NewService(st, connectorFactory)
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher,
		Collector: collectorService, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "MDKJ", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		PollIntervalSeconds: 300, Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	start := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), nil)
	start.Header.Set("Authorization", "Bearer admin-secret")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	var startEnvelope struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &startEnvelope); err != nil || startResponse.Code != http.StatusCreated || startEnvelope.Data.Code == "" {
		t.Fatalf("pair start status=%d body=%s err=%v", startResponse.Code, startResponse.Body.String(), err)
	}

	payload := []byte(fmt.Sprintf(`{"name":"MDKJ browser session","kind":"browser_pairing","payload":{"captured_origin":%q,"local_storage":{"auth_token":"expired-access","refresh_token":"captured-refresh","auth_user":"{\"id\":42}"}}}`, upstream.URL))
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(payload))
	complete.Header.Set("Content-Type", "application/json")
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusCreated {
		t.Fatalf("pair complete status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d want=1", refreshCalls.Load())
	}

	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID == nil {
		t.Fatalf("paired supplier=%#v err=%v", updated, err)
	}
	_, stored, err := credentialService.Load(context.Background(), *updated.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Authorization != "Bearer fresh-access" || stored.LocalStorage["refresh_token"] != "rotated-refresh" {
		t.Fatalf("stored refreshed payload authorization=%q refresh=%q", stored.Authorization, stored.LocalStorage["refresh_token"])
	}
}

func TestBrowserPairingFailedRefreshCountsOneAttemptAndPersistsNothing(t *testing.T) {
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/refresh" {
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"code":401,"message":"refresh rejected"}`)
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
	credentialService := credentials.NewService(st, vault)
	connectorFactory := connectors.NewFactory(upstream.Client(), credentialService)
	collectorService := collector.NewService(st, connectorFactory)
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher,
		Collector: collectorService, StaticDir: t.TempDir(), AdminToken: "admin-secret", PairingMaximumAttempts: 3})
	supplier := domain.Supplier{Name: "MDKJ", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		PollIntervalSeconds: 300, Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	start := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), nil)
	start.Header.Set("Authorization", "Bearer admin-secret")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	var startEnvelope struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	_ = json.Unmarshal(startResponse.Body.Bytes(), &startEnvelope)
	payload := []byte(fmt.Sprintf(`{"name":"MDKJ browser session","kind":"browser_pairing","payload":{"captured_origin":%q,"local_storage":{"auth_token":"expired-access","refresh_token":"captured-refresh"}}}`, upstream.URL))
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(payload))
	complete.Header.Set("Content-Type", "application/json")
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusUnauthorized || !bytes.Contains(completeResponse.Body.Bytes(), []byte(`"attempt_count":1`)) ||
		!bytes.Contains(completeResponse.Body.Bytes(), []byte(`"attempts_remaining":2`)) {
		t.Fatalf("failed refresh status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d want=1", refreshCalls.Load())
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID != nil {
		t.Fatalf("failed refresh persisted credential: %#v err=%v", updated, err)
	}
}

func TestBrowserPairingRemoteWriteGatePreservesPendingAttemptAndPersistsNothing(t *testing.T) {
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
	credentialService := credentials.NewService(st, vault)
	collectorService := collector.NewService(st, connectors.NewFactory(upstream.Client(), credentialService))
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher,
		Collector: collectorService, StaticDir: t.TempDir(), AdminToken: "admin-secret", PairingMaximumAttempts: 3,
		RemoteWriteGate: remotewrite.New(false)})
	supplier := domain.Supplier{Name: "MDKJ candidate", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		PollIntervalSeconds: 300, Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	start := authenticatedRequest(handler, http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), "")
	var startEnvelope struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &startEnvelope); err != nil || start.Code != http.StatusCreated {
		t.Fatalf("pair start status=%d body=%s err=%v", start.Code, start.Body.String(), err)
	}
	payload := []byte(fmt.Sprintf(`{"name":"MDKJ browser session","kind":"browser_pairing","payload":{"captured_origin":%q,"local_storage":{"auth_token":"expired-access","refresh_token":"captured-refresh"}}}`, upstream.URL))
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+startEnvelope.Data.Code, bytes.NewReader(payload))
	complete.Header.Set("Content-Type", "application/json")
	completeResponse := httptest.NewRecorder()
	handler.ServeHTTP(completeResponse, complete)
	if completeResponse.Code != http.StatusServiceUnavailable ||
		!bytes.Contains(completeResponse.Body.Bytes(), []byte(remotewrite.DisabledMessage)) ||
		!bytes.Contains(completeResponse.Body.Bytes(), []byte(`"attempt_consumed":false`)) {
		t.Fatalf("blocked refresh status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("blocked refresh reached upstream %d times", refreshCalls.Load())
	}
	state, err := st.PairingStatusDetail(context.Background(), tokenHash(startEnvelope.Data.Code), time.Now().UTC())
	if err != nil || state.Status != "pending" || state.AttemptCount != 0 || state.LastErrorClass != "" {
		t.Fatalf("pairing state=%#v err=%v", state, err)
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID != nil {
		t.Fatalf("blocked refresh persisted credential: %#v err=%v", updated, err)
	}
}
