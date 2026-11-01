package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/store"
)

// kanban_pins_test.go — characterization pins with no database.

// TestKanbanPin_LandedReceiptConfirmsTwoPrefixesOnly: Core confirms a landed move
// of its own two window pulls, by prefix, and no other order it asked for: every
// other ask waits for its receipt or the auto-confirm sweep.
func TestKanbanPin_LandedReceiptConfirmsTwoPrefixesOnly(t *testing.T) {
	t.Parallel()
	for uuid, want := range map[string]string{
		dispatch.Stage2EdgeUUIDPrefix + "1": stage2Receipt,
		dispatch.DrainEdgeUUIDPrefix + "1":  drainReceipt,
		"core-mnt-1":                        "",
		"core-l1-1":                         "",
		"edge-order-1":                      "",
		"":                                  "",
	} {
		if got := landedReceipt(uuid); got != want {
			t.Errorf("landedReceipt(%q) = %q, want %q", uuid, got, want)
		}
	}
	if dispatch.Stage2EdgeUUIDPrefix != "core-s2-" || dispatch.DrainEdgeUUIDPrefix != "core-dr-" {
		t.Errorf("prefixes %q and %q, want core-s2- and core-dr-", dispatch.Stage2EdgeUUIDPrefix, dispatch.DrainEdgeUUIDPrefix)
	}
}

// TestKanbanPin_WalkLightsNothingOutsideItsOwnGroup: the walk's whole world is
// its group's lines. A 32 x 30 cart at a line declared at 0, its own floor having
// no other line, is assigned nothing; a 45 x 48 cart with its group whole is
// assigned nothing, wherever it could otherwise be repaid. The assignment names a
// type and a height and says nothing of what a panel can show: the panel half
// (a choice with no button lights nothing) is the Edge's clearHighlight/isLit.
func TestKanbanPin_WalkLightsNothingOutsideItsOwnGroup(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KP-32X30", Want: 0, Have: 0},
		{Code: "KP-BIG", Want: 1, Have: 1},
		{Code: "KP-SMALL", Want: 1, Have: 1},
		{Code: "KP-TOTES", Want: 1, Have: 1},
	}
	sizes := map[string]domain.FloorSize{}
	for code, wl := range map[string][2]float64{
		"KP-32X30": {32, 30}, "KP-BIG": {45, 48}, "KP-SMALL": {45, 48}, "KP-TOTES": {48, 45},
	} {
		fs, ok := domain.BinType{WidthIn: wl[0], LengthIn: wl[1]}.FloorSize()
		if !ok {
			t.Fatalf("no floor size for %s", code)
		}
		sizes[code] = fs
	}
	windows := []protocol.NodeBinsRow{
		cart("W1", "KP-32X30", false),
		cart("W2", "KP-SMALL", true),
		cart("W3", "KP-BIG", false),
	}
	if got := said(assignWindows(lines, windows, nil, sizes)); got != "" {
		t.Errorf("walk with the group whole = %q, want nothing lit", got)
	}

	// Short of small only: the empty big and the full small are both 45 x 48, the
	// empty walked first takes the one short; the 32 x 30 still lights nothing.
	lines[2].Have = 0
	if got := said(assignWindows(lines, windows, nil, sizes)); got != "W3=KP-SMALL" {
		t.Errorf("walk short one small = %q, want W3=KP-SMALL", got)
	}
}
