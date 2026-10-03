// A keep-staged cell recovering by itself after a cancelled changeover, end to
// end across the two modules.
//
// The cell runs style A with an A spare on its spot. A changeover to B (a
// different part, carrier and market) starts and is cancelled at one moment of
// the spot's handover: before A's spare leaves, after it leaves, while B's
// refill flies, after B's spare lands, while B's refill digs it out of a lane,
// with the changeover's own swap robots waiting at the cell, and, on a
// single-robot cell, with the line's bin parked on outbound staging by the
// changeover's leg. Then nobody touches anything: the fleet carries what it
// was given, Core's scanner passes, messages flow. After that an operator
// REQUEST for A must run a normal A swap (or fill a line the cancel left empty)
// to the end, the level keeper's next ask must run the fast swap from a
// standing A spare, and at the end there is one A spare on the spot or coming,
// nothing of B in flight, nothing waiting, no lane held and every B bin back in
// B's market.
//
// Core is the real engine behind its real HTTP router with the fleet simulator
// as its fleet (startKeepStagedCore); Edge is the real engine; the Bus carries
// the orders between them. Robots are driven through the simulator's vendor
// states, the operator through Edge's own release and confirm entry points.
//
//go:build docker

package scenarios

import (
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/debuglog"
	"shingo/protocol/router"

	coreconfig "shingocore/config"
	"shingocore/dispatch"
	coreengine "shingocore/engine"
	coremessaging "shingocore/messaging"
	"shingocore/service"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreplantclaims "shingocore/store/plantclaims"
	coreharness "shingocore/testharness"
	"shingocore/www"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/catalog"
	edgeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// ── the level keeper's ticker ────────────────────────────────────────────────
//
// The level keeper (sweepCellLevels) is unexported, reached only from the
// demand reconciler's loop on a 60s ticker taken from clock.Default(). The test
// installs a clock that is the real clock in every respect except that one
// ticker, which it hands back to the test to fire. The loop and the sweep it
// runs are the production ones.

type kickTicker struct{ c chan time.Time }

func (k *kickTicker) C() <-chan time.Time { return k.c }
func (k *kickTicker) Stop()               {}
func (k *kickTicker) Reset(time.Duration) {}

type kickClock struct {
	mu      sync.Mutex
	tickers []*kickTicker
}

func (k *kickClock) Now() time.Time                         { return time.Now() }
func (k *kickClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (k *kickClock) NewTicker(d time.Duration) clock.Ticker {
	// Edge's loop only: Core has a runDemandReconciler of its own.
	if calledFrom("shingoedge/engine.(*Engine).runDemandReconciler") {
		kt := &kickTicker{c: make(chan time.Time)}
		k.mu.Lock()
		k.tickers = append(k.tickers, kt)
		k.mu.Unlock()
		return kt
	}
	return clock.Real().NewTicker(d)
}

func (k *kickClock) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.tickers)
}

func (k *kickClock) ticker(i int) *kickTicker {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tickers[i]
}

func calledFrom(fn string) bool {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function == fn {
			return true
		}
		if !more {
			return false
		}
	}
}

// ── the cell ────────────────────────────────────────────────────────────────

const (
	ksrLine   = "KSR-LINE"
	ksrSpot   = "KSR-SPOT"
	ksrDest   = "KSR-DEST"
	ksrOutStg = "KSR-OUTSTG"
	ksrMktA   = "KSR-MKT-A"
	ksrMktB   = "KSR-MKT-B"
	ksrPartA  = "KSR-PA"
	ksrPartB  = "KSR-PB"
	ksrPartC  = "KSR-PC" // the blocker in B's lane (case e)
)

// ksrCell is one keep-staged cell, Core and Edge, on style A with an A spare
// on the spot. Each style has its own part, its own carrier type and its own
// market, so "a B bin" is a bin of B's carrier type and "B's source" is B's
// market.
type ksrCell struct {
	t         *testing.T
	core      kscCore
	edge      *edgeharness.Edge
	bus       *harness.Bus
	kicks     *kickClock
	kick      *kickTicker
	role      protocol.ClaimRole
	mode      protocol.SwapMode
	processID int64
	nodeID    int64
	styleA    int64
	styleB    int64
	claimA    int64

	typeA, typeB *corebins.BinType
	mktA, mktB   *corenodes.Node
	laneB        *corenodes.Node // case e only
	robot        int
	robots       map[string]string // vendor order -> robot

	// case e only: the fleet reports its robots' decks, so Core's carried-bin
	// watch can read the jack of the robot carrying the blocker.
	deck *deckFleet
	dig  ksrDig
	// heldBlocker: case e-held only. No claim names the blocker's part, so the
	// carried-bin return holds it on its robot, and the end checks expect it
	// there.
	heldBlocker bool
}

// ksrDig is what case e's moment saw at the cancel: the dig leg carrying the
// blocker, its robot, its parent and every child.
type ksrDig struct {
	child    *coreorders.Order
	robot    string
	parentID int64
	blocker  int64
	kids     []*coreorders.Order
}

