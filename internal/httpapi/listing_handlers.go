package httpapi

import (
	"net/http"
	"strconv"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
)

func (s *Server) handlePreviewListingPlan(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	var input domain.ListingPlanRequest
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := s.listing.Preview(r.Context(), input)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}

func (s *Server) handleListListingPlans(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	items, err := s.listing.ListPlans(r.Context(), r.URL.Query().Get("status"), queryLimit(r, 100))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetListingPlan(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	id, err := parseID(r, "id")
	if err == nil {
		var plan domain.ListingPlan
		plan, err = s.listing.GetPlan(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, plan)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleApplyListingPlan(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input listing.ApplyInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	change, err := s.listing.Apply(r.Context(), id, input)
	if err != nil {
		writeJSON(w, statusCode(err), map[string]any{"change_set": change, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, change)
}

func (s *Server) handleListListingChangeSets(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	var planID int64
	if raw := r.URL.Query().Get("plan_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "invalid plan_id")
			return
		}
		planID = parsed
	}
	items, err := s.listing.ListChangeSets(r.Context(), planID, queryLimit(r, 100))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetListingChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	id, err := parseID(r, "id")
	if err == nil {
		var change domain.ListingChangeSet
		change, err = s.listing.GetChangeSet(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, change)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleRollbackListingChangeSet(w http.ResponseWriter, r *http.Request) {
	if s.listing == nil {
		writeError(w, http.StatusServiceUnavailable, "listing service is unavailable")
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input listing.RollbackInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	change, err := s.listing.Rollback(r.Context(), id, input)
	if err != nil {
		writeJSON(w, statusCode(err), map[string]any{"change_set": change, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, change)
}

func queryLimit(r *http.Request, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
