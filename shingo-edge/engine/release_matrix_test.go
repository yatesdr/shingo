package engine

// release_matrix_test.go — the characterisation matrix: door × gate × order kind.
//
// One row per cell. A cell builds a plant-shaped fixture, drives ONE door, and
// compares the whole outcome (release_harness_test.go) against what the cell
// says. The doors are numbered as in the curtain brief's §3 table:
//
//	1  operator pair click          ReleaseStagedOrders
//	2  operator per-order click     ReleaseOrderWithLineside
//	3  Material page                ReleaseNodeWithRemainingUOP (releaseNodeWithClaim, no fallback)
//	4  changeover position evac     EvacuateNode's fallback (releaseNodeWithClaim with a fallback)
//	5  changeover sweep             ReleaseChangeoverWait / ReleaseChangeoverWaitForNode
//	6  single-leg changeover node   ReleaseStagedOrders → releaseSingleLegChangeoverNode
//	7  drop-situation evac          the trunk's drop fast path
//	8  deferred sibling re-fire     staged → handleSiblingReleaseRefire
//	9  swap survivor                partner terminal-success → releaseSurvivorOfFinishedPartner
//	10 changeover supply at pickup  HandleBinPickedUp
//	11 sim auto-operator            release_matrix_sim_test.go (-tags sim)
//
// RECORDING. RELEASE_MATRIX_RECORD=1 prints every cell's observed outcome
// instead of asserting — how a new cell's `today` is taken from the code rather
// than from anyone's reading of it. RELEASE_MATRIX_WANT=1 asserts every cell's
// corrected outcome, ignoring the bug tags: at a tree where a bug is live, its
// cells fail and only its cells fail, which is the evidence that each tag names
// a real defect.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// relCell is one characterised cell.
type relCell struct {
	name  string
	build func(h *relHarness) // fixture, statuses, curtain — everything before the act
	act   func(h *relHarness) error
	// probe adds cell-specific fields to the outcome after the act (task state,
	// piles, envelope contents, the chip).
	probe func(h *relHarness) []string
	bug   string // known defect id while the cell holds today's outcome
	today string // the outcome at the base, when bug is set
	want  string // the correct outcome
}

func runRelCells(t *testing.T, cells []relCell) {
	t.Helper()
	record := os.Getenv("RELEASE_MATRIX_RECORD") != ""
	wantOnly := os.Getenv("RELEASE_MATRIX_WANT") != ""
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newRelHarness(t)
			c.build(h)
			h.mark()
			err := c.act(h)
			if c.probe != nil {
				h.extra = append(h.extra, c.probe(h)...)
			}
			got := h.outcome(err)
			if record {
				t.Logf("RECORD %s\n  %q", c.name, got)
				return
			}
			if wantOnly {
				// The RED evidence: every tagged cell must fail here, and no
				// untagged one may.
				if got != c.want {
					t.Errorf("RED (bug:%s)\n got  %q\n want %q", c.bug, got, c.want)
				}
				return
			}
			pinOutcome(t, c.bug, got, c.today, c.want)
		})
	}
}

// ── Dispositions the doors are driven with ───────────────────────────────

var (
	dispEmpty   = ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "operator"}
	dispNone    = ReleaseDisposition{CalledBy: "operator"}
	dispPartial = func(n int) ReleaseDisposition {
		return ReleaseDisposition{Mode: DispositionSendPartialBack, PartialCount: &n, CalledBy: "operator"}
	}
	dispPull = func(q int) ReleaseDisposition {
		return ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{fxPart: q}, CalledBy: "operator"}
	}
)

// ── Small builders the cells share ───────────────────────────────────────

func statuses(h *relHarness, pairs ...any) {
	h.t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		h.setStatus(pairs[i].(string), pairs[i+1].(protocol.Status))
	}
}

func pairAt(s pairSpec, st ...any) func(h *relHarness) {
	return func(h *relHarness) {
		h.steadyPair(s)
		statuses(h, st...)
	}
}

