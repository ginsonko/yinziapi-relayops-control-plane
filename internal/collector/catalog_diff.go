package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
)

const catalogDiffExampleLimit = 8

// catalogDiffSummary is deliberately a semantic diff rather than a response
// hash diff. Upstream pages frequently change ordering, whitespace, or other
// presentation details without changing a price. Those changes remain useful
// evidence, but must not look like a pricing write is required.
type catalogDiffSummary struct {
	AddedGroups   []catalogDiffEntry
	RemovedGroups []catalogDiffEntry
	ChangedGroups []catalogDiffEntry
	AddedModels   []catalogDiffEntry
	RemovedModels []catalogDiffEntry
	ChangedModels []catalogDiffEntry

	affectedGroups map[string]struct{}
}

type catalogDiffEntry struct {
	Key    string
	Label  string
	Group  string
	Before string
	After  string
	Detail string
}

func summarizeCatalogDiff(previous *domain.CatalogSnapshot, current domain.CatalogSnapshot) catalogDiffSummary {
	diff := catalogDiffSummary{affectedGroups: make(map[string]struct{})}
	if previous == nil {
		return diff
	}

	previousGroups := make(map[string]domain.GroupOffer, len(previous.Groups))
	currentGroups := make(map[string]domain.GroupOffer, len(current.Groups))
	for _, group := range previous.Groups {
		previousGroups[groupDiffKey(group)] = group
	}
	for _, group := range current.Groups {
		currentGroups[groupDiffKey(group)] = group
	}
	groupKeys := make([]string, 0, len(previousGroups)+len(currentGroups))
	seenGroups := make(map[string]struct{}, len(previousGroups)+len(currentGroups))
	for key := range previousGroups {
		seenGroups[key] = struct{}{}
		groupKeys = append(groupKeys, key)
	}
	for key := range currentGroups {
		if _, exists := seenGroups[key]; !exists {
			groupKeys = append(groupKeys, key)
		}
	}
	sort.Strings(groupKeys)
	for _, key := range groupKeys {
		oldGroup, hadOld := previousGroups[key]
		newGroup, hadNew := currentGroups[key]
		switch {
		case !hadOld:
			diff.AddedGroups = append(diff.AddedGroups, catalogDiffEntry{Key: key, Label: groupLabel(newGroup), After: groupDecisionRatio(newGroup), Detail: fmt.Sprintf("新增分组「%s」倍率 %s", groupLabel(newGroup), displayCatalogValue(groupDecisionRatio(newGroup)))})
			diff.markGroup(oldGroup, newGroup)
		case !hadNew:
			diff.RemovedGroups = append(diff.RemovedGroups, catalogDiffEntry{Key: key, Label: groupLabel(oldGroup), Before: groupDecisionRatio(oldGroup), Detail: fmt.Sprintf("删除分组「%s」（原倍率 %s）", groupLabel(oldGroup), displayCatalogValue(groupDecisionRatio(oldGroup)))})
			diff.markGroup(oldGroup, newGroup)
		case normalizedGroupSignature(oldGroup) != normalizedGroupSignature(newGroup):
			entry := catalogDiffEntry{Key: key, Label: groupLabel(newGroup), Before: groupDecisionRatio(oldGroup), After: groupDecisionRatio(newGroup), Detail: groupChangeDetail(oldGroup, newGroup)}
			diff.ChangedGroups = append(diff.ChangedGroups, entry)
			diff.markGroup(oldGroup, newGroup)
		}
	}

	previousModels := make(map[string]domain.ModelOffer, len(previous.Models))
	currentModels := make(map[string]domain.ModelOffer, len(current.Models))
	for _, model := range previous.Models {
		previousModels[modelDiffKey(model)] = model
	}
	for _, model := range current.Models {
		currentModels[modelDiffKey(model)] = model
	}
	modelKeys := make([]string, 0, len(previousModels)+len(currentModels))
	seenModels := make(map[string]struct{}, len(previousModels)+len(currentModels))
	for key := range previousModels {
		seenModels[key] = struct{}{}
		modelKeys = append(modelKeys, key)
	}
	for key := range currentModels {
		if _, exists := seenModels[key]; !exists {
			modelKeys = append(modelKeys, key)
		}
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		oldModel, hadOld := previousModels[key]
		newModel, hadNew := currentModels[key]
		switch {
		case !hadOld:
			diff.AddedModels = append(diff.AddedModels, catalogDiffEntry{Key: key, Label: modelLabel(newModel), Group: newModel.Group, Detail: fmt.Sprintf("新增模型「%s」", modelLabel(newModel))})
			diff.markModel(oldModel, newModel)
		case !hadNew:
			diff.RemovedModels = append(diff.RemovedModels, catalogDiffEntry{Key: key, Label: modelLabel(oldModel), Group: oldModel.Group, Detail: fmt.Sprintf("删除模型「%s」", modelLabel(oldModel))})
			diff.markModel(oldModel, newModel)
		case normalizedModelSignature(oldModel) != normalizedModelSignature(newModel):
			diff.ChangedModels = append(diff.ChangedModels, catalogDiffEntry{Key: key, Label: modelLabel(newModel), Group: newModel.Group, Detail: modelChangeDetail(oldModel, newModel)})
			diff.markModel(oldModel, newModel)
		}
	}
	return diff
}

