package automation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
)

const diagnosticModelSampleLimit = 5

type policyDiagnosticError struct {
	prefix                  string
	category                string
	action                  string
	policy                  domain.PricingPolicy
	supplier                domain.Supplier
	target                  domain.TargetSite
	sourceGroup             domain.GroupOffer
	matchedModels           []string
	missingTargetModels     []string
	targetCatalogStatus     string
	targetCatalogModelCount int
	targetGroupExists       bool
	impact                  pricing.PricingImpact
	cause                   error
}

func (e *policyDiagnosticError) Error() string {
	prefix := strings.TrimSpace(e.prefix)
	if prefix == "" {
		prefix = "pricing rule cannot be recalculated"
	}
	sourceGroup := firstNonBlank(e.sourceGroup.Name, e.policy.SourceGroup)
	if e.sourceGroup.ExternalID != "" && e.sourceGroup.ExternalID != sourceGroup {
		sourceGroup += " (#" + e.sourceGroup.ExternalID + ")"
	}
	targetCatalog := e.targetCatalogStatus
	if targetCatalog == "" {
		targetCatalog = "未读取"
	}
	targetGroupStatus := "当前不存在（规则应用时会创建）"
	if e.targetGroupExists {
		targetGroupStatus = "已存在"
	}
	message := fmt.Sprintf(
		"%s：规则 #%d「%s」；上游 #%d %s（%s）/ 分组 %s；模型模式 %q，当前匹配 %d 个%s；本站 #%d %s（%s）/ 分组 %s（%s）；目标模型目录 %s，共 %d 个模型；诊断 %s；原因：%v；建议：%s",
		prefix, e.policy.ID, displayDiagnosticValue(e.policy.Name), e.supplier.ID, displayDiagnosticValue(e.supplier.Name),
		displayDiagnosticValue(e.supplier.BaseURL), displayDiagnosticValue(sourceGroup), defaultPattern(e.policy.SourceModelPattern),
		len(e.matchedModels), modelSamplesSuffix(e.matchedModels), e.target.ID, displayDiagnosticValue(e.target.Name),
		displayDiagnosticValue(e.target.BaseURL), displayDiagnosticValue(e.policy.TargetGroup), targetGroupStatus,
		targetCatalog, e.targetCatalogModelCount, diagnosticCategoryLabel(e.category), e.cause, e.action,
	)
	if len(e.missingTargetModels) > 0 {
		message += "；本站目录缺少模型：" + strings.Join(sampleStrings(e.missingTargetModels, diagnosticModelSampleLimit), "、")
	}
	if e.impact.TotalDimensions > 0 || e.impact.ComparableDimensions > 0 {
		message += fmt.Sprintf("；可比价格维度 %d/%d", e.impact.ComparableDimensions, e.impact.TotalDimensions)
	}
	return message
}

func (e *policyDiagnosticError) Unwrap() error { return e.cause }

func (e *policyDiagnosticError) metadata() map[string]string {
	metadata := map[string]string{
		"diagnostic_version":         "pricing-rule-v1",
		"diagnostic_category":        e.category,
		"policy_id":                  strconv.FormatInt(e.policy.ID, 10),
		"policy_name":                e.policy.Name,
		"supplier_id":                strconv.FormatInt(e.supplier.ID, 10),
		"supplier_name":              e.supplier.Name,
		"source_group":               e.policy.SourceGroup,
		"source_group_name":          e.sourceGroup.Name,
		"source_group_id":            e.sourceGroup.ExternalID,
		"source_model_pattern":       defaultPattern(e.policy.SourceModelPattern),
		"matched_model_count":        strconv.Itoa(len(e.matchedModels)),
		"matched_model_samples":      strings.Join(sampleStrings(e.matchedModels, diagnosticModelSampleLimit), ", "),
		"target_site_id":             strconv.FormatInt(e.target.ID, 10),
		"target_site_name":           e.target.Name,
		"target_group":               e.policy.TargetGroup,
		"target_group_exists":        strconv.FormatBool(e.targetGroupExists),
		"target_catalog_status":      e.targetCatalogStatus,
		"target_catalog_model_count": strconv.Itoa(e.targetCatalogModelCount),
		"missing_target_models":      strings.Join(sampleStrings(e.missingTargetModels, diagnosticModelSampleLimit), ", "),
		"comparable_dimensions":      strconv.Itoa(e.impact.ComparableDimensions),
		"total_dimensions":           strconv.Itoa(e.impact.TotalDimensions),
		"suggested_action":           e.action,
	}
	metadata["automation_failure_key"] = e.coalesceKey()
	return metadata
}

