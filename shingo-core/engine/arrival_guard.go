package engine

import (
	"fmt"
	"sync"

	"shingocore/store/bins"
	"shingocore/store/orders"
)

// ── THE ARRIVAL CLAIM GUARD ───────────────────────────────────────────────
//
// One question, asked by every writer that moves a bin to an order's
// destination: MAY THIS ORDER PLACE THIS BIN?
//
// It exists because of the SMN_001 / SMN_002 teleports: an order whose bin was
// re-claimed and moved between the fleet's FINISHED and Core processing it would
// otherwise drag that bin's record to its own destination, teleporting a bin that
// was physically somewhere else.
//
// IT USED TO BE FOUR COPIES. Delivery-time and completion-time, each in a
// single-bin and a multi-bin flavour, every one re-deriving the same predicate
// inline. Four copies of a rule is four places for it to drift, and they had
// already drifted in the way that mattered most: two logged through logFn and two
// through dbg — a nil-able debug hook — so with debug off HALF THE REFUSALS WERE
// INVISIBLE. A guard nobody can hear is indistinguishable from a guard that isn't
// there (standing law 9).
//
// AND ALL FOUR WERE VOID. Each logged and returned; none reported the refusal to
// anything. The completion path runs on the fleet receipt and never learned that
// the arrival it was completing had been refused, so an order could be told "you
// did not deliver this" and go `confirmed` two seconds later. Observed on the rig
// 2026-08-11: bin 17 refused twice, for two different orders, 76s apart, both
// refusals silent, both orders confirmed, the bin left at _TRANSIT (PLAN §R.5).
//
// WHAT HAPPENS TO A REFUSED ORDER IS NOT DECIDED HERE. Whether it should fail or
// park in a needs-attention state is an open owner ruling, deliberately deferred
// until the post-fix count below exists — the population that prompted the
// question was one bin two orders both owned, and its cause is now fixed
// upstream. This file makes the refusal SAYABLE and COUNTABLE. It does not act on
// it beyond declining the move, which is the behaviour that was always correct.

// ArrivalRefusal is a refused placement: the order tried to move a bin it does
// not own. Site names the writer that refused, so a count can be read back to a
// path without grepping.
//
// It carries the bin's POSITION and the intended destination as well as the
// claim, because the one survivor of the corrected soak could not be diagnosed
// afterwards: `order_bins` rows are deleted at terminal, and there is no bin
// position history, so by the time anyone reads the line the evidence is gone.
// A tripwire that fires and leaves nothing behind buys a count and no diagnosis.
type ArrivalRefusal struct {
	OrderID   int64
	BinID     int64
	ClaimedBy *int64
	Site      string
	// AtNodeID is where the bin actually sits; DestNodeID where this order meant
	// to put it. Zero means unknown/unset rather than node 0.
	AtNodeID   int64
	DestNodeID int64
}

// Reason renders the refusal for an operator-facing line or a cause string.
func (r *ArrivalRefusal) Reason() string {
	claimed := "nobody"
	if r.ClaimedBy != nil {
		claimed = fmt.Sprintf("order %d", *r.ClaimedBy)
	}
	return fmt.Sprintf("bin %d is claimed by %s, not by order %d", r.BinID, claimed, r.OrderID)
}

// Context is the diagnosable half: where the bin is versus where it was owed.
func (r *ArrivalRefusal) Context() string {
	at := "unknown"
	if r.AtNodeID != 0 {
		at = fmt.Sprintf("node %d", r.AtNodeID)
	}
	dest := "unknown"
	if r.DestNodeID != 0 {
		dest = fmt.Sprintf("node %d", r.DestNodeID)
	}
	return fmt.Sprintf("bin is at %s, this order's destination was %s", at, dest)
}

// refuseArrival is THE predicate. nil means the placement may proceed.
//
// COMPOUND CHILDREN ARE EXEMPT, and that is not a loophole: one multi-step plan
// touching a bin in several legs claims it for the LAST leg only, so an interim
// child legitimately moves a bin its sibling holds. CreateCompoundChildren's
// sibling-scoped compare-and-set is what makes this safe — a bin claimed from
// OUTSIDE the compound fails the whole transaction, so a child reaching here
// cannot be carrying an unrelated order's bin.
func refuseArrival(order *orders.Order, bin *bins.Bin, destNodeID int64, site string) *ArrivalRefusal {
	if order == nil || bin == nil {
		return nil
	}
	if order.ParentOrderID != nil {
		return nil
	}
	if bin.ClaimedBy != nil && *bin.ClaimedBy == order.ID {
		return nil
	}
	at := int64(0)
	if bin.NodeID != nil {
		at = *bin.NodeID
	}
	return &ArrivalRefusal{
		OrderID: order.ID, BinID: bin.ID, ClaimedBy: bin.ClaimedBy, Site: site,
		AtNodeID: at, DestNodeID: destNodeID,
	}
}

