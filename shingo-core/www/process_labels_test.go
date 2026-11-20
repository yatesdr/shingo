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
		// A plain order names its press too (protocol.OrderRequest.ProcessNode):
		// a retrieve to a press's outside position, which no claim names, reads
		// as the press's process, where naming none it read as nobody's.
		{"plain retrieve to a press's outside, naming the press", domain.Order{ProcessNode: "ALN_001", SourceNode: "SMN_005", DeliveryNode: "ALN_001_OUT"}, "SNF2"},
		{"the same retrieve naming no press", domain.Order{SourceNode: "SMN_005", DeliveryNode: "ALN_001_OUT"}, ""},
		{"loader refill, no line node", domain.Order{SourceNode: "AMR Supermarket", DeliveryNode: "SMN_015"}, "Bin Loader"},
		{"shared node", domain.Order{DeliveryNode: "SMN_001"}, "SNF3 / SNF4"},
		{"supermarket to supermarket", domain.Order{SourceNode: "SMN_005", DeliveryNode: "SMN_008"}, ""},
	} {
		if got := p.order(&tc.o); got != tc.want {
			t.Errorf("%s: process %q, want %q", tc.name, got, tc.want)
		}
	}
	// A Bins-table row: a bin at a node is its node's process; a bin in flight
	// is the process of the order carrying it, never of _TRANSIT.
	atNode := &domain.Bin{NodeName: "SMN_001"}
	carried := &domain.Bin{NodeName: domain.TransitNodeName}
	line := &domain.Order{DeliveryNode: "ALN_001"}
	if got := p.bin(atNode, nil); got != "SNF3 / SNF4" {
		t.Errorf("bin at a node: process %q, want %q", got, "SNF3 / SNF4")
	}
	if got := p.bin(carried, line); got != "SNF2" {
		t.Errorf("bin in flight to ALN_001: process %q, want %q", got, "SNF2")
	}
	if got := p.bin(carried, nil); got != "" {
		t.Errorf("bin in flight with no order: process %q, want nothing", got)
	}
	if got := processLabels(nil).node("ALN_001"); got != "" {
		t.Errorf("a failed read labels %q, want nothing", got)
	}
}