// startKsrCoreOnDeck is startKeepStagedCore with a fleet that lists robots and
// their decks (deckFleet): the real engine, router and simulator, and a robot
// report the scenario sets.
func startKsrCoreOnDeck(t *testing.T, fl *deckFleet) kscCore {
	t.Helper()
	db := coreharness.OpenDB(t)
	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "core"
	eng := coreengine.New(coreengine.Config{AppConfig: cfg, DB: db, Fleet: fl, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)
	dbg, err := debuglog.New(64, nil)
	mustNil(t, err, "debuglog")
	r, stop, err := www.NewRouter(eng, dbg)
	mustNil(t, err, "core router")
	t.Cleanup(stop)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return kscCore{eng: eng, sim: fl.SimulatorBackend, url: srv.URL}
}

type ksrOpts struct {
	role protocol.ClaimRole
	mode protocol.SwapMode
	// buriedB stocks B's market as one lane: the only B bin behind a
	// blocker, so B's refill is a dig.
	buriedB bool
	// claimC: some line's consume claim sources the blocker's part (C) from
	// B's market, so a carried blocker has a declared place to go back to.
	claimC bool
}

func newKsrCell(t *testing.T, o ksrOpts) *ksrCell {
	t.Helper()
	kicks := &kickClock{}
	clock.SetDefault(kicks)
	t.Cleanup(func() { clock.SetDefault(clock.Real()) })

	c := &ksrCell{t: t, kicks: kicks, role: o.role, mode: o.mode, robots: map[string]string{},
		heldBlocker: o.buriedB && !o.claimC}
	if o.buriedB {
		c.deck = newDeckFleet()
		c.core = startKsrCoreOnDeck(t, c.deck)
	} else {
		c.core = startKeepStagedCore(t)
	}
	coreDB := c.core.eng.DB()
	produce := o.role == protocol.ClaimRoleProduce

	stor, err := coreDB.GetNodeTypeByCode("STOR")
	if err != nil {
		stor = &corenodes.NodeType{Code: "STOR", Name: "Storage Slot"}
		mustNil(t, coreDB.CreateNodeType(stor), "STOR type")
	}
	ngrp, err := coreDB.GetNodeTypeByCode("NGRP")
	mustNil(t, err, "NGRP type")
	node := func(n *corenodes.Node) *corenodes.Node {
		n.Enabled = true
		mustNil(t, coreDB.CreateNode(n), "node "+n.Name)
		return n
	}
	line := node(&corenodes.Node{Name: ksrLine})
	spot := node(&corenodes.Node{Name: ksrSpot})
	node(&corenodes.Node{Name: ksrOutStg})
	// The outbound destination is a group of slots: each swap's evac leaves a
	// bin there.
	dest := node(&corenodes.Node{Name: ksrDest, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	c.mktA = node(&corenodes.Node{Name: ksrMktA, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	c.mktB = node(&corenodes.Node{Name: ksrMktB, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	slotsOf := func(mkt *corenodes.Node, n int) []*corenodes.Node {
		var out []*corenodes.Node
		for i := 1; i <= n; i++ {
			out = append(out, node(&corenodes.Node{
				Name: fmt.Sprintf("%s-%d", mkt.Name, i), ParentID: &mkt.ID, NodeTypeID: &stor.ID,
			}))
		}
		return out
	}
	slotsA := slotsOf(c.mktA, 8)
	slotsOf(dest, 6)

	c.typeA = &corebins.BinType{Code: "KSR-TA"}
	c.typeB = &corebins.BinType{Code: "KSR-TB"}
	mustNil(t, coreDB.CreateBinType(c.typeA), "bin type A")
	mustNil(t, coreDB.CreateBinType(c.typeB), "bin type B")
	for _, p := range []struct {
		code string
		bt   *corebins.BinType
	}{{ksrPartA, c.typeA}, {ksrPartB, c.typeB}, {ksrPartC, c.typeB}} {
		pl := &corepayloads.Payload{Code: p.code, UOPCapacity: 40}
		mustNil(t, coreDB.CreatePayload(pl), "payload "+p.code)
		mustNil(t, coreDB.SetPayloadBinTypes(pl.ID, []int64{p.bt.ID}), "carrier rule "+p.code)
	}
	// stock is a bin a market or the spot holds: full of the style's part for
	// a consume cell, an empty of the style's carrier for a produce cell.
	stock := func(label, part string, bt *corebins.BinType, at *corenodes.Node) *corebins.Bin {
		b := &corebins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &at.ID, Status: "available"}
		mustNil(t, coreDB.CreateBin(b), "bin "+label)
		if !produce || part == ksrPartC {
			mustNil(t, coreDB.SetBinManifest(b.ID, `{"items":[]}`, part, 40), "manifest "+label)
			mustNil(t, coreDB.ConfirmBinManifest(b.ID, ""), "confirm "+label)
		}
		return b
	}
	// The line holds A's bin: being drawn down on a consume cell, being filled
	// on a produce cell. Either way it carries A's part.
	lb := &corebins.Bin{BinTypeID: c.typeA.ID, Label: "KSR-LINE-BIN", NodeID: &line.ID, Status: "available"}
	mustNil(t, coreDB.CreateBin(lb), "line bin")
	mustNil(t, coreDB.SetBinManifest(lb.ID, `{"items":[]}`, ksrPartA, 40), "line manifest")
	mustNil(t, coreDB.ConfirmBinManifest(lb.ID, ""), "line confirm")

	stock("KSR-A-SPARE", ksrPartA, c.typeA, spot)
	for i := 0; i < 4; i++ {
		stock(fmt.Sprintf("KSR-A%d", i+1), ksrPartA, c.typeA, slotsA[i])
	}
	if o.buriedB {
		// B's market is one lane two deep: the blocker (another part, B's
		// carrier) at the mouth, the only B bin behind it, and a free slot
		// outside the lane for the dig to park the blocker in.
		lane, err := coreDB.GetNodeTypeByCode("LANE")
		mustNil(t, err, "LANE type")
		c.laneB = node(&corenodes.Node{Name: ksrMktB + "-L1", NodeTypeID: &lane.ID, ParentID: &c.mktB.ID, IsSynthetic: true})
		var deep []*corenodes.Node
		for d := 1; d <= 2; d++ {
			depth := d
			deep = append(deep, node(&corenodes.Node{
				Name: fmt.Sprintf("%s-L1-S%d", ksrMktB, d), ParentID: &c.laneB.ID, Depth: &depth,
			}))
		}
		node(&corenodes.Node{Name: ksrMktB + "-SHUF", ParentID: &c.mktB.ID})
		stock("KSR-C-BLOCKER", ksrPartC, c.typeB, deep[0])
		stock("KSR-B1", ksrPartB, c.typeB, deep[1])
	} else {
		slotsB := slotsOf(c.mktB, 6)
		for i := 0; i < 3; i++ {
			stock(fmt.Sprintf("KSR-B%d", i+1), ksrPartB, c.typeB, slotsB[i])
		}
	}

	// ── Edge ──
	c.edge = edgeharness.NewEdgeWithCoreAPI(t, "edge.test", c.core.url)
	edge := c.edge
	// The level keeper's loop has started on its own goroutine: wait for its
	// ticker.
	for i := 0; kicks.count() == 0; i++ {
		if i > 200 {
			t.Fatal("the demand reconciler never took its ticker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.kick = kicks.ticker(kicks.count() - 1)
	// The catalog Core would sync: the produce level is the part's capacity.
	for i, code := range []string{ksrPartA, ksrPartB} {
		mustNil(t, edge.DB.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: int64(i + 1), Name: code, Code: code, UOPCapacity: 40}),
			"catalog "+code)
	}
	c.processID, err = edge.DB.CreateProcess("KSR-PROC", "keep-staged recovery", "active_production", "", "", false)
	mustNil(t, err, "process")
	c.nodeID, err = edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: c.processID, CoreNodeName: ksrLine, Code: "KSR1", Name: ksrLine, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "process node")
	_, err = edge.DB.EnsureProcessNodeRuntime(c.nodeID)
	mustNil(t, err, "runtime")
	c.styleA, err = edge.DB.CreateStyle("KSR-A", "", c.processID)
	mustNil(t, err, "style A")
	c.styleB, err = edge.DB.CreateStyle("KSR-B", "", c.processID)
	mustNil(t, err, "style B")
	mustNil(t, edge.DB.SetActiveStyle(c.processID, &c.styleA), "active style")
	for _, s := range []struct {
		style      int64
		part, mkt  string
		claimIDOut *int64
	}{{c.styleA, ksrPartA, ksrMktA, &c.claimA}, {c.styleB, ksrPartB, ksrMktB, nil}} {
		id, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: s.style, CoreNodeName: ksrLine, Role: o.role, SwapMode: o.mode,
			PayloadCode: s.part, UOPCapacity: 40, ReorderPoint: 10,
			InboundSource: s.mkt, InboundStaging: ksrSpot, OutboundStaging: ksrOutStg,
			OutboundDestination: ksrDest, KeepStaged: domain.Ptr(true), AutoReorder: domain.Ptr(true),
		})
		mustNil(t, err, "claim "+s.part)
		if s.claimIDOut != nil {
			*s.claimIDOut = id
		}
	}
	// Mid-run, nowhere near the level: the keeper asks for nothing until a
	// case sets the level breached.
	mustNil(t, edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, c.unbreached()), "runtime claim")

	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", c.core.eng.Dispatcher())
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeOrderRequest, coreHandler.HandleOrderRequest)
	router.Register(coreRouter, protocol.TypeComplexOrderRequest, coreHandler.HandleComplexOrderRequest)
	router.Register(coreRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(coreRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	router.Register(coreRouter, protocol.TypeOrderRelease, coreHandler.HandleOrderRelease)
	if o.buriedB {
		// The Edge's claims reach Core's mirror as in the plant: the publisher's
		// report over the wire. The cancel-return policy reads that mirror.
		dataSvc := coremessaging.NewCoreDataService(coreDB, coreHandler, service.EpochAnnounce{
			Topic: "shingo.dispatch", CoreStation: "core",
		})
		subjects := router.NewSubject()
		router.RegisterSubject(subjects, protocol.SubjectPlantClaims, dataSvc.HandlePlantClaims)
		router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
			subjects.Dispatch(env, p)
		})
	}
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }
	c.bus = harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)
	if o.buriedB {
		mustNil(t, edgemessaging.NewPlantClaimsPublisher(edge.DB, "edge.test", 0).PublishChanged(c.processID),
			"publish plant claims")
	}
	c.settle()
	if o.claimC {
		// Another station's line draws C from B's market. Its claim reaches
		// Core's mirror the way every claim does, by process; this cell's
		// publisher owns KSR-PROC only, so the two do not overwrite each other.
		mustNil(t, coreDB.ReplacePlantClaims("KSR-PROC-C",
			[]coreplantclaims.StyleRow{{ProcessID: "KSR-PROC-C", StyleID: "KSR-STY-C", ConfigGen: 1, IsActive: true}},
			[]coreplantclaims.ClaimRow{{ProcessID: "KSR-PROC-C", StyleID: "KSR-STY-C", CoreNodeName: "KSR-LINE-C",
				Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: ksrPartC,
				InboundSource: ksrMktB}}, 0), "a claim that sources C from B's market")
	}
	if o.buriedB {
		for _, part := range []string{ksrPartA, ksrPartB, ksrPartC} {
			srcs, err := coreDB.ReturnSourcesForPayload(part, nil)
			mustNil(t, err, "return sources for "+part)
			t.Logf("case e: Core's claim mirror sources %s from %v", part, srcs)
		}
		srcs, err := coreDB.ReturnSourcesForPayload(ksrPartA, nil)
		mustNil(t, err, "return sources for "+ksrPartA)
		if len(srcs) == 0 && o.role == protocol.ClaimRoleConsume {
			t.Fatalf("Core's claim mirror names no source for %s after the publish", ksrPartA)
		}
	}
	return c
}

