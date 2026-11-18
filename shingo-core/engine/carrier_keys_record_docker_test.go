//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// carrier_keys_record_docker_test.go — the carrier keys through the engine:
// what the fleet is sent for carts whose families name keys, block by block as
// the simulator received them, and what each sized block's finish does through
// the block record (orders.block_actions): a sized wait counts its step, a
// sized lift puts its cart in _TRANSIT. Each case is the block record pin's
// shape (block_record_pin_docker_test.go) on a plant that names keys, and its
// transcript is that pin's with the sized names in it; nothing else differs.
//
// The scene: every location a case lifts or waits at lists the defaults and
// both sized pairs; a set-down location lists the defaults only, as the
// plant's would until an engineer adds the sizes there.

// ckrSized is the keys a sized location lists: the three defaults and both
// families' pairs.
func ckrSized() []testdb.SceneBinTask {
	return append(testdb.DefaultSceneBinTasks(),
		testdb.SceneBinTask{Name: "jackload_4548", Operation: "JackLoad"},
		testdb.SceneBinTask{Name: "wait_4548", Operation: "Wait"},
		testdb.SceneBinTask{Name: "jackload_3230", Operation: "JackLoad"},
		testdb.SceneBinTask{Name: "wait_3230", Operation: "Wait"})
}

// ckrScene seeds each sized location with the sized keys and each plain one
// with the defaults only.
func ckrScene(t *testing.T, db *store.DB, sized, plain []string) {
	t.Helper()
	for _, loc := range sized {
		testdb.SeedSceneBinTasks(t, db, loc, ckrSized()...)
	}
	for _, loc := range plain {
		testdb.SeedSceneBinTasks(t, db, loc, testdb.DefaultSceneBinTasks()...)
	}
}

// ckrTypes makes the two carrier types, each of a family of its own: the
// 45x48 and the 32x30. It returns their type ids and family ids.
func ckrTypes(t *testing.T, db *store.DB) (bigType, bigFam, smallType, smallFam int64) {
	t.Helper()
	mk := func(code string) (int64, int64) {
		testutil.MustNoErr(t, db.CreateBinType(&bins.BinType{Code: code, Description: code}), "type "+code)
		bt, err := db.GetBinTypeByCode(code)
		bt = testutil.Must(t, bt, err, "read type "+code)
		return bt.ID, bt.FamilyID
	}
	bigType, bigFam = mk("CKR-4548")
	smallType, smallFam = mk("CKR-3230")
	if bigFam == smallFam {
		t.Fatalf("fixture: the two carriers share family %d", bigFam)
	}
	return bigType, bigFam, smallType, smallFam
}

// ckrKey keys a family through the Bins page's door, which reloads the set
// the builder reads.
func ckrKey(t *testing.T, r *pinRig, family int64, lift, wait string) {
	t.Helper()
	testutil.MustNoErr(t, r.eng.Dispatcher().SaveCarrierFamilyKeys(family, lift, wait), "key a family")
}

// ckrKeyBoth keys the 45x48 and the 32x30 families.
func ckrKeyBoth(t *testing.T, r *pinRig, big, small int64) {
	t.Helper()
	ckrKey(t, r, big, "jackload_4548", "wait_4548")
	ckrKey(t, r, small, "jackload_3230", "wait_3230")
}

// ckrKeyBothAroundTheSave keys the two families as the save would, but without
// its door: the save re-checks every table-turn node against the fleet's keys
// there (turnsAdmitWaitKey), and the simulator cannot report a location's keys,
// so on a plant with a turn node it refuses every wait key. The case it serves
// pins what the builder sends at a turn node, not that door
// (TestTableTurnDoor_AFamilyWaitKeySaveIsCheckedAtEveryTurnNode pins the door).
func ckrKeyBothAroundTheSave(t *testing.T, r *pinRig, big, small int64) {
	t.Helper()
	testutil.MustNoErr(t, r.db.SetCarrierFamilyKeys(big, "jackload_4548", "wait_4548"), "key 4548")
	testutil.MustNoErr(t, r.db.SetCarrierFamilyKeys(small, "jackload_3230", "wait_3230"), "key 3230")
	_, err := r.eng.Dispatcher().ReloadCarrierKeys()
	testutil.MustNoErr(t, err, "reload the keyed families")
}

