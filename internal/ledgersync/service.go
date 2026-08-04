package ledgersync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/netguard"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/targets"
)

var ErrSyncBusy = errors.New("target ledger sync is already running")

const (
	streamUsers       = "users"
	streamLogs        = "logs"
	streamTopups      = "topups"
	streamCommissions = "commissions"
)

type StreamStatus struct {
	store.TargetLedgerSyncState
	HasLastKnownGood bool   `json:"has_last_known_good"`
	Unsupported      bool   `json:"unsupported"`
	AuthRequired     bool   `json:"auth_required"`
	NextAction       string `json:"next_action"`
}

type TargetStatus struct {
	Config                   store.TargetLedgerSyncConfig            `json:"config"`
	Streams                  []StreamStatus                          `json:"streams"`
	LastRunAt                *time.Time                              `json:"last_run_at,omitempty"`
	NextRunAt                *time.Time                              `json:"next_run_at,omitempty"`
	Running                  bool                                    `json:"running"`
	UnitMode                 string                                  `json:"unit_mode"`
	EffectiveCurrency        string                                  `json:"effective_currency,omitempty"`
	EffectiveQuotaCashFactor string                                  `json:"effective_quota_cash_factor,omitempty"`
	UnitSource               string                                  `json:"unit_source,omitempty"`
	UnitObservedAt           *time.Time                              `json:"unit_observed_at,omitempty"`
	UnitWarning              string                                  `json:"unit_warning,omitempty"`
	CostResolution           *store.TargetLedgerCostResolutionStatus `json:"cost_resolution,omitempty"`
	GeneratedAt              time.Time                               `json:"generated_at"`
}

type SyncResult struct {
	TargetSiteID int64        `json:"target_site_id"`
	Status       TargetStatus `json:"status"`
	StartedAt    time.Time    `json:"started_at"`
	FinishedAt   time.Time    `json:"finished_at"`
}

type credentialLoader interface {
	Load(context.Context, int64) (domain.Credential, domain.CredentialPayload, error)
}

type Service struct {
	store         *store.Store
	credentials   credentialLoader
	client        *http.Client
	targetFactory targetFactory
	ownerID       string
	unitMu        sync.RWMutex
	units         map[int64]effectiveUnitContract
}

// targetFactory is intentionally narrow so ledger enrichment cannot acquire
// write-capable target operations. It only needs a channel read adapter.
type targetFactory interface {
	ForTarget(domain.TargetSite) (targets.Adapter, error)
}

type effectiveUnitContract struct {
	Currency, Factor, Source, Warning string
	ObservedAt                        time.Time
}

func NewService(st *store.Store, credentialService *credentials.Service, client *http.Client) *Service {
	return newService(st, credentialService, client)
}

func (s *Service) SetTargetFactory(factory targetFactory) { s.targetFactory = factory }

func newService(st *store.Store, credentialService credentialLoader, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{store: st, credentials: credentialService, client: client, ownerID: randomOwnerID(), units: map[int64]effectiveUnitContract{}}
}

func DefaultConfig(targetID int64) store.TargetLedgerSyncConfig {
	return store.TargetLedgerSyncConfig{TargetSiteID: targetID, Enabled: true, IntervalSeconds: 300, OverlapSeconds: 600,
		InitialLookbackSeconds: 86400, PageSize: 100, MaxPages: 100, CostEvidenceMaxAgeSeconds: 86400,
		SupplierCostCashRate: "1", LogPath: "/api/log/", TopupPath: "/api/user/topup", UserPath: "/api/user/"}
}

func NormalizeConfig(config store.TargetLedgerSyncConfig) store.TargetLedgerSyncConfig {
	defaults := DefaultConfig(config.TargetSiteID)
	if config.IntervalSeconds <= 0 {
		config.IntervalSeconds = defaults.IntervalSeconds
	}
	if config.OverlapSeconds < 0 {
		config.OverlapSeconds = defaults.OverlapSeconds
	}
	if config.InitialLookbackSeconds <= 0 {
		config.InitialLookbackSeconds = defaults.InitialLookbackSeconds
	}
	if config.PageSize <= 0 {
		config.PageSize = defaults.PageSize
	}
	if config.MaxPages <= 0 {
		config.MaxPages = defaults.MaxPages
	}
	if config.CostEvidenceMaxAgeSeconds <= 0 {
		config.CostEvidenceMaxAgeSeconds = defaults.CostEvidenceMaxAgeSeconds
	}
	if strings.TrimSpace(config.SupplierCostCashRate) == "" {
		config.SupplierCostCashRate = defaults.SupplierCostCashRate
	}
	if strings.TrimSpace(config.LogPath) == "" {
		config.LogPath = defaults.LogPath
	}
	if strings.TrimSpace(config.TopupPath) == "" {
		config.TopupPath = defaults.TopupPath
	}
	if strings.TrimSpace(config.UserPath) == "" {
		config.UserPath = defaults.UserPath
	}
	config.Currency = strings.ToUpper(strings.TrimSpace(config.Currency))
	config.QuotaCashFactor = strings.TrimSpace(config.QuotaCashFactor)
	config.LogPath, config.TopupPath, config.UserPath = strings.TrimSpace(config.LogPath), strings.TrimSpace(config.TopupPath), strings.TrimSpace(config.UserPath)
	return config
}

