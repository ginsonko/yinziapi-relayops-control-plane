package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
)

func TestConsoleBootstrapScopeContract(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "full", "overview", "pricing", "changes", "connectors", "discoveries"} {
		if _, err := parseConsoleBootstrapScope(raw); err != nil {
			t.Fatalf("parseConsoleBootstrapScope(%q): %v", raw, err)
		}
	}
	if _, err := parseConsoleBootstrapScope("everything"); err == nil {
		t.Fatal("unknown scope silently fell back to full")
	}
	if !bootstrapScopePricing.includesPricing() || bootstrapScopePricing.includesChanges() ||
		!bootstrapScopeChanges.includesChanges() || bootstrapScopeChanges.includesConnectors() ||
		!bootstrapScopeConnectors.includesConnectors() || bootstrapScopeConnectors.needsTargetSnapshots() ||
		!bootstrapScopeDiscoveries.includesDiscoveries() || bootstrapScopeOverview.needsTargetSnapshots() {
		t.Fatal("scoped bootstrap capability matrix is inconsistent")
	}
}

func TestOptionalFloatValuePreservesUnknownAndExplicitZero(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "  ", "not-a-number"} {
		if got := optionalFloatValue(value); got != nil {
			t.Fatalf("optionalFloatValue(%q) = %v, want nil", value, *got)
		}
	}
	zero := optionalFloatValue("0")
	if zero == nil || *zero != 0 {
		t.Fatalf("explicit zero = %v, want pointer to 0", zero)
	}
	priced := optionalFloatValue("0.06")
	if priced == nil || *priced != 0.06 {
		t.Fatalf("priced ratio = %v, want pointer to 0.06", priced)
	}
}

func TestDetailedRunwayForecastCapsLongHorizons(t *testing.T) {
	t.Parallel()
	observed := time.Date(2026, 7, 25, 4, 0, 0, 0, time.UTC)
	if got := detailedRunwayForecastAt(observed, 48); got != observed.Add(48*time.Hour).Format(time.RFC3339) {
		t.Fatalf("48 hour forecast = %q", got)
	}
	if got := detailedRunwayForecastAt(observed, 721); got != "" {
		t.Fatalf("long runway emitted misleading exact forecast %q", got)
	}
	if got := detailedRunwayForecastAt(observed, -1); got != "" {
		t.Fatalf("negative runway emitted forecast %q", got)
	}
}

func TestOptionalCapabilityFailureDoesNotInvalidateFreshCoreSession(t *testing.T) {
	t.Parallel()
	items := []domain.AuthCapabilityEvidence{
		{Capability: "customer_identity", EndpointType: "customer_identity", Status: "verified"},
		{Capability: "customer_usage", EndpointType: "customer_usage", Status: "denied", HTTPStatus: http.StatusForbidden, ErrorClass: "permission_denied"},
	}
	if status := authStatusFromCapabilities(items); status != "valid" {
		t.Fatalf("optional endpoint invalidated core session: %q", status)
	}
	if authAttentionFromCapabilities(items) {
		t.Fatal("optional endpoint failure requested human reauthorization")
	}
	items[0] = domain.AuthCapabilityEvidence{Capability: "customer_identity", EndpointType: "customer_identity", Status: "expired_or_invalid", HTTPStatus: http.StatusUnauthorized}
	if status := authStatusFromCapabilities(items); status != "invalid" || !authAttentionFromCapabilities(items) {
		t.Fatalf("core auth failure was not surfaced: status=%q attention=%v", status, authAttentionFromCapabilities(items))
	}
}

