package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

// CandidateValidator must return an operator-safe error that contains no
// credential material. Its reason is surfaced when a replacement is rejected.
type CandidateValidator func(context.Context, domain.Supplier, domain.Credential, domain.CredentialPayload) error

func (r *Refresher) WithCandidateValidator(validator CandidateValidator) *Refresher {
	r.validator = validator
	return r
}

func (r *Refresher) WithLoginPolicy(policy LoginPolicy) *Refresher {
	r.loginPolicy = defaultLoginPolicy(policy)
	return r
}

func (r *Refresher) passwordRenew(ctx context.Context, credential domain.Credential, payload domain.CredentialPayload) error {
	if payload.Login == nil || !payload.Login.Enabled {
		return newRenewalError("login_not_configured", "configuration_required",
			"automatic password login is not enabled for this credential", "configure_automatic_login")
	}
	rule, err := normalizeLoginRule(*payload.Login, r.loginPolicy)
	if err != nil {
		renewalErr := newRenewalError("invalid_login_rule", "configuration_required",
			"automatic login configuration is invalid", "review_login_rule")
		r.recordRenewalFailure(ctx, credential, rule, renewalErr)
		return renewalErr
	}
	if strings.EqualFold(credential.Kind, "browser_pairing") && !rule.AllowReplaceBrowserPairing {
		renewalErr := newRenewalError("replacement_not_allowed", "permission_denied",
			"automatic login is not allowed to replace this browser-paired credential", "allow_browser_pairing_replacement")
		r.recordRenewalFailure(ctx, credential, rule, renewalErr)
		return renewalErr
	}
	now := time.Now().UTC()
	current, _ := r.store.GetCredentialRenewal(ctx, credential.ID)
	current.Configured = true
	current.Enabled = true
	current.State = "password_login_attempt"
	current.LastAttemptAt = &now
	current.NextAttemptAt = nil
	current.HumanAction = ""
	current.LastErrorClass = ""
	_ = r.store.SetCredentialRenewalState(ctx, credential.ID, current)

	candidate, expiresAt, loginErr := r.performPasswordLogin(ctx, credential, payload, rule)
	if loginErr != nil {
		r.recordRenewalFailure(ctx, credential, rule, loginErr)
		return loginErr
	}
	if validationErr := r.validateCandidate(ctx, credential, candidate); validationErr != nil {
		renewalErr := newRenewalError("permission_denied", "permission_denied",
			"the renewed session did not pass the required read-only capability checks: "+validationErr.Error(),
			"grant_required_upstream_role")
		r.recordRenewalFailure(ctx, credential, rule, renewalErr)
		return renewalErr
	}
	success := current
	success.State = "healthy"
	success.ConsecutiveFailures = 0
	success.LastSuccessAt = &now
	nextAttempt := now.Add(time.Duration(rule.MinimumIntervalSeconds) * time.Second)
	success.NextAttemptAt = &nextAttempt
	success.HumanAction = ""
	success.LastErrorClass = ""
	if err := r.service.CommitRenewal(ctx, credential, candidate, expiresAt, success); err != nil {
		if errors.Is(err, store.ErrCredentialChanged) {
			// The competing writer owns both the newer encrypted payload and its
			// lifecycle state. Do not overwrite that state with this stale result.
			return newRenewalError("concurrent_change", "retry_backoff",
				"the credential changed while renewal was being verified; the newer value was preserved", "retry_manual_renewal")
		}
		renewalErr := newRenewalError("persistence_error", "retry_backoff",
			"the renewed credential could not be committed", "retry_manual_renewal")
		r.recordRenewalFailure(ctx, credential, rule, renewalErr)
		return renewalErr
	}
	_ = r.store.AddActivity(ctx, domain.Activity{Kind: "credential_login_renewed", Severity: "success",
		Title: "自动登录续约成功", Message: credential.Name + " 已完成自动登录，并通过必需的只读能力校验",
		Metadata: map[string]string{"credential_id": strconv.FormatInt(credential.ID, 10)}})
	return nil
}

