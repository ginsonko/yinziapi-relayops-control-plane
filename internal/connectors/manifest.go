package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const maximumConnectorManifestSize = 256 << 10

var (
	manifestNameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)
	manifestHeaderRE   = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
	manifestJSONPathRE = regexp.MustCompile(`^[A-Za-z0-9_$.-]+(?:\[[0-9]+\])?(?:\.[A-Za-z0-9_$-]+(?:\[[0-9]+\])?)*$`)
)

var nativeManifestCapabilities = map[string]map[string]string{
	"newapi": {
		"status": "newapi_status_path", "models": "newapi_models_path", "usage": "usage_path",
		"admin_probe": "newapi_admin_probe_path", "pricing": "pricing_path", "balance": "balance_path",
	},
	"sub2api": {
		"health": "sub2api_health_path", "customer_catalog": "sub2api_customer_catalog_path",
		"customer_groups": "sub2api_customer_groups_path", "customer_rates": "sub2api_customer_rates_path",
		"customer_balance": "sub2api_customer_balance_path", "customer_usage": "sub2api_customer_usage_path",
		"models": "sub2api_models_path", "usage": "sub2api_usage_path", "admin_groups": "sub2api_admin_groups_path",
		"admin_accounts": "sub2api_admin_accounts_path", "admin_dashboard": "sub2api_admin_dashboard_path",
		"admin_today_stats": "sub2api_admin_today_stats_path", "admin_account_models": "sub2api_admin_account_models_path",
		"billing": "sub2api_billing_path",
	},
}

