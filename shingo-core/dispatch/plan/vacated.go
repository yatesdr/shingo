package plan

import (
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// vacated.go — A DROP MAY LAND ON THE SLOT A COMMITTED LIFT VACATES.
//
// ── WHAT WAS BROKEN ─────────────────────────────────────────────────────────
//
// A complex leg's drop was refused wherever the only thing between it and a free
// slot was a lift already committed to happen first: its own earlier pickup, or
// its partner's. SPR 2026-09-28, pairs 7060/7061 and 7062/7063: the supply was
// widened FIFO onto a buffer partial, the return found its home holding a bin
// nobody lifts and every buffer full, and the pair parked on loader-park-no-slot
// every pass until an operator cancelled it. The buffer the supply was about to
// empty was the answer, and nothing could say so.
//
// ── THE RULE ────────────────────────────────────────────────────────────────
//
// A drop at step i of leg L onto node N is clear when N's only bin is taken
// before L gets there by
//
//	(a) L's own pickup at N strictly before step i (BinsAtStep, carried logic
//	    included), or
//	(b) L's partner's pickup at N, when the partner ran EARLIER IN THIS PASS,
//	    the pickup is CommittedAtDispatch, it lifts a bin the partner claimed
//	    this pass, and L's drop is HeldForRelease,
//
// and nothing else is in flight to N, by the asker's own in-flight predicate.
// Never a lane slot: lane holds are per order, and two orders on one mouth is
// the order 22/23 deadlock.
//
// CONSULTED ONLY WHERE TODAY'S ANSWER IS WAIT. Every asker keeps its answer
// and asks this only in the branch that would have parked, so a free slot
// always wins and every ordinary drop is byte-unchanged.
//
// MUTUAL GRANTS ARE IMPOSSIBLE BY CONSTRUCTION. Only the leg that runs second
// in stage 1 is handed partner facts (Need.Pair), so two legs can never each
// be cleared by the other's lift.
//
// ── WHERE THE FACTS COME FROM ────────────────────────────────────────────────
//
// The partner's leg (Need.Pair) is the pass's own output for the pair — what
// the leg that ran first resolved and claimed THIS pass — handed over by the
// dispatcher where it forms the pair, never read back. A partner committed or
// faulted on an earlier pass is not in this pass, and its lift is not a promise
// anyone here made. What the books say (who holds the bin standing there) is
// read, through the Reader: BinsAtStep, the first read of the slot book.
//
// The planner decides and writes nothing. The dispatcher stamps the persisted
// plan, logs the grant, asks its own in-flight question, and keeps the release
// fence.
//
// ── THE STAMP, AND WHY IT VALIDATES ITSELF ──────────────────────────────────
//
// A (b) grant is a promise about ORDER: the partner lifts before L drops. The
// fleet does not keep it — release gates on L being staged, not on the partner
// having lifted — so the release fence in HandleOrderRelease keeps it, and the
// stamp is what tells the fence which drops need keeping. Only a drop the rule
// admitted is stamped; nothing today's code already dispatches ever is.
//
// It names its node, and is honoured only while the step still drops there. Five
// writers can re-point a drop, one of them at release; each clears the stamp as
// housekeeping, but the self-check is the guarantee, because the next writer
// someone adds can forget to.

// Vacate records what the vacated-slot rule counted when it admitted a drop.
// Partner is 0 for an own-lift (a) grant, which needs no release fence: one
// robot lifts before it drops.
type Vacate struct {
	Node    string `json:"node"`
	Bin     int64  `json:"bin"`
	Partner int64  `json:"partner,omitempty"`
}

// PairLeg is the partner's leg as the pass that formed the pair has it: the
// partner order, its THIS-PASS resolved steps, and the pickups it claimed.
type PairLeg struct {
	Partner int64
	Steps   []Step
	Lifts   []Lift
}

// Lift is one pickup the partner claimed this pass: the bin, where it stands,
// and the step of the partner's plan that lifts it.
type Lift struct {
	Bin  int64
	Node string
	Step int
}

// StampHonoured returns the step's stamp when it still speaks for the node the
// step drops at, else nil. Every reader goes through this.
func StampHonoured(s Step) *Vacate {
	if s.Action != protocol.ActionDropoff || s.Vacate == nil || s.Vacate.Node == "" || s.Vacate.Node != s.Node {
		return nil
	}
	return s.Vacate
}

// PartnerStamp is StampHonoured narrowed to a (b) grant — the only kind the
// release fence and the slot claim's partner arm read.
func PartnerStamp(s Step) *Vacate {
	if st := StampHonoured(s); st != nil && st.Partner != 0 {
		return st
	}
	return nil
}

// ── THE TWO BOUNDARY PREDICATES ─────────────────────────────────────────────
//
// Deliberately not widen's predicate. Widen asks which pickups should see today's
// pools; these ask what is physically committed.

// SplitAtWait is the prefix of a plan the fleet create sends: everything before
// the plan's first wait, and the wait itself when it names a node (it becomes a
// Wait block). hasWait reports whether there is one.
func SplitAtWait(steps []Step) (preWait []Step, hasWait bool) {
	for i, s := range steps {
		if s.Action == protocol.ActionWait {
			if s.Node != "" {
				// Wait-with-node: include it (becomes a Wait block), split after.
				return steps[:i+1], true
			}
			// Bare wait: split before (no block for this step).
			return steps[:i], true
		}
	}
	return steps, false
}

// CommittedAtDispatch reports whether the pickup at step i is sent to the fleet
// by the dispatch itself: it sits in the SplitAtWait prefix — before the plan's
// first wait of ANY kind, which is what the create sends — and no step at or
// before it touches a lane node, since the splice can put a gate wait in front
// of a lane entry at fleet create and delay the lift behind it. isLaneNode nil
// skips the lane test, for callers that only rank legs.
func CommittedAtDispatch(steps []Step, i int, isLaneNode func(string) bool) bool {
	if i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionPickup || steps[i].Node == "" {
		return false
	}
	pre, _ := SplitAtWait(steps)
	if i >= len(pre) {
		return false
	}
	if isLaneNode == nil {
		return true
	}
	for j := 0; j <= i; j++ {
		if steps[j].Action != protocol.ActionWait && steps[j].Node != "" && isLaneNode(steps[j].Node) {
			return false
		}
	}
	return true
}

// HeldForRelease reports whether the drop at step i waits for a station release:
// a station-owned wait comes before it. A leg with none is never a (b) dropper,
// because nothing then stands between its dispatch and its drop for the release
// fence to hold. protocol.IsStationWaitKind still counts an untagged wait as the
// station's during the drain window; when that arm goes, so does this answer for
// untagged plans, and TestHeldForRelease_UntaggedWait pins it so the change is
// seen.
func HeldForRelease(steps []Step, i int) bool {
	if i > len(steps) {
		i = len(steps)
	}
	for j := 0; j < i; j++ {
		if steps[j].Action == protocol.ActionWait && protocol.IsStationWaitKind(steps[j].WaitKind) {
			return true
		}
	}
	return false
}

// LiftsBeforeFirstWait is the stage-1 ranking key: a leg with a pickup the
// dispatch commits runs first, so the leg after it can be handed that fact. It
// reads the persisted plan and asks no lane question — ranking a leg first grants
// nothing, and the grant asks the full CommittedAtDispatch.
func LiftsBeforeFirstWait(steps []Step) bool {
	for i := range steps {
		if CommittedAtDispatch(steps, i, nil) {
			return true
		}
	}
	return false
}

// ── THE SLOT BOOK ───────────────────────────────────────────────────────────

// SlotBookDB is the slot book's read surface: what stands on a node and who
// holds it, and what an order holds.
type SlotBookDB interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	ListBinsByNode(nodeID int64) ([]*bins.Bin, error)
	BinsReservedByOrder(orderID int64) ([]int64, error)
}

