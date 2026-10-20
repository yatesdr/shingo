// wiring_counter_delta.go — counter-delta UOP tracking, A/B paired-node
// cycling, and the lineside drain that consume ticks run before touching
// the node counter.
//
// IT DECIDES NOTHING ABOUT REPLENISHMENT, and that is the point of the
// shape. A tick is an accumulator: it moves the count, records the bin
// delta, and marks each drained lineside pile's level dirty. Whether a level has been breached, and whether to ask for
// material, is read off that count once a period by the demand
// reconciler's level sweep (demand_reconciler.go). The evaluation used to
// live here, on the edge, which meant it went blind at exactly the moment
// a cell ran dry and stopped ticking.
//
// Subscribed via wireEventHandlers (wiring.go) on EventCounterDelta;
// dispatches to consume / produce / fallback handlers via handleCounterDelta.

package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	storemsg "shingoedge/store/messaging"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// counterDeltaRows reads, once per tick, the three things the walk below needs
// for every node: the process, the claims of its styles, and the nodes' runtime
// rows.
//
// THE WALK'S READS ARE THREE STATEMENTS, NOT THREE PER NODE. The walk used to
// read each node's runtime row, then resolve its claim — which re-read the same
// process for every node — so a tick cost 3N+1 statements before it moved a
// single count, on a store pinned to one SQLite connection and on the hottest
// path Edge has: this fires on every PLC count.
//
// EACH FAILURE IS CARRIED, NOT RETURNED, so the skip ladder answers exactly what
// it answered before. A process that cannot be read left every node with no
// claim (requestedClaimAtNode swallowed the error and returned nil), so a nil
// process here produces an empty claim set and the same no_claim for every node.
// A claims read that fails does the same. A runtimes read that fails leaves
// every node absent from the map, which is the no_runtime the per-node read
// gave. In all three cases the walk still runs and the caller's unattributed
// line still names what happened.
//
// It returns bare values rather than an error precisely because there is nothing
// for a caller to do with one: the tick is already here and the ladder already
// has an honest answer for each absence.
func (e *Engine) counterDeltaRows(processID int64, nodes []processes.Node) (*processes.Process, *store.NodeClaimSet, map[int64]*processes.RuntimeState) {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		process = nil
	}
	claims, err := e.db.NodeClaimsForStyles(store.ClaimStyleIDs(process))
	if err != nil {
		claims = nil
	}
	runtimes, err := e.db.ProcessNodeRuntimes(nodeIDsOf(nodes))
	if err != nil {
		runtimes = nil
	}
	return process, claims, runtimes
}

// handleCounterDelta processes a production counter tick: a consume node's
// bound bin is drawn down, a produce node's is filled (up to its pack when the
// claim's strict_pack applies; strictPackApplies), and with nothing chargeable
// the tick is held on the seat for the next bin of its part.
//
// This walk validates the tick, resolves each node's claim against the tick's
// style, skips the A/B inactive side, and hands each node to its role's
// handler: handleConsumeTick (the lineside drain first), handleProduceTick, or
// the A/B safety net handleABFallthrough. The seat arithmetic every one of them
// shares is applyHoldAndReplay's.
func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.ProcessID == 0 || delta.StyleID == 0 || delta.Delta <= 0 {
		// A malformed tick is not a quiet no-op: the PLC path only
		// emits delta > 0 with both ids set (plc/manager.go), so
		// anything landing here means the count moved somewhere
		// upstream and arrived unusable.
		log.Printf("ERROR: counter delta malformed, dropped: process=%d style=%d delta=%d",
			delta.ProcessID, delta.StyleID, delta.Delta)
		return
	}
	// A jump or a reset is counted like any tick (close-out 2b: the PLC is
	// the truth). A reset's delta is newCount, the strokes since the counter
	// restarted, and it used to be dropped here.

	nodes, err := e.db.ListProcessNodesByProcess(delta.ProcessID)
	if err != nil {
		log.Printf("ERROR: counter delta dropped, cannot list nodes: process=%d style=%d delta=%d: %v",
			delta.ProcessID, delta.StyleID, delta.Delta, err)
		return
	}
	// Why this tick did or did not land. A counter tick that matches no
	// node used to fall off the end of this function in silence: the
	// consume path has the A/B fallthrough net below, produce has none,
	// so a produce tick against an unclaimed or mis-styled node moved a
	// PLC count that nothing in this system ever recorded or mentioned.
	// Diagnosing that took a database. It should take a log line.
	attr := tickAttribution{nodesWalked: len(nodes)}
	// A/B fallthrough tracking: if all paired consume nodes are inactive,
	// decrement the first one found as a safety net ("count to lineside storage").
	var pairedFallbackNode *processes.Node
	var pairedFallbackRuntime *processes.RuntimeState
	var pairedFallbackClaim *processes.NodeClaim
	pairedConsumeHandled := false

	process, claims, runtimes := e.counterDeltaRows(delta.ProcessID, nodes)

	for _, node := range nodes {
		runtime := runtimes[node.ID]
		if runtime == nil {
			attr.noRuntime++
			continue
		}

		// Look up active claim for this node
		claim := claims.Resolve(process, &node, store.ActiveStyleFirst)
		if claim == nil {
			attr.noClaim++
			continue
		}
		// Only process nodes with a claim matching this style
		if claim.StyleID != delta.StyleID {
			attr.styleMismatch++
			continue
		}

		// manual_swap nodes (loader/unloader) are forklift-managed
		// staging points, not production cells. They have no PLC tags
		// directly tied to their bin contents — the line's PLC counts
		// the parts. Skip cache decrement and Core-side bin delta
		// emission for these nodes so a forklift-loaded bin's manifest
		// doesn't drift from the operator-declared count.
		if claim.IsLoaderNode() {
			attr.manualSwap++
			continue
		}

		switch claim.Role {
		case protocol.ClaimRoleConsume:
			// A/B cycling: only decrement the active-pull side.
			// The inactive side holds staged material.
			if isInactivePairedNode(claim, runtime) {
				attr.inactiveConsume++
				// Remember first inactive paired node as fallback
				if pairedFallbackNode == nil {
					nodeCopy := node
					pairedFallbackNode = &nodeCopy
					pairedFallbackRuntime = runtime
					// Capture the claim that just passed the
					// claim.StyleID == delta.StyleID guard above so the
					// fallback emits against the tick's style, not a
					// re-derived one (R43-1).
					pairedFallbackClaim = claim
				}
				continue
			}
			if claim.PairedCoreNode != "" {
				pairedConsumeHandled = true
			}
			nodeCopy := node
			attr.consumeTicks++
			e.handleConsumeTick(&nodeCopy, runtime, claim, int(delta.Delta))

		case protocol.ClaimRoleProduce:
			// A/B cycling: only increment the active-pull side.
			// The inactive side holds its current production.
			if isInactivePairedNode(claim, runtime) {
				attr.inactiveProduce++
				continue
			}
			nodeCopy := node
			attr.produceTicks++
			e.handleProduceTick(&nodeCopy, runtime, claim, int(delta.Delta))

		default:
			attr.unknownRole++
		}
	}

	// A/B fallthrough: if no paired consume node was active but we found
	// an inactive paired node, decrement it as a safety net. This covers
	// the "count to lineside storage" case when neither A nor B is active.
	if !pairedConsumeHandled && pairedFallbackNode != nil && pairedFallbackRuntime != nil {
		attr.fallthroughTicks++
		e.handleABFallthrough(delta.ProcessID, pairedFallbackNode, pairedFallbackRuntime, pairedFallbackClaim, int(delta.Delta))
	}

	if !attr.landed() {
		log.Printf("ERROR: counter delta unattributed: process=%d style=%d delta=%d rp=%d %s — the cell counted and no node took it",
			delta.ProcessID, delta.StyleID, delta.Delta, delta.ReportingPointID, attr.skipReport())
		// Also to the debug log, where it lands on the diagnostics page
		// under the "engine" subsystem. The journal is the record; this
		// is the copy a technician can reach from the UI without SSH.
		e.debugFn("counter delta unattributed: process=%d style=%d delta=%d rp=%d %s",
			delta.ProcessID, delta.StyleID, delta.Delta, delta.ReportingPointID, attr.skipReport())
	}
}

