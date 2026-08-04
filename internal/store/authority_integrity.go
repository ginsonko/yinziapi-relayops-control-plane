package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type authorityActivationWatermark struct {
	protocol                   int
	activationUUID             string
	catalogDigest              []byte
	protocol1AttemptMaxID      int64
	protocol1AttemptCount      int64
	protocol1AttemptDigest     []byte
	protocol1ReservationMaxID  int64
	protocol1ReservationCount  int64
	protocol1ReservationDigest []byte
	protocol1ChildCount        int64
	protocol1ChildDigest       []byte
	nextAttemptIDFloor         int64
	activatedAt                string
}

func auditCollectionAuthorityDataTx(ctx context.Context, tx *sql.Tx, catalogDigest [sha256.Size]byte, full bool) error {
	state, err := readAuthorityActivationWatermarkTx(ctx, tx)
	if err != nil {
		return err
	}
	if state.protocol != authorityProtocolV2 || !bytes.Equal(state.catalogDigest, catalogDigest[:]) {
		return authorityIntegrityError("activation catalog identity differs")
	}
	if !validAuthorityUUID(state.activationUUID) {
		return authorityIntegrityError("activation UUID is invalid")
	}
	activatedAt, err := time.Parse(time.RFC3339Nano, state.activatedAt)
	if err != nil || formatTime(activatedAt) != state.activatedAt {
		return authorityIntegrityError("activation timestamp is not canonical UTC RFC3339Nano")
	}
	if state.protocol1AttemptMaxID < 0 || state.protocol1AttemptCount < 0 ||
		state.protocol1ReservationMaxID < 0 || state.protocol1ReservationCount < 0 ||
		state.protocol1ChildCount < 0 || state.nextAttemptIDFloor <= 0 ||
		state.nextAttemptIDFloor <= state.protocol1AttemptMaxID ||
		state.nextAttemptIDFloor <= state.protocol1ReservationMaxID {
		return authorityIntegrityError("activation watermark bounds are invalid")
	}
	for name, digest := range map[string][]byte{
		"catalog": state.catalogDigest, "protocol-1 attempts": state.protocol1AttemptDigest,
		"protocol-1 reservations": state.protocol1ReservationDigest, "protocol-1 children": state.protocol1ChildDigest,
	} {
		if len(digest) != sha256.Size {
			return authorityIntegrityError("%s digest is missing", name)
		}
	}
	if full {
		if err := auditProtocolOneWatermarksTx(ctx, tx, state); err != nil {
			return err
		}
	}
	return auditAuthorityCarrierRowsTx(ctx, tx, state, catalogDigest)
}

