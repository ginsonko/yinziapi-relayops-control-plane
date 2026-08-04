package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/forecast"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/listing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type consoleBootstrap struct {
	AutomationEnabled        bool                `json:"automationEnabled"`
	BackgroundWorkersEnabled bool                `json:"backgroundWorkersEnabled"`
	RemoteWritesEnabled      bool                `json:"remoteWritesEnabled"`
	Metrics                  consoleMetrics      `json:"metrics"`
	Suppliers                []consoleSupplier   `json:"suppliers"`
	WatchItems               []consoleWatch      `json:"watchItems"`
	WatchSettings            store.WatchSettings `json:"watchSettings"`
	WatchCandidateCount      int                 `json:"watchCandidateCount"`
	Policies                 []consolePolicy     `json:"policies"`
	Balances                 []consoleBalance    `json:"balances"`
	Discoveries              []consoleDiscovery  `json:"discoveries"`
	Proposals                []consoleProposal   `json:"proposals"`
	Connectors               []consoleConnector  `json:"connectors"`
	Credentials              []consoleCredential `json:"credentials"`
	Activities               []consoleActivity   `json:"activities"`
	Runtime                  consoleRuntime      `json:"runtime"`
	GeneratedAt              string              `json:"generatedAt"`
}

type consoleRuntime struct {
	StartedAt          string                 `json:"startedAt"`
	UptimeSeconds      int64                  `json:"uptimeSeconds"`
	DatabaseBytes      int64                  `json:"databaseBytes"`
	ProcessMemoryBytes uint64                 `json:"processMemoryBytes"`
	Goroutines         int                    `json:"goroutines"`
	Targets            []consoleRuntimeTarget `json:"targets"`
	GeneratedAt        string                 `json:"generatedAt"`
}

type consoleRuntimeTarget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
}

type consoleMetrics struct {
	SupplierCount     int      `json:"supplierCount"`
	HealthySuppliers  int      `json:"healthySuppliers"`
	ActiveAlerts      int      `json:"activeAlerts"`
	TodayCost         *float64 `json:"todayCost"`
	TodayRevenue      *float64 `json:"todayRevenue"`
	GrossMargin       *float64 `json:"grossMargin"`
	AutomatedPolicies int      `json:"automatedPolicies"`
	PendingChanges    int      `json:"pendingChanges"`
}

type consoleSupplier struct {
	ID                    string   `json:"id"`
	CredentialID          *string  `json:"credentialId"`
	Name                  string   `json:"name"`
	BaseURL               string   `json:"baseUrl"`
	Kind                  string   `json:"kind"`
	Enabled               bool     `json:"enabled"`
	Status                string   `json:"status"`
	AuthStatus            string   `json:"authStatus"`
	AuthAttention         bool     `json:"authAttention"`
	AuthAttentionReasons  []string `json:"authAttentionReasons,omitempty"`
	CollectInterval       int      `json:"collectInterval"`
	RechargeValueFactor   string   `json:"rechargeValueFactor"`
	LastCollectedAt       string   `json:"lastCollectedAt"`
	LastAttemptAt         string   `json:"lastAttemptAt"`
	LastSuccessAt         string   `json:"lastSuccessAt"`
	NextCollectedAt       string   `json:"nextCollectedAt"`
	LatencyMS             *int64   `json:"latencyMs"`
	LastFailureDurationMS *int64   `json:"lastFailureDurationMs"`
	LastErrorClass        string   `json:"lastErrorClass,omitempty"`
	GroupCount            int      `json:"groupCount"`
	ModelCount            int      `json:"modelCount"`
	Balance               *float64 `json:"balance"`
	BalanceCurrency       string   `json:"balanceCurrency,omitempty"`
	PriceChangeCount      int      `json:"priceChangeCount"`
	Focused               bool     `json:"focused"`
	Note                  string   `json:"note,omitempty"`
}

type consoleWatch struct {
	ID            string   `json:"id"`
	SupplierID    int64    `json:"supplierId"`
	Supplier      string   `json:"supplier"`
	ScopeType     string   `json:"scopeType"`
	GroupKey      string   `json:"groupKey"`
	ModelKey      string   `json:"modelKey"`
	Group         string   `json:"group"`
	Model         string   `json:"model"`
	DisplayName   string   `json:"displayName"`
	Note          string   `json:"note,omitempty"`
	Manual        bool     `json:"manual"`
	SourcePricing bool     `json:"sourcePricing"`
	SourceMirror  bool     `json:"sourceMirror"`
	SourceChange  bool     `json:"sourceChange"`
	EvidenceHash  string   `json:"evidenceHash,omitempty"`
	UpstreamRate  *float64 `json:"upstreamRate"`
	LocalRate     *float64 `json:"localRate"`
	GrossMargin   *float64 `json:"grossMargin"`
	Delta24H      *float64 `json:"delta24h"`
	Status        string   `json:"status"`
	UpdatedAt     string   `json:"updatedAt"`
}

type consolePolicy struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Supplier            string   `json:"supplier"`
	UpstreamGroup       string   `json:"upstreamGroup"`
	TargetGroup         string   `json:"targetGroup"`
	Mode                string   `json:"mode"`
	Parameter           float64  `json:"parameter"`
	UpstreamRate        float64  `json:"upstreamRate"`
	SuggestedRate       *float64 `json:"suggestedRate"`
	CurrentRate         *float64 `json:"currentRate"`
	CurrentRateStatus   string   `json:"currentRateStatus"`
	SourceBasis         string   `json:"sourceBasis"`
	RuleBasisRate       *float64 `json:"ruleBasisRate"`
	EquivalentCostRate  *float64 `json:"equivalentCostRate"`
	RuleMargin          *float64 `json:"ruleMargin"`
	CatalogRiskMargin   *float64 `json:"catalogRiskMargin"`
	CashMargin          *float64 `json:"cashMargin"`
	Enabled             bool     `json:"enabled"`
	AllowNegativeMargin bool     `json:"allowNegativeMargin"`
	ExecutionMode       string   `json:"executionMode"`
	CoverageMode        string   `json:"coverageMode"`
	MinimumCoverage     float64  `json:"minimumCoverage"`
	TargetWriteMode     string   `json:"targetWriteMode"`
	ProposalMode        string   `json:"proposalMode,omitempty"`
	ProposalStatus      string   `json:"proposalStatus,omitempty"`
	ProposalError       string   `json:"proposalError,omitempty"`
	AutomationEligible  *bool    `json:"automationEligible,omitempty"`
	AutomationReason    string   `json:"automationReason,omitempty"`
	LastRunAt           string   `json:"lastRunAt"`
}

type consoleBalance struct {
	ID                string    `json:"id"`
	Supplier          string    `json:"supplier"`
	Balance           *float64  `json:"balance"`
	Currency          string    `json:"currency,omitempty"`
	SpentToday        *float64  `json:"spentToday"`
	DailyAverage      *float64  `json:"dailyAverage"`
	RunwayHours       *float64  `json:"runwayHours"`
	ThresholdHours    float64   `json:"thresholdHours"`
	ForecastAt        string    `json:"forecastAt"`
	Status            string    `json:"status"`
	Samples           []float64 `json:"samples"`
	RawValue          string    `json:"rawValue,omitempty"`
	RawUnit           string    `json:"rawUnit,omitempty"`
	SourceField       string    `json:"sourceField,omitempty"`
	Divisor           string    `json:"divisor,omitempty"`
	Formula           string    `json:"formula,omitempty"`
	ObservedAt        string    `json:"observedAt,omitempty"`
	HistoricalSpend   *float64  `json:"historicalSpend,omitempty"`
	RequestCount      *int64    `json:"requestCount,omitempty"`
	DailyRequestCount *int64    `json:"dailyRequestCount,omitempty"`
	UsageSource       string    `json:"usageSource,omitempty"`
	UsageObservedAt   string    `json:"usageObservedAt,omitempty"`
	RawBalance        string    `json:"rawBalance,omitempty"`
	RawDailySpend     string    `json:"rawDailySpend,omitempty"`
	RawCurrency       string    `json:"rawCurrency,omitempty"`
	CashFactor        string    `json:"cashFactor,omitempty"`
	ConversionFormula string    `json:"conversionFormula,omitempty"`
}

