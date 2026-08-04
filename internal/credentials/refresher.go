package credentials

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
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type Refresher struct {
	store        *store.Store
	service      *Service
	client       *http.Client
	interval     time.Duration
	refreshAhead time.Duration
	loginPolicy  LoginPolicy
	validator    CandidateValidator
	remoteWrites *remotewrite.Gate
	mu           sync.Mutex
	inFlight     map[int64]*refreshCall
	wake         chan struct{}
}

type refreshCall struct {
	done chan struct{}
	err  error
}

func NewRefresher(st *store.Store, service *Service, client *http.Client, interval time.Duration) *Refresher {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Refresher{store: st, service: service, client: client, interval: interval, refreshAhead: 15 * time.Minute,
		loginPolicy: defaultLoginPolicy(LoginPolicy{}),
		inFlight:    make(map[int64]*refreshCall), wake: make(chan struct{}, 1)}
}

func (r *Refresher) WithRefreshAhead(value time.Duration) *Refresher {
	if value > 0 {
		r.refreshAhead = value
	}
	return r
}

func (r *Refresher) WithRemoteWriteGate(gate *remotewrite.Gate) *Refresher {
	r.remoteWrites = gate
	return r
}

func (r *Refresher) ConfigureLogin(ctx context.Context, id int64, rule domain.LoginRule,
	preservePassword bool) (domain.CredentialRenewalDetail, error) {
	detail, err := r.service.ConfigureLogin(ctx, id, rule, preservePassword, r.loginPolicy)
	if err == nil && detail.Status.Enabled && detail.Status.State == "auth_expired" {
		r.Wake()
	}
	return detail, err
}

func (r *Refresher) RenewalDetail(ctx context.Context, id int64) (domain.CredentialRenewalDetail, error) {
	return r.service.RenewalDetail(ctx, id)
}

func (r *Refresher) RemoveLogin(ctx context.Context, id int64) (domain.CredentialRenewalDetail, error) {
	return r.service.RemoveLogin(ctx, id)
}

func (r *Refresher) Run(ctx context.Context) {
	r.refreshDue(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			r.refreshDue(ctx)
		case <-ticker.C:
			r.refreshDue(ctx)
		}
	}
}

