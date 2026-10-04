package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// ONE ANSWER PER LINE STATE. Every request button of both roles asks the same
// questions of a line before it plans anything (guardLineRequest), so for each
// state a line can be in, the material request on a consume line and the
// produce request and the empty-bin request on a produce line give the same
// answer: the same refusal in the same words, or the same orders with the same
// number of Core round trips. Material handling is circular: only what the bin
// carries differs, a full to a consume line and an empty to a produce line.

// buttonWorld is one line, Core answering node-bins from occ, with every call
// to Core counted.
type buttonWorld struct {
	t     *testing.T
	eng   *Engine
	db    *store.DB
	role  protocol.ClaimRole
	mode  protocol.SwapMode
	node  int64
	mu    sync.Mutex
	occ   map[string]bool
	calls atomic.Int32
}

// buttonAnswer is what one press of a button did: refused with a sentence, or
// made orders, and the Core round trips it took.
type buttonAnswer struct {
	refused string
	legs    int // swap legs
	toLine  int // plain orders bound for the line
	toDeck  int // plain orders bound for a press's paired position
	trips   int // Core round trips
}

func (w *buttonWorld) occupied(names ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		w.occ[n] = true
	}
}

func (w *buttonWorld) bare(names ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		w.occ[n] = false
	}
}

// serveCore points the engine at a Core stub that answers node-bins from occ.
func (w *buttonWorld) serveCore() {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls.Add(1)
		out := []NodeBinInfo{}
		if r.URL.Path == "/api/telemetry/node-bins" {
			w.mu.Lock()
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n != "" {
					out = append(out, NodeBinInfo{NodeName: n, Occupied: w.occ[n]})
				}
			}
			w.mu.Unlock()
		}
		_ = json.NewEncoder(rw).Encode(out)
	}))
	w.t.Cleanup(srv.Close)
	w.eng.coreClient = NewCoreClient(srv.URL)
}

// newButtonWorld is a census line of the role and mode, its line and a press's
// paired position holding a bin.
func newButtonWorld(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode) *buttonWorld {
	t.Helper()
	eng, db, nodeID := seedCensusPress(t, role, mode, 30, false)
	eng.wireEventHandlers()
	w := &buttonWorld{t: t, eng: eng, db: db, role: role, mode: mode, node: nodeID, occ: map[string]bool{}}
	w.occupied(ksLine, censusDeck)
	w.serveCore()
	return w
}

// request is the role's own request button, used to put the line in a state.
func (w *buttonWorld) request() {
	w.t.Helper()
	var err error
	if w.role == protocol.ClaimRoleProduce {
		_, err = w.eng.RequestProduceSwap(w.node)
	} else {
		_, err = w.eng.RequestNodeMaterial(w.node, 1)
	}
	testutil.MustNoErr(w.t, err, "the request that sets the state")
}

