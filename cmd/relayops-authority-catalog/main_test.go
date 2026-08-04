package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestAuthorityCatalogTwoFreshDatabaseRunsByteIdentical(t *testing.T) {
	root := t.TempDir()
	paths := func(name string) (string, string, string) {
		dir := filepath.Join(root, name)
		return filepath.Join(dir, "fresh.db"), filepath.Join(dir, "catalog.bin"), filepath.Join(dir, "receipt.json")
	}
	dbA, catalogA, receiptA := paths("a")
	dbB, catalogB, receiptB := paths("b")
	for _, args := range [][]string{
		{"--fresh-db", dbA, "--catalog", catalogA, "--receipt", receiptA, "--create-new"},
		{"--fresh-db", dbB, "--catalog", catalogB, "--receipt", receiptB, "--create-new"},
	} {
		if err := runProduce(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	comparison := filepath.Join(root, "comparison.json")
	if err := runCompare([]string{"--catalog-a", catalogA, "--catalog-b", catalogB,
		"--receipt", comparison, "--create-new"}); err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	payload, err := os.ReadFile(comparison)
	if err != nil || json.Unmarshal(payload, &receipt) != nil || receipt["equal"] != true {
		t.Fatalf("invalid comparison receipt: receipt=%v err=%v", receipt, err)
	}
}

func TestAuthorityCloneMigrationNoLossAndFailureBoundaries(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.db")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "clone supplier", BaseURL: "https://clone.example", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage.db")
	restore := filepath.Join(root, "restore.db")
	receiptPath := filepath.Join(root, "migration.json")
	if err := runMigrateClone(context.Background(), []string{"--clone", source, "--stage", stage,
		"--restore-reentry", restore, "--receipt", receiptPath, "--remote-writes", "disabled",
		"--target-writes", "disabled", "--create-new-outputs"}); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(restore)
	if err != nil {
		t.Fatalf("protocol-1 restore re-entry: %v", err)
	}
	defer restored.Close()
	if _, err := restored.GetSupplier(context.Background(), supplier.ID); err != nil {
		t.Fatalf("restored supplier missing: %v", err)
	}
	migrated, err := store.Open(stage)
	if err != nil {
		t.Fatalf("protocol-2 stage re-entry: %v", err)
	}
	defer migrated.Close()
	if _, err := migrated.AuditCollectionAuthority(context.Background()); err != nil {
		t.Fatalf("migrated authority audit: %v", err)
	}
}

func TestAuthorityCatalogCompareRejectsByteDrift(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.bin")
	b := filepath.Join(root, "b.bin")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCompare([]string{"--catalog-a", a, "--catalog-b", b,
		"--receipt", filepath.Join(root, "receipt.json"), "--create-new"}); err == nil {
		t.Fatal("byte-different catalogs were accepted")
	}
}
