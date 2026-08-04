package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func openWatchStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "watchlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createWatchSupplier(t *testing.T, st *Store) domain.Supplier {
	t.Helper()
	supplier := domain.Supplier{Name: "watch upstream", BaseURL: "https://watch.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	return supplier
}

func TestManualWatchUpsertUsesStableIdentityAndPersistedOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	supplier := createWatchSupplier(t, st)
	first := WatchRecord{SupplierID: supplier.ID, ScopeType: "group", GroupKey: "economy", DisplayName: "first"}
	second := WatchRecord{SupplierID: supplier.ID, ScopeType: "group", GroupKey: "premium", DisplayName: "second"}
	if err := st.CreateManualWatch(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateManualWatch(ctx, &second); err != nil {
		t.Fatal(err)
	}
	repeated := WatchRecord{SupplierID: supplier.ID, ScopeType: "group", GroupKey: "economy", DisplayName: "renamed"}
	if err := st.CreateManualWatch(ctx, &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ID != first.ID || repeated.ManualRank != first.ManualRank {
		t.Fatalf("upsert identity/rank = %d/%d, want %d/%d", repeated.ID, repeated.ManualRank, first.ID, first.ManualRank)
	}
	if err := st.ReorderManualWatches(ctx, []int64{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListWatchRecords(ctx)
	if err != nil || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID || items[1].DisplayName != "renamed" {
		t.Fatalf("reordered items=%#v err=%v", items, err)
	}
}

func TestManualWatchDeleteAndAutomaticSuppressionAreIndependent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	supplier := createWatchSupplier(t, st)
	manual := WatchRecord{SupplierID: supplier.ID, ScopeType: "model", GroupKey: "video", ModelKey: "seedance", Note: "keep"}
	if err := st.CreateManualWatch(ctx, &manual); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteManualWatch(ctx, manual.ID); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListWatchRecords(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("deleted manual items=%#v err=%v", items, err)
	}
	if err := st.SuppressAutomaticWatch(ctx, supplier.ID, "group", "economy", "", "semantic-v1"); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListWatchRecords(ctx)
	if err != nil || len(items) != 1 || items[0].ManualPinned || items[0].SuppressedHash != "semantic-v1" {
		t.Fatalf("suppressed automatic items=%#v err=%v", items, err)
	}
}

func TestWatchSettingsPreserveDisableAndBoundInvalidNumbers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	settings, err := st.WatchSettings(ctx)
	if err != nil || settings != DefaultWatchSettings() {
		t.Fatalf("defaults=%#v err=%v", settings, err)
	}
	saved, err := st.SaveWatchSettings(ctx, WatchSettings{AutoEnabled: false, RetentionDays: 0, AutomaticLimit: 999, OverviewLimit: 21})
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultWatchSettings()
	want.AutoEnabled = false
	if saved != want {
		t.Fatalf("normalized settings=%#v want=%#v", saved, want)
	}
}

func TestWatchGroupRatioHistoryKeepsExplicitZeroAndLatestOldSample(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	supplier := createWatchSupplier(t, st)
	now := time.Now().UTC()
	for index, fixture := range []struct {
		ratio string
		at    time.Time
	}{{"0", now.Add(-26 * time.Hour)}, {"0.2", now.Add(-25 * time.Hour)}, {"0.3", now.Add(-time.Hour)}} {
		snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
			Groups:   []domain.GroupOffer{{ExternalID: "economy", Name: "Economy", Ratio: fixture.ratio}},
			Evidence: domain.Evidence{ObservedAt: fixture.at, ResponseHash: string(rune('a' + index))}}
		if err := st.SaveSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	samples, err := st.ListWatchGroupRatiosAtOrBefore(ctx, now.Add(-24*time.Hour), []WatchGroupRatioKey{{
		SupplierID: supplier.ID,
		GroupKey:   "economy",
	}})
	if err != nil || len(samples) != 1 || samples[0].Ratio != "0.2" {
		t.Fatalf("historical samples=%#v err=%v", samples, err)
	}
}

func TestAutomaticWatchChangesUseRecentAttemptsAndOneHistoricalPredecessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	supplier := createWatchSupplier(t, st)
	now := time.Now().UTC().Truncate(time.Second)
	insertProof := func(id int64, at time.Time, group, hash string) {
		t.Helper()
		stamp := formatTime(at)
		if _, err := st.db.ExecContext(ctx, `INSERT INTO collection_attempts
			(id,supplier_id,status,error_class,error_message,catalog_updated,balance_updated,started_at,finished_at,duration_ms)
			VALUES(?,?, 'success','','',1,0,?,?,1)`, id, supplier.ID, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO catalog_group_collection_proofs
			(attempt_id,supplier_id,source_group,source_semantic_hash,proof_json,observed_at)
			VALUES(?,?,?,?, '{}',?)`, id, supplier.ID, group, hash, stamp); err != nil {
			t.Fatal(err)
		}
	}
	insertProof(1, now.Add(-10*24*time.Hour), "economy", "hash-a")
	insertProof(2, now.Add(-6*24*time.Hour), "economy", "hash-b")
	insertProof(3, now.Add(-5*24*time.Hour), "economy", "hash-b")
	insertProof(4, now.Add(-24*time.Hour), "economy", "hash-c")
	insertProof(5, now.Add(-6*24*time.Hour), "stable", "stable-a")
	insertProof(6, now.Add(-24*time.Hour), "stable", "stable-a")

	sources, err := st.ListAutomaticWatchSources(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].GroupKey != "economy" || !sources[0].Changed ||
		sources[0].SemanticHash != "hash-c" || !sources[0].ChangedAt.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("automatic sources=%#v", sources)
	}
}

func TestWatchQueriesUseBoundedExistingIndexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openWatchStore(t)
	supplier := createWatchSupplier(t, st)
	assertPlan := func(query string, args []any, required ...string) {
		t.Helper()
		rows, err := st.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		plan := strings.Join(details, " | ")
		for _, value := range []string{"SCAN p", "SCAN price_observations"} {
			if strings.Contains(plan, value) {
				t.Fatalf("query plan %q contains unbounded %q", plan, value)
			}
		}
		for _, value := range required {
			if !strings.Contains(plan, value) {
				t.Fatalf("query plan %q missing %q", plan, value)
			}
		}
	}
	assertPlan(`SELECT p.attempt_id,p.source_group,p.source_semantic_hash,p.observed_at
		FROM collection_attempts AS a INDEXED BY idx_collection_attempts_supplier_time
		CROSS JOIN catalog_group_collection_proofs AS p
		WHERE a.supplier_id=? AND a.started_at>=? AND p.attempt_id=a.id AND p.supplier_id=a.supplier_id
		ORDER BY p.source_group,p.attempt_id`, []any{supplier.ID, formatTime(time.Now().UTC().Add(-7 * 24 * time.Hour))},
		"idx_collection_attempts_supplier_time", "catalog_group_collection_proofs")
	assertPlan(`SELECT price_json,observed_at FROM price_observations
		INDEXED BY idx_price_supplier_entity_time
		WHERE supplier_id=? AND entity_type='group' AND entity_key=?
		AND julianday(observed_at)<=julianday(?)
		ORDER BY julianday(observed_at) DESC,id DESC LIMIT 1`,
		[]any{supplier.ID, "economy", formatTime(time.Now().UTC())}, "idx_price_supplier_entity_time")
}

func TestWatchMigrationImportsLegacyFieldsWithoutChangingRollbackData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-watchlist.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "legacy upstream", BaseURL: "https://legacy-watch.example", Adapter: "newapi", Enabled: true,
		FocusGroups: []string{"economy"}, FocusModels: []string{"seedance"}}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE watch_items`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	items, err := migrated.ListWatchRecords(ctx)
	if err != nil || len(items) != 2 || !items[0].ManualPinned || !items[1].ManualPinned {
		t.Fatalf("migrated watch items=%#v err=%v", items, err)
	}
	unchanged, err := migrated.GetSupplier(ctx, supplier.ID)
	if err != nil || len(unchanged.FocusGroups) != 1 || unchanged.FocusGroups[0] != "economy" ||
		len(unchanged.FocusModels) != 1 || unchanged.FocusModels[0] != "seedance" {
		t.Fatalf("legacy rollback fields changed: %#v err=%v", unchanged, err)
	}
}
