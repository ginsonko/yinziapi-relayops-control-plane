package connectors

import (
	"context"
	"fmt"
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type capabilityDocumentValidator func([]byte) error

func (c *Sub2API) probeHealthCandidates(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base string) error {
	key := profileKey("sub2api", base, "health", authAnonymous)
	selections := capabilitySelections(c.profiles, supplier, "sub2api", base, "health", authAnonymous,
		metadataValue(supplier, "sub2api_health_path", ""), []string{"/health", "/api/health"}, domain.CredentialPayload{})
	var best error
	for _, selection := range selections {
		err := probeSub2Health(ctx, session, client, base+selection.Path)
		if err == nil {
			c.profiles.set(key, selection)
			return nil
		}
		best = preferredCapabilityError(best, err)
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: "health"}
	}
	return best
}

func (c *NewAPI) negotiateEnvelope(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, capability, endpointType, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload) (capabilitySelection, error) {
	key := profileKey("newapi", base, capability, role)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, capability, role, explicitPath, defaults, payload)
	var best error
	for _, selection := range selections {
		err := probeNewAPIEnvelope(ctx, session, client, base+selection.Path, endpointType, role,
			capabilityCredential(payload, selection.Auth))
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		best = preferredCapabilityError(best, err)
		if terminalCapabilityAuthError(err) {
			break
		}
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: endpointType}
	}
	return capabilitySelection{}, best
}

func (c *NewAPI) negotiateModelCatalog(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, role, explicitPath string, defaults []string, payload domain.CredentialPayload) (capabilitySelection, error) {
	key := profileKey("newapi", base, "catalog", role)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, "catalog", role, explicitPath, defaults, payload)
	var best error
	for _, selection := range selections {
		err := probeModelCatalog(ctx, session, client, base+selection.Path, role, capabilityCredential(payload, selection.Auth))
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		best = preferredCapabilityError(best, err)
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: "catalog"}
	}
	return capabilitySelection{}, best
}

func (c *NewAPI) negotiatePricingCatalog(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload) (capabilitySelection, newAPICatalog, diagnosticResponse, error) {
	key := profileKey("newapi", base, "pricing", role)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, "pricing", role, explicitPath, defaults, payload)
	var bestErr error
	var bestResponse diagnosticResponse
	retryGET := len(selections) == 1
	for _, selection := range selections {
		catalog, response, err := c.fetchPricingCatalog(ctx, client, base+selection.Path,
			capabilityCredential(payload, selection.Auth), role, session, retryGET)
		if err == nil {
			c.profiles.set(key, selection)
			return selection, catalog, response, nil
		}
		if bestErr == nil || capabilityErrorRank(err) > capabilityErrorRank(bestErr) {
			bestErr = err
			bestResponse = response
		}
		if terminalCapabilityAuthError(err) {
			break
		}
	}
	if bestErr == nil {
		bestErr = &DiagnosticError{Class: "contract_changed", Operation: "catalog"}
	}
	return capabilitySelection{}, newAPICatalog{}, bestResponse, bestErr
}

func (c *NewAPI) negotiateJSONDocument(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, capability, endpointType, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload, validate capabilityDocumentValidator) (capabilitySelection, error) {
	key := profileKey("newapi", base, capability, role)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, capability, role, explicitPath, defaults, payload)
	var bestErr error
	for _, selection := range selections {
		response, err := session.request(ctx, client, diagnosticRequest{
			EndpointType: endpointType, AuthCapability: role, Method: http.MethodGet,
			URL: base + selection.Path, Credential: capabilityCredential(payload, selection.Auth), MaxBytes: 4 << 20,
		})
		if err == nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
			err = &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: endpointType, StatusCode: response.StatusCode}
		}
		if err == nil && validate != nil {
			err = validate(response.Body)
			if err != nil {
				session.markLastContract(endpointType, "unknown", "contract_changed", "response did not match the capability contract")
				if ErrorClass(err) == "unknown_error" {
					err = &DiagnosticError{Class: "contract_changed", Operation: endpointType, Cause: err}
				}
			}
		}
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		bestErr = preferredCapabilityError(bestErr, err)
		if terminalCapabilityAuthError(err) {
			break
		}
	}
	if bestErr == nil {
		bestErr = &DiagnosticError{Class: "contract_changed", Operation: endpointType}
	}
	return capabilitySelection{}, bestErr
}

func (c *Sub2API) negotiateEnvelope(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, capability, endpointType, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload) (capabilitySelection, error) {
	key := profileKey("sub2api", base, capability, role)
	selections := capabilitySelections(c.profiles, supplier, "sub2api", base, capability, role, explicitPath, defaults, payload)
	var best error
	for _, selection := range selections {
		err := probeSub2Envelope(ctx, session, client, base+selection.Path, endpointType, role,
			capabilityCredential(payload, selection.Auth))
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		best = preferredCapabilityError(best, err)
		if terminalSub2CapabilityAuthError(err, role) {
			break
		}
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: endpointType}
	}
	return capabilitySelection{}, best
}

