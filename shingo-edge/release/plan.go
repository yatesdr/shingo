package release

import (
	"fmt"
	"strconv"

	"shingo/protocol"
)

// Verdict is a gate's answer for one leg or one door.
type Verdict int

const (
	Go     Verdict = iota // let it go
	Skip                  // not on this act, and not an error: pending, deferred or terminal
	Refuse                // the act is refused; nothing has changed
)

// SkipReason says why a leg was passed over.
type SkipReason int

const (
	SkipNone          SkipReason = iota
	SkipTerminal                 // already ran, cancelled or failed: nothing owed
	SkipNotReleasable            // not at its wait yet: pending, or deferred by the door
)

// Arm is the shape of the release a leg gets once every gate has passed.
type Arm int

const (
	ArmPlain    Arm = iota // no process node: a plain release, no paperwork
	ArmDrop                // a changeover drop's evac: the disposition passes straight through
	ArmNoClaim             // no claim to reset against: a plain release, after the flip
	ArmProduce             // a produce node: no count on the envelope (the ingest is the manifest)
	ArmLineside            // the lineside release: capture, count, task, flush, envelope
)

// ModeCaptureLineside is the disposition mode an operator's RELEASE EMPTY or
// pulled-parts release carries (uop.DispositionCaptureLineside).
const ModeCaptureLineside = "capture_lineside"

// Releasable reports whether Core will accept a release for an order in this
// status: staged, or in_transit (orders.ReleasableAtCore).
func Releasable(s protocol.Status) bool {
	return s == protocol.StatusStaged || s == protocol.StatusInTransit
}

// TerminalSuccess reports whether a terminal order ran its half
// (orders.IsTerminalSuccess).
func TerminalSuccess(s protocol.Status) bool { return s == protocol.StatusConfirmed }

// ── The trunk ─────────────────────────────────────────────────────────────

// LegPlan is the decision for one leg.
type LegPlan struct {
	Need    Need // non-zero: load this group and plan again
	Verdict Verdict
	Gate    string
	Refusal error
	Skip    SkipReason
	// Go: the release's shape and its side effects, in commit order: the
	// flip, the produce finalize, the release.
	Arm              Arm
	Flip             bool
	Finalize         bool
	SuppressManifest bool // the supply leg of a two-robot swap: its bin's manifest is left alone
	U1               bool // a produce bin declared full: the unloader side-cycle
	Logs             []Log
}

