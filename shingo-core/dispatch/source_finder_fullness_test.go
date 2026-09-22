package dispatch

import (
	"testing"

	"shingo/protocol"

	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
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

// ── AN OPERATOR'S MOVE IS NOT A SELECTION ───────────────────────────────────
//
// The rule above is about CHOOSING: when the plant goes shopping for a carrier
// to feed a drain window, it must not come back with an empty. A person who
// pointed at a carrier and at a destination has already chosen, and there is
// nothing left for the rule to prefer between.
//
// Applying it anyway wedged a live plant. Hopkinsville, 2026-09-22: an operator
// moved an EMPTY carrier off a supermarket node onto a consume-role window from
// the Core bins page. Every move is IntentFull (see the Intent constants —
// "retrieve, move"), so the drain-window rule fired, and the order parked as
// "Waiting for material" waiting for a full carrier nobody was ever going to
// send. The operator's own bin stood at the source node the whole time. Nothing
// would ever have released it.

func TestOperatorMove_CanMoveAnEmptyOntoADrainWindow(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	// An EMPTY carrier — no payload, nothing in it — standing at a source node.
	db.fifoBin = atStorage(db, &bins.Bin{ID: 99, UOPRemaining: 0, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
		// The operator signal: a person named this at a door, so no episode.
		OriginClass: protocol.OriginClassNoDemand,
	}, IntentFull)

	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 99 {
		t.Fatalf("outcome=%v bin=%v, want the empty carrier the operator named.\n\n"+
			"The drain-window fullness rule is a SELECTION preference and there is nothing "+
			"here to select: a person chose the carrier and the destination. Refusing parks "+
			"the order as \"Waiting for material\" forever — no full carrier is coming, "+
			"because none was asked for.", res.Outcome, res.Bin)
	}
}

// AND THE RULE STILL HOLDS FOR THE PLANT'S OWN PULLS. The guard keys on the
// operator signal, not on emptiness, so an automatic pull into a drain window
// is refused an empty exactly as before. Without this the fix would read as
// "drain windows accept anything now".
func TestAutomaticPull_StillRefusesAnEmptyIntoADrainWindow(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()

	window := &nodes.Node{ID: 10, Name: "SMN_001", Enabled: true}
	db.addNode(window)
	db.addConsumeLoaderWindow(1, window.ID)

	db.fifoBin = atStorage(db, &bins.Bin{ID: 99, PayloadCode: "PART-A", UOPRemaining: 0, UOPCapacity: 100})

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		PayloadCode:  "PART-A",
		DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentFull,
		// No OriginClass: the plant asked, not a person.
	}, IntentFull)

	if res.Outcome == OutcomeFound {
		t.Fatalf("an automatic pull brought bin %d (uop=%d of %d) to a drain window — "+
			"the operator guard must key on WHO ASKED, not on whether the carrier is empty",
			res.Bin.ID, res.Bin.UOPRemaining, res.Bin.UOPCapacity)
	}
}
