package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	_ "modernc.org/sqlite"
)

func TestSerializedWriteWaitIsCancelableAndDoesNotPoisonQueue(t *testing.T) {
	ctx := context.Background()
	st, targetID := openSerializedWriteTestStore(t)
	release, err := st.acquireWriteGate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, err = st.UpsertOperatingLedgerEventsCount(waitCtx, []OperatingLedgerEvent{serializedWriteTestEvent("cancelled", targetID)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued write error=%v, want deadline exceeded", err)
	}
	release()

	inserted, err := st.UpsertOperatingLedgerEventsCount(ctx, []OperatingLedgerEvent{serializedWriteTestEvent("after-cancel", targetID)})
	if err != nil || inserted != 1 {
		t.Fatalf("queue did not recover after cancellation: inserted=%d err=%v", inserted, err)
	}
}

func TestSerializedWriteRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	st, targetID := openSerializedWriteTestStore(t)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite", st.path+"?_pragma=busy_timeout(1)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	tx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('write-test-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	type result struct {
		inserted int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		inserted, err := st.UpsertOperatingLedgerEventsCount(ctx, []OperatingLedgerEvent{serializedWriteTestEvent("busy-retry", targetID)})
		done <- result{inserted: inserted, err: err}
	}()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.inserted != 1 {
			t.Fatalf("busy retry did not recover: inserted=%d err=%v", outcome.inserted, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("busy retry remained blocked")
	}
}

func TestCreateMirrorPolicyRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", st.path+"?_pragma=busy_timeout(1)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	tx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('mirror-write-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`,
		formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "busy-retry")
	done := make(chan error, 1)
	go func() { done <- st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()) }()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("mirror create did not recover from short busy lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mirror create remained blocked")
	}
	stored, err := st.GetMirrorPolicy(ctx, policy.PolicyUUID)
	if err != nil || stored.PolicyVersion != 1 || stored.Enabled != policy.Enabled {
		t.Fatalf("mirror create readback=%#v err=%v", stored, err)
	}
}

func TestCreateMirrorChangeSetRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Now().UTC()
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "busy-change-set")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireMirrorLease(ctx, target.ID, "busy-change-set", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", st.path+"?_pragma=busy_timeout(1)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	tx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('mirror-change-set-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(now)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	changeSet := newMirrorTestChangeSet(policy, target.ID, "busy-operation", "busy-request", "busy-step")
	type result struct {
		replayed bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		replayed, err := st.CreateMirrorChangeSet(ctx, &changeSet, lease, now.Add(time.Second))
		done <- result{replayed: replayed, err: err}
	}()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.replayed || changeSet.ID <= 0 || changeSet.Steps[0].ID <= 0 {
			t.Fatalf("change-set busy retry failed: replay=%v set=%#v err=%v", outcome.replayed, changeSet, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("change-set busy retry remained blocked")
	}
	stored, err := st.GetMirrorChangeSet(ctx, changeSet.ID)
	if err != nil || len(stored.Steps) != 1 || stored.OperationKey != changeSet.OperationKey {
		t.Fatalf("change-set readback=%#v err=%v", stored, err)
	}
}

func openSerializedWriteTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "write target", Kind: "access-token", SiteURL: "https://write-target.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: credential.Name, BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	return st, target.ID
}

func serializedWriteTestEvent(id string, targetID int64) OperatingLedgerEvent {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return OperatingLedgerEvent{EventID: id, TargetSiteID: targetID, OccurredAt: now, EventKind: "usage",
		Requests: 1, Source: "write-serialization-test", PayloadHash: id, CreatedAt: now}
}
