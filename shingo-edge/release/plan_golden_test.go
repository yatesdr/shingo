package release

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
)

// The matrix golden: every plan function over named synthetic snapshots, one
// line each — verdict, gate, refusal, the release's shape, and every log line
// the decision writes. A change to any decision shows as a diff in
// testdata/plan.golden. Regenerate with:
//
//	go test ./release -run TestPlanGolden -update
//
// The engine's matrix (release_matrix_test.go) proves the doors reach these
// decisions through the real loader and commit; this file proves what the
// decisions are, with no I/O at all.

var update = flag.Bool("update", false, "rewrite testdata/plan.golden")

var errRead = errors.New("disk I/O error")

func ptr(v int64) *int64 { return &v }

// allLoaded marks every leg fact group read, as a door's loop leaves it.
const allLoaded = NeedOrder | NeedCurtain | NeedNode | NeedPull | NeedRuntime | NeedKind | NeedSupply | NeedDeparts | NeedFlip

func baseLeg() Leg {
	return Leg{Loaded: allLoaded, OrderID: 11, Label: "evac", TaskNode: "SYN-PRESS", Mode: ModeCaptureLineside,
		Status: protocol.StatusStaged, HasProcessNode: true, ProcessNodeID: 5, NodeName: "Press 1", CoreNode: "SYN-PRESS",
		ActiveClaim: "7", ClaimResolved: true}
}

func renderLog(logs []Log) string {
	if len(logs) == 0 {
		return ""
	}
	sinks := map[Sink]string{SinkRelease: "release", SinkEngine: "engine", SinkStd: "std"}
	var out []string
	for _, l := range logs {
		out = append(out, sinks[l.Sink]+": "+l.Text)
	}
	return "\n    log " + strings.Join(out, "\n    log ")
}

func renderLeg(p LegPlan) string {
	switch p.Verdict {
	case Refuse:
		return fmt.Sprintf("refuse %s: %v%s", p.Gate, p.Refusal, renderLog(p.Logs))
	case Skip:
		reason := map[SkipReason]string{SkipTerminal: "terminal", SkipNotReleasable: "not-releasable"}[p.Skip]
		return fmt.Sprintf("skip %s: %s%s", p.Gate, reason, renderLog(p.Logs))
	}
	arm := map[Arm]string{ArmPlain: "plain", ArmDrop: "drop", ArmNoClaim: "no-claim", ArmProduce: "produce", ArmLineside: "lineside"}[p.Arm]
	return fmt.Sprintf("go %s flip=%v finalize=%v suppress=%v u1=%v%s", arm, p.Flip, p.Finalize, p.SuppressManifest, p.U1, renderLog(p.Logs))
}

