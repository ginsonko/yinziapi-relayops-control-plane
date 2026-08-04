package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/decimalnorm"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/intelligence"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type Service struct {
	store                *store.Store
	targets              *targets.Factory
	intelligence         *intelligence.Service
	refreshSupplier      func(context.Context, int64) error
	automaticWrites      atomic.Bool
	remoteWrites         *remotewrite.Gate
	locksMu              sync.Mutex
	targetLocks          map[int64]*sync.Mutex
	proposalLocks        map[int64]*sync.Mutex
	recollectMu          sync.Mutex
	recollecting         map[int64]struct{}
	targetReadMu         sync.Mutex
	targetReadCache      map[int64]targetReadCacheEntry
	targetReadFlights    map[targetReadFlightKey]*targetReadFlight
	targetReadGeneration map[int64]uint64
}

type targetReadState struct {
	snapshot   domain.TargetSnapshot
	catalog    domain.CatalogSnapshot
	catalogErr error
}

type targetReadCacheEntry struct {
	key       string
	state     targetReadState
	err       error
	expiresAt time.Time
}

type targetReadFlightKey struct {
	targetID   int64
	generation uint64
	key        string
}

type targetReadFlight struct {
	done  chan struct{}
	state targetReadState
	err   error
}

// ConfirmationConfig controls the short, targeted evidence confirmation pass
// after a real price difference is observed. It is read from the settings
// store for every pass, so operators can tune it without a restart.
type ConfirmationConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	MaxAttempts     int  `json:"max_attempts"`
}

// SafetyConfig contains the operator-tunable gates that prevent a single bad
// price dimension or an implausible target jump from becoming an unattended
// write. A rejected automatic write remains available as an approval proposal.
type SafetyConfig struct {
	TargetReadCacheMilliseconds  int    `json:"target_read_cache_milliseconds"`
	EquivalentCostMultipleLimit  string `json:"equivalent_cost_multiple_limit"`
	MinimumComparableDimensions  int    `json:"minimum_comparable_dimensions"`
	MaximumAutomaticIncreaseRate string `json:"maximum_automatic_increase_rate"`
	MaximumAutomaticDecreaseRate string `json:"maximum_automatic_decrease_rate"`
}

const (
	confirmationConfigKey = "automation.confirmation_recollect.v1"
	safetyConfigKey       = "automation.safety.v1"
)

func DefaultConfirmationConfig() ConfirmationConfig {
	return ConfirmationConfig{Enabled: true, IntervalSeconds: 2, MaxAttempts: 2}
}

func DefaultSafetyConfig() SafetyConfig {
	return SafetyConfig{
		TargetReadCacheMilliseconds:  30 * 60 * 1000,
		EquivalentCostMultipleLimit:  "4",
		MinimumComparableDimensions:  2,
		MaximumAutomaticIncreaseRate: "1",
		MaximumAutomaticDecreaseRate: "0.75",
	}
}

func normalizeConfirmationConfig(config ConfirmationConfig) ConfirmationConfig {
	defaults := DefaultConfirmationConfig()
	if config.IntervalSeconds < 1 {
		config.IntervalSeconds = defaults.IntervalSeconds
	}
	if config.IntervalSeconds > 60 {
		config.IntervalSeconds = 60
	}
	if config.MaxAttempts < 1 {
		config.MaxAttempts = defaults.MaxAttempts
	}
	if config.MaxAttempts > 4 {
		config.MaxAttempts = 4
	}
	return config
}

func normalizeSafetyConfig(config SafetyConfig) (SafetyConfig, error) {
	if config.TargetReadCacheMilliseconds < 60000 || config.TargetReadCacheMilliseconds > 60*60*1000 {
		return SafetyConfig{}, errors.New("target read cache must be between 1 and 60 minutes")
	}
	if config.MinimumComparableDimensions < 0 || config.MinimumComparableDimensions > 1000 {
		return SafetyConfig{}, errors.New("minimum comparable dimensions must be between 0 and 1000")
	}
	for label, raw := range map[string]string{
		"equivalent cost multiple limit":  config.EquivalentCostMultipleLimit,
		"maximum automatic increase rate": config.MaximumAutomaticIncreaseRate,
		"maximum automatic decrease rate": config.MaximumAutomaticDecreaseRate,
	} {
		value, err := decimal.NewFromString(strings.TrimSpace(raw))
		if err != nil || value.IsNegative() {
			return SafetyConfig{}, fmt.Errorf("%s must be a nonnegative decimal", label)
		}
		if value.GreaterThan(decimal.NewFromInt(1000)) {
			return SafetyConfig{}, fmt.Errorf("%s cannot exceed 1000", label)
		}
	}
	return config, nil
}

func NewService(st *store.Store, targetFactory *targets.Factory) *Service {
	return &Service{store: st, targets: targetFactory, intelligence: intelligence.NewService(st),
		targetLocks: make(map[int64]*sync.Mutex), proposalLocks: make(map[int64]*sync.Mutex), recollecting: make(map[int64]struct{}),
		targetReadCache: make(map[int64]targetReadCacheEntry), targetReadFlights: make(map[targetReadFlightKey]*targetReadFlight),
		targetReadGeneration: make(map[int64]uint64)}
}

func (s *Service) ConfirmationConfig(ctx context.Context) (ConfirmationConfig, error) {
	config := DefaultConfirmationConfig()
	if err := s.store.GetSetting(ctx, confirmationConfigKey, &config); err != nil {
		if store.IsNotFound(err) {
			return config, nil
		}
		return ConfirmationConfig{}, err
	}
	return normalizeConfirmationConfig(config), nil
}

func (s *Service) SetConfirmationConfig(ctx context.Context, config ConfirmationConfig) (ConfirmationConfig, error) {
	config = normalizeConfirmationConfig(config)
	if err := s.store.SetSetting(ctx, confirmationConfigKey, config); err != nil {
		return ConfirmationConfig{}, err
	}
	return config, nil
}

func (s *Service) SafetyConfig(ctx context.Context) (SafetyConfig, error) {
	config := DefaultSafetyConfig()
	if err := s.store.GetSetting(ctx, safetyConfigKey, &config); err != nil {
		if store.IsNotFound(err) {
			return config, nil
		}
		return SafetyConfig{}, err
	}
	return normalizeSafetyConfig(config)
}

func (s *Service) SetSafetyConfig(ctx context.Context, config SafetyConfig) (SafetyConfig, error) {
	config, err := normalizeSafetyConfig(config)
	if err != nil {
		return SafetyConfig{}, err
	}
	if err := s.store.SetSetting(ctx, safetyConfigKey, config); err != nil {
		return SafetyConfig{}, err
	}
	s.InvalidateAllTargetReads()
	return config, nil
}

func (s *Service) SetSupplierRefresher(refresh func(context.Context, int64) error) {
	s.refreshSupplier = refresh
}

// SetAutomaticWritesEnabled is the service-level kill switch for unattended
// remote writes. Manual, explicitly authenticated Apply calls remain
// available. ReconcileSupplier still refreshes simulations and proposals while
// this switch is off, but cannot produce a remote write.
func (s *Service) SetAutomaticWritesEnabled(enabled bool) {
	s.automaticWrites.Store(enabled)
}

func (s *Service) SetRemoteWriteGate(gate *remotewrite.Gate) {
	s.remoteWrites = gate
}

