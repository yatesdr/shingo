package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
)

// vacate_owner_test.go — who owns a wait, asked in one place (coreOwnsWait).

func voWait(node, kind string) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: node, WaitKind: kind}
}

func voDrop(node string, st *vacateStamp) resolvedStep {
	return resolvedStep{Action: protocol.ActionDropoff, Node: node, Vacate: st}
}

// voTurnHeld is a station wait whose release Core took for the table turn.
func voTurnHeld(node string) resolvedStep {
	at := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	w := voWait(node, WaitKindStation)
	w.TurnHeldAt = &at
	return w
}

func voHeld(node string) *vacateStamp {
	at := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	return &vacateStamp{Node: node, Bin: 7, Partner: 9, HeldAt: &at}
}

// TestCoreOwnsWait: a Core kind (lane, vacate, or one this build does not
// know), or the wait's next drop carries a held vacate stamp. Nothing else
// makes a wait Core's.
func TestCoreOwnsWait(t *testing.T) {
	t.Parallel()
	pick := resolvedStep{Action: protocol.ActionPickup, Node: "LINE"}
	unheld := &vacateStamp{Node: "B1", Bin: 7, Partner: 9}
	for _, c := range []struct {
		name  string
		steps []resolvedStep
		at    int
		want  bool
	}{
		{"a lane wait", []resolvedStep{voWait("G", WaitKindLane), voDrop("B1", nil)}, 0, true},
		{"a stored vacate wait", []resolvedStep{pick, voWait("", WaitKindVacate), voDrop("B1", nil)}, 0, true},
		{"a station wait", []resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("B1", nil)}, 0, false},
		{"an untagged wait (drain window)", []resolvedStep{voWait("LINE", ""), pick, voDrop("B1", nil)}, 0, false},
		// Core's, as on the Edge (protocol.IsCoreWaitKind): never released from a
		// station. Splice refuses one before it is stored.
		{"an unrecognised kind", []resolvedStep{voWait("LINE", "bogus"), pick, voDrop("B1", nil)}, 0, true},
		{"a station wait, next drop held", []resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("B1", voHeld("B1"))}, 0, true},
		{"a station wait, next drop stamped but not held",
			[]resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("B1", unheld)}, 0, false},
		{"a held stamp naming another node (re-pointed drop)",
			[]resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("B2", voHeld("B1"))}, 0, false},
		{"held, but on a drop after the next one",
			[]resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("STAGE", nil), pick, voDrop("B1", voHeld("B1"))}, 0, false},
		{"held, but in the next segment, not this one",
			[]resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("STAGE", nil), voWait("", WaitKindStation), voDrop("B1", voHeld("B1"))}, 0, false},
		{"the second wait, its next drop held",
			[]resolvedStep{voWait("LINE", WaitKindStation), pick, voDrop("STAGE", nil), voWait("", WaitKindStation), voDrop("B1", voHeld("B1"))}, 1, true},
		{"no wait at that index", []resolvedStep{voWait("G", WaitKindLane)}, 1, false},
		{"a turn wait Core inserted", []resolvedStep{voWait("LINE", WaitKindTurn), pick, voDrop("B1", nil)}, 0, true},
		{"a station wait held for the table", []resolvedStep{voTurnHeld("LINE"), pick, voDrop("B1", nil)}, 0, true},
	} {
		if got := coreOwnsWait(c.steps, c.at); got != c.want {
			t.Errorf("%s: coreOwnsWait = %v, want %v", c.name, got, c.want)
		}
	}
	if CoreOwnsWaitAt("", 0) || CoreOwnsWaitAt("{not json", 0) {
		t.Error("an absent or unreadable plan must answer false: nothing offers an override for a wait it cannot identify")
	}
}

// TestStationPoint_VacateIsCoreOwned: the station is told a Core kind and no
// ordinal for a wait Core owns on the vacate fact, and the ordinals of the
// station waits around it are unchanged (the Edge numbers its own copy).
func TestStationPoint_VacateIsCoreOwned(t *testing.T) {
	t.Parallel()
	pick := resolvedStep{Action: protocol.ActionPickup, Node: "N"}
	plan := []resolvedStep{
		voWait("A", WaitKindStation), pick, voDrop("B1", voHeld("B1")), // 0: held → vacate
		voWait("", WaitKindStation), pick, voDrop("B2", nil), // 1: station:1
		voWait("", WaitKindVacate), voDrop("B3", nil), // 2: stored vacate
		voWait("", WaitKindStation), // 3: station:2
	}
	for _, c := range []struct {
		waitIndex int
		want      string
	}{{0, "vacate"}, {1, "station:1"}, {2, "vacate"}, {3, "station:2"}} {
		at, kind, ok := stationPoint(plan, c.waitIndex)
		got := "none"
		switch {
		case ok && at != nil:
			got = fmt.Sprintf("%s:%d", kind, *at)
		case ok:
			got = kind
		}
		if got != c.want {
			t.Errorf("stationPoint(wait_index %d) = %s, want %s", c.waitIndex, got, c.want)
		}
	}
}

