package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommitMirrorOwnershipAfterReadbackIsAtomicReplayableAndFenced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 28, 6, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	lease1, err := st.AcquireMirrorLease(ctx, target.ID, "executor-1", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + policy.PolicyUUID
	channel := MirrorChannelOwnership{
		PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", ChannelID: 77,
		OwnershipMarker: marker, SourceIdentity: "supplier/group", ModelSetAuthority: "exclusive",
		StatusAuthority: "disable_only", LastAppliedFunctionalHash: "channel-before",
	}
	if err := st.ClaimMirrorChannelOwnership(ctx, &channel, lease1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"keep", "remove"} {
		owned := MirrorModelOwnership{
			PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", TargetModel: model,
			OwnershipMarker: marker, SourceIdentity: "supplier/group/model/" + model,
			LastAppliedFunctionalHash: "model-before-" + model,
		}
		if err := st.ClaimMirrorModelOwnership(ctx, &owned, lease1, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		member := MirrorChannelModelOwnership{
			PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", ChannelID: 77,
			TargetModel: model, OwnershipMarker: marker, SourceIdentity: "supplier/group/model/" + model,
			LastAppliedFunctionalHash: "member-before-" + model,
		}
		if err := st.ClaimMirrorChannelModelOwnership(ctx, &member, lease1, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	expected := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID)
	desired := MirrorOwnershipState{Channel: expected.Channel}
	desired.Channel.LastAppliedFunctionalHash = "channel-after"
	desired.Models = []MirrorModelOwnership{
		withModelOwnershipHash(expected.Models[0], "model-after-keep"),
		{
			PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", TargetModel: "new",
			OwnershipMarker: marker, SourceIdentity: "supplier/group/model/new",
			LastAppliedFunctionalHash: "model-after-new",
		},
	}
	// Normalize test ordering independently of the database IDs.
	if desired.Models[0].TargetModel != "keep" {
		desired.Models[0] = withModelOwnershipHash(expected.Models[1], "model-after-keep")
	}
	desired.Memberships = []MirrorChannelModelOwnership{
		{
			PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", ChannelID: 77,
			TargetModel: "keep", OwnershipMarker: marker, SourceIdentity: "supplier/group/model/keep",
			LastAppliedFunctionalHash: "member-before-keep",
		},
		{
			PolicyUUID: policy.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail", ChannelID: 77,
			TargetModel: "new", OwnershipMarker: marker, SourceIdentity: "supplier/group/model/new",
			LastAppliedFunctionalHash: "member-after-new",
		},
	}

	replayed, err := st.CommitMirrorOwnershipAfterReadback(ctx, expected, desired, policy.PolicyVersion,
		lease1, now.Add(4*time.Second))
	if err != nil || replayed {
		t.Fatalf("first ownership commit replay=%v error=%v", replayed, err)
	}
	actual := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID)
	normalizedDesired, err := normalizeMirrorOwnershipState(desired)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMirrorOwnershipState(actual, normalizedDesired) {
		t.Fatalf("committed ownership = %#v, want %#v", actual, normalizedDesired)
	}
	replayed, err = st.CommitMirrorOwnershipAfterReadback(ctx, expected, desired, policy.PolicyVersion,
		lease1, now.Add(5*time.Second))
	if err != nil || !replayed {
		t.Fatalf("ownership replay=%v error=%v", replayed, err)
	}

	conflicting := desired
	conflicting.Channel.LastAppliedFunctionalHash = "channel-third"
	if _, err := st.CommitMirrorOwnershipAfterReadback(ctx, expected, conflicting, policy.PolicyVersion,
		lease1, now.Add(6*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("third ownership state error = %v", err)
	}
	afterConflict := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID)
	if !sameMirrorOwnershipState(afterConflict, normalizedDesired) {
		t.Fatalf("conflicting commit partially mutated ownership: %#v", afterConflict)
	}

	lease2, err := st.AcquireMirrorLease(ctx, target.ID, "executor-2", now.Add(61*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	future := normalizedDesired
	future.Channel.LastAppliedFunctionalHash = "channel-future"
	if _, err := st.CommitMirrorOwnershipAfterReadback(ctx, normalizedDesired, future, policy.PolicyVersion,
		lease1, now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale fence commit error = %v", err)
	}
	if _, err := st.CommitMirrorOwnershipAfterReadback(ctx, normalizedDesired, future, policy.PolicyVersion,
		lease2, now.Add(62*time.Second)); err != nil {
		t.Fatalf("current fence commit: %v", err)
	}
	current := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID)
	policy.Enabled = false
	updated, err := st.UpdateMirrorPolicy(ctx, policy, policy.PolicyVersion, now.Add(63*time.Second))
	if err != nil || updated.PolicyVersion == policy.PolicyVersion {
		t.Fatalf("disable policy: updated=%#v error=%v", updated, err)
	}
	further := current
	further.Channel.LastAppliedFunctionalHash = "channel-after-disabled"
	if _, err := st.CommitMirrorOwnershipAfterReadback(ctx, current, further, policy.PolicyVersion,
		lease2, now.Add(64*time.Second)); !errors.Is(err, ErrMirrorPolicyVersionConflict) {
		t.Fatalf("disabled policy ownership commit error = %v", err)
	}
	if afterDisabled := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID); !sameMirrorOwnershipState(afterDisabled, current) {
		t.Fatalf("disabled policy partially mutated ownership: %#v", afterDisabled)
	}
}

func readMirrorOwnershipStateForTest(t *testing.T, st *Store, policyUUID string) MirrorOwnershipState {
	t.Helper()
	channels, err := st.ListActiveMirrorChannelOwnerships(context.Background(), policyUUID)
	if err != nil || len(channels) != 1 {
		t.Fatalf("channel ownerships=%#v error=%v", channels, err)
	}
	models, err := st.ListActiveMirrorModelOwnerships(context.Background(), policyUUID)
	if err != nil {
		t.Fatal(err)
	}
	memberships, err := st.ListActiveMirrorChannelModelOwnerships(context.Background(), policyUUID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := normalizeMirrorOwnershipState(MirrorOwnershipState{
		Channel: channels[0], Models: models, Memberships: memberships,
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func withModelOwnershipHash(item MirrorModelOwnership, hash string) MirrorModelOwnership {
	item.LastAppliedFunctionalHash = hash
	return item
}
