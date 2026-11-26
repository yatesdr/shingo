// One seat claimed by two stations, both sides real (owner ruling 2026-11-22:
// the state is flagged, greyed and corrected, never refused or arbitrated).
// Two paths follow from it, and each is run here and pinned as it behaves:
//
//   - DOUBLE COUNT: the second Edge binds the carrier the first holds (its own
//     record count, or a reseat after a cancelled changeover), and Core applies
//     the counts of each station, its cursor being per station
//     (shingocore uop applier).
//   - TWO ORDERS: each level keeper reads only the orders of its own Edge, so
//     both ask for the one seat. Core refuses neither.
//
// Real: the Core engine over Postgres and the fleet simulator, its router and
// its asks; two Edge engines over SQLite with their level keepers (the test
// fires each ticker), their claim publishers and their count accumulators.
// Carried by the Bus: every message, each Edge's bins on Core's own feed
// (harness.StartBinsFeed). Stood in for: the broker.
//
//go:build docker

package scenarios

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
	edgeuop "shingoedge/uop"
)

const (
	tcSeat   = "TC-SEAT"
	tcMarket = "TC-MKT"
	tcDest   = "TC-DEST"
	tcInStg  = "TC-INSTG"
	tcOutStg = "TC-OUTSTG"
	tcPart   = "TC-PART"
	tcPartB  = "TC-PART-B"
	tcPack   = 40
	tcLevel  = 10
)

// claimantEdge is one of the two stations claiming the seat.
type claimantEdge struct {
	uid     string
	edge    *edgeharness.Edge
	acc     *edgeuop.Mutator
	kick    *kickTicker
	adjust  *protocol.Ingestor
	bus     *harness.Bus
	process int64
	node    int64
	style   int64
	styleB  int64
}

// twoClaimants is the seat, its market and the two stations.
type twoClaimants struct {
	t    *testing.T
	core kscCore
	a, b *claimantEdge
}

