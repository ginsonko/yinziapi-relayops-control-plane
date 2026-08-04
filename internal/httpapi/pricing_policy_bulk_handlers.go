package httpapi

import (
	"errors"
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func (s *Server) handlePreviewPolicyBulkMutation(w http.ResponseWriter, r *http.Request) {
	var input domain.PricingPolicyBulkMutation
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := s.store.PreviewPolicyBulkMutation(r.Context(), input)
	if err != nil {
		writePolicyBulkError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyPolicyBulkMutation(w http.ResponseWriter, r *http.Request) {
	var input domain.PricingPolicyBulkMutation
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.store.ApplyPolicyBulkMutation(r.Context(), input)
	if err != nil {
		writePolicyBulkError(w, err, &result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writePolicyBulkError(w http.ResponseWriter, err error, result *domain.PricingPolicyBulkResult) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrPolicyBulkInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, store.ErrPolicyBulkStale), errors.Is(err, store.ErrPolicyBulkBlocked):
		status = http.StatusConflict
	}
	if result == nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, status, map[string]any{"message": err.Error(), "result": result})
}
