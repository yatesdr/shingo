package dispatch

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// clear_home.go — A DEDICATED LOADER HOME'S CLEAR, AUTHORED BY CORE.
//
// The station declares the carrier on a home H empty (E) and asks for a buffer
// partial P of the home's part to come home from its spot S. The clear is a
// COMPOUND PARENT (the job's row: the fence key, the exemption key, the re-run
// anchor) with PLAIN-MOVE CHILDREN, the dig's shape (compound.go). A child is
// never a multi-lift order (STOP 1, 2026-10-18): one pickup, one drop.
//
//	the partial's move   P S→H, sequence 2, born at confirm. Its drop carries
//	                     the vacate stamp (E, H). On the demand and spot arms
//	                     Core holds it before dispatch until E has been lifted
//	                     off H (clearDropHeld, the admission gate), so no robot
//	                     stands holding P while E's carrier is fetched.
//	the empty's move     E H→X, sequence 1, born only on the spot arm, or
//	                     E H→S, sequence 1, on the trade.
//
// THE ARMS (choose, re-runnable; confirm runs it once, the re-run again):
//
//	demand  a queued empty pull that could see E takes it through its own
//	        finder — the exemption in the empty finders' loader arm makes E
//	        sourceable while the clear lives (bins.emptyCarrierWhere), and a
//	        group-sourced pull for another position of the same loader (the
//	        loader's own demand) sees E even when the home stands outside its
//	        group (bins.InGroupArm). The pull lifts E; P goes on the lift.
//	spot    else an open buffer spot X takes E: the empty's move, E reserved
//	        at birth so no other finder takes it.
//	trade   else two robots trade the bins: the empty's move E H→S and the
//	        partial's move P S→H, sibling-linked, each plan
//	        [pickup, wait(vacate), drop] with its drop stamped on the bin the
//	        other lifts (issueClearTrade). Both are sent in one advance or
//	        neither (tradePartnerReady); each robot lifts, stands under its bin,
//	        and Core appends its drop once the other bin has left
//	        (redriveVacateOrder). The swap's death rule binds them
//	        (HandleSwapPeerTerminal): a leg's failure, cancel or skip, or the
//	        fleet refusing the second leg's create, ends the other, unless the
//	        other's drop is already appended. "" from choose only when the
//	        trade cannot be born (clearTradeRefusal); the caller ends the job.
//
// WHAT FENCES H AND X. No reservation: a child naming its bin counts inbound to
// its delivery node from birth (orders.InFlightForDropoffSQL's dig-leg arm),
// which the plain capacity gate and the loader-window gate read
// (TestClearFence_ChildDropCountsInboundFromBirth).
//
// WHAT IS NOT A DIG ABOUT IT. The parent holds no lane, and a lost leg never
// sends it back to `pending` (the dig's Restart would put the job in the
// scanner's population): clearChapterEnd and clearLegFailed decide instead.
// SweepStalledChapters reads a vacate-held child as a wait (chapter_floor.go).

// IsClearParent reports whether an order is a home clear's parent.
func IsClearParent(o *orders.Order) bool {
	return o != nil && o.ParentOrderID == nil && strings.HasPrefix(o.EdgeUUID, orders.ClearUUIDPrefix)
}

// ClearJob is the clear as the door resolved it, before anything is written.
type ClearJob struct {
	Station   string
	Home      *nodes.Node
	LoaderID  int64
	Payload   string
	Empty     *bins.Bin // E, standing on Home
	Partial   *bins.Bin // P
	PartialAt *nodes.Node
}

// ClearIssue is what IssueClearTx wrote, for AfterClearIssue once committed.
type ClearIssue struct {
	Parent  *orders.Order
	Partial *orders.Order
	after   func()
	// taken is what the station's issue of the partial's move took from
	// strangers, and partial the bin it took it on.
	taken   Taken
	partial *bins.Bin
}

// ErrClearPartialPromised is a clear's issue refused because the partial is
// not the station's to take: a robot's order holds it (a confirmed hold), or
// it has left the spot the clear read it on.
var ErrClearPartialPromised = errors.New("the partial is held by another order")

// clearStamp is the vacate stamp on a clear child's drop: the bin that must
// leave the node first. nil for a child that carries none (the empty's move).
func clearStamp(o *orders.Order) *plan.Vacate {
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		return nil
	}
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Vacate != nil {
			return s.Vacate
		}
	}
	return nil
}

// heldTradePlan is a home clear's trade leg's stored plan: a compound child
// whose plan carries a stored vacate wait (issueClearTrade writes it; no other
// writer does). ok=false for every other order, after a substring test that
// decodes nothing, so the plain create seam pays no decode for it.
func heldTradePlan(o *orders.Order) ([]resolvedStep, bool) {
	if o == nil || o.ParentOrderID == nil || !strings.Contains(o.StepsJSON, `"wait_kind":"vacate"`) {
		return nil, false
	}
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		return nil, false
	}
	for _, s := range steps {
		if s.Action == protocol.ActionWait && s.WaitKind == WaitKindVacate {
			return steps, true
		}
	}
	return nil, false
}

