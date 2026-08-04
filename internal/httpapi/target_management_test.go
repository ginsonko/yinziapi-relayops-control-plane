package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type managedNewAPI struct {
	mu                    sync.Mutex
	ratios                map[string]json.Number
	testHits              int
	optionPuts            int
	acceptedAuthorization map[string]bool
}

func TestTargetReauthorizationValidatesBeforeAtomicSwapAndPreservesManualDisable(t *testing.T) {
	t.Parallel()
	upstream := &managedNewAPI{ratios: map[string]json.Number{"default": "1"},
		acceptedAuthorization: map[string]bool{"Bearer old-token": true, "Bearer new-token": true}}
	targetServer := httptest.NewServer(upstream)
	defer targetServer.Close()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "old", Kind: "newapi-management", SiteURL: targetServer.URL}
	if err := credentialService.Create(context.Background(), &credential,
		domain.CredentialPayload{Authorization: "Bearer old-token", UserID: 17}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: targetServer.URL, Adapter: "newapi", CredentialID: credential.ID,
		Enabled: false, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, Credentials: credentialService,
		Targets: targets.NewFactory(targetServer.Client(), credentialService), StaticDir: t.TempDir()})

	requestBody := func(token string) string {
		encoded, _ := json.Marshal(map[string]any{"auth_method": "token", "token": token, "user_id": 17,
			"expected_target_updated_at": target.UpdatedAt, "expected_credential_id": credential.ID,
			"expected_credential_updated_at": credential.UpdatedAt})
		return string(encoded)
	}
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/reauthorize", target.ID), strings.NewReader(requestBody("bad-token"))))
	if failed.Code != http.StatusUnauthorized {
		t.Fatalf("failed reauth status=%d body=%s", failed.Code, failed.Body.String())
	}
	unchanged, _ := st.GetTarget(context.Background(), target.ID)
	if unchanged.CredentialID != credential.ID || unchanged.Enabled {
		t.Fatalf("failed reauth changed target: %#v", unchanged)
	}

	succeeded := httptest.NewRecorder()
	handler.ServeHTTP(succeeded, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/reauthorize", target.ID), strings.NewReader(requestBody("new-token"))))
	if succeeded.Code != http.StatusOK {
		t.Fatalf("successful reauth status=%d body=%s", succeeded.Code, succeeded.Body.String())
	}
	updated, _ := st.GetTarget(context.Background(), target.ID)
	if updated.CredentialID == credential.ID || updated.Enabled {
		t.Fatalf("successful reauth did not swap atomically or enabled target: %#v", updated)
	}
}

func (site *managedNewAPI) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	authorization := request.Header.Get("Authorization")
	authorized := authorization == "Bearer root-token"
	if site.acceptedAuthorization != nil {
		authorized = site.acceptedAuthorization[authorization]
	}
	if !authorized || request.Header.Get("New-Api-User") != "17" {
		http.Error(response, "bad management headers", http.StatusUnauthorized)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	site.mu.Lock()
	defer site.mu.Unlock()
	switch {
	case request.URL.Path == "/api/option/" && request.Method == http.MethodGet:
		encoded, _ := json.Marshal(site.ratios)
		fmt.Fprintf(response, `{"success":true,"data":[{"key":"GroupRatio","value":%q}]}`, string(encoded))
	case request.URL.Path == "/api/option/" && request.Method == http.MethodPut:
		site.optionPuts++
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		var next map[string]json.Number
		if input.Key != "GroupRatio" || json.Unmarshal([]byte(input.Value), &next) != nil {
			http.Error(response, "bad ratio", http.StatusBadRequest)
			return
		}
		site.ratios = next
		fmt.Fprint(response, `{"success":true}`)
	case request.URL.Path == "/api/pricing":
		encoded, _ := json.Marshal(site.ratios)
		fmt.Fprintf(response, `{"success":true,"data":[{"model_name":"gpt-test","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["economy"]}],"group_ratio":%s}`, encoded)
	case request.URL.Path == "/api/channel/" && request.Method == http.MethodGet:
		fmt.Fprint(response, `{"success":true,"data":{"items":[{"id":5,"name":"upstream","type":1,"status":1,"base_url":"https://upstream.example","models":"gpt-test","group":"economy","priority":0,"weight":0,"auto_ban":1}],"total":1}}`)
	case request.URL.Path == "/api/channel/5" && request.Method == http.MethodGet:
		fmt.Fprint(response, `{"success":true,"data":{"id":5,"name":"upstream","type":1,"status":1,"base_url":"https://upstream.example","models":"gpt-test","group":"economy","priority":0,"weight":0,"auto_ban":1}}`)
	case request.URL.Path == "/api/channel/test/5" && request.Method == http.MethodGet:
		site.testHits++
		fmt.Fprint(response, `{"success":true,"data":{"time":0.2}}`)
	default:
		http.NotFound(response, request)
	}
}

