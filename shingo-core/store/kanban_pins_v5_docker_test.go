//go:build docker

package store_test

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_pins_v5_docker_test.go — the family's numbers: found by place with its flight
// arm and the spoken-for exclusion, cover from the claims, and the minimum pool. Taken
// before the reads change. Prediction per pin: PINS.md, "BRIEF v5".

// pinFamily is one (kanban, family) as a reader sees it.
type pinFamily struct{ slot, line, window, flight, coming int }

func (f pinFamily) found() int { return f.slot + f.line + f.window + f.flight + f.coming }

// pinFound reads the family's found, by place, off the kanban's one loop read.
func pinFound(t *testing.T, db *store.DB, kanban int64, types ...string) pinFamily {
	t.Helper()
	counts, err := db.ReadKanbanCounts([]int64{kanban})
	testutil.MustNoErr(t, err, "the counts")
	bt, err := db.GetBinTypeByCode(types[0])
	testutil.MustNoErr(t, err, "the type")
	found := counts.Found(kanban, bt.FamilyID)
	return pinFamily{slot: found.Slot, line: found.Line, window: found.Window, flight: found.Flight}
}

// pinFamilyOf groups types into a family, where the table exists to say so.
func pinFamilyOf(t *testing.T, db *store.DB, family string, types ...string) {
	t.Helper()
	if _, err := db.DB.Exec(`INSERT INTO carrier_families (code) VALUES ($1) ON CONFLICT DO NOTHING`, family); err != nil {
		t.Logf("no family table at this tree: %v", err)
		return
	}
	for _, code := range types {
		_, err := db.DB.Exec(`UPDATE bin_types SET family_id = (SELECT id FROM carrier_families WHERE code = $1) WHERE code = $2`, family, code)
		testutil.MustNoErr(t, err, "family of "+code)
	}
}

