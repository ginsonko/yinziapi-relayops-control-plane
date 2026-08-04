package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSupplierManagementRequiresPreviewAndExplicitDeleteConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	supplier := domain.Supplier{Name: "managed supplier", BaseURL: "https://managed.example", Adapter: "newapi", Enabled: true,
		PollIntervalSeconds: 300, RechargeValueFactor: "1", FocusGroups: []string{"economy"}, Metadata: map[string]string{"owner": "test"}}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	invalidUpdate := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID),
		`{"name":"","base_url":"https://managed.example","adapter":"newapi","enabled":true,"poll_interval_seconds":300,"recharge_value_factor":"1","focus_groups":[],"focus_models":[],"metadata":{}}`)
	if invalidUpdate.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status=%d body=%s", invalidUpdate.Code, invalidUpdate.Body.String())
	}
	update := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID),
		`{"name":"renamed supplier","base_url":"https://managed.example","adapter":"sub2api","pricing_path":"/pricing","balance_path":"/balance","enabled":false,"poll_interval_seconds":120,"recharge_value_factor":"0.4","focus_groups":["economy"],"focus_models":[],"metadata":{"owner":"test","api_key_management_path":"/keys"}}`)
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
	stored, err := st.GetSupplier(ctx, supplier.ID)
	if err != nil || stored.Name != "renamed supplier" || stored.Adapter != "sub2api" || stored.Enabled || stored.PollIntervalSeconds != 120 ||
		stored.RechargeValueFactor != "0.4" || stored.PricingPath != "/pricing" || stored.Metadata["owner"] != "test" ||
		stored.Metadata["api_key_management_path"] != "/keys" {
		t.Fatalf("updated supplier=%#v err=%v", stored, err)
	}
	unsafePath := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID),
		`{"name":"renamed supplier","base_url":"https://managed.example","adapter":"sub2api","enabled":false,"poll_interval_seconds":120,"recharge_value_factor":"0.4","focus_groups":[],"focus_models":[],"metadata":{"api_key_management_path":"//evil.example/keys"}}`)
	if unsafePath.Code != http.StatusBadRequest {
		t.Fatalf("unsafe key-management path status=%d body=%s", unsafePath.Code, unsafePath.Body.String())
	}

	unsafeDelete := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID), "")
	if unsafeDelete.Code != http.StatusBadRequest {
		t.Fatalf("empty delete status=%d body=%s", unsafeDelete.Code, unsafeDelete.Body.String())
	}
	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/suppliers/%d/deletion-impact", supplier.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.SupplierDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || preview.Data.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	wrong := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID),
		fmt.Sprintf(`{"confirm_name":"wrong","impact_hash":%q}`, preview.Data.ImpactHash))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	confirmed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/suppliers/%d", supplier.ID),
		fmt.Sprintf(`{"confirm_name":"renamed supplier","impact_hash":%q}`, preview.Data.ImpactHash))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirmed delete status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}

func TestCreateSupplierReturnsFriendlyConflictForExistingBaseURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	supplier := domain.Supplier{Name: "hubway", BaseURL: "https://hubway.cc", Adapter: "sub2api", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	response := authenticatedRequest(handler, http.MethodPost, "/api/v1/suppliers",
		`{"name":"hubway duplicate","base_url":"https://hubway.cc/","adapter":"sub2api","enabled":true,"collect_now":false}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), fmt.Sprintf("#%d hubway", supplier.ID)) ||
		strings.Contains(response.Body.String(), "UNIQUE constraint") {
		t.Fatalf("duplicate supplier status=%d body=%s", response.Code, response.Body.String())
	}
}