// TestCarrierKeysWire_TheTwoRobotSwap is the third wire: the incoming 32x30
// from SRC via STG, the outgoing 45x48 at LINE, OUT a set-down location. The
// supply's create sends its lift and the wait in front of its relay's lift
// sized by the 32x30, its append the relay's lift; the evac's create sends the
// wait in front of the line cart's lift sized by the 45x48 (its rows were
// written before the create), its append that lift; every set-down sends
// JackUnload. With STG listing the defaults only, the supply's STG wait and
// lift send Wait and JackLoad. Each sized wait counts its step when it
// finishes and each sized lift lifts its cart.
func TestCarrierKeysWire_TheTwoRobotSwap(t *testing.T) {
	t.Parallel()
	runPinCases(t, []pinCase{
		{name: "STG lists the sized keys", run: func(t *testing.T) *pinRig { return ckrTwoRobotSwap(t, true) }, want: pinLines(
			"supply create b1 STORAGE-A1 jackload_3230",
			"supply create b2 INBOUND-STAGING JackUnload",
			"supply create b3 INBOUND-STAGING wait_3230",
			"evac create b1 LINE1-IN wait_4548",
			"supply finish b1 STORAGE-A1 jackload_3230 -> lift FRESH | steps_done 1 | FRESH@_TRANSIT[supply] OLD@LINE1-IN[evac]",
			"supply finish b2 INBOUND-STAGING JackUnload -> store FRESH | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply finish b3 INBOUND-STAGING wait_3230 -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply waiting -> staged",
			"evac finish b1 LINE1-IN wait_4548 -> none | steps_done 1 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"evac waiting -> staged",
			"evac released -> in_transit",
			"evac append b2 LINE1-IN jackload_4548",
			"evac append b3 OUTBOUND-DEST JackUnload",
			"evac finish b2 LINE1-IN jackload_4548 -> lift OLD | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finished -> delivered | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@OUTBOUND-DEST",
			"supply released -> in_transit",
			"supply append b4 INBOUND-STAGING jackload_3230",
			"supply append b5 LINE1-IN JackUnload",
			"supply finish b4 INBOUND-STAGING jackload_3230 -> lift FRESH | steps_done 4 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finish b5 LINE1-IN JackUnload -> none | steps_done 5 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finished -> delivered | steps_done 5 | FRESH@LINE1-IN OLD@OUTBOUND-DEST",
		)},
		{name: "STG lists the defaults only", run: func(t *testing.T) *pinRig { return ckrTwoRobotSwap(t, false) }, want: pinLines(
			"supply create b1 STORAGE-A1 jackload_3230",
			"supply create b2 INBOUND-STAGING JackUnload",
			"supply create b3 INBOUND-STAGING Wait",
			"evac create b1 LINE1-IN wait_4548",
			"supply finish b1 STORAGE-A1 jackload_3230 -> lift FRESH | steps_done 1 | FRESH@_TRANSIT[supply] OLD@LINE1-IN[evac]",
			"supply finish b2 INBOUND-STAGING JackUnload -> store FRESH | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply finish b3 INBOUND-STAGING Wait -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply waiting -> staged",
			"evac finish b1 LINE1-IN wait_4548 -> none | steps_done 1 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"evac waiting -> staged",
			"evac released -> in_transit",
			"evac append b2 LINE1-IN jackload_4548",
			"evac append b3 OUTBOUND-DEST JackUnload",
			"evac finish b2 LINE1-IN jackload_4548 -> lift OLD | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finished -> delivered | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@OUTBOUND-DEST",
			"supply released -> in_transit",
			"supply append b4 INBOUND-STAGING JackLoad",
			"supply append b5 LINE1-IN JackUnload",
			"supply finish b4 INBOUND-STAGING JackLoad -> lift FRESH | steps_done 4 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finish b5 LINE1-IN JackUnload -> none | steps_done 5 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finished -> delivered | steps_done 5 | FRESH@LINE1-IN OLD@OUTBOUND-DEST",
		)},
	})
}