func newTwoClaimants(t *testing.T) *twoClaimants {
	t.Helper()
	c := &twoClaimants{t: t, core: startKeepStagedCore(t)}
	db := c.core.eng.DB()

	stor, err := db.GetNodeTypeByCode("STOR")
	if err != nil {
		stor = &corenodes.NodeType{Code: "STOR", Name: "Storage Slot"}
		mustNil(t, db.CreateNodeType(stor), "STOR type")
	}
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	mustNil(t, err, "NGRP type")
	node := func(n *corenodes.Node) *corenodes.Node {
		n.Enabled = true
		mustNil(t, db.CreateNode(n), "node "+n.Name)
		return n
	}
	for _, name := range []string{tcSeat, tcDest, tcInStg, tcOutStg} {
		node(&corenodes.Node{Name: name})
	}
	market := node(&corenodes.Node{Name: tcMarket, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	tote := &corebins.BinType{Code: "TC-TOTE"}
	mustNil(t, db.CreateBinType(tote), "bin type")
	for _, code := range []string{tcPart, tcPartB} {
		p := &corepayloads.Payload{Code: code, UOPCapacity: tcPack}
		mustNil(t, db.CreatePayload(p), "payload "+code)
		mustNil(t, db.SetPayloadBinTypes(p.ID, []int64{tote.ID}), "carrier rule "+code)
	}
	for i, part := range []string{tcPart, tcPart, tcPart, tcPartB} {
		slot := node(&corenodes.Node{Name: fmt.Sprintf("%s-%d", tcMarket, i+1), ParentID: &market.ID, NodeTypeID: &stor.ID})
		b := &corebins.Bin{BinTypeID: tote.ID, Label: fmt.Sprintf("TC-BIN-%d", i+1), NodeID: &slot.ID, Status: "available"}
		mustNil(t, db.CreateBin(b), "bin")
		mustNil(t, db.SetBinManifest(b.ID, `{"items":[]}`, part, tcPack), "manifest")
		mustNil(t, db.ConfirmBinManifest(b.ID, ""), "confirm")
	}

	// Core's tables as main builds them, less what this cell leaves out: the
	// five order types, the claims and count subjects, the asks, the watch list
	// and the heartbeat, and every hook but the bins feed's (FeedHooks): each
	// Edge's bins come on Core's own feed.
	coreSide := coreharness.NewCore(t, c.core.eng,
		coreharness.RoutesOnly(protocol.TypeOrderRequest, protocol.TypeComplexOrderRequest, protocol.TypeOrderCancel,
			protocol.TypeOrderReceipt, protocol.TypeOrderRelease, protocol.SubjectPlantClaims, protocol.SubjectBinUOPDelta,
			protocol.SubjectEdgeAsk, protocol.SubjectBinsWatch, protocol.SubjectEdgeHeartbeat),
		coreharness.HooksOnly(coreharness.FeedHooks...))
	in := coreSide.Ingestor

	c.a = c.claimant("edge.claim-a", "TC-PROC-A")
	c.b = c.claimant("edge.claim-b", "TC-PROC-B")
	for _, e := range []*claimantEdge{c.a, c.b} {
		e.bus = harness.NewBus(t,
			harness.EdgeSide{EdgeStore: e.edge.DB, EdgeIngestor: c.a.edge.Ingestor},
			harness.CoreSide{CoreStore: db, CoreIngestor: in})
		// Core has one outbox and the broker one topic: whichever bus drains
		// it, every row reaches both consumers, each taking its own by the
		// address on the envelope.
		e.bus.RewriteCoreToEdge(func(raw []byte) []byte {
			c.a.adjust.HandleRaw(raw)
			c.b.adjust.HandleRaw(raw)
			c.b.edge.Ingestor.HandleRaw(raw)
			return raw
		})
	}
	for _, e := range []*claimantEdge{c.a, c.b} {
		harness.StartBinsFeed(t, e.bus, coreSide.Data, e.edge)
	}
	// The claims of each station, as its boot publishes them.
	for _, e := range []*claimantEdge{c.a, c.b} {
		mustNil(t, edgemessaging.NewPlantClaimsPublisher(e.edge.DB, e.uid, 0).PublishAll(), "publish claims of "+e.uid)
	}
	c.settle()
	sts, err := db.StationsClaimingNode(tcSeat)
	mustNil(t, err, "claimants")
	if !slices.Equal(sts, []string{c.a.uid, c.b.uid}) {
		t.Fatalf("setup: %s is claimed by %v, want both stations", tcSeat, sts)
	}
	return c
}

// twoClaimantsSchema is what both stations registered at: a v0.19.0 Edge's.
const twoClaimantsSchema = 59

// claimant is one station with a process of its own claiming the seat: a
// single-robot consume cell that reorders itself at tcLevel.
func (c *twoClaimants) claimant(uid, process string) *claimantEdge {
	t := c.t
	t.Helper()
	db := c.core.eng.DB()
	if _, err := db.Exec(`INSERT INTO edge_registry (station_uid, display_name, schema_version) VALUES ($1, $2, $3)`,
		uid, process+" station", twoClaimantsSchema); err != nil {
		t.Fatalf("register %s: %v", uid, err)
	}
	kicks := newKickClock()
	e := &claimantEdge{uid: uid, edge: edgeharness.NewEdge(t, uid, func(ec *edgeengine.Config) { ec.DemandReconcileClock = kicks })}
	select {
	case e.kick = <-kicks.took:
	case <-time.After(2 * time.Second):
		t.Fatal("the level keeper never took its ticker")
	}
	edb := e.edge.DB
	e.acc = edgeuop.New(edb, uid, edb, edb)
	e.edge.Engine.SetInventoryDeltaSink(e.acc)
	var err error
	for i, code := range []string{tcPart, tcPartB} {
		mustNil(t, edb.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: int64(i + 1), Name: code, Code: code, UOPCapacity: tcPack}), "catalog")
	}
	e.edge.Engine.SetPayloadBinTypes([]protocol.PayloadBinTypeInfo{
		{PayloadCode: tcPart, BinTypeCode: "TC-TOTE"}, {PayloadCode: tcPartB, BinTypeCode: "TC-TOTE"}})
	e.process, err = edb.CreateProcess(process, "", "", "", false)
	mustNil(t, err, "process")
	e.node, err = edb.CreateProcessNode(processes.NodeInput{
		ProcessID: e.process, CoreNodeName: tcSeat, Code: "TC1", Name: tcSeat, Sequence: 1, Enabled: true})
	mustNil(t, err, "process node")
	_, err = edb.EnsureProcessNodeRuntime(e.node)
	mustNil(t, err, "runtime")
	var claim int64
	for _, s := range []struct {
		name, part string
		id         *int64
	}{{"TC-RUN", tcPart, &e.style}, {"TC-ALT", tcPartB, &e.styleB}} {
		*s.id, err = edb.CreateStyle(s.name, "", e.process)
		mustNil(t, err, "style "+s.name)
		id, err := edb.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: *s.id, CoreNodeName: tcSeat, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSingleRobot,
			PayloadCode: s.part, UOPCapacity: tcPack, ReorderPoint: tcLevel, AutoReorder: domain.Ptr(true),
			InboundSource: tcMarket, OutboundDestination: tcDest, InboundStaging: tcInStg, OutboundStaging: tcOutStg,
		})
		mustNil(t, err, "claim "+s.name)
		if s.name == "TC-RUN" {
			claim = id
		}
	}
	mustNil(t, edb.SetActiveStyle(e.process, &e.style), "active style")
	mustNil(t, edb.SetProcessNodeRuntime(e.node, &claim, 0), "an empty seat")

	// The two count subjects the Edge harness router does not carry, as
	// cmd/shingoedge main.go routes them.
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectUOPAdjustment, func(_ *protocol.Envelope, adj *protocol.UOPAdjustment) {
		e.edge.Engine.HandleUOPAdjustment(*adj)
	})
	router.RegisterSubject(subjects, protocol.SubjectBinEpochRefresh, func(_ *protocol.Envelope, rf *protocol.BinEpochRefresh) {
		e.edge.Engine.HandleBinEpochRefresh(*rf)
	})
	types := router.New[string]()
	router.Register(types, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { subjects.Dispatch(env, p) })
	e.adjust = protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == uid || hdr.Dst.Station == protocol.StationBroadcast
	})
	e.adjust.Dispatch = func(env *protocol.Envelope) { types.Dispatch(env, env.Type) }
	return e
}

