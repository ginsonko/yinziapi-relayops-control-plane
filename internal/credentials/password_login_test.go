package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestPasswordLoginValidatesAndAtomicallyReplacesCredential(t *testing.T) {
	var loginCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCalls.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["username"] != "renew-user" || body["password"] != "renew-password" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"new-access-token","id":42}}`)
	}))
	defer upstream.Close()

	service, st, credential, refresher := newPasswordLoginFixture(t, upstream, domain.LoginRule{}, func(_ context.Context, _ domain.Supplier, _ domain.Credential, payload domain.CredentialPayload) error {
		if tokenFromPayload(payload) != "new-access-token" {
			return errors.New("candidate token was not updated")
		}
		return nil
	})
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatal(err)
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("login calls = %d, want 1", loginCalls.Load())
	}
	stored, payload, err := service.Load(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tokenFromPayload(payload) != "new-access-token" || payload.UserID != 42 || stored.Status != "active" {
		t.Fatalf("renewed credential = %#v, payload token/user = %q/%d", stored, tokenFromPayload(payload), payload.UserID)
	}
	detail, err := service.RenewalDetail(context.Background(), credential.ID)
	if err != nil || detail.Status.State != "healthy" || detail.Status.ConsecutiveFailures != 0 || detail.Status.LastSuccessAt == nil {
		t.Fatalf("renewal detail = %#v, err=%v", detail, err)
	}
	list, _ := st.ListCredentials(context.Background())
	activities, _ := st.RecentActivity(context.Background(), 20)
	assertPublicValuesDoNotContainSecrets(t, list, detail, activities)
}

func TestPasswordLoginDropsStaleCookieWhenFreshLoginReturnsOnlyToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"fresh-token","user":{"id":55}}}`)
	}))
	defer upstream.Close()

	service, _, credential, refresher := newPasswordLoginFixture(t, upstream, domain.LoginRule{},
		func(_ context.Context, _ domain.Supplier, _ domain.Credential, payload domain.CredentialPayload) error {
			if payload.Cookie != "" {
				return fmt.Errorf("stale cookie survived fresh login")
			}
			if tokenFromPayload(payload) != "fresh-token" || payload.UserID != 55 {
				return fmt.Errorf("fresh token or nested user id was not installed")
			}
			return nil
		})
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatal(err)
	}
	_, payload, err := service.Load(context.Background(), credential.ID)
	if err != nil || payload.Cookie != "" || tokenFromPayload(payload) != "fresh-token" || payload.UserID != 55 {
		t.Fatalf("renewed payload token=%q cookie=%q user=%d err=%v", tokenFromPayload(payload), payload.Cookie, payload.UserID, err)
	}
}

