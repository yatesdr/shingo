package dispatch

import (
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_place.go — the PARK side of the dedicated home loader, the inverse of the
// source side (loader_source.go). When a dedicated-loader changeover returns a bin
// from a home position, Core decides where it lands: its HOME if provably free,
// else another free home pinned to its part, else a free buffer slot
// (home_kind='buffer', the M1 representation the source side
// already pools). Source and park use the SAME Core representation, so a parked
// partial is re-sourced by the same pool and the loop closes end-to-end.
//
// THE THIRD BRANCH IS A WAIT, not a drain. No code in dispatch/ reads
// bin_loaders.outbound_dest, and a produce loader has none to read: fulls leave by
// order, not to a configured pool. When neither home nor buffer is free, an order
// already pointed at some OTHER node keeps it; an order whose only destination is
// the home just found unavailable is QUEUED (CauseLoaderParkNoSlot) and re-placed
// every scanner pass until a home or buffer frees. It used to be dispatched onto
// the occupied home, which delivered two carriers onto occupied homes at SMN_016
// and SMN_035 on 2026-08-26 and faulted AMR-10 at SMN_034 on 2026-09-25.
//
// BEFORE THE WAIT, THE VACATED SLOT. A member — home or buffer — whose only bin
// the partner's committed lift takes first is a destination too
// (placeVacated, vacated_slot.go). SPR 2026-09-28, pairs 7060/7061 and 7062/7063:
// the supply was widened onto a buffer partial, every buffer was full, and the
// return parked every pass beside the buffer being emptied. Order: another home
// of the part, free buffer, drain, vacated, wait. The release fence holds the
// drop until the lift is real.
//
// LOCUS — Core is the single authority. The Edge ships the evac order with
// DeliveryNode="" and holds no authoritative bin-landing record; Core resolves the
// dropoff here and moves it in the plan the pass sends and the stored one
// (setParkDestination, through repoint), so the create carries it. Divergence-free.
//
// NEVER-2N — the park's placement MUST consult the Core in-flight authority (the
// SAME order-truth restock gates on, the in-flight delivery-node counts /
// planning_service.go's CheckDropoffCapacity) — NEVER a bespoke count. Committing
// DeliveryNode=home does NOT by itself make this order in-flight: in-flight means
// holding a claimed bin (orders.InFlightForDropoffSQL), and placement runs before
// the sources are claimed. The order becomes in-flight to the home when its claim
// lands later in the same pass, so a later restock's own gate sees it and yields —
// and a pair that parks after placing gives the claim back and is not in-flight
// again until it next claims. If a restock got there first, this read sees it and
// yields to buffer. A lost race requeues (scanner replay), the same contract every
// dropoff has. Do NOT add ClaimSlot here.
//
// The home occupancy check is split by LEG ROLE, read from the leg's own steps
// (legReturnsToHome). It used to be split on "does the plan contain a wait step",
// which is a proxy, and at Springfield the proxy is simply wrong — every return leg
// carries a station wait, so every return read as a supply leg. See
// legReturnsToHome for what that cost.
//
//   - Return legs whose home holds nothing, or holds only the carrier this swap is
//     itself lifting (homeClearForReturn): in-flight orders only. The bin standing
//     there is the one leaving, so counting it would force buffer incorrectly and
//     hand the home to the replenishment loop the moment it clears.
//
//   - Everything else — supply legs, unreadable shapes, and return legs whose home
//     holds a carrier nobody is coming for: full CheckDropoffCapacity, both physical
//     bins and in-flight orders. A leg routed onto a home that really is occupied
//     faults the robot on arrival.
//
// The buffer read is always full CheckDropoffCapacity — a buffer legitimately holds
// a parked partial, so its physical occupancy is real and must block.
//
// ANOTHER ORDER'S RESERVATION OR HARD CLAIM takes a member out at every choose
// point, as it does for every other chooser: the claim door refuses it, so
// picking it held the order at the door on every pass, and on a member with no
// door (neither storage-classed nor exclusive) the order was dispatched onto it.
// It is read once per placement, for the whole loader (LoaderMembersForPlacement,
// from nodes.SlotHeldByStrangerSQL). A live order that only NAMES a member is
// not a hold here: for a loader the first to claim goes (CheckDropoffCapacity),
// see the 2026-08-26 chain at legReturnsToHome.
//
// AND BEFORE ANY OF THAT, THE PAYLOAD QUESTION. Everything above answers "is there
// room?"; a pinned home also asks "room for WHAT?". Both patterns check the carrier
// against bin_loader_homes.payload_code first (homeAcceptsCarrier), because a home
// can be genuinely, physically empty and still be the wrong place for the bin in
// hand — which is exactly how a 74871 carrier reached SMN_029, pinned 63145, on
// 2026-09-03 and held it for 11h24m. A mismatch routes to the carrier's OWN home,
// else a buffer (placeMismatchedCarrier). This is a BACKSTOP, not the cause: the
// destination is authored on the Edge from the style being requested rather than
// the carrier being lifted, and that is fixed there. Core is simply the only place
// that holds both facts at once, so it is the only place the question can be asked.
//
// Returns the name of the home the order must WAIT for, or "" when placement
// left it with a destination it can drive to. A non-empty return means neither
// the home nor any buffer can take the bin and the only destination left is the
// home just found occupied (for a set-down before the last, the member it was
// pointed at: placeEarlierDrops); the caller queues the order rather than
// dispatching it there (see placeForLoader).
func (d *Dispatcher) placeForDedicatedLoader(order *orders.Order, steps []resolvedStep, pc *pairPass) (waitHome string) {
	// Pattern A: SourceNode is a home position, the plan holds no wait and the leg
	// does not set its cart down at its own process node (legPlacesLineBin), read
	// as a return lifted at its home. No builder's leg is one. A leg that fills its
	// line is a supply and is left to Pattern B, so the Sequential backfill and a
	// staged delivery off a home go to the line (B23, brief H;
	// TestPlaceForDedicatedLoader_SequentialBackfillFromHome_Untouched). The legs
	// it still takes are supplies with no wait lifted at a home that set down off
	// the line, whose drop it moves onto that home: a part change's stage leg, which
	// has no step at the line, and a no-wait leg off a home sent with no process
	// node, which no step builder sends (TestLoaderPark_EveryBuilderLeg's stage
	// and pn=none rows).
	// Pattern B: the last drop (DeliveryNode, or its anchor) is a home position:
	// a return, or a supply to the home.
	// Both fall back to the same buffer/vacated/wait logic (placeForLoader). They
	// differ in their first question: Pattern A asks the in-flight count and its
	// own plan (orderDeliversTo); Pattern B asks a return's home clear
	// (homeClearForReturn) and everything else the physical gate. Only Pattern A
	// can reach the drain (it points at a node other than the home). A set-down
	// before the plan's last is asked first, on its own (placeEarlierDrops, B24,
	// brief H): it used to be asked nothing.
	if wait := d.placeEarlierDrops(order, steps); wait != "" {
		return wait
	}

	// Pattern A DECLINING must fall through to Pattern B, not end placement.
	// Before this was extracted, Pattern A's three lookup failures each
	// `return`ed out of placeForDedicatedLoader entirely, so a leg whose SOURCE
	// is not a dedicated-loader home never reached the delivery-side branch
	// below — Pattern B was unreachable for every such order even when its
	// DeliveryNode WAS a home. tryPlaceFromHomeSource reports whether it took
	// ownership; "false" means "not mine", never "done".
	if order.SourceNode != "" && !hasWaitStep(steps) && !legPlacesLineBin(steps, order.ProcessNode) {
		if owned, wait := d.tryPlaceFromHomeSource(order, steps, pc); owned {
			return wait
		}
	}

	// A RETURN THE VACATED ARM ONCE RE-POINTED RE-DERIVES FROM ITS HOME. The
	// vacated slot it was sent to last pass was a fact about that pass's partner;
	// read as "the home" it would pin the leg to wherever FIFO sent the supply
	// then. Only the vacated arm writes an anchor, so every other placement —
	// including today's stickiness on a buffer — reads DeliveryNode as before.
	target := order.DeliveryNode
	anchored := false
	if f := lastDropIndex(steps); f >= 0 && steps[f].Anchor != "" {
		target, anchored = steps[f].Anchor, true
		// The in-memory plan still names last pass's vacated slot. Whatever this
		// pass decides, the gates and the slot claim after this read the plan, so
		// it follows the destination — and a stamp for a slot it left stops
		// counting. Never-vacated orders are not touched, and keep today's plan.
		defer func() {
			if steps[f].Node != order.DeliveryNode {
				steps[f].Node = order.DeliveryNode
				steps[f].Vacate = nil
			}
		}()
	}

	if target != "" {
		// Pattern B: explicit DeliveryNode is a home position. Two shapes land here:
		//   - Return leg: the robot bringing the line's spent bin back to the home.
		//     In-flight check only, but ONLY once homeClearForReturn has established
		//     that anything standing on the home is this swap's own — see header.
		//   - Supply leg: a fresh bin sourced from a staging/supermarket node
		//     delivering to the home. Full capacity gate, so a physically-occupied
		//     home routes to buffer instead of faulting on arrival.
		destNode, err := d.db.GetNodeByDotName(target)
		if err != nil || destNode == nil {
			return ""
		}
		members, err := d.db.LoaderMembersForPlacement(destNode.ID, order.ID)
		home := memberAt(members, destNode.ID)
		if err != nil || home == nil {
			return ""
		}
		loader, err := d.db.GetLoader(home.LoaderID)
		if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
			return ""
		}
		homeName := destNode.Name
		// PAYLOAD BEFORE CAPACITY, because both arms below only ever answer "is
		// there room?" and room is not the question a pinned home asks.
		//
		// Springfield, 2026-09-03 01:40:40: a SYN-PART09A.06 carrier parked on
		// SMN_029, which is pinned SYN-PART03H.10. Every check passed and each
		// was right on its own terms — the home read CLEAR because the carrier
		// standing on it was this swap's own supply bin, already being lifted by
		// the sibling leg, so in-flight was 0 and nothing was physically there.
		// The home was empty. It just was not empty FOR THIS BIN.
		//
		// It sat there 11h24m. Every replenishment pass logged
		// `held=map[SMN_029:dropoff-occupied] created=0` and nothing else, while
		// lineside 63145 drained 286 → 152 with no refill creatable.
		//
		// The upstream cause is elsewhere (the Edge picked the destination from
		// the style being REQUESTED rather than the carrier being lifted, after a
		// changeover completed with this node abandoned) and is fixed on its own
		// terms. This is the backstop: it is the only place that holds both facts
		// — what the carrier is, and what the home is for — so it is the only
		// place that can answer the question at all.
		pl := placement{steps: steps, pc: pc, anchored: anchored, members: members}
		if carrier, known := d.carrierPayloadFor(order, steps); !homeAcceptsCarrier(home.PayloadCode, carrier, known) {
			return d.placeMismatchedCarrier(order, home.LoaderID, homeName, carrier, pl)
		}
		// A RETURN leg landing on a home whose only occupant this swap is
		// already lifting takes the in-flight check alone: the bin standing
		// there is the one leaving, so reading it as a blocker surrenders the
		// home. Everything else — a supply leg, an unreadable shape, or a home
		// holding a carrier nobody is coming for — takes the full gate, which
		// is the physical question.
		// SAY WHICH ARM, ALWAYS. setParkDestination is idempotent and silent when the
		// destination is already right — and a return leg arrives already pointed at its
		// home, so "held the home" wrote NOTHING while "yielded to buffer" wrote a line.
		// Reading the outcome therefore only ever showed the yields, which is how a sim
		// A/B of this decision came out unreadable: both arms looked like buffer.
		//
		// ANOTHER ORDER'S RESERVATION OR HARD CLAIM ON THE HOME is asked first, in
		// both arms, and sends the leg on to the buffers. The claim door refuses a
		// slot another order holds, so picking it would hold this order at the
		// door on every pass for as long as the other order holds it; and on a
		// node with no door (neither storage-classed nor exclusive) nothing would
		// refuse it at all.
		if home.HeldByStranger {
			d.dbg("place: order %d home %s is held by another order's reservation or hard claim",
				order.ID, homeName)
			return d.placeForLoader(order, home.LoaderID, homeName, pl)
		}
		isReturn := d.legReturnsToHome(order, steps)
		clear := isReturn && d.homeClearForReturn(order, homeName, pc)
		if isReturn && clear {
			inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
			d.dbg("place: order %d return leg, home %s clear (occupant is this swap's own or none) — in-flight %d, err %v",
				order.ID, homeName, inFlight, ierr)
			if ierr == nil && inFlight == 0 {
				d.setParkDestination(order, steps, homeName, "home")
				return ""
			}
			return d.placeForLoader(order, home.LoaderID, homeName, pl)
		}
		blocked, block := CheckDropoffCapacity(d.db, homeName, order.ID)
		d.dbg("place: order %d takes the physical gate on home %s (return=%v, clear=%v) — blocked=%v cause=%s",
			order.ID, homeName, isReturn, clear, blocked, block.Cause)
		if blocked {
			return d.placeForLoader(order, home.LoaderID, homeName, pl)
		}
		d.setParkDestination(order, steps, homeName, "home")
	}
	return ""
}

