package engine

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/store"
)

// kanban_pins_test.go — characterization pins with no database.

// TestKanbanPin_LandedReceiptConfirmsThreePrefixesOnly: Core confirms a landed move
// of its own two window pulls and of the surplus arm, by prefix, and no other order it
// asked for: every other ask waits for its receipt or the auto-confirm sweep.
func TestKanbanPin_LandedReceiptConfirmsThreePrefixesOnly(t *testing.T) {
	t.Parallel()
	for uuid, want := range map[string]string{
		dispatch.Stage2EdgeUUIDPrefix + "1":  stage2Receipt,
		dispatch.DrainEdgeUUIDPrefix + "1":   drainReceipt,
		dispatch.SurplusEdgeUUIDPrefix + "1": surplusReceipt,
		"core-mnt-1":                         "",
		"core-l1-1":                          "",
		"edge-order-1":                       "",
		"":                                   "",
	} {
		if got := landedReceipt(uuid); got != want {
			t.Errorf("landedReceipt(%q) = %q, want %q", uuid, got, want)
		}
	}
	if dispatch.Stage2EdgeUUIDPrefix != "core-s2-" || dispatch.DrainEdgeUUIDPrefix != "core-dr-" || dispatch.SurplusEdgeUUIDPrefix != "core-sp-" {
		t.Errorf("prefixes %q, %q and %q, want core-s2-, core-dr- and core-sp-",
			dispatch.Stage2EdgeUUIDPrefix, dispatch.DrainEdgeUUIDPrefix, dispatch.SurplusEdgeUUIDPrefix)
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
		t.Errorf("the kanban's own pass with the group whole = %q, want nothing lit", got)
	}
	noneShort := []protocol.NodeBinsAssignment{{Node: "W1", NoneShort: true}, {Node: "W2", NoneShort: true}, {Node: "W3", NoneShort: true}}
	if got := walkGroups(lines, nil, windows, nil, sizes); !reflect.DeepEqual(got, noneShort) {
		t.Errorf("walk with the group whole and no sources = %+v, want every cart none short", got)
	}

	// A whole group with a source short of big: the 45 x 48 carts are asked for it
	// in the source's name, the empty first, and the 32 x 30 lights nothing on it.
	source := []sourceLines{{name: "KP-Z45", lines: []store.TypeLine{{Code: "KP-BIG", Want: 2, Have: 1}}}}
	want := []protocol.NodeBinsAssignment{{Node: "W1", NoneShort: true}, {Node: "W2", NoneShort: true},
		{Node: "W3", BinType: "KP-BIG", Group: "KP-Z45"}}
	if got := walkGroups(lines, source, windows, nil, sizes); !reflect.DeepEqual(got, want) {
		t.Errorf("walk with a source short one big = %+v, want %+v", got, want)
	}
	// Short one at each of two sources: rank order over what is left.
	two := []sourceLines{source[0], {name: "KP-Z2", lines: []store.TypeLine{{Code: "KP-SMALL", Want: 1, Have: 0}}}}
	want = []protocol.NodeBinsAssignment{{Node: "W1", NoneShort: true}, {Node: "W2", BinType: "KP-SMALL", Group: "KP-Z2"},
		{Node: "W3", BinType: "KP-BIG", Group: "KP-Z45"}}
	if got := walkGroups(lines, two, windows, nil, sizes); !reflect.DeepEqual(got, want) {
		t.Errorf("walk with two sources short = %+v, want %+v", got, want)
	}
	// A plain source has no lines: it is short of nothing and lights nothing.
	if got := walkGroups(lines, []sourceLines{{name: "KP-PLAIN"}}, windows, nil, sizes); !reflect.DeepEqual(got, noneShort) {
		t.Errorf("walk with a plain source = %+v, want every cart none short", got)
	}
	// A cart on its way already chose: walked by no group, and said nothing of.
	inbound := map[int64]bool{7: true}
	onWay := []protocol.NodeBinsRow{windows[0], windows[1]}
	onWay[1].BinID = 7
	if got := walkGroups(lines, nil, onWay, inbound, sizes); !reflect.DeepEqual(got, noneShort[:1]) {
		t.Errorf("walk with a cart on its way = %+v, want only W1 said", got)
	}

	// Short of small only: the empty big and the full small are both 45 x 48, the
	// empty walked first takes the one short; the 32 x 30 still lights nothing.
	lines[2].Have = 0
	if got := said(assignWindows(lines, windows, nil, sizes)); got != "W3=KP-SMALL" {
		t.Errorf("walk short one small = %q, want W3=KP-SMALL", got)
	}
}

// TestMaintainerGroupState_TheCardsJSON: what the card reads is on the wire, and the
// tick's own fields are not.
func TestMaintainerGroupState_TheCardsJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(MaintainerGroupState{GroupNode: "K", BinTypeCode: "T", Want: 9, Base: 1, Created: 3,
		OriginID: "o", OpenedAt: time.Now(), HeldBy: "why", RunningProcesses: []string{"P"}, Slots: 8, TotalWant: 9, CannotCover: 1,
		NothingProcesses: []string{"N"}, FallbackProcesses: []string{"F"}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"created", "origin_id", "opened_at", "Created", "OriginID", "OpenedAt"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s is on the wire: %s", gone, raw)
		}
	}
	for _, kept := range []string{"want", "base", "held_by", "running_processes", "slots", "total_want", "cannot_cover",
		"nothing_processes", "fallback_processes"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("%s is not on the wire: %s", kept, raw)
		}
	}
}
