package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/collector"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type pairingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function pairingRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPairingNetworkFailureDoesNotSpendAttemptAndEnforcesCooldown(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	supplier := domain.Supplier{Name: "reset upstream", BaseURL: "https://reset.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	var upstreamRequests atomic.Int32
	client := &http.Client{Transport: pairingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		upstreamRequests.Add(1)
		return nil, errors.New("read tcp: connection reset by peer")
	})}
	factory := connectors.NewFactory(client, nil)
	collectorService := collector.NewService(st, factory)
	handler := New(Dependencies{Store: st, Collector: collectorService, StaticDir: t.TempDir(), AdminToken: "admin-secret-value-that-is-long-enough",
		PairingMaximumAttempts: 3, PairingTransientCooldown: time.Minute})

	start := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/credentials/%d/pair", supplier.ID), nil)
	start.Header.Set("Authorization", "Bearer admin-secret-value-that-is-long-enough")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start pair = %d %s", startResponse.Code, startResponse.Body.String())
	}
	code := extractPairingCode(t, startResponse.Body.Bytes())
	payload := []byte(`{"name":"Browser login","kind":"browser_pairing","payload":{"captured_origin":"https://reset.example.com","cookie":"session=candidate"}}`)
	first := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+code, bytes.NewReader(payload))
	first.Header.Set("Content-Type", "application/json")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusBadGateway || !bytes.Contains(firstResponse.Body.Bytes(), []byte(`"attempt_consumed":false`)) ||
		!bytes.Contains(firstResponse.Body.Bytes(), []byte(`"attempts_remaining":3`)) ||
		!bytes.Contains(firstResponse.Body.Bytes(), []byte(`"old_credential_preserved":false`)) {
		t.Fatalf("first transient response = %d %s", firstResponse.Code, firstResponse.Body.String())
	}
	requestsAfterFirst := upstreamRequests.Load()
	if requestsAfterFirst == 0 {
		t.Fatal("credential validation did not reach the upstream transport")
	}

	second := httptest.NewRequest(http.MethodPost, "/api/v1/pair/"+code, bytes.NewReader(payload))
	second.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusTooManyRequests || secondResponse.Header().Get("Retry-After") == "" ||
		!bytes.Contains(secondResponse.Body.Bytes(), []byte(`"attempt_consumed":false`)) {
		t.Fatalf("cooldown response = %d %s", secondResponse.Code, secondResponse.Body.String())
	}
	if upstreamRequests.Load() != requestsAfterFirst {
		t.Fatalf("cooldown reached upstream: before=%d after=%d", requestsAfterFirst, upstreamRequests.Load())
	}
	state, err := st.PairingStatusDetail(context.Background(), tokenHash(code), time.Now().UTC())
	if err != nil || state.Status != "pending" || state.AttemptCount != 0 || state.LastErrorClass != "connection_reset" {
		t.Fatalf("stored transient state = %#v err=%v", state, err)
	}
}

func extractPairingCode(t *testing.T, payload []byte) string {
	t.Helper()
	var envelope struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Data.Code == "" {
		t.Fatalf("pairing response has no valid code: %s err=%v", payload, err)
	}
	return envelope.Data.Code
}