func coAt(s coSpec, st ...any) func(h *relHarness) {
	return func(h *relHarness) {
		h.changeover(s)
		statuses(h, st...)
	}
}

func withCurtain(reading bool, b func(h *relHarness)) func(h *relHarness) {
	return func(h *relHarness) {
		b(h)
		h.armCurtain(reading)
	}
}

func pairClick(d ReleaseDisposition) func(h *relHarness) error {
	return func(h *relHarness) error { return h.eng.ReleaseStagedOrders(h.nodeID, d) }
}

func orderClick(leg string, d ReleaseDisposition) func(h *relHarness) error {
	return func(h *relHarness) error { return h.eng.ReleaseOrderWithLineside(h.leg(leg), d) }
}

func sweepClick(d ReleaseDisposition) func(h *relHarness) error {
	return func(h *relHarness) error {
		res, err := h.eng.ReleaseChangeoverWait(h.processID, d)
		h.extra = append(h.extra, fmt.Sprintf("sweep released=%d pending=%d flip=%v", res.Released, res.Pending, res.NeedsFlip))
		return err
	}
}

func nodeCOClick(d ReleaseDisposition) func(h *relHarness) error {
	return func(h *relHarness) error {
		res, err := h.eng.ReleaseChangeoverWaitForNode(h.processID, h.nodeID, d)
		h.extra = append(h.extra, fmt.Sprintf("node released=%d pending=%d flip=%v", res.Released, res.Pending, res.NeedsFlip))
		return err
	}
}

// deferred reports whether the in-memory pair deferral holds a leg.
func deferred(h *relHarness, leg string) bool {
	h.eng.pendingSiblingReleaseMu.Lock()
	defer h.eng.pendingSiblingReleaseMu.Unlock()
	_, ok := h.eng.pendingSiblingRelease[h.leg(leg)]
	return ok
}

// chipOf is the release-error chip the board would show for a leg if it held
// the leg's runtime slot — the latest order_history rollback sentence.
func chipOf(h *relHarness, leg string) string {
	h.t.Helper()
	id := h.leg(leg)
	c := store.LookupLastReleaseError(h.db, &processes.RuntimeState{ActiveOrderID: &id})
	if c == "" {
		return "-"
	}
	if i := strings.Index(c, ":"); i > 0 {
		return c[:i]
	}
	return c
}

// uopOf is the node's cached count.
func uopOf(h *relHarness) string {
	return fmt.Sprintf("uop=%d", remainingUOP(h.t, h.db, h.nodeID))
}

// envOf renders the manifest half of each release envelope since mark().
func envOf(h *relHarness) []string {
	var out []string
	envs := h.releaseEnvelopes()
	for _, l := range h.legs {
		for _, e := range envs[l.name] {
			uop := "nil"
			if e.RemainingUOP != nil {
				uop = fmt.Sprint(*e.RemainingUOP)
			}
			kind := "-"
			if e.Disposition != nil {
				kind = string(e.Disposition.Kind)
				if e.Disposition.Count != 0 {
					kind += fmt.Sprintf("/%d", e.Disposition.Count)
				}
			}
			out = append(out, fmt.Sprintf("env:%s uop=%s kind=%s", l.name, uop, kind))
		}
	}
	return out
}

func probes(ps ...func(h *relHarness) []string) func(h *relHarness) []string {
	return func(h *relHarness) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p(h)...)
		}
		return out
	}
}

func pUOP(h *relHarness) []string { return []string{uopOf(h)} }
func pEnv(h *relHarness) []string { return envOf(h) }
func pDeferred(h *relHarness) []string {
	var out []string
	for _, l := range h.legs {
		if deferred(h, l.name) {
			out = append(out, "deferred="+l.name)
		}
	}
	return out
}
func pChip(leg string) func(h *relHarness) []string {
	return func(h *relHarness) []string { return []string{"chip:" + leg + "=" + chipOf(h, leg)} }
}

// ── Gates before side effects ────────────────────────────────────────────