func (s *Service) Simulate(ctx context.Context, policyID int64) (domain.PolicySimulation, error) {
	policy, err := s.store.GetPolicy(ctx, policyID)
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	supplier, err := s.store.GetSupplier(ctx, policy.SupplierID)
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	target, err := s.store.GetTarget(ctx, policy.TargetSiteID)
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "target_read_failed",
			"检查本站目标站点配置和管理授权", policy, supplier, domain.TargetSite{ID: policy.TargetSiteID}, domain.GroupOffer{}, nil, err)
	}
	snapshot, err := s.store.LatestEffectiveSnapshot(ctx, policy.SupplierID)
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "supplier_snapshot_missing",
			"先重新采集该上游并确认采集成功，再重算规则", policy, supplier, target, domain.GroupOffer{}, nil,
			fmt.Errorf("latest supplier snapshot: %w", err))
	}
	evidence, err := s.store.CatalogEvidenceStatus(ctx, policy.SupplierID, snapshot.Evidence.ResponseHash)
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "catalog_evidence_unavailable",
			"重新采集上游目录并等待证据状态更新", policy, supplier, target, domain.GroupOffer{}, nil,
			fmt.Errorf("catalog evidence status: %w", err))
	}
	evidence = evaluateCatalogEvidence(evidence, snapshot.Evidence.SourceType)
	group, err := findGroup(snapshot, policy.SourceGroup)
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "source_group_missing",
			"重新采集上游并检查规则绑定的分组名称；若上游已下架该分组，请暂停或编辑规则", policy, supplier, target,
			domain.GroupOffer{Name: policy.SourceGroup}, nil, err)
	}
	sourceBasis := strings.ToLower(strings.TrimSpace(policy.SourceBasis))
	if sourceBasis == "" {
		sourceBasis = "group_ratio"
	}
	if sourceBasis != "group_ratio" && sourceBasis != "equivalent_cost" && sourceBasis != "group_model_contract" {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "source_basis_invalid",
			"编辑规则并选择真实分组倍率或逐模型等价成本", policy, supplier, target, group, nil,
			fmt.Errorf("unsupported source basis %q", policy.SourceBasis))
	}
	groupRatioBasis := sourceBasis == "group_ratio"
	matchedModels, matchErr := matchSourceModels(group, policy.SourceModelPattern)
	if matchErr != nil && !groupRatioBasis {
		category, action := "source_model_pattern_unmatched", "检查模型匹配模式与上游分组模型；上游恢复后重新采集即可自动重算"
		if len(group.ModelIDs) == 0 {
			category, action = "source_group_models_empty", "重新采集上游；若分组仍为空，检查上游是否下架了模型，或暂停/编辑这条规则"
		}
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", category, action,
			policy, supplier, target, group, nil, matchErr)
	}
	if matchErr != nil {
		matchedModels = []string{}
	}
	if strings.TrimSpace(ratios.DecisionRatio(group)) == "" {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "source_ratio_unavailable",
			"重新采集上游倍率或改用一个有可信倍率证据的分组", policy, supplier, target, group, matchedModels,
			fmt.Errorf("source group %q has no trustworthy ratio", group.Name))
	}
	safety, err := s.SafetyConfig(ctx)
	if err != nil {
		return domain.PolicySimulation{}, err
	}
	targetState, err := s.readTargetState(ctx, target, time.Duration(safety.TargetReadCacheMilliseconds)*time.Millisecond)
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "target_read_failed",
			"检查本站管理授权和模型/倍率只读接口，恢复后重新计算", policy, supplier, target, group, matchedModels, err)
	}
	targetSnapshot := targetState.snapshot
	if sourceBasis == "group_model_contract" {
		return s.simulateGroupModelContract(ctx, policy, supplier, target, snapshot, evidence, group, matchedModels, targetState, safety)
	}
	impact := pricing.PricingImpact{Models: append([]string{}, matchedModels...), CostRatioVector: map[string]string{},
		CoverageRatio: "0", Complete: false, Confidence: "unavailable", TotalDimensions: len(matchedModels)}
	targetModelPriceHash := ""
	impactErr := targetState.catalogErr
	if impactErr == nil {
		targetModelPriceHash = pricing.ModelPriceHash(targetState.catalog)
		impact, impactErr = pricing.ResolvePricingImpactForTargetGroup(snapshot, targetState.catalog, group,
			policy.TargetGroup, matchedModels)
	}
	if impactErr != nil && !groupRatioBasis {
		return domain.PolicySimulation{}, newPricingImpactDiagnosticError(policy, supplier, target, group, matchedModels,
			targetState, impact, impactErr)
	}
	currentRatio := targetSnapshot.GroupRatio[policy.TargetGroup]
	ruleSourceRatio := ratios.DecisionRatio(group)
	if !groupRatioBasis {
		ruleSourceRatio = impact.EquivalentCostRatio
	}
	calculation, err := pricing.Calculate(pricing.Input{SourceRatio: ruleSourceRatio, CostRatio: ruleSourceRatio,
		CurrentRatio: currentRatio, Policy: policy})
	if err != nil {
		return domain.PolicySimulation{}, newRuleContextError("pricing rule cannot be recalculated", "rule_calculation_invalid",
			"检查规则类型、参数、上下限和舍入步长，再重新保存规则", policy, supplier, target, group, matchedModels, err)
	}
	calculation.EquivalentCostRatio = impact.EquivalentCostRatio
	calculation.SourceEvidenceHash = evidence.ResponseHash
	calculation.EvidenceHash = pricingImpactHash(evidence.ResponseHash, targetModelPriceHash, impact.CostRatioVector)
	calculation.RawSourceRatio = group.RawRatio
	calculation.RechargeValueFactor = supplier.RechargeValueFactor
	calculation.PolicyFingerprint = pricing.PolicyFingerprint(policy)
	fingerprintInput := pricing.DecisionFingerprintInput{
		Policy: policy, SourceEvidenceHash: evidence.ResponseHash, PricingEvidenceHash: calculation.EvidenceHash,
		TargetHash: targetSnapshot.Hash, TargetCatalogHash: targetModelPriceHash,
		RawSourceRatio: group.RawRatio, EffectiveSourceRatio: ratios.DecisionRatio(group),
		RechargeValueFactor: supplier.RechargeValueFactor, EquivalentCostRatio: impact.EquivalentCostRatio,
		CurrentRatio: currentRatio, CostRatioVector: impact.CostRatioVector,
	}
	if groupRatioBasis {
		// Model prices are owned by complete mirrors. A group-ratio policy must
		// not become stale merely because that independent contract changed.
		fingerprintInput.SourceEvidenceHash = ""
		fingerprintInput.PricingEvidenceHash = ""
		fingerprintInput.TargetCatalogHash = ""
		fingerprintInput.EquivalentCostRatio = ""
		fingerprintInput.CostRatioVector = nil
	}
	calculation.InputFingerprint = pricing.DecisionInputFingerprint(fingerprintInput)
	warnings := append([]string{}, snapshot.Evidence.Warnings...)
	if impactErr == nil {
		warnings = append(warnings, impact.Warnings...)
	} else {
		message := fmt.Sprintf("逐模型目录风险暂不可验证（%d 个模型没有形成可比的同币种金额维度）；本规则按真实分组倍率定价，因此不影响建议值和自动跟价", len(matchedModels))
		calculation.Warnings = append(calculation.Warnings, message)
		warnings = append(warnings, message)
	}
	if !groupRatioBasis && !impact.Complete {
		coverageAllowed, message := coverageEligibility(policy, impact)
		if !coverageAllowed {
			calculation.AutomationEligible = false
			calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
		}
		calculation.Warnings = append(calculation.Warnings, message)
		warnings = append(warnings, message)
	}
	applySafetyGates(&calculation, policy, group, impact, currentRatio, safety, &warnings)
	hasPriceDifference := !decimalEqual(currentRatio, calculation.TargetRatio)
	evidenceOnlyBlocked := hasPriceDifference && calculation.AutomationEligible && !evidence.AutoEligible
	if hasPriceDifference && !evidence.AutoEligible {
		message := fmt.Sprintf("当前价格目录还需连续 %d 次相同读数确认；模拟和人工审批可立即进行，无人值守自动执行暂按审批处理",
			evidence.RemainingObservations)
		calculation.AutomationEligible = false
		calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
		calculation.Warnings = append(calculation.Warnings, message)
		warnings = append(warnings, message)
	}
	warnings = append(warnings, fmt.Sprintf("模型范围 %q 当前匹配 %d 个模型", defaultPattern(policy.SourceModelPattern), len(matchedModels)))
	if groupRatioBasis {
		warnings = append(warnings, fmt.Sprintf("跟价规则按现金折算后的有效分组倍率 %s 计算；上游原始倍率 %s，充值折算系数 %s；规则价差与逐模型目录风险分开显示",
			ratios.DecisionRatio(group), group.RawRatio, group.RechargeValueFactor))
	} else {
		warnings = append(warnings, fmt.Sprintf("跟价规则明确选择逐模型等价成本 %s 作为定价基准", impact.EquivalentCostRatio))
	}
	if strings.TrimSpace(impact.EquivalentCostRatio) != "" {
		warnings = append(warnings, fmt.Sprintf("逐模型计费维度核算后的保守等效成本倍率为 %s；该值是目录风险上界，不是真实现金采购成本", impact.EquivalentCostRatio))
	}
	if currentRatio == "" {
		warnings = append(warnings, "目标分组当前不存在；应用后会创建该分组倍率")
	}
	age := time.Since(snapshot.Evidence.ObservedAt)
	if age > 2*time.Duration(max(policyFreshnessSeconds(policy), 300))*time.Second {
		warnings = append(warnings, "上游快照已经偏旧，应用前会重新采集并重新计算")
	}
	return domain.PolicySimulation{Policy: policy, SourceGroup: group, MatchedModels: matchedModels,
		EquivalentCostRatio: impact.EquivalentCostRatio, CostRatioVector: impact.CostRatioVector,
		ComparableDimensions: impact.ComparableDimensions, TotalDimensions: impact.TotalDimensions,
		CoverageRatio: impact.CoverageRatio, ImpactComplete: impact.Complete, ImpactConfidence: impact.Confidence,
		UnknownDimensions: impact.UnknownDimensions, CurrentRatio: currentRatio,
		Calculation: calculation, Evidence: evidence, TargetHash: targetSnapshot.Hash,
		EvidenceOnlyBlocked: evidenceOnlyBlocked,
		TargetCatalogHash:   targetModelPriceHash, Warnings: warnings}, nil
}

