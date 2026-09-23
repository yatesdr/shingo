package dispatch

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// THE EMPTY-OUT (U2) AND THE PART IT JUST CLEARED.
//
// A U2 is a move of whatever carrier is standing on the unloader window, to the
// empties destination. Tier 4 takes the resident of the named source node.
//
// Since a9a0eb78 (Hopkinsville orders 2233/2234) tier 4 judges an EMPTY
// carrier with the unrestricted rule, so a part-tagged move of an empty no
// longer parks on the carrier rule: both shared-window shapes below find the
// carrier. What the tag still does wrong is pinned further down. At a
// home-location unloader it turns the move into a pool Drain selection that
// drives the wrong bin out, and at the destination it gates the store on the
// stale part (binresolver TestU2Destination_StalePartIsGatedByTheDestinationRule).
// The Edge side of D2 drops the part from the U2 (createUnloaderEmptyOut).

// u2Fixture is an unloader window holding an EMPTY carrier of bin type 7, where
// payload_bin_types says PART-X travels only in type 5.
func u2Fixture() *fakeFinderDB {
	db := newFakeFinderDB()
	window := &nodes.Node{ID: 10, Name: "UNL-W1", Enabled: true}
	db.addNode(window)
	db.addNode(&nodes.Node{ID: 20, Name: "EMPTY-TOTES", Enabled: true})
	db.addConsumeLoaderWindow(1, window.ID)
	id := window.ID
	db.addBin(&bins.Bin{ID: 71, NodeID: &id, BinTypeID: 7, BinTypeCode: "TOTE-B", Status: "available", UOPRemaining: 0, UOPCapacity: 100})
	db.binTypeRule = map[string][]int64{"PART-X": {5}}
	return db
}

func u2Order(source, payload string) *orders.Order {
	return &orders.Order{
		ID: 9, OrderType: OrderTypeMove, SourceIntent: SourceIntentLocal,
		SourceNode: source, DeliveryNode: "EMPTY-TOTES", PayloadCode: payload,
	}
}

func TestPayloadlessEmptyOut_SourcesAResidentThePartCouldNotCarry(t *testing.T) {
	t.Parallel()
	db := u2Fixture()
	res := NewSourceFinder(db, nil, nil).FindSource(u2Order("UNL-W1", ""), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 71 {
		t.Fatalf("payload-less U2: outcome=%v bin=%v, want the resident carrier 71.\n\n"+
			"A move that names no part takes whatever stands on the node; the carrier "+
			"rule has nothing to judge it against.", res.Outcome, res.Bin)
	}
	if res.Node == nil || res.Node.Name != "UNL-W1" {
		t.Errorf("node = %v, want UNL-W1", res.Node)
	}
}

// Main's ruling (a9a0eb78): an EMPTY carrier is judged with the unrestricted
// rule even when the move names a part, so the part-tagged U2 at a shared
// window finds its own carrier too.
func TestEmptyOutNamingThePart_FindsItsEmptyCarrier(t *testing.T) {
	t.Parallel()
	db := u2Fixture()
	res := NewSourceFinder(db, nil, nil).FindSource(u2Order("UNL-W1", "PART-X"), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 71 {
		t.Fatalf("U2 naming PART-X: outcome=%v cause=%q bin=%v, want the resident empty 71 — "+
			"an empty carrier is not judged by the part's carrier rule", res.Outcome, res.QueueCause, res.Bin)
	}
}

// ── WHAT D2 STILL FIXES: THE HOME-LOCATION UNLOADER ─────────────────────────
//
// A dedicated_positions consume loader with two homes pinned to PART-X. Home A
// holds the carrier the operator just cleared (empty); home B holds a partial
// of PART-X with stock left. A U2 from A that names PART-X enters tier 2, the
// pool Drain selection over the loader's whole pool, and drives B's partial out
// instead of A's empty. The payload-less U2 skips the pool (tier 2 gates on a
// payload) and takes A's resident at tier 4.

func homeUnloaderFixture() *fakeFinderDB {
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 20, Name: "EMPTY-TOTES", Enabled: true})
	homeA, homeB := int64(51), int64(52)
	db.addNode(&nodes.Node{ID: homeA, Name: "UNL-HOME-A", Enabled: true})
	db.addNode(&nodes.Node{ID: homeB, Name: "UNL-HOME-B", Enabled: true})
	db.addDedicatedLoader(3, homeA, "PART-X")
	db.loaders[3].Role = loaders.RoleConsume
	h := loaders.Home{LoaderID: 3, PositionNodeID: homeB, PayloadCode: "PART-X"}
	db.homes = append(db.homes, h)
	db.homeByPos[homeB] = &db.homes[len(db.homes)-1]
	db.addBin(&bins.Bin{ID: 81, NodeID: &homeA, Status: "available", UOPRemaining: 0, UOPCapacity: 100})
	db.addBin(&bins.Bin{ID: 82, NodeID: &homeB, PayloadCode: "PART-X", Status: "available", UOPRemaining: 40, UOPCapacity: 100})
	return db
}

func TestHomeUnloader_PartTaggedEmptyOutDrainsAnotherHome(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(homeUnloaderFixture(), nil, nil).FindSource(u2Order("UNL-HOME-A", "PART-X"), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 82 {
		t.Fatalf("part-tagged U2 from home A: outcome=%v cause=%q bin=%v, want home B's partial 82 "+
			"(a tier-2 pool Drain selection)", res.Outcome, res.QueueCause, res.Bin)
	}
	if res.Node == nil || res.Node.Name != "UNL-HOME-B" {
		t.Errorf("node = %v, want UNL-HOME-B — the wrong home", res.Node)
	}
}

func TestHomeUnloader_PayloadlessEmptyOutTakesItsOwnResident(t *testing.T) {
	t.Parallel()
	res := NewSourceFinder(homeUnloaderFixture(), nil, nil).FindSource(u2Order("UNL-HOME-A", ""), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 81 {
		t.Fatalf("payload-less U2 from home A: outcome=%v cause=%q bin=%v, want A's empty 81",
			res.Outcome, res.QueueCause, res.Bin)
	}
	if res.Node == nil || res.Node.Name != "UNL-HOME-A" {
		t.Errorf("node = %v, want UNL-HOME-A", res.Node)
	}
}