// tickAttribution is the running account of one counter tick's walk over a
// process's nodes: how many were looked at, and the reason each one that
// declined the tick declined it. It exists so an unattributed tick can name
// its own cause in a log line instead of leaving a query against the runtime
// table as the only way to find out.
type tickAttribution struct {
	nodesWalked int

	// Skip reasons, in the order the loop tests them.
	noRuntime       int
	noClaim         int
	styleMismatch   int
	manualSwap      int
	inactiveConsume int
	inactiveProduce int
	unknownRole     int

	// Landings.
	consumeTicks     int
	produceTicks     int
	fallthroughTicks int
}

// landed reports whether any node — or the A/B safety net — took the tick.
func (a tickAttribution) landed() bool {
	return a.consumeTicks+a.produceTicks+a.fallthroughTicks > 0
}

// skipReport renders the walk for a log line: the node count followed by
// every non-zero skip reason. Zero-valued reasons are omitted so the line
// says what happened rather than what did not.
func (a tickAttribution) skipReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "nodes=%d", a.nodesWalked)
	for _, r := range []struct {
		name string
		n    int
	}{
		{"no_runtime", a.noRuntime},
		{"no_claim", a.noClaim},
		{"style_mismatch", a.styleMismatch},
		{string(protocol.SwapModeManualSwap), a.manualSwap},
		{"inactive_consume", a.inactiveConsume},
		{"inactive_produce", a.inactiveProduce},
		{"unknown_role", a.unknownRole},
	} {
		if r.n > 0 {
			fmt.Fprintf(&b, " %s=%d", r.name, r.n)
		}
	}
	if a.nodesWalked == 0 {
		b.WriteString(" (process has no nodes)")
	}
	return b.String()
}

