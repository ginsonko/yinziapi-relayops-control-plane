package credentials

import (
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNormalizeLoginRuleAcceptsPasswordReloginWithoutRefreshURL(t *testing.T) {
	t.Parallel()
	rule, err := normalizeLoginRule(domain.LoginRule{
		Enabled: true, Username: "operator@example.com", Password: "secret",
		RefreshMethod: http.MethodPost, RefreshTokenField: "refresh_token",
	}, LoginPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if rule.RefreshURL != "" || rule.RefreshMethod != "" || rule.RefreshTokenField != "" {
		t.Fatalf("legacy disabled refresh was not normalized: %#v", rule)
	}
}

func TestNormalizeLoginRuleEmptyRefreshURLDisablesAllRefreshMappings(t *testing.T) {
	t.Parallel()
	tests := []domain.LoginRule{
		{RefreshMethod: http.MethodPut, RefreshTokenField: "refresh_token"},
		{RefreshMethod: http.MethodPost, RefreshTokenField: "custom_refresh"},
		{RefreshMethod: http.MethodPost, RefreshTokenField: "refresh_token", LoginRefreshTokenJSONPath: "data.refresh_token"},
		{RefreshMethod: http.MethodPost, RefreshTokenField: "refresh_token", RefreshResponseTokenPath: "data.access_token"},
	}
	for _, input := range tests {
		input.Enabled = true
		input.Username = "operator@example.com"
		input.Password = "secret"
		rule, err := normalizeLoginRule(input, LoginPolicy{})
		if err != nil {
			t.Fatalf("disabled refresh contract %#v returned %v", input, err)
		}
		if rule.RefreshURL != "" || rule.RefreshMethod != "" || rule.RefreshTokenField != "" ||
			rule.LoginRefreshTokenJSONPath != "" || rule.RefreshResponseTokenPath != "" ||
			rule.RefreshResponseRefreshPath != "" || rule.RefreshResponseExpiresPath != "" ||
			rule.RefreshResponseExpiryPath != "" {
			t.Fatalf("disabled refresh mappings were retained: %#v", rule)
		}
	}
}

func TestNormalizeLoginRuleKeepsCompleteSub2RefreshContract(t *testing.T) {
	t.Parallel()
	rule, err := normalizeLoginRule(domain.LoginRule{
		Enabled: true, Username: "operator@example.com", Password: "secret",
		RefreshURL: "/api/v1/auth/refresh", RefreshMethod: http.MethodPost,
		RefreshTokenField: "refresh_token", LoginRefreshTokenJSONPath: "data.refresh_token",
		RefreshResponseTokenPath: "data.access_token",
	}, LoginPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if rule.RefreshURL != "/api/v1/auth/refresh" || rule.RefreshMethod != http.MethodPost ||
		rule.RefreshTokenField != "refresh_token" || rule.LoginRefreshTokenJSONPath != "data.refresh_token" {
		t.Fatalf("complete refresh contract changed: %#v", rule)
	}
}
