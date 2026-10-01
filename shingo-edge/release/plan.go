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
// status: staged, or in_transit (TestReleasable pins it against Core).
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
//	(no process node: a plain release)
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

// PlanPair decides a pair click's door: the route (a changeover node goes to
// the changeover act), G4 the node and its claim and mode, G4 the pair, G4
// the departing-bin check. Its two legs are then the act's (PlanAct), which
// decides G1, G3, G6 and G7 for both together.
func PlanPair(p Pair) PairPlan {
	if !p.Loaded.Has(NeedRoute) {
		return PairPlan{Need: NeedRoute}
	}
	switch {
	case p.TaskEvac != p.TaskSupply:
		// One leg on one robot: not a swap pair (N1-d).
		return PairPlan{Route: true}
	case p.TaskEvac && p.TaskSupply:
		// Both legs: a two-robot swap stays on the pair path, which releases
		// both at the click. Any other mode, or a node with no claim, is the
		// changeover act's (§6.4): the pair path would refuse it.
		if !p.Loaded.Has(NeedActive) {
			return PairPlan{Need: NeedActive}
		}
		if p.LoadErr == nil && (!p.ClaimResolved || !p.Mode.IsTwoRobot()) {
			return PairPlan{Route: true}
		}
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

func pairRefusal(gate string, err error) PairPlan {
	return PairPlan{Verdict: Refuse, Gate: gate, Refusal: err}
}

func idStr(id *int64) string {
	if id == nil {
		return "nil"
	}
	return strconv.FormatInt(*id, 10)
}

// ── Doors 5 and 6: the changeover ─────────────────────────────────────────

// Slot is one leg of a changeover task the act covers.
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
	Logs      []Log
}

// ChangeoverPlan is the decision for a changeover release.
type ChangeoverPlan struct {
	Refusal error
	Tasks   []TaskPlan
}

// PlanChangeover decides which tasks a changeover release covers. A task that
// is `unchanged`, or not the clicked node, is out of scope. A sweep declines a
// position the line pulls from (G5), or whose pull state cannot be read, and
// names it; a click aimed at the node leaves it to the trunk, which moves the
// line. Each covered task's legs are then the act's (PlanAct): the evac goes
// at the click, and the paired supply goes with it when they co-release or
// holds until the evac's lift (G7), at the wait the act's purpose names.
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
		if t.Evac != nil {
			tp.Slots = append(tp.Slots, Slot{Order: *t.Evac, Kind: "evac"})
		}
		if t.Supply != nil {
			tp.Slots = append(tp.Slots, Slot{Order: *t.Supply, Kind: "supply"})
		}
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
