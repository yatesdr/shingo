//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// storLaneSlots builds a real deep lane whose slots are STOR-class and linked to
// `payloadCode`, which is what makes them candidates for the carried-bin
// recovery's tier 2. Returns the slots, shallowest first.
//
// The lane matters: STOR is the class of a LANE SLOT as well as a flat position
// — 30 of demo.yaml's SMN_* slots carry it — and every bug this file pins is one
// that is invisible if the fixture only has flat nodes.
func storLaneSlots(t *testing.T, db *store.DB, name, payloadCode string, depth int) []*nodes.Node {
	t.Helper()
	_, slots := laneWithSlots(t, db, name, depth)
	storType, err := db.GetNodeTypeByCode(protocol.NodeClassSTOR)
	testutil.MustNoErr(t, err, "get STOR type")
	p, err := db.GetPayloadByCode(payloadCode)
	testutil.MustNoErr(t, err, "get payload")
	for _, s := range slots {
		s.NodeTypeID = &storType.ID
		testutil.MustNoErr(t, db.UpdateNode(s), "type the slot STOR")
		testutil.MustNoErr(t, db.SetNodePayloads(s.ID, []int64{p.ID}), "link payload")
	}
	return slots
}

// TestCarriedBinRecovery_Tier2WillNotPickAnUnreachableSlot is the census finding:
// this was the only destination reader in the plant consulting no shared
// predicate at all.
//
// A recovery drop is UNATTENDED. Sending a robot to a slot with an occupied slot
// in front of it ends with the robot standing in the aisle holding the bin —
// which is the exact state the carried-bin path exists to end, reached by the
// path that was supposed to end it.
func TestCarriedBinRecovery_Tier2WillNotPickAnUnreachableSlot(t *testing.T) {
	db := testdb.Open(t)
	_, _, p := setupTestData(t, db)
	slots := storLaneSlots(t, db, "CBR-REACH", p.Code, 3)

	// Mouth occupied; everything behind it is walled in.
	createTestBinAtNode(t, db, p.Code, slots[0].ID, "CBR-REACH-MOUTH")

	node, err := db.FindEmptyStorageNodeForPayload(p.Code)
	testutil.MustNoErr(t, err, "find storage node")
	if node != nil {
		t.Fatalf("picked %s, but the only empty slots in that lane sit behind an occupied mouth — "+
			"a robot sent there cannot lower the bin and will stand in the aisle holding it", node.Name)
	}
}

// TestCarriedBinRecovery_Tier2PacksDeepestFirst pins the ordering. The old query
// ordered by NAME on the argument that "there is no best free slot here" — true
// of a flat position, false inside a lane, where filling the mouth before the
// back is how a lane grows a bubble.
func TestCarriedBinRecovery_Tier2PacksDeepestFirst(t *testing.T) {
	db := testdb.Open(t)
	_, _, p := setupTestData(t, db)
	slots := storLaneSlots(t, db, "CBR-DEPTH", p.Code, 3)

	node, err := db.FindEmptyStorageNodeForPayload(p.Code)
	testutil.MustNoErr(t, err, "find storage node")
	if node == nil {
		t.Fatal("an empty lane offered no slot at all")
	}
	deepest := slots[len(slots)-1]
	if node.ID != deepest.ID {
		t.Errorf("picked %s, want the DEEPEST slot %s — name order fills the mouth first and seals "+
			"the lane behind it", node.Name, deepest.Name)
	}
}

// TestCarriedBinRecovery_Tier2YieldsToAnInboundOrder pins the second missing
// question. Another live order's delivery_node already names the slot: two bins,
// one slot, and the second robot cannot place either.
func TestCarriedBinRecovery_Tier2YieldsToAnInboundOrder(t *testing.T) {
	db := testdb.Open(t)
	_, _, p := setupTestData(t, db)
	// ONE slot, so the spoken-for slot is the only candidate and "pick a
	// different one" is not an escape. With two slots this test passes on the
	// unfixed query by accident — name order hands back the other slot.
	only := storLaneSlots(t, db, "CBR-SPOKEN", p.Code, 1)[0]

	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "in_transit"
		o.DeliveryNode = only.Name
	})

	node, err := db.FindEmptyStorageNodeForPayload(p.Code)
	testutil.MustNoErr(t, err, "find storage node")
	if node != nil {
		t.Fatalf("picked %s while a live order is already delivering there — two bins, one slot, "+
			"and the second robot cannot place either", node.Name)
	}
}

// TestUsableDropPoint_AsksTheTwoQuestionsEveryTierNeeds covers tiers 1 and 3,
// which name a node somebody already had in mind rather than picking one. They
// share this gate precisely so a tier cannot forget a condition — and it was
// missing the two that only matter in a lane.
func TestUsableDropPoint_AsksTheTwoQuestionsEveryTierNeeds(t *testing.T) {
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())
	_, _, p := setupTestData(t, db)
	slots := storLaneSlots(t, db, "CBR-GATE", p.Code, 3)

	if eng.usableDropPoint(slots[2]) == nil {
		t.Fatal("a reachable, empty, unspoken-for slot was refused — the gate is now too tight")
	}

	createTestBinAtNode(t, db, p.Code, slots[0].ID, "CBR-GATE-MOUTH")
	if got := eng.usableDropPoint(slots[2]); got != nil {
		t.Errorf("accepted %s with an occupied slot in front of it", got.Name)
	}

	free := storLaneSlots(t, db, "CBR-GATE2", p.Code, 1)[0]
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = "in_transit"
		o.DeliveryNode = free.Name
	})
	if got := eng.usableDropPoint(free); got != nil {
		t.Errorf("accepted %s while a live order is already delivering there", got.Name)
	}
}
