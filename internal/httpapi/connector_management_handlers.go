package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type updateConnectorSpecInput struct {
	Name              string    `json:"name"`
	SchemaVersion     string    `json:"schema_version"`
	SpecJSON          string    `json:"spec_json"`
	Enabled           bool      `json:"enabled"`
	ExpectedUpdatedAt time.Time `json:"expected_updated_at"`
}

func (s *Server) handleUpdateConnectorSpec(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input updateConnectorSpecInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := connectors.ParseJSONConnectorSpec([]byte(input.SpecJSON))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = parsed.Name
	}
	parsedVersion := strconv.Itoa(parsed.SchemaVersion)
	if strings.TrimSpace(input.SchemaVersion) != "" && strings.TrimSpace(input.SchemaVersion) != parsedVersion {
		writeError(w, http.StatusBadRequest, "Connector schema_version 与规则内容不一致")
		return
	}
	item := domain.ConnectorSpec{ID: id, Name: name, SchemaVersion: parsedVersion, SpecJSON: input.SpecJSON, Enabled: input.Enabled}
	if err := s.store.UpdateConnectorSpec(r.Context(), &item, input.ExpectedUpdatedAt); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	updated, err := s.store.GetConnectorSpec(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleConnectorSpecDeletionImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	impact, err := s.store.PreviewConnectorSpecDeletion(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (s *Server) handleDeleteConnectorSpec(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input struct {
		ConfirmName string `json:"confirm_name"`
		ImpactHash  string `json:"impact_hash"`
	}
	if err = readJSON(r, &input); err == nil {
		_, err = s.store.DeleteConnectorSpecSafely(r.Context(), id, input.ConfirmName, input.ImpactHash)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

type updateConnectorManifestInput struct {
	Manifest          json.RawMessage `json:"manifest"`
	ExpectedUpdatedAt time.Time       `json:"expected_updated_at"`
}

func (s *Server) handleUpdateConnectorManifest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input updateConnectorManifestInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	manifest, canonical, err := connectors.ParseConnectorManifest(input.Manifest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := json.Unmarshal(canonical, &manifest); err != nil {
		writeError(w, http.StatusInternalServerError, "connector manifest canonicalization failed")
		return
	}
	record := domain.ConnectorManifestRecord{
		ID: id, Name: manifest.Name, SchemaVersion: manifest.SchemaVersion, AdapterFamily: manifest.AdapterFamily,
		Manifest: manifest, Source: "edited", ValidationStatus: "valid",
	}
	if err := s.store.UpdateConnectorManifestSafely(r.Context(), &record, input.ExpectedUpdatedAt, func(supplier domain.Supplier) error {
		if !connectors.ConnectorManifestMatchesOrigin(manifest, supplier.BaseURL) {
			return fmt.Errorf("来源站点不在 origins 允许范围内")
		}
		adapter := strings.ToLower(strings.TrimSpace(supplier.Adapter))
		if adapter != "" && adapter != "auto" && !manifestAdapterMatches(adapter, manifest.AdapterFamily) {
			return fmt.Errorf("adapter_family 与上游适配器不一致")
		}
		return nil
	}); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	updated, err := s.store.GetConnectorManifest(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleConnectorManifestDeletionImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	impact, err := s.store.PreviewConnectorManifestDeletion(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (s *Server) handleDeleteConnectorManifest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input struct {
		ConfirmName string `json:"confirm_name"`
		ImpactHash  string `json:"impact_hash"`
	}
	if err = readJSON(r, &input); err == nil {
		_, err = s.store.DeleteConnectorManifestSafely(r.Context(), id, input.ConfirmName, input.ImpactHash)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
