package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (s *Store) GetDiscovery(ctx context.Context, supplierID int64, kind, externalID string) (domain.DiscoveryItem, error) {
	var item domain.DiscoveryItem
	var first, last string
	var acknowledged sql.NullString
	var baseline int
	err := s.db.QueryRowContext(ctx, `SELECT d.supplier_id,s.name,d.kind,d.external_id,d.name,d.ratio,d.model_count,
		d.first_seen_at,d.last_seen_at,d.acknowledged_at,d.is_baseline
		FROM discoveries d JOIN suppliers s ON s.id=d.supplier_id
		WHERE d.supplier_id=? AND d.kind=? AND d.external_id=?`, supplierID, kind, externalID).
		Scan(&item.SupplierID, &item.SupplierName, &item.Kind, &item.ExternalID, &item.Name, &item.Ratio,
			&item.ModelCount, &first, &last, &acknowledged, &baseline)
	if err != nil {
		return domain.DiscoveryItem{}, err
	}
	item.FirstSeenAt = parseTime(first)
	item.LastSeenAt = parseTime(last)
	if acknowledged.Valid {
		value := parseTime(acknowledged.String)
		item.AcknowledgedAt = &value
	}
	item.Baseline = baseline != 0
	return item, nil
}

func (s *Store) CreateListingPlan(ctx context.Context, plan *domain.ListingPlan) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO listing_plans
		(supplier_id,discovery_kind,discovery_external_id,source_snapshot_hash,source_observed_at,source_group,source_ratio,
		equivalent_cost_ratio,cost_ratio_vector_json,target_catalog_hash,profit_verified,profit_verification,
		target_site_id,target_group_mode,target_group,expected_target_hash,expected_group_ratios_json,desired_target_hash,
		expected_group_existed,expected_group_ratio,
		proposed_group_ratio,upstream_credential_id,channel_json,pricing_rule_json,calculation_json,status,error,
		group_model_pricing_plan_json,idempotency_key,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		plan.SupplierID, plan.DiscoveryKind, plan.DiscoveryExternalID, plan.SourceSnapshotHash,
		formatTime(plan.SourceObservedAt), plan.SourceGroup, plan.SourceRatio, plan.EquivalentCostRatio,
		mustJSON(plan.CostRatioVector), plan.TargetCatalogHash, boolInt(plan.ProfitVerified), plan.ProfitVerification,
		plan.TargetSiteID, plan.TargetGroupMode, plan.TargetGroup, plan.ExpectedTargetHash,
		mustJSON(plan.ExpectedGroupRatios), plan.DesiredTargetHash, boolInt(plan.ExpectedGroupExisted), plan.ExpectedGroupRatio,
		plan.ProposedGroupRatio, plan.UpstreamCredentialID, mustJSON(plan.Channel), mustJSON(plan.PricingRule),
		mustJSON(plan.Calculation), plan.Status, plan.Error, mustJSON(plan.GroupModelPricing), plan.IdempotencyKey, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create listing plan: %w", err)
	}
	plan.ID, _ = result.LastInsertId()
	plan.CreatedAt = now
	plan.UpdatedAt = now
	return nil
}

func (s *Store) GetListingPlan(ctx context.Context, id int64) (domain.ListingPlan, error) {
	return scanListingPlan(s.db.QueryRowContext(ctx, listingPlanSelect+` WHERE id=?`, id))
}

func (s *Store) GetListingPlanByIdempotency(ctx context.Context, key string) (domain.ListingPlan, error) {
	return scanListingPlan(s.db.QueryRowContext(ctx, listingPlanSelect+` WHERE idempotency_key=?`, key))
}

