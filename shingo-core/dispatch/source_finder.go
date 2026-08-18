package dispatch

// source_finder.go — the one shared source-finding seam behind BOTH intake
// planning (planRetrieve / planRetrieveEmpty / planMove) and the fulfillment
// scanner's replay path. One pure seam both callers share.
//
// Why it exists: the scanner's inline finder had drifted from the intake
// planners — it dropped the dedicated-loader-pool and group-scoped-empty tiers
// and mis-classified the NGRP error path — so an order that queued at intake was
// re-sourced with different (wrong) scoping on replay, silently re-opening two
// previously-fixed bugs (loader oldest-first/partial-buffer consumption;
// supermarket/lane empty isolation). One finder, one tier cascade, both callers
// route through it. A forbidigo rule (.golangci.yml) forbids the raw
// db.FindSourceBinFIFO / db.FindEmptyCompatibleBin fallbacks outside this file so
// the drift cannot silently reappear.
//
// The finder is PURE: it finds a bin, it never claims, transitions, or writes an
// order. The caller owns the capacity gate, the claim (ClaimForDispatch), the
// MoveToSourcing transition, result assembly, and fleet dispatch. Disposition
// (queue vs reshuffle vs terminal) lives INSIDE the finder as a closed outcome
// enum — that is deliberate: the NGRP error-path drift lived in the caller's
// error handling, so the classifier must live in the seam.

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// SourceFinder is the shared source-finding engine.
type SourceFinder struct {
	db       FinderDB
	resolver NodeResolver // may be nil (tier 1 self-guards)
	dbg      func(string, ...any)
}

// NewSourceFinder constructs a SourceFinder. resolver may be nil — the NGRP tier
// self-guards on it, exactly as the intake planners do (s.resolver != nil).
func NewSourceFinder(db FinderDB, resolver NodeResolver, dbg func(string, ...any)) *SourceFinder {
	return &SourceFinder{db: db, resolver: resolver, dbg: dbg}
}

// isFullCarrier reports whether a carrier is FULL: at or above its payload's
// per-bin capacity.
//
// At-or-above, not equal. Overpacking is explicitly legal here — a nominally
// 1000-unit carrier that takes 1005 because the operator ran one more cycle
// before noticing — and an overpacked carrier is not less full than a nominal
// one. Written as equality, this rule would reject the fullest carriers on the
// floor.
//
// A capacity of zero means the catalog has no answer for this payload, and
// nothing can be known to be full against an unknown capacity. Refusing is the
// established answer to that in this system rather than guessing: the sizing
// arithmetic refuses a zero per-bin capacity by name for the same reason.
//
// This is the same shape as binsource.isFullOf, which ranks the produce-side
// loader pool. The two are deliberately NOT shared: that one treats an unknown
// capacity as full (its caller has already excluded zero-count carriers, so the
// case cannot arise there), and unifying them would quietly loosen a different
// gate. If they are ever merged, merge them on purpose.
func isFullCarrier(b *bins.Bin) bool {
	return b != nil && b.UOPCapacity > 0 && b.UOPRemaining >= b.UOPCapacity
}

// requiresFullCarrier reports whether this need is feeding a DRAIN WINDOW — a
// member node of a consume loader — and must therefore be given a full carrier.
//
// Derived from the destination rather than carried on the order, so no caller
// can forget it. That is the same reasoning that put the generation
// announcement inside the epoch bump: a rule every caller must remember is a
// rule that gets forgotten, and this one is forgotten silently.
//
// Deliberately narrow, and each exclusion removes a real counterexample:
//
//   - ROLE, not merely "is a loader window". The produce side's own pool picker
//     takes partials FIRST on purpose — a kept partial returned to a loader
//     should be consumed before a fresh full — so applying this to a produce
//     window would invert a written contract.
//   - FULL intent only. A retrieve_empty to the same window wants an empty
//     carrier, which is never full by definition.
//
// A complex order's DeliveryNode is its LAST step's node rather than any one
// pickup's destination, so a rule read off it would tag every pickup in the
// order. It cannot happen here and needs no guard of its own: every complex
// full-intent need either carries no DeliveryNode at all, or is node-local —
// and node-local makes the plant-wide tier unreachable by type. Both exclusions
// already existed. A third was written and removed as dead weight.
//
// Everywhere that is not a drain window keeps taking partials. A cell asking
// for material can work a half carrier down, and refusing it because no full
// exists would stop a line that had parts available to it.
func (f *SourceFinder) requiresFullCarrier(need SourceNeed) bool {
	if need.Intent != IntentFull || need.DeliveryNode == "" {
		return false
	}
	dest, err := f.db.GetNodeByDotName(need.DeliveryNode)
	if err != nil || dest == nil {
		return false
	}
	home, err := f.db.GetLoaderHomeByPositionNode(dest.ID)
	if err != nil || home == nil {
		return false // not a loader member node at all — an ordinary destination
	}
	l, err := f.db.GetLoader(home.LoaderID)
	if err != nil || l == nil {
		return false
	}
	return l.Role == loaders.RoleConsume
}

