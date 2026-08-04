package ratios

import (
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestEffectiveGroupRatioUsesExactDecimalArithmetic(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]struct {
		raw, factor, want string
	}{
		"cash discount":  {raw: "3", factor: "0.4", want: "1.2"},
		"legacy default": {raw: "3", factor: "", want: "3"},
		"identity":       {raw: "0.12", factor: "1", want: "0.12"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := EffectiveGroupRatio(fixture.raw, fixture.factor)
			if err != nil || got != fixture.want {
				t.Fatalf("EffectiveGroupRatio(%q,%q)=%q,%v want %q", fixture.raw, fixture.factor, got, err, fixture.want)
			}
		})
	}
}

func TestNormalizeRechargeValueFactorRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-0.4", "not-a-number", "0.1234567890123"} {
		if _, err := NormalizeRechargeValueFactor(value); err == nil {
			t.Fatalf("factor %q unexpectedly accepted", value)
		}
	}
}

func TestEnrichSnapshotPreservesObservedRatio(t *testing.T) {
	t.Parallel()
	raw := domain.CatalogSnapshot{Groups: []domain.GroupOffer{{ExternalID: "ccmax", Ratio: "3"}}}
	enriched, err := EnrichSnapshot(raw, "0.4")
	if err != nil {
		t.Fatal(err)
	}
	group := enriched.Groups[0]
	if group.Ratio != "3" || group.RawRatio != "3" || group.RechargeValueFactor != "0.4" || group.EffectiveRatio != "1.2" {
		t.Fatalf("enriched group=%#v", group)
	}
	if raw.Groups[0].EffectiveRatio != "" {
		t.Fatalf("source snapshot was mutated: %#v", raw.Groups[0])
	}
}
