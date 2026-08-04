package credentials

import (
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestNormalizeCapturedPayloadExtractsNestedNewAPISession(t *testing.T) {
	payload := NormalizeCapturedPayload(domain.CredentialPayload{LocalStorage: map[string]string{
		"currentUser": `{"profile":{"id":42},"accessToken":"dkby-access-token","refreshToken":"dkby-refresh-token","expiresAt":1893456000000}`,
	}})
	if payload.Authorization != "Bearer dkby-access-token" || payload.UserID != 42 {
		t.Fatalf("normalized payload = %#v", payload)
	}
	if payload.LocalStorage["refresh_token"] != "dkby-refresh-token" || payload.LocalStorage["expires_at"] != "1893456000000" {
		t.Fatalf("normalized storage = %#v", payload.LocalStorage)
	}
}

func TestNormalizeCapturedPayloadDoesNotTreatUnrelatedIDsAsUser(t *testing.T) {
	payload := NormalizeCapturedPayload(domain.CredentialPayload{SessionStorage: map[string]string{
		"application": `{"model":{"id":77},"session":{"token":"session-token-value"}}`,
	}})
	if payload.Authorization != "Bearer session-token-value" || payload.UserID != 0 {
		t.Fatalf("normalized payload = %#v", payload)
	}
}

func TestNormalizeCapturedPayloadPreservesExplicitCredential(t *testing.T) {
	payload := NormalizeCapturedPayload(domain.CredentialPayload{
		Authorization: "ApiKey explicit", UserID: 9,
		LocalStorage: map[string]string{"user": `{"id":42,"token":"nested-token-value"}`},
	})
	if payload.Authorization != "ApiKey explicit" || payload.UserID != 9 {
		t.Fatalf("explicit credential was replaced: %#v", payload)
	}
}
