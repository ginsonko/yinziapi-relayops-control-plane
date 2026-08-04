package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

func (s *Service) simulateGroupModelContract(ctx context.Context, policy domain.PricingPolicy, supplier domain.Supplier,
	target domain.TargetSite, snapshot domain.CatalogSnapshot, evidence domain.CatalogEvidenceStatus, group domain.GroupOffer,
	matchedModels []string, targetState targetReadState, safety SafetyConfig) (domain.PolicySimulation, error) {
	current := targetState.snapshot
	if !current.GroupModelPricingSupported || current.GroupModelPricing == nil {
		reason := current.GroupModelPricingError
		if reason == "" {
			reason = "target does not expose the atomic group-model pricing capability"
		}
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "group_model_pricing_unavailable",
			"upgrade the target NewAPI or change this rule to a proven legacy representation", policy, supplier, target, group,
			matchedModels, errors.New(reason))
	}
	groupRatio := strings.TrimSpace(current.GroupRatio[policy.TargetGroup])
	if _, err := positiveContractDecimal(groupRatio, "target group ratio"); err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "target_group_ratio_unavailable",
			"restore a positive target GroupRatio before continuing per-model synchronization", policy, supplier, target, group,
			matchedModels, err)
	}
	plan, err := listing.PlanGroupModelPricing(listing.GroupModelPricingPlanInput{
		SupplierID: supplier.ID, SourceCatalog: snapshot, SourceGroup: group, Models: matchedModels,
		TargetGroup: policy.TargetGroup, TargetGroupRatio: groupRatio, PricingRule: listingRuleFromPolicy(policy),
		Expected: *current.GroupModelPricing,
	})
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "group_model_pricing_plan_invalid",
			"inspect the listed model, unit and monetary-dimension diagnostics and correct the upstream connector or rule",
			policy, supplier, target, group, matchedModels, err)
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	groupModelAdapter, err := targets.AsGroupModelPricingAdapter(adapter)
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	preview, err := groupModelAdapter.PreviewGroupModelPricing(ctx, target, current.GroupModelPricing.Revision, plan.DesiredContract)
	if err != nil {
		return domain.PolicySimulation{}, fmt.Errorf("preview continuing group-model pricing: %w", err)
	}
	if !preview.ExpectedRevisionMatches || preview.CurrentRevision != current.GroupModelPricing.Revision {
		return domain.PolicySimulation{}, errors.New("target group-model pricing changed during simulation; recalculate the rule")
	}
	if sameFunctionalContractOwnership(preview.Current, preview.Desired) {
		preview.Desired = preview.Current
		preview.DesiredRevision = preview.CurrentRevision
		preview.Diff = nil
		preview.NoChange = true
	}
	plan.ExpectedRevision = preview.CurrentRevision
	plan.DesiredRevision = preview.DesiredRevision
	plan.ExpectedContract = preview.Current
	plan.DesiredContract = preview.Desired
	plan.Diff = preview.Diff

	calculation, err := pricing.Calculate(pricing.Input{SourceRatio: ratios.DecisionRatio(group), CostRatio: ratios.DecisionRatio(group),
		CurrentRatio: groupRatio, Policy: policy})
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	calculation.EquivalentCostRatio = ratios.DecisionRatio(group)
	// A contract policy changes per-model base prices while deliberately
	// keeping GroupRatio stable. The complete desired values live in plan.Models.
	calculation.TargetRatio = groupRatio
	calculation.SourceEvidenceHash = snapshot.Evidence.ResponseHash
	calculation.EvidenceHash = plan.DesiredRevision
	calculation.RawSourceRatio = group.RawRatio
	calculation.RechargeValueFactor = supplier.RechargeValueFactor
	calculation.PolicyFingerprint = pricing.PolicyFingerprint(policy)
	calculation.InputFingerprint = groupModelSimulationFingerprint(policy, snapshot.Evidence.ResponseHash, groupRatio, plan)
	calculation.MarginRate = contractStringPointer(plan.MinimumMarginRate)
	calculation.Formula = "each effective sale dimension is recalculated from frozen supplier cash cost; target base price = effective sale / GroupRatio"
	calculation.Warnings = append(calculation.Warnings,
		"The target GroupRatio remains unchanged; only affected group-model contract entries are synchronized")

	warnings := append([]string{}, snapshot.Evidence.Warnings...)
	warnings = append(warnings, calculation.Warnings...)
	if contractHasChange(plan) {
		if !evidence.AutoEligible {
			message := fmt.Sprintf("supplier price evidence still needs %d matching observation(s); automatic contract write is paused", evidence.RemainingObservations)
			calculation.AutomationEligible = false
			calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
			warnings = append(warnings, message)
		}
		freshness := time.Duration(max(policyFreshnessSeconds(policy), 300)) * time.Second
		if snapshot.Evidence.ObservedAt.IsZero() || time.Since(snapshot.Evidence.ObservedAt) > freshness {
			message := "supplier price evidence is stale; automatic contract write is paused until collection succeeds"
			calculation.AutomationEligible = false
			calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
			warnings = append(warnings, message)
		}
		eligible, safetyWarnings := contractChangeSafety(plan, safety)
		warnings = append(warnings, safetyWarnings...)
		if !eligible {
			calculation.AutomationEligible = false
			for _, message := range safetyWarnings {
				calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
			}
		}
	}

	totalDimensions := 0
	costVector := make(map[string]string)
	for _, model := range plan.Models {
		for _, dimension := range model.Dimensions {
			totalDimensions++
			costVector[model.Model+"/"+dimension.Dimension] = dimension.SourceCashCost
		}
	}
	targetCatalogHash := ""
	if targetState.catalogErr == nil {
		targetCatalogHash = pricing.ModelPriceHash(targetState.catalog)
	}
	return domain.PolicySimulation{
		Policy: policy, SourceGroup: group, MatchedModels: append([]string(nil), matchedModels...),
		EquivalentCostRatio: ratios.DecisionRatio(group), CostRatioVector: costVector,
		ComparableDimensions: totalDimensions, TotalDimensions: totalDimensions, CoverageRatio: "1",
		ImpactComplete: true, ImpactConfidence: "high", CurrentRatio: groupRatio, Calculation: calculation,
		Evidence: evidence, EvidenceOnlyBlocked: contractHasChange(plan) && !evidence.AutoEligible,
		TargetHash: plan.ExpectedRevision, TargetCatalogHash: targetCatalogHash, Warnings: warnings,
		GroupModelPricing: plan,
	}, nil
}

