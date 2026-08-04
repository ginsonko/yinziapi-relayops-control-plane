package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const (
	sub2ModeAuto     = "auto"
	sub2ModeAdmin    = "admin"
	sub2ModeUpstream = "upstream"
)

type Sub2API struct {
	client         *http.Client
	credentials    *credentials.Service
	profiles       *capabilityProfileCache
	diagnostics    *diagnosticSession
	authCapability string
	credentialID   *int64
}

func (c *Sub2API) collectCustomerSession(ctx context.Context, base string, supplier domain.Supplier,
	credential domain.CredentialPayload) (domain.CatalogSnapshot, error) {
	role := authCustomerSession
	warnings := make([]string, 0)
	balance, balanceWarnings := c.collectCustomerBalance(ctx, base, supplier, credential, role)
	warnings = append(warnings, balanceWarnings...)
	groups := make(map[int64]sub2CustomerGroup)
	groupSelection, groupBody, groupsErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "customer_groups", "customer_groups", role,
		metadataValue(supplier, "sub2api_customer_groups_path", ""),
		[]string{"/api/v1/groups/available"}, credential, 4<<20, validateSub2CustomerGroups)
	recordCapabilityFromRequest(c.diagnostics, role, c.credentialID, "customer_groups", groupsErr)
	if groupsErr != nil {
		warnings = append(warnings, "customer group details are temporarily unavailable: "+ErrorClass(groupsErr))
	} else {
		var listed []sub2CustomerGroup
		if decodeErr := decodeSub2Envelope(groupBody, &listed); decodeErr != nil {
			groupsErr = &DiagnosticError{Class: "contract_changed", Operation: "customer_groups", Cause: decodeErr}
			warnings = append(warnings, "customer group details could not be decoded")
		} else {
			for _, group := range listed {
				groups[group.ID] = group
			}
		}
	}

	var channels []sub2CustomerChannel
	channelSelection, channelBody, channelsErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "customer_channels", "customer_channels", role,
		metadataValue(supplier, "sub2api_customer_catalog_path", ""),
		[]string{"/api/v1/channels/available"}, credential, maxResponseBytes, validateSub2CustomerChannels)
	recordCapabilityFromRequest(c.diagnostics, role, c.credentialID, "customer_channels", channelsErr)
	if channelsErr != nil {
		warnings = append(warnings, "optional customer channels are unavailable; group and key catalogs will be used: "+ErrorClass(channelsErr))
	} else if decoded, decodeErr := decodeSub2CustomerChannels(channelBody); decodeErr != nil {
		channelsErr = &DiagnosticError{Class: "contract_changed", Operation: "customer_channels", Cause: decodeErr}
		warnings = append(warnings, "optional customer channels could not be decoded")
	} else {
		channels = decoded
		for id, group := range customerGroupsFromChannels(channels) {
			if _, exists := groups[id]; !exists {
				groups[id] = group
			}
		}
	}
	catalogErr := preferredCapabilityError(groupsErr, channelsErr)
	if groupsErr == nil || channelsErr == nil {
		catalogErr = nil
	}
	// Keep the aggregate capability for existing clients while exposing the
	// two underlying capabilities independently.
	recordDerivedCapability(c.diagnostics, role, c.credentialID, "customer_catalog", catalogErr, false)
	if groupsErr != nil && channelsErr != nil {
		return customerBalanceOnlySnapshot(supplier.ID, base, balance, warnings),
			fmt.Errorf("fetch Sub2API customer groups and optional channels: %w", catalogErr)
	}

	rates := make(map[string]json.Number)
	_, ratesBody, ratesErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "customer_rates", "customer_rates", role,
		metadataValue(supplier, "sub2api_customer_rates_path", ""),
		[]string{"/api/v1/groups/rates"}, credential, 2<<20, validateSub2CustomerRates)
	recordCapabilityFromRequest(c.diagnostics, role, c.credentialID, "customer_rates", ratesErr)
	if ratesErr != nil {
		warnings = append(warnings, "customer-specific group rates are temporarily unavailable; default group rates are shown: "+ErrorClass(ratesErr))
	} else if decodeErr := decodeSub2Envelope(ratesBody, &rates); decodeErr != nil {
		warnings = append(warnings, "customer-specific group rates could not be decoded")
	}

	groupOffers, modelOffers := buildSub2CustomerCatalog(channels, groups, rates, nil)
	catalogSourcePath := groupSelection.Path
	if len(modelOffers) > 0 && channelSelection.Path != "" {
		catalogSourcePath = channelSelection.Path
	}
	if len(modelOffers) == 0 && len(groups) > 0 {
		keyModels, keySourcePath, keyWarnings := c.collectCustomerKeyModels(ctx, base, supplier, credential, groups)
		warnings = append(warnings, keyWarnings...)
		groupOffers, modelOffers = buildSub2CustomerCatalog(channels, groups, rates, keyModels)
		if len(modelOffers) > 0 {
			catalogSourcePath = keySourcePath
		}
	}
	if len(groupOffers) == 0 || len(modelOffers) == 0 {
		return customerBalanceOnlySnapshot(supplier.ID, base, balance, warnings), &DiagnosticError{Class: "contract_changed", Operation: "customer_catalog",
			Cause: errors.New("customer catalog contains no usable group/model intersection")}
	}

	return domain.CatalogSnapshot{
		SupplierID: supplier.ID,
		Groups:     groupOffers,
		Models:     modelOffers,
		Balance:    balance,
		Evidence: domain.Evidence{
			SourceType: "authenticated_customer_session", SourceURL: base + catalogSourcePath,
			Adapter: c.Name(), Confidence: 94, ObservedAt: time.Now().UTC(),
			ResponseHash: sub2CatalogHash(groupOffers, modelOffers), Warnings: warnings,
		},
	}, nil
}

func (c *Sub2API) collectCustomerBalance(ctx context.Context, base string, supplier domain.Supplier,
	credential domain.CredentialPayload, role string) (*domain.BalanceState, []string) {
	warnings := make([]string, 0)
	_, profileBody, profileErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "customer_balance", "customer_balance", role,
		metadataValue(supplier, "sub2api_customer_balance_path", ""),
		[]string{"/api/v1/auth/me", "/api/v1/user/profile", "/api/v1/user"}, credential, 2<<20, validateSub2CustomerProfile)
	recordCapabilityFromRequest(c.diagnostics, role, c.credentialID, "customer_balance", profileErr)
	var balance *domain.BalanceState
	if profileErr != nil {
		warnings = append(warnings, "customer balance is temporarily unavailable: "+ErrorClass(profileErr))
	} else if parsed, parseErr := parseSub2CustomerProfile(profileBody, time.Now().UTC()); parseErr == nil {
		balance = parsed
	} else {
		warnings = append(warnings, "customer balance response could not be decoded")
	}

	usageSelection, usageBody, usageErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "customer_usage", "customer_usage", role,
		metadataValue(supplier, "sub2api_customer_usage_path", ""),
		[]string{"/api/v1/usage", "/api/v1/usage/dashboard/stats", "/api/v1/usage/stats?period=today", "/api/v1/usage/dashboard/snapshot-v2"}, credential, 4<<20, validateSub2CustomerUsage)
	recordCapabilityFromRequest(c.diagnostics, role, c.credentialID, "customer_usage", usageErr)
	if usageErr != nil {
		warnings = append(warnings, "customer 24-hour usage is temporarily unavailable: "+ErrorClass(usageErr))
	} else if balance != nil {
		if usage, parseErr := parseSub2CustomerUsage(usageBody); parseErr == nil {
			balance.DailySpend = canonicalSub2Number(usage.TodayActualCost)
			balance.DailyRequestCount = &usage.TodayRequests
			if usage.HasLifetime {
				balance.HistoricalSpend = canonicalSub2Number(usage.TotalActualCost)
				balance.RequestCount = &usage.TotalRequests
			}
			balance.UsageSource = usageSelection.Path
			now := time.Now().UTC()
			balance.UsageObservedAt = &now
		} else {
			warnings = append(warnings, "customer usage response could not be decoded")
		}
	}
	return balance, warnings
}

func customerBalanceOnlySnapshot(supplierID int64, base string, balance *domain.BalanceState, warnings []string) domain.CatalogSnapshot {
	return domain.CatalogSnapshot{SupplierID: supplierID, Balance: balance, Evidence: domain.Evidence{
		SourceType: "authenticated_customer_session", SourceURL: base, Adapter: "sub2api",
		ObservedAt: time.Now().UTC(), Warnings: warnings,
	}}
}

func NewSub2API(client *http.Client, credentialService *credentials.Service) *Sub2API {
	return &Sub2API{client: client, credentials: credentialService, profiles: newCapabilityProfileCache()}
}

func (c *Sub2API) Name() string { return "sub2api" }

type sub2HTTPError struct {
	Path       string
	StatusCode int
	Status     string
}

func (e *sub2HTTPError) Error() string {
	return fmt.Sprintf("Sub2API endpoint %s returned %s", e.Path, e.Status)
}

func (e *sub2HTTPError) ErrorClass() string {
	if e == nil {
		return "unknown_error"
	}
	return classifyHTTPStatus(e.StatusCode)
}

