// The 4x2-shaped cell the catid_4x2 scenarios run, end to end across the two
// modules: WELD-6, a two-robot weld with five consume positions, and its line.
//
//	position  RUN (CATID 1001)  ALT (CATID 1002)
//	W6-P1     COMMON-1          COMMON-1
//	W6-P2     COMMON-2          COMMON-2
//	W6-P3     COMMON-3          COMMON-3
//	W6-P4     MIRROR-L          MIRROR-R
//	W6-P5     MIRROR-L          MIRROR-R
//
// The two styles share three parts and differ at the two mirror positions;
// both CATIDs are pinned by a person and the cell has run both before (the
// Edge boots with both observed, as a plant Edge does). The PLC is
// simwarlink.Line: a load takes one part from the cart at every position
// (the load station clicks), and the production counter ticks w6Depth loads
// later. The scenario turns the line one machine cycle at a time and runs one
// Edge poll pass per cycle (w6Clock hands the scenario the poll's ticker), so
// it knows every load and every count; the CATID monitor and its windows run
// on the process clock, which reads the wall here, as at a plant.
//
// Each scenario asserts three ledgers against the line: the carts (every
// cart's count is its stated count less what was drawn from it, and per
// position the draws, the seat's hold and the holds ledger add up to the parts
// counted at the position's counter; a cart is drawn for the strokes made
// under its own part, share), the holds, and the production record
// (counter_snapshots by CATID is what the PLC made; by style it agrees with
// hourly and with what Core received). Every order the run placed lands and is
// confirmed.
//
//go:build docker

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	corepayloads "shingocore/store/payloads"

	edgeconfig "shingoedge/config"
	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	"shingoedge/engine/releasefake"
	edgemessaging "shingoedge/messaging"
	"shingoedge/plc"
	"shingoedge/plc/simwarlink"
	edgeservice "shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
	edgeuop "shingoedge/uop"
)

const (
	w6PLC      = "WELD-6"
	w6Struct   = "MES_WELD_6" // the cell's MES struct, five CATID_0N / Prod_Counter_0N slots
	w6Counter  = "MES_WELD_6.Prod_Counter_01"
	w6Identity = "MES_WELD_6.CATID_01" // the Edge derives it from the counter tag
	w6Station  = "STA_W6_LOAD.Load_Count"
	w6Edge     = "edge.test" // ksrCell.registerEdge's station
	w6Market   = "W6-MKT"
	w6Dest     = "W6-DEST"
	// w6Depth is L, the loads between the load station and the counter.
	w6Depth = 8
	// w6Full is every cart's stated count, the parts' capacity.
	w6Full = 40
	catRUN = 1001
	catALT = 1002
)

var (
	w6Pos      = [5]string{"W6-P1", "W6-P2", "W6-P3", "W6-P4", "W6-P5"}
	w6RunParts = [5]string{"COMMON-1", "COMMON-2", "COMMON-3", "MIRROR-L", "MIRROR-L"}
	w6AltParts = [5]string{"COMMON-1", "COMMON-2", "COMMON-3", "MIRROR-R", "MIRROR-R"}
	w6AllParts = []string{"COMMON-1", "COMMON-2", "COMMON-3", "MIRROR-L", "MIRROR-R"}
)

// ── the clock ───────────────────────────────────────────────────────────────

// w6LoopClock is one loop's clock on one Edge the cell starts: the PLC poll's,
// the demand reconciler's or the parked-ticks monitor's, given to the Edge
// through its Config (PollClock, DemandReconcileClock, StrandedClock). It is
// the real clock but for the loop's ticker, which it hands the cell to fire
// (passTicker). Nothing here is process-wide, so the scenarios run beside the
// package's others; a restarted Edge is built with clocks of its own.
type w6LoopClock struct {
	mu sync.Mutex
	tk *passTicker
}

func (k *w6LoopClock) Now() time.Time                         { return time.Now() }
func (k *w6LoopClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTicker is the loop's: the first ticker asked for is the cell's to fire.
func (k *w6LoopClock) NewTicker(d time.Duration) clock.Ticker {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tk == nil {
		k.tk = &passTicker{}
		return k.tk
	}
	return clock.Real().NewTicker(d)
}

// ticker is the loop's ticker once the loop has taken it, else nil.
func (k *w6LoopClock) ticker() *passTicker {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tk
}

// w6Clocks are one Edge's three loop clocks.
type w6Clocks struct {
	poll, sweep, stranded w6LoopClock
}

// w6Tickers are one Edge's driven tickers: the poll's, the reconciler's and
// the parked-ticks monitor's (stranded).
type w6Tickers struct {
	poll, sweep, stranded *passTicker
}

// started waits for each of the Edge's three loops to take its ticker, and
// returns them.
func (k *w6Clocks) started(t *testing.T) *w6Tickers {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		c := &w6Tickers{poll: k.poll.ticker(), sweep: k.sweep.ticker(), stranded: k.stranded.ticker()}
		if c.poll != nil && c.sweep != nil && c.stranded != nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("the Edge's poll, reconciler or parked-ticks monitor never took its ticker")
		}
	}
}

// passTicker is a loop's ticker the scenario fires. Each time the loop waits
// on it (C, called as its select is entered) it gets a channel of its own, so
// a pass is known to be over when the loop asks for the next one.
type passTicker struct {
	mu  sync.Mutex
	cur chan time.Time
	gen int
}

func (p *passTicker) C() <-chan time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cur = make(chan time.Time)
	p.gen++
	return p.cur
}

func (p *passTicker) Stop()               {}
func (p *passTicker) Reset(time.Duration) {}

