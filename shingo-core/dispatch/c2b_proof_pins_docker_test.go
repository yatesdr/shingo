//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// c2b_proof_pins_docker_test.go — the two defects C2b's proof runs found
// (2026-10-07, packed and fleet-refusal after 71effbef).
//
// Source: RULINGS-C2b-sims-two-defects-2026-10-07.md, rulings 4 and 5.

// newProofDispatcher is a dispatcher with the REAL resolver over backend: both
// pins turn on a group being re-asked, which a nil resolver cannot answer.
func newProofDispatcher(t *testing.T, db *store.DB, backend fleet.Backend) *Dispatcher {
	t.Helper()
	d := NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db})
	d.DebugLog = testDebugLog(t)
	return d
}

// proofGroup is a node group of two one-slot lanes, each slot holding a
// PART-X full: lane A's slot and lane B's slot.
func proofGroup(t *testing.T, db *store.DB, prefix string) (group string, a, b *nodes.Node) {
	t.Helper()
	groupID, _, a := gatedLane(t, db, prefix+"A", "")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	laneB := &nodes.Node{Name: prefix + "B-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &groupID}
	testutil.MustNoErr(t, db.CreateNode(laneB), "create lane B")
	d0 := 0
	b = &nodes.Node{Name: prefix + "B-S0", Enabled: true, ParentID: &laneB.ID, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(b), "create lane B's slot")
	g, err := db.GetNode(groupID)
	testutil.MustNoErr(t, err, "reload the group")
	// gatedLane's second slot (depth 1) stays empty: a has nothing behind it.
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", a.ID, prefix+"-A-FULL", 10, now)
	makeLoaderBin(t, db, "PART-X", b.ID, prefix+"-B-FULL", 10, now)
	return g.Name, a, b
}

// pickupNode is the node the order's first pickup names now.
func pickupNode(t *testing.T, o *orders.Order) (node, group string) {
	t.Helper()
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatalf("order %d: plan unreadable", o.ID)
	}
	for _, s := range steps {
		if s.Action == protocol.ActionPickup {
			return s.Node, s.Group
		}
	}
	t.Fatalf("order %d: no pickup in its plan", o.ID)
	return "", ""
}

// Ruling 4 — a pickup resolved from a group is re-chosen when its slot no
// longer holds a bin it can take.
//
// Order 229 (packed after 71effbef): a swap's full pickup from SYN_STAMP was
// resolved to LSD_002; a dig leg claimed the bin there 19 ms before the swap's
// reserve; the plan kept LSD_002 for ever. Replay re-resolves only a step still
// NAMING the group, and the supply widen skips a synthetic anchor, so nothing
// re-asked: 3,650 reserve misses in 14 minutes while the order held line 1's
// carrier.
//
// The fixture: a group of two lanes, each with a full. Intake resolves the
// pickup to one of them; a stranger then claims that bin. A dispatch pass or two
// later the order must be sent from the other one. And a pickup the Edge NAMED
// (no group) is not re-chosen: it waits on its bin.
func TestC2bPin_AGroupPickupWhoseBinIsTakenIsReChosen(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	parkFixture(t, db) // PART-X and the DEFAULT bin type
	d := newProofDispatcher(t, db, testdb.NewTrackingBackend())
	group, a, b := proofGroup(t, db, "C2BP1")
	line := prNode(t, db, "C2BP1-LINE")

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2bp1", PayloadCode: "PART-X", Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{prPick(group), prDrop(line.Name)},
	})
	o := prReloadUUID(t, db, "c2bp1")
	chosen, stamped := pickupNode(t, o)
	if stamped != group || (chosen != a.Name && chosen != b.Name) {
		t.Fatalf("fixture: intake left the pickup at %q (group %q), want one of the group's slots stamped %q",
			chosen, stamped, group)
	}
	other := b
	if chosen == b.Name {
		other = a
	}
	// The stranger takes the chosen slot's bin before this order's reserve.
	taken, err := db.ListBinsByNode(nodeID(t, db, chosen))
	testutil.MustNoErr(t, err, "list the chosen slot's bins")
	stranger := testdb.CreateOrder(t, db, func(s *orders.Order) { s.EdgeUUID = "c2bp1-stranger"; s.Status = StatusDispatched })
	testdb.ClaimBinForTest(t, db, taken[0].ID, stranger.ID)

	for range 3 {
		prScanPass(t, d, db, "c2bp1")
	}
	var red []string
	got := prReloadUUID(t, db, "c2bp1")
	if node, _ := pickupNode(t, got); got.VendorOrderID == "" || node != other.Name {
		red = append(red, fmt.Sprintf("after 3 passes the order is %s under %q with its pickup at %s (sent: %v), "+
			"want sent from %s", got.Status, got.QueueCause, node, got.VendorOrderID != "", other.Name))
	}
	if got.Status == StatusFailed {
		red = append(red, fmt.Sprintf("the order failed (%q): a taken slot is a wait, never a fail", got.ErrorDetail))
	}

	// A group with no bin left is a capacity wait under the group's cause: never
	// a fail, and never a hold on the slot it lost.
	dryGroup, da, db2 := proofGroup(t, db, "C2BP3")
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2bp3", PayloadCode: "PART-X", Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{prPick(dryGroup), prDrop(line.Name)},
	})
	for _, n := range []*nodes.Node{da, db2} {
		bs, err := db.ListBinsByNode(n.ID)
		testutil.MustNoErr(t, err, "list the dry group's bins")
		testdb.ClaimBinForTest(t, db, bs[0].ID, stranger.ID)
	}
	for range 3 {
		prScanPass(t, d, db, "c2bp3")
	}
	dry := prReloadUUID(t, db, "c2bp3")
	switch {
	case dry.Status == StatusFailed:
		red = append(red, fmt.Sprintf("the dry group's order failed (%q)", dry.ErrorDetail))
	case dry.VendorOrderID != "":
		red = append(red, "the dry group's order was sent with no bin to fetch")
	case dry.QueueCause == "" || dry.QueueCause == string(CauseReserveHolding):
		node, _ := pickupNode(t, dry)
		red = append(red, fmt.Sprintf("the dry group's order waits under %q with its pickup at %s, want the "+
			"group's capacity wait", dry.QueueCause, node))
	}

	// The named pickup is not re-chosen: the Edge said which bin.
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2bp1-named", PayloadCode: "PART-X", Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{prPick(chosen), prDrop(line.Name)},
	})
	for range 3 {
		prScanPass(t, d, db, "c2bp1-named")
	}
	named := prReloadUUID(t, db, "c2bp1-named")
	if node, _ := pickupNode(t, named); node != chosen || named.VendorOrderID != "" {
		t.Fatalf("the Edge-named pickup was re-chosen: at %s, sent %v (want waiting at %s)",
			node, named.VendorOrderID != "", chosen)
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
}

