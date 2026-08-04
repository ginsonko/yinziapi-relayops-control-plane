package opportunity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type parsedConfig struct {
	recent7, recent30, priorDemand, priorConfidence decimal.Decimal
	minimumConfidence, minimumAvailability          decimal.Decimal
	minimumCoverage, demandLower, priorLower        decimal.Decimal
	costUpperRisk, monthlyDays                      decimal.Decimal
}

type recommendationAdjustment struct {
	modelIndex       int
	baseRevenue      decimal.Decimal
	originalRevenue  decimal.Decimal
	baseLowerRevenue decimal.Decimal
	originalLower    decimal.Decimal
}

func Evaluate(candidate Candidate, config Config) Result {
	now := time.Now().UTC()
	result := Result{ID: candidate.ID, SupplierID: candidate.SupplierID, Supplier: candidate.Supplier, Group: candidate.Group,
		Classification: ClassificationInsufficient, ModelIDs: []string{}, ModelFamilies: []string{}, Models: []ModelResult{},
		Warnings: []string{}, HardFilterReasons: []string{}, ObservedAt: candidate.ObservedAt, EvaluatedAt: now}
	result.InputHash = inputHash(candidate, config)
	cfg, err := parseConfig(config)
	if err != nil {
		result.HardFilterReasons = append(result.HardFilterReasons, "收益模型配置无效")
		result.Reason = "收益模型配置需要管理员修正后才能计算"
		result.Warnings = append(result.Warnings, err.Error())
		return result
	}

	result.HardFilterReasons = hardFilterReasons(candidate)
	if len(result.HardFilterReasons) > 0 {
		result.Reason = result.HardFilterReasons[0]
		return result
	}

	availability, availabilityKnown := optionalUnitInterval(candidate.Availability)
	if !availabilityKnown {
		availability = decimal.NewFromFloat(0.5)
		result.Warnings = append(result.Warnings, "上游可用性尚无完整样本，使用保守折扣")
	}
	availabilityLower, lowerKnown := optionalUnitInterval(candidate.AvailabilityLower)
	if !lowerKnown || availabilityLower.GreaterThan(availability) {
		availabilityLower = decimal.Max(decimal.Zero, availability.Sub(decimal.NewFromFloat(0.15)))
	}
	evidenceConfidence, confidenceKnown := optionalUnitInterval(candidate.EvidenceConfidence)
	if !confidenceKnown {
		evidenceConfidence = decimal.NewFromFloat(0.4)
		result.Warnings = append(result.Warnings, "目录证据置信度未知，使用保守置信度")
	}

	totalProfit := decimal.Zero
	totalSaving := decimal.Zero
	totalRevenue := decimal.Zero
	weightedTargetBaseRevenue := decimal.Zero
	recommendationAdjustments := make([]recommendationAdjustment, 0)
	recommendationModels := 0
	unitMismatchModels := 0
	totalLower := decimal.Zero
	coverageNumerator := decimal.Zero
	coverageDenominator := decimal.Zero
	confidenceWeighted := decimal.Zero
	confidenceWeight := decimal.Zero
	currency := ""
	currencyConflict := false
	comparableModels := 0
	recognizedModels := 0
	familySet := map[string]struct{}{}

	for _, model := range candidate.Models {
		if !model.Recognized || strings.TrimSpace(model.ModelID) == "" {
			continue
		}
		recognizedModels++
		result.ModelIDs = append(result.ModelIDs, model.ModelID)
		family := strings.TrimSpace(model.Family)
		if family != "" {
			familySet[family] = struct{}{}
		}
		modelCurrency := strings.ToUpper(strings.TrimSpace(model.Currency))
		if modelCurrency != "" {
			if currency == "" {
				currency = modelCurrency
			} else if currency != modelCurrency {
				currencyConflict = true
			}
		}

		revenue, revenueOK := optionalNonNegative(model.SalesUnitPrice)
		cost, costOK := optionalNonNegative(model.CandidateUnitCost)
		baseline, baselineOK := optionalNonNegative(model.CurrentUnitCost)
		if !revenueOK || !costOK || !baselineOK || currencyConflict {
			continue
		}
		fees, feesOK := optionalNonNegativeDefault(model.FeesUnit)
		retryLoss, retryOK := optionalNonNegativeDefault(model.RetryLossUnit)
		if !feesOK || !retryOK {
			continue
		}
		costUpper, costUpperOK := optionalNonNegative(model.CandidateCostUpper)
		if !costUpperOK {
			costUpper = cost.Mul(decimal.NewFromInt(1).Add(cfg.costUpperRisk))
		}
		if costUpper.LessThan(cost) {
			costUpper = cost
		}

		demand, demandLower, demandConfidence, evidenceKind, demandOK := resolveDailyDemand(model, cfg)
		if !demandOK {
			continue
		}
		if demand.IsPositive() && evidenceKind == "observed" && !compatibleDemandUnit(model.DemandUnit, model.PriceUnit) {
			unitMismatchModels++
			result.Warnings = append(result.Warnings, fmt.Sprintf("模型 %s 的用量单位 %s 与价格单位 %s 不一致，已从利润计算中排除", model.ModelID,
				displayDemandUnit(model.DemandUnit), displayDemandUnit(model.PriceUnit)))
			continue
		}
		if evidenceKind == "prior" {
			result.UsesPrior = true
		}
		quality := decimal.Min(evidenceConfidence, demandConfidence)
		qualityLower := quality.Mul(decimal.NewFromFloat(0.8))
		unitProfit := revenue.Sub(cost).Sub(fees).Sub(retryLoss)
		unitSaving := decimal.Max(decimal.Zero, baseline.Sub(cost))
		modelProfit := demand.Mul(unitProfit).Mul(availability).Mul(quality)
		modelSaving := demand.Mul(unitSaving).Mul(availability).Mul(quality)
		lowerUnitProfit := revenue.Sub(costUpper).Sub(fees).Sub(retryLoss)
		lowerDemand := demandLower
		if lowerUnitProfit.IsNegative() {
			lowerDemand = decimal.Max(demand, demandLower).Mul(decimal.NewFromFloat(1.25))
		}
		modelLower := lowerDemand.Mul(lowerUnitProfit).Mul(availabilityLower).Mul(qualityLower)
		modelRevenue := demand.Mul(revenue).Mul(availability).Mul(quality)
		if targetRatio, ratioOK := optionalNonNegative(model.TargetSalesRatio); ratioOK && targetRatio.IsPositive() {
			lowerRevenue := lowerDemand.Mul(revenue).Mul(availabilityLower).Mul(qualityLower)
			weightedTargetBaseRevenue = weightedTargetBaseRevenue.Add(modelRevenue.Div(targetRatio))
			recommendationAdjustments = append(recommendationAdjustments, recommendationAdjustment{
				modelIndex: len(result.Models), baseRevenue: modelRevenue.Div(targetRatio), originalRevenue: modelRevenue,
				baseLowerRevenue: lowerRevenue.Div(targetRatio), originalLower: lowerRevenue,
			})
			recommendationModels++
		}

		totalProfit = totalProfit.Add(modelProfit)
		totalSaving = totalSaving.Add(modelSaving)
		totalLower = totalLower.Add(modelLower)
		totalRevenue = totalRevenue.Add(modelRevenue)
		comparableModels++
		compared := model.ComparableDimensions
		total := model.TotalDimensions
		if total <= 0 {
			total = 1
		}
		if compared <= 0 {
			compared = 1
		}
		coverageNumerator = coverageNumerator.Add(decimal.NewFromInt(int64(compared)))
		coverageDenominator = coverageDenominator.Add(decimal.NewFromInt(int64(total)))
		confidenceWeighted = confidenceWeighted.Add(quality.Mul(demand))
		confidenceWeight = confidenceWeight.Add(demand)
		result.Models = append(result.Models, ModelResult{ModelID: model.ModelID, Family: family, DailyDemand: canonical(demand),
			ExpectedDailyProfit: canonical(modelProfit), ExpectedDailySaving: canonical(modelSaving),
			ConservativeLowerBound: canonical(modelLower), ComparableCoverage: canonical(decimal.NewFromInt(int64(compared)).Div(decimal.NewFromInt(int64(total)))),
			DemandEvidence: evidenceKind, DemandUnit: model.DemandUnit, PriceUnit: model.PriceUnit})
	}

	result.ModelIDs = uniqueSorted(result.ModelIDs)
	for family := range familySet {
		result.ModelFamilies = append(result.ModelFamilies, family)
	}
	sort.Strings(result.ModelFamilies)
	if recognizedModels == 0 {
		result.HardFilterReasons = append(result.HardFilterReasons, "分组没有可识别模型")
		result.Reason = "分组没有可识别模型，已从建议列表过滤"
		return result
	}
	if comparableModels == 0 || currencyConflict {
		result.HardFilterReasons = append(result.HardFilterReasons, "成本、币种或价格函数不可比较")
		if unitMismatchModels > 0 {
			result.Reason = "本站用量与价格函数单位不一致，已停止生成可能失真的利润数字"
		} else {
			result.Reason = "当前证据不足以诚实计算收益"
		}
		return result
	}

	coverage := decimal.Zero
	if coverageDenominator.IsPositive() {
		coverage = decimal.Min(decimal.NewFromInt(1), coverageNumerator.Div(coverageDenominator))
	}
	confidence := evidenceConfidence
	if confidenceWeight.IsPositive() {
		confidence = decimal.Min(evidenceConfidence, confidenceWeighted.Div(confidenceWeight))
	}
	if recommendationModels == comparableModels && weightedTargetBaseRevenue.IsPositive() {
		step := decimal.RequireFromString("0.000001")
		recommendedRatio := totalRevenue.Div(weightedTargetBaseRevenue).Div(step).Ceil().Mul(step)
		for _, adjustment := range recommendationAdjustments {
			adjustedRevenue := adjustment.baseRevenue.Mul(recommendedRatio)
			deltaRevenue := adjustedRevenue.Sub(adjustment.originalRevenue)
			totalRevenue = totalRevenue.Add(deltaRevenue)
			totalProfit = totalProfit.Add(deltaRevenue)
			adjustedLowerRevenue := adjustment.baseLowerRevenue.Mul(recommendedRatio)
			deltaLower := adjustedLowerRevenue.Sub(adjustment.originalLower)
			totalLower = totalLower.Add(deltaLower)
			modelProfit, _ := decimal.NewFromString(result.Models[adjustment.modelIndex].ExpectedDailyProfit)
			modelLower, _ := decimal.NewFromString(result.Models[adjustment.modelIndex].ConservativeLowerBound)
			result.Models[adjustment.modelIndex].ExpectedDailyProfit = canonical(modelProfit.Add(deltaRevenue))
			result.Models[adjustment.modelIndex].ConservativeLowerBound = canonical(modelLower.Add(deltaLower))
		}
		result.RecommendedRuleType = "fixed"
		result.RecommendedRuleValue = canonical(recommendedRatio)
		result.RecommendedRounding = canonical(step)
		result.RecommendationBasis = "demand_weighted_current_minimum_sale_ratio_v1"
		result.RecommendationNote = "该固定倍率按本站各模型近期需求、可用性和证据权重反推，并按规则步长向上量化；只有使用此规则快照时，上方利润预测才可直接比较"
	} else {
		result.Warnings = append(result.Warnings, "目标销售倍率证据不完整，暂不生成可执行的推荐映射规则")
	}
	result.Eligible = true
	result.ExpectedDailyProfit = canonical(totalProfit)
	result.ExpectedMonthlyProfit = canonical(totalProfit.Mul(cfg.monthlyDays))
	result.ExpectedDailySaving = canonical(totalSaving)
	result.ExpectedMonthlySaving = canonical(totalSaving.Mul(cfg.monthlyDays))
	result.ConservativeLowerBound = canonical(totalLower)
	result.ModelCoverage = canonical(coverage)
	result.Availability = canonical(availability)
	result.Confidence = canonical(confidence)
	result.Currency = currency
	if totalRevenue.IsPositive() {
		result.GrossMargin = canonical(totalProfit.Div(totalRevenue))
	}

	if totalProfit.IsNegative() {
		required := totalProfit.Neg()
		result.SubsidyRequiredDaily = canonical(required)
		result.SubsidyBudgetDaily = strings.TrimSpace(candidate.SubsidyBudgetDaily)
		if budget, ok := optionalNonNegative(candidate.SubsidyBudgetDaily); ok && candidate.AllowSubsidy && budget.GreaterThanOrEqual(required) {
			result.SubsidyWithinBudget = true
			result.Classification = ClassificationWatch
			result.Reason = fmt.Sprintf("公益补贴预算可覆盖每日 %s %s 的预计缺口，保留为人工确认机会", canonical(required), currency)
			return result
		}
	}

	evidencePass := confidence.GreaterThanOrEqual(cfg.minimumConfidence) && availability.GreaterThanOrEqual(cfg.minimumAvailability) && coverage.GreaterThanOrEqual(cfg.minimumCoverage) && !result.UsesPrior
	switch {
	case totalLower.IsPositive() && totalSaving.IsPositive() && evidencePass:
		result.Classification = ClassificationRecommended
		result.Reason = fmt.Sprintf("保守收益下界仍为正，预计每日贡献利润 %s %s", canonical(totalProfit), currency)
	case totalProfit.IsPositive() && totalSaving.IsPositive():
		result.Classification = ClassificationWatch
		if result.UsesPrior {
			result.Reason = "具备成本或利润空间，等待本站真实用量后提升置信度"
		} else if availability.LessThan(cfg.minimumAvailability) {
			result.Reason = "具备成本或利润空间，当前可用性仍需观察"
		} else {
			result.Reason = "具备潜在价值，但证据门槛或保守收益下界尚未通过"
		}
	default:
		result.Classification = ClassificationNoAdvantage
		result.Reason = "按当前需求、售价和采购成本计算暂无收益优势"
	}
	return result
}

