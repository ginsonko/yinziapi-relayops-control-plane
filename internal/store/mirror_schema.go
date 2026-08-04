package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

const (
	mirrorSchemaMigrationVersion = 2026072801
	mirrorLeaseDimension         = "mirror"
	mirrorPolicyIntegrityMaxRows = 10_000
	mirrorPolicyPayloadMaxBytes  = 64 << 10
	mirrorPolicyPayloadTotalMax  = 64 << 20
	mirrorPolicySQLTextTotalMax  = 12 << 20
)

var (
	ErrMirrorPolicyVersionConflict = errors.New("mirror policy version conflict")
	ErrMirrorOwnershipConflict     = errors.New("mirror ownership conflict")
	ErrMirrorIdempotencyConflict   = errors.New("mirror operation identity conflict")
	ErrMirrorInvalidTransition     = errors.New("invalid mirror state transition")

	legacyMirrorPolicyNamespace = uuid.MustParse("fa7819cf-1f87-4dc1-9db5-9fc00af82b5b")
)

// MirrorChannelOwnership is the exclusive authority over one target channel's
// managed model set. Historical rows remain after release.
type MirrorChannelOwnership struct {
	ID                        int64      `json:"id"`
	PolicyUUID                string     `json:"policy_uuid"`
	TargetSiteID              int64      `json:"target_site_id"`
	TargetGroup               string     `json:"target_group"`
	ChannelID                 int64      `json:"channel_id"`
	OwnershipMarker           string     `json:"ownership_marker"`
	SourceIdentity            string     `json:"source_identity"`
	ModelSetAuthority         string     `json:"model_set_authority"`
	StatusAuthority           string     `json:"status_authority"`
	LastAppliedFunctionalHash string     `json:"last_applied_functional_hash"`
	Active                    bool       `json:"active"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	ReleasedAt                *time.Time `json:"released_at,omitempty"`
}

// MirrorModelOwnership is authority over one target group-model contract item.
type MirrorModelOwnership struct {
	ID                        int64      `json:"id"`
	PolicyUUID                string     `json:"policy_uuid"`
	TargetSiteID              int64      `json:"target_site_id"`
	TargetGroup               string     `json:"target_group"`
	TargetModel               string     `json:"target_model"`
	OwnershipMarker           string     `json:"ownership_marker"`
	SourceIdentity            string     `json:"source_identity"`
	LastAppliedFunctionalHash string     `json:"last_applied_functional_hash"`
	Active                    bool       `json:"active"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	ReleasedAt                *time.Time `json:"released_at,omitempty"`
}

// MirrorChannelModelOwnership is authority over one model membership in one
// target channel. It is intentionally distinct from contract-item ownership.
type MirrorChannelModelOwnership struct {
	ID                        int64      `json:"id"`
	PolicyUUID                string     `json:"policy_uuid"`
	TargetSiteID              int64      `json:"target_site_id"`
	TargetGroup               string     `json:"target_group"`
	ChannelID                 int64      `json:"channel_id"`
	TargetModel               string     `json:"target_model"`
	OwnershipMarker           string     `json:"ownership_marker"`
	SourceIdentity            string     `json:"source_identity"`
	LastAppliedFunctionalHash string     `json:"last_applied_functional_hash"`
	Active                    bool       `json:"active"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`
	ReleasedAt                *time.Time `json:"released_at,omitempty"`
}

type MirrorChangeSet struct {
	ID                       int64              `json:"id"`
	OperationKey             string             `json:"operation_key"`
	RequestHash              string             `json:"request_hash"`
	PolicyUUID               string             `json:"policy_uuid"`
	PolicyVersion            int64              `json:"policy_version"`
	TargetSiteID             int64              `json:"target_site_id"`
	SourceProofJSON          string             `json:"-"`
	SourceSemanticHash       string             `json:"source_semantic_hash"`
	BeforeOwnedJSON          string             `json:"-"`
	DesiredOwnedJSON         string             `json:"-"`
	ExpectedContractRevision string             `json:"expected_contract_revision"`
	DesiredContractRevision  string             `json:"desired_contract_revision"`
	Status                   string             `json:"status"`
	LeaseOwnerID             string             `json:"lease_owner_id"`
	FencingToken             int64              `json:"fencing_token"`
	Error                    string             `json:"error,omitempty"`
	CreatedAt                time.Time          `json:"created_at"`
	UpdatedAt                time.Time          `json:"updated_at"`
	FinishedAt               *time.Time         `json:"finished_at,omitempty"`
	Steps                    []MirrorChangeStep `json:"steps,omitempty"`
}

