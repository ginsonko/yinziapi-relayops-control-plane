package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

var (
	ErrAlreadyRunning = errors.New("supplier collection is already running")
	ErrCapacity       = errors.New("collector concurrency limit reached")
	sensitiveHeaderRE = regexp.MustCompile(`(?i)(authorization|cookie|x-api-key|x-token|api-key)\s*[:=]\s*(?:bearer\s+)?[^\s,;]+`)
	bearerTokenRE     = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`)
	sensitiveQueryRE  = regexp.MustCompile(`(?i)([?&](?:token|access_token|api_key|apikey|key|secret)=)[^&#\s]+`)
)

type afterTask struct {
	supplierID int64
}

type collectionFlight struct {
	done   chan struct{}
	result domain.CollectionResult
	err    error
}

type detailedConnector interface {
	CollectDetailed(context.Context, domain.Supplier) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error)
}

type Service struct {
	store   *store.Store
	factory *connectors.Factory
	ownerID string

	configMu       sync.RWMutex
	collectSlots   chan struct{}
	leaseTTL       time.Duration
	joinTimeout    time.Duration
	transientRetry time.Duration

	flightMu sync.Mutex
	flights  map[int64]*collectionFlight

	afterMu       sync.Mutex
	afterCollect  func(context.Context, int64) error
	afterPending  map[int64]struct{}
	afterDirty    map[int64]struct{}
	afterDeferred map[int64]int
	afterBusy     map[int64]int
	afterQueue    chan afterTask
	afterOnce     sync.Once
	afterWorkers  int
	afterTimeout  time.Duration

	authExpiredMu       sync.Mutex
	authExpiredHook     func(context.Context, int64) error
	authExpiredLast     map[int64]time.Time
	authExpiredPending  map[int64]struct{}
	authExpiredCooldown time.Duration
}

func NewService(st *store.Store, factory *connectors.Factory) *Service {
	return &Service{
		store: st, factory: factory, ownerID: newOwnerID(),
		collectSlots: make(chan struct{}, 4), leaseTTL: 3 * time.Minute, joinTimeout: 3 * time.Minute,
		transientRetry: time.Minute, flights: make(map[int64]*collectionFlight),
		afterPending: make(map[int64]struct{}), afterDirty: make(map[int64]struct{}),
		afterDeferred: make(map[int64]int), afterBusy: make(map[int64]int), afterQueue: make(chan afterTask, 128),
		afterWorkers: 2, afterTimeout: 2 * time.Minute,
		authExpiredLast: make(map[int64]time.Time), authExpiredPending: make(map[int64]struct{}), authExpiredCooldown: time.Minute,
	}
}

// ValidateCredential performs the connector's bounded, read-only capability
// probe without persisting or binding the supplied credential material.
func (s *Service) ValidateCredential(ctx context.Context, supplier domain.Supplier, kind string, payload domain.CredentialPayload) (domain.CollectionDiagnostics, error) {
	return s.factory.ValidateCredential(ctx, supplier, kind, payload)
}

// SetMaxConcurrent configures the one concurrency gate shared by scheduler,
// collect-all and direct collection calls. Call it during startup.
func (s *Service) SetMaxConcurrent(workers int) {
	if workers <= 0 {
		workers = 4
	}
	s.configMu.Lock()
	s.collectSlots = make(chan struct{}, workers)
	s.configMu.Unlock()
}

func (s *Service) SetLeaseTTL(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	s.configMu.Lock()
	s.leaseTTL = ttl
	s.configMu.Unlock()
}

func (s *Service) SetJoinTimeout(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	s.configMu.Lock()
	s.joinTimeout = timeout
	s.configMu.Unlock()
}

func (s *Service) SetTransientRetryDelay(delay time.Duration) {
	if delay <= 0 {
		return
	}
	s.configMu.Lock()
	s.transientRetry = delay
	s.configMu.Unlock()
}

// SetAuthExpiredHook connects explicit, credential-bound authentication
// failures to the credential lifecycle service. Collection results remain
// independent from renewal scheduling failures.
func (s *Service) SetAuthExpiredHook(hook func(context.Context, int64) error) {
	s.authExpiredMu.Lock()
	s.authExpiredHook = hook
	s.authExpiredMu.Unlock()
}

func (s *Service) SetAfterCollect(hook func(context.Context, int64)) {
	if hook == nil {
		s.SetAfterCollectWithError(nil)
		return
	}
	s.SetAfterCollectWithError(func(ctx context.Context, supplierID int64) error {
		hook(ctx, supplierID)
		return ctx.Err()
	})
}

// SetAfterCollectWithError allows hooks to report reconciliation failures.
// The legacy void hook is retained for existing callers.
func (s *Service) SetAfterCollectWithError(hook func(context.Context, int64) error) {
	s.afterMu.Lock()
	s.afterCollect = hook
	s.afterMu.Unlock()
	if hook != nil {
		s.startAfterWorkers()
	}
}