// placeEarlierDrops asks the park's question of every set-down before the
// plan's last that lands on a dedicated-loader member (B24, brief H): may the
// cart go down there now? It takes the gate a supply to a home takes in Pattern
// B, the physical one (CheckDropoffCapacity), and another order's reservation
// or hard claim. Refused, the set-down moves to the loader's first free buffer
// the plan does not already set down at; with none, the order waits on the
// member, as a last drop does (placeForLoader), and is asked again every pass.
// The plan's last drop is placeForDedicatedLoader's own question. Before this,
// a part change's old cart, a tool change's leg and an index press's Robot 1
// were sent to set down on an occupied home (TestLoaderPark_EveryBuilderLeg's
// tooling, part-change and press-index rows), and the part change's landing
// evicted the record of the spare standing there
// (TestScenario_SequentialLine_PartChangeThroughItsHomes).
//
// A SET-DOWN AT A NODE THE PLAN LIFTS FROM EARLIER IS NOT ASKED, as before. The
// cart standing there is the one the plan lifts first, which the gate cannot
// tell from a stranger's, and a set-down the plan lifts from again later would
// be moved away from that lift (the Manual Order page's order staged at a home:
// TestLoaderPark_TheBriefsRows, "Manual Order page: staged at HOME, delivered
// to the line"; TestLoaderPlacementPicks row 02).
func (d *Dispatcher) placeEarlierDrops(order *orders.Order, steps []resolvedStep) (waitAt string) {
	last := lastDropIndex(steps)
	for i := 0; i < last; i++ {
		if steps[i].Action != protocol.ActionDropoff || steps[i].Node == "" || liftsAtBefore(steps, i) {
			continue
		}
		members, name, refused := d.earlierDropRefused(order, steps[i].Node)
		if !refused {
			continue
		}
		buffer := d.earlierDropBuffer(members, steps, order.ID)
		if buffer == "" {
			d.dbg("place: order %d set-down %d at %s cannot go down and no buffer is free — it waits", order.ID, i, name)
			return name
		}
		if _, err := d.repoint(order, steps, stepMove{at: i, action: protocol.ActionDropoff, node: buffer}, nil); err != nil {
			log.Printf("dispatch: place order %d set-down %d at %s → %s: %v", order.ID, i, name, buffer, err)
			return name
		}
		d.dbg("place: order %d set-down %d at %s cannot go down — moved to buffer %s", order.ID, i, name, buffer)
	}
	return ""
}

