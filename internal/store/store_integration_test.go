package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestOpenMigratesLegacyTargetSiteColumnsWithoutLosingRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE target_sites (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		base_url TEXT NOT NULL UNIQUE,
		adapter TEXT NOT NULL,
		credential_id INTEGER NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	stamp := "2026-07-20T10:00:00Z"
	if _, err := raw.Exec(`INSERT INTO target_sites(id,name,base_url,adapter,credential_id,enabled,created_at,updated_at)
		VALUES(1,'production','https://api.example.test','newapi',7,1,?,?)`, stamp, stamp); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated database: %v", err)
	}
	defer st.Close()
	target, err := st.GetTarget(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetTarget after migration: %v", err)
	}
	if target.Name != "production" || target.WriteMode != "approval" || target.LastStatus != "pending" || target.LastError != "" {
		t.Fatalf("migrated target = %#v", target)
	}
	for column, wantDefault := range map[string]string{"write_mode": "'approval'", "last_status": "'pending'", "last_error": "''"} {
		var found bool
		rows, err := st.db.Query(`PRAGMA table_info(target_sites)`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, kind string
			var defaultValue sql.NullString
			if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
				t.Fatal(err)
			}
			if name == column {
				found = true
				if !defaultValue.Valid || defaultValue.String != wantDefault {
					t.Errorf("%s default = %#v, want %q", column, defaultValue, wantDefault)
				}
			}
		}
		_ = rows.Close()
		if !found {
			t.Errorf("migrated column %s not found", column)
		}
	}
}

func TestOpenAddsPairingStatusBeforeCreatingStatusIndex(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-pairing.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE pairing_tokens (
		token_hash TEXT PRIMARY KEY,
		supplier_id INTEGER NOT NULL,
		expires_at TEXT NOT NULL,
		used_at TEXT
	)`)
	if err != nil {
		t.Fatalf("create legacy pairing schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy pairing schema: %v", err)
	}
	defer store.Close()
	var indexName string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_pairing_tokens_status_expiry'`).Scan(&indexName); err != nil {
		t.Fatalf("read pairing status index: %v", err)
	}
	if indexName != "idx_pairing_tokens_status_expiry" {
		t.Fatalf("pairing status index = %q", indexName)
	}
}

func TestOpenMigratesLegacyAuthCapabilityIdentityWithoutLosingRows(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-auth.db")
	initial, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	supplier := domain.Supplier{Name: "legacy auth", BaseURL: "https://legacy-auth.example", Enabled: true}
	if err := initial.CreateSupplier(context.Background(), &supplier); err != nil {
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`DELETE FROM schema_migrations WHERE version=2026072202`,
		`DROP TABLE auth_capabilities`,
		`CREATE TABLE auth_capabilities (
			supplier_id INTEGER NOT NULL REFERENCES suppliers(id) ON DELETE CASCADE,
			capability TEXT NOT NULL,status TEXT NOT NULL,credential_id INTEGER,
			endpoint_type TEXT NOT NULL DEFAULT '',http_status INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,error_class TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',last_attempt_at TEXT NOT NULL,verified_at TEXT,
			PRIMARY KEY(supplier_id,capability))`,
		`INSERT INTO auth_capabilities(supplier_id,capability,status,endpoint_type,http_status,last_attempt_at,verified_at)
			VALUES(1,'customer_session','verified','balance',200,'2026-07-22T09:00:00Z','2026-07-22T09:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated auth database: %v", err)
	}
	defer migrated.Close()
	items, err := migrated.ListAuthCapabilities(context.Background(), supplier.ID)
	if err != nil || len(items) != 1 || items[0].EndpointType != "balance" || items[0].Status != "verified" {
		t.Fatalf("migrated capabilities=%#v err=%v", items, err)
	}
	var endpointPK int
	rows, err := migrated.db.Query(`PRAGMA table_info(auth_capabilities)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "endpoint_type" {
			endpointPK = primaryKey
		}
	}
	_ = rows.Close()
	if endpointPK != 3 {
		t.Fatalf("endpoint_type primary key position=%d, want 3", endpointPK)
	}
}

func TestCoreEntityCRUDAndStateRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)

	credential := domain.Credential{Name: "admin", Kind: "access-token", SiteURL: "https://target.example"}
	if err := st.CreateCredential(ctx, &credential, "encrypted"); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	supplier := domain.Supplier{
		Name: "upstream", BaseURL: "https://upstream.example/", Enabled: true,
		FocusGroups: []string{"economy"}, FocusModels: []string{"gpt-5.6-sol"},
		Metadata: map[string]string{"region": "cn"},
	}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatalf("CreateSupplier: %v", err)
	}
	if supplier.Adapter != "auto" || supplier.PollIntervalSeconds != 300 || supplier.BaseURL != "https://upstream.example/" {
		t.Fatalf("supplier defaults = %#v", supplier)
	}
	storedSupplier, err := st.FindSupplierByBaseURL(ctx, "https://upstream.example/")
	if err != nil {
		t.Fatalf("FindSupplierByBaseURL: %v", err)
	}
	if storedSupplier.BaseURL != "https://upstream.example" || !reflect.DeepEqual(storedSupplier.FocusGroups, supplier.FocusGroups) || !reflect.DeepEqual(storedSupplier.Metadata, supplier.Metadata) {
		t.Fatalf("stored supplier = %#v", storedSupplier)
	}
	storedSupplier.Name = "renamed upstream"
	storedSupplier.PollIntervalSeconds = 60
	if err := st.UpdateSupplier(ctx, storedSupplier); err != nil {
		t.Fatalf("UpdateSupplier: %v", err)
	}
	updatedSupplier, _ := st.GetSupplier(ctx, supplier.ID)
	if updatedSupplier.Name != "renamed upstream" || updatedSupplier.PollIntervalSeconds != 60 {
		t.Fatalf("updated supplier = %#v", updatedSupplier)
	}

	target := domain.TargetSite{Name: "production", BaseURL: "https://target.example/", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if target.Adapter != "newapi" || target.WriteMode != "approval" || target.LastStatus != "pending" {
		t.Fatalf("target defaults = %#v", target)
	}
	if err := st.MarkTargetResult(ctx, target.ID, "healthy", ""); err != nil {
		t.Fatalf("MarkTargetResult: %v", err)
	}
	storedTarget, _ := st.GetTarget(ctx, target.ID)
	if storedTarget.LastStatus != "healthy" || storedTarget.BaseURL != "https://target.example" {
		t.Fatalf("stored target = %#v", storedTarget)
	}

	policy := domain.PricingPolicy{
		Name: "economy markup", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		TargetGroup: "economy", RuleType: "multiply", Value: "1.2", Enabled: true,
	}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if policy.SourceModelPattern != "*" || policy.ProfitBasis != "contribution" || policy.RoundingStep != "0.001" || policy.ExecutionMode != "approval" {
		t.Fatalf("policy defaults = %#v", policy)
	}
	policy.ExecutionMode = "auto"
	policy.MaximumRatio = "0.5"
	if err := st.UpdatePolicy(ctx, policy); err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	storedPolicy, _ := st.GetPolicy(ctx, policy.ID)
	if storedPolicy.ExecutionMode != "auto" || storedPolicy.MaximumRatio != "0.5" {
		t.Fatalf("stored policy = %#v", storedPolicy)
	}

	proposal := domain.ChangeProposal{
		PolicyID: policy.ID, TargetSiteID: target.ID, TargetGroup: "economy", ExpectedOld: "0.05",
		ProposedValue: "0.06", ExpectedHash: "hash-before", Status: "pending", Mode: "approval",
		IdempotencyKey: "proposal-1", Calculation: domain.PriceCalculation{TargetRatio: "0.06", Formula: "target = source * value"},
	}
	if err := st.CreateProposal(ctx, &proposal); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	byKey, err := st.GetProposalByIdempotency(ctx, "proposal-1")
	if err != nil || byKey.ID != proposal.ID || byKey.Calculation.Formula != proposal.Calculation.Formula {
		t.Fatalf("GetProposalByIdempotency = %#v, %v", byKey, err)
	}
	if err := st.UpdateProposalResult(ctx, proposal.ID, "applied", "0.06", ""); err != nil {
		t.Fatalf("UpdateProposalResult: %v", err)
	}
	applied, _ := st.GetProposal(ctx, proposal.ID)
	if applied.Status != "applied" || applied.ReadbackValue != "0.06" {
		t.Fatalf("applied proposal = %#v", applied)
	}
	if err := st.TransitionProposal(ctx, proposal.ID, []string{"pending", "failed"}, "rejected", "", "late reject"); err == nil {
		t.Fatal("terminal applied proposal accepted an invalid late rejection")
	}
	filtered, err := st.ListProposals(ctx, "applied", 10)
	if err != nil || len(filtered) != 1 || filtered[0].ID != proposal.ID {
		t.Fatalf("ListProposals(applied) = %#v, %v", filtered, err)
	}

	spec := domain.ConnectorSpec{Name: "custom", SchemaVersion: "1", SpecJSON: `{"schema_version":1}`, Enabled: true}
	if err := st.CreateConnectorSpec(ctx, &spec); err != nil {
		t.Fatalf("CreateConnectorSpec: %v", err)
	}
	specs, err := st.ListConnectorSpecs(ctx)
	if err != nil || len(specs) != 1 || !specs[0].Enabled || specs[0].SpecJSON != spec.SpecJSON {
		t.Fatalf("ListConnectorSpecs = %#v, %v", specs, err)
	}

	observed := time.Now().UTC().Add(-time.Minute)
	snapshot := domain.CatalogSnapshot{SupplierID: supplier.ID,
		Groups:   []domain.GroupOffer{{ExternalID: "economy", Name: "Economy", Ratio: "0.05", ModelIDs: []string{"gpt-5.6-sol"}}},
		Models:   []domain.ModelOffer{{ExternalID: "gpt-5.6-sol", Name: "gpt-5.6-sol", Group: "economy", BillingMode: "ratio", ModelRatio: "1"}},
		Balance:  &domain.BalanceState{Amount: "10", Available: "9", Currency: "USD", DailySpend: "1", ObservedAt: observed},
		Evidence: domain.Evidence{ObservedAt: observed, ResponseHash: "catalog-hash", SourceURL: "https://upstream.example/api/pricing"},
	}
	if err := st.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := st.UpsertDiscoveries(ctx, snapshot, false); err != nil {
		t.Fatalf("UpsertDiscoveries: %v", err)
	}
	latest, err := st.LatestSnapshot(ctx, supplier.ID)
	if err != nil || latest.Evidence.ResponseHash != "catalog-hash" || len(latest.Groups) != 1 {
		t.Fatalf("LatestSnapshot = %#v, %v", latest, err)
	}
	prices, err := st.LatestPrices(ctx, supplier.ID, 10)
	if err != nil || len(prices) != 2 {
		t.Fatalf("LatestPrices = %#v, %v", prices, err)
	}
	balance, err := st.LatestBalance(ctx, supplier.ID)
	if err != nil || balance.Available != "9" {
		t.Fatalf("LatestBalance = %#v, %v", balance, err)
	}
	discoveries, err := st.ListDiscoveries(ctx, true, 10)
	if err != nil || len(discoveries) != 1 || discoveries[0].ExternalID != "economy" {
		t.Fatalf("ListDiscoveries = %#v, %v", discoveries, err)
	}
	if err := st.AcknowledgeDiscovery(ctx, supplier.ID, "group", "economy"); err != nil {
		t.Fatalf("AcknowledgeDiscovery: %v", err)
	}
	remaining, _ := st.ListDiscoveries(ctx, true, 10)
	if len(remaining) != 0 {
		t.Fatalf("unacknowledged discoveries = %#v", remaining)
	}

	setting := map[string]any{"enabled": true, "threshold": 48}
	if err := st.SetSetting(ctx, "alerts", setting); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	var readSetting map[string]any
	if err := st.GetSetting(ctx, "alerts", &readSetting); err != nil || readSetting["enabled"] != true || readSetting["threshold"] != float64(48) {
		t.Fatalf("GetSetting = %#v, %v", readSetting, err)
	}
	if err := st.UpdatePolicy(ctx, domain.PricingPolicy{ID: 999}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing UpdatePolicy error = %v", err)
	}
}

func TestPairingTokenIsSingleUseAndExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	supplier := domain.Supplier{Name: "upstream", BaseURL: "https://upstream.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SavePairingToken(ctx, "single-use", supplier.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("SavePairingToken: %v", err)
	}
	id, err := st.PendingPairingSupplier(ctx, "single-use", now)
	if err != nil || id != supplier.ID {
		t.Fatalf("PendingPairingSupplier = %d, %v", id, err)
	}
	credential := domain.Credential{Name: "paired", Kind: "browser_pairing", SiteURL: supplier.BaseURL}
	if err := st.CreateCredentialFromPairing(ctx, "single-use", now, supplier.ID, &credential, "encrypted"); err != nil {
		t.Fatalf("CreateCredentialFromPairing: %v", err)
	}
	if credential.ID == 0 {
		t.Fatal("paired credential id was not assigned")
	}
	updated, err := st.GetSupplier(ctx, supplier.ID)
	if err != nil || updated.CredentialID == nil || *updated.CredentialID != credential.ID {
		t.Fatalf("paired supplier = %#v, %v", updated, err)
	}
	if err := st.CreateCredentialFromPairing(ctx, "single-use", now, supplier.ID, &domain.Credential{Name: "replay", Kind: "browser_pairing", SiteURL: supplier.BaseURL}, "encrypted"); err == nil {
		t.Fatal("second CreateCredentialFromPairing unexpectedly succeeded")
	}
	if err := st.SavePairingToken(ctx, "expired", supplier.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PendingPairingSupplier(ctx, "expired", now); err == nil {
		t.Fatal("expired pairing token unexpectedly succeeded")
	}
}

