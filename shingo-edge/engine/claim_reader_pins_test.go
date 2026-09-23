package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// claim_reader_pins_test.go — the readers that resolved a node's claim from
// stored style_node_claims only (requestedClaimAtNode, pinned at 1bb689bc) and
// now use claimAtNode, read at a Core-owned loader window: a node in Core's loader cache with NO stored claim,
// which after the claim quarantine is every loader window. claimAtNode (stored,
// else SynthClaim) answers differently ONLY there; a node with a stored claim
// keeps it, and a node that belongs to no loader synthesizes nothing.
//
// One reader changed a decision (the delivered handler, both arms, now binds the
// carrier); one changed only a refusal's sentence (pairedNodeOf); every other
// site gives the same answer through a synthesized claim, and its pin states it.

// crFixture is one Core-owned consume window W, plus a plain node P on the same
// process that belongs to no loader and has no claim.
type crFixture struct {
	eng          *Engine
	db           *store.DB
	w, p         int64
	wCore, pCore string
}

func newCRFixture(t *testing.T, prefix string) *crFixture {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()
	f := &crFixture{eng: eng, db: db, wCore: prefix + "-W", pCore: prefix + "-P"}
	procID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
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
	f.w, f.p = node(f.wCore, 1), node(f.pCore, 2)
	info := sharedLoaderInfo(f.wCore, "consume", "operator", "PART-CR", 0, 0)
	info.OutboundDest = "EMPTY-TOTES"
	seedCoreLoader(t, eng, info)
	wNode, err := db.GetProcessNode(f.w)
	testutil.MustNoErr(t, err, "read W")
	if requestedClaimAtNode(db, wNode) != nil || eng.claimAtNode(wNode) == nil {
		t.Fatalf("fixture: W must have no stored claim and a synthesized one")
	}
	return f
}

// deliverU1 files a U1 at W tracked at W and drives it to delivered with bin 991
// carrying 30.
func (f *crFixture) deliverU1(t *testing.T, at int64, core string) {
	t.Helper()
	id, err := f.db.CreateOrder("cr-u1-"+core, orders.TypeRetrieve, &at, false, 1, core, "", "FG-SUPER", "", false, "PART-CR", "", "")
	testutil.MustNoErr(t, err, "create U1")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(id, string(protocol.StatusDispatched)), "dispatch")
	bin, uop := int64(991), 30
	testutil.MustNoErr(t, f.eng.orderMgr.HandleDeliveredWithExpiry("cr-u1-"+core, "delivered", nil, &bin, &uop, nil, 0, core, ""),
		"deliver")
}

func (f *crFixture) boundBin(t *testing.T, id int64) (bin *int64, uop int, claimID *int64) {
	t.Helper()
	rt, err := f.db.GetProcessNodeRuntime(id)
	testutil.MustNoErr(t, err, "runtime")
	return rt.ActiveBinID, rt.RemainingUOPCached, rt.ActiveClaimID
}

// wiring_delivered.go handleNodeOrderDelivered — a delivery at a Core-owned
// window binds the carrier, and writes NO claim id: the synthesized claim has
// none. Before (1bb689bc) it raised "delivered but NOT bound … no active claim at
// node" and bound nothing.
func TestPinClaimReader_DeliveredAtACoreOwnedWindow_Binds(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRD")
	f.deliverU1(t, f.w, f.wCore)
	bin, uop, claimID := f.boundBin(t, f.w)
	if bin == nil || *bin != 991 || uop != 30 {
		t.Errorf("W runtime after delivery = bin %v uop %d, want bin 991 with 30", bin, uop)
	}
	if claimID != nil {
		t.Errorf("active_claim_id = %d, want nil — a synthesized claim persists no id", *claimID)
	}
}

// wiring_delivered.go handleFallbackDelivered (a delivery with no Edge row) —
// the same reader: binds at a Core-owned window, no claim id. Before: unbound.
func TestPinClaimReader_FallbackDeliveredAtACoreOwnedWindow_Binds(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRF")
	bin, uop := int64(992), 30
	if err := f.eng.orderMgr.HandleDeliveredWithExpiry("no-edge-row", "delivered", nil, &bin, &uop, nil, 0, f.wCore, ""); err == nil {
		t.Fatalf("fixture: an unknown uuid must take the fallback path (the manager reports not-found)")
	}
	got, u, claimID := f.boundBin(t, f.w)
	if got == nil || *got != 992 || u != 30 {
		t.Errorf("W runtime after the fallback delivery = bin %v uop %d, want bin 992 with 30", got, u)
	}
	if claimID != nil {
		t.Errorf("active_claim_id = %d, want nil", *claimID)
	}
}

// The control: a node that belongs to no loader and has no claim is unbound
// before and after — claimAtNode synthesizes nothing for it.
func TestPinClaimReader_DeliveredAtAPlainNodeWithNoClaim_DoesNotBind(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRP")
	f.deliverU1(t, f.p, f.pCore)
	if bin, uop, _ := f.boundBin(t, f.p); bin != nil || uop != 0 {
		t.Errorf("plain node runtime after delivery = bin %v uop %d, want unbound", bin, uop)
	}
}

