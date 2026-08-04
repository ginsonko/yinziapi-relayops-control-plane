package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

// CredentialValidator verifies newly supplied material before it is persisted.
// Implementations may only use bounded, read-only requests and must never call
// model generation endpoints.
type CredentialValidator interface {
	ValidateCredential(context.Context, domain.Supplier, string, domain.CredentialPayload) (domain.CollectionDiagnostics, error)
}

// CredentialValidationError is deliberately safe to return through an API. It
// carries a structured class and capability without echoing upstream bodies,
// URLs with query strings, or any supplied credential material.
type CredentialValidationError struct {
	Capability string
	Class      string
	Message    string
	Cause      error
}

func (e *CredentialValidationError) Error() string {
	if e == nil {
		return ""
	}
	return sanitizeDiagnosticMessage(e.Message)
}

func (e *CredentialValidationError) ErrorClass() string {
	if e == nil {
		return ""
	}
	return e.Class
}

func (e *CredentialValidationError) Unwrap() error { return e.Cause }

// ValidateCredential selects the supplier's native connector and performs an
// ephemeral capability check. It does not create, update, or bind credentials.
func (f *Factory) ValidateCredential(ctx context.Context, supplier domain.Supplier, kind string, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	connector, _, err := f.ForSupplier(ctx, supplier)
	if err != nil {
		return domain.CollectionDiagnostics{}, err
	}
	validator, ok := connector.(CredentialValidator)
	if !ok {
		return domain.CollectionDiagnostics{}, &CredentialValidationError{
			Class: "contract_changed", Message: fmt.Sprintf("connector %q does not provide a bounded credential validation probe", connector.Name()),
		}
	}
	return validator.ValidateCredential(ctx, supplier, kind, payload)
}

