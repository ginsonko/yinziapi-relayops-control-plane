package intelligence

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

const (
	ledgerCacheSchemaVersion = 1
	ledgerCacheMaxBytes      = 128 << 20
	ledgerChangeBatchLimit   = 2000
	ledgerPersistInterval    = 5 * time.Minute
)

var errLedgerCacheLimit = errors.New("operating ledger cache capacity exceeded")

type ledgerCacheState struct {
	loaded            bool
	revision          int64
	generatedAt       time.Time
	lastPersisted     time.Time
	estimatedBytes    int64
	persistError      string
	persistInFlight   bool
	persistPending    bool
	projectionReady   bool
	persistedEvents   []store.OperatingLedgerEvent
	projectedEvents   []store.OperatingLedgerEvent
	projectedProfiles []store.TargetLedgerUserProfile
	ledgerSummary     store.LedgerSummary
	events            map[string]store.OperatingLedgerEvent
	profiles          map[string]store.TargetLedgerUserProfile
}

type persistedLedgerCache struct {
	SchemaVersion int                             `json:"schema_version"`
	DatabaseID    string                          `json:"database_id"`
	Revision      int64                           `json:"revision"`
	GeneratedAt   time.Time                       `json:"generated_at"`
	Events        []store.OperatingLedgerEvent    `json:"events"`
	Profiles      []store.TargetLedgerUserProfile `json:"profiles"`
}

type ledgerCacheView struct {
	Events         []store.OperatingLedgerEvent
	Profiles       []store.TargetLedgerUserProfile
	Ledger         store.LedgerSummary
	Revision       int64
	SourceRevision int64
	GeneratedAt    time.Time
	State          string
}

func (s *Service) ledgerData(ctx context.Context) (ledgerCacheView, error) {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	if !s.ledgerCache.loaded {
		loaded, err := s.loadPersistedLedgerCache()
		if err != nil {
			return ledgerCacheView{}, fmt.Errorf("load operating ledger cache: %w", err)
		}
		if !loaded {
			if err := s.buildLedgerCache(ctx); err != nil {
				return ledgerCacheView{}, err
			}
		}
	}

	sourceRevision, err := s.store.CurrentOperatingLedgerRevision(ctx)
	if err != nil {
		return ledgerCacheView{}, err
	}
	applied := 0
	if sourceRevision > s.ledgerCache.revision {
		applied, err = s.applyLedgerChanges(ctx)
		if err != nil {
			return ledgerCacheView{}, err
		}
	}
	if len(s.ledgerCache.events) > store.MaxOperatingLedgerCacheEvents || s.ledgerCache.estimatedBytes > ledgerCacheMaxBytes {
		return ledgerCacheView{}, fmt.Errorf("%w: %d events and estimated %d bytes", errLedgerCacheLimit,
			len(s.ledgerCache.events), s.ledgerCache.estimatedBytes)
	}
	if applied > 0 && (applied >= 100 || time.Since(s.ledgerCache.lastPersisted) >= ledgerPersistInterval) {
		s.scheduleLedgerCachePersistenceLocked()
	}
	view := ledgerCacheView{Revision: s.ledgerCache.revision, SourceRevision: sourceRevision,
		GeneratedAt: s.ledgerCache.generatedAt, State: "ready"}
	if s.ledgerCache.persistError != "" {
		view.State = "memory_only"
	}
	if view.Revision < view.SourceRevision {
		view.State = "catching_up"
	}
	if !s.ledgerCache.projectionReady {
		s.rebuildLedgerProjection()
	}
	view.Events = s.ledgerCache.projectedEvents
	view.Profiles = s.ledgerCache.projectedProfiles
	view.Ledger = s.ledgerCache.ledgerSummary
	return view, nil
}

