package engine

import (
	"errors"
	"fmt"
	"shingo/protocol/testutil"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/release"
	"shingoedge/uop"
)

// release_s5_test.go — S5's pins: the intent, its wakes, and G7 over Core's
// point. Same runner and outcome as the matrix (release_matrix_test.go).

// intentOf renders a leg's release intent for the outcome: "-" for none,
// "held@N" for one not yet sent, "sent@N" once its release went out.
func intentOf(h *relHarness, leg string) string {
	in, err := release.DecodeIntent(h.order(leg).ReleaseIntent)
	switch {
	case err != nil:
		return "unreadable"
	case in == nil:
		return "-"
	case in.Sent():
		return fmt.Sprintf("sent@%d", in.StationWait)
	}
	return fmt.Sprintf("held@%d", in.StationWait)
}

func pIntents(h *relHarness) []string {
	var out []string
	for _, l := range h.legs {
		out = append(out, "intent:"+l.name+"="+intentOf(h, l.name))
	}
	return out
}

// lifts reports Core's BinPickedUp for a leg lifting at node (anywhere: the
// wake comes before the handler's station-node filter).
func lifts(leg, node string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.lifted[node] = true
		h.eng.HandleBinPickedUp(h.order(leg).UUID, fxBin, node)
		return nil
	}
}

// floor runs the intent worker's 15 s floor once.
func floor(h *relHarness) error { h.eng.wakeIntentNodes("floor"); return nil }

// coreBack makes Core reachable again for releasePoints.
func coreBack(h *relHarness) error {
	h.core.mu.Lock()
	h.core.pointsDown = false
	h.core.mu.Unlock()
	return nil
}

// stagesAtLane reports Core's OrderStaged at a lane wait (no station
// ordinal): the lane's own coordination, which no station button releases.
func stagesAtLane(leg string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: h.order(leg).UUID,
			Detail: "stub: lane wait", WaitKind: protocol.WaitKindLane})
		return nil
	}
}

// liftsElsewhere is a lift Core's point records at node while the BinPickedUp
// names a location the pickup handler's station filter ignores: the intent
// wake must not depend on that filter.
func liftsElsewhere(leg, node string) func(h *relHarness) error {
	return func(h *relHarness) error {
		h.lifted[node] = true
		h.eng.HandleBinPickedUp(h.order(leg).UUID, fxBin, "SYN-AISLE-9")
		return nil
	}
}

// held runs a click that must hold (not refuse, not fail) part-way through a
// cell, and records the hold's gate in the outcome.
func held(act func(h *relHarness) error) func(h *relHarness) error {
	return func(h *relHarness) error {
		err := act(h)
		var he *release.HeldError
		if !errors.As(err, &he) {
			return fmt.Errorf("want the click held, got %v", err)
		}
		h.extra = append(h.extra, "click=held:"+he.Gate)
		return nil
	}
}

// pCurtainNotes counts the curtain-hold notes on each leg's order_history.
func pCurtainNotes(h *relHarness) []string {
	var out []string
	for _, l := range h.legs {
		hist, err := h.db.ListOrderHistory(l.id)
		testutil.MustNoErr(h.t, err, "history "+l.name)
		n := 0
		for _, r := range hist {
			if strings.HasPrefix(r.Detail, "Release the light curtain at") {
				n++
			}
		}
		out = append(out, fmt.Sprintf("notes:%s=%d", l.name, n))
	}
	return out
}

