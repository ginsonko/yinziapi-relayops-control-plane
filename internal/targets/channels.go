package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
)

const (
	channelStatusEnabled          = 1
	channelStatusManuallyDisabled = 2
)

var ErrChannelNotFound = errors.New("target channel not found")

type ChannelAdapter interface {
	PricingCatalog(context.Context, domain.TargetSite) (domain.CatalogSnapshot, error)
	ListChannels(context.Context, domain.TargetSite) ([]domain.TargetChannel, error)
	CreateDisabledChannel(context.Context, domain.TargetSite, domain.ListingChannelSpec, string, string, string) (domain.TargetChannel, error)
	FindOwnedChannel(context.Context, domain.TargetSite, string) (domain.TargetChannel, bool, error)
	GetChannel(context.Context, domain.TargetSite, int64) (domain.TargetChannel, error)
	FetchChannelModels(context.Context, domain.TargetSite, int64) ([]string, error)
	TestChannel(context.Context, domain.TargetSite, int64, string) error
	EnableOwnedChannel(context.Context, domain.TargetSite, int64, string) (domain.TargetChannel, error)
	DisableOwnedChannel(context.Context, domain.TargetSite, int64, string) (domain.TargetChannel, error)
	DeleteOwnedDisabledChannel(context.Context, domain.TargetSite, int64, string) error
}

func (a *NewAPI) ListChannels(ctx context.Context, target domain.TargetSite) ([]domain.TargetChannel, error) {
	load := func(loadCtx context.Context) ([]domain.TargetChannel, error) {
		return a.listChannelsUncached(loadCtx, target)
	}
	if a.channels == nil {
		return load(ctx)
	}
	return a.channels.read(ctx, targetCatalogCacheKey(target), load)
}

func (a *NewAPI) listChannelsUncached(ctx context.Context, target domain.TargetSite) ([]domain.TargetChannel, error) {
	const pageSize = 1000
	result := make([]domain.TargetChannel, 0)
	for page := 1; page <= 100; page++ {
		query := url.Values{"p": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}, "id_sort": {"true"}, "tag_mode": {"false"}}
		body, err := a.managementRequest(ctx, target, http.MethodGet, "/api/channel/", query, nil)
		if err != nil {
			return nil, fmt.Errorf("list target channels: %w", err)
		}
		var pageData struct {
			Items []newAPIChannel `json:"items"`
			Total int             `json:"total"`
		}
		if err := decodeManagementData(body, &pageData); err != nil {
			return nil, err
		}
		for _, item := range pageData.Items {
			result = append(result, item.toDomain())
		}
		if len(pageData.Items) == 0 || page*pageSize >= pageData.Total {
			return result, nil
		}
	}
	return nil, errors.New("target channel listing exceeded the safety page limit")
}

func (a *NewAPI) FindOwnedChannel(ctx context.Context, target domain.TargetSite, marker string) (domain.TargetChannel, bool, error) {
	return a.findChannelByMarker(ctx, target, marker)
}

func (a *NewAPI) PricingCatalog(ctx context.Context, target domain.TargetSite) (domain.CatalogSnapshot, error) {
	load := func(loadCtx context.Context) (domain.CatalogSnapshot, error) {
		credentialID := target.CredentialID
		connector := connectors.NewNewAPI(a.client, a.credentials)
		snapshot, err := connector.Collect(loadCtx, domain.Supplier{
			ID: target.ID, BaseURL: target.BaseURL, Adapter: "newapi", PricingPath: "/api/pricing",
			CredentialID: &credentialID, Metadata: map[string]string{},
		})
		if err != nil {
			return domain.CatalogSnapshot{}, fmt.Errorf("read target pricing catalog: %w", err)
		}
		return snapshot, nil
	}
	if a.catalogs == nil {
		return load(ctx)
	}
	return a.catalogs.read(ctx, targetCatalogCacheKey(target), load)
}

func AsChannelAdapter(adapter Adapter) (ChannelAdapter, error) {
	value, ok := adapter.(ChannelAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support channel lifecycle operations", adapter.Name())
	}
	return value, nil
}

type newAPIChannel struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Type         int     `json:"type"`
	Status       int     `json:"status"`
	BaseURL      *string `json:"base_url"`
	Models       string  `json:"models"`
	Group        string  `json:"group"`
	Priority     *int64  `json:"priority"`
	Weight       *uint   `json:"weight"`
	AutoBan      *int    `json:"auto_ban"`
	ModelMapping *string `json:"model_mapping"`
	Tag          *string `json:"tag"`
	Remark       *string `json:"remark"`
}

