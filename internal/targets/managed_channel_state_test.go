package targets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

type managedChannelStateSite struct {
	mu          sync.Mutex
	state       domain.TargetManagedChannelState
	putCount    int
	failPutOnce bool
	failGetOnce bool
	getCount    int
	forbidden   bool
}

func (site *managedChannelStateSite) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	site.mu.Lock()
	defer site.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	if request.URL.Path != "/api/channel/41/managed_state" {
		http.NotFound(response, request)
		return
	}
	switch request.Method {
	case http.MethodGet:
		site.getCount++
		if site.failGetOnce {
			site.failGetOnce = false
			response.Header().Set("Retry-After", "0")
			http.Error(response, `{"success":false}`, http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"channel_id": site.state.ChannelID, "models": site.state.Models,
				"status": site.state.Status, "hash": site.state.Hash,
				"key": "must-never-leave-adapter", "base_url": "https://secret.example",
			},
		})
	case http.MethodPut:
		site.putCount++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		for _, field := range []string{"key", "base_url", "group", "model_mapping", "type"} {
			if _, exists := payload[field]; exists {
				site.forbidden = true
			}
		}
		modelsRaw, _ := payload["desired_models"].([]any)
		models := make([]string, 0, len(modelsRaw))
		for _, value := range modelsRaw {
			if model, ok := value.(string); ok {
				models = append(models, model)
			}
		}
		status := site.state.Status
		if raw, ok := payload["desired_status"].(float64); ok {
			status = int(raw)
		}
		site.state.Models = normalizeModels(models)
		site.state.Status = status
		site.state.Hash = "hash-after"
		if site.failPutOnce {
			site.failPutOnce = false
			http.Error(response, "response lost", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true, "data": map[string]any{"changed": true, "state": site.state},
		})
	default:
		http.Error(response, "method", http.StatusMethodNotAllowed)
	}
}

func TestManagedChannelStateReadRetriesTransientRateLimitWithoutWriting(t *testing.T) {
	site := &managedChannelStateSite{failGetOnce: true, state: domain.TargetManagedChannelState{
		ChannelID: 41, Models: []string{"model-a"}, Status: channelStatusEnabled, Hash: "hash-before",
	}}
	adapter, target, server := newManagedStateTarget(t, site, true)
	defer server.Close()

	state, err := adapter.ReadManagedChannelState(context.Background(), target, 41)
	if err != nil || state.Hash != "hash-before" {
		t.Fatalf("read after transient 429: state=%#v error=%v", state, err)
	}
	site.mu.Lock()
	reads, writes := site.getCount, site.putCount
	site.mu.Unlock()
	if reads != 2 || writes != 0 {
		t.Fatalf("reads=%d writes=%d", reads, writes)
	}
}

func newManagedStateTarget(t *testing.T, site *managedChannelStateSite, writes bool) (*NewAPI, domain.TargetSite, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(site)
	adapter, target := newTargetAdapter(t, server)
	target.Adapter = "newapi"
	target.WriteMode = "auto"
	adapter.remoteWrites = remotewrite.New(writes)
	return adapter, target, server
}

func TestManagedChannelStateNoOpAndCASWrite(t *testing.T) {
	site := &managedChannelStateSite{state: domain.TargetManagedChannelState{
		ChannelID: 41, Models: []string{"model-b", "model-a"}, Status: channelStatusEnabled, Hash: "hash-before",
	}}
	adapter, target, server := newManagedStateTarget(t, site, true)
	defer server.Close()

	state, err := adapter.ReadManagedChannelState(context.Background(), target, 41)
	if err != nil {
		t.Fatalf("read managed state: %v", err)
	}
	if !reflect.DeepEqual(state.Models, []string{"model-a", "model-b"}) {
		t.Fatalf("normalized models=%v", state.Models)
	}

	guardCalls := 0
	result, err := adapter.ApplyManagedChannelState(context.Background(), target, 41, state.Hash,
		[]string{"model-b", "model-a", "model-b"}, nil, "operation-noop", func() error {
			guardCalls++
			return nil
		})
	if err != nil || result.ResultState != "verified_success" || result.Changed || guardCalls != 0 {
		t.Fatalf("no-op result=%#v guard=%d error=%v", result, guardCalls, err)
	}

	disabled := channelStatusManuallyDisabled
	result, err = adapter.ApplyManagedChannelState(context.Background(), target, 41, state.Hash,
		[]string{"model-c"}, &disabled, "operation-write", func() error {
			guardCalls++
			return nil
		})
	if err != nil || result.ResultState != "verified_success" || !result.Changed || guardCalls != 1 {
		t.Fatalf("write result=%#v guard=%d error=%v", result, guardCalls, err)
	}
	site.mu.Lock()
	putCount, forbidden := site.putCount, site.forbidden
	site.mu.Unlock()
	if putCount != 1 || forbidden {
		t.Fatalf("PUT count=%d forbidden fields=%v", putCount, forbidden)
	}
}

func TestManagedChannelStateRecoversLostWriteResponseAndRejectsStaleHash(t *testing.T) {
	site := &managedChannelStateSite{state: domain.TargetManagedChannelState{
		ChannelID: 41, Models: []string{"old"}, Status: channelStatusEnabled, Hash: "hash-before",
	}, failPutOnce: true}
	adapter, target, server := newManagedStateTarget(t, site, true)
	defer server.Close()

	result, err := adapter.ApplyManagedChannelState(context.Background(), target, 41, "hash-before",
		[]string{"new"}, nil, "operation-lost-response", func() error { return nil })
	if err != nil || result.ResultState != "verified_success" || !result.Changed || result.ReadbackHash != "hash-after" {
		t.Fatalf("lost-response recovery=%#v error=%v", result, err)
	}

	result, err = adapter.ApplyManagedChannelState(context.Background(), target, 41, "hash-before",
		[]string{"other"}, nil, "operation-stale", func() error { t.Fatal("stale write reached guard"); return nil })
	if err == nil || result.ResultState != "conflict_before_write" {
		t.Fatalf("stale result=%#v error=%v", result, err)
	}
	site.mu.Lock()
	putCount := site.putCount
	site.mu.Unlock()
	if putCount != 1 {
		t.Fatalf("stale hash produced extra PUT; count=%d", putCount)
	}
}

func TestManagedChannelStateWriteGateBlocksBeforeNetwork(t *testing.T) {
	var requests atomic.Int64
	site := &managedChannelStateSite{state: domain.TargetManagedChannelState{
		ChannelID: 41, Models: []string{"old"}, Status: channelStatusEnabled, Hash: "hash-before",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		site.ServeHTTP(response, request)
	}))
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	adapter.remoteWrites = remotewrite.New(false)

	result, err := adapter.ApplyManagedChannelState(context.Background(), target, 41, "hash-before",
		[]string{"new"}, nil, "operation-disabled", func() error { return nil })
	if !errors.Is(err, remotewrite.ErrDisabled) || result.ResultState != "rejected_before_write" {
		t.Fatalf("gate result=%#v error=%v", result, err)
	}
	if requests.Load() != 0 {
		t.Fatalf("write-disabled managed state reached network %d times", requests.Load())
	}
}
