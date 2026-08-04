package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const validConnectorSpecJSON = `{"schema_version":1,"name":"test-connector","endpoints":[{"name":"catalog","method":"GET","path":"/api/catalog","groups":{"items_path":"data.groups","external_id":{"path":"id","required":true},"name":{"path":"name","required":true}}}]}`

func TestConnectorSpecManagementRejectsStaleUpdatesAndReferencedDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	spec := domain.ConnectorSpec{Name: "custom catalog", SchemaVersion: "1", SpecJSON: validConnectorSpecJSON, Enabled: true}
	if err := st.CreateConnectorSpec(ctx, &spec); err != nil {
		t.Fatal(err)
	}
	originalVersion := spec.UpdatedAt
	spec.Name = "custom catalog v2"
	if err := st.UpdateConnectorSpec(ctx, &spec, originalVersion); err != nil {
		t.Fatal(err)
	}
	stale := spec
	stale.Name = "stale update"
	if err := st.UpdateConnectorSpec(ctx, &stale, originalVersion); !errors.Is(err, ErrConnectorSpecConflict) {
		t.Fatalf("stale update error = %v", err)
	}

	supplier := domain.Supplier{Name: "bound supplier", BaseURL: "https://bound.example", Adapter: "auto", Enabled: true,
		Metadata: map[string]string{"connector_spec_id": strconv.FormatInt(spec.ID, 10)}}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	impact, err := st.PreviewConnectorSpecDeletion(ctx, spec.ID)
	if err != nil || len(impact.SupplierRefs) != 1 || impact.SupplierRefs[0].ID != supplier.ID {
		t.Fatalf("impact=%#v err=%v", impact, err)
	}
	if _, err := st.DeleteConnectorSpecSafely(ctx, spec.ID, spec.Name, impact.ImpactHash); !errors.Is(err, ErrConnectorSpecConflict) {
		t.Fatalf("referenced delete error = %v", err)
	}

	supplier.Metadata = map[string]string{}
	if err := st.UpdateSupplier(ctx, supplier); err != nil {
		t.Fatal(err)
	}
	impact, err = st.PreviewConnectorSpecDeletion(ctx, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteConnectorSpecSafely(ctx, spec.ID, spec.Name, impact.ImpactHash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetConnectorSpec(ctx, spec.ID); !IsNotFound(err) {
		t.Fatalf("deleted spec lookup error = %v", err)
	}
	activities, err := st.RecentActivity(ctx, 10)
	if err != nil || len(activities) == 0 || activities[0].Kind != "connector_spec_deleted" {
		t.Fatalf("activities=%#v err=%v", activities, err)
	}
	if strings.Contains(strings.ToLower(activities[0].Message), "secret") {
		t.Fatalf("audit leaked unexpected payload: %#v", activities[0])
	}
}

func TestConnectorManifestManagementRejectsStaleUpdatesAndBoundDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "relayops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	record := testConnectorManifestRecord("manifest-one", "api.example.com")
	if err := st.CreateConnectorManifest(ctx, &record); err != nil {
		t.Fatal(err)
	}
	originalVersion := record.UpdatedAt
	record.Name = "manifest-two"
	record.Manifest.Name = record.Name
	if err := st.UpdateConnectorManifest(ctx, &record, originalVersion); err != nil {
		t.Fatal(err)
	}
	stale := record
	stale.Name = "manifest-stale"
	if err := st.UpdateConnectorManifest(ctx, &stale, originalVersion); !errors.Is(err, ErrConnectorManifestConflict) {
		t.Fatalf("stale manifest update error = %v", err)
	}

	supplier := domain.Supplier{Name: "manifest supplier", BaseURL: "https://api.example.com", Adapter: "newapi", Enabled: true}
	if err := st.CreateSupplier(ctx, &supplier); err != nil {
		t.Fatal(err)
	}
	if err := st.BindSupplierConnectorManifest(ctx, supplier.ID, record.ID, supplier.BaseURL, record.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	impact, err := st.PreviewConnectorManifestDeletion(ctx, record.ID)
	if err != nil || len(impact.SupplierRefs) != 1 {
		t.Fatalf("impact=%#v err=%v", impact, err)
	}
	if _, err := st.DeleteConnectorManifestSafely(ctx, record.ID, record.Name, impact.ImpactHash); !errors.Is(err, ErrConnectorManifestConflict) {
		t.Fatalf("bound manifest delete error = %v", err)
	}
	if err := st.UnbindSupplierConnectorManifest(ctx, supplier.ID); err != nil {
		t.Fatal(err)
	}
	impact, err = st.PreviewConnectorManifestDeletion(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteConnectorManifestSafely(ctx, record.ID, record.Name, impact.ImpactHash); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetConnectorManifest(ctx, record.ID); !IsNotFound(err) {
		t.Fatalf("deleted manifest lookup error = %v", err)
	}
}

func testConnectorManifestRecord(name, host string) domain.ConnectorManifestRecord {
	return domain.ConnectorManifestRecord{
		Name: name, SchemaVersion: domain.ConnectorManifestSchemaV1, AdapterFamily: "newapi", Source: "test", ValidationStatus: "valid",
		Manifest: domain.ConnectorManifest{SchemaVersion: domain.ConnectorManifestSchemaV1, Name: name, AdapterFamily: "newapi",
			Origins:      domain.ConnectorManifestOrigins{Schemes: []string{"https"}, Hosts: []string{host}},
			Capabilities: map[string]domain.ConnectorManifestCapability{"status": {Paths: []string{"/api/status"}, Contract: "native"}}},
	}
}
