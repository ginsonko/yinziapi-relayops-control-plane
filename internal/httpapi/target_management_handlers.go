package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

type targetConnectInput struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Adapter    string `json:"adapter"`
	WriteMode  string `json:"write_mode"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

func (s *Server) handleConnectTarget(w http.ResponseWriter, r *http.Request) {
	var input targetConnectInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	if strings.TrimSpace(input.Name) == "" || input.BaseURL == "" {
		writeError(w, http.StatusBadRequest, "目标站名称和地址不能为空")
		return
	}
	if _, err := netguard.ParseOrigin(input.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Adapter == "" {
		input.Adapter = "newapi"
	}
	if input.WriteMode == "" {
		input.WriteMode = "approval"
	}
	var payload domain.CredentialPayload
	credentialName := input.Name + " 管理凭据"
	switch strings.ToLower(strings.TrimSpace(input.AuthMethod)) {
	case "password":
		if !s.requireRemoteWrites(w, "target administrator login") {
			return
		}
		login, err := s.targets.AuthenticateNewAPI(r.Context(), input.BaseURL, input.Username, input.Password)
		input.Password = ""
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		payload = login.Payload
		credentialName = input.Name + " root 会话"
	case "token", "access-token":
		if strings.TrimSpace(input.Token) == "" || input.UserID <= 0 {
			writeError(w, http.StatusBadRequest, "Access Token 模式需要 Token 和 root 用户 ID")
			return
		}
		payload = domain.CredentialPayload{Authorization: bearerAuthorization(input.Token), AuthorizationMode: "bearer", UserID: input.UserID,
			CapturedOrigin: input.BaseURL}
		input.Token = ""
	default:
		writeError(w, http.StatusBadRequest, "auth_method 必须是 password 或 token")
		return
	}
	probeTarget := domain.TargetSite{Name: strings.TrimSpace(input.Name), BaseURL: input.BaseURL, Adapter: input.Adapter,
		Enabled: true, WriteMode: input.WriteMode}
	capabilities := s.targets.ValidateNewAPIManagementCredential(r.Context(), probeTarget, payload)
	if !hasCoreTargetReadCapabilities(capabilities) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"message":      "凭据未通过 Option 与渠道只读验证，因此没有创建目标站或保存凭据",
			"capabilities": capabilities,
		})
		return
	}
	credential := domain.Credential{Name: credentialName, Kind: "newapi-management", SiteURL: input.BaseURL}
	if err := s.credentials.Create(r.Context(), &credential, payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target := domain.TargetSite{Name: strings.TrimSpace(input.Name), BaseURL: input.BaseURL, Adapter: input.Adapter,
		CredentialID: credential.ID, Enabled: true, WriteMode: input.WriteMode}
	if err := s.store.CreateTarget(r.Context(), &target); err != nil {
		_ = s.store.DeleteCredential(r.Context(), credential.ID)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	adapter, err := s.targets.ForTarget(target)
	if err != nil {
		target.Enabled = false
		_ = s.store.UpdateTarget(r.Context(), target)
		_ = s.store.MarkTargetResult(r.Context(), target.ID, "degraded", err.Error())
		writeJSON(w, http.StatusCreated, map[string]any{"target": target, "credential": credential, "connection_error": err.Error()})
		return
	}
	snapshot, err := adapter.Snapshot(r.Context(), target)
	if err != nil {
		target.Enabled = false
		_ = s.store.UpdateTarget(r.Context(), target)
		_ = s.store.MarkTargetResult(r.Context(), target.ID, "degraded", err.Error())
		writeJSON(w, http.StatusCreated, map[string]any{"target": target, "credential": credential, "connection_error": err.Error()})
		return
	}
	_ = s.store.MarkTargetResult(r.Context(), target.ID, "healthy", "")
	writeJSON(w, http.StatusCreated, map[string]any{"target": target, "credential": credential, "snapshot": snapshot, "capabilities": capabilities})
}

func (s *Server) handleTargetCatalog(w http.ResponseWriter, r *http.Request) {
	target, channelAdapter, ok := s.targetChannelAdapter(w, r)
	if !ok {
		return
	}
	snapshot, err := channelAdapter.PricingCatalog(r.Context(), target)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleTargetChannels(w http.ResponseWriter, r *http.Request) {
	target, adapter, ok := s.targetChannelAdapter(w, r)
	if !ok {
		return
	}
	items, err := adapter.ListChannels(r.Context(), target)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type groupRatioApplyInput struct {
	ExpectedHash string            `json:"expected_hash"`
	GroupRatio   map[string]string `json:"group_ratio"`
}

func (s *Server) handleApplyTargetGroupRatios(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusGone, "此旧写入接口已停用；请先调用 GroupRatio 预览接口，再携带 preview_hash、幂等键和明确确认调用受控 apply 接口")
}

type channelTestInput struct {
	Model string `json:"model"`
}

func (s *Server) handleTestTargetChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireRemoteWrites(w, "billable target channel test") {
		return
	}
	target, adapter, ok := s.targetChannelAdapter(w, r)
	if !ok {
		return
	}
	channelID, err := strconv.ParseInt(chi.URLParam(r, "channelID"), 10, 64)
	if err != nil || channelID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid channelID")
		return
	}
	var input channelTestInput
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	channel, err := adapter.GetChannel(r.Context(), target, channelID)
	if err != nil {
		writeError(w, statusCode(err), err.Error())
		return
	}
	model := strings.TrimSpace(input.Model)
	if model == "" && len(channel.Models) > 0 {
		model = channel.Models[0]
	}
	if model == "" {
		writeError(w, http.StatusBadRequest, "该渠道没有可用于测试的模型")
		return
	}
	if !containsString(channel.Models, model) {
		writeError(w, http.StatusBadRequest, "测试模型不在该渠道已配置模型中")
		return
	}
	if err := adapter.TestChannel(r.Context(), target, channelID, model); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel_id": channelID, "model": model, "passed": true})
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (s *Server) targetChannelAdapter(w http.ResponseWriter, r *http.Request) (domain.TargetSite, targets.ChannelAdapter, bool) {
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
	adapter, err := targets.AsChannelAdapter(base)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return domain.TargetSite{}, nil, false
	}
	return target, adapter, true
}
