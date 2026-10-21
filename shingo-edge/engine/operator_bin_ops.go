package engine

import (
	"errors"
	"fmt"
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// operatorRequestOrigin opens — or joins — the cell demand episode for an
// operator asking for material at a node, and returns the origin to stamp on
// the order that request creates.
//
// THIS PATH HAD NO EPISODE AT ALL. The button an operator presses at a node to
// ask for parts is the plainest demand the plant has, and it created its order
// without opening anything, so every one of those orders reached Core with no
// origin and was classified — correctly, given what it carried — as an orphan.
// The effect was that ordinary cell demand did not appear on the episode
// surface: not mislabelled, absent. The only episodes anyone ever saw were
// changeovers, which are long-lived, and thresholds.
//
// openCellEpisode was built for this. Its own doc names "an operator push" as
// the common case for JOINING, and EpisodeTriggerOperator is described as "the
// HMI button" — the machinery was written and never wired to the door it
// describes.
//
// Attribution never blocks transport. An episode that cannot be opened logs and
// returns an empty origin, and the order is created unattributed exactly as it
// is today — the same posture operator_stations and operator_produce take, for
// the reason stated there: a wrong class here is indistinguishable from a real
// answer, so say nothing and let Core classify.
// ── AND IT NO LONGER TAKES A DIRECTION ────────────────────────────────────
//
// It used to, and both callers passed the SUPPLY spelling. That was right for
// RequestFullBin, whose claim is guarded to consume. It was WRONG for
// RequestEmptyBin, whose claim is guarded to produce (`only produce nodes
// request empty bins`) — so a produce cell asking for an empty opened a second
// episode in the consume vocabulary, alongside the one RequestProduceSwap opens
// for the very same cell and payload. One cell, one circle, two open episodes,
// and neither could see the other's orders.
//
// Under §R.87 that is the defect, not the labelling: the episode IS the circle,
// and a produce cell's circle is empty in, fill, full out. Taking the role off
// the claim collapses those two rows into the one episode that was always meant
// to be there, and removes the parameter a caller could get wrong.
//
// expected is the number of orders this press creates, the episode's
// denominator: one for a loader's empty or full, the plan's own count for a
// produce line's empty-bin request, which can be a swap of two legs or an empty
// with a prime for each bare paired position.
func (e *Engine) operatorRequestOrigin(node *processes.Node, claim *processes.NodeClaim, remaining, expected int) ordermgr.Origin {
	// Discretionary means the operator asked while still above the reorder
	// point — a pull they chose rather than one the level forced. Same
	// predicate as the produce and station paths so the flag means one thing.
	discretionary := claim != nil && claim.ReorderPoint > 0 && remaining > claim.ReorderPoint

	originID, _, err := e.openCellEpisode(
		node.ProcessID, claim,
		protocol.EpisodeTriggerOperator,
		expected,
		remaining,
		discretionary,
	)
	if err != nil {
		e.logFn("demand_episode: open episode for operator request node=%s: %v", node.Name, err)
	}
	if originID == "" {
		return ordermgr.Origin{}
	}
	return ordermgr.Attached(originID)
}

// changeoverLoadOrigin returns the active changeover's episode when THIS load
// is one the changeover asked for, and a zero Origin otherwise.
//
// Gated on the STATION's directive setting, not merely on a changeover
// existing. A station that was never opted in serves its ordinary steady-state
// demand right through a changeover — attributing those to the changeover would
// inflate its ratio with bins it did not cause, which is the mirror of the
// under-count this exists to fix.
//
// The setting is Core's (bin_loaders), read through the same loader resolver the
// card uses, so the two cannot disagree about whether this station is opted in.
func (e *Engine) changeoverLoadOrigin(node *processes.Node, claim *processes.NodeClaim) ordermgr.Origin {
	if claim == nil || !e.stationTakesLoadDirective(node.CoreNodeName) {
		return ordermgr.Origin{}
	}
	co, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return ordermgr.Origin{}
	}
	return e.changeoverOrigin(co.ID)
}

// requestEmptyOrigin answers who asked for this empty.
//
// The operator asking for an empty IS the demand, so the cell episode opens (or
// is joined) before anything is created and the order can name what caused it.
//
// UNLESS A CHANGEOVER ASKED. A load the operator makes off the changeover
// directive is that changeover's demand, not this cell's steady-state pull:
// attributed to the cell it reads in demand-origin reporting as an orphan
// replenishment nobody can tie to the changeover it served, and the
// changeover's own ratio under-counts by exactly the bins it caused.
//
// One function rather than two calls and an if at the call site, because
// RequestEmptyBin is at its statement budget: main extracted it down to the
// limit and retired its funlen exclusion in the same commit that this addition
// met at the merge. The two lookups were always one question.
func (e *Engine) requestEmptyOrigin(node *processes.Node, claim *processes.NodeClaim, cachedUOP, expected int) ordermgr.Origin {
	if o := e.changeoverLoadOrigin(node, claim); o.ID != "" {
		return o
	}
	return e.operatorRequestOrigin(node, claim, cachedUOP, expected)
}

// loadablePayloads returns the payload codes an operator may load or request at
// this manual_swap loader node: the Core loader's set at the node, else the
// claim's own list. Resolved from the loader store — the SAME resolver the
// operator board uses in station_service — so the board and this gate never
// disagree: the operator must be able to load every payload their board offers.
//
// It is deliberately NOT gated by which style is active — a loader responds to
// what is called for, not the running style: a shared loader (e.g. SNF2 + SNF3)
// must accept either cell's payload, and an operator-driven loader stages ahead
// for upcoming styles. The loader's set is style-agnostic by construction.
//
// The claim's own list answers only where the loader names no payload for the
// node, or the node is no Core loader's (a stored claim before the first
// non-empty loader sync). Pinned by TestLoadablePayloads_PrefersAggregateOverStaleClaim.
//
// RESOLVED BY NODE, NOT BY THE CLAIM'S ROLE. Which role a loader is, is Core's
// fact, and asking the loader store for it through a claim's copy of it is how
// a live loader becomes invisible. LoaderForNode cannot be ambiguous —
// bin_loader_homes is UNIQUE on position_node_id, so a node belongs to exactly
// one loader whatever its role.
func (e *Engine) loadablePayloads(node *processes.Node, claim *processes.NodeClaim) []string {
	if l, err := e.loaders().LoaderForNode(domain.NodeID(node.CoreNodeName)); err == nil && l != nil {
		// Scoped to THIS node: a dedicated home loads only its own pinned payload,
		// not the loader's other positions' parts; a shared window loads the whole set.
		if codes := l.LoadablePayloadCodesAt(domain.NodeID(node.CoreNodeName)); len(codes) > 0 {
			return codes
		}
	}
	return claim.AllowedPayloads()
}

