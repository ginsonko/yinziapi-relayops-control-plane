package credentials

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type Service struct {
	store *store.Store
	vault *secrets.Vault

	hookMu              sync.RWMutex
	credentialUpdatedFn func()
}

func NewService(store *store.Store, vault *secrets.Vault) *Service {
	return &Service{store: store, vault: vault}
}

// SetCredentialUpdatedHook installs a non-blocking scheduler wake-up. The
// durable due marker is committed by the store first, so losing a wake-up only
// defers work until the next normal scheduler tick.
func (s *Service) SetCredentialUpdatedHook(hook func()) {
	s.hookMu.Lock()
	s.credentialUpdatedFn = hook
	s.hookMu.Unlock()
}

func (s *Service) notifyCredentialUpdated() {
	s.hookMu.RLock()
	hook := s.credentialUpdatedFn
	s.hookMu.RUnlock()
	if hook != nil {
		hook()
	}
}

func (s *Service) Create(ctx context.Context, credential *domain.Credential, payload domain.CredentialPayload) error {
	encrypted, err := s.prepare(credential, &payload, time.Now().UTC())
	if err != nil {
		return err
	}
	if payload.Login != nil {
		status := domain.CredentialRenewalStatus{Configured: true, Enabled: payload.Login.Enabled, State: "disabled"}
		if payload.Login.Enabled {
			status.State = "healthy"
		}
		return s.store.CreateCredentialWithRenewal(ctx, credential, encrypted, status)
	}
	return s.store.CreateCredential(ctx, credential, encrypted)
}

// Prepare validates, normalizes and encrypts a credential without persisting
// it. Remote-target reauthorization uses this to validate the new material
// before atomically replacing the existing binding.
func (s *Service) Prepare(credential *domain.Credential, payload domain.CredentialPayload) (string, error) {
	return s.prepare(credential, &payload, time.Now().UTC())
}

func (s *Service) CreateFromPairing(ctx context.Context, tokenHash string, now time.Time, supplierID int64, credential *domain.Credential, payload domain.CredentialPayload) error {
	encrypted, err := s.prepare(credential, &payload, now)
	if err != nil {
		return err
	}
	return s.store.CreateCredentialFromPairing(ctx, tokenHash, now, supplierID, credential, encrypted)
}

func (s *Service) CreateFromPairingWithCustody(ctx context.Context, tokenHash string, now time.Time, supplierID int64,
	credential *domain.Credential, payload domain.CredentialPayload, expectedOrigin string, custodyExpiresAt time.Time) error {
	encrypted, err := s.prepare(credential, &payload, now)
	if err != nil {
		return err
	}
	return s.store.CreateCredentialFromPairingWithCustody(ctx, tokenHash, now, supplierID, credential, encrypted,
		expectedOrigin, custodyExpiresAt)
}

// CreateForSupplier persists an already verified login candidate and its
// supplier binding as one transaction.
func (s *Service) CreateForSupplier(ctx context.Context, supplierID int64, credential *domain.Credential, payload domain.CredentialPayload) error {
	encrypted, err := s.prepare(credential, &payload, time.Now().UTC())
	if err != nil {
		return err
	}
	status := domain.CredentialRenewalStatus{State: "unconfigured"}
	if payload.Login != nil {
		status.Configured = true
		status.Enabled = payload.Login.Enabled
		status.State = "disabled"
		if payload.Login.Enabled {
			now := time.Now().UTC()
			status.State = "healthy"
			status.LastSuccessAt = &now
			next := now.Add(time.Duration(payload.Login.MinimumIntervalSeconds) * time.Second)
			status.NextAttemptAt = &next
		}
	}
	return s.store.CreateCredentialForSupplier(ctx, supplierID, credential, encrypted, status)
}

func (s *Service) prepare(credential *domain.Credential, payload *domain.CredentialPayload, now time.Time) (string, error) {
	if err := normalizeAuthorization(payload); err != nil {
		return "", err
	}
	if payload.Login != nil {
		normalized, err := normalizeLoginRule(*payload.Login, LoginPolicy{})
		if err != nil {
			return "", err
		}
		payload.Login = &normalized
	}
	if credential.ExpiresAt == nil {
		credential.ExpiresAt = inferExpiry(*payload, now)
	}
	return s.encryptPayload(*payload)
}

func (s *Service) encryptPayload(payload domain.CredentialPayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode credential payload: %w", err)
	}
	encrypted, err := s.vault.Encrypt(encoded)
	if err != nil {
		return "", err
	}
	return encrypted, nil
}

func (s *Service) Load(ctx context.Context, id int64) (domain.Credential, domain.CredentialPayload, error) {
	credential, encrypted, err := s.store.GetCredentialEncrypted(ctx, id)
	if err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, err
	}
	plain, err := s.vault.Decrypt(encrypted)
	if err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, err
	}
	var payload domain.CredentialPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, fmt.Errorf("decode credential payload: %w", err)
	}
	return credential, payload, nil
}