// unbreached and breached are the line's remaining count away from and at the
// claim's level (reorder point 10 for consume; capacity 40 for produce).
func (c *ksrCell) unbreached() int {
	if c.role == protocol.ClaimRoleProduce {
		return 5
	}
	return 30
}

func (c *ksrCell) breached() int {
	if c.role == protocol.ClaimRoleProduce {
		return 40
	}
	return 5
}

// settle pumps the bus until both outboxes stay empty.
func (c *ksrCell) settle() {
	for i := 0; i < 40; i++ {
		if c.bus.PumpAll() == 0 {
			time.Sleep(50 * time.Millisecond)
			if c.bus.PumpAll() == 0 {
				return
			}
		}
	}
}

// tick is the world moving with no person in it: messages flow and Core's
// fulfillment scanner passes once.
func (c *ksrCell) tick() {
	c.settle()
	c.core.eng.RunFulfillmentScan()
	c.settle()
}

func (c *ksrCell) eventually(what string, ok func() bool) {
	c.t.Helper()
	for i := 0; i < 60; i++ {
		c.tick()
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.dump("timed out: " + what)
	c.t.Fatalf("timed out waiting for: %s", what)
}

func (c *ksrCell) coreOf(o domain.Order) *coreorders.Order {
	c.t.Helper()
	co, err := c.core.eng.DB().GetOrderByUUID(o.UUID)
	mustNil(c.t, err, "core order for "+o.UUID)
	return co
}

func (c *ksrCell) edgeRow(id int64) *domain.Order {
	c.t.Helper()
	e, err := c.edge.DB.GetOrder(id)
	mustNil(c.t, err, "edge order")
	return e
}

// rows is every Edge order of the process, newest last.
func (c *ksrCell) rows() []domain.Order {
	c.t.Helper()
	all, err := c.edge.DB.ListOrdersByProcess(c.processID)
	mustNil(c.t, err, "edge rows")
	return all
}

func (c *ksrCell) live() []domain.Order {
	var out []domain.Order
	for _, o := range c.rows() {
		if !protocol.IsTerminal(o.Status) {
			out = append(out, o)
		}
	}
	return out
}

func (c *ksrCell) newRows(after int64) []domain.Order {
	var out []domain.Order
	for _, o := range c.rows() {
		if o.ID > after {
			out = append(out, o)
		}
	}
	return out
}

func (c *ksrCell) lastID() int64 {
	var top int64
	for _, o := range c.rows() {
		top = max(top, o.ID)
	}
	return top
}

// dump logs what the station and Core show for every order of the process.
func (c *ksrCell) dump(label string) {
	c.t.Helper()
	c.t.Logf("── %s ──", label)
	for _, o := range c.rows() {
		co, err := c.core.eng.DB().GetOrderByUUID(o.UUID)
		core := "no Core row"
		if err == nil {
			bin := int64(0)
			if co.BinID != nil {
				bin = *co.BinID
			}
			core = fmt.Sprintf("status=%s cause=%q reason=%q vendor=%q bin=%d", co.Status, co.QueueCause,
				co.QueueReason, co.VendorOrderID, bin)
		}
		c.t.Logf("  edge %d %s %s->%s %s | Edge: status=%s reason=%q code=%q | Core: %s",
			o.ID, o.OrderType, o.SourceNode, o.DeliveryNode, o.PayloadCode, o.Status, o.QueueReason, o.QueueCode, core)
		if err == nil {
			res, rerr := c.core.eng.DB().ListReservationsByOrder(co.ID)
			if rerr != nil {
				c.t.Logf("    Core order %d: reservations unreadable: %v", co.ID, rerr)
			}
			for _, r := range res {
				c.t.Logf("    Core order %d holds a %v reservation (%v) on bin %d / node %s", co.ID, r.Kind, r.State,
					r.BinID, c.nodeName(&r.NodeID))
			}
		}
	}
	bins, berr := c.core.eng.DB().ListBins()
	if berr != nil {
		c.t.Logf("  bins unreadable: %v", berr)
	}
	for _, b := range bins {
		at := "nowhere"
		if b.NodeID != nil {
			if n, err := c.core.eng.DB().GetNode(*b.NodeID); err == nil {
				at = n.Name
			}
		}
		claimed := int64(0)
		if b.ClaimedBy != nil {
			claimed = *b.ClaimedBy
		}
		c.t.Logf("  bin %s at %s payload=%q status=%s claimed=%d", b.Label, at, b.PayloadCode, b.Status, claimed)
	}
}

// ── the fleet ───────────────────────────────────────────────────────────────

// robotFor is the robot on a vendor order: one per order, kept across the
// separate calls that drive it.
func (c *ksrCell) robotFor(vid string) string {
	if r, ok := c.robots[vid]; ok {
		return r
	}
	// A recovery order is pinned to the robot already carrying the bin.
	if c.deck != nil {
		if r := c.deck.pinnedTo(vid); r != "" {
			c.robots[vid] = r
			return r
		}
	}
	c.robot++
	r := fmt.Sprintf("KSR-AMR-%d", c.robot)
	c.robots[vid] = r
	return r
}

// drive moves one Core order's vendor order through states, its robot on it.
func (c *ksrCell) drive(o *coreorders.Order, states ...string) {
	c.t.Helper()
	if o.VendorOrderID == "" {
		c.t.Fatalf("order %d (%s) has no vendor order to drive", o.ID, o.Status)
	}
	robot := c.robotFor(o.VendorOrderID)
	for _, s := range states {
		for i := 0; ; i++ {
			_, _, deferred := c.core.sim.DriveStateWithRobot(o.VendorOrderID, s, robot)
			if !deferred {
				break
			}
			if i > 20 {
				c.t.Fatalf("vendor order %s: %s deferred for good", o.VendorOrderID, s)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	c.settle()
}

// liftAt is the fleet reporting the order's pickup block at node done, as the
// real poller does mid-order: Core frees the slot and puts the bin in transit.
// settle pumps the messages that follows.
func (c *ksrCell) liftAt(o *coreorders.Order, node string, settle bool) bool {
	c.t.Helper()
	v := c.core.sim.GetOrder(o.VendorOrderID)
	if v == nil {
		return false
	}
	for _, b := range v.Blocks {
		if b.Location == node {
			ok := c.core.sim.CompleteBlock(o.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			if settle {
				c.settle()
			}
			return ok
		}
	}
	return false
}

// vendorState is the simulator's state for a vendor order, "" when it has none.
func (c *ksrCell) vendorState(vid string) string {
	if vid == "" {
		return ""
	}
	if v := c.core.sim.GetOrder(vid); v != nil {
		return v.State
	}
	return ""
}

// fleetStep is the fleet carrying every plain order it holds one step on:
// CREATED to RUNNING, RUNNING to FINISHED, or, for a dig leg that dwells at
// the lane until Core gives it somewhere to go, RUNNING to WAITING and, once
// Core has sealed it, on to FINISHED. Swap legs (complex) are not the fleet's
// to finish: they wait for the operator. Returns how many orders moved.
func (c *ksrCell) fleetStep() int {
	moved := 0
	active, err := c.core.eng.DB().ListActiveOrders()
	mustNil(c.t, err, "core active orders")
	for _, o := range active {
		if o.OrderType == protocol.OrderTypeComplex || o.VendorOrderID == "" {
			continue
		}
		view := c.core.sim.GetOrder(o.VendorOrderID)
		if view == nil {
			continue
		}
		switch view.State {
		case "CREATED":
			c.drive(o, "RUNNING")
		case "RUNNING":
			if dispatch.IsGateStaged(o) && !view.Complete {
				c.drive(o, "WAITING")
			} else {
				c.drive(o, "FINISHED")
			}
		case "WAITING":
			if !view.Complete {
				continue
			}
			c.drive(o, "RUNNING", "FINISHED")
		default:
			continue
		}
		moved++
	}
	return moved
}

// quiet lets the world run with nobody touching anything: messages flow,
// Core's scanner passes, the fleet carries what it has, until nothing moves.
func (c *ksrCell) quiet() {
	c.t.Helper()
	still := 0
	for i := 0; i < 80 && still < 2; i++ {
		c.tick()
		if c.fleetStep() == 0 {
			still++
		} else {
			still = 0
		}
	}
}

// ── what stands where ───────────────────────────────────────────────────────

func (c *ksrCell) nodeName(id *int64) string {
	if id == nil {
		return ""
	}
	n, err := c.core.eng.DB().GetNode(*id)
	if err != nil {
		return ""
	}
	return n.Name
}

// spareOnSpot is the A spare standing on the spot, or nil: a bin of A's
// carrier, full of A on a consume cell, empty on a produce cell.
func (c *ksrCell) spareOnSpot() *corebins.Bin {
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if c.nodeName(b.NodeID) != ksrSpot || b.BinTypeID != c.typeA.ID {
			continue
		}
		if c.role == protocol.ClaimRoleConsume && b.PayloadCode != ksrPartA {
			continue
		}
		if c.role == protocol.ClaimRoleProduce && b.PayloadCode != "" {
			continue
		}
		return b
	}
	return nil
}

// inB reports whether a node is B's market or under it (a slot, a lane slot).
func (c *ksrCell) inB(id *int64) bool {
	for hops := 0; id != nil && hops < 4; hops++ {
		if *id == c.mktB.ID {
			return true
		}
		n, err := c.core.eng.DB().GetNode(*id)
		if err != nil {
			return false
		}
		id = n.ParentID
	}
	return false
}

// ksrIsRetrieve is either spelling of a retrieve: Core promotes a produce
// refill to retrieve_empty and Edge's row follows.
func ksrIsRetrieve(t protocol.OrderType) bool {
	return t == protocol.OrderTypeRetrieve || t == protocol.OrderTypeRetrieveEmpty
}

// ── the swap ────────────────────────────────────────────────────────────────

func (c *ksrCell) operatorRequest() {
	c.t.Helper()
	var err error
	if c.role == protocol.ClaimRoleProduce {
		_, err = c.edge.Engine.RequestProduceSwap(c.nodeID)
	} else {
		_, err = c.edge.Engine.RequestNodeMaterial(c.nodeID, 1)
	}
	if err != nil {
		c.dump("REQUEST refused")
		c.t.Fatalf("operator REQUEST for A refused: %v", err)
	}
}

// levelKeeper sets the line's count at its level and fires the demand
// reconciler's ticker once: one pass of the production level sweep. The pass
// runs on the reconciler's goroutine; this waits for the legs it asks for.
func (c *ksrCell) levelKeeper() {
	c.t.Helper()
	mustNil(c.t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, c.breached()), "count at the level")
	before := c.lastID()
	select {
	case c.kick.c <- time.Now():
	case <-time.After(5 * time.Second):
		c.t.Fatal("the demand reconciler did not take the tick")
	}
	// The produce request's Core reads have taken several seconds on a loaded
	// docker host; give the pass room.
	for i := 0; i < 600; i++ {
		for _, o := range c.newRows(before) {
			if o.OrderType == protocol.OrderTypeComplex {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	c.dump("the level keeper asked for nothing")
	if active, err := c.edge.DB.ListActiveOrdersByProcessNode(c.nodeID); err == nil {
		for _, o := range active {
			c.t.Logf("  the line's active row %d %s %s->%s %s", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode, o.Status)
		}
	}
	ok, why := c.edge.Engine.CanAcceptOrders(c.nodeID)
	rt, rterr := c.edge.DB.GetProcessNodeRuntime(c.nodeID)
	mustNil(c.t, rterr, "the line's runtime row")
	level, auto := 0, false
	if claims, err := c.edge.DB.ListStyleNodeClaims(c.styleA); err == nil && len(claims) > 0 {
		level, auto = claims[0].DemandLevel(), claims[0].AutoReorder
	}
	c.t.Fatalf("the level keeper, with the count at the level, asked for no swap "+
		"(node accepting=%v: %s; count now %d; claim level %d, auto_reorder %v)", ok, why, rt.RemainingUOPCached, level, auto)
}

// swap runs one swap the trigger asks for to the end: its legs go to the
// fleet, the robots reach their waits, the operator releases, the robots
// finish (the evac first, so the line is clear for the supply), the operator
// confirms. The fleet carries the plain orders it has meanwhile. Returns the
// legs and whether they went to the fleet on the first pass (the fast swap).
func (c *ksrCell) swap(label string, trigger func()) (legs []domain.Order, fast bool) {
	c.t.Helper()
	before := c.lastID()
	trigger()
	c.tick()
	for _, o := range c.newRows(before) {
		if o.OrderType == protocol.OrderTypeComplex {
			legs = append(legs, o)
		}
	}
	want := 2
	if c.mode == protocol.SwapModeSingleRobot {
		want = 1
	}
	if len(legs) != want {
		c.dump(label)
		c.t.Fatalf("%s: %d swap legs, want %d", label, len(legs), want)
	}
	// Evac (bound away from the line) first.
	if len(legs) == 2 && legs[0].DeliveryNode == ksrLine {
		legs[0], legs[1] = legs[1], legs[0]
	}
	fast = true
	for _, l := range legs {
		if c.coreOf(l).VendorOrderID == "" {
			fast = false
		}
	}
	c.eventually(label+": the swap's legs with the fleet", func() bool {
		c.fleetStep()
		for _, l := range legs {
			if c.coreOf(l).VendorOrderID == "" {
				return false
			}
		}
		return true
	})
	for _, l := range legs {
		c.drive(c.coreOf(l), "RUNNING", "WAITING")
	}
	c.eventually(label+": the legs staged at Edge", func() bool {
		for _, l := range legs {
			if c.edgeRow(l.ID).Status != protocol.StatusStaged {
				return false
			}
		}
		return true
	})
	disp := edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}
	if c.mode == protocol.SwapModeSingleRobot {
		mustNil(c.t, c.edge.Engine.ReleaseOrderWithLineside(legs[0].ID, disp), label+": release")
	} else {
		mustNil(c.t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, disp), label+": release")
	}
	c.settle()
	for _, l := range legs {
		c.drive(c.coreOf(l), "RUNNING", "FINISHED")
	}
	c.eventually(label+": the legs delivered and confirmed", func() bool {
		done := true
		for _, l := range legs {
			if c.edgeRow(l.ID).Status == protocol.StatusDelivered {
				mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(l.ID, 1), "confirm")
			}
			if !protocol.IsTerminal(c.edgeRow(l.ID).Status) || !protocol.IsTerminal(c.coreOf(l).Status) {
				done = false
			}
		}
		return done
	})
	for _, l := range legs {
		if s := c.coreOf(l).Status; s != protocol.StatusConfirmed {
			c.t.Errorf("%s: leg %d ended %s at Core, want confirmed", label, l.ID, s)
		}
	}
	return legs, fast
}

// fill runs the delivery the trigger asks for on an empty line to the end: one
// plain order to the line, carried by the fleet, confirmed by the operator.
func (c *ksrCell) fill(label string, trigger func()) {
	c.t.Helper()
	before := c.lastID()
	trigger()
	c.tick()
	var to []domain.Order
	for _, o := range c.newRows(before) {
		if o.OrderType == protocol.OrderTypeComplex {
			c.dump(label)
			c.t.Fatalf("%s: a swap leg %d on an empty line, want a delivery", label, o.ID)
		}
		if o.DeliveryNode == ksrLine {
			to = append(to, o)
		}
	}
	if len(to) != 1 {
		c.dump(label)
		c.t.Fatalf("%s: %d deliveries to the line, want 1", label, len(to))
	}
	d := to[0]
	c.eventually(label+": the delivery landed and confirmed", func() bool {
		c.fleetStep()
		if c.edgeRow(d.ID).Status == protocol.StatusDelivered {
			mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(d.ID, 1), "confirm")
		}
		return protocol.IsTerminal(c.edgeRow(d.ID).Status) && protocol.IsTerminal(c.coreOf(d).Status)
	})
	if s := c.coreOf(d).Status; s != protocol.StatusConfirmed {
		c.t.Errorf("%s: delivery %d ended %s at Core, want confirmed", label, d.ID, s)
	}
}

// ── the changeover and its moments ──────────────────────────────────────────

// ksrCO is what the changeover start made.
type ksrCO struct {
	legs, refills, returns []domain.Order
}

func (c *ksrCell) startChangeover() ksrCO {
	c.t.Helper()
	before := c.lastID()
	if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", ""); err != nil {
		c.t.Fatalf("changeover to B: %v", err)
	}
	c.tick()
	var co ksrCO
	for _, o := range c.newRows(before) {
		switch {
		case o.OrderType == protocol.OrderTypeComplex:
			co.legs = append(co.legs, o)
		case ksrIsRetrieve(o.OrderType) && o.DeliveryNode == ksrSpot:
			co.refills = append(co.refills, o)
		case o.OrderType == protocol.OrderTypeMove && o.SourceNode == ksrSpot:
			co.returns = append(co.returns, o)
		}
	}
	if len(co.returns) != 1 || len(co.refills) == 0 {
		c.dump("changeover start")
		c.t.Fatalf("NOT REACHED: the changeover made returns=%d refills=%d, want A's spare sent back and B refills",
			len(co.returns), len(co.refills))
	}
	return co
}

// notReached stops a case whose moment could not be set up.
func (c *ksrCell) notReached(what string) {
	c.t.Helper()
	c.dump("NOT REACHED")
	c.t.Fatalf("NOT REACHED: %s", what)
}

// spareLeaves runs A's spare's return to its end: the robot lifts it and sets
// it down in A's market. Messages flow; Core's scanner does not pass.
func (c *ksrCell) spareLeaves(co ksrCO) {
	c.t.Helper()
	ret := c.coreOf(co.returns[0])
	if ret.VendorOrderID == "" {
		c.notReached("A's spare's return never went to the fleet")
	}
	c.drive(ret, "RUNNING", "FINISHED")
	if c.spareOnSpot() != nil {
		c.notReached("A's spare is still on the spot after its return finished")
	}
}

// refillFlying waits for a B refill to go to the fleet and puts a robot on
// it: from here a cancel cannot withdraw it.
func (c *ksrCell) refillFlying(co ksrCO) *coreorders.Order {
	c.t.Helper()
	var flying *coreorders.Order
	c.eventually("a B refill with the fleet", func() bool {
		for _, r := range co.refills {
			if o := c.coreOf(r); o.VendorOrderID != "" {
				flying = o
				return true
			}
		}
		return false
	})
	c.drive(flying, "RUNNING")
	return flying
}

func (c *ksrCell) bOnSpot() bool {
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if c.nodeName(b.NodeID) == ksrSpot && b.BinTypeID == c.typeB.ID {
			return true
		}
	}
	return false
}