// TestReleasePathsGateBeforeSideEffects holds every door to the rule the
// release code states at its gates: a refusal is reachable without having
// changed anything. For every characterised cell whose correct outcome is a
// refusal or a hold, that outcome must show no release envelope and no
// paperwork. The matrix then proves each cell reaches its outcome, so a door
// that refuses AFTER its paperwork fails there, against a want this test has
// already held to "no trace".
func TestReleasePathsGateBeforeSideEffects(t *testing.T) {
	t.Parallel()
	cells := append(releaseMatrixCells(), releasePinCells()...)
	refusals := 0
	for _, c := range cells {
		verdict, _, _ := strings.Cut(c.want, " | ")
		if !strings.HasPrefix(verdict, "refuse:") && !strings.HasPrefix(verdict, "hold:") {
			continue
		}
		refusals++
		if !strings.Contains(c.want, "| rel=- |") || !strings.Contains(c.want, "| ingest=0 capred=0") {
			t.Errorf("%s: a %s must leave no trace, but its outcome is %q", c.name, verdict, c.want)
		}
	}
	if refusals == 0 {
		t.Fatal("no refusal cells found — the matrix lost its refusal rows")
	}
}

// ── The matrix ───────────────────────────────────────────────────────────

func TestReleaseMatrix(t *testing.T) {
	t.Parallel()
	runRelCells(t, releaseMatrixCells())
}

