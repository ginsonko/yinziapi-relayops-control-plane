package credentials

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// LoginRuleFromView adds the account fields to a public declarative rule and
// runs the same validation used by automatic renewal.
func LoginRuleFromView(view domain.CredentialLoginRuleView, username, password string, enabled bool) (domain.LoginRule, error) {
	rule := domain.LoginRule{
		Enabled: enabled, Username: strings.TrimSpace(username), Password: password,
		URL: view.URL, Method: view.Method, ContentType: view.ContentType,
		UsernameField: view.UsernameField, PasswordField: view.PasswordField,
		TokenJSONPath: view.TokenJSONPath, UserIDJSONPath: view.UserIDJSONPath,
		ExpiresInJSONPath: view.ExpiresInJSONPath, ExpiresAtJSONPath: view.ExpiresAtJSONPath,
		SuccessJSONPath: view.SuccessJSONPath, SuccessValue: view.SuccessValue, MessageJSONPath: view.MessageJSONPath,
		LoginRefreshTokenJSONPath: view.LoginRefreshTokenJSONPath, RefreshURL: view.RefreshURL,
		RefreshMethod: view.RefreshMethod, RefreshTokenField: view.RefreshTokenField,
		RefreshResponseTokenPath: view.RefreshResponseTokenPath, RefreshResponseRefreshPath: view.RefreshResponseRefreshPath,
		RefreshResponseExpiresPath: view.RefreshResponseExpiresPath, RefreshResponseExpiryPath: view.RefreshResponseExpiryPath,
		CaptchaJSONPath: view.CaptchaJSONPath, TwoFactorJSONPath: view.TwoFactorJSONPath,
		CookieResponse: view.CookieResponse, MinimumIntervalSeconds: view.MinimumIntervalSeconds,
		InitialBackoffSeconds: view.InitialBackoffSeconds, MaximumBackoffSeconds: view.MaximumBackoffSeconds,
		MaximumConsecutiveFailures: view.MaximumConsecutiveFailures,
		HumanActionCooldownSeconds: view.HumanActionCooldownSeconds,
		AllowReplaceBrowserPairing: view.AllowReplaceBrowserPairing,
	}
	if rule.Username == "" || rule.Password == "" {
		return domain.LoginRule{}, errors.New("username and password are required")
	}
	return normalizeLoginRule(rule, LoginPolicy{})
}

// PasswordLoginCandidate performs an ephemeral, same-origin HTTP login. It
// does not persist anything; callers must validate read-only capabilities
// before committing the candidate.
func (r *Refresher) PasswordLoginCandidate(ctx context.Context, siteURL, username, password string,
	view domain.CredentialLoginRuleView, enableRenewal bool) (domain.CredentialPayload, *time.Time, error) {
	rule, err := LoginRuleFromView(view, username, password, enableRenewal)
	if err != nil {
		return domain.CredentialPayload{}, nil, err
	}
	credential := domain.Credential{SiteURL: strings.TrimRight(strings.TrimSpace(siteURL), "/"), Kind: "password_login"}
	payload := domain.CredentialPayload{CapturedOrigin: credential.SiteURL, Login: &rule}
	return r.performPasswordLogin(ctx, credential, payload, rule)
}