func (s *Service) readTargetState(ctx context.Context, target domain.TargetSite, ttl time.Duration) (targetReadState, error) {
	key := targetReadConfigKey(target)
	now := time.Now()
	s.targetReadMu.Lock()
	generation := s.targetReadGeneration[target.ID]
	if cached, ok := s.targetReadCache[target.ID]; ok && cached.key == key && now.Before(cached.expiresAt) {
		state := cached.state
		err := cached.err
		s.targetReadMu.Unlock()
		return state, err
	}
	flightKey := targetReadFlightKey{targetID: target.ID, generation: generation, key: key}
	if flight := s.targetReadFlights[flightKey]; flight != nil {
		s.targetReadMu.Unlock()
		return waitForTargetRead(ctx, flight)
	}
	flight := &targetReadFlight{done: make(chan struct{})}
	s.targetReadFlights[flightKey] = flight
	s.targetReadMu.Unlock()

	go s.runTargetReadFlight(target, ttl, flightKey, flight)
	return waitForTargetRead(ctx, flight)
}

func (s *Service) runTargetReadFlight(target domain.TargetSite, ttl time.Duration, key targetReadFlightKey, flight *targetReadFlight) {
	readCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state, err := s.performTargetRead(readCtx, target)

	s.targetReadMu.Lock()
	flight.state, flight.err = state, err
	delete(s.targetReadFlights, key)
	cacheTTL := ttl
	if err != nil && isTargetRateLimit(err) && cacheTTL < 5*time.Second {
		cacheTTL = 5 * time.Second
	}
	if cacheTTL > 0 && (err == nil || isTargetRateLimit(err)) && s.targetReadGeneration[target.ID] == key.generation {
		s.targetReadCache[target.ID] = targetReadCacheEntry{key: key.key, state: state, err: err,
			expiresAt: time.Now().Add(cacheTTL)}
	}
	close(flight.done)
	s.targetReadMu.Unlock()
}

func (s *Service) performTargetRead(ctx context.Context, target domain.TargetSite) (targetReadState, error) {
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		return targetReadState{}, err
	}
	snapshot, err := adapter.Snapshot(ctx, target)
	if err != nil {
		_ = s.store.MarkTargetResult(ctx, target.ID, "degraded", err.Error())
		return targetReadState{}, err
	}
	_ = s.store.MarkTargetResult(ctx, target.ID, "healthy", "")
	state := targetReadState{snapshot: snapshot}
	channelAdapter, err := targets.AsChannelAdapter(adapter)
	if err != nil {
		state.catalogErr = fmt.Errorf("target pricing catalog: %w", err)
		return state, nil
	}
	state.catalog, err = channelAdapter.PricingCatalog(ctx, target)
	if isTargetRateLimit(err) {
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return state, ctx.Err()
		case <-timer.C:
			state.catalog, err = channelAdapter.PricingCatalog(ctx, target)
		}
	}
	if err != nil {
		state.catalogErr = fmt.Errorf("target pricing catalog: %w", err)
	}
	return state, nil
}

func isTargetRateLimit(err error) bool {
	if err == nil {
		return false
	}
	if targets.RemoteHTTPStatus(err) == http.StatusTooManyRequests {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "429") || strings.Contains(message, "too many requests") ||
		strings.Contains(message, "rate limit")
}

func waitForTargetRead(ctx context.Context, flight *targetReadFlight) (targetReadState, error) {
	select {
	case <-ctx.Done():
		return targetReadState{}, ctx.Err()
	case <-flight.done:
		return flight.state, flight.err
	}
}

func targetReadConfigKey(target domain.TargetSite) string {
	// UpdatedAt also changes when health is recorded, so including it would let
	// a successful read invalidate its own cache and recreate the N+1 burst.
	return fmt.Sprintf("%d|%s|%s|%d|%t|%s", target.ID, strings.TrimSpace(target.BaseURL), strings.TrimSpace(target.Adapter),
		target.CredentialID, target.Enabled, strings.TrimSpace(target.WriteMode))
}

// InvalidateTargetRead prevents any read started before a target mutation from
// being published back into the short consistency cache after that mutation.
func (s *Service) InvalidateTargetRead(targetID int64) {
	if s.targets != nil {
		s.targets.InvalidateTarget(targetID)
	}
	s.targetReadMu.Lock()
	s.targetReadGeneration[targetID]++
	delete(s.targetReadCache, targetID)
	s.targetReadMu.Unlock()
}

func (s *Service) InvalidateAllTargetReads() {
	s.targetReadMu.Lock()
	seen := make(map[int64]struct{})
	for targetID := range s.targetReadCache {
		seen[targetID] = struct{}{}
	}
	for key := range s.targetReadFlights {
		seen[key.targetID] = struct{}{}
	}
	for targetID := range seen {
		s.targetReadGeneration[targetID]++
	}
	s.targetReadCache = make(map[int64]targetReadCacheEntry)
	s.targetReadMu.Unlock()
}