// liftsAtBefore reports whether the plan picks up at step i's node before step i.
func liftsAtBefore(steps []resolvedStep, i int) bool {
	for _, s := range steps[:i] {
		if s.Action == protocol.ActionPickup && s.Node == steps[i].Node {
			return true
		}
	}
	return false
}

// earlierDropRefused reports whether a set-down at node may not be made now:
// node is a member of a dedicated-positions loader, and another order holds it
// or the physical gate finds it taken. It returns the loader's members for the
// buffer walk and the member's name. A node that is no such member, or cannot
// be read, is not refused (it was asked nothing before brief H).
func (d *Dispatcher) earlierDropRefused(order *orders.Order, node string) (members []store.LoaderPlacementMember, name string, refused bool) {
	n, err := d.db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return nil, "", false
	}
	members, err = d.db.LoaderMembersForPlacement(n.ID, order.ID)
	m := memberAt(members, n.ID)
	if err != nil || m == nil {
		return nil, "", false
	}
	if l, err := d.db.GetLoader(m.LoaderID); err != nil || l == nil || l.Layout != loaders.LayoutDedicatedPositions {
		return nil, "", false
	}
	if m.HeldByStranger {
		return members, n.Name, true
	}
	blocked, _ := CheckDropoffCapacity(d.db, n.Name, order.ID)
	return members, n.Name, blocked
}

// earlierDropBuffer is the first buffer of the loader, in member order, that
// takes a set-down now (unheld, and clear by the full capacity gate, as
// placeForLoader walks them) and that the plan does not already set down at:
// the gate does not count the asker's own drops. "" when there is none.
func (d *Dispatcher) earlierDropBuffer(members []store.LoaderPlacementMember, steps []resolvedStep, orderID int64) string {
	for _, m := range members {
		if m.Kind != loaders.HomeKindBuffer || m.HeldByStranger || orderDeliversTo(steps, m.NodeName) {
			continue
		}
		if blocked, _ := CheckDropoffCapacity(d.db, m.NodeName, orderID); blocked {
			continue
		}
		return m.NodeName
	}
	return ""
}

// tryPlaceFromHomeSource is Pattern A: the order lifts its bin AT a
// dedicated-loader home, so that home is the first candidate for the return —
// home if provably free, else a buffer slot, else drain.
//
// Reports whether it took OWNERSHIP of the placement. Every `false` means
// "this order is not a dedicated-loader home return" and the caller must go on
// to try Pattern B; it never means "placement is finished". That distinction is
// the whole reason this is a separate function: as inline code the three
// lookup failures below returned from placeForDedicatedLoader and silently
// skipped the delivery-side branch.
//
// `owned` covers the drain and the wait outcomes too — if placeForLoader finds
// no free buffer, Pattern A still owned and answered the question; waitHome
// carries placeForLoader's answer through.
func (d *Dispatcher) tryPlaceFromHomeSource(order *orders.Order, steps []resolvedStep, pc *pairPass) (owned bool, waitHome string) {
	srcNode, err := d.db.GetNodeByDotName(order.SourceNode)
	if err != nil || srcNode == nil {
		return false, ""
	}
	members, err := d.db.LoaderMembersForPlacement(srcNode.ID, order.ID)
	home := memberAt(members, srcNode.ID)
	if err != nil || home == nil {
		return false, "" // source is not a loader home — Pattern B may still apply
	}
	pl := placement{steps: steps, pc: pc, members: members}
	loader, err := d.db.GetLoader(home.LoaderID)
	if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return false, ""
	}
	homeName := srcNode.Name
	// Same gate as Pattern B, and inert in the ordinary case: this leg lifts AT
	// the home, so the carrier it reads is the home's own and matches the pin by
	// construction. It earns its place in the case where it does NOT — a home
	// already holding a carrier that does not belong to it. Putting that one back
	// where it was found is not a repair, and this is the only pass that notices.
	if carrier, known := d.carrierPayloadFor(order, steps); !homeAcceptsCarrier(home.PayloadCode, carrier, known) {
		return true, d.placeMismatchedCarrier(order, home.LoaderID, homeName, carrier, pl)
	}
	// The bin standing on the home is the one this leg lifts, so it is not a
	// hold; another order's reservation or hard claim on the home is.
	if !orderDeliversTo(steps, homeName) && !home.HeldByStranger {
		inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
		if ierr == nil && inFlight == 0 {
			d.setParkDestination(order, steps, homeName, "home")
			return true, ""
		}
	}
	return true, d.placeForLoader(order, home.LoaderID, homeName, pl)
}

// carrierPayloadFor reports the payload of the carrier THIS LEG IS MOVING, and
// whether it could be read at all. The two returns are not interchangeable:
// "" with known=true is an empty carrier, which is a real and common answer;
// "" with known=false means the question could not be answered and the caller
// must not act on it.
//
// TWO SOURCES, IN THIS ORDER, AND THE SECOND IS THE ONE THAT MATTERS.
// The leg's cart (its row) is the direct answer but it is usually absent here:
// prepareComplexSteps runs BEFORE reserveComplexSources in prepareComplexHand
// (complex_dispatch.go), so at placement time the bin is resolved but not yet
// claimed. That is not a defect to route around — placement is a
// resolution-time read on purpose, so the swap supply leg is never gated — it
// just means the row cannot be the only source.
//
// So the fallback is the physical one: read what is standing on the node this
// leg opens its pickup at. That is the same question homeClearForReturn already
// asks of the destination, through the same call.
//
// EXACTLY ONE OCCUPANT, or unknown. Zero means the leg's carrier is not there
// (in transit, or a plan this function does not understand) and more than one
// means the node is in a state where "the carrier" does not name anything. Both
// are honest "cannot say" answers, and homeAcceptsCarrier treats them as such.
func (d *Dispatcher) carrierPayloadFor(order *orders.Order, steps []resolvedStep) (string, bool) {
	// The leg's one cart (a home clear's leg carries one). A leg carrying
	// several has no one carrier to answer for: "cannot say".
	cart, err := order.OnlyBin()
	if err != nil {
		log.Printf("dispatch: order %d: %v — its carrier's payload cannot be said", order.ID, err)
		return "", false
	}
	if cart != nil {
		b, err := d.db.GetBin(*cart)
		if err != nil || b == nil {
			return "", false
		}
		return b.PayloadCode, true
	}
	pickup := firstPickupNode(steps)
	if pickup == "" {
		return "", false
	}
	node, err := d.db.GetNodeByDotName(pickup)
	if err != nil || node == nil {
		return "", false
	}
	occupants, err := d.db.ListBinsByNode(node.ID)
	if err != nil || len(occupants) != 1 {
		return "", false
	}
	return occupants[0].PayloadCode, true
}