func (a *NewAPI) CreateDisabledChannel(ctx context.Context, target domain.TargetSite, spec domain.ListingChannelSpec, group, upstreamKey, marker string) (domain.TargetChannel, error) {
	if err := a.remoteWrites.Check("target channel create"); err != nil {
		return domain.TargetChannel{}, err
	}
	if strings.TrimSpace(upstreamKey) == "" {
		return domain.TargetChannel{}, errors.New("upstream API key is empty")
	}
	if strings.TrimSpace(marker) == "" || len(marker) > 255 {
		return domain.TargetChannel{}, errors.New("channel ownership marker is missing or too long")
	}
	if existing, found, err := a.findChannelByMarker(ctx, target, marker); err != nil {
		return domain.TargetChannel{}, err
	} else if found {
		if err := verifyOwnedChannel(existing, spec, group, marker, channelStatusManuallyDisabled); err != nil {
			return domain.TargetChannel{}, fmt.Errorf("ownership marker collision: %w", err)
		}
		return existing, nil
	}

	baseURL := strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/")
	modelMapping := nullableString(spec.ModelMapping)
	tag := nullableString(spec.Tag)
	remark := marker
	priority := spec.Priority
	weight := spec.Weight
	autoBan := 0
	if spec.AutoBan {
		autoBan = 1
	}
	payload := map[string]any{
		"mode": "single",
		"channel": map[string]any{
			"type": spec.Type, "key": upstreamKey, "status": channelStatusManuallyDisabled,
			"name": strings.TrimSpace(spec.Name), "base_url": baseURL, "models": strings.Join(normalizeModels(spec.Models), ","),
			"group": group, "priority": priority, "weight": weight, "auto_ban": autoBan,
			"model_mapping": modelMapping, "tag": tag, "remark": &remark,
		},
	}
	if _, err := a.managementRequest(ctx, target, http.MethodPost, "/api/channel/", nil, payload); err != nil {
		return domain.TargetChannel{}, fmt.Errorf("create disabled target channel: %w", err)
	}
	created, found, err := a.findChannelByMarker(ctx, target, marker)
	if err != nil {
		return domain.TargetChannel{}, fmt.Errorf("read back created target channel: %w", err)
	}
	if !found {
		return domain.TargetChannel{}, errors.New("target accepted channel create but ownership marker was not found on readback")
	}
	if err := verifyOwnedChannel(created, spec, group, marker, channelStatusManuallyDisabled); err != nil {
		return domain.TargetChannel{}, fmt.Errorf("created channel readback mismatch: %w", err)
	}
	return created, nil
}

func (a *NewAPI) GetChannel(ctx context.Context, target domain.TargetSite, id int64) (domain.TargetChannel, error) {
	body, err := a.managementRequest(ctx, target, http.MethodGet, "/api/channel/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	var channel newAPIChannel
	if err := decodeManagementData(body, &channel); err != nil {
		return domain.TargetChannel{}, fmt.Errorf("decode target channel: %w", err)
	}
	if channel.ID == 0 {
		return domain.TargetChannel{}, ErrChannelNotFound
	}
	return channel.toDomain(), nil
}

func (a *NewAPI) FetchChannelModels(ctx context.Context, target domain.TargetSite, id int64) ([]string, error) {
	body, err := a.managementRequest(ctx, target, http.MethodGet, "/api/channel/fetch_models/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch upstream models: %w", err)
	}
	var models []string
	if err := decodeManagementData(body, &models); err != nil {
		return nil, fmt.Errorf("decode upstream models: %w", err)
	}
	models = normalizeModels(models)
	return models, nil
}

func (a *NewAPI) TestChannel(ctx context.Context, target domain.TargetSite, id int64, model string) error {
	if err := a.remoteWrites.Check("billable target channel test"); err != nil {
		return err
	}
	query := url.Values{}
	query.Set("model", strings.TrimSpace(model))
	if _, err := a.managementRequest(ctx, target, http.MethodGet, "/api/channel/test/"+strconv.FormatInt(id, 10), query, nil); err != nil {
		return fmt.Errorf("test target channel model %q: %w", model, err)
	}
	return nil
}