// BinsAtStep is the first read of the slot book: what a plan finds on a node
// when it reaches step `at` — the bins on the node now, less the ones this
// plan's earlier pickups there take.
//
// ── ONE QUESTION, FOUR ASKERS ────────────────────────────────────────────────
//
// Four checks ask whether a node is clear, and each used to ask it of the world
// NOW — the wrong moment for every step after the plan's first. A choreography
// can place onto a node that holds a bin right now because its own earlier step
// carries that bin away, and it can re-collect at a node that holds a bin right
// now because that bin is the one its earlier step takes, not the one it set
// down. Keep-staged combined is both: it lifts the kept bin off inbound staging,
// stages its own carrier there, and collects that carrier later.
//
//   - the destination gate (reserveComplexDestination) asks it of a declared
//     exclusive dropoff, before anything is reserved;
//   - the relay rule (reserveComplexPlan) asks it of a re-collect;
//   - the slot claim (confirmComplexPlan, and the solo order's hand) asks it of
//     the staging slot it is about to take, and ConfirmSlotClaim re-checks the
//     answer inside the claim;
//   - the vacated-slot rule (Vacated) asks it of a drop that would otherwise
//     wait, as case (a). Its case (b) — the PARTNER's lift — is not this
//     function's: this counts one plan's own steps, and the partner's bin is
//     credited separately.
//
// Asked a different way by any one of them, the same plan is clear to one check
// and blocked at the next — which is how the keep-staged combined supply got past
// the gate and then held a partial set for ever at the reserve.
//
// ── WHICH BIN AN EARLIER PICKUP TAKES ─────────────────────────────────────────
//
// One: the bin this order already holds there, by reservation or claim, else —
// while it holds none there yet — one nobody holds. A bin another order holds is
// not this plan's to take whatever its steps say, so it stays on the node, and so
// does this plan's own bin once it has lost the hold; both are exactly what the
// slot claim must still refuse. A pickup that re-collects a carrier this plan set
// down at the node earlier takes that carrier, not a bin that was already there.
//
// taken is what the earlier pickups take; remaining is what is left. A dropoff at
// `at` finds the node clear exactly when remaining is empty.
func BinsAtStep(db SlotBookDB, orderID int64, steps []Step, at int, node string) (
	remaining []*bins.Bin, taken []int64, err error) {
	if node == "" {
		return nil, nil, nil
	}
	n, err := db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return nil, nil, nil // an unresolvable node holds nothing anyone can count
	}
	onNode, err := db.ListBinsByNode(n.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list bins at %s: %w", node, err)
	}
	if len(onNode) == 0 {
		return nil, nil, nil
	}
	reserved, err := db.BinsReservedByOrder(orderID)
	if err != nil {
		return nil, nil, fmt.Errorf("list reservations for order %d: %w", orderID, err)
	}
	ours := make(map[int64]bool, len(reserved))
	for _, id := range reserved {
		ours[id] = true
	}
	for _, b := range onNode {
		if b.ClaimedBy != nil && *b.ClaimedBy == orderID {
			ours[b.ID] = true
		}
	}

	if at > len(steps) {
		at = len(steps)
	}
	takenSet := make(map[int64]bool)
	carried := 0 // carriers this plan has set down at the node and not yet re-collected
	for _, s := range steps[:at] {
		if s.Node != node {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			carried++
		case protocol.ActionPickup:
			if carried > 0 {
				carried--
				continue
			}
			if b := takenByPickup(onNode, takenSet, ours); b != nil {
				takenSet[b.ID] = true
			}
		}
	}
	for _, b := range onNode {
		if takenSet[b.ID] {
			taken = append(taken, b.ID)
		} else {
			remaining = append(remaining, b)
		}
	}
	return remaining, taken, nil
}