func (c *NewAPI) ValidateCredential(ctx context.Context, supplier domain.Supplier, kind string, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	session := newDiagnosticSession(supplier.ID)
	payload, err := normalizeCredentialForValidation(payload)
	if err != nil {
		return session.result(), validationFailure("newapi", "", err, "credential material is not valid")
	}
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return session.result(), validationFailure("newapi", "", err, "supplier URL is not valid")
	}
	client, err := netguard.SameOriginClient(c.client, base)
	if err != nil {
		return session.result(), validationFailure("newapi", "", err, "supplier origin could not be secured")
	}
	requestCtx, cancel := requestDeadline(ctx, 30*time.Second)
	defer cancel()

	_, publicErr := c.negotiateEnvelope(requestCtx, session, client, supplier, base, "status", "status", authAnonymous,
		metadataValue(supplier, "newapi_status_path", ""), []string{"/api/status", "/api/setting"}, domain.CredentialPayload{})
	recordCapabilityFromRequest(session, authAnonymous, nil, "status", publicErr)

	role := validationCredentialRole(kind, payload)
	switch role {
	case authBusinessAPIKey:
		_, catalogErr := c.negotiateModelCatalog(requestCtx, session, client, supplier, base, authBusinessAPIKey,
			metadataValue(supplier, "newapi_models_path", ""), []string{"/v1/models", "/api/models"}, payload)
		recordEphemeralCapability(session, authBusinessAPIKey, "catalog", catalogErr)
		_, usageErr := c.negotiateJSONDocument(requestCtx, session, client, supplier, base, "usage", "usage", authBusinessAPIKey,
			metadataValue(supplier, "usage_path", ""), []string{"/api/data/self", "/api/user/usage", "/api/user/dashboard"}, payload,
			newAPIUsageValidator(supplier))
		recordEphemeralCapability(session, authBusinessAPIKey, "usage", usageErr)
		if catalogErr == nil {
			session.diagnostics.CatalogSucceeded = true
			return session.result(), nil
		}
		return session.result(), validationFailure("newapi", authBusinessAPIKey, catalogErr, "NewAPI business API Key could not read the model catalog")
	case authSiteAdmin:
		_, adminErr := c.negotiateEnvelope(requestCtx, session, client, supplier, base, "admin_read", "admin_read", authSiteAdmin,
			metadataValue(supplier, "newapi_admin_probe_path", ""), []string{"/api/channel/?p=1&page_size=1", "/api/channel?page=1&page_size=1"}, payload)
		recordEphemeralCapability(session, authSiteAdmin, "admin_read", adminErr)
		if adminErr == nil {
			return session.result(), nil
		}
		return session.result(), validationFailure("newapi", authSiteAdmin, adminErr, "NewAPI site-admin capability could not be verified")
	default:
		_, selfErr := c.negotiateEnvelope(requestCtx, session, client, supplier, base, "customer_self", "customer_self", authCustomerSession,
			"", []string{"/api/user/self", "/api/user/me", "/api/user/profile"}, payload)
		recordEphemeralCapability(session, authCustomerSession, "customer_self", selfErr)
		_, balanceErr := c.negotiateJSONDocument(requestCtx, session, client, supplier, base, "balance", "balance", authCustomerSession,
			supplier.BalancePath, []string{"/api/user/self", "/api/user/balance", "/api/user/quota"}, payload,
			newAPIBalanceValidator(supplier))
		recordEphemeralCapability(session, authCustomerSession, "balance", balanceErr)
		_, usageErr := c.negotiateJSONDocument(requestCtx, session, client, supplier, base, "usage", "usage", authCustomerSession,
			metadataValue(supplier, "usage_path", ""), []string{"/api/data/self", "/api/user/usage", "/api/user/dashboard"}, payload,
			newAPIUsageValidator(supplier))
		recordEphemeralCapability(session, authCustomerSession, "usage", usageErr)
		privateErr := preferredCapabilityError(preferredCapabilityError(selfErr, balanceErr), usageErr)
		if selfErr != nil && balanceErr != nil && usageErr != nil {
			message := "NewAPI customer session could not be verified"
			if payload.UserID == 0 {
				message += "; New-Api-User user id is missing or the captured session/token is invalid"
			}
			return session.result(), validationFailure("newapi", authCustomerSession, privateErr, message)
		}
		session.diagnostics.BalanceSucceeded = balanceErr == nil

		// A browser session can belong to either a normal customer or a site
		// administrator. This optional GET makes that distinction explicit; a
		// denied admin probe does not invalidate an otherwise valid customer.
		_, adminErr := c.negotiateEnvelope(requestCtx, session, client, supplier, base, "admin_read", "admin_read", authSiteAdmin,
			metadataValue(supplier, "newapi_admin_probe_path", ""), []string{"/api/channel/?p=1&page_size=1", "/api/channel?page=1&page_size=1"}, payload)
		recordEphemeralCapability(session, authSiteAdmin, "admin_read", adminErr)
		return session.result(), nil
	}
}

