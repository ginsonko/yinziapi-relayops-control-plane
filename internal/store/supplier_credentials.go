package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// CreateCredentialForSupplier atomically creates a verified credential,
// initializes its renewal state, and replaces the supplier binding. The
// previous credential is deleted only when no supplier or target still uses it.
func (s *Store) CreateCredentialForSupplier(ctx context.Context, supplierID int64, credential *domain.Credential,
	encryptedPayload string, renewal domain.CredentialRenewalStatus) error {
	if credential == nil {
		return fmt.Errorf("credential is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousCredentialID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT credential_id FROM suppliers WHERE id=?`, supplierID).Scan(&previousCredentialID); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO credentials
		(name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		credential.Name, credential.Kind, credential.SiteURL, encryptedPayload, nullableTime(credential.ExpiresAt),
		"active", formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create supplier credential: %w", err)
	}
	credentialID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read supplier credential id: %w", err)
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
		return fmt.Errorf("create supplier credential renewal state: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE suppliers SET credential_id=?,updated_at=? WHERE id=?`, credentialID, formatTime(now), supplierID)
	if err != nil {
		return fmt.Errorf("bind supplier credential: %w", err)
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if previousCredentialID.Valid && previousCredentialID.Int64 != credentialID {
		if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id=?
			AND NOT EXISTS(SELECT 1 FROM suppliers WHERE credential_id=credentials.id)
			AND NOT EXISTS(SELECT 1 FROM target_sites WHERE credential_id=credentials.id)`, previousCredentialID.Int64); err != nil {
			return fmt.Errorf("remove unreferenced previous credential: %w", err)
		}
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