func (f *SourceFinder) debug(format string, args ...any) {
	if f.dbg != nil {
		f.dbg(format, args...)
	}
}

// explainEmptyPool records WHY a dedicated loader's pool produced no source, in
// the two halves the failure actually has.
//
//   - POOL — every loader member with its kind, its pinned payload and whether
//     InSourcePool admitted it. A member marked OUT never reaches the candidate
//     list at all, so a bin parked on it is invisible to sourcing while looking
//     entirely normal on every other surface. That asymmetry is unreadable from
//     the candidate list alone, which is why this half comes first.
//   - CANDS — every bin that DID reach the selector, with binsource.RejectReason.
//     Shares one implementation with the selector, so the two cannot disagree.
//
// One line, not N: this fires once per replenish tick and a chronically
// unavailable payload will emit it for hours (Springfield 2026-08-05 would have
// produced ~540). Compact and greppable beats structured-and-flooding.
//
// Routed through f.debug, matching this file. Verified reaching journald at
// Springfield, which is how the 08-05 trace was read at all — but that depends on
// `dispatch` being in logging.stderr_subsystems. A plant with it off gets no
// record, and promoting this to info (with per-tuple throttling, as
// threshold_monitor does for NEGATIVE COUNT) is the open call for review.
func (f *SourceFinder) explainEmptyPool(loaderID int64, anchor, payloadCode string, intent binsource.Intent,
	members []loaders.Home, slotBins []*bins.Bin, cands []binsource.Cand) {
	pool := make([]string, 0, len(members))
	for _, m := range members {
		name := fmt.Sprintf("node%d", m.PositionNodeID)
		if n, err := f.db.GetNode(m.PositionNodeID); err == nil && n != nil {
			name = n.Name
		}
		kind := m.Kind
		if kind == "" {
			kind = "home" // blank normalises to home — see UpsertHome
		}
		payload := m.PayloadCode
		if payload == "" {
			payload = "-"
		}
		admitted := "in"
		if !m.InSourcePool() {
			admitted = "OUT:unpinned-home"
		}
		pool = append(pool, fmt.Sprintf("%s[%s,%s,%s]", name, kind, payload, admitted))
	}

	want := binsource.Want{Payload: payloadCode, Intent: intent}
	rejects := make([]string, 0, len(cands))
	for i, c := range cands {
		at := ""
		if i < len(slotBins) && slotBins[i] != nil {
			at = "@" + slotBins[i].NodeName
		}
		rejects = append(rejects, fmt.Sprintf("bin%d%s:%s", c.BinID, at, binsource.RejectReason(c, want)))
	}
	if len(rejects) == 0 {
		rejects = append(rejects, "(no bins on any admitted slot)")
	}

	f.debug("finder: loader %d pool empty for %q anchor=%s intent=%v | POOL %s | CANDS %s",
		loaderID, payloadCode, anchor, intent, strings.Join(pool, " "), strings.Join(rejects, " "))
}