func (c *Sub2API) ValidateCredential(ctx context.Context, supplier domain.Supplier, kind string, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	session := newDiagnosticSession(supplier.ID)
	payload, err := normalizeCredentialForValidation(payload)
	if err != nil {
		return session.result(), validationFailure("sub2api", "", err, "credential material is not valid")
	}
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return session.result(), validationFailure("sub2api", "", err, "supplier URL is not valid")
	}
	client, err := netguard.SameOriginClient(c.client, base)
	if err != nil {
		return session.result(), validationFailure("sub2api", "", err, "supplier origin could not be secured")
	}
	requestCtx, cancel := requestDeadline(ctx, 30*time.Second)
	defer cancel()

	publicErr := c.probeHealthCandidates(requestCtx, session, client, supplier, base)
	recordCapabilityFromRequest(session, authAnonymous, nil, "health", publicErr)

	mode := sub2CollectionMode(supplier)
	role := validationCredentialRole(kind, payload)
	if mode == sub2ModeAdmin {
		return c.validateSub2AdminCredential(requestCtx, session, client, base, supplier, payload)
	}
	if mode == sub2ModeAuto {
		adminDiagnostics, adminErr := c.validateSub2AdminCredential(requestCtx, session, client, base, supplier, payload)
		_ = adminDiagnostics
		if adminErr == nil {
			return session.result(), nil
		}
	}
	if role == authCustomerSession {
		identityPath := metadataValue(supplier, "sub2api_customer_identity_path", "")
		if identityPath == "" {
			identityPath = metadataValue(supplier, "sub2api_customer_balance_path", "")
		}
		_, identityBody, identityErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_identity", "customer_identity", role,
			identityPath, []string{"/api/v1/auth/me", "/api/v1/user/profile", "/api/v1/user"}, payload,
			2<<20, validateSub2CustomerIdentity)
		recordEphemeralCapability(session, role, "customer_identity", identityErr)
		balanceErr := identityErr
		if identityErr == nil {
			_, balanceErr = parseSub2CustomerProfile(identityBody, time.Now().UTC())
		}
		recordDerivedCapability(session, role, nil, "customer_balance", balanceErr, true)
		_, _, groupsErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_groups", "customer_groups", role,
			metadataValue(supplier, "sub2api_customer_groups_path", ""), []string{"/api/v1/groups/available"}, payload,
			4<<20, validateSub2CustomerGroups)
		recordEphemeralCapability(session, role, "customer_groups", groupsErr)
		_, _, channelsErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_channels", "customer_channels", role,
			metadataValue(supplier, "sub2api_customer_catalog_path", ""), []string{"/api/v1/channels/available"}, payload,
			maxResponseBytes, validateSub2CustomerChannels)
		recordEphemeralCapability(session, role, "customer_channels", channelsErr)
		catalogErr := preferredCapabilityError(groupsErr, channelsErr)
		if groupsErr == nil || channelsErr == nil {
			catalogErr = nil
		}
		recordDerivedCapability(session, role, nil, "customer_catalog", catalogErr, true)
		_, _, ratesErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_rates", "customer_rates", role,
			metadataValue(supplier, "sub2api_customer_rates_path", ""), []string{"/api/v1/groups/rates"}, payload,
			2<<20, validateSub2CustomerRates)
		recordEphemeralCapability(session, role, "customer_rates", ratesErr)
		_, _, usageErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_usage", "customer_usage", role,
			metadataValue(supplier, "sub2api_customer_usage_path", ""), []string{"/api/v1/usage", "/api/v1/usage/dashboard/stats", "/api/v1/usage/stats?period=today", "/api/v1/usage/dashboard/snapshot-v2"}, payload,
			4<<20, validateSub2CustomerUsage)
		recordEphemeralCapability(session, role, "customer_usage", usageErr)
		_, _, keysErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "customer_keys", "customer_keys", role,
			metadataValue(supplier, "sub2api_customer_keys_path", ""), []string{"/api/v1/keys?page=1&page_size=1"}, payload,
			2<<20, validateSub2CustomerKeys)
		recordEphemeralCapability(session, role, "customer_keys", keysErr)
		if identityErr != nil {
			return session.result(), validationFailure("sub2api", role, identityErr,
				"Sub2API customer session login material was returned, but the current identity endpoint rejected it")
		}
		// The current-identity endpoint is the authorization authority. Catalog,
		// balance, usage, and key endpoints are optional product capabilities on
		// Sub2 deployments and may legitimately be disabled for customer roles.
		// Preserve their diagnostics, but do not turn a proven login into a false
		// username/password rejection.
		session.diagnostics.CatalogSucceeded = catalogErr == nil || keysErr == nil
		session.diagnostics.BalanceSucceeded = balanceErr == nil
		return session.result(), nil
	}

	modelsPath := supplier.PricingPath
	if strings.TrimSpace(modelsPath) == "" {
		modelsPath = metadataValue(supplier, "sub2api_models_path", "")
	}
	_, catalogErr := c.negotiateModelCatalog(requestCtx, session, client, supplier, base, role, modelsPath,
		[]string{"/v1/models", "/api/v1/models"}, payload)
	recordEphemeralCapability(session, role, "catalog", catalogErr)
	_, _, usageErr := c.fetchNegotiatedDocument(requestCtx, session, client, supplier, base, "usage", "usage", role,
		metadataValue(supplier, "sub2api_usage_path", ""), []string{"/v1/usage", "/api/v1/usage"}, payload,
		4<<20, validateSub2KeyUsage)
	recordEphemeralCapability(session, role, "usage", usageErr)
	if catalogErr == nil {
		session.diagnostics.CatalogSucceeded = true
		return session.result(), nil
	}
	if role == authCustomerSession {
		return session.result(), validationFailure("sub2api", role, catalogErr, "Sub2API browser session did not prove model-catalog access; this supplier requires a business API Key unless an authorized admin mode is configured")
	}
	return session.result(), validationFailure("sub2api", authBusinessAPIKey, catalogErr, "Sub2API business API Key could not read the model catalog")
}

