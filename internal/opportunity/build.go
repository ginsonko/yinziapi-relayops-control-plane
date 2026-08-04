package opportunity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
)

type DiscoveryEvidence struct {
	ID                 string
	SupplierID         int64
	Supplier           string
	SupplierBaseURL    string
	SupplierEnabled    bool
	SupplierStatus     string
	SupplierErrorClass string
	Offer              domain.GroupOffer
	Fingerprint        string
	ObservedAt         time.Time
}

type CatalogEvidence struct {
	Supplier domain.Supplier
	Snapshot domain.CatalogSnapshot
}

type UsageEvidence struct {
	ModelID      string
	WindowDays   int
	DemandWeight string
	DemandUnit   string
	Confidence   string
	ObservedAt   time.Time
}

type BuildInput struct {
	Discoveries   []DiscoveryEvidence
	Catalogs      []CatalogEvidence
	Target        domain.CatalogSnapshot
	Usage         []UsageEvidence
	CurrentRoutes map[string]struct{}
	Config        Config
}

var recognizableModel = regexp.MustCompile(`[A-Za-z0-9\p{Han}]`)

func BuildCandidates(input BuildInput) []Candidate {
	config := withDefaults(input.Config, DefaultConfig())
	feeRate, feeOK := optionalNonNegative(config.FeeRate)
	if !feeOK {
		feeRate = decimal.Zero
	}
	retryRate, retryOK := optionalNonNegative(config.RetryLossRate)
	if !retryOK {
		retryRate = decimal.Zero
	}
	targetModels := modelIndex(input.Target.Models)
	usage := usageIndex(input.Usage)
	blacklist := normalizedSet(config.BlacklistedEntities)
	allowedFamilies := normalizedSet(config.AllowedModelFamilies)
	results := make([]Candidate, 0, len(input.Discoveries))
	for _, discovery := range input.Discoveries {
		candidate := Candidate{ID: discovery.ID, SupplierID: discovery.SupplierID, Supplier: discovery.Supplier,
			Group: discovery.Offer.Name, EntityFingerprint: discovery.Fingerprint, Models: []ModelInput{}, ObservedAt: discovery.ObservedAt}
		if strings.TrimSpace(candidate.Group) == "" {
			candidate.Group = discovery.Offer.ExternalID
		}
		candidate.Blacklisted = containsNormalized(blacklist, discovery.Fingerprint) || containsNormalized(blacklist, discovery.Supplier+":"+candidate.Group)
		candidate.Unavailable = !discovery.SupplierEnabled || isUnavailableStatus(discovery.SupplierStatus, discovery.SupplierErrorClass)
		candidate.Availability = availability(discovery.SupplierStatus, discovery.Offer.Metadata)
		candidate.AvailabilityLower = availabilityLower(candidate.Availability)
		candidate.EvidenceConfidence = evidenceConfidence(discovery.SupplierID, input.Catalogs)
		candidate.AllowSubsidy, candidate.SubsidyBudgetDaily = subsidyPolicy(discovery.SupplierID, candidate.Group, input.Catalogs)

		candidateCatalog, foundCatalog := catalogBySupplier(discovery.SupplierID, input.Catalogs)
		for _, modelID := range uniqueSorted(discovery.Offer.ModelIDs) {
			recognized := recognizableModel.MatchString(strings.TrimSpace(modelID))
			family := ModelFamily(modelID)
			targetModel, targetFound := targetModels[strings.ToLower(modelID)]
			if targetFound {
				candidate.TargetIntersection = true
			} else if containsNormalized(allowedFamilies, family) {
				candidate.CreateEligible = true
			}
			model := ModelInput{ModelID: modelID, Family: family, Recognized: recognized}
			if evidence, exists := usage[strings.ToLower(modelID)]; exists {
				model.Demand7D = evidence[7].DemandWeight
				model.Demand30D = evidence[30].DemandWeight
				model.DemandUnit = usageDemandUnit(evidence)
				model.DemandConfidence = bestConfidence(evidence[7].Confidence, evidence[30].Confidence)
			}
			if !foundCatalog || !targetFound {
				candidate.Models = append(candidate.Models, model)
				continue
			}
			sourceModel, sourceFound := findModel(candidateCatalog.Snapshot.Models, modelID, discovery.Offer.Name)
			if !sourceFound {
				candidate.Models = append(candidate.Models, model)
				continue
			}
			targetMultiplier := targetSalesMultiplier(input.Target, modelID)
			candidateCost, salePrice, compared, total, currency, priceUnit, comparable := comparableBasket(sourceModel.PriceFunction, targetModel.PriceFunction, ratios.DecisionRatio(discovery.Offer), targetMultiplier)
			if !comparable {
				candidate.Models = append(candidate.Models, model)
				continue
			}
			baseline, baselineFound := cheapestBaseline(modelID, discovery, input.Catalogs, input.CurrentRoutes, targetModel.PriceFunction)
			model.Currency = currency
			model.PriceUnit = priceUnit
			model.CandidateUnitCost = canonical(candidateCost)
			model.SalesUnitPrice = canonical(salePrice)
			model.TargetSalesRatio = targetMultiplier
			if baselineFound {
				model.CurrentUnitCost = canonical(baseline)
			}
			model.FeesUnit = canonical(candidateCost.Mul(feeRate))
			availabilityValue, _ := optionalUnitInterval(candidate.Availability)
			model.RetryLossUnit = canonical(candidateCost.Mul(retryRate).Mul(decimal.NewFromInt(1).Sub(availabilityValue)))
			model.ComparableDimensions = compared
			model.TotalDimensions = total
			candidate.Models = append(candidate.Models, model)
		}
		results = append(results, candidate)
	}
	return results
}

