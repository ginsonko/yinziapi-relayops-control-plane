package opportunity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const maximumUsageResponseBytes = 16 << 20

type ObservedUsage struct {
	ModelID      string
	WindowDays   int
	DemandWeight string
	DemandUnit   string
	Confidence   string
	ObservedAt   time.Time
}

type flowUsageRow struct {
	ModelName string      `json:"model_name"`
	TokenUsed json.Number `json:"token_used"`
	Count     json.Number `json:"count"`
}

func ReadNewAPIUsage(ctx context.Context, client *http.Client, credentialService *credentials.Service, target domain.TargetSite, observedAt time.Time) ([]ObservedUsage, error) {
	if client == nil || credentialService == nil {
		return nil, errors.New("NewAPI usage reader is not configured")
	}
	credential, payload, err := credentialService.Load(ctx, target.CredentialID)
	if err != nil {
		return nil, fmt.Errorf("load target usage credential: %w", err)
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, target.BaseURL); err != nil {
		return nil, fmt.Errorf("target usage credential origin mismatch: %w", err)
	}
	safeClient, err := netguard.SameOriginClient(client, target.BaseURL)
	if err != nil {
		return nil, err
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	type result struct {
		items []ObservedUsage
		err   error
	}
	results := make(chan result, 2)
	for _, days := range []int{7, 30} {
		days := days
		go func() {
			items, readErr := readFlowWindow(ctx, safeClient, target, payload, days, observedAt)
			results <- result{items: items, err: readErr}
		}()
	}
	combined := make([]ObservedUsage, 0)
	for range 2 {
		value := <-results
		if value.err != nil {
			return nil, value.err
		}
		combined = append(combined, value.items...)
	}
	return combined, nil
}

func readFlowWindow(ctx context.Context, client *http.Client, target domain.TargetSite, payload domain.CredentialPayload, days int, observedAt time.Time) ([]ObservedUsage, error) {
	query := url.Values{}
	query.Set("start_timestamp", strconv.FormatInt(observedAt.Add(-time.Duration(days)*24*time.Hour).Unix(), 10))
	query.Set("end_timestamp", strconv.FormatInt(observedAt.Unix(), 10))
	requestURL := strings.TrimRight(target.BaseURL, "/") + "/api/data/flow?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if err := authorizeUsageRequest(request, payload); err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read %d-day NewAPI model usage: %w", days, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumUsageResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maximumUsageResponseBytes {
		return nil, fmt.Errorf("%d-day NewAPI model usage response exceeded the safety limit", days)
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%d-day NewAPI model usage credential lacks administrator access", days)
		}
		return nil, fmt.Errorf("%d-day NewAPI model usage returned HTTP %d", days, response.StatusCode)
	}
	var envelope struct {
		Success *bool          `json:"success"`
		Message string         `json:"message"`
		Data    []flowUsageRow `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode %d-day NewAPI model usage: %w", days, err)
	}
	if envelope.Success == nil || !*envelope.Success {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = "response did not confirm success"
		}
		return nil, fmt.Errorf("read %d-day NewAPI model usage: %s", days, message)
	}
	type aggregate struct{ tokens, count decimal.Decimal }
	byModel := map[string]aggregate{}
	names := map[string]string{}
	for _, row := range envelope.Data {
		key := strings.ToLower(strings.TrimSpace(row.ModelName))
		if key == "" {
			continue
		}
		current := byModel[key]
		current.tokens = current.tokens.Add(numberDecimal(row.TokenUsed))
		current.count = current.count.Add(numberDecimal(row.Count))
		byModel[key] = current
		names[key] = strings.TrimSpace(row.ModelName)
	}
	items := make([]ObservedUsage, 0, len(byModel))
	for key, aggregate := range byModel {
		demand := aggregate.count
		unit := "requests"
		confidence := "0.8"
		if aggregate.tokens.IsPositive() {
			demand = aggregate.tokens.Div(decimal.NewFromInt(1_000_000))
			unit = "million_tokens"
			confidence = "0.95"
		}
		items = append(items, ObservedUsage{ModelID: names[key], WindowDays: days, DemandWeight: demand.String(),
			DemandUnit: unit, Confidence: confidence, ObservedAt: observedAt})
	}
	return items, nil
}

func authorizeUsageRequest(request *http.Request, payload domain.CredentialPayload) error {
	if strings.TrimSpace(payload.Authorization) != "" {
		request.Header.Set("Authorization", payload.Authorization)
	}
	if strings.TrimSpace(payload.Cookie) != "" {
		request.Header.Set("Cookie", payload.Cookie)
	}
	if payload.UserID > 0 {
		request.Header.Set("New-Api-User", strconv.FormatInt(payload.UserID, 10))
	}
	for key, value := range payload.Headers {
		if strings.TrimSpace(key) != "" {
			request.Header.Set(key, value)
		}
	}
	if request.Header.Get("Authorization") == "" && request.Header.Get("Cookie") == "" {
		return errors.New("target usage credential contains neither authorization nor session cookie")
	}
	return nil
}

func numberDecimal(value json.Number) decimal.Decimal {
	if strings.TrimSpace(value.String()) == "" {
		return decimal.Zero
	}
	parsed, err := decimal.NewFromString(value.String())
	if err != nil || parsed.IsNegative() {
		return decimal.Zero
	}
	return parsed
}
