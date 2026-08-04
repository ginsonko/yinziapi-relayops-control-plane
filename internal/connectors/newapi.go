package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const maxResponseBytes = 12 << 20

type NewAPI struct {
	client      *http.Client
	credentials *credentials.Service
	profiles    *capabilityProfileCache
}

func NewNewAPI(client *http.Client, credentialService *credentials.Service) *NewAPI {
	return &NewAPI{client: client, credentials: credentialService, profiles: newCapabilityProfileCache()}
}

func (c *NewAPI) Name() string { return "newapi" }

type newAPIPricingResponse struct {
	Success                   bool                       `json:"success"`
	Message                   string                     `json:"message"`
	Data                      []newAPIPrice              `json:"data"`
	GroupRatio                map[string]json.RawMessage `json:"group_ratio"`
	UsableGroup               map[string]string          `json:"usable_group"`
	SupportedEndpoint         map[string]any             `json:"supported_endpoint"`
	PricingVersion            string                     `json:"pricing_version"`
	GroupModelPricingRevision string                     `json:"group_model_pricing_revision"`
}

type newAPIPrice struct {
	ModelName            string                        `json:"model_name"`
	Description          string                        `json:"description"`
	QuotaType            int                           `json:"quota_type"`
	ModelRatio           *json.Number                  `json:"model_ratio"`
	ModelPrice           *json.Number                  `json:"model_price"`
	CompletionRatio      *json.Number                  `json:"completion_ratio"`
	CacheRatio           *json.Number                  `json:"cache_ratio"`
	CreateCacheRatio     *json.Number                  `json:"create_cache_ratio"`
	ImageRatio           *json.Number                  `json:"image_ratio"`
	AudioRatio           *json.Number                  `json:"audio_ratio"`
	AudioCompletionRatio *json.Number                  `json:"audio_completion_ratio"`
	EnableGroups         []string                      `json:"enable_groups"`
	BillingMode          string                        `json:"billing_mode"`
	BillingExpr          string                        `json:"billing_expr"`
	GroupPricing         map[string]newAPIGroupPricing `json:"group_pricing"`
}

type newAPILegacyTaskProfile struct {
	Unit                 string
	FixedDurationSeconds int
	Evidence             string
	Ambiguity            string
}

var (
	newAPIFixedDurationPrefix = regexp.MustCompile(`(?i)(?:固定(?:时长|時長)|fixed[\s_-]*(?:duration|length))\s*[:：]?\s*(\d{1,5})\s*(?:s|秒|seconds?)`)
	newAPIFixedDurationSuffix = regexp.MustCompile(`(?i)(\d{1,5})\s*(?:s|秒|seconds?)\s*(?:固定(?:时长|時長)|fixed[\s_-]*(?:duration|length))`)
)

type newAPIGroupPricing struct {
	Group                string       `json:"group"`
	Scope                string       `json:"scope"`
	ContractRevision     string       `json:"contract_revision"`
	BillingMode          string       `json:"billing_mode"`
	BillingUnit          string       `json:"billing_unit"`
	QuotaType            int          `json:"quota_type"`
	ModelPrice           *json.Number `json:"model_price"`
	ModelRatio           *json.Number `json:"model_ratio"`
	CompletionRatio      *json.Number `json:"completion_ratio"`
	CacheRatio           *json.Number `json:"cache_ratio"`
	CacheCreationRatio   *json.Number `json:"cache_creation_ratio"`
	CacheCreation5mRatio *json.Number `json:"cache_creation_5m_ratio"`
	CacheCreation1hRatio *json.Number `json:"cache_creation_1h_ratio"`
	ImageRatio           *json.Number `json:"image_ratio"`
	AudioRatio           *json.Number `json:"audio_ratio"`
	AudioCompletionRatio *json.Number `json:"audio_completion_ratio"`
	BillingExpr          string       `json:"billing_expr"`
	FixedDurationSeconds int          `json:"fixed_duration_seconds"`
	BaseGroupRatio       *json.Number `json:"base_group_ratio"`
	EffectiveGroupRatio  *json.Number `json:"effective_group_ratio"`
}

func (c *NewAPI) Collect(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error) {
	snapshot, _, err := c.CollectDetailed(ctx, supplier)
	return snapshot, err
}

func (c *NewAPI) CollectDetailed(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error) {
	session := newDiagnosticSession(supplier.ID)
	snapshot, err := c.collectDetailed(ctx, supplier, session)
	session.diagnostics.CatalogSucceeded = err == nil && len(snapshot.Models) > 0
	session.diagnostics.BalanceSucceeded = snapshot.Balance != nil && snapshot.Balance.Amount != "" && snapshot.Balance.Currency != ""
	if len(snapshot.Evidence.Warnings) > 0 {
		session.diagnostics.Warnings = append(session.diagnostics.Warnings, snapshot.Evidence.Warnings...)
	}
	return snapshot, session.result(), err
}

type newAPICatalog struct {
	Groups                 []domain.GroupOffer
	Models                 []domain.ModelOffer
	Hash                   string
	SourceRevision         string
	RawItemCount           int
	ParsedItemCount        int
	UniqueItemCount        int
	DroppedItemCount       int
	UnitErrorCount         int
	AmbiguousIdentityCount int
}

type newAPIUnitContract struct {
	QuotaPerUnit string
	Currency     string
	ObservedAt   time.Time
}

