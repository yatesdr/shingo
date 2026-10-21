package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch/plan"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// vacate_hold.go — CORE HOLDS A RELEASE UNTIL A NAMED BIN HAS LEFT A NAMED NODE.
//
// ── WHAT IT REPLACES ────────────────────────────────────────────────────────
//
// A grant (b) drop (plan/vacated.go) is admitted on a promise: the partner's
// lift empties the node before this leg drops there. The fleet does not keep
// that promise — a release appends the segment whatever the partner has done —
// so a fence used to refuse the station's release while the bin still stood
// (arm Z), and refuse the partner too while this leg was held (arm Y). The
// station got invalid_state and a sentence, and someone had to press again
// once the robot had lifted. Core computed the fact and threw it away.
//
// ── WHAT IT DOES NOW ────────────────────────────────────────────────────────
//
// The station's release is TAKEN: its manifest sync is written, and the drop's
// stamp is marked held (Vacate.HeldAt). From then the wait is Core's
// (coreOwnsWait), the station is told kind "vacate", and Core appends the same
// segment through appendSegmentAndAdvance — ClaimAppend's CAS, so a second
// releaser appends nothing — once the bin is lifted off the node:
//
//	bins.last_lift_at > stamp.stamped_at  AND  bins.node_id <> the stamp's node
//
// read from the lift itself (bins.LeftSince), never from the bin's recorded
// node alone. A ghost eviction moves a bin to _TRANSIT without a lift and so
// never releases; a lift that returns the bin to the node does not either.
//
// Y is stamped exactly where it used to refuse (ruled 2026-10-18): this leg's
// sibling is staged with a grant in its pending segment, and this leg drops on
// a node holding a bin that sibling has claimed. The sibling stamp has its own
// kind and is written here, at release — the pair is with the fleet by then —
// so no slot claim ever credits it (plan.PartnerStamp).
//
// A home clear's partial move (clear_home.go) waits on the same fact BEFORE
// any robot is sent: its drop carries the stamp, the compound advance holds it
// (clearDropHeld), and RedriveVacateWaits advances its parent on the lift.
// When the lift will not come — the carrier stands on the home held by nobody
// — the clear's re-run (RerunClears) chooses again.
//
// ── WHAT ENDS A HOLD (the abandon sweep's bound) ────────────────────────────
//
//   - the lift: EventBinEnteredTransit re-drives the holds that name the bin
//     (RedriveVacateWaits), and the 60 s floor (SweepLaneWaiters) re-reads all
//     of them, so a missed event costs a minute, not a robot;
//   - the lifter's MOOT end (skipped, as the death rule reads moot): it
//     releases the hold (ruled 2026-10-18).
//     The order that was to lift the bin found nothing to lift; the hold's own
//     condition can no longer come true, and the swap's death rule leaves this
//     leg alive by design (HandleSwapPeerTerminal);
//   - the lifter's cancel or failure does NOT release: the death rule cancels
//     this leg (HandleSwapPeerTerminal). Until it does, or for a lifter whose
//     end the death rule does not cover, the row says CauseVacateLifterGone;
//   - the Core hard release, and this order's own end.
// The abandon sweep exempts a held leg (CoreOwnsWaitAt); nothing above is a
// timer.

// heldDrop is one drop a release must keep: its index in the plan and the
// stamp as it will be persisted.
type heldDrop struct {
	idx int
	st  plan.Vacate
}

// vacateSince is the moment a stamp's lift must come after: when the stamp was
// written, else (a stamp written before stamped_at existed) when it was held.
func vacateSince(st plan.Vacate) time.Time {
	switch {
	case st.StampedAt != nil:
		return *st.StampedAt
	case st.HeldAt != nil:
		return *st.HeldAt
	}
	return time.Time{}
}

// segmentStart is the index of the first step after wait waitIndex, counting
// waits exactly as splitSegment does; -1 when there is no such wait.
func segmentStart(steps []resolvedStep, waitIndex int) int {
	seen := 0
	for i, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			return i + 1
		}
		seen++
	}
	return -1
}

