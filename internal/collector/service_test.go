package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/credentials"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/secrets"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestCollectManyUsesBoundedSharedConcurrency(t *testing.T) {
	t.Parallel()
	var active, maximum, requests atomic.Int32
	startedTwo := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		if requests.Add(1) == 2 {
			startedOnce.Do(func() { close(startedTwo) })
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	st := openCollectorStore(t, "bounded.db")
	service := NewService(st, connectors.NewFactory(upstream.Client(), nil))
	service.SetMaxConcurrent(2)
	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		supplier := domain.Supplier{Name: fmt.Sprintf("supplier-%d", i), BaseURL: fmt.Sprintf("%s?supplier=%d", upstream.URL, i), Adapter: "newapi", Enabled: true}
		if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, supplier.ID)
	}
	done := make(chan struct{})
	go func() {
		service.CollectMany(context.Background(), ids)
		close(done)
	}()
	select {
	case <-startedTwo:
	case <-time.After(3 * time.Second):
		t.Fatal("two collection workers did not start")
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent requests = %d, want 2", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collection batch did not finish")
	}
	if got := requests.Load(); got != int32(len(ids)) {
		t.Fatalf("requests = %d, want %d", got, len(ids))
	}
	if got := maximum.Load(); got > 2 {
		t.Fatalf("concurrency exceeded limit: %d", got)
	}
}

func TestCollectUsesDatabaseLeaseAcrossServiceInstances(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "shared.db")
	blue, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blue.Close()
	green, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer green.Close()
	supplier := domain.Supplier{Name: "shared", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := blue.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	first := NewService(blue, connectors.NewFactory(upstream.Client(), nil))
	second := NewService(green, connectors.NewFactory(upstream.Client(), nil))
	firstDone := make(chan error, 1)
	go func() {
		_, collectErr := first.Collect(context.Background(), supplier.ID)
		firstDone <- collectErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first collector did not reach upstream")
	}
	if _, err := second.Collect(context.Background(), supplier.ID); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second collector error = %v, want ErrAlreadyRunning", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first collection: %v", err)
	}
}

func TestCollectOrJoinSharesRunningCollectionResult(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var pricingRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pricing" {
			http.NotFound(w, r)
			return
		}
		pricingRequests.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	st := openCollectorStore(t, "join-running.db")
	supplier := domain.Supplier{Name: "joined", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, connectors.NewFactory(upstream.Client(), nil))
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Collect(context.Background(), supplier.ID)
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first collection did not reach upstream")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	joined, joinedErr := service.CollectOrJoin(context.Background(), supplier.ID)
	if joinedErr != nil || joined.Attempt.Status != "success" {
		t.Fatalf("joined collection=%#v err=%v", joined, joinedErr)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("leader collection: %v", err)
	}
	if pricingRequests.Load() != 1 {
		t.Fatalf("joined collection sent %d pricing requests, want 1", pricingRequests.Load())
	}
	attempts, err := st.ListCollectionAttempts(context.Background(), supplier.ID, 10)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("joined collection attempts=%d err=%v", len(attempts), err)
	}
}

func TestCollectOrJoinWaitsForCollectionOwnedByAnotherService(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var pricingRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pricing" {
			http.NotFound(w, r)
			return
		}
		pricingRequests.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "external-join.db")
	blue, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blue.Close()
	green, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer green.Close()
	supplier := domain.Supplier{Name: "external-join", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := blue.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	first := NewService(blue, connectors.NewFactory(upstream.Client(), nil))
	second := NewService(green, connectors.NewFactory(upstream.Client(), nil))
	firstDone := make(chan error, 1)
	go func() {
		_, collectErr := first.Collect(context.Background(), supplier.ID)
		firstDone <- collectErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first collector did not reach upstream")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	joined, joinedErr := second.CollectOrJoin(context.Background(), supplier.ID)
	if joinedErr != nil || joined.Attempt.Status != "success" {
		t.Fatalf("joined external collection=%#v err=%v", joined, joinedErr)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first collection: %v", err)
	}
	if pricingRequests.Load() != 1 {
		t.Fatalf("external join sent %d pricing requests, want 1", pricingRequests.Load())
	}
	attempts, err := green.ListCollectionAttempts(context.Background(), supplier.ID, 10)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("external joined collection attempts=%d err=%v", len(attempts), err)
	}
}

