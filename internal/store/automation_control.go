package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const automationControlSettingKey = "runtime.automation_control"

func (s *Store) ResolveAutomationControl(ctx context.Context, fallback bool, workersEnabled bool) (domain.AutomationControl, error) {
	var control domain.AutomationControl
	if err := s.GetSetting(ctx, automationControlSettingKey, &control); err == nil {
		control.BackgroundWorkersEnabled = workersEnabled
		return control, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.AutomationControl{}, err
	}
	control = domain.AutomationControl{Enabled: fallback, BackgroundWorkersEnabled: workersEnabled, UpdatedAt: time.Now().UTC()}
	if err := s.SetSetting(ctx, automationControlSettingKey, control); err != nil {
		return domain.AutomationControl{}, err
	}
	return control, nil
}

func (s *Store) SetAutomationControl(ctx context.Context, enabled bool, workersEnabled bool) (domain.AutomationControl, error) {
	control := domain.AutomationControl{Enabled: enabled, BackgroundWorkersEnabled: workersEnabled, UpdatedAt: time.Now().UTC()}
	if err := s.SetSetting(ctx, automationControlSettingKey, control); err != nil {
		return domain.AutomationControl{}, err
	}
	return control, nil
}