type sub2Envelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type sub2AdminGroup struct {
	ID                           int64    `json:"id"`
	Name                         string   `json:"name"`
	Platform                     string   `json:"platform"`
	Status                       string   `json:"status"`
	RateMultiplier               float64  `json:"rate_multiplier"`
	PeakRateEnabled              bool     `json:"peak_rate_enabled"`
	PeakStart                    string   `json:"peak_start"`
	PeakEnd                      string   `json:"peak_end"`
	PeakRateMultiplier           float64  `json:"peak_rate_multiplier"`
	ImageRateIndependent         bool     `json:"image_rate_independent"`
	ImageRateMultiplier          float64  `json:"image_rate_multiplier"`
	VideoRateIndependent         bool     `json:"video_rate_independent"`
	VideoRateMultiplier          float64  `json:"video_rate_multiplier"`
	ImagePrice1K                 *float64 `json:"image_price_1k"`
	ImagePrice2K                 *float64 `json:"image_price_2k"`
	ImagePrice4K                 *float64 `json:"image_price_4k"`
	VideoPrice480P               *float64 `json:"video_price_480p"`
	VideoPrice720P               *float64 `json:"video_price_720p"`
	VideoPrice1080P              *float64 `json:"video_price_1080p"`
	WebSearchPricePerCall        *float64 `json:"web_search_price_per_call"`
	SubscriptionType             string   `json:"subscription_type"`
	DailyLimitUSD                *float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD               *float64 `json:"weekly_limit_usd"`
	MonthlyLimitUSD              *float64 `json:"monthly_limit_usd"`
	AllowImageGeneration         bool     `json:"allow_image_generation"`
	AllowBatchImageGeneration    bool     `json:"allow_batch_image_generation"`
	BatchImageDiscountMultiplier float64  `json:"batch_image_discount_multiplier"`
}

type sub2AccountGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type sub2AdminAccount struct {
	ID                      int64              `json:"id"`
	Name                    string             `json:"name"`
	Platform                string             `json:"platform"`
	Type                    string             `json:"type"`
	Status                  string             `json:"status"`
	Schedulable             bool               `json:"schedulable"`
	RateMultiplier          float64            `json:"rate_multiplier"`
	ErrorMessage            string             `json:"error_message"`
	LastUsedAt              *time.Time         `json:"last_used_at"`
	ExpiresAt               *int64             `json:"expires_at"`
	RateLimitResetAt        *time.Time         `json:"rate_limit_reset_at"`
	OverloadUntil           *time.Time         `json:"overload_until"`
	TempUnschedulableUntil  *time.Time         `json:"temp_unschedulable_until"`
	TempUnschedulableReason string             `json:"temp_unschedulable_reason"`
	Concurrency             int                `json:"concurrency"`
	CurrentConcurrency      int                `json:"current_concurrency"`
	CredentialsStatus       map[string]bool    `json:"credentials_status"`
	GroupIDs                []int64            `json:"group_ids"`
	Groups                  []sub2AccountGroup `json:"groups"`
	QuotaLimit              *float64           `json:"quota_limit"`
	QuotaUsed               *float64           `json:"quota_used"`
	QuotaDailyLimit         *float64           `json:"quota_daily_limit"`
	QuotaDailyUsed          *float64           `json:"quota_daily_used"`
	QuotaWeeklyLimit        *float64           `json:"quota_weekly_limit"`
	QuotaWeeklyUsed         *float64           `json:"quota_weekly_used"`
}

type sub2AccountPage struct {
	Items    []sub2AdminAccount `json:"items"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
	Pages    int                `json:"pages"`
}

type sub2AccountStats struct {
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	StandardCost float64 `json:"standard_cost"`
	UserCost     float64 `json:"user_cost"`
}

type sub2TodayStatsData struct {
	Stats map[string]sub2AccountStats `json:"stats"`
}

type sub2Dashboard struct {
	TotalAccounts     int64   `json:"total_accounts"`
	NormalAccounts    int64   `json:"normal_accounts"`
	ErrorAccounts     int64   `json:"error_accounts"`
	RateLimitAccounts int64   `json:"ratelimit_accounts"`
	OverloadAccounts  int64   `json:"overload_accounts"`
	TodayRequests     int64   `json:"today_requests"`
	TodayTokens       int64   `json:"today_tokens"`
	TodayAccountCost  float64 `json:"today_account_cost"`
	TotalAccountCost  float64 `json:"total_account_cost"`
}

type sub2KeyBilling struct {
	Object                  string     `json:"object"`
	SchemaVersion           int        `json:"schema_version"`
	BillingScope            string     `json:"billing_scope"`
	GroupRateMultiplier     *float64   `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64   `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  *float64   `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool       `json:"peak_rate_enabled"`
	PeakStart               *string    `json:"peak_start"`
	PeakEnd                 *string    `json:"peak_end"`
	PeakRateMultiplier      *float64   `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64   `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier *float64   `json:"effective_rate_multiplier"`
	Timezone                *string    `json:"timezone"`
	ObservedAt              *time.Time `json:"observed_at"`
}

type sub2UsageNumber struct {
	Limit     json.RawMessage `json:"limit"`
	Used      json.RawMessage `json:"used"`
	Remaining json.RawMessage `json:"remaining"`
	Unit      string          `json:"unit"`
}

type sub2UsageSummary struct {
	ActualCost json.RawMessage `json:"actual_cost"`
}

type sub2KeyUsage struct {
	Mode      string           `json:"mode"`
	IsValid   *bool            `json:"isValid"`
	Status    string           `json:"status"`
	PlanName  string           `json:"planName"`
	Unit      string           `json:"unit"`
	Remaining json.RawMessage  `json:"remaining"`
	Balance   json.RawMessage  `json:"balance"`
	ExpiresAt *time.Time       `json:"expires_at"`
	Quota     *sub2UsageNumber `json:"quota"`
	Usage     *struct {
		Today sub2UsageSummary `json:"today"`
		Total sub2UsageSummary `json:"total"`
	} `json:"usage"`
	Subscription *struct {
		DailyUsageUSD   json.RawMessage `json:"daily_usage_usd"`
		WeeklyUsageUSD  json.RawMessage `json:"weekly_usage_usd"`
		MonthlyUsageUSD json.RawMessage `json:"monthly_usage_usd"`
		DailyLimitUSD   json.RawMessage `json:"daily_limit_usd"`
		WeeklyLimitUSD  json.RawMessage `json:"weekly_limit_usd"`
		MonthlyLimitUSD json.RawMessage `json:"monthly_limit_usd"`
		ExpiresAt       *time.Time      `json:"expires_at"`
	} `json:"subscription"`
}

type sub2CustomerGroup struct {
	ID                 int64       `json:"id"`
	Name               string      `json:"name"`
	Description        string      `json:"description"`
	Platform           string      `json:"platform"`
	SubscriptionType   string      `json:"subscription_type"`
	RateMultiplier     json.Number `json:"rate_multiplier"`
	PeakRateEnabled    bool        `json:"peak_rate_enabled"`
	PeakStart          string      `json:"peak_start"`
	PeakEnd            string      `json:"peak_end"`
	PeakRateMultiplier json.Number `json:"peak_rate_multiplier"`
	IsExclusive        bool        `json:"is_exclusive"`
}

type sub2CustomerPricingInterval struct {
	MinTokens       int64        `json:"min_tokens"`
	MaxTokens       *int64       `json:"max_tokens"`
	TierLabel       string       `json:"tier_label"`
	InputPrice      *json.Number `json:"input_price"`
	OutputPrice     *json.Number `json:"output_price"`
	CacheWritePrice *json.Number `json:"cache_write_price"`
	CacheReadPrice  *json.Number `json:"cache_read_price"`
	PerRequestPrice *json.Number `json:"per_request_price"`
}

type sub2CustomerModelPricing struct {
	BillingMode      string                        `json:"billing_mode"`
	InputPrice       *json.Number                  `json:"input_price"`
	OutputPrice      *json.Number                  `json:"output_price"`
	CacheWritePrice  *json.Number                  `json:"cache_write_price"`
	CacheReadPrice   *json.Number                  `json:"cache_read_price"`
	ImageInputPrice  *json.Number                  `json:"image_input_price"`
	ImageOutputPrice *json.Number                  `json:"image_output_price"`
	PerRequestPrice  *json.Number                  `json:"per_request_price"`
	Intervals        []sub2CustomerPricingInterval `json:"intervals"`
}

type sub2CustomerModel struct {
	Name     string                    `json:"name"`
	Platform string                    `json:"platform"`
	Pricing  *sub2CustomerModelPricing `json:"pricing"`
}

type sub2CustomerChannelSection struct {
	Platform        string              `json:"platform"`
	Groups          []sub2CustomerGroup `json:"groups"`
	SupportedModels []sub2CustomerModel `json:"supported_models"`
}

type sub2CustomerChannel struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Platforms   []sub2CustomerChannelSection `json:"platforms"`
}

type sub2CustomerUsage struct {
	TodayActualCost json.Number `json:"today_actual_cost"`
	TotalActualCost json.Number `json:"total_actual_cost"`
	TodayCost       json.Number `json:"today_cost"`
	TotalCost       json.Number `json:"total_cost"`
	TodayRequests   int64       `json:"today_requests"`
	TotalRequests   int64       `json:"total_requests"`
	TodayTokens     int64       `json:"today_tokens"`
	HasLifetime     bool        `json:"-"`
}

type sub2CustomerKey struct {
	Key     string `json:"key"`
	APIKey  string `json:"api_key"`
	Status  string `json:"status"`
	GroupID int64  `json:"group_id"`
}