func TestConsoleDiscoverySerializesUnknownRateAsNull(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(consoleDiscovery{ID: "sample", UpstreamGroup: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"upstreamRate":null`) {
		t.Fatalf("unknown rate payload = %s", payload)
	}
}

func TestConsoleProposalPreservesLifecycleEvidence(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(consoleProposal{ID: "7", Status: "stale", Readback: "0.06", Error: "evidence changed"})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	for _, fragment := range []string{`"status":"stale"`, `"readback":"0.06"`, `"error":"evidence changed"`} {
		if !strings.Contains(encoded, fragment) {
			t.Fatalf("proposal payload %s does not contain %s", encoded, fragment)
		}
	}
}

func TestConsoleProposalExplainsAtomicModelPricingWithoutTreatingRevisionAsRatio(t *testing.T) {
	t.Parallel()
	oldPrice := "2"
	proposal := domain.ChangeProposal{ID: 31, TargetGroup: "special-video", ExpectedOld: "0.5", ProposedValue: "0.5",
		Status: "applied", CreatedAt: time.Date(2026, 7, 26, 1, 2, 3, 0, time.UTC), ReadbackValue: "sha256-readback",
		Calculation: domain.PriceCalculation{Formula: "cost / (1 - margin)", ContributionProfit: "0.25"},
		GroupModelPricing: domain.ListingGroupModelPricingPlan{Mode: "atomic_contract", TargetGroup: "special-video",
			TargetGroupRatio: "0.5", ExpectedRevision: "sha256-before-contract", DesiredRevision: "sha256-after-contract",
			MinimumMarginRate: "0.2", ExpectedContract: domain.GroupModelPricingContract{Groups: map[string]map[string]domain.GroupModelPricingSetting{
				"special-video": {"jimeng-video": {BillingMode: "fixed_price", ModelPrice: &oldPrice,
					TaskProfile: &domain.GroupModelTaskProfile{Unit: "per_second"}}},
			}}, Models: []domain.ListingModelPricingEvidence{{Model: "jimeng-video", BillingMode: "fixed_price",
				BillingUnit: "per_second", Currency: "CNY", MinimumMarginRate: "0.2",
				Dimensions: []domain.ListingPriceDimensionEvidence{{Dimension: "per_second", RiskAdjustedCashCost: "0.8",
					EffectiveSale: "1.25", MarginRate: "0.36"}}}}},
		GroupModelPricingReceipt: domain.ListingGroupModelPricingReceipt{AppliedRevision: "sha256-readback"}}

	result := buildConsoleProposal(proposal, "Yinzi API")
	if result.PricingMode != "group_model_contract" || result.Title != "special-video 逐模型价格同步" ||
		result.Before == displayConsoleValue(proposal.ExpectedOld) || result.After == displayConsoleValue(proposal.ProposedValue) {
		t.Fatalf("contract proposal was flattened into a ratio: %#v", result)
	}
	if result.ChangedModels != 1 || result.MinimumMargin != "0.2" || result.Readback != "sha256-readb…" || len(result.ModelChanges) != 1 {
		t.Fatalf("contract evidence is incomplete: %#v", result)
	}
	dimension := result.ModelChanges[0].Dimensions[0]
	if dimension.CurrentSale != "1" || dimension.ProposedSale != "1.25" || dimension.SourceCashCost != "0.8" || dimension.CurrentInherited {
		t.Fatalf("model price diff = %#v", dimension)
	}
}

func TestAuthStatusKeepsValidCapabilityWhenOptionalEndpointFails(t *testing.T) {
	t.Parallel()
	items := []domain.AuthCapabilityEvidence{
		{Capability: "customer_session", EndpointType: "balance", Status: "verified"},
		{Capability: "customer_session", EndpointType: "usage", Status: "expired_or_invalid"},
	}
	if got := authStatusFromCapabilities(items); got != "valid" {
		t.Fatalf("mixed endpoint status = %q, want valid", got)
	}
	if authAttentionFromCapabilities(items) {
		t.Fatal("an optional usage failure must not request reauthorization while a core balance capability is verified")
	}
	items[0].Status = "failed"
	if got := authStatusFromCapabilities(items); got != "invalid" {
		t.Fatalf("all endpoint failures = %q, want invalid", got)
	}
}

func TestAuthAttentionIgnoresPublicAndTransportFailures(t *testing.T) {
	t.Parallel()
	items := []domain.AuthCapabilityEvidence{
		{Capability: "anonymous", EndpointType: "catalog", Status: "failed", HTTPStatus: 401, ErrorClass: "auth_missing"},
		{Capability: "customer_session", EndpointType: "balance", Status: "failed", ErrorClass: "connection_reset"},
	}
	if authAttentionFromCapabilities(items) {
		t.Fatal("public rejection or private transport failure must not request credential renewal")
	}
	items[1].HTTPStatus = 401
	items[1].ErrorClass = "auth_expired"
	if !authAttentionFromCapabilities(items) {
		t.Fatal("private 401 must surface credential attention")
	}
}

func TestFreshCoreEvidenceSupersedesStaleFailureFromOldCredential(t *testing.T) {
	t.Parallel()
	oldTime := time.Date(2026, 7, 22, 18, 18, 0, 0, time.UTC)
	freshTime := oldTime.Add(48 * time.Hour)
	credentialID := int64(20)
	items := []domain.AuthCapabilityEvidence{
		{Capability: "customer_session", EndpointType: "catalog", Status: "expired_or_invalid", HTTPStatus: http.StatusUnauthorized,
			ErrorClass: "auth_expired", LastAttemptAt: oldTime},
		{Capability: "customer_session", EndpointType: "customer_catalog", Status: "verified", CredentialID: &credentialID,
			LastAttemptAt: freshTime, VerifiedAt: &freshTime},
		{Capability: "customer_session", EndpointType: "customer_balance", Status: "verified", CredentialID: &credentialID,
			LastAttemptAt: freshTime, VerifiedAt: &freshTime},
	}
	if got := authStatusFromCapabilities(items); got != "valid" {
		t.Fatalf("fresh credential status = %q", got)
	}
	if reasons := authAttentionReasonsFromCapabilities(items); len(reasons) != 0 {
		t.Fatalf("stale failure still requested authorization: %#v", reasons)
	}
	items[1] = domain.AuthCapabilityEvidence{Capability: "customer_session", EndpointType: "customer_catalog", Status: "failed",
		CredentialID: &credentialID, HTTPStatus: http.StatusForbidden, ErrorClass: "permission_denied", LastAttemptAt: freshTime.Add(time.Minute)}
	if reasons := authAttentionReasonsFromCapabilities(items); len(reasons) != 1 || reasons[0] != "私有目录" {
		t.Fatalf("fresh catalog failure reasons = %#v", reasons)
	}
}

func TestConsoleBootstrapSerializesAutomationTruth(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(consoleBootstrap{AutomationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"automationEnabled":true`) {
		t.Fatalf("automation truth missing from bootstrap payload: %s", payload)
	}
}

func TestConsoleActivityMetadataAllowsDiagnosticsAndRejectsInternalOrCredentialFields(t *testing.T) {
	t.Parallel()
	metadata := consoleActivityMetadata(map[string]string{
		"diagnostic_category":    "source_group_models_empty",
		"policy_id":              "17",
		"supplier_name":          "upstream",
		"source_group":           "economy",
		"target_site_name":       "production",
		"target_group":           "gpt-plus",
		"suggested_action":       "refresh the upstream catalog",
		"readback_revision":      "sha256-readback",
		"changed_models":         "3",
		"minimum_margin":         "0.2",
		"automation_failure_key": "internal-coalescing-key",
		"error_class":            "internal-error-class",
		"authorization":          "Bearer secret",
		"cookie":                 "session=secret",
		"password":               "secret",
		"response_body":          "private upstream response",
	})
	for _, key := range []string{"diagnostic_category", "policy_id", "supplier_name", "source_group", "target_site_name", "target_group", "suggested_action", "readback_revision", "changed_models", "minimum_margin"} {
		if metadata[key] == "" {
			t.Fatalf("allowed diagnostic field %q was removed: %#v", key, metadata)
		}
	}
	for _, key := range []string{"automation_failure_key", "error_class", "authorization", "cookie", "password", "response_body"} {
		if _, exists := metadata[key]; exists {
			t.Fatalf("private activity field %q reached console metadata: %#v", key, metadata)
		}
	}
}

func TestNoChangeAutomationProjectionRemovesHistoricalBlocker(t *testing.T) {
	t.Parallel()
	eligible := false
	projectedEligible, reason := normalizeNoChangeAutomationProjection("no_change", &eligible, "还需连续两次相同读数确认")
	if projectedEligible != nil || reason != "" {
		t.Fatalf("no-change projection retained to-do state: eligible=%v reason=%q", projectedEligible, reason)
	}
	projectedEligible, reason = normalizeNoChangeAutomationProjection("pending", &eligible, "真实价差仍需确认")
	if projectedEligible == nil || *projectedEligible || reason != "真实价差仍需确认" {
		t.Fatalf("actionable projection was changed: eligible=%v reason=%q", projectedEligible, reason)
	}
}

func TestConsolePolicyPriceChangeRequiresTwoKnownDifferentValues(t *testing.T) {
	t.Parallel()
	current, same, changed := 0.16, 0.1600000001, 0.17
	if consolePolicyHasPriceChange(nil, &changed) || consolePolicyHasPriceChange(&current, nil) {
		t.Fatal("unknown target or suggestion was presented as a confirmed price change")
	}
	if consolePolicyHasPriceChange(&current, &same) {
		t.Fatal("display-equivalent values were presented as a price change")
	}
	if !consolePolicyHasPriceChange(&current, &changed) {
		t.Fatal("real price difference was hidden")
	}
}

func TestDisplayConsoleValueCanonicalizesDecimalText(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{"0.100000000000": "0.1", "6.350000000000": "6.35", "": "未设置", "unknown": "unknown"} {
		if got := displayConsoleValue(input); got != want {
			t.Fatalf("displayConsoleValue(%q)=%q, want %q", input, got, want)
		}
	}
	if got := optionalConsoleValue(""); got != "" {
		t.Fatalf("optional empty value = %q, want empty", got)
	}
}

func TestProjectedCredentialStatusUsesRealRenewalWindowAndCoreEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 6, 0, 0, 0, time.UTC)
	farExpiry := now.Add(time.Hour)
	nearExpiry := now.Add(10 * time.Minute)
	expired := now.Add(-time.Second)
	cases := []struct {
		name       string
		credential domain.Credential
		core       string
		managed    bool
		pending    bool
		want       string
	}{
		{name: "far from window", credential: domain.Credential{Status: "active", ExpiresAt: &farExpiry}, want: "valid"},
		{name: "near expiry needs operator", credential: domain.Credential{Status: "active", ExpiresAt: &nearExpiry}, want: "expiring"},
		{name: "healthy custody covers near expiry", credential: domain.Credential{Status: "active", ExpiresAt: &nearExpiry}, managed: true, want: "valid"},
		{name: "expired is invalid even when managed", credential: domain.Credential{Status: "active", ExpiresAt: &expired}, managed: true, want: "invalid"},
		{name: "bounded automatic recovery keeps prior status", credential: domain.Credential{Status: "active", ExpiresAt: &expired}, managed: true, pending: true, want: "valid"},
		{name: "bounded automatic recovery covers fresh auth failure", credential: domain.Credential{Status: "active", ExpiresAt: &farExpiry}, core: "invalid", managed: true, pending: true, want: "valid"},
		{name: "fresh core evidence supersedes retry projection", credential: domain.Credential{Status: "retry_backoff", ExpiresAt: &nearExpiry}, core: "valid", managed: true, want: "valid"},
		{name: "failed core evidence wins", credential: domain.Credential{Status: "active", ExpiresAt: &farExpiry}, core: "invalid", managed: true, want: "invalid"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := projectedCredentialStatus(test.credential, test.core, test.managed, test.pending, now, 15*time.Minute); got != test.want {
				t.Fatalf("status=%q want=%q", got, test.want)
			}
		})
	}
}