func (a *NewAPI) EnableOwnedChannel(ctx context.Context, target domain.TargetSite, id int64, marker string) (domain.TargetChannel, error) {
	if err := a.remoteWrites.Check("target channel enable"); err != nil {
		return domain.TargetChannel{}, err
	}
	current, err := a.GetChannel(ctx, target, id)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	if current.Remark != marker {
		return domain.TargetChannel{}, errors.New("refusing to enable a channel not owned by this change set")
	}
	if current.Status == channelStatusEnabled {
		return current, nil
	}
	if current.Status != channelStatusManuallyDisabled {
		return domain.TargetChannel{}, fmt.Errorf("refusing to enable channel in status %d", current.Status)
	}
	path := "/api/channel/" + strconv.FormatInt(id, 10) + "/status"
	if _, err := a.managementRequest(ctx, target, http.MethodPost, path, nil, map[string]int{"status": channelStatusEnabled}); err != nil {
		return domain.TargetChannel{}, fmt.Errorf("enable target channel: %w", err)
	}
	readback, err := a.GetChannel(ctx, target, id)
	if err != nil {
		return domain.TargetChannel{}, fmt.Errorf("read back enabled target channel: %w", err)
	}
	if readback.Remark != marker || readback.Status != channelStatusEnabled {
		return domain.TargetChannel{}, errors.New("enabled channel readback did not preserve ownership and enabled status")
	}
	return readback, nil
}

func (a *NewAPI) DisableOwnedChannel(ctx context.Context, target domain.TargetSite, id int64, marker string) (domain.TargetChannel, error) {
	if err := a.remoteWrites.Check("target channel disable"); err != nil {
		return domain.TargetChannel{}, err
	}
	current, err := a.GetChannel(ctx, target, id)
	if err != nil {
		return domain.TargetChannel{}, err
	}
	if current.Remark != marker {
		return domain.TargetChannel{}, errors.New("refusing to disable a channel not owned by this change set")
	}
	if current.Status == channelStatusManuallyDisabled {
		return current, nil
	}
	if current.Status != channelStatusEnabled {
		return domain.TargetChannel{}, fmt.Errorf("refusing to disable channel in status %d", current.Status)
	}
	path := "/api/channel/" + strconv.FormatInt(id, 10) + "/status"
	if _, err := a.managementRequest(ctx, target, http.MethodPost, path, nil, map[string]int{"status": channelStatusManuallyDisabled}); err != nil {
		return domain.TargetChannel{}, fmt.Errorf("disable target channel: %w", err)
	}
	readback, err := a.GetChannel(ctx, target, id)
	if err != nil {
		return domain.TargetChannel{}, fmt.Errorf("read back disabled target channel: %w", err)
	}
	if readback.Remark != marker || readback.Status != channelStatusManuallyDisabled {
		return domain.TargetChannel{}, errors.New("disabled channel readback did not preserve ownership and disabled status")
	}
	return readback, nil
}

func (a *NewAPI) DeleteOwnedDisabledChannel(ctx context.Context, target domain.TargetSite, id int64, marker string) error {
	if err := a.remoteWrites.Check("target channel delete"); err != nil {
		return err
	}
	// Re-read immediately before deletion. Stock NewAPI has no delete-CAS
	// endpoint, so this is the narrowest possible compensation guard without a
	// NewAPI patch; the ownership marker prevents deleting pre-existing rows.
	current, err := a.GetChannel(ctx, target, id)
	if errors.Is(err, ErrChannelNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Remark != marker {
		return errors.New("refusing to delete a channel not owned by this change set")
	}
	if current.Status != channelStatusManuallyDisabled {
		return fmt.Errorf("refusing to delete owned channel because status is %d instead of manually disabled", current.Status)
	}
	if _, err := a.managementRequest(ctx, target, http.MethodDelete, "/api/channel/"+strconv.FormatInt(id, 10), nil, nil); err != nil {
		return fmt.Errorf("delete owned disabled channel: %w", err)
	}
	return nil
}

func (a *NewAPI) findChannelByMarker(ctx context.Context, target domain.TargetSite, marker string) (domain.TargetChannel, bool, error) {
	const pageSize = 1000
	for page := 1; page <= 100; page++ {
		query := url.Values{"p": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}, "id_sort": {"true"}, "tag_mode": {"false"}}
		body, err := a.managementRequest(ctx, target, http.MethodGet, "/api/channel/", query, nil)
		if err != nil {
			return domain.TargetChannel{}, false, fmt.Errorf("list target channels: %w", err)
		}
		var pageData struct {
			Items []newAPIChannel `json:"items"`
			Total int             `json:"total"`
		}
		if err := decodeManagementData(body, &pageData); err != nil {
			return domain.TargetChannel{}, false, err
		}
		for _, item := range pageData.Items {
			if stringPointer(item.Remark) == marker {
				return item.toDomain(), true, nil
			}
		}
		if len(pageData.Items) == 0 || page*pageSize >= pageData.Total {
			return domain.TargetChannel{}, false, nil
		}
	}
	return domain.TargetChannel{}, false, errors.New("target channel listing exceeded the safety page limit")
}

