package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/ratios"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type pairingStatusResponse struct {
	store.PairingTokenState
	ExpectedOrigin         string                       `json:"expected_origin"`
	BrowserCustody         *domain.BrowserCustodyStatus `json:"browser_custody,omitempty"`
	RetryAfterSeconds      int                          `json:"retry_after_seconds,omitempty"`
	AttemptConsumed        *bool                        `json:"attempt_consumed,omitempty"`
	ValidationStage        string                       `json:"validation_stage,omitempty"`
	OldCredentialPreserved bool                         `json:"old_credential_preserved,omitempty"`
}

type supplierInput struct {
	Name                string            `json:"name"`
	BaseURL             string            `json:"base_url"`
	Adapter             string            `json:"adapter"`
	PricingPath         string            `json:"pricing_path"`
	BalancePath         string            `json:"balance_path"`
	CredentialID        *int64            `json:"credential_id"`
	Enabled             *bool             `json:"enabled"`
	PollIntervalSeconds int               `json:"poll_interval_seconds"`
	RechargeValueFactor string            `json:"recharge_value_factor"`
	FocusGroups         []string          `json:"focus_groups"`
	FocusModels         []string          `json:"focus_models"`
	Metadata            map[string]string `json:"metadata"`
	CollectNow          *bool             `json:"collect_now"`
}

func normalizeSupplierMetadata(input map[string]string) (map[string]string, error) {
	metadata := make(map[string]string, len(input))
	for key, value := range input {
		metadata[key] = value
	}
	const keyManagementPath = "api_key_management_path"
	if raw, exists := metadata[keyManagementPath]; exists {
		normalized, err := netguard.NormalizeSameOriginPath(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyManagementPath, err)
		}
		if normalized == "" {
			delete(metadata, keyManagementPath)
		} else {
			metadata[keyManagementPath] = normalized
		}
	}
	return metadata, nil
}