// pass fires the loop once and returns when that turn of it is over.
func (p *passTicker) pass(t *testing.T, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var ch chan time.Time
	var gen int
	for {
		p.mu.Lock()
		ch, gen = p.cur, p.gen
		p.mu.Unlock()
		if ch != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the loop never waited on its ticker", what)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case ch <- time.Now():
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: the loop did not take the tick", what)
	}
	for {
		p.mu.Lock()
		done := p.gen != gen
		p.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the pass did not end", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ── the PLC ─────────────────────────────────────────────────────────────────

// w6WarLink is WarLink in front of the cell's PLC, the line: the MES struct
// MES_WELD_6 (five CATID_0N / Prod_Counter_0N pairs; the line counts on slot 1
// until it moves its count) and the load station's counter, DINTs. WarLink
// publishes the three tags it is configured with (Prod_Counter_01, CATID_01,
// the station's) and each struct member the Edge enables; a direct read
// answers any member, and the struct's name reads the whole struct, as WarLink
// reads a UDT.
type w6WarLink struct {
	mu        sync.Mutex
	line      *simwarlink.Line
	published map[string]bool
	// counted is every count the line has made, by the identity the counter
	// read when it counted: what the PLC's own production record says.
	counted map[int64]int64
	// loadIDs and countIDs are the identity of each load the line has taken
	// (the one it was loaded under, LineState.ByIdentity) and of each count it
	// has made (the one the counter read), in order: load k is loadIDs[k].
	loadIDs, countIDs []int64
	// structReads are the times the struct was read whole: the Edge's slot
	// watch reads so the members WarLink does not publish.
	structReads []time.Time
}

var _ plc.WarlinkClient = (*w6WarLink)(nil)

func newW6WarLink() *w6WarLink {
	l := simwarlink.NewLine(w6Depth, 1)
	l.SetIdentity(catRUN)
	return &w6WarLink{line: l, published: map[string]bool{}, counted: map[int64]int64{}}
}

func (f *w6WarLink) state() simwarlink.LineState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.line.State()
}

// cycle is one machine cycle, its count booked to the identity it read and
// each load and count to the identity it was made under.
func (f *w6WarLink) cycle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := f.line.State()
	f.line.Tick()
	after := f.line.State()
	for id, n := range after.ByIdentity {
		for k := before.ByIdentity[id]; k < n; k++ {
			f.loadIDs = append(f.loadIDs, id)
		}
	}
	if n := (after.Loads - int64(after.InFlight)) - (before.Loads - int64(before.InFlight)); n > 0 {
		f.counted[after.Identity] += n
		for k := int64(0); k < n; k++ {
			f.countIDs = append(f.countIDs, after.Identity)
		}
	}
}

// strokes are the identities of the loads and the counts made so far.
func (f *w6WarLink) strokes() (loads, counts []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.loadIDs...), append([]int64(nil), f.countIDs...)
}

func (f *w6WarLink) do(fn func(l *simwarlink.Line)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.line)
}

func (f *w6WarLink) countedBy() map[int64]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]int64{}
	for k, v := range f.counted {
		out[k] = v
	}
	return out
}

// members is every tag the PLC has: the struct's ten members and the load
// station's counter. Caller holds f.mu.
func (f *w6WarLink) members() map[string]float64 {
	out := map[string]float64{w6Station: float64(f.line.State().Loads)}
	for i, s := range f.line.Slots() {
		out[w6Struct+"."+simwarlink.CATIDMember(i+1)] = float64(s.CATID)
		out[w6Struct+"."+simwarlink.CounterMember(i+1)] = float64(s.Counter)
	}
	return out
}

// values are the tags WarLink publishes: the three it is configured with and
// each struct member the Edge enabled.
func (f *w6WarLink) values() map[string]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]float64{}
	for tag, v := range f.members() {
		if tag == w6Counter || tag == w6Identity || tag == w6Station || f.published[tag] {
			out[tag] = v
		}
	}
	return out
}

// structReadAfter is the first read of the whole struct after t, zero when
// there has been none.
func (f *w6WarLink) structReadAfter(t time.Time) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.structReads {
		if r.After(t) {
			return r
		}
	}
	return time.Time{}
}

func (f *w6WarLink) ListPLCs(context.Context) ([]plc.WarlinkPLC, error) {
	return []plc.WarlinkPLC{{Name: w6PLC, Status: "Connected"}}, nil
}

func (f *w6WarLink) ListTags(_ context.Context, plcName string) (map[string]plc.WarlinkTag, error) {
	out := map[string]plc.WarlinkTag{}
	for tag, v := range f.values() {
		out[plcName+"."+tag] = plc.WarlinkTag{PLC: plcName, Name: tag, Type: "DINT", Value: v}
	}
	return out, nil
}

func (f *w6WarLink) ListAllTags(context.Context, string) ([]plc.WarlinkTagInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []plc.WarlinkTagInfo
	for _, tag := range []string{w6Counter, w6Station, w6Identity} {
		out = append(out, plc.WarlinkTagInfo{Name: tag, Type: "DINT", Configured: true, Enabled: f.published[tag]})
	}
	return out, nil
}

func (f *w6WarLink) SetTagPublishing(_ context.Context, _, tag string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[tag] = enabled
	return nil
}

func (f *w6WarLink) ReadTagValue(_ context.Context, _, tag string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.members()
	if tag == w6Struct {
		f.structReads = append(f.structReads, time.Now())
		out := map[string]any{}
		for t, v := range all {
			if m, ok := strings.CutPrefix(t, w6Struct+"."); ok {
				out[m] = v
			}
		}
		return out, nil
	}
	v, ok := all[tag]
	if !ok {
		return nil, fmt.Errorf("tag %s not found", tag)
	}
	return v, nil
}

func (f *w6WarLink) WriteTagValue(context.Context, string, string, any) error {
	return errors.New("the cell's tags are not writable")
}