type ksrMoment struct {
	name string
	what string
	// at runs the changeover up to the moment of the cancel.
	at func(c *ksrCell, co ksrCO)
	// buriedB stocks B's market as a lane with B's only bin dug for.
	buriedB bool
	// claimC: a claim names the dig's blocker's part (see ksrOpts).
	claimC bool
	// singleRobot: the moment exists only in the single-robot changeover leg.
	singleRobot bool
	// lineEmpty: the cancel leaves the line without a bin, so the operator's
	// REQUEST is answered by a plain delivery to the line, not a swap. Consume
	// only: a produce line left empty has no request that fills it (every
	// produce request builds a swap, whose lift at the empty line holds).
	lineEmpty bool
}

var ksrMoments = []ksrMoment{
	{name: "a", what: "before A's spare has left the spot", at: func(c *ksrCell, co ksrCO) {
		if st := c.vendorState(c.coreOf(co.returns[0]).VendorOrderID); st != "" && st != "CREATED" {
			c.notReached("A's spare's return already has a robot: " + st)
		}
		if c.spareOnSpot() == nil {
			c.notReached("A's spare is not on the spot")
		}
	}},
	{name: "b", what: "after A's spare left, before B's refill is dispatched", at: func(c *ksrCell, co ksrCO) {
		// The spot freeing (a bin-moved event) kicks Core's fulfillment scanner
		// on its own goroutine, which dispatches the waiting refill. There is no
		// pause between the two to cancel in. So the moment is taken as the
		// station sees it: the robot lifts the spare (its pickup block completes
		// and Core moves the bin to transit) and the cancel follows at once,
		// before any message from Core reaches Edge, while Edge still holds
		// every B refill as not yet with the fleet.
		ret := c.coreOf(co.returns[0])
		if ret.VendorOrderID == "" {
			c.notReached("A's spare's return never went to the fleet")
		}
		c.drive(ret, "RUNNING")
		if !c.liftAt(ret, ksrSpot, false) || c.spareOnSpot() != nil {
			c.notReached("the return's robot lifted A's spare but Core still has it on the spot")
		}
		for _, r := range co.refills {
			if s := c.edgeRow(r.ID).Status; protocol.ChangeoverStartActionFor(s) != protocol.ChangeoverStartCancel {
				c.notReached(fmt.Sprintf("B refill %d reads %s at Edge: already with the fleet", r.ID, s))
			}
			if o := c.coreOf(r); o.VendorOrderID != "" {
				c.t.Logf("moment b: Core's scanner had already handed B refill %d to the fleet (%s, no robot yet)",
					r.ID, c.vendorState(o.VendorOrderID))
			}
		}
	}},
	{name: "c", what: "while B's refill is in flight", at: func(c *ksrCell, co ksrCO) {
		c.spareLeaves(co)
		r := c.refillFlying(co)
		c.settle()
		e, err := c.edge.DB.GetOrderByUUID(r.EdgeUUID)
		mustNil(c.t, err, "the flying refill at Edge")
		if e == nil || protocol.ChangeoverStartActionFor(e.Status) == protocol.ChangeoverStartCancel {
			c.notReached("the flying B refill still reads cancellable at Edge")
		}
	}},
	{name: "d", what: "after B's spare has landed on the spot", at: func(c *ksrCell, co ksrCO) {
		c.spareLeaves(co)
		r := c.refillFlying(co)
		c.drive(r, "FINISHED")
		if !c.bOnSpot() {
			c.notReached("B's refill finished but no B bin is on the spot")
		}
	}},
	{name: "e", what: "while B's refill is a dig", buriedB: true, claimC: true, at: ksrDigMoment},
	{name: "e-held", what: "while B's refill is a dig, its blocker's part named by no claim", buriedB: true,
		at: ksrDigMoment},
	{name: "f", what: "with the changeover's swap robots at their waits", at: func(c *ksrCell, co ksrCO) {
		c.spareLeaves(co)
		r := c.refillFlying(co)
		c.drive(r, "FINISHED")
		c.eventually("the changeover's legs with the fleet", func() bool {
			c.fleetStep()
			for _, l := range co.legs {
				if c.coreOf(l).VendorOrderID == "" {
					return false
				}
			}
			return true
		})
		for _, l := range co.legs {
			c.drive(c.coreOf(l), "RUNNING", "WAITING")
		}
		c.eventually("the changeover's legs staged at Edge", func() bool {
			for _, l := range co.legs {
				if c.edgeRow(l.ID).Status != protocol.StatusStaged {
					return false
				}
			}
			return true
		})
	}},
	{name: "g", what: "with the line's bin parked on outbound staging", singleRobot: true, lineEmpty: true,
		at: ksrParkedMoment},
}