type MirrorChangeStep struct {
	ID                int64      `json:"id"`
	ChangeSetID       int64      `json:"change_set_id"`
	StepKey           string     `json:"step_key"`
	Phase             string     `json:"phase"`
	ResourceKind      string     `json:"resource_kind"`
	ResourceID        string     `json:"resource_id"`
	Action            string     `json:"action"`
	RequestID         string     `json:"request_id"`
	ExpectedHash      string     `json:"expected_hash"`
	DesiredHash       string     `json:"desired_hash"`
	BeforeJSON        string     `json:"-"`
	DesiredJSON       string     `json:"-"`
	IntentJSON        string     `json:"-"`
	ResultJSON        string     `json:"-"`
	ReadbackJSON      string     `json:"-"`
	ReadbackHash      string     `json:"readback_hash,omitempty"`
	State             string     `json:"state"`
	Attempts          int        `json:"attempts"`
	FencingToken      int64      `json:"fencing_token"`
	Error             string     `json:"error,omitempty"`
	CompensationState string     `json:"compensation_state,omitempty"`
	CompensationJSON  string     `json:"-"`
	IntentAt          *time.Time `json:"intent_at,omitempty"`
	ResultAt          *time.Time `json:"result_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type MirrorChangeEvent struct {
	ID           int64     `json:"id"`
	ChangeSetID  int64     `json:"change_set_id"`
	StepID       *int64    `json:"step_id,omitempty"`
	EventType    string    `json:"event_type"`
	Phase        string    `json:"phase"`
	FencingToken int64     `json:"fencing_token"`
	PayloadJSON  string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) migrateMirrorSchemaV3(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mirror schema-v3 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range mirrorSchemaV3Statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply mirror schema-v3: %w", err)
		}
	}

	var alreadyApplied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, mirrorSchemaMigrationVersion).Scan(&alreadyApplied); err != nil {
		return fmt.Errorf("read mirror migration journal: %w", err)
	}
	if alreadyApplied == 0 {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE change_proposals SET status='stale',
			error=CASE WHEN error='' THEN 'superseded by schema-v3 mirror policy migration' ELSE error END,
			updated_at=? WHERE status='pending'`, formatTime(now)); err != nil {
			return fmt.Errorf("stale legacy pending proposals: %w", err)
		}
		if err := syncLegacyMirrorPoliciesTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`,
			mirrorSchemaMigrationVersion, formatTime(now)); err != nil {
			return fmt.Errorf("journal mirror schema-v3 migration: %w", err)
		}
	} else if err := syncLegacyMirrorPoliciesTx(ctx, tx); err != nil {
		// Later opens import only legacy rows that do not already have a mirror
		// policy. Adopted/native policies are never rewritten.
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mirror schema-v3 migration: %w", err)
	}
	return nil
}

var mirrorSchemaV3Statements = []string{
	`CREATE TABLE IF NOT EXISTS mirror_policies (
		policy_uuid TEXT PRIMARY KEY,
		policy_version INTEGER NOT NULL,
		schema_version INTEGER NOT NULL,
		legacy_policy_id INTEGER UNIQUE,
		origin TEXT NOT NULL,
		supplier_id INTEGER NOT NULL,
		source_group TEXT NOT NULL,
		target_site_id INTEGER NOT NULL,
		target_group TEXT NOT NULL,
		policy_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK(schema_version=3),
		CHECK(policy_version>=1),
		CHECK(origin IN ('native','legacy_compat'))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mirror_policies_binding
		ON mirror_policies(target_site_id,target_group,supplier_id,source_group)`,
	`CREATE TABLE IF NOT EXISTS mirror_policy_deletions (
		policy_uuid TEXT PRIMARY KEY REFERENCES mirror_policies(policy_uuid),
		deleted_version INTEGER NOT NULL,
		deleted_at TEXT NOT NULL,
		CHECK(deleted_version>=1)
	)`,
	`CREATE TABLE IF NOT EXISTS pricing_policy_channel_ownership (
		id INTEGER PRIMARY KEY,
		policy_uuid TEXT NOT NULL REFERENCES mirror_policies(policy_uuid),
		target_site_id INTEGER NOT NULL,
		target_group TEXT NOT NULL,
		channel_id INTEGER NOT NULL,
		ownership_marker TEXT NOT NULL,
		source_identity TEXT NOT NULL,
		model_set_authority TEXT NOT NULL,
		status_authority TEXT NOT NULL,
		last_applied_functional_hash TEXT NOT NULL,
		active INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		released_at TEXT,
		CHECK(model_set_authority IN ('exclusive','managed_subset')),
		CHECK(status_authority IN ('none','disable_only'))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_mirror_channel_owner_active
		ON pricing_policy_channel_ownership(target_site_id,channel_id) WHERE active=1`,
	`CREATE TABLE IF NOT EXISTS pricing_policy_model_ownership (
		id INTEGER PRIMARY KEY,
		policy_uuid TEXT NOT NULL REFERENCES mirror_policies(policy_uuid),
		target_site_id INTEGER NOT NULL,
		target_group TEXT NOT NULL,
		target_model TEXT NOT NULL,
		ownership_marker TEXT NOT NULL,
		source_identity TEXT NOT NULL,
		last_applied_functional_hash TEXT NOT NULL,
		active INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		released_at TEXT
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_mirror_model_owner_active
		ON pricing_policy_model_ownership(target_site_id,target_group,target_model) WHERE active=1`,
	`CREATE TABLE IF NOT EXISTS pricing_policy_channel_model_ownership (
		id INTEGER PRIMARY KEY,
		policy_uuid TEXT NOT NULL REFERENCES mirror_policies(policy_uuid),
		target_site_id INTEGER NOT NULL,
		target_group TEXT NOT NULL,
		channel_id INTEGER NOT NULL,
		target_model TEXT NOT NULL,
		ownership_marker TEXT NOT NULL,
		source_identity TEXT NOT NULL,
		last_applied_functional_hash TEXT NOT NULL,
		active INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		released_at TEXT
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_mirror_channel_model_owner_active
		ON pricing_policy_channel_model_ownership(target_site_id,channel_id,target_model) WHERE active=1`,
	`CREATE TABLE IF NOT EXISTS mirror_change_sets (
		id INTEGER PRIMARY KEY,
		operation_key TEXT NOT NULL UNIQUE,
		request_hash TEXT NOT NULL,
		policy_uuid TEXT NOT NULL REFERENCES mirror_policies(policy_uuid),
		policy_version INTEGER NOT NULL,
		target_site_id INTEGER NOT NULL,
		source_proof_json TEXT NOT NULL,
		source_semantic_hash TEXT NOT NULL,
		before_owned_json TEXT NOT NULL,
		desired_owned_json TEXT NOT NULL,
		expected_contract_revision TEXT NOT NULL,
		desired_contract_revision TEXT NOT NULL,
		status TEXT NOT NULL,
		lease_owner_id TEXT NOT NULL,
		fencing_token INTEGER NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		finished_at TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mirror_change_recovery
		ON mirror_change_sets(status,updated_at,id)`,
	`CREATE TABLE IF NOT EXISTS mirror_change_channel_steps (
		id INTEGER PRIMARY KEY,
		change_set_id INTEGER NOT NULL REFERENCES mirror_change_sets(id),
		step_key TEXT NOT NULL,
		phase TEXT NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		action TEXT NOT NULL,
		request_id TEXT NOT NULL,
		expected_hash TEXT NOT NULL,
		desired_hash TEXT NOT NULL,
		before_json TEXT NOT NULL,
		desired_json TEXT NOT NULL,
		intent_json TEXT NOT NULL DEFAULT '{}',
		result_json TEXT NOT NULL DEFAULT '{}',
		readback_json TEXT NOT NULL DEFAULT '{}',
		readback_hash TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		fencing_token INTEGER NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		compensation_state TEXT NOT NULL DEFAULT '',
		compensation_json TEXT NOT NULL DEFAULT '{}',
		intent_at TEXT,
		result_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(change_set_id,step_key),
		UNIQUE(change_set_id,request_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mirror_steps_state
		ON mirror_change_channel_steps(change_set_id,state,id)`,
	`CREATE TABLE IF NOT EXISTS mirror_change_events (
		id INTEGER PRIMARY KEY,
		change_set_id INTEGER NOT NULL REFERENCES mirror_change_sets(id),
		step_id INTEGER REFERENCES mirror_change_channel_steps(id),
		event_type TEXT NOT NULL,
		phase TEXT NOT NULL,
		fencing_token INTEGER NOT NULL,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mirror_events_change
		ON mirror_change_events(change_set_id,id)`,
}

func syncLegacyMirrorPoliciesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, policySelect+` ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list legacy policies for schema-v3 migration: %w", err)
	}
	legacy := make([]domain.PricingPolicy, 0)
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy policy for schema-v3 migration: %w", err)
		}
		legacy = append(legacy, policy)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, old := range legacy {
		policyUUID := deterministicLegacyMirrorPolicyUUID(old)
		policy, err := mirror.MigrateLegacyPolicy(policyUUID, old)
		if err != nil {
			return fmt.Errorf("migrate legacy pricing policy %d: %w", old.ID, err)
		}
		payload, err := encodeCanonicalMirrorPolicy(policy)
		if err != nil {
			return fmt.Errorf("encode migrated policy %d: %w", old.ID, err)
		}
		createdAt := old.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Unix(0, 0).UTC()
		}
		updatedAt := old.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = createdAt
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_policies
			(policy_uuid,policy_version,schema_version,legacy_policy_id,origin,supplier_id,source_group,target_site_id,
			target_group,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(legacy_policy_id) DO NOTHING`, policy.PolicyUUID, policy.PolicyVersion, policy.SchemaVersion,
			old.ID, "legacy_compat", policy.SupplierID, policy.SourceGroup, policy.TargetSiteID, policy.TargetGroup,
			string(payload), formatTime(createdAt), formatTime(updatedAt)); err != nil {
			return fmt.Errorf("persist migrated policy %d: %w", old.ID, err)
		}
	}
	if _, err := listMirrorPolicies(ctx, tx); err != nil {
		return fmt.Errorf("verify migrated mirror policies: %w", err)
	}
	return nil
}

func deterministicLegacyMirrorPolicyUUID(policy domain.PricingPolicy) string {
	created := policy.CreatedAt.UTC().Format(time.RFC3339Nano)
	identity := fmt.Sprintf("pricing_policies/%d/%s", policy.ID, created)
	return uuid.NewSHA1(legacyMirrorPolicyNamespace, []byte(identity)).String()
}

func (s *Store) CreateMirrorPolicy(ctx context.Context, policy mirror.Policy, now time.Time) error {
	if policy.PolicyVersion != 1 || policy.LegacyPolicyID != 0 {
		return errors.New("native mirror policy must start at version 1 without a legacy identity")
	}
	if err := validateStoredMirrorPolicy(policy); err != nil {
		return err
	}
	payload, err := encodeCanonicalMirrorPolicy(policy)
	if err != nil {
		return err
	}
	now = now.UTC()
	return serializedWrite(ctx, s, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `INSERT INTO mirror_policies
			(policy_uuid,policy_version,schema_version,legacy_policy_id,origin,supplier_id,source_group,target_site_id,
			target_group,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, policy.PolicyUUID,
			policy.PolicyVersion, policy.SchemaVersion, nil, "native", policy.SupplierID, policy.SourceGroup,
			policy.TargetSiteID, policy.TargetGroup, string(payload), formatTime(now), formatTime(now))
		if err != nil {
			return fmt.Errorf("create mirror policy: %w", err)
		}
		readback, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`, policy.PolicyUUID))
		if err != nil {
			return fmt.Errorf("verify created mirror policy: %w", err)
		}
		if !reflect.DeepEqual(readback, policy) {
			return errors.New("created mirror policy readback mismatch")
		}
		return tx.Commit()
	})
}

func (s *Store) GetMirrorPolicy(ctx context.Context, policyUUID string) (mirror.Policy, error) {
	if !canonicalMirrorPolicyUUID(policyUUID) {
		return mirror.Policy{}, errors.New("mirror_policy_identity_invalid")
	}
	return s.getMirrorPolicyPoint(ctx, `policy_uuid=? AND NOT EXISTS (
		SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid)`, policyUUID)
}

func (s *Store) GetMirrorPolicyByLegacyID(ctx context.Context, legacyID int64) (mirror.Policy, error) {
	if legacyID <= 0 {
		return mirror.Policy{}, errors.New("mirror_policy_legacy_identity_invalid")
	}
	return s.getMirrorPolicyPoint(ctx, `legacy_policy_id=? AND NOT EXISTS (
		SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid)`, legacyID)
}

func (s *Store) getMirrorPolicyPoint(ctx context.Context, predicate string, argument any) (mirror.Policy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mirror.Policy{}, err
	}
	defer func() { _ = tx.Rollback() }()
	policy, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE `+predicate, argument))
	if err != nil {
		return mirror.Policy{}, err
	}
	if err := tx.Commit(); err != nil {
		return mirror.Policy{}, err
	}
	return policy, nil
}

func (s *Store) ListMirrorPolicies(ctx context.Context) ([]mirror.Policy, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	policies, err := listMirrorPolicies(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return policies, nil
}

type mirrorPolicyQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func listMirrorPolicies(ctx context.Context, queryer mirrorPolicyQueryer) ([]mirror.Policy, error) {
	var count, maximumPayloadBytes, totalPayloadBytes, totalSQLTextBytes, invalidRows int64
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(MAX(length(CAST(policy_json AS BLOB))),0),
		COALESCE(SUM(length(CAST(policy_json AS BLOB))),0),
		COALESCE(SUM(length(CAST(policy_uuid AS BLOB))+length(CAST(origin AS BLOB))+
			length(CAST(source_group AS BLOB))+length(CAST(target_group AS BLOB))),0),
		COALESCE(SUM(CASE WHEN policy_json IS NULL OR length(CAST(policy_uuid AS BLOB))!=36 OR
			length(CAST(origin AS BLOB))>13 OR length(CAST(source_group AS BLOB))>512 OR
			length(CAST(target_group AS BLOB))>512 THEN 1 ELSE 0 END),0)
		FROM mirror_policies WHERE NOT EXISTS (
			SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid)`).
		Scan(&count, &maximumPayloadBytes, &totalPayloadBytes, &totalSQLTextBytes, &invalidRows); err != nil {
		return nil, err
	}
	if count < 0 || maximumPayloadBytes < 0 || totalPayloadBytes < 0 || totalSQLTextBytes < 0 || invalidRows != 0 {
		return nil, errors.New("mirror_policy_integrity_capacity_exceeded")
	}
	if count > mirrorPolicyIntegrityMaxRows {
		return nil, errors.New("mirror_policy_integrity_capacity_exceeded")
	}
	if maximumPayloadBytes > mirrorPolicyPayloadMaxBytes {
		return nil, errors.New("mirror_policy_payload_capacity_exceeded")
	}
	if totalPayloadBytes > mirrorPolicyPayloadTotalMax {
		return nil, errors.New("mirror_policy_total_payload_capacity_exceeded")
	}
	if totalSQLTextBytes > mirrorPolicySQLTextTotalMax {
		return nil, errors.New("mirror_policy_identity_capacity_exceeded")
	}
	rows, err := queryer.QueryContext(ctx, mirrorPolicySelect+` WHERE NOT EXISTS (
		SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid) ORDER BY policy_uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]mirror.Policy, 0, int(count))
	for rows.Next() {
		policy, err := scanMirrorPolicy(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, policy)
	}
	return result, rows.Err()
}

func (s *Store) UpdateMirrorPolicy(ctx context.Context, policy mirror.Policy, expectedVersion int64, now time.Time) (mirror.Policy, error) {
	if expectedVersion < 1 || !canonicalMirrorPolicyUUID(policy.PolicyUUID) {
		return mirror.Policy{}, errors.New("mirror policy identity and expected version are required")
	}
	policy.PolicyVersion = expectedVersion + 1
	policy.SchemaVersion = mirror.PolicySchemaVersion
	if err := validateStoredMirrorPolicy(policy); err != nil {
		return mirror.Policy{}, err
	}
	payload, err := encodeCanonicalMirrorPolicy(policy)
	if err != nil {
		return mirror.Policy{}, err
	}
	return serializedWriteResult(ctx, s, func() (mirror.Policy, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return mirror.Policy{}, err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`, policy.PolicyUUID))
		if err != nil {
			return mirror.Policy{}, err
		}
		if current.PolicyVersion != expectedVersion || current.LegacyPolicyID != policy.LegacyPolicyID {
			return mirror.Policy{}, ErrMirrorPolicyVersionConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE mirror_policies SET policy_version=?,schema_version=?,supplier_id=?,
			source_group=?,target_site_id=?,target_group=?,policy_json=?,updated_at=?
			WHERE policy_uuid=? AND policy_version=?`, policy.PolicyVersion, policy.SchemaVersion, policy.SupplierID,
			policy.SourceGroup, policy.TargetSiteID, policy.TargetGroup, string(payload), formatTime(now.UTC()),
			policy.PolicyUUID, expectedVersion)
		if err != nil {
			return mirror.Policy{}, err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return mirror.Policy{}, ErrMirrorPolicyVersionConflict
		}
		readback, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`, policy.PolicyUUID))
		if err != nil {
			return mirror.Policy{}, fmt.Errorf("verify updated mirror policy: %w", err)
		}
		if !reflect.DeepEqual(readback, policy) {
			return mirror.Policy{}, errors.New("updated mirror policy readback mismatch")
		}
		if err := tx.Commit(); err != nil {
			return mirror.Policy{}, err
		}
		return policy, nil
	})
}

func (s *Store) DeleteMirrorPolicy(ctx context.Context, policyUUID string, expectedVersion int64, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.deleteMirrorPolicy(ctx, policyUUID, expectedVersion, now)
	})
}

func (s *Store) deleteMirrorPolicy(ctx context.Context, policyUUID string, expectedVersion int64, now time.Time) error {
	if !canonicalMirrorPolicyUUID(policyUUID) || expectedVersion < 1 {
		return errors.New("mirror policy identity and expected version are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=? AND NOT EXISTS (
		SELECT 1 FROM mirror_policy_deletions d WHERE d.policy_uuid=mirror_policies.policy_uuid)`, policyUUID))
	if err != nil {
		return err
	}
	if current.PolicyVersion != expectedVersion {
		return ErrMirrorPolicyVersionConflict
	}
	if current.LegacyPolicyID != 0 || current.Kind != mirror.PolicyKindCompleteMirror {
		return errors.New("only native complete-mirror policies can be deleted here")
	}
	var unfinished int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_change_sets WHERE policy_uuid=?
		AND status NOT IN ('applied','compensated')`, policyUUID).Scan(&unfinished); err != nil {
		return err
	}
	if unfinished != 0 {
		return errors.New("mirror policy has an unfinished synchronization")
	}
	now = now.UTC()
	for _, table := range []string{
		"pricing_policy_channel_ownership", "pricing_policy_model_ownership", "pricing_policy_channel_model_ownership",
	} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET active=0,updated_at=?,released_at=?
			WHERE policy_uuid=? AND active=1`, formatTime(now), formatTime(now), policyUUID); err != nil {
			return fmt.Errorf("release mirror ownership: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mirror_policy_deletions(policy_uuid,deleted_version,deleted_at)
		VALUES(?,?,?)`, policyUUID, expectedVersion, formatTime(now)); err != nil {
		return fmt.Errorf("delete mirror policy: %w", err)
	}
	return tx.Commit()
}

func validateStoredMirrorPolicy(policy mirror.Policy) error {
	err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{})
	if err != nil && !errors.Is(err, mirror.ErrCapabilityRequired) {
		return err
	}
	return nil
}

const mirrorPolicySelect = `SELECT
	CASE WHEN length(CAST(policy_uuid AS BLOB))=36 THEN policy_uuid ELSE NULL END,
	policy_version,schema_version,legacy_policy_id,
	CASE WHEN length(CAST(origin AS BLOB))<=13 THEN origin ELSE NULL END,supplier_id,
	CASE WHEN length(CAST(source_group AS BLOB))<=512 THEN source_group ELSE NULL END,target_site_id,
	CASE WHEN length(CAST(target_group AS BLOB))<=512 THEN target_group ELSE NULL END,
	CASE WHEN length(CAST(policy_json AS BLOB))<=65536 THEN policy_json ELSE NULL END FROM mirror_policies`

func scanMirrorPolicy(row scanner) (mirror.Policy, error) {
	var policy mirror.Policy
	var policyUUID, origin, sourceGroup, targetGroup sql.NullString
	var policyVersion int64
	var schemaVersion int
	var legacyID sql.NullInt64
	var supplierID, targetSiteID int64
	var payload sql.NullString
	if err := row.Scan(&policyUUID, &policyVersion, &schemaVersion, &legacyID, &origin, &supplierID,
		&sourceGroup, &targetSiteID, &targetGroup, &payload); err != nil {
		return mirror.Policy{}, err
	}
	if !policyUUID.Valid || !origin.Valid || !sourceGroup.Valid || !targetGroup.Valid {
		return mirror.Policy{}, errors.New("mirror_policy_row_identity_capacity_exceeded")
	}
	if !payload.Valid || len(payload.String) == 0 || len(payload.String) > mirrorPolicyPayloadMaxBytes {
		return mirror.Policy{}, errors.New("mirror_policy_payload_capacity_exceeded")
	}
	policy, err := decodeCanonicalMirrorPolicy([]byte(payload.String))
	if err != nil {
		if policy.SchemaVersion != 0 && policy.SchemaVersion != schemaVersion {
			return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "schema_version")
		}
		return mirror.Policy{}, errors.New("mirror_policy_payload_invalid")
	}
	if !canonicalMirrorPolicyUUID(policyUUID.String) || policy.PolicyUUID != policyUUID.String {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "policy_uuid")
	}
	if policy.PolicyVersion != policyVersion {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "policy_version")
	}
	if policy.SchemaVersion != schemaVersion {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "schema_version")
	}
	if (legacyID.Valid && policy.LegacyPolicyID != legacyID.Int64) || (!legacyID.Valid && policy.LegacyPolicyID != 0) {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "legacy_policy_id")
	}
	expectedOrigin := "native"
	if legacyID.Valid {
		expectedOrigin = "legacy_compat"
	}
	if origin.String != expectedOrigin {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "origin")
	}
	if policy.SupplierID != supplierID {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "supplier_id")
	}
	if !canonicalMirrorPolicyBinding(sourceGroup.String) || policy.SourceGroup != sourceGroup.String {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "source_group")
	}
	if policy.TargetSiteID != targetSiteID {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "target_site_id")
	}
	if !canonicalMirrorPolicyBinding(targetGroup.String) || policy.TargetGroup != targetGroup.String {
		return mirror.Policy{}, mirrorPolicyStorageIdentityError(policyUUID.String, "target_group")
	}
	if err := validateStoredMirrorPolicy(policy); err != nil {
		return mirror.Policy{}, fmt.Errorf("mirror policy %s stored policy is invalid: %w", redactedMirrorPolicyIdentity(policyUUID.String), err)
	}
	return policy, nil
}

func canonicalMirrorPolicyBinding(value string) bool {
	return value != "" && len(value) <= 512 && value == strings.TrimSpace(value) && !strings.ContainsRune(value, '\x00')
}

func mirrorPolicyStorageIdentityError(policyUUID, field string) error {
	return fmt.Errorf("mirror policy %s storage identity mismatch: %s", redactedMirrorPolicyIdentity(policyUUID), field)
}

func canonicalMirrorPolicyUUID(value string) bool {
	if len(value) != 36 || value != strings.TrimSpace(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func redactedMirrorPolicyIdentity(value string) string {
	if canonicalMirrorPolicyUUID(value) {
		return value
	}
	return "<invalid-policy-identity>"
}

func decodeCanonicalMirrorPolicy(payload []byte) (mirror.Policy, error) {
	if len(payload) == 0 || len(payload) > mirrorPolicyPayloadMaxBytes {
		return mirror.Policy{}, errors.New("mirror policy payload is outside the allowed size")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var policy mirror.Policy
	if err := decoder.Decode(&policy); err != nil {
		return mirror.Policy{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return mirror.Policy{}, errors.New("mirror policy payload contains trailing JSON")
		}
		return mirror.Policy{}, err
	}
	if err := validateStoredMirrorPolicy(policy); err != nil {
		return policy, err
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return mirror.Policy{}, err
	}
	if !bytes.Equal(canonical, payload) {
		return mirror.Policy{}, errors.New("mirror policy payload is not canonical")
	}
	return policy, nil
}

func encodeCanonicalMirrorPolicy(policy mirror.Policy) ([]byte, error) {
	if err := validateStoredMirrorPolicy(policy); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > mirrorPolicyPayloadMaxBytes {
		return nil, errors.New("mirror_policy_payload_capacity_exceeded")
	}
	return payload, nil
}

func (s *Store) AcquireMirrorLease(ctx context.Context, targetID int64, ownerID string, now time.Time, ttl time.Duration) (domain.TargetControlLease, error) {
	return s.AcquireTargetControlLease(ctx, targetID, mirrorLeaseDimension, ownerID, now, ttl)
}

func expectedMirrorOwnershipMarker(policyUUID string) (string, error) {
	policyUUID = strings.TrimSpace(policyUUID)
	if _, err := uuid.Parse(policyUUID); err != nil {
		return "", errors.New("ownership requires a valid policy UUID")
	}
	return "relayops:policy/" + policyUUID, nil
}

func validateMirrorOwnership(policyUUID, marker, sourceIdentity, lastHash string, targetID int64) error {
	expected, err := expectedMirrorOwnershipMarker(policyUUID)
	if err != nil {
		return err
	}
	if marker != expected {
		return errors.New("ownership marker does not match the immutable policy UUID")
	}
	if targetID <= 0 || strings.TrimSpace(sourceIdentity) == "" || strings.TrimSpace(lastHash) == "" {
		return errors.New("ownership target, source identity and functional hash are required")
	}
	return nil
}

func (s *Store) ClaimMirrorChannelOwnership(ctx context.Context, item *MirrorChannelOwnership, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.claimMirrorChannelOwnership(ctx, item, lease, now)
	})
}

func (s *Store) claimMirrorChannelOwnership(ctx context.Context, item *MirrorChannelOwnership, lease domain.TargetControlLease, now time.Time) error {
	if item == nil || item.ChannelID <= 0 || strings.TrimSpace(item.TargetGroup) == "" {
		return errors.New("channel ownership binding is required")
	}
	if item.ModelSetAuthority != "exclusive" && item.ModelSetAuthority != "managed_subset" {
		return errors.New("invalid model-set authority")
	}
	if item.StatusAuthority != "none" && item.StatusAuthority != "disable_only" {
		return errors.New("invalid status authority")
	}
	if err := validateMirrorOwnership(item.PolicyUUID, item.OwnershipMarker, item.SourceIdentity,
		item.LastAppliedFunctionalHash, item.TargetSiteID); err != nil {
		return err
	}
	tx, err := s.beginMirrorLeaseTx(ctx, lease, item.TargetSiteID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var existing MirrorChannelOwnership
	existing, err = scanMirrorChannelOwnership(tx.QueryRowContext(ctx, mirrorChannelOwnershipSelect+
		` WHERE target_site_id=? AND channel_id=? AND active=1`, item.TargetSiteID, item.ChannelID))
	if err == nil {
		if sameMirrorChannelOwnership(existing, *item) {
			*item = existing
			return tx.Commit()
		}
		return ErrMirrorOwnershipConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now = now.UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_channel_ownership
		(policy_uuid,target_site_id,target_group,channel_id,ownership_marker,source_identity,model_set_authority,
		status_authority,last_applied_functional_hash,active,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,1,?,?)`, item.PolicyUUID, item.TargetSiteID, item.TargetGroup, item.ChannelID,
		item.OwnershipMarker, item.SourceIdentity, item.ModelSetAuthority, item.StatusAuthority,
		item.LastAppliedFunctionalHash, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrMirrorOwnershipConflict
		}
		return err
	}
	item.ID, _ = result.LastInsertId()
	item.Active = true
	item.CreatedAt, item.UpdatedAt = now, now
	return tx.Commit()
}

func (s *Store) ClaimMirrorModelOwnership(ctx context.Context, item *MirrorModelOwnership, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.claimMirrorModelOwnership(ctx, item, lease, now)
	})
}

