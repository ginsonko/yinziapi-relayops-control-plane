package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestTargetDeletionRequiresFreshImpactAndExactName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	credential := domain.Credential{Name: "target admin", Kind: "newapi-management", SiteURL: "https://delete-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "fixture target", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	unsafeDelete := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/targets/%d", target.ID), "")
	if unsafeDelete.Code != http.StatusBadRequest {
		t.Fatalf("empty target delete status=%d body=%s", unsafeDelete.Code, unsafeDelete.Body.String())
	}
	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/targets/%d/deletion-impact", target.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.TargetDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || preview.Data.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview.Data, err)
	}
	wrong := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/targets/%d", target.ID),
		fmt.Sprintf(`{"confirm_name":"wrong","impact_hash":%q}`, preview.Data.ImpactHash))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	confirmed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/targets/%d", target.ID),
		fmt.Sprintf(`{"confirm_name":%q,"impact_hash":%q}`, target.Name, preview.Data.ImpactHash))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirmed target delete status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}
