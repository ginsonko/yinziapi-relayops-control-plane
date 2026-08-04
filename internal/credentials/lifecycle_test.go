package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestCredentialCreateNormalizesModesAndInfersExpiry(t *testing.T) {
	expiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	token := unsignedJWT(expiry)
	tests := []struct {
		name, mode, header, wantAuthorization, wantHeader string
	}{
		{name: "bearer", mode: "bearer", wantAuthorization: "Bearer " + token},
		{name: "raw", mode: "raw", wantAuthorization: token},
		{name: "header", mode: "header", header: "X-API-Key", wantHeader: token},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _ := newCredentialServices(t)
			item := domain.Credential{Name: test.name, SiteURL: "https://example.test"}
			payload := domain.CredentialPayload{AuthorizationMode: test.mode, AuthorizationHeader: test.header,
				LocalStorage: map[string]string{"access_token": token}}
			if err := service.Create(context.Background(), &item, payload); err != nil {
				t.Fatal(err)
			}
			credential, loaded, err := service.Load(context.Background(), item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Authorization != test.wantAuthorization || loaded.Headers[test.header] != test.wantHeader {
				t.Fatalf("normalized payload = %#v", loaded)
			}
			if credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(expiry) {
				t.Fatalf("expiry = %v, want %v", credential.ExpiresAt, expiry)
			}
		})
	}
}

func TestRefreshCustomHeaderCookieAndExpiryPreservation(t *testing.T) {
	var requestOK atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOK.Store(r.Header.Get("X-API-Key") == "old-token" && r.Header.Get("Cookie") == "old=1")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "renewed", Path: "/"})
		_, _ = fmt.Fprint(w, `{"session":{"token":"new-token"}}`)
	}))
	defer server.Close()

	service, st := newCredentialServices(t)
	expiry := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Second)
	item := domain.Credential{Name: "header", SiteURL: server.URL, ExpiresAt: &expiry}
	payload := domain.CredentialPayload{AuthorizationMode: "header", AuthorizationHeader: "X-API-Key", Cookie: "old=1",
		LocalStorage: map[string]string{"access_token": "old-token"},
		Refresh:      &domain.RefreshRule{URL: "/refresh", TokenJSONPath: "$.session.token"}}
	if err := service.Create(context.Background(), &item, payload); err != nil {
		t.Fatal(err)
	}
	if err := NewRefresher(st, service, server.Client(), time.Minute).Refresh(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	credential, loaded, err := service.Load(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !requestOK.Load() || loaded.Headers["X-API-Key"] != "new-token" || loaded.Cookie != "old=1; session=renewed" {
		t.Fatalf("refresh result = %#v, requestOK=%v", loaded, requestOK.Load())
	}
	if credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(expiry) {
		t.Fatalf("expiry changed to %v", credential.ExpiresAt)
	}
}

func TestCookieOnlyRefreshAndConcurrentCoalescing(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "new", Path: "/"})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	service, st := newCredentialServices(t)
	item := domain.Credential{Name: "cookie", SiteURL: server.URL}
	if err := service.Create(context.Background(), &item, domain.CredentialPayload{Cookie: "sid=old",
		Refresh: &domain.RefreshRule{URL: "/refresh", CookieResponse: true}}); err != nil {
		t.Fatal(err)
	}
	refresher := NewRefresher(st, service, server.Client(), time.Millisecond)
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); errorsSeen <- refresher.Refresh(context.Background(), item.ID) }()
	<-started
	go func() { defer wait.Done(); errorsSeen <- refresher.Refresh(context.Background(), item.ID) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", calls.Load())
	}
	_, loaded, _ := service.Load(context.Background(), item.ID)
	if loaded.Cookie != "sid=new" {
		t.Fatalf("cookie = %q", loaded.Cookie)
	}
}

func TestRefreshFailureActivityAndRecovery(t *testing.T) {
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprint(w, `{"access_token":"new","expires_in":3600}`)
	}))
	defer server.Close()

	service, st := newCredentialServices(t)
	item := domain.Credential{Name: "refreshable", SiteURL: server.URL}
	if err := service.Create(context.Background(), &item, domain.CredentialPayload{Authorization: "Bearer old",
		Refresh: &domain.RefreshRule{URL: "/refresh"}}); err != nil {
		t.Fatal(err)
	}
	refresher := NewRefresher(st, service, server.Client(), time.Millisecond)
	for i := 0; i < 2; i++ {
		if err := refresher.Refresh(context.Background(), item.ID); err == nil {
			t.Fatal("expected refresh failure")
		}
	}
	failed, _, _ := service.Load(context.Background(), item.ID)
	activities, _ := st.RecentActivity(context.Background(), 10)
	if failed.Status != "refresh_failed" || activityCount(activities, "credential_refresh_failed") != 1 {
		t.Fatalf("failed status/activity = %q/%#v", failed.Status, activities)
	}
	healthy.Store(true)
	time.Sleep(2 * time.Millisecond)
	refresher.refreshDue(context.Background())
	recovered, _, _ := service.Load(context.Background(), item.ID)
	activities, _ = st.RecentActivity(context.Background(), 10)
	if recovered.Status != "active" || activityCount(activities, "credential_refresh_recovered") != 1 {
		t.Fatalf("recovered status/activity = %q/%#v", recovered.Status, activities)
	}
}