func TestCollectOrJoinTimeoutDoesNotRecordUpstreamFailure(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pricing" {
			http.NotFound(w, r)
			return
		}
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "external-timeout.db")
	blue, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blue.Close()
	green, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer green.Close()
	supplier := domain.Supplier{Name: "external-timeout", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := blue.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	first := NewService(blue, connectors.NewFactory(upstream.Client(), nil))
	second := NewService(green, connectors.NewFactory(upstream.Client(), nil))
	second.SetJoinTimeout(40 * time.Millisecond)
	firstDone := make(chan error, 1)
	go func() {
		_, collectErr := first.Collect(context.Background(), supplier.ID)
		firstDone <- collectErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first collector did not reach upstream")
	}
	if _, err := second.CollectOrJoin(context.Background(), supplier.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join timeout error = %v, want context deadline exceeded", err)
	}
	attempts, err := green.ListCollectionAttempts(context.Background(), supplier.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("join timeout recorded %d upstream attempt(s), want 0", len(attempts))
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first collection: %v", err)
	}
}

func TestAfterCollectRunsInBackgroundAndRecordsFailure(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()
	st := openCollectorStore(t, "after.db")
	supplier := domain.Supplier{Name: "after", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, connectors.NewFactory(upstream.Client(), nil))
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	service.SetAfterCollectWithError(func(context.Context, int64) error {
		close(hookStarted)
		<-releaseHook
		return errors.New("reconciliation failed")
	})
	started := time.Now()
	if _, err := service.Collect(context.Background(), supplier.ID); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("collection waited for background hook: %s", elapsed)
	}
	select {
	case <-hookStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("after-collect hook did not start")
	}
	close(releaseHook)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		activities, err := st.RecentActivity(context.Background(), 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, activity := range activities {
			if activity.Kind == "after_collect_failed" && activity.Message == "reconciliation failed" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("after-collect failure activity was not recorded")
}

func TestAfterFailureAlreadyRecordedOnlySuppressesPerPolicyAggregate(t *testing.T) {
	if !afterFailureAlreadyRecorded(errors.New("legacy pricing policy 7 propose: failed\nlegacy pricing policy 9 apply: failed")) {
		t.Fatal("per-policy failures should not create a duplicate collection summary")
	}
	if !afterFailureAlreadyRecorded(errors.New("legacy pricing target 1 read: remote target returned Too Many Requests (429)")) {
		t.Fatal("target-level failure should not create a duplicate collection summary")
	}
	if afterFailureAlreadyRecorded(errors.New("mirror policy abc preview: failed\nlegacy pricing policy 7 propose: failed")) {
		t.Fatal("an unrecorded mirror failure must still create a collection summary")
	}
}

func TestAfterCollectNetworkFailuresNeverEnterActivityLog(t *testing.T) {
	t.Parallel()
	st := openCollectorStore(t, "after-coalesced.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	supplier := domain.Supplier{Name: "after-coalesced", BaseURL: "https://example.test", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		service.recordAfterFailure(supplier.ID, "采集后重算失败", "read target options: Too Many Requests (429)")
	}
	activities, err := st.RecentActivity(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Kind == "after_collect_failed" {
			t.Fatalf("network failure was exposed after repetition: %#v", activity)
		}
	}
}

func TestCollectionBusyOnlyAlertsAfterThreeConsecutiveFailures(t *testing.T) {
	t.Parallel()
	st := openCollectorStore(t, "busy-log-threshold.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	supplier := domain.Supplier{Name: "busy-log-threshold", BaseURL: "https://example.test", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	record := func(message string) {
		service.recordFailure(context.Background(), supplier, time.Now().Add(-time.Second),
			domain.CollectionDiagnostics{}, errors.New(message))
	}
	assertFailures := func(want int) {
		t.Helper()
		activities, err := st.RecentActivity(context.Background(), 20)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, activity := range activities {
			if activity.Kind == "collection_failed" {
				got++
			}
		}
		if got != want {
			t.Fatalf("collection failure activities=%d, want %d; all=%#v", got, want, activities)
		}
	}

	record("database is locked")
	record("database is locked")
	assertFailures(0)
	record("lookup example.test: no such host")
	record("database is locked")
	record("database is locked")
	assertFailures(0)
	record("database is locked")
	assertFailures(1)
}

func TestAfterCollectBusyOnlyAlertsAfterThreeConsecutiveCycles(t *testing.T) {
	t.Parallel()
	st := openCollectorStore(t, "after-busy-threshold.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	supplier := domain.Supplier{Name: "after-busy-threshold", BaseURL: "https://example.test", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	service.SetAfterCollectWithError(func(context.Context, int64) error {
		calls.Add(1)
		return errors.New("database is locked")
	})
	waitCycle := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			service.afterMu.Lock()
			_, pending := service.afterPending[supplier.ID]
			service.afterMu.Unlock()
			if calls.Load() >= want && !pending {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("after-collect cycle %d did not finish; calls=%d", want, calls.Load())
	}
	countFailures := func() int {
		t.Helper()
		activities, err := st.RecentActivity(context.Background(), 20)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, activity := range activities {
			if activity.Kind == "after_collect_failed" {
				count++
			}
		}
		return count
	}

	for cycle := int32(1); cycle <= 2; cycle++ {
		service.enqueueAfterCollect(supplier.ID)
		waitCycle(cycle)
		if got := countFailures(); got != 0 {
			t.Fatalf("busy cycle %d exposed %d activities before threshold", cycle, got)
		}
	}
	service.enqueueAfterCollect(supplier.ID)
	waitCycle(3)
	if got := countFailures(); got != 1 {
		t.Fatalf("third consecutive busy cycle activities=%d, want 1", got)
	}
}

