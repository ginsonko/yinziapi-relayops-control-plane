package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const (
	JSONSpecSchemaVersion       = 1
	defaultJSONSpecTimeout      = 25 * time.Second
	maximumJSONSpecTimeout      = 2 * time.Minute
	defaultJSONSpecResponseSize = int64(2 << 20)
	maximumJSONSpecResponseSize = int64(16 << 20)
	maximumJSONSpecBodySize     = 64 << 10
	maximumJSONSpecPages        = 50
	maximumJSONSpecEndpoints    = 16
	maximumJSONSpecHeaders      = 32
	maximumJSONSpecPathLength   = 512
	maximumJSONSpecPathSteps    = 64
)

// JSONSpecCredentialLoader is deliberately narrower than credentials.Service. It
// also makes a JSON connector testable without exposing decrypted credentials.
type JSONSpecCredentialLoader interface {
	Load(context.Context, int64) (domain.Credential, domain.CredentialPayload, error)
}

// JSONConnectorSpec is the complete, declarative import format. Unknown fields
// are rejected by ParseJSONConnectorSpec. No field is evaluated as code or as a
// text template.
type JSONConnectorSpec struct {
	SchemaVersion int                 `json:"schema_version"`
	Name          string              `json:"name"`
	Limits        JSONConnectorLimits `json:"limits,omitempty"`
	Endpoints     []JSONEndpointSpec  `json:"endpoints"`
}

type JSONConnectorLimits struct {
	TimeoutSeconds   int   `json:"timeout_seconds,omitempty"`
	MaxResponseBytes int64 `json:"max_response_bytes,omitempty"`
}

type JSONEndpointSpec struct {
	Name       string                    `json:"name"`
	Method     string                    `json:"method"`
	Path       string                    `json:"path"`
	Headers    map[string]JSONHeaderSpec `json:"headers,omitempty"`
	Body       json.RawMessage           `json:"body,omitempty"`
	Pagination *JSONPaginationSpec       `json:"pagination,omitempty"`
	Groups     *JSONGroupMapping         `json:"groups,omitempty"`
	Models     *JSONModelMapping         `json:"models,omitempty"`
	Balance    *JSONBalanceMapping       `json:"balance,omitempty"`
}

// JSONHeaderSpec permits either a public static value or one explicit reference
// into an encrypted CredentialPayload. Secrets are never interpolated into URLs
// or request bodies.
type JSONHeaderSpec struct {
	Static        string `json:"static,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

// JSONPaginationSpec supports bounded page-number and cursor pagination. The
// pagination value is only ever placed in a query parameter.
type JSONPaginationSpec struct {
	Mode              string `json:"mode"`
	Parameter         string `json:"parameter"`
	StartPage         int    `json:"start_page,omitempty"`
	MaxPages          int    `json:"max_pages"`
	NextCursorPath    string `json:"next_cursor_path,omitempty"`
	StopWhenPathEmpty string `json:"stop_when_path_empty,omitempty"`
}

// JSONFieldSpec selects either a value relative to the current item or a fixed
// literal. Exactly one is allowed when a field is configured.
type JSONFieldSpec struct {
	Path     string `json:"path,omitempty"`
	Constant string `json:"constant,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type JSONGroupMapping struct {
	ItemsPath     string            `json:"items_path"`
	ObjectEntries bool              `json:"object_entries,omitempty"`
	ExternalID    JSONFieldSpec     `json:"external_id"`
	Name          JSONFieldSpec     `json:"name"`
	Ratio         JSONFieldSpec     `json:"ratio,omitempty"`
	ModelIDs      JSONFieldSpec     `json:"model_ids,omitempty"`
	MetadataPaths map[string]string `json:"metadata_paths,omitempty"`
}

type JSONModelMapping struct {
	ItemsPath       string                   `json:"items_path"`
	ObjectEntries   bool                     `json:"object_entries,omitempty"`
	ExternalID      JSONFieldSpec            `json:"external_id"`
	Name            JSONFieldSpec            `json:"name"`
	Group           JSONFieldSpec            `json:"group,omitempty"`
	BillingMode     JSONFieldSpec            `json:"billing_mode,omitempty"`
	ModelRatio      JSONFieldSpec            `json:"model_ratio,omitempty"`
	CompletionRatio JSONFieldSpec            `json:"completion_ratio,omitempty"`
	CacheRatio      JSONFieldSpec            `json:"cache_ratio,omitempty"`
	ModelPrice      JSONFieldSpec            `json:"model_price,omitempty"`
	BillingExpr     JSONFieldSpec            `json:"billing_expr,omitempty"`
	Currency        JSONFieldSpec            `json:"currency,omitempty"`
	PriceFunction   JSONPriceFunctionMapping `json:"price_function,omitempty"`
	MetadataPaths   map[string]string        `json:"metadata_paths,omitempty"`
}