func (f *w6WarLink) OpenEventStream(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// ── the Edge ────────────────────────────────────────────────────────────────

// w6Preflight is the changeover preflight with every part stocked, as
// testharness.NewEdge answers it.
type w6Preflight struct{}

func (w6Preflight) Available() bool { return true }
func (w6Preflight) PreflightInventory([]string) (*edgeservice.PreflightCoreResult, error) {
	return &edgeservice.PreflightCoreResult{Missing: []string{}}, nil
}

// startW6Edge boots an Edge on db with the cell's PLC behind WarLink, wired as
// testharness.NewEdge wires one (its handlers and ingestor), and returns it
// with its driven tickers. A restart is this again on the same database.
func startW6Edge(t *testing.T, db *store.DB, wl plc.WarlinkClient) (*edgeharness.Edge, *w6Tickers, *edgeuop.Mutator) {
	t.Helper()
	cfg := edgeconfig.Defaults()
	cfg.StationUID, cfg.Namespace, cfg.LineID = w6Edge, "test", "edge-test"
	cfg.Messaging.StationID = w6Edge
	// No WarLink poller: the scenario writes each read into the PLC cache
	// (w6Cell.publish) and the poll reads it there; WarLink's own verbs (the
	// tag lists, publishing, a direct read) answer from the line.
	cfg.WarLink.Enabled = false
	clocks := &w6Clocks{}
	eng := edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf, Warlink: wl,
		PollClock: &clocks.poll, DemandReconcileClock: &clocks.sweep, StrandedClock: &clocks.stranded})
	eng.SetReleasePointSource(releasefake.DB{DB: db})
	eng.SetPreflightSource(w6Preflight{})
	eng.Start()
	tickers := clocks.started(t)
	t.Cleanup(eng.Stop) // a second Stop, after a restart's, is a no-op
	// The inventory sink, as a plant runs it: a pickup clears the seat and a
	// draw reaches Core's bin through it (flushed at a pickup, and by the
	// scenario before it reads a count, in place of the periodic flush).
	sink := edgeuop.New(db, w6Edge, db, db)
	eng.SetInventoryDeltaSink(sink)

	h := edgemessaging.NewEdgeHandler(eng.OrderManager())
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, r *protocol.NodeListResponse) {
		eng.SetCoreNodes(r.Nodes)
	})
	router.RegisterSubject(subjects, protocol.SubjectBinPickedUp, func(_ *protocol.Envelope, bp *protocol.BinPickedUp) {
		eng.HandleBinPickedUpMessage(*bp)
	})
	router.RegisterSubject(subjects, protocol.SubjectBinLanded, func(_ *protocol.Envelope, bl *protocol.BinLanded) {
		eng.HandleBinLanded(*bl)
	})
	router.RegisterSubject(subjects, protocol.SubjectBinReturn, func(_ *protocol.Envelope, br *protocol.BinReturn) {
		eng.HandleBinReturn(*br)
	})
	router.RegisterSubject(subjects, protocol.SubjectEdgeAnswer, eng.HandleEdgeAnswer)
	router.RegisterSubject(subjects, protocol.SubjectSourcingState, func(_ *protocol.Envelope, r *protocol.SourcingStateReport) {
		eng.HandleSourcingState(*r)
	})
	ing := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == w6Edge || hdr.Dst.Station == protocol.StationBroadcast
	})
	r := router.New[string]()
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { subjects.Dispatch(env, p) })
	router.Register(r, protocol.TypeOrderRequest, func(*protocol.Envelope, *protocol.OrderRequest) {})
	router.Register(r, protocol.TypeOrderCancel, func(*protocol.Envelope, *protocol.OrderCancel) {})
	router.Register(r, protocol.TypeOrderReceipt, func(*protocol.Envelope, *protocol.OrderReceipt) {})
	router.Register(r, protocol.TypeOrderRedirect, func(*protocol.Envelope, *protocol.OrderRedirect) {})
	router.Register(r, protocol.TypeComplexOrderRequest, func(*protocol.Envelope, *protocol.ComplexOrderRequest) {})
	router.Register(r, protocol.TypeOrderRelease, func(*protocol.Envelope, *protocol.OrderRelease) {})
	router.Register(r, protocol.TypeOrderIngest, func(*protocol.Envelope, *protocol.OrderIngestRequest) {})
	router.Register(r, protocol.TypeOrderAck, h.HandleOrderAck)
	router.Register(r, protocol.TypeOrderWaybill, h.HandleOrderWaybill)
	router.Register(r, protocol.TypeOrderUpdate, h.HandleOrderUpdate)
	router.Register(r, protocol.TypeOrderDelivered, h.HandleOrderDelivered)
	router.Register(r, protocol.TypeOrderError, h.HandleOrderError)
	router.Register(r, protocol.TypeOrderCancelled, h.HandleOrderCancelled)
	router.Register(r, protocol.TypeOrderStaged, h.HandleOrderStaged)
	router.Register(r, protocol.TypeOrderSkipped, h.HandleOrderSkipped)
	ing.Dispatch = func(env *protocol.Envelope) { r.Dispatch(env, env.Type) }
	return &edgeharness.Edge{Engine: eng, Ingestor: ing, DB: db}, tickers, sink
}

// ── the cell ────────────────────────────────────────────────────────────────

// w6Opts is how a scenario's cell counts.
type w6Opts struct {
	// station lists the positions (0-4) counted at the load station; the
	// rest are counted at the cell's production counter.
	station []int
	// unbound makes the load station's counter with no position counted at
	// it, when station lists none: it measures the line and charges nothing.
	unbound bool
	// reorder is every claim's reorder point; 0 is 10. The demand sweep runs
	// only when a scenario fires it (sweep).
	reorder int
}

// w6Tenure is one cart's stay at a position: the station clicks and the
// counter's counts at its landing and its departure.
type w6Tenure struct {
	bin                 int64
	label               string
	part                string // the cart's part at Core when it landed
	pos                 int
	loadsIn, loadsOut   int64
	countsIn, countsOut int64
	open                bool
}

// w6Cell is the cell, Core and Edge. The embedded ksrCell carries the orders,
// the fleet and the bus as the keep-staged scenarios drive them.
type w6Cell struct {
	*ksrCell
	plc      *w6WarLink
	tickers  *w6Tickers
	sink     *edgeuop.Mutator
	push     *harness.BinsPush
	shipper  *edgemessaging.TickShipper
	coreIn   *protocol.Ingestor
	askKinds []string
	nodes    [5]int64
	run, alt int64
	counter  int64 // the load station's counter, 0 when the cell has none
	station  [5]bool
	unbound  bool // the load station's counter is made, bound to no position
	tenures  [5][]*w6Tenure
	reorder  int
	// lastPublish is when the scenario last wrote the PLC's tags into the
	// Edge's cache (publish).
	lastPublish time.Time
}

// newW6Cell builds the cell on RUN, each position holding a full RUN cart
// delivered by Core, the line empty, the Edge booted with both CATIDs seen.
func newW6Cell(t *testing.T, o w6Opts) *w6Cell {
	t.Helper()
	c := &w6Cell{ksrCell: &ksrCell{t: t, role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot,
		robots: map[string]string{}}, plc: newW6WarLink()}
	for _, p := range o.station {
		c.station[p] = true
	}
	c.unbound = o.unbound
	c.reorder = o.reorder
	if c.reorder == 0 {
		c.reorder = 10
	}
	c.core = startKeepStagedCore(t)
	// Core's heartbeat maintenance makes the part-event partitions a plant
	// Core keeps; this Core runs none, so the scenario makes today's.
	mustNil(t, c.core.eng.DB().EnsureHeartbeatPartitions(time.Now()), "part-event partitions")
	c.seedCore()

	db, err := store.Open(filepath.Join(t.TempDir(), "edge.db"))
	mustNil(t, err, "edge db")
	t.Cleanup(func() { _ = db.Close() })
	c.edge, c.tickers, c.sink = startW6Edge(t, db, c.plc)
	c.configureEdge()
	// The cell has run both styles: the Edge has seen both CATIDs, and boots
	// knowing it (the monitor reads the observations at its start).
	for _, v := range []int64{catALT, catRUN} {
		mustNil(t, db.RecordPLCCATIDObservation(c.processID, w6PLC, fmt.Sprint(v)), "observation")
	}
	c.restart("boot")

	c.shipper, err = edgemessaging.NewTickShipper(db, func(data []byte) error {
		c.coreIn.HandleRaw(data)
		return nil
	}, w6Edge)
	mustNil(t, err, "tick shipper")
	c.fill()
	// The first pass reads the counters where they stand: each seeds.
	c.cycle(0)
	return c
}