// holdsAfterLift reports whether o is a home clear's trade leg: it lifts at
// dispatch and holds its drop on its partner's lift.
func holdsAfterLift(o *orders.Order) bool {
	_, ok := heldTradePlan(o)
	return ok
}

// tradeSteps is one trade leg's plan: lift at from, hold (a bare vacate wait,
// so the create stops at the lift and the robot stands under its bin), drop
// at to once the stamp's bin has left it.
func tradeSteps(from, to string, st plan.Vacate) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: from},
		{Action: protocol.ActionWait, WaitKind: WaitKindVacate},
		{Action: protocol.ActionDropoff, Node: to, Vacate: &st},
	}
}

// dispatchHeldTrade is the create seam's trade arm (dispatchToFleetCore): the
// stored plan persisted as it is, the create its lift alone and unsealed
// (buildUnsealedCreate, the lane valve's create), the hand and the fleet
// through commitToFleet declaring the pickup's node. Then the hold is asked
// once (redriveVacateOrder): the partner may have lifted already, and its
// lift's re-drive did not see this order while it had no vendor order. A trade
// never touches a lane (clearTradeRefusal refuses one at birth), so the plan
// is not spliced.
func (d *Dispatcher) dispatchHeldTrade(order *orders.Order, steps []resolvedStep, sourceNode *nodes.Node, payloadCode string, hand Hand) (string, error) {
	req, _, err := d.buildUnsealedCreate(order, steps, d.loadSequenceForPayload(payloadCode), "held")
	if err != nil {
		return "", err
	}
	if err := d.commitToFleet(order, req, "dispatcher", hand, sourceNode); err != nil {
		return "", err
	}
	d.dbg("order %d dispatched as %s (%s, held for its drop at %s)", order.ID, req.OrderID, sourceNode.Name, order.DeliveryNode)
	d.emitter.EmitOrderDispatched(order.ID, req.OrderID, order.SourceNode, order.DeliveryNode)
	d.redriveVacateOrder(order, 0)
	return req.OrderID, nil
}

// IssueClearTx issues the clear's parent and the partial's move on the
// caller's transaction (the door commits the empty's zero with them): the
// parent issued and moved to `reshuffling` (transitionTx — never in the
// scanner's population, even across a crash), then the partial's move, a leg of
// the parent at its turn, holding the partial from its issue (the Drain race).
// The station asked for the clear, so its issue takes a stranger's pending hold
// on the partial out loud (AfterClearIssue ends one that is an order for it);
// a robot's hold refuses it. Statements: the parent's INSERT and history, its
// status CAS, the leg's INSERT and history, the partial's hold.
func (d *Dispatcher) IssueClearTx(tx *sql.Tx, j ClearJob) (*ClearIssue, error) {
	now := clock.Now().UTC()
	parent, _, err := issue(tx, Need{
		Type: protocol.OrderTypeMove, Station: j.Station, Ref: orders.ClearUUIDPrefix + uuid.New().String(),
		Quantity: 1, PayloadCode: j.Payload,
		Desc: fmt.Sprintf("clear %s: %s comes home from %s", j.Home.Name, j.Partial.Label, j.PartialAt.Name),
		// Housekeeping the station asked for; no place is asking for material.
		OriginClass: protocol.OriginClassNoDemand,
		// The station asked for it and is sent its rows (AfterClearIssue).
		StationKnows: true,
		Source:       j.Home.Name, Delivery: j.Home.Name,
	}, plan.Plan{})
	if err != nil {
		return nil, fmt.Errorf("issue the clear's parent: %w", err)
	}
	after, err := d.lifecycle.transitionTx(tx, parent, StatusReshuffling, Event{
		Actor: "system", Reason: fmt.Sprintf("clearing %s: the partial %s comes home", j.Home.Name, j.Partial.Label)})
	if err != nil {
		return nil, fmt.Errorf("move the clear's parent to reshuffling: %w", err)
	}
	steps, err := json.Marshal([]resolvedStep{
		{Action: protocol.ActionPickup, Node: j.PartialAt.Name},
		{Action: protocol.ActionDropoff, Node: j.Home.Name,
			Vacate: &plan.Vacate{Node: j.Home.Name, Bin: j.Empty.ID, StampedAt: &now}},
	})
	if err != nil {
		return nil, err
	}
	need := clearLeg(parent, clearPartialSeq, j.Partial, j.PartialAt.Name, j.Home.Name,
		fmt.Sprintf("clear %s: partial %s home from %s", j.Home.Name, j.Partial.Label, j.PartialAt.Name))
	need.Steps = string(steps)
	child, taken, err := issue(tx, need, plan.Plan{})
	if errors.Is(err, reservations.ErrReservationConflict) || errors.Is(err, reservations.ErrNotTheNamedBin) {
		return nil, ErrClearPartialPromised
	}
	if err != nil {
		return nil, fmt.Errorf("issue the partial's move: %w", err)
	}
	return &ClearIssue{Parent: parent, Partial: child, after: after, taken: taken, partial: j.Partial}, nil
}