// applyHoldAndReplay applies one UOP tick to a process node's cached counter
// using the hold-and-replay rule shared verbatim by the consume, produce, and
// A/B-fallthrough ticks (previously three near-identical copies).
//
// When a bin is bound (active_bin_id set) it charges the bin this tick PLUS any
// counts held while the slot was empty (pending_uop_delta), writes the new
// cached value, and clears the hold. When no bin is bound (the pickup→delivery
// gap) it holds `input` in pending so the count lands on the next bin instead
// of being lost or charged to a departed bin.
//
// sign is +1 for produce (fill) and -1 for consume / fallthrough (drain).
// Returns the seat the tick was applied to, whether a bin was charged, and
// binAttributed (the amount charged to the bin this tick, including any
// replayed pending; 0 when nothing was charged, so no bin delta is recorded).
// The caller records its bin delta against the returned seat.
//
// ── THE SEAT IS WRITTEN AS IT WAS READ ───────────────────────────────────
//
// The caller read the runtime row before it took countMu (counterDeltaRows,
// once per tick for every node). A release's count, a pickup's leave, an
// operator's count or a landing can be written under countMu in between. The
// tick's write therefore names the seat it read in its WHERE (carrier,
// generation, released-empty, hold: processes.SeatAsRead) and moves the count
// by a delta, never to a value computed from the old row. A seat that changed
// refuses the write; the tick re-reads it once, under the lock, and applies
// itself to the seat as it now is (charged to the new carrier, or held). The
// re-read runs only on a refusal: +0 statements on every other tick. partsOf
// gives the tick's parts for a seat, since the carrier's part and the
// released-empty flag are the seat's.
//
// ── THE HOLD HAS A PART (Lane H R2/R3, DECISIONS D12) ────────────────────
//
// The hold records the part of the ticks it holds (pending_uop_payload, in
// the hold's own statement) and replays only onto a bound carrier of that
// part. It used to be a bare integer that replayed onto whatever bound next,
// of any part: across a cutover the old part's held ticks landed on the new
// part's bin, stamped with the new part, so Core could not catch it (pin
// TestPinR2_HeldTicksAreNotReplayedOntoADifferentPart).
//
//   - A bound, chargeable carrier of the held part takes the tick plus the
//     hold, and the hold (and its part) clears. A carrier of another part
//     takes the tick alone; the hold is kept for its own part's carrier.
//   - A carrier released as empty (parts.notCharged, R3) is not charged: the
//     tick is held, exactly as if the seat were empty.
//   - With nothing chargeable, the tick is held when the hold is empty or
//     already of its part. When the seat holds ANOTHER part's ticks, that hold
//     is kept and this tick is neither held nor charged: its own pile has
//     already drained what it could (drainLinesideFirst runs first), and the
//     rest is logged once per episode, by part, and not charged (D12: no new
//     anomaly rows for an over-pull). The stranded monitor (EventUOPStranded)
//     watches a consume seat's hold as it grows.
//
// A hold written before the part existed reads "" and replays as it always
// did.
//
// ── AN UNBOUND PRODUCE SEAT HOLDS ONLY WHILE A CARRIER IS COMING ─────────
//
// With nothing chargeable, a produce tick asks one question first: is a live
// order placing a carrier at the node (carrierComing, store.DB.LiveOrderPlacing)?
// If one is, the rules above hold it. If none is, nothing will take the hold:
// the tick is logged and not held, and a standing pile at the seat is dropped
// with a hold_dropped row (dropPileAndLogTick). The strokes logged are counted
// in memory and sent as one hold_dropped (no_bin) row when the episode ends,
// in the transaction of the write that ends it. The stranded monitor's produce
// arm (sweepProduceHold) drops such a pile between ticks and at boot, and ends
// an episode whose seat has since bound.
//
// ── A STRICT PRODUCE SEAT STOPS AT THE PACK (strictPackApplies) ──────────
//
// When the tick's claim carries strict_pack (parts.pack > 0), a bound bin
// takes only what fits under the pack and the rest stays in the hold for the
// next bin of the part, which it replays onto as room allows. chargeStrict and
// splitStrict say how; nothing is ever written down, and a hold or a tick that
// fits nowhere is dropped with a hold_dropped row, never in silence.
func (e *Engine) applyHoldAndReplay(node *processes.Node, runtime *processes.RuntimeState, input, sign int,
	partsOf func(*processes.RuntimeState) tickParts) (rt *processes.RuntimeState, bound bool, binAttributed int) {
	rt = runtime
	for attempt := 0; ; attempt++ {
		var written bool
		bound, binAttributed, written = e.chargeOrHold(node, rt, input, sign, partsOf(rt))
		if written {
			return rt, bound, binAttributed
		}
		if attempt == 1 {
			log.Printf("counter delta: node %s: the seat changed twice under the count lock; %d not charged or held",
				node.CoreNodeName, input)
			return rt, false, 0
		}
		fresh, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || fresh == nil {
			log.Printf("counter delta: node %s: the seat changed since the tick read it and cannot be re-read (%v); %d not charged or held",
				node.CoreNodeName, err, input)
			return rt, false, 0
		}
		rt = fresh
	}
}

// chargeOrHold is one attempt of applyHoldAndReplay on the seat as rt holds
// it. written is false only when the seat refused the write (it changed since
// rt was read); a failed statement is logged and counts as written, as before.
func (e *Engine) chargeOrHold(node *processes.Node, rt *processes.RuntimeState, input, sign int, parts tickParts) (bound bool, binAttributed int, written bool) {
	seat := processes.SeatAsReadOf(rt)
	held := int(rt.PendingUOPDelta)
	if held == 0 {
		e.endUnheldEpisode(node.ID)
	}
	if parts.produce && rt.ActiveBinID != nil {
		e.endCarrierNote(node.ID)
	}
	if rt.ActiveBinID != nil && !parts.notCharged && parts.pack > 0 {
		return e.chargeStrict(node, rt, input, parts)
	}
	if rt.ActiveBinID != nil && !parts.notCharged {
		binAttributed = input
		replay := held != 0 && holdIsOf(rt.PendingUOPPayload, parts.carrier)
		if replay {
			binAttributed = input + held
		}
		noBin := e.noBinRows(node.ID)
		ok, err := e.writeSeatWithDrops(node, noBin, func(ex processes.DBTX) (bool, error) {
			return processes.ChargeSeatUOP(ex, node.ID, sign*binAttributed, replay, seat)
		})
		if err != nil {
			log.Printf("update UOP for node %d (replay %t): %v", node.ID, replay, err)
			return true, binAttributed, true
		}
		if !ok {
			return false, 0, false
		}
		e.endNoBinEpisode(node.ID, noBin)
		if replay {
			e.endUnheldEpisode(node.ID)
		}
		return true, binAttributed, true
	}
	if input == 0 {
		return false, 0, true
	}
	if parts.produce && !e.carrierComing(node) {
		return e.dropPileAndLogTick(node, rt, input, parts.tick)
	}
	if held == 0 || holdIsOf(rt.PendingUOPPayload, parts.tick) {
		noBin := e.noBinRows(node.ID)
		ok, err := e.writeSeatWithDrops(node, noBin, func(ex processes.DBTX) (bool, error) {
			return processes.HoldSeatUOP(ex, node.ID, input, parts.tick, seat)
		})
		if err != nil {
			log.Printf("hold pending UOP for node %d: %v", node.ID, err)
			return false, 0, true
		}
		if ok {
			e.endNoBinEpisode(node.ID, noBin)
		}
		return false, 0, ok
	}
	e.sendNoBinEpisode(node, holdDropByTick)
	e.noteUnheldTick(node, rt.PendingUOPPayload, parts.tick, held, input)
	return false, 0, true
}