func TestCredentialRecoveryRequiresEnabledNonInteractivePath(t *testing.T) {
	t.Parallel()
	healthy := domain.CredentialRenewalStatus{Configured: true, Enabled: true, State: "healthy"}
	if !credentialRenewalCanRecover(healthy) {
		t.Fatal("healthy automatic renewal was not recognized")
	}
	healthy.HumanAction = "solve_captcha"
	if credentialRenewalCanRecover(healthy) {
		t.Fatal("human action requirement was hidden as automatic recovery")
	}
	if browserCustodyCanRecover(&domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online", NeedsCapture: true}) {
		t.Fatal("custody that still needs capture was marked recoverable")
	}
	if !browserCustodyCanRecover(&domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online", NeedsCapture: true, RecoveryPending: true}) {
		t.Fatal("bounded online custody recovery was not recognized")
	}
	if browserCustodyCanRecover(&domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online", NeedsCapture: true, RecoveryPending: true, RecoveryOverdue: true}) {
		t.Fatal("overdue custody recovery was hidden as recoverable")
	}
	if !browserCustodyCanRecover(&domain.BrowserCustodyStatus{Enabled: true, HelperOnline: true, HelperState: "online"}) {
		t.Fatal("healthy online custody was not recognized")
	}
}

func TestConsoleCalculationValuesDoNotAliasSuggestedRateAndMargin(t *testing.T) {
	t.Parallel()
	contributionMargin := "-65.176"
	basis, suggested, ruleMargin := consoleCalculationValues(domain.PriceCalculation{
		SourceRatio: "0.08", TargetRatio: "0.10", MarginRate: &contributionMargin,
	})
	if basis == nil || suggested == nil || ruleMargin == nil || *basis != 0.08 || *suggested != 0.10 || *ruleMargin != 20 {
		t.Fatalf("console values = basis %v suggested %v margin %v", basis, suggested, ruleMargin)
	}
}

