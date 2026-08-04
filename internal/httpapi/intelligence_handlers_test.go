package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestIntelligenceConfigHotUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin"})

	update := []byte(`{"price_elasticity":"-0.4","prior_uncertainty":"0.8","observed_uncertainty":"0.2","minimum_calibration_samples":12,"forecast_currency":" cny "}`)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/intelligence/config", bytes.NewReader(update))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/intelligence/config", nil)
	get.Header.Set("X-RelayOps-Token", "admin")
	readback := httptest.NewRecorder()
	handler.ServeHTTP(readback, get)
	if readback.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", readback.Code, readback.Body.String())
	}
	var envelope struct {
		Data intelligence.Config `json:"data"`
	}
	if err := json.Unmarshal(readback.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ForecastCurrency != "CNY" || envelope.Data.PriorUncertainty != "0.8" || envelope.Data.MinimumCalibrationN != 12 {
		t.Fatalf("config readback = %#v", envelope.Data)
	}

	invalid := httptest.NewRequest(http.MethodPut, "/api/v1/intelligence/config", bytes.NewReader([]byte(`{"forecast_currency":"RMB1"}`)))
	invalid.Header.Set("Content-Type", "application/json")
	invalid.Header.Set("X-RelayOps-Token", "admin")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid currency status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestOverviewIntelligenceIsAuthenticatedAndAvailableWithoutFullDashboard(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/intelligence/overview", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized overview status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/intelligence/overview", nil)
	request.Header.Set("X-RelayOps-Token", "admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"raw_event_count":0`)) ||
		!bytes.Contains(response.Body.Bytes(), []byte(`"operations"`)) {
		t.Fatalf("overview status=%d body=%s", response.Code, response.Body.String())
	}
}