func compatibleDemandUnit(demandUnit, priceUnit string) bool {
	demandUnit = strings.ToLower(strings.TrimSpace(demandUnit))
	priceUnit = strings.ToLower(strings.TrimSpace(priceUnit))
	if demandUnit == "" && priceUnit == "" {
		return true
	}
	return demandUnit != "" && demandUnit == priceUnit
}

func displayDemandUnit(unit string) string {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "million_tokens":
		return "百万 Token"
	case "requests":
		return "请求次数"
	case "images":
		return "图片数"
	case "seconds":
		return "秒数"
	case "frames":
		return "帧数"
	case "mixed":
		return "混合单位"
	case "normalized_demand":
		return "归一化需求"
	default:
		return "未知单位"
	}
}

func EvaluateAll(candidates []Candidate, config Config) []Result {
	results := make([]Result, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		fingerprint := strings.ToLower(strings.TrimSpace(candidate.EntityFingerprint))
		if fingerprint != "" {
			if _, exists := seen[fingerprint]; exists {
				candidate.DuplicateFingerprint = true
			} else {
				seen[fingerprint] = struct{}{}
			}
		}
		results = append(results, Evaluate(candidate, config))
	}
	sort.SliceStable(results, func(i, j int) bool {
		leftClass, rightClass := classRank(results[i].Classification), classRank(results[j].Classification)
		if leftClass != rightClass {
			return leftClass < rightClass
		}
		leftProfit, _ := decimal.NewFromString(defaultZero(results[i].ExpectedDailyProfit))
		rightProfit, _ := decimal.NewFromString(defaultZero(results[j].ExpectedDailyProfit))
		if !leftProfit.Equal(rightProfit) {
			return leftProfit.GreaterThan(rightProfit)
		}
		leftSaving, _ := decimal.NewFromString(defaultZero(results[i].ExpectedDailySaving))
		rightSaving, _ := decimal.NewFromString(defaultZero(results[j].ExpectedDailySaving))
		if !leftSaving.Equal(rightSaving) {
			return leftSaving.GreaterThan(rightSaving)
		}
		return results[i].ID < results[j].ID
	})
	return results
}