func legCases() []struct {
	name string
	act  Act
	leg  Leg
} {
	station := Act{Origin: OriginStationOrder}
	pair := Act{Origin: OriginStationPair}
	sweep := Act{Origin: OriginChangeoverSweep}
	with := func(f func(*Leg)) Leg { l := baseLeg(); f(&l); return l }
	return []struct {
		name string
		act  Act
		leg  Leg
	}{
		{"door2/read error", station, with(func(l *Leg) { l.ReadErr = errRead })},
		{"pair/read error", pair, with(func(l *Leg) { l.ReadErr = errRead })},
		{"sweep/read error", sweep, with(func(l *Leg) { l.ReadErr = errRead })},
		{"door2/queued", station, with(func(l *Leg) {
			l.Status, l.QueueReason, l.QueueCode = protocol.StatusQueued, "no empty slot at SMN_029", "no_slot"
		})},
		{"pair/queued", pair, with(func(l *Leg) { l.Status = protocol.StatusQueued })},
		{"pair/confirmed", pair, with(func(l *Leg) { l.Status = protocol.StatusConfirmed })},
		{"sweep/dispatched", sweep, with(func(l *Leg) { l.Status = protocol.StatusDispatched })},
		{"sweep/cancelled", sweep, with(func(l *Leg) { l.Status = protocol.StatusCancelled })},
		{"door2/curtain live", station, with(func(l *Leg) {
			l.Curtain = &CurtainHeldError{Node: "SYN-PRESS", Sentence: "Release the light curtain at SYN-PRESS, then press RELEASE again."}
		})},
		{"sweep/curtain live", sweep, with(func(l *Leg) {
			l.Curtain = &CurtainHeldError{Node: "SYN-PRESS", Sentence: "Release the light curtain at SYN-PRESS, then press RELEASE again."}
		})},
		{"door2/no process node", station, with(func(l *Leg) { l.HasProcessNode = false })},
		{"door2/node unreadable", station, with(func(l *Leg) { l.NodeErr = errRead })},
		{"door2/pull state unreadable", station, with(func(l *Leg) { l.PullErr, l.PullNode = errRead, "SYN-PRESS" })},
		{"door2/runtime unreadable", station, with(func(l *Leg) { l.RuntimeErr = errRead })},
		{"door2/drop, departing check unreadable", station, with(func(l *Leg) {
			l.Drop, l.DepartErr = true, errors.New("departing-bin check: order 11: load steps: disk I/O error")
		})},
		{"door2/drop of a produce bin", station, with(func(l *Leg) { l.Drop, l.Departs = true, true })},
		{"door2/no claim", station, with(func(l *Leg) { l.ClaimResolved, l.ActiveClaim = false, "<nil>" })},
		{"door2/no claim, partner blocked", station, with(func(l *Leg) {
			l.ClaimResolved, l.ActiveClaim = false, "<nil>"
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, Partner: "SYN-B", NotReady: "SYN-B has no bin on it", Blocked: true}
		})},
		{"door2/supply unclassifiable", station, with(func(l *Leg) {
			l.SupplyErr = errors.New("supply-leg check: order 11: no steps stored")
		})},
		{"door2/departing check unreadable", station, with(func(l *Leg) {
			l.DepartErr = errors.New("departing-bin check: node Press 1: read claim 7: disk I/O error")
		})},
		{"door2/lineside evac", station, with(func(l *Leg) {})},
		{"pair/lineside supply", pair, with(func(l *Leg) { l.IsSupply, l.Label, l.Mode = true, "supply", "" })},
		{"door2/produce evac, bin full", station, with(func(l *Leg) { l.Produce, l.Departs = true, true })},
		{"door2/produce supply", station, with(func(l *Leg) { l.Produce, l.IsSupply = true, true })},
		{"door2/produce, consume-side capture mode absent", station, with(func(l *Leg) { l.Produce, l.Mode = true, "" })},
		{"door2/sequential, flip to a ready partner", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, Partner: "SYN-B"}
		})},
		{"door2/sequential, partner has no bin (count waits)", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, Partner: "SYN-B", NotReady: "SYN-B has no bin on it"}
		})},
		{"door2/sequential, partner's outgoing carrier bound (L3-co)", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, Partner: "SYN-B",
				NotReady: "SYN-B's changeover order 2 has not delivered (submitted) — release it first", Blocked: true}
		})},
		{"door2/sequential, parked side (own bit off)", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A"}
		})},
		{"door2/sequential, runtime row missing", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", RuntimeErr: ErrRuntimeMissing}
		})},
		{"door2/sequential, partner unresolved", station, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, PartnerErr: errors.New("paired node SYN-B not found")}
		})},
		{"sweep/flip refused", sweep, with(func(l *Leg) {
			l.Flip = Flip{Applies: true, Node: "SYN-A", OwnPull: true, PartnerErr: errors.New("paired node SYN-B not found")}
		})},
	}
}

