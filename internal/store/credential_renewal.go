package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

var ErrCredentialChanged = errors.New("credential changed concurrently")

// CreateCredentialWithRenewal persists encrypted login material and its public
// lifecycle state in one transaction, so list/scheduler views can never observe
// a configured login as unconfigured after a partial write.
func (s *Store) CreateCredentialWithRenewal(ctx context.Context, credential *domain.Credential, encryptedPayload string,
	renewal domain.CredentialRenewalStatus) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO credentials
		(name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		credential.Name, credential.Kind, credential.SiteURL, encryptedPayload, nullableTime(credential.ExpiresAt),
		"active", formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	credentialID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read credential id: %w", err)
	}
	if renewal.State == "" {
		renewal.State = "unconfigured"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO credential_renewal_state
		(credential_id,configured,enabled,state,consecutive_failures,last_attempt_at,last_success_at,next_attempt_at,
		 human_action,last_error_class,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, credentialID, boolInt(renewal.Configured),
		boolInt(renewal.Enabled), renewal.State, renewal.ConsecutiveFailures, nullableTime(renewal.LastAttemptAt),
		nullableTime(renewal.LastSuccessAt), nullableTime(renewal.NextAttemptAt), renewal.HumanAction,
		renewal.LastErrorClass, formatTime(now)); err != nil {
		return fmt.Errorf("create credential renewal state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	credential.ID = credentialID
	credential.Status = "active"
	credential.CreatedAt = now
	credential.UpdatedAt = now
	credential.Renewal = renewal
	return nil
}

func (s *Store) GetCredentialRenewal(ctx context.Context, credentialID int64) (domain.CredentialRenewalStatus, error) {
	var status domain.CredentialRenewalStatus
	var configured, enabled int
	var lastAttempt, lastSuccess, nextAttempt, updated sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT configured,enabled,state,consecutive_failures,last_attempt_at,last_success_at,
		next_attempt_at,human_action,last_error_class,updated_at FROM credential_renewal_state WHERE credential_id=?`, credentialID).
		Scan(&configured, &enabled, &status.State, &status.ConsecutiveFailures, &lastAttempt, &lastSuccess, &nextAttempt,
			&status.HumanAction, &status.LastErrorClass, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CredentialRenewalStatus{State: "unconfigured"}, nil
	}
	if err != nil {
		return domain.CredentialRenewalStatus{}, fmt.Errorf("read credential renewal state: %w", err)
	}
	status.Configured = configured != 0
	status.Enabled = enabled != 0
	status.LastAttemptAt = nullableParsedTime(lastAttempt)
	status.LastSuccessAt = nullableParsedTime(lastSuccess)
	status.NextAttemptAt = nullableParsedTime(nextAttempt)
	status.UpdatedAt = nullableParsedTime(updated)
	return status, nil
}

func (s *Store) SetCredentialRenewalState(ctx context.Context, credentialID int64, status domain.CredentialRenewalStatus) error {
	now := time.Now().UTC()
	if status.State == "" {
		status.State = "unconfigured"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO credential_renewal_state
		(credential_id,configured,enabled,state,consecutive_failures,last_attempt_at,last_success_at,next_attempt_at,
		 human_action,last_error_class,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(credential_id) DO UPDATE SET configured=excluded.configured,enabled=excluded.enabled,state=excluded.state,
		consecutive_failures=excluded.consecutive_failures,last_attempt_at=excluded.last_attempt_at,
		last_success_at=excluded.last_success_at,next_attempt_at=excluded.next_attempt_at,human_action=excluded.human_action,
		last_error_class=excluded.last_error_class,updated_at=excluded.updated_at`, credentialID, boolInt(status.Configured),
		boolInt(status.Enabled), status.State, status.ConsecutiveFailures, nullableTime(status.LastAttemptAt),
		nullableTime(status.LastSuccessAt), nullableTime(status.NextAttemptAt), status.HumanAction, status.LastErrorClass,
		formatTime(now))
	if err != nil {
		return fmt.Errorf("write credential renewal state: %w", err)
	}
	return nil
}

// CommitCredentialRenewal atomically replaces a previously loaded encrypted
// credential and its public lifecycle state. The updated_at predicate prevents
// a background renewal from overwriting a concurrent manual reauthorization.
func (s *Store) CommitCredentialRenewal(ctx context.Context, credentialID int64, expectedUpdatedAt time.Time,
	encryptedPayload string, expiresAt *time.Time, credentialStatus string, renewal domain.CredentialRenewalStatus) error {
	return s.commitCredentialRenewal(ctx, credentialID, expectedUpdatedAt, encryptedPayload, expiresAt, credentialStatus, renewal, false, nil)
}

// CommitSuccessfulCredentialRenewal commits a verified replacement and makes
// every enabled supplier using it immediately due for collection in the same
// transaction. Callers can therefore wake the scheduler without exposing a
// window where the new credential is visible but stale auth evidence remains
// scheduled for a later polling interval.
func (s *Store) CommitSuccessfulCredentialRenewal(ctx context.Context, credentialID int64, expectedUpdatedAt time.Time,
	encryptedPayload string, expiresAt *time.Time, credentialStatus string, renewal domain.CredentialRenewalStatus) error {
	return s.commitCredentialRenewal(ctx, credentialID, expectedUpdatedAt, encryptedPayload, expiresAt, credentialStatus, renewal, true, nil)
}

// CommitSuccessfulBrowserCustodyRenewal additionally advances the scoped
// custody audit record in the same transaction. A candidate can therefore
// never become active without its successful submission being observable.
func (s *Store) CommitSuccessfulBrowserCustodyRenewal(ctx context.Context, credentialID int64,
	expectedUpdatedAt time.Time, encryptedPayload string, expiresAt *time.Time, credentialStatus string,
	renewal domain.CredentialRenewalStatus, tokenHash string, supplierID int64) error {
	hook := func(ctx context.Context, tx *sql.Tx, now time.Time) error {
		result, err := tx.ExecContext(ctx, `UPDATE browser_custody_tokens SET last_submission_at=?,last_success_at=?,
			submission_count=submission_count+1,last_error_class='',last_error_message='',updated_at=?
			WHERE token_hash=? AND supplier_id=? AND credential_id=? AND revoked_at IS NULL AND expires_at>?`,
			formatTime(now), formatTime(now), formatTime(now), tokenHash, supplierID, credentialID, formatTime(now))
		if err != nil {
			return fmt.Errorf("advance browser custody audit: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return errors.New("browser custody binding changed before credential commit")
		}
		return nil
	}
	return s.commitCredentialRenewal(ctx, credentialID, expectedUpdatedAt, encryptedPayload, expiresAt,
		credentialStatus, renewal, true, hook)
}

type credentialRenewalCommitHook func(context.Context, *sql.Tx, time.Time) error

func (s *Store) commitCredentialRenewal(ctx context.Context, credentialID int64, expectedUpdatedAt time.Time,
	encryptedPayload string, expiresAt *time.Time, credentialStatus string, renewal domain.CredentialRenewalStatus,
	scheduleCollection bool, hook credentialRenewalCommitHook) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE credentials SET encrypted_payload=?,expires_at=?,status=?,updated_at=?
		WHERE id=? AND updated_at=?`, encryptedPayload, nullableTime(expiresAt), credentialStatus, formatTime(now),
		credentialID, formatTime(expectedUpdatedAt))
	if err != nil {
		return fmt.Errorf("replace renewed credential: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrCredentialChanged
	}
	if renewal.State == "" {
		renewal.State = "unconfigured"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO credential_renewal_state
		(credential_id,configured,enabled,state,consecutive_failures,last_attempt_at,last_success_at,next_attempt_at,
		 human_action,last_error_class,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(credential_id) DO UPDATE SET configured=excluded.configured,enabled=excluded.enabled,state=excluded.state,
		consecutive_failures=excluded.consecutive_failures,last_attempt_at=excluded.last_attempt_at,
		last_success_at=excluded.last_success_at,next_attempt_at=excluded.next_attempt_at,human_action=excluded.human_action,
		last_error_class=excluded.last_error_class,updated_at=excluded.updated_at`, credentialID, boolInt(renewal.Configured),
		boolInt(renewal.Enabled), renewal.State, renewal.ConsecutiveFailures, nullableTime(renewal.LastAttemptAt),
		nullableTime(renewal.LastSuccessAt), nullableTime(renewal.NextAttemptAt), renewal.HumanAction,
		renewal.LastErrorClass, formatTime(now)); err != nil {
		return fmt.Errorf("commit credential renewal state: %w", err)
	}
	if scheduleCollection {
		if err := scheduleSuppliersUsingCredential(ctx, tx, credentialID, now); err != nil {
			return err
		}
	}
	if hook != nil {
		if err := hook(ctx, tx, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateCredentialPayloadAndSchedule is the equivalent transaction for token
// refresh rules that do not also carry a password-login lifecycle state.
func (s *Store) UpdateCredentialPayloadAndSchedule(ctx context.Context, credentialID int64, encryptedPayload string,
	expiresAt *time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE credentials SET encrypted_payload=?,expires_at=?,status='active',updated_at=? WHERE id=?`,
		encryptedPayload, nullableTime(expiresAt), formatTime(now), credentialID)
	if err != nil {
		return fmt.Errorf("replace refreshed credential: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if err := scheduleSuppliersUsingCredential(ctx, tx, credentialID, now); err != nil {
		return err
	}
	return tx.Commit()
}

type credentialRenewalExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scheduleSuppliersUsingCredential(ctx context.Context, execer credentialRenewalExecer, credentialID int64, now time.Time) error {
	formatted := formatTime(now)
	if _, err := execer.ExecContext(ctx, `UPDATE suppliers SET next_collection_at=?,updated_at=?
		WHERE credential_id=? AND enabled=1`, formatted, formatted, credentialID); err != nil {
		return fmt.Errorf("schedule suppliers after credential renewal: %w", err)
	}
	return nil
}

func (s *Store) ClearCredentialRenewalState(ctx context.Context, credentialID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM credential_renewal_state WHERE credential_id=?`, credentialID)
	return err
}

func (s *Store) SuppliersUsingCredential(ctx context.Context, credentialID int64) ([]domain.Supplier, error) {
	rows, err := s.db.QueryContext(ctx, supplierSelect+` WHERE credential_id=? ORDER BY id`, credentialID)
	if err != nil {
		return nil, fmt.Errorf("list suppliers using credential: %w", err)
	}
	defer rows.Close()
	var result []domain.Supplier
	for rows.Next() {
		item, err := scanSupplier(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