// clearLeg is one plain-move leg of a clear, holding its bin from its issue as
// the station's act (Person): the cart a move of its parent's, at its turn.
func clearLeg(parent *orders.Order, seq int, bin *bins.Bin, from, to, desc string) Need {
	return Need{
		Type: OrderTypeMove, Station: parent.StationID, Ref: uuid.New().String(), Quantity: 1,
		PayloadCode: bin.PayloadCode, Desc: desc, Source: from, Delivery: to, StationKnows: true,
		OriginID: parent.OriginID, OriginClass: parent.OriginClass,
		Leg:  &Leg{Parent: parent.ID, Sequence: seq},
		Cart: bin.ID, By: Person(parent.StationID),
	}
}

// AfterClearIssue is the committed issue's tail: the transition's actions, an
// order for the partial whose hold the station's issue took ended by name, the
// rows pushed to the station (parent and child — a child is not admitted, so
// nothing else would push it), and the partial's hold on its row.
func (d *Dispatcher) AfterClearIssue(b *ClearIssue, home string, emptyBin int64) {
	b.after()
	d.lifecycle.failTakenForCart(b.taken, b.Partial, b.partial)
	d.projectClear(b.Parent)
	d.projectClear(b.Partial)
	d.setQueueReason(b.Partial, protocol.QueueWaitingForSlot, CauseVacateWaitingLift,
		QueueParams{Destination: home, VacateBin: d.binLabel(emptyBin)})
}

// projectClear pushes one of the clear's rows to its station.
func (d *Dispatcher) projectClear(o *orders.Order) {
	if o == nil || o.StationID == "" {
		return
	}
	d.emitter.ProjectOrder(o.StationID, ProjectionFor(o))
}

// ClearFacts is what choose needs, read back from the parent and the
// partial's move — the same facts at confirm and on a re-run.
type ClearFacts struct {
	Parent    *orders.Order
	Home      *nodes.Node
	LoaderID  int64
	EmptyBin  *bins.Bin
	PartialAt string // the partial's node, never an open spot while P stands there
	// Partial is the partial's move, which the trade re-plans in place.
	Partial *orders.Order
	// Prepared says Pulls and Spot were read by the caller under the same hold
	// of the Door (the confirm's refusal check reads both before the birth), so
	// choose does not read them twice; with no Spot the caller has also asked
	// whether the trade can be born (ClearTradeRefusal). A re-run leaves it
	// false.
	Prepared bool
	Pulls    []int64
	Spot     *nodes.Node
}

// clearFactsOf reads a live clear's facts from its parent and partial's move.
func (d *Dispatcher) clearFactsOf(parent, partial *orders.Order) (ClearFacts, error) {
	f := ClearFacts{Parent: parent, PartialAt: partial.SourceNode, Partial: partial}
	st := clearStamp(partial)
	if st == nil {
		return f, fmt.Errorf("clear %d: its partial's move carries no stamp", parent.ID)
	}
	home, err := d.db.GetNodeByDotName(st.Node)
	if err != nil || home == nil {
		return f, fmt.Errorf("clear %d: home %q: %v", parent.ID, st.Node, err)
	}
	h, err := d.db.GetLoaderHomeByPositionNode(home.ID)
	if err != nil || h == nil {
		return f, fmt.Errorf("clear %d: %s is no loader position (%v)", parent.ID, home.Name, err)
	}
	e, err := d.db.GetBin(st.Bin)
	if err != nil || e == nil {
		return f, fmt.Errorf("clear %d: the empty %d: %v", parent.ID, st.Bin, err)
	}
	f.Home, f.LoaderID, f.EmptyBin = home, h.LoaderID, e
	return f, nil
}

// ClearSpot is the first open buffer spot of the loader that takes the
// empty's carrier type: enabled, not excluded (the partial's node), and free to
// the plain capacity gate (no bin, nothing inbound). nil when there is none.
func (d *Dispatcher) ClearSpot(loaderID int64, e *bins.Bin, exclude string) (*nodes.Node, error) {
	members, err := d.db.ListLoaderHomes(loaderID)
	if err != nil {
		return nil, err
	}
	capable, err := d.db.ListLoaderHomeBinTypes(loaderID)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if m.Kind != "buffer" {
			continue
		}
		if types, restricted := capable[m.PositionNodeID]; restricted && !containsCode(types, e.BinTypeCode) {
			continue
		}
		n, err := d.db.GetNode(m.PositionNodeID)
		if err != nil || n == nil || !n.Enabled || n.Name == exclude {
			continue
		}
		if blocked, _ := CheckDropoffCapacityForType(d.db, n.Name, 0, binresolver.KnownCarrier(e)); blocked {
			continue
		}
		return n, nil
	}
	return nil, nil
}

func containsCode(codes []string, c string) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}

// ClearPulls is the demand arm's candidates: the acquiring empty pulls whose
// own finders could see a carrier on the home (orders.ClearPullCandidates).
func (d *Dispatcher) ClearPulls(home *nodes.Node, loaderID int64) ([]int64, error) {
	members, err := d.db.ListLoaderHomes(loaderID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(members))
	for _, m := range members {
		if n, err := d.db.GetNode(m.PositionNodeID); err == nil && n != nil {
			names = append(names, n.Name)
		}
	}
	return d.db.ClearPullCandidates(home.ID, home.Name, names)
}