// seedCore makes the cell's nodes, carts and market at Core.
func (c *w6Cell) seedCore() {
	t, db := c.t, c.core.eng.DB()
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
	for _, p := range w6Pos {
		node(&corenodes.Node{Name: p})
		node(&corenodes.Node{Name: p + "-IN"})
		node(&corenodes.Node{Name: p + "-OUT"})
	}
	mkt := node(&corenodes.Node{Name: w6Market, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	dest := node(&corenodes.Node{Name: w6Dest, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	var slots []*corenodes.Node
	for i := 1; i <= 16; i++ {
		slots = append(slots, node(&corenodes.Node{Name: fmt.Sprintf("%s-%d", w6Market, i), ParentID: &mkt.ID, NodeTypeID: &stor.ID}))
	}
	for i := 1; i <= 10; i++ {
		node(&corenodes.Node{Name: fmt.Sprintf("%s-%d", w6Dest, i), ParentID: &dest.ID, NodeTypeID: &stor.ID})
	}
	cart := &corebins.BinType{Code: "W6-CART"}
	mustNil(t, db.CreateBinType(cart), "cart type")
	for _, part := range w6AllParts {
		pl := &corepayloads.Payload{Code: part, UOPCapacity: w6Full}
		mustNil(t, db.CreatePayload(pl), "payload "+part)
		mustNil(t, db.SetPayloadBinTypes(pl.ID, []int64{cart.ID}), "carrier rule "+part)
	}
	// Two carts of each common part and four of each mirror part: the five
	// RUN carts the cell starts on, a swap's worth more of each, and the
	// MIRROR-R carts a changeover brings.
	stock := map[string]int{"COMMON-1": 2, "COMMON-2": 2, "COMMON-3": 2, "MIRROR-L": 4, "MIRROR-R": 4}
	i := 0
	for _, part := range w6AllParts {
		for n := 1; n <= stock[part]; n++ {
			b := &corebins.Bin{BinTypeID: cart.ID, Label: fmt.Sprintf("%s-%d", part, n), NodeID: &slots[i].ID, Status: "available"}
			mustNil(t, db.CreateBin(b), "cart "+b.Label)
			mustNil(t, db.SetBinManifest(b.ID, `{"items":[]}`, part, w6Full), "manifest "+b.Label)
			mustNil(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+b.Label)
			i++
		}
	}
}

// configureEdge sets the cell up on the Edge as its Settings pages do: the
// process, the positions, the styles and their claims, the pins, the counter,
// and the load station's counter with the positions counted at it.
func (c *w6Cell) configureEdge() {
	t, db, eng := c.t, c.edge.DB, c.edge.Engine
	for i, part := range w6AllParts {
		mustNil(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: int64(i + 1), Name: part, Code: part, UOPCapacity: w6Full}),
			"catalog "+part)
	}
	pairs, err := c.core.eng.DB().ListPayloadBinTypeMappings()
	mustNil(t, err, "carrier list")
	var carriers []protocol.PayloadBinTypeInfo
	for _, p := range pairs {
		carriers = append(carriers, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}
	eng.SetPayloadBinTypes(carriers)

	c.processID, err = db.CreateProcess(w6PLC, "the 4x2-shaped cell", "", "", false)
	mustNil(t, err, "process")
	for i, p := range w6Pos {
		c.nodes[i], err = db.CreateProcessNode(processes.NodeInput{
			ProcessID: c.processID, CoreNodeName: p, Code: fmt.Sprintf("W6P%d", i+1), Name: p, Sequence: i + 1, Enabled: true,
		})
		mustNil(t, err, "position "+p)
		_, err = db.EnsureProcessNodeRuntime(c.nodes[i])
		mustNil(t, err, "runtime "+p)
	}
	c.run, err = db.CreateStyle("RUN", "", c.processID)
	mustNil(t, err, "style RUN")
	c.alt, err = db.CreateStyle("ALT", "", c.processID)
	mustNil(t, err, "style ALT")
	for _, s := range []struct {
		id    int64
		parts [5]string
	}{{c.run, w6RunParts}, {c.alt, w6AltParts}} {
		for i, p := range w6Pos {
			_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
				StyleID: s.id, CoreNodeName: p, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
				PayloadCode: s.parts[i], UOPCapacity: w6Full, ReorderPoint: c.reorder,
				InboundSource: w6Market, InboundStaging: p + "-IN", OutboundStaging: p + "-OUT",
				OutboundDestination: w6Dest, KeepStagedNode: domain.Ptr(""), AutoReorder: domain.Ptr(true),
			})
			mustNil(t, err, "claim "+s.parts[i]+" at "+p)
		}
	}
	mustNil(t, db.SetActiveStyle(c.processID, &c.run), "RUN active")
	for i := range w6Pos {
		claim, err := db.GetStyleNodeClaimByNode(c.run, w6Pos[i])
		mustNil(t, err, "RUN claim")
		mustNil(t, db.SetProcessNodeRuntime(c.nodes[i], &claim.ID, 0), "runtime claim")
	}
	// A person's pins: one CATID each.
	mustNil(t, db.SetStyleExpectedCATID(c.run, fmt.Sprint(catRUN)), "pin RUN")
	mustNil(t, db.SetStyleExpectedCATID(c.alt, fmt.Sprint(catALT)), "pin ALT")

	c.publish()
	mustNil(t, eng.EditProcessCounter(c.processID, domain.ProcessCounterEdit{PLCName: w6PLC, TagName: w6Counter, Enabled: true}),
		"the counter")
	if !c.anyStation() && !c.unbound {
		return
	}
	lc, err := eng.CreateStationCounter(c.processID, "load station", w6Station, "")
	mustNil(t, err, "the load station's counter")
	c.counter = lc.ID
	for i := range w6Pos {
		if c.station[i] {
			mustNil(t, eng.SetCountedAt(c.nodes[i], &c.counter), "counted at the load station: "+w6Pos[i])
		}
	}
}

func (c *w6Cell) anyStation() bool {
	for _, s := range c.station {
		if s {
			return true
		}
	}
	return false
}

// restart stops the Edge and boots another on the same database, as a
// process restart does: what was in memory is gone, the database stays. The
// shutdown is the composition root's: the delta sink's final flush, then the
// engine.
func (c *w6Cell) restart(label string) {
	c.t.Helper()
	if c.push != nil {
		c.push.Stop()
	}
	c.sink.Stop()
	c.edge.Engine.Stop()
	v := c.plc.state().Identity
	seen := c.observations(v)
	c.edge, c.tickers, c.sink = startW6Edge(c.t, c.edge.DB, c.plc)
	c.wireBus()
	c.publish()
	// The monitor's first read after a start is its baseline.
	c.awaitConfirm(label+": the restarted monitor's first read", v, seen)
}

// wireBus routes the Edge and Core to each other, as ksrCell does: Core's
// order handlers, the Edge's register, its draws (bin_uop_delta), its
// production ticks, its claims and Core's asks.
func (c *w6Cell) wireBus() {
	t, coreDB := c.t, c.core.eng.DB()
	h := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", c.core.eng.Dispatcher())
	in := protocol.NewIngestor(nil)
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	router.Register(r, protocol.TypeComplexOrderRequest, h.HandleComplexOrderRequest)
	router.Register(r, protocol.TypeOrderCancel, h.HandleOrderCancel)
	router.Register(r, protocol.TypeOrderReceipt, h.HandleOrderReceipt)
	router.Register(r, protocol.TypeOrderRelease, h.HandleOrderRelease)
	router.Register(r, protocol.TypeOrderIngest, h.HandleOrderIngest)
	data := coremessaging.NewCoreDataService(coreDB, h, service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core"})
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectEdgeRegister, data.HandleEdgeRegister)
	router.RegisterSubject(subjects, protocol.SubjectBinUOPDelta, data.HandleBinUOPDelta)
	router.RegisterSubject(subjects, protocol.SubjectProductionTicks, data.HandleProductionTicks)
	router.RegisterSubject(subjects, protocol.SubjectPlantClaims, data.HandlePlantClaims)
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { subjects.Dispatch(env, p) })
	if c.askKinds == nil {
		c.askKinds = registerAsks(subjects, coreDB, c.core.doors, h)
	} else {
		registerAsks(subjects, coreDB, c.core.doors, h)
	}
	in.Dispatch = func(env *protocol.Envelope) { r.Dispatch(env, env.Type) }
	c.coreIn = in
	c.bus = harness.NewBus(t,
		harness.EdgeSide{EdgeStore: c.edge.DB, EdgeIngestor: c.edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: in},
	)
	c.push = harness.StartBinsPush(t, coreDB, c.edge.Engine, 20*time.Millisecond)
	c.bus.SetBinsPush(c.push)
	c.push.AnswerAsks(c.askKinds)
	c.registerEdge()
	c.settle()
}