func (a *NewAPI) managementRequest(ctx context.Context, target domain.TargetSite, method, path string, query url.Values, payload any) ([]byte, error) {
	if err := a.remoteWrites.CheckRequest(ctx, method, "target management mutation"); err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	client, err := netguard.SameOriginClient(a.client, base)
	if err != nil {
		return nil, err
	}
	var bodyBytes []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyBytes = encoded
	}
	requestURL := base + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	requestContext, cancel := withTimeout(ctx)
	defer cancel()
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var body io.Reader
		if payload != nil {
			body = strings.NewReader(string(bodyBytes))
		}
		req, err := http.NewRequestWithContext(requestContext, method, requestURL, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := a.authorize(ctx, target, req); err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &RemoteError{Operation: method + " " + path, Err: err}
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		retryAfter := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if resp.StatusCode == http.StatusTooManyRequests && attempt < attempts-1 {
				if err := waitTargetReadRetry(requestContext, retryAfter, attempt); err != nil {
					return nil, err
				}
				continue
			}
			if resp.StatusCode == http.StatusNotFound {
				return nil, ErrChannelNotFound
			}
			return nil, remoteHTTPError(method+" "+path, resp.StatusCode, responseBody)
		}
		var envelope struct {
			Success *bool  `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(responseBody, &envelope); err != nil {
			return nil, &RemoteError{Operation: "decode " + method + " " + path, Definitive: true, Err: err}
		}
		if envelope.Success == nil {
			return nil, &RemoteError{Operation: method + " " + path, Message: "target response omitted success", Definitive: true}
		}
		if !*envelope.Success {
			return nil, &RemoteError{Operation: method + " " + path,
				Message: safeRemoteMessage(envelope.Message, "target rejected the operation"), Definitive: true}
		}
		if method != http.MethodGet {
			a.invalidateTargetReads(target)
		}
		return responseBody, nil
	}
	return nil, errors.New("target management read retry exhausted")
}

func decodeManagementData(body []byte, destination any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("target response omitted data")
	}
	return json.Unmarshal(envelope.Data, destination)
}

func (channel newAPIChannel) toDomain() domain.TargetChannel {
	autoBan := false
	if channel.AutoBan != nil {
		autoBan = *channel.AutoBan != 0
	}
	result := domain.TargetChannel{
		ID: channel.ID, Name: channel.Name, Type: channel.Type, Status: channel.Status,
		BaseURL: stringPointer(channel.BaseURL), Models: normalizeModels(strings.Split(channel.Models, ",")),
		Group: channel.Group, Priority: int64Pointer(channel.Priority), Weight: uintPointer(channel.Weight),
		AutoBan: autoBan, ModelMapping: stringPointer(channel.ModelMapping), Tag: stringPointer(channel.Tag),
		Remark: stringPointer(channel.Remark),
	}
	result.ConfigHash = HashTargetChannel(result)
	return result
}

func HashTargetChannel(channel domain.TargetChannel) string {
	canonical := struct {
		ID           int64    `json:"id"`
		Name         string   `json:"name"`
		Type         int      `json:"type"`
		Status       int      `json:"status"`
		BaseURL      string   `json:"base_url"`
		Models       []string `json:"models"`
		Group        string   `json:"group"`
		Priority     int64    `json:"priority"`
		Weight       uint     `json:"weight"`
		AutoBan      bool     `json:"auto_ban"`
		ModelMapping string   `json:"model_mapping"`
		Tag          string   `json:"tag"`
		Remark       string   `json:"remark"`
	}{
		ID: channel.ID, Name: channel.Name, Type: channel.Type, Status: channel.Status,
		BaseURL: strings.TrimRight(channel.BaseURL, "/"), Models: normalizeModels(channel.Models), Group: channel.Group,
		Priority: channel.Priority, Weight: channel.Weight, AutoBan: channel.AutoBan, ModelMapping: channel.ModelMapping,
		Tag: channel.Tag, Remark: channel.Remark,
	}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func verifyOwnedChannel(channel domain.TargetChannel, spec domain.ListingChannelSpec, group, marker string, expectedStatus int) error {
	if channel.Remark != marker {
		return errors.New("ownership marker differs")
	}
	if channel.Status != expectedStatus {
		return fmt.Errorf("status is %d, expected %d", channel.Status, expectedStatus)
	}
	if channel.Name != strings.TrimSpace(spec.Name) || channel.Type != spec.Type ||
		strings.TrimRight(channel.BaseURL, "/") != strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/") || channel.Group != group {
		return errors.New("channel identity fields differ")
	}
	wantModels := normalizeModels(spec.Models)
	gotModels := normalizeModels(channel.Models)
	if strings.Join(wantModels, "\x00") != strings.Join(gotModels, "\x00") {
		return errors.New("channel model scope differs")
	}
	return nil
}

func normalizeModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	result := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	sort.Strings(result)
	return result
}

func nullableString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func stringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int64Pointer(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func uintPointer(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
}