func (d catalogDiffSummary) semanticChanged() bool {
	return len(d.AddedGroups)+len(d.RemovedGroups)+len(d.ChangedGroups)+len(d.AddedModels)+len(d.RemovedModels)+len(d.ChangedModels) > 0
}

func (d catalogDiffSummary) totalEntries() int {
	return len(d.AddedGroups) + len(d.RemovedGroups) + len(d.ChangedGroups) + len(d.AddedModels) + len(d.RemovedModels) + len(d.ChangedModels)
}

func (d catalogDiffSummary) key() string {
	if !d.semanticChanged() {
		return "format_only"
	}
	parts := make([]string, 0, d.totalEntries())
	for _, entry := range append(append(append(append(append(append([]catalogDiffEntry{}, d.AddedGroups...), d.RemovedGroups...), d.ChangedGroups...), d.AddedModels...), d.RemovedModels...), d.ChangedModels...) {
		parts = append(parts, strings.Join([]string{entry.Key, entry.Group, entry.Before, entry.After, entry.Detail}, "\x1f"))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(sum[:])
}

func (d catalogDiffSummary) activity(supplier domain.Supplier, previousHash, currentHash string, affectedPolicies int) domain.Activity {
	metadata := map[string]string{
		"previous_hash":         previousHash,
		"current_hash":          currentHash,
		"catalog_diff_key":      d.key(),
		"diff_kind":             "format_only",
		"groups_added":          strconv.Itoa(len(d.AddedGroups)),
		"groups_removed":        strconv.Itoa(len(d.RemovedGroups)),
		"groups_changed":        strconv.Itoa(len(d.ChangedGroups)),
		"models_added":          strconv.Itoa(len(d.AddedModels)),
		"models_removed":        strconv.Itoa(len(d.RemovedModels)),
		"models_changed":        strconv.Itoa(len(d.ChangedModels)),
		"affected_policy_count": strconv.Itoa(affectedPolicies),
	}
	if d.semanticChanged() {
		metadata["diff_kind"] = "semantic"
	}
	id := supplier.ID
	if !d.semanticChanged() {
		return domain.Activity{Kind: "pricing_changed", Severity: "info", Title: supplier.Name + " 目录响应有变化（价格未变）", Message: fmt.Sprintf("目录响应哈希发生变化，但分组/模型价格与计费字段未变化；未发现需要改价的映射（关联规则 %d 条）。", affectedPolicies), SupplierID: &id, Metadata: metadata}
	}
	examples := d.examples()
	message := fmt.Sprintf("%s；关联规则 %d 条。已重新计算相关映射；实际需要写价的变化会单独进入变更中心。", strings.Join(examples, "；"), affectedPolicies)
	return domain.Activity{Kind: "pricing_changed", Severity: "info", Title: supplier.Name + " 价格目录有变化", Message: message, SupplierID: &id, Metadata: metadata}
}

func (d catalogDiffSummary) examples() []string {
	entries := make([]catalogDiffEntry, 0, d.totalEntries())
	entries = append(entries, d.AddedGroups...)
	entries = append(entries, d.RemovedGroups...)
	entries = append(entries, d.ChangedGroups...)
	entries = append(entries, d.AddedModels...)
	entries = append(entries, d.RemovedModels...)
	entries = append(entries, d.ChangedModels...)
	result := make([]string, 0, catalogDiffExampleLimit)
	for _, entry := range entries {
		if len(result) >= catalogDiffExampleLimit {
			break
		}
		result = append(result, entry.Detail)
	}
	if len(result) == 0 {
		return []string{"目录语义未变化"}
	}
	if remaining := len(entries) - len(result); remaining > 0 {
		result = append(result, fmt.Sprintf("其余 %d 项差异见采集快照", remaining))
	}
	return result
}

func (d *catalogDiffSummary) markGroup(oldGroup, newGroup domain.GroupOffer) {
	if d.affectedGroups == nil {
		d.affectedGroups = make(map[string]struct{})
	}
	for _, value := range []string{oldGroup.ExternalID, oldGroup.Name, newGroup.ExternalID, newGroup.Name} {
		if value = strings.TrimSpace(value); value != "" {
			d.affectedGroups[value] = struct{}{}
		}
	}
}

func (d *catalogDiffSummary) markModel(oldModel, newModel domain.ModelOffer) {
	if d.affectedGroups == nil {
		d.affectedGroups = make(map[string]struct{})
	}
	for _, value := range []string{oldModel.Group, newModel.Group} {
		if value = strings.TrimSpace(value); value != "" {
			d.affectedGroups[value] = struct{}{}
		}
	}
}

func (d catalogDiffSummary) affectsGroup(name string) bool {
	_, exists := d.affectedGroups[strings.TrimSpace(name)]
	return exists
}

func groupDiffKey(group domain.GroupOffer) string {
	if value := strings.TrimSpace(group.ExternalID); value != "" {
		return "id:" + value
	}
	return "name:" + strings.TrimSpace(group.Name)
}

func modelDiffKey(model domain.ModelOffer) string {
	if value := strings.TrimSpace(model.ExternalID); value != "" {
		return "id:" + value
	}
	return "name:" + strings.TrimSpace(model.Group) + "\x00" + strings.TrimSpace(model.Name)
}

func groupLabel(group domain.GroupOffer) string {
	if value := strings.TrimSpace(group.Name); value != "" {
		return value
	}
	return strings.TrimSpace(group.ExternalID)
}

func modelLabel(model domain.ModelOffer) string {
	if value := strings.TrimSpace(model.Name); value != "" {
		return value
	}
	return strings.TrimSpace(model.ExternalID)
}

func groupDecisionRatio(group domain.GroupOffer) string {
	return normalizeCatalogValue(ratios.DecisionRatio(group))
}

func normalizedGroupSignature(group domain.GroupOffer) string {
	modelIDs := append([]string(nil), group.ModelIDs...)
	sort.Strings(modelIDs)
	normalizedIDs := make([]string, len(modelIDs))
	for index, value := range modelIDs {
		normalizedIDs[index] = strings.TrimSpace(value)
	}
	return strings.Join([]string{strings.TrimSpace(group.Name), groupDecisionRatio(group), normalizeCatalogValue(group.RawRatio), normalizeCatalogValue(group.EffectiveRatio), normalizeCatalogValue(group.RechargeValueFactor), strings.Join(normalizedIDs, ",")}, "\x1f")
}

func normalizedModelSignature(model domain.ModelOffer) string {
	return strings.Join([]string{strings.TrimSpace(model.Name), strings.TrimSpace(model.Group), strings.TrimSpace(model.BillingMode), normalizeCatalogValue(model.ModelRatio), normalizeCatalogValue(model.CompletionRatio), normalizeCatalogValue(model.CacheRatio), normalizeCatalogValue(model.ModelPrice), strings.TrimSpace(model.BillingExpr), strings.TrimSpace(model.Currency), priceFunctionSignature(model.PriceFunction)}, "\x1f")
}

func priceFunctionSignature(value domain.PriceFunction) string {
	fields := []string{value.Currency, value.Basis, value.InputPer1M, value.OutputPer1M, value.CacheReadPer1M, value.CacheWritePer1M, value.CacheWrite5MPer1M, value.CacheWrite1HPer1M, value.ImageInputPer1M, value.AudioInputPer1M, value.AudioOutputPer1M, value.PerRequest, value.PerImage, value.PerSecond, value.PerFrame, value.FixedFee, value.GroupMultiplier, value.Expression, value.Derivation}
	for index := range fields {
		fields[index] = normalizeCatalogValue(fields[index])
	}
	keys := make([]string, 0, len(value.Dimensions))
	for key := range value.Dimensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fields = append(fields, strings.TrimSpace(key)+"="+normalizeCatalogValue(value.Dimensions[key]))
	}
	if value.RequiresModelBasePrice {
		fields = append(fields, "requires_model_base_price=true")
	}
	return strings.Join(fields, "\x1e")
}