func listingRuleFromPolicy(policy domain.PricingPolicy) domain.ListingPricingRule {
	return domain.ListingPricingRule{
		RuleType: policy.RuleType, Value: policy.Value, ExecutionMode: policy.ExecutionMode,
		ProfitBasis: policy.ProfitBasis, RevenueCashFactor: policy.RevenueCashFactor,
		CostCashFactor: policy.CostCashFactor, RiskBufferRate: policy.RiskBufferRate,
		AllowNegativeProfit: policy.AllowNegativeProfit, SubsidyBudgetDaily: policy.SubsidyBudgetDaily,
		MinimumRatio: policy.MinimumRatio, MaximumRatio: policy.MaximumRatio,
		ModelPriceRoundingStep: policy.RoundingStep, OnlyFollowIncrease: policy.OnlyFollowIncrease,
		OnlyFollowDecrease: policy.OnlyFollowDecrease,
	}
}

func contractHasChange(plan domain.ListingGroupModelPricingPlan) bool {
	return plan.ExpectedRevision != "" && plan.DesiredRevision != "" && plan.ExpectedRevision != plan.DesiredRevision
}

func sameFunctionalContractOwnership(current, desired domain.GroupModelPricingContract) bool {
	if current.SchemaVersion != desired.SchemaVersion || len(current.Groups) != len(desired.Groups) {
		return false
	}
	for group, currentModels := range current.Groups {
		desiredModels, exists := desired.Groups[group]
		if !exists || len(currentModels) != len(desiredModels) {
			return false
		}
		for model, currentSetting := range currentModels {
			desiredSetting, exists := desiredModels[model]
			if !exists || strings.TrimSpace(currentSetting.Source) != strings.TrimSpace(desiredSetting.Source) {
				return false
			}
			currentHash, currentErr := mirror.TargetModelFunctionalHash(group, model, currentSetting)
			desiredHash, desiredErr := mirror.TargetModelFunctionalHash(group, model, desiredSetting)
			if currentErr != nil || desiredErr != nil || currentHash != desiredHash {
				return false
			}
		}
	}
	return true
}