func cheapestBaseline(modelID string, candidate DiscoveryEvidence, catalogs []CatalogEvidence, currentRoutes map[string]struct{}, target domain.PriceFunction) (decimal.Decimal, bool) {
	if len(currentRoutes) == 0 {
		return decimal.Zero, false
	}
	var cheapest decimal.Decimal
	found := false
	for _, catalog := range catalogs {
		if !catalog.Supplier.Enabled || isUnavailableStatus(catalog.Supplier.LastStatus, catalog.Supplier.LastErrorClass) {
			continue
		}
		for _, group := range catalog.Snapshot.Groups {
			if catalog.Supplier.ID == candidate.SupplierID && (group.ExternalID == candidate.Offer.ExternalID || group.Name == candidate.Offer.Name) {
				continue
			}
			if !groupContainsModel(group, modelID) {
				continue
			}
			if _, active := currentRoutes[RouteKey(catalog.Supplier.ID, firstNonEmpty(group.ExternalID, group.Name))]; !active {
				if _, active = currentRoutes[RouteKey(catalog.Supplier.ID, group.Name)]; !active {
					continue
				}
			}
			source, ok := findModel(catalog.Snapshot.Models, modelID, group.Name)
			if !ok {
				continue
			}
			cost, _, _, _, _, _, ok := comparableBasket(source.PriceFunction, target, ratios.DecisionRatio(group), "1")
			if !ok {
				continue
			}
			if !found || cost.LessThan(cheapest) {
				cheapest, found = cost, true
			}
		}
	}
	return cheapest, found
}

func RouteKey(supplierID int64, group string) string {
	return fmt.Sprintf("%d:%s", supplierID, strings.ToLower(strings.TrimSpace(group)))
}

func comparableBasket(source, target domain.PriceFunction, sourceMultiplier, targetMultiplier string) (decimal.Decimal, decimal.Decimal, int, int, string, string, bool) {
	if source.RequiresModelBasePrice || target.RequiresModelBasePrice {
		return decimal.Zero, decimal.Zero, 0, 0, "", "", false
	}
	sourceCurrency := strings.ToUpper(strings.TrimSpace(source.Currency))
	targetCurrency := strings.ToUpper(strings.TrimSpace(target.Currency))
	if sourceCurrency == "" || targetCurrency == "" || sourceCurrency != targetCurrency {
		return decimal.Zero, decimal.Zero, 0, 0, "", "", false
	}
	sourceFactor, sourceOK := optionalNonNegative(sourceMultiplier)
	targetFactor, targetOK := optionalNonNegative(targetMultiplier)
	if !sourceOK || !targetOK {
		return decimal.Zero, decimal.Zero, 0, 0, "", "", false
	}
	sourceDimensions := priceDimensions(source)
	targetDimensions := priceDimensions(target)
	weights := dimensionWeights(targetDimensions)
	if len(weights) == 0 {
		return decimal.Zero, decimal.Zero, 0, len(targetDimensions), "", "", false
	}
	weightedSource, weightedTarget, weightSum := decimal.Zero, decimal.Zero, decimal.Zero
	compared := 0
	for dimension, weight := range weights {
		sourceValue, sourceExists := sourceDimensions[dimension]
		targetValue, targetExists := targetDimensions[dimension]
		if !sourceExists || !targetExists {
			continue
		}
		weightedSource = weightedSource.Add(sourceValue.Mul(weight))
		weightedTarget = weightedTarget.Add(targetValue.Mul(weight))
		weightSum = weightSum.Add(weight)
		compared++
	}
	if compared == 0 || !weightSum.IsPositive() {
		return decimal.Zero, decimal.Zero, 0, len(weights), "", "", false
	}
	return weightedSource.Div(weightSum).Mul(sourceFactor), weightedTarget.Div(weightSum).Mul(targetFactor), compared, len(weights), targetCurrency, basketDemandUnit(weights), true
}