func TestSuccessfulRenewalMakesBoundSupplierDueAndWakesScheduler(t *testing.T) {
	t.Parallel()
	service, st := newCredentialServices(t)
	credential := domain.Credential{Name: "renewed", SiteURL: "https://renew.example"}
	payload := domain.CredentialPayload{Authorization: "Bearer old-token"}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "bound", BaseURL: "https://renew.example", Adapter: "newapi", Enabled: true,
		CredentialID: &credentialID, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.MarkSupplierResult(context.Background(), supplier.ID, "healthy", "", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	woken := make(chan struct{}, 1)
	service.SetCredentialUpdatedHook(func() { woken <- struct{}{} })
	loadedCredential, loadedPayload, err := service.Load(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedPayload.Authorization = "Bearer renewed-token"
	successAt := time.Now().UTC()
	status := domain.CredentialRenewalStatus{Configured: true, Enabled: true, State: "healthy",
		LastAttemptAt: &successAt, LastSuccessAt: &successAt}
	if err := service.CommitRenewal(context.Background(), loadedCredential, loadedPayload, nil, status); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woken:
	default:
		t.Fatal("successful renewal did not wake the collection scheduler")
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.NextCollectionAt == nil || updated.NextCollectionAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("renewed supplier was not made immediately due: %#v err=%v", updated, err)
	}
	due, err := st.DueSuppliers(context.Background(), time.Now().UTC().Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].ID != supplier.ID {
		t.Fatalf("due suppliers after renewal = %#v err=%v", due, err)
	}
}

func TestRefreshDueSkipsActiveBrowserCustody(t *testing.T) {
	service, st := newCredentialServices(t)
	ctx := context.Background()
	supplier := domain.Supplier{Name: "browser-managed", BaseURL: "https://custody.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SavePairingToken(ctx, "custody-token", supplier.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Minute)
	credential := domain.Credential{Name: "browser session", Kind: "browser_pairing", SiteURL: supplier.BaseURL,
		ExpiresAt: &expiresAt}
	if err := service.CreateFromPairingWithCustody(ctx, "custody-token", now, supplier.ID, &credential,
		domain.CredentialPayload{Cookie: "session=managed"}, supplier.BaseURL, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	refresher := NewRefresher(st, service, http.DefaultClient, time.Nanosecond)
	refresher.refreshDue(ctx)
	loaded, _, err := service.Load(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	activities, err := st.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "active" || activityCount(activities, "credential_refresh_failed") != 0 {
		t.Fatalf("browser-managed credential was refreshed: status=%q activities=%#v", loaded.Status, activities)
	}
	if err := st.RevokeBrowserCustodyBySupplier(ctx, supplier.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	refresher.refreshDue(ctx)
	loaded, _, err = service.Load(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	activities, err = st.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "refresh_failed" || activityCount(activities, "credential_refresh_failed") != 1 {
		t.Fatalf("revoked custody did not restore normal refresh lifecycle: status=%q activities=%#v", loaded.Status, activities)
	}
}

func TestConfigureExpiredCredentialWakesRenewalLoop(t *testing.T) {
	t.Parallel()
	service, st := newCredentialServices(t)
	credential := domain.Credential{Name: "expired", Kind: "password_login", SiteURL: "https://login.example"}
	if err := service.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer expired-token"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCredentialStatus(context.Background(), credential.ID, "auth_expired"); err != nil {
		t.Fatal(err)
	}
	refresher := NewRefresher(st, service, http.DefaultClient, 5*time.Minute)
	rule := defaultPasswordLoginRule()
	rule.Enabled = true
	rule.Username = "operator@example.com"
	rule.Password = "secret-password"
	detail, err := refresher.ConfigureLogin(context.Background(), credential.ID, rule, false)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Status.State != "auth_expired" {
		t.Fatalf("configured renewal state = %#v", detail.Status)
	}
	select {
	case <-refresher.wake:
	default:
		t.Fatal("configuring an expired credential did not wake renewal")
	}
}

func TestRefreshCapturedSub2CandidateUsesDirectResponseFallbacks(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["refresh_token"] != "captured-refresh-token" {
			http.Error(w, "bad refresh body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"fresh-access-token","refresh_token":"rotated-refresh-token","expires_in":3600}`)
	}))
	defer server.Close()
	service, st := newCredentialServices(t)
	refresher := NewRefresher(st, service, server.Client(), time.Minute)
	supplier := domain.Supplier{Name: "sub2", BaseURL: server.URL, Adapter: "sub2api_upstream", Enabled: true}
	payload := domain.CredentialPayload{Authorization: "Bearer expired-access-token",
		LocalStorage: map[string]string{"refresh_token": "captured-refresh-token"}}
	refreshed, expiresAt, err := refresher.RefreshCapturedCandidate(context.Background(), supplier, payload)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Authorization != "Bearer fresh-access-token" || refreshed.LocalStorage["refresh_token"] != "rotated-refresh-token" ||
		refreshed.Refresh == nil || expiresAt == nil || time.Until(*expiresAt) < 59*time.Minute {
		t.Fatalf("refreshed candidate = %#v expires=%v", refreshed, expiresAt)
	}
}

func newCredentialServices(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewService(st, vault), st
}

func unsignedJWT(expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(expiry.Unix(), 10) + `}`))
	return header + "." + claims + ".signature"
}

func activityCount(items []domain.Activity, kind string) int {
	count := 0
	for _, item := range items {
		if item.Kind == kind {
			count++
		}
	}
	return count
}