func (c *Sub2API) negotiateModelCatalog(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, role, explicitPath string, defaults []string, payload domain.CredentialPayload) (capabilitySelection, error) {
	key := profileKey("sub2api", base, "catalog", role)
	selections := capabilitySelections(c.profiles, supplier, "sub2api", base, "catalog", role, explicitPath, defaults, payload)
	var best error
	for _, selection := range selections {
		err := probeModelCatalog(ctx, session, client, base+selection.Path, role, capabilityCredential(payload, selection.Auth))
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		best = preferredCapabilityError(best, err)
		if terminalSub2CapabilityAuthError(err, role) {
			break
		}
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: "catalog"}
	}
	return capabilitySelection{}, best
}

func (c *Sub2API) negotiateJSONCapability(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, capability, endpointType, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload) (capabilitySelection, error) {
	key := profileKey("sub2api", base, capability, role)
	selections := capabilitySelections(c.profiles, supplier, "sub2api", base, capability, role, explicitPath, defaults, payload)
	var best error
	for _, selection := range selections {
		err := probeJSONDocument(ctx, session, client, base+selection.Path, endpointType, role,
			capabilityCredential(payload, selection.Auth))
		if err == nil {
			c.profiles.set(key, selection)
			return selection, nil
		}
		best = preferredCapabilityError(best, err)
		if terminalSub2CapabilityAuthError(err, role) {
			break
		}
	}
	if best == nil {
		best = &DiagnosticError{Class: "contract_changed", Operation: endpointType}
	}
	return capabilitySelection{}, best
}

func (c *Sub2API) fetchNegotiatedDocument(ctx context.Context, session *diagnosticSession, client *http.Client,
	supplier domain.Supplier, base, capability, endpointType, role, explicitPath string, defaults []string,
	payload domain.CredentialPayload, maxBytes int64, validate capabilityDocumentValidator) (capabilitySelection, []byte, error) {
	key := profileKey("sub2api", base, capability, role)
	selections := capabilitySelections(c.profiles, supplier, "sub2api", base, capability, role, explicitPath, defaults, payload)
	var bestErr error
	for _, selection := range selections {
		response, err := session.request(ctx, client, diagnosticRequest{
			EndpointType: endpointType, AuthCapability: role, Method: http.MethodGet,
			URL: base + selection.Path, Credential: capabilityCredential(payload, selection.Auth), MaxBytes: maxBytes,
		})
		if err == nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
			err = &sub2HTTPError{Path: selection.Path, StatusCode: response.StatusCode, Status: response.Status}
		}
		if err == nil && validate != nil {
			err = validate(response.Body)
			if err != nil {
				session.markLastContract(endpointType, "unknown", "contract_changed", "response did not match the capability contract")
				if ErrorClass(err) == "unknown_error" {
					err = &DiagnosticError{Class: "contract_changed", Operation: endpointType, Cause: err}
				}
			}
		}
		if err == nil {
			c.profiles.set(key, selection)
			return selection, response.Body, nil
		}
		bestErr = preferredCapabilityError(bestErr, err)
		if terminalSub2CapabilityAuthError(err, role) {
			break
		}
	}
	if bestErr == nil {
		bestErr = &DiagnosticError{Class: "contract_changed", Operation: endpointType,
			Cause: fmt.Errorf("no safe candidate endpoint is configured")}
	}
	return capabilitySelection{}, nil, bestErr
}

// A 401/403 is evidence about the credential's authorization for this exact
// role, not an invitation to replay the same secret across every candidate
// path and header shape. Callers may still evaluate a different, explicitly
// separated capability (for example anonymous catalog then customer session).
func terminalCapabilityAuthError(err error) bool {
	switch ErrorClass(err) {
	case "auth_missing", "permission_denied":
		return true
	default:
		return false
	}
}

// Some Sub2API forks run authentication before route matching, so a removed
// legacy customer endpoint can return 401 even while the current endpoint
// accepts the same session. Customer sessions have one native auth shape and a
// bounded path list, making path fallback safe. Business keys and site-admin
// credentials retain fail-fast behavior to avoid replay across auth contracts.
func terminalSub2CapabilityAuthError(err error, role string) bool {
	return role != authCustomerSession && terminalCapabilityAuthError(err)
}

func preferredCapabilityError(current, candidate error) error {
	if current == nil || capabilityErrorRank(candidate) > capabilityErrorRank(current) {
		return candidate
	}
	return current
}

func capabilityErrorRank(err error) int {
	switch ErrorClass(err) {
	case "permission_denied":
		return 5
	case "auth_missing":
		return 4
	case "rate_limited", "upstream_5xx", "timeout", "connection_error", "tls_error", "dns_error":
		return 3
	case "contract_changed":
		return 2
	default:
		return 1
	}
}
