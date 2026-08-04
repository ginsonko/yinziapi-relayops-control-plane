package httpapi

import (
	"net/http"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (s *Server) handleAutomationConfirmation(w http.ResponseWriter, r *http.Request) {
	if s.automation == nil {
		writeError(w, http.StatusNotImplemented, "automation service is unavailable")
		return
	}
	config, err := s.automation.ConfirmationConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleUpdateAutomationConfirmation(w http.ResponseWriter, r *http.Request) {
	if s.automation == nil {
		writeError(w, http.StatusNotImplemented, "automation service is unavailable")
		return
	}
	current, err := s.automation.ConfirmationConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var input struct {
		Enabled         *bool `json:"enabled"`
		IntervalSeconds *int  `json:"interval_seconds"`
		MaxAttempts     *int  `json:"max_attempts"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if input.IntervalSeconds != nil {
		current.IntervalSeconds = *input.IntervalSeconds
	}
	if input.MaxAttempts != nil {
		current.MaxAttempts = *input.MaxAttempts
	}
	updated, err := s.automation.SetConfirmationConfig(r.Context(), current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "automation_confirmation_updated", Severity: "info",
		Title: "价差定向复采设置已热更新", Message: "仅影响证据不足时的短间隔确认，不会降低自动写价门禁"})
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleAutomationSafety(w http.ResponseWriter, r *http.Request) {
	if s.automation == nil {
		writeError(w, http.StatusNotImplemented, "automation service is unavailable")
		return
	}
	config, err := s.automation.SafetyConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handleUpdateAutomationSafety(w http.ResponseWriter, r *http.Request) {
	if s.automation == nil {
		writeError(w, http.StatusNotImplemented, "automation service is unavailable")
		return
	}
	current, err := s.automation.SafetyConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var input struct {
		TargetReadCacheMilliseconds  *int    `json:"target_read_cache_milliseconds"`
		EquivalentCostMultipleLimit  *string `json:"equivalent_cost_multiple_limit"`
		MinimumComparableDimensions  *int    `json:"minimum_comparable_dimensions"`
		MaximumAutomaticIncreaseRate *string `json:"maximum_automatic_increase_rate"`
		MaximumAutomaticDecreaseRate *string `json:"maximum_automatic_decrease_rate"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.TargetReadCacheMilliseconds != nil {
		current.TargetReadCacheMilliseconds = *input.TargetReadCacheMilliseconds
	}
	if input.EquivalentCostMultipleLimit != nil {
		current.EquivalentCostMultipleLimit = *input.EquivalentCostMultipleLimit
	}
	if input.MinimumComparableDimensions != nil {
		current.MinimumComparableDimensions = *input.MinimumComparableDimensions
	}
	if input.MaximumAutomaticIncreaseRate != nil {
		current.MaximumAutomaticIncreaseRate = *input.MaximumAutomaticIncreaseRate
	}
	if input.MaximumAutomaticDecreaseRate != nil {
		current.MaximumAutomaticDecreaseRate = *input.MaximumAutomaticDecreaseRate
	}
	updated, err := s.automation.SetSafetyConfig(r.Context(), current)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "automation_safety_updated", Severity: "warning",
		Title: "自动写价安全门禁已热更新", Message: "新门槛立即用于后续模拟和无人值守写价；已执行价格不会被自动回调"})
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) automationControl() domain.AutomationControl {
	return domain.AutomationControl{
		Enabled: s.automationEnabled.Load(), BackgroundWorkersEnabled: s.backgroundWorkersEnabled,
		RemoteWritesEnabled: s.remoteWrites.Enabled(),
	}
}

func (s *Server) handleAutomationControl(w http.ResponseWriter, r *http.Request) {
	control, err := s.store.ResolveAutomationControl(r.Context(), s.automationDefaultEnabled, s.backgroundWorkersEnabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	control.RemoteWritesEnabled = s.remoteWrites.Enabled()
	control.Enabled = control.Enabled && control.RemoteWritesEnabled
	writeJSON(w, http.StatusOK, control)
}

func (s *Server) handleUpdateAutomationControl(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled 不能为空")
		return
	}
	if *input.Enabled && !s.remoteWrites.Enabled() {
		writeError(w, http.StatusConflict, "本地验收候选禁止远程写入；不能开启无人值守自动改价")
		return
	}
	if *input.Enabled && !s.backgroundWorkersEnabled {
		writeError(w, http.StatusConflict, "后台调度未启动，不能把无人值守状态标记为开启；请以正式运行模式启动 RelayOps")
		return
	}
	control, err := s.store.SetAutomationControl(r.Context(), *input.Enabled, s.backgroundWorkersEnabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	control.RemoteWritesEnabled = s.remoteWrites.Enabled()
	s.setAutomaticWritesEnabled(control.Enabled)
	severity, title, message := "warning", "无人值守自动改价已暂停", "后台继续采集、续约、模拟和生成建议，但不会执行新的自动价格写入。"
	if control.Enabled {
		severity, title, message = "success", "无人值守自动改价已开启", "满足规则、证据、利润、目标授权和读回门禁的价格变更将由后台自动执行。"
	}
	_ = s.store.AddActivity(r.Context(), domain.Activity{Kind: "automation_control_updated", Severity: severity, Title: title, Message: message})
	writeJSON(w, http.StatusOK, control)
}
