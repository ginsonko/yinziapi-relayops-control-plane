package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RetentionPolicy uses zero to disable cleanup for a data class. The latest
// supplier/entity state is always retained even if it is older than the window.
type RetentionPolicy struct {
	OperatingLedger      time.Duration
	CatalogSnapshots     time.Duration
	PriceObservations    time.Duration
	BalanceObservations  time.Duration
	Activities           time.Duration
	PairingTokens        time.Duration
	CollectionAttempts   time.Duration
	CollectionKeepLatest int
	BatchSize            int
}

type RetentionResult struct {
	OperatingLedger     int64 `json:"operating_ledger"`
	CatalogSnapshots    int64 `json:"catalog_snapshots"`
	PriceObservations   int64 `json:"price_observations"`
	BalanceObservations int64 `json:"balance_observations"`
	Activities          int64 `json:"activities"`
	PairingTokens       int64 `json:"pairing_tokens"`
	CollectionAttempts  int64 `json:"collection_attempts"`
}

// DefaultRetentionPolicy keeps a compact operational history while retaining
// the latest state and current successful collection proof separately.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		OperatingLedger:      8 * 24 * time.Hour,
		CatalogSnapshots:     7 * 24 * time.Hour,
		PriceObservations:    7 * 24 * time.Hour,
		BalanceObservations:  7 * 24 * time.Hour,
		Activities:           7 * 24 * time.Hour,
		PairingTokens:        24 * time.Hour,
		CollectionAttempts:   12 * time.Hour,
		CollectionKeepLatest: 10,
		BatchSize:            250,
	}
}

