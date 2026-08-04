package targets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type groupModelPricingSite struct {
	mu          sync.Mutex
	contract    map[string]any
	revision    string
	next        int
	putCount    int
	putAttempts int
	putFailures int
	putStatus   int
	putRevision string
	fenceChecks int
	numericBody bool
	getCount    int
	getFailures int
}

func newGroupModelPricingSite() *groupModelPricingSite {
	return &groupModelPricingSite{contract: map[string]any{"schema_version": 1, "groups": map[string]any{}}, revision: "revision-initial", next: 1}
}

func (site *groupModelPricingSite) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer management-token" || request.Header.Get("New-Api-User") != "17" {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	site.mu.Lock()
	defer site.mu.Unlock()
	switch {
	case request.URL.Path == "/api/option/" && request.Method == http.MethodGet:
		fmt.Fprint(response, `{"success":true,"data":[{"key":"GroupRatio","value":"{\"default\":1}"}]}`)
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodGet:
		fmt.Fprint(response, `{"success":true,"data":{"items":[],"total":0}}`)
	case request.URL.Path == "/api/option/group-model-pricing" && request.Method == http.MethodGet:
		site.getCount++
		if site.getFailures > 0 {
			site.getFailures--
			response.Header().Set("Retry-After", "0")
			http.Error(response, `{"success":false}`, http.StatusTooManyRequests)
			return
		}
		site.writeContractResponse(response, site.contract, site.revision, "", true)
	case request.URL.Path == "/api/option/group-model-pricing/preview" && request.Method == http.MethodPost:
		var input struct {
			ExpectedRevision string         `json:"expected_revision"`
			Contract         map[string]any `json:"contract"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(response, "bad preview", http.StatusBadRequest)
			return
		}
		desired := cloneAnyMap(input.Contract)
		desiredRevision := site.revisionFor(desired)
		site.writeContractResponse(response, desired, desiredRevision, site.revision, input.ExpectedRevision == "" || input.ExpectedRevision == site.revision)
	case request.URL.Path == "/api/option/group-model-pricing" && request.Method == http.MethodPut:
		var input struct {
			ExpectedRevision string         `json:"expected_revision"`
			Contract         map[string]any `json:"contract"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(response, "bad update", http.StatusBadRequest)
			return
		}
		if input.ExpectedRevision != site.revision {
			http.Error(response, "revision conflict", http.StatusConflict)
			return
		}
		site.putAttempts++
		if site.putFailures > 0 {
			site.putFailures--
			if site.putRevision != "" {
				site.revision = site.putRevision
			}
			status := site.putStatus
			if status == 0 {
				status = http.StatusBadRequest
			}
			http.Error(response, "database is locked", status)
			return
		}
		site.putCount++
		site.numericBody = contractContainsNumericModelPrice(input.Contract)
		site.contract = cloneAnyMap(input.Contract)
		site.revision = site.revisionFor(site.contract)
		site.writeContractResponse(response, site.contract, site.revision, "", true)
	default:
		http.NotFound(response, request)
	}
}

func TestGroupModelPricingReadRetriesTransientRateLimitWithoutWriting(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	site.getFailures = 1
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)

	contract, err := adapter.ReadGroupModelPricing(context.Background(), target)
	if err != nil || contract.Revision != "revision-initial" {
		t.Fatalf("read after transient 429: contract=%#v error=%v", contract, err)
	}
	site.mu.Lock()
	reads, writeAttempts := site.getCount, site.putAttempts
	site.mu.Unlock()
	if reads != 2 || writeAttempts != 0 {
		t.Fatalf("reads=%d writeAttempts=%d", reads, writeAttempts)
	}
}

func TestGroupModelPricingWriteRecoversFromTransientPersistenceFailure(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	site.putFailures = 1
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	fenceChecks := 0
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error {
		fenceChecks++
		return nil
	})
	if err != nil || result.ResultState != "verified_success" || result.ReadbackHash != "revision-special-video" {
		t.Fatalf("transient apply result = %#v, err = %v", result, err)
	}
	site.mu.Lock()
	attempts, writes := site.putAttempts, site.putCount
	site.mu.Unlock()
	if attempts != 2 || writes != 1 || fenceChecks != 2 {
		t.Fatalf("attempts = %d, successful writes = %d, fence checks = %d", attempts, writes, fenceChecks)
	}
}