type sub2CustomerKeyPage struct {
	Items    []sub2CustomerKey `json:"items"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Pages    int               `json:"pages"`
}

func (c *Sub2API) Collect(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error) {
	snapshot, _, err := c.CollectDetailed(ctx, supplier)
	return snapshot, err
}

func (c *Sub2API) CollectDetailed(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error) {
	session := newDiagnosticSession(supplier.ID)
	snapshot, err := c.collectDetailed(ctx, supplier, session)
	session.diagnostics.CatalogSucceeded = err == nil && len(snapshot.Models) > 0
	session.diagnostics.BalanceSucceeded = snapshot.Balance != nil && snapshot.Balance.Amount != "" && snapshot.Balance.Currency != ""
	if len(snapshot.Evidence.Warnings) > 0 {
		session.diagnostics.Warnings = append(session.diagnostics.Warnings, snapshot.Evidence.Warnings...)
	}
	if session.diagnostics.CatalogSucceeded {
		session.diagnostics.CatalogProof = sub2CatalogProof(snapshot, session.diagnostics,
			sub2CollectionMode(supplier), supplier.CredentialID != nil)
	}
	return snapshot, session.result(), err
}

func (c *Sub2API) collectDetailed(ctx context.Context, supplier domain.Supplier, session *diagnosticSession) (domain.CatalogSnapshot, error) {
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	if c.client == nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("Sub2API HTTP client is required")
	}
	client, err := netguard.SameOriginClient(c.client, base)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	credential, payload, err := c.loadCredential(ctx, supplier)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	requester := *c
	requester.client = client
	requester.diagnostics = session
	requester.credentialID = supplier.CredentialID
	c = &requester
	requestCtx, cancel := requestDeadline(ctx, 35*time.Second)
	defer cancel()

	mode := sub2CollectionMode(supplier)
	var snapshot domain.CatalogSnapshot
	var collectErr error
	switch mode {
	case sub2ModeAdmin:
		c.authCapability = authSiteAdmin
		snapshot, collectErr = c.collectAdmin(requestCtx, base, supplier, payload)
		recordLatestCapability(session, c.authCapability, supplier.CredentialID, collectErr)
	case sub2ModeUpstream:
		c.authCapability = authBusinessAPIKey
		if supplier.CredentialID != nil {
			c.authCapability = credentialCapability(credential.Kind, payload, false)
		}
		if c.authCapability == authCustomerSession {
			snapshot, collectErr = c.collectCustomerSession(requestCtx, base, supplier, payload)
		} else {
			snapshot, collectErr = c.collectNegotiatedUpstream(requestCtx, base, supplier, payload)
		}
		recordLatestCapability(session, c.authCapability, supplier.CredentialID, collectErr)
	case sub2ModeAuto:
		c.authCapability = authSiteAdmin
		adminSnapshot, adminErr := c.collectAdmin(requestCtx, base, supplier, payload)
		recordLatestCapability(session, c.authCapability, supplier.CredentialID, adminErr)
		if adminErr == nil {
			return adminSnapshot, nil
		}
		c.authCapability = authBusinessAPIKey
		if supplier.CredentialID != nil {
			c.authCapability = credentialCapability(credential.Kind, payload, false)
		}
		var upstreamSnapshot domain.CatalogSnapshot
		var upstreamErr error
		if c.authCapability == authCustomerSession {
			upstreamSnapshot, upstreamErr = c.collectCustomerSession(requestCtx, base, supplier, payload)
		} else {
			upstreamSnapshot, upstreamErr = c.collectNegotiatedUpstream(requestCtx, base, supplier, payload)
		}
		recordLatestCapability(session, c.authCapability, supplier.CredentialID, upstreamErr)
		if upstreamErr == nil {
			upstreamSnapshot.Evidence.Warnings = append(upstreamSnapshot.Evidence.Warnings,
				"管理接口不可用，已自动使用普通 Sub2API Key 采集模式")
			return upstreamSnapshot, nil
		}
		if upstreamSnapshot.Balance != nil {
			snapshot = upstreamSnapshot
		} else if adminSnapshot.Balance != nil {
			snapshot = adminSnapshot
		}
		collectErr = fmt.Errorf("Sub2API auto detection failed (admin: %v; upstream: %w)", adminErr, upstreamErr)
	default:
		return domain.CatalogSnapshot{}, fmt.Errorf("unsupported Sub2API collection mode %q", mode)
	}
	if collectErr != nil {
		return snapshot, sub2CredentialGuidance(collectErr, supplier.CredentialID != nil, mode, c.authCapability)
	}
	return snapshot, nil
}

func sub2CredentialGuidance(err error, credentialAttached bool, mode, capability string) error {
	var httpErr *sub2HTTPError
	if !errors.As(err, &httpErr) || (httpErr.StatusCode != http.StatusUnauthorized && httpErr.StatusCode != http.StatusForbidden) {
		return err
	}
	if credentialAttached {
		if mode == sub2ModeUpstream && capability == authCustomerSession {
			return fmt.Errorf("Sub2API web session could not read the customer-visible catalog; re-pair an active customer session, or attach a business API Key for /v1/models: %w", err)
		}
		return fmt.Errorf("Sub2API credential was rejected or expired; replace the API Key or re-pair an authorized site-admin browser session: %w", err)
	}
	if mode == sub2ModeAdmin {
		return fmt.Errorf("Sub2API admin pricing requires an authorized site-admin browser pairing or admin token: %w", err)
	}
	return fmt.Errorf("Sub2API pricing is credential-restricted; attach an API Key (or use an authorized site-admin browser pairing in admin mode): %w", err)
}

func (c *Sub2API) loadCredential(ctx context.Context, supplier domain.Supplier) (domain.Credential, domain.CredentialPayload, error) {
	if supplier.CredentialID == nil {
		return domain.Credential{}, domain.CredentialPayload{}, nil
	}
	if c.credentials == nil {
		return domain.Credential{}, domain.CredentialPayload{}, fmt.Errorf("Sub2API credential service is required")
	}
	credential, payload, err := c.credentials.Load(ctx, *supplier.CredentialID)
	if err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, fmt.Errorf("load supplier credential: %w", err)
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, supplier.BaseURL); err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, fmt.Errorf("supplier credential origin mismatch: %w", err)
	}
	return credential, payload, nil
}

func sub2CollectionMode(supplier domain.Supplier) string {
	for _, key := range []string{"sub2api_mode", "mode"} {
		if value := strings.ToLower(strings.TrimSpace(supplier.Metadata[key])); value != "" {
			switch value {
			case "admin", "sub2api_admin":
				return sub2ModeAdmin
			case "upstream", "key", "sub2api_upstream":
				return sub2ModeUpstream
			case "auto":
				return sub2ModeAuto
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(supplier.Adapter)) {
	case "sub2api_admin":
		return sub2ModeAdmin
	case "sub2api_upstream":
		return sub2ModeUpstream
	default:
		return sub2ModeAuto
	}
}

func (c *Sub2API) collectAdmin(ctx context.Context, base string, supplier domain.Supplier, credential domain.CredentialPayload) (domain.CatalogSnapshot, error) {
	warnings := make([]string, 0)

	groupsPath := metadataPath(supplier, "sub2api_admin_groups_path", "/api/v1/admin/groups/all")
	groupsBody, err := c.sub2Request(ctx, base, groupsPath, http.MethodGet, nil, credential)
	if err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("fetch Sub2API groups: %w", err)
	}
	var groups []sub2AdminGroup
	if err := decodeSub2Envelope(groupsBody, &groups); err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("decode Sub2API groups: %w", err)
	}
	if len(groups) == 0 {
		return domain.CatalogSnapshot{}, fmt.Errorf("Sub2API admin returned no groups")
	}

	accountsPath := metadataPath(supplier, "sub2api_admin_accounts_path", "/api/v1/admin/accounts?page=1&page_size=1000&lite=true")
	accountsBody, err := c.sub2Request(ctx, base, accountsPath, http.MethodGet, nil, credential)
	if err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("fetch Sub2API accounts: %w", err)
	}
	var accountPage sub2AccountPage
	if err := decodeSub2Envelope(accountsBody, &accountPage); err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("decode Sub2API accounts: %w", err)
	}
	if accountPage.Pages > 1 {
		warnings = append(warnings, fmt.Sprintf("账号池超过单次采集上限：已读取 %d/%d 个账号", len(accountPage.Items), accountPage.Total))
	}

	dashboardPath := metadataPath(supplier, "sub2api_admin_dashboard_path", "/api/v1/admin/dashboard/stats")
	dashboardBody, err := c.sub2Request(ctx, base, dashboardPath, http.MethodGet, nil, credential)
	var dashboard sub2Dashboard
	if err != nil {
		warnings = append(warnings, "Sub2API 仪表盘统计暂不可用："+err.Error())
	} else {
		if decodeErr := decodeSub2Envelope(dashboardBody, &dashboard); decodeErr != nil {
			warnings = append(warnings, "Sub2API 仪表盘统计无法解析："+decodeErr.Error())
		}
	}

	accountIDs := make([]int64, 0, len(accountPage.Items))
	for _, account := range accountPage.Items {
		accountIDs = append(accountIDs, account.ID)
	}
	todayPath := metadataPath(supplier, "sub2api_admin_today_stats_path", "/api/v1/admin/accounts/today-stats/batch")
	todayBody, err := c.sub2Request(ctx, base, todayPath, http.MethodPost, map[string]any{"account_ids": accountIDs}, credential)
	todayStats := make(map[string]sub2AccountStats)
	if err != nil {
		warnings = append(warnings, "Sub2API 今日账号成本暂不可用："+err.Error())
	} else {
		var data sub2TodayStatsData
		if decodeErr := decodeSub2Envelope(todayBody, &data); decodeErr != nil {
			warnings = append(warnings, "Sub2API 今日账号成本无法解析："+decodeErr.Error())
		} else {
			todayStats = data.Stats
		}
	}

	groupModels, modelWarnings := c.collectAdminModels(ctx, base, supplier, credential, groups, accountPage.Items)
	warnings = append(warnings, modelWarnings...)
	groupOffers, modelOffers := buildSub2AdminOffers(groups, accountPage.Items, todayStats, dashboard, groupModels)
	if accountPage.Pages > 1 || len(modelWarnings) > 0 {
		for index := range groupOffers {
			if groupOffers[index].Metadata == nil {
				groupOffers[index].Metadata = map[string]string{}
			}
			groupOffers[index].Metadata["catalog_incomplete"] = "true"
		}
	}
	warnings = append(warnings, fmt.Sprintf(
		"账号池：%d 个账号，正常 %d，错误 %d，限流 %d，过载 %d；今日账号成本 %s USD",
		dashboard.TotalAccounts, dashboard.NormalAccounts, dashboard.ErrorAccounts,
		dashboard.RateLimitAccounts, dashboard.OverloadAccounts, number(dashboard.TodayAccountCost),
	))
	warnings = append(warnings, "Sub2API 账号额度包含多种窗口/积分语义，不会被错误合并为单一美元余额")

	now := time.Now().UTC()
	return domain.CatalogSnapshot{
		SupplierID: supplier.ID,
		Groups:     groupOffers,
		Models:     modelOffers,
		Evidence: domain.Evidence{
			SourceType:   "authenticated_admin_api",
			SourceURL:    base + groupsPath,
			Adapter:      c.Name(),
			Confidence:   95,
			ObservedAt:   now,
			ResponseHash: sub2CatalogHash(groupOffers, modelOffers),
			Warnings:     warnings,
		},
	}, nil
}

func (c *Sub2API) collectAdminModels(
	ctx context.Context,
	base string,
	supplier domain.Supplier,
	credential domain.CredentialPayload,
	groups []sub2AdminGroup,
	accounts []sub2AdminAccount,
) (map[int64][]string, []string) {
	representative := make(map[int64]int64)
	for _, account := range accounts {
		for _, groupID := range accountGroupIDs(account) {
			if _, exists := representative[groupID]; !exists || (account.Status == "active" && account.Schedulable) {
				representative[groupID] = account.ID
			}
		}
	}
	modelsByAccount := make(map[int64][]string)
	modelsByGroup := make(map[int64][]string)
	warnings := make([]string, 0)
	for _, group := range groups {
		accountID, ok := representative[group.ID]
		if !ok {
			continue
		}
		models, cached := modelsByAccount[accountID]
		if !cached {
			pathTemplate := metadataValue(supplier, "sub2api_admin_account_models_path", "/api/v1/admin/accounts/{id}/models")
			path := strings.ReplaceAll(pathTemplate, "{id}", strconv.FormatInt(accountID, 10))
			body, err := c.sub2Request(ctx, base, path, http.MethodGet, nil, credential)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("分组 %s 的模型目录暂不可用：%v", group.Name, err))
				modelsByAccount[accountID] = nil
				continue
			}
			parsed, parseWarnings := parseSub2Models(body)
			for _, warning := range parseWarnings {
				warnings = append(warnings, fmt.Sprintf("分组 %s：%s", group.Name, warning))
			}
			models = make([]string, 0, len(parsed))
			for _, model := range parsed {
				models = append(models, model.ExternalID)
			}
			models = uniqueSorted(models)
			modelsByAccount[accountID] = models
		}
		modelsByGroup[group.ID] = append(modelsByGroup[group.ID], models...)
	}
	for groupID, modelIDs := range modelsByGroup {
		modelsByGroup[groupID] = uniqueSorted(modelIDs)
	}
	return modelsByGroup, warnings
}

func buildSub2AdminOffers(
	groups []sub2AdminGroup,
	accounts []sub2AdminAccount,
	today map[string]sub2AccountStats,
	dashboard sub2Dashboard,
	groupModels map[int64][]string,
) ([]domain.GroupOffer, []domain.ModelOffer) {
	type groupStats struct {
		total, active, schedulable, credentialReady, errorCount int
		currentConcurrency, concurrencyLimit                    int
		accountCost, standardCost, userCost                     float64
	}
	stats := make(map[int64]*groupStats)
	for _, group := range groups {
		stats[group.ID] = &groupStats{}
	}
	for _, account := range accounts {
		accountStats := today[strconv.FormatInt(account.ID, 10)]
		for _, groupID := range accountGroupIDs(account) {
			item := stats[groupID]
			if item == nil {
				continue
			}
			item.total++
			if account.Status == "active" {
				item.active++
			}
			if account.Status == "error" {
				item.errorCount++
			}
			if account.Status == "active" && account.Schedulable {
				item.schedulable++
			}
			if credentialPresent(account.CredentialsStatus) {
				item.credentialReady++
			}
			item.currentConcurrency += account.CurrentConcurrency
			item.concurrencyLimit += account.Concurrency
			item.accountCost += accountStats.Cost
			item.standardCost += accountStats.StandardCost
			item.userCost += accountStats.UserCost
		}
	}

	groupOffers := make([]domain.GroupOffer, 0, len(groups))
	modelOffers := make([]domain.ModelOffer, 0)
	for _, group := range groups {
		metadata := map[string]string{
			"platform":                group.Platform,
			"status":                  group.Status,
			"subscription_type":       group.SubscriptionType,
			"peak_rate_enabled":       strconv.FormatBool(group.PeakRateEnabled),
			"image_rate_independent":  strconv.FormatBool(group.ImageRateIndependent),
			"image_rate_multiplier":   number(group.ImageRateMultiplier),
			"video_rate_independent":  strconv.FormatBool(group.VideoRateIndependent),
			"video_rate_multiplier":   number(group.VideoRateMultiplier),
			"allow_image_generation":  strconv.FormatBool(group.AllowImageGeneration),
			"allow_batch_image":       strconv.FormatBool(group.AllowBatchImageGeneration),
			"batch_image_discount":    number(group.BatchImageDiscountMultiplier),
			"pool_total_accounts":     strconv.FormatInt(dashboard.TotalAccounts, 10),
			"pool_normal_accounts":    strconv.FormatInt(dashboard.NormalAccounts, 10),
			"pool_error_accounts":     strconv.FormatInt(dashboard.ErrorAccounts, 10),
			"pool_ratelimit_accounts": strconv.FormatInt(dashboard.RateLimitAccounts, 10),
			"pool_overload_accounts":  strconv.FormatInt(dashboard.OverloadAccounts, 10),
			"pool_today_account_cost": number(dashboard.TodayAccountCost),
			"pool_total_account_cost": number(dashboard.TotalAccountCost),
		}
		if group.PeakRateEnabled {
			metadata["peak_start"] = group.PeakStart
			metadata["peak_end"] = group.PeakEnd
			metadata["peak_rate_multiplier"] = number(group.PeakRateMultiplier)
		}
		setOptionalFloat(metadata, "image_price_1k", group.ImagePrice1K)
		setOptionalFloat(metadata, "image_price_2k", group.ImagePrice2K)
		setOptionalFloat(metadata, "image_price_4k", group.ImagePrice4K)
		setOptionalFloat(metadata, "video_price_480p", group.VideoPrice480P)
		setOptionalFloat(metadata, "video_price_720p", group.VideoPrice720P)
		setOptionalFloat(metadata, "video_price_1080p", group.VideoPrice1080P)
		setOptionalFloat(metadata, "web_search_price_per_call", group.WebSearchPricePerCall)
		setOptionalFloat(metadata, "daily_limit_usd", group.DailyLimitUSD)
		setOptionalFloat(metadata, "weekly_limit_usd", group.WeeklyLimitUSD)
		setOptionalFloat(metadata, "monthly_limit_usd", group.MonthlyLimitUSD)
		if aggregate := stats[group.ID]; aggregate != nil {
			metadata["account_count"] = strconv.Itoa(aggregate.total)
			metadata["active_account_count"] = strconv.Itoa(aggregate.active)
			metadata["schedulable_account_count"] = strconv.Itoa(aggregate.schedulable)
			metadata["credential_ready_count"] = strconv.Itoa(aggregate.credentialReady)
			metadata["error_account_count"] = strconv.Itoa(aggregate.errorCount)
			metadata["current_concurrency"] = strconv.Itoa(aggregate.currentConcurrency)
			metadata["concurrency_limit"] = strconv.Itoa(aggregate.concurrencyLimit)
			metadata["today_account_cost"] = number(aggregate.accountCost)
			metadata["today_standard_cost"] = number(aggregate.standardCost)
			metadata["today_user_cost"] = number(aggregate.userCost)
		}
		modelIDs := uniqueSorted(groupModels[group.ID])
		groupOffers = append(groupOffers, domain.GroupOffer{
			ExternalID: strconv.FormatInt(group.ID, 10),
			Name:       group.Name,
			Ratio:      number(group.RateMultiplier),
			ModelIDs:   modelIDs,
			Metadata:   metadata,
		})
		for _, modelID := range modelIDs {
			priceFunction := sub2GroupPriceFunction(group)
			modelOffers = append(modelOffers, domain.ModelOffer{
				ExternalID:    modelID,
				Name:          modelID,
				Group:         group.Name,
				BillingMode:   "unknown",
				PriceFunction: priceFunction,
				Metadata: map[string]string{
					"platform":          group.Platform,
					"group_external_id": strconv.FormatInt(group.ID, 10),
				},
			})
		}
	}
	sort.Slice(groupOffers, func(i, j int) bool { return groupOffers[i].Name < groupOffers[j].Name })
	sort.Slice(modelOffers, func(i, j int) bool {
		if modelOffers[i].Group == modelOffers[j].Group {
			return modelOffers[i].Name < modelOffers[j].Name
		}
		return modelOffers[i].Group < modelOffers[j].Group
	})
	return groupOffers, modelOffers
}

func validateSub2CustomerChannels(body []byte) error {
	_, err := decodeSub2CustomerChannels(body)
	return err
}

func decodeSub2CustomerChannels(body []byte) ([]sub2CustomerChannel, error) {
	var channels []sub2CustomerChannel
	if err := decodeSub2Envelope(body, &channels); err != nil {
		return nil, err
	}
	return channels, nil
}

func validateSub2CustomerGroups(body []byte) error {
	var groups []sub2CustomerGroup
	if err := decodeSub2Envelope(body, &groups); err != nil {
		return err
	}
	return nil
}

func validateSub2CustomerRates(body []byte) error {
	var rates map[string]json.Number
	return decodeSub2Envelope(body, &rates)
}

func validateSub2CustomerIdentity(body []byte) error {
	root, err := decodeSub2DataRoot(body)
	if err != nil {
		return err
	}
	status, _ := firstSub2Scalar(root, "status", "user.status", "account.status", "profile.status")
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "disabled", "suspended", "banned", "deleted", "inactive":
		return fmt.Errorf("customer identity status is %s", status)
	}
	identity, _ := firstSub2Scalar(root, "id", "user_id", "user.id", "account.id", "profile.id")
	if validSub2Identity(identity) {
		return nil
	}
	identity, _ = firstSub2Scalar(root, "email", "username", "user.email", "user.username", "account.email", "profile.email")
	if strings.TrimSpace(identity) == "" {
		return errors.New("customer identity contains no stable user identifier")
	}
	return nil
}

func validateSub2CustomerProfile(body []byte) error {
	_, err := parseSub2CustomerProfile(body, time.Now().UTC())
	return err
}

func validateSub2CustomerUsage(body []byte) error {
	_, err := parseSub2CustomerUsage(body)
	return err
}

func validateSub2CustomerKeys(body []byte) error {
	var page sub2CustomerKeyPage
	if err := decodeSub2Envelope(body, &page); err == nil {
		return nil
	}
	var items []sub2CustomerKey
	return decodeSub2Envelope(body, &items)
}

func customerGroupsFromChannels(channels []sub2CustomerChannel) map[int64]sub2CustomerGroup {
	groups := make(map[int64]sub2CustomerGroup)
	for _, channel := range channels {
		for _, section := range channel.Platforms {
			for _, group := range section.Groups {
				groups[group.ID] = group
			}
		}
	}
	return groups
}

func buildSub2CustomerCatalog(channels []sub2CustomerChannel, groups map[int64]sub2CustomerGroup,
	rates map[string]json.Number, fallbackModels map[int64][]string) ([]domain.GroupOffer, []domain.ModelOffer) {
	groupModels := make(map[int64]map[string]struct{})
	groupChannels := make(map[int64]map[string]struct{})
	models := make(map[string]domain.ModelOffer)
	for id := range groups {
		groupModels[id] = make(map[string]struct{})
		groupChannels[id] = make(map[string]struct{})
	}
	for _, channel := range channels {
		for _, section := range channel.Platforms {
			for _, groupRef := range section.Groups {
				group := groupRef
				if configured, ok := groups[groupRef.ID]; ok {
					group = configured
				}
				if groupModels[group.ID] == nil {
					groupModels[group.ID] = make(map[string]struct{})
					groupChannels[group.ID] = make(map[string]struct{})
				}
				if strings.TrimSpace(channel.Name) != "" {
					groupChannels[group.ID][channel.Name] = struct{}{}
				}
				for _, model := range section.SupportedModels {
					name := strings.TrimSpace(model.Name)
					if name == "" {
						continue
					}
					groupModels[group.ID][name] = struct{}{}
					key := strconv.FormatInt(group.ID, 10) + "\x00" + name
					if _, exists := models[key]; exists {
						continue
					}
					billingMode := "unknown"
					priceFunction := domain.PriceFunction{Currency: "USD", Basis: "sub2api_customer_catalog",
						GroupMultiplier: customerGroupRatio(group, rates)}
					metadata := map[string]string{"platform": firstNonEmpty(model.Platform, section.Platform),
						"channel": channel.Name, "group_external_id": strconv.FormatInt(group.ID, 10)}
					modelPrice := ""
					if model.Pricing != nil {
						billingMode = firstNonEmpty(strings.TrimSpace(model.Pricing.BillingMode), "token")
						priceFunction = sub2CustomerPriceFunction(model.Pricing, priceFunction.GroupMultiplier)
						modelPrice = priceFunction.PerRequest
						if len(model.Pricing.Intervals) > 0 {
							if encoded, err := json.Marshal(model.Pricing.Intervals); err == nil {
								metadata["pricing_intervals"] = string(encoded)
							}
						}
					}
					models[key] = domain.ModelOffer{ExternalID: name, Name: name, Group: group.Name,
						BillingMode: billingMode, ModelPrice: modelPrice, Currency: "USD",
						PriceFunction: priceFunction, Metadata: metadata}
				}
			}
		}
	}
	for groupID, names := range fallbackModels {
		group, exists := groups[groupID]
		if !exists {
			continue
		}
		if groupModels[groupID] == nil {
			groupModels[groupID] = make(map[string]struct{})
			groupChannels[groupID] = make(map[string]struct{})
		}
		for _, rawName := range names {
			name := strings.TrimSpace(rawName)
			if name == "" {
				continue
			}
			groupModels[groupID][name] = struct{}{}
			key := strconv.FormatInt(groupID, 10) + "\x00" + name
			if _, exists := models[key]; exists {
				continue
			}
			models[key] = domain.ModelOffer{ExternalID: name, Name: name, Group: group.Name,
				BillingMode: "unknown", Currency: "USD",
				PriceFunction: domain.PriceFunction{Currency: "USD", Basis: "sub2api_customer_key_catalog",
					GroupMultiplier: customerGroupRatio(group, rates)},
				Metadata: map[string]string{"platform": group.Platform,
					"group_external_id": strconv.FormatInt(groupID, 10), "catalog_source": "customer_api_key"}}
		}
	}
	groupOffers := make([]domain.GroupOffer, 0, len(groups))
	for id, modelSet := range groupModels {
		group := groups[id]
		modelIDs := make([]string, 0, len(modelSet))
		for modelID := range modelSet {
			modelIDs = append(modelIDs, modelID)
		}
		channelNames := make([]string, 0, len(groupChannels[id]))
		for name := range groupChannels[id] {
			channelNames = append(channelNames, name)
		}
		sort.Strings(channelNames)
		metadata := map[string]string{
			"platform": group.Platform, "subscription_type": group.SubscriptionType,
			"is_exclusive": strconv.FormatBool(group.IsExclusive), "channels": strings.Join(channelNames, ", "),
			"peak_rate_enabled": strconv.FormatBool(group.PeakRateEnabled),
		}
		if len(modelSet) == 0 {
			metadata["model_coverage"] = "unknown_no_active_key"
		} else {
			metadata["model_coverage"] = "known"
		}
		if group.PeakRateEnabled {
			metadata["peak_start"] = group.PeakStart
			metadata["peak_end"] = group.PeakEnd
			metadata["peak_rate_multiplier"] = canonicalSub2Number(group.PeakRateMultiplier)
		}
		groupOffers = append(groupOffers, domain.GroupOffer{ExternalID: strconv.FormatInt(id, 10), Name: group.Name,
			Ratio: customerGroupRatio(group, rates), ModelIDs: uniqueSorted(modelIDs), Metadata: metadata})
	}
	modelOffers := make([]domain.ModelOffer, 0, len(models))
	for _, model := range models {
		modelOffers = append(modelOffers, model)
	}
	sort.Slice(groupOffers, func(i, j int) bool { return groupOffers[i].Name < groupOffers[j].Name })
	sort.Slice(modelOffers, func(i, j int) bool {
		if modelOffers[i].Group == modelOffers[j].Group {
			return modelOffers[i].Name < modelOffers[j].Name
		}
		return modelOffers[i].Group < modelOffers[j].Group
	})
	return groupOffers, modelOffers
}

func (c *Sub2API) collectCustomerKeyModels(ctx context.Context, base string, supplier domain.Supplier,
	credential domain.CredentialPayload, groups map[int64]sub2CustomerGroup) (map[int64][]string, string, []string) {
	const maximumGroups = 32
	keysPath := metadataPath(supplier, "sub2api_customer_keys_path", "/api/v1/keys?page=1&page_size=100")
	keysBody, err := c.sub2Request(ctx, base, keysPath, http.MethodGet, nil, credential)
	recordCapabilityFromRequest(c.diagnostics, authCustomerSession, c.credentialID, "customer_keys", err)
	if err != nil {
		return nil, keysPath, []string{"customer key catalog is temporarily unavailable: " + ErrorClass(err)}
	}
	var page sub2CustomerKeyPage
	if err := decodeSub2Envelope(keysBody, &page); err != nil {
		var items []sub2CustomerKey
		if arrayErr := decodeSub2Envelope(keysBody, &items); arrayErr != nil {
			return nil, keysPath, []string{"customer key catalog response could not be decoded"}
		}
		page.Items = items
	}
	representatives := make(map[int64]string)
	for _, item := range page.Items {
		status := strings.ToLower(strings.TrimSpace(item.Status))
		if status != "active" && status != "enabled" {
			continue
		}
		if _, exists := groups[item.GroupID]; !exists {
			continue
		}
		key := strings.TrimSpace(firstNonEmpty(item.Key, item.APIKey))
		if key == "" {
			continue
		}
		if _, exists := representatives[item.GroupID]; !exists {
			representatives[item.GroupID] = key
		}
	}
	groupIDs := make([]int64, 0, len(representatives))
	for groupID := range representatives {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	warnings := make([]string, 0)
	if len(groupIDs) > maximumGroups {
		groupIDs = groupIDs[:maximumGroups]
		warnings = append(warnings, "customer key model coverage was bounded to 32 groups for one collection")
	}
	modelsByGroup := make(map[int64][]string)
	modelsPath := metadataPath(supplier, "sub2api_customer_key_models_path", "/v1/models")
	for _, groupID := range groupIDs {
		key := representatives[groupID]
		keyCredential := domain.CredentialPayload{Authorization: "Bearer " + key,
			Headers: map[string]string{"X-API-Key": key}}
		response, requestErr := c.diagnostics.request(ctx, c.client, diagnosticRequest{
			EndpointType: "customer_key_catalog", AuthCapability: authCustomerSession,
			Method: http.MethodGet, URL: base + normalizedPath(modelsPath), Credential: keyCredential,
			MaxBytes: 4 << 20,
		})
		if requestErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			warnings = append(warnings, fmt.Sprintf("group %d model catalog is temporarily unavailable", groupID))
			continue
		}
		models, _ := parseSub2Models(response.Body)
		if len(models) == 0 {
			warnings = append(warnings, fmt.Sprintf("group %d model catalog returned no recognized models", groupID))
			continue
		}
		names := make([]string, 0, len(models))
		for _, model := range models {
			names = append(names, model.ExternalID)
		}
		modelsByGroup[groupID] = uniqueSorted(names)
	}
	if len(modelsByGroup) == 0 {
		warnings = append(warnings, "no active customer key produced a usable model catalog")
	}
	return modelsByGroup, keysPath, warnings
}

func customerGroupRatio(group sub2CustomerGroup, rates map[string]json.Number) string {
	if override, ok := rates[strconv.FormatInt(group.ID, 10)]; ok {
		if value := canonicalSub2Number(override); value != "" {
			return value
		}
	}
	return canonicalSub2Number(group.RateMultiplier)
}

func sub2CustomerPriceFunction(pricing *sub2CustomerModelPricing, groupRatio string) domain.PriceFunction {
	result := domain.PriceFunction{Currency: "USD", Basis: "sub2api_customer_catalog", GroupMultiplier: groupRatio,
		InputPer1M: sub2PerTokenToMillion(pricing.InputPrice), OutputPer1M: sub2PerTokenToMillion(pricing.OutputPrice),
		CacheWritePer1M: sub2PerTokenToMillion(pricing.CacheWritePrice), CacheReadPer1M: sub2PerTokenToMillion(pricing.CacheReadPrice),
		ImageInputPer1M: sub2PerTokenToMillion(pricing.ImageInputPrice), PerRequest: canonicalSub2NumberPointer(pricing.PerRequestPrice),
	}
	if imageOutput := sub2PerTokenToMillion(pricing.ImageOutputPrice); imageOutput != "" {
		result.Dimensions = map[string]string{"image_output_per_1m": imageOutput}
	}
	return result
}

func sub2PerTokenToMillion(value *json.Number) string {
	raw := canonicalSub2NumberPointer(value)
	if raw == "" {
		return ""
	}
	parsed, err := decimal.NewFromString(raw)
	if err != nil {
		return ""
	}
	return parsed.Mul(decimal.NewFromInt(1_000_000)).String()
}

func canonicalSub2NumberPointer(value *json.Number) string {
	if value == nil {
		return ""
	}
	return canonicalSub2Number(*value)
}

func canonicalSub2Number(value json.Number) string {
	raw := strings.TrimSpace(value.String())
	if raw == "" {
		return ""
	}
	parsed, err := decimal.NewFromString(raw)
	if err != nil {
		return ""
	}
	return parsed.String()
}

func parseSub2CustomerProfile(body []byte, observedAt time.Time) (*domain.BalanceState, error) {
	root, err := decodeSub2DataRoot(body)
	if err != nil {
		return nil, err
	}
	balanceText, balancePath := firstSub2Scalar(root, "balance", "wallet.balance", "account.balance", "user.balance",
		"available_balance", "available", "wallet.available_balance", "account.available_balance")
	balance, err := decimal.NewFromString(strings.TrimSpace(balanceText))
	if err != nil || balance.IsNegative() {
		return nil, errors.New("customer profile contains no balance")
	}
	available := balance
	if availableText, _ := firstSub2Scalar(root, "available_balance", "available", "wallet.available_balance", "account.available_balance"); availableText != "" {
		if parsed, parseErr := decimal.NewFromString(strings.TrimSpace(availableText)); parseErr == nil && !parsed.IsNegative() {
			available = parsed
		}
	} else if frozenText, _ := firstSub2Scalar(root, "frozen_balance", "wallet.frozen_balance", "account.frozen_balance", "user.frozen_balance"); frozenText != "" {
		if frozen, parseErr := decimal.NewFromString(strings.TrimSpace(frozenText)); parseErr == nil && !frozen.IsNegative() {
			available = balance.Sub(frozen)
			if available.IsNegative() {
				available = decimal.Zero
			}
		}
	}
	currency, _ := firstSub2Scalar(root, "currency", "wallet.currency", "account.currency", "user.currency")
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "USD"
	}
	return &domain.BalanceState{Amount: balance.String(), Available: available.String(), Currency: currency,
		Confidence: "authenticated", ObservedAt: observedAt, SourceField: balancePath, RawValue: balance.String(),
		RawUnit: currency, RawDisplayText: "customer balance", NormalizationFormula: "identity"}, nil
}

func decodeSub2DataRoot(body []byte) (any, error) {
	var envelope sub2Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	code := strings.Trim(strings.TrimSpace(string(envelope.Code)), `"`)
	if !sub2EnvelopeCodeOK(envelope.Code) {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = "request failed"
		}
		return nil, fmt.Errorf("code %s: %s", code, message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, errors.New("response has no data")
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	return root, nil
}

func firstSub2Scalar(root any, paths ...string) (string, string) {
	for _, path := range paths {
		values, err := evaluateJSONPath(root, path)
		if err != nil {
			continue
		}
		for _, value := range values {
			if scalar, ok := scalarValue(value); ok && strings.TrimSpace(scalar) != "" {
				return strings.TrimSpace(scalar), path
			}
		}
	}
	return "", ""
}

func validSub2Identity(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if numeric, err := decimal.NewFromString(value); err == nil {
		return numeric.IsPositive()
	}
	return len(value) >= 2
}

func parseSub2CustomerUsage(body []byte) (sub2CustomerUsage, error) {
	var usage sub2CustomerUsage
	if err := decodeSub2Envelope(body, &usage); err != nil {
		return sub2CustomerUsage{}, err
	}
	if canonicalSub2Number(usage.TodayActualCost) != "" {
		usage.HasLifetime = canonicalSub2Number(usage.TotalActualCost) != ""
		return usage, nil
	}
	// /usage/stats?period=today reports the selected period through total_*
	// fields. Treat it as daily evidence, not as lifetime spend.
	if canonicalSub2Number(usage.TotalActualCost) != "" {
		usage.TodayActualCost = usage.TotalActualCost
		usage.TodayRequests = usage.TotalRequests
		usage.TotalActualCost = ""
		usage.TotalRequests = 0
		return usage, nil
	}
	return sub2CustomerUsage{}, errors.New("customer usage contains no actual-cost field")
}

func (c *Sub2API) collectUpstream(ctx context.Context, base string, supplier domain.Supplier, credential domain.CredentialPayload) (domain.CatalogSnapshot, error) {
	warnings := make([]string, 0)
	modelsPath := supplier.PricingPath
	if modelsPath == "" {
		modelsPath = metadataPath(supplier, "sub2api_models_path", "/v1/models")
	}
	modelsPath = normalizedPath(modelsPath)
	modelsBody, err := c.sub2Request(ctx, base, modelsPath, http.MethodGet, nil, credential)
	if err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("fetch Sub2API models: %w", err)
	}
	models, modelWarnings := parseSub2Models(modelsBody)
	warnings = append(warnings, modelWarnings...)
	if len(models) == 0 {
		return domain.CatalogSnapshot{}, fmt.Errorf("Sub2API response did not expose a model catalog")
	}

	usagePath := supplier.BalancePath
	if usagePath == "" {
		usagePath = metadataPath(supplier, "sub2api_usage_path", "/v1/usage")
	}
	usagePath = normalizedPath(usagePath)
	var balance *domain.BalanceState
	groupName := metadataValue(supplier, "sub2api_group_name", "API Key 分组")
	usageBody, usageErr := c.sub2Request(ctx, base, usagePath, http.MethodGet, nil, credential)
	if usageErr != nil {
		warnings = append(warnings, "Sub2API Key 用量/余额暂不可用："+usageErr.Error())
	} else {
		parsedBalance, planName, usageWarnings := parseSub2UsageBalance(usageBody, time.Now().UTC())
		balance = parsedBalance
		warnings = append(warnings, usageWarnings...)
		if planName != "" && planName != "钱包余额" {
			groupName = planName
		}
	}

	billingPath := metadataPath(supplier, "sub2api_billing_path", "/v1/sub2api/billing")
	billingBody, billingErr := c.sub2Request(ctx, base, billingPath, http.MethodGet, nil, credential)
	var billing sub2KeyBilling
	if billingErr != nil {
		if httpErr, ok := billingErr.(*sub2HTTPError); ok && httpErr.StatusCode == http.StatusNotFound {
			warnings = append(warnings, "当前 Sub2API 运行模式未开放倍率接口；模型与余额仍已正常采集")
		} else {
			warnings = append(warnings, "Sub2API 实时倍率暂不可用："+billingErr.Error())
		}
	} else {
		if err := json.Unmarshal(billingBody, &billing); err != nil {
			warnings = append(warnings, "Sub2API 实时倍率无法解析："+err.Error())
		} else if billing.Object != "sub2api.key_billing" || billing.SchemaVersion != 1 || billing.BillingScope != "token" {
			warnings = append(warnings, "Sub2API 实时倍率返回了未知契约，已忽略倍率以避免误定价")
			billing = sub2KeyBilling{}
		}
	}

	ratio, billingMetadata := sub2BillingRatio(billing)
	modelIDs := make([]string, 0, len(models))
	for i := range models {
		modelIDs = append(modelIDs, models[i].ExternalID)
		models[i].Group = groupName
		if models[i].Metadata == nil {
			models[i].Metadata = make(map[string]string)
		}
		for key, value := range billingMetadata {
			models[i].Metadata[key] = value
		}
		models[i].PriceFunction = domain.PriceFunction{
			Currency: "USD", Basis: "sub2api_key_multiplier", GroupMultiplier: ratio,
			RequiresModelBasePrice: true,
			Derivation:             "the effective Sub2API key multiplier is known; the model base price must be supplied by the upstream catalog or billing expression",
		}
	}
	groupID := metadataValue(supplier, "sub2api_group_external_id", "api-key-group")
	groups := []domain.GroupOffer{{
		ExternalID: groupID,
		Name:       groupName,
		Ratio:      ratio,
		ModelIDs:   uniqueSorted(modelIDs),
		Metadata:   billingMetadata,
	}}

	confidence := 92
	if ratio == "" {
		confidence = 78
	}
	now := time.Now().UTC()
	sourceType := "authenticated_api_key"
	if c.authCapability == authCustomerSession {
		sourceType = "authenticated_customer_session"
	}
	return domain.CatalogSnapshot{
		SupplierID: supplier.ID,
		Groups:     groups,
		Models:     models,
		Balance:    balance,
		Evidence: domain.Evidence{
			SourceType:   sourceType,
			SourceURL:    base + modelsPath,
			Adapter:      c.Name(),
			Confidence:   confidence,
			ObservedAt:   now,
			ResponseHash: sub2CatalogHash(groups, models),
			Warnings:     warnings,
		},
	}, nil
}

