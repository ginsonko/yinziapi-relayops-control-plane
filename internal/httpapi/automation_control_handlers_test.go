package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/automation"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorservice"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func TestAutomationControlHotUpdateIsAuthenticatedPersistentAndVisibleInBootstrap(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	service := automation.NewService(st, nil)
	mirror, err := mirrorservice.New(st, targets.NewFactory(http.DefaultClient, nil), nil, mirrorservice.Options{})
	if err != nil {
		t.Fatalf("create mirror service: %v", err)
	}
	handler := New(Dependencies{Store: st, Automation: service, Mirror: mirror, StaticDir: t.TempDir(), AdminToken: "admin-secret",
		AutomationEnabled: true, BackgroundWorkersEnabled: true})
	if !mirror.AutomaticWritesEnabled() {
		t.Fatal("mirror automatic writes were not enabled during server initialization")
	}

	get := authenticatedRequest(handler, http.MethodGet, "/api/v1/automation-control", "")
	if get.Code != http.StatusOK {
		t.Fatalf("get automation control status=%d body=%s", get.Code, get.Body.String())
	}
	var initial struct {
		Data domain.AutomationControl `json:"data"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &initial); err != nil || !initial.Data.Enabled || !initial.Data.BackgroundWorkersEnabled || initial.Data.UpdatedAt.IsZero() {
		t.Fatalf("initial automation control=%#v err=%v", initial.Data, err)
	}

	disable := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-control", `{"enabled":false}`)
	if disable.Code != http.StatusOK || !strings.Contains(disable.Body.String(), `"enabled":false`) {
		t.Fatalf("disable status=%d body=%s", disable.Code, disable.Body.String())
	}
	if mirror.AutomaticWritesEnabled() {
		t.Fatal("mirror automatic writes remained enabled after global disable")
	}
	bootstrap := authenticatedRequest(handler, http.MethodGet, "/api/v1/console/bootstrap", "")
	if bootstrap.Code != http.StatusOK || !strings.Contains(bootstrap.Body.String(), `"automationEnabled":false`) || !strings.Contains(bootstrap.Body.String(), `"backgroundWorkersEnabled":true`) {
		t.Fatalf("disabled bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	enable := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-control", `{"enabled":true}`)
	if enable.Code != http.StatusOK || !strings.Contains(enable.Body.String(), `"enabled":true`) {
		t.Fatalf("enable status=%d body=%s", enable.Code, enable.Body.String())
	}
	if !mirror.AutomaticWritesEnabled() {
		t.Fatal("mirror automatic writes remained disabled after global enable")
	}
}

func TestAutomationControlCannotClaimEnabledWithoutWorkers(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	service := automation.NewService(st, nil)
	handler := New(Dependencies{Store: st, Automation: service, StaticDir: t.TempDir(), AdminToken: "admin-secret",
		AutomationEnabled: false, BackgroundWorkersEnabled: false})
	response := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-control", `{"enabled":true}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("enable without workers status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAutomationSafetyHotUpdateIsValidatedAndPersistent(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	service := automation.NewService(st, nil)
	handler := New(Dependencies{Store: st, Automation: service, StaticDir: t.TempDir(), AdminToken: "admin-secret",
		AutomationEnabled: true, BackgroundWorkersEnabled: true})

	initial := authenticatedRequest(handler, http.MethodGet, "/api/v1/automation-safety", "")
	if initial.Code != http.StatusOK || !strings.Contains(initial.Body.String(), `"equivalent_cost_multiple_limit":"4"`) {
		t.Fatalf("initial safety status=%d body=%s", initial.Code, initial.Body.String())
	}
	update := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-safety",
		`{"target_read_cache_milliseconds":900000,"equivalent_cost_multiple_limit":"8","minimum_comparable_dimensions":3,"maximum_automatic_increase_rate":"2","maximum_automatic_decrease_rate":"0.5"}`)
	if update.Code != http.StatusOK || !strings.Contains(update.Body.String(), `"equivalent_cost_multiple_limit":"8"`) {
		t.Fatalf("update safety status=%d body=%s", update.Code, update.Body.String())
	}
	reloaded := automation.NewService(st, nil)
	config, err := reloaded.SafetyConfig(context.Background())
	if err != nil || config.TargetReadCacheMilliseconds != 900000 || config.MinimumComparableDimensions != 3 ||
		config.EquivalentCostMultipleLimit != "8" || config.MaximumAutomaticIncreaseRate != "2" || config.MaximumAutomaticDecreaseRate != "0.5" {
		t.Fatalf("persisted safety=%#v err=%v", config, err)
	}
	invalid := authenticatedRequest(handler, http.MethodPut, "/api/v1/automation-safety", `{"maximum_automatic_increase_rate":"-1"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid safety status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}
