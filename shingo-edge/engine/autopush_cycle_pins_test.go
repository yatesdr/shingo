package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// autopush_cycle_pins_test.go — one auto_push unloader cycle (first pinned at e83cccd1):
// a full is on the window, the operator CLEARs it, the empty-out (U2) lifts the
// carrier, the U2 lands. Where the next full is pulled, and what each step costs
// in Core node-bins reads.
//
// Core frees the window at the U2's PICKUP, not its landing: the pickup block
// handler moves the carrier to _TRANSIT before it sends BinPickedUp
// (shingo-core TestU2Pickup_WindowReadsEmptyBeforeTheLanding). So the fake Core
// here reads the window empty from the pickup on.

// newCycleUnloader is one Core-owned, single-window consume loader with auto_push,
// and its window's process node.
func newCycleUnloader(t *testing.T, prefix string) *ugFixture {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	core := newSCCore(t)
	eng.coreClient = NewCoreClient(core.srv.URL)
	f := &ugFixture{eng: eng, db: db, core: core, nCore: prefix + "-N"}
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: prefix, LoaderKey: "loader:" + prefix, Role: "consume", Layout: "shared_window",
		Replenishment: "operator", InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: f.nCore, Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-AC"}}, AutoPush: true,
	})
	var err error
	f.nProcessID, err = db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	f.n, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: f.nProcessID, CoreNodeName: f.nCore, Code: f.nCore, Name: f.nCore, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.EnsureProcessNodeRuntime(f.n)
	testutil.MustNoErr(t, err, "runtime")
	return f
}

// deliveredFullAt seeds the U1 that brought the full now standing on core.
func (f *ugFixture) deliveredFullAt(t *testing.T, nodeID int64, core, payload string) int64 {
	t.Helper()
	id, err := f.db.CreateOrder("ac-u1-"+core, orders.TypeRetrieve, &nodeID, false, 1, core, "", "FG-SUPER", "", false, payload, "", "")
	testutil.MustNoErr(t, err, "create U1")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(id, string(protocol.StatusDelivered)), "deliver U1")
	f.core.set(core, true, payload)
	return id
}

// TestAutoPushCycle_SingleWindow_RefillsAtThePickup: the refill of a
// single-window auto_push unloader comes from the U2's PICKUP, when Core has
// already freed the window. The CLEAR gate counts the tapped window held and is
// covered locally (the carrier is still on it); the landing finds the pull in
// flight locally too. Core gate reads per cycle: CLEAR 0 + pickup 1 + landing 0.
//
// BEFORE (pinned at e83cccd1): the pickup had no gate and the refill came from the
// landing (CLEAR 1 + landing 1 = 2 reads), so the window stood empty for the
// whole U2 trip.
func TestAutoPushCycle_SingleWindow_RefillsAtThePickup(t *testing.T) {
	t.Parallel()
	f := newCycleUnloader(t, "ACS")
	f.deliveredFullAt(t, f.n, f.nCore, "PART-AC")

	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
	if reads := f.core.nodeBinReads() - before; reads != 1 {
		t.Errorf("CLEAR: node-bins reads = %d, want 1 (the tap; the gate is covered locally — the carrier is held)", reads)
	}
	if got := f.fulls(t, f.nCore); got != 0 {
		t.Fatalf("CLEAR: U1s = %d, want 0 — the carrier is still on the window", got)
	}
	u2 := f.onlyU2(t)

	f.pickUp(t, u2)
	if got := f.fulls(t, f.nCore); got != 1 {
		t.Errorf("pickup: U1s = %d, want 1 — the window is free on Core and the pickup re-pulls", got)
	}

	before = f.core.nodeBinReads()
	scLand(t, f.eng, f.db, u2)
	if reads := f.core.nodeBinReads() - before; reads != 0 {
		t.Errorf("landing: node-bins reads = %d, want 0 — the pull is already in flight, known locally", reads)
	}
	if got := f.fulls(t, f.nCore); got != 1 {
		t.Errorf("landing: U1s = %d, want still 1", got)
	}
}