type consoleDiscovery struct {
	ID             string   `json:"id"`
	Supplier       string   `json:"supplier"`
	UpstreamGroup  string   `json:"upstreamGroup"`
	UpstreamRate   *float64 `json:"upstreamRate"`
	ModelCount     int      `json:"modelCount"`
	FeaturedModels []string `json:"featuredModels"`
	FoundAt        string   `json:"foundAt"`
	Recommendation string   `json:"recommendation"`
	Status         string   `json:"status"`
	Classification string   `json:"classification"`
}

type consoleProposal struct {
	ID               string                    `json:"id"`
	Title            string                    `json:"title"`
	Target           string                    `json:"target"`
	Type             string                    `json:"type"`
	Status           string                    `json:"status"`
	CreatedAt        string                    `json:"createdAt"`
	CreatedBy        string                    `json:"createdBy"`
	Reason           string                    `json:"reason"`
	Before           string                    `json:"before"`
	After            string                    `json:"after"`
	Impact           string                    `json:"impact"`
	Readback         string                    `json:"readback,omitempty"`
	Error            string                    `json:"error,omitempty"`
	PricingMode      string                    `json:"pricingMode,omitempty"`
	CurrentRevision  string                    `json:"currentRevision,omitempty"`
	ProposedRevision string                    `json:"proposedRevision,omitempty"`
	ChangedModels    int                       `json:"changedModels,omitempty"`
	MinimumMargin    string                    `json:"minimumMargin,omitempty"`
	ModelChanges     []consoleModelPriceChange `json:"modelChanges,omitempty"`
}

type consoleModelPriceChange struct {
	Model         string                             `json:"model"`
	BillingMode   string                             `json:"billingMode"`
	BillingUnit   string                             `json:"billingUnit"`
	Currency      string                             `json:"currency,omitempty"`
	MinimumMargin string                             `json:"minimumMargin,omitempty"`
	Dimensions    []consoleModelPriceDimensionChange `json:"dimensions"`
}

type consoleModelPriceDimensionChange struct {
	Dimension        string `json:"dimension"`
	Currency         string `json:"currency,omitempty"`
	SourceCashCost   string `json:"sourceCashCost,omitempty"`
	CurrentSale      string `json:"currentSale,omitempty"`
	ProposedSale     string `json:"proposedSale,omitempty"`
	MarginRate       string `json:"marginRate,omitempty"`
	CurrentInherited bool   `json:"currentInherited,omitempty"`
}

type consoleConnector struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Protocol      string   `json:"protocol"`
	Version       string   `json:"version"`
	SupplierCount int      `json:"supplierCount"`
	Status        string   `json:"status"`
	Capabilities  []string `json:"capabilities"`
	UpdatedAt     string   `json:"updatedAt"`
}

type consoleCredential struct {
	ID                  string                         `json:"id"`
	Supplier            string                         `json:"supplier"`
	Method              string                         `json:"method"`
	Status              string                         `json:"status"`
	ExpiresAt           *string                        `json:"expiresAt"`
	LastRefreshedAt     string                         `json:"lastRefreshedAt"`
	AutoRefresh         bool                           `json:"autoRefresh"`
	Renewal             domain.CredentialRenewalStatus `json:"renewal"`
	RecollectionPending bool                           `json:"recollectionPending"`
	BrowserCustody      *domain.BrowserCustodyStatus   `json:"browserCustody,omitempty"`
}