func (c *NewAPI) collectDetailed(ctx context.Context, supplier domain.Supplier, session *diagnosticSession) (domain.CatalogSnapshot, error) {
	base, err := normalizedBaseURL(supplier.BaseURL)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	client, err := netguard.SameOriginClient(c.client, base)
	if err != nil {
		return domain.CatalogSnapshot{}, err
	}
	var payload domain.CredentialPayload
	var credential domain.Credential
	if supplier.CredentialID != nil {
		if c.credentials == nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("NewAPI credential service is required")
		}
		loadedCredential, loaded, loadErr := c.credentials.Load(ctx, *supplier.CredentialID)
		if loadErr != nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("load supplier credential: %w", loadErr)
		}
		if originErr := netguard.RequireSameOrigin(loadedCredential.SiteURL, base); originErr != nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("supplier credential origin mismatch: %w", originErr)
		}
		credential = loadedCredential
		payload = loaded
	}
	requestCtx, cancel := requestDeadline(ctx, 25*time.Second)
	defer cancel()
	pricingPath := strings.TrimSpace(supplier.PricingPath)
	anonymousSelection, anonymousCatalog, anonymousResponse, anonymousErr := c.negotiatePricingCatalog(
		requestCtx, session, client, supplier, base, authAnonymous, pricingPath,
		[]string{"/api/pricing"}, domain.CredentialPayload{})
	recordCapabilityFromRequest(session, authAnonymous, nil, "catalog", anonymousErr)

	authCapability := credentialCapability(credential.Kind, payload, false)
	var authenticatedCatalog newAPICatalog
	var authenticatedResponse diagnosticResponse
	var authenticatedSelection capabilitySelection
	var authenticatedErr error
	if supplier.CredentialID != nil {
		authenticatedSelection, authenticatedCatalog, authenticatedResponse, authenticatedErr = c.negotiatePricingCatalog(
			requestCtx, session, client, supplier, base, authCapability, pricingPath,
			[]string{"/api/pricing"}, payload)
		recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "catalog", authenticatedErr)
	}

	warnings := make([]string, 0)
	var balance *domain.BalanceState
	if supplier.CredentialID != nil {
		unitContract, unitWarnings := c.fetchNegotiatedUnitContract(ctx, supplier, base, client, session)
		warnings = append(warnings, unitWarnings...)
		var balanceWarnings []string
		balance, balanceWarnings = c.fetchNegotiatedBalance(ctx, supplier, base, client, payload, authCapability, unitContract, session)
		warnings = append(warnings, balanceWarnings...)
	}
	chosen := anonymousCatalog
	chosenSelection := anonymousSelection
	sourceType := "public_api"
	confidence := 80
	if anonymousErr != nil {
		if authenticatedErr != nil || supplier.CredentialID == nil {
			failure := newAPICatalogFailure(anonymousResponse, anonymousErr, false, 0)
			if supplier.CredentialID != nil {
				failure = newAPICatalogFailure(authenticatedResponse, authenticatedErr, true, payload.UserID)
			}
			return domain.CatalogSnapshot{SupplierID: supplier.ID, Balance: balance, Evidence: domain.Evidence{
				Adapter: c.Name(), ObservedAt: time.Now().UTC(), Warnings: warnings,
			}}, failure
		}
		chosen = authenticatedCatalog
		chosenSelection = authenticatedSelection
		sourceType = "authenticated_api"
		confidence = 90
	} else {
		warnings = append(warnings, "当前目录可由公开价格接口验证")
		if supplier.CredentialID != nil {
			switch {
			case authenticatedErr != nil:
				warnings = append(warnings, "公开目录可用，但当前登录凭据未通过目录验证；余额等私有字段可能需要重新授权")
			case authenticatedCatalog.Hash != anonymousCatalog.Hash:
				chosen = authenticatedCatalog
				chosenSelection = authenticatedSelection
				sourceType = "authenticated_api"
				confidence = 90
				warnings = append(warnings, "登录后的目录与公开目录不同，已使用账号专属目录")
			default:
				warnings = append(warnings, "登录目录与公开目录一致，目录证据保持 public_api")
			}
		}
	}

	now := time.Now().UTC()
	session.diagnostics.CatalogProof = &domain.CatalogCollectionProofBasis{
		AuthoritativeEndpoint:  "newapi:" + chosenSelection.Path,
		AuthenticationRequired: sourceType == "authenticated_api", Authenticated: true,
		SourceRevision: chosen.SourceRevision,
		Pages:          []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: chosen.RawItemCount, Terminal: true}},
		RawItemCount:   chosen.RawItemCount, ParsedItemCount: chosen.ParsedItemCount,
		UniqueItemCount: chosen.UniqueItemCount, DroppedItemCount: chosen.DroppedItemCount,
		UnitErrorCount: chosen.UnitErrorCount, AmbiguousIdentityCount: chosen.AmbiguousIdentityCount,
		ObservedAt: now,
	}
	return domain.CatalogSnapshot{
		SupplierID: supplier.ID,
		Groups:     chosen.Groups,
		Models:     chosen.Models,
		Balance:    balance,
		Evidence: domain.Evidence{
			SourceType:   sourceType,
			SourceURL:    base + chosenSelection.Path,
			Adapter:      c.Name(),
			Confidence:   confidence,
			ObservedAt:   now,
			ResponseHash: chosen.Hash,
			Warnings:     warnings,
		},
	}, nil
}

func (c *NewAPI) fetchPricingCatalog(ctx context.Context, client *http.Client, requestURL string, payload domain.CredentialPayload,
	authCapability string, session *diagnosticSession, retryGET bool) (newAPICatalog, diagnosticResponse, error) {
	response, err := session.request(ctx, client, diagnosticRequest{
		EndpointType: "catalog", AuthCapability: authCapability, Method: http.MethodGet, URL: requestURL,
		Credential: payload, MaxBytes: maxResponseBytes, RetryGET: retryGET,
	})
	if err != nil {
		return newAPICatalog{}, response, err
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return newAPICatalog{}, response, &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "catalog", StatusCode: response.StatusCode}
	}
	if response.StatusCode != http.StatusOK {
		return newAPICatalog{}, response, &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "catalog", StatusCode: response.StatusCode}
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/html") || bytes.HasPrefix(bytes.TrimSpace(response.Body), []byte("<")) {
		session.markLastContract("catalog", "html", "auth_missing", "pricing endpoint returned a login page")
		return newAPICatalog{}, response, &DiagnosticError{Class: "auth_missing", Operation: "catalog", Cause: errors.New("pricing endpoint returned a login page")}
	}
	var upstream newAPIPricingResponse
	if err := json.Unmarshal(response.Body, &upstream); err != nil {
		session.markLastContract("catalog", "unknown", "contract_changed", "pricing response is not valid JSON")
		return newAPICatalog{}, response, &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: err}
	}
	if !upstream.Success {
		message := strings.ToLower(upstream.Message)
		for _, marker := range []string{"unauthor", "not logged", "login", "sign in", "access token"} {
			if strings.Contains(message, marker) {
				session.markLastContract("catalog", "newapi.pricing.v1", "auth_missing", "pricing contract reports authentication is required")
				return newAPICatalog{}, response, &DiagnosticError{Class: "auth_missing", Operation: "catalog"}
			}
		}
		session.markLastContract("catalog", "newapi.pricing.v1", "contract_changed", "pricing contract returned success=false")
		return newAPICatalog{}, response, &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: errors.New("pricing contract returned success=false")}
	}
	if len(upstream.Data) == 0 {
		session.markLastContract("catalog", "newapi.pricing.v1", "contract_changed", "pricing response contains no visible models")
		return newAPICatalog{}, response, &DiagnosticError{Class: "contract_changed", Operation: "catalog", Cause: errors.New("pricing response contains no visible models")}
	}
	catalog := parseNewAPICatalog(upstream)
	session.markLastContract("catalog", "newapi.pricing.v1", "", "")
	return catalog, response, nil
}