func (s *Server) handleListSuppliers(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListSuppliers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateSupplier(w http.ResponseWriter, r *http.Request) {
	var input supplierInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.BaseURL) == "" {
		writeError(w, http.StatusBadRequest, "名称和站点地址不能为空")
		return
	}
	if _, err := netguard.ParseOrigin(input.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	factor, err := ratios.NormalizeRechargeValueFactor(input.RechargeValueFactor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	metadata, err := normalizeSupplierMetadata(input.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.CredentialID != nil {
		if err := s.validateCredentialOrigin(r.Context(), *input.CredentialID, input.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if existing, findErr := s.store.FindSupplierByBaseURL(r.Context(), input.BaseURL); findErr == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("上游站点已存在：#%d %s，请打开现有站点完成登录或修改", existing.ID, existing.Name))
		return
	} else if !store.IsNotFound(findErr) {
		writeError(w, http.StatusInternalServerError, findErr.Error())
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	item := domain.Supplier{Name: input.Name, BaseURL: input.BaseURL, Adapter: input.Adapter, PricingPath: input.PricingPath,
		BalancePath: input.BalancePath, CredentialID: input.CredentialID, Enabled: enabled,
		PollIntervalSeconds: input.PollIntervalSeconds, RechargeValueFactor: factor,
		FocusGroups: input.FocusGroups, FocusModels: input.FocusModels,
		Metadata: metadata}
	if err := s.store.CreateSupplier(r.Context(), &item); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	collectNow := input.CollectNow == nil || *input.CollectNow
	if !collectNow {
		writeJSON(w, http.StatusCreated, map[string]any{"supplier": item, "collection": nil})
		return
	}
	result, err := s.collector.Collect(r.Context(), item.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"supplier": item, "collection_error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"supplier": result.Supplier, "collection": result})
}

func (s *Server) handleUpdateSupplier(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	current, err := s.store.GetSupplier(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	var input supplierInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.BaseURL) == "" {
		writeError(w, http.StatusBadRequest, "名称和站点地址不能为空")
		return
	}
	if input.PollIntervalSeconds < 30 || input.PollIntervalSeconds > 86400 {
		writeError(w, http.StatusBadRequest, "采集间隔必须在 30 秒到 24 小时之间")
		return
	}
	current.Name = input.Name
	current.BaseURL = input.BaseURL
	current.Adapter = input.Adapter
	current.PricingPath = input.PricingPath
	current.BalancePath = input.BalancePath
	current.CredentialID = input.CredentialID
	current.PollIntervalSeconds = input.PollIntervalSeconds
	factor, err := ratios.NormalizeRechargeValueFactor(input.RechargeValueFactor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	current.RechargeValueFactor = factor
	current.FocusGroups = input.FocusGroups
	current.FocusModels = input.FocusModels
	metadata, err := normalizeSupplierMetadata(input.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	current.Metadata = metadata
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if _, err := netguard.ParseOrigin(current.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if current.CredentialID != nil {
		if err := s.validateCredentialOrigin(r.Context(), *current.CredentialID, current.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.store.UpdateSupplier(r.Context(), current); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	updated, _ := s.store.GetSupplier(r.Context(), id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleSupplierDeletionImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	impact, err := s.store.PreviewSupplierDeletion(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

type supplierDeleteInput struct {
	ConfirmName string `json:"confirm_name"`
	ImpactHash  string `json:"impact_hash"`
}

func (s *Server) handleDeleteSupplier(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input supplierDeleteInput
	if err = readJSON(r, &input); err == nil {
		_, err = s.store.DeleteSupplierSafely(r.Context(), id, input.ConfirmName, input.ImpactHash)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleCollectSupplier(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	wait := r.URL.Query().Get("wait") == "true"
	if wait {
		result, collectErr := s.collector.CollectOrJoin(r.Context(), id)
		if collectErr != nil {
			writeError(w, statusCode(collectErr), collectErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	go func() {
		ctx, cancel := background(90 * time.Second)
		defer cancel()
		_, _ = s.collector.CollectOrJoin(ctx, id)
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) handleCollectAll(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListSuppliers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jobs := make([]collectionJobSupplier, 0, len(items))
	for _, item := range items {
		if item.Enabled {
			jobs = append(jobs, collectionJobSupplier{ID: item.ID, Name: item.Name})
		}
	}
	job, created, err := s.startCollectionJob(jobs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": created, "job": job})
}

type collectionJobSupplier struct {
	ID   int64
	Name string
}

type collectionJobResult struct {
	SupplierID   int64  `json:"supplier_id"`
	SupplierName string `json:"supplier_name"`
	Status       string `json:"status"`
	Changed      bool   `json:"changed"`
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
}

type collectionJob struct {
	ID         string                `json:"id"`
	Status     string                `json:"status"`
	Total      int                   `json:"total"`
	Completed  int                   `json:"completed"`
	Succeeded  int                   `json:"succeeded"`
	Failed     int                   `json:"failed"`
	Results    []collectionJobResult `json:"results"`
	StartedAt  time.Time             `json:"started_at"`
	FinishedAt *time.Time            `json:"finished_at,omitempty"`
}

func (s *Server) startCollectionJob(suppliers []collectionJobSupplier) (collectionJob, bool, error) {
	s.collectionJobsMu.Lock()
	if active, ok := s.collectionJobs[s.activeCollectionJobID]; ok && active.Status == "running" {
		copy := cloneCollectionJob(active)
		s.collectionJobsMu.Unlock()
		return copy, false, nil
	}
	id, err := randomToken(12)
	if err != nil {
		s.collectionJobsMu.Unlock()
		return collectionJob{}, false, err
	}
	started := time.Now().UTC()
	job := collectionJob{ID: id, Status: "running", Total: len(suppliers), Results: []collectionJobResult{}, StartedAt: started}
	s.collectionJobs[id], s.activeCollectionJobID = job, id
	for key, existing := range s.collectionJobs {
		if key != id && existing.FinishedAt != nil && started.Sub(*existing.FinishedAt) > 24*time.Hour {
			delete(s.collectionJobs, key)
		}
	}
	s.collectionJobsMu.Unlock()
	_ = s.store.AddActivity(context.Background(), domain.Activity{Kind: "collection_batch_started", Severity: "info",
		Title: "全局刷新已启动", Message: fmt.Sprintf("将刷新 %d 个已启用上游", len(suppliers)), Metadata: map[string]string{"job_id": id}})
	if len(suppliers) == 0 {
		now := time.Now().UTC()
		s.collectionJobsMu.Lock()
		job = s.collectionJobs[id]
		job.Status, job.FinishedAt = "completed", &now
		s.collectionJobs[id], s.activeCollectionJobID = job, ""
		s.collectionJobsMu.Unlock()
		return cloneCollectionJob(job), true, nil
	}
	go s.runCollectionJob(id, suppliers)
	return cloneCollectionJob(job), true, nil
}

func (s *Server) runCollectionJob(id string, suppliers []collectionJobSupplier) {
	ctx, cancel := background(10 * time.Minute)
	defer cancel()
	workers := 4
	if workers > len(suppliers) {
		workers = len(suppliers)
	}
	queue := make(chan collectionJobSupplier)
	var workersDone sync.WaitGroup
	workersDone.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer workersDone.Done()
			for supplier := range queue {
				started := time.Now()
				result, err := s.collector.CollectOrJoin(ctx, supplier.ID)
				item := collectionJobResult{SupplierID: supplier.ID, SupplierName: supplier.Name,
					Status: "succeeded", DurationMS: time.Since(started).Milliseconds()}
				if err != nil {
					item.Status, item.Error = "failed", err.Error()
				} else {
					item.Changed, item.DurationMS = result.Changed, result.Duration
				}
				s.collectionJobsMu.Lock()
				job := s.collectionJobs[id]
				job.Completed++
				if err != nil {
					job.Failed++
				} else {
					job.Succeeded++
				}
				job.Results = append(job.Results, item)
				s.collectionJobs[id] = job
				s.collectionJobsMu.Unlock()
			}
		}()
	}
	for _, supplier := range suppliers {
		select {
		case queue <- supplier:
		case <-ctx.Done():
			break
		}
	}
	close(queue)
	workersDone.Wait()
	now := time.Now().UTC()
	s.collectionJobsMu.Lock()
	job := s.collectionJobs[id]
	if job.Completed < job.Total {
		completed := make(map[int64]struct{}, len(job.Results))
		for _, result := range job.Results {
			completed[result.SupplierID] = struct{}{}
		}
		for _, supplier := range suppliers {
			if _, found := completed[supplier.ID]; found {
				continue
			}
			job.Results = append(job.Results, collectionJobResult{SupplierID: supplier.ID, SupplierName: supplier.Name,
				Status: "failed", Error: "全局刷新任务超时，站点未完成采集"})
		}
		job.Failed += job.Total - job.Completed
		job.Completed = job.Total
	}
	switch {
	case job.Failed == 0:
		job.Status = "completed"
	case job.Succeeded == 0:
		job.Status = "failed"
	default:
		job.Status = "partial_failed"
	}
	job.FinishedAt = &now
	s.collectionJobs[id] = job
	if s.activeCollectionJobID == id {
		s.activeCollectionJobID = ""
	}
	s.collectionJobsMu.Unlock()
	severity := "success"
	if job.Failed > 0 {
		severity = "warning"
	}
	_ = s.store.AddActivity(context.Background(), domain.Activity{Kind: "collection_batch_completed", Severity: severity,
		Title: "全局刷新已完成", Message: fmt.Sprintf("成功 %d，失败 %d，共 %d 个上游", job.Succeeded, job.Failed, job.Total),
		Metadata: map[string]string{"job_id": id, "succeeded": fmt.Sprint(job.Succeeded), "failed": fmt.Sprint(job.Failed)}})
}

func (s *Server) handleCollectionJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	s.collectionJobsMu.RLock()
	job, ok := s.collectionJobs[id]
	s.collectionJobsMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "collection job not found")
		return
	}
	writeJSON(w, http.StatusOK, cloneCollectionJob(job))
}

func cloneCollectionJob(job collectionJob) collectionJob {
	job.Results = append([]collectionJobResult(nil), job.Results...)
	return job
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var snapshot domain.CatalogSnapshot
		snapshot, err = s.store.LatestEffectiveSnapshot(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, snapshot)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var items []domain.PriceObservation
		items, err = s.store.LatestPrices(r.Context(), id, 5000)
		if err == nil {
			writeJSON(w, http.StatusOK, items)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleCollectionAttempts(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.store.ListCollectionAttempts(r.Context(), id, queryLimit(r, 50))
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleSupplierCapabilities(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := s.store.ListAuthCapabilities(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListCredentials(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type credentialInput struct {
	Name    string                   `json:"name"`
	Kind    string                   `json:"kind"`
	SiteURL string                   `json:"site_url"`
	Payload domain.CredentialPayload `json:"payload"`
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var input credentialInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := netguard.ParseOrigin(input.SiteURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Payload.CapturedOrigin) != "" {
		if err := netguard.RequireSameOrigin(input.Payload.CapturedOrigin, input.SiteURL); err != nil {
			writeError(w, http.StatusBadRequest, "浏览器捕获站点与凭据站点不一致")
			return
		}
	}
	item := domain.Credential{Name: input.Name, Kind: input.Kind, SiteURL: input.SiteURL}
	if err := s.credentials.Create(r.Context(), &item, normalizeCredential(input.Payload)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	var input struct {
		ConfirmName string `json:"confirm_name"`
		ImpactHash  string `json:"impact_hash"`
	}
	if err == nil {
		err = readJSON(r, &input)
	}
	if err == nil {
		_, err = s.store.DeleteCredentialSafely(r.Context(), id, input.ConfirmName, input.ImpactHash)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleCredentialDeletionImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	impact, err := s.store.PreviewCredentialDeletion(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (s *Server) handleRefreshCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "credential refresh") {
		return
	}
	id, err := parseID(r, "id")
	if err == nil {
		err = s.refresher.Refresh(r.Context(), id)
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	detail, err := s.refresher.RenewalDetail(r.Context(), id)
	if err != nil {
		writeCredentialRenewalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	supplierID, err := parseID(r, "supplierID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.store.GetSupplier(r.Context(), supplierID); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	token, err := randomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	if err := s.store.SavePairingToken(r.Context(), tokenHash(token), supplierID, expires); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"code": token, "expiresIn": 300, "expires_at": expires})
}

func (s *Server) handlePairCompleteLegacy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	token := chiParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "配对码为空")
		return
	}
	var input credentialInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	tokenDigest := tokenHash(token)
	supplierID, err := s.store.PendingPairingSupplier(r.Context(), tokenDigest, now)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), supplierID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	if strings.TrimSpace(input.Payload.CapturedOrigin) == "" {
		writeError(w, http.StatusBadRequest, "配对内容缺少 captured_origin")
		return
	}
	if err := netguard.RequireSameOrigin(input.Payload.CapturedOrigin, supplier.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, "浏览器当前站点与配对上游不一致")
		return
	}
	if !hasCredentialMaterial(input.Payload) {
		writeError(w, http.StatusBadRequest, "当前页面没有捕获到登录凭据，请确认已经登录上游后重试")
		return
	}
	item := domain.Credential{Name: input.Name, Kind: input.Kind, SiteURL: supplier.BaseURL}
	if item.Name == "" {
		item.Name = supplier.Name + " 登录态"
	}
	if item.Kind == "" {
		item.Kind = "browser_pairing"
	}
	payload := normalizeCredential(input.Payload)
	diagnostics, err := s.collector.ValidateCredential(r.Context(), supplier, item.Kind, payload)
	if err != nil && s.refresher != nil {
		errorClass := connectors.ErrorClass(err)
		if errorClass == "auth_expired" || errorClass == "auth_missing" {
			if refreshed, expiresAt, refreshErr := s.refresher.RefreshCapturedCandidate(r.Context(), supplier, payload); refreshErr == nil {
				payload = refreshed
				item.ExpiresAt = expiresAt
				diagnostics, err = s.collector.ValidateCredential(r.Context(), supplier, item.Kind, payload)
			} else if errors.Is(refreshErr, remotewrite.ErrDisabled) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": remotewrite.DisabledMessage,
					"error_class": "remote_writes_disabled", "retryable": false, "attempt_consumed": false,
					"old_credential_preserved": supplier.CredentialID != nil})
				return
			}
		}
	}
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"message":     err.Error(),
			"error_class": connectors.ErrorClass(err),
			"diagnostics": diagnostics,
			"retryable":   true,
		})
		return
	}
	if err := s.credentials.CreateFromPairing(r.Context(), tokenDigest, now, supplier.ID, &item, payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	go func() {
		ctx, cancel := background(90 * time.Second)
		defer cancel()
		_, _ = s.collector.CollectOrJoin(ctx, supplier.ID)
	}()
	writeJSON(w, http.StatusCreated, map[string]any{"credential": item, "supplier_id": supplier.ID, "validation": diagnostics, "collection_started": true})
}

func (s *Server) handlePairStatusLegacy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	token := chiParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "配对码为空")
		return
	}
	status, supplierID, completedAt, err := s.store.PairingStatus(r.Context(), tokenHash(token), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusNotFound, "配对码不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "supplier_id": supplierID, "completed_at": completedAt})
}

func (s *Server) handlePairComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	token := chiParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "pairing code is required")
		return
	}
	now := time.Now().UTC()
	tokenDigest := tokenHash(token)
	lockValue, _ := s.pairingValidationLocks.LoadOrStore(tokenDigest, &sync.Mutex{})
	pairingLock := lockValue.(*sync.Mutex)
	if !pairingLock.TryLock() {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"status": "pending", "error_class": "validation_in_progress",
			"message":   "RelayOps 正在验证刚提交的登录态，请等待当前验证完成。",
			"retryable": true, "attempt_consumed": false, "retry_after_seconds": 2,
			"validation_stage": "upstream_readonly", "submission_received": true,
		})
		return
	}
	defer func() {
		pairingLock.Unlock()
		s.pairingValidationLocks.CompareAndDelete(tokenDigest, pairingLock)
	}()
	pairState, err := s.store.PairingStatusDetail(r.Context(), tokenDigest, now)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "pairing code is invalid")
		return
	}
	if pairState.Status != "pending" {
		pairState = s.pairingAttemptPolicy(pairState)
		writeJSON(w, http.StatusConflict, pairState)
		return
	}
	pairState = s.pairingAttemptPolicy(pairState)
	if retryAfter := s.pairingRetryAfter(pairState, now); retryAfter > 0 {
		retrySeconds := durationCeilSeconds(retryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"status": pairState.Status, "error_class": pairState.LastErrorClass,
			"message": pairState.LastErrorMessage, "retryable": true,
			"attempt_count": pairState.AttemptCount, "attempts_remaining": pairState.AttemptsRemaining,
			"attempt_consumed": false, "retry_after_seconds": retrySeconds,
			"validation_stage": "upstream_readonly", "submission_received": true,
			"old_credential_preserved": true,
		})
		return
	}
	var input credentialInput
	if err := readJSON(r, &input); err != nil {
		s.recordPairingFailure(w, r, tokenDigest, "invalid_payload", "The browser helper submitted an invalid credential payload.", http.StatusBadRequest, false)
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), pairState.SupplierID)
	if err != nil {
		s.recordPairingFailure(w, r, tokenDigest, "supplier_missing", "The configured upstream no longer exists. Start a new pairing session after adding it again.", http.StatusNotFound, true)
		return
	}
	if strings.TrimSpace(input.Payload.CapturedOrigin) == "" {
		s.recordPairingFailure(w, r, tokenDigest, "origin_missing", "The browser helper did not report the page origin. Reopen the configured upstream and retry this pairing code.", http.StatusBadRequest, false)
		return
	}
	if err := netguard.RequireSameOrigin(input.Payload.CapturedOrigin, supplier.BaseURL); err != nil {
		s.recordPairingFailure(w, r, tokenDigest, "origin_mismatch", "The active browser tab does not match the configured upstream origin. Open the correct site and start a new pairing session.", http.StatusBadRequest, true)
		return
	}
	if !hasCredentialMaterial(input.Payload) {
		s.recordPairingFailure(w, r, tokenDigest, "credential_missing", "No reusable login session was found. Sign in to the upstream, then retry this pairing code.", http.StatusBadRequest, false)
		return
	}
	item := domain.Credential{Name: input.Name, Kind: input.Kind, SiteURL: supplier.BaseURL}
	if item.Name == "" {
		item.Name = supplier.Name + " browser session"
	}
	if item.Kind == "" {
		item.Kind = "browser_pairing"
	}
	payload := normalizeCredential(input.Payload)
	diagnostics, err := s.collector.ValidateCredential(r.Context(), supplier, item.Kind, payload)
	if err != nil {
		errorClass := connectors.ErrorClass(err)
		// A browser capture can contain an expired access token together with a
		// still-valid refresh token. Treat that as a recoverable candidate: do one
		// same-origin refresh, then run the normal bounded validator again. The
		// pairing attempt is recorded only once if both checks fail, and neither
		// the original nor refreshed candidate is persisted before re-validation.
		if s.refresher != nil && (errorClass == "auth_expired" || errorClass == "auth_missing") {
			if refreshed, expiresAt, refreshErr := s.refresher.RefreshCapturedCandidate(r.Context(), supplier, payload); refreshErr == nil {
				refreshedDiagnostics, refreshedErr := s.collector.ValidateCredential(r.Context(), supplier, item.Kind, refreshed)
				if refreshedErr == nil {
					payload = refreshed
					item.ExpiresAt = expiresAt
					diagnostics, err = refreshedDiagnostics, nil
				} else {
					// Keep the final diagnostic evidence for the operator, while
					// reporting the refreshed candidate's actual rejection class.
					diagnostics = refreshedDiagnostics
					err = refreshedErr
				}
			} else if errors.Is(refreshErr, remotewrite.ErrDisabled) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "pending",
					"message": remotewrite.DisabledMessage, "error_class": "remote_writes_disabled", "retryable": false,
					"attempt_consumed": false, "attempt_count": pairState.AttemptCount,
					"attempts_remaining": pairState.AttemptsRemaining, "validation_stage": "refresh_blocked",
					"submission_received": true, "old_credential_preserved": supplier.CredentialID != nil})
				return
			}
		}
	}
	if err != nil {
		errorClass := connectors.ErrorClass(err)
		message := pairingValidationMessage(errorClass)
		s.recordPairingValidationFailure(w, r, tokenDigest, errorClass, message, diagnostics, supplier.CredentialID != nil)
		return
	}
	expectedOrigin, err := pairingExpectedOrigin(supplier.BaseURL)
	if err != nil {
		s.recordPairingFailure(w, r, tokenDigest, "origin_invalid", "The configured upstream origin is invalid.", http.StatusConflict, true)
		return
	}
	custodyExpiresAt := now.Add(365 * 24 * time.Hour)
	if err := s.credentials.CreateFromPairingWithCustody(r.Context(), tokenDigest, now, supplier.ID, &item, payload,
		expectedOrigin, custodyExpiresAt); err != nil {
		s.recordPairingFailure(w, r, tokenDigest, "persistence_error", "The verified session could not be saved. Start a new pairing session and try again.", http.StatusConflict, true)
		return
	}
	go func() {
		ctx, cancel := background(90 * time.Second)
		defer cancel()
		_, _ = s.collector.CollectOrJoin(ctx, supplier.ID)
	}()
	writeJSON(w, http.StatusCreated, map[string]any{
		"credential": item, "supplier_id": supplier.ID, "validation": diagnostics, "collection_started": true,
		"browser_custody": domain.BrowserCustodyStatus{Enabled: true, SupplierID: supplier.ID,
			CredentialID: item.ID, ExpectedOrigin: expectedOrigin, ExpiresAt: custodyExpiresAt,
			MinimumVersion: browserCustodyMinimumHelperVersion, HelperState: "binding_pending",
			CreatedAt: now, UpdatedAt: now},
	})
}

func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	token := chiParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "pairing code is required")
		return
	}
	state, err := s.store.PairingStatusDetail(r.Context(), tokenHash(token), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusNotFound, "pairing code does not exist")
		return
	}
	supplier, err := s.store.GetSupplier(r.Context(), state.SupplierID)
	if err != nil {
		writeError(w, http.StatusNotFound, "the configured upstream no longer exists")
		return
	}
	expectedOrigin, err := pairingExpectedOrigin(supplier.BaseURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the configured upstream origin is invalid")
		return
	}
	policyState := s.pairingAttemptPolicy(state)
	response := pairingStatusResponse{PairingTokenState: policyState, ExpectedOrigin: expectedOrigin}
	if policyState.LastAttemptAt != nil {
		consumed := !pairingValidationFailureDoesNotConsume(policyState.LastErrorClass)
		response.AttemptConsumed = &consumed
		response.OldCredentialPreserved = supplier.CredentialID != nil
		response.ValidationStage = pairingValidationStage(policyState.LastErrorClass)
		response.RetryAfterSeconds = durationCeilSeconds(s.pairingRetryAfter(policyState, time.Now().UTC()))
	}
	if custody, custodyErr := s.store.GetBrowserCustodyByToken(r.Context(), tokenHash(token), time.Now().UTC()); custodyErr == nil {
		status := custody.BrowserCustodyStatus
		response.BrowserCustody = &status
	}
	writeJSON(w, http.StatusOK, response)
}