// loopRig is K with a window of its own unloader, a supported position and its slots.
type loopRig struct {
	*pictureRig
	window, position, slot, transit *nodes.Node
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	r := &loopRig{pictureRig: newPictureRig(t)}
	db := r.db
	r.window = &nodes.Node{Name: "PIC-WINDOW", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(r.window), "window")
	lid, err := db.CreateLoader(loaders.Loader{Name: "PIC-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: "PIC-K"})
	testutil.MustNoErr(t, err, "unloader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: r.window.ID}), "window row")
	r.position, err = db.GetNodeByName("PIC-FRONT")
	testutil.MustNoErr(t, err, "position")
	r.slot, err = db.GetNodeByName("PIC-K-P1")
	testutil.MustNoErr(t, err, "slot")
	r.transit, err = db.GetNodeByName("_TRANSIT")
	testutil.MustNoErr(t, err, "transit")
	return r
}

func (r *loopRig) cart(label, code string, node int64) int64 {
	r.t.Helper()
	var id int64
	testutil.MustNoErr(r.t, r.db.QueryRow(`INSERT INTO bins (label, bin_type_id, node_id) VALUES ($1,$2,$3) RETURNING id`,
		label, r.types[code], node).Scan(&id), "cart "+label)
	return id
}

// delivering is a live order carrying the cart into node.
func (r *loopRig) delivering(uuid string, cart int64, from, to string) *orders.Order {
	r.t.Helper()
	o := testdb.CreateOrder(r.t, r.db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "edge.1"
		o.OrderType = "move"
		o.OriginClass = protocol.OriginClassNoDemand
		o.SourceNode = from
		o.DeliveryNode = to
		o.NamedBinID = &cart
		o.HoldFromBirth(cart)
		o.Status = protocol.StatusInTransit
	})
	return o
}

// TestKanbanPin_FoundCountsTheCartOnTheRobot: a lift writes the cart's node to _TRANSIT
// (bins.LiftToTransitTx). A cart a live order is delivering into the loop is the loop's.
// A cart at _TRANSIT delivering anywhere else is not.
func TestKanbanPin_FoundCountsTheCartOnTheRobot(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	r.cart("LOOP-K", "PIC-SMALL", r.slot.ID)
	onRobot := r.cart("LOOP-AIR", "PIC-SMALL", r.transit.ID)
	r.delivering("loop-air", onRobot, r.position.Name, r.window.Name)
	away := r.cart("LOOP-AWAY", "PIC-SMALL", r.transit.ID)
	r.delivering("loop-away", away, r.slot.Name, "PIC-NOT-OURS")
	if got := pinFound(t, r.db, r.k, "PIC-SMALL"); got.found() != 2 {
		t.Errorf("found %+v = %d, want 2: the cart standing and the cart on the robot, not the cart leaving", got, got.found())
	}
}

// TestKanbanPin_AStandingFiledCartCountsOnce: a cart at a window with its empty-out filed
// is both "at a window" and "coming". One predicate decides, and it counts once. At the
// tree the kanban cannot see the window at all, so it reads one through the coming arm
// alone: this pin is green here and guards the arm the read adds.
func TestKanbanPin_AStandingFiledCartCountsOnce(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	filed := r.cart("LOOP-FILED", "PIC-SMALL", r.window.ID)
	o := r.delivering("loop-filed", filed, r.window.Name, "PIC-K")
	_, err := r.db.DB.Exec(`UPDATE orders SET status = $1 WHERE id = $2`, protocol.StatusQueued, o.ID)
	testutil.MustNoErr(t, err, "the push waits")
	unfiled := r.cart("LOOP-UNFILED", "PIC-SMALL", r.position.ID)
	if got := pinFound(t, r.db, r.k, "PIC-SMALL"); got.found() != 2 {
		t.Errorf("found %+v = %d, want 2: the filed cart once and the cart on the line (%d)", got, got.found(), unfiled)
	}
}

// TestKanbanPin_ABareCartAtAWindowCountsUnderItsCarrier: stage 1's CLEAR stamps a marker
// on the cart it leaves. It is a cart of the family and every empties read refuses it.
func TestKanbanPin_ABareCartAtAWindowCountsUnderItsCarrier(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	tx, err := r.db.DB.Begin()
	testutil.MustNoErr(t, err, "tx")
	marker, err := bins.EnsureBareMarkerTx(tx, r.types["PIC-SMALL"])
	testutil.MustNoErr(t, err, "the marker")
	testutil.MustNoErr(t, tx.Commit(), "commit")
	var id int64
	testutil.MustNoErr(t, r.db.QueryRow(`INSERT INTO bins (label, bin_type_id, node_id) VALUES ('LOOP-BARE',$1,$2) RETURNING id`,
		marker, r.window.ID).Scan(&id), "the bare cart")
	if got := pinFound(t, r.db, r.k, "PIC-SMALL"); got.found() != 1 {
		t.Errorf("found %+v = %d, want the bare cart under its carrier's family", got, got.found())
	}
}

// TestKanbanPin_MarkerTakesItsCarriersFamily: bins.EnsureBareMarkerTx copies the family as
// it copies the robot group.
func TestKanbanPin_MarkerTakesItsCarriersFamily(t *testing.T) {
	t.Parallel()
	r := newPictureRig(t)
	pinFamilyOf(t, r.db, "PIC-FAM", "PIC-SMALL", "PIC-BIG")
	tx, err := r.db.DB.Begin()
	testutil.MustNoErr(t, err, "tx")
	marker, err := bins.EnsureBareMarkerTx(tx, r.types["PIC-SMALL"])
	testutil.MustNoErr(t, err, "the marker")
	testutil.MustNoErr(t, tx.Commit(), "commit")
	var same bool
	err = r.db.QueryRow(`SELECT m.family_id = c.family_id FROM bin_types m JOIN bin_types c ON c.id = m.bare_of WHERE m.id = $1`, marker).Scan(&same)
	if err != nil || !same {
		t.Errorf("the marker's family equals its carrier's: %v (err %v)", same, err)
	}
}

// TestKanbanPin_AMixedStyleCoversByItsOwnClaims: two parts, two types, one family, on four
// positions. Left is small, right is big: cover 2 + 2. Today each type is counted on the
// kanban's cover in full: want 6 + 6 of 10 carts, short 2 of each forever.
func TestKanbanPin_AMixedStyleCoversByItsOwnClaims(t *testing.T) {
	t.Parallel()
	r := newPictureRig(t)
	for _, code := range []string{"PIC-SMALL", "PIC-BIG", "PIC-TOTES"} {
		testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k, BinTypeID: r.types[code], Want: 2}), "level")
	}
	r.part("PIC-L", "PIC-SMALL")
	r.part("PIC-R", "PIC-BIG")
	r.mirror("PIC-PRESS", styleOf{"MIXED", true, [][2]string{
		{"PIC-FRONT", "PIC-L"}, {"PIC-PAIRED", "PIC-L"}, {"PIC-SECOND", "PIC-R"}, {"PIC-WELD", "PIC-R"}}})
	got := map[string]int{}
	for code, row := range r.picture() {
		got[code] = row.Want()
	}
	if got["PIC-SMALL"] != 4 || got["PIC-BIG"] != 4 || got["PIC-TOTES"] != 2 {
		t.Errorf("want = %v, want small 2+2, big 2+2, totes 2: 10 of the family", got)
	}
}

