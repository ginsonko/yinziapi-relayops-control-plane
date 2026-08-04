package targets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

func TestRemoteWriteGateAllowsExactReadOnlyPreviewAndBlocksTargetSideEffectsBeforeNetwork(t *testing.T) {
	site := newGroupModelPricingSite()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		site.ServeHTTP(response, request)
	}))
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"
	adapter.remoteWrites = remotewrite.New(false)

	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	preview, err := adapter.PreviewGroupModelPricing(context.Background(), target, "revision-initial", desired)
	if err != nil || preview.DesiredRevision == "" {
		t.Fatalf("read-only group-model preview = %#v, error=%v", preview, err)
	}
	requestsAfterPreview := requests.Load()
	if requestsAfterPreview != 2 {
		t.Fatalf("preview requests=%d, want one GET and one semantic-read POST", requestsAfterPreview)
	}

	assertBlocked := func(action string, err error) {
		t.Helper()
		if !errors.Is(err, remotewrite.ErrDisabled) {
			t.Fatalf("%s error=%v, want remote-write gate", action, err)
		}
		if requests.Load() != requestsAfterPreview {
			t.Fatalf("%s reached the target; requests=%d want=%d", action, requests.Load(), requestsAfterPreview)
		}
	}
	_, err = adapter.ApplyGroupRatio(context.Background(), target, "revision-initial", map[string]string{"default": "1"})
	assertBlocked("GroupRatio apply", err)
	_, err = adapter.RollbackGroupRatio(context.Background(), target, "revision-initial", map[string]string{"default": "1"})
	assertBlocked("GroupRatio rollback", err)
	_, err = adapter.ApplyPricingOption(context.Background(), target, "ModelRatio", "revision-initial", map[string]string{"model": "1"})
	assertBlocked("pricing option apply", err)
	_, err = adapter.CreateDisabledChannel(context.Background(), target, domain.ListingChannelSpec{}, "group", "secret", "marker")
	assertBlocked("channel create", err)
	assertBlocked("billable channel test", adapter.TestChannel(context.Background(), target, 1, "model"))
	_, err = adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error { return nil })
	assertBlocked("group-model apply", err)
	_, err = adapter.RollbackGroupModelPricing(context.Background(), target, "revision-initial", desired)
	assertBlocked("group-model rollback", err)
	_, err = adapter.RollbackGroupModelPricingControlled(context.Background(), target, "revision-initial", desired,
		func() error { return nil })
	assertBlocked("controlled group-model rollback", err)

	site.mu.Lock()
	putAttempts, putCount := site.putAttempts, site.putCount
	site.mu.Unlock()
	if putAttempts != 0 || putCount != 0 {
		t.Fatalf("blocked target actions produced PUT attempts=%d successful=%d", putAttempts, putCount)
	}
}

func TestRemoteWriteGateBlocksTargetAdministratorLoginBeforeNetwork(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	factory := NewFactory(server.Client(), nil)
	factory.SetRemoteWriteGate(remotewrite.New(false))
	_, err := factory.AuthenticateNewAPI(context.Background(), server.URL, "root", "password")
	if !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("login error=%v, want remote-write gate", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("blocked login reached target %d times", requests.Load())
	}
}

func TestRemoteWriteGateProjectsTargetCapabilitiesAsReadOnlyWithoutHidingReads(t *testing.T) {
	site := newGroupModelPricingSite()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/channel/" {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"success":true,"data":{"items":[],"total":0}}`))
			return
		}
		site.ServeHTTP(response, request)
	}))
	defer server.Close()
	factory := NewFactory(server.Client(), nil)
	factory.SetRemoteWriteGate(remotewrite.New(false))
	target := domain.TargetSite{ID: 7, Name: "candidate", BaseURL: server.URL, Adapter: "newapi", Enabled: true, WriteMode: "auto"}
	matrix := factory.ValidateNewAPIManagementCredential(context.Background(), target,
		domain.CredentialPayload{Authorization: "Bearer management-token", UserID: 17})

	groupRatio := matrix.PricingOptions["GroupRatio"]
	if !groupRatio.Read.Available || groupRatio.Write.Available || groupRatio.Write.Status != "read_only" ||
		groupRatio.Write.Reason != remotewrite.DisabledMessage {
		t.Fatalf("GroupRatio capabilities=%#v", groupRatio)
	}
	if !matrix.GroupModelPricing.Read.Available || !matrix.GroupModelPricing.Preview.Available ||
		matrix.GroupModelPricing.Write.Available || matrix.GroupModelPricing.Write.Status != "read_only" ||
		matrix.GroupModelPricing.Write.Reason != remotewrite.DisabledMessage {
		t.Fatalf("group-model capabilities=%#v", matrix.GroupModelPricing)
	}
	if !matrix.Channels.Read.Available {
		t.Fatalf("channel read was hidden: %#v", matrix.Channels)
	}
	for name, capability := range map[string]domain.TargetCapability{
		"routing": matrix.Channels.RoutingWrite, "status": matrix.Channels.StatusWrite, "billable test": matrix.Channels.BillableTest,
	} {
		if capability.Available || capability.Status != "read_only" || capability.Reason != remotewrite.DisabledMessage {
			t.Fatalf("%s capability=%#v", name, capability)
		}
	}
	if !matrix.ReadOnly || matrix.ReadOnlyReason != remotewrite.DisabledMessage {
		t.Fatalf("matrix read-only projection=%#v", matrix)
	}
}
