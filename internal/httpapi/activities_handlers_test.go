package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestActivityHandlersPaginateFilterExcludeAndDeleteBoundedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openHTTPTestStore(t)
	base := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	fixtures := []domain.Activity{
		{Kind: "collection_completed", Severity: "success", Title: "collect", Message: "catalog read", CreatedAt: base},
		{Kind: "credential_refresh_failed", Severity: "warning", Title: "auth", Message: "login required", CreatedAt: base.Add(time.Minute)},
		{Kind: "proposal_applied", Severity: "info", Title: "price", Message: "ratio changed", CreatedAt: base.Add(2 * time.Minute)},
		{Kind: "complete_mirror_applied", Severity: "success", Title: "mirror", Message: "models synchronized", CreatedAt: base.Add(3 * time.Minute)},
	}
	for _, activity := range fixtures {
		if err := st.AddActivity(ctx, activity); err != nil {
			t.Fatal(err)
		}
	}

	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	first := authenticatedRequest(handler, http.MethodGet, "/api/v1/activities?page=1&limit=2", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status=%d body=%s", first.Code, first.Body.String())
	}
	var firstEnvelope struct {
		Data consoleActivityPage `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstEnvelope); err != nil {
		t.Fatalf("decode first page: %v; body=%s", err, first.Body.String())
	}
	if firstEnvelope.Data.Total != 4 || firstEnvelope.Data.Page != 1 || firstEnvelope.Data.Limit != 2 ||
		len(firstEnvelope.Data.Items) != 2 || firstEnvelope.Data.Items[0].Kind != "mirror" || firstEnvelope.Data.Items[0].RawKind != "complete_mirror_applied" {
		t.Fatalf("first page=%#v", firstEnvelope.Data)
	}
	incremental := authenticatedRequest(handler, http.MethodGet, "/api/v1/activities?after_id=2&limit=50&include_total=false", "")
	if incremental.Code != http.StatusOK {
		t.Fatalf("incremental status=%d body=%s", incremental.Code, incremental.Body.String())
	}
	var incrementalEnvelope struct {
		Data consoleActivityPage `json:"data"`
	}
	if err := json.Unmarshal(incremental.Body.Bytes(), &incrementalEnvelope); err != nil {
		t.Fatalf("decode incremental page: %v; body=%s", err, incremental.Body.String())
	}
	if incrementalEnvelope.Data.Total != 0 || len(incrementalEnvelope.Data.Items) != 2 ||
		incrementalEnvelope.Data.Items[0].ID != "3" || incrementalEnvelope.Data.Items[1].ID != "4" {
		t.Fatalf("incremental page=%#v", incrementalEnvelope.Data)
	}
	invalidCursor := authenticatedRequest(handler, http.MethodGet, "/api/v1/activities?after_id=bad&include_total=false", "")
	if invalidCursor.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status=%d body=%s", invalidCursor.Code, invalidCursor.Body.String())
	}

	query := url.Values{}
	query.Set("from", base.Add(time.Minute).Format(time.RFC3339))
	query.Set("status", "healthy")
	query.Set("exclude_category", "price")
	filtered := authenticatedRequest(handler, http.MethodGet, "/api/v1/activities?"+query.Encode(), "")
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), `"total":1`) ||
		!strings.Contains(filtered.Body.String(), `"kind":"mirror"`) || strings.Contains(filtered.Body.String(), `"kind":"price"`) {
		t.Fatalf("filtered status=%d body=%s", filtered.Code, filtered.Body.String())
	}

	selectedID := firstEnvelope.Data.Items[0].ID
	deleted := authenticatedRequest(handler, http.MethodDelete, "/api/v1/activities", `{"ids":[`+selectedID+`],"limit":500}`)
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":1`) {
		t.Fatalf("delete selected status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	clearAuth := authenticatedRequest(handler, http.MethodDelete, "/api/v1/activities", `{"category":"auth","limit":500}`)
	if clearAuth.Code != http.StatusOK || !strings.Contains(clearAuth.Body.String(), `"deleted":1`) {
		t.Fatalf("clear filtered status=%d body=%s", clearAuth.Code, clearAuth.Body.String())
	}
	unscoped := authenticatedRequest(handler, http.MethodDelete, "/api/v1/activities", `{}`)
	if unscoped.Code != http.StatusBadRequest {
		t.Fatalf("unscoped delete status=%d body=%s", unscoped.Code, unscoped.Body.String())
	}
	invalidRange := authenticatedRequest(handler, http.MethodGet,
		"/api/v1/activities?from=2026-07-30T02%3A00%3A00Z&to=2026-07-30T01%3A00%3A00Z", "")
	if invalidRange.Code != http.StatusBadRequest {
		t.Fatalf("invalid range status=%d body=%s", invalidRange.Code, invalidRange.Body.String())
	}
}

func TestMirrorDraftBatchActivityRecordsBoundedRowReasons(t *testing.T) {
	t.Parallel()
	st := openHTTPTestStore(t)
	handler := New(Dependencies{Store: st, StaticDir: t.TempDir(), AdminToken: "admin-secret"})
	body := `{"target_site_id":1,"created":1,"updated":0,"unchanged":0,"failed":1,"results":[` +
		`{"target_group":"video","supplier":"AIZZZ","source_group":"jimeng","channel":"channel #8","status":"created"},` +
		`{"target_group":"backup","supplier":"BiuAPI","source_group":"plus3","channel":"channel #17","status":"failed","reason":"channel model set differs"}]}`
	response := authenticatedRequest(handler, http.MethodPost, "/api/v1/activities/mirror-draft-batch", body)
	if response.Code != http.StatusOK {
		t.Fatalf("record batch status=%d body=%s", response.Code, response.Body.String())
	}
	page := authenticatedRequest(handler, http.MethodGet, "/api/v1/activities?category=mirror", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `"rawKind":"mirror_draft_batch"`) ||
		!strings.Contains(page.Body.String(), `backup \u003c- BiuAPI / plus3 / channel #17`) ||
		!strings.Contains(page.Body.String(), "channel model set differs") {
		t.Fatalf("mirror batch activity status=%d body=%s", page.Code, page.Body.String())
	}
	invalid := authenticatedRequest(handler, http.MethodPost, "/api/v1/activities/mirror-draft-batch",
		`{"target_site_id":1,"created":1,"updated":0,"unchanged":0,"failed":0,"results":[]}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}