// LoadBin marks a bin at a manual_swap node as loaded with the given manifest.
// Calls Core's HTTP API directly to set the manifest on the existing bin at
// that node. No transport order is created — the bin stays in place until a
// move order sends it to OutboundDestination.
//
// uopCount is absent-or-value: nil when nobody declared a count, in which case
// Core answers from the payload's standard pack. Core resolves it either way
// and returns what it wrote, and that answer — not this argument — is what
// gets seated on the node. See seatManuallyLoadedBin.
func (e *Engine) LoadBin(nodeID int64, payloadCode string, uopCount *int64, manifest []protocol.IngestManifestItem) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}

	// A1 (hop 2026-07-23): a paired / on-deck press-index position holds ONLY an
	// empty carrier — it is the slot a fresh empty waits on to be indexed onto
	// the core (front) position. LOAD always stamps a part number, and a stamped
	// part on a paired node is exactly what hung the Hopkinsville press-index
	// swap (KK21 stamped on PLN_02/05): the evac legs cleared the presses but the
	// index legs then saw the wrong-part on-deck bins as unavailable and hung.
	// Refuse the stamp on any node another claim names as a paired/on-deck
	// position, with a clear operator-facing message (surfaced as a toast). Runs
	// before requireLoaderClaim so a paired node gets THIS message, not the
	// generic "not a manual_swap node" — which is why that helper takes an
	// already-loaded claim instead of resolving one itself, and why moving this
	// check below it would silently swap the operator's diagnosis for a worse
	// one. Fail-open on a read error — a local
	// SQLite blip must not block a legitimate loader load; the guard is defense
	// in depth, not the only backstop.
	if onDeck, derr := e.db.IsPairedOnDeckNode(node.ProcessID, node.CoreNodeName); derr != nil {
		e.logFn("LoadBin: on-deck check for node %s failed: %v — allowing (fail-open on read error)", node.Name, derr)
	} else if onDeck {
		return fmt.Errorf("%s is an on-deck / paired position — it may hold only an empty carrier, never a stamped part. Load the part at the press's core (front) position instead", node.Name)
	}

	if err := requireLoaderClaim(node, claim); err != nil {
		return err
	}
	if len(manifest) == 0 {
		return fmt.Errorf("manifest is empty")
	}

	// Check that a bin is actually at this node and that it's empty.
	// Loading on top of an existing payload would silently overwrite the
	// manifest and double-trigger the side-cycle (bin already in flight to
	// outbound). The card stays clickable in stale views — server has to
	// refuse rather than rely on the UI gate.
	//
	// FROM THE BINS TABLE (site 4 of the reads), under the decision rule. A
	// table that may not be decided from, or a window it holds no row for,
	// refuses with its own sentence (BinsNotCurrent), never "no bin at node X —
	// request an empty bin first": that is a positive claim about the physical
	// world the Edge did not verify, told to an operator standing in front of
	// the bin, and it prescribes a corrective action that the request path then
	// refuses too.
	//
	// THE BIN IT CHECKED IS THE BIN IT LOADS: its id rides the load request, so
	// Core writes the carrier this check found empty rather than whichever one
	// its own query returns first.
	//
	// AND IT SAYS WHAT IT READ: the carrier's generation and that it carries no
	// part (protocol.LoadExpectBare: a labelled carrier is answered above
	// without a load). Core refuses the LOAD when either has changed since,
	// rather than loading over what stands there now, and its refusal's rows
	// redraw the table before its sentence shows.
	row, err := e.binsRowAt(node.CoreNodeName)
	if err != nil {
		return err
	}
	if !row.Occupied {
		return fmt.Errorf("no bin at node %s — request an empty bin first", node.Name)
	}
	if row.PayloadCode != "" {
		return e.loadOntoLabelledCarrier(node, claim, row)
	}
	checkedBinID, checkedEpoch, expect := row.BinID, row.Epoch, protocol.LoadExpectBare

	// Validate payload code against the loader-wide loadable set (see
	// loadablePayloads) — the loader fills what the system/operator calls for
	// across every cell sharing it, not just this node's running style.
	allowed := e.loadablePayloads(node, claim)
	if payloadCode == "" && len(allowed) > 0 {
		payloadCode = allowed[0]
	}
	if payloadCode == "" {
		return fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(allowed, payloadCode) {
		return fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	// NO LOCAL FALLBACK. An undeclared count travels as absence and Core
	// resolves it from the payload's standard pack — the same template this
	// used to fetch, read by the side that owns the ledger. Resolving it here
	// as well meant two answers to one question, and the Edge's was the one
	// with no way to say "I could not reach the template": the fetch reports
	// every failure as a nil result, so the fallback quietly produced 0 and
	// then wrote that 0 onto the node as if somebody had counted it.
	//
	// Load bin via direct HTTP to Core — synchronous, immediate feedback
	items := make([]BinLoadItem, len(manifest))
	for i, m := range manifest {
		items[i] = BinLoadItem(m)
	}
	req := &BinLoadRequest{
		NodeName:    node.CoreNodeName,
		BinID:       checkedBinID,
		Epoch:       checkedEpoch,
		Expect:      expect,
		PayloadCode: payloadCode,
		UOPCount:    uopCount,
		Manifest:    items,
	}
	// The LOAD goes as an ask when Core answers it (bin.load), through the
	// bin-load door otherwise. An answer that comes after the tap's wait runs
	// the same follow-up then (afterLoad), in this process.
	loadResp, err := askOrDoorLate(e, protocol.AskBinLoad, req,
		func() (*BinLoadResponse, error) { return e.coreClient.LoadBin(req) },
		func(late *BinLoadResponse, lerr error) {
			if lerr != nil {
				log.Printf("bin_ops: LOAD at node %s, answered late: %v", node.CoreNodeName, e.refused(lerr))
				return
			}
			e.afterLoad(nodeID, node, claim, payloadCode, late)
		})
	if errors.Is(err, ErrAskPending) {
		return fmt.Errorf("load bin: %w", err)
	}
	if err != nil {
		// A refusal redraws the window from Core's rows before its sentence.
		return fmt.Errorf("load bin: %w", e.refused(err))
	}
	e.afterLoad(nodeID, node, claim, payloadCode, loadResp)
	return nil
}

// afterLoad is a LOAD's follow-up once Core has loaded the carrier: the
// carrier's identity, the L1's confirm and the L2, the seat bound to Core's
// count and stamp, the refusal it ends. Run when the answer comes, whether
// inside the tap's wait or after it.
func (e *Engine) afterLoad(nodeID int64, node *processes.Node, claim *processes.NodeClaim, payloadCode string, loadResp *BinLoadResponse) {
	// CORE'S ANSWER IS THE COUNT, and everything downstream of here uses it
	// rather than the argument. Core resolved the number it actually wrote to
	// the ledger — the operator's when they declared one, the standard pack
	// when they did not — so this is the same value the bin now carries, and
	// seating anything else puts the Edge and the ledger into disagreement
	// about a carrier both of them can see.
	seatedUOP := int64(loadResp.UOPRemaining)

	// The operator's tap on LOAD is the explicit confirmation that the L1
	// retrieve_empty arrived and has been filled. Confirming the L1 here
	// transitions it delivered → confirmed and sends a delivery receipt to Core.
	// An L1 Core already confirmed (its auto-confirm sweep, for a Core-born
	// empty) is not found here and takes the branch below; either way the L2 is
	// filed by this LOAD, after the confirm (fileLoadedOutbound).
	//
	// The operator is the instrument for what the window now holds: the same
	// write the branch below makes through seatManuallyLoadedBin. Pinned by
	// TestPinD3a_LoadAfterEcho_ConfirmsTheL1AndFilesOneL2.
	e.recordLinesideCarrier(node.ID, node.CoreNodeName,
		domain.KnownCarrier(domain.LinesidePayloadCode(payloadCode)), domain.CarrierFromOperator)
	if l1ID, l1Confirmed := e.confirmDeliveredAt(node.CoreNodeName, true, seatedUOP); l1Confirmed {
		log.Printf("bin_ops: confirmed L1 order %d on operator load at node %d", l1ID, nodeID)
		// The L1's completion left the runtime pointer to this LOAD
		// (applyLoaderEmptyIn): onto the L2, or off the confirmed L1 when no
		// L2 is filed (no outbound, or one already owed).
		if !e.fileLoadedOutbound(node, claim, payloadCode, 0) {
			if err := e.db.ClearProcessNodeRuntimeOrderRefs(l1ID); err != nil {
				log.Printf("bin_ops: clear runtime pointers to L1 %d: %v", l1ID, err)
			}
		}
		// Belt-and-suspenders: set active_bin_id directly from Core's LoadBin
		// response. The delivered handler also sets it
		// from the L1 order's BinID, but if Core's order.delivered envelope
		// arrived without bin_id (multi-bin order, or pre-fix Core build)
		// the event handler ends up with nil. The LoadBin response is the
		// authoritative pointer at this exact moment.
		if loadResp != nil && loadResp.BinID > 0 {
			if e.inventoryDelta != nil {
				if err := e.inventoryDelta.BindActiveBin(nodeID, loadResp.BinID, loadResp.DeltaEpoch); err != nil {
					log.Printf("bin_ops: bind active bin for node %d: %v", nodeID, err)
				}
			}
		}
		// Flush trigger: bin-loader confirm is the produce/manual_swap
		// loader-side boundary at which the outgoing bin is "done" —
		// any accumulated deltas attributed to that bin should ship
		// before the new bin starts driving counts. Periodic 5s flush
		// would catch them eventually, but firing here makes the audit
		// trail align with the operator action.
		if e.inventoryDelta != nil {
			e.inventoryDelta.Flush()
		}
		e.clearRefusalOnLoad(node.CoreNodeName, payloadCode)
		return
	}

	// No delivered L1 here: the operator loaded a bin placed at the loader by
	// hand, or Core already confirmed the empty that brought it (its auto-confirm
	// sweep, for a Core-born L1). Seat the bin and file the L2 so it still goes
	// to outbound. active_bin_id comes from Core's LoadBin response — that's the
	// authoritative pointer to the bin physically at this slot, regardless
	// of whether any order was tracking it.
	claimIDPtr := persistedClaimID(claim)
	var activeBinID *int64
	var deltaEpoch int64
	if loadResp != nil && loadResp.BinID > 0 {
		v := loadResp.BinID
		activeBinID = &v
		deltaEpoch = loadResp.DeltaEpoch
	}
	e.seatManuallyLoadedBin(node, claimIDPtr, activeBinID, deltaEpoch, int(seatedUOP), payloadCode)
	e.fileLoadedOutbound(node, claim, payloadCode, 0)

	e.clearRefusalOnLoad(node.CoreNodeName, payloadCode)
}

// clearRefusalOnLoad is CLEAR-ON-LOAD, the NORMAL end of a supply refusal. The
// parts arrived, the operator loads them, and the card goes back to normal
// without anyone having to remember to undo anything. Both LOAD paths run it —
// the side cycle confirming its L1 and the fallback with none in flight — since
// the operator loaded the part either way.
//
// It has to be explicit rather than left to the order going terminal: the
// order lags the load, and in that gap the card would still read REFUSED
// about material the operator is standing there holding. They just fixed it;
// the screen should say so.
//
// Closed goes to Core and the other Edges only when a row was deleted. Best-
// effort and last: a failed delete must not fail the load, and a stale refusal
// is visible and undoable, where a lost load is neither.
func (e *Engine) clearRefusalOnLoad(loaderNode, payloadCode string) {
	cleared, err := e.db.DeleteSupplyRefusal(loaderNode, payloadCode)
	if err != nil {
		log.Printf("bin_ops: clear supply refusal on load at %s: %v", loaderNode, err)
		return
	}
	if cleared {
		e.announceRefusal(loaderNode, payloadCode)
		e.emitSupplyRefusal(protocol.SupplyRefusalState{Action: protocol.SupplyRefusalClosed, LoaderNode: loaderNode, PayloadCode: payloadCode})
	}
}

// confirmDeliveredAt confirms the oldest delivered side-cycle retrieve at this
// core node, treating the operator's tap as the receipt acknowledgement: LOAD at
// a loader confirms the empty-in (L1, retrieveEmpty=true) with the count Core
// seated, CLEAR at an unloader confirms the full-in (U1, retrieveEmpty=false)
// with 0 — the bin is empty once the operator has processed the contents.
// Direction and count are the caller's, stated at the call, because the two taps
// differ in exactly those two facts and nothing else.
//
// Returns (orderID, true) when an order was actually confirmed; (0, false)
// otherwise (none delivered, or the confirm transition itself failed).
//
// Looks the order up by the CORE NODE (delivery_node), NOT by the process_node
// the operator acted at. On a loader shared across styles/cells one core node
// has many process_node rows, and the staged order may be tracked against a
// different row than the operator's station — a process-node-scoped query then
// misses it, the tap falls through, and the order orphans at `delivered` while
// the bin still ships (plant 2026-06-01). A core node is one physical slot, so
// there is at most one delivered bin there to confirm. Querying delivery_node
// also sidesteps a drifted runtime.ActiveOrderID (a prior fallback overwrites it
// with the L2 move ID).
//
// The lookup matches either spelling of the empty-in's type — Core's echo
// rewrites retrieve to retrieve_empty (see ListDeliveredRetrieveByDeliveryNode).
// Pinned by TestPinConfirmL1_OldestDeliveredAtTheCoreNode_WithTheSeatedCount,
// TestPinConfirmL1_FindsTheEchoedTypeSpelling and the TestConfirmUnloaderU1OnClear
// family.
func (e *Engine) confirmDeliveredAt(coreNodeName string, retrieveEmpty bool, finalCount int64) (int64, bool) {
	what, tap := "full-ins", "clear"
	leg := "U1"
	if retrieveEmpty {
		what, tap, leg = "empties", "load", "L1"
	}
	delivered, err := e.db.ListDeliveredRetrieveByDeliveryNode(coreNodeName, retrieveEmpty)
	if err != nil {
		log.Printf("bin_ops: list delivered %s for node %s: %v", what, coreNodeName, err)
		return 0, false
	}
	if len(delivered) == 0 {
		return 0, false
	}
	id := delivered[0].ID // oldest delivered at this slot
	if err := e.orderMgr.ConfirmDelivery(id, finalCount); err != nil {
		log.Printf("bin_ops: confirm %s %d on %s: %v", leg, id, tap, err)
		return 0, false
	}
	return id, true
}

// seatManuallyLoadedBin writes what a hand-load just put on a node: the runtime
// binding, and what the carrier is.
//
// THE OPERATOR IS THE INSTRUMENT HERE. A manual load has no delivery envelope
// behind it, so their selection is not a weaker answer than Core's — it is the
// only one. They chose payloadCode and it was validated against what this node
// accepts, which makes it exactly the statement the resident identity is for.
func (e *Engine) seatManuallyLoadedBin(node *processes.Node, claimIDPtr, activeBinID *int64, deltaEpoch int64, uopCount int, payloadCode string) {
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.ManualLoad(node.ID, claimIDPtr, activeBinID, deltaEpoch, uopCount); err != nil {
			log.Printf("bin_ops: set runtime for node %d: %v", node.ID, err)
		}
	}
	e.recordLinesideCarrier(node.ID, node.CoreNodeName,
		domain.KnownCarrier(domain.LinesidePayloadCode(payloadCode)), domain.CarrierFromOperator)
}

