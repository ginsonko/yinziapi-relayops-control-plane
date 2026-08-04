package mirror

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompleteCollectionProofAndConfirmationThresholds(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	first, err := VerifyCollectionProof(validCollectionProofInput(1, now.Add(-3*time.Minute)), now)
	if err != nil || !first.Complete {
		t.Fatalf("first proof = %#v, %v", first, err)
	}
	second, _ := VerifyCollectionProof(validCollectionProofInput(2, now.Add(-2*time.Minute)), now)
	third, _ := VerifyCollectionProof(validCollectionProofInput(3, now.Add(-time.Minute)), now)

	price, err := ConfirmEquivalentChange(ChangePrice, SourceStatePresent, "semantic-a", []CollectionProof{first, first, second}, now, time.Hour)
	if err != nil || !price.Eligible || price.Confirmed != 2 || len(price.RejectedAttemptIDs) != 1 {
		t.Fatalf("price confirmation = %#v, %v", price, err)
	}
	removal, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", []CollectionProof{first, second}, now, time.Hour)
	if err != nil || removal.Eligible || removal.Required != 3 || removal.Confirmed != 2 {
		t.Fatalf("removal before third proof = %#v, %v", removal, err)
	}
	removal, err = ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", []CollectionProof{first, second, third}, now, time.Hour)
	if err != nil || !removal.Eligible || removal.Confirmed != 3 {
		t.Fatalf("removal confirmation = %#v, %v", removal, err)
	}
}

func TestIncompleteCollectionCannotBecomeDestructiveEvidence(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*CollectionProofInput)
		reason string
	}{
		{name: "wrong endpoint", mutate: func(input *CollectionProofInput) { input.Pages[0].Endpoint = "https://other.example/catalog" }, reason: "non_authoritative_endpoint"},
		{name: "pagination unfinished", mutate: func(input *CollectionProofInput) { input.Pages[1].Terminal = false; input.Pages[1].NextCursor = "" }, reason: "terminal_page_unproven"},
		{name: "pagination cursor loop", mutate: func(input *CollectionProofInput) {
			pageCount := 3
			input.DeclaredPageCount = &pageCount
			input.Pages[1].Terminal = false
			input.Pages[1].NextCursor = "page-2"
			input.Pages = append(input.Pages, CollectionPageEvidence{AttemptID: input.AttemptID, PageNumber: 3,
				Endpoint: input.AuthoritativeEndpoint, SourceRevision: input.SourceRevision, Terminal: true})
		}, reason: "pagination_cursor_repeated"},
		{name: "parse loss", mutate: func(input *CollectionProofInput) { input.ParsedItemCount = 2 }, reason: "parse_count_mismatch"},
		{name: "duplicate identity", mutate: func(input *CollectionProofInput) { input.UniqueItemCount = 2 }, reason: "unique_count_mismatch"},
		{name: "dropped unit", mutate: func(input *CollectionProofInput) { input.UnitErrorCount = 1 }, reason: "unit_normalization_failed"},
		{name: "ambiguous mapping", mutate: func(input *CollectionProofInput) { input.AmbiguousIdentityCount = 1 }, reason: "identity_mapping_ambiguous"},
		{name: "authentication failed", mutate: func(input *CollectionProofInput) { input.AuthenticationRequired = true; input.Authenticated = false }, reason: "source_authentication_unproven"},
		{name: "stale", mutate: func(input *CollectionProofInput) { input.ObservedAt = now.Add(-2 * time.Hour) }, reason: "attempt_stale"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := validCollectionProofInput(99, now.Add(-time.Minute))
			input.Pages = append([]CollectionPageEvidence(nil), input.Pages...)
			testCase.mutate(&input)
			proof, err := VerifyCollectionProof(input, now)
			if err != nil {
				t.Fatal(err)
			}
			if proof.Complete || !strings.Contains(strings.Join(proof.Reasons, ","), testCase.reason) {
				t.Fatalf("incomplete proof = %#v", proof)
			}
			confirmation, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", []CollectionProof{proof, proof, proof}, now, time.Hour)
			if err != nil || confirmation.Eligible || confirmation.Confirmed != 0 {
				t.Fatalf("incomplete destructive confirmation = %#v, %v", confirmation, err)
			}
		})
	}
}