func TestPasswordLoginCapturesTopLevelUserIDAndRotatedCookie(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "rotated", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"success":true,"token":"fresh-top-level-token","id":77}`)
	}))
	defer upstream.Close()

	service, _, credential, refresher := newPasswordLoginFixture(t, upstream,
		domain.LoginRule{TokenJSONPath: "token", UserIDJSONPath: "missing.id", CookieResponse: true},
		func(_ context.Context, _ domain.Supplier, _ domain.Credential, payload domain.CredentialPayload) error {
			if tokenFromPayload(payload) != "fresh-top-level-token" || payload.UserID != 77 || !strings.Contains(payload.Cookie, "session=rotated") {
				return fmt.Errorf("top-level identity or rotated cookie was not installed")
			}
			return nil
		})
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatal(err)
	}
	_, payload, err := service.Load(context.Background(), credential.ID)
	if err != nil || tokenFromPayload(payload) != "fresh-top-level-token" || payload.UserID != 77 || !strings.Contains(payload.Cookie, "session=rotated") {
		t.Fatalf("renewed payload token=%q cookie=%q user=%d err=%v", tokenFromPayload(payload), payload.Cookie, payload.UserID, err)
	}
}

func TestPasswordLoginFailuresPreserveExistingCredential(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantClass  string
		wantState  string
	}{
		{name: "bad password", statusCode: http.StatusUnauthorized, body: `{"success":false,"message":"invalid password"}`, wantClass: "bad_credentials", wantState: "suspended_backoff"},
		{name: "generic unauthorized challenge", statusCode: http.StatusUnauthorized, body: `{"success":false,"message":"login rejected"}`, wantClass: "login_rejected", wantState: "human_action_required"},
		{name: "forbidden verification", statusCode: http.StatusForbidden, body: `{"success":false,"message":"additional verification required"}`, wantClass: "login_rejected", wantState: "human_action_required"},
		{name: "captcha", statusCode: http.StatusOK, body: `{"success":false,"message":"captcha required","data":{"captcha_required":true}}`, wantClass: "captcha_or_2fa", wantState: "human_action_required"},
		{name: "empty success", statusCode: http.StatusOK, body: `{"success":true}`, wantClass: "insufficient_login_material", wantState: "configuration_required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer upstream.Close()
			service, _, credential, refresher := newPasswordLoginFixture(t, upstream, domain.LoginRule{}, nil)
			err := refresher.Refresh(context.Background(), credential.ID)
			if RenewalErrorClass(err) != test.wantClass {
				t.Fatalf("error = %v, class=%q, want %q", err, RenewalErrorClass(err), test.wantClass)
			}
			_, payload, loadErr := service.Load(context.Background(), credential.ID)
			if loadErr != nil || tokenFromPayload(payload) != "old-access-token" || payload.Cookie != "old-session=1" {
				t.Fatalf("failed renewal changed payload: token=%q cookie=%q err=%v", tokenFromPayload(payload), payload.Cookie, loadErr)
			}
			detail, detailErr := service.RenewalDetail(context.Background(), credential.ID)
			if detailErr != nil || detail.Status.State != test.wantState || detail.Status.ConsecutiveFailures != 1 {
				t.Fatalf("renewal detail = %#v, err=%v", detail, detailErr)
			}
			if test.wantState == "human_action_required" && (detail.Status.NextAttemptAt == nil || detail.Status.LastAttemptAt == nil ||
				detail.Status.NextAttemptAt.Sub(*detail.Status.LastAttemptAt) != 12*time.Hour) {
				t.Fatalf("human-action cooldown = %#v, want 12h", detail.Status)
			}
			assertPublicValuesDoNotContainSecrets(t, err, detail)
		})
	}
}

func TestPasswordLoginRejectsCrossOriginRedirectBeforeSecretsEscape(t *testing.T) {
	var escaped atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		escaped.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	service, _, credential, refresher := newPasswordLoginFixture(t, source, domain.LoginRule{}, nil)
	err := refresher.Refresh(context.Background(), credential.ID)
	if RenewalErrorClass(err) != "network_error" || escaped.Load() != 0 {
		t.Fatalf("redirect result = %v, escaped requests=%d", err, escaped.Load())
	}
	_, payload, _ := service.Load(context.Background(), credential.ID)
	if tokenFromPayload(payload) != "old-access-token" {
		t.Fatal("cross-origin redirect changed the credential")
	}
}

func TestPasswordLoginCandidateValidationFailurePreservesExistingCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"unprivileged-token"}}`)
	}))
	defer upstream.Close()
	service, _, credential, refresher := newPasswordLoginFixture(t, upstream, domain.LoginRule{}, func(context.Context, domain.Supplier, domain.Credential, domain.CredentialPayload) error {
		return errors.New("required read capability is missing")
	})
	err := refresher.Refresh(context.Background(), credential.ID)
	if RenewalErrorClass(err) != "permission_denied" {
		t.Fatalf("error = %v, class=%q", err, RenewalErrorClass(err))
	}
	if !strings.Contains(err.Error(), "required read capability is missing") {
		t.Fatalf("validation failure lost its safe cause: %v", err)
	}
	_, payload, _ := service.Load(context.Background(), credential.ID)
	if tokenFromPayload(payload) != "old-access-token" {
		t.Fatal("unverified candidate replaced the credential")
	}
	detail, _ := service.RenewalDetail(context.Background(), credential.ID)
	if detail.Status.State != "permission_denied" || detail.Status.NextAttemptAt != nil {
		t.Fatalf("permission failure status = %#v", detail.Status)
	}
}