// fill brings each empty position a full cart of the running style's part:
// the operator's REQUEST at an empty position is one plain delivery, which
// the fleet carries and the operator confirms.
func (c *w6Cell) fill() {
	c.t.Helper()
	before := c.lastID()
	for i, n := range c.nodes {
		if _, err := c.edge.Engine.RequestNodeMaterial(n, 1); err != nil {
			c.dump("fill")
			c.t.Fatalf("fill %s: %v", w6Pos[i], err)
		}
	}
	c.tick()
	c.carryAll("the cell filled", before, nil)
	c.noteCarts()
	for i := range w6Pos {
		if c.cart(i) == nil {
			c.dump("fill")
			c.t.Fatalf("%s holds no cart after its fill", w6Pos[i])
		}
	}
}

// ── the line ────────────────────────────────────────────────────────────────

// publish writes the tags WarLink publishes into the Edge's PLC cache, as
// WarLink's poll would after the scan.
func (c *w6Cell) publish() {
	pm := c.edge.Engine.PLCManager()
	for tag, v := range c.plc.values() {
		pm.SetTagValueForTest(w6PLC, tag, v)
	}
	c.lastPublish = time.Now()
}

// cycle turns the line n machine cycles, one Edge poll pass after each. When
// a cycle changes the identity (a drain change landing), the next cycle waits
// for the monitor to confirm it, as the poll's attribution reads the
// confirmed part. n = 0 is one pass with the line standing.
func (c *w6Cell) cycle(n int) {
	c.t.Helper()
	if n == 0 {
		c.publish()
		c.tickers.poll.pass(c.t, "the poll")
	}
	for i := 0; i < n; i++ {
		before := c.plc.state().Identity
		c.plc.cycle()
		c.publish()
		c.tickers.poll.pass(c.t, "the poll")
		if after := c.plc.state().Identity; after != before {
			c.awaitConfirm(fmt.Sprintf("the program change to %d", after), after, c.observations(after))
		}
	}
	c.settle()
}

// change is the program change the PLC drains for: loading stops, the line
// counts out, then the identity changes and the counter resets (cycle waits
// for the confirm).
func (c *w6Cell) change(v int64) { c.plc.do(func(l *simwarlink.Line) { l.Change(v) }) }

// observations is how many times the monitor has confirmed v on the process.
func (c *w6Cell) observations(v int64) int64 {
	var n int64
	err := c.edge.DB.QueryRow(`SELECT observations FROM plc_catid_observations WHERE process_id = ? AND catid = ?`,
		c.processID, fmt.Sprint(v)).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// awaitConfirm waits until the monitor has confirmed v (the observation it
// records on each confirm), plus one of its reads for the snapshot the poll
// attributes by.
func (c *w6Cell) awaitConfirm(what string, v, seen int64) {
	c.t.Helper()
	c.waitFor(what+": the monitor confirms it", 15*time.Second, func() bool { return c.observations(v) > seen })
	time.Sleep(600 * time.Millisecond)
}

// waitFor polls ok, settling the bus, until it holds or the budget is spent.
func (c *w6Cell) waitFor(what string, budget time.Duration, ok func() bool) {
	c.t.Helper()
	for deadline := time.Now().Add(budget); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		c.settle()
		if ok() {
			return
		}
	}
	c.dump("timed out: " + what)
	c.t.Fatalf("timed out waiting for: %s", what)
}

// ── the carts ───────────────────────────────────────────────────────────────