// ClearBin clears the manifest on the bin at a manual_swap node. For consume-role
// nodes (unloaders) it ALSO drives the side-cycle's empty-out (U2): the operator's
// CLEAR tap means "I processed this bin's contents; the now-empty bin is ready to
// go back." Used by unloaders after physical removal and for fixing mis-loads.
//
// The empty-out is driven by the CLEAR itself (createUnloaderEmptyOut), not by a U1
// retrieve completing. That is the whole point: a press/forklift-fed drain has NO
// inbound U1 (the press delivers the full directly), so the old U1-completion trigger
// never fired and the empty bin stranded at the window. Driving off the clear fires
// the U2 for an AMR-fed unloader (which still confirms its U1 here, receipt-ack style)
// and a directly-fed drain alike — one path, no double-fire.
//
// The empty-out is created AFTER the manifest clear has committed on Core. It used to
// be created before, on the reasoning that Core's bin record was "still coherent" — but
// the ordering bought nothing and cost a race: between the create and the clear, the
// bin Core hands the U2 still carries its payload, and a mover that reads it in that
// window carries a labelled carrier to the empty-totes destination. Creating it after
// the clear commits means the carrier a U2 ever names is already empty.
//
// It is still gated on a bin actually being present, and the clear itself is the
// answer: Core refuses to clear a node that holds no bin (400 "no bin at node",
// TestApiBinClear_NodeWithoutBin_Refuses) before it clears anything, so clearing an
// already-empty window returns that refusal and creates nothing — and a clear that
// succeeds proves a carrier was there. It still fires for EVERY consume drain, not
// just AMR-fed ones, which was the reason the create sat on the CLEAR in the first
// place.
//
// THE PRE-READ IS GONE. ClearBin used to read node-bins before the clear only to
// learn "was there a bin, and what did it hold". That was a Core round trip per CLEAR
// at every consume station, and it was wrong on a failed read: an unreachable
// node-bins answered "no bin", the clear then committed anyway, and the cleared
// carrier got no U2 (TestPinFold_ClearWhenThePreReadFails). The payload for the log
// line now rides the clear's own answer (cleared_payload_code); a Core too old to
// send it leaves the line blank and changes nothing else
// (TestPinFold_ClearAgainstTodaysCoreResponse).
//
// Post-clear, if the claim has AutoPush enabled, rePushOwnUnloader offers the next pull
// for this node's own unloader to the reservation seam — a no-inbound drain is gated
// there too, so it's a no-op.
// ClearBin clears the bin at the consume-unloader window. binTypeCode is the
// dunnage type the operator selected at the confirm tap, sent as given; empty
// string sends none. The Edge never fills it in. At a two-stage unloader Core
// stamps the cart itself from the cart's own type — the stage-1 marker on a
// Full off, the carrier back on a stage-2 Send on — so the board posts a blank
// code there and the Edge passes it through
// (TestClearBin_BinTypeCodeAtACoreOwnedUnloader). layers is the height the
// operator declared with the CLEAR (the panel's height button), passed to Core
// as given; nil declares none (TestClearBin_DeclaredLayersReachCore).
//
// When the clear commits but the unloader has nowhere to send the carrier
// (createUnloaderEmptyOut finds no outbound), the bookkeeping below still runs
// and ClearBin returns an error that says the bin WAS cleared: the operator has
// to learn the carrier is staying, and a retry is not the fix
// (TestClearBin_NoOutboundTellsTheOperator).
//
// unload is the person's second tap on a carrier Core holds as an unfinished
// partial in its home: it is cleared anyway, booked as any clear. Without it
// Core refuses to clear one.
func (e *Engine) ClearBin(nodeID int64, binTypeCode string, unload bool, layers *int) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if err := requireLoaderClaim(node, claim); err != nil {
		return err
	}
	if claim.Role == protocol.ClaimRoleConsume {
		// Confirm any AMR-fed inbound (U1) — the operator's CLEAR tap IS the receipt
		// ack. A press/forklift-fed drain has no U1; the helper returns ok=false and
		// we proceed to the empty-out regardless (it no longer depends on a U1).
		if u1ID, ok := e.confirmDeliveredAt(node.CoreNodeName, false, 0); ok {
			log.Printf("bin_ops: confirmed U1 order %d on operator clear at node %s", u1ID, node.CoreNodeName)
		}
	}
	// The CLEAR names the carrier the operator saw on the tile (boardBinID):
	// Core refuses it when another stands there, rather than clearing that one.
	req := protocol.BinClearRequest{NodeName: node.CoreNodeName,
		BinID: e.boardBinID(node.CoreNodeName), BinTypeCode: binTypeCode, Unload: unload, Layers: layers}
	// The CLEAR goes as an ask when Core answers it (bin.clear), through the
	// bin-clear door otherwise. An answer that comes after the tap's wait runs
	// the same follow-up then (afterClear), in this process.
	cleared, err := askOrDoorLate(e, protocol.AskBinClear, req,
		func() (*BinClearResponse, error) { return e.coreClient.ClearBin(req) },
		func(late *BinClearResponse, lerr error) {
			if lerr != nil {
				log.Printf("bin_ops: CLEAR at node %s, answered late: %v", node.CoreNodeName, e.refused(lerr))
				return
			}
			if ferr := e.afterClear(nodeID, node, claim, late); ferr != nil {
				log.Printf("bin_ops: CLEAR at node %s, answered late: %v", node.CoreNodeName, ferr)
			}
		})
	if errors.Is(err, ErrAskPending) {
		return fmt.Errorf("clear bin: %w", err)
	}
	if err != nil {
		// A refusal redraws the window from Core's rows before its sentence.
		return fmt.Errorf("clear bin: %w", e.refused(err))
	}
	return e.afterClear(nodeID, node, claim, cleared)
}