// binAlreadyAt answers "is there anything left to place?" — one question, asked
// by both settle sites, spelled once so they cannot drift (law 3's rider: one
// spelling OF ONE QUESTION; these two genuinely ask the same one).
//
// It is NOT the whole of either site's decision, which is why it is a small
// predicate and not a shared guard: reapplyRefused wraps it in a terminal cut it
// alone needs, and the delivery-time loop asks it for a reason of its own (see
// applyMultiBinArrivalForOrder). What both need is the same test, in the same
// place — BEFORE the ownership question — because a bin that already landed is a
// finished delivery whoever holds the claim by now, and asking about ownership
// first is what made this instrument unreadable twice (121, then 2).
//
// A nil node_id — the bin is at _TRANSIT or unplaced — is NOT "already there".
func binAlreadyAt(bin *bins.Bin, destNodeID int64) bool {
	return bin != nil && bin.NodeID != nil && *bin.NodeID == destNodeID
}

// reapplyRefused is the COMPLETION-TIME question, and it is not the same
// question as refuseArrival even though the two share an ownership test.
//
// A delivery-time writer is ABOUT TO PLACE, so it must own the bin and any
// mismatch is a refusal. The completion safety net asks something else — is
// there anything left to re-apply? — and its three answers were already written
// out at the call site long before this extraction:
//
//   - claimed_by == this  → re-apply; the arrival was missed.
//   - claimed_by == nil   → ApplyArrival ALREADY RAN and released the claim (or
//     the order failed/cancelled). Nothing to do. This is
//     the SUCCESS case and it is the common one.
//   - claimed_by == other → a newer order owns the bin now. Re-applying would
//     clobber it — the SMN_001/SMN_002 teleport. A real
//     refusal.
//
// Extracting on the shape of the predicate instead of on the question it
// answers made every ordinary completion count as a refusal: 121 of them in a
// nine-minute rig soak, against an instrument whose whole value is reading zero.
//
// ── THE ARRIVED CHECK COMES FIRST, AND IT LIVES IN HERE ───────────────────
// Asking about ownership before asking whether the bin ALREADY LANDED is what
// made the count unreadable, and a first correction only silenced the nil arm —
// which left the `other` arm counting a second benign shape: bin delivered,
// claim released, the NEXT order claims it, and a repeat completion firing sees
// the new owner. Nothing is wrong in that sequence either.
//
// So the destination is a parameter and the arrived test is the first thing
// asked. Reaching the ownership question therefore MEANS THE BIN DID NOT ARRIVE,
// and only then is a claim that is not ours a finding. The ordering is inside
// this function rather than left to the two call sites on purpose: an ordering
// that must be remembered is an ordering that will eventually be forgotten.
//
// Past that gate the question is exactly the delivery writers' question, so it
// is answered by the same refuseArrival rather than a second spelling of it —
// including the nil arm, which at that point means the order lost its claim AND
// its bin never landed. That is the bin-17 shape (PLAN §R.5).
func reapplyRefused(order *orders.Order, bin *bins.Bin, destNodeID int64, site string) (skip bool, refusal *ArrivalRefusal) {
	if order == nil || bin == nil {
		return false, nil
	}
	if order.ParentOrderID != nil {
		return false, nil
	}
	// ── A TERMINAL ORDER'S SAFETY NET HAS NO WORK TO DO ───────────────────
	// The completion handler fires twice: on (X → delivered) and again on
	// (delivered → confirmed). `delivered` is NOT terminal — it has an outgoing
	// transition — so the first firing still runs the net and still recovers a
	// missed arrival. That is the firing the net exists for.
	//
	// The LATER firings are the problem. By then the order is done and its bin has
	// often, correctly, moved on to a successor that claimed it — so "not at my
	// destination, owned by someone else" is the expected state of a finished
	// order, not a defect. It was the fourth benign shape to reach this counter,
	// and the only one the arrived check below cannot catch: a completed order's
	// bin has genuinely left. Specimen: order 7 (confirmed, dest LSD_023) against
	// bin 62, already held by live order 67 at LSD_022 (PLAN §R.12).
	//
	// Skipping HERE and not at the call sites is deliberate: handleMultiBinCompleted
	// deletes its junction rows on exactly this terminal firing, and an early return
	// up there would leak them. Skip the bin's work, not the handler's.
	//
	// orderIsTerminal, not a bare protocol.IsTerminal: the fail-closed arm for an
	// unset status lives inside the predicate, spelled once, and its reasoning is
	// written out there. This site used to carry that arm inline while its twin at
	// the junction-row delete did not — one rule, two spellings, and the fail-open
	// one guarding the destructive act (D3).
	if orderIsTerminal(order) {
		return true, nil
	}
	if binAlreadyAt(bin, destNodeID) {
		// It landed. There is nothing to re-apply and nothing to report — this is
		// the ordinary outcome of a delivery that worked, whoever holds the claim
		// by now.
		return true, nil
	}
	if r := refuseArrival(order, bin, destNodeID, site); r != nil {
		return true, r
	}
	return false, nil // still ours and not yet landed — re-apply, that is the net's job
}

