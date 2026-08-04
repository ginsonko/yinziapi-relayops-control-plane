package httpapi

import (
	"errors"
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type credentialRenewalInput struct {
	Enabled  bool                     `json:"enabled"`
	Username *string                  `json:"username,omitempty"`
	Password *string                  `json:"password,omitempty"`
	Rule     credentialLoginRuleInput `json:"rule"`
}

type credentialLoginRuleInput struct {
	URL                        string `json:"url"`
	Method                     string `json:"method"`
	ContentType                string `json:"content_type"`
	UsernameField              string `json:"username_field"`
	PasswordField              string `json:"password_field"`
	TokenJSONPath              string `json:"token_json_path"`
	UserIDJSONPath             string `json:"user_id_json_path"`
	ExpiresInJSONPath          string `json:"expires_in_json_path"`
	ExpiresAtJSONPath          string `json:"expires_at_json_path"`
	SuccessJSONPath            string `json:"success_json_path"`
	SuccessValue               string `json:"success_value"`
	MessageJSONPath            string `json:"message_json_path"`
	LoginRefreshTokenJSONPath  string `json:"login_refresh_token_json_path"`
	RefreshURL                 string `json:"refresh_url"`
	RefreshMethod              string `json:"refresh_method"`
	RefreshTokenField          string `json:"refresh_token_field"`
	RefreshResponseTokenPath   string `json:"refresh_response_token_json_path"`
	RefreshResponseRefreshPath string `json:"refresh_response_refresh_token_json_path"`
	RefreshResponseExpiresPath string `json:"refresh_response_expires_in_json_path"`
	RefreshResponseExpiryPath  string `json:"refresh_response_expires_at_json_path"`
	CaptchaJSONPath            string `json:"captcha_json_path"`
	TwoFactorJSONPath          string `json:"two_factor_json_path"`
	CookieResponse             bool   `json:"cookie_response"`
	MinimumIntervalSeconds     int64  `json:"minimum_interval_seconds"`
	InitialBackoffSeconds      int64  `json:"initial_backoff_seconds"`
	MaximumBackoffSeconds      int64  `json:"maximum_backoff_seconds"`
	MaximumConsecutiveFailures int    `json:"maximum_consecutive_failures"`
	HumanActionCooldownSeconds int64  `json:"human_action_cooldown_seconds"`
	AllowReplaceBrowserPairing bool   `json:"allow_replace_browser_pairing"`
}

func (s *Server) handleCredentialRenewalStatus(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var detail domain.CredentialRenewalDetail
		detail, err = s.refresher.RenewalDetail(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, detail)
			return
		}
	}
	writeCredentialRenewalError(w, err)
}

func (s *Server) handleConfigureCredentialRenewal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeCredentialRenewalError(w, err)
		return
	}
	var input credentialRenewalInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "automatic login configuration is not valid JSON")
		return
	}
	rule := domain.LoginRule{Enabled: input.Enabled, URL: input.Rule.URL, Method: input.Rule.Method,
		ContentType: input.Rule.ContentType, UsernameField: input.Rule.UsernameField, PasswordField: input.Rule.PasswordField,
		TokenJSONPath: input.Rule.TokenJSONPath, UserIDJSONPath: input.Rule.UserIDJSONPath,
		ExpiresInJSONPath: input.Rule.ExpiresInJSONPath, ExpiresAtJSONPath: input.Rule.ExpiresAtJSONPath,
		SuccessJSONPath: input.Rule.SuccessJSONPath, SuccessValue: input.Rule.SuccessValue, MessageJSONPath: input.Rule.MessageJSONPath,
		LoginRefreshTokenJSONPath: input.Rule.LoginRefreshTokenJSONPath, RefreshURL: input.Rule.RefreshURL,
		RefreshMethod: input.Rule.RefreshMethod, RefreshTokenField: input.Rule.RefreshTokenField,
		RefreshResponseTokenPath:   input.Rule.RefreshResponseTokenPath,
		RefreshResponseRefreshPath: input.Rule.RefreshResponseRefreshPath,
		RefreshResponseExpiresPath: input.Rule.RefreshResponseExpiresPath,
		RefreshResponseExpiryPath:  input.Rule.RefreshResponseExpiryPath,
		CaptchaJSONPath:            input.Rule.CaptchaJSONPath, TwoFactorJSONPath: input.Rule.TwoFactorJSONPath,
		CookieResponse: input.Rule.CookieResponse, MinimumIntervalSeconds: input.Rule.MinimumIntervalSeconds,
		InitialBackoffSeconds: input.Rule.InitialBackoffSeconds, MaximumBackoffSeconds: input.Rule.MaximumBackoffSeconds,
		MaximumConsecutiveFailures: input.Rule.MaximumConsecutiveFailures,
		HumanActionCooldownSeconds: input.Rule.HumanActionCooldownSeconds,
		AllowReplaceBrowserPairing: input.Rule.AllowReplaceBrowserPairing}
	if input.Username != nil {
		rule.Username = *input.Username
	}
	if input.Password != nil {
		rule.Password = *input.Password
	}
	detail, err := s.refresher.ConfigureLogin(r.Context(), id, rule, input.Password == nil)
	if err != nil {
		writeCredentialRenewalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleCredentialRenewalPreset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	family, label, rule, supported := credentials.RecommendedLoginRule(supplier.Adapter)
	writeJSON(w, http.StatusOK, domain.CredentialRenewalPreset{
		SupplierID: supplier.ID, Adapter: supplier.Adapter, AdapterFamily: family,
		Label: label, Supported: supported, Rule: rule,
	})
}

func (s *Server) handleRemoveCredentialRenewal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var detail domain.CredentialRenewalDetail
		detail, err = s.refresher.RemoveLogin(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, detail)
			return
		}
	}
	writeCredentialRenewalError(w, err)
}

func (s *Server) handleTriggerCredentialRenewal(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "credential renewal trigger") {
		return
	}
	id, err := parseID(r, "id")
	if err == nil {
		err = s.refresher.Refresh(r.Context(), id)
	}
	if err != nil {
		writeCredentialRenewalError(w, err)
		return
	}
	detail, err := s.refresher.RenewalDetail(r.Context(), id)
	if err != nil {
		writeCredentialRenewalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func writeCredentialRenewalError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if errors.Is(err, remotewrite.ErrDisabled) {
		status = http.StatusServiceUnavailable
	} else if store.IsNotFound(err) {
		status = http.StatusNotFound
	} else if errors.Is(err, store.ErrCredentialChanged) {
		status = http.StatusConflict
	} else {
		switch credentials.RenewalErrorClass(err) {
		case "network_error":
			status = http.StatusBadGateway
		case "captcha_or_2fa", "concurrent_change":
			status = http.StatusConflict
		case "":
			status = http.StatusBadRequest
		}
	}
	message := "credential renewal request failed"
	if err != nil {
		message = err.Error()
	}
	writeError(w, status, message)
}