// ckrTwoRobotSwap is pinTwoRobotSwap on a keyed plant: FRESH a 32x30, OLD a
// 45x48; stgSized says whether STG lists the sized keys.
func ckrTwoRobotSwap(t *testing.T, stgSized bool) *pinRig {
	db := testDB(t)
	storage, line, inStage, _, outDest, bp := setupProductionNodes(t, db)
	bigType, big, smallType, small := ckrTypes(t, db)
	sized, plain := []string{storage.Name, line.Name}, []string{outDest.Name}
	if stgSized {
		sized = append(sized, inStage.Name)
	} else {
		plain = append(plain, inStage.Name)
	}
	ckrScene(t, db, sized, plain)
	r := newPinSim(t, db)
	ckrKeyBoth(t, r, big, small)
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, smallType, "CKR-FRESH", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, bigType, "CKR-OLD", 2))
	pinSwapLegs(r, bp.Code, line.Name, "ckr-supply", "ckr-evac",
		[]protocol.ComplexOrderStep{pinPick(storage.Name),
			{Action: protocol.ActionDropoff, Node: inStage.Name, ExclusiveSlot: true},
			pinStationWait(inStage.Name), pinPick(inStage.Name), pinDrop(line.Name)},
		[]protocol.ComplexOrderStep{pinStationWait(line.Name), pinPick(line.Name), pinDrop(outDest.Name)})
	r.trackUUID("supply", "ckr-supply")
	r.trackUUID("evac", "ckr-evac")
	r.run("supply")
	r.finish("supply")
	r.finish("supply")
	r.finish("supply")
	r.stage("supply")
	r.run("evac")
	r.finish("evac")
	r.stage("evac")
	r.release("evac", "ckr-evac")
	r.finish("evac")
	r.finish("evac")
	r.deliver("evac")
	r.release("supply", "ckr-supply")
	r.finish("supply")
	r.finish("supply")
	r.deliver("supply")
	return r
}

// TestCarrierKeysWire_EachLiftByItsOwnCart: a single-robot swap carrying two
// families sizes each lift by the cart it lifts, and the wait in front of the
// line cart's lift by the line cart; the carryover round trip lifts one cart
// twice (two pick rows), and both lifts and the wait in front of the second
// are sized by it. Set-downs send JackUnload.
func TestCarrierKeysWire_EachLiftByItsOwnCart(t *testing.T) {
	t.Parallel()
	runPinCases(t, []pinCase{
		{name: "single-robot swap, two families", run: ckrSingleRobotSwap, want: pinLines(
			"swap create b1 STORAGE-A1 jackload_3230",
			"swap create b2 INBOUND-STAGING JackUnload",
			"swap create b3 LINE1-IN wait_4548",
			"swap finish b1 STORAGE-A1 jackload_3230 -> lift FRESH | steps_done 1 | FRESH@_TRANSIT[swap] OLD@LINE1-IN[swap]",
			"swap finish b2 INBOUND-STAGING JackUnload -> store FRESH | steps_done 2 | FRESH@INBOUND-STAGING[swap] OLD@LINE1-IN[swap]",
			"swap finish b3 LINE1-IN wait_4548 -> none | steps_done 3 | FRESH@INBOUND-STAGING[swap] OLD@LINE1-IN[swap]",
			"swap waiting -> staged",
			"swap released -> in_transit",
			"swap append b4 LINE1-IN jackload_4548",
			"swap append b5 OUTBOUND-STAGING JackUnload",
			"swap append b6 INBOUND-STAGING jackload_3230",
			"swap append b7 LINE1-IN JackUnload",
			"swap append b8 OUTBOUND-STAGING jackload_4548",
			"swap append b9 OUTBOUND-DEST JackUnload",
			"swap finish b4 LINE1-IN jackload_4548 -> lift OLD | steps_done 4 | FRESH@INBOUND-STAGING[swap] OLD@_TRANSIT[swap]",
			"swap finish b5 OUTBOUND-STAGING JackUnload -> store OLD | steps_done 5 | FRESH@INBOUND-STAGING[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b6 INBOUND-STAGING jackload_3230 -> lift FRESH | steps_done 6 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b7 LINE1-IN JackUnload -> store FRESH | steps_done 7 | FRESH@LINE1-IN[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b8 OUTBOUND-STAGING jackload_4548 -> lift OLD | steps_done 8 | FRESH@LINE1-IN[swap] OLD@_TRANSIT[swap]",
			"swap finish b9 OUTBOUND-DEST JackUnload -> none | steps_done 9 | FRESH@LINE1-IN[swap] OLD@_TRANSIT[swap]",
			"swap finished -> delivered | steps_done 9 | FRESH@LINE1-IN[swap] OLD@OUTBOUND-DEST",
		)},
		{name: "carryover round trip, one cart lifted twice", run: ckrCarryover, want: pinLines(
			"carry create b1 LINE1-IN jackload_4548",
			"carry create b2 INBOUND-STAGING JackUnload",
			"carry create b3 INBOUND-STAGING wait_4548",
			"carry finish b1 LINE1-IN jackload_4548 -> lift TOOL | steps_done 1 | TOOL@_TRANSIT[carry]",
			"carry finish b2 INBOUND-STAGING JackUnload -> store TOOL | steps_done 2 | TOOL@INBOUND-STAGING[carry]",
			"carry finish b3 INBOUND-STAGING wait_4548 -> none | steps_done 3 | TOOL@INBOUND-STAGING[carry]",
			"carry waiting -> staged",
			"carry released -> in_transit",
			"carry append b4 INBOUND-STAGING jackload_4548",
			"carry append b5 LINE1-IN JackUnload",
			"carry finish b4 INBOUND-STAGING jackload_4548 -> lift TOOL | steps_done 4 | TOOL@_TRANSIT[carry]",
			"carry finish b5 LINE1-IN JackUnload -> none | steps_done 5 | TOOL@_TRANSIT[carry]",
			"carry finished -> delivered | steps_done 5 | TOOL@LINE1-IN",
		)},
	})
}