// PlanLeg decides one leg's release. The gates run in this order, and each
// reads only the facts it needs (Need):
//
//	G4 the order reads               G1 Core will release it
//	G6 the curtain                   (no process node: a plain release)
//	G4 the node reads                G5 the pull state reads
//	G4 the runtime reads             (the arm: drop, no claim, produce, lineside)
//	G4 the supply classification     G4 the departing-bin check
//	G5 the flip
func PlanLeg(a Act, l Leg) LegPlan {
	if !l.Loaded.Has(NeedOrder) {
		return LegPlan{Need: NeedOrder}
	}
	if l.ReadErr != nil {
		return legReadRefusal(a, l)
	}
	if !Releasable(l.Status) {
		return legNotReleasable(a, l)
	}
	if !l.Loaded.Has(NeedCurtain) {
		return LegPlan{Need: NeedCurtain}
	}
	if l.Curtain != nil {
		return legRefusal(a, l, G6, l.Curtain, Log{SinkRelease,
			fmt.Sprintf("order=%d disposition=%q - curtain refused the release: %v", l.OrderID, l.Mode, l.Curtain)})
	}
	if !l.HasProcessNode {
		return LegPlan{Verdict: Go, Arm: ArmPlain}
	}
	if !l.Loaded.Has(NeedNode) {
		return LegPlan{Need: NeedNode}
	}
	if l.NodeErr != nil {
		return legRefusal(a, l, G4, fmt.Errorf("get process node %d: %w", l.ProcessNodeID, l.NodeErr))
	}
	if !l.Loaded.Has(NeedPull) {
		return LegPlan{Need: NeedPull}
	}
	if l.PullErr != nil {
		return legRefusal(a, l, G5,
			fmt.Errorf("node %s: could not read whether the line is pulling from it (%w)", l.PullNode, l.PullErr),
			Log{SinkStd, fmt.Sprintf("release order %d at node %s: %v — declining rather than releasing on an "+
				"unread pull state", l.OrderID, l.PullNode, l.PullErr)})
	}
	if !l.Loaded.Has(NeedRuntime) {
		return LegPlan{Need: NeedRuntime}
	}
	if l.RuntimeErr != nil {
		return legRefusal(a, l, G4, fmt.Errorf("ensure runtime for node %d: %w", l.ProcessNodeID, l.RuntimeErr))
	}
	if !l.Loaded.Has(NeedKind) {
		return LegPlan{Need: NeedKind}
	}
	switch {
	case l.Drop:
		if p, done := departsGate(a, l); done {
			return p
		}
		return withFlip(a, l, LegPlan{Verdict: Go, Arm: ArmDrop, Finalize: l.Departs})
	case !l.ClaimResolved:
		// No claim to reset against: the disposition is dropped, and said so
		// before anything else is decided (the ALN_002 breadcrumb).
		p := withFlip(a, l, LegPlan{Verdict: Go, Arm: ArmNoClaim})
		p.Logs = append([]Log{{SinkRelease, fmt.Sprintf("order %d on node %s — toClaim is nil (runtime.ActiveClaimID=%s), skipping manifest sync; disposition %q dropped",
			l.OrderID, l.NodeName, l.ActiveClaim, l.Mode)}}, p.Logs...)
		return p
	}
	if !l.Loaded.Has(NeedSupply) {
		return LegPlan{Need: NeedSupply}
	}
	if l.SupplyErr != nil {
		return legRefusal(a, l, G4, l.SupplyErr, Log{SinkRelease,
			fmt.Sprintf("order=%d node=%s disposition=%q — refusing release: %v", l.OrderID, l.NodeName, l.Mode, l.SupplyErr)})
	}
	if p, done := departsGate(a, l); done {
		return p
	}
	arm := ArmLineside
	if l.Produce {
		arm = ArmProduce
	}
	return withFlip(a, l, LegPlan{
		Verdict: Go, Arm: arm, Finalize: l.Departs, SuppressManifest: l.IsSupply,
		U1: arm == ArmProduce && !l.IsSupply && l.Mode == ModeCaptureLineside,
	})
}

// departsGate is G4's departing-bin check: whether releasing the leg takes a
// produce bin off the node could not be answered, and the release refuses
// rather than ship or skip a bin's count on a guess.
func departsGate(a Act, l Leg) (LegPlan, bool) {
	if !l.Loaded.Has(NeedDeparts) {
		return LegPlan{Need: NeedDeparts}, true
	}
	if l.DepartErr != nil {
		return legRefusal(a, l, G4, l.DepartErr, Log{SinkRelease,
			fmt.Sprintf("order=%d node=%s — refusing release: %v", l.OrderID, l.NodeName, l.DepartErr)}), true
	}
	return LegPlan{}, false
}

// withFlip is G5's second half: the release on a sequential position the line
// feeds from moves the line to the partner first, and refuses only when the
// partner cannot take it.
func withFlip(a Act, l Leg, p LegPlan) LegPlan {
	if !l.Loaded.Has(NeedFlip) {
		return LegPlan{Need: NeedFlip}
	}
	f := l.Flip
	if !f.Applies {
		return p
	}
	if f.RuntimeErr != nil {
		return legRefusal(a, l, G5,
			fmt.Errorf("node %s: could not read whether the line is pulling from it (%w)", f.Node, unwrapNil(f.RuntimeErr)),
			Log{SinkStd, fmt.Sprintf("release at node %s: runtime unreadable (%v) — declining rather than releasing on an "+
				"unread pull state", f.Node, unwrapNil(f.RuntimeErr))})
	}
	if !f.OwnPull {
		return p
	}
	if f.PartnerErr != nil {
		return legRefusal(a, l, G5, fmt.Errorf("the line is pulling from %s and its partner could not be resolved: %w",
			f.Node, f.PartnerErr))
	}
	if f.NotReady != "" && f.Blocked {
		return legRefusal(a, l, G5, fmt.Errorf("cannot release %s yet: the line's parts would land on the carrier still on %s: %s",
			f.Node, f.Partner, f.NotReady))
	}
	p.Flip = true
	return p
}