// FindSource runs the tier cascade for one order and one intent and returns a
// closed outcome. It never claims, transitions, or writes.
//
// Tier cascade (intake's order — reproduced faithfully per intent/shape so
// replay cannot drift from intake):
//
//  1. NGRP synthetic source  → resolver.Resolve, classified   (full intent)
//  2. dedicated-loader pool  → sourceFromDedicatedLoader        (Drain/Fill)
//  3. group/lane-scoped empty → FindEmptyCompatibleBinInGroup   (empty intent)
//  4. concrete-node candidate → ListBinsByNode                  (move-shaped)
//  5. plant-wide fallback    → FindSourceBinFIFO / FindEmptyCompatibleBin
//  6. post-find buried check → IsSlotAccessible                 (empty intent)
//
// A move sources node-locally (tiers 1,2,4) and never falls through to the
// plant-wide scan; a retrieve_empty scoped to a synthetic source queues rather
// than widening; an NGRP capacity/buried error queues (or reshuffles) scoped —
// none of these fall through to tier 5. Those "no fall-through" edges are the
// bugs the collapse fixes; keep them exact.
func (f *SourceFinder) FindSource(order *orders.Order, intent Intent) SourceResult {
	return f.FindSourceForNeed(SourceNeed{
		SourceNode:   order.SourceNode,
		PayloadCode:  order.PayloadCode,
		DeliveryNode: order.DeliveryNode,
		Intent:       intent,
		// Move-shaped: a node-local source relocates the bin AT a concrete
		// source node and never scans plant-wide. Keyed on the sourcing intent
		// data (SourceIntentLocal), stamped at intake, never on OrderType.
		NodeLocal: order.SourceIntent == SourceIntentLocal,
		// Both read straight off the order, never re-derived: the values are in
		// hand here, and a per-step lookup would put a database round trip inside
		// the tier cascade for something the caller was already holding.
		OriginID:    order.OriginID,
		ProcessNode: order.ProcessNode,
	})
}

