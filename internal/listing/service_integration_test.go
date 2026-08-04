package listing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type fakeChannel struct {
	domain.TargetChannel
	Key string
}

type fakeNewAPI struct {
	mu                         sync.Mutex
	ratios                     map[string]string
	modelPrices                map[string]string
	usableGroups               map[string]string
	channels                   map[int64]fakeChannel
	nextID                     int64
	createCalls                int
	fetchModelCalls            int
	testCalls                  int
	optionWrites               int
	visibilityWrites           int
	events                     []string
	failCreateBusiness         bool
	failTest                   bool
	failTestMissingPrice       bool
	externalChangeOnTest       bool
	externalEnableOnTest       bool
	groupPricingSupported      bool
	groupPricing               domain.GroupModelPricingContract
	groupPricingWrites         int
	failGroupPricingWrite      bool
	externalContractAfterWrite bool
	opaqueGroupRevisions       bool
	alterNextGroupPreview      bool
	onPricingRead              func()
	availableModels            []string
	fetchModelStatus           int
	pricingData                []map[string]any
	managedStateReads          int
	managedStateWrites         int
	onManagedStateRead         func()
	foreignMarkerOnRead        bool
}

type fakeGroupModelWireContract struct {
	SchemaVersion int                                             `json:"schema_version"`
	Revision      string                                          `json:"revision,omitempty"`
	Groups        map[string]map[string]fakeGroupModelWireSetting `json:"groups"`
}

type fakeGroupModelWireSetting struct {
	BillingMode            string                        `json:"billing_mode"`
	ModelPrice             *json.Number                  `json:"model_price,omitempty"`
	ModelRatio             *json.Number                  `json:"model_ratio,omitempty"`
	CompletionRatio        *json.Number                  `json:"completion_ratio,omitempty"`
	CacheRatio             *json.Number                  `json:"cache_ratio,omitempty"`
	CacheCreationRatio     *json.Number                  `json:"cache_creation_ratio,omitempty"`
	CacheCreation5mRatio   *json.Number                  `json:"cache_creation_5m_ratio,omitempty"`
	CacheCreation1hRatio   *json.Number                  `json:"cache_creation_1h_ratio,omitempty"`
	ImageRatio             *json.Number                  `json:"image_ratio,omitempty"`
	AudioRatio             *json.Number                  `json:"audio_ratio,omitempty"`
	AudioCompletionRatio   *json.Number                  `json:"audio_completion_ratio,omitempty"`
	BillingExpr            string                        `json:"billing_expr,omitempty"`
	TaskProfile            *domain.GroupModelTaskProfile `json:"task_profile,omitempty"`
	Source                 string                        `json:"source,omitempty"`
	SourceRevision         string                        `json:"source_revision,omitempty"`
	SourceObservedAtUnixMS int64                         `json:"source_observed_at_unix_ms,omitempty"`
}