// tickParts is what applyHoldAndReplay compares for one tick (D12).
type tickParts struct {
	// tick is the part the tick is of: the matched claim's part. A hold
	// written for this tick records it.
	tick string
	// carrier is the part of the bound carrier, as the tick's bin delta is
	// stamped (binAtNode for a draw, the claim's part for a fill). A hold
	// replays onto the carrier only when it is of this part.
	carrier string
	// notCharged: the bound carrier was released as empty and takes no tick
	// (R3); the tick is held as if the seat were empty.
	notCharged bool
	// pack is the bound bin's cap when the strict arm applies to the seat as
	// read (strictPackFor), else 0. Carried per attempt like the parts, so a
	// retry after a refused write decides it again on the fresh row.
	pack int
	// produce: the tick is a produce claim's. With nothing chargeable it is
	// held only while a carrier is coming (carrierComing, dropPileAndLogTick).
	produce bool
}

// holdIsOf reports whether a hold recorded as heldPart may take, or replay
// onto, part. "" is a hold from before the part was recorded, which goes
// anywhere, as every hold did then.
func holdIsOf(heldPart, part string) bool {
	return heldPart == "" || heldPart == part
}

// noteUnheldTick logs, once per episode, a tick neither charged nor held
// because the seat holds another part's ticks (D12). The episode is the pair
// of parts at this node; it ends when the hold clears. Caller holds countMu.
func (e *Engine) noteUnheldTick(node *processes.Node, heldPart, tickPart string, held, input int) {
	key := heldPart + "|" + tickPart
	if e.unheldTickLogged == nil {
		e.unheldTickLogged = map[int64]string{}
	}
	if e.unheldTickLogged[node.ID] == key {
		return
	}
	e.unheldTickLogged[node.ID] = key
	log.Printf("counter delta: node %s: %d of part %s not charged -- no carrier takes it and the seat holds %d of part %s "+
		"for that part's next carrier, which keeps its hold (D12); logged once until the hold clears",
		node.CoreNodeName, input, tickPart, held, heldPart)
}

// endUnheldEpisode forgets the once-per-episode note for a node whose hold has
// cleared. Caller holds countMu.
func (e *Engine) endUnheldEpisode(nodeID int64) {
	if e.unheldTickLogged != nil {
		delete(e.unheldTickLogged, nodeID)
	}
}

// handleConsumeTick applies a delta to one active-pull consume node:
// drain lineside first, then decrement node UOP. Caller is responsible
// for the A/B inactive-pair check; this is invoked only on the active
// side. The level that count feeds is evaluated by the demand
// reconciler's sweep, not here.
//
// Post-May-4 (commit 6d226d1) Edge is authoritative for the count of
// any bin physically at one of its nodes. The local count write
// write is the durable truth for at-node bins, not a write-through
// cache — there is no reconciler healing back from Core. Core mirrors
// the bin via the deltas published below and the piles via their levels.
// If a bin delta is rejected at Core (e.g., payload_code mismatch), Core
// records the refusal on its ledger; nothing on the Edge heals it.
//
// Per SME lock (open-items.md §"Process semantics"): bins can go
// negative. A real bin nominally rated 1000 might overpack to 1005
// (operator runs an extra cycle before noticing); the next bin
// underpacks to 995. Over time these wash out at the inventory
// aggregate level. Tracking the actual count — including signed
// values — is the whole point of bin-as-truth. The runtime cache
// must mirror this; clamping at zero would force a permanent drift
// against Core (which doesn't clamp) and noisy reconciliation logs
// as the heal/clamp/heal/clamp loop ping-pongs forever.
func (e *Engine) handleConsumeTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	// The drain, the count write and the record are one change to the seat;
	// the lineside report must see all of it or none (countMu).
	e.countMu.Lock()
	defer e.countMu.Unlock()
	// Lineside first: drain the active pile of this node's primary part
	// before touching the node counter. The pile is parts the operator
	// pulled to lineside at the release, which physically leave the station
	// before the new bin is tapped, until the cutover strands it. Remainder
	// flows to the bin counter.
	drains, binRemainder := e.drainLinesideFirst(node.ID, claim, delta)

	// Hold-and-replay. The count follows the bin physically at the slot
	// (active_bin_id). When a bin is bound we apply this tick PLUS any
	// counts held while the slot was empty (pending_uop_delta), then clear
	// the hold. When no bin is bound (the pickup→delivery gap), we hold the
	// bin portion so it lands on the next bin instead of being lost or
	// charged to a departed bin. The lineside drain is marked every tick
	// regardless — parts leaving the rack are independent of which bin is
	// at the slot.
	//
	// A carrier released as empty is not charged: what the pile did not take
	// is held for the next carrier of the part (Lane H R3, DECISIONS D12; pin
	// TestPinR3_ReleasedEmptyIsNotChargedThenTheNextBinOfThePartTakesTheHold).
	// A count release leaves the flag clear and keeps charging until the
	// lift (R1).
	runtime, _, binAttributed := e.applyHoldAndReplay(node, runtime, binRemainder, -1, func(rt *processes.RuntimeState) tickParts {
		_, carrierPart, _ := e.binAtNode(rt, claim)
		return tickParts{tick: claim.PayloadCode, carrier: carrierPart, notCharged: rt.ReleasedEmpty}
	})

	// emitConsumeTickDeltas marks the drained piles always, and records a
	// bin delta for binAttributed — which binAtNode skips when no bin
	// is bound (binID 0), so the held portion isn't double-emitted; it
	// ships on the bound tick that replays it.
	e.emitConsumeTickDeltas(node, runtime, claim, drains, binAttributed)
}

