package httpapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

const (
	targetControlLeaseTTL = 2 * time.Minute
	channelTestCooldown   = time.Minute
)

type pricingOptionPreviewInput struct {
	Values map[string]string `json:"values"`
}

type pricingOptionApplyInput struct {
	ExpectedHash   string            `json:"expected_hash"`
	PreviewHash    string            `json:"preview_hash"`
	Values         map[string]string `json:"values"`
	IdempotencyKey string            `json:"idempotency_key"`
	Confirm        bool              `json:"confirm"`
}

type channelRoutingPreviewInput struct {
	Routing domain.TargetChannelRoutingUpdate `json:"routing"`
}

type channelRoutingApplyInput struct {
	ExpectedHash   string                            `json:"expected_hash"`
	PreviewHash    string                            `json:"preview_hash"`
	Routing        domain.TargetChannelRoutingUpdate `json:"routing"`
	IdempotencyKey string                            `json:"idempotency_key"`
	Confirm        bool                              `json:"confirm"`
}

type channelStatusPreviewInput struct {
	Status int `json:"status"`
}

type channelStatusApplyInput struct {
	ExpectedHash   string `json:"expected_hash"`
	PreviewHash    string `json:"preview_hash"`
	Status         int    `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
	Confirm        bool   `json:"confirm"`
}

type controlledChannelTestInput struct {
	Model           string `json:"model"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	AcknowledgeCost bool   `json:"acknowledge_cost"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type targetReauthorizeInput struct {
	AuthMethod                string    `json:"auth_method"`
	Token                     string    `json:"token"`
	UserID                    int64     `json:"user_id"`
	Username                  string    `json:"username"`
	Password                  string    `json:"password"`
	ExpectedTargetUpdatedAt   time.Time `json:"expected_target_updated_at"`
	ExpectedCredentialID      int64     `json:"expected_credential_id"`
	ExpectedCredentialUpdated time.Time `json:"expected_credential_updated_at"`
}

func (s *Server) handleTargetCapabilities(w http.ResponseWriter, r *http.Request) {
	target, adapter, ok := s.controlledTarget(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, adapter.Capabilities(r.Context(), target))
}

func (s *Server) handlePreviewTargetPricingOption(w http.ResponseWriter, r *http.Request) {
	target, adapter, ok := s.controlledTarget(w, r)
	if !ok {
		return
	}
	var input pricingOptionPreviewInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := adapter.PreviewPricingOption(r.Context(), target, chi.URLParam(r, "option"), input.Values)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyTargetPricingOption(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "controlled target pricing apply") {
		return
	}
	target, adapter, ok := s.controlledTarget(w, r)
	if !ok {
		return
	}
	var input pricingOptionApplyInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(w, http.StatusBadRequest, "必须确认已核对字段差异和删除项")
		return
	}
	if err := validateControlledApplyIdentity(input.ExpectedHash, input.PreviewHash, input.IdempotencyKey); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	option, ok := targets.CanonicalPricingOptionKey(chi.URLParam(r, "option"))
	if !ok {
		writeError(w, http.StatusBadRequest, "价格 Option 不在允许清单中")
		return
	}
	requestHash := targetRequestHash("pricing_option_apply", target.ID, option, input)
	if existing, handled := s.replayTargetControlOperation(w, r, target.ID, input.IdempotencyKey, requestHash); handled {
		return
	} else if existing.ID != 0 {
		s.resumePricingTargetOperation(w, r, target, adapter, existing, option, input)
		return
	}
	preview, err := adapter.PreviewPricingOption(r.Context(), target, option, input.Values)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	if preview.ExpectedHash != input.ExpectedHash || preview.PreviewHash != input.PreviewHash {
		writeError(w, http.StatusConflict, "目标价格已变化，请重新查看差异预览后再确认")
		return
	}
	dimension := "pricing_option:" + option
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, dimension,
		"pricing_option_apply", input.IdempotencyKey, requestHash, preview.ExpectedHash, preview.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	result, mutationErr := adapter.ApplyPricingOptionControlled(r.Context(), target, option, preview.ExpectedHash,
		input.Values, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) handlePreviewTargetChannelRouting(w http.ResponseWriter, r *http.Request) {
	target, adapter, channelID, ok := s.controlledTargetChannel(w, r)
	if !ok {
		return
	}
	var input channelRoutingPreviewInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := adapter.PreviewChannelRouting(r.Context(), target, channelID, input.Routing)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyTargetChannelRouting(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "controlled target channel routing apply") {
		return
	}
	target, adapter, channelID, ok := s.controlledTargetChannel(w, r)
	if !ok {
		return
	}
	var input channelRoutingApplyInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(w, http.StatusBadRequest, "必须确认已核对渠道路由差异")
		return
	}
	if err := validateControlledApplyIdentity(input.ExpectedHash, input.PreviewHash, input.IdempotencyKey); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	requestHash := targetRequestHash("channel_routing_apply", target.ID, strconv.FormatInt(channelID, 10), input)
	if existing, handled := s.replayTargetControlOperation(w, r, target.ID, input.IdempotencyKey, requestHash); handled {
		return
	} else if existing.ID != 0 {
		s.resumeChannelRoutingOperation(w, r, target, adapter, channelID, existing, input)
		return
	}
	preview, err := adapter.PreviewChannelRouting(r.Context(), target, channelID, input.Routing)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	if preview.ExpectedHash != input.ExpectedHash || preview.PreviewHash != input.PreviewHash {
		writeError(w, http.StatusConflict, "渠道配置已变化，请重新查看差异预览后再确认")
		return
	}
	dimension := "channel:" + strconv.FormatInt(channelID, 10) + ":routing"
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, dimension,
		"channel_routing_apply", input.IdempotencyKey, requestHash, preview.ExpectedHash, preview.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	result, mutationErr := adapter.ApplyChannelRoutingControlled(r.Context(), target, channelID, preview.ExpectedHash,
		input.Routing, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) handlePreviewTargetChannelStatus(w http.ResponseWriter, r *http.Request) {
	target, adapter, channelID, ok := s.controlledTargetChannel(w, r)
	if !ok {
		return
	}
	var input channelStatusPreviewInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := adapter.PreviewChannelStatus(r.Context(), target, channelID, input.Status)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleApplyTargetChannelStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "controlled target channel status apply") {
		return
	}
	target, adapter, channelID, ok := s.controlledTargetChannel(w, r)
	if !ok {
		return
	}
	var input channelStatusApplyInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !input.Confirm {
		writeError(w, http.StatusBadRequest, "必须确认已核对渠道状态差异")
		return
	}
	if err := validateControlledApplyIdentity(input.ExpectedHash, input.PreviewHash, input.IdempotencyKey); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	requestHash := targetRequestHash("channel_status_apply", target.ID, strconv.FormatInt(channelID, 10), input)
	if existing, handled := s.replayTargetControlOperation(w, r, target.ID, input.IdempotencyKey, requestHash); handled {
		return
	} else if existing.ID != 0 {
		s.resumeChannelStatusOperation(w, r, target, adapter, channelID, existing, input)
		return
	}
	preview, err := adapter.PreviewChannelStatus(r.Context(), target, channelID, input.Status)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	if preview.ExpectedHash != input.ExpectedHash || preview.PreviewHash != input.PreviewHash {
		writeError(w, http.StatusConflict, "渠道状态已变化，请重新查看差异预览后再确认")
		return
	}
	dimension := "channel:" + strconv.FormatInt(channelID, 10) + ":status"
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, dimension,
		"channel_status_apply", input.IdempotencyKey, requestHash, preview.ExpectedHash, preview.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	result, mutationErr := adapter.ApplyChannelStatusControlled(r.Context(), target, channelID, preview.ExpectedHash,
		input.Status, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) handleControlledTargetChannelTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "billable target channel test") {
		return
	}
	target, controlled, channelID, ok := s.controlledTargetChannel(w, r)
	if !ok {
		return
	}
	base, err := s.targets.ForTarget(target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	channelAdapter, err := targets.AsChannelAdapter(base)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input controlledChannelTestInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	if !input.AcknowledgeCost || input.Model == "" || strings.TrimSpace(input.IdempotencyKey) == "" {
		writeError(w, http.StatusBadRequest, "必须选择模型、确认可能产生上游费用并提供幂等键")
		return
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 60 {
		writeError(w, http.StatusBadRequest, "timeout_seconds 必须在 1 到 60 秒之间")
		return
	}
	requestHash := targetRequestHash("channel_billable_test", target.ID, strconv.FormatInt(channelID, 10), input)
	if existing, handled := s.replayTargetControlOperation(w, r, target.ID, input.IdempotencyKey, requestHash); handled {
		return
	} else if existing.ID != 0 {
		s.finishRecoveredBillableTestAsAmbiguous(w, r, target, existing, requestHash)
		return
	}
	channel, err := channelAdapter.GetChannel(r.Context(), target, channelID)
	if err != nil {
		writeError(w, targetErrorStatus(err), err.Error())
		return
	}
	if !containsString(channel.Models, input.Model) {
		writeError(w, http.StatusBadRequest, "测试模型不在该渠道已配置模型中")
		return
	}
	capabilities := controlled.Capabilities(r.Context(), target)
	if !capabilities.Channels.BillableTest.Available {
		writeError(w, http.StatusConflict, capabilities.Channels.BillableTest.Reason)
		return
	}
	dimension := "channel:" + strconv.FormatInt(channelID, 10) + ":test"
	if latest, err := s.store.LatestTargetControlOperation(r.Context(), target.ID, dimension, "channel_billable_test"); err == nil &&
		time.Since(latest.StartedAt) < channelTestCooldown {
		remaining := channelTestCooldown - time.Since(latest.StartedAt)
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("渠道测试仍在冷却中，请约 %d 秒后重试", int(remaining.Seconds())+1))
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	desiredHash := targetRequestHash("channel_test_selection", target.ID, strconv.FormatInt(channelID, 10), map[string]string{"model": input.Model})
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, dimension,
		"channel_billable_test", input.IdempotencyKey, requestHash, channel.ConfigHash, desiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	if err := s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()); err != nil {
		s.finishTargetControlMutation(w, r, target, operation, lease,
			domain.TargetMutationResult{ResultState: "rejected_before_write", ExpectedHash: channel.ConfigHash, DesiredHash: desiredHash, Error: err.Error()}, err)
		return
	}
	testCtx, cancel := context.WithTimeout(r.Context(), time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	testErr := channelAdapter.TestChannel(testCtx, target, channelID, input.Model)
	result := domain.TargetMutationResult{ExpectedHash: channel.ConfigHash, DesiredHash: desiredHash}
	if testErr == nil {
		result.ResultState = "verified_success"
		result.ReadbackHash = desiredHash
	} else if targets.IsDefinitiveRemoteRejection(testErr) {
		result.ResultState = "rejected_before_write"
		result.Error = testErr.Error()
	} else {
		result.ResultState = "ambiguous_after_write"
		result.Error = testErr.Error()
	}
	s.finishTargetControlMutation(w, r, target, operation, lease, result, testErr)
}

func (s *Server) handleListTargetControlOperations(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.store.ListTargetControlOperations(r.Context(), id, limit)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleReauthorizeTarget(w http.ResponseWriter, r *http.Request) {
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
	var input targetReauthorizeInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.ExpectedCredentialID != target.CredentialID || input.ExpectedTargetUpdatedAt.IsZero() || input.ExpectedCredentialUpdated.IsZero() {
		writeError(w, http.StatusConflict, "目标和旧凭据版本必须与当前页面一致")
		return
	}
	var payload domain.CredentialPayload
	credentialName := target.Name + " 管理凭据"
	switch strings.ToLower(strings.TrimSpace(input.AuthMethod)) {
	case "password":
		if !s.requireRemoteWrites(w, "target administrator login") {
			return
		}
		login, err := s.targets.AuthenticateNewAPI(r.Context(), target.BaseURL, input.Username, input.Password)
		input.Password = ""
		if err != nil {
			writeError(w, targetErrorStatus(err), err.Error())
			return
		}
		payload = login.Payload
		credentialName = target.Name + " root 会话"
	case "token", "access-token":
		if strings.TrimSpace(input.Token) == "" || input.UserID <= 0 {
			writeError(w, http.StatusBadRequest, "Access Token 模式需要 Token 和 root 用户 ID")
			return
		}
		payload = domain.CredentialPayload{Authorization: bearerAuthorization(input.Token), AuthorizationMode: "bearer", UserID: input.UserID,
			CapturedOrigin: target.BaseURL}
		input.Token = ""
	default:
		writeError(w, http.StatusBadRequest, "auth_method 必须是 password 或 token")
		return
	}
	validationTarget := target
	validationTarget.Enabled = true
	matrix := s.targets.ValidateNewAPIManagementCredential(r.Context(), validationTarget, payload)
	if !hasCoreTargetReadCapabilities(matrix) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "新凭据未通过 Option 与渠道只读验证，旧凭据保持不变", "capabilities": matrix})
		return
	}
	credential := domain.Credential{Name: credentialName, Kind: "newapi-management", SiteURL: target.BaseURL}
	encrypted, err := s.credentials.Prepare(&credential, payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	replacedAt, err := s.store.ReplaceTargetCredentialAtomic(r.Context(), target.ID, input.ExpectedCredentialID,
		input.ExpectedTargetUpdatedAt, input.ExpectedCredentialUpdated, &credential, encrypted)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if s.automation != nil {
		s.automation.InvalidateTargetRead(target.ID)
	}
	persistedTarget, err := s.store.GetTarget(r.Context(), target.ID)
	if err == nil {
		persistedAdapter, adapterErr := s.targets.ForTarget(persistedTarget)
		if adapterErr == nil {
			controlled, castErr := targets.AsControlledAdapter(persistedAdapter)
			if castErr == nil {
				matrix = controlled.Capabilities(r.Context(), persistedTarget)
				if !hasCoreTargetReadCapabilities(matrix) {
					err = errors.New("persisted credential failed the mandatory readback")
				}
			} else {
				err = castErr
			}
		} else {
			err = adapterErr
		}
	}
	if err != nil {
		if _, restoreErr := s.store.RestoreTargetCredentialAtomic(r.Context(), target.ID, credential.ID,
			target.CredentialID, replacedAt); restoreErr != nil {
			writeError(w, http.StatusBadGateway, "新凭据持久化后读回失败，且自动恢复旧绑定失败，请停止写操作并检查本地审计日志")
			return
		}
		if s.automation != nil {
			s.automation.InvalidateTargetRead(target.ID)
		}
		writeError(w, http.StatusBadGateway, "新凭据持久化后读回失败，已自动恢复旧凭据")
		return
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "target_reauthorized", Severity: "success",
		Title: "目标站凭据已重新授权并读回", Message: target.Name,
		Metadata: map[string]string{"target_site_id": strconv.FormatInt(target.ID, 10)}})
	writeJSON(w, http.StatusOK, map[string]any{"target": persistedTarget, "credential": credential, "capabilities": matrix})
}

func (s *Server) resumePricingTargetOperation(w http.ResponseWriter, r *http.Request, target domain.TargetSite,
	adapter targets.ControlledAdapter, existing domain.TargetControlOperation, option string, input pricingOptionApplyInput) {
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, existing.Dimension,
		existing.Action, existing.IdempotencyKey, existing.RequestHash, existing.ExpectedHash, existing.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	preview, previewErr := adapter.PreviewPricingOption(r.Context(), target, option, input.Values)
	if previewErr != nil {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, Error: "recovered operation could not inspect remote state: " + previewErr.Error()}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, previewErr)
		return
	}
	if preview.ExpectedHash == existing.DesiredHash && preview.DesiredHash == existing.DesiredHash {
		optionReadback := domain.TargetPriceOption{Key: option, Values: preview.Current, Hash: preview.ExpectedHash}
		result := domain.TargetMutationResult{ResultState: "verified_success", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash, PricingOption: &optionReadback}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, nil)
		return
	}
	if preview.ExpectedHash != existing.ExpectedHash || preview.DesiredHash != existing.DesiredHash {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash,
			Error: "remote state differs from both the operation's expected and desired hashes"}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, errors.New(result.Error))
		return
	}
	result, mutationErr := adapter.ApplyPricingOptionControlled(r.Context(), target, option, existing.ExpectedHash,
		input.Values, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) resumeChannelRoutingOperation(w http.ResponseWriter, r *http.Request, target domain.TargetSite,
	adapter targets.ControlledAdapter, channelID int64, existing domain.TargetControlOperation, input channelRoutingApplyInput) {
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, existing.Dimension,
		existing.Action, existing.IdempotencyKey, existing.RequestHash, existing.ExpectedHash, existing.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	preview, previewErr := adapter.PreviewChannelRouting(r.Context(), target, channelID, input.Routing)
	if previewErr != nil {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, Error: "recovered operation could not inspect remote state: " + previewErr.Error()}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, previewErr)
		return
	}
	if preview.ExpectedHash == existing.DesiredHash {
		channel := preview.Before
		result := domain.TargetMutationResult{ResultState: "verified_success", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash, Channel: &channel}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, nil)
		return
	}
	if preview.ExpectedHash != existing.ExpectedHash || preview.DesiredHash != existing.DesiredHash {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash,
			Error: "remote channel differs from both the operation's expected and desired hashes"}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, errors.New(result.Error))
		return
	}
	result, mutationErr := adapter.ApplyChannelRoutingControlled(r.Context(), target, channelID, existing.ExpectedHash,
		input.Routing, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) resumeChannelStatusOperation(w http.ResponseWriter, r *http.Request, target domain.TargetSite,
	adapter targets.ControlledAdapter, channelID int64, existing domain.TargetControlOperation, input channelStatusApplyInput) {
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, existing.Dimension,
		existing.Action, existing.IdempotencyKey, existing.RequestHash, existing.ExpectedHash, existing.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	preview, previewErr := adapter.PreviewChannelStatus(r.Context(), target, channelID, input.Status)
	if previewErr != nil {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, Error: "recovered operation could not inspect remote state: " + previewErr.Error()}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, previewErr)
		return
	}
	if preview.ExpectedHash == existing.DesiredHash {
		channel := preview.Before
		result := domain.TargetMutationResult{ResultState: "verified_success", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash, Channel: &channel}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, nil)
		return
	}
	if preview.ExpectedHash != existing.ExpectedHash || preview.DesiredHash != existing.DesiredHash {
		result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
			DesiredHash: existing.DesiredHash, ReadbackHash: preview.ExpectedHash,
			Error: "remote channel differs from both the operation's expected and desired hashes"}
		s.finishTargetControlMutation(w, r, target, operation, lease, result, errors.New(result.Error))
		return
	}
	result, mutationErr := adapter.ApplyChannelStatusControlled(r.Context(), target, channelID, existing.ExpectedHash,
		input.Status, func() error { return s.store.CheckTargetControlLease(r.Context(), lease, time.Now().UTC()) })
	s.finishTargetControlMutation(w, r, target, operation, lease, result, mutationErr)
}

func (s *Server) finishRecoveredBillableTestAsAmbiguous(w http.ResponseWriter, r *http.Request,
	target domain.TargetSite, existing domain.TargetControlOperation, requestHash string) {
	operation, lease, replay, err := s.beginTargetControlOperation(r.Context(), target.ID, existing.Dimension,
		existing.Action, existing.IdempotencyKey, requestHash, existing.ExpectedHash, existing.DesiredHash)
	if err != nil {
		writeError(w, targetOperationErrorStatus(err), err.Error())
		return
	}
	if replay {
		writeTargetOperation(w, operation)
		return
	}
	defer func() { _ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC()) }()
	result := domain.TargetMutationResult{ResultState: "ambiguous_after_write", ExpectedHash: existing.ExpectedHash,
		DesiredHash: existing.DesiredHash, Error: "the earlier billable test may already have reached the target and will not be repeated automatically"}
	s.finishTargetControlMutation(w, r, target, operation, lease, result, errors.New(result.Error))
}

func (s *Server) controlledTarget(w http.ResponseWriter, r *http.Request) (domain.TargetSite, targets.ControlledAdapter, bool) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return domain.TargetSite{}, nil, false
	}
	target, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return domain.TargetSite{}, nil, false
	}
	base, err := s.targets.ForTarget(target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return domain.TargetSite{}, nil, false
	}
	adapter, err := targets.AsControlledAdapter(base)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return domain.TargetSite{}, nil, false
	}
	return target, adapter, true
}

func (s *Server) controlledTargetChannel(w http.ResponseWriter, r *http.Request) (domain.TargetSite, targets.ControlledAdapter, int64, bool) {
	target, adapter, ok := s.controlledTarget(w, r)
	if !ok {
		return domain.TargetSite{}, nil, 0, false
	}
	channelID, err := strconv.ParseInt(chi.URLParam(r, "channelID"), 10, 64)
	if err != nil || channelID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid channelID")
		return domain.TargetSite{}, nil, 0, false
	}
	return target, adapter, channelID, true
}

func (s *Server) replayTargetControlOperation(w http.ResponseWriter, r *http.Request, targetID int64,
	key, requestHash string) (domain.TargetControlOperation, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key 必填")
		return domain.TargetControlOperation{}, true
	}
	existing, err := s.store.GetTargetControlOperationByKey(r.Context(), targetID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TargetControlOperation{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return domain.TargetControlOperation{}, true
	}
	if existing.RequestHash != requestHash {
		writeError(w, http.StatusConflict, store.ErrTargetControlIdempotency.Error())
		return domain.TargetControlOperation{}, true
	}
	if existing.ResultState != "in_progress" {
		writeTargetOperation(w, existing)
		return existing, true
	}
	return existing, false
}

func (s *Server) beginTargetControlOperation(ctx context.Context, targetID int64, dimension, action,
	idempotencyKey, requestHash, expectedHash, desiredHash string) (domain.TargetControlOperation, domain.TargetControlLease, bool, error) {
	existing, err := s.store.GetTargetControlOperationByKey(ctx, targetID, idempotencyKey)
	if err == nil {
		if existing.RequestHash != requestHash {
			return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, store.ErrTargetControlIdempotency
		}
		if existing.ResultState != "in_progress" {
			return existing, domain.TargetControlLease{}, true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, err
	}
	owner, err := randomToken(24)
	if err != nil {
		return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, err
	}
	lease, err := s.store.AcquireTargetControlLease(ctx, targetID, dimension, owner, time.Now().UTC(), targetControlLeaseTTL)
	if err != nil {
		return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, err
	}
	if existing.ID != 0 {
		if err := s.store.ResumeTargetControlOperation(ctx, existing.ID, requestHash, lease, time.Now().UTC()); err != nil {
			_ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC())
			return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, err
		}
		existing.FencingToken = lease.FencingToken
		existing.ResultState = "in_progress"
		return existing, lease, false, nil
	}
	operation := domain.TargetControlOperation{TargetSiteID: targetID, Dimension: dimension, Action: action,
		IdempotencyKey: idempotencyKey, RequestHash: requestHash, ExpectedHash: expectedHash, DesiredHash: desiredHash,
		ResultState: "in_progress", FencingToken: lease.FencingToken, StartedAt: time.Now().UTC()}
	if err := s.store.CreateTargetControlOperation(ctx, &operation); err != nil {
		_ = s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC())
		return domain.TargetControlOperation{}, domain.TargetControlLease{}, false, err
	}
	return operation, lease, false, nil
}

func (s *Server) finishTargetControlMutation(w http.ResponseWriter, r *http.Request, target domain.TargetSite,
	operation domain.TargetControlOperation, lease domain.TargetControlLease, result domain.TargetMutationResult, mutationErr error) {
	if s.automation != nil {
		s.automation.InvalidateTargetRead(target.ID)
	}
	if result.ResultState == "" {
		result.ResultState = "ambiguous_after_write"
	}
	if result.Error == "" && mutationErr != nil {
		result.Error = mutationErr.Error()
	}
	encoded, _ := json.Marshal(result)
	if err := s.store.FinishTargetControlOperation(r.Context(), operation.ID, lease, result.ResultState,
		result.ReadbackHash, string(encoded), result.Error, time.Now().UTC()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	finished, err := s.store.GetTargetControlOperation(r.Context(), operation.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	severity := "success"
	if result.ResultState != "verified_success" {
		severity = "warning"
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "target_control_" + operation.Action, Severity: severity,
		Title: "远程目标操作完成", Message: target.Name + ": " + result.ResultState,
		Metadata: map[string]string{"target_site_id": strconv.FormatInt(target.ID, 10), "operation_id": strconv.FormatInt(operation.ID, 10), "result_state": result.ResultState}})
	writeTargetOperation(w, finished)
}

func writeTargetOperation(w http.ResponseWriter, operation domain.TargetControlOperation) {
	status := http.StatusOK
	switch operation.ResultState {
	case "verified_success":
		status = http.StatusOK
	case "rejected_before_write":
		status = http.StatusBadRequest
	case "conflict_before_write", "rolled_back_verified":
		status = http.StatusConflict
	case "ambiguous_after_write", "compensation_failed":
		status = http.StatusBadGateway
	case "in_progress":
		status = http.StatusConflict
	}
	writeJSON(w, status, operation)
}

func validateControlledApplyIdentity(expectedHash, previewHash, idempotencyKey string) error {
	if strings.TrimSpace(expectedHash) == "" || strings.TrimSpace(previewHash) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return errors.New("expected_hash、preview_hash 和 idempotency_key 必填")
	}
	if len(idempotencyKey) > 256 {
		return errors.New("idempotency_key 过长")
	}
	return nil
}

func targetRequestHash(action string, targetID int64, discriminator string, input any) string {
	encoded, _ := json.Marshal(struct {
		Action        string `json:"action"`
		TargetID      int64  `json:"target_id"`
		Discriminator string `json:"discriminator"`
		Input         any    `json:"input"`
	}{Action: action, TargetID: targetID, Discriminator: discriminator, Input: input})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func hasCoreTargetReadCapabilities(matrix domain.TargetCapabilityMatrix) bool {
	group, exists := matrix.PricingOptions["GroupRatio"]
	return exists && group.Read.Available && matrix.Channels.Read.Available
}

func targetErrorStatus(err error) int {
	if errors.Is(err, remotewrite.ErrDisabled) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, targets.ErrChannelNotFound) {
		return http.StatusNotFound
	}
	switch targets.RemoteHTTPStatus(err) {
	case http.StatusUnauthorized:
		return http.StatusUnauthorized
	case http.StatusForbidden:
		return http.StatusForbidden
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadRequest
}

func targetOperationErrorStatus(err error) int {
	if errors.Is(err, store.ErrTargetControlLeaseBusy) || errors.Is(err, store.ErrTargetControlFenceLost) ||
		errors.Is(err, store.ErrTargetControlIdempotency) {
		return http.StatusConflict
	}
	return targetErrorStatus(err)
}

func bearerAuthorization(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 7 && strings.EqualFold(token[:7], "Bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	return "Bearer " + token
}
