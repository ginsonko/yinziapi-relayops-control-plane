// relayops-newapi-fixture is a loopback-only NewAPI contract fixture used for
// local browser acceptance. It must never be exposed as a real API service.
package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fixtureToken    = "fixture-management-token"
	fixtureUserID   = 17
	fixtureUsername = "relayops-root"
	fixturePassword = "relayops-local-only"
)

type fixture struct {
	mu        sync.RWMutex
	options   map[string]map[string]string
	channel   map[string]any
	listing   bool
	startedAt int64
	runID     string
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18192", "loopback listen address")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal(err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		log.Fatal("the acceptance fixture may only listen on a loopback IP")
	}

	site := newFixture()
	server := &http.Server{
		Addr:              *listen,
		Handler:           site,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("RelayOps NewAPI fixture listening at http://%s", *listen)
	log.Printf("token=%s user_id=%d username=%s password=%s", fixtureToken, fixtureUserID, fixtureUsername, fixturePassword)
	log.Fatal(server.ListenAndServe())
}

func newFixture() *fixture {
	baseURL := "https://upstream.fixture.invalid"
	modelMapping := `{"fixture-alias":"gpt-test"}`
	remark := "relayops-local-acceptance"
	tag := "fixture"
	return &fixture{
		options: map[string]map[string]string{
			"GroupRatio":           {"default": "1", "economy": "0.05", "stable": "0.12", "inactive": "0.2"},
			"ModelRatio":           {"gpt-test": "1", "gpt-image-test": "2"},
			"ModelPrice":           {"gpt-per-call-test": "0.02"},
			"CompletionRatio":      {"gpt-test": "4"},
			"CacheRatio":           {"gpt-test": "0.1"},
			"CreateCacheRatio":     {"gpt-test": "1.25"},
			"ImageRatio":           {"gpt-image-test": "1.5"},
			"AudioRatio":           {"gpt-audio-test": "2"},
			"AudioCompletionRatio": {"gpt-audio-test": "3"},
		},
		channel: map[string]any{
			"id": int64(5), "name": "本地验收渠道", "type": 1, "status": 1,
			"base_url": baseURL, "models": "gpt-test,gpt-image-test", "group": "economy",
			"priority": int64(10), "weight": uint(20), "auto_ban": 1,
			"model_mapping": modelMapping, "remark": remark, "tag": tag,
		},
		startedAt: time.Now().UTC().Add(-5 * time.Minute).Unix(),
		runID:     newRunID(),
	}
}

func newRunID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return fmt.Sprintf("%x", value[:])
	}
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
}

func (site *fixture) eventID(prefix string) string {
	return prefix + "-" + site.runID
}

func (site *fixture) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.HasPrefix(request.URL.Path, "/__fixture__/") {
		site.fixtureControl(response, request)
		return
	}
	if request.URL.Path == "/v1/models" && request.Method == http.MethodGet {
		site.readUpstreamModels(response, request)
		return
	}
	if request.URL.Path == "/v1/chat/completions" && request.Method == http.MethodPost {
		site.completeUpstreamChat(response, request)
		return
	}
	if request.URL.Path == "/api/user/login" && request.Method == http.MethodPost {
		site.login(response, request)
		return
	}
	if request.URL.Path == "/api/status" && request.Method == http.MethodGet {
		writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
			"quota_per_unit": 500000, "quota_display_type": "CNY", "display_in_currency": true, "usd_exchange_rate": 1,
		}})
		return
	}
	if !authorized(request) {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"success": false, "message": "fixture credential rejected"})
		return
	}

	switch {
	case request.URL.Path == "/api/option/" && request.Method == http.MethodGet:
		site.readOptions(response)
	case request.URL.Path == "/api/option/" && request.Method == http.MethodPut:
		site.writeOption(response, request)
	case request.URL.Path == "/api/pricing" && request.Method == http.MethodGet:
		site.readPricing(response)
	case request.URL.Path == "/api/user/self" && request.Method == http.MethodGet:
		site.readCustomerSelf(response)
	case request.URL.Path == "/api/data/self" && request.Method == http.MethodGet:
		site.readCustomerUsage(response)
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodGet:
		site.readChannels(response)
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodPut:
		site.writeChannel(response, request)
	case request.URL.Path == "/api/channel/5" && request.Method == http.MethodGet:
		site.readChannel(response)
	case request.URL.Path == "/api/channel/5/status" && request.Method == http.MethodPost:
		site.writeChannelStatus(response, request)
	case request.URL.Path == "/api/channel/fetch_models/5" && request.Method == http.MethodGet:
		writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": []string{"gpt-test", "gpt-image-test"}})
	case request.URL.Path == "/api/channel/test/5" && request.Method == http.MethodGet:
		if model := strings.TrimSpace(request.URL.Query().Get("model")); model == "gpt-test" || model == "gpt-image-test" {
			writeJSON(response, http.StatusOK, map[string]any{"success": true, "time": 0.01})
		} else {
			writeJSON(response, http.StatusBadRequest, map[string]any{"success": false, "message": "unknown fixture model"})
		}
	case request.URL.Path == "/api/user/" && request.Method == http.MethodGet:
		site.readLedgerUsers(response)
	case request.URL.Path == "/api/log/" && request.Method == http.MethodGet:
		site.readLedgerLogs(response)
	case request.URL.Path == "/api/user/topup" && request.Method == http.MethodGet:
		site.readLedgerTopups(response)
	default:
		writeJSON(response, http.StatusNotFound, map[string]any{"success": false, "message": "fixture endpoint not found"})
	}
}