// handleProduceTick applies a delta to one active-pull produce node:
// increment node UOP. Caller is responsible for the A/B inactive-pair
// check. Asking for the next bin is the demand reconciler's sweep, not this
// function's. A press does not stop at the pack: it keeps making parts until
// someone swaps the bin, and those ticks arrive here. Without strict_pack they
// count on past the pack; with it (strictPackApplies) the bin stops at the pack
// and the rest is held for the next bin of the part.
//
// Emits BinUOPDelta(produce_tick) for what the bin was charged: the tick plus
// any replayed hold, or under strict_pack only the part of those that fit
// (0 charged, nothing emitted). No pile is touched — produce nodes don't drain
// lineside; they fill bins directly via the PLC.
func (e *Engine) handleProduceTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	// Hold-and-replay, mirror of consume. Increment the bin physically at
	// the slot; when none is bound (finalize→new-empty gap) hold the
	// produced parts in pending and replay onto the next empty bin when it
	// binds.
	//
	// The count write and the record are one change to the seat; the lineside
	// report must see both or neither (countMu).
	e.countMu.Lock()
	// A fill's carrier is the part it is being filled with, the claim's
	// (producedPayload): an empty arrives known-empty, so its resident part
	// would never match the hold.
	part, pack := producedPayload(claim), strictPackFor(claim)
	runtime, _, binAttributed := e.applyHoldAndReplay(node, runtime, delta, +1, func(*processes.RuntimeState) tickParts {
		return tickParts{tick: part, carrier: part, pack: pack, produce: true}
	})

	if e.inventoryDelta != nil && binAttributed > 0 {
		binID, _, epoch := e.binAtNode(runtime, claim)
		payload := producedPayload(claim)
		_ = e.inventoryDelta.Produced(uop.TickEvent{
			NodeID:       node.ID,
			BinID:        binID,
			PayloadCode:  payload,
			BinEpoch:     epoch,
			BinRemainder: binAttributed, // what the bin was charged: this tick + any replayed hold, capped by the pack
		})
	}
	e.countMu.Unlock()
}

// producedPayload is the part a produce tick is stamped with: the part of the
// claim the tick was matched to (handleCounterDelta's claim.StyleID ==
// delta.StyleID guard), which is the part the press just made.
//
// NOT binAtNode's resident payload. That is the right answer for a consume
// tick, which draws down what the carrier already holds. A produce node is
// delivered an EMPTY, which Core reports as a known-empty carrier, so the
// resident payload is "" for the whole fill and Core's first-delta bind
// (uop/applier.go), which needs a part on the wire, never labels the bin.
// That was the state from 69df3870 (2026-09-06) until this: HK press bins
// labelled only at release, the SPR FG bin never.
func producedPayload(claim *processes.NodeClaim) string {
	if claim == nil {
		return ""
	}
	return claim.PayloadCode
}

// ── Strict pack ──────────────────────────────────────────────────────────

// strictPackApplies is the gate for the strict arm: the tick's matched claim
// carries strict_pack, is a produce claim, and has a pack (UOPCapacity,
// resolved from the catalog on every read; the claim's uop_capacity column is
// dead and a produce bin's resident payload is "" for the whole fill, so
// neither is read). The seat's active_claim_id is not read: it is NULL on a
// seat never through a changeover, a Clear or a Record Count, which would
// leave the arm off at exactly the seats it is for. A bin still bound under an
// outgoing claim after a cutover is the case the pointer could tell apart; the
// tick matches the active style's claim and the cutover clears the departing
// part's hold, so that bin is capped at the incoming pack only for the ticks
// that land on it before it leaves. The mode is not read: the cap is per seat.
// A strict claim with no pack is logged where the claim is read
// (processes.logStrictPackWithoutPack), so the arm is never off in silence.
func strictPackApplies(claim *processes.NodeClaim) bool {
	return claim != nil && claim.StrictPack && claim.Role == protocol.ClaimRoleProduce && claim.UOPCapacity > 0
}

// strictPackFor is the pack the strict arm caps the seat's bound bin at, or 0
// when strictPackApplies says it does not apply.
func strictPackFor(claim *processes.NodeClaim) int {
	if !strictPackApplies(claim) {
		return 0
	}
	return claim.UOPCapacity
}

// holdDropByTick is the actor a tick-time hold_dropped row names.
const holdDropByTick = "edge:tick"

// strictSplit is one strict tick's division of the seat: what is offered to
// the bin (avail), what the bin takes (charge), the hold the seat keeps (hold
// of holdPart), and what fits nowhere (drops, one per reason).
type strictSplit struct {
	avail, charge, hold int
	holdPart            string
	drops               []protocol.HoldDropped
}