func parseNewAPICatalog(upstream newAPIPricingResponse) newAPICatalog {
	groupModels := make(map[string][]string)
	structuredGroupRatios := make(map[string]string)
	models := make([]domain.ModelOffer, 0, len(upstream.Data))
	droppedItems := 0
	for _, item := range upstream.Data {
		if strings.TrimSpace(item.ModelName) == "" {
			droppedItems++
			continue
		}
		if len(item.GroupPricing) > 0 {
			groups := make([]string, 0, len(item.GroupPricing))
			for group := range item.GroupPricing {
				groups = append(groups, group)
			}
			sort.Strings(groups)
			for _, mapGroup := range groups {
				pricing := item.GroupPricing[mapGroup]
				group := strings.TrimSpace(pricing.Group)
				if group == "" {
					group = strings.TrimSpace(mapGroup)
				}
				offer := newAPIGroupPricingOffer(item.ModelName, group, pricing, upstream.GroupModelPricingRevision)
				if group == "" {
					markNewAPIPricingAmbiguity(&offer, "group_pricing entry has no group identity")
				} else if strings.TrimSpace(pricing.Group) != "" && strings.TrimSpace(pricing.Group) != strings.TrimSpace(mapGroup) {
					markNewAPIPricingAmbiguity(&offer, "group_pricing map key and group field disagree")
				}
				baseRatio := numberPointer(pricing.BaseGroupRatio)
				if group != "" && baseRatio != "" {
					if previous := structuredGroupRatios[group]; previous != "" && !equalDecimalStrings(previous, baseRatio) {
						markNewAPIPricingAmbiguity(&offer, "group_pricing entries disagree on the base group ratio")
					} else {
						structuredGroupRatios[group] = baseRatio
					}
					if topLevel := rawNumber(upstream.GroupRatio[group]); topLevel != "" && !equalDecimalStrings(topLevel, baseRatio) {
						markNewAPIPricingAmbiguity(&offer, "group_pricing base ratio conflicts with the top-level group ratio")
					}
				}
				if group != "" {
					groupModels[group] = append(groupModels[group], item.ModelName)
				}
				models = append(models, offer)
			}
			continue
		}
		offer := domain.ModelOffer{
			ExternalID:      item.ModelName,
			Name:            item.ModelName,
			BillingMode:     item.BillingMode,
			ModelRatio:      numberPointer(item.ModelRatio),
			CompletionRatio: numberPointer(item.CompletionRatio),
			BillingExpr:     item.BillingExpr,
			Metadata:        map[string]string{},
		}
		if item.QuotaType == 1 {
			profile := classifyNewAPILegacyTaskBilling(item.Description)
			offer.BillingMode = "per_call"
			offer.ModelPrice = numberPointer(item.ModelPrice)
			offer.Metadata["billing_unit"] = profile.Unit
			if profile.FixedDurationSeconds > 0 {
				offer.Metadata["fixed_duration_seconds"] = strconv.Itoa(profile.FixedDurationSeconds)
			}
			if profile.Evidence != "" {
				offer.Metadata["billing_evidence"] = profile.Evidence
				offer.Metadata["billing_evidence_source"] = "upstream_description"
			}
			if profile.Ambiguity != "" {
				offer.Metadata["pricing_ambiguity"] = profile.Ambiguity
			}
		}
		if description := strings.TrimSpace(item.Description); description != "" {
			offer.Metadata["description"] = description
		}
		if offer.BillingMode == "" {
			offer.BillingMode = "ratio"
		}
		if item.CacheRatio != nil {
			offer.CacheRatio = item.CacheRatio.String()
		}
		if item.CreateCacheRatio != nil {
			offer.Metadata["create_cache_ratio"] = item.CreateCacheRatio.String()
		}
		if item.ImageRatio != nil {
			offer.Metadata["image_ratio"] = item.ImageRatio.String()
		}
		if item.AudioRatio != nil {
			offer.Metadata["audio_ratio"] = item.AudioRatio.String()
		}
		if item.AudioCompletionRatio != nil {
			offer.Metadata["audio_completion_ratio"] = item.AudioCompletionRatio.String()
		}
		offer.PriceFunction = newAPIPriceFunction(item)
		if ambiguity := strings.TrimSpace(offer.Metadata["pricing_ambiguity"]); ambiguity != "" {
			markNewAPIPricingAmbiguity(&offer, ambiguity)
		}
		if len(item.EnableGroups) == 1 {
			offer.Group = item.EnableGroups[0]
		}
		for _, group := range item.EnableGroups {
			groupModels[group] = append(groupModels[group], item.ModelName)
		}
		models = append(models, offer)
	}
	groups := make([]domain.GroupOffer, 0, len(upstream.GroupRatio))
	for name, raw := range upstream.GroupRatio {
		ratio := rawNumber(raw)
		groups = append(groups, domain.GroupOffer{ExternalID: name, Name: name, Ratio: ratio, ModelIDs: uniqueSorted(groupModels[name])})
	}
	for name, modelIDs := range groupModels {
		if _, exists := upstream.GroupRatio[name]; exists {
			continue
		}
		groups = append(groups, domain.GroupOffer{ExternalID: name, Name: name, Ratio: structuredGroupRatios[name], ModelIDs: uniqueSorted(modelIDs)})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	hash := catalogResponseHash(groups, models)
	revision := strings.TrimSpace(upstream.PricingVersion)
	if revision == "" {
		revision = strings.TrimSpace(upstream.GroupModelPricingRevision)
	}
	if revision == "" {
		revision = hash
	}
	unique := make(map[string]struct{}, len(groups)+len(models))
	ambiguous := 0
	for _, group := range groups {
		unique["group\x00"+strings.TrimSpace(group.ExternalID)] = struct{}{}
	}
	for _, model := range models {
		identity := "model\x00" + strings.TrimSpace(model.Group) + "\x00" + strings.TrimSpace(model.ExternalID)
		unique[identity] = struct{}{}
		if strings.TrimSpace(model.Metadata["pricing_ambiguity"]) != "" {
			ambiguous++
		}
	}
	parsed := len(groups) + len(models)
	return newAPICatalog{Groups: groups, Models: models, Hash: hash, SourceRevision: revision,
		RawItemCount: parsed + droppedItems, ParsedItemCount: parsed, UniqueItemCount: len(unique),
		DroppedItemCount: droppedItems, AmbiguousIdentityCount: ambiguous}
}

func classifyNewAPILegacyTaskBilling(description string) newAPILegacyTaskProfile {
	description = strings.TrimSpace(description)
	profile := newAPILegacyTaskProfile{Unit: "per_request"}
	if description == "" {
		return profile
	}
	lower := strings.ToLower(description)
	perSecond := strings.Contains(description, "按秒计费") || strings.Contains(description, "按秒收费") ||
		strings.Contains(description, "每秒计费") || strings.Contains(description, "每秒收费") ||
		strings.Contains(lower, "per second") || strings.Contains(lower, "/second")
	fixedDuration := explicitNewAPIFixedDuration(description)
	if perSecond && fixedDuration > 0 {
		profile.Ambiguity = "upstream description declares both per-second and fixed-duration billing"
		profile.Evidence = description
		return profile
	}
	if fixedDuration > 0 {
		profile.Unit = "fixed_duration"
		profile.FixedDurationSeconds = fixedDuration
		profile.Evidence = description
		return profile
	}
	if perSecond {
		profile.Unit = "per_second"
		profile.Evidence = description
	}
	return profile
}

func explicitNewAPIFixedDuration(description string) int {
	for _, pattern := range []*regexp.Regexp{newAPIFixedDurationPrefix, newAPIFixedDurationSuffix} {
		match := pattern.FindStringSubmatch(description)
		if len(match) != 2 {
			continue
		}
		seconds, err := strconv.Atoi(match[1])
		if err == nil && seconds > 0 && seconds <= 86400 {
			return seconds
		}
	}
	return 0
}

func newAPIGroupPricingOffer(modelName, group string, pricing newAPIGroupPricing, responseRevision string) domain.ModelOffer {
	unit := strings.ToLower(strings.TrimSpace(pricing.BillingUnit))
	revision := strings.TrimSpace(pricing.ContractRevision)
	if revision == "" {
		revision = strings.TrimSpace(responseRevision)
	}
	metadata := map[string]string{
		"billing_unit":     unit,
		"pricing_scope":    strings.TrimSpace(pricing.Scope),
		"pricing_revision": revision,
	}
	if pricing.FixedDurationSeconds > 0 {
		metadata["fixed_duration_seconds"] = strconv.Itoa(pricing.FixedDurationSeconds)
	}
	if ratio := numberPointer(pricing.BaseGroupRatio); ratio != "" {
		metadata["base_group_ratio"] = ratio
	}
	offer := domain.ModelOffer{
		ExternalID: modelName, Name: modelName, Group: group, BillingMode: strings.TrimSpace(pricing.BillingMode),
		ModelRatio: numberPointer(pricing.ModelRatio), CompletionRatio: numberPointer(pricing.CompletionRatio),
		CacheRatio: numberPointer(pricing.CacheRatio), ModelPrice: numberPointer(pricing.ModelPrice),
		BillingExpr: pricing.BillingExpr, Currency: "USD", Metadata: metadata,
	}
	offer.PriceFunction = newAPIGroupPriceFunction(pricing)
	switch unit {
	case "per_million_tokens":
		if pricing.QuotaType != 0 || pricing.ModelRatio == nil || pricing.CompletionRatio == nil {
			markNewAPIPricingAmbiguity(&offer, "token billing requires model_ratio and completion_ratio")
		}
	case "per_request":
		if pricing.QuotaType != 1 || pricing.ModelPrice == nil {
			markNewAPIPricingAmbiguity(&offer, "per-request billing requires model_price")
		}
	case "per_second":
		if pricing.QuotaType != 1 || pricing.ModelPrice == nil {
			markNewAPIPricingAmbiguity(&offer, "per-second billing requires model_price")
		}
	case "fixed_duration":
		if pricing.QuotaType != 1 || pricing.ModelPrice == nil || pricing.FixedDurationSeconds <= 0 {
			markNewAPIPricingAmbiguity(&offer, "fixed-duration billing requires model_price and a positive fixed duration")
		}
	default:
		markNewAPIPricingAmbiguity(&offer, "group_pricing has an unknown or missing billing unit")
	}
	return offer
}

func newAPIGroupPriceFunction(pricing newAPIGroupPricing) domain.PriceFunction {
	unit := strings.ToLower(strings.TrimSpace(pricing.BillingUnit))
	result := domain.PriceFunction{
		Currency: "USD", Basis: "newapi_group_pricing", Expression: pricing.BillingExpr,
		GroupMultiplier: numberPointer(pricing.BaseGroupRatio),
		Derivation:      "group-scoped NewAPI price before applying base_group_ratio",
	}
	switch unit {
	case "per_request":
		result.PerRequest = numberPointer(pricing.ModelPrice)
		return result
	case "per_second":
		result.PerSecond = numberPointer(pricing.ModelPrice)
		return result
	case "fixed_duration":
		result.FixedFee = numberPointer(pricing.ModelPrice)
		return result
	case "per_million_tokens":
		legacy := newAPIPrice{
			QuotaType: pricing.QuotaType, ModelRatio: pricing.ModelRatio, ModelPrice: pricing.ModelPrice,
			CompletionRatio: pricing.CompletionRatio, CacheRatio: pricing.CacheRatio,
			CreateCacheRatio: pricing.CacheCreationRatio, ImageRatio: pricing.ImageRatio,
			AudioRatio: pricing.AudioRatio, AudioCompletionRatio: pricing.AudioCompletionRatio,
			BillingMode: pricing.BillingMode, BillingExpr: pricing.BillingExpr,
		}
		result = newAPIPriceFunction(legacy)
		result.Basis = "newapi_group_pricing"
		result.GroupMultiplier = numberPointer(pricing.BaseGroupRatio)
		result.Derivation = "group-scoped token vector before applying base_group_ratio"
		modelRatio, err := decimal.NewFromString(numberPointer(pricing.ModelRatio))
		if err == nil {
			input := modelRatio.Mul(decimal.NewFromInt(2))
			variables, expressionScoped := billingExpressionVariables(pricing.BillingMode, pricing.BillingExpr)
			if !expressionScoped || variables["cc"] {
				result.CacheWrite5MPer1M = multiplyPrice(input, pricing.CacheCreation5mRatio)
			}
			if !expressionScoped || variables["cc1h"] {
				result.CacheWrite1HPer1M = multiplyPrice(input, pricing.CacheCreation1hRatio)
			}
		}
		return result
	default:
		result.RequiresModelBasePrice = true
		return result
	}
}

func markNewAPIPricingAmbiguity(offer *domain.ModelOffer, reason string) {
	if offer.Metadata == nil {
		offer.Metadata = map[string]string{}
	}
	if previous := strings.TrimSpace(offer.Metadata["pricing_ambiguity"]); previous != "" && previous != reason {
		reason = previous + "; " + reason
	}
	offer.Metadata["pricing_ambiguity"] = reason
	offer.PriceFunction.RequiresModelBasePrice = true
}

func equalDecimalStrings(left, right string) bool {
	leftValue, leftErr := decimal.NewFromString(strings.TrimSpace(left))
	rightValue, rightErr := decimal.NewFromString(strings.TrimSpace(right))
	return leftErr == nil && rightErr == nil && leftValue.Equal(rightValue)
}

func newAPICatalogFailure(response diagnosticResponse, requestErr error, credentialAttached bool, userID int64) error {
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || ErrorClass(requestErr) == "auth_missing" || ErrorClass(requestErr) == "permission_denied" {
		guidance := newAPICredentialError(credentialAttached)
		if credentialAttached && userID == 0 {
			guidance = fmt.Errorf("NewAPI credential did not prove an authorized catalog; a New-Api-User user id may be missing, or the session/token lacks permission")
		}
		if requestErr != nil && strings.Contains(strings.ToLower(requestErr.Error()), "login page") {
			return fmt.Errorf("NewAPI pricing returned a login page: %w", guidance)
		}
		return fmt.Errorf("fetch pricing: %w", guidance)
	}
	if requestErr != nil {
		return fmt.Errorf("fetch pricing: %w", requestErr)
	}
	return fmt.Errorf("pricing endpoint returned HTTP %d", response.StatusCode)
}

func newAPICredentialError(credentialAttached bool) error {
	if credentialAttached {
		return fmt.Errorf("NewAPI pricing credential was rejected or expired; re-pair the browser session or replace the API access token")
	}
	return fmt.Errorf("NewAPI pricing is login-restricted; attach a browser-paired session or API access token")
}

func newAPIPriceFunction(item newAPIPrice) domain.PriceFunction {
	result := domain.PriceFunction{
		Currency:   "USD",
		Basis:      "newapi_catalog",
		Expression: item.BillingExpr,
	}
	if item.QuotaType == 1 && item.ModelPrice != nil {
		profile := classifyNewAPILegacyTaskBilling(item.Description)
		switch profile.Unit {
		case "per_second":
			result.PerSecond = item.ModelPrice.String()
			result.Derivation = "per_second = model_price from explicit upstream description; final cost also applies requested seconds and the selected group multiplier"
		case "fixed_duration":
			result.FixedFee = item.ModelPrice.String()
			result.Derivation = "fixed_duration = model_price from explicit upstream description; final cost applies once per fixed-duration request and the selected group multiplier"
		default:
			result.PerRequest = item.ModelPrice.String()
			result.Derivation = "per_request = model_price; final cost also applies the selected group multiplier"
		}
		return result
	}
	if item.ModelRatio == nil {
		result.RequiresModelBasePrice = true
		return result
	}
	// NewAPI's ratio contract uses $0.002/1K input tokens as ratio 1,
	// therefore a model ratio converts to USD/1M by multiplying by 2.
	input, err := decimal.NewFromString(item.ModelRatio.String())
	if err != nil {
		result.RequiresModelBasePrice = true
		return result
	}
	input = input.Mul(decimal.NewFromInt(2))
	variables, expressionScoped := billingExpressionVariables(item.BillingMode, item.BillingExpr)
	if !expressionScoped || variables["p"] {
		result.InputPer1M = canonicalDecimal(input)
	}
	if !expressionScoped || variables["c"] {
		result.OutputPer1M = multiplyPrice(input, item.CompletionRatio)
	}
	if !expressionScoped || variables["cr"] {
		result.CacheReadPer1M = multiplyPrice(input, item.CacheRatio)
	}
	if !expressionScoped || variables["cc"] {
		result.CacheWritePer1M = multiplyPrice(input, item.CreateCacheRatio)
	}
	if !expressionScoped || variables["i"] {
		result.ImageInputPer1M = multiplyPrice(input, item.ImageRatio)
	}
	if !expressionScoped || variables["a"] {
		result.AudioInputPer1M = multiplyPrice(input, item.AudioRatio)
	}
	if !expressionScoped || variables["ac"] {
		result.AudioOutputPer1M = multiplyPrice(input, item.AudioCompletionRatio)
	}
	result.Derivation = "input_per_1m = model_ratio * 2 USD; other token dimensions multiply the input price; final cost also applies the selected group multiplier"
	if expressionScoped {
		result.Derivation += "; tiered billing dimensions are limited to variables actually referenced by billing_expr"
	}
	return result
}

// billingExpressionVariables performs a bounded lexical pass over NewAPI's
// tiered expression. It does not evaluate the expression; it only determines
// which documented monetary variables participate, while ignoring quoted tier
// names. An invalid quoted string keeps the conservative legacy behavior.
func billingExpressionVariables(mode, expression string) (map[string]bool, bool) {
	if !strings.EqualFold(strings.TrimSpace(mode), "tiered_expr") || strings.TrimSpace(expression) == "" {
		return nil, false
	}
	variables := make(map[string]bool)
	for index := 0; index < len(expression); {
		character := expression[index]
		if character == '\'' || character == '"' {
			quote := character
			index++
			closed := false
			for index < len(expression) {
				if expression[index] == '\\' {
					index += 2
					continue
				}
				if expression[index] == quote {
					index++
					closed = true
					break
				}
				index++
			}
			if !closed {
				return nil, false
			}
			continue
		}
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' {
			start := index
			index++
			for index < len(expression) {
				character = expression[index]
				if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
					(character >= '0' && character <= '9') || character == '_') {
					break
				}
				index++
			}
			variables[strings.ToLower(expression[start:index])] = true
			continue
		}
		index++
	}
	return variables, true
}

