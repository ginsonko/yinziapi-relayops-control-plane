package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestFixturePasswordSessionExposesCustomerReadContracts(t *testing.T) {
	site := newFixture()
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/user/login",
		strings.NewReader(`{"username":"relayops-root","password":"relayops-local-only"}`))
	site.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}

	for _, path := range []string{"/api/user/self", "/api/data/self"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("New-Api-User", "17")
		request.AddCookie(login.Result().Cookies()[0])
		site.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":true`) {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}

func TestFixtureLedgerEventsAreStableWithinRunAndUniqueAcrossRestarts(t *testing.T) {
	first := newFixture()
	second := newFixture()
	second.startedAt = first.startedAt
	if first.runID == second.runID {
		t.Fatal("independent fixture runs reused the same run id")
	}

	for _, test := range []struct {
		path string
		key  string
	}{
		{path: "/api/log/", key: "request_id"},
		{path: "/api/user/topup", key: "trade_no"},
	} {
		withinRunA := readFixtureItems(t, first, test.path)
		withinRunB := readFixtureItems(t, first, test.path)
		otherRun := readFixtureItems(t, second, test.path)
		if !reflect.DeepEqual(withinRunA, withinRunB) {
			t.Fatalf("%s changed within one fixture run", test.path)
		}
		if withinRunA[0][test.key] == otherRun[0][test.key] {
			t.Fatalf("%s reused %s across fixture runs: %v", test.path, test.key, withinRunA[0][test.key])
		}
	}
}

func TestFixtureListingStagePublishesGroupScopedBillingContracts(t *testing.T) {
	site := newFixture()

	baseline := readFixturePricing(t, site)
	if baseline.GroupRatios["r27-source"] != nil {
		t.Fatalf("baseline exposed the staged group: %#v", baseline.GroupRatios)
	}
	if len(baseline.Models) != 2 {
		t.Fatalf("baseline models = %#v", baseline.Models)
	}

	unauthorized := httptest.NewRecorder()
	site.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/__fixture__/stage/listing", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized stage status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	stage := httptest.NewRecorder()
	stageRequest := httptest.NewRequest(http.MethodPost, "/__fixture__/stage/listing", nil)
	stageRequest.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(stage, stageRequest)
	if stage.Code != http.StatusOK {
		t.Fatalf("stage status=%d body=%s", stage.Code, stage.Body.String())
	}

	listed := readFixturePricing(t, site)
	if value, ok := listed.GroupRatios["r27-source"].(string); !ok || value != "0.4" {
		t.Fatalf("listed group ratio = %#v", listed.GroupRatios["r27-source"])
	}
	if len(listed.Models) != 6 {
		t.Fatalf("listed model count=%d models=%#v", len(listed.Models), listed.Models)
	}

	wantUnits := map[string]string{
		"fixture-token-r27":   "per_million_tokens",
		"fixture-request-r27": "per_request",
		"fixture-second-r27":  "per_second",
		"fixture-fixed-r27":   "fixed_duration",
	}
	for _, model := range listed.Models {
		wantUnit, wanted := wantUnits[model.ModelName]
		if !wanted {
			continue
		}
		groupPrice, ok := model.GroupPricing["r27-source"]
		if !ok {
			t.Fatalf("%s has no r27-source contract: %#v", model.ModelName, model.GroupPricing)
		}
		if groupPrice.BillingUnit != wantUnit || groupPrice.ContractRevision != "fixture-r27-source-v1" {
			t.Fatalf("%s contract = %#v", model.ModelName, groupPrice)
		}
		delete(wantUnits, model.ModelName)
	}
	if len(wantUnits) != 0 {
		t.Fatalf("missing staged models: %#v", wantUnits)
	}
}

func TestFixtureUpstreamProbeRequiresKeyAndPublishedModel(t *testing.T) {
	site := newFixture()

	unauthorized := httptest.NewRecorder()
	site.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized models status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	models := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"fixture-fixed-r27"`) {
		t.Fatalf("models status=%d body=%s", models.Code, models.Body.String())
	}

	valid := httptest.NewRecorder()
	validRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fixture-second-r27","messages":[{"role":"user","content":"probe"}]}`))
	validRequest.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(valid, validRequest)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"total_tokens":2`) {
		t.Fatalf("valid probe status=%d body=%s", valid.Code, valid.Body.String())
	}

	unknown := httptest.NewRecorder()
	unknownRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"not-published","messages":[{"role":"user","content":"probe"}]}`))
	unknownRequest.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(unknown, unknownRequest)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown probe status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

type fixturePricingResponse struct {
	GroupRatios map[string]any        `json:"group_ratio"`
	Models      []fixturePricingModel `json:"data"`
}

type fixturePricingModel struct {
	ModelName    string                         `json:"model_name"`
	GroupPricing map[string]fixtureGroupPricing `json:"group_pricing"`
}

type fixtureGroupPricing struct {
	ContractRevision string `json:"contract_revision"`
	BillingUnit      string `json:"billing_unit"`
}

func readFixturePricing(t *testing.T, site *fixture) fixturePricingResponse {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/pricing", nil)
	request.Header.Set("New-Api-User", "17")
	request.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("pricing status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope fixturePricingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode pricing: %v", err)
	}
	return envelope
}

func readFixtureItems(t *testing.T, site *fixture, path string) []map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("New-Api-User", "17")
	request.Header.Set("Authorization", "Bearer "+fixtureToken)
	site.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(envelope.Data.Items) == 0 {
		t.Fatalf("%s returned no fixture items", path)
	}
	return envelope.Data.Items
}