// takenByPickup is the bin one earlier pickup takes off the node: one this order
// holds, else one nobody holds. nil when every bin left there is somebody else's.
func takenByPickup(onNode []*bins.Bin, taken, ours map[int64]bool) *bins.Bin {
	for _, b := range onNode {
		if !taken[b.ID] && ours[b.ID] {
			return b
		}
	}
	for _, b := range onNode {
		if !taken[b.ID] && b.ClaimedBy == nil && !b.HasPendingReservation && !b.Locked {
			return b
		}
	}
	return nil
}

// ── THE RULE ────────────────────────────────────────────────────────────────

// IsLaneNode reports whether a node is a lane or a lane slot. Unreadable counts
// as a lane: the answer only ever refuses a grant, and refusing is today's wait.
func (p *Planner) IsLaneNode(name string) bool {
	n, err := p.db.GetNodeByDotName(name)
	if err != nil || n == nil {
		return true
	}
	return p.nodeIsLaneOrLaneSlot(n)
}

func (p *Planner) nodeIsLaneOrLaneSlot(n *nodes.Node) bool {
	if n.NodeTypeCode == protocol.NodeClassLANE {
		return true
	}
	if n.ParentID == nil {
		return false
	}
	parent, err := p.db.GetNode(*n.ParentID)
	if err != nil || parent == nil {
		return true
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE
}

// Vacated is the rule's bin half: whether the drop at step i onto node is
// cleared by a committed lift, and the stamp that says which. nil means today's
// answer stands. The in-flight half is the caller's, through its own predicate.
// need.Order is the leg asking; need.Pair is its partner's leg, or nil.
//
// ONE BIN, TAKEN. The node must hold exactly one bin and that bin must be the
// one the lift takes. Two bins, or a bin held by anyone else, is a node this
// lift does not empty.
func (p *Planner) Vacated(need Need, steps []Step, i int, node string) *Vacate {
	defer observe(p.db, "vacated")()
	if need.Order == 0 || node == "" || i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionDropoff {
		return nil
	}
	n, err := p.db.GetNodeByDotName(node)
	if err != nil || n == nil || n.IsSynthetic || p.nodeIsLaneOrLaneSlot(n) {
		return nil
	}
	remaining, taken, err := BinsAtStep(p.db, need.Order, steps, i, node)
	if err != nil {
		return nil
	}
	// (a) L's own earlier pickup takes the only bin.
	if len(remaining) == 0 && len(taken) == 1 {
		return &Vacate{Node: node, Bin: taken[0]}
	}
	if len(remaining) != 1 || len(taken) != 0 {
		return nil
	}
	// (b) the partner's committed lift takes it.
	pc := need.Pair
	if pc == nil || pc.Partner == 0 || !HeldForRelease(steps, i) {
		return nil
	}
	b := remaining[0]
	if !heldByPartner(b, pc.Partner) {
		return nil
	}
	for _, l := range pc.Lifts {
		if l.Bin != b.ID || l.Node != node {
			continue
		}
		if CommittedAtDispatch(pc.Steps, l.Step, p.IsLaneNode) && PartnerLiftsResident(pc.Steps, l.Step, node) {
			return &Vacate{Node: node, Bin: b.ID, Partner: pc.Partner}
		}
	}
	return nil
}

// heldByPartner reports whether the partner holds b: by its claim, or by its
// pending reservation with no claim on the bin.
//
// THE SOFT HOLD COUNTS HERE BECAUSE THE HAND RE-ASKS IT HARD. A pair is one
// hand (C2b A.2): when this leg is planned, its partner has only reserved its
// bins, and the claims are taken in the one transaction that takes both legs'
// hands, the lifter's first. This leg's slot claim then credits the partner's
// bin only if the partner holds it the way a confirmed claim leaves it, alive
// and linked (store/nodes ClaimSlotTx), so a partner that lost the bin between
// the plan and the hand refuses the hand, and the pair waits.
func heldByPartner(b *bins.Bin, partner int64) bool {
	if b.ClaimedBy != nil {
		return *b.ClaimedBy == partner
	}
	return b.HasPendingReservation && b.PendingReservedBy != nil && *b.PendingReservedBy == partner
}

// PartnerLiftsResident reports whether the partner's pickup at step k takes a bin
// that was on node before the partner arrived, rather than re-collecting a
// carrier the partner itself set down there earlier — BinsAtStep's carried
// logic, walked over the partner's plan. A re-collect vacates nothing.
func PartnerLiftsResident(steps []Step, k int, node string) bool {
	if k < 0 || k >= len(steps) || steps[k].Node != node || steps[k].Action != protocol.ActionPickup {
		return false
	}
	carried := 0
	for j := 0; j < k; j++ {
		if steps[j].Node != node {
			continue
		}
		switch steps[j].Action {
		case protocol.ActionDropoff:
			carried++
		case protocol.ActionPickup:
			if carried > 0 {
				carried--
			}
		}
	}
	return carried == 0
}

// VacatedGroupChild is the NGRP store arm: a drop into a group with no free
// child, asked of the children a committed lift is emptying — this leg's own
// earlier pickups (a) and the partner's claimed ones (b). The bin half is the
// rule's; everything else about the slot is asked through the resolver's own
// fences (binresolver.ResolveStoreVacated), including the in-flight count with
// this order left out. First admitted child wins; "" and nil mean capacity
// stands.
func (p *Planner) VacatedGroupChild(need Need, steps []Step, i int, group *nodes.Node) (string, *Vacate) {
	defer observe(p.db, "vacated-group")()
	var candidates []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			candidates = append(candidates, n)
		}
	}
	for j := 0; j < i; j++ {
		if steps[j].Action == protocol.ActionPickup {
			add(steps[j].Node)
		}
	}
	if need.Pair != nil {
		for _, l := range need.Pair.Lifts {
			add(l.Node)
		}
	}
	gr := &binresolver.GroupResolver{DB: p.db, DebugLog: p.dbg}
	for _, name := range candidates {
		n, err := p.db.GetNodeByDotName(name)
		if err != nil || n == nil || n.ParentID == nil || *n.ParentID != group.ID {
			continue
		}
		st := p.Vacated(need, steps, i, name)
		if st == nil {
			continue
		}
		carrier := binresolver.BinTypeFrom(p.BinTypeBeforeStep(steps, i),
			"complex step: no preceding pickup resolved to exactly one carrier")
		payload := ResolvedStepPayload(steps[i], need.PayloadCode)
		if _, err := gr.ResolveStoreVacated(group, n, []int64{st.Bin}, payload, carrier, need.Asker); err != nil {
			p.debug("vacated: order %d step %d: %s refused by the group's fences: %v", need.Order, i, name, err)
			continue
		}
		return name, st
	}
	return "", nil
}