func (c *Sub2API) collectNegotiatedUpstream(ctx context.Context, base string, supplier domain.Supplier,
	credential domain.CredentialPayload) (domain.CatalogSnapshot, error) {
	warnings := make([]string, 0)
	modelsPath := strings.TrimSpace(supplier.PricingPath)
	if modelsPath == "" {
		modelsPath = metadataValue(supplier, "sub2api_models_path", "")
	}
	modelsSelection, modelsBody, err := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "catalog", "catalog", c.authCapability, modelsPath,
		[]string{"/v1/models", "/api/v1/models"}, credential, 4<<20, validateSub2ModelCatalog)
	recordCapabilityFromRequest(c.diagnostics, c.authCapability, c.credentialID, "catalog", err)
	if err != nil {
		return domain.CatalogSnapshot{}, fmt.Errorf("fetch Sub2API models: %w", err)
	}
	models, modelWarnings := parseSub2Models(modelsBody)
	warnings = append(warnings, modelWarnings...)
	if len(models) == 0 {
		return domain.CatalogSnapshot{}, &DiagnosticError{Class: "contract_changed", Operation: "catalog",
			Cause: errors.New("Sub2API response did not expose a model catalog")}
	}

	usagePath := strings.TrimSpace(supplier.BalancePath)
	if usagePath == "" {
		usagePath = metadataValue(supplier, "sub2api_usage_path", "")
	}
	_, usageBody, usageErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "usage", "usage", c.authCapability, usagePath,
		[]string{"/v1/usage", "/api/v1/usage"}, credential, 4<<20, validateSub2KeyUsage)
	recordCapabilityFromRequest(c.diagnostics, c.authCapability, c.credentialID, "usage", usageErr)
	var balance *domain.BalanceState
	groupName := metadataValue(supplier, "sub2api_group_name", "API Key group")
	if usageErr != nil {
		warnings = append(warnings, "Sub2API Key usage/balance is temporarily unavailable: "+ErrorClass(usageErr))
	} else {
		parsedBalance, planName, usageWarnings := parseSub2UsageBalance(usageBody, time.Now().UTC())
		balance = parsedBalance
		warnings = append(warnings, usageWarnings...)
		if strings.TrimSpace(planName) != "" {
			groupName = planName
		}
	}

	billingPath := metadataValue(supplier, "sub2api_billing_path", "")
	_, billingBody, billingErr := c.fetchNegotiatedDocument(ctx, c.diagnostics, c.client,
		supplier, base, "billing", "pricing", c.authCapability, billingPath,
		[]string{"/v1/sub2api/billing", "/api/v1/sub2api/billing"}, credential, 2<<20, validateSub2KeyBilling)
	recordCapabilityFromRequest(c.diagnostics, c.authCapability, c.credentialID, "pricing", billingErr)
	var billing sub2KeyBilling
	if billingErr != nil {
		warnings = append(warnings, "Sub2API effective multiplier is temporarily unavailable; catalog and balance remain usable: "+ErrorClass(billingErr))
	} else if err := json.Unmarshal(billingBody, &billing); err != nil {
		warnings = append(warnings, "Sub2API effective multiplier could not be decoded")
	}

	ratio, billingMetadata := sub2BillingRatio(billing)
	modelIDs := make([]string, 0, len(models))
	for index := range models {
		modelIDs = append(modelIDs, models[index].ExternalID)
		models[index].Group = groupName
		if models[index].Metadata == nil {
			models[index].Metadata = make(map[string]string)
		}
		for key, value := range billingMetadata {
			models[index].Metadata[key] = value
		}
		models[index].PriceFunction = domain.PriceFunction{
			Currency: "USD", Basis: "sub2api_key_multiplier", GroupMultiplier: ratio,
			RequiresModelBasePrice: true,
			Derivation:             "effective key multiplier is known; model base price must come from a compatible catalog",
		}
	}
	groups := []domain.GroupOffer{{
		ExternalID: metadataValue(supplier, "sub2api_group_external_id", "api-key-group"),
		Name:       groupName, Ratio: ratio, ModelIDs: uniqueSorted(modelIDs), Metadata: billingMetadata,
	}}
	confidence := 92
	if ratio == "" {
		confidence = 78
	}
	sourceType := "authenticated_api_key"
	if c.credentialID == nil {
		sourceType = "public_api"
	}
	return domain.CatalogSnapshot{
		SupplierID: supplier.ID, Groups: groups, Models: models, Balance: balance,
		Evidence: domain.Evidence{SourceType: sourceType, SourceURL: base + modelsSelection.Path,
			Adapter: c.Name(), Confidence: confidence, ObservedAt: time.Now().UTC(),
			ResponseHash: sub2CatalogHash(groups, models), Warnings: warnings},
	}, nil
}