// CollectMany drains a collection batch with a fixed worker set. Callers can
// launch one background task for the batch without creating one goroutine per
// supplier. All work still passes through Collect's shared capacity and lease.
func (s *Service) CollectMany(ctx context.Context, supplierIDs []int64) {
	if len(supplierIDs) == 0 {
		return
	}
	s.configMu.RLock()
	workers := cap(s.collectSlots)
	s.configMu.RUnlock()
	if workers <= 0 {
		workers = 1
	}
	if workers > len(supplierIDs) {
		workers = len(supplierIDs)
	}
	jobs := make(chan int64)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for supplierID := range jobs {
				if ctx.Err() != nil {
					return
				}
				_, _ = s.collect(ctx, supplierID, true, false)
			}
		}()
	}
	seen := make(map[int64]struct{}, len(supplierIDs))
sendLoop:
	for _, supplierID := range supplierIDs {
		if _, duplicate := seen[supplierID]; duplicate {
			continue
		}
		seen[supplierID] = struct{}{}
		select {
		case jobs <- supplierID:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Service) Collect(ctx context.Context, supplierID int64) (domain.CollectionResult, error) {
	return s.collect(ctx, supplierID, false, false)
}

// CollectOrJoin waits for capacity and joins an already-running collection for
// the same supplier. It is the correct contract for operator refreshes and
// evidence confirmation, where "already running" is progress rather than an
// upstream failure.
func (s *Service) CollectOrJoin(ctx context.Context, supplierID int64) (domain.CollectionResult, error) {
	baselineID := s.latestAttemptID(ctx, supplierID)
	result, err := s.collect(ctx, supplierID, true, true)
	if !errors.Is(err, ErrAlreadyRunning) {
		return result, err
	}
	return s.waitForAttemptAfter(ctx, supplierID, baselineID)
}

func (s *Service) collect(ctx context.Context, supplierID int64, waitForCapacity, joinExisting bool) (result domain.CollectionResult, resultErr error) {
	flight, leader := s.beginCollectionFlight(supplierID)
	if !leader {
		if !joinExisting {
			return domain.CollectionResult{}, ErrAlreadyRunning
		}
		return s.waitForCollectionFlight(ctx, flight)
	}
	defer func() { s.finishCollectionFlight(supplierID, flight, result, resultErr) }()

	slots, acquired := s.acquireCapacity(ctx, waitForCapacity)
	if !acquired {
		if ctx.Err() != nil {
			return domain.CollectionResult{}, ctx.Err()
		}
		return domain.CollectionResult{}, ErrCapacity
	}
	defer func() { <-slots }()

	ttl := s.currentLeaseTTL()
	claimed, err := s.store.TryAcquireCollectionLease(ctx, supplierID, s.ownerID, time.Now().UTC(), ttl)
	if err != nil {
		return domain.CollectionResult{}, err
	}
	if !claimed {
		return domain.CollectionResult{}, ErrAlreadyRunning
	}
	ctx, cancelCollection := context.WithCancel(ctx)
	stopRenewal := s.keepLeaseAlive(supplierID, ttl, cancelCollection)
	defer func() {
		stopRenewal()
		cancelCollection()
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.ReleaseCollectionLease(releaseCtx, supplierID, s.ownerID)
	}()

	supplier, err := s.store.GetSupplier(ctx, supplierID)
	if err != nil {
		return domain.CollectionResult{}, err
	}
	started := time.Now().UTC()
	diagnostics := domain.CollectionDiagnostics{Requests: []domain.RequestEvidence{}, Capabilities: []domain.AuthCapabilityEvidence{}}
	previousHash := ""
	var previousSnapshot *domain.CatalogSnapshot
	if previous, previousErr := s.store.LatestSnapshot(ctx, supplierID); previousErr == nil {
		previousHash = previous.Evidence.ResponseHash
		copy := previous
		previousSnapshot = &copy
	} else if !store.IsNotFound(previousErr) {
		s.recordFailure(ctx, supplier, started, diagnostics, previousErr)
		return domain.CollectionResult{}, previousErr
	}

	connector, detectedAdapter, err := s.factory.ForSupplier(ctx, supplier)
	if err != nil {
		diagnostics.Requests = append(diagnostics.Requests, requestFailureEvidence(supplier, "adapter_detection", "anonymous", started, err))
		s.recordFailure(ctx, supplier, started, diagnostics, err)
		return domain.CollectionResult{}, err
	}
	if supplier.Adapter == "auto" && detectedAdapter != "" {
		supplier.Adapter = detectedAdapter
		_ = s.store.UpdateSupplier(ctx, supplier)
	}

	var snapshot domain.CatalogSnapshot
	if detailed, ok := connector.(detailedConnector); ok {
		snapshot, diagnostics, err = detailed.CollectDetailed(ctx, supplier)
	} else {
		snapshot, err = connector.Collect(ctx, supplier)
		finished := time.Now().UTC()
		status := "success"
		if err != nil {
			status = "failed"
		}
		diagnostics = domain.CollectionDiagnostics{
			CatalogSucceeded: err == nil,
			BalanceSucceeded: err == nil && snapshot.Balance != nil,
			Requests: []domain.RequestEvidence{{SupplierID: supplier.ID, EndpointType: "collection", AuthCapability: "unknown",
				Method: "GET", SourceURL: supplier.BaseURL, Status: status, ErrorClass: classifyCollectionError(err),
				ErrorMessage: safeCollectionError(err), DurationMS: finished.Sub(started).Milliseconds(), ObservedAt: finished}},
			Capabilities: []domain.AuthCapabilityEvidence{},
		}
	}
	s.signalAuthExpired(ctx, supplier, diagnostics)
	if err != nil {
		// Some authenticated suppliers expose balance independently from their
		// catalog. Preserve that trusted observation and its request evidence
		// without moving the last known-good price directory.
		if snapshot.Balance != nil {
			finished := time.Now().UTC()
			errorClass := classifyCollectionError(err)
			message := safeCollectionError(err)
			if status, diagnosticClass, diagnosticMessage := collectionOutcome(diagnostics); status == "partial_success" {
				if diagnosticClass != "" {
					errorClass = diagnosticClass
				}
				if diagnosticMessage != "" {
					message = diagnosticMessage
				}
			}
			attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: "partial_success",
				ErrorClass: errorClass, ErrorMessage: message, CatalogUpdated: false, BalanceUpdated: true,
				StartedAt: started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds()}
			next := finished.Add(s.collectionRetryInterval(supplier, errorClass, finished))
			if commitErr := s.store.CommitCollectionPartial(ctx, supplier.ID, *snapshot.Balance, &attempt, diagnostics, next); commitErr != nil {
				s.recordFailure(ctx, supplier, started, diagnostics, commitErr)
				return domain.CollectionResult{}, fmt.Errorf("commit partial collection result: %w", commitErr)
			}
			updated, _ := s.store.GetSupplier(ctx, supplier.ID)
			return domain.CollectionResult{Supplier: updated, Snapshot: snapshot, Attempt: attempt,
				Diagnostics: diagnostics, Changed: false, Duration: attempt.DurationMS}, nil
		}
		s.recordFailure(ctx, supplier, started, diagnostics, err)
		return domain.CollectionResult{}, err
	}
	if snapshot.SupplierID == 0 {
		snapshot.SupplierID = supplier.ID
	}
	if snapshot.Evidence.ObservedAt.IsZero() {
		snapshot.Evidence.ObservedAt = time.Now().UTC()
	}
	catalogDiff := summarizeCatalogDiff(previousSnapshot, snapshot)
	finished := time.Now().UTC()
	next := finished.Add(jitteredInterval(intervalFor(supplier), supplier.ID, finished))
	attemptStatus, attemptClass, attemptMessage := collectionOutcome(diagnostics)
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: attemptStatus, ErrorClass: attemptClass,
		ErrorMessage: attemptMessage, CatalogUpdated: true,
		BalanceUpdated: snapshot.Balance != nil, StartedAt: started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds()}
	changed, err := s.store.CommitCollectionSuccess(ctx, snapshot, &attempt, diagnostics, next)
	if err != nil {
		s.recordFailure(ctx, supplier, started, diagnostics, err)
		return domain.CollectionResult{}, fmt.Errorf("commit collection result: %w", err)
	}
	if changed {
		affectedPolicies := s.catalogAffectedPolicyCount(ctx, supplier.ID, catalogDiff)
		_ = s.store.AddActivityCoalesced(ctx, catalogDiff.activity(supplier, previousHash, snapshot.Evidence.ResponseHash, affectedPolicies), 10*time.Minute)
	}
	updated, _ := s.store.GetSupplier(ctx, supplier.ID)
	// A response hash can change because of ordering or presentation only. Do
	// not read the target or re-plan every mirror unless the catalog semantics
	// changed, this is the first trusted catalog, or a previous deferred
	// reconciliation is waiting for the next normal collection.
	if previousSnapshot == nil || catalogDiff.semanticChanged() || s.afterReconcilePending(supplier.ID) {
		s.enqueueAfterCollect(supplier.ID)
	}
	return domain.CollectionResult{
		Supplier: updated, Snapshot: snapshot, Attempt: attempt, Diagnostics: diagnostics,
		Changed: changed, Duration: attempt.DurationMS,
	}, nil
}