// splitStrict divides one tick on a bound strict seat, from the seat as read:
// count is the bin's count, held / heldPart the hold, part the tick's part.
// Pure. The bin takes MAX(0, MIN(avail, pack - count)), so a count already at
// or past the pack (a person's, a bin bound before the setting, a pack lowered
// mid-bin) takes 0 and is never written down.
//
//   - A hold of ANOTHER part (held != 0, !holdIsOf) is not offered and not
//     relabelled: the tick charges to room, and what of the tick does not fit
//     is dropped (foreign_hold). The hold stays for its own part's bin.
//   - A hold of the part LARGER THAN THE PACK is the backstop: it is offered
//     nothing and dropped whole (backstop), and the tick charges to room alone.
//   - Otherwise the hold is offered with the tick, and what the bin does not
//     take stays held, its part the tick's. A partial replay keeps its
//     remainder; the hold clears only when the bin took all of it.
//
// In the last two cases the hold left behind is capped at one pack (hold_cap):
// a bin at its pack with no carrier coming would otherwise grow it without
// bound.
func splitStrict(count, held int, heldPart, part string, input, pack int) strictSplit {
	room := max(0, pack-count)
	if held != 0 && !holdIsOf(heldPart, part) {
		s := strictSplit{avail: input, charge: min(input, room), hold: held, holdPart: heldPart}
		if over := input - s.charge; over > 0 {
			s.drops = append(s.drops, protocol.HoldDropped{PayloadCode: part, Qty: int64(over), Reason: protocol.HoldDropForeignHold})
		}
		return s
	}
	s := strictSplit{avail: input + held}
	if held > pack {
		s.avail = input
		s.drops = append(s.drops, protocol.HoldDropped{PayloadCode: heldPart, Qty: int64(held), Reason: protocol.HoldDropBackstop})
	}
	s.charge = min(s.avail, room)
	s.hold = s.avail - s.charge
	if s.hold > pack {
		s.drops = append(s.drops, protocol.HoldDropped{PayloadCode: part, Qty: int64(s.hold - pack), Reason: protocol.HoldDropHoldCap})
		s.hold = pack
	}
	if s.hold > 0 {
		s.holdPart = part
	}
	return s
}

// chargeStrict is chargeOrHold's arm for a bound bin under strict_pack: one
// guarded statement (processes.ChargeSeatUOPStrict, the count pinned with the
// seat) and, only when something fits nowhere, its hold_dropped rows in the
// same transaction (writeSeatWithDrops). It returns the CHARGED amount as
// binAttributed, so handleProduceTick emits what the bin took and the held
// ticks are emitted once, on the bin that takes them. A refused write (the
// seat or its count changed) returns written false and applyHoldAndReplay
// re-reads once. A failed write is logged and emits nothing: the count did
// not move.
func (e *Engine) chargeStrict(node *processes.Node, rt *processes.RuntimeState, input int, parts tickParts) (bound bool, binAttributed int, written bool) {
	s := splitStrict(rt.RemainingUOPCached, int(rt.PendingUOPDelta), rt.PendingUOPPayload, parts.tick, input, parts.pack)
	seat := processes.SeatAsReadOf(rt)
	noBin := e.noBinRows(node.ID)
	ok, err := e.writeSeatWithDrops(node, append(s.drops, noBin...), func(ex processes.DBTX) (bool, error) {
		return processes.ChargeSeatUOPStrict(ex, node.ID, s.avail, parts.pack, s.hold, s.holdPart, seat)
	})
	if err != nil {
		log.Printf("counter delta: node %s: strict charge of %d (pack %d): %v", node.CoreNodeName, input, parts.pack, err)
		return true, 0, true
	}
	if !ok {
		return false, 0, false
	}
	e.endNoBinEpisode(node.ID, noBin)
	if s.hold == 0 {
		e.endUnheldEpisode(node.ID)
	}
	return true, s.charge, true
}

// errSeatRefused rolls back writeSeatWithDrops's transaction when the seat
// refused the write: no seat change, so no message.
var errSeatRefused = errors.New("seat changed since it was read")

// writeSeatWithDrops writes one change to a produce seat and, when it lands,
// one hold_dropped message per drop (protocol.SubjectHoldDropped, built by
// orders.Manager.HoldDroppedEnvelope), in ONE SQLite transaction: a crash
// leaves both or neither. write is the seat's guarded statement, run on the
// transaction; it reports whether the seat was still as read, and a refusal
// writes no message. Each drop is stamped with the node, the time and, when it
// names none, the tick as its actor, and logged once after the commit.
//
// With no drop it is write alone on the store, outside any transaction: the
// one statement a tick always ran.
//
// The drops' one writer: the tick's and the sweep's (sweepProduceHold) go
// through it, the flip carry (carryFlipBucket) through writeSeatsInTx, and
// the cutover clear (cutoverClears), which rides the strand's transaction,
// through its halves (holdDrops, writeSeatAndDropsIn on that transaction,
// dropsQueued after the commit). Caller holds countMu.
func (e *Engine) writeSeatWithDrops(node *processes.Node, drops []protocol.HoldDropped, write func(processes.DBTX) (bool, error)) (bool, error) {
	if len(drops) == 0 {
		return write(e.db.DB)
	}
	return e.writeSeatsInTx(node, drops, write)
}

