package mirror

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestPlanCompleteMirrorExactFunctionalDiffs(t *testing.T) {
	tests := []struct {
		name           string
		input          CompleteMirrorPlannerInput
		wantDecision   MirrorPlanDecision
		wantDiffs      []string
		wantStages     []MirrorPlanPhase
		wantModels     []string
		wantEnabled    bool
		wantRemoved    []string
		wantGateCodes  []string
		wantBlockCodes []string
	}{
		{
			name: "source price change",
			input: plannerFixture(
				[]CanonicalModel{plannerTaskModel("m", "per_second", 0, "2.00")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_second", 0, "1")},
			),
			wantDecision: DecisionAutomatic,
			wantDiffs:    []string{"contract_cas|model_price|m|model_price|1|2"},
			wantStages:   []MirrorPlanPhase{PhaseContractCAS, PhaseReadback}, wantModels: []string{"m"}, wantEnabled: true,
		},
		{
			name: "token output price change",
			input: plannerFixture(
				[]CanonicalModel{plannerTokenModel("m", "2", "6")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTokenSetting("1", "2")},
			),
			wantDecision: DecisionAutomatic,
			wantDiffs:    []string{"contract_cas|model_price|m|completion_ratio|2|3"},
			wantStages:   []MirrorPlanPhase{PhaseContractCAS, PhaseReadback}, wantModels: []string{"m"}, wantEnabled: true,
		},
		{
			name: "billing unit change",
			input: plannerFixture(
				[]CanonicalModel{plannerTaskModel("m", "per_request", 0, "1")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_second", 0, "1.0")},
			),
			wantDecision: DecisionAutomatic,
			wantDiffs:    []string{"contract_cas|billing|m|billing_unit|per_second|per_request"},
			wantStages:   []MirrorPlanPhase{PhaseContractCAS, PhaseReadback}, wantModels: []string{"m"}, wantEnabled: true,
		},
		{
			name: "fixed duration metadata change",
			input: plannerFixture(
				[]CanonicalModel{plannerTaskModel("m", "fixed_duration", 8, "1")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")},
			),
			wantDecision: DecisionAutomatic,
			wantDiffs: []string{
				"contract_cas|billing|m|billing_unit|per_request|fixed_duration",
				"contract_cas|billing|m|fixed_duration_seconds||8",
			},
			wantStages: []MirrorPlanPhase{PhaseContractCAS, PhaseReadback}, wantModels: []string{"m"}, wantEnabled: true,
		},
		{
			name: "addition at default percentage boundary",
			input: plannerFixture(
				[]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1"), plannerTaskModel("b", "per_request", 0, "1"), plannerTaskModel("c", "per_request", 0, "1")},
				[]string{"a", "b"}, map[string]domain.GroupModelPricingSetting{"a": plannerTaskSetting("per_request", 0, "1"), "b": plannerTaskSetting("per_request", 0, "1")},
			),
			wantDecision: DecisionAutomatic,
			wantDiffs: []string{
				"contract_cas|billing|c|billing_mode||fixed_price",
				"contract_cas|billing|c|billing_unit||per_request",
				"contract_cas|model_price|c|model_price||1",
				"additions_expose|addition|c|channel_membership|absent|present",
			},
			wantStages: []MirrorPlanPhase{PhaseContractCAS, PhaseAdditionsExpose, PhaseReadback}, wantModels: []string{"a", "b", "c"}, wantEnabled: true,
		},
		{
			name: "owned removal preserves foreign model",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture(
					[]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1"), plannerTaskModel("b", "per_request", 0, "1"), plannerTaskModel("c", "per_request", 0, "1")},
					[]string{"a", "b", "c", "d", "foreign"}, map[string]domain.GroupModelPricingSetting{
						"a": plannerTaskSetting("per_request", 0, "1"), "b": plannerTaskSetting("per_request", 0, "1"),
						"c": plannerTaskSetting("per_request", 0, "1"), "d": plannerTaskSetting("per_request", 0, "1"),
						"foreign": plannerTaskSetting("per_request", 0, "7"),
					})
				input.OwnedModels = plannerOwnership("a", "b", "c", "d")
				return input
			}(),
			wantDecision: DecisionAutomatic,
			wantDiffs: []string{
				"removal_quiesce|removal|d|channel_membership|present|absent",
				"contract_cas|billing|d|billing_mode|fixed_price|",
				"contract_cas|billing|d|billing_unit|per_request|",
				"contract_cas|model_price|d|model_price|1|",
			},
			wantStages: []MirrorPlanPhase{PhaseRemovalQuiesce, PhaseContractCAS, PhaseReadback},
			wantModels: []string{"a", "b", "c", "foreign"}, wantEnabled: true, wantRemoved: []string{"d"},
		},
		{
			name: "empty group removes every managed model",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture(nil, []string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")})
				input.SourceState = SourceGroupEmpty
				input.SourceGroup.Models = nil
				return input
			}(),
			wantDecision: DecisionAutomatic,
			wantDiffs: []string{
				"removal_quiesce|removal|m|channel_membership|present|absent",
				"contract_cas|billing|m|billing_mode|fixed_price|",
				"contract_cas|billing|m|billing_unit|per_request|",
				"contract_cas|model_price|m|model_price|1|",
			},
			wantStages: []MirrorPlanPhase{PhaseRemovalQuiesce, PhaseContractCAS, PhaseReadback},
			wantModels: []string{}, wantEnabled: true, wantRemoved: []string{"m"},
		},
		{
			name: "decimal formatting and observation timestamp are no-op",
			input: func() CompleteMirrorPlannerInput {
				setting := plannerTaskSetting("per_request", 0, "1")
				setting.Source = "relayops:policy/9c03cdef-d23a-4a35-a2d3-3b178e6de1d1"
				setting.SourceRevision = "old"
				setting.SourceObservedAtUnixMS = 10
				input := plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "1.0000")},
					[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": setting})
				input.CurrentGroupRatio = "1.000"
				input.SourceRevision = "new"
				input.SourceObservedAtUnixMS = 999
				return input
			}(),
			wantDecision: DecisionNoChange, wantModels: []string{"m"}, wantEnabled: true,
		},
		{
			name: "explicit zero token vector remains a no-op",
			input: plannerFixture([]CanonicalModel{plannerTokenModel("grok-imagine-image", "0", "0")},
				[]string{"grok-imagine-image"}, map[string]domain.GroupModelPricingSetting{
					"grok-imagine-image": plannerTokenSetting("0", "1"),
				}),
			wantDecision: DecisionNoChange, wantModels: []string{"grok-imagine-image"}, wantEnabled: true,
		},
		{
			name: "zero upstream ratio with positive add transform",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "1")},
					[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")})
				input.SourceGroup.RawGroupRatio = "0"
				input.CurrentGroupRatio = "0.05"
				input.Policy.GroupRatioRule = Rule{Type: "add", Value: "0.1"}
				return input
			}(),
			wantDecision: DecisionNoChange,
			wantDiffs:    nil,
			wantStages:   nil, wantModels: []string{"m"}, wantEnabled: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanCompleteMirror(test.input)
			if err != nil {
				t.Fatalf("PlanCompleteMirror() error = %v", err)
			}
			if plan.Decision != test.wantDecision {
				t.Fatalf("decision = %s, want %s", plan.Decision, test.wantDecision)
			}
			if got := plannerDiffSignatures(plan.Diffs); !reflect.DeepEqual(got, test.wantDiffs) {
				t.Fatalf("diffs = %#v, want %#v", got, test.wantDiffs)
			}
			if got := plannerStagePhases(plan.Stages); !reflect.DeepEqual(got, test.wantStages) {
				t.Fatalf("stages = %#v, want %#v", got, test.wantStages)
			}
			if !reflect.DeepEqual(plan.DesiredChannelModels, test.wantModels) {
				t.Fatalf("desired models = %#v, want %#v", plan.DesiredChannelModels, test.wantModels)
			}
			if plan.DesiredChannelEnabled != test.wantEnabled {
				t.Fatalf("desired enabled = %t, want %t", plan.DesiredChannelEnabled, test.wantEnabled)
			}
			if !reflect.DeepEqual(plan.RemovedContractModels, test.wantRemoved) {
				t.Fatalf("removed contracts = %#v, want %#v", plan.RemovedContractModels, test.wantRemoved)
			}
			if got := plannerGateCodes(plan.ApprovalGates); !reflect.DeepEqual(got, test.wantGateCodes) {
				t.Fatalf("approval gates = %#v, want %#v", got, test.wantGateCodes)
			}
			if got := plannerGateCodes(plan.BlockingGates); !reflect.DeepEqual(got, test.wantBlockCodes) {
				t.Fatalf("blocking gates = %#v, want %#v", got, test.wantBlockCodes)
			}
		})
	}
}