// vacateHoldsAtRelease is what a station release of order must keep: the
// fence stamps in the segment it would append whose bin has not yet left
// (arm Z's condition), else the sibling stamps arm Y's condition calls for.
// The segment is read as it will be sent (after patchRedirectSegments): a
// stamp on a drop the redirect re-pointed does not count.
func (d *Dispatcher) vacateHoldsAtRelease(order *orders.Order, steps []resolvedStep) []heldDrop {
	start := segmentStart(steps, order.WaitIndex)
	stored, moreWaits, _ := splitSegment(steps, order.WaitIndex)
	if start < 0 || stored == nil {
		return nil
	}
	seg := append([]resolvedStep(nil), stored...)
	d.patchRedirectSegments(seg, order, moreWaits)

	var holds []heldDrop
	var qs []bins.LeftQuery
	for k, s := range seg {
		st := plan.FenceStamp(s)
		if st == nil || stored[k].Node != s.Node {
			continue
		}
		holds = append(holds, heldDrop{idx: start + k, st: *st})
		qs = append(qs, bins.LeftQuery{Bin: st.Bin, Node: st.Node, Since: vacateSince(*st)})
	}
	if len(qs) > 0 {
		left, err := d.db.BinsLeftSince(qs)
		if err != nil {
			// Unread is not left: hold, and the floor re-reads it.
			log.Printf("dispatch: vacate: order %d: could not read whether its stamped bins left (%v) — holding", order.ID, err)
			return holds
		}
		var unmet []heldDrop
		for i, h := range holds {
			if !left[i] {
				unmet = append(unmet, h)
			}
		}
		if len(unmet) > 0 {
			return unmet
		}
	}
	return d.siblingHolds(order, steps, seg, stored, start)
}

// siblingHolds is arm Y's condition, stamped instead of refused: this leg's
// sibling is staged with a grant in its pending segment, and this leg drops on
// a node holding a bin that sibling has claimed. One stamp per such drop.
func (d *Dispatcher) siblingHolds(order *orders.Order, steps, seg, stored []resolvedStep, start int) []heldDrop {
	if order.SiblingOrderUUID == "" {
		return nil
	}
	z, err := d.db.GetOrderByUUID(order.SiblingOrderUUID)
	if err != nil || z == nil || z.Status != StatusStaged {
		return nil
	}
	zSeg, ok := d.pendingSegment(z)
	if !ok {
		return nil
	}
	granted := false
	for _, s := range zSeg {
		if plan.PartnerStamp(s) != nil {
			granted = true
			break
		}
	}
	if !granted {
		return nil
	}
	var holds []heldDrop
	for k, s := range seg {
		if s.Action != protocol.ActionDropoff || s.Node == "" || stored[k].Node != s.Node {
			continue
		}
		if plan.FenceStamp(steps[start+k]) != nil {
			continue // already stamped: arm Z's, read above
		}
		n, err := d.db.GetNodeByDotName(s.Node)
		if err != nil || n == nil {
			continue
		}
		onNode, err := d.db.ListBinsByNode(n.ID)
		if err != nil {
			continue
		}
		for _, b := range onNode {
			if b.ClaimedBy != nil && *b.ClaimedBy == z.ID {
				holds = append(holds, heldDrop{idx: start + k, st: plan.Vacate{
					Node: s.Node, Bin: b.ID, Partner: z.ID, Kind: plan.VacateKindSibling}})
				break
			}
		}
	}
	return holds
}