func TestTransientCollectionFailureNeverEntersActivityLog(t *testing.T) {
	t.Parallel()
	st := openCollectorStore(t, "transient-log-threshold.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	supplier := domain.Supplier{Name: "transient-log-threshold", BaseURL: "https://example.test", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		service.recordFailure(context.Background(), supplier, time.Now().Add(-time.Second),
			domain.CollectionDiagnostics{}, errors.New("lookup example.test: no such host"))
		activities, err := st.RecentActivity(context.Background(), 20)
		if err != nil {
			t.Fatal(err)
		}
		failures := 0
		for _, activity := range activities {
			if activity.Kind == "collection_failed" {
				failures++
			}
		}
		if failures != 0 {
			t.Fatalf("transient attempt %d was exposed: %#v", attempt, activities)
		}
	}
}

func TestAfterCollectDeferredErrorsWaitWithoutActivity(t *testing.T) {
	t.Parallel()
	st := openCollectorStore(t, "deferred-after-collect.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	supplier := domain.Supplier{Name: "deferred-after-collect", BaseURL: "https://example.test", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service.SetAfterCollectWithError(func(context.Context, int64) error { return deferredError{} })
	service.enqueueAfterCollect(supplier.ID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !service.afterReconcilePending(supplier.ID) {
		time.Sleep(10 * time.Millisecond)
	}
	if !service.afterReconcilePending(supplier.ID) {
		t.Fatal("deferred reconciliation was not retained for the next normal collection")
	}
	activities, err := st.RecentActivity(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, activity := range activities {
		if activity.Kind == "after_collect_failed" {
			t.Fatalf("deferred evidence wait was exposed as a failure: %#v", activity)
		}
	}
}

type deferredError struct{}

func (deferredError) Error() string              { return "source evidence is pending" }
func (deferredError) AfterCollectDeferred() bool { return true }

func TestUnchangedCatalogDoesNotRunAfterCollectAgain(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()
	st := openCollectorStore(t, "unchanged-after-collect.db")
	supplier := domain.Supplier{Name: "unchanged", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, connectors.NewFactory(upstream.Client(), nil))
	runs := make(chan struct{}, 2)
	service.SetAfterCollectWithError(func(context.Context, int64) error {
		runs <- struct{}{}
		return nil
	})
	if _, err := service.Collect(context.Background(), supplier.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("first trusted catalog did not run reconciliation")
	}
	if _, err := service.Collect(context.Background(), supplier.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runs:
		t.Fatal("unchanged catalog ran reconciliation again")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCollectPersistsBalanceWhenCatalogFailsWithoutReplacingTrustedCatalog(t *testing.T) {
	t.Parallel()
	var failCatalog atomic.Bool
	var balance atomic.Int64
	balance.Store(10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/pricing":
			if failCatalog.Load() {
				http.Error(w, `{"success":false,"message":"catalog unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, pricingPayload("0.05"))
		case "/api/status":
			fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		case "/api/user/self":
			fmt.Fprintf(w, `{"success":true,"data":{"balance":%d,"currency":"USD"}}`, balance.Load())
		case "/api/data/self":
			fmt.Fprint(w, `{"success":true,"data":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	st := openCollectorStore(t, "partial-balance.db")
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "partial balance", Kind: "browser_pairing", SiteURL: upstream.URL}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{Cookie: "session=paired"}); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "partial balance", BaseURL: upstream.URL, Adapter: "newapi",
		CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, connectors.NewFactory(upstream.Client(), credentialService))
	baseline, err := service.Collect(context.Background(), supplier.ID)
	if err != nil || !baseline.Attempt.CatalogUpdated || baseline.Snapshot.Evidence.ResponseHash == "" {
		t.Fatalf("baseline collection=%#v err=%v", baseline, err)
	}

	failCatalog.Store(true)
	balance.Store(7)
	partial, err := service.Collect(context.Background(), supplier.ID)
	if err != nil {
		t.Fatalf("partial collection returned an error: %v", err)
	}
	if partial.Attempt.Status != "partial_success" || partial.Attempt.CatalogUpdated || !partial.Attempt.BalanceUpdated || partial.Changed {
		t.Fatalf("partial result=%#v", partial)
	}
	latestCatalog, err := st.LatestSnapshot(context.Background(), supplier.ID)
	if err != nil || latestCatalog.Evidence.ResponseHash != baseline.Snapshot.Evidence.ResponseHash {
		t.Fatalf("trusted catalog moved: %#v err=%v", latestCatalog, err)
	}
	latestBalance, err := st.LatestBalance(context.Background(), supplier.ID)
	if err != nil || latestBalance.Available != "7" {
		t.Fatalf("partial balance=%#v err=%v", latestBalance, err)
	}
	attempts, err := st.ListCollectionAttempts(context.Background(), supplier.ID, 10)
	if err != nil || len(attempts) != 2 || attempts[0].Status != "partial_success" || len(attempts[0].Requests) == 0 {
		t.Fatalf("attempt evidence=%#v err=%v", attempts, err)
	}
}

func TestCollectConnectionResetPreservesLastKnownGoodCatalog(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/pricing" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, pricingPayload("0.05"))
	}))
	defer upstream.Close()

	var resetCatalog atomic.Bool
	baseTransport := upstream.Client().Transport
	client := &http.Client{Transport: collectorRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if resetCatalog.Load() && request.URL.Path == "/api/pricing" {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return baseTransport.RoundTrip(request)
	})}

	st := openCollectorStore(t, "last-known-good.db")
	supplier := domain.Supplier{Name: "intermittent catalog", BaseURL: upstream.URL, Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, connectors.NewFactory(client, nil))
	baseline, err := service.Collect(context.Background(), supplier.ID)
	if err != nil || baseline.Snapshot.Evidence.ResponseHash == "" {
		t.Fatalf("baseline collection=%#v err=%v", baseline, err)
	}

	resetCatalog.Store(true)
	if _, err := service.Collect(context.Background(), supplier.ID); err == nil || connectors.ErrorClass(err) != "connection_reset" {
		t.Fatalf("connection reset collection error=%v class=%q", err, connectors.ErrorClass(err))
	}
	latest, err := st.LatestSnapshot(context.Background(), supplier.ID)
	if err != nil || latest.Evidence.ResponseHash != baseline.Snapshot.Evidence.ResponseHash || len(latest.Models) == 0 {
		t.Fatalf("last-known-good catalog=%#v err=%v", latest, err)
	}
	attempt, err := st.LatestCollectionAttempt(context.Background(), supplier.ID)
	if err != nil || attempt.Status != "failed" || attempt.ErrorClass != "connection_reset" || attempt.CatalogUpdated {
		t.Fatalf("failed attempt=%#v err=%v", attempt, err)
	}
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.LastStatus != "degraded" || updated.LastSuccessAt == nil || updated.LastErrorClass != "connection_reset" {
		t.Fatalf("supplier LKG state=%#v err=%v", updated, err)
	}
}

func TestAuthExpiredSignalRequiresBoundCredentialEvidenceAndDeduplicates(t *testing.T) {
	t.Parallel()
	credentialID := int64(41)
	otherCredentialID := int64(42)
	supplier := domain.Supplier{ID: 9, CredentialID: &credentialID}
	service := NewService(nil, nil)
	var calls atomic.Int32
	service.SetAuthExpiredHook(func(context.Context, int64) error {
		calls.Add(1)
		return errors.New("renewal is not configured")
	})

	invalidEvidence := []domain.CollectionDiagnostics{
		{Capabilities: []domain.AuthCapabilityEvidence{{Capability: "anonymous", Status: "expired_or_invalid", ErrorClass: "auth_expired"}}},
		{Capabilities: []domain.AuthCapabilityEvidence{{Capability: "customer_session", CredentialID: &otherCredentialID, Status: "expired_or_invalid", ErrorClass: "auth_expired"}}},
		{Capabilities: []domain.AuthCapabilityEvidence{{Capability: "site_admin", CredentialID: &credentialID, Status: "denied", ErrorClass: "permission_denied", HTTPStatus: http.StatusForbidden}}},
		{Capabilities: []domain.AuthCapabilityEvidence{{Capability: "customer_session", CredentialID: &credentialID, Status: "failed", ErrorClass: "auth_expired", HTTPStatus: http.StatusUnauthorized}}},
	}
	for _, diagnostics := range invalidEvidence {
		service.signalAuthExpired(context.Background(), supplier, diagnostics)
	}
	if calls.Load() != 0 {
		t.Fatalf("non-authoritative evidence signaled renewal %d times", calls.Load())
	}

	expired := domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{{
		Capability: "customer_session", CredentialID: &credentialID, Status: "expired_or_invalid", ErrorClass: "auth_expired", HTTPStatus: http.StatusUnauthorized,
	}}}
	service.signalAuthExpired(context.Background(), supplier, expired)
	service.signalAuthExpired(context.Background(), supplier, expired)
	if calls.Load() != 1 {
		t.Fatalf("duplicate auth-expired evidence signaled renewal %d times, want 1", calls.Load())
	}
}

func TestAuthExpiredSignalDoesNotRenewCredentialWithVerifiedCapability(t *testing.T) {
	credentialID := int64(91)
	supplier := domain.Supplier{CredentialID: &credentialID}
	diagnostics := domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{
		{CredentialID: &credentialID, Capability: "site_admin", EndpointType: "admin_groups",
			Status: "expired_or_invalid", ErrorClass: "auth_expired"},
		{CredentialID: &credentialID, Capability: "customer_session", EndpointType: "customer_catalog",
			Status: "verified"},
	}}
	if id, ok := authExpiredCredentialID(supplier, diagnostics); ok || id != 0 {
		t.Fatalf("verified credential was incorrectly scheduled for renewal: id=%d ok=%v", id, ok)
	}
}