// firstPickupNode returns the node of this plan's opening pickup — the one that
// lifts the carrier the leg carries. Blank when no pickup names a node.
func firstPickupNode(steps []resolvedStep) string {
	for _, s := range steps {
		if s.Action == protocol.ActionPickup && s.Node != "" {
			return s.Node
		}
	}
	return ""
}

// homeAcceptsCarrier reports whether a home may take this carrier.
//
// FAILS OPEN, DELIBERATELY, on all three of its "I do not know" inputs, because
// every one of them is also the shape of an ordinary correct park:
//
//   - homePin == "": a buffer, or an unassigned position. Takes anything, and
//     always has.
//   - !known: the carrier could not be read. Refusing here would divert legs on
//     an unread fact, which trades a rare wrong park for a common wrong one.
//   - carrierPayload == "": a SPENT carrier, whose payload is cleared on
//     release. This is the single most common return in the plant and its home
//     is exactly where it belongs. Diverting these would send every swap return
//     to buffer and exhaust the pool within a shift — see placeForLoader's WARN,
//     which already fires at Springfield without any help from this function.
//
// The one case it refuses is the one it was written for: a carrier that is known
// to hold something, and that something is not what this home is for.
func homeAcceptsCarrier(homePin, carrierPayload string, known bool) bool {
	if homePin == "" || !known || carrierPayload == "" {
		return true
	}
	return carrierPayload == homePin
}

// homeForPayload returns the name of the home in this loader pinned to payload
// and able to take a bin right now, or "" when there is none.
//
// Buffers are skipped: they carry no pin, so every one of them would match a
// blank comparison and the first buffer in sort order would masquerade as "the
// carrier's own home" in the log line. A buffer is the FALLBACK, and
// placeMismatchedCarrier says so in different words.
//
// A home another order holds by reservation or hard claim is not able to take
// it, the same as for every other member.
func (d *Dispatcher) homeForPayload(members []store.LoaderPlacementMember, payload string, orderID int64) string {
	if payload == "" {
		return ""
	}
	for _, m := range members {
		if m.Kind == loaders.HomeKindBuffer || m.PayloadCode != payload || m.HeldByStranger {
			continue
		}
		if blocked, _ := CheckDropoffCapacity(d.db, m.NodeName, orderID); blocked {
			continue
		}
		return m.NodeName
	}
	return ""
}

// memberAt returns the member standing on nodeID, or nil.
func memberAt(members []store.LoaderPlacementMember, nodeID int64) *store.LoaderPlacementMember {
	for i := range members {
		if members[i].PositionNodeID == nodeID {
			return &members[i]
		}
	}
	return nil
}

// placeMismatchedCarrier routes a carrier that does not belong on the home it
// was pointed at: to its OWN home when that home is free, else to a buffer.
//
// ITS OWN HOME FIRST, not straight to buffer. Buffer is where a carrier goes
// when nothing better is available, and it leaves a bin somebody has to move
// again later; the Springfield carrier's own home (SMN_031) was occupied that
// night, but on the FIRST attempt of the same swap — the one that was cancelled
// — the Edge had authored exactly that destination and Core's buffer fallback
// handled it correctly. Sending a mismatched carrier home is therefore not a new
// behaviour, it is the behaviour the cancelled leg would have had.
//
// LOUD IN BOTH ARMS. A mismatch means two parts of the system disagreed about
// what is on a cell, and that is worth a line whether or not the recovery was
// clean — the recovery hides the disagreement, which is how this one survived a
// full shift.
func (d *Dispatcher) placeMismatchedCarrier(order *orders.Order, loaderID int64, homeName, carrierPayload string, pl placement) (waitHome string) {
	if own := d.homeForPayload(pl.members, carrierPayload, order.ID); own != "" {
		log.Printf("WARN: order %d carries %s, which does not belong on home %s — routing to %s, "+
			"the carrier's own home. Something upstream picked this destination from the style being "+
			"requested rather than the carrier being lifted; the park is corrected but the disagreement is not.",
			order.ID, carrierPayload, homeName, own)
		d.setParkDestination(order, pl.steps, own, "home")
		return ""
	}
	log.Printf("WARN: order %d carries %s, which does not belong on home %s, and its own home is "+
		"occupied or unconfigured — falling back to a buffer.",
		order.ID, carrierPayload, homeName)
	return d.placeForLoader(order, loaderID, homeName, pl)
}

