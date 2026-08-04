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

func TestTargetControlLeaseUsesMonotonicFencingAndRejectsStaleOwner(t *testing.T) {
	t.Parallel()
	st, target := openTargetControlTestStore(t, true)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	first, err := st.AcquireTargetControlLease(ctx, target.ID, "pricing_option:GroupRatio", "blue", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AcquireTargetControlLease(ctx, target.ID, first.Dimension, "green", now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrTargetControlLeaseBusy) {
		t.Fatalf("competing lease error = %v", err)
	}
	second, err := st.AcquireTargetControlLease(ctx, target.ID, first.Dimension, "green", now.Add(61*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatalf("fencing tokens first=%d second=%d", first.FencingToken, second.FencingToken)
	}
	if err := st.CheckTargetControlLease(ctx, first, now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale lease check = %v", err)
	}
	if err := st.ReleaseTargetControlLease(ctx, first, now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale release = %v", err)
	}
	if err := st.CheckTargetControlLease(ctx, second, now.Add(62*time.Second)); err != nil {
		t.Fatalf("current lease check: %v", err)
	}
}

func TestTargetControlOperationFinishRequiresCurrentFenceAndPersistsReplay(t *testing.T) {
	t.Parallel()
	st, target := openTargetControlTestStore(t, true)
	ctx := context.Background()
	now := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	first, err := st.AcquireTargetControlLease(ctx, target.ID, "channel:5:status", "first", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.TargetControlOperation{TargetSiteID: target.ID, Dimension: first.Dimension, Action: "channel_status_apply",
		IdempotencyKey: "status-1", RequestHash: "request-a", ExpectedHash: "before", DesiredHash: "after",
		FencingToken: first.FencingToken, ResultState: "in_progress", StartedAt: now}
	if err := st.CreateTargetControlOperation(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	second, err := st.AcquireTargetControlLease(ctx, target.ID, first.Dimension, "second", now.Add(61*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTargetControlOperation(ctx, operation.ID, first, "verified_success", "after", `{}`, "", now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale finish = %v", err)
	}
	if err := st.ResumeTargetControlOperation(ctx, operation.ID, "request-a", second, now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTargetControlOperation(ctx, operation.ID, second, "verified_success", "after",
		`{"result_state":"verified_success"}`, "", now.Add(63*time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetTargetControlOperationByKey(ctx, target.ID, "status-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ResultState != "verified_success" || stored.ReadbackHash != "after" || stored.Result == nil {
		t.Fatalf("stored operation = %#v", stored)
	}
	duplicate := operation
	duplicate.ID = 0
	duplicate.RequestHash = "different-request"
	if err := st.CreateTargetControlOperation(ctx, &duplicate); err == nil {
		t.Fatal("duplicate idempotency key unexpectedly inserted")
	}
}

func TestReplaceTargetCredentialAtomicPreservesManualDisableAndCanCompensate(t *testing.T) {
	t.Parallel()
	st, target := openTargetControlTestStore(t, false)
	ctx := context.Background()
	oldCredential, _, err := st.GetCredentialEncrypted(ctx, target.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := domain.Credential{Name: "replacement", Kind: "newapi-management", SiteURL: target.BaseURL}
	replacedAt, err := st.ReplaceTargetCredentialAtomic(ctx, target.ID, target.CredentialID, target.UpdatedAt,
		oldCredential.UpdatedAt, &replacement, "encrypted-new")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CredentialID != replacement.ID || updated.Enabled {
		t.Fatalf("updated target = %#v", updated)
	}
	if _, err := st.RestoreTargetCredentialAtomic(ctx, target.ID, replacement.ID, target.CredentialID, replacedAt); err != nil {
		t.Fatal(err)
	}
	restored, err := st.GetTarget(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.CredentialID != target.CredentialID || restored.Enabled {
		t.Fatalf("restored target = %#v", restored)
	}
}

func TestAcquireTargetControlLeaseRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	st, target := openTargetControlTestStore(t, true)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('target-control-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	type result struct {
		lease domain.TargetControlLease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, err := st.AcquireTargetControlLease(ctx, target.ID, "channel:49:status", "busy-retry", time.Now().UTC(), time.Minute)
		done <- result{lease: lease, err: err}
	}()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.lease.FencingToken != 1 {
			t.Fatalf("lease busy retry failed: lease=%#v err=%v", outcome.lease, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("lease busy retry remained blocked")
	}
}

func TestCreateTargetControlOperationRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	st, target := openTargetControlTestStore(t, true)
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireTargetControlLease(ctx, target.ID, "channel:49:operation", "busy-retry", time.Now().UTC(), time.Minute)
	if err != nil {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('target-operation-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	operation := domain.TargetControlOperation{TargetSiteID: target.ID, Dimension: lease.Dimension, Action: "channel_status_apply",
		IdempotencyKey: "busy-operation-1", RequestHash: "request-a", ExpectedHash: "before", DesiredHash: "after",
		FencingToken: lease.FencingToken, ResultState: "in_progress"}
	done := make(chan error, 1)
	go func() { done <- st.CreateTargetControlOperation(ctx, &operation) }()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || operation.ID <= 0 {
			t.Fatalf("operation busy retry failed: id=%d err=%v", operation.ID, err)
		}
	case <-time.After(time.Second):
		t.Fatal("operation busy retry remained blocked")
	}
	rows, err := st.ListTargetControlOperations(ctx, target.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != operation.ID {
		t.Fatalf("operation rows=%#v, want one inserted receipt", rows)
	}
}

func TestTargetControlPersistentBusyLockRemainsBounded(t *testing.T) {
	ctx := context.Background()
	st, target := openTargetControlTestStore(t, true)
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
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('target-control-persistent-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = st.AcquireTargetControlLease(ctx, target.ID, "channel:49:persistent", "busy", started.UTC(), time.Minute)
	if !isSQLiteBusy(err) {
		t.Fatalf("persistent lock error=%v, want sqlite busy", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("persistent lock took %s, want bounded failure", elapsed)
	}
}

func openTargetControlTestStore(t *testing.T, enabled bool) (*Store, domain.TargetSite) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "old", Kind: "newapi-management", SiteURL: "https://target.example"}
	if err := st.CreateCredential(context.Background(), &credential, "encrypted-old"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: credential.SiteURL, Adapter: "newapi", CredentialID: credential.ID,
		Enabled: enabled, WriteMode: "approval"}
	if err := st.CreateTarget(context.Background(), &target); err != nil {
		t.Fatal(err)
	}
	return st, target
}
