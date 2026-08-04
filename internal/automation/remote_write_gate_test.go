package automation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/remotewrite"
)

func TestRemoteWriteGatePreservesPendingProposalBeforeApplyOrRollbackStateTransitions(t *testing.T) {
	fixture := newAutomationFixture(t, "add", "0.01", "0.05")
	defer fixture.cleanup()
	proposal, err := fixture.service.Propose(context.Background(), fixture.policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.GetProposal(context.Background(), proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	valueBefore, writesBefore := fixture.target.value("economy")
	snapshotReadsBefore, pricingReadsBefore := fixture.target.reads()
	fixture.service.SetRemoteWriteGate(remotewrite.New(false))

	if _, err := fixture.service.Apply(context.Background(), proposal.ID); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("apply error=%v, want remote-write gate", err)
	}
	if _, err := fixture.service.Rollback(context.Background(), proposal.ID); !errors.Is(err, remotewrite.ErrDisabled) {
		t.Fatalf("rollback error=%v, want remote-write gate", err)
	}
	after, err := fixture.store.GetProposal(context.Background(), proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("blocked proposal mutation changed state\nbefore=%#v\nafter=%#v", before, after)
	}
	valueAfter, writesAfter := fixture.target.value("economy")
	snapshotReadsAfter, pricingReadsAfter := fixture.target.reads()
	if valueAfter != valueBefore || writesAfter != writesBefore || snapshotReadsAfter != snapshotReadsBefore || pricingReadsAfter != pricingReadsBefore {
		t.Fatalf("blocked proposal reached target: value %s->%s writes %d->%d snapshot reads %d->%d pricing reads %d->%d",
			valueBefore, valueAfter, writesBefore, writesAfter, snapshotReadsBefore, snapshotReadsAfter, pricingReadsBefore, pricingReadsAfter)
	}
}