func ckrSingleRobotSwap(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, inStage, outStage, outDest, bp := setupProductionNodes(t, db)
	bigType, big, smallType, small := ckrTypes(t, db)
	ckrScene(t, db, []string{storage.Name, line.Name, inStage.Name, outStage.Name}, []string{outDest.Name})
	r := newPinSim(t, db)
	ckrKeyBoth(t, r, big, small)
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, smallType, "CKR-FRESH", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, bigType, "CKR-OLD", 2))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "ckr-single", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			pinPick(storage.Name), pinDrop(inStage.Name),
			{Action: protocol.ActionWait, Node: line.Name},
			pinPick(line.Name), pinDrop(outStage.Name),
			pinPick(inStage.Name), pinDrop(line.Name),
			pinPick(outStage.Name), pinDrop(outDest.Name),
		},
	})
	r.trackUUID("swap", "ckr-single")
	r.run("swap")
	r.finish("swap")
	r.finish("swap")
	r.finish("swap")
	r.stage("swap")
	r.release("swap", "ckr-single")
	for range 6 {
		r.finish("swap")
	}
	r.deliver("swap")
	return r
}

func ckrCarryover(t *testing.T) *pinRig {
	db := testDB(t)
	_, line, inStage, _, _, bp := setupProductionNodes(t, db)
	bigType, big, _, small := ckrTypes(t, db)
	ckrScene(t, db, []string{line.Name, inStage.Name}, nil)
	r := newPinSim(t, db)
	ckrKeyBoth(t, r, big, small)
	r.cart("TOOL", confirmedCart(t, db, bp.Code, line.ID, bigType, "CKR-TOOL", 5))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "ckr-carry", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			pinPick(line.Name), pinDrop(inStage.Name),
			{Action: protocol.ActionWait, Node: inStage.Name, WaitKind: protocol.WaitKindStation},
			pinPick(inStage.Name), pinDrop(line.Name),
		},
	})
	r.trackUUID("carry", "ckr-carry")
	r.run("carry")
	r.finish("carry")
	r.finish("carry")
	r.finish("carry")
	r.stage("carry")
	r.release("carry", "ckr-carry")
	r.finish("carry")
	r.finish("carry")
	r.deliver("carry")
	return r
}

