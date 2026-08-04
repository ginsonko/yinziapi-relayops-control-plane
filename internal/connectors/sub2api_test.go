package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestSub2APIAdminCollectionMapsPoolStateAndCosts(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	requests := make(map[string]int)
	var todayAccountIDs []int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "admin-secret" {
			http.Error(response, "missing admin credential", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		requests[request.Method+" "+request.URL.Path]++
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")

		switch request.URL.Path {
		case "/api/v1/admin/groups/all":
			fmt.Fprint(response, `{"code":0,"message":"success","data":[
 {"id":2,"name":"economy","platform":"openai","status":"active","rate_multiplier":0.05,"peak_rate_enabled":true,"peak_start":"00:00","peak_end":"06:00","peak_rate_multiplier":0.2,"image_rate_independent":true,"image_rate_multiplier":0.7,"video_rate_independent":true,"video_rate_multiplier":0.8,"subscription_type":"standard","daily_limit_usd":40,"allow_image_generation":true},
 {"id":1,"name":"stable","platform":"anthropic","status":"active","rate_multiplier":0.1,"subscription_type":"subscription","weekly_limit_usd":100}
]}`)
		case "/api/v1/admin/accounts":
			if request.URL.Query().Get("page") != "1" || request.URL.Query().Get("page_size") != "1000" || request.URL.Query().Get("lite") != "true" {
				http.Error(response, "unexpected account query", http.StatusBadRequest)
				return
			}
			fmt.Fprint(response, `{"code":0,"message":"success","data":{"items":[
 {"id":10,"name":"openai-1","platform":"openai","type":"oauth","status":"active","schedulable":true,"rate_multiplier":1,"concurrency":5,"current_concurrency":2,"credentials_status":{"has_access_token":true},"group_ids":[2],"groups":[{"id":2,"name":"economy"}]},
 {"id":11,"name":"anthropic-1","platform":"anthropic","type":"setup-token","status":"error","schedulable":false,"error_message":"expired","concurrency":3,"current_concurrency":0,"credentials_status":{"has_setup_token":true},"groups":[{"id":1,"name":"stable"}]}
],"total":2,"page":1,"page_size":1000,"pages":1}}`)
		case "/api/v1/admin/dashboard/stats":
			fmt.Fprint(response, `{"code":0,"message":"success","data":{"total_accounts":2,"normal_accounts":1,"error_accounts":1,"ratelimit_accounts":0,"overload_accounts":0,"today_requests":7,"today_tokens":900,"today_account_cost":3.25,"total_account_cost":75}}`)
		case "/api/v1/admin/accounts/today-stats/batch":
			if request.Method != http.MethodPost {
				http.Error(response, "expected POST", http.StatusMethodNotAllowed)
				return
			}
			var payload struct {
				AccountIDs []int64 `json:"account_ids"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				http.Error(response, "invalid body", http.StatusBadRequest)
				return
			}
			mu.Lock()
			todayAccountIDs = append([]int64(nil), payload.AccountIDs...)
			mu.Unlock()
			fmt.Fprint(response, `{"code":0,"message":"success","data":{"stats":{"10":{"requests":5,"tokens":700,"cost":1.25,"standard_cost":2.5,"user_cost":3},"11":{"requests":2,"tokens":200,"cost":2,"standard_cost":4,"user_cost":4.5}}}}`)
		case "/api/v1/admin/accounts/10/models":
			fmt.Fprint(response, `{"code":0,"message":"success","data":[{"id":"gpt-5.6-sol","object":"model"},{"id":"gpt-image-2","object":"model"}]}`)
		case "/api/v1/admin/accounts/11/models":
			fmt.Fprint(response, `{"code":0,"message":"success","data":["claude-opus-4.8"]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newSub2TestCredential(t, server.URL, domain.CredentialPayload{
		Headers: map[string]string{"X-API-Key": "admin-secret"},
	})
	connector := NewSub2API(server.Client(), credentialService)
	snapshot, err := connector.Collect(context.Background(), domain.Supplier{
		ID:           42,
		BaseURL:      server.URL,
		Adapter:      "sub2api",
		CredentialID: &credentialID,
		Metadata:     map[string]string{"sub2api_mode": "admin"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	mu.Lock()
	gotTodayAccountIDs := append([]int64(nil), todayAccountIDs...)
	gotRequests := make(map[string]int, len(requests))
	for key, value := range requests {
		gotRequests[key] = value
	}
	mu.Unlock()
	if !reflect.DeepEqual(gotTodayAccountIDs, []int64{10, 11}) {
		t.Fatalf("today-stats account_ids = %v, want [10 11]", gotTodayAccountIDs)
	}
	for _, endpoint := range []string{
		"GET /api/v1/admin/groups/all",
		"GET /api/v1/admin/accounts",
		"GET /api/v1/admin/dashboard/stats",
		"POST /api/v1/admin/accounts/today-stats/batch",
		"GET /api/v1/admin/accounts/10/models",
		"GET /api/v1/admin/accounts/11/models",
	} {
		if gotRequests[endpoint] != 1 {
			t.Fatalf("request count for %s = %d, want 1; all = %#v", endpoint, gotRequests[endpoint], gotRequests)
		}
	}

	if snapshot.SupplierID != 42 || snapshot.Balance != nil {
		t.Fatalf("snapshot identity/balance = %#v", snapshot)
	}
	if len(snapshot.Groups) != 2 || len(snapshot.Models) != 3 {
		t.Fatalf("catalog sizes: groups=%d models=%d", len(snapshot.Groups), len(snapshot.Models))
	}
	economy := findSub2Group(t, snapshot.Groups, "2")
	if economy.Ratio != "0.05" || !reflect.DeepEqual(economy.ModelIDs, []string{"gpt-5.6-sol", "gpt-image-2"}) {
		t.Fatalf("economy group = %#v", economy)
	}
	wantEconomyMetadata := map[string]string{
		"account_count":             "1",
		"active_account_count":      "1",
		"schedulable_account_count": "1",
		"credential_ready_count":    "1",
		"current_concurrency":       "2",
		"concurrency_limit":         "5",
		"today_account_cost":        "1.25",
		"today_standard_cost":       "2.5",
		"today_user_cost":           "3",
		"peak_rate_multiplier":      "0.2",
		"daily_limit_usd":           "40",
	}
	for key, want := range wantEconomyMetadata {
		if got := economy.Metadata[key]; got != want {
			t.Errorf("economy metadata[%q] = %q, want %q", key, got, want)
		}
	}
	stable := findSub2Group(t, snapshot.Groups, "1")
	if stable.Metadata["error_account_count"] != "1" || stable.Metadata["schedulable_account_count"] != "0" {
		t.Fatalf("stable health metadata = %#v", stable.Metadata)
	}
	imageModel := findSub2Model(t, snapshot.Models, "gpt-image-2", "economy")
	if imageModel.Metadata["platform"] != "openai" || imageModel.Metadata["group_external_id"] != "2" {
		t.Fatalf("image model metadata = %#v", imageModel.Metadata)
	}
	if snapshot.Evidence.SourceType != "authenticated_admin_api" || snapshot.Evidence.Adapter != "sub2api" || snapshot.Evidence.Confidence != 95 || snapshot.Evidence.ResponseHash == "" {
		t.Fatalf("evidence = %#v", snapshot.Evidence)
	}
	if !warningsContain(snapshot.Evidence.Warnings, "2 个账号") {
		t.Fatalf("warnings did not include account-pool summary: %#v", snapshot.Evidence.Warnings)
	}
}

func TestSub2APIUpstreamCollectionMapsUsageAndEffectiveBilling(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer upstream-key" || request.Header.Get("X-API-Key") != "upstream-key" {
			http.Error(response, "missing API key", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/models":
			fmt.Fprint(response, `{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"},{"id":"gpt-image-2","object":"model"}]}`)
		case "/v1/usage":
			fmt.Fprint(response, `{"mode":"wallet","isValid":true,"status":"active","planName":"Economy Key","unit":"USD","remaining":19.5,"balance":20,"expires_at":"2026-08-01T00:00:00Z","usage":{"today":{"actual_cost":0.5},"total":{"actual_cost":8}}}`)
		case "/v1/sub2api/billing":
			fmt.Fprint(response, `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":0.06,"resolved_rate_multiplier":0.05,"effective_rate_multiplier":0.04,"peak_rate_enabled":true,"peak_rate_multiplier":0.8,"applied_peak_multiplier":0.8,"timezone":"Asia/Shanghai","observed_at":"2026-07-22T00:00:00Z"}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialService, credentialID := newSub2TestCredential(t, server.URL, domain.CredentialPayload{
		Authorization: "Bearer upstream-key",
		Headers:       map[string]string{"X-API-Key": "upstream-key"},
	})
	connector := NewSub2API(server.Client(), credentialService)
	snapshot, err := connector.Collect(context.Background(), domain.Supplier{
		ID:           9,
		BaseURL:      server.URL,
		Adapter:      "sub2api",
		CredentialID: &credentialID,
		Metadata:     map[string]string{"sub2api_mode": "upstream"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(snapshot.Groups) != 1 || snapshot.Groups[0].Name != "Economy Key" || snapshot.Groups[0].Ratio != "0.04" {
		t.Fatalf("group = %#v", snapshot.Groups)
	}
	if !reflect.DeepEqual(snapshot.Groups[0].ModelIDs, []string{"gpt-5.6-sol", "gpt-image-2"}) {
		t.Fatalf("model IDs = %#v", snapshot.Groups[0].ModelIDs)
	}
	if snapshot.Groups[0].Metadata["group_rate_multiplier"] != "0.06" || snapshot.Groups[0].Metadata["resolved_rate_multiplier"] != "0.05" || snapshot.Groups[0].Metadata["effective_rate_multiplier"] != "0.04" {
		t.Fatalf("billing metadata = %#v", snapshot.Groups[0].Metadata)
	}
	if snapshot.Balance == nil || snapshot.Balance.Amount != "20" || snapshot.Balance.Available != "19.5" || snapshot.Balance.DailySpend != "0.5" || snapshot.Balance.Currency != "USD" {
		t.Fatalf("balance = %#v", snapshot.Balance)
	}
	if snapshot.Balance.ExpiresAt == nil || snapshot.Balance.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00") != "2026-08-01T00:00:00Z" {
		t.Fatalf("balance expiry = %#v", snapshot.Balance.ExpiresAt)
	}
	for _, model := range snapshot.Models {
		if model.Group != "Economy Key" || model.Metadata["effective_rate_multiplier"] != "0.04" {
			t.Fatalf("model billing mapping = %#v", model)
		}
	}
	if snapshot.Evidence.SourceType != "authenticated_api_key" || snapshot.Evidence.Confidence != 92 || snapshot.Evidence.ResponseHash == "" {
		t.Fatalf("evidence = %#v", snapshot.Evidence)
	}
}

func TestSub2APIUpstreamSimpleModeKeepsCatalogWhenBillingIsUnavailable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/models":
			fmt.Fprint(response, `{"object":"list","data":[{"id":"claude-opus-4.8"}]}`)
		case "/v1/usage":
			fmt.Fprint(response, `{"mode":"subscription","isValid":true,"status":"active","planName":"Subscription Pool","unit":"USD","remaining":"7.5","subscription":{"daily_limit_usd":10,"daily_usage_usd":2.5,"expires_at":"2026-09-01T00:00:00Z"}}`)
		case "/v1/sub2api/billing":
			http.Error(response, "billing is disabled in simple mode", http.StatusNotFound)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	connector := NewSub2API(server.Client(), nil)
	snapshot, err := connector.Collect(context.Background(), domain.Supplier{
		ID:       3,
		BaseURL:  server.URL,
		Adapter:  "sub2api_upstream",
		Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].Ratio != "" || snapshot.Groups[0].Name != "Subscription Pool" {
		t.Fatalf("groups = %#v", snapshot.Groups)
	}
	if snapshot.Balance == nil || snapshot.Balance.Available != "7.5" || snapshot.Balance.SourceField != "subscription.remaining" {
		t.Fatalf("balance = %#v", snapshot.Balance)
	}
	if snapshot.Evidence.Confidence != 78 || !warningsContain(snapshot.Evidence.Warnings, "effective multiplier is temporarily unavailable") {
		t.Fatalf("evidence = %#v", snapshot.Evidence)
	}
}

func TestSub2APIAutoFallsBackFromAdminAuthorizationToUpstreamKey(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	requested := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requested = append(requested, request.URL.Path)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/admin/groups/all":
			http.Error(response, "private upstream detail", http.StatusForbidden)
		case "/v1/models":
			fmt.Fprint(response, `{"object":"list","data":[{"id":"gpt-5.6-sol"}]}`)
		case "/v1/usage", "/v1/sub2api/billing":
			http.NotFound(response, request)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	connector := NewSub2API(server.Client(), nil)
	snapshot, err := connector.Collect(context.Background(), domain.Supplier{ID: 7, BaseURL: server.URL, Adapter: "sub2api"})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	mu.Lock()
	gotRequested := append([]string(nil), requested...)
	mu.Unlock()
	if len(gotRequested) < 2 || gotRequested[0] != "/api/v1/admin/groups/all" || gotRequested[1] != "/v1/models" {
		t.Fatalf("request order = %#v", gotRequested)
	}
	if warningsContain(snapshot.Evidence.Warnings, "private upstream detail") {
		t.Fatalf("HTTP response body leaked into warnings: %#v", snapshot.Evidence.Warnings)
	}
	if !warningsContain(snapshot.Evidence.Warnings, "普通 Sub2API Key") {
		t.Fatalf("fallback warning missing: %#v", snapshot.Evidence.Warnings)
	}
	if len(snapshot.Models) != 1 || snapshot.Models[0].ExternalID != "gpt-5.6-sol" {
		t.Fatalf("models = %#v", snapshot.Models)
	}
}

func TestSub2APIRejectsOversizedRequiredResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, strings.Repeat("x", maxResponseBytes+1))
	}))
	defer server.Close()

	connector := NewSub2API(server.Client(), nil)
	_, err := connector.Collect(context.Background(), domain.Supplier{
		ID:       5,
		BaseURL:  server.URL,
		Adapter:  "sub2api",
		Metadata: map[string]string{"sub2api_mode": "upstream"},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestSub2CatalogHashIgnoresTelemetryButTracksPriceAndModels(t *testing.T) {
	groups := []domain.GroupOffer{{
		ExternalID: "1", Name: "economy", Ratio: "0.05", ModelIDs: []string{"gpt-5.6-sol"},
		Metadata: map[string]string{
			"platform": "openai", "today_account_cost": "1.25", "current_concurrency": "2",
			"account_count": "3", "effective_rate_multiplier": "0.05",
		},
	}}
	models := []domain.ModelOffer{{
		ExternalID: "gpt-5.6-sol", Name: "gpt-5.6-sol", Group: "economy", BillingMode: "unknown",
		PriceFunction: domain.PriceFunction{Currency: "USD", Basis: "sub2api_group", GroupMultiplier: "0.05"},
	}}
	baseline := sub2CatalogHash(groups, models)

	groups[0].Metadata["today_account_cost"] = "99"
	groups[0].Metadata["current_concurrency"] = "9"
	groups[0].Metadata["account_count"] = "20"
	if got := sub2CatalogHash(groups, models); got != baseline {
		t.Fatalf("telemetry changed catalog hash: %s != %s", got, baseline)
	}

	groups[0].Ratio = "0.08"
	if got := sub2CatalogHash(groups, models); got == baseline {
		t.Fatal("price change did not change catalog hash")
	}
	groups[0].Ratio = "0.05"
	groups[0].ModelIDs = append(groups[0].ModelIDs, "gpt-image-2")
	if got := sub2CatalogHash(groups, models); got == baseline {
		t.Fatal("model membership change did not change catalog hash")
	}
}

func newSub2TestCredential(t *testing.T, siteURL string, payload domain.CredentialPayload) (*credentials.Service, int64) {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatalf("open test vault: %v", err)
	}
	service := credentials.NewService(database, vault)
	credential := domain.Credential{Name: "Sub2API test", Kind: "api-key", SiteURL: siteURL}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatalf("create test credential: %v", err)
	}
	return service, credential.ID
}

func findSub2Group(t *testing.T, groups []domain.GroupOffer, externalID string) domain.GroupOffer {
	t.Helper()
	for _, group := range groups {
		if group.ExternalID == externalID {
			return group
		}
	}
	t.Fatalf("group %q not found in %#v", externalID, groups)
	return domain.GroupOffer{}
}

func findSub2Model(t *testing.T, models []domain.ModelOffer, externalID, groupName string) domain.ModelOffer {
	t.Helper()
	for _, model := range models {
		if model.ExternalID == externalID && model.Group == groupName {
			return model
		}
	}
	t.Fatalf("model %q/%q not found in %#v", externalID, groupName, models)
	return domain.ModelOffer{}
}

func warningsContain(warnings []string, substring string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, substring) {
			return true
		}
	}
	return false
}
