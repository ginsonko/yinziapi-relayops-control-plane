package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

type Connector interface {
	Name() string
	Collect(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error)
}

// DetailedConnector is optional so existing third-party connectors remain
// source compatible. Native connectors implement it to expose request-level
// evidence without putting secrets or response bodies into diagnostics.
type DetailedConnector interface {
	Connector
	CollectDetailed(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error)
}

func catalogResponseHash(groups []domain.GroupOffer, models []domain.ModelOffer) string {
	groupCopy := append([]domain.GroupOffer(nil), groups...)
	modelCopy := append([]domain.ModelOffer(nil), models...)
	for index := range groupCopy {
		groupCopy[index].ModelIDs = append([]string(nil), groupCopy[index].ModelIDs...)
		sort.Strings(groupCopy[index].ModelIDs)
	}
	sort.Slice(groupCopy, func(i, j int) bool {
		if groupCopy[i].Name == groupCopy[j].Name {
			return groupCopy[i].ExternalID < groupCopy[j].ExternalID
		}
		return groupCopy[i].Name < groupCopy[j].Name
	})
	sort.Slice(modelCopy, func(i, j int) bool {
		if modelCopy[i].Group == modelCopy[j].Group {
			if modelCopy[i].Name == modelCopy[j].Name {
				return modelCopy[i].ExternalID < modelCopy[j].ExternalID
			}
			return modelCopy[i].Name < modelCopy[j].Name
		}
		return modelCopy[i].Group < modelCopy[j].Group
	})
	payload, _ := json.Marshal(struct {
		Groups []domain.GroupOffer `json:"groups"`
		Models []domain.ModelOffer `json:"models"`
	}{Groups: groupCopy, Models: modelCopy})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

type Factory struct {
	client      *http.Client
	credentials *credentials.Service
	specs       ConnectorSpecLoader
	manifests   ConnectorManifestLoader
	profiles    *capabilityProfileCache
}

type ConnectorSpecLoader interface {
	ListConnectorSpecs(context.Context) ([]domain.ConnectorSpec, error)
}

type ConnectorManifestLoader interface {
	BoundConnectorManifest(context.Context, int64) (domain.ConnectorManifestRecord, bool, error)
}

func NewFactory(client *http.Client, credentialService *credentials.Service) *Factory {
	return &Factory{client: client, credentials: credentialService, profiles: newCapabilityProfileCache()}
}

func (f *Factory) SetConnectorSpecLoader(loader ConnectorSpecLoader) {
	f.specs = loader
}

func (f *Factory) SetConnectorManifestLoader(loader ConnectorManifestLoader) {
	f.manifests = loader
}

func (f *Factory) ForSupplier(ctx context.Context, supplier domain.Supplier) (Connector, string, error) {
	resolvedSupplier, manifestBound, err := f.resolveConnectorManifest(ctx, supplier)
	if err != nil {
		return nil, "", err
	}
	supplier = resolvedSupplier
	adapter := strings.ToLower(strings.TrimSpace(supplier.Adapter))
	if adapter == "" || adapter == "auto" {
		detected, err := f.detect(ctx, supplier)
		if err != nil {
			return nil, "", err
		}
		adapter = detected
	}
	switch adapter {
	case "newapi", "newapi-native":
		connector := NewNewAPI(f.client, f.credentials)
		connector.profiles = f.profiles
		return bindManifestConnector(connector, supplier, manifestBound), adapter, nil
	case "sub2api", "sub2api_admin", "sub2api_upstream":
		connector := NewSub2API(f.client, f.credentials)
		connector.profiles = f.profiles
		return bindManifestConnector(connector, supplier, manifestBound), adapter, nil
	case "json", "jsonspec", "json_connector":
		raw, err := f.connectorSpec(ctx, supplier)
		if err != nil {
			return nil, adapter, err
		}
		spec, err := ParseJSONConnectorSpec([]byte(raw))
		if err != nil {
			return nil, adapter, err
		}
		connector, err := NewJSONSpec(f.client, f.credentials, spec)
		return bindManifestConnector(connector, supplier, manifestBound), adapter, err
	default:
		return nil, adapter, fmt.Errorf("adapter %q is not available", adapter)
	}
}

func (f *Factory) resolveConnectorManifest(ctx context.Context, supplier domain.Supplier) (domain.Supplier, bool, error) {
	if f.manifests == nil || supplier.ID <= 0 {
		return supplier, false, nil
	}
	record, found, err := f.manifests.BoundConnectorManifest(ctx, supplier.ID)
	if err != nil {
		return supplier, false, fmt.Errorf("load connector manifest: %w", err)
	}
	if !found {
		return supplier, false, nil
	}
	if record.ValidationStatus != "valid" {
		return supplier, false, fmt.Errorf("connector manifest %d is not valid", record.ID)
	}
	if !ConnectorManifestMatchesOrigin(record.Manifest, supplier.BaseURL) {
		return supplier, false, fmt.Errorf("connector manifest %d no longer matches supplier origin", record.ID)
	}
	adapter := canonicalManifestAdapter(supplier.Adapter)
	if adapter == "" || adapter == "auto" {
		supplier.Adapter = record.AdapterFamily
	} else if adapter != record.AdapterFamily {
		return supplier, false, fmt.Errorf("connector manifest adapter %q does not match supplier adapter %q", record.AdapterFamily, supplier.Adapter)
	}
	metadata := make(map[string]string, len(supplier.Metadata)+16)
	for key, value := range supplier.Metadata {
		metadata[key] = value
	}
	for key, value := range ConnectorManifestMetadata(record.Manifest) {
		metadata[key] = value
	}
	metadata["connector_manifest_id"] = strconv.FormatInt(record.ID, 10)
	supplier.Metadata = metadata
	return supplier, true, nil
}

type manifestBoundConnector struct {
	delegate Connector
	supplier domain.Supplier
}

func bindManifestConnector(delegate Connector, supplier domain.Supplier, bound bool) Connector {
	if delegate == nil || !bound {
		return delegate
	}
	return &manifestBoundConnector{delegate: delegate, supplier: supplier}
}

func (c *manifestBoundConnector) Name() string { return c.delegate.Name() }

func (c *manifestBoundConnector) Collect(ctx context.Context, _ domain.Supplier) (domain.CatalogSnapshot, error) {
	return c.delegate.Collect(ctx, c.supplier)
}

func (c *manifestBoundConnector) CollectDetailed(ctx context.Context, _ domain.Supplier) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error) {
	if detailed, ok := c.delegate.(DetailedConnector); ok {
		return detailed.CollectDetailed(ctx, c.supplier)
	}
	snapshot, err := c.delegate.Collect(ctx, c.supplier)
	return snapshot, domain.CollectionDiagnostics{}, err
}

