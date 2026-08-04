package mappingdraft

import (
	"sort"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"
)

func bestNameSimilarity(sourceNames []string, target string) (int64, bool) {
	best := int64(0)
	exact := false
	for _, source := range sourceNames {
		score, isExact := nameSimilarity(source, target)
		if score > best {
			best = score
		}
		if isExact {
			exact = true
		}
	}
	return best, exact
}

func nameSimilarity(left, right string) (int64, bool) {
	leftNormalized := normalizeName(left)
	rightNormalized := normalizeName(right)
	if leftNormalized == "" || rightNormalized == "" {
		return 0, false
	}
	if leftNormalized == rightNormalized {
		return 10000, true
	}
	leftTokens := nameTokens(left)
	rightTokens := nameTokens(right)
	score, _ := jaccard(leftTokens, rightTokens)
	if (strings.Contains(leftNormalized, rightNormalized) || strings.Contains(rightNormalized, leftNormalized)) && score < 7000 &&
		len([]rune(leftNormalized)) >= 3 && len([]rune(rightNormalized)) >= 3 {
		score = 7000
	}
	return score, false
}

func normalizeName(value string) string {
	var builder strings.Builder
	for _, runeValue := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(runeValue) || unicode.IsNumber(runeValue) {
			builder.WriteRune(runeValue)
		}
	}
	return builder.String()
}

func nameTokens(value string) []string {
	parts := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	return sortedUnique(parts)
}

func modelFamilies(models []string) []string {
	result := make([]string, 0, len(models))
	for _, model := range models {
		value := strings.ToLower(strings.TrimSpace(model))
		if slash := strings.LastIndexAny(value, "/:"); slash >= 0 && slash+1 < len(value) {
			value = value[slash+1:]
		}
		parts := strings.FieldsFunc(value, func(r rune) bool {
			return !unicode.IsLetter(r)
		})
		if len(parts) > 0 && parts[0] != "" {
			result = append(result, parts[0])
		}
	}
	return sortedUnique(result)
}

func jaccard(left, right []string) (int64, []string) {
	score, intersection, _ := jaccardStats(left, right)
	return score, intersection
}

func jaccardStats(left, right []string) (int64, []string, int) {
	return jaccardSetStats(stringSet(left), stringSet(right))
}

func jaccardSetStats(leftSet, rightSet map[string]string) (int64, []string, int) {
	intersection := make([]string, 0)
	union := make(map[string]string, len(leftSet)+len(rightSet))
	for key, value := range leftSet {
		union[key] = value
		if _, exists := rightSet[key]; exists {
			intersection = append(intersection, value)
		}
	}
	for key, value := range rightSet {
		union[key] = value
	}
	sort.Strings(intersection)
	if len(union) == 0 {
		return 0, intersection, 0
	}
	return int64(len(intersection) * 10000 / len(union)), intersection, len(union)
}

func unionCount(left, right []string) int {
	union := stringSet(left)
	for key, value := range stringSet(right) {
		union[key] = value
	}
	return len(union)
}

func ratioSimilarity(leftRaw, rightRaw string) (int64, bool) {
	left, leftErr := decimal.NewFromString(strings.TrimSpace(leftRaw))
	right, rightErr := decimal.NewFromString(strings.TrimSpace(rightRaw))
	if leftErr != nil || rightErr != nil || left.IsNegative() || right.IsNegative() {
		return 0, false
	}
	if left.IsZero() && right.IsZero() {
		return 10000, true
	}
	if left.IsZero() || right.IsZero() {
		return 0, true
	}
	minimum, maximum := left, right
	if minimum.GreaterThan(maximum) {
		minimum, maximum = maximum, minimum
	}
	return minimum.Div(maximum).Mul(decimal.NewFromInt(10000)).Round(0).IntPart(), true
}

func stringSet(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			result[strings.ToLower(trimmed)] = trimmed
		}
	}
	return result
}

func containsFold(values map[string]string, value string) bool {
	_, exists := values[strings.ToLower(strings.TrimSpace(value))]
	return exists
}

func sortedUnique(values []string) []string {
	set := stringSet(values)
	result := make([]string, 0, len(set))
	for _, value := range set {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i]) < strings.ToLower(result[j]) })
	return result
}

func formatBasisPoints(value int64) string {
	if value < 0 {
		value = 0
	}
	return decimal.NewFromInt(value).Div(decimal.NewFromInt(10000)).StringFixed(4)
}

func basisPoints(value string) int64 {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed.Mul(decimal.NewFromInt(10000)).Round(0).IntPart()
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	return values
}