func TestAssertEveryWaitDeclaresAnOwner_AcceptsVacate(t *testing.T) {
	t.Parallel()
	if err := assertEveryWaitDeclaresAnOwner([]resolvedStep{
		{Action: protocol.ActionPickup, Node: "S"}, voWait("", WaitKindVacate), voDrop("H", nil)}); err != nil {
		t.Errorf("a Core-authored vacate wait was refused: %v", err)
	}
}

// TestWidenStopsAtAStationOrVacateWait: widening runs past a lane wait and
// stops at a station or vacate wait (widenSupplyPickups says why).
func TestWidenStopsAtAStationOrVacateWait(t *testing.T) {
	t.Parallel()
	for kind, want := range map[string]bool{WaitKindLane: false, WaitKindStation: true, "": true, WaitKindVacate: true} {
		if got := widenStopsAt(voWait("N", kind)); got != want {
			t.Errorf("widenStopsAt(wait %q) = %v, want %v", kind, got, want)
		}
	}
	if widenStopsAt(resolvedStep{Action: protocol.ActionPickup, Node: "N"}) {
		t.Error("widening stopped at a pickup")
	}
}

// TestOnlyCoreOwnsWaitReadsTheHeldStamp: ownership is decided in one function
// (ruled 2026-10-18). A held vacate stamp is the only part of plan.Vacate that
// speaks to ownership, so its readers in this package are pinned by name: the
// owner function itself and the hold's own machinery, which writes the stamp
// and releases it. Any other file reading HeldAt is a second owner question.
func TestOnlyCoreOwnsWaitReadsTheHeldStamp(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{
		"complex.go":     true, // coreOwnsWait / heldVacateAfter / turnHeldOrTurnWait
		"vacate_hold.go": true, // writes HeldAt and TurnHeldAt at the tap (markHeld), and releases what it held
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// The table turn's held mark on a wait (plan.Step.TurnHeldAt) is the same
	// kind of fact and has the same readers.
	read := regexp.MustCompile(`\.(Turn)?HeldAt\b`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		checked++
		if read.Match(b) && !allowed[f] {
			t.Errorf("%s reads a held mark (plan.Vacate.HeldAt or plan.Step.TurnHeldAt). Who owns a wait is "+
				"coreOwnsWait's answer (complex.go); ask it rather than reading the mark", f)
		}
	}
	if checked == 0 {
		t.Fatal("no production files read — the test is vacuous")
	}
}

// TestTurnApplies is the table turn's predicate over a given set of turn
// nodes: a station or turn wait at the node, and the segment after it opening
// with the lift there (or the set-down, on a set-down node). Nothing else.
func TestTurnApplies(t *testing.T) {
	t.Parallel()
	nodes := map[string]turnNode{
		"LINE": {rad: 1.57, side: TableTurnSideLift},
		"DROP": {rad: -1.57, side: TableTurnSideSetDown},
	}
	at := func(n string) (turnNode, bool) { tn, ok := nodes[n]; return tn, ok }
	pick := func(n string) resolvedStep { return resolvedStep{Action: protocol.ActionPickup, Node: n} }
	drop := func(n string) resolvedStep { return resolvedStep{Action: protocol.ActionDropoff, Node: n} }
	for _, c := range []struct {
		name  string
		steps []resolvedStep
		at    int
		want  bool
	}{
		{"a swap's station wait before its lift", []resolvedStep{voWait("LINE", WaitKindStation), pick("LINE"), drop("B1")}, 0, true},
		{"a turn wait before the lift", []resolvedStep{voWait("LINE", WaitKindTurn), pick("LINE"), drop("B1")}, 0, true},
		{"an untagged wait (drain window)", []resolvedStep{voWait("LINE", ""), pick("LINE"), drop("B1")}, 0, true},
		{"a node with no angle", []resolvedStep{voWait("OTHER", WaitKindStation), pick("OTHER"), drop("B1")}, 0, false},
		{"a wait elsewhere before the lift", []resolvedStep{voWait("STAGE", WaitKindStation), pick("LINE"), drop("B1")}, 0, false},
		{"a lane wait", []resolvedStep{voWait("LINE", WaitKindLane), pick("LINE"), drop("B1")}, 0, false},
		{"a vacate wait", []resolvedStep{pick("SRC"), voWait("LINE", WaitKindVacate), pick("LINE")}, 0, false},
		{"a drop, at a lift node", []resolvedStep{voWait("LINE", WaitKindStation), drop("LINE")}, 0, false},
		{"a set-down node's drop", []resolvedStep{pick("SRC"), voWait("DROP", WaitKindTurn), drop("DROP")}, 0, true},
		{"a set-down node's lift", []resolvedStep{voWait("DROP", WaitKindStation), pick("DROP"), drop("B1")}, 0, false},
		{"the lift is not the segment's first step", []resolvedStep{voWait("LINE", WaitKindStation), drop("B2"), pick("LINE")}, 0, false},
		{"past the last wait", []resolvedStep{voWait("LINE", WaitKindStation), pick("LINE")}, 1, false},
		{"the second wait", []resolvedStep{voWait("STAGE", WaitKindStation), pick("SRC"), voWait("LINE", WaitKindTurn), pick("LINE")}, 1, true},
	} {
		if _, _, got := turnAppliesWith(c.steps, c.at, at); got != c.want {
			t.Errorf("%s: turnApplies = %v, want %v", c.name, got, c.want)
		}
	}
}
