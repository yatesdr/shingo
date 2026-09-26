package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// unloader_gate_pins_test.go — the three unloader re-pull gates: CLEAR
// (ClearBin), PUSH EMPTY (PushEmptyOut) and U2-landed (applyManualSwap). Each
// fires only when the node's claim says AutoPush, and each asks only the unloader
// that owns the node (rePushOwnUnloader): one Core node-bins read.
//
// BEFORE (pinned at 1bb689bc) each gate called the all-unloader sweep, which read
// Core once per auto-pulling consume loader and pulled a full into any OTHER
// unloader's free window too: 1 + 2 reads and a U1 at W3 in this fixture.
//
// The fixture has two operator-drained consume loaders: L (windows N, N2) and M
// (window W3). N carries a STORED consume claim with AutoPush on, seeded after
// the loader sync so the quarantine does not move it: SynthClaim never sets
// AutoPush, so a stored claim is the only way to switch a gate on at this base.

type ugFixture struct {
	eng        *Engine
	db         *store.DB
	core       *scCore
	n, n2, w3  int64
	nCore      string
	n2Core     string
	w3Core     string
	nProcessID int64
}

func newUGFixture(t *testing.T, prefix string) *ugFixture {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)

	f := &ugFixture{eng: eng, db: db, core: core,
		nCore: prefix + "-N", n2Core: prefix + "-N2", w3Core: prefix + "-W3"}
	unloader := func(key string, windows ...string) protocol.LoaderInfo {
		info := protocol.LoaderInfo{
			Name: key, LoaderKey: "loader:" + key, Role: "consume", Layout: "shared_window",
			Replenishment: "operator", InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES", ConfigGen: 1,
			Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-UG"}},
		}
		for _, w := range windows {
			info.Positions = append(info.Positions, protocol.LoaderPosition{CoreNodeName: w, Kind: "window"})
		}
		return info
	}
	seedCoreLoader(t, eng, unloader(prefix+"-L", f.nCore, f.n2Core), unloader(prefix+"-M", f.w3Core))

	var err error
	f.nProcessID, err = db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle(prefix+"-STYLE", "", f.nProcessID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(f.nProcessID, &styleID), "activate style")
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
	_, err = upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: f.nCore, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "PART-UG", UOPCapacity: 100,
		InboundSource: "FG-SUPER", OutboundDestination: "EMPTY-TOTES", AutoPush: true,
	})
	testutil.MustNoErr(t, err, "stored AutoPush claim at N")
	_, _, claim, err := eng.loadActiveNode(f.n)
	testutil.MustNoErr(t, err, "load N")
	if claim == nil || claim.ID == 0 || !claim.AutoPush {
		t.Fatalf("fixture: want a stored AutoPush claim at %s, got %+v", f.nCore, claim)
	}
	core.set(f.n2Core, false, "")
	core.set(f.w3Core, false, "")
	return f
}

// fulls counts the non-terminal full-ins (U1s) delivering to core.
func (f *ugFixture) fulls(t *testing.T, core string) int {
	t.Helper()
	active, err := f.db.ListActiveOrdersByDeliveryNodeSet([]string{core})
	testutil.MustNoErr(t, err, "list active")
	n := 0
	for _, o := range active {
		if o.OrderType == orders.TypeRetrieve && !o.RetrieveEmpty {
			n++
		}
	}
	return n
}

// checkGate asserts where the gate's pull landed and what it cost in Core reads.
func (f *ugFixture) checkGate(t *testing.T, gate string, reads, wantReads, atN2, atW3 int) {
	t.Helper()
	if got := f.fulls(t, f.n2Core); got != atN2 {
		t.Errorf("%s: U1s to the own loader's free window %s = %d, want %d", gate, f.n2Core, got, atN2)
	}
	if got := f.fulls(t, f.w3Core); got != atW3 {
		t.Errorf("%s: U1s to the OTHER loader's free window %s = %d, want %d", gate, f.w3Core, got, atW3)
	}
	if reads != wantReads {
		t.Errorf("%s: node-bins reads during the tap = %d, want %d", gate, reads, wantReads)
	}
}