func basketDemandUnit(weights map[string]decimal.Decimal) string {
	if len(weights) == 1 {
		for dimension := range weights {
			switch dimension {
			case "request", "fixed":
				return "requests"
			case "image":
				return "images"
			case "second":
				return "seconds"
			case "frame":
				return "frames"
			}
		}
	}
	for dimension := range weights {
		switch dimension {
		case "input", "output", "cache_read", "cache_write", "cache_write_5m", "cache_write_1h", "image_input", "audio_input", "audio_output":
			return "million_tokens"
		}
	}
	return "normalized_demand"
}

func priceDimensions(function domain.PriceFunction) map[string]decimal.Decimal {
	raw := map[string]string{
		"input": function.InputPer1M, "output": function.OutputPer1M, "cache_read": function.CacheReadPer1M,
		"cache_write": function.CacheWritePer1M, "cache_write_5m": function.CacheWrite5MPer1M,
		"cache_write_1h": function.CacheWrite1HPer1M, "image_input": function.ImageInputPer1M,
		"audio_input": function.AudioInputPer1M, "audio_output": function.AudioOutputPer1M,
		"request": function.PerRequest, "image": function.PerImage, "second": function.PerSecond,
		"frame": function.PerFrame, "fixed": function.FixedFee,
	}
	for key, value := range function.Dimensions {
		raw["custom:"+key] = value
	}
	result := make(map[string]decimal.Decimal, len(raw))
	for key, value := range raw {
		if parsed, ok := optionalNonNegative(value); ok {
			result[key] = parsed
		}
	}
	return result
}

func dimensionWeights(dimensions map[string]decimal.Decimal) map[string]decimal.Decimal {
	for _, singleton := range []string{"request", "image", "second", "frame", "fixed"} {
		if _, exists := dimensions[singleton]; exists {
			return map[string]decimal.Decimal{singleton: decimal.NewFromInt(1)}
		}
	}
	base := map[string]string{"input": "0.55", "output": "0.25", "cache_read": "0.08", "cache_write": "0.04",
		"cache_write_5m": "0.02", "cache_write_1h": "0.02", "image_input": "0.02", "audio_input": "0.01", "audio_output": "0.01"}
	result := map[string]decimal.Decimal{}
	for dimension := range dimensions {
		if raw, exists := base[dimension]; exists {
			result[dimension], _ = decimal.NewFromString(raw)
		} else if strings.HasPrefix(dimension, "custom:") {
			result[dimension] = decimal.NewFromFloat(0.01)
		}
	}
	return result
}

func modelIndex(models []domain.ModelOffer) map[string]domain.ModelOffer {
	result := make(map[string]domain.ModelOffer, len(models))
	for _, model := range models {
		key := strings.ToLower(strings.TrimSpace(firstNonEmpty(model.ExternalID, model.Name)))
		if key != "" {
			result[key] = model
		}
	}
	return result
}

func findModel(models []domain.ModelOffer, modelID, group string) (domain.ModelOffer, bool) {
	key := strings.ToLower(strings.TrimSpace(modelID))
	var fallback domain.ModelOffer
	found := false
	for _, model := range models {
		modelKey := strings.ToLower(strings.TrimSpace(firstNonEmpty(model.ExternalID, model.Name)))
		if modelKey != key {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.Group), strings.TrimSpace(group)) {
			return model, true
		}
		if !found {
			fallback, found = model, true
		}
	}
	return fallback, found
}

func targetSalesMultiplier(target domain.CatalogSnapshot, modelID string) string {
	var minimum decimal.Decimal
	found := false
	for _, group := range target.Groups {
		if !groupContainsModel(group, modelID) {
			continue
		}
		value, ok := optionalNonNegative(group.Ratio)
		if !ok || !value.IsPositive() {
			continue
		}
		if !found || value.LessThan(minimum) {
			minimum, found = value, true
		}
	}
	if found {
		return canonical(minimum)
	}
	return "1"
}

func groupContainsModel(group domain.GroupOffer, modelID string) bool {
	for _, candidate := range group.ModelIDs {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(modelID)) {
			return true
		}
	}
	return false
}

func usageIndex(items []UsageEvidence) map[string]map[int]UsageEvidence {
	result := map[string]map[int]UsageEvidence{}
	for _, item := range items {
		key := strings.ToLower(strings.TrimSpace(item.ModelID))
		if key == "" || (item.WindowDays != 7 && item.WindowDays != 30) {
			continue
		}
		if result[key] == nil {
			result[key] = map[int]UsageEvidence{}
		}
		current, exists := result[key][item.WindowDays]
		if !exists || item.ObservedAt.After(current.ObservedAt) {
			result[key][item.WindowDays] = item
		}
	}
	return result
}