func applySafetyGates(calculation *domain.PriceCalculation, policy domain.PricingPolicy, group domain.GroupOffer,
	impact pricing.PricingImpact, currentRatio string, config SafetyConfig, warnings *[]string) {
	block := func(message string) {
		calculation.AutomationEligible = false
		calculation.AutomationReason = appendReason(calculation.AutomationReason, message)
		calculation.Warnings = append(calculation.Warnings, message)
		*warnings = append(*warnings, message)
	}
	if strings.EqualFold(strings.TrimSpace(policy.SourceBasis), "equivalent_cost") {
		if config.MinimumComparableDimensions > 0 && impact.ComparableDimensions < config.MinimumComparableDimensions {
			block(fmt.Sprintf("逐模型等价成本只有 %d 个可比维度，低于无人值守门槛 %d；建议值保留，但仅允许人工审批",
				impact.ComparableDimensions, config.MinimumComparableDimensions))
		}
		limit, limitErr := decimal.NewFromString(strings.TrimSpace(config.EquivalentCostMultipleLimit))
		equivalent, equivalentErr := decimal.NewFromString(strings.TrimSpace(impact.EquivalentCostRatio))
		effective, effectiveErr := decimal.NewFromString(strings.TrimSpace(ratios.DecisionRatio(group)))
		if limitErr != nil || equivalentErr != nil || effectiveErr != nil || effective.LessThanOrEqual(decimal.Zero) {
			block("逐模型等价成本与有效分组倍率的倍数关系无法验证；建议值保留，但仅允许人工审批")
		} else if limit.IsPositive() {
			multiple := equivalent.Div(effective)
			if multiple.GreaterThan(limit) {
				block(fmt.Sprintf("逐模型等价成本 %s 是有效分组倍率 %s 的 %s 倍，超过无人值守门槛 %s；可能存在单位或单点维度异常",
					equivalent.String(), effective.String(), multiple.StringFixed(2), limit.String()))
			}
		}
	}

	current, currentErr := decimal.NewFromString(strings.TrimSpace(currentRatio))
	target, targetErr := decimal.NewFromString(strings.TrimSpace(calculation.TargetRatio))
	if currentErr != nil || targetErr != nil || !current.IsPositive() || target.IsNegative() || current.Equal(target) {
		return
	}
	if target.GreaterThan(current) {
		limit, _ := decimal.NewFromString(strings.TrimSpace(config.MaximumAutomaticIncreaseRate))
		rate := target.Sub(current).Div(current)
		if limit.IsPositive() && rate.GreaterThan(limit) {
			block(fmt.Sprintf("目标倍率单次上涨 %s%%，超过无人值守门槛 %s%%；建议值保留，但仅允许人工审批",
				rate.Mul(decimal.NewFromInt(100)).StringFixed(2), limit.Mul(decimal.NewFromInt(100)).StringFixed(2)))
		}
		return
	}
	limit, _ := decimal.NewFromString(strings.TrimSpace(config.MaximumAutomaticDecreaseRate))
	rate := current.Sub(target).Div(current)
	if limit.IsPositive() && rate.GreaterThan(limit) {
		block(fmt.Sprintf("目标倍率单次下降 %s%%，超过无人值守门槛 %s%%；建议值保留，但仅允许人工审批",
			rate.Mul(decimal.NewFromInt(100)).StringFixed(2), limit.Mul(decimal.NewFromInt(100)).StringFixed(2)))
	}
}

func coverageEligibility(policy domain.PricingPolicy, impact pricing.PricingImpact) (bool, string) {
	mode := strings.TrimSpace(policy.CoverageMode)
	if mode == "" {
		mode = "threshold"
	}
	minimumRaw := strings.TrimSpace(policy.MinimumCoverage)
	if minimumRaw == "" {
		minimumRaw = "0.8"
	}
	coverage, err := decimal.NewFromString(impact.CoverageRatio)
	if err != nil || coverage.IsNegative() || coverage.GreaterThan(decimal.NewFromInt(1)) {
		return false, "价格维度覆盖率无效，自动执行已停止"
	}
	prefix := fmt.Sprintf("价格维度覆盖率为 %s（%d/%d）", impact.CoverageRatio, impact.ComparableDimensions, impact.TotalDimensions)
	switch mode {
	case "strict":
		return false, prefix + "；当前规则要求所有相关价格维度完整，因此只生成审批建议"
	case "owner_override":
		if impact.ComparableDimensions < 1 {
			return false, prefix + "；没有任何可比成本维度，所有者覆盖不能执行"
		}
		return true, prefix + "；站点所有者已明确接受未知维度，系统保留风险告警并允许自动执行"
	case "threshold":
		minimum, parseErr := decimal.NewFromString(minimumRaw)
		if parseErr != nil || minimum.IsNegative() || minimum.GreaterThan(decimal.NewFromInt(1)) {
			return false, "价格覆盖率门槛配置无效，自动执行已停止"
		}
		if impact.ComparableDimensions < 1 || coverage.LessThan(minimum) {
			return false, fmt.Sprintf("%s；低于规则门槛 %s，因此只生成审批建议", prefix, minimum.String())
		}
		return true, fmt.Sprintf("%s；已达到规则门槛 %s，未知维度保留告警并允许自动执行", prefix, minimum.String())
	default:
		return false, fmt.Sprintf("价格覆盖策略 %q 无效，自动执行已停止", mode)
	}
}

func (s *Service) Propose(ctx context.Context, policyID int64) (domain.ChangeProposal, error) {
	lock := s.proposalLock(policyID)
	lock.Lock()
	defer lock.Unlock()

	simulation, err := s.Simulate(ctx, policyID)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	mode := simulation.Policy.ExecutionMode
	if !simulation.Calculation.AutomationEligible {
		mode = "approval"
	}
	semanticKey := proposalKey(simulation)
	if existing, findErr := s.store.GetActiveProposalBySemanticKey(ctx, semanticKey); findErr == nil {
		if existing.Status == "no_change" {
			existing.TargetSiteID = simulation.Policy.TargetSiteID
			existing.TargetGroup = simulation.Policy.TargetGroup
			existing.ExpectedOld = simulation.CurrentRatio
			existing.ProposedValue = simulation.Calculation.TargetRatio
			existing.ExpectedHash = simulation.TargetHash
			existing.Calculation = simulation.Calculation
			existing.GroupModelPricing = simulation.GroupModelPricing
			return s.store.RefreshNoChangeProposal(ctx, existing.ID, existing)
		}
		if existing.Status == "pending" && existing.Mode != mode {
			return s.store.UpdatePendingProposalMode(ctx, existing.ID, mode, simulation.Calculation)
		}
		return existing, nil
	} else if !store.IsNotFound(findErr) {
		return domain.ChangeProposal{}, findErr
	}
	noChange := !simulationHasChange(simulation)
	if _, err := s.store.SupersedeActionableProposals(ctx, policyID,
		"上游或本站模型价格证据在预览后发生变化，已生成新的变更建议"); err != nil {
		return domain.ChangeProposal{}, err
	}
	idempotency := fmt.Sprintf("%s:%d", semanticKey, time.Now().UTC().UnixNano())
	proposal := domain.ChangeProposal{
		PolicyID: simulation.Policy.ID, TargetSiteID: simulation.Policy.TargetSiteID,
		TargetGroup: simulation.Policy.TargetGroup, ExpectedOld: simulation.CurrentRatio,
		ProposedValue: simulation.Calculation.TargetRatio, ExpectedHash: simulation.TargetHash,
		Calculation: simulation.Calculation, Status: "pending", Mode: mode,
		GroupModelPricing: simulation.GroupModelPricing,
		IdempotencyKey:    idempotency,
	}
	if simulation.GroupModelPricing.Mode == "atomic_contract" {
		// These compatibility fields remain decimal GroupRatio values. Contract
		// revisions have dedicated typed fields and must never enter arithmetic,
		// display formatting or the elasticity model as if they were prices.
		proposal.ExpectedOld = simulation.CurrentRatio
		proposal.ProposedValue = simulation.CurrentRatio
		proposal.ExpectedHash = simulation.GroupModelPricing.ExpectedRevision
		proposal.GroupModelPricingReceipt = domain.ListingGroupModelPricingReceipt{
			Mode: "atomic_contract", ExpectedRevision: simulation.GroupModelPricing.ExpectedRevision,
			DesiredRevision: simulation.GroupModelPricing.DesiredRevision, Before: simulation.GroupModelPricing.ExpectedContract,
			After: simulation.GroupModelPricing.DesiredContract, Diff: append([]domain.TargetFieldDiff(nil), simulation.GroupModelPricing.Diff...),
		}
	}
	if noChange {
		proposal.Status = "no_change"
		if existing, findErr := s.store.GetLatestNoChangeProposal(ctx, policyID); findErr == nil {
			return s.store.RefreshNoChangeProposal(ctx, existing.ID, proposal)
		} else if !store.IsNotFound(findErr) {
			return domain.ChangeProposal{}, findErr
		}
	}
	if err := s.store.CreateProposal(ctx, &proposal); err != nil {
		return domain.ChangeProposal{}, err
	}
	if noChange {
		return proposal, nil
	}
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "proposal_created", Severity: "info", Title: "已生成价格变更预览",
		Message:  fmt.Sprintf("%s: %s -> %s", proposal.TargetGroup, displayEmpty(proposal.ExpectedOld), displayEmpty(proposal.ProposedValue)),
		Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID)}})
	return proposal, nil
}

