package www

import (
	"testing"

	"shingocore/domain"
)

// TestProcessLabels: one rule for every board's Process column. An order
// serves the process of its line node, else of where it delivers, else of
// where it picks up; a node shared by two processes names both.
func TestProcessLabels(t *testing.T) {
	t.Parallel()
	p := processLabels{
		"ALN_001": {"SNF2"},
		"SMN_015": {"Bin Loader"},
		"SMN_001": {"SNF3", "SNF4"},
	}
	for _, tc := range []struct {
		name string
		o    domain.Order
		want string
	}{
		{"line order", domain.Order{ProcessNode: "ALN_001", SourceNode: "SMN_015", DeliveryNode: "ALN_001"}, "SNF2"},
		{"evac to a loader window", domain.Order{ProcessNode: "ALN_001", SourceNode: "ALN_001", DeliveryNode: "SMN_015"}, "SNF2"},
		{"loader refill, no line node", domain.Order{SourceNode: "AMR Supermarket", DeliveryNode: "SMN_015"}, "Bin Loader"},
		{"shared node", domain.Order{DeliveryNode: "SMN_001"}, "SNF3 / SNF4"},
		{"supermarket to supermarket", domain.Order{SourceNode: "SMN_005", DeliveryNode: "SMN_008"}, ""},
	} {
		if got := p.order(&tc.o); got != tc.want {
			t.Errorf("%s: process %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := processLabels(nil).node("ALN_001"); got != "" {
		t.Errorf("a failed read labels %q, want nothing", got)
	}
}