func NormalizeConfig(config Config) Config {
	return withDefaults(config, DefaultConfig())
}

func ValidateConfig(config Config) error {
	config = NormalizeConfig(config)
	if _, err := parseConfig(config); err != nil {
		return err
	}
	for name, raw := range map[string]string{"fee rate": config.FeeRate, "retry loss rate": config.RetryLossRate} {
		value, ok := optionalUnitInterval(raw)
		if !ok || value.IsNegative() {
			return fmt.Errorf("%s must be a decimal between 0 and 1", name)
		}
	}
	if config.TargetCatalogTTLSeconds < 30 || config.TargetCatalogTTLSeconds > 86400 {
		return fmt.Errorf("target catalog TTL must be between 30 and 86400 seconds")
	}
	if config.UsageTTLSeconds < 30 || config.UsageTTLSeconds > 86400 {
		return fmt.Errorf("usage TTL must be between 30 and 86400 seconds")
	}
	return nil
}

func hardFilterReasons(candidate Candidate) []string {
	reasons := make([]string, 0, 4)
	if strings.TrimSpace(candidate.Group) == "" || len(candidate.Models) == 0 {
		reasons = append(reasons, "上游分组为空或没有模型")
	}
	if !candidate.TargetIntersection && !candidate.CreateEligible {
		reasons = append(reasons, "与本站模型目录没有交集，且未允许创建该模型")
	}
	if candidate.DuplicateFingerprint {
		reasons = append(reasons, "相同实体指纹的发现已存在")
	}
	if candidate.Blacklisted {
		reasons = append(reasons, "候选已被管理员加入黑名单")
	}
	if candidate.Unavailable {
		reasons = append(reasons, "候选凭据或协议当前不可用")
	}
	return reasons
}

