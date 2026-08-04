package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

var (
	ErrTargetControlLeaseBusy   = errors.New("target control dimension is busy")
	ErrTargetControlFenceLost   = errors.New("target control fencing lease was lost")
	ErrTargetControlIdempotency = errors.New("idempotency key was already used for a different request")
)

func (s *Store) AcquireTargetControlLease(ctx context.Context, targetID int64, dimension, ownerID string, now time.Time, ttl time.Duration) (domain.TargetControlLease, error) {
	dimension = strings.TrimSpace(dimension)
	ownerID = strings.TrimSpace(ownerID)
	if targetID <= 0 || dimension == "" || ownerID == "" {
		return domain.TargetControlLease{}, errors.New("target, dimension and lease owner are required")
	}
	if len(dimension) > 256 || len(ownerID) > 256 {
		return domain.TargetControlLease{}, errors.New("target control lease key is too long")
	}
	if ttl <= 0 {
		return domain.TargetControlLease{}, errors.New("target control lease ttl must be positive")
	}
	now = now.UTC()
	until := now.Add(ttl)
	return serializedWriteResult(ctx, s, func() (domain.TargetControlLease, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return domain.TargetControlLease{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := checkReleaseAdmissionOpenTx(ctx, tx); err != nil {
			return domain.TargetControlLease{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO target_control_leases
			(target_site_id,dimension,owner_id,fencing_token,leased_until,updated_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(target_site_id,dimension) DO UPDATE SET
			owner_id=excluded.owner_id,fencing_token=target_control_leases.fencing_token+1,
			leased_until=excluded.leased_until,updated_at=excluded.updated_at
			WHERE target_control_leases.leased_until<=excluded.updated_at`, targetID, dimension, ownerID, 1,
			formatLeaseTime(until), formatLeaseTime(now))
		if err != nil {
			return domain.TargetControlLease{}, fmt.Errorf("acquire target control lease: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return domain.TargetControlLease{}, err
		}
		if count != 1 {
			return domain.TargetControlLease{}, ErrTargetControlLeaseBusy
		}
		var token int64
		var storedUntil string
		if err := tx.QueryRowContext(ctx, `SELECT fencing_token,leased_until FROM target_control_leases
			WHERE target_site_id=? AND dimension=? AND owner_id=?`, targetID, dimension, ownerID).Scan(&token, &storedUntil); err != nil {
			return domain.TargetControlLease{}, fmt.Errorf("read acquired target control lease: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return domain.TargetControlLease{}, err
		}
		return domain.TargetControlLease{TargetSiteID: targetID, Dimension: dimension, OwnerID: ownerID,
			FencingToken: token, LeasedUntil: parseTime(storedUntil)}, nil
	})
}

func (s *Store) RenewTargetControlLease(ctx context.Context, lease domain.TargetControlLease, now time.Time, ttl time.Duration) (domain.TargetControlLease, error) {
	if ttl <= 0 {
		return domain.TargetControlLease{}, errors.New("target control lease ttl must be positive")
	}
	now = now.UTC()
	until := now.Add(ttl)
	return serializedWriteResult(ctx, s, func() (domain.TargetControlLease, error) {
		result, err := s.db.ExecContext(ctx, `UPDATE target_control_leases SET leased_until=?,updated_at=?
			WHERE target_site_id=? AND dimension=? AND owner_id=? AND fencing_token=? AND leased_until>?`,
			formatLeaseTime(until), formatLeaseTime(now), lease.TargetSiteID, lease.Dimension, lease.OwnerID,
			lease.FencingToken, formatLeaseTime(now))
		if err != nil {
			return domain.TargetControlLease{}, err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return domain.TargetControlLease{}, ErrTargetControlFenceLost
		}
		updated := lease
		updated.LeasedUntil = until
		return updated, nil
	})
}

func (s *Store) CheckTargetControlLease(ctx context.Context, lease domain.TargetControlLease, now time.Time) error {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM target_control_leases
		WHERE target_site_id=? AND dimension=? AND owner_id=? AND fencing_token=? AND leased_until>?`,
		lease.TargetSiteID, lease.Dimension, lease.OwnerID, lease.FencingToken, formatLeaseTime(now.UTC())).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrTargetControlFenceLost
	}
	return nil
}

func (s *Store) ReleaseTargetControlLease(ctx context.Context, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		result, err := s.db.ExecContext(ctx, `UPDATE target_control_leases SET owner_id='',leased_until=?,updated_at=?
			WHERE target_site_id=? AND dimension=? AND owner_id=? AND fencing_token=?`, formatLeaseTime(now.UTC()),
			formatLeaseTime(now.UTC()), lease.TargetSiteID, lease.Dimension, lease.OwnerID, lease.FencingToken)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ErrTargetControlFenceLost
		}
		return nil
	})
}

func (s *Store) CreateTargetControlOperation(ctx context.Context, operation *domain.TargetControlOperation) error {
	if operation == nil || operation.TargetSiteID <= 0 || strings.TrimSpace(operation.IdempotencyKey) == "" || strings.TrimSpace(operation.RequestHash) == "" {
		return errors.New("target control operation identity is required")
	}
	if len(operation.IdempotencyKey) > 256 || len(operation.Dimension) > 256 || len(operation.Action) > 128 {
		return errors.New("target control operation identity is too long")
	}
	now := time.Now().UTC()
	if operation.StartedAt.IsZero() {
		operation.StartedAt = now
	}
	operation.UpdatedAt = now
	if operation.ResultState == "" {
		operation.ResultState = "in_progress"
	}
	id, err := serializedWriteResult(ctx, s, func() (int64, error) {
		result, err := s.db.ExecContext(ctx, `INSERT INTO target_control_operations
			(target_site_id,dimension,action,idempotency_key,request_hash,expected_hash,desired_hash,readback_hash,
			result_state,fencing_token,result_json,error,started_at,finished_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, operation.TargetSiteID, operation.Dimension, operation.Action,
			operation.IdempotencyKey, operation.RequestHash, operation.ExpectedHash, operation.DesiredHash,
			operation.ReadbackHash, operation.ResultState, operation.FencingToken, operation.ResultJSON, operation.Error,
			formatTime(operation.StartedAt), nullableTime(operation.FinishedAt), formatTime(now))
		if err != nil {
			return 0, err
		}
		return result.LastInsertId()
	})
	if err != nil {
		return err
	}
	operation.ID = id
	return nil
}

func (s *Store) GetTargetControlOperationByKey(ctx context.Context, targetID int64, key string) (domain.TargetControlOperation, error) {
	return scanTargetControlOperation(s.db.QueryRowContext(ctx, targetControlOperationSelect+
		` WHERE target_site_id=? AND idempotency_key=?`, targetID, key))
}

func (s *Store) GetTargetControlOperation(ctx context.Context, id int64) (domain.TargetControlOperation, error) {
	return scanTargetControlOperation(s.db.QueryRowContext(ctx, targetControlOperationSelect+` WHERE id=?`, id))
}

func (s *Store) LatestTargetControlOperation(ctx context.Context, targetID int64, dimension, action string) (domain.TargetControlOperation, error) {
	return scanTargetControlOperation(s.db.QueryRowContext(ctx, targetControlOperationSelect+
		` WHERE target_site_id=? AND dimension=? AND action=? ORDER BY started_at DESC,id DESC LIMIT 1`, targetID, dimension, action))
}

func (s *Store) ListTargetControlOperations(ctx context.Context, targetID int64, limit int) ([]domain.TargetControlOperation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, targetControlOperationSelect+
		` WHERE target_site_id=? ORDER BY started_at DESC,id DESC LIMIT ?`, targetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.TargetControlOperation, 0)
	for rows.Next() {
		item, err := scanTargetControlOperation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ResumeTargetControlOperation(ctx context.Context, id int64, requestHash string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		result, err := s.db.ExecContext(ctx, `UPDATE target_control_operations SET result_state='in_progress',fencing_token=?,
			readback_hash='',result_json='',error='',finished_at=NULL,updated_at=? WHERE id=? AND request_hash=?`,
			lease.FencingToken, formatTime(now.UTC()), id, requestHash)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ErrTargetControlIdempotency
		}
		return nil
	})
}

func (s *Store) FinishTargetControlOperation(ctx context.Context, operationID int64, lease domain.TargetControlLease,
	state, readbackHash, resultJSON, message string, now time.Time) error {
	if !isTargetControlTerminalState(state) {
		return fmt.Errorf("invalid target control terminal state %q", state)
	}
	return serializedWrite(ctx, s, func() error {
		result, err := s.db.ExecContext(ctx, `UPDATE target_control_operations SET result_state=?,readback_hash=?,result_json=?,
			error=?,finished_at=?,updated_at=? WHERE id=? AND fencing_token=? AND EXISTS (
			SELECT 1 FROM target_control_leases l WHERE l.target_site_id=target_control_operations.target_site_id
			AND l.dimension=target_control_operations.dimension AND l.owner_id=? AND l.fencing_token=? AND l.leased_until>?)`,
			state, readbackHash, resultJSON, message, formatTime(now.UTC()), formatTime(now.UTC()), operationID,
			lease.FencingToken, lease.OwnerID, lease.FencingToken, formatLeaseTime(now.UTC()))
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ErrTargetControlFenceLost
		}
		return nil
	})
}

func isTargetControlTerminalState(state string) bool {
	switch state {
	case "verified_success", "rejected_before_write", "conflict_before_write", "ambiguous_after_write", "rolled_back_verified", "compensation_failed":
		return true
	default:
		return false
	}
}

const targetControlOperationSelect = `SELECT id,target_site_id,dimension,action,idempotency_key,request_hash,
	expected_hash,desired_hash,readback_hash,result_state,fencing_token,result_json,error,started_at,finished_at,updated_at
	FROM target_control_operations`

func scanTargetControlOperation(row scanner) (domain.TargetControlOperation, error) {
	var operation domain.TargetControlOperation
	var startedAt, updatedAt string
	var finishedAt sql.NullString
	if err := row.Scan(&operation.ID, &operation.TargetSiteID, &operation.Dimension, &operation.Action,
		&operation.IdempotencyKey, &operation.RequestHash, &operation.ExpectedHash, &operation.DesiredHash,
		&operation.ReadbackHash, &operation.ResultState, &operation.FencingToken, &operation.ResultJSON,
		&operation.Error, &startedAt, &finishedAt, &updatedAt); err != nil {
		return domain.TargetControlOperation{}, err
	}
	operation.StartedAt = parseTime(startedAt)
	operation.UpdatedAt = parseTime(updatedAt)
	if finishedAt.Valid {
		value := parseTime(finishedAt.String)
		operation.FinishedAt = &value
	}
	if strings.TrimSpace(operation.ResultJSON) != "" {
		var value any
		if json.Unmarshal([]byte(operation.ResultJSON), &value) == nil {
			operation.Result = value
		}
	}
	return operation, nil
}

// ReplaceTargetCredentialAtomic persists and binds a pre-validated credential
// without changing the target's administrator-controlled enabled state.
func (s *Store) ReplaceTargetCredentialAtomic(ctx context.Context, targetID, expectedCredentialID int64,
	expectedTargetUpdatedAt, expectedCredentialUpdatedAt time.Time, credential *domain.Credential, encryptedPayload string) (time.Time, error) {
	if credential == nil || targetID <= 0 || expectedCredentialID <= 0 || expectedTargetUpdatedAt.IsZero() || expectedCredentialUpdatedAt.IsZero() {
		return time.Time{}, errors.New("target and credential versions are required")
	}
	now := time.Now().UTC()
	credentialID, err := serializedWriteResult(ctx, s, func() (int64, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		var currentCredentialID int64
		var targetUpdatedAt string
		if err := tx.QueryRowContext(ctx, `SELECT credential_id,updated_at FROM target_sites WHERE id=?`, targetID).
			Scan(&currentCredentialID, &targetUpdatedAt); err != nil {
			return 0, err
		}
		if currentCredentialID != expectedCredentialID || !parseTime(targetUpdatedAt).Equal(expectedTargetUpdatedAt) {
			return 0, errors.New("target changed since reauthorization was opened")
		}
		var credentialUpdatedAt string
		if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM credentials WHERE id=?`, expectedCredentialID).Scan(&credentialUpdatedAt); err != nil {
			return 0, err
		}
		if !parseTime(credentialUpdatedAt).Equal(expectedCredentialUpdatedAt) {
			return 0, errors.New("target credential changed since reauthorization was opened")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO credentials
			(name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			credential.Name, credential.Kind, credential.SiteURL, encryptedPayload, nullableTime(credential.ExpiresAt), "active",
			formatTime(now), formatTime(now))
		if err != nil {
			return 0, err
		}
		credentialID, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE target_sites SET credential_id=?,updated_at=?
			WHERE id=? AND credential_id=? AND updated_at=?`, credentialID, formatTime(now), targetID,
			expectedCredentialID, targetUpdatedAt)
		if err != nil {
			return 0, err
		}
		count, _ := updated.RowsAffected()
		if count != 1 {
			return 0, errors.New("target changed while replacing its credential")
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return credentialID, nil
	})
	if err != nil {
		return time.Time{}, err
	}
	credential.ID = credentialID
	credential.Status = "active"
	credential.CreatedAt = now
	credential.UpdatedAt = now
	return now, nil
}

func (s *Store) RestoreTargetCredentialAtomic(ctx context.Context, targetID, newCredentialID, oldCredentialID int64,
	expectedTargetUpdatedAt time.Time) (time.Time, error) {
	now := time.Now().UTC()
	return serializedWriteResult(ctx, s, func() (time.Time, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return time.Time{}, err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.ExecContext(ctx, `UPDATE target_sites SET credential_id=?,updated_at=?
			WHERE id=? AND credential_id=? AND updated_at=?`, oldCredentialID, formatTime(now), targetID,
			newCredentialID, formatTime(expectedTargetUpdatedAt))
		if err != nil {
			return time.Time{}, err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return time.Time{}, errors.New("target changed before credential compensation")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET status='rejected',updated_at=? WHERE id=?`, formatTime(now), newCredentialID); err != nil {
			return time.Time{}, err
		}
		if err := tx.Commit(); err != nil {
			return time.Time{}, err
		}
		return now, nil
	})
}