// noteCarts reads which cart each position holds and opens and closes the
// tenures at the line's counts as they stand: a cart lands or leaves between
// machine cycles.
func (c *w6Cell) noteCarts() {
	c.t.Helper()
	st := c.plc.state()
	counts := st.Loads - int64(st.InFlight)
	for i, n := range c.nodes {
		rt, err := c.edge.DB.GetProcessNodeRuntime(n)
		mustNil(c.t, err, "runtime")
		var bin int64
		if rt.ActiveBinID != nil {
			bin = *rt.ActiveBinID
		}
		cur := c.cart(i)
		if cur != nil && cur.bin == bin {
			continue
		}
		if cur != nil {
			cur.open, cur.loadsOut, cur.countsOut = false, st.Loads, counts
		}
		if bin != 0 {
			b, err := c.core.eng.DB().GetBin(bin)
			mustNil(c.t, err, "cart")
			c.tenures[i] = append(c.tenures[i], &w6Tenure{bin: bin, label: b.Label, part: b.PayloadCode, pos: i,
				loadsIn: st.Loads, countsIn: counts, open: true})
		}
	}
}

// cart is the tenure of the cart standing at position i, nil when none.
func (c *w6Cell) cart(i int) *w6Tenure {
	if n := len(c.tenures[i]); n > 0 && c.tenures[i][n-1].open {
		return c.tenures[i][n-1]
	}
	return nil
}

// truth is what was taken from a cart: the loads made under its own part
// during its stay (share).
func (c *w6Cell) truth(tn *w6Tenure) int64 {
	loads, _ := c.plc.strokes()
	return c.share(tn, loads, func(t *w6Tenure) (int64, int64) {
		if t.open {
			return t.loadsIn, int64(len(loads))
		}
		return t.loadsIn, t.loadsOut
	})
}

// counts are the counter's counts a cart is charged: the counts made under its
// own part during its stay (share). A count between carts is logged, never the
// next cart's (decision 55); the scenarios land carts between cycles, so none
// falls between them.
func (c *w6Cell) counts(tn *w6Tenure) int64 {
	_, counts := c.plc.strokes()
	return c.share(tn, counts, func(t *w6Tenure) (int64, int64) {
		if t.open {
			return t.countsIn, int64(len(counts))
		}
		return t.countsIn, t.countsOut
	})
}

// share is a cart's strokes (decision 55): over ids (the identity each stroke
// at the position was made under, its loads or its counts) and the cart's span
// of them, the strokes of the cart's own part (partOfStroke). Any other stroke
// in its stay — ALT's part at a cart of RUN's where the two differ, RUN's at a
// cart of ALT's — is logged, and no cart's; nothing a cart did not take carries
// to the next one. A part no style pins is the standing cart's: its draw is the
// ledger's until a person Charges it there (read apart: the cart check skips a
// position with an open row).
func (c *w6Cell) share(tn *w6Tenure, ids []int64, span func(*w6Tenure) (int64, int64)) int64 {
	in, out := span(tn)
	var got int64
	for k := in; k < out && k < int64(len(ids)); k++ {
		if p := partOfStroke(tn.pos, ids[k]); p == "" || p == tn.part {
			got++
		}
	}
	return got
}

// partOfStroke is the part a stroke made under identity v draws at position
// i: RUN's or ALT's part there, "" for a part no style pins.
func partOfStroke(i int, v int64) string {
	switch v {
	case catRUN:
		return w6RunParts[i]
	case catALT:
		return w6AltParts[i]
	}
	return ""
}

// count is a cart's count as Core holds it, and as the Edge's seat holds it
// while the cart stands there (both must agree).
func (c *w6Cell) count(tn *w6Tenure) int64 {
	c.t.Helper()
	b, err := c.core.eng.DB().GetBin(tn.bin)
	mustNil(c.t, err, "cart "+tn.label)
	if tn.open {
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[tn.pos])
		mustNil(c.t, err, "runtime")
		if int64(rt.RemainingUOPCached) != int64(b.UOPRemaining) {
			c.t.Errorf("cart %s at %s: the Edge counts %d, Core %d", tn.label, w6Pos[tn.pos], rt.RemainingUOPCached, b.UOPRemaining)
		}
	}
	return int64(b.UOPRemaining)
}

// flush sends the Edge's draws to Core and lets Core apply them.
func (c *w6Cell) flush() {
	c.sink.Flush()
	c.settle()
}

// counted is every count the line has made.
func (c *w6Cell) counted() int64 {
	st := c.plc.state()
	return st.Loads - int64(st.InFlight)
}

// offsets fails unless every standing cart reads want high: its count less
// what the line took from it (negative: it reads low).
func (c *w6Cell) offsets(label string, want int64) {
	c.t.Helper()
	c.flush()
	c.noteCarts()
	for i := range w6Pos {
		tn := c.cart(i)
		if tn == nil {
			c.t.Fatalf("%s: %s holds no cart", label, w6Pos[i])
		}
		drawn := w6Full - c.count(tn)
		if got := c.truth(tn) - drawn; got != want {
			c.dump(label)
			c.t.Fatalf("%s: cart %s at %s drew %d against %d taken from it: it reads %d high, want %d", label, tn.label,
				w6Pos[i], drawn, c.truth(tn), got, want)
		}
	}
}

// swapAll is the operator's REQUEST at every position: a two-robot swap
// each, carried to the end.
func (c *w6Cell) swapAll(label string) {
	c.t.Helper()
	before := c.lastID()
	for i, n := range c.nodes {
		if _, err := c.edge.Engine.RequestNodeMaterial(n, 1); err != nil {
			c.dump(label)
			c.t.Fatalf("%s: REQUEST at %s: %v", label, w6Pos[i], err)
		}
	}
	c.tick()
	if legs := len(c.newRows(before)); legs != 2*len(w6Pos) {
		c.dump(label)
		c.t.Fatalf("%s: %d legs, want a swap's two at each of the %d positions", label, legs, len(w6Pos))
	}
	c.carryAll(label, before, nil)
}

// ── the ledger ──────────────────────────────────────────────────────────────

// w6Holds is the holds ledger as it stands, read from the table.
type w6Holds struct {
	rows                []store.CatidHold
	open, charged, writ int64
}

func (c *w6Cell) holds() w6Holds {
	c.t.Helper()
	rows, err := c.edge.DB.Query(`SELECT id FROM catid_holds WHERE process_id = ? ORDER BY id`, c.processID)
	mustNil(c.t, err, "holds")
	var ids []int64
	for rows.Next() {
		var id int64
		mustNil(c.t, rows.Scan(&id), "hold id")
		ids = append(ids, id)
	}
	mustNil(c.t, rows.Close(), "holds")
	var h w6Holds
	for _, id := range ids {
		r, err := c.edge.DB.GetCatidHold(id)
		mustNil(c.t, err, "hold")
		h.rows = append(h.rows, *r)
		switch {
		case r.ResolvedAt == nil:
			h.open += r.Qty
		case r.Resolution == "written-off":
			h.writ += r.Qty
		default:
			h.charged += r.Qty
		}
	}
	return h
}