func (site *fixture) readCustomerSelf(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"id": fixtureUserID, "username": fixtureUsername, "role": 100, "quota": 30965000,
		"used_quota": 129500000, "request_count": 39502, "currency": "quota",
	}})
}

func (site *fixture) readCustomerUsage(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": []map[string]any{
		{"created_at": site.startedAt, "quota": 5000000, "count": 10},
		{"created_at": site.startedAt + 60, "quota": 5760000, "count": 11},
	}})
}

func (site *fixture) readLedgerUsers(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"items": []map[string]any{
			{"id": fixtureUserID, "username": fixtureUsername, "role": 100, "group": "administrator"},
			{"id": 21, "username": "fixture-user", "role": 1, "group": "standard"},
			{"id": 22, "username": "fixture-test", "role": 1, "group": "test", "is_test": true},
		}, "total": 3, "page": 0, "page_size": 100,
	}})
}

func (site *fixture) readLedgerLogs(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"items": []map[string]any{
			{"id": 101, "request_id": site.eventID("fixture-request-user"), "created_at": site.startedAt, "user_id": 21, "token_id": 7,
				"model_name": "gpt-test", "group": "standard", "channel": 5, "quota": 250000, "prompt_tokens": 1000, "completion_tokens": 200},
			{"id": 102, "request_id": site.eventID("fixture-request-admin"), "created_at": site.startedAt + 1, "user_id": fixtureUserID,
				"model_name": "gpt-test", "group": "administrator", "channel": 5, "quota": 100000},
			{"id": 103, "request_id": site.eventID("fixture-request-test"), "created_at": site.startedAt + 2, "user_id": 22,
				"model_name": "gpt-image-test", "group": "test", "channel": 5, "quota": 50000, "is_test": true},
		}, "total": 3, "page": 0, "page_size": 100,
	}})
}

func (site *fixture) readLedgerTopups(response http.ResponseWriter) {
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"items": []map[string]any{
			{"id": 201, "trade_no": site.eventID("fixture-trade-user"), "user_id": 21, "money": "25.50", "status": "success",
				"create_time": site.startedAt - 60, "complete_time": site.startedAt + 3},
			{"id": 202, "trade_no": site.eventID("fixture-trade-pending"), "user_id": 21, "money": "99.00", "status": "pending",
				"create_time": site.startedAt - 30},
		}, "total": 2, "page": 0, "page_size": 100,
	}})
}

func (site *fixture) login(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if json.NewDecoder(request.Body).Decode(&input) != nil || input.Username != fixtureUsername || input.Password != fixturePassword {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"success": false, "message": "invalid fixture login"})
		return
	}
	http.SetCookie(response, &http.Cookie{Name: "session", Value: "fixture-session", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{
		"id": fixtureUserID, "username": fixtureUsername, "role": 100, "require_2fa": false,
	}})
}

