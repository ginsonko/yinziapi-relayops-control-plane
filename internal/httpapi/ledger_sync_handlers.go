package httpapi

import (
	"errors"
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ledgersync"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func (s *Server) handleTargetLedgerSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.ledgerSync == nil {
		writeError(w, http.StatusServiceUnavailable, "target ledger sync service is not configured")
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, err := s.ledgerSync.Status(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleTargetLedgerSyncConfig(w http.ResponseWriter, r *http.Request) {
	if s.ledgerSync == nil {
		writeError(w, http.StatusServiceUnavailable, "target ledger sync service is not configured")
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input store.TargetLedgerSyncConfig
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.TargetSiteID = id
	config, err := s.ledgerSync.UpdateConfig(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleRunTargetLedgerSync(w http.ResponseWriter, r *http.Request) {
	if s.ledgerSync == nil {
		writeError(w, http.StatusServiceUnavailable, "target ledger sync service is not configured")
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.ledgerSync.SyncTarget(r.Context(), id, true)
	if errors.Is(err, ledgersync.ErrSyncBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