// ErrRuntimeMissing is the loader's mark for a runtime row that read as
// absent with no error: the refusal prints the nil error the read returned,
// as it always has.
var ErrRuntimeMissing error = runtimeMissing{}

type runtimeMissing struct{}

func (runtimeMissing) Error() string { return "runtime row missing" }

func unwrapNil(err error) error {
	if err == ErrRuntimeMissing {
		return nil
	}
	return err
}

// legReadRefusal is G4 for an order that could not be read, in the door's words.
func legReadRefusal(a Act, l Leg) LegPlan {
	switch a.Origin {
	case OriginStationOrder:
		return LegPlan{Verdict: Refuse, Gate: G4, Refusal: fmt.Errorf("get order %d: %w", l.OrderID, l.ReadErr)}
	case OriginChangeoverSweep, OriginChangeoverNode:
		return LegPlan{Verdict: Refuse, Gate: G4,
			Refusal: fmt.Errorf("node %s (%s): get order: %w", l.TaskNode, l.Label, l.ReadErr),
			Logs:    []Log{{SinkStd, fmt.Sprintf("release changeover wait node %s (%s): get order: %v", l.TaskNode, l.Label, l.ReadErr)}}}
	default:
		return LegPlan{Verdict: Refuse, Gate: G4, Refusal: fmt.Errorf("get order %s (%d): %w", l.Label, l.OrderID, l.ReadErr)}
	}
}

// legNotReleasable is G1: the per-order click refuses with Core's reason;
// every other door passes the leg over.
func legNotReleasable(a Act, l Leg) LegPlan {
	terminal := protocol.IsTerminal(l.Status)
	if !a.passesOverUnreleasable() {
		return LegPlan{Verdict: Refuse, Gate: G1, Refusal: fmt.Errorf("order %d is %s, which Core will not release%s",
			l.OrderID, l.Status, QueueReasonSuffix(l.QueueReason, l.QueueCode))}
	}
	p := LegPlan{Verdict: Skip, Gate: G1, Skip: SkipNotReleasable}
	if terminal {
		p.Skip = SkipTerminal
	}
	switch a.Origin {
	case OriginChangeoverSweep, OriginChangeoverNode:
		if !terminal {
			p.Logs = []Log{{SinkStd, fmt.Sprintf("release changeover wait node %s (%s): order %d status=%q not releasable at Core — counting pending",
				l.TaskNode, l.Label, l.OrderID, l.Status)}}
		}
	default:
		if terminal {
			p.Logs = []Log{{SinkEngine, fmt.Sprintf("deferred release: order %s (%d) status=%q is terminal — skipping", l.Label, l.OrderID, l.Status)}}
		} else {
			p.Logs = []Log{{SinkEngine, fmt.Sprintf("deferred release: order %s (%d) status=%q is not releasable at Core (needs staged or in_transit) — skipping, will release when it stages",
				l.Label, l.OrderID, l.Status)}}
		}
	}
	return p
}

// legRefusal refuses the leg with the trunk's error, in the door's words.
func legRefusal(a Act, l Leg, gate string, err error, logs ...Log) LegPlan {
	p := LegPlan{Verdict: Refuse, Gate: gate, Refusal: err, Logs: logs}
	switch a.Origin {
	case OriginStationOrder:
	case OriginChangeoverSweep, OriginChangeoverNode:
		p.Refusal = fmt.Errorf("node %s (%s): %w", l.TaskNode, l.Label, err)
		p.Logs = append(p.Logs, Log{SinkStd, fmt.Sprintf("release changeover wait node %s (%s): %v", l.TaskNode, l.Label, err)})
	default:
		p.Refusal = fmt.Errorf("release order %s (%d): %w", l.Label, l.OrderID, err)
	}
	return p
}

