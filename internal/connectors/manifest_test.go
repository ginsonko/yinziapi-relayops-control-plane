package connectors

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestParseConnectorManifestIsStrictSameOriginAndDeclarative(t *testing.T) {
	valid := `{
		"schema_version":"relayops.connector-manifest/v1",
		"name":"newapi-v1",
		"adapter_family":"newapi",
		"origins":{"schemes":["https"],"hosts":["api.example.com","*.relay.example"]},
		"auth":{"header":"Authorization","scheme":"bearer","user_header":"New-Api-User"},
		"capabilities":{
			"status":{"paths":["/api/status"],"contract":"native"},
			"balance":{"paths":["/api/user/self"],"contract":"json","json_paths":{"amount":"data.balance","currency":"data.currency"}}
		},
		"login":{"url":"/api/user/login","method":"POST","content_type":"application/json","username_field":"username","password_field":"password","user_id_json_path":"data.id","success_json_path":"success","message_json_path":"message","cookie_response":true}
	}`
	manifest, canonical, err := ParseConnectorManifest([]byte(valid))
	if err != nil {
		t.Fatalf("ParseConnectorManifest: %v", err)
	}
	if len(canonical) == 0 || manifest.AdapterFamily != "newapi" || manifest.Capabilities["balance"].Contract != "json" {
		t.Fatalf("manifest = %#v canonical=%q", manifest, canonical)
	}
	if !ConnectorManifestMatchesOrigin(manifest, "https://api.example.com/pricing") ||
		!ConnectorManifestMatchesOrigin(manifest, "https://cn.relay.example") ||
		ConnectorManifestMatchesOrigin(manifest, "https://relay.example") ||
		ConnectorManifestMatchesOrigin(manifest, "http://api.example.com") {
		t.Fatal("origin allowlist matching is not exact")
	}

	for name, rewrite := range map[string]func(string) string{
		"unknown field": func(value string) string {
			return strings.Replace(value, `"name":"newapi-v1"`, `"name":"newapi-v1","script":"alert(1)"`, 1)
		},
		"cross origin": func(value string) string {
			return strings.Replace(value, `"/api/status"`, `"https://evil.example/api/status"`, 1)
		},
		"static secret": func(value string) string {
			return strings.Replace(value, `"scheme":"bearer"`, `"scheme":"bearer","value":"secret"`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseConnectorManifest([]byte(rewrite(valid))); err == nil {
				t.Fatal("unsafe manifest unexpectedly passed validation")
			}
		})
	}
}

type staticManifestLoader struct {
	record domain.ConnectorManifestRecord
}

func (s staticManifestLoader) BoundConnectorManifest(context.Context, int64) (domain.ConnectorManifestRecord, bool, error) {
	return s.record, true, nil
}

func TestFactoryResolvesBoundManifestIntoNativeMetadata(t *testing.T) {
	manifest, _, err := ParseConnectorManifest([]byte(`{
		"schema_version":"relayops.connector-manifest/v1","name":"local-newapi","adapter_family":"newapi",
		"origins":{"schemes":["https"],"hosts":["api.example.com"]},
		"auth":{"header":"X-API-Key","scheme":"raw"},
		"capabilities":{"models":{"paths":["/custom/models","/v1/models"]},"usage":{"paths":["/custom/usage"]}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	factory := NewFactory(nil, nil)
	factory.SetConnectorManifestLoader(staticManifestLoader{record: domain.ConnectorManifestRecord{
		ID: 7, AdapterFamily: "newapi", ValidationStatus: "valid", Manifest: manifest,
	}})
	resolved, bound, err := factory.resolveConnectorManifest(context.Background(), domain.Supplier{
		ID: 1, BaseURL: "https://api.example.com", Adapter: "newapi", Metadata: map[string]string{"kept": "yes"},
	})
	if err != nil || !bound {
		t.Fatalf("resolveConnectorManifest = %#v %v %v", resolved, bound, err)
	}
	if resolved.Metadata["newapi_models_path"] != "/custom/models" || resolved.Metadata["newapi_catalog_auth_header"] != "X-API-Key" ||
		resolved.Metadata["connector_manifest_id"] != "7" || resolved.Metadata["kept"] != "yes" {
		t.Fatalf("resolved metadata = %#v", resolved.Metadata)
	}
}

func TestManifestJSONBalanceAndUsageContractsAreExecutableMappings(t *testing.T) {
	manifest, _, err := ParseConnectorManifest([]byte(`{
		"schema_version":"relayops.connector-manifest/v1","name":"custom-documents","adapter_family":"newapi",
		"origins":{"schemes":["https"],"hosts":["api.example.com"]},
		"capabilities":{
			"balance":{"paths":["/account"],"contract":"json","json_paths":{"amount":"result.credit","currency":"result.currency","available":"result.available","request_count":"result.calls"}},
			"usage":{"paths":["/usage"],"contract":"json","json_paths":{"daily_spend":"stats.day_cost","currency":"stats.currency","request_count":"stats.requests"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Metadata: ConnectorManifestMetadata(manifest)}
	balancePaths, ok := manifestJSONPaths(supplier, "balance")
	if !ok {
		t.Fatal("balance JSON contract was not activated")
	}
	balance, _, err := parseManifestBalance([]byte(`{"result":{"credit":"12.50","available":"11.25","currency":"USD","calls":9}}`),
		balancePaths, newAPIUnitContract{})
	if err != nil || balance.Amount != "12.50" || balance.Available != "11.25" || balance.Currency != "USD" || balance.RequestCount == nil || *balance.RequestCount != 9 {
		t.Fatalf("balance=%#v err=%v", balance, err)
	}
	usagePaths, ok := manifestJSONPaths(supplier, "usage")
	if !ok {
		t.Fatal("usage JSON contract was not activated")
	}
	usage, err := parseManifestUsage([]byte(`{"stats":{"day_cost":"0.75","currency":"USD","requests":4}}`),
		usagePaths, newAPIUnitContract{}, time.Now().UTC())
	if err != nil || usage.Amount != "0.75" || usage.RequestCount != 4 {
		t.Fatalf("usage=%#v err=%v", usage, err)
	}
}