func (c *twoClaimants) pump() int { return c.b.bus.PumpEdgeOutbox() + c.a.bus.PumpAll() }

func (c *twoClaimants) settle() {
	for i := 0; i < 100; i++ {
		if c.pump() == 0 {
			time.Sleep(30 * time.Millisecond)
			if c.pump() == 0 {
				return
			}
		}
	}
}

func (c *twoClaimants) eventually(what string, ok func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c.settle()
		c.core.eng.RunFulfillmentScan()
		c.settle()
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s; orders at Core: %s", what, c.orders())
		}
	}
}

// sweep is one pass of the level keeper of e. The ticker is unbuffered, so the
// second tick is taken only once the first pass is over.
func (c *twoClaimants) sweep(e *claimantEdge) {
	c.t.Helper()
	for i := 0; i < 2; i++ {
		select {
		case e.kick.c <- time.Now():
		case <-time.After(15 * time.Second):
			c.t.Fatalf("the level keeper of %s did not take the tick", e.uid)
		}
	}
	c.settle()
}

// ordersOf is the live orders at Core that a station asked.
func (c *twoClaimants) ordersOf(e *claimantEdge) []*coreorders.Order {
	c.t.Helper()
	all, err := c.core.eng.DB().ListActiveOrders()
	mustNil(c.t, err, "active orders")
	var out []*coreorders.Order
	for _, o := range all {
		if o.StationUID == e.uid {
			out = append(out, o)
		}
	}
	return out
}

func (c *twoClaimants) orders() string {
	all, err := c.core.eng.DB().ListActiveOrders()
	if err != nil {
		return err.Error()
	}
	var s string
	for _, o := range all {
		s += fmt.Sprintf("[%d %s %s %s->%s %s cause=%q vendor=%t]", o.ID, o.StationUID, o.OrderType, o.SourceNode,
			o.DeliveryNode, o.Status, o.QueueCause, o.VendorOrderID != "")
	}
	return s
}

