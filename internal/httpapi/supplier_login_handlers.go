package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type supplierPasswordLoginInput struct {
	Username      string                          `json:"username"`
	Password      string                          `json:"password"`
	EnableRenewal *bool                           `json:"enable_renewal,omitempty"`
	Rule          *domain.CredentialLoginRuleView `json:"rule,omitempty"`
}

func (s *Server) handleSupplierPasswordLogin(w http.ResponseWriter, r *http.Request) {
	if s.refresher == nil || s.collector == nil || s.credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "credential login service is not configured")
		return
	}
	if !s.requireRemoteWrites(w, "supplier password login") {
		return
	}
	supplierID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), supplierID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	var input supplierPasswordLoginInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "login request is not valid JSON")
		return
	}
	if strings.TrimSpace(input.Username) == "" || input.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	rule, label, err := s.loginRuleForSupplier(r, supplier, input.Rule)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	enableRenewal := true
	if input.EnableRenewal != nil {
		enableRenewal = *input.EnableRenewal
	}
	payload, expiresAt, err := s.refresher.PasswordLoginCandidate(r.Context(), supplier.BaseURL, input.Username, input.Password, rule, enableRenewal)
	if err != nil {
		writeInitialLoginError(w, err)
		return
	}
	diagnostics, err := s.collector.ValidateCredential(r.Context(), supplier, "password_login", payload)
	if err != nil {
		writeJSON(w, credentialValidationStatus(connectors.ErrorClass(err)), map[string]any{
			"status": "failed", "error_class": connectors.ErrorClass(err),
			"message": pairingValidationMessage(connectors.ErrorClass(err)), "diagnostics": diagnostics,
		})
		return
	}
	credential := domain.Credential{
		Name: supplier.Name + " account login", Kind: "password_login", SiteURL: supplier.BaseURL, ExpiresAt: expiresAt,
	}
	if err := s.credentials.CreateForSupplier(r.Context(), supplier.ID, &credential, payload); err != nil {
		writeError(w, http.StatusConflict, "verified login could not be bound to the supplier")
		return
	}
	go func() {
		ctx, cancel := background(90 * time.Second)
		defer cancel()
		_, _ = s.collector.CollectOrJoin(ctx, supplier.ID)
	}()
	writeJSON(w, http.StatusCreated, map[string]any{
		"credential": credential, "supplier_id": supplier.ID, "preset": label,
		"validation": diagnostics, "renewal_enabled": enableRenewal, "collection_started": true,
	})
}

func credentialValidationStatus(class string) int {
	switch class {
	case "auth_missing", "auth_expired":
		return http.StatusUnauthorized
	case "permission_denied":
		return http.StatusForbidden
	case "contract_changed":
		return http.StatusUnprocessableEntity
	case "rate_limited":
		return http.StatusTooManyRequests
	case "upstream_5xx", "connect_timeout", "read_timeout", "connection_error", "connection_reset", "tls_error", "dns_error":
		return http.StatusBadGateway
	default:
		return http.StatusUnprocessableEntity
	}
}

func (s *Server) loginRuleForSupplier(r *http.Request, supplier domain.Supplier,
	override *domain.CredentialLoginRuleView) (domain.CredentialLoginRuleView, string, error) {
	if override != nil {
		return *override, "request override", nil
	}
	binding, bindingErr := s.store.GetSupplierManifestBinding(r.Context(), supplier.ID)
	if bindingErr != nil && !store.IsNotFound(bindingErr) {
		return domain.CredentialLoginRuleView{}, "", &initialLoginConfigError{"connector manifest binding could not be read"}
	}
	if bindingErr == nil && binding.Manifest != nil && binding.Manifest.Manifest.Login != nil {
		if !connectors.ConnectorManifestMatchesOrigin(binding.Manifest.Manifest, supplier.BaseURL) {
			return domain.CredentialLoginRuleView{}, "", &initialLoginConfigError{"bound connector manifest no longer matches the supplier origin"}
		}
		return *binding.Manifest.Manifest.Login, binding.Manifest.Name, nil
	}
	_, label, rule, supported := credentials.RecommendedLoginRule(supplier.Adapter)
	if !supported || rule == nil {
		return domain.CredentialLoginRuleView{}, "", &initialLoginConfigError{"this adapter has no password-login preset; bind a connector manifest or use browser pairing"}
	}
	return *rule, label, nil
}

type initialLoginConfigError struct{ message string }

func (e *initialLoginConfigError) Error() string { return e.message }

func writeInitialLoginError(w http.ResponseWriter, err error) {
	class := credentials.RenewalErrorClass(err)
	status := http.StatusUnprocessableEntity
	message := "The upstream login could not be completed."
	humanAction := ""
	switch class {
	case "captcha_or_2fa":
		status = http.StatusConflict
		message = "The upstream requires CAPTCHA or two-factor verification. Complete it in the browser, then use the browser helper."
		humanAction = "browser_pairing"
	case "login_rejected":
		status = http.StatusConflict
		message = "The upstream rejected this automatic login session. Complete login or verification in the browser, then use the browser helper."
		humanAction = "browser_pairing"
	case "bad_credentials":
		status = http.StatusUnauthorized
		message = "The upstream rejected the username or password."
	case "network_error":
		status = http.StatusBadGateway
		message = "The upstream login service is temporarily unreachable."
	case "cross_origin_blocked", "invalid_origin", "invalid_login_rule", "insufficient_login_material":
		message = "The declarative login contract is incomplete or no longer matches the upstream."
	}
	writeJSON(w, status, map[string]any{
		"status": "failed", "error_class": class, "message": message, "human_action": humanAction,
	})
}