// ksrParkedMoment runs the single-robot changeover leg past its park: the
// operator releases it, the robot lifts the line's bin and sets it down on
// outbound staging, and the cancel comes before it collects the B spare.
func ksrParkedMoment(c *ksrCell, co ksrCO) {
	c.spareLeaves(co)
	r := c.refillFlying(co)
	c.drive(r, "FINISHED")
	if len(co.legs) != 1 {
		c.notReached(fmt.Sprintf("the single-robot changeover made %d legs, want 1", len(co.legs)))
	}
	leg := co.legs[0]
	c.eventually("the changeover's leg with the fleet", func() bool {
		c.fleetStep()
		return c.coreOf(leg).VendorOrderID != ""
	})
	c.drive(c.coreOf(leg), "RUNNING", "WAITING")
	c.eventually("the changeover's leg staged at Edge", func() bool {
		return c.edgeRow(leg.ID).Status == protocol.StatusStaged
	})
	mustNil(c.t, c.edge.Engine.ReleaseOrderWithLineside(leg.ID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}),
		"release the changeover's leg")
	c.settle()
	o := c.coreOf(leg)
	c.drive(o, "RUNNING")
	if !c.blockDone(o, ksrLine, "JackLoad") || !c.blockDone(o, ksrOutStg, "JackUnload") {
		c.notReached("the changeover's leg has no lift at the line or set-down on outbound staging")
	}
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.Label == "KSR-LINE-BIN" && c.nodeName(b.NodeID) == ksrOutStg {
			return
		}
	}
	c.notReached("the line's bin is not on outbound staging after the leg set it down")
}

