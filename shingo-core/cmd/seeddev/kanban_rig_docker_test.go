//go:build docker

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/plantspec"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/www"
)

// kanban_rig_docker_test.go — the rig's loop on its real plant file in the docker
// shards, on every gate. plants/kanban-press.yaml seeded by seedCore, the real keeper,
// doors and walk, with no compose stack: scripts/kanban-rig.sh R1..R6 on the stack is the
// certification pair before a merge (docs/fleet-simulator/kanban.md).
//
// WHAT IS REAL. The orders come in through the wire's door (HandleOrderRequest) as the
// Edge sends them. The fleet is the simulator, its orders driven to FINISHED, and each
// delivery is confirmed by the receipt the Edge would send. The window's lit type is read
// off the snapshot Core sends the Edge, the tap is the CLEAR's door and the Edge's
// empty-out, and the keeper is Tick.
//
// WHAT IS NOT. A press-index swap's two complex legs are plain moves here: a full off the
// front, the back indexed forward and a refill from K. The legs' choreography has its own
// tests and the stack runs it. What these pin is the loop's counts. The process's parts
// are stood on a cart by SetBinManifest: no counter runs here.
//
// THE ENGINE IS STARTED AND STOPPED, as newGoldenPlant's is and for its reason: from then
// on every decision is made by a synchronous call from this test, so a statement count
// has nothing else in it and a keeper tick never lands between two of its steps.

const (
	rigStation = "edge1.line1"
	rigK       = "PRESS-KANBAN"
	rigK2      = "WELD-KANBAN"
	rigZ45     = "ZONE-45"
	rigSpare   = "ZONE-45-SPARE"
	rigSM      = "KP-SM"
	rigSmall   = "SMALL-45"

	// The settled counts, the numbers TestKanbanCost_* assert in engine on the cost rig's
	// shape (kanban_pins_cost_docker_test.go: keeperTickSteady, snapshotBuild,
	// intakeDoorToASource, parkedArm). Here they are on the plant the runs use, after the
	// loop has gone round.
	rigKeeperTickSteady = 4
	rigSnapshotBuild    = 10
	rigIntakeDoor       = 8
	rigParkedArm        = 8
)

// rigSide is one side of the two-off press: a claim's front and back, and its part.
type rigSide struct{ front, back, part string }

var rigSides = []rigSide{
	{"PLN_021", "PLN_022", "KP-SM-L"},
	{"PLN_023", "PLN_024", "KP-SM-R"},
}

// kanbanRig is the seeded plant and a stopped engine. cdb counts what the engine reads;
// the test's own reads and writes go through db and are not counted.
type kanbanRig struct {
	t       *testing.T
	db      *store.DB
	eng     *engine.Engine
	sim     *simulator.SimulatorBackend
	counter *store.QueryCounter
	doors   *www.Doors
	feed    *messaging.CoreDataService
	windows []string
	digest  string
	next    int
}

func newKanbanRig(t *testing.T) *kanbanRig {
	t.Helper()
	db, dbcfg := testdb.OpenWithConfig(t)
	plant := kanbanPressPlant(t)
	testutil.MustNoErr(t, seedCore(db, plant, map[string]int64{}), "seed the rig")
	seedClaimsMirror(t, db, rigClaimedPlant(plant))

	cdb, counter, err := store.OpenCounting(dbcfg)
	testutil.MustNoErr(t, err, "open the counting store")
	t.Cleanup(func() { cdb.Close() })
	sim := simulator.New()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: cdb, Fleet: sim, LogFunc: func(string, ...any) {}})
	eng.Start()
	eng.Stop()

	feed := messaging.NewCoreDataService(cdb, messaging.NewCoreHandler(cdb, nil, "core", "shingo.dispatch", nil),
		service.EpochAnnounce{})
	feed.SetGroupLinesFunc(eng.UnloaderGroupLines)
	windows := []string{"ULN_021", "ULN_022", "ULN_023", "ULN_024"}
	digest, err := protocol.BinWatchDigest(windows)
	testutil.MustNoErr(t, err, "watch digest")
	r := &kanbanRig{t: t, db: db, eng: eng, sim: sim, counter: counter, doors: www.NewDoors(eng), feed: feed,
		windows: windows, digest: digest}
	r.tick()
	r.wholeAtRest("as seeded")
	return r
}