// FindSourceForNeed runs the tier cascade for one NEED. Same cascade, same
// no-fall-through edges as FindSource -- which is now a two-line adapter over
// this -- but callable per-need, which is what lets a complex order's distinct
// source needs each get correctly-scoped resolution instead of inheriting the
// order's shape.
func (f *SourceFinder) FindSourceForNeed(need SourceNeed) SourceResult {
	payloadCode := need.PayloadCode
	intent := need.Intent
	// moveShaped keeps its historical name inside the cascade; it now means
	// exactly need.NodeLocal, and the tier-5 gate on !moveShaped is what makes
	// plant-wide widening unreachable by type for node-local needs.
	moveShaped := need.NodeLocal

	// Destination resolved once — excludeNodeID (prevent same-node retrieve) and
	// preferZone (zone-preferring empty fallback). Kills the four open-coded
	// copies (planning_service ×2, scanner ×2).
	var (
		excludeID  int64
		preferZone string
	)
	if need.DeliveryNode != "" {
		if dest, err := f.db.GetNodeByDotName(need.DeliveryNode); err == nil && dest != nil {
			excludeID = dest.ID
			preferZone = dest.Zone
		}
	}

	// Source node resolved once (tiers 1–4). A lookup miss leaves it nil; tiers
	// gate on nil and fall through to the plant-wide scan (retrieve) or queue
	// (move — no plant-wide fallback).
	var srcNode *nodes.Node
	if need.SourceNode != "" {
		srcNode, _ = f.db.GetNodeByDotName(need.SourceNode)
	}

	var (
		bin     *bins.Bin
		binNode *nodes.Node
	)

	// ── Tier 1: NGRP synthetic source (full intent only) ──────────────────
	// Empties never route through the retrieve resolver: ResolveRetrieve is
	// payload-match-required and rejects PayloadCode=="" bins, so an empty pull
	// on an NGRP source falls to the group-scoped empty tier (planRetrieveEmpty's
	// comment). Errors route through the SAME classifier intake uses — this is
	// where the A4 drift lived (the scanner checked only *StructuralError and
	// fell through to plant-wide FIFO on a capacity/buried error).
	if intent == IntentFull && srcNode != nil && srcNode.IsSynthetic &&
		srcNode.NodeTypeCode == protocol.NodeClassNGRP && f.resolver != nil {
		result, err := f.resolver.Resolve(srcNode, binresolver.ResolveModeRetrieve, payloadCode, nil, need.Asker)
		if err != nil {
			switch class, payload := classifyResolutionError(err); class {
			case ResolutionBuried:
				return SourceResult{Outcome: OutcomeReshuffle, Buried: payload.(*BuriedError)}
			case ResolutionStructural:
				return SourceResult{Outcome: OutcomeStructural, TermCode: codeStructural, Err: payload.(*StructuralError)}
			default:
				// Capacity / Transient / Fatal all QUEUE SCOPED — never fall
				// through to the plant-wide scan. (Intake queues here too.)
				f.debug("finder: no source in group %s for payload=%s, waiting", need.SourceNode, payloadCode)
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  CauseFinderGroupEmpty,
					QueueParams: QueueParams{Payload: payloadCode, Destination: need.SourceNode},
				}
			}
		}
		if result.Bin == nil {
			// Resolver returned a node but no concrete bin — queue and retry.
			// Matches planMove's defensive branch; safe for retrieve, where
			// ResolveRetrieve always carries a Bin on success.
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  CauseFinderGroupEmpty,
				QueueParams: QueueParams{Payload: payloadCode, Destination: need.SourceNode},
			}
		}
		bin = result.Bin
	}

	// ── Tier 2: dedicated-loader pool ─────────────────────────────────────
	// Drain (full) / Fill (empty). A payload-less move (full intent, blank
	// payload) skips the pool source — it is a direct relocation of the physical
	// bin at the position, handled by the concrete-node tier below. This mirrors
	// planMove:580 (`isLoaderPos && payloadCode != ""`); planRetrieve and
	// planRetrieveEmpty always carry a payload/intent that reaches here.
	if bin == nil && need.SourceNode != "" && (intent == IntentEmpty || payloadCode != "") {
		loaderIntent := binsource.Drain
		if intent == IntentEmpty {
			loaderIntent = binsource.Fill
		}
		chosen, node, isLoaderPos, lerr := f.sourceFromDedicatedLoader(need.SourceNode, payloadCode, loaderIntent)
		if lerr != nil {
			// WAIT, NOT FAIL, and this arm is the callee's own stated disposition
			// finally being honoured. Every error sourceFromDedicatedLoader returns
			// wraps a database read — the source node, the loader home, the loader,
			// its members, the bins across them — and each of those returns says so
			// in a comment: "Propagate so the order queues instead", "propagates →
			// the order queues". This site did the opposite, mapping all five to a
			// structural terminal, so a momentary read failure while sourcing from a
			// dedicated loader KILLED the order.
			//
			// A read that failed is not a fact about the plant. The releaser is the
			// ordinary one — the scanner re-runs on its event set and on the sweep,
			// and a read that failed once usually succeeds next time. The cause is in
			// the undetermined family (queue_cause.go) so a histogram keeps it apart
			// from an honest empty pool: this is Core declining to answer, not the
			// loader being out of material.
			f.debug("finder: loader source for %s unreadable (%v) — holding", need.SourceNode, lerr)
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  CauseLoaderSourceUnreadable,
				QueueParams: QueueParams{Payload: payloadCode, Destination: need.SourceNode},
			}
		}
		if isLoaderPos {
			if chosen == nil {
				// Loader position, no eligible bin of X in the pool — QUEUE; do
				// NOT fall through to the plant-wide scan (the no-fall-through
				// invariant). Scoping oldest-part-first / partial-buffer is the
				// whole point of the loader pool.
				f.debug("finder: loader pool for %s has no %q, waiting", need.SourceNode, payloadCode)
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  CauseFinderPoolEmpty,
					QueueParams: QueueParams{Payload: payloadCode, Destination: need.SourceNode},
				}
			}
			bin, binNode = chosen, node
		}
	}

	// WHICH CARRIER TYPE, when the destination loader has declared a mix. Empty
	// string means no mix declared — first come, first served, exactly as
	// before. Resolved once, used by both empty tiers, because a declared mix
	// has to be searched FOR: asking for any empty and rejecting the wrong type
	// would let one wrong-typed carrier mask every right one behind it.
	wantType := f.wantedBinType(need)

	// ── Tier 3: group/lane-scoped empty (empty intent, synthetic source) ──
	// Restricts empties to descendants of the SourceNode NGRP/LANE so a
	// multi-supermarket setup doesn't pull from the wrong supermarket or the
	// empty-tote return area. On no-empty it QUEUES scoped — no fall-through to
	// the plant-wide empty scan.
	if bin == nil && intent == IntentEmpty && srcNode != nil && srcNode.IsSynthetic &&
		(srcNode.NodeTypeCode == protocol.NodeClassNGRP || srcNode.NodeTypeCode == protocol.NodeClassLANE) {
		var groupBin *bins.Bin
		var gerr error
		if wantType != "" {
			groupBin, gerr = f.db.FindEmptyBinOfTypeInGroup(wantType, srcNode.ID, excludeID)
		} else {
			groupBin, gerr = f.db.FindEmptyCompatibleBinInGroup(payloadCode, srcNode.ID, excludeID)
		}
		if gerr != nil || groupBin == nil {
			cause := CauseFinderGroupEmpty
			if wantType != "" {
				// The loader asked for a specific type and the group has none.
				// It WAITS rather than taking another: a declared mix that is
				// abandoned when inconvenient is not a mix.
				cause = CauseFinderNoEmptyOfType
				f.debug("finder: no empty %s in group %s for %s, waiting", wantType, need.SourceNode, need.DeliveryNode)
			} else {
				f.debug("finder: no empty in group %s for payload=%s, waiting", need.SourceNode, payloadCode)
			}
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  cause,
				QueueParams: QueueParams{Kind: "empty", Payload: payloadCode, Destination: need.SourceNode},
			}
		}
		bin = groupBin
	}

	// ── Tier 4: concrete-node candidates (node-local needs) ───────────────
	// A node-local need sources the bin parked AT its concrete source node —
	// the first available candidate (BinUnavailableReason=="", which skips the
	// payload check for a payload-less move, exactly as claimFirstAvailable
	// does at intake). No plant-wide fallback: not-found queues, never widens.
	//
	// IntentEmpty extension (C(i), DORMANT until C(ii)): tier 4 also serves
	// node-local EMPTY needs, filtered to empty carriers exactly as the
	// allocator's step.Empty branch filters. No caller produces
	// NodeLocal+IntentEmpty today (SourceIntentForType pairs Move→Local with
	// full intent, and retrieve_empty is never Local — pinned by test), so
	// this changes nothing now; without it, C(ii)'s node-local empty needs
	// would fall past every tier into a permanent Wait and strand every
	// produce-side press-index empty refill.
	if bin == nil && moveShaped && srcNode != nil && (intent == IntentFull || intent == IntentEmpty) {
		candidates, _ := f.db.ListBinsByNode(srcNode.ID)
		claimPayload := payloadCode
		if intent == IntentEmpty {
			candidates = emptyBinsOnly(candidates)
			claimPayload = ""
		}
		for _, b := range candidates {
			if BinUnavailableReason(b, claimPayload) != "" {
				continue
			}
			bin, binNode = b, srcNode
			break
		}
		if bin == nil {
			params := QueueParams{Payload: payloadCode, Destination: need.SourceNode}
			if intent == IntentEmpty {
				params.Kind = "empty"
			}
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  CauseFinderNodeEmpty,
				QueueParams: params,
			}
		}
	}

	// ── Tier 5: plant-wide fallback (retrieve-shaped only) ────────────────
	// Move-shaped needs never reach here (tier 4 is terminal for a move).
	if bin == nil && !moveShaped {
		if intent == IntentFull {
			b, err := f.db.FindSourceBinFIFO(payloadCode, excludeID)
			if err != nil || b == nil {
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  CauseFinderPlantEmpty,
					QueueParams: QueueParams{Payload: payloadCode},
				}
			}
			bin = b
		} else {
			var b *bins.Bin
			var err error
			cause := CauseFinderPlantEmpty
			if wantType != "" {
				// EXCLUDING THE GROUP THIS ASK IS FILLING, when there is one.
				// Zero for every other need, and then this is FindEmptyBinOfType
				// by delegation. See maintainedGroupExclusion.
				b, err = f.db.FindEmptyBinOfTypeOutsideGroup(
					wantType, preferZone, f.maintainedGroupExclusion(need), excludeID)
				cause = CauseFinderNoEmptyOfType
			} else {
				b, err = f.db.FindEmptyCompatibleBin(payloadCode, preferZone, excludeID)
			}
			if err != nil || b == nil {
				return SourceResult{
					Outcome:     OutcomeWait,
					QueueCode:   protocol.QueueWaitingForMaterial,
					QueueCause:  cause,
					QueueParams: QueueParams{Kind: "empty", Payload: payloadCode},
				}
			}
			bin = b
		}
	}

	if bin == nil {
		params := QueueParams{Payload: payloadCode}
		cause := CauseFinderPlantEmpty
		if intent == IntentEmpty {
			params = QueueParams{Kind: "empty", Payload: payloadCode}
		}
		return SourceResult{
			Outcome:     OutcomeWait,
			QueueCode:   protocol.QueueWaitingForMaterial,
			QueueCause:  cause,
			QueueParams: params,
		}
	}

	// A DRAIN WINDOW TAKES FULL CARRIERS ONLY.
	//
	// Here rather than in a tier, because every tier funnels through this point
	// and the rule is about the DESTINATION, not about where the carrier was
	// found. An unloader configured with an inbound source resolves through the
	// group tier; one with none falls to the plant-wide scan; both arrive here.
	//
	// Waiting rather than refusing: the order queues for material the same way
	// an empty plant does, which is a visible state carrying a reason.
	//
	// KNOWN LIMIT, and it errs the safe way: this declines a partial the tiers
	// already chose, so a full sitting BEHIND a partial in a lane is not dug
	// for — the pull waits instead of reshuffling. Teaching the group resolver
	// to prefer fulls would need the fullness rule inside it and inside the
	// buried-bin lookups behind it, or it would dig to expose a carrier this
	// check then declines. Worth doing if a plant ever stacks partials in front
	// of fulls; not worth the surgery before that.
	if bin != nil && f.requiresFullCarrier(need) && !isFullCarrier(bin) {
		f.debug("finder: %s is a drain window and bin %d is a partial (%d of %d) — waiting for a full",
			need.DeliveryNode, bin.ID, bin.UOPRemaining, bin.UOPCapacity)
		return SourceResult{
			Outcome:     OutcomeWait,
			QueueCode:   protocol.QueueWaitingForMaterial,
			QueueCause:  CauseFinderNoFullCarrier,
			QueueParams: QueueParams{Payload: payloadCode, Destination: need.DeliveryNode},
		}
	}

	// Resolve the bin's node if a tier set `bin` without one (tiers 1 and 5).
	//
	// A bin with no node at all is a broken row and stays terminal. The LOOKUP
	// splits three ways for the reason in read_vs_missing.go: a node that is not
	// there is configuration a human fixes, and a read that did not answer is a
	// hiccup that must not kill the order. This site mapped both to a terminal.
	//
	// Releaser for the park: the finder's two callers are intake planning and the
	// scanner's replay, and the scanner re-runs on its whole event set plus the
	// sweep — so the retry is the loop that was going to run anyway.
	if binNode == nil {
		if bin.NodeID == nil {
			return SourceResult{Outcome: OutcomeStructural, TermCode: codeNode,
				Err: fmt.Errorf("source bin %d has no node", bin.ID)}
		}
		n, err := f.db.GetNode(*bin.NodeID)
		if readFailed(err) {
			f.debug("finder: could not read node %d for bin %d (%v) — holding", *bin.NodeID, bin.ID, err)
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueWaitingForMaterial,
				QueueCause:  CauseReadFailed,
				QueueParams: QueueParams{Payload: payloadCode},
			}
		}
		if err != nil || n == nil {
			return SourceResult{Outcome: OutcomeStructural, TermCode: codeInvalidNode,
				Err: fmt.Errorf("%s (source bin %d points at it)",
					configFailureID("node", *bin.NodeID), bin.ID)}
		}
		binNode = n
	}

	// ── Tier 6: post-find buried check (empty intent only) ────────────────
	// Preserves planRetrieveEmpty's last-resort reshuffle (:421-434): the empty
	// finder prefers lane-mouth empties, so a buried empty landing here means
	// every compatible empty is buried — dig this one out rather than dispatch a
	// robot to an unreachable slot. The full-retrieve path has no post-find
	// buried check (the NGRP resolver detects buried internally; a FIFO result
	// is not lane-buried).
	//
	// FAIL CLOSED, and note which way this used to lean. The three reads below
	// were guarded `err == nil`, `serr == nil`, `lerr == nil` — so every one of
	// them, on failure, fell through to OutcomeFound and dispatched a robot to a
	// slot nothing had successfully checked. An unreadable lane is a BLOCKED
	// lane: refusing to move is recoverable, and driving into a lane whose state
	// you could not read is not. The order waits for the next scan instead.
	//
	// storage_rearranging is the honest code for it — "waiting on storage to
	// become reachable" is exactly what an unanswered reachability question
	// leaves the order doing.
	if intent == IntentEmpty && bin.NodeID != nil {
		unreadable := func(what string, err error) SourceResult {
			f.debug("finder: %s for empty bin %d unreadable (%v) — treating the lane as blocked, not as clear", what, bin.ID, err)
			return SourceResult{
				Outcome:     OutcomeWait,
				QueueCode:   protocol.QueueStorageRearranging,
				QueueCause:  CauseFinderAccessibilityUnreadable,
				QueueParams: QueueParams{Kind: "empty", Payload: payloadCode},
			}
		}
		accessible, err := f.db.IsSlotAccessible(*bin.NodeID)
		if err != nil {
			return unreadable("accessibility", err)
		}
		if !accessible {
			slot, serr := f.db.GetNode(*bin.NodeID)
			if serr != nil {
				return unreadable("buried slot", serr)
			}
			if slot.ParentID != nil {
				lane, lerr := f.db.GetNode(*slot.ParentID)
				if lerr != nil {
					return unreadable("buried slot's lane", lerr)
				}
				if lane.NodeTypeCode == protocol.NodeClassLANE {
					f.debug("finder: empty bin %d buried at slot %s in lane %s, reshuffle", bin.ID, slot.Name, lane.Name)
					return SourceResult{Outcome: OutcomeReshuffle, Buried: &BuriedError{Bin: bin, Slot: slot, LaneID: lane.ID}}
				}
			}
			// Buried, but not in a LANE — there is no dig to plan. Unchanged and
			// deliberately so: this is the NGRP-direct single-file geometry
			// AuditLaneGeometry warns about at boot, not an error disposition.
		}
	}

	return SourceResult{Outcome: OutcomeFound, Bin: bin, Node: binNode}
}