func TestPasswordLoginCASConflictPreservesConcurrentConfiguration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"candidate-token"}}`)
	}))
	defer upstream.Close()
	var service *Service
	var credential domain.Credential
	validator := func(ctx context.Context, _ domain.Supplier, _ domain.Credential, _ domain.CredentialPayload) error {
		_, payload, err := service.Load(ctx, credential.ID)
		if err != nil {
			return err
		}
		rule := *payload.Login
		rule.Password = "concurrent-password"
		_, err = service.ConfigureLogin(ctx, credential.ID, rule, false, LoginPolicy{})
		return err
	}
	var refresher *Refresher
	service, _, credential, refresher = newPasswordLoginFixture(t, upstream, domain.LoginRule{}, validator)
	err := refresher.Refresh(context.Background(), credential.ID)
	if RenewalErrorClass(err) != "concurrent_change" {
		t.Fatalf("error = %v, class=%q", err, RenewalErrorClass(err))
	}
	_, payload, _ := service.Load(context.Background(), credential.ID)
	detail, _ := service.RenewalDetail(context.Background(), credential.ID)
	if payload.Login == nil || payload.Login.Password != "concurrent-password" || tokenFromPayload(payload) != "old-access-token" {
		t.Fatalf("concurrent value was overwritten: %#v", payload.Login)
	}
	if detail.Status.State != "healthy" || detail.Status.ConsecutiveFailures != 0 || detail.Status.LastErrorClass != "" {
		t.Fatalf("stale renewal polluted concurrent state: %#v", detail.Status)
	}
}

func TestPasswordLoginBackoffStopsAfterMaximumFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"success":false,"message":"invalid password"}`)
	}))
	defer upstream.Close()
	rule := domain.LoginRule{InitialBackoffSeconds: 1, MaximumBackoffSeconds: 4, MaximumConsecutiveFailures: 3}
	service, _, credential, refresher := newPasswordLoginFixture(t, upstream, rule, nil)
	for attempt, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 0} {
		if err := refresher.Refresh(context.Background(), credential.ID); RenewalErrorClass(err) != "bad_credentials" {
			t.Fatalf("attempt %d error = %v", attempt+1, err)
		}
		detail, _ := service.RenewalDetail(context.Background(), credential.ID)
		if detail.Status.ConsecutiveFailures != attempt+1 || detail.Status.State != "suspended_backoff" {
			t.Fatalf("attempt %d status = %#v", attempt+1, detail.Status)
		}
		if wantDelay == 0 {
			if detail.Status.NextAttemptAt != nil {
				t.Fatalf("terminal suspension still scheduled at %v", detail.Status.NextAttemptAt)
			}
			continue
		}
		if detail.Status.NextAttemptAt == nil || detail.Status.LastAttemptAt == nil || detail.Status.NextAttemptAt.Sub(*detail.Status.LastAttemptAt) != wantDelay {
			t.Fatalf("attempt %d backoff = %#v, want %v", attempt+1, detail.Status, wantDelay)
		}
	}
}

func TestPasswordLoginTransientFailureKeepsSlowRecoveryScheduled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	rule := domain.LoginRule{InitialBackoffSeconds: 1, MaximumBackoffSeconds: 4, MaximumConsecutiveFailures: 3}
	service, _, credential, refresher := newPasswordLoginFixture(t, upstream, rule, nil)
	for attempt, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if err := refresher.Refresh(context.Background(), credential.ID); RenewalErrorClass(err) != "network_error" {
			t.Fatalf("attempt %d error = %v", attempt+1, err)
		}
		detail, _ := service.RenewalDetail(context.Background(), credential.ID)
		if detail.Status.ConsecutiveFailures != attempt+1 || detail.Status.State != "retry_backoff" ||
			detail.Status.NextAttemptAt == nil || detail.Status.LastAttemptAt == nil {
			t.Fatalf("attempt %d status = %#v", attempt+1, detail.Status)
		}
		if delay := detail.Status.NextAttemptAt.Sub(*detail.Status.LastAttemptAt); delay != wantDelay {
			t.Fatalf("attempt %d backoff = %v, want %v", attempt+1, delay, wantDelay)
		}
	}
}

