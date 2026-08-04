package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type stubJSONCredentialLoader struct {
	credential domain.Credential
	payload    domain.CredentialPayload
	err        error
	loads      int
}

func (s *stubJSONCredentialLoader) Load(context.Context, int64) (domain.Credential, domain.CredentialPayload, error) {
	s.loads++
	return s.credential, s.payload, s.err
}

func TestParseJSONConnectorSpecRejectsExecutableAndUnsafeRules(t *testing.T) {
	t.Parallel()
	valid := `{
  "schema_version": 1,
  "name": "safe catalog",
  "endpoints": [{
    "name": "catalog",
    "method": "GET",
    "path": "/api/catalog",
    "models": {
      "items_path": "$.data[*]",
      "external_id": {"path": "$.id", "required": true},
      "name": {"path": "$.name"}
    }
  }]
}`
	if _, err := ParseJSONConnectorSpec([]byte(valid)); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	tests := []struct {
		name    string
		rewrite func(string) string
		want    string
	}{
		{"unknown script field", func(value string) string {
			return strings.Replace(value, `"path": "/api/catalog",`, `"path": "/api/catalog", "script": "return process.env",`, 1)
		}, "unknown field"},
		{"absolute URL", func(value string) string {
			return strings.Replace(value, `"/api/catalog"`, `"https://attacker.invalid/catalog"`, 1)
		}, "must not contain"},
		{"unsupported method", func(value string) string { return strings.Replace(value, `"GET"`, `"DELETE"`, 1) }, "GET or POST"},
		{"executable JSONPath filter", func(value string) string { return strings.Replace(value, `$.data[*]`, `$.data[?(@.enabled)]`, 1) }, "unsupported bracket selector"},
		{"trailing document", func(value string) string { return value + `{}` }, "multiple documents"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseJSONConnectorSpec([]byte(test.rewrite(valid)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateJSONConnectorSpecRejectsUnsafeHeadersAndLimits(t *testing.T) {
	t.Parallel()
	spec := testJSONSpec()
	spec.Endpoints[0].Headers = map[string]JSONHeaderSpec{"Authorization": {Static: "Bearer plaintext-secret"}}
	if err := ValidateJSONConnectorSpec(spec); err == nil || !strings.Contains(err.Error(), "credential_ref") {
		t.Fatalf("static authorization error = %v", err)
	}
	for _, header := range []string{"X-API-Key", "X-Token", "X-Amz-Security-Token", "Secret-Key"} {
		spec = testJSONSpec()
		spec.Endpoints[0].Headers = map[string]JSONHeaderSpec{header: {Static: "plaintext-secret"}}
		if err := ValidateJSONConnectorSpec(spec); err == nil || !strings.Contains(err.Error(), "credential_ref") {
			t.Fatalf("static %s error = %v", header, err)
		}
	}
	spec = testJSONSpec()
	spec.Endpoints[0].Headers = map[string]JSONHeaderSpec{"Host": {Static: "attacker.invalid"}}
	if err := ValidateJSONConnectorSpec(spec); err == nil || !strings.Contains(err.Error(), "not configurable") {
		t.Fatalf("host header error = %v", err)
	}
	spec = testJSONSpec()
	spec.Endpoints[0].Pagination.MaxPages = maximumJSONSpecPages + 1
	if err := ValidateJSONConnectorSpec(spec); err == nil || !strings.Contains(err.Error(), "max_pages") {
		t.Fatalf("pagination limit error = %v", err)
	}
	spec = testJSONSpec()
	spec.Limits.MaxResponseBytes = maximumJSONSpecResponseSize + 1
	if err := ValidateJSONConnectorSpec(spec); err == nil || !strings.Contains(err.Error(), "max_response_bytes") {
		t.Fatalf("response limit error = %v", err)
	}
}

func TestJSONSpecCollectsPaginatedCatalogBalanceAndPrices(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	requestedPages := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer encrypted-token" {
			http.Error(response, "missing authorization", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-Client") != "procurement-console" {
			http.Error(response, "missing static header", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/catalog":
			page := request.URL.Query().Get("page")
			mu.Lock()
			requestedPages = append(requestedPages, page)
			mu.Unlock()
			switch page {
			case "1":
				fmt.Fprint(response, `{"data":[
 {"group":{"id":"cheap","label":"低价组","ratio":0.05},"id":"gpt-5.6-sol","name":"GPT Sol","billing":"ratio","input":1,"output":1,"cache":0.25,"currency":"USD","tags":["fast","text"]},
 {"group":{"id":"cheap","label":"低价组","ratio":0.05},"id":"claude-opus","name":"Claude Opus","billing":"ratio","input":2,"output":5,"cache":0.1,"currency":"USD","tags":["text"]}
]}`)
			case "2":
				fmt.Fprint(response, `{"data":[
 {"group":{"id":"stable","label":"稳定组","ratio":0.1},"id":"gpt-5.6-sol","name":"GPT Sol","billing":"ratio","input":1,"output":1,"cache":0.25,"currency":"USD","tags":["stable"]}
]}`)
			default:
				fmt.Fprint(response, `{"data":[]}`)
			}
		case "/api/account":
			if request.Method != http.MethodPost {
				http.Error(response, "balance endpoint requires POST", http.StatusMethodNotAllowed)
				return
			}
			var requestBody map[string]bool
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil || !requestBody["detail"] {
				http.Error(response, "unexpected request body", http.StatusBadRequest)
				return
			}
			fmt.Fprint(response, `{"account":{"balance":"123.45","available":120,"currency":"CNY","daily_spend":3.5,"runway_hours":822.5,"expires_at":"2026-08-01T00:00:00Z"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialID := int64(9)
	loader := &stubJSONCredentialLoader{
		credential: domain.Credential{SiteURL: server.URL},
		payload:    domain.CredentialPayload{Authorization: "Bearer encrypted-token"},
	}
	connector, err := NewJSONSpec(server.Client(), loader, testJSONSpec())
	if err != nil {
		t.Fatalf("NewJSONSpec: %v", err)
	}
	result, err := connector.CollectObservations(context.Background(), domain.Supplier{ID: 42, BaseURL: server.URL, CredentialID: &credentialID})
	if err != nil {
		t.Fatalf("CollectObservations: %v", err)
	}
	if loader.loads != 1 {
		t.Fatalf("credential loads = %d, want 1", loader.loads)
	}
	mu.Lock()
	pages := append([]string(nil), requestedPages...)
	mu.Unlock()
	if strings.Join(pages, ",") != "1,2,3" {
		t.Fatalf("requested pages = %v, want [1 2 3]", pages)
	}
	if len(result.Catalog.Groups) != 2 {
		t.Fatalf("groups = %#v", result.Catalog.Groups)
	}
	if result.Catalog.Groups[0].ExternalID != "cheap" || result.Catalog.Groups[0].Ratio != "0.05" {
		t.Fatalf("first group = %#v", result.Catalog.Groups[0])
	}
	if len(result.Catalog.Models) != 3 {
		t.Fatalf("models = %#v", result.Catalog.Models)
	}
	cheapGPT := findModel(t, result.Catalog.Models, "gpt-5.6-sol", "cheap")
	if cheapGPT.ModelRatio != "1" || cheapGPT.CompletionRatio != "1" || cheapGPT.CacheRatio != "0.25" {
		t.Fatalf("equal-valued pricing fields were not all preserved: %#v", cheapGPT)
	}
	if cheapGPT.PriceFunction.InputPer1M != "1" || cheapGPT.PriceFunction.OutputPer1M != "1" || cheapGPT.PriceFunction.CacheReadPer1M != "0.25" || cheapGPT.PriceFunction.Currency != "USD" {
		t.Fatalf("normalized price function = %#v", cheapGPT.PriceFunction)
	}
	if cheapGPT.Metadata["first_tag"] != "fast" {
		t.Fatalf("model metadata = %#v", cheapGPT.Metadata)
	}
	if result.Balance == nil || result.Balance.Amount != "123.45" || result.Balance.Available != "120" || result.Balance.Currency != "CNY" {
		t.Fatalf("balance = %#v", result.Balance)
	}
	if result.Balance.ExpiresAt == nil || result.Balance.ExpiresAt.UTC().Format(time.RFC3339) != "2026-08-01T00:00:00Z" {
		t.Fatalf("balance expiry = %#v", result.Balance.ExpiresAt)
	}
	if len(result.Prices) != 5 {
		t.Fatalf("price observations = %d, want 5", len(result.Prices))
	}
	if result.Catalog.Evidence.SourceType != "authenticated_declared_json_api" || result.Catalog.Evidence.ResponseHash == "" {
		t.Fatalf("evidence = %#v", result.Catalog.Evidence)
	}
}

func TestJSONSpecExtractsObjectEntryMaps(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(response, `{"group_ratio":{"default":1,"cheap":0.05}}`)
	}))
	defer server.Close()
	spec := JSONConnectorSpec{
		SchemaVersion: 1,
		Name:          "map shaped groups",
		Endpoints: []JSONEndpointSpec{{
			Name:   "groups",
			Method: http.MethodGet,
			Path:   "/groups",
			Groups: &JSONGroupMapping{
				ItemsPath:     "$.group_ratio",
				ObjectEntries: true,
				ExternalID:    JSONFieldSpec{Path: "$.key", Required: true},
				Name:          JSONFieldSpec{Path: "$.key"},
				Ratio:         JSONFieldSpec{Path: "$.value", Required: true},
			},
		}},
	}
	connector, err := NewJSONSpec(server.Client(), nil, spec)
	if err != nil {
		t.Fatalf("NewJSONSpec: %v", err)
	}
	catalog, err := connector.Collect(context.Background(), domain.Supplier{ID: 7, BaseURL: server.URL})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(catalog.Groups) != 2 || catalog.Groups[0].ExternalID != "cheap" || catalog.Groups[0].Ratio != "0.05" || catalog.Groups[1].ExternalID != "default" {
		t.Fatalf("groups = %#v", catalog.Groups)
	}
}

func TestJSONSpecEnforcesResponseLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(response, `{"data":[{"id":"%s"}]}`, strings.Repeat("x", 256))
	}))
	defer server.Close()
	spec := testJSONSpec()
	spec.Limits.MaxResponseBytes = 64
	spec.Endpoints = spec.Endpoints[:1]
	spec.Endpoints[0].Headers = nil
	connector, err := NewJSONSpec(server.Client(), nil, spec)
	if err != nil {
		t.Fatalf("NewJSONSpec: %v", err)
	}
	_, err = connector.Collect(context.Background(), domain.Supplier{ID: 1, BaseURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "response body") {
		t.Fatalf("limit error = %v", err)
	}
}

func TestJSONSpecBlocksCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(response, `{"data":[{"id":"stolen"}]}`)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/catalog", http.StatusFound)
	}))
	defer source.Close()
	spec := testJSONSpec()
	spec.Endpoints = spec.Endpoints[:1]
	spec.Endpoints[0].Headers = nil
	connector, err := NewJSONSpec(source.Client(), nil, spec)
	if err != nil {
		t.Fatalf("NewJSONSpec: %v", err)
	}
	_, err = connector.Collect(context.Background(), domain.Supplier{ID: 1, BaseURL: source.URL})
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestJSONSpecHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(response, `{"data":[]}`)
	}))
	defer server.Close()
	spec := testJSONSpec()
	spec.Endpoints = spec.Endpoints[:1]
	spec.Endpoints[0].Headers = nil
	connector, err := NewJSONSpec(server.Client(), nil, spec)
	if err != nil {
		t.Fatalf("NewJSONSpec: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = connector.Collect(ctx, domain.Supplier{ID: 1, BaseURL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("deadline error = %v", err)
	}
}

func testJSONSpec() JSONConnectorSpec {
	credentialHeaders := map[string]JSONHeaderSpec{
		"Authorization": {CredentialRef: "authorization"},
		"X-Client":      {Static: "procurement-console"},
	}
	return JSONConnectorSpec{
		SchemaVersion: 1,
		Name:          "test upstream",
		Limits:        JSONConnectorLimits{TimeoutSeconds: 5, MaxResponseBytes: 1 << 20},
		Endpoints: []JSONEndpointSpec{
			{
				Name:       "catalog",
				Method:     http.MethodGet,
				Path:       "/api/catalog",
				Headers:    credentialHeaders,
				Pagination: &JSONPaginationSpec{Mode: "page", Parameter: "page", StartPage: 1, MaxPages: 4},
				Groups: &JSONGroupMapping{
					ItemsPath:  "$.data[*]",
					ExternalID: JSONFieldSpec{Path: "$.group.id", Required: true},
					Name:       JSONFieldSpec{Path: "$.group.label"},
					Ratio:      JSONFieldSpec{Path: "$.group.ratio"},
					ModelIDs:   JSONFieldSpec{Path: "$.id"},
				},
				Models: &JSONModelMapping{
					ItemsPath:       "$.data[*]",
					ExternalID:      JSONFieldSpec{Path: "$.id", Required: true},
					Name:            JSONFieldSpec{Path: "$.name"},
					Group:           JSONFieldSpec{Path: "$.group.id"},
					BillingMode:     JSONFieldSpec{Path: "$.billing"},
					ModelRatio:      JSONFieldSpec{Path: "$.input"},
					CompletionRatio: JSONFieldSpec{Path: "$.output"},
					CacheRatio:      JSONFieldSpec{Path: "$.cache"},
					Currency:        JSONFieldSpec{Path: "$.currency"},
					PriceFunction: JSONPriceFunctionMapping{
						Currency:       JSONFieldSpec{Path: "$.currency"},
						Basis:          JSONFieldSpec{Constant: "declared_money"},
						InputPer1M:     JSONFieldSpec{Path: "$.input"},
						OutputPer1M:    JSONFieldSpec{Path: "$.output"},
						CacheReadPer1M: JSONFieldSpec{Path: "$.cache"},
					},
					MetadataPaths: map[string]string{"first_tag": "$.tags[0]"},
				},
			},
			{
				Name:    "balance",
				Method:  http.MethodPost,
				Path:    "/api/account",
				Headers: credentialHeaders,
				Body:    json.RawMessage(`{"detail":true}`),
				Balance: &JSONBalanceMapping{
					RootPath:    "$.account",
					Amount:      JSONFieldSpec{Path: "$.balance", Required: true},
					Available:   JSONFieldSpec{Path: "$.available"},
					Currency:    JSONFieldSpec{Path: "$.currency"},
					DailySpend:  JSONFieldSpec{Path: "$.daily_spend"},
					RunwayHours: JSONFieldSpec{Path: "$.runway_hours"},
					ExpiresAt:   JSONFieldSpec{Path: "$.expires_at"},
				},
			},
		},
	}
}

func findModel(t *testing.T, models []domain.ModelOffer, id, group string) domain.ModelOffer {
	t.Helper()
	for _, model := range models {
		if model.ExternalID == id && model.Group == group {
			return model
		}
	}
	t.Fatalf("model %s/%s not found in %#v", id, group, models)
	return domain.ModelOffer{}
}