// TestCarrierKeysWire_ACarryingWaitSendsWait: a wait made while the robot
// carries a cart sends Wait, though its location lists the cart's sized wait
// key and the cart's family names one: the lane gate point a store waits at,
// the dig dwell a reshuffle leg holds its blocker at, and the turn a set-down
// waits for. So does a gate point in front of a lift at another node. Each
// case's lifts are sized.
func TestCarrierKeysWire_ACarryingWaitSendsWait(t *testing.T) {
	t.Parallel()
	runPinCases(t, []pinCase{
		{name: "the lane gate point a store waits at", run: ckrGatedStore, want: pinLines(
			"lane slots CKRGS-S0 CKRGS-S1",
			"store create b1 LINE1-IN jackload_4548",
			"store create b2 CKRGS-GRP-WAIT Wait",
			"store create b3 CKRGS-S1 JackUnload",
			"store finish b1 LINE1-IN jackload_4548 -> lift A | steps_done 1 | A@_TRANSIT[store]",
			"store finish b2 CKRGS-GRP-WAIT Wait -> none | steps_done 2 | A@_TRANSIT[store]",
			"store finish b3 CKRGS-S1 JackUnload -> none | steps_done 3 | A@_TRANSIT[store]",
			"store finished -> delivered | steps_done 3 | A@CKRGS-S1",
		)},
		{name: "the dig dwell", run: ckrDig, want: pinLines(
			"leg1 create b1 GRP-CKRDIG-WAIT Wait",
			"leg1 create b2 GRP-CKRDIG-L1-S1 jackload_4548",
			"leg1 create b3 GRP-CKRDIG-L1-S1 Wait",
			"leg1 finish b1 GRP-CKRDIG-WAIT Wait -> none | steps_done 1 | TARGET@GRP-CKRDIG-L1-S2 BLOCKER1@GRP-CKRDIG-L1-S1[leg1] BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg1 finish b2 GRP-CKRDIG-L1-S1 jackload_4548 -> lift BLOCKER1 | steps_done 2 | TARGET@GRP-CKRDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg1 append b4 GRP-CKRDIG-SHUF JackUnload",
			"leg2 create b1 GRP-CKRDIG-WAIT Wait",
			"leg2 create b2 GRP-CKRDIG-L1-S2 jackload_4548",
			"leg2 create b3 LINE-CKRDIG JackUnload",
			"leg1 finish b3 GRP-CKRDIG-L1-S1 Wait -> none | steps_done 3 | TARGET@GRP-CKRDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg1 finish b4 GRP-CKRDIG-SHUF JackUnload -> none | steps_done 4 | TARGET@GRP-CKRDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg1 finished -> confirmed | steps_done 4 | TARGET@GRP-CKRDIG-L1-S2[leg2] BLOCKER1@GRP-CKRDIG-SHUF BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg2 finish b1 GRP-CKRDIG-WAIT Wait -> none | steps_done 1 | TARGET@GRP-CKRDIG-L1-S2[leg2] BLOCKER1@GRP-CKRDIG-SHUF BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg2 finish b2 GRP-CKRDIG-L1-S2 jackload_4548 -> lift TARGET | steps_done 2 | TARGET@_TRANSIT[leg2] BLOCKER1@GRP-CKRDIG-SHUF BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg2 finish b3 LINE-CKRDIG JackUnload -> none | steps_done 3 | TARGET@_TRANSIT[leg2] BLOCKER1@GRP-CKRDIG-SHUF BLOCKER2@GRP-CKRDIG-L1-S3",
			"leg2 finished -> confirmed | steps_done 3 | TARGET@LINE-CKRDIG BLOCKER1@GRP-CKRDIG-SHUF BLOCKER2@GRP-CKRDIG-L1-S3",
		)},
		{name: "the turn a set-down waits for", run: ckrTurnSetDown, want: pinLines(
			"move create b1 STORAGE-A1 jackload_4548",
			"move create b2 LINE1-IN Wait",
			"move finish b1 STORAGE-A1 jackload_4548 -> lift A | steps_done 1 | A@_TRANSIT[move]",
			"move finish b2 LINE1-IN Wait -> none | steps_done 2 | A@_TRANSIT[move]",
			"move waiting -> staged | turns 1",
			"move next poll | turns 1",
			"move append b3 LINE1-IN JackUnload",
			"move finish b3 LINE1-IN JackUnload -> none | steps_done 3 | A@_TRANSIT[move]",
			"move finished -> delivered | steps_done 3 | A@LINE1-IN",
		)},
	})
}

func ckrGatedStore(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	_, slots := laneWithTwoSlots(t, db, "CKRGS")
	bigType, big, _, small := ckrTypes(t, db)
	ckrScene(t, db, []string{sd.LineNode.Name, "CKRGS-GRP-WAIT", slots[0].Name, slots[1].Name}, nil)
	r := newPinSim(t, db)
	ckrKeyBoth(t, r, big, small)
	r.cart("A", confirmedCart(t, db, sd.Payload.Code, sd.LineNode.ID, bigType, "CKR-A", 5))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "ckr-gstore", OrderType: protocol.OrderTypeMove, PayloadCode: sd.Payload.Code,
		SourceNode: sd.LineNode.Name, DeliveryNode: "CKRGS-GRP", Quantity: 1,
	})
	r.say("lane slots %s %s", slots[0].Name, slots[1].Name)
	r.trackUUID("store", "ckr-gstore")
	r.walk("store")
	return r
}