// writeSeatsInTx is writeSeatWithDrops always in one transaction, drops or
// none: for a write of more than one guarded statement (the flip carry's two
// seats), which must land together or not at all. write reports false when
// any of its seats refused, and the transaction rolls back. Caller holds
// countMu.
func (e *Engine) writeSeatsInTx(node *processes.Node, drops []protocol.HoldDropped, write func(processes.DBTX) (bool, error)) (bool, error) {
	q, err := e.holdDrops(node, drops)
	if err != nil {
		return false, err
	}
	err = e.db.InTx(func(tx *sql.Tx) error {
		ok, err := writeSeatAndDropsIn(tx, write, q)
		if err == nil && !ok {
			err = errSeatRefused
		}
		return err
	})
	if errors.Is(err, errSeatRefused) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dropsQueued(q)
	return true, nil
}

// queuedDrops is one seat's hold_dropped rows, built and ready to enqueue.
type queuedDrops struct {
	node    string
	drops   []protocol.HoldDropped
	envs    [][]byte
	msgType string
}

// holdDrops stamps each drop with the node, the time and, when it names none,
// the tick as its actor, and builds its envelope
// (orders.Manager.HoldDroppedEnvelope). No statement.
func (e *Engine) holdDrops(node *processes.Node, drops []protocol.HoldDropped) (queuedDrops, error) {
	q := queuedDrops{node: node.CoreNodeName, drops: drops, envs: make([][]byte, len(drops))}
	now := time.Now().UTC()
	for i := range drops {
		drops[i].Node = node.CoreNodeName
		drops[i].OccurredAt = now
		if drops[i].CalledBy == "" {
			drops[i].CalledBy = holdDropByTick
		}
		env, mt, err := e.orderMgr.HoldDroppedEnvelope(drops[i])
		if err != nil {
			return q, err
		}
		q.envs[i], q.msgType = env, mt
	}
	return q, nil
}

// writeSeatAndDropsIn runs the seat's guarded write on tx and, when it lands,
// enqueues the drops' rows on the same tx. A refusal (the seat changed since it
// was read) wrote nothing and enqueues nothing: false, nil.
func writeSeatAndDropsIn(tx *sql.Tx, write func(processes.DBTX) (bool, error), q queuedDrops) (bool, error) {
	ok, err := write(tx)
	if err != nil || !ok {
		return false, err
	}
	for _, env := range q.envs {
		if _, err := store.EnqueueOutboxIn(tx, env, q.msgType); err != nil {
			return false, fmt.Errorf("queue hold_dropped: %w", err)
		}
	}
	return true, nil
}

// dropsQueued wakes the drainer and logs each drop, after the commit.
func dropsQueued(q queuedDrops) {
	if len(q.drops) == 0 {
		return
	}
	storemsg.NotifyEnqueued()
	for _, d := range q.drops {
		log.Printf("produce hold: node %s: %d of part %q dropped from the hold (%s, by %s); hold_dropped queued for Core",
			q.node, d.Qty, d.PayloadCode, d.Reason, d.CalledBy)
	}
}

// handleABFallthrough is the safety-net path when no active-pull consume node
// existed for a process tick but at least one inactive paired consume node
// was visible. It decrements the first such fallback node so the count
// flows to lineside storage instead of being dropped on the floor.
//
// Phase 1: emits BinUOPDelta(ab_fallthrough, ...) against the
// inactive-paired node's bin and marks its drained active piles. The plan
// (B5 fix) singles this path out — it's the case where neither
// operator action nor active-pull state surfaces a flush trigger, so
// the periodic flush is the only signal that captures the change.
func (e *Engine) handleABFallthrough(processID int64, node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	log.Printf("A/B fallthrough: no active-pull node for process %d, decrementing fallback node %s",
		processID, node.Name)
	e.countMu.Lock() // one change to the seat, as in handleConsumeTick
	defer e.countMu.Unlock()

	// claim is the one captured in handleCounterDelta's loop, which
	// already passed the claim.StyleID == delta.StyleID guard. Do NOT
	// re-derive via requestedClaimAtNode here: it prefers the process
	// ActiveStyleID claim, which during a changeover can differ from the
	// tick's style and mis-attribute the lineside drain and bin delta
	// (R43-1).
	var drains map[string]int
	binRemainder := delta
	if claim != nil {
		drains, binRemainder = e.drainLinesideFirst(node.ID, claim, delta)
	}
	partsOf := func(rt *processes.RuntimeState) tickParts {
		if claim == nil {
			return tickParts{}
		}
		_, carrierPart, _ := e.binAtNode(rt, claim)
		return tickParts{tick: claim.PayloadCode, carrier: carrierPart}
	}

	// Hold-and-replay, mirror of handleConsumeTick: decrement the bound
	// bin (this tick + any held pending of its part), or hold when no bin is
	// bound. The released-empty flag is the consume tick's rule (D12), not
	// this safety net's.
	runtime, _, binAttributed := e.applyHoldAndReplay(node, runtime, binRemainder, -1, partsOf)

	if claim != nil {
		e.emitFallthroughDeltas(node, runtime, claim, drains, binAttributed)
	}
}

// emitConsumeTickDeltas records the pile drains and the bin delta for one
// consume tick. Resolves the bin context via binAtNode, then delegates
// the actual emission to uop.Mutator.Consumed which locks in the
// reason taxonomy (a pile drain + consume_tick).
func (e *Engine) emitConsumeTickDeltas(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]int, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	binID, payload, epoch := e.binAtNode(runtime, claim)
	_ = e.inventoryDelta.Consumed(uop.TickEvent{
		NodeID:       node.ID,
		CoreNodeName: node.CoreNodeName,
		BinID:        binID,
		PayloadCode:  payload,
		BinEpoch:     epoch,
		Drains:       drains,
		BinRemainder: binRemainder,
	})
}