func resolveDailyDemand(model ModelInput, cfg parsedConfig) (decimal.Decimal, decimal.Decimal, decimal.Decimal, string, bool) {
	d7, has7 := optionalNonNegative(model.Demand7D)
	d30, has30 := optionalNonNegative(model.Demand30D)
	confidence, hasConfidence := optionalUnitInterval(model.DemandConfidence)
	if !hasConfidence {
		confidence = decimal.NewFromFloat(0.75)
	}
	if has7 || has30 {
		var daily decimal.Decimal
		if has7 && has30 {
			daily = d7.Div(decimal.NewFromInt(7)).Mul(cfg.recent7).Add(d30.Div(decimal.NewFromInt(30)).Mul(cfg.recent30))
		} else if has7 {
			daily = d7.Div(decimal.NewFromInt(7))
			confidence = confidence.Mul(decimal.NewFromFloat(0.85))
		} else {
			daily = d30.Div(decimal.NewFromInt(30))
			confidence = confidence.Mul(decimal.NewFromFloat(0.8))
		}
		return daily, daily.Mul(cfg.demandLower), confidence, "observed", true
	}
	prior := cfg.priorDemand
	if value, ok := optionalNonNegative(model.PriorDailyDemand); ok {
		prior = value
	}
	if prior.IsNegative() {
		return decimal.Zero, decimal.Zero, decimal.Zero, "unknown", false
	}
	return prior, prior.Mul(cfg.priorLower), cfg.priorConfidence, "prior", true
}