func (s *Store) ListListingPlans(ctx context.Context, status string, limit int) ([]domain.ListingPlan, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := listingPlanSelect
	args := make([]any, 0, 2)
	if strings.TrimSpace(status) != "" {
		query += ` WHERE status=?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ListingPlan, 0)
	for rows.Next() {
		item, err := scanListingPlan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) TransitionListingPlan(ctx context.Context, id int64, from []string, status, message string) error {
	if len(from) == 0 {
		return errors.New("listing plan transition requires at least one source state")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	query := `UPDATE listing_plans SET status=?,error=?,updated_at=? WHERE id=? AND status IN (` + placeholders + `)`
	args := []any{status, message, formatTime(time.Now().UTC()), id}
	for _, value := range from {
		args = append(args, value)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("listing plan %d state changed concurrently", id)
	}
	return nil
}

const listingPlanSelect = `SELECT id,supplier_id,discovery_kind,discovery_external_id,source_snapshot_hash,
	source_observed_at,source_group,source_ratio,equivalent_cost_ratio,cost_ratio_vector_json,target_catalog_hash,
	profit_verified,profit_verification,target_site_id,target_group_mode,target_group,expected_target_hash,
	expected_group_ratios_json,desired_target_hash,expected_group_existed,expected_group_ratio,proposed_group_ratio,upstream_credential_id,channel_json,
	pricing_rule_json,calculation_json,status,error,group_model_pricing_plan_json,idempotency_key,created_at,updated_at FROM listing_plans`

func scanListingPlan(row scanner) (domain.ListingPlan, error) {
	var item domain.ListingPlan
	var sourceObserved, vectorJSON, expectedRatiosJSON, channelJSON, ruleJSON, calculationJSON, groupModelPricingJSON, created, updated string
	var existed, profitVerified int
	err := row.Scan(&item.ID, &item.SupplierID, &item.DiscoveryKind, &item.DiscoveryExternalID,
		&item.SourceSnapshotHash, &sourceObserved, &item.SourceGroup, &item.SourceRatio, &item.EquivalentCostRatio,
		&vectorJSON, &item.TargetCatalogHash, &profitVerified, &item.ProfitVerification, &item.TargetSiteID,
		&item.TargetGroupMode, &item.TargetGroup, &item.ExpectedTargetHash, &expectedRatiosJSON, &item.DesiredTargetHash,
		&existed, &item.ExpectedGroupRatio,
		&item.ProposedGroupRatio, &item.UpstreamCredentialID, &channelJSON, &ruleJSON, &calculationJSON,
		&item.Status, &item.Error, &groupModelPricingJSON, &item.IdempotencyKey, &created, &updated)
	if err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(channelJSON), &item.Channel); err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(vectorJSON), &item.CostRatioVector); err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(expectedRatiosJSON), &item.ExpectedGroupRatios); err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(ruleJSON), &item.PricingRule); err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(calculationJSON), &item.Calculation); err != nil {
		return domain.ListingPlan{}, err
	}
	if err := json.Unmarshal([]byte(groupModelPricingJSON), &item.GroupModelPricing); err != nil {
		return domain.ListingPlan{}, err
	}
	item.ExpectedGroupExisted = existed != 0
	item.ProfitVerified = profitVerified != 0
	item.SourceObservedAt = parseTime(sourceObserved)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

// CreateOrGetListingChangeSet provides request-level idempotency without
// trusting a unique-constraint error string from a specific SQLite driver.
func (s *Store) CreateOrGetListingChangeSet(ctx context.Context, change *domain.ListingChangeSet) (bool, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO listing_change_sets
		(plan_id,target_site_id,status,step,idempotency_key,ownership_marker,expected_target_hash,applied_target_hash,
		created_channel_id,pricing_policy_id,policy_created,policy_initially_on,policy_activated,ratio_changed,channel_enabled,
		rollback_status,group_model_pricing_receipt_json,error,started_at,finished_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, change.PlanID, change.TargetSiteID, change.Status, change.Step,
		change.IdempotencyKey, change.OwnershipMarker, change.ExpectedTargetHash, change.AppliedTargetHash,
		change.CreatedChannelID, change.PricingPolicyID, boolInt(change.PolicyCreated), boolInt(change.PolicyInitiallyOn),
		boolInt(change.PolicyActivated), boolInt(change.RatioChanged), boolInt(change.ChannelEnabled), change.RollbackStatus, mustJSON(change.GroupModelPricing),
		change.Error, nullableTime(change.StartedAt), nullableTime(change.FinishedAt), formatTime(now), formatTime(now))
	if err == nil {
		change.ID, _ = result.LastInsertId()
		change.CreatedAt = now
		change.UpdatedAt = now
		return true, nil
	}
	existing, getErr := s.GetListingChangeSetByIdempotency(ctx, change.IdempotencyKey)
	if getErr != nil {
		return false, fmt.Errorf("create listing change set: %w", err)
	}
	if existing.PlanID != change.PlanID {
		return false, fmt.Errorf("idempotency key already belongs to listing plan %d", existing.PlanID)
	}
	*change = existing
	return false, nil
}

func (s *Store) GetListingChangeSet(ctx context.Context, id int64) (domain.ListingChangeSet, error) {
	return scanListingChangeSet(s.db.QueryRowContext(ctx, listingChangeSetSelect+` WHERE id=?`, id))
}

func (s *Store) GetListingChangeSetByIdempotency(ctx context.Context, key string) (domain.ListingChangeSet, error) {
	return scanListingChangeSet(s.db.QueryRowContext(ctx, listingChangeSetSelect+` WHERE idempotency_key=?`, key))
}

func (s *Store) ListListingChangeSets(ctx context.Context, planID int64, limit int) ([]domain.ListingChangeSet, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := listingChangeSetSelect
	args := make([]any, 0, 2)
	if planID > 0 {
		query += ` WHERE plan_id=?`
		args = append(args, planID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.ListingChangeSet, 0)
	for rows.Next() {
		item, err := scanListingChangeSet(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// UpdateListingChangeSet writes the complete checkpoint using a status CAS.
func (s *Store) UpdateListingChangeSet(ctx context.Context, change domain.ListingChangeSet, from []string) error {
	if len(from) == 0 {
		return errors.New("listing change set transition requires at least one source state")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	query := `UPDATE listing_change_sets SET status=?,step=?,applied_target_hash=?,created_channel_id=?,pricing_policy_id=?,
		policy_created=?,policy_initially_on=?,policy_activated=?,ratio_changed=?,channel_enabled=?,rollback_status=?,error=?,
		group_model_pricing_receipt_json=?,started_at=?,finished_at=?,updated_at=? WHERE id=? AND status IN (` + placeholders + `)`
	args := []any{change.Status, change.Step, change.AppliedTargetHash, change.CreatedChannelID, change.PricingPolicyID,
		boolInt(change.PolicyCreated), boolInt(change.PolicyInitiallyOn), boolInt(change.PolicyActivated), boolInt(change.RatioChanged),
		boolInt(change.ChannelEnabled), change.RollbackStatus, change.Error, mustJSON(change.GroupModelPricing), nullableTime(change.StartedAt),
		nullableTime(change.FinishedAt), formatTime(time.Now().UTC()), change.ID}
	for _, value := range from {
		args = append(args, value)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("listing change set %d state changed concurrently", change.ID)
	}
	return nil
}

const listingChangeSetSelect = `SELECT id,plan_id,target_site_id,status,step,idempotency_key,ownership_marker,
	expected_target_hash,applied_target_hash,created_channel_id,pricing_policy_id,policy_created,policy_initially_on,
	policy_activated,ratio_changed,channel_enabled,rollback_status,group_model_pricing_receipt_json,error,
	started_at,finished_at,created_at,updated_at FROM listing_change_sets`

func scanListingChangeSet(row scanner) (domain.ListingChangeSet, error) {
	var item domain.ListingChangeSet
	var channelID, policyID sql.NullInt64
	var policyCreated, policyInitiallyOn, policyActivated, ratioChanged, channelEnabled int
	var groupModelPricingJSON string
	var started, finished sql.NullString
	var created, updated string
	err := row.Scan(&item.ID, &item.PlanID, &item.TargetSiteID, &item.Status, &item.Step,
		&item.IdempotencyKey, &item.OwnershipMarker, &item.ExpectedTargetHash, &item.AppliedTargetHash,
		&channelID, &policyID, &policyCreated, &policyInitiallyOn, &policyActivated, &ratioChanged, &channelEnabled,
		&item.RollbackStatus, &groupModelPricingJSON, &item.Error, &started, &finished,
		&created, &updated)
	if err != nil {
		return domain.ListingChangeSet{}, err
	}
	if channelID.Valid {
		value := channelID.Int64
		item.CreatedChannelID = &value
	}
	if policyID.Valid {
		value := policyID.Int64
		item.PricingPolicyID = &value
	}
	item.PolicyCreated = policyCreated != 0
	item.PolicyInitiallyOn = policyInitiallyOn != 0
	item.PolicyActivated = policyActivated != 0
	item.RatioChanged = ratioChanged != 0
	item.ChannelEnabled = channelEnabled != 0
	if err := json.Unmarshal([]byte(groupModelPricingJSON), &item.GroupModelPricing); err != nil {
		return domain.ListingChangeSet{}, err
	}
	if started.Valid {
		value := parseTime(started.String)
		item.StartedAt = &value
	}
	if finished.Valid {
		value := parseTime(finished.String)
		item.FinishedAt = &value
	}
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

// GetPolicyByTarget returns the single publisher allowed to own a NewAPI group
// multiplier. The database uniqueness constraint is the final race guard.
func (s *Store) GetPolicyByTarget(ctx context.Context, targetID int64, targetGroup string) (domain.PricingPolicy, error) {
	return scanPolicy(s.db.QueryRowContext(ctx, policySelect+` WHERE target_site_id=? AND target_group=?`, targetID, targetGroup))
}

// BindListingPolicy atomically creates or binds the continuing pricing policy
// and checkpoints its ownership before any remote target mutation starts.
func (s *Store) BindListingPolicy(ctx context.Context, changeID int64, desired domain.PricingPolicy) (domain.PricingPolicy, bool, bool, bool, error) {
	desired.SourceBasis = "group_ratio"
	applyPolicyDefaults(&desired)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.PricingPolicy{}, false, false, false, err
	}
	defer tx.Rollback()

	var boundID sql.NullInt64
	var createdFlag, initialFlag, activatedFlag int
	if err := tx.QueryRowContext(ctx, `SELECT pricing_policy_id,policy_created,policy_initially_on,policy_activated
		FROM listing_change_sets WHERE id=? AND status='applying'`, changeID).
		Scan(&boundID, &createdFlag, &initialFlag, &activatedFlag); err != nil {
		return domain.PricingPolicy{}, false, false, false, err
	}
	if boundID.Valid {
		bound, err := scanPolicy(tx.QueryRowContext(ctx, policySelect+` WHERE id=?`, boundID.Int64))
		if err != nil {
			return domain.PricingPolicy{}, false, false, false, err
		}
		if !samePolicyDefinition(bound, desired) {
			return domain.PricingPolicy{}, false, false, false, errors.New("the target group is already bound to a different pricing policy")
		}
		if err := tx.Commit(); err != nil {
			return domain.PricingPolicy{}, false, false, false, err
		}
		return bound, createdFlag != 0, initialFlag != 0, activatedFlag != 0, nil
	}

	bound, findErr := scanPolicy(tx.QueryRowContext(ctx, policySelect+` WHERE target_site_id=? AND target_group=?`,
		desired.TargetSiteID, desired.TargetGroup))
	created := false
	initiallyOn := false
	if errors.Is(findErr, sql.ErrNoRows) {
		now := time.Now().UTC()
		desired.Enabled = false
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO pricing_policies
			(name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,profit_basis,
			revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,
			maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, desired.Name, desired.SupplierID, desired.SourceGroup,
			desired.SourceModelPattern, desired.SourceBasis, desired.TargetSiteID, desired.TargetGroup, desired.RuleType, desired.Value, desired.ProfitBasis,
			desired.RevenueCashFactor, desired.CostCashFactor, desired.RiskBufferRate, boolInt(desired.AllowNegativeProfit),
			desired.SubsidyBudgetDaily, desired.MinimumRatio, desired.MaximumRatio, desired.RoundingStep,
			boolInt(desired.OnlyFollowIncrease), boolInt(desired.OnlyFollowDecrease), 0, desired.ExecutionMode,
			formatTime(now), formatTime(now))
		if insertErr != nil {
			return domain.PricingPolicy{}, false, false, false, fmt.Errorf("create listing pricing policy: %w", insertErr)
		}
		desired.ID, _ = result.LastInsertId()
		desired.CreatedAt = now
		desired.UpdatedAt = now
		bound = desired
		created = true
	} else if findErr != nil {
		return domain.PricingPolicy{}, false, false, false, findErr
	} else {
		if !samePolicyDefinition(bound, desired) {
			return domain.PricingPolicy{}, false, false, false, errors.New("the target group is already owned by a different pricing policy")
		}
		initiallyOn = bound.Enabled
	}

	result, err := tx.ExecContext(ctx, `UPDATE listing_change_sets SET pricing_policy_id=?,policy_created=?,
		policy_initially_on=?,policy_activated=?,updated_at=? WHERE id=? AND status='applying' AND pricing_policy_id IS NULL`,
		bound.ID, boolInt(created), boolInt(initiallyOn), boolInt(initiallyOn), formatTime(time.Now().UTC()), changeID)
	if err != nil {
		return domain.PricingPolicy{}, false, false, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return domain.PricingPolicy{}, false, false, false, errors.New("listing policy binding changed concurrently")
	}
	if err := tx.Commit(); err != nil {
		return domain.PricingPolicy{}, false, false, false, err
	}
	return bound, created, initiallyOn, initiallyOn, nil
}

func (s *Store) ActivateListingPolicy(ctx context.Context, changeID, policyID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE pricing_policies SET enabled=1,updated_at=? WHERE id=?`, formatTime(time.Now().UTC()), policyID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	result, err = tx.ExecContext(ctx, `UPDATE listing_change_sets SET policy_activated=1,updated_at=?
		WHERE id=? AND status='applying' AND pricing_policy_id=?`, formatTime(time.Now().UTC()), changeID, policyID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("listing policy activation checkpoint changed concurrently")
	}
	return tx.Commit()
}

// RestoreListingPolicy removes a policy created by this change set, or restores
// a previously disabled bound policy to disabled. Existing enabled policies are
// never disabled by compensation.
func (s *Store) RestoreListingPolicy(ctx context.Context, changeID int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var policyID sql.NullInt64
	var created, initiallyOn, activated int
	if err := tx.QueryRowContext(ctx, `SELECT pricing_policy_id,policy_created,policy_initially_on,policy_activated
		FROM listing_change_sets WHERE id=?`, changeID).Scan(&policyID, &created, &initiallyOn, &activated); err != nil {
		return "", err
	}
	if !policyID.Valid {
		return "no listing pricing policy to restore", tx.Commit()
	}
	message := "existing pricing policy preserved"
	if created != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE listing_change_sets SET pricing_policy_id=NULL,policy_created=0,
			policy_initially_on=0,policy_activated=0,updated_at=? WHERE id=?`, formatTime(time.Now().UTC()), changeID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_policies WHERE id=?`, policyID.Int64); err != nil {
			return "", err
		}
		message = "new listing pricing policy removed"
	} else if initiallyOn == 0 && activated != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE pricing_policies SET enabled=0,updated_at=? WHERE id=?`,
			formatTime(time.Now().UTC()), policyID.Int64); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE listing_change_sets SET policy_activated=0,updated_at=? WHERE id=?`,
			formatTime(time.Now().UTC()), changeID); err != nil {
			return "", err
		}
		message = "previously disabled pricing policy restored to disabled"
	}
	return message, tx.Commit()
}

