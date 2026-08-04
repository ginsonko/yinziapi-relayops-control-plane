package httpapi

import (
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mappingdraft"
)

func (s *Server) handlePreviewMappingDrafts(w http.ResponseWriter, r *http.Request) {
	var request mappingdraft.PreviewRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := mappingdraft.NewService(s.store, s.targets).Preview(r.Context(), request)
	if err != nil {
		writeError(w, mappingDraftStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMaterializeMappingDrafts(w http.ResponseWriter, r *http.Request) {
	var request mappingdraft.MaterializeRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := mappingdraft.NewService(s.store, s.targets).Materialize(r.Context(), request)
	if err != nil {
		writeError(w, mappingDraftStatus(err), err.Error())
		return
	}
	status := http.StatusOK
	for _, item := range result.Policies {
		if item.Created {
			status = http.StatusCreated
			break
		}
	}
	writeJSON(w, status, result)
}

func (s *Server) handlePreviewManualMappingDrafts(w http.ResponseWriter, r *http.Request) {
	var request mappingdraft.ManualPreviewRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := mappingdraft.NewService(s.store, s.targets).ManualPreview(r.Context(), request)
	if err != nil {
		writeError(w, mappingDraftStatus(err), err.Error())
		return
	}
	writeCompressedJSON(w, r, http.StatusOK, result)
}

func (s *Server) handleMaterializeManualMappingDrafts(w http.ResponseWriter, r *http.Request) {
	var request mappingdraft.ManualMaterializeRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := mappingdraft.NewService(s.store, s.targets).ManualMaterialize(r.Context(), request)
	if err != nil {
		writeError(w, mappingDraftStatus(err), err.Error())
		return
	}
	status := http.StatusOK
	for _, item := range result.Policies {
		if item.Created {
			status = http.StatusCreated
			break
		}
	}
	writeJSON(w, status, result)
}

func mappingDraftStatus(err error) int {
	if mappingdraft.IsConflict(err) {
		return http.StatusConflict
	}
	return statusCode(err)
}
