package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestCatalogEvidenceTracksTotalObservationsAndCurrentStreak(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "evidence", BaseURL: "https://evidence.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC)
	save := func(hash string, offset time.Duration) {
		t.Helper()
		snapshot := historySnapshot(supplier.ID, hash, "0.05", "9", base.Add(offset))
		snapshot.Evidence.SourceType = "public_api"
		if err := st.SaveSnapshot(ctx, snapshot); err != nil {
			t.Fatalf("SaveSnapshot(%s): %v", hash, err)
		}
	}

	save("A", 0)
	save("A", time.Minute)
	statusA, err := st.CatalogEvidenceStatus(ctx, supplier.ID, "A")
	if err != nil || statusA.TotalObservations != 2 || statusA.ConsecutiveObservations != 2 || !statusA.LastSeen.Equal(base.Add(time.Minute)) {
		t.Fatalf("A after consecutive reads = %#v, %v", statusA, err)
	}
	save("B", 2*time.Minute)
	statusB, err := st.CatalogEvidenceStatus(ctx, supplier.ID, "B")
	if err != nil || statusB.TotalObservations != 1 || statusB.ConsecutiveObservations != 1 {
		t.Fatalf("B after switch = %#v, %v", statusB, err)
	}
	save("A", 3*time.Minute)
	statusA, err = st.CatalogEvidenceStatus(ctx, supplier.ID, "A")
	if err != nil || statusA.TotalObservations != 3 || statusA.ConsecutiveObservations != 1 {
		t.Fatalf("A after returning from B = %#v, %v", statusA, err)
	}
	save("A", 4*time.Minute)
	statusA, err = st.CatalogEvidenceStatus(ctx, supplier.ID, "A")
	if err != nil || statusA.TotalObservations != 4 || statusA.ConsecutiveObservations != 2 {
		t.Fatalf("A after second current-streak read = %#v, %v", statusA, err)
	}
	latest, err := st.CatalogEvidenceStatus(ctx, supplier.ID, "")
	if err != nil || latest.ResponseHash != "A" || latest.SourceType != "public_api" || !latest.LastSeen.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("latest evidence = %#v, %v", latest, err)
	}
}

func TestOpenMigratesLegacyCatalogEvidenceColumns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-evidence.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE catalog_snapshots (
		id INTEGER PRIMARY KEY,
		supplier_id INTEGER NOT NULL,
		payload_json TEXT NOT NULL,
		response_hash TEXT NOT NULL,
		observed_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 7, 22, 6, 30, 0, 0, time.UTC)
	payload, _ := json.Marshal(domain.CatalogSnapshot{SupplierID: 7,
		Evidence: domain.Evidence{SourceType: "declared_json_api", ResponseHash: "legacy", ObservedAt: observed}})
	if _, err := raw.Exec(`INSERT INTO catalog_snapshots(supplier_id,payload_json,response_hash,observed_at) VALUES(?,?,?,?)`,
		7, string(payload), "legacy", formatTime(observed)); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy store: %v", err)
	}
	defer st.Close()
	status, err := st.CatalogEvidenceStatus(context.Background(), 7, "legacy")
	if err != nil || status.TotalObservations != 1 || status.ConsecutiveObservations != 1 || !status.LastSeen.Equal(observed) {
		t.Fatalf("migrated evidence = %#v, %v", status, err)
	}
}
