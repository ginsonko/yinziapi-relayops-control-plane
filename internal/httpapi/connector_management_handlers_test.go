package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const httpValidConnectorSpec = `{"schema_version":1,"name":"http-connector","endpoints":[{"name":"catalog","method":"GET","path":"/api/catalog","groups":{"items_path":"data.groups","external_id":{"path":"id","required":true},"name":{"path":"name","required":true}}}]}`

func TestConnectorSpecHTTPManagementValidatesCASAndDeletionImpact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	spec := domain.ConnectorSpec{Name: "http spec", SchemaVersion: "1", SpecJSON: httpValidConnectorSpec, Enabled: true}
	if err := st.CreateConnectorSpec(ctx, &spec); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	updateBody := fmt.Sprintf(`{"name":"http spec edited","schema_version":"1","spec_json":%q,"enabled":false,"expected_updated_at":%q}`,
		httpValidConnectorSpec, spec.UpdatedAt.Format(time.RFC3339Nano))
	update := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-specs/%d", spec.ID), updateBody)
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
	stale := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-specs/%d", spec.ID), updateBody)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	invalid := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-specs/%d", spec.ID),
		fmt.Sprintf(`{"name":"invalid","schema_version":"1","spec_json":%q,"enabled":true,"expected_updated_at":%q}`, `{"schema_version":1}`, spec.UpdatedAt))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/connector-specs/%d/deletion-impact", spec.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.ConnectorSpecDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || preview.Data.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview.Data, err)
	}
	wrong := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/connector-specs/%d", spec.ID),
		fmt.Sprintf(`{"confirm_name":"wrong","impact_hash":%q}`, preview.Data.ImpactHash))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	confirmed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/connector-specs/%d", spec.ID),
		fmt.Sprintf(`{"confirm_name":"http spec edited","impact_hash":%q}`, preview.Data.ImpactHash))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}

func TestConnectorManifestHTTPManagementValidatesDocumentAndReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	record := domain.ConnectorManifestRecord{
		Name: "http-manifest", SchemaVersion: domain.ConnectorManifestSchemaV1, AdapterFamily: "newapi", Source: "test", ValidationStatus: "valid",
		Manifest: domain.ConnectorManifest{SchemaVersion: domain.ConnectorManifestSchemaV1, Name: "http-manifest", AdapterFamily: "newapi",
			Origins:      domain.ConnectorManifestOrigins{Schemes: []string{"https"}, Hosts: []string{"api.example.com"}},
			Capabilities: map[string]domain.ConnectorManifestCapability{"status": {Paths: []string{"/api/status"}, Contract: "native"}}},
	}
	if err := st.CreateConnectorManifest(ctx, &record); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "bound", BaseURL: "https://api.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	if err := st.BindSupplierConnectorManifest(ctx, supplier.ID, record.ID, supplier.BaseURL, record.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	manifest := `{"schema_version":"relayops.connector-manifest/v1","name":"http-manifest-edited","adapter_family":"newapi","origins":{"schemes":["https"],"hosts":["api.example.com"]},"capabilities":{"status":{"paths":["/api/status"],"contract":"native"}}}`
	update := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-manifests/%d", record.ID),
		fmt.Sprintf(`{"manifest":%s,"expected_updated_at":%q}`, manifest, record.UpdatedAt.Format(time.RFC3339Nano)))
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
	var updated struct {
		Data domain.ConnectorManifestRecord `json:"data"`
	}
	if err := json.Unmarshal(update.Body.Bytes(), &updated); err != nil || updated.Data.UpdatedAt.IsZero() {
		t.Fatalf("updated manifest=%#v err=%v", updated.Data, err)
	}
	incompatible := `{"schema_version":"relayops.connector-manifest/v1","name":"http-manifest-incompatible","adapter_family":"newapi","origins":{"schemes":["https"],"hosts":["other.example.com"]},"capabilities":{"status":{"paths":["/api/status"],"contract":"native"}}}`
	incompatibleUpdate := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-manifests/%d", record.ID),
		fmt.Sprintf(`{"manifest":%s,"expected_updated_at":%q}`, incompatible, updated.Data.UpdatedAt.Format(time.RFC3339Nano)))
	if incompatibleUpdate.Code != http.StatusConflict {
		t.Fatalf("incompatible bound update status=%d body=%s", incompatibleUpdate.Code, incompatibleUpdate.Body.String())
	}
	preserved, err := st.GetConnectorManifest(ctx, record.ID)
	if err != nil || preserved.Name != "http-manifest-edited" || preserved.Manifest.Origins.Hosts[0] != "api.example.com" {
		t.Fatalf("manifest changed after rejected update: %#v err=%v", preserved, err)
	}
	stale := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-manifests/%d", record.ID),
		fmt.Sprintf(`{"manifest":%s,"expected_updated_at":%q}`, manifest, record.UpdatedAt.Format(time.RFC3339Nano)))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
	invalid := authenticatedRequest(handler, http.MethodPut, fmt.Sprintf("/api/v1/connector-manifests/%d", record.ID),
		fmt.Sprintf(`{"manifest":{"schema_version":"bad"},"expected_updated_at":%q}`, record.UpdatedAt.Format(time.RFC3339Nano)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/connector-manifests/%d/deletion-impact", record.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.ConnectorManifestDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || len(preview.Data.SupplierRefs) != 1 {
		t.Fatalf("preview=%#v err=%v", preview.Data, err)
	}
	blocked := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/connector-manifests/%d", record.ID),
		fmt.Sprintf(`{"confirm_name":"http-manifest-edited","impact_hash":%q}`, preview.Data.ImpactHash))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("bound delete status=%d body=%s", blocked.Code, blocked.Body.String())
	}
}