func nodeID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n, err := db.GetNodeByDotName(name)
	testutil.MustNoErr(t, err, "node "+name)
	return n.ID
}

// Ruling 5, the shape the first fix missed — a refused gated order whose pickup
// is then re-chosen into ANOTHER gated lane is sent with one lane wait, for the
// lane it enters now.
//
// Fleet-refusal after 62666564, order 46: the supply's plan was persisted as
// [wait LS_C8, pickup LSC_026 (group SYN_COMP), …] and the create refused. On
// the retry LSC_026's bin was taken, the pickup went back to its group
// and came back as LSC_028 in another lane. A splice that kept
// the wait it recognised kept LS_C8's, inserted the new lane's in front of the
// pickup, and the check failed the order (invalid_steps); the evac was cancelled
// with it.
func TestC2bPin_ARefusedGatedOrderWhosePickupMovesLaneIsSentWithOneWait(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	parkFixture(t, db)
	backend := &failCreateOnce{MockTrackingBackend: testdb.NewTrackingBackend(), uuid: "c2bp4-supply"}
	d := newProofDispatcher(t, db, backend)
	groupID, laneA, a := gatedLane(t, db, "C2BP4A", "C2BP4A-WAIT")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	laneB := &nodes.Node{Name: "C2BP4B-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &groupID}
	testutil.MustNoErr(t, db.CreateNode(laneB), "create lane B")
	testutil.MustNoErr(t, db.SetNodeProperty(laneB.ID, PropLaneGatePoint, "C2BP4B-WAIT"), "gate lane B")
	d0 := 0
	b := &nodes.Node{Name: "C2BP4B-S0", Enabled: true, ParentID: &laneB.ID, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(b), "create lane B's slot")
	g, err := db.GetNode(groupID)
	testutil.MustNoErr(t, err, "reload the group")
	line, stage, home := prNode(t, db, "C2BP4-LINE"), prNode(t, db, "C2BP4-STAGE"), prNode(t, db, "C2BP4-HOME")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", a.ID, "c2bp4-a-full", 10, now)
	makeLoaderBin(t, db, "PART-X", b.ID, "c2bp4-b-full", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "c2bp4-resident", 3, now)

	vsSubmitPair(d, "c2bp4", "PART-X", line.Name, stage.Name, g.Name, home.Name, false)
	prScanPass(t, d, db, "c2bp4-return", "c2bp4-supply")
	if !backend.refused {
		t.Fatalf("fixture: the supply's create was never attempted (%s, %q)",
			prReloadUUID(t, db, "c2bp4-supply").Status, prReloadUUID(t, db, "c2bp4-supply").QueueCause)
	}
	// The stranger takes the bin the refused plan chose.
	chosen, _ := pickupNode(t, prReloadUUID(t, db, "c2bp4-supply"))
	want, wantLane := b, laneB.ID
	if chosen == b.Name {
		want, wantLane = a, laneA
	}
	taken, err := db.ListBinsByNode(nodeID(t, db, chosen))
	testutil.MustNoErr(t, err, "list the chosen slot's bins")
	// The refused supply keeps its paper as a pending row (Q6); the run's bin was
	// taken from under it all the same, which the fixture does by hand.
	sup := prReloadUUID(t, db, "c2bp4-supply")
	testutil.MustNoErr(t, db.ReleaseReservation(sup.ID, taken[0].ID), "give up the supply's pending row")
	stranger := testdb.CreateOrder(t, db, func(s *orders.Order) { s.EdgeUUID = "c2bp4-stranger"; s.Status = StatusDispatched })
	testdb.ClaimBinForTest(t, db, taken[0].ID, stranger.ID)

	for range 3 {
		prScanPass(t, d, db, "c2bp4-return", "c2bp4-supply")
	}
	var red []string
	supply := prReloadUUID(t, db, "c2bp4-supply")
	if supply.VendorOrderID == "" || supply.Status == StatusFailed {
		red = append(red, fmt.Sprintf("the refused supply was not sent again: %s under %q (%q)",
			supply.Status, supply.QueueCause, supply.ErrorDetail))
	} else if steps, ok := decodeSteps(supply.StepsJSON); ok {
		var lanes []int64
		for _, s := range steps {
			if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
				lanes = append(lanes, s.WaitLane)
			}
		}
		if node, _ := pickupNode(t, supply); len(lanes) != 1 || lanes[0] != wantLane || node != want.Name {
			red = append(red, fmt.Sprintf("the supply was sent from %s with lane waits for %v, want from %s with "+
				"one wait for lane %d", node, lanes, want.Name, wantLane))
		}
	}
	if r := prReloadUUID(t, db, "c2bp4-return"); r.Status == StatusCancelled || r.Status == StatusFailed {
		red = append(red, fmt.Sprintf("the partner was ended (%s) for the supply's refusal", r.Status))
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
}

// Ruling 5 — a gated complex order the fleet refuses is sent again on the next
// pass, with one lane wait per gated entry, and its swap partner is never
// cancelled for the refusal.
//
// Fleet-refusal after 71effbef: 23 orders failed invalid_steps on the retry and
// 12 partners were cancelled with them. The valve persists the spliced plan
// before the create (persist-before-create); Q6 re-queues the refused order with
// its paper, so the retry splices a plan already carrying the wait, inserts a
// second in front of the first, and assertEachWaitGatesItsEntry finds a wait
// whose next step is the other wait's map point. At the base it is red at the
// refusal itself: the refused create failed the order (fleet_failed).
func TestC2bPin_ARefusedGatedComplexOrderIsSentAgainWithOneWait(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	parkFixture(t, db)
	backend := &failCreateOnce{MockTrackingBackend: testdb.NewTrackingBackend(), uuid: "c2bp2-supply"}
	d := newProofDispatcher(t, db, backend)
	_, _, src := gatedLane(t, db, "C2BP2", "C2BP2-WAIT")
	line, stage, home := prNode(t, db, "C2BP2-LINE"), prNode(t, db, "C2BP2-STAGE"), prNode(t, db, "C2BP2-HOME")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", src.ID, "c2bp2-full", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "c2bp2-resident", 3, now)

	vsSubmitPair(d, "c2bp2", "PART-X", line.Name, stage.Name, src.Name, home.Name, false)
	prScanPass(t, d, db, "c2bp2-return", "c2bp2-supply")
	if !backend.refused {
		t.Fatalf("fixture: the supply's create was never attempted (%s, %q)",
			prReloadUUID(t, db, "c2bp2-supply").Status, prReloadUUID(t, db, "c2bp2-supply").QueueCause)
	}

	prScanPass(t, d, db, "c2bp2-return", "c2bp2-supply")
	var red []string
	supply := prReloadUUID(t, db, "c2bp2-supply")
	if supply.VendorOrderID == "" || supply.Status == StatusFailed {
		red = append(red, fmt.Sprintf("the refused supply was not sent again: %s under %q (%q)",
			supply.Status, supply.QueueCause, supply.ErrorDetail))
	}
	if steps, ok := decodeSteps(supply.StepsJSON); ok {
		waits := 0
		for _, s := range steps {
			if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
				waits++
			}
		}
		if waits != 1 {
			red = append(red, fmt.Sprintf("the supply's plan carries %d lane wait(s) for its one gated entry", waits))
		}
	}
	if r := prReloadUUID(t, db, "c2bp2-return"); r.Status == StatusCancelled || r.Status == StatusFailed {
		red = append(red, fmt.Sprintf("the partner was ended (%s) for the supply's refusal", r.Status))
	}
	if len(red) != 0 {
		t.Fatalf("%v", red)
	}
}
