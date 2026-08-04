package mirrorservice

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

func (s *Service) confirmPlan(plan mirror.CompleteMirrorPlan, state mirror.SourceGroupState,
	semanticHash string, proofs []mirror.CollectionProof) ([]Confirmation, bool, error) {
	kinds := confirmationKinds(plan, state)
	if len(kinds) == 0 {
		return []Confirmation{}, true, nil
	}
	now := s.now()
	consecutive := consecutiveEquivalentProofs(proofs, string(state), semanticHash, now,
		s.options.EvidenceMaximumAge)
	result := make([]Confirmation, 0, len(kinds))
	ready := true
	for _, kind := range kinds {
		confirmation, err := mirror.ConfirmEquivalentChange(kind, string(state), semanticHash,
			consecutive, now, s.options.EvidenceMaximumAge)
		if err != nil {
			return nil, false, fmt.Errorf("confirm mirror %s: %w", kind, err)
		}
		result = append(result, Confirmation{Kind: kind, Result: confirmation})
		ready = ready && confirmation.Eligible
	}
	return result, ready, nil
}

func confirmationKinds(plan mirror.CompleteMirrorPlan, state mirror.SourceGroupState) []mirror.ChangeKind {
	set := make(map[mirror.ChangeKind]struct{})
	for _, diff := range plan.Diffs {
		switch diff.Kind {
		case mirror.DiffGroupRatio, mirror.DiffModelPrice:
			set[mirror.ChangePrice] = struct{}{}
		case mirror.DiffBilling:
			set[mirror.ChangeBilling] = struct{}{}
		case mirror.DiffAddition:
			set[mirror.ChangeAddition] = struct{}{}
		case mirror.DiffRemoval:
			if state == mirror.SourceGroupMissing {
				set[mirror.ChangeGroupDisappearance] = struct{}{}
			} else {
				set[mirror.ChangeRemoval] = struct{}{}
			}
		case mirror.DiffChannelState:
			if diff.After == "false" {
				if state == mirror.SourceGroupMissing {
					set[mirror.ChangeGroupDisappearance] = struct{}{}
				} else {
					set[mirror.ChangeRemoval] = struct{}{}
				}
			} else {
				set[mirror.ChangeAddition] = struct{}{}
			}
		}
	}
	ordered := []mirror.ChangeKind{mirror.ChangePrice, mirror.ChangeBilling, mirror.ChangeAddition,
		mirror.ChangeRemoval, mirror.ChangeGroupDisappearance}
	result := make([]mirror.ChangeKind, 0, len(set))
	for _, kind := range ordered {
		if _, ok := set[kind]; ok {
			result = append(result, kind)
		}
	}
	return result
}

// consecutiveEquivalentProofs enforces the scheduler contract more strictly
// than a historical-count query: the newest failed, partial, stale, duplicate
// or semantically different attempt ends the sequence for every change kind.
func consecutiveEquivalentProofs(proofs []mirror.CollectionProof, state, semanticHash string, now time.Time,
	maximumAge time.Duration) []mirror.CollectionProof {
	ordered := append([]mirror.CollectionProof(nil), proofs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AttemptSequence != ordered[j].AttemptSequence {
			return ordered[i].AttemptSequence > ordered[j].AttemptSequence
		}
		return ordered[i].AttemptID > ordered[j].AttemptID
	})
	seenIDs := make(map[string]struct{}, len(ordered))
	seenSequences := make(map[int64]struct{}, len(ordered))
	result := make([]mirror.CollectionProof, 0, len(ordered))
	for _, proof := range ordered {
		attemptID := strings.TrimSpace(proof.AttemptID)
		_, duplicateID := seenIDs[attemptID]
		_, duplicateSequence := seenSequences[proof.AttemptSequence]
		matching := attemptID != "" && proof.AttemptSequence > 0 &&
			attemptID == strconv.FormatInt(proof.AttemptSequence, 10) && !duplicateID && !duplicateSequence &&
			proof.Complete && strings.TrimSpace(proof.SourceState) == state &&
			strings.TrimSpace(proof.SourceSemanticHash) == semanticHash && !proof.ObservedAt.IsZero() &&
			!proof.ObservedAt.After(now.Add(time.Minute)) && now.Sub(proof.ObservedAt) <= maximumAge
		if !matching {
			break
		}
		seenIDs[attemptID] = struct{}{}
		seenSequences[proof.AttemptSequence] = struct{}{}
		result = append(result, proof)
	}
	return result
}
