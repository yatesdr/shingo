package release

import (
	"fmt"
	"strings"

	"shingo/protocol"
)

// ── The static point ──────────────────────────────────────────────────────

// StaticPoint is where a leg stands against its station waits, from what the
// Edge holds: its status, the station wait Core last staged it at
// (orders.station_wait), its intent, and its waits' purposes.
type StaticPoint struct {
	Ordinal int     // the station wait it is at (AtWait) or heading to
	Purpose Purpose // that wait's purpose; "" past its last station wait
	AtWait  bool    // staged at it
	Driving bool    // in_transit, heading to it
	HasWait bool    // there is such a wait
	// AtLane: staged at a lane wait, which is Core's to release (G2).
	AtLane bool
	// NoWaits: the leg's plan has no station wait at all (a plain order, or
	// one authored before waits): an act covers it as the doors always did.
	NoWaits bool
}

// PointOf locates a leg. A staged leg is at the station wait Core numbered;
// an in_transit leg is heading to the one after the wait its release was for
// (its intent's) or, with no intent, after the one Core last staged it at, or
// to its first. A leg not yet moving is heading to its first.
func PointOf(status protocol.Status, stationWait *int, waitKind string, intent *Intent, purposes []string) StaticPoint {
	var p StaticPoint
	released := func() int {
		switch {
		case intent != nil && intent.Sent():
			return intent.StationWait + 1
		case stationWait != nil:
			return *stationWait + 1
		}
		return 0
	}
	switch {
	case status == protocol.StatusStaged && waitKind == protocol.WaitKindStation && stationWait != nil:
		p.Ordinal, p.AtWait = *stationWait, true
	case status == protocol.StatusStaged:
		// At a lane wait, or staged by a Core that does not number waits: the
		// next station wait is the one after the last released.
		p.Ordinal = released()
		if intent != nil && !intent.Sent() {
			p.Ordinal = intent.StationWait
		}
		p.AtLane = waitKind == protocol.WaitKindLane
		p.AtWait = !p.AtLane
	case status == protocol.StatusInTransit:
		p.Ordinal, p.Driving = released(), true
	default:
		p.Ordinal = 0
	}
	p.NoWaits = len(purposes) == 0
	p.HasWait = p.Ordinal < len(purposes)
	if p.HasWait {
		p.Purpose = Purpose(purposes[p.Ordinal])
	}
	return p
}

// ── The fetched point and the act ─────────────────────────────────────────

// LiftDep is one lift dependency (SHAPE §3.4): this leg sets a bin down on
// Node, which holds a bin now, and Lifter — its Core sibling — lifts from Node
// before any drop of its own there. CoRelease: Core's answer that the lifter
// is staged at the wait whose next segment lifts there, and this leg fetches
// before it places.
type LiftDep struct {
	Lifter    int64
	Node      string
	CoRelease bool
}

// Point is Core's dynamic half of a leg's release point, fetched once per act.
type Point struct {
	Found      bool
	Enters     []string
	AwaitsLift []LiftDep
}

// ActFacts are the facts of the act itself.
type ActFacts struct {
	Loaded   Need            // NeedPoint
	PointErr error           // G3: the act's point could not be fetched
	Points   map[int64]Point // by order id
}

// NeedPoint is the act's one Core read (releasePoints).
const NeedPoint Need = 1 << 30

// ActLeg is one leg of an act: its facts, where it stands, and its press —
// the legs one decision of the operator's covers together (a pair, a
// changeover task), which G1 remembers a not-yet-moving leg for.
type ActLeg struct {
	Leg   Leg
	Point StaticPoint
	Press string
}

// Wake is what re-plans a held leg.
type Wake string

const (
	WakeStaged  Wake = "staged"  // the leg's OrderStaged
	WakeLift    Wake = "lift"    // the lifter's BinPickedUp (any location)
	WakeCurtain Wake = "curtain" // the tag's change; the floor
	WakeCore    Wake = "core"    // Core reachable again; the floor
	WakeClick   Wake = "click"   // an operator's next act
	WakePartner Wake = "partner" // the partner's release
)

// Decision is the act's verdict for one leg.
type Decision struct {
	OrderID  int64
	Index    int // the leg's index in the act
	Verdict  Verdict
	Gate     string
	Sentence string // the hold's or refusal's words, for the chip
	Wake     Wake
	Trunk    LegPlan
	Point    StaticPoint
	Logs     []Log
	// DropIntent: the hold ends the leg's intent (a system-created hold at a
	// live curtain, Q8); the next RELEASE press writes a new one.
	DropIntent bool
}

