package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type automationTarget struct {
	mu                sync.Mutex
	ratios            map[string]string
	pricingModelRatio string
	writeFailure      bool
	writes            int
	snapshotReads     int
	pricingReads      int
	optionStatus      int
}

func (target *automationTarget) serveHTTP(response http.ResponseWriter, request *http.Request) {
	target.mu.Lock()
	defer target.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	if request.Method == http.MethodGet && request.URL.Path == "/api/pricing" {
		target.pricingReads++
		modelRatio := target.pricingModelRatio
		if modelRatio == "" {
			modelRatio = "1"
		}
		fmt.Fprintf(response, `{"success":true,"data":[{"model_name":"gpt-5.6-sol","quota_type":0,"model_ratio":%s,"completion_ratio":1,"enable_groups":["default","economy"]}],"group_ratio":{"default":1,"economy":0.05}}`, modelRatio)
		return
	}
	switch request.Method {
	case http.MethodGet:
		target.snapshotReads++
		if target.optionStatus != 0 && target.optionStatus != http.StatusOK {
			response.Header().Set("Retry-After", "0")
			http.Error(response, "target read failed", target.optionStatus)
			return
		}
		encoded, _ := json.Marshal(target.ratios)
		fmt.Fprintf(response, `{"success":true,"data":[{"key":"GroupRatio","value":%q}]}`, string(encoded))
	case http.MethodPut:
		target.writes++
		if target.writeFailure {
			http.Error(response, "simulated write failure", http.StatusBadGateway)
			return
		}
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Key != "GroupRatio" {
			http.Error(response, "invalid request", http.StatusBadRequest)
			return
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(input.Value), &raw); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		values := make(map[string]string, len(raw))
		for name, value := range raw {
			var number json.Number
			if err := json.Unmarshal(value, &number); err == nil {
				values[name] = number.String()
				continue
			}
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			values[name] = text
		}
		target.ratios = values
		fmt.Fprint(response, `{"success":true}`)
	default:
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (target *automationTarget) set(name, value string) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.ratios[name] = value
}

func (target *automationTarget) value(name string) (string, int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.ratios[name], target.writes
}

func (target *automationTarget) setPricingModelRatio(value string) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.pricingModelRatio = value
}

func (target *automationTarget) reads() (snapshot, pricing int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.snapshotReads, target.pricingReads
}

func (target *automationTarget) resetReads() {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.snapshotReads = 0
	target.pricingReads = 0
}

func (target *automationTarget) setOptionStatus(status int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.optionStatus = status
}

type automationFixture struct {
	store   *store.Store
	service *Service
	policy  domain.PricingPolicy
	target  *automationTarget
	server  *httptest.Server
	cleanup func()
}

type contractAutomationTarget struct {
	mu       sync.Mutex
	ratios   map[string]string
	contract map[string]any
	revision string
	writes   int
}

func newContractAutomationTarget() *contractAutomationTarget {
	contract := map[string]any{"schema_version": 1, "groups": map[string]any{"economy": map[string]any{
		"video-model": map[string]any{"billing_mode": "fixed_price", "model_price": 1.0,
			"task_profile": map[string]any{"unit": "per_request"}},
	}}}
	return &contractAutomationTarget{ratios: map[string]string{"default": "1", "economy": "1"}, contract: contract,
		revision: contractAutomationRevision(contract)}
}

func (target *contractAutomationTarget) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	target.mu.Lock()
	defer target.mu.Unlock()
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/pricing":
		fmt.Fprint(response, `{"success":true,"data":[{"model_name":"video-model","quota_type":1,"model_price":1,"enable_groups":["economy"]}],"group_ratio":{"default":1,"economy":1}}`)
	case request.Method == http.MethodGet && request.URL.Path == "/api/option/":
		encoded, _ := json.Marshal(target.ratios)
		fmt.Fprintf(response, `{"success":true,"data":[{"key":"GroupRatio","value":%q}]}`, string(encoded))
	case request.Method == http.MethodGet && request.URL.Path == "/api/channel/":
		fmt.Fprint(response, `{"success":true,"data":{"items":[],"total":0}}`)
	case request.Method == http.MethodGet && request.URL.Path == "/api/option/group-model-pricing":
		target.writeContractResponse(response, target.contract, target.revision, "", true)
	case request.Method == http.MethodPost && request.URL.Path == "/api/option/group-model-pricing/preview":
		var input struct {
			ExpectedRevision string         `json:"expected_revision"`
			Contract         map[string]any `json:"contract"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(response, "invalid preview", http.StatusBadRequest)
			return
		}
		desired := cloneAutomationContract(input.Contract)
		desiredRevision := contractAutomationRevision(desired)
		target.writeContractResponse(response, desired, desiredRevision, target.revision,
			input.ExpectedRevision == "" || input.ExpectedRevision == target.revision)
	case request.Method == http.MethodPut && request.URL.Path == "/api/option/group-model-pricing":
		var input struct {
			ExpectedRevision string         `json:"expected_revision"`
			Contract         map[string]any `json:"contract"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(response, "invalid update", http.StatusBadRequest)
			return
		}
		if input.ExpectedRevision != target.revision {
			http.Error(response, "revision conflict", http.StatusConflict)
			return
		}
		target.contract = cloneAutomationContract(input.Contract)
		target.revision = contractAutomationRevision(target.contract)
		target.writes++
		target.writeContractResponse(response, target.contract, target.revision, "", true)
	default:
		http.NotFound(response, request)
	}
}