func ValidateConfig(config store.TargetLedgerSyncConfig) error {
	config = NormalizeConfig(config)
	if config.TargetSiteID <= 0 {
		return errors.New("target_site_id is required")
	}
	if config.IntervalSeconds < 60 || config.IntervalSeconds > 86400 {
		return errors.New("interval_seconds must be between 60 and 86400")
	}
	if config.OverlapSeconds < 0 || config.OverlapSeconds > 86400 {
		return errors.New("overlap_seconds must be between 0 and 86400")
	}
	if config.InitialLookbackSeconds < 300 || config.InitialLookbackSeconds > 90*86400 {
		return errors.New("initial_lookback_seconds must be between 300 and 7776000")
	}
	if config.PageSize < 10 || config.PageSize > 500 || config.MaxPages < 1 || config.MaxPages > 500 {
		return errors.New("page_size must be 10..500 and max_pages must be 1..500")
	}
	if config.CostEvidenceMaxAgeSeconds < 300 || config.CostEvidenceMaxAgeSeconds > 90*86400 {
		return errors.New("cost_evidence_max_age_seconds must be between 300 and 7776000")
	}
	if factor, err := decimal.NewFromString(strings.TrimSpace(config.SupplierCostCashRate)); err != nil || !factor.IsPositive() {
		return errors.New("supplier_cost_cash_rate must be a positive decimal")
	}
	for name, path := range map[string]string{"log_path": config.LogPath, "topup_path": config.TopupPath, "user_path": config.UserPath} {
		parsed, err := url.Parse(path)
		if err != nil || !strings.HasPrefix(path, "/") || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s must be a same-origin absolute path", name)
		}
	}
	if config.Currency != "" {
		if len(config.Currency) != 3 {
			return errors.New("currency must be a three-letter code")
		}
		for _, char := range config.Currency {
			if char < 'A' || char > 'Z' {
				return errors.New("currency must be a three-letter code")
			}
		}
	}
	if config.QuotaCashFactor != "" {
		if config.Currency == "" {
			return errors.New("currency is required when quota_cash_factor is configured")
		}
		factor, err := decimal.NewFromString(config.QuotaCashFactor)
		if err != nil || !factor.IsPositive() {
			return errors.New("quota_cash_factor must be a positive decimal")
		}
	}
	return nil
}

func (s *Service) Config(ctx context.Context, targetID int64) (store.TargetLedgerSyncConfig, error) {
	config, err := s.store.GetTargetLedgerSyncConfig(ctx, targetID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, targetErr := s.store.GetTarget(ctx, targetID); targetErr != nil {
			return store.TargetLedgerSyncConfig{}, targetErr
		}
		return DefaultConfig(targetID), nil
	}
	if err != nil {
		return store.TargetLedgerSyncConfig{}, err
	}
	return NormalizeConfig(config), nil
}

func (s *Service) UpdateConfig(ctx context.Context, config store.TargetLedgerSyncConfig) (store.TargetLedgerSyncConfig, error) {
	config = NormalizeConfig(config)
	if err := ValidateConfig(config); err != nil {
		return store.TargetLedgerSyncConfig{}, err
	}
	if _, err := s.store.GetTarget(ctx, config.TargetSiteID); err != nil {
		return store.TargetLedgerSyncConfig{}, err
	}
	if err := s.store.UpsertTargetLedgerSyncConfig(ctx, &config); err != nil {
		return store.TargetLedgerSyncConfig{}, err
	}
	return config, nil
}

