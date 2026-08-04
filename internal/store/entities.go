package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func (s *Store) ListCredentials(ctx context.Context) ([]domain.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.name,c.kind,c.site_url,c.expires_at,c.status,c.created_at,c.updated_at,
		COALESCE(r.configured,0),COALESCE(r.enabled,0),COALESCE(r.state,'unconfigured'),
		COALESCE(r.consecutive_failures,0),r.last_attempt_at,r.last_success_at,r.next_attempt_at,
		COALESCE(r.human_action,''),COALESCE(r.last_error_class,''),r.updated_at
		FROM credentials c LEFT JOIN credential_renewal_state r ON r.credential_id=c.id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Credential, 0)
	for rows.Next() {
		var item domain.Credential
		var expires sql.NullString
		var created, updated string
		var configured, enabled int
		var renewalUpdated, lastAttempt, lastSuccess, nextAttempt sql.NullString
		if err := rows.Scan(&item.ID, &item.Name, &item.Kind, &item.SiteURL, &expires, &item.Status, &created, &updated,
			&configured, &enabled, &item.Renewal.State, &item.Renewal.ConsecutiveFailures, &lastAttempt, &lastSuccess,
			&nextAttempt, &item.Renewal.HumanAction, &item.Renewal.LastErrorClass, &renewalUpdated); err != nil {
			return nil, err
		}
		item.Renewal.Configured = configured != 0
		item.Renewal.Enabled = enabled != 0
		item.Renewal.LastAttemptAt = nullableParsedTime(lastAttempt)
		item.Renewal.LastSuccessAt = nullableParsedTime(lastSuccess)
		item.Renewal.NextAttemptAt = nullableParsedTime(nextAttempt)
		item.Renewal.UpdatedAt = nullableParsedTime(renewalUpdated)
		item.CreatedAt = parseTime(created)
		item.UpdatedAt = parseTime(updated)
		if expires.Valid {
			value := parseTime(expires.String)
			item.ExpiresAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func nullableParsedTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed := parseTime(value.String)
	return &parsed
}

func (s *Store) DeleteCredential(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MarkCredentialStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE credentials SET status=?,updated_at=? WHERE id=?`, status, formatTime(time.Now().UTC()), id)
	return err
}

func (s *Store) FindSupplierByBaseURL(ctx context.Context, baseURL string) (domain.Supplier, error) {
	return scanSupplier(s.db.QueryRowContext(ctx, supplierSelect+` WHERE base_url=?`, strings.TrimRight(baseURL, "/")))
}

func (s *Store) CreateTarget(ctx context.Context, target *domain.TargetSite) error {
	now := time.Now().UTC()
	if target.Adapter == "" {
		target.Adapter = "newapi"
	}
	if target.WriteMode == "" {
		target.WriteMode = "approval"
	}
	if target.LastStatus == "" {
		target.LastStatus = "pending"
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO target_sites
		(name,base_url,adapter,credential_id,enabled,write_mode,last_status,last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, target.Name, strings.TrimRight(target.BaseURL, "/"), target.Adapter,
		target.CredentialID, boolInt(target.Enabled), target.WriteMode, target.LastStatus, target.LastError,
		formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create target: %w", err)
	}
	target.ID, _ = result.LastInsertId()
	target.CreatedAt = now
	target.UpdatedAt = now
	return nil
}

func (s *Store) UpdateTarget(ctx context.Context, target domain.TargetSite) error {
	result, err := s.db.ExecContext(ctx, `UPDATE target_sites SET name=?,base_url=?,adapter=?,credential_id=?,enabled=?,write_mode=?,updated_at=? WHERE id=?`,
		target.Name, strings.TrimRight(target.BaseURL, "/"), target.Adapter, target.CredentialID, boolInt(target.Enabled), target.WriteMode,
		formatTime(time.Now().UTC()), target.ID)
	if err != nil {
		return fmt.Errorf("update target: %w", err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) MarkTargetResult(ctx context.Context, id int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE target_sites SET last_status=?,last_error=?,updated_at=? WHERE id=?`,
		status, message, formatTime(time.Now().UTC()), id)
	return err
}

func (s *Store) GetTarget(ctx context.Context, id int64) (domain.TargetSite, error) {
	return scanTarget(s.db.QueryRowContext(ctx, targetSelect+` WHERE id=?`, id))
}

