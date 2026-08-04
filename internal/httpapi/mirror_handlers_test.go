package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorservice"
)

func TestMirrorHTTPAuditRoutesAreProtectedBoundedAndStableWithoutWriter(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/mirror-change-sets", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	policies := authenticatedRequest(handler, http.MethodGet, "/api/v1/mirror-policies", "")
	if policies.Code != http.StatusServiceUnavailable {
		t.Errorf("mirror policy status = %d, want %d", policies.Code, http.StatusServiceUnavailable)
	}

	invalid := authenticatedRequest(handler, http.MethodGet, "/api/v1/mirror-change-sets?policy_uuid=bad", "")
	if invalid.Code != http.StatusBadRequest {
		t.Errorf("invalid policy status = %d, want %d", invalid.Code, http.StatusBadRequest)
	}

	list := authenticatedRequest(handler, http.MethodGet, "/api/v1/mirror-change-sets?limit=500", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", list.Code, http.StatusOK)
	}
	if body := list.Body.String(); !strings.Contains(body, `"data":[]`) {
		t.Fatalf("list response %q does not contain an empty data array", body)
	}
}

type mirrorHandlerService struct {
	previewRequest mirrorservice.AdoptionPreviewRequest
	applyRequest   mirrorservice.AdoptionApplyRequest
	preview        mirrorservice.AdoptionPreview
	deletedUUID    string
	deletedVersion int64
}

func (*mirrorHandlerService) SetAutomaticWritesEnabled(bool) {}
func (*mirrorHandlerService) ListPolicies(context.Context) ([]mirror.Policy, error) {
	return []mirror.Policy{}, nil
}
func (*mirrorHandlerService) CreatePolicyDocument(context.Context,
	mirror.PolicyDocument) (mirror.Policy, error) {
	return mirror.Policy{}, nil
}
func (*mirrorHandlerService) UpdatePolicyDocument(context.Context, mirror.PolicyDocument,
	int64) (mirror.Policy, error) {
	return mirror.Policy{}, nil
}
func (service *mirrorHandlerService) DeletePolicy(_ context.Context, policyUUID string, version int64) error {
	service.deletedUUID, service.deletedVersion = policyUUID, version
	return nil
}
func (*mirrorHandlerService) PreviewPolicy(context.Context,
	mirrorservice.PreviewRequest) (mirrorservice.Preview, error) {
	return mirrorservice.Preview{}, nil
}
func (*mirrorHandlerService) GetPreview(string) (mirrorservice.Preview, error) {
	return mirrorservice.Preview{}, nil
}
func (*mirrorHandlerService) ApplyPreview(context.Context,
	mirrorservice.ApplyRequest) (mirrorservice.ApplyReceipt, error) {
	return mirrorservice.ApplyReceipt{}, nil
}
func (service *mirrorHandlerService) PreviewExistingChannelAdoption(_ context.Context,
	request mirrorservice.AdoptionPreviewRequest) (mirrorservice.AdoptionPreview, error) {
	service.previewRequest = request
	return service.preview, nil
}
func (service *mirrorHandlerService) GetAdoptionPreview(id string) (mirrorservice.AdoptionPreview, error) {
	result := service.preview
	result.ID = id
	return result, nil
}
func (service *mirrorHandlerService) ApplyExistingChannelAdoption(_ context.Context,
	request mirrorservice.AdoptionApplyRequest) (mirrorservice.AdoptionReceipt, error) {
	service.applyRequest = request
	return mirrorservice.AdoptionReceipt{PreviewID: request.PreviewID, ChannelID: service.preview.Channel.ID}, nil
}

func TestMirrorAdoptionHTTPRoutesAreAuthenticatedAndBindRouteIdentity(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	service := &mirrorHandlerService{preview: mirrorservice.AdoptionPreview{ID: "preview-1",
		Channel: domainTargetChannelForMirrorHandler(41), Executable: true}}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", Mirror: service})
	body := `{"channel_id":41,"policy":{"supplier_id":7,"source_group":"video","target_site_id":9,"target_group":"sale"}}`

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/mirror-adoptions/preview",
		strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	preview := authenticatedRequest(handler, http.MethodPost, "/api/v1/mirror-adoptions/preview", body)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"id":"preview-1"`) ||
		service.previewRequest.ChannelID != 41 || service.previewRequest.Policy.TargetGroup != "sale" {
		t.Fatalf("preview status=%d body=%q request=%#v", preview.Code, preview.Body.String(), service.previewRequest)
	}

	readback := authenticatedRequest(handler, http.MethodGet, "/api/v1/mirror-adoption-previews/preview-2", "")
	if readback.Code != http.StatusOK || !strings.Contains(readback.Body.String(), `"id":"preview-2"`) {
		t.Fatalf("readback status=%d body=%q", readback.Code, readback.Body.String())
	}

	apply := authenticatedRequest(handler, http.MethodPost, "/api/v1/mirror-adoption-previews/preview-2/apply",
		`{"approval_granted":true}`)
	if apply.Code != http.StatusOK || service.applyRequest.PreviewID != "preview-2" ||
		!service.applyRequest.ApprovalGranted || !strings.Contains(apply.Body.String(), `"channel_id":41`) {
		t.Fatalf("apply status=%d body=%q request=%#v", apply.Code, apply.Body.String(), service.applyRequest)
	}

	invalid := authenticatedRequest(handler, http.MethodPost, "/api/v1/mirror-adoptions/preview", `{`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status=%d body=%q", invalid.Code, invalid.Body.String())
	}
}

func TestDeleteMirrorPolicyRequiresVersionAndBindsRoute(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	service := &mirrorHandlerService{}
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret", Mirror: service})

	missingVersion := authenticatedRequest(handler, http.MethodDelete,
		"/api/v1/mirror-policies/711ae725-6828-5641-b2c7-018676adfec0", "")
	if missingVersion.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing version status=%d", missingVersion.Code)
	}
	request := httptest.NewRequest(http.MethodDelete,
		"/api/v1/mirror-policies/711ae725-6828-5641-b2c7-018676adfec0", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	request.Header.Set("If-Match", "4")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || service.deletedUUID != "711ae725-6828-5641-b2c7-018676adfec0" ||
		service.deletedVersion != 4 {
		t.Fatalf("delete status=%d uuid=%q version=%d", response.Code, service.deletedUUID, service.deletedVersion)
	}
}

func domainTargetChannelForMirrorHandler(id int64) domain.TargetChannel {
	return domain.TargetChannel{ID: id, Models: []string{"model-a"}, Group: "sale", ConfigHash: strings.Repeat("a", 64)}
}