// blockDone is the fleet reporting the order's first block at node with the
// given task done, as the real poller does mid-order.
func (c *ksrCell) blockDone(o *coreorders.Order, node, task string) bool {
	c.t.Helper()
	v := c.core.sim.GetOrder(o.VendorOrderID)
	if v == nil {
		return false
	}
	for _, b := range v.Blocks {
		if b.Location == node && b.BinTask == task {
			ok := c.core.sim.CompleteBlock(o.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			c.settle()
			return ok
		}
	}
	return false
}

// ksrDigMoment runs the changeover until B's refill is a dig whose first leg
// has lifted the blocker and waits at the lane with it.
func ksrDigMoment(c *ksrCell, co ksrCO) {
	c.spareLeaves(co)
	var child *coreorders.Order
	c.eventually("B's refill digging, its first leg with the fleet", func() bool {
		for _, r := range co.refills {
			parent := c.coreOf(r)
			kids, err := c.core.eng.DB().ListChildOrders(parent.ID)
			mustNil(c.t, err, "children")
			for _, k := range kids {
				if k.VendorOrderID != "" && !protocol.IsTerminal(k.Status) {
					child = k
					return true
				}
			}
		}
		return false
	})
	// The dig's first leg lifts the blocker and dwells at the lane with it,
	// waiting for Core to give it somewhere to go.
	c.drive(child, "RUNNING")
	if !c.liftAt(child, child.SourceNode, true) {
		c.notReached("the dig's first leg has no pickup at " + child.SourceNode)
	}
	// The deck reads loaded from the lift on.
	robot := c.robotFor(child.VendorOrderID)
	c.deck.set(robot, true, true)
	c.eventually("Core's robot poll reads the dig robot's deck loaded", func() bool {
		r, ok := c.core.eng.GetCachedRobotStatus(robot)
		return ok && r.JackState == 1
	})
	c.drive(child, "WAITING")
	if !c.core.eng.Dispatcher().LaneLock().IsLocked(c.laneB.ID) {
		c.notReached("B's lane is not held by the dig")
	}
	child = c.mustOrder(child.ID)
	if child.ParentOrderID == nil {
		c.notReached("the dig's leg has no parent")
	}
	kids, err := c.core.eng.DB().ListChildOrders(*child.ParentOrderID)
	mustNil(c.t, err, "children at the cancel")
	c.dig = ksrDig{child: child, robot: robot, parentID: *child.ParentOrderID, kids: kids}
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.Label == "KSR-C-BLOCKER" {
			if child.BinID != nil && *child.BinID != b.ID {
				c.t.Logf("case e: the dig leg carries bin %d, not the blocker %d", *child.BinID, b.ID)
			}
			c.dig.blocker = b.ID
			c.t.Logf("case e: at the cancel the blocker %s (payload %q, type %s) is at %s; dig leg %d (%s, %s->%s) on %s, parent %d, %d children",
				b.Label, b.PayloadCode, b.BinTypeCode, c.nodeName(b.NodeID), child.ID, child.Status,
				child.SourceNode, child.DeliveryNode, robot, c.dig.parentID, len(kids))
		}
	}
}

// ── the end state ───────────────────────────────────────────────────────────

func (c *ksrCell) assertRecovered() {
	c.t.Helper()
	coreDB := c.core.eng.DB()

	// One A spare on the spot or coming.
	spares := 0
	if c.spareOnSpot() != nil {
		spares++
	}
	for _, o := range c.live() {
		if ksrIsRetrieve(o.OrderType) && o.DeliveryNode == ksrSpot && o.PayloadCode == ksrPartA {
			spares++
		}
	}
	if spares != 1 {
		c.t.Errorf("A spares on the spot or coming = %d, want 1", spares)
	}

	// No non-terminal order for B at this cell, at Edge or at Core.
	for _, o := range c.live() {
		if o.PayloadCode == ksrPartB {
			c.t.Errorf("Edge order %d (%s %s->%s) for B is still %s", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode, o.Status)
		}
	}
	active, err := coreDB.ListActiveOrders()
	mustNil(c.t, err, "core active")
	for _, o := range active {
		if o.PayloadCode == ksrPartB {
			c.t.Errorf("Core order %d (%s %s->%s) for B is still %s (cause %q: %s)", o.ID, o.OrderType,
				o.SourceNode, o.DeliveryNode, o.Status, o.QueueCause, o.QueueReason)
		}
	}

	// Nothing left waiting: no live order either side, no swap in the
	// line's slots, no bin claimed.
	for _, o := range c.live() {
		c.t.Errorf("Edge order %d (%s %s->%s %s) left %s: %q", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode,
			o.PayloadCode, o.Status, o.QueueReason)
	}
	for _, o := range active {
		// A carried bin's return that has set its bin down is done. No station
		// receipts it, so it waits for the delivered-order auto-confirm
		// (staging.auto_confirm_delivered, 5 minutes by default), holding nothing.
		if o.RecoversOrderID != nil && o.Status == protocol.StatusDelivered {
			continue
		}
		if o.PayloadCode != ksrPartB {
			c.t.Errorf("Core order %d (%s %s->%s %s) left %s (cause %q: %s)", o.ID, o.OrderType, o.SourceNode,
				o.DeliveryNode, o.PayloadCode, o.Status, o.QueueCause, o.QueueReason)
		}
	}
	rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodeID)
	mustNil(c.t, err, "runtime")
	// A slot may keep pointing at the last swap's finished leg; only a live
	// order there is a swap still in flight.
	for _, slot := range []*int64{rt.ActiveOrderID, rt.StagedOrderID} {
		if slot != nil {
			if o := c.edgeRow(*slot); !protocol.IsTerminal(o.Status) {
				c.t.Errorf("the line's runtime slot holds live order %d (%s, %s)", o.ID, o.OrderType, o.Status)
			}
		}
	}
	bins, err := coreDB.ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.ClaimedBy != nil {
			c.t.Errorf("bin %s at %s still claimed by order %d", b.Label, c.nodeName(b.NodeID), *b.ClaimedBy)
		}
	}

	// No lane held, and no reservation (bin, slot, mouth) left with an order
	// that has ended.
	if c.laneB != nil && c.core.eng.Dispatcher().LaneLock().IsLocked(c.laneB.ID) {
		c.t.Errorf("B's lane is still held")
	}
	for _, o := range c.rows() {
		co, err := coreDB.GetOrderByUUID(o.UUID)
		if err != nil || !protocol.IsTerminal(co.Status) {
			continue
		}
		res, err := coreDB.ListReservationsByOrder(co.ID)
		mustNil(c.t, err, "reservations")
		for _, r := range res {
			c.t.Errorf("%s Core order %d (%s) still holds a %v reservation (%v) on bin %d / node %q",
				co.Status, co.ID, co.OrderType, r.Kind, r.State, r.BinID, c.nodeName(&r.NodeID))
		}
	}

	// Every B bin back in B's source, and B's source holding nothing of A's.
	for _, b := range bins {
		if c.heldBlocker && b.ID == c.dig.blocker {
			continue // held on its robot: a person's (assertBlockerHeld)
		}
		if b.BinTypeID == c.typeB.ID && !c.inB(b.NodeID) {
			c.t.Errorf("B bin %s is at %s, not back in %s", b.Label, c.nodeName(b.NodeID), ksrMktB)
		}
		if b.BinTypeID == c.typeA.ID && c.inB(b.NodeID) {
			c.t.Errorf("A bin %s (payload %q) was sent to B's source: it is at %s", b.Label, b.PayloadCode,
				c.nodeName(b.NodeID))
		}
	}
}