func TestAuthExpiredSignalIgnoresAnonymous401WithoutBoundCredential(t *testing.T) {
	t.Parallel()
	service := NewService(nil, nil)
	var calls atomic.Int32
	service.SetAuthExpiredHook(func(context.Context, int64) error {
		calls.Add(1)
		return nil
	})
	diagnostics := domain.CollectionDiagnostics{Capabilities: []domain.AuthCapabilityEvidence{{
		Capability: "anonymous", Status: "missing", ErrorClass: "auth_missing", HTTPStatus: http.StatusUnauthorized,
	}}}
	service.signalAuthExpired(context.Background(), domain.Supplier{ID: 10}, diagnostics)
	if calls.Load() != 0 {
		t.Fatalf("anonymous 401 signaled credential renewal %d times", calls.Load())
	}
}

func TestCollectSignalsBoundCredentialExpiryWithoutDuplicating(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/pricing":
			fmt.Fprint(response, pricingPayload("0.05"))
		case "/api/status", "/api/setting":
			fmt.Fprint(response, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
		default:
			http.Error(response, `{"success":false,"message":"Unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	st := openCollectorStore(t, "auth-expired-signal.db")
	vault, err := secrets.Open(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	credentialService := credentials.NewService(st, vault)
	credential := domain.Credential{Name: "expired session", Kind: "browser_pairing", SiteURL: upstream.URL}
	if err := credentialService.Create(context.Background(), &credential, domain.CredentialPayload{Cookie: "session=expired", UserID: 7}); err != nil {
		t.Fatal(err)
	}
	credentialID := credential.ID
	supplier := domain.Supplier{Name: "expired session", BaseURL: upstream.URL, Adapter: "newapi", CredentialID: &credentialID, Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}

	service := NewService(st, connectors.NewFactory(upstream.Client(), credentialService))
	var calls atomic.Int32
	var signaledID atomic.Int64
	service.SetAuthExpiredHook(func(_ context.Context, id int64) error {
		signaledID.Store(id)
		calls.Add(1)
		return nil
	})
	if _, err := service.Collect(context.Background(), supplier.ID); err != nil {
		t.Fatalf("first collection: %v", err)
	}
	if _, err := service.Collect(context.Background(), supplier.ID); err != nil {
		t.Fatalf("second collection: %v", err)
	}
	if calls.Load() != 1 || signaledID.Load() != credential.ID {
		t.Fatalf("auth-expired signals=%d credential=%d, want one signal for %d", calls.Load(), signaledID.Load(), credential.ID)
	}
}

func TestJitteredIntervalStaysWithinTenPercentAndIsDeterministic(t *testing.T) {
	base := 5 * time.Minute
	observed := time.Date(2026, 7, 22, 10, 30, 0, 0, time.UTC)
	first := jitteredInterval(base, 42, observed)
	second := jitteredInterval(base, 42, observed)
	if first != second {
		t.Fatalf("jitter changed for the same schedule key: %s != %s", first, second)
	}
	if first < base*9/10 || first > base*11/10 {
		t.Fatalf("jittered interval %s is outside +/-10%% of %s", first, base)
	}
}

func TestCollectionFailureHonorsConfiguredPollInterval(t *testing.T) {
	st := openCollectorStore(t, "failure-schedule.db")
	supplier := domain.Supplier{Name: "failed upstream", BaseURL: "https://upstream.example", Adapter: "newapi",
		Enabled: true, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	before := time.Now().UTC()
	service.recordFailure(context.Background(), supplier, before, domain.CollectionDiagnostics{}, errors.New("HTTP 401"))
	after := time.Now().UTC()
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.NextCollectionAt == nil {
		t.Fatalf("failed schedule=%#v err=%v", updated, err)
	}
	if updated.NextCollectionAt.Before(before.Add(300 * time.Second)) {
		t.Fatalf("failure retry scheduled too early: next=%v before=%v", updated.NextCollectionAt, before)
	}
	if updated.NextCollectionAt.After(after.Add(300*time.Second + time.Second)) {
		t.Fatalf("failure retry scheduled too late: next=%v after=%v", updated.NextCollectionAt, after)
	}
	dueEarly, err := st.DueSuppliers(context.Background(), before.Add(299*time.Second), 10)
	if err != nil || len(dueEarly) != 0 {
		t.Fatalf("supplier became due before poll interval: due=%#v err=%v", dueEarly, err)
	}
	dueLater, err := st.DueSuppliers(context.Background(), after.Add(301*time.Second), 10)
	if err != nil || len(dueLater) != 1 || dueLater[0].ID != supplier.ID {
		t.Fatalf("supplier did not become due after poll interval: due=%#v err=%v", dueLater, err)
	}
}

func TestTransientCollectionFailureUsesBoundedShortRecovery(t *testing.T) {
	st := openCollectorStore(t, "transient-recovery.db")
	supplier := domain.Supplier{Name: "dns recovery", BaseURL: "https://upstream.example", Adapter: "newapi",
		Enabled: true, PollIntervalSeconds: 300}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	service := NewService(st, nil)
	service.SetTransientRetryDelay(time.Minute)
	before := time.Now().UTC()
	service.recordFailure(context.Background(), supplier, before, domain.CollectionDiagnostics{}, errors.New("lookup upstream.example: no such host"))
	after := time.Now().UTC()
	updated, err := st.GetSupplier(context.Background(), supplier.ID)
	if err != nil || updated.NextCollectionAt == nil {
		t.Fatalf("transient schedule=%#v err=%v", updated, err)
	}
	if updated.NextCollectionAt.Before(before.Add(54*time.Second)) || updated.NextCollectionAt.After(after.Add(66*time.Second)) {
		t.Fatalf("transient retry is outside one-minute jitter window: %v", updated.NextCollectionAt)
	}
}

func TestCollectionOutcomeDoesNotLetOptionalUsageOverrideCoreSuccess(t *testing.T) {
	t.Parallel()
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, BalanceSucceeded: true, Requests: []domain.RequestEvidence{
		{EndpointType: "catalog", HTTPStatus: 200, Status: "success"},
		{EndpointType: "balance", HTTPStatus: 200, Status: "success"},
		{EndpointType: "usage", HTTPStatus: 200, Status: "failed", ErrorClass: "contract_changed"},
	}}
	status, class, message := collectionOutcome(diagnostics)
	if status != "success" || class != "" || message != "" {
		t.Fatalf("optional usage changed core outcome: %q %q %q", status, class, message)
	}
}

func TestCollectionOutcomeDoesNotLetOptionalAdminProbeOverrideCoreSuccess(t *testing.T) {
	t.Parallel()
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, BalanceSucceeded: true, Requests: []domain.RequestEvidence{
		{EndpointType: "customer_catalog", AuthCapability: "customer_session", HTTPStatus: 200, Status: "success"},
		{EndpointType: "customer_balance", AuthCapability: "customer_session", HTTPStatus: 200, Status: "success"},
		{EndpointType: "admin_groups", AuthCapability: "site_admin", HTTPStatus: http.StatusForbidden, Status: "failed", ErrorClass: "permission_denied"},
	}}
	status, class, message := collectionOutcome(diagnostics)
	if status != "success" || class != "" || message != "" {
		t.Fatalf("optional admin probe changed core outcome: %q %q %q", status, class, message)
	}
}

func TestCollectionOutcomeUsesFinalSuccessfulNegotiatedEndpoint(t *testing.T) {
	t.Parallel()
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, Requests: []domain.RequestEvidence{
		{EndpointType: "catalog", HTTPStatus: 404, Status: "failed", ErrorClass: "not_found"},
		{EndpointType: "catalog", HTTPStatus: 200, Status: "success"},
	}}
	status, class, message := collectionOutcome(diagnostics)
	if status != "success" || class != "" || message != "" {
		t.Fatalf("successful negotiated endpoint remained partial: %q %q %q", status, class, message)
	}
}

func TestCollectionOutcomeKeepsMissingPricingVisible(t *testing.T) {
	t.Parallel()
	diagnostics := domain.CollectionDiagnostics{CatalogSucceeded: true, BalanceSucceeded: true, Requests: []domain.RequestEvidence{
		{EndpointType: "catalog", HTTPStatus: 200, Status: "success"},
		{EndpointType: "balance", HTTPStatus: 200, Status: "success"},
		{EndpointType: "pricing", HTTPStatus: 404, Status: "failed", ErrorClass: "not_found"},
	}}
	status, class, _ := collectionOutcome(diagnostics)
	if status != "partial_success" || class != "not_found" {
		t.Fatalf("missing pricing was hidden: %q %q", status, class)
	}
}

func pricingPayload(ratio string) string {
	return fmt.Sprintf(`{"success":true,"data":[{"model_name":"gpt-test","quota_type":0,"model_ratio":1,"completion_ratio":1,"enable_groups":["economy"]}],"group_ratio":{"economy":%s}}`, ratio)
}

type collectorRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function collectorRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func openCollectorStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
