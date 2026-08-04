package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

type collectionAttemptAuthority struct {
	AttemptID          any
	Protocol           int
	Incarnation        any
	LeaseGeneration    any
	CatalogDigest      any
	CommitOrdinal      any
	AutomationEligible bool
}

func collectionAttemptAuthorityTx(ctx context.Context, tx *sql.Tx, supplierID int64) (collectionAttemptAuthority, error) {
	var protocol int
	var storedDigest []byte
	if err := tx.QueryRowContext(ctx, `SELECT authority_protocol,authority_catalog_digest
		FROM collection_authority_state WHERE singleton_id=1`).Scan(&protocol, &storedDigest); err != nil {
		return collectionAttemptAuthority{}, err
	}
	if protocol != authorityProtocolV2 {
		return collectionAttemptAuthority{Protocol: authorityProtocolLegacy}, nil
	}
	if len(storedDigest) != sha256.Size {
		return collectionAttemptAuthority{}, fmt.Errorf("%w: missing catalog digest", ErrAuthorityCatalogInvalid)
	}
	var incarnation string
	var leaseGeneration int64
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT l.supplier_incarnation_uuid,l.lease_generation,
		CASE WHEN l.owner_id<>'' AND l.retired=0 AND l.leased_until_unix_ms>
		CAST((julianday('now')-2440587.5)*86400000 AS INTEGER) THEN 1 ELSE 0 END
		FROM collection_leases l JOIN suppliers s ON s.id=l.supplier_id
		JOIN supplier_incarnation_registry r ON r.incarnation_uuid=l.supplier_incarnation_uuid
			AND r.supplier_id=l.supplier_id AND r.retired_at IS NULL
		WHERE l.supplier_id=? AND s.current_incarnation_uuid=l.supplier_incarnation_uuid`, supplierID).
		Scan(&incarnation, &leaseGeneration, &active); err != nil {
		return collectionAttemptAuthority{}, fmt.Errorf("read collection authority lease: %w", err)
	}
	if active != 1 || leaseGeneration <= 0 {
		return collectionAttemptAuthority{}, errors.New("collection authority lease is not active")
	}
	var attemptID int64
	if err := tx.QueryRowContext(ctx, `SELECT next_attempt_id FROM collection_attempt_id_sequence WHERE singleton_id=1`).Scan(&attemptID); err != nil {
		return collectionAttemptAuthority{}, err
	}
	if attemptID <= 0 || attemptID == 9223372036854775807 {
		return collectionAttemptAuthority{}, errors.New("collection attempt identity exhausted")
	}
	result, err := tx.ExecContext(ctx, `UPDATE collection_attempt_id_sequence SET next_attempt_id=?
		WHERE singleton_id=1 AND next_attempt_id=?`, attemptID+1, attemptID)
	if err != nil {
		return collectionAttemptAuthority{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return collectionAttemptAuthority{}, errors.New("collection attempt sequence changed")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO collection_commit_sequences
		(supplier_id,supplier_incarnation_uuid,next_commit_ordinal,retired_at) VALUES(?,?,2,NULL)
		ON CONFLICT(supplier_id,supplier_incarnation_uuid) DO UPDATE SET
		next_commit_ordinal=collection_commit_sequences.next_commit_ordinal+1
		WHERE collection_commit_sequences.retired_at IS NULL AND
		collection_commit_sequences.next_commit_ordinal<9223372036854775807`, supplierID, incarnation); err != nil {
		return collectionAttemptAuthority{}, err
	}
	var nextOrdinal int64
	if err := tx.QueryRowContext(ctx, `SELECT next_commit_ordinal FROM collection_commit_sequences
		WHERE supplier_id=? AND supplier_incarnation_uuid=? AND retired_at IS NULL`, supplierID, incarnation).Scan(&nextOrdinal); err != nil {
		return collectionAttemptAuthority{}, err
	}
	commitOrdinal := nextOrdinal - 1
	if commitOrdinal <= 0 {
		return collectionAttemptAuthority{}, errors.New("collection commit ordinal is invalid")
	}
	return collectionAttemptAuthority{AttemptID: attemptID, Protocol: authorityProtocolV2,
		Incarnation: incarnation, LeaseGeneration: leaseGeneration, CatalogDigest: bytes.Clone(storedDigest),
		CommitOrdinal: commitOrdinal, AutomationEligible: true}, nil
}
