package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestConfigPortabilityHTTPPreviewApplyReadAndRollback(t *testing.T) {
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})

	exportResponse := portabilityRequest(t, handler, http.MethodGet, "/api/v1/config/export", nil)
	if exportResponse.Code != http.StatusOK || exportResponse.Header().Get("Content-Disposition") == "" {
		t.Fatalf("export = %d %s", exportResponse.Code, exportResponse.Body.String())
	}

	bundle := domain.PortableConfig{SchemaVersion: domain.PortableConfigSchemaVersion,
		Suppliers: []domain.PortableSupplier{{Name: "Public upstream", BaseURL: "https://public.example", Adapter: "newapi", Enabled: false, PollIntervalSeconds: 300}},
	}
	previewBody, _ := json.Marshal(domain.ConfigImportPreviewRequest{Mode: domain.ConfigImportAdd, Bundle: bundle})
	previewResponse := portabilityRequest(t, handler, http.MethodPost, "/api/v1/config/imports/preview", previewBody)
	if previewResponse.Code != http.StatusCreated {
		t.Fatalf("preview = %d %s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewEnvelope struct {
		Data domain.ConfigImportPreview `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if previewEnvelope.Data.ID == 0 || previewEnvelope.Data.PreviewHash == "" || previewEnvelope.Data.Summary.Create != 1 {
		t.Fatalf("preview payload = %#v", previewEnvelope.Data)
	}

	applyBody, _ := json.Marshal(domain.ConfigImportApplyRequest{PreviewID: previewEnvelope.Data.ID, PreviewHash: previewEnvelope.Data.PreviewHash})
	applyResponse := portabilityRequest(t, handler, http.MethodPost, "/api/v1/config/imports/apply", applyBody)
	if applyResponse.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", applyResponse.Code, applyResponse.Body.String())
	}
	var runEnvelope struct {
		Data domain.ConfigImportRun `json:"data"`
	}
	if err := json.Unmarshal(applyResponse.Body.Bytes(), &runEnvelope); err != nil {
		t.Fatal(err)
	}
	if runEnvelope.Data.ID == 0 || runEnvelope.Data.Status != "applied" {
		t.Fatalf("run = %#v", runEnvelope.Data)
	}

	getResponse := portabilityRequest(t, handler, http.MethodGet, "/api/v1/config/imports/"+strconv.FormatInt(runEnvelope.Data.ID, 10), nil)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get run = %d %s", getResponse.Code, getResponse.Body.String())
	}
	rollbackResponse := portabilityRequest(t, handler, http.MethodPost, "/api/v1/config/imports/"+strconv.FormatInt(runEnvelope.Data.ID, 10)+"/rollback", []byte(`{}`))
	if rollbackResponse.Code != http.StatusOK {
		t.Fatalf("rollback = %d %s", rollbackResponse.Code, rollbackResponse.Body.String())
	}
	if suppliers, _ := st.ListSuppliers(context.Background()); len(suppliers) != 0 {
		t.Fatalf("rollback left suppliers: %#v", suppliers)
	}
}

func TestConfigPortabilityHTTPRejectsUnknownSchema(t *testing.T) {
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	body := []byte(`{"mode":"add","bundle":{"schema_version":"future/v99","credential_refs":[],"suppliers":[],"targets":[],"pricing_policies":[],"connector_specs":[],"settings":[]}}`)
	response := portabilityRequest(t, handler, http.MethodPost, "/api/v1/config/imports/preview", body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown schema = %d %s", response.Code, response.Body.String())
	}
}

func portabilityRequest(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
