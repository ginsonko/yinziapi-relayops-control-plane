package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestConnectorManifestBindingAndPairingFailureStateRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://api.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	manifest := domain.ConnectorManifestRecord{
		Name: "newapi-v1", SchemaVersion: domain.ConnectorManifestSchemaV1, AdapterFamily: "newapi", Source: "test",
		ValidationStatus: "valid", Manifest: domain.ConnectorManifest{SchemaVersion: domain.ConnectorManifestSchemaV1,
			Name: "newapi-v1", AdapterFamily: "newapi", Origins: domain.ConnectorManifestOrigins{Schemes: []string{"https"}, Hosts: []string{"api.example.com"}},
			Capabilities: map[string]domain.ConnectorManifestCapability{"status": {Paths: []string{"/api/status"}, Contract: "native"}}},
	}
	if err := st.CreateConnectorManifest(ctx, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := st.BindSupplierConnectorManifest(ctx, supplier.ID, manifest.ID, supplier.BaseURL, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	bound, found, err := st.BoundConnectorManifest(ctx, supplier.ID)
	if err != nil || !found || bound.ID != manifest.ID || bound.Manifest.Capabilities["status"].Paths[0] != "/api/status" {
		t.Fatalf("bound manifest = %#v found=%v err=%v", bound, found, err)
	}

	now := time.Now().UTC()
	if err := st.SavePairingToken(ctx, "pair-hash", supplier.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.FailPairingToken(ctx, "pair-hash", "origin_mismatch", "safe public message", now); err != nil {
		t.Fatal(err)
	}
	state, err := st.PairingStatusDetail(ctx, "pair-hash", now)
	if err != nil || state.Status != "failed" || state.AttemptCount != 1 || state.LastErrorClass != "origin_mismatch" || state.LastErrorMessage != "safe public message" {
		t.Fatalf("pair state = %#v err=%v", state, err)
	}
	if err := st.FailPairingToken(ctx, "pair-hash", "other", "overwrite", now); err == nil {
		t.Fatal("terminal pairing failure was overwritten")
	}
}

func TestPairingTransientFailureRemainsPendingUntilAttemptBudgetIsExhausted(t *testing.T) {
	t.Parallel()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "retry upstream", BaseURL: "https://retry.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SavePairingToken(ctx, "retry-pair", supplier.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		state, recordErr := st.RecordPairingFailure(ctx, "retry-pair", "auth_expired", "safe message", now.Add(time.Duration(attempt)*time.Second), 3, false)
		if recordErr != nil {
			t.Fatalf("attempt %d: %v", attempt, recordErr)
		}
		wantStatus := "pending"
		wantRetryable := true
		if attempt == 3 {
			wantStatus = "failed"
			wantRetryable = false
		}
		if state.Status != wantStatus || state.AttemptCount != attempt || state.Retryable != wantRetryable || state.AttemptsRemaining != 3-attempt {
			t.Fatalf("attempt %d state = %#v", attempt, state)
		}
	}
	credential := domain.Credential{Name: "must-not-bind", Kind: "browser_pairing", SiteURL: supplier.BaseURL}
	if err := st.CreateCredentialFromPairing(ctx, "retry-pair", now.Add(4*time.Second), supplier.ID, &credential, "encrypted"); err == nil {
		t.Fatal("exhausted pairing token unexpectedly created a credential")
	}
}

func TestPairingUpstreamFailureDoesNotConsumeAttemptBudget(t *testing.T) {
	t.Parallel()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	supplier := domain.Supplier{Name: "network upstream", BaseURL: "https://network.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SavePairingToken(ctx, "network-pair", supplier.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, err := st.RecordPairingTransientFailure(ctx, "network-pair", "connection_reset", "safe message", now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "pending" || state.AttemptCount != 0 || state.LastErrorClass != "connection_reset" {
		t.Fatalf("transient state = %#v", state)
	}
	state, err = st.RecordPairingFailure(ctx, "network-pair", "auth_expired", "candidate rejected", now.Add(time.Second), 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "pending" || state.AttemptCount != 1 || state.AttemptsRemaining != 2 {
		t.Fatalf("candidate state = %#v", state)
	}
}
