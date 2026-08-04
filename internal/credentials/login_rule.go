package credentials

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type LoginPolicy struct {
	MinimumInterval     time.Duration
	InitialBackoff      time.Duration
	MaximumBackoff      time.Duration
	MaximumFailures     int
	HumanActionCooldown time.Duration
}

var (
	loginFieldPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	jsonPathPattern   = regexp.MustCompile(`^\$?\.?[A-Za-z0-9_'"\[\].-]{0,192}$`)
)

func defaultLoginPolicy(policy LoginPolicy) LoginPolicy {
	if policy.MinimumInterval <= 0 {
		policy.MinimumInterval = 15 * time.Minute
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = time.Minute
	}
	if policy.MaximumBackoff <= 0 {
		policy.MaximumBackoff = time.Hour
	}
	if policy.MaximumBackoff < policy.InitialBackoff {
		policy.MaximumBackoff = policy.InitialBackoff
	}
	if policy.MaximumFailures <= 0 {
		policy.MaximumFailures = 5
	}
	if policy.HumanActionCooldown <= 0 {
		policy.HumanActionCooldown = 12 * time.Hour
	}
	return policy
}

func normalizeLoginRule(rule domain.LoginRule, policy LoginPolicy) (domain.LoginRule, error) {
	policy = defaultLoginPolicy(policy)
	rule.Username = strings.TrimSpace(rule.Username)
	rule.URL = strings.TrimSpace(rule.URL)
	if rule.URL == "" {
		rule.URL = "/api/user/login"
	}
	rule.Method = strings.ToUpper(strings.TrimSpace(rule.Method))
	if rule.Method == "" {
		rule.Method = http.MethodPost
	}
	if rule.Method != http.MethodPost && rule.Method != http.MethodPut {
		return domain.LoginRule{}, errors.New("login method must be POST or PUT")
	}
	rule.ContentType = strings.ToLower(strings.TrimSpace(strings.Split(rule.ContentType, ";")[0]))
	if rule.ContentType == "" {
		rule.ContentType = "application/json"
	}
	if rule.ContentType != "application/json" && rule.ContentType != "application/x-www-form-urlencoded" {
		return domain.LoginRule{}, errors.New("login content_type must be application/json or application/x-www-form-urlencoded")
	}
	if rule.UsernameField == "" {
		rule.UsernameField = "username"
	}
	if rule.PasswordField == "" {
		rule.PasswordField = "password"
	}
	if !loginFieldPattern.MatchString(rule.UsernameField) || !loginFieldPattern.MatchString(rule.PasswordField) {
		return domain.LoginRule{}, errors.New("login username/password field mapping is invalid")
	}
	if rule.UsernameField == rule.PasswordField {
		return domain.LoginRule{}, errors.New("login username and password fields must be different")
	}
	if rule.Enabled && (rule.Username == "" || rule.Password == "") {
		return domain.LoginRule{}, errors.New("enabled automatic login requires a username and password")
	}
	rule.RefreshURL = strings.TrimSpace(rule.RefreshURL)
	if rule.RefreshURL == "" {
		// An empty refresh URL explicitly disables token refresh. Frontends keep
		// draft field values while the toggle is off, so discard every dependent
		// mapping instead of rejecting otherwise valid password re-login.
		rule.LoginRefreshTokenJSONPath = ""
		rule.RefreshMethod = ""
		rule.RefreshTokenField = ""
		rule.RefreshResponseTokenPath = ""
		rule.RefreshResponseRefreshPath = ""
		rule.RefreshResponseExpiresPath = ""
		rule.RefreshResponseExpiryPath = ""
	}
	paths := []*string{&rule.TokenJSONPath, &rule.UserIDJSONPath, &rule.ExpiresInJSONPath, &rule.ExpiresAtJSONPath,
		&rule.SuccessJSONPath, &rule.MessageJSONPath, &rule.LoginRefreshTokenJSONPath,
		&rule.RefreshResponseTokenPath, &rule.RefreshResponseRefreshPath, &rule.RefreshResponseExpiresPath,
		&rule.RefreshResponseExpiryPath, &rule.CaptchaJSONPath, &rule.TwoFactorJSONPath}
	for _, path := range paths {
		*path = strings.TrimSpace(*path)
		if *path != "" && !jsonPathPattern.MatchString(*path) {
			return domain.LoginRule{}, fmt.Errorf("login response path %q is invalid", *path)
		}
	}
	if rule.SuccessJSONPath == "" {
		rule.SuccessJSONPath = "success"
	}
	rule.SuccessValue = strings.TrimSpace(rule.SuccessValue)
	if len(rule.SuccessValue) > 128 || strings.ContainsAny(rule.SuccessValue, "\r\n") {
		return domain.LoginRule{}, errors.New("login success_value is invalid")
	}
	if rule.MessageJSONPath == "" {
		rule.MessageJSONPath = "message"
	}
	rule.RefreshMethod = strings.ToUpper(strings.TrimSpace(rule.RefreshMethod))
	rule.RefreshTokenField = strings.TrimSpace(rule.RefreshTokenField)
	if rule.RefreshURL != "" {
		parsed, err := url.Parse(rule.RefreshURL)
		if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Path == "" || strings.ContainsAny(rule.RefreshURL, "\r\n") {
			return domain.LoginRule{}, errors.New("login refresh_url must be a same-origin relative path")
		}
		if rule.RefreshMethod == "" {
			rule.RefreshMethod = http.MethodPost
		}
		if rule.RefreshMethod != http.MethodPost {
			return domain.LoginRule{}, errors.New("login refresh_method must be POST")
		}
		if rule.RefreshTokenField == "" {
			rule.RefreshTokenField = "refresh_token"
		}
		if !loginFieldPattern.MatchString(rule.RefreshTokenField) {
			return domain.LoginRule{}, errors.New("login refresh_token_field is invalid")
		}
		if rule.LoginRefreshTokenJSONPath == "" {
			rule.LoginRefreshTokenJSONPath = "data.refresh_token"
		}
		if rule.RefreshResponseTokenPath == "" {
			rule.RefreshResponseTokenPath = "data.access_token"
		}
		if rule.RefreshResponseRefreshPath == "" {
			rule.RefreshResponseRefreshPath = "data.refresh_token"
		}
		if rule.RefreshResponseExpiresPath == "" {
			rule.RefreshResponseExpiresPath = "data.expires_in"
		}
	}
	if rule.UserIDJSONPath == "" {
		rule.UserIDJSONPath = "data.id"
	}
	if !rule.CookieResponse && rule.TokenJSONPath == "" {
		// NewAPI commonly authenticates with a reusable session cookie. Token
		// extraction remains available as a fallback even with this default.
		rule.CookieResponse = true
	}
	rule.MinimumIntervalSeconds = durationSeconds(rule.MinimumIntervalSeconds, policy.MinimumInterval, 10*time.Second, 7*24*time.Hour)
	rule.InitialBackoffSeconds = durationSeconds(rule.InitialBackoffSeconds, policy.InitialBackoff, time.Second, 24*time.Hour)
	rule.MaximumBackoffSeconds = durationSeconds(rule.MaximumBackoffSeconds, policy.MaximumBackoff,
		time.Duration(rule.InitialBackoffSeconds)*time.Second, 7*24*time.Hour)
	if rule.MaximumConsecutiveFailures <= 0 {
		rule.MaximumConsecutiveFailures = policy.MaximumFailures
	}
	if rule.MaximumConsecutiveFailures < 1 || rule.MaximumConsecutiveFailures > 20 {
		return domain.LoginRule{}, errors.New("maximum_consecutive_failures must be between 1 and 20")
	}
	rule.HumanActionCooldownSeconds = durationSeconds(rule.HumanActionCooldownSeconds, policy.HumanActionCooldown,
		time.Minute, 30*24*time.Hour)
	return rule, nil
}

