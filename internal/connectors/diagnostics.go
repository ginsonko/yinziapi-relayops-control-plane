package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

const (
	authAnonymous       = "anonymous"
	authCustomerSession = "customer_session"
	authBusinessAPIKey  = "business_api_key"
	authSiteAdmin       = "site_admin"
)

type DiagnosticError struct {
	Class      string
	Operation  string
	StatusCode int
	Retryable  bool
	Cause      error
}

func (e *DiagnosticError) Error() string {
	if e == nil {
		return ""
	}
	message := e.Class
	if e.Operation != "" {
		message = e.Operation + ": " + message
	}
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.Cause != nil {
		cause := sanitizeDiagnosticMessage(e.Cause.Error())
		if cause == "" {
			cause = diagnosticClassMessage(e.Class)
		}
		message += ": " + cause
	}
	return message
}

func (e *DiagnosticError) Unwrap() error { return e.Cause }

// ErrorClass lets callers classify connector failures without importing or
// depending on the concrete DiagnosticError type.
func (e *DiagnosticError) ErrorClass() string {
	if e == nil {
		return ""
	}
	return e.Class
}

func ErrorClass(err error) string {
	var classified interface{ ErrorClass() string }
	if errors.As(err, &classified) {
		if class := classified.ErrorClass(); class != "" {
			return class
		}
	}
	return classifyTransportError(err)
}

type diagnosticSession struct {
	supplierID  int64
	diagnostics domain.CollectionDiagnostics
}

type diagnosticRequest struct {
	EndpointType   string
	AuthCapability string
	Method         string
	URL            string
	Body           []byte
	Credential     domain.CredentialPayload
	MaxBytes       int64
	RetryGET       bool
	SemanticRead   bool
}

type diagnosticResponse struct {
	Body       []byte
	StatusCode int
	Status     string
	Header     http.Header
}

func newDiagnosticSession(supplierID int64) *diagnosticSession {
	return &diagnosticSession{supplierID: supplierID, diagnostics: domain.CollectionDiagnostics{
		Requests: []domain.RequestEvidence{}, Capabilities: []domain.AuthCapabilityEvidence{},
	}}
}

func (s *diagnosticSession) result() domain.CollectionDiagnostics {
	if s == nil {
		return domain.CollectionDiagnostics{Requests: []domain.RequestEvidence{}, Capabilities: []domain.AuthCapabilityEvidence{}}
	}
	return s.diagnostics
}

