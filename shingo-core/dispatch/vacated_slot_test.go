package dispatch

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// vacated_slot_test.go — the rule's pure parts: the two boundary predicates, the
// stamp's self-check, the partner walk and the stage-1 order. The database halves
// are in vacated_slot_docker_test.go.

func vsPick(n string) resolvedStep { return resolvedStep{Action: protocol.ActionPickup, Node: n} }
func vsDrop(n string) resolvedStep { return resolvedStep{Action: protocol.ActionDropoff, Node: n} }
func vsWait(n string) resolvedStep { return resolvedStep{Action: protocol.ActionWait, Node: n} }
func vsLane(n string) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: n, WaitKind: WaitKindLane}
}
func vsBare() resolvedStep { return resolvedStep{Action: protocol.ActionWait} }
func vsStation(n string) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: n, WaitKind: WaitKindStation}
}

// The two-robot supply shape: pickup, stage, wait, pickup, deliver.
func vsSupply(src string) []resolvedStep {
	return []resolvedStep{vsPick(src), vsDrop("STAGE"), vsWait("STAGE"), vsPick("STAGE"), vsDrop("LINE")}
}

// committedAtDispatch is the boundary of what the fleet is sent at dispatch.
func TestCommittedAtDispatch(t *testing.T) {
	t.Parallel()
	noLane := func(string) bool { return false }
	laneAt := func(name string) func(string) bool { return func(n string) bool { return n == name } }
	cases := []struct {
		name  string
		steps []resolvedStep
		i     int
		lane  func(string) bool
		want  bool
	}{
		{"two-robot supply lift, before its wait", vsSupply("B1"), 0, noLane, true},
		{"the pickup after the wait", vsSupply("B1"), 3, noLane, false},
		{"a dropoff is not a lift", vsSupply("B1"), 1, noLane, false},
		// P6: the partner's lift comes after its wait — both press-index legs.
		{"press-index index leg lifts after its wait",
			[]resolvedStep{vsWait("B"), vsPick("B"), vsDrop("A")}, 1, noLane, false},
		// The wait is ANY kind: a bare split marker still ends what the create sends.
		{"a bare wait ends the prefix too",
			[]resolvedStep{vsBare(), vsPick("B1"), vsDrop("X")}, 1, noLane, false},
		{"a lane wait ends it too",
			[]resolvedStep{vsLane("G"), vsPick("B1"), vsDrop("X")}, 1, noLane, false},
		// P6: a lane node before the lift — the splice can gate it at create.
		{"a lane node at or before the lift",
			[]resolvedStep{vsDrop("L1"), vsPick("B1"), vsDrop("S"), vsWait("S")}, 1, laneAt("L1"), false},
		{"the lift itself in a lane",
			vsSupply("L1"), 0, laneAt("L1"), false},
		{"nil lane test ranks only", vsSupply("L1"), 0, nil, true},
		{"out of range", vsSupply("B1"), 9, noLane, false},
	}
	for _, c := range cases {
		if got := committedAtDispatch(c.steps, c.i, c.lane); got != c.want {
			t.Errorf("%s: committedAtDispatch = %v, want %v", c.name, got, c.want)
		}
	}
}

// heldForRelease: a station wait before the drop, and only a station wait.
func TestHeldForRelease(t *testing.T) {
	t.Parallel()
	evac := []resolvedStep{vsStation("LINE"), vsPick("LINE"), vsDrop("B1")}
	if !heldForRelease(evac, 2) {
		t.Error("an evac's drop after its station wait must be held for release")
	}
	// P6 / P16: L has no station wait before its drop.
	if heldForRelease([]resolvedStep{vsPick("LINE"), vsDrop("B1")}, 1) {
		t.Error("a leg with no wait is never a (b) dropper — nothing holds its drop for the fence")
	}
	if heldForRelease([]resolvedStep{vsLane("G"), vsPick("LINE"), vsDrop("B1")}, 2) {
		t.Error("a LANE wait is Core's, not the station's: it does not hold the drop for a release")
	}
	if heldForRelease([]resolvedStep{vsPick("LINE"), vsDrop("B1"), vsStation("X")}, 1) {
		t.Error("a station wait AFTER the drop does not hold it")
	}
}

// TestHeldForRelease_UntaggedWait pins the drain-window reading: an untagged
// wait counts as the station's today. When IsStationWait's `== ""` arm is
// deleted this goes red, which is the point — an untagged plan then stops being
// a (b) dropper, and that change must be seen, not discovered.
func TestHeldForRelease_UntaggedWait(t *testing.T) {
	t.Parallel()
	if !heldForRelease([]resolvedStep{vsWait("LINE"), vsPick("LINE"), vsDrop("B1")}, 2) {
		t.Fatal("an UNTAGGED wait no longer holds a drop for release. If the drain window closed on " +
			"purpose, every pre-ruling plan just stopped qualifying for the vacated-slot rule — decide " +
			"that, then update this pin")
	}
}