func (s *Service) buildLedgerCache(ctx context.Context) error {
	snapshot, err := s.store.LoadOperatingLedgerCacheSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("build operating ledger cache: %w", err)
	}
	events := make(map[string]store.OperatingLedgerEvent, len(snapshot.Events))
	for _, event := range snapshot.Events {
		events[event.EventID] = compactLedgerEvent(event)
	}
	profiles := make(map[string]store.TargetLedgerUserProfile, len(snapshot.Profiles))
	for _, profile := range snapshot.Profiles {
		profiles[ledgerProfileKey(profile.TargetSiteID, profile.UserID)] = profile
	}
	if err := validateLedgerCacheCapacity(events, profiles); err != nil {
		return err
	}
	s.ledgerCache = ledgerCacheState{loaded: true, revision: snapshot.Revision, generatedAt: time.Now().UTC(),
		estimatedBytes: estimateLedgerCacheBytes(events, profiles), events: events, profiles: profiles}
	s.recordLedgerCachePersistence()
	return nil
}

func (s *Service) applyLedgerChanges(ctx context.Context) (int, error) {
	changes, err := s.store.ListOperatingLedgerChanges(ctx, s.ledgerCache.revision, ledgerChangeBatchLimit)
	if err != nil {
		return 0, err
	}
	if len(changes) == 0 {
		return 0, fmt.Errorf("operating ledger revision advanced without readable changes")
	}
	if changes[0].Revision != s.ledgerCache.revision+1 {
		return 0, fmt.Errorf("operating ledger change history has a gap after revision %d; explicit cache rebuild is required", s.ledgerCache.revision)
	}
	eventIDs := make([]string, 0, len(changes))
	profileTargets := map[int64]struct{}{}
	for _, change := range changes {
		switch change.ChangeKind {
		case "insert", "cost_update":
			eventIDs = append(eventIDs, change.EventID)
		case "profile_revision":
			profileTargets[change.TargetSiteID] = struct{}{}
		default:
			return 0, fmt.Errorf("unsupported operating ledger change kind %q", change.ChangeKind)
		}
	}
	changedEvents, err := s.store.ListOperatingLedgerEventsByIDs(ctx, eventIDs)
	if err != nil {
		return 0, err
	}
	changedByID := make(map[string]store.OperatingLedgerEvent, len(changedEvents))
	prospectiveBytes := s.ledgerCache.estimatedBytes
	prospectiveEvents := len(s.ledgerCache.events)
	for _, event := range changedEvents {
		event = compactLedgerEvent(event)
		if existing, ok := s.ledgerCache.events[event.EventID]; ok {
			prospectiveBytes -= estimateLedgerEventBytes(existing)
		} else {
			prospectiveEvents++
		}
		prospectiveBytes += estimateLedgerEventBytes(event)
		changedByID[event.EventID] = event
	}
	profilesByTarget := make(map[int64][]store.TargetLedgerUserProfile, len(profileTargets))
	for targetID := range profileTargets {
		profiles, err := s.store.ListTargetLedgerUserProfilesForTarget(ctx, targetID)
		if err != nil {
			return 0, err
		}
		profilesByTarget[targetID] = profiles
		for _, existing := range s.ledgerCache.profiles {
			if existing.TargetSiteID == targetID {
				prospectiveBytes -= estimateLedgerProfileBytes(existing)
			}
		}
		for _, profile := range profiles {
			prospectiveBytes += estimateLedgerProfileBytes(profile)
		}
	}
	if prospectiveEvents > store.MaxOperatingLedgerCacheEvents || prospectiveBytes > ledgerCacheMaxBytes {
		return 0, fmt.Errorf("%w: %d events and estimated %d bytes", errLedgerCacheLimit, prospectiveEvents, prospectiveBytes)
	}
	appendProjection := s.ledgerCache.projectionReady && len(profileTargets) == 0
	if appendProjection {
		for eventID, event := range changedByID {
			if _, exists := s.ledgerCache.events[eventID]; exists {
				appendProjection = false
				break
			}
			if len(s.ledgerCache.persistedEvents) > 0 && ledgerEventLess(event, s.ledgerCache.persistedEvents[len(s.ledgerCache.persistedEvents)-1]) {
				appendProjection = false
				break
			}
		}
	}
	for eventID, event := range changedByID {
		s.ledgerCache.events[eventID] = event
	}
	for targetID, profiles := range profilesByTarget {
		for key, profile := range s.ledgerCache.profiles {
			if profile.TargetSiteID == targetID {
				delete(s.ledgerCache.profiles, key)
			}
		}
		for _, profile := range profiles {
			s.ledgerCache.profiles[ledgerProfileKey(profile.TargetSiteID, profile.UserID)] = profile
		}
	}
	s.ledgerCache.estimatedBytes = prospectiveBytes
	s.ledgerCache.revision = changes[len(changes)-1].Revision
	s.ledgerCache.generatedAt = time.Now().UTC()
	if appendProjection {
		raw := make([]store.OperatingLedgerEvent, 0, len(changedByID))
		for _, event := range changedByID {
			raw = append(raw, event)
		}
		sort.Slice(raw, func(i, j int) bool { return ledgerEventLess(raw[i], raw[j]) })
		projected := store.ApplyTargetLedgerUserProfiles(raw, s.ledgerCache.projectedProfiles)
		s.ledgerCache.persistedEvents = append(s.ledgerCache.persistedEvents, raw...)
		s.ledgerCache.projectedEvents = append(s.ledgerCache.projectedEvents, projected...)
		extendLedgerSummary(&s.ledgerCache.ledgerSummary, store.SummarizeOperatingLedgerEvents(projected))
	} else {
		s.ledgerCache.projectionReady = false
	}
	return len(changes), nil
}