func TestTargetManagementConnectInspectAndApply(t *testing.T) {
	t.Parallel()
	upstream := &managedNewAPI{ratios: map[string]json.Number{"default": "1", "economy": "0.05"}}
	targetServer := httptest.NewServer(upstream)
	defer targetServer.Close()

	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := secrets.Open(filepath.Join(directory, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	targetFactory := targets.NewFactory(targetServer.Client(), credentialService)
	handler := New(Dependencies{Store: st, Credentials: credentialService, Targets: targetFactory, StaticDir: t.TempDir()})

	connectBody := fmt.Sprintf(`{"name":"本站","base_url":%q,"adapter":"newapi","write_mode":"approval","auth_method":"token","token":"root-token","user_id":17}`, targetServer.URL)
	connect := httptest.NewRecorder()
	handler.ServeHTTP(connect, httptest.NewRequest(http.MethodPost, "/api/v1/targets/connect", strings.NewReader(connectBody)))
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect status=%d body=%s", connect.Code, connect.Body.String())
	}
	var connected struct {
		Data struct {
			Target struct {
				ID int64 `json:"id"`
			} `json:"target"`
			Snapshot struct {
				Hash string `json:"hash"`
			} `json:"snapshot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(connect.Body.Bytes(), &connected); err != nil || connected.Data.Target.ID == 0 || connected.Data.Snapshot.Hash == "" {
		t.Fatalf("connect response=%#v err=%v", connected, err)
	}
	id := connected.Data.Target.ID

	for _, path := range []string{
		fmt.Sprintf("/api/v1/targets/%d/catalog", id),
		fmt.Sprintf("/api/v1/targets/%d/channels", id),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "root-token") {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/pricing-options/GroupRatio/preview", id),
		strings.NewReader(`{"values":{"default":"1","economy":"0.08"}}`)))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewResponse struct {
		Data struct {
			ExpectedHash string `json:"expected_hash"`
			PreviewHash  string `json:"preview_hash"`
		} `json:"data"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewResponse); err != nil {
		t.Fatal(err)
	}
	applyBody := fmt.Sprintf(`{"expected_hash":%q,"preview_hash":%q,"values":{"default":"1","economy":"0.08"},"idempotency_key":"price-change-1","confirm":true}`,
		previewResponse.Data.ExpectedHash, previewResponse.Data.PreviewHash)
	apply := httptest.NewRecorder()
	handler.ServeHTTP(apply, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/pricing-options/GroupRatio/apply", id), strings.NewReader(applyBody)))
	if apply.Code != http.StatusOK || !strings.Contains(apply.Body.String(), `"result_state":"verified_success"`) {
		t.Fatalf("apply status=%d body=%s", apply.Code, apply.Body.String())
	}
	replayApply := httptest.NewRecorder()
	handler.ServeHTTP(replayApply, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/pricing-options/GroupRatio/apply", id), strings.NewReader(applyBody)))
	if replayApply.Code != http.StatusOK || !strings.Contains(replayApply.Body.String(), `"result_state":"verified_success"`) {
		t.Fatalf("apply replay status=%d body=%s", replayApply.Code, replayApply.Body.String())
	}
	recoveryInput := pricingOptionApplyInput{ExpectedHash: previewResponse.Data.ExpectedHash,
		PreviewHash: previewResponse.Data.PreviewHash, Values: map[string]string{"default": "1", "economy": "0.08"},
		IdempotencyKey: "crash-recovery-1", Confirm: true}
	recoveryHash := targetRequestHash("pricing_option_apply", id, "GroupRatio", recoveryInput)
	recoveryOperation := domain.TargetControlOperation{TargetSiteID: id, Dimension: "pricing_option:GroupRatio",
		Action: "pricing_option_apply", IdempotencyKey: recoveryInput.IdempotencyKey, RequestHash: recoveryHash,
		ExpectedHash: recoveryInput.ExpectedHash, DesiredHash: targets.HashDecimalMap(recoveryInput.Values),
		ResultState: "in_progress", StartedAt: time.Now().UTC().Add(-3 * time.Minute)}
	if err := st.CreateTargetControlOperation(context.Background(), &recoveryOperation); err != nil {
		t.Fatal(err)
	}
	recoveryBody, _ := json.Marshal(recoveryInput)
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/pricing-options/GroupRatio/apply", id), bytes.NewReader(recoveryBody)))
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), `"result_state":"verified_success"`) {
		t.Fatalf("crash recovery status=%d body=%s", recovered.Code, recovered.Body.String())
	}

	testChannel := httptest.NewRecorder()
	handler.ServeHTTP(testChannel, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/channels/5/test", id),
		bytes.NewBufferString(`{"model":"gpt-test","timeout_seconds":10,"acknowledge_cost":true,"idempotency_key":"channel-test-1"}`)))
	if testChannel.Code != http.StatusOK || !strings.Contains(testChannel.Body.String(), `"result_state":"verified_success"`) {
		t.Fatalf("channel test status=%d body=%s", testChannel.Code, testChannel.Body.String())
	}
	replayTest := httptest.NewRecorder()
	handler.ServeHTTP(replayTest, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/channels/5/test", id),
		bytes.NewBufferString(`{"model":"gpt-test","timeout_seconds":10,"acknowledge_cost":true,"idempotency_key":"channel-test-1"}`)))
	if replayTest.Code != http.StatusOK {
		t.Fatalf("test replay status=%d body=%s", replayTest.Code, replayTest.Body.String())
	}
	cooldownTest := httptest.NewRecorder()
	handler.ServeHTTP(cooldownTest, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/targets/%d/channels/5/test", id),
		bytes.NewBufferString(`{"model":"gpt-test","timeout_seconds":10,"acknowledge_cost":true,"idempotency_key":"channel-test-2"}`)))
	if cooldownTest.Code != http.StatusTooManyRequests {
		t.Fatalf("test cooldown status=%d body=%s", cooldownTest.Code, cooldownTest.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.testHits != 1 || upstream.optionPuts != 1 || upstream.ratios["economy"].String() != "0.08" {
		t.Fatalf("upstream state testHits=%d optionPuts=%d ratios=%#v", upstream.testHits, upstream.optionPuts, upstream.ratios)
	}
	if _, err := st.ListTargets(context.Background()); err != nil {
		t.Fatal(err)
	}
}