func (r *Refresher) performPasswordLogin(ctx context.Context, credential domain.Credential, payload domain.CredentialPayload,
	rule domain.LoginRule) (domain.CredentialPayload, *time.Time, error) {
	if err := r.remoteWrites.Check("supplier password login"); err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	requestURL, err := safeRefreshURL(credential.SiteURL, rule.URL)
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("cross_origin_blocked", "configuration_required",
			"the configured login URL must stay on the credential origin", "review_login_rule")
	}
	client, err := netguard.SameOriginClient(r.client, credential.SiteURL)
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("invalid_origin", "configuration_required",
			"the credential origin is invalid", "review_login_rule")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("internal_error", "retry_backoff",
			"automatic login could not initialize a temporary session", "retry_manual_renewal")
	}
	client.Jar = jar
	body, err := loginRequestBody(rule)
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("invalid_login_rule", "configuration_required",
			"automatic login request mapping is invalid", "review_login_rule")
	}
	req, err := http.NewRequestWithContext(ctx, rule.Method, requestURL, bytes.NewReader(body))
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("invalid_login_rule", "configuration_required",
			"automatic login request could not be created", "review_login_rule")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", rule.ContentType)
	userAgent := netguard.ControlPlaneUserAgent
	if configured := strings.TrimSpace(payload.Headers["User-Agent"]); configured != "" && !strings.ContainsAny(configured, "\r\n") {
		userAgent = configured
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("network_error", "retry_backoff",
			"automatic login could not reach the configured upstream", "check_upstream_connectivity")
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return domain.CredentialPayload{}, nil, newRenewalError("network_error", "retry_backoff",
			"automatic login response could not be read", "check_upstream_connectivity")
	}
	var root any
	if len(bytes.TrimSpace(responseBody)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(responseBody))
		decoder.UseNumber()
		if err := decoder.Decode(&root); err != nil {
			return domain.CredentialPayload{}, nil, newRenewalError("invalid_login_response", "retry_backoff",
				"automatic login returned an unsupported response", "review_login_rule")
		}
	}
	message := firstScalar(root, rule.MessageJSONPath, "data.message", "message")
	if loginNeedsHumanAction(root, rule, message) {
		return domain.CredentialPayload{}, nil, newRenewalError("captcha_or_2fa", "human_action_required",
			"the upstream requires a captcha or two-factor verification", "complete_captcha_or_two_factor_login")
	}
	credentialsRejected := loginExplicitlyRejectsCredentials(message)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if credentialsRejected {
			return domain.CredentialPayload{}, nil, newRenewalError("bad_credentials", "suspended_backoff",
				"the upstream explicitly rejected the configured username or password", "check_username_and_password")
		}
		return domain.CredentialPayload{}, nil, newRenewalError("login_rejected", "human_action_required",
			"the upstream rejected this login session without identifying invalid credentials", "complete_browser_login")
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return domain.CredentialPayload{}, nil, newRenewalError("network_error", "retry_backoff",
			"the upstream login service is temporarily unavailable", "check_upstream_connectivity")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if credentialsRejected {
			return domain.CredentialPayload{}, nil, newRenewalError("bad_credentials", "suspended_backoff",
				"the upstream explicitly rejected the configured username or password", "check_username_and_password")
		}
		return domain.CredentialPayload{}, nil, newRenewalError("login_rejected", "human_action_required",
			"the upstream rejected this automatic login request", "complete_browser_login")
	}
	if success, known := loginResponseSucceeded(root, rule); known && !success {
		if credentialsRejected {
			return domain.CredentialPayload{}, nil, newRenewalError("bad_credentials", "suspended_backoff",
				"the upstream explicitly rejected the configured username or password", "check_username_and_password")
		}
		return domain.CredentialPayload{}, nil, newRenewalError("login_rejected", "human_action_required",
			"the upstream rejected this automatic login request", "complete_browser_login")
	}

	candidate := cloneCredentialPayload(payload)
	if candidate.Headers == nil {
		candidate.Headers = make(map[string]string)
	}
	// Preserve the identity that received the token so validation, collection,
	// and later refreshes cannot silently switch to a different client identity.
	candidate.Headers["User-Agent"] = userAgent
	candidate.Login = &rule
	installLoginRefreshRule(&candidate, root, rule)
	token := firstScalar(root, rule.TokenJSONPath, "data.access_token", "data.token", "access_token", "token")
	if token != "" {
		if err := setAccessToken(&candidate, token); err != nil {
			return domain.CredentialPayload{}, nil, newRenewalError("invalid_login_response", "configuration_required",
				"the returned access token could not be normalized", "review_login_rule")
		}
	}
	if userID := firstScalar(root, rule.UserIDJSONPath, "data.user.id", "data.user_id", "data.id", "user.id", "user_id", "id"); userID != "" {
		if parsed, parseErr := strconv.ParseInt(userID, 10, 64); parseErr == nil && parsed > 0 {
			candidate.UserID = parsed
		}
	}
	cookieURL := resp.Request.URL
	if cookieURL == nil {
		cookieURL, _ = url.Parse(strings.TrimRight(credential.SiteURL, "/"))
	}
	responseCookies := jar.Cookies(cookieURL)
	// The login uses a clean temporary cookie jar. Its result replaces the old
	// session cookie set; otherwise some forks prefer the stale cookie over the
	// fresh token and reject an otherwise valid renewal candidate.
	candidate.Cookie = ""
	if rule.CookieResponse {
		candidate.Cookie = mergeSetCookies("", responseCookies)
	}
	candidate.CapturedOrigin = strings.TrimRight(credential.SiteURL, "/")
	// A successful HTTP response is not enough: the current login attempt must
	// produce fresh reusable material. An old cookie copied from payload must
	// never make an empty login response look like a successful renewal.
	if token == "" && (!rule.CookieResponse || len(responseCookies) == 0) {
		return domain.CredentialPayload{}, nil, newRenewalError("insufficient_login_material", "configuration_required",
			"automatic login succeeded but returned no reusable token or cookie", "review_login_rule")
	}
	expiresAt := expiryFromLogin(root, rule, token, time.Now().UTC())
	if expiresAt == nil {
		expiresAt = credential.ExpiresAt
	}
	return candidate, expiresAt, nil
}