// TestAutoPushLanding_PullsWhenNoPickupWasReported: the landing is the fallback.
// Two ways the pickup re-pull does not happen — the BinPickedUp never reaches the
// Edge, or it does and the re-pull's Core read fails — and in both the landing
// finds nothing in flight and pulls. Without it the window would idle until a
// restart: no full arrives, so no CLEAR comes.
func TestAutoPushLanding_PullsWhenNoPickupWasReported(t *testing.T) {
	t.Parallel()
	t.Run("no_pickup_event", func(t *testing.T) {
		t.Parallel()
		f := newCycleUnloader(t, "ACN")
		f.deliveredFullAt(t, f.n, f.nCore, "PART-AC")
		testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
		u2 := f.onlyU2(t)
		f.core.set(f.nCore, false, "") // Core freed the window; the Edge was not told
		before := f.core.nodeBinReads()
		scLand(t, f.eng, f.db, u2)
		if got := f.fulls(t, f.nCore); got != 1 {
			t.Errorf("landing without a pickup: U1s = %d, want 1", got)
		}
		if reads := f.core.nodeBinReads() - before; reads != 1 {
			t.Errorf("landing without a pickup: node-bins reads = %d, want 1", reads)
		}
	})
	t.Run("pickup_read_failed", func(t *testing.T) {
		t.Parallel()
		f := newCycleUnloader(t, "ACF")
		f.deliveredFullAt(t, f.n, f.nCore, "PART-AC")
		testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
		u2 := f.onlyU2(t)
		live := f.eng.coreClient
		f.eng.coreClient = NewCoreClient(testCoreURL) // Core unreachable for the pickup's read
		f.pickUp(t, u2)
		f.eng.coreClient = live
		if got := f.fulls(t, f.nCore); got != 0 {
			t.Fatalf("pickup with Core unreachable: U1s = %d, want 0 (the seam fails closed)", got)
		}
		scLand(t, f.eng, f.db, u2)
		if got := f.fulls(t, f.nCore); got != 1 {
			t.Errorf("landing after a failed pickup read: U1s = %d, want 1", got)
		}
	})
}

// onlyU2 returns the one U2 leaving N.
func (f *ugFixture) onlyU2(t *testing.T) storeorders.Order {
	t.Helper()
	moves := scMovesFrom(t, f.db, f.nCore)
	if len(moves) != 1 {
		t.Fatalf("fixture: U2s = %d, want 1", len(moves))
	}
	return moves[0]
}

// pickUp is the U2 lifting the carrier: Core has already moved it to _TRANSIT, then
// tells the Edge.
func (f *ugFixture) pickUp(t *testing.T, u2 storeorders.Order) {
	t.Helper()
	f.core.set(f.nCore, false, "")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(u2.ID, string(protocol.StatusInTransit)), "in transit")
	f.eng.HandleBinPickedUp(u2.UUID, 77, f.nCore)
}