func (s *Store) CleanupRetention(ctx context.Context, now time.Time, policy RetentionPolicy) (RetentionResult, error) {
	if err := validateRetentionPolicy(policy); err != nil {
		return RetentionResult{}, err
	}
	batchSize := policy.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	keepLatest := policy.CollectionKeepLatest
	if keepLatest <= 0 {
		keepLatest = 1
	}
	var result RetentionResult
	run := func(destination *int64, query string, args ...any) error {
		count, err := s.cleanupRetentionDelete(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("cleanup retained history: %w", err)
		}
		*destination = count
		return nil
	}
	if policy.OperatingLedger > 0 {
		cutoff := formatTime(now.Add(-policy.OperatingLedger))
		if err := run(&result.OperatingLedger, `DELETE FROM operating_ledger_events
			WHERE event_id IN (
				SELECT event_id FROM operating_ledger_events ledger
				WHERE occurred_at<?
				AND event_id NOT IN (
					SELECT related_event_id FROM operating_ledger_events
					WHERE occurred_at>=? AND related_event_id!=''
				)
				AND NOT EXISTS (
					SELECT 1 FROM decision_outcomes outcome
					JOIN decision_exposures exposure ON exposure.proposal_id=outcome.proposal_id
					WHERE outcome.status NOT IN ('observed','censored','no_exposure','incompatible')
					AND NOT (outcome.status='awaiting_ledger' AND COALESCE(outcome.exclusion_reason,'')='non_cash_basis')
					AND exposure.target_site_id=ledger.target_site_id
					AND lower(trim(exposure.target_group))=lower(trim(ledger.group_name))
					AND ledger.occurred_at>=exposure.effective_at
					AND ledger.occurred_at<=outcome.due_at
				)
				ORDER BY occurred_at,event_id LIMIT ?
			)`, cutoff, cutoff, batchSize); err != nil {
			return result, err
		}
	}
	if policy.CatalogSnapshots > 0 {
		if err := run(&result.CatalogSnapshots, `DELETE FROM catalog_snapshots WHERE id IN (
			SELECT id FROM catalog_snapshots
			WHERE julianday(observed_at)<julianday(?) AND id NOT IN (
				SELECT id FROM (
					SELECT id,ROW_NUMBER() OVER(PARTITION BY supplier_id ORDER BY julianday(observed_at) DESC,id DESC) AS row_num
					FROM catalog_snapshots
				) WHERE row_num=1
			) ORDER BY observed_at,id LIMIT ?
		)`, formatTime(now.Add(-policy.CatalogSnapshots)), batchSize); err != nil {
			return result, err
		}
	}
	if policy.PriceObservations > 0 {
		if err := run(&result.PriceObservations, `DELETE FROM price_observations WHERE id IN (
			SELECT id FROM price_observations
			WHERE julianday(observed_at)<julianday(?) AND id NOT IN (
				SELECT id FROM (
					SELECT id,ROW_NUMBER() OVER(
						PARTITION BY supplier_id,entity_type,entity_key,group_name ORDER BY julianday(observed_at) DESC,id DESC
					) AS row_num FROM price_observations
				) WHERE row_num=1
			) ORDER BY observed_at,id LIMIT ?
		)`, formatTime(now.Add(-policy.PriceObservations)), batchSize); err != nil {
			return result, err
		}
	}
	if policy.BalanceObservations > 0 {
		if err := run(&result.BalanceObservations, `DELETE FROM balance_observations WHERE id IN (
			SELECT id FROM balance_observations
			WHERE julianday(observed_at)<julianday(?) AND id NOT IN (
				SELECT id FROM (
					SELECT id,ROW_NUMBER() OVER(PARTITION BY supplier_id ORDER BY julianday(observed_at) DESC,id DESC) AS row_num
					FROM balance_observations
				) WHERE row_num=1
			) ORDER BY observed_at,id LIMIT ?
		)`, formatTime(now.Add(-policy.BalanceObservations)), batchSize); err != nil {
			return result, err
		}
	}
	if policy.Activities > 0 {
		if err := run(&result.Activities, `DELETE FROM activities WHERE id IN (
			SELECT id FROM activities WHERE julianday(created_at)<julianday(?) ORDER BY created_at,id LIMIT ?
		)`, formatTime(now.Add(-policy.Activities)), batchSize); err != nil {
			return result, err
		}
	}
	if policy.PairingTokens > 0 {
		cutoff := formatTime(now.Add(-policy.PairingTokens))
		if err := run(&result.PairingTokens, `DELETE FROM pairing_tokens WHERE token_hash IN (
			SELECT token_hash FROM pairing_tokens
			WHERE julianday(expires_at)<julianday(?) AND (used_at IS NULL OR julianday(used_at)<julianday(?))
			ORDER BY expires_at,token_hash LIMIT ?
		)`, cutoff, cutoff, batchSize); err != nil {
			return result, err
		}
	}
	if policy.CollectionAttempts > 0 {
		if err := run(&result.CollectionAttempts, `DELETE FROM collection_attempts WHERE id IN (
			SELECT id FROM collection_attempts a
			WHERE julianday(a.finished_at)<julianday(?)
			AND a.id NOT IN (
				SELECT id FROM (
					SELECT id,ROW_NUMBER() OVER(PARTITION BY supplier_id ORDER BY id DESC) AS row_num
					FROM collection_attempts
				) WHERE row_num<=?
			)
			AND a.id NOT IN (
				SELECT MAX(id) FROM collection_attempts
				WHERE status IN ('success','partial_success') AND catalog_updated=1 GROUP BY supplier_id
			)
			ORDER BY a.finished_at,a.id LIMIT ?
		)`, formatTime(now.Add(-policy.CollectionAttempts)), keepLatest, batchSize); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) cleanupRetentionDelete(ctx context.Context, query string, args ...any) (int64, error) {
	return serializedWriteResult(ctx, s, func() (int64, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		count, err := deleteCount(ctx, tx, query, args...)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return count, nil
	})
}

func validateRetentionPolicy(policy RetentionPolicy) error {
	for name, interval := range map[string]time.Duration{
		"operating ledger": policy.OperatingLedger, "catalog snapshots": policy.CatalogSnapshots,
		"price observations": policy.PriceObservations, "balance observations": policy.BalanceObservations,
		"activities": policy.Activities, "pairing tokens": policy.PairingTokens,
		"collection attempts": policy.CollectionAttempts,
	} {
		if interval < 0 {
			return fmt.Errorf("%s retention cannot be negative", name)
		}
	}
	if policy.CollectionKeepLatest < 0 || policy.BatchSize < 0 {
		return fmt.Errorf("retention batch and latest-attempt limits cannot be negative")
	}
	return nil
}

func deleteCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
