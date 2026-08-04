package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

func TestMirrorSchemaV3MigratesLegacyPoliciesDeterministicallyAndIdempotently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relayops.db")
	st, supplier, target := openMirrorTestStoreAt(t, path)

	legacy := domain.PricingPolicy{
		Name: "legacy", SupplierID: supplier.ID, SourceGroup: "plus", SourceModelPattern: "gpt-*",
		SourceBasis: "group_ratio", TargetSiteID: target.ID, TargetGroup: "retail", RuleType: "add",
		Value: "0.03", MinimumRatio: "0.1", MaximumRatio: "9", RoundingStep: "0.01",
		OnlyFollowIncrease: true, Enabled: false, ExecutionMode: "auto", CoverageMode: "strict",
		MinimumCoverage: "1",
	}
	if err := st.CreatePolicy(ctx, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO change_proposals
		(policy_id,target_site_id,target_group,expected_old,proposed_value,expected_hash,calculation_json,
		group_model_pricing_plan_json,group_model_pricing_receipt_json,status,mode,idempotency_key,readback_value,
		error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, legacy.ID, target.ID, "retail",
		"1", "1.03", "before", `{}`, `{}`, `{}`, "pending", "auto", "legacy-pending", "", "",
		formatTime(time.Now().UTC()), formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	// Recreate the real upgrade boundary: the legacy data predates schema-v3.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM mirror_policies`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=?`, mirrorSchemaMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := migrated.GetMirrorPolicyByLegacyID(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantUUID := deterministicLegacyMirrorPolicyUUID(legacy)
	if policy.PolicyUUID != wantUUID || policy.PolicyVersion != 1 || policy.SchemaVersion != 3 {
		t.Fatalf("migrated identity = %#v, want uuid %s", policy, wantUUID)
	}
	if policy.Enabled || policy.ExecutionMode != "auto" || !policy.SyncGroupRatio || policy.SyncModelPrices ||
		policy.SyncModelAdditions || policy.SyncModelRemovals || policy.GroupRatioRule.Type != "add" ||
		policy.GroupRatioRule.Value != "0.03" || policy.LegacyRuleTarget != "group_ratio" {
		t.Fatalf("legacy semantics changed during migration: %#v", policy)
	}
	var proposalStatus string
	if err := migrated.db.QueryRowContext(ctx, `SELECT status FROM change_proposals WHERE idempotency_key='legacy-pending'`).Scan(&proposalStatus); err != nil {
		t.Fatal(err)
	}
	if proposalStatus != "stale" {
		t.Fatalf("legacy proposal status = %q", proposalStatus)
	}
	for _, table := range []string{
		"pricing_policy_channel_ownership", "pricing_policy_model_ownership", "pricing_policy_channel_model_ownership",
	} {
		var count int
		if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("migration granted destructive ownership in %s", table)
		}
	}

	legacy.Value = "8"
	if err := migrated.UpdatePolicy(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	second := domain.PricingPolicy{
		Name: "contract", SupplierID: supplier.ID, SourceGroup: "video", SourceModelPattern: "jimeng-*",
		SourceBasis: "group_model_contract", TargetSiteID: target.ID, TargetGroup: "video-retail",
		RuleType: "target_margin", Value: "0.2", Enabled: true, ExecutionMode: "approval",
		CoverageMode: "threshold", MinimumCoverage: "0.8",
	}
	if err := migrated.CreatePolicy(ctx, &second); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	unchanged, err := reopened.GetMirrorPolicyByLegacyID(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.GroupRatioRule.Value != "0.03" || unchanged.PolicyUUID != wantUUID {
		t.Fatalf("existing migrated policy was rewritten: %#v", unchanged)
	}
	newlySynced, err := reopened.GetMirrorPolicyByLegacyID(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newlySynced.PolicyUUID != deterministicLegacyMirrorPolicyUUID(second) || newlySynced.PriceAnchor != mirror.PriceAnchorEffectiveSale ||
		!newlySynced.SyncModelPrices || !newlySynced.SyncBillingMode || newlySynced.SyncModelAdditions || newlySynced.SyncModelRemovals {
		t.Fatalf("new legacy policy migration = %#v", newlySynced)
	}
	var count int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mirror_policies WHERE legacy_policy_id=?`, legacy.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("legacy migration count = %d", count)
	}
}

func TestMirrorPolicyOptimisticVersioningRejectsStaleUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	first := policy
	first.GroupRatioRule = mirror.Rule{Type: "add", Value: "0.1"}
	updated, err := st.UpdateMirrorPolicy(ctx, first, 1, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.PolicyVersion != 2 {
		t.Fatalf("updated version = %d", updated.PolicyVersion)
	}
	stale := policy
	stale.GroupRatioRule = mirror.Rule{Type: "fixed", Value: "2"}
	if _, err := st.UpdateMirrorPolicy(ctx, stale, 1, now.Add(2*time.Second)); !errors.Is(err, ErrMirrorPolicyVersionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	stored, err := st.GetMirrorPolicy(ctx, policy.PolicyUUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PolicyVersion != 2 || stored.GroupRatioRule.Type != "add" || stored.GroupRatioRule.Value != "0.1" {
		t.Fatalf("stored policy = %#v", stored)
	}
}

func TestListMirrorPoliciesRejectsEveryDuplicatedStorageIdentityMutation(t *testing.T) {
	tests := []struct {
		name        string
		expectedErr string
		mutate      func(*testing.T, context.Context, *Store, mirror.Policy)
	}{
		{name: "sql policy uuid", expectedErr: "policy_uuid", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET policy_uuid=? WHERE policy_uuid=?`, uuid.NewString(), policy.PolicyUUID)
		}},
		{name: "sql policy version", expectedErr: "policy_version", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET policy_version=policy_version+1 WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql legacy identity", expectedErr: "legacy_policy_id", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET legacy_policy_id=999 WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql origin", expectedErr: "origin", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET origin='legacy_compat' WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql supplier", expectedErr: "supplier_id", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET supplier_id=supplier_id+1000 WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql source group", expectedErr: "source_group", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET source_group='other-source' WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql target site", expectedErr: "target_site_id", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET target_site_id=target_site_id+1000 WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "sql target group", expectedErr: "target_group", mutate: func(t *testing.T, ctx context.Context, st *Store, policy mirror.Policy) {
			mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET target_group='other-target' WHERE policy_uuid=?`, policy.PolicyUUID)
		}},
		{name: "json policy uuid", expectedErr: "policy_uuid", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.PolicyUUID = uuid.NewString() })},
		{name: "json policy version", expectedErr: "policy_version", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.PolicyVersion++ })},
		{name: "json schema version", expectedErr: "schema_version", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.SchemaVersion++ })},
		{name: "json legacy identity", expectedErr: "legacy_policy_id", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.LegacyPolicyID = 999 })},
		{name: "json supplier", expectedErr: "supplier_id", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.SupplierID++ })},
		{name: "json source group", expectedErr: "source_group", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.SourceGroup = "other-source" })},
		{name: "json target site", expectedErr: "target_site_id", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.TargetSiteID++ })},
		{name: "json target group", expectedErr: "target_group", mutate: mutateMirrorPolicyJSON(func(policy *mirror.Policy) { policy.TargetGroup = "other-target" })},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			st, supplier, target := openMirrorTestStore(t)
			policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
			if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, ctx, st, policy)
			if _, err := st.ListMirrorPolicies(ctx); err == nil || !strings.Contains(err.Error(), testCase.expectedErr) {
				t.Fatalf("ListMirrorPolicies error = %v, want field %q", err, testCase.expectedErr)
			}
		})
	}
}

func TestMirrorPolicyIntegrityCapacityBoundaries(t *testing.T) {
	t.Run("policy canonicality and 64 KiB envelope", func(t *testing.T) {
		ctx := context.Background()
		st, supplier, target := openMirrorTestStore(t)
		policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
		if err := st.CreateMirrorPolicy(ctx, policy, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var payload string
		if err := st.db.QueryRowContext(ctx, `SELECT policy_json FROM mirror_policies WHERE policy_uuid=?`, policy.PolicyUUID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) >= mirrorPolicyPayloadMaxBytes {
			t.Fatalf("test policy payload is unexpectedly large: %d", len(payload))
		}
		exact := payload + strings.Repeat(" ", mirrorPolicyPayloadMaxBytes-len(payload))
		mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET policy_json=? WHERE policy_uuid=?`, exact, policy.PolicyUUID)
		if _, err := st.ListMirrorPolicies(ctx); err == nil || !strings.Contains(err.Error(), "mirror_policy_payload_invalid") {
			t.Fatalf("non-canonical 64 KiB policy error = %v", err)
		}
		mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET policy_json=? WHERE policy_uuid=?`, exact+" ", policy.PolicyUUID)
		if _, err := st.ListMirrorPolicies(ctx); err == nil || !strings.Contains(err.Error(), "mirror_policy_payload_capacity_exceeded") {
			t.Fatalf("64 KiB + 1 policy error = %v", err)
		}
	})

	t.Run("policy row count 10000", func(t *testing.T) {
		ctx := context.Background()
		st, supplier, target := openMirrorTestStore(t)
		policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
		now := time.Now().UTC()
		if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
			t.Fatal(err)
		}
		var payload string
		if err := st.db.QueryRowContext(ctx, `SELECT policy_json FROM mirror_policies WHERE policy_uuid=?`, policy.PolicyUUID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		_, err := st.db.ExecContext(ctx, `WITH digits(d) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
			seq(n) AS (SELECT a.d*1000+b.d*100+c.d*10+d.d+1 FROM digits a,digits b,digits c,digits d)
			INSERT INTO mirror_policies(policy_uuid,policy_version,schema_version,legacy_policy_id,origin,supplier_id,
				source_group,target_site_id,target_group,policy_json,created_at,updated_at)
			SELECT printf('00000000-0000-4000-8000-%012d',n),1,3,NULL,'native',?,?,?,?,
				replace(?,?,printf('00000000-0000-4000-8000-%012d',n)),?,? FROM seq WHERE n<=9999`,
			supplier.ID, policy.SourceGroup, target.ID, policy.TargetGroup, payload, policy.PolicyUUID, formatTime(now), formatTime(now))
		if err != nil {
			t.Fatal(err)
		}
		if policies, err := st.ListMirrorPolicies(ctx); err != nil || len(policies) != mirrorPolicyIntegrityMaxRows {
			t.Fatalf("10,000 policy list = %d, %v", len(policies), err)
		}
		extraUUID := "00000000-0000-4000-8000-000000010000"
		extraPayload := strings.Replace(payload, policy.PolicyUUID, extraUUID, 1)
		mustExecMirrorMutation(t, ctx, st, `INSERT INTO mirror_policies(policy_uuid,policy_version,schema_version,
			legacy_policy_id,origin,supplier_id,source_group,target_site_id,target_group,policy_json,created_at,updated_at)
			VALUES(?,1,3,NULL,'native',?,?,?,?,?,?,?)`, extraUUID, supplier.ID, policy.SourceGroup, target.ID,
			policy.TargetGroup, extraPayload, formatTime(now), formatTime(now))
		if _, err := st.ListMirrorPolicies(ctx); err == nil || !strings.Contains(err.Error(), "mirror_policy_integrity_capacity_exceeded") {
			t.Fatalf("10,001 policy error = %v", err)
		}
	})
}

func TestDeleteMirrorPolicyHidesPolicyAndReleasesOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + policy.PolicyUUID
	mustExecMirrorMutation(t, ctx, st, `INSERT INTO pricing_policy_channel_ownership
		(policy_uuid,target_site_id,target_group,channel_id,ownership_marker,source_identity,model_set_authority,
		status_authority,last_applied_functional_hash,active,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,1,?,?)`, policy.PolicyUUID, target.ID, policy.TargetGroup, 77, marker,
		"supplier/group", "exclusive", "disable_only", "hash", formatTime(now), formatTime(now))
	if err := st.DeleteMirrorPolicy(ctx, policy.PolicyUUID, policy.PolicyVersion, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMirrorPolicy(ctx, policy.PolicyUUID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted policy read error = %v", err)
	}
	policies, err := st.ListMirrorPolicies(ctx)
	if err != nil || len(policies) != 0 {
		t.Fatalf("policies after delete = %#v, %v", policies, err)
	}
	var active int
	var releasedAt *string
	if err := st.db.QueryRowContext(ctx, `SELECT active,released_at FROM pricing_policy_channel_ownership
		WHERE policy_uuid=?`, policy.PolicyUUID).Scan(&active, &releasedAt); err != nil {
		t.Fatal(err)
	}
	if active != 0 || releasedAt == nil || *releasedAt == "" {
		t.Fatalf("ownership active=%d released_at=%v", active, releasedAt)
	}
	if err := st.DeleteMirrorPolicy(ctx, policy.PolicyUUID, policy.PolicyVersion, now.Add(2*time.Minute)); err == nil {
		t.Fatal("repeated delete unexpectedly succeeded")
	}
}

func mustExecMirrorMutation(t *testing.T, ctx context.Context, st *Store, query string, args ...any) {
	t.Helper()
	if _, err := st.db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func mutateMirrorPolicyJSON(mutate func(*mirror.Policy)) func(*testing.T, context.Context, *Store, mirror.Policy) {
	return func(t *testing.T, ctx context.Context, st *Store, stored mirror.Policy) {
		t.Helper()
		var payload string
		if err := st.db.QueryRowContext(ctx, `SELECT policy_json FROM mirror_policies WHERE policy_uuid=?`, stored.PolicyUUID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var policy mirror.Policy
		if err := json.Unmarshal([]byte(payload), &policy); err != nil {
			t.Fatal(err)
		}
		mutate(&policy)
		encoded, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		mustExecMirrorMutation(t, ctx, st, `UPDATE mirror_policies SET policy_json=? WHERE policy_uuid=?`, string(encoded), stored.PolicyUUID)
	}
}

func TestMirrorOwnershipRequiresExactMarkerHashTargetAndCurrentFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	other := newMirrorTestPolicy(t, supplier.ID, target.ID, "other")
	if err := st.CreateMirrorPolicy(ctx, other, now); err != nil {
		t.Fatal(err)
	}
	lease1, err := st.AcquireMirrorLease(ctx, target.ID, "worker-1", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + policy.PolicyUUID
	channel := MirrorChannelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail",
		ChannelID: 77, OwnershipMarker: marker, SourceIdentity: "supplier/group", ModelSetAuthority: "exclusive",
		StatusAuthority: "disable_only", LastAppliedFunctionalHash: "channel-v1"}
	if err := st.ClaimMirrorChannelOwnership(ctx, &channel, lease1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	channels, err := st.ListActiveMirrorChannelOwnerships(ctx, policy.PolicyUUID)
	if err != nil || len(channels) != 1 || channels[0].ChannelID != channel.ChannelID {
		t.Fatalf("active channel ownerships = %#v, %v", channels, err)
	}
	id := channel.ID
	if err := st.ClaimMirrorChannelOwnership(ctx, &channel, lease1, now.Add(2*time.Second)); err != nil || channel.ID != id {
		t.Fatalf("idempotent channel claim id=%d err=%v", channel.ID, err)
	}
	competing := channel
	competing.ID = 0
	competing.PolicyUUID = other.PolicyUUID
	competing.OwnershipMarker = "relayops:policy/" + other.PolicyUUID
	if err := st.ClaimMirrorChannelOwnership(ctx, &competing, lease1, now.Add(3*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("competing channel claim = %v", err)
	}
	wrongTarget := channel
	wrongTarget.ID = 0
	wrongTarget.ChannelID = 88
	wrongTarget.TargetSiteID = target.ID + 1000
	if err := st.ClaimMirrorChannelOwnership(ctx, &wrongTarget, lease1, now.Add(3*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("cross-target lease claim = %v", err)
	}

	model := MirrorModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail",
		TargetModel: "gpt-5", OwnershipMarker: marker, SourceIdentity: "supplier/group/gpt-5",
		LastAppliedFunctionalHash: "model-v1"}
	if err := st.ClaimMirrorModelOwnership(ctx, &model, lease1, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	channelModel := MirrorChannelModelOwnership{PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID,
		TargetGroup: "retail", ChannelID: 77, TargetModel: "gpt-5", OwnershipMarker: marker,
		SourceIdentity: "supplier/group/gpt-5", LastAppliedFunctionalHash: "membership-v1"}
	if err := st.ClaimMirrorChannelModelOwnership(ctx, &channelModel, lease1, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateMirrorModelOwnershipHash(ctx, target.ID, "retail", "gpt-5", policy.PolicyUUID,
		marker, "wrong", "model-v2", lease1, now.Add(6*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("wrong expected ownership hash = %v", err)
	}
	if err := st.UpdateMirrorModelOwnershipHash(ctx, target.ID, "retail", "gpt-5", policy.PolicyUUID,
		marker, "model-v1", "model-v2", lease1, now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	storedModel, err := st.GetActiveMirrorModelOwnership(ctx, target.ID, "retail", "gpt-5")
	if err != nil || storedModel.LastAppliedFunctionalHash != "model-v2" {
		t.Fatalf("stored model ownership=%#v err=%v", storedModel, err)
	}
	modelItems, err := st.ListActiveMirrorModelOwnerships(ctx, policy.PolicyUUID)
	if err != nil || len(modelItems) != 1 {
		t.Fatalf("active model ownership list=%#v err=%v", modelItems, err)
	}

	lease2, err := st.AcquireMirrorLease(ctx, target.ID, "worker-2", now.Add(61*time.Second), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseMirrorChannelOwnership(ctx, target.ID, 77, policy.PolicyUUID, marker, "channel-v1",
		lease1, now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale ownership release = %v", err)
	}
	if err := st.ReleaseMirrorModelOwnership(ctx, target.ID, "retail", "gpt-5", policy.PolicyUUID, marker,
		"model-v1", lease2, now.Add(63*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("old ownership hash release = %v", err)
	}
	if err := st.ReleaseMirrorModelOwnership(ctx, target.ID, "retail", "gpt-5", policy.PolicyUUID, marker,
		"model-v2", lease2, now.Add(64*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseMirrorChannelModelOwnership(ctx, target.ID, 77, "gpt-5", policy.PolicyUUID, marker,
		"membership-v1", lease2, now.Add(65*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseMirrorChannelOwnership(ctx, target.ID, 77, policy.PolicyUUID, marker, "channel-v1",
		lease2, now.Add(66*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"pricing_policy_channel_ownership", "pricing_policy_model_ownership", "pricing_policy_channel_model_ownership",
	} {
		var active int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE active=1`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 0 {
			t.Fatalf("active ownership remains in %s", table)
		}
	}
}