func validateSub2ModelCatalog(body []byte) error {
	models, _ := parseSub2Models(body)
	if len(models) == 0 {
		return errors.New("model catalog is empty or unrecognized")
	}
	return nil
}

func validateSub2KeyUsage(body []byte) error {
	balance, _, _ := parseSub2UsageBalance(body, time.Now().UTC())
	if balance == nil {
		return errors.New("usage response contains no recognized balance")
	}
	return nil
}

func validateSub2KeyBilling(body []byte) error {
	var billing sub2KeyBilling
	if err := json.Unmarshal(body, &billing); err != nil {
		return err
	}
	if billing.Object != "sub2api.key_billing" || billing.SchemaVersion != 1 || billing.BillingScope != "token" {
		return errors.New("billing response contract is not recognized")
	}
	return nil
}

func (c *Sub2API) sub2Request(ctx context.Context, base, path, method string, body any, credential domain.CredentialPayload) ([]byte, error) {
	path = normalizedPath(path)
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
	}
	response, err := c.diagnostics.request(ctx, c.client, diagnosticRequest{
		EndpointType: sub2EndpointType(path), AuthCapability: c.authCapability, Method: method, URL: base + path,
		Body: encoded, Credential: credential, MaxBytes: maxResponseBytes, RetryGET: method == http.MethodGet,
		SemanticRead: method == http.MethodPost && path == "/api/v1/admin/accounts/today-stats/batch",
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &sub2HTTPError{Path: path, StatusCode: response.StatusCode, Status: response.Status}
	}
	return response.Body, nil
}