type consoleActivity struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	RawKind  string            `json:"rawKind"`
	Title    string            `json:"title"`
	Detail   string            `json:"detail"`
	At       string            `json:"at"`
	Status   string            `json:"status"`
	Severity string            `json:"severity"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type policyProposalView struct {
	Fresh             domain.ChangeProposal
	HasFresh          bool
	TrustedReadback   string
	TrustedReadbackAt time.Time
}

const staleProposalProjectionMessage = "规则或上游证据已变化，请使用实时建议重新生成变更"

type consoleBootstrapScope string

const (
	bootstrapScopeFull        consoleBootstrapScope = "full"
	bootstrapScopeOverview    consoleBootstrapScope = "overview"
	bootstrapScopePricing     consoleBootstrapScope = "pricing"
	bootstrapScopeChanges     consoleBootstrapScope = "changes"
	bootstrapScopeConnectors  consoleBootstrapScope = "connectors"
	bootstrapScopeDiscoveries consoleBootstrapScope = "discoveries"
)

func parseConsoleBootstrapScope(raw string) (consoleBootstrapScope, error) {
	scope := consoleBootstrapScope(strings.ToLower(strings.TrimSpace(raw)))
	if scope == "" {
		return bootstrapScopeFull, nil
	}
	switch scope {
	case bootstrapScopeFull, bootstrapScopeOverview, bootstrapScopePricing, bootstrapScopeChanges,
		bootstrapScopeConnectors, bootstrapScopeDiscoveries:
		return scope, nil
	default:
		return "", fmt.Errorf("不支持的控制台数据范围 %q", raw)
	}
}

func (scope consoleBootstrapScope) includesPricing() bool {
	return scope == bootstrapScopeFull || scope == bootstrapScopePricing
}

func (scope consoleBootstrapScope) includesChanges() bool {
	return scope == bootstrapScopeFull || scope == bootstrapScopeChanges
}

func (scope consoleBootstrapScope) includesConnectors() bool {
	return scope == bootstrapScopeFull || scope == bootstrapScopeConnectors
}

func (scope consoleBootstrapScope) includesDiscoveries() bool {
	return scope == bootstrapScopeFull || scope == bootstrapScopeDiscoveries
}

func (scope consoleBootstrapScope) needsTargetSnapshots() bool {
	return scope.includesPricing() || scope.includesChanges()
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scope, err := parseConsoleBootstrapScope(r.URL.Query().Get("scope"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	suppliers, err := s.store.ListSuppliers(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	policies, _ := s.store.ListPolicies(ctx)
	proposals, _ := s.store.ListProposals(ctx, "", 200)
	var credentials []domain.Credential
	var discoveries []domain.DiscoveryItem
	var specs []domain.ConnectorSpec
	var activities []domain.Activity
	if scope.includesConnectors() {
		credentials, _ = s.store.ListCredentials(ctx)
		specs, _ = s.store.ListConnectorSpecs(ctx)
	}
	if scope.includesDiscoveries() {
		discoveries, _ = s.store.ListDiscoveries(ctx, false, 200)
	}
	if scope.includesChanges() {
		activities, _ = s.store.RecentActivity(ctx, 20)
	}
	runway, _ := forecast.All(ctx, s.store, s.runwayHours)

	supplierByID := make(map[int64]domain.Supplier, len(suppliers))
	snapshots := make(map[int64]domain.CatalogSnapshot, len(suppliers))
	for _, supplier := range suppliers {
		supplierByID[supplier.ID] = supplier
		if snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplier.ID); snapshotErr == nil {
			snapshots[supplier.ID] = snapshot
		}
	}
	targetsList, _ := s.store.ListTargets(ctx)
	targetNames := make(map[int64]string, len(targetsList))
	targetWriteModes := make(map[int64]string, len(targetsList))
	for _, target := range targetsList {
		targetNames[target.ID] = target.Name
		targetWriteModes[target.ID] = target.WriteMode
	}
	targetSnapshots := make(map[int64]domain.TargetSnapshot)
	targetSnapshotErrors := make(map[int64]string)
	if scope.needsTargetSnapshots() {
		targetSnapshots, targetSnapshotErrors = s.loadBootstrapTargetSnapshots(ctx, policies, targetsList)
	}
	proposals = projectProposalLifecycles(proposals, policies, snapshots)
	proposalViews := buildPolicyProposalViews(proposals, policies, snapshots)
	freshProposals := make(map[int64]domain.ChangeProposal)
	for policyID, view := range proposalViews {
		if view.HasFresh {
			freshProposals[policyID] = view.Fresh
		}
	}

	now := time.Now().UTC()
	browserCustodyBySupplier := make(map[int64]domain.BrowserCustodyStatus, len(suppliers))
	for _, supplier := range suppliers {
		if binding, custodyErr := s.store.GetBrowserCustodyForSupplier(r.Context(), supplier.ID, now); custodyErr == nil {
			browserCustodyBySupplier[supplier.ID] = s.browserCustodyStatus(r.Context(), binding, now)
		}
	}
	output := consoleBootstrap{AutomationEnabled: s.automationEnabled.Load(), BackgroundWorkersEnabled: s.backgroundWorkersEnabled,
		RemoteWritesEnabled: s.remoteWrites.Enabled(),
		GeneratedAt:         now.Format(time.RFC3339),
		Suppliers:           []consoleSupplier{}, WatchItems: []consoleWatch{}, Policies: []consolePolicy{}, Balances: []consoleBalance{},
		Discoveries: []consoleDiscovery{}, Proposals: []consoleProposal{}, Connectors: []consoleConnector{},
		Credentials: []consoleCredential{}, Activities: []consoleActivity{}}
	output.Runtime = s.projectConsoleRuntime(now, targetsList)
	runwayBySupplier := make(map[int64]domain.RunwayForecast, len(runway))
	supplierAuthStatuses := make(map[int64]string, len(suppliers))
	for _, item := range runway {
		runwayBySupplier[item.SupplierID] = item
	}
	output.Metrics.SupplierCount = len(suppliers)
	for _, supplier := range suppliers {
		snapshot := snapshots[supplier.ID]
		status := mapSupplierStatus(supplier.LastStatus)
		item := consoleSupplier{ID: fmt.Sprint(supplier.ID), Name: supplier.Name, BaseURL: supplier.BaseURL, Enabled: supplier.Enabled,
			Kind: connectorName(supplier.Adapter), Status: status, AuthStatus: "missing", CollectInterval: supplier.PollIntervalSeconds,
			RechargeValueFactor: supplier.RechargeValueFactor,
			GroupCount:          len(snapshot.Groups), ModelCount: len(snapshot.Models), Focused: len(supplier.FocusGroups)+len(supplier.FocusModels) > 0,
			Note: supplier.LastError, LastErrorClass: supplier.LastErrorClass, LastFailureDurationMS: supplier.LastFailureDuration}
		if supplier.CredentialID != nil {
			credentialID := strconv.FormatInt(*supplier.CredentialID, 10)
			item.CredentialID = &credentialID
			item.AuthStatus = "pending"
			if capabilities, capabilityErr := s.store.ListAuthCapabilities(ctx, supplier.ID); capabilityErr == nil {
				item.AuthStatus = authStatusFromCapabilities(capabilities)
				supplierAuthStatuses[supplier.ID] = item.AuthStatus
				item.AuthAttentionReasons = authAttentionReasonsFromCapabilities(capabilities)
				item.AuthAttention = len(item.AuthAttentionReasons) > 0
			}
		}
		if custody, ok := browserCustodyBySupplier[supplier.ID]; ok && browserCustodyCanRecover(&custody) {
			item.AuthStatus = "valid"
			item.AuthAttention = false
			item.AuthAttentionReasons = nil
			if isAuthFailureClass(item.LastErrorClass) && (item.Status == "warning" || item.Status == "error") {
				item.Status = "healthy"
			}
		}
		if item.Status == "healthy" {
			output.Metrics.HealthySuppliers++
		} else if item.Status != "unknown" {
			output.Metrics.ActiveAlerts++
		}
		if supplier.LastAttemptAt != nil {
			item.LastAttemptAt = supplier.LastAttemptAt.Format(time.RFC3339)
		}
		if supplier.LastSuccessAt != nil {
			item.LastSuccessAt = supplier.LastSuccessAt.Format(time.RFC3339)
		}
		if supplier.LastCollectedAt != nil {
			item.LastCollectedAt = supplier.LastCollectedAt.Format(time.RFC3339)
		}
		if supplier.NextCollectionAt != nil {
			item.NextCollectedAt = supplier.NextCollectionAt.Format(time.RFC3339)
		}
		if balance, exists := runwayBySupplier[supplier.ID]; exists {
			if raw := firstKnown(balance.BalanceAmount); raw != "" {
				value := floatValue(raw)
				item.Balance = &value
				item.BalanceCurrency = balance.Currency
			}
		}
		item.LatencyMS = supplier.LastSuccessDuration
		output.Suppliers = append(output.Suppliers, item)
		appendWatchItems(&output, supplier, snapshot, policies, freshProposals)
	}

	if !scope.includesPricing() {
		for _, policy := range policies {
			if policy.Enabled && policy.ExecutionMode == "auto" {
				output.Metrics.AutomatedPolicies++
			}
		}
	} else {
		for _, policy := range policies {
			supplier := supplierByID[policy.SupplierID]
			snapshot := snapshots[policy.SupplierID]
			upstream := groupRatio(snapshot, policy.SourceGroup)
			var current, suggested, ruleBasisRate, equivalentCostRate, ruleMargin, catalogRiskMargin *float64
			var proposalMode, proposalStatus, proposalError, automationReason string
			var automationEligible *bool
			lastRun := ""
			view := proposalViews[policy.ID]
			currentRaw, currentStatus := policyCurrentRatio(policy, targetSnapshots, view)
			if view.HasFresh {
				proposal := view.Fresh
				if expected := strings.TrimSpace(proposal.ExpectedOld); currentRaw == "" && currentStatus == "unavailable" && expected != "" {
					currentRaw = expected
					currentStatus = "proposal_expectation"
				}
				if currentRaw == "" && currentStatus == "unavailable" &&
					(proposal.Status == "applied" || proposal.Status == "rolled_back") && strings.TrimSpace(proposal.ReadbackValue) != "" {
					currentRaw = strings.TrimSpace(proposal.ReadbackValue)
					currentStatus = "trusted_readback"
				}
				if currentRaw != "" {
					value := floatValue(currentRaw)
					current = &value
				}
				liveSource := groupRatioString(snapshot, policy.SourceGroup)
				equivalentCostRaw := strings.TrimSpace(proposal.Calculation.EquivalentCostRatio)
				if equivalentCostRaw == "" && strings.TrimSpace(proposal.Calculation.CostRatio) != strings.TrimSpace(proposal.Calculation.SourceRatio) {
					equivalentCostRaw = strings.TrimSpace(proposal.Calculation.CostRatio)
				}
				if !strings.EqualFold(strings.TrimSpace(policy.SourceBasis), "group_ratio") {
					if equivalentCostRaw != "" {
						liveSource = equivalentCostRaw
					} else if strings.TrimSpace(proposal.Calculation.SourceRatio) != "" {
						liveSource = proposal.Calculation.SourceRatio
					}
				}
				liveCalculation, liveErr := pricing.Calculate(pricing.Input{SourceRatio: liveSource,
					CostRatio: liveSource, CurrentRatio: currentRaw, Policy: policy})
				if liveErr == nil {
					ruleBasisRate, suggested, ruleMargin = consoleCalculationValues(liveCalculation)
				} else {
					proposalError = joinConsoleReason(proposalError, "实时重算失败："+liveErr.Error())
				}
				if ruleMargin == nil {
					ruleMargin = ratioMarginPercent(proposal.Calculation.SourceRatio, proposal.Calculation.TargetRatio)
				}
				if equivalentCostRaw != "" {
					value := floatValue(equivalentCostRaw)
					equivalentCostRate = &value
					if suggested != nil && *suggested > 0 {
						marginValue := (*suggested - value) / *suggested * 100
						catalogRiskMargin = &marginValue
					}
				}
				value := proposal.Calculation.AutomationEligible
				automationEligible = &value
				automationReason = proposal.Calculation.AutomationReason
				proposalMode, proposalStatus = proposal.Mode, proposal.Status
				automationEligible, automationReason = normalizeNoChangeAutomationProjection(proposalStatus, automationEligible, automationReason)
				proposalError = joinConsoleReason(proposal.Error, proposalError)
				lastRun = proposal.UpdatedAt.Format(time.RFC3339)
			} else {
				if currentRaw != "" {
					value := floatValue(currentRaw)
					current = &value
				}
				sourceRatio := groupRatioString(snapshot, policy.SourceGroup)
				if strings.EqualFold(strings.TrimSpace(policy.SourceBasis), "group_ratio") {
					calculation, calcErr := pricing.Calculate(pricing.Input{SourceRatio: sourceRatio, CostRatio: sourceRatio, CurrentRatio: currentRaw, Policy: policy})
					if calcErr == nil {
						ruleBasisRate, suggested, ruleMargin = consoleCalculationValues(calculation)
						eligible := false
						automationEligible = &eligible
						automationReason = "最新分组倍率已到达；后台正在生成带目标站读回的完整提案，当前列表公式只用于预览，不会单独触发写入"
					}
				} else {
					automationReason = "规则选择逐模型等价成本，等待完整的同币种模型价格证据后才会生成建议；不会退回分组倍率猜算"
					eligible := false
					automationEligible = &eligible
				}
				if !snapshot.Evidence.ObservedAt.IsZero() {
					lastRun = snapshot.Evidence.ObservedAt.Format(time.RFC3339)
				}
			}
			hasPriceChange := consolePolicyHasPriceChange(current, suggested)
			if hasPriceChange && !s.automationEnabled.Load() && policy.ExecutionMode == "auto" {
				automationReason = joinConsoleReason(automationReason, "已实时重算；全局无人值守写价开关关闭，等待开启后才会修改本站")
			}
			if currentStatus == "unavailable" && targetSnapshotErrors[policy.TargetSiteID] != "" {
				automationReason = joinConsoleReason(automationReason, "当前目标倍率实时读回失败："+targetSnapshotErrors[policy.TargetSiteID])
			}
			output.Policies = append(output.Policies, consolePolicy{ID: fmt.Sprint(policy.ID), Name: policy.Name,
				Supplier: supplier.Name, UpstreamGroup: policy.SourceGroup, TargetGroup: policy.TargetGroup, Mode: policy.RuleType,
				Parameter: floatValue(policy.Value), UpstreamRate: upstream, SuggestedRate: suggested, CurrentRate: current,
				CurrentRateStatus: currentStatus, SourceBasis: policy.SourceBasis, RuleBasisRate: ruleBasisRate, EquivalentCostRate: equivalentCostRate,
				RuleMargin: ruleMargin, CatalogRiskMargin: catalogRiskMargin, CashMargin: nil,
				Enabled: policy.Enabled, AllowNegativeMargin: policy.AllowNegativeProfit,
				ExecutionMode: policy.ExecutionMode, CoverageMode: policy.CoverageMode,
				MinimumCoverage: floatValue(policy.MinimumCoverage), TargetWriteMode: targetWriteModes[policy.TargetSiteID],
				ProposalMode: proposalMode, ProposalStatus: proposalStatus, ProposalError: proposalError,
				AutomationEligible: automationEligible, AutomationReason: automationReason, LastRunAt: lastRun})
			if policy.Enabled && policy.ExecutionMode == "auto" {
				output.Metrics.AutomatedPolicies++
			}
		}
	}

	for _, item := range runway {
		var hours *float64
		forecastAt := ""
		status := "unknown"
		if item.RunwayHours != nil {
			value := floatValue(*item.RunwayHours)
			hours = &value
			forecastAt = detailedRunwayForecastAt(item.ObservedAt, value)
			status = "healthy"
		}
		if item.NeedsAttention {
			status = "warning"
			output.Metrics.ActiveAlerts++
		}
		var balanceValue *float64
		if strings.TrimSpace(item.BalanceAmount) != "" {
			value := floatValue(item.BalanceAmount)
			balanceValue = &value
		}
		var spentToday *float64
		var dailyAverage *float64
		if strings.TrimSpace(item.DailySpend) != "" {
			value := floatValue(item.DailySpend)
			dailyAverage = &value
			spentToday = &value
		}
		row := consoleBalance{ID: fmt.Sprint(item.SupplierID), Supplier: item.SupplierName,
			Balance: balanceValue, Currency: item.Currency, SpentToday: spentToday, DailyAverage: dailyAverage,
			RunwayHours: hours, ThresholdHours: s.runwayHours.InexactFloat64(), ForecastAt: forecastAt,
			Status: status, Samples: balanceSamples(dailyAverage), RawBalance: item.RawBalanceAmount,
			RawDailySpend: item.RawDailySpend, RawCurrency: item.RawCurrency, CashFactor: item.CashFactor,
			ConversionFormula: item.ConversionFormula}
		if raw, rawErr := s.store.LatestBalance(ctx, item.SupplierID); rawErr == nil {
			row.RawValue = raw.RawValue
			row.RawUnit = raw.RawUnit
			row.SourceField = raw.SourceField
			row.Divisor = raw.NormalizationDivisor
			row.Formula = raw.NormalizationFormula
			row.ObservedAt = raw.ObservedAt.Format(time.RFC3339)
			if strings.TrimSpace(raw.HistoricalSpend) != "" {
				if converted, ok := forecast.ConvertCash(raw.HistoricalSpend, item.CashFactor); ok {
					value := floatValue(converted)
					row.HistoricalSpend = &value
				}
			}
			row.RequestCount = raw.RequestCount
			row.DailyRequestCount = raw.DailyRequestCount
			row.UsageSource = raw.UsageSource
			if raw.UsageObservedAt != nil {
				row.UsageObservedAt = raw.UsageObservedAt.Format(time.RFC3339)
			}
		}
		output.Balances = append(output.Balances, row)
	}
	for _, discovery := range discoveries {
		if discovery.Baseline {
			continue
		}
		status := "new"
		if discovery.AcknowledgedAt != nil {
			status = "reviewing"
		}
		discoveryRate := discovery.Ratio
		if supplier, ok := supplierByID[discovery.SupplierID]; ok {
			if effective, effectiveErr := ratios.EffectiveGroupRatio(discovery.Ratio, supplier.RechargeValueFactor); effectiveErr == nil {
				discoveryRate = effective
			}
		}
		output.Discoveries = append(output.Discoveries, consoleDiscovery{ID: discoveryID(discovery), Supplier: discovery.SupplierName,
			UpstreamGroup: discovery.Name, UpstreamRate: optionalFloatValue(discoveryRate), ModelCount: discovery.ModelCount,
			FeaturedModels: []string{}, FoundAt: discovery.FirstSeenAt.Format(time.RFC3339),
			Recommendation: "已发现上游分组，可在核对模型和利润后确认一键上架", Status: status, Classification: "new"})
	}
	for _, proposal := range proposals {
		if scope.includesChanges() {
			output.Proposals = append(output.Proposals, buildConsoleProposal(proposal, targetNames[proposal.TargetSiteID]))
		}
		if proposal.Status == "pending" {
			output.Metrics.PendingChanges++
		}
	}
	if scope.includesConnectors() {
		output.Connectors = append(output.Connectors,
			consoleConnector{ID: "builtin-newapi", Name: "NewAPI 原生连接器", Kind: "builtin", Protocol: "NewAPI", Version: "v1",
				SupplierCount: countAdapter(suppliers, "newapi"), Status: connectorHealth(suppliers, "newapi"), Capabilities: []string{"模型目录", "分组倍率", "登录价格", "余额"}, UpdatedAt: output.GeneratedAt},
			consoleConnector{ID: "builtin-sub2api", Name: "Sub2API 原生连接器", Kind: "builtin", Protocol: "Sub2API", Version: "v0.1.161+",
				SupplierCount: countAdapter(suppliers, "sub2api"), Status: connectorHealth(suppliers, "sub2api"), Capabilities: []string{"账号池", "分组倍率", "消耗", "额度窗口"}, UpdatedAt: output.GeneratedAt})
		for _, spec := range specs {
			status := "unknown"
			if !spec.Enabled {
				status = "warning"
			} else if health := connectorSpecHealth(suppliers, fmt.Sprint(spec.ID)); health != "unknown" {
				status = health
			}
			output.Connectors = append(output.Connectors, consoleConnector{ID: fmt.Sprint(spec.ID), Name: spec.Name, Kind: "json",
				Protocol: "Custom", Version: spec.SchemaVersion, Status: status, Capabilities: []string{"声明式采集", "受限 JSONPath"},
				SupplierCount: countConnectorSpecUsers(suppliers, fmt.Sprint(spec.ID)), UpdatedAt: spec.UpdatedAt.Format(time.RFC3339)})
		}
	}
	for _, credential := range credentials {
		supplierName := credential.SiteURL
		recollectionPending := false
		coreAuthStatus := ""
		var browserCustody *domain.BrowserCustodyStatus
		for _, supplier := range suppliers {
			if supplier.CredentialID != nil && *supplier.CredentialID == credential.ID {
				supplierName = supplier.Name
				if authStatus := supplierAuthStatuses[supplier.ID]; authStatus == "valid" || (authStatus == "invalid" && coreAuthStatus == "") {
					coreAuthStatus = authStatus
				}
				if credential.Renewal.LastSuccessAt != nil &&
					(supplier.LastAttemptAt == nil || supplier.LastAttemptAt.Before(*credential.Renewal.LastSuccessAt)) {
					recollectionPending = true
				}
				if status, ok := browserCustodyBySupplier[supplier.ID]; ok {
					statusCopy := status
					browserCustody = &statusCopy
				}
			}
		}
		var expires *string
		if credential.ExpiresAt != nil {
			value := credential.ExpiresAt.Format(time.RFC3339)
			expires = &value
		}
		managedRenewalHealthy := credentialRenewalCanRecover(credential.Renewal) || browserCustodyCanRecover(browserCustody)
		automaticRecoveryPending := browserCustody != nil && browserCustody.RecoveryPending
		status := projectedCredentialStatus(credential, coreAuthStatus, managedRenewalHealthy,
			automaticRecoveryPending, now, s.credentialRefreshAhead)
		if status != "valid" {
			output.Metrics.ActiveAlerts++
		}
		output.Credentials = append(output.Credentials, consoleCredential{ID: fmt.Sprint(credential.ID), Supplier: supplierName,
			Method: credentialMethod(credential.Kind), Status: status, ExpiresAt: expires,
			LastRefreshedAt: credential.UpdatedAt.Format(time.RFC3339), AutoRefresh: credential.Renewal.Enabled,
			Renewal: credential.Renewal, RecollectionPending: recollectionPending, BrowserCustody: browserCustody})
	}
	for _, activity := range activities {
		output.Activities = append(output.Activities, projectConsoleActivity(activity))
	}
	if scope != bootstrapScopeFull {
		output.WatchItems, output.WatchSettings, output.WatchCandidateCount = s.refreshOverviewWatchCache(supplierByID, snapshots, policies, freshProposals)
	} else if watchItems, settings, candidates, watchErr := s.buildConsoleWatchItems(r.Context(), suppliers, snapshots, policies, freshProposals); watchErr == nil {
		output.WatchSettings = settings
		output.WatchCandidateCount = candidates
		if len(watchItems) > settings.OverviewLimit {
			watchItems = watchItems[:settings.OverviewLimit]
		}
		output.WatchItems = watchItems
		s.storeOverviewWatchCache(watchItems, settings, candidates)
	}
	writeJSON(w, http.StatusOK, output)
}

func (s *Server) projectConsoleRuntime(now time.Time, targets []domain.TargetSite) consoleRuntime {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	databaseBytes, _ := s.store.DatabaseStorageBytes()
	startedAt := s.startedAt
	if startedAt.IsZero() || startedAt.After(now) {
		startedAt = now
	}
	result := consoleRuntime{StartedAt: startedAt.Format(time.RFC3339), UptimeSeconds: int64(now.Sub(startedAt).Seconds()),
		DatabaseBytes: databaseBytes, ProcessMemoryBytes: memory.Sys, Goroutines: runtime.NumGoroutine(),
		Targets: []consoleRuntimeTarget{}, GeneratedAt: now.Format(time.RFC3339)}
	for _, target := range targets {
		if !strings.EqualFold(strings.TrimSpace(target.Adapter), "newapi") {
			continue
		}
		updatedAt := ""
		if !target.UpdatedAt.IsZero() {
			updatedAt = target.UpdatedAt.Format(time.RFC3339)
		}
		result.Targets = append(result.Targets, consoleRuntimeTarget{ID: fmt.Sprint(target.ID), Name: target.Name,
			Enabled: target.Enabled, Status: mapSupplierStatus(target.LastStatus), UpdatedAt: updatedAt})
	}
	return result
}

func (s *Server) storeOverviewWatchCache(items []consoleWatch, settings store.WatchSettings, candidates int) {
	s.overviewWatchMu.Lock()
	s.overviewWatchItems = append([]consoleWatch(nil), items...)
	s.overviewWatchSettings = settings
	s.overviewWatchCandidates = candidates
	s.overviewWatchMu.Unlock()
}

func (s *Server) refreshOverviewWatchCache(suppliers map[int64]domain.Supplier, snapshots map[int64]domain.CatalogSnapshot,
	policies []domain.PricingPolicy, proposals map[int64]domain.ChangeProposal) ([]consoleWatch, store.WatchSettings, int) {
	s.overviewWatchMu.RLock()
	cached := append([]consoleWatch(nil), s.overviewWatchItems...)
	settings := s.overviewWatchSettings
	candidates := s.overviewWatchCandidates
	s.overviewWatchMu.RUnlock()
	if settings.OverviewLimit == 0 {
		settings = store.DefaultWatchSettings()
	}
	result := make([]consoleWatch, 0, len(cached))
	for _, previous := range cached {
		supplier, ok := suppliers[previous.SupplierID]
		if !ok {
			continue
		}
		candidate := &watchCandidate{supplier: supplier, snapshot: snapshots[previous.SupplierID], scopeType: previous.ScopeType,
			groupKey: previous.GroupKey, modelKey: previous.ModelKey, manual: previous.Manual,
			sourcePricing: previous.SourcePricing, sourceMirror: previous.SourceMirror, sourceChange: previous.SourceChange}
		current := s.projectConsoleWatch(candidate, policies, proposals, "", previous.EvidenceHash)
		current.ID, current.DisplayName, current.Note = previous.ID, previous.DisplayName, previous.Note
		current.Manual, current.SourcePricing, current.SourceMirror, current.SourceChange = previous.Manual, previous.SourcePricing, previous.SourceMirror, previous.SourceChange
		current.EvidenceHash, current.Delta24H = previous.EvidenceHash, previous.Delta24H
		result = append(result, current)
	}
	return result, settings, candidates
}

func buildConsoleProposal(proposal domain.ChangeProposal, targetName string) consoleProposal {
	result := consoleProposal{ID: fmt.Sprint(proposal.ID), Title: proposal.TargetGroup + " 倍率调整",
		Target: targetName, Type: "price", Status: proposal.Status, CreatedAt: proposal.CreatedAt.Format(time.RFC3339),
		CreatedBy: "规则引擎", Reason: proposal.Calculation.Formula, Before: displayConsoleValue(proposal.ExpectedOld),
		After: displayConsoleValue(proposal.ProposedValue), Impact: "贡献利润 " + displayConsoleValue(proposal.Calculation.ContributionProfit),
		Readback: optionalConsoleValue(proposal.ReadbackValue), Error: proposal.Error}
	plan := proposal.GroupModelPricing
	if plan.Mode != "atomic_contract" {
		return result
	}
	result.Title = proposal.TargetGroup + " 逐模型价格同步"
	result.PricingMode = "group_model_contract"
	result.CurrentRevision = plan.ExpectedRevision
	result.ProposedRevision = plan.DesiredRevision
	result.ChangedModels = len(plan.Models)
	result.MinimumMargin = plan.MinimumMarginRate
	result.Before = "合同 " + shortConsoleRevision(plan.ExpectedRevision)
	result.After = "合同 " + shortConsoleRevision(plan.DesiredRevision)
	result.Impact = fmt.Sprintf("%d 个模型 · 最低真实毛利 %s", len(plan.Models), consolePercent(plan.MinimumMarginRate))
	result.Reason = joinConsoleReason("按上游逐模型现金成本和计费单位重新计算", proposal.Calculation.Formula)
	readbackRevision := firstNonEmptyConsoleValue(proposal.GroupModelPricingReceipt.AppliedRevision, proposal.ReadbackValue)
	result.Readback = shortConsoleRevisionOptional(readbackRevision)

	expectedModels := plan.ExpectedContract.Groups[plan.TargetGroup]
	groupRatio := strings.TrimSpace(plan.TargetGroupRatio)
	if groupRatio == "" {
		groupRatio = strings.TrimSpace(proposal.ExpectedOld)
	}
	if groupRatio == "" {
		groupRatio = "1"
	}
	result.ModelChanges = make([]consoleModelPriceChange, 0, len(plan.Models))
	for _, model := range plan.Models {
		currentDimensions := map[string]string{}
		currentSetting, hasCurrentSetting := expectedModels[model.Model]
		if hasCurrentSetting {
			if resolved, err := listing.EffectiveContractDimensions(currentSetting, groupRatio); err == nil {
				currentDimensions = resolved
			}
		}
		entry := consoleModelPriceChange{Model: model.Model, BillingMode: model.BillingMode, BillingUnit: model.BillingUnit,
			Currency: model.Currency, MinimumMargin: model.MinimumMarginRate,
			Dimensions: make([]consoleModelPriceDimensionChange, 0, len(model.Dimensions))}
		for _, dimension := range model.Dimensions {
			currentSale, hasCurrentSale := currentDimensions[dimension.Dimension]
			entry.Dimensions = append(entry.Dimensions, consoleModelPriceDimensionChange{Dimension: dimension.Dimension,
				Currency: model.Currency, SourceCashCost: dimension.RiskAdjustedCashCost, CurrentSale: currentSale,
				ProposedSale: dimension.EffectiveSale, MarginRate: dimension.MarginRate,
				CurrentInherited: !hasCurrentSetting || !hasCurrentSale})
		}
		result.ModelChanges = append(result.ModelChanges, entry)
	}
	return result
}

func shortConsoleRevision(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "无"
	}
	if len(trimmed) <= 12 {
		return trimmed
	}
	return trimmed[:12] + "…"
}

func shortConsoleRevisionOptional(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return shortConsoleRevision(value)
}

func consolePercent(value string) string {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return displayConsoleValue(value)
	}
	return parsed.Mul(decimal.NewFromInt(100)).StringFixed(2) + "%"
}

func firstNonEmptyConsoleValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func consoleActivityMetadata(metadata map[string]string) map[string]string {
	allowed := []string{"diagnostic_version", "diagnostic_category", "policy_id", "policy_name", "supplier_id", "supplier_name",
		"source_group", "source_group_name", "source_group_id", "source_model_pattern", "matched_model_count", "matched_model_samples",
		"target_site_id", "target_site_name", "target_group", "target_group_exists", "target_catalog_status",
		"target_catalog_model_count", "missing_target_models", "comparable_dimensions", "total_dimensions", "suggested_action", "repeat_count",
		"proposal_id", "readback_revision", "changed_models", "minimum_margin", "total", "created", "updated", "unchanged", "failed",
		"first_seen_at", "last_seen_at"}
	result := make(map[string]string)
	for _, key := range allowed {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

type bootstrapTargetSnapshotResult struct {
	targetID int64
	snapshot domain.TargetSnapshot
	err      error
}

func (s *Server) loadBootstrapTargetSnapshots(ctx context.Context, policies []domain.PricingPolicy,
	targetsList []domain.TargetSite) (map[int64]domain.TargetSnapshot, map[int64]string) {
	snapshots := make(map[int64]domain.TargetSnapshot)
	errorsByTarget := make(map[int64]string)
	if s.targets == nil {
		return snapshots, errorsByTarget
	}

	referenced := make(map[int64]struct{}, len(policies))
	for _, policy := range policies {
		referenced[policy.TargetSiteID] = struct{}{}
	}
	jobs := make([]domain.TargetSite, 0, len(referenced))
	for _, target := range targetsList {
		if _, needed := referenced[target.ID]; !needed {
			continue
		}
		if !target.Enabled {
			errorsByTarget[target.ID] = "目标站已停用"
			continue
		}
		jobs = append(jobs, target)
	}
	if len(jobs) == 0 {
		return snapshots, errorsByTarget
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	jobQueue := make(chan domain.TargetSite, len(jobs))
	results := make(chan bootstrapTargetSnapshotResult, len(jobs))
	for _, target := range jobs {
		jobQueue <- target
	}
	close(jobQueue)
	workers := len(jobs)
	if workers > 4 {
		workers = 4
	}
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for target := range jobQueue {
				adapter, err := s.targets.ForTarget(target)
				if err != nil {
					results <- bootstrapTargetSnapshotResult{targetID: target.ID, err: err}
					continue
				}
				snapshot, err := adapter.Snapshot(readCtx, target)
				results <- bootstrapTargetSnapshotResult{targetID: target.ID, snapshot: snapshot, err: err}
			}
		}()
	}
	wait.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			errorsByTarget[result.targetID] = result.err.Error()
			continue
		}
		snapshots[result.targetID] = result.snapshot
	}
	return snapshots, errorsByTarget
}

func policyCurrentRatio(policy domain.PricingPolicy, targetSnapshots map[int64]domain.TargetSnapshot,
	view policyProposalView) (string, string) {
	if snapshot, ok := targetSnapshots[policy.TargetSiteID]; ok {
		if value, exists := snapshot.GroupRatio[policy.TargetGroup]; exists && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), "live"
		}
		return "", "missing_group"
	}
	if value := strings.TrimSpace(view.TrustedReadback); value != "" {
		return value, "trusted_readback"
	}
	return "", "unavailable"
}

func consoleCalculationValues(calculation domain.PriceCalculation) (basis, suggested, margin *float64) {
	basisValue := floatValue(calculation.SourceRatio)
	suggestedValue := floatValue(calculation.TargetRatio)
	basis, suggested = &basisValue, &suggestedValue
	margin = ratioMarginPercent(calculation.SourceRatio, calculation.TargetRatio)
	return basis, suggested, margin
}

func ratioMarginPercent(basis, target string) *float64 {
	basisValue, basisErr := decimal.NewFromString(strings.TrimSpace(basis))
	targetValue, targetErr := decimal.NewFromString(strings.TrimSpace(target))
	if basisErr != nil || targetErr != nil || targetValue.LessThanOrEqual(decimal.Zero) {
		return nil
	}
	value, _ := targetValue.Sub(basisValue).Div(targetValue).Mul(decimal.NewFromInt(100)).Float64()
	return &value
}

func detailedRunwayForecastAt(observedAt time.Time, runwayHours float64) string {
	if runwayHours < 0 || runwayHours > 720 {
		return ""
	}
	return observedAt.Add(time.Duration(runwayHours * float64(time.Hour))).Format(time.RFC3339)
}

func balanceSamples(value *float64) []float64 {
	if value == nil {
		return []float64{}
	}
	return []float64{*value}
}

func appendWatchItems(output *consoleBootstrap, supplier domain.Supplier, snapshot domain.CatalogSnapshot, policies []domain.PricingPolicy, proposals map[int64]domain.ChangeProposal) {
	focusGroups := make(map[string]bool)
	for _, name := range supplier.FocusGroups {
		focusGroups[name] = true
	}
	focusModels := make(map[string]bool)
	for _, name := range supplier.FocusModels {
		focusModels[name] = true
	}
	for _, group := range snapshot.Groups {
		groupFocused := focusGroups[group.Name] || focusGroups[group.ExternalID]
		modelFocused := false
		for _, modelID := range group.ModelIDs {
			if focusModels[modelID] {
				modelFocused = true
				break
			}
		}
		if len(focusGroups)+len(focusModels) == 0 || (!groupFocused && !modelFocused) {
			continue
		}
		var local, margin *float64
		for _, policy := range policies {
			if policy.SupplierID != supplier.ID || (policy.SourceGroup != group.Name && policy.SourceGroup != group.ExternalID) {
				continue
			}
			if proposal, ok := proposals[policy.ID]; ok {
				local = optionalFloatValue(proposal.ProposedValue)
				margin = ratioMarginPercent(proposal.Calculation.SourceRatio, proposal.Calculation.TargetRatio)
			}
		}
		output.WatchItems = append(output.WatchItems, consoleWatch{ID: fmt.Sprintf("%d:%s", supplier.ID, group.ExternalID),
			Supplier: supplier.Name, Group: group.Name, Model: fmt.Sprintf("%d 个模型", len(group.ModelIDs)),
			UpstreamRate: optionalFloatValue(ratios.DecisionRatio(group)), LocalRate: local, GrossMargin: margin, Status: mapSupplierStatus(supplier.LastStatus),
			UpdatedAt: snapshot.Evidence.ObservedAt.Format(time.RFC3339)})
	}
}

func buildPolicyProposalViews(proposals []domain.ChangeProposal, policies []domain.PricingPolicy,
	snapshots map[int64]domain.CatalogSnapshot) map[int64]policyProposalView {
	policyByID := make(map[int64]domain.PricingPolicy, len(policies))
	for _, policy := range policies {
		policyByID[policy.ID] = policy
	}
	views := make(map[int64]policyProposalView, len(policies))
	for _, proposal := range proposals {
		view := views[proposal.PolicyID]
		policy, policyExists := policyByID[proposal.PolicyID]
		snapshot := snapshots[policy.SupplierID]
		if policyExists && proposalMatchesBootstrapInputs(proposal, policy, snapshot) &&
			(!view.HasFresh || proposalIsNewer(proposal, view.Fresh)) {
			view.Fresh = proposal
			view.HasFresh = true
		}
		if trustedProposalReadback(proposal) &&
			(view.TrustedReadbackAt.IsZero() || proposal.UpdatedAt.After(view.TrustedReadbackAt)) {
			view.TrustedReadback = strings.TrimSpace(proposal.ReadbackValue)
			view.TrustedReadbackAt = proposal.UpdatedAt
		}
		views[proposal.PolicyID] = view
	}
	return views
}

func projectProposalLifecycles(proposals []domain.ChangeProposal, policies []domain.PricingPolicy,
	snapshots map[int64]domain.CatalogSnapshot) []domain.ChangeProposal {
	policyByID := make(map[int64]domain.PricingPolicy, len(policies))
	for _, policy := range policies {
		policyByID[policy.ID] = policy
	}
	projected := make([]domain.ChangeProposal, len(proposals))
	copy(projected, proposals)
	for index := range projected {
		proposal := &projected[index]
		if proposal.Status != "pending" && proposal.Status != "failed" {
			continue
		}
		policy, policyExists := policyByID[proposal.PolicyID]
		var snapshot domain.CatalogSnapshot
		snapshotExists := false
		if policyExists {
			snapshot, snapshotExists = snapshots[policy.SupplierID]
		}
		if policyExists && snapshotExists && proposalMatchesBootstrapInputs(*proposal, policy, snapshot) {
			continue
		}
		proposal.Status = "stale"
		proposal.Error = staleProposalProjectionMessage
	}
	return projected
}

func (s *Server) projectProposalsAgainstCurrentInputs(ctx context.Context,
	proposals []domain.ChangeProposal) ([]domain.ChangeProposal, error) {
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	snapshots := make(map[int64]domain.CatalogSnapshot, len(policies))
	for _, policy := range policies {
		if _, exists := snapshots[policy.SupplierID]; exists {
			continue
		}
		if snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, policy.SupplierID); snapshotErr == nil {
			snapshots[policy.SupplierID] = snapshot
		}
	}
	return projectProposalLifecycles(proposals, policies, snapshots), nil
}

func proposalMatchesBootstrapInputs(proposal domain.ChangeProposal, policy domain.PricingPolicy, snapshot domain.CatalogSnapshot) bool {
	calculation := proposal.Calculation
	if calculation.InputFingerprint == "" || calculation.PolicyFingerprint == "" ||
		calculation.PolicyFingerprint != pricing.PolicyFingerprint(policy) ||
		calculation.SourceEvidenceHash == "" || calculation.SourceEvidenceHash != snapshot.Evidence.ResponseHash ||
		proposal.TargetSiteID != policy.TargetSiteID || proposal.TargetGroup != policy.TargetGroup {
		return false
	}
	for _, group := range snapshot.Groups {
		if group.ExternalID != policy.SourceGroup && group.Name != policy.SourceGroup {
			continue
		}
		factor, factorErr := ratios.NormalizeRechargeValueFactor(group.RechargeValueFactor)
		proposalFactor, proposalFactorErr := ratios.NormalizeRechargeValueFactor(calculation.RechargeValueFactor)
		if factorErr != nil || proposalFactorErr != nil || factor != proposalFactor {
			return false
		}
		raw := strings.TrimSpace(group.RawRatio)
		if raw == "" {
			raw = strings.TrimSpace(group.Ratio)
		}
		return decimalTextEqual(raw, calculation.RawSourceRatio)
	}
	return false
}

func decimalTextEqual(left, right string) bool {
	leftValue, leftErr := decimal.NewFromString(strings.TrimSpace(left))
	rightValue, rightErr := decimal.NewFromString(strings.TrimSpace(right))
	return leftErr == nil && rightErr == nil && leftValue.Equal(rightValue)
}

func proposalIsNewer(left, right domain.ChangeProposal) bool {
	return left.UpdatedAt.After(right.UpdatedAt) || (left.UpdatedAt.Equal(right.UpdatedAt) && left.ID > right.ID)
}

func trustedProposalReadback(proposal domain.ChangeProposal) bool {
	if strings.TrimSpace(proposal.ReadbackValue) == "" {
		return false
	}
	return proposal.Status == "applied" || proposal.Status == "rolled_back"
}

func joinConsoleReason(existing, addition string) string {
	if strings.TrimSpace(existing) == "" {
		return addition
	}
	return existing + "；" + addition
}

func connectorName(adapter string) string {
	value := strings.ToLower(adapter)
	if strings.Contains(value, "sub2") {
		return "Sub2API"
	}
	if strings.Contains(value, "json") {
		return "JSON Connector"
	}
	return "NewAPI"
}

func mapSupplierStatus(status string) string {
	switch status {
	case "healthy":
		return "healthy"
	case "degraded":
		return "warning"
	case "error":
		return "error"
	default:
		return "unknown"
	}
}

func floatValue(value string) float64 {
	parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return parsed
}

func optionalFloatValue(value string) *float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func firstNotEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "0"
}

func firstKnown(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func authStatusFromCapabilities(items []domain.AuthCapabilityEvidence) string {
	latestCore := latestCoreAuthCapabilities(items)
	coreSeen, coreFailure, coreVerified := len(latestCore) > 0, false, false
	anyVerified, anyFailure := false, false
	for _, item := range items {
		capability := strings.ToLower(strings.TrimSpace(item.Capability))
		if capability == "anonymous" || strings.HasPrefix(capability, "public") {
			continue
		}
		if item.Status == "verified" {
			anyVerified = true
		}
		if item.Status == "rejected" || item.Status == "expired" || item.Status == "expired_or_invalid" || item.Status == "denied" || item.Status == "failed" {
			anyFailure = true
		}
	}
	for _, item := range latestCore {
		if strings.EqualFold(strings.TrimSpace(item.Status), "verified") {
			coreVerified = true
		}
		if authCapabilityNeedsAttention(item) {
			coreFailure = true
		}
	}
	if coreVerified {
		return "valid"
	}
	if coreSeen && coreFailure {
		return "invalid"
	}
	if anyVerified {
		return "valid"
	}
	if anyFailure {
		return "invalid"
	}
	return "pending"
}

func authAttentionFromCapabilities(items []domain.AuthCapabilityEvidence) bool {
	return len(authAttentionReasonsFromCapabilities(items)) > 0
}

func authAttentionReasonsFromCapabilities(items []domain.AuthCapabilityEvidence) []string {
	latest := latestCoreAuthCapabilities(items)
	reasons := make([]string, 0, len(latest))
	for _, family := range []string{"identity", "catalog", "balance"} {
		item, ok := latest[family]
		if !ok || !authCapabilityNeedsAttention(item) {
			continue
		}
		switch family {
		case "identity":
			reasons = append(reasons, "身份")
		case "catalog":
			reasons = append(reasons, "私有目录")
		case "balance":
			reasons = append(reasons, "余额")
		}
	}
	return reasons
}

func latestCoreAuthCapabilities(items []domain.AuthCapabilityEvidence) map[string]domain.AuthCapabilityEvidence {
	latest := make(map[string]domain.AuthCapabilityEvidence)
	for _, item := range items {
		capability := strings.ToLower(strings.TrimSpace(item.Capability))
		if capability == "anonymous" || strings.HasPrefix(capability, "public") {
			continue
		}
		family := coreAuthCapabilityFamily(capability, strings.ToLower(strings.TrimSpace(item.EndpointType)))
		if family == "" {
			continue
		}
		current, found := latest[family]
		if !found || item.LastAttemptAt.After(current.LastAttemptAt) ||
			(item.LastAttemptAt.Equal(current.LastAttemptAt) && capabilityEvidenceTieBreak(item, current)) {
			latest[family] = item
		}
	}
	return latest
}

func capabilityEvidenceTieBreak(candidate, current domain.AuthCapabilityEvidence) bool {
	if (candidate.CredentialID != nil) != (current.CredentialID != nil) {
		return candidate.CredentialID != nil
	}
	return strings.EqualFold(strings.TrimSpace(candidate.Status), "verified") &&
		!strings.EqualFold(strings.TrimSpace(current.Status), "verified")
}

func authCapabilityNeedsAttention(item domain.AuthCapabilityEvidence) bool {
	status := strings.ToLower(strings.TrimSpace(item.Status))
	errorClass := strings.ToLower(strings.TrimSpace(item.ErrorClass))
	switch status {
	case "rejected", "expired", "expired_or_invalid":
		return true
	case "denied", "failed":
		return item.HTTPStatus == http.StatusUnauthorized || item.HTTPStatus == http.StatusForbidden ||
			errorClass == "auth_expired" || errorClass == "auth_missing" || errorClass == "permission_denied"
	default:
		return false
	}
}

func coreAuthCapability(capability, endpoint string) bool {
	return coreAuthCapabilityFamily(capability, endpoint) != ""
}

func coreAuthCapabilityFamily(capability, endpoint string) string {
	for _, value := range []string{endpoint, capability} {
		switch value {
		case "identity", "customer_identity":
			return "identity"
		case "catalog", "customer_catalog", "models", "customer_models":
			return "catalog"
		case "balance", "customer_balance":
			return "balance"
		}
	}
	return ""
}

func pointerValue(value *string) string {
	if value == nil {
		return "0"
	}
	return *value
}

func connectorHealth(suppliers []domain.Supplier, adapter string) string {
	status := "unknown"
	for _, supplier := range suppliers {
		if !strings.Contains(strings.ToLower(supplier.Adapter), adapter) {
			continue
		}
		mapped := mapSupplierStatus(supplier.LastStatus)
		switch mapped {
		case "error":
			return "error"
		case "warning":
			status = "warning"
		case "healthy":
			if status == "unknown" {
				status = "healthy"
			}
		}
	}
	return status
}

func connectorSpecHealth(suppliers []domain.Supplier, specID string) string {
	status := "unknown"
	for _, supplier := range suppliers {
		if supplier.Metadata["connector_spec_id"] != specID {
			continue
		}
		mapped := mapSupplierStatus(supplier.LastStatus)
		if mapped == "error" {
			return "error"
		}
		if mapped == "warning" || (mapped == "healthy" && status == "unknown") {
			status = mapped
		}
	}
	return status
}

func countConnectorSpecUsers(suppliers []domain.Supplier, specID string) int {
	count := 0
	for _, supplier := range suppliers {
		if supplier.Metadata["connector_spec_id"] == specID {
			count++
		}
	}
	return count
}

func credentialStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "active", "valid":
		return "valid"
	case "refresh_failed", "auth_expired", "invalid", "error":
		return "invalid"
	case "expired":
		return "invalid"
	case "disabled":
		return "invalid"
	default:
		return "pending"
	}
}

func normalizeNoChangeAutomationProjection(status string, eligible *bool, reason string) (*bool, string) {
	if strings.EqualFold(strings.TrimSpace(status), "no_change") {
		return nil, ""
	}
	return eligible, reason
}

func consolePolicyHasPriceChange(current, suggested *float64) bool {
	if current == nil || suggested == nil {
		return false
	}
	return decimal.NewFromFloat(*current).Sub(decimal.NewFromFloat(*suggested)).Abs().GreaterThan(decimal.NewFromFloat(0.000000001))
}

func projectedCredentialStatus(credential domain.Credential, coreAuthStatus string, managedRenewalHealthy,
	automaticRecoveryPending bool, now time.Time, refreshAhead time.Duration) string {
	if refreshAhead <= 0 {
		refreshAhead = 15 * time.Minute
	}
	if automaticRecoveryPending {
		return "valid"
	}
	if credential.ExpiresAt != nil && !credential.ExpiresAt.After(now) {
		return "invalid"
	}
	status := credentialStatus(credential.Status)
	switch coreAuthStatus {
	case "valid":
		status = "valid"
	case "invalid":
		status = "invalid"
	}
	if credential.ExpiresAt == nil || credential.ExpiresAt.After(now.Add(refreshAhead)) {
		return status
	}
	if status == "valid" && managedRenewalHealthy {
		return "valid"
	}
	if status == "valid" {
		return "expiring"
	}
	return status
}

func credentialRenewalCanRecover(renewal domain.CredentialRenewalStatus) bool {
	if !renewal.Configured || !renewal.Enabled || strings.TrimSpace(renewal.HumanAction) != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(renewal.State)) {
	case "healthy", "password_login_attempt", "refresh_token_attempt", "retry_backoff":
		return true
	default:
		return false
	}
}

func browserCustodyCanRecover(status *domain.BrowserCustodyStatus) bool {
	return status != nil && status.Enabled && status.HelperOnline && !status.RecoveryOverdue &&
		(!status.NeedsCapture || status.RecoveryPending) &&
		strings.EqualFold(strings.TrimSpace(status.HelperState), "online")
}

func groupRatio(snapshot domain.CatalogSnapshot, name string) float64 {
	return floatValue(groupRatioString(snapshot, name))
}

func groupRatioString(snapshot domain.CatalogSnapshot, name string) string {
	for _, group := range snapshot.Groups {
		if group.ExternalID == name || group.Name == name {
			return ratios.DecisionRatio(group)
		}
	}
	return ""
}

func discoveryID(item domain.DiscoveryItem) string {
	return fmt.Sprintf("%d:%s:%s", item.SupplierID, item.Kind, item.ExternalID)
}

func displayConsoleValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未设置"
	}
	if parsed, err := decimal.NewFromString(strings.TrimSpace(value)); err == nil {
		return parsed.String()
	}
	return strings.TrimSpace(value)
}

func optionalConsoleValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return displayConsoleValue(value)
}

func countAdapter(suppliers []domain.Supplier, adapter string) int {
	count := 0
	for _, supplier := range suppliers {
		if strings.Contains(strings.ToLower(supplier.Adapter), adapter) {
			count++
		}
	}
	return count
}

func credentialMethod(kind string) string {
	value := strings.ToLower(kind)
	if strings.Contains(value, "cookie") {
		return "Cookie"
	}
	if strings.Contains(value, "pair") || strings.Contains(value, "browser") {
		return "Browser Pairing"
	}
	if strings.Contains(value, "api") {
		return "API Key"
	}
	return "Access Token"
}

func activityKind(kind string) string {
	switch {
	case strings.Contains(kind, "mirror"):
		return "mirror"
	case strings.Contains(kind, "price"), strings.Contains(kind, "pricing"), strings.Contains(kind, "proposal"), strings.Contains(kind, "automation"):
		return "price"
	case strings.Contains(kind, "balance"), strings.Contains(kind, "ledger"), strings.Contains(kind, "financial"):
		return "balance"
	case strings.Contains(kind, "credential"), strings.Contains(kind, "auth"), strings.Contains(kind, "pair"), strings.Contains(kind, "custody"):
		return "auth"
	case strings.Contains(kind, "discovery"), strings.Contains(kind, "listing"):
		return "listing"
	case strings.Contains(kind, "config"), strings.Contains(kind, "target_"), strings.Contains(kind, "supplier_"), strings.Contains(kind, "control"):
		return "system"
	default:
		return "collect"
	}
}

func projectConsoleActivity(activity domain.Activity) consoleActivity {
	return consoleActivity{ID: fmt.Sprint(activity.ID), Kind: activityKind(activity.Kind), RawKind: activity.Kind,
		Title: activity.Title, Detail: activity.Message, At: activity.CreatedAt.Format(time.RFC3339),
		Status: activityStatus(activity.Severity), Severity: activity.Severity,
		Metadata: consoleActivityMetadata(activity.Metadata)}
}

func activityStatus(severity string) string {
	switch severity {
	case "success", "info":
		return "healthy"
	case "warning":
		return "warning"
	case "error":
		return "error"
	default:
		return "unknown"
	}
}