// ── the table ───────────────────────────────────────────────────────────────

// TestScenario_KeepStagedRecovery_AfterCancelledChangeover cancels a
// changeover from A to B at each moment of the keep-staged spot's handover and
// then leaves the cell alone. It must come back by itself: an operator REQUEST
// for A runs a normal A swap to the end, the level keeper's next ask runs the
// fast swap from a standing A spare, and nothing of B is left anywhere but B's
// market.
func TestScenario_KeepStagedRecovery_AfterCancelledChangeover(t *testing.T) {
	type cell struct {
		role protocol.ClaimRole
		mode protocol.SwapMode
	}
	cells := []cell{
		{protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot},
		{protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot},
		{protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot},
		{protocol.ClaimRoleProduce, protocol.SwapModeSingleRobot},
	}
	for _, cl := range cells {
		for _, m := range ksrMoments {
			if m.singleRobot && cl.mode != protocol.SwapModeSingleRobot {
				continue
			}
			if m.lineEmpty && cl.role != protocol.ClaimRoleConsume {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s/%s", cl.role, cl.mode, m.name), func(t *testing.T) {
				started := time.Now()
				defer func() { t.Logf("case %s ran %s", m.name, time.Since(started).Round(time.Second)) }()
				c := newKsrCell(t, ksrOpts{role: cl.role, mode: cl.mode, buriedB: m.buriedB, claimC: m.claimC})
				co := c.startChangeover()
				m.at(c, co)
				c.dump("at the cancel: " + m.what)
				mustNil(t, c.edge.Engine.CancelProcessChangeover(c.processID), "cancel")
				if m.buriedB {
					c.caseEReturn()
				}

				// Nobody touches anything.
				c.quiet()
				c.dump("the cell left alone after the cancel")

				// 1. The operator asks for A: a normal swap, to the end; on a line
				// the cancel left empty, the delivery that fills it.
				if m.lineEmpty {
					c.fill("REQUEST", c.operatorRequest)
				} else {
					c.swap("REQUEST", c.operatorRequest)
				}
				c.quiet()
				// 2. The level keeper asks for A: the fast swap from a standing spare.
				spare := c.spareOnSpot()
				if spare == nil {
					c.dump("before the level keeper's swap")
					t.Fatalf("no A spare standing on the spot after the first A cycle")
				}
				_, fast := c.swap("level keeper", c.levelKeeper)
				if !fast {
					t.Errorf("the level keeper's swap did not go to the fleet at once: not the fast swap")
				}
				if b, err := c.core.eng.DB().GetBin(spare.ID); err != nil || c.nodeName(b.NodeID) != ksrLine {
					t.Errorf("the spare %s that stood on the spot is not at the line after the fast swap", spare.Label)
				}
				c.quiet()
				c.dump("the end")
				// 3. The end state.
				c.assertRecovered()
				if m.buriedB {
					c.assertNothingCarried("the end")
					c.assertNoTerminalReservations("the end")
				}
			})
		}
	}
}

// ── case e on the join: the carried blocker's return ────────────────────────

func (c *ksrCell) mustOrder(id int64) *coreorders.Order {
	c.t.Helper()
	o, err := c.core.eng.DB().GetOrder(id)
	mustNil(c.t, err, fmt.Sprintf("core order %d", id))
	return o
}

// recoveryRows is every cancel-return / carried-bin recovery_actions row for
// the bin, oldest first.
func (c *ksrCell) recoveryRows(binID int64) []string {
	c.t.Helper()
	rows, err := c.core.eng.DB().DB.Query(`SELECT action, detail, actor FROM recovery_actions
		WHERE target_type = 'bin' AND target_id = $1 ORDER BY id`, binID)
	mustNil(c.t, err, "recovery actions")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, d, actor string
		mustNil(c.t, rows.Scan(&a, &d, &actor), "scan recovery action")
		out = append(out, a+" | "+actor+" | "+d)
	}
	return out
}

// onDeckOrders is every on-deck (carried-bin) order the bin has had.
func (c *ksrCell) onDeckOrders(binID int64) []*coreorders.Order {
	c.t.Helper()
	ords, err := c.core.eng.DB().ListOrdersByBin(binID, 50)
	mustNil(c.t, err, "orders of the blocker")
	var out []*coreorders.Order
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck {
			out = append(out, o)
		}
	}
	return out
}

// inDeclaredGroup reports whether the node is, or sits under, a place some
// claim declares as the source for the bin's payload (an empty: a declared
// empties place for its type), and which.
func (c *ksrCell) inDeclaredGroup(b *corebins.Bin) (string, []string, bool) {
	c.t.Helper()
	db := c.core.eng.DB()
	var names []string
	var err error
	if b.PayloadCode != "" {
		names, err = db.ReturnSourcesForPayload(b.PayloadCode, nil)
	} else {
		names, err = db.EmptiesPlacesForBinType(b.BinTypeID, nil)
	}
	mustNil(c.t, err, "declared places")
	id := b.NodeID
	for hops := 0; id != nil && hops < 5; hops++ {
		n, err := db.GetNode(*id)
		if err != nil {
			break
		}
		for _, name := range names {
			if n.Name == name {
				return name, names, true
			}
		}
		id = n.ParentID
	}
	return "", names, false
}

// assertNothingCarried: no bin at _TRANSIT or on any robot's carrier node.
func (c *ksrCell) assertNothingCarried(when string) {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if c.heldBlocker && b.ID == c.dig.blocker {
			continue // held on its robot: a person's (assertBlockerHeld)
		}
		at := c.nodeName(b.NodeID)
		if at == "_TRANSIT" || strings.HasPrefix(at, corebins.CarrierNodePrefix) {
			c.t.Errorf("%s: bin %s (payload %q) is at %s", when, b.Label, b.PayloadCode, at)
		}
	}
}

// assertNoTerminalReservations: no reservation row of any kind (bin, slot,
// mouth, occupancy) owned by a terminal Core order, and none on B's lane.
func (c *ksrCell) assertNoTerminalReservations(when string) {
	c.t.Helper()
	rows, err := c.core.eng.DB().DB.Query(`SELECT r.order_id, o.status, r.resource_kind, r.state,
		COALESCE(r.node_id, 0) FROM reservations r JOIN orders o ON o.id = r.order_id`)
	mustNil(c.t, err, "reservations")
	defer rows.Close()
	for rows.Next() {
		var oid, node int64
		var st, kind, state string
		mustNil(c.t, rows.Scan(&oid, &st, &kind, &state, &node), "scan reservation")
		if protocol.IsTerminal(protocol.Status(st)) {
			c.t.Errorf("%s: %s order %d still holds a %s reservation (%s) on node %q", when, st, oid, kind, state,
				c.nodeName(&node))
		}
		if c.laneB != nil && node == c.laneB.ID {
			c.t.Errorf("%s: order %d (%s) holds a %s reservation (%s) on B's lane", when, oid, st, kind, state)
		}
	}
	if c.laneB != nil && c.core.eng.Dispatcher().LaneLock().IsLocked(c.laneB.ID) {
		c.t.Errorf("%s: B's lane is still held", when)
	}
}