func parseConfig(config Config) (parsedConfig, error) {
	defaults := DefaultConfig()
	config = withDefaults(config, defaults)
	values := []struct {
		name, raw string
		dst       *decimal.Decimal
	}{
		{"7-day weight", config.RecentWeight7D, nil}, {"30-day weight", config.RecentWeight30D, nil},
		{"prior demand", config.PriorDailyDemand, nil}, {"prior confidence", config.PriorConfidence, nil},
		{"minimum confidence", config.MinimumConfidence, nil}, {"minimum availability", config.MinimumAvailability, nil},
		{"minimum coverage", config.MinimumCoverage, nil}, {"demand lower factor", config.DemandLowerFactor, nil},
		{"prior lower factor", config.PriorLowerFactor, nil}, {"cost upper risk rate", config.CostUpperRiskRate, nil},
		{"monthly days", config.MonthlyDays, nil},
	}
	parsed := make([]decimal.Decimal, len(values))
	for index := range values {
		value, err := decimal.NewFromString(values[index].raw)
		if err != nil || value.IsNegative() {
			return parsedConfig{}, fmt.Errorf("%s must be a non-negative decimal", values[index].name)
		}
		parsed[index] = value
	}
	if !parsed[0].Add(parsed[1]).Equal(decimal.NewFromInt(1)) {
		return parsedConfig{}, fmt.Errorf("7-day and 30-day weights must sum to 1")
	}
	for _, index := range []int{3, 4, 5, 6, 7, 8, 9} {
		if parsed[index].GreaterThan(decimal.NewFromInt(1)) {
			return parsedConfig{}, fmt.Errorf("%s must be between 0 and 1", values[index].name)
		}
	}
	if !parsed[10].IsPositive() {
		return parsedConfig{}, fmt.Errorf("monthly days must be positive")
	}
	return parsedConfig{recent7: parsed[0], recent30: parsed[1], priorDemand: parsed[2], priorConfidence: parsed[3],
		minimumConfidence: parsed[4], minimumAvailability: parsed[5], minimumCoverage: parsed[6], demandLower: parsed[7],
		priorLower: parsed[8], costUpperRisk: parsed[9], monthlyDays: parsed[10]}, nil
}

