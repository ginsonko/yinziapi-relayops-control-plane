package automation

import (
	"strings"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/pricing"
)

func TestCoverageEligibilityModes(t *testing.T) {
	tests := []struct {
		name        string
		policy      domain.PricingPolicy
		impact      pricing.PricingImpact
		wantAllowed bool
		wantReason  string
	}{
		{
			name:        "default threshold accepts twenty of twenty four dimensions",
			policy:      domain.PricingPolicy{CoverageMode: "threshold", MinimumCoverage: "0.8"},
			impact:      pricing.PricingImpact{CoverageRatio: "0.8333333333333333", ComparableDimensions: 20, TotalDimensions: 24},
			wantAllowed: true,
			wantReason:  "已达到规则门槛",
		},
		{
			name:        "threshold rejects seventy nine percent",
			policy:      domain.PricingPolicy{CoverageMode: "threshold", MinimumCoverage: "0.8"},
			impact:      pricing.PricingImpact{CoverageRatio: "0.79", ComparableDimensions: 79, TotalDimensions: 100},
			wantAllowed: false,
			wantReason:  "低于规则门槛",
		},
		{
			name:        "strict rejects any unknown dimension",
			policy:      domain.PricingPolicy{CoverageMode: "strict", MinimumCoverage: "0.8"},
			impact:      pricing.PricingImpact{CoverageRatio: "0.99", ComparableDimensions: 99, TotalDimensions: 100},
			wantAllowed: false,
			wantReason:  "要求所有相关价格维度完整",
		},
		{
			name:        "owner override accepts partial evidence",
			policy:      domain.PricingPolicy{CoverageMode: "owner_override", MinimumCoverage: "0.8"},
			impact:      pricing.PricingImpact{CoverageRatio: "0.01", ComparableDimensions: 1, TotalDimensions: 100},
			wantAllowed: true,
			wantReason:  "所有者已明确接受",
		},
		{
			name:        "owner override cannot invent a comparable cost",
			policy:      domain.PricingPolicy{CoverageMode: "owner_override", MinimumCoverage: "0.8"},
			impact:      pricing.PricingImpact{CoverageRatio: "0", ComparableDimensions: 0, TotalDimensions: 100},
			wantAllowed: false,
			wantReason:  "没有任何可比成本维度",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, reason := coverageEligibility(test.policy, test.impact)
			if allowed != test.wantAllowed {
				t.Fatalf("allowed = %v, want %v; reason=%q", allowed, test.wantAllowed, reason)
			}
			if !strings.Contains(reason, test.wantReason) {
				t.Fatalf("reason = %q, want substring %q", reason, test.wantReason)
			}
		})
	}
}