// caseEReturn follows the cancel while B's refill is a dig: the cascade, the
// blocker parked on its robot, the carried-bin watch's verdict, and the return
// to the end.
func (c *ksrCell) caseEReturn() {
	c.t.Helper()
	d := c.dig
	if d.child == nil || d.blocker == 0 {
		c.notReached("case e saw no dig leg carrying the blocker")
	}
	db := c.core.eng.DB()

	// 1. The cascade cancelled the dig parent and every non-terminal child.
	c.eventually("the dig and its legs end", func() bool {
		return protocol.IsTerminal(c.mustOrder(d.parentID).Status) && protocol.IsTerminal(c.mustOrder(d.child.ID).Status)
	})
	parent := c.mustOrder(d.parentID)
	c.t.Logf("case e: dig parent %d is %s (cause %q: %s)", parent.ID, parent.Status, parent.QueueCause, parent.QueueReason)
	if parent.Status != protocol.StatusCancelled {
		c.t.Errorf("the dig parent %d ended %s, want cancelled", parent.ID, parent.Status)
	}
	for _, k := range d.kids {
		now := c.mustOrder(k.ID)
		c.t.Logf("case e: dig child %d (%s->%s) was %s at the cancel, now %s (cause %q: %s)", k.ID,
			k.SourceNode, k.DeliveryNode, k.Status, now.Status, now.QueueCause, now.QueueReason)
		if !protocol.IsTerminal(k.Status) && now.Status != protocol.StatusCancelled {
			c.t.Errorf("dig child %d was %s at the cancel and ended %s, want cancelled", k.ID, k.Status, now.Status)
		}
	}

	// 2. The blocker rides its robot (branch B), not _TRANSIT.
	want := corebins.CarrierNodePrefix + d.robot
	var at string
	c.eventually("the blocker off _TRANSIT", func() bool {
		b, err := db.GetBin(d.blocker)
		mustNil(c.t, err, "blocker")
		at = c.nodeName(b.NodeID)
		return at != "_TRANSIT"
	})
	if at != want {
		c.dump("STOP: blocker not on its robot")
		c.t.Fatalf("STOP: the blocker is at %q after the cancel, want %s: the watch never sees it", at, want)
	}
	c.t.Logf("case e: the blocker is parked on %s", at)

	// 3. The fleet stops the job; the deck stays loaded. The robot poll's watch
	// decides.
	c.deck.set(d.robot, false, true)
	var verdict []string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c.settle()
		verdict = nil
		for _, r := range c.recoveryRows(d.blocker) {
			if strings.HasPrefix(r, "carried_bin_return_") {
				verdict = append(verdict, r)
			}
		}
		if len(verdict) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, r := range c.recoveryRows(d.blocker) {
		c.t.Logf("case e: recovery_actions row for the blocker: %s", r)
	}
	if len(verdict) == 0 {
		c.dump("STOP: the watch made no decision")
		c.t.Fatalf("STOP: the carried-bin watch neither returned nor held the blocker in 20s")
	}
	if strings.HasPrefix(verdict[0], "carried_bin_return_held") {
		if !c.heldBlocker {
			c.dump("case e: the blocker held on the robot")
			c.t.Errorf("the carried-bin watch held the blocker instead of returning it: %s", verdict[0])
			return
		}
		c.assertBlockerHeld(d, parent, verdict[0])
		return
	}
	if c.heldBlocker {
		c.t.Errorf("no claim names the blocker's part, yet the watch did not hold it: %v", verdict)
		return
	}
	rets := c.onDeckOrders(d.blocker)
	if len(rets) != 1 || len(verdict) != 1 {
		c.t.Errorf("the watch ordered %d returns (%d verdict rows), want exactly one", len(rets), len(verdict))
	}
	if len(rets) == 0 {
		c.t.FailNow()
	}
	ret := rets[0]
	if ret.RecoversOrderID == nil || *ret.RecoversOrderID != d.child.ID {
		c.t.Errorf("return %d recovers %v, want the cancelled dig leg %d", ret.ID, ret.RecoversOrderID, d.child.ID)
	}
	c.eventually("the return with the fleet", func() bool {
		ret = c.mustOrder(ret.ID)
		return ret.VendorOrderID != ""
	})
	rec := int64(0)
	if ret.RecoversOrderID != nil {
		rec = *ret.RecoversOrderID
	}
	c.t.Logf("case e: return order %d recovers_order_id=%d %s->%s vendor=%s pinned to %q", ret.ID, rec,
		ret.SourceNode, ret.DeliveryNode, ret.VendorOrderID, c.deck.pinnedTo(ret.VendorOrderID))
	if p := c.deck.pinnedTo(ret.VendorOrderID); p != d.robot {
		c.t.Errorf("the return went to the fleet pinned to %q, want %s", p, d.robot)
	}

	// 4. The robot sets the blocker down.
	c.deck.set(d.robot, true, true)
	c.drive(ret, "RUNNING", "FINISHED")
	c.deck.set(d.robot, false, false)
	c.eventually("the return ends", func() bool {
		return protocol.IsTerminal(c.mustOrder(ret.ID).Status) || c.mustOrder(ret.ID).Status == protocol.StatusDelivered
	})
	ret = c.mustOrder(ret.ID)
	b, err := db.GetBin(d.blocker)
	mustNil(c.t, err, "blocker after its return")
	group, names, ok := c.inDeclaredGroup(b)
	c.t.Logf("case e: return %d ended %s; the blocker is at %s (declared places for it: %v)", ret.ID, ret.Status,
		c.nodeName(b.NodeID), names)
	if !ok {
		c.t.Errorf("STOP: the blocker landed at %s, in no group a claim declares for %q (declared: %v)",
			c.nodeName(b.NodeID), b.PayloadCode, names)
	} else {
		c.t.Logf("case e: the blocker is in %s, a declared source for its payload", group)
	}

	// 5. End state of the episode.
	c.tick()
	c.assertNothingCarried("after the return")
	c.assertNoTerminalReservations("after the return")
}

// assertBlockerHeld: with no claim naming the blocker's part there is nowhere
// declared to take it, so the carried-bin return holds it on its robot, and the
// station that placed the cancelled refill is told so on that refill's row. The
// dig leg is Core's own order; the Edge has no row for it.
func (c *ksrCell) assertBlockerHeld(d ksrDig, parent *coreorders.Order, verdict string) {
	c.t.Helper()
	c.t.Logf("case e-held: %s", verdict)
	b, err := c.core.eng.DB().GetBin(d.blocker)
	mustNil(c.t, err, "the held blocker")
	if at := c.nodeName(b.NodeID); at != corebins.CarrierNodePrefix+d.robot {
		c.t.Errorf("the held blocker is at %s, want on %s", at, corebins.CarrierNodePrefix+d.robot)
	}
	if rets := c.onDeckOrders(d.blocker); len(rets) != 0 {
		c.t.Errorf("a held blocker has %d return orders, want none", len(rets))
	}
	row, err := c.edge.DB.GetOrderByUUID(parent.EdgeUUID)
	if err != nil || row == nil {
		c.t.Fatalf("the dig's parent %d (%s) is not an order the station holds: %v", parent.ID, parent.EdgeUUID, err)
	}
	var line string
	c.eventually("the hold sentence on the cancelled refill's station row", func() bool {
		c.settle()
		line = c.edge.Engine.OrderService().BinReturnLines([]edgeorders.Order{*row})[row.ID]
		return line != ""
	})
	c.t.Logf("case e-held: station row %d (%s %s->%s, %s) reads %q", row.ID, row.OrderType, row.SourceNode,
		row.DeliveryNode, row.Status, line)
	if !strings.Contains(line, ksrPartC) {
		c.t.Errorf("the station row reads %q, want the hold naming %s", line, ksrPartC)
	}
	c.tick()
	c.assertNoTerminalReservations("after the hold")
}
