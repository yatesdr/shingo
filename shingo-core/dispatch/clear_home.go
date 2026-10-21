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
// never a multi-lift order (STOP 1, 2026-10-18): the compound path dispatches
// a child as a two-node transport and never reads a plan.
//
//	the partial's move   P S→H, sequence 2, born at confirm. Its drop carries
//	                     the vacate stamp (E, H): Core holds it before dispatch
//	                     until E has been lifted off H (clearDropHeld, the
//	                     admission gate), so no robot stands holding P while
//	                     E's carrier is fetched.
//	the empty's move     E H→X, sequence 1, born only on the spot arm.
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
//	none    else the job ends (EndClear); the home keeps the zero.
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

// ClearBirth is what BirthClearTx wrote, for AfterClearBirth once committed.
type ClearBirth struct {
	Parent    *orders.Order
	Partial   *orders.Order
	after     func()
	displaced []store.DisplacedByHand
	steals    store.Steals
}

// ErrClearPartialPromised is a birth refused because another order outranks
// the clear for the partial (the steal's refusal).
var ErrClearPartialPromised = errors.New("the partial is promised to another order")

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

// BirthClearTx writes the clear's parent and the partial's move on the
// caller's transaction (the door commits the empty's zero with them): the
// parent born and moved to `reshuffling` (transitionTx — never in the
// scanner's population, even across a crash), the child with its stamp, and
// the child's soft hold on the partial (the Drain race). Statements: the
// parent's INSERT and history, its status CAS, the child's INSERT and history,
// the steal's victim read, the soft hold.
func (d *Dispatcher) BirthClearTx(tx *sql.Tx, j ClearJob) (*ClearBirth, error) {
	now := clock.Now().UTC()
	parent, err := Build(tx, Need{
		Type: protocol.OrderTypeMove, Station: j.Station, Ref: orders.ClearUUIDPrefix + uuid.New().String(),
		Quantity: 1, PayloadCode: j.Payload,
		Desc: fmt.Sprintf("clear %s: %s comes home from %s", j.Home.Name, j.Partial.Label, j.PartialAt.Name),
		// Housekeeping the station asked for; no place is asking for material.
		OriginClass: protocol.OriginClassNoDemand,
		// The station asked for it and is sent its rows (AfterClearBirth).
		StationKnows: true,
		Source:       j.Home.Name, Delivery: j.Home.Name,
	}, plan.Plan{})
	if err != nil {
		return nil, fmt.Errorf("birth the clear's parent: %w", err)
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
	child := d.clearChild(parent, 2, j.Partial, j.PartialAt.Name, j.Home.Name,
		fmt.Sprintf("clear %s: partial %s home from %s", j.Home.Name, j.Partial.Label, j.PartialAt.Name))
	child.StepsJSON = string(steps)
	displaced, steals, err := store.CreateCompoundChildrenTx(tx, []store.CompoundChild{{Order: child, BinID: j.Partial.ID}}, nil)
	if err != nil {
		var claimed *store.BlockerClaimedError
		if errors.As(err, &claimed) {
			return nil, ErrClearPartialPromised
		}
		return nil, fmt.Errorf("birth the partial's move: %w", err)
	}
	if err := reservations.Acquire(tx, child.ID, parent.ID, j.Partial.ID, "clear"); err != nil {
		if errors.Is(err, reservations.ErrReservationConflict) {
			return nil, ErrClearPartialPromised
		}
		return nil, fmt.Errorf("hold the partial: %w", err)
	}
	return &ClearBirth{Parent: parent, Partial: child, after: after, displaced: displaced, steals: steals}, nil
}

// clearChild is one plain-move child of a clear.
func (d *Dispatcher) clearChild(parent *orders.Order, seq int, bin *bins.Bin, from, to, desc string) *orders.Order {
	return &orders.Order{
		EdgeUUID: uuid.New().String(), StationID: parent.StationID, OrderType: OrderTypeMove,
		Status: StatusPending, Quantity: 1, ParentOrderID: &parent.ID, Sequence: seq,
		BinID: &bin.ID, PayloadCode: bin.PayloadCode, PayloadDesc: desc,
		SourceNode: from, DeliveryNode: to, SourceIntent: SourceIntentForType(OrderTypeMove),
		OriginID: parent.OriginID, OriginClass: parent.OriginClass,
	}
}

// AfterClearBirth is the committed birth's tail: the transition's actions,
// the displaced and stolen holds, the rows pushed to the station (parent and
// child — a child is not admitted, so nothing else would push it), and the
// partial's hold on its row.
func (d *Dispatcher) AfterClearBirth(b *ClearBirth, home string, emptyBin int64) {
	b.after()
	store.LogSteals(b.steals)
	d.failDisplacedByHand(b.displaced)
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
	// Prepared says Pulls and Spot were read by the caller under the same hold
	// of the Door (the confirm's refusal check reads both before the birth), so
	// choose does not read them twice. A re-run leaves it false.
	Prepared bool
	Pulls    []int64
	Spot     *nodes.Node
}

// clearFactsOf reads a live clear's facts from its parent and partial's move.
func (d *Dispatcher) clearFactsOf(parent, partial *orders.Order) (ClearFacts, error) {
	f := ClearFacts{Parent: parent, PartialAt: partial.SourceNode}
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
		if blocked, _ := CheckDropoffCapacityForType(d.db, n.Name, 0, &e.BinTypeID); blocked {
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
// Door: demand, then spot. "" when neither arm is open; the caller ends the
// job. A re-run comes through here exactly as the confirm does.
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
		return "", nil
	}
	if err := d.birthClearSpot(f, x); err != nil {
		return "", err
	}
	log.Printf("dispatch: clear %d: spot arm — the empty %s goes to %s", f.Parent.ID, f.EmptyBin.Label, x.Name)
	return protocol.ClearArmSpot, nil
}

// birthClearSpot writes the empty's move E H→X, sequence 1, and its soft hold
// on E, in one transaction: E is reserved at birth, so a tier-2 fill cannot
// take it and skip the move.
func (d *Dispatcher) birthClearSpot(f ClearFacts, x *nodes.Node) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	child := d.clearChild(f.Parent, 1, f.EmptyBin, f.Home.Name, x.Name,
		fmt.Sprintf("clear %s: empty %s to %s", f.Home.Name, f.EmptyBin.Label, x.Name))
	displaced, steals, err := store.CreateCompoundChildrenTx(tx, []store.CompoundChild{{Order: child, BinID: f.EmptyBin.ID}}, nil)
	if err != nil {
		return fmt.Errorf("birth the empty's move: %w", err)
	}
	if err := reservations.Acquire(tx, child.ID, f.Parent.ID, f.EmptyBin.ID, "clear"); err != nil {
		return fmt.Errorf("hold the empty: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	store.LogSteals(steals)
	d.failDisplacedByHand(displaced)
	d.projectClear(child)
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
// since the stamp. A child with no stamp is never held. One statement
// (bins.LeftSince) per ask; the label read only when the cause changes.
func (d *Dispatcher) clearDropHeld(o *orders.Order) bool {
	st := clearStamp(o)
	if st == nil {
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
// lock, then choose. True when it acted.
func (d *Dispatcher) rerunClearLocked(partialID int64) bool {
	partial, err := d.db.GetOrder(partialID)
	if err != nil || partial == nil || partial.ParentOrderID == nil || protocol.IsTerminal(partial.Status) ||
		partial.VendorOrderID != "" || !d.clearStranded(partial) {
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
		d.EndClear(parent, fmt.Sprintf("nothing will take the empty %s off %s: no pull wants it and no buffer spot is open",
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

// clearPartialOf returns the clear's partial move: the child carrying the stamp.
func clearPartialOf(children []*orders.Order) *orders.Order {
	for _, c := range children {
		if clearStamp(c) != nil {
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
// nobody).
func (d *Dispatcher) clearLegFailed(parent *orders.Order, childID int64) {
	children, err := d.db.ListChildOrders(parent.ID)
	if err != nil {
		log.Printf("dispatch: clear %d: list children after leg %d failed: %v", parent.ID, childID, err)
		return
	}
	if p := clearPartialOf(children); p != nil && p.ID == childID {
		d.EndClear(parent, "the partial's move failed")
		return
	}
	if err := d.clearChapterEnd(parent, children); err != nil {
		log.Printf("dispatch: clear %d: %v", parent.ID, err)
	}
}