// sourceFromDedicatedLoader is the dedicated-home-loader source path, moved onto
// the finder (from PlanningService) so the loader tier is compile-time
// unreachable from anywhere else. If sourceNodeName is a position on a
// dedicated_positions loader, it ranks the loader's WHOLE pool — its
// payload-pinned home positions AND its buffer slots (home_kind=buffer) — with
// binsource.Source and returns the chosen bin plus the node it sits at. That is
// what lets a cell bound to one home position consume a partial of X parked in
// the buffer: sourcing is over the loader's pool, not one slot. A shared_window
// (market) loader's windows live in the same table but are layout-gated out
// below (D5) so a window name never enters the flat-pool ranker.
//
//   - isLoaderPos=false → not a loader position; the caller falls back to its
//     normal (supermarket / global) sourcing, unchanged.
//   - isLoaderPos=true, bin=nil → a loader position but no eligible bin of X in
//     the pool; the caller QUEUES (must not fall through to the global scan).
//   - isLoaderPos=true, bin!=nil → the chosen bin and the node it is parked at.
func (f *SourceFinder) sourceFromDedicatedLoader(sourceNodeName, payloadCode string, intent binsource.Intent) (bin *bins.Bin, binNode *nodes.Node, isLoaderPos bool, err error) {
	srcNode, err := f.db.GetNodeByDotName(sourceNodeName)
	if err != nil {
		// A real lookup error must NOT be reported as "not a loader position" —
		// that would fall the caller through to the plant-wide scan (the very
		// bug this path fixes). Propagate so the order queues instead.
		return nil, nil, false, fmt.Errorf("resolve source node %s: %w", sourceNodeName, err)
	}
	if srcNode == nil {
		return nil, nil, false, nil // name doesn't resolve to a node → not a loader position
	}
	home, err := f.db.GetLoaderHomeByPositionNode(srcNode.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader for node %s: %w", sourceNodeName, err)
	}
	if home == nil {
		return nil, nil, false, nil // not a loader position at all
	}
	// Layout gate (D5 / M3): Source ranks dedicated_positions loaders only. A
	// shared_window loader ALSO stores its windows in bin_loader_homes, so
	// without this a window node name would be ranked as a flat pool, bypassing
	// the supermarket/seam semantics that govern a market loader. A non-dedicated
	// (or vanished/archived) loader → treat as "not a loader source" and fall through.
	loader, err := f.db.GetLoader(home.LoaderID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader %d for node %s: %w", home.LoaderID, sourceNodeName, err)
	}
	if loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return nil, nil, false, nil
	}

	// Pool = the loader's sourceable members: pinned home positions + buffer
	// slots (kept partials). An UNPINNED home (home_kind=home, no payload yet) is
	// inert and excluded (InSourcePool) so a stray bin on a half-configured
	// position is never sourced — the D4 buffer/unpinned-home disambiguation.
	members, err := f.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list loader %d members: %w", home.LoaderID, err)
	}
	// Collect the sourceable members' node ids, then read every bin across them
	// in ONE query (ListBinsByNodes) rather than N per-member reads on the hot
	// path. A read error now FAILS the source (propagates → the order queues)
	// instead of being swallowed per member — a swallowed read silently shrank
	// the pool and could mis-source.
	poolNodes := make([]int64, 0, len(members))
	for _, m := range members {
		if !m.InSourcePool() {
			continue // unpinned home — inert, not a buffer
		}
		poolNodes = append(poolNodes, m.PositionNodeID)
	}
	slotBins, err := f.db.ListBinsByNodes(poolNodes)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list bins for loader %d pool: %w", home.LoaderID, err)
	}
	cands := make([]binsource.Cand, 0, len(slotBins))
	byID := make(map[int64]*bins.Bin, len(slotBins))
	for _, b := range slotBins {
		cands = append(cands, candFromBin(b))
		byID[b.ID] = b
	}

	best, ok := binsource.Source(cands, binsource.Want{Payload: payloadCode, Intent: intent})
	if !ok {
		// The pool came up empty. Say WHY, in two halves, because "no eligible
		// bin" has two different shapes and the bare message cannot tell them
		// apart — Springfield 2026-08-05 burned a shift on exactly that ambiguity.
		//
		// MEMBERSHIP FIRST, and it is the half that matters. The InSourcePool
		// filter above runs BEFORE ListBinsByNodes, so a bin parked on an excluded
		// slot (an unpinned home: home_kind='home' with no payload) never becomes a
		// candidate at all. It is ABSENT from the rejection list, not rejected by
		// it. A candidate-only log would show zero rows for that case and teach
		// nobody anything.
		f.explainEmptyPool(home.LoaderID, sourceNodeName, payloadCode, intent, members, slotBins, cands)
		return nil, nil, true, nil // loader position, no eligible bin of X → caller queues
	}
	chosen := byID[best.BinID]
	if chosen == nil || chosen.NodeID == nil {
		// Defensive: Source only returns a BinID it was handed, and a pool bin
		// always carries the node it was read at — but never deref a nil node id.
		return nil, nil, true, fmt.Errorf("loader %d chose bin %d with no resolvable node", home.LoaderID, best.BinID)
	}
	node, err := f.db.GetNode(*chosen.NodeID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("resolve node for bin %d: %w", chosen.ID, err)
	}
	return chosen, node, true, nil
}
