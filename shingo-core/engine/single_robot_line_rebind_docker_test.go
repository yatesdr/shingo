//go:build docker

package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// ── U1: THE WELD-2 REBIND ──────────────────────────────────────────────────
//
// The freeze (sim run3): order 940 delivered a full carrier to ALN_003 at
// 00:14:50 and WELD-2's counter, last ticked 00:12:57, never ticked again.
// SimMachineReady stops an active non-manual node for two reasons only —
// active_bin_id NULL/0, or a consume node with remaining_uop_cached <= 0 — and
// the bin was physically standing there. The state to explain is UNBOUND.
//
// ALN_003 is role: consume, swap_mode: single_robot. That is ONE order crossing
// the bind seam twice (BuildSingleSwapSteps):
//
//	1 pickup(InboundSource)     fetch the fresh carrier
//	2 dropoff(InboundStaging)   park it
//	3 wait(LINE)                drive to the line and hold   <- dispatch stops here
//	4 pickup(LINE)              lift the spent carrier       <- Edge UNBINDS here
//	5 dropoff(OutboundStaging)  park the spent one
//	6 pickup(InboundStaging)    collect the fresh carrier
//	7 dropoff(LINE)             PLACE IT                     <- Edge must REBIND here
//	8 pickup(OutboundStaging)   collect the spent one again
//	9 dropoff(OutboundDest)     take it to the market        <- the order's DeliveryNode
//
// Step 7 is an INTERMEDIATE dropoff — the order's delivery node is the market at
// step 9 — so the rebind rides handleStoreBlockCompleted's UOPAdjustment{Bound}
// broadcast, the channel the admin-Move fix added. Every other arm of Edge's
// HandleUOPAdjustment refuses an empty slot, each for its own good reason, so if
// this announcement does not land the node is permanently unbound with no
// releaser and nothing to re-ask.
//
// NO EXISTING TEST DROVE THIS SEQUENCE, and that absence is itself a finding:
// the store-block tests cover an intermediate store at a SUPERMARKET slot, and
// the delivered-not-bound tests cover the whole-order path. The one step that
// rebinds a consume line mid-order had no coverage on either side of the wire.

// singleRobotSwapSteps is BuildSingleSwapSteps' shape, written out so this test
// does not depend on the Edge package. Verified against the builder.
func singleRobotSwapSteps(t *testing.T, line, inStage, outStage, source, outDest string) string {
	t.Helper()
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: source},
		{Action: protocol.ActionDropoff, Node: inStage},
		{Action: protocol.ActionWait, Node: line},
		{Action: protocol.ActionPickup, Node: line},
		{Action: protocol.ActionDropoff, Node: outStage},
		{Action: protocol.ActionPickup, Node: inStage},
		{Action: protocol.ActionDropoff, Node: line},
		{Action: protocol.ActionPickup, Node: outStage},
		{Action: protocol.ActionDropoff, Node: outDest},
	}
	b, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal single_robot steps")
	return string(b)
}

// boundAnnouncementsFor returns every UOPAdjustment{Bound:true} sitting in the
// outbox for coreNodeName. The outbox is the seam: SendDataToEdge enqueues
// there, so a test can read exactly what Core would put on the wire.
func boundAnnouncementsFor(t *testing.T, db *store.DB, coreNodeName string) []protocol.UOPAdjustment {
	t.Helper()
	rows, err := db.DB.Query(`SELECT payload FROM outbox ORDER BY id`)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()

	var out []protocol.UOPAdjustment
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		if !strings.Contains(string(payload), coreNodeName) {
			continue
		}
		// The envelope nests the subject payload under "p": {subject, data}.
		// Decoded structurally rather than by substring — an assertion that
		// matched on the raw bytes would pass on an envelope whose body meant
		// something else entirely.
		var env struct {
			P struct {
				Subject string          `json:"subject"`
				Data    json.RawMessage `json:"data"`
			} `json:"p"`
		}
		if err := json.Unmarshal(payload, &env); err != nil || len(env.P.Data) == 0 {
			continue
		}
		var adj protocol.UOPAdjustment
		if err := json.Unmarshal(env.P.Data, &adj); err != nil {
			continue
		}
		if adj.Bound && adj.CoreNodeName == coreNodeName {
			out = append(out, adj)
		}
	}
	return out
}

