package intelligence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestLedgerCachePersistenceFailureKeepsMemorySnapshotUsable(t *testing.T) {
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	service := NewService(st)
	if _, err := service.ledgerData(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(service.ledgerCachePath()); err != nil {
		t.Fatal(err)
	}
	// A directory created after the successful initial load makes the next
	// atomic rename fail without making the database itself unwritable.
	cachePath := filepath.Join(directory, "intelligence-ledger-cache-v1.json.gz")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	service.recordLedgerCachePersistence()
	result, err := service.Dashboard(context.Background(), DefaultConfig())
	if err != nil {
		t.Fatalf("dashboard must keep serving the memory snapshot: %v", err)
	}
	if result.CacheState != "memory_only" || service.ledgerCache.persistError == "" {
		t.Fatalf("cache state=%q persist error=%q", result.CacheState, service.ledgerCache.persistError)
	}
}

func TestCorruptPersistedCacheDoesNotTriggerAutomaticFullRebuild(t *testing.T) {
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cachePath := filepath.Join(directory, "intelligence-ledger-cache-v1.json.gz")
	if err := os.WriteFile(cachePath, []byte("not-gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(st)
	if _, err := service.ledgerData(context.Background()); err == nil {
		t.Fatal("corrupt persisted cache silently triggered a full database rebuild")
	}
	if service.ledgerCache.loaded {
		t.Fatal("corrupt persisted cache was marked loaded")
	}
}

func TestLedgerCachePersistsIncrementalBatchInBackground(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "async target", Kind: "access-token", SiteURL: "https://async-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: credential.Name, BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	service := NewService(st)
	if _, err := service.ledgerData(ctx); err != nil {
		t.Fatal(err)
	}
	events := make([]store.OperatingLedgerEvent, 100)
	for index := range events {
		now := time.Now().UTC().Add(time.Duration(index) * time.Nanosecond)
		events[index] = store.OperatingLedgerEvent{EventID: fmt.Sprintf("async-%03d", index), TargetSiteID: target.ID,
			OccurredAt: now, EventKind: "usage", Requests: 1, Source: "test", PayloadHash: fmt.Sprintf("hash-%03d", index), CreatedAt: now}
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	updated, err := service.ledgerData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond || len(updated.Events) != 100 {
		t.Fatalf("incremental read blocked on persistence: duration=%s events=%d", time.Since(started), len(updated.Events))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		service.ledgerMu.Lock()
		inFlight, persistErr := service.ledgerCache.persistInFlight, service.ledgerCache.persistError
		service.ledgerMu.Unlock()
		if !inFlight {
			if persistErr != "" {
				t.Fatalf("background persistence failed: %s", persistErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background persistence did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	restarted := NewService(st)
	loaded, err := restarted.loadPersistedLedgerCache()
	if err != nil || !loaded || restarted.ledgerCache.revision != updated.Revision {
		t.Fatalf("persisted incremental cache was not restartable: loaded=%v revision=%d want=%d err=%v",
			loaded, restarted.ledgerCache.revision, updated.Revision, err)
	}
}

func TestLedgerCacheAppliesEventAndProfileRevisions(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	st, err := store.Open(filepath.Join(directory, "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	credential := domain.Credential{Name: "cache target", Kind: "access-token", SiteURL: "https://cache-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: credential.Name, BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	service := NewService(st)
	initial, err := service.ledgerData(ctx)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	event := store.OperatingLedgerEvent{EventID: "incremental-event", TargetSiteID: target.ID, OccurredAt: now,
		EventKind: "usage", SubjectUserID: "42", Requests: 1, Source: "test"}
	if err := st.UpsertOperatingLedgerEvents(ctx, []store.OperatingLedgerEvent{event}); err != nil {
		t.Fatal(err)
	}
	profile := store.TargetLedgerUserProfile{TargetSiteID: target.ID, UserID: "42", Role: "administrator",
		UserSegment: "operator", SourceHash: "profile-v1", ObservedAt: now}
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, []store.TargetLedgerUserProfile{profile}); err != nil {
		t.Fatal(err)
	}

	updated, err := service.ledgerData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision <= initial.Revision || updated.Revision != updated.SourceRevision || len(updated.Events) != 1 {
		t.Fatalf("initial revision=%d updated=%#v", initial.Revision, updated)
	}
	if updated.Events[0].SubjectRole != "administrator" || updated.Events[0].UserSegment != "operator" {
		t.Fatalf("profile projection was not applied: %#v", updated.Events[0])
	}

	beforeDuplicate := updated.Revision
	if err := st.ReplaceTargetLedgerUserProfiles(ctx, target.ID, []store.TargetLedgerUserProfile{profile}); err != nil {
		t.Fatal(err)
	}
	afterDuplicate, err := st.CurrentOperatingLedgerRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterDuplicate != beforeDuplicate {
		t.Fatalf("identical profile snapshot advanced revision: before=%d after=%d", beforeDuplicate, afterDuplicate)
	}
}

func TestLedgerCacheRejectsPersistedSnapshotFromAnotherDatabase(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first", "relayops.db")
	if err := os.MkdirAll(filepath.Dir(firstPath), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := store.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	firstService := NewService(first)
	if _, err := firstService.ledgerData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	secondPath := filepath.Join(directory, "second", "relayops.db")
	if err := os.MkdirAll(filepath.Dir(secondPath), 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := store.Open(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	foreignCache := filepath.Join(filepath.Dir(firstPath), "intelligence-ledger-cache-v1.json.gz")
	secondCache := filepath.Join(filepath.Dir(secondPath), "intelligence-ledger-cache-v1.json.gz")
	content, err := os.ReadFile(foreignCache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondCache, content, 0o600); err != nil {
		t.Fatal(err)
	}
	secondService := NewService(second)
	loaded, err := secondService.loadPersistedLedgerCache()
	if err != nil {
		t.Fatal(err)
	}
	if loaded || secondService.ledgerCache.loaded {
		t.Fatal("persisted snapshot from another database was accepted")
	}
}