// carry has the fleet carry a plain order to its end.
func (c *twoClaimants) carry(o *coreorders.Order) {
	c.t.Helper()
	c.eventually("a vendor order for "+o.EdgeUUID, func() bool {
		fresh, err := c.core.eng.DB().GetOrderByUUID(o.EdgeUUID)
		mustNil(c.t, err, "order")
		o = fresh
		return o.VendorOrderID != ""
	})
	c.drive(o, "RUNNING", "FINISHED")
}

// seatBin is the bin Core has standing at the seat, 0 for none, with its count.
func (c *twoClaimants) seatBin() (id int64, count int) {
	c.t.Helper()
	rows, err := c.core.eng.DB().Query(`SELECT b.id, b.uop_remaining FROM bins b JOIN nodes n ON n.id = b.node_id
		WHERE n.name = $1 AND b.status <> $2`, tcSeat, "retired")
	mustNil(c.t, err, "the bin at the seat")
	defer rows.Close()
	for rows.Next() {
		mustNil(c.t, rows.Scan(&id, &count), "scan")
	}
	mustNil(c.t, rows.Err(), "rows")
	return id, count
}

// seat is what the seat of e holds: the bin, 0 for none, and the count.
func (c *twoClaimants) seat(e *claimantEdge) (bin int64, count int) {
	c.t.Helper()
	rt, err := e.edge.DB.GetProcessNodeRuntime(e.node)
	mustNil(c.t, err, "runtime of "+e.uid)
	if rt.ActiveBinID != nil {
		bin = *rt.ActiveBinID
	}
	return bin, rt.RemainingUOPCached
}

// seatBooks is the three books of the seat: the bin and the count at Core, and
// at each station.
type seatBooks struct {
	bin  int64
	core int
	aBin int64
	a    int
	bBin int64
	b    int
}

func (s seatBooks) String() string {
	return fmt.Sprintf("Core bin %d at %d, A bin %d at %d, B bin %d at %d", s.bin, s.core, s.aBin, s.a, s.bBin, s.b)
}

// wantBooks fails unless the three books read as given.
func (c *twoClaimants) wantBooks(step string, want seatBooks) {
	c.t.Helper()
	var got seatBooks
	got.bin, got.core = c.seatBin()
	got.aBin, got.a = c.seat(c.a)
	got.bBin, got.b = c.seat(c.b)
	if got != want {
		c.t.Fatalf("%s: %v; want %v", step, got, want)
	}
	c.t.Logf("%s: %v", step, got)
}

// deliveredToA has the level keeper of A ask for the bare seat and the fleet
// carry that order there: A binds on the landing, and B hears nothing.
func (c *twoClaimants) deliveredToA() int64 {
	c.t.Helper()
	c.sweep(c.a)
	asked := c.ordersOf(c.a)
	if len(asked) != 1 {
		c.t.Fatalf("the keeper of A at a bare seat made %d orders, want 1: %s", len(asked), c.orders())
	}
	c.carry(asked[0])
	c.eventually("A bound to its delivery", func() bool { b, _ := c.seat(c.a); return b != 0 })
	bin, _ := c.seatBin()
	c.wantBooks("delivered to A", seatBooks{bin, tcPack, bin, tcPack, 0, 0})
	return bin
}

// strokes is n consume strokes counted by e and flushed to Core.
func (c *twoClaimants) strokes(e *claimantEdge, n int) {
	c.t.Helper()
	for i := 0; i < n; i++ {
		e.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
			Payload: edgeengine.CounterDeltaEvent{ProcessID: e.process, StyleID: e.style, Delta: 1}})
	}
	e.acc.Flush()
	c.settle()
}

// whilePumping runs an act that waits on an answer from Core, carrying the
// messages until it returns.
func (c *twoClaimants) whilePumping(what string, act func() error) {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- act() }()
	for {
		select {
		case err := <-done:
			mustNil(c.t, err, what)
			c.settle()
			return
		case <-time.After(10 * time.Millisecond):
			c.pump()
		}
	}
}