// P24 — the stamp honours itself only while the step still drops at its node.
func TestStampHonoured_OnlyWhileTheStepStillDropsThere(t *testing.T) {
	t.Parallel()
	st := &vacateStamp{Node: "B1", Bin: 7, Partner: 3}
	s := resolvedStep{Action: protocol.ActionDropoff, Node: "B1", Vacate: st}
	if stampHonoured(s) == nil || partnerStamp(s) == nil {
		t.Fatal("a stamp on the drop it was granted for must count")
	}
	s.Node = "B2" // re-pointed by a writer that forgot to clear it
	if stampHonoured(s) != nil || partnerStamp(s) != nil {
		t.Fatal("a stamp left on a re-pointed drop still counted — the self-check is the guarantee")
	}
	own := resolvedStep{Action: protocol.ActionDropoff, Node: "B1", Vacate: &vacateStamp{Node: "B1", Bin: 7}}
	if stampHonoured(own) == nil || partnerStamp(own) != nil {
		t.Fatal("an own-lift stamp counts for the gates but never for the release fence")
	}
	if stampHonoured(resolvedStep{Action: protocol.ActionPickup, Node: "B1", Vacate: st}) != nil {
		t.Fatal("a stamp only means something on a dropoff")
	}
}

// The stamp rides steps_json, and an unstamped plan serialises as before.
func TestStampSurvivesStepsJSON(t *testing.T) {
	t.Parallel()
	steps := []resolvedStep{vsStation("LINE"), vsPick("LINE"),
		{Action: protocol.ActionDropoff, Node: "C1", Group: "GRP", Vacate: &vacateStamp{Node: "C1", Bin: 1, Partner: 2}}}
	j := mustJSON(t, steps)
	var back []resolvedStep
	if err := json.Unmarshal(j, &back); err != nil || back[2].Vacate == nil || *back[2].Vacate != *steps[2].Vacate {
		t.Fatalf("the stamp must survive steps_json: %v %+v", err, back)
	}
	if strings.Contains(string(mustJSON(t, []resolvedStep{vsDrop("X")})), "vacate") {
		t.Fatal("an unstamped step must serialise exactly as before (omitempty)")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// P15 — a partner that sets a carrier down at N and picks it up again before
// its wait re-collects its own carrier; it vacates nothing.
func TestPartnerLiftsResident_ReCollectIsNotAVacate(t *testing.T) {
	t.Parallel()
	recollect := []resolvedStep{vsPick("SRC"), vsDrop("N"), vsPick("N"), vsDrop("S"), vsWait("S")}
	if partnerLiftsResident(recollect, 2, "N") {
		t.Fatal("a re-collect of the partner's own carrier was counted as emptying N")
	}
	if !partnerLiftsResident(vsSupply("N"), 0, "N") {
		t.Fatal("a plain lift of N's resident must count")
	}
}

// The stage-1 order: a leg that lifts before its first wait runs first, ties in
// id order, and nothing else moves.
func TestStageOneOrder_LifterFirst(t *testing.T) {
	t.Parallel()
	leg := func(id int64, steps []resolvedStep) *orders.Order {
		j := mustJSON(t, steps)
		return &orders.Order{ID: id, StepsJSON: string(j)}
	}
	evac := leg(1, []resolvedStep{vsStation("LINE"), vsPick("LINE"), vsDrop("B1")})
	supply := leg(2, vsSupply("B1"))
	if got := stageOneOrder([]*orders.Order{evac, supply}); got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("inverted pair: order %d then %d, want the lifter (2) first", got[0].ID, got[1].ID)
	}
	r1 := leg(1, []resolvedStep{vsWait("A"), vsPick("A"), vsDrop("OUT")})
	r2 := leg(2, []resolvedStep{vsWait("B"), vsPick("B"), vsDrop("A")})
	if got := stageOneOrder([]*orders.Order{r1, r2}); got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("press-index (neither lifts before its wait): order %d then %d, want id order", got[0].ID, got[1].ID)
	}
}

// Each boundary predicate stays ONE named function, and the rule asks it by
// name. A second spelling is how two askers come to disagree about one drop.
func TestVacatedSlotBoundaryPredicatesStayNamed(t *testing.T) {
	t.Parallel()
	src := readRepoFile(t, filepath.Join("shingo-core", "dispatch", "vacated_slot.go"))
	for _, sig := range []string{"func committedAtDispatch(", "func heldForRelease("} {
		if strings.Count(src, sig) != 1 {
			t.Errorf("%s must be defined exactly once in vacated_slot.go", sig)
		}
	}
	rule := funcBody(t, src, "func (d *Dispatcher) vacatedFor(")
	for _, call := range []string{"committedAtDispatch(", "heldForRelease("} {
		if !strings.Contains(rule, call) {
			t.Errorf("vacatedFor no longer asks %s — the rule has grown a second spelling of its boundary", call)
		}
	}
}
