package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type BrowserCustodyBinding struct {
	domain.BrowserCustodyStatus
	TokenHash string `json:"-"`
}

const browserCustodySelect = `SELECT token_hash,supplier_id,credential_id,expected_origin,helper_version,expires_at,revoked_at,
	last_check_at,last_submission_at,last_success_at,submission_count,last_error_class,last_error_message,
	created_at,updated_at FROM browser_custody_tokens`

type browserCustodyScanner interface {
	Scan(...any) error
}

func scanBrowserCustody(row browserCustodyScanner, now time.Time) (BrowserCustodyBinding, error) {
	var item BrowserCustodyBinding
	var expires, created, updated string
	var revoked, lastCheck, lastSubmission, lastSuccess sql.NullString
	if err := row.Scan(&item.TokenHash, &item.SupplierID, &item.CredentialID, &item.ExpectedOrigin, &item.HelperVersion, &expires,
		&revoked, &lastCheck, &lastSubmission, &lastSuccess, &item.SubmissionCount, &item.LastErrorClass,
		&item.LastErrorMessage, &created, &updated); err != nil {
		return BrowserCustodyBinding{}, err
	}
	item.ExpiresAt = parseTime(expires)
	item.RevokedAt = nullableParsedTime(revoked)
	item.LastCheckAt = nullableParsedTime(lastCheck)
	item.LastSubmissionAt = nullableParsedTime(lastSubmission)
	item.LastSuccessAt = nullableParsedTime(lastSuccess)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	item.Enabled = item.RevokedAt == nil && item.ExpiresAt.After(now)
	return item, nil
}

func (s *Store) GetBrowserCustodyByToken(ctx context.Context, tokenHash string, now time.Time) (BrowserCustodyBinding, error) {
	if strings.TrimSpace(tokenHash) == "" {
		return BrowserCustodyBinding{}, sql.ErrNoRows
	}
	return scanBrowserCustody(s.db.QueryRowContext(ctx, browserCustodySelect+` WHERE token_hash=?`, tokenHash), now)
}

func (s *Store) GetActiveBrowserCustodyByToken(ctx context.Context, tokenHash string, now time.Time) (BrowserCustodyBinding, error) {
	item, err := s.GetBrowserCustodyByToken(ctx, tokenHash, now)
	if err != nil {
		return BrowserCustodyBinding{}, err
	}
	if !item.Enabled {
		return BrowserCustodyBinding{}, sql.ErrNoRows
	}
	return item, nil
}

func (s *Store) GetBrowserCustodyForSupplier(ctx context.Context, supplierID int64, now time.Time) (BrowserCustodyBinding, error) {
	return scanBrowserCustody(s.db.QueryRowContext(ctx, browserCustodySelect+` WHERE supplier_id=?`, supplierID), now)
}

func (s *Store) ListBrowserCustodies(ctx context.Context, now time.Time) ([]BrowserCustodyBinding, error) {
	rows, err := s.db.QueryContext(ctx, browserCustodySelect+` ORDER BY supplier_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]BrowserCustodyBinding, 0)
	for rows.Next() {
		item, err := scanBrowserCustody(rows, now)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func upsertBrowserCustodyTx(ctx context.Context, tx *sql.Tx, tokenHash string, supplierID, credentialID int64,
	expectedOrigin string, expiresAt, now time.Time) error {
	if strings.TrimSpace(tokenHash) == "" || supplierID <= 0 || credentialID <= 0 ||
		strings.TrimSpace(expectedOrigin) == "" || !expiresAt.After(now) {
		return errors.New("browser custody binding is incomplete")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO browser_custody_tokens
		(token_hash,supplier_id,credential_id,expected_origin,expires_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(supplier_id) DO UPDATE SET
		token_hash=excluded.token_hash,credential_id=excluded.credential_id,expected_origin=excluded.expected_origin,
		expires_at=excluded.expires_at,revoked_at=NULL,last_check_at=NULL,last_submission_at=NULL,last_success_at=NULL,
		submission_count=0,last_error_class='',last_error_message='',helper_version='',created_at=excluded.created_at,updated_at=excluded.updated_at`,
		tokenHash, supplierID, credentialID, expectedOrigin, formatTime(expiresAt), formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create browser custody binding: %w", err)
	}
	return nil
}

func (s *Store) RecordBrowserCustodyCheck(ctx context.Context, tokenHash, helperVersion string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE browser_custody_tokens SET last_check_at=?,helper_version=?,updated_at=?
		WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`, formatTime(now), helperVersion, formatTime(now), tokenHash, formatTime(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RecordBrowserCustodyFailure(ctx context.Context, tokenHash, errorClass, safeMessage string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE browser_custody_tokens SET last_submission_at=?,submission_count=submission_count+1,
		last_error_class=?,last_error_message=?,updated_at=?
		WHERE token_hash=? AND revoked_at IS NULL AND expires_at>?`, formatTime(now), errorClass, safeMessage,
		formatTime(now), tokenHash, formatTime(now))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RevokeBrowserCustodyBySupplier(ctx context.Context, supplierID int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE browser_custody_tokens SET revoked_at=?,updated_at=?
		WHERE supplier_id=? AND revoked_at IS NULL`, formatTime(now), formatTime(now), supplierID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RevokeBrowserCustodyByToken(ctx context.Context, tokenHash string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE browser_custody_tokens SET revoked_at=?,updated_at=?
		WHERE token_hash=? AND revoked_at IS NULL`, formatTime(now), formatTime(now), tokenHash)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
