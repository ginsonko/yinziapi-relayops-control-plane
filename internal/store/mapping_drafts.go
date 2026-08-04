package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type MappingDraftPolicyResult struct {
	Policy  domain.PricingPolicy
	Created bool
}

var ErrManualMappingStale = errors.New("manual mapping preview is stale")

type ManualMappingPolicyResult struct {
	Policy     domain.PricingPolicy
	Created    bool
	Updated    bool
	Idempotent bool
}

type ManualMappingPolicyMutation struct {
	Policy                  domain.PricingPolicy
	ExpectedPolicyID        int64
	ExpectedPolicyUpdatedAt *time.Time
}

type MappingOwnershipConflictError struct {
	TargetSiteID int64
	TargetGroup  string
	Owner        domain.PricingPolicy
}

func (e *MappingOwnershipConflictError) Error() string {
	return fmt.Sprintf("target group %q on target %d is already owned by pricing policy %d", e.TargetGroup, e.TargetSiteID, e.Owner.ID)
}

// CreateMappingDraftPolicies creates an all-or-nothing batch of disabled,
// approval-only policies. The INSERT predicate is evaluated by SQLite in the
// same write statement, so a competing owner cannot be silently overwritten.
func (s *Store) CreateMappingDraftPolicies(ctx context.Context, policies []domain.PricingPolicy) ([]MappingDraftPolicyResult, error) {
	if len(policies) == 0 {
		return nil, errors.New("at least one mapping draft policy is required")
	}
	seen := make(map[string]struct{}, len(policies))
	for index := range policies {
		policies[index].SourceBasis = "group_ratio"
		applyPolicyDefaults(&policies[index])
		if policies[index].Enabled || policies[index].ExecutionMode != "approval" {
			return nil, fmt.Errorf("mapping draft %d must be disabled and use approval execution mode", index)
		}
		if strings.TrimSpace(policies[index].TargetGroup) == "" {
			return nil, fmt.Errorf("mapping draft %d has no target group", index)
		}
		if err := validatePolicyCoverage(policies[index]); err != nil {
			return nil, err
		}
		key := mappingOwnerKey(policies[index].TargetSiteID, policies[index].TargetGroup)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("mapping draft batch contains more than one owner for target group %q", policies[index].TargetGroup)
		}
		seen[key] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	results := make([]MappingDraftPolicyResult, 0, len(policies))
	for index := range policies {
		policy := &policies[index]
		owner, ownerErr := findMappingOwnerTx(ctx, tx, policy.TargetSiteID, policy.TargetGroup)
		if ownerErr == nil {
			if sameMappingOwner(owner, *policy) {
				results = append(results, MappingDraftPolicyResult{Policy: owner, Created: false})
				continue
			}
			return nil, &MappingOwnershipConflictError{TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, Owner: owner}
		}
		if !errors.Is(ownerErr, sql.ErrNoRows) {
			return nil, ownerErr
		}
		now := time.Now().UTC()
		result, err := tx.ExecContext(ctx, `INSERT INTO pricing_policies
			(name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,profit_basis,
			revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,
			maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,coverage_mode,
			minimum_coverage_ratio,created_at,updated_at)
			SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
			WHERE NOT EXISTS (
				SELECT 1 FROM pricing_policies WHERE target_site_id=? AND lower(trim(target_group))=lower(trim(?))
			)`, policy.Name, policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID,
			policy.TargetGroup, policy.RuleType, policy.Value, policy.ProfitBasis, policy.RevenueCashFactor,
			policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily,
			policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep, boolInt(policy.OnlyFollowIncrease),
			boolInt(policy.OnlyFollowDecrease), 0, "approval", policy.CoverageMode, policy.MinimumCoverage,
			formatTime(now), formatTime(now), policy.TargetSiteID, policy.TargetGroup)
		if err != nil {
			return nil, fmt.Errorf("create mapping draft policy: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			owner, ownerErr = findMappingOwnerTx(ctx, tx, policy.TargetSiteID, policy.TargetGroup)
			if ownerErr == nil && sameMappingOwner(owner, *policy) {
				results = append(results, MappingDraftPolicyResult{Policy: owner, Created: false})
				continue
			}
			if ownerErr == nil {
				return nil, &MappingOwnershipConflictError{TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, Owner: owner}
			}
			return nil, errors.New("mapping draft owner changed concurrently")
		}
		policy.ID, _ = result.LastInsertId()
		policy.Enabled = false
		policy.ExecutionMode = "approval"
		policy.CreatedAt = now
		policy.UpdatedAt = now
		results = append(results, MappingDraftPolicyResult{Policy: *policy, Created: true})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

// SaveManualMappingPolicy creates or edits one explicitly confirmed mapping.
// The target group remains the ownership boundary. Existing edits require the
// policy ID and timestamp from the preview, while exact request replays are
// idempotent even after the first create has committed.
func (s *Store) SaveManualMappingPolicy(ctx context.Context, policy domain.PricingPolicy, expectedPolicyID int64,
	expectedUpdatedAt *time.Time) (ManualMappingPolicyResult, error) {
	results, err := s.SaveManualMappingPolicies(ctx, []ManualMappingPolicyMutation{{
		Policy: policy, ExpectedPolicyID: expectedPolicyID, ExpectedPolicyUpdatedAt: expectedUpdatedAt,
	}})
	if err != nil {
		return ManualMappingPolicyResult{}, err
	}
	return results[0], nil
}

// SaveManualMappingPolicies atomically creates or edits an explicitly
// confirmed mapping batch. Any stale preview, ownership conflict or write
// failure rolls back every mutation in the batch.
func (s *Store) SaveManualMappingPolicies(ctx context.Context, mutations []ManualMappingPolicyMutation) ([]ManualMappingPolicyResult, error) {
	if len(mutations) == 0 {
		return nil, errors.New("at least one manual mapping mutation is required")
	}
	seen := make(map[string]struct{}, len(mutations))
	for index := range mutations {
		mutations[index].Policy.SourceBasis = "group_ratio"
		applyPolicyDefaults(&mutations[index].Policy)
		policy := mutations[index].Policy
		if strings.TrimSpace(policy.TargetGroup) == "" || policy.TargetSiteID <= 0 || policy.SupplierID <= 0 || strings.TrimSpace(policy.SourceGroup) == "" {
			return nil, fmt.Errorf("manual mapping %d requires target, target group, supplier and source group", index)
		}
		if err := validatePolicyCoverage(policy); err != nil {
			return nil, fmt.Errorf("manual mapping %d: %w", index, err)
		}
		key := mappingOwnerKey(policy.TargetSiteID, policy.TargetGroup)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("manual mapping batch contains more than one mutation for target group %q", policy.TargetGroup)
		}
		seen[key] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	results := make([]ManualMappingPolicyResult, 0, len(mutations))
	for index := range mutations {
		mutation := mutations[index]
		result, saveErr := saveManualMappingPolicyTx(ctx, tx, mutation.Policy, mutation.ExpectedPolicyID, mutation.ExpectedPolicyUpdatedAt)
		if saveErr != nil {
			return nil, fmt.Errorf("manual mapping %d: %w", index, saveErr)
		}
		results = append(results, result)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func saveManualMappingPolicyTx(ctx context.Context, tx *sql.Tx, policy domain.PricingPolicy, expectedPolicyID int64,
	expectedUpdatedAt *time.Time) (ManualMappingPolicyResult, error) {
	owners, err := listMappingOwnersTx(ctx, tx, policy.TargetSiteID, policy.TargetGroup)
	if err != nil {
		return ManualMappingPolicyResult{}, err
	}
	if len(owners) > 1 {
		return ManualMappingPolicyResult{}, fmt.Errorf("%w: target group %q has multiple policy owners", ErrManualMappingStale, policy.TargetGroup)
	}
	if len(owners) == 1 {
		owner := owners[0]
		if !sameMappingOwner(owner, policy) {
			return ManualMappingPolicyResult{}, &MappingOwnershipConflictError{TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, Owner: owner}
		}
		policy.ID = owner.ID
		policy.CreatedAt = owner.CreatedAt
		if equivalentManualPolicy(owner, policy) {
			return ManualMappingPolicyResult{Policy: owner, Idempotent: true}, nil
		}
		if expectedPolicyID != owner.ID || expectedUpdatedAt == nil || !expectedUpdatedAt.Equal(owner.UpdatedAt) {
			return ManualMappingPolicyResult{}, fmt.Errorf("%w: policy %d changed since preview", ErrManualMappingStale, owner.ID)
		}
		now := time.Now().UTC()
		result, updateErr := tx.ExecContext(ctx, `UPDATE pricing_policies SET name=?,supplier_id=?,source_group=?,source_model_pattern=?,source_basis=?,
			target_site_id=?,target_group=?,rule_type=?,value=?,profit_basis=?,revenue_cash_factor=?,cost_cash_factor=?,
			risk_buffer_rate=?,allow_negative_profit=?,subsidy_budget_daily=?,minimum_ratio=?,maximum_ratio=?,rounding_step=?,
			only_follow_increase=?,only_follow_decrease=?,enabled=?,execution_mode=?,coverage_mode=?,minimum_coverage_ratio=?,
			updated_at=? WHERE id=? AND updated_at=?`, policy.Name, policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern,
			policy.SourceBasis, policy.TargetSiteID, policy.TargetGroup, policy.RuleType, policy.Value, policy.ProfitBasis, policy.RevenueCashFactor,
			policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily,
			policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep, boolInt(policy.OnlyFollowIncrease),
			boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled), policy.ExecutionMode, policy.CoverageMode,
			policy.MinimumCoverage, formatTime(now), owner.ID, formatTime(owner.UpdatedAt))
		if updateErr != nil {
			return ManualMappingPolicyResult{}, fmt.Errorf("update manual mapping policy: %w", updateErr)
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return ManualMappingPolicyResult{}, fmt.Errorf("%w: policy %d changed concurrently", ErrManualMappingStale, owner.ID)
		}
		policy.UpdatedAt = now
		return ManualMappingPolicyResult{Policy: policy, Updated: true}, nil
	}
	if expectedPolicyID != 0 || expectedUpdatedAt != nil {
		return ManualMappingPolicyResult{}, fmt.Errorf("%w: the expected policy no longer owns target group %q", ErrManualMappingStale, policy.TargetGroup)
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO pricing_policies
		(name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,profit_basis,
		revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,
		maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,coverage_mode,
		minimum_coverage_ratio,created_at,updated_at)
		SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
		WHERE NOT EXISTS (
			SELECT 1 FROM pricing_policies WHERE target_site_id=? AND lower(trim(target_group))=lower(trim(?))
		)`, policy.Name, policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID,
		policy.TargetGroup, policy.RuleType, policy.Value, policy.ProfitBasis, policy.RevenueCashFactor,
		policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily,
		policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep, boolInt(policy.OnlyFollowIncrease),
		boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled), policy.ExecutionMode, policy.CoverageMode,
		policy.MinimumCoverage, formatTime(now), formatTime(now), policy.TargetSiteID, policy.TargetGroup)
	if err != nil {
		return ManualMappingPolicyResult{}, fmt.Errorf("create manual mapping policy: %w", err)
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		owner, ownerErr := findMappingOwnerTx(ctx, tx, policy.TargetSiteID, policy.TargetGroup)
		if ownerErr == nil && sameMappingOwner(owner, policy) && equivalentManualPolicy(owner, policy) {
			return ManualMappingPolicyResult{Policy: owner, Idempotent: true}, nil
		}
		if ownerErr == nil {
			return ManualMappingPolicyResult{}, &MappingOwnershipConflictError{TargetSiteID: policy.TargetSiteID, TargetGroup: policy.TargetGroup, Owner: owner}
		}
		return ManualMappingPolicyResult{}, fmt.Errorf("%w: target group owner changed concurrently", ErrManualMappingStale)
	}
	policy.ID, _ = result.LastInsertId()
	policy.CreatedAt, policy.UpdatedAt = now, now
	return ManualMappingPolicyResult{Policy: policy, Created: true}, nil
}

func listMappingOwnersTx(ctx context.Context, tx *sql.Tx, targetSiteID int64, targetGroup string) ([]domain.PricingPolicy, error) {
	rows, err := tx.QueryContext(ctx, policySelect+` WHERE target_site_id=? AND lower(trim(target_group))=lower(trim(?)) ORDER BY id`,
		targetSiteID, targetGroup)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := make([]domain.PricingPolicy, 0, 2)
	for rows.Next() {
		owner, scanErr := scanPolicy(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

func equivalentManualPolicy(left, right domain.PricingPolicy) bool {
	left.ID, right.ID = 0, 0
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func findMappingOwnerTx(ctx context.Context, tx *sql.Tx, targetSiteID int64, targetGroup string) (domain.PricingPolicy, error) {
	return scanPolicy(tx.QueryRowContext(ctx, policySelect+` WHERE target_site_id=? AND lower(trim(target_group))=lower(trim(?)) ORDER BY id LIMIT 1`,
		targetSiteID, targetGroup))
}

func sameMappingOwner(existing, desired domain.PricingPolicy) bool {
	return existing.TargetSiteID == desired.TargetSiteID && strings.EqualFold(strings.TrimSpace(existing.TargetGroup), strings.TrimSpace(desired.TargetGroup)) &&
		existing.SupplierID == desired.SupplierID && strings.EqualFold(strings.TrimSpace(existing.SourceGroup), strings.TrimSpace(desired.SourceGroup))
}

func mappingOwnerKey(targetSiteID int64, targetGroup string) string {
	return fmt.Sprintf("%d\x00%s", targetSiteID, strings.ToLower(strings.TrimSpace(targetGroup)))
}
