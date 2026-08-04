package connectors

import (
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func sub2CatalogProof(snapshot domain.CatalogSnapshot, diagnostics domain.CollectionDiagnostics,
	mode string, credentialAttached bool) *domain.CatalogCollectionProofBasis {
	if len(snapshot.Groups) == 0 || len(snapshot.Models) == 0 || snapshot.Evidence.ResponseHash == "" {
		return nil
	}
	parsed := len(snapshot.Groups) + len(snapshot.Models)
	identities := make(map[string]struct{}, parsed)
	ambiguous := 0
	unitErrors := 0
	coverageUncertain := false
	for _, group := range snapshot.Groups {
		groupID := strings.TrimSpace(group.ExternalID)
		if groupID == "" {
			groupID = strings.TrimSpace(group.Name)
		}
		if groupID == "" {
			ambiguous++
		}
		identities["group\x00"+groupID] = struct{}{}
		if group.Metadata["catalog_incomplete"] == "true" ||
			(group.Metadata["model_coverage"] != "" && group.Metadata["model_coverage"] != "known") {
			coverageUncertain = true
		}
	}
	for _, model := range snapshot.Models {
		modelID := strings.TrimSpace(model.ExternalID)
		if modelID == "" {
			modelID = strings.TrimSpace(model.Name)
		}
		if modelID == "" || strings.TrimSpace(model.Group) == "" {
			ambiguous++
		}
		identities["model\x00"+strings.TrimSpace(model.Group)+"\x00"+modelID] = struct{}{}
		price := model.PriceFunction
		if price.RequiresModelBasePrice || (price.InputPer1M == "" && price.OutputPer1M == "" &&
			price.PerRequest == "" && price.PerImage == "" && price.PerSecond == "" &&
			price.PerFrame == "" && price.FixedFee == "" && price.Expression == "") {
			unitErrors++
		}
	}
	for _, warning := range diagnostics.Warnings {
		lower := strings.ToLower(warning)
		for _, marker := range []string{"bounded to", "catalog returned no recognized models",
			"no active customer key produced", "model catalog is temporarily unavailable"} {
			if strings.Contains(lower, marker) {
				coverageUncertain = true
			}
		}
	}
	dropped := 0
	if coverageUncertain {
		dropped = 1
	}
	raw := parsed + dropped
	return &domain.CatalogCollectionProofBasis{
		AuthoritativeEndpoint:  "sub2api:" + strings.TrimSpace(mode) + ":catalog",
		AuthenticationRequired: true, Authenticated: credentialAttached,
		SourceRevision: snapshot.Evidence.ResponseHash,
		Pages:          []domain.CatalogCollectionPage{{PageNumber: 1, ItemCount: raw, Terminal: true}},
		RawItemCount:   raw, ParsedItemCount: parsed, UniqueItemCount: len(identities),
		DroppedItemCount: dropped, UnitErrorCount: unitErrors, AmbiguousIdentityCount: ambiguous,
		ObservedAt: snapshot.Evidence.ObservedAt,
	}
}