// afterClear is a CLEAR's follow-up once Core has cleared the carrier: the
// empty-out at a consume window, the seat's zero under Core's new stamp, the
// carrier's identity, the push-driven re-offers. Run when the answer comes,
// whether inside the tap's wait or after it.
func (e *Engine) afterClear(nodeID int64, node *processes.Node, claim *processes.NodeClaim, cleared *BinClearResponse) error {
	// What the carrier held, for the CLEAR log line only — it does NOT ride the
	// empty-out (see createUnloaderEmptyOut for why the U2 names no part). Blank
	// from a Core that predates the field.
	clearedPayload := cleared.ClearedPayloadCode
	// Empty-out (U2): send the now-empty bin to the unloader's outbound (empty
	// totes). Fired off the CLEAR — so it runs for every consume drain, not just
	// ones an AMR fed — but only after the clear has COMMITTED on Core, so the
	// carrier the U2 names is empty on Core's side too and not merely about to be.
	// The clear succeeding is what says a carrier was there (see the doc above).
	var notSent error
	if claim.Role == protocol.ClaimRoleConsume {
		// Double-tap guard, the same one PushEmptyOut carries: the order layer has
		// no dedup for move orders, so a second CLEAR tap on the same window would
		// mint a second U2 for one physical carrier. Moving the create after the
		// clear widened the window for that, because the clear is a round trip to
		// Core.
		//
		// It SKIPS the U2 rather than failing, which is where it differs from
		// PushEmptyOut. There the empty-out IS the operation, so refusing it is the
		// honest answer. Here the clear has already committed; returning an error
		// would tell the operator their CLEAR failed when it did not, and a retry
		// would then find no bin and create nothing at all.
		//
		// A read error skips the U2 too, matching PushEmptyOut's direction. A
		// missed empty-out strands one carrier at a window the operator can still
		// tap PUSH EMPTY on; a duplicate sends two robots for one bin, and the
		// second finds nothing there.
		if claim.PulledDirectly {
			// A stage 2 pulled directly by the process: the finished cart
			// stays on the window for the line to take, so there is no
			// empty-out and nothing to tell the operator.
			log.Printf("bin_ops: %s is pulled directly by the process — the cart waits on the window for the line",
				node.Name)
		} else if existingID, inFlight, lerr := e.emptyOutInFlight(nodeID); lerr != nil {
			log.Printf("bin_ops: check in-flight move for node %s: %v", node.Name, lerr)
		} else if inFlight {
			log.Printf("bin_ops: skipping empty-out at node %s — order %d is already moving this carrier out",
				node.Name, existingID)
		} else if nerr := e.createUnloaderEmptyOut(node, claim, cleared.BinID); nerr != nil {
			notSent = nerr
		}
	}
	claimIDPtr := persistedClaimID(claim)
	if e.inventoryDelta != nil {
		// Take the new generation stamp with the zeroed count. The clear ended
		// this carrier's life on Core and started the next one; keeping the old
		// stamp means every count reported for it from here on is discarded.
		if err := e.inventoryDelta.SetClaimCountAndEpoch(nodeID, claimIDPtr, 0, cleared.BinID, cleared.DeltaEpoch); err != nil {
			log.Printf("bin_ops: set runtime for node %d: %v", nodeID, err)
		}
	}
	// THE CLEAR IS AN IDENTITY EVENT, and it was not going through the doorway.
	// The three writers were delivery, hand-load and departure; CLEAR left the
	// previous occupant's part number standing on a carrier the operator had
	// just emptied, and the doorway's own header names that as the failure mode
	// — "a value held over from the previous occupant is a confident wrong
	// answer about this one".
	//
	// A KNOWN EMPTY carrier, not an unknown one. The operator is looking at it.
	// That distinction is why lineside_payload_known has to exist before this
	// call can be made: writing "" without the known bit would re-arm
	// binAtNode's claim fallback and put the claim's part number straight back
	// on the carrier, which is the cross product this pair was split to end.
	e.recordLinesideCarrier(nodeID, node.CoreNodeName, domain.KnownCarrier(""), domain.CarrierFromOperator)
	// The count that just went away, as Core counted it. Core records it durably
	// (ClearForReuseTx writes a bin_uop_ledger row with before->0 and op
	// clear_for_reuse, in the clear's own transaction); this line is what lets
	// somebody reading the Edge log find that row. It used to print this Edge's
	// runtime cache, which a window never learns, so every clear read 0.
	discarded := cleared.ClearedUOP
	log.Printf("bin_ops: CLEAR at node %s discarded %d parts on bin %d (payload=%q, type=%q, new epoch=%d) — "+
		"Core holds the ledger row (clear_for_reuse)",
		node.CoreNodeName, discarded, cleared.BinID, clearedPayload, cleared.ClearedBinTypeCode, cleared.DeltaEpoch)
	// Push-driven unloader: offer the next pull at the unloader this window
	// belongs to. The AutoPush gate is this condition. The carrier is still on the
	// window (it leaves at the U2's pickup), so it is counted held and the offer
	// goes to Core only when the order table shows a pull the seam could make —
	// see rePushOwnUnloaderIfUncovered.
	if claim.Role == protocol.ClaimRoleConsume && claim.AutoPush {
		e.rePushOwnUnloaderIfUncovered(node, node.CoreNodeName)
	}
	// Push-driven loader (operator-staged): the operator cleared the window, so
	// stage the next empty at the loader this window belongs to — only that one,
	// and only if it is operator-staged (rePushOwnLoader). The walk over every
	// operator-staged loader this replaced read Core once per loader and could
	// stage an empty at a loader whose window this tap did not free.
	if claim.Role == protocol.ClaimRoleProduce {
		e.rePushOwnLoader(node)
	}
	if notSent != nil {
		return fmt.Errorf("bin cleared, but %w", notSent)
	}
	return nil
}