// placeForLoader routes to another free home of the cart's part, else a free
// buffer slot, for the given loader. Shared by Pattern A and Pattern B after the
// home-first check fails.
//
// Returns homeName when the order must WAIT: no other home of the part and no
// buffer is free, and the only destination left is the home the caller just
// found unable to take the bin. Returns "" when the order has somewhere to go —
// another home, a buffer, or a drain to a different node it was already pointed
// at.
//
// ANOTHER HOME OF THE CART'S PART BEFORE A BUFFER (brief H): a loader may pin
// one part on two homes (plantspec HomeOf), and the cart belongs on either; a
// buffer is the fallback, and a cart parked there waits to be moved again. The
// part is the cart's own, or the order's when the cart is empty or cannot be
// read, as an empty belongs on its part's home (homeAcceptsCarrier). The wait's
// sentence has always named such a home as a spot to empty (spotsThatTake);
// this is what makes that true (TestLoaderPark_TheBriefsRows, the two-homes
// rows).
//
// The members come from the placement's one read of the loader
// (LoaderMembersForPlacement), which also says whether another order holds each
// one by reservation or hard claim. A held home or buffer is skipped like a
// full one.
func (d *Dispatcher) placeForLoader(order *orders.Order, loaderID int64, homeName string, pl placement) (waitHome string) {
	part, known := d.carrierPayloadFor(order, pl.steps)
	if !known || part == "" {
		part = order.PayloadCode
	}
	if other := d.homeForPayload(pl.members, part, order.ID); other != "" && other != homeName {
		d.setParkDestination(order, pl.steps, other, "home")
		return ""
	}
	for _, m := range pl.members {
		if m.Kind != loaders.HomeKindBuffer || m.HeldByStranger {
			continue
		}
		if blocked, _ := CheckDropoffCapacity(d.db, m.NodeName, order.ID); blocked {
			continue
		}
		d.setParkDestination(order, pl.steps, m.NodeName, "buffer")
		return ""
	}
	// NO BUFFER IS FREE. Two cases, told apart by where the order already points.
	//
	// DRAIN: it points somewhere other than this home (an outbound the Edge named).
	// That node is not the one just found unable to take the bin, so the order
	// keeps it. Recorded, because a drain is still the pool running out of room.
	// An ANCHORED order points at the slot the vacated arm chose last pass, which
	// is not an outbound anybody named: it re-asks below instead of draining.
	if order.DeliveryNode != "" && order.DeliveryNode != homeName && !pl.anchored {
		log.Printf("WARN: loader %d home %s cannot take order %d and no buffer is free — it drains to %s",
			loaderID, homeName, order.ID, order.DeliveryNode)
		if err := d.db.RecordRecoveryAction("loader_park_no_slot", "order", order.ID,
			fmt.Sprintf("home %s unavailable and every buffer full — draining to %s", homeName, order.DeliveryNode),
			"system"); err != nil {
			log.Printf("dispatch: record loader_park_no_slot for order %d: %v", order.ID, err)
		}
		return ""
	}
	// WAIT: the only destination left is the home just found unable to take the
	// bin. This used to dispatch anyway, and both outcomes were bad: if the robot
	// reported arrival, PlaceBinTx evicted the occupant's RECORD to _TRANSIT while
	// the carrier stayed put (SMN_016 and SMN_035, 2026-08-26); if it could not
	// lower, it stood at the home holding its bin until someone cancelled it
	// (Springfield order 6985, 2026-09-25: AMR-10 faulted 60011 at SMN_034, which
	// held a full carrier, while SMN_0012 had been free for seventeen minutes).
	// The caller queues the order instead, and the scanner re-runs this placement
	// every pass, so the first home or buffer that frees takes it.
	//
	// Waiting cannot recreate the 2026-08-31 reverted attempt. That one gated a
	// leg whose only option was its home, so a queued leg — invisible to the
	// replenishment yield because it holds no claim — let the loop refill the home
	// and then waited on the refill forever. Here the wait ends at another home of
	// the part or ANY free buffer as well as the home, and a return whose home is
	// free (or held only by this swap's own supply carrier) never reaches this
	// branch at all.
	//
	// VACATED, BEFORE THE WAIT: a member of this loader — home or buffer — whose
	// only bin the partner's committed lift takes first (vacated_slot.go). SPR
	// 2026-09-28, pairs 7060/7061 and 7062/7063: the supply was widened onto a
	// buffer partial, every buffer was full, and the return parked on its home
	// every pass while the buffer it needed was the one being emptied.
	if d.placeVacated(order, homeName, pl) {
		return ""
	}
	return homeName
}

// placement is what the loader park carries down from its one read of the
// loader: the members (with whether another order holds each), and for the
// vacated-slot rule the plan, the partner that acquired earlier in this pass,
// and whether the order's destination was re-derived from its anchor.
type placement struct {
	steps    []resolvedStep
	pc       *pairPass
	anchored bool
	members  []store.LoaderPlacementMember
}

// placeVacated is the loader's vacated arm. It tries each member the rule clears
// — the pinned-payload question asked first, exactly as for the home — and
// commits the first: DeliveryNode, the stamp the release fence reads, and the
// anchor the next pass re-derives from. The in-memory plan follows too, so the
// destination gate and the slot claim later in this pass read the slot that was
// chosen. Reports whether it placed. A member another order holds by
// reservation or hard claim is not a destination, whoever is emptying it.
func (d *Dispatcher) placeVacated(order *orders.Order, homeName string, pl placement) bool {
	f := lastDropIndex(pl.steps)
	if f < 0 {
		return false
	}
	carrier, known := d.carrierPayloadFor(order, pl.steps)
	for _, m := range pl.members {
		if m.HeldByStranger || (m.Kind != loaders.HomeKindBuffer && !homeAcceptsCarrier(m.PayloadCode, carrier, known)) {
			continue
		}
		st := d.vacatedFor(order, pl.steps, f, m.NodeName, pl.pc)
		if st == nil || !d.noOtherInFlight(m.NodeName, order) {
			continue
		}
		d.setParkDestination(order, pl.steps, m.NodeName, "vacated")
		pl.steps[f].Vacate = st
		pl.steps[f].Anchor = homeName
		d.persistStepStamp(order, f, st, homeName)
		logVacated(order, st, "loader park")
		return true
	}
	return false
}

// placeForContainment is quality containment's divert (v100): when a payload's
// containment flag is ACTIVE in Core, an FG-bound delivery leg of that payload
// re-points to the producing claim's containment_destination instead of the
// claim's ordinary OutboundDestination.
//
// ── WHY CORE, WHY HERE ────────────────────────────────────────────────────
// The Edge authors the removal leg with the FG drop already named, but the
// containment decision is QUALITY-CRITICAL state: a stale Edge-side copy of
// the flag could deliver a contained bin to FG, which is the exact escape the
// feature exists to prevent. Core is the single authority (the same LOCUS
// rule the loader park runs on): the flag lives in Core, the divert rides
// Core dispatch, and the re-point reaches the fleet through the one writer
// the park uses (repoint), in the plan this pass sends.
//
// ── SCOPE, THREE WAYS ─────────────────────────────────────────────────────
//   - Payload-keyed: only the payload flag gates the divert here. A held BIN
//     does not need this hook — its containment move is created explicitly by
//     the station action the moment it is held, while the bin is standing at
//     the produce node.
//   - Claim-scoped: the lookup keys on the order's ProcessNode (the producing
//     node), takes the mirrored claim that DECLARES a containment route, and
//     diverts only the carrier the order lifts there (containedSetDown), when
//     its drop as asked for (authoredDrop) IS that claim's outbound
//     destination. Swap returns, changeover evac legs and supermarket refills
//     drop somewhere else and pass through untouched — diverting a swap's
//     return would strand the supermarket home and stall the whole cell.
//   - Fail-closed on capacity, fail-open on read: a FULL containment node
//     parks the order (queue, waiting_for_slot, containment-capacity) rather
//     than letting the bin continue to FG — a contained payload must not
//     reach FG because containment ran out of room. A DATABASE READ ERROR,
//     on the other hand, logs and lets the order through: the scanner
//     re-evaluates every tick, the read is a PK lookup on a tiny table, and
//     parking the line on a flaky read trades a bounded quality risk for an
//     unbounded production one. The asymmetry is deliberate and recorded
//     here because the next person will look for it.
//
// Ambiguous route (two claims on one node disagreeing about containment) and
// a containment destination that does not resolve to a node both WARN and
// leave the leg alone — misconfiguration is visible, not guessed. A GROUP
// destination is the expected multi-spot shape: the divert re-points to the
// group and the NGRP resolution spreads across its free children, parking on
// full; only a CONCRETE node's capacity gate parks here.
func (d *Dispatcher) placeForContainment(order *orders.Order, steps []resolvedStep) *dispatchStep {
	f := containedSetDown(order, steps)
	if f < 0 {
		return nil
	}
	destNode, memberOf := containmentDivert(d.db, order, authoredDrop(steps[f]), d.dbg)
	if destNode == nil {
		return nil
	}
	containment, payload := destNode.Name, order.PayloadCode
	if memberOf != nil {
		b := containmentMemberBlock(order, destNode, memberOf)
		d.setQueueReason(order, protocol.QueueWaitingForSlot, b.Cause, b.Params)
		return &dispatchStep{done: true, err: fmt.Errorf("containment destination %s is a member of group %s",
			containment, memberOf.Name)}
	}
	// A GROUP is the expected shape for a containment area with several spots:
	// the re-pointed dropoff names the group, and the pass stops, so the next
	// pass's group re-resolve (prepareComplexSteps) spreads bins across its free
	// children and parks the order on ngrp-full — so for a group the local
	// capacity gate is deliberately SKIPPED: capacity is the group resolver's
	// business. A concrete node keeps the gate: a full containment node parks
	// the order here, fail-closed.
	//
	// Another order's reservation or hard claim on the concrete node parks it
	// too, asked first: the claim door refuses a slot another order holds. An
	// unreadable answer parks, as an unreadable capacity read does.
	group := destNode.NodeTypeCode == protocol.NodeClassNGRP
	if !group {
		held, herr := d.db.SlotHeldByStranger(destNode.ID, order.ID)
		blocked := herr != nil || held
		if !blocked {
			blocked, _ = CheckDropoffCapacity(d.db, containment, order.ID)
		}
		if blocked {
			log.Printf("dispatch: order %d parked — containment node %s has no room (full, or held by another "+
				"order; payload %s is contained; FG delivery refuses to run while containment has no room)",
				order.ID, containment, payload)
			d.setQueueReason(order, protocol.QueueWaitingForSlot, CauseContainmentCapacity,
				QueueParams{Destination: containment, Payload: payload})
			return &dispatchStep{done: true, err: fmt.Errorf("containment node %s has no room", containment)}
		}
	}
	if steps[f].Node == containment {
		return nil // already re-pointed (scanner replay) — idempotent
	}
	// The plan this pass sends moves with the stored one (repoint, before the
	// splice): a leg with no wait ahead of its drop goes to the fleet whole at
	// this create, and an unmoved copy would send it to FG.
	if _, err := d.repoint(order, steps, stepMove{at: f, action: protocol.ActionDropoff, node: containment}, nil); err != nil {
		log.Printf("dispatch: containment re-point order %d → %s: %v", order.ID, containment, err)
		return nil
	}
	d.dbg("containment: order %d (payload %s, contained) diverted → %s", order.ID, payload, containment)
	if group {
		return &dispatchStep{done: true}
	}
	return nil
}

