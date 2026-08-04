package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestPricingPolicyBulkHTTPPreviewAndBestEffortApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	credential := domain.Credential{Name: "bulk admin", Kind: "access-token", SiteURL: "https://bulk-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "bulk upstream", BaseURL: "https://bulk-upstream.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	readyTarget := domain.TargetSite{Name: "ready", BaseURL: "https://bulk-target.example", CredentialID: credential.ID, Enabled: true, WriteMode: "auto"}
	blockedTarget := domain.TargetSite{Name: "blocked", BaseURL: "https://bulk-blocked.example", CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(ctx, &readyTarget); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTarget(ctx, &blockedTarget); err != nil {
		t.Fatal(err)
	}
	ready := domain.PricingPolicy{Name: "ready", SupplierID: supplier.ID, SourceGroup: "a", TargetSiteID: readyTarget.ID,
		TargetGroup: "a", RuleType: "add", Value: "0.01", Enabled: true}
	blocked := domain.PricingPolicy{Name: "blocked", SupplierID: supplier.ID, SourceGroup: "b", TargetSiteID: blockedTarget.ID,
		TargetGroup: "b", RuleType: "add", Value: "0.01", Enabled: true}
	if err := st.CreatePolicy(ctx, &ready); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePolicy(ctx, &blocked); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})

	previewBody := []byte(fmt.Sprintf(`{"policy_ids":[%d,%d],"execution_mode":"auto"}`, ready.ID, blocked.ID))
	previewResponse := performBulkPolicyRequest(handler, http.MethodPost, "/api/v1/pricing-policies/bulk/preview", previewBody, true)
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewEnvelope struct {
		Data domain.PricingPolicyBulkPreview `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if previewEnvelope.Data.Summary.Changed != 1 || previewEnvelope.Data.Summary.Blocked != 1 {
		t.Fatalf("preview=%#v", previewEnvelope.Data)
	}

	atomicBody := []byte(fmt.Sprintf(`{"policy_ids":[%d,%d],"execution_mode":"auto","strategy":"atomic","preview_hash":%q}`,
		ready.ID, blocked.ID, previewEnvelope.Data.PreviewHash))
	atomicResponse := performBulkPolicyRequest(handler, http.MethodPost, "/api/v1/pricing-policies/bulk/apply", atomicBody, true)
	if atomicResponse.Code != http.StatusConflict || !bytes.Contains(atomicResponse.Body.Bytes(), []byte(`"status":"blocked"`)) {
		t.Fatalf("atomic status=%d body=%s", atomicResponse.Code, atomicResponse.Body.String())
	}

	partialBody := []byte(fmt.Sprintf(`{"policy_ids":[%d,%d],"execution_mode":"auto","strategy":"best_effort","preview_hash":%q}`,
		ready.ID, blocked.ID, previewEnvelope.Data.PreviewHash))
	partialResponse := performBulkPolicyRequest(handler, http.MethodPost, "/api/v1/pricing-policies/bulk/apply", partialBody, true)
	if partialResponse.Code != http.StatusOK || !bytes.Contains(partialResponse.Body.Bytes(), []byte(`"status":"partial"`)) {
		t.Fatalf("partial status=%d body=%s", partialResponse.Code, partialResponse.Body.String())
	}
	stored, _ := st.GetPolicy(ctx, ready.ID)
	if stored.ExecutionMode != "auto" {
		t.Fatalf("ready policy mode=%q", stored.ExecutionMode)
	}
}

func TestPricingPolicyBulkHTTPRequiresAdminAndValidMutation(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", RunwayHours: 48})
	unauthorized := performBulkPolicyRequest(handler, http.MethodPost, "/api/v1/pricing-policies/bulk/preview", []byte(`{"policy_ids":[1],"enabled":true}`), false)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	invalid := performBulkPolicyRequest(handler, http.MethodPost, "/api/v1/pricing-policies/bulk/preview", []byte(`{"policy_ids":[1],"execution_mode":"unsafe"}`), true)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func performBulkPolicyRequest(handler http.Handler, method, path string, body []byte, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authorized {
		request.Header.Set("X-RelayOps-Token", "admin-secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