func (s *Service) catalogAffectedPolicyCount(ctx context.Context, supplierID int64, diff catalogDiffSummary) int {
	if !diff.semanticChanged() {
		return 0
	}
	policies, err := s.store.ListPolicies(ctx)
	if err != nil {
		return 0
	}
	count := 0
	for _, policy := range policies {
		if policy.SupplierID == supplierID && diff.affectsGroup(policy.SourceGroup) {
			count++
		}
	}
	return count
}

func (s *Service) beginCollectionFlight(supplierID int64) (*collectionFlight, bool) {
	s.flightMu.Lock()
	defer s.flightMu.Unlock()
	if flight := s.flights[supplierID]; flight != nil {
		return flight, false
	}
	flight := &collectionFlight{done: make(chan struct{})}
	s.flights[supplierID] = flight
	return flight, true
}

func (s *Service) finishCollectionFlight(supplierID int64, flight *collectionFlight, result domain.CollectionResult, err error) {
	s.flightMu.Lock()
	flight.result, flight.err = result, err
	if s.flights[supplierID] == flight {
		delete(s.flights, supplierID)
	}
	close(flight.done)
	s.flightMu.Unlock()
}

func (s *Service) waitForCollectionFlight(ctx context.Context, flight *collectionFlight) (domain.CollectionResult, error) {
	joinCtx, cancel := s.joinContext(ctx)
	defer cancel()
	select {
	case <-joinCtx.Done():
		return domain.CollectionResult{}, fmt.Errorf("wait for running supplier collection: %w", joinCtx.Err())
	case <-flight.done:
		return flight.result, flight.err
	}
}