// lastOrder is the newest order of any process node.
func (w *buttonWorld) lastOrder() int64 {
	w.t.Helper()
	var id int64
	testutil.MustNoErr(w.t, w.db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM orders`).Scan(&id), "last order")
	return id
}

func (w *buttonWorld) setStatus(id int64, s protocol.Status) {
	w.t.Helper()
	testutil.MustNoErr(w.t, w.db.UpdateOrderStatus(id, string(s)), "order status")
}

// press presses the door's button once and reads what it did.
func (w *buttonWorld) press(door string) buttonAnswer {
	w.t.Helper()
	before := w.lastOrder()
	w.calls.Store(0)
	var err error
	switch door {
	case doorMaterial:
		_, err = w.eng.RequestNodeMaterial(w.node, 1)
	case doorProduce:
		_, err = w.eng.RequestProduceSwap(w.node)
	case doorEmptyBin:
		_, err = w.eng.RequestEmptyBin(w.node, ksPart)
	}
	got := buttonAnswer{trips: int(w.calls.Load())}
	if err != nil {
		got.refused = err.Error()
	}
	all, lerr := w.db.ListOrders()
	testutil.MustNoErr(w.t, lerr, "orders")
	for _, o := range all {
		switch {
		case o.ID <= before:
		case o.OrderType == orders.TypeComplex:
			got.legs++
		case o.DeliveryNode == censusDeck:
			got.toDeck++
		default:
			got.toLine++
		}
	}
	// The one difference left between the roles: a consume request asks Core
	// whether the part has any stock before it arms a pair, and a produce
	// request cannot ask that about empties (guardSourceKnownDry).
	if door == doorMaterial && got.legs == 2 {
		got.trips--
	}
	return got
}

// sibling is a process node of another process on the line's own core node,
// with the same claim: a second station sharing one physical position. Its
// order, a bin on its way to the line, is returned.
func (w *buttonWorld) sibling() int64 {
	w.t.Helper()
	db := w.db
	proc, err := db.CreateProcess("SIBLING-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(w.t, err, "sibling process")
	sib, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: proc, CoreNodeName: ksLine, Code: "SIB", Name: ksLine + "-2", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(w.t, err, "sibling node")
	style, err := db.CreateStyle("SIBLING-STYLE", "", proc)
	testutil.MustNoErr(w.t, err, "sibling style")
	testutil.MustNoErr(w.t, db.SetActiveStyle(proc, &style), "sibling active style")
	in := processes.NodeClaimInput{
		StyleID: style, CoreNodeName: ksLine, Role: w.role, SwapMode: w.mode, PayloadCode: ksPart,
		UOPCapacity: 40, InboundSource: ksMarket, InboundStaging: ksSpot, OutboundDestination: ksDest,
	}
	switch w.mode {
	case protocol.SwapModeSingleRobot:
		in.OutboundStaging = "KS-OUT"
	case protocol.SwapModeTwoRobotPressIndex:
		in.PairedCoreNode = censusDeck
	}
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(w.t, err, "sibling claim")
	id, err := db.CreateOrder("uuid-sibling", orders.TypeRetrieve, &sib, w.role == protocol.ClaimRoleProduce, 1,
		ksLine, "", ksMarket, "standard", false, ksPart, "", "")
	testutil.MustNoErr(w.t, err, "the sibling's bin")
	w.setStatus(id, protocol.StatusInTransit)
	return id
}

// newChangeoverWorld is a two_robot process mid-changeover, FROM to TO: line X
// is in both styles (its swap is the changeover's), line Y is new in TO (the
// changeover delivers its first bin). The world's line is X; a state moves it.
func newChangeoverWorld(t *testing.T, role protocol.ClaimRole) (w *buttonWorld, x, y, proc, to int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	eng.wireEventHandlers()
	proc, err := db.CreateProcess("EX-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "process")
	x, err = db.CreateProcessNode(processes.NodeInput{ProcessID: proc, CoreNodeName: "EX-X", Code: "X", Name: "EX-X", Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "X")
	y, err = db.CreateProcessNode(processes.NodeInput{ProcessID: proc, CoreNodeName: "EX-Y", Code: "Y", Name: "EX-Y", Sequence: 2, Enabled: true})
	testutil.MustNoErr(t, err, "Y")
	from, err := db.CreateStyle("EX-FROM", "", proc)
	testutil.MustNoErr(t, err, "from")
	to, err = db.CreateStyle("EX-TO", "", proc)
	testutil.MustNoErr(t, err, "to")
	testutil.MustNoErr(t, db.SetActiveStyle(proc, &from), "active style")
	claim := func(style int64, node, part string) int64 {
		id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: style, CoreNodeName: node, Role: role, SwapMode: protocol.SwapModeTwoRobot,
			PayloadCode: part, UOPCapacity: 40, InboundSource: "EX-MKT", InboundStaging: node + "-STG",
			OutboundDestination: "EX-DEST",
		})
		testutil.MustNoErr(t, err, "claim")
		return id
	}
	fx := claim(from, "EX-X", "P-OLD")
	claim(to, "EX-X", "P-NEW")
	ty := claim(to, "EX-Y", "P-NEW")
	for _, n := range []struct{ id, claim int64 }{{x, fx}, {y, ty}} {
		_, err := db.EnsureProcessNodeRuntime(n.id)
		testutil.MustNoErr(t, err, "runtime")
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(n.id, &n.claim, 30), "runtime claim")
	}
	w = &buttonWorld{t: t, eng: eng, db: db, role: role, mode: protocol.SwapModeTwoRobot, node: x, occ: map[string]bool{}}
	w.occupied("EX-X")
	w.serveCore()
	_, err = eng.StartProcessChangeover(proc, to, "test", "")
	testutil.MustNoErr(t, err, "changeover start")
	return w, x, y, proc, to
}

func (w *buttonWorld) finishOrdersOf(node int64) {
	w.t.Helper()
	_, err := w.db.DB.Exec(`UPDATE orders SET status = 'confirmed' WHERE process_node_id = ?`, node)
	testutil.MustNoErr(w.t, err, "the node's orders done")
}

// lineState is one state a line can be in, made the same way for both roles.
type lineState struct {
	name  string
	modes []protocol.SwapMode
	// world builds the state and returns the line to press at.
	world func(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode) *buttonWorld
	// want is the answer every button gives; legs -1 means one swap of the mode.
	want buttonAnswer
}

const (
	swapBusy    = "a swap is already in progress — wait for the current cycle to complete or abort it before requesting more material"
	armed       = "A changeover to EX-TO is armed on this press — abandon it to request EX-FROM material."
	onItsWay    = "order 1 is already bringing a bin to KS-LINE — wait for it to land"
	noClaimText = "has no active claim"
)

func lineStates() []lineState {
	all := protocol.ConfigurableSwapModes()
	tr := []protocol.SwapMode{protocol.SwapModeTwoRobot}
	seq := []protocol.SwapMode{protocol.SwapModeSequential}
	cell := func(setup func(w *buttonWorld)) func(*testing.T, protocol.ClaimRole, protocol.SwapMode) *buttonWorld {
		return func(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode) *buttonWorld {
			w := newButtonWorld(t, role, mode)
			setup(w)
			return w
		}
	}
	changeover := func(setup func(w *buttonWorld, x, y, proc, to int64)) func(*testing.T, protocol.ClaimRole, protocol.SwapMode) *buttonWorld {
		return func(t *testing.T, role protocol.ClaimRole, _ protocol.SwapMode) *buttonWorld {
			w, x, y, proc, to := newChangeoverWorld(t, role)
			setup(w, x, y, proc, to)
			return w
		}
	}
	swap := buttonAnswer{legs: -1, trips: 1}
	return []lineState{
		{"the line holds a bin, nothing on its way", all, cell(func(*buttonWorld) {}), swap},
		{"the line is bare, nothing on its way", all, cell(func(w *buttonWorld) { w.bare(ksLine) }),
			buttonAnswer{toLine: 1, trips: 1}},

		// A changeover armed on the process.
		{"changeover armed, the line's own changeover leg in flight", tr,
			changeover(func(*buttonWorld, int64, int64, int64, int64) {}), buttonAnswer{refused: armed}},
		{"changeover armed, the line's own changeover legs done", tr,
			changeover(func(w *buttonWorld, x, _, _, _ int64) { w.finishOrdersOf(x) }), buttonAnswer{refused: armed}},
		{"changeover armed, a line new in the incoming style, its delivery in flight", tr,
			changeover(func(w *buttonWorld, _, y, _, _ int64) { w.node = y }),
			buttonAnswer{refused: "node EX-Y: " + swapBusy}},
		{"changeover armed, a line new in the incoming style, its delivery landed and confirmed", tr,
			changeover(func(w *buttonWorld, _, y, _, _ int64) {
				w.node = y
				w.finishOrdersOf(y)
				w.occupied("EX-Y")
			}), buttonAnswer{legs: 2, trips: 1}},
		{"changeover armed, the style flipped by hand, the line's changeover legs done", tr,
			changeover(func(w *buttonWorld, x, _, proc, to int64) {
				w.finishOrdersOf(x)
				testutil.MustNoErr(w.t, w.db.SetActiveStyle(proc, &to), "style flip")
			}), buttonAnswer{legs: 2, trips: 1}},
		{"a process node with no claim", all, func(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode) *buttonWorld {
			w := newButtonWorld(t, role, mode)
			procID, err := w.db.CreateProcess("BARE-PROC", "", "active_production", "", "", false)
			testutil.MustNoErr(t, err, "process")
			w.node, err = w.db.CreateProcessNode(processes.NodeInput{
				ProcessID: procID, CoreNodeName: "NO-CLAIM", Code: "NC", Name: "NO-CLAIM", Sequence: 1, Enabled: true,
			})
			testutil.MustNoErr(t, err, "node")
			return w
		}, buttonAnswer{refused: "node NO-CLAIM " + noClaimText}},

		// The line's own orders.
		{"the line's own swap working it", all, cell(func(w *buttonWorld) { w.request() }),
			buttonAnswer{refused: "node KS-LINE: " + swapBusy}},
		{"the line's own delivery on its way to the bare line", all, cell(func(w *buttonWorld) {
			w.bare(ksLine)
			w.request()
		}), buttonAnswer{refused: "node KS-LINE: " + swapBusy}},
		{"the line's own delivery delivered, not confirmed", all, cell(func(w *buttonWorld) {
			w.bare(ksLine)
			w.request()
			w.setStatus(w.lastOrder(), protocol.StatusDelivered)
			w.occupied(ksLine)
		}), buttonAnswer{refused: "node KS-LINE: " + swapBusy}},

		// Another station on the same line.
		{"another station's bin on its way to the bare line", all, cell(func(w *buttonWorld) {
			w.bare(ksLine)
			w.sibling()
		}), buttonAnswer{refused: "node KS-LINE: " + onItsWay}},
		{"another station's bin delivered, not confirmed", all, cell(func(w *buttonWorld) {
			w.setStatus(w.sibling(), protocol.StatusDelivered)
		}), swap},

		// A sequential line's own swap.
		{"the removal at the line, its backfill on its way", seq, cell(func(w *buttonWorld) {
			w.request()
			removal := w.lastOrder()
			driveToInTransit(w.t, w.eng, removal, w.node)
			if w.lastOrder() == removal {
				w.t.Fatal("the removal on its way made no backfill")
			}
		}), buttonAnswer{refused: "node KS-LINE: " + swapBusy}},
		{"the removal done, its backfill delivered, not confirmed", seq, cell(func(w *buttonWorld) {
			w.request()
			removal := w.lastOrder()
			driveToInTransit(w.t, w.eng, removal, w.node)
			w.setStatus(removal, protocol.StatusConfirmed)
			w.setStatus(w.lastOrder(), protocol.StatusDelivered)
		}), buttonAnswer{refused: "node KS-LINE: " + swapBusy}},
	}
}

// swapLegs is how many legs a swap of the mode has.
func swapLegs(mode protocol.SwapMode) int {
	if mode == protocol.SwapModeTwoRobot || mode == protocol.SwapModeTwoRobotPressIndex {
		return 2
	}
	return 1
}

func TestRequestButtons_OneAnswerPerLineState(t *testing.T) {
	t.Parallel()
	for _, st := range lineStates() {
		for _, mode := range st.modes {
			name := st.name
			if len(st.modes) > 1 {
				name += "/" + string(mode)
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				want := st.want
				if want.legs == -1 {
					want.legs = swapLegs(mode)
				}
				answers := map[string]buttonAnswer{
					doorMaterial: st.world(t, protocol.ClaimRoleConsume, mode).press(doorMaterial),
					doorProduce:  st.world(t, protocol.ClaimRoleProduce, mode).press(doorProduce),
					doorEmptyBin: st.world(t, protocol.ClaimRoleProduce, mode).press(doorEmptyBin),
				}
				for door, got := range answers {
					refusedOK := got.refused == want.refused ||
						want.refused != "" && strings.HasSuffix(got.refused, want.refused)
					g, w := got, want
					g.refused, w.refused = "", ""
					if !refusedOK || g != w {
						t.Errorf("%s: got %+v refused %q, want %+v refused %q", door, g, got.refused, w, want.refused)
					}
				}
				if a, b, c := answers[doorMaterial], answers[doorProduce], answers[doorEmptyBin]; a != b || b != c {
					t.Errorf("the buttons disagree:\n  material  %+v\n  produce   %+v\n  empty-bin %+v", a, b, c)
				}
			})
		}
	}
}

// The level keeper asks through the same door, so it is refused the same way:
// with another station's bin on its way to the line, a breached level asks for
// nothing, where it made a second delivery.
func TestLevelSweep_AnotherStationsBinOnItsWayAsksForNothing(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			w := newButtonWorld(t, role, protocol.SwapModeTwoRobot)
			w.bare(ksLine)
			level := 5 // at the consume claim's level of 10 or below
			if role == protocol.ClaimRoleProduce {
				level = 40 // the produce claim's capacity
			}
			_, err := w.db.DB.Exec(`UPDATE style_node_claims SET auto_reorder = 1, reorder_point = 10 WHERE core_node_name = ?`, ksLine)
			testutil.MustNoErr(t, err, "arm the level keeper")
			testutil.MustNoErr(t, w.db.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: 1, Name: ksPart, Code: ksPart, UOPCapacity: 40}),
				"the part's capacity")
			rt, err := w.db.GetProcessNodeRuntime(w.node)
			testutil.MustNoErr(t, err, "runtime")
			testutil.MustNoErr(t, w.db.SetProcessNodeRuntime(w.node, rt.ActiveClaimID, level), "count at the level")
			w.sibling()
			before := w.lastOrder()

			w.eng.sweepCellLevels()
			if after := w.lastOrder(); after != before {
				rows, err := w.db.ListOrders()
				testutil.MustNoErr(t, err, "orders the sweep made")
				for _, o := range rows {
					t.Logf("order %d %s %s->%s %s", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode, o.Status)
				}
				t.Fatalf("the level keeper made %d orders with another station's bin on its way, want none", after-before)
			}
		})
	}
}