// shares is the ledger's held and off-plan rows alone: a CATID's draw Shingo
// could not place, without the strokes logged with no cart of their part (a
// swap's gap, a cart of another part standing there).
func (c *w6Cell) shares() w6Holds {
	c.t.Helper()
	all := c.holds()
	var h w6Holds
	for _, r := range all.rows {
		if r.Reason == "no-cart" {
			continue
		}
		h.rows = append(h.rows, r)
		switch {
		case r.ResolvedAt == nil:
			h.open += r.Qty
		case r.Resolution == "written-off":
			h.writ += r.Qty
		default:
			h.charged += r.Qty
		}
	}
	return h
}

// loggedAt is what the ledger keeps of position i's draw, open or written
// off: its own rows (a held or off-plan share, a stroke with no cart of its
// part), and on the cell counter the whole-draw (node 0) rows.
func (c *w6Cell) loggedAt(h w6Holds, i int) int64 {
	var n int64
	for _, r := range h.rows {
		if r.ResolvedAt != nil && r.Resolution != "written-off" {
			continue // charged or released: it is on a cart now
		}
		if r.ProcessNodeID == c.nodes[i] || (r.ProcessNodeID == 0 && !c.station[i]) {
			n += r.Qty
		}
	}
	return n
}

// w6Ledger is the run's three ledgers, read at one instant: the carts' draws
// and the ledger's logged strokes, against the loads.
type w6Ledger struct {
	carts, logged, loads int64
	// holdsOpen is the open held and off-plan shares (shares); noCart the
	// open strokes logged with no cart of their part.
	holdsOpen, noCart int64
	snapByStyle       map[int64]int64
	snapByCATID       map[string]int64
	hourly            map[int64]int64
	core              map[int64]int64
}

// ledger reads the three ledgers and fails on the first disagreement with the
// line: at each position, carts + logged + written off = loads (decision 55);
// checkCarts is false for a position-by-position check the scenario makes
// itself.
func (c *w6Cell) ledger(label string, checkCarts bool) w6Ledger {
	c.t.Helper()
	c.flush()
	c.noteCarts()
	st := c.plc.state()
	h := c.holds()
	l := w6Ledger{}
	for _, r := range h.rows {
		switch {
		case r.ResolvedAt != nil:
		case r.Reason == "no-cart":
			l.noCart += r.Qty
		default:
			l.holdsOpen += r.Qty
		}
	}
	for i := range w6Pos {
		var drawn int64
		for _, tn := range c.tenures[i] {
			drawn += w6Full - c.count(tn)
		}
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
		mustNil(c.t, err, "runtime")
		if rt.PendingUOPDelta != 0 {
			c.dump(label)
			c.t.Fatalf("%s: %s: the seat holds %d of %q; a consume seat holds nothing (decision 55)", label, w6Pos[i],
				rt.PendingUOPDelta, rt.PendingUOPPayload)
		}
		logged := c.loggedAt(h, i)
		got := drawn + logged
		want := st.Loads
		if !c.station[i] {
			want = st.Loads - int64(st.InFlight)
		}
		if got != want {
			c.dump(label)
			c.t.Fatalf("%s: %s: carts drew %d and the ledger logged %d (= %d), the line counted %d at "+
				"its counter (%d loads, %d in flight)", label, w6Pos[i], drawn, logged, got, want, st.Loads, st.InFlight)
		}
		l.carts += drawn
		l.logged += logged
		l.loads += want
		if !checkCarts || logged != 0 {
			continue
		}
		for _, tn := range c.tenures[i] {
			want := c.truth(tn)
			if !c.station[i] {
				want = c.counts(tn)
			}
			got := w6Full - c.count(tn)
			if got != want {
				c.dump(label)
				c.t.Fatalf("%s: cart %s at %s drew %d, want %d (%d loads of its part, %d counts of its part)",
					label, tn.label, w6Pos[i], got, want, c.truth(tn), c.counts(tn))
			}
		}
	}
	c.record(label, &l)
	return l
}