// wiring_status_changed.go handleSequentialBackfill — SAME ANSWER. The sequential backfill needs a
// sequential claim; nil and a synthesized manual_swap claim both return.
func TestPinClaimReader_SequentialBackfill_NoneAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRS")
	move, err := f.eng.orderMgr.CreateMoveOrder(&f.w, 1, f.wCore, "EMPTY-TOTES", true, orders.NoDemand())
	testutil.MustNoErr(t, err, "create move")
	testutil.MustNoErr(t, f.db.SetProcessNodeRuntimeActiveOrder(f.w, &move.ID), "point W at it")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(move.ID, string(protocol.StatusDispatched)), "dispatch")
	testutil.MustNoErr(t, f.eng.orderMgr.TransitionOrder(move.ID, protocol.StatusInTransit, "moving"), "in transit")
	all, err := f.db.ListOrders()
	testutil.MustNoErr(t, err, "list")
	for _, o := range all {
		if o.OrderType == orders.TypeComplex {
			t.Errorf("a backfill (complex order %d) was minted at a loader window", o.ID)
		}
	}
}

// leg_departure.go stampDepartureIfLeftCell and settleCellPlacement — SAME ANSWER. A loader window's legs are simple
// moves with no steps, so neither the departure stamp nor the placement settle
// has anything to act on through a synthesized claim either.
func TestPinClaimReader_DepartureAndSettle_NothingAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRL")
	move, err := f.eng.orderMgr.CreateMoveOrder(&f.w, 1, f.wCore, "EMPTY-TOTES", true, orders.NoDemand())
	testutil.MustNoErr(t, err, "create move")
	f.eng.stampDepartureIfLeftCell(move, f.wCore)
	f.eng.settleCellPlacement(f.w)
	got, err := f.db.GetOrder(move.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.CellLeftAt != nil || got.Departed {
		t.Errorf("move at a loader window: cell_left_at=%v departed=%v, want neither", got.CellLeftAt, got.Departed)
	}
}

// operator_stations.go CanAcceptOrders — SAME ANSWER. The Core-loader shortcut
// above the claim read already returns true for a loader window, so that read is
// not reached for one.
func TestPinClaimReader_CanAcceptOrders_TrueAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRA")
	if ok, why := f.eng.CanAcceptOrders(f.w); !ok {
		t.Errorf("CanAcceptOrders(W) = false (%s), want true", why)
	}
}

// operator_ab_cycling.go flipTargetReady and pairedNodeOf — SAME DECISION, one
// message. A flip at a loader window is refused either way: flipTargetReady
// answers first (no bin, no changeover — its consume arm is reached only through
// a changeover task, which a loader window has none of), and pairedNodeOf
// refuses the confirmed flip. Through the synthesized claim it says "is not part
// of an A/B pair"; before (1bb689bc) it said "has no active claim".
func TestPinClaimReader_FlipAtACoreOwnedWindow_IsRefused(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRB")
	err := f.eng.FlipABNode(f.w, FlipRequest{CalledBy: "test"})
	if err == nil || !strings.Contains(err.Error(), "has no bin on it") {
		t.Errorf("unconfirmed flip at W = %v, want the no-bin refusal", err)
	}
	err = f.eng.FlipABNode(f.w, FlipRequest{CalledBy: "test", Confirm: true})
	if err == nil || !strings.Contains(err.Error(), "is not part of an A/B pair") {
		t.Errorf("confirmed flip at W = %v, want pairedNodeOf's refusal", err)
	}
}

// operator_changeover_release.go linePullsFrom — SAME ANSWER. Not a paired position.
func TestPinClaimReader_LinePullsFrom_NothingAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRR")
	pulling, own, partner, err := f.eng.linePullsFrom(f.w)
	if pulling || own != f.wCore || partner != "" || err != nil {
		t.Errorf("linePullsFrom(W) = (%v, %q, %q, %v), want (false, %q, \"\", nil)", pulling, own, partner, err, f.wCore)
	}
}

// changeover_applier.go clearActivePullForEvacuate — SAME ANSWER. An evacuate clears the pull bit on
// the node alone; a loader window has no A/B partner to clear.
func TestPinClaimReader_ClearActivePull_OnlyTheNodeAtACoreOwnedWindow(t *testing.T) {
	t.Parallel()
	f := newCRFixture(t, "CRE")
	testutil.MustNoErr(t, f.db.SetActivePull(f.w, true), "set W")
	testutil.MustNoErr(t, f.db.SetActivePull(f.p, true), "set P")
	f.eng.clearActivePullForEvacuate(f.w)
	rtW, err := f.db.GetProcessNodeRuntime(f.w)
	testutil.MustNoErr(t, err, "runtime W")
	rtP, err := f.db.GetProcessNodeRuntime(f.p)
	testutil.MustNoErr(t, err, "runtime P")
	if rtW.ActivePull || !rtP.ActivePull {
		t.Errorf("after evacuate at W: W active_pull=%v P active_pull=%v, want false / true", rtW.ActivePull, rtP.ActivePull)
	}
}