func (s *Service) Status(ctx context.Context, targetID int64) (TargetStatus, error) {
	config, err := s.Config(ctx, targetID)
	if err != nil {
		return TargetStatus{}, err
	}
	states, err := s.store.ListTargetLedgerSyncStates(ctx, targetID)
	if err != nil {
		return TargetStatus{}, err
	}
	byStream := make(map[string]store.TargetLedgerSyncState, len(states))
	for _, state := range states {
		byStream[state.Stream] = state
	}
	result := TargetStatus{Config: config, Streams: make([]StreamStatus, 0, 4), GeneratedAt: time.Now().UTC()}
	if costStatus, costErr := s.store.GetTargetLedgerCostResolutionStatus(ctx, targetID); costErr == nil {
		result.CostResolution = &costStatus
	} else if !errors.Is(costErr, sql.ErrNoRows) {
		return TargetStatus{}, costErr
	}
	if config.Currency != "" {
		result.UnitMode, result.EffectiveCurrency, result.EffectiveQuotaCashFactor = "manual", config.Currency, config.QuotaCashFactor
		result.UnitSource = "ledger sync configuration"
	} else {
		result.UnitMode = "auto"
		s.unitMu.RLock()
		unit, ok := s.units[targetID]
		s.unitMu.RUnlock()
		if ok {
			result.EffectiveCurrency, result.EffectiveQuotaCashFactor = unit.Currency, unit.Factor
			result.UnitSource, result.UnitWarning = unit.Source, unit.Warning
			if !unit.ObservedAt.IsZero() {
				observed := unit.ObservedAt
				result.UnitObservedAt = &observed
			}
		}
	}
	for _, stream := range []string{streamUsers, streamLogs, streamTopups, streamCommissions} {
		state, ok := byStream[stream]
		if !ok {
			state = store.TargetLedgerSyncState{TargetSiteID: targetID, Stream: stream, Status: "pending"}
		}
		entry := StreamStatus{TargetLedgerSyncState: state, HasLastKnownGood: state.LastSuccessAt != nil,
			Unsupported: state.Status == "unsupported", AuthRequired: state.Status == "auth_required"}
		entry.NextAction = nextAction(entry)
		result.Streams = append(result.Streams, entry)
		if state.LastAttemptAt != nil && (result.LastRunAt == nil || state.LastAttemptAt.After(*result.LastRunAt)) {
			value := *state.LastAttemptAt
			result.LastRunAt = &value
		}
		if !state.NextAttemptAt.IsZero() && (result.NextRunAt == nil || state.NextAttemptAt.Before(*result.NextRunAt)) {
			value := state.NextAttemptAt
			result.NextRunAt = &value
		}
	}
	return result, nil
}

func nextAction(status StreamStatus) string {
	switch status.Status {
	case "auth_required":
		return "等待目标站凭据自动续约；也可重新授权后立即同步"
	case "unsupported":
		if status.Stream == streamCommissions {
			return "目标 NewAPI 版本尚未提供消费返佣成本接口；其它账本流继续同步，升级目标站后会自动启用"
		}
		return "目标站未暴露该只读接口；可在同步配置中覆写接口路径"
	case "failed":
		return "保留上次成功游标并在 next_attempt_at 自动重试"
	case "healthy":
		return "无需操作；系统将在 next_attempt_at 自动增量同步"
	default:
		return "等待首次只读同步"
	}
}