// record reads the production record three ways and checks it against the
// PLC: counter_snapshots by CATID is what the line counted under each
// identity; by style it agrees with hourly and with what Core received.
func (c *w6Cell) record(label string, l *w6Ledger) {
	c.t.Helper()
	l.snapByStyle, l.snapByCATID, l.hourly, l.core = map[int64]int64{}, map[string]int64{}, map[int64]int64{}, map[int64]int64{}
	sums := func(sql string, into func(k string, v int64)) {
		rows, err := c.edge.DB.Query(sql, c.processID)
		mustNil(c.t, err, "record")
		defer rows.Close()
		for rows.Next() {
			var k string
			var v int64
			mustNil(c.t, rows.Scan(&k, &v), "record row")
			into(k, v)
		}
		mustNil(c.t, rows.Err(), "record rows")
	}
	style := func(into map[int64]int64) func(string, int64) {
		return func(k string, v int64) {
			var id int64
			if _, err := fmt.Sscan(k, &id); err != nil {
				c.t.Fatalf("record: style %q: %v", k, err)
			}
			into[id] = v
		}
	}
	sums(`SELECT CAST(COALESCE(style_id, 0) AS TEXT), SUM(delta) FROM counter_snapshots WHERE process_id = ? AND delta > 0 GROUP BY style_id`,
		style(l.snapByStyle))
	sums(`SELECT catid, SUM(delta) FROM counter_snapshots WHERE process_id = ? AND delta > 0 GROUP BY catid`,
		func(k string, v int64) { l.snapByCATID[k] = v })
	sums(`SELECT CAST(style_id AS TEXT), SUM(delta) FROM hourly_counts WHERE process_id = ? GROUP BY style_id`,
		style(l.hourly))
	mustNil(c.t, c.shipper.ShipPending(), "ship the production ticks")
	events, err := c.core.eng.DB().ListCellPartEvents(w6Edge, time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
	mustNil(c.t, err, "Core's part events")
	for _, e := range events {
		if e.ProcessID == c.processID {
			l.core[e.StyleID] += e.Delta
		}
	}
	for v, n := range c.plc.countedBy() {
		if got := l.snapByCATID[fmt.Sprint(v)]; got != n {
			c.t.Fatalf("%s: the record has %d counted under CATID %d, the PLC counted %d (record by CATID %v)",
				label, got, v, n, l.snapByCATID)
		}
	}
	for _, s := range []int64{c.run, c.alt} {
		if l.snapByStyle[s] != l.hourly[s] || l.snapByStyle[s] != l.core[s] {
			c.t.Fatalf("%s: style %d: counter_snapshots %d, hourly %d, Core received %d", label, s, l.snapByStyle[s],
				l.hourly[s], l.core[s])
		}
	}
}

// line prints the ledger line.
func (c *w6Cell) line(label string, l w6Ledger) {
	c.t.Logf("LEDGER %s: carts Σ %d + logged (written off included) %d = loads Σ %d; shares open %d, no cart %d; "+
		"record RUN %d / ALT %d; Core received %d+%d", label, l.carts, l.logged, l.loads, l.holdsOpen, l.noCart,
		l.snapByStyle[c.run], l.snapByStyle[c.alt], l.core[c.run], l.core[c.alt])
}

// wantRecord fails unless the record by style is run / alt.
func (c *w6Cell) wantRecord(label string, l w6Ledger, run, alt int64) {
	c.t.Helper()
	if l.snapByStyle[c.run] != run || l.snapByStyle[c.alt] != alt {
		c.t.Fatalf("%s: the record has RUN %d / ALT %d, want RUN %d / ALT %d (by CATID %v)", label,
			l.snapByStyle[c.run], l.snapByStyle[c.alt], run, alt, l.snapByCATID)
	}
}

// wantRecordRunOut fails unless the record holds RUN's counts to the landing
// and past it the run-out at least (decision 55: the run-out is RUN's), every
// other count ALT's: the counts the cell made before the monitor confirmed the
// new part are RUN's too, by the CATID it still read.
func (c *w6Cell) wantRecordRunOut(label string, l w6Ledger, run int64) {
	c.t.Helper()
	got := l.snapByStyle[c.run] + l.snapByStyle[c.alt]
	if l.snapByStyle[c.run] < run+w6Depth || got != c.counted() {
		c.t.Fatalf("%s: the record has RUN %d / ALT %d, want RUN at least %d (the landing's %d and the run-out's %d) and %d in all (by CATID %v)",
			label, l.snapByStyle[c.run], l.snapByStyle[c.alt], run+w6Depth, run, w6Depth, c.counted(), l.snapByCATID)
	}
}

// ── the orders ──────────────────────────────────────────────────────────────

// carryAll runs every order of the process made after `after` to the end at
// both ends, as the floor does: the fleet carries plain orders; a leg staged
// at its wait is released (a changeover's at its node, as the tile's RELEASE
// does; another's by its own RELEASE); a delivered order is confirmed. A
// position in hold is not released: its legs wait at their waits, the old
// cart stays, and a changeover's gate stays red. A later carryAll without the
// hold releases and finishes them.
func (c *w6Cell) carryAll(label string, after int64, hold map[int]bool) {
	c.t.Helper()
	disp := edgeengine.ReleaseDisposition{CalledBy: "w6-operator"}
	c.eventually(label+": every order landed and confirmed", func() bool {
		c.fleetStep()
		done := true
		for _, o := range c.newRows(after) {
			edge := c.edgeRow(o.ID)
			if edge.Status == protocol.StatusDelivered {
				mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
			}
			if protocol.IsTerminal(edge.Status) && protocol.IsTerminal(c.coreOf(o).Status) {
				continue
			}
			core := c.coreOf(o)
			if o.OrderType != protocol.OrderTypeComplex || core.VendorOrderID == "" {
				done = false // the fleet carries a plain order (fleetStep)
				continue
			}
			view := c.core.sim.GetOrder(core.VendorOrderID)
			if view == nil {
				done = false
				continue
			}
			held := edge.ProcessNodeID != nil && hold[c.posOf(*edge.ProcessNodeID)]
			if held && view.State == "WAITING" && !view.Complete && edge.Status == protocol.StatusStaged {
				continue // held at its wait
			}
			done = false
			switch view.State {
			case "CREATED":
				c.drive(core, "RUNNING")
			case "RUNNING":
				if view.Complete {
					c.drive(core, "FINISHED")
				} else {
					c.drive(core, "WAITING")
				}
			case "WAITING":
				if view.Complete {
					c.drive(core, "RUNNING")
				} else if edge.Status == protocol.StatusStaged && edge.ProcessNodeID != nil {
					c.release(label, o, *edge.ProcessNodeID, disp)
				}
			}
		}
		return done
	})
	c.noteCarts()
}

// release releases a staged leg as the station's tile does: at its node while
// a changeover open changes that node (the tile's RELEASE on a changeover
// leg), by its own RELEASE otherwise — a position the changeover leaves
// unchanged shows the ordinary RELEASE.
func (c *w6Cell) release(label string, o domain.Order, nodeID int64, disp edgeengine.ReleaseDisposition) {
	if c.changingOver(nodeID) {
		if _, err := c.edge.Engine.ReleaseChangeoverWaitForNode(c.processID, nodeID, disp); err != nil {
			c.t.Logf("%s: release at node %d: %v", label, nodeID, err)
		}
	} else if err := c.edge.Engine.ReleaseOrderWithLineside(o.ID, disp); err != nil {
		c.t.Logf("%s: release of order %d: %v", label, o.ID, err)
	}
	c.settle()
}

// changingOver reports whether the open changeover has a task at the node
// that is not "unchanged".
func (c *w6Cell) changingOver(nodeID int64) bool {
	co, err := c.edge.DB.GetActiveProcessChangeover(c.processID)
	if err != nil || co == nil {
		return false
	}
	task, err := c.edge.DB.GetChangeoverNodeTaskByNode(co.ID, nodeID)
	return err == nil && task != nil && task.Situation != "unchanged"
}

func (c *w6Cell) posOf(nodeID int64) int {
	for i, n := range c.nodes {
		if n == nodeID {
			return i
		}
	}
	return -1
}

// confirmedAll fails unless every order of the process ended confirmed at
// Core (the deliveries ledger).
func (c *w6Cell) confirmedAll(label string) int {
	c.t.Helper()
	n := 0
	for _, o := range c.rows() {
		if s := c.coreOf(o).Status; s != protocol.StatusConfirmed {
			c.dump(label)
			c.t.Fatalf("%s: order %d (%s -> %s) ended %s at Core, want confirmed", label, o.ID, o.SourceNode, o.DeliveryNode, s)
		}
		n++
	}
	return n
}