// ── Door 1: the pair click ────────────────────────────────────────────────

// PairPlan is the decision for a RELEASE on a paired node.
type PairPlan struct {
	Need Need
	// Route: the changeover act takes this node's click (doors 5 and 6).
	Route   bool
	Verdict Verdict
	Gate    string
	Refusal error
	// Go: finalize the departing bin first, then release the evac with the
	// operator's disposition and the supply with none, each through the trunk.
	Finalize     bool
	Evac, Supply *int64
	Logs         []Log
}

// PlanPair decides a pair click. Order: the route (a single-leg changeover
// node goes to the changeover act), G4 the node and its claim and mode, G4
// the pair, G7 the press-index collision, G6 the curtain, G4 the
// departing-bin check.
func PlanPair(p Pair) PairPlan {
	if !p.Loaded.Has(NeedRoute) {
		return PairPlan{Need: NeedRoute}
	}
	if p.TaskEvac != p.TaskSupply {
		// One leg on one robot: not a swap pair (N1-d).
		return PairPlan{Route: true}
	}
	if !p.Loaded.Has(NeedActive) {
		return PairPlan{Need: NeedActive}
	}
	if p.LoadErr != nil {
		return pairRefusal(G4, fmt.Errorf("get runtime for node %d: %w", p.NodeID, p.LoadErr))
	}
	if !p.ClaimResolved {
		return pairRefusal(G4, fmt.Errorf("node %s: no active claim for release", p.NodeName))
	}
	if !p.Mode.IsTwoRobot() {
		return pairRefusal(G4, fmt.Errorf("node %s: release-staged requires a two-robot swap mode, got %q", p.NodeName, p.Mode))
	}
	if !p.Loaded.Has(NeedPair) {
		return PairPlan{Need: NeedPair}
	}
	if p.ResolveErr != nil {
		out := pairRefusal(G4, fmt.Errorf("node %s: %w", p.NodeName, p.ResolveErr))
		out.Logs = []Log{{SinkEngine, fmt.Sprintf("release-staged REFUSED node=%s: %v (runtime staged=%s active=%s, task=%t) — operator clicked and got nothing",
			p.NodeName, p.ResolveErr, idStr(p.StagedPtr), idStr(p.ActivePtr), p.HasTask)}}
		return out
	}
	var logs []Log
	if p.Relabelled {
		logs = append(logs, Log{SinkEngine, fmt.Sprintf("release-staged node=%s: steps say the pair is inverted relative to the runtime slots - evac=%d supply=%d (slots said evac=%d supply=%d)",
			p.NodeName, *p.Evac, *p.Supply, p.SlotEvac, p.SlotSupply)})
	}
	logs = append(logs, Log{SinkEngine, fmt.Sprintf("release-staged node=%s resolved evac=%s supply=%s",
		p.NodeName, idStr(p.Evac), idStr(p.Supply))})
	if p.Mode == protocol.SwapModeTwoRobotPressIndex && p.Evac != nil && p.Supply != nil {
		if !p.Loaded.Has(NeedCollision) {
			return PairPlan{Need: NeedCollision}
		}
		if out, held := PlanCollision(p); held {
			out.Logs = append(logs, out.Logs...)
			return out
		}
	}
	if !p.Loaded.Has(NeedCurtain) {
		return PairPlan{Need: NeedCurtain}
	}
	if p.Curtain != nil {
		out := pairRefusal(G6, p.Curtain)
		out.Logs = logs
		return out
	}
	if !p.Loaded.Has(NeedDeparts) {
		return PairPlan{Need: NeedDeparts}
	}
	if p.DepartErr != nil {
		out := pairRefusal(G4, fmt.Errorf("node %s: %w", p.NodeName, p.DepartErr))
		out.Logs = logs
		return out
	}
	if p.Departs && p.DepartingReadErr != nil {
		out := pairRefusal(G4, fmt.Errorf("node %s: read departing order %d: %w", p.NodeName, *p.Evac, p.DepartingReadErr))
		out.Logs = logs
		return out
	}
	return PairPlan{Verdict: Go, Finalize: p.Departs, Evac: p.Evac, Supply: p.Supply, Logs: logs}
}

