package credentials

import (
	"net/http"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// RecommendedLoginRule returns a validated, secret-free login mapping for a
// native adapter. Unknown and custom adapters deliberately have no preset.
func RecommendedLoginRule(adapter string) (string, string, *domain.CredentialLoginRuleView, bool) {
	adapter = strings.ToLower(strings.TrimSpace(adapter))
	var family, label string
	var rule domain.LoginRule
	switch adapter {
	case "newapi", "newapi-native":
		family, label = "newapi", "NewAPI 官方登录契约"
		rule = domain.LoginRule{
			URL: "/api/user/login", Method: http.MethodPost, ContentType: "application/json",
			UsernameField: "username", PasswordField: "password", UserIDJSONPath: "data.id",
			SuccessJSONPath: "success", MessageJSONPath: "message", CookieResponse: true,
		}
	case "sub2api", "sub2api_admin", "sub2api_upstream":
		family, label = "sub2api", "Sub2API 官方登录契约"
		rule = domain.LoginRule{
			URL: "/api/v1/auth/login", Method: http.MethodPost, ContentType: "application/json",
			UsernameField: "email", PasswordField: "password", TokenJSONPath: "data.access_token",
			UserIDJSONPath: "data.user.id", ExpiresInJSONPath: "data.expires_in",
			SuccessJSONPath: "code", SuccessValue: "0", MessageJSONPath: "message",
			TwoFactorJSONPath: "data.requires_2fa", LoginRefreshTokenJSONPath: "data.refresh_token",
			RefreshURL: "/api/v1/auth/refresh", RefreshMethod: http.MethodPost, RefreshTokenField: "refresh_token",
			RefreshResponseTokenPath: "data.access_token", RefreshResponseRefreshPath: "data.refresh_token",
			RefreshResponseExpiresPath: "data.expires_in",
		}
	default:
		return "", "", nil, false
	}
	normalized, err := normalizeLoginRule(rule, LoginPolicy{})
	if err != nil {
		return family, label, nil, false
	}
	return family, label, loginRuleView(normalized), true
}
