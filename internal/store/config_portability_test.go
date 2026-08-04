package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestPortableConfigExportApplyAndConditionalRollback(t *testing.T) {
	ctx := context.Background()
	source := openPortabilityTestStore(t)
	credential := createPortabilityCredential(t, source, "upstream login", "browser_pairing", "https://upstream.example", "encrypted-secret-payload")
	targetCredential := createPortabilityCredential(t, source, "target admin", "access-token", "https://target.example", "target-encrypted-secret")
	connector := domain.ConnectorSpec{Name: "custom", SchemaVersion: "1", Enabled: true, SpecJSON: validPortableConnectorSpec("custom")}
	if err := source.CreateConnectorSpec(ctx, &connector); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"connector_spec_id": intString(connector.ID), "catalog_visibility": "private",
		"api_key_management_path": "/keys", "api_key": "must-not-export"}
	supplier := domain.Supplier{Name: "Upstream", BaseURL: "https://upstream.example", Adapter: "jsonspec", CredentialID: &credential.ID,
		Enabled: true, PollIntervalSeconds: 60, FocusGroups: []string{"economy"}, Metadata: metadata}
	if err := source.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "Target", BaseURL: "https://target.example", Adapter: "newapi", CredentialID: targetCredential.ID, Enabled: true, WriteMode: "approval"}
	if err := source.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "Follow price", SupplierID: supplier.ID, SourceGroup: "economy", TargetSiteID: target.ID,
		TargetGroup: "economy", RuleType: "multiplier", Value: "1.2", Enabled: true}
	if err := source.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	if err := source.SetSetting(ctx, "alerts", map[string]any{"enabled": true, "hours": 48}); err != nil {
		t.Fatal(err)
	}
	if err := source.SetSetting(ctx, "internal.token", "must-not-export"); err != nil {
		t.Fatal(err)
	}

	bundle, _, err := source.ExportPortableConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(bundle)
	for _, forbidden := range []string{"encrypted-secret-payload", "must-not-export", `"credential_id"`, `"api_key"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("export leaked %q: %s", forbidden, encoded)
		}
	}
	if len(bundle.CredentialRefs) != 2 || len(bundle.Settings) != 1 || bundle.Suppliers[0].ConnectorSpec != "custom" {
		t.Fatalf("unexpected export: %#v", bundle)
	}
	if bundle.Suppliers[0].Metadata["api_key_management_path"] != "/keys" {
		t.Fatalf("key-management path was not preserved: %#v", bundle.Suppliers[0].Metadata)
	}

	destination := openPortabilityTestStore(t)
	createPortabilityCredential(t, destination, "upstream login", "browser_pairing", "https://upstream.example", "different-local-secret")
	createPortabilityCredential(t, destination, "target admin", "access-token", "https://target.example", "different-target-secret")
	preview, err := destination.PreviewPortableConfig(ctx, domain.ConfigImportPreviewRequest{Mode: domain.ConfigImportAdd, Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Create < 5 || preview.Summary.Unresolved != 0 {
		t.Fatalf("preview summary = %#v, actions=%#v", preview.Summary, preview.Actions)
	}
	run, err := destination.ApplyPortableConfig(ctx, preview.ID, preview.PreviewHash)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "applied" || run.AfterHash == run.BeforeHash {
		t.Fatalf("run = %#v", run)
	}
	if targets, _ := destination.ListTargets(ctx); len(targets) != 1 || !targets[0].Enabled {
		t.Fatalf("imported targets = %#v", targets)
	}
	if policies, _ := destination.ListPolicies(ctx); len(policies) != 1 || !policies[0].Enabled {
		t.Fatalf("imported policies = %#v", policies)
	}
	rolledBack, err := destination.RollbackPortableConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Status != "rolled_back" {
		t.Fatalf("rollback = %#v", rolledBack)
	}
	if suppliers, _ := destination.ListSuppliers(ctx); len(suppliers) != 0 {
		t.Fatalf("suppliers survived rollback: %#v", suppliers)
	}
	if targets, _ := destination.ListTargets(ctx); len(targets) != 0 {
		t.Fatalf("targets survived rollback: %#v", targets)
	}
}

func TestPortableConfigUnresolvedCredentialDisablesAndCASRejectsDrift(t *testing.T) {
	ctx := context.Background()
	st := openPortabilityTestStore(t)
	credential := domain.CredentialReference{Name: "missing", Kind: "api-key", SiteURL: "https://upstream.example"}
	targetCredential := domain.CredentialReference{Name: "missing target", Kind: "access-token", SiteURL: "https://target.example"}
	bundle := domain.PortableConfig{SchemaVersion: domain.PortableConfigSchemaVersion,
		CredentialRefs: []domain.CredentialReference{credential, targetCredential},
		Suppliers:      []domain.PortableSupplier{{Name: "Supplier", BaseURL: "https://upstream.example", Adapter: "newapi", Credential: &credential, Enabled: true, PollIntervalSeconds: 60}},
		Targets:        []domain.PortableTarget{{Name: "Target", BaseURL: "https://target.example", Adapter: "newapi", Credential: targetCredential, Enabled: true, WriteMode: "approval"}},
		PricingPolicies: []domain.PortablePricingPolicy{{Name: "Policy", Supplier: domain.PortableObjectReference{Name: "Supplier", BaseURL: "https://upstream.example"},
			Target: domain.PortableObjectReference{Name: "Target", BaseURL: "https://target.example"}, TargetGroup: "economy", RuleType: "multiplier", Value: "1.1", Enabled: true}},
	}
	preview, err := st.PreviewPortableConfig(ctx, domain.ConfigImportPreviewRequest{Mode: domain.ConfigImportAdd, Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Summary.Unresolved < 3 || preview.Summary.Disabled < 3 {
		t.Fatalf("unresolved preview = %#v actions=%#v", preview.Summary, preview.Actions)
	}
	run, err := st.ApplyPortableConfig(ctx, preview.ID, preview.PreviewHash)
	if err != nil {
		t.Fatal(err)
	}
	if targets, _ := st.ListTargets(ctx); len(targets) != 0 {
		t.Fatalf("unresolved target was created: %#v", targets)
	}
	if suppliers, _ := st.ListSuppliers(ctx); len(suppliers) != 1 || suppliers[0].Enabled {
		t.Fatalf("unresolved supplier = %#v", suppliers)
	}
	if run.Summary.Unresolved < 3 {
		t.Fatalf("run summary = %#v", run.Summary)
	}

	second, err := st.PreviewPortableConfig(ctx, domain.ConfigImportPreviewRequest{Mode: domain.ConfigImportOverwrite, Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "alerts", map[string]bool{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyPortableConfig(ctx, second.ID, second.PreviewHash); !errors.Is(err, ErrConfigCASConflict) {
		t.Fatalf("CAS error = %v", err)
	}
}

func TestPortableConfigRedactsStaticConnectorCredential(t *testing.T) {
	st := openPortabilityTestStore(t)
	spec := domain.ConnectorSpec{Name: "unsafe", SchemaVersion: "1", Enabled: true,
		SpecJSON: `{"schema_version":1,"name":"unsafe","endpoints":[{"name":"catalog","method":"GET","path":"/api/pricing","headers":{"X-API-Key":{"static":"sk-sensitive-value"}},"models":{"items_path":"data","external_id":{"path":"id"},"name":{"path":"name"}}}]}`}
	if err := st.CreateConnectorSpec(context.Background(), &spec); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := st.ExportPortableConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(bundle)
	if strings.Contains(string(encoded), "sk-sensitive-value") || !strings.Contains(string(encoded), "credential_ref") {
		t.Fatalf("connector credential was not safely redacted: %s", encoded)
	}
	if len(bundle.ConnectorSpecs) != 1 || bundle.ConnectorSpecs[0].Enabled || len(bundle.ConnectorSpecs[0].DisabledReasons) == 0 {
		t.Fatalf("sanitized connector state = %#v", bundle.ConnectorSpecs)
	}
	if err := ValidatePortableConfig(bundle); err != nil {
		t.Fatalf("sanitized export cannot be re-imported: %v", err)
	}
}

func TestPortableConfigConflictModesAreObjectLevelAndExplicit(t *testing.T) {
	ctx := context.Background()
	st := openPortabilityTestStore(t)
	credential := createPortabilityCredential(t, st, "login", "api-key", "https://same.example", "encrypted")
	supplier := domain.Supplier{Name: "Same", BaseURL: "https://same.example", Adapter: "newapi", CredentialID: &credential.ID, Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	target := domain.TargetSite{Name: "Same target", BaseURL: "https://same.example", Adapter: "newapi", CredentialID: credential.ID, Enabled: true}
	if err := st.CreateTarget(ctx, &target); err != nil {
		t.Fatal(err)
	}
	policy := domain.PricingPolicy{Name: "Same policy", SupplierID: supplier.ID, TargetSiteID: target.ID, TargetGroup: "same", RuleType: "multiplier", Value: "1", Enabled: true}
	if err := st.CreatePolicy(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := st.ExportPortableConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[domain.ConfigImportMode]map[string]string{
		domain.ConfigImportAdd:       {"supplier": "reuse", "target": "reuse", "pricing_policy": "reuse"},
		domain.ConfigImportOverwrite: {"supplier": "overwrite", "target": "overwrite", "pricing_policy": "overwrite"},
		domain.ConfigImportCopy:      {"supplier": "reuse", "target": "reuse", "pricing_policy": "copy"},
	}
	for mode, expected := range checks {
		preview, err := st.PreviewPortableConfig(ctx, domain.ConfigImportPreviewRequest{Mode: mode, Bundle: bundle})
		if err != nil {
			t.Fatalf("preview %s: %v", mode, err)
		}
		actual := make(map[string]string)
		for _, action := range preview.Actions {
			actual[action.ObjectType] = action.Action
		}
		for objectType, want := range expected {
			if actual[objectType] != want {
				t.Fatalf("mode %s %s action = %q, want %q; actions=%#v", mode, objectType, actual[objectType], want, preview.Actions)
			}
		}
	}
}

func openPortabilityTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createPortabilityCredential(t *testing.T, st *Store, name, kind, siteURL, payload string) domain.Credential {
	t.Helper()
	item := domain.Credential{Name: name, Kind: kind, SiteURL: siteURL}
	if err := st.CreateCredential(context.Background(), &item, payload); err != nil {
		t.Fatal(err)
	}
	return item
}

func validPortableConnectorSpec(name string) string {
	return `{"schema_version":1,"name":"` + name + `","endpoints":[{"name":"catalog","method":"GET","path":"/api/pricing","headers":{"Accept":{"static":"application/json"}},"models":{"items_path":"data","external_id":{"path":"id"},"name":{"path":"name"}}}]}`
}

func intString(value int64) string { return strconv.FormatInt(value, 10) }