// pinMinimum reads the minimum pool of (kanban, family), through the one statement the
// save and the census share.
func pinMinimum(t *testing.T, db *store.DB, kanban int64, family string) int {
	t.Helper()
	mins, err := db.ListKanbanMinimums([]int64{kanban})
	testutil.MustNoErr(t, err, "the minimums")
	for _, m := range mins {
		if m.FamilyCode == family {
			return m.Minimum()
		}
	}
	return -1
}

// fivePresses is five Press-6 shapes on one kanban, each with a 45x48 style and a 32x30
// style on its four positions, and a mix of 2/2/2/2.
func fivePresses(t *testing.T) *pictureRig {
	t.Helper()
	r := newPictureRig(t)
	db := r.db
	var t32 int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('PIC-32') RETURNING id`).Scan(&t32), "32")
	r.types["PIC-32"] = t32
	for _, code := range []string{"PIC-SMALL", "PIC-BIG", "PIC-TOTES", "PIC-32"} {
		testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k, BinTypeID: r.types[code], Want: 2}), "level")
	}
	pinFamilyOf(t, db, "45x48", "PIC-SMALL", "PIC-BIG", "PIC-TOTES")
	pinFamilyOf(t, db, "32x30", "PIC-32")
	r.part("PIC-P45", "PIC-SMALL")
	r.part("PIC-P32", "PIC-32")
	var supports []int64
	for p := 1; p <= 5; p++ {
		var positions, positions32 [][2]string
		for i := 1; i <= 4; i++ {
			n := &nodes.Node{Name: fmt.Sprintf("PIC-P%d-%d", p, i), Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(n), "position")
			supports = append(supports, n.ID)
			positions = append(positions, [2]string{n.Name, "PIC-P45"})
			positions32 = append(positions32, [2]string{n.Name, "PIC-P32"})
		}
		r.mirror(fmt.Sprintf("PIC-PROC-%d", p), styleOf{"S45", true, positions}, styleOf{"S32", false, positions32})
	}
	testutil.MustNoErr(t, db.SetKanbanSupports(r.k, supports), "supports")
	return r
}

// TestKanbanPin_MinimumOnFiveProcesses: Σ mix + Σ over supported processes of MAX over that
// process's styles of DISTINCT positions in the family: 6 + 5x4 = 26 and 2 + 5x4 = 22.
func TestKanbanPin_MinimumOnFiveProcesses(t *testing.T) {
	t.Parallel()
	r := fivePresses(t)
	if got := pinMinimum(t, r.db, r.k, "45x48"); got != 26 {
		t.Errorf("minimum of 45x48 = %d, want 26", got)
	}
	if got := pinMinimum(t, r.db, r.k, "32x30"); got != 22 {
		t.Errorf("minimum of 32x30 = %d, want 22", got)
	}
}

// TestKanbanPin_ArrivalIsWhatOldestMeans: ListKanbanSurplus's "oldest" is the arrival. An
// UPDATE that moves updated_at on a cart which arrived first does not make it younger.
func TestKanbanPin_ArrivalIsWhatOldestMeans(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	first := r.cart("ARR-FIRST", "PIC-SMALL", r.slot.ID)
	p2, err := r.db.GetNodeByName("PIC-K-P2")
	testutil.MustNoErr(t, err, "slot 2")
	second := r.cart("ARR-SECOND", "PIC-SMALL", p2.ID)
	_, err = r.db.DB.Exec(`UPDATE bins SET created_at = NOW() - INTERVAL '2 hour', updated_at = NOW() WHERE id = $1`, first)
	testutil.MustNoErr(t, err, "the first arrived first")
	_, err = r.db.DB.Exec(`UPDATE bins SET created_at = NOW() - INTERVAL '1 hour', updated_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, second)
	testutil.MustNoErr(t, err, "the second arrived second")
	got, err := r.db.ListKanbanSurplus(r.k, []string{"PIC-SMALL"})
	testutil.MustNoErr(t, err, "the surplus")
	if len(got) != 2 || got[0].ID != first {
		t.Errorf("oldest first = %d carts, first %d; want the cart that arrived first, %d", len(got), got[0].ID, first)
	}
}

// TestKanbanPin_TheDoorKnowsTheLoopBySource: a window of the kanban's own unloader, one of
// its supports and one of its slots (the cancel-return's source) are the loop's. Anywhere
// else is a stranger's, and the door answers nothing.
func TestKanbanPin_TheDoorKnowsTheLoopBySource(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	stranger := &nodes.Node{Name: "PIC-STRANGER", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(stranger), "stranger")
	for name, loop := range map[string]bool{r.window.Name: true, r.position.Name: true, r.slot.Name: true, stranger.Name: false} {
		door, err := r.db.ReadKanbanDoor(r.k, r.types["PIC-SMALL"], name, 0)
		testutil.MustNoErr(t, err, "door of "+name)
		if (door != nil) != loop {
			t.Errorf("door from %s answered %v, the source is the loop's: %v", name, door != nil, loop)
		}
	}
}

