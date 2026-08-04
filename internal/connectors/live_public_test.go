package connectors

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// TestLivePublicUpstreamCapabilities is opt-in because it contacts real
// upstreams. It sends public GET requests only: no credentials, no model
// generation, and no mutation. Credential-restricted catalogs are expected to
// reject the anonymous request while still proving network reachability.
func TestLivePublicUpstreamCapabilities(t *testing.T) {
	if os.Getenv("RELAYOPS_LIVE_PUBLIC_CONNECTORS") != "1" {
		t.Skip("set RELAYOPS_LIVE_PUBLIC_CONNECTORS=1 to run public upstream checks")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	tests := []struct {
		name             string
		supplier         domain.Supplier
		publicCatalog    bool
		allowNetworkFlap bool
	}{
		{name: "MDKJ", supplier: domain.Supplier{ID: 901, BaseURL: "https://mdkj.lol", Adapter: "sub2api_upstream", PricingPath: "/v1/models", Metadata: map[string]string{"sub2api_mode": "upstream"}}},
		{name: "DKBY", supplier: domain.Supplier{ID: 902, BaseURL: "https://dkby.com", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "AIZZZ", supplier: domain.Supplier{ID: 903, BaseURL: "https://api.aizzz.xyz", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "BiuAPI", supplier: domain.Supplier{ID: 904, BaseURL: "https://api.biuapi.com", Adapter: "sub2api_upstream", PricingPath: "/v1/models", Metadata: map[string]string{"sub2api_mode": "upstream"}}},
		{name: "Doro", publicCatalog: true, supplier: domain.Supplier{ID: 905, BaseURL: "https://doro.lol", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "Kuaipao", publicCatalog: true, supplier: domain.Supplier{ID: 906, BaseURL: "https://kuaipao.pro", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "OPCC", publicCatalog: true, supplier: domain.Supplier{ID: 907, BaseURL: "https://opcc.store", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "52CodeFlow", publicCatalog: true, allowNetworkFlap: true, supplier: domain.Supplier{ID: 908, BaseURL: "https://apiv3.52codeflow.top", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
		{name: "CodexAPIs", publicCatalog: true, supplier: domain.Supplier{ID: 909, BaseURL: "https://www.codexapis.com", Adapter: "newapi", PricingPath: "/api/pricing", Metadata: map[string]string{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			factory := NewFactory(client, nil)
			connector, _, err := factory.ForSupplier(ctx, test.supplier)
			if err != nil {
				t.Fatalf("select connector: %v", err)
			}
			snapshot, collectErr := connector.Collect(ctx, test.supplier)
			if test.publicCatalog {
				if collectErr != nil || len(snapshot.Models) == 0 {
					if test.allowNetworkFlap && isTransientLiveError(collectErr) {
						t.Logf("known intermittent public endpoint: %v", collectErr)
						return
					}
					t.Fatalf("public catalog models=%d error=%v", len(snapshot.Models), collectErr)
				}
				return
			}
			if collectErr == nil {
				t.Fatal("credential-restricted catalog unexpectedly accepted an anonymous request")
			}
		})
	}
}

func isTransientLiveError(err error) bool {
	switch ErrorClass(err) {
	case "connection_reset", "connect_timeout", "read_timeout", "dns_error":
		return true
	default:
		return false
	}
}