func TestCollectionLeaseIsAtomicAndRecoversAfterExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lease.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	supplier := domain.Supplier{Name: "leased", BaseURL: "https://lease.example", Enabled: true}
	if err := first.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 4, 0, 0, 0, time.UTC)
	if ok, err := first.TryAcquireCollectionLease(ctx, supplier.ID, "blue", now, time.Minute); err != nil || !ok {
		t.Fatalf("first lease = %v, %v", ok, err)
	}
	if ok, err := second.TryAcquireCollectionLease(ctx, supplier.ID, "green", now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("overlapping lease = %v, %v", ok, err)
	}
	if ok, err := second.TryAcquireCollectionLease(ctx, supplier.ID, "green", now.Add(61*time.Second), time.Minute); err != nil || !ok {
		t.Fatalf("expired lease recovery = %v, %v", ok, err)
	}
	if err := first.ReleaseCollectionLease(ctx, supplier.ID, "blue"); err != nil {
		t.Fatal(err)
	}
	if ok, err := first.TryAcquireCollectionLease(ctx, supplier.ID, "blue", now.Add(62*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("stale owner release removed active lease: ok=%v err=%v", ok, err)
	}
	if ok, err := second.RenewCollectionLease(ctx, supplier.ID, "green", now.Add(62*time.Second), time.Minute); err != nil || !ok {
		t.Fatalf("renew active lease = %v, %v", ok, err)
	}
}

func TestSnapshotAndBalanceHistoryAreCoalescedWithoutLosingTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	st.SetBalanceHeartbeat(time.Hour)
	supplier := domain.Supplier{Name: "history", BaseURL: "https://history.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 22, 5, 0, 0, 0, time.UTC)
	save := func(hash, ratio, available string, observed time.Time) {
		t.Helper()
		snapshot := historySnapshot(supplier.ID, hash, ratio, available, observed)
		if err := st.SaveSnapshot(ctx, snapshot); err != nil {
			t.Fatalf("SaveSnapshot(%s): %v", hash, err)
		}
	}
	save("A", "0.05", "9", base)
	save("A", "0.05", "9", base.Add(time.Minute))
	save("B", "0.08", "8", base.Add(2*time.Minute))
	save("A", "0.05", "8", base.Add(3*time.Minute))
	save("A", "0.05", "8", base.Add(2*time.Hour))

	assertTableCount(t, st, "catalog_snapshots", 2)
	assertTableCount(t, st, "price_observations", 6)
	assertTableCount(t, st, "balance_observations", 3)
	latest, err := st.LatestSnapshot(ctx, supplier.ID)
	if err != nil || latest.Evidence.ResponseHash != "A" || !latest.Evidence.ObservedAt.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("latest coalesced snapshot = %#v, %v", latest, err)
	}
}