func readAuthorityActivationWatermarkTx(ctx context.Context, tx *sql.Tx) (authorityActivationWatermark, error) {
	var singletonCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM collection_authority_state`).Scan(&singletonCount); err != nil {
		return authorityActivationWatermark{}, err
	}
	if singletonCount != 1 {
		return authorityActivationWatermark{}, authorityIntegrityError("activation singleton count is %d", singletonCount)
	}
	var state authorityActivationWatermark
	err := tx.QueryRowContext(ctx, `SELECT authority_protocol,activation_uuid,authority_catalog_digest,
		protocol1_attempt_max_id,protocol1_attempt_count,protocol1_attempt_digest,
		protocol1_reservation_max_id,protocol1_reservation_count,protocol1_reservation_digest,
		protocol1_child_row_count,protocol1_child_digest,next_attempt_id_floor,activated_at
		FROM collection_authority_state WHERE singleton_id=1`).Scan(&state.protocol, &state.activationUUID,
		&state.catalogDigest, &state.protocol1AttemptMaxID, &state.protocol1AttemptCount, &state.protocol1AttemptDigest,
		&state.protocol1ReservationMaxID, &state.protocol1ReservationCount, &state.protocol1ReservationDigest,
		&state.protocol1ChildCount, &state.protocol1ChildDigest, &state.nextAttemptIDFloor, &state.activatedAt)
	if err != nil {
		return authorityActivationWatermark{}, authorityIntegrityError("cannot read activation watermark: %v", err)
	}
	return state, nil
}

func auditProtocolOneWatermarksTx(ctx context.Context, tx *sql.Tx, state authorityActivationWatermark) error {
	attemptColumns := []string{"id", "supplier_id", "status", "error_class", "error_message", "catalog_updated",
		"balance_updated", "started_at", "finished_at", "duration_ms"}
	attempts, err := tableReceiptWhereTx(ctx, tx, "collection_attempts", "protocol1-attempts", "1", attemptColumns,
		`authority_protocol=1`)
	if err != nil {
		return err
	}
	var attemptCount, attemptMax int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(id),0) FROM collection_attempts
		WHERE authority_protocol=1`).Scan(&attemptCount, &attemptMax); err != nil {
		return err
	}
	if attemptCount != state.protocol1AttemptCount || attemptMax != state.protocol1AttemptMaxID ||
		!bytes.Equal(attempts.AggregateDigest[:], state.protocol1AttemptDigest) {
		return authorityIntegrityError("protocol-1 attempt watermark differs")
	}

	reservationColumns := []string{"request_uuid", "attempt_id", "supplier_id", "authority_protocol", "supplier_incarnation_uuid",
		"lease_generation", "lease_schema_digest", "state", "created_at", "resolved_at"}
	reservations, err := tableReceiptWhereTx(ctx, tx, "collection_attempt_reservations", "protocol1-reservations", "1",
		reservationColumns, `authority_protocol=1`)
	if err != nil {
		return err
	}
	reservationDigest := reservations.AggregateDigest
	if len(reservations.Rows) == 0 {
		reservationDigest = emptyAuthorityDigest("protocol1-reservations")
	}
	var reservationCount, reservationMax int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(attempt_id),0)
		FROM collection_attempt_reservations WHERE authority_protocol=1`).
		Scan(&reservationCount, &reservationMax); err != nil {
		return err
	}
	if reservationCount != state.protocol1ReservationCount || reservationMax != state.protocol1ReservationMaxID ||
		!bytes.Equal(reservationDigest[:], state.protocol1ReservationDigest) {
		return authorityIntegrityError("protocol-1 reservation watermark differs")
	}

	children, childCount, err := protocolOneChildReceiptTx(ctx, tx)
	if err != nil {
		return err
	}
	if childCount != state.protocol1ChildCount || !bytes.Equal(children[:], state.protocol1ChildDigest) {
		return authorityIntegrityError("protocol-1 child watermark differs")
	}
	return nil
}

func auditAuthorityCarrierRowsTx(ctx context.Context, tx *sql.Tx, state authorityActivationWatermark,
	catalogDigest [sha256.Size]byte) error {
	checks := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "supplier incarnation", query: `SELECT COUNT(*) FROM suppliers s
			LEFT JOIN supplier_incarnation_registry r ON r.supplier_id=s.id
				AND r.incarnation_uuid=s.current_incarnation_uuid AND r.retired_at IS NULL
			WHERE s.current_incarnation_uuid IS NULL OR s.current_incarnation_uuid='' OR r.incarnation_uuid IS NULL`},
		{name: "active registry cardinality", query: `SELECT COUNT(*) FROM (
			SELECT s.id FROM suppliers s LEFT JOIN supplier_incarnation_registry r
				ON r.supplier_id=s.id AND r.retired_at IS NULL
			GROUP BY s.id HAVING COUNT(r.incarnation_uuid)<>1)`},
		{name: "orphan active registry", query: `SELECT COUNT(*) FROM supplier_incarnation_registry r
			LEFT JOIN suppliers s ON s.id=r.supplier_id
			WHERE r.retired_at IS NULL AND (s.id IS NULL OR s.current_incarnation_uuid<>r.incarnation_uuid)`},
		{name: "lease incarnation", query: `SELECT COUNT(*) FROM collection_leases l
			LEFT JOIN supplier_incarnation_registry r ON r.supplier_id=l.supplier_id
				AND r.incarnation_uuid=l.supplier_incarnation_uuid
			LEFT JOIN suppliers s ON s.id=l.supplier_id
			WHERE l.supplier_incarnation_uuid IS NULL OR l.supplier_incarnation_uuid='' OR
				l.lease_generation<=0 OR r.incarnation_uuid IS NULL OR
				(l.retired=0 AND (s.id IS NULL OR r.retired_at IS NOT NULL OR
					s.current_incarnation_uuid IS NULL OR s.current_incarnation_uuid<>l.supplier_incarnation_uuid)) OR
				(l.retired=1 AND (r.retired_at IS NULL OR l.owner_id<>'' OR
					(s.id IS NOT NULL AND s.current_incarnation_uuid=l.supplier_incarnation_uuid)))`},
		{name: "protocol-1 attempt boundary", query: `SELECT COUNT(*) FROM collection_attempts
			WHERE authority_protocol=1 AND (id>? OR automation_eligible<>0 OR supplier_incarnation_uuid IS NOT NULL OR
				lease_generation IS NOT NULL OR lease_schema_digest IS NOT NULL OR commit_ordinal IS NOT NULL)`,
			args: []any{state.protocol1AttemptMaxID}},
		{name: "protocol-2 attempt carrier", query: `SELECT COUNT(*) FROM collection_attempts a
			LEFT JOIN supplier_incarnation_registry r ON r.supplier_id=a.supplier_id
				AND r.incarnation_uuid=a.supplier_incarnation_uuid
			LEFT JOIN collection_leases l ON l.supplier_id=a.supplier_id
				AND l.supplier_incarnation_uuid=a.supplier_incarnation_uuid
			WHERE a.authority_protocol=2 AND (a.id<? OR a.supplier_incarnation_uuid IS NULL OR
				a.supplier_incarnation_uuid='' OR a.lease_generation IS NULL OR a.lease_generation<=0 OR
				a.lease_schema_digest IS NULL OR length(a.lease_schema_digest)<>32 OR a.lease_schema_digest<>? OR
				a.commit_ordinal IS NULL OR a.commit_ordinal<=0 OR a.automation_eligible<>1 OR r.incarnation_uuid IS NULL OR
				l.supplier_id IS NULL OR a.lease_generation>l.lease_generation)`,
			args: []any{state.nextAttemptIDFloor, catalogDigest[:]}},
		{name: "unknown attempt protocol", query: `SELECT COUNT(*) FROM collection_attempts
			WHERE authority_protocol NOT IN (1,2)`},
		{name: "duplicate commit ordinal", query: `SELECT COUNT(*) FROM (
			SELECT supplier_id,supplier_incarnation_uuid,commit_ordinal FROM collection_attempts
			WHERE authority_protocol=2 GROUP BY supplier_id,supplier_incarnation_uuid,commit_ordinal HAVING COUNT(*)<>1)`},
		{name: "attempt commit sequence", query: `SELECT COUNT(*) FROM collection_attempts a
			LEFT JOIN collection_commit_sequences c ON c.supplier_id=a.supplier_id
				AND c.supplier_incarnation_uuid=a.supplier_incarnation_uuid
			WHERE a.authority_protocol=2 AND (c.supplier_id IS NULL OR c.next_commit_ordinal<=a.commit_ordinal)`},
		{name: "commit sequence incarnation", query: `SELECT COUNT(*) FROM collection_commit_sequences c
			LEFT JOIN supplier_incarnation_registry r ON r.supplier_id=c.supplier_id
				AND r.incarnation_uuid=c.supplier_incarnation_uuid
			WHERE c.next_commit_ordinal<=0 OR r.incarnation_uuid IS NULL OR
				(c.retired_at IS NULL AND r.retired_at IS NOT NULL) OR
				(c.retired_at IS NOT NULL AND r.retired_at IS NULL)`},
		{name: "protocol-1 reservation boundary", query: `SELECT COUNT(*) FROM collection_attempt_reservations
			WHERE authority_protocol=1 AND (attempt_id>? OR state='reserved' OR supplier_incarnation_uuid IS NOT NULL OR
				lease_generation IS NOT NULL OR lease_schema_digest IS NOT NULL)`,
			args: []any{state.protocol1ReservationMaxID}},
		{name: "protocol-2 reservation carrier", query: `SELECT COUNT(*) FROM collection_attempt_reservations q
			LEFT JOIN supplier_incarnation_registry r ON r.supplier_id=q.supplier_id
				AND r.incarnation_uuid=q.supplier_incarnation_uuid
			LEFT JOIN collection_leases l ON l.supplier_id=q.supplier_id
				AND l.supplier_incarnation_uuid=q.supplier_incarnation_uuid
			LEFT JOIN collection_attempts a ON a.id=q.attempt_id
			WHERE q.authority_protocol=2 AND (q.attempt_id<? OR q.supplier_incarnation_uuid IS NULL OR q.supplier_incarnation_uuid='' OR
				q.lease_generation IS NULL OR q.lease_generation<=0 OR q.lease_schema_digest IS NULL OR
				length(q.lease_schema_digest)<>32 OR q.lease_schema_digest<>? OR r.incarnation_uuid IS NULL OR
				l.supplier_id IS NULL OR q.lease_generation>l.lease_generation OR
				(a.id IS NOT NULL AND (q.state<>'committed' OR a.authority_protocol<>2 OR a.supplier_id<>q.supplier_id OR
					a.supplier_incarnation_uuid<>q.supplier_incarnation_uuid OR a.lease_generation<>q.lease_generation OR
					a.lease_schema_digest<>q.lease_schema_digest)) OR (q.state='committed' AND a.id IS NULL))`,
			args: []any{state.nextAttemptIDFloor, catalogDigest[:]}},
		{name: "unknown reservation protocol", query: `SELECT COUNT(*) FROM collection_attempt_reservations
			WHERE authority_protocol NOT IN (1,2)`},
		{name: "proof parent carrier", query: `SELECT COUNT(*) FROM catalog_group_collection_proofs p
			LEFT JOIN collection_attempts a ON a.id=p.attempt_id
			WHERE a.id IS NULL OR p.supplier_id<>a.supplier_id OR p.authority_protocol<>a.authority_protocol OR
				(a.authority_protocol=1 AND p.supplier_incarnation_uuid IS NOT NULL) OR
				(a.authority_protocol=2 AND (p.supplier_incarnation_uuid IS NULL OR
					p.supplier_incarnation_uuid<>a.supplier_incarnation_uuid))`},
		{name: "request evidence parent", query: `SELECT COUNT(*) FROM request_evidence e
			LEFT JOIN collection_attempts a ON a.id=e.attempt_id
			WHERE a.id IS NULL OR e.supplier_id<>a.supplier_id`},
	}
	for _, check := range checks {
		var invalid int64
		if err := tx.QueryRowContext(ctx, check.query, check.args...).Scan(&invalid); err != nil {
			return fmt.Errorf("audit %s: %w", check.name, err)
		}
		if invalid != 0 {
			return authorityIntegrityError("%s has %d invalid rows", check.name, invalid)
		}
	}

	var sequenceCount, nextAttemptID, maxAttemptID, maxReservationID int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(next_attempt_id),0)
		FROM collection_attempt_id_sequence WHERE singleton_id=1`).Scan(&sequenceCount, &nextAttemptID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM collection_attempts`).Scan(&maxAttemptID); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_id),0) FROM collection_attempt_reservations`).Scan(&maxReservationID); err != nil {
		return err
	}
	if sequenceCount != 1 || nextAttemptID < state.nextAttemptIDFloor || nextAttemptID <= maxAttemptID ||
		nextAttemptID <= maxReservationID {
		return authorityIntegrityError("attempt identity sequence is behind its durable floor")
	}
	return nil
}

func validAuthorityUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.NewReplacer("-", "").Replace(value)
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16 && decoded[6]>>4 == 4 && decoded[8]&0xc0 == 0x80
}

func authorityIntegrityError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAuthorityCatalogInvalid, fmt.Sprintf(format, args...))
}