func TestRefreshTokenCandidateIsValidatedBeforePasswordFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh":
			_, _ = fmt.Fprint(w, `{"access_token":"refresh-candidate"}`)
		case "/api/user/login":
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"token":"login-candidate"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	service, st := newCredentialServices(t)
	rule := defaultPasswordLoginRule()
	credential := domain.Credential{Name: "refresh then login", Kind: "token", SiteURL: upstream.URL}
	payload := defaultPasswordLoginPayload(rule)
	payload.Refresh = &domain.RefreshRule{URL: "/refresh"}
	if err := service.Create(context.Background(), &credential, payload); err != nil {
		t.Fatal(err)
	}
	bindCredentialSupplier(t, st, credential, upstream.URL)
	var candidates []string
	refresher := NewRefresher(st, service, upstream.Client(), time.Minute).WithCandidateValidator(func(_ context.Context, _ domain.Supplier, _ domain.Credential, payload domain.CredentialPayload) error {
		token := tokenFromPayload(payload)
		candidates = append(candidates, token)
		if token == "refresh-candidate" {
			return errors.New("refresh candidate lacks required capability")
		}
		return nil
	})
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatal(err)
	}
	_, stored, _ := service.Load(context.Background(), credential.ID)
	if strings.Join(candidates, ",") != "refresh-candidate,login-candidate" || tokenFromPayload(stored) != "login-candidate" {
		t.Fatalf("candidate order=%v stored=%q", candidates, tokenFromPayload(stored))
	}
}