func (c *Sub2API) validateSub2AdminCredential(ctx context.Context, session *diagnosticSession, client *http.Client, base string, supplier domain.Supplier, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	_, err := c.negotiateEnvelope(ctx, session, client, supplier, base, "admin_groups", "admin_groups", authSiteAdmin,
		metadataValue(supplier, "sub2api_admin_groups_path", ""), []string{"/api/v1/admin/groups/all", "/api/admin/groups"}, payload)
	recordEphemeralCapability(session, authSiteAdmin, "admin_groups", err)
	if err == nil {
		session.diagnostics.CatalogSucceeded = true
		return session.result(), nil
	}
	return session.result(), validationFailure("sub2api", authSiteAdmin, err, "Sub2API site-admin capability could not be verified")
}

func validationCredentialRole(kind string, payload domain.CredentialPayload) string {
	lower := strings.ToLower(strings.TrimSpace(kind))
	if strings.Contains(lower, "admin") || strings.Contains(lower, "management") {
		return authSiteAdmin
	}
	return credentialCapability(kind, payload, false)
}

func normalizeCredentialForValidation(payload domain.CredentialPayload) (domain.CredentialPayload, error) {
	payload = credentials.NormalizeCapturedPayload(payload)
	payload.Headers = cloneStringMap(payload.Headers)
	payload.LocalStorage = cloneStringMap(payload.LocalStorage)
	payload.SessionStorage = cloneStringMap(payload.SessionStorage)
	if strings.TrimSpace(payload.Authorization) == "" {
		for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
			for _, key := range []string{"auth_token", "access_token", "token"} {
				if value := strings.TrimSpace(values[key]); value != "" {
					payload.Authorization = "Bearer " + trimValidationBearer(value)
					break
				}
			}
			if payload.Authorization != "" {
				break
			}
		}
	}
	if payload.UserID == 0 {
		for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
			for _, key := range []string{"uid", "user_id"} {
				value := strings.Trim(strings.TrimSpace(values[key]), `"`)
				if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
					payload.UserID = parsed
					break
				}
			}
			if payload.UserID > 0 {
				break
			}
		}
	}

	mode := strings.ToLower(strings.TrimSpace(payload.AuthorizationMode))
	if mode == "" {
		if value := strings.TrimSpace(payload.Authorization); value != "" && !strings.HasPrefix(strings.ToLower(value), "bearer ") {
			mode = "raw"
		} else {
			mode = "bearer"
		}
	}
	switch mode {
	case "bearer":
		if value := strings.TrimSpace(payload.Authorization); value != "" && !strings.HasPrefix(strings.ToLower(value), "bearer ") {
			payload.Authorization = "Bearer " + value
		}
	case "raw":
		// Keep the caller's exact Authorization value.
	case "header":
		header := strings.TrimSpace(payload.AuthorizationHeader)
		if header == "" || strings.ContainsAny(header, "\r\n:") {
			return domain.CredentialPayload{}, errors.New("authorization_header is required for header authorization mode")
		}
		if payload.Headers == nil {
			payload.Headers = make(map[string]string)
		}
		if value := strings.TrimSpace(payload.Authorization); value != "" {
			payload.Headers[header] = trimValidationBearer(value)
		}
		payload.Authorization = ""
	default:
		return domain.CredentialPayload{}, fmt.Errorf("unsupported authorization_mode %q", payload.AuthorizationMode)
	}
	payload.AuthorizationMode = mode
	return payload, nil
}

