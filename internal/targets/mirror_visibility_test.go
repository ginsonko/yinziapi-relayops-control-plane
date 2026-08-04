package targets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReadMirrorVisibilityAcceptsBoundReceiptAndUsesGET(t *testing.T) {
	var requests atomic.Int64
	receipt := validMirrorVisibilityReceipt(41, "sale", []string{"model-b", "model-a"})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/api/channel/41/mirror_visibility" ||
			request.URL.Query().Get("group") != "sale" {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.String())
			http.Error(response, "unexpected request", http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer management-token" || request.Header.Get("New-Api-User") != "17" {
			t.Errorf("management authorization was not projected")
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "data": receipt})
	}))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	observed, err := adapter.ReadMirrorVisibility(context.Background(), target, 41, "sale")
	if err != nil {
		t.Fatalf("ReadMirrorVisibility: %v", err)
	}
	if requests.Load() != 1 || observed.EvidenceHash != receipt.EvidenceHash || observed.ModelCount != 2 {
		t.Fatalf("observed=%#v requests=%d", observed, requests.Load())
	}
}

func TestReadMirrorVisibilityRejectsMalformedOrMismatchedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MirrorVisibilityReceipt)
		want   string
	}{
		{name: "schema", mutate: func(value *MirrorVisibilityReceipt) { value.Schema = "future-v2" }, want: "unsupported mirror visibility schema"},
		{name: "binding", mutate: func(value *MirrorVisibilityReceipt) { value.Group = "other" }, want: "binding differs"},
		{name: "count", mutate: func(value *MirrorVisibilityReceipt) { value.ModelCount = 50_001 }, want: "outside the supported range"},
		{name: "hash", mutate: func(value *MirrorVisibilityReceipt) { value.ChannelStateHash = "not-a-hash" }, want: "canonical SHA-256"},
		{name: "cache mode", mutate: func(value *MirrorVisibilityReceipt) { value.CacheMode = "pretend" }, want: "unsupported mirror visibility cache mode"},
		{name: "evidence", mutate: func(value *MirrorVisibilityReceipt) { value.MarketplaceVisible = false }, want: "evidence hash does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			receipt := validMirrorVisibilityReceipt(41, "sale", []string{"model-a"})
			test.mutate(&receipt)
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "data": receipt})
			}))
			defer server.Close()
			adapter, target := newTargetAdapter(t, server)
			_, err := adapter.ReadMirrorVisibility(context.Background(), target, 41, "sale")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestReadMirrorVisibilityRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"success":true,"data":{"schema":"newapi-mirror-visibility-v1","unknown":true}}`,
		`{"success":true,"data":{"schema":"newapi-mirror-visibility-v1"} {}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte(body))
		}))
		adapter, target := newTargetAdapter(t, server)
		_, err := adapter.ReadMirrorVisibility(context.Background(), target, 41, "sale")
		server.Close()
		if err == nil {
			t.Fatalf("malformed body was accepted: %s", body)
		}
	}
}

func validMirrorVisibilityReceipt(channelID int64, group string, models []string) MirrorVisibilityReceipt {
	hash := strings.Repeat("a", 64)
	receipt := MirrorVisibilityReceipt{
		Schema: mirrorVisibilitySchema, ChannelID: channelID, Group: group,
		ChannelStateHash: hash, ModelSetHash: HashMirrorModelSet(group, models), ModelCount: len(models),
		ContractRevision: strings.Repeat("b", 64), MarketplaceHash: strings.Repeat("c", 64),
		AbilityHash: strings.Repeat("d", 64), CacheHash: strings.Repeat("e", 64),
		MarketplaceVisible: true, AbilityVisible: true, CacheVisible: true, CacheMode: "memory",
	}
	receipt.EvidenceHash = HashMirrorVisibilityReceipt(receipt)
	return receipt
}