// rigClaimedPlant is the plant as far as the Edge's claims go: a loader's windows carry
// no claim row on the Edge (TestSeedKanbanPress_EdgeBuildsTheBoards), so the mirror it
// publishes has none.
func rigClaimedPlant(p *plantspec.Plant) *plantspec.Plant {
	out := *p
	out.Claims = nil
	for _, c := range p.Claims {
		if c.WindowOf == "" {
			out.Claims = append(out.Claims, c)
		}
	}
	return &out
}

func rigEnvelope(station string) *protocol.Envelope {
	return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station}}
}

// tick is one keeper tick, and the snapshot it leaves is what card reads.
func (r *kanbanRig) tick() { r.eng.KanbanKeeper().Tick() }

// card is a kanban's line of a type, as of the last tick.
func (r *kanbanRig) card(group, binType string) engine.KanbanLineState {
	r.t.Helper()
	for _, s := range r.eng.KanbanLineStates() {
		if s.GroupNode == group && s.BinTypeCode == binType {
			return s
		}
	}
	r.t.Fatalf("no card for %s/%s", group, binType)
	return engine.KanbanLineState{}
}

// residents is the carts of a type standing in a group.
func (r *kanbanRig) residents(group, binType string) int {
	r.t.Helper()
	var n int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM bins b JOIN nodes n ON n.id = b.node_id
		JOIN nodes g ON g.id = n.parent_id JOIN bin_types t ON t.id = b.bin_type_id
		WHERE g.name = $1 AND t.code = $2`, group, binType).Scan(&n), "residents")
	return n
}

// cartAt is the id of the one cart on a node, 0 for none.
func (r *kanbanRig) cartAt(node string) int64 {
	r.t.Helper()
	n, err := r.db.GetNodeByName(node)
	testutil.MustNoErr(r.t, err, "node "+node)
	on, err := r.db.ListBinsByNode(n.ID)
	testutil.MustNoErr(r.t, err, "bins at "+node)
	if len(on) == 0 {
		return 0
	}
	return on[0].ID
}

// groupOf is a position's group, or the node's own name for an order still naming a group.
func (r *kanbanRig) groupOf(node string) string {
	r.t.Helper()
	var group string
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT COALESCE(g.name, n.name) FROM nodes n
		LEFT JOIN nodes g ON g.id = n.parent_id WHERE n.name = $1`, node).Scan(&group), "the group of "+node)
	return group
}

// order is what the Edge sends, through the wire's door.
func (r *kanbanRig) order(req protocol.OrderRequest) *orders.Order {
	r.t.Helper()
	r.next++
	req.OrderUUID = fmt.Sprintf("rig-%d", r.next)
	req.Quantity = 1
	r.eng.Dispatcher().HandleOrderRequest(rigEnvelope(rigStation), &req)
	o := testdb.RequireOrder(r.t, r.db, req.OrderUUID)
	if o == nil {
		r.t.Fatalf("the door took no order for %+v", req)
	}
	return o
}

func (r *kanbanRig) move(from, to string, cart int64) *orders.Order {
	r.t.Helper()
	return r.order(protocol.OrderRequest{OrderType: protocol.OrderTypeMove, OriginClass: protocol.OriginClassNoDemand,
		SourceNode: from, DeliveryNode: to, BinID: cart})
}

