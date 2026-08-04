package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type consoleActivityPage struct {
	Items []consoleActivity `json:"items"`
	Page  int               `json:"page"`
	Limit int               `json:"limit"`
	Total int64             `json:"total"`
}

type deleteActivitiesInput struct {
	IDs             []int64 `json:"ids,omitempty"`
	From            string  `json:"from,omitempty"`
	To              string  `json:"to,omitempty"`
	Category        string  `json:"category,omitempty"`
	Status          string  `json:"status,omitempty"`
	ExcludeCategory string  `json:"exclude_category,omitempty"`
	All             bool    `json:"all,omitempty"`
	Limit           int     `json:"limit,omitempty"`
}

type mirrorDraftBatchResult struct {
	TargetGroup string `json:"target_group"`
	Supplier    string `json:"supplier"`
	SourceGroup string `json:"source_group"`
	Channel     string `json:"channel"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

type mirrorDraftBatchInput struct {
	TargetSiteID int64                    `json:"target_site_id"`
	Created      int                      `json:"created"`
	Updated      int                      `json:"updated"`
	Unchanged    int                      `json:"unchanged"`
	Failed       int                      `json:"failed"`
	Results      []mirrorDraftBatchResult `json:"results"`
}

func (s *Server) handleRecordMirrorDraftBatch(w http.ResponseWriter, r *http.Request) {
	var input mirrorDraftBatchInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.TargetSiteID <= 0 || len(input.Results) == 0 || len(input.Results) > 200 ||
		input.Created < 0 || input.Updated < 0 || input.Unchanged < 0 || input.Failed < 0 ||
		input.Created+input.Updated+input.Unchanged+input.Failed != len(input.Results) {
		writeError(w, http.StatusBadRequest, "invalid mirror draft batch summary")
		return
	}
	failures := make([]string, 0, 10)
	for _, result := range input.Results {
		if !validMirrorDraftBatchField(result.TargetGroup, 128) || !validMirrorDraftBatchField(result.Supplier, 160) ||
			!validMirrorDraftBatchField(result.SourceGroup, 160) || !validMirrorDraftBatchField(result.Channel, 160) ||
			(result.Status != "created" && result.Status != "updated" && result.Status != "unchanged" && result.Status != "failed") ||
			len(result.Reason) > 500 || strings.ContainsAny(result.Reason, "\x00\r\n") {
			writeError(w, http.StatusBadRequest, "invalid mirror draft batch row")
			return
		}
		if result.Status == "failed" && len(failures) < 10 {
			failures = append(failures, fmt.Sprintf("%s <- %s / %s / %s：%s", result.TargetGroup,
				result.Supplier, result.SourceGroup, result.Channel, strings.TrimSpace(result.Reason)))
		}
	}
	message := fmt.Sprintf("处理 %d 个分组：新建 %d、更新 %d、无需变更 %d、失败 %d。",
		len(input.Results), input.Created, input.Updated, input.Unchanged, input.Failed)
	if len(failures) > 0 {
		message += " 失败明细：" + strings.Join(failures, "；")
		if input.Failed > len(failures) {
			message += fmt.Sprintf("；其余 %d 条请查看映射草案逐行结果", input.Failed-len(failures))
		}
	}
	severity := "success"
	if input.Failed > 0 {
		severity = "warning"
	}
	err := s.store.AddActivity(r.Context(), domain.Activity{Kind: "mirror_draft_batch", Severity: severity,
		Title: "分组镜像草案批量处理完成", Message: message, Metadata: map[string]string{
			"target_site_id": strconv.FormatInt(input.TargetSiteID, 10), "total": strconv.Itoa(len(input.Results)),
			"created": strconv.Itoa(input.Created), "updated": strconv.Itoa(input.Updated),
			"unchanged": strconv.Itoa(input.Unchanged), "failed": strconv.Itoa(input.Failed),
		}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": true})
}

func validMirrorDraftBatchField(value string, limit int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}

func (s *Server) handleListActivities(w http.ResponseWriter, r *http.Request) {
	query, err := parseActivityQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.store.QueryActivities(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items := make([]consoleActivity, 0, len(page.Items))
	for _, activity := range page.Items {
		items = append(items, projectConsoleActivity(activity))
	}
	writeJSON(w, http.StatusOK, consoleActivityPage{Items: items, Page: page.Page, Limit: page.Limit, Total: page.Total})
}

func (s *Server) handleDeleteActivities(w http.ResponseWriter, r *http.Request) {
	var input deleteActivitiesInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query, err := activityQueryFromValues(1, 50, input.From, input.To, input.Category, input.Status, input.ExcludeCategory)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	deleted, err := s.store.DeleteActivities(r.Context(), store.ActivityDeleteRequest{
		IDs: input.IDs, Query: query, All: input.All, Limit: input.Limit,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "limit": boundedActivityDeleteLimit(input.Limit)})
}

func parseActivityQuery(r *http.Request) (store.ActivityQuery, error) {
	page := parsePositiveQueryInt(r.URL.Query().Get("page"), 1)
	limit := parsePositiveQueryInt(r.URL.Query().Get("limit"), 50)
	query, err := activityQueryFromValues(page, limit, r.URL.Query().Get("from"), r.URL.Query().Get("to"),
		r.URL.Query().Get("category"), r.URL.Query().Get("status"), r.URL.Query().Get("exclude_category"))
	if err != nil {
		return store.ActivityQuery{}, err
	}
	afterIDText := strings.TrimSpace(r.URL.Query().Get("after_id"))
	if afterIDText != "" {
		afterID, parseErr := strconv.ParseInt(afterIDText, 10, 64)
		if parseErr != nil || afterID < 0 {
			return store.ActivityQuery{}, errors.New("after_id must be a non-negative integer")
		}
		query.AfterID = afterID
	}
	query.SkipTotal = strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_total")), "false")
	return query, nil
}

func activityQueryFromValues(page, limit int, fromText, toText, category, status, exclude string) (store.ActivityQuery, error) {
	from, err := parseOptionalActivityTime(fromText)
	if err != nil {
		return store.ActivityQuery{}, errors.New("from must be an RFC3339 timestamp")
	}
	to, err := parseOptionalActivityTime(toText)
	if err != nil {
		return store.ActivityQuery{}, errors.New("to must be an RFC3339 timestamp")
	}
	if from != nil && to != nil && from.After(*to) {
		return store.ActivityQuery{}, errors.New("from must not be after to")
	}
	return store.ActivityQuery{Page: page, Limit: limit, From: from, To: to,
		Category: strings.TrimSpace(category), Status: strings.TrimSpace(status), ExcludeCategory: strings.TrimSpace(exclude)}, nil
}

func parseOptionalActivityTime(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func parsePositiveQueryInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func boundedActivityDeleteLimit(value int) int {
	if value <= 0 || value > 500 {
		return 500
	}
	return value
}