func (s *Service) Apply(ctx context.Context, proposalID int64) (domain.ChangeProposal, error) {
	return s.apply(ctx, proposalID, true)
}

func (s *Service) apply(ctx context.Context, proposalID int64, refresh bool) (domain.ChangeProposal, error) {
	ctx = targets.WithFreshRead(ctx)
	if err := s.remoteWrites.Check("pricing proposal apply"); err != nil {
		return domain.ChangeProposal{}, err
	}
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	lock := s.targetLock(proposal.TargetSiteID)
	lock.Lock()
	defer lock.Unlock()
	proposal, err = s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	if proposal.Status == "applied" || proposal.Status == "no_change" {
		return proposal, nil
	}
	if proposal.Status == "stale" && strings.TrimSpace(proposal.Error) != "" {
		return domain.ChangeProposal{}, errors.New(proposal.Error)
	}
	if proposal.Status != "pending" && proposal.Status != "failed" {
		return domain.ChangeProposal{}, fmt.Errorf("proposal in state %q cannot be applied", proposal.Status)
	}
	policy, err := s.store.GetPolicy(ctx, proposal.PolicyID)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	releaseLease, err := s.acquireTargetLease(ctx, proposal.TargetSiteID, fmt.Sprintf("automation:apply:%d", proposal.ID))
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	defer releaseLease()
	if refresh {
		if s.refreshSupplier != nil {
			if refreshErr := s.refreshSupplier(ctx, policy.SupplierID); refreshErr != nil {
				_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", refreshErr.Error())
				return domain.ChangeProposal{}, fmt.Errorf("refresh upstream before apply: %w", refreshErr)
			}
		}
		// An explicit approval must re-read both the target ratios and model
		// catalog. A short simulation cache is valid for batch previews, but not
		// as the final evidence immediately before a remote write.
		s.InvalidateTargetRead(proposal.TargetSiteID)
		currentSimulation, simulationErr := s.Simulate(ctx, policy.ID)
		if simulationErr != nil {
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", simulationErr.Error())
			return domain.ChangeProposal{}, simulationErr
		}
		if proposal.Calculation.InputFingerprint == "" || proposal.Calculation.InputFingerprint != currentSimulation.Calculation.InputFingerprint {
			message := "上游或本站模型价格证据在预览后发生变化，请使用新的变更建议"
			readback := ""
			if currentSimulation.TargetHash != proposal.ExpectedHash {
				message = "目标价格在预览后发生变化，请重新生成预览"
				readback = currentSimulation.CurrentRatio
			}
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "stale", readback, message)
			return domain.ChangeProposal{}, errors.New(message)
		}
	}
	target, err := s.store.GetTarget(ctx, proposal.TargetSiteID)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	if !target.Enabled {
		err = fmt.Errorf("target %q is disabled", target.Name)
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	current, err := adapter.Snapshot(ctx, target)
	if err != nil {
		s.InvalidateTargetRead(target.ID)
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	isGroupModelContract := strings.EqualFold(strings.TrimSpace(policy.SourceBasis), "group_model_contract") ||
		proposal.GroupModelPricing.Mode == "atomic_contract"
	if isGroupModelContract {
		if current.GroupModelPricing == nil || current.GroupModelPricing.Revision != proposal.ExpectedHash {
			s.InvalidateTargetRead(target.ID)
			actual := "unavailable"
			if current.GroupModelPricing != nil {
				actual = current.GroupModelPricing.Revision
			}
			message := "target group-model pricing changed after preview; generate a new proposal"
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "stale", actual, message)
			return domain.ChangeProposal{}, errors.New(message)
		}
	} else if current.Hash != proposal.ExpectedHash {
		s.InvalidateTargetRead(target.ID)
		message := "目标价格在预览后发生变化，请重新生成预览"
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "stale", current.GroupRatio[proposal.TargetGroup], message)
		return domain.ChangeProposal{}, errors.New(message)
	}
	decisionSnapshot, err := s.intelligence.PrepareDecisionSnapshot(ctx, proposal.ID, "approve", time.Now().UTC())
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{proposal.Status}, "failed", "", err.Error())
		return domain.ChangeProposal{}, fmt.Errorf("freeze decision snapshot: %w", err)
	}
	if err := s.store.BeginProposalApplication(ctx, proposal.ID, []string{proposal.Status}, decisionSnapshot); err != nil {
		return domain.ChangeProposal{}, err
	}
	if isGroupModelContract {
		groupModelAdapter, adapterErr := targets.AsGroupModelPricingAdapter(adapter)
		if adapterErr != nil {
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"applying"}, "failed", "", adapterErr.Error())
			return domain.ChangeProposal{}, adapterErr
		}
		plan := proposal.GroupModelPricing
		receipt := proposal.GroupModelPricingReceipt
		if !plan.Exact || plan.ExpectedRevision != proposal.ExpectedHash || plan.DesiredRevision == "" ||
			(receipt.ExpectedRevision != "" && receipt.ExpectedRevision != plan.ExpectedRevision) ||
			(receipt.DesiredRevision != "" && receipt.DesiredRevision != plan.DesiredRevision) {
			err = errors.New("proposal group-model pricing plan is incomplete or internally inconsistent")
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"applying"}, "failed", "", err.Error())
			return domain.ChangeProposal{}, err
		}
		s.InvalidateTargetRead(target.ID)
		result, applyErr := groupModelAdapter.ApplyGroupModelPricingControlled(ctx, target, plan.ExpectedRevision,
			plan.DesiredContract, func() error {
				guard, guardErr := adapter.Snapshot(ctx, target)
				if guardErr != nil {
					return guardErr
				}
				if !decimalEqual(guard.GroupRatio[proposal.TargetGroup], plan.TargetGroupRatio) {
					return errors.New("target GroupRatio changed before the group-model pricing write")
				}
				return nil
			})
		s.InvalidateTargetRead(target.ID)
		if applyErr != nil {
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"applying"}, "failed", "", applyErr.Error())
			return domain.ChangeProposal{}, applyErr
		}
		if result.ResultState != "verified_success" || result.ReadbackHash != plan.DesiredRevision {
			err = errors.New("group-model pricing apply did not return the verified desired revision")
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"applying"}, "failed", result.ReadbackHash, err.Error())
			return domain.ChangeProposal{}, err
		}
		receipt.Mode = "atomic_contract"
		receipt.ExpectedRevision = plan.ExpectedRevision
		receipt.DesiredRevision = plan.DesiredRevision
		receipt.AppliedRevision = result.ReadbackHash
		receipt.Before = plan.ExpectedContract
		receipt.After = plan.DesiredContract
		receipt.Diff = append([]domain.TargetFieldDiff(nil), plan.Diff...)
		receipt.Changed = plan.ExpectedRevision != plan.DesiredRevision
		if err := s.store.UpdateProposalGroupModelPricingReceipt(ctx, proposal.ID, receipt, []string{"applying"}); err != nil {
			return domain.ChangeProposal{}, err
		}
		if err := s.store.CompleteProposalApplication(ctx, proposal.ID, result.ReadbackHash, result.ReadbackHash, time.Now().UTC()); err != nil {
			return domain.ChangeProposal{}, err
		}
		_ = s.store.AddActivity(ctx, domain.Activity{Kind: "proposal_group_model_pricing_applied", Severity: "success",
			Title: "Per-model pricing synchronized and verified",
			Message: fmt.Sprintf("%s: revision %s -> %s; %d model price entries changed; minimum margin %s",
				proposal.TargetGroup, shortRevision(plan.ExpectedRevision), shortRevision(result.ReadbackHash), len(plan.Models), plan.MinimumMarginRate),
			Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID), "readback_revision": result.ReadbackHash,
				"changed_models": fmt.Sprint(len(plan.Models)), "minimum_margin": plan.MinimumMarginRate}})
		return s.store.GetProposal(ctx, proposal.ID)
	}
	next := cloneRatios(current.GroupRatio)
	next[proposal.TargetGroup] = proposal.ProposedValue
	s.InvalidateTargetRead(target.ID)
	readback, err := adapter.ApplyGroupRatio(ctx, target, proposal.ExpectedHash, next)
	s.InvalidateTargetRead(target.ID)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"applying"}, "failed", "", err.Error())
		return domain.ChangeProposal{}, err
	}
	value := readback.GroupRatio[proposal.TargetGroup]
	if err := s.store.CompleteProposalApplication(ctx, proposal.ID, value, readback.Hash, time.Now().UTC()); err != nil {
		return domain.ChangeProposal{}, err
	}
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "proposal_applied", Severity: "success", Title: "价格变更已应用并读回",
		Message:  fmt.Sprintf("%s: %s -> %s", proposal.TargetGroup, displayEmpty(proposal.ExpectedOld), displayEmpty(value)),
		Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID), "readback_hash": readback.Hash}})
	return s.store.GetProposal(ctx, proposal.ID)
}

