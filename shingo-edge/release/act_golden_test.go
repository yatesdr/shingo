package release

import (
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
)

// The act half of the golden (plan_golden_test.go): where a leg stands
// (PointOf), and what an act decides over its legs (PlanAct) — scope, the
// trunk, G1, G2, G3, G6, G7, re-evaluation and commit order.

func iptr(v int) *int { return &v }

var swapWaits = []string{string(PurposeSwap)}

func writeActGolden(b *strings.Builder) {
	b.WriteString("\n## PointOf — where a leg stands\n")
	sent := &Intent{StationWait: 0, SentAt: "2026-09-30T00:00:00Z"}
	held := &Intent{StationWait: 1}
	coWaits := []string{string(PurposeReady), string(PurposeToolingDone)}
	for _, c := range []struct {
		name     string
		status   protocol.Status
		wait     *int
		kind     string
		intent   *Intent
		purposes []string
	}{
		{"staged at station wait 0", protocol.StatusStaged, iptr(0), protocol.WaitKindStation, nil, coWaits},
		{"staged at station wait 1", protocol.StatusStaged, iptr(1), protocol.WaitKindStation, nil, coWaits},
		{"staged at a lane wait, nothing released", protocol.StatusStaged, nil, protocol.WaitKindLane, nil, coWaits},
		{"staged, Core numbers no waits, intent held for 1", protocol.StatusStaged, nil, "", held, coWaits},
		{"driving, released at 0", protocol.StatusInTransit, iptr(0), protocol.WaitKindStation, sent, coWaits},
		{"driving past its last wait", protocol.StatusInTransit, iptr(1), protocol.WaitKindStation, nil, coWaits},
		{"dispatched, not yet at a wait", protocol.StatusDispatched, nil, "", nil, coWaits},
		{"a plan with no station waits", protocol.StatusStaged, nil, "", nil, nil},
	} {
		p := PointOf(c.status, c.wait, c.kind, c.intent, c.purposes)
		fmt.Fprintf(b, "%s\n  ordinal=%d purpose=%q at-wait=%v driving=%v has-wait=%v lane=%v no-waits=%v\n",
			c.name, p.Ordinal, p.Purpose, p.AtWait, p.Driving, p.HasWait, p.AtLane, p.NoWaits)
	}

	b.WriteString("\n## PlanAct — an act over its legs\n")
	for _, c := range actCases() {
		fmt.Fprintf(b, "%s\n%s", c.name, renderAct(PlanAct(c.act, c.facts, c.legs)))
	}
}

type actCase struct {
	name  string
	act   Act
	facts ActFacts
	legs  []ActLeg
}