// Out is a leg outside the act's scope: another decision's wait.
const Out Verdict = 10

// Hold is "not yet": the intent is recorded, the reason shows on the board,
// and a named wake re-evaluates it.
const Hold Verdict = 11

// HeldError is an operator act that released nothing and held at least one
// leg: not a refusal (the press is remembered, the robot goes by itself), so
// it is advisory, and its text is the hold's sentence for the toast.
type HeldError struct {
	Gate      string
	Sentences []string
}

func (h *HeldError) Error() string { return strings.Join(h.Sentences, "; ") }

// Advisory marks a hold as the system working rather than a fault.
func (h *HeldError) Advisory() bool { return true }

// NewHeldError is a hold reported by a door that releases through another
// act (a changeover node's station button).
func NewHeldError(gate string, sentences []string) error {
	return &HeldError{Gate: gate, Sentences: sentences}
}

// Held returns the plan's HeldError when the act let nothing go and held a
// leg, else nil.
func (p ActPlan) Held() error {
	if p.Refusal != nil {
		return nil
	}
	var held *HeldError
	for _, d := range p.Decisions {
		switch d.Verdict {
		case Go:
			return nil
		case Hold:
			if held == nil {
				held = &HeldError{Gate: d.Gate}
			}
			held.Sentences = append(held.Sentences, d.Sentence)
		}
	}
	if held == nil {
		return nil
	}
	return held
}

// ActPlan is the act's decision: a refusal (nothing recorded), or a decision
// for each leg, in commit order (lifters first).
type ActPlan struct {
	Need      Need
	NeedLeg   int // the leg a per-leg Need is for; -1 for the act
	Purpose   Purpose
	Refusal   error
	Gate      string
	Decisions []Decision
	Logs      []Log
}

// purposeOrder is the order a cell owes its decisions in: a changeover's
// ready before its tooling done; the steady state's swap.
var purposeOrder = []Purpose{PurposeReady, PurposeToolingDone, PurposeSwap}

// PurposeOrder is the order a cell owes its decisions in, for the board's
// buttons.
func PurposeOrder() []Purpose { return append([]Purpose(nil), purposeOrder...) }

// PlanAct decides an act over its leg set (SHAPE §3.3-3.5):
//
//  1. scope: a leg is covered when its current or next station wait carries
//     the act's purpose (an act with no purpose takes the earliest decision
//     its legs owe); a re-evaluation covers a leg at its intent's wait;
//  2. the trunk's gates per covered leg (G4, G5, G1 releasable);
//  3. G1: a leg not yet moving holds when another leg goes on the same act
//     (remembered for the other robot of the same press), and is otherwise
//     the per-order click's refusal or a pending leg;
//  4. G3: no point from Core holds every leg that would go;
//  5. G6: a curtained node on the point holds (the press is remembered);
//  6. G7: a lift dependency holds unless its lifter goes in this act and Core
//     says they co-release; holds propagate to a fixed point.
//
// An operator act is refused at the first refusal-class verdict and nothing
// is recorded; a re-evaluation turns a refusal into a hold that waits for a
// click.
func PlanAct(a Act, f ActFacts, legs []ActLeg) ActPlan {
	for i := range legs {
		if !legs[i].Leg.Loaded.Has(NeedOrder) {
			return ActPlan{Need: NeedOrder, NeedLeg: i}
		}
	}
	purpose, explicit := a.Purpose, a.Purpose != ""
	if !explicit && !a.Reevaluation {
		purpose = inferredPurpose(legs)
	}
	plan := ActPlan{Purpose: purpose, NeedLeg: -1}
	decisions := make([]Decision, len(legs))
	for i, l := range legs {
		d := Decision{OrderID: l.Leg.OrderID, Index: i, Point: l.Point}
		if l.Leg.ReadErr == nil && l.Point.AtLane {
			// The per-order click names this one order, so it answers in
			// Core's words rather than reporting a release that sent nothing.
			if !a.passesOverUnreleasable() && !a.Reevaluation {
				return ActPlan{Purpose: purpose, NeedLeg: -1, Gate: G2, Refusal: fmt.Errorf(
					"order %d is waiting on a lane, not on the station: Core releases it when the lane is safe", l.Leg.OrderID)}
			}
			d.Verdict, d.Gate = Out, G2
			decisions[i] = d
			continue
		}
		if l.Leg.ReadErr == nil && !covered(a, purpose, explicit, l) {
			d.Verdict, d.Gate = Out, G1
			decisions[i] = d
			continue
		}
		trunk := PlanLeg(a, l.Leg)
		operator := !a.Reevaluation
		if trunk.Need != 0 {
			return ActPlan{Need: trunk.Need, NeedLeg: i}
		}
		d.Trunk, d.Logs = trunk, trunk.Logs
		switch trunk.Verdict {
		case Go:
			d.Verdict = Go
		case Skip:
			d.Verdict, d.Gate = Skip, G1
		case Refuse:
			if !operator {
				d.Verdict, d.Gate, d.Wake = Hold, trunk.Gate, WakeClick
				d.Sentence = trunk.Refusal.Error()
				break
			}
			return ActPlan{Purpose: purpose, NeedLeg: -1, Refusal: trunk.Refusal, Gate: trunk.Gate, Logs: trunk.Logs}
		}
		decisions[i] = d
	}

	holdForThePress(a, legs, decisions)
	if need := gatesOnThePoint(a, f, legs, decisions); need != nil {
		return *need
	}
	liftDependencies(f, legs, decisions)

	plan.Decisions = commitOrder(f, decisions)
	return plan
}