// PushEmptyOut sends the empty bin currently sitting in a drain window outbound
// without waiting for the next full-bin delivery. Used when the slot holds an
// empty (payload_code=="") and the operator taps PUSH EMPTY on the unloader
// board — the carrier exits immediately so the AMR can bring a fresh full bin.
//
// Delegates to createUnloaderEmptyOut so all the same routing/logging rules
// apply (outbound from loader aggregate, auto-confirms, runtime order updated).
func (e *Engine) PushEmptyOut(nodeID int64) error {
	node, _, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	// Mirror ClearBin: PushEmptyOut is for manual_swap consume windows only.
	if err := requireLoaderClaim(node, claim); err != nil {
		return err
	}
	if claim.Role != protocol.ClaimRoleConsume {
		return fmt.Errorf("node %s is not a consume node", node.Name)
	}
	// FAILS CLOSED — no bin means refuse, never "push anyway". The window is
	// read from the bins table (site 5 of the reads): a table that may not be
	// decided from, or a window it holds no row for, refuses with its own
	// sentence (BinsNotCurrent), never "no bin": the carrier may still be there,
	// and nothing is pushed.
	row, err := e.binsRowAt(node.CoreNodeName)
	if err != nil {
		return err
	}
	if !row.Occupied {
		return fmt.Errorf("node %s has no bin to push", node.Name)
	}
	// Only push if the bin is actually empty (no payload). A full bin sitting
	// in the window means the operator confirmed earlier but the AMR is still
	// staging — don't evict it.
	if row.PayloadCode != "" {
		return fmt.Errorf("node %s bin is not empty (payload=%q)", node.Name, row.PayloadCode)
	}
	// Double-tap guard: the order layer has no dedup for move orders, so two
	// rapid taps would each call createUnloaderEmptyOut and create two U2
	// orders for the same physical bin. Here the empty-out IS the operation, so
	// an active move — or a read that could not say — refuses it; ClearBin runs
	// the same check and skips instead. See emptyOutInFlight for what it keys on.
	if _, inFlight, err := e.emptyOutInFlight(nodeID); err != nil {
		return fmt.Errorf("check in-flight move for node %s: %w", node.Name, err)
	} else if inFlight {
		return fmt.Errorf("node %s already has an empty-out in flight", node.Name)
	}
	// Same empty-out as ClearBin's door, and it names no part either: the U2 is a
	// removal of whatever carrier stands on the window, never a fetch for a part.
	// See createUnloaderEmptyOut. Nowhere to send it is a refusal here: the
	// push is the whole operation and nothing has been committed.
	if err := e.createUnloaderEmptyOut(node, claim, row.BinID); err != nil {
		return err
	}
	// Re-arm the delivery seam — mirrors ClearBin's gate at its tail, with the
	// tapped window counted held for the same reason.
	if claim.AutoPush {
		e.rePushOwnUnloaderIfUncovered(node, node.CoreNodeName)
	}
	return nil
}

