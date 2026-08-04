package mirrorservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

// ReconcileSupplier plans every enabled native policy independently. One
// broken binding cannot suppress another native policy or the later legacy
// reconciliation stage composed by main.
func (s *Service) ReconcileSupplier(ctx context.Context, supplierID int64) error {
	policies, err := s.store.ListMirrorPolicies(ctx)
	if err != nil {
		return err
	}
	failures := make([]error, 0)
	for _, policy := range policies {
		policy = mirror.NormalizeOwnership(policy)
		if !policy.Enabled || policy.SupplierID != supplierID || policy.Kind != mirror.PolicyKindCompleteMirror {
			continue
		}
		preview, previewErr := s.PreviewPolicy(ctx, PreviewRequest{PolicyUUID: policy.PolicyUUID})
		if previewErr != nil {
			if errors.Is(previewErr, ErrSourceEvidencePending) {
				failures = append(failures, previewErr)
				continue
			}
			failures = append(failures, fmt.Errorf("mirror policy %s preview: %w", policy.PolicyUUID, previewErr))
			continue
		}
		if !preview.Plan.NoChange && !preview.EvidenceReady && policy.ExecutionMode == "auto" {
			failures = append(failures, sourceEvidencePending(fmt.Sprintf(
				"%s -> %s is waiting for the next scheduled complete collection", policy.SourceGroup, policy.TargetGroup)))
			continue
		}
		if preview.Plan.NoChange || !preview.EvidenceReady || preview.Plan.Decision != mirror.DecisionAutomatic {
			continue
		}
		if !s.automaticWrites.Load() || !s.remote.Enabled() || s.visibility == nil {
			continue
		}
		target, targetErr := s.store.GetTarget(ctx, policy.TargetSiteID)
		if targetErr != nil {
			failures = append(failures, fmt.Errorf("mirror policy %s target: %w", policy.PolicyUUID, targetErr))
			continue
		}
		if !target.Enabled || strings.ToLower(strings.TrimSpace(target.WriteMode)) != "auto" || !preview.Executable {
			continue
		}
		receipt, applyErr := s.ApplyPreview(ctx, ApplyRequest{PreviewID: preview.ID})
		if applyErr != nil {
			failures = append(failures, fmt.Errorf("mirror policy %s apply: %w", policy.PolicyUUID, applyErr))
			continue
		}
		id := supplierID
		_ = s.store.AddActivity(ctx, domain.Activity{Kind: "complete_mirror_applied", Severity: "success",
			Title:      "Complete mirror applied and verified",
			Message:    fmt.Sprintf("%s -> %s; change set #%d", policy.SourceGroup, policy.TargetGroup, receipt.Receipt.ID),
			SupplierID: &id, Metadata: map[string]string{"policy_uuid": policy.PolicyUUID,
				"change_set_id": fmt.Sprint(receipt.Receipt.ID), "preview_id": preview.ID}})
	}
	return errors.Join(failures...)
}

func (s *Service) confirmImmediately(ctx context.Context, supplierID int64, policy mirror.Policy,
	preview Preview) (Preview, error) {
	s.confirmationMu.Lock()
	refresh := s.refreshSupplier
	if refresh == nil {
		s.confirmationMu.Unlock()
		return preview, nil
	}
	if _, exists := s.confirming[supplierID]; exists {
		s.confirmationMu.Unlock()
		return preview, nil
	}
	s.confirming[supplierID] = struct{}{}
	s.confirmationMu.Unlock()
	defer func() {
		s.confirmationMu.Lock()
		delete(s.confirming, supplierID)
		s.confirmationMu.Unlock()
	}()

	attempts := s.options.ConfirmationAttempts
	remaining := maximumRemaining(preview.Confirmations)
	if remaining < attempts {
		attempts = remaining
	}
	for attempt := 0; attempt < attempts && !preview.EvidenceReady; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(s.options.ConfirmationInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return preview, ctx.Err()
			case <-timer.C:
			}
		}
		if err := refresh(ctx, supplierID); err != nil {
			return preview, err
		}
		entry, err := s.buildPreview(ctx, policy)
		if err != nil {
			return preview, err
		}
		s.remember(entry)
		preview = entry.public
	}
	return preview, nil
}

func maximumRemaining(confirmations []Confirmation) int {
	remaining := 0
	for _, confirmation := range confirmations {
		value := confirmation.Result.Required - confirmation.Result.Confirmed
		if value > remaining {
			remaining = value
		}
	}
	return remaining
}
