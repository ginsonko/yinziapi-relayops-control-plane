package mirror

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const testPolicyUUID = "7c3bb676-f121-4ced-9618-3fb9f62159de"

func TestCompleteMirrorDefaultsAreExplicitAndCapabilityGated(t *testing.T) {
	document := NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID = 7
	document.SourceGroup = "cheap"
	document.TargetSiteID = 3
	document.TargetGroup = "sale"
	policy, err := DecodePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	if policy.PriceAnchor != PriceAnchorSourceBase || policy.SyncGroupRatio || !policy.SyncModelPrices ||
		!policy.SyncBillingMode || !policy.SyncModelAdditions || !policy.SyncModelRemovals ||
		policy.ModelScopeMode != "mirror_group" || policy.ExecutionMode != "auto" || !policy.Enabled {
		t.Fatalf("complete mirror defaults = %#v", policy)
	}
	if err := ValidatePolicy(policy, PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		t.Fatalf("capability validation: %v", err)
	}
	if err := ValidatePolicy(policy, PolicyCapabilities{CompleteModelSet: true}); !errors.Is(err, ErrCapabilityRequired) {
		t.Fatalf("missing CAS capability error = %v", err)
	}
}

func TestCompleteMirrorDecodeNormalizesLegacyGroupRatioOwnership(t *testing.T) {
	document := NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 7, "cheap"
	document.TargetSiteID, document.TargetGroup = 3, "sale"
	yes := true
	document.SyncGroupRatio = &yes
	document.GroupRatioRule = Rule{Type: "multiply", Value: "9"}

	policy, err := DecodePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	if policy.SyncGroupRatio || policy.GroupRatioRule != (Rule{Type: "same"}) {
		t.Fatalf("complete mirror retained GroupRatio ownership: %#v", policy)
	}
}

func TestCompleteMirrorExceptionModelsAreNormalizedAndBackwardCompatible(t *testing.T) {
	document := NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 7, "cheap"
	document.TargetSiteID, document.TargetGroup = 3, "sale"
	document.ExcludedModels = []string{" z-model ", "a-model", "z-model"}
	policy, err := DecodePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(policy.ExcludedModels, []string{"a-model", "z-model"}) {
		t.Fatalf("normalized exclusions=%#v", policy.ExcludedModels)
	}

	document.ExcludedModels = nil
	legacyCompatible, err := DecodePolicy(document)
	if err != nil || len(legacyCompatible.ExcludedModels) != 0 {
		t.Fatalf("missing optional exclusion list=%#v error=%v", legacyCompatible.ExcludedModels, err)
	}
}

func TestSchemaV3MissingBooleanIsNotDecodedAsFalse(t *testing.T) {
	document := NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 7, "cheap"
	document.TargetSiteID, document.TargetGroup = 3, "sale"
	document.SyncModelRemovals = nil
	if _, err := DecodePolicy(document); err == nil || !strings.Contains(err.Error(), "sync_model_removals") {
		t.Fatalf("missing boolean error = %v", err)
	}
}

func TestLegacyPolicyMigrationMatrixPreservesBindingsAndLimits(t *testing.T) {
	base := domain.PricingPolicy{ID: 19, SupplierID: 7, SourceGroup: "upstream", SourceModelPattern: "gpt-*",
		TargetSiteID: 3, TargetGroup: "local", SourceBasis: "equivalent_cost", RuleType: "add", Value: "0.03",
		Enabled: false, ExecutionMode: "auto", MinimumRatio: "0.1", MaximumRatio: "9", RoundingStep: "0.01",
		OnlyFollowIncrease: true}
	groupPolicy, err := MigrateLegacyPolicy(testPolicyUUID, base)
	if err != nil {
		t.Fatal(err)
	}
	if groupPolicy.Kind != PolicyKindLegacyGroup || groupPolicy.PriceAnchor != PriceAnchorSourceBase ||
		!groupPolicy.SyncGroupRatio || groupPolicy.SyncModelPrices || groupPolicy.SyncModelAdditions ||
		groupPolicy.GroupRatioRule != (Rule{Type: "add", Value: "0.03"}) || groupPolicy.Enabled ||
		groupPolicy.SourceModelPattern != "gpt-*" || groupPolicy.MinimumRatio != "0.1" ||
		groupPolicy.MaximumRatio != "9" || !groupPolicy.OnlyFollowIncrease {
		t.Fatalf("legacy group migration = %#v", groupPolicy)
	}

	base.SourceBasis = "group_model_contract"
	base.RuleType = "target_margin"
	base.Value = "0.2"
	contractPolicy, err := MigrateLegacyPolicy(testPolicyUUID, base)
	if err != nil {
		t.Fatal(err)
	}
	if contractPolicy.Kind != PolicyKindLegacyContract || contractPolicy.PriceAnchor != PriceAnchorEffectiveSale ||
		contractPolicy.SyncGroupRatio || !contractPolicy.SyncModelPrices || !contractPolicy.SyncBillingMode ||
		contractPolicy.ModelScopeMode != "fixed_allowlist" || contractPolicy.LegacyRuleTarget != "model_price" ||
		contractPolicy.ModelPriceRule != (Rule{Type: "target_margin", Value: "0.2"}) {
		t.Fatalf("legacy contract migration = %#v", contractPolicy)
	}
}

func TestPolicyRejectsUnsafeCombinations(t *testing.T) {
	document := NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 1, "g"
	document.TargetSiteID, document.TargetGroup = 2, "t"
	noBilling := false
	document.SyncBillingMode = &noBilling
	if _, err := DecodePolicy(document); err == nil || !strings.Contains(err.Error(), "billing-mode") {
		t.Fatalf("price-without-billing error = %v", err)
	}

	document = NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 1, "g"
	document.TargetSiteID, document.TargetGroup = 2, "t"
	document.ModelScopeMode = "fixed_allowlist"
	if _, err := DecodePolicy(document); err == nil || !strings.Contains(err.Error(), "mirror_group") {
		t.Fatalf("set-change ownership error = %v", err)
	}

	document = NewCompleteMirrorDocument(testPolicyUUID)
	document.SupplierID, document.SourceGroup = 1, "g"
	document.TargetSiteID, document.TargetGroup = 2, "t"
	document.ModelPriceRule = Rule{Type: "target_margin", Value: "0.2"}
	if _, err := DecodePolicy(document); err == nil || !strings.Contains(err.Error(), "group ratio") {
		t.Fatalf("source-base model margin error = %v", err)
	}
	document.ModelPriceRule = Rule{Type: "same"}
	document.GroupRatioRule = Rule{Type: "target_margin", Value: "0.2"}
	if _, err := DecodePolicy(document); err != nil {
		t.Fatalf("source-base group margin should be valid: %v", err)
	}
}