func ledgerEventLess(left, right store.OperatingLedgerEvent) bool {
	if !left.OccurredAt.Equal(right.OccurredAt) {
		return left.OccurredAt.Before(right.OccurredAt)
	}
	return left.EventID < right.EventID
}

func extendLedgerSummary(current *store.LedgerSummary, delta store.LedgerSummary) {
	current.EventCount += delta.EventCount
	current.ExactRevenueEvents += delta.ExactRevenueEvents
	current.ExactCostEvents += delta.ExactCostEvents
	current.EstimatedRevenueEvents += delta.EstimatedRevenueEvents
	current.EstimatedCostEvents += delta.EstimatedCostEvents
	if delta.EarliestAt != nil && (current.EarliestAt == nil || delta.EarliestAt.Before(*current.EarliestAt)) {
		copy := *delta.EarliestAt
		current.EarliestAt = &copy
	}
	if delta.LatestAt != nil && (current.LatestAt == nil || delta.LatestAt.After(*current.LatestAt)) {
		copy := *delta.LatestAt
		current.LatestAt = &copy
	}
}

func (s *Service) rebuildLedgerProjection() {
	profiles := make([]store.TargetLedgerUserProfile, 0, len(s.ledgerCache.profiles))
	for _, profile := range s.ledgerCache.profiles {
		profiles = append(profiles, profile)
	}
	rawEvents := make([]store.OperatingLedgerEvent, 0, len(s.ledgerCache.events))
	for _, event := range s.ledgerCache.events {
		rawEvents = append(rawEvents, event)
	}
	sort.Slice(rawEvents, func(i, j int) bool { return ledgerEventLess(rawEvents[i], rawEvents[j]) })
	projectedEvents := store.ApplyTargetLedgerUserProfiles(rawEvents, profiles)
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].TargetSiteID != profiles[j].TargetSiteID {
			return profiles[i].TargetSiteID < profiles[j].TargetSiteID
		}
		return profiles[i].UserID < profiles[j].UserID
	})
	s.ledgerCache.persistedEvents = rawEvents
	s.ledgerCache.projectedEvents = projectedEvents
	s.ledgerCache.projectedProfiles = profiles
	s.ledgerCache.ledgerSummary = store.SummarizeOperatingLedgerEvents(projectedEvents)
	s.ledgerCache.projectionReady = true
}