// emptyOutInFlight is the double-tap check shared by ClearBin and PushEmptyOut:
// is any non-terminal MOVE tracked at this process_node? It returns the oldest
// one's ID for the log line. The CALLERS decide what a hit means — PushEmptyOut
// refuses, ClearBin skips its U2 (its clear has already committed) — and both
// treat a read error as "do not create".
//
// KEYED ON THE PROCESS NODE AND THE ORDER TYPE, and it counts a move ARRIVING
// here as readily as one leaving. That is the opposite shape from
// outboundMoveInFlight (core node, departures only, loader side), which is why
// the two are not one function: merged, one of them would change which taps it
// refuses. Pinned by TestPinDoubleTapGuard_KeysOnAnyMoveAtTheProcessNode.
func (e *Engine) emptyOutInFlight(nodeID int64) (existingID int64, inFlight bool, err error) {
	existing, err := e.db.ListActiveOrdersByProcessNodeAndType(nodeID, protocol.OrderTypeMove)
	if err != nil {
		return 0, false, err
	}
	if len(existing) == 0 {
		return 0, false, nil
	}
	return existing[0].ID, true, nil
}

// createUnloaderEmptyOut fires the side-cycle empty-out (U2): a move of the now-empty
// bin from the unloader window to the unloader's outbound destination (e.g. empty
// totes). Called from ClearBin once the operator confirms a processed bin, so it fires
// for an AMR-fed unloader AND a press/forklift-fed drain — the latter has no inbound
// U1, so the old order-completion trigger (handleUnloaderFullInCompletion, now removed)
// never fired for it.
//
// Outbound resolves from the loader AGGREGATE (consume role), falling back to the
// claim — the same severing-the-legacy-claim source the old handler used. U2
// auto-confirms: outbound is an unattended supermarket node with no operator to tap
// CONFIRM (same rule as L2).
//
// THE U2 NAMES NO PART. Core reads a part on a move as "this carrier is fetched for
// part X", and three things follow from that, all wrong for a removal:
//
//   - tier 4 judges the resident carrier against the part's carrier rule
//     (binresolver BinUnavailableReason). A shared window's CLEAR picker offers the
//     union of its payloads' carrier types, so the operator can declare a type the
//     cleared part may not travel in; the U2 is then refused its own carrier and
//     parks as finder-node-empty while the carrier stands there;
//   - at a home-location (dedicated) unloader, tier 2 turns the move into a pool
//     Drain selection (sourceFromDedicatedLoader), rejects the just-cleared empty,
//     and drives out the oldest bin of that part anywhere in the pool;
//   - the destination is gated through payloadAllowedAt for a part the carrier no
//     longer holds.
//
// The carrier rule's own doc says a removal leg passes payloadCode "", and the
// ALN_006 removal ruling (allocator) binds by node, never by the order's payload
// tag. CreateMoveOrderForBin sends the carried payload as given and never
// back-fills it from a claim, so "" reaches the wire. The operator board
// attributes the move by source_node instead (cardModel in
// operator-window-state.js, and the modal's demand queue). Pinned by
// TestEmptyOut_EnvelopeNamesNoPart and, Core side,
// TestPayloadlessEmptyOut_SourcesAResidentThePartCouldNotCarry.
//
// IT NAMES ITS BIN. binID is Core's id for the carrier: the one the CLEAR's
// reply says it cleared, or the one PUSH EMPTY's own read found on the window.
// Core then lifts that carrier and no other, ends the move skipped once it has
// left the window, and types the order by it before dispatch claims it (Core's
// orders.named_bin_id: counted as coming into a levelled group, judged on its
// own type's level). 0 from a Core that sends no id moves whatever stands
// there, as before. Pinned by TestEmptyOut_ClearNamesTheBinItCleared and
// TestEmptyOut_PushEmptyNamesTheBinItRead.
//
// NO OUTBOUND IS AN ERROR, not a quiet return. It used to return nothing, so a
// CLEAR at an unloader with no outbound (or one pointing at itself) told the
// operator it worked while the carrier stayed in the window with no move owed.
// outboundFor logs the reason; the error is what reaches the operator, through
// ClearBin (which has already cleared, and says so) and PushEmptyOut (where the
// move is the whole operation, so it is a refusal). A failure to CREATE the move
// is still only logged.
func (e *Engine) createUnloaderEmptyOut(node *processes.Node, claim *processes.NodeClaim, binID int64) error {
	outbound, ok := e.outboundFor(node, claim, domain.RoleConsume)
	if !ok {
		return fmt.Errorf("%s has nowhere to send the empty carrier: it stays in the window until it is moved by hand", node.Name)
	}
	nodeID := node.ID
	// NoDemand: the side-cycle's empty-out is the system's own consequence of a
	// clear, not a demand anybody expressed.
	order, err := e.orderMgr.CreateMoveOrderForBin(&nodeID, node.CoreNodeName, outbound, "", binID,
		ordermgr.NoDemand())
	if err != nil {
		e.logFn("side-cycle: create U2 (empty-out) for unloader %s: %v", node.Name, err)
		return nil
	}
	log.Printf("side-cycle: U2 (empty-out) order %d for unloader %s → %s (names no part)", order.ID, node.Name, outbound)
	// Point the runtime active order at U2 so the unloader UI shows the empty-out next.
	// (ClearBin's SetClaimCountAndEpoch zeroes the count but leaves this pointer.)
	if err := e.db.SetProcessNodeRuntimeActiveOrder(node.ID, &order.ID); err != nil {
		log.Printf("side-cycle: update runtime orders for unloader %d: %v", node.ID, err)
	}
	return nil
}

