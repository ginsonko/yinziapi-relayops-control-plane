package connectors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func manifestJSONPaths(supplier domain.Supplier, capability string) (map[string]string, bool) {
	if !strings.EqualFold(strings.TrimSpace(supplier.Metadata["manifest_"+capability+"_contract"]), "json") {
		return nil, false
	}
	var paths map[string]string
	if err := json.Unmarshal([]byte(supplier.Metadata["manifest_"+capability+"_json_paths"]), &paths); err != nil || len(paths) == 0 {
		return nil, false
	}
	return paths, true
}

func parseManifestJSONRoot(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, &DiagnosticError{Class: "contract_changed", Operation: "manifest_json", Cause: err}
	}
	return root, nil
}

func manifestJSONScalar(root any, paths map[string]string, key string) (string, bool, error) {
	path := strings.TrimSpace(paths[key])
	if path == "" {
		return "", false, nil
	}
	values, err := evaluateJSONPath(root, path)
	if err != nil {
		return "", false, err
	}
	value, ok := firstScalar(values)
	return strings.TrimSpace(value), ok, nil
}

func parseManifestBalance(body []byte, paths map[string]string, unit newAPIUnitContract) (*domain.BalanceState, []string, error) {
	root, err := parseManifestJSONRoot(body)
	if err != nil {
		return nil, nil, err
	}
	amount, ok, err := manifestJSONScalar(root, paths, "amount")
	if err != nil || !ok {
		return nil, nil, &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: err}
	}
	value, err := decimal.NewFromString(amount)
	if err != nil || value.IsNegative() {
		return nil, nil, &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: err}
	}
	currency, _, _ := manifestJSONScalar(root, paths, "currency")
	available, _, _ := manifestJSONScalar(root, paths, "available")
	dailySpend, _, _ := manifestJSONScalar(root, paths, "daily_spend")
	rawUnit, _, _ := manifestJSONScalar(root, paths, "raw_unit")
	state := &domain.BalanceState{Amount: amount, Available: available, DailySpend: dailySpend,
		Currency: normalizeCurrency(currency), Confidence: "authenticated", ObservedAt: time.Now().UTC(),
		SourceField: paths["amount"], RawValue: amount, RawUnit: rawUnit, NormalizationFormula: "identity"}
	if state.Available == "" {
		state.Available = state.Amount
	}
	if strings.EqualFold(currency, "quota") || strings.EqualFold(rawUnit, "quota") {
		normalized, formula, normalizedOK := normalizeQuota(amount, unit.QuotaPerUnit, unit.Currency)
		state.RawUnit = "quota"
		state.NormalizationDivisor = unit.QuotaPerUnit
		state.NormalizationFormula = formula
		state.RawDisplayText = formula
		if !normalizedOK {
			state.Amount, state.Available, state.Currency = "", "", ""
			state.Confidence = "invalid_unit"
			return state, []string{"manifest balance returned quota but the status contract did not provide a usable quota conversion"}, nil
		}
		state.Amount, state.Available, state.Currency = normalized, normalized, unit.Currency
	}
	if requestCount, found, _ := manifestJSONScalar(root, paths, "request_count"); found {
		parsed, parseErr := strconv.ParseInt(requestCount, 10, 64)
		if parseErr != nil || parsed < 0 {
			return nil, nil, &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: parseErr}
		}
		state.RequestCount = &parsed
	}
	if expiresAt, found, _ := manifestJSONScalar(root, paths, "expires_at"); found {
		if parsed := parseManifestTime(expiresAt); parsed != nil {
			state.ExpiresAt = parsed
		}
	}
	return state, nil, nil
}

func parseManifestUsage(body []byte, paths map[string]string, unit newAPIUnitContract, observedAt time.Time) (newAPIUsageSummary, error) {
	root, err := parseManifestJSONRoot(body)
	if err != nil {
		return newAPIUsageSummary{}, err
	}
	amount, ok, err := manifestJSONScalar(root, paths, "amount")
	if !ok {
		amount, ok, err = manifestJSONScalar(root, paths, "daily_spend")
	}
	if err != nil || !ok {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err}
	}
	parsedAmount, parseErr := decimal.NewFromString(amount)
	if parseErr != nil || parsedAmount.IsNegative() {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: parseErr}
	}
	currency, _, _ := manifestJSONScalar(root, paths, "currency")
	rawUnit, _, _ := manifestJSONScalar(root, paths, "raw_unit")
	if strings.EqualFold(currency, "quota") || strings.EqualFold(rawUnit, "quota") {
		normalized, _, normalizedOK := normalizeQuota(amount, unit.QuotaPerUnit, unit.Currency)
		if !normalizedOK {
			return newAPIUsageSummary{}, &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
		}
		amount = normalized
	}
	var requestCount int64
	if rawCount, found, _ := manifestJSONScalar(root, paths, "request_count"); found {
		requestCount, parseErr = strconv.ParseInt(rawCount, 10, 64)
		if parseErr != nil || requestCount < 0 {
			return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: parseErr}
		}
	}
	return newAPIUsageSummary{Amount: amount, RequestCount: requestCount, ObservedAt: observedAt}, nil
}

func validateManifestBalanceDocument(paths map[string]string) capabilityDocumentValidator {
	return func(body []byte) error {
		_, _, err := parseManifestBalance(body, paths, newAPIUnitContract{QuotaPerUnit: "1", Currency: "USD"})
		return err
	}
}

func validateManifestUsageDocument(paths map[string]string) capabilityDocumentValidator {
	return func(body []byte) error {
		_, err := parseManifestUsage(body, paths, newAPIUnitContract{QuotaPerUnit: "1", Currency: "USD"}, time.Now().UTC())
		return err
	}
}

func parseManifestTime(raw string) *time.Time {
	if value, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
		value = value.UTC()
		return &value
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	if seconds > 1_000_000_000_000 {
		seconds /= 1000
	}
	value := time.Unix(seconds, 0).UTC()
	return &value
}

func manifestContractName(capability string) string {
	return fmt.Sprintf("manifest.%s.json.v1", capability)
}