func (s *Service) Update(ctx context.Context, id int64, payload domain.CredentialPayload) error {
	return s.UpdateWithExpiry(ctx, id, payload, nil)
}

func (s *Service) UpdateWithExpiry(ctx context.Context, id int64, payload domain.CredentialPayload, expiresAt *time.Time) error {
	if err := normalizeAuthorization(&payload); err != nil {
		return err
	}
	if expiresAt == nil {
		credential, _, err := s.Load(ctx, id)
		if err != nil {
			return fmt.Errorf("load credential expiry: %w", err)
		}
		expiresAt = credential.ExpiresAt
	}
	encrypted, err := s.encryptPayload(payload)
	if err != nil {
		return err
	}
	if err := s.store.UpdateCredentialPayloadAndSchedule(ctx, id, encrypted, expiresAt); err != nil {
		return err
	}
	s.notifyCredentialUpdated()
	return nil
}

func (s *Service) ConfigureLogin(ctx context.Context, id int64, rule domain.LoginRule, preservePassword bool,
	policy LoginPolicy) (domain.CredentialRenewalDetail, error) {
	credential, payload, err := s.Load(ctx, id)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	if preservePassword && strings.TrimSpace(rule.Password) == "" && payload.Login != nil {
		rule.Password = payload.Login.Password
	}
	if strings.TrimSpace(rule.Username) == "" && payload.Login != nil {
		rule.Username = payload.Login.Username
	}
	if strings.EqualFold(credential.Kind, "browser_pairing") && rule.Enabled && !rule.AllowReplaceBrowserPairing {
		return domain.CredentialRenewalDetail{}, errors.New("automatic login for a browser-paired credential requires allow_replace_browser_pairing")
	}
	rule, err = normalizeLoginRule(rule, policy)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	payload.Login = &rule
	status := domain.CredentialRenewalStatus{Configured: true, Enabled: rule.Enabled, State: "disabled"}
	if rule.Enabled {
		status.State = "healthy"
		if credential.Status != "" && credential.Status != "active" {
			status.State = "auth_expired"
		}
	}
	encrypted, err := s.encryptPayload(payload)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	credentialStatus := credential.Status
	if credentialStatus == "" {
		credentialStatus = "active"
	}
	if err := s.store.CommitCredentialRenewal(ctx, credential.ID, credential.UpdatedAt, encrypted, credential.ExpiresAt,
		credentialStatus, status); err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	return s.RenewalDetail(ctx, id)
}

func (s *Service) RemoveLogin(ctx context.Context, id int64) (domain.CredentialRenewalDetail, error) {
	credential, payload, err := s.Load(ctx, id)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	payload.Login = nil
	encrypted, err := s.encryptPayload(payload)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	status := domain.CredentialRenewalStatus{State: "unconfigured"}
	if err := s.store.CommitCredentialRenewal(ctx, credential.ID, credential.UpdatedAt, encrypted, credential.ExpiresAt,
		credential.Status, status); err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	return s.RenewalDetail(ctx, id)
}

func (s *Service) RenewalDetail(ctx context.Context, id int64) (domain.CredentialRenewalDetail, error) {
	credential, payload, err := s.Load(ctx, id)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	status, err := s.store.GetCredentialRenewal(ctx, id)
	if err != nil {
		return domain.CredentialRenewalDetail{}, err
	}
	detail := domain.CredentialRenewalDetail{CredentialID: credential.ID, Status: status}
	if payload.Login == nil {
		return detail, nil
	}
	detail.UsernameConfigured = strings.TrimSpace(payload.Login.Username) != ""
	detail.PasswordConfigured = payload.Login.Password != ""
	detail.Rule = loginRuleView(*payload.Login)
	return detail, nil
}

func (s *Service) CommitRenewal(ctx context.Context, credential domain.Credential, payload domain.CredentialPayload,
	expiresAt *time.Time, status domain.CredentialRenewalStatus) error {
	if err := normalizeAuthorization(&payload); err != nil {
		return err
	}
	encrypted, err := s.encryptPayload(payload)
	if err != nil {
		return err
	}
	if err := s.store.CommitSuccessfulCredentialRenewal(ctx, credential.ID, credential.UpdatedAt, encrypted, expiresAt, "active", status); err != nil {
		return err
	}
	s.notifyCredentialUpdated()
	return nil
}

