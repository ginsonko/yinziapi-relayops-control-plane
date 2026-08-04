package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestPricingPolicyDeletionRequiresPreviewAndExactName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	supplier := domain.Supplier{Name: "source", BaseURL: "https://policy-delete-source.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	credential := domain.Credential{Name: "target", Kind: "newapi-management", SiteURL: "https://policy-delete-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "fixture policy", SupplierID: supplier.ID, SourceGroup: "upstream", TargetSiteID: target.ID,
		TargetGroup: "local", RuleType: "add", Value: "0.03", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/pricing-policies/%d/deletion-impact", policy.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.PricingPolicyDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || preview.Data.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview.Data, err)
	}
	wrong := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/pricing-policies/%d", policy.ID),
		fmt.Sprintf(`{"confirm_name":"wrong","impact_hash":%q}`, preview.Data.ImpactHash))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	confirmed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/pricing-policies/%d", policy.ID),
		fmt.Sprintf(`{"confirm_name":%q,"impact_hash":%q}`, policy.Name, preview.Data.ImpactHash))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirmed status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}