// fly lands everything in the air: the scanner acquires, the fleet finishes what it was
// given, and the Edge confirms each delivery. It ends when a pass moves nothing; what is
// left live is parked, and quiet says so.
func (r *kanbanRig) fly() {
	r.t.Helper()
	for pass := 0; pass < 12; pass++ {
		r.eng.RunFulfillmentScan()
		moved := false
		for _, o := range r.live() {
			switch {
			case o.Status == protocol.StatusDelivered:
				r.eng.Dispatcher().HandleOrderReceipt(rigEnvelope(o.StationID),
					&protocol.OrderReceipt{OrderUUID: o.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
				moved = true
			case o.VendorOrderID != "" && r.sim.HasOrder(o.VendorOrderID):
				r.sim.DriveSimpleLifecycle(o.VendorOrderID)
				moved = true
			}
		}
		if !moved {
			return
		}
	}
	r.t.Fatalf("the fleet did not settle in 12 passes: %s", r.said(r.live()))
}

// live is every order not at its end, oldest first.
func (r *kanbanRig) live() []*orders.Order {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT id FROM orders WHERE status NOT IN (` + protocol.TerminalStatusSQLList() + `) ORDER BY id`)
	testutil.MustNoErr(r.t, err, "live orders")
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		testutil.MustNoErr(r.t, rows.Scan(&id), "scan")
		ids = append(ids, id)
	}
	testutil.MustNoErr(r.t, rows.Err(), "rows")
	out := make([]*orders.Order, 0, len(ids))
	for _, id := range ids {
		o, err := r.db.GetOrder(id)
		testutil.MustNoErr(r.t, err, "order")
		out = append(out, o)
	}
	return out
}

func (r *kanbanRig) said(os []*orders.Order) string {
	var out []string
	for _, o := range os {
		out = append(out, fmt.Sprintf("%d %s %s->%s %s %q", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode, o.Status, o.QueueCause))
	}
	return strings.Join(out, "; ")
}

// lit is the windows' assignments on the snapshot Core sends the Edge, by window: the
// read the board lights a tile from.
func (r *kanbanRig) lit() map[string]protocol.NodeBinsAssignment {
	r.t.Helper()
	r.feed.HandleBinWatch(rigEnvelope(rigStation), &protocol.BinWatchList{Digest: r.digest, Nodes: r.windows})
	var sent []byte
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT payload FROM outbox WHERE msg_type = $1 ORDER BY id DESC LIMIT 1`,
		"data."+protocol.SubjectBinsSnapshot).Scan(&sent), "the snapshot off the outbox")
	var env protocol.Envelope
	testutil.MustNoErr(r.t, json.Unmarshal(sent, &env), "the envelope")
	var data protocol.Data
	testutil.MustNoErr(r.t, env.DecodePayload(&data), "the data")
	var snap protocol.NodeBinsSnapshot
	testutil.MustNoErr(r.t, json.Unmarshal(data.Body, &snap), "the snapshot")
	out := map[string]protocol.NodeBinsAssignment{}
	for _, g := range snap.Groups {
		if g.Group != rigK {
			continue
		}
		for _, a := range g.Assignments {
			out[a.Node] = a
		}
	}
	return out
}

// fill is the process standing its parts on a cart.
func (r *kanbanRig) fill(cart int64, part string) {
	r.t.Helper()
	testutil.MustNoErr(r.t, r.db.SetBinManifest(cart, `{"items": []}`, part, 30), "fill the cart")
}

// leave is a side's full off its front to a free window, and its back indexed forward.
// The back's position is left for refill.
func (r *kanbanRig) leave(s rigSide) {
	r.t.Helper()
	full := r.cartAt(s.front)
	if full == 0 {
		r.t.Fatalf("no cart on %s", s.front)
	}
	r.fill(full, s.part)
	r.move(s.front, "PRESS-UNLOAD", full)
	r.fly()
	r.move(s.back, s.front, r.cartAt(s.back))
	r.fly()
}

// refill is a side's empty off its kanban to its back.
func (r *kanbanRig) refill(s rigSide) *orders.Order {
	r.t.Helper()
	return r.order(protocol.OrderRequest{OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true,
		PayloadCode: s.part, SourceNode: rigK, DeliveryNode: s.back, OriginClass: protocol.OriginClassNoDemand})
}

// clear is the CLEAR's door at a window, re-typing as what is lit ("" a plain CLEAR). It
// answers the cart.
func (r *kanbanRig) clear(window, as string) int64 {
	r.t.Helper()
	cart := r.cartAt(window)
	if cart == 0 {
		r.t.Fatalf("no cart on %s to CLEAR", window)
	}
	raw, err := json.Marshal(protocol.BinClearRequest{NodeName: window, BinID: cart, BinTypeCode: as})
	testutil.MustNoErr(r.t, err, "the CLEAR")
	if res := r.doors.AskBinClear(rigStation, raw); res.Status != http.StatusOK {
		r.t.Fatalf("CLEAR at %s refused (%d): %s", window, res.Status, res.Sentence)
	}
	return cart
}