func TestRuleMarginUsesRuleBasisAndRejectsUndefinedDenominator(t *testing.T) {
	t.Parallel()
	margin := ratioMarginPercent("0.15", "0.17")
	if margin == nil || *margin < 11.7647 || *margin > 11.7648 {
		t.Fatalf("0.15 -> 0.17 rule margin = %v", margin)
	}
	for _, target := range []string{"", "0", "-1", "not-a-number"} {
		if got := ratioMarginPercent("0.1", target); got != nil {
			t.Fatalf("undefined rule margin for target %q = %v", target, *got)
		}
	}
}

func TestPolicyCurrentRatioPrefersLiveTargetAndDoesNotReviveDeletedGroup(t *testing.T) {
	t.Parallel()
	policy := domain.PricingPolicy{TargetSiteID: 3, TargetGroup: "gpt稳定备用分组-plus3"}
	view := policyProposalView{TrustedReadback: "0.16"}
	snapshots := map[int64]domain.TargetSnapshot{3: {GroupRatio: map[string]string{"gpt稳定备用分组-plus3": "0.1"}}}
	if value, status := policyCurrentRatio(policy, snapshots, view); value != "0.1" || status != "live" {
		t.Fatalf("live current ratio = %q/%q", value, status)
	}
	if value, status := policyCurrentRatio(policy, map[int64]domain.TargetSnapshot{3: {GroupRatio: map[string]string{}}}, view); value != "" || status != "missing_group" {
		t.Fatalf("deleted target group revived stale readback = %q/%q", value, status)
	}
	if value, status := policyCurrentRatio(policy, nil, view); value != "0.16" || status != "trusted_readback" {
		t.Fatalf("trusted fallback = %q/%q", value, status)
	}
}

