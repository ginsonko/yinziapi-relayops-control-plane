package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDiagnosticRetryRecoversPricingGETAfter429WithoutWrites(t *testing.T) {
	var reads atomic.Int64
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writes.Add(1)
		}
		if reads.Add(1) == 1 {
			response.Header().Set("Retry-After", "0")
			http.Error(response, "rate limited", http.StatusTooManyRequests)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"success":true,"data":[]}`)
	}))
	defer server.Close()

	session := newDiagnosticSession(31)
	response, err := session.request(context.Background(), server.Client(), diagnosticRequest{
		EndpointType: "catalog", AuthCapability: authAnonymous, Method: http.MethodGet,
		URL: server.URL + "/api/pricing", RetryGET: true,
	})
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("pricing GET after 429 = status %d, err %v", response.StatusCode, err)
	}
	if reads.Load() != 2 || writes.Load() != 0 {
		t.Fatalf("reads = %d, writes = %d", reads.Load(), writes.Load())
	}
	if len(session.result().Requests) != 2 || session.result().Requests[0].ErrorClass != "rate_limited" {
		t.Fatalf("diagnostics = %#v", session.result().Requests)
	}
}