// RequestEmptyBin delivers an empty bin to a produce node. A loader window
// issues a single retrieve order through the never-2N seam; the swap modes
// (single_robot, two_robot, two_robot_press_index, sequential) plan and apply
// exactly what the produce request would, without its guards about the parts,
// so the robot choreography and the checks on the line are the produce
// request's. Any other mode is refused. Returns the primary order (the plain empty, the first swap leg, or
// the first prime); a second leg is tracked on the runtime row.
func (e *Engine) RequestEmptyBin(nodeID int64, payloadCode string) (*orders.Order, error) {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if claim == nil {
		return nil, fmt.Errorf("node %s has no active claim", node.Name)
	}
	if claim.Role != protocol.ClaimRoleProduce {
		return nil, fmt.Errorf("node %s: only produce nodes request empty bins", node.Name)
	}
	// A SWAP-MODE LINE ASKS WHAT THE PRODUCE REQUEST ASKS, and nothing more: the
	// same guards on the line (guardLineRequest, under the cell's lock), the
	// same plan, the same orders. It used to ask CanAcceptOrders and count the
	// empties already inbound first, and those refused what the produce request
	// allowed: a line whose own changeover leg had landed, and a line holding a
	// bin another station had delivered. Both buttons on one line give one
	// answer.
	if slices.Contains(protocol.ConfigurableSwapModes(), claim.SwapMode) {
		res, err := e.produceRequest(node, runtime, claim, produceAsk{})
		if err != nil {
			return nil, err
		}
		return res.primary(), nil
	}
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	// Payload handling splits by mode:
	//
	//   - manual_swap (bin loader): an empty is a generic carrier, so the
	//     operator-initiated request is payload-AGNOSTIC. A blank payloadCode is
	//     the normal case — the order ships untagged, Core's planTransport
	//     sources any compatible empty, and LoadBin binds the real payload when
	//     the operator fills it. A non-blank code (direct API caller, or a
	//     future carrier picker) is still validated against the loadable set.
	//
	//     Blank sourcing assumes the loader is SINGLE-CARRIER unless it declares
	//     a carrier mix. Which carrier type an empty going to a loader window
	//     should be is Core's call, from the loader's declared mix and the
	//     window's capability (wantedBinType, dispatch/source_finder_want.go), so
	//     the order needs no carrier field. A loader that declares no mix — every
	//     loader today — gets "" from wantedBinType and takes the first compatible
	//     empty, which on a loader spanning several carrier types can be the
	//     wrong container.
	//
	//   - a swap-mode line (above): the empty is the claim's, planned and
	//     created by the produce request's own path, which reads the claim and
	//     not this argument. The code is neither required nor checked there: a
	//     check of an argument nothing reads refuses requests for no reason.
	if claim.IsLoaderNode() {
		if payloadCode != "" && !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
			return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
		}
		reqOrigin := e.requestEmptyOrigin(node, claim, runtime.RemainingUOPCached, 1)
		return e.requestEmptyAtManualSwapLoader(nodeID, node, claim, payloadCode, reqOrigin)
	}
	// A mode outside the configurable set: the retired "simple", or blank. No
	// door stores one and neither plant holds one; it had an arm of its own
	// here, a plain retrieve. Pinned by
	// TestRequestEmptyBin_AModeOutsideTheConfigurableSetIsRefused.
	return nil, fmt.Errorf("node %s: swap mode %q takes no request; set the line's swap mode", node.Name, claim.SwapMode)
}

// requestEmptyAtManualSwapLoader is RequestEmptyBin's manual_swap arm, lifted out
// whole. It returns on every path, which is what made it liftable — and the same
// property dc97331c relied on when it found the two blocks below this one were
// unreachable.
//
// EXTRACTED FOR THE CEILING, and the ceiling is real. dc97331c trimmed
// RequestEmptyBin from 65 to 60 statements and wrote down that the file then held
// two functions sitting at exactly 60 — "both one statement from the ceiling".
// d030a8ca added the press-index prime guard, which is correct and belongs there,
// and the function went to 64. Shaving it back to exactly 60 would have restored
// the same trap for the next person with a legitimate guard to add, so the arm
// moves out instead and the caller gets headroom rather than a fresh tripwire.
//
// The seam, unchanged: an operator request and a kanban signal can't both pass the
// in-flight count and both fire — the never-2N invariant. The seam owns the count,
// the budget, and the create atomically; want=1 (the operator asks for one empty),
// autoConfirm forced off (the operator confirms after loading, matching the
// side-cycle path). Budget exhausted (a retrieve_empty already inbound across the
// loader's cluster) means the seam fires nothing and we surface the familiar
// "already inbound" error.
func (e *Engine) requestEmptyAtManualSwapLoader(
	nodeID int64,
	node *processes.Node,
	claim *processes.NodeClaim,
	payloadCode string,
	reqOrigin ordermgr.Origin,
) (*orders.Order, error) {
	{
		// Resolve the loader from the Core aggregate — the SAME read-model the
		// demand/threshold path uses — so the never-2N seam locks on the loader_key
		// token, the identity every entry point now shares. Pre-cutover this built a
		// throwaway single-window loader keyed on the node NAME, so the operator path
		// and the automatic path locked different mutexes (BUG-1). LoaderAt resolves a
		// manual_swap node via Contains (window or position).
		dl, lerr := e.loaders().LoaderAt(domain.NodeID(node.CoreNodeName), domain.RoleProduce)
		if lerr != nil || dl == nil {
			return nil, fmt.Errorf("node %s: not a configured loader: %w", node.Name, lerr)
		}
		// member = the operator's node. A dedicated loader routes the empty to that
		// specific position; a shared loader IGNORES member (ReservationTarget) and the
		// seam stages at a free window — the deliberate behaviour choice (see the impl
		// log): consistent with the shared multi-window model, where an empty may go to
		// any free window. The InboundSource is the aggregate's (== the old claim's).
		var created *orders.Order
		n, rerr := e.withLoaderBudget(dl, domain.PayloadCode(payloadCode), 1, domain.NodeID(node.CoreNodeName), true, func(deliveryNodes []string) (int, error) {
			made := 0
			for _, deliveryNode := range deliveryNodes {
				// autoConfirm=false, skipAutoConfirm=true: a manual_swap loader needs the
				// OPERATOR to confirm after physically loading the bin. claim.AutoConfirm is
				// true on these claims (mandatory for the robot-drop signal), but that flag
				// means "robot confirms it dropped the bin", not "operator confirmed they
				// loaded parts". Auto-confirming here fires L2/U2 (move back to supermarket)
				// before the operator has finished. Matches the automatic side-cycle paths
				// (MaybeCreateUnloaderFullIn and the loader push).
				order, cerr := e.orderMgr.CreateRetrieveOrder(
					&nodeID, true, 1, deliveryNode, dl.InboundSource(), "",
					"standard", payloadCode, false, true, reqOrigin,
				)
				if cerr != nil {
					return made, cerr
				}
				created = order
				if uerr := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); uerr != nil {
					log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, uerr)
				}
				made++
			}
			return made, nil
		})
		if rerr != nil {
			return nil, fmt.Errorf("node %s: request empty: %w", node.Name, rerr)
		}
		if n == 0 || created == nil {
			return nil, fmt.Errorf("node %s: an empty bin is already inbound", node.Name)
		}
		return created, nil
	}
}

