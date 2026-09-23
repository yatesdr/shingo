package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// unloader_gate_synth_test.go — the three re-pull gates at a Core-owned unloader
// (NO stored claim): the gate's AutoPush comes from the loader Core owns, through
// SynthClaim, and the pull reaches only that unloader. The companion of
// unloader_gate_pins_test.go, whose fixture had to switch a gate on with a stored
// claim because SynthClaim carried no AutoPush.

// newSynthUGFixture is newUGFixture's layout — unloader L (N, N2), unloader M (W3)
// — with no stored claim anywhere and L's auto_push set to lAuto. M's auto_push is
// on, so a walk that reached M would be visible.
func newSynthUGFixture(t *testing.T, prefix string, lAuto bool) *ugFixture {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)

	f := &ugFixture{eng: eng, db: db, core: core,
		nCore: prefix + "-N", n2Core: prefix + "-N2", w3Core: prefix + "-W3"}
	unloader := func(key string, auto bool, windows ...string) protocol.LoaderInfo {
		info := protocol.LoaderInfo{
			Name: key, LoaderKey: "loader:" + key, Role: "consume", Layout: "shared_window",
			Replenishment: "operator", InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES", ConfigGen: 1,
			Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-UG"}}, AutoPush: auto,
		}
		for _, w := range windows {
			info.Positions = append(info.Positions, protocol.LoaderPosition{CoreNodeName: w, Kind: "window"})
		}
		return info
	}
	seedCoreLoader(t, eng, unloader(prefix+"-L", lAuto, f.nCore, f.n2Core), unloader(prefix+"-M", true, f.w3Core))

	var err error
	f.nProcessID, err = db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	node := func(core string, seq int) int64 {
		id, nerr := db.CreateProcessNode(processes.NodeInput{
			ProcessID: f.nProcessID, CoreNodeName: core, Code: core, Name: core, Sequence: seq, Enabled: true,
		})
		testutil.MustNoErr(t, nerr, "create node "+core)
		_, nerr = db.EnsureProcessNodeRuntime(id)
		testutil.MustNoErr(t, nerr, "runtime "+core)
		return id
	}
	f.n, f.n2, f.w3 = node(f.nCore, 1), node(f.n2Core, 2), node(f.w3Core, 3)
	_, _, claim, err := eng.loadActiveNode(f.n)
	testutil.MustNoErr(t, err, "load N")
	if claim == nil || claim.ID != 0 || claim.AutoPush != lAuto {
		t.Fatalf("fixture: want a synthesized claim at %s with AutoPush=%v, got %+v", f.nCore, lAuto, claim)
	}
	core.set(f.n2Core, false, "")
	core.set(f.w3Core, false, "")
	return f
}

// TestUnloaderGate_SynthClaimAutoPush_ClearAndPushEmpty: with auto_push on the
// loader, CLEAR and PUSH EMPTY at a Core-owned window each pull one full into the
// own unloader's free window and none into the other unloader's, for one Core
// read beyond the tap's own.
func TestUnloaderGate_SynthClaimAutoPush_ClearAndPushEmpty(t *testing.T) {
	t.Parallel()
	t.Run("clear", func(t *testing.T) {
		t.Parallel()
		f := newSynthUGFixture(t, "SGC", true)
		f.core.set(f.nCore, true, "PART-UG")
		before := f.core.nodeBinReads()
		testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
		f.checkGate(t, "CLEAR", f.core.nodeBinReads()-before, 1, 1, 0) // the gate's read; the clear answers hadBin
	})
	t.Run("push_empty", func(t *testing.T) {
		t.Parallel()
		f := newSynthUGFixture(t, "SGP", true)
		f.core.set(f.nCore, true, "")
		before := f.core.nodeBinReads()
		testutil.MustNoErr(t, f.eng.PushEmptyOut(f.n), "PushEmptyOut")
		f.checkGate(t, "PUSH EMPTY", f.core.nodeBinReads()-before, 1+1, 1, 0)
	})
}

// TestUnloaderGate_SynthClaimAutoPush_U2Landed: the U2 landing at a Core-owned
// window re-pulls its own unloader — applyManualSwap reaches it through
// claimAtNode, and its AutoPush through SynthClaim.
func TestUnloaderGate_SynthClaimAutoPush_U2Landed(t *testing.T) {
	t.Parallel()
	f := newSynthUGFixture(t, "SGL", true)
	f.core.set(f.nCore, true, "")
	nNode, err := f.db.GetProcessNode(f.n)
	testutil.MustNoErr(t, err, "read N")
	_, _, claim, err := f.eng.loadActiveNode(f.n)
	testutil.MustNoErr(t, err, "load N")
	f.eng.createUnloaderEmptyOut(nNode, claim)
	moves := scMovesFrom(t, f.db, f.nCore)
	if len(moves) != 1 {
		t.Fatalf("fixture: U2s leaving %s = %d, want 1", f.nCore, len(moves))
	}
	f.core.set(f.nCore, false, "")
	before := f.core.nodeBinReads()
	scLand(t, f.eng, f.db, moves[0])
	if got := f.fulls(t, f.nCore) + f.fulls(t, f.n2Core); got != 1 {
		t.Errorf("U2-landed: U1s to the own unloader = %d, want 1", got)
	}
	if got := f.fulls(t, f.w3Core); got != 0 {
		t.Errorf("U2-landed: U1s to the other unloader = %d, want 0", got)
	}
	if reads := f.core.nodeBinReads() - before; reads != 1 {
		t.Errorf("U2-landed: node-bins reads = %d, want 1", reads)
	}
}

// TestUnloaderGate_SynthClaimAutoPushOff_FiresNothing: auto_push off on the
// loader — the default, and today's live behaviour at both plants — keeps every
// gate off at a Core-owned window.
func TestUnloaderGate_SynthClaimAutoPushOff_FiresNothing(t *testing.T) {
	t.Parallel()
	f := newSynthUGFixture(t, "SGO", false)
	f.core.set(f.nCore, true, "PART-UG")
	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
	f.checkGate(t, "CLEAR (auto_push off)", f.core.nodeBinReads()-before, 0, 0, 0)
}
