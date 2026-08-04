package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mappingdraft"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type mappingTargetFixture struct {
	mu         sync.Mutex
	writeCount int
}

func (f *mappingTargetFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer root-token" || r.Header.Get("New-Api-User") != "17" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		f.mu.Lock()
		f.writeCount++
		f.mu.Unlock()
		http.Error(w, "read-only fixture", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/status":
		_, _ = w.Write([]byte(`{"success":true,"data":{"version":"0.8.5"}}`))
	case "/api/user/self":
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":17,"username":"admin","role":100}}`))
	case "/api/pricing":
		_, _ = w.Write([]byte(`{"success":true,"data":[{"model_name":"gpt-test","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["economy"]}],"group_ratio":{"economy":0.05}}`))
	case "/api/channel/":
		_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"id":7,"name":"upstream","type":1,"status":1,"base_url":"https://upstream.example/v1","models":"gpt-test","group":"economy","priority":0,"weight":0,"auto_ban":1}],"total":1}}`))
	default:
		http.NotFound(w, r)
	}
}

func TestMappingDraftHTTPPreviewAndMaterializeContract(t *testing.T) {
	remote := &mappingTargetFixture{}
	targetServer := httptest.NewServer(remote)
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
	credential := domain.Credential{Name: "target-admin", Kind: "newapi-admin", SiteURL: targetServer.URL, Status: "active"}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{
		UserID: 17, Authorization: "Bearer root-token",
	}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: targetServer.URL, Adapter: "newapi", CredentialID: credential.ID,
		Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi", Enabled: true, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups: []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "0.04", ModelIDs: []string{"gpt-test"}}},
		Models: []domain.ModelOffer{{ExternalID: "gpt-test", PriceFunction: domain.PriceFunction{
			Currency: "USD", InputPer1M: "2", OutputPer1M: "2",
		}}},
		Evidence: domain.Evidence{ResponseHash: "source-v1", SourceType: "authenticated_api", ObservedAt: observed}}); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, Credentials: credentialService, Targets: targets.NewFactory(targetServer.Client(), credentialService),
		StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/mapping-drafts/preview",
		bytes.NewBufferString(`{"target_site_id":1}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	previewBody, _ := json.Marshal(mappingdraft.PreviewRequest{TargetSiteID: target.ID})
	previewResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/preview", previewBody)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewEnvelope struct {
		Data mappingdraft.Preview `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(previewEnvelope.Data.Candidates) != 1 || previewEnvelope.Data.Candidates[0].Status != "prefilled" {
		t.Fatalf("preview=%#v", previewEnvelope.Data)
	}
	candidate := previewEnvelope.Data.Candidates[0]
	materializeBody, _ := json.Marshal(mappingdraft.MaterializeRequest{TargetSiteID: target.ID,
		TargetEvidenceHash: previewEnvelope.Data.TargetEvidenceHash, Selections: []mappingdraft.MaterializeSelection{{
			CandidateID: candidate.ID, SupplierID: candidate.SupplierID, SourceGroup: candidate.SourceGroup,
			SourceSnapshotHash: candidate.SourceSnapshotHash, TargetGroup: candidate.ProposedTargetGroup,
		}}})
	materializeResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/materialize", materializeBody)
	if materializeResponse.Code != http.StatusCreated {
		t.Fatalf("materialize status=%d body=%s", materializeResponse.Code, materializeResponse.Body.String())
	}
	policies, err := st.ListPolicies(context.Background())
	if err != nil || len(policies) != 1 || policies[0].Enabled || policies[0].ExecutionMode != "approval" || policies[0].RuleType != "add" {
		t.Fatalf("policies=%#v err=%v", policies, err)
	}
	remote.mu.Lock()
	writes := remote.writeCount
	remote.mu.Unlock()
	if writes != 0 {
		t.Fatalf("mapping draft endpoints sent %d target writes", writes)
	}
}

func TestManualMappingDraftHTTPCreateEditAndStaleContract(t *testing.T) {
	remote := &mappingTargetFixture{}
	targetServer := httptest.NewServer(remote)
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
	credential := domain.Credential{Name: "target-admin", Kind: "newapi-admin", SiteURL: targetServer.URL, Status: "active"}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{
		UserID: 17, Authorization: "Bearer root-token",
	}); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: targetServer.URL, Adapter: "newapi", CredentialID: credential.ID,
		Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Adapter: "newapi", Enabled: true, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSnapshot(context.Background(), domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: "0.04", ModelIDs: []string{"gpt-test"}}},
		Models:   []domain.ModelOffer{{ExternalID: "gpt-test", Group: "economy", ModelRatio: "1"}},
		Evidence: domain.Evidence{ResponseHash: "source-v1", SourceType: "authenticated_api", ObservedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, Credentials: credentialService, Targets: targets.NewFactory(targetServer.Client(), credentialService),
		StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/mapping-drafts/manual/preview",
		bytes.NewBufferString(`{"target_site_id":1}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	previewBody, _ := json.Marshal(mappingdraft.ManualPreviewRequest{TargetSiteID: target.ID})
	previewResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/manual/preview", previewBody)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("manual preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewEnvelope struct {
		Data mappingdraft.ManualPreview `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(previewEnvelope.Data.Groups) != 1 || len(previewEnvelope.Data.Groups[0].Options) != 1 {
		t.Fatalf("manual preview=%#v", previewEnvelope.Data)
	}
	gzipRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mapping-drafts/manual/preview", bytes.NewReader(previewBody))
	gzipRequest.Header.Set("Content-Type", "application/json")
	gzipRequest.Header.Set("X-RelayOps-Token", "admin-secret")
	gzipRequest.Header.Set("Accept-Encoding", "br, gzip")
	gzipResponse := httptest.NewRecorder()
	handler.ServeHTTP(gzipResponse, gzipRequest)
	if gzipResponse.Code != http.StatusOK || gzipResponse.Header().Get("Content-Encoding") != "gzip" ||
		gzipResponse.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip preview status=%d encoding=%q vary=%q", gzipResponse.Code,
			gzipResponse.Header().Get("Content-Encoding"), gzipResponse.Header().Get("Vary"))
	}
	reader, err := gzip.NewReader(gzipResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var gzipEnvelope struct {
		Data mappingdraft.ManualPreview `json:"data"`
	}
	if err := json.Unmarshal(decoded, &gzipEnvelope); err != nil {
		t.Fatal(err)
	}
	if gzipEnvelope.Data.TargetEvidenceHash != previewEnvelope.Data.TargetEvidenceHash ||
		len(gzipEnvelope.Data.Groups) != len(previewEnvelope.Data.Groups) ||
		gzipEnvelope.Data.Groups[0].Options[0].CandidateID != previewEnvelope.Data.Groups[0].Options[0].CandidateID {
		t.Fatalf("compressed preview changed semantics: %#v", gzipEnvelope.Data)
	}
	identityRequest := httptest.NewRequest(http.MethodPost, "/api/v1/mapping-drafts/manual/preview", bytes.NewReader(previewBody))
	identityRequest.Header.Set("Content-Type", "application/json")
	identityRequest.Header.Set("X-RelayOps-Token", "admin-secret")
	identityRequest.Header.Set("Accept-Encoding", "gzip;q=0")
	identityResponse := httptest.NewRecorder()
	handler.ServeHTTP(identityResponse, identityRequest)
	if identityResponse.Code != http.StatusOK || identityResponse.Header().Get("Content-Encoding") != "" {
		t.Fatalf("identity preview status=%d encoding=%q", identityResponse.Code, identityResponse.Header().Get("Content-Encoding"))
	}
	group, option := previewEnvelope.Data.Groups[0], previewEnvelope.Data.Groups[0].Options[0]
	selection := mappingdraft.ManualMaterializeSelection{CandidateID: option.CandidateID, SupplierID: option.SupplierID,
		SourceGroup: option.SourceGroup, SourceSnapshotHash: option.SourceSnapshotHash, TargetGroup: group.TargetGroup,
		ConfirmManual: true, Rule: mappingdraft.ManualRuleInput{RuleType: "add", Value: "0.01", ExecutionMode: "approval"}}
	staleBody, _ := json.Marshal(mappingdraft.ManualMaterializeRequest{TargetSiteID: target.ID,
		TargetEvidenceHash: "stale-target-evidence", Selections: []mappingdraft.ManualMaterializeSelection{selection}})
	staleResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/manual/materialize", staleBody)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale materialize status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	createBody, _ := json.Marshal(mappingdraft.ManualMaterializeRequest{TargetSiteID: target.ID,
		TargetEvidenceHash: previewEnvelope.Data.TargetEvidenceHash, Selections: []mappingdraft.ManualMaterializeSelection{selection}})
	createResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/manual/materialize", createBody)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("manual create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}

	currentResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/manual/preview", previewBody)
	var currentEnvelope struct {
		Data mappingdraft.ManualPreview `json:"data"`
	}
	if currentResponse.Code != http.StatusOK || json.Unmarshal(currentResponse.Body.Bytes(), &currentEnvelope) != nil ||
		currentEnvelope.Data.Groups[0].ExistingPolicy == nil {
		t.Fatalf("manual current status=%d body=%s", currentResponse.Code, currentResponse.Body.String())
	}
	currentGroup := currentEnvelope.Data.Groups[0]
	currentOption := currentGroup.Options[0]
	owner := currentGroup.ExistingPolicy
	editSelection := mappingdraft.ManualMaterializeSelection{CandidateID: currentOption.CandidateID, SupplierID: currentOption.SupplierID,
		SourceGroup: currentOption.SourceGroup, SourceSnapshotHash: currentOption.SourceSnapshotHash, TargetGroup: currentGroup.TargetGroup,
		ExpectedPolicyID: owner.ID, ExpectedPolicyUpdatedAt: &owner.UpdatedAt, ConfirmManual: true,
		Rule: mappingdraft.ManualRuleInput{Name: owner.Name, RuleType: "multiply", Value: "1.2", ExecutionMode: "approval"}}
	editBody, _ := json.Marshal(mappingdraft.ManualMaterializeRequest{TargetSiteID: target.ID,
		TargetEvidenceHash: currentEnvelope.Data.TargetEvidenceHash, Selections: []mappingdraft.ManualMaterializeSelection{editSelection}})
	editResponse := authenticatedMappingRequest(handler, http.MethodPost, "/api/v1/mapping-drafts/manual/materialize", editBody)
	if editResponse.Code != http.StatusOK {
		t.Fatalf("manual edit status=%d body=%s", editResponse.Code, editResponse.Body.String())
	}
	policies, err := st.ListPolicies(context.Background())
	if err != nil || len(policies) != 1 || policies[0].RuleType != "multiply" || policies[0].Value != "1.2" {
		t.Fatalf("manual policies=%#v err=%v", policies, err)
	}
	remote.mu.Lock()
	writes := remote.writeCount
	remote.mu.Unlock()
	if writes != 0 {
		t.Fatalf("manual mapping endpoints sent %d target writes", writes)
	}
}

func authenticatedMappingRequest(handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-RelayOps-Token", "admin-secret")
	handler.ServeHTTP(response, request)
	return response
}