func releaseMatrixCells() []relCell {
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	twoRobotConsume := pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}
	pi2 := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}
	pi3 := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex, threePos: true}
	pi2f := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex, flipped: true}
	pi3f := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex, threePos: true, flipped: true}
	S, Q, D, T := protocol.StatusStaged, protocol.StatusQueued, protocol.StatusDispatched, protocol.StatusInTransit

	return []relCell{
		// ── Door 1: the operator's pair click ─────────────────────────────
		{name: "d1/two_robot/both staged",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0", build: pairAt(twoRobot, "evac", S, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP, pDeferred)},
		{name: "d1/two_robot/supply still dispatched",
			want: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=1 capred=0 | uop=0 | deferred=supply", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: pairClick(dispEmpty), probe: probes(pUOP, pDeferred)},
		{name: "d1/pi2 unflipped/both staged",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0", build: pairAt(pi2, "evac", S, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi3 unflipped/both staged",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0", build: pairAt(pi3, "evac", S, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2 unflipped/R2 staged R1 queued",
			want: "hold:collision | evac=queued supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(pi2, "evac", Q, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2 unflipped/R2 staged R1 in_transit",
			bug:   "N-b",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			want:  "hold:collision | evac=in_transit supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(pi2, "evac", T, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2 unflipped/R1 staged R2 in_transit",
			bug:   "N-b",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			want:  "hold:collision | evac=staged supply=in_transit | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(pi2, "evac", S, "supply", T),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi3 unflipped/R2 staged R1 in_transit",
			bug:   "N-b",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			want:  "hold:collision | evac=in_transit supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(pi3, "evac", T, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2 flipped/R2 staged R1 in_transit",
			bug:   "N-b",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			want:  "hold:collision | evac=in_transit supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(pi2f, "evac", T, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi3 flipped/R1 staged R2 queued",
			want: "ok | evac=in_transit supply=queued | rel=evac | ingest=1 capred=0 | uop=0 | deferred=supply", build: pairAt(pi3f, "evac", S, "supply", Q),
			act: pairClick(dispEmpty), probe: probes(pUOP, pDeferred)},
		{name: "d1/two_robot/curtain live",
			want: "refuse:curtain | evac=staged supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, pairAt(twoRobot, "evac", S, "supply", S)),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/two_robot/curtain safe",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0", build: withCurtain(curtainSafe, pairAt(twoRobot, "evac", S, "supply", S)),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/two_robot/first cycle curtain live",
			want: "refuse:curtain | evac=staged supply=staged | rel=- | ingest=0 capred=0 | uop=42",
			build: withCurtain(curtainLive, func(h *relHarness) {
				pairAt(twoRobot, "evac", S, "supply", S)(h)
				testutil.MustNoErr(h.t, h.db.SetProcessNodeRuntime(h.nodeID, nil, fxCount), "unstamp")
			}),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2/curtain goes live after the top reads",
			bug:   "A2",
			today: "refuse:curtain | evac=staged supply=staged | rel=- | ingest=1 capred=0 | uop=0",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			build: func(h *relHarness) {
				withCurtain(curtainSafe, pairAt(pi2, "evac", S, "supply", S))(h)
				h.wl.seq = []any{curtainSafe, curtainSafe, curtainLive}
			},
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/pi2/curtain goes live between the legs",
			bug:   "A2",
			today: "refuse:curtain | evac=in_transit supply=staged | rel=evac | ingest=1 capred=0 | uop=0",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | uop=0",
			build: func(h *relHarness) {
				withCurtain(curtainSafe, pairAt(pi2, "evac", S, "supply", S))(h)
				h.wl.seq = []any{curtainSafe, curtainSafe, curtainSafe, curtainLive}
			},
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/co pi swap/curtain live",
			want: "refuse:curtain | evac=staged supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex}, "evac", S, "supply", S)),
			act: pairClick(dispEmpty), probe: probes(pUOP)},
		{name: "d1/co two_robot/both staged",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S),
			act: pairClick(dispEmpty), probe: probes(pUOP)},

		// ── Door 2: the operator's per-order click ───────────────────────
		{name: "d2/two_robot/evac staged",
			bug:   "L4",
			today: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=42 | env:evac uop=nil kind=-",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=1 capred=0 | uop=0 | env:evac uop=nil kind=-", build: pairAt(twoRobot, "evac", S, "supply", D),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP, pEnv)},
		{name: "d2/two_robot/supply alone, evac not staged",
			bug:   "G7-door2",
			today: "ok | evac=dispatched supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42 | env:supply uop=nil kind=-",
			want:  "hold:lift | evac=dispatched supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(twoRobot, "evac", D, "supply", S),
			act: orderClick("supply", dispNone), probe: probes(pUOP, pEnv)},
		{name: "d2/two_robot/queued leg",
			want: "refuse:not-releasable | evac=queued supply=queued | rel=- | ingest=0 capred=0 | uop=42", build: pairAt(twoRobot, "evac", Q, "supply", Q),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP)},
		{name: "d2/two_robot/curtain live",
			want: "refuse:curtain | evac=staged supply=dispatched | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, pairAt(twoRobot, "evac", S, "supply", D)),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP)},
		{name: "d2/two_robot/first cycle curtain live",
			bug:   "A4",
			today: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=42",
			want:  "refuse:curtain | evac=staged supply=dispatched | rel=- | ingest=0 capred=0 | uop=42",
			build: withCurtain(curtainLive, func(h *relHarness) {
				pairAt(twoRobot, "evac", S, "supply", D)(h)
				testutil.MustNoErr(h.t, h.db.SetProcessNodeRuntime(h.nodeID, nil, fxCount), "unstamp")
			}),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP)},
		{name: "d2/co two_robot/evac, curtain live",
			bug:   "curtain-exempt",
			today: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=42",
			want:  "refuse:curtain | evac=staged supply=dispatched | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", D)),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP)},
		{name: "d2/co drop/evac, curtain live",
			bug:   "curtain-exempt",
			today: "ok | evac=in_transit | rel=evac | ingest=0 capred=0 | uop=42 | env:evac uop=0 kind=release_empty",
			want:  "refuse:curtain | evac=staged | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobot, drop: true}, "evac", S)),
			act: orderClick("evac", dispEmpty), probe: probes(pUOP, pEnv)},
		{name: "d2/co drop/evac",
			want: "ok | evac=in_transit | rel=evac | ingest=0 capred=0 | uop=42 | env:evac uop=7 kind=release_partial/7", build: coAt(coSpec{mode: protocol.SwapModeTwoRobot, drop: true}, "evac", S),
			act: orderClick("evac", dispPartial(7)), probe: probes(pUOP, pEnv)},
		{name: "d2/two_robot consume/send partial 0",
			bug:   "L7",
			today: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=42 | env:evac uop=42 kind=release_partial/42",
			want:  "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=0 | env:evac uop=0 kind=release_empty", build: pairAt(twoRobotConsume, "evac", S, "supply", D),
			act: orderClick("evac", dispPartial(0)), probe: probes(pUOP, pEnv)},
		{name: "d2/two_robot consume/send partial 5",
			want: "ok | evac=in_transit supply=dispatched | rel=evac | ingest=0 capred=0 | uop=5 | env:evac uop=5 kind=release_partial/5", build: pairAt(twoRobotConsume, "evac", S, "supply", D),
			act: orderClick("evac", dispPartial(5)), probe: probes(pUOP, pEnv)},
		{name: "d1/two_robot consume/send partial 0",
			bug:   "L7",
			today: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | uop=42 | env:evac uop=42 kind=release_partial/42 | env:supply uop=nil kind=-",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=0 capred=0 | uop=0 | env:evac uop=0 kind=release_empty | env:supply uop=nil kind=-", build: pairAt(twoRobotConsume, "evac", S, "supply", S),
			act: pairClick(dispPartial(0)), probe: probes(pUOP, pEnv)},

		// ── Door 5: the changeover sweep and per-node click ──────────────
		{name: "d5/co two_robot/both staged",
			want: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S),
			act: sweepClick(dispNone), probe: probes(pUOP)},
		{name: "d5/co two_robot/curtain live",
			bug:   "curtain-exempt",
			today: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | uop=42",
			want:  "refuse:curtain | evac=staged supply=staged | rel=- | ingest=0 capred=0 | sweep released=0 pending=1 flip=[] | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobot}, "evac", S, "supply", S)),
			act: sweepClick(dispNone), probe: probes(pUOP)},
		{name: "d5/co pi tooling/evac staged",
			want: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | sweep released=1 pending=1 flip=[] | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}, "evac", S, "supply", S),
			act: sweepClick(dispNone), probe: probes(pUOP)},
		{name: "d5/co pi tooling/node click",
			want: "ok | evac=in_transit supply=staged | rel=evac | ingest=0 capred=0 | node released=1 pending=1 flip=[] | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, tooling: true}, "evac", S, "supply", S),
			act: nodeCOClick(dispNone), probe: probes(pUOP)},

		// ── Door 6: the station button on a single-leg changeover node ────
		{name: "d6/co pi marked/single leg",
			want: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, marked: true}, "supply", S),
			act: pairClick(dispNone), probe: probes(pUOP)},
		{name: "d6/co carryover/round trip at its hold",
			want: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, carryover: true}, "supply", S),
			act: pairClick(dispNone), probe: probes(pUOP)},
		{name: "d6/co carryover/round trip at its hold, curtain live",
			bug:   "curtain-exempt",
			today: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42",
			want:  "refuse:curtain | supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, carryover: true}, "supply", S)),
			act: pairClick(dispNone), probe: probes(pUOP)},
		// The per-position fan-out: one order per position, and no station wait
		// anywhere in it — the robot lifts the press's bin at dispatch. Nothing
		// is left for a release door to gate (the S7 population).
		{name: "d6/co per-position/no wait to release",
			want: "ok | supply=dispatched | rel=- | ingest=0 capred=0 | uop=42", build: coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, perPosition: true}, "supply", protocol.StatusDispatched),
			act: pairClick(dispNone), probe: probes(pUOP)},
		{name: "d6/co pi marked/single leg, curtain live",
			bug:   "curtain-exempt",
			today: "ok | supply=in_transit | rel=supply | ingest=0 capred=0 | uop=42",
			want:  "refuse:curtain | supply=staged | rel=- | ingest=0 capred=0 | uop=42", build: withCurtain(curtainLive, coAt(coSpec{mode: protocol.SwapModeTwoRobotPressIndex, marked: true}, "supply", S)),
			act: pairClick(dispNone), probe: probes(pUOP)},
	}
}
