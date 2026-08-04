package credentials

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestRemoteWriteGateBlocksCredentialRenewalAndLoginBeforeNetworkOrStateChange(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"success":true,"data":{"access_token":"unexpected"}}`))
	}))
	defer upstream.Close()

	database, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(database, vault)
	credential := domain.Credential{Name: "candidate renewal", Kind: "oauth", SiteURL: upstream.URL}
	payload := domain.CredentialPayload{
		Authorization: "Bearer preserved-token",
		LocalStorage:  map[string]string{"refresh_token": "preserved-refresh"},
		Refresh: &domain.RefreshRule{
			URL: "/api/v1/auth/refresh", Method: http.MethodGet,
			TokenJSONPath: "data.access_token",
		},
	}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatal(err)
	}
	beforeCredential, beforePayload, err := service.Load(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}

	refresher := NewRefresher(database, service, upstream.Client(), time.Minute).
		WithRemoteWriteGate(remotewrite.New(false))
	if err := refresher.Refresh(context.Background(), credential.ID); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("refresh error=%v, want remote-write gate", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("blocked GET refresh reached upstream %d times", requests.Load())
	}
	afterCredential, afterPayload, err := service.Load(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeCredential, afterCredential) || !reflect.DeepEqual(beforePayload, afterPayload) {
		t.Fatalf("blocked renewal changed credential\nbefore=%#v %#v\nafter=%#v %#v",
			beforeCredential, beforePayload, afterCredential, afterPayload)
	}

	loginView := domain.CredentialLoginRuleView{
		URL: "/api/user/login", Method: http.MethodPost, ContentType: "application/json",
		UsernameField: "username", PasswordField: "password", TokenJSONPath: "data.access_token",
	}
	if _, _, err := refresher.PasswordLoginCandidate(context.Background(), upstream.URL, "user", "password", loginView, true); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("password login error=%v, want remote-write gate", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("blocked password login reached upstream %d times", requests.Load())
	}

	supplier := domain.Supplier{Name: "Sub2 candidate", BaseURL: upstream.URL, Adapter: "sub2api_upstream", Enabled: true}
	captured := domain.CredentialPayload{CapturedOrigin: upstream.URL, LocalStorage: map[string]string{"refresh_token": "captured-refresh"}}
	if _, _, err := refresher.RefreshCapturedCandidate(context.Background(), supplier, captured); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("captured refresh error=%v, want remote-write gate", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("blocked captured refresh reached upstream %d times", requests.Load())
	}
}