func TestReleaseS5Pins(t *testing.T) {
	t.Parallel()
	twoRobot := pairSpec{mode: protocol.SwapModeTwoRobot}
	twoRobotConsume := pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume}
	pi2 := pairSpec{mode: protocol.SwapModeTwoRobotPressIndex}
	S, D, T := protocol.StatusStaged, protocol.StatusDispatched, protocol.StatusInTransit
	runRelCells(t, []relCell{
		// A bypass can last a shift, and the held legs are re-planned on every
		// press and every floor: the sentence is written and noted once.
		{name: "S5/a curtain hold is noted once, however often it is re-planned",
			want:  "ok | evac=staged supply=staged | rel=- | ingest=1 capred=0 | click=held:G6 | click=held:G6 | notes:evac=1 | notes:supply=1",
			build: withCurtain(curtainLive, pairAt(twoRobot, "evac", S, "supply", S)),
			act:   seq(held(pairClick(dispEmpty)), floor, floor, held(pairClick(dispEmpty)), floor),
			probe: pCurtainNotes},
		// The pair click remembers a supply not yet at its wait (G1); an Edge
		// restart keeps it (the intent is on the row); the supply stages, then
		// waits for the evac's lift (G7) and goes at it.
		{name: "S5/a held supply survives a restart and goes at the evac's lift",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act: seq(pairClick(dispEmpty), func(h *relHarness) error { h.restart(); return nil },
				stagesAt("supply", 0), lifts("evac", fxPress)),
			probe: pIntents},
		// N-b: R2 staged, R1 still driving to its wait. Both hold (R2 waits on
		// R1's lift, R1's co-release partner is not going); they go together
		// when R1 parks.
		{name: "S5/press-index unflipped, R1 driving: both hold, then go together when R1 parks",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | click=held:G7 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(pi2, "evac", T, "supply", S),
			act:   seq(held(pairClick(dispEmpty)), stagesAt("evac", 0)),
			probe: pIntents},
		// A Core rejection clears the intent in its own statement before the
		// rollback's emit: the leg does not re-fire by itself.
		{name: "S5/a rejected release clears its intent: no re-fire",
			want:  "ok | evac=staged supply=in_transit | rel=evac,supply | ingest=0 capred=0 | intent:evac=- | intent:supply=sent@0",
			build: pairAt(twoRobotConsume, "evac", S, "supply", S),
			act:   seq(pairClick(dispEmpty), refuse("evac", "invalid_state"), stagesAt("evac", 0)),
			probe: pIntents},
		// Core's fleet refused the appended segment and re-staged the leg at
		// the same wait after its release went out (the faulted no-op): the
		// release is re-sent once, and only once.
		{name: "S5/staged again at the same wait: re-sent once",
			want:  "ok | evac=staged supply=in_transit | rel=evac,supply,evac | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act: seq(pairClick(dispEmpty),
				func(h *relHarness) error { h.setStatus("evac", S); return nil }, stagesAt("evac", 0),
				func(h *relHarness) error { h.setStatus("evac", S); return nil }, stagesAt("evac", 0)),
			probe: pIntents},
		// G3: Core unreachable holds; the floor fires it when Core answers.
		{name: "S5/Core unreachable: held, then the floor releases when Core is back",
			want: "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | click=held:G3 | intent:evac=sent@0 | intent:supply=sent@0",
			build: func(h *relHarness) {
				pairAt(twoRobot, "evac", S, "supply", S)(h)
				h.core.mu.Lock()
				h.core.pointsDown = true
				h.core.mu.Unlock()
			},
			act:   seq(held(pairClick(dispEmpty)), coreBack, floor),
			probe: pIntents},
		// The lift is the wake wherever it is reported.
		{name: "S5/a held supply goes at a lift reported away from the station",
			want:  "ok | evac=in_transit supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=sent@0 | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stagesAt("supply", 0), liftsElsewhere("evac", fxPress)),
			probe: pIntents},
		// G2: a leg parked at a lane wait is the lane's, not the button's.
		{name: "S5/a leg at a lane wait is out of the act's scope",
			gate:  "G2",
			want:  "hold:lift | evac=staged supply=staged | rel=- | ingest=1 capred=0 | intent:evac=- | intent:supply=held@0",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act:   seq(stagesAtLane("evac"), pairClick(dispEmpty)),
			probe: pIntents},
		// The per-order click names one order: at a lane wait it refuses in
		// Core's words instead of reporting a release that sent nothing.
		{name: "S5/per-order click on a leg at a lane wait: refused",
			gate:  "G2",
			want:  "err:order 2 is waiting on Core, not on the station:  | evac=staged supply=staged | rel=- | ingest=0 capred=0 | intent:evac=- | intent:supply=-",
			build: pairAt(twoRobot, "evac", S, "supply", S),
			act:   seq(stagesAtLane("evac"), orderClick("evac", dispEmpty)),
			probe: pIntents},
		// The sibling ending is a wake: with the evac gone there is nothing
		// left for the held supply to wait on.
		{name: "S5/a held supply goes when its evac ends",
			want:  "ok | evac=confirmed supply=in_transit | rel=evac,supply | ingest=1 capred=0 | intent:evac=- | intent:supply=sent@0",
			build: pairAt(twoRobot, "evac", S, "supply", D),
			act:   seq(pairClick(dispEmpty), stagesAt("supply", 0), confirms("evac")),
			probe: pIntents},
	})
}

// ── A stopped engine ──────────────────────────────────────────────────────

// engineLog collects an engine's log lines. A started engine logs from its own
// goroutines, which can outlive the test that started it: t.Logf there would
// panic the binary, so the lines are kept and a failure prints them.
type engineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *engineLog) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

