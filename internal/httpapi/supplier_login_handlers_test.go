package httpapi

import (
	"bytes"
	"context"
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
)

func TestSupplierPasswordLoginValidatesThenAtomicallyBindsAndEnablesRenewal(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/user/login":
			var body bytes.Buffer
			_, _ = body.ReadFrom(r.Body)
			if !strings.Contains(body.String(), `"username":"owner"`) || !strings.Contains(body.String(), `"password":"correct"`) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"success":false,"message":"invalid credentials"}`)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "fresh-session", Path: "/", HttpOnly: true})
			fmt.Fprint(w, `{"success":true,"data":{"id":42}}`)
		case "/api/status":
			fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/user/self":
			if cookie, _ := r.Cookie("session"); cookie == nil || cookie.Value != "fresh-session" || r.Header.Get("New-Api-User") != "42" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"success":false,"message":"expired"}`)
				return
			}
			fmt.Fprint(w, `{"success":true,"data":{"id":42,"balance":12.5,"currency":"USD"}}`)
		case "/api/data/self":
			fmt.Fprint(w, `{"success":true,"data":{"quota":1250000,"used_quota":1000}}`)
		case "/api/channel/":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"message":"not admin"}`)
		case "/api/pricing":
			fmt.Fprint(w, `{"success":true,"data":[{"model_name":"gpt-test","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["default"]}],"group_ratio":{"default":1}}`)
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
	factory := connectors.NewFactory(upstream.Client(), credentialService)
	factory.SetConnectorSpecLoader(st)
	factory.SetConnectorManifestLoader(st)
	collectorService := collector.NewService(st, factory)
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute).WithCandidateValidator(
		func(ctx context.Context, supplier domain.Supplier, credential domain.Credential, payload domain.CredentialPayload) error {
			_, err := collectorService.ValidateCredential(ctx, supplier, credential.Kind, payload)
			return err
		})
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "login upstream", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/suppliers/%d/login", supplier.ID),
		strings.NewReader(`{"username":"owner","password":"correct","enable_renewal":true}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "correct") || strings.Contains(response.Body.String(), "fresh-session") {
		t.Fatalf("login response exposed credential material: %s", response.Body.String())
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID == nil {
		t.Fatalf("updated supplier=%#v err=%v", updated, err)
	}
	credential, payload, err := credentialService.Load(context.Background(), *updated.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Kind != "password_login" || payload.Cookie != "session=fresh-session" || payload.UserID != 42 ||
		payload.Login == nil || !payload.Login.Enabled || payload.Login.Username != "owner" || payload.Login.Password != "correct" {
		t.Fatalf("persisted login candidate credential=%#v payload=%#v", credential, payload)
	}
	renewal, err := st.GetCredentialRenewal(context.Background(), credential.ID)
	if err != nil || !renewal.Configured || !renewal.Enabled || renewal.State != "healthy" || renewal.LastSuccessAt == nil {
		t.Fatalf("renewal=%#v err=%v", renewal, err)
	}
}

func TestSub2SupplierPasswordLoginBindsAndRecollectsWhenChannelsFeatureIsDisabled(t *testing.T) {
	t.Parallel()
	const customerToken = "sub2-customer-token"
	const businessKey = "sub2-business-key"
	var loginUserAgent atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/login" {
			var body bytes.Buffer
			_, _ = body.ReadFrom(r.Body)
			if !strings.Contains(body.String(), `"email":"owner@example.com"`) || !strings.Contains(body.String(), `"password":"correct"`) {
				http.Error(w, `{"code":401,"message":"invalid credentials"}`, http.StatusUnauthorized)
				return
			}
			if strings.TrimSpace(r.UserAgent()) == "" {
				http.Error(w, `{"code":400,"message":"client identity required"}`, http.StatusBadRequest)
				return
			}
			loginUserAgent.Store(r.UserAgent())
			fmt.Fprintf(w, `{"code":"0","data":{"access_token":%q,"refresh_token":"refresh-value","expires_in":3600,"user":{"id":9}}}`, customerToken)
			return
		}
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") != "Bearer "+businessKey {
				http.Error(w, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-current"}]}`)
			return
		}
		expectedUserAgent, _ := loginUserAgent.Load().(string)
		if r.Header.Get("Authorization") != "Bearer "+customerToken || r.UserAgent() != expectedUserAgent {
			http.Error(w, `{"code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/auth/me":
			fmt.Fprint(w, `{"code":0,"data":{"id":9,"balance":61.93,"frozen_balance":0,"status":"active"}}`)
		case "/api/v1/groups/available":
			fmt.Fprint(w, `{"code":0,"data":[{"id":7,"name":"Economy","platform":"openai","rate_multiplier":0.08}]}`)
		case "/api/v1/groups/rates":
			fmt.Fprint(w, `{"code":0,"data":{"7":0.05}}`)
		case "/api/v1/usage":
			fmt.Fprint(w, `{"code":0,"data":{"today_actual_cost":0.31,"today_requests":4}}`)
		case "/api/v1/keys":
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"key":%q,"status":"active","group_id":7}],"total":1,"page":1,"page_size":100,"pages":1}}`, businessKey)
		case "/api/v1/channels/available":
			http.Error(w, `{"code":403,"message":"available channels disabled"}`, http.StatusForbidden)
		default:
			http.Error(w, `{"code":401,"message":"legacy endpoint unavailable"}`, http.StatusUnauthorized)
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
	factory := connectors.NewFactory(upstream.Client(), credentialService)
	collectorService := collector.NewService(st, factory)
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute).WithCandidateValidator(
		func(ctx context.Context, supplier domain.Supplier, credential domain.Credential, payload domain.CredentialPayload) error {
			_, err := collectorService.ValidateCredential(ctx, supplier, credential.Kind, payload)
			return err
		})
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "MDKJ fixture", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/suppliers/%d/login", supplier.ID),
		strings.NewReader(`{"username":"owner@example.com","password":"correct","enable_renewal":true}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("Sub2 login status=%d body=%s", response.Code, response.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		snapshot, snapshotErr := st.LatestSnapshot(context.Background(), supplier.ID)
		if snapshotErr == nil && len(snapshot.Models) == 1 && snapshot.Balance != nil {
			if snapshot.Models[0].Name != "gpt-current" || snapshot.Balance.Amount != "61.93" || snapshot.Balance.DailySpend != "0.31" {
				t.Fatalf("recollected snapshot=%#v", snapshot)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verified Sub2 login was bound, but immediate background recollection did not finish")
}

func TestSub2SupplierPasswordLoginPersistsValidIdentityWhenOptionalCapabilitiesAreUnavailable(t *testing.T) {
	t.Parallel()
	const customerToken = "identity-only-token"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"refresh-value","expires_in":3600,"user":{"id":9}}}`, customerToken)
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer "+customerToken {
				http.Error(w, `{}`, http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"code":0,"data":{"id":"9","email":"owner@example.test","status":"active"}}`)
		default:
			http.Error(w, `{"code":403,"message":"optional capability disabled"}`, http.StatusForbidden)
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
	factory := connectors.NewFactory(upstream.Client(), credentialService)
	collectorService := collector.NewService(st, factory)
	refresher := credentials.NewRefresher(st, credentialService, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: credentialService, Refresher: refresher, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "MDKJ identity fixture", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true,
		Metadata: map[string]string{"sub2api_mode": "upstream"}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/suppliers/%d/login", supplier.ID),
		strings.NewReader(`{"username":"owner@example.test","password":"correct","enable_renewal":true}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("identity-only login status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID == nil {
		t.Fatalf("verified identity was not bound: supplier=%#v err=%v", updated, err)
	}
}

func TestSupplierPasswordLoginRequiresHumanActionWithoutPersisting(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/user/login" {
			fmt.Fprint(w, `{"success":false,"message":"captcha verification required"}`)
			return
		}
		if r.URL.Path == "/api/status" {
			fmt.Fprint(w, `{"success":true}`)
			return
		}
		http.NotFound(w, r)
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
	factory := connectors.NewFactory(upstream.Client(), service)
	collectorService := collector.NewService(st, factory)
	refresher := credentials.NewRefresher(st, service, upstream.Client(), time.Minute)
	handler := New(Dependencies{Store: st, Credentials: service, Refresher: refresher, Collector: collectorService,
		StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	supplier := domain.Supplier{Name: "captcha upstream", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/suppliers/%d/login", supplier.ID),
		strings.NewReader(`{"username":"owner","password":"secret","enable_renewal":true}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"error_class":"captcha_or_2fa"`) ||
		!strings.Contains(response.Body.String(), `"human_action":"browser_pairing"`) {
		t.Fatalf("human action response=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.CredentialID != nil {
		t.Fatalf("failed login persisted credential: %#v err=%v", updated, err)
	}
}
