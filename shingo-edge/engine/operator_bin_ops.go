package engine

import (
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
func (e *Engine) operatorRequestOrigin(node *processes.Node, claim *processes.NodeClaim, remaining int) ordermgr.Origin {
	// Discretionary means the operator asked while still above the reorder
	// point — a pull they chose rather than one the level forced. Same
	// predicate as the produce and station paths so the flag means one thing.
	discretionary := claim != nil && claim.ReorderPoint > 0 && remaining > claim.ReorderPoint

	originID, _, err := e.openCellEpisode(
		node.ProcessID, claim,
		protocol.EpisodeTriggerOperator,
		1, // one press, one order
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
func (e *Engine) requestEmptyOrigin(node *processes.Node, claim *processes.NodeClaim, cachedUOP int) ordermgr.Origin {
	if o := e.changeoverLoadOrigin(node, claim); o.ID != "" {
		return o
	}
	return e.operatorRequestOrigin(node, claim, cachedUOP)
}

// loadablePayloads returns the payload codes an operator may load or request at
// this manual_swap loader node. Post-cutover the loader's payload set is
// Core-owned, so this resolves it from the aggregate — the SAME resolver the
// operator board uses in station_service — so the board and this gate never
// disagree: the operator must be able to load every payload their board offers.
//
// It is deliberately NOT gated by which style is active — a loader responds to
// what is called for, not the running style: a shared loader (e.g. SNF2 + SNF3)
// must accept either cell's payload, and an operator-driven loader stages ahead
// for upcoming styles. The aggregate's set is style-agnostic by construction.
//
// Fallback order keeps the operator from being stranded with zero loadable cards
// when the loader isn't (yet) in the aggregate — legacy / not-migrated nodes use
// the per-style edge claim union (PayloadsForLoader), then the claim's own list,
// and a union read error fails open to the active claim. This is the pre-cutover
// behaviour, preserved only for the non-aggregate path.
//
// RESOLVED BY NODE, NOT BY THE CLAIM'S ROLE. Which role a loader is, is Core's
// fact, and asking the aggregate for it through the stored claim's copy is how a
// live loader becomes invisible: a claim left saying "produce" over a loader Core
// now runs as consume misses the lookup entirely, and this falls all the way back
// to the retired claim's payload list. LoaderForNode cannot be ambiguous —
// bin_loader_homes is UNIQUE on position_node_id, so a node belongs to exactly one
// loader whatever its role. The claim's role still keys the legacy union below,
// which is a query over stored claims and is the right key for that.
func (e *Engine) loadablePayloads(node *processes.Node, claim *processes.NodeClaim) []string {
	if l, err := e.loaders().LoaderForNode(domain.NodeID(node.CoreNodeName)); err == nil && l != nil {
		// Scoped to THIS node: a dedicated home loads only its own pinned payload,
		// not the loader's other positions' parts; a shared window loads the whole set.
		if codes := l.LoadablePayloadCodesAt(domain.NodeID(node.CoreNodeName)); len(codes) > 0 {
			return codes
		}
	}
	_, all, _, err := processes.PayloadsForLoader(e.db.DB, node.CoreNodeName, claim.Role)
	if err != nil {
		e.logFn("loader: payload union read for %s failed, using active claim: %v", node.CoreNodeName, err)
		return claim.AllowedPayloads()
	}
	if len(all) == 0 {
		return claim.AllowedPayloads()
	}
	return all
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
	// Available() SAYS A URL IS CONFIGURED, NOT THAT CORE IS ANSWERING. Past it,
	// an unreachable Core returns no bins, which used to produce the same
	// refusal as a genuinely empty node: "no bin at node X — request an empty
	// bin first". That is a positive claim about the physical world the Edge did
	// not verify, told to an operator standing in front of the bin, and it
	// prescribes a corrective action that the request path then refuses too
	// (claimOccupancy assumes occupied on the same failure). Refusing is right;
	// saying why is the part that was missing.
	if e.coreClient.Available() {
		bins, reachable, ferr := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		if !reachable {
			return fmt.Errorf("cannot check node %s — Core %s. The bin may well be there; "+
				"this is a read that did not complete, not an empty window",
				node.Name, OccupancyOutcome(reachable, ferr))
		}
		if len(bins) == 0 || !bins[0].Occupied {
			return fmt.Errorf("no bin at node %s — request an empty bin first", node.Name)
		}
		if bins[0].PayloadCode != "" {
			return fmt.Errorf("bin at node %s already loaded with %s — wait for outbound move", node.Name, bins[0].PayloadCode)
		}
	}

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
		items[i] = BinLoadItem{PartNumber: m.PartNumber, Quantity: m.Quantity, Description: m.Description}
	}
	loadResp, err := e.coreClient.LoadBin(&BinLoadRequest{
		NodeName:    node.CoreNodeName,
		PayloadCode: payloadCode,
		UOPCount:    uopCount,
		Manifest:    items,
	})
	if err != nil {
		return fmt.Errorf("load bin: %w", err)
	}

	// CORE'S ANSWER IS THE COUNT, and everything downstream of here uses it
	// rather than the argument. Core resolved the number it actually wrote to
	// the ledger — the operator's when they declared one, the standard pack
	// when they did not — so this is the same value the bin now carries, and
	// seating anything else puts the Edge and the ledger into disagreement
	// about a carrier both of them can see.
	seatedUOP := int64(loadResp.UOPRemaining)

	// The operator's tap on LOAD is the explicit confirmation that the L1
	// retrieve_empty arrived and has been filled. Confirming the L1 here
	// transitions it delivered → confirmed, sends a delivery receipt to Core,
	// and emits the EventOrderCompleted that applyLoaderEmptyIn is wired to —
	// that handler creates the L2 (filled-bin → outbound) move order and
	// updates the loader's runtime. Pre-fix the L1 stayed at `delivered`
	// indefinitely (Core would auto-confirm on its side, but Edge had no
	// continuous status sync) and L2 was created here directly, duplicating the
	// side-cycle handler's responsibility.
	//
	// THE LOADED PART IS HANDED OVER ON THE RUNTIME ROW, before the confirm. The
	// completion runs synchronously inside ConfirmDelivery and already reads this
	// node's runtime row, so applyLoaderEmptyIn takes the L2's part from there
	// instead of asking Core what it just wrote — no second round trip. It is the
	// same write the fallback below makes (seatManuallyLoadedBin), and the
	// operator is the instrument either way. Pinned by
	// TestPinD3a_LoadAfterEcho_ConfirmsTheL1AndFilesOneL2.
	e.recordLinesideCarrier(node.ID, node.CoreNodeName,
		domain.KnownCarrier(domain.LinesidePayloadCode(payloadCode)), domain.CarrierFromOperator)
	if l1ID, l1Confirmed := e.confirmDeliveredAt(node.CoreNodeName, true, seatedUOP); l1Confirmed {
		log.Printf("bin_ops: confirmed L1 order %d on operator load at node %d", l1ID, nodeID)
		// Belt-and-suspenders: set active_bin_id directly from Core's LoadBin
		// response. The L1-completion path will also try to set it
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
		return nil
	}

	// Fallback: no L1 in flight (e.g. operator loaded a bin that was placed
	// at the loader manually rather than via a retrieve_empty). Set runtime
	// and create L2 directly so the bin still gets dispatched to outbound.
	// active_bin_id comes from Core's LoadBin response — that's the
	// authoritative pointer to the bin physically at this slot, regardless
	// of whether any order was tracking it.
	// claim.ID is 0 for a synthesized Core-loader claim (no style_node_claim row);
	// pass nil rather than a 0 foreign key into the runtime's active_claim_id.
	var claimIDPtr *int64
	if claim.ID != 0 {
		claimIDPtr = &claim.ID
	}
	var activeBinID *int64
	var deltaEpoch int64
	if loadResp != nil && loadResp.BinID > 0 {
		v := loadResp.BinID
		activeBinID = &v
		deltaEpoch = loadResp.DeltaEpoch
	}
	e.seatManuallyLoadedBin(node, claimIDPtr, activeBinID, deltaEpoch, int(seatedUOP), payloadCode)
	// L2 to OutboundDestination is unattended (supermarket node) — must
	// auto-confirm or it sticks at `delivered` forever. See the same reasoning in
	// applyLoaderEmptyIn. Thread the operator-selected payloadCode through so the
	// order tile in operator-station renders IN_TRANSIT against the correct
	// payload card (claim's primary payload would mis-bind on multi-payload
	// loaders).
	//
	// THROUGH THE SHARED GUARD. "No L1 in flight" is a question about a moment;
	// whether this slot already owes an outbound move is a question about the
	// world, and only the second one is safe to create an order on. Two callers
	// racing this branch and applyLoaderEmptyIn is what doubled every outbound
	// move on the lane-stress rig — see loader_outbound_guard.go.
	//
	// THE AGGREGATE WINS, THE CLAIM IS THE FALLBACK — byte-for-byte the shape
	// applyLoaderEmptyIn already uses, and RoleProduce for the same reason it
	// does: these two are the only creators of this one L2 move, so a different
	// resolution here is a way for them to send the same carrier to two places.
	// They could: this read was the claim's alone, so a loader whose outbound was
	// edited in Core — or retired and recreated — routed the LOAD's move to the
	// old destination and the L1-completion's move to the new one.
	//
	// A consume window reaching LoadBin (the modal's delivered-card tap) misses
	// the produce lookup and keeps the claim's outbound, exactly as today.
	//
	// outboundFor also refuses a blank or same-node outbound here, as the other
	// two creators always did; this door alone used to file a move from the
	// window to itself.
	if outbound, ok := e.outboundFor(node, claim, domain.RoleProduce); ok {
		if orderID, created := e.createLoaderOutbound(nodeID, node.CoreNodeName, outbound, payloadCode, "load-fallback"); created {
			if err := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &orderID); err != nil {
				log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
			}
		}
	}

	// CLEAR-ON-LOAD is the NORMAL end of a supply refusal. The parts arrived,
	// the operator loads them, and the card goes back to normal without anyone
	// having to remember to undo anything.
	//
	// It has to be explicit rather than left to the order going terminal: the
	// order lags the load, and in that gap the card would still read REFUSED
	// about material the operator is standing there holding. They just fixed it;
	// the screen should say so.
	//
	// Best-effort and last: a failed delete must not fail the load, and a stale
	// refusal is visible and undoable, where a lost load is neither.
	if err := e.db.DeleteSupplyRefusal(node.CoreNodeName, payloadCode); err != nil {
		log.Printf("bin_ops: clear supply refusal on load at %s: %v", node.CoreNodeName, err)
	}

	return nil
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
// what the U2 needs from that record is captured into a local first (hadBin), so the
// ordering bought nothing and cost a race: between the create and the
// clear, the bin Core hands the U2 still carries its payload, and a mover that reads it
// in that window carries a labelled carrier to the empty-totes destination. Creating it
// after the clear commits means the carrier a U2 ever names is already empty.
//
// It is still gated on a bin actually being present at the tap, so clearing an
// already-empty window creates nothing — and it still fires for EVERY consume drain,
// not just AMR-fed ones, which was the reason the create sat on the CLEAR in the first
// place. That property is preserved by capturing hadBin before the clear rather than
// re-reading the window after it.
//
// Post-clear, if the claim has AutoPush enabled, MaybePushUnloader offers the next pull
// to the reservation seam — a no-inbound drain is gated there too, so it's a no-op.
// ClearBin clears the bin at the consume-unloader window. binTypeCode is the
// dunnage type the operator selected at the confirm tap; empty string means no
// change to the carrier's bin_type_id (existing behaviour for all other callers).
func (e *Engine) ClearBin(nodeID int64, binTypeCode string) error {
	node, runtime, claim, err := e.loadActiveNode(nodeID)
	if err != nil {
		return err
	}
	if err := requireLoaderClaim(node, claim); err != nil {
		return err
	}
	// Capture the bin in the window BEFORE confirm/clear, while Core's manifest is
	// still coherent. hadBin gates the empty-out so clearing an already-empty window
	// creates nothing; clearedPayload is recorded in the CLEAR log line below. It does
	// NOT ride the empty-out — see createUnloaderEmptyOut for why the U2 names no part.
	var clearedPayload string
	var hadBin bool
	if claim.Role == protocol.ClaimRoleConsume {
		if bins, _, _ := e.coreClient.FetchNodeBins([]string{node.CoreNodeName}); len(bins) > 0 && bins[0].Occupied {
			clearedPayload = bins[0].PayloadCode
			hadBin = true
		}
		// Confirm any AMR-fed inbound (U1) — the operator's CLEAR tap IS the receipt
		// ack. A press/forklift-fed drain has no U1; the helper returns ok=false and
		// we proceed to the empty-out regardless (it no longer depends on a U1).
		if u1ID, ok := e.confirmDeliveredAt(node.CoreNodeName, false, 0); ok {
			log.Printf("bin_ops: confirmed U1 order %d on operator clear at node %s", u1ID, node.CoreNodeName)
		}
	}
	cleared, err := e.coreClient.ClearBin(node.CoreNodeName, binTypeCode)
	if err != nil {
		return fmt.Errorf("clear bin: %w", err)
	}
	// Empty-out (U2): send the now-empty bin to the unloader's outbound (empty
	// totes). Fired off the CLEAR — so it runs for every consume drain, not just
	// ones an AMR fed — but only after the clear has COMMITTED on Core, so the
	// carrier the U2 names is empty on Core's side too and not merely about to be.
	// Gated on hadBin, captured above while the window still held the bin.
	if claim.Role == protocol.ClaimRoleConsume && hadBin {
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
		if existingID, inFlight, lerr := e.emptyOutInFlight(nodeID); lerr != nil {
			log.Printf("bin_ops: check in-flight move for node %s: %v", node.Name, lerr)
		} else if inFlight {
			log.Printf("bin_ops: skipping empty-out at node %s — order %d is already moving this carrier out",
				node.Name, existingID)
		} else {
			e.createUnloaderEmptyOut(node, claim)
		}
	}
	// claim.ID is 0 for a synthesized Core-loader claim — pass nil, not a 0 FK.
	var claimIDPtr *int64
	if claim.ID != 0 {
		claimIDPtr = &claim.ID
	}
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
	// The count that just went away, on the Edge side of the wire. Core records
	// it durably (ClearForReuseTx writes a bin_uop_ledger row with before->0 and
	// op clear_for_reuse, in the clear's own transaction); this line is what
	// lets somebody reading the Edge log find that row, and it names the number
	// so a clear of a bin that was not empty is visible from either end.
	discarded := 0
	if runtime != nil {
		discarded = runtime.RemainingUOPCached
	}
	log.Printf("bin_ops: CLEAR at node %s discarded %d parts on bin %d (payload=%q, new epoch=%d) — "+
		"Core holds the ledger row (clear_for_reuse)",
		node.CoreNodeName, discarded, cleared.BinID, clearedPayload, cleared.DeltaEpoch)
	// Push-driven unloader: bin just left the window, fire the next pull.
	// Gated inside MaybePushUnloader so non-push claims are no-ops.
	if claim.Role == protocol.ClaimRoleConsume && claim.AutoPush {
		e.MaybePushUnloader(nodeID)
	}
	// Push-driven loader (transitional): the operator cleared the window, so
	// stage the next empty. Gated inside MaybePushLoader on transitional.
	if claim.Role == protocol.ClaimRoleProduce {
		e.MaybePushLoader(nodeID)
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
	// FAILS CLOSED, AND THAT IS CORRECT — no bins means refuse, never "push
	// anyway". What was wrong was the sentence: an unreachable Core also returns
	// no bins, and the operator was told the window is empty on a read nobody
	// completed. Same decision, a message that says what happened.
	bins, reachable, ferr := e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
	if !reachable {
		return fmt.Errorf("cannot check node %s — Core %s. The carrier may still be there; "+
			"nothing was pushed", node.Name, OccupancyOutcome(reachable, ferr))
	}
	if len(bins) == 0 || !bins[0].Occupied {
		return fmt.Errorf("node %s has no bin to push", node.Name)
	}
	// Only push if the bin is actually empty (no payload). A full bin sitting
	// in the window means the operator confirmed earlier but the AMR is still
	// staging — don't evict it.
	if bins[0].PayloadCode != "" {
		return fmt.Errorf("node %s bin is not empty (payload=%q)", node.Name, bins[0].PayloadCode)
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
	// See createUnloaderEmptyOut.
	e.createUnloaderEmptyOut(node, claim)
	// Re-arm the delivery seam so the next full bin is requested after the
	// empty departs — mirrors ClearBin's gated MaybePushUnloader at its tail.
	if claim.AutoPush {
		e.MaybePushUnloader(nodeID)
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
// tag. lookupPayloadMeta does not backfill a manual_swap claim, so "" reaches the
// wire. The operator board attributes the move by source_node instead (cardModel
// in operator-window-state.js, and the modal's demand queue). Pinned by
// TestEmptyOut_EnvelopeNamesNoPart and, Core side,
// TestPayloadlessEmptyOut_SourcesAResidentThePartCouldNotCarry.
func (e *Engine) createUnloaderEmptyOut(node *processes.Node, claim *processes.NodeClaim) {
	outbound, ok := e.outboundFor(node, claim, domain.RoleConsume)
	if !ok {
		return
	}
	nodeID := node.ID
	// NoDemand: the side-cycle's empty-out is the system's own consequence of a
	// clear, not a demand anybody expressed.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&nodeID, 1, node.CoreNodeName, outbound, "", true,
		ordermgr.NoDemand())
	if err != nil {
		e.logFn("side-cycle: create U2 (empty-out) for unloader %s: %v", node.Name, err)
		return
	}
	log.Printf("side-cycle: U2 (empty-out) order %d for unloader %s → %s (names no part)", order.ID, node.Name, outbound)
	// Point the runtime active order at U2 so the unloader UI shows the empty-out next.
	// (ClearBin's SetClaimAndCount zeroes the count/claim but leaves this pointer.)
	if err := e.db.SetProcessNodeRuntimeActiveOrder(node.ID, &order.ID); err != nil {
		log.Printf("side-cycle: update runtime orders for unloader %d: %v", node.ID, err)
	}
}

// RequestEmptyBin delivers an empty bin to a produce node. Manual_swap and
// simple modes issue a single retrieve order; multi-step modes (single_robot,
// two_robot, two_robot_press_index, sequential) reuse the swap dispatch so
// the robot choreography is identical to a Finalize swap — empties move
// through the same multi-stop trip a full bin would. Returns the primary
// order; the second leg (R2) is tracked on the runtime row.
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
	if ok, reason := e.CanAcceptOrders(nodeID); !ok {
		return nil, fmt.Errorf("node %s unavailable: %s", node.Name, reason)
	}

	reqOrigin := e.requestEmptyOrigin(node, claim, runtime.RemainingUOPCached)

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
	//   - simple / multi-step (press swap) nodes: the empty rides the same robot
	//     choreography as the part it precedes, so a payload is still required.
	//
	// Validation and routing are ONE decision, asked once: a loader validates
	// loosely and routes to the per-loader reservation seam — the same seam the
	// demand and threshold paths use — while every other mode validates strictly
	// and routes to the swap seam. They were two consecutive branches on the same
	// predicate, which read as though a claim could answer them differently.
	if claim.IsLoaderNode() {
		if payloadCode != "" && !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
			return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
		}
		return e.requestEmptyAtManualSwapLoader(nodeID, node, claim, payloadCode, reqOrigin)
	}

	if payloadCode == "" {
		return nil, fmt.Errorf("no payload code specified")
	}
	if !slices.Contains(e.loadablePayloads(node, claim), payloadCode) {
		return nil, fmt.Errorf("payload %q not in allowed list for node %s", payloadCode, node.Name)
	}
	return e.requestEmptyForSwapModes(nodeID, node, runtime, claim, payloadCode, reqOrigin)
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

// requestEmptyForSwapModes is RequestEmptyBin's non-manual_swap arm: the simple and
// multi-step (press swap) modes, where the empty rides the same robot choreography
// as the part it precedes.
//
// Split from the manual_swap arm above for the funlen ceiling — see that function
// for why the arm moved rather than being shaved. The two are genuinely different
// mechanisms that shared only a name: one reserves through the per-loader never-2N
// seam, the other guards a single physical slot and builds a swap dispatch.
func (e *Engine) requestEmptyForSwapModes(
	nodeID int64,
	node *processes.Node,
	runtime *processes.RuntimeState,
	claim *processes.NodeClaim,
	payloadCode string,
	reqOrigin ordermgr.Origin,
) (*orders.Order, error) {
	// Anti-spam for simple / multi-step modes (manual_swap is handled above via
	// the reservation seam): one physical slot, so reject a second request while a
	// retrieve_empty is already non-terminal at this CORE NODE (delivery_node, not
	// process_node — a shared node has many process_node rows for one slot; see
	// [[shingo_manual_swap_core_node_scoping]]). The board greys its request button
	// the instant a request fires; this is belt-and-suspenders for double-tap races
	// and direct API callers. Fail closed on a read error.
	inFlightEmpties, err := e.countActiveOrdersAtNode(node.CoreNodeName, func(o orders.Order) bool {
		return o.RetrieveEmpty
	})
	if err != nil {
		return nil, fmt.Errorf("node %s: check in-flight empties: %w", node.Name, err)
	}
	if inFlightEmpties > 0 {
		return nil, fmt.Errorf("node %s: an empty bin is already inbound", node.Name)
	}

	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm

	// ── THE SECOND DOOR ONTO A PRESS-INDEX SWAP ─────────────────────────
	//
	// The partial-empty prime lives in BuildProducePlan, which is REQUEST SWAP's
	// planner. This is REQUEST EMPTY BIN, and it reaches BuildSwapDispatch
	// directly — so a cell whose on-deck position is bare minted the full
	// two-leg swap here with no guard at all, and the index leg opened with a
	// pickup at a position holding nothing. Core cannot reserve a bin that is
	// not there: the leg parks in `sourcing` forever, the release gate refuses
	// its sibling for a collision that will never clear, and the operator
	// cancels a pair that never had a chance. Springfield PLN_004, cancelled
	// eight times on 2026-08-26 without one completed cycle.
	//
	// This is the button an operator reaches for when they are LOOKING at an
	// empty position, so it is the door that most needs the guard, not the one
	// that could go without it.
	//
	// Same reads, same lock, same order shape as the produce path — see
	// primeBarePressIndexPositions. A prime is a legitimate answer to "request
	// an empty bin": it puts an empty carrier exactly where the operator can
	// see one is missing, and the next press runs the swap against a full cell.
	if primes, suppressed, perr := e.primeBarePressIndexPositions(node, claim, reqOrigin); suppressed {
		if perr != nil {
			return nil, perr
		}
		return primes[0], nil
	} else if perr != nil {
		return nil, perr
	}

	// Multi-step swap modes reuse the same dispatch the consume side uses on
	// RequestNodeMaterial / produce uses on Finalize. Robots execute the same
	// choreography for empty and full bins; the order shape doesn't depend
	// on contents.
	// See swap_evac_dest.go: the outgoing carrier goes to ITS home, not the
	// requested style's. Blank override = today's behaviour.
	dispatch, err := BuildSwapDispatch(node, withResidentEvacDest(claim, e.residentEvacDest(runtime, claim)))
	if err != nil {
		return nil, err
	}
	if dispatch != nil {
		if dispatch.RequiresActiveSwapGuard {
			if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
				return nil, err
			}
		}
		// NO DRY-SOURCE GUARD ON THIS DOOR, and not by omission. The consume door
		// refuses to arm a pair into a payload Core has no bin of
		// (guardSourceKnownDry); this door asks for an EMPTY carrier, and the
		// preflight counts bins of a payload — it cannot say whether empties
		// exist. Guarded, this door would always let the request through, and a
		// check that never refuses is a comment that runs.
		// reqOrigin, not Origin{}. The episode was opened at the top of this
		// method precisely so the orders it creates could name what caused
		// them, and then the multi-step arm dropped it on the floor while the
		// manual_swap arm four screens up carried it. Both legs of one swap
		// belong to the operator's one request: an R2 that names no demand is
		// an orphan by construction, and a service dig raised for it cannot
		// look up who is collecting its target.
		//
		// Both uuids before either create, as at the consume door and for the
		// same reason: minted inside the create, leg A went to Core unpaired.
		uuidA, uuidB := ordermgr.NewOrderUUID(), ""
		if dispatch.StepsB != nil {
			uuidB = ordermgr.NewOrderUUID()
		}
		sibA, sibB := coreSiblings(dispatch.StepsA, dispatch.StepsB, uuidA, uuidB)
		orderA, err := e.dispatchPairedLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, sibA, uuidA, reqOrigin)
		if err != nil {
			return nil, err
		}
		var orderB *orders.Order
		if dispatch.StepsB != nil {
			orderB, err = e.dispatchPairedLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, sibB, uuidB, reqOrigin)
			if err != nil {
				return nil, err
			}
		}
		var orderBID *int64
		if orderB != nil {
			orderBID = &orderB.ID
		}
		if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, orderBID); err != nil {
			log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
		}
		if orderB != nil {
			// Return-error on failure: see comment in
			// operator_stations.go:LinkOrderSiblings call site.
			if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
				return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
			}
		}
		return orderA, nil
	}

	// Simple mode: single retrieve (manual_swap returned above via the seam; the
	// multi-step modes returned in the dispatch branch). Core queues if no empty
	// is immediately available.
	//
	// Source group is the loader's claim.InboundSource (the supermarket the
	// operator is configured to pull empties from). Without this, Core's
	// planTransport falls back to a global FIFO scan and can return a
	// payload-matching empty bin from anywhere — including the empty-tote
	// return area (Hopkinsville, 2026-05-14, Mission #51 pulled SMN_07
	// instead of from Supermarket Area).
	order, err := e.orderMgr.CreateRetrieveOrder(
		&nodeID, true, 1, node.CoreNodeName, claim.InboundSource, "",
		"standard", payloadCode, autoConfirm, false, reqOrigin,
	)
	if err != nil {
		return nil, err
	}
	if err := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); err != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, err)
	}
	return order, nil
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
	reqOrigin := e.operatorRequestOrigin(node, claim, runtime.RemainingUOPCached)

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
	// Source stays claim.InboundSource (the FG supermarket the unloader pulls
	// from; without it Core's planTransport falls back to global FIFO and can pull
	// from the wrong supermarket).
	//
	// THE CLAIM WINS HERE, AND IT IS NOT EQUAL TO THE AGGREGATE. This comment used
	// to say the two were "believed equal — the produce branch uses it". They are
	// not, and the belief was never checked. style_node_claims.inbound_source and
	// bin_loaders' are authored on different screens with no cross-check between
	// them, so nothing makes them agree. Driven against a real store: a stored
	// consume claim carrying OLD-FG-MARKET beside a live loader carrying
	// NEW-FG-MARKET resolves the stored claim, and the order this creates carries
	// OLD-FG-MARKET while `dl` two lines below says NEW.
	//
	// They ARE equal for a SYNTHESIZED claim, which copies the aggregate's value —
	// which is why the produce branch gets away with reading the aggregate and
	// this one would not. So the question is whether a STORED claim can still
	// reach this line, and it can, by two paths:
	//
	//   1. Before the first node-list sync of a boot. The quarantine that removes
	//      stored loader claims runs only in SetCoreLoaders, so a legacy row
	//      survives startup, migration and every operator tap until a node-list
	//      RESPONSE arrives. Unbounded when Core never answers — which is a
	//      condition this plant has been in.
	//   2. CloneStyle / GenerateStyles. cloneStyleTx copies swap_mode with a raw
	//      INSERT that never sees the upsert allowlist, so cloning a style that
	//      still carries a loader claim mints another one. See styles.go.
	//
	// SO FLIPPING THIS TO PREFER THE AGGREGATE IS A LIVE ROUTING CHANGE, not a
	// severing: it would move which supermarket a real unloader pulls from at any
	// plant holding such a row. A blank or wrong source here is the shape of
	// Hopkinsville 2026-05-14 — planTransport falls back to a global FIFO scan
	// and pulls from the wrong market. That decision wants plant data behind it,
	// so it is stated rather than taken.
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
				&nodeID, false, 1, deliveryNode, claim.InboundSource, "",
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
