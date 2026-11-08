//go:build docker

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
	"shingocore/store/bins"
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
	rigSM      = "KP-SM"
	rigSmall   = "SMALL-45"
	rigBig     = "BIG-45"
	rigTotes   = "TOTES-45"
	rig32      = "CART-32"

	// The settled counts on the plant the runs use, after the loop has gone round. TestKanbanCost_*
	// assert the like in engine on the cost rig's shape.
	rigKeeperTickSteady = 3
	rigSnapshotBuild    = 9
	rigIntakeDoor       = 8
	rigParkedArm        = 8
	// rigClearStatements is the CLEAR door measured at ec95f722 on this plant.
	rigClearStatements = 22
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
	r := newKanbanRigOf(t, kanbanPressPlant(t), []string{"ULN_021", "ULN_022", "ULN_023", "ULN_024"})
	r.atRest("as seeded")
	return r
}

// newKanbanRigOf seeds a rig's plant, on the windows its unloader has.
func newKanbanRigOf(t *testing.T, plant *plantspec.Plant, windows []string) *kanbanRig {
	t.Helper()
	db, dbcfg := testdb.OpenWithConfig(t)
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
	digest, err := protocol.BinWatchDigest(windows)
	testutil.MustNoErr(t, err, "watch digest")
	r := &kanbanRig{t: t, db: db, eng: eng, sim: sim, counter: counter, doors: www.NewDoors(eng), feed: feed,
		windows: windows, digest: digest}
	r.tick()
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
	// By hand, not a move: this file's plain moves stand in for the legs, and a plain move
	// naming an empty off a line position is diverted to a kanban short of it
	// (dispatch.dropByContents). K is short for as long as the press runs. The index leg
	// is not a move naming that cart, and the stack runs it.
	r.byHand(r.cartAt(s.back), s.front)
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

// quiet says nothing is live, nothing parked at a level or anywhere else, nothing went to
// the plain supermarket, and every cart in K stands on a slot its type is allowed.
func (r *kanbanRig) quiet(when string) {
	r.t.Helper()
	if left := r.live(); len(left) != 0 {
		r.t.Errorf("%s: orders left live: %s", when, r.said(left))
	}
	var parked int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE queue_cause = $1`,
		string(dispatch.CauseNGRPAtLevel)).Scan(&parked), "parked")
	if parked != 0 {
		r.t.Errorf("%s: %d push(es) parked at a level", when, parked)
	}
	r.nothingToSM(when)
	r.slotsKeepTheirTypes(when)
}

func (r *kanbanRig) nothingToSM(when string) {
	r.t.Helper()
	var toSM int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders o JOIN nodes n ON n.name = o.delivery_node
		JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id WHERE g.name = $1`, rigSM).Scan(&toSM), "to SM")
	if toSM != 0 {
		r.t.Errorf("%s: %d order(s) delivered into %s", when, toSM, rigSM)
	}
}