func authorized(request *http.Request) bool {
	if request.Header.Get("New-Api-User") != strconv.Itoa(fixtureUserID) {
		return false
	}
	if request.Header.Get("Authorization") == "Bearer "+fixtureToken {
		return true
	}
	cookie, err := request.Cookie("session")
	return err == nil && cookie.Value == "fixture-session"
}

func (site *fixture) readOptions(response http.ResponseWriter) {
	site.mu.RLock()
	defer site.mu.RUnlock()
	keys := []string{"GroupRatio", "ModelRatio", "ModelPrice", "CompletionRatio", "CacheRatio", "CreateCacheRatio", "ImageRatio", "AudioRatio", "AudioCompletionRatio"}
	data := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		encoded, _ := json.Marshal(site.options[key])
		data = append(data, map[string]string{"key": key, "value": string(encoded)})
	}
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": data})
}

func (site *fixture) writeOption(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if json.NewDecoder(request.Body).Decode(&input) != nil {
		writeJSON(response, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid option request"})
		return
	}
	var values map[string]json.Number
	if _, exists := site.options[input.Key]; !exists || json.Unmarshal([]byte(input.Value), &values) != nil || len(values) == 0 {
		writeJSON(response, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid fixture option"})
		return
	}
	next := make(map[string]string, len(values))
	for key, value := range values {
		next[key] = value.String()
	}
	site.mu.Lock()
	site.options[input.Key] = next
	site.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true})
}

func (site *fixture) readPricing(response http.ResponseWriter) {
	site.mu.RLock()
	groups := cloneMap(site.options["GroupRatio"])
	listing := site.listing
	site.mu.RUnlock()
	// NewAPI's effective pricing page only publishes groups that currently
	// expose models, while the raw GroupRatio option retains inactive groups.
	delete(groups, "inactive")
	models := []map[string]any{
		{"model_name": "gpt-test", "quota_type": 0, "model_ratio": 1, "completion_ratio": 4, "cache_ratio": 0.1, "enable_groups": []string{"default", "economy", "stable"}},
		{"model_name": "gpt-image-test", "quota_type": 0, "model_ratio": 2, "image_ratio": 1.5, "enable_groups": []string{"default", "economy", "stable"}},
	}
	if listing {
		models = append(models, listingFixtureModels()...)
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"success":                      true,
		"group_ratio":                  groups,
		"usable_group":                 map[string]string{"default": "默认", "economy": "经济", "stable": "稳定"},
		"pricing_version":              "fixture-v1",
		"group_model_pricing_revision": "fixture-r27-source-v1",
		"data":                         models,
	})
}

func (site *fixture) fixtureControl(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+fixtureToken {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"success": false, "message": "fixture control credential rejected"})
		return
	}
	if request.URL.Path == "/__fixture__/state" && request.Method == http.MethodGet {
		site.mu.RLock()
		listing := site.listing
		site.mu.RUnlock()
		writeJSON(response, http.StatusOK, map[string]any{"success": true, "listing": listing})
		return
	}
	if request.URL.Path != "/__fixture__/stage/listing" || request.Method != http.MethodPost {
		writeJSON(response, http.StatusNotFound, map[string]any{"success": false, "message": "fixture control endpoint not found"})
		return
	}
	site.mu.Lock()
	site.listing = true
	site.options["GroupRatio"]["r27-source"] = "0.4"
	site.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "listing": true})
}