func compactLedgerEvent(event store.OperatingLedgerEvent) store.OperatingLedgerEvent {
	metadata := map[string]string{}
	for _, key := range []string{"financial_watermark_id", "financial_window_start", "financial_window_end",
		"financial_authoritative_event_count", "pricing_group", "pricing_model", "pricing_scope", "pricing_revision",
		"billing_mode", "task_billing_unit", "billing_unit", "task_fixed_duration", "cost_kind"} {
		if value := strings.TrimSpace(event.Metadata[key]); value != "" {
			metadata[key] = value
		}
	}
	event.Metadata = metadata
	event.PayloadHash = ""
	return event
}

func validateLedgerCacheCapacity(events map[string]store.OperatingLedgerEvent, profiles map[string]store.TargetLedgerUserProfile) error {
	if len(events) > store.MaxOperatingLedgerCacheEvents {
		return fmt.Errorf("%w: %d events exceeds %d", errLedgerCacheLimit, len(events), store.MaxOperatingLedgerCacheEvents)
	}
	estimated := estimateLedgerCacheBytes(events, profiles)
	if estimated > ledgerCacheMaxBytes {
		return fmt.Errorf("%w: estimated %d bytes exceeds %d", errLedgerCacheLimit, estimated, ledgerCacheMaxBytes)
	}
	return nil
}

func estimateLedgerCacheBytes(events map[string]store.OperatingLedgerEvent, profiles map[string]store.TargetLedgerUserProfile) int64 {
	var estimated int64
	for _, event := range events {
		estimated += estimateLedgerEventBytes(event)
	}
	for _, profile := range profiles {
		estimated += estimateLedgerProfileBytes(profile)
	}
	return estimated
}

func estimateLedgerEventBytes(event store.OperatingLedgerEvent) int64 {
	estimated := int64(320 + len(event.EventID) + len(event.EventKind) + len(event.ModelID) + len(event.GroupName) + len(event.UserSegment) +
		len(event.SubjectUserID) + len(event.SubjectTokenID) + len(event.SubjectRole) + len(event.RelatedEventID) + len(event.Amount) +
		len(event.UsageWeight) + len(event.UsageUnit) + len(event.CashRevenue) + len(event.ProcurementCost) + len(event.Currency) +
		len(event.Source) + len(event.EvidenceRevision))
	for key, value := range event.Metadata {
		estimated += int64(len(key) + len(value) + 32)
	}
	return estimated
}

func estimateLedgerProfileBytes(profile store.TargetLedgerUserProfile) int64 {
	return int64(256 + len(profile.UserID) + len(profile.Username) + len(profile.Role) + len(profile.UserSegment) +
		len(profile.AvailableQuota) + len(profile.CashBalance) + len(profile.Currency) + len(profile.QuotaCashFactor) + len(profile.SourceHash))
}

func ledgerProfileKey(targetID int64, userID string) string {
	return strconv.FormatInt(targetID, 10) + "\x00" + userID
}

func (s *Service) ledgerCachePath() string {
	return filepath.Join(filepath.Dir(s.store.DatabasePath()), "intelligence-ledger-cache-v1.json.gz")
}

func (s *Service) loadPersistedLedgerCache() (bool, error) {
	file, err := os.Open(s.ledgerCachePath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return false, err
	}
	defer reader.Close()
	var persisted persistedLedgerCache
	if err := json.NewDecoder(reader).Decode(&persisted); err != nil {
		return false, err
	}
	databaseID, err := s.store.OperatingLedgerCacheIdentity(context.Background())
	if err != nil {
		return false, err
	}
	if persisted.SchemaVersion != ledgerCacheSchemaVersion || persisted.DatabaseID != databaseID || len(persisted.Events) > store.MaxOperatingLedgerCacheEvents {
		return false, nil
	}
	events := make(map[string]store.OperatingLedgerEvent, len(persisted.Events))
	for _, event := range persisted.Events {
		if strings.TrimSpace(event.EventID) == "" {
			return false, nil
		}
		events[event.EventID] = compactLedgerEvent(event)
	}
	profiles := make(map[string]store.TargetLedgerUserProfile, len(persisted.Profiles))
	for _, profile := range persisted.Profiles {
		profiles[ledgerProfileKey(profile.TargetSiteID, profile.UserID)] = profile
	}
	if err := validateLedgerCacheCapacity(events, profiles); err != nil {
		return false, err
	}
	s.ledgerCache = ledgerCacheState{loaded: true, revision: persisted.Revision, generatedAt: persisted.GeneratedAt,
		lastPersisted: time.Now(), estimatedBytes: estimateLedgerCacheBytes(events, profiles), events: events, profiles: profiles}
	return true, nil
}