func loginResponseSucceeded(root any, rule domain.LoginRule) (bool, bool) {
	if rule.SuccessValue != "" {
		value := valueAtPath(root, rule.SuccessJSONPath)
		if value == nil {
			return false, false
		}
		return strings.TrimSpace(fmt.Sprint(value)) == rule.SuccessValue, true
	}
	return booleanAtPath(root, rule.SuccessJSONPath)
}

func installLoginRefreshRule(candidate *domain.CredentialPayload, root any, rule domain.LoginRule) {
	if candidate == nil || rule.RefreshURL == "" {
		return
	}
	// This refresh contract belongs to the new login session. A stale refresh
	// token from the replaced session must not survive a successful login.
	candidate.Refresh = nil
	if candidate.LocalStorage != nil {
		delete(candidate.LocalStorage, "refresh_token")
	}
	refreshToken := firstScalar(root, rule.LoginRefreshTokenJSONPath, "data.refresh_token", "refresh_token")
	if refreshToken == "" {
		return
	}
	if candidate.LocalStorage == nil {
		candidate.LocalStorage = make(map[string]string)
	}
	candidate.LocalStorage["refresh_token"] = refreshToken
	body, _ := json.Marshal(map[string]string{rule.RefreshTokenField: "${refresh_token}"})
	candidate.Refresh = &domain.RefreshRule{
		URL: rule.RefreshURL, Method: rule.RefreshMethod, Body: string(body),
		TokenJSONPath: rule.RefreshResponseTokenPath, RefreshTokenJSONPath: rule.RefreshResponseRefreshPath,
		ExpiresInJSONPath: rule.RefreshResponseExpiresPath, ExpiresAtJSONPath: rule.RefreshResponseExpiryPath,
	}
}