func (c *manifestBoundConnector) ValidateCredential(ctx context.Context, _ domain.Supplier, kind string, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	validator, ok := c.delegate.(CredentialValidator)
	if !ok {
		return domain.CollectionDiagnostics{}, &CredentialValidationError{Class: "contract_changed", Message: "bound connector does not support credential validation"}
	}
	return validator.ValidateCredential(ctx, c.supplier, kind, payload)
}

func (f *Factory) connectorSpec(ctx context.Context, supplier domain.Supplier) (string, error) {
	specIDText := strings.TrimSpace(supplier.Metadata["connector_spec_id"])
	if specIDText == "" {
		raw := strings.TrimSpace(supplier.Metadata["connector_spec"])
		if raw == "" {
			return "", fmt.Errorf("JSON connector supplier requires metadata.connector_spec_id")
		}
		return raw, nil
	}
	specID, err := strconv.ParseInt(specIDText, 10, 64)
	if err != nil || specID <= 0 {
		return "", fmt.Errorf("invalid metadata.connector_spec_id %q", specIDText)
	}
	if f.specs == nil {
		return "", fmt.Errorf("JSON connector spec loader is not configured")
	}
	items, err := f.specs.ListConnectorSpecs(ctx)
	if err != nil {
		return "", fmt.Errorf("load JSON connector specs: %w", err)
	}
	for _, item := range items {
		if item.ID != specID {
			continue
		}
		if !item.Enabled {
			return "", fmt.Errorf("JSON connector spec %d is disabled", specID)
		}
		return item.SpecJSON, nil
	}
	return "", fmt.Errorf("JSON connector spec %d was not found", specID)
}