// holdRelease takes a station release Core cannot append yet: the manifest
// sync (the station's count is declared now, whatever the robot does next),
// the held stamps on the plan, the cause on the row, and the kind the station
// is told. Then it asks once more, so a lift that landed between the read and
// the write is not left to the floor.
func (d *Dispatcher) holdRelease(env *protocol.Envelope, order *orders.Order, p *protocol.OrderRelease,
	steps []resolvedStep, holds []heldDrop) {
	if err := d.syncManifestForRelease(env, order, p); err != nil {
		return
	}
	// The count is final now, as on a release that goes at once: an unfinished
	// carrier the segment lifts off the line goes to its claim's partial home
	// (place_by_contents.go), and the held segment appends from the re-pointed
	// plan. The re-point moves a drop off the line, never the held drop onto it,
	// so the holds' step indexes stand.
	if d.placePartialAtRelease(order, steps, order.WaitIndex) {
		if again, ok := decodeSteps(order.StepsJSON); ok {
			steps = again
		}
	}
	now := clock.Now().UTC()
	for _, h := range holds {
		st := h.st
		st.HeldAt = &now
		if st.StampedAt == nil {
			st.StampedAt = &now
		}
		steps[h.idx].Vacate = &st
	}
	j, err := json.Marshal(steps)
	if err != nil {
		log.Printf("dispatch: vacate hold: order %d: marshal: %v", order.ID, err)
		d.sendError(env, p.OrderUUID, "invalid_state", "Core could not record the hold on this order")
		return
	}
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(j)); err != nil {
		log.Printf("dispatch: vacate hold: order %d: %v", order.ID, err)
		d.sendError(env, p.OrderUUID, "invalid_state", "Core could not record the hold on this order")
		return
	}
	order.StepsJSON = string(j)
	first := holds[0].st
	label := d.binLabel(first.Bin)
	log.Printf("dispatch: release held (vacate): order %d drops on %s, which %s must leave first (partner order %d)",
		order.ID, first.Node, label, first.Partner)
	d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseVacateWaitingLift,
		QueueParams{Destination: first.Node, VacateBin: label})
	if d.redriveVacateOrder(order, 0) {
		return
	}
	d.replies.SendStaged(env, order.EdgeUUID,
		fmt.Sprintf("held by Core until %s leaves %s", label, first.Node), nil, WaitKindVacate)
}

// binLabel names a bin for a sentence: its label, else "bin <id>".
func (d *Dispatcher) binLabel(id int64) string {
	if b, err := d.db.GetBin(id); err == nil && b != nil && b.Label != "" {
		return b.Label
	}
	return fmt.Sprintf("bin %d", id)
}

// vacateStampsAt is what must hold before the segment after wait waitIndex
// goes: the held fence stamps in it, or, at a stored vacate wait (a plan Core
// authors), every honoured stamp in it.
func vacateStampsAt(steps []resolvedStep, waitIndex int, storedVacate bool) []plan.Vacate {
	seg, _, _ := splitSegment(steps, waitIndex)
	var out []plan.Vacate
	for _, s := range seg {
		st := plan.StampHonoured(s)
		if st == nil {
			continue
		}
		if storedVacate || (st.Partner != 0 && st.HeldAt != nil) {
			out = append(out, *st)
		}
	}
	return out
}

// RedriveVacateWaits is PopVacateHeld's re-driver: it re-asks every vacate hold
// and lets go the ones whose fact now holds. binID narrows the pass to the
// holds that name that bin — EventBinEnteredTransit's — and 0 asks all of them:
// the order-terminal events (a lifter's moot end releases; its cancel or
// failure is recorded) and the floor. Returns the orders it released.
func (d *Dispatcher) RedriveVacateWaits(binID int64) []int64 {
	released, _ := d.RedriveVacate(binID)
	return released
}

// RedriveVacate is RedriveVacateWaits also returning the home clears' partial
// moves still held on a bin that has not left (stranded candidates for the
// clear's re-run, RerunClearsFor), so a caller that re-runs reads the
// population once.
//
// Statements: ONE population read (VacateWaitCandidates, "who waits on this
// bin" — with a binID, no row when nothing waits on it); per hold it asks, one
// fact read (bins.LeftSince), and on the 0 pass one partner read per stamp
// whose bin has not left; per release, the append path (a station-released
// hold) or the compound advance (a clear's partial move).
func (d *Dispatcher) RedriveVacate(binID int64) (released, stranded []int64) {
	cands, err := d.db.VacateWaitCandidates(binID)
	if err != nil {
		log.Printf("dispatch: vacate re-drive: could not read the held set: %v (the floor retries)", err)
		return nil, nil
	}
	for _, o := range cands {
		// A HOME CLEAR'S PARTIAL MOVE is held on the same fact before any robot
		// is sent (clear_home.go, clearDropHeld): the lift that empties the home
		// lets its parent advance it.
		if o.ParentOrderID != nil && o.VendorOrderID == "" {
			sent, held := d.redriveClearPartial(o, binID)
			if sent {
				released = append(released, o.ID)
			} else if held {
				stranded = append(stranded, o.ID)
			}
			continue
		}
		if d.redriveVacateOrder(o, binID) {
			released = append(released, o.ID)
		}
	}
	return released, stranded
}

