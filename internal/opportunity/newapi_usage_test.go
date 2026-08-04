package opportunity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestReadNewAPIUsageAggregatesBothWindowsWithoutExposingCredentials(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/data/flow" || request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("New-Api-User") != "9" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.URL.Query().Get("start_timestamp") == "" || request.URL.Query().Get("end_timestamp") == "" {
			http.Error(response, "missing range", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"success":true,"message":"","data":[{"model_name":"gpt-test","token_used":1000000,"count":2},{"model_name":"gpt-test","token_used":500000,"count":1},{"model_name":"image-test","token_used":0,"count":4}]}`)
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
	credential := domain.Credential{Name: "target", Kind: "newapi-management", SiteURL: upstream.URL}
	if err := service.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer secret", UserID: 9}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{ID: 1, BaseURL: upstream.URL, CredentialID: credential.ID}
	items, err := ReadNewAPIUsage(context.Background(), upstream.Client(), service, target, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || len(items) != 4 {
		t.Fatalf("requests=%d items=%#v", requests.Load(), items)
	}
	for _, item := range items {
		switch item.ModelID {
		case "gpt-test":
			if item.DemandWeight != "1.5" || item.Confidence != "0.95" {
				t.Fatalf("token usage=%#v", item)
			}
		case "image-test":
			if item.DemandWeight != "4" || item.Confidence != "0.8" {
				t.Fatalf("request usage=%#v", item)
			}
		default:
			t.Fatalf("unexpected item=%#v", item)
		}
	}
}