func simulationHasChange(simulation domain.PolicySimulation) bool {
	if simulation.Policy.SourceBasis == "group_model_contract" || simulation.GroupModelPricing.Mode == "atomic_contract" {
		return contractHasChange(simulation.GroupModelPricing)
	}
	return !decimalEqual(simulation.CurrentRatio, simulation.Calculation.TargetRatio)
}

func contractChangeSafety(plan domain.ListingGroupModelPricingPlan, config SafetyConfig) (bool, []string) {
	warnings := make([]string, 0)
	decreaseLimit, _ := decimal.NewFromString(strings.TrimSpace(config.MaximumAutomaticDecreaseRate))
	for _, model := range plan.Models {
		current, ok := plan.ExpectedContract.Groups[plan.TargetGroup][model.Model]
		if !ok {
			message := fmt.Sprintf("model %s has no previous owned contract entry; automatic replacement requires approval", model.Model)
			warnings = append(warnings, message)
			continue
		}
		if current.BillingMode != model.Setting.BillingMode || !sameTaskProfile(current.TaskProfile, model.Setting.TaskProfile) {
			warnings = append(warnings, fmt.Sprintf("model %s changes billing mode or unit; automatic replacement requires approval", model.Model))
			continue
		}
		currentDimensions, err := listing.EffectiveContractDimensions(current, model.TargetGroupRatio)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("model %s current contract is not comparable: %v", model.Model, err))
			continue
		}
		for _, dimension := range model.Dimensions {
			beforeRaw, exists := currentDimensions[dimension.Dimension]
			if !exists {
				warnings = append(warnings, fmt.Sprintf("model %s dimension %s has no comparable previous price", model.Model, dimension.Dimension))
				continue
			}
			before, _ := decimal.NewFromString(beforeRaw)
			after, _ := decimal.NewFromString(dimension.EffectiveSale)
			if before.IsPositive() && after.LessThan(before) && decreaseLimit.IsPositive() {
				rate := before.Sub(after).Div(before)
				if rate.GreaterThan(decreaseLimit) {
					warnings = append(warnings, fmt.Sprintf("model %s dimension %s decreases by %s%%, above the unattended decrease gate %s%%",
						model.Model, dimension.Dimension, rate.Mul(decimal.NewFromInt(100)).StringFixed(2),
						decreaseLimit.Mul(decimal.NewFromInt(100)).StringFixed(2)))
				}
			}
		}
	}
	// Every warning produced here represents a missing comparison or a hard
	// decrease gate. Confirmed increases intentionally remain automatic so an
	// upstream price hike cannot leave the operator selling below the rule.
	return len(warnings) == 0, warnings
}

func groupModelSimulationFingerprint(policy domain.PricingPolicy, sourceEvidence, groupRatio string,
	plan domain.ListingGroupModelPricingPlan) string {
	payload := struct {
		Policy         string `json:"policy"`
		SourceEvidence string `json:"source_evidence"`
		GroupRatio     string `json:"group_ratio"`
		Expected       string `json:"expected"`
		Desired        string `json:"desired"`
	}{pricing.PolicyFingerprint(policy), sourceEvidence, groupRatio, plan.ExpectedRevision, plan.DesiredRevision}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func positiveContractDecimal(raw, name string) (decimal.Decimal, error) {
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil || !value.IsPositive() {
		return decimal.Zero, fmt.Errorf("%s must be a positive decimal", name)
	}
	return value, nil
}

func contractStringPointer(value string) *string { return &value }

func sameTaskProfile(left, right *domain.GroupModelTaskProfile) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Unit == right.Unit && left.FixedDurationSeconds == right.FixedDurationSeconds
}