func pairCases() []struct {
	name string
	p    Pair
} {
	all := NeedRoute | NeedActive | NeedPair | NeedCollision | NeedCurtain | NeedDeparts
	base := func() Pair {
		return Pair{Loaded: all, NodeID: 5, NodeName: "Press 1", ClaimResolved: true, Mode: protocol.SwapModeTwoRobot,
			Evac: ptr(11), Supply: ptr(12)}
	}
	with := func(f func(*Pair)) Pair { p := base(); f(&p); return p }
	pi := func(p *Pair) { p.Mode = protocol.SwapModeTwoRobotPressIndex }
	arms := func(supplyState, evacState protocol.Status, evacPlaces string) []CollisionArm {
		return []CollisionArm{
			{Leg: 12, Sibling: 11, LegStatus: supplyState, SiblingState: evacState, PlacesAt: "SYN-PRESS"},
			{Leg: 11, Sibling: 12, LegStatus: evacState, SiblingState: supplyState, PlacesAt: evacPlaces},
		}
	}
	return []struct {
		name string
		p    Pair
	}{
		{"single-leg changeover task: the changeover act", with(func(p *Pair) { p.TaskSupply = true })},
		{"relay task, single_robot claim (§6.4)", with(func(p *Pair) {
			p.TaskEvac, p.TaskSupply, p.Mode = true, true, protocol.SwapModeSingleRobot
		})},
		{"changeover pair, two_robot claim: the pair path", with(func(p *Pair) { p.TaskEvac, p.TaskSupply = true, true })},
		{"node unreadable", with(func(p *Pair) { p.LoadErr = errRead })},
		{"no claim", with(func(p *Pair) { p.ClaimResolved = false })},
		{"not a two-robot mode", with(func(p *Pair) { p.Mode = protocol.SwapModeSequential })},
		{"no pair resolves", with(func(p *Pair) {
			p.ResolveErr, p.Evac, p.Supply = errors.New("no tracked orders to release"), nil, nil
		})},
		{"slots inverted by the steps", with(func(p *Pair) { p.Relabelled, p.SlotEvac, p.SlotSupply = true, 12, 11 })},
		{"press-index, R2 staged, R1 queued", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusQueued, "SYN-B")
		})},
		{"press-index, R2 staged, R1 driving to its wait (N-b)", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusInTransit, "SYN-B")
		})},
		{"press-index, R1 released earlier and driving on", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusInTransit, "SYN-B")
			p.Arms[0].SiblingPassed, p.Arms[1].LegPassed = true, true
		})},
		{"press-index flipped, R1 staged, R2 queued", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusQueued, protocol.StatusStaged, "")
		})},
		{"press-index unflipped, R1 staged, R2 queued", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusQueued, protocol.StatusStaged, "SYN-B")
		})},
		{"press-index, sibling history unreadable", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusInTransit, "SYN-B")
			p.Arms[0].SiblingPassedErr = errRead
		})},
		{"press-index, leg unreadable", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusStaged, "SYN-B")
			p.Arms[0].LegErr = errRead
		})},
		{"press-index, both staged", with(func(p *Pair) {
			pi(p)
			p.Arms = arms(protocol.StatusStaged, protocol.StatusStaged, "SYN-B")
		})},
		{"curtain live", with(func(p *Pair) {
			p.Curtain = &CurtainHeldError{Node: "SYN-PRESS", Sentence: "Release the light curtain at SYN-PRESS, then press RELEASE again."}
		})},
		{"departing check unreadable", with(func(p *Pair) {
			p.DepartErr = errors.New("departing-bin check: node Press 1: read claim 7: disk I/O error")
		})},
		{"departing order unreadable", with(func(p *Pair) { p.Departs, p.DepartingReadErr = true, errRead })},
		{"produce pair: finalize first", with(func(p *Pair) { p.Departs = true })},
		{"consume pair", with(func(p *Pair) {})},
	}
}

func renderPair(p PairPlan) string {
	switch {
	case p.Route:
		return "route: the changeover act"
	case p.Verdict == Refuse:
		return fmt.Sprintf("refuse %s: %v%s", p.Gate, p.Refusal, renderLog(p.Logs))
	}
	return fmt.Sprintf("go finalize=%v evac=%s supply=%s%s", p.Finalize, idStr(p.Evac), idStr(p.Supply), renderLog(p.Logs))
}