// holdForThePress is G1's remembering: a leg not yet at its wait, whose
// partner on the same press goes on this act, holds; a re-evaluation keeps
// holding it.
func holdForThePress(a Act, legs []ActLeg, decisions []Decision) {
	pressGoes := map[string]bool{}
	for i, d := range decisions {
		if d.Verdict == Go && legs[i].Press != "" {
			pressGoes[legs[i].Press] = true
		}
	}
	for i := range decisions {
		d := &decisions[i]
		if d.Verdict != Skip || d.Trunk.Skip != SkipNotReleasable {
			continue
		}
		if pressGoes[legs[i].Press] || a.Reevaluation {
			d.Verdict, d.Wake = Hold, WakeStaged
			d.Sentence = "waiting for its robot to reach its wait"
		}
	}
}

// gatesOnThePoint asks Core's point and the curtain on it: G3 (no point) and
// G6 (a curtained node on it) for every leg that would go. It returns the
// fact group still to load, or nil.
func gatesOnThePoint(a Act, f ActFacts, legs []ActLeg, decisions []Decision) *ActPlan {
	if !f.Loaded.Has(NeedPoint) && anyGoOrHold(decisions) {
		return &ActPlan{Need: NeedPoint, NeedLeg: -1}
	}
	for i := range decisions {
		d := &decisions[i]
		if d.Verdict != Go {
			continue
		}
		if f.PointErr != nil {
			d.Verdict, d.Gate, d.Wake = Hold, G3, WakeCore
			d.Sentence = "waiting for Core"
			continue
		}
		if !legs[i].Leg.Loaded.Has(NeedCurtain) {
			return &ActPlan{Need: NeedCurtain, NeedLeg: i}
		}
		if c := legs[i].Leg.Curtain; c != nil {
			d.Verdict, d.Gate, d.Wake = Hold, G6, WakeCurtain
			d.Sentence = curtainHoldSentence(c)
			// Q8: a system-created hold has no press to remember. It is
			// dropped, and the chip asks for one in the curtain's own words.
			if in := legs[i].Leg.Intent; a.Reevaluation && in != nil && in.System {
				d.DropIntent, d.Wake = true, WakeClick
				d.Sentence = c.Error()
			}
			d.Logs = append(d.Logs, Log{SinkRelease, fmt.Sprintf("order=%d disposition=%q - curtain held the release: %v",
				legs[i].Leg.OrderID, legs[i].Leg.Mode, c)})
		}
	}
	return nil
}