func (s *diagnosticSession) request(ctx context.Context, client *http.Client, spec diagnosticRequest) (diagnosticResponse, error) {
	if client == nil {
		return diagnosticResponse{}, &DiagnosticError{Class: "unknown_error", Operation: spec.EndpointType, Cause: errors.New("HTTP client is not configured")}
	}
	if spec.MaxBytes <= 0 {
		spec.MaxBytes = maxResponseBytes
	}
	maxAttempts := 1
	if spec.RetryGET && spec.Method == http.MethodGet {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		started := time.Now().UTC()
		requestContext := ctx
		if spec.SemanticRead {
			var permitErr error
			requestContext, permitErr = remotewrite.WithReadOnlyRequest(ctx, spec.Method, spec.URL, spec.Body)
			if permitErr != nil {
				return diagnosticResponse{}, &DiagnosticError{Class: "contract_changed", Operation: spec.EndpointType, Cause: permitErr}
			}
		}
		req, err := http.NewRequestWithContext(requestContext, spec.Method, spec.URL, bytes.NewReader(spec.Body))
		if err != nil {
			return diagnosticResponse{}, &DiagnosticError{Class: "contract_changed", Operation: spec.EndpointType, Cause: err}
		}
		req.Header.Set("Accept", "application/json")
		if len(spec.Body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		applyCredential(req, spec.Credential)
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			finished := time.Now().UTC()
			class := classifyTransportError(requestErr)
			s.appendRequest(spec, started, finished, 0, "failed", class, requestErr, "", "")
			lastErr = &DiagnosticError{Class: class, Operation: spec.EndpointType, Retryable: retryableErrorClass(class), Cause: requestErr}
			if attempt < maxAttempts && retryableErrorClass(class) {
				if err := waitRetry(ctx, s.supplierID, attempt); err != nil {
					return diagnosticResponse{}, &DiagnosticError{Class: classifyTransportError(err), Operation: spec.EndpointType, Cause: err}
				}
				continue
			}
			return diagnosticResponse{}, lastErr
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, spec.MaxBytes+1))
		_ = resp.Body.Close()
		finished := time.Now().UTC()
		if readErr != nil {
			class := classifyReadError(readErr)
			s.appendRequest(spec, started, finished, resp.StatusCode, "failed", class, readErr, responseSchema(resp.Header, nil), "")
			lastErr = &DiagnosticError{Class: class, Operation: spec.EndpointType, StatusCode: resp.StatusCode, Retryable: retryableErrorClass(class), Cause: readErr}
			if attempt < maxAttempts && retryableErrorClass(class) {
				if err := waitRetry(ctx, s.supplierID, attempt); err != nil {
					return diagnosticResponse{}, &DiagnosticError{Class: classifyTransportError(err), Operation: spec.EndpointType, Cause: err}
				}
				continue
			}
			return diagnosticResponse{}, lastErr
		}
		if int64(len(body)) > spec.MaxBytes {
			err := fmt.Errorf("response exceeded %d bytes", spec.MaxBytes)
			s.appendRequest(spec, started, finished, resp.StatusCode, "failed", "contract_changed", err, responseSchema(resp.Header, body), responseHash(body))
			return diagnosticResponse{}, &DiagnosticError{Class: "contract_changed", Operation: spec.EndpointType, StatusCode: resp.StatusCode, Cause: err}
		}
		status := "success"
		errorClass := ""
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			status = "failed"
			errorClass = classifyHTTPStatus(resp.StatusCode)
		}
		s.appendRequest(spec, started, finished, resp.StatusCode, status, errorClass, nil, responseSchema(resp.Header, body), responseHash(body))
		result := diagnosticResponse{Body: body, StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
		if attempt < maxAttempts && retryableStatus(resp.StatusCode) {
			lastErr = &DiagnosticError{Class: errorClass, Operation: spec.EndpointType, StatusCode: resp.StatusCode, Retryable: true}
			if err := waitResponseRetry(ctx, s.supplierID, attempt, resp.StatusCode, resp.Header.Get("Retry-After")); err != nil {
				return diagnosticResponse{}, &DiagnosticError{Class: classifyTransportError(err), Operation: spec.EndpointType, Cause: err}
			}
			continue
		}
		return result, nil
	}
	return diagnosticResponse{}, lastErr
}

func (s *diagnosticSession) appendRequest(spec diagnosticRequest, started, finished time.Time, statusCode int, status, errorClass string, requestErr error, schema, hash string) {
	if s == nil {
		return
	}
	duration := finished.Sub(started).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	message := ""
	if requestErr != nil {
		message = diagnosticClassMessage(errorClass)
	} else if statusCode >= 400 {
		message = fmt.Sprintf("HTTP %d", statusCode)
	}
	s.diagnostics.Requests = append(s.diagnostics.Requests, domain.RequestEvidence{
		SupplierID: s.supplierID, EndpointType: spec.EndpointType, AuthCapability: spec.AuthCapability,
		Method: spec.Method, SourceURL: safeEvidenceURL(spec.URL), HTTPStatus: statusCode, Status: status,
		ErrorClass: errorClass, ErrorMessage: message, DurationMS: duration, SchemaVersion: schema,
		ResponseHash: hash, ObservedAt: finished,
	})
}

func (s *diagnosticSession) markLastContract(endpointType, schema, errorClass, message string) {
	if s == nil {
		return
	}
	for index := len(s.diagnostics.Requests) - 1; index >= 0; index-- {
		item := &s.diagnostics.Requests[index]
		if item.EndpointType != endpointType {
			continue
		}
		if schema != "" {
			item.SchemaVersion = schema
		}
		if errorClass != "" {
			item.Status = "failed"
			item.ErrorClass = errorClass
			item.ErrorMessage = sanitizeDiagnosticMessage(message)
		}
		return
	}
}

func (s *diagnosticSession) capability(capability, status string, credentialID *int64, endpointType string, statusCode int, durationMS int64, errorClass, message string) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	item := domain.AuthCapabilityEvidence{
		SupplierID: s.supplierID, Capability: capability, Status: status, CredentialID: credentialID,
		EndpointType: endpointType, HTTPStatus: statusCode, DurationMS: durationMS, ErrorClass: errorClass,
		ErrorMessage: sanitizeDiagnosticMessage(message), LastAttemptAt: now,
	}
	if status == "verified" {
		verified := now
		item.VerifiedAt = &verified
	}
	for index := range s.diagnostics.Capabilities {
		if s.diagnostics.Capabilities[index].Capability == capability && s.diagnostics.Capabilities[index].EndpointType == endpointType {
			s.diagnostics.Capabilities[index] = item
			return
		}
	}
	s.diagnostics.Capabilities = append(s.diagnostics.Capabilities, item)
}