// containedSetDown is the drop of the carrier the order lifts at its press —
// the carrier the contained part is on: the pickup at order.ProcessNode, then
// carrierSetDown (through a park at staging and the lift that follows it).
// Not the plan's last drop: on an index press refilled by Robot 1 that is the
// back position, where the refill goes, and the full went to finished goods
// before it. -1 when the order lifts nothing at its press (a supply leg) or
// sets it down nowhere.
func containedSetDown(order *orders.Order, steps []resolvedStep) int {
	if order.ProcessNode == "" {
		return -1
	}
	p := liftOff(steps, order.ProcessNode, 0, len(steps))
	if p < 0 {
		return -1
	}
	return carrierSetDown(steps, p)
}

// authoredDrop is the drop as the order asked for it: the group a slot was
// resolved from (Group), else the node itself. A claim's outbound may be a
// group, and by the time the divert asks, the group re-resolve has put one of
// its slots on the step.
func authoredDrop(s resolvedStep) string {
	if s.Group != "" {
		return s.Group
	}
	return s.Node
}

// containmentDivert is the divert's decision, one rule for the multi-step leg
// (placeForContainment, at dispatch) and the plain move (decideTransport, at
// planning): the node the press's carrier, asked for at final, goes to instead,
// nil to keep it. It diverts only when the order's payload is contained, the
// claim at the order's process node declares a containment route, final IS
// that claim's outbound destination as the order asked for it (a group, when
// the claim's outbound is one), and the containment destination resolves to a
// node. A failed read, an ambiguous route and a destination that does not
// resolve keep the drop and say so (fail-open on read; misconfiguration is
// visible, not guessed). An order that names no process node has no claim to
// read the route from and keeps its drop. It writes nothing.
//
// A DESTINATION INSIDE A GROUP PARKS. memberOf is the group when the
// containment destination is a member of one (a lane slot, or another child of
// a group): the group picks the spot, so a carrier sent to the member can land
// at another one, and the arrival hold, which matches the destination's own
// subtree (store.StampContainmentArrival), misses it. The caller parks the
// order: it is not sent to the member, and the contained part does not go on to
// FG. A failed read of the group diverts as before (fail-open on read).
func containmentDivert(db *store.DB, order *orders.Order, final string, dbg func(string, ...any)) (dest, memberOf *nodes.Node) {
	payload := order.PayloadCode
	if payload == "" || order.ProcessNode == "" {
		return nil, nil
	}
	contained, err := db.PayloadContainmentActive(payload)
	if err != nil {
		log.Printf("dispatch: containment read for %s failed — order %d continues (fail-open on read): %v",
			payload, order.ID, err)
		return nil, nil
	}
	if !contained {
		return nil, nil
	}
	outbound, containment, ambiguous, err := db.ContainmentRouteForNode(order.ProcessNode)
	if err != nil {
		log.Printf("dispatch: containment route read for node %s failed — order %d continues: %v",
			order.ProcessNode, order.ID, err)
		return nil, nil
	}
	if ambiguous {
		log.Printf("WARN: containment route for node %s is ambiguous (claims disagree on the destination) — "+
			"order %d continues to its authored destination. Fix the claims: one node, one containment route.",
			order.ProcessNode, order.ID)
		return nil, nil
	}
	if containment == "" {
		// The flag is on but this node's claim declares no route: visible, not
		// guessed. The toggle's guard refuses activation when NO claim for the
		// payload has a route, so reaching here means a route existed and was
		// edited away (or the claim's payload binding changed) mid-containment.
		log.Printf("WARN: payload %s is contained but node %s's claim declares no containment destination — "+
			"order %d continues to its authored destination", payload, order.ProcessNode, order.ID)
		return nil, nil
	}
	// Scope check: this leg must be the claim's own FG flow. Anything else
	// (swap return, evac, refill) passes through — see placeForContainment.
	if final == "" || final != outbound {
		if dbg != nil {
			dbg("containment: order %d's final dropoff %q is not the claim's outbound %q — not an FG leg, no divert",
				order.ID, final, outbound)
		}
		return nil, nil
	}
	destNode, err := db.GetNodeByDotName(containment)
	if err != nil || destNode == nil {
		log.Printf("WARN: containment destination %q for node %s does not resolve to a node — "+
			"order %d continues to its authored destination", containment, order.ProcessNode, order.ID)
		return nil, nil
	}
	group, err := db.GroupOf(destNode.ID)
	if err != nil {
		log.Printf("dispatch: group read for containment destination %s failed — order %d diverts to it "+
			"(fail-open on read): %v", destNode.Name, order.ID, err)
		return destNode, nil
	}
	if group != nil && order.QueueCause != string(CauseContainmentMember) {
		claims, _ := db.ContainmentClaimsAt(order.ProcessNode, containment)
		log.Printf("WARN: order %d (%s) parked — contained %s's containment destination %s is a member of group %s "+
			"(claim %s at %s): it is not sent there and does not go on to FG. Set the claim's containment "+
			"destination to %s or to a node in no group.",
			order.ID, order.EdgeUUID, payload, destNode.Name, group.Name, strings.Join(claims, ", "),
			order.ProcessNode, group.Name)
	}
	return destNode, group
}