// TestKanbanPin_ACartOnARobotOnASwapCountsOnce: a swap's cart is a row of the swap's, lifted at the
// slot and set down on the position. Lifted, it stands on a robot with a live order, and that order
// speaks for it: it is not also a cart on a robot with none, whatever its last plain order named.
func TestKanbanPin_ACartOnARobotOnASwapCountsOnce(t *testing.T) {
	t.Parallel()
	r := newLoopRig(t)
	onRobot := r.cart("LOOP-SWAP", "PIC-SMALL", r.slot.ID)
	last := r.delivering("loop-last", onRobot, r.window.Name, r.slot.Name)
	_, err := r.db.Exec(`UPDATE orders SET status = 'confirmed' WHERE id = $1`, last.ID)
	testutil.MustNoErr(t, err, "its last order ends")
	_, err = r.db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, r.transit.ID, onRobot)
	testutil.MustNoErr(t, err, "the lift")
	swap := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID = "loop-swap"
		o.StationID = "edge.1"
		o.OrderType = "complex"
		o.OriginClass = protocol.OriginClassNoDemand
		o.Status = protocol.StatusInTransit
		o.StepsJSON = `[{"action":"pickup","node":"` + r.slot.Name + `"},{"action":"dropoff","node":"` + r.position.Name + `"}]`
	})
	testdb.SeedCartRow(t, r.db, swap.ID, onRobot, 0, 1)
	got := pinFound(t, r.db, r.k, "PIC-SMALL")
	if got.found() != 1 || got.flight != 1 {
		t.Errorf("found %+v = %d, want the one cart, in flight", got, got.found())
	}
}

// TestKanbanPin_ACartOnARobotAfterItsSwapEndedStaysCounted: a two-cart swap carries the cart at its
// process node and the cart it has lifted off the kanban, each a row of its own. Cancelled with the
// second on its robot, the rows stay, so that cart still has its order on record: the loop counts it
// in flight and the stranded watch can find it. With no order naming it the rig read 21 of 22.
func TestKanbanPin_ACartOnARobotAfterItsSwapEndedStaysCounted(t *testing.T) {
	t.Parallel()
	for _, status := range []protocol.Status{protocol.StatusCancelled, protocol.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			r := newLoopRig(t)
			atLine := r.cart("LOOP-SWAP-LINE", "PIC-SMALL", r.position.ID)
			onRobot := r.cart("LOOP-SWAP-END", "PIC-SMALL", r.slot.ID)
			swap := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
				o.EdgeUUID = "loop-swap-end"
				o.StationID = "edge.1"
				o.OrderType = "complex"
				o.OriginClass = protocol.OriginClassNoDemand
				o.SourceNode = r.position.Name
				o.DeliveryNode = r.position.Name
				o.Status = protocol.StatusInTransit
				o.StepsJSON = `[{"action":"pickup","node":"` + r.position.Name + `"},{"action":"dropoff","node":"` + r.window.Name +
					`"},{"action":"pickup","node":"` + r.slot.Name + `"},{"action":"dropoff","node":"` + r.position.Name + `"}]`
			})
			testdb.SeedCartRow(t, r.db, swap.ID, atLine, 0, 1)
			testdb.SeedCartRow(t, r.db, swap.ID, onRobot, 2, 3)
			_, err := r.db.Exec(`UPDATE bins SET node_id = $1, claimed_by = $2 WHERE id = $3`, r.transit.ID, swap.ID, onRobot)
			testutil.MustNoErr(t, err, "the lift")
			if got := pinFound(t, r.db, r.k, "PIC-SMALL"); got.found() != 2 {
				t.Fatalf("fixture drift: found %+v before the end, want both carts", got)
			}
			_, err = r.db.TerminalizeOrder(swap.ID, status, "test")
			testutil.MustNoErr(t, err, "the swap ends")
			got := pinFound(t, r.db, r.k, "PIC-SMALL")
			if got.found() != 2 || got.flight != 1 {
				t.Errorf("found %+v = %d, want both carts, the lifted one in flight", got, got.found())
			}
			if !testdb.HasCartRow(t, r.db, swap.ID, onRobot) {
				t.Errorf("the ended swap's rows %v lost the cart on the robot, %d", testdb.CartRowBins(t, r.db, swap.ID), onRobot)
			}
		})
	}
}
