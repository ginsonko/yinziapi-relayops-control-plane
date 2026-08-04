package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const intelligenceConfigKey = intelligence.ConfigSettingKey

func (s *Server) handleIntelligence(w http.ResponseWriter, r *http.Request) {
	config, err := s.intelligenceConfig(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read intelligence config: "+err.Error())
		return
	}
	result, err := s.intelligence.Dashboard(r.Context(), config)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleOverviewIntelligence(w http.ResponseWriter, r *http.Request) {
	config, err := s.intelligenceConfig(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read intelligence config: "+err.Error())
		return
	}
	result, err := s.intelligence.Overview(r.Context(), config)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleIntelligenceConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		config, err := s.intelligenceConfig(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read intelligence config: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, config)
		return
	}
	var config intelligence.Config
	if err := readJSON(r, &config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	config = intelligence.NormalizeConfig(config)
	if err := intelligence.ValidateConfig(config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, reference := range config.LedgerExclusions.ExcludedTokenIDs {
		if looksLikeSecretReference(reference) {
			writeError(w, http.StatusBadRequest, "excluded_token_ids must contain stable identifiers, not API keys or bearer tokens")
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), intelligenceConfigKey, config); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleIntelligenceLedger(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Events []store.OperatingLedgerEvent `json:"events"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Events) == 0 || len(input.Events) > 10_000 {
		writeError(w, http.StatusBadRequest, "events must contain between 1 and 10000 records")
		return
	}
	for index := range input.Events {
		if looksLikeSecretReference(input.Events[index].SubjectTokenID) {
			writeError(w, http.StatusBadRequest, "subject_token_id must be a stable identifier, not an API key or bearer token")
			return
		}
		for key, value := range input.Events[index].Metadata {
			lower := strings.ToLower(strings.TrimSpace(key))
			if strings.Contains(lower, "prompt") || strings.Contains(lower, "token") || strings.Contains(lower, "cookie") ||
				strings.Contains(lower, "authorization") || strings.Contains(lower, "password") {
				writeError(w, http.StatusBadRequest, "ledger metadata cannot contain prompts or credentials")
				return
			}
			if len(key) > 120 || len(value) > 1000 {
				writeError(w, http.StatusBadRequest, "ledger metadata exceeds the safety limit")
				return
			}
		}
	}
	if err := s.store.UpsertOperatingLedgerEvents(r.Context(), input.Events); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	summary, err := s.store.OperatingLedgerSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func looksLikeSecretReference(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "bearer ") ||
		strings.HasPrefix(lower, "session-") || strings.HasPrefix(lower, "sess-") || strings.Count(value, ".") == 2 && strings.HasPrefix(value, "eyJ")
}

func (s *Server) intelligenceConfig(r *http.Request) (intelligence.Config, error) {
	config := intelligence.DefaultConfig()
	var stored intelligence.Config
	if err := s.store.GetSetting(r.Context(), intelligenceConfigKey, &stored); err == nil {
		config = intelligence.NormalizeConfig(stored)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return intelligence.Config{}, err
	}
	return config, nil
}
