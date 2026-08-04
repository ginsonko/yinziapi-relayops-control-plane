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
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/mirror"
)

// AdoptMirrorOwnershipAndEnable atomically installs the complete ownership
// projection and enables a disabled native policy after independent remote
// readback. An exact replay is a no-op; every partial or foreign projection is
// rejected without committing any local ownership row.
func (s *Store) AdoptMirrorOwnershipAndEnable(
	ctx context.Context,
	desired MirrorOwnershipState,
	expectedPolicyVersion int64,
	lease domain.TargetControlLease,
	now time.Time,
) (mirror.Policy, bool, error) {
	desired, err := normalizeMirrorOwnershipState(desired)
	if err != nil {
		return mirror.Policy{}, false, fmt.Errorf("desired adoption ownership: %w", err)
	}
	if expectedPolicyVersion < 1 {
		return mirror.Policy{}, false, errors.New("adoption requires the current disabled policy version")
	}
	result, err := serializedWriteResult(ctx, s, func() (mirrorAdoptionResult, error) {
		policy, replayed, adoptErr := s.adoptMirrorOwnershipAndEnable(
			ctx, desired, expectedPolicyVersion, lease, now)
		return mirrorAdoptionResult{policy: policy, replayed: replayed}, adoptErr
	})
	return result.policy, result.replayed, err
}

type mirrorAdoptionResult struct {
	policy   mirror.Policy
	replayed bool
}

