package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// authority_exceptions_test.go — the C3 audit's standing guard.
//
// The rule: every path that sends a robot into a lane goes through admission.
// The audit enumerated them and closed the four that did not — the bin-move
// door, redirect, complex dispatch, and the manual robot move (which is closed
// by being refused a lane at all rather than by asking).
//
// An audit is a snapshot. These tests are what stop the list growing a fifth
// entry silently: they key on the STRUCTURE that makes the rule true — where a
// fleet order can be created from, and who asks admission — rather than on a
// count of call sites.

// TestFleetCreate_HasExactlyTwoDoors is the load-bearing one.
//
// A robot moves because a fleet order was created. There are two places in Core
// that can create one:
//
//   - handoverToFleet (dispatch/fleet_handover.go), which every order-backed
//     dispatch funnels through — plain, complex, gated. Admission is asked by
//     each of its callers' paths, which is what the rest of this file checks.
//   - apiRobotMoveTo (www/handlers_robots.go), the manual robot move, which has
//     no order and is therefore refused a lane destination outright.
//
// A THIRD would be a path with no established relationship to admission at all,
// and it would be invisible in a review of dispatch/ alone — the second door is
// in www/. That is exactly how the manual robot move stayed off the map.
func TestFleetCreate_HasExactlyTwoDoors(t *testing.T) {
	t.Parallel()

	root := repoRootFor(t)
	// The vendor create call, however it is spelled at the call site.
	create := regexp.MustCompile(`\.CreateOrder\(\s*(fleet\.CreateOrderRequest|req)\b`)

	known := map[string]string{
		filepath.Join("shingo-core", "dispatch", "fleet_handover.go"): "the one order-backed door",
		filepath.Join("shingo-core", "www", "handlers_robots.go"):     "the manual robot move; refuses lane destinations statically",
	}

	var found []string
	for _, dir := range []string{"shingo-core"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			name := info.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			// The simulator and the fleet adapters IMPLEMENT CreateOrder; they do
			// not decide to send a robot.
			if strings.Contains(rel, string(filepath.Separator)+"fleet"+string(filepath.Separator)) {
				return nil
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if create.Match(body) {
				found = append(found, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	for _, f := range found {
		if _, ok := known[f]; !ok {
			t.Errorf("%s creates a fleet order and is not one of the two known doors.\n"+
				"A new way to send a robot has to say how it answers the lane question: ask admission "+
				"(and have something to park and something to wake it), or refuse a lane destination "+
				"the way the manual robot move does. Add it to the map in admission.go and to this test.", f)
		}
	}
	if len(found) < len(known) {
		t.Errorf("found %d fleet-create door(s) %v, expected at least the %d known ones — if a door "+
			"was removed, remove it from `known` deliberately rather than leaving this test weaker "+
			"than the map it guards", len(found), found, len(known))
	}
}

// TestManualRobotMove_RefusesLaneDestinations pins the shape of the #5 ruling in
// the source: the door reads the lane and refuses, and it does so BEFORE it
// reaches the fleet.
//
// A behavioural test would need the whole www handler stack and a database; what
// actually has to hold is an ordering property in one function, and reading it
// is honest about that. The alternative — no test — is how the door got built
// without a lane question in the first place.
func TestManualRobotMove_RefusesLaneDestinations(t *testing.T) {
	t.Parallel()

	body := readRepoFile(t, filepath.Join("shingo-core", "www", "handlers_robots.go"))
	fn := funcBody(t, body, "func (h *Handlers) apiRobotMoveTo(")

	laneAt := strings.Index(fn, "LaneForNode(")
	if laneAt < 0 {
		t.Fatal("apiRobotMoveTo does not resolve the destination's lane. Without it the door sends a " +
			"bare robot into a corridor with no order, no occupancy row and nothing to release it — " +
			"the one lane entry that keeps no record")
	}
	createAt := strings.Index(fn, "CreateOrder(")
	if createAt < 0 {
		t.Fatal("apiRobotMoveTo no longer creates a fleet order; re-derive this test against whatever " +
			"replaced it rather than deleting it")
	}
	if laneAt > createAt {
		t.Error("the lane check runs AFTER the fleet create — the robot is already going")
	}
	if !strings.Contains(fn, "cannot target lane slots") {
		t.Error("the refusal does not say what it is refusing. An operator who is told only \"bad " +
			"request\" retries it; the message has to name the two things that DO work (a bin move, " +
			"or the vendor console)")
	}
}

// TestBinMoveDoor_AsksAdmission — the #6 expected finding, pinned.
//
// The engineers' and operators' bin-move door went from a capacity preview
// straight to the fleet. Capacity is "is there room there"; it is not "is
// someone digging this lane" or "is a robot already inside it".
//
// The ORDER matters as much as the presence: the ask has to come before
// ConfirmForDispatch, because a refusal must leave the bin SOFT — that is the
// state the scanner's held-bin path expects to pick the order up in, and it is
// what makes the retry the ordinary machinery instead of a second copy of it.
func TestBinMoveDoor_AsksAdmission(t *testing.T) {
	t.Parallel()

	body := readRepoFile(t, filepath.Join("shingo-core", "engine", "bin_move.go"))
	fn := funcBody(t, body, "func (e *Engine) CreateBinMove(")

	askAt := strings.Index(fn, "AcquireLanesForOrder(")
	if askAt < 0 {
		t.Fatal("CreateBinMove does not ask admission. A core operator's bin move is a robot in a lane " +
			"exactly as much as a compound leg is")
	}
	confirmAt := strings.Index(fn, "ConfirmForDispatch(")
	if confirmAt < 0 {
		t.Fatal("CreateBinMove no longer confirms at dispatch; re-derive the ordering rule below")
	}
	if askAt > confirmAt {
		t.Error("admission is asked AFTER ConfirmForDispatch, so a refusal leaves the bin hard-claimed " +
			"— which is not the state the scanner's held-bin retry expects, and the park would need " +
			"its own rollback")
	}
	dispatchAt := strings.Index(fn, "DispatchDirect(")
	if dispatchAt >= 0 && askAt > dispatchAt {
		t.Error("admission is asked after the fleet call, which is not asking")
	}
}

// TestRedirect_AsksAdmission — a redirect points a live order at a node its
// original admission said nothing about, and it went straight to the fleet.
func TestRedirect_AsksAdmission(t *testing.T) {
	t.Parallel()

	body := readRepoFile(t, filepath.Join("shingo-core", "dispatch", "dispatcher.go"))
	fn := funcBody(t, body, "func (d *Dispatcher) HandleOrderRedirect(")

	askAt := strings.Index(fn, "AcquireLanesForOrder(")
	if askAt < 0 {
		t.Fatal("HandleOrderRedirect does not ask admission for the NEW destination. The order's " +
			"original admission answered a question about a different node")
	}
	sendAt := strings.Index(fn, "dispatchToFleet(")
	if sendAt < 0 {
		t.Fatal("HandleOrderRedirect no longer dispatches; re-derive this test")
	}
	if askAt > sendAt {
		t.Error("the redirect asks admission after it has already dispatched")
	}
}

// TestComplexDispatch_AsksAdmission — the biggest of the four. A coordinated
// order never reaches the scanner's admit, and the valve only guards a GATED
// lane, so on the ungated lanes both plants run there was nothing between a
// changeover swap and a corridor.
func TestComplexDispatch_AsksAdmission(t *testing.T) {
	t.Parallel()

	body := readRepoFile(t, filepath.Join("shingo-core", "dispatch", "complex_dispatch.go"))
	fn := funcBody(t, body, "func (d *Dispatcher) DispatchPreparedComplex(")

	askAt := strings.Index(fn, "admitComplexLanes(")
	if askAt < 0 {
		t.Fatal("DispatchPreparedComplex does not ask admission. The gated valve does not cover this: " +
			"it only stands in front of a gated lane, and neither plant has one")
	}
	sendAt := strings.Index(fn, "dispatchComplexToFleet(")
	if sendAt < 0 {
		t.Fatal("DispatchPreparedComplex no longer dispatches; re-derive this test")
	}
	if askAt > sendAt {
		t.Error("the complex tail asks admission after the fleet create")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func repoRootFor(t *testing.T) string {
	t.Helper()
	// This package is <root>/shingo-core/dispatch.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootFor(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// funcBody returns the text from a function's signature to the next top-level
// closing brace. Crude, and sufficient: every function it is used on is
// brace-balanced Go that gofmt has already indented, so a line that is exactly
// "}" ends it.
func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("could not find %q — it was renamed or removed; re-derive this test against what "+
			"replaced it rather than deleting the guard", signature)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