// emitFallthroughDeltas mirrors emitConsumeTickDeltas but routes through
// uop.Mutator.Fallthrough which tags the bin delta with ab_fallthrough
// while marking the pile drains the same way (the pile physically
// drained regardless of which side of the A/B pair the count
// attributed to).
func (e *Engine) emitFallthroughDeltas(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]int, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	binID, payload, epoch := e.binAtNode(runtime, claim)
	_ = e.inventoryDelta.Fallthrough(uop.TickEvent{
		NodeID:       node.ID,
		CoreNodeName: node.CoreNodeName,
		BinID:        binID,
		PayloadCode:  payload,
		BinEpoch:     epoch,
		Drains:       drains,
		BinRemainder: binRemainder,
	})
}

// binAtNode resolves the bin attribution for an emitted delta:
// (binID, payloadCode, epoch). epoch is the bin's load-lifecycle
// epoch — used to stamp the outgoing BinUOPDelta so Core's
// epoch-aware dedup accepts it. Returns (0, "", 0) when no bin is
// at the slot (gap window with active_bin_id nil); caller skips
// bin delta emission in that case.
//
// runtime.ActiveBinID is the canonical "bin physically at this slot"
// pointer. Set on delivery completion (when the bin arrives at the
// node), cleared on pickup (when the bin physically leaves). Edge
// owns this pointer; no order walk is needed. PLC ticks attribute to
// whatever bin is at the slot regardless of which order delivered it
// — covering the gap between order completion and the next order's
// delivery, manual loads, and any other path where a bin is present
// without a tracking order.
//
// payload is what Core validates the wire envelope against, so it has to be
// what the BIN is, not what the cell was asked for.
//
// IT USED TO BE THE CLAIM'S, AND THAT IS THE MISMATCH ITSELF. The claim is the
// requested identity: it follows the process's active style, so a changeover
// over a node that still holds the outgoing carrier moves it while the bin
// stays put. Every tick then arrived at Core stamped with the incoming part
// against a bin holding the outgoing one, Core refused the deltas as
// payload_mismatch_dropped, and the count went nowhere — which is the 36-minute
// warning on the SMN_029 carrier, logged from the wrong end of the wire.
//
// The resident payload is Core's own answer about this bin, sent on the
// delivery envelope. Falls back to the claim when nothing has been recorded —
// an older Core, or a node not delivered to since the upgrade — which is
// exactly today's behaviour and the same value it would have sent anyway.
//
// THE FALLBACK ASKS THE KNOWN BIT, NOT THE EMPTY STRING. A carrier known to be
// carrying nothing spells itself "" and so does a carrier nobody could read,
// and only the second one wants the claim. Reading emptiness as absence puts
// the claim's part number back on a carrier the operator has just cleared —
// the cross product this pair was split to end. Rows written before the known
// bit existed read as not-established and take the fallback, which is the
// behaviour they already had.
func (e *Engine) binAtNode(runtime *processes.RuntimeState, claim *processes.NodeClaim) (int64, string, int64) {
	if runtime == nil || runtime.ActiveBinID == nil {
		return 0, "", 0
	}
	payload := string(runtime.LinesidePayloadCode)
	if !runtime.LinesidePayloadKnown {
		payload = claim.PayloadCode
	}
	return *runtime.ActiveBinID, payload, runtime.ActiveBinEpoch
}

// drainLinesideFirst decrements the node's active lineside pile(s) for the
// claim's parts and returns:
//
//   - drains: per-part qty actually drained from each pile. Empty map (not
//     nil-valued — the caller iterates safely) means no pile drained.
//   - binRemainder: the units that should flow to the node counter
//     after the primary-part drain. The primary's drain reduces
//     binRemainder; secondary drains do not (they keep the UI
//     honest but the node counter is one unit per assembly).
//
// The caller hands the drains to the mutator, which marks each pile's level
// dirty with its drain, and the binRemainder becomes a BinUOPDelta
// (consume_tick).
//
// Only ACTIVE piles drain; a pile stranded at a cutover never does. The tick's
// style is not consulted: a pile captured at the release keeps draining
// through the changeover window until the cutover strands it.
//
// Multi-part claims (claims with more than one entry in
// AllowedPayloads) drain each part by up to delta independently. The
// rationale: the node counter is a single integer (one UOP = one
// assembly), and staging/reorder thresholds key off that value;
// secondary part piles drain so the UI stays honest, even though
// their draining doesn't affect the node counter arithmetic. If a
// plant ever ships a claim where secondary parts can deplete
// independently (e.g. consumables), revisit this.
func (e *Engine) drainLinesideFirst(nodeID int64, claim *processes.NodeClaim, delta int) (drains map[string]int, binRemainder int) {
	drains = make(map[string]int)
	binRemainder = delta
	if delta <= 0 || claim == nil {
		return drains, binRemainder
	}

	// Primary part controls the node-counter math.
	if primary := claim.PayloadCode; primary != "" {
		drained, err := e.db.DrainLinesideBucket(nodeID, primary, delta)
		if err != nil {
			log.Printf("lineside: drain primary part %q on node %d: %v", primary, nodeID, err)
		} else {
			if drained > 0 {
				drains[primary] = drained
			}
			binRemainder = delta - drained
		}
	}

	// Secondary parts: drain independently for UI honesty. Skip if they
	// match the primary (avoids a double-drain when AllowedPayloads
	// includes the primary).
	for _, part := range claim.AllowedPayloads() {
		if part == "" || part == claim.PayloadCode {
			continue
		}
		drained, err := e.db.DrainLinesideBucket(nodeID, part, delta)
		if err != nil {
			log.Printf("lineside: drain secondary part %q on node %d: %v", part, nodeID, err)
			continue
		}
		if drained > 0 {
			drains[part] = drained
		}
	}
	return drains, binRemainder
}