// ChooseClearLocked runs the clear's choice for a caller inside the scanner's
// Door: demand, then spot, then the trade. "" only when the trade cannot be
// born (ClearTradeRefusal); the caller ends the job. A re-run comes through
// here exactly as the confirm does.
//
// The demand arm is targeted, not a pass: each candidate is asked once,
// through its own finder with no pick (AcquireLocked(o, nil)), in demand
// order, and the arm stops at the first that takes the empty. Statements: the
// candidate read, then per candidate its acquire and one read of the empty.
func (d *Dispatcher) ChooseClearLocked(f ClearFacts) (string, error) {
	asked := false
	if d.acquirer != nil {
		pulls := f.Pulls
		if !f.Prepared {
			var err error
			if pulls, err = d.ClearPulls(f.Home, f.LoaderID); err != nil {
				return "", err
			}
		}
		for _, id := range pulls {
			asked = true
			d.acquirer.AcquireLocked(id, nil)
			free, err := d.db.BinStandsUnheld(f.EmptyBin.ID, f.Home.Name)
			if err != nil {
				return "", err
			}
			if !free {
				log.Printf("dispatch: clear %d: demand arm — order %d took the empty %s off %s",
					f.Parent.ID, id, f.EmptyBin.Label, f.Home.Name)
				return protocol.ClearArmDemand, nil
			}
		}
	}
	// A pull the demand arm sent may have been bound for the prepared spot, so
	// the spot is read again whenever a pull was asked.
	x := f.Spot
	if !f.Prepared || asked {
		var err error
		if x, err = d.ClearSpot(f.LoaderID, f.EmptyBin, f.PartialAt); err != nil {
			return "", err
		}
	}
	if x == nil {
		return d.chooseClearTrade(f)
	}
	if err := d.issueClearSpot(f, x); err != nil {
		return "", err
	}
	log.Printf("dispatch: clear %d: spot arm — the empty %s goes to %s", f.Parent.ID, f.EmptyBin.Label, x.Name)
	return protocol.ClearArmSpot, nil
}

