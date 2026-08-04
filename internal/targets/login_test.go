package targets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticateNewAPIExchangesRootPasswordForSession(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/user/login" || request.Method != http.MethodPost {
			http.NotFound(response, request)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["username"] != "root" || body["password"] != "secret" {
			t.Errorf("login body = %#v", body)
		}
		http.SetCookie(response, &http.Cookie{Name: "session", Value: "opaque", Path: "/", HttpOnly: true})
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"success":true,"data":{"id":17,"username":"root","role":100}}`))
	}))
	defer server.Close()

	factory := NewFactory(server.Client(), nil)
	result, err := factory.AuthenticateNewAPI(context.Background(), server.URL, "root", "secret")
	if err != nil {
		t.Fatalf("AuthenticateNewAPI: %v", err)
	}
	if result.UserID != 17 || result.Role != 100 || result.Payload.UserID != 17 || result.Payload.Cookie != "session=opaque" {
		t.Fatalf("result = %#v", result)
	}
	if result.Payload.Authorization != "" || result.Payload.CapturedOrigin != server.URL {
		t.Fatalf("payload = %#v", result.Payload)
	}
}

func TestAuthenticateNewAPIRejects2FAAndNonRoot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "2fa", body: `{"success":true,"data":{"require_2fa":true}}`, want: "requires 2FA"},
		{name: "non-root", body: `{"success":true,"data":{"id":9,"username":"admin","role":10}}`, want: "not a root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				http.SetCookie(response, &http.Cookie{Name: "session", Value: "opaque", Path: "/"})
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := NewFactory(server.Client(), nil).AuthenticateNewAPI(context.Background(), server.URL, "user", "password")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAuthenticateNewAPIDoesNotFollowCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		t.Error("login request escaped to a different origin")
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := NewFactory(source.Client(), nil).AuthenticateNewAPI(context.Background(), source.URL, "root", "secret")
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("error = %v", err)
	}
}