func trimValidationBearer(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func probeNewAPIEnvelope(ctx context.Context, session *diagnosticSession, client *http.Client, requestURL, endpointType, capability string, payload domain.CredentialPayload) error {
	response, err := session.request(ctx, client, diagnosticRequest{EndpointType: endpointType, AuthCapability: capability, Method: http.MethodGet, URL: requestURL, Credential: payload, MaxBytes: 2 << 20})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: endpointType, StatusCode: response.StatusCode}
	}
	if strings.EqualFold(responseSchema(response.Header, response.Body), "html") || bytes.HasPrefix(bytes.TrimSpace(response.Body), []byte("<")) {
		session.markLastContract(endpointType, "html", "auth_missing", "endpoint returned a login page")
		return &DiagnosticError{Class: "auth_missing", Operation: endpointType}
	}
	var envelope struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		session.markLastContract(endpointType, "unknown", "contract_changed", "response is not valid JSON")
		return &DiagnosticError{Class: "contract_changed", Operation: endpointType, Cause: err}
	}
	if !envelope.Success || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		class := classifyContractMessage(envelope.Message)
		session.markLastContract(endpointType, "newapi.envelope.v1", class, "response did not prove the requested capability")
		return &DiagnosticError{Class: class, Operation: endpointType}
	}
	session.markLastContract(endpointType, "newapi.envelope.v1", "", "")
	return nil
}

func validateNewAPIBalanceDocument(body []byte) error {
	data, message, err := newAPIEnvelopeObject(body)
	if err != nil {
		if class := classifyContractMessage(message); class != "contract_changed" {
			return &DiagnosticError{Class: class, Operation: "balance", Cause: err}
		}
		return &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: err}
	}
	if value, _ := firstNumeric(data, "balance", "remaining", "available", "quota"); value == "" {
		return &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: errors.New("no recognized balance field")}
	}
	return nil
}

func newAPIBalanceValidator(supplier domain.Supplier) capabilityDocumentValidator {
	if paths, custom := manifestJSONPaths(supplier, "balance"); custom {
		return validateManifestBalanceDocument(paths)
	}
	return validateNewAPIBalanceDocument
}

func newAPIUsageValidator(supplier domain.Supplier) capabilityDocumentValidator {
	if paths, custom := manifestJSONPaths(supplier, "usage"); custom {
		return validateManifestUsageDocument(paths)
	}
	return validateNewAPIUsageDocument
}

func validateNewAPIUsageDocument(body []byte) error {
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err}
	}
	if envelope.Success != nil && !*envelope.Success {
		return &DiagnosticError{Class: classifyContractMessage(envelope.Message), Operation: "usage"}
	}
	if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: errors.New("usage data is missing")}
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rows); err == nil {
		return nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err}
	}
	if value, _ := firstNumeric(data, "daily_spend", "today_actual_cost", "today_cost", "actual_cost", "used_quota", "quota"); value == "" {
		return &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: errors.New("no recognized usage field")}
	}
	return nil
}

func newAPIEnvelopeObject(body []byte) (map[string]json.RawMessage, string, error) {
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, "", err
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, envelope.Message, errors.New("upstream reported success=false")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return nil, envelope.Message, err
	}
	return data, envelope.Message, nil
}

func probeModelCatalog(ctx context.Context, session *diagnosticSession, client *http.Client, requestURL, capability string, payload domain.CredentialPayload) error {
	response, err := session.request(ctx, client, diagnosticRequest{EndpointType: "catalog", AuthCapability: capability, Method: http.MethodGet, URL: requestURL, Credential: payload, MaxBytes: 4 << 20})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "catalog", StatusCode: response.StatusCode}
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		session.markLastContract("catalog", "unknown", "contract_changed", "model-list response contract is not recognized")
		return &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: err}
	}
	var models []json.RawMessage
	if err := json.Unmarshal(envelope.Data, &models); err != nil {
		session.markLastContract("catalog", "unknown", "contract_changed", "model-list data is not an array")
		return &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: err}
	}
	if len(models) == 0 {
		session.markLastContract("catalog", "openai.models.v1", "contract_changed", "model-list data is empty")
		return &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: errors.New("model-list data is empty")}
	}
	session.markLastContract("catalog", "openai.models.v1", "", "")
	return nil
}