// tap is the operator at a window: the CLEAR, then the empty-out the Edge sends off it
// (Engine.sendCartOut), a move at the unloader's outbound naming no part. The door chooses
// where it lands.
func (r *kanbanRig) tap(window, outbound, as string) *orders.Order {
	r.t.Helper()
	return r.move(window, outbound, r.clear(window, as))
}

// borrow is a cart off the process with K refilled and topped up by its keeper: the cart
// stands on a window, K is whole and K's first source with one to lend is short by it.
func (r *kanbanRig) borrow(s rigSide) {
	r.t.Helper()
	r.leave(s)
	r.refill(s)
	r.fly()
	r.tick()
	r.fly()
}

// standing is the windows with a cart on them.
func (r *kanbanRig) standing() []string {
	r.t.Helper()
	var out []string
	for _, w := range r.windows {
		if r.cartAt(w) != 0 {
			out = append(out, w)
		}
	}
	return out
}

// quiet says nothing is live, nothing parked at a level or anywhere else, and nothing
// went to the plain supermarket.
func (r *kanbanRig) quiet(when string) {
	r.t.Helper()
	if left := r.live(); len(left) != 0 {
		r.t.Errorf("%s: orders left live: %s", when, r.said(left))
	}
	var parked, toSM int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE queue_cause = $1`,
		string(dispatch.CauseNGRPAtLevel)).Scan(&parked), "parked")
	if parked != 0 {
		r.t.Errorf("%s: %d push(es) parked at a level", when, parked)
	}
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders o JOIN nodes n ON n.name = o.delivery_node
		JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id WHERE g.name = $1`, rigSM).Scan(&toSM), "to SM")
	if toSM != 0 {
		r.t.Errorf("%s: %d order(s) delivered into %s", when, toSM, rigSM)
	}
}

// wholeAtRest is the steady picture on KP-SMALL, on the last tick's cards and the
// floor's: K wants and holds small 5, K2 small 2, Z45 small 2, no lineside card held.
func (r *kanbanRig) wholeAtRest(when string) {
	r.t.Helper()
	for _, c := range []struct {
		group string
		want  int
	}{{rigK, 5}, {rigK2, 2}, {rigZ45, 2}} {
		got := r.card(c.group, rigSmall)
		if got.Want != c.want || got.Resident != c.want || got.Gap != 0 {
			r.t.Errorf("%s: %s small = want %d, resident %d, gap %d; want %d of %d", when, c.group, got.Want, got.Resident, got.Gap, c.want, c.want)
		}
	}
	for _, s := range r.eng.KanbanLineStates() {
		if (s.GroupNode == rigK || s.GroupNode == rigK2) && s.HeldBy != "" {
			r.t.Errorf("%s: %s/%s is held by %q", when, s.GroupNode, s.BinTypeCode, s.HeldBy)
		}
	}
}