func pairingExpectedOrigin(raw string) (string, error) {
	if _, err := netguard.ParseOrigin(raw); err != nil {
		return "", err
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: strings.ToLower(parsed.Scheme), Host: strings.ToLower(parsed.Host)}).String(), nil
}

func (s *Server) pairingAttemptPolicy(state store.PairingTokenState) store.PairingTokenState {
	state.AttemptsRemaining = s.pairingMaxAttempts - state.AttemptCount
	if state.AttemptsRemaining < 0 {
		state.AttemptsRemaining = 0
	}
	state.Retryable = state.Status == "pending" && state.AttemptsRemaining > 0
	return state
}

func (s *Server) recordPairingFailure(w http.ResponseWriter, r *http.Request, tokenDigest, errorClass, message string,
	status int, terminal bool) {
	s.recordPairingFailureWithDiagnostics(w, r, tokenDigest, errorClass, message, status, terminal, domain.CollectionDiagnostics{})
}

func (s *Server) recordPairingFailureWithDiagnostics(w http.ResponseWriter, r *http.Request, tokenDigest, errorClass,
	message string, status int, terminal bool, diagnostics domain.CollectionDiagnostics) {
	state, err := s.store.RecordPairingFailure(r.Context(), tokenDigest, errorClass, message, time.Now().UTC(),
		s.pairingMaxAttempts, terminal)
	if err != nil {
		writeError(w, http.StatusConflict, "pairing code is no longer available")
		return
	}
	response := map[string]any{
		"status": state.Status, "error_class": errorClass, "message": message,
		"retryable": state.Retryable, "attempt_count": state.AttemptCount,
		"attempts_remaining": state.AttemptsRemaining, "attempt_consumed": true,
		"validation_stage": pairingValidationStage(errorClass), "submission_received": true,
	}
	if len(diagnostics.Requests) > 0 || len(diagnostics.Capabilities) > 0 {
		response["diagnostics"] = diagnostics
	}
	writeJSON(w, status, response)
}