// issueClearSpot issues the empty's move E H→X, a leg of the clear at sequence
// 1, holding E from its issue, in one transaction under the parent found live
// (store.OpenCompoundTx): E is held at its issue, so a tier-2 fill cannot take
// it and skip the move. The clear is the station's act, so a stranger's pending
// hold on E is taken out loud.
func (d *Dispatcher) issueClearSpot(f ClearFacts, x *nodes.Node) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if err := store.OpenCompoundTx(tx, f.Parent.ID, nil); err != nil {
		return err
	}
	child, taken, err := issue(tx, clearLeg(f.Parent, 1, f.EmptyBin, f.Home.Name, x.Name,
		fmt.Sprintf("clear %s: empty %s to %s", f.Home.Name, f.EmptyBin.Label, x.Name)), plan.Plan{})
	if err != nil {
		return fmt.Errorf("issue the empty's move: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.lifecycle.failTakenForCart(taken, child, f.EmptyBin)
	d.projectClear(child)
	return nil
}

// chooseClearTrade is choose's last arm: no pull took the empty and no spot is
// open, so the two robots trade. "" when the trade cannot be born.
func (d *Dispatcher) chooseClearTrade(f ClearFacts) (string, error) {
	if f.Partial == nil || f.Partial.BinID == nil {
		return "", fmt.Errorf("clear %d: the trade needs the partial's move and its bin", f.Parent.ID)
	}
	if !f.Prepared || f.Spot != nil {
		s, err := d.db.GetNodeByDotName(f.PartialAt)
		if err != nil || s == nil {
			return "", fmt.Errorf("clear %d: the partial's spot %q: %v", f.Parent.ID, f.PartialAt, err)
		}
		p, err := d.db.GetBin(*f.Partial.BinID)
		if err != nil || p == nil {
			return "", fmt.Errorf("clear %d: the partial %d: %v", f.Parent.ID, *f.Partial.BinID, err)
		}
		refusal, err := d.ClearTradeRefusal(f.LoaderID, f.Home, s, f.EmptyBin, p)
		if err != nil {
			return "", err
		}
		if refusal != "" {
			log.Printf("dispatch: clear %d: no arm — the trade cannot be born: %s", f.Parent.ID, refusal)
			return "", nil
		}
	}
	if err := d.issueClearTrade(f); err != nil {
		return "", err
	}
	log.Printf("dispatch: clear %d: trade arm — the empty %s goes to %s and the partial comes home to %s",
		f.Parent.ID, f.EmptyBin.Label, f.PartialAt, f.Home.Name)
	return protocol.ClearArmTrade, nil
}

// ClearTradeRefusal is the one sentence saying why the trade cannot be born,
// "" when it can. The trade puts the empty on the partial's spot and the
// partial on the home, so each window must take the other carrier's type
// (ListLoaderHomeBinTypes, as ClearSpot asks of a spot); and neither may be a
// lane slot, because a trade leg's plan is not spliced for a lane gate
// (dispatchHeldTrade). Statements: the window types, and one lane read per node.
func (d *Dispatcher) ClearTradeRefusal(loaderID int64, home, spot *nodes.Node, e, p *bins.Bin) (string, error) {
	capable, err := d.db.ListLoaderHomeBinTypes(loaderID)
	if err != nil {
		return "", err
	}
	if types, restricted := capable[spot.ID]; restricted && !containsCode(types, e.BinTypeCode) {
		return fmt.Sprintf("%s does not take %s's carrier type %s", spot.Name, e.Label, e.BinTypeCode), nil
	}
	if types, restricted := capable[home.ID]; restricted && !containsCode(types, p.BinTypeCode) {
		return fmt.Sprintf("%s does not take %s's carrier type %s", home.Name, p.Label, p.BinTypeCode), nil
	}
	for _, n := range []*nodes.Node{home, spot} {
		lane, err := d.db.LaneForNode(n.ID)
		if err != nil {
			return "", err
		}
		if lane != nil {
			return fmt.Sprintf("%s is a slot of lane %s, and two robots cannot trade inside a lane", n.Name, lane.Name), nil
		}
	}
	return "", nil
}

// issueClearTrade issues the trade in one transaction under the parent found
// live (store.OpenCompoundTx): the empty's move E H→S (a leg at sequence 1,
// plan [pickup H, wait(vacate), drop S] stamped on the partial), holding E from
// its issue as the station's act, and the partial's move re-planned in place to
// [pickup S, wait(vacate), drop H] (its stamp on E kept, with the time it was
// written at confirm), the two naming each other (sibling_order_uuid: the
// pair, minted by Core, both uuids Core's). The swap's death rule reads that
// link (HandleSwapPeerTerminal). Neither stamp names a partner order: a
// partner stamp is what the slot claim and the release fence read, and these
// drops are declared by the clear, never granted.
//
// The partial's move is only re-planned while the fleet does not have it
// (BindTradeLegTx's guard). Its row is not pushed again: the projection
// carries neither its plan nor its sibling.
func (d *Dispatcher) issueClearTrade(f ClearFacts) error {
	l1 := f.Partial
	st := clearStamp(l1)
	if st == nil {
		return fmt.Errorf("clear %d: the partial's move %d carries no stamp", f.Parent.ID, l1.ID)
	}
	now := clock.Now().UTC()
	l2Steps, err := json.Marshal(tradeSteps(f.Home.Name, f.PartialAt,
		plan.Vacate{Node: f.PartialAt, Bin: *l1.BinID, StampedAt: &now}))
	if err != nil {
		return err
	}
	need := clearLeg(f.Parent, 1, f.EmptyBin, f.Home.Name, f.PartialAt,
		fmt.Sprintf("clear %s: empty %s to %s, trading with the partial", f.Home.Name, f.EmptyBin.Label, f.PartialAt))
	need.Steps, need.Sibling = string(l2Steps), l1.EdgeUUID
	l1Steps, err := json.Marshal(tradeSteps(f.PartialAt, f.Home.Name, *st))
	if err != nil {
		return err
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if err := store.OpenCompoundTx(tx, f.Parent.ID, nil); err != nil {
		return err
	}
	l2, taken, err := issue(tx, need, plan.Plan{})
	if err != nil {
		return fmt.Errorf("issue the empty's move: %w", err)
	}
	bound, err := orders.BindTradeLegTx(tx, l1.ID, string(l1Steps), l2.EdgeUUID)
	if err != nil {
		return fmt.Errorf("re-plan the partial's move: %w", err)
	}
	if !bound {
		return fmt.Errorf("clear %d: the partial's move %d has been sent or has ended", f.Parent.ID, l1.ID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l1.StepsJSON, l1.SiblingOrderUUID = string(l1Steps), l2.EdgeUUID
	d.lifecycle.failTakenForCart(taken, l2, f.EmptyBin)
	d.projectClear(l2)
	return nil
}

// EndClear ends a clear that has nowhere to go: every live child not carrying
// its bin is cancelled, and the parent with them when none is carrying (a
// carrying child sets its bin down and the chapter's end closes the parent).
// The home keeps the zero; nothing else is undone.
func (d *Dispatcher) EndClear(parent *orders.Order, why string) {
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: end clear %d: list children: %v", parent.ID, err)
		return
	}
	var live []*orders.Order
	for _, c := range children {
		if !protocol.IsTerminal(c.Status) {
			live = append(live, c)
		}
	}
	cancel, kept, err := d.splitLiftedLegs(parent.ID, live)
	if err != nil {
		cancel, kept = live, nil
	}
	for _, c := range cancel {
		d.lifecycle.CancelOrder(c, parent.StationID, "clear ended: "+why, CancelCause{})
	}
	if len(kept) == 0 {
		d.lifecycle.CancelOrder(parent, parent.StationID, why, CancelCause{})
	}
	log.Printf("dispatch: clear %d ended: %s", parent.ID, why)
}

// clearDropHeld is the admission gate on a clear child's drop: true (held,
// cause on the row) while the stamped bin has not been lifted off the node
// since the stamp. A child with no stamp is never held, and neither is a trade
// leg: it holds its drop in its plan, after its lift (dispatchHeldTrade). One
// statement (bins.LeftSince) per ask; the label read only when the cause
// changes.
func (d *Dispatcher) clearDropHeld(o *orders.Order) bool {
	st := clearStamp(o)
	if st == nil || holdsAfterLift(o) {
		return false
	}
	left, err := d.db.BinsLeftSince([]bins.LeftQuery{{Bin: st.Bin, Node: st.Node, Since: vacateSince(*st)}})
	if err == nil && len(left) == 1 && left[0] {
		return false
	}
	if err != nil {
		log.Printf("dispatch: clear child %d: could not read whether bin %d left %s (%v) — holding", o.ID, st.Bin, st.Node, err)
	}
	if c := QueueCause(o.QueueCause); c != CauseVacateWaitingLift && c != CauseVacateLifterGone {
		d.setQueueReason(o, protocol.QueueWaitingForSlot, CauseVacateWaitingLift,
			QueueParams{Destination: st.Node, VacateBin: d.binLabel(st.Bin)})
	}
	return true
}

// redriveClearPartial is the vacate re-drive's answer for a clear's partial
// move held before dispatch — a row of PopVacateHeld's one population read
// (VacateWaitCandidates): when its stamped bin has left the node, its parent
// is advanced. binID, when not 0, is the lifted bin; a move whose stamp names
// another is not asked. sent: this call dispatched it. held: its bin has not
// left (a stranded candidate for the clear's re-run). Statements: one fact
// read; on a release, the compound advance.
func (d *Dispatcher) redriveClearPartial(o *orders.Order, binID int64) (sent, held bool) {
	st := clearStamp(o)
	if st == nil || (binID != 0 && st.Bin != binID) {
		return false, false
	}
	left, err := d.db.BinsLeftSince([]bins.LeftQuery{{Bin: st.Bin, Node: st.Node, Since: vacateSince(*st)}})
	if err != nil || len(left) != 1 {
		return false, false
	}
	if !left[0] {
		return false, true
	}
	dispatched := false
	if err := d.advanceCompound(*o.ParentOrderID, &dispatched); err != nil {
		log.Printf("dispatch: clear re-drive: advance clear %d: %v", *o.ParentOrderID, err)
	}
	return dispatched, false
}

// RerunClears is the clear's re-run over the whole population: one read of
// "who waits on a bin" (RedriveVacate(0)), then RerunClearsFor the partial
// moves still held. The floor and an order's end do exactly this, in two
// halves (the read on the event's goroutine, the re-run on the after-queue).
func (d *Dispatcher) RerunClears() int {
	_, stranded := d.RedriveVacate(0)
	return d.RerunClearsFor(stranded)
}

// RerunClearsFor is the clear's re-run, keyed on STATE: for each held partial
// move (ids from the vacate re-drive's one read) whose clear's empty stands on
// the home held by no live order — its pull was cancelled, or demoted and its
// hold reaped; the spot move failed before its lift — it reuses the live
// partial's move and never re-zeroes: it writes CauseVacateLifterGone on that
// move, then runs choose again under the scanner's Door — demand, spot, else
// the job ends with the home keeping the zero. A clear whose empty's move is
// live holds the empty, so it is not stranded. Must not run on an event-bus
// goroutine (the Door).
//
// Statements: per id one order read and one BinStandsUnheld; the state is asked again inside
// the Door before anything is written. No id, no statement.
func (d *Dispatcher) RerunClearsFor(partials []int64) int {
	ran := 0
	for _, id := range partials {
		o, err := d.db.GetOrder(id)
		if err != nil || o == nil || !d.clearStranded(o) {
			continue
		}
		d.door(func() {
			if d.rerunClearLocked(id) {
				ran++
			}
		})
	}
	return ran
}

// clearStranded is the re-run's state for one held partial move: its stamped
// bin stands on its node and nobody holds it.
func (d *Dispatcher) clearStranded(partial *orders.Order) bool {
	st := clearStamp(partial)
	if st == nil {
		return false
	}
	free, err := d.db.BinStandsUnheld(st.Bin, st.Node)
	return err == nil && free
}

// rerunClearLocked is one re-run, inside the Door: the state re-read under the
// lock, then choose. True when it acted. A partial's move that is a trade leg
// is never re-run: the trade's own death rule ends it when its partner ends
// (HandleSwapPeerTerminal, clearLegFailed), and a re-run would choose again
// for a move whose partner is already born.
func (d *Dispatcher) rerunClearLocked(partialID int64) bool {
	partial, err := d.db.GetOrder(partialID)
	if err != nil || partial == nil || partial.ParentOrderID == nil || protocol.IsTerminal(partial.Status) ||
		partial.VendorOrderID != "" || holdsAfterLift(partial) || !d.clearStranded(partial) {
		return false
	}
	parent, err := d.db.GetOrder(*partial.ParentOrderID)
	if err != nil || !IsClearParent(parent) || protocol.IsTerminal(parent.Status) {
		return false
	}
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		return false
	}
	for _, c := range children {
		if c.ID != partial.ID && !protocol.IsTerminal(c.Status) {
			return false
		}
	}
	f, err := d.clearFactsOf(parent, partial)
	if err != nil {
		log.Printf("dispatch: clear re-run: %v", err)
		return false
	}
	d.setQueueReason(partial, protocol.QueueWaitingForSlot, CauseVacateLifterGone,
		QueueParams{Destination: f.Home.Name, VacateBin: f.EmptyBin.Label, VacateLifterGone: true})
	arm, err := d.ChooseClearLocked(f)
	if err != nil {
		log.Printf("dispatch: clear re-run %d: %v (the floor retries)", parent.ID, err)
		return false
	}
	if arm == "" {
		d.EndClear(parent, fmt.Sprintf("nothing will take the empty %s off %s: no pull wants it, no buffer spot is open and two robots cannot trade",
			f.EmptyBin.Label, f.Home.Name))
		return true
	}
	d.setQueueReason(partial, protocol.QueueWaitingForSlot, CauseVacateWaitingLift,
		QueueParams{Destination: f.Home.Name, VacateBin: f.EmptyBin.Label})
	log.Printf("dispatch: clear re-run %d: %s arm", parent.ID, arm)
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		log.Printf("dispatch: clear re-run %d: advance: %v", parent.ID, err)
	}
	return true
}

// clearPartialSeq is the partial's move's sequence under its clear's parent
// (IssueClearTx); the empty's move, on the spot arm or the trade, is 1.
const clearPartialSeq = 2

// clearPartialOf returns the clear's partial move: the stamped child at the
// partial's sequence. A trade's empty's move carries a stamp too.
func clearPartialOf(children []*orders.Order) *orders.Order {
	for _, c := range children {
		if c.Sequence == clearPartialSeq && clearStamp(c) != nil {
			return c
		}
	}
	return nil
}

// clearChapterEnd is advanceCompoundChapterEnd for a clear's parent: nothing
// to dispatch. While any child lives the job waits (a lost empty's move is the
// re-run's, never a Restart). When all have ended: the partial's move done is
// the job done; otherwise the job ends with what it changed.
func (d *Dispatcher) clearChapterEnd(parent *orders.Order, children []*orders.Order) error {
	if protocol.IsTerminal(parent.Status) {
		return nil
	}
	for _, c := range children {
		if !protocol.IsTerminal(c.Status) {
			return nil
		}
	}
	p := clearPartialOf(children)
	if p != nil && p.Status == StatusConfirmed {
		if err := d.lifecycle.CompleteCompound(parent); err != nil {
			log.Printf("dispatch: complete clear %d: %v", parent.ID, err)
		}
		if err := d.db.CompleteOrder(parent.ID); err != nil {
			log.Printf("dispatch: complete clear %d: %v", parent.ID, err)
		}
		return nil
	}
	why := "the partial's move ended without landing"
	if p != nil {
		why = fmt.Sprintf("the partial's move ended %s", p.Status)
	}
	d.lifecycle.CancelOrder(parent, parent.StationID, why, CancelCause{})
	return nil
}

// clearLegFailed is HandleChildOrderFailure for a clear: the partial's move
// failing ends the job (the empty's move, if it has not lifted, goes with it);
// the empty's move failing ends nothing — the partial's move stays, and the
// re-run is keyed on the state that leaves (the empty on the home, held by
// nobody). A trade leg is the exception on both counts: its failure is the
// swap's death (HandleSwapPeerTerminal, the one rule its cancel and skip already
// reach through the engine), and the job ends when both legs have.
func (d *Dispatcher) clearLegFailed(parent *orders.Order, childID int64) {
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: clear %d: list children after leg %d failed: %v", parent.ID, childID, err)
		return
	}
	for _, c := range children {
		if c.ID == childID && c.SiblingOrderUUID != "" && holdsAfterLift(c) {
			d.endTradeLeg(parent, childID, SwapTerminalFailed)
			return
		}
	}
	if p := clearPartialOf(children); p != nil && p.ID == childID {
		d.EndClear(parent, "the partial's move failed")
		return
	}
	if err := d.clearChapterEnd(parent, children); err != nil {
		log.Printf("dispatch: clear %d: %v", parent.ID, err)
	}
}