func (fake *fakeNewAPI) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	switch {
	case request.URL.Path == "/api/option/group-model-pricing" && request.Method == http.MethodGet:
		if !fake.groupPricingSupported {
			http.NotFound(response, request)
			return
		}
		if fake.externalContractAfterWrite && fake.groupPricingWrites > 0 {
			price := "99"
			fake.groupPricing.Groups["external"] = map[string]domain.GroupModelPricingSetting{
				"changed": {BillingMode: "fixed_price", ModelPrice: &price},
			}
			fake.groupPricing.Revision = fake.groupModelRevision(fake.groupPricing)
			fake.externalContractAfterWrite = false
		}
		fake.writeGroupPricingEnvelope(response, fake.groupPricing, fake.groupPricing.Revision, true, false)
	case request.URL.Path == "/api/option/group-model-pricing/preview" && request.Method == http.MethodPost:
		if !fake.groupPricingSupported {
			http.NotFound(response, request)
			return
		}
		var input struct {
			ExpectedRevision string                     `json:"expected_revision"`
			Contract         fakeGroupModelWireContract `json:"contract"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		desired := fakeGroupContractFromWire(input.Contract)
		if fake.alterNextGroupPreview {
			price := "99"
			desired.Groups["outside-filtered-listing"] = map[string]domain.GroupModelPricingSetting{
				"changed": {BillingMode: "fixed_price", ModelPrice: &price},
			}
			fake.alterNextGroupPreview = false
		}
		desired.Revision = fake.groupModelRevision(desired)
		fake.writeGroupPricingEnvelope(response, desired, fake.groupPricing.Revision,
			input.ExpectedRevision == fake.groupPricing.Revision, desired.Revision != fake.groupPricing.Revision)
	case request.URL.Path == "/api/option/group-model-pricing" && request.Method == http.MethodPut:
		if !fake.groupPricingSupported {
			http.NotFound(response, request)
			return
		}
		var input struct {
			ExpectedRevision string                     `json:"expected_revision"`
			Contract         fakeGroupModelWireContract `json:"contract"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		if fake.failGroupPricingWrite {
			writeFake(response, map[string]any{"success": false, "message": "contract write rejected"})
			return
		}
		if input.ExpectedRevision != fake.groupPricing.Revision {
			writeFake(response, map[string]any{"success": false, "message": "contract revision conflict"})
			return
		}
		desired := fakeGroupContractFromWire(input.Contract)
		desired.Revision = fake.groupModelRevision(desired)
		fake.groupPricing = desired
		fake.groupPricingWrites++
		fake.events = append(fake.events, "contract:write")
		fake.writeGroupPricingEnvelope(response, desired, input.ExpectedRevision, true, true)
	case request.URL.Path == "/api/option/" && request.Method == http.MethodGet:
		encoded, _ := json.Marshal(fake.ratios)
		modelPrices, _ := json.Marshal(fake.modelPrices)
		usable, _ := json.Marshal(fake.usableGroups)
		writeFake(response, map[string]any{"success": true, "data": []map[string]string{
			{"key": "GroupRatio", "value": string(encoded)},
			{"key": "ModelPrice", "value": string(modelPrices)},
			{"key": "UserUsableGroups", "value": string(usable)},
		}})
	case request.URL.Path == "/api/option/" && request.Method == http.MethodPut:
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		if input.Key == "UserUsableGroups" {
			var values map[string]string
			if json.Unmarshal([]byte(input.Value), &values) != nil {
				writeFake(response, map[string]any{"success": false, "message": "invalid UserUsableGroups"})
				return
			}
			fake.usableGroups = values
			fake.visibilityWrites++
			fake.events = append(fake.events, "visibility:write")
			writeFake(response, map[string]any{"success": true})
			return
		}
		var values map[string]json.Number
		if (input.Key != "GroupRatio" && input.Key != "ModelPrice") || json.Unmarshal([]byte(input.Value), &values) != nil {
			writeFake(response, map[string]any{"success": false, "message": "invalid pricing option"})
			return
		}
		destination := make(map[string]string, len(values))
		for key, value := range values {
			destination[key] = value.String()
		}
		if input.Key == "ModelPrice" {
			fake.modelPrices = destination
			fake.events = append(fake.events, "model-price:write")
		} else {
			fake.ratios = destination
			fake.events = append(fake.events, "ratio:write")
		}
		fake.optionWrites++
		writeFake(response, map[string]any{"success": true})
	case request.URL.Path == "/api/pricing" && request.Method == http.MethodGet:
		if callback := fake.onPricingRead; callback != nil {
			fake.onPricingRead = nil
			callback()
		}
		data := fake.pricingData
		if data == nil {
			data = []map[string]any{{"model_name": "m1", "quota_type": 0, "model_ratio": 2,
				"completion_ratio": 2, "enable_groups": []string{"default"}}}
		}
		writeFake(response, map[string]any{"success": true, "data": data, "group_ratio": fake.ratios})
	case request.URL.Path == "/api/user/self" && request.Method == http.MethodGet:
		writeFake(response, map[string]any{"success": true, "data": map[string]any{"balance": 100, "currency": "USD"}})
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodGet:
		items := make([]map[string]any, 0, len(fake.channels))
		for _, channel := range fake.channels {
			items = append(items, fakeChannelJSON(channel.TargetChannel))
		}
		writeFake(response, map[string]any{"success": true, "data": map[string]any{"items": items, "total": len(items)}})
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodPost:
		fake.createCalls++
		fake.events = append(fake.events, "channel:create")
		if fake.failCreateBusiness {
			writeFake(response, map[string]any{"success": false, "message": "business rejected create"})
			return
		}
		var input struct {
			Channel struct {
				Name         string  `json:"name"`
				Type         int     `json:"type"`
				Key          string  `json:"key"`
				Status       int     `json:"status"`
				BaseURL      string  `json:"base_url"`
				Models       string  `json:"models"`
				Group        string  `json:"group"`
				Priority     int64   `json:"priority"`
				Weight       uint    `json:"weight"`
				AutoBan      int     `json:"auto_ban"`
				ModelMapping *string `json:"model_mapping"`
				Tag          *string `json:"tag"`
				Remark       *string `json:"remark"`
			} `json:"channel"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		fake.nextID++
		channel := domain.TargetChannel{ID: fake.nextID, Name: input.Channel.Name, Type: input.Channel.Type,
			Status: input.Channel.Status, BaseURL: input.Channel.BaseURL, Models: strings.Split(input.Channel.Models, ","),
			Group: input.Channel.Group, Priority: input.Channel.Priority, Weight: input.Channel.Weight,
			AutoBan: input.Channel.AutoBan != 0, ModelMapping: valueOf(input.Channel.ModelMapping),
			Tag: valueOf(input.Channel.Tag), Remark: valueOf(input.Channel.Remark)}
		fake.channels[channel.ID] = fakeChannel{TargetChannel: channel, Key: input.Channel.Key}
		writeFake(response, map[string]any{"success": true})
	case strings.HasPrefix(request.URL.Path, "/api/channel/fetch_models/") && request.Method == http.MethodGet:
		fake.fetchModelCalls++
		if fake.fetchModelStatus != 0 {
			response.WriteHeader(fake.fetchModelStatus)
			writeFake(response, map[string]any{"success": false, "message": "temporary discovery failure"})
			return
		}
		models := fake.availableModels
		if models == nil {
			models = []string{"m1"}
		}
		writeFake(response, map[string]any{"success": true, "data": models})
	case strings.HasPrefix(request.URL.Path, "/api/channel/test/") && request.Method == http.MethodGet:
		fake.testCalls++
		fake.events = append(fake.events, "channel:test")
		if fake.externalChangeOnTest {
			fake.ratios["external"] = "9"
		}
		if fake.externalEnableOnTest {
			id := channelIDFromPath(request.URL.Path)
			channel := fake.channels[id]
			channel.Status = 1
			fake.channels[id] = channel
		}
		if fake.failTest {
			writeFake(response, map[string]any{"success": false, "message": "upstream test failed"})
			return
		}
		if fake.failTestMissingPrice {
			model := strings.TrimSpace(request.URL.Query().Get("model"))
			if _, configured := fake.modelPrices[model]; !configured {
				writeFake(response, map[string]any{"success": false, "message": "模型价格未配置"})
				return
			}
		}
		writeFake(response, map[string]any{"success": true, "time": 0.01})
	case strings.HasSuffix(request.URL.Path, "/status") && request.Method == http.MethodPost:
		id := channelIDFromPath(request.URL.Path)
		channel := fake.channels[id]
		var input struct {
			Status int `json:"status"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		channel.Status = input.Status
		if input.Status == 1 {
			fake.events = append(fake.events, "channel:enable")
		} else {
			fake.events = append(fake.events, "channel:disable")
		}
		fake.channels[id] = channel
		writeFake(response, map[string]any{"success": true, "data": true})
	case strings.HasSuffix(request.URL.Path, "/managed_state") && request.Method == http.MethodGet:
		id := channelIDFromPath(request.URL.Path)
		channel, ok := fake.channels[id]
		if !ok {
			writeFake(response, map[string]any{"success": false, "message": "not found"})
			return
		}
		fake.managedStateReads++
		if callback := fake.onManagedStateRead; callback != nil {
			fake.onManagedStateRead = nil
			callback()
		}
		if fake.foreignMarkerOnRead {
			fake.foreignMarkerOnRead = false
			for groupName, models := range fake.groupPricing.Groups {
				for modelName, setting := range models {
					setting.Source = "relayops:policy/foreign"
					models[modelName] = setting
					fake.groupPricing.Groups[groupName] = models
					break
				}
				break
			}
		}
		writeFake(response, map[string]any{"success": true, "data": fakeManagedChannelState(channel)})
	case strings.HasSuffix(request.URL.Path, "/managed_state") && request.Method == http.MethodPut:
		id := channelIDFromPath(request.URL.Path)
		channel, ok := fake.channels[id]
		if !ok {
			writeFake(response, map[string]any{"success": false, "message": "not found"})
			return
		}
		var input struct {
			ExpectedHash  string   `json:"expected_hash"`
			DesiredModels []string `json:"desired_models"`
			DesiredStatus *int     `json:"desired_status"`
			RequestID     string   `json:"request_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeFake(response, map[string]any{"success": false, "message": "invalid managed state"})
			return
		}
		before := fakeManagedChannelState(channel)
		if input.ExpectedHash != before.Hash || strings.TrimSpace(input.RequestID) == "" {
			writeFake(response, map[string]any{"success": false, "message": "managed state conflict"})
			return
		}
		channel.Models = sortedUnique(input.DesiredModels)
		if input.DesiredStatus != nil {
			channel.Status = *input.DesiredStatus
		}
		fake.channels[id] = channel
		fake.managedStateWrites++
		fake.events = append(fake.events, "managed-state:write")
		after := fakeManagedChannelState(channel)
		writeFake(response, map[string]any{"success": true, "data": map[string]any{
			"changed": before.Hash != after.Hash, "state": after,
		}})
	case strings.HasPrefix(request.URL.Path, "/api/channel/") && request.Method == http.MethodGet:
		id := channelIDFromPath(request.URL.Path)
		channel, ok := fake.channels[id]
		if !ok {
			writeFake(response, map[string]any{"success": false, "message": "not found"})
			return
		}
		writeFake(response, map[string]any{"success": true, "data": fakeChannelJSON(channel.TargetChannel)})
	case strings.HasPrefix(request.URL.Path, "/api/channel/") && request.Method == http.MethodDelete:
		id := channelIDFromPath(request.URL.Path)
		delete(fake.channels, id)
		fake.events = append(fake.events, "channel:delete")
		writeFake(response, map[string]any{"success": true})
	default:
		http.NotFound(response, request)
	}
}

func (fake *fakeNewAPI) enableGroupModelPricing() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.groupPricingSupported = true
	fake.groupPricing = domain.GroupModelPricingContract{SchemaVersion: 1, Groups: map[string]map[string]domain.GroupModelPricingSetting{}}
	fake.groupPricing.Revision = fake.groupModelRevision(fake.groupPricing)
}

func (fake *fakeNewAPI) useOpaqueGroupPricingRevisions() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.opaqueGroupRevisions = true
	if fake.groupPricingSupported {
		fake.groupPricing.Revision = fake.groupModelRevision(fake.groupPricing)
	}
}

func (fake *fakeNewAPI) alterNextGroupPricingPreview() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.alterNextGroupPreview = true
}

func (fake *fakeNewAPI) groupModelRevision(contract domain.GroupModelPricingContract) string {
	revision := targets.HashGroupModelPricingContract(contract)
	if fake.opaqueGroupRevisions {
		return "target-authority-" + revision
	}
	return revision
}

func (fake *fakeNewAPI) writeGroupPricingEnvelope(response http.ResponseWriter, contract domain.GroupModelPricingContract,
	currentRevision string, expectedMatches, changed bool) {
	writeFake(response, map[string]any{"success": true, "data": map[string]any{
		"supported": true, "schema_version": 1, "revision": contract.Revision,
		"current_revision": currentRevision, "expected_revision_matches": expectedMatches,
		"changed": changed, "contract": fakeGroupContractToWire(contract),
	}})
}

func fakeGroupContractToWire(contract domain.GroupModelPricingContract) fakeGroupModelWireContract {
	wire := fakeGroupModelWireContract{SchemaVersion: contract.SchemaVersion, Revision: contract.Revision,
		Groups: make(map[string]map[string]fakeGroupModelWireSetting, len(contract.Groups))}
	for group, models := range contract.Groups {
		wire.Groups[group] = make(map[string]fakeGroupModelWireSetting, len(models))
		for model, setting := range models {
			wireSetting := fakeGroupModelWireSetting{BillingMode: setting.BillingMode, BillingExpr: setting.BillingExpr,
				TaskProfile: setting.TaskProfile, Source: setting.Source, SourceRevision: setting.SourceRevision,
				SourceObservedAtUnixMS: setting.SourceObservedAtUnixMS}
			fakeCopyStringNumbersToJSON(setting, &wireSetting)
			wire.Groups[group][model] = wireSetting
		}
	}
	return wire
}

func fakeGroupContractFromWire(wire fakeGroupModelWireContract) domain.GroupModelPricingContract {
	contract := domain.GroupModelPricingContract{SchemaVersion: wire.SchemaVersion,
		Groups: make(map[string]map[string]domain.GroupModelPricingSetting, len(wire.Groups))}
	for group, models := range wire.Groups {
		contract.Groups[group] = make(map[string]domain.GroupModelPricingSetting, len(models))
		for model, setting := range models {
			converted := domain.GroupModelPricingSetting{BillingMode: setting.BillingMode, BillingExpr: setting.BillingExpr,
				TaskProfile: setting.TaskProfile, Source: setting.Source, SourceRevision: setting.SourceRevision,
				SourceObservedAtUnixMS: setting.SourceObservedAtUnixMS}
			fakeCopyJSONNumbersToString(setting, &converted)
			contract.Groups[group][model] = converted
		}
	}
	return contract
}

func fakeCopyStringNumbersToJSON(source domain.GroupModelPricingSetting, target *fakeGroupModelWireSetting) {
	pairs := []struct {
		source *string
		target **json.Number
	}{
		{source.ModelPrice, &target.ModelPrice}, {source.ModelRatio, &target.ModelRatio},
		{source.CompletionRatio, &target.CompletionRatio}, {source.CacheRatio, &target.CacheRatio},
		{source.CacheCreationRatio, &target.CacheCreationRatio}, {source.CacheCreation5mRatio, &target.CacheCreation5mRatio},
		{source.CacheCreation1hRatio, &target.CacheCreation1hRatio}, {source.ImageRatio, &target.ImageRatio},
		{source.AudioRatio, &target.AudioRatio}, {source.AudioCompletionRatio, &target.AudioCompletionRatio},
	}
	for _, pair := range pairs {
		if pair.source != nil {
			value := json.Number(*pair.source)
			*pair.target = &value
		}
	}
}

func fakeCopyJSONNumbersToString(source fakeGroupModelWireSetting, target *domain.GroupModelPricingSetting) {
	pairs := []struct {
		source *json.Number
		target **string
	}{
		{source.ModelPrice, &target.ModelPrice}, {source.ModelRatio, &target.ModelRatio},
		{source.CompletionRatio, &target.CompletionRatio}, {source.CacheRatio, &target.CacheRatio},
		{source.CacheCreationRatio, &target.CacheCreationRatio}, {source.CacheCreation5mRatio, &target.CacheCreation5mRatio},
		{source.CacheCreation1hRatio, &target.CacheCreation1hRatio}, {source.ImageRatio, &target.ImageRatio},
		{source.AudioRatio, &target.AudioRatio}, {source.AudioCompletionRatio, &target.AudioCompletionRatio},
	}
	for _, pair := range pairs {
		if pair.source != nil {
			value := pair.source.String()
			*pair.target = &value
		}
	}
}

func (fake *fakeNewAPI) snapshot() (map[string]string, int, int, int, int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	ratios := make(map[string]string, len(fake.ratios))
	for key, value := range fake.ratios {
		ratios[key] = value
	}
	return ratios, len(fake.channels), fake.createCalls, fake.testCalls, fake.optionWrites
}

func (fake *fakeNewAPI) modelDiscoveryCalls() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.fetchModelCalls
}

func (fake *fakeNewAPI) groupPricingSnapshot() (domain.GroupModelPricingContract, int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	encoded, _ := json.Marshal(fake.groupPricing)
	var contract domain.GroupModelPricingContract
	_ = json.Unmarshal(encoded, &contract)
	return contract, fake.groupPricingWrites
}

func (fake *fakeNewAPI) setRatio(key, value string) {
	fake.mu.Lock()
	fake.ratios[key] = value
	fake.mu.Unlock()
}

func (fake *fakeNewAPI) setAvailableModels(models []string) {
	fake.mu.Lock()
	fake.availableModels = sortedUnique(models)
	fake.mu.Unlock()
}

func (fake *fakeNewAPI) setFetchModelStatus(status int) {
	fake.mu.Lock()
	fake.fetchModelStatus = status
	fake.mu.Unlock()
}

func (fake *fakeNewAPI) channelModels() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, channel := range fake.channels {
		return sortedUnique(channel.Models)
	}
	return nil
}

func (fake *fakeNewAPI) setPricingData(data []map[string]any) {
	fake.mu.Lock()
	fake.pricingData = data
	fake.mu.Unlock()
}

func (fake *fakeNewAPI) eventLog() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return strings.Join(fake.events, ",")
}

func (fake *fakeNewAPI) managedStateSnapshot() (int, int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.managedStateReads, fake.managedStateWrites
}

func (fake *fakeNewAPI) visibilitySnapshot() (map[string]string, int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return cloneRatios(fake.usableGroups), fake.visibilityWrites
}

type listingFixture struct {
	service *Service
	store   *store.Store
	remote  *fakeNewAPI
	server  *httptest.Server
	request domain.ListingPlanRequest
}

func newListingFixture(t *testing.T) listingFixture {
	return newListingFixtureWithBaseline(t, false)
}

func newListingFixtureWithBaseline(t *testing.T, baseline bool) listingFixture {
	t.Helper()
	remote := &fakeNewAPI{ratios: map[string]string{"default": "1"}, modelPrices: map[string]string{}, usableGroups: map[string]string{"default": "Default"}, channels: map[int64]fakeChannel{}, nextID: 10,
		availableModels: []string{"m1"}}
	server := httptest.NewServer(remote)
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	management := domain.Credential{Name: "target admin", Kind: "management_token", SiteURL: server.URL}
	if err := credentialService.Create(context.Background(), &management, domain.CredentialPayload{Authorization: "Bearer admin", UserID: 1}); err != nil {
		t.Fatal(err)
	}
	upstreamKey := domain.Credential{Name: "supplier API key", Kind: "upstream_api_key", SiteURL: server.URL}
	if err := credentialService.Create(context.Background(), &upstreamKey, domain.CredentialPayload{AuthorizationMode: "raw", Authorization: "sk-upstream"}); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "supplier", BaseURL: server.URL, Adapter: "newapi", Enabled: true, Metadata: map[string]string{}}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: []string{"m1"}}},
		Models: []domain.ModelOffer{{ExternalID: "m1", Name: "m1", BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "4"}}},
		Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "source-catalog-v1"}}
	if err := st.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDiscoveries(context.Background(), snapshot, baseline); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "production", BaseURL: server.URL, Adapter: "newapi", CredentialID: management.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, targets.NewFactory(server.Client(), credentialService), credentialService)
	t.Cleanup(func() { server.Close(); _ = st.Close() })
	return listingFixture{service: service, store: st, remote: remote, server: server,
		request: domain.ListingPlanRequest{SupplierID: supplier.ID, DiscoveryKind: "group", DiscoveryExternalID: "promo",
			TargetSiteID: target.ID, TargetGroupMode: "new", TargetGroup: "promo-local", UpstreamCredentialID: upstreamKey.ID,
			Channel: domain.ListingChannelSpec{Name: "promo upstream", Type: 1, BaseURL: server.URL, Models: []string{"m1"},
				TestModel: "m1", AutoBan: true},
			PricingRule: domain.ListingPricingRule{RuleType: "multiply", Value: "1.2"}, IdempotencyKey: "preview-1"}}
}

func catalogListingRequest(fixture listingFixture, key string) domain.ListingPlanRequest {
	request := fixture.request
	request.DiscoveryKind = "catalog_group"
	request.SourceSnapshotHash = "source-catalog-v1"
	request.ExpectedSourceRatio = "0.5"
	request.ConfirmCatalogGroup = true
	request.IdempotencyKey = key
	return request
}

func assertNoListingTargetWrites(t *testing.T, fixture listingFixture) {
	t.Helper()
	_, channels, creates, tests, ratioWrites := fixture.remote.snapshot()
	_, contractWrites := fixture.remote.groupPricingSnapshot()
	if channels != 0 || creates != 0 || tests != 0 || ratioWrites != 0 || contractWrites != 0 {
		t.Fatalf("rejected preview mutated target: channels=%d creates=%d tests=%d ratioWrites=%d contractWrites=%d",
			channels, creates, tests, ratioWrites, contractWrites)
	}
}

func saveListingSourceSnapshot(t *testing.T, fixture listingFixture, hash string, groups []domain.GroupOffer) {
	saveListingSourceSnapshotAt(t, fixture, hash, groups, time.Now().UTC().Add(time.Second))
}

func saveListingSourceSnapshotAt(t *testing.T, fixture listingFixture, hash string, groups []domain.GroupOffer, observedAt time.Time) {
	t.Helper()
	snapshot := domain.CatalogSnapshot{SupplierID: fixture.request.SupplierID, Groups: groups,
		Models: []domain.ModelOffer{{ExternalID: "m1", Name: "m1", BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "4"}}},
		Evidence: domain.Evidence{ObservedAt: observedAt, ResponseHash: hash}}
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
}

func TestListingCatalogGroupSourceGuards(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, listingFixture, *domain.ListingPlanRequest)
		want   string
	}{
		{name: "explicit confirmation", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.ConfirmCatalogGroup = false
		}, want: "confirm_catalog_group=true"},
		{name: "frozen snapshot hash", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.SourceSnapshotHash = ""
		}, want: "source_snapshot_hash is required"},
		{name: "frozen effective ratio", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.ExpectedSourceRatio = ""
		}, want: "expected_source_ratio is required"},
		{name: "changed effective ratio", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.ExpectedSourceRatio = "0.4"
		}, want: "source effective ratio changed"},
		{name: "latest snapshot hash", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.SourceSnapshotHash = "stale-catalog"
		}, want: "source_snapshot_hash is stale"},
		{name: "fresh observation time", mutate: func(t *testing.T, fixture listingFixture, _ *domain.ListingPlanRequest) {
			saveListingSourceSnapshotAt(t, fixture, "source-catalog-v1", []domain.GroupOffer{{
				ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: []string{"m1"},
			}}, time.Now().UTC().Add(-catalogListingFreshnessWindow-time.Minute))
		}, want: "catalog snapshot is too old"},
		{name: "future observation time", mutate: func(t *testing.T, fixture listingFixture, _ *domain.ListingPlanRequest) {
			saveListingSourceSnapshotAt(t, fixture, "source-catalog-v1", []domain.GroupOffer{{
				ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: []string{"m1"},
			}}, time.Now().UTC().Add(catalogFutureClockTolerance+time.Minute))
		}, want: "is in the future"},
		{name: "enabled supplier", mutate: func(t *testing.T, fixture listingFixture, _ *domain.ListingPlanRequest) {
			supplier, err := fixture.store.GetSupplier(context.Background(), fixture.request.SupplierID)
			if err != nil {
				t.Fatal(err)
			}
			supplier.Enabled = false
			if err := fixture.store.UpdateSupplier(context.Background(), supplier); err != nil {
				t.Fatal(err)
			}
		}, want: "supplier is disabled"},
		{name: "current exact group", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.DiscoveryExternalID = "removed-group"
		}, want: "absent from the latest supplier snapshot"},
		{name: "nonempty group models", mutate: func(t *testing.T, fixture listingFixture, request *domain.ListingPlanRequest) {
			saveListingSourceSnapshot(t, fixture, "source-empty-group", []domain.GroupOffer{{ExternalID: "promo", Name: "promo", Ratio: "0.5"}})
			request.SourceSnapshotHash = "source-empty-group"
		}, want: "has no models"},
		{name: "selected model subset", mutate: func(_ *testing.T, _ listingFixture, request *domain.ListingPlanRequest) {
			request.Channel.Models = []string{"m1", "m2"}
		}, want: "outside catalog group"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newListingFixtureWithBaseline(t, true)
			request := catalogListingRequest(fixture, "guard-"+strings.ReplaceAll(test.name, " ", "-"))
			test.mutate(t, fixture, &request)
			if _, err := fixture.service.Preview(context.Background(), request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preview error = %v, want %q", err, test.want)
			}
			assertNoListingTargetWrites(t, fixture)
		})
	}
}

func TestListingCatalogGroupCanListBaselineWithFrozenSourceAndRollback(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)

	legacy := fixture.request
	legacy.IdempotencyKey = "baseline-discovery-rejected"
	if _, err := fixture.service.Preview(context.Background(), legacy); err == nil || !strings.Contains(err.Error(), "first catalog baseline") {
		t.Fatalf("baseline discovery error = %v", err)
	}

	request := catalogListingRequest(fixture, "catalog-baseline-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("catalog Preview: %v", err)
	}
	if plan.DiscoveryKind != "catalog_group" || plan.SourceSnapshotHash != request.SourceSnapshotHash || plan.SourceGroup != "promo" {
		t.Fatalf("catalog plan source = %#v", plan)
	}
	stored, err := fixture.store.GetListingPlan(context.Background(), plan.ID)
	if err != nil || stored.DiscoveryKind != "catalog_group" || stored.SourceSnapshotHash != request.SourceSnapshotHash {
		t.Fatalf("stored catalog plan = %#v, %v", stored, err)
	}
	activities, err := fixture.store.RecentActivity(context.Background(), 10)
	if err != nil || len(activities) == 0 || activities[0].Metadata["discovery_kind"] != "catalog_group" ||
		activities[0].Metadata["source_group"] != "promo" || activities[0].Metadata["source_snapshot_hash"] != request.SourceSnapshotHash {
		t.Fatalf("catalog activity metadata = %#v, %v", activities, err)
	}
	replayedPlan, err := fixture.service.Preview(context.Background(), request)
	if err != nil || replayedPlan.ID != plan.ID {
		t.Fatalf("catalog preview replay = %#v, %v", replayedPlan, err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-baseline-apply", ConfirmEnable: true})
	if err != nil || change.Status != "applied" {
		t.Fatalf("catalog Apply = %#v, %v", change, err)
	}
	replayedChange, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-baseline-apply", ConfirmEnable: true})
	if err != nil || replayedChange.ID != change.ID {
		t.Fatalf("catalog apply replay = %#v, %v", replayedChange, err)
	}
	rolledBack, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err != nil || rolledBack.Status != "rolled_back" {
		t.Fatalf("catalog Rollback = %#v, %v", rolledBack, err)
	}
}

func TestListingCatalogGroupApplyRejectsChangedSourceSnapshot(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)
	request := catalogListingRequest(fixture, "catalog-stale-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	saveListingSourceSnapshot(t, fixture, "source-catalog-v2", []domain.GroupOffer{{
		ExternalID: "promo", Name: "promo", Ratio: "0.6", ModelIDs: []string{"m1"},
	}})
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-stale-apply", ConfirmEnable: true})
	if err == nil || change.Status != "stale" || !strings.Contains(err.Error(), "supplier pricing catalog changed") {
		t.Fatalf("stale catalog Apply = %#v, %v", change, err)
	}
	assertNoListingTargetWrites(t, fixture)
}

func TestListingCatalogGroupApplyRejectsSupplierDisabledAfterPreview(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)
	request := catalogListingRequest(fixture, "catalog-disabled-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := fixture.store.GetSupplier(context.Background(), fixture.request.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	supplier.Enabled = false
	if err := fixture.store.UpdateSupplier(context.Background(), supplier); err != nil {
		t.Fatal(err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-disabled-apply", ConfirmEnable: true})
	if err == nil || change.Status != "stale" || !strings.Contains(err.Error(), "supplier was disabled") {
		t.Fatalf("disabled catalog Apply = %#v, %v", change, err)
	}
	assertNoListingTargetWrites(t, fixture)
}

func TestListingCatalogGroupApplyRejectsExpiredUnchangedSnapshot(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)
	request := catalogListingRequest(fixture, "catalog-expired-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	saveListingSourceSnapshotAt(t, fixture, request.SourceSnapshotHash, []domain.GroupOffer{{
		ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: []string{"m1"},
	}}, time.Now().UTC().Add(-catalogListingFreshnessWindow-time.Minute))
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-expired-apply", ConfirmEnable: true})
	if err == nil || change.Status != "stale" || !strings.Contains(err.Error(), "catalog snapshot is too old") {
		t.Fatalf("expired catalog Apply = %#v, %v", change, err)
	}
	assertNoListingTargetWrites(t, fixture)
}

func TestListingCatalogGroupApplyRejectsRechargeFactorChange(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)
	request := catalogListingRequest(fixture, "catalog-factor-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := fixture.store.GetSupplier(context.Background(), fixture.request.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	supplier.RechargeValueFactor = "0.4"
	if err := fixture.store.UpdateSupplier(context.Background(), supplier); err != nil {
		t.Fatal(err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "catalog-factor-apply", ConfirmEnable: true})
	if err == nil || change.Status != "stale" || !strings.Contains(err.Error(), "effective source ratio changed") {
		t.Fatalf("recharge-factor catalog Apply = %#v, %v", change, err)
	}
	assertNoListingTargetWrites(t, fixture)
}

func TestListingCatalogGroupApplyRejectsFactorChangeBetweenSourcePreflights(t *testing.T) {
	fixture := newListingFixtureWithBaseline(t, true)
	request := catalogListingRequest(fixture, "catalog-factor-race-preview")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.mu.Lock()
	fixture.remote.onPricingRead = func() {
		supplier, getErr := fixture.store.GetSupplier(context.Background(), fixture.request.SupplierID)
		if getErr != nil {
			return
		}
		supplier.RechargeValueFactor = "0.4"
		_ = fixture.store.UpdateSupplier(context.Background(), supplier)
	}
	fixture.remote.mu.Unlock()
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "catalog-factor-race-apply", ConfirmEnable: true,
	})
	if err == nil || change.Status != "stale" || !strings.Contains(err.Error(), "effective source ratio changed") {
		t.Fatalf("preflight factor-race Apply = %#v, %v", change, err)
	}
	assertNoListingTargetWrites(t, fixture)
}

func TestValidateFrozenCatalogEvidenceUsesPreviewDeadline(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{Evidence: domain.Evidence{ObservedAt: now, ResponseHash: "same-catalog"}}
	expiredPlan := domain.ListingPlan{SourceObservedAt: now.Add(-catalogListingFreshnessWindow - time.Second)}
	if err := validateFrozenCatalogEvidence(expiredPlan, snapshot, now); err == nil || !strings.Contains(err.Error(), "preview source evidence expired") {
		t.Fatalf("newer identical recollection extended the preview deadline: %v", err)
	}
	freshPlan := domain.ListingPlan{SourceObservedAt: now.Add(-time.Minute)}
	if err := validateFrozenCatalogEvidence(freshPlan, snapshot, now); err != nil {
		t.Fatalf("newer identical recollection inside the preview deadline was rejected: %v", err)
	}
	olderSnapshot := domain.CatalogSnapshot{Evidence: domain.Evidence{ObservedAt: freshPlan.SourceObservedAt.Add(-time.Second), ResponseHash: "same-catalog"}}
	if err := validateFrozenCatalogEvidence(freshPlan, olderSnapshot, now); err == nil || !strings.Contains(err.Error(), "older than the listing preview") {
		t.Fatalf("older evidence was accepted: %v", err)
	}
}

func TestListingApplySuccessAndIdempotentReplay(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !plan.ProfitVerified || plan.EquivalentCostRatio != "0.25" || plan.ProposedGroupRatio != "0.300000000000" {
		t.Fatalf("plan calculation = %#v", plan)
	}
	replayedPlan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil || replayedPlan.ID != plan.ID {
		t.Fatalf("idempotent preview = %#v, %v", replayedPlan, err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-1", ConfirmEnable: true})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	if change.PricingPolicyID == nil || !change.PolicyCreated || !change.PolicyActivated {
		t.Fatalf("listing did not establish a continuing pricing policy: %#v", change)
	}
	policy, err := fixture.store.GetPolicy(context.Background(), *change.PricingPolicyID)
	if err != nil || !policy.Enabled || policy.SupplierID != fixture.request.SupplierID ||
		policy.SourceGroup != fixture.request.DiscoveryExternalID || policy.TargetGroup != fixture.request.TargetGroup {
		t.Fatalf("listing policy = %#v, %v", policy, err)
	}
	ratios, channels, creates, tests, _ := fixture.remote.snapshot()
	if ratios["promo-local"] != "0.3" || channels != 1 || creates != 1 || tests != 0 {
		t.Fatalf("remote after apply: ratios=%#v channels=%d creates=%d tests=%d", ratios, channels, creates, tests)
	}
	if events := fixture.remote.eventLog(); events != "channel:create,ratio:write,channel:enable" {
		t.Fatalf("unsafe listing operation order: %s", events)
	}
	replay, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-1", ConfirmEnable: true})
	if err != nil || replay.ID != change.ID {
		t.Fatalf("idempotent apply = %#v, %v", replay, err)
	}
	_, _, creates, tests, _ = fixture.remote.snapshot()
	if creates != 1 || tests != 0 {
		t.Fatalf("idempotent replay repeated side effects: creates=%d tests=%d", creates, tests)
	}
}

func TestListingAtomicContractApplyOrderReadbackAndPersistence(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.request.PricingRule = domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2",
		ModelPriceRoundingStep: "0.000000000001"}
	fixture.request.IdempotencyKey = "preview-atomic"
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if plan.GroupModelPricing.Mode != "atomic_contract" || !plan.GroupModelPricing.Supported || !plan.GroupModelPricing.Exact ||
		plan.ProposedGroupRatio != "1" || len(plan.GroupModelPricing.Models) != 1 {
		t.Fatalf("atomic plan = %#v", plan)
	}
	persisted, err := fixture.store.GetListingPlan(context.Background(), plan.ID)
	if err != nil || persisted.GroupModelPricing.DesiredRevision != plan.GroupModelPricing.DesiredRevision ||
		len(persisted.GroupModelPricing.Models) != 1 {
		t.Fatalf("persisted atomic plan = %#v, %v", persisted.GroupModelPricing, err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-atomic", ConfirmEnable: true})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled ||
		change.GroupModelPricing.AppliedRevision != plan.GroupModelPricing.DesiredRevision || !change.GroupModelPricing.Changed {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	policy, err := fixture.store.GetPolicy(context.Background(), *change.PricingPolicyID)
	if err != nil || policy.SourceBasis != "group_ratio" || !policy.Enabled {
		t.Fatalf("continuing policy = %#v, %v", policy, err)
	}
	contract, writes := fixture.remote.groupPricingSnapshot()
	setting := contract.Groups[fixture.request.TargetGroup]["m1"]
	if writes != 1 || setting.ModelRatio == nil || *setting.ModelRatio != "0.625" ||
		setting.CompletionRatio == nil || *setting.CompletionRatio != "2" {
		t.Fatalf("remote contract = %#v writes=%d", contract, writes)
	}
	if events := fixture.remote.eventLog(); events != "channel:create,ratio:write,contract:write,channel:enable" {
		t.Fatalf("unsafe atomic listing operation order: %s", events)
	}
	replay, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-atomic", ConfirmEnable: true})
	if err != nil || replay.ID != change.ID {
		t.Fatalf("atomic replay = %#v, %v", replay, err)
	}
	_, writes = fixture.remote.groupPricingSnapshot()
	if writes != 1 {
		t.Fatalf("idempotent replay repeated contract write: %d", writes)
	}
}

func completeMirrorListingRequest(fixture listingFixture, previewKey string) domain.ListingPlanRequest {
	yes := true
	no := false
	request := fixture.request
	request.IdempotencyKey = previewKey
	request.PricingRule = domain.ListingPricingRule{
		CreateGroupMirror: &yes, MirrorExecutionMode: "auto", CreatePricingMapping: &yes,
		RuleType: "multiply", Value: "1", ExecutionMode: "auto",
		PriceAnchor: "source_base", GroupRatioRuleType: "same", ModelPriceRuleType: "same",
		ModelPriceRoundingStep: "0.000000000001",
		SyncGroupRatio:         &no, SyncModelPrices: &yes, SyncBillingMode: &yes,
		SyncModelAdditions: &yes, SyncModelRemovals: &yes,
		ModelScopeMode: "mirror_group", EmptyGroupAction: "remove_managed_models",
	}
	return request
}

func TestListingContinuingRulesAreIndependentlyOptional(t *testing.T) {
	tests := []struct {
		name, suffix    string
		mirror, pricing bool
	}{
		{name: "both", suffix: "both", mirror: true, pricing: true},
		{name: "mirror only", suffix: "mirror", mirror: true, pricing: false},
		{name: "pricing only", suffix: "pricing", mirror: false, pricing: true},
		{name: "neither", suffix: "neither", mirror: false, pricing: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newListingFixture(t)
			fixture.remote.enableGroupModelPricing()
			setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
			request := completeMirrorListingRequest(fixture, "preview-optional-"+test.suffix)
			request.Channel.Models = []string{"m1", "m2"}
			request.PricingRule.CreateGroupMirror = &test.mirror
			request.PricingRule.CreatePricingMapping = &test.pricing

			plan, err := fixture.service.Preview(context.Background(), request)
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if got := plan.PricingRule.MirrorPolicyUUID != ""; got != test.mirror {
				t.Fatalf("mirror UUID present=%t want=%t", got, test.mirror)
			}
			change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
				IdempotencyKey: "apply-optional-" + test.suffix, ConfirmEnable: true,
			})
			if err != nil || change.Status != "applied" || !change.ChannelEnabled {
				t.Fatalf("Apply = %#v, %v", change, err)
			}
			if got := change.PricingPolicyID != nil; got != test.pricing {
				t.Fatalf("pricing policy present=%t want=%t", got, test.pricing)
			}
			if test.mirror {
				policy, getErr := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
				if getErr != nil || !policy.Enabled || policy.SyncGroupRatio {
					t.Fatalf("mirror policy = %#v, %v", policy, getErr)
				}
			}
			rolledBack, rollbackErr := fixture.service.Rollback(context.Background(), change.ID,
				RollbackInput{ConfirmRollback: true})
			if rollbackErr != nil || rolledBack.Status != "rolled_back" || rolledBack.ChannelEnabled {
				t.Fatalf("Rollback = %#v, %v", rolledBack, rollbackErr)
			}
		})
	}
}

func TestMirrorOnlyListingPreservesExistingPriceMapping(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	existing := domain.PricingPolicy{Name: "existing ratio owner", SupplierID: fixture.request.SupplierID,
		SourceGroup: "legacy-source", SourceModelPattern: "*", TargetSiteID: fixture.request.TargetSiteID,
		TargetGroup: fixture.request.TargetGroup, RuleType: "fixed", Value: "0.9", Enabled: true,
		ExecutionMode: "auto"}
	if err := fixture.store.CreatePolicy(context.Background(), &existing); err != nil {
		t.Fatal(err)
	}
	request := completeMirrorListingRequest(fixture, "preview-mirror-preserve-existing-price")
	request.Channel.Models = []string{"m1", "m2"}
	no := false
	request.PricingRule.CreatePricingMapping = &no
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("mirror-only preview: %v", err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-mirror-preserve-existing-price", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || change.PricingPolicyID != nil {
		t.Fatalf("mirror-only apply=%#v error=%v", change, err)
	}
	preserved, err := fixture.store.GetPolicy(context.Background(), existing.ID)
	if err != nil || !equivalentPolicyDefinition(preserved, existing) || !preserved.Enabled {
		t.Fatalf("existing price mapping changed: %#v error=%v", preserved, err)
	}
}

func TestListingAutomaticallyKeepsConflictingExistingPriceMapping(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	existing := domain.PricingPolicy{Name: "existing ratio owner", SupplierID: fixture.request.SupplierID,
		SourceGroup: "legacy-source", SourceModelPattern: "*", TargetSiteID: fixture.request.TargetSiteID,
		TargetGroup: fixture.request.TargetGroup, RuleType: "fixed", Value: "0.9", Enabled: true,
		ExecutionMode: "auto"}
	if err := fixture.store.CreatePolicy(context.Background(), &existing); err != nil {
		t.Fatal(err)
	}
	request := completeMirrorListingRequest(fixture, "preview-auto-preserve-existing-price")
	request.Channel.Models = []string{"m1", "m2"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil || listingUsesPricingMapping(plan.PricingRule) || len(plan.Calculation.Warnings) == 0 {
		t.Fatalf("auto-preserve preview=%#v error=%v", plan, err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-auto-preserve-existing-price", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || change.PricingPolicyID != nil {
		t.Fatalf("auto-preserve apply=%#v error=%v", change, err)
	}
	preserved, err := fixture.store.GetPolicy(context.Background(), existing.ID)
	if err != nil || !equivalentPolicyDefinition(preserved, existing) || !preserved.Enabled {
		t.Fatalf("existing price mapping changed: %#v error=%v", preserved, err)
	}
}

func setCompleteMirrorSourceModels(t *testing.T, fixture listingFixture, models []string) {
	t.Helper()
	models = sortedUnique(models)
	offers := make([]domain.ModelOffer, 0, len(models))
	for _, modelName := range models {
		offers = append(offers, domain.ModelOffer{ExternalID: modelName, Name: modelName, BillingMode: "ratio",
			PriceFunction: domain.PriceFunction{Currency: "USD", InputPer1M: "2", OutputPer1M: "4"}})
	}
	snapshot := domain.CatalogSnapshot{SupplierID: fixture.request.SupplierID,
		Groups: []domain.GroupOffer{{ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: models}},
		Models: offers, Evidence: domain.Evidence{ObservedAt: time.Now().UTC(), ResponseHash: "source-complete-mirror"}}
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	fixture.remote.setAvailableModels(models)
}

func setFixedPriceMirrorSource(t *testing.T, fixture listingFixture, price string) {
	t.Helper()
	snapshot := domain.CatalogSnapshot{SupplierID: fixture.request.SupplierID,
		Groups: []domain.GroupOffer{{ExternalID: "promo", Name: "promo", Ratio: "0.5", ModelIDs: []string{"m1"}}},
		Models: []domain.ModelOffer{{ExternalID: "m1", Name: "m1", Group: "promo", BillingMode: "fixed_price",
			ModelPrice: price, PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: price}}},
		Evidence: domain.Evidence{ObservedAt: time.Now().UTC(), ResponseHash: "source-fixed-price"}}
	if err := fixture.store.SaveSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	fixture.remote.setAvailableModels([]string{"m1"})
	fixture.remote.setPricingData([]map[string]any{{"model_name": "existing-model", "quota_type": 0,
		"model_ratio": 1, "completion_ratio": 1, "enable_groups": []string{"default"}}})
}

func TestListingAddsOnlyMissingGlobalModelPriceBeforeEnable(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.remote.failTestMissingPrice = true
	setFixedPriceMirrorSource(t, fixture, "2.5")
	request := completeMirrorListingRequest(fixture, "preview-fill-global-price")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	global := plan.GroupModelPricing.GlobalModelPrice
	if !global.Supported || !global.Changed || global.Added["m1"] != "2.5" || len(global.Expected) != 0 {
		t.Fatalf("global ModelPrice plan = %#v", global)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-fill-global-price", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	fixture.remote.mu.Lock()
	price := fixture.remote.modelPrices["m1"]
	fixture.remote.mu.Unlock()
	if price != "2.5" || change.GroupModelPricing.GlobalModelPrice.AppliedHash != global.DesiredHash {
		t.Fatalf("global ModelPrice readback=%q receipt=%#v", price, change.GroupModelPricing.GlobalModelPrice)
	}
	if events := fixture.remote.eventLog(); strings.Index(events, "contract:write") < 0 ||
		strings.Index(events, "model-price:write") < strings.Index(events, "contract:write") ||
		strings.Index(events, "channel:enable") < strings.Index(events, "model-price:write") ||
		strings.Contains(events, "channel:test") {
		t.Fatalf("global ModelPrice was not written after contract and before a no-test enable: %s", events)
	}
	rolledBack, rollbackErr := fixture.service.Rollback(context.Background(), change.ID,
		RollbackInput{ConfirmRollback: true})
	if rollbackErr != nil || rolledBack.Status != "rolled_back" {
		t.Fatalf("Rollback = %#v, %v", rolledBack, rollbackErr)
	}
	fixture.remote.mu.Lock()
	_, stillPresent := fixture.remote.modelPrices["m1"]
	fixture.remote.mu.Unlock()
	if stillPresent {
		t.Fatal("listing-added global ModelPrice survived rollback")
	}
}

func TestListingPreservesExistingGlobalModelPrice(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.remote.failTestMissingPrice = true
	fixture.remote.modelPrices["m1"] = "7"
	setFixedPriceMirrorSource(t, fixture, "2.5")
	request := completeMirrorListingRequest(fixture, "preview-preserve-global-price")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	global := plan.GroupModelPricing.GlobalModelPrice
	if global.Changed || len(global.Added) != 0 || global.Desired["m1"] != "7" {
		t.Fatalf("existing global ModelPrice was not preserved in plan: %#v", global)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-preserve-global-price", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	fixture.remote.mu.Lock()
	price := fixture.remote.modelPrices["m1"]
	fixture.remote.mu.Unlock()
	if price != "7" || strings.Contains(fixture.remote.eventLog(), "model-price:write") {
		t.Fatalf("existing global ModelPrice changed: price=%q events=%s", price, fixture.remote.eventLog())
	}
}

func TestListingCompleteMirrorClaimsOwnedStateAndReplaysIdempotently(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	request := completeMirrorListingRequest(fixture, "preview-complete-mirror")
	request.Channel.Models = []string{"m1", "m2"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if plan.PricingRule.MirrorPolicyUUID == "" || plan.GroupModelPricing.Mode != "atomic_contract" {
		t.Fatalf("complete mirror plan = %#v", plan)
	}
	marker := "relayops:policy/" + plan.PricingRule.MirrorPolicyUUID
	draftObserved := make(chan error, 1)
	fixture.remote.mu.Lock()
	fixture.remote.onManagedStateRead = func() {
		policy, getErr := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
		if getErr == nil && policy.Enabled {
			getErr = errors.New("native mirror policy was enabled before ownership readback")
		}
		draftObserved <- getErr
	}
	fixture.remote.mu.Unlock()

	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-complete-mirror", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled || change.CreatedChannelID == nil {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	visibleGroups, visibilityWrites := fixture.remote.visibilitySnapshot()
	if visibleGroups[plan.TargetGroup] != plan.TargetGroup || visibilityWrites != 1 ||
		change.GroupModelPricing.GroupVisibility.AppliedHash != plan.GroupModelPricing.GroupVisibility.DesiredHash {
		t.Fatalf("group visibility = %#v writes=%d receipt=%#v", visibleGroups, visibilityWrites,
			change.GroupModelPricing.GroupVisibility)
	}
	select {
	case observationErr := <-draftObserved:
		if observationErr != nil {
			t.Fatalf("disabled draft observation: %v", observationErr)
		}
	default:
		t.Fatal("managed-state readback did not observe the disabled native mirror draft")
	}

	policy, err := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
	if err != nil || !policy.Enabled || policy.PolicyVersion != 2 || policy.Kind != "complete_mirror" {
		t.Fatalf("enabled native mirror policy = %#v, %v", policy, err)
	}
	channelOwnerships, err := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), policy.PolicyUUID)
	if err != nil || len(channelOwnerships) != 1 {
		t.Fatalf("channel ownerships = %#v, %v", channelOwnerships, err)
	}
	channelOwnership := channelOwnerships[0]
	if channelOwnership.ChannelID != *change.CreatedChannelID || channelOwnership.TargetGroup != plan.TargetGroup ||
		channelOwnership.OwnershipMarker != marker || channelOwnership.ModelSetAuthority != "exclusive" ||
		channelOwnership.StatusAuthority != "disable_only" || channelOwnership.LastAppliedFunctionalHash == "" {
		t.Fatalf("channel ownership = %#v", channelOwnership)
	}
	modelOwnerships, err := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), policy.PolicyUUID)
	if err != nil || len(modelOwnerships) != len(plan.Channel.Models) {
		t.Fatalf("model ownerships = %#v, %v", modelOwnerships, err)
	}
	memberOwnerships, err := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), policy.PolicyUUID)
	if err != nil || len(memberOwnerships) != len(plan.Channel.Models) {
		t.Fatalf("channel-model ownerships = %#v, %v", memberOwnerships, err)
	}
	for index, modelName := range plan.Channel.Models {
		modelOwnership := modelOwnerships[index]
		memberOwnership := memberOwnerships[index]
		if modelOwnership.TargetModel != modelName || modelOwnership.OwnershipMarker != marker ||
			modelOwnership.LastAppliedFunctionalHash == "" || memberOwnership.TargetModel != modelName ||
			memberOwnership.ChannelID != *change.CreatedChannelID || memberOwnership.OwnershipMarker != marker ||
			memberOwnership.LastAppliedFunctionalHash == "" {
			t.Fatalf("ownership for %q: model=%#v membership=%#v", modelName, modelOwnership, memberOwnership)
		}
	}
	contract, _ := fixture.remote.groupPricingSnapshot()
	for _, modelName := range plan.Channel.Models {
		if setting := contract.Groups[plan.TargetGroup][modelName]; setting.Source != marker {
			t.Fatalf("contract source for %q = %q, want %q", modelName, setting.Source, marker)
		}
	}

	if err := fixture.service.ensureNativeMirrorOwnership(context.Background(), plan, &change); err != nil {
		t.Fatalf("ownership replay: %v", err)
	}
	replayedChannels, _ := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), policy.PolicyUUID)
	replayedModels, _ := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), policy.PolicyUUID)
	replayedMembers, _ := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), policy.PolicyUUID)
	if len(replayedChannels) != 1 || replayedChannels[0].ID != channelOwnership.ID ||
		len(replayedModels) != len(modelOwnerships) || replayedModels[0].ID != modelOwnerships[0].ID ||
		len(replayedMembers) != len(memberOwnerships) || replayedMembers[0].ID != memberOwnerships[0].ID {
		t.Fatalf("ownership replay duplicated rows: channel=%#v model=%#v member=%#v",
			replayedChannels, replayedModels, replayedMembers)
	}
	beforeEvents := fixture.remote.eventLog()
	beforeReads, beforeWrites := fixture.remote.managedStateSnapshot()
	replayedChange, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-complete-mirror", ConfirmEnable: true,
	})
	if err != nil || replayedChange.ID != change.ID {
		t.Fatalf("Apply replay = %#v, %v", replayedChange, err)
	}
	afterReads, afterWrites := fixture.remote.managedStateSnapshot()
	if fixture.remote.eventLog() != beforeEvents || afterReads != beforeReads || afterWrites != beforeWrites {
		t.Fatalf("Apply replay repeated remote effects: events=%q reads=%d->%d writes=%d->%d",
			fixture.remote.eventLog(), beforeReads, afterReads, beforeWrites, afterWrites)
	}
}

func TestListingCompleteMirrorIgnoresUnchangedHistoricalOptionExceptions(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.remote.mu.Lock()
	fixture.remote.usableGroups["legacy-empty-a"] = ""
	fixture.remote.usableGroups["legacy-empty-b"] = ""
	fixture.remote.ratios[""] = "2"
	fixture.remote.modelPrices[" legacy-model "] = "0.2"
	fixture.remote.mu.Unlock()
	setFixedPriceMirrorSource(t, fixture, "2.5")

	request := completeMirrorListingRequest(fixture, "preview-historical-option-exceptions")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview with historical exceptions: %v", err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-historical-option-exceptions", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled || change.CreatedChannelID == nil {
		t.Fatalf("Apply with historical exceptions = %#v, %v", change, err)
	}

	visibleGroups, _ := fixture.remote.visibilitySnapshot()
	ratios, channels, creates, tests, _ := fixture.remote.snapshot()
	fixture.remote.mu.Lock()
	modelPrices := cloneRatios(fixture.remote.modelPrices)
	fixture.remote.mu.Unlock()
	if visibleGroups["legacy-empty-a"] != "" || visibleGroups["legacy-empty-b"] != "" ||
		visibleGroups[plan.TargetGroup] != plan.TargetGroup {
		t.Fatalf("historical visibility was changed or new group was not added: %#v", visibleGroups)
	}
	if ratios[""] != "2" || modelPrices[" legacy-model "] != "0.2" || modelPrices["m1"] != "2.5" {
		t.Fatalf("historical pricing changed or missing model price was not added: ratios=%#v prices=%#v", ratios, modelPrices)
	}
	if channels != 1 || creates != 1 || tests != 0 || strings.Contains(fixture.remote.eventLog(), "channel:test") {
		t.Fatalf("listing effects channels=%d creates=%d tests=%d events=%q", channels, creates, tests, fixture.remote.eventLog())
	}

	rolledBack, rollbackErr := fixture.service.Rollback(context.Background(), change.ID,
		RollbackInput{ConfirmRollback: true})
	if rollbackErr != nil || rolledBack.Status != "rolled_back" {
		t.Fatalf("Rollback with historical exceptions = %#v, %v", rolledBack, rollbackErr)
	}
	visibleGroups, _ = fixture.remote.visibilitySnapshot()
	fixture.remote.mu.Lock()
	modelPrices = cloneRatios(fixture.remote.modelPrices)
	fixture.remote.mu.Unlock()
	if visibleGroups["legacy-empty-a"] != "" || visibleGroups["legacy-empty-b"] != "" {
		t.Fatalf("historical visibility was not preserved through rollback: %#v", visibleGroups)
	}
	if _, exists := visibleGroups[plan.TargetGroup]; exists {
		t.Fatalf("listing-owned group survived rollback: %#v", visibleGroups)
	}
	if modelPrices[" legacy-model "] != "0.2" {
		t.Fatalf("historical ModelPrice was not preserved through rollback: %#v", modelPrices)
	}
	if _, exists := modelPrices["m1"]; exists {
		t.Fatalf("listing-added ModelPrice survived rollback: %#v", modelPrices)
	}
}

func TestListingCompleteMirrorRollbackDisablesPolicyAndReleasesOwnedState(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	request := completeMirrorListingRequest(fixture, "preview-complete-mirror-rollback")
	request.Channel.Models = []string{"m1", "m2"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-complete-mirror-rollback", ConfirmEnable: true,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	rolledBack, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err != nil || rolledBack.Status != "rolled_back" || rolledBack.ChannelEnabled {
		t.Fatalf("Rollback = %#v, %v", rolledBack, err)
	}
	policy, err := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
	if err != nil || policy.Enabled || policy.PolicyVersion != 3 {
		t.Fatalf("disabled native mirror policy = %#v, %v", policy, err)
	}
	channels, channelErr := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), policy.PolicyUUID)
	models, modelErr := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), policy.PolicyUUID)
	members, memberErr := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), policy.PolicyUUID)
	if channelErr != nil || modelErr != nil || memberErr != nil || len(channels) != 0 || len(models) != 0 || len(members) != 0 {
		t.Fatalf("active ownership remains after rollback: channels=%#v models=%#v members=%#v errors=%v/%v/%v",
			channels, models, members, channelErr, modelErr, memberErr)
	}
	if _, channelCount, _, _, _ := fixture.remote.snapshot(); channelCount != 0 {
		t.Fatalf("managed channel remains after rollback: %d", channelCount)
	}
	contract, _ := fixture.remote.groupPricingSnapshot()
	if contract.Revision != plan.GroupModelPricing.ExpectedRevision {
		t.Fatalf("contract revision after rollback = %s, want %s", contract.Revision, plan.GroupModelPricing.ExpectedRevision)
	}
	visibleGroups, visibilityWrites := fixture.remote.visibilitySnapshot()
	if _, exists := visibleGroups[plan.TargetGroup]; exists || visibilityWrites != 2 ||
		rolledBack.GroupModelPricing.GroupVisibility.RollbackStatus == "" {
		t.Fatalf("visibility after rollback = %#v writes=%d receipt=%#v", visibleGroups, visibilityWrites,
			rolledBack.GroupModelPricing.GroupVisibility)
	}
}

func TestListingCompleteMirrorRollbackRejectsForeignOwnershipBeforeMutation(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	request := completeMirrorListingRequest(fixture, "preview-complete-mirror-rollback-conflict")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-complete-mirror-rollback-conflict", ConfirmEnable: true,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fixture.remote.mu.Lock()
	fixture.remote.foreignMarkerOnRead = true
	fixture.remote.mu.Unlock()
	beforeEvents := fixture.remote.eventLog()

	result, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err == nil || !errors.Is(err, ErrRollbackConflict) || result.Status != "applied" {
		t.Fatalf("conflicting Rollback = %#v, %v", result, err)
	}
	if fixture.remote.eventLog() != beforeEvents {
		t.Fatalf("ownership conflict produced remote writes: %q -> %q", beforeEvents, fixture.remote.eventLog())
	}
	policy, getErr := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
	if getErr != nil || !policy.Enabled || policy.PolicyVersion != 2 {
		t.Fatalf("ownership conflict changed native policy = %#v, %v", policy, getErr)
	}
}

func TestListingCompleteMirrorCompensatesForeignContractMarkerWithoutPartialOwnership(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	request := completeMirrorListingRequest(fixture, "preview-complete-mirror-foreign")
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	fixture.remote.mu.Lock()
	fixture.remote.foreignMarkerOnRead = true
	fixture.remote.mu.Unlock()
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-complete-mirror-foreign", ConfirmEnable: true,
	})
	if err == nil || !strings.Contains(err.Error(), "foreign ownership marker") || change.ChannelEnabled || change.CreatedChannelID == nil {
		t.Fatalf("foreign-marker Apply = %#v, %v", change, err)
	}
	policy, getErr := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
	if getErr != nil || policy.Enabled {
		t.Fatalf("foreign marker enabled native mirror policy = %#v, %v", policy, getErr)
	}
	channelOwnerships, listErr := fixture.store.ListActiveMirrorChannelOwnerships(context.Background(), policy.PolicyUUID)
	if listErr != nil || len(channelOwnerships) != 0 {
		t.Fatalf("foreign marker left partial channel ownership: %#v, %v", channelOwnerships, listErr)
	}
	modelOwnerships, listErr := fixture.store.ListActiveMirrorModelOwnerships(context.Background(), policy.PolicyUUID)
	if listErr != nil || len(modelOwnerships) != 0 {
		t.Fatalf("foreign marker claimed model ownership: %#v, %v", modelOwnerships, listErr)
	}
	memberOwnerships, listErr := fixture.store.ListActiveMirrorChannelModelOwnerships(context.Background(), policy.PolicyUUID)
	if listErr != nil || len(memberOwnerships) != 0 {
		t.Fatalf("foreign marker claimed channel-model ownership: %#v, %v", memberOwnerships, listErr)
	}
	if _, channelCount, _, _, _ := fixture.remote.snapshot(); channelCount != 0 {
		t.Fatalf("foreign marker left an unmanaged channel enabled or allocated: %d", channelCount)
	}
	contract, _ := fixture.remote.groupPricingSnapshot()
	if contract.Revision != plan.GroupModelPricing.ExpectedRevision {
		t.Fatalf("foreign marker compensation left contract revision %s, want %s",
			contract.Revision, plan.GroupModelPricing.ExpectedRevision)
	}
}

func TestListingAtomicContractFailureCompensatesOnlyOwnedState(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.request.PricingRule = domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2"}
	fixture.request.IdempotencyKey = "preview-atomic-failure"
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.failGroupPricingWrite = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-atomic-failure", ConfirmEnable: true})
	if err == nil || change.Status != "failed" || change.ChannelEnabled {
		t.Fatalf("failed apply = %#v, %v", change, err)
	}
	ratios, channels, _, _, ratioWrites := fixture.remote.snapshot()
	contract, contractWrites := fixture.remote.groupPricingSnapshot()
	if channels != 0 || ratios[fixture.request.TargetGroup] != "" || ratioWrites != 2 || contractWrites != 0 ||
		contract.Revision != plan.GroupModelPricing.ExpectedRevision ||
		!strings.Contains(change.RollbackStatus, "group-model pricing already at the preview revision") {
		t.Fatalf("compensation = %#v ratios=%#v channels=%d ratioWrites=%d contract=%#v contractWrites=%d",
			change, ratios, channels, ratioWrites, contract, contractWrites)
	}
}

func TestListingAtomicContractExternalChangeIsNeverOverwritten(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.request.PricingRule = domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2"}
	fixture.request.IdempotencyKey = "preview-atomic-external"
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.externalContractAfterWrite = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-atomic-external", ConfirmEnable: true})
	if err == nil || change.Status != "failed" {
		t.Fatalf("external conflict = %#v, %v", change, err)
	}
	contract, writes := fixture.remote.groupPricingSnapshot()
	if writes != 2 || contract.Groups["external"]["changed"].ModelPrice == nil ||
		!strings.Contains(change.RollbackStatus, "target group conditionally rolled back") {
		t.Fatalf("external contract was not preserved: contract=%#v writes=%d change=%#v", contract, writes, change)
	}
}

func TestListingRejectsBusinessFailureAndRollsBack(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.failCreateBusiness = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-business-fail", ConfirmEnable: true})
	if err == nil || !strings.Contains(err.Error(), "business rejected create") {
		t.Fatalf("Apply error = %v", err)
	}
	ratios, channels, _, _, writes := fixture.remote.snapshot()
	if change.Status != "failed" || channels != 0 || ratios["promo-local"] != "" || writes != 0 ||
		change.PricingPolicyID != nil {
		t.Fatalf("compensation = %#v ratios=%#v channels=%d writes=%d", change, ratios, channels, writes)
	}
	if policies, listErr := fixture.store.ListPolicies(context.Background()); listErr != nil || len(policies) != 0 {
		t.Fatalf("failed listing left pricing policies: %#v, %v", policies, listErr)
	}
}

func TestListingStopsOnPreviewCASConflict(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.setRatio("external", "2")
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-stale", ConfirmEnable: true})
	if err == nil || change.Status != "stale" {
		t.Fatalf("CAS Apply = %#v, %v", change, err)
	}
	_, channels, creates, _, writes := fixture.remote.snapshot()
	if channels != 0 || creates != 0 || writes != 0 {
		t.Fatalf("CAS conflict mutated target: channels=%d creates=%d writes=%d", channels, creates, writes)
	}
}

func TestListingDoesNotIssueBillableModelTest(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.failTest = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-test-fail", ConfirmEnable: true})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("no-test listing=%#v error=%v", change, err)
	}
	ratios, channels, _, tests, writes := fixture.remote.snapshot()
	if channels != 1 || ratios["promo-local"] != "0.3" || writes != 1 || tests != 0 {
		t.Fatalf("billable test was issued or listing did not apply: change=%#v ratios=%#v channels=%d writes=%d tests=%d",
			change, ratios, channels, writes, tests)
	}
}

func TestListingPartiallyAvailableModelsAreFilteredAndPersistedAsMirrorExceptions(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.remote.useOpaqueGroupPricingRevisions()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2", "m3"})
	fixture.remote.setAvailableModels([]string{"m1", "m2"})
	request := completeMirrorListingRequest(fixture, "preview-partial-models")
	request.Channel.Models = []string{"m1", "m2", "m3"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-partial-models", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("partial listing=%#v error=%v", change, err)
	}
	if calls := fixture.remote.modelDiscoveryCalls(); calls != 1 {
		t.Fatalf("model discovery calls=%d want=1", calls)
	}
	if got := fixture.remote.channelModels(); strings.Join(got, ",") != "m1,m2" {
		t.Fatalf("channel models=%v want=[m1 m2]", got)
	}
	if strings.Join(change.GroupModelPricing.RequestedModels, ",") != "m1,m2,m3" ||
		strings.Join(change.GroupModelPricing.EffectiveModels, ",") != "m1,m2" ||
		strings.Join(change.GroupModelPricing.ExcludedModels, ",") != "m3" ||
		change.GroupModelPricing.DiscoveryStatus != "confirmed" {
		t.Fatalf("model-resolution receipt=%#v", change.GroupModelPricing)
	}
	contract, _ := fixture.remote.groupPricingSnapshot()
	if _, exists := contract.Groups[request.TargetGroup]["m3"]; exists || len(contract.Groups[request.TargetGroup]) != 2 {
		t.Fatalf("filtered pricing contract=%#v", contract.Groups[request.TargetGroup])
	}
	if !strings.HasPrefix(change.GroupModelPricing.AppliedRevision, "target-authority-") ||
		change.GroupModelPricing.AppliedRevision != change.GroupModelPricing.DesiredRevision ||
		change.GroupModelPricing.AppliedRevision == targets.HashGroupModelPricingContract(change.GroupModelPricing.After) {
		t.Fatalf("target-authoritative revision receipt=%#v", change.GroupModelPricing)
	}
	policy, getErr := fixture.store.GetMirrorPolicy(context.Background(), plan.PricingRule.MirrorPolicyUUID)
	if getErr != nil || strings.Join(policy.ExcludedModels, ",") != "m3" || !policy.Enabled {
		t.Fatalf("mirror exception policy=%#v error=%v", policy, getErr)
	}
	_, channels, _, tests, _ := fixture.remote.snapshot()
	if channels != 1 || tests != 0 {
		t.Fatalf("listing result channels=%d billable_tests=%d", channels, tests)
	}
	rolledBack, rollbackErr := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if rollbackErr != nil || rolledBack.Status != "rolled_back" || rolledBack.ChannelEnabled {
		t.Fatalf("partial listing rollback=%#v error=%v", rolledBack, rollbackErr)
	}
	if _, channelCount, _, _, _ := fixture.remote.snapshot(); channelCount != 0 {
		t.Fatalf("partial listing rollback left %d channels", channelCount)
	}
}

func TestListingRejectsOutOfScopeAuthoritativePreviewBeforePricingWrite(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2", "m3"})
	fixture.remote.setAvailableModels([]string{"m1", "m2"})
	request := completeMirrorListingRequest(fixture, "preview-authority-scope")
	request.Channel.Models = []string{"m1", "m2", "m3"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.alterNextGroupPricingPreview()
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-authority-scope", ConfirmEnable: true,
	})
	if err == nil || !strings.Contains(err.Error(), "changed content outside the filtered listing") {
		t.Fatalf("out-of-scope authoritative preview change=%#v error=%v", change, err)
	}
	_, channels, _, tests, _ := fixture.remote.snapshot()
	_, contractWrites := fixture.remote.groupPricingSnapshot()
	if channels != 0 || tests != 0 || contractWrites != 0 {
		t.Fatalf("out-of-scope preview residue channels=%d tests=%d contract_writes=%d", channels, tests, contractWrites)
	}
}

func TestListingResumesAfterModelResolutionReceiptBeforeChannelFiltering(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	fixture.remote.useOpaqueGroupPricingRevisions()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2", "m3"})
	request := completeMirrorListingRequest(fixture, "preview-resolution-resume")
	request.Channel.Models = []string{"m1", "m2", "m3"}
	ctx := context.Background()
	plan, err := fixture.service.Preview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	marker := ownershipMarker(plan.ID, "apply-resolution-resume")
	change := domain.ListingChangeSet{PlanID: plan.ID, TargetSiteID: plan.TargetSiteID, Status: "pending", Step: "created",
		IdempotencyKey: "apply-resolution-resume", OwnershipMarker: marker, ExpectedTargetHash: plan.ExpectedTargetHash,
		GroupModelPricing: domain.ListingGroupModelPricingReceipt{
			Mode: plan.GroupModelPricing.Mode, ExpectedRevision: plan.GroupModelPricing.ExpectedRevision,
			DesiredRevision: plan.GroupModelPricing.DesiredRevision, Before: plan.GroupModelPricing.ExpectedContract,
			After: plan.GroupModelPricing.DesiredContract, Diff: append([]domain.TargetFieldDiff(nil), plan.GroupModelPricing.Diff...),
			GroupVisibility: domain.ListingGroupVisibilityReceipt{
				ExpectedHash: plan.GroupModelPricing.GroupVisibility.ExpectedHash,
				DesiredHash:  plan.GroupModelPricing.GroupVisibility.DesiredHash,
				Before:       cloneRatios(plan.GroupModelPricing.GroupVisibility.Expected),
				After:        cloneRatios(plan.GroupModelPricing.GroupVisibility.Desired),
				Changed:      plan.GroupModelPricing.GroupVisibility.Changed,
			},
			GlobalModelPrice: domain.ListingGlobalModelPriceReceipt{
				ExpectedHash: plan.GroupModelPricing.GlobalModelPrice.ExpectedHash,
				DesiredHash:  plan.GroupModelPricing.GlobalModelPrice.DesiredHash,
				Before:       cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Expected),
				After:        cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Desired),
				Added:        cloneRatios(plan.GroupModelPricing.GlobalModelPrice.Added),
				Changed:      plan.GroupModelPricing.GlobalModelPrice.Changed,
			},
			RequestedModels: append([]string(nil), plan.Channel.Models...),
		},
	}
	if _, err := fixture.store.CreateOrGetListingChangeSet(ctx, &change); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change.Status, change.StartedAt = "applying", &now
	if err := fixture.store.UpdateListingChangeSet(ctx, change, []string{"pending"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.TransitionListingPlan(ctx, plan.ID, []string{"ready"}, "applying", ""); err != nil {
		t.Fatal(err)
	}
	target, targetAdapter, channelAdapter, _, err := fixture.service.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := fixture.store.GetSupplier(ctx, plan.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	_, upstreamKey, err := fixture.service.loadUpstreamKey(ctx, supplier, plan.UpstreamCredentialID, plan.Channel.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := channelAdapter.CreateDisabledChannel(ctx, target, plan.Channel, plan.TargetGroup, upstreamKey, marker)
	if err != nil {
		t.Fatal(err)
	}
	change.CreatedChannelID = &channel.ID
	resolved, err := resolveListingPlan(plan, []string{"m1", "m2"}, []string{"m3"})
	if err != nil {
		t.Fatal(err)
	}
	groupAdapter, err := targets.AsGroupModelPricingAdapter(targetAdapter)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = bindAuthoritativeResolvedPricing(ctx, groupAdapter, target, resolved)
	if err != nil {
		t.Fatal(err)
	}
	recordListingModelResolution(&change, resolved, plan.Channel.Models, []string{"m1", "m2"}, []string{"m3"}, "confirmed")
	if err := fixture.store.UpdateListingChangeSet(ctx, change, []string{"applying"}); err != nil {
		t.Fatal(err)
	}

	resumed, err := fixture.service.Apply(ctx, plan.ID, ApplyInput{IdempotencyKey: "apply-resolution-resume", ConfirmEnable: true})
	if err != nil || resumed.Status != "applied" || !resumed.ChannelEnabled {
		t.Fatalf("resumed listing=%#v error=%v", resumed, err)
	}
	if strings.Join(fixture.remote.channelModels(), ",") != "m1,m2" {
		t.Fatalf("resumed channel models=%v", fixture.remote.channelModels())
	}
	_, managedWrites := fixture.remote.managedStateSnapshot()
	if managedWrites != 1 || fixture.remote.modelDiscoveryCalls() != 0 {
		t.Fatalf("resume managed_writes=%d rediscovery_calls=%d", managedWrites, fixture.remote.modelDiscoveryCalls())
	}
}

func TestListingSuccessfulEmptyDiscoveryStopsBeforePricingAndLeavesNoResidue(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	fixture.remote.setAvailableModels([]string{})
	request := completeMirrorListingRequest(fixture, "preview-empty-discovery")
	request.Channel.Models = []string{"m1", "m2"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-empty-discovery", ConfirmEnable: true,
	})
	if err == nil || change.Status != "failed" || change.ChannelEnabled {
		t.Fatalf("empty discovery change=%#v error=%v", change, err)
	}
	_, channels, _, tests, writes := fixture.remote.snapshot()
	_, contractWrites := fixture.remote.groupPricingSnapshot()
	if channels != 0 || tests != 0 || writes != 0 || contractWrites != 0 {
		t.Fatalf("empty discovery residue channels=%d tests=%d option_writes=%d contract_writes=%d",
			channels, tests, writes, contractWrites)
	}
	if policies, listErr := fixture.store.ListPolicies(context.Background()); listErr != nil || len(policies) != 0 {
		t.Fatalf("empty discovery pricing policies=%#v error=%v", policies, listErr)
	}
	if mirrors, listErr := fixture.store.ListMirrorPolicies(context.Background()); listErr != nil || len(mirrors) != 0 {
		t.Fatalf("empty discovery mirror policies=%#v error=%v", mirrors, listErr)
	}
}

func TestListingDiscoveryFailureFallsBackWithoutExclusionsOrBillableTests(t *testing.T) {
	fixture := newListingFixture(t)
	fixture.remote.enableGroupModelPricing()
	setCompleteMirrorSourceModels(t, fixture, []string{"m1", "m2"})
	fixture.remote.setFetchModelStatus(http.StatusServiceUnavailable)
	request := completeMirrorListingRequest(fixture, "preview-discovery-fallback")
	request.Channel.Models = []string{"m1", "m2"}
	plan, err := fixture.service.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-discovery-fallback", ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("discovery fallback=%#v error=%v", change, err)
	}
	if change.GroupModelPricing.DiscoveryStatus != "frozen_catalog_fallback" ||
		len(change.GroupModelPricing.ExcludedModels) != 0 ||
		strings.Join(change.GroupModelPricing.EffectiveModels, ",") != "m1,m2" {
		t.Fatalf("fallback receipt=%#v", change.GroupModelPricing)
	}
	_, _, _, tests, _ := fixture.remote.snapshot()
	if tests != 0 || strings.Join(fixture.remote.channelModels(), ",") != "m1,m2" {
		t.Fatalf("fallback channel=%v billable_tests=%d", fixture.remote.channelModels(), tests)
	}
}

func TestListingBillableTestHooksCannotMutateTarget(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.failTest = true
	fixture.remote.externalChangeOnTest = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-external-change", ConfirmEnable: true})
	if err != nil || change.Status != "applied" {
		t.Fatalf("no-test listing=%#v error=%v", change, err)
	}
	ratios, channels, _, tests, writes := fixture.remote.snapshot()
	if channels != 1 || ratios["external"] != "" || ratios["promo-local"] != "0.3" || writes != 1 || tests != 0 {
		t.Fatalf("removed billable test hook mutated target: ratios=%#v channels=%d writes=%d tests=%d", ratios, channels, writes, tests)
	}
}

func TestListingDoesNotUseBillableTestToEnableChannel(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.remote.failTest = true
	fixture.remote.externalEnableOnTest = true
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-external-enable", ConfirmEnable: true})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("no-test listing=%#v error=%v", change, err)
	}
	_, channels, _, tests, writes := fixture.remote.snapshot()
	if channels != 1 || writes != 1 || tests != 0 {
		t.Fatalf("channel enable depended on billable test: change=%#v channels=%d writes=%d tests=%d", change, channels, writes, tests)
	}
}

func TestRecoverIncompleteDiscoversRemotelyEnabledOwnedChannel(t *testing.T) {
	fixture := newListingFixture(t)
	ctx := context.Background()
	plan, err := fixture.service.Preview(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	marker := ownershipMarker(plan.ID, "recover-after-enable")
	change := domain.ListingChangeSet{PlanID: plan.ID, TargetSiteID: plan.TargetSiteID, Status: "pending", Step: "created",
		IdempotencyKey: "recover-after-enable", OwnershipMarker: marker, ExpectedTargetHash: plan.ExpectedTargetHash}
	if _, err := fixture.store.CreateOrGetListingChangeSet(ctx, &change); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	change.Status = "applying"
	change.StartedAt = &now
	if err := fixture.store.UpdateListingChangeSet(ctx, change, []string{"pending"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.TransitionListingPlan(ctx, plan.ID, []string{"ready"}, "applying", ""); err != nil {
		t.Fatal(err)
	}
	policy, _, _, _, err := fixture.store.BindListingPolicy(ctx, change.ID, listingPolicyForPlan(plan))
	if err != nil {
		t.Fatal(err)
	}
	target, adapter, channelAdapter, _, err := fixture.service.loadTarget(ctx, plan.TargetSiteID)
	if err != nil {
		t.Fatal(err)
	}
	supplier, err := fixture.store.GetSupplier(ctx, plan.SupplierID)
	if err != nil {
		t.Fatal(err)
	}
	_, upstreamKey, err := fixture.service.loadUpstreamKey(ctx, supplier, plan.UpstreamCredentialID, plan.Channel.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := channelAdapter.CreateDisabledChannel(ctx, target, plan.Channel, plan.TargetGroup, upstreamKey, marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := channelAdapter.TestChannel(ctx, target, channel.ID, "m1"); err != nil {
		t.Fatal(err)
	}
	desired := cloneRatios(plan.ExpectedGroupRatios)
	desired[plan.TargetGroup] = plan.ProposedGroupRatio
	if _, err := adapter.ApplyGroupRatio(ctx, target, plan.ExpectedTargetHash, desired); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.ActivateListingPolicy(ctx, change.ID, policy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := channelAdapter.EnableOwnedChannel(ctx, target, channel.ID, marker); err != nil {
		t.Fatal(err)
	}

	if err := fixture.service.RecoverIncomplete(ctx); err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}
	recovered, err := fixture.store.GetListingChangeSet(ctx, change.ID)
	if err != nil || recovered.Status != "applied" || !recovered.ChannelEnabled || recovered.CreatedChannelID == nil ||
		recovered.PricingPolicyID == nil || !recovered.PolicyActivated {
		t.Fatalf("recovered change = %#v, %v", recovered, err)
	}
	_, channels, creates, tests, writes := fixture.remote.snapshot()
	if channels != 1 || creates != 1 || tests != 1 || writes != 1 {
		t.Fatalf("recovery repeated remote effects: channels=%d creates=%d tests=%d writes=%d", channels, creates, tests, writes)
	}
}

func writeFake(response http.ResponseWriter, value any) {
	_ = json.NewEncoder(response).Encode(value)
}

func channelIDFromPath(path string) int64 {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for index := len(parts) - 1; index >= 0; index-- {
		if value, err := strconv.ParseInt(parts[index], 10, 64); err == nil {
			return value
		}
	}
	return 0
}

func valueOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func fakeChannelJSON(channel domain.TargetChannel) map[string]any {
	return map[string]any{
		"id": channel.ID, "name": channel.Name, "type": channel.Type, "status": channel.Status,
		"base_url": channel.BaseURL, "models": strings.Join(channel.Models, ","), "group": channel.Group,
		"priority": channel.Priority, "weight": channel.Weight, "auto_ban": boolIntForTest(channel.AutoBan),
		"model_mapping": channel.ModelMapping, "tag": channel.Tag, "remark": channel.Remark,
	}
}

func fakeManagedChannelState(channel fakeChannel) domain.TargetManagedChannelState {
	models := sortedUnique(channel.Models)
	return domain.TargetManagedChannelState{
		ChannelID: channel.ID,
		Models:    models,
		Status:    channel.Status,
		Hash: strconv.FormatInt(channel.ID, 10) + ":" + strconv.Itoa(channel.Status) + ":" +
			strings.Join(models, "\x00"),
	}
}

func boolIntForTest(value bool) int {
	if value {
		return 1
	}
	return 0
}

func applyAtomicListingForRollback(t *testing.T, fixture listingFixture, suffix string) (domain.ListingPlan, domain.ListingChangeSet) {
	t.Helper()
	fixture.remote.enableGroupModelPricing()
	fixture.request.PricingRule = domain.ListingPricingRule{RuleType: "target_margin", Value: "0.2",
		ModelPriceRoundingStep: "0.000000000001"}
	fixture.request.IdempotencyKey = "preview-rollback-" + suffix
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{
		IdempotencyKey: "apply-rollback-" + suffix, ConfirmEnable: true,
	})
	if err != nil || change.Status != "applied" || !change.ChannelEnabled {
		t.Fatalf("Apply = %#v, %v", change, err)
	}
	return plan, change
}

func TestListingRollbackRestoresOwnedStateInSafeOrderAndIsIdempotent(t *testing.T) {
	fixture := newListingFixture(t)
	plan, change := applyAtomicListingForRollback(t, fixture, "success")
	beforeEvents := fixture.remote.eventLog()
	if _, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{}); err == nil ||
		!strings.Contains(err.Error(), "confirm_rollback=true") {
		t.Fatalf("missing confirmation error = %v", err)
	}
	if fixture.remote.eventLog() != beforeEvents {
		t.Fatal("unconfirmed rollback changed the target")
	}

	rolledBack, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err != nil || rolledBack.Status != "rolled_back" || rolledBack.Step != "rolled_back" || rolledBack.ChannelEnabled {
		t.Fatalf("Rollback = %#v, %v", rolledBack, err)
	}
	if !strings.Contains(rolledBack.RollbackStatus, "owned channel #") ||
		!strings.Contains(rolledBack.RollbackStatus, "policy #") ||
		!strings.Contains(rolledBack.RollbackStatus, "rollback verified and complete") {
		t.Fatalf("rollback receipt = %q", rolledBack.RollbackStatus)
	}
	ratios, channels, creates, tests, ratioWrites := fixture.remote.snapshot()
	contract, contractWrites := fixture.remote.groupPricingSnapshot()
	if channels != 0 || creates != 1 || tests != 0 || ratioWrites != 2 || contractWrites != 2 ||
		ratios[fixture.request.TargetGroup] != "" || contract.Revision != plan.GroupModelPricing.ExpectedRevision {
		t.Fatalf("restored target: ratios=%#v channels=%d creates=%d tests=%d ratioWrites=%d contract=%#v contractWrites=%d",
			ratios, channels, creates, tests, ratioWrites, contract, contractWrites)
	}
	wantEvents := "channel:create,ratio:write,contract:write,channel:enable," +
		"channel:disable,contract:write,ratio:write,channel:delete"
	if events := fixture.remote.eventLog(); events != wantEvents {
		t.Fatalf("unsafe rollback order: %s", events)
	}
	if policies, listErr := fixture.store.ListPolicies(context.Background()); listErr != nil || len(policies) != 0 {
		t.Fatalf("rollback left its created policy: %#v, %v", policies, listErr)
	}
	storedPlan, err := fixture.store.GetListingPlan(context.Background(), plan.ID)
	if err != nil || storedPlan.Status != "rolled_back" {
		t.Fatalf("plan after rollback = %#v, %v", storedPlan, err)
	}

	replayed, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err != nil || replayed.Status != "rolled_back" || replayed.ID != rolledBack.ID {
		t.Fatalf("idempotent replay = %#v, %v", replayed, err)
	}
	if fixture.remote.eventLog() != wantEvents {
		t.Fatal("idempotent rollback replay repeated a target write")
	}
}

func TestListingRollbackKeepsExactLegacyTargetCompatible(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil || plan.GroupModelPricing.Mode != "legacy_group_ratio" {
		t.Fatalf("legacy Preview = %#v, %v", plan, err)
	}
	change, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "apply-legacy-rollback", ConfirmEnable: true})
	if err != nil || change.Status != "applied" {
		t.Fatalf("legacy Apply = %#v, %v", change, err)
	}
	rolledBack, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err != nil || rolledBack.Status != "rolled_back" {
		t.Fatalf("legacy Rollback = %#v, %v", rolledBack, err)
	}
	ratios, channels, _, _, writes := fixture.remote.snapshot()
	if channels != 0 || writes != 2 || ratios[fixture.request.TargetGroup] != "" {
		t.Fatalf("legacy target was not restored: ratios=%#v channels=%d writes=%d", ratios, channels, writes)
	}
	if events := fixture.remote.eventLog(); events != "channel:create,ratio:write,channel:enable,channel:disable,ratio:write,channel:delete" {
		t.Fatalf("legacy rollback order = %s", events)
	}
}

func TestListingRollbackPreflightConflictsPerformNoRemoteWrites(t *testing.T) {
	t.Run("group model contract", func(t *testing.T) {
		fixture := newListingFixture(t)
		_, change := applyAtomicListingForRollback(t, fixture, "contract-conflict")
		fixture.remote.mu.Lock()
		price := "99"
		fixture.remote.groupPricing.Groups["external"] = map[string]domain.GroupModelPricingSetting{
			"changed": {BillingMode: "fixed_price", ModelPrice: &price},
		}
		fixture.remote.groupPricing.Revision = targets.HashGroupModelPricingContract(fixture.remote.groupPricing)
		fixture.remote.mu.Unlock()
		assertRollbackConflictHasNoRemoteWrite(t, fixture, change)
	})

	t.Run("GroupRatio", func(t *testing.T) {
		fixture := newListingFixture(t)
		_, change := applyAtomicListingForRollback(t, fixture, "ratio-conflict")
		fixture.remote.setRatio("external", "9")
		assertRollbackConflictHasNoRemoteWrite(t, fixture, change)
	})

	t.Run("owned channel", func(t *testing.T) {
		fixture := newListingFixture(t)
		_, change := applyAtomicListingForRollback(t, fixture, "channel-conflict")
		fixture.remote.mu.Lock()
		channel := fixture.remote.channels[*change.CreatedChannelID]
		channel.Remark = "external-owner"
		fixture.remote.channels[*change.CreatedChannelID] = channel
		fixture.remote.mu.Unlock()
		assertRollbackConflictHasNoRemoteWrite(t, fixture, change)
	})

	t.Run("continuing policy", func(t *testing.T) {
		fixture := newListingFixture(t)
		_, change := applyAtomicListingForRollback(t, fixture, "policy-conflict")
		policy, err := fixture.store.GetPolicy(context.Background(), *change.PricingPolicyID)
		if err != nil {
			t.Fatal(err)
		}
		policy.Value = "0.33"
		if err := fixture.store.UpdatePolicy(context.Background(), policy); err != nil {
			t.Fatal(err)
		}
		assertRollbackConflictHasNoRemoteWrite(t, fixture, change)
	})
}

func assertRollbackConflictHasNoRemoteWrite(t *testing.T, fixture listingFixture, change domain.ListingChangeSet) {
	t.Helper()
	beforeEvents := fixture.remote.eventLog()
	_, _, _, _, beforeRatioWrites := fixture.remote.snapshot()
	_, beforeContractWrites := fixture.remote.groupPricingSnapshot()
	result, err := fixture.service.Rollback(context.Background(), change.ID, RollbackInput{ConfirmRollback: true})
	if err == nil || !errors.Is(err, ErrRollbackConflict) || result.Status != "applied" {
		t.Fatalf("conflict result = %#v, %v", result, err)
	}
	_, _, _, _, afterRatioWrites := fixture.remote.snapshot()
	_, afterContractWrites := fixture.remote.groupPricingSnapshot()
	if fixture.remote.eventLog() != beforeEvents || afterRatioWrites != beforeRatioWrites || afterContractWrites != beforeContractWrites {
		t.Fatalf("preflight conflict wrote target state: events=%q ratio=%d->%d contract=%d->%d",
			fixture.remote.eventLog(), beforeRatioWrites, afterRatioWrites, beforeContractWrites, afterContractWrites)
	}
}

func TestRecoverIncompleteResumesRollbackAfterRemoteDisableBeforeCheckpoint(t *testing.T) {
	fixture := newListingFixture(t)
	plan, change := applyAtomicListingForRollback(t, fixture, "recover")
	target, _, channelAdapter, _, err := fixture.service.loadTarget(context.Background(), plan.TargetSiteID)
	if err != nil {
		t.Fatal(err)
	}
	change.Status = "rolling_back"
	change.Step = "rollback_started"
	change.RollbackStatus = "preflight verified; rollback started"
	if err := fixture.store.UpdateListingChangeSet(context.Background(), change, []string{"applied"}); err != nil {
		t.Fatal(err)
	}
	if _, err := channelAdapter.DisableOwnedChannel(context.Background(), target, *change.CreatedChannelID, change.OwnershipMarker); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.RecoverIncomplete(context.Background()); err != nil {
		t.Fatalf("RecoverIncomplete: %v", err)
	}
	recovered, err := fixture.store.GetListingChangeSet(context.Background(), change.ID)
	if err != nil || recovered.Status != "rolled_back" || recovered.ChannelEnabled {
		t.Fatalf("recovered rollback = %#v, %v", recovered, err)
	}
	if strings.Count(fixture.remote.eventLog(), "channel:disable") != 1 {
		t.Fatalf("recovery repeated channel disable: %s", fixture.remote.eventLog())
	}
}

func TestListingUnknownTargetModelPriceCannotBecomeZeroCost(t *testing.T) {
	_, err := pricing.ResolvePricingImpact(
		domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{InputPer1M: "1"}}}},
		domain.CatalogSnapshot{Models: []domain.ModelOffer{{ExternalID: "m", PriceFunction: domain.PriceFunction{RequiresModelBasePrice: true}}}},
		domain.GroupOffer{Name: "g", Ratio: "0.1", ModelIDs: []string{"m"}}, []string{"m"},
	)
	if err == nil || !strings.Contains(err.Error(), "currencies") {
		t.Fatalf("unknown target price error = %v", err)
	}
}