func usageDemandUnit(evidence map[int]UsageEvidence) string {
	unit7 := strings.ToLower(strings.TrimSpace(evidence[7].DemandUnit))
	unit30 := strings.ToLower(strings.TrimSpace(evidence[30].DemandUnit))
	switch {
	case unit7 == "":
		return unit30
	case unit30 == "":
		return unit7
	case unit7 == unit30:
		return unit7
	default:
		return "mixed"
	}
}

func availability(status string, metadata map[string]string) string {
	for _, key := range []string{"availability", "success_rate", "successRate"} {
		if value, ok := optionalUnitInterval(metadata[key]); ok {
			return canonical(value)
		}
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy", "ok", "active":
		return "0.95"
	case "degraded", "warning", "partial":
		return "0.65"
	case "error", "failed", "disabled":
		return "0.2"
	default:
		return "0.5"
	}
}

func availabilityLower(raw string) string {
	value, ok := optionalUnitInterval(raw)
	if !ok {
		return "0.3"
	}
	return canonical(decimal.Max(decimal.Zero, value.Sub(decimal.NewFromFloat(0.15))))
}

func evidenceConfidence(supplierID int64, catalogs []CatalogEvidence) string {
	for _, catalog := range catalogs {
		if catalog.Supplier.ID == supplierID {
			value := catalog.Snapshot.Evidence.Confidence
			if value < 0 {
				value = 0
			}
			if value > 100 {
				value = 100
			}
			return decimal.NewFromInt(int64(value)).Div(decimal.NewFromInt(100)).String()
		}
	}
	return "0.4"
}

func catalogBySupplier(id int64, catalogs []CatalogEvidence) (CatalogEvidence, bool) {
	for _, catalog := range catalogs {
		if catalog.Supplier.ID == id {
			return catalog, true
		}
	}
	return CatalogEvidence{}, false
}

func subsidyPolicy(supplierID int64, group string, catalogs []CatalogEvidence) (bool, string) {
	for _, catalog := range catalogs {
		if catalog.Supplier.ID != supplierID {
			continue
		}
		for _, candidate := range catalog.Snapshot.Groups {
			if candidate.Name != group && candidate.ExternalID != group {
				continue
			}
			allow := strings.EqualFold(candidate.Metadata["allow_subsidy"], "true")
			return allow, candidate.Metadata["subsidy_budget_daily"]
		}
	}
	return false, ""
}

func ModelFamily(model string) string {
	value := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(value, "claude"), strings.Contains(value, "ccmax"), strings.Contains(value, "kiro"), strings.Contains(value, "fable"):
		return "Claude"
	case strings.Contains(value, "gpt"), strings.Contains(value, "codex"), strings.HasPrefix(value, "o1"), strings.HasPrefix(value, "o3"), strings.HasPrefix(value, "o4"):
		return "OpenAI"
	case strings.Contains(value, "gemini"):
		return "Gemini"
	case strings.Contains(value, "grok"):
		return "Grok"
	case strings.Contains(value, "deepseek"):
		return "DeepSeek"
	case strings.Contains(value, "qwen"):
		return "Qwen"
	case strings.Contains(value, "image"), strings.Contains(value, "flux"), strings.Contains(value, "midjourney"):
		return "Image"
	case strings.Contains(value, "video"), strings.Contains(value, "veo"), strings.Contains(value, "sora"):
		return "Video"
	default:
		return "Other"
	}
}

func isUnavailableStatus(status, errorClass string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	errorClass = strings.ToLower(strings.TrimSpace(errorClass))
	return status == "disabled" || status == "suspended" || errorClass == "auth_missing" || errorClass == "permission_denied"
}

func normalizedSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func containsNormalized(values map[string]struct{}, value string) bool {
	_, exists := values[strings.ToLower(strings.TrimSpace(value))]
	return exists
}

func bestConfidence(values ...string) string {
	best := decimal.Zero
	found := false
	for _, raw := range values {
		if value, ok := optionalUnitInterval(raw); ok && (!found || value.GreaterThan(best)) {
			best, found = value, true
		}
	}
	if found {
		return canonical(best)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func ContentFingerprint(baseURL string, offer domain.GroupOffer) string {
	models := uniqueSorted(offer.ModelIDs)
	payload := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/") + "|" + strings.TrimSpace(offer.ExternalID) + "|" + strings.Join(models, ",") + "|" + strings.TrimSpace(offer.Ratio))
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