func TestMirrorChangeSetPersistsIntentAndRecoversCrashAndLostResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	lease1, err := st.AcquireMirrorLease(ctx, target.ID, "worker-1", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changeSet := newMirrorTestChangeSet(policy, target.ID, "operation-1", "request-1", "step-1")
	replay, err := st.CreateMirrorChangeSet(ctx, &changeSet, lease1, now.Add(time.Second))
	if err != nil || replay {
		t.Fatalf("create change set replay=%v err=%v", replay, err)
	}
	if changeSet.ID == 0 || changeSet.Steps[0].ID == 0 {
		t.Fatalf("change set was not durably identified: %#v", changeSet)
	}
	duplicate := newMirrorTestChangeSet(policy, target.ID, "operation-1", "request-1", "step-1")
	replay, err = st.CreateMirrorChangeSet(ctx, &duplicate, lease1, now.Add(2*time.Second))
	if err != nil || !replay || duplicate.ID != changeSet.ID {
		t.Fatalf("idempotent create replay=%v err=%v set=%#v", replay, err, duplicate)
	}
	conflict := newMirrorTestChangeSet(policy, target.ID, "operation-1", "different", "step-1")
	if _, err := st.CreateMirrorChangeSet(ctx, &conflict, lease1, now.Add(3*time.Second)); !errors.Is(err, ErrMirrorIdempotencyConflict) {
		t.Fatalf("conflicting operation key = %v", err)
	}
	if err := st.RecordMirrorStepIntent(ctx, changeSet.ID, "step-1", `{"request":"send"}`, lease1,
		now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}

	// The process crashes after durable intent but before a response. A new
	// lease reads the live expected state and retries exactly once.
	lease2, err := st.AcquireMirrorLease(ctx, target.ID, "worker-2", now.Add(61*time.Second), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClaimMirrorChangeSet(ctx, changeSet.ID, lease2, now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMirrorStepIntent(ctx, changeSet.ID, "step-1", `{"request":"send"}`, lease1,
		now.Add(63*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale intent write = %v", err)
	}
	if err := st.RecordMirrorStepResult(ctx, changeSet.ID, "step-1", `{}`, "late result", lease1,
		now.Add(63*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale result write = %v", err)
	}
	if err := st.RecordMirrorStepCompensation(ctx, changeSet.ID, "step-1", "compensated", `{}`, lease1,
		now.Add(63*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale compensation write = %v", err)
	}
	if err := st.AppendMirrorChangeEvent(ctx, changeSet.ID, nil, "worker_note", "recovery", `{}`, lease1,
		now.Add(63*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale event append = %v", err)
	}
	if err := st.AppendMirrorChangeEvent(ctx, changeSet.ID, nil, "worker_note", "recovery",
		`{"owner":"worker-2"}`, lease2, now.Add(63*time.Second)); err != nil {
		t.Fatal(err)
	}
	classification, err := st.ClassifyMirrorStepReadback(ctx, changeSet.ID, "step-1", "before",
		`{"hash":"before"}`, lease2, now.Add(64*time.Second))
	if err != nil || classification != "retryable_pending" {
		t.Fatalf("crash recovery classification=%q err=%v", classification, err)
	}
	if err := st.RecordMirrorStepIntent(ctx, changeSet.ID, "step-1", `{"request":"send"}`, lease2,
		now.Add(65*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMirrorStepResult(ctx, changeSet.ID, "step-1", `{"transport":"lost"}`, "response lost",
		lease2, now.Add(66*time.Second)); err != nil {
		t.Fatal(err)
	}
	classification, err = st.ClassifyMirrorStepReadback(ctx, changeSet.ID, "step-1", "desired",
		`{"hash":"desired"}`, lease2, now.Add(67*time.Second))
	if err != nil || classification != "already_applied" {
		t.Fatalf("lost response classification=%q err=%v", classification, err)
	}
	stored, err := st.GetMirrorChangeSet(ctx, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Steps) != 1 || stored.Steps[0].State != "succeeded" || stored.Steps[0].Attempts != 2 ||
		stored.Steps[0].ResultJSON != `{"transport":"lost"}` || stored.Steps[0].ReadbackJSON != `{"hash":"desired"}` {
		t.Fatalf("recovered step = %#v", stored.Steps)
	}
	for _, transition := range [][2]string{
		{"pending", "preflight_verified"}, {"preflight_verified", "removals_quiesced"},
		{"removals_quiesced", "contract_applied"}, {"contract_applied", "additions_exposed"},
		{"additions_exposed", "readback_verified"}, {"readback_verified", "applied"},
	} {
		if err := st.TransitionMirrorChangeSet(ctx, changeSet.ID, transition[0], transition[1], lease2,
			now.Add(68*time.Second)); err != nil {
			t.Fatalf("transition %v: %v", transition, err)
		}
	}
	if err := st.TransitionMirrorChangeSet(ctx, changeSet.ID, "pending", "preflight_verified", lease1,
		now.Add(69*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale transition = %v", err)
	}
	events, err := st.ListMirrorChangeEvents(ctx, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 12 {
		t.Fatalf("event journal too short: %d", len(events))
	}
}

func TestMirrorChangeSetThirdStateStopsAndRequiresReconciliation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 28, 4, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireMirrorLease(ctx, target.ID, "worker", now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changeSet := newMirrorTestChangeSet(policy, target.ID, "operation-third", "request-third", "step-third")
	if _, err := st.CreateMirrorChangeSet(ctx, &changeSet, lease, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, transition := range [][2]string{
		{"pending", "preflight_verified"}, {"preflight_verified", "removals_quiesced"},
		{"removals_quiesced", "contract_applied"}, {"contract_applied", "additions_exposed"},
	} {
		if err := st.TransitionMirrorChangeSet(ctx, changeSet.ID, transition[0], transition[1], lease,
			now.Add(time.Second)); err != nil {
			t.Fatalf("prepare transition %v: %v", transition, err)
		}
	}
	if err := st.TransitionMirrorChangeSet(ctx, changeSet.ID, "additions_exposed", "readback_verified", lease,
		now.Add(time.Second)); !errors.Is(err, ErrMirrorInvalidTransition) {
		t.Fatalf("unfinished steps passed final readback gate: %v", err)
	}
	if err := st.RecordMirrorStepIntent(ctx, changeSet.ID, "step-third", `{"send":true}`, lease,
		now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMirrorStepResult(ctx, changeSet.ID, "step-third", `{"ok":true}`, "", lease,
		now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	classification, err := st.ClassifyMirrorStepReadback(ctx, changeSet.ID, "step-third", "manual-third-state",
		`{"hash":"manual-third-state"}`, lease, now.Add(4*time.Second))
	if err != nil || classification != "needs_reconciliation" {
		t.Fatalf("third-state classification=%q err=%v", classification, err)
	}
	stored, err := st.GetMirrorChangeSet(ctx, changeSet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "needs_reconciliation" || stored.Steps[0].State != "needs_reconciliation" {
		t.Fatalf("third state did not stop change set: %#v", stored)
	}
	if err := st.RecordMirrorStepCompensation(ctx, changeSet.ID, "step-third", "compensated", `{"restored":true}`,
		lease, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.TransitionMirrorChangeSet(ctx, changeSet.ID, "needs_reconciliation", "compensated", lease,
		now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	recoverable, err := st.ListRecoverableMirrorChangeSets(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range recoverable {
		if item.ID == changeSet.ID {
			t.Fatal("compensated change set remained recoverable")
		}
	}
}

func openMirrorTestStore(t *testing.T) (*Store, domain.Supplier, domain.TargetSite) {
	t.Helper()
	return openMirrorTestStoreAt(t, filepath.Join(t.TempDir(), "relayops.db"))
}

func openMirrorTestStoreAt(t *testing.T, path string) (*Store, domain.Supplier, domain.TargetSite) {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	credential := domain.Credential{Name: "target-admin", Kind: "newapi-management", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://supplier.example", Adapter: "newapi",
		PollIntervalSeconds: 300, RechargeValueFactor: "1", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "target", BaseURL: credential.SiteURL, Adapter: "newapi",
		CredentialID: credential.ID, Enabled: true, WriteMode: "approval"}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	return st, supplier, target
}

func newMirrorTestPolicy(t *testing.T, supplierID, targetID int64, targetGroup string) mirror.Policy {
	t.Helper()
	document := mirror.NewCompleteMirrorDocument(newTestUUID(t))
	document.SupplierID = supplierID
	document.SourceGroup = "source"
	document.SourceModelPattern = "*"
	document.TargetSiteID = targetID
	document.TargetGroup = targetGroup
	policy, err := mirror.DecodePolicy(document)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func newMirrorTestChangeSet(policy mirror.Policy, targetID int64, operationKey, requestHash, stepKey string) MirrorChangeSet {
	return MirrorChangeSet{
		OperationKey: operationKey, RequestHash: requestHash, PolicyUUID: policy.PolicyUUID,
		PolicyVersion: policy.PolicyVersion, TargetSiteID: targetID, SourceProofJSON: `{"attempts":["a","b"]}`,
		SourceSemanticHash: "source", BeforeOwnedJSON: `{"owned":"before"}`,
		DesiredOwnedJSON: `{"owned":"desired"}`, ExpectedContractRevision: "contract-before",
		DesiredContractRevision: "contract-desired", Steps: []MirrorChangeStep{{
			StepKey: stepKey, Phase: "contract", ResourceKind: "group_model_contract", ResourceID: "retail",
			Action: "apply", RequestID: operationKey + ":contract", ExpectedHash: "before", DesiredHash: "desired",
			BeforeJSON: `{"hash":"before"}`, DesiredJSON: `{"hash":"desired"}`,
		}},
	}
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}