func TestPolicyProposalViewSeparatesFreshSuggestionFromTrustedCurrentReadback(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	policies := []domain.PricingPolicy{{ID: 7, SupplierID: 3, TargetSiteID: 4, TargetGroup: "local", SourceGroup: "upstream", RuleType: "add", Value: "0.01"}}
	fingerprint := pricing.PolicyFingerprint(policies[0])
	snapshots := map[int64]domain.CatalogSnapshot{3: {Evidence: domain.Evidence{ResponseHash: "catalog-new"},
		Groups: []domain.GroupOffer{{ExternalID: "upstream", Ratio: "0.1", RawRatio: "0.1", EffectiveRatio: "0.1", RechargeValueFactor: "1"}}}}
	proposals := []domain.ChangeProposal{
		{ID: 12, PolicyID: 7, TargetSiteID: 4, TargetGroup: "local", Status: "pending", ExpectedOld: "0.09", ProposedValue: "0.11",
			Calculation: domain.PriceCalculation{SourceEvidenceHash: "catalog-new", PolicyFingerprint: fingerprint,
				InputFingerprint: "current-input", RawSourceRatio: "0.1", RechargeValueFactor: "1"}, UpdatedAt: base.Add(time.Minute)},
		{ID: 11, PolicyID: 7, Status: "applied", ExpectedOld: "0.06", ProposedValue: "0.09", ReadbackValue: "0.09",
			Calculation: domain.PriceCalculation{SourceEvidenceHash: "catalog-old"}, UpdatedAt: base},
	}

	view := buildPolicyProposalViews(proposals, policies, snapshots)[7]
	if !view.HasFresh || view.Fresh.ID != 12 || view.Fresh.ProposedValue != "0.11" {
		t.Fatalf("fresh proposal view = %#v", view)
	}
	if view.TrustedReadback != "0.09" {
		t.Fatalf("trusted current readback = %q, want 0.09", view.TrustedReadback)
	}
}