// containmentMemberBlock is the park for a containment destination inside a
// group (containmentDivert's memberOf).
func containmentMemberBlock(order *orders.Order, dest, group *nodes.Node) CapacityBlock {
	return CapacityBlock{Cause: CauseContainmentMember,
		Params: QueueParams{Destination: dest.Name, MemberOf: group.Name, Payload: order.PayloadCode}}
}

// orderDeliversTo reports whether any dropoff step in this order targets node. Used
// to catch the single-robot-swap case where the SAME order delivers the new style to
// the home — that bin claims the home, so the returning partial must go to buffer.
func orderDeliversTo(steps []resolvedStep, node string) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node == node {
			return true
		}
	}
	return false
}

// hasWaitStep reports whether any step in a resolved step list is a wait action.
// Pattern A (placeForDedicatedLoader) reads "no wait", on a leg that does not
// set its cart down at its own process node, as "a return lifted at a home",
// and legReturnsToHome reads it that way for an order that names no process
// node.
//
// KNOWN FALSE PROXY — deliberately still here. A wait step is a proxy for leg
// role, not the role itself. What it catches, read from every leg the Edge's
// builders write (TestLoaderPark_EveryBuilderLeg, 2026-11-25): no builder writes
// a return lifted at a home with no wait. The no-wait legs that lift at a home
// are all supplies (the Sequential backfill, a part change's stage, a staged
// delivery whose staging is a home). Those that set down at their line, with
// their process node, Pattern A leaves to Pattern B (legPlacesLineBin, brief H);
// the rest it sets back down on the home it lifts from: the stage, which has no
// step at the line, and any of them sent with no process node (B23's class,
// same test). So its misroute is not "biased to buffer", as this comment once
// said; legReturnsToHome's is, with no process node (the pn=none two-robot
// returns through their home land in a buffer, same test).
//
// Replacing it with the role predicates in swap_leg_role.go was planned and
// FALSIFIED as specified (Amendment 1 §A1.5; EXEC-LOG-cobalt-kestrel-2284.md,
// "Queue item 4 — B-park"): legTakesLineBin answers false, not "unknown", with
// no ProcessNode, and 193 of 1518 complex orders measured at Springfield carried
// none (all from before 2026-05-04, as that measurement reported). No step
// builder sends one now. The one door that does is the Edge's complex-order API
// (the Manual Order and production pages, with no process node picked or a
// loader's node), and its order always carries a wait, so no order without a
// process node reaches Pattern A (TestLoaderPark_TheBriefsRows, the Manual Order
// page rows). The two call sites ask opposite questions, so one predicate should
// not serve both by negation.
func hasWaitStep(steps []resolvedStep) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionWait {
			return true
		}
	}
	return false
}

// legReturnsToHome reports whether this leg is the RETURN half of a swap — the
// one bringing the line's spent carrier back to a dedicated home.
//
// Role comes from the leg's own steps, not from whether the plan contains a
// wait. The wait was a proxy and it is wrong here: every Springfield return leg
// carries a station wait (the robot dwells at the line until the operator
// releases it), so the proxy read every return as a supply leg. Consequence,
// SMN_016 and SMN_035 on 2026-08-26: the return ran the physical gate against
// its own home, saw the carrier its sibling was 83s from lifting, yielded to a
// buffer, and left the home unclaimed — so the replenishment loop filled it 3.8s
// after it cleared, and the returning carrier had nowhere to land five hours
// later. Its record was then evicted as a ghost by the delivery that landed on
// top of it.
//
// UNKNOWN IS NOT SUPPLY. legTakesLineBin answers false both for "this is a
// supply leg" and for "I cannot tell" (empty ProcessNode); collapsing those is
// the trap SHINGO_TODO records against the naive swap. The unreadable case is
// separated out first and keeps the wait proxy. It is reachable: no step builder
// sends an empty ProcessNode (TestLoaderPark_EveryBuilderLeg), but the Edge's
// complex-order API does when no process node, or a loader's node, is picked
// (the Manual Order page), and that order carries a wait, so its drop at a home
// takes the physical gate (TestLoaderPark_TheBriefsRows, "Manual Order page:
// staged at STAGE, delivered onto HOME").
func (d *Dispatcher) legReturnsToHome(order *orders.Order, steps []resolvedStep) bool {
	if order.ProcessNode == "" {
		return !hasWaitStep(steps)
	}
	return legTakesLineBin(steps, order.ProcessNode)
}

// homeClearForReturn reports whether a return leg may take its home as the
// landing slot: the home is empty, or its occupant is the carrier this swap is
// already lifting.
//
// This is the distinction the old evac branch did not draw. It skipped the
// physical check outright, on the stated ground that "the physical bin at the
// home is the one being evac'd" — true of the carrier the sibling lifts, false
// of one some other order parked there, and the second case drives a robot at an
// occupied position. Ask which it is rather than assuming either.
//
// FAILS CLOSED: every unreadable answer returns false, routing the leg to the
// full capacity gate, which is the more conservative of the two paths.
//
// Inside a pair pass the sibling's row and plan come from the pass context (see
// swapSiblingPlan); outside one they are read.
func (d *Dispatcher) homeClearForReturn(order *orders.Order, homeName string, pc *pairPass) bool {
	node, err := d.db.GetNodeByDotName(homeName)
	if err != nil || node == nil {
		return false
	}
	occupants, err := d.db.ListBinsByNode(node.ID)
	if err != nil {
		return false
	}
	if len(occupants) == 0 {
		// EMPTY IS NOT THE INTERESTING CASE. The physical gate would also pass an
		// empty home, so this arm and the old behaviour agree here. The arm that
		// only this predicate reaches is the one below: a home standing occupied by
		// the carrier this swap is itself lifting.
		d.dbg("place: order %d home %s stands empty — both paths would hold it", order.ID, homeName)
		return true
	}
	d.dbg("place: order %d home %s holds %d carrier(s) — checking whether this swap lifts it",
		order.ID, homeName, len(occupants))
	// Occupied — acceptable only if this swap's own supply sibling lifts from
	// here. A sibling that has already gone terminal vouches for nothing: if it
	// had lifted the carrier the node would read empty above.
	sib, sibSteps, ok := d.swapSiblingPlan(order, pc)
	if !ok || protocol.IsTerminal(sib.Status) {
		return false
	}
	for _, s := range sibSteps {
		if s.Action == protocol.ActionPickup && s.Node == homeName {
			return true
		}
	}
	return false
}

