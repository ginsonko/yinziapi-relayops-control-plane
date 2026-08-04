package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

var (
	ErrCatalogGroupProofIncomplete = errors.New("catalog group proof is incomplete")
	evidenceHeaderSecretRE         = regexp.MustCompile(`(?i)(authorization|cookie|x-api-key|x-token|api-key)\s*[:=]\s*(?:bearer\s+)?[^\s,;]+`)
	evidenceBearerRE               = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`)
	evidenceQuerySecretRE          = regexp.MustCompile(`(?i)([?&](?:token|access_token|api_key|apikey|key|secret)=)[^&#\s]+`)
)

// RecordCollectionAttempt commits a standalone run summary, request evidence,
// authorization capabilities and supplier status. Successful collector runs
// use CommitCollectionSuccess so their snapshot is in the same transaction;
// failed runs use this method and cannot move the last trusted snapshot.
func (s *Store) RecordCollectionAttempt(ctx context.Context, attempt *domain.CollectionAttempt, diagnostics domain.CollectionDiagnostics, nextAt time.Time) error {
	if attempt == nil {
		return errors.New("collection attempt requires a supplier")
	}
	release, err := s.acquireWriteGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	for retry := 0; ; retry++ {
		staged := *attempt
		err := s.recordCollectionAttemptOnce(ctx, &staged, diagnostics, nextAt)
		if err == nil {
			*attempt = staged
			return nil
		}
		if !isSQLiteBusy(err) || retry >= 2 {
			return err
		}
		delay := time.Duration(25*(1<<retry)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) recordCollectionAttemptOnce(ctx context.Context, attempt *domain.CollectionAttempt,
	diagnostics domain.CollectionDiagnostics, nextAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkCollectionAuthorityTx(ctx, tx); err != nil {
		return err
	}
	if err := s.recordCollectionAttemptTx(ctx, tx, attempt, diagnostics, nextAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// CommitCollectionSuccess publishes every artifact of one successful or
// partially successful collection as a single database change. A later write
// failure therefore cannot expose a new latest snapshot without its matching
// discoveries, diagnostics and supplier success state.
func (s *Store) CommitCollectionSuccess(ctx context.Context, snapshot domain.CatalogSnapshot, attempt *domain.CollectionAttempt,
	diagnostics domain.CollectionDiagnostics, nextAt time.Time) (bool, error) {
	if attempt == nil || attempt.SupplierID <= 0 {
		return false, errors.New("collection attempt requires a supplier")
	}
	if snapshot.SupplierID <= 0 || snapshot.SupplierID != attempt.SupplierID {
		return false, errors.New("collection snapshot and attempt supplier must match")
	}
	if attempt.Status != "success" && attempt.Status != "partial_success" {
		return false, fmt.Errorf("atomic collection commit requires success or partial_success, got %q", attempt.Status)
	}
	if !attempt.CatalogUpdated {
		return false, errors.New("atomic collection commit requires a catalog update")
	}
	release, err := s.acquireWriteGate(ctx)
	if err != nil {
		return false, err
	}
	defer release()

	for retry := 0; ; retry++ {
		staged := *attempt
		changed, err := s.commitCollectionSuccessOnce(ctx, snapshot, &staged, diagnostics, nextAt)
		if err == nil {
			*attempt = staged
			return changed, nil
		}
		if !isSQLiteBusy(err) || retry >= 2 {
			return false, err
		}
		delay := time.Duration(25*(1<<retry)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) commitCollectionSuccessOnce(ctx context.Context, snapshot domain.CatalogSnapshot,
	attempt *domain.CollectionAttempt, diagnostics domain.CollectionDiagnostics, nextAt time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkCollectionAuthorityTx(ctx, tx); err != nil {
		return false, err
	}
	var previousHash string
	previousErr := tx.QueryRowContext(ctx, `SELECT response_hash FROM catalog_snapshots WHERE supplier_id=?
		ORDER BY julianday(COALESCE(NULLIF(last_seen,''),observed_at)) DESC,id DESC LIMIT 1`, snapshot.SupplierID).Scan(&previousHash)
	if previousErr != nil && !errors.Is(previousErr, sql.ErrNoRows) {
		return false, previousErr
	}
	firstSeen := errors.Is(previousErr, sql.ErrNoRows)
	changed := !firstSeen && previousHash != snapshot.Evidence.ResponseHash

	if err := s.saveSnapshotTx(ctx, tx, snapshot); err != nil {
		return false, err
	}
	if err := s.upsertDiscoveriesTx(ctx, tx, snapshot, firstSeen); err != nil {
		return false, err
	}
	staged := *attempt
	if err := s.recordCollectionAttemptTx(ctx, tx, &staged, diagnostics, nextAt); err != nil {
		return false, err
	}
	if err := s.recordCatalogGroupProofsTx(ctx, tx, snapshot, staged, diagnostics); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	*attempt = staged
	return changed, nil
}

func (s *Store) recordCatalogGroupProofsTx(ctx context.Context, tx *sql.Tx, snapshot domain.CatalogSnapshot,
	attempt domain.CollectionAttempt, diagnostics domain.CollectionDiagnostics) error {
	if diagnostics.CatalogProof == nil {
		return nil
	}
	basis := *diagnostics.CatalogProof
	attemptID := strconv.FormatInt(attempt.ID, 10)
	endpoint := strings.TrimSpace(basis.AuthoritativeEndpoint)
	sourceRevision := strings.TrimSpace(basis.SourceRevision)
	observedAt := basis.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = snapshot.Evidence.ObservedAt.UTC()
	}
	pages := make([]mirror.CollectionPageEvidence, 0, len(basis.Pages))
	for _, page := range basis.Pages {
		pages = append(pages, mirror.CollectionPageEvidence{AttemptID: attemptID, PageNumber: page.PageNumber,
			Endpoint: endpoint, SourceRevision: sourceRevision, ItemCount: page.ItemCount,
			Terminal: page.Terminal, NextCursor: page.NextCursor})
	}
	parsedSnapshotCount := len(snapshot.Groups) + len(snapshot.Models)
	globalReasons := make([]string, 0, 6)
	if !diagnostics.CatalogSucceeded {
		globalReasons = append(globalReasons, "catalog_success_unproven")
	}
	if basis.ParsedItemCount != parsedSnapshotCount {
		globalReasons = append(globalReasons, "snapshot_parsed_count_mismatch")
	}
	groupCounts := make(map[string]int, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		groupID := strings.TrimSpace(group.ExternalID)
		if groupID == "" {
			groupID = strings.TrimSpace(group.Name)
		}
		if groupID == "" || strings.ContainsRune(groupID, '\x00') {
			globalReasons = append(globalReasons, "source_group_identity_invalid")
			continue
		}
		groupCounts[groupID]++
	}
	type proofCandidate struct {
		groupID      string
		sourceState  string
		semanticHash string
		reasons      []string
	}
	type encodedCatalogGroupProof struct {
		candidate proofCandidate
		proof     mirror.CollectionProof
		payload   []byte
	}
	candidates := make([]proofCandidate, 0, len(snapshot.Groups))
	observed := make(map[string]struct{}, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		groupID := strings.TrimSpace(group.ExternalID)
		if groupID == "" {
			groupID = strings.TrimSpace(group.Name)
		}
		if groupID == "" || strings.ContainsRune(groupID, '\x00') {
			continue
		}
		if _, duplicate := observed[groupID]; duplicate {
			continue
		}
		observed[groupID] = struct{}{}
		candidate := proofCandidate{groupID: groupID, sourceState: mirror.SourceStatePresent}
		semanticHash := ""
		extraReasons := make([]string, 0, 3)
		if len(group.ModelIDs) == 0 {
			candidate.sourceState = mirror.SourceStateEmpty
			var hashErr error
			semanticHash, hashErr = mirror.EmptySourceSemanticHash(group)
			if hashErr != nil {
				extraReasons = append(extraReasons, "empty_group_semantic_hash_invalid")
			}
		} else {
			canonical, canonicalErr := mirror.NormalizeSourceGroup(snapshot, group)
			if canonicalErr != nil {
				extraReasons = append(extraReasons, "canonical_group_invalid")
			} else if semanticHash, canonicalErr = mirror.SourceSemanticHash(canonical); canonicalErr != nil {
				extraReasons = append(extraReasons, "source_semantic_hash_invalid")
			}
		}
		if groupCounts[groupID] != 1 {
			extraReasons = append(extraReasons, "source_group_identity_ambiguous")
			globalReasons = append(globalReasons, "source_group_identity_ambiguous")
		}
		if len(extraReasons) > 0 {
			globalReasons = append(globalReasons, "catalog_group_canonicalization_failed")
		}
		candidate.semanticHash = semanticHash
		candidate.reasons = extraReasons
		candidates = append(candidates, candidate)
	}

	boundGroups, err := boundNativeMirrorSourceGroupsTx(ctx, tx, snapshot.SupplierID)
	if err != nil {
		return err
	}
	for _, groupID := range boundGroups {
		if _, present := observed[groupID]; present {
			continue
		}
		semanticHash, hashErr := mirror.MissingSourceSemanticHash(groupID)
		candidate := proofCandidate{groupID: groupID, sourceState: mirror.SourceStateMissing,
			semanticHash: semanticHash}
		if hashErr != nil {
			candidate.reasons = append(candidate.reasons, "missing_group_semantic_hash_invalid")
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].groupID < candidates[j].groupID })
	globalReasons = compactSortedStrings(sortedStrings(globalReasons))
	encoded := make([]encodedCatalogGroupProof, 0, len(candidates))
	totalProofBytes := 0
	for _, candidate := range candidates {
		proof, err := mirror.VerifyCollectionProof(mirror.CollectionProofInput{
			AttemptID: attemptID, AttemptSequence: attempt.ID, AttemptStatus: attempt.Status,
			CatalogUpdated: attempt.CatalogUpdated, SourceState: candidate.sourceState, AuthoritativeEndpoint: endpoint,
			AuthenticationRequired: basis.AuthenticationRequired, Authenticated: basis.Authenticated,
			SourceRevision: sourceRevision, ResponseHash: snapshot.Evidence.ResponseHash,
			SourceSemanticHash: candidate.semanticHash, Pages: pages, DeclaredPageCount: basis.DeclaredPageCount,
			DeclaredTotal: basis.DeclaredTotal, RawItemCount: basis.RawItemCount,
			ParsedItemCount: basis.ParsedItemCount, UniqueItemCount: basis.UniqueItemCount,
			DroppedItemCount: basis.DroppedItemCount, UnitErrorCount: basis.UnitErrorCount,
			AmbiguousIdentityCount: basis.AmbiguousIdentityCount, ObservedAt: observedAt,
			MaximumAge: time.Hour,
		}, attempt.FinishedAt.UTC())
		if err != nil {
			return err
		}
		extraReasons := append(append([]string(nil), globalReasons...), candidate.reasons...)
		if len(extraReasons) > 0 {
			proof.Complete = false
			proof.Reasons = append(proof.Reasons, extraReasons...)
			sort.Strings(proof.Reasons)
			proof.Reasons = compactSortedStrings(proof.Reasons)
		}
		if err := mirror.ValidateStoredCollectionProof(proof, attempt.FinishedAt.UTC()); err != nil {
			return fmt.Errorf("validate catalog group collection proof: %w", err)
		}
		payload, err := json.Marshal(proof)
		if err != nil {
			return fmt.Errorf("encode catalog group collection proof: %w", err)
		}
		if len(payload) == 0 || len(payload) > 65_536 {
			return errors.New("collection_proof_payload_capacity_exceeded")
		}
		encoded = append(encoded, encodedCatalogGroupProof{candidate: candidate, proof: proof, payload: payload})
		if len(payload) > 16_777_216-totalProofBytes {
			return errors.New("collection_proof_batch_capacity_exceeded")
		}
		totalProofBytes += len(payload)
	}
	for _, item := range encoded {
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_group_collection_proofs
			(attempt_id,supplier_id,source_group,source_semantic_hash,proof_json,observed_at,
			supplier_incarnation_uuid,authority_protocol)
			SELECT ?,?,?,?,?,?,supplier_incarnation_uuid,authority_protocol FROM collection_attempts WHERE id=?`,
			attempt.ID, snapshot.SupplierID, item.candidate.groupID, item.proof.SourceSemanticHash,
			string(item.payload), formatTime(item.proof.ObservedAt), attempt.ID); err != nil {
			return fmt.Errorf("save catalog group collection proof: %w", err)
		}
	}
	return nil
}

func boundNativeMirrorSourceGroupsTx(ctx context.Context, tx *sql.Tx, supplierID int64) ([]string, error) {
	policies, err := listMirrorPolicies(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list native mirror source bindings: %w", err)
	}
	groups := make(map[string]struct{})
	for _, policy := range policies {
		if policy.SupplierID != supplierID || !policy.Enabled || policy.Kind != mirror.PolicyKindCompleteMirror ||
			policy.SchemaVersion != mirror.PolicySchemaVersion || policy.LegacyPolicyID != 0 {
			continue
		}
		groups[policy.SourceGroup] = struct{}{}
	}
	result := make([]string, 0, len(groups))
	for group := range groups {
		result = append(result, group)
	}
	sort.Strings(result)
	return result, nil
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func compactSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func (s *Store) ListCatalogGroupCollectionProofs(ctx context.Context, supplierID int64, sourceGroup string,
	limit int) ([]mirror.CollectionProof, error) {
	if supplierID <= 0 || strings.TrimSpace(sourceGroup) == "" {
		return nil, errors.New("supplier and source group are required")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	normalizedGroup := strings.TrimSpace(sourceGroup)
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.status,a.catalog_updated,a.finished_at,
		p.attempt_id,p.supplier_id,p.source_group,p.source_semantic_hash,p.proof_json
		FROM collection_attempts a LEFT JOIN catalog_group_collection_proofs p
		ON p.attempt_id=a.id AND p.source_group=?
		WHERE a.supplier_id=? ORDER BY a.id DESC LIMIT ?`, normalizedGroup, supplierID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	proofs := make([]mirror.CollectionProof, 0)
	for rows.Next() {
		var attemptID int64
		var status, finished string
		var catalogUpdated int
		var proofAttemptID, proofSupplierID sql.NullInt64
		var proofSourceGroup, proofSemanticHash, payload sql.NullString
		if err := rows.Scan(&attemptID, &status, &catalogUpdated, &finished, &proofAttemptID, &proofSupplierID,
			&proofSourceGroup, &proofSemanticHash, &payload); err != nil {
			return nil, err
		}
		barrier := mirror.CollectionProof{AttemptID: strconv.FormatInt(attemptID, 10), AttemptSequence: attemptID,
			ObservedAt: parseTime(finished), Complete: false}
		outcomeReasons := collectionAttemptProofBarrierReasons(status, catalogUpdated)
		if len(outcomeReasons) > 0 {
			barrier.Reasons = outcomeReasons
			proofs = append(proofs, barrier)
			continue
		}
		if !payload.Valid {
			barrier.Reasons = []string{"collection_attempt_without_group_proof"}
			proofs = append(proofs, barrier)
			continue
		}
		var proof mirror.CollectionProof
		if err := json.Unmarshal([]byte(payload.String), &proof); err != nil {
			barrier.Reasons = []string{"catalog_group_proof_json_invalid"}
			proofs = append(proofs, barrier)
			continue
		}
		mismatches := make([]string, 0, 6)
		if !proofAttemptID.Valid || proofAttemptID.Int64 != attemptID {
			mismatches = append(mismatches, "proof_row_attempt_identity_mismatch")
		}
		if !proofSupplierID.Valid || proofSupplierID.Int64 != supplierID {
			mismatches = append(mismatches, "proof_row_supplier_identity_mismatch")
		}
		if !proofSourceGroup.Valid || proofSourceGroup.String != normalizedGroup {
			mismatches = append(mismatches, "proof_row_source_group_mismatch")
		}
		if proof.AttemptID != strconv.FormatInt(attemptID, 10) || proof.AttemptSequence != attemptID {
			mismatches = append(mismatches, "proof_json_attempt_identity_mismatch")
		}
		if !proofSemanticHash.Valid || proofSemanticHash.String != proof.SourceSemanticHash {
			mismatches = append(mismatches, "proof_semantic_hash_mismatch")
		}
		if len(mismatches) > 0 {
			barrier.Reasons = compactSortedStrings(sortedStrings(mismatches))
			proofs = append(proofs, barrier)
			continue
		}
		proofs = append(proofs, proof)
	}
	return proofs, rows.Err()
}

// FindCatalogGroupCollectionProof locates the immutable proof for the current
// effective snapshot even when later failed attempts have displaced it from
// the bounded confirmation window.
func (s *Store) FindCatalogGroupCollectionProof(ctx context.Context, supplierID int64, sourceGroup,
	sourceSemanticHash, responseHash string) (mirror.CollectionProof, error) {
	sourceGroup = strings.TrimSpace(sourceGroup)
	sourceSemanticHash = strings.TrimSpace(sourceSemanticHash)
	responseHash = strings.TrimSpace(responseHash)
	if supplierID <= 0 || sourceGroup == "" || sourceSemanticHash == "" || responseHash == "" {
		return mirror.CollectionProof{}, errors.New("supplier, source group, semantic hash and response hash are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.status,a.catalog_updated,a.finished_at,
		p.supplier_id,p.source_group,p.source_semantic_hash,p.proof_json
		FROM catalog_group_collection_proofs p JOIN collection_attempts a
		ON a.id=p.attempt_id AND a.supplier_id=p.supplier_id AND
			COALESCE(a.supplier_incarnation_uuid,'')=COALESCE(p.supplier_incarnation_uuid,'') AND
			a.authority_protocol=p.authority_protocol
		WHERE p.supplier_id=? AND p.source_group=? AND p.source_semantic_hash=?
		ORDER BY a.id DESC`, supplierID, sourceGroup, sourceSemanticHash)
	if err != nil {
		return mirror.CollectionProof{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var attemptID, proofSupplierID int64
		var status, finished, proofSourceGroup, proofSemanticHash, payload string
		var catalogUpdated int
		if err := rows.Scan(&attemptID, &status, &catalogUpdated, &finished, &proofSupplierID,
			&proofSourceGroup, &proofSemanticHash, &payload); err != nil {
			return mirror.CollectionProof{}, err
		}
		var proof mirror.CollectionProof
		if err := json.Unmarshal([]byte(payload), &proof); err != nil {
			return mirror.CollectionProof{}, fmt.Errorf("decode exact catalog group proof: %w", err)
		}
		if strings.TrimSpace(proof.ResponseHash) != responseHash {
			continue
		}
		if proofSupplierID != supplierID || proofSourceGroup != sourceGroup ||
			proofSemanticHash != sourceSemanticHash || proof.SourceSemanticHash != sourceSemanticHash ||
			proof.AttemptID != strconv.FormatInt(attemptID, 10) || proof.AttemptSequence != attemptID ||
			proof.AttemptStatus != status || proof.CatalogUpdated != (catalogUpdated != 0) {
			return mirror.CollectionProof{}, errors.New("exact catalog group proof identity is inconsistent")
		}
		if !proof.Complete {
			return mirror.CollectionProof{}, fmt.Errorf("%w: exact catalog group proof", ErrCatalogGroupProofIncomplete)
		}
		if err := mirror.ValidateStoredCollectionProof(proof, parseTime(finished)); err != nil {
			return mirror.CollectionProof{}, fmt.Errorf("validate exact catalog group proof: %w", err)
		}
		return proof, nil
	}
	if err := rows.Err(); err != nil {
		return mirror.CollectionProof{}, err
	}
	return mirror.CollectionProof{}, sql.ErrNoRows
}

func collectionAttemptProofBarrierReasons(status string, catalogUpdated int) []string {
	reasons := make([]string, 0, 2)
	if status != "success" {
		reasons = append(reasons, "collection_attempt_not_success")
	}
	if catalogUpdated != 1 {
		reasons = append(reasons, "collection_attempt_catalog_not_updated")
	}
	return reasons
}

// CommitCollectionPartial publishes a trusted balance together with its
// diagnostics when the catalog failed. It never creates or replaces a catalog
// snapshot, so the last trusted price directory remains intact.
func (s *Store) CommitCollectionPartial(ctx context.Context, supplierID int64, balance domain.BalanceState,
	attempt *domain.CollectionAttempt, diagnostics domain.CollectionDiagnostics, nextAt time.Time) error {
	if attempt == nil || supplierID <= 0 || attempt.SupplierID != supplierID {
		return errors.New("partial collection requires a matching supplier")
	}
	if attempt.Status != "partial_success" || attempt.CatalogUpdated || !attempt.BalanceUpdated {
		return errors.New("partial collection must update balance without updating catalog")
	}
	if balance.ObservedAt.IsZero() {
		balance.ObservedAt = attempt.FinishedAt
		if balance.ObservedAt.IsZero() {
			balance.ObservedAt = time.Now().UTC()
		}
	}
	release, err := s.acquireWriteGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	for retry := 0; ; retry++ {
		staged := *attempt
		err := s.commitCollectionPartialOnce(ctx, supplierID, balance, &staged, diagnostics, nextAt)
		if err == nil {
			*attempt = staged
			return nil
		}
		if !isSQLiteBusy(err) || retry >= 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(25*(1<<retry)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) commitCollectionPartialOnce(ctx context.Context, supplierID int64, balance domain.BalanceState,
	attempt *domain.CollectionAttempt, diagnostics domain.CollectionDiagnostics, nextAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkCollectionAuthorityTx(ctx, tx); err != nil {
		return err
	}
	if err := s.saveBalanceObservationTx(ctx, tx, supplierID, balance); err != nil {
		return err
	}
	if err := s.recordCollectionAttemptTx(ctx, tx, attempt, diagnostics, nextAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) recordCollectionAttemptTx(ctx context.Context, tx *sql.Tx, attempt *domain.CollectionAttempt,
	diagnostics domain.CollectionDiagnostics, nextAt time.Time) error {
	if attempt == nil || attempt.SupplierID <= 0 {
		return errors.New("collection attempt requires a supplier")
	}
	if attempt.StartedAt.IsZero() {
		return errors.New("collection attempt requires started_at")
	}
	if attempt.FinishedAt.IsZero() {
		attempt.FinishedAt = time.Now().UTC()
	}
	if attempt.FinishedAt.Before(attempt.StartedAt) {
		return errors.New("collection attempt finished before it started")
	}
	if attempt.DurationMS < 0 {
		return errors.New("collection attempt duration cannot be negative")
	}
	if attempt.DurationMS == 0 {
		attempt.DurationMS = attempt.FinishedAt.Sub(attempt.StartedAt).Milliseconds()
	}
	if attempt.Status != "success" && attempt.Status != "partial_success" && attempt.Status != "failed" && attempt.Status != "cancelled" {
		return fmt.Errorf("unsupported collection attempt status %q", attempt.Status)
	}
	attempt.ErrorClass = cleanDiagnosticText(attempt.ErrorClass, 80)
	attempt.ErrorMessage = cleanDiagnosticText(attempt.ErrorMessage, 1200)

	carrier, err := collectionAttemptAuthorityTx(ctx, tx, attempt.SupplierID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO collection_attempts
		(id,supplier_id,status,error_class,error_message,catalog_updated,balance_updated,started_at,finished_at,duration_ms,
		authority_protocol,supplier_incarnation_uuid,lease_generation,lease_schema_digest,commit_ordinal,automation_eligible)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, carrier.AttemptID, attempt.SupplierID, attempt.Status, attempt.ErrorClass,
		attempt.ErrorMessage, boolInt(attempt.CatalogUpdated), boolInt(attempt.BalanceUpdated), formatTime(attempt.StartedAt),
		formatTime(attempt.FinishedAt), attempt.DurationMS, carrier.Protocol, carrier.Incarnation, carrier.LeaseGeneration,
		carrier.CatalogDigest, carrier.CommitOrdinal, boolInt(carrier.AutomationEligible))
	if err != nil {
		return fmt.Errorf("save collection attempt: %w", err)
	}
	attempt.ID = 0
	if reservedID, ok := carrier.AttemptID.(int64); ok {
		attempt.ID = reservedID
	}
	if attempt.ID == 0 {
		attempt.ID, _ = result.LastInsertId()
	}
	attempt.Requests = make([]domain.RequestEvidence, 0, len(diagnostics.Requests))
	for _, evidence := range diagnostics.Requests {
		evidence.AttemptID = attempt.ID
		evidence.SupplierID = attempt.SupplierID
		evidence.SourceURL = sanitizeEvidenceURL(evidence.SourceURL)
		evidence.EndpointType = cleanDiagnosticText(evidence.EndpointType, 80)
		evidence.AuthCapability = cleanDiagnosticText(evidence.AuthCapability, 80)
		evidence.Method = strings.ToUpper(cleanDiagnosticText(evidence.Method, 12))
		evidence.Status = cleanDiagnosticText(evidence.Status, 40)
		evidence.ErrorClass = cleanDiagnosticText(evidence.ErrorClass, 80)
		evidence.ErrorMessage = cleanDiagnosticText(evidence.ErrorMessage, 1200)
		evidence.SchemaVersion = cleanDiagnosticText(evidence.SchemaVersion, 120)
		evidence.ResponseHash = cleanDiagnosticText(evidence.ResponseHash, 128)
		if evidence.ObservedAt.IsZero() {
			evidence.ObservedAt = attempt.FinishedAt
		}
		insert, insertErr := tx.ExecContext(ctx, `INSERT INTO request_evidence
			(attempt_id,supplier_id,endpoint_type,auth_capability,method,source_url,http_status,status,error_class,
			error_message,duration_ms,schema_version,response_hash,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			evidence.AttemptID, evidence.SupplierID, evidence.EndpointType, evidence.AuthCapability, evidence.Method,
			evidence.SourceURL, evidence.HTTPStatus, evidence.Status, evidence.ErrorClass, evidence.ErrorMessage,
			evidence.DurationMS, evidence.SchemaVersion, evidence.ResponseHash, formatTime(evidence.ObservedAt))
		if insertErr != nil {
			return fmt.Errorf("save request evidence: %w", insertErr)
		}
		evidence.ID, _ = insert.LastInsertId()
		attempt.Requests = append(attempt.Requests, evidence)
	}
	for _, capability := range diagnostics.Capabilities {
		if err := upsertAuthCapability(ctx, tx, attempt.SupplierID, capability, attempt.FinishedAt); err != nil {
			return err
		}
	}

	if nextAt.IsZero() {
		nextAt = attempt.FinishedAt
	}
	switch attempt.Status {
	case "success":
		_, err = tx.ExecContext(ctx, `UPDATE suppliers SET last_status='healthy',last_error='',last_error_class='',
			last_attempt_at=?,last_success_at=?,last_success_duration_ms=?,last_collected_at=?,next_collection_at=?,updated_at=? WHERE id=?`,
			formatTime(attempt.FinishedAt), formatTime(attempt.FinishedAt), attempt.DurationMS,
			formatTime(attempt.FinishedAt), formatTime(nextAt), formatTime(time.Now().UTC()), attempt.SupplierID)
	case "partial_success":
		_, err = tx.ExecContext(ctx, `UPDATE suppliers SET last_status='degraded',last_error=?,last_error_class=?,
			last_attempt_at=?,last_success_at=?,last_success_duration_ms=?,last_collected_at=?,next_collection_at=?,updated_at=? WHERE id=?`,
			attempt.ErrorMessage, attempt.ErrorClass, formatTime(attempt.FinishedAt), formatTime(attempt.FinishedAt),
			attempt.DurationMS, formatTime(attempt.FinishedAt), formatTime(nextAt), formatTime(time.Now().UTC()), attempt.SupplierID)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE suppliers SET last_status='degraded',last_error=?,last_error_class=?,
			last_attempt_at=?,last_failure_duration_ms=?,next_collection_at=?,updated_at=? WHERE id=?`,
			attempt.ErrorMessage, attempt.ErrorClass, formatTime(attempt.FinishedAt), attempt.DurationMS,
			formatTime(nextAt), formatTime(time.Now().UTC()), attempt.SupplierID)
	}
	if err != nil {
		return fmt.Errorf("update supplier collection status: %w", err)
	}
	return nil
}

func upsertAuthCapability(ctx context.Context, tx *sql.Tx, supplierID int64, capability domain.AuthCapabilityEvidence, fallback time.Time) error {
	capability.SupplierID = supplierID
	capability.Capability = cleanDiagnosticText(capability.Capability, 80)
	if capability.Capability == "" {
		return nil
	}
	capability.Status = cleanDiagnosticText(capability.Status, 40)
	capability.EndpointType = cleanDiagnosticText(capability.EndpointType, 80)
	if capability.Status == "" {
		capability.Status = "unknown"
	}
	if capability.LastAttemptAt.IsZero() {
		capability.LastAttemptAt = fallback
	}
	var verified any
	if capability.Status == "verified" {
		value := capability.LastAttemptAt
		if capability.VerifiedAt != nil {
			value = *capability.VerifiedAt
		}
		verified = formatTime(value)
	} else {
		verified = nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO auth_capabilities
		(supplier_id,capability,status,credential_id,endpoint_type,http_status,duration_ms,error_class,error_message,last_attempt_at,verified_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(supplier_id,capability,endpoint_type) DO UPDATE SET
		status=excluded.status,credential_id=excluded.credential_id,endpoint_type=excluded.endpoint_type,
		http_status=excluded.http_status,duration_ms=excluded.duration_ms,error_class=excluded.error_class,
		error_message=excluded.error_message,last_attempt_at=excluded.last_attempt_at,
		verified_at=CASE WHEN excluded.status='verified' THEN excluded.verified_at ELSE auth_capabilities.verified_at END`,
		supplierID, capability.Capability, capability.Status, capability.CredentialID,
		capability.EndpointType, capability.HTTPStatus, capability.DurationMS,
		cleanDiagnosticText(capability.ErrorClass, 80), cleanDiagnosticText(capability.ErrorMessage, 1200),
		formatTime(capability.LastAttemptAt), verified)
	if err != nil {
		return fmt.Errorf("save authorization capability %s: %w", capability.Capability, err)
	}
	return nil
}

func (s *Store) LatestCollectionAttempt(ctx context.Context, supplierID int64) (domain.CollectionAttempt, error) {
	return s.scanCollectionAttempt(ctx, `WHERE supplier_id=? ORDER BY id DESC LIMIT 1`, supplierID)
}

func (s *Store) ListCollectionAttempts(ctx context.Context, supplierID int64, limit int) ([]domain.CollectionAttempt, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,supplier_id,status,error_class,error_message,catalog_updated,
		balance_updated,started_at,finished_at,duration_ms FROM collection_attempts WHERE supplier_id=? ORDER BY id DESC LIMIT ?`, supplierID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.CollectionAttempt, 0)
	for rows.Next() {
		item, err := scanCollectionAttemptRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range items {
		var err error
		items[index].Requests, err = s.listRequestEvidence(ctx, items[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

// ConsecutiveCollectionBusyFailures reports only the uninterrupted tail of
// SQLite busy failures. Other failures and successful attempts reset the tail.
func (s *Store) ConsecutiveCollectionBusyFailures(ctx context.Context, supplierID int64, limit int) (int, error) {
	if supplierID <= 0 {
		return 0, errors.New("supplier is required")
	}
	if limit <= 0 || limit > 20 {
		limit = 3
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status,error_class FROM collection_attempts WHERE supplier_id=? ORDER BY id DESC LIMIT ?`,
		supplierID, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var status, errorClass string
		if err := rows.Scan(&status, &errorClass); err != nil {
			return 0, err
		}
		if status != "failed" || errorClass != "database_busy" {
			break
		}
		count++
	}
	return count, rows.Err()
}

func (s *Store) scanCollectionAttempt(ctx context.Context, suffix string, args ...any) (domain.CollectionAttempt, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,supplier_id,status,error_class,error_message,catalog_updated,
		balance_updated,started_at,finished_at,duration_ms FROM collection_attempts `+suffix, args...)
	item, err := scanCollectionAttemptRow(row)
	if err != nil {
		return domain.CollectionAttempt{}, err
	}
	item.Requests, err = s.listRequestEvidence(ctx, item.ID)
	return item, err
}

func scanCollectionAttemptRow(row scanner) (domain.CollectionAttempt, error) {
	var item domain.CollectionAttempt
	var catalogUpdated, balanceUpdated int
	var started, finished string
	err := row.Scan(&item.ID, &item.SupplierID, &item.Status, &item.ErrorClass, &item.ErrorMessage,
		&catalogUpdated, &balanceUpdated, &started, &finished, &item.DurationMS)
	if err != nil {
		return domain.CollectionAttempt{}, err
	}
	item.CatalogUpdated = catalogUpdated != 0
	item.BalanceUpdated = balanceUpdated != 0
	item.StartedAt = parseTime(started)
	item.FinishedAt = parseTime(finished)
	return item, nil
}

func (s *Store) listRequestEvidence(ctx context.Context, attemptID int64) ([]domain.RequestEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,attempt_id,supplier_id,endpoint_type,auth_capability,method,source_url,
		http_status,status,error_class,error_message,duration_ms,schema_version,response_hash,observed_at
		FROM request_evidence WHERE attempt_id=? ORDER BY id`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RequestEvidence, 0)
	for rows.Next() {
		var item domain.RequestEvidence
		var observed string
		if err := rows.Scan(&item.ID, &item.AttemptID, &item.SupplierID, &item.EndpointType, &item.AuthCapability,
			&item.Method, &item.SourceURL, &item.HTTPStatus, &item.Status, &item.ErrorClass, &item.ErrorMessage,
			&item.DurationMS, &item.SchemaVersion, &item.ResponseHash, &observed); err != nil {
			return nil, err
		}
		item.ObservedAt = parseTime(observed)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListAuthCapabilities(ctx context.Context, supplierID int64) ([]domain.AuthCapabilityEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT supplier_id,capability,status,credential_id,endpoint_type,http_status,
		duration_ms,error_class,error_message,last_attempt_at,verified_at FROM auth_capabilities
		WHERE supplier_id=? ORDER BY capability,endpoint_type`, supplierID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AuthCapabilityEvidence, 0)
	for rows.Next() {
		var item domain.AuthCapabilityEvidence
		var credential sql.NullInt64
		var attempted string
		var verified sql.NullString
		if err := rows.Scan(&item.SupplierID, &item.Capability, &item.Status, &credential, &item.EndpointType,
			&item.HTTPStatus, &item.DurationMS, &item.ErrorClass, &item.ErrorMessage, &attempted, &verified); err != nil {
			return nil, err
		}
		if credential.Valid {
			value := credential.Int64
			item.CredentialID = &value
		}
		item.LastAttemptAt = parseTime(attempted)
		if verified.Valid {
			value := parseTime(verified.String)
			item.VerifiedAt = &value
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func sanitizeEvidenceURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func cleanDiagnosticText(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	value = evidenceHeaderSecretRE.ReplaceAllString(value, "$1=[redacted]")
	value = evidenceBearerRE.ReplaceAllString(value, "Bearer [redacted]")
	value = evidenceQuerySecretRE.ReplaceAllString(value, "$1[redacted]")
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}