func (s *Service) SyncTarget(ctx context.Context, targetID int64, force bool) (SyncResult, error) {
	started := time.Now().UTC()
	target, err := s.store.GetTarget(ctx, targetID)
	if err != nil {
		return SyncResult{}, err
	}
	config, err := s.Config(ctx, targetID)
	if err != nil {
		return SyncResult{}, err
	}
	if !target.Enabled || !config.Enabled {
		status, statusErr := s.Status(ctx, targetID)
		return SyncResult{TargetSiteID: targetID, Status: status, StartedAt: started, FinishedAt: time.Now().UTC()}, statusErr
	}
	if target.Adapter != "" && !strings.EqualFold(target.Adapter, "newapi") && !strings.EqualFold(target.Adapter, "auto") {
		return SyncResult{}, fmt.Errorf("target adapter %q is not supported by the NewAPI ledger synchronizer", target.Adapter)
	}
	lease, err := s.store.AcquireTargetControlLease(ctx, targetID, "ledger_sync", s.ownerID, started, 10*time.Minute)
	if errors.Is(err, store.ErrTargetControlLeaseBusy) {
		return SyncResult{}, ErrSyncBusy
	}
	if err != nil {
		return SyncResult{}, err
	}
	defer s.store.ReleaseTargetControlLease(context.Background(), lease, time.Now().UTC())

	states, err := s.store.ListTargetLedgerSyncStates(ctx, targetID)
	if err != nil {
		return SyncResult{}, err
	}
	stateByStream := make(map[string]store.TargetLedgerSyncState, len(states))
	for _, state := range states {
		stateByStream[state.Stream] = state
	}
	due := func(stream string) bool {
		state, ok := stateByStream[stream]
		return force || !ok || state.NextAttemptAt.IsZero() || !state.NextAttemptAt.After(started)
	}
	usersDue, logsDue, topupsDue, commissionsDue := due(streamUsers), due(streamLogs), due(streamTopups), due(streamCommissions)
	if !usersDue && !logsDue && !topupsDue && !commissionsDue {
		status, statusErr := s.Status(ctx, targetID)
		return SyncResult{TargetSiteID: targetID, Status: status, StartedAt: started, FinishedAt: time.Now().UTC()}, statusErr
	}
	credential, payload, err := s.loadCredential(ctx, target)
	if err != nil {
		for _, stream := range []string{streamUsers, streamLogs, streamTopups, streamCommissions} {
			if due(stream) {
				_ = s.recordFailure(ctx, config, targetID, stream, "auth_required", err, started, 0, 0)
			}
		}
		status, _ := s.Status(ctx, targetID)
		return SyncResult{TargetSiteID: targetID, Status: status, StartedAt: started, FinishedAt: time.Now().UTC()}, nil
	}
	_ = credential
	if (usersDue || logsDue || topupsDue || commissionsDue) && config.Currency == "" && config.QuotaCashFactor == "" {
		config = s.withDiscoveredUnitContract(ctx, target, config, started)
	}

	if usersDue {
		s.syncUsers(ctx, target, config, payload, stateByStream[streamUsers], started)
	}
	if logsDue {
		s.syncEvents(ctx, target, config, payload, stateByStream[streamLogs], streamLogs, started)
	}
	if topupsDue {
		s.syncEvents(ctx, target, config, payload, stateByStream[streamTopups], streamTopups, started)
	}
	if commissionsDue {
		s.syncCommissionEvents(ctx, target, config, payload, stateByStream[streamCommissions], started)
	}
	status, err := s.Status(ctx, targetID)
	return SyncResult{TargetSiteID: targetID, Status: status, StartedAt: started, FinishedAt: time.Now().UTC()}, err
}

func (s *Service) loadCredential(ctx context.Context, target domain.TargetSite) (domain.Credential, domain.CredentialPayload, error) {
	if target.CredentialID <= 0 || s.credentials == nil {
		return domain.Credential{}, domain.CredentialPayload{}, errors.New("target requires an encrypted management credential")
	}
	credential, payload, err := s.credentials.Load(ctx, target.CredentialID)
	if err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, err
	}
	if err := netguard.RequireSameOrigin(credential.SiteURL, target.BaseURL); err != nil {
		return domain.Credential{}, domain.CredentialPayload{}, fmt.Errorf("target credential origin mismatch: %w", err)
	}
	if strings.TrimSpace(payload.Authorization) == "" && strings.TrimSpace(payload.Cookie) == "" {
		return domain.Credential{}, domain.CredentialPayload{}, errors.New("management credential contains neither authorization nor session cookie")
	}
	return credential, payload, nil
}

func (s *Service) recordFailure(ctx context.Context, config store.TargetLedgerSyncConfig, targetID int64, stream, status string,
	err error, attemptedAt time.Time, scanned, skipped int) error {
	next := attemptedAt.Add(time.Duration(config.IntervalSeconds) * time.Second)
	if status == "unsupported" {
		next = attemptedAt.Add(6 * time.Hour)
	}
	return s.store.RecordTargetLedgerSyncFailure(ctx, targetID, stream, status, err.Error(), attemptedAt, next, scanned, skipped)
}

func (s *Service) SyncDue(ctx context.Context) (int, error) {
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		return 0, err
	}
	completed := 0
	var failures []error
	for _, target := range targets {
		if !target.Enabled {
			continue
		}
		config, configErr := s.Config(ctx, target.ID)
		if configErr != nil || !config.Enabled {
			if configErr != nil {
				failures = append(failures, fmt.Errorf("target %d config: %w", target.ID, configErr))
			}
			continue
		}
		if _, syncErr := s.SyncTarget(ctx, target.ID, false); syncErr != nil && !errors.Is(syncErr, ErrSyncBusy) {
			failures = append(failures, fmt.Errorf("target %d: %w", target.ID, syncErr))
			continue
		}
		completed++
	}
	return completed, errors.Join(failures...)
}

func (s *Service) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_, _ = s.SyncDue(ctx)
			timer.Reset(30 * time.Second)
		}
	}
}

func randomOwnerID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err == nil {
		return "ledger-sync-" + hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("ledger-sync-%d", time.Now().UnixNano())
}