func (s *Store) ListTargets(ctx context.Context) ([]domain.TargetSite, error) {
	rows, err := s.db.QueryContext(ctx, targetSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.TargetSite, 0)
	for rows.Next() {
		item, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

const targetSelect = `SELECT id,name,base_url,adapter,credential_id,enabled,write_mode,last_status,last_error,created_at,updated_at FROM target_sites`

func scanTarget(row scanner) (domain.TargetSite, error) {
	var item domain.TargetSite
	var enabled int
	var created, updated string
	err := row.Scan(&item.ID, &item.Name, &item.BaseURL, &item.Adapter, &item.CredentialID, &enabled, &item.WriteMode,
		&item.LastStatus, &item.LastError, &created, &updated)
	if err != nil {
		return domain.TargetSite{}, err
	}
	item.Enabled = enabled != 0
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) CreatePolicy(ctx context.Context, policy *domain.PricingPolicy) error {
	now := time.Now().UTC()
	applyPolicyDefaults(policy)
	if err := validatePolicyCoverage(*policy); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO pricing_policies
		(name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,profit_basis,
		revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,
		maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,coverage_mode,
		minimum_coverage_ratio,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, policy.Name, policy.SupplierID, policy.SourceGroup,
		policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID, policy.TargetGroup, policy.RuleType, policy.Value, policy.ProfitBasis,
		policy.RevenueCashFactor, policy.CostCashFactor, policy.RiskBufferRate, boolInt(policy.AllowNegativeProfit),
		policy.SubsidyBudgetDaily, policy.MinimumRatio, policy.MaximumRatio, policy.RoundingStep,
		boolInt(policy.OnlyFollowIncrease), boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled), policy.ExecutionMode,
		policy.CoverageMode, policy.MinimumCoverage, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create pricing policy: %w", err)
	}
	policy.ID, _ = result.LastInsertId()
	policy.CreatedAt = now
	policy.UpdatedAt = now
	return nil
}

func (s *Store) UpdatePolicy(ctx context.Context, policy domain.PricingPolicy) error {
	applyPolicyDefaults(&policy)
	if err := validatePolicyCoverage(policy); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE pricing_policies SET name=?,supplier_id=?,source_group=?,source_model_pattern=?,source_basis=?,
		target_site_id=?,target_group=?,rule_type=?,value=?,profit_basis=?,revenue_cash_factor=?,cost_cash_factor=?,
		risk_buffer_rate=?,allow_negative_profit=?,subsidy_budget_daily=?,minimum_ratio=?,maximum_ratio=?,rounding_step=?,
		only_follow_increase=?,only_follow_decrease=?,enabled=?,execution_mode=?,coverage_mode=?,minimum_coverage_ratio=?,
		updated_at=? WHERE id=?`, policy.Name,
		policy.SupplierID, policy.SourceGroup, policy.SourceModelPattern, policy.SourceBasis, policy.TargetSiteID, policy.TargetGroup, policy.RuleType,
		policy.Value, policy.ProfitBasis, policy.RevenueCashFactor, policy.CostCashFactor, policy.RiskBufferRate,
		boolInt(policy.AllowNegativeProfit), policy.SubsidyBudgetDaily, policy.MinimumRatio, policy.MaximumRatio,
		policy.RoundingStep, boolInt(policy.OnlyFollowIncrease), boolInt(policy.OnlyFollowDecrease), boolInt(policy.Enabled),
		policy.ExecutionMode, policy.CoverageMode, policy.MinimumCoverage, formatTime(time.Now().UTC()), policy.ID)
	if err != nil {
		return fmt.Errorf("update pricing policy: %w", err)
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func applyPolicyDefaults(policy *domain.PricingPolicy) {
	if policy.SourceModelPattern == "" {
		policy.SourceModelPattern = "*"
	}
	if policy.SourceBasis == "" {
		policy.SourceBasis = "group_ratio"
	}
	policy.SourceBasis = strings.ToLower(strings.TrimSpace(policy.SourceBasis))
	if policy.ProfitBasis == "" {
		policy.ProfitBasis = "contribution"
	}
	if policy.RevenueCashFactor == "" {
		policy.RevenueCashFactor = "1"
	}
	if policy.CostCashFactor == "" {
		policy.CostCashFactor = "1"
	}
	if policy.RiskBufferRate == "" {
		policy.RiskBufferRate = "0"
	}
	if policy.SubsidyBudgetDaily == "" {
		policy.SubsidyBudgetDaily = "0"
	}
	if policy.MinimumRatio == "" {
		policy.MinimumRatio = "0"
	}
	if policy.MaximumRatio == "" {
		policy.MaximumRatio = "0"
	}
	if policy.RoundingStep == "" {
		policy.RoundingStep = "0.001"
	}
	if policy.ExecutionMode == "" {
		policy.ExecutionMode = "approval"
	}
	if policy.CoverageMode == "" {
		policy.CoverageMode = "threshold"
	}
	if policy.MinimumCoverage == "" {
		policy.MinimumCoverage = "0.8"
	}
}

func validatePolicyCoverage(policy domain.PricingPolicy) error {
	switch policy.SourceBasis {
	case "group_ratio", "equivalent_cost", "group_model_contract":
	default:
		return fmt.Errorf("source_basis must be group_ratio, equivalent_cost, or group_model_contract")
	}
	switch policy.CoverageMode {
	case "strict", "threshold", "owner_override":
	default:
		return fmt.Errorf("coverage_mode must be strict, threshold, or owner_override")
	}
	minimum, err := decimal.NewFromString(policy.MinimumCoverage)
	if err != nil || minimum.IsNegative() || minimum.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("minimum_coverage_ratio must be between 0 and 1")
	}
	return nil
}

func (s *Store) GetPolicy(ctx context.Context, id int64) (domain.PricingPolicy, error) {
	return scanPolicy(s.db.QueryRowContext(ctx, policySelect+` WHERE id=?`, id))
}

func (s *Store) ListPolicies(ctx context.Context) ([]domain.PricingPolicy, error) {
	rows, err := s.db.QueryContext(ctx, policySelect+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.PricingPolicy, 0)
	for rows.Next() {
		item, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

const policySelect = `SELECT id,name,supplier_id,source_group,source_model_pattern,source_basis,target_site_id,target_group,rule_type,value,
	profit_basis,revenue_cash_factor,cost_cash_factor,risk_buffer_rate,allow_negative_profit,subsidy_budget_daily,minimum_ratio,
	maximum_ratio,rounding_step,only_follow_increase,only_follow_decrease,enabled,execution_mode,coverage_mode,
	minimum_coverage_ratio,created_at,updated_at FROM pricing_policies`

func scanPolicy(row scanner) (domain.PricingPolicy, error) {
	var item domain.PricingPolicy
	var allowNegative, onlyIncrease, onlyDecrease, enabled int
	var created, updated string
	err := row.Scan(&item.ID, &item.Name, &item.SupplierID, &item.SourceGroup, &item.SourceModelPattern, &item.SourceBasis,
		&item.TargetSiteID, &item.TargetGroup, &item.RuleType, &item.Value, &item.ProfitBasis, &item.RevenueCashFactor,
		&item.CostCashFactor, &item.RiskBufferRate, &allowNegative, &item.SubsidyBudgetDaily, &item.MinimumRatio,
		&item.MaximumRatio, &item.RoundingStep, &onlyIncrease, &onlyDecrease, &enabled, &item.ExecutionMode,
		&item.CoverageMode, &item.MinimumCoverage, &created, &updated)
	if err != nil {
		return domain.PricingPolicy{}, err
	}
	item.AllowNegativeProfit = allowNegative != 0
	item.OnlyFollowIncrease = onlyIncrease != 0
	item.OnlyFollowDecrease = onlyDecrease != 0
	item.Enabled = enabled != 0
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) CreateProposal(ctx context.Context, proposal *domain.ChangeProposal) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO change_proposals
		(policy_id,target_site_id,target_group,expected_old,proposed_value,expected_hash,calculation_json,
		group_model_pricing_plan_json,group_model_pricing_receipt_json,status,mode,
		idempotency_key,readback_value,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, proposal.PolicyID,
		proposal.TargetSiteID, proposal.TargetGroup, proposal.ExpectedOld, proposal.ProposedValue, proposal.ExpectedHash,
		mustJSON(proposal.Calculation), mustJSON(proposal.GroupModelPricing), mustJSON(proposal.GroupModelPricingReceipt),
		proposal.Status, proposal.Mode, proposal.IdempotencyKey, proposal.ReadbackValue,
		proposal.Error, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("create proposal: %w", err)
	}
	proposal.ID, _ = result.LastInsertId()
	proposal.CreatedAt = now
	proposal.UpdatedAt = now
	return nil
}

func (s *Store) GetProposal(ctx context.Context, id int64) (domain.ChangeProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, proposalSelect+` WHERE id=?`, id))
}

func (s *Store) GetProposalByIdempotency(ctx context.Context, key string) (domain.ChangeProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, proposalSelect+` WHERE idempotency_key=?`, key))
}

func (s *Store) GetActiveProposalBySemanticKey(ctx context.Context, key string) (domain.ChangeProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, proposalSelect+
		` WHERE idempotency_key LIKE ? AND status IN ('pending','no_change','applying') ORDER BY id DESC LIMIT 1`, key+":%"))
}

func (s *Store) GetLatestNoChangeProposal(ctx context.Context, policyID int64) (domain.ChangeProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, proposalSelect+
		` WHERE policy_id=? AND status='no_change' ORDER BY id DESC LIMIT 1`, policyID))
}

func (s *Store) RefreshNoChangeProposal(ctx context.Context, id int64, proposal domain.ChangeProposal) (domain.ChangeProposal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE change_proposals SET target_site_id=?,target_group=?,expected_old=?,proposed_value=?,
		expected_hash=?,calculation_json=?,group_model_pricing_plan_json=?,group_model_pricing_receipt_json=?,
		mode=?,idempotency_key=?,readback_value='',error='',updated_at=?
		WHERE id=? AND status='no_change'`, proposal.TargetSiteID, proposal.TargetGroup, proposal.ExpectedOld,
		proposal.ProposedValue, proposal.ExpectedHash, mustJSON(proposal.Calculation), mustJSON(proposal.GroupModelPricing),
		mustJSON(proposal.GroupModelPricingReceipt), proposal.Mode, proposal.IdempotencyKey,
		formatTime(time.Now().UTC()), id)
	if err != nil {
		return domain.ChangeProposal{}, fmt.Errorf("refresh no-change proposal: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	if count == 0 {
		return domain.ChangeProposal{}, sql.ErrNoRows
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM change_proposals WHERE policy_id=? AND status='no_change'`, proposal.PolicyID)
	if err != nil {
		return domain.ChangeProposal{}, fmt.Errorf("list no-change proposals for compaction: %w", err)
	}
	var noChangeIDs []int64
	for rows.Next() {
		var proposalID int64
		if err := rows.Scan(&proposalID); err != nil {
			rows.Close()
			return domain.ChangeProposal{}, err
		}
		noChangeIDs = append(noChangeIDs, proposalID)
	}
	if err := rows.Close(); err != nil {
		return domain.ChangeProposal{}, err
	}
	for _, proposalID := range noChangeIDs {
		metadataPattern := `%"proposal_id":"` + fmt.Sprint(proposalID) + `"%`
		if _, err := tx.ExecContext(ctx, `DELETE FROM activities WHERE kind='proposal_created' AND metadata_json LIKE ?`, metadataPattern); err != nil {
			return domain.ChangeProposal{}, fmt.Errorf("compact no-change proposal activity: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM change_proposals WHERE policy_id=? AND status='no_change' AND id<>?`, proposal.PolicyID, id); err != nil {
		return domain.ChangeProposal{}, fmt.Errorf("compact no-change proposals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.ChangeProposal{}, err
	}
	return s.GetProposal(ctx, id)
}

func (s *Store) ListProposals(ctx context.Context, status string, limit int) ([]domain.ChangeProposal, error) {
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	query := proposalSelect
	args := make([]any, 0, 2)
	if status != "" {
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
	result := make([]domain.ChangeProposal, 0)
	for rows.Next() {
		item, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) UpdateProposalResult(ctx context.Context, id int64, status, readback, message string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE change_proposals SET status=?,readback_value=?,error=?,updated_at=? WHERE id=?`,
		status, readback, message, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) TransitionProposal(ctx context.Context, id int64, from []string, status, readback, message string) error {
	if len(from) == 0 {
		return errors.New("proposal transition requires at least one source state")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	query := `UPDATE change_proposals SET status=?,readback_value=?,error=?,updated_at=? WHERE id=? AND status IN (` + placeholders + `)`
	args := make([]any, 0, 5+len(from))
	args = append(args, status, readback, message, formatTime(time.Now().UTC()), id)
	for _, state := range from {
		args = append(args, state)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return fmt.Errorf("proposal %d state changed concurrently", id)
	}
	return nil
}

const proposalSelect = `SELECT id,policy_id,target_site_id,target_group,expected_old,proposed_value,expected_hash,calculation_json,
	group_model_pricing_plan_json,group_model_pricing_receipt_json,status,mode,idempotency_key,readback_value,error,created_at,updated_at FROM change_proposals`

func scanProposal(row scanner) (domain.ChangeProposal, error) {
	var item domain.ChangeProposal
	var calculation, groupModelPricingPlan, groupModelPricingReceipt, created, updated string
	err := row.Scan(&item.ID, &item.PolicyID, &item.TargetSiteID, &item.TargetGroup, &item.ExpectedOld,
		&item.ProposedValue, &item.ExpectedHash, &calculation, &groupModelPricingPlan, &groupModelPricingReceipt,
		&item.Status, &item.Mode, &item.IdempotencyKey,
		&item.ReadbackValue, &item.Error, &created, &updated)
	if err != nil {
		return domain.ChangeProposal{}, err
	}
	if err := json.Unmarshal([]byte(calculation), &item.Calculation); err != nil {
		return domain.ChangeProposal{}, err
	}
	if err := json.Unmarshal([]byte(groupModelPricingPlan), &item.GroupModelPricing); err != nil {
		return domain.ChangeProposal{}, err
	}
	if err := json.Unmarshal([]byte(groupModelPricingReceipt), &item.GroupModelPricingReceipt); err != nil {
		return domain.ChangeProposal{}, err
	}
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	return item, nil
}

func (s *Store) UpdateProposalGroupModelPricingReceipt(ctx context.Context, id int64,
	receipt domain.ListingGroupModelPricingReceipt, from []string) error {
	if len(from) == 0 {
		return errors.New("proposal receipt update requires at least one source state")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	query := `UPDATE change_proposals SET group_model_pricing_receipt_json=?,updated_at=? WHERE id=? AND status IN (` + placeholders + `)`
	args := []any{mustJSON(receipt), formatTime(time.Now().UTC()), id}
	for _, state := range from {
		args = append(args, state)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("proposal %d state changed while saving group-model pricing receipt", id)
	}
	return nil
}

func (s *Store) CreateConnectorSpec(ctx context.Context, spec *domain.ConnectorSpec) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO connector_specs(name,schema_version,spec_json,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?)`, spec.Name, spec.SchemaVersion, spec.SpecJSON, boolInt(spec.Enabled), formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	spec.ID, _ = result.LastInsertId()
	spec.CreatedAt = now
	spec.UpdatedAt = now
	return nil
}

func (s *Store) ListConnectorSpecs(ctx context.Context) ([]domain.ConnectorSpec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,schema_version,spec_json,enabled,created_at,updated_at FROM connector_specs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ConnectorSpec, 0)
	for rows.Next() {
		item, err := scanConnectorSpec(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SavePairingToken(ctx context.Context, tokenHash string, supplierID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pairing_tokens(token_hash,supplier_id,expires_at) VALUES(?,?,?)`,
		tokenHash, supplierID, formatTime(expiresAt))
	return err
}

func (s *Store) PendingPairingSupplier(ctx context.Context, tokenHash string, now time.Time) (int64, error) {
	var supplierID int64
	var expires, status string
	var used sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT supplier_id,expires_at,used_at,status FROM pairing_tokens WHERE token_hash=?`, tokenHash).
		Scan(&supplierID, &expires, &used, &status); err != nil {
		return 0, err
	}
	if used.Valid {
		return 0, fmt.Errorf("pairing token has already been used")
	}
	if !parseTime(expires).After(now) {
		return 0, fmt.Errorf("pairing token has expired")
	}
	if status != "pending" {
		return 0, fmt.Errorf("pairing token is no longer pending")
	}
	return supplierID, nil
}

func (s *Store) PairingStatus(ctx context.Context, tokenHash string, now time.Time) (string, int64, *time.Time, error) {
	var supplierID int64
	var expires, status string
	var used sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT supplier_id,expires_at,used_at,status FROM pairing_tokens WHERE token_hash=?`, tokenHash).
		Scan(&supplierID, &expires, &used, &status); err != nil {
		return "", 0, nil, err
	}
	if used.Valid {
		value := parseTime(used.String)
		return "completed", supplierID, &value, nil
	}
	if !parseTime(expires).After(now) {
		return "expired", supplierID, nil, nil
	}
	if status == "failed" {
		return "failed", supplierID, nil, nil
	}
	return "pending", supplierID, nil, nil
}

// CreateCredentialFromPairing binds the encrypted credential, supplier and
// one-time token in one transaction. A malformed or failed pairing therefore
// cannot burn the token or leave an orphan credential behind.
func (s *Store) CreateCredentialFromPairing(ctx context.Context, tokenHash string, now time.Time, expectedSupplierID int64, credential *domain.Credential, encryptedPayload string) error {
	return s.createCredentialFromPairing(ctx, tokenHash, now, expectedSupplierID, credential, encryptedPayload, "", time.Time{})
}

// CreateCredentialFromPairingWithCustody consumes the one-time pairing token,
// binds the verified credential and promotes that same high-entropy token to a
// narrowly scoped browser-custody capability in one transaction.
func (s *Store) CreateCredentialFromPairingWithCustody(ctx context.Context, tokenHash string, now time.Time,
	expectedSupplierID int64, credential *domain.Credential, encryptedPayload, expectedOrigin string,
	custodyExpiresAt time.Time) error {
	return s.createCredentialFromPairing(ctx, tokenHash, now, expectedSupplierID, credential, encryptedPayload,
		expectedOrigin, custodyExpiresAt)
}

func (s *Store) createCredentialFromPairing(ctx context.Context, tokenHash string, now time.Time,
	expectedSupplierID int64, credential *domain.Credential, encryptedPayload, expectedOrigin string,
	custodyExpiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var supplierID int64
	var expires, status string
	var used sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT supplier_id,expires_at,used_at,status FROM pairing_tokens WHERE token_hash=?`, tokenHash).
		Scan(&supplierID, &expires, &used, &status); err != nil {
		return err
	}
	if used.Valid {
		return fmt.Errorf("pairing token has already been used")
	}
	if !parseTime(expires).After(now) {
		return fmt.Errorf("pairing token has expired")
	}
	if status != "pending" {
		return fmt.Errorf("pairing token is no longer pending")
	}
	if supplierID != expectedSupplierID {
		return fmt.Errorf("pairing token supplier changed during binding")
	}
	var previousCredentialID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT credential_id FROM suppliers WHERE id=?`, supplierID).Scan(&previousCredentialID); err != nil {
		return err
	}
	createdAt := now.UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO credentials(name,kind,site_url,encrypted_payload,expires_at,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`, credential.Name, credential.Kind, credential.SiteURL, encryptedPayload, nullableTime(credential.ExpiresAt),
		"active", formatTime(createdAt), formatTime(createdAt))
	if err != nil {
		return fmt.Errorf("create paired credential: %w", err)
	}
	credentialID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read paired credential id: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE suppliers SET credential_id=?,updated_at=? WHERE id=?`, credentialID, formatTime(createdAt), supplierID)
	if err != nil {
		return fmt.Errorf("bind paired supplier: %w", err)
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	result, err = tx.ExecContext(ctx, `UPDATE pairing_tokens SET used_at=?,status='completed' WHERE token_hash=? AND used_at IS NULL AND status='pending'`, formatTime(createdAt), tokenHash)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return fmt.Errorf("pairing token was consumed concurrently")
	}
	if expectedOrigin != "" {
		if err := upsertBrowserCustodyTx(ctx, tx, tokenHash, supplierID, credentialID, expectedOrigin,
			custodyExpiresAt, createdAt); err != nil {
			return err
		}
	}
	if previousCredentialID.Valid && previousCredentialID.Int64 != credentialID {
		if _, err := tx.ExecContext(ctx, `DELETE FROM credentials WHERE id=?
			AND NOT EXISTS(SELECT 1 FROM suppliers WHERE credential_id=credentials.id)
			AND NOT EXISTS(SELECT 1 FROM target_sites WHERE credential_id=credentials.id)`, previousCredentialID.Int64); err != nil {
			return fmt.Errorf("remove unreferenced previous credential: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	credential.ID = credentialID
	credential.Status = "active"
	credential.CreatedAt = createdAt
	credential.UpdatedAt = createdAt
	return nil
}

func (s *Store) UpsertDiscoveries(ctx context.Context, snapshot domain.CatalogSnapshot, baseline bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertDiscoveriesTx(ctx, tx, snapshot, baseline); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) upsertDiscoveriesTx(ctx context.Context, tx *sql.Tx, snapshot domain.CatalogSnapshot, baseline bool) error {
	now := snapshot.Evidence.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, group := range snapshot.Groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO discoveries
			(supplier_id,kind,external_id,name,ratio,model_count,payload_json,first_seen_at,last_seen_at,is_baseline)
			VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(supplier_id,kind,external_id) DO UPDATE SET
			name=excluded.name,ratio=excluded.ratio,model_count=excluded.model_count,payload_json=excluded.payload_json,last_seen_at=excluded.last_seen_at`,
			snapshot.SupplierID, "group", group.ExternalID, group.Name, group.Ratio, len(group.ModelIDs), mustJSON(group),
			formatTime(now), formatTime(now), boolInt(baseline)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListDiscoveries(ctx context.Context, onlyUnacknowledged bool, limit int) ([]domain.DiscoveryItem, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT d.supplier_id,s.name,d.kind,d.external_id,d.name,d.ratio,d.model_count,d.first_seen_at,d.last_seen_at,d.acknowledged_at,d.is_baseline
		FROM discoveries d JOIN suppliers s ON s.id=d.supplier_id`
	if onlyUnacknowledged {
		query += ` WHERE d.acknowledged_at IS NULL`
	}
	query += ` ORDER BY d.first_seen_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.DiscoveryItem, 0)
	for rows.Next() {
		var item domain.DiscoveryItem
		var first, last string
		var acknowledged sql.NullString
		var baseline int
		if err := rows.Scan(&item.SupplierID, &item.SupplierName, &item.Kind, &item.ExternalID, &item.Name, &item.Ratio,
			&item.ModelCount, &first, &last, &acknowledged, &baseline); err != nil {
			return nil, err
		}
		item.FirstSeenAt = parseTime(first)
		item.LastSeenAt = parseTime(last)
		if acknowledged.Valid {
			value := parseTime(acknowledged.String)
			item.AcknowledgedAt = &value
		}
		item.Baseline = baseline != 0
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) AcknowledgeDiscovery(ctx context.Context, supplierID int64, kind, externalID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE discoveries SET acknowledged_at=? WHERE supplier_id=? AND kind=? AND external_id=?`,
		formatTime(time.Now().UTC()), supplierID, kind, externalID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) LatestBalance(ctx context.Context, supplierID int64) (domain.BalanceState, error) {
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM balance_observations WHERE supplier_id=? ORDER BY id DESC LIMIT 1`, supplierID).
		Scan(&payload); err != nil {
		return domain.BalanceState{}, err
	}
	var balance domain.BalanceState
	if err := json.Unmarshal([]byte(payload), &balance); err != nil {
		return domain.BalanceState{}, err
	}
	return balance, nil
}

func (s *Store) BalanceHistory(ctx context.Context, supplierID int64, limit int) ([]domain.BalanceState, error) {
	if limit <= 0 || limit > 500 {
		limit = 30
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM balance_observations WHERE supplier_id=? ORDER BY id DESC LIMIT ?`, supplierID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.BalanceState, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var item domain.BalanceState
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return serializedWrite(ctx, s, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value_json,updated_at) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json,updated_at=excluded.updated_at`,
			key, string(encoded), formatTime(time.Now().UTC()))
		return err
	})
}

func (s *Store) GetSetting(ctx context.Context, key string, target any) error {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT value_json FROM settings WHERE key=?`, key).Scan(&encoded); err != nil {
		return err
	}
	return json.Unmarshal([]byte(encoded), target)
}

func (s *Store) CountToday(ctx context.Context, table string, condition string, args ...any) (int, error) {
	allowed := map[string]bool{"catalog_snapshots": true, "change_proposals": true}
	if !allowed[table] {
		return 0, fmt.Errorf("unsupported count table")
	}
	query := `SELECT COUNT(*) FROM ` + table + ` WHERE created_at>=?`
	if table == "catalog_snapshots" {
		query = `SELECT COUNT(*) FROM catalog_snapshots WHERE observed_at>=?`
	}
	values := []any{formatTime(time.Now().UTC().Truncate(24 * time.Hour))}
	if strings.TrimSpace(condition) != "" {
		query += ` AND ` + condition
		values = append(values, args...)
	}
	var count int
	return count, s.db.QueryRowContext(ctx, query, values...).Scan(&count)
}