// Wake asks the lifecycle loop to evaluate due credentials immediately. The
// channel is deliberately coalescing because due state is durable in SQLite.
func (r *Refresher) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Refresher) refreshDue(ctx context.Context) {
	items, err := r.store.ListCredentials(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	custodied := make(map[int64]struct{})
	if bindings, listErr := r.store.ListBrowserCustodies(ctx, now); listErr == nil {
		for _, binding := range bindings {
			if binding.Enabled {
				custodied[binding.CredentialID] = struct{}{}
			}
		}
	}
	for _, item := range items {
		if _, browserManaged := custodied[item.ID]; browserManaged {
			continue
		}
		expiring := item.ExpiresAt != nil && !item.ExpiresAt.After(now.Add(r.refreshAhead))
		legacyRetry := item.Status == "refresh_failed" || item.Status == "auth_expired"
		renewalDue := false
		if item.Renewal.Configured && item.Renewal.Enabled {
			switch item.Renewal.State {
			case "human_action_required", "permission_denied", "configuration_required":
				continue
			case "suspended_backoff":
				if item.Renewal.NextAttemptAt == nil {
					continue
				}
			}
			if item.Renewal.NextAttemptAt != nil && item.Renewal.NextAttemptAt.After(now) {
				continue
			}
			renewalDue = expiring || item.Status == "auth_expired" || item.Renewal.State == "auth_expired" ||
				item.Renewal.State == "retry_backoff" || item.Renewal.State == "suspended_backoff"
		}
		if !expiring && !legacyRetry && !renewalDue {
			continue
		}
		if !item.UpdatedAt.IsZero() && now.Sub(item.UpdatedAt) < r.interval && !renewalDue {
			continue
		}
		_ = r.Refresh(ctx, item.ID)
	}
}

func (r *Refresher) Refresh(ctx context.Context, id int64) error {
	if err := r.remoteWrites.Check("credential refresh or login renewal"); err != nil {
		return err
	}
	r.mu.Lock()
	if active := r.inFlight[id]; active != nil {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-active.done:
			return active.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	r.inFlight[id] = call
	r.mu.Unlock()

	call.err = r.refresh(ctx, id)
	r.mu.Lock()
	delete(r.inFlight, id)
	close(call.done)
	r.mu.Unlock()
	return call.err
}

func (r *Refresher) refresh(ctx context.Context, id int64) error {
	credential, payload, err := r.service.Load(ctx, id)
	if err != nil {
		if known, ok := r.credentialMetadata(ctx, id); ok {
			r.recordFailure(ctx, known, err)
		}
		return err
	}
	previousStatus := credential.Status
	var refreshErr error
	if payload.Refresh != nil {
		if payload.Login != nil && payload.Login.Enabled {
			status, _ := r.store.GetCredentialRenewal(ctx, credential.ID)
			now := time.Now().UTC()
			status.Configured = true
			status.Enabled = true
			status.State = "refresh_token_attempt"
			status.LastAttemptAt = &now
			_ = r.store.SetCredentialRenewalState(ctx, credential.ID, status)
		}
		refreshErr = r.refreshLoaded(ctx, credential, payload)
		if refreshErr == nil {
			if previousStatus == "refresh_failed" || previousStatus == "auth_expired" || previousStatus == "retry_backoff" || previousStatus == "suspended_backoff" {
				_ = r.store.AddActivity(ctx, domain.Activity{
					Kind: "credential_refresh_recovered", Severity: "success", Title: "Credential refresh recovered",
					Message:  credential.Name + " automatic refresh recovered",
					Metadata: map[string]string{"credential_id": strconv.FormatInt(id, 10)},
				})
			}
			return nil
		}
		if errors.Is(refreshErr, store.ErrCredentialChanged) {
			return newRenewalError("concurrent_change", "retry_backoff",
				"the credential changed while renewal was being verified; the newer value was preserved", "retry_manual_renewal")
		}
	}
	if payload.Login != nil && payload.Login.Enabled {
		return r.passwordRenew(ctx, credential, payload)
	}
	if refreshErr == nil {
		refreshErr = errors.New("credential has no refresh or automatic login rule")
	}
	r.recordFailure(ctx, credential, refreshErr)
	return refreshErr
}

// SignalAuthExpired is the integration point for bounded 401/403 evidence.
// It schedules, but does not synchronously launch, an automatic login attempt.
func (r *Refresher) SignalAuthExpired(ctx context.Context, id int64) error {
	credential, payload, err := r.service.Load(ctx, id)
	if err != nil {
		return err
	}
	if (payload.Login == nil || !payload.Login.Enabled) && payload.Refresh != nil {
		if err := r.store.MarkCredentialStatus(ctx, credential.ID, "auth_expired"); err != nil {
			return err
		}
		r.Wake()
		return nil
	}
	if payload.Login == nil || !payload.Login.Enabled {
		return errors.New("credential has no enabled automatic login or refresh rule")
	}
	rule, err := normalizeLoginRule(*payload.Login, r.loginPolicy)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	status, err := r.store.GetCredentialRenewal(ctx, id)
	if err != nil {
		return err
	}
	status.Configured = true
	status.Enabled = true
	status.State = "auth_expired"
	status.LastErrorClass = "auth_expired"
	status.HumanAction = ""
	next := now
	if status.LastAttemptAt != nil {
		minimum := status.LastAttemptAt.Add(time.Duration(rule.MinimumIntervalSeconds) * time.Second)
		if minimum.After(next) {
			next = minimum
		}
	}
	status.NextAttemptAt = &next
	if err := r.store.SetCredentialRenewalState(ctx, id, status); err != nil {
		return err
	}
	if err := r.store.MarkCredentialStatus(ctx, credential.ID, "auth_expired"); err != nil {
		return err
	}
	r.Wake()
	return nil
}

func (r *Refresher) refreshLoaded(ctx context.Context, credential domain.Credential, payload domain.CredentialPayload) error {
	payload, expiresAt, err := r.refreshCandidate(ctx, credential, payload)
	if err != nil {
		return err
	}
	if payload.Login != nil && payload.Login.Enabled {
		if err := r.validateCandidate(ctx, credential, payload); err != nil {
			return fmt.Errorf("validate refreshed credential: %w", err)
		}
		now := time.Now().UTC()
		status, _ := r.store.GetCredentialRenewal(ctx, credential.ID)
		status.Configured = true
		status.Enabled = true
		status.State = "healthy"
		status.ConsecutiveFailures = 0
		status.LastAttemptAt = &now
		status.LastSuccessAt = &now
		nextAttempt := now.Add(time.Duration(payload.Login.MinimumIntervalSeconds) * time.Second)
		status.NextAttemptAt = &nextAttempt
		status.HumanAction = ""
		status.LastErrorClass = ""
		return r.service.CommitRenewal(ctx, credential, payload, expiresAt, status)
	}
	return r.service.UpdateWithExpiry(ctx, credential.ID, payload, expiresAt)
}

func (r *Refresher) refreshCandidate(ctx context.Context, credential domain.Credential,
	payload domain.CredentialPayload) (domain.CredentialPayload, *time.Time, error) {
	if err := r.remoteWrites.Check("credential refresh exchange"); err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	if payload.Refresh == nil {
		return domain.CredentialPayload{}, nil, errors.New("credential has no refresh rule")
	}
	rule := payload.Refresh
	requestURL, err := safeRefreshURL(credential.SiteURL, rule.URL)
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	client, err := netguard.SameOriginClient(r.client, credential.SiteURL)
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(rule.Method))
	if method == "" {
		method = http.MethodPost
	}
	if method != http.MethodPost && method != http.MethodGet {
		return domain.CredentialPayload{}, nil, errors.New("credential refresh method must be GET or POST")
	}
	body := expandRefreshBody(rule.Body, payload)
	req, err := http.NewRequestWithContext(ctx, method, requestURL, strings.NewReader(body))
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	netguard.ApplyControlPlaneUserAgent(req)
	if payload.Authorization != "" {
		req.Header.Set("Authorization", payload.Authorization)
	}
	if payload.Cookie != "" {
		req.Header.Set("Cookie", payload.Cookie)
	}
	for key, value := range payload.Headers {
		if strings.TrimSpace(key) != "" && !strings.ContainsAny(key+value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	for key, value := range rule.Headers {
		if strings.ContainsAny(key+value, "\r\n") {
			return domain.CredentialPayload{}, nil, errors.New("refresh header contains a newline")
		}
		req.Header.Set(key, expandRefreshBody(value, payload))
	}
	resp, err := client.Do(req)
	if err != nil {
		return domain.CredentialPayload{}, nil, fmt.Errorf("refresh credential: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.CredentialPayload{}, nil, fmt.Errorf("credential refresh returned %s", resp.Status)
	}
	cookies := resp.Cookies()
	var root any
	if len(bytes.TrimSpace(responseBody)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(responseBody))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			if len(cookies) == 0 {
				return domain.CredentialPayload{}, nil, fmt.Errorf("decode credential refresh response: %w", err)
			}
		}
	}
	path := rule.TokenJSONPath
	if path == "" {
		path = "data.access_token"
	}
	token := scalarAtPath(root, path)
	if token == "" {
		token = firstScalar(root, "data.access_token", "access_token", "data.token", "token")
	}
	if token != "" {
		if err := setAccessToken(&payload, token); err != nil {
			return domain.CredentialPayload{}, nil, err
		}
	}
	refreshTokenPaths := []string{rule.RefreshTokenJSONPath, "data.refresh_token", "refresh_token"}
	for _, candidate := range refreshTokenPaths {
		if candidate == "" {
			continue
		}
		if value := scalarAtPath(root, candidate); value != "" {
			if payload.LocalStorage == nil {
				payload.LocalStorage = make(map[string]string)
			}
			payload.LocalStorage["refresh_token"] = value
			break
		}
	}
	if len(cookies) > 0 {
		payload.Cookie = mergeSetCookies(payload.Cookie, cookies)
	}
	if token == "" && len(cookies) == 0 {
		return domain.CredentialPayload{}, nil, fmt.Errorf("refresh response contains neither %s nor Set-Cookie", path)
	}
	expiresAt := expiryFromRefresh(root, rule, token, time.Now().UTC())
	if expiresAt == nil {
		expiresAt = credential.ExpiresAt
	}
	return payload, expiresAt, nil
}

// RefreshCapturedCandidate performs one same-origin refresh-token exchange for
// an unpersisted browser capture. It is used only after the first read-only
// validation proves the captured access token is expired, and the refreshed
// candidate must still pass the normal connector validator before storage.
func (r *Refresher) RefreshCapturedCandidate(ctx context.Context, supplier domain.Supplier,
	payload domain.CredentialPayload) (domain.CredentialPayload, *time.Time, error) {
	payload = NormalizeCapturedPayload(payload)
	_, _, view, supported := RecommendedLoginRule(supplier.Adapter)
	if !supported || view == nil || strings.TrimSpace(view.RefreshURL) == "" {
		return domain.CredentialPayload{}, nil, errors.New("captured session has no supported refresh contract")
	}
	refreshToken := firstCapturedValue(payload, "refresh_token")
	if refreshToken == "" {
		return domain.CredentialPayload{}, nil, errors.New("captured session contains no refresh token")
	}
	if payload.LocalStorage == nil {
		payload.LocalStorage = make(map[string]string)
	}
	payload.LocalStorage["refresh_token"] = refreshToken
	body, err := json.Marshal(map[string]string{view.RefreshTokenField: "${refresh_token}"})
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	payload.Refresh = &domain.RefreshRule{
		URL: view.RefreshURL, Method: view.RefreshMethod, Body: string(body),
		TokenJSONPath: view.RefreshResponseTokenPath, RefreshTokenJSONPath: view.RefreshResponseRefreshPath,
		ExpiresInJSONPath: view.RefreshResponseExpiresPath, ExpiresAtJSONPath: view.RefreshResponseExpiryPath,
	}
	credential := domain.Credential{SiteURL: supplier.BaseURL, Kind: "browser_pairing"}
	return r.refreshCandidate(ctx, credential, payload)
}

func expiryFromRefresh(root any, rule *domain.RefreshRule, token string, now time.Time) *time.Time {
	expiresAtPaths := []string{rule.ExpiresAtJSONPath, "data.expires_at", "expires_at"}
	for _, candidate := range expiresAtPaths {
		if candidate == "" {
			continue
		}
		if value := parseExpiryValue(scalarAtPath(root, candidate)); value != nil {
			return value
		}
	}
	expiresInPaths := []string{rule.ExpiresInJSONPath, "data.expires_in", "expires_in"}
	for _, candidate := range expiresInPaths {
		if candidate == "" {
			continue
		}
		if value := scalarAtPath(root, candidate); value != "" {
			if seconds, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && seconds > 0 {
				expires := now.Add(time.Duration(seconds) * time.Second)
				return &expires
			}
		}
	}
	return jwtExpiry(token)
}

func firstScalar(root any, paths ...string) string {
	for _, path := range paths {
		if value := scalarAtPath(root, path); value != "" {
			return value
		}
	}
	return ""
}

func (r *Refresher) recordFailure(ctx context.Context, credential domain.Credential, refreshErr error) {
	_ = r.store.MarkCredentialStatus(ctx, credential.ID, "refresh_failed")
	if credential.Status == "refresh_failed" {
		return
	}
	_ = r.store.AddActivity(ctx, domain.Activity{
		Kind: "credential_refresh_failed", Severity: "error", Title: "凭据刷新失败",
		Message:  credential.Name + "：" + refreshErr.Error(),
		Metadata: map[string]string{"credential_id": strconv.FormatInt(credential.ID, 10)},
	})
}

func (r *Refresher) credentialMetadata(ctx context.Context, id int64) (domain.Credential, bool) {
	items, err := r.store.ListCredentials(ctx)
	if err != nil {
		return domain.Credential{}, false
	}
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return domain.Credential{}, false
}

func safeRefreshURL(siteURL, raw string) (string, error) {
	base, err := url.Parse(strings.TrimRight(siteURL, "/"))
	if err != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return "", errors.New("credential site URL is invalid")
	}
	reference, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(reference)
	if err := netguard.RequireSameOrigin(base.String(), resolved.String()); err != nil {
		return "", errors.New("credential refresh URL must stay on the credential site origin")
	}
	return resolved.String(), nil
}

func expandRefreshBody(value string, payload domain.CredentialPayload) string {
	result := strings.ReplaceAll(value, "${authorization}", payload.Authorization)
	result = strings.ReplaceAll(result, "${access_token}", tokenFromPayload(payload))
	result = strings.ReplaceAll(result, "${refresh_token}", payload.LocalStorage["refresh_token"])
	return result
}

func scalarAtPath(root any, path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")
	current := root
	for _, part := range strings.Split(path, ".") {
		part = strings.Trim(part, "'[]\"")
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[part]
		if !ok {
			return ""
		}
	}
	switch value := current.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func mergeSetCookies(existing string, cookies []*http.Cookie) string {
	values := make(map[string]string)
	for _, item := range strings.Split(existing, ";") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) == 2 {
			values[parts[0]] = parts[1]
		}
	}
	for _, cookie := range cookies {
		values[cookie.Name] = cookie.Value
	}
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+"="+value)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