func (s *Service) waitForAttemptAfter(ctx context.Context, supplierID, baselineID int64) (domain.CollectionResult, error) {
	joinCtx, cancel := s.joinContext(ctx)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		attempt, err := s.store.LatestCollectionAttempt(joinCtx, supplierID)
		if err == nil && attempt.ID > baselineID {
			return s.resultFromJoinedAttempt(joinCtx, supplierID, attempt)
		}
		if err != nil && !store.IsNotFound(err) {
			return domain.CollectionResult{}, err
		}
		select {
		case <-joinCtx.Done():
			return domain.CollectionResult{}, fmt.Errorf("wait for leased supplier collection: %w", joinCtx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Service) resultFromJoinedAttempt(ctx context.Context, supplierID int64, attempt domain.CollectionAttempt) (domain.CollectionResult, error) {
	if attempt.Status != "success" && attempt.Status != "partial_success" {
		message := strings.TrimSpace(attempt.ErrorMessage)
		if message == "" {
			message = "joined collection finished with status " + attempt.Status
		}
		return domain.CollectionResult{Attempt: attempt, Duration: attempt.DurationMS}, errors.New(message)
	}
	supplier, err := s.store.GetSupplier(ctx, supplierID)
	if err != nil {
		return domain.CollectionResult{}, err
	}
	snapshot, snapshotErr := s.store.LatestEffectiveSnapshot(ctx, supplierID)
	if snapshotErr != nil && attempt.CatalogUpdated {
		return domain.CollectionResult{}, snapshotErr
	}
	return domain.CollectionResult{Supplier: supplier, Snapshot: snapshot, Attempt: attempt,
		Diagnostics: domain.CollectionDiagnostics{Requests: attempt.Requests}, Duration: attempt.DurationMS}, nil
}

func (s *Service) latestAttemptID(ctx context.Context, supplierID int64) int64 {
	attempt, err := s.store.LatestCollectionAttempt(ctx, supplierID)
	if err != nil {
		return 0
	}
	return attempt.ID
}

func (s *Service) joinContext(ctx context.Context) (context.Context, context.CancelFunc) {
	s.configMu.RLock()
	timeout := s.joinTimeout
	s.configMu.RUnlock()
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Service) signalAuthExpired(ctx context.Context, supplier domain.Supplier, diagnostics domain.CollectionDiagnostics) {
	credentialID, ok := authExpiredCredentialID(supplier, diagnostics)
	if !ok {
		return
	}

	now := time.Now().UTC()
	s.authExpiredMu.Lock()
	hook := s.authExpiredHook
	if hook == nil {
		s.authExpiredMu.Unlock()
		return
	}
	if _, pending := s.authExpiredPending[credentialID]; pending {
		s.authExpiredMu.Unlock()
		return
	}
	if last := s.authExpiredLast[credentialID]; !last.IsZero() && now.Sub(last) < s.authExpiredCooldown {
		s.authExpiredMu.Unlock()
		return
	}
	// Reserve the cooldown before calling the hook. This suppresses concurrent
	// signals when several suppliers share one credential, even if renewal is
	// not configured or the local scheduling write temporarily fails.
	s.authExpiredLast[credentialID] = now
	s.authExpiredPending[credentialID] = struct{}{}
	s.authExpiredMu.Unlock()

	signalCtx := ctx
	var cancel context.CancelFunc
	if signalCtx == nil || signalCtx.Err() != nil {
		signalCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	_ = hook(signalCtx, credentialID)

	s.authExpiredMu.Lock()
	delete(s.authExpiredPending, credentialID)
	s.authExpiredMu.Unlock()
}

func authExpiredCredentialID(supplier domain.Supplier, diagnostics domain.CollectionDiagnostics) (int64, bool) {
	if supplier.CredentialID == nil || *supplier.CredentialID <= 0 {
		return 0, false
	}
	expired := false
	for _, capability := range diagnostics.Capabilities {
		if capability.CredentialID == nil || *capability.CredentialID != *supplier.CredentialID {
			continue
		}
		// Any verified bound capability proves the credential is still accepted.
		// A 401 from another role or optional endpoint is a capability mismatch,
		// not evidence that the complete credential has expired.
		if capability.Status == "verified" && verifiedCapabilityProvesCredential(diagnostics, capability) {
			return 0, false
		}
		if capability.Status == "expired_or_invalid" && capability.ErrorClass == "auth_expired" {
			expired = true
		}
	}
	if expired {
		return *supplier.CredentialID, true
	}
	return 0, false
}

func verifiedCapabilityProvesCredential(diagnostics domain.CollectionDiagnostics, capability domain.AuthCapabilityEvidence) bool {
	// A pricing catalog that produced the same successful response without a
	// credential does not authenticate the bound session. This is common on
	// NewAPI and must not suppress renewal after its private balance endpoint
	// returns 401.
	if capability.EndpointType != "catalog" {
		return true
	}
	for _, authenticated := range diagnostics.Requests {
		if authenticated.EndpointType != capability.EndpointType || authenticated.AuthCapability != capability.Capability ||
			authenticated.Status != "success" || authenticated.HTTPStatus < 200 || authenticated.HTTPStatus >= 300 {
			continue
		}
		for _, anonymous := range diagnostics.Requests {
			if anonymous.EndpointType == "catalog" && (anonymous.AuthCapability == "anonymous" || anonymous.AuthCapability == "public_catalog") &&
				anonymous.Status == "success" && anonymous.HTTPStatus >= 200 && anonymous.HTTPStatus < 300 &&
				anonymous.SourceURL == authenticated.SourceURL && anonymous.ResponseHash != "" && anonymous.ResponseHash == authenticated.ResponseHash {
				return false
			}
		}
		return true
	}
	return true
}

func collectionOutcome(diagnostics domain.CollectionDiagnostics) (status, errorClass, message string) {
	successfulEndpoints := make(map[string]bool)
	authenticatedCatalogSucceeded := false
	for _, request := range diagnostics.Requests {
		if request.Status != "success" || request.HTTPStatus < 200 || request.HTTPStatus >= 300 {
			continue
		}
		successfulEndpoints[request.EndpointType] = true
		if request.EndpointType == "catalog" && request.AuthCapability != "anonymous" && request.AuthCapability != "public_catalog" {
			authenticatedCatalogSucceeded = true
		}
	}
	for _, request := range diagnostics.Requests {
		if request.Status == "success" && request.HTTPStatus >= 200 && request.HTTPStatus < 300 {
			continue
		}
		if request.Status == "" && request.ErrorClass == "" && request.HTTPStatus == 0 {
			continue
		}
		// Capability negotiation can try several bounded endpoints before one
		// succeeds. Keep every failed request as evidence, but do not let an
		// earlier candidate overwrite the final capability outcome.
		if successfulEndpoints[request.EndpointType] {
			continue
		}
		if request.EndpointType == "catalog" && (request.AuthCapability == "anonymous" || request.AuthCapability == "public_catalog") && authenticatedCatalogSucceeded {
			continue
		}
		if request.EndpointType == "catalog" && diagnostics.CatalogSucceeded {
			continue
		}
		if request.EndpointType == "balance" && diagnostics.BalanceSucceeded {
			continue
		}
		// Daily usage is an optional enrichment of a trusted balance. Sites
		// that do not expose it must not turn a valid catalog and balance red.
		if request.EndpointType == "usage" && diagnostics.CatalogSucceeded && diagnostics.BalanceSucceeded {
			continue
		}
		// The public status endpoint is only needed to normalize quota units.
		// A successfully normalized balance proves that the missing status
		// candidate did not compromise the current result.
		if request.EndpointType == "status" && diagnostics.BalanceSucceeded {
			continue
		}
		// Customer-facing catalog and balance are the collection contract. Some
		// upstreams expose extra admin endpoints as bounded fallbacks; rejecting
		// those must not turn a fully verified customer session yellow.
		if request.AuthCapability == "site_admin" && diagnostics.CatalogSucceeded && diagnostics.BalanceSucceeded {
			continue
		}
		errorClass = request.ErrorClass
		if errorClass == "" {
			errorClass = "partial_success"
		}
		message = request.ErrorMessage
		if message == "" {
			message = request.EndpointType + " was not updated"
		}
		return "partial_success", errorClass, message
	}
	return "success", "", ""
}

func (s *Service) recordFailure(ctx context.Context, supplier domain.Supplier, started time.Time, diagnostics domain.CollectionDiagnostics, collectErr error) {
	now := time.Now().UTC()
	recordCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		recordCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	errorClass := classifyCollectionError(collectErr)
	// Authentication and rate-limit failures keep their normal polling contract:
	// credential renewal and upstream Retry-After semantics own those paths.
	// Transport and 5xx failures get one bounded short recovery opportunity so a
	// transient local DNS/TLS window is not stretched to the full poll interval.
	retry := s.collectionRetryInterval(supplier, errorClass, now)
	message := safeCollectionError(collectErr)
	status := "failed"
	if errors.Is(collectErr, context.Canceled) {
		status = "cancelled"
	}
	attempt := domain.CollectionAttempt{SupplierID: supplier.ID, Status: status, ErrorClass: errorClass,
		ErrorMessage: message, CatalogUpdated: false, BalanceUpdated: false, StartedAt: started,
		FinishedAt: now, DurationMS: now.Sub(started).Milliseconds()}
	_ = s.store.RecordCollectionAttempt(recordCtx, &attempt, diagnostics, now.Add(retry))
	// Upstream/network jitter is expected operational noise. It remains in the
	// collection-attempt history for retry scheduling and diagnosis, but never
	// enters the user activity stream regardless of repetition.
	if isTransientCollectionClass(errorClass) || errorClass == "rate_limited" {
		return
	}
	// SQLite busy is different: the transaction already retried locally, so a
	// repeated busy condition is actionable and is surfaced only after the
	// bounded consecutive-failure threshold.
	if errorClass == "database_busy" {
		failures, countErr := s.store.ConsecutiveCollectionBusyFailures(recordCtx, supplier.ID, 3)
		if countErr != nil || failures < 3 {
			return
		}
	}
	id := supplier.ID
	_ = s.store.AddActivityCoalesced(recordCtx, domain.Activity{
		Kind: "collection_failed", Severity: "warning", Title: supplier.Name + " 采集暂时失败",
		Message: message, SupplierID: &id, Metadata: map[string]string{"error_class": errorClass},
	}, 6*time.Hour)
}

func (s *Service) collectionRetryInterval(supplier domain.Supplier, errorClass string, observedAt time.Time) time.Duration {
	regular := intervalFor(supplier)
	if !isTransientCollectionClass(errorClass) {
		return regular
	}
	s.configMu.RLock()
	short := s.transientRetry
	s.configMu.RUnlock()
	if short <= 0 {
		short = time.Minute
	}
	if regular <= short {
		return regular
	}
	return jitteredInterval(short, supplier.ID, observedAt)
}

func isTransientCollectionClass(errorClass string) bool {
	switch strings.TrimSpace(errorClass) {
	case "dns_error", "tls_error", "connection_reset", "connect_timeout", "read_timeout", "upstream_5xx":
		return true
	default:
		return false
	}
}

func requestFailureEvidence(supplier domain.Supplier, endpointType, capability string, started time.Time, err error) domain.RequestEvidence {
	finished := time.Now().UTC()
	return domain.RequestEvidence{SupplierID: supplier.ID, EndpointType: endpointType, AuthCapability: capability,
		Method: "GET", SourceURL: supplier.BaseURL, Status: "failed", ErrorClass: classifyCollectionError(err),
		ErrorMessage: safeCollectionError(err), DurationMS: finished.Sub(started).Milliseconds(), ObservedAt: finished}
}

func classifyCollectionError(err error) string {
	if err == nil {
		return ""
	}
	type errorClassifier interface{ ErrorClass() string }
	var classified errorClassifier
	if errors.As(err, &classified) && strings.TrimSpace(classified.ErrorClass()) != "" {
		return strings.TrimSpace(classified.ErrorClass())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "read_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "connect_timeout"
	}
	text := strings.ToLower(err.Error())
	for _, candidate := range []struct {
		contains string
		class    string
	}{
		{"database is locked", "database_busy"}, {"database table is locked", "database_busy"},
		{"sqlite_busy", "database_busy"}, {"sqlite_locked", "database_busy"},
		{"401", "auth_expired"}, {"unauthorized", "auth_expired"}, {"credential", "auth_missing"},
		{"403", "permission_denied"}, {"forbidden", "permission_denied"}, {"429", "rate_limited"},
		{"rate limit", "rate_limited"}, {"connection reset", "connection_reset"}, {"tls", "tls_error"},
		{"certificate", "tls_error"}, {"no such host", "dns_error"}, {"server misbehaving", "dns_error"},
		{"timeout", "read_timeout"}, {"deadline", "read_timeout"}, {"contract", "contract_changed"},
		{"schema", "contract_changed"}, {"unit", "invalid_unit"}, {"500", "upstream_5xx"},
		{"502", "upstream_5xx"}, {"503", "upstream_5xx"}, {"504", "upstream_5xx"},
	} {
		if strings.Contains(text, candidate.contains) {
			return candidate.class
		}
	}
	return "unknown_error"
}

func safeCollectionError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\r", " "), "\n", " "))
	value = sensitiveHeaderRE.ReplaceAllString(value, "$1=[redacted]")
	value = bearerTokenRE.ReplaceAllString(value, "Bearer [redacted]")
	value = sensitiveQueryRE.ReplaceAllString(value, "$1[redacted]")
	if len(value) > 1200 {
		value = value[:1200]
	}
	return value
}

func (s *Service) acquireCapacity(ctx context.Context, wait bool) (chan struct{}, bool) {
	s.configMu.RLock()
	slots := s.collectSlots
	s.configMu.RUnlock()
	if wait {
		select {
		case slots <- struct{}{}:
			return slots, true
		case <-ctx.Done():
			return slots, false
		}
	}
	select {
	case slots <- struct{}{}:
		return slots, true
	default:
		return slots, false
	}
}

func (s *Service) currentLeaseTTL() time.Duration {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.leaseTTL
}

func (s *Service) keepLeaseAlive(supplierID int64, ttl time.Duration, cancelCollection context.CancelFunc) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		interval := ttl / 3
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, 5*time.Second)
				ok, err := s.store.RenewCollectionLease(renewCtx, supplierID, s.ownerID, now.UTC(), ttl)
				renewCancel()
				if err != nil || !ok {
					if ctx.Err() != nil {
						return
					}
					cancelCollection()
					id := supplierID
					message := "collection lease was lost"
					if err != nil {
						message = err.Error()
					}
					activityCtx, activityCancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = s.store.AddActivity(activityCtx, domain.Activity{Kind: "collection_lease_lost", Severity: "warning",
						Title: "采集租约续期失败", Message: message, SupplierID: &id})
					activityCancel()
					return
				}
			}
		}
	}()
	return cancel
}

