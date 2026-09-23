package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"

	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// A DRAIN WINDOW MUST BE FED FULL CARRIERS.
//
// An unloader's job is to be emptied: a person works a carrier down and clears
// the window. Sending it one that is already half consumed halves the work the
// trip paid for, and the plant configures how many carriers it wants on hand
// expecting each to be worth a full one.
//
// The source query never enforced that. It matched on payload and on
// manifest_confirmed — which means "somebody declared what is in this" and NOT
// "it is full" — then ordered FIFO by load time. So a partially drained carrier
// of the right part was eligible, and being the older row, FIFO PREFERRED it.
// The pull is named a full-in and could deliver a partial every time one
// existed.
//
// "Full" is uop_remaining >= the payload's capacity. Greater-or-equal, not
// equal: overpacking a carrier is explicitly legal here (a nominal 1000 that
// takes 1005 because the operator ran one more cycle), and an overpacked
// carrier is not less full than a nominal one.

// atStorage places a candidate carrier at a real source node. The finder
// resolves the found bin's node before returning it, so a nodeless bin fails
// structurally — which would make every case here red for the wrong reason.
func atStorage(f *fakeFinderDB, b *bins.Bin) *bins.Bin {
	const storageID = int64(5)
	if _, ok := f.nodesByID[storageID]; !ok {
		f.addNode(&nodes.Node{ID: storageID, Name: "FG-STORAGE", Enabled: true})
	}
	id := storageID
	b.NodeID = &id
	f.addBin(b)
	return b
}

// addConsumeLoaderWindow registers a shared_window CONSUME loader with one
// window at positionNodeID — an unloader, the thing a drain window belongs to.
func (f *fakeFinderDB) addConsumeLoaderWindow(loaderID, positionNodeID int64) {
	f.loaders[loaderID] = &loaders.Loader{
		ID:     loaderID,
		Role:   loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow,
	}
	h := loaders.Home{LoaderID: loaderID, PositionNodeID: positionNodeID}
	f.homes = append(f.homes, h)
	f.homeByPos[positionNodeID] = &f.homes[len(f.homes)-1]
}