func (s *Service) persistLedgerCache() error {
	if !s.ledgerCache.loaded {
		return nil
	}
	databaseID, err := s.store.OperatingLedgerCacheIdentity(context.Background())
	if err != nil {
		return err
	}
	persisted := persistedLedgerCache{SchemaVersion: ledgerCacheSchemaVersion, DatabaseID: databaseID, Revision: s.ledgerCache.revision,
		GeneratedAt: s.ledgerCache.generatedAt, Events: make([]store.OperatingLedgerEvent, 0, len(s.ledgerCache.events)),
		Profiles: make([]store.TargetLedgerUserProfile, 0, len(s.ledgerCache.profiles))}
	for _, event := range s.ledgerCache.events {
		persisted.Events = append(persisted.Events, event)
	}
	for _, profile := range s.ledgerCache.profiles {
		persisted.Profiles = append(persisted.Profiles, profile)
	}
	if err := writePersistedLedgerCache(s.ledgerCachePath(), persisted); err != nil {
		return err
	}
	s.ledgerCache.lastPersisted = time.Now()
	return nil
}

func writePersistedLedgerCache(path string, persisted persistedLedgerCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".intelligence-cache-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	writer, err := gzip.NewWriterLevel(temporary, gzip.BestSpeed)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(writer).Encode(persisted); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Service) recordLedgerCachePersistence() {
	if err := s.persistLedgerCache(); err != nil {
		s.ledgerCache.persistError = err.Error()
		return
	}
	s.ledgerCache.persistError = ""
}

func (s *Service) scheduleLedgerCachePersistenceLocked() {
	if s.ledgerCache.persistInFlight {
		s.ledgerCache.persistPending = true
		return
	}
	if !s.ledgerCache.projectionReady {
		s.rebuildLedgerProjection()
	}
	databaseID, err := s.store.OperatingLedgerCacheIdentity(context.Background())
	if err != nil {
		s.ledgerCache.persistError = err.Error()
		return
	}
	persisted := persistedLedgerCache{SchemaVersion: ledgerCacheSchemaVersion, DatabaseID: databaseID,
		Revision: s.ledgerCache.revision, GeneratedAt: s.ledgerCache.generatedAt,
		Events: s.ledgerCache.persistedEvents, Profiles: s.ledgerCache.projectedProfiles}
	s.ledgerCache.persistInFlight = true
	s.ledgerCache.persistPending = false
	go s.persistLedgerCacheSnapshot(s.ledgerCachePath(), persisted)
}

func (s *Service) persistLedgerCacheSnapshot(path string, persisted persistedLedgerCache) {
	err := writePersistedLedgerCache(path, persisted)
	if err == nil {
		pruneContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.store.PruneOperatingLedgerChanges(pruneContext, persisted.Revision, 5000, 2000)
		cancel()
	}
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	s.ledgerCache.persistInFlight = false
	if err != nil {
		s.ledgerCache.persistError = err.Error()
		return
	}
	s.ledgerCache.persistError = ""
	s.ledgerCache.lastPersisted = time.Now()
	if s.ledgerCache.persistPending || s.ledgerCache.revision > persisted.Revision {
		s.scheduleLedgerCachePersistenceLocked()
	}
}
