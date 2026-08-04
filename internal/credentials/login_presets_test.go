package credentials

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestRecommendedLoginRuleCoversNativeAdapterAliases(t *testing.T) {
	tests := []struct {
		adapter, family, path, usernameField, tokenPath, successPath, successValue string
		cookie                                                                     bool
	}{
		{adapter: "newapi", family: "newapi", path: "/api/user/login", usernameField: "username", successPath: "success", cookie: true},
		{adapter: "newapi-native", family: "newapi", path: "/api/user/login", usernameField: "username", successPath: "success", cookie: true},
		{adapter: "sub2api", family: "sub2api", path: "/api/v1/auth/login", usernameField: "email", tokenPath: "data.access_token", successPath: "code", successValue: "0"},
		{adapter: "sub2api_admin", family: "sub2api", path: "/api/v1/auth/login", usernameField: "email", tokenPath: "data.access_token", successPath: "code", successValue: "0"},
		{adapter: "sub2api_upstream", family: "sub2api", path: "/api/v1/auth/login", usernameField: "email", tokenPath: "data.access_token", successPath: "code", successValue: "0"},
	}
	for _, test := range tests {
		t.Run(test.adapter, func(t *testing.T) {
			family, label, rule, ok := RecommendedLoginRule(test.adapter)
			if !ok || rule == nil || family != test.family || label == "" {
				t.Fatalf("preset family=%q label=%q rule=%#v ok=%v", family, label, rule, ok)
			}
			if rule.URL != test.path || rule.UsernameField != test.usernameField || rule.PasswordField != "password" ||
				rule.TokenJSONPath != test.tokenPath || rule.SuccessJSONPath != test.successPath ||
				rule.SuccessValue != test.successValue || rule.CookieResponse != test.cookie {
				t.Fatalf("unexpected preset: %#v", rule)
			}
			if test.family == "sub2api" && (rule.LoginRefreshTokenJSONPath != "data.refresh_token" ||
				rule.RefreshURL != "/api/v1/auth/refresh" || rule.RefreshTokenField != "refresh_token" ||
				rule.RefreshResponseTokenPath != "data.access_token" || rule.RefreshResponseRefreshPath != "data.refresh_token" ||
				rule.RefreshResponseExpiresPath != "data.expires_in") {
				t.Fatalf("Sub2 refresh preset: %#v", rule)
			}
			encoded, err := json.Marshal(rule)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{`"username":`, `"password":`, `"authorization":`, `"cookie":`} {
				if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
					t.Fatalf("preset exposed secret-bearing field %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestRecommendedLoginRuleRejectsUnknownAdapter(t *testing.T) {
	if family, label, rule, ok := RecommendedLoginRule("json"); ok || family != "" || label != "" || rule != nil {
		t.Fatalf("unknown adapter returned family=%q label=%q rule=%#v ok=%v", family, label, rule, ok)
	}
}

func TestLoginResponseSucceededSupportsExplicitSuccessValue(t *testing.T) {
	rule := mustPresetRule(t, "sub2api")
	if success, known := loginResponseSucceeded(map[string]any{"code": json.Number("0")}, rule); !known || !success {
		t.Fatalf("code=0 success=%v known=%v", success, known)
	}
	if success, known := loginResponseSucceeded(map[string]any{"code": json.Number("401")}, rule); !known || success {
		t.Fatalf("code=401 success=%v known=%v", success, known)
	}
	if _, known := loginResponseSucceeded(map[string]any{"message": "missing code"}, rule); known {
		t.Fatal("missing success path must remain unknown so HTTP status and reusable-material checks decide")
	}
}

func TestLoginNeedsHumanActionRecognizesLocalizedAndMFAChallenges(t *testing.T) {
	if !loginNeedsHumanAction(nil, domain.LoginRule{}, "\u9700\u8981\u5b8c\u6210\u9a8c\u8bc1\u7801\u540e\u767b\u5f55") {
		t.Fatal("localized captcha challenge was not classified as human_action_required")
	}
	if !loginNeedsHumanAction(map[string]any{"data": map[string]any{"mfa_required": true}}, domain.LoginRule{}, "") {
		t.Fatal("MFA challenge flag was not classified as human_action_required")
	}
}

func mustPresetRule(t *testing.T, adapter string) domain.LoginRule {
	t.Helper()
	_, _, view, ok := RecommendedLoginRule(adapter)
	if !ok || view == nil {
		t.Fatalf("preset %q unavailable", adapter)
	}
	return domain.LoginRule{SuccessJSONPath: view.SuccessJSONPath, SuccessValue: view.SuccessValue}
}
