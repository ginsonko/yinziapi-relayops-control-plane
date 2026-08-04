package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAdoptMirrorOwnershipAndEnableIsAtomicReplayableAndFenced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	policy.Enabled = false
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireMirrorLease(ctx, target.ID, "adoption-1", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	desired := mirrorAdoptionStateForTest(t, policy.PolicyUUID, supplier.ID, target.ID, "retail", 77,
		"channel-hash", []string{"alpha", "beta"})

	adopted, replayed, err := st.AdoptMirrorOwnershipAndEnable(ctx, desired, 1, lease, now.Add(time.Second))
	if err != nil || replayed {
		t.Fatalf("first adoption policy=%#v replay=%v error=%v", adopted, replayed, err)
	}
	if !adopted.Enabled || adopted.PolicyVersion != 2 {
		t.Fatalf("adopted policy = %#v", adopted)
	}
	actual := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID)
	normalized, err := normalizeMirrorOwnershipState(desired)
	if err != nil {
		t.Fatal(err)
	}
	if !sameMirrorOwnershipState(actual, normalized) {
		t.Fatalf("adopted ownership = %#v, want %#v", actual, normalized)
	}

	adopted, replayed, err = st.AdoptMirrorOwnershipAndEnable(ctx, desired, 1, lease, now.Add(2*time.Second))
	if err != nil || !replayed || adopted.PolicyVersion != 2 {
		t.Fatalf("replay policy=%#v replay=%v error=%v", adopted, replayed, err)
	}
	conflict := desired
	conflict.Channel.LastAppliedFunctionalHash = "changed"
	if _, _, err := st.AdoptMirrorOwnershipAndEnable(ctx, conflict, 1, lease, now.Add(3*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	if after := readMirrorOwnershipStateForTest(t, st, policy.PolicyUUID); !sameMirrorOwnershipState(after, normalized) {
		t.Fatalf("conflicting replay mutated ownership: %#v", after)
	}

	competing, err := st.AcquireMirrorLease(ctx, target.ID, "adoption-2", now.Add(61*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_ = competing
	if _, _, err := st.AdoptMirrorOwnershipAndEnable(ctx, desired, 1, lease, now.Add(62*time.Second)); !errors.Is(err, ErrTargetControlFenceLost) {
		t.Fatalf("stale fence error = %v", err)
	}
}

func TestAdoptMirrorOwnershipAndEnableRollsBackEveryRowOnForeignConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, supplier, target := openMirrorTestStore(t)
	now := time.Date(2026, 7, 29, 7, 0, 0, 0, time.UTC)
	policy := newMirrorTestPolicy(t, supplier.ID, target.ID, "retail")
	policy.Enabled = false
	if err := st.CreateMirrorPolicy(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	foreign := newMirrorTestPolicy(t, supplier.ID, target.ID, "other")
	if err := st.CreateMirrorPolicy(ctx, foreign, now); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireMirrorLease(ctx, target.ID, "foreign-owner", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	marker := "relayops:policy/" + foreign.PolicyUUID
	owned := MirrorModelOwnership{PolicyUUID: foreign.PolicyUUID, TargetSiteID: target.ID, TargetGroup: "retail",
		TargetModel: "beta", OwnershipMarker: marker, SourceIdentity: "supplier/foreign/group",
		LastAppliedFunctionalHash: "foreign-hash"}
	if err := st.ClaimMirrorModelOwnership(ctx, &owned, lease, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseTargetControlLease(ctx, lease, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	lease, err = st.AcquireMirrorLease(ctx, target.ID, "adoption", now.Add(3*time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	desired := mirrorAdoptionStateForTest(t, policy.PolicyUUID, supplier.ID, target.ID, "retail", 77,
		"channel-hash", []string{"alpha", "beta"})
	if _, _, err := st.AdoptMirrorOwnershipAndEnable(ctx, desired, 1, lease, now.Add(4*time.Second)); !errors.Is(err, ErrMirrorOwnershipConflict) {
		t.Fatalf("foreign conflict error = %v", err)
	}
	stored, err := st.GetMirrorPolicy(ctx, policy.PolicyUUID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Enabled || stored.PolicyVersion != 1 {
		t.Fatalf("failed adoption changed policy = %#v", stored)
	}
	channels, _ := st.ListActiveMirrorChannelOwnerships(ctx, policy.PolicyUUID)
	models, _ := st.ListActiveMirrorModelOwnerships(ctx, policy.PolicyUUID)
	memberships, _ := st.ListActiveMirrorChannelModelOwnerships(ctx, policy.PolicyUUID)
	if len(channels) != 0 || len(models) != 0 || len(memberships) != 0 {
		t.Fatalf("failed adoption partially committed channel=%d models=%d memberships=%d",
			len(channels), len(models), len(memberships))
	}
}

func mirrorAdoptionStateForTest(t *testing.T, policyUUID string, supplierID, targetID int64,
	targetGroup string, channelID int64, channelHash string, models []string) MirrorOwnershipState {
	t.Helper()
	marker := "relayops:policy/" + policyUUID
	source := "supplier/" + int64Text(supplierID) + "/group/source"
	state := MirrorOwnershipState{Channel: MirrorChannelOwnership{PolicyUUID: policyUUID, TargetSiteID: targetID,
		TargetGroup: targetGroup, ChannelID: channelID, OwnershipMarker: marker, SourceIdentity: source,
		ModelSetAuthority: "exclusive", StatusAuthority: "disable_only", LastAppliedFunctionalHash: channelHash}}
	for _, modelName := range models {
		state.Models = append(state.Models, MirrorModelOwnership{PolicyUUID: policyUUID, TargetSiteID: targetID,
			TargetGroup: targetGroup, TargetModel: modelName, OwnershipMarker: marker,
			SourceIdentity: source + "/model/" + modelName, LastAppliedFunctionalHash: "model-" + modelName})
		state.Memberships = append(state.Memberships, MirrorChannelModelOwnership{PolicyUUID: policyUUID,
			TargetSiteID: targetID, TargetGroup: targetGroup, ChannelID: channelID, TargetModel: modelName,
			OwnershipMarker: marker, SourceIdentity: source + "/model/" + modelName,
			LastAppliedFunctionalHash: "membership-" + modelName})
	}
	return state
}

func int64Text(value int64) string {
	return fmt.Sprintf("%d", value)
}