func TestCollectionProofAllowsOptionalDeclaredCounts(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name                  string
		omitDeclaredPageCount bool
		omitDeclaredTotal     bool
	}{
		{name: "both declarations omitted", omitDeclaredPageCount: true, omitDeclaredTotal: true},
		{name: "page count omitted", omitDeclaredPageCount: true},
		{name: "item total omitted", omitDeclaredTotal: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := validCollectionProofInput(100, now.Add(-time.Minute))
			if testCase.omitDeclaredPageCount {
				input.DeclaredPageCount = nil
			}
			if testCase.omitDeclaredTotal {
				input.DeclaredTotal = nil
			}
			proof, err := VerifyCollectionProof(input, now)
			if err != nil || !proof.Complete || len(proof.Reasons) != 0 {
				t.Fatalf("proof with optional declarations omitted = %#v, %v", proof, err)
			}
		})
	}
}

func TestOmittedDeclaredCountsDoNotBypassIntrinsicCompleteness(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*CollectionProofInput)
		reason string
	}{
		{name: "terminal still required", mutate: func(input *CollectionProofInput) {
			input.Pages[len(input.Pages)-1].Terminal = false
		}, reason: "terminal_page_unproven"},
		{name: "contiguous pages still required", mutate: func(input *CollectionProofInput) {
			input.Pages[len(input.Pages)-1].PageNumber = 3
		}, reason: "pagination_not_contiguous"},
		{name: "unique pages still required", mutate: func(input *CollectionProofInput) {
			input.Pages[len(input.Pages)-1].PageNumber = 1
		}, reason: "pagination_duplicate_page"},
		{name: "raw counts still required", mutate: func(input *CollectionProofInput) {
			input.RawItemCount++
		}, reason: "page_raw_count_mismatch"},
		{name: "parsed counts still required", mutate: func(input *CollectionProofInput) {
			input.ParsedItemCount--
		}, reason: "parse_count_mismatch"},
		{name: "unique counts still required", mutate: func(input *CollectionProofInput) {
			input.UniqueItemCount--
		}, reason: "unique_count_mismatch"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := validCollectionProofInput(101, now.Add(-time.Minute))
			input.DeclaredPageCount = nil
			input.DeclaredTotal = nil
			testCase.mutate(&input)
			proof, err := VerifyCollectionProof(input, now)
			if err != nil {
				t.Fatal(err)
			}
			if proof.Complete || !strings.Contains(strings.Join(proof.Reasons, ","), testCase.reason) {
				t.Fatalf("intrinsically incomplete proof = %#v", proof)
			}
		})
	}
}

func TestCollectionProofStrictlyChecksPresentDeclaredCounts(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*CollectionProofInput)
		reason string
	}{
		{name: "page count mismatch", mutate: func(input *CollectionProofInput) {
			declared := len(input.Pages) + 1
			input.DeclaredPageCount = &declared
		}, reason: "declared_page_count_mismatch"},
		{name: "negative page count", mutate: func(input *CollectionProofInput) {
			declared := -1
			input.DeclaredPageCount = &declared
		}, reason: "declared_page_count_mismatch"},
		{name: "item total mismatch", mutate: func(input *CollectionProofInput) {
			declared := input.UniqueItemCount + 1
			input.DeclaredTotal = &declared
		}, reason: "declared_total_mismatch"},
		{name: "negative item total", mutate: func(input *CollectionProofInput) {
			declared := -1
			input.DeclaredTotal = &declared
		}, reason: "declared_total_mismatch"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := validCollectionProofInput(102, now.Add(-time.Minute))
			testCase.mutate(&input)
			proof, err := VerifyCollectionProof(input, now)
			if err != nil {
				t.Fatal(err)
			}
			if proof.Complete || !strings.Contains(strings.Join(proof.Reasons, ","), testCase.reason) {
				t.Fatalf("mismatched declaration proof = %#v", proof)
			}
		})
	}
}

func TestCollectionProofAllowsAuthoritativeEmptyTerminalWithoutDeclaredCounts(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	input := validCollectionProofInput(103, now.Add(-time.Minute))
	input.Pages = []CollectionPageEvidence{{
		AttemptID: input.AttemptID, PageNumber: 1, Endpoint: input.AuthoritativeEndpoint,
		SourceRevision: input.SourceRevision, Terminal: true,
	}}
	input.DeclaredPageCount = nil
	input.DeclaredTotal = nil
	input.RawItemCount = 0
	input.ParsedItemCount = 0
	input.UniqueItemCount = 0

	proof, err := VerifyCollectionProof(input, now)
	if err != nil || !proof.Complete || len(proof.Reasons) != 0 {
		t.Fatalf("empty authoritative terminal proof = %#v, %v", proof, err)
	}
}

