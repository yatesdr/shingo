//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// block_record_docker_test.go — a finished block is read from Core's record of
// what it sent (orders.block_actions), never from its binTask name. Driven with
// block_record_pin_docker_test.go's recorder, so each case reads as one of its
// transcripts.

// finishReportedAs reports the order's next block FINISHED under binTask, as
// a fleet reports the key it ran: the name a plant's engineers gave the key.
func (r *pinRig) finishReportedAs(label, binTask string) {
	r.t.Helper()
	po := r.order(label)
	blocks := r.sim.BlocksForOrder(po.vid)
	if po.done >= len(blocks) {
		r.t.Fatalf("pin: %s has no block left to finish", label)
	}
	b := blocks[po.done]
	b.BinTask = binTask
	r.complete(po, b, "finish")
	po.done++
}

func (r *pinRig) check(want string) {
	r.t.Helper()
	if got := r.transcript(); got != want {
		r.t.Errorf("transcript:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A sequence whose lift is a key naming neither "load" nor "pick", with a turn
// before it whose name does hold "load": the cart lifts on the LiftAt block and
// on no other. By the name rule the turn would have lifted it and the key never.
func TestBlockRecord_ASequenceLiftsOnItsLiftAtWhateverTheNames(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	mustExec(t, db, `INSERT INTO load_sequences (name, task_names, lift_at)
		VALUES ('Lift by key', '["preload_turn","jacklift_4548","Spin_back"]', 1)`)
	mustExec(t, db, `UPDATE payloads SET advanced_load_sequence = 'Lift by key' WHERE code = $1`, bp.Code)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "rec-key", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "rec-key")
	r.run("retrieve")
	for range 4 {
		r.finish("retrieve")
	}
	r.deliver("retrieve")
	r.check(pinLines(
		"retrieve create b1 STORAGE-A1 preload_turn",
		"retrieve create b1-1 STORAGE-A1 jacklift_4548",
		"retrieve create b1-2 STORAGE-A1 Spin_back",
		"retrieve create b2 LINE1-IN JackUnload",
		"retrieve finish b1 STORAGE-A1 preload_turn -> none | steps_done 0 | A@STORAGE-A1[retrieve]",
		"retrieve finish b1-1 STORAGE-A1 jacklift_4548 -> lift A | steps_done 1 | A@_TRANSIT[retrieve]",
		"retrieve finish b1-2 STORAGE-A1 Spin_back -> none | steps_done 1 | A@_TRANSIT[retrieve]",
		"retrieve finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
		"retrieve finished -> delivered | steps_done 2 | A@LINE1-IN",
	))
}

// Every block reported under a key the plant named, sized for the carrier: the
// wait counts its step, the lift lifts, the set-down sets down. Each is read by
// what Core sent it to do.
func TestBlockRecord_ABlockUnderAnyKeyDoesWhatItWasSentToDo(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, line, inStage, _, _, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("TOOL", createTestBinAtNode(t, db, bp.Code, line.ID, "PIN-TOOL"))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "rec-sized", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			pinStationWait(line.Name), pinPick(line.Name), pinDrop(inStage.Name),
			pinStationWait(inStage.Name), pinPick(inStage.Name), pinDrop(line.Name),
		},
	})
	r.trackUUID("carry", "rec-sized")
	r.run("carry")
	r.finishReportedAs("carry", "wait_4548")
	r.stage("carry")
	r.release("carry", "rec-sized")
	r.finishReportedAs("carry", "jackload_4548")
	r.finishReportedAs("carry", "putdown_4548")
	r.finishReportedAs("carry", "wait_4548")
	r.stage("carry")
	r.release("carry", "rec-sized")
	r.finish("carry")
	r.finish("carry")
	r.deliver("carry")
	r.check(pinLines(
		"carry create b1 LINE1-IN Wait",
		"carry finish b1 LINE1-IN wait_4548 -> none | steps_done 1 | TOOL@LINE1-IN[carry]",
		"carry waiting -> staged",
		"carry released -> in_transit",
		"carry append b2 LINE1-IN JackLoad",
		"carry append b3 INBOUND-STAGING JackUnload",
		"carry append b4 INBOUND-STAGING Wait",
		"carry finish b2 LINE1-IN jackload_4548 -> lift TOOL | steps_done 2 | TOOL@_TRANSIT[carry]",
		"carry finish b3 INBOUND-STAGING putdown_4548 -> store TOOL | steps_done 3 | TOOL@INBOUND-STAGING[carry]",
		"carry finish b4 INBOUND-STAGING wait_4548 -> none | steps_done 4 | TOOL@INBOUND-STAGING[carry]",
		"carry waiting -> staged",
		"carry released -> in_transit",
		"carry append b5 INBOUND-STAGING JackLoad",
		"carry append b6 LINE1-IN JackUnload",
		"carry finish b5 INBOUND-STAGING JackLoad -> lift TOOL | steps_done 5 | TOOL@_TRANSIT[carry]",
		"carry finish b6 LINE1-IN JackUnload -> none | steps_done 6 | TOOL@_TRANSIT[carry]",
		"carry finished -> delivered | steps_done 6 | TOOL@LINE1-IN",
	))
}