func sub2EndpointType(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "/keys"):
		return "customer_keys"
	case strings.Contains(lower, "model"):
		return "catalog"
	case strings.Contains(lower, "usage") || strings.Contains(lower, "dashboard") || strings.Contains(lower, "stats"):
		return "balance"
	case strings.Contains(lower, "billing") || strings.Contains(lower, "rate"):
		return "pricing"
	case strings.Contains(lower, "group"):
		return "admin_groups"
	case strings.Contains(lower, "account"):
		return "admin_accounts"
	default:
		return "sub2_api"
	}
}

func decodeSub2Envelope(body []byte, target any) error {
	var envelope sub2Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	code := strings.Trim(strings.TrimSpace(string(envelope.Code)), `"`)
	if !sub2EnvelopeCodeOK(envelope.Code) {
		if envelope.Message == "" {
			envelope.Message = "request failed"
		}
		return fmt.Errorf("code %s: %s", code, envelope.Message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("response has no data")
	}
	return json.Unmarshal(envelope.Data, target)
}

func sub2EnvelopeCodeOK(raw json.RawMessage) bool {
	code := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return code == "" || code == "0"
}

func parseSub2Models(body []byte) ([]domain.ModelOffer, []string) {
	warnings := make([]string, 0)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, []string{"Sub2API 返回内容不是 JSON"}
	}
	var names []string
	if err := json.Unmarshal(envelope.Data, &names); err != nil {
		var objects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if objectErr := json.Unmarshal(envelope.Data, &objects); objectErr != nil {
			return nil, []string{"Sub2API 模型目录结构需要站点专属适配规则"}
		}
		for _, object := range objects {
			name := object.ID
			if name == "" {
				name = object.Name
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	models := make([]domain.ModelOffer, 0, len(names))
	for _, name := range uniqueSorted(names) {
		models = append(models, domain.ModelOffer{ExternalID: name, Name: name, BillingMode: "unknown"})
	}
	return models, warnings
}

func parseSub2UsageBalance(body []byte, observedAt time.Time) (*domain.BalanceState, string, []string) {
	var usage sub2KeyUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, "", []string{"Sub2API Key 用量响应不是有效 JSON"}
	}
	warnings := make([]string, 0)
	if usage.IsValid != nil && !*usage.IsValid {
		warnings = append(warnings, "Sub2API Key 当前状态不可用："+usage.Status)
	}
	currency := usage.Unit
	if currency == "" {
		currency = "USD"
	}
	balance := &domain.BalanceState{Currency: currency, Confidence: "high", ObservedAt: observedAt}
	if usage.Usage != nil {
		balance.DailySpend = rawJSONNumber(usage.Usage.Today.ActualCost)
	}
	if usage.ExpiresAt != nil {
		balance.ExpiresAt = usage.ExpiresAt
	}

	switch {
	case usage.Quota != nil:
		balance.Amount = rawJSONNumber(usage.Quota.Limit)
		balance.Available = rawJSONNumber(usage.Quota.Remaining)
		balance.SourceField = "quota.remaining"
		balance.RawDisplayText = "API Key 配额"
	case rawJSONNumber(usage.Balance) != "":
		balance.Amount = rawJSONNumber(usage.Balance)
		balance.Available = firstNonEmpty(rawJSONNumber(usage.Remaining), balance.Amount)
		balance.SourceField = "balance"
		balance.RawDisplayText = "钱包余额"
	case usage.Subscription != nil:
		remaining := rawJSONNumber(usage.Remaining)
		balance.Amount = remaining
		balance.Available = remaining
		balance.SourceField = "subscription.remaining"
		balance.RawDisplayText = "订阅窗口剩余额度"
		if balance.ExpiresAt == nil {
			balance.ExpiresAt = usage.Subscription.ExpiresAt
		}
	default:
		return nil, usage.PlanName, append(warnings, "Sub2API Key 用量响应未提供可归一化余额")
	}
	if balance.Amount == "" {
		balance.Amount = balance.Available
	}
	return balance, usage.PlanName, warnings
}

func sub2BillingRatio(billing sub2KeyBilling) (string, map[string]string) {
	metadata := make(map[string]string)
	setBillingFloat := func(key string, value *float64) {
		if value != nil {
			metadata[key] = number(*value)
		}
	}
	setBillingFloat("group_rate_multiplier", billing.GroupRateMultiplier)
	setBillingFloat("user_rate_multiplier", billing.UserRateMultiplier)
	setBillingFloat("resolved_rate_multiplier", billing.ResolvedRateMultiplier)
	setBillingFloat("peak_rate_multiplier", billing.PeakRateMultiplier)
	setBillingFloat("applied_peak_multiplier", billing.AppliedPeakMultiplier)
	setBillingFloat("effective_rate_multiplier", billing.EffectiveRateMultiplier)
	if billing.BillingScope != "" {
		metadata["billing_scope"] = billing.BillingScope
	}
	metadata["peak_rate_enabled"] = strconv.FormatBool(billing.PeakRateEnabled)
	if billing.PeakStart != nil {
		metadata["peak_start"] = *billing.PeakStart
	}
	if billing.PeakEnd != nil {
		metadata["peak_end"] = *billing.PeakEnd
	}
	if billing.Timezone != nil {
		metadata["timezone"] = *billing.Timezone
	}
	if billing.ObservedAt != nil {
		metadata["billing_observed_at"] = billing.ObservedAt.UTC().Format(time.RFC3339)
	}
	for _, value := range []*float64{billing.EffectiveRateMultiplier, billing.ResolvedRateMultiplier, billing.GroupRateMultiplier} {
		if value != nil {
			return number(*value), metadata
		}
	}
	return "", metadata
}

func accountGroupIDs(account sub2AdminAccount) []int64 {
	ids := append([]int64(nil), account.GroupIDs...)
	for _, group := range account.Groups {
		ids = append(ids, group.ID)
	}
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func credentialPresent(status map[string]bool) bool {
	for key, present := range status {
		if strings.HasPrefix(key, "has_") && present {
			return true
		}
	}
	return false
}

func setOptionalFloat(metadata map[string]string, key string, value *float64) {
	if value != nil {
		metadata[key] = number(*value)
	}
}

func metadataPath(supplier domain.Supplier, key, fallback string) string {
	return normalizedPath(metadataValue(supplier, key, fallback))
}

func metadataValue(supplier domain.Supplier, key, fallback string) string {
	if value := strings.TrimSpace(supplier.Metadata[key]); value != "" {
		return value
	}
	return fallback
}

func normalizedPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func sub2GroupPriceFunction(group sub2AdminGroup) domain.PriceFunction {
	dimensions := make(map[string]string)
	set := func(name string, value *float64) {
		if value != nil {
			dimensions[name] = number(*value)
		}
	}
	set("image_1k", group.ImagePrice1K)
	set("image_2k", group.ImagePrice2K)
	set("image_4k", group.ImagePrice4K)
	set("video_480p", group.VideoPrice480P)
	set("video_720p", group.VideoPrice720P)
	set("video_1080p", group.VideoPrice1080P)
	set("web_search_per_call", group.WebSearchPricePerCall)
	if len(dimensions) == 0 {
		dimensions = nil
	}
	return domain.PriceFunction{
		Currency: "USD", Basis: "sub2api_group", GroupMultiplier: number(group.RateMultiplier),
		Dimensions: dimensions, RequiresModelBasePrice: true,
		Derivation: "group multiplier and group-specific media prices are known; token model base prices require an upstream model catalog",
	}
}

// sub2CatalogHash deliberately excludes balance, daily spend, concurrency and
// health counters. Those values belong to telemetry history and must not look
// like a catalog price change on every collection.
func sub2CatalogHash(groups []domain.GroupOffer, models []domain.ModelOffer) string {
	type canonicalGroup struct {
		ExternalID string            `json:"external_id"`
		Name       string            `json:"name"`
		Ratio      string            `json:"ratio"`
		ModelIDs   []string          `json:"model_ids"`
		Pricing    map[string]string `json:"pricing,omitempty"`
	}
	type canonicalModel struct {
		ExternalID    string               `json:"external_id"`
		Name          string               `json:"name"`
		Group         string               `json:"group"`
		BillingMode   string               `json:"billing_mode"`
		ModelRatio    string               `json:"model_ratio"`
		Completion    string               `json:"completion_ratio"`
		Cache         string               `json:"cache_ratio"`
		ModelPrice    string               `json:"model_price"`
		BillingExpr   string               `json:"billing_expr"`
		PriceFunction domain.PriceFunction `json:"price_function"`
	}
	stableGroupKeys := []string{
		"platform", "status", "subscription_type", "peak_rate_enabled", "peak_start", "peak_end",
		"peak_rate_multiplier", "image_rate_independent", "image_rate_multiplier", "video_rate_independent",
		"video_rate_multiplier", "allow_image_generation", "allow_batch_image", "batch_image_discount",
		"image_price_1k", "image_price_2k", "image_price_4k", "video_price_480p", "video_price_720p",
		"video_price_1080p", "web_search_price_per_call", "daily_limit_usd", "weekly_limit_usd",
		"monthly_limit_usd", "billing_scope", "group_rate_multiplier", "user_rate_multiplier",
		"resolved_rate_multiplier", "effective_rate_multiplier", "applied_peak_multiplier", "timezone",
	}
	canonicalGroups := make([]canonicalGroup, 0, len(groups))
	for _, group := range groups {
		pricing := make(map[string]string)
		for _, key := range stableGroupKeys {
			if value, ok := group.Metadata[key]; ok {
				pricing[key] = value
			}
		}
		if len(pricing) == 0 {
			pricing = nil
		}
		canonicalGroups = append(canonicalGroups, canonicalGroup{
			ExternalID: group.ExternalID, Name: group.Name, Ratio: group.Ratio,
			ModelIDs: uniqueSorted(group.ModelIDs), Pricing: pricing,
		})
	}
	canonicalModels := make([]canonicalModel, 0, len(models))
	for _, model := range models {
		canonicalModels = append(canonicalModels, canonicalModel{
			ExternalID: model.ExternalID, Name: model.Name, Group: model.Group, BillingMode: model.BillingMode,
			ModelRatio: model.ModelRatio, Completion: model.CompletionRatio, Cache: model.CacheRatio,
			ModelPrice: model.ModelPrice, BillingExpr: model.BillingExpr, PriceFunction: model.PriceFunction,
		})
	}
	sort.Slice(canonicalGroups, func(i, j int) bool {
		if canonicalGroups[i].Name == canonicalGroups[j].Name {
			return canonicalGroups[i].ExternalID < canonicalGroups[j].ExternalID
		}
		return canonicalGroups[i].Name < canonicalGroups[j].Name
	})
	sort.Slice(canonicalModels, func(i, j int) bool {
		if canonicalModels[i].Group == canonicalModels[j].Group {
			if canonicalModels[i].Name == canonicalModels[j].Name {
				return canonicalModels[i].ExternalID < canonicalModels[j].ExternalID
			}
			return canonicalModels[i].Name < canonicalModels[j].Name
		}
		return canonicalModels[i].Group < canonicalModels[j].Group
	})
	payload, _ := json.Marshal(struct {
		Groups []canonicalGroup `json:"groups"`
		Models []canonicalModel `json:"models"`
	}{Groups: canonicalGroups, Models: canonicalModels})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func rawJSONNumber(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value json.Number
	if err := json.Unmarshal(raw, &value); err == nil {
		return value.String()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return text
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