// TestKanbanRig_R1_SteadyLoop: the press's loop on the rig's plant. Each cycle a pair of
// fulls leaves the positions for the windows and K refills the backs, and the windows'
// carts go back as small. On alternate cycles they find K still short (the door takes
// them into K, and the keeper has nothing to do) and K already topped up from Z45 (the
// door repays Z45). Then the counts on the plant file, after the loop has gone round.
//
// RED with the walk lighting nothing (kanban.AssignAcross): the windows' carts light
// nothing. Checked by mutation.
func TestKanbanRig_R1_SteadyLoop(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)

	for cycle := 1; cycle <= 4; cycle++ {
		when := fmt.Sprintf("cycle %d", cycle)
		keeperFirst := cycle%2 == 0
		for _, s := range rigSides {
			r.leave(s)
		}
		for _, s := range rigSides {
			r.refill(s)
		}
		r.fly()
		if got := r.residents(rigK, rigSmall); got != 3 {
			t.Fatalf("%s: the refills left K %d small, want 3", when, got)
		}

		// Who the windows' carts repay decides the group lit and the group they land in.
		wantGroup, wantLand := "", rigK
		if keeperFirst {
			r.tick()
			r.fly()
			if got := r.residents(rigZ45, rigSmall); got != 0 {
				t.Fatalf("%s: K's keeper left Z45 %d small, want it to have borrowed both", when, got)
			}
			wantGroup, wantLand = rigZ45, rigZ45
		}
		lit := r.lit()
		carts := r.standing()
		if len(carts) != 2 {
			t.Fatalf("%s: %d carts on the windows, want the pair", when, len(carts))
		}
		var pushes []*orders.Order
		for _, w := range carts {
			a, ok := lit[w]
			if !ok || a.NoneShort || a.BinType != rigSmall || a.Group != wantGroup {
				t.Errorf("%s: %s lit %+v (present %v), want small for group %q", when, w, a, ok, wantGroup)
			}
			pushes = append(pushes, r.tap(w, rigK, a.BinType))
		}
		for _, o := range pushes {
			if got := r.groupOf(o.DeliveryNode); got != wantLand {
				t.Errorf("%s: the push off %s took %s in %s, want %s", when, o.SourceNode, o.DeliveryNode, got, wantLand)
			}
		}
		r.fly()
		r.tick()
		r.fly()
		r.tick()
		r.wholeAtRest(when)
		r.quiet(when)
		if len(r.standing()) != 0 {
			t.Errorf("%s: carts left on the windows: %v", when, r.standing())
		}
	}
	for group, n := range map[string]int{rigK: 5, rigZ45: 2, rigK2: 2} {
		if got := r.residents(group, rigSmall); got != n {
			t.Errorf("after the loop %s holds %d small, want %d", group, got, n)
		}
	}
	r.counts()
}

// settled waits out what a bin's move arms and Stop does not stop: the sourceability
// monitor's debounced recompute (engine/sourceability_monitor.go arm, time.AfterFunc at
// sourceabilityDebounceWindow, 300 ms) reads through the counting store, and landed in a
// counted window under a loaded package run. The count holding still for a second is the
// last of it. Nothing in a counted window arms it before the window ends: a tick, a build
// and a door's reads move no bin.
func (r *kanbanRig) settled() {
	r.t.Helper()
	last := int64(-1)
	for i := 0; i < 30; i++ {
		n := r.counter.Count()
		if n == last {
			return
		}
		last = n
		time.Sleep(time.Second)
	}
	r.t.Fatalf("the store did not go quiet in 30 s: %d statements so far", last)
}