// TestPinAutoPushGate_ClearIsTheOnlyTriggerWhileAPeerFullIsUnconfirmed names the
// window shape in which the CLEAR gate fires something no other gate would fire
// at that moment: a shared unloader with two windows and one payload. N holds a
// delivered, unconfirmed full; N2 is free. The seam wants ONE U1 per payload and
// counts N's delivered U1 as in flight, so a re-pull while N's full stands —
// which is what N2's own departure would have run — fires nothing. The CLEAR at N
// confirms that U1, and its gate then pulls the next full into N2.
func TestPinAutoPushGate_ClearIsTheOnlyTriggerWhileAPeerFullIsUnconfirmed(t *testing.T) {
	t.Parallel()
	f := newSynthUGFixture(t, "ACC", true)
	f.deliveredFullAt(t, f.n, f.nCore, "PART-UG")

	nNode, err := f.db.GetProcessNode(f.n2)
	testutil.MustNoErr(t, err, "read N2")
	f.eng.rePushOwnUnloader(nNode) // what a re-pull at N2's freeing would see
	if got := f.fulls(t, f.n2Core); got != 0 {
		t.Fatalf("re-pull while N's full is unconfirmed: U1s to N2 = %d, want 0 (the payload's one in flight)", got)
	}

	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin at N")
	if reads := f.core.nodeBinReads() - before; reads != 1+1 {
		t.Errorf("CLEAR at N: node-bins reads = %d, want 1 (hadBin pre-read) + 1 (the gate)", reads)
	}
	if got := f.fulls(t, f.n2Core); got != 1 {
		t.Errorf("CLEAR at N: U1s to N2 = %d, want 1 — the confirm frees the payload's budget and the gate fires", got)
	}
}

// TestPinAutoPushGate_PushEmptyFillsASiblingNothingElseTriggered names the shape
// in which the PUSH EMPTY gate fires: a sibling window N2 was freed by an event
// that has no re-pull of its own — here its U1 was cancelled — and PUSH EMPTY at
// N is the next thing to ask. The carrier on N is still resident, so the pull
// goes to N2.
func TestPinAutoPushGate_PushEmptyFillsASiblingNothingElseTriggered(t *testing.T) {
	t.Parallel()
	f := newSynthUGFixture(t, "ACP", true)
	n2 := f.n2
	cancelled, err := f.db.CreateOrder("acp-u1-n2", orders.TypeRetrieve, &n2, false, 1, f.n2Core, "", "FG-SUPER", "", false, "PART-UG", "", "")
	testutil.MustNoErr(t, err, "create N2's U1")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(cancelled, string(protocol.StatusCancelled)), "cancel it")
	f.core.set(f.nCore, true, "")

	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.PushEmptyOut(f.n), "PushEmptyOut at N")
	if reads := f.core.nodeBinReads() - before; reads != 1+1 {
		t.Errorf("PUSH EMPTY at N: node-bins reads = %d, want 1 (the tap) + 1 (the gate)", reads)
	}
	if got := f.fulls(t, f.n2Core); got != 1 {
		t.Errorf("PUSH EMPTY at N: U1s to N2 = %d, want 1", got)
	}
}

// TestAutoPushLanding_MultiPayloadWindowCountCovers: one window, two payloads.
// The pickup re-pull fills the window with a full of one payload; the other has
// no full-in, but there is no window left for it, so the landing is covered by
// the window count and asks Core nothing.
func TestAutoPushLanding_MultiPayloadWindowCountCovers(t *testing.T) {
	t.Parallel()
	f := newCycleUnloader(t, "ACM")
	seedCoreLoader(t, f.eng, protocol.LoaderInfo{
		Name: "ACM", LoaderKey: "loader:ACM", Role: "consume", Layout: "shared_window",
		Replenishment: "operator", InboundSource: "FG-SUPER", OutboundDest: "EMPTY-TOTES", ConfigGen: 2,
		Positions: []protocol.LoaderPosition{{CoreNodeName: f.nCore, Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-AC"}, {PayloadCode: "PART-AD"}}, AutoPush: true,
	})
	f.deliveredFullAt(t, f.n, f.nCore, "PART-AC")
	testutil.MustNoErr(t, f.eng.ClearBin(f.n, ""), "ClearBin")
	u2 := f.onlyU2(t)
	f.pickUp(t, u2)
	if got := f.fulls(t, f.nCore); got != 1 {
		t.Fatalf("pickup: U1s = %d, want 1 (one window)", got)
	}
	before := f.core.nodeBinReads()
	scLand(t, f.eng, f.db, u2)
	if reads := f.core.nodeBinReads() - before; reads != 0 {
		t.Errorf("landing: node-bins reads = %d, want 0 — every window already has its full-in", reads)
	}
}
