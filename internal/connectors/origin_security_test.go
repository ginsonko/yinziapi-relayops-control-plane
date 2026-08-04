package connectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNewAPIRejectsCredentialFromAnotherOriginBeforeRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(response, "unexpected request", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	credentials, credentialID := newSub2TestCredential(t, "https://different-origin.invalid", domain.CredentialPayload{
		Authorization: "Bearer must-not-be-sent",
	})
	connector := NewNewAPI(upstream.Client(), credentials)
	_, err := connector.Collect(context.Background(), domain.Supplier{
		ID: 1, BaseURL: upstream.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err == nil || !strings.Contains(err.Error(), "credential origin mismatch") {
		t.Fatalf("Collect error = %v, want credential origin mismatch", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func TestNewAPICredentialDoesNotFollowCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var escapedRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		escapedRequests.Add(1)
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-Secret") != "" {
			t.Error("credential escaped to redirect destination")
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	credentials, credentialID := newSub2TestCredential(t, source.URL, domain.CredentialPayload{
		Authorization: "Bearer must-not-leave",
		Headers:       map[string]string{"X-Secret": "must-not-leave"},
	})
	connector := NewNewAPI(source.Client(), credentials)
	_, err := connector.Collect(context.Background(), domain.Supplier{
		ID: 1, BaseURL: source.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("Collect error = %v, want cross-origin redirect rejection", err)
	}
	if got := escapedRequests.Load(); got != 0 {
		t.Fatalf("redirect destination requests = %d, want 0", got)
	}
}

func TestSub2APICredentialDoesNotFollowCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var escapedRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		escapedRequests.Add(1)
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-API-Key") != "" {
			t.Error("Sub2API credential escaped to redirect destination")
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusPermanentRedirect)
	}))
	defer source.Close()

	credentials, credentialID := newSub2TestCredential(t, source.URL, domain.CredentialPayload{
		Authorization: "Bearer must-not-leave",
		Headers:       map[string]string{"X-API-Key": "must-not-leave"},
	})
	connector := NewSub2API(source.Client(), credentials)
	_, err := connector.Collect(context.Background(), domain.Supplier{
		ID: 1, BaseURL: source.URL, Adapter: "sub2api_upstream", CredentialID: &credentialID,
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("Collect error = %v, want cross-origin redirect rejection", err)
	}
	if got := escapedRequests.Load(); got != 0 {
		t.Fatalf("redirect destination requests = %d, want 0", got)
	}
}
