package decimalnorm

import "testing"

func TestFunctionalCollapsesObservedFloatNoise(t *testing.T) {
	tests := map[string]string{
		"30.000000000024":    "30",
		"30.100000400000003": "30.1000004",
		"0.5999999999994":    "0.6",
		"0.7000000000000015": "0.7",
		"10.900000000000002": "10.9",
		"0.0000000000001":    "0.0000000000001",
		"0.123456789":        "0.123456789",
		"30.1002":            "30.1002",
	}
	for raw, want := range tests {
		got, err := String(raw)
		if err != nil || got != want {
			t.Errorf("String(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestEqualKeepsRealPriceChanges(t *testing.T) {
	if !Equal("30.100000400000003", "30.1000004") || !Equal("0.5999999999994", "0.6") {
		t.Fatal("float round-trip noise was treated as a price change")
	}
	if Equal("30.1000004", "30.1002") || Equal("0.6", "0.600001") || Equal("0", "0.0000000000001") {
		t.Fatal("a real price change was collapsed")
	}
}