// ckrDig is pinDig with the compound's carrier family keyed (its carts share
// one type), and every slot, the gate point and the shuffle listing the sized
// keys.
func ckrDig(t *testing.T) *pinRig {
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "CKRDIG", NumSlots: 3, TargetSlot: 2,
		TargetAge: 2 * time.Hour})
	locs := []string{"GRP-CKRDIG-WAIT", "GRP-CKRDIG-SHUF", sc.LineNode.Name}
	for i := 1; i <= 3; i++ {
		locs = append(locs, fmt.Sprintf("GRP-CKRDIG-L1-S%d", i))
	}
	ckrScene(t, db, locs, nil)
	r := newPinSim(t, db)
	target, err := db.GetBin(sc.TargetBin.ID)
	testutil.MustNoErr(t, err, "read the target's family")
	ckrKey(t, r, target.BinFamilyID, "jackload_4548", "wait_4548")
	r.cart("TARGET", sc.TargetBin)
	for i, b := range sc.Blockers {
		r.cart(fmt.Sprintf("BLOCKER%d", i+1), b)
	}
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "ckr-dig", OrderType: protocol.OrderTypeRetrieve, PayloadCode: sc.Payload.Code,
		SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name, Quantity: 1,
	})
	parent := testdb.RequireOrder(t, db, "ckr-dig")
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs")
	for i, c := range children {
		r.track(fmt.Sprintf("leg%d", i+1), c.ID)
	}
	for i := range children {
		r.walk(fmt.Sprintf("leg%d", i+1))
	}
	return r
}

func ckrTurnSetDown(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	bigType, big, _, small := ckrTypes(t, db)
	ckrScene(t, db, []string{storage.Name, line.Name}, nil)
	r := pinTurnRig(t, db, line, dispatch.TableTurnSideSetDown)
	ckrKeyBothAroundTheSave(t, r, big, small)
	r.cart("A", confirmedCart(t, db, bp.Code, storage.ID, bigType, "CKR-A", 5))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "ckr-turn-sd", OrderType: protocol.OrderTypeMove, PayloadCode: bp.Code,
		SourceNode: storage.Name, DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("move", "ckr-turn-sd")
	r.sim.SetRobotJackUp("AMR-T1", true)
	r.turnWalk("move", "AMR-T1")
	return r
}

// TestCarrierKeysRecord_ASizedWaitCountsAndASizedLiftLifts: the block record,
// not the name, says what a finished block was. A staged removal of a 45x48 at
// a keyed plant sends wait_4548 and, on release, jackload_4548: the wait's
// finish counts its step, and the lift's puts the cart in _TRANSIT and counts
// its step, exactly as Wait and JackLoad do in the block record pin's staged
// release.
func TestCarrierKeysRecord_ASizedWaitCountsAndASizedLiftLifts(t *testing.T) {
	t.Parallel()
	runPinCases(t, []pinCase{
		{name: "staged release [wait X, pickup X, drop]", run: ckrStagedRelease, want: pinLines(
			"removal create b1 LINE1-IN wait_4548",
			"removal finish b1 LINE1-IN wait_4548 -> none | steps_done 1 | OLD@LINE1-IN[removal]",
			"removal waiting -> staged",
			"removal released -> in_transit",
			"removal append b2 LINE1-IN jackload_4548",
			"removal append b3 OUTBOUND-DEST JackUnload",
			"removal finish b2 LINE1-IN jackload_4548 -> lift OLD | steps_done 2 | OLD@_TRANSIT[removal]",
			"removal finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | OLD@_TRANSIT[removal]",
			"removal finished -> delivered | steps_done 3 | OLD@OUTBOUND-DEST",
		)},
	})
}

func ckrStagedRelease(t *testing.T) *pinRig {
	db := testDB(t)
	_, line, _, _, outDest, bp := setupProductionNodes(t, db)
	bigType, big, _, small := ckrTypes(t, db)
	ckrScene(t, db, []string{line.Name}, []string{outDest.Name})
	r := newPinSim(t, db)
	ckrKeyBoth(t, r, big, small)
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, bigType, "CKR-OLD", 2))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "ckr-staged", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: line.Name},
			pinPick(line.Name), pinDrop(outDest.Name),
		},
	})
	r.trackUUID("removal", "ckr-staged")
	r.run("removal")
	r.finish("removal")
	r.stage("removal")
	r.release("removal", "ckr-staged")
	r.finish("removal")
	r.finish("removal")
	r.deliver("removal")
	return r
}
