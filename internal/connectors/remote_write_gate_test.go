package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

func TestRemoteWriteGateAllowsOnlyExactFixedSub2SemanticReadPost(t *testing.T) {
	var fixedRequests atomic.Int64
	var overriddenRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts/today-stats/batch":
			fixedRequests.Add(1)
			fmt.Fprint(response, `{"code":0,"data":{"stats":{}}}`)
		case "/api/v1/admin/accounts/custom-batch":
			overriddenRequests.Add(1)
			fmt.Fprint(response, `{"mutated":true}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := connectorClientWithRemoteWrites(server.Client(), false)
	connector := NewSub2API(client, nil)
	connector.diagnostics = newDiagnosticSession(1)

	if _, err := connector.sub2Request(context.Background(), server.URL,
		"/api/v1/admin/accounts/today-stats/batch", http.MethodPost,
		map[string]any{"account_ids": []int64{1}}, domain.CredentialPayload{}); err != nil {
		t.Fatalf("fixed semantic-read POST was blocked: %v", err)
	}
	supplier := domain.Supplier{Metadata: map[string]string{
		"sub2api_admin_today_stats_path": "/api/v1/admin/accounts/custom-batch",
	}}
	overriddenPath := metadataPath(supplier, "sub2api_admin_today_stats_path", "/api/v1/admin/accounts/today-stats/batch")
	if _, err := connector.sub2Request(context.Background(), server.URL, overriddenPath, http.MethodPost,
		map[string]any{"account_ids": []int64{1}}, domain.CredentialPayload{}); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("metadata-overridden POST error=%v, want remote-write gate", err)
	}
	if fixedRequests.Load() != 1 || overriddenRequests.Load() != 0 {
		t.Fatalf("Sub2 request counts fixed=%d overridden=%d", fixedRequests.Load(), overriddenRequests.Load())
	}
}

func TestRemoteWriteGateBlocksJSONConnectorPostAndPreservesJSONGet(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"groups":[{"id":"economy","name":"Economy","ratio":"0.05"}]}`)
	}))
	defer server.Close()
	groupMapping := &JSONGroupMapping{
		ItemsPath: "$.groups[*]", ExternalID: JSONFieldSpec{Path: "$.id", Required: true},
		Name: JSONFieldSpec{Path: "$.name", Required: true}, Ratio: JSONFieldSpec{Path: "$.ratio", Required: true},
	}
	specFor := func(method string) JSONConnectorSpec {
		endpoint := JSONEndpointSpec{Name: "catalog", Method: method, Path: "/api/catalog", Groups: groupMapping}
		if method == http.MethodPost {
			endpoint.Body = json.RawMessage(`{"scope":"all"}`)
		}
		return JSONConnectorSpec{SchemaVersion: 1, Name: method + " catalog", Endpoints: []JSONEndpointSpec{endpoint}}
	}

	postConnector, err := NewJSONSpec(connectorClientWithRemoteWrites(server.Client(), false), nil, specFor(http.MethodPost))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postConnector.Collect(context.Background(), domain.Supplier{ID: 1, BaseURL: server.URL}); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("JSON POST error=%v, want remote-write gate", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("blocked JSON POST reached upstream %d times", requests.Load())
	}

	getConnector, err := NewJSONSpec(connectorClientWithRemoteWrites(server.Client(), false), nil, specFor(http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := getConnector.Collect(context.Background(), domain.Supplier{ID: 1, BaseURL: server.URL})
	if err != nil {
		t.Fatalf("JSON GET was blocked: %v", err)
	}
	if requests.Load() != 1 || len(catalog.Groups) != 1 || catalog.Groups[0].Ratio != "0.05" {
		t.Fatalf("JSON GET requests=%d catalog=%#v", requests.Load(), catalog)
	}
}

func connectorClientWithRemoteWrites(source *http.Client, enabled bool) *http.Client {
	clone := *source
	clone.Transport = remotewrite.New(enabled).Wrap(source.Transport)
	return &clone
}