// TestSingleRobotSwap_LineDropoffAnnouncesTheRebind is the named question from
// the brief, asked of Core: for a single_robot CONSUME swap's line-dropoff, is a
// Bound=true UOPAdjustment emitted?
//
// If this is red, the announcement is never emitted for this shape and that is
// the defect. If it is green, the announcement is emitted and the question moves
// to whether Edge processes it.
func TestSingleRobotSwap_LineDropoffAnnouncesTheRebind(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	line := sd.LineNode
	market := sd.StorageNode
	inStage := &nodes.Node{Name: "SR-IN-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(inStage), "create inbound staging")
	outStage := &nodes.Node{Name: "SR-OUT-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(outStage), "create outbound staging")

	ord := &orders.Order{
		EdgeUUID:     "single-robot-rebind",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		Coordinated:  true,
		SourceNode:   market.Name,
		DeliveryNode: market.Name, // step 9 — the order ENDS at the market
		ProcessNode:  line.Name,
		PayloadCode:  sd.Payload.Code,
		StepsJSON:    singleRobotSwapSteps(t, line.Name, inStage.Name, outStage.Name, market.Name, market.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create single_robot swap order")

	// The state at step 7: the robot is carrying the FRESH carrier (picked up at
	// step 6 from inbound staging), so it is at _TRANSIT under this order's
	// claim. The spent carrier was parked at outbound staging at step 5 — still
	// claimed, but not at _TRANSIT, which is what lets resolveDropoffBin find
	// exactly one.
	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "SR-FRESH")
	spent := testdb.CreateBinAtNode(t, db, sd.Payload.Code, outStage.ID, "SR-SPENT")
	testdb.ClaimBinForTest(t, db, fresh.ID, ord.ID)
	testdb.ClaimBinForTest(t, db, spent.ID, ord.ID)

	// Step 7: the line dropoff block finishes.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "sr-b7",
		Location: line.Name,
		BinTask:  "JackUnload",
	})

	// The fresh carrier is recorded on the line.
	testdb.RequireBinAtNode(t, db, fresh.ID, line.ID)

	got := boundAnnouncementsFor(t, db, line.Name)
	if len(got) == 0 {
		t.Fatalf("NO Bound=true UOPAdjustment was emitted for %s on the single_robot line dropoff.\n"+
			"Step 7 is where the consume node rebinds. Every other arm of Edge's HandleUOPAdjustment "+
			"refuses an empty slot, so without this the node stays unbound with no releaser — "+
			"active_bin_id NULL, SimMachineReady stops the cell, and nothing re-asks. That is WELD-2.",
			line.Name)
	}
	if got[0].BinID != fresh.ID {
		t.Errorf("the announcement names bin %d, want the FRESH carrier %d — binding the wrong bin "+
			"attributes the cell's ticks to a carrier that is driving away", got[0].BinID, fresh.ID)
	}
}

// TestSingleRobotSwap_LineRebindIsSilentlyDisarmedByATrailingTransitBin is a
// CHARACTERIZATION of the chain's single point of failure, not a fix.
//
// The step-7 announcement rides resolveDropoffBin, which resolves "the bin this
// order just set down" as THE ONE bin the order still has claimed at _TRANSIT.
// For this 9-step shape that is only true if step 5 landed: the spent carrier
// has to have been recorded at outbound staging, out of _TRANSIT, before step 6
// puts the fresh one there.
//
// If step 5's intermediate store did NOT land — its block event lost, its own
// resolve declining, the order cancelled and replayed — then at step 7 the
// order has TWO bins at _TRANSIT, resolveDropoffBin returns false, and the
// rebind announcement is never sent. The line node stays unbound with a full
// carrier standing on it and no releaser.
//
// AND IT IS SILENT. The decline logs at e.dbg — debug level — so a run that
// takes this path prints nothing at info and no alarm anywhere names the node.
// That is the property this test exists to record: one missed step five nodes
// earlier disarms the rebind, and nothing says so.
//
// Recorded rather than repaired: the brief's stop condition is that the
// announcement is emitted and processed, which the two tests above establish.
// Which of these mechanisms actually produced WELD-2 is a question for the run
// and the design round, not for a unilateral fix here.
func TestSingleRobotSwap_LineRebindIsSilentlyDisarmedByATrailingTransitBin(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	line := sd.LineNode
	market := sd.StorageNode
	inStage := &nodes.Node{Name: "SRD-IN-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(inStage), "create inbound staging")
	outStage := &nodes.Node{Name: "SRD-OUT-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(outStage), "create outbound staging")

	ord := &orders.Order{
		EdgeUUID:     "single-robot-rebind-disarmed",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		Coordinated:  true,
		SourceNode:   market.Name,
		DeliveryNode: market.Name,
		ProcessNode:  line.Name,
		PayloadCode:  sd.Payload.Code,
		StepsJSON:    singleRobotSwapSteps(t, line.Name, inStage.Name, outStage.Name, market.Name, market.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create single_robot swap order")

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	// BOTH carriers at _TRANSIT: step 5 did not land, so the spent one is still
	// recorded as in flight when the fresh one is picked up at step 6.
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "SRD-FRESH")
	spent := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "SRD-SPENT")
	testdb.ClaimBinForTest(t, db, fresh.ID, ord.ID)
	testdb.ClaimBinForTest(t, db, spent.ID, ord.ID)

	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "srd-b7",
		Location: line.Name,
		BinTask:  "JackUnload",
	})

	if got := boundAnnouncementsFor(t, db, line.Name); len(got) != 0 {
		t.Fatalf("resolveDropoffBin now resolves a two-bin _TRANSIT state — this characterization is "+
			"stale and the disarm it records may be closed. Re-derive before deleting it. got: %+v", got)
	}
	// And the carrier is left at _TRANSIT: the node reads empty while a bin
	// physically stands on it.
	testdb.RequireBinAtNode(t, db, fresh.ID, transitID)
}

// TestSingleRobotSwap_UnresolvedLineRebindRaisesAnAlarm is the audibility fix,
// red-first.
//
// The case above establishes that a trailing _TRANSIT bin makes resolveDropoffBin
// decline and the rebind announcement never leave Core. This asserts that the
// decline is now AUDIBLE: a durable recovery_actions row naming the order, the
// node, and what the operator can do about it.
//
// ── WHY recovery_actions AND NOT THE DeliveredNotBound FAMILY ──────────────
//
// The brief asked for this to ride DeliveredNotBound, which carries exactly the
// right operator instruction. That family lives on the EDGE
// (shingo-edge/engine/wiring_delivered.go) and this decline happens on CORE,
// with no Core→Edge alarm subject on the wire to carry it across. Rather than
// invent one, the alarm is raised on the side where the fact is known, through
// Core's established durable surface for "something that should have happened
// did not" — the same table the lane liveness floor, the chapter floor and the
// dig standoff tripwire write to. It carries the SAME operator instruction, so
// the front door the operator is sent to is unchanged.
//
// ── AND IT IS NARROW ON PURPOSE ────────────────────────────────────────────
//
// The alarm fires only when the declined dropoff is at the ORDER'S OWN PROCESS
// NODE — the line placement, the one whose failure leaves a cell unbound. The
// same decline at a staging or supermarket slot is ordinary (two existing tests
// cover those shapes) and stays at debug. A check that fired on those would be
// the cry-wolf this codebase warns about at every other floor.
func TestSingleRobotSwap_UnresolvedLineRebindRaisesAnAlarm(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	line := sd.LineNode
	market := sd.StorageNode
	inStage := &nodes.Node{Name: "SRA-IN-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(inStage), "create inbound staging")
	outStage := &nodes.Node{Name: "SRA-OUT-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(outStage), "create outbound staging")

	ord := &orders.Order{
		EdgeUUID:     "single-robot-rebind-alarm",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		Coordinated:  true,
		SourceNode:   market.Name,
		DeliveryNode: market.Name,
		ProcessNode:  line.Name,
		PayloadCode:  sd.Payload.Code,
		StepsJSON:    singleRobotSwapSteps(t, line.Name, inStage.Name, outStage.Name, market.Name, market.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create single_robot swap order")

	var transitID int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM nodes WHERE name='_TRANSIT'`).Scan(&transitID),
		"lookup _TRANSIT")
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "SRA-FRESH")
	spent := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transitID, "SRA-SPENT")
	testdb.ClaimBinForTest(t, db, fresh.ID, ord.ID)
	testdb.ClaimBinForTest(t, db, spent.ID, ord.ID)

	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "sra-b7",
		Location: line.Name,
		BinTask:  "JackUnload",
	})

	var n int
	var detail string
	err := db.DB.QueryRow(
		`SELECT count(*), COALESCE(max(detail),'') FROM recovery_actions
		  WHERE action = $1 AND target_type = 'order' AND target_id = $2`,
		"line_rebind_unannounced", ord.ID).Scan(&n, &detail)
	testutil.MustNoErr(t, err, "read recovery_actions")
	if n == 0 {
		t.Fatal("the rebind was NOT announced and NOTHING said so.\n" +
			"A cell is now standing with a full carrier and no binding, and the only trace is a " +
			"debug line. This is the alarm that makes it audible.")
	}
	if !strings.Contains(detail, line.Name) {
		t.Errorf("the alarm does not name the node (%q) — the operator's first question is WHICH cell.\ndetail: %s",
			line.Name, detail)
	}
	if !strings.Contains(detail, "Record Count") {
		t.Errorf("the alarm carries no operator instruction. The front door is a count correction, "+
			"which binds through HandleUOPAdjustment's repair arm.\ndetail: %s", detail)
	}
}

// TestStoreBlockDecline_AtAStagingSlotStaysQuiet is the narrowness assertion.
// The same resolveDropoffBin decline at a node that is NOT the order's process
// node is ordinary — an intermediate store whose bin the order is coming back
// for — and must not raise an alarm, or the alarm becomes noise and stops being
// read.
func TestStoreBlockDecline_AtAStagingSlotStaysQuiet(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	line := sd.LineNode
	market := sd.StorageNode
	outStage := &nodes.Node{Name: "SRQ-OUT-STAGE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(outStage), "create outbound staging")

	ord := &orders.Order{
		EdgeUUID:     "staging-decline-quiet",
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusInTransit,
		SourceNode:   market.Name,
		DeliveryNode: market.Name,
		ProcessNode:  line.Name,
		PayloadCode:  sd.Payload.Code,
		StepsJSON:    singleRobotSwapSteps(t, line.Name, "SRQ-IN", outStage.Name, market.Name, market.Name),
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create order")

	// No bin at _TRANSIT at all: resolveDropoffBin declines for the other reason.
	eng.handleStoreBlockCompleted(BlockCompletedEvent{
		OrderID:  ord.ID,
		BlockID:  "srq-b5",
		Location: outStage.Name,
		BinTask:  "JackUnload",
	})

	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM recovery_actions WHERE action = $1 AND target_id = $2`,
		"line_rebind_unannounced", ord.ID).Scan(&n), "read recovery_actions")
	if n != 0 {
		t.Fatalf("a decline at a STAGING slot raised %d line-rebind alarm(s). That decline is "+
			"ordinary — the order is coming back for that bin — and an alarm on it is the "+
			"cry-wolf every other floor in this codebase warns about", n)
	}
}
