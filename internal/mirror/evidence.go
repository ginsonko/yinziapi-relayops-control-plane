package mirror

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type CollectionPageEvidence struct {
	AttemptID      string `json:"attempt_id"`
	PageNumber     int    `json:"page_number"`
	Endpoint       string `json:"endpoint"`
	SourceRevision string `json:"source_revision"`
	ItemCount      int    `json:"item_count"`
	Terminal       bool   `json:"terminal"`
	NextCursor     string `json:"next_cursor,omitempty"`
}

type CollectionProofInput struct {
	AttemptID              string
	AttemptSequence        int64
	AttemptStatus          string
	CatalogUpdated         bool
	SourceState            string
	AuthoritativeEndpoint  string
	AuthenticationRequired bool
	Authenticated          bool
	SourceRevision         string
	ResponseHash           string
	SourceSemanticHash     string
	Pages                  []CollectionPageEvidence
	DeclaredPageCount      *int
	DeclaredTotal          *int
	RawItemCount           int
	ParsedItemCount        int
	UniqueItemCount        int
	DroppedItemCount       int
	UnitErrorCount         int
	AmbiguousIdentityCount int
	ObservedAt             time.Time
	MaximumAge             time.Duration
}

type CollectionProof struct {
	AttemptID          string    `json:"attempt_id"`
	AttemptSequence    int64     `json:"attempt_sequence"`
	AttemptStatus      string    `json:"attempt_status"`
	CatalogUpdated     bool      `json:"catalog_updated"`
	SourceState        string    `json:"source_state"`
	SourceRevision     string    `json:"source_revision"`
	ResponseHash       string    `json:"response_hash"`
	SourceSemanticHash string    `json:"source_semantic_hash"`
	ObservedAt         time.Time `json:"observed_at"`
	Complete           bool      `json:"complete"`
	Reasons            []string  `json:"reasons,omitempty"`
	PageCount          int       `json:"page_count"`
	RawItemCount       int       `json:"raw_item_count"`
	ParsedItemCount    int       `json:"parsed_item_count"`
	UniqueItemCount    int       `json:"unique_item_count"`
}

const (
	SourceStatePresent = "present"
	SourceStateEmpty   = "empty"
	SourceStateMissing = "missing"
)