// counts asserts the settled numbers on the plant file, from where the loop left it.
func (r *kanbanRig) counts() {
	t := r.t
	t.Helper()

	r.settled()
	r.counter.Reset()
	r.tick()
	t.Logf("STATEMENTS: steady keeper tick: %d", r.counter.Count())
	if n := r.counter.Count(); n != rigKeeperTickSteady {
		t.Errorf("steady keeper tick = %d statements, want %d\n%s", n, rigKeeperTickSteady,
			testutil.StatementBreakdown(r.counter.Statements()))
	}

	r.settled()
	r.counter.Reset()
	r.lit()
	stmts := r.counter.Statements()
	from, to := testutil.FirstBegin(stmts), len(stmts)
	for i := from; i < len(stmts); i++ {
		if strings.Contains(strings.ToLower(stmts[i]), "into outbox") {
			to = i
			break
		}
	}
	t.Logf("STATEMENTS: snapshot build: %d", to-from)
	if to-from != rigSnapshotBuild {
		t.Errorf("snapshot build = %d statements, want %d\n%s", to-from, rigSnapshotBuild,
			testutil.StatementBreakdown(stmts[from:to]))
	}

	// A cart off the process, K topped up from Z45, and the cart re-typed: an empty at K
	// with K whole and Z45 short. The intake door up to the order's birth.
	s := rigSides[0]
	r.borrow(s)
	w := r.standing()[0]
	cart := r.clear(w, rigSmall)
	r.settled()
	r.counter.Reset()
	o, _, lerr := r.eng.Dispatcher().Lifecycle().CreateInboundOrder(rigStation, &protocol.OrderRequest{
		OrderUUID: "rig-door", OrderType: dispatch.OrderTypeMove, Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
		SourceNode: w, DeliveryNode: rigK, BinID: cart}, nil)
	if lerr != nil {
		t.Fatalf("intake refused the push: %s", lerr.Detail)
	}
	stmts = r.counter.Statements()
	door := stmts[:testutil.FirstBegin(stmts)]
	if r.groupOf(o.DeliveryNode) != rigZ45 {
		t.Fatalf("the door sent the push to %s, want %s", o.DeliveryNode, rigZ45)
	}
	t.Logf("STATEMENTS: intake door to a source: %d", len(door))
	if len(door) != rigIntakeDoor {
		t.Errorf("intake door = %d statements, want %d\n%s", len(door), rigIntakeDoor, testutil.StatementBreakdown(door))
	}
	r.fly()

	// The same cart's shape parked: K with nowhere to send it parks a push at its level,
	// and the pass that finds Z45 short again takes it. The arm is the pass to the re-aim.
	r.borrow(s)
	w = r.standing()[0]
	cart = r.clear(w, rigSmall)
	k, err := r.db.GetNodeByName(rigK)
	testutil.MustNoErr(t, err, "K")
	sources, err := r.db.ListKanbanSources(k.ID)
	testutil.MustNoErr(t, err, "K's sources")
	var ids []int64
	for _, src := range sources {
		ids = append(ids, src.SourceNodeID)
	}
	testdb.KanbanOverflow(t, r.db, k.ID, "")
	testdb.KanbanSources(t, r.db, k.ID)
	parked := r.move(w, rigK, cart)
	r.eng.RunFulfillmentScan()
	got, err := r.db.GetOrder(parked.ID)
	testutil.MustNoErr(t, err, "the push")
	if got.Status != protocol.StatusQueued || got.QueueCause != string(dispatch.CauseNGRPAtLevel) {
		t.Fatalf("the push is %s (%s), want parked at K's level", got.Status, got.QueueCause)
	}
	testdb.KanbanSources(t, r.db, k.ID, ids...)
	testdb.KanbanOverflow(t, r.db, k.ID, rigSpare)
	r.settled()
	r.counter.Reset()
	r.eng.RunFulfillmentScan()
	stmts = r.counter.Statements()
	arm := len(stmts)
	for i, st := range stmts {
		if strings.HasPrefix(strings.TrimSpace(st), "UPDATE orders SET delivery_node") {
			arm = i
			break
		}
	}
	got, err = r.db.GetOrder(parked.ID)
	testutil.MustNoErr(t, err, "the push")
	if got.Status != protocol.StatusDispatched || r.groupOf(got.DeliveryNode) != rigZ45 {
		t.Fatalf("the push is %s to %s (%s), want dispatched into %s", got.Status, got.DeliveryNode, got.QueueCause, rigZ45)
	}
	t.Logf("STATEMENTS: parked arm to the re-aim: %d, the pass %d", arm, len(stmts))
	if arm != rigParkedArm {
		t.Errorf("parked arm = %d statements, want %d\n%s", arm, rigParkedArm, testutil.StatementBreakdown(stmts[:arm]))
	}
}

