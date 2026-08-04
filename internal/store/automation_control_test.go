package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	_ "modernc.org/sqlite"
)

func TestAutomationControlPersistsAcrossRestartAndKeepsWorkerTruthRuntimeOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relayops.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := first.ResolveAutomationControl(ctx, true, true)
	if err != nil || !control.Enabled || !control.BackgroundWorkersEnabled || control.UpdatedAt.IsZero() {
		t.Fatalf("initial control=%#v err=%v", control, err)
	}
	updated, err := first.SetAutomationControl(ctx, false, true)
	if err != nil || updated.Enabled || updated.UpdatedAt.IsZero() {
		t.Fatalf("updated control=%#v err=%v", updated, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	persisted, err := reopened.ResolveAutomationControl(ctx, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Enabled {
		t.Fatalf("persisted control ignored saved kill switch: %#v", persisted)
	}
	if persisted.BackgroundWorkersEnabled {
		t.Fatalf("worker truth must come from the current process, got %#v", persisted)
	}
}

func TestAutomationControlRetriesShortExternalBusyLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relayops.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.ExecContext(ctx, `PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	tx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES('settings-write-lock','1',?)
		ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`, formatTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.SetAutomationControl(ctx, false, true)
		done <- err
	}()
	time.Sleep(45 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("automation control did not recover from short busy lock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("automation control busy retry remained blocked")
	}
	var persisted domain.AutomationControl
	if err := st.GetSetting(ctx, automationControlSettingKey, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Enabled || !persisted.BackgroundWorkersEnabled {
		t.Fatalf("automation control readback=%#v", persisted)
	}
}