// RequestFullBin requests a full bin of the given payload to be delivered to a
// manual_swap consume node. Core queues the order if no full bins of that
// payload are available. Routed through the reservation seam, so it shares the
// unloader's never-2N budget with the automatic U1 path.
func (e *Engine) RequestFullBin(nodeID int64, payloadCode string) (*orders.Order, error) {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return nil, err
	}
	if err := requireLoaderClaim(node, claim); err != nil {
		return nil, err
	}
	if claim.Role != protocol.ClaimRoleConsume {
		return nil, fmt.Errorf("node %s: only consume nodes request full bins", node.Name)
	}
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	// Same for a full-carrier request on the consume side: the press asking to
	// be fed is cell demand, and it had no episode either.
	reqOrigin := e.operatorRequestOrigin(node, claim, runtime.RemainingUOPCached, 1)

	// Validate payload code against the loader's Core-owned payload set — same
	// aggregate-first resolution as the produce side (see loadablePayloads), so a
	// consume loader's board and gate agree on what may be requested.
	if payloadCode == "" {
		return nil, fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
		return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}

	// Route through the reservation seam, mirroring the produce-side manual_swap
	// branch of RequestEmptyBin. Before this, RequestFullBin created its order
	// directly — no budget, no in-flight count, no occupancy — so an unloader
	// could be sent a second full while the first was still inbound, and the
	// never-2N invariant did not cover the path at all. The seam counts fulls
	// (retrieveEmpty=false) across the unloader's delivery set under the loader's
	// mutex, so this button and the automatic U1 now contend on one lock instead
	// of racing.
	//
	// member = the operator's node. A dedicated unloader routes the full to that
	// position; a shared unloader ignores member and the seam assigns a FREE
	// window — same rule as the produce side, and it means a full is no longer
	// sent to a window that already holds one.
	//
	// Source is the unloader's inbound source (the FG supermarket it pulls from;
	// without it Core's planTransport falls back to global FIFO and can pull from
	// the wrong supermarket, the shape of Hopkinsville 2026-05-14). The loader
	// answers, not the claim: at a node Core's loader set names, claimAtNode
	// already returns the loader's own claim, and a stored claim's
	// inbound_source, authored on another screen with no cross-check, is not
	// read. Pinned by TestRequestFullBin_PullsFromTheUnloadersInboundSource.
	dl, lerr := e.loaders().LoaderAt(domain.NodeID(node.CoreNodeName), domain.RoleConsume)
	if lerr != nil {
		return nil, fmt.Errorf("node %s: resolve unloader: %w", node.Name, lerr)
	}
	if dl == nil {
		return nil, fmt.Errorf("node %s: not a configured unloader", node.Name)
	}
	// manual_swap unloader requires operator confirmation: U1 must not
	// auto-confirm, or U2 fires before the operator has processed the bin.
	const autoConfirm = false
	var created *orders.Order
	n, rerr := e.withLoaderBudget(dl, domain.PayloadCode(payloadCode), 1, domain.NodeID(node.CoreNodeName), false, func(deliveryNodes []string) (int, error) {
		made := 0
		for _, deliveryNode := range deliveryNodes {
			order, cerr := e.orderMgr.CreateRetrieveOrder(
				&nodeID, false, 1, deliveryNode, dl.InboundSource(), "",
				"standard", payloadCode, autoConfirm, true, reqOrigin,
			)
			if cerr != nil {
				return made, cerr
			}
			created = order
			if uerr := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); uerr != nil {
				log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, uerr)
			}
			made++
		}
		return made, nil
	})
	if rerr != nil {
		return nil, fmt.Errorf("node %s: request full: %w", node.Name, rerr)
	}
	if n == 0 || created == nil {
		return nil, fmt.Errorf("node %s: a full bin is already inbound", node.Name)
	}
	return created, nil
}

// stationTakesLoadDirective asks the Core-owned loader a node belongs to whether
// a changeover commandeers its card. A clean miss is false.
func (e *Engine) stationTakesLoadDirective(coreNodeName string) bool {
	if coreNodeName == "" {
		return false
	}
	l, err := e.loaders().LoaderForNode(domain.NodeID(coreNodeName))
	if err != nil || l == nil {
		return false
	}
	return l.ChangeoverLoadDirective()
}

// loadOntoLabelledCarrier answers a LOAD at a window whose carrier already
// carries a label: nil when the load is in fact done, else the refusal that
// names what frees the window.
func (e *Engine) loadOntoLabelledCarrier(node *processes.Node, claim *processes.NodeClaim, b protocol.NodeBinsRow) error {
	// A LABELLED CARRIER AT 0 has no outbound move coming: Core will not send a
	// carrier with no units, and LOAD will not fill one with a label. The
	// operator clears it, then loads (row L item 5). (Not stocked: at or below
	// 0, as a ledger can go negative.) With units it is stock, and waits.
	if !b.Stocked {
		return fmt.Errorf("bin at node %s is labelled %s but holds 0 — confirm it is empty, CLEAR it, then load", node.Name, b.PayloadCode)
	}
	// A LOAD WHOSE REPLY WAS LOST. Core loaded the carrier and the reply timed
	// out, so the Edge never filed the loader's outbound move; the retry lands
	// here, and a refusal naming "wait for outbound move" waits for a move
	// nothing will file (Lane 0 calibration run, PLK_X2). A carrier of a part
	// this loader loads, with units, at a window that owes no outbound, is that
	// carrier: file its outbound now. fileLoadedOutbound owns the owed-move
	// guard, so a window whose outbound is already filed still refuses.
	if slices.Contains(e.loadablePayloads(node, claim), b.PayloadCode) &&
		e.fileLoadedOutbound(node, claim, b.PayloadCode, b.BinID) {
		log.Printf("bin_ops: LOAD at %s found %s already loaded with no outbound owed (a lost reply); outbound filed",
			node.Name, b.PayloadCode)
		return nil
	}
	return fmt.Errorf("bin at node %s already loaded with %s — wait for outbound move", node.Name, b.PayloadCode)
}