func TestPlanGolden(t *testing.T) {
	var b strings.Builder
	b.WriteString("# The release plans over named snapshots (release/plan_golden_test.go).\n")
	b.WriteString("\n## PlanLeg — the trunk\n")
	for _, c := range legCases() {
		fmt.Fprintf(&b, "%s\n  %s\n", c.name, renderLeg(PlanLeg(c.act, c.leg)))
	}
	b.WriteString("\n## PlanPair — door 1\n")
	for _, c := range pairCases() {
		fmt.Fprintf(&b, "%s\n  %s\n", c.name, renderPair(PlanPair(c.p)))
	}
	b.WriteString("\n## PlanDeferral — door 1's deferral\n")
	for _, c := range []struct {
		name string
		d    Deferral
	}{
		{"leg went", Deferral{Leg: 12, Released: true, SiblingReleased: true}},
		{"neither went, sibling never finished", Deferral{Leg: 12}},
		{"sibling went on this click, leg dispatched", Deferral{Leg: 12, SiblingReleased: true, LegStatus: protocol.StatusDispatched}},
		{"sibling already finished its half, leg queued", Deferral{Leg: 12, SiblingSucceeded: true, LegStatus: protocol.StatusQueued}},
		{"sibling went, leg terminal", Deferral{Leg: 12, SiblingReleased: true, LegStatus: protocol.StatusCancelled}},
		{"sibling went, leg releasable (went another way)", Deferral{Leg: 12, SiblingReleased: true, LegStatus: protocol.StatusStaged}},
		{"sibling went, leg unreadable", Deferral{Leg: 12, SiblingReleased: true, LegErr: errRead}},
	} {
		remember, logs := PlanDeferral(c.d)
		fmt.Fprintf(&b, "%s\n  remember=%v%s\n", c.name, remember, renderLog(logs))
	}
	b.WriteString("\n## PlanChangeover — doors 5 and 6\n")
	for _, c := range []struct {
		name string
		c    Changeover
	}{
		{"changeover unreadable", Changeover{ReadErr: errRead}},
		{"sweep: unchanged, out of scope, pulled, unreadable, paired, later wait, lone supply", Changeover{Sweep: true, Tasks: []Task{
			{NodeName: "N-unchanged", Situation: "unchanged"},
			{NodeName: "N-other", Situation: "swap", Evac: ptr(1)},
			{NodeName: "N-pulled", Situation: "swap", InScope: true, Pulling: true, PullNode: "SYN-A", Evac: ptr(2)},
			{NodeName: "N-unread", Situation: "swap", InScope: true, PullErr: errRead, Evac: ptr(3)},
			{NodeName: "N-paired", Situation: "swap", InScope: true, Evac: ptr(4), Supply: ptr(5), SupplyLive: true},
			{NodeName: "N-tooling", Situation: "evacuate", InScope: true, Evac: ptr(6), Supply: ptr(7), SupplyAtLaterWait: true},
			{NodeName: "N-add", Situation: "add", InScope: true, Supply: ptr(8)},
			{NodeName: "N-paired-done", Situation: "swap", InScope: true, Evac: ptr(9), Supply: ptr(10)},
		}}},
		{"node click on a pulled position: the trunk flips", Changeover{Tasks: []Task{
			{NodeName: "N-pulled", Situation: "swap", InScope: true, Pulling: true, Evac: ptr(2)},
		}}},
	} {
		plan := PlanChangeover(c.c)
		fmt.Fprintf(&b, "%s\n", c.name)
		if plan.Refusal != nil {
			fmt.Fprintf(&b, "  refuse: %v\n", plan.Refusal)
			continue
		}
		for _, tp := range plan.Tasks {
			var slots []string
			for _, s := range tp.Slots {
				slots = append(slots, fmt.Sprintf("%s=%d", s.Kind, s.Order))
			}
			fmt.Fprintf(&b, "  %s: in-scope=%v needs-flip=%v slots=[%s] deferred=%v%s\n", c.c.Tasks[tp.Task].NodeName,
				tp.InScope, tp.NeedsFlip, strings.Join(slots, ","), tp.Deferred, renderLog(tp.Logs))
		}
	}
	b.WriteString("\n## PlanMaterial — doors 3 and 4\n")
	allM := NeedActive | NeedCurtain | NeedInFlight
	okM := Material{Loaded: allM, NodeName: "Press 1", Qty: 1, ClaimResolved: true, ClaimNode: "SYN-PRESS", OutboundDest: "SYN-OUT"}
	withM := func(f func(*Material)) Material { m := okM; f(&m); return m }
	for _, c := range []struct {
		name string
		m    Material
	}{
		{"node unreadable", withM(func(m *Material) { m.LoadErr = errRead })},
		{"qty 0", withM(func(m *Material) { m.Qty = 0 })},
		{"no claim", withM(func(m *Material) { m.ClaimResolved = false })},
		{"no outbound destination", withM(func(m *Material) { m.OutboundDest = "" })},
		{"curtain live", withM(func(m *Material) {
			m.Curtain = &CurtainHeldError{Node: "SYN-PRESS", Sentence: "Release the light curtain at SYN-PRESS, then press RELEASE again."}
		})},
		{"the first tap's order unreadable", withM(func(m *Material) { m.InFlightErr = errRead })},
		{"a second tap while the first robot is coming (L8)", withM(func(m *Material) {
			m.InFlight, m.InFlightOrder, m.InFlightStatus = true, 41, protocol.StatusDispatched
		})},
		{"go", okM},
	} {
		p := PlanMaterial(c.m)
		line := "go"
		if p.Verdict == Refuse {
			line = fmt.Sprintf("refuse %s: %v", p.Gate, p.Refusal)
		}
		fmt.Fprintf(&b, "%s\n  %s\n", c.name, line)
	}
	b.WriteString("\n## PlanSurvivor — door 9\n")
	allS := NeedSiblings | NeedScope
	for _, c := range []struct {
		name string
		s    Survivor
	}{
		{"not paired", Survivor{Loaded: allS, OrderID: 12}},
		{"partner has not finished", Survivor{Loaded: allS, OrderID: 12, Paired: true, SiblingID: 11}},
		{"relay leg", Survivor{Loaded: allS, OrderID: 12, Paired: true, SiblingID: 11, SiblingSucceeded: true, Relay: true}},
		{"changeover leg", Survivor{Loaded: allS, OrderID: 12, Paired: true, SiblingID: 11, SiblingSucceeded: true,
			InChangeover: true, TaskID: 3, TaskSituation: "evacuate"}},
		{"already fired this lifetime", Survivor{Loaded: allS, OrderID: 12, Paired: true, SiblingID: 11, SiblingSucceeded: true, AlreadyFired: true}},
		{"owed the click", Survivor{Loaded: allS, OrderID: 12, Paired: true, SiblingID: 11, SiblingSucceeded: true}},
	} {
		p := PlanSurvivor(c.s)
		fmt.Fprintf(&b, "%s\n  release=%v gate=%s%s\n", c.name, p.Release, p.Gate, renderLog(p.Logs))
	}
	b.WriteString("\n## PlanPickup — door 10\n")
	for _, c := range []struct {
		name   string
		passed bool
		err    error
	}{
		{"deferred to this lift", false, nil},
		{"already released past a wait (N-a')", true, nil},
		{"history unreadable", false, errRead},
	} {
		fire, logs := PlanPickup(8, "uuid-evac", c.passed, c.err)
		fmt.Fprintf(&b, "%s\n  release=%v%s\n", c.name, fire, renderLog(logs))
	}

	path := filepath.Join("testdata", "plan.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run with -update to create it): %v", path, err)
	}
	if got := b.String(); got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("the plans moved from %s; if the change is intended, run with -update and review the diff.\n%s",
			path, firstDiff(got, string(want)))
	}
}

// firstDiff shows the first line where got and want part.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(strings.ReplaceAll(want, "\r\n", "\n"), "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d:\n got  %s\n want %s", i+1, gl, wl)
		}
	}
	return ""
}