func (s *Service) enqueueAfterCollect(supplierID int64) {
	s.afterMu.Lock()
	if s.afterCollect == nil {
		s.afterMu.Unlock()
		return
	}
	if _, pending := s.afterPending[supplierID]; pending {
		s.afterDirty[supplierID] = struct{}{}
		s.afterMu.Unlock()
		return
	}
	s.afterPending[supplierID] = struct{}{}
	s.afterMu.Unlock()

	select {
	case s.afterQueue <- afterTask{supplierID: supplierID}:
	default:
		s.afterMu.Lock()
		delete(s.afterPending, supplierID)
		delete(s.afterDirty, supplierID)
		s.afterMu.Unlock()
		s.recordAfterFailure(supplierID, "采集后任务队列已满", "本次后台重算未入队，将在下次采集后重试。")
	}
}

func (s *Service) startAfterWorkers() {
	s.afterOnce.Do(func() {
		for i := 0; i < s.afterWorkers; i++ {
			go s.runAfterWorker()
		}
	})
}

func (s *Service) runAfterWorker() {
	for task := range s.afterQueue {
		s.afterMu.Lock()
		hook := s.afterCollect
		timeout := s.afterTimeout
		s.afterMu.Unlock()
		var hookErr error
		if hook != nil {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			hookErr = callAfterHook(ctx, hook, task.supplierID)
			cancel()
		}
		recordFailure := false
		s.afterMu.Lock()
		if hookErr == nil {
			delete(s.afterDeferred, task.supplierID)
			delete(s.afterBusy, task.supplierID)
		} else {
			category := afterCollectFailureClass(hookErr.Error())
			if afterCollectDeferredOnly(hookErr) || isSilentAfterCollectClass(category) {
				s.afterDeferred[task.supplierID] = 1
				delete(s.afterBusy, task.supplierID)
			} else if category == "database_busy" {
				s.afterDeferred[task.supplierID] = 1
				s.afterBusy[task.supplierID]++
				recordFailure = s.afterBusy[task.supplierID] >= 3
			} else {
				delete(s.afterDeferred, task.supplierID)
				delete(s.afterBusy, task.supplierID)
				recordFailure = true
			}
		}
		s.afterMu.Unlock()
		if recordFailure && !afterFailureAlreadyRecorded(hookErr) {
			s.recordAfterFailure(task.supplierID, "采集后重算失败", hookErr.Error())
		}
		s.afterMu.Lock()
		if _, dirty := s.afterDirty[task.supplierID]; dirty {
			delete(s.afterDirty, task.supplierID)
			s.afterMu.Unlock()
			select {
			case s.afterQueue <- task:
			default:
				s.afterMu.Lock()
				delete(s.afterPending, task.supplierID)
				s.afterMu.Unlock()
				s.recordAfterFailure(task.supplierID, "采集后任务队列已满", "最新一次目录变更未能重新入队，将在下次采集后重试。")
			}
			continue
		}
		delete(s.afterPending, task.supplierID)
		s.afterMu.Unlock()
	}
}

