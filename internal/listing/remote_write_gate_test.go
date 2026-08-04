package listing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

func TestRemoteWriteGatePreservesListingPlanAndCreatesNoChangeSet(t *testing.T) {
	fixture := newListingFixture(t)
	plan, err := fixture.service.Preview(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.GetListingPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.SetRemoteWriteGate(remotewrite.New(false))

	if _, err := fixture.service.Apply(context.Background(), plan.ID, ApplyInput{IdempotencyKey: "blocked-apply", ConfirmEnable: true}); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("apply error=%v, want remote-write gate", err)
	}
	if _, err := fixture.service.Rollback(context.Background(), 1, RollbackInput{ConfirmRollback: true}); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("rollback error=%v, want remote-write gate", err)
	}
	if err := fixture.service.RecoverIncomplete(context.Background()); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("recovery error=%v, want remote-write gate", err)
	}
	after, err := fixture.store.GetListingPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("blocked listing changed plan\nbefore=%#v\nafter=%#v", before, after)
	}
	changes, err := fixture.store.ListListingChangeSets(context.Background(), plan.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("blocked listing created change sets: %#v", changes)
	}
	assertNoListingTargetWrites(t, fixture)
}