func withDefaults(value, defaults Config) Config {
	fields := []*string{&value.RecentWeight7D, &value.RecentWeight30D, &value.PriorDailyDemand, &value.PriorConfidence,
		&value.MinimumConfidence, &value.MinimumAvailability, &value.MinimumCoverage, &value.DemandLowerFactor,
		&value.PriorLowerFactor, &value.CostUpperRiskRate, &value.MonthlyDays}
	defaultFields := []string{defaults.RecentWeight7D, defaults.RecentWeight30D, defaults.PriorDailyDemand, defaults.PriorConfidence,
		defaults.MinimumConfidence, defaults.MinimumAvailability, defaults.MinimumCoverage, defaults.DemandLowerFactor,
		defaults.PriorLowerFactor, defaults.CostUpperRiskRate, defaults.MonthlyDays}
	for index := range fields {
		if strings.TrimSpace(*fields[index]) == "" {
			*fields[index] = defaultFields[index]
		}
	}
	if strings.TrimSpace(value.FeeRate) == "" {
		value.FeeRate = defaults.FeeRate
	}
	if strings.TrimSpace(value.RetryLossRate) == "" {
		value.RetryLossRate = defaults.RetryLossRate
	}
	if value.AllowedModelFamilies == nil {
		value.AllowedModelFamilies = append([]string(nil), defaults.AllowedModelFamilies...)
	}
	if value.BlacklistedEntities == nil {
		value.BlacklistedEntities = append([]string(nil), defaults.BlacklistedEntities...)
	}
	if value.TargetCatalogTTLSeconds <= 0 {
		value.TargetCatalogTTLSeconds = defaults.TargetCatalogTTLSeconds
	}
	if value.UsageTTLSeconds <= 0 {
		value.UsageTTLSeconds = defaults.UsageTTLSeconds
	}
	return value
}

func optionalNonNegative(raw string) (decimal.Decimal, bool) {
	if strings.TrimSpace(raw) == "" {
		return decimal.Zero, false
	}
	value, err := decimal.NewFromString(strings.TrimSpace(raw))
	return value, err == nil && !value.IsNegative()
}

func optionalNonNegativeDefault(raw string) (decimal.Decimal, bool) {
	if strings.TrimSpace(raw) == "" {
		return decimal.Zero, true
	}
	return optionalNonNegative(raw)
}

func optionalUnitInterval(raw string) (decimal.Decimal, bool) {
	value, ok := optionalNonNegative(raw)
	return value, ok && !value.GreaterThan(decimal.NewFromInt(1))
}

func canonical(value decimal.Decimal) string { return value.String() }

func inputHash(candidate Candidate, config Config) string {
	payload, _ := json.Marshal(struct {
		Candidate Candidate `json:"candidate"`
		Config    Config    `json:"config"`
	}{candidate, config})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func classRank(classification string) int {
	switch classification {
	case ClassificationRecommended:
		return 0
	case ClassificationWatch:
		return 1
	case ClassificationNoAdvantage:
		return 2
	default:
		return 3
	}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func defaultZero(value string) string {
	if strings.TrimSpace(value) == "" {
		return "0"
	}
	return value
}