func TestGroupModelPricingWriteRechecksFenceBeforeRetry(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	site.putFailures = 1
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	fenceLost := errors.New("fenced lease lost")
	fenceChecks := 0
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error {
		fenceChecks++
		if fenceChecks == 2 {
			return fenceLost
		}
		return nil
	})
	if !errors.Is(err, fenceLost) || result.ResultState != "rejected_before_write" {
		t.Fatalf("fenced retry result = %#v, err = %v", result, err)
	}
	site.mu.Lock()
	attempts, writes, revision := site.putAttempts, site.putCount, site.revision
	site.mu.Unlock()
	if fenceChecks != 2 || attempts != 1 || writes != 0 || revision != "revision-initial" {
		t.Fatalf("fence checks=%d attempts=%d writes=%d revision=%q", fenceChecks, attempts, writes, revision)
	}
}

func TestGroupModelPricingWritePersistentFailureIsBounded(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	site.putFailures = 10
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error { return nil })
	if err == nil || result.ResultState != "rejected_before_write" {
		t.Fatalf("persistent apply result = %#v, err = %v", result, err)
	}
	site.mu.Lock()
	attempts, writes, revision := site.putAttempts, site.putCount, site.revision
	site.mu.Unlock()
	if attempts != groupModelPricingWriteAttempts || writes != 0 || revision != "revision-initial" {
		t.Fatalf("attempts = %d, writes = %d, revision = %q", attempts, writes, revision)
	}
}

func TestGroupModelPricingWriteDoesNotOverwriteRevisionChangedDuringRetry(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	site.putFailures = 1
	site.putRevision = "revision-external"
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "changed while recovering") {
		t.Fatalf("external-change result = %#v, err = %v", result, err)
	}
	site.mu.Lock()
	attempts, writes, revision := site.putAttempts, site.putCount, site.revision
	site.mu.Unlock()
	if attempts != 1 || writes != 0 || revision != "revision-external" {
		t.Fatalf("attempts = %d, writes = %d, revision = %q", attempts, writes, revision)
	}
}

func (site *groupModelPricingSite) revisionFor(contract map[string]any) string {
	encoded, _ := json.Marshal(contract)
	if !strings.Contains(string(encoded), "special-video") {
		return fmt.Sprintf("revision-empty-%d", site.next)
	}
	return "revision-special-video"
}

func (site *groupModelPricingSite) writeContractResponse(response http.ResponseWriter, contract map[string]any, revision,
	currentRevision string, matches bool) {
	copy := cloneAnyMap(contract)
	copy["revision"] = revision
	payload := map[string]any{"supported": true, "schema_version": 1, "revision": revision, "contract": copy,
		"expected_revision_matches": matches}
	if currentRevision != "" {
		payload["current_revision"] = currentRevision
	}
	_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "data": payload})
}

func TestGroupModelPricingAdapterPreviewApplyReadbackAndRollback(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	current, err := adapter.ReadGroupModelPricing(context.Background(), target)
	if err != nil || current.Revision != "revision-initial" || len(current.Groups) != 0 {
		t.Fatalf("initial contract = %#v, err = %v", current, err)
	}
	price := "1.625000"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {
			"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price,
				TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"}, Source: "relayops:test"},
		},
	}}
	preview, err := adapter.PreviewGroupModelPricing(context.Background(), target, current.Revision, desired)
	if err != nil {
		t.Fatalf("PreviewGroupModelPricing: %v", err)
	}
	if !preview.ExpectedRevisionMatches || preview.NoChange || preview.DesiredRevision != "revision-special-video" || len(preview.Diff) != 1 {
		t.Fatalf("preview = %#v", preview)
	}
	fenceChecks := 0
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, current.Revision, preview.Desired, func() error {
		fenceChecks++
		return nil
	})
	if err != nil {
		t.Fatalf("ApplyGroupModelPricingControlled: %v", err)
	}
	if result.ResultState != "verified_success" || result.ReadbackHash != "revision-special-video" || fenceChecks != 1 {
		t.Fatalf("apply result = %#v, fence checks = %d", result, fenceChecks)
	}
	site.mu.Lock()
	putCount, numericBody := site.putCount, site.numericBody
	site.mu.Unlock()
	if putCount != 1 || !numericBody {
		t.Fatalf("writes = %d, numeric model_price = %t", putCount, numericBody)
	}

	idempotent, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, "revision-initial", desired, func() error {
		return nil
	})
	if err != nil || idempotent.ResultState != "verified_success" || idempotent.ReadbackHash != "revision-special-video" {
		t.Fatalf("lost-response retry result = %#v, err = %v", idempotent, err)
	}
	rolledBack, err := adapter.RollbackGroupModelPricing(context.Background(), target, "revision-special-video", current)
	if err != nil {
		t.Fatalf("RollbackGroupModelPricing: %v", err)
	}
	if len(rolledBack.Groups) != 0 || rolledBack.Revision == "revision-special-video" {
		t.Fatalf("rollback = %#v", rolledBack)
	}
}

