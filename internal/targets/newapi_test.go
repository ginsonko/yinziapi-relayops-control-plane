package targets

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestNewAPITargetCredentialDoesNotFollowCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var escapedRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		escapedRequests.Add(1)
		if request.Header.Get("Authorization") != "" || request.Header.Get("New-Api-User") != "" {
			t.Error("management credential escaped to redirect destination")
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL+"/stolen", http.StatusPermanentRedirect)
	}))
	defer source.Close()
	adapter, target := newTargetAdapter(t, source)

	_, err := adapter.Snapshot(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "redirect escaped") {
		t.Fatalf("Snapshot error = %v, want cross-origin redirect rejection", err)
	}
	if got := escapedRequests.Load(); got != 0 {
		t.Fatalf("redirect destination requests = %d, want 0", got)
	}
}

type optionTarget struct {
	mu                 sync.Mutex
	ratios             map[string]string
	modelPrices        map[string]string
	usableGroups       map[string]string
	strictNumbers      bool
	ignoreWrites       bool
	ignoreUsableWrites bool
	putCount           int
	modelPricePutCount int
	usablePutCount     int
	authorization      string
	newAPIUser         string
	requestProblems    []string
}

func (target *optionTarget) handler(response http.ResponseWriter, request *http.Request) {
	target.mu.Lock()
	defer target.mu.Unlock()
	if got := request.Header.Get("Authorization"); got != "Bearer management-token" {
		target.requestProblems = append(target.requestProblems, "Authorization="+got)
	}
	if got := request.Header.Get("New-Api-User"); got != "17" {
		target.requestProblems = append(target.requestProblems, "New-Api-User="+got)
	}
	response.Header().Set("Content-Type", "application/json")
	switch request.Method {
	case http.MethodGet:
		encoded, _ := json.Marshal(target.ratios)
		usable, _ := json.Marshal(target.usableGroups)
		options := []map[string]string{{"key": "GroupRatio", "value": string(encoded)}}
		if target.usableGroups != nil {
			options = append(options, map[string]string{"key": "UserUsableGroups", "value": string(usable)})
		}
		if target.modelPrices != nil {
			encodedPrices, _ := json.Marshal(target.modelPrices)
			options = append(options, map[string]string{"key": "ModelPrice", "value": string(encodedPrices)})
		}
		_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "data": options})
	case http.MethodPut:
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(response, "invalid option request", http.StatusBadRequest)
			return
		}
		if input.Key == "UserUsableGroups" {
			target.usablePutCount++
			var next map[string]string
			if err := json.Unmarshal([]byte(input.Value), &next); err != nil {
				http.Error(response, "invalid UserUsableGroups JSON", http.StatusBadRequest)
				return
			}
			if !target.ignoreUsableWrites {
				target.usableGroups = next
			}
			fmt.Fprint(response, `{"success":true}`)
			return
		}
		if input.Key != "GroupRatio" && input.Key != "ModelPrice" {
			http.Error(response, "invalid option request", http.StatusBadRequest)
			return
		}
		if input.Key == "ModelPrice" {
			target.modelPricePutCount++
		} else {
			target.putCount++
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input.Value), &raw); err != nil {
			http.Error(response, "invalid GroupRatio JSON", http.StatusBadRequest)
			return
		}
		next := make(map[string]string, len(raw))
		for name, value := range raw {
			var number json.Number
			if err := json.Unmarshal(value, &number); err != nil {
				if target.strictNumbers {
					http.Error(response, "GroupRatio values must be JSON numbers", http.StatusBadRequest)
					return
				}
				var text string
				if err := json.Unmarshal(value, &text); err != nil {
					http.Error(response, "invalid GroupRatio value", http.StatusBadRequest)
					return
				}
				next[name] = text
				continue
			}
			next[name] = number.String()
		}
		if !target.ignoreWrites {
			if input.Key == "ModelPrice" {
				target.modelPrices = next
			} else {
				target.ratios = next
			}
		}
		fmt.Fprint(response, `{"success":true}`)
	default:
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (target *optionTarget) setRatios(values map[string]string) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.ratios = cloneTestRatios(values)
}

func (target *optionTarget) snapshot() (map[string]string, int, []string) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return cloneTestRatios(target.ratios), target.putCount, append([]string(nil), target.requestProblems...)
}

