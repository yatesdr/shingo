package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch/plan"
)

// table_turn_test.go — the turn rule over the in-memory set of turn nodes. It
// reads no database, so these need none.

// ruleDispatcher is a dispatcher that knows only its turn nodes.
func ruleDispatcher(nodes map[string]turnNode) *Dispatcher {
	return &Dispatcher{turnNodes: nodes}
}

func ttPick(n string) resolvedStep { return resolvedStep{Action: protocol.ActionPickup, Node: n} }
func ttDrop(n string) resolvedStep { return resolvedStep{Action: protocol.ActionDropoff, Node: n} }

// shape renders a plan as action@node[:kind] for a comparison.
func shape(steps []resolvedStep) string {
	var parts []string
	for _, s := range steps {
		p := s.Action + "@" + s.Node
		if s.WaitKind != "" {
			p += ":" + s.WaitKind
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

// TestTurnRule_LiftSide: a turn wait in front of every lift at a lift-side
// turn node, unless a wait at that node already stands in front; nothing at a
// node with no angle.
func TestTurnRule_LiftSide(t *testing.T) {
	t.Parallel()
	d := ruleDispatcher(map[string]turnNode{"X": {rad: 1.57, side: TableTurnSideLift}})
	for _, c := range []struct {
		name string
		in   []resolvedStep
		want string
	}{
		{"a plain move from X", []resolvedStep{ttPick("X"), ttDrop("Y")},
			"wait@X:turn pickup@X dropoff@Y"},
		{"a swap's opening: its station wait stands at X", []resolvedStep{voWait("X", WaitKindStation), ttPick("X"), ttDrop("Y")},
			"wait@X:station pickup@X dropoff@Y"},
		{"the curtained evacuation: the Edge's wait at X", []resolvedStep{ttPick("S"), ttDrop("Z"), voWait("X", WaitKindStation), ttPick("X"), ttDrop("Y")},
			"pickup@S dropoff@Z wait@X:station pickup@X dropoff@Y"},
		{"a bare wait before the lift still gets one after it", []resolvedStep{voWait("", WaitKindStation), ttPick("X"), ttDrop("Y")},
			"wait@:station wait@X:turn pickup@X dropoff@Y"},
		{"a mid-segment lift", []resolvedStep{ttPick("A"), ttDrop("B"), ttPick("X"), ttDrop("C")},
			"pickup@A dropoff@B wait@X:turn pickup@X dropoff@C"},
		{"a drop at X is not a lift", []resolvedStep{ttPick("A"), ttDrop("X")},
			"pickup@A dropoff@X"},
		{"no turn node on the path", []resolvedStep{ttPick("A"), ttDrop("B")},
			"pickup@A dropoff@B"},
		{"a home clear's trade leg lifting at X", tradeSteps("X", "H", plan.Vacate{Node: "H", Bin: 7}),
			"wait@X:turn pickup@X wait@:vacate dropoff@H"},
	} {
		out, err := d.spliceTurnWaits(c.in)
		if err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
			continue
		}
		if got := shape(out); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// TestTurnRule_WaitsReleaseInOrder: with a bare station wait in front of the
// turn wait, the station's release sends the robot to the turn wait, and the
// lift goes out only after it. The Edge's numbering is unchanged: the turn wait
// has no station ordinal and reports kind turn.
func TestTurnRule_WaitsReleaseInOrder(t *testing.T) {
	t.Parallel()
	d := ruleDispatcher(map[string]turnNode{"X": {rad: 1.57, side: TableTurnSideLift}})
	out, err := d.spliceTurnWaits([]resolvedStep{voWait("", WaitKindStation), ttPick("X"), ttDrop("Y"), voWait("", WaitKindStation), ttPick("Y")})
	if err != nil {
		t.Fatal(err)
	}
	if seg, more, _ := splitSegment(out, 0); shape(seg) != "wait@X:turn" || !more {
		t.Errorf("after the station's release: %s (more=%v), want only the drive to the turn wait", shape(seg), more)
	}
	if seg, _, _ := splitSegment(out, 1); shape(seg) != "pickup@X dropoff@Y" {
		t.Errorf("after the turn: %s, want the lift", shape(seg))
	}
	for _, c := range []struct {
		at   int
		want string
	}{{0, "station:0"}, {1, "turn"}, {2, "station:1"}} {
		at, kind, ok := stationPoint(out, c.at)
		got := kind
		if at != nil {
			got = kind + ":" + string(rune('0'+*at))
		}
		if !ok || got != c.want {
			t.Errorf("stationPoint(%d) = %s, want %s", c.at, got, c.want)
		}
	}
}

// TestTurnRule_RefusesALiftWithoutItsWait: the check after the insert names
// the node. A plan the rule did not foresee is refused, not shipped.
func TestTurnRule_RefusesALiftWithoutItsWait(t *testing.T) {
	t.Parallel()
	d := ruleDispatcher(map[string]turnNode{"X": {rad: 1.57, side: TableTurnSideLift}})
	if err := d.assertEveryTurnHasItsWait([]resolvedStep{ttPick("A"), ttPick("X")}); err == nil ||
		!strings.Contains(err.Error(), "X") {
		t.Errorf("a lift at X with no wait at X in front: %v, want a refusal naming X", err)
	}
	if err := d.assertEveryTurnHasItsWait([]resolvedStep{voWait("X", WaitKindTurn), ttPick("X")}); err != nil {
		t.Errorf("a lift with its wait: %v", err)
	}
}

// TestWithoutInsertedWaits_StripsTheTurnWait: a re-queued plan loses the turn
// wait with the lane waits, so the splice inserts it again where the plan now
// lifts, and a pickup that moved off X gets none.
func TestWithoutInsertedWaits_StripsTheTurnWait(t *testing.T) {
	t.Parallel()
	in := []resolvedStep{voWait("X", WaitKindTurn), ttPick("X"), voWait("STAGE", WaitKindStation), ttDrop("Y")}
	if got := shape(withoutInsertedWaits(in)); got != "pickup@X wait@STAGE:station dropoff@Y" {
		t.Errorf("stripped to %s", got)
	}
	d := ruleDispatcher(map[string]turnNode{"X": {rad: 1.57, side: TableTurnSideLift}})
	moved := []resolvedStep{ttPick("Z"), voWait("STAGE", WaitKindStation), ttDrop("Y")}
	out, err := d.spliceTurnWaits(moved)
	if err != nil || shape(out) != "pickup@Z wait@STAGE:station dropoff@Y" {
		t.Errorf("a pickup moved off X: %s (%v), want no turn wait", shape(out), err)
	}
}

// TestSpliceStepShift_CountsATurnWaitAsInserted: a cart row behind a turn wait
// moves one step, as behind a lane wait.
func TestSpliceStepShift_CountsATurnWaitAsInserted(t *testing.T) {
	t.Parallel()
	shift := spliceStepShift([]resolvedStep{ttPick("A"), ttDrop("B"), voWait("X", WaitKindTurn), ttPick("X"), ttDrop("C")})
	if len(shift) != 2 || shift[2] != 3 || shift[3] != 4 {
		t.Errorf("shift = %v, want {2:3 3:4}", shift)
	}
}