func probeSub2Health(ctx context.Context, session *diagnosticSession, client *http.Client, requestURL string) error {
	response, err := session.request(ctx, client, diagnosticRequest{EndpointType: "health", AuthCapability: authAnonymous, Method: http.MethodGet, URL: requestURL, MaxBytes: 1 << 20})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "health", StatusCode: response.StatusCode}
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(response.Body, &health); err != nil || !strings.EqualFold(strings.TrimSpace(health.Status), "ok") {
		session.markLastContract("health", "unknown", "contract_changed", "health response contract is not recognized")
		return &DiagnosticError{Class: "contract_changed", Operation: "health", Cause: err}
	}
	session.markLastContract("health", "sub2api.health.v1", "", "")
	return nil
}

func probeSub2Envelope(ctx context.Context, session *diagnosticSession, client *http.Client, requestURL, endpointType, capability string, payload domain.CredentialPayload) error {
	response, err := session.request(ctx, client, diagnosticRequest{EndpointType: endpointType, AuthCapability: capability, Method: http.MethodGet, URL: requestURL, Credential: payload, MaxBytes: 2 << 20})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: endpointType, StatusCode: response.StatusCode}
	}
	var envelope sub2Envelope
	if err := json.Unmarshal(response.Body, &envelope); err != nil {
		session.markLastContract(endpointType, "unknown", "contract_changed", "response is not valid JSON")
		return &DiagnosticError{Class: "contract_changed", Operation: endpointType, Cause: err}
	}
	if !sub2EnvelopeCodeOK(envelope.Code) || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		class := classifyContractMessage(envelope.Message)
		session.markLastContract(endpointType, "sub2api.envelope.v1", class, "response did not prove the requested capability")
		return &DiagnosticError{Class: class, Operation: endpointType}
	}
	session.markLastContract(endpointType, "sub2api.envelope.v1", "", "")
	return nil
}

func probeJSONDocument(ctx context.Context, session *diagnosticSession, client *http.Client, requestURL, endpointType,
	capability string, payload domain.CredentialPayload) error {
	response, err := session.request(ctx, client, diagnosticRequest{EndpointType: endpointType, AuthCapability: capability,
		Method: http.MethodGet, URL: requestURL, Credential: payload, MaxBytes: 4 << 20})
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: endpointType, StatusCode: response.StatusCode}
	}
	var document any
	if err := json.Unmarshal(response.Body, &document); err != nil || document == nil {
		session.markLastContract(endpointType, "unknown", "contract_changed", "response is not a JSON document")
		return &DiagnosticError{Class: "contract_changed", Operation: endpointType, Cause: err}
	}
	session.markLastContract(endpointType, "json.read.v1", "", "")
	return nil
}

func classifyContractMessage(message string) string {
	lower := strings.ToLower(message)
	for _, marker := range []string{"unauthor", "not logged", "login", "sign in", "token expired", "invalid token", "api key", "未登录", "请登录", "令牌无效", "令牌过期", "密钥无效"} {
		if strings.Contains(lower, marker) {
			return "auth_missing"
		}
	}
	for _, marker := range []string{"forbidden", "permission", "not allowed", "access denied", "无权限", "权限不足", "无权访问"} {
		if strings.Contains(lower, marker) {
			return "permission_denied"
		}
	}
	return "contract_changed"
}

func recordEphemeralCapability(session *diagnosticSession, capability, endpointType string, requestErr error) {
	request, ok := requestEvidenceForOutcome(session, endpointType, capability, requestErr)
	if !ok {
		session.capability(capability, "unverified", nil, endpointType, 0, 0, ErrorClass(requestErr), "no request evidence was produced")
		return
	}
	status := "verified"
	errorClass := request.ErrorClass
	message := request.ErrorMessage
	if requestErr != nil || request.Status != "success" || request.HTTPStatus < 200 || request.HTTPStatus >= 300 {
		switch errorClass {
		case "auth_missing":
			status = "expired_or_invalid"
			errorClass = "auth_expired"
		case "permission_denied":
			status = "denied"
		default:
			status = "failed"
		}
	}
	session.capability(capability, status, nil, endpointType, request.HTTPStatus, request.DurationMS, errorClass, message)
}

func validationFailure(adapter, capability string, cause error, message string) error {
	class := ErrorClass(cause)
	if class == "unknown_error" && cause != nil {
		class = "contract_changed"
	}
	return &CredentialValidationError{Capability: capability, Class: class, Message: adapter + " credential validation failed: " + message, Cause: cause}
}