func durationSeconds(value int64, fallback, minimum, maximum time.Duration) int64 {
	duration := time.Duration(value) * time.Second
	if value <= 0 {
		duration = fallback
	}
	if duration < minimum {
		duration = minimum
	}
	if duration > maximum {
		duration = maximum
	}
	return int64(duration / time.Second)
}

func loginRuleView(rule domain.LoginRule) *domain.CredentialLoginRuleView {
	return &domain.CredentialLoginRuleView{
		URL: rule.URL, Method: rule.Method, ContentType: rule.ContentType, UsernameField: rule.UsernameField,
		PasswordField: rule.PasswordField, TokenJSONPath: rule.TokenJSONPath, UserIDJSONPath: rule.UserIDJSONPath,
		ExpiresInJSONPath: rule.ExpiresInJSONPath, ExpiresAtJSONPath: rule.ExpiresAtJSONPath,
		SuccessJSONPath: rule.SuccessJSONPath, SuccessValue: rule.SuccessValue, MessageJSONPath: rule.MessageJSONPath,
		LoginRefreshTokenJSONPath: rule.LoginRefreshTokenJSONPath, RefreshURL: rule.RefreshURL,
		RefreshMethod: rule.RefreshMethod, RefreshTokenField: rule.RefreshTokenField,
		RefreshResponseTokenPath: rule.RefreshResponseTokenPath, RefreshResponseRefreshPath: rule.RefreshResponseRefreshPath,
		RefreshResponseExpiresPath: rule.RefreshResponseExpiresPath, RefreshResponseExpiryPath: rule.RefreshResponseExpiryPath,
		CaptchaJSONPath:   rule.CaptchaJSONPath,
		TwoFactorJSONPath: rule.TwoFactorJSONPath, CookieResponse: rule.CookieResponse,
		MinimumIntervalSeconds: rule.MinimumIntervalSeconds, InitialBackoffSeconds: rule.InitialBackoffSeconds,
		MaximumBackoffSeconds: rule.MaximumBackoffSeconds, MaximumConsecutiveFailures: rule.MaximumConsecutiveFailures,
		HumanActionCooldownSeconds: rule.HumanActionCooldownSeconds, AllowReplaceBrowserPairing: rule.AllowReplaceBrowserPairing,
	}
}

type renewalError struct {
	class       string
	state       string
	message     string
	humanAction string
}

func (e *renewalError) Error() string { return e.message }

func RenewalErrorClass(err error) string {
	var typed *renewalError
	if errors.As(err, &typed) {
		return typed.class
	}
	return ""
}

func newRenewalError(class, state, message, humanAction string) error {
	return &renewalError{class: class, state: state, message: message, humanAction: humanAction}
}