func (s *Service) ReconcileSupplier(ctx context.Context, supplierID int64) {
	_ = s.ReconcileSupplierWithError(ctx, supplierID)
}

// ReconcileSupplierWithError preserves legacy behavior while reporting an
// aggregate result to the shared post-collection dispatcher.
func (s *Service) ReconcileSupplierWithError(ctx context.Context, supplierID int64) error {
	failures := make([]error, 0)
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		s.recordAutomationFailure(ctx, supplierID, 0, "读取定价规则失败", err)
		return err
	}
	s.maybeConfirmPriceEvidence(ctx, supplierID, policies)
	targetPolicyCounts := make(map[int64]int)
	for _, policy := range policies {
		if policy.Enabled && policy.SupplierID == supplierID {
			targetPolicyCounts[policy.TargetSiteID]++
		}
	}
	failedTargets := make(map[int64]struct{})
	for _, policy := range policies {
		if !policy.Enabled || policy.SupplierID != supplierID {
			continue
		}
		if _, failed := failedTargets[policy.TargetSiteID]; failed {
			continue
		}
		proposal, proposalErr := s.Propose(ctx, policy.ID)
		if proposalErr != nil {
			if isAutomationWaitingCondition(proposalErr) {
				s.recordAutomationWaiting(ctx, supplierID, policy.ID, proposalErr)
				continue
			}
			if isSharedTargetReadFailure(proposalErr) {
				failedTargets[policy.TargetSiteID] = struct{}{}
				s.recordTargetReadFailure(ctx, supplierID, policy.TargetSiteID,
					targetPolicyCounts[policy.TargetSiteID], proposalErr)
				failures = append(failures, fmt.Errorf("legacy pricing target %d read: %w", policy.TargetSiteID, proposalErr))
				continue
			}
			s.recordAutomationFailure(ctx, supplierID, policy.ID, "重新计算定价规则失败", proposalErr)
			failures = append(failures, fmt.Errorf("legacy pricing policy %d propose: %w", policy.ID, proposalErr))
			continue
		}
		if proposal.Status == "no_change" {
			continue
		}
		if !s.automaticWrites.Load() {
			continue
		}
		target, targetErr := s.store.GetTarget(ctx, policy.TargetSiteID)
		if targetErr != nil {
			s.recordAutomationFailure(ctx, supplierID, policy.ID, "读取目标站点失败", targetErr)
			failures = append(failures, fmt.Errorf("legacy pricing policy %d target: %w", policy.ID, targetErr))
			continue
		}
		if proposal.Mode != "auto" || target.WriteMode != "auto" {
			continue
		}
		if _, applyErr := s.apply(ctx, proposal.ID, false); applyErr != nil {
			s.recordAutomationFailure(ctx, supplierID, policy.ID, "自动应用价格失败", applyErr)
			failures = append(failures, fmt.Errorf("legacy pricing policy %d apply: %w", policy.ID, applyErr))
		}
	}
	return errors.Join(failures...)
}

func isSharedTargetReadFailure(cause error) bool {
	diagnostic := diagnosticFromError(cause)
	return diagnostic != nil && diagnostic.category == "target_read_failed"
}

func (s *Service) recordTargetReadFailure(ctx context.Context, supplierID, targetID int64, affectedRules int, cause error) {
	if isSilentAutomationFailureCategory(automationFailureCategory(cause)) {
		return
	}
	target, targetErr := s.store.GetTarget(ctx, targetID)
	targetName := fmt.Sprintf("#%d", targetID)
	if targetErr == nil && strings.TrimSpace(target.Name) != "" {
		targetName = target.Name
	}
	category := automationFailureCategory(cause)
	message := fmt.Sprintf("本站「%s」配置暂时读取失败，影响本轮 %d 条价格规则；系统已停止重复请求，将在下次采集后自动重试。原因：%v",
		targetName, affectedRules, cause)
	id := supplierID
	metadata := map[string]string{
		"target_site_id": fmt.Sprint(targetID), "target_site_name": targetName,
		"affected_rule_count": fmt.Sprint(affectedRules), "error_class": category,
		"automation_failure_key": fmt.Sprintf("target-read:%d:%s", targetID, category),
	}
	_ = s.store.AddActivityCoalesced(ctx, domain.Activity{Kind: "automation_target_read_failed", Severity: "warning",
		Title: "本站配置暂时读取失败", Message: message, SupplierID: &id, Metadata: metadata}, 10*time.Minute)
}

// maybeConfirmPriceEvidence performs the narrow fast path requested by the
// operator: when a rule has a real target difference and every other
// automation check passes, collect a small number of additional observations
// immediately. It never changes the consecutive-observation gate and never
// runs more than one confirmation loop for a supplier at a time.
func (s *Service) maybeConfirmPriceEvidence(ctx context.Context, supplierID int64, policies []domain.PricingPolicy) {
	if s.refreshSupplier == nil {
		return
	}
	config, err := s.ConfirmationConfig(ctx)
	if err != nil || !config.Enabled {
		return
	}
	candidates := make([]domain.PricingPolicy, 0, len(policies))
	for _, policy := range policies {
		if !policy.Enabled || policy.SupplierID != supplierID || strings.ToLower(strings.TrimSpace(policy.ExecutionMode)) != "auto" {
			continue
		}
		simulation, simulationErr := s.Simulate(ctx, policy.ID)
		if simulationErr != nil || !simulation.EvidenceOnlyBlocked || simulation.Evidence.RemainingObservations <= 0 ||
			!simulationHasChange(simulation) {
			continue
		}
		candidates = append(candidates, policy)
	}
	if len(candidates) == 0 {
		return
	}
	s.recollectMu.Lock()
	if _, exists := s.recollecting[supplierID]; exists {
		s.recollectMu.Unlock()
		return
	}
	s.recollecting[supplierID] = struct{}{}
	s.recollectMu.Unlock()
	defer func() {
		s.recollectMu.Lock()
		delete(s.recollecting, supplierID)
		s.recollectMu.Unlock()
	}()

	id := supplierID
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "price_confirmation_started", Severity: "info",
		Title: "发现价差，开始定向复采", Message: fmt.Sprintf("%d 条自动规则仍需连续证据；最多补采 %d 次，间隔 %d 秒", len(candidates), config.MaxAttempts, config.IntervalSeconds), SupplierID: &id,
		Metadata: map[string]string{"max_attempts": fmt.Sprint(config.MaxAttempts), "interval_seconds": fmt.Sprint(config.IntervalSeconds)}})
	for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
		if attempt > 1 {
			timer := time.NewTimer(time.Duration(config.IntervalSeconds) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if refreshErr := s.refreshSupplier(ctx, supplierID); refreshErr != nil {
			if isSilentAutomationFailureCategory(automationFailureCategory(refreshErr)) {
				return
			}
			_ = s.store.AddActivity(ctx, domain.Activity{Kind: "price_confirmation_failed", Severity: "warning",
				Title: "定向复采未完成", Message: fmt.Sprintf("第 %d 次复采失败：%v", attempt, refreshErr), SupplierID: &id})
			return
		}
		blocked := false
		for _, policy := range candidates {
			simulation, simulationErr := s.Simulate(ctx, policy.ID)
			if simulationErr == nil && simulation.EvidenceOnlyBlocked && simulation.Evidence.RemainingObservations > 0 &&
				simulationHasChange(simulation) {
				blocked = true
			}
		}
		if !blocked {
			_ = s.store.AddActivity(ctx, domain.Activity{Kind: "price_confirmation_completed", Severity: "success",
				Title: "定向复采已满足证据门禁", Message: fmt.Sprintf("第 %d 次复采后已停止；后续自动规则可按正常门禁执行", attempt), SupplierID: &id,
				Metadata: map[string]string{"attempt": fmt.Sprint(attempt)}})
			return
		}
	}
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "price_confirmation_waiting", Severity: "info",
		Title: "定向复采完成，继续等待稳定证据", Message: fmt.Sprintf("已补采 %d 次，仍未满足连续读数门禁；下一轮按上游常规周期继续", config.MaxAttempts), SupplierID: &id,
		Metadata: map[string]string{"attempts": fmt.Sprint(config.MaxAttempts)}})
}