// ParseConnectorManifest strictly decodes and canonicalizes a native connector
// manifest. Unknown JSON fields are rejected so a typo cannot silently disable
// an authentication or capability constraint.
func ParseConnectorManifest(data []byte) (domain.ConnectorManifest, []byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return domain.ConnectorManifest{}, nil, errors.New("connector manifest is empty")
	}
	if len(data) > maximumConnectorManifestSize {
		return domain.ConnectorManifest{}, nil, fmt.Errorf("connector manifest exceeds %d bytes", maximumConnectorManifestSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest domain.ConnectorManifest
	if err := decoder.Decode(&manifest); err != nil {
		return domain.ConnectorManifest{}, nil, fmt.Errorf("decode connector manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return domain.ConnectorManifest{}, nil, errors.New("connector manifest contains multiple JSON documents")
		}
		return domain.ConnectorManifest{}, nil, fmt.Errorf("decode trailing connector manifest content: %w", err)
	}
	if err := ValidateConnectorManifest(&manifest); err != nil {
		return domain.ConnectorManifest{}, nil, err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return domain.ConnectorManifest{}, nil, fmt.Errorf("encode canonical connector manifest: %w", err)
	}
	return manifest, canonical, nil
}

func ValidateConnectorManifest(manifest *domain.ConnectorManifest) error {
	if manifest == nil {
		return errors.New("connector manifest is nil")
	}
	manifest.SchemaVersion = strings.TrimSpace(manifest.SchemaVersion)
	if manifest.SchemaVersion != domain.ConnectorManifestSchemaV1 {
		return fmt.Errorf("schema_version must be %q", domain.ConnectorManifestSchemaV1)
	}
	manifest.Name = strings.TrimSpace(manifest.Name)
	if manifest.Name == "" || len(manifest.Name) > 120 {
		return errors.New("name must contain 1 to 120 characters")
	}
	manifest.AdapterFamily = canonicalManifestAdapter(manifest.AdapterFamily)
	allowedCapabilities, supported := nativeManifestCapabilities[manifest.AdapterFamily]
	if !supported {
		return errors.New("adapter_family must be newapi or sub2api")
	}
	if err := normalizeManifestOrigins(&manifest.Origins); err != nil {
		return err
	}
	if err := normalizeManifestAuth(&manifest.Auth, "auth"); err != nil {
		return err
	}
	if len(manifest.Capabilities) == 0 || len(manifest.Capabilities) > 24 {
		return errors.New("capabilities must contain 1 to 24 entries")
	}
	normalizedCapabilities := make(map[string]domain.ConnectorManifestCapability, len(manifest.Capabilities))
	for rawName, capability := range manifest.Capabilities {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !manifestNameRE.MatchString(name) {
			return fmt.Errorf("capability name %q is invalid", rawName)
		}
		if _, ok := allowedCapabilities[name]; !ok {
			return fmt.Errorf("capability %q is not supported by adapter_family %q", name, manifest.AdapterFamily)
		}
		if _, exists := normalizedCapabilities[name]; exists {
			return fmt.Errorf("capability %q is duplicated", name)
		}
		if len(capability.Paths) == 0 || len(capability.Paths) > maxCapabilityPaths {
			return fmt.Errorf("capabilities.%s.paths must contain 1 to %d entries", name, maxCapabilityPaths)
		}
		seenPaths := make(map[string]struct{}, len(capability.Paths))
		paths := make([]string, 0, len(capability.Paths))
		for _, rawPath := range capability.Paths {
			path, ok := safeCapabilityPath(rawPath)
			if !ok {
				return fmt.Errorf("capabilities.%s.paths contains an unsafe cross-origin or invalid path", name)
			}
			if _, exists := seenPaths[path]; exists {
				continue
			}
			seenPaths[path] = struct{}{}
			paths = append(paths, path)
		}
		capability.Paths = paths
		capability.Contract = strings.ToLower(strings.TrimSpace(capability.Contract))
		if capability.Contract == "" {
			capability.Contract = "native"
		}
		if capability.Contract != "native" && capability.Contract != "json" {
			return fmt.Errorf("capabilities.%s.contract must be native or json", name)
		}
		if capability.Auth != nil {
			if err := normalizeManifestAuth(capability.Auth, "capabilities."+name+".auth"); err != nil {
				return err
			}
		}
		if len(capability.JSONPaths) > 24 {
			return fmt.Errorf("capabilities.%s.json_paths contains too many entries", name)
		}
		if capability.Contract == "json" && len(capability.JSONPaths) == 0 {
			return fmt.Errorf("capabilities.%s.json_paths is required for the json contract", name)
		}
		if capability.Contract == "json" {
			if manifest.AdapterFamily != "newapi" || (name != "balance" && name != "usage") {
				return fmt.Errorf("capabilities.%s json contract is only supported for NewAPI balance and usage; use a JSON Connector for other custom documents", name)
			}
			if name == "balance" && strings.TrimSpace(capability.JSONPaths["amount"]) == "" {
				return errors.New("capabilities.balance.json_paths.amount is required")
			}
			if name == "usage" && strings.TrimSpace(capability.JSONPaths["amount"]) == "" && strings.TrimSpace(capability.JSONPaths["daily_spend"]) == "" {
				return errors.New("capabilities.usage.json_paths.amount or daily_spend is required")
			}
		}
		for key, path := range capability.JSONPaths {
			if !manifestNameRE.MatchString(strings.TrimSpace(key)) || !manifestJSONPathRE.MatchString(strings.TrimSpace(path)) {
				return fmt.Errorf("capabilities.%s.json_paths contains an invalid declarative path", name)
			}
		}
		normalizedCapabilities[name] = capability
	}
	manifest.Capabilities = normalizedCapabilities
	if manifest.Login != nil {
		if err := validateManifestLogin(manifest.Login); err != nil {
			return err
		}
	}
	return nil
}

func canonicalManifestAdapter(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "newapi", "newapi-native":
		return "newapi"
	case "sub2api", "sub2api_admin", "sub2api_upstream":
		return "sub2api"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func normalizeManifestOrigins(origins *domain.ConnectorManifestOrigins) error {
	if len(origins.Schemes) == 0 || len(origins.Schemes) > 2 {
		return errors.New("origins.schemes must contain http and/or https")
	}
	rawSchemes := append([]string(nil), origins.Schemes...)
	schemeSet := make(map[string]struct{})
	origins.Schemes = origins.Schemes[:0]
	for _, raw := range rawSchemes {
		scheme := strings.ToLower(strings.TrimSpace(raw))
		if scheme != "http" && scheme != "https" {
			return errors.New("origins.schemes only supports http and https")
		}
		if _, exists := schemeSet[scheme]; !exists {
			schemeSet[scheme] = struct{}{}
			origins.Schemes = append(origins.Schemes, scheme)
		}
	}
	if len(origins.Hosts) == 0 || len(origins.Hosts) > 32 {
		return errors.New("origins.hosts must contain 1 to 32 host names")
	}
	hostSet := make(map[string]struct{})
	normalizedHosts := make([]string, 0, len(origins.Hosts))
	for _, raw := range origins.Hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		wildcard := strings.HasPrefix(host, "*.")
		candidate := strings.TrimPrefix(host, "*.")
		if candidate == "" || strings.ContainsAny(candidate, "/?#@\r\n") {
			return fmt.Errorf("origin host %q is invalid", raw)
		}
		name := candidate
		if parsedHost, _, err := net.SplitHostPort(candidate); err == nil {
			name = parsedHost
		}
		if net.ParseIP(strings.Trim(name, "[]")) == nil && !validDNSName(name) {
			return fmt.Errorf("origin host %q is invalid", raw)
		}
		if wildcard && net.ParseIP(strings.Trim(name, "[]")) != nil {
			return fmt.Errorf("origin host %q cannot wildcard an IP address", raw)
		}
		if _, exists := hostSet[host]; !exists {
			hostSet[host] = struct{}{}
			normalizedHosts = append(normalizedHosts, host)
		}
	}
	sort.Strings(normalizedHosts)
	origins.Hosts = normalizedHosts
	return nil
}

func validDNSName(value string) bool {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if value == "localhost" {
		return true
	}
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func normalizeManifestAuth(auth *domain.ConnectorManifestAuth, field string) error {
	auth.Header = strings.TrimSpace(auth.Header)
	auth.Scheme = strings.ToLower(strings.TrimSpace(auth.Scheme))
	auth.UserHeader = strings.TrimSpace(auth.UserHeader)
	if auth.Header != "" && !manifestHeaderRE.MatchString(auth.Header) {
		return fmt.Errorf("%s.header is not a valid HTTP header name", field)
	}
	if auth.UserHeader != "" && !manifestHeaderRE.MatchString(auth.UserHeader) {
		return fmt.Errorf("%s.user_header is not a valid HTTP header name", field)
	}
	if auth.Scheme != "" && auth.Scheme != "bearer" && auth.Scheme != "raw" {
		return fmt.Errorf("%s.scheme must be bearer or raw", field)
	}
	if auth.Scheme != "" && auth.Header == "" {
		auth.Header = "Authorization"
	}
	return nil
}

func validateManifestLogin(rule *domain.CredentialLoginRuleView) error {
	if rule == nil {
		return nil
	}
	for field, raw := range map[string]string{
		"url": rule.URL, "refresh_url": rule.RefreshURL,
	} {
		if strings.TrimSpace(raw) == "" && field == "refresh_url" {
			continue
		}
		if _, ok := safeCapabilityPath(raw); !ok {
			return fmt.Errorf("login.%s must be a same-origin relative path", field)
		}
	}
	method := strings.ToUpper(strings.TrimSpace(rule.Method))
	if method != "POST" && method != "GET" {
		return errors.New("login.method must be GET or POST")
	}
	if strings.TrimSpace(rule.UsernameField) == "" || strings.TrimSpace(rule.PasswordField) == "" {
		return errors.New("login username_field and password_field are required")
	}
	for field, path := range map[string]string{
		"token_json_path": rule.TokenJSONPath, "user_id_json_path": rule.UserIDJSONPath,
		"expires_in_json_path": rule.ExpiresInJSONPath, "expires_at_json_path": rule.ExpiresAtJSONPath,
		"success_json_path": rule.SuccessJSONPath, "message_json_path": rule.MessageJSONPath,
		"captcha_json_path": rule.CaptchaJSONPath, "two_factor_json_path": rule.TwoFactorJSONPath,
	} {
		if strings.TrimSpace(path) != "" && !manifestJSONPathRE.MatchString(strings.TrimSpace(path)) {
			return fmt.Errorf("login.%s is not a valid declarative JSON path", field)
		}
	}
	return nil
}

// ConnectorManifestMatchesOrigin verifies the binding at both import time and
// use time. A wildcard only covers subdomains, never the apex host.
func ConnectorManifestMatchesOrigin(manifest domain.ConnectorManifest, baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return false
	}
	schemeOK := false
	for _, scheme := range manifest.Origins.Schemes {
		if strings.EqualFold(scheme, parsed.Scheme) {
			schemeOK = true
			break
		}
	}
	if !schemeOK {
		return false
	}
	host := strings.ToLower(parsed.Host)
	hostname := strings.ToLower(parsed.Hostname())
	for _, allowed := range manifest.Origins.Hosts {
		allowed = strings.ToLower(allowed)
		if host == allowed || hostname == allowed {
			return true
		}
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*.")
			if hostname != suffix && strings.HasSuffix(hostname, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// ConnectorManifestMetadata translates a validated manifest into the existing
// native connector metadata contract. Values are endpoint/header semantics;
// no credential material is introduced.
func ConnectorManifestMetadata(manifest domain.ConnectorManifest) map[string]string {
	metadata := make(map[string]string)
	keys := nativeManifestCapabilities[manifest.AdapterFamily]
	for capabilityName, capability := range manifest.Capabilities {
		pathKey := keys[capabilityName]
		if pathKey == "" || len(capability.Paths) == 0 {
			continue
		}
		metadata[pathKey] = capability.Paths[0]
		encodedPaths, _ := json.Marshal(capability.Paths)
		prefix := manifest.AdapterFamily + "_" + capabilityNegotiationName(capabilityName)
		metadata[prefix+"_paths"] = string(encodedPaths)
		auth := manifest.Auth
		if capability.Auth != nil {
			auth = *capability.Auth
		}
		if auth.Header != "" {
			metadata[prefix+"_auth_header"] = auth.Header
		}
		if auth.Scheme != "" {
			metadata[prefix+"_auth_scheme"] = auth.Scheme
		}
		if auth.UserHeader != "" {
			metadata[prefix+"_user_header"] = auth.UserHeader
		}
		if len(capability.JSONPaths) > 0 {
			encoded, _ := json.Marshal(capability.JSONPaths)
			metadata["manifest_"+capabilityName+"_json_paths"] = string(encoded)
		}
		metadata["manifest_"+capabilityName+"_contract"] = capability.Contract
	}
	return metadata
}

func capabilityNegotiationName(name string) string {
	switch name {
	case "models", "customer_catalog":
		return "catalog"
	case "customer_balance":
		return "balance"
	case "customer_usage":
		return "usage"
	case "admin_probe", "admin_groups", "admin_accounts", "admin_dashboard", "admin_today_stats", "admin_account_models":
		return "admin"
	default:
		return name
	}
}