// endTradeLeg is the trade's death, from the clear's side: the swap's rule
// ends the dead leg's partner (HandleSwapPeerTerminal — cancelled with
// TermPeerTerminal, unless its drop is already appended), and the job ends
// once both legs have (clearChapterEnd). kind is the dead leg's terminal
// (SwapTerminal*).
func (d *Dispatcher) endTradeLeg(parent *orders.Order, deadID int64, kind string) {
	d.HandleSwapPeerTerminal(deadID, kind)
	fresh, err := d.db.GetOrder(parent.ID)
	if err != nil || fresh == nil {
		log.Printf("dispatch: clear %d: reload after trade leg %d ended: %v", parent.ID, deadID, err)
		return
	}
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: clear %d: list children after trade leg %d ended: %v", parent.ID, deadID, err)
		return
	}
	if err := d.clearChapterEnd(fresh, children); err != nil {
		log.Printf("dispatch: clear %d: %v", parent.ID, err)
	}
}

// tradePartnerReady is the trade's both-or-neither, asked of a trade leg after
// its own admission and before it is sent (advanceCompound): when its partner
// is not yet with the fleet, the partner is asked what its own turn will ask —
// its two nodes, admission, and that its bin is claimable by the clear — and
// if any of those says no, this leg is not sent either, so neither robot lifts
// a bin the other cannot come for. The leg rests unsent with the partner's
// cause on its row, and the re-drive (the vacate floor, redriveVacate) asks
// again. True for every other child, and for the second leg once the first is
// with the fleet. Statements: the partner's read; when it is unsent, two node
// reads, admission's lane reads, and one bin read.
func (d *Dispatcher) tradePartnerReady(leg *orders.Order) bool {
	if leg.SiblingOrderUUID == "" || !holdsAfterLift(leg) {
		return true
	}
	sib, err := d.db.GetOrderByUUID(leg.SiblingOrderUUID)
	if err != nil || sib == nil {
		log.Printf("dispatch: trade leg %d: could not read its partner %s (%v) — holding", leg.ID, leg.SiblingOrderUUID, err)
		d.setQueueReason(leg, protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{})
		return false
	}
	if protocol.IsTerminal(sib.Status) {
		// The death rule ends this leg; it is not sent meanwhile.
		return false
	}
	if sib.VendorOrderID != "" {
		return true
	}
	code, cause, params, ok := d.tradeLegSendable(sib, leg)
	if ok {
		return true
	}
	log.Printf("dispatch: trade leg %d held: its partner %d cannot go (%s), and neither goes without the other",
		leg.ID, sib.ID, cause)
	d.setQueueReason(leg, code, cause, params)
	return false
}