// The sites, named once so the tally's keys cannot drift from the call sites.
const (
	arrivalSiteDelivery          = "delivery"
	arrivalSiteMultiBinDelivery  = "multi-bin delivery"
	arrivalSiteCompletionNet     = "completion safety-net"
	arrivalSiteMultiBinCompleted = "multi-bin completion safety-net"
)

// arrivalRefusalTally counts refusals per site for the run. It is the number the
// deferred fail-vs-park ruling waits on: if it stays at zero once the upstream
// causes are fixed, a refusal is an impossible state and failing loud is cheap
// and right; if it keeps climbing, it is a real recurring population and parking
// has to earn a releaser and a floor.
//
// In-process and reset-on-restart ON PURPOSE — it is a tripwire reading, not a
// fact anything recovers from. The durable evidence is the log line.
var arrivalRefusalTally = struct {
	mu     sync.Mutex
	bySite map[string]int
}{bySite: map[string]int{}}

// nil is "nothing was refused" and must stay cheap to pass in — callers hand it
// the predicate's result directly. A tripwire that panics the engine it watches
// is worse than the state it exists to catch.
func noteArrivalRefusal(r *ArrivalRefusal) {
	if r == nil {
		return
	}
	arrivalRefusalTally.mu.Lock()
	defer arrivalRefusalTally.mu.Unlock()
	arrivalRefusalTally.bySite[r.Site]++
}

// ArrivalRefusalTally returns refusals-so-far by site. Expected to be EMPTY: a
// refusal means two orders were pointed at one bin, which the claim lifecycle is
// supposed to make impossible.
func ArrivalRefusalTally() map[string]int {
	arrivalRefusalTally.mu.Lock()
	defer arrivalRefusalTally.mu.Unlock()
	out := make(map[string]int, len(arrivalRefusalTally.bySite))
	for k, v := range arrivalRefusalTally.bySite {
		out[k] = v
	}
	return out
}

// resetArrivalRefusalTally exists for tests, which must not inherit a count from
// whichever test ran before them.
func resetArrivalRefusalTally() {
	arrivalRefusalTally.mu.Lock()
	defer arrivalRefusalTally.mu.Unlock()
	arrivalRefusalTally.bySite = map[string]int{}
}

// ArrivalRefusalMarker is the per-event line's search string, named once so the
// emitter, the periodic tally and the guard test share one definition.
//
// The tally line must NOT contain it. A should-be-zero that quotes its own grep
// pattern is counted by that grep, so the number read back is
// tally-lines-plus-events and the counter reads non-zero forever — `grep -c` on
// this exact string returned 148 against a true count of 2 (PLAN §R.9).
const ArrivalRefusalMarker = "arrival refused at"

// recordArrivalRefusal is what every call site uses: count it, and say it at a
// level that is always on. Returns the refusal so the caller can propagate it.
func (e *Engine) recordArrivalRefusal(r *ArrivalRefusal) *ArrivalRefusal {
	if r == nil {
		return nil
	}
	noteArrivalRefusal(r)
	e.logFn("WARN: "+ArrivalRefusalMarker+" %s — %s (%s). The order will not place it. Expected count "+
		"is ZERO: the bin did not reach this order's destination AND this order does not own it, so "+
		"two orders were pointed at one bin.",
		r.Site, r.Reason(), r.Context())
	return r
}