// VerifyCollectionProof proves that one fresh collection attempt traversed
// the connector's authoritative endpoint to an explicit terminal page without
// dropping, duplicating, mispricing or ambiguously mapping any item.
func VerifyCollectionProof(input CollectionProofInput, now time.Time) (CollectionProof, error) {
	if now.IsZero() {
		return CollectionProof{}, errors.New("proof verification time is required")
	}
	proof := CollectionProof{AttemptID: strings.TrimSpace(input.AttemptID), AttemptSequence: input.AttemptSequence,
		AttemptStatus: input.AttemptStatus, CatalogUpdated: input.CatalogUpdated,
		SourceState:    strings.TrimSpace(input.SourceState),
		SourceRevision: strings.TrimSpace(input.SourceRevision), ResponseHash: strings.TrimSpace(input.ResponseHash),
		SourceSemanticHash: strings.TrimSpace(input.SourceSemanticHash), ObservedAt: input.ObservedAt.UTC(),
		PageCount: len(input.Pages), RawItemCount: input.RawItemCount, ParsedItemCount: input.ParsedItemCount,
		UniqueItemCount: input.UniqueItemCount}
	reasons := make([]string, 0)
	if proof.AttemptID == "" {
		reasons = append(reasons, "attempt_id_missing")
	}
	if proof.AttemptSequence <= 0 {
		reasons = append(reasons, "attempt_sequence_invalid")
	} else if proof.AttemptID != strconv.FormatInt(proof.AttemptSequence, 10) {
		reasons = append(reasons, "attempt_identity_mismatch")
	}
	if proof.AttemptStatus != "success" && proof.AttemptStatus != "partial_success" {
		reasons = append(reasons, "collection_attempt_not_catalog_authoritative")
	}
	if !proof.CatalogUpdated {
		reasons = append(reasons, "collection_attempt_not_catalog_authoritative")
	}
	if !validSourceState(proof.SourceState) {
		reasons = append(reasons, "source_state_invalid")
	}
	endpoint := strings.TrimSpace(input.AuthoritativeEndpoint)
	if endpoint == "" {
		reasons = append(reasons, "authoritative_endpoint_missing")
	}
	if input.AuthenticationRequired && !input.Authenticated {
		reasons = append(reasons, "source_authentication_unproven")
	}
	if proof.SourceRevision == "" {
		reasons = append(reasons, "source_revision_missing")
	}
	if proof.ResponseHash == "" {
		reasons = append(reasons, "response_hash_missing")
	}
	if proof.SourceSemanticHash == "" {
		reasons = append(reasons, "source_semantic_hash_missing")
	}
	if proof.ObservedAt.IsZero() {
		reasons = append(reasons, "observed_at_missing")
	} else {
		if proof.ObservedAt.After(now.Add(time.Minute)) {
			reasons = append(reasons, "observed_at_in_future")
		}
		if input.MaximumAge <= 0 {
			reasons = append(reasons, "maximum_age_missing")
		} else if now.Sub(proof.ObservedAt) > input.MaximumAge {
			reasons = append(reasons, "attempt_stale")
		}
	}
	if len(input.Pages) == 0 {
		reasons = append(reasons, "pages_missing")
	} else {
		reasons = append(reasons, validateCollectionPages(input, endpoint)...)
	}
	if input.RawItemCount < 0 || input.ParsedItemCount < 0 || input.UniqueItemCount < 0 {
		reasons = append(reasons, "negative_item_count")
	} else {
		pageItems := 0
		for _, page := range input.Pages {
			if page.ItemCount < 0 {
				reasons = append(reasons, "negative_page_item_count")
				continue
			}
			pageItems += page.ItemCount
		}
		if pageItems != input.RawItemCount {
			reasons = append(reasons, "page_raw_count_mismatch")
		}
		if input.ParsedItemCount != input.RawItemCount {
			reasons = append(reasons, "parse_count_mismatch")
		}
		if input.UniqueItemCount != input.ParsedItemCount {
			reasons = append(reasons, "unique_count_mismatch")
		}
		if input.DeclaredTotal != nil && (*input.DeclaredTotal < 0 || *input.DeclaredTotal != input.UniqueItemCount) {
			reasons = append(reasons, "declared_total_mismatch")
		}
	}
	if input.DroppedItemCount != 0 {
		reasons = append(reasons, "items_dropped")
	}
	if input.UnitErrorCount != 0 {
		reasons = append(reasons, "unit_normalization_failed")
	}
	if input.AmbiguousIdentityCount != 0 {
		reasons = append(reasons, "identity_mapping_ambiguous")
	}
	proof.Reasons = sortedUniqueStrings(reasons)
	proof.Complete = len(proof.Reasons) == 0
	return proof, nil
}

// ValidateStoredCollectionProof is the shared writer/reader semantic contract.
// It validates typed state only and never repairs or rewrites forensic input.
func ValidateStoredCollectionProof(proof CollectionProof, attemptFinished time.Time) error {
	if proof.AttemptSequence <= 0 || proof.AttemptID != strconv.FormatInt(proof.AttemptSequence, 10) {
		return errors.New("collection proof attempt identity is invalid")
	}
	if proof.AttemptStatus != "success" && proof.AttemptStatus != "partial_success" {
		return errors.New("collection proof attempt status is invalid")
	}
	if !proof.CatalogUpdated {
		return errors.New("collection proof catalog outcome is invalid")
	}
	if !validSourceState(proof.SourceState) {
		return errors.New("collection proof source state is invalid")
	}
	if proof.ObservedAt.IsZero() || proof.ObservedAt.Location() != time.UTC || attemptFinished.IsZero() {
		return errors.New("collection proof time is invalid")
	}
	finished := attemptFinished.UTC()
	if proof.ObservedAt.After(finished.Add(time.Minute)) || finished.Sub(proof.ObservedAt) > time.Hour {
		return errors.New("collection proof observation is outside the writer window")
	}
	for name, item := range map[string]struct {
		value   string
		maximum int
	}{
		"source_revision": {proof.SourceRevision, 512},
		"response_hash": {proof.ResponseHash, 128},
		"semantic_hash": {proof.SourceSemanticHash, 128},
	} {
		if len(item.value) > item.maximum || !utf8.ValidString(item.value) || strings.ContainsRune(item.value, '\x00') || item.value != strings.TrimSpace(item.value) {
			return fmt.Errorf("collection proof %s is not canonical", name)
		}
		if proof.Complete && item.value == "" {
			return fmt.Errorf("complete collection proof %s is missing", name)
		}
	}
	if proof.PageCount < 0 || proof.PageCount > 10_000 || proof.RawItemCount < 0 || proof.RawItemCount > 10_000_000 ||
		proof.ParsedItemCount < 0 || proof.ParsedItemCount > 10_000_000 || proof.UniqueItemCount < 0 || proof.UniqueItemCount > 10_000_000 {
		return errors.New("collection proof counts are outside the allowed range")
	}
	if proof.Complete {
		if proof.PageCount == 0 || proof.RawItemCount != proof.ParsedItemCount || proof.ParsedItemCount != proof.UniqueItemCount {
			return errors.New("complete collection proof counts are inconsistent")
		}
	}
	if proof.Complete != (len(proof.Reasons) == 0) {
		return errors.New("collection proof completeness and reasons disagree")
	}
	if len(proof.Reasons) > 64 {
		return errors.New("collection proof has too many reasons")
	}
	previous := ""
	for _, reason := range proof.Reasons {
		if !validCollectionProofReason(reason) || (previous != "" && reason <= previous) {
			return errors.New("collection proof reasons are not canonical")
		}
		previous = reason
	}
	return nil
}