func (r *kanbanRig) slotsKeepTheirTypes(when string) {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT n.name, b.label, t.code FROM bins b JOIN nodes n ON n.id = b.node_id
		JOIN nodes g ON g.id = n.parent_id JOIN bin_types t ON t.id = b.bin_type_id WHERE g.name = $1`, rigK)
	testutil.MustNoErr(r.t, err, "K's carts")
	defer rows.Close()
	table := rigSlotTypes()
	for rows.Next() {
		var slot, label, code string
		testutil.MustNoErr(r.t, rows.Scan(&slot, &label, &code), "scan")
		if !slices.Contains(table[slot], code) {
			r.t.Errorf("%s: %s, a %s, stands on %s, which allows %v", when, label, code, slot, table[slot])
		}
	}
	testutil.MustNoErr(r.t, rows.Err(), "rows")
}

// line is one line of a kanban's card as a test expects it.
type rigLine struct {
	group, binType      string
	want, resident, gap int
	held                string
}

func (r *kanbanRig) cardReads(when string, lines ...rigLine) {
	r.t.Helper()
	for _, l := range lines {
		got := r.card(l.group, l.binType)
		if got.Want != l.want || got.Resident != l.resident || got.Gap != l.gap || got.HeldBy != l.held {
			r.t.Errorf("%s: %s %s = want %d, resident %d, gap %d, held %q; expected want %d, resident %d, gap %d, held %q",
				when, l.group, l.binType, got.Want, got.Resident, got.Gap, got.HeldBy, l.want, l.resident, l.gap, l.held)
		}
	}
}

// atRest is the steady picture on KP-SMALL, on the last tick's cards. K's small wants 6, 2 in K
// and 4 held on the positions, short of nothing. K's 32x30 stand 4 over their want in their own
// slots.
func (r *kanbanRig) atRest(when string) {
	r.t.Helper()
	r.cardReads(when,
		rigLine{rigK, rigSmall, 6, 2, 0, ""},
		rigLine{rigK, rigBig, 2, 2, 0, ""},
		rigLine{rigK, rigTotes, 2, 2, 0, ""},
		rigLine{rigK, rig32, 2, 6, -4, ""},
		rigLine{rigK2, rigSmall, 3, 2, 0, ""},
	)
}

// TestKanbanRig_R1_SteadyLoop: the press's loop on the rig's plant. Each cycle a pair of fulls
// leaves the positions for the windows and K refills the backs; K then holds none of the running
// type, the windows' carts are lit small for K and land on a slot that allows them, and K is
// back at its 2 with nothing parked. Then the counts on the plant file,
// after the loop has gone round.
//
// RED with the walk lighting nothing (kanban.AssignAcross): the windows' carts light
// nothing. Checked by mutation.
func TestKanbanRig_R1_SteadyLoop(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)

	for cycle := 1; cycle <= 4; cycle++ {
		when := fmt.Sprintf("cycle %d", cycle)
		for _, s := range rigSides {
			r.leave(s)
		}
		for _, s := range rigSides {
			r.refill(s)
		}
		r.fly()
		if got := r.residents(rigK, rigSmall); got != 0 {
			t.Fatalf("%s: the refills left K %d small, want 0", when, got)
		}
		// A keeper tick here asks nothing: the family is whole, and the pair at the windows is
		// what small is short of.
		r.tick()
		r.fly()
		r.cardReads(when+", the pair at the windows", rigLine{rigK, rigSmall, 6, 0, 0, "2 at the unloader's windows coming back as " + rigSmall})

		lit := r.lit()
		carts := r.standing()
		if len(carts) != 2 {
			t.Fatalf("%s: %d carts on the windows, want the pair", when, len(carts))
		}
		var pushes []*orders.Order
		for _, w := range carts {
			a, ok := lit[w]
			if !ok || a.NoneShort || a.BinType != rigSmall || a.Group != "" {
				t.Errorf("%s: %s lit %+v (present %v), want small for K itself", when, w, a, ok)
			}
			pushes = append(pushes, r.tap(w, rigK, a.BinType))
		}
		for _, o := range pushes {
			if got := r.groupOf(o.DeliveryNode); got != rigK {
				t.Errorf("%s: the push off %s took %s in %s, want K", when, o.SourceNode, o.DeliveryNode, got)
			}
		}
		r.fly()
		r.tick()
		r.fly()
		r.tick()
		r.atRest(when)
		r.quiet(when)
		if len(r.standing()) != 0 {
			t.Errorf("%s: carts left on the windows: %v", when, r.standing())
		}
	}
	for group, n := range map[string]int{rigK: 2, rigK2: 2} {
		if got := r.residents(group, rigSmall); got != n {
			t.Errorf("after the loop %s holds %d small, want %d", group, got, n)
		}
	}
	r.counts()
}

// settled waits out what a bin's move arms and Stop does not stop: the sourceability
// monitor's debounced recompute (engine/sourceability_monitor.go arm, time.AfterFunc at
// engine.SourceabilityDebounceWindow) reads through the counting store, and landed in a
// counted window under a loaded package run. The count holding still for twice that window
// is the last of it: the arm's own statements moved the count, so a still count that long
// means the timer fired a full window ago and its first read would have moved it since.
// Polled every 50 ms: the stillness is the wait, not the poll (a 1 s poll made every call
// cost at least a second). Nothing in a counted window arms it before the window ends: a
// tick, a build and a door's reads move no bin.
func (r *kanbanRig) settled() {
	r.t.Helper()
	quiet := 2 * engine.SourceabilityDebounceWindow
	deadline := time.Now().Add(30 * time.Second)
	last, since := r.counter.Count(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if n := r.counter.Count(); n != last {
			last, since = n, time.Now()
			continue
		}
		if time.Since(since) >= quiet {
			return
		}
	}
	r.t.Fatalf("the store did not go quiet in 30 s: %d statements so far", last)
}

// countTick is one keeper tick's statements.
func (r *kanbanRig) countTick() []string {
	r.t.Helper()
	r.settled()
	r.counter.Reset()
	r.tick()
	return r.counter.Statements()
}

// countBuild is one snapshot build's statements, up to its row on the outbox.
func (r *kanbanRig) countBuild() []string {
	r.t.Helper()
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
	return stmts[from:to]
}

// countDoor is the intake door's statements for a cart at a window going to K, up to the
// order's birth, and the order it left.
func (r *kanbanRig) countDoor(window string, cart int64) ([]string, *orders.Order) {
	r.t.Helper()
	r.settled()
	r.next++
	r.counter.Reset()
	o, _, lerr := r.eng.Dispatcher().Lifecycle().CreateInboundOrder(rigStation, &protocol.OrderRequest{
		OrderUUID: fmt.Sprintf("rig-door-%d", r.next), OrderType: dispatch.OrderTypeMove, Quantity: 1,
		OriginClass: protocol.OriginClassNoDemand, SourceNode: window, DeliveryNode: rigK, BinID: cart}, nil)
	if lerr != nil {
		r.t.Fatalf("intake refused the push: %s", lerr.Detail)
	}
	stmts := r.counter.Statements()
	return stmts[:testutil.FirstBegin(stmts)], o
}

// counts asserts the settled numbers on the plant file, from where the loop left it.
func (r *kanbanRig) counts() {
	t := r.t
	t.Helper()

	stmts := r.countTick()
	t.Logf("STATEMENTS: keeper tick, nothing short: %d", len(stmts))
	if len(stmts) != rigKeeperTickSteady {
		t.Errorf("keeper tick = %d statements, want %d\n%s", len(stmts), rigKeeperTickSteady, testutil.StatementBreakdown(stmts))
	}

	stmts = r.countBuild()
	t.Logf("STATEMENTS: snapshot build: %d", len(stmts))
	if len(stmts) != rigSnapshotBuild {
		t.Errorf("snapshot build = %d statements, want %d\n%s", len(stmts), rigSnapshotBuild, testutil.StatementBreakdown(stmts))
	}

	// A cart off the process: the intake door up to the order's birth.
	s := rigSides[0]
	r.leave(s)
	w := r.standing()[0]
	door, o := r.countDoor(w, r.clear(w, rigSmall))
	if r.groupOf(o.DeliveryNode) != rigK {
		t.Fatalf("the door sent the push to %s, want K", o.DeliveryNode)
	}
	t.Logf("STATEMENTS: intake door into K: %d", len(door))
	if len(door) != rigIntakeDoor {
		t.Errorf("intake door = %d statements, want %d\n%s", len(door), rigIntakeDoor, testutil.StatementBreakdown(door))
	}
	r.fly()
	r.refill(s)
	r.fly()

	// Every slot admitting the family taken: a return parks, visible, as a full group's does.
	// Standing a cart elsewhere frees one, and the pass that finds it so takes the push: the arm
	// is the pass to the re-aim.
	r.fillK()
	r.leave(s)
	w = r.standing()[0]
	parked := r.move(w, rigK, r.clear(w, rigSmall))
	r.eng.RunFulfillmentScan()
	got, err := r.db.GetOrder(parked.ID)
	testutil.MustNoErr(t, err, "the push")
	if got.Status != protocol.StatusQueued || got.QueueCause != string(dispatch.CauseNGRPFull) {
		t.Fatalf("the push is %s (%s), want parked with no slot", got.Status, got.QueueCause)
	}
	r.byHand(r.oneOf(rigK, rigSmall), s.back)
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
	if got.Status != protocol.StatusDispatched || r.groupOf(got.DeliveryNode) != rigK {
		t.Fatalf("the push is %s to %s (%s), want dispatched into K", got.Status, got.DeliveryNode, got.QueueCause)
	}
	t.Logf("STATEMENTS: parked arm to the re-aim: %d, the pass %d", arm, len(stmts))
	if arm != rigParkedArm {
		t.Errorf("parked arm = %d statements, want %d\n%s", arm, rigParkedArm, testutil.StatementBreakdown(stmts[:arm]))
	}
}

// fillK stands a hand cart on every slot of K admitting the family that holds none.
func (r *kanbanRig) fillK() {
	r.t.Helper()
	for i := 1; i <= 10; i++ {
		if slot := fmt.Sprintf("PKB_%03d", i); r.cartAt(slot) == 0 {
			r.handCarts(rigSmall, slot)
		}
	}
}

// handCarts is a person registering carts of a type on free slots.
func (r *kanbanRig) handCarts(binType string, slots ...string) {
	r.t.Helper()
	bt, err := r.db.GetBinTypeByCode(binType)
	testutil.MustNoErr(r.t, err, "type "+binType)
	for _, slot := range slots {
		n, err := r.db.GetNodeByName(slot)
		testutil.MustNoErr(r.t, err, "slot "+slot)
		id := n.ID
		testutil.MustNoErr(r.t, r.db.CreateBin(&bins.Bin{BinTypeID: bt.ID, Label: "KB-HAND-" + slot, NodeID: &id, Status: "available"}), "hand "+slot)
	}
}

// oneOf is the id of one cart of a type standing in a group.
func (r *kanbanRig) oneOf(group, binType string) int64 {
	r.t.Helper()
	var id int64
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT b.id FROM bins b JOIN nodes n ON n.id = b.node_id
		JOIN nodes g ON g.id = n.parent_id JOIN bin_types t ON t.id = b.bin_type_id
		WHERE g.name = $1 AND t.code = $2 ORDER BY b.id LIMIT 1`, group, binType).Scan(&id), "a "+binType+" in "+group)
	return id
}

