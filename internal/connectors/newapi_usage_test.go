package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNewAPICollectsBalanceHistoricalAnd24HourUsage(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/api/pricing" && request.URL.Path != "/api/status" &&
			(request.Header.Get("Authorization") != "Bearer paired-token" || request.Header.Get("New-Api-User") != "42") {
			http.Error(response, `{"success":false}`, http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"USD","display_in_currency":true}}`)
		case "/api/user/self":
			fmt.Fprint(response, `{"success":true,"data":{"quota":30965000,"used_quota":129500000,"request_count":39502,"currency":"quota"}}`)
		case "/api/data/self":
			start, startErr := strconv.ParseInt(request.URL.Query().Get("start_timestamp"), 10, 64)
			end, endErr := strconv.ParseInt(request.URL.Query().Get("end_timestamp"), 10, 64)
			if startErr != nil || endErr != nil || end-start < 23*3600 || end-start > 25*3600 {
				http.Error(response, `{"success":false,"message":"bad range"}`, http.StatusBadRequest)
				return
			}
			fmt.Fprint(response, `{"success":true,"data":[{"created_at":1,"quota":5000000,"count":10},{"created_at":2,"quota":5760000,"count":11}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{
		Authorization: "Bearer paired-token", UserID: 42,
	})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 101, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	balance := snapshot.Balance
	if balance == nil || balance.Amount != "61.93" || balance.DailySpend != "21.52" || balance.HistoricalSpend != "259" {
		t.Fatalf("balance and usage = %#v", balance)
	}
	if balance.RequestCount == nil || *balance.RequestCount != 39502 || balance.DailyRequestCount == nil || *balance.DailyRequestCount != 21 {
		t.Fatalf("request counts = %#v", balance)
	}
	if balance.UsageSource != "/api/data/self" || balance.UsageObservedAt == nil || time.Since(*balance.UsageObservedAt) > time.Minute {
		t.Fatalf("usage evidence = %#v", balance)
	}
	assertRequestEvidence(t, diagnostics.Requests, "usage", authCustomerSession, "success")
}

func TestNewAPIUsageFailureKeepsKnownBalanceAndDoesNotInventZero(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/user/self":
			fmt.Fprint(response, `{"success":true,"data":{"quota":5000000,"currency":"quota"}}`)
		case "/api/data/self":
			http.Error(response, `{"success":false}`, http.StatusUnauthorized)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 102, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "10" || snapshot.Balance.DailySpend != "" || snapshot.Balance.DailyRequestCount != nil {
		t.Fatalf("partial balance = %#v", snapshot.Balance)
	}
	if countRequests(diagnostics.Requests, "usage", authCustomerSession) != 3 {
		t.Fatalf("bounded usage fallback candidates were not tried exactly once: %#v", diagnostics.Requests)
	}
}

func TestNewAPIEmptyUsageWindowIsKnownZero(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/user/self":
			fmt.Fprint(response, `{"success":true,"data":{"quota":5000000,"currency":"quota"}}`)
		case "/api/data/self":
			fmt.Fprint(response, `{"success":true,"data":[]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	snapshot, _, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 103, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.DailySpend != "0" || snapshot.Balance.DailyRequestCount == nil || *snapshot.Balance.DailyRequestCount != 0 {
		t.Fatalf("known zero usage = %#v", snapshot.Balance)
	}
}

func TestNewAPICustomCurrencySymbolNormalizesQuotaBalance(t *testing.T) {
	t.Parallel()
	pricing := readConnectorFixture(t, "newapi_public_pricing.json")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			_, _ = response.Write(pricing)
		case "/api/status":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"CUSTOM","custom_currency_symbol":"$"}}`)
		case "/api/user/self":
			fmt.Fprint(response, `{"success":true,"data":{"quota":94066678,"currency":"quota"}}`)
		case "/api/data/self":
			fmt.Fprint(response, `{"success":true,"data":[]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	credentialService, credentialID := newConnectorCredential(t, server.URL, "browser_pairing", domain.CredentialPayload{Cookie: "session=paired"})
	snapshot, diagnostics, err := NewNewAPI(server.Client(), credentialService).CollectDetailed(context.Background(), domain.Supplier{
		ID: 104, BaseURL: server.URL, Adapter: "newapi", CredentialID: &credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "188.133356" || snapshot.Balance.Currency != "USD" ||
		snapshot.Balance.DailySpend != "0" || snapshot.Balance.Confidence != "authenticated" {
		t.Fatalf("custom currency balance = %#v", snapshot.Balance)
	}
	if !diagnostics.BalanceSucceeded {
		t.Fatalf("custom currency balance was not marked successful: %#v", diagnostics)
	}
}