func (s *Service) afterReconcilePending(supplierID int64) bool {
	s.afterMu.Lock()
	defer s.afterMu.Unlock()
	return s.afterDeferred[supplierID] > 0
}

type afterCollectDeferredError interface {
	AfterCollectDeferred() bool
}

// afterCollectDeferredOnly follows wrapped and joined errors without letting
// one deferred branch hide an unrelated real failure.
func afterCollectDeferredOnly(err error) bool {
	if err == nil {
		return false
	}
	if marker, ok := err.(afterCollectDeferredError); ok {
		return marker.AfterCollectDeferred()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !afterCollectDeferredOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return afterCollectDeferredOnly(wrapped.Unwrap())
	}
	return false
}

func afterFailureAlreadyRecorded(err error) bool {
	if err == nil {
		return false
	}
	lines := strings.Split(err.Error(), "\n")
	found := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		found = true
		if !strings.HasPrefix(line, "legacy pricing policy ") && !strings.HasPrefix(line, "legacy pricing target ") {
			return false
		}
	}
	return found
}

func (s *Service) recordAfterFailure(supplierID int64, title, message string) {
	id := supplierID
	activityCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	category := afterCollectFailureClass(message)
	if isSilentAfterCollectClass(category) {
		cancel()
		return
	}
	_ = s.store.AddActivityCoalesced(activityCtx, domain.Activity{Kind: "after_collect_failed", Severity: "warning",
		Title: title, Message: message, SupplierID: &id, Metadata: map[string]string{
			"error_class": category, "automation_failure_key": fmt.Sprintf("after-collect:%d:%s:%s", supplierID, title, category),
		}}, 30*time.Minute)
	cancel()
}