func (target *contractAutomationTarget) writeContractResponse(response http.ResponseWriter, contract map[string]any,
	revision, currentRevision string, matches bool) {
	copy := cloneAutomationContract(contract)
	copy["revision"] = revision
	data := map[string]any{"supported": true, "schema_version": 1, "revision": revision, "contract": copy,
		"expected_revision_matches": matches}
	if currentRevision != "" {
		data["current_revision"] = currentRevision
	}
	_ = json.NewEncoder(response).Encode(map[string]any{"success": true, "data": data})
}

func (target *contractAutomationTarget) state() (string, int) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.revision, target.writes
}

func (target *contractAutomationTarget) replaceContract(contract map[string]any) {
	target.mu.Lock()
	defer target.mu.Unlock()
	target.contract = cloneAutomationContract(contract)
	target.revision = contractAutomationRevision(target.contract)
}

func contractAutomationRevision(contract map[string]any) string {
	copy := cloneAutomationContract(contract)
	delete(copy, "revision")
	encoded, _ := json.Marshal(copy)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func cloneAutomationContract(source map[string]any) map[string]any {
	encoded, _ := json.Marshal(source)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}

type contractAutomationFixture struct {
	store   *store.Store
	service *Service
	policy  domain.PricingPolicy
	target  *contractAutomationTarget
	server  *httptest.Server
	cleanup func()
}

func TestProposalLifecycleIsIdempotentAndConditionallyReversible(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()

	proposal, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal.Status != "pending" || proposal.ExpectedOld != "0.05" || proposal.ProposedValue != "0.060000000000" || proposal.ExpectedHash == "" {
		t.Fatalf("proposal = %#v", proposal)
	}
	duplicate, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil || duplicate.ID != proposal.ID {
		t.Fatalf("idempotent Propose = %#v, %v", duplicate, err)
	}

	applied, err := fixture.service.Apply(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Status != "applied" || !testDecimalEqual(applied.ReadbackValue, proposal.ProposedValue) {
		t.Fatalf("applied proposal = %#v", applied)
	}
	decision, err := fixture.store.GetDecisionSnapshot(ctx, proposal.ID)
	if err != nil || decision.DecisionKind != "approve" || decision.ModelVersion == "" || len(decision.Payload) == 0 {
		t.Fatalf("immutable decision snapshot = %#v, %v", decision, err)
	}
	effect, err := fixture.store.GetDecisionEffect(ctx, proposal.ID)
	if err != nil || effect.ReadbackHash == "" || !testDecimalEqual(effect.ReadbackValue, proposal.ProposedValue) {
		t.Fatalf("decision effect receipt = %#v, %v", effect, err)
	}
	exposure, err := fixture.store.GetDecisionExposure(ctx, proposal.ID)
	if err != nil || exposure.EndedAt != nil || exposure.StartHash != effect.ReadbackHash {
		t.Fatalf("active decision exposure = %#v, %v", exposure, err)
	}
	value, writes := fixture.target.value("economy")
	if !testDecimalEqual(value, proposal.ProposedValue) || writes != 1 {
		t.Fatalf("target after apply value=%q writes=%d", value, writes)
	}
	replayed, err := fixture.service.Apply(ctx, proposal.ID)
	if err != nil || replayed.Status != "applied" {
		t.Fatalf("idempotent Apply = %#v, %v", replayed, err)
	}
	_, writes = fixture.target.value("economy")
	if writes != 1 {
		t.Fatalf("idempotent Apply repeated write; writes=%d", writes)
	}

	rolledBack, err := fixture.service.Rollback(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack.Status != "rolled_back" || rolledBack.ReadbackValue != "0.05" {
		t.Fatalf("rolled-back proposal = %#v", rolledBack)
	}
	exposure, err = fixture.store.GetDecisionExposure(ctx, proposal.ID)
	if err != nil || exposure.EndedAt == nil || exposure.EndReason != "rolled_back" || exposure.EndHash == "" {
		t.Fatalf("closed decision exposure = %#v, %v", exposure, err)
	}
	value, writes = fixture.target.value("economy")
	if value != "0.05" || writes != 2 {
		t.Fatalf("target after rollback value=%q writes=%d", value, writes)
	}
	if _, err := fixture.service.Rollback(ctx, proposal.ID); err == nil || !strings.Contains(err.Error(), "only an applied proposal") {
		t.Fatalf("second rollback error = %v", err)
	}
	replacement, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("Propose after terminal rollback: %v", err)
	}
	if replacement.ID == proposal.ID || replacement.Status != "pending" {
		t.Fatalf("terminal proposal was incorrectly reused: old=%d replacement=%#v", proposal.ID, replacement)
	}
}

func TestGroupModelProposalKeepsDecimalFieldsAndUsesTypedRevisionsForApplyAndRollback(t *testing.T) {
	t.Parallel()
	fixture := newGroupModelAutomationFixture(t)
	defer fixture.cleanup()
	ctx := context.Background()

	proposal, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal.Status != "pending" || !testDecimalEqual(proposal.ExpectedOld, "1") ||
		!testDecimalEqual(proposal.ProposedValue, "1") {
		t.Fatalf("contract proposal leaked revisions into decimal fields: %#v", proposal)
	}
	if proposal.ExpectedHash == "" || proposal.GroupModelPricing.ExpectedRevision != proposal.ExpectedHash ||
		proposal.GroupModelPricing.DesiredRevision == "" || proposal.GroupModelPricing.DesiredRevision == proposal.ProposedValue {
		t.Fatalf("typed contract revisions are incomplete: %#v", proposal.GroupModelPricing)
	}

	applied, err := fixture.service.Apply(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Status != "applied" || applied.ReadbackValue != proposal.GroupModelPricing.DesiredRevision ||
		applied.GroupModelPricingReceipt.AppliedRevision != proposal.GroupModelPricing.DesiredRevision {
		t.Fatalf("applied contract proposal = %#v", applied)
	}
	decision, err := fixture.store.GetDecisionSnapshot(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recommendation struct {
		ForecastReady    bool   `json:"forecast_ready"`
		PricingMode      string `json:"pricing_mode"`
		CurrentRevision  string `json:"current_revision"`
		ForecastCoverage string `json:"forecast_coverage"`
	}
	if err := json.Unmarshal(decision.Payload, &recommendation); err != nil {
		t.Fatal(err)
	}
	if !recommendation.ForecastReady || recommendation.PricingMode != "group_model_contract" ||
		recommendation.CurrentRevision != proposal.GroupModelPricing.ExpectedRevision || recommendation.ForecastCoverage != "1/1" {
		t.Fatalf("immutable group-model decision snapshot = %#v", recommendation)
	}
	_, writes := fixture.target.state()
	if writes != 1 {
		t.Fatalf("contract writes after apply = %d", writes)
	}
	replayed, err := fixture.service.Apply(ctx, proposal.ID)
	if err != nil || replayed.Status != "applied" {
		t.Fatalf("idempotent Apply = %#v, %v", replayed, err)
	}
	_, writes = fixture.target.state()
	if writes != 1 {
		t.Fatalf("idempotent Apply repeated contract write: %d", writes)
	}

	rolledBack, err := fixture.service.Rollback(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack.Status != "rolled_back" || rolledBack.GroupModelPricingReceipt.RollbackStatus == "" {
		t.Fatalf("rolled back contract proposal = %#v", rolledBack)
	}
	currentRevision, writes := fixture.target.state()
	if currentRevision != proposal.GroupModelPricing.ExpectedRevision || writes != 2 {
		t.Fatalf("target after rollback revision=%s writes=%d", currentRevision, writes)
	}
}

func TestGroupModelObservationRefreshCreatesNoActionableProposalOrWrite(t *testing.T) {
	t.Parallel()
	fixture := newGroupModelAutomationFixture(t)
	defer fixture.cleanup()
	ctx := context.Background()

	first, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Apply(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(ctx, fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Evidence.ResponseHash = "supplier-contract-catalog-refreshed"
	snapshot.Evidence.ObservedAt = snapshot.Evidence.ObservedAt.Add(time.Minute)
	if err := fixture.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	second, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "no_change" || second.GroupModelPricing.DesiredRevision != second.GroupModelPricing.ExpectedRevision {
		t.Fatalf("observation-only refresh created actionable proposal: %#v", second)
	}
	_, writes := fixture.target.state()
	if writes != 1 {
		t.Fatalf("observation-only refresh performed %d contract writes, want initial apply only", writes)
	}
}

func TestGroupModelRollbackRefusesExternalContractEdit(t *testing.T) {
	t.Parallel()
	fixture := newGroupModelAutomationFixture(t)
	defer fixture.cleanup()
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Apply(context.Background(), proposal.ID); err != nil {
		t.Fatal(err)
	}
	fixture.target.replaceContract(map[string]any{"schema_version": 1, "groups": map[string]any{"economy": map[string]any{
		"video-model": map[string]any{"billing_mode": "fixed_price", "model_price": 9.0,
			"task_profile": map[string]any{"unit": "per_request"}},
	}}})
	if _, err := fixture.service.Rollback(context.Background(), proposal.ID); err == nil || !strings.Contains(err.Error(), "edited after apply") {
		t.Fatalf("unsafe contract rollback error = %v", err)
	}
	_, writes := fixture.target.state()
	if writes != 1 {
		t.Fatalf("unsafe rollback wrote over external contract: writes=%d", writes)
	}
}

func TestGroupModelConfirmedUpstreamIncreaseExecutesAutomatically(t *testing.T) {
	t.Parallel()
	fixture := newGroupModelAutomationFixture(t)
	defer fixture.cleanup()
	ctx := context.Background()
	snapshot, err := fixture.store.LatestSnapshot(ctx, fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Evidence.ObservedAt = snapshot.Evidence.ObservedAt.Add(time.Second)
	if err := fixture.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	policy := fixture.policy
	policy.ExecutionMode = "auto"
	if err := fixture.store.UpdatePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	fixture.service.ReconcileSupplier(ctx, policy.SupplierID)
	proposals, err := fixture.store.ListProposals(ctx, "", 10)
	if err != nil || len(proposals) == 0 {
		t.Fatalf("automatic proposals = %#v, %v", proposals, err)
	}
	if proposals[0].Status != "applied" || proposals[0].Mode != "auto" ||
		proposals[0].GroupModelPricingReceipt.AppliedRevision == "" {
		t.Fatalf("confirmed upstream increase was not automatically protected: %#v", proposals[0])
	}
	_, writes := fixture.target.state()
	if writes != 1 {
		t.Fatalf("automatic contract writes = %d", writes)
	}
}

func TestRollbackRefusesToOverwriteLaterManualTargetChange(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Apply(context.Background(), proposal.ID); err != nil {
		t.Fatal(err)
	}
	fixture.target.set("economy", "0.08")
	if _, err := fixture.service.Rollback(context.Background(), proposal.ID); err == nil || !strings.Contains(err.Error(), "edited after apply") {
		t.Fatalf("unsafe rollback error = %v", err)
	}
	value, writes := fixture.target.value("economy")
	if value != "0.08" || writes != 1 {
		t.Fatalf("unsafe rollback changed target value=%q writes=%d", value, writes)
	}
	stored, err := fixture.store.GetProposal(context.Background(), proposal.ID)
	if err != nil || stored.Status != "applied" {
		t.Fatalf("proposal after refused rollback = %#v, %v", stored, err)
	}
}

func TestApplyMarksProposalStaleWhenTargetChangedAfterPreview(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "multiply", "1.2", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()
	proposal, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.target.set("economy", "0.055")

	_, err = fixture.service.Apply(ctx, proposal.ID)
	if err == nil || !strings.Contains(err.Error(), "预览后发生变化") {
		t.Fatalf("stale apply error = %v", err)
	}
	stored, err := fixture.store.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stale" || stored.ReadbackValue != "0.055" || stored.Error == "" {
		t.Fatalf("stale proposal = %#v", stored)
	}
	_, writes := fixture.target.value("economy")
	if writes != 0 {
		t.Fatalf("stale proposal performed %d writes", writes)
	}
}

func TestApplyMarksProposalFailedWhenTargetWriteFails(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "multiply", "1.2", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()
	proposal, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.target.mu.Lock()
	fixture.target.writeFailure = true
	fixture.target.mu.Unlock()

	_, err = fixture.service.Apply(ctx, proposal.ID)
	if err == nil || !strings.Contains(err.Error(), "remote target returned Bad Gateway") {
		t.Fatalf("failed apply error = %v", err)
	}
	stored, err := fixture.store.GetProposal(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "failed" || !strings.Contains(stored.Error, "remote target returned Bad Gateway") || strings.Contains(stored.Error, "simulated write failure") {
		t.Fatalf("failed proposal = %#v", stored)
	}
}

func TestProposeTreatsNumericallyEqualRatiosAsNoChange(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "fixed", "0.05", "0.05")
	defer fixture.cleanup()

	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal.Status != "no_change" {
		t.Fatalf("numerically equal 0.05 and %s produced status %q, want no_change", proposal.ProposedValue, proposal.Status)
	}
	if proposal.Calculation.AutomationReason != "" {
		t.Fatalf("no-change proposal retained an automation blocker: %q", proposal.Calculation.AutomationReason)
	}
	for _, warning := range proposal.Calculation.Warnings {
		if strings.Contains(warning, "连续") || strings.Contains(warning, "读数") {
			t.Fatalf("no-change proposal retained evidence to-do warning: %q", warning)
		}
	}
	if _, err := fixture.service.Apply(context.Background(), proposal.ID); err != nil {
		t.Fatalf("Apply no_change: %v", err)
	}
	_, writes := fixture.target.value("economy")
	if writes != 0 {
		t.Fatalf("no_change proposal performed %d writes", writes)
	}
}

func TestProposeRefreshesSingleNoChangeObservationWithoutActivityNoise(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "fixed", "0.05", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()

	first, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("first Propose: %v", err)
	}
	if first.Status != "no_change" {
		t.Fatalf("first proposal status = %q, want no_change", first.Status)
	}
	snapshot, err := fixture.store.LatestSnapshot(ctx, fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Evidence.ResponseHash = "supplier-catalog-same-price-new-response"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	second, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("second Propose: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("same no-change state created proposal %d, want refreshed proposal %d", second.ID, first.ID)
	}
	if second.Calculation.SourceEvidenceHash != snapshot.Evidence.ResponseHash ||
		second.Calculation.InputFingerprint != first.Calculation.InputFingerprint {
		t.Fatalf("refreshed proposal retained stale evidence: first=%#v second=%#v", first.Calculation, second.Calculation)
	}
	proposals, err := fixture.store.ListProposals(ctx, "no_change", 10)
	if err != nil || len(proposals) != 1 || proposals[0].ID != first.ID {
		t.Fatalf("no-change proposals = %#v, error = %v", proposals, err)
	}
	activities, err := fixture.store.RecentActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Kind == "proposal_created" {
			t.Fatalf("no-change observation created misleading activity: %#v", activity)
		}
	}
}

func TestProposeCompactsHistoricalNoChangeRowsAndTheirMisleadingActivities(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "fixed", "0.05", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()

	first, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := first
	legacy.ID = 0
	legacy.IdempotencyKey = "legacy-no-change-duplicate"
	if err := fixture.store.CreateProposal(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	for _, proposalID := range []int64{first.ID, legacy.ID} {
		if err := fixture.store.AddActivity(ctx, domain.Activity{Kind: "proposal_created", Severity: "info",
			Title: "已生成价格变更预览", Metadata: map[string]string{"proposal_id": fmt.Sprint(proposalID)}}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := fixture.store.LatestSnapshot(ctx, fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Evidence.ResponseHash = "supplier-catalog-compaction"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	refreshed, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != first.ID {
		t.Fatalf("refreshed proposal id = %d, want active semantic id %d", refreshed.ID, first.ID)
	}
	proposals, err := fixture.store.ListProposals(ctx, "no_change", 10)
	if err != nil || len(proposals) != 1 || proposals[0].ID != first.ID {
		t.Fatalf("compacted proposals = %#v, error=%v", proposals, err)
	}
	activities, err := fixture.store.RecentActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Kind == "proposal_created" {
			t.Fatalf("misleading no-change activity survived compaction: %#v", activity)
		}
	}
}

func TestProposeMarksPendingSuggestionStaleWhenTargetAlreadyMatches(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()

	pending, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("pending Propose: %v", err)
	}
	if pending.Status != "pending" || pending.ProposedValue != "0.060000000000" {
		t.Fatalf("pending proposal = %#v", pending)
	}
	fixture.target.set("economy", pending.ProposedValue)
	fixture.service.InvalidateTargetRead(fixture.policy.TargetSiteID)

	current, err := fixture.service.Propose(ctx, fixture.policy.ID)
	if err != nil {
		t.Fatalf("no-change Propose: %v", err)
	}
	if current.Status != "no_change" || current.ExpectedOld != pending.ProposedValue {
		t.Fatalf("current proposal = %#v", current)
	}
	storedPending, err := fixture.store.GetProposal(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedPending.Status != "stale" || storedPending.Error == "" {
		t.Fatalf("obsolete pending proposal = %#v", storedPending)
	}
}

func TestConcurrentSimulationsShareShortTargetRead(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	config := DefaultSafetyConfig()
	config.TargetReadCacheMilliseconds = 60000
	if _, err := fixture.service.SetSafetyConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Simulate(context.Background(), fixture.policy.ID); err != nil {
		t.Fatal(err)
	}
	baselineSnapshot, baselinePricing := fixture.target.reads()
	fixture.service.InvalidateTargetRead(fixture.policy.TargetSiteID)
	fixture.target.resetReads()

	const workers = 12
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshotReads, pricingReads := fixture.target.reads()
	if snapshotReads != baselineSnapshot || pricingReads != baselinePricing {
		t.Fatalf("concurrent simulations read target snapshot=%d pricing=%d, want one logical-read baseline %d/%d",
			snapshotReads, pricingReads, baselineSnapshot, baselinePricing)
	}
}

func TestSuccessfulWriteInvalidatesTargetReadCache(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	config := DefaultSafetyConfig()
	config.TargetReadCacheMilliseconds = 60000
	if _, err := fixture.service.SetSafetyConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Apply(context.Background(), proposal.ID); err != nil {
		t.Fatal(err)
	}
	beforeSnapshot, beforePricing := fixture.target.reads()
	if _, err := fixture.service.Simulate(context.Background(), fixture.policy.ID); err != nil {
		t.Fatal(err)
	}
	afterSnapshot, afterPricing := fixture.target.reads()
	if afterSnapshot <= beforeSnapshot || afterPricing <= beforePricing {
		t.Fatalf("post-write simulation reused stale cache: before=%d/%d after=%d/%d",
			beforeSnapshot, beforePricing, afterSnapshot, afterPricing)
	}
}

func TestEquivalentCostOutlierKeepsSuggestionButRequiresApproval(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.1", "0.25")
	defer fixture.cleanup()
	snapshot, err := fixture.store.LatestSnapshot(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups[0].Ratio = "0.25"
	snapshot.Models[0].PriceFunction.InputPer1M = "50"
	snapshot.Models[0].PriceFunction.OutputPer1M = "50"
	snapshot.Evidence.ResponseHash = "equivalent-outlier"
	snapshot.Evidence.SourceType = "authenticated_api"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	fixture.policy.SourceBasis = "equivalent_cost"
	fixture.policy.ExecutionMode = "auto"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if simulation.EquivalentCostRatio != "6.25" || simulation.Calculation.TargetRatio != "6.350000000000" {
		t.Fatalf("outlier suggestion was altered: %#v", simulation)
	}
	if simulation.Calculation.AutomationEligible || !strings.Contains(simulation.Calculation.AutomationReason, "25.00 倍") {
		t.Fatalf("outlier automatic gate = %#v", simulation.Calculation)
	}
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || proposal.Mode != "approval" || proposal.ProposedValue != "6.350000000000" {
		t.Fatalf("outlier proposal=%#v err=%v", proposal, err)
	}

	config := DefaultSafetyConfig()
	config.EquivalentCostMultipleLimit = "30"
	config.MaximumAutomaticIncreaseRate = "30"
	if _, err := fixture.service.SetSafetyConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	simulation, err = fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !simulation.Calculation.AutomationEligible {
		t.Fatalf("hot-updated owner thresholds did not restore eligibility: %q", simulation.Calculation.AutomationReason)
	}
}

func TestSimulationUsesModelPriceVectorInsteadOfRawGroupRatio(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	fixture.target.setPricingModelRatio("2")
	fixture.policy.SourceBasis = "equivalent_cost"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if simulation.SourceGroup.Ratio != "0.05" || simulation.EquivalentCostRatio != "0.025" ||
		simulation.Calculation.TargetRatio != "0.035000000000" {
		t.Fatalf("simulation = %#v", simulation)
	}
	if simulation.CostRatioVector["gpt-5.6-sol/input_per_1m"] != "0.025" {
		t.Fatalf("cost ratio vector = %#v", simulation.CostRatioVector)
	}
}

func TestSimulationKeepsGroupRatioRuleSpreadSeparateFromEquivalentCostRisk(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	fixture.target.setPricingModelRatio("2")
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if simulation.SourceGroup.Ratio != "0.05" || simulation.EquivalentCostRatio != "0.025" ||
		simulation.Calculation.SourceRatio != "0.050000000000" || simulation.Calculation.CostRatio != "0.050000000000" ||
		simulation.Calculation.EquivalentCostRatio != "0.025" || simulation.Calculation.TargetRatio != "0.060000000000" ||
		simulation.Calculation.ContributionProfit != "0.010000000000" {
		t.Fatalf("group-ratio simulation = %#v", simulation)
	}
}

func TestSimulationAppliesRechargeFactorExactlyOnceBeforeFixedAdd(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.1", "1.3")
	defer fixture.cleanup()

	supplier, err := fixture.store.GetSupplier(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	supplier.RechargeValueFactor = "0.4"
	if err := fixture.store.UpdateSupplier(context.Background(), supplier); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(context.Background(), supplier.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups[0].Ratio = "3"
	snapshot.Groups[0].RawRatio = ""
	snapshot.Groups[0].EffectiveRatio = ""
	snapshot.Groups[0].RechargeValueFactor = ""
	snapshot.Evidence.ResponseHash = "supplier-catalog-discounted"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if simulation.SourceGroup.RawRatio != "3" || simulation.SourceGroup.RechargeValueFactor != "0.4" ||
		simulation.SourceGroup.EffectiveRatio != "1.2" || simulation.Calculation.SourceRatio != "1.200000000000" ||
		simulation.Calculation.TargetRatio != "1.300000000000" {
		t.Fatalf("discounted fixed-add simulation = %#v", simulation)
	}
}

func TestGroupRatioRuleContinuesWhenModelDimensionRiskIsUnavailable(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.02", "0.1")
	defer fixture.cleanup()
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range snapshot.Models {
		snapshot.Models[index].PriceFunction = domain.PriceFunction{}
	}
	snapshot.Evidence.ResponseHash = "group-ratio-only-catalog"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}

	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatalf("group-ratio simulation was blocked by optional model-price evidence: %v", err)
	}
	if simulation.CurrentRatio != "0.1" || simulation.Calculation.SourceRatio != "0.050000000000" ||
		simulation.Calculation.TargetRatio != "0.070000000000" || simulation.EquivalentCostRatio != "" ||
		simulation.ImpactConfidence != "unavailable" ||
		strings.Contains(simulation.Calculation.AutomationReason, "覆盖率") ||
		strings.Contains(simulation.Calculation.AutomationReason, "同币种") {
		t.Fatalf("group-ratio-only simulation = %#v", simulation)
	}

	fixture.policy.SourceBasis = "equivalent_cost"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Simulate(context.Background(), fixture.policy.ID); err == nil ||
		!strings.Contains(err.Error(), "pricing impact cannot be verified") {
		t.Fatalf("equivalent-cost rule accepted missing model-price evidence: %v", err)
	}
}

func TestGroupRatioRuleContinuesWhenSourceGroupHasNoModels(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.02", "0.1")
	defer fixture.cleanup()
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups[0].ModelIDs = nil
	snapshot.Models = nil
	snapshot.Evidence.ResponseHash = "group-ratio-empty-model-catalog"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	simulation, err := fixture.service.Simulate(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatalf("empty model catalog blocked GroupRatio mapping: %v", err)
	}
	if simulation.Calculation.SourceRatio != "0.050000000000" || simulation.Calculation.TargetRatio != "0.070000000000" {
		t.Fatalf("empty-catalog GroupRatio simulation = %#v", simulation)
	}
}

func TestMissingSourceGroupIsWaitingInsteadOfAutomationFailure(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.02", "0.1")
	defer fixture.cleanup()
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups = nil
	snapshot.Models = nil
	snapshot.Evidence.ResponseHash = "source-group-removed"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.ReconcileSupplierWithError(context.Background(), fixture.policy.SupplierID); err != nil {
		t.Fatalf("waiting source group was returned as reconciliation failure: %v", err)
	}
	activities, err := fixture.store.RecentActivity(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	foundWaiting := false
	for _, activity := range activities {
		if activity.Kind == "automation_failed" {
			t.Fatalf("missing source group still emitted an automation failure: %#v", activity)
		}
		if activity.Kind == "pricing_mapping_waiting" {
			foundWaiting = true
		}
	}
	if !foundWaiting {
		t.Fatalf("waiting activity missing: %#v", activities)
	}
}

func TestReconcileSupplierKeepsSharedNetworkFailureOutOfActivityLog(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.02", "0.1")
	defer fixture.cleanup()
	ctx := context.Background()
	second := fixture.policy
	second.ID = 0
	second.Name += " second"
	second.TargetGroup = "economy-second"
	fixture.target.set(second.TargetGroup, "0.1")
	if err := fixture.store.CreatePolicy(ctx, &second); err != nil {
		t.Fatal(err)
	}
	config := DefaultSafetyConfig()
	config.TargetReadCacheMilliseconds = 60000
	if _, err := fixture.service.SetSafetyConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	fixture.target.resetReads()
	fixture.target.setOptionStatus(http.StatusTooManyRequests)

	err := fixture.service.ReconcileSupplierWithError(ctx, fixture.policy.SupplierID)
	if err == nil || !strings.Contains(err.Error(), "legacy pricing target") {
		t.Fatalf("shared target failure=%v", err)
	}
	snapshotReads, pricingReads := fixture.target.reads()
	if snapshotReads != 3 || pricingReads != 0 {
		t.Fatalf("target reads=%d/%d, want one logical read with three bounded attempts", snapshotReads, pricingReads)
	}
	activities, err := fixture.store.RecentActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		switch activity.Kind {
		case "automation_target_read_failed":
			t.Fatalf("shared network failure entered the activity stream: %#v", activity)
		case "automation_failed":
			t.Fatalf("shared target failure still emitted a per-rule activity: %#v", activity)
		}
	}
}

func TestReconcileSupplierLogsActionableContextForRemovedModels(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	ctx := context.Background()
	fixture.policy.SourceBasis = "equivalent_cost"
	fixture.policy.SourceModelPattern = "*"
	if err := fixture.store.UpdatePolicy(ctx, fixture.policy); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.LatestSnapshot(ctx, fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups[0].ModelIDs = nil
	snapshot.Models = nil
	snapshot.Evidence.ResponseHash = "supplier-catalog-models-removed"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		fixture.service.ReconcileSupplier(ctx, fixture.policy.SupplierID)
	}
	activities, err := fixture.store.RecentActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var failure *domain.Activity
	for index := range activities {
		if activities[index].Kind == "automation_failed" {
			failure = &activities[index]
			break
		}
	}
	if failure == nil {
		t.Fatalf("automation failure activity missing: %#v", activities)
	}
	for _, fragment := range []string{"规则 #", "policy", "supplier", "分组 economy", "模型模式 \"*\"", "当前匹配 0 个", "production", "上游分组没有模型", "建议"} {
		if !strings.Contains(failure.Message, fragment) {
			t.Fatalf("diagnostic message %q does not contain %q", failure.Message, fragment)
		}
	}
	if failure.Metadata["diagnostic_category"] != "source_group_models_empty" ||
		failure.Metadata["policy_id"] != fmt.Sprint(fixture.policy.ID) || failure.Metadata["source_group"] != "economy" ||
		failure.Metadata["target_group"] != "economy" || failure.Metadata["matched_model_count"] != "0" ||
		failure.Metadata["repeat_count"] != "2" || failure.Metadata["automation_failure_key"] == "" {
		t.Fatalf("diagnostic metadata = %#v", failure.Metadata)
	}
	if strings.Contains(strings.ToLower(failure.Message), "bearer") || strings.Contains(strings.ToLower(failure.Message), "cookie") {
		t.Fatalf("diagnostic leaked credential-shaped content: %q", failure.Message)
	}
}

func TestChangedFixedAddRuleSupersedesOldSuggestionWithSameCatalog(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.16", "0.29")
	defer fixture.cleanup()

	snapshot, err := fixture.store.LatestSnapshot(context.Background(), fixture.policy.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Groups[0].Ratio = "0.13"
	snapshot.Evidence.ResponseHash = "supplier-catalog-013"
	snapshot.Evidence.ObservedAt = time.Now().UTC().Add(time.Second)
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	fixture.policy.SourceBasis = "group_ratio"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}

	oldProposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil || oldProposal.ProposedValue != "0.290000000000" {
		t.Fatalf("old proposal=%#v err=%v", oldProposal, err)
	}
	fixture.policy.Value = "0.03"
	if err := fixture.store.UpdatePolicy(context.Background(), fixture.policy); err != nil {
		t.Fatal(err)
	}
	newProposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newProposal.ID == oldProposal.ID || newProposal.ProposedValue != "0.160000000000" ||
		newProposal.Calculation.SourceRatio != "0.130000000000" ||
		newProposal.Calculation.PolicyFingerprint == oldProposal.Calculation.PolicyFingerprint ||
		newProposal.Calculation.InputFingerprint == oldProposal.Calculation.InputFingerprint {
		t.Fatalf("changed add rule reused stale proposal: old=%#v new=%#v", oldProposal, newProposal)
	}
	storedOld, err := fixture.store.GetProposal(context.Background(), oldProposal.ID)
	if err != nil || storedOld.Status != "no_change" {
		t.Fatalf("historical no-change audit record was not preserved: %#v err=%v", storedOld, err)
	}
}

func TestGroupRatioApplyIgnoresIndependentTargetModelPriceChange(t *testing.T) {
	t.Parallel()
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.target.setPricingModelRatio("2")

	applied, err := fixture.service.Apply(context.Background(), proposal.ID)
	if err != nil || applied.Status != "applied" {
		t.Fatalf("group-ratio Apply = %#v, %v", applied, err)
	}
	_, writes := fixture.target.value("economy")
	if writes != 1 {
		t.Fatalf("group-ratio apply writes = %d, want 1", writes)
	}
}

func TestMatchSourceModelsEnforcesConfiguredScope(t *testing.T) {
	t.Parallel()
	group := domain.GroupOffer{Name: "mixed", ModelIDs: []string{"gpt-5.6-sol", "gpt-image-2", "claude-opus-4.8"}}
	matched, err := matchSourceModels(group, "gpt-*,claude-opus-4.8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(matched, ",") != "gpt-5.6-sol,gpt-image-2,claude-opus-4.8" {
		t.Fatalf("matched models = %#v", matched)
	}
	if _, err := matchSourceModels(group, "gemini-*"); err == nil {
		t.Fatal("expected an unmatched model pattern to be rejected")
	}
	if _, err := matchSourceModels(group, "["); err == nil {
		t.Fatal("expected an invalid glob to be rejected")
	}
}

func TestMatchSourceModelsBareStarIncludesSlashModels(t *testing.T) {
	t.Parallel()
	group := domain.GroupOffer{Name: "relay", ModelIDs: []string{"openai/gpt-5.6-sol", "anthropic/claude-4.8"}}
	matched, err := matchSourceModels(group, "*")
	if err != nil {
		t.Fatalf("bare star returned error: %v", err)
	}
	if len(matched) != len(group.ModelIDs) {
		t.Fatalf("bare star matched %v, want all %v", matched, group.ModelIDs)
	}
}

func newAutomationFixture(t *testing.T, ruleType, ruleValue, currentRatio string) automationFixture {
	t.Helper()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		_ = st.Close()
		t.Fatalf("open vault: %v", err)
	}
	credentialService := credentials.NewService(st, vault)
	remote := &automationTarget{ratios: map[string]string{"default": "1", "economy": currentRatio}}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	credential := domain.Credential{Name: "target admin", Kind: "access-token", SiteURL: server.URL}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{Authorization: "Bearer token", UserID: 1}); err != nil {
		server.Close()
		_ = st.Close()
		t.Fatalf("create credential: %v", err)
	}
	supplier := domain.Supplier{Name: "supplier", BaseURL: "https://supplier.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{
		SupplierID: supplier.ID,
		Groups:     []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "0.05", ModelIDs: []string{"gpt-5.6-sol"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-5.6-sol", Name: "gpt-5.6-sol", Group: "economy", BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "2"}}},
		Evidence: domain.Evidence{ObservedAt: observed, ResponseHash: "supplier-catalog"},
	}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "production", BaseURL: server.URL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{
		Name: "policy", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		SourceModelPattern: "gpt-*", TargetGroup: "economy", RuleType: ruleType, Value: ruleValue, Enabled: true, ExecutionMode: "approval",
	}
	if err := st.CreatePolicy(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	factory := targets.NewFactory(server.Client(), credentialService)
	service := NewService(st, factory)
	service.SetAutomaticWritesEnabled(true)
	return automationFixture{
		store: st, service: service, policy: policy, target: remote, server: server,
		cleanup: func() {
			server.Close()
			_ = st.Close()
		},
	}
}

func newGroupModelAutomationFixture(t *testing.T) contractAutomationFixture {
	t.Helper()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		_ = st.Close()
		t.Fatalf("open vault: %v", err)
	}
	credentialService := credentials.NewService(st, vault)
	remote := newContractAutomationTarget()
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	credential := domain.Credential{Name: "target admin", Kind: "access-token", SiteURL: server.URL}
	if err := credentialService.Create(context.Background(), &credential,
		domain.CredentialPayload{Authorization: "Bearer token", UserID: 1}); err != nil {
		server.Close()
		_ = st.Close()
		t.Fatalf("create credential: %v", err)
	}
	supplier := domain.Supplier{Name: "supplier", BaseURL: "https://supplier.example", Adapter: "newapi", Enabled: true,
		RechargeValueFactor: "1"}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "1", RawRatio: "1",
			EffectiveRatio: "1", RechargeValueFactor: "1", ModelIDs: []string{"video-model"}}},
		Models: []domain.ModelOffer{{ExternalID: "video-model", Name: "video-model", Group: "economy", BillingMode: "fixed_price",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1.2"}, Metadata: map[string]string{"billing_unit": "per_request"}}},
		Evidence: domain.Evidence{SourceType: "public_api", ObservedAt: observed, ResponseHash: "supplier-contract-catalog", Confidence: 100},
	}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "production", BaseURL: server.URL, Adapter: "newapi", CredentialID: credential.ID,
		Enabled: true, WriteMode: "auto"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOpportunityUsage(context.Background(), []store.OpportunityUsage{
		{TargetSiteID: target.ID, ModelID: "video-model", WindowDays: 7, DemandWeight: "700", DemandUnit: "requests",
			Source: "test", Confidence: "0.95", ObservedAt: observed},
		{TargetSiteID: target.ID, ModelID: "video-model", WindowDays: 30, DemandWeight: "3000", DemandUnit: "requests",
			Source: "test", Confidence: "0.95", ObservedAt: observed},
	}); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "contract policy", SupplierID: supplier.ID, SourceGroup: "economy",
		SourceModelPattern: "video-*", SourceBasis: "group_model_contract", TargetSiteID: target.ID, TargetGroup: "economy",
		RuleType: "target_margin", Value: "0.2", ProfitBasis: "cash", RevenueCashFactor: "1", CostCashFactor: "1",
		RiskBufferRate: "0", RoundingStep: "0.0001", Enabled: true, ExecutionMode: "approval"}
	if err := st.CreatePolicy(context.Background(), &policy); err != nil {
		t.Fatal(err)
	}
	factory := targets.NewFactory(server.Client(), credentialService)
	service := NewService(st, factory)
	service.SetAutomaticWritesEnabled(true)
	return contractAutomationFixture{store: st, service: service, policy: policy, target: remote, server: server,
		cleanup: func() {
			server.Close()
			_ = st.Close()
		}}
}

func testDecimalEqual(left, right string) bool {
	leftValue, leftErr := decimal.NewFromString(left)
	rightValue, rightErr := decimal.NewFromString(right)
	return leftErr == nil && rightErr == nil && leftValue.Equal(rightValue)
}