func TestControlledGroupModelPricingRollbackChecksFenceBeforeEveryAttempt(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	current, err := adapter.ReadGroupModelPricing(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, current.Revision, desired,
		func() error { return nil })
	if err != nil || result.ReadbackHash != "revision-special-video" {
		t.Fatalf("apply before rollback = %#v, err = %v", result, err)
	}

	site.mu.Lock()
	site.putFailures = 1
	site.mu.Unlock()
	fenceChecks := 0
	rolledBack, err := adapter.RollbackGroupModelPricingControlled(context.Background(), target,
		"revision-special-video", current, func() error {
			fenceChecks++
			return nil
		})
	if err != nil || rolledBack.Revision == "revision-special-video" {
		t.Fatalf("controlled rollback = %#v, err = %v", rolledBack, err)
	}
	if fenceChecks != 2 {
		t.Fatalf("controlled rollback fence checks = %d, want 2", fenceChecks)
	}
}

func TestControlledGroupModelPricingRollbackFenceFailureSendsNoWrite(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"

	current, err := adapter.ReadGroupModelPricing(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	price := "0.2"
	desired := domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{
		"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &price}},
	}}
	result, err := adapter.ApplyGroupModelPricingControlled(context.Background(), target, current.Revision, desired,
		func() error { return nil })
	if err != nil || result.ReadbackHash != "revision-special-video" {
		t.Fatalf("apply before guarded rollback = %#v, err = %v", result, err)
	}
	site.mu.Lock()
	attemptsBefore := site.putAttempts
	site.mu.Unlock()

	fenceLost := errors.New("fenced lease lost")
	_, err = adapter.RollbackGroupModelPricingControlled(context.Background(), target,
		"revision-special-video", current, func() error { return fenceLost })
	if !errors.Is(err, fenceLost) {
		t.Fatalf("controlled rollback fence error = %v", err)
	}
	site.mu.Lock()
	attemptsAfter, revision := site.putAttempts, site.revision
	site.mu.Unlock()
	if attemptsAfter != attemptsBefore || revision != "revision-special-video" {
		t.Fatalf("guarded rollback attempts=%d want=%d revision=%q", attemptsAfter, attemptsBefore, revision)
	}
}

func TestGroupModelPricingCapabilityAndOldTargetFallback(t *testing.T) {
	t.Parallel()
	site := newGroupModelPricingSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	target.WriteMode = "auto"
	matrix := adapter.Capabilities(context.Background(), target)
	if !matrix.GroupModelPricing.Read.Available || !matrix.GroupModelPricing.Preview.Available ||
		!matrix.GroupModelPricing.Write.Available || matrix.GroupModelPricing.Revision != "revision-initial" {
		t.Fatalf("group-model capability = %#v", matrix.GroupModelPricing)
	}

	legacyServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/option/" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"success":true,"data":[{"key":"GroupRatio","value":"{\"default\":1}"}]}`)
	}))
	defer legacyServer.Close()
	legacyAdapter, legacyTarget := newTargetAdapter(t, legacyServer)
	snapshot, err := legacyAdapter.Snapshot(context.Background(), legacyTarget)
	if err != nil {
		t.Fatalf("legacy Snapshot: %v", err)
	}
	if snapshot.GroupModelPricingSupported || snapshot.GroupModelPricing != nil || snapshot.GroupModelPricingError != "" {
		t.Fatalf("legacy group-model state = %#v", snapshot)
	}
}

func TestGroupModelPricingValidationRejectsUnrepresentableSetting(t *testing.T) {
	t.Parallel()
	completion := "3"
	_, _, err := contractToWire(domain.GroupModelPricingContract{SchemaVersion: 1,
		Groups: map[string]map[string]domain.GroupModelPricingSetting{"g": {"m": {
			BillingMode: "ratio", CompletionRatio: &completion,
		}}}})
	if err == nil || !strings.Contains(err.Error(), "requires model_ratio") {
		t.Fatalf("validation error = %v", err)
	}
}

func contractContainsNumericModelPrice(contract map[string]any) bool {
	groups, ok := contract["groups"].(map[string]any)
	if !ok {
		return false
	}
	models, ok := groups["special-video"].(map[string]any)
	if !ok {
		return false
	}
	setting, ok := models["jimeng-video"].(map[string]any)
	if !ok {
		return false
	}
	_, numeric := setting["model_price"].(float64)
	return numeric
}

func cloneAnyMap(source map[string]any) map[string]any {
	encoded, _ := json.Marshal(source)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}
