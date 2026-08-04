package connectors

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const (
	maxCapabilityPaths    = 6
	maxCapabilityAuth     = 4
	maxCapabilityAttempts = 12
)

type capabilityAuthProfile struct {
	Name        string
	TokenHeader string
	Scheme      string
	UserHeader  string
	Native      bool
}

type capabilitySelection struct {
	Path string
	Auth capabilityAuthProfile
}

type capabilityProfileKey struct {
	Adapter    string
	Origin     string
	Capability string
	Role       string
}

// capabilityProfileCache stores only endpoint paths and header semantics. It
// never stores tokens, cookies, passwords, request bodies, or response bodies.
type capabilityProfileCache struct {
	mu      sync.RWMutex
	entries map[capabilityProfileKey]capabilitySelection
}

func newCapabilityProfileCache() *capabilityProfileCache {
	return &capabilityProfileCache{entries: make(map[capabilityProfileKey]capabilitySelection)}
}

func (c *capabilityProfileCache) get(key capabilityProfileKey) (capabilitySelection, bool) {
	if c == nil {
		return capabilitySelection{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.entries[key]
	return value, ok
}

func (c *capabilityProfileCache) set(key capabilityProfileKey, value capabilitySelection) {
	if c == nil || value.Path == "" {
		return
	}
	c.mu.Lock()
	c.entries[key] = value
	c.mu.Unlock()
}

func profileKey(adapter, base, capability, role string) capabilityProfileKey {
	return capabilityProfileKey{Adapter: strings.ToLower(adapter), Origin: strings.ToLower(strings.TrimRight(base, "/")),
		Capability: strings.ToLower(capability), Role: strings.ToLower(role)}
}

func capabilitySelections(cache *capabilityProfileCache, supplier domain.Supplier, adapter, base, capability, role,
	explicitPath string, defaults []string, payload domain.CredentialPayload) []capabilitySelection {
	key := profileKey(adapter, base, capability, role)
	cached, hasCached := cache.get(key)
	paths, overridden := capabilityPaths(supplier, adapter, capability, explicitPath, defaults)
	auth, authOverridden := capabilityAuthProfiles(supplier, adapter, capability, role, payload)
	result := make([]capabilitySelection, 0, maxCapabilityAttempts)
	seen := make(map[string]struct{})
	appendSelection := func(value capabilitySelection) {
		if len(result) >= maxCapabilityAttempts || value.Path == "" {
			return
		}
		identity := value.Path + "\x00" + authProfileIdentity(value.Auth)
		if _, exists := seen[identity]; exists {
			return
		}
		seen[identity] = struct{}{}
		result = append(result, value)
	}
	// Explicit metadata is authoritative and goes first. Otherwise the last
	// verified combination is reused before the bounded built-in candidates.
	if hasCached && !overridden && !authOverridden {
		appendSelection(cached)
	}
	for _, path := range paths {
		for _, authProfile := range auth {
			appendSelection(capabilitySelection{Path: path, Auth: authProfile})
		}
	}
	return result
}

func capabilityPaths(supplier domain.Supplier, adapter, capability, explicitPath string, defaults []string) ([]string, bool) {
	prefix := strings.ToLower(strings.TrimSpace(adapter)) + "_" + strings.ToLower(strings.TrimSpace(capability))
	raw, hasList := metadataLookup(supplier.Metadata, prefix+"_paths")
	singular, hasSingular := metadataLookup(supplier.Metadata, prefix+"_path")
	overridden := hasList || hasSingular || strings.TrimSpace(explicitPath) != ""
	values := make([]string, 0, maxCapabilityPaths)
	if strings.TrimSpace(explicitPath) != "" {
		values = append(values, explicitPath)
	}
	if hasSingular {
		values = append(values, singular)
	}
	if hasList {
		values = append(values, parseMetadataList(raw)...)
	}
	if !overridden {
		values = append(values, defaults...)
	}
	result := make([]string, 0, maxCapabilityPaths)
	seen := make(map[string]struct{})
	for _, value := range values {
		path, ok := safeCapabilityPath(value)
		if !ok {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
		if len(result) == maxCapabilityPaths {
			break
		}
	}
	return result, overridden
}

func parseMetadataList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var values []string
	if strings.HasPrefix(raw, "[") && json.Unmarshal([]byte(raw), &values) == nil {
		return values
	}
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' })
}

func safeCapabilityPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, "\r\n") {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Path == "" {
		return "", false
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		parsed.Path = "/" + parsed.Path
	}
	parsed.Fragment = ""
	return parsed.String(), true
}

func capabilityAuthProfiles(supplier domain.Supplier, adapter, capability, role string, payload domain.CredentialPayload) ([]capabilityAuthProfile, bool) {
	prefix := strings.ToLower(adapter) + "_" + strings.ToLower(capability)
	header, headerSet := firstMetadata(supplier.Metadata, prefix+"_auth_header", strings.ToLower(adapter)+"_auth_header")
	scheme, schemeSet := firstMetadata(supplier.Metadata, prefix+"_auth_scheme", strings.ToLower(adapter)+"_auth_scheme")
	userHeader, userSet := firstMetadata(supplier.Metadata, prefix+"_user_header", strings.ToLower(adapter)+"_user_header")
	overridden := headerSet || schemeSet || userSet
	profiles := make([]capabilityAuthProfile, 0, maxCapabilityAuth)
	seen := make(map[string]struct{})
	appendProfile := func(profile capabilityAuthProfile) {
		if len(profiles) >= maxCapabilityAuth || !validAuthProfile(profile) {
			return
		}
		identity := authProfileIdentity(profile)
		if _, exists := seen[identity]; exists {
			return
		}
		seen[identity] = struct{}{}
		profiles = append(profiles, profile)
	}
	if overridden {
		if header == "" {
			header = "Authorization"
		}
		if scheme == "" {
			scheme = "bearer"
		}
		appendProfile(capabilityAuthProfile{Name: "metadata", TokenHeader: header, Scheme: scheme, UserHeader: userHeader})
		return profiles, true
	}
	appendProfile(capabilityAuthProfile{Name: "native", Native: true})
	// Session credentials are already bound to one authentication contract.
	// Replaying them through business-key header shapes can turn a transient
	// upstream failure into a misleading 401 and an unnecessary renewal.
	if role == authCustomerSession || role == authSiteAdmin {
		return profiles, false
	}
	if credentialToken(payload) != "" {
		appendProfile(capabilityAuthProfile{Name: "bearer", TokenHeader: "Authorization", Scheme: "bearer", UserHeader: "New-Api-User"})
		appendProfile(capabilityAuthProfile{Name: "x-api-key", TokenHeader: "X-API-Key", Scheme: "raw", UserHeader: "New-Api-User"})
		appendProfile(capabilityAuthProfile{Name: "api-key", TokenHeader: "API-Key", Scheme: "raw", UserHeader: "X-User-ID"})
	}
	return profiles, false
}

func capabilityCredential(payload domain.CredentialPayload, profile capabilityAuthProfile) domain.CredentialPayload {
	if profile.Native {
		return payload
	}
	result := payload
	result.Headers = cloneStringMap(payload.Headers)
	for key := range result.Headers {
		if equalFoldAny(key, "Authorization", "X-API-Key", "API-Key", "Access-Token", "X-Token", "X-Auth-Token",
			"Auth-Token", "New-Api-User", "X-User-ID", payload.AuthorizationHeader) {
			delete(result.Headers, key)
		}
	}
	token := credentialToken(payload)
	result.Authorization = ""
	result.UserID = 0
	value := token
	if strings.EqualFold(profile.Scheme, "bearer") && token != "" {
		value = "Bearer " + token
	}
	if strings.EqualFold(profile.TokenHeader, "Authorization") {
		result.Authorization = value
	} else if profile.TokenHeader != "" && value != "" {
		if result.Headers == nil {
			result.Headers = make(map[string]string)
		}
		result.Headers[profile.TokenHeader] = value
	}
	if profile.UserHeader != "" && payload.UserID > 0 {
		if result.Headers == nil {
			result.Headers = make(map[string]string)
		}
		result.Headers[profile.UserHeader] = strconv.FormatInt(payload.UserID, 10)
	}
	return result
}

func credentialToken(payload domain.CredentialPayload) string {
	if value := strings.TrimSpace(payload.Authorization); value != "" {
		return trimValidationBearer(value)
	}
	if header := strings.TrimSpace(payload.AuthorizationHeader); header != "" {
		for actual, value := range payload.Headers {
			if strings.EqualFold(actual, header) && strings.TrimSpace(value) != "" {
				return trimValidationBearer(value)
			}
		}
	}
	for _, values := range []map[string]string{payload.Headers, payload.LocalStorage, payload.SessionStorage} {
		for _, key := range []string{"X-API-Key", "API-Key", "Access-Token", "auth_token", "access_token", "token"} {
			for actual, value := range values {
				if strings.EqualFold(actual, key) && strings.TrimSpace(value) != "" {
					return trimValidationBearer(value)
				}
			}
		}
	}
	return ""
}

func validAuthProfile(profile capabilityAuthProfile) bool {
	if profile.Native {
		return true
	}
	if !validHeaderName(profile.TokenHeader) || (profile.UserHeader != "" && !validHeaderName(profile.UserHeader)) {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(profile.Scheme))
	return scheme == "" || scheme == "raw" || scheme == "bearer"
}

func validHeaderName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || strings.ContainsAny(value, "\r\n:") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' {
			return false
		}
	}
	return true
}

func authProfileIdentity(profile capabilityAuthProfile) string {
	if profile.Native {
		return "native"
	}
	return strings.ToLower(profile.TokenHeader + "|" + profile.Scheme + "|" + profile.UserHeader)
}

func metadataLookup(metadata map[string]string, key string) (string, bool) {
	for actual, value := range metadata {
		if strings.EqualFold(strings.TrimSpace(actual), key) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func firstMetadata(metadata map[string]string, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := metadataLookup(metadata, key); ok {
			return value, true
		}
	}
	return "", false
}

func equalFoldAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}