func TestDifferentSemanticHashDoesNotConfirmChange(t *testing.T) {
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	first, _ := VerifyCollectionProof(validCollectionProofInput(1, now.Add(-2*time.Minute)), now)
	secondInput := validCollectionProofInput(2, now.Add(-time.Minute))
	secondInput.SourceSemanticHash = "semantic-b"
	second, _ := VerifyCollectionProof(secondInput, now)
	result, err := ConfirmEquivalentChange(ChangePrice, SourceStatePresent, "semantic-a", []CollectionProof{first, second}, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 1 {
		t.Fatalf("mixed semantic confirmation = %#v, %v", result, err)
	}
}

func TestGroupDisappearanceRequiresThreeConsecutiveCompleteMissingProofs(t *testing.T) {
	now := time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC)
	missingProof := func(attempt int64, observedAt time.Time) CollectionProof {
		input := validCollectionProofInput(attempt, observedAt)
		input.SourceState = SourceStateMissing
		input.SourceSemanticHash = "missing-video"
		proof, err := VerifyCollectionProof(input, now)
		if err != nil || !proof.Complete {
			t.Fatalf("missing proof %d = %#v, %v", attempt, proof, err)
		}
		return proof
	}
	first := missingProof(1, now.Add(-4*time.Minute))
	second := missingProof(2, now.Add(-3*time.Minute))
	third := missingProof(4, now.Add(-time.Minute))

	result, err := ConfirmEquivalentChange(ChangeGroupDisappearance, SourceStateMissing, "missing-video",
		[]CollectionProof{first, second}, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 2 || result.Required != 3 {
		t.Fatalf("two missing proofs = %#v, %v", result, err)
	}
	result, err = ConfirmEquivalentChange(ChangeGroupDisappearance, SourceStateMissing, "missing-video",
		[]CollectionProof{third, first, second}, now, time.Hour)
	if err != nil || !result.Eligible || result.Confirmed != 3 {
		t.Fatalf("three missing proofs = %#v, %v", result, err)
	}

	barrier := CollectionProof{AttemptID: "3", AttemptSequence: 3, ObservedAt: now.Add(-2 * time.Minute), Complete: false,
		Reasons: []string{"collection_attempt_not_success"}}
	result, err = ConfirmEquivalentChange(ChangeGroupDisappearance, SourceStateMissing, "missing-video",
		[]CollectionProof{first, second, barrier, third}, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 1 || len(result.RejectedAttemptIDs) != 1 {
		t.Fatalf("failed attempt did not break the missing streak = %#v, %v", result, err)
	}
}

func TestLegacyStateLessProofCannotConfirmDestructiveChange(t *testing.T) {
	now := time.Date(2026, 7, 28, 7, 30, 0, 0, time.UTC)
	proofs := make([]CollectionProof, 0, 3)
	for index, age := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute} {
		input := validCollectionProofInput(int64(3-index), now.Add(-age))
		proof, err := VerifyCollectionProof(input, now)
		if err != nil || !proof.Complete {
			t.Fatalf("present proof = %#v, %v", proof, err)
		}
		proofs = append(proofs, proof)
	}
	proofs[0].SourceState = ""
	result, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", proofs, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 0 {
		t.Fatalf("legacy state-less proof did not fail closed = %#v, %v", result, err)
	}
}

func TestDestructiveEvidenceUsesNumericAttemptOrderWhenTimesTie(t *testing.T) {
	now := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)
	observedAt := now.Add(-time.Minute)
	proofs := make([]CollectionProof, 0, 4)
	for _, attemptID := range []int64{7, 8, 9} {
		input := validCollectionProofInput(attemptID, observedAt)
		input.SourceState = SourceStateMissing
		input.SourceSemanticHash = "missing-video"
		proof, err := VerifyCollectionProof(input, now)
		if err != nil || !proof.Complete {
			t.Fatalf("missing proof = %#v, %v", proof, err)
		}
		proofs = append(proofs, proof)
	}
	proofs = append(proofs, CollectionProof{AttemptID: "10", AttemptSequence: 10, ObservedAt: observedAt, Complete: false,
		Reasons: []string{"collection_attempt_not_success"}})
	result, err := ConfirmEquivalentChange(ChangeGroupDisappearance, SourceStateMissing, "missing-video", proofs, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 0 || len(result.RejectedAttemptIDs) != 1 ||
		result.RejectedAttemptIDs[0] != "10" {
		t.Fatalf("newest numeric barrier was skipped = %#v, %v", result, err)
	}
}