func validCollectionProofReason(reason string) bool {
	if len(reason) < 1 || len(reason) > 96 {
		return false
	}
	for index := 0; index < len(reason); index++ {
		character := reason[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validateCollectionPages(input CollectionProofInput, endpoint string) []string {
	reasons := make([]string, 0)
	pages := append([]CollectionPageEvidence(nil), input.Pages...)
	sort.Slice(pages, func(i, j int) bool { return pages[i].PageNumber < pages[j].PageNumber })
	if input.DeclaredPageCount != nil && (*input.DeclaredPageCount != len(pages) || *input.DeclaredPageCount < 1) {
		reasons = append(reasons, "declared_page_count_mismatch")
	}
	seenPage := make(map[int]struct{}, len(pages))
	seenCursor := make(map[string]struct{}, len(pages))
	terminalCount := 0
	for index, page := range pages {
		if page.PageNumber != index+1 {
			reasons = append(reasons, "pagination_not_contiguous")
		}
		if _, exists := seenPage[page.PageNumber]; exists {
			reasons = append(reasons, "pagination_duplicate_page")
		}
		seenPage[page.PageNumber] = struct{}{}
		if strings.TrimSpace(page.AttemptID) != strings.TrimSpace(input.AttemptID) {
			reasons = append(reasons, "page_attempt_mismatch")
		}
		if strings.TrimSpace(page.SourceRevision) != strings.TrimSpace(input.SourceRevision) {
			reasons = append(reasons, "page_source_revision_mismatch")
		}
		if strings.TrimSpace(page.Endpoint) != endpoint {
			reasons = append(reasons, "non_authoritative_endpoint")
		}
		if page.Terminal {
			terminalCount++
			if strings.TrimSpace(page.NextCursor) != "" || index != len(pages)-1 {
				reasons = append(reasons, "terminal_page_invalid")
			}
		} else {
			cursor := strings.TrimSpace(page.NextCursor)
			if cursor == "" {
				reasons = append(reasons, "pagination_cursor_missing")
			} else if _, duplicate := seenCursor[cursor]; duplicate {
				reasons = append(reasons, "pagination_cursor_repeated")
			} else {
				seenCursor[cursor] = struct{}{}
			}
		}
	}
	if terminalCount != 1 {
		reasons = append(reasons, "terminal_page_unproven")
	}
	return reasons
}

type ChangeKind string

const (
	ChangePrice              ChangeKind = "price"
	ChangeBilling            ChangeKind = "billing"
	ChangeAddition           ChangeKind = "addition"
	ChangeRemoval            ChangeKind = "removal"
	ChangeGroupDisappearance ChangeKind = "group_disappearance"
)

type ConfirmationResult struct {
	Required           int      `json:"required"`
	Confirmed          int      `json:"confirmed"`
	Eligible           bool     `json:"eligible"`
	DistinctAttemptIDs []string `json:"distinct_attempt_ids"`
	RejectedAttemptIDs []string `json:"rejected_attempt_ids,omitempty"`
}

// ConfirmEquivalentChange counts only distinct, complete, fresh collection
// attempts with the same functional source hash. A duplicated stored snapshot
// or attempt ID cannot increase the confirmation count.
func ConfirmEquivalentChange(kind ChangeKind, sourceState, semanticHash string, proofs []CollectionProof, now time.Time,
	maximumAge time.Duration) (ConfirmationResult, error) {
	required, err := requiredConfirmations(kind)
	if err != nil {
		return ConfirmationResult{}, err
	}
	semanticHash = strings.TrimSpace(semanticHash)
	sourceState = strings.TrimSpace(sourceState)
	if semanticHash == "" || now.IsZero() || maximumAge <= 0 || !sourceStateAllowedForChange(kind, sourceState) {
		return ConfirmationResult{}, errors.New("source state, semantic hash, verification time and maximum age are required")
	}
	destructive := kind == ChangeRemoval || kind == ChangeGroupDisappearance
	result := ConfirmationResult{Required: required}
	if destructive {
		for _, proof := range proofs {
			attemptID := strings.TrimSpace(proof.AttemptID)
			if attemptID == "" || proof.AttemptSequence <= 0 || attemptID != strconv.FormatInt(proof.AttemptSequence, 10) {
				if attemptID == "" {
					attemptID = "<missing>"
				}
				result.RejectedAttemptIDs = append(result.RejectedAttemptIDs, attemptID)
				sort.Strings(result.RejectedAttemptIDs)
				return result, nil
			}
		}
	}
	ordered := append([]CollectionProof(nil), proofs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AttemptSequence != ordered[j].AttemptSequence {
			return ordered[i].AttemptSequence > ordered[j].AttemptSequence
		}
		return strings.TrimSpace(ordered[i].AttemptID) > strings.TrimSpace(ordered[j].AttemptID)
	})
	seenIDs := make(map[string]struct{})
	seenSequences := make(map[int64]struct{})
	for _, proof := range ordered {
		attemptID := strings.TrimSpace(proof.AttemptID)
		if attemptID == "" {
			result.RejectedAttemptIDs = append(result.RejectedAttemptIDs, "<missing>")
			continue
		}
		_, duplicateID := seenIDs[attemptID]
		_, duplicateSequence := seenSequences[proof.AttemptSequence]
		if duplicateID || duplicateSequence {
			result.RejectedAttemptIDs = append(result.RejectedAttemptIDs, attemptID)
			if destructive {
				break
			}
			continue
		}
		seenIDs[attemptID] = struct{}{}
		seenSequences[proof.AttemptSequence] = struct{}{}
		matching := proof.AttemptSequence > 0 && attemptID == strconv.FormatInt(proof.AttemptSequence, 10) &&
			proof.Complete && strings.TrimSpace(proof.SourceState) == sourceState &&
			strings.TrimSpace(proof.SourceSemanticHash) == semanticHash && !proof.ObservedAt.IsZero() &&
			!proof.ObservedAt.After(now.Add(time.Minute)) && now.Sub(proof.ObservedAt) <= maximumAge
		if !matching {
			result.RejectedAttemptIDs = append(result.RejectedAttemptIDs, attemptID)
			if destructive {
				break
			}
			continue
		}
		result.DistinctAttemptIDs = append(result.DistinctAttemptIDs, attemptID)
		if destructive && len(result.DistinctAttemptIDs) >= required {
			break
		}
	}
	sort.Strings(result.DistinctAttemptIDs)
	sort.Strings(result.RejectedAttemptIDs)
	result.Confirmed = len(result.DistinctAttemptIDs)
	result.Eligible = result.Confirmed >= result.Required
	return result, nil
}

func validSourceState(value string) bool {
	return value == SourceStatePresent || value == SourceStateEmpty || value == SourceStateMissing
}

func sourceStateAllowedForChange(kind ChangeKind, state string) bool {
	switch kind {
	case ChangePrice, ChangeBilling, ChangeAddition:
		return state == SourceStatePresent
	case ChangeRemoval:
		return state == SourceStatePresent || state == SourceStateEmpty
	case ChangeGroupDisappearance:
		return state == SourceStateMissing
	default:
		return false
	}
}

func requiredConfirmations(kind ChangeKind) (int, error) {
	switch kind {
	case ChangePrice, ChangeBilling, ChangeAddition:
		return 2, nil
	case ChangeRemoval, ChangeGroupDisappearance:
		return 3, nil
	default:
		return 0, fmt.Errorf("unsupported change kind %q", kind)
	}
}