func loginRequestBody(rule domain.LoginRule) ([]byte, error) {
	if rule.ContentType == "application/x-www-form-urlencoded" {
		values := url.Values{}
		values.Set(rule.UsernameField, rule.Username)
		values.Set(rule.PasswordField, rule.Password)
		return []byte(values.Encode()), nil
	}
	root := map[string]any{}
	if err := setMappedField(root, rule.UsernameField, rule.Username); err != nil {
		return nil, err
	}
	if err := setMappedField(root, rule.PasswordField, rule.Password); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

func setMappedField(root map[string]any, path, value string) error {
	parts := strings.Split(path, ".")
	current := root
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if !exists {
			child := map[string]any{}
			current[part] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("conflicting login field mapping")
		}
		current = child
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func loginNeedsHumanAction(root any, rule domain.LoginRule, message string) bool {
	for _, path := range []string{rule.CaptchaJSONPath, rule.TwoFactorJSONPath, "data.require_2fa", "require_2fa",
		"data.two_factor_required", "two_factor_required", "data.mfa_required", "mfa_required",
		"data.captcha_required", "captcha_required", "data.need_captcha", "need_captcha",
		"data.turnstile_required", "turnstile_required", "data.verification_required", "verification_required"} {
		if path != "" {
			if value, known := booleanAtPath(root, path); known && value {
				return true
			}
		}
	}
	normalized := strings.ToLower(message)
	for _, marker := range []string{"captcha", "turnstile", "two-factor", "two factor", "2fa", "mfa",
		"验证码", "人机验证", "二次验证", "二步验证", "两步验证", "短信验证", "邮箱验证", "动态口令"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func loginExplicitlyRejectsCredentials(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	for _, marker := range []string{
		"invalid password", "incorrect password", "wrong password", "password is incorrect", "password error",
		"invalid credentials", "bad credentials", "invalid username", "incorrect username", "user not found",
		"username or password", "email or password", "account or password",
		"密码错误", "密码不正确", "密码无效", "账号或密码", "帐户或密码", "用户名或密码", "邮箱或密码",
		"用户不存在", "账号不存在", "帐户不存在",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func booleanAtPath(root any, path string) (bool, bool) {
	value := valueAtPath(root, path)
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	case json.Number:
		return typed.String() != "0", true
	case float64:
		return typed != 0, true
	default:
		return false, false
	}
}

func valueAtPath(root any, path string) any {
	path = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(path, "$"), "."))
	if path == "" {
		return nil
	}
	current := root
	for _, part := range strings.Split(path, ".") {
		part = strings.Trim(part, "'[]\"")
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[part]
		if !ok {
			return nil
		}
	}
	return current
}

func expiryFromLogin(root any, rule domain.LoginRule, token string, now time.Time) *time.Time {
	refreshRule := &domain.RefreshRule{ExpiresAtJSONPath: rule.ExpiresAtJSONPath, ExpiresInJSONPath: rule.ExpiresInJSONPath}
	return expiryFromRefresh(root, refreshRule, token, now)
}

func cloneCredentialPayload(payload domain.CredentialPayload) domain.CredentialPayload {
	clone := payload
	clone.Headers = cloneStrings(payload.Headers)
	clone.LocalStorage = cloneStrings(payload.LocalStorage)
	clone.SessionStorage = cloneStrings(payload.SessionStorage)
	if payload.Login != nil {
		rule := *payload.Login
		clone.Login = &rule
	}
	if payload.Refresh != nil {
		rule := *payload.Refresh
		rule.Headers = cloneStrings(payload.Refresh.Headers)
		clone.Refresh = &rule
	}
	return clone
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func (r *Refresher) validateCandidate(ctx context.Context, credential domain.Credential, payload domain.CredentialPayload) error {
	if r.validator == nil {
		return fmt.Errorf("credential candidate validator is unavailable")
	}
	suppliers, err := r.store.SuppliersUsingCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if len(suppliers) == 0 {
		return fmt.Errorf("credential is not bound to an upstream supplier")
	}
	for _, supplier := range suppliers {
		if err := r.validator(ctx, supplier, credential, payload); err != nil {
			return fmt.Errorf("candidate did not satisfy required capability: %w", err)
		}
	}
	return nil
}

func (r *Refresher) recordRenewalFailure(ctx context.Context, credential domain.Credential, rule domain.LoginRule, failure error) {
	now := time.Now().UTC()
	current, _ := r.store.GetCredentialRenewal(ctx, credential.ID)
	previousState, previousClass := current.State, current.LastErrorClass
	current.Configured = true
	current.Enabled = rule.Enabled
	current.LastAttemptAt = &now
	current.ConsecutiveFailures++
	current.HumanAction = ""
	current.NextAttemptAt = nil
	var typed *renewalError
	if !errorsAsRenewal(failure, &typed) {
		typed = &renewalError{class: "network_error", state: "retry_backoff", message: "automatic credential renewal failed", humanAction: "retry_manual_renewal"}
	}
	current.State = typed.state
	current.LastErrorClass = typed.class
	current.HumanAction = typed.humanAction
	if typed.state == "human_action_required" {
		next := now.Add(time.Duration(rule.HumanActionCooldownSeconds) * time.Second)
		current.NextAttemptAt = &next
	} else if typed.state == "retry_backoff" {
		// Transport failures and upstream 5xx responses are recoverable without
		// operator action. Keep retrying at the bounded maximum interval instead
		// of silently turning a temporary outage into permanent credential loss.
		delay := boundedBackoff(rule, current.ConsecutiveFailures)
		next := now.Add(delay)
		current.NextAttemptAt = &next
	} else if typed.state == "suspended_backoff" {
		// Credential rejection can lock an account when replayed forever. It is
		// intentionally terminal after the configured number of attempts.
		if current.ConsecutiveFailures >= rule.MaximumConsecutiveFailures {
			current.NextAttemptAt = nil
			if current.HumanAction == "" {
				current.HumanAction = "review_renewal_failures"
			}
		} else {
			delay := boundedBackoff(rule, current.ConsecutiveFailures)
			next := now.Add(delay)
			current.NextAttemptAt = &next
		}
	}
	_ = r.store.SetCredentialRenewalState(ctx, credential.ID, current)
	_ = r.store.MarkCredentialStatus(ctx, credential.ID, current.State)
	if previousState == current.State && previousClass == current.LastErrorClass {
		return
	}
	_ = r.store.AddActivity(ctx, domain.Activity{Kind: "credential_login_attention", Severity: "warning",
		Title: "自动登录续约需要关注", Message: credential.Name + "：" + renewalFailureActivityMessage(typed.class),
		Metadata: map[string]string{"credential_id": strconv.FormatInt(credential.ID, 10), "error_class": typed.class,
			"state": current.State, "human_action": current.HumanAction}})
}

func renewalFailureActivityMessage(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "bad_credentials":
		return "账号或密码被上游拒绝，请核对保存的登录信息"
	case "captcha_or_2fa":
		return "上游要求完成人机验证或二次验证"
	case "login_rejected":
		return "上游拒绝了当前自动登录会话，请在浏览器完成登录或验证后重新配对"
	case "permission_denied":
		return "续约后的会话未通过必需的只读能力校验"
	case "network_error", "upstream_unavailable":
		return "连接上游失败，系统会按退避策略自动重试"
	case "configuration_required", "invalid_login_rule":
		return "自动登录规则需要补充或修正"
	case "replacement_not_allowed":
		return "当前凭据不允许由账号密码自动替换"
	default:
		return "自动续约未完成，系统已按当前策略安排后续处理"
	}
}

func errorsAsRenewal(err error, target **renewalError) bool {
	typed, ok := err.(*renewalError)
	if ok {
		*target = typed
	}
	return ok
}

func boundedBackoff(rule domain.LoginRule, failures int) time.Duration {
	delay := time.Duration(rule.InitialBackoffSeconds) * time.Second
	maximum := time.Duration(rule.MaximumBackoffSeconds) * time.Second
	for step := 1; step < failures && delay < maximum; step++ {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	return delay
}
