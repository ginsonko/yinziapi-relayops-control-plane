package connectors

import (
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSub2CatalogProofDistinguishesCompleteAndUncertainCatalogs(t *testing.T) {
	observed := time.Date(2026, 7, 28, 5, 45, 0, 0, time.UTC)
	snapshot := domain.CatalogSnapshot{
		Groups: []domain.GroupOffer{{ExternalID: "video", Name: "Video"}},
		Models: []domain.ModelOffer{{ExternalID: "jimeng-video", Name: "jimeng-video", Group: "video",
			PriceFunction: domain.PriceFunction{Currency: "USD", PerRequest: "1"}}},
		Evidence: domain.Evidence{ResponseHash: "response-hash", ObservedAt: observed},
	}

	complete := sub2CatalogProof(snapshot, domain.CollectionDiagnostics{}, "authenticated", true)
	if complete == nil {
		t.Fatal("complete catalog proof was not produced")
	}
	if !complete.Authenticated || complete.RawItemCount != 2 || complete.ParsedItemCount != 2 ||
		complete.UniqueItemCount != 2 || complete.DroppedItemCount != 0 || complete.UnitErrorCount != 0 ||
		len(complete.Pages) != 1 || !complete.Pages[0].Terminal {
		t.Fatalf("complete proof basis = %#v", complete)
	}

	uncertain := sub2CatalogProof(snapshot, domain.CollectionDiagnostics{
		Warnings: []string{"catalog returned no recognized models on an optional endpoint"},
	}, "authenticated", true)
	if uncertain == nil || uncertain.DroppedItemCount != 1 ||
		uncertain.RawItemCount != uncertain.ParsedItemCount+1 {
		t.Fatalf("uncertain proof basis = %#v", uncertain)
	}
}

func TestSub2CatalogProofRejectsUnpricedOrUnauthenticatedEvidence(t *testing.T) {
	snapshot := domain.CatalogSnapshot{
		Groups: []domain.GroupOffer{{ExternalID: "g", Name: "G"}},
		Models: []domain.ModelOffer{{ExternalID: "m", Name: "m", Group: "g",
			PriceFunction: domain.PriceFunction{Currency: "USD", RequiresModelBasePrice: true}}},
		Evidence: domain.Evidence{ResponseHash: "response-hash", ObservedAt: time.Now().UTC()},
	}
	proof := sub2CatalogProof(snapshot, domain.CollectionDiagnostics{}, "public", false)
	if proof == nil || proof.Authenticated || proof.UnitErrorCount != 1 {
		t.Fatalf("unsafe proof basis = %#v", proof)
	}
}