func TestPlanCompleteMirrorRejectsNegativeTransformedSale(t *testing.T) {
	input := plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "0.5")},
		[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "0.5")})
	input.Policy.ModelPriceRule = Rule{Type: "add", Value: "-1"}
	if _, err := PlanCompleteMirror(input); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("negative transformed sale error = %v", err)
	}
}

func TestPlanCompleteMirrorDisasterGates(t *testing.T) {
	tests := []struct {
		name          string
		input         CompleteMirrorPlannerInput
		wantDecision  MirrorPlanDecision
		wantApprovals []string
		wantBlocks    []string
	}{
		{
			name: "addition percentage",
			input: plannerFixture(
				[]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1"), plannerTaskModel("b", "per_request", 0, "1"), plannerTaskModel("c", "per_request", 0, "1")},
				[]string{"a"}, map[string]domain.GroupModelPricingSetting{"a": plannerTaskSetting("per_request", 0, "1")},
			),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"addition_percentage_exceeded"},
		},
		{
			name: "owned removal percentage",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1")},
					[]string{"a", "b"}, map[string]domain.GroupModelPricingSetting{
						"a": plannerTaskSetting("per_request", 0, "1"), "b": plannerTaskSetting("per_request", 0, "1"),
					})
				input.OwnedModels = plannerOwnership("a", "b")
				return input
			}(),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"removal_percentage_exceeded"},
		},
		{
			name: "price increase over ten times",
			input: plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "10.01")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")}),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"price_increase_exceeded"},
		},
		{
			name: "price increase from zero is never silently automatic",
			input: plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "1")},
				[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "0")}),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"price_increase_exceeded"},
		},
		{
			name: "target contract entry hard cap",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1"), plannerTaskModel("b", "per_request", 0, "1")},
					[]string{"a"}, map[string]domain.GroupModelPricingSetting{"a": plannerTaskSetting("per_request", 0, "1")})
				input.Limits.MaximumContractEntries = 1
				return input
			}(),
			wantDecision:  DecisionBlocked,
			wantApprovals: []string{"addition_percentage_exceeded"},
			wantBlocks:    []string{"contract_entry_hard_cap_exceeded", "channel_model_hard_cap_exceeded"},
		},
		{
			name: "target serialized contract hard cap",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "2")},
					[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")})
				input.Limits.MaximumContractBytes = 1
				return input
			}(),
			wantDecision: DecisionBlocked, wantBlocks: []string{"contract_size_hard_cap_exceeded"},
		},
		{
			name: "empty group model removal does not require channel status authority",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture(nil, []string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")})
				input.SourceState = SourceGroupEmpty
				input.SourceGroup.Models = nil
				input.ChannelStatusAuthority = "none"
				return input
			}(),
			wantDecision: DecisionAutomatic,
		},
		{
			name: "foreign contract change requires takeover approval",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("m", "per_request", 0, "2")},
					[]string{"m"}, map[string]domain.GroupModelPricingSetting{"m": plannerTaskSetting("per_request", 0, "1")})
				input.OwnedModels = nil
				return input
			}(),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"contract_takeover_required"},
		},
		{
			name: "contract-only removal participates in removal gate",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture([]CanonicalModel{plannerTaskModel("a", "per_request", 0, "1")},
					[]string{"a"}, map[string]domain.GroupModelPricingSetting{
						"a": plannerTaskSetting("per_request", 0, "1"), "retired": plannerTaskSetting("per_request", 0, "1"),
					})
				input.OwnedModels = []OwnedModelState{
					{TargetModel: "a", ContractOwned: true, MembershipOwned: true},
					{TargetModel: "retired", ContractOwned: true},
				}
				return input
			}(),
			wantDecision: DecisionApprovalRequired, wantApprovals: []string{"removal_percentage_exceeded"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanCompleteMirror(test.input)
			if err != nil {
				t.Fatalf("PlanCompleteMirror() error = %v", err)
			}
			if plan.Decision != test.wantDecision {
				t.Fatalf("decision = %s, want %s", plan.Decision, test.wantDecision)
			}
			if got := plannerGateCodes(plan.ApprovalGates); !reflect.DeepEqual(got, test.wantApprovals) {
				t.Fatalf("approval gates = %#v, want %#v", got, test.wantApprovals)
			}
			if got := plannerGateCodes(plan.BlockingGates); !reflect.DeepEqual(got, test.wantBlocks) {
				t.Fatalf("blocking gates = %#v, want %#v", got, test.wantBlocks)
			}
		})
	}
}

