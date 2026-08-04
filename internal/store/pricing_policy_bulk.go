package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

var (
	ErrPolicyBulkInvalid = errors.New("invalid pricing policy bulk mutation")
	ErrPolicyBulkBlocked = errors.New("pricing policy bulk mutation is blocked")
	ErrPolicyBulkStale   = errors.New("pricing policy bulk preview is stale")
)

const maximumBulkPolicyCount = 500

type policyBulkQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) PreviewPolicyBulkMutation(ctx context.Context, input domain.PricingPolicyBulkMutation) (domain.PricingPolicyBulkPreview, error) {
	normalized, err := normalizePolicyBulkMutation(input)
	if err != nil {
		return domain.PricingPolicyBulkPreview{}, err
	}
	return buildPolicyBulkPreview(ctx, s.db, normalized, time.Now().UTC())
}

func (s *Store) ApplyPolicyBulkMutation(ctx context.Context, input domain.PricingPolicyBulkMutation) (domain.PricingPolicyBulkResult, error) {
	normalized, err := normalizePolicyBulkMutation(input)
	if err != nil {
		return domain.PricingPolicyBulkResult{}, err
	}
	if strings.TrimSpace(normalized.PreviewHash) == "" {
		return domain.PricingPolicyBulkResult{}, fmt.Errorf("%w: preview_hash is required", ErrPolicyBulkInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PricingPolicyBulkResult{}, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	preview, err := buildPolicyBulkPreview(ctx, tx, normalized, now)
	if err != nil {
		return domain.PricingPolicyBulkResult{}, err
	}
	result := domain.PricingPolicyBulkResult{
		Status: "previewed", Strategy: normalized.Strategy, Summary: preview.Summary,
		Items: preview.Items, Targets: preview.Targets, PreviewHash: preview.PreviewHash,
	}
	if !strings.EqualFold(strings.TrimSpace(normalized.PreviewHash), preview.PreviewHash) {
		result.Status = "stale"
		return result, fmt.Errorf("%w: rules or target write permissions changed; preview again", ErrPolicyBulkStale)
	}
	if normalized.Strategy == domain.PolicyBulkStrategyAtomic && preview.Summary.Blocked > 0 {
		result.Status = "blocked"
		return result, fmt.Errorf("%w: %d selected rules are blocked", ErrPolicyBulkBlocked, preview.Summary.Blocked)
	}

	applied := 0
	for index := range result.Items {
		item := &result.Items[index]
		switch item.Status {
		case "ready":
			update, updateErr := tx.ExecContext(ctx, `UPDATE pricing_policies SET execution_mode=?,enabled=?,updated_at=? WHERE id=? AND updated_at=?`,
				item.AfterExecutionMode, boolInt(item.AfterEnabled), formatTime(now), item.PolicyID, formatTime(*item.PolicyUpdatedAt))
			if updateErr != nil {
				return domain.PricingPolicyBulkResult{}, fmt.Errorf("update pricing policy %d: %w", item.PolicyID, updateErr)
			}
			count, countErr := update.RowsAffected()
			if countErr != nil {
				return domain.PricingPolicyBulkResult{}, fmt.Errorf("read pricing policy %d update result: %w", item.PolicyID, countErr)
			}
			if count != 1 {
				return domain.PricingPolicyBulkResult{}, fmt.Errorf("%w: pricing policy %d changed while applying", ErrPolicyBulkStale, item.PolicyID)
			}
			item.Status = "applied"
			item.Message = "规则状态已更新"
			updated := now
			item.PolicyUpdatedAt = &updated
			applied++
		case "unchanged":
			item.Message = "规则已经处于目标状态"
		}
	}

	result.Summary.Changed = applied
	result.Status = "no_change"
	if applied > 0 {
		result.Status = "applied"
		if result.Summary.Blocked > 0 {
			result.Status = "partial"
		}
		appliedAt := now
		result.AppliedAt = &appliedAt
	}
	if result.Summary.Blocked > 0 && applied == 0 {
		result.Status = "blocked"
	}

	metadata := map[string]string{
		"strategy": normalized.Strategy, "selected": fmt.Sprint(result.Summary.Selected),
		"applied": fmt.Sprint(applied), "unchanged": fmt.Sprint(result.Summary.Unchanged),
		"blocked": fmt.Sprint(result.Summary.Blocked), "preview_hash": preview.PreviewHash,
		"policy_ids": joinPolicyIDs(normalized.PolicyIDs),
	}
	if normalized.ExecutionMode != nil {
		metadata["execution_mode"] = *normalized.ExecutionMode
	}
	if normalized.Enabled != nil {
		metadata["enabled"] = fmt.Sprint(*normalized.Enabled)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activities(kind,severity,title,message,supplier_id,metadata_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		"pricing_policy_bulk_update", "info", "批量规则状态已处理",
		fmt.Sprintf("已修改 %d 条，无需修改 %d 条，阻断 %d 条", applied, result.Summary.Unchanged, result.Summary.Blocked),
		nil, mustJSON(metadata), formatTime(now)); err != nil {
		return domain.PricingPolicyBulkResult{}, fmt.Errorf("write pricing policy bulk audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.PricingPolicyBulkResult{}, err
	}
	return result, nil
}

func normalizePolicyBulkMutation(input domain.PricingPolicyBulkMutation) (domain.PricingPolicyBulkMutation, error) {
	if input.ExecutionMode == nil && input.Enabled == nil {
		return input, fmt.Errorf("%w: execution_mode or enabled is required", ErrPolicyBulkInvalid)
	}
	if input.ExecutionMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*input.ExecutionMode))
		if mode != "approval" && mode != "auto" {
			return input, fmt.Errorf("%w: execution_mode must be approval or auto", ErrPolicyBulkInvalid)
		}
		input.ExecutionMode = &mode
	}
	strategy := strings.ToLower(strings.TrimSpace(input.Strategy))
	if strategy == "" {
		strategy = domain.PolicyBulkStrategyAtomic
	}
	if strategy != domain.PolicyBulkStrategyAtomic && strategy != domain.PolicyBulkStrategyBestEffort {
		return input, fmt.Errorf("%w: strategy must be atomic or best_effort", ErrPolicyBulkInvalid)
	}
	input.Strategy = strategy
	unique := make(map[int64]struct{}, len(input.PolicyIDs))
	for _, id := range input.PolicyIDs {
		if id <= 0 {
			return input, fmt.Errorf("%w: policy_ids must contain positive integers", ErrPolicyBulkInvalid)
		}
		unique[id] = struct{}{}
	}
	if len(unique) == 0 || len(unique) > maximumBulkPolicyCount {
		return input, fmt.Errorf("%w: policy_ids must contain between 1 and %d unique IDs", ErrPolicyBulkInvalid, maximumBulkPolicyCount)
	}
	input.PolicyIDs = make([]int64, 0, len(unique))
	for id := range unique {
		input.PolicyIDs = append(input.PolicyIDs, id)
	}
	sort.Slice(input.PolicyIDs, func(i, j int) bool { return input.PolicyIDs[i] < input.PolicyIDs[j] })
	return input, nil
}

func buildPolicyBulkPreview(ctx context.Context, query policyBulkQuerier, input domain.PricingPolicyBulkMutation, now time.Time) (domain.PricingPolicyBulkPreview, error) {
	preview := domain.PricingPolicyBulkPreview{
		ExecutionMode: input.ExecutionMode, Enabled: input.Enabled, Strategy: input.Strategy,
		Items:   make([]domain.PricingPolicyBulkItem, 0, len(input.PolicyIDs)),
		Targets: make([]domain.PricingPolicyBulkTargetCheck, 0), GeneratedAt: now,
	}
	preview.Summary.Selected = len(input.PolicyIDs)
	targetChecks := map[int64]*domain.PricingPolicyBulkTargetCheck{}

	for _, id := range input.PolicyIDs {
		policy, err := scanPolicy(query.QueryRowContext(ctx, policySelect+` WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			preview.Items = append(preview.Items, domain.PricingPolicyBulkItem{PolicyID: id, Status: "blocked", Code: "policy_not_found", Message: "规则不存在或已被删除"})
			preview.Summary.Blocked++
			continue
		}
		if err != nil {
			return domain.PricingPolicyBulkPreview{}, fmt.Errorf("read pricing policy %d: %w", id, err)
		}
		preview.Summary.Found++
		target, err := scanTarget(query.QueryRowContext(ctx, targetSelect+` WHERE id=?`, policy.TargetSiteID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return domain.PricingPolicyBulkPreview{}, fmt.Errorf("read target site %d: %w", policy.TargetSiteID, err)
		}
		item := domain.PricingPolicyBulkItem{
			PolicyID: policy.ID, PolicyName: policy.Name, TargetSiteID: policy.TargetSiteID,
			BeforeExecutionMode: policy.ExecutionMode, AfterExecutionMode: policy.ExecutionMode,
			BeforeEnabled: policy.Enabled, AfterEnabled: policy.Enabled, Status: "ready", Message: "可以应用",
		}
		policyUpdated := policy.UpdatedAt
		item.PolicyUpdatedAt = &policyUpdated
		if input.ExecutionMode != nil {
			item.AfterExecutionMode = *input.ExecutionMode
		}
		if input.Enabled != nil {
			item.AfterEnabled = *input.Enabled
		}
		requiresAutoWrite := item.AfterExecutionMode == "auto" && (input.ExecutionMode != nil || (input.Enabled != nil && *input.Enabled))
		if errors.Is(err, sql.ErrNoRows) {
			item.Status, item.Code, item.Message = "blocked", "target_not_found", "规则对应的目标站点不存在"
		} else {
			item.TargetSiteName, item.TargetEnabled, item.TargetWriteMode = target.Name, target.Enabled, target.WriteMode
			targetUpdated := target.UpdatedAt
			item.TargetUpdatedAt = &targetUpdated
			check := targetChecks[target.ID]
			if check == nil {
				check = &domain.PricingPolicyBulkTargetCheck{TargetSiteID: target.ID, TargetSiteName: target.Name,
					Enabled: target.Enabled, WriteMode: target.WriteMode, AutoWriteReady: true,
					Message: "本次操作不需要目标站自动写权限"}
				targetChecks[target.ID] = check
			}
			check.PolicyCount++
			if requiresAutoWrite {
				check.RequiresAutoWrite = true
				check.AutoWriteReady = target.Enabled && target.WriteMode == "auto"
				if !target.Enabled {
					check.Message = "目标站点已停用"
					item.Status, item.Code, item.Message = "blocked", "target_disabled", "目标站点已停用，不能启用自动规则"
					preview.Summary.AutoWriteBlocked++
				} else if target.WriteMode != "auto" {
					check.Message = "目标站点仍处于仅审批写入"
					item.Status, item.Code, item.Message = "blocked", "target_write_mode_not_auto", "目标站点未允许受控自动写入"
					preview.Summary.AutoWriteBlocked++
				} else {
					check.Message = "目标站点允许受控自动写入"
					preview.Summary.AutoWriteReady++
				}
			}
		}
		if item.Status != "blocked" && item.BeforeExecutionMode == item.AfterExecutionMode && item.BeforeEnabled == item.AfterEnabled {
			item.Status, item.Code, item.Message = "unchanged", "already_desired", "规则已经处于目标状态"
			preview.Summary.Unchanged++
		} else if item.Status == "blocked" {
			preview.Summary.Blocked++
		} else {
			preview.Summary.Changed++
			if item.BeforeExecutionMode != item.AfterExecutionMode {
				preview.Summary.ExecutionModeChanges++
			}
			if item.BeforeEnabled != item.AfterEnabled {
				if item.AfterEnabled {
					preview.Summary.EnableChanges++
				} else {
					preview.Summary.DisableChanges++
				}
			}
		}
		preview.Items = append(preview.Items, item)
	}

	for _, check := range targetChecks {
		preview.Targets = append(preview.Targets, *check)
	}
	sort.Slice(preview.Targets, func(i, j int) bool { return preview.Targets[i].TargetSiteID < preview.Targets[j].TargetSiteID })
	preview.Summary.TargetsAffected = len(preview.Targets)
	hash, err := policyBulkPreviewHash(input, preview.Items)
	if err != nil {
		return domain.PricingPolicyBulkPreview{}, err
	}
	preview.PreviewHash = hash
	return preview, nil
}

func policyBulkPreviewHash(input domain.PricingPolicyBulkMutation, items []domain.PricingPolicyBulkItem) (string, error) {
	canonical := struct {
		PolicyIDs     []int64                        `json:"policy_ids"`
		ExecutionMode *string                        `json:"execution_mode,omitempty"`
		Enabled       *bool                          `json:"enabled,omitempty"`
		Items         []domain.PricingPolicyBulkItem `json:"items"`
	}{PolicyIDs: input.PolicyIDs, ExecutionMode: input.ExecutionMode, Enabled: input.Enabled, Items: items}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode pricing policy bulk preview: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func joinPolicyIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for index, id := range ids {
		parts[index] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}