func groupChangeDetail(oldGroup, newGroup domain.GroupOffer) string {
	oldRatio, newRatio := groupDecisionRatio(oldGroup), groupDecisionRatio(newGroup)
	if oldRatio != newRatio {
		return fmt.Sprintf("分组「%s」倍率 %s -> %s", groupLabel(newGroup), displayCatalogValue(oldRatio), displayCatalogValue(newRatio))
	}
	oldCount, newCount := len(oldGroup.ModelIDs), len(newGroup.ModelIDs)
	if oldCount != newCount {
		return fmt.Sprintf("分组「%s」模型集合 %d -> %d 个", groupLabel(newGroup), oldCount, newCount)
	}
	if normalizeCatalogValue(oldGroup.RechargeValueFactor) != normalizeCatalogValue(newGroup.RechargeValueFactor) {
		return fmt.Sprintf("分组「%s」充值折算 %s -> %s", groupLabel(newGroup), displayCatalogValue(oldGroup.RechargeValueFactor), displayCatalogValue(newGroup.RechargeValueFactor))
	}
	return fmt.Sprintf("分组「%s」目录字段发生变化", groupLabel(newGroup))
}

func modelChangeDetail(oldModel, newModel domain.ModelOffer) string {
	if normalizeCatalogValue(oldModel.ModelPrice) != normalizeCatalogValue(newModel.ModelPrice) {
		return fmt.Sprintf("模型「%s」价格 %s -> %s", modelLabel(newModel), displayCatalogValue(oldModel.ModelPrice), displayCatalogValue(newModel.ModelPrice))
	}
	if normalizeCatalogValue(oldModel.ModelRatio) != normalizeCatalogValue(newModel.ModelRatio) ||
		normalizeCatalogValue(oldModel.CompletionRatio) != normalizeCatalogValue(newModel.CompletionRatio) ||
		normalizeCatalogValue(oldModel.CacheRatio) != normalizeCatalogValue(newModel.CacheRatio) ||
		priceFunctionSignature(oldModel.PriceFunction) != priceFunctionSignature(newModel.PriceFunction) {
		return fmt.Sprintf("模型「%s」计费参数发生变化", modelLabel(newModel))
	}
	return fmt.Sprintf("模型「%s」目录字段发生变化", modelLabel(newModel))
}

func normalizeCatalogValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if parsed, err := decimal.NewFromString(value); err == nil {
		return parsed.String()
	}
	return value
}

func displayCatalogValue(value string) string {
	if normalized := normalizeCatalogValue(value); normalized != "" {
		return normalized
	}
	return "未知"
}