func afterCollectFailureClass(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "database is locked"), strings.Contains(lower, "database table is locked"),
		strings.Contains(lower, "sqlite_busy"), strings.Contains(lower, "sqlite_locked"):
		return "database_busy"
	case strings.Contains(lower, "source evidence is pending"), strings.Contains(lower, "proof is incomplete"),
		strings.Contains(lower, "proof is not bound"):
		return "evidence_pending"
	case strings.Contains(lower, "429"), strings.Contains(lower, "too many requests"), strings.Contains(lower, "rate limit"):
		return "rate_limited"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "no such host"), strings.Contains(lower, "name resolution"), strings.Contains(lower, "dns"):
		return "dns_error"
	case strings.Contains(lower, "tls"), strings.Contains(lower, "x509"), strings.Contains(lower, "handshake"):
		return "tls_error"
	case strings.Contains(lower, "connection reset"), strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "unexpected eof"), strings.Contains(lower, "network is unreachable"):
		return "connection_error"
	case strings.Contains(lower, "status 500"), strings.Contains(lower, "status 502"),
		strings.Contains(lower, "status 503"), strings.Contains(lower, "status 504"),
		strings.Contains(lower, "bad gateway"), strings.Contains(lower, "service unavailable"):
		return "upstream_5xx"
	case strings.Contains(lower, "401"), strings.Contains(lower, "unauthorized"):
		return "authorization"
	case strings.Contains(lower, "403"), strings.Contains(lower, "permission"):
		return "permission_denied"
	default:
		return "reconciliation_failed"
	}
}

func isSilentAfterCollectClass(class string) bool {
	switch class {
	case "timeout", "rate_limited", "evidence_pending", "dns_error", "tls_error", "connection_error", "upstream_5xx":
		return true
	default:
		return false
	}
}

func callAfterHook(ctx context.Context, hook func(context.Context, int64) error, supplierID int64) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("after-collect hook panicked: %v", recovered)
		}
	}()
	return hook(ctx, supplierID)
}

func intervalFor(supplier domain.Supplier) time.Duration {
	seconds := supplier.PollIntervalSeconds
	if seconds <= 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func jitteredInterval(base time.Duration, supplierID int64, observedAt time.Time) time.Duration {
	if base <= 0 {
		return base
	}
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "%d:%d", supplierID, observedAt.Unix()/60)
	offsetBasisPoints := int64(hash.Sum32()%2001) - 1000
	offset := time.Duration(int64(base) * offsetBasisPoints / 10000)
	return base + offset
}

func IsAlreadyRunning(err error) bool {
	return errors.Is(err, ErrAlreadyRunning)
}

func newOwnerID() string {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	return fmt.Sprintf("collector-%d", time.Now().UnixNano())
}