func (s *Service) Rollback(ctx context.Context, proposalID int64) (domain.ChangeProposal, error) {
	ctx = targets.WithFreshRead(ctx)
	if err := s.remoteWrites.Check("pricing proposal rollback"); err != nil {
		return domain.ChangeProposal{}, err
	}
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	lock := s.targetLock(proposal.TargetSiteID)
	lock.Lock()
	defer lock.Unlock()
	proposal, err = s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	if proposal.Status != "applied" {
		return domain.ChangeProposal{}, fmt.Errorf("only an applied proposal can be rolled back")
	}
	releaseLease, err := s.acquireTargetLease(ctx, proposal.TargetSiteID, fmt.Sprintf("automation:rollback:%d", proposal.ID))
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	defer releaseLease()
	if err := s.store.TransitionProposal(ctx, proposal.ID, []string{"applied"}, "rolling_back", proposal.ReadbackValue, ""); err != nil {
		return domain.ChangeProposal{}, err
	}
	target, err := s.store.GetTarget(ctx, proposal.TargetSiteID)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
		return domain.ChangeProposal{}, err
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
		return domain.ChangeProposal{}, err
	}
	current, err := adapter.Snapshot(ctx, target)
	if err != nil {
		s.InvalidateTargetRead(target.ID)
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
		return domain.ChangeProposal{}, err
	}
	policy, policyErr := s.store.GetPolicy(ctx, proposal.PolicyID)
	if policyErr != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, policyErr.Error())
		return domain.ChangeProposal{}, policyErr
	}
	if strings.EqualFold(strings.TrimSpace(policy.SourceBasis), "group_model_contract") ||
		proposal.GroupModelPricing.Mode == "atomic_contract" {
		groupModelAdapter, adapterErr := targets.AsGroupModelPricingAdapter(adapter)
		if adapterErr != nil {
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, adapterErr.Error())
			return domain.ChangeProposal{}, adapterErr
		}
		expectedApplied := firstNonEmptyString(proposal.GroupModelPricingReceipt.AppliedRevision,
			proposal.GroupModelPricingReceipt.DesiredRevision, proposal.GroupModelPricing.DesiredRevision)
		if current.GroupModelPricing == nil || current.GroupModelPricing.Revision != expectedApplied {
			s.InvalidateTargetRead(target.ID)
			err = errors.New("cannot rollback because the group-model pricing contract was edited after apply")
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
			return domain.ChangeProposal{}, err
		}
		s.InvalidateTargetRead(target.ID)
		readback, rollbackErr := groupModelAdapter.RollbackGroupModelPricing(ctx, target, expectedApplied,
			proposal.GroupModelPricing.ExpectedContract)
		s.InvalidateTargetRead(target.ID)
		if rollbackErr != nil {
			_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, rollbackErr.Error())
			return domain.ChangeProposal{}, rollbackErr
		}
		receipt := proposal.GroupModelPricingReceipt
		receipt.RollbackStatus = "restored revision " + readback.Revision
		if err := s.store.UpdateProposalGroupModelPricingReceipt(ctx, proposal.ID, receipt, []string{"rolling_back"}); err != nil {
			return domain.ChangeProposal{}, err
		}
		if err := s.store.CompleteProposalRollback(ctx, proposal.ID, readback.Revision, readback.Revision, time.Now().UTC()); err != nil {
			return domain.ChangeProposal{}, err
		}
		_ = s.store.AddActivity(ctx, domain.Activity{Kind: "proposal_group_model_pricing_rolled_back", Severity: "success",
			Title: "Per-model pricing change safely rolled back",
			Message: fmt.Sprintf("%s: revision %s -> %s; target still matched this proposal before rollback",
				proposal.TargetGroup, shortRevision(expectedApplied), shortRevision(readback.Revision)),
			Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID), "readback_revision": readback.Revision}})
		return s.store.GetProposal(ctx, proposal.ID)
	}
	if !decimalEqual(current.GroupRatio[proposal.TargetGroup], proposal.ProposedValue) {
		s.InvalidateTargetRead(target.ID)
		err = fmt.Errorf("cannot rollback because this group was edited after apply")
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
		return domain.ChangeProposal{}, err
	}
	previous := cloneRatios(current.GroupRatio)
	if proposal.ExpectedOld == "" {
		delete(previous, proposal.TargetGroup)
	} else {
		previous[proposal.TargetGroup] = proposal.ExpectedOld
	}
	s.InvalidateTargetRead(target.ID)
	readback, err := adapter.RollbackGroupRatio(ctx, target, current.Hash, previous)
	s.InvalidateTargetRead(target.ID)
	if err != nil {
		_ = s.store.TransitionProposal(ctx, proposal.ID, []string{"rolling_back"}, "applied", proposal.ReadbackValue, err.Error())
		return domain.ChangeProposal{}, err
	}
	value := readback.GroupRatio[proposal.TargetGroup]
	if err := s.store.CompleteProposalRollback(ctx, proposal.ID, value, readback.Hash, time.Now().UTC()); err != nil {
		return domain.ChangeProposal{}, err
	}
	_ = s.store.AddActivity(ctx, domain.Activity{Kind: "proposal_rolled_back", Severity: "success",
		Title: "自动价格变更已安全撤回", Message: fmt.Sprintf("%s: %s -> %s；目标仍匹配本次写入值", proposal.TargetGroup, proposal.ProposedValue, value),
		Metadata: map[string]string{"proposal_id": fmt.Sprint(proposal.ID), "readback_hash": readback.Hash}})
	return s.store.GetProposal(ctx, proposal.ID)
}

func shortRevision(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func findGroup(snapshot domain.CatalogSnapshot, name string) (domain.GroupOffer, error) {
	for _, group := range snapshot.Groups {
		if group.ExternalID == name || group.Name == name {
			return group, nil
		}
	}
	return domain.GroupOffer{}, fmt.Errorf("source group %q is not present in the latest catalog", name)
}

func matchSourceModels(group domain.GroupOffer, rawPattern string) ([]string, error) {
	pattern := defaultPattern(rawPattern)
	if len(group.ModelIDs) == 0 {
		if pattern == "*" {
			return []string{}, nil
		}
		return nil, fmt.Errorf("source group %q has no model catalog, so pattern %q cannot be verified", group.Name, pattern)
	}
	patterns := make([]string, 0)
	for _, candidate := range strings.Split(pattern, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			patterns = append(patterns, strings.ToLower(candidate))
		}
	}
	if len(patterns) == 0 {
		patterns = []string{"*"}
	}
	matched := make([]string, 0, len(group.ModelIDs))
	for _, model := range group.ModelIDs {
		for _, candidate := range patterns {
			// A bare star is the explicit "all models in this group" contract.
			// path.Match treats '/' as a separator, so using it for this case
			// incorrectly rejects normal provider/model identifiers such as
			// "openai/gpt-4o".
			if candidate == "*" {
				matched = append(matched, model)
				break
			}
			ok, err := path.Match(candidate, strings.ToLower(model))
			if err != nil {
				return nil, fmt.Errorf("invalid source model pattern %q: %w", candidate, err)
			}
			if ok {
				matched = append(matched, model)
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("source model pattern %q does not match any model in group %q", pattern, group.Name)
	}
	return matched, nil
}

func defaultPattern(pattern string) string {
	if strings.TrimSpace(pattern) == "" {
		return "*"
	}
	return strings.TrimSpace(pattern)
}

func (s *Service) targetLock(targetID int64) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock := s.targetLocks[targetID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.targetLocks[targetID] = lock
	}
	return lock
}

func (s *Service) proposalLock(policyID int64) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if lock, ok := s.proposalLocks[policyID]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	s.proposalLocks[policyID] = lock
	return lock
}