// tradeLegSendable asks of an unsent trade leg what advanceCompound will ask
// at its turn, short of the hand: its nodes resolve, admission admits, and its
// bin is free or held by its own clear (ClaimCompoundLegBinTx's rule).
func (d *Dispatcher) tradeLegSendable(o, partner *orders.Order) (protocol.QueueCode, QueueCause, QueueParams, bool) {
	src, err := d.db.GetNodeByDotName(o.SourceNode)
	if err != nil || src == nil {
		return protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{}, false
	}
	dst, err := d.db.GetNodeByDotName(o.DeliveryNode)
	if err != nil || dst == nil {
		return protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{}, false
	}
	v, err := d.admit(admissionSituation{order: o, sourceNode: src, destNode: dst})
	if err != nil {
		return protocol.QueueWaitingForSlot, CauseAdmissionError, QueueParams{Destination: dst.Name}, false
	}
	if !v.Admitted() {
		return protocol.QueueStorageRearranging, v.Cause(), QueueParams{Lane: v.Lane(), Payload: o.PayloadCode}, false
	}
	if o.BinID != nil {
		b, err := d.db.GetBin(*o.BinID)
		if err != nil || b == nil {
			return protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{}, false
		}
		if c := b.ClaimedBy; c != nil && (o.ParentOrderID == nil || *c != *o.ParentOrderID) && *c != o.ID && *c != partner.ID {
			return protocol.QueueStorageRearranging, CauseDigBlockerClaimed, QueueParams{Payload: o.PayloadCode}, false
		}
	}
	return "", "", QueueParams{}, true
}