// covered is the act's scope (SHAPE §3.5). An act that names its purpose
// covers a leg at, or heading to, a wait carrying it. An act that does not
// (today's single button) takes its purpose from the legs at a wait or not yet
// released past one, and covers only those: a leg released past a wait and
// driving to a later one is covered only by an act naming that wait's purpose
// — never by a second press of the first (N-a).
func covered(a Act, purpose Purpose, explicit bool, l ActLeg) bool {
	if protocol.IsTerminal(l.Leg.Status) {
		return false
	}
	if a.Reevaluation {
		in := l.Leg.Intent
		return in != nil && l.Point.Ordinal == in.StationWait
	}
	if l.Point.NoWaits {
		// A plan with no station wait: the station's own doors cover it as they
		// always did; a changeover's no-wait leg (a relay's stage leg, a
		// per-position fan-out) has nothing a release lets go.
		return a.Origin == OriginStationOrder || a.Origin == OriginStationPair
	}
	if !l.Point.HasWait {
		// Released past its last station wait: nothing an act can release,
		// except the per-order click aimed at it (Core no-ops a release past a
		// leg's last wait).
		return a.Origin == OriginStationOrder
	}
	if l.Point.Purpose == "" {
		// A wait written before purposes (an order in flight at the upgrade):
		// nothing to scope it by, so the act covers it as the doors always did.
		return explicit || nominates(l)
	}
	if !explicit && !nominates(l) {
		return false
	}
	return purpose != "" && l.Point.Purpose == purpose
}

// nominates reports whether a leg can give a purposeless act its purpose: it
// is at a station wait, or has not yet been released past one.
func nominates(l ActLeg) bool {
	return l.Point.AtWait || (l.Point.Ordinal == 0 && l.Point.HasWait)
}

// inferredPurpose is the earliest decision, in the cell's order, among the
// waits the act's legs stand at or (never released) are heading to.
func inferredPurpose(legs []ActLeg) Purpose {
	present := map[Purpose]bool{}
	for _, l := range legs {
		if l.Leg.ReadErr == nil && !protocol.IsTerminal(l.Leg.Status) && l.Point.HasWait && nominates(l) && l.Point.Purpose != "" {
			present[l.Point.Purpose] = true
		}
	}
	for _, p := range purposeOrder {
		if present[p] {
			return p
		}
	}
	return ""
}

func anyGoOrHold(ds []Decision) bool {
	for _, d := range ds {
		if d.Verdict == Go {
			return true
		}
	}
	return false
}

// curtainHoldSentence is Q8's: the press is remembered and the robot goes
// when the curtain clears.
func curtainHoldSentence(err error) string {
	if held, ok := err.(*CurtainHeldError); ok && held.Node != "" &&
		strings.HasPrefix(held.Sentence, "Release the light curtain") {
		return fmt.Sprintf("Release the light curtain at %s: the robot goes when it clears.", held.Node)
	}
	return err.Error()
}

// liftDependencies is G7 (SHAPE §3.4): a leg that would go, whose next
// segment sets a bin down on a node its sibling must lift from first, goes
// only when the lifter goes in this act and Core says they co-release. A
// held lifter holds its placer, and a placer's co-release partner that is not
// going holds too, so the rule runs to a fixed point.
func liftDependencies(f ActFacts, legs []ActLeg, ds []Decision) {
	byOrder := map[int64]int{}
	for i, l := range legs {
		byOrder[l.Leg.OrderID] = i
	}
	for changed := true; changed; {
		changed = false
		for i := range ds {
			d := &ds[i]
			if d.Verdict != Go || f.PointErr != nil {
				continue
			}
			pt := f.Points[d.OrderID]
			for _, dep := range pt.AwaitsLift {
				j, inAct := byOrder[dep.Lifter]
				if inAct && ds[j].Verdict == Go && dep.CoRelease {
					continue
				}
				d.Verdict, d.Gate, d.Wake = Hold, G7, WakeLift
				d.Sentence = fmt.Sprintf("waiting for the bin on %s to be lifted", dep.Node)
				changed = true
				break
			}
		}
	}
}

// waitsOn reports whether placer's point waits on lifter.
func waitsOn(f ActFacts, placer, lifter int64) bool {
	for _, dep := range f.Points[placer].AwaitsLift {
		if dep.Lifter == lifter {
			return true
		}
	}
	return false
}

// commitOrder puts lifters first: a leg another going leg waits on goes
// before it. A two-way dependency (each lifts what the other sets down, the
// unflipped press-index pair) keeps the act's own order, the step-derived
// evac first.
func commitOrder(f ActFacts, ds []Decision) []Decision {
	out := make([]Decision, 0, len(ds))
	placed := map[int]bool{}
	var visit func(i int)
	visit = func(i int) {
		if placed[i] {
			return
		}
		placed[i] = true
		for _, dep := range f.Points[ds[i].OrderID].AwaitsLift {
			for j := range ds {
				if ds[j].OrderID == dep.Lifter && !placed[j] && !waitsOn(f, dep.Lifter, ds[i].OrderID) {
					visit(j)
				}
			}
		}
		out = append(out, ds[i])
	}
	for i := range ds {
		visit(i)
	}
	return out
}