// redriveVacateOrder re-asks one hold. True when it appended the segment.
func (d *Dispatcher) redriveVacateOrder(o *orders.Order, binID int64) bool {
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok || !coreOwnsWait(steps, o.WaitIndex) {
		return false
	}
	w, _ := waitAt(steps, o.WaitIndex)
	if w.WaitKind == WaitKindLane {
		return false // the lane evaluator's
	}
	stamps := vacateStampsAt(steps, o.WaitIndex, w.WaitKind == WaitKindVacate)
	if len(stamps) == 0 {
		return false
	}
	if binID != 0 {
		named := false
		for _, st := range stamps {
			named = named || st.Bin == binID
		}
		if !named {
			return false
		}
	}
	qs := make([]bins.LeftQuery, len(stamps))
	for i, st := range stamps {
		qs[i] = bins.LeftQuery{Bin: st.Bin, Node: st.Node, Since: vacateSince(st)}
	}
	left, err := d.db.BinsLeftSince(qs)
	if err != nil {
		log.Printf("dispatch: vacate re-drive: order %d: could not read its stamped bins (%v) — still held", o.ID, err)
		return false
	}
	met, lifterGone, moot := true, false, int64(0)
	var gone plan.Vacate
	for i, st := range stamps {
		if left[i] {
			continue
		}
		if binID == 0 && st.Partner != 0 {
			if lifter, err := d.db.GetOrder(st.Partner); err == nil && lifter != nil && protocol.IsTerminal(lifter.Status) {
				if endedMoot(lifter) {
					moot = lifter.ID
					continue
				}
				lifterGone, gone = true, st
			}
		}
		met = false
	}
	if !met {
		if lifterGone && QueueCause(o.QueueCause) != CauseVacateLifterGone {
			d.setQueueReason(o, protocol.QueueWaitingForSlot, CauseVacateLifterGone,
				QueueParams{Destination: gone.Node, VacateBin: d.binLabel(gone.Bin), VacateLifterGone: true})
		}
		return false
	}
	if moot != 0 {
		log.Printf("dispatch: vacate: order %d released on the moot end of order %d, which was to lift the bin", o.ID, moot)
		d.db.AppendAudit("order", o.ID, "vacate_released_moot", "",
			fmt.Sprintf("held release appended: order %d, which was to lift the bin, ended skipped (moot)", moot), "system")
	}
	segment, moreWaits, blockOffset := splitSegment(steps, o.WaitIndex)
	if segment == nil {
		return false
	}
	d.patchRedirectSegments(segment, o, moreWaits)
	if err := d.appendSegmentAndAdvance(o, segment, moreWaits, blockOffset, "vacate release"); err != nil {
		if !errors.Is(err, ErrAppendNotOwed) {
			log.Printf("dispatch: vacate release: order %d: %v (still held; the next event or the floor retries)", o.ID, err)
		}
		return IsAppendLanded(err)
	}
	d.evaluateLaneWaitAfterAppend(o)
	return true
}

// endedMoot reports whether a lifter ended MOOT in the swap death rule's own
// sense (HandleSwapPeerTerminal): skipped, and the leg that takes the line's
// bin — it found no bin to lift. Its sibling survives the death rule, so the
// sibling's hold on that bin is released. A skipped supply is not moot: it is
// a death, and the rule cancels the sibling, so its hold must not append.
func endedMoot(lifter *orders.Order) bool {
	if lifter.Status != StatusSkipped {
		return false
	}
	steps, ok := decodeSteps(lifter.StepsJSON)
	return ok && legTakesLineBin(steps, lifter.ProcessNode)
}

// sweepVacateWaiters is PopVacateHeld's floor, run on the lane floor's 60 s
// tick: every hold re-asked, and a record for each it released — a release
// the event path should have made.
func (d *Dispatcher) sweepVacateWaiters() {
	released, stranded := d.RedriveVacate(0)
	for _, id := range released {
		d.recordFloorRelease(floorWaiter{orderID: id, pop: PopVacateHeld, cause: CauseVacateWaitingLift})
	}
	// The home clear's re-run is floored on this tick too: its state (the empty
	// on the home, held by nobody, while the partial's move waits) has no event
	// of its own when a pull's soft hold is reaped.
	d.RerunClearsFor(stranded)
}