func TestSub2PasswordLoginInstallsRefreshRuleAndNextRenewalUsesIt(t *testing.T) {
	var loginCalls atomic.Int32
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			loginCalls.Add(1)
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["email"] != "admin@example.test" || body["password"] != "renew-password" {
				http.Error(w, "bad login body", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":0,"message":"success","data":{"access_token":"login-access","refresh_token":"refresh-1","expires_in":3600,"user":{"id":42}}}`)
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["refresh_token"] != "refresh-1" {
				http.Error(w, "bad refresh body", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":0,"message":"success","data":{"access_token":"refresh-access","refresh_token":"refresh-2","expires_in":7200}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	service, st := newCredentialServices(t)
	rule := domain.LoginRule{Enabled: true, Username: "admin@example.test", Password: "renew-password",
		URL: "/api/v1/auth/login", Method: http.MethodPost, ContentType: "application/json",
		UsernameField: "email", PasswordField: "password", TokenJSONPath: "data.access_token",
		UserIDJSONPath: "data.user.id", ExpiresInJSONPath: "data.expires_in", SuccessJSONPath: "code", SuccessValue: "0",
		LoginRefreshTokenJSONPath: "data.refresh_token", RefreshURL: "/api/v1/auth/refresh", RefreshMethod: http.MethodPost,
		RefreshTokenField: "refresh_token", RefreshResponseTokenPath: "data.access_token",
		RefreshResponseRefreshPath: "data.refresh_token", RefreshResponseExpiresPath: "data.expires_in"}
	credential := domain.Credential{Name: "Sub2 login", Kind: "access-token", SiteURL: upstream.URL}
	if err := service.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer old", Login: &rule}); err != nil {
		t.Fatal(err)
	}
	bindCredentialSupplier(t, st, credential, upstream.URL)
	refresher := NewRefresher(st, service, upstream.Client(), time.Minute).WithCandidateValidator(
		func(context.Context, domain.Supplier, domain.Credential, domain.CredentialPayload) error { return nil })
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatalf("password login renewal: %v", err)
	}
	_, afterLogin, err := service.Load(context.Background(), credential.ID)
	if err != nil || afterLogin.Refresh == nil || afterLogin.LocalStorage["refresh_token"] != "refresh-1" ||
		afterLogin.Refresh.URL != "/api/v1/auth/refresh" || !strings.Contains(afterLogin.Refresh.Body, "${refresh_token}") {
		t.Fatalf("installed refresh contract=%#v payload=%#v err=%v", afterLogin.Refresh, afterLogin.LocalStorage, err)
	}
	if err := refresher.Refresh(context.Background(), credential.ID); err != nil {
		t.Fatalf("refresh-token renewal: %v", err)
	}
	_, renewed, err := service.Load(context.Background(), credential.ID)
	if err != nil || tokenFromPayload(renewed) != "refresh-access" || renewed.LocalStorage["refresh_token"] != "refresh-2" {
		t.Fatalf("renewed token=%q refresh=%q err=%v", tokenFromPayload(renewed), renewed.LocalStorage["refresh_token"], err)
	}
	if loginCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("login calls=%d refresh calls=%d, want refresh-first after one login", loginCalls.Load(), refreshCalls.Load())
	}
}

func newPasswordLoginFixture(t *testing.T, upstream *httptest.Server, overrides domain.LoginRule,
	validator CandidateValidator) (*Service, *store.Store, domain.Credential, *Refresher) {
	t.Helper()
	service, st := newCredentialServices(t)
	rule := defaultPasswordLoginRule()
	mergeLoginRuleOverrides(&rule, overrides)
	credential := domain.Credential{Name: "password login", Kind: "access-token", SiteURL: upstream.URL}
	if err := service.Create(context.Background(), &credential, defaultPasswordLoginPayload(rule)); err != nil {
		t.Fatal(err)
	}
	bindCredentialSupplier(t, st, credential, upstream.URL)
	if validator == nil {
		validator = func(context.Context, domain.Supplier, domain.Credential, domain.CredentialPayload) error { return nil }
	}
	refresher := NewRefresher(st, service, upstream.Client(), time.Minute).WithCandidateValidator(validator)
	return service, st, credential, refresher
}

func defaultPasswordLoginRule() domain.LoginRule {
	return domain.LoginRule{Enabled: true, Username: "renew-user", Password: "renew-password", URL: "/api/user/login",
		Method: http.MethodPost, ContentType: "application/json", UsernameField: "username", PasswordField: "password",
		TokenJSONPath: "data.token", UserIDJSONPath: "data.id"}
}

func defaultPasswordLoginPayload(rule domain.LoginRule) domain.CredentialPayload {
	return domain.CredentialPayload{AuthorizationMode: "bearer", Authorization: "Bearer old-access-token",
		Cookie: "old-session=1", LocalStorage: map[string]string{"auth_token": "old-access-token"}, Login: &rule}
}

func mergeLoginRuleOverrides(rule *domain.LoginRule, overrides domain.LoginRule) {
	if overrides.TokenJSONPath != "" {
		rule.TokenJSONPath = overrides.TokenJSONPath
	}
	if overrides.UserIDJSONPath != "" {
		rule.UserIDJSONPath = overrides.UserIDJSONPath
	}
	if overrides.CookieResponse {
		rule.CookieResponse = true
	}
	if overrides.InitialBackoffSeconds != 0 {
		rule.InitialBackoffSeconds = overrides.InitialBackoffSeconds
	}
	if overrides.MaximumBackoffSeconds != 0 {
		rule.MaximumBackoffSeconds = overrides.MaximumBackoffSeconds
	}
	if overrides.MaximumConsecutiveFailures != 0 {
		rule.MaximumConsecutiveFailures = overrides.MaximumConsecutiveFailures
	}
}

func bindCredentialSupplier(t *testing.T, st *store.Store, credential domain.Credential, baseURL string) {
	t.Helper()
	id := credential.ID
	supplier := domain.Supplier{Name: "bound supplier", BaseURL: baseURL, Adapter: "newapi", CredentialID: &id, Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
}

func TestRenewalFailureActivityMessagesAreOperatorFacingChinese(t *testing.T) {
	t.Parallel()
	for _, class := range []string{"bad_credentials", "captcha_or_2fa", "login_rejected", "permission_denied", "network_error", "invalid_login_rule", "replacement_not_allowed", "unknown"} {
		message := renewalFailureActivityMessage(class)
		if strings.TrimSpace(message) == "" || strings.Contains(strings.ToLower(message), "credential login") {
			t.Fatalf("class %q produced unsuitable activity message %q", class, message)
		}
	}
}

func assertPublicValuesDoNotContainSecrets(t *testing.T, values ...any) {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{"renew-user", "renew-password", "old-access-token", "new-access-token", "old-session=1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("public value leaked %q: %s", secret, text)
		}
	}
}
