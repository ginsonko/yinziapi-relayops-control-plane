package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestManualWatchIDAcceptsSinglePathEncoding(t *testing.T) {
	id, err := manualWatchID("manual%3A42")
	if err != nil || id != 42 {
		t.Fatalf("encoded manual id=%d err=%v", id, err)
	}
	if _, err := manualWatchID("manual%253A42"); err == nil {
		t.Fatal("double-encoded manual id must remain invalid")
	}
	if _, err := manualWatchID("auto%3A42"); err == nil {
		t.Fatal("automatic id must not be accepted as manual")
	}
}

func TestWatchlistEndpointsManageItemsWithoutCollection(t *testing.T) {
	st := openHTTPTestStore(t)
	supplier := domain.Supplier{Name: "watch upstream", BaseURL: "https://watch.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "watch target", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "economy mapping", SupplierID: supplier.ID, SourceGroup: "economy",
		TargetSiteID: target.ID, TargetGroup: "economy", RuleType: "add", Value: "0.01", Enabled: true}
	if err := st.CreatePolicy(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: "economy", Name: "Economy", Ratio: "0.1"}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "watch-catalog"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/watchlist", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	createBody := fmt.Sprintf(`{"supplier_id":%d,"scope_type":"group","group_key":"economy","model_key":"","display_name":"主力分组","note":"长期关注"}`, supplier.ID)
	created := authenticatedRequest(handler, http.MethodPost, "/api/v1/watchlist/items", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var createdEnvelope struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdEnvelope); err != nil || createdEnvelope.Data.ID == 0 {
		t.Fatalf("create envelope=%s err=%v", created.Body.String(), err)
	}

	listed := authenticatedRequest(handler, http.MethodGet, "/api/v1/watchlist", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"displayName":"主力分组"`) || !strings.Contains(listed.Body.String(), `"manual":true`) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}

	updateBody := fmt.Sprintf(`{"supplier_id":%d,"scope_type":"group","group_key":"economy","model_key":"","display_name":"主力分组 A","note":"已修改"}`, supplier.ID)
	updated := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/watchlist/items/manual%%3A%d", createdEnvelope.Data.ID), updateBody)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}

	settings := authenticatedRequest(handler, http.MethodPut, "/api/v1/watchlist/settings", `{"auto_enabled":false,"retention_days":14,"automatic_limit":45,"overview_limit":12}`)
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `"overview_limit":12`) || !strings.Contains(settings.Body.String(), `"auto_enabled":false`) {
		t.Fatalf("settings status=%d body=%s", settings.Code, settings.Body.String())
	}
	listed = authenticatedRequest(handler, http.MethodGet, "/api/v1/watchlist", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"manual":true`) || !strings.Contains(listed.Body.String(), `"sourcePricing":true`) {
		t.Fatalf("manual item lost rule source when automatic display was disabled: %s", listed.Body.String())
	}

	deleted := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/watchlist/items/manual%%3A%d", createdEnvelope.Data.ID), updateBody)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	listed = authenticatedRequest(handler, http.MethodGet, "/api/v1/watchlist", "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), `"displayName":"主力分组 A"`) {
		t.Fatalf("deleted item remained: %s", listed.Body.String())
	}
}

func TestWatchlistAppliesAutomaticLimitBeforeProjection(t *testing.T) {
	st := openHTTPTestStore(t)
	supplier := domain.Supplier{Name: "automatic upstream", BaseURL: "https://automatic.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "automatic target", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: "https://target.example", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	groups := []domain.GroupOffer{
		{ExternalID: "alpha", Name: "Alpha", Ratio: "0.1"},
		{ExternalID: "beta", Name: "Beta", Ratio: "0.2"},
		{ExternalID: "gamma", Name: "Gamma", Ratio: "0.3"},
	}
	for index, group := range groups {
		policy := domain.PricingPolicy{Name: "mapping " + group.ExternalID, SupplierID: supplier.ID,
			SourceGroup: group.ExternalID, TargetSiteID: target.ID, TargetGroup: group.ExternalID,
			RuleType: "add", Value: fmt.Sprintf("0.0%d", index+1), Enabled: true}
		if err := st.CreatePolicy(context.Background(), &policy); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{SupplierID: supplier.ID, Groups: groups,
		Evidence: domain.Evidence{ObservedAt: time.Now().UTC(), ResponseHash: "automatic-catalog"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveWatchSettings(context.Background(), store.WatchSettings{
		AutoEnabled: true, RetentionDays: 7, AutomaticLimit: 1, OverviewLimit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	listed := authenticatedRequest(handler, http.MethodGet, "/api/v1/watchlist", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var envelope struct {
		Data struct {
			Items               []consoleWatch `json:"items"`
			AutomaticCandidates int            `json:"automatic_candidates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Items) != 1 || envelope.Data.AutomaticCandidates != 3 {
		t.Fatalf("items=%d candidates=%d body=%s", len(envelope.Data.Items), envelope.Data.AutomaticCandidates, listed.Body.String())
	}
}