// THE DOUBLE-COUNT PATH (pewter §9, 5660 §9). A holds the delivered bin. A
// person counts it on the bins page: a correction alone binds nobody. Of the
// two doors the reviews read for B to bind the same bin, one does and one does
// not:
//   - its own count at the line does not bind (setSeatCountLocked writes the
//     count and leaves the pointer), so only A draws the bin;
//   - the reseat a cancelled changeover asks binds it, and the two stations
//     each count five parts of the one line: Core takes ten off for five made.
//
// Both corrections reach A, as lifecycle (owner ruling 2026-11-22; at 6bd55616
// neither did, and A kept a count nobody believed).
func TestScenario_TwoClaimants_DoubleCountPath(t *testing.T) {
	t.Parallel()
	t.Run("the second line records a count", func(t *testing.T) {
		t.Parallel()
		c := newTwoClaimants(t)
		bin := c.deliveredToA()

		fresh, err := c.core.eng.DB().GetBin(bin)
		mustNil(t, err, "bin")
		_, err = c.core.eng.BinService().RecordCount(fresh, 35, "clerk")
		mustNil(t, err, "the count on the bins page")
		c.settle()
		c.wantBooks("counted 35 on the bins page", seatBooks{bin, 35, bin, 35, 0, 0})

		c.whilePumping("the count at the line of B", func() error {
			return c.b.edge.Engine.RecordBinCount(c.b.node, 30, "operator-b")
		})
		c.wantBooks("counted 30 at the line of B", seatBooks{bin, 30, bin, 30, 0, 30})

		c.strokes(c.a, 5)
		c.strokes(c.b, 5)
		c.wantBooks("five parts, drawn by A alone", seatBooks{bin, 25, bin, 25, 0, 30})

		rt, err := c.b.edge.DB.GetProcessNodeRuntime(c.b.node)
		mustNil(t, err, "runtime of B")
		if rt.PendingUOPDelta != 0 {
			t.Fatalf("B holds a pile of %d, want none: an unbound consume seat logs its strokes as no cart", rt.PendingUOPDelta)
		}
	})
	t.Run("the second line reseats", func(t *testing.T) {
		t.Parallel()
		c := newTwoClaimants(t)
		bin := c.deliveredToA()

		c.whilePumping("the changeover of B", func() error {
			_, err := c.b.edge.Engine.StartProcessChangeover(c.b.process, c.b.styleB, "scenario", "")
			return err
		})
		c.whilePumping("the cancel at B", func() error { return c.b.edge.Engine.CancelProcessChangeover(c.b.process) })
		c.eventually("B bound by the reseat", func() bool { b, _ := c.seat(c.b); return b != 0 })
		c.wantBooks("B reseated", seatBooks{bin, 40, bin, 40, bin, 40})

		c.strokes(c.a, 5)
		c.strokes(c.b, 5)
		c.wantBooks("five parts, counted by both", seatBooks{bin, 30, bin, 35, bin, 35})
	})
}