// TestPinUnloaderGate_Clear: CLEAR at N. The tap reads no node-bins (the clear's
// own answer says a carrier was there); the gate reads its own unloader once and
// pulls into N2 only.
func TestPinUnloaderGate_Clear(t *testing.T) {
	t.Parallel()
	f := newUGFixture(t, "UGC")
	f.core.set(f.nCore, true, "PART-UG")
	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
	f.checkGate(t, "CLEAR", f.core.nodeBinReads()-before, 1, 1, 0) // the gate's read; the clear answers hadBin
}

// TestPinUnloaderGate_PushEmpty: PUSH EMPTY at N (an empty carrier on it). Same
// shape: one read by the tap, one by the gate.
func TestPinUnloaderGate_PushEmpty(t *testing.T) {
	t.Parallel()
	f := newUGFixture(t, "UGP")
	f.core.set(f.nCore, true, "")
	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.PushEmptyOut(f.n), "PushEmptyOut")
	f.checkGate(t, "PUSH EMPTY", f.core.nodeBinReads()-before, 1+1, 1, 0)
}

// TestPinUnloaderGate_U2Landed: N's U2 lands at the empties destination. The
// landing reads nothing itself; the gate reads its own unloader once.
func TestPinUnloaderGate_U2Landed(t *testing.T) {
	t.Parallel()
	f := newUGFixture(t, "UGL")
	f.core.set(f.nCore, true, "")
	nNode, err := f.db.GetProcessNode(f.n)
	testutil.MustNoErr(t, err, "read N")
	_, _, claim, err := f.eng.loadActiveNode(f.n)
	testutil.MustNoErr(t, err, "load N")
	testutil.MustNoErr(t, f.eng.createUnloaderEmptyOut(nNode, claim), "the U2, without the tap's own gate")
	moves := scMovesFrom(t, f.db, f.nCore)
	if len(moves) != 1 {
		t.Fatalf("fixture: U2s leaving %s = %d, want 1", f.nCore, len(moves))
	}
	f.core.set(f.nCore, false, "") // the carrier left
	before := f.core.nodeBinReads()
	scLand(t, f.eng, f.db, moves[0])
	// N itself is now free too, and it is the own loader's first free window.
	if got := f.fulls(t, f.nCore) + f.fulls(t, f.n2Core); got != 1 {
		t.Errorf("U2-landed: U1s to the own loader = %d, want 1", got)
	}
	if got := f.fulls(t, f.w3Core); got != 0 {
		t.Errorf("U2-landed: U1s to the OTHER loader's free window = %d, want 0", got)
	}
	if reads := f.core.nodeBinReads() - before; reads != 1 {
		t.Errorf("U2-landed: node-bins reads during the landing = %d, want 1", reads)
	}
}

// TestPinUnloaderGate_OffAtACoreOwnedWindow: the same CLEAR at a window with no
// stored claim — the shape every unloader has after the claim quarantine —
// fires nothing, because SynthClaim never sets AutoPush. This is live behaviour
// at both plants until the loader carries the flag (P1).
func TestPinUnloaderGate_OffAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newUGFixture(t, "UGO")
	f.core.set(f.n2Core, true, "PART-UG")
	f.core.set(f.nCore, false, "")
	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.ClearBin(f.n2, ""), "ClearBin at N2 (synthesized claim)")
	if got := f.fulls(t, f.nCore) + f.fulls(t, f.w3Core); got != 0 {
		t.Errorf("U1s after CLEAR at a Core-owned window = %d, want 0 (AutoPush is off on SynthClaim)", got)
	}
	if reads := f.core.nodeBinReads() - before; reads != 0 {
		t.Errorf("node-bins reads = %d, want 0", reads)
	}
}