func (s *Store) claimMirrorModelOwnership(ctx context.Context, item *MirrorModelOwnership, lease domain.TargetControlLease, now time.Time) error {
	if item == nil || strings.TrimSpace(item.TargetGroup) == "" || strings.TrimSpace(item.TargetModel) == "" {
		return errors.New("model ownership binding is required")
	}
	if err := validateMirrorOwnership(item.PolicyUUID, item.OwnershipMarker, item.SourceIdentity,
		item.LastAppliedFunctionalHash, item.TargetSiteID); err != nil {
		return err
	}
	tx, err := s.beginMirrorLeaseTx(ctx, lease, item.TargetSiteID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanMirrorModelOwnership(tx.QueryRowContext(ctx, mirrorModelOwnershipSelect+
		` WHERE target_site_id=? AND target_group=? AND target_model=? AND active=1`, item.TargetSiteID,
		item.TargetGroup, item.TargetModel))
	if err == nil {
		if sameMirrorModelOwnership(existing, *item) {
			*item = existing
			return tx.Commit()
		}
		return ErrMirrorOwnershipConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now = now.UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_model_ownership
		(policy_uuid,target_site_id,target_group,target_model,ownership_marker,source_identity,last_applied_functional_hash,
		active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?)`, item.PolicyUUID, item.TargetSiteID,
		item.TargetGroup, item.TargetModel, item.OwnershipMarker, item.SourceIdentity, item.LastAppliedFunctionalHash,
		formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrMirrorOwnershipConflict
		}
		return err
	}
	item.ID, _ = result.LastInsertId()
	item.Active = true
	item.CreatedAt, item.UpdatedAt = now, now
	return tx.Commit()
}

func (s *Store) ClaimMirrorChannelModelOwnership(ctx context.Context, item *MirrorChannelModelOwnership, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.claimMirrorChannelModelOwnership(ctx, item, lease, now)
	})
}

func (s *Store) claimMirrorChannelModelOwnership(ctx context.Context, item *MirrorChannelModelOwnership, lease domain.TargetControlLease, now time.Time) error {
	if item == nil || item.ChannelID <= 0 || strings.TrimSpace(item.TargetGroup) == "" || strings.TrimSpace(item.TargetModel) == "" {
		return errors.New("channel-model ownership binding is required")
	}
	if err := validateMirrorOwnership(item.PolicyUUID, item.OwnershipMarker, item.SourceIdentity,
		item.LastAppliedFunctionalHash, item.TargetSiteID); err != nil {
		return err
	}
	tx, err := s.beginMirrorLeaseTx(ctx, lease, item.TargetSiteID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanMirrorChannelModelOwnership(tx.QueryRowContext(ctx, mirrorChannelModelOwnershipSelect+
		` WHERE target_site_id=? AND channel_id=? AND target_model=? AND active=1`, item.TargetSiteID,
		item.ChannelID, item.TargetModel))
	if err == nil {
		if sameMirrorChannelModelOwnership(existing, *item) {
			*item = existing
			return tx.Commit()
		}
		return ErrMirrorOwnershipConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now = now.UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_channel_model_ownership
		(policy_uuid,target_site_id,target_group,channel_id,target_model,ownership_marker,source_identity,
		last_applied_functional_hash,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, item.PolicyUUID,
		item.TargetSiteID, item.TargetGroup, item.ChannelID, item.TargetModel, item.OwnershipMarker,
		item.SourceIdentity, item.LastAppliedFunctionalHash, formatTime(now), formatTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrMirrorOwnershipConflict
		}
		return err
	}
	item.ID, _ = result.LastInsertId()
	item.Active = true
	item.CreatedAt, item.UpdatedAt = now, now
	return tx.Commit()
}

func (s *Store) GetActiveMirrorChannelOwnership(ctx context.Context, targetID, channelID int64) (MirrorChannelOwnership, error) {
	return scanMirrorChannelOwnership(s.db.QueryRowContext(ctx, mirrorChannelOwnershipSelect+
		` WHERE target_site_id=? AND channel_id=? AND active=1`, targetID, channelID))
}

func (s *Store) ListActiveMirrorChannelOwnerships(ctx context.Context, policyUUID string) ([]MirrorChannelOwnership, error) {
	rows, err := s.db.QueryContext(ctx, mirrorChannelOwnershipSelect+
		` WHERE policy_uuid=? AND active=1 ORDER BY target_site_id,channel_id`, strings.TrimSpace(policyUUID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChannelOwnership, 0)
	for rows.Next() {
		item, err := scanMirrorChannelOwnership(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetActiveMirrorModelOwnership(ctx context.Context, targetID int64, targetGroup,
	targetModel string) (MirrorModelOwnership, error) {
	return scanMirrorModelOwnership(s.db.QueryRowContext(ctx, mirrorModelOwnershipSelect+
		` WHERE target_site_id=? AND target_group=? AND target_model=? AND active=1`, targetID, targetGroup, targetModel))
}

func (s *Store) GetActiveMirrorChannelModelOwnership(ctx context.Context, targetID, channelID int64,
	targetModel string) (MirrorChannelModelOwnership, error) {
	return scanMirrorChannelModelOwnership(s.db.QueryRowContext(ctx, mirrorChannelModelOwnershipSelect+
		` WHERE target_site_id=? AND channel_id=? AND target_model=? AND active=1`, targetID, channelID, targetModel))
}

func (s *Store) ListActiveMirrorModelOwnerships(ctx context.Context, policyUUID string) ([]MirrorModelOwnership, error) {
	rows, err := s.db.QueryContext(ctx, mirrorModelOwnershipSelect+
		` WHERE policy_uuid=? AND active=1 ORDER BY target_site_id,target_group,target_model`, policyUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorModelOwnership, 0)
	for rows.Next() {
		item, err := scanMirrorModelOwnership(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListActiveMirrorChannelModelOwnerships(ctx context.Context, policyUUID string) ([]MirrorChannelModelOwnership, error) {
	rows, err := s.db.QueryContext(ctx, mirrorChannelModelOwnershipSelect+
		` WHERE policy_uuid=? AND active=1 ORDER BY target_site_id,channel_id,target_model`, policyUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChannelModelOwnership, 0)
	for rows.Next() {
		item, err := scanMirrorChannelModelOwnership(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ReleaseMirrorChannelOwnership(ctx context.Context, targetID, channelID int64, policyUUID,
	marker, expectedHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.releaseMirrorOwnership(ctx, `pricing_policy_channel_ownership`, `target_site_id=? AND channel_id=?`,
		[]any{targetID, channelID}, targetID, policyUUID, marker, expectedHash, lease, now)
}

func (s *Store) ReleaseMirrorModelOwnership(ctx context.Context, targetID int64, targetGroup, targetModel,
	policyUUID, marker, expectedHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.releaseMirrorOwnership(ctx, `pricing_policy_model_ownership`,
		`target_site_id=? AND target_group=? AND target_model=?`, []any{targetID, targetGroup, targetModel},
		targetID, policyUUID, marker, expectedHash, lease, now)
}

func (s *Store) ReleaseMirrorChannelModelOwnership(ctx context.Context, targetID, channelID int64, targetModel,
	policyUUID, marker, expectedHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.releaseMirrorOwnership(ctx, `pricing_policy_channel_model_ownership`,
		`target_site_id=? AND channel_id=? AND target_model=?`, []any{targetID, channelID, targetModel},
		targetID, policyUUID, marker, expectedHash, lease, now)
}

func (s *Store) UpdateMirrorChannelOwnershipHash(ctx context.Context, targetID, channelID int64, policyUUID,
	marker, expectedHash, desiredHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.updateMirrorOwnershipHash(ctx, `pricing_policy_channel_ownership`, `target_site_id=? AND channel_id=?`,
		[]any{targetID, channelID}, targetID, policyUUID, marker, expectedHash, desiredHash, lease, now)
}

func (s *Store) UpdateMirrorModelOwnershipHash(ctx context.Context, targetID int64, targetGroup, targetModel,
	policyUUID, marker, expectedHash, desiredHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.updateMirrorOwnershipHash(ctx, `pricing_policy_model_ownership`,
		`target_site_id=? AND target_group=? AND target_model=?`, []any{targetID, targetGroup, targetModel},
		targetID, policyUUID, marker, expectedHash, desiredHash, lease, now)
}

func (s *Store) UpdateMirrorChannelModelOwnershipHash(ctx context.Context, targetID, channelID int64, targetModel,
	policyUUID, marker, expectedHash, desiredHash string, lease domain.TargetControlLease, now time.Time) error {
	return s.updateMirrorOwnershipHash(ctx, `pricing_policy_channel_model_ownership`,
		`target_site_id=? AND channel_id=? AND target_model=?`, []any{targetID, channelID, targetModel},
		targetID, policyUUID, marker, expectedHash, desiredHash, lease, now)
}

func (s *Store) updateMirrorOwnershipHash(ctx context.Context, table, keyPredicate string, keyArgs []any, targetID int64,
	policyUUID, marker, expectedHash, desiredHash string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.updateMirrorOwnershipHashOnce(ctx, table, keyPredicate, keyArgs, targetID, policyUUID, marker,
			expectedHash, desiredHash, lease, now)
	})
}

func (s *Store) updateMirrorOwnershipHashOnce(ctx context.Context, table, keyPredicate string, keyArgs []any, targetID int64,
	policyUUID, marker, expectedHash, desiredHash string, lease domain.TargetControlLease, now time.Time) error {
	if strings.TrimSpace(desiredHash) == "" || desiredHash == expectedHash {
		return errors.New("a distinct desired functional hash is required")
	}
	if err := validateMirrorOwnership(policyUUID, marker, "update", expectedHash, targetID); err != nil {
		return err
	}
	tx, err := s.beginMirrorLeaseTx(ctx, lease, targetID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE ` + table + ` SET last_applied_functional_hash=?,updated_at=? WHERE ` + keyPredicate +
		` AND policy_uuid=? AND ownership_marker=? AND last_applied_functional_hash=? AND active=1`
	args := []any{desiredHash, formatTime(now.UTC())}
	args = append(args, keyArgs...)
	args = append(args, policyUUID, marker, expectedHash)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorOwnershipConflict
	}
	return tx.Commit()
}

func (s *Store) releaseMirrorOwnership(ctx context.Context, table, keyPredicate string, keyArgs []any, targetID int64,
	policyUUID, marker, expectedHash string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.releaseMirrorOwnershipOnce(ctx, table, keyPredicate, keyArgs, targetID, policyUUID, marker,
			expectedHash, lease, now)
	})
}