func TestPlanCompleteMirrorAbsoluteCountGatesAndFirstListingExemption(t *testing.T) {
	makeState := func(currentCount, sourceCount int) CompleteMirrorPlannerInput {
		models := make([]CanonicalModel, 0, sourceCount)
		current := make([]string, 0, currentCount)
		settings := make(map[string]domain.GroupModelPricingSetting, currentCount)
		for index := 0; index < sourceCount; index++ {
			name := "m-" + strconv.Itoa(index)
			models = append(models, plannerTaskModel(name, "per_request", 0, "1"))
		}
		for index := 0; index < currentCount; index++ {
			name := "m-" + strconv.Itoa(index)
			current = append(current, name)
			settings[name] = plannerTaskSetting("per_request", 0, "1")
		}
		return plannerFixture(models, current, settings)
	}

	addition := makeState(202, 303)
	plan, err := PlanCompleteMirror(addition)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannerGateCodes(plan.ApprovalGates); !reflect.DeepEqual(got, []string{"addition_count_exceeded"}) {
		t.Fatalf("absolute addition gates = %#v", got)
	}

	removal := makeState(104, 78)
	plan, err = PlanCompleteMirror(removal)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannerGateCodes(plan.ApprovalGates); !reflect.DeepEqual(got, []string{"removal_count_exceeded"}) {
		t.Fatalf("absolute removal gates = %#v", got)
	}

	firstListing := makeState(0, 101)
	firstListing.InitialApprovedListing = true
	plan, err = PlanCompleteMirror(firstListing)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Decision != DecisionAutomatic || len(plan.ApprovalGates) != 0 {
		t.Fatalf("approved first listing must be exempt only from delta-count gates: decision=%s gates=%+v", plan.Decision, plan.ApprovalGates)
	}
}

