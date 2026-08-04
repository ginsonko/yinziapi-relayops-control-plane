package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrReleaseAdmissionDraining = errors.New("release admission is draining")

type ReleaseAdmissionStatus struct {
	State                    string
	Generation               int64
	ActiveCollectionLeases   int64
	ActiveTargetLeases       int64
	ActiveTargetChangeLeases int64
	PendingReservations      int64
	UpdatedAt                time.Time
}

func checkReleaseAdmissionOpenTx(ctx context.Context, tx *sql.Tx) error {
	var state string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM release_admission_state WHERE singleton_id=1`).Scan(&state); err != nil {
		return fmt.Errorf("read release admission: %w", err)
	}
	if state != "open" {
		return ErrReleaseAdmissionDraining
	}
	return nil
}

// SetReleaseAdmissionDraining is the durable first step of a managed cutover.
// It prevents new collection and target-control leases before operators wait
// for the already-issued leases to drain and stop the old process.
func (s *Store) SetReleaseAdmissionDraining(ctx context.Context, draining bool) (ReleaseAdmissionStatus, error) {
	wanted := "open"
	if draining {
		wanted = "draining"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now, err := sqliteUnixMillisTx(ctx, tx)
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE release_admission_state SET state=?,
		generation=CASE WHEN state=? THEN generation ELSE generation+1 END,updated_at=? WHERE singleton_id=1`,
		wanted, wanted, time.UnixMilli(now).UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ReleaseAdmissionStatus{}, errors.New("release admission singleton is missing")
	}
	if err := tx.Commit(); err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	return s.ReleaseAdmissionStatus(ctx)
}

func (s *Store) ReleaseAdmissionStatus(ctx context.Context) (ReleaseAdmissionStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()
	status, err := releaseAdmissionStatusTx(ctx, tx)
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	return status, nil
}

func releaseAdmissionStatusTx(ctx context.Context, tx *sql.Tx) (ReleaseAdmissionStatus, error) {
	var status ReleaseAdmissionStatus
	var updated string
	if err := tx.QueryRowContext(ctx, `SELECT state,generation,updated_at FROM release_admission_state
		WHERE singleton_id=1`).Scan(&status.State, &status.Generation, &updated); err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	status.UpdatedAt = parseTime(updated)
	now, err := sqliteUnixMillisTx(ctx, tx)
	if err != nil {
		return ReleaseAdmissionStatus{}, err
	}
	queries := []struct {
		destination *int64
		query       string
		args        []any
	}{
		{&status.ActiveCollectionLeases, `SELECT COUNT(*) FROM collection_leases WHERE owner_id<>'' AND
			(CASE WHEN leased_until_unix_ms>0 THEN leased_until_unix_ms ELSE CAST((julianday(leased_until)-2440587.5)*86400000 AS INTEGER) END)>?`, []any{now}},
		{&status.ActiveTargetLeases, `SELECT COUNT(*) FROM target_control_leases WHERE owner_id<>'' AND leased_until>?`, []any{formatLeaseTime(time.UnixMilli(now).UTC())}},
		{&status.ActiveTargetChangeLeases, `SELECT COUNT(*) FROM target_change_leases WHERE owner_id<>'' AND leased_until>?`, []any{formatLeaseTime(time.UnixMilli(now).UTC())}},
		{&status.PendingReservations, `SELECT COUNT(*) FROM collection_attempt_reservations WHERE state='reserved'`, nil},
	}
	for _, query := range queries {
		if err := tx.QueryRowContext(ctx, query.query, query.args...).Scan(query.destination); err != nil {
			return ReleaseAdmissionStatus{}, err
		}
	}
	return status, nil
}

func (status ReleaseAdmissionStatus) Drained() bool {
	return status.State == "draining" && status.ActiveCollectionLeases == 0 &&
		status.ActiveTargetLeases == 0 && status.ActiveTargetChangeLeases == 0 && status.PendingReservations == 0
}

func (s *Store) WaitForReleaseDrain(ctx context.Context, interval time.Duration) (ReleaseAdmissionStatus, error) {
	if interval <= 0 || interval > time.Second {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		status, err := s.ReleaseAdmissionStatus(ctx)
		if err != nil {
			return ReleaseAdmissionStatus{}, err
		}
		if status.Drained() {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-ticker.C:
		}
	}
}