func (s *Server) recordPairingValidationFailure(w http.ResponseWriter, r *http.Request, tokenDigest, errorClass,
	message string, diagnostics domain.CollectionDiagnostics, oldCredentialPreserved bool) {
	consumeAttempt := !pairingValidationFailureDoesNotConsume(errorClass)
	var (
		state store.PairingTokenState
		err   error
	)
	if consumeAttempt {
		state, err = s.store.RecordPairingFailure(r.Context(), tokenDigest, errorClass, message, time.Now().UTC(),
			s.pairingMaxAttempts, false)
	} else {
		state, err = s.store.RecordPairingTransientFailure(r.Context(), tokenDigest, errorClass, message, time.Now().UTC())
		state = s.pairingAttemptPolicy(state)
	}
	if err != nil {
		writeError(w, http.StatusConflict, "pairing code is no longer available")
		return
	}
	response := map[string]any{
		"status": state.Status, "error_class": errorClass, "message": message,
		"retryable": state.Retryable, "attempt_count": state.AttemptCount,
		"attempts_remaining": state.AttemptsRemaining, "attempt_consumed": consumeAttempt,
		"validation_stage": "upstream_readonly", "submission_received": true,
		"old_credential_preserved": oldCredentialPreserved,
	}
	if !consumeAttempt {
		retrySeconds := durationCeilSeconds(s.pairingTransientCooldown)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		response["retry_after_seconds"] = retrySeconds
	}
	if len(diagnostics.Requests) > 0 || len(diagnostics.Capabilities) > 0 {
		response["diagnostics"] = diagnostics
	}
	writeJSON(w, credentialValidationStatus(errorClass), response)
}

