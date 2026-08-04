package main

import (
	"context"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestRunRetentionConvergesOnStoppedDatabase(t *testing.T) {
	path := t.TempDir() + "/relayops.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if err := st.AddActivity(context.Background(), domain.Activity{Kind: "test", Severity: "info", Title: "old",
		Message: "old", CreatedAt: now.Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	receipt, err := runRetention(path, 10, 0, 5*time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Converged || receipt.Activities != 1 || receipt.Batches != 2 {
		t.Fatalf("unexpected retention receipt: %#v", receipt)
	}
}

func TestRunRetentionReportsBoundedNonConvergence(t *testing.T) {
	path := t.TempDir() + "/relayops.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 251; index++ {
		if err := st.AddActivity(context.Background(), domain.Activity{Kind: "test", Severity: "info", Title: "old",
			Message: "old", CreatedAt: now.Add(-8 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	receipt, err := runRetention(path, 1, 0, 5*time.Second, now)
	if err == nil || receipt.Converged || receipt.Activities != 250 || receipt.Batches != 1 {
		t.Fatalf("bounded retention receipt=%#v err=%v", receipt, err)
	}
}