type JSONPriceFunctionMapping struct {
	Currency               JSONFieldSpec     `json:"currency,omitempty"`
	Basis                  JSONFieldSpec     `json:"basis,omitempty"`
	InputPer1M             JSONFieldSpec     `json:"input_per_1m,omitempty"`
	OutputPer1M            JSONFieldSpec     `json:"output_per_1m,omitempty"`
	CacheReadPer1M         JSONFieldSpec     `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M        JSONFieldSpec     `json:"cache_write_per_1m,omitempty"`
	CacheWrite5MPer1M      JSONFieldSpec     `json:"cache_write_5m_per_1m,omitempty"`
	CacheWrite1HPer1M      JSONFieldSpec     `json:"cache_write_1h_per_1m,omitempty"`
	ImageInputPer1M        JSONFieldSpec     `json:"image_input_per_1m,omitempty"`
	AudioInputPer1M        JSONFieldSpec     `json:"audio_input_per_1m,omitempty"`
	AudioOutputPer1M       JSONFieldSpec     `json:"audio_output_per_1m,omitempty"`
	PerRequest             JSONFieldSpec     `json:"per_request,omitempty"`
	PerImage               JSONFieldSpec     `json:"per_image,omitempty"`
	PerSecond              JSONFieldSpec     `json:"per_second,omitempty"`
	PerFrame               JSONFieldSpec     `json:"per_frame,omitempty"`
	FixedFee               JSONFieldSpec     `json:"fixed_fee,omitempty"`
	GroupMultiplier        JSONFieldSpec     `json:"group_multiplier,omitempty"`
	Expression             JSONFieldSpec     `json:"expression,omitempty"`
	Derivation             JSONFieldSpec     `json:"derivation,omitempty"`
	RequiresModelBasePrice JSONFieldSpec     `json:"requires_model_base_price,omitempty"`
	Dimensions             map[string]string `json:"dimensions,omitempty"`
}

type JSONBalanceMapping struct {
	RootPath    string        `json:"root_path,omitempty"`
	Amount      JSONFieldSpec `json:"amount"`
	Currency    JSONFieldSpec `json:"currency,omitempty"`
	Available   JSONFieldSpec `json:"available,omitempty"`
	DailySpend  JSONFieldSpec `json:"daily_spend,omitempty"`
	RunwayHours JSONFieldSpec `json:"runway_hours,omitempty"`
	ExpiresAt   JSONFieldSpec `json:"expires_at,omitempty"`
	DisplayText JSONFieldSpec `json:"display_text,omitempty"`
}

// JSONCollectionResult keeps the normalized price observations alongside the
// catalog. BalanceState is embedded in Catalog because the current domain model
// intentionally has no separate BalanceObservation type.
type JSONCollectionResult struct {
	Catalog domain.CatalogSnapshot    `json:"catalog"`
	Prices  []domain.PriceObservation `json:"prices"`
	Balance *domain.BalanceState      `json:"balance,omitempty"`
}

type JSONSpecConnector struct {
	client      *http.Client
	credentials JSONSpecCredentialLoader
	spec        JSONConnectorSpec
}

// ParseJSONConnectorSpec performs strict schema decoding and semantic
// validation. This is the intended entry point for rules generated by an LLM.
func ParseJSONConnectorSpec(data []byte) (JSONConnectorSpec, error) {
	if len(data) == 0 {
		return JSONConnectorSpec{}, errors.New("JSON connector rule is empty")
	}
	if len(data) > 1<<20 {
		return JSONConnectorSpec{}, errors.New("JSON connector rule exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var spec JSONConnectorSpec
	if err := decoder.Decode(&spec); err != nil {
		return JSONConnectorSpec{}, fmt.Errorf("decode JSON connector rule: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return JSONConnectorSpec{}, errors.New("JSON connector rule contains multiple documents")
		}
		return JSONConnectorSpec{}, fmt.Errorf("decode trailing JSON connector content: %w", err)
	}
	if err := ValidateJSONConnectorSpec(spec); err != nil {
		return JSONConnectorSpec{}, err
	}
	return spec, nil
}

func ValidateJSONConnectorSpec(spec JSONConnectorSpec) error {
	if spec.SchemaVersion != JSONSpecSchemaVersion {
		return fmt.Errorf("schema_version must be %d", JSONSpecSchemaVersion)
	}
	if strings.TrimSpace(spec.Name) == "" || len(spec.Name) > 120 {
		return errors.New("name must contain 1 to 120 characters")
	}
	if spec.Limits.TimeoutSeconds < 0 || time.Duration(spec.Limits.TimeoutSeconds)*time.Second > maximumJSONSpecTimeout {
		return fmt.Errorf("limits.timeout_seconds must be between 0 and %d", int(maximumJSONSpecTimeout/time.Second))
	}
	if spec.Limits.MaxResponseBytes < 0 || spec.Limits.MaxResponseBytes > maximumJSONSpecResponseSize {
		return fmt.Errorf("limits.max_response_bytes must be between 0 and %d", maximumJSONSpecResponseSize)
	}
	if len(spec.Endpoints) == 0 || len(spec.Endpoints) > maximumJSONSpecEndpoints {
		return fmt.Errorf("endpoints must contain 1 to %d entries", maximumJSONSpecEndpoints)
	}
	seenNames := make(map[string]struct{}, len(spec.Endpoints))
	for index, endpoint := range spec.Endpoints {
		prefix := fmt.Sprintf("endpoints[%d]", index)
		name := strings.TrimSpace(endpoint.Name)
		if name == "" || len(name) > 80 {
			return fmt.Errorf("%s.name must contain 1 to 80 characters", prefix)
		}
		if _, exists := seenNames[name]; exists {
			return fmt.Errorf("%s.name %q is duplicated", prefix, name)
		}
		seenNames[name] = struct{}{}
		method := strings.ToUpper(strings.TrimSpace(endpoint.Method))
		if method != http.MethodGet && method != http.MethodPost {
			return fmt.Errorf("%s.method must be GET or POST", prefix)
		}
		if err := validateRelativeEndpointPath(endpoint.Path); err != nil {
			return fmt.Errorf("%s.path: %w", prefix, err)
		}
		if len(endpoint.Headers) > maximumJSONSpecHeaders {
			return fmt.Errorf("%s.headers exceeds %d entries", prefix, maximumJSONSpecHeaders)
		}
		for header, source := range endpoint.Headers {
			if err := validateHeaderSource(header, source); err != nil {
				return fmt.Errorf("%s.headers[%q]: %w", prefix, header, err)
			}
		}
		if len(endpoint.Body) > 0 {
			if method != http.MethodPost {
				return fmt.Errorf("%s.body is only allowed for POST", prefix)
			}
			if len(endpoint.Body) > maximumJSONSpecBodySize || !json.Valid(endpoint.Body) {
				return fmt.Errorf("%s.body must be valid JSON no larger than %d bytes", prefix, maximumJSONSpecBodySize)
			}
		}
		if endpoint.Groups == nil && endpoint.Models == nil && endpoint.Balance == nil {
			return fmt.Errorf("%s must define groups, models, or balance extraction", prefix)
		}
		if endpoint.Pagination != nil {
			if err := validatePagination(*endpoint.Pagination); err != nil {
				return fmt.Errorf("%s.pagination: %w", prefix, err)
			}
		}
		if endpoint.Groups != nil {
			if err := validateGroupMapping(*endpoint.Groups); err != nil {
				return fmt.Errorf("%s.groups: %w", prefix, err)
			}
		}
		if endpoint.Models != nil {
			if err := validateModelMapping(*endpoint.Models); err != nil {
				return fmt.Errorf("%s.models: %w", prefix, err)
			}
		}
		if endpoint.Balance != nil {
			if err := validateBalanceMapping(*endpoint.Balance); err != nil {
				return fmt.Errorf("%s.balance: %w", prefix, err)
			}
		}
	}
	return nil
}

func NewJSONSpec(client *http.Client, credentialLoader JSONSpecCredentialLoader, spec JSONConnectorSpec) (*JSONSpecConnector, error) {
	if err := ValidateJSONConnectorSpec(spec); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &JSONSpecConnector{client: client, credentials: credentialLoader, spec: spec}, nil
}

func (c *JSONSpecConnector) Name() string { return "jsonspec" }

func (c *JSONSpecConnector) Collect(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error) {
	result, err := c.CollectObservations(ctx, supplier)
	return result.Catalog, err
}

func (c *JSONSpecConnector) CollectObservations(ctx context.Context, supplier domain.Supplier) (JSONCollectionResult, error) {
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return JSONCollectionResult{}, err
	}
	baseURL, _ := url.Parse(base)
	timeout := defaultJSONSpecTimeout
	if c.spec.Limits.TimeoutSeconds > 0 {
		timeout = time.Duration(c.spec.Limits.TimeoutSeconds) * time.Second
	}
	collectionCtx, cancel := requestDeadline(ctx, timeout)
	defer cancel()

	responseLimit := defaultJSONSpecResponseSize
	if c.spec.Limits.MaxResponseBytes > 0 {
		responseLimit = c.spec.Limits.MaxResponseBytes
	}
	payload, authenticated, err := c.loadCredential(collectionCtx, supplier)
	if err != nil {
		return JSONCollectionResult{}, err
	}
	client := sameOriginClient(c.client, baseURL)
	groups := make([]domain.GroupOffer, 0)
	models := make([]domain.ModelOffer, 0)
	var balance *domain.BalanceState
	sourceURLs := make([]string, 0, len(c.spec.Endpoints))
	warnings := make([]string, 0)
	now := time.Now().UTC()

	for _, endpoint := range c.spec.Endpoints {
		endpointResult, collectErr := c.collectEndpoint(collectionCtx, client, baseURL, endpoint, payload, responseLimit, now)
		if collectErr != nil {
			return JSONCollectionResult{}, fmt.Errorf("collect endpoint %q: %w", endpoint.Name, collectErr)
		}
		groups = append(groups, endpointResult.groups...)
		models = append(models, endpointResult.models...)
		if endpointResult.balance != nil {
			balance = endpointResult.balance
		}
		sourceURLs = append(sourceURLs, endpointResult.sourceURL)
		warnings = append(warnings, endpointResult.warnings...)
	}
	groups = mergeJSONGroups(groups)
	models = mergeJSONModels(models)
	if len(groups) == 0 && len(models) == 0 && balance == nil {
		return JSONCollectionResult{}, errors.New("JSON connector extracted no groups, models, or balance")
	}
	confidence := 70
	sourceType := "declared_json_api"
	if authenticated {
		confidence = 80
		sourceType = "authenticated_declared_json_api"
	}
	evidence := domain.Evidence{
		SourceType:   sourceType,
		SourceURL:    strings.Join(uniqueSorted(sourceURLs), ","),
		Adapter:      c.Name(),
		Confidence:   confidence,
		ObservedAt:   now,
		ResponseHash: catalogResponseHash(groups, models),
		Warnings:     uniqueSorted(warnings),
	}
	catalog := domain.CatalogSnapshot{SupplierID: supplier.ID, Groups: groups, Models: models, Balance: balance, Evidence: evidence}
	prices := makeJSONPriceObservations(supplier.ID, groups, models, evidence)
	return JSONCollectionResult{Catalog: catalog, Prices: prices, Balance: balance}, nil
}

type jsonEndpointResult struct {
	groups    []domain.GroupOffer
	models    []domain.ModelOffer
	balance   *domain.BalanceState
	sourceURL string
	warnings  []string
}

func (c *JSONSpecConnector) collectEndpoint(
	ctx context.Context,
	client *http.Client,
	baseURL *url.URL,
	endpoint JSONEndpointSpec,
	payload domain.CredentialPayload,
	responseLimit int64,
	now time.Time,
) (jsonEndpointResult, error) {
	result := jsonEndpointResult{}
	page := 0
	maxPages := 1
	pageNumber := 1
	cursor := ""
	seenCursors := make(map[string]struct{})
	if endpoint.Pagination != nil {
		maxPages = endpoint.Pagination.MaxPages
		pageNumber = endpoint.Pagination.StartPage
		if pageNumber == 0 {
			pageNumber = 1
		}
	}
	for page < maxPages {
		requestURL, err := resolveEndpointURL(baseURL, endpoint.Path)
		if err != nil {
			return result, err
		}
		if endpoint.Pagination != nil {
			query := requestURL.Query()
			switch endpoint.Pagination.Mode {
			case "page":
				query.Set(endpoint.Pagination.Parameter, strconv.Itoa(pageNumber))
			case "cursor":
				if page > 0 {
					query.Set(endpoint.Pagination.Parameter, cursor)
				}
			}
			requestURL.RawQuery = query.Encode()
		}
		var body io.Reader
		if len(endpoint.Body) > 0 {
			body = bytes.NewReader(endpoint.Body)
		}
		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(endpoint.Method), requestURL.String(), body)
		if err != nil {
			return result, err
		}
		req.Header.Set("Accept", "application/json")
		if len(endpoint.Body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		for header, source := range endpoint.Headers {
			value := source.Static
			if source.CredentialRef != "" {
				value, err = credentialReference(payload, source.CredentialRef)
				if err != nil {
					return result, fmt.Errorf("resolve header %q: %w", header, err)
				}
			}
			req.Header.Set(header, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return result, err
		}
		responseBody, readErr := readBoundedResponse(resp, responseLimit)
		if readErr != nil {
			return result, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return result, fmt.Errorf("endpoint returned %s", resp.Status)
		}
		var root any
		decoder := json.NewDecoder(bytes.NewReader(responseBody))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return result, fmt.Errorf("decode JSON response: %w", err)
		}
		before := len(result.groups) + len(result.models)
		if endpoint.Groups != nil {
			extracted, extractErr := extractJSONGroups(root, *endpoint.Groups)
			if extractErr != nil {
				return result, extractErr
			}
			result.groups = append(result.groups, extracted...)
		}
		if endpoint.Models != nil {
			extracted, extractErr := extractJSONModels(root, *endpoint.Models)
			if extractErr != nil {
				return result, extractErr
			}
			result.models = append(result.models, extracted...)
		}
		if endpoint.Balance != nil {
			extracted, extractErr := extractJSONBalance(root, *endpoint.Balance, now)
			if extractErr != nil {
				return result, extractErr
			}
			if extracted != nil {
				result.balance = extracted
			}
		}
		result.sourceURL = requestURL.String()
		page++
		if endpoint.Pagination == nil {
			break
		}
		if endpoint.Pagination.StopWhenPathEmpty != "" {
			values, pathErr := evaluateJSONPath(root, endpoint.Pagination.StopWhenPathEmpty)
			if pathErr != nil {
				return result, pathErr
			}
			if len(flattenValues(values)) == 0 {
				break
			}
		} else if (endpoint.Groups != nil || endpoint.Models != nil) && len(result.groups)+len(result.models) == before {
			break
		}
		switch endpoint.Pagination.Mode {
		case "page":
			pageNumber++
		case "cursor":
			values, pathErr := evaluateJSONPath(root, endpoint.Pagination.NextCursorPath)
			if pathErr != nil {
				return result, pathErr
			}
			next, ok := firstScalar(values)
			if !ok || next == "" {
				return result, nil
			}
			if _, repeated := seenCursors[next]; repeated {
				return result, fmt.Errorf("pagination cursor %q repeated", next)
			}
			seenCursors[next] = struct{}{}
			cursor = next
		}
	}
	if endpoint.Pagination != nil && page == maxPages {
		result.warnings = append(result.warnings, fmt.Sprintf("endpoint %s reached pagination limit %d", endpoint.Name, maxPages))
	}
	return result, nil
}

func (c *JSONSpecConnector) loadCredential(ctx context.Context, supplier domain.Supplier) (domain.CredentialPayload, bool, error) {
	needsCredential := false
	for _, endpoint := range c.spec.Endpoints {
		for _, source := range endpoint.Headers {
			if source.CredentialRef != "" {
				needsCredential = true
				break
			}
		}
	}
	if !needsCredential {
		return domain.CredentialPayload{}, false, nil
	}
	if supplier.CredentialID == nil {
		return domain.CredentialPayload{}, false, errors.New("connector rule references credentials but supplier has no credential_id")
	}
	if c.credentials == nil {
		return domain.CredentialPayload{}, false, errors.New("connector rule references credentials but no credential loader is configured")
	}
	credential, payload, err := c.credentials.Load(ctx, *supplier.CredentialID)
	if err != nil {
		return domain.CredentialPayload{}, false, fmt.Errorf("load supplier credential: %w", err)
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, supplier.BaseURL); err != nil {
		return domain.CredentialPayload{}, false, fmt.Errorf("supplier credential origin mismatch: %w", err)
	}
	return payload, true, nil
}

func validateRelativeEndpointPath(raw string) error {
	if strings.TrimSpace(raw) == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") {
		return errors.New("must be a non-empty relative URL no longer than 2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || strings.HasPrefix(raw, "//") || parsed.Fragment != "" {
		return errors.New("must not contain a scheme, authority, credentials, or fragment")
	}
	return nil
}

func validateHeaderSource(header string, source JSONHeaderSpec) error {
	if !validHTTPToken(header) {
		return errors.New("invalid HTTP header name")
	}
	lower := strings.ToLower(header)
	switch lower {
	case "host", "content-length", "transfer-encoding", "connection", "proxy-connection", "upgrade", "trailer":
		return errors.New("hop-by-hop or transport header is not configurable")
	}
	configured := 0
	if source.Static != "" {
		configured++
	}
	if source.CredentialRef != "" {
		configured++
	}
	if configured != 1 {
		return errors.New("exactly one of static or credential_ref is required")
	}
	if strings.ContainsAny(source.Static, "\r\n") {
		return errors.New("static value contains a newline")
	}
	if source.Static != "" && sensitiveHeaderName(lower) {
		return errors.New("sensitive header values must use credential_ref")
	}
	if source.CredentialRef != "" && !validCredentialReference(source.CredentialRef) {
		return errors.New("unsupported credential_ref")
	}
	return nil
}

func sensitiveHeaderName(lower string) bool {
	switch lower {
	case "authorization", "cookie", "proxy-authorization", "api-key", "apikey", "x-api-key", "token", "x-token", "access-token", "x-access-token", "x-auth-token", "secret", "x-secret":
		return true
	}
	return strings.Contains(lower, "api-key") || strings.Contains(lower, "access-key") || strings.Contains(lower, "secret-key") || strings.HasSuffix(lower, "-token")
}

func validCredentialReference(reference string) bool {
	if reference == "authorization" || reference == "cookie" || reference == "user_id" {
		return true
	}
	for _, prefix := range []string{"headers.", "local_storage.", "session_storage."} {
		if strings.HasPrefix(reference, prefix) {
			key := strings.TrimPrefix(reference, prefix)
			return key != "" && len(key) <= 256 && !strings.ContainsAny(key, "\r\n")
		}
	}
	return false
}

func validatePagination(spec JSONPaginationSpec) error {
	if spec.Mode != "page" && spec.Mode != "cursor" {
		return errors.New("mode must be page or cursor")
	}
	if !validQueryParameter(spec.Parameter) {
		return errors.New("parameter is invalid")
	}
	if spec.MaxPages < 1 || spec.MaxPages > maximumJSONSpecPages {
		return fmt.Errorf("max_pages must be between 1 and %d", maximumJSONSpecPages)
	}
	if spec.StartPage < 0 {
		return errors.New("start_page must not be negative")
	}
	if spec.Mode == "cursor" {
		if spec.NextCursorPath == "" {
			return errors.New("next_cursor_path is required for cursor mode")
		}
		if err := validateJSONPath(spec.NextCursorPath); err != nil {
			return fmt.Errorf("next_cursor_path: %w", err)
		}
	}
	if spec.StopWhenPathEmpty != "" {
		if err := validateJSONPath(spec.StopWhenPathEmpty); err != nil {
			return fmt.Errorf("stop_when_path_empty: %w", err)
		}
	}
	return nil
}

func validateGroupMapping(mapping JSONGroupMapping) error {
	if err := validateJSONPath(mapping.ItemsPath); err != nil {
		return fmt.Errorf("items_path: %w", err)
	}
	if emptyJSONField(mapping.ExternalID) && emptyJSONField(mapping.Name) {
		return errors.New("external_id or name is required")
	}
	for name, field := range map[string]JSONFieldSpec{
		"external_id": mapping.ExternalID, "name": mapping.Name, "ratio": mapping.Ratio, "model_ids": mapping.ModelIDs,
	} {
		if err := validateJSONField(field); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return validateMetadataPaths(mapping.MetadataPaths)
}

func validateModelMapping(mapping JSONModelMapping) error {
	if err := validateJSONPath(mapping.ItemsPath); err != nil {
		return fmt.Errorf("items_path: %w", err)
	}
	if emptyJSONField(mapping.ExternalID) && emptyJSONField(mapping.Name) {
		return errors.New("external_id or name is required")
	}
	fields := map[string]JSONFieldSpec{
		"external_id": mapping.ExternalID, "name": mapping.Name, "group": mapping.Group,
		"billing_mode": mapping.BillingMode, "model_ratio": mapping.ModelRatio,
		"completion_ratio": mapping.CompletionRatio, "cache_ratio": mapping.CacheRatio,
		"model_price": mapping.ModelPrice, "billing_expr": mapping.BillingExpr, "currency": mapping.Currency,
	}
	for name, field := range fields {
		if err := validateJSONField(field); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := validatePriceFunctionMapping(mapping.PriceFunction); err != nil {
		return fmt.Errorf("price_function: %w", err)
	}
	return validateMetadataPaths(mapping.MetadataPaths)
}

func validatePriceFunctionMapping(mapping JSONPriceFunctionMapping) error {
	fields := map[string]JSONFieldSpec{
		"currency": mapping.Currency, "basis": mapping.Basis, "input_per_1m": mapping.InputPer1M,
		"output_per_1m": mapping.OutputPer1M, "cache_read_per_1m": mapping.CacheReadPer1M,
		"cache_write_per_1m": mapping.CacheWritePer1M, "cache_write_5m_per_1m": mapping.CacheWrite5MPer1M,
		"cache_write_1h_per_1m": mapping.CacheWrite1HPer1M, "image_input_per_1m": mapping.ImageInputPer1M,
		"audio_input_per_1m": mapping.AudioInputPer1M, "audio_output_per_1m": mapping.AudioOutputPer1M,
		"per_request": mapping.PerRequest, "per_image": mapping.PerImage, "per_second": mapping.PerSecond,
		"per_frame": mapping.PerFrame, "fixed_fee": mapping.FixedFee, "group_multiplier": mapping.GroupMultiplier,
		"expression": mapping.Expression, "derivation": mapping.Derivation,
		"requires_model_base_price": mapping.RequiresModelBasePrice,
	}
	for name, field := range fields {
		if err := validateJSONField(field); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return validateMetadataPaths(mapping.Dimensions)
}

func validateBalanceMapping(mapping JSONBalanceMapping) error {
	if mapping.RootPath != "" {
		if err := validateJSONPath(mapping.RootPath); err != nil {
			return fmt.Errorf("root_path: %w", err)
		}
	}
	if emptyJSONField(mapping.Amount) {
		return errors.New("amount is required")
	}
	fields := map[string]JSONFieldSpec{
		"amount": mapping.Amount, "currency": mapping.Currency, "available": mapping.Available,
		"daily_spend": mapping.DailySpend, "runway_hours": mapping.RunwayHours,
		"expires_at": mapping.ExpiresAt, "display_text": mapping.DisplayText,
	}
	for name, field := range fields {
		if err := validateJSONField(field); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func validateJSONField(field JSONFieldSpec) error {
	configured := 0
	if field.Path != "" {
		configured++
		if err := validateJSONPath(field.Path); err != nil {
			return err
		}
	}
	if field.Constant != "" {
		configured++
	}
	if configured > 1 {
		return errors.New("path and constant are mutually exclusive")
	}
	if field.Required && configured == 0 {
		return errors.New("required field has no path or constant")
	}
	return nil
}

func validateMetadataPaths(paths map[string]string) error {
	if len(paths) > 32 {
		return errors.New("metadata_paths exceeds 32 entries")
	}
	for name, path := range paths {
		if strings.TrimSpace(name) == "" || len(name) > 120 {
			return errors.New("metadata key must contain 1 to 120 characters")
		}
		if err := validateJSONPath(path); err != nil {
			return fmt.Errorf("metadata path %q: %w", name, err)
		}
	}
	return nil
}

func emptyJSONField(field JSONFieldSpec) bool { return field.Path == "" && field.Constant == "" }

func resolveEndpointURL(base *url.URL, rawPath string) (*url.URL, error) {
	if err := validateRelativeEndpointPath(rawPath); err != nil {
		return nil, err
	}
	reference, _ := url.Parse(rawPath)
	resolved := base.ResolveReference(reference)
	if !sameOrigin(base, resolved) {
		return nil, errors.New("endpoint URL escaped supplier base origin")
	}
	return resolved, nil
}

func sameOriginClient(source *http.Client, base *url.URL) *http.Client {
	clone := *source
	upstreamRedirect := source.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !sameOrigin(base, req.URL) {
			return errors.New("redirect escaped supplier base origin")
		}
		if upstreamRedirect != nil {
			return upstreamRedirect(req, via)
		}
		return nil
	}
	return &clone
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func readBoundedResponse(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("response body declares %d bytes, limit is %d", resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

func credentialReference(payload domain.CredentialPayload, reference string) (string, error) {
	var value string
	switch {
	case reference == "authorization":
		value = payload.Authorization
	case reference == "cookie":
		value = payload.Cookie
	case reference == "user_id":
		if payload.UserID != 0 {
			value = strconv.FormatInt(payload.UserID, 10)
		}
	case strings.HasPrefix(reference, "headers."):
		value = payload.Headers[strings.TrimPrefix(reference, "headers.")]
	case strings.HasPrefix(reference, "local_storage."):
		value = payload.LocalStorage[strings.TrimPrefix(reference, "local_storage.")]
	case strings.HasPrefix(reference, "session_storage."):
		value = payload.SessionStorage[strings.TrimPrefix(reference, "session_storage.")]
	default:
		return "", errors.New("unsupported credential reference")
	}
	if value == "" {
		return "", fmt.Errorf("credential reference %q resolved to an empty value", reference)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("credential reference %q contains a newline", reference)
	}
	return value, nil
}

func extractJSONGroups(root any, mapping JSONGroupMapping) ([]domain.GroupOffer, error) {
	items, err := jsonPathItemsMode(root, mapping.ItemsPath, mapping.ObjectEntries)
	if err != nil {
		return nil, err
	}
	result := make([]domain.GroupOffer, 0, len(items))
	for index, item := range items {
		externalID, idOK, err := extractScalarField(item, mapping.ExternalID)
		if err != nil {
			return nil, fmt.Errorf("group item %d external_id: %w", index, err)
		}
		name, nameOK, err := extractScalarField(item, mapping.Name)
		if err != nil {
			return nil, fmt.Errorf("group item %d name: %w", index, err)
		}
		if !idOK {
			externalID = name
		}
		if !nameOK {
			name = externalID
		}
		if externalID == "" || name == "" {
			continue
		}
		ratio, _, err := extractScalarField(item, mapping.Ratio)
		if err != nil {
			return nil, fmt.Errorf("group item %d ratio: %w", index, err)
		}
		modelIDs, err := extractStringListField(item, mapping.ModelIDs)
		if err != nil {
			return nil, fmt.Errorf("group item %d model_ids: %w", index, err)
		}
		metadata, err := extractMetadata(item, mapping.MetadataPaths)
		if err != nil {
			return nil, fmt.Errorf("group item %d metadata: %w", index, err)
		}
		result = append(result, domain.GroupOffer{ExternalID: externalID, Name: name, Ratio: ratio, ModelIDs: uniqueSorted(modelIDs), Metadata: metadata})
	}
	return result, nil
}

func extractJSONModels(root any, mapping JSONModelMapping) ([]domain.ModelOffer, error) {
	items, err := jsonPathItemsMode(root, mapping.ItemsPath, mapping.ObjectEntries)
	if err != nil {
		return nil, err
	}
	result := make([]domain.ModelOffer, 0, len(items))
	for index, item := range items {
		externalID, idOK, err := extractScalarField(item, mapping.ExternalID)
		if err != nil {
			return nil, fmt.Errorf("model item %d external_id: %w", index, err)
		}
		name, nameOK, err := extractScalarField(item, mapping.Name)
		if err != nil {
			return nil, fmt.Errorf("model item %d name: %w", index, err)
		}
		if !idOK {
			externalID = name
		}
		if !nameOK {
			name = externalID
		}
		if externalID == "" || name == "" {
			continue
		}
		offer := domain.ModelOffer{ExternalID: externalID, Name: name, BillingMode: "unknown"}
		fields := []struct {
			spec   JSONFieldSpec
			target *string
		}{
			{mapping.Group, &offer.Group}, {mapping.BillingMode, &offer.BillingMode},
			{mapping.ModelRatio, &offer.ModelRatio}, {mapping.CompletionRatio, &offer.CompletionRatio},
			{mapping.CacheRatio, &offer.CacheRatio}, {mapping.ModelPrice, &offer.ModelPrice},
			{mapping.BillingExpr, &offer.BillingExpr}, {mapping.Currency, &offer.Currency},
		}
		for _, field := range fields {
			value, ok, fieldErr := extractScalarField(item, field.spec)
			if fieldErr != nil {
				return nil, fmt.Errorf("model item %d: %w", index, fieldErr)
			}
			if ok {
				*field.target = value
			}
		}
		offer.Metadata, err = extractMetadata(item, mapping.MetadataPaths)
		if err != nil {
			return nil, fmt.Errorf("model item %d metadata: %w", index, err)
		}
		offer.PriceFunction, err = extractPriceFunction(item, mapping.PriceFunction, offer)
		if err != nil {
			return nil, fmt.Errorf("model item %d price_function: %w", index, err)
		}
		result = append(result, offer)
	}
	return result, nil
}

func extractPriceFunction(item any, mapping JSONPriceFunctionMapping, offer domain.ModelOffer) (domain.PriceFunction, error) {
	result := domain.PriceFunction{}
	fields := []struct {
		spec   JSONFieldSpec
		target *string
	}{
		{mapping.Currency, &result.Currency}, {mapping.Basis, &result.Basis},
		{mapping.InputPer1M, &result.InputPer1M}, {mapping.OutputPer1M, &result.OutputPer1M},
		{mapping.CacheReadPer1M, &result.CacheReadPer1M}, {mapping.CacheWritePer1M, &result.CacheWritePer1M},
		{mapping.CacheWrite5MPer1M, &result.CacheWrite5MPer1M}, {mapping.CacheWrite1HPer1M, &result.CacheWrite1HPer1M},
		{mapping.ImageInputPer1M, &result.ImageInputPer1M}, {mapping.AudioInputPer1M, &result.AudioInputPer1M},
		{mapping.AudioOutputPer1M, &result.AudioOutputPer1M}, {mapping.PerRequest, &result.PerRequest},
		{mapping.PerImage, &result.PerImage}, {mapping.PerSecond, &result.PerSecond},
		{mapping.PerFrame, &result.PerFrame}, {mapping.FixedFee, &result.FixedFee},
		{mapping.GroupMultiplier, &result.GroupMultiplier}, {mapping.Expression, &result.Expression},
		{mapping.Derivation, &result.Derivation},
	}
	for _, field := range fields {
		value, ok, err := extractScalarField(item, field.spec)
		if err != nil {
			return domain.PriceFunction{}, err
		}
		if ok {
			*field.target = value
		}
	}
	if value, ok, err := extractScalarField(item, mapping.RequiresModelBasePrice); err != nil {
		return domain.PriceFunction{}, err
	} else if ok {
		parsed, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			return domain.PriceFunction{}, errors.New("requires_model_base_price must be a boolean")
		}
		result.RequiresModelBasePrice = parsed
	}
	result.Dimensions, _ = extractMetadata(item, mapping.Dimensions)
	if result.Currency == "" {
		result.Currency = offer.Currency
	}
	if result.Expression == "" {
		result.Expression = offer.BillingExpr
	}
	if result.PerRequest == "" && offer.BillingMode == "per_call" {
		result.PerRequest = offer.ModelPrice
	}
	return result, nil
}

func extractJSONBalance(root any, mapping JSONBalanceMapping, observedAt time.Time) (*domain.BalanceState, error) {
	item := root
	if mapping.RootPath != "" {
		items, err := jsonPathItems(root, mapping.RootPath)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, nil
		}
		item = items[0]
	}
	amount, ok, err := extractScalarField(item, mapping.Amount)
	if err != nil {
		return nil, err
	}
	if !ok || amount == "" {
		return nil, nil
	}
	state := &domain.BalanceState{Amount: amount, Confidence: "declared", ObservedAt: observedAt, SourceField: mapping.Amount.Path}
	fields := []struct {
		spec   JSONFieldSpec
		target *string
	}{
		{mapping.Currency, &state.Currency}, {mapping.Available, &state.Available},
		{mapping.DailySpend, &state.DailySpend}, {mapping.DisplayText, &state.RawDisplayText},
	}
	for _, field := range fields {
		value, present, fieldErr := extractScalarField(item, field.spec)
		if fieldErr != nil {
			return nil, fieldErr
		}
		if present {
			*field.target = value
		}
	}
	if runway, present, fieldErr := extractScalarField(item, mapping.RunwayHours); fieldErr != nil {
		return nil, fieldErr
	} else if present {
		state.RunwayHours = &runway
	}
	if expires, present, fieldErr := extractScalarField(item, mapping.ExpiresAt); fieldErr != nil {
		return nil, fieldErr
	} else if present {
		parsed, parseErr := time.Parse(time.RFC3339, expires)
		if parseErr != nil {
			return nil, fmt.Errorf("balance expires_at must be RFC3339: %w", parseErr)
		}
		state.ExpiresAt = &parsed
	}
	return state, nil
}

func extractScalarField(item any, field JSONFieldSpec) (string, bool, error) {
	if field.Constant != "" {
		return field.Constant, true, nil
	}
	if field.Path == "" {
		return "", false, nil
	}
	values, err := evaluateJSONPath(item, field.Path)
	if err != nil {
		return "", false, err
	}
	value, ok := firstScalar(values)
	if !ok && field.Required {
		return "", false, fmt.Errorf("required path %q produced no scalar value", field.Path)
	}
	return value, ok, nil
}

func extractStringListField(item any, field JSONFieldSpec) ([]string, error) {
	if field.Constant != "" {
		return []string{field.Constant}, nil
	}
	if field.Path == "" {
		return nil, nil
	}
	values, err := evaluateJSONPath(item, field.Path)
	if err != nil {
		return nil, err
	}
	result := scalarStrings(flattenValues(values))
	if len(result) == 0 && field.Required {
		return nil, fmt.Errorf("required path %q produced no scalar values", field.Path)
	}
	return result, nil
}

func extractMetadata(item any, paths map[string]string) (map[string]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	result := make(map[string]string)
	keys := make([]string, 0, len(paths))
	for key := range paths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values, err := evaluateJSONPath(item, paths[key])
		if err != nil {
			return nil, err
		}
		if value, ok := firstScalar(values); ok {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func jsonPathItems(root any, path string) ([]any, error) {
	return jsonPathItemsMode(root, path, false)
}

func jsonPathItemsMode(root any, path string, objectEntries bool) ([]any, error) {
	values, err := evaluateJSONPath(root, path)
	if err != nil {
		return nil, err
	}
	if objectEntries {
		entries := make([]any, 0)
		for _, value := range values {
			object, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("path %q must resolve to JSON objects when object_entries is true", path)
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				entries = append(entries, map[string]any{"key": key, "value": object[key]})
			}
		}
		return entries, nil
	}
	return flattenValues(values), nil
}

type jsonPathStep struct {
	kind  byte
	key   string
	index int
}

func validateJSONPath(path string) error {
	_, err := parseJSONPath(path)
	return err
}

// parseJSONPath implements a non-executable subset: $, dotted properties,
// bracket-quoted properties, array indexes, and [*]. Filters, recursion,
// functions, unions, slices, and expressions are intentionally unsupported.
func parseJSONPath(raw string) ([]jsonPathStep, error) {
	path := strings.TrimSpace(raw)
	if path == "" || len(path) > maximumJSONSpecPathLength || !utf8.ValidString(path) {
		return nil, errors.New("JSON path must be valid UTF-8 with 1 to 512 bytes")
	}
	if path == "$" {
		return nil, nil
	}
	if strings.HasPrefix(path, "$") {
		path = path[1:]
	}
	steps := make([]jsonPathStep, 0, 8)
	for len(path) > 0 {
		if len(steps) >= maximumJSONSpecPathSteps {
			return nil, fmt.Errorf("JSON path exceeds %d steps", maximumJSONSpecPathSteps)
		}
		switch path[0] {
		case '.':
			path = path[1:]
			if path == "" || path[0] == '.' || path[0] == '[' {
				return nil, errors.New("empty dotted property")
			}
			end := strings.IndexAny(path, ".[")
			if end < 0 {
				end = len(path)
			}
			key := path[:end]
			if key == "*" || strings.ContainsAny(key, "@?(){}|:,") {
				return nil, fmt.Errorf("unsupported dotted property %q", key)
			}
			steps = append(steps, jsonPathStep{kind: 'p', key: key})
			path = path[end:]
		case '[':
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return nil, errors.New("unterminated bracket")
			}
			inside := strings.TrimSpace(path[1:end])
			switch {
			case inside == "*":
				steps = append(steps, jsonPathStep{kind: 'w'})
			case len(inside) >= 2 && inside[0] == '"' && inside[len(inside)-1] == '"':
				key, err := strconv.Unquote(inside)
				if err != nil || key == "" {
					return nil, errors.New("invalid quoted bracket property")
				}
				steps = append(steps, jsonPathStep{kind: 'p', key: key})
			case len(inside) >= 2 && inside[0] == '\'' && inside[len(inside)-1] == '\'':
				key := inside[1 : len(inside)-1]
				if key == "" || strings.Contains(key, "'") {
					return nil, errors.New("invalid single-quoted bracket property")
				}
				steps = append(steps, jsonPathStep{kind: 'p', key: key})
			default:
				index, err := strconv.Atoi(inside)
				if err != nil || index < 0 {
					return nil, fmt.Errorf("unsupported bracket selector %q", inside)
				}
				steps = append(steps, jsonPathStep{kind: 'i', index: index})
			}
			path = path[end+1:]
		default:
			end := strings.IndexAny(path, ".[")
			if end < 0 {
				end = len(path)
			}
			key := path[:end]
			if key == "" || key == "*" || strings.ContainsAny(key, "@?(){}|:,") {
				return nil, fmt.Errorf("unsupported property %q", key)
			}
			steps = append(steps, jsonPathStep{kind: 'p', key: key})
			path = path[end:]
		}
	}
	return steps, nil
}

func evaluateJSONPath(root any, path string) ([]any, error) {
	steps, err := parseJSONPath(path)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON path %q: %w", path, err)
	}
	current := []any{root}
	for _, step := range steps {
		next := make([]any, 0)
		for _, value := range current {
			switch step.kind {
			case 'p':
				if object, ok := value.(map[string]any); ok {
					if child, exists := object[step.key]; exists {
						next = append(next, child)
					}
				}
			case 'i':
				if array, ok := value.([]any); ok && step.index < len(array) {
					next = append(next, array[step.index])
				}
			case 'w':
				switch collection := value.(type) {
				case []any:
					next = append(next, collection...)
				case map[string]any:
					keys := make([]string, 0, len(collection))
					for key := range collection {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					for _, key := range keys {
						next = append(next, collection[key])
					}
				}
			}
		}
		current = next
		if len(current) == 0 {
			break
		}
	}
	return current, nil
}

func flattenValues(values []any) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		if array, ok := value.([]any); ok {
			result = append(result, array...)
		} else if value != nil {
			result = append(result, value)
		}
	}
	return result
}

func firstScalar(values []any) (string, bool) {
	for _, value := range flattenValues(values) {
		if scalar, ok := scalarValue(value); ok {
			return scalar, true
		}
	}
	return "", false
}

func scalarStrings(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if scalar, ok := scalarValue(value); ok {
			result = append(result, scalar)
		}
	}
	return result
}

func scalarValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

func mergeJSONGroups(groups []domain.GroupOffer) []domain.GroupOffer {
	byKey := make(map[string]domain.GroupOffer)
	for _, group := range groups {
		key := group.ExternalID
		if key == "" {
			key = group.Name
		}
		if existing, ok := byKey[key]; ok {
			if group.Name != "" {
				existing.Name = group.Name
			}
			if group.Ratio != "" {
				existing.Ratio = group.Ratio
			}
			existing.ModelIDs = uniqueSorted(append(existing.ModelIDs, group.ModelIDs...))
			existing.Metadata = mergeStringMaps(existing.Metadata, group.Metadata)
			byKey[key] = existing
		} else {
			group.ModelIDs = uniqueSorted(group.ModelIDs)
			byKey[key] = group
		}
	}
	result := make([]domain.GroupOffer, 0, len(byKey))
	for _, group := range byKey {
		result = append(result, group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ExternalID < result[j].ExternalID })
	return result
}

func mergeJSONModels(models []domain.ModelOffer) []domain.ModelOffer {
	byKey := make(map[string]domain.ModelOffer)
	for _, model := range models {
		key := model.ExternalID + "\x00" + model.Group
		if existing, ok := byKey[key]; ok {
			if model.Name != "" {
				existing.Name = model.Name
			}
			mergeModelOffer(&existing, model)
			byKey[key] = existing
		} else {
			byKey[key] = model
		}
	}
	result := make([]domain.ModelOffer, 0, len(byKey))
	for _, model := range byKey {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ExternalID == result[j].ExternalID {
			return result[i].Group < result[j].Group
		}
		return result[i].ExternalID < result[j].ExternalID
	})
	return result
}

func mergeModelOffer(target *domain.ModelOffer, source domain.ModelOffer) {
	if source.BillingMode != "" && source.BillingMode != "unknown" {
		target.BillingMode = source.BillingMode
	}
	fields := []struct {
		value       string
		destination *string
	}{
		{source.ModelRatio, &target.ModelRatio}, {source.CompletionRatio, &target.CompletionRatio},
		{source.CacheRatio, &target.CacheRatio}, {source.ModelPrice, &target.ModelPrice},
		{source.BillingExpr, &target.BillingExpr}, {source.Currency, &target.Currency},
	}
	for _, field := range fields {
		if field.value != "" {
			*field.destination = field.value
		}
	}
	target.Metadata = mergeStringMaps(target.Metadata, source.Metadata)
	mergePriceFunction(&target.PriceFunction, source.PriceFunction)
}

func mergePriceFunction(target *domain.PriceFunction, source domain.PriceFunction) {
	fields := []struct {
		value       string
		destination *string
	}{
		{source.Currency, &target.Currency}, {source.Basis, &target.Basis},
		{source.InputPer1M, &target.InputPer1M}, {source.OutputPer1M, &target.OutputPer1M},
		{source.CacheReadPer1M, &target.CacheReadPer1M}, {source.CacheWritePer1M, &target.CacheWritePer1M},
		{source.CacheWrite5MPer1M, &target.CacheWrite5MPer1M}, {source.CacheWrite1HPer1M, &target.CacheWrite1HPer1M},
		{source.ImageInputPer1M, &target.ImageInputPer1M}, {source.AudioInputPer1M, &target.AudioInputPer1M},
		{source.AudioOutputPer1M, &target.AudioOutputPer1M}, {source.PerRequest, &target.PerRequest},
		{source.PerImage, &target.PerImage}, {source.PerSecond, &target.PerSecond},
		{source.PerFrame, &target.PerFrame}, {source.FixedFee, &target.FixedFee},
		{source.GroupMultiplier, &target.GroupMultiplier}, {source.Expression, &target.Expression},
		{source.Derivation, &target.Derivation},
	}
	for _, field := range fields {
		if field.value != "" {
			*field.destination = field.value
		}
	}
	target.RequiresModelBasePrice = target.RequiresModelBasePrice || source.RequiresModelBasePrice
	target.Dimensions = mergeStringMaps(target.Dimensions, source.Dimensions)
}

func mergeStringMaps(left, right map[string]string) map[string]string {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	result := make(map[string]string, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func makeJSONPriceObservations(supplierID int64, groups []domain.GroupOffer, models []domain.ModelOffer, evidence domain.Evidence) []domain.PriceObservation {
	result := make([]domain.PriceObservation, 0, len(groups)+len(models))
	for _, group := range groups {
		if group.Ratio == "" {
			continue
		}
		price := domain.ModelOffer{ExternalID: group.ExternalID, Name: group.Name, Group: group.Name, BillingMode: "group_ratio", ModelRatio: group.Ratio}
		result = append(result, domain.PriceObservation{SupplierID: supplierID, EntityType: "group", EntityKey: group.ExternalID, GroupName: group.Name, Price: price, Evidence: evidence, ObservedAt: evidence.ObservedAt})
	}
	for _, model := range models {
		result = append(result, domain.PriceObservation{SupplierID: supplierID, EntityType: "model", EntityKey: model.ExternalID, GroupName: model.Group, ModelName: model.Name, Price: model, Evidence: evidence, ObservedAt: evidence.ObservedAt})
	}
	return result
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			return false
		}
	}
	return true
}

func validQueryParameter(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "&=?#\r\n")
}
