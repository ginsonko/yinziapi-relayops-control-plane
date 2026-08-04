package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PairingTokenState struct {
	Status            string     `json:"status"`
	SupplierID        int64      `json:"supplier_id"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	FailedAt          *time.Time `json:"failed_at,omitempty"`
	LastErrorClass    string     `json:"last_error_class,omitempty"`
	LastErrorMessage  string     `json:"last_error_message,omitempty"`
	LastAttemptAt     *time.Time `json:"last_attempt_at,omitempty"`
	AttemptCount      int        `json:"attempt_count"`
	Retryable         bool       `json:"retryable"`
	AttemptsRemaining int        `json:"attempts_remaining"`
}

func (s *Store) PairingStatusDetail(ctx context.Context, tokenHash string, now time.Time) (PairingTokenState, error) {
	var result PairingTokenState
	var expires, status string
	var usedAt, lastAttempt sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT supplier_id,expires_at,used_at,status,last_error_class,
		last_error_message,last_attempt_at,attempt_count FROM pairing_tokens WHERE token_hash=?`, tokenHash).
		Scan(&result.SupplierID, &expires, &usedAt, &status, &result.LastErrorClass, &result.LastErrorMessage,
			&lastAttempt, &result.AttemptCount); err != nil {
		return PairingTokenState{}, err
	}
	if lastAttempt.Valid {
		value := parseTime(lastAttempt.String)
		result.LastAttemptAt = &value
	}
	if usedAt.Valid {
		value := parseTime(usedAt.String)
		result.Status = "completed"
		result.CompletedAt = &value
		return result, nil
	}
	if status == "failed" {
		result.Status = "failed"
		result.FailedAt = result.LastAttemptAt
		return result, nil
	}
	if !parseTime(expires).After(now) {
		result.Status = "expired"
		return result, nil
	}
	result.Status = "pending"
	return result, nil
}

// FailPairingToken retains the terminal-failure API used by callers that know
// a pairing request cannot be safely retried (for example an origin mismatch).
func (s *Store) FailPairingToken(ctx context.Context, tokenHash, errorClass, safeMessage string, now time.Time) error {
	_, err := s.RecordPairingFailure(ctx, tokenHash, errorClass, safeMessage, now, 1, true)
	return err
}

// RecordPairingFailure keeps transient validation failures pending until the
// bounded attempt budget is exhausted. The update is atomic, so concurrent
// submissions cannot both observe and spend the same remaining attempt.
func (s *Store) RecordPairingFailure(ctx context.Context, tokenHash, errorClass, safeMessage string, now time.Time,
	maxAttempts int, terminal bool) (PairingTokenState, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	terminalValue := 0
	if terminal {
		terminalValue = 1
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pairing_tokens SET last_error_class=?,
		last_error_message=?,last_attempt_at=?,attempt_count=attempt_count+1,
		status=CASE WHEN ?=1 OR attempt_count+1>=? THEN 'failed' ELSE 'pending' END
		WHERE token_hash=? AND status='pending' AND used_at IS NULL AND expires_at>?`, errorClass, safeMessage,
		formatTime(now), terminalValue, maxAttempts, tokenHash, formatTime(now))
	if err != nil {
		return PairingTokenState{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return PairingTokenState{}, fmt.Errorf("pairing token is no longer pending")
	}
	state, err := s.PairingStatusDetail(ctx, tokenHash, now)
	if err != nil {
		return PairingTokenState{}, err
	}
	state.AttemptsRemaining = maxAttempts - state.AttemptCount
	if state.AttemptsRemaining < 0 {
		state.AttemptsRemaining = 0
	}
	state.Retryable = state.Status == "pending" && state.AttemptsRemaining > 0
	return state, nil
}

// RecordPairingTransientFailure records an upstream or control-plane failure
// without charging it against the operator's credential attempt budget.
func (s *Store) RecordPairingTransientFailure(ctx context.Context, tokenHash, errorClass, safeMessage string,
	now time.Time) (PairingTokenState, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pairing_tokens SET last_error_class=?,
		last_error_message=?,last_attempt_at=?
		WHERE token_hash=? AND status='pending' AND used_at IS NULL AND expires_at>?`, errorClass, safeMessage,
		formatTime(now), tokenHash, formatTime(now))
	if err != nil {
		return PairingTokenState{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return PairingTokenState{}, fmt.Errorf("pairing token is no longer pending")
	}
	return s.PairingStatusDetail(ctx, tokenHash, now)
}