func TestPlanCompleteMirrorSafePhaseOrderAndDeterminism(t *testing.T) {
	input := plannerFixture(
		[]CanonicalModel{plannerTaskModel("keep", "per_request", 0, "2"), plannerTaskModel("new", "per_request", 0, "1")},
		[]string{"keep", "old"}, map[string]domain.GroupModelPricingSetting{
			"keep": plannerTaskSetting("per_request", 0, "1"), "old": plannerTaskSetting("per_request", 0, "1"),
		},
	)
	input.OwnedModels = plannerOwnership("keep", "old")
	input.InitialApprovedListing = true

	first, err := PlanCompleteMirror(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanCompleteMirror(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same immutable inputs produced different plans")
	}
	want := []MirrorPlanPhase{PhaseRemovalQuiesce, PhaseContractCAS, PhaseAdditionsExpose, PhaseReadback}
	if got := plannerStagePhases(first.Stages); !reflect.DeepEqual(got, want) {
		t.Fatalf("stage order = %#v, want %#v", got, want)
	}
	if got := plannerDiffPhaseSequence(first.Diffs); !reflect.DeepEqual(got,
		[]MirrorPlanPhase{PhaseRemovalQuiesce, PhaseContractCAS, PhaseContractCAS, PhaseContractCAS,
			PhaseContractCAS, PhaseContractCAS, PhaseContractCAS, PhaseContractCAS, PhaseAdditionsExpose}) {
		t.Fatalf("diff phase order = %#v", got)
	}
}

func TestDefaultDisasterLimitsAreRev3Bounds(t *testing.T) {
	limits := DefaultDisasterLimits()
	if limits.MaximumAdditions != 100 || limits.MaximumAdditionPercentage != "0.5" ||
		limits.MaximumRemovals != 25 || limits.MaximumRemovalPercentage != "0.25" ||
		limits.MaximumPriceIncrease != "10" || limits.MaximumContractEntries != 50000 ||
		limits.MaximumContractBytes != 8*1024*1024 {
		t.Fatalf("unexpected Rev3 defaults: %+v", limits)
	}
}

func TestCompleteMirrorNeverPlansGroupRatioEvenForLegacyStoredPolicy(t *testing.T) {
	input := plannerFixture([]CanonicalModel{plannerTaskModel("video", "per_request", 0, "2")}, nil, nil)
	input.SourceGroup.RawGroupRatio = "9"
	input.CurrentGroupRatio = "1.25"
	input.Policy.SyncGroupRatio = true
	input.Policy.GroupRatioRule = Rule{Type: "multiply", Value: "3"}

	plan, err := PlanCompleteMirror(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DesiredGroupRatio != "1.25" || plan.ExpectedGroupRatio != "1.25" {
		t.Fatalf("GroupRatio changed in native mirror plan: before=%s after=%s", plan.ExpectedGroupRatio, plan.DesiredGroupRatio)
	}
	for _, diff := range plan.Diffs {
		if diff.Kind == DiffGroupRatio {
			t.Fatalf("native mirror emitted GroupRatio diff: %#v", diff)
		}
	}
}

func TestPlanCompleteMirrorMembershipOnlyPricingFallbacks(t *testing.T) {
	t.Run("preserves existing target-group price", func(t *testing.T) {
		current := plannerTokenSetting("5", "5")
		input := plannerFixture([]CanonicalModel{plannerMembershipOnlyModel("claude-fable-5")},
			[]string{"claude-fable-5"}, map[string]domain.GroupModelPricingSetting{"claude-fable-5": current})
		plan, err := PlanCompleteMirror(input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Decision != DecisionNoChange || !reflect.DeepEqual(plan.DesiredContract.Groups["sale"]["claude-fable-5"], current) {
			t.Fatalf("existing price was not preserved: decision=%s desired=%#v", plan.Decision,
				plan.DesiredContract.Groups["sale"]["claude-fable-5"])
		}
	})

	t.Run("never copies another group price when source price is missing", func(t *testing.T) {
		input := plannerFixture([]CanonicalModel{plannerMembershipOnlyModel("claude-fable-5")}, nil, nil)
		input.InitialApprovedListing = true
		delete(input.CurrentContract.Groups, "sale")
		input.CurrentContract.Groups["other-a"] = map[string]domain.GroupModelPricingSetting{
			"claude-fable-5": plannerTokenSetting("5", "5"),
		}
		input.CurrentContract.Groups["other-b"] = map[string]domain.GroupModelPricingSetting{
			"claude-fable-5": plannerTokenSetting("5.0", "5.00"),
		}
		plan, err := PlanCompleteMirror(input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Decision != DecisionAutomatic || !reflect.DeepEqual(plan.DesiredChannelModels, []string{"claude-fable-5"}) {
			t.Fatalf("membership-only plan = %#v", plan)
		}
		if _, exists := plan.DesiredContract.Groups["sale"]["claude-fable-5"]; exists {
			t.Fatalf("another group price must not be copied into the target: %#v", plan.DesiredContract.Groups["sale"])
		}
		if _, exists := plan.DesiredContract.Groups["sale"]; exists {
			t.Fatalf("membership-only planning created an empty target contract group: %#v", plan.DesiredContract.Groups)
		}
		if plan.ExpectedFunctionalHash != plan.DesiredFunctionalHash {
			t.Fatalf("membership-only pricing changed the target functional hash: before=%s after=%s",
				plan.ExpectedFunctionalHash, plan.DesiredFunctionalHash)
		}
		if got := plannerDiffSignatures(plan.Diffs); !reflect.DeepEqual(got,
			[]string{"additions_expose|addition|claude-fable-5|channel_membership|absent|present"}) {
			t.Fatalf("membership-only diffs = %#v", got)
		}
	})

	t.Run("ambiguous fallback adds membership without guessing a price", func(t *testing.T) {
		input := plannerFixture([]CanonicalModel{plannerMembershipOnlyModel("claude-fable-5")}, nil, nil)
		input.InitialApprovedListing = true
		delete(input.CurrentContract.Groups, "sale")
		input.CurrentContract.Groups["other-a"] = map[string]domain.GroupModelPricingSetting{
			"claude-fable-5": plannerTokenSetting("5", "5"),
		}
		input.CurrentContract.Groups["other-b"] = map[string]domain.GroupModelPricingSetting{
			"claude-fable-5": plannerTokenSetting("6", "5"),
		}
		plan, err := PlanCompleteMirror(input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Decision != DecisionAutomatic || !reflect.DeepEqual(plan.DesiredChannelModels, []string{"claude-fable-5"}) {
			t.Fatalf("ambiguous fallback plan = %#v", plan)
		}
		if _, exists := plan.DesiredContract.Groups["sale"]["claude-fable-5"]; exists {
			t.Fatalf("ambiguous target prices must not be guessed: %#v", plan.DesiredContract.Groups["sale"])
		}
		if _, exists := plan.DesiredContract.Groups["sale"]; exists {
			t.Fatalf("ambiguous membership-only planning created an empty target contract group: %#v", plan.DesiredContract.Groups)
		}
		if got := plannerDiffSignatures(plan.Diffs); !reflect.DeepEqual(got,
			[]string{"additions_expose|addition|claude-fable-5|channel_membership|absent|present"}) {
			t.Fatalf("membership-only diffs = %#v", got)
		}
	})
}

func TestPlanCompleteMirrorConvertsAbsoluteFiveThirtyTokenPricesToNewAPIContract(t *testing.T) {
	input := plannerFixture([]CanonicalModel{plannerTokenModel("sol", "5", "30")},
		[]string{"sol"}, map[string]domain.GroupModelPricingSetting{"sol": plannerTokenSetting("1", "1")})

	plan, err := PlanCompleteMirror(input)
	if err != nil {
		t.Fatal(err)
	}
	setting, exists := plan.DesiredContract.Groups["sale"]["sol"]
	if !exists || setting.ModelRatio == nil || *setting.ModelRatio != "2.5" ||
		setting.CompletionRatio == nil || *setting.CompletionRatio != "6" {
		t.Fatalf("$5/$30 contract = %#v, want model_ratio=2.5 completion_ratio=6", setting)
	}
	if plan.DesiredGroupRatio != input.CurrentGroupRatio {
		t.Fatalf("mirror changed GroupRatio from %q to %q", input.CurrentGroupRatio, plan.DesiredGroupRatio)
	}
}

func TestPlanCompleteMirrorExcludedModelIsOutsideAllMirrorOwnership(t *testing.T) {
	excludedSetting := plannerTaskSetting("per_second", 0, "7.25")
	tests := []struct {
		name  string
		input CompleteMirrorPlannerInput
	}{
		{
			name: "source addition",
			input: plannerFixture([]CanonicalModel{plannerTaskModel("exception", "per_request", 0, "9")},
				nil, map[string]domain.GroupModelPricingSetting{}),
		},
		{
			name: "source price and billing change",
			input: plannerFixture([]CanonicalModel{plannerTaskModel("exception", "fixed_duration", 15, "99")},
				[]string{"exception"}, map[string]domain.GroupModelPricingSetting{"exception": excludedSetting}),
		},
		{
			name: "source removal",
			input: plannerFixture([]CanonicalModel{plannerTaskModel("kept", "per_request", 0, "1")},
				[]string{"exception", "kept"}, map[string]domain.GroupModelPricingSetting{
					"exception": excludedSetting, "kept": plannerTaskSetting("per_request", 0, "1"),
				}),
		},
		{
			name: "source group empty",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture(nil, []string{"exception"},
					map[string]domain.GroupModelPricingSetting{"exception": excludedSetting})
				input.SourceState = SourceGroupEmpty
				input.SourceGroup.Models = nil
				return input
			}(),
		},
		{
			name: "source group missing",
			input: func() CompleteMirrorPlannerInput {
				input := plannerFixture(nil, []string{"exception"},
					map[string]domain.GroupModelPricingSetting{"exception": excludedSetting})
				input.SourceState = SourceGroupMissing
				return input
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.input.Policy.ExcludedModels = []string{"exception"}
			beforeModels := append([]string(nil), test.input.CurrentChannelModels...)
			beforeSetting, hadSetting := test.input.CurrentContract.Groups["sale"]["exception"]
			plan, err := PlanCompleteMirror(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.NoChange || plan.Decision != DecisionNoChange || len(plan.Diffs) != 0 ||
				strings.Join(plan.DesiredChannelModels, "\x00") != strings.Join(beforeModels, "\x00") {
				t.Fatalf("excluded model was mutated: %#v", plan)
			}
			afterSetting, hasSetting := plan.DesiredContract.Groups["sale"]["exception"]
			if hadSetting != hasSetting || (hadSetting && !reflect.DeepEqual(afterSetting, beforeSetting)) {
				t.Fatalf("excluded contract changed: before=%#v after=%#v", beforeSetting, afterSetting)
			}
		})
	}
}

func plannerFixture(sourceModels []CanonicalModel, currentModels []string,
	settings map[string]domain.GroupModelPricingSetting) CompleteMirrorPlannerInput {
	policy := Policy{
		PolicyUUID: "9c03cdef-d23a-4a35-a2d3-3b178e6de1d1", PolicyVersion: 1, SchemaVersion: PolicySchemaVersion,
		SupplierID: 1, SourceGroup: "upstream", SourceModelPattern: "*", TargetSiteID: 2, TargetGroup: "sale",
		Enabled: true, Kind: PolicyKindCompleteMirror, PriceAnchor: PriceAnchorSourceBase,
		SyncGroupRatio: true, GroupRatioRule: Rule{Type: "same"}, SyncModelPrices: true,
		ModelPriceRule: Rule{Type: "same"}, SyncBillingMode: true, SyncModelAdditions: true,
		SyncModelRemovals: true, ModelScopeMode: "mirror_group", EmptyGroupAction: "disable_managed_channel",
		ExecutionMode: "auto", RoundingStep: defaultRoundingStep,
	}
	return CompleteMirrorPlannerInput{
		Policy: policy, SourceState: SourceGroupPresent,
		SourceGroup: CanonicalGroup{SourceGroupID: "upstream", SourceGroupName: "Upstream", RawGroupRatio: "1",
			RechargeValueFactor: "1", Models: sourceModels},
		SourceRevision: "source-rev", SourceObservedAtUnixMS: 100, CurrentGroupRatio: "1",
		CurrentContract: domain.GroupModelPricingContract{SchemaVersion: 1, Revision: "target-rev",
			Groups: map[string]map[string]domain.GroupModelPricingSetting{"sale": settings}},
		CurrentChannelModels: currentModels, CurrentChannelEnabled: true, ChannelStatusAuthority: "disable_only",
		OwnedModels: plannerOwnership(currentModels...),
	}
}

func plannerTaskModel(name, unit string, duration int, price string) CanonicalModel {
	kind := unit
	dimensionUnit := "currency_" + unit
	if unit == "fixed_duration" {
		dimensionUnit = "currency_per_fixed_duration"
	}
	return CanonicalModel{SourceModelID: name, SourceExposedName: name, UpstreamRequestName: name,
		BillingMode: "fixed_price", Currency: "USD", PriceDimensions: []PriceDimension{{
			Kind: kind, Unit: dimensionUnit, DurationSeconds: duration, DecimalValue: price,
		}}}
}

func plannerTaskSetting(unit string, duration int, price string) domain.GroupModelPricingSetting {
	return domain.GroupModelPricingSetting{BillingMode: "fixed_price", ModelPrice: plannerString(price),
		TaskProfile: &domain.GroupModelTaskProfile{Unit: unit, FixedDurationSeconds: duration},
		Source:      "relayops:policy/9c03cdef-d23a-4a35-a2d3-3b178e6de1d1", SourceRevision: "old", SourceObservedAtUnixMS: 1}
}

func plannerTokenModel(name, inputPrice, outputPrice string) CanonicalModel {
	return CanonicalModel{SourceModelID: name, SourceExposedName: name, UpstreamRequestName: name,
		BillingMode: "token_vector", Currency: "USD", PriceDimensions: []PriceDimension{
			{Kind: "input_per_1m", Unit: "currency_per_1m_tokens", DecimalValue: inputPrice},
			{Kind: "output_per_1m", Unit: "currency_per_1m_tokens", DecimalValue: outputPrice},
		}}
}

func plannerMembershipOnlyModel(name string) CanonicalModel {
	return CanonicalModel{SourceModelID: name, SourceExposedName: name, UpstreamRequestName: name,
		BillingMode: membershipOnlyBillingMode, Currency: "USD"}
}

func plannerTokenSetting(modelRatio, completionRatio string) domain.GroupModelPricingSetting {
	return domain.GroupModelPricingSetting{BillingMode: "ratio", ModelRatio: plannerString(modelRatio),
		CompletionRatio: plannerString(completionRatio), Source: "relayops:policy/9c03cdef-d23a-4a35-a2d3-3b178e6de1d1",
		SourceRevision: "old", SourceObservedAtUnixMS: 1}
}

func plannerString(value string) *string { return &value }

func plannerOwnership(models ...string) []OwnedModelState {
	result := make([]OwnedModelState, 0, len(models))
	seen := map[string]struct{}{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, OwnedModelState{TargetModel: model, ContractOwned: true, MembershipOwned: true})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TargetModel < result[j].TargetModel })
	return result
}

func plannerDiffSignatures(diffs []MirrorFunctionalDiff) []string {
	if len(diffs) == 0 {
		return nil
	}
	result := make([]string, 0, len(diffs))
	for _, diff := range diffs {
		result = append(result, strings.Join([]string{string(diff.Phase), string(diff.Kind), diff.Model,
			diff.Field, diff.Before, diff.After}, "|"))
	}
	return result
}

func plannerStagePhases(stages []MirrorPlanStage) []MirrorPlanPhase {
	if len(stages) == 0 {
		return nil
	}
	result := make([]MirrorPlanPhase, 0, len(stages))
	for _, stage := range stages {
		result = append(result, stage.Phase)
	}
	return result
}

func plannerDiffPhaseSequence(diffs []MirrorFunctionalDiff) []MirrorPlanPhase {
	result := make([]MirrorPlanPhase, 0, len(diffs))
	for _, diff := range diffs {
		result = append(result, diff.Phase)
	}
	return result
}

func plannerGateCodes(gates []MirrorPlanGate) []string {
	if len(gates) == 0 {
		return nil
	}
	result := make([]string, 0, len(gates))
	for _, gate := range gates {
		result = append(result, gate.Code)
	}
	return result
}