func (s *Store) releaseMirrorOwnershipOnce(ctx context.Context, table, keyPredicate string, keyArgs []any, targetID int64,
	policyUUID, marker, expectedHash string, lease domain.TargetControlLease, now time.Time) error {
	if err := validateMirrorOwnership(policyUUID, marker, "release", expectedHash, targetID); err != nil {
		return err
	}
	tx, err := s.beginMirrorLeaseTx(ctx, lease, targetID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	query := `UPDATE ` + table + ` SET active=0,released_at=?,updated_at=? WHERE ` + keyPredicate +
		` AND policy_uuid=? AND ownership_marker=? AND last_applied_functional_hash=? AND active=1`
	args := []any{formatTime(now.UTC()), formatTime(now.UTC())}
	args = append(args, keyArgs...)
	args = append(args, policyUUID, marker, expectedHash)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorOwnershipConflict
	}
	return tx.Commit()
}

func (s *Store) CreateMirrorChangeSet(ctx context.Context, changeSet *MirrorChangeSet,
	lease domain.TargetControlLease, now time.Time) (bool, error) {
	return serializedWriteResult(ctx, s, func() (bool, error) {
		return s.createMirrorChangeSet(ctx, changeSet, lease, now)
	})
}

func (s *Store) createMirrorChangeSet(ctx context.Context, changeSet *MirrorChangeSet,
	lease domain.TargetControlLease, now time.Time) (bool, error) {
	if changeSet == nil || strings.TrimSpace(changeSet.OperationKey) == "" || strings.TrimSpace(changeSet.RequestHash) == "" ||
		changeSet.PolicyVersion < 1 || changeSet.TargetSiteID <= 0 || len(changeSet.Steps) == 0 {
		return false, errors.New("mirror change-set identity and steps are required")
	}
	if len(changeSet.OperationKey) > 256 || len(changeSet.RequestHash) > 256 {
		return false, errors.New("mirror change-set identity is too long")
	}
	if changeSet.Status == "" {
		changeSet.Status = "pending"
	}
	if changeSet.Status != "pending" {
		return false, errors.New("new mirror change set must be pending")
	}
	for i := range changeSet.Steps {
		if err := validateNewMirrorStep(changeSet.Steps[i]); err != nil {
			return false, fmt.Errorf("step %d: %w", i, err)
		}
	}
	var err error
	for label, value := range map[string]*string{
		"source proof":  &changeSet.SourceProofJSON,
		"before owned":  &changeSet.BeforeOwnedJSON,
		"desired owned": &changeSet.DesiredOwnedJSON,
	} {
		*value, err = normalizeMirrorJSON(*value)
		if err != nil {
			return false, fmt.Errorf("%s: %w", label, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanMirrorChangeSet(tx.QueryRowContext(ctx, mirrorChangeSetSelect+` WHERE operation_key=?`, changeSet.OperationKey))
	if err == nil {
		if existing.RequestHash != changeSet.RequestHash || existing.PolicyUUID != changeSet.PolicyUUID ||
			existing.PolicyVersion != changeSet.PolicyVersion || existing.TargetSiteID != changeSet.TargetSiteID {
			return false, ErrMirrorIdempotencyConflict
		}
		steps, err := listMirrorChangeStepsTx(ctx, tx, existing.ID)
		if err != nil {
			return false, err
		}
		existing.Steps = steps
		*changeSet = existing
		return true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err := checkMirrorLeaseTx(ctx, tx, lease, now, changeSet.TargetSiteID); err != nil {
		return false, err
	}
	var policyCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_policies WHERE policy_uuid=? AND policy_version=?`,
		changeSet.PolicyUUID, changeSet.PolicyVersion).Scan(&policyCount); err != nil {
		return false, err
	}
	if policyCount != 1 {
		return false, ErrMirrorPolicyVersionConflict
	}
	now = now.UTC()
	changeSet.LeaseOwnerID = lease.OwnerID
	changeSet.FencingToken = lease.FencingToken
	result, err := tx.ExecContext(ctx, `INSERT INTO mirror_change_sets
		(operation_key,request_hash,policy_uuid,policy_version,target_site_id,source_proof_json,source_semantic_hash,
		before_owned_json,desired_owned_json,expected_contract_revision,desired_contract_revision,status,
		lease_owner_id,fencing_token,error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, changeSet.OperationKey, changeSet.RequestHash, changeSet.PolicyUUID,
		changeSet.PolicyVersion, changeSet.TargetSiteID, changeSet.SourceProofJSON, changeSet.SourceSemanticHash,
		changeSet.BeforeOwnedJSON, changeSet.DesiredOwnedJSON, changeSet.ExpectedContractRevision,
		changeSet.DesiredContractRevision, changeSet.Status, changeSet.LeaseOwnerID, changeSet.FencingToken,
		changeSet.Error, formatTime(now), formatTime(now))
	if err != nil {
		return false, err
	}
	changeSet.ID, _ = result.LastInsertId()
	changeSet.CreatedAt, changeSet.UpdatedAt = now, now
	for i := range changeSet.Steps {
		step := &changeSet.Steps[i]
		step.ChangeSetID = changeSet.ID
		step.State = "pending"
		step.FencingToken = lease.FencingToken
		step.BeforeJSON, err = normalizeMirrorJSON(step.BeforeJSON)
		if err != nil {
			return false, err
		}
		step.DesiredJSON, err = normalizeMirrorJSON(step.DesiredJSON)
		if err != nil {
			return false, err
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO mirror_change_channel_steps
			(change_set_id,step_key,phase,resource_kind,resource_id,action,request_id,expected_hash,desired_hash,
			before_json,desired_json,state,fencing_token,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, step.ChangeSetID, step.StepKey, step.Phase, step.ResourceKind,
			step.ResourceID, step.Action, step.RequestID, step.ExpectedHash, step.DesiredHash, step.BeforeJSON,
			step.DesiredJSON, step.State, step.FencingToken, formatTime(now), formatTime(now))
		if err != nil {
			return false, err
		}
		step.ID, _ = insert.LastInsertId()
		step.CreatedAt, step.UpdatedAt = now, now
	}
	if err := appendMirrorEventTx(ctx, tx, changeSet.ID, nil, "change_set_created", "pending",
		lease.FencingToken, `{"status":"pending"}`, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) GetMirrorChangeSet(ctx context.Context, id int64) (MirrorChangeSet, error) {
	changeSet, err := scanMirrorChangeSet(s.db.QueryRowContext(ctx, mirrorChangeSetSelect+` WHERE id=?`, id))
	if err != nil {
		return MirrorChangeSet{}, err
	}
	changeSet.Steps, err = s.ListMirrorChangeSteps(ctx, id)
	return changeSet, err
}

func (s *Store) GetMirrorChangeSetByOperationKey(ctx context.Context, operationKey string) (MirrorChangeSet, error) {
	changeSet, err := scanMirrorChangeSet(s.db.QueryRowContext(ctx, mirrorChangeSetSelect+` WHERE operation_key=?`, operationKey))
	if err != nil {
		return MirrorChangeSet{}, err
	}
	changeSet.Steps, err = s.ListMirrorChangeSteps(ctx, changeSet.ID)
	return changeSet, err
}

// ListMirrorChangeSets returns both completed and non-terminal executions for
// the administrator audit surface. The filters are exact identifiers; no raw
// proof or remote request payload is exposed by MirrorChangeSet's JSON shape.
func (s *Store) ListMirrorChangeSets(ctx context.Context, policyUUID, status string, limit int) ([]MirrorChangeSet, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	policyUUID = strings.TrimSpace(policyUUID)
	status = strings.TrimSpace(status)
	query := mirrorChangeSetSelect + ` WHERE 1=1`
	arguments := make([]any, 0, 3)
	if policyUUID != "" {
		if !canonicalMirrorPolicyUUID(policyUUID) {
			return nil, errors.New("policy_uuid must be a canonical UUID")
		}
		query += ` AND policy_uuid=?`
		arguments = append(arguments, policyUUID)
	}
	if status != "" {
		if len(status) > 64 || strings.ContainsAny(status, "\x00\r\n\t ") {
			return nil, errors.New("status must be a bounded token")
		}
		query += ` AND status=?`
		arguments = append(arguments, status)
	}
	query += ` ORDER BY updated_at DESC,id DESC LIMIT ?`
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChangeSet, 0)
	for rows.Next() {
		item, err := scanMirrorChangeSet(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListRecoverableMirrorChangeSets(ctx context.Context, limit int) ([]MirrorChangeSet, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, mirrorChangeSetSelect+`
		WHERE status NOT IN ('applied','compensated') ORDER BY updated_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChangeSet, 0)
	for rows.Next() {
		item, err := scanMirrorChangeSet(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ClaimMirrorChangeSet(ctx context.Context, changeSetID int64, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.claimMirrorChangeSet(ctx, changeSetID, lease, now)
	})
}

func (s *Store) claimMirrorChangeSet(ctx context.Context, changeSetID int64, lease domain.TargetControlLease, now time.Time) error {
	tx, err := s.beginMirrorLeaseTx(ctx, lease, lease.TargetSiteID, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE mirror_change_sets SET lease_owner_id=?,fencing_token=?,updated_at=?
		WHERE id=? AND target_site_id=? AND status NOT IN ('applied','compensated')`, lease.OwnerID,
		lease.FencingToken, formatTime(now.UTC()), changeSetID, lease.TargetSiteID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorInvalidTransition
	}
	if err := appendMirrorEventTx(ctx, tx, changeSetID, nil, "lease_claimed", "recovery", lease.FencingToken,
		`{"owner_id":`+mustJSONString(lease.OwnerID)+`}`, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordMirrorStepIntent(ctx context.Context, changeSetID int64, stepKey, intentJSON string,
	lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.recordMirrorStepIntent(ctx, changeSetID, stepKey, intentJSON, lease, now)
	})
}

func (s *Store) recordMirrorStepIntent(ctx context.Context, changeSetID int64, stepKey, intentJSON string,
	lease domain.TargetControlLease, now time.Time) error {
	intentJSON, err := normalizeMirrorJSON(intentJSON)
	if err != nil {
		return err
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	step, err := scanMirrorChangeStep(tx.QueryRowContext(ctx, mirrorChangeStepSelect+`
		WHERE change_set_id=? AND step_key=?`, changeSetID, stepKey))
	if err != nil {
		return err
	}
	if step.State == "intent_recorded" && step.IntentJSON == intentJSON && step.FencingToken == lease.FencingToken {
		return tx.Commit()
	}
	if step.State != "pending" {
		return ErrMirrorInvalidTransition
	}
	result, err := tx.ExecContext(ctx, `UPDATE mirror_change_channel_steps SET state='intent_recorded',intent_json=?,
		attempts=attempts+1,fencing_token=?,intent_at=?,updated_at=? WHERE id=? AND state='pending'`, intentJSON,
		lease.FencingToken, formatTime(now.UTC()), formatTime(now.UTC()), step.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorInvalidTransition
	}
	if err := appendMirrorEventTx(ctx, tx, changeSetID, &step.ID, "intent_recorded", step.Phase,
		lease.FencingToken, intentJSON, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordMirrorStepResult(ctx context.Context, changeSetID int64, stepKey, resultJSON,
	message string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.recordMirrorStepResult(ctx, changeSetID, stepKey, resultJSON, message, lease, now)
	})
}

func (s *Store) recordMirrorStepResult(ctx context.Context, changeSetID int64, stepKey, resultJSON,
	message string, lease domain.TargetControlLease, now time.Time) error {
	resultJSON, err := normalizeMirrorJSON(resultJSON)
	if err != nil {
		return err
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	step, err := scanMirrorChangeStep(tx.QueryRowContext(ctx, mirrorChangeStepSelect+`
		WHERE change_set_id=? AND step_key=?`, changeSetID, stepKey))
	if err != nil {
		return err
	}
	if step.State == "readback_pending" && step.ResultJSON == resultJSON && step.Error == message &&
		step.FencingToken == lease.FencingToken {
		return tx.Commit()
	}
	if step.State != "intent_recorded" {
		return ErrMirrorInvalidTransition
	}
	result, err := tx.ExecContext(ctx, `UPDATE mirror_change_channel_steps SET state='readback_pending',result_json=?,
		error=?,fencing_token=?,result_at=?,updated_at=? WHERE id=? AND state='intent_recorded'`, resultJSON,
		message, lease.FencingToken, formatTime(now.UTC()), formatTime(now.UTC()), step.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorInvalidTransition
	}
	if err := appendMirrorEventTx(ctx, tx, changeSetID, &step.ID, "result_recorded", step.Phase,
		lease.FencingToken, resultJSON, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// ClassifyMirrorStepReadback is deliberately non-writing with respect to the
// remote target. It only records whether recovery may retry, is already done,
// or encountered a third state that must not be overwritten.
func (s *Store) ClassifyMirrorStepReadback(ctx context.Context, changeSetID int64, stepKey, liveHash,
	readbackJSON string, lease domain.TargetControlLease, now time.Time) (string, error) {
	return serializedWriteResult(ctx, s, func() (string, error) {
		return s.classifyMirrorStepReadback(ctx, changeSetID, stepKey, liveHash, readbackJSON, lease, now)
	})
}

func (s *Store) classifyMirrorStepReadback(ctx context.Context, changeSetID int64, stepKey, liveHash,
	readbackJSON string, lease domain.TargetControlLease, now time.Time) (string, error) {
	readbackJSON, err := normalizeMirrorJSON(readbackJSON)
	if err != nil {
		return "", err
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	step, err := scanMirrorChangeStep(tx.QueryRowContext(ctx, mirrorChangeStepSelect+`
		WHERE change_set_id=? AND step_key=?`, changeSetID, stepKey))
	if err != nil {
		return "", err
	}
	classification := "needs_reconciliation"
	state := "needs_reconciliation"
	switch liveHash {
	case step.ExpectedHash:
		classification, state = "retryable_pending", "pending"
	case step.DesiredHash:
		classification, state = "already_applied", "succeeded"
	}
	result, err := tx.ExecContext(ctx, `UPDATE mirror_change_channel_steps SET state=?,readback_hash=?,readback_json=?,
		fencing_token=?,updated_at=? WHERE id=?`, state, liveHash, readbackJSON, lease.FencingToken,
		formatTime(now.UTC()), step.ID)
	if err != nil {
		return "", err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return "", ErrMirrorInvalidTransition
	}
	if classification == "needs_reconciliation" {
		if _, err := tx.ExecContext(ctx, `UPDATE mirror_change_sets SET status='needs_reconciliation',
			error=?,updated_at=? WHERE id=?`, "remote state matched neither expected nor desired hash",
			formatTime(now.UTC()), changeSetID); err != nil {
			return "", err
		}
	}
	payload := `{"classification":` + mustJSONString(classification) + `,"live_hash":` + mustJSONString(liveHash) + `}`
	if err := appendMirrorEventTx(ctx, tx, changeSetID, &step.ID, "readback_classified", step.Phase,
		lease.FencingToken, payload, now.UTC()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return classification, nil
}

func (s *Store) TransitionMirrorChangeSet(ctx context.Context, changeSetID int64, fromStatus, toStatus string,
	lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.transitionMirrorChangeSet(ctx, changeSetID, fromStatus, toStatus, lease, now)
	})
}

func (s *Store) transitionMirrorChangeSet(ctx context.Context, changeSetID int64, fromStatus, toStatus string,
	lease domain.TargetControlLease, now time.Time) error {
	if !validMirrorChangeTransition(fromStatus, toStatus) {
		return ErrMirrorInvalidTransition
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if toStatus == "readback_verified" || toStatus == "applied" {
		var incomplete int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_change_channel_steps
			WHERE change_set_id=? AND state!='succeeded'`, changeSetID).Scan(&incomplete); err != nil {
			return err
		}
		if incomplete != 0 {
			return ErrMirrorInvalidTransition
		}
	}
	if toStatus == "compensated" {
		var unresolved int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_change_channel_steps WHERE change_set_id=? AND (
			state IN ('intent_recorded','readback_pending') OR
			(state IN ('succeeded','needs_reconciliation') AND compensation_state!='compensated'))`,
			changeSetID).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved != 0 {
			return ErrMirrorInvalidTransition
		}
	}
	var finished any
	if toStatus == "applied" || toStatus == "compensated" {
		finished = formatTime(now.UTC())
	}
	result, err := tx.ExecContext(ctx, `UPDATE mirror_change_sets SET status=?,finished_at=?,updated_at=?
		WHERE id=? AND status=?`, toStatus, finished, formatTime(now.UTC()), changeSetID, fromStatus)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorInvalidTransition
	}
	payload := `{"from":` + mustJSONString(fromStatus) + `,"to":` + mustJSONString(toStatus) + `}`
	if err := appendMirrorEventTx(ctx, tx, changeSetID, nil, "phase_transition", toStatus,
		lease.FencingToken, payload, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordMirrorStepCompensation(ctx context.Context, changeSetID int64, stepKey, state,
	payloadJSON string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.recordMirrorStepCompensation(ctx, changeSetID, stepKey, state, payloadJSON, lease, now)
	})
}

func (s *Store) recordMirrorStepCompensation(ctx context.Context, changeSetID int64, stepKey, state,
	payloadJSON string, lease domain.TargetControlLease, now time.Time) error {
	if state != "compensated" && state != "compensation_failed" {
		return ErrMirrorInvalidTransition
	}
	payloadJSON, err := normalizeMirrorJSON(payloadJSON)
	if err != nil {
		return err
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	step, err := scanMirrorChangeStep(tx.QueryRowContext(ctx, mirrorChangeStepSelect+
		` WHERE change_set_id=? AND step_key=?`, changeSetID, stepKey))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mirror_change_channel_steps SET compensation_state=?,
		compensation_json=?,fencing_token=?,updated_at=? WHERE id=?`, state, payloadJSON, lease.FencingToken,
		formatTime(now.UTC()), step.ID); err != nil {
		return err
	}
	if err := appendMirrorEventTx(ctx, tx, changeSetID, &step.ID, state, step.Phase, lease.FencingToken,
		payloadJSON, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AppendMirrorChangeEvent(ctx context.Context, changeSetID int64, stepID *int64, eventType,
	phase, payloadJSON string, lease domain.TargetControlLease, now time.Time) error {
	return serializedWrite(ctx, s, func() error {
		return s.appendMirrorChangeEvent(ctx, changeSetID, stepID, eventType, phase, payloadJSON, lease, now)
	})
}

func (s *Store) appendMirrorChangeEvent(ctx context.Context, changeSetID int64, stepID *int64, eventType,
	phase, payloadJSON string, lease domain.TargetControlLease, now time.Time) error {
	if strings.TrimSpace(eventType) == "" || strings.TrimSpace(phase) == "" {
		return errors.New("mirror event type and phase are required")
	}
	payloadJSON, err := normalizeMirrorJSON(payloadJSON)
	if err != nil {
		return err
	}
	tx, err := s.beginMirrorChangeMutation(ctx, changeSetID, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if stepID != nil {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_change_channel_steps WHERE id=? AND change_set_id=?`,
			*stepID, changeSetID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return sql.ErrNoRows
		}
	}
	if err := appendMirrorEventTx(ctx, tx, changeSetID, stepID, eventType, phase, lease.FencingToken,
		payloadJSON, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListMirrorChangeSteps(ctx context.Context, changeSetID int64) ([]MirrorChangeStep, error) {
	rows, err := s.db.QueryContext(ctx, mirrorChangeStepSelect+` WHERE change_set_id=? ORDER BY id`, changeSetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChangeStep, 0)
	for rows.Next() {
		step, err := scanMirrorChangeStep(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, step)
	}
	return result, rows.Err()
}

func (s *Store) ListMirrorChangeEvents(ctx context.Context, changeSetID int64) ([]MirrorChangeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,change_set_id,step_id,event_type,phase,fencing_token,payload_json,created_at
		FROM mirror_change_events WHERE change_set_id=? ORDER BY id`, changeSetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChangeEvent, 0)
	for rows.Next() {
		var event MirrorChangeEvent
		var stepID sql.NullInt64
		var created string
		if err := rows.Scan(&event.ID, &event.ChangeSetID, &stepID, &event.EventType, &event.Phase,
			&event.FencingToken, &event.PayloadJSON, &created); err != nil {
			return nil, err
		}
		if stepID.Valid {
			value := stepID.Int64
			event.StepID = &value
		}
		event.CreatedAt = parseTime(created)
		result = append(result, event)
	}
	return result, rows.Err()
}

func validateNewMirrorStep(step MirrorChangeStep) error {
	if strings.TrimSpace(step.StepKey) == "" || strings.TrimSpace(step.Phase) == "" ||
		strings.TrimSpace(step.ResourceKind) == "" || strings.TrimSpace(step.ResourceID) == "" ||
		strings.TrimSpace(step.Action) == "" || strings.TrimSpace(step.RequestID) == "" ||
		strings.TrimSpace(step.ExpectedHash) == "" || strings.TrimSpace(step.DesiredHash) == "" {
		return errors.New("step identity, action and hashes are required")
	}
	if step.ExpectedHash == step.DesiredHash {
		return errors.New("functional no-op must not create a remote change step")
	}
	if len(step.StepKey) > 128 || len(step.RequestID) > 256 {
		return errors.New("step identity is too long")
	}
	return nil
}

func (s *Store) beginMirrorLeaseTx(ctx context.Context, lease domain.TargetControlLease, targetID int64, now time.Time) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := checkMirrorLeaseTx(ctx, tx, lease, now, targetID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *Store) beginMirrorChangeMutation(ctx context.Context, changeSetID int64,
	lease domain.TargetControlLease, now time.Time) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := checkMirrorLeaseTx(ctx, tx, lease, now, lease.TargetSiteID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_change_sets WHERE id=? AND target_site_id=?
		AND lease_owner_id=? AND fencing_token=? AND status NOT IN ('applied','compensated')`, changeSetID, lease.TargetSiteID, lease.OwnerID,
		lease.FencingToken).Scan(&count)
	if err != nil || count != 1 {
		_ = tx.Rollback()
		if err != nil {
			return nil, err
		}
		return nil, ErrTargetControlFenceLost
	}
	return tx, nil
}

func checkMirrorLeaseTx(ctx context.Context, tx *sql.Tx, lease domain.TargetControlLease, now time.Time, targetID int64) error {
	if lease.TargetSiteID != targetID || lease.Dimension != mirrorLeaseDimension || strings.TrimSpace(lease.OwnerID) == "" ||
		lease.FencingToken < 1 {
		return ErrTargetControlFenceLost
	}
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM target_control_leases WHERE target_site_id=? AND dimension=?
		AND owner_id=? AND fencing_token=? AND leased_until>?`, targetID, mirrorLeaseDimension, lease.OwnerID,
		lease.FencingToken, formatLeaseTime(now.UTC())).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrTargetControlFenceLost
	}
	return nil
}

func appendMirrorEventTx(ctx context.Context, tx *sql.Tx, changeSetID int64, stepID *int64, eventType, phase string,
	fencingToken int64, payloadJSON string, now time.Time) error {
	payloadJSON, err := normalizeMirrorJSON(payloadJSON)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mirror_change_events
		(change_set_id,step_id,event_type,phase,fencing_token,payload_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		changeSetID, stepID, eventType, phase, fencingToken, payloadJSON, formatTime(now.UTC()))
	return err
}

func normalizeMirrorJSON(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "{}"
	}
	if !json.Valid([]byte(value)) {
		return "", errors.New("value must be valid JSON")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(value)); err != nil {
		return "", err
	}
	return compact.String(), nil
}

func mustJSONString(value string) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}

func validMirrorChangeTransition(from, to string) bool {
	if to == "needs_reconciliation" && from != "applied" && from != "compensated" {
		return true
	}
	if to == "compensated" && from == "needs_reconciliation" {
		return true
	}
	next := map[string]string{
		"pending":            "preflight_verified",
		"preflight_verified": "removals_quiesced",
		"removals_quiesced":  "contract_applied",
		"contract_applied":   "additions_exposed",
		"additions_exposed":  "readback_verified",
		"readback_verified":  "applied",
	}
	return next[from] == to
}

const mirrorChannelOwnershipSelect = `SELECT id,policy_uuid,target_site_id,target_group,channel_id,ownership_marker,
	source_identity,model_set_authority,status_authority,last_applied_functional_hash,active,created_at,updated_at,released_at
	FROM pricing_policy_channel_ownership`

func scanMirrorChannelOwnership(row scanner) (MirrorChannelOwnership, error) {
	var item MirrorChannelOwnership
	var active int
	var created, updated string
	var released sql.NullString
	err := row.Scan(&item.ID, &item.PolicyUUID, &item.TargetSiteID, &item.TargetGroup, &item.ChannelID,
		&item.OwnershipMarker, &item.SourceIdentity, &item.ModelSetAuthority, &item.StatusAuthority,
		&item.LastAppliedFunctionalHash, &active, &created, &updated, &released)
	if err != nil {
		return MirrorChannelOwnership{}, err
	}
	item.Active = active != 0
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	if released.Valid {
		value := parseTime(released.String)
		item.ReleasedAt = &value
	}
	return item, nil
}

func sameMirrorChannelOwnership(a, b MirrorChannelOwnership) bool {
	return a.PolicyUUID == b.PolicyUUID && a.TargetSiteID == b.TargetSiteID && a.TargetGroup == b.TargetGroup &&
		a.ChannelID == b.ChannelID && a.OwnershipMarker == b.OwnershipMarker && a.SourceIdentity == b.SourceIdentity &&
		a.ModelSetAuthority == b.ModelSetAuthority && a.StatusAuthority == b.StatusAuthority &&
		a.LastAppliedFunctionalHash == b.LastAppliedFunctionalHash
}

const mirrorModelOwnershipSelect = `SELECT id,policy_uuid,target_site_id,target_group,target_model,ownership_marker,
	source_identity,last_applied_functional_hash,active,created_at,updated_at,released_at FROM pricing_policy_model_ownership`

func scanMirrorModelOwnership(row scanner) (MirrorModelOwnership, error) {
	var item MirrorModelOwnership
	var active int
	var created, updated string
	var released sql.NullString
	err := row.Scan(&item.ID, &item.PolicyUUID, &item.TargetSiteID, &item.TargetGroup, &item.TargetModel,
		&item.OwnershipMarker, &item.SourceIdentity, &item.LastAppliedFunctionalHash, &active, &created, &updated, &released)
	if err != nil {
		return MirrorModelOwnership{}, err
	}
	item.Active = active != 0
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	if released.Valid {
		value := parseTime(released.String)
		item.ReleasedAt = &value
	}
	return item, nil
}

func sameMirrorModelOwnership(a, b MirrorModelOwnership) bool {
	return a.PolicyUUID == b.PolicyUUID && a.TargetSiteID == b.TargetSiteID && a.TargetGroup == b.TargetGroup &&
		a.TargetModel == b.TargetModel && a.OwnershipMarker == b.OwnershipMarker && a.SourceIdentity == b.SourceIdentity &&
		a.LastAppliedFunctionalHash == b.LastAppliedFunctionalHash
}

const mirrorChannelModelOwnershipSelect = `SELECT id,policy_uuid,target_site_id,target_group,channel_id,target_model,
	ownership_marker,source_identity,last_applied_functional_hash,active,created_at,updated_at,released_at
	FROM pricing_policy_channel_model_ownership`

func scanMirrorChannelModelOwnership(row scanner) (MirrorChannelModelOwnership, error) {
	var item MirrorChannelModelOwnership
	var active int
	var created, updated string
	var released sql.NullString
	err := row.Scan(&item.ID, &item.PolicyUUID, &item.TargetSiteID, &item.TargetGroup, &item.ChannelID,
		&item.TargetModel, &item.OwnershipMarker, &item.SourceIdentity, &item.LastAppliedFunctionalHash,
		&active, &created, &updated, &released)
	if err != nil {
		return MirrorChannelModelOwnership{}, err
	}
	item.Active = active != 0
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	if released.Valid {
		value := parseTime(released.String)
		item.ReleasedAt = &value
	}
	return item, nil
}

func sameMirrorChannelModelOwnership(a, b MirrorChannelModelOwnership) bool {
	return a.PolicyUUID == b.PolicyUUID && a.TargetSiteID == b.TargetSiteID && a.TargetGroup == b.TargetGroup &&
		a.ChannelID == b.ChannelID && a.TargetModel == b.TargetModel && a.OwnershipMarker == b.OwnershipMarker &&
		a.SourceIdentity == b.SourceIdentity && a.LastAppliedFunctionalHash == b.LastAppliedFunctionalHash
}

const mirrorChangeSetSelect = `SELECT id,operation_key,request_hash,policy_uuid,policy_version,target_site_id,
	source_proof_json,source_semantic_hash,before_owned_json,desired_owned_json,expected_contract_revision,
	desired_contract_revision,status,lease_owner_id,fencing_token,error,created_at,updated_at,finished_at FROM mirror_change_sets`

func scanMirrorChangeSet(row scanner) (MirrorChangeSet, error) {
	var item MirrorChangeSet
	var created, updated string
	var finished sql.NullString
	err := row.Scan(&item.ID, &item.OperationKey, &item.RequestHash, &item.PolicyUUID, &item.PolicyVersion,
		&item.TargetSiteID, &item.SourceProofJSON, &item.SourceSemanticHash, &item.BeforeOwnedJSON,
		&item.DesiredOwnedJSON, &item.ExpectedContractRevision, &item.DesiredContractRevision, &item.Status,
		&item.LeaseOwnerID, &item.FencingToken, &item.Error, &created, &updated, &finished)
	if err != nil {
		return MirrorChangeSet{}, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	if finished.Valid {
		value := parseTime(finished.String)
		item.FinishedAt = &value
	}
	return item, nil
}

const mirrorChangeStepSelect = `SELECT id,change_set_id,step_key,phase,resource_kind,resource_id,action,request_id,
	expected_hash,desired_hash,before_json,desired_json,intent_json,result_json,readback_json,readback_hash,state,attempts,
	fencing_token,error,compensation_state,compensation_json,intent_at,result_at,created_at,updated_at
	FROM mirror_change_channel_steps`

func scanMirrorChangeStep(row scanner) (MirrorChangeStep, error) {
	var item MirrorChangeStep
	var intentAt, resultAt sql.NullString
	var created, updated string
	err := row.Scan(&item.ID, &item.ChangeSetID, &item.StepKey, &item.Phase, &item.ResourceKind,
		&item.ResourceID, &item.Action, &item.RequestID, &item.ExpectedHash, &item.DesiredHash,
		&item.BeforeJSON, &item.DesiredJSON, &item.IntentJSON, &item.ResultJSON, &item.ReadbackJSON, &item.ReadbackHash,
		&item.State, &item.Attempts, &item.FencingToken, &item.Error, &item.CompensationState,
		&item.CompensationJSON, &intentAt, &resultAt, &created, &updated)
	if err != nil {
		return MirrorChangeStep{}, err
	}
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	if intentAt.Valid {
		value := parseTime(intentAt.String)
		item.IntentAt = &value
	}
	if resultAt.Valid {
		value := parseTime(resultAt.String)
		item.ResultAt = &value
	}
	return item, nil
}

func listMirrorChangeStepsTx(ctx context.Context, tx *sql.Tx, changeSetID int64) ([]MirrorChangeStep, error) {
	rows, err := tx.QueryContext(ctx, mirrorChangeStepSelect+` WHERE change_set_id=? ORDER BY id`, changeSetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MirrorChangeStep, 0)
	for rows.Next() {
		step, err := scanMirrorChangeStep(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, step)
	}
	return result, rows.Err()
}