func (s *Service) acquireTargetLease(ctx context.Context, targetID int64, owner string) (func(), error) {
	leased, err := s.store.TryAcquireTargetChangeLease(ctx, targetID, owner, time.Now().UTC(), 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if !leased {
		return nil, errors.New("another target change is currently in progress")
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.ReleaseTargetChangeLease(releaseCtx, targetID, owner)
	}, nil
}

func (s *Service) recordAutomationFailure(ctx context.Context, supplierID, policyID int64, title string, cause error) {
	if isSilentAutomationFailureCategory(automationFailureCategory(cause)) {
		return
	}
	var supplier *int64
	if supplierID > 0 {
		value := supplierID
		supplier = &value
	}
	metadata := map[string]string{"policy_id": fmt.Sprint(policyID)}
	message := cause.Error()
	category := automationFailureCategory(cause)
	if diagnostic := diagnosticFromError(cause); diagnostic != nil {
		metadata = diagnostic.metadata()
		category = diagnostic.category
	} else if policyID > 0 {
		if policy, policyErr := s.store.GetPolicy(ctx, policyID); policyErr == nil {
			metadata["policy_name"] = policy.Name
			metadata["source_group"] = policy.SourceGroup
			metadata["source_model_pattern"] = defaultPattern(policy.SourceModelPattern)
			metadata["target_site_id"] = fmt.Sprint(policy.TargetSiteID)
			metadata["target_group"] = policy.TargetGroup
			if upstream, upstreamErr := s.store.GetSupplier(ctx, policy.SupplierID); upstreamErr == nil {
				metadata["supplier_name"] = upstream.Name
			}
			if target, targetErr := s.store.GetTarget(ctx, policy.TargetSiteID); targetErr == nil {
				metadata["target_site_name"] = target.Name
			}
			message = fmt.Sprintf("规则 #%d「%s」；上游 %s / 分组 %s；本站 %s / 分组 %s；原因：%v",
				policy.ID, displayDiagnosticValue(policy.Name), displayDiagnosticValue(metadata["supplier_name"]),
				displayDiagnosticValue(policy.SourceGroup), displayDiagnosticValue(metadata["target_site_name"]),
				displayDiagnosticValue(policy.TargetGroup), cause)
		}
	}
	metadata["error_class"] = category
	if metadata["automation_failure_key"] == "" {
		metadata["automation_failure_key"] = automationFailureKey(policyID, title, category)
	}
	_ = s.store.AddActivityCoalesced(ctx, domain.Activity{Kind: "automation_failed", Severity: "warning", Title: title,
		Message: message, SupplierID: supplier, Metadata: metadata}, 10*time.Minute)
}

func isAutomationWaitingCondition(cause error) bool {
	diagnostic := diagnosticFromError(cause)
	if diagnostic == nil {
		return false
	}
	switch diagnostic.category {
	case "source_group_missing", "source_ratio_unavailable":
		return true
	default:
		return false
	}
}

func (s *Service) recordAutomationWaiting(ctx context.Context, supplierID, policyID int64, cause error) {
	diagnostic := diagnosticFromError(cause)
	if diagnostic == nil {
		return
	}
	metadata := diagnostic.metadata()
	metadata["waiting_key"] = diagnostic.coalesceKey()
	message := fmt.Sprintf("规则 #%d「%s」暂不改价：上游分组 %s 当前没有可用倍率；恢复采集后会自动继续。模型增删由分组镜像同步。",
		diagnostic.policy.ID, displayDiagnosticValue(diagnostic.policy.Name),
		displayDiagnosticValue(firstNonBlank(diagnostic.sourceGroup.Name, diagnostic.policy.SourceGroup)))
	var supplier *int64
	if supplierID > 0 {
		value := supplierID
		supplier = &value
	}
	_ = s.store.AddActivityCoalesced(ctx, domain.Activity{Kind: "pricing_mapping_waiting", Severity: "info",
		Title: "价格映射等待上游倍率", Message: message, SupplierID: supplier, Metadata: metadata}, 6*time.Hour)
}

func automationFailureCategory(cause error) string {
	if cause == nil {
		return "unknown"
	}
	switch status := targets.RemoteHTTPStatus(cause); {
	case status == http.StatusUnauthorized:
		return "authorization"
	case status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= http.StatusInternalServerError && status <= 599:
		return "upstream_5xx"
	}
	message := strings.ToLower(cause.Error())
	switch {
	case errors.Is(cause, context.DeadlineExceeded), strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return "timeout"
	case strings.Contains(message, "401"), strings.Contains(message, "unauthorized"), strings.Contains(message, "auth"):
		return "authorization"
	case strings.Contains(message, "403"), strings.Contains(message, "permission"):
		return "permission_denied"
	case strings.Contains(message, "429"), strings.Contains(message, "rate limit"):
		return "rate_limited"
	case strings.Contains(message, "502"), strings.Contains(message, "503"), strings.Contains(message, "504"),
		strings.Contains(message, "bad gateway"), strings.Contains(message, "service unavailable"):
		return "upstream_5xx"
	case strings.Contains(message, "not present"), strings.Contains(message, "no rows"):
		return "not_found"
	case strings.Contains(message, "connection"), strings.Contains(message, "network"), strings.Contains(message, "dns"),
		strings.Contains(message, "no such host"), strings.Contains(message, "tls"), strings.Contains(message, "x509"),
		strings.Contains(message, "handshake"), strings.Contains(message, "unexpected eof"):
		return "network"
	default:
		return "automation_error"
	}
}

func isSilentAutomationFailureCategory(category string) bool {
	switch category {
	case "timeout", "rate_limited", "upstream_5xx", "network":
		return true
	default:
		return false
	}
}

func automationFailureKey(policyID int64, title, category string) string {
	value := fmt.Sprintf("%d\x00%s\x00%s", policyID, title, category)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func proposalKey(simulation domain.PolicySimulation) string {
	value := "v3|" + simulation.Calculation.InputFingerprint + "|" + simulation.Calculation.TargetRatio
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func pricingImpactHash(sourceHash, targetHash string, vector map[string]string) string {
	keys := make([]string, 0, len(vector))
	for key := range vector {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(sourceHash)
	builder.WriteByte('\n')
	builder.WriteString(targetHash)
	builder.WriteByte('\n')
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(vector[key])
		builder.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func evaluateCatalogEvidence(status domain.CatalogEvidenceStatus, sourceType string) domain.CatalogEvidenceStatus {
	status.SourceType = strings.ToLower(strings.TrimSpace(sourceType))
	status.RequiredObservations = requiredCatalogObservations(status.SourceType)
	status.RemainingObservations = status.RequiredObservations - status.ConsecutiveObservations
	if status.RemainingObservations < 0 {
		status.RemainingObservations = 0
	}
	status.AutoEligible = status.ConsecutiveObservations >= status.RequiredObservations
	return status
}

func requiredCatalogObservations(sourceType string) int {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "authenticated_api", "authenticated_admin_api", "authenticated_api_key", "authenticated_declared_json_api":
		return 1
	case "public_api", "declared_json_api":
		return 2
	default:
		return 3
	}
}

func appendReason(existing, message string) string {
	if strings.TrimSpace(existing) == "" {
		return message
	}
	return existing + "；" + message
}

func cloneRatios(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func policyFreshnessSeconds(policy domain.PricingPolicy) int {
	return 300
}

func displayEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未设置"
	}
	return canonicalDisplayDecimal(value)
}

func canonicalDisplayDecimal(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if parsed, err := decimal.NewFromString(trimmed); err == nil {
		return decimalnorm.Functional(parsed).String()
	}
	return trimmed
}

func decimalEqual(left, right string) bool {
	return decimalnorm.Equal(left, right)
}