// TestKanbanRig_R5_Repayment: repayment by count. K borrows from Z45, and K's next cart,
// K whole, lights small for Z45 and goes there, not to SM. K2 borrows from Z45, and K's
// next cart repays Z45 for it. K over-handed from SM: nothing is short, the next cart
// says so on its line, and it goes to K's overflow, the spare zone.
//
// RED with the chooser answering K always (plan.Planner.ChoosePush): the repaying pushes
// park at K or land in it. Checked by mutation.
func TestKanbanRig_R5_Repayment(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	s := rigSides[0]

	// repays puts the one cart on the windows through the tap and says it landed in Z45.
	repays := func(step string, want string) {
		t.Helper()
		carts := r.standing()
		if len(carts) != 1 {
			t.Fatalf("%s: %d carts on the windows, want one", step, len(carts))
		}
		a := r.lit()[carts[0]]
		if a.NoneShort || a.BinType != rigSmall || a.Group != rigZ45 {
			t.Errorf("%s: the cart lit %+v, want small for %s", step, a, rigZ45)
		}
		o := r.tap(carts[0], rigK, a.BinType)
		if got := r.groupOf(o.DeliveryNode); got != want {
			t.Errorf("%s: the push took %s in %s, want %s", step, o.DeliveryNode, got, want)
		}
		r.fly()
		if got := r.residents(rigZ45, rigSmall); got != 2 {
			t.Errorf("%s: Z45 holds %d small after the push, want 2", step, got)
		}
	}

	// K borrows one small from Z45: its refill leaves it short and the keeper tops it up.
	r.borrow(s)
	if k, z := r.residents(rigK, rigSmall), r.residents(rigZ45, rigSmall); k != 5 || z != 1 {
		t.Fatalf("K borrowed: K holds %d small and Z45 %d, want 5 and 1", k, z)
	}
	repays("K's loan", rigZ45)
	r.tick()
	r.fly()
	r.tick()
	r.wholeAtRest("K's loan repaid")
	r.quiet("K's loan repaid")

	// K2 borrows: the weld's full stays on its own window, K2's refill leaves it short, and
	// its keeper tops it up from Z45.
	weldCart := r.cartAt("ALN_021")
	r.fill(weldCart, "KW-PART")
	r.move("ALN_021", "WELD-UNLOAD", weldCart)
	r.fly()
	r.order(protocol.OrderRequest{OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, PayloadCode: "KW-PART",
		SourceNode: rigK2, DeliveryNode: "ALN_021", OriginClass: protocol.OriginClassNoDemand})
	r.fly()
	r.tick()
	r.fly()
	if k2, z := r.residents(rigK2, rigSmall), r.residents(rigZ45, rigSmall); k2 != 2 || z != 1 {
		t.Fatalf("K2 borrowed: K2 holds %d small and Z45 %d, want 2 and 1", k2, z)
	}

	// K's next cart off the process, K whole: it repays what K2 borrowed.
	r.leave(s)
	repays("K2's loan, by K's cart", rigZ45)
	if got := r.residents(rigK, rigSmall); got != 5 {
		t.Errorf("K's cart repaid K2's loan, and K holds %d small, want 5", got)
	}
	r.refill(s)
	r.fly()
	r.tick()
	r.fly()
	// The refill borrowed one more; the weld's cart, on K2's line, repays that.
	weldWindow := ""
	for _, w := range []string{"ULN_031", "ULN_032"} {
		if r.cartAt(w) != 0 {
			weldWindow = w
		}
	}
	if weldWindow == "" {
		t.Fatal("the weld's full left its window")
	}
	o := r.tap(weldWindow, rigK2, rigSmall)
	if got := r.groupOf(o.DeliveryNode); got != rigZ45 {
		t.Errorf("the weld's cart took %s in %s, want %s: K2 whole and Z45 short", o.DeliveryNode, got, rigZ45)
	}
	r.fly()
	r.tick()
	r.fly()
	r.tick()
	r.wholeAtRest("K2's loan repaid")
	r.quiet("K2's loan repaid")

	// K over-handed from SM, a person's move onto its free position. SM holds no level,
	// so nothing can repay it: K and Z45 whole, the next cart lights nothing and says so,
	// and K's overflow takes it.
	res, err := r.eng.CreateBinMove(engine.BinMoveRequest{Selection: engine.BinSelectionByLabel, BinLabel: "KB-SM-S1",
		DestNodeName: "PKB_008", StationID: "rig-page"})
	testutil.MustNoErr(t, err, "a person's move from SM")
	if res == nil {
		t.Fatal("no move from SM")
	}
	r.fly()
	if got := r.residents(rigK, rigSmall); got != 6 {
		t.Fatalf("K over-handed holds %d small, want 6", got)
	}
	r.leave(s)
	carts := r.standing()
	if len(carts) != 1 {
		t.Fatalf("%d carts on the windows, want one", len(carts))
	}
	if a := r.lit()[carts[0]]; !a.NoneShort || a.BinType != "" {
		t.Errorf("the cart with nothing short lit %+v, want its line to say none short", a)
	}
	push := r.tap(carts[0], rigK, "")
	if got := r.groupOf(push.DeliveryNode); got != rigSpare {
		t.Errorf("the push took %s in %s, want K's overflow %s", push.DeliveryNode, got, rigSpare)
	}
	r.fly()
	if got := r.residents(rigSpare, rigSmall); got != 1 {
		t.Errorf("%s holds %d small, want the one cart", rigSpare, got)
	}
	r.quiet("the overflow's cart")
}