func pairingValidationFailureDoesNotConsume(errorClass string) bool {
	switch errorClass {
	case "rate_limited", "upstream_5xx", "connect_timeout", "read_timeout", "timeout", "connection_error",
		"connection_reset", "tls_error", "dns_error", "contract_changed", "unknown_error":
		return true
	default:
		return false
	}
}

func pairingValidationStage(errorClass string) string {
	switch errorClass {
	case "auth_expired", "auth_missing", "permission_denied", "contract_changed", "rate_limited", "upstream_5xx",
		"connect_timeout", "read_timeout", "timeout", "connection_error", "connection_reset", "tls_error", "dns_error", "unknown_error":
		return "upstream_readonly"
	default:
		return "input_validation"
	}
}

func (s *Server) pairingRetryAfter(state store.PairingTokenState, now time.Time) time.Duration {
	if state.LastAttemptAt == nil || !pairingValidationFailureDoesNotConsume(state.LastErrorClass) {
		return 0
	}
	remaining := state.LastAttemptAt.Add(s.pairingTransientCooldown).Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func durationCeilSeconds(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func pairingValidationMessage(errorClass string) string {
	switch errorClass {
	case "auth_expired", "auth_missing":
		return "上游明确拒绝了当前登录会话。请确认账号密码或会话仍有效；若有验证码或二次验证，请改用浏览器配对。"
	case "permission_denied":
		return "登录身份有效，但该账号没有读取所需控制台数据的权限。请换用具备目录或余额读取权限的账号。"
	case "contract_changed":
		return "上游登录已响应，但某项只读数据结构与当前适配器不同。身份有效时不会再因余额字段差异被判为密码错误；请查看下方能力证据。"
	case "rate_limited", "upstream_5xx", "connect_timeout", "read_timeout", "connection_error", "connection_reset", "tls_error", "dns_error":
		return "上游暂时未能完成只读验证。旧凭据和旧数据均已保留，请稍后重试。"
	default:
		return "登录候选没有通过只读能力验证，且未保存。请查看能力证据确认是身份、目录、余额还是用量能力失败。"
	}
}

func hasCredentialMaterial(payload domain.CredentialPayload) bool {
	if strings.TrimSpace(payload.Authorization) != "" || strings.TrimSpace(payload.Cookie) != "" || len(payload.Headers) > 0 {
		return true
	}
	for _, values := range []map[string]string{payload.LocalStorage, payload.SessionStorage} {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func (s *Server) handlePairOptions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

func normalizeCredential(payload domain.CredentialPayload) domain.CredentialPayload {
	return credentials.NormalizeCapturedPayload(payload)
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var item domain.TargetSite
	if err := readJSON(r, &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item.ID = 0
	if item.Name == "" || item.BaseURL == "" || item.CredentialID == 0 {
		writeError(w, http.StatusBadRequest, "目标站需要名称、地址和管理凭据")
		return
	}
	if err := s.validateCredentialOrigin(r.Context(), item.CredentialID, item.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item.Enabled = true
	if err := s.store.CreateTarget(r.Context(), &item); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var item domain.TargetSite
	if err := readJSON(r, &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item.ID = id
	if item.CredentialID == 0 {
		writeError(w, http.StatusBadRequest, "目标站需要管理凭据")
		return
	}
	if err := s.validateCredentialOrigin(r.Context(), item.CredentialID, item.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateTarget(r.Context(), item); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	if s.automation != nil {
		s.automation.InvalidateTargetRead(id)
	}
	updated, _ := s.store.GetTarget(r.Context(), id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	snapshot, err := adapter.Snapshot(r.Context(), target)
	if err != nil {
		_ = s.store.MarkTargetResult(r.Context(), id, "degraded", err.Error())
		writeError(w, statusCode(err), err.Error())
		return
	}
	_ = s.store.MarkTargetResult(r.Context(), id, "healthy", "")
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListPolicies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	var item domain.PricingPolicy
	if err := readJSON(r, &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item.ID = 0
	item.SourceBasis = "group_ratio"
	if err := s.store.CreatePolicy(r.Context(), &item); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var item domain.PricingPolicy
	if err := readJSON(r, &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item.ID = id
	item.SourceBasis = "group_ratio"
	if err := s.store.UpdatePolicy(r.Context(), item); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	updated, _ := s.store.GetPolicy(r.Context(), id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePatchPolicy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &input); err != nil || input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "需要 enabled 字段")
		return
	}
	item, err := s.store.GetPolicy(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	item.Enabled = *input.Enabled
	if err := s.store.UpdatePolicy(r.Context(), item); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": item.Enabled})
}

func (s *Server) handleSimulatePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var result domain.PolicySimulation
		result, err = s.automation.Simulate(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleProposePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var result domain.ChangeProposal
		result, err = s.automation.Propose(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListProposals(r.Context(), "", 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items, err = s.projectProposalsAgainstCurrentInputs(r.Context(), items)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		filtered := make([]domain.ChangeProposal, 0, len(items))
		for _, item := range items {
			if item.Status == status {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleApplyProposal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var result domain.ChangeProposal
		result, err = s.automation.Apply(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
		current, currentErr := s.store.GetProposal(r.Context(), id)
		if currentErr == nil && current.Status == "stale" {
			message := err.Error()
			if strings.TrimSpace(current.Error) != "" {
				message = current.Error
			}
			payload := map[string]any{"message": message, "proposal": current}
			if replacement, replacementErr := s.automation.Propose(r.Context(), current.PolicyID); replacementErr == nil {
				payload["replacement"] = replacement
			}
			writeJSON(w, http.StatusConflict, payload)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleRejectProposal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var snapshotErr error
		snapshot, snapshotErr := s.intelligence.PrepareDecisionSnapshot(r.Context(), id, "reject", time.Now().UTC())
		err = snapshotErr
		if err == nil {
			err = s.store.RejectProposalWithDecisionSnapshot(r.Context(), id, []string{"pending", "failed", "stale"}, snapshot, "管理员已拒绝")
		}
	}
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func (s *Server) handleRollbackProposal(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err == nil {
		var result domain.ChangeProposal
		result, err = s.automation.Rollback(r.Context(), id)
		if err == nil {
			writeJSON(w, http.StatusOK, result)
			return
		}
	}
	writeError(w, statusCode(err), err.Error())
}

func (s *Server) handleDiscoveries(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListDiscoveries(r.Context(), r.URL.Query().Get("all") != "true", 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("include_baseline") != "true" {
		filtered := make([]domain.DiscoveryItem, 0, len(items))
		for _, item := range items {
			if !item.Baseline {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleAcknowledgeDiscovery(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SupplierID int64  `json:"supplier_id"`
		Kind       string `json:"kind"`
		ExternalID string `json:"external_id"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.AcknowledgeDiscovery(r.Context(), input.SupplierID, input.Kind, input.ExternalID); err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"acknowledged": true})
}

func (s *Server) handleConnectorSpecs(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListConnectorSpecs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateConnectorSpec(w http.ResponseWriter, r *http.Request) {
	var item domain.ConnectorSpec
	if err := readJSON(r, &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := connectors.ParseJSONConnectorSpec([]byte(item.SpecJSON))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if item.Name == "" {
		item.Name = parsed.Name
	}
	parsedVersion := strconv.Itoa(parsed.SchemaVersion)
	if item.SchemaVersion != "" && item.SchemaVersion != parsedVersion {
		writeError(w, http.StatusBadRequest, "Connector schema_version 与规则内容不一致")
		return
	}
	item.SchemaVersion = parsedVersion
	if err := s.store.CreateConnectorSpec(r.Context(), &item); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func chiParam(r *http.Request, key string) string {
	return strings.TrimSpace(chi.URLParam(r, key))
}
