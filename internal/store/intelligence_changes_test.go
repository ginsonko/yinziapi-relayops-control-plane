package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestPruneOperatingLedgerChangesKeepsRecentTailAndCurrentRevision(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/relayops.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	credential := domain.Credential{Name: "revision target", Kind: "access-token", SiteURL: "https://revision-target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: credential.Name, BaseURL: credential.SiteURL, CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	events := make([]OperatingLedgerEvent, 20)
	for index := range events {
		now := time.Now().UTC().Add(time.Duration(index) * time.Nanosecond)
		events[index] = OperatingLedgerEvent{EventID: fmt.Sprintf("prune-%02d", index), TargetSiteID: target.ID,
			OccurredAt: now, EventKind: "usage", Requests: 1, Source: "test", PayloadHash: fmt.Sprintf("hash-%02d", index), CreatedAt: now}
	}
	if err := st.UpsertOperatingLedgerEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	current, err := st.CurrentOperatingLedgerRevision(ctx)
	if err != nil || current != 20 {
		t.Fatalf("revision=%d err=%v", current, err)
	}
	deleted, err := st.PruneOperatingLedgerChanges(ctx, current, 5, 10)
	if err != nil || deleted != 10 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	remaining, err := st.ListOperatingLedgerChanges(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 10 || remaining[0].Revision != 11 || remaining[len(remaining)-1].Revision != current {
		t.Fatalf("remaining revisions=%#v", remaining)
	}
	after, err := st.CurrentOperatingLedgerRevision(ctx)
	if err != nil || after != current {
		t.Fatalf("current revision changed after prune: before=%d after=%d err=%v", current, after, err)
	}
}