// PlanCollision is G7 at a press-index pair: never place onto a position the
// sibling has not cleared. A terminal sibling is not pending, and neither is
// one staged (both go on this click) or one this station already released.
// The supply is the placing leg by classification; the evac places only when
// its steps say so (unflipped, at the back position). An unreadable leg holds.
func PlanCollision(p Pair) (PairPlan, bool) {
	held := func(state string, line string) (PairPlan, bool) {
		return PairPlan{Verdict: Refuse, Gate: G7,
			Refusal: &SwapPairNotReadyError{NodeName: p.NodeName, SiblingState: state},
			Logs:    []Log{{SinkEngine, line}}}, true
	}
	for _, arm := range p.Arms {
		if arm.LegErr != nil {
			return held("unreadable", fmt.Sprintf("release-staged HELD node=%s: cannot read leg %d to check for a collision: %v",
				p.NodeName, arm.Leg, arm.LegErr))
		}
		if !Releasable(arm.LegStatus) {
			continue
		}
		if arm.LegStatus == protocol.StatusInTransit && arm.LegPassed {
			continue
		}
		if arm.SiblingErr != nil {
			return held("unreadable", fmt.Sprintf("release-staged HELD node=%s: cannot read sibling %d to check for a collision: %v",
				p.NodeName, arm.Sibling, arm.SiblingErr))
		}
		if protocol.IsTerminal(arm.SiblingState) || arm.SiblingState == protocol.StatusStaged {
			continue
		}
		if arm.SiblingState == protocol.StatusInTransit {
			if arm.SiblingPassedErr != nil {
				return held("unreadable", fmt.Sprintf("release-staged HELD node=%s: cannot read sibling %d's release history: %v",
					p.NodeName, arm.Sibling, arm.SiblingPassedErr))
			}
			if arm.SiblingPassed {
				continue
			}
		}
		if arm.PlacesAt == "" {
			continue // sets nothing down on the press — the flipped R1
		}
		return held(string(arm.SiblingState), fmt.Sprintf("release-staged HELD node=%s: leg %d is staged and would place a bin at %s, "+
			"but leg %d is %q and has not cleared it", p.NodeName, arm.Leg, arm.PlacesAt, arm.Sibling, arm.SiblingState))
	}
	return PairPlan{}, false
}

func pairRefusal(gate string, err error) PairPlan {
	return PairPlan{Verdict: Refuse, Gate: gate, Refusal: err}
}

func idStr(id *int64) string {
	if id == nil {
		return "nil"
	}
	return strconv.FormatInt(*id, 10)
}

// ── Door 1's deferral (fired by door 8) ───────────────────────────────────

// PlanDeferral decides whether a pair leg that did not go on the click is
// remembered, to be released when it reaches its wait (G7, today's deferral):
// only when its sibling went, by either spelling (released on this click, or
// already finished its half), and only for a leg that is neither terminal
// (nothing to fire) nor releasable (released on this click already).
func PlanDeferral(d Deferral) (remember bool, logs []Log) {
	if d.Released || !(d.SiblingReleased || d.SiblingSucceeded) || d.LegErr != nil {
		return false, nil
	}
	if protocol.IsTerminal(d.LegStatus) || Releasable(d.LegStatus) {
		return false, nil
	}
	return true, []Log{{SinkEngine, fmt.Sprintf("two-robot release: leg %d (%s) deferred — sibling already released; will re-fire when it reaches staged",
		d.Leg, d.LegStatus)}}
}

// ── Doors 5 and 6: the changeover ─────────────────────────────────────────