func (s *diagnosticSession) lastRequest(endpointType, authCapability string) (domain.RequestEvidence, bool) {
	if s == nil {
		return domain.RequestEvidence{}, false
	}
	for index := len(s.diagnostics.Requests) - 1; index >= 0; index-- {
		item := s.diagnostics.Requests[index]
		if item.EndpointType == endpointType && (authCapability == "" || item.AuthCapability == authCapability) {
			return item, true
		}
	}
	return domain.RequestEvidence{}, false
}

func credentialCapability(kind string, payload domain.CredentialPayload, admin bool) string {
	if admin {
		return authSiteAdmin
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if strings.Contains(kind, "api-key") || strings.Contains(kind, "apikey") || strings.Contains(kind, "business") {
		return authBusinessAPIKey
	}
	for key := range payload.Headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(lower, "api-key") || lower == "x-token" {
			return authBusinessAPIKey
		}
	}
	if payload.Cookie != "" || strings.Contains(kind, "browser") || strings.Contains(kind, "cookie") || strings.Contains(kind, "session") {
		return authCustomerSession
	}
	if payload.Authorization != "" {
		return authCustomerSession
	}
	return authCustomerSession
}

func recordCapabilityFromRequest(session *diagnosticSession, capability string, credentialID *int64, endpointType string, requestErr error) {
	request, ok := requestEvidenceForOutcome(session, endpointType, capability, requestErr)
	if !ok {
		status := "missing"
		if credentialID != nil {
			status = "unverified"
		}
		session.capability(capability, status, credentialID, endpointType, 0, 0, ErrorClass(requestErr), "no request evidence was produced")
		return
	}
	status := "verified"
	errorClass := request.ErrorClass
	message := request.ErrorMessage
	if requestErr != nil || request.Status != "success" || request.HTTPStatus < 200 || request.HTTPStatus >= 300 {
		switch errorClass {
		case "auth_missing":
			if credentialID != nil {
				status = "expired_or_invalid"
				errorClass = "auth_expired"
			} else {
				status = "missing"
			}
		case "permission_denied":
			status = "denied"
		default:
			status = "failed"
		}
	}
	session.capability(capability, status, credentialID, endpointType, request.HTTPStatus, request.DurationMS, errorClass, message)
}

// recordDerivedCapability exposes a compatibility/summary capability derived
// from independently evidenced child capabilities. It deliberately does not
// invent a synthetic HTTP request.
func recordDerivedCapability(session *diagnosticSession, capability string, credentialID *int64,
	endpointType string, requestErr error, ephemeral bool) {
	status := "verified"
	errorClass := ""
	message := "derived from independently probed capabilities"
	if requestErr != nil {
		errorClass = ErrorClass(requestErr)
		message = "all underlying capability probes failed"
		switch errorClass {
		case "auth_missing":
			if ephemeral || credentialID != nil {
				status = "expired_or_invalid"
				errorClass = "auth_expired"
			} else {
				status = "missing"
			}
		case "permission_denied":
			status = "denied"
		default:
			status = "failed"
		}
	}
	session.capability(capability, status, credentialID, endpointType, 0, 0, errorClass, message)
}

// requestEvidenceForOutcome keeps negotiated-capability evidence aligned with
// the error selected by the bounded negotiator. A later 404 from a fallback
// path must not hide an earlier, more meaningful 401/403 from the matching
// capability, while successful negotiation still resolves to the successful
// request.
func requestEvidenceForOutcome(session *diagnosticSession, endpointType, capability string, requestErr error) (domain.RequestEvidence, bool) {
	if session == nil {
		return domain.RequestEvidence{}, false
	}
	targetClass := ErrorClass(requestErr)
	for index := len(session.diagnostics.Requests) - 1; index >= 0; index-- {
		item := session.diagnostics.Requests[index]
		if item.EndpointType != endpointType || (capability != "" && item.AuthCapability != capability) {
			continue
		}
		if requestErr == nil {
			if item.Status == "success" && item.HTTPStatus >= 200 && item.HTTPStatus < 300 {
				return item, true
			}
			continue
		}
		if item.ErrorClass == targetClass || classifyHTTPStatus(item.HTTPStatus) == targetClass {
			return item, true
		}
	}
	return session.lastRequest(endpointType, capability)
}

