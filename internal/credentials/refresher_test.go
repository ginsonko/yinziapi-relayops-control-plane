package credentials

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestRefreshDoesNotForwardSecretsAcrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var escapedRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		escapedRequests.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(database, vault)
	credential := domain.Credential{Name: "refresh test", Kind: "oauth", SiteURL: source.URL}
	payload := domain.CredentialPayload{
		Authorization: "Bearer old-access-token",
		LocalStorage:  map[string]string{"refresh_token": "refresh-secret"},
		Refresh: &domain.RefreshRule{
			URL: "/refresh", Method: http.MethodPost,
			Headers: map[string]string{"Authorization": "Bearer ${refresh_token}"},
			Body:    `{"refresh_token":"${refresh_token}"}`,
		},
	}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatal(err)
	}
	refresher := NewRefresher(database, service, source.Client(), 0)
	err = refresher.Refresh(context.Background(), credential.ID)
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("Refresh error = %v, want cross-origin redirect rejection", err)
	}
	if got := escapedRequests.Load(); got != 0 {
		t.Fatalf("redirect destination requests = %d, want 0", got)
	}
}