func (s *Store) adoptMirrorOwnershipAndEnable(
	ctx context.Context,
	desired MirrorOwnershipState,
	expectedPolicyVersion int64,
	lease domain.TargetControlLease,
	now time.Time,
) (mirror.Policy, bool, error) {
	tx, err := s.beginMirrorLeaseTx(ctx, lease, desired.Channel.TargetSiteID, now)
	if err != nil {
		return mirror.Policy{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	policy, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`,
		desired.Channel.PolicyUUID))
	if err != nil {
		return mirror.Policy{}, false, err
	}
	if err := validateAdoptionPolicyBinding(policy, desired); err != nil {
		return mirror.Policy{}, false, err
	}
	var origin string
	if err := tx.QueryRowContext(ctx, `SELECT origin FROM mirror_policies WHERE policy_uuid=?`,
		policy.PolicyUUID).Scan(&origin); err != nil {
		return mirror.Policy{}, false, err
	}
	if origin != "native" || policy.LegacyPolicyID != 0 {
		return mirror.Policy{}, false, errors.New("only a native mirror draft can adopt an existing channel")
	}

	current, err := loadOptionalMirrorOwnershipStateTx(ctx, tx, policy.PolicyUUID)
	if err != nil {
		return mirror.Policy{}, false, err
	}
	if policy.Enabled {
		if !mirrorOwnershipStatePresent(current) || !sameMirrorOwnershipState(current, desired) {
			return mirror.Policy{}, false, ErrMirrorOwnershipConflict
		}
		if err := tx.Commit(); err != nil {
			return mirror.Policy{}, false, err
		}
		return policy, true, nil
	}
	if policy.PolicyVersion != expectedPolicyVersion {
		return mirror.Policy{}, false, ErrMirrorPolicyVersionConflict
	}
	if mirrorOwnershipStatePresent(current) && !sameMirrorOwnershipState(current, desired) {
		return mirror.Policy{}, false, ErrMirrorOwnershipConflict
	}
	if !mirrorOwnershipStatePresent(current) &&
		(len(current.Models) != 0 || len(current.Memberships) != 0) {
		return mirror.Policy{}, false, ErrMirrorOwnershipConflict
	}

	if !mirrorOwnershipStatePresent(current) {
		if err := insertAdoptedMirrorOwnershipTx(ctx, tx, desired, now.UTC()); err != nil {
			return mirror.Policy{}, false, err
		}
	}

	policy.Enabled = true
	policy.PolicyVersion = expectedPolicyVersion + 1
	if err := mirror.ValidatePolicy(policy, mirror.PolicyCapabilities{CompleteModelSet: true, ManagedChannelCAS: true}); err != nil {
		return mirror.Policy{}, false, err
	}
	payload, err := encodeCanonicalMirrorPolicy(policy)
	if err != nil {
		return mirror.Policy{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE mirror_policies SET policy_version=?,policy_json=?,updated_at=?
		WHERE policy_uuid=? AND policy_version=?`, policy.PolicyVersion, string(payload), formatTime(now.UTC()),
		policy.PolicyUUID, expectedPolicyVersion)
	if err != nil {
		return mirror.Policy{}, false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return mirror.Policy{}, false, ErrMirrorPolicyVersionConflict
	}
	readback, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`, policy.PolicyUUID))
	if err != nil {
		return mirror.Policy{}, false, err
	}
	if !reflect.DeepEqual(readback, policy) {
		return mirror.Policy{}, false, errors.New("adopted mirror policy readback mismatch")
	}
	owned, err := loadOptionalMirrorOwnershipStateTx(ctx, tx, policy.PolicyUUID)
	if err != nil {
		return mirror.Policy{}, false, err
	}
	if !mirrorOwnershipStatePresent(owned) || !sameMirrorOwnershipState(owned, desired) {
		return mirror.Policy{}, false, errors.New("adopted mirror ownership readback mismatch")
	}
	if err := tx.Commit(); err != nil {
		return mirror.Policy{}, false, err
	}
	return policy, false, nil
}

func validateAdoptionPolicyBinding(policy mirror.Policy, desired MirrorOwnershipState) error {
	channel := desired.Channel
	wantSource := fmt.Sprintf("supplier/%d/group/%s", policy.SupplierID, policy.SourceGroup)
	if policy.Kind != mirror.PolicyKindCompleteMirror || policy.PolicyUUID != channel.PolicyUUID ||
		policy.TargetSiteID != channel.TargetSiteID || policy.TargetGroup != channel.TargetGroup ||
		channel.SourceIdentity != wantSource {
		return errors.New("adoption ownership does not match the immutable mirror policy binding")
	}
	return nil
}

func mirrorOwnershipStatePresent(state MirrorOwnershipState) bool {
	return state.Channel.ChannelID > 0
}

func loadOptionalMirrorOwnershipStateTx(ctx context.Context, tx *sql.Tx, policyUUID string) (MirrorOwnershipState, error) {
	result := MirrorOwnershipState{}
	channels, err := tx.QueryContext(ctx, mirrorChannelOwnershipSelect+` WHERE policy_uuid=? AND active=1`, policyUUID)
	if err != nil {
		return result, err
	}
	for channels.Next() {
		item, scanErr := scanMirrorChannelOwnership(channels)
		if scanErr != nil {
			_ = channels.Close()
			return result, scanErr
		}
		if result.Channel.ChannelID != 0 {
			_ = channels.Close()
			return result, ErrMirrorOwnershipConflict
		}
		result.Channel = item
	}
	if err := channels.Err(); err != nil {
		_ = channels.Close()
		return result, err
	}
	if err := channels.Close(); err != nil {
		return result, err
	}

	models, err := tx.QueryContext(ctx, mirrorModelOwnershipSelect+` WHERE policy_uuid=? AND active=1`, policyUUID)
	if err != nil {
		return result, err
	}
	for models.Next() {
		item, scanErr := scanMirrorModelOwnership(models)
		if scanErr != nil {
			_ = models.Close()
			return result, scanErr
		}
		result.Models = append(result.Models, item)
	}
	if err := models.Err(); err != nil {
		_ = models.Close()
		return result, err
	}
	if err := models.Close(); err != nil {
		return result, err
	}

	memberships, err := tx.QueryContext(ctx, mirrorChannelModelOwnershipSelect+` WHERE policy_uuid=? AND active=1`, policyUUID)
	if err != nil {
		return result, err
	}
	for memberships.Next() {
		item, scanErr := scanMirrorChannelModelOwnership(memberships)
		if scanErr != nil {
			_ = memberships.Close()
			return result, scanErr
		}
		result.Memberships = append(result.Memberships, item)
	}
	if err := memberships.Err(); err != nil {
		_ = memberships.Close()
		return result, err
	}
	if err := memberships.Close(); err != nil {
		return result, err
	}
	if !mirrorOwnershipStatePresent(result) {
		return result, nil
	}
	return normalizeMirrorOwnershipState(result)
}

func insertAdoptedMirrorOwnershipTx(ctx context.Context, tx *sql.Tx, desired MirrorOwnershipState, now time.Time) error {
	channel := desired.Channel
	if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_channel_ownership
		(policy_uuid,target_site_id,target_group,channel_id,ownership_marker,source_identity,model_set_authority,
		status_authority,last_applied_functional_hash,active,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,1,?,?)`, channel.PolicyUUID, channel.TargetSiteID, channel.TargetGroup,
		channel.ChannelID, channel.OwnershipMarker, channel.SourceIdentity, channel.ModelSetAuthority,
		channel.StatusAuthority, channel.LastAppliedFunctionalHash, formatTime(now), formatTime(now)); err != nil {
		return adoptionOwnershipInsertError(err)
	}
	for _, item := range desired.Models {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_model_ownership
			(policy_uuid,target_site_id,target_group,target_model,ownership_marker,source_identity,
			last_applied_functional_hash,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?)`,
			item.PolicyUUID, item.TargetSiteID, item.TargetGroup, item.TargetModel, item.OwnershipMarker,
			item.SourceIdentity, item.LastAppliedFunctionalHash, formatTime(now), formatTime(now)); err != nil {
			return adoptionOwnershipInsertError(err)
		}
	}
	for _, item := range desired.Memberships {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_channel_model_ownership
			(policy_uuid,target_site_id,target_group,channel_id,target_model,ownership_marker,source_identity,
			last_applied_functional_hash,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`,
			item.PolicyUUID, item.TargetSiteID, item.TargetGroup, item.ChannelID, item.TargetModel,
			item.OwnershipMarker, item.SourceIdentity, item.LastAppliedFunctionalHash,
			formatTime(now), formatTime(now)); err != nil {
			return adoptionOwnershipInsertError(err)
		}
	}
	return nil
}

func adoptionOwnershipInsertError(err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrMirrorOwnershipConflict
	}
	return err
}
