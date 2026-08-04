package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

func TestRemoteWriteGateIsDisclosedAndCannotBeOverriddenThroughHTTP(t *testing.T) {
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret",
		AutomationEnabled: true, BackgroundWorkersEnabled: true, RemoteWriteGate: remotewrite.New(false)})

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"remote_writes_enabled":false`) {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}
	bootstrap := authenticatedRequest(handler, http.MethodGet, "/api/v1/console/bootstrap", "")
	if bootstrap.Code != http.StatusOK || !strings.Contains(bootstrap.Body.String(), `"remoteWritesEnabled":false`) ||
		!strings.Contains(bootstrap.Body.String(), `"automationEnabled":false`) {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	control := authenticatedRequest(handler, http.MethodGet, "/api/v1/automation-control", "")
	if control.Code != http.StatusOK || !strings.Contains(control.Body.String(), `"remote_writes_enabled":false`) ||
		!strings.Contains(control.Body.String(), `"enabled":false`) {
		t.Fatalf("automation control status=%d body=%s", control.Code, control.Body.String())
	}
	enable := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-control", `{"enabled":true}`)
	if enable.Code != http.StatusConflict || !strings.Contains(enable.Body.String(), remotewrite.DisabledMessage) {
		t.Fatalf("automation enable status=%d body=%s", enable.Code, enable.Body.String())
	}
	persisted, err := st.ResolveAutomationControl(context.Background(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Enabled {
		t.Fatalf("rejected effective enable unexpectedly rewrote persisted operator preference: %#v", persisted)
	}
}

func TestRemoteWriteHTTPTriggersReturnExplicitCandidateStatus(t *testing.T) {
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret",
		RemoteWriteGate: remotewrite.New(false)})

	refresh := authenticatedRequest(handler, http.MethodPost, "/api/v1/credentials/123/refresh", "")
	if refresh.Code != http.StatusServiceUnavailable || !strings.Contains(refresh.Body.String(), remotewrite.DisabledMessage) {
		t.Fatalf("credential refresh status=%d body=%s", refresh.Code, refresh.Body.String())
	}
	renew := authenticatedRequest(handler, http.MethodPost, "/api/v1/credentials/123/renewal/trigger", "")
	if renew.Code != http.StatusServiceUnavailable || !strings.Contains(renew.Body.String(), remotewrite.DisabledMessage) {
		t.Fatalf("credential renewal status=%d body=%s", renew.Code, renew.Body.String())
	}
	targetBody := `{"name":"candidate target","base_url":"https://target.example","adapter":"newapi","write_mode":"approval","auth_method":"password","username":"root","password":"secret"}`
	target := authenticatedRequest(handler, http.MethodPost, "/api/v1/targets/connect", targetBody)
	if target.Code != http.StatusServiceUnavailable || !strings.Contains(target.Body.String(), remotewrite.DisabledMessage) {
		t.Fatalf("target password login status=%d body=%s", target.Code, target.Body.String())
	}
	if bytes.Contains(target.Body.Bytes(), []byte("secret")) {
		t.Fatalf("target password leaked in gate response: %s", target.Body.String())
	}
}