// byHand is a person carrying a cart: no order, as the bin page's move.
func (r *kanbanRig) byHand(cart int64, to string) {
	r.t.Helper()
	n, err := r.db.GetNodeByName(to)
	testutil.MustNoErr(r.t, err, "node "+to)
	testutil.MustNoErr(r.t, r.db.MoveBinClearingStaging(cart, n.ID, false), "carry to "+to)
}

// TestKanbanRig_R5_SMIsNotRepaid: a cart leaves the loop by hand. K's family is one short, so its
// keeper takes one from SM, a plain group, by its plant-wide search (it names no source), one ask
// for the one cart it lost, and no order ever delivers into SM.
func TestKanbanRig_R5_SMIsNotRepaid(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.byHand(r.cartAt("PKB_001"), "KSM_001")
	r.tick()
	if got := r.card(rigK, rigSmall); got.Want != 6 || got.Resident != 1 || got.Created != 1 {
		t.Fatalf("K with one small out = %+v, want one ask", got)
	}
	r.fly()
	var fromSM int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE source_node LIKE 'KSM_%' AND status = $1`,
		protocol.StatusConfirmed).Scan(&fromSM), "from SM")
	if fromSM != 1 {
		t.Errorf("%d orders brought a cart off SM, want 1: %s", fromSM, r.said(r.live()))
	}
	if got := r.residents(rigK, rigSmall); got != 2 {
		t.Errorf("K holds %d small, want its 2", got)
	}
	r.nothingToSM("K took from SM")
	r.slotsKeepTheirTypes("K took from SM")
	if left := r.live(); len(left) != 0 {
		t.Errorf("asks left past the one cart: %s; K's card: %s", r.said(left), r.cardsSaid())
	}
	r.tick()
	if got := r.card(rigK, rigSmall); got.OriginID != "" || got.Gap > 0 {
		t.Errorf("K whole again reads %+v, want its episode closed", got)
	}

	// The next cart off the process is lit for K and goes to K: SM is short of nothing.
	s := rigSides[0]
	r.leave(s)
	carts := r.standing()
	if len(carts) != 1 {
		t.Fatalf("%d carts on the windows, want one", len(carts))
	}
	a := r.lit()[carts[0]]
	if a.NoneShort || a.BinType != rigSmall || a.Group != "" {
		t.Errorf("the cart lit %+v, want small for K", a)
	}
	o := r.tap(carts[0], rigK, a.BinType)
	if got := r.groupOf(o.DeliveryNode); got != rigK {
		t.Errorf("the push took %s in %s, want K", o.DeliveryNode, got)
	}
	r.fly()
	r.nothingToSM("the next cart")
}

// TestKanbanRig_R5_Repayment: with no source, repayment is K itself. K over-handed until every
// slot admitting the family stands taken: the next cart off the process lights nothing and says
// so (NoneShort), and it parks named at K as a full group's does. A slot freed, it lands.
func TestKanbanRig_R5_Repayment(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	s := rigSides[0]

	r.handCarts(rigSmall, "PKB_007", "PKB_008", "PKB_009", "PKB_010")
	r.tick()
	r.cardReads("K over-handed", rigLine{rigK, rigSmall, 6, 6, -4, ""})

	r.leave(s)
	carts := r.standing()
	if len(carts) != 1 {
		t.Fatalf("%d carts on the windows, want one", len(carts))
	}
	if a := r.lit()[carts[0]]; !a.NoneShort || a.BinType != "" {
		t.Errorf("the cart with nothing short lit %+v, want its line to say none short", a)
	}
	push := r.tap(carts[0], rigK, "")
	if push.Status != protocol.StatusQueued || push.QueueCause != string(dispatch.CauseNGRPFull) || push.DeliveryNode != rigK {
		t.Errorf("the push is %s to %s (%s), want parked named at K, no slot", push.Status, push.DeliveryNode, push.QueueCause)
	}
	r.eng.RunFulfillmentScan()
	again, err := r.db.GetOrder(push.ID)
	testutil.MustNoErr(t, err, "the push")
	if again.Status != protocol.StatusQueued {
		t.Errorf("a pass took the push with no slot: %s to %s", again.Status, again.DeliveryNode)
	}
	if got := len(r.standing()); got != 1 {
		t.Errorf("%d carts on the windows, want the parked one", got)
	}

	// The refill leaves a slot free: the parked push is taken on the next pass.
	r.refill(s)
	r.fly()
	again, err = r.db.GetOrder(push.ID)
	testutil.MustNoErr(t, err, "the push")
	if again.Status != protocol.StatusConfirmed || r.groupOf(again.DeliveryNode) != rigK {
		t.Errorf("the push is %s to %s (%s), want it landed in K", again.Status, again.DeliveryNode, again.QueueCause)
	}
	if got := r.residents(rigK, rigSmall); got != 6 {
		t.Errorf("K holds %d small after the push, want 6", got)
	}
	if got := r.standing(); len(got) != 0 {
		t.Errorf("carts left on the windows: %v", got)
	}
	r.nothingToSM("the parked cart")
	r.slotsKeepTheirTypes("the parked cart")
}

// openChangeover is the episode the Edge upserts at START (engine/demand_episode.go), the
// target's first produce claim naming the style.
func (r *kanbanRig) openChangeover(part string) string {
	r.t.Helper()
	r.next++
	id := fmt.Sprintf("c0c0c0c0-0000-4000-8000-%012d", r.next)
	testutil.MustNoErr(r.t, r.db.UpsertDemandOrigin(store.DemandOrigin{OriginID: id, Revision: 1, EpisodeKey: "co|" + id,
		Kind: protocol.EpisodeKindChangeover, StationID: rigStation, ProcessID: "KP-PRESS", CoreNodeName: "PLN_021",
		PayloadCode: part, OpenedAt: time.Now().UTC()}), "open the changeover")
	return id
}

// TestKanbanRig_WalkCounts holds the statements of a keeper tick, a snapshot build and the
// intake door to their budgets on the fixture in the four states of the walk. Each state's first tick
// pays once for what the state changed (an episode opened or closed), so both are logged.
//
// THE STATES ARE SET, NOT DRIVEN: carts are landed by hand as the walk would have left them
// and the episode upserted as the Edge sends it. On KP-32 the 4 45x48 off the positions stand
// on the windows, because K is whole of them, no source, no overflow.
func TestKanbanRig_WalkCounts(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	log := func(state string, first, settled, build, door []string, o *orders.Order) {
		t.Helper()
		t.Logf("WALK %s: tick first %d, settled %d; build %d; door %d (%s to %s, %q)", state, len(first), len(settled),
			len(build), len(door), o.Status, o.DeliveryNode, o.QueueCause)
		if len(first) > 4 {
			t.Logf("WALK %s: the first tick\n%s", state, testutil.StatementBreakdown(first))
		}
		if len(settled) > 4 {
			t.Logf("WALK %s: the settled tick\n%s", state, testutil.StatementBreakdown(settled))
		}
		t.Logf("WALK %s: cards %s", state, r.cardsSaid())
		if len(settled) > 4 || len(build) > 10 || len(door) > 8 {
			t.Errorf("WALK %s: over budget: tick %d of 4, build %d of 10, door %d of 8\n%s", state, len(settled), len(build), len(door), testutil.StatementBreakdown(door))
		}
	}
	s := rigSides[0]

	// Steady KP-SMALL: K short of small, a cart off the process going to K.
	first, settled := r.countTick(), r.countTick()
	build := r.countBuild()
	r.leave(s)
	w := r.standing()[0]
	door, o := r.countDoor(w, r.clear(w, rigSmall))
	log("steady KP-SMALL", first, settled, build, door, o)
	r.fly()
	r.refill(s)
	r.fly()

	// Small -> 32, open: K's cover has moved. Nothing is short: 32x30 want 6, held; small want 2.
	r.openChangeover("KP-32")
	first, settled = r.countTick(), r.countTick()
	build = r.countBuild()
	r.leave(s)
	w = r.standing()[0]
	door, o = r.countDoor(w, r.clear(w, ""))
	log("changeover small -> 32", first, settled, build, door, o)

	// Steady KP-32: the mirror says so, 4 32x30 on the positions and K short of them, 4 45x48
	// standing on the windows. The cart the door above parked is one of the 4.
	r.closeOrder(o.ID)
	r.closeChangeovers()
	plant := kanbanPressPlant(t)
	for i := range plant.Processes {
		if plant.Processes[i].Name == "KP-PRESS" {
			plant.Processes[i].ActiveStyle = "KP-32"
		}
	}
	seedClaimsMirror(t, r.db, rigClaimedPlant(plant))
	for _, pos := range []string{"PLN_021", "PLN_022", "PLN_023", "PLN_024"} {
		if c := r.cartAt(pos); c != 0 {
			r.byHand(c, firstFreeWindow(r))
		}
	}
	for i, pos := range []string{"PLN_021", "PLN_022", "PLN_023", "PLN_024"} {
		r.byHand(r.cartAt(fmt.Sprintf("PKB_%03d", 11+i)), pos)
	}
	first, settled = r.countTick(), r.countTick()
	build = r.countBuild()
	// The door: one 32x30 off the process, K short of them. It needs a window, and all 4
	// are held: free one by hand, as the door is what is being counted.
	held := r.standing()
	spare := r.cartAt(held[0])
	r.byHand(spare, "KSM_001")
	r.byHand(r.cartAt("PLN_021"), held[0])
	door, o = r.countDoor(held[0], r.clear(held[0], ""))
	log("steady KP-32", first, settled, build, door, o)
	r.fly()
	r.byHand(spare, firstFreeWindow(r))

	// 32 -> big, open: K short of big, 45x48 still on the windows. A small cart's door: K is
	// short of big and whole of small, and the door asks by the cart's own type.
	r.openChangeover("KP-BIG")
	first, settled = r.countTick(), r.countTick()
	build = r.countBuild()
	held = r.standing()
	door, o = r.countDoor(held[0], r.clear(held[0], ""))
	log("changeover 32 -> big", first, settled, build, door, o)
}

func firstFreeWindow(r *kanbanRig) string {
	r.t.Helper()
	for _, w := range r.windows {
		if r.cartAt(w) == 0 {
			return w
		}
	}
	r.t.Fatal("no free window")
	return ""
}

// closeOrder and closeChangeovers set a walk state's paper straight by hand.
func (r *kanbanRig) closeOrder(id int64) {
	r.t.Helper()
	_, err := r.db.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, id)
	testutil.MustNoErr(r.t, err, "close the order")
}

func (r *kanbanRig) closeChangeovers() {
	r.t.Helper()
	_, err := r.db.Exec(`UPDATE demand_origins SET closed_at = now() WHERE kind = $1 AND closed_at IS NULL`, protocol.EpisodeKindChangeover)
	testutil.MustNoErr(r.t, err, "close the changeover")
}

// cardsSaid is K's lines of the last tick, for the log.
func (r *kanbanRig) cardsSaid() string {
	var out []string
	for _, s := range r.eng.KanbanLineStates() {
		if s.GroupNode == rigK {
			out = append(out, fmt.Sprintf("%s want %d resident %d gap %d held %q surplus %q", s.BinTypeCode, s.Want, s.Resident, s.Gap, s.HeldBy, s.SurplusCause))
		}
	}
	return strings.Join(out, "; ")
}

// TestKanbanRig_FivePressSteady: plants/kanban-press5.yaml on its one kanban: steady, each press
// lets a pair go and takes a pair back, the family stays whole, and the counts are the one-press
// counts whatever the presses.
func TestKanbanRig_FivePressSteady(t *testing.T) {
	t.Parallel()
	windows := []string{"ULN_501", "ULN_502", "ULN_503", "ULN_504", "ULN_505", "ULN_506", "ULN_507", "ULN_508", "ULN_509", "ULN_510"}
	plant, err := plantspec.Load("../../../plants/kanban-press5.yaml")
	testutil.MustNoErr(t, err, "the plant file")
	r := newKanbanRigOf(t, plant, windows)
	r.settled()
	const k = "P5-KANBAN"
	line := func() engine.KanbanLineState { return r.card(k, rigSmall) }
	if got := line(); got.Want != 22 || got.Resident != 2 || got.Gap != 0 || got.Held != 20 {
		t.Fatalf("steady: %+v, want 22 wanted, 2 standing, 20 on the line, short 0", got)
	}
	if stmts := r.countTick(); len(stmts) > 3 {
		t.Errorf("tick = %d statements on five presses\n%s", len(stmts), testutil.StatementBreakdown(stmts))
	}
	var asked int
	testutil.MustNoErr(t, r.db.QueryRow(`SELECT count(*) FROM orders`).Scan(&asked), "orders")
	if asked != 0 {
		t.Errorf("%d orders with every family whole: %s", asked, r.said(r.live()))
	}
	families := r.eng.KanbanFamilyStates()
	for _, f := range families {
		if f.GroupNode == k && f.Found != f.Pool {
			t.Errorf("%s: found %d of its pool %d", f.Family, f.Found, f.Pool)
		}
	}
}