// holding is the lines that contain s.
func (l *engineLog) holding(s string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

// startOn starts an engine on the harness's database the way a plant's
// composition root does (engine.New, Start, then its own delta accumulator),
// with the harness's fake Core and curtain script behind it. Stopped at the
// test's end; a test that stops it sooner stops its accumulator first, as the
// root does.
func (h *relHarness) startOn() (*Engine, *uop.Mutator, *engineLog) {
	h.t.Helper()
	lg := &engineLog{}
	eng := New(Config{
		AppConfig: &config.Config{StationUID: "test.station", Web: config.WebConfig{AutoConfirm: true}},
		DB:        h.db, LogFunc: lg.logf, Warlink: h.wl,
	})
	eng.SetReleasePointSource(h.core)
	eng.Start()
	h.t.Cleanup(eng.Stop)
	bridgeCore(h.t, eng, h.core.srv.URL)
	eng.cfg.SimCoreAPI = h.core.srv.URL
	mut := uop.New(h.db, "test.station", h.db, h.db)
	eng.SetInventoryDeltaSink(mut)
	return eng, mut, lg
}

// intentWorkerRunning reports whether eng's release-intent worker goroutine is
// still there to drain a wake.
func intentWorkerRunning(eng *Engine) bool {
	eng.intents.mu.Lock()
	defer eng.intents.mu.Unlock()
	return eng.intents.running
}

// TestIntentWorker_AStoppedEngineReleasesNothing: Stop ends the release-intent
// worker and its 15 s floor, and a wake on a stopped engine does nothing.
//
// Engine A is started and stopped; engine B is started on the same database
// with its own accumulator, counts six strokes at a consume seat, and holds
// both legs of the seat's swap with an unsent intent (Core's point was
// unreachable at the click, G3). A's floor then runs. A used to keep its
// worker and its floor for the life of the process, so the wake re-planned the
// held legs on A and A released the outgoing cart with a fence of net 0: the
// six draws are in B's accumulator, which A's fence cannot see, and Core
// applied them a second time on top of the declared count. That is docket
// G12's double count (two engines on one database is the scenario rig's world,
// every W6 cell's first Edge; a plant builds one).
//
// B's own floor is the control: it releases the same leg once, declaring the
// seat's 36 as of net -6, so the leg A left alone was one a floor could send.
func TestIntentWorker_AStoppedEngineReleasesNothing(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	pairAt(pairSpec{mode: protocol.SwapModeTwoRobot, role: protocol.ClaimRoleConsume},
		"evac", protocol.StatusStaged, "supply", protocol.StatusStaged)(h)
	style, err := h.db.GetActiveStyleID(h.processID)
	testutil.MustNoErr(t, err, "read the active style")

	a, aSink, aLog := h.startOn()
	aSink.Stop()
	a.Stop()

	b, bSink, _ := h.startOn()
	h.eng, h.mutator, h.handler = b, bSink, newEdgeHandlerFor(b)
	for i := 0; i < 6; i++ {
		b.handleCounterDelta(CounterDeltaEvent{ProcessID: h.processID, StyleID: *style, Delta: 1})
	}
	h.core.mu.Lock()
	h.core.pointsDown = true
	h.core.mu.Unlock()
	h.mark()
	clickErr := b.ReleaseStagedOrders(h.nodeID, ReleaseDisposition{Mode: DispositionSendPartialBack, CalledBy: "operator"})
	var held *release.HeldError
	if !errors.As(clickErr, &held) || held.Gate != release.G3 {
		t.Fatalf("the click with Core's point unreachable = %v, want it held at G3", clickErr)
	}
	if got := intentOf(h, "evac"); got != "held@0" {
		t.Fatalf("the evac's intent after the held click = %s, want held@0", got)
	}
	testutil.MustNoErr(t, coreBack(h), "Core's point back")

	a.wakeIntentNodes("floor")
	// A's worker leaves on the stop channel, and until Stop joins it that is
	// off Stop's goroutine: wait for it to have left, or, where it never
	// leaves, for the release it makes of this wake. Each ends on a fact.
	deadline := time.Now().Add(10 * time.Second)
	for intentWorkerRunning(a) && intentOf(h, "evac") == "held@0" {
		if time.Now().After(deadline) {
			t.Fatal("the stopped engine's worker neither left nor released in 10 s")
		}
		time.Sleep(time.Millisecond)
	}
	if intentWorkerRunning(a) {
		t.Errorf("the stopped engine's release-intent worker is still running")
	}
	if lines := aLog.holding("release intent: order"); len(lines) != 0 {
		t.Errorf("the stopped engine released on its floor: %q", lines)
	}

	b.wakeIntentNodes("floor")
	for intentOf(h, "evac") == "held@0" {
		if time.Now().After(deadline) {
			t.Fatal("the running engine's floor did not release the held evac in 10 s")
		}
		time.Sleep(time.Millisecond)
	}
	// The intent is marked sent inside the act, under the node's lock: taking
	// the lock once waits the rest of the act out, so the test does not end
	// under it.
	b.lockNode(h.nodeID)()
	envs := h.releaseEnvelopes()["evac"]
	if len(envs) != 1 {
		t.Fatalf("the evac got %d OrderRelease envelopes, want 1 (the running engine's): %+v", len(envs), envs)
	}
	rel := envs[0]
	if rel.AsOf == nil || rel.AsOf.AsOfNet != -6 {
		t.Errorf("the evac's release fence = %+v, want as of net -6: the six draws the running engine holds unflushed", rel.AsOf)
	}
	if rel.RemainingUOP == nil || *rel.RemainingUOP != fxCount-6 {
		t.Errorf("the evac's declared count = %d, want %d", deref(rel.RemainingUOP), fxCount-6)
	}
}
