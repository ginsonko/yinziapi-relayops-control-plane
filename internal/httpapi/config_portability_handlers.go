package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func (s *Server) registerConfigPortabilityRoutes(router chi.Router) {
	router.Get("/config/export", s.handleExportConfig)
	router.Post("/config/imports/preview", s.handlePreviewConfigImport)
	router.Post("/config/imports/apply", s.handleApplyConfigImport)
	router.Get("/config/imports/{id}", s.handleGetConfigImport)
	router.Post("/config/imports/{id}/rollback", s.handleRollbackConfigImport)
}

func (s *Server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	bundle, hash, err := s.store.ExportPortableConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="relayops-config.json"`)
	writeJSON(w, http.StatusOK, map[string]any{"config_hash": hash, "bundle": bundle})
}

func (s *Server) handlePreviewConfigImport(w http.ResponseWriter, r *http.Request) {
	var input domain.ConfigImportPreviewRequest
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := store.ValidatePortableConfig(input.Bundle); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, item := range input.Bundle.ConnectorSpecs {
		parsed, err := connectors.ParseJSONConnectorSpec(item.Spec)
		if err != nil {
			writeError(w, http.StatusBadRequest, "connector spec "+strconv.Quote(item.Name)+": "+err.Error())
			return
		}
		if strconv.Itoa(parsed.SchemaVersion) != item.SchemaVersion {
			writeError(w, http.StatusBadRequest, "connector spec "+strconv.Quote(item.Name)+" schema_version does not match its document")
			return
		}
	}
	preview, err := s.store.PreviewPortableConfig(r.Context(), input)
	if err != nil {
		writeConfigPortabilityError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, preview)
}

func (s *Server) handleApplyConfigImport(w http.ResponseWriter, r *http.Request) {
	var input domain.ConfigImportApplyRequest
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := s.store.ApplyPortableConfig(r.Context(), input.PreviewID, input.PreviewHash)
	if err != nil {
		writeConfigPortabilityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleGetConfigImport(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var run domain.ConfigImportRun
		run, err = s.store.GetConfigImportRun(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, run)
			return
		}
	}
	writeConfigPortabilityError(w, err)
}

func (s *Server) handleRollbackConfigImport(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var run domain.ConfigImportRun
		run, err = s.store.RollbackPortableConfig(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, run)
			return
		}
	}
	writeConfigPortabilityError(w, err)
}

func writeConfigPortabilityError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrConfigCASConflict) {
		status = http.StatusConflict
	} else if store.IsNotFound(err) {
		status = http.StatusNotFound
	}
	writeError(w, status, err.Error())
}