func TestDuplicateAttemptBreaksDestructiveEvidenceSequence(t *testing.T) {
	now := time.Date(2026, 7, 28, 8, 30, 0, 0, time.UTC)
	proofs := make([]CollectionProof, 0, 5)
	for index, attemptID := range []int64{4, 3, 3, 2, 1} {
		input := validCollectionProofInput(attemptID, now.Add(-time.Duration(index)*time.Minute))
		proof, err := VerifyCollectionProof(input, now)
		if err != nil || !proof.Complete {
			t.Fatalf("present proof = %#v, %v", proof, err)
		}
		proofs = append(proofs, proof)
	}
	result, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", proofs, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 2 || len(result.RejectedAttemptIDs) != 1 ||
		result.RejectedAttemptIDs[0] != "3" {
		t.Fatalf("duplicate attempt did not break destructive sequence = %#v, %v", result, err)
	}
}

func TestDestructiveEvidenceUsesAttemptSequenceInsteadOfSourceObservationTime(t *testing.T) {
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	newestBarrier := validCollectionProofInput(4, now.Add(-4*time.Minute))
	newestBarrier.SourceSemanticHash = "semantic-new"
	barrier, err := VerifyCollectionProof(newestBarrier, now)
	if err != nil || !barrier.Complete {
		t.Fatalf("barrier proof = %#v, %v", barrier, err)
	}
	proofs := []CollectionProof{barrier}
	for sequence, age := range map[int64]time.Duration{3: time.Minute, 2: 2 * time.Minute, 1: 3 * time.Minute} {
		proof, err := VerifyCollectionProof(validCollectionProofInput(sequence, now.Add(-age)), now)
		if err != nil || !proof.Complete {
			t.Fatalf("matching proof = %#v, %v", proof, err)
		}
		proofs = append(proofs, proof)
	}
	result, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", proofs, now, time.Hour)
	if err != nil || result.Eligible || result.Confirmed != 0 || len(result.RejectedAttemptIDs) != 1 ||
		result.RejectedAttemptIDs[0] != "4" {
		t.Fatalf("newest semantic barrier was reordered by source time = %#v, %v", result, err)
	}
}

func TestInvalidAttemptIdentityFailsDestructivePreflightBeforeSorting(t *testing.T) {
	now := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
	matching := make([]CollectionProof, 0, 3)
	for _, sequence := range []int64{3, 2, 1} {
		proof, err := VerifyCollectionProof(validCollectionProofInput(sequence, now.Add(-time.Duration(4-sequence)*time.Minute)), now)
		if err != nil || !proof.Complete {
			t.Fatal(err)
		}
		matching = append(matching, proof)
	}
	for _, invalid := range []CollectionProof{
		{AttemptID: "", AttemptSequence: 4, ObservedAt: now.Add(-4 * time.Minute), Complete: false},
		{AttemptID: "4", AttemptSequence: 0, ObservedAt: now.Add(-4 * time.Minute), Complete: false},
		{AttemptID: "04", AttemptSequence: 4, ObservedAt: now.Add(-4 * time.Minute), Complete: false},
	} {
		proofs := append([]CollectionProof{invalid}, matching...)
		result, err := ConfirmEquivalentChange(ChangeRemoval, SourceStatePresent, "semantic-a", proofs, now, time.Hour)
		if err != nil || result.Eligible || result.Confirmed != 0 || len(result.RejectedAttemptIDs) != 1 {
			t.Fatalf("invalid identity was sorted behind matching proofs: invalid=%#v result=%#v err=%v", invalid, result, err)
		}
	}
}

func validCollectionProofInput(attemptSequence int64, observedAt time.Time) CollectionProofInput {
	pages, total := 2, 3
	endpoint := "https://upstream.example/api/pricing"
	attemptID := strconv.FormatInt(attemptSequence, 10)
	return CollectionProofInput{
		AttemptID: attemptID, AttemptSequence: attemptSequence, AttemptStatus: "success", CatalogUpdated: true,
		SourceState:           SourceStatePresent,
		AuthoritativeEndpoint: endpoint, Authenticated: true,
		SourceRevision: "catalog-revision-7", ResponseHash: "raw-response-hash", SourceSemanticHash: "semantic-a",
		Pages: []CollectionPageEvidence{
			{AttemptID: attemptID, PageNumber: 1, Endpoint: endpoint, SourceRevision: "catalog-revision-7", ItemCount: 2, NextCursor: "page-2"},
			{AttemptID: attemptID, PageNumber: 2, Endpoint: endpoint, SourceRevision: "catalog-revision-7", ItemCount: 1, Terminal: true},
		},
		DeclaredPageCount: &pages, DeclaredTotal: &total, RawItemCount: 3, ParsedItemCount: 3, UniqueItemCount: 3,
		ObservedAt: observedAt, MaximumAge: time.Hour,
	}
}