// An order sent before the record (block_actions NULL) is read by its block
// names for its whole life, the append claimed after the migration included:
// the claim leaves the record NULL, a block reported as Wait counts, and one
// reported under a key the name rule does not know does nothing, as before.
func TestBlockRecord_AnOrderSentBeforeTheRecordIsReadByItsNames(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, line, _, _, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("OLD", createTestBinAtNode(t, db, bp.Code, line.ID, "PIN-OLD"))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "rec-null", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{pinStationWait(line.Name), pinPick(line.Name), pinDrop(outDest.Name),
			pinStationWait(outDest.Name)},
	})
	r.trackUUID("removal", "rec-null")
	mustExec(t, db, `UPDATE orders SET block_actions = NULL WHERE edge_uuid = 'rec-null'`)
	r.run("removal")
	r.finish("removal")
	r.stage("removal")
	r.release("removal", "rec-null")
	if o := testdb.RequireOrder(t, db, "rec-null"); o.BlockActions != nil {
		t.Errorf("the append claimed on an order sent before the record wrote one: %v", o.BlockActions)
	}
	r.finish("removal")
	r.finish("removal")
	r.finishReportedAs("removal", "wait_4548")
	r.check(pinLines(
		"removal create b1 LINE1-IN Wait",
		"removal finish b1 LINE1-IN Wait -> none | steps_done 1 | OLD@LINE1-IN[removal]",
		"removal waiting -> staged",
		"removal released -> in_transit",
		"removal append b2 LINE1-IN JackLoad",
		"removal append b3 OUTBOUND-DEST JackUnload",
		"removal append b4 OUTBOUND-DEST Wait",
		"removal finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | OLD@_TRANSIT[removal]",
		"removal finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | OLD@_TRANSIT[removal]",
		"removal finish b4 OUTBOUND-DEST wait_4548 -> none | steps_done 3 | OLD@_TRANSIT[removal]",
	))
}

// A block the order's record does not name does nothing but its leg row: no
// lift, no count. Its leg says it did nothing.
func TestBlockRecord_ABlockTheRecordDoesNotNameOnlyLeavesItsLeg(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "rec-absent", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "rec-absent")
	o := testdb.RequireOrder(t, db, "rec-absent")
	mustExec(t, db, `UPDATE orders SET block_actions = block_actions - $2 WHERE id = $1`, o.ID, o.VendorOrderID+"-b1")
	r.run("retrieve")
	r.finish("retrieve")
	r.check(pinLines(
		"retrieve create b1 STORAGE-A1 JackLoad",
		"retrieve create b2 LINE1-IN JackUnload",
		"retrieve finish b1 STORAGE-A1 JackLoad -> none | steps_done 0 | A@STORAGE-A1[retrieve]",
	))
	var blocks string
	testutil.MustNoErr(t, db.QueryRow(`SELECT blocks_json FROM mission_events WHERE order_id = $1 AND new_state = $2`,
		o.ID, BlockLegState).Scan(&blocks), "the block's leg row")
	var raw []map[string]any
	testutil.MustNoErr(t, json.Unmarshal([]byte(blocks), &raw), "decode the leg")
	if len(raw) != 1 || raw[0]["action"] != "" {
		t.Errorf("the unnamed block's leg is %s, want one block recorded with action \"\" (doing nothing)", blocks)
	}
}