func (target *optionTarget) modelPriceSnapshot() (map[string]string, int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return cloneTestRatios(target.modelPrices), target.modelPricePutCount
}

func TestNewAPISnapshotReadsManagementOptionsAndCredentials(t *testing.T) {
	t.Parallel()
	upstream := &optionTarget{ratios: map[string]string{"stable": "0.1", "cheap": "0.05"}}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	snapshot, err := adapter.Snapshot(context.Background(), target)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.TargetSiteID != target.ID || snapshot.SourceURL != server.URL+"/api/option/" {
		t.Fatalf("snapshot identity = %#v", snapshot)
	}
	if snapshot.GroupRatio["cheap"] != "0.05" || snapshot.GroupRatio["stable"] != "0.1" {
		t.Fatalf("ratios = %#v", snapshot.GroupRatio)
	}
	if snapshot.Hash != HashRatios(snapshot.GroupRatio) || snapshot.Hash == "" {
		t.Fatalf("hash = %q", snapshot.Hash)
	}
	_, _, problems := upstream.snapshot()
	if len(problems) != 0 {
		t.Fatalf("management headers: %v", problems)
	}
}

func TestNewAPISnapshotRetriesTransientRateLimitWithoutWriting(t *testing.T) {
	t.Parallel()
	var optionReads atomic.Int64
	var writes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/option/":
			if optionReads.Add(1) == 1 {
				response.Header().Set("Retry-After", "0")
				http.Error(response, `{"success":false}`, http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(response, `{"success":true,"data":[{"key":"GroupRatio","value":"{\"default\":1}"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/option/group-model-pricing":
			http.NotFound(response, request)
		default:
			writes.Add(1)
			http.Error(response, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	snapshot, err := adapter.Snapshot(context.Background(), target)
	if err != nil {
		t.Fatalf("Snapshot after transient 429: %v", err)
	}
	if snapshot.GroupRatio["default"] != "1" || optionReads.Load() != 2 || writes.Load() != 0 {
		t.Fatalf("snapshot=%#v optionReads=%d writes=%d", snapshot, optionReads.Load(), writes.Load())
	}
	if IsDefinitiveRemoteRejection(remoteHTTPError("read", http.StatusTooManyRequests, nil)) {
		t.Fatal("HTTP 429 must remain transient")
	}
}

func TestNewAPIApplyWritesNumericRatiosAndReadsBack(t *testing.T) {
	t.Parallel()
	upstream := &optionTarget{ratios: map[string]string{"default": "1"}, strictNumbers: true}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	expected := HashRatios(map[string]string{"default": "1"})
	want := map[string]string{"default": "1", "economy": "0.050000000000"}
	readback, err := adapter.ApplyGroupRatio(context.Background(), target, expected, want)
	if err != nil {
		t.Fatalf("ApplyGroupRatio against NewAPI numeric contract: %v", err)
	}
	if readback.Hash != HashRatios(want) || readback.GroupRatio["economy"] != "0.05" {
		t.Fatalf("readback = %#v", readback)
	}
	_, puts, problems := upstream.snapshot()
	if puts != 1 || len(problems) != 0 {
		t.Fatalf("puts=%d header problems=%v", puts, problems)
	}
}

func TestNewAPIApplyRejectsStalePreviewBeforeWrite(t *testing.T) {
	t.Parallel()
	upstream := &optionTarget{ratios: map[string]string{"default": "1", "economy": "0.08"}}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	staleHash := HashRatios(map[string]string{"default": "1", "economy": "0.05"})
	_, err := adapter.ApplyGroupRatio(context.Background(), target, staleHash, map[string]string{"default": "1", "economy": "0.06"})
	if err == nil || !strings.Contains(err.Error(), "target changed since preview") {
		t.Fatalf("stale apply error = %v", err)
	}
	_, puts, _ := upstream.snapshot()
	if puts != 0 {
		t.Fatalf("stale apply performed %d writes, want 0", puts)
	}
}

func TestNewAPIApplyDetectsReadbackMismatch(t *testing.T) {
	t.Parallel()
	upstream := &optionTarget{ratios: map[string]string{"default": "1"}, ignoreWrites: true}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	_, err := adapter.ApplyGroupRatio(context.Background(), target, HashRatios(upstream.ratios), map[string]string{"default": "1", "economy": "0.05"})
	if err == nil || !strings.Contains(err.Error(), "readback differs") {
		t.Fatalf("readback mismatch error = %v", err)
	}
}

func TestNewAPIRollbackIsConditionalOnPostApplyHash(t *testing.T) {
	t.Parallel()
	applied := map[string]string{"default": "1", "economy": "0.06"}
	previous := map[string]string{"default": "1", "economy": "0.05"}
	upstream := &optionTarget{ratios: cloneTestRatios(applied)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	upstream.setRatios(map[string]string{"default": "1", "economy": "0.07"})
	_, err := adapter.RollbackGroupRatio(context.Background(), target, HashRatios(applied), previous)
	if err == nil || !strings.Contains(err.Error(), "target changed after apply") {
		t.Fatalf("conditional rollback error = %v", err)
	}
	_, puts, _ := upstream.snapshot()
	if puts != 0 {
		t.Fatalf("conditional rollback performed %d writes, want 0", puts)
	}

	upstream.setRatios(applied)
	readback, err := adapter.RollbackGroupRatio(context.Background(), target, HashRatios(applied), previous)
	if err != nil {
		t.Fatalf("RollbackGroupRatio: %v", err)
	}
	if readback.Hash != HashRatios(previous) {
		t.Fatalf("rollback readback = %#v", readback)
	}
}

func TestNewAPIGroupVisibilityUsesStringMapCASAndConditionalRollback(t *testing.T) {
	t.Parallel()
	before := map[string]string{"default": "Default", "vip": "VIP users"}
	desired := map[string]string{"default": "Default", "vip": "VIP users", "promo": "promo"}
	upstream := &optionTarget{ratios: map[string]string{"default": "1"}, usableGroups: cloneTestRatios(before)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	snapshot, err := adapter.Snapshot(context.Background(), target)
	if err != nil || !snapshot.UserUsableGroupsSupported || snapshot.UserUsableGroupsHash != HashStringMap(before) {
		t.Fatalf("visibility snapshot = %#v, %v", snapshot, err)
	}
	readback, err := adapter.ApplyGroupVisibility(context.Background(), target, HashStringMap(before), desired)
	if err != nil || readback.Hash != HashStringMap(desired) || readback.Values["vip"] != "VIP users" {
		t.Fatalf("visibility apply = %#v, %v", readback, err)
	}

	upstream.mu.Lock()
	upstream.usableGroups["manual"] = "Manual"
	putsBeforeConflict := upstream.usablePutCount
	upstream.mu.Unlock()
	if _, err := adapter.RollbackGroupVisibility(context.Background(), target, HashStringMap(desired), before); err == nil ||
		!strings.Contains(err.Error(), "changed after apply") {
		t.Fatalf("visibility rollback conflict = %v", err)
	}
	upstream.mu.Lock()
	if upstream.usablePutCount != putsBeforeConflict {
		t.Fatalf("visibility conflict performed a write: %d -> %d", putsBeforeConflict, upstream.usablePutCount)
	}
	upstream.usableGroups = cloneTestRatios(desired)
	upstream.mu.Unlock()
	rolledBack, err := adapter.RollbackGroupVisibility(context.Background(), target, HashStringMap(desired), before)
	if err != nil || rolledBack.Hash != HashStringMap(before) || rolledBack.Values["vip"] != "VIP users" {
		t.Fatalf("visibility rollback = %#v, %v", rolledBack, err)
	}
}

func TestNewAPIGroupVisibilityPreservesHistoricalExceptionsButRejectsNewOnes(t *testing.T) {
	t.Parallel()
	before := map[string]string{"default": "Default", "legacy-empty": "", " legacy-name ": "Legacy"}
	desired := cloneTestRatios(before)
	desired["promo"] = "Promo"
	upstream := &optionTarget{ratios: map[string]string{"default": "1"}, usableGroups: cloneTestRatios(before)}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	readback, err := adapter.ApplyGroupVisibility(context.Background(), target, HashStringMap(before), desired)
	if err != nil || readback.Values["legacy-empty"] != "" || readback.Values[" legacy-name "] != "Legacy" ||
		readback.Values["promo"] != "Promo" {
		t.Fatalf("historical visibility compatibility = %#v, %v", readback, err)
	}
	rolledBack, err := adapter.RollbackGroupVisibility(context.Background(), target, HashStringMap(desired), before)
	if err != nil || rolledBack.Hash != HashStringMap(before) || rolledBack.Values["legacy-empty"] != "" {
		t.Fatalf("historical visibility rollback = %#v, %v", rolledBack, err)
	}

	upstream.mu.Lock()
	putsBefore := upstream.usablePutCount
	upstream.mu.Unlock()
	invalid := cloneTestRatios(before)
	invalid["new-group"] = ""
	if _, err := adapter.ApplyGroupVisibility(context.Background(), target, HashStringMap(before), invalid); err == nil ||
		!strings.Contains(err.Error(), "label") {
		t.Fatalf("new invalid label error = %v", err)
	}
	modifiedLegacy := cloneTestRatios(before)
	modifiedLegacy["legacy-empty"] = " "
	if _, err := adapter.ApplyGroupVisibility(context.Background(), target, HashStringMap(before), modifiedLegacy); err == nil ||
		!strings.Contains(err.Error(), "label") {
		t.Fatalf("modified historical label error = %v", err)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.usablePutCount != putsBefore {
		t.Fatalf("invalid visibility mutation performed writes: %d -> %d", putsBefore, upstream.usablePutCount)
	}
}

func TestNewAPIDecimalOptionsPreserveHistoricalExceptionsForGroupRatioAndModelPrice(t *testing.T) {
	t.Parallel()
	beforeRatios := map[string]string{"default": "1", "": "2"}
	beforePrices := map[string]string{"existing": "0.1", " legacy-model ": "0.2"}
	upstream := &optionTarget{ratios: cloneTestRatios(beforeRatios), modelPrices: cloneTestRatios(beforePrices), strictNumbers: true}
	server := httptest.NewServer(http.HandlerFunc(upstream.handler))
	defer server.Close()

	adapter, target := newTargetAdapter(t, server)
	desiredRatios := cloneTestRatios(beforeRatios)
	desiredRatios["promo"] = "0.25"
	if _, err := adapter.ApplyGroupRatio(context.Background(), target, HashRatios(beforeRatios), desiredRatios); err != nil {
		t.Fatalf("GroupRatio historical compatibility: %v", err)
	}
	desiredPrices := cloneTestRatios(beforePrices)
	desiredPrices["new-model"] = "0.3"
	readback, err := adapter.ApplyPricingOption(context.Background(), target, "ModelPrice", HashDecimalMap(beforePrices), desiredPrices)
	if err != nil || readback.Values[" legacy-model "] != "0.2" || readback.Values["new-model"] != "0.3" {
		t.Fatalf("ModelPrice historical compatibility = %#v, %v", readback, err)
	}

	invalid := cloneTestRatios(desiredPrices)
	invalid["bad-model"] = "not-a-price"
	_, putsBefore := upstream.modelPriceSnapshot()
	if _, err := adapter.ApplyPricingOption(context.Background(), target, "ModelPrice", HashDecimalMap(desiredPrices), invalid); err == nil ||
		!strings.Contains(err.Error(), "non-negative decimal") {
		t.Fatalf("new invalid ModelPrice error = %v", err)
	}
	_, putsAfter := upstream.modelPriceSnapshot()
	if putsAfter != putsBefore {
		t.Fatalf("invalid ModelPrice mutation performed writes: %d -> %d", putsBefore, putsAfter)
	}
}

func newTargetAdapter(t *testing.T, server *httptest.Server) (*NewAPI, domain.TargetSite) {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatalf("open test vault: %v", err)
	}
	service := credentials.NewService(database, vault)
	credential := domain.Credential{Name: "management", Kind: "access-token", SiteURL: server.URL}
	if err := service.Create(context.Background(), &credential, domain.CredentialPayload{
		Authorization: "Bearer management-token",
		UserID:        17,
	}); err != nil {
		t.Fatalf("create management credential: %v", err)
	}
	target := domain.TargetSite{ID: 9, Name: "target", BaseURL: server.URL, CredentialID: credential.ID, Enabled: true}
	return &NewAPI{client: server.Client(), credentials: service}, target
}

func cloneTestRatios(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