func (e *policyDiagnosticError) coalesceKey() string {
	value := strings.Join([]string{strconv.FormatInt(e.policy.ID, 10), e.prefix, e.category, e.policy.SourceGroup,
		defaultPattern(e.policy.SourceModelPattern), strconv.FormatInt(e.target.ID, 10), e.policy.TargetGroup}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newPricingImpactDiagnosticError(policy domain.PricingPolicy, supplier domain.Supplier, target domain.TargetSite,
	group domain.GroupOffer, matchedModels []string, state targetReadState, impact pricing.PricingImpact, cause error) error {
	category, action, missing := classifyPricingImpactFailure(group, matchedModels, state, impact, cause)
	status := "可读"
	if state.catalogErr != nil {
		status = "不可读"
	} else if len(state.catalog.Models) == 0 {
		status = "为空"
	}
	_, targetGroupExists := state.snapshot.GroupRatio[policy.TargetGroup]
	return &policyDiagnosticError{prefix: "pricing impact cannot be verified", category: category, action: action,
		policy: policy, supplier: supplier, target: target, sourceGroup: group, matchedModels: sortedUniqueStrings(matchedModels),
		missingTargetModels: missing, targetCatalogStatus: status, targetCatalogModelCount: len(state.catalog.Models),
		targetGroupExists: targetGroupExists, impact: impact, cause: cause}
}

func newRuleContextError(prefix, category, action string, policy domain.PricingPolicy, supplier domain.Supplier,
	target domain.TargetSite, group domain.GroupOffer, matchedModels []string, cause error) error {
	return &policyDiagnosticError{prefix: prefix, category: category, action: action, policy: policy, supplier: supplier,
		target: target, sourceGroup: group, matchedModels: sortedUniqueStrings(matchedModels),
		targetCatalogStatus: "未读取", cause: cause}
}

func classifyPricingImpactFailure(group domain.GroupOffer, matchedModels []string, state targetReadState,
	impact pricing.PricingImpact, cause error) (category, action string, missing []string) {
	switch {
	case len(group.ModelIDs) == 0:
		return "source_group_models_empty", "重新采集上游；若分组仍为空，检查上游是否下架了模型，或暂停/编辑这条规则", nil
	case len(matchedModels) == 0:
		return "source_model_pattern_unmatched", "检查模型匹配模式与上游分组现有模型；上游恢复后重新采集即可自动重算", nil
	case state.catalogErr != nil:
		return "target_catalog_unavailable", "检查本站管理授权和模型定价目录接口；修复后重新计算，不要手工猜测价格", nil
	}
	missing = missingModels(state.catalog.Models, matchedModels)
	if len(missing) > 0 {
		return "target_models_missing", "在本站模型定价中补齐缺失模型，或缩小规则的模型匹配范围；随后重新计算", missing
	}
	lower := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(lower, "currency"):
		return "currency_not_comparable", "为上下游模型配置同币种价格或显式汇率证据；未配置前保持人工审批", nil
	case impact.ComparableDimensions == 0 || strings.Contains(lower, "no comparable") || strings.Contains(lower, "price function"):
		return "no_comparable_price_dimension", "补齐上下游模型的输入、输出、缓存或按次价格维度，或改用真实分组倍率口径", nil
	default:
		return "pricing_evidence_unverified", "重新采集上下游目录并检查模型计费字段；证据完整前不会自动写价", nil
	}
}

func missingModels(targetModels []domain.ModelOffer, selected []string) []string {
	known := make(map[string]struct{}, len(targetModels)*2)
	for _, model := range targetModels {
		if value := strings.TrimSpace(model.ExternalID); value != "" {
			known[value] = struct{}{}
		}
		if value := strings.TrimSpace(model.Name); value != "" {
			known[value] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, model := range selected {
		if _, ok := known[model]; !ok {
			missing = append(missing, model)
		}
	}
	return sortedUniqueStrings(missing)
}

func diagnosticCategoryLabel(category string) string {
	labels := map[string]string{
		"source_group_missing":           "上游分组已不存在",
		"source_group_models_empty":      "上游分组没有模型",
		"source_model_pattern_unmatched": "模型模式未匹配",
		"source_ratio_unavailable":       "上游倍率不可用",
		"target_catalog_unavailable":     "本站模型目录不可读",
		"target_models_missing":          "本站缺少对应模型",
		"currency_not_comparable":        "上下游币种不可比较",
		"no_comparable_price_dimension":  "没有可比较的价格维度",
		"pricing_evidence_unverified":    "价格证据不可核验",
		"rule_calculation_invalid":       "规则公式或参数无效",
		"catalog_evidence_unavailable":   "上游证据状态不可读",
		"supplier_snapshot_missing":      "上游可信快照缺失",
		"target_read_failed":             "本站配置读取失败",
		"source_model_pattern_invalid":   "模型匹配模式无效",
		"source_basis_invalid":           "跟价来源口径无效",
	}
	if label := labels[category]; label != "" {
		return label
	}
	return firstNonBlank(category, "未知失败")
}

func modelSamplesSuffix(models []string) string {
	models = sampleStrings(models, diagnosticModelSampleLimit)
	if len(models) == 0 {
		return ""
	}
	return "（例如 " + strings.Join(models, "、") + "）"
}

func sampleStrings(values []string, limit int) []string {
	values = sortedUniqueStrings(values)
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	return values
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func displayDiagnosticValue(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "未配置"
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func diagnosticFromError(err error) *policyDiagnosticError {
	var diagnostic *policyDiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic
	}
	return nil
}