// tradeSecondLegRefused is parkLegOnFleetRefusal's trade arm: the fleet
// refused a trade leg's create while its partner is already with the fleet.
// That is a death, not a demote: the partner's robot would otherwise stand
// under its lifted bin waiting on a lift no robot is coming for. The refused
// leg is cancelled (TermPeerTerminal, it holds nothing the fleet has), and the
// death rule takes the partner (endTradeLeg). A refusal of the first leg sent
// is the ordinary demote: nothing of the trade is with the fleet, and the
// vacate floor sends both again (redriveVacate). Every other pair keeps its
// demote (commitPairToFleet). True when it ended the trade.
func (d *Dispatcher) tradeSecondLegRefused(parentOrderID int64, leg *orders.Order, cause error) bool {
	if leg.SiblingOrderUUID == "" || !holdsAfterLift(leg) {
		return false
	}
	sib, err := d.db.GetOrderByUUID(leg.SiblingOrderUUID)
	if err != nil || sib == nil || sib.VendorOrderID == "" || protocol.IsTerminal(sib.Status) {
		return false
	}
	parent, err := d.db.GetOrder(parentOrderID)
	if err != nil || parent == nil {
		return false
	}
	log.Printf("dispatch: clear %d: the fleet refused trade leg %d's create while its partner %d is with the fleet: %v — ending the trade",
		parentOrderID, leg.ID, sib.ID, cause)
	d.lifecycle.CancelOrder(leg, leg.StationID,
		fmt.Sprintf("the fleet refused this trade leg (%v) after its partner order %d was sent; the trade ends", cause, sib.ID),
		CancelCause{Code: protocol.TermPeerTerminal})
	d.endTradeLeg(parent, leg.ID, SwapTerminalCancelled)
	return true
}
