package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (s *Server) handleListConnectorManifests(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListConnectorManifests(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleImportConnectorManifest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "connector manifest could not be read")
		return
	}
	manifest, canonical, err := connectors.ParseConnectorManifest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Round-trip the canonical document before storage. This keeps exports
	// byte-stable while Store remains independent from connector validation.
	if err := json.Unmarshal(canonical, &manifest); err != nil {
		writeError(w, http.StatusInternalServerError, "connector manifest canonicalization failed")
		return
	}
	record := domain.ConnectorManifestRecord{
		Name: manifest.Name, SchemaVersion: manifest.SchemaVersion, AdapterFamily: manifest.AdapterFamily,
		Manifest: manifest, Source: "import", ValidationStatus: "valid",
	}
	if err := s.store.CreateConnectorManifest(r.Context(), &record); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (s *Server) handleExportConnectorManifest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.store.GetConnectorManifest(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="connector-manifest-`+strconv.FormatInt(id, 10)+`.json"`)
	writeJSON(w, http.StatusOK, record.Manifest)
}

type bindConnectorManifestInput struct {
	ManifestID *int64 `json:"manifest_id"`
}

func (s *Server) handleGetSupplierConnectorManifest(w http.ResponseWriter, r *http.Request) {
	supplierID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	binding, err := s.store.GetSupplierManifestBinding(r.Context(), supplierID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

func (s *Server) handleBindSupplierConnectorManifest(w http.ResponseWriter, r *http.Request) {
	supplierID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input bindConnectorManifestInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "manifest binding is not valid JSON")
		return
	}
	if input.ManifestID == nil || *input.ManifestID == 0 {
		if err := s.store.UnbindSupplierConnectorManifest(r.Context(), supplierID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"supplier_id": supplierID, "manifest_id": nil})
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), supplierID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	record, err := s.store.GetConnectorManifest(r.Context(), *input.ManifestID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	if record.ValidationStatus != "valid" {
		writeError(w, http.StatusUnprocessableEntity, "connector manifest is not valid")
		return
	}
	if !connectors.ConnectorManifestMatchesOrigin(record.Manifest, supplier.BaseURL) {
		writeError(w, http.StatusUnprocessableEntity, "connector manifest does not allow this supplier origin")
		return
	}
	adapter := strings.ToLower(strings.TrimSpace(supplier.Adapter))
	if adapter != "" && adapter != "auto" && !manifestAdapterMatches(adapter, record.AdapterFamily) {
		writeError(w, http.StatusUnprocessableEntity, "connector manifest adapter does not match the supplier adapter")
		return
	}
	boundOrigin := normalizedBindingOrigin(supplier.BaseURL)
	if err := s.store.BindSupplierConnectorManifest(r.Context(), supplierID, record.ID, boundOrigin, time.Now().UTC()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	binding, err := s.store.GetSupplierManifestBinding(r.Context(), supplierID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, binding)
}

func manifestAdapterMatches(adapter, family string) bool {
	switch strings.ToLower(adapter) {
	case "newapi", "newapi-native":
		return family == "newapi"
	case "sub2api", "sub2api_admin", "sub2api_upstream":
		return family == "sub2api"
	default:
		return false
	}
}

func normalizedBindingOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}