func samePolicyDefinition(left, right domain.PricingPolicy) bool {
	applyPolicyDefaults(&left)
	applyPolicyDefaults(&right)
	return left.SupplierID == right.SupplierID && left.SourceGroup == right.SourceGroup &&
		left.SourceModelPattern == right.SourceModelPattern && left.SourceBasis == right.SourceBasis && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.RuleType == right.RuleType && left.Value == right.Value &&
		left.ProfitBasis == right.ProfitBasis && left.RevenueCashFactor == right.RevenueCashFactor &&
		left.CostCashFactor == right.CostCashFactor && left.RiskBufferRate == right.RiskBufferRate &&
		left.AllowNegativeProfit == right.AllowNegativeProfit && left.SubsidyBudgetDaily == right.SubsidyBudgetDaily &&
		left.MinimumRatio == right.MinimumRatio && left.MaximumRatio == right.MaximumRatio &&
		left.RoundingStep == right.RoundingStep && left.OnlyFollowIncrease == right.OnlyFollowIncrease &&
		left.OnlyFollowDecrease == right.OnlyFollowDecrease && left.ExecutionMode == right.ExecutionMode
}

func (s *Store) TryAcquireTargetChangeLease(ctx context.Context, targetID int64, ownerID string, now time.Time, ttl time.Duration) (bool, error) {
	if targetID <= 0 || strings.TrimSpace(ownerID) == "" || ttl <= 0 {
		return false, errors.New("target change lease requires target, owner and positive ttl")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkReleaseAdmissionOpenTx(ctx, tx); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO target_change_leases(target_site_id,owner_id,leased_until,updated_at)
		VALUES(?,?,?,?) ON CONFLICT(target_site_id) DO UPDATE SET owner_id=excluded.owner_id,
		leased_until=excluded.leased_until,updated_at=excluded.updated_at
		WHERE target_change_leases.leased_until<=? OR target_change_leases.owner_id=?`, targetID, ownerID,
		formatLeaseTime(now.Add(ttl)), formatLeaseTime(now), formatLeaseTime(now), ownerID)
	if err != nil {
		return false, fmt.Errorf("acquire target change lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ReleaseTargetChangeLease(ctx context.Context, targetID int64, ownerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM target_change_leases WHERE target_site_id=? AND owner_id=?`, targetID, ownerID)
	return err
}