func actCases() []actCase {
	pair := Act{Origin: OriginStationPair}
	station := Act{Origin: OriginStationOrder}
	sweep := Act{Origin: OriginChangeoverSweep}
	intent := Act{Origin: OriginIntent, Reevaluation: true}
	at := func(purpose Purpose, ordinal int, purposes []string) StaticPoint {
		return StaticPoint{Ordinal: ordinal, Purpose: purpose, AtWait: true, HasWait: true, NoWaits: len(purposes) == 0}
	}
	leg := func(id int64, label string, status protocol.Status, pt StaticPoint, f func(*Leg)) ActLeg {
		l := baseLeg()
		l.OrderID, l.Label, l.Status = id, label, status
		if label == "supply" {
			l.IsSupply, l.Mode = true, ""
		}
		if f != nil {
			f(&l)
		}
		return ActLeg{Leg: l, Point: pt, Press: "pair:5"}
	}
	evac := func(pt StaticPoint, f func(*Leg)) ActLeg { return leg(11, "evac", protocol.StatusStaged, pt, f) }
	supply := func(pt StaticPoint, f func(*Leg)) ActLeg { return leg(12, "supply", protocol.StatusStaged, pt, f) }
	driving := StaticPoint{Ordinal: 0, Purpose: PurposeSwap, Driving: true, HasWait: true}
	notMoving := StaticPoint{Ordinal: 0, Purpose: PurposeSwap, HasWait: true}
	swap := at(PurposeSwap, 0, swapWaits)
	points := func(m map[int64]Point) ActFacts { return ActFacts{Loaded: NeedPoint, Points: m} }
	found := Point{Found: true, Enters: []string{"SYN-PRESS"}}
	awaits := func(lifter int64, node string, co bool) Point {
		return Point{Found: true, Enters: []string{node}, AwaitsLift: []LiftDep{{Lifter: lifter, Node: node, CoRelease: co}}}
	}
	return []actCase{
		{"pair: both at their swap wait, nothing to wait on", pair,
			points(map[int64]Point{11: found, 12: found}), []ActLeg{evac(swap, nil), supply(swap, nil)}},
		{"pair: the supply sets down where the evac has not lifted (G7)", pair,
			points(map[int64]Point{11: found, 12: awaits(11, "SYN-PRESS", false)}), []ActLeg{supply(swap, nil), evac(swap, nil)}},
		{"pair: press-index, both staged, Core says they co-release", pair,
			points(map[int64]Point{11: found, 12: awaits(11, "SYN-B", true)}), []ActLeg{supply(swap, nil), evac(swap, nil)}},
		{"pair: each waits on the other's lift (two-way)", pair,
			points(map[int64]Point{11: awaits(12, "SYN-B", false), 12: awaits(11, "SYN-PRESS", false)}),
			[]ActLeg{evac(swap, nil), supply(swap, nil)}},
		{"pair: the supply driving to its swap wait: released ahead (the echo)", pair,
			points(map[int64]Point{11: found, 12: found}),
			[]ActLeg{evac(swap, nil), leg(12, "supply", protocol.StatusInTransit, driving, nil)}},
		{"pair: the supply not yet moving (G1, remembered)", pair,
			points(map[int64]Point{11: found}),
			[]ActLeg{evac(swap, nil), leg(12, "supply", protocol.StatusQueued, notMoving, nil)}},
		{"per-order click on a leg not yet moving (G1)", station, points(nil),
			[]ActLeg{leg(11, "evac", protocol.StatusQueued, notMoving, nil)}},
		{"pair: the evac at a lane wait (G2)", pair,
			points(map[int64]Point{12: found}),
			[]ActLeg{evac(StaticPoint{AtLane: true, HasWait: true, Purpose: PurposeSwap}, nil), supply(swap, nil)}},
		{"per-order click on a leg at a lane wait (G2)", station, ActFacts{},
			[]ActLeg{evac(StaticPoint{AtLane: true, HasWait: true, Purpose: PurposeSwap}, nil)}},
		{"pair: Core unreachable (G3)", pair, ActFacts{Loaded: NeedPoint, PointErr: errors.New("core API not configured")},
			[]ActLeg{evac(swap, nil), supply(swap, nil)}},
		{"pair: a curtained node on the point (G6)", pair, points(map[int64]Point{11: found, 12: found}),
			[]ActLeg{evac(swap, func(l *Leg) {
				l.Curtain = &CurtainHeldError{Node: "SYN-PRESS", Sentence: "Release the light curtain at SYN-PRESS, then press RELEASE again."}
			}), supply(swap, nil)}},
		{"pair: a configuration refusal refuses the act (G4)", pair, ActFacts{},
			[]ActLeg{evac(swap, func(l *Leg) { l.NodeErr = errRead }), supply(swap, nil)}},
		{"explicit ready on a swap wait: out of scope", Act{Origin: OriginChangeoverNode, Purpose: PurposeReady}, ActFacts{},
			[]ActLeg{evac(swap, nil)}},
		{"no purpose named: the earliest owed (ready before tooling done)", Act{Origin: OriginChangeoverNode},
			points(map[int64]Point{11: found}),
			[]ActLeg{evac(at(PurposeReady, 0, coWaitsFor()), nil), supply(at(PurposeToolingDone, 1, coWaitsFor()), nil)}},
		{"a legacy wait with no purpose: covered", pair, points(map[int64]Point{11: found}),
			[]ActLeg{evac(StaticPoint{AtWait: true, HasWait: true}, nil)}},
		{"no station waits: the pair door covers it", pair, points(map[int64]Point{11: found}),
			[]ActLeg{evac(StaticPoint{NoWaits: true}, nil)}},
		{"no station waits: the sweep leaves it", sweep, ActFacts{},
			[]ActLeg{evac(StaticPoint{NoWaits: true}, nil)}},
		{"terminal leg: out", pair, ActFacts{},
			[]ActLeg{leg(11, "evac", protocol.StatusConfirmed, StaticPoint{}, nil)}},
		{"re-evaluation: a leg at its intent's wait goes", intent, points(map[int64]Point{12: found}),
			[]ActLeg{supply(swap, func(l *Leg) { l.Intent = &Intent{StationWait: 0} })}},
		{"re-evaluation: a refusal holds for the next click", intent, ActFacts{},
			[]ActLeg{supply(swap, func(l *Leg) { l.Intent = &Intent{StationWait: 0}; l.NodeErr = errRead })}},
	}
}

func coWaitsFor() []string { return []string{string(PurposeReady), string(PurposeToolingDone)} }

func renderAct(p ActPlan) string {
	var b strings.Builder
	if p.Need != 0 {
		return fmt.Sprintf("  needs %d (leg %d)\n", p.Need, p.NeedLeg)
	}
	fmt.Fprintf(&b, "  purpose=%q%s\n", p.Purpose, renderLog(p.Logs))
	if p.Refusal != nil {
		fmt.Fprintf(&b, "  refuse %s: %v\n", p.Gate, p.Refusal)
		return b.String()
	}
	for _, d := range p.Decisions {
		switch d.Verdict {
		case Go:
			fmt.Fprintf(&b, "  order %d: %s\n", d.OrderID, renderLeg(d.Trunk))
		case Hold:
			fmt.Fprintf(&b, "  order %d: hold %s wake=%s: %s%s\n", d.OrderID, d.Gate, d.Wake, d.Sentence, renderLog(d.Logs))
		case Out:
			fmt.Fprintf(&b, "  order %d: out %s%s\n", d.OrderID, d.Gate, renderLog(d.Logs))
		default:
			fmt.Fprintf(&b, "  order %d: %s\n", d.OrderID, renderLeg(d.Trunk))
		}
	}
	if err := p.Held(); err != nil {
		fmt.Fprintf(&b, "  held: %v\n", err)
	}
	return b.String()
}