func recordLatestCapability(session *diagnosticSession, capability string, credentialID *int64, requestErr error) {
	if session == nil {
		return
	}
	for index := len(session.diagnostics.Requests) - 1; index >= 0; index-- {
		request := session.diagnostics.Requests[index]
		if request.AuthCapability != capability {
			continue
		}
		recordCapabilityFromRequest(session, capability, credentialID, request.EndpointType, requestErr)
		return
	}
	recordCapabilityFromRequest(session, capability, credentialID, "catalog", requestErr)
}

func classifyHTTPStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "auth_missing"
	case status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 500:
		return "upstream_5xx"
	default:
		return "contract_changed"
	}
}

func classifyTransportError(err error) string {
	if err == nil {
		return "unknown_error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "connect_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connect_timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "connection reset"), strings.Contains(message, "forcibly closed"), strings.Contains(message, "eof"):
		return "connection_reset"
	case strings.Contains(message, "tls"), strings.Contains(message, "certificate"), strings.Contains(message, "x509"):
		return "tls_error"
	default:
		return "unknown_error"
	}
}

func classifyReadError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "read_timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "read_timeout"
	}
	if class := classifyTransportError(err); class != "unknown_error" {
		return class
	}
	return "unknown_error"
}

func retryableErrorClass(class string) bool {
	switch class {
	case "dns_error", "connect_timeout", "read_timeout", "connection_reset", "upstream_5xx", "rate_limited":
		return true
	default:
		return false
	}
}

func waitRetry(ctx context.Context, supplierID int64, attempt int) error {
	base := time.Duration(50*(1<<(attempt-1))) * time.Millisecond
	jitter := time.Duration((supplierID+int64(attempt*17))%31) * time.Millisecond
	return waitRetryDelay(ctx, base+jitter)
}

func waitResponseRetry(ctx context.Context, supplierID int64, attempt, status int, retryAfter string) error {
	if status != http.StatusTooManyRequests {
		return waitRetry(ctx, supplierID, attempt)
	}
	delay := time.Duration(attempt) * 500 * time.Millisecond
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if observedAt, err := http.ParseTime(strings.TrimSpace(retryAfter)); err == nil {
		delay = time.Until(observedAt)
		if delay < 0 {
			delay = 0
		}
	}
	if delay > 3*time.Second {
		delay = 3 * time.Second
	}
	return waitRetryDelay(ctx, delay)
}

func waitRetryDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func responseSchema(header http.Header, body []byte) string {
	contentType := strings.ToLower(header.Get("Content-Type"))
	trimmed := bytes.TrimSpace(body)
	if strings.Contains(contentType, "json") || len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "json"
	}
	if strings.Contains(contentType, "html") || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<!doctype html")) || bytes.HasPrefix(bytes.ToLower(trimmed), []byte("<html")) {
		return "html"
	}
	return "unknown"
}

func responseHash(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func safeEvidenceURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

var (
	diagnosticURLPattern         = regexp.MustCompile(`https?://[^\s<>"']+`)
	diagnosticBearerPattern      = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	diagnosticNamedSecretPattern = regexp.MustCompile(`(?i)\b(authorization|cookie|set-cookie|api[-_ ]?key|access[-_ ]?token|refresh[-_ ]?token|session[-_ ]?token|secret[-_ ]?key)\b\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^,;\s]+)`)
	diagnosticKeyPattern         = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]{8,}`)
)

func sanitizeDiagnosticMessage(message string) string {
	message = strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")), " ")
	message = diagnosticURLPattern.ReplaceAllStringFunc(message, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED_URL]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
	message = diagnosticBearerPattern.ReplaceAllString(message, "Bearer [REDACTED]")
	message = diagnosticNamedSecretPattern.ReplaceAllString(message, "$1=[REDACTED]")
	message = diagnosticKeyPattern.ReplaceAllString(message, "sk-[REDACTED]")
	if len(message) > 300 {
		return message[:300]
	}
	return message
}

func diagnosticClassMessage(class string) string {
	switch class {
	case "dns_error":
		return "DNS resolution failed"
	case "connect_timeout":
		return "connection timed out"
	case "read_timeout":
		return "response read timed out"
	case "connection_reset":
		return "connection was reset"
	case "tls_error":
		return "TLS verification or negotiation failed"
	case "rate_limited":
		return "upstream rate limited the request"
	case "upstream_5xx":
		return "upstream service returned a server error"
	case "auth_missing":
		return "authentication is required"
	case "auth_expired":
		return "credential is expired or invalid"
	case "permission_denied":
		return "credential lacks permission"
	case "contract_changed":
		return "response contract is not recognized"
	case "invalid_unit":
		return "monetary unit is incomplete or invalid"
	default:
		return "request failed"
	}
}