// returnLiftFrom is asked for a swap's SUPPLY leg before its bin is chosen: the
// position nodes it may lift from so its sibling's returning carrier has
// somewhere to land, or nil for no restriction (plan.Need.LiftFrom).
//
// THE TWO DECISIONS ARE ONE. The supply's bin was chosen first, FIFO over the
// loader's homes and buffers, and the return was placed after it: its home, a
// free buffer, then the spot the supply just emptied when that spot takes the
// carrier (placeVacated). The first could leave the second nowhere, because a
// pinned home the supply empties does not take another part's carrier. So the
// return is asked first.
//
// nil, and the pick is plain FIFO, when this leg has no sibling returning to a
// dedicated-loader home (or it cannot be read), when spotsThatTake has nothing
// to say, or when one of those spots stands free: the return has room without
// the supply's help (a free second home of its part included, since brief H;
// a free home with no part pinned is the one spot named here it does not land
// on, see spotsThatTake). Otherwise the spots themselves; why names them, for
// the record a changed pick writes.
func (d *Dispatcher) returnLiftFrom(supply *orders.Order) (liftFrom []int64, why string) {
	ret, retSteps, ok := d.swapSiblingPlan(supply, nil)
	if !ok || protocol.IsTerminal(ret.Status) || !d.legReturnsToHome(ret, retSteps) {
		return nil, ""
	}
	spots, carrier, ok := d.spotsThatTake(ret, retSteps)
	if !ok {
		return nil, ""
	}
	for _, m := range spots {
		if !m.HeldByStranger {
			if blocked, _ := CheckDropoffCapacity(d.db, m.NodeName, ret.ID); !blocked {
				return nil, ""
			}
		}
		liftFrom = append(liftFrom, m.PositionNodeID)
	}
	return liftFrom, fmt.Sprintf("order %d's returning %s has no free spot (%s)", ret.ID, carrier, spotNames(spots))
}

// spotsThatTake is the members of the dedicated loader a return leg parks at
// that take its carrier: every buffer, and any home the carrier belongs on
// (homeAcceptsCarrier). They are what the wait's sentence names
// (parkNoSlotParams) and what returnLiftFrom asks about. The placement chain
// lands the carrier on its own home, another home pinned to its part, a buffer
// (placeForLoader), or a member a partner empties (placeVacated), so emptying
// any of them should let a waiting return go (TestLoaderPark_TheBriefsRows, the
// two-homes rows: a bare second home takes it, since brief H). One exception: a
// home with no part pinned takes any carrier here, but placeForLoader offers
// only a home pinned to the carrier's part (homeForPayload) and placeVacated
// only a member a lift empties (plan.Vacated), so such a home standing bare is
// named and not taken. ok is false, with nothing to say, when the leg does not
// park at a dedicated loader, or its carrier's part cannot be read or it carries
// none (every member takes it then).
func (d *Dispatcher) spotsThatTake(ret *orders.Order, retSteps []resolvedStep) (spots []store.LoaderPlacementMember, carrier string, ok bool) {
	target := ret.DeliveryNode
	if f := lastDropIndex(retSteps); f >= 0 && retSteps[f].Anchor != "" {
		target = retSteps[f].Anchor
	}
	if target == "" {
		return nil, "", false
	}
	dest, err := d.db.GetNodeByDotName(target)
	if err != nil || dest == nil {
		return nil, "", false
	}
	members, err := d.db.LoaderMembersForPlacement(dest.ID, ret.ID)
	home := memberAt(members, dest.ID)
	if err != nil || home == nil {
		return nil, "", false
	}
	if loader, lerr := d.db.GetLoader(home.LoaderID); lerr != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return nil, "", false
	}
	carrier, known := d.carrierPayloadFor(ret, retSteps)
	if !known || carrier == "" {
		return nil, "", false
	}
	for _, m := range members {
		if m.Kind == loaders.HomeKindBuffer || homeAcceptsCarrier(m.PayloadCode, carrier, known) {
			spots = append(spots, m)
		}
	}
	return spots, carrier, true
}

// parkNoSlotParams is the wait a return parks under when its loader has no spot
// for it: the home it waits at, and the spots spotsThatTake names (a bare home
// with no part pinned among them is not taken; see spotsThatTake).
func (d *Dispatcher) parkNoSlotParams(ret *orders.Order, retSteps []resolvedStep, waitHome string) QueueParams {
	params := QueueParams{Destination: waitHome}
	if spots, _, ok := d.spotsThatTake(ret, retSteps); ok {
		params.EmptyOneOf = spotNames(spots)
	}
	return params
}

// spotNames lists the spots' node names for an operator sentence or a record.
func spotNames(spots []store.LoaderPlacementMember) string {
	names := make([]string, 0, len(spots))
	for _, m := range spots {
		names = append(names, m.NodeName)
	}
	return strings.Join(names, ", ")
}

// noteSwapThroughBuffer records that a supply took another bin than FIFO's so
// its sibling's returning carrier can land (returnLiftFrom). A queued order makes
// its choices again every pass (asIntentKeeping), so it is written once: only
// while the stored plan does not already lift from the chosen spot.
func (d *Dispatcher) noteSwapThroughBuffer(order *orders.Order, picked string, passed *bins.Bin, why string) {
	if stored, ok := decodeSteps(order.StepsJSON); ok {
		for _, s := range stored {
			if s.Action == protocol.ActionPickup && s.Node == picked {
				return
			}
		}
	}
	from := "another spot"
	if passed.NodeName != "" {
		from = passed.NodeName
	}
	detail := fmt.Sprintf("supply lifts from %s rather than FIFO's bin %d on %s: %s", picked, passed.ID, from, why)
	log.Printf("dispatch: swap through a buffer: order %d %s", order.ID, detail)
	if err := d.db.RecordRecoveryAction("swap_through_buffer", "order", order.ID, detail, "system"); err != nil {
		log.Printf("dispatch: record swap_through_buffer for order %d: %v", order.ID, err)
	}
}

// swapSiblingPlan returns this leg's swap sibling and the sibling's plan.
//
// In a pair pass the leg placed second is handed the first leg (pairPass), and
// when that leg is this one's sibling its row and plan are what two reads would
// return: its phases persisted the plan they resolved (persistWidenedPlan and
// repoint), and a leg that went terminal in them ended the
// pass before this one ran. So the pass context answers, and the reads are
// taken only outside a pass or when the context holds some other leg.
func (d *Dispatcher) swapSiblingPlan(order *orders.Order, pc *pairPass) (*orders.Order, []resolvedStep, bool) {
	if pc != nil && pc.partner != nil && order.SiblingOrderUUID != "" && pc.partner.EdgeUUID == order.SiblingOrderUUID {
		return pc.partner, pc.steps, true
	}
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil || sibUUID == "" {
		return nil, nil, false
	}
	sib, err := d.db.GetOrderByUUID(sibUUID)
	if err != nil || sib == nil {
		return nil, nil, false
	}
	sibSteps, ok := decodeSteps(sib.StepsJSON)
	if !ok {
		return nil, nil, false
	}
	return sib, sibSteps, true
}

// setParkDestination commits the chosen dropoff for a dedicated-loader
// return leg: the plan's last drop, in the plan this pass sends and the stored
// one (repoint, before the splice), and delivery_node with it. It does not make
// the order in-flight on its own: the never-2N handshake completes when the
// order claims its bin later in the pass (see the NEVER-2N note in the header).
func (d *Dispatcher) setParkDestination(order *orders.Order, steps []resolvedStep, node, kind string) {
	f := lastDropIndex(steps)
	if f < 0 {
		log.Printf("dispatch: place park dest order %d → %s: the plan drops nothing", order.ID, node)
		return
	}
	if order.DeliveryNode == node && steps[f].Node == node {
		return // already there — idempotent across scanner replays
	}
	if _, err := d.repoint(order, steps, stepMove{at: f, action: protocol.ActionDropoff, node: node}, nil); err != nil {
		log.Printf("dispatch: place park dest order %d → %s: %v", order.ID, node, err)
		return
	}
	d.dbg("place: order %d returning partial → %s (%s)", order.ID, node, kind)
}