// Slot is one leg a changeover task releases on this act.
type Slot struct {
	Order int64
	Kind  string // "evac" | "supply"
}

// TaskPlan is the decision for one changeover task.
type TaskPlan struct {
	Task      int
	InScope   bool
	NeedsFlip bool // declined: the line is pulling from it (a sweep only)
	Slots     []Slot
	Deferred  bool // a paired supply deferred to its evac's lift (G7)
	Logs      []Log
}

// ChangeoverPlan is the decision for a changeover release.
type ChangeoverPlan struct {
	Refusal error
	Tasks   []TaskPlan
}

// PlanChangeover decides which legs of which tasks a changeover release lets
// go. A task that is `unchanged`, or not the clicked node, is out of scope. A
// sweep declines a position the line pulls from (G5), or whose pull state
// cannot be read, and names it; a click aimed at the node leaves it to the
// trunk, which moves the line. The evac goes at the click; a paired supply is
// deferred to the evac's lift (G7) unless that deferral was already served
// (it is staged again at a later wait, the tooling hold).
func PlanChangeover(c Changeover) ChangeoverPlan {
	if c.ReadErr != nil {
		return ChangeoverPlan{Refusal: c.ReadErr}
	}
	out := ChangeoverPlan{Tasks: make([]TaskPlan, 0, len(c.Tasks))}
	for i, t := range c.Tasks {
		tp := TaskPlan{Task: i, InScope: t.InScope}
		if !t.InScope {
			out.Tasks = append(out.Tasks, tp)
			continue
		}
		if c.Sweep && (t.PullErr != nil || t.Pulling) {
			tp.NeedsFlip = true
			if t.PullErr != nil {
				tp.Logs = []Log{{SinkStd, fmt.Sprintf("release changeover wait node %s: %v — the sweep declines rather than releasing on an "+
					"unread pull state", t.NodeName, t.PullErr)}}
			} else {
				tp.Logs = []Log{{SinkStd, fmt.Sprintf("release changeover wait: node %s skipped — the line is pulling from it, and a plant-wide "+
					"sweep does not move the line on the operator's behalf", t.PullNode)}}
			}
			out.Tasks = append(out.Tasks, tp)
			continue
		}
		paired := t.Evac != nil && t.Supply != nil
		if t.Evac != nil {
			tp.Slots = append(tp.Slots, Slot{Order: *t.Evac, Kind: "evac"})
		}
		if t.Supply != nil && (!paired || t.SupplyAtLaterWait) {
			tp.Slots = append(tp.Slots, Slot{Order: *t.Supply, Kind: "supply"})
		}
		tp.Deferred = paired && !t.SupplyAtLaterWait && t.SupplyLive
		out.Tasks = append(out.Tasks, tp)
	}
	return out
}

// ── Doors 3 and 4: the Material page ──────────────────────────────────────

// MaterialPlan is the decision for a Material-page release of a node's bin.
type MaterialPlan struct {
	Need    Need
	Verdict Verdict
	Gate    string
	Refusal error
}

// PlanMaterial decides a Material-page release: G4 the node, the count, its
// claim and its outbound destination; G6 the curtain at the node the bin
// leaves; G8 one robot per bin. The door is not line-pull guarded (owner,
// 2026-08-28: an admin release).
func PlanMaterial(m Material) MaterialPlan {
	if !m.Loaded.Has(NeedActive) {
		return MaterialPlan{Need: NeedActive}
	}
	refuse := func(gate string, err error) MaterialPlan {
		return MaterialPlan{Verdict: Refuse, Gate: gate, Refusal: err}
	}
	switch {
	case m.LoadErr != nil:
		return refuse(G4, m.LoadErr)
	case m.Qty < 1:
		return refuse(G4, fmt.Errorf("qty must be at least 1"))
	case !m.ClaimResolved:
		return refuse(G4, fmt.Errorf("node %s has no active claim for release", m.NodeName))
	case m.OutboundDest == "":
		return refuse(G4, fmt.Errorf("node %s has no outbound destination configured", m.NodeName))
	}
	if !m.Loaded.Has(NeedCurtain) {
		return MaterialPlan{Need: NeedCurtain}
	}
	if m.Curtain != nil {
		return refuse(G6, m.Curtain)
	}
	if !m.Loaded.Has(NeedInFlight) {
		return MaterialPlan{Need: NeedInFlight}
	}
	if m.InFlightErr != nil {
		return refuse(G8, fmt.Errorf("node %s: could not read the order already working this bin (%w)", m.NodeName, m.InFlightErr))
	}
	if m.InFlight {
		return refuse(G8, fmt.Errorf("node %s: a release for this bin is already on its way (order %d, %s)",
			m.NodeName, m.InFlightOrder, m.InFlightStatus))
	}
	return MaterialPlan{Verdict: Go}
}

