package httpapi

import "net/http"

func (s *Server) handlePricingPolicyDeletionImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	impact, err := s.store.PreviewPricingPolicyDeletion(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (s *Server) handleDeletePricingPolicy(w http.ResponseWriter, r *http.Request) {
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
		_, err = s.store.DeletePricingPolicySafely(r.Context(), id, input.ConfirmName, input.ImpactHash)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
