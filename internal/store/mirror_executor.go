package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

// MirrorOwnershipState is the complete active ownership projection for one
// complete-mirror policy. The executor commits this projection only after the
// remote target has independently read back the requested state.
type MirrorOwnershipState struct {
	Channel     MirrorChannelOwnership        `json:"channel"`
	Models      []MirrorModelOwnership        `json:"models"`
	Memberships []MirrorChannelModelOwnership `json:"memberships"`
}

// CommitMirrorOwnershipAfterReadback atomically advances ownership from the
// exact expected projection to the exact desired projection under the current
// mirror fence. Replaying an already committed desired projection is a no-op;
// every other projection is a conflict.
func (s *Store) CommitMirrorOwnershipAfterReadback(
	ctx context.Context,
	expected MirrorOwnershipState,
	desired MirrorOwnershipState,
	expectedPolicyVersion int64,
	lease domain.TargetControlLease,
	now time.Time,
) (bool, error) {
	return serializedWriteResult(ctx, s, func() (bool, error) {
		return s.commitMirrorOwnershipAfterReadback(ctx, expected, desired, expectedPolicyVersion, lease, now)
	})
}

func (s *Store) commitMirrorOwnershipAfterReadback(
	ctx context.Context,
	expected MirrorOwnershipState,
	desired MirrorOwnershipState,
	expectedPolicyVersion int64,
	lease domain.TargetControlLease,
	now time.Time,
) (bool, error) {
	expected, err := normalizeMirrorOwnershipState(expected)
	if err != nil {
		return false, fmt.Errorf("expected ownership: %w", err)
	}
	desired, err = normalizeMirrorOwnershipState(desired)
	if err != nil {
		return false, fmt.Errorf("desired ownership: %w", err)
	}
	if !sameMirrorOwnershipBinding(expected.Channel, desired.Channel) {
		return false, errors.New("ownership channel binding cannot change during a mirror apply")
	}

	tx, err := s.beginMirrorLeaseTx(ctx, lease, expected.Channel.TargetSiteID, now)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	policy, err := scanMirrorPolicy(tx.QueryRowContext(ctx, mirrorPolicySelect+` WHERE policy_uuid=?`,
		expected.Channel.PolicyUUID))
	if err != nil {
		return false, err
	}
	if policy.PolicyVersion != expectedPolicyVersion || !policy.Enabled {
		return false, ErrMirrorPolicyVersionConflict
	}

	current, err := loadMirrorOwnershipStateTx(ctx, tx, expected.Channel.PolicyUUID)
	if err != nil {
		return false, err
	}
	current, err = normalizeMirrorOwnershipState(current)
	if err != nil {
		return false, fmt.Errorf("stored ownership: %w", err)
	}
	if sameMirrorOwnershipState(current, desired) {
		return true, tx.Commit()
	}
	if !sameMirrorOwnershipState(current, expected) {
		return false, ErrMirrorOwnershipConflict
	}

	if err := applyMirrorChannelOwnershipTx(ctx, tx, expected.Channel, desired.Channel, now.UTC()); err != nil {
		return false, err
	}
	if err := applyMirrorModelOwnershipsTx(ctx, tx, expected.Models, desired.Models, now.UTC()); err != nil {
		return false, err
	}
	if err := applyMirrorMembershipOwnershipsTx(ctx, tx, expected.Memberships, desired.Memberships, now.UTC()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func normalizeMirrorOwnershipState(state MirrorOwnershipState) (MirrorOwnershipState, error) {
	channel := &state.Channel
	channel.PolicyUUID = strings.TrimSpace(channel.PolicyUUID)
	channel.TargetGroup = strings.TrimSpace(channel.TargetGroup)
	channel.OwnershipMarker = strings.TrimSpace(channel.OwnershipMarker)
	channel.SourceIdentity = strings.TrimSpace(channel.SourceIdentity)
	channel.LastAppliedFunctionalHash = strings.TrimSpace(channel.LastAppliedFunctionalHash)
	if channel.ChannelID <= 0 || channel.TargetGroup == "" {
		return MirrorOwnershipState{}, errors.New("one active channel ownership is required")
	}
	if channel.ModelSetAuthority != "exclusive" && channel.ModelSetAuthority != "managed_subset" {
		return MirrorOwnershipState{}, errors.New("invalid channel model-set authority")
	}
	if channel.StatusAuthority != "none" && channel.StatusAuthority != "disable_only" {
		return MirrorOwnershipState{}, errors.New("invalid channel status authority")
	}
	if err := validateMirrorOwnership(channel.PolicyUUID, channel.OwnershipMarker, channel.SourceIdentity,
		channel.LastAppliedFunctionalHash, channel.TargetSiteID); err != nil {
		return MirrorOwnershipState{}, err
	}
	channel.Active = true

	modelKeys := make(map[string]struct{}, len(state.Models))
	for index := range state.Models {
		item := &state.Models[index]
		item.PolicyUUID = strings.TrimSpace(item.PolicyUUID)
		item.TargetGroup = strings.TrimSpace(item.TargetGroup)
		item.TargetModel = strings.TrimSpace(item.TargetModel)
		item.OwnershipMarker = strings.TrimSpace(item.OwnershipMarker)
		item.SourceIdentity = strings.TrimSpace(item.SourceIdentity)
		item.LastAppliedFunctionalHash = strings.TrimSpace(item.LastAppliedFunctionalHash)
		if item.TargetModel == "" || item.PolicyUUID != channel.PolicyUUID || item.TargetSiteID != channel.TargetSiteID ||
			item.TargetGroup != channel.TargetGroup || item.OwnershipMarker != channel.OwnershipMarker {
			return MirrorOwnershipState{}, fmt.Errorf("model ownership %q is outside the channel binding", item.TargetModel)
		}
		if err := validateMirrorOwnership(item.PolicyUUID, item.OwnershipMarker, item.SourceIdentity,
			item.LastAppliedFunctionalHash, item.TargetSiteID); err != nil {
			return MirrorOwnershipState{}, err
		}
		if _, duplicate := modelKeys[item.TargetModel]; duplicate {
			return MirrorOwnershipState{}, fmt.Errorf("duplicate model ownership %q", item.TargetModel)
		}
		modelKeys[item.TargetModel] = struct{}{}
		item.Active = true
	}

	membershipKeys := make(map[string]struct{}, len(state.Memberships))
	for index := range state.Memberships {
		item := &state.Memberships[index]
		item.PolicyUUID = strings.TrimSpace(item.PolicyUUID)
		item.TargetGroup = strings.TrimSpace(item.TargetGroup)
		item.TargetModel = strings.TrimSpace(item.TargetModel)
		item.OwnershipMarker = strings.TrimSpace(item.OwnershipMarker)
		item.SourceIdentity = strings.TrimSpace(item.SourceIdentity)
		item.LastAppliedFunctionalHash = strings.TrimSpace(item.LastAppliedFunctionalHash)
		if item.TargetModel == "" || item.PolicyUUID != channel.PolicyUUID || item.TargetSiteID != channel.TargetSiteID ||
			item.TargetGroup != channel.TargetGroup || item.ChannelID != channel.ChannelID ||
			item.OwnershipMarker != channel.OwnershipMarker {
			return MirrorOwnershipState{}, fmt.Errorf("membership ownership %q is outside the channel binding", item.TargetModel)
		}
		if err := validateMirrorOwnership(item.PolicyUUID, item.OwnershipMarker, item.SourceIdentity,
			item.LastAppliedFunctionalHash, item.TargetSiteID); err != nil {
			return MirrorOwnershipState{}, err
		}
		if _, duplicate := membershipKeys[item.TargetModel]; duplicate {
			return MirrorOwnershipState{}, fmt.Errorf("duplicate membership ownership %q", item.TargetModel)
		}
		membershipKeys[item.TargetModel] = struct{}{}
		item.Active = true
	}

	sort.Slice(state.Models, func(i, j int) bool { return state.Models[i].TargetModel < state.Models[j].TargetModel })
	sort.Slice(state.Memberships, func(i, j int) bool {
		return state.Memberships[i].TargetModel < state.Memberships[j].TargetModel
	})
	return state, nil
}

func loadMirrorOwnershipStateTx(ctx context.Context, tx *sql.Tx, policyUUID string) (MirrorOwnershipState, error) {
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
	if result.Channel.ChannelID == 0 {
		return result, ErrMirrorOwnershipConflict
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
	return result, nil
}

func sameMirrorOwnershipState(left, right MirrorOwnershipState) bool {
	if !sameMirrorChannelOwnershipValue(left.Channel, right.Channel) || len(left.Models) != len(right.Models) ||
		len(left.Memberships) != len(right.Memberships) {
		return false
	}
	for index := range left.Models {
		if !sameMirrorModelOwnershipValue(left.Models[index], right.Models[index]) {
			return false
		}
	}
	for index := range left.Memberships {
		if !sameMirrorMembershipOwnershipValue(left.Memberships[index], right.Memberships[index]) {
			return false
		}
	}
	return true
}

func sameMirrorOwnershipBinding(left, right MirrorChannelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.ChannelID == right.ChannelID &&
		left.OwnershipMarker == right.OwnershipMarker && left.SourceIdentity == right.SourceIdentity &&
		left.ModelSetAuthority == right.ModelSetAuthority && left.StatusAuthority == right.StatusAuthority
}

func sameMirrorChannelOwnershipValue(left, right MirrorChannelOwnership) bool {
	return sameMirrorOwnershipBinding(left, right) &&
		left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func sameMirrorModelOwnershipValue(left, right MirrorModelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.TargetModel == right.TargetModel &&
		left.OwnershipMarker == right.OwnershipMarker && left.SourceIdentity == right.SourceIdentity &&
		left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func sameMirrorMembershipOwnershipValue(left, right MirrorChannelModelOwnership) bool {
	return left.PolicyUUID == right.PolicyUUID && left.TargetSiteID == right.TargetSiteID &&
		left.TargetGroup == right.TargetGroup && left.ChannelID == right.ChannelID &&
		left.TargetModel == right.TargetModel && left.OwnershipMarker == right.OwnershipMarker &&
		left.SourceIdentity == right.SourceIdentity &&
		left.LastAppliedFunctionalHash == right.LastAppliedFunctionalHash
}

func applyMirrorChannelOwnershipTx(ctx context.Context, tx *sql.Tx, before, after MirrorChannelOwnership, now time.Time) error {
	if sameMirrorChannelOwnershipValue(before, after) {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE pricing_policy_channel_ownership
		SET last_applied_functional_hash=?,updated_at=?
		WHERE policy_uuid=? AND target_site_id=? AND channel_id=? AND ownership_marker=?
		AND last_applied_functional_hash=? AND active=1`, after.LastAppliedFunctionalHash, formatTime(now),
		before.PolicyUUID, before.TargetSiteID, before.ChannelID, before.OwnershipMarker,
		before.LastAppliedFunctionalHash)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return ErrMirrorOwnershipConflict
	}
	return nil
}

func applyMirrorModelOwnershipsTx(ctx context.Context, tx *sql.Tx, before, after []MirrorModelOwnership, now time.Time) error {
	beforeByModel := make(map[string]MirrorModelOwnership, len(before))
	afterByModel := make(map[string]MirrorModelOwnership, len(after))
	for _, item := range before {
		beforeByModel[item.TargetModel] = item
	}
	for _, item := range after {
		afterByModel[item.TargetModel] = item
	}
	for model, previous := range beforeByModel {
		next, retained := afterByModel[model]
		if !retained {
			result, err := tx.ExecContext(ctx, `UPDATE pricing_policy_model_ownership
				SET active=0,released_at=?,updated_at=? WHERE policy_uuid=? AND target_site_id=?
				AND target_group=? AND target_model=? AND ownership_marker=?
				AND last_applied_functional_hash=? AND active=1`, formatTime(now), formatTime(now),
				previous.PolicyUUID, previous.TargetSiteID, previous.TargetGroup, previous.TargetModel,
				previous.OwnershipMarker, previous.LastAppliedFunctionalHash)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrMirrorOwnershipConflict
			}
			continue
		}
		if !sameMirrorModelOwnershipValue(previous, next) {
			if previous.PolicyUUID != next.PolicyUUID || previous.TargetSiteID != next.TargetSiteID ||
				previous.TargetGroup != next.TargetGroup || previous.TargetModel != next.TargetModel ||
				previous.OwnershipMarker != next.OwnershipMarker || previous.SourceIdentity != next.SourceIdentity {
				return ErrMirrorOwnershipConflict
			}
			result, err := tx.ExecContext(ctx, `UPDATE pricing_policy_model_ownership
				SET last_applied_functional_hash=?,updated_at=? WHERE policy_uuid=? AND target_site_id=?
				AND target_group=? AND target_model=? AND ownership_marker=?
				AND last_applied_functional_hash=? AND active=1`, next.LastAppliedFunctionalHash, formatTime(now),
				previous.PolicyUUID, previous.TargetSiteID, previous.TargetGroup, previous.TargetModel,
				previous.OwnershipMarker, previous.LastAppliedFunctionalHash)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrMirrorOwnershipConflict
			}
		}
	}
	for model, item := range afterByModel {
		if _, existed := beforeByModel[model]; existed {
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_model_ownership
			(policy_uuid,target_site_id,target_group,target_model,ownership_marker,source_identity,
			last_applied_functional_hash,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?)`,
			item.PolicyUUID, item.TargetSiteID, item.TargetGroup, item.TargetModel, item.OwnershipMarker,
			item.SourceIdentity, item.LastAppliedFunctionalHash, formatTime(now), formatTime(now))
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrMirrorOwnershipConflict
			}
			return err
		}
	}
	return nil
}

func applyMirrorMembershipOwnershipsTx(ctx context.Context, tx *sql.Tx, before, after []MirrorChannelModelOwnership, now time.Time) error {
	beforeByModel := make(map[string]MirrorChannelModelOwnership, len(before))
	afterByModel := make(map[string]MirrorChannelModelOwnership, len(after))
	for _, item := range before {
		beforeByModel[item.TargetModel] = item
	}
	for _, item := range after {
		afterByModel[item.TargetModel] = item
	}
	for model, previous := range beforeByModel {
		next, retained := afterByModel[model]
		if !retained {
			result, err := tx.ExecContext(ctx, `UPDATE pricing_policy_channel_model_ownership
				SET active=0,released_at=?,updated_at=? WHERE policy_uuid=? AND target_site_id=?
				AND channel_id=? AND target_model=? AND ownership_marker=?
				AND last_applied_functional_hash=? AND active=1`, formatTime(now), formatTime(now),
				previous.PolicyUUID, previous.TargetSiteID, previous.ChannelID, previous.TargetModel,
				previous.OwnershipMarker, previous.LastAppliedFunctionalHash)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrMirrorOwnershipConflict
			}
			continue
		}
		if !sameMirrorMembershipOwnershipValue(previous, next) {
			if previous.PolicyUUID != next.PolicyUUID || previous.TargetSiteID != next.TargetSiteID ||
				previous.TargetGroup != next.TargetGroup || previous.ChannelID != next.ChannelID ||
				previous.TargetModel != next.TargetModel || previous.OwnershipMarker != next.OwnershipMarker ||
				previous.SourceIdentity != next.SourceIdentity {
				return ErrMirrorOwnershipConflict
			}
			result, err := tx.ExecContext(ctx, `UPDATE pricing_policy_channel_model_ownership
				SET last_applied_functional_hash=?,updated_at=? WHERE policy_uuid=? AND target_site_id=?
				AND channel_id=? AND target_model=? AND ownership_marker=?
				AND last_applied_functional_hash=? AND active=1`, next.LastAppliedFunctionalHash, formatTime(now),
				previous.PolicyUUID, previous.TargetSiteID, previous.ChannelID, previous.TargetModel,
				previous.OwnershipMarker, previous.LastAppliedFunctionalHash)
			if err != nil {
				return err
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return ErrMirrorOwnershipConflict
			}
		}
	}
	for model, item := range afterByModel {
		if _, existed := beforeByModel[model]; existed {
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pricing_policy_channel_model_ownership
			(policy_uuid,target_site_id,target_group,channel_id,target_model,ownership_marker,source_identity,
			last_applied_functional_hash,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`,
			item.PolicyUUID, item.TargetSiteID, item.TargetGroup, item.ChannelID, item.TargetModel,
			item.OwnershipMarker, item.SourceIdentity, item.LastAppliedFunctionalHash,
			formatTime(now), formatTime(now))
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrMirrorOwnershipConflict
			}
			return err
		}
	}
	return nil
}