// ── Door 9: the swap survivor ─────────────────────────────────────────────

// SurvivorPlan is the decision for a leg whose partner may have finished.
type SurvivorPlan struct {
	Need Need
	// Release: release the leg through the trunk with the supply's (zero)
	// disposition. False with no logs: not a survivor at all.
	Release bool
	Gate    string
	Logs    []Log
}

// PlanSurvivor decides the durable half of the pair deferral: a leg whose
// partner finished successfully is owed the operator's click, once per Edge
// lifetime. A relay leg's wait and a changeover leg's waits are not the
// survivor rule's to release (G2): a station wait is the station's.
func PlanSurvivor(s Survivor) SurvivorPlan {
	if !s.Loaded.Has(NeedSiblings) {
		return SurvivorPlan{Need: NeedSiblings}
	}
	if !s.Paired || !s.SiblingSucceeded {
		return SurvivorPlan{}
	}
	if !s.Loaded.Has(NeedScope) {
		return SurvivorPlan{Need: NeedScope}
	}
	if s.Relay {
		return SurvivorPlan{Gate: G2, Logs: []Log{{SinkEngine, fmt.Sprintf("swap-survivor release: order %d is a relay leg — its partner %d finishing is not a release of its wait; left for the operator",
			s.OrderID, s.SiblingID)}}}
	}
	if s.InChangeover {
		return SurvivorPlan{Gate: G2, Logs: []Log{{SinkEngine, fmt.Sprintf("swap-survivor release: order %d is a leg of changeover task %d (%s) — its waits are the changeover's to release; left for the operator",
			s.OrderID, s.TaskID, s.TaskSituation)}}}
	}
	if s.AlreadyFired {
		return SurvivorPlan{}
	}
	return SurvivorPlan{Release: true}
}

// ── Door 10: the evac's lift releases its deferred supply ─────────────────

// PlanPickup decides whether an evac's lift releases the changeover supply the
// sweep deferred to it (G7): only while this station has never released the
// supply past a wait. A supply already released at "ready" by the pair click
// is on its way to a later hold, and releasing it again would carry it past
// that hold (N-a'); an unreadable history holds too.
func PlanPickup(supply int64, evacUUID string, passed bool, passedErr error) (release bool, logs []Log) {
	if passedErr != nil || passed {
		return false, []Log{{SinkEngine, fmt.Sprintf("bin_picked_up: supply %d not released at evac %s's pickup — already released past a wait (passed=%v err=%v)",
			supply, evacUUID, passed, passedErr)}}
	}
	return true, nil
}

// CommitFailure renders an error the commit hit after every gate passed (an
// I/O failure: a flush, a write, the outbox) in the door's words, as the
// trunk's own errors always were.
func CommitFailure(a Act, l Leg, err error) LegPlan {
	return legRefusal(a, l, "", err)
}

// PlanFlip is G5's flip on its own, for a release already let go: whether
// the line moves to the partner, or the refusal when the partner cannot take
// it.
func PlanFlip(f Flip) LegPlan {
	return withFlip(Act{Origin: OriginStationOrder}, Leg{Loaded: NeedFlip, Flip: f}, LegPlan{Verdict: Go})
}