func TestPolicyProposalViewDoesNotPresentOldEvidenceAsCurrentSuggestion(t *testing.T) {
	t.Parallel()
	policies := []domain.PricingPolicy{{ID: 7, SupplierID: 3, TargetSiteID: 4, TargetGroup: "local", SourceGroup: "upstream", RuleType: "add", Value: "0.01"}}
	snapshots := map[int64]domain.CatalogSnapshot{3: {Evidence: domain.Evidence{ResponseHash: "catalog-new"}}}
	proposals := []domain.ChangeProposal{{ID: 11, PolicyID: 7, Status: "applied", ExpectedOld: "0.06",
		ProposedValue: "0.09", ReadbackValue: "0.09",
		Calculation: domain.PriceCalculation{SourceEvidenceHash: "catalog-old"}, UpdatedAt: time.Now().UTC()}}

	view := buildPolicyProposalViews(proposals, policies, snapshots)[7]
	if view.HasFresh {
		t.Fatalf("old evidence was exposed as a fresh suggestion: %#v", view.Fresh)
	}
	if view.TrustedReadback != "0.09" {
		t.Fatalf("applied readback was lost: %#v", view)
	}
}

func TestPolicyProposalViewInvalidatesChangedRuleWithSameCatalogHash(t *testing.T) {
	t.Parallel()
	oldPolicy := domain.PricingPolicy{ID: 7, SupplierID: 3, TargetSiteID: 4, TargetGroup: "local", SourceGroup: "upstream", RuleType: "add", Value: "0.16"}
	currentPolicy := oldPolicy
	currentPolicy.Value = "0.03"
	snapshots := map[int64]domain.CatalogSnapshot{3: {Evidence: domain.Evidence{ResponseHash: "same-catalog"},
		Groups: []domain.GroupOffer{{ExternalID: "upstream", Ratio: "0.13", RawRatio: "0.13", EffectiveRatio: "0.13", RechargeValueFactor: "1"}}}}
	proposal := domain.ChangeProposal{ID: 12, PolicyID: 7, TargetSiteID: 4, TargetGroup: "local", ProposedValue: "0.29",
		Calculation: domain.PriceCalculation{SourceEvidenceHash: "same-catalog", PolicyFingerprint: pricing.PolicyFingerprint(oldPolicy),
			InputFingerprint: "old-input", RawSourceRatio: "0.13", RechargeValueFactor: "1"}}

	view := buildPolicyProposalViews([]domain.ChangeProposal{proposal}, []domain.PricingPolicy{currentPolicy}, snapshots)[7]
	if view.HasFresh {
		t.Fatalf("old 0.29 proposal survived the current 0.13 + 0.03 rule: %#v", view.Fresh)
	}
}

func TestProposalLifecycleProjectionExpiresOldActionableRuleWithoutMutatingAuditRecord(t *testing.T) {
	t.Parallel()
	oldPolicy := domain.PricingPolicy{ID: 7, SupplierID: 3, TargetSiteID: 4, TargetGroup: "gpt-pro", SourceGroup: "codex-pro", RuleType: "add", Value: "0.16"}
	currentPolicy := oldPolicy
	currentPolicy.Value = "0.03"
	snapshots := map[int64]domain.CatalogSnapshot{3: {
		Evidence: domain.Evidence{ResponseHash: "catalog-current"},
		Groups:   []domain.GroupOffer{{ExternalID: "codex-pro", RawRatio: "0.13", Ratio: "0.13", EffectiveRatio: "0.13", RechargeValueFactor: "1"}},
	}}
	proposal := domain.ChangeProposal{ID: 12, PolicyID: 7, TargetSiteID: 4, TargetGroup: "gpt-pro", Status: "pending", ProposedValue: "0.29",
		Calculation: domain.PriceCalculation{SourceEvidenceHash: "catalog-current", PolicyFingerprint: pricing.PolicyFingerprint(oldPolicy),
			InputFingerprint: "old-input", RawSourceRatio: "0.13", RechargeValueFactor: "1"}}

	projected := projectProposalLifecycles([]domain.ChangeProposal{proposal}, []domain.PricingPolicy{currentPolicy}, snapshots)
	if len(projected) != 1 || projected[0].Status != "stale" || projected[0].Error != staleProposalProjectionMessage {
		t.Fatalf("projected proposal = %#v", projected)
	}
	if proposal.Status != "pending" || proposal.Error != "" {
		t.Fatalf("projection mutated stored audit value: %#v", proposal)
	}

	current := proposal
	current.Calculation.PolicyFingerprint = pricing.PolicyFingerprint(currentPolicy)
	current.Calculation.InputFingerprint = "current-input"
	if got := projectProposalLifecycles([]domain.ChangeProposal{current}, []domain.PricingPolicy{currentPolicy}, snapshots)[0]; got.Status != "pending" {
		t.Fatalf("current proposal was incorrectly expired: %#v", got)
	}
}
