package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// produce_clear_repush_pins_test.go — CLEAR at a produce (loader) window
// re-pushes only the loader that owns the window (rePushOwnLoader): one Core
// node-bins read, and nothing staged at another loader.
//
// BEFORE (pinned at e83cccd1) it walked EVERY operator-staged produce loader with
// a read each, and staged an empty at any of them that had a free window —
// including a loader whose window this tap did not free.
//
// Two Core-owned, operator-staged produce loaders: A (window WA, the one being
// cleared) and B (window WB, free, nothing in flight).
func TestProduceClear_RePushesOnlyItsOwnLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)

	const wa, wb = "PCR-WA", "PCR-WB"
	seedCoreLoader(t, eng,
		sharedLoaderInfo(wa, "produce", "operator", "PART-PC", 0, 0),
		sharedLoaderInfo(wb, "produce", "operator", "PART-PC", 0, 0))
	procID, err := db.CreateProcess("PCR-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	node := func(core string, seq int) int64 {
		id, nerr := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: core, Code: core, Name: core, Sequence: seq, Enabled: true,
		})
		testutil.MustNoErr(t, nerr, "create node "+core)
		_, nerr = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, nerr, "runtime "+core)
		return id
	}
	waID := node(wa, 1)
	node(wb, 2)
	core.set(wa, true, "PART-PC") // the carrier being cleared stays on WA
	core.set(wb, false, "")

	before := core.nodeBinReads()
	testutil.MustNoErr(t, eng.ClearBin(waID, ""), "ClearBin at a produce window")
	if n := scActiveEmptiesTo(t, db, wb); n != 0 {
		t.Errorf("empties staged at the OTHER loader's window %s = %d, want 0", wb, n)
	}
	if n := scActiveEmptiesTo(t, db, wa); n != 0 {
		t.Errorf("empties staged at %s = %d, want 0 (its carrier is still resident)", wa, n)
	}
	if reads := core.nodeBinReads() - before; reads != 1 {
		t.Errorf("node-bins reads during the produce CLEAR = %d, want 1 (the own loader)", reads)
	}
}