// CommitBrowserCustodyRenewal preserves declarative login/refresh rules while
// replacing only the captured browser session. The encrypted credential, due
// collection marker and custody success audit share one CAS transaction.
func (s *Service) CommitBrowserCustodyRenewal(ctx context.Context, credential domain.Credential,
	existing, captured domain.CredentialPayload, status domain.CredentialRenewalStatus, tokenHash string,
	supplierID int64) error {
	candidate := NormalizeCapturedPayload(captured)
	candidate.Login = existing.Login
	candidate.Refresh = existing.Refresh
	if strings.TrimSpace(existing.AuthorizationMode) != "" {
		candidate.AuthorizationMode = existing.AuthorizationMode
		candidate.AuthorizationHeader = existing.AuthorizationHeader
	}
	if err := normalizeAuthorization(&candidate); err != nil {
		return err
	}
	expiresAt := inferExpiry(candidate, time.Now().UTC())
	encrypted, err := s.encryptPayload(candidate)
	if err != nil {
		return err
	}
	if err := s.store.CommitSuccessfulBrowserCustodyRenewal(ctx, credential.ID, credential.UpdatedAt, encrypted,
		expiresAt, "active", status, tokenHash, supplierID); err != nil {
		return err
	}
	s.notifyCredentialUpdated()
	return nil
}

func normalizeAuthorization(payload *domain.CredentialPayload) error {
	mode := strings.ToLower(strings.TrimSpace(payload.AuthorizationMode))
	if mode == "" {
		if strings.TrimSpace(payload.Authorization) != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(payload.Authorization)), "bearer ") {
			mode = "raw"
		} else {
			mode = "bearer"
		}
	}
	if mode != "bearer" && mode != "raw" && mode != "header" {
		return fmt.Errorf("unsupported authorization_mode %q", payload.AuthorizationMode)
	}
	payload.AuthorizationMode = mode

	token := tokenFromPayload(*payload)
	switch mode {
	case "bearer":
		if token != "" {
			payload.Authorization = "Bearer " + token
		}
	case "raw":
		// An explicit raw Authorization value is authoritative. Browser pairing
		// often supplies both it and the underlying token; in that case use the
		// token so an earlier UI-side Bearer default cannot override this mode.
		if stored := tokenFromStorage(*payload); stored != "" {
			payload.Authorization = stored
		}
	case "header":
		header := strings.TrimSpace(payload.AuthorizationHeader)
		if header == "" || strings.ContainsAny(header, "\r\n:") {
			return errors.New("authorization_header is required for header authorization mode")
		}
		payload.AuthorizationHeader = header
		if payload.Headers == nil {
			payload.Headers = make(map[string]string)
		}
		if token != "" {
			payload.Headers[header] = token
		}
		payload.Authorization = ""
	}
	return nil
}

func setAccessToken(payload *domain.CredentialPayload, token string) error {
	if payload.LocalStorage == nil {
		payload.LocalStorage = make(map[string]string)
	}
	for _, key := range []string{"access_token", "auth_token", "token"} {
		if _, exists := payload.LocalStorage[key]; exists {
			payload.LocalStorage[key] = token
		}
		if payload.SessionStorage != nil {
			if _, exists := payload.SessionStorage[key]; exists {
				payload.SessionStorage[key] = token
			}
		}
	}
	payload.LocalStorage["auth_token"] = token
	return normalizeAuthorization(payload)
}

func tokenFromPayload(payload domain.CredentialPayload) string {
	if token := tokenFromStorage(payload); token != "" {
		return token
	}
	if payload.AuthorizationMode == "header" && payload.Headers != nil {
		if token := strings.TrimSpace(payload.Headers[payload.AuthorizationHeader]); token != "" {
			return trimBearer(token)
		}
	}
	return trimBearer(payload.Authorization)
}

func tokenFromStorage(payload domain.CredentialPayload) string {
	for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
		for _, key := range []string{"access_token", "auth_token", "token"} {
			if value := strings.TrimSpace(values[key]); value != "" {
				return trimBearer(value)
			}
		}
	}
	return ""
}

func trimBearer(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func inferExpiry(payload domain.CredentialPayload, now time.Time) *time.Time {
	if payload.ExpiresAt != nil && !payload.ExpiresAt.IsZero() {
		value := payload.ExpiresAt.UTC()
		return &value
	}
	if payload.ExpiresIn > 0 {
		value := now.Add(time.Duration(payload.ExpiresIn) * time.Second)
		return &value
	}
	for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
		for _, key := range []string{"expires_at", "expiry", "expiration"} {
			if value := parseExpiryValue(values[key]); value != nil {
				return value
			}
		}
		for _, key := range []string{"expires_in", "expiresIn"} {
			if seconds, err := strconv.ParseInt(strings.TrimSpace(values[key]), 10, 64); err == nil && seconds > 0 {
				value := now.Add(time.Duration(seconds) * time.Second)
				return &value
			}
		}
	}
	return jwtExpiry(tokenFromPayload(payload))
}

func parseExpiryValue(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if value, err := time.Parse(time.RFC3339, raw); err == nil {
		value = value.UTC()
		return &value
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	if seconds > 1_000_000_000_000 {
		seconds /= 1000
	}
	value := time.Unix(seconds, 0).UTC()
	return &value
}

func jwtExpiry(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		claims, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var body struct {
		Expires json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(claims)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil
	}
	seconds, err := body.Expires.Int64()
	if err != nil || seconds <= 0 {
		return nil
	}
	value := time.Unix(seconds, 0).UTC()
	return &value
}
