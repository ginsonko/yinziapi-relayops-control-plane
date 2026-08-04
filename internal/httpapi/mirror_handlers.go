package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirrorservice"
)

type mirrorServiceAPI interface {
	SetAutomaticWritesEnabled(bool)
	ListPolicies(context.Context) ([]mirror.Policy, error)
	CreatePolicyDocument(context.Context, mirror.PolicyDocument) (mirror.Policy, error)
	UpdatePolicyDocument(context.Context, mirror.PolicyDocument, int64) (mirror.Policy, error)
	DeletePolicy(context.Context, string, int64) error
	PreviewPolicy(context.Context, mirrorservice.PreviewRequest) (mirrorservice.Preview, error)
	GetPreview(string) (mirrorservice.Preview, error)
	ApplyPreview(context.Context, mirrorservice.ApplyRequest) (mirrorservice.ApplyReceipt, error)
	PreviewExistingChannelAdoption(context.Context, mirrorservice.AdoptionPreviewRequest) (mirrorservice.AdoptionPreview, error)
	GetAdoptionPreview(string) (mirrorservice.AdoptionPreview, error)
	ApplyExistingChannelAdoption(context.Context, mirrorservice.AdoptionApplyRequest) (mirrorservice.AdoptionReceipt, error)
}

func (s *Server) requireMirrorService(w http.ResponseWriter) bool {
	if s.mirror != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "complete mirror service is unavailable")
	return false
}

func (s *Server) handleListMirrorPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	items, err := s.mirror.ListPolicies(r.Context())
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateMirrorPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	var document mirror.PolicyDocument
	if err := readJSON(r, &document); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	policy, err := s.mirror.CreatePolicyDocument(r.Context(), document)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, policy)
}

func (s *Server) handleUpdateMirrorPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	var document mirror.PolicyDocument
	if err := readJSON(r, &document); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	document.PolicyUUID = strings.TrimSpace(document.PolicyUUID)
	if pathUUID := strings.TrimSpace(chi.URLParam(r, "uuid")); pathUUID == "" ||
		document.PolicyUUID != pathUUID {
		writeError(w, http.StatusBadRequest, "policy_uuid must exactly match the route")
		return
	}
	expectedVersion, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("If-Match")), 10, 64)
	if err != nil || expectedVersion <= 0 || expectedVersion != document.PolicyVersion {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the current numeric policy_version")
		return
	}
	policy, err := s.mirror.UpdatePolicyDocument(r.Context(), document, expectedVersion)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleDeleteMirrorPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	policyUUID := strings.TrimSpace(chi.URLParam(r, "uuid"))
	expectedVersion, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("If-Match")), 10, 64)
	if policyUUID == "" || err != nil || expectedVersion <= 0 {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the current numeric policy_version")
		return
	}
	if err := s.mirror.DeletePolicy(r.Context(), policyUUID, expectedVersion); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePreviewMirrorPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	preview, err := s.mirror.PreviewPolicy(r.Context(), mirrorservice.PreviewRequest{
		PolicyUUID: strings.TrimSpace(chi.URLParam(r, "uuid")),
	})
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleGetMirrorPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	preview, err := s.mirror.GetPreview(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyMirrorPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	var input struct {
		ApprovalGranted bool `json:"approval_granted"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	receipt, err := s.mirror.ApplyPreview(r.Context(), mirrorservice.ApplyRequest{
		PreviewID: strings.TrimSpace(chi.URLParam(r, "id")), ApprovalGranted: input.ApprovalGranted,
	})
	if err != nil {
		writeJSON(w, statusCode(err), map[string]any{"receipt": receipt, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handlePreviewMirrorAdoption(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	var request mirrorservice.AdoptionPreviewRequest
	if err := readJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := s.mirror.PreviewExistingChannelAdoption(r.Context(), request)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleGetMirrorAdoptionPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	preview, err := s.mirror.GetAdoptionPreview(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyMirrorAdoption(w http.ResponseWriter, r *http.Request) {
	if !s.requireMirrorService(w) {
		return
	}
	var input struct {
		ApprovalGranted bool `json:"approval_granted"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	receipt, err := s.mirror.ApplyExistingChannelAdoption(r.Context(), mirrorservice.AdoptionApplyRequest{
		PreviewID: strings.TrimSpace(chi.URLParam(r, "id")), ApprovalGranted: input.ApprovalGranted,
	})
	if err != nil {
		writeJSON(w, statusCode(err), map[string]any{"receipt": receipt, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleListMirrorChangeSets(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListMirrorChangeSets(r.Context(), r.URL.Query().Get("policy_uuid"),
		r.URL.Query().Get("status"), queryLimit(r, 100))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetMirrorChangeSet(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := s.store.GetMirrorChangeSet(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) handleListMirrorChangeEvents(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetMirrorChangeSet(r.Context(), id); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	items, err := s.store.ListMirrorChangeEvents(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}