func TestFullIntoADrainWindow_RefusesAPartial(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	// The only carrier of this part anywhere is half consumed.
	db.fifoBin = atStorage(db, &bins.Bin{ID: 77, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome == OutcomeFound {
		t.Fatalf("finder returned bin %d (uop=%d of %d) for a drain window — a partial is "+
			"not a full, and FIFO prefers it precisely because it is the older row",
			res.Bin.ID, res.Bin.UOPRemaining, res.Bin.UOPCapacity)
	}
	if res.Outcome != OutcomeWait {
		t.Errorf("outcome = %v, want a wait — with no full anywhere the order queues for "+
			"material, which is visible; refusing outright is not", res.Outcome)
	}
}

func TestFullIntoADrainWindow_TakesAFull(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	db.fifoBin = atStorage(db, &bins.Bin{ID: 88, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 88 {
		t.Fatalf("outcome=%v bin=%v, want the full carrier — the rule refuses partials, "+
			"not everything", res.Outcome, res.Bin)
	}
}

// An OVERPACKED carrier is full. Overpack is legal and deliberate here, and a
// rule written as equality rather than at-or-above would reject the fullest
// carriers on the floor.
func TestFullIntoADrainWindow_TakesAnOverpackedCarrier(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	db.fifoBin = atStorage(db, &bins.Bin{ID: 99, PayloadCode: "PART-A", UOPRemaining: 105, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 99 {
		t.Fatalf("outcome=%v bin=%v, want the overpacked carrier — 105 of 100 is not less "+
			"full than 100 of 100", res.Outcome, res.Bin)
	}
}

// AND THE OTHER DIRECTION, which is the one that would starve a plant if the
// rule were written too widely. Everywhere that is NOT a drain window keeps
// taking partials: a cell asking for material can work a half carrier down, and
// refusing it because a full does not exist would stop a line that had parts
// available to it.
func TestFullEverywhereElse_StillTakesAPartial(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	cell := &nodes.Node{ID: 20, Name: "ALN_003", Enabled: true}
	db.addNode(cell)
	// No loader at this node at all — an ordinary lineside destination.

	db.fifoBin = atStorage(db, &bins.Bin{ID: 55, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "ALN_003",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 55 {
		t.Fatalf("outcome=%v bin=%v, want the partial — the fullness rule belongs to drain "+
			"windows only; applying it everywhere refuses usable material to a running cell",
			res.Outcome, res.Bin)
	}
}

// A PRODUCE loader's window never reaches this rule in normal operation, and
// the reason is worth stating because it is easy to get backwards: a produce
// window receives EMPTIES for a person to fill. The pull to it is a
// retrieve_empty, and this rule only fires when fetching a FULL.
//
// So why check the role at all? Because one door — the HTTP order API — accepts
// an arbitrary destination node, and a full retrieve aimed at a produce window
// through it would otherwise match "is a loader member node" and start
// demanding fulls at a station that wants empties. The check costs one lookup
// and makes the rule say what it means: DRAIN windows.
func TestProduceLoaderWindow_StillTakesAPartial(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 30, Name: "PLN_01", Enabled: true}
	db.addNode(window)
	db.loaders[2] = &loaders.Loader{ID: 2, Role: loaders.RoleProduce, Layout: loaders.LayoutSharedWindow}
	h := loaders.Home{LoaderID: 2, PositionNodeID: window.ID}
	db.homes = append(db.homes, h)
	db.homeByPos[window.ID] = &db.homes[len(db.homes)-1]

	db.fifoBin = atStorage(db, &bins.Bin{ID: 66, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "PLN_01",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 66 {
		t.Fatalf("outcome=%v bin=%v, want the partial — this is a PRODUCE loader's window",
			res.Outcome, res.Bin)
	}
}

// A carrier whose payload has NO configured capacity cannot be judged full, and
// the established answer in this system is to refuse rather than guess: the
// sizing arithmetic refuses a zero per-bin capacity by name rather than
// inventing one. Guessing here would deliver whatever happened to be oldest and
// call it a full.
func TestFullIntoADrainWindow_RefusesWhenCapacityIsUnknown(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	db.fifoBin = atStorage(db, &bins.Bin{ID: 44, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 0})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome == OutcomeFound {
		t.Fatalf("finder returned bin %d with capacity 0 — nothing can be known to be full "+
			"against an unknown capacity", res.Bin.ID)
	}
}

// THE BUFFERED SETUP. An unloader can be configured two ways, and both are
// legitimate:
//
//   - Like Hopkinsville: the windows ARE a node group's children, material
//     lands straight in them, nothing is fetched. Inbound is blank.
//   - With an inbound source: the unloader buffers, and fulls are pulled from
//     that source to the windows.
//
// The first needs no rule — nothing sources. This is the second, and it is the
// setup the rule exists for.
//
// The source is a NODE GROUP, which is what a buffer is at these plants: every
// structural thing at Hopkinsville is one. That resolves through a different
// tier from the blank-inbound case, which is exactly why the check sits at the
// finder's single exit rather than inside one tier.
func TestBufferedUnloader_RefusesAPartialFromItsGroup(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	group := &nodes.Node{ID: 40, Name: "FG-BUFFER", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP}
	db.addNode(group)
	slot := &nodes.Node{ID: 41, Name: "FG-BUFFER-01", Enabled: true}
	db.addNode(slot)

	// The group hands back its best candidate — a partly-drained carrier.
	resolver := &fakeResolver{result: &ResolveResult{
		Bin:  &bins.Bin{ID: 33, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100, NodeID: &slot.ID},
		Node: slot,
	}}
	// Nothing plant-wide, so a fall-through would show as a different wait
	// rather than quietly passing this test for the wrong reason.
	db.fifoBin = nil

	f := NewSourceFinder(db, resolver, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		SourceNode:   "FG-BUFFER",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome == OutcomeFound {
		t.Fatalf("finder returned bin %d (uop=%d of %d) from the buffer — a buffered "+
			"unloader wants fulls just as much as an unbuffered one",
			res.Bin.ID, res.Bin.UOPRemaining, res.Bin.UOPCapacity)
	}
	if res.QueueCause != "finder-no-full-carrier" {
		t.Errorf("queued as %q, want finder-no-full-carrier — anything else means the "+
			"group's carrier was never considered and this passed for the wrong reason",
			res.QueueCause)
	}
}

func TestBufferedUnloader_TakesAFullFromItsGroup(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	group := &nodes.Node{ID: 40, Name: "FG-BUFFER", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP}
	db.addNode(group)
	slot := &nodes.Node{ID: 41, Name: "FG-BUFFER-01", Enabled: true}
	db.addNode(slot)

	resolver := &fakeResolver{result: &ResolveResult{
		Bin:  &bins.Bin{ID: 34, PayloadCode: "PART-A", UOPRemaining: 100, UOPCapacity: 100, NodeID: &slot.ID},
		Node: slot,
	}}
	db.fifoBin = nil

	f := NewSourceFinder(db, resolver, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		SourceNode:   "FG-BUFFER",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 34 {
		t.Fatalf("outcome=%v bin=%v, want the full carrier from the buffer", res.Outcome, res.Bin)
	}
}

// ── THE CARRIER ON THE NAMED NODE IS NOT A SELECTION ────────────────────────
//
// The rule above is about CHOOSING: when the plant goes shopping for a carrier
// to feed a drain window, it must not come back with a partial or an empty. A
// move of the carrier standing on the node it names has nothing to choose
// between, and the finder exempts exactly that: a carrier tier 4 found.
//
// Applying the rule anyway wedged a live plant. Hopkinsville, 2026-09-22 (order
// 2213): an operator moved an EMPTY carrier onto a consume-role window from the
// orders page. Every move is IntentFull (see the Intent constants — "retrieve,
// move"), so the drain-window rule fired, and the order parked as "Waiting for
// material" waiting for a full carrier nobody was ever going to send. The
// operator's own bin stood at the source node the whole time.
//
// The first fix keyed the exemption on the no_demand origin. The automatic
// unloader pull (U1) is no_demand too, so it switched the rule off for every
// U1. The cases below use the shapes production sends.

// u1Order is the order CreateInboundOrder builds from the Edge's automatic
// unloader pull (operator_demand_unloader.go CreateRetrieveOrder): a retrieve,
// a payload, the window as destination, the loader's inbound source (blank or
// an NGRP), skip_auto_confirm, and origin no_demand passed through unchanged by
// classifyInboundOrigin. The intake half is pinned against a real Postgres by
// TestIntake_EdgeU1Envelope_PartialParksForAFullCarrier.
func u1Order(source string) *orders.Order {
	return &orders.Order{
		OrderType:       OrderTypeRetrieve,
		PayloadCode:     "PART-A",
		SourceNode:      source,
		DeliveryNode:    "SMN_001",
		Quantity:        1,
		SkipAutoConfirm: true,
		SourceIntent:    SourceIntentForType(OrderTypeRetrieve),
		OriginClass:     protocol.OriginClassNoDemand,
	}
}

// The U1 with a blank inbound source: plant-wide FIFO, whose only candidate is
// a partial. It parks for a full.
func TestD1_U1FromPlantWide_PartialWaitsForAFull(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	db.fifoBin = atStorage(db, &bins.Bin{ID: 77, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})

	res := NewSourceFinder(db, nil, nil).FindSource(u1Order(""), IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNoFullCarrier {
		t.Fatalf("outcome=%v cause=%q bin=%v, want Wait %q — an automatic unloader pull is a "+
			"selection, and no_demand does not make it a person's choice", res.Outcome, res.QueueCause,
			res.Bin, CauseFinderNoFullCarrier)
	}
}

// The U1 with an NGRP inbound source: the group scan is handed the fullness
// filter, so the partial is not a candidate.
func TestD1_U1FromItsGroup_ScanSkipsThePartial(t *testing.T) {
	t.Parallel()
	db, r := drainWindowGroupFixture()
	res := NewSourceFinder(db, r, nil).FindSource(u1Order("FG-BUFFER"), IntentFull)
	if !r.sawFilter {
		t.Error("group resolver was given no filter for a U1")
	}
	if res.Outcome == OutcomeFound {
		t.Fatalf("U1 Found bin %d (uop %d of %d) from its group", res.Bin.ID, res.Bin.UOPRemaining, res.Bin.UOPCapacity)
	}
}

// AN UNLOADER THAT ACCEPTS PARTIALS takes one, from either tier: the loader's
// own accept_partials setting is the only switch on the rule.
func TestD1_U1_AcceptPartials_TakesThePartial(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	db.loaders[1].AcceptPartials = true
	db.fifoBin = atStorage(db, &bins.Bin{ID: 77, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100})

	res := NewSourceFinder(db, nil, nil).FindSource(u1Order(""), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 77 {
		t.Fatalf("outcome=%v cause=%q bin=%v, want the partial 77 — this unloader accepts partials",
			res.Outcome, res.QueueCause, res.Bin)
	}
}

func TestD1_U1FromItsGroup_AcceptPartials_ScanIsUnfiltered(t *testing.T) {
	t.Parallel()
	db, r := drainWindowGroupFixture()
	db.loaders[1].AcceptPartials = true
	res := NewSourceFinder(db, r, nil).FindSource(u1Order("FG-BUFFER"), IntentFull)
	if r.sawFilter {
		t.Error("group resolver was given the fullness filter for an unloader that accepts partials")
	}
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 33 {
		t.Fatalf("outcome=%v bin=%v, want the group's partial 33", res.Outcome, res.Bin)
	}
}

// The orders-page spot move (handlers_orders.go): a move, a concrete source, no
// payload, no_demand. The carrier standing on the named node is an empty, and
// the destination is a drain window. It is Found — order 2213.
func TestD1_SpotMove_EmptyResidentOntoADrainWindow_Found(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	srcID := int64(70)
	db.addNode(&nodes.Node{ID: srcID, Name: "SMN_014", Enabled: true})
	db.addBin(&bins.Bin{ID: 99, NodeID: &srcID, UOPRemaining: 0, UOPCapacity: 100, Status: "available"})

	res := NewSourceFinder(db, nil, nil).FindSource(&orders.Order{
		OrderType:    OrderTypeMove,
		SourceNode:   "SMN_014",
		DeliveryNode: "SMN_001",
		Quantity:     1,
		SourceIntent: SourceIntentForType(OrderTypeMove),
		OriginClass:  protocol.OriginClassNoDemand,
	}, IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 99 {
		t.Fatalf("outcome=%v cause=%q bin=%v, want the resident empty 99 — a move of the carrier on "+
			"a named node selects nothing", res.Outcome, res.QueueCause, res.Bin)
	}
}

// ── THE DRAIN-WINDOW RULE, TIER BY TIER ─────────────────────────────────────
//
// One verdict per tier that can hand a carrier to a drain window, with and
// without the no_demand origin. Pinned at c0c525c0; the three no_demand /
// complex cases flipped with the tier-4 exemption, and the names say the new
// verdict.

// filterHonouringResolver stands in for the group resolver: it returns the first
// candidate the caller's filter admits, and records whether a filter was passed
// at all. The shared fakeResolver ignores the filter, which cannot tell "the
// group scan was told to skip partials" from "it was not".
type filterHonouringResolver struct {
	cands     []*ResolveResult
	sawFilter bool
}

func (r *filterHonouringResolver) Resolve(grp *nodes.Node, _ binresolver.ResolveMode, payload string,
	_ binresolver.BinTypeStatement, _ reservations.DigAsker, filter binresolver.BinFilter) (*ResolveResult, error) {
	r.sawFilter = filter != nil
	for _, c := range r.cands {
		if filter == nil || filter(c.Bin) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no bin of requested payload in node group %s: %s", grp.Name, payload)
}

// drainWindowGroupFixture is a consume window SMN_001 fed from NGRP FG-BUFFER,
// whose only carrier is a partial of PART-A.
func drainWindowGroupFixture() (*fakeFinderDB, *filterHonouringResolver) {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	db.addNode(&nodes.Node{ID: 40, Name: "FG-BUFFER", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP})
	slot := &nodes.Node{ID: 41, Name: "FG-BUFFER-01", Enabled: true}
	db.addNode(slot)
	return db, &filterHonouringResolver{cands: []*ResolveResult{{
		Bin:  &bins.Bin{ID: 33, PayloadCode: "PART-A", UOPRemaining: 40, UOPCapacity: 100, NodeID: &slot.ID},
		Node: slot,
	}}}
}

// TIER 1, the plant's own pull: the group scan is handed the fullness filter,
// so the partial is not a candidate and the need waits scoped to the group.
func TestDrainWindowPin_Tier1_PlantPullFiltersTheGroup(t *testing.T) {
	t.Parallel()
	db, r := drainWindowGroupFixture()
	res := NewSourceFinder(db, r, nil).FindSource(&orders.Order{
		PayloadCode: "PART-A", SourceNode: "FG-BUFFER", DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
	}, IntentFull)
	if !r.sawFilter {
		t.Error("group resolver was given no filter — a plant pull into a drain window must skip partials in the scan")
	}
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderGroupEmpty {
		t.Errorf("outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderGroupEmpty)
	}
}

// TIER 1, no_demand: choosing from a group is a selection whoever asked, so the
// scan is filtered the same way. (At c0c525c0 it ran unfiltered and the partial
// was Found.)
func TestDrainWindowPin_Tier1_NoDemandIsFilteredToo(t *testing.T) {
	t.Parallel()
	db, r := drainWindowGroupFixture()
	res := NewSourceFinder(db, r, nil).FindSource(&orders.Order{
		PayloadCode: "PART-A", SourceNode: "FG-BUFFER", DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull, OriginClass: protocol.OriginClassNoDemand,
	}, IntentFull)
	if !r.sawFilter {
		t.Error("group resolver was given no filter for a no_demand need")
	}
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderGroupEmpty {
		t.Errorf("outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderGroupEmpty)
	}
}

// drainWindowPoolFixture is a consume window SMN_001 whose need names L1, a
// dedicated loader home holding a partial of PART-A.
func drainWindowPoolFixture() *fakeFinderDB {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	posID := int64(51)
	db.addNode(&nodes.Node{ID: posID, Name: "L1", Enabled: true})
	db.addDedicatedLoader(2, posID, "PART-A")
	db.addBin(&bins.Bin{ID: 101, PayloadCode: "PART-A", NodeID: &posID, UOPRemaining: 40, UOPCapacity: 100, Status: "available"})
	return db
}

// TIER 2, the plant's own pull: the pool Drain selection takes the partial and
// the late seatbelt refuses it.
func TestDrainWindowPin_Tier2_PlantPullSeatbeltRefusesPoolPartial(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(drainWindowPoolFixture(), nil, nil).FindSource(&orders.Order{
		PayloadCode: "PART-A", SourceNode: "L1", DeliveryNode: "SMN_001", SourceIntent: SourceIntentFull,
	}, IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNoFullCarrier {
		t.Errorf("outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderNoFullCarrier)
	}
}

// TIER 2, no_demand: ranking a loader pool is a selection too, so the seatbelt
// refuses the partial. (At c0c525c0 it was skipped and the partial was Found.)
func TestDrainWindowPin_Tier2_NoDemandHeldToFullness(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(drainWindowPoolFixture(), nil, nil).FindSource(&orders.Order{
		PayloadCode: "PART-A", SourceNode: "L1", DeliveryNode: "SMN_001", SourceIntent: SourceIntentFull,
		OriginClass: protocol.OriginClassNoDemand,
	}, IntentFull)
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderNoFullCarrier {
		t.Errorf("outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseFinderNoFullCarrier)
	}
}

// drainWindowConcreteFixture is a consume window SMN_001 and a concrete,
// non-loader node ALN_010 with a partial of PART-A standing on it.
func drainWindowConcreteFixture() *fakeFinderDB {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 10, Name: "SMN_001", Enabled: true})
	db.addConsumeLoaderWindow(1, 10)
	srcID := int64(70)
	db.addNode(&nodes.Node{ID: srcID, Name: "ALN_010", Enabled: true})
	db.addBin(&bins.Bin{ID: 202, PayloadCode: "PART-A", NodeID: &srcID, UOPRemaining: 40, UOPCapacity: 100, Status: "available"})
	return db
}

// TIER 4, the complex needs built at complex_steps.go (the widen loop and the
// buried-need recalculation): node-local, a payload, a DeliveryNode, no origin.
// The partial standing on the concrete anchor is the carrier the need named, so
// it is taken. (At c0c525c0 the seatbelt refused it.)
func TestDrainWindowPin_Tier4_ComplexNodeLocalNeedTakesTheResident(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(drainWindowConcreteFixture(), nil, nil).FindSourceForNeed(SourceNeed{
		SourceNode:   "ALN_010",
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		Intent:       IntentFull,
		NodeLocal:    true,
	})
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 202 {
		t.Errorf("outcome=%v cause=%q bin=%v, want the resident partial 202", res.Outcome, res.QueueCause, res.Bin)
	}
}

// TIER 4, a no_demand move of the partial standing on a named node: Found.
func TestDrainWindowPin_Tier4_NoDemandMoveTakesTheResident(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(drainWindowConcreteFixture(), nil, nil).FindSource(&orders.Order{
		OrderType: OrderTypeMove, PayloadCode: "PART-A", SourceNode: "ALN_010", DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentLocal, OriginClass: protocol.OriginClassNoDemand,
	}, IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 202 {
		t.Errorf("outcome=%v bin=%v, want the resident partial 202 Found", res.Outcome, res.Bin)
	}
}