func TestCleanupRetentionKeepsCurrentStateAndDeletesExpiredHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIntegrationStore(t)
	st.SetBalanceHeartbeat(0)
	now := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	first := domain.Supplier{Name: "active history", BaseURL: "https://retention-a.example", Enabled: true}
	second := domain.Supplier{Name: "stale only", BaseURL: "https://retention-b.example", Enabled: true}
	if err := st.CreateSupplier(ctx, &first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSupplier(ctx, &second); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []domain.CatalogSnapshot{
		historySnapshot(first.ID, "old", "0.04", "10", now.Add(-10*24*time.Hour)),
		historySnapshot(first.ID, "current", "0.05", "9", now.Add(-time.Hour)),
		historySnapshot(second.ID, "stale-current", "0.06", "8", now.Add(-10*24*time.Hour)),
	} {
		if err := st.SaveSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	oldID, currentID := first.ID, second.ID
	if err := st.AddActivity(ctx, domain.Activity{Kind: "old", Severity: "info", Title: "old", Message: "old", SupplierID: &oldID, CreatedAt: now.Add(-10 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddActivity(ctx, domain.Activity{Kind: "current", Severity: "info", Title: "current", Message: "current", SupplierID: &currentID, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePairingToken(ctx, "expired", first.ID, now.Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePairingToken(ctx, "recent", first.ID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := st.CleanupRetention(ctx, now, RetentionPolicy{
		CatalogSnapshots: 24 * time.Hour, PriceObservations: 24 * time.Hour,
		BalanceObservations: 24 * time.Hour, Activities: 24 * time.Hour, PairingTokens: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("CleanupRetention: %v", err)
	}
	if result.CatalogSnapshots != 1 || result.PriceObservations != 2 || result.BalanceObservations != 1 || result.Activities != 1 || result.PairingTokens != 1 {
		t.Fatalf("cleanup result = %#v", result)
	}
	assertTableCount(t, st, "catalog_snapshots", 2)
	assertTableCount(t, st, "price_observations", 4)
	assertTableCount(t, st, "balance_observations", 2)
	assertTableCount(t, st, "activities", 1)
	assertTableCount(t, st, "pairing_tokens", 1)
	if _, err := st.LatestSnapshot(ctx, second.ID); err != nil {
		t.Fatalf("stale supplier lost its current snapshot: %v", err)
	}
	if _, err := st.CleanupRetention(ctx, now, RetentionPolicy{Activities: -time.Hour}); err == nil {
		t.Fatal("negative retention unexpectedly accepted")
	}
}

func historySnapshot(supplierID int64, hash, ratio, available string, observed time.Time) domain.CatalogSnapshot {
	return domain.CatalogSnapshot{
		SupplierID: supplierID,
		Groups:     []domain.GroupOffer{{ExternalID: "economy", Name: "economy", Ratio: ratio, ModelIDs: []string{"model"}}},
		Models:     []domain.ModelOffer{{ExternalID: "model", Name: "model", Group: "economy", BillingMode: "ratio", ModelRatio: "1"}},
		Balance:    &domain.BalanceState{Amount: available, Available: available, Currency: "USD", ObservedAt: observed},
		Evidence:   domain.Evidence{ObservedAt: observed, ResponseHash: hash, SourceURL: "https://history.example/api/pricing"},
	}
}

func assertTableCount(t *testing.T, st *Store, table string, want int) {
	t.Helper()
	var got int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func TestDiscoveryBaselineIsPreservedAndSeparatedFromNewGroups(t *testing.T) {
	st := openIntegrationStore(t)
	ctx := context.Background()
	supplier := domain.Supplier{Name: "baseline", BaseURL: "https://baseline.example", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	baseline := domain.CatalogSnapshot{SupplierID: supplier.ID, Groups: []domain.GroupOffer{{ExternalID: "existing", Name: "Existing"}}, Evidence: domain.Evidence{ObservedAt: observed}}
	if err := st.UpsertDiscoveries(ctx, baseline, true); err != nil {
		t.Fatal(err)
	}
	second := domain.CatalogSnapshot{SupplierID: supplier.ID, Groups: []domain.GroupOffer{{ExternalID: "existing", Name: "Existing"}, {ExternalID: "new", Name: "New"}}, Evidence: domain.Evidence{ObservedAt: observed.Add(time.Minute)}}
	if err := st.UpsertDiscoveries(ctx, second, false); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListDiscoveries(ctx, true, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("discoveries = %#v, %v", items, err)
	}
	states := map[string]bool{}
	for _, item := range items {
		states[item.ExternalID] = item.Baseline
	}
	if !states["existing"] || states["new"] {
		t.Fatalf("baseline states = %#v", states)
	}
}

func openIntegrationStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
