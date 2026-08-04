package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCredentialDeletionRequiresImpactPreviewAndExactName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	credential := domain.Credential{Name: "unused credential", Kind: "access-token", SiteURL: "https://unused-credential.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	previewResponse := authenticatedRequest(handler, http.MethodGet, fmt.Sprintf("/api/v1/credentials/%d/deletion-impact", credential.ID), "")
	if previewResponse.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var preview struct {
		Data domain.CredentialDeletionImpact `json:"data"`
	}
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preview); err != nil || preview.Data.ImpactHash == "" {
		t.Fatalf("preview=%#v err=%v", preview.Data, err)
	}
	if preview.Data.SupplierRefs == nil || preview.Data.TargetRefs == nil {
		t.Fatalf("empty credential references must be JSON arrays: %#v", preview.Data)
	}
	wrong := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/credentials/%d", credential.ID),
		fmt.Sprintf(`{"confirm_name":"wrong","impact_hash":%q}`, preview.Data.ImpactHash))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	confirmed := authenticatedRequest(handler, http.MethodDelete, fmt.Sprintf("/api/v1/credentials/%d", credential.ID),
		fmt.Sprintf(`{"confirm_name":%q,"impact_hash":%q}`, credential.Name, preview.Data.ImpactHash))
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirmed status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
}