// THE TWO-ORDER PATH (pewter §9, 5660 §9). The level keeper of each station
// reads its own orders only. Asked together at a bare seat, both ask; asked
// after the delivery to A, the unbound seat of B still reads 0 and asks for a
// swap of a full bin.
func TestScenario_TwoClaimants_TwoOrderPath(t *testing.T) {
	t.Parallel()
	t.Run("both ask at a bare seat", func(t *testing.T) {
		t.Parallel()
		c := newTwoClaimants(t)
		c.sweep(c.a)
		c.sweep(c.b)
		ofA, ofB := c.ordersOf(c.a), c.ordersOf(c.b)
		if len(ofA) != 1 || len(ofB) != 1 {
			t.Fatalf("orders for one seat: A %d, B %d, want 1 and 1: %s", len(ofA), len(ofB), c.orders())
		}
		t.Logf("both asked: %s", c.orders())
		c.sweep(c.a)
		c.sweep(c.b)
		if a, b := len(c.ordersOf(c.a)), len(c.ordersOf(c.b)); a != 1 || b != 1 {
			t.Fatalf("a second pass made more (A %d, B %d): each station dedups its own", a, b)
		}

		c.carry(ofA[0])
		c.eventually("A bound", func() bool { b, _ := c.seat(c.a); return b != 0 })
		first, _ := c.seatBin()
		c.wantBooks("delivered to A", seatBooks{first, tcPack, first, tcPack, 0, 0})
		parked, err := c.core.eng.DB().GetOrderByUUID(ofB[0].EdgeUUID)
		mustNil(t, err, "the order of B")
		if parked.Status != protocol.StatusQueued || parked.VendorOrderID != "" {
			t.Fatalf("the order of B at a full seat reads %s (vendor %q), want queued and not flown: %s",
				parked.Status, parked.VendorOrderID, c.orders())
		}
		t.Logf("parked: %s", c.orders())

		// The seat empties; nobody asks again, and the carrier of B comes.
		dest, err := c.core.eng.DB().GetNodeByName(tcDest)
		mustNil(t, err, "dest")
		mustNil(t, c.core.eng.DB().MoveBinClearingStaging(first, dest.ID, true), "the seat empties")
		c.carry(parked)
		c.eventually("B bound", func() bool { b, _ := c.seat(c.b); return b != 0 })
		second, _ := c.seatBin()
		if got, _ := c.seat(c.b); second == first || got != second {
			t.Fatalf("the seat holds bin %d and B bound %d, want a second carrier (the first was %d)", second, got, first)
		}
		t.Logf("the second carrier %d came for the one seat", second)
	})
	t.Run("the unbound station asks over a full bin", func(t *testing.T) {
		t.Parallel()
		c := newTwoClaimants(t)
		bin := c.deliveredToA()
		c.sweep(c.b)
		ofB := c.ordersOf(c.b)
		if len(ofB) != 1 || ofB[0].OrderType != protocol.OrderTypeComplex {
			t.Fatalf("the keeper of B over a full bin made %d orders, want one swap: %s", len(ofB), c.orders())
		}
		c.wantBooks("B asked", seatBooks{bin, tcPack, bin, tcPack, 0, 0})
		swap := ofB[0]
		if swap.VendorOrderID == "" || !c.holdsSeatBin(swap, bin) {
			t.Fatalf("the swap of B is flown %t and holds the bin of A %t, want both: %s",
				swap.VendorOrderID != "", c.holdsSeatBin(swap, bin), c.orders())
		}
		t.Logf("the swap of a full bin: %s", c.orders())

		c.finishSwap(c.b, swap)
		c.eventually("B bound by its swap", func() bool { b, _ := c.seat(c.b); return b != 0 })
		second, _ := c.seatBin()
		if second == bin || second == 0 {
			t.Fatalf("the seat holds bin %d after the swap, want a second carrier (the first was %d)", second, bin)
		}
		c.wantBooks("the swap of B finished", seatBooks{second, tcPack, bin, tcPack, second, tcPack})

		// A was told nothing of the lift (the pickup goes to the station of the
		// order): it goes on counting the carrier that left, and its tile and its
		// level follow that carrier. Core drops every window of it as stale: the
		// epoch refresh names the node the carrier went to, which A does not have.
		goneCount := func() int {
			gone, err := c.core.eng.DB().GetBin(bin)
			mustNil(t, err, "the carrier that left")
			return gone.UOPRemaining
		}
		c.strokes(c.a, 5)
		c.strokes(c.b, 5)
		c.wantBooks("five parts after the swap", seatBooks{second, 35, bin, 35, second, 35})
		if got := goneCount(); got != tcPack {
			t.Fatalf("the carrier that left reads %d after the stale window, want %d", got, tcPack)
		}
		c.strokes(c.a, 5)
		c.wantBooks("five more at A", seatBooks{second, 35, bin, 30, second, 35})
		if got := goneCount(); got != tcPack {
			t.Fatalf("the carrier that left reads %d after the second stale window, want %d", got, tcPack)
		}
	})
	t.Run("both ask at a low bin", func(t *testing.T) {
		t.Parallel()
		c := newTwoClaimants(t)
		bin := c.deliveredToA()
		c.strokes(c.a, tcPack-tcLevel)
		// The operator confirms the delivery, so nothing of A is in flight.
		c.confirmDelivered(c.a)
		c.sweep(c.a)
		c.sweep(c.b)
		ofA, ofB := c.ordersOf(c.a), c.ordersOf(c.b)
		if len(ofA) != 1 || len(ofB) != 1 {
			t.Fatalf("swaps for one seat: A %d, B %d, want 1 and 1: %s", len(ofA), len(ofB), c.orders())
		}
		t.Logf("both asked: %s", c.orders())
		if !c.holdsSeatBin(ofA[0], bin) || ofB[0].VendorOrderID != "" || ofB[0].Status != protocol.StatusQueued {
			t.Fatalf("want the swap of A holding bin %d and the swap of B queued: %s", bin, c.orders())
		}
		c.core.eng.RunFulfillmentScan()
		c.settle()
		again := c.ordersOf(c.b)
		if len(again) != 1 || again[0].Status != protocol.StatusQueued {
			t.Fatalf("the swap of B after a scan, want it still parked and never refused: %s", c.orders())
		}

		// The swap of A finishes, and the parked one lifts the full bin it brought.
		c.finishSwap(c.a, ofA[0])
		c.eventually("A bound by its swap", func() bool { b, _ := c.seat(c.a); return b != 0 && b != bin })
		second, _ := c.seatBin()
		c.confirmDelivered(c.a)
		c.eventually("the swap of B flown", func() bool {
			o, err := c.core.eng.DB().GetOrderByUUID(ofB[0].EdgeUUID)
			mustNil(t, err, "the swap of B")
			return o.VendorOrderID != ""
		})
		if !c.holdsSeatBin(ofB[0], second) {
			t.Fatalf("the swap of B does not hold the full bin %d: %s", second, c.orders())
		}
		t.Logf("then a second robot for the full bin %d: %s", second, c.orders())
	})
}