func (f *Factory) detect(ctx context.Context, supplier domain.Supplier) (string, error) {
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return "", err
	}
	client, err := netguard.SameOriginClient(f.client, base)
	if err != nil {
		return "", err
	}
	requestCtx, cancel := requestDeadline(ctx, 12*time.Second)
	defer cancel()
	if selection, capability, ok := detectNewAPI(requestCtx, client, supplier, base); ok {
		f.profiles.set(profileKey("newapi", base, capability, authAnonymous), selection)
		return "newapi", nil
	}
	if selection, capability, ok := detectSub2API(requestCtx, client, supplier, base); ok {
		f.profiles.set(profileKey("sub2api", base, capability, authAnonymous), selection)
		return "sub2api", nil
	}
	return "", fmt.Errorf("unable to identify upstream type at %s", base)
}

func detectNewAPI(ctx context.Context, client *http.Client, supplier domain.Supplier, base string) (capabilitySelection, string, bool) {
	statusPaths, _ := capabilityPaths(supplier, "newapi", "status", metadataValue(supplier, "newapi_status_path", ""),
		[]string{"/api/status", "/api/setting"})
	for _, path := range statusPaths {
		statusCode, body, ok := detectGET(ctx, client, base+path, 1<<20)
		if !ok || statusCode != http.StatusOK {
			continue
		}
		var status struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &status) == nil && status.Success && len(status.Data) > 2 {
			return capabilitySelection{Path: path, Auth: capabilityAuthProfile{Name: "native", Native: true}}, "status", true
		}
	}
	pricingPaths, _ := capabilityPaths(supplier, "newapi", "pricing", supplier.PricingPath, []string{"/api/pricing"})
	for _, path := range pricingPaths {
		statusCode, body, ok := detectGET(ctx, client, base+path, 4<<20)
		if !ok || statusCode != http.StatusOK {
			continue
		}
		var pricing newAPIPricingResponse
		if json.Unmarshal(body, &pricing) == nil && pricing.Success && len(pricing.Data) > 0 {
			return capabilitySelection{Path: path, Auth: capabilityAuthProfile{Name: "native", Native: true}}, "pricing", true
		}
	}
	return capabilitySelection{}, "", false
}

func detectSub2API(ctx context.Context, client *http.Client, supplier domain.Supplier, base string) (capabilitySelection, string, bool) {
	healthPaths, _ := capabilityPaths(supplier, "sub2api", "health", metadataValue(supplier, "sub2api_health_path", ""),
		[]string{"/health", "/api/health"})
	for _, path := range healthPaths {
		statusCode, body, ok := detectGET(ctx, client, base+path, 64<<10)
		if !ok || statusCode != http.StatusOK {
			continue
		}
		var health struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &health) == nil && strings.EqualFold(strings.TrimSpace(health.Status), "ok") {
			return capabilitySelection{Path: path, Auth: capabilityAuthProfile{Name: "native", Native: true}}, "health", true
		}
	}
	ratePaths, _ := capabilityPaths(supplier, "sub2api", "customer_rates", "", []string{"/api/v1/groups/rates"})
	for _, path := range ratePaths {
		statusCode, _, ok := detectGET(ctx, client, base+path, 1<<20)
		if ok && (statusCode == http.StatusOK || statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden) {
			return capabilitySelection{Path: path, Auth: capabilityAuthProfile{Name: "native", Native: true}}, "customer_rates", true
		}
	}
	return capabilitySelection{}, "", false
}

func detectGET(ctx context.Context, client *http.Client, requestURL string, maxBytes int64) (int, []byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, nil, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return resp.StatusCode, nil, false
	}
	return resp.StatusCode, body, true
}

func applyCredential(req *http.Request, payload domain.CredentialPayload) {
	netguard.ApplyControlPlaneUserAgent(req)
	if payload.Authorization != "" {
		req.Header.Set("Authorization", payload.Authorization)
	}
	if payload.Cookie != "" {
		req.Header.Set("Cookie", payload.Cookie)
	}
	if payload.UserID > 0 {
		req.Header.Set("New-Api-User", fmt.Sprintf("%d", payload.UserID))
	}
	for key, value := range payload.Headers {
		if strings.TrimSpace(key) != "" {
			req.Header.Set(key, value)
		}
	}
}

func normalizedBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid supplier URL %q", raw)
	}
	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func requestDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}