func multiplyPrice(base decimal.Decimal, ratio *json.Number) string {
	if ratio == nil {
		return ""
	}
	parsed, err := decimal.NewFromString(ratio.String())
	if err != nil {
		return ""
	}
	return canonicalDecimal(base.Mul(parsed))
}

func canonicalDecimal(value decimal.Decimal) string {
	return value.String()
}

func numberPointer(value *json.Number) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func (c *NewAPI) fetchUnitContract(ctx context.Context, base string, client *http.Client, session *diagnosticSession) (newAPIUnitContract, []string) {
	requestCtx, cancel := requestDeadline(ctx, 15*time.Second)
	defer cancel()
	response, err := session.request(requestCtx, client, diagnosticRequest{
		EndpointType: "status", AuthCapability: authAnonymous, Method: http.MethodGet, URL: base + "/api/status",
		MaxBytes: 2 << 20, RetryGET: true,
	})
	if err != nil {
		return newAPIUnitContract{}, []string{"公开状态接口暂不可用，quota 余额将保持未知单位：" + ErrorClass(err)}
	}
	if response.StatusCode != http.StatusOK {
		return newAPIUnitContract{}, []string{fmt.Sprintf("公开状态接口返回 HTTP %d，quota 余额将保持未知单位", response.StatusCode)}
	}
	var envelope struct {
		Success bool                       `json:"success"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || !envelope.Success {
		session.markLastContract("status", "unknown", "contract_changed", "status response contract is not recognized")
		return newAPIUnitContract{}, []string{"公开状态接口结构无法识别，quota 余额将保持未知单位"}
	}
	quotaPerUnit, _ := firstNumeric(envelope.Data, "quota_per_unit", "quotaPerUnit")
	currency := firstStatusCurrency(envelope.Data)
	session.markLastContract("status", "newapi.status.v1", "", "")
	warnings := make([]string, 0)
	if quotaPerUnit == "" {
		warnings = append(warnings, "状态接口未提供 quota_per_unit，原始 quota 不会被当作金额")
	}
	if currency == "" {
		warnings = append(warnings, "状态接口未提供可识别币种，余额与利润计算保持未知")
	}
	return newAPIUnitContract{QuotaPerUnit: quotaPerUnit, Currency: currency, ObservedAt: time.Now().UTC()}, warnings
}

func firstStatusCurrency(data map[string]json.RawMessage) string {
	for _, key := range []string{
		"currency", "quota_currency", "billing_currency", "display_currency",
		"quota_display_type", "quotaDisplayType", "custom_currency_symbol", "customCurrencySymbol",
		"currency_symbol", "currencySymbol",
	} {
		if value := normalizeCurrency(stringValue(data[key])); value != "" {
			return value
		}
	}
	return ""
}

func (c *NewAPI) fetchNegotiatedUnitContract(ctx context.Context, supplier domain.Supplier, base string,
	client *http.Client, session *diagnosticSession) (newAPIUnitContract, []string) {
	requestCtx, cancel := requestDeadline(ctx, 15*time.Second)
	defer cancel()
	key := profileKey("newapi", base, "status", authAnonymous)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, "status", authAnonymous,
		metadataValue(supplier, "newapi_status_path", ""), []string{"/api/status", "/api/setting"}, domain.CredentialPayload{})
	var bestErr error
	retryGET := len(selections) == 1
	for _, selection := range selections {
		response, err := session.request(requestCtx, client, diagnosticRequest{
			EndpointType: "status", AuthCapability: authAnonymous, Method: http.MethodGet,
			URL: base + selection.Path, MaxBytes: 2 << 20, RetryGET: retryGET,
		})
		if err == nil && response.StatusCode != http.StatusOK {
			err = &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "status", StatusCode: response.StatusCode}
		}
		var envelope struct {
			Success bool                       `json:"success"`
			Data    map[string]json.RawMessage `json:"data"`
		}
		if err == nil {
			if decodeErr := json.Unmarshal(response.Body, &envelope); decodeErr != nil || !envelope.Success || envelope.Data == nil {
				session.markLastContract("status", "unknown", "contract_changed", "status response contract is not recognized")
				err = &DiagnosticError{Class: "contract_changed", Operation: "status", Cause: decodeErr}
			}
		}
		if err != nil {
			bestErr = preferredCapabilityError(bestErr, err)
			continue
		}
		c.profiles.set(key, selection)
		quotaPerUnit, _ := firstNumeric(envelope.Data, "quota_per_unit", "quotaPerUnit")
		currency := firstStatusCurrency(envelope.Data)
		session.markLastContract("status", "newapi.status.v1", "", "")
		recordCapabilityFromRequest(session, authAnonymous, nil, "status", nil)
		warnings := make([]string, 0, 2)
		if quotaPerUnit == "" {
			warnings = append(warnings, "status endpoint did not provide quota_per_unit; raw quota remains unconverted")
		}
		if currency == "" {
			warnings = append(warnings, "status endpoint did not provide a recognized currency; normalized balance remains unknown")
		}
		return newAPIUnitContract{QuotaPerUnit: quotaPerUnit, Currency: currency, ObservedAt: time.Now().UTC()}, warnings
	}
	recordCapabilityFromRequest(session, authAnonymous, nil, "status", bestErr)
	return newAPIUnitContract{}, []string{"public status capability is unavailable; raw quota remains unconverted: " + ErrorClass(bestErr)}
}

func normalizeCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "$", "US$", "USD":
		return "USD"
	case "¥", "￥", "CNY", "RMB":
		return "CNY"
	case "€", "EUR":
		return "EUR"
	}
	if len(value) == 3 {
		for _, character := range value {
			if character < 'A' || character > 'Z' {
				return ""
			}
		}
		return value
	}
	return ""
}

func (c *NewAPI) fetchBalance(ctx context.Context, supplier domain.Supplier, base string, client *http.Client, payload domain.CredentialPayload, authCapability string, unit newAPIUnitContract, session *diagnosticSession) (*domain.BalanceState, []string) {
	if supplier.CredentialID == nil {
		return nil, nil
	}
	path := supplier.BalancePath
	if path == "" {
		path = "/api/user/self"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	requestCtx, cancel := requestDeadline(ctx, 20*time.Second)
	defer cancel()
	response, err := session.request(requestCtx, client, diagnosticRequest{
		EndpointType: "balance", AuthCapability: authCapability, Method: http.MethodGet, URL: base + path,
		Credential: payload, MaxBytes: 2 << 20, RetryGET: true,
	})
	recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "balance", err)
	if err != nil {
		return nil, []string{"余额接口暂不可用：" + ErrorClass(err)}
	}
	if response.StatusCode != http.StatusOK {
		warning := fmt.Sprintf("余额接口返回 HTTP %d", response.StatusCode)
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && payload.UserID == 0 {
			warning += "；当前凭据缺少 New-Api-User 用户 ID，或会话/Token 没有该能力"
		}
		return nil, []string{warning}
	}
	data, err := objectData(response.Body)
	if err != nil {
		session.markLastContract("balance", "unknown", "contract_changed", "balance response contract is not recognized")
		return nil, []string{"余额接口结构无法识别"}
	}
	amount, field := firstNumeric(data, "balance", "remaining", "available", "quota")
	if amount == "" {
		return nil, []string{"余额接口未提供可识别的余额字段"}
	}
	currency := normalizeCurrency(stringValue(data["currency"]))
	rawUnit := normalizeCurrency(stringValue(data["currency"]))
	if rawUnit == "" && strings.EqualFold(stringValue(data["currency"]), "quota") {
		rawUnit = "quota"
	}
	state := &domain.BalanceState{ObservedAt: time.Now().UTC(), SourceField: field, Confidence: "authenticated", RawValue: amount, RawUnit: rawUnit}
	isQuota := strings.Contains(strings.ToLower(field), "quota") || strings.EqualFold(stringValue(data["currency"]), "quota")
	if isQuota {
		normalized, formula, ok := normalizeQuota(amount, unit.QuotaPerUnit, unit.Currency)
		state.RawDisplayText = formula
		state.NormalizationDivisor = unit.QuotaPerUnit
		state.NormalizationFormula = formula
		state.RawUnit = "quota"
		if !ok {
			state.Confidence = "invalid_unit"
			return state, []string{"余额返回了原始 quota，但同一采样缺少有效 quota_per_unit 或币种；已保留原始证据并保持金额未知"}
		}
		state.Amount = normalized
		state.Available = normalized
		state.Currency = unit.Currency
	} else {
		state.Amount = amount
		state.Available = amount
		state.Currency = currency
		state.RawDisplayText = fmt.Sprintf("raw %s=%s %s", field, amount, currency)
		state.NormalizationFormula = "identity"
		if currency == "" {
			state.Confidence = "unknown_currency"
		}
	}
	if usedQuota, _ := firstNumeric(data, "used_quota", "usedQuota", "historical_usage", "historical_spend"); usedQuota != "" {
		if normalized, _, ok := normalizeQuota(usedQuota, unit.QuotaPerUnit, unit.Currency); ok {
			state.HistoricalSpend = normalized
		}
	}
	if requestCount, _ := firstNumeric(data, "request_count", "requestCount"); requestCount != "" {
		if parsed, parseErr := strconv.ParseInt(requestCount, 10, 64); parseErr == nil && parsed >= 0 {
			state.RequestCount = &parsed
		}
	}
	session.markLastContract("balance", "newapi.user.self.v1", "", "")
	warnings := make([]string, 0)
	if usagePath := supplier.Metadata["usage_path"]; strings.TrimSpace(usagePath) != "" {
		if daily, usageErr := c.fetchDailySpend(ctx, client, base, usagePath, payload, authCapability, unit, session); usageErr == nil {
			state.DailySpend = daily
			state.UsageSource = normalizedEndpointPath(usagePath)
			now := time.Now().UTC()
			state.UsageObservedAt = &now
		} else {
			warnings = append(warnings, "已配置的 24 小时用量接口暂不可用："+ErrorClass(usageErr))
		}
	}
	if state.DailySpend == "" {
		usage, usageErr := c.fetchNewAPIUsage24h(ctx, client, base, payload, authCapability, unit, session)
		if usageErr == nil {
			state.DailySpend = usage.Amount
			state.DailyRequestCount = &usage.RequestCount
			state.UsageSource = "/api/data/self"
			state.UsageObservedAt = &usage.ObservedAt
		} else {
			warnings = append(warnings, "24 小时消耗暂未取得，余额仍保持可用："+ErrorClass(usageErr))
		}
	}
	return state, warnings
}

func (c *NewAPI) fetchNegotiatedBalance(ctx context.Context, supplier domain.Supplier, base string,
	client *http.Client, payload domain.CredentialPayload, authCapability string, unit newAPIUnitContract,
	session *diagnosticSession) (*domain.BalanceState, []string) {
	if supplier.CredentialID == nil {
		return nil, nil
	}
	requestCtx, cancel := requestDeadline(ctx, 20*time.Second)
	defer cancel()
	key := profileKey("newapi", base, "balance", authCapability)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, "balance", authCapability,
		supplier.BalancePath, []string{"/api/user/self", "/api/user/balance", "/api/user/quota"}, payload)
	var bestErr error
	retryGET := len(selections) == 1
	for _, selection := range selections {
		response, err := session.request(requestCtx, client, diagnosticRequest{
			EndpointType: "balance", AuthCapability: authCapability, Method: http.MethodGet,
			URL: base + selection.Path, Credential: capabilityCredential(payload, selection.Auth),
			MaxBytes: 2 << 20, RetryGET: retryGET,
		})
		if err == nil && response.StatusCode != http.StatusOK {
			err = &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "balance", StatusCode: response.StatusCode}
		}
		var state *domain.BalanceState
		var warnings []string
		if err == nil {
			if paths, custom := manifestJSONPaths(supplier, "balance"); custom {
				state, warnings, err = parseManifestBalance(response.Body, paths, unit)
			} else {
				state, warnings, err = parseNewAPIBalance(response.Body, unit)
			}
			if err != nil {
				session.markLastContract("balance", "unknown", "contract_changed", "balance response contract is not recognized")
			}
		}
		if err != nil {
			bestErr = preferredCapabilityError(bestErr, err)
			if terminalCapabilityAuthError(err) {
				break
			}
			continue
		}
		c.profiles.set(key, selection)
		contract := "newapi.balance.v1"
		if _, custom := manifestJSONPaths(supplier, "balance"); custom {
			contract = manifestContractName("balance")
		}
		session.markLastContract("balance", contract, "", "")
		recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "balance", nil)
		usageUnit := unit
		if usageUnit.Currency == "" {
			usageUnit.Currency = state.Currency
		}
		usage, usageErr := c.fetchNegotiatedNewAPIUsage24h(ctx, supplier, client, base, payload, authCapability, usageUnit, session)
		if usageErr == nil {
			state.DailySpend = usage.Amount
			state.DailyRequestCount = &usage.RequestCount
			state.UsageSource = usage.Source
			state.UsageObservedAt = &usage.ObservedAt
		} else {
			warnings = append(warnings, "24-hour usage is unavailable; balance remains usable: "+ErrorClass(usageErr))
		}
		return state, warnings
	}
	recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "balance", bestErr)
	warning := "balance capability is unavailable: " + ErrorClass(bestErr)
	if (ErrorClass(bestErr) == "auth_missing" || ErrorClass(bestErr) == "permission_denied") && payload.UserID == 0 {
		warning += "; the session may require a captured user id or a different token header"
	}
	return nil, []string{warning}
}

func parseNewAPIBalance(body []byte, unit newAPIUnitContract) (*domain.BalanceState, []string, error) {
	data, err := objectData(body)
	if err != nil {
		return nil, nil, &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: err}
	}
	amount, field := firstNumeric(data, "balance", "remaining", "available", "quota")
	if amount == "" {
		return nil, nil, &DiagnosticError{Class: "contract_changed", Operation: "balance", Cause: errors.New("no balance field")}
	}
	currency := normalizeCurrency(stringValue(data["currency"]))
	rawUnit := currency
	if rawUnit == "" && strings.EqualFold(stringValue(data["currency"]), "quota") {
		rawUnit = "quota"
	}
	state := &domain.BalanceState{ObservedAt: time.Now().UTC(), SourceField: field, Confidence: "authenticated",
		RawValue: amount, RawUnit: rawUnit}
	warnings := make([]string, 0, 1)
	isQuota := strings.Contains(strings.ToLower(field), "quota") || strings.EqualFold(stringValue(data["currency"]), "quota")
	if isQuota {
		normalized, formula, ok := normalizeQuota(amount, unit.QuotaPerUnit, unit.Currency)
		state.RawDisplayText = formula
		state.NormalizationDivisor = unit.QuotaPerUnit
		state.NormalizationFormula = formula
		state.RawUnit = "quota"
		if !ok {
			state.Confidence = "invalid_unit"
			warnings = append(warnings, "raw quota was preserved because quota_per_unit or currency is unknown")
		} else {
			state.Amount = normalized
			state.Available = normalized
			state.Currency = unit.Currency
		}
	} else {
		state.Amount = amount
		state.Available = amount
		state.Currency = currency
		state.RawDisplayText = fmt.Sprintf("raw %s=%s %s", field, amount, currency)
		state.NormalizationFormula = "identity"
		if currency == "" {
			state.Confidence = "unknown_currency"
		}
	}
	if usedQuota, _ := firstNumeric(data, "used_quota", "usedQuota", "historical_usage", "historical_spend"); usedQuota != "" {
		if normalized, _, ok := normalizeQuota(usedQuota, unit.QuotaPerUnit, unit.Currency); ok {
			state.HistoricalSpend = normalized
		}
	}
	if requestCount, _ := firstNumeric(data, "request_count", "requestCount"); requestCount != "" {
		if parsed, parseErr := strconv.ParseInt(requestCount, 10, 64); parseErr == nil && parsed >= 0 {
			state.RequestCount = &parsed
		}
	}
	return state, warnings, nil
}

type newAPIUsageSummary struct {
	Amount       string
	RequestCount int64
	ObservedAt   time.Time
	Source       string
}

func (c *NewAPI) fetchNewAPIUsage24h(ctx context.Context, client *http.Client, base string, payload domain.CredentialPayload,
	authCapability string, unit newAPIUnitContract, session *diagnosticSession) (newAPIUsageSummary, error) {
	now := time.Now().UTC()
	endpoint, err := url.Parse(base + "/api/data/self")
	if err != nil {
		return newAPIUsageSummary{}, err
	}
	query := endpoint.Query()
	query.Set("start_timestamp", strconv.FormatInt(now.Add(-24*time.Hour).Unix(), 10))
	query.Set("end_timestamp", strconv.FormatInt(now.Unix(), 10))
	query.Set("time_granularity", "hour")
	query.Set("default_time", "hour")
	endpoint.RawQuery = query.Encode()
	requestCtx, cancel := requestDeadline(ctx, 20*time.Second)
	defer cancel()
	response, err := session.request(requestCtx, client, diagnosticRequest{
		EndpointType: "usage", AuthCapability: authCapability, Method: http.MethodGet, URL: endpoint.String(),
		Credential: payload, MaxBytes: 4 << 20, RetryGET: true,
	})
	if err != nil {
		return newAPIUsageSummary{}, err
	}
	if response.StatusCode != http.StatusOK {
		return newAPIUsageSummary{}, &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "usage", StatusCode: response.StatusCode}
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &envelope); err != nil || envelope.Success == nil || !*envelope.Success {
		session.markLastContract("usage", "unknown", "contract_changed", "usage response contract is not recognized")
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rows); err != nil {
		session.markLastContract("usage", "unknown", "contract_changed", "usage data is not an array")
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
	}
	quota := decimal.Zero
	var count int64
	for _, row := range rows {
		rawQuota, _ := firstNumeric(row, "quota", "used_quota", "usedQuota")
		if rawQuota != "" {
			parsed, parseErr := decimal.NewFromString(rawQuota)
			if parseErr != nil || parsed.IsNegative() {
				return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
			}
			quota = quota.Add(parsed)
		}
		rawCount, _ := firstNumeric(row, "count", "request_count", "requestCount")
		if rawCount != "" {
			parsed, parseErr := strconv.ParseInt(rawCount, 10, 64)
			if parseErr != nil || parsed < 0 {
				return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
			}
			count += parsed
		}
	}
	normalized, _, ok := normalizeQuota(quota.String(), unit.QuotaPerUnit, unit.Currency)
	if !ok {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
	}
	session.markLastContract("usage", "newapi.data.self.v1", "", "")
	return newAPIUsageSummary{Amount: normalized, RequestCount: count, ObservedAt: now}, nil
}

func (c *NewAPI) fetchNegotiatedNewAPIUsage24h(ctx context.Context, supplier domain.Supplier, client *http.Client,
	base string, payload domain.CredentialPayload, authCapability string, unit newAPIUnitContract,
	session *diagnosticSession) (newAPIUsageSummary, error) {
	now := time.Now().UTC()
	requestCtx, cancel := requestDeadline(ctx, 20*time.Second)
	defer cancel()
	explicitPath := metadataValue(supplier, "usage_path", "")
	key := profileKey("newapi", base, "usage", authCapability)
	selections := capabilitySelections(c.profiles, supplier, "newapi", base, "usage", authCapability,
		explicitPath, []string{"/api/data/self", "/api/user/usage", "/api/user/dashboard"}, payload)
	var bestErr error
	retryGET := len(selections) == 1
	for _, selection := range selections {
		endpoint, err := url.Parse(base + selection.Path)
		if err != nil {
			bestErr = preferredCapabilityError(bestErr, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err})
			continue
		}
		query := endpoint.Query()
		query.Set("start_timestamp", strconv.FormatInt(now.Add(-24*time.Hour).Unix(), 10))
		query.Set("end_timestamp", strconv.FormatInt(now.Unix(), 10))
		query.Set("time_granularity", "hour")
		query.Set("default_time", "hour")
		endpoint.RawQuery = query.Encode()
		response, requestErr := session.request(requestCtx, client, diagnosticRequest{
			EndpointType: "usage", AuthCapability: authCapability, Method: http.MethodGet, URL: endpoint.String(),
			Credential: capabilityCredential(payload, selection.Auth), MaxBytes: 4 << 20, RetryGET: retryGET,
		})
		if requestErr == nil && response.StatusCode != http.StatusOK {
			requestErr = &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "usage", StatusCode: response.StatusCode}
		}
		var summary newAPIUsageSummary
		if requestErr == nil {
			if paths, custom := manifestJSONPaths(supplier, "usage"); custom {
				summary, requestErr = parseManifestUsage(response.Body, paths, unit, now)
			} else {
				summary, requestErr = parseNewAPIUsage24h(response.Body, unit, now)
			}
			if requestErr != nil {
				session.markLastContract("usage", "unknown", "contract_changed", "usage response contract is not recognized")
			}
		}
		if requestErr != nil {
			bestErr = preferredCapabilityError(bestErr, requestErr)
			if terminalCapabilityAuthError(requestErr) && !hasVerifiedPrivateCapability(session, authCapability) {
				break
			}
			continue
		}
		summary.Source = selection.Path
		c.profiles.set(key, selection)
		contract := "newapi.usage.v1"
		if _, custom := manifestJSONPaths(supplier, "usage"); custom {
			contract = manifestContractName("usage")
		}
		session.markLastContract("usage", contract, "", "")
		recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "usage", nil)
		return summary, nil
	}
	recordCapabilityFromRequest(session, authCapability, supplier.CredentialID, "usage", bestErr)
	if bestErr == nil {
		bestErr = &DiagnosticError{Class: "contract_changed", Operation: "usage"}
	}
	return newAPIUsageSummary{}, bestErr
}

func hasVerifiedPrivateCapability(session *diagnosticSession, capability string) bool {
	if session == nil {
		return false
	}
	for _, item := range session.diagnostics.Capabilities {
		if item.Capability == capability && item.Status == "verified" && item.EndpointType != "catalog" {
			return true
		}
	}
	return false
}

func parseNewAPIUsage24h(body []byte, unit newAPIUnitContract, observedAt time.Time) (newAPIUsageSummary, error) {
	var envelope struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err}
	}
	if envelope.Success != nil && !*envelope.Success {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "auth_missing", Operation: "usage"}
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &rows); err == nil {
		quota := decimal.Zero
		var count int64
		for _, row := range rows {
			rawQuota, _ := firstNumeric(row, "quota", "used_quota", "usedQuota")
			if rawQuota != "" {
				parsed, parseErr := decimal.NewFromString(rawQuota)
				if parseErr != nil || parsed.IsNegative() {
					return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
				}
				quota = quota.Add(parsed)
			}
			rawCount, _ := firstNumeric(row, "count", "request_count", "requestCount")
			if rawCount != "" {
				parsed, parseErr := strconv.ParseInt(rawCount, 10, 64)
				if parseErr != nil || parsed < 0 {
					return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
				}
				count += parsed
			}
		}
		normalized, _, ok := normalizeQuota(quota.String(), unit.QuotaPerUnit, unit.Currency)
		if !ok {
			return newAPIUsageSummary{}, &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
		}
		return newAPIUsageSummary{Amount: normalized, RequestCount: count, ObservedAt: observedAt}, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: err}
	}
	value, field := firstNumeric(data, "daily_spend", "today_actual_cost", "today_cost", "actual_cost", "used_quota", "quota")
	if value == "" {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage", Cause: errors.New("no usage amount field")}
	}
	amount := value
	if strings.Contains(strings.ToLower(field), "quota") {
		var ok bool
		amount, _, ok = normalizeQuota(value, unit.QuotaPerUnit, unit.Currency)
		if !ok {
			return newAPIUsageSummary{}, &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
		}
	} else if unit.Currency == "" {
		return newAPIUsageSummary{}, &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
	}
	var count int64
	if rawCount, _ := firstNumeric(data, "today_requests", "request_count", "requestCount", "count"); rawCount != "" {
		parsed, err := strconv.ParseInt(rawCount, 10, 64)
		if err != nil || parsed < 0 {
			return newAPIUsageSummary{}, &DiagnosticError{Class: "contract_changed", Operation: "usage"}
		}
		count = parsed
	}
	return newAPIUsageSummary{Amount: amount, RequestCount: count, ObservedAt: observedAt}, nil
}

func normalizedEndpointPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if parsed, err := url.Parse(path); err == nil && parsed.Path != "" {
		return parsed.Path
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func (c *NewAPI) fetchDailySpend(ctx context.Context, client *http.Client, base, path string, payload domain.CredentialPayload, authCapability string, unit newAPIUnitContract, session *diagnosticSession) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	response, err := session.request(ctx, client, diagnosticRequest{
		EndpointType: "usage", AuthCapability: authCapability, Method: http.MethodGet, URL: base + path,
		Credential: payload, MaxBytes: 2 << 20, RetryGET: true,
	})
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", &DiagnosticError{Class: classifyHTTPStatus(response.StatusCode), Operation: "usage", StatusCode: response.StatusCode}
	}
	data, err := objectData(response.Body)
	if err != nil {
		return "", err
	}
	value, _ := firstNumeric(data, "daily_spend", "today_cost", "quota", "used_quota")
	if value == "" {
		return "", fmt.Errorf("usage response has no cost field")
	}
	normalized, _, ok := normalizeQuota(value, unit.QuotaPerUnit, unit.Currency)
	if !ok {
		return "", &DiagnosticError{Class: "invalid_unit", Operation: "usage"}
	}
	return normalized, nil
}

func objectData(body []byte) (map[string]json.RawMessage, error) {
	var root struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(root.Data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func firstNumeric(data map[string]json.RawMessage, keys ...string) (string, string) {
	for _, key := range keys {
		if raw, exists := data[key]; exists {
			value := rawNumber(raw)
			if value != "" {
				return value, key
			}
		}
	}
	return "", ""
}

func stringValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func normalizeQuota(amount, quotaPerUnit, currency string) (string, string, bool) {
	value, valueErr := decimal.NewFromString(amount)
	unit, unitErr := decimal.NewFromString(quotaPerUnit)
	currency = normalizeCurrency(currency)
	formula := fmt.Sprintf("raw quota=%s / quota_per_unit=%s = unknown", amount, quotaPerUnit)
	if valueErr != nil || unitErr != nil || !unit.IsPositive() || currency == "" {
		return "", formula, false
	}
	normalized := value.Div(unit).String()
	formula = fmt.Sprintf("raw quota=%s / quota_per_unit=%s = %s %s", amount, quotaPerUnit, normalized, currency)
	return normalized, formula, true
}

func number(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func rawNumber(raw json.RawMessage) string {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return ""
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
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