func (site *fixture) readUpstreamModels(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+fixtureToken {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid fixture API key"}})
		return
	}
	models := make([]map[string]string, 0)
	for _, model := range listingFixtureModelNames() {
		models = append(models, map[string]string{"id": model, "object": "model", "owned_by": "relayops-fixture"})
	}
	writeJSON(response, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (site *fixture) completeUpstreamChat(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+fixtureToken {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid fixture API key"}})
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if json.NewDecoder(request.Body).Decode(&input) != nil || !containsFixtureModel(input.Model) {
		writeJSON(response, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "unknown fixture model"}})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"id": "chatcmpl-fixture", "object": "chat.completion", "created": time.Now().Unix(), "model": input.Model,
		"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func listingFixtureModels() []map[string]any {
	return []map[string]any{
		{"model_name": "fixture-token-r27", "quota_type": 0, "model_ratio": 99, "completion_ratio": 99, "enable_groups": []string{"r27-source"}, "group_pricing": map[string]any{"r27-source": map[string]any{
			"group": "r27-source", "scope": "group_model", "contract_revision": "fixture-r27-source-v1", "billing_mode": "ratio", "billing_unit": "per_million_tokens", "quota_type": 0,
			"model_ratio": 1.5, "completion_ratio": 4, "cache_ratio": 0.1, "base_group_ratio": 0.4, "effective_group_ratio": 0.4, "effective_model_ratio": 0.6}}},
		{"model_name": "fixture-request-r27", "quota_type": 1, "model_price": 9, "enable_groups": []string{"r27-source"}, "group_pricing": map[string]any{"r27-source": map[string]any{
			"group": "r27-source", "scope": "group_model", "contract_revision": "fixture-r27-source-v1", "billing_mode": "fixed_price", "billing_unit": "per_request", "quota_type": 1,
			"model_price": 0.4, "base_group_ratio": 0.4, "effective_group_ratio": 0.4, "effective_model_price": 0.16}}},
		{"model_name": "fixture-second-r27", "quota_type": 1, "model_price": 9, "enable_groups": []string{"r27-source"}, "group_pricing": map[string]any{"r27-source": map[string]any{
			"group": "r27-source", "scope": "group_model", "contract_revision": "fixture-r27-source-v1", "billing_mode": "fixed_price", "billing_unit": "per_second", "quota_type": 1,
			"model_price": 0.05, "base_group_ratio": 0.4, "effective_group_ratio": 0.4, "effective_model_price": 0.02}}},
		{"model_name": "fixture-fixed-r27", "quota_type": 1, "model_price": 9, "enable_groups": []string{"r27-source"}, "group_pricing": map[string]any{"r27-source": map[string]any{
			"group": "r27-source", "scope": "group_model", "contract_revision": "fixture-r27-source-v1", "billing_mode": "fixed_price", "billing_unit": "fixed_duration", "quota_type": 1,
			"model_price": 2, "fixed_duration_seconds": 8, "base_group_ratio": 0.4, "effective_group_ratio": 0.4, "effective_model_price": 0.8}}},
	}
}

func listingFixtureModelNames() []string {
	return []string{"fixture-token-r27", "fixture-request-r27", "fixture-second-r27", "fixture-fixed-r27"}
}

func containsFixtureModel(model string) bool {
	for _, candidate := range listingFixtureModelNames() {
		if model == candidate {
			return true
		}
	}
	return false
}

func (site *fixture) readChannels(response http.ResponseWriter) {
	site.mu.RLock()
	channel := cloneMapAny(site.channel)
	site.mu.RUnlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"items": []any{channel}, "total": 1}})
}

func (site *fixture) readChannel(response http.ResponseWriter) {
	site.mu.RLock()
	channel := cloneMapAny(site.channel)
	site.mu.RUnlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": channel})
}

func (site *fixture) writeChannel(response http.ResponseWriter, request *http.Request) {
	var input map[string]any
	decoder := json.NewDecoder(request.Body)
	decoder.UseNumber()
	if decoder.Decode(&input) != nil || numberString(input["id"]) != "5" {
		writeJSON(response, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid channel update"})
		return
	}
	site.mu.Lock()
	for _, key := range []string{"name", "type", "status", "base_url", "models", "group", "priority", "weight", "auto_ban", "model_mapping", "tag", "remark"} {
		if value, exists := input[key]; exists {
			site.channel[key] = value
		}
	}
	site.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": map[string]any{}})
}

func (site *fixture) writeChannelStatus(response http.ResponseWriter, request *http.Request) {
	var input struct {
		Status int `json:"status"`
	}
	if json.NewDecoder(request.Body).Decode(&input) != nil || (input.Status != 1 && input.Status != 2) {
		writeJSON(response, http.StatusBadRequest, map[string]any{"success": false, "message": "invalid channel status"})
		return
	}
	site.mu.Lock()
	site.channel["status"] = input.Status
	site.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"success": true, "data": true})
}

func numberString(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return fmt.Sprint(value)
	}
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneMapAny(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