// finishSwap has the operator of e release swap at the seat and the robot
// finish it, the fleet reporting each block as the poller does: the lift at
// the seat bumps the generation and tells the station that asked.
func (c *twoClaimants) finishSwap(e *claimantEdge, swap *coreorders.Order) {
	c.t.Helper()
	done := 0
	blocks := func() {
		c.t.Helper()
		v := c.core.sim.GetOrder(swap.VendorOrderID)
		if v == nil {
			c.t.Fatalf("no vendor order for swap %d", swap.ID)
		}
		for ; done < len(v.Blocks); done++ {
			b := v.Blocks[done]
			c.core.sim.CompleteBlock(swap.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			c.settle()
		}
	}
	c.drive(swap, "RUNNING")
	blocks()
	c.drive(swap, "WAITING")
	var edgeID int64
	c.eventually("the swap staged at "+e.uid, func() bool {
		rows, err := e.edge.DB.ListActiveOrdersByProcessNode(e.node)
		mustNil(c.t, err, "orders at "+e.uid)
		for _, o := range rows {
			if o.UUID == swap.EdgeUUID && o.Status == protocol.StatusStaged {
				edgeID = o.ID
				return true
			}
		}
		return false
	})
	c.whilePumping("the release at "+e.uid, func() error {
		return e.edge.Engine.ReleaseOrderWithLineside(edgeID, edgeengine.ReleaseDisposition{CalledBy: "operator"})
	})
	c.drive(swap, "RUNNING")
	blocks()
	c.drive(swap, "FINISHED")
}

// confirmDelivered is the operator of e confirming what was delivered there.
func (c *twoClaimants) confirmDelivered(e *claimantEdge) {
	c.t.Helper()
	confirmed := false
	c.eventually("a delivery to confirm at "+e.uid, func() bool {
		rows, err := e.edge.DB.ListActiveOrdersByProcessNode(e.node)
		mustNil(c.t, err, "orders at "+e.uid)
		for _, o := range rows {
			if o.Status == protocol.StatusDelivered {
				mustNil(c.t, e.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
				confirmed = true
			}
		}
		return confirmed
	})
	c.settle()
}

// holdsSeatBin reports whether o holds the claim on bin.
func (c *twoClaimants) holdsSeatBin(o *coreorders.Order, bin int64) bool {
	c.t.Helper()
	b, err := c.core.eng.DB().GetBin(bin)
	mustNil(c.t, err, "bin")
	return b.ClaimedBy != nil && *b.ClaimedBy == o.ID
}

// drive moves the vendor order of o through states.
func (c *twoClaimants) drive(o *coreorders.Order, states ...string) {
	c.t.Helper()
	for _, state := range states {
		for i := 0; i < 20; i++ {
			if _, _, deferred := c.core.sim.DriveStateWithRobot(o.VendorOrderID, state, "TC-AMR"); !deferred {
				break
			}
			c.settle()
		}
		c.settle()
	}
}
