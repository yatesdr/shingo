//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// overflow_parked_docker_test.go — MG6-1 for a parked push. Intake sends a push
// to a maintained group's overflow when the group is at its level; a push that
// parked because the overflow had no room either is asked again on every
// scanner pass, and takes the overflow once it has room, through the one re-aim
// the reserve makes (the group's resolve, the back-to-group arm). An order a
// robot has is never re-aimed.
//
// The sim's shape (round 4, orders 432 and 1251): UNLOADER-A's empty-out met
// SYN_PRESS_EMPTIES at its level while SYN_MARKET had no slot, parked, and held
// the unloader's window for 233 sim-min while SYN_MARKET's slots came and went.

// parkedPush is a maintained group at its level (one position held of two, a
// level of one), its overflow with one position a blocker stands on, and a
// move holding an empty cart at a line, aimed at the group, waiting.
type parkedPush struct {
	group, overflow string
	blocker, away   int64
	push            *orders.Order
}

func newParkedPush(t *testing.T, db *store.DB, pfx string, withOverflow bool) parkedPush {
	t.Helper()
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`
		INSERT INTO bin_types (code) VALUES ($1)
		ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
		RETURNING id`, pfx+"-T").Scan(&btID), "bin type")
	carrier := func(label string, nodeID int64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
			btID, label, nodeID).Scan(&id), "carrier "+label)
		return id
	}
	group := func(name string, positions int) (int64, []int64) {
		id, err := nodes.CreateGroup(db.DB, name)
		testutil.MustNoErr(t, err, "group "+name)
		testdb.MarkGroup(t, db, id)
		var ids []int64
		for i := 0; i < positions; i++ {
			n := &nodes.Node{Name: name + "-P" + string(rune('A'+i)), Enabled: true, ParentID: &id}
			testutil.MustNoErr(t, db.CreateNode(n), "position")
			ids = append(ids, n.ID)
		}
		return id, ids
	}
	plain := func(name string) int64 {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n.ID
	}

	fx := parkedPush{group: pfx + "-FULL", overflow: pfx + "-SPILL"}
	grpID, grpPos := group(fx.group, 2)
	carrier(pfx+"-RES", grpPos[0])
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, Want: 1}), "level")
	_, spillPos := group(fx.overflow, 1)
	fx.blocker = carrier(pfx+"-BLOCK", spillPos[0])
	if withOverflow {
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropOverflowDestination, fx.overflow), "overflow")
	}
	fx.away = plain(pfx + "-AWAY")
	src := pfx + "-SRC"
	cart := carrier(pfx+"-CART", plain(src))

	fx.push = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = pfx + "-push"
		o.StationID = "ovp-line"
		o.OrderType = dispatch.OrderTypeMove
		o.SourceNode = src
		o.DeliveryNode = fx.group
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, fx.push.ID, fx.push.ID, cart, "test"), "the push holds its cart")
	testutil.MustNoErr(t, db.UpdateOrderBinID(fx.push.ID, cart), "the push points at its cart")
	return fx
}

// freeOverflow moves the blocker off the overflow's one position.
func (fx parkedPush) freeOverflow(t *testing.T, db *store.DB) {
	t.Helper()
	_, err := db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, fx.away, fx.blocker)
	testutil.MustNoErr(t, err, "free the overflow's position")
}

// slotRows is the nodes an order holds live slot rows on.
func slotRows(t *testing.T, db *store.DB, orderID int64) []string {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT n.name FROM reservations r JOIN nodes n ON n.id = r.node_id
		 WHERE r.order_id = $1 AND r.resource_kind = 'slot' AND `+reservations.ActiveStateSQL("r.")+`
		 ORDER BY n.name`, orderID)
	testutil.MustNoErr(t, err, "slot rows")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		testutil.MustNoErr(t, rows.Scan(&n), "scan slot row")
		out = append(out, n)
	}
	return out
}

// TestOverflow_AParkedPushTakesTheOverflowOnThePassAfterASlotFreesThere: the
// push parks at its group's level while the overflow is full, and the pass after
// the overflow's slot frees sends it there and hands it to the fleet.
//
// RED at 6235f088: the push stays aimed at the group, queued, for as long as the
// group holds its level (the overflow was asked at admission only).
func TestOverflow_AParkedPushTakesTheOverflowOnThePassAfterASlotFreesThere(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	fx := newParkedPush(t, db, "OVP1", true)

	eng.fulfillment.RunOnce()
	parked, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the push")
	if parked.Status != protocol.StatusQueued || parked.DeliveryNode != fx.group || parked.QueueCause != string(dispatch.CauseNGRPAtLevel) {
		t.Fatalf("with the overflow full the push is %s to %s (%s), want queued to %s (%s)",
			parked.Status, parked.DeliveryNode, parked.QueueCause, fx.group, dispatch.CauseNGRPAtLevel)
	}

	fx.freeOverflow(t, db)
	eng.fulfillment.RunOnce()
	got, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the push")
	if got.DeliveryNode != fx.overflow+"-PA" || got.Status != protocol.StatusDispatched {
		t.Errorf("the pass after the overflow's slot freed left the push %s to %s (%s %q); want dispatched to %s",
			got.Status, got.DeliveryNode, got.QueueCause, got.QueueReason, fx.overflow+"-PA")
	}
}

// TestOverflow_APushParkedAtAFullGroupTakesItsOverflowToo: intake takes the
// overflow for a group that refuses for room either way, at its level or full,
// so the parked half does too: a group with no level and every position taken.
//
// RED at 6235f088, as the level's case.
func TestOverflow_APushParkedAtAFullGroupTakesItsOverflowToo(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	fx := newParkedPush(t, db, "OVP5", true)
	_, err := db.Exec(`DELETE FROM node_maintain_levels WHERE group_node_id = (SELECT id FROM nodes WHERE name = $1)`, fx.group)
	testutil.MustNoErr(t, err, "no level")
	_, err = db.Exec(`INSERT INTO bins (bin_type_id, label, node_id, status)
		SELECT bin_type_id, 'OVP5-RES2', (SELECT id FROM nodes WHERE name = $1), 'available' FROM bins WHERE id = $2`,
		fx.group+"-PB", fx.blocker)
	testutil.MustNoErr(t, err, "fill the group")

	eng.fulfillment.RunOnce()
	parked, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the push")
	if parked.DeliveryNode != fx.group || parked.QueueCause != string(dispatch.CauseNGRPFull) {
		t.Fatalf("with the overflow full the push is %s to %s (%s), want queued to %s (%s)",
			parked.Status, parked.DeliveryNode, parked.QueueCause, fx.group, dispatch.CauseNGRPFull)
	}
	fx.freeOverflow(t, db)
	eng.fulfillment.RunOnce()
	got, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the push")
	if got.DeliveryNode != fx.overflow+"-PA" || got.Status != protocol.StatusDispatched {
		t.Errorf("the pass after the overflow's slot freed left the push %s to %s (%s); want dispatched to %s",
			got.Status, got.DeliveryNode, got.QueueCause, fx.overflow+"-PA")
	}
}

// TestOverflow_ADispatchedPushIsNeverReAimed: the passes that send a parked push
// to the overflow leave an order a robot has where it was going, even into the
// same group at its level: MG6-1's no mid-route re-aim.
func TestOverflow_ADispatchedPushIsNeverReAimed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)
	fx := newParkedPush(t, db, "OVP2", true)

	var inFlightBin int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status)
		 SELECT bin_type_id, 'OVP2-INFLIGHT', $1, 'available' FROM bins WHERE id = $2 RETURNING id`,
		fx.away, fx.blocker).Scan(&inFlightBin), "the in-flight carrier")
	moving := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "OVP2-moving"
		o.StationID = "ovp-line"
		o.OrderType = dispatch.OrderTypeMove
		o.Status = protocol.StatusDispatched
		o.SourceNode = "OVP2-AWAY"
		o.DeliveryNode = fx.group + "-PB"
		o.RobotID = "AMR-OVP2"
		o.BinID = &inFlightBin
	})
	testdb.ClaimBinForTest(t, db, inFlightBin, moving.ID)

	fx.freeOverflow(t, db)
	asked := len(backend.CreateRequests())
	eng.fulfillment.RunOnce()
	eng.fulfillment.RunOnce()

	p, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the parked push")
	if p.DeliveryNode != fx.overflow+"-PA" {
		t.Fatalf("the parked push is %s to %s, not sent to the overflow, so these passes prove nothing", p.Status, p.DeliveryNode)
	}
	got, err := db.GetOrder(moving.ID)
	testutil.MustNoErr(t, err, "reload the moving order")
	if got.DeliveryNode != fx.group+"-PB" || got.Status != protocol.StatusDispatched {
		t.Errorf("the order a robot has is %s to %s, want dispatched to %s, untouched", got.Status, got.DeliveryNode, fx.group+"-PB")
	}
	for _, r := range backend.CreateRequests()[asked:] {
		if r.ExternalID == moving.EdgeUUID {
			t.Errorf("the moving order was sent to the fleet again: %+v", r)
		}
	}
}

// TestOverflow_AReAimLeavesNoSlotRowBehind: parked, the push holds no slot row;
// sent to the overflow, its one slot row is on its new drop and none is left on
// the group (A6's sweep, clause 4, asked here as well as at cleanup).
func TestOverflow_AReAimLeavesNoSlotRowBehind(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	fx := newParkedPush(t, db, "OVP3", true)

	eng.fulfillment.RunOnce()
	if rows := slotRows(t, db, fx.push.ID); len(rows) != 0 {
		t.Errorf("parked at its group's level, the push holds slot rows on %v, want none", rows)
	}
	fx.freeOverflow(t, db)
	eng.fulfillment.RunOnce()
	if rows := slotRows(t, db, fx.push.ID); len(rows) != 1 || rows[0] != fx.overflow+"-PA" {
		t.Errorf("sent to the overflow, the push holds slot rows on %v, want only %s", rows, fx.overflow+"-PA")
	}
	testdb.AssertArmorMatchesStatus(t, db)
}

// TestOverflow_AParkedPushIsNotPlacedOverItsTypesLevel: the reserve's resolve,
// newly asked of an order the gate parked at its level, judges the carrier's own
// type as the gate does (the resolver settles it from the asking order), not the
// group's total: a group at its level for this type with room under another
// type's level keeps the push parked while the overflow is full.
func TestOverflow_AParkedPushIsNotPlacedOverItsTypesLevel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())
	fx := newParkedPush(t, db, "OVP4", true)
	grp, err := db.GetNodeByDotName(fx.group)
	testutil.MustNoErr(t, err, "the group")
	third := &nodes.Node{Name: fx.group + "-PC", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(third), "a third position")
	var other int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('OVP4-OTHER') RETURNING id`).Scan(&other), "another type")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: grp.ID, BinTypeID: other, Want: 1}), "its level, unmet")

	eng.fulfillment.RunOnce()
	got, err := db.GetOrder(fx.push.ID)
	testutil.MustNoErr(t, err, "reload the push")
	if got.DeliveryNode != fx.group || got.Status != protocol.StatusQueued || got.QueueCause != string(dispatch.CauseNGRPAtLevel) {
		t.Errorf("the push is %s to %s (%s); want queued to %s (%s): its type is at its level, and the "+
			"group's total having room under another type's level is not room for it",
			got.Status, got.DeliveryNode, got.QueueCause, fx.group, dispatch.CauseNGRPAtLevel)
	}
}

// TestOverflow_OnlyAParkedPushPaysPerPass counts one scanner pass's statements
// with one waiting order (store.OpenCounting; an unstarted engine wired with its
// dispatcher and scanner alone, so nothing else reads), at the pass after the
// one that parked it, and pins the cost's SHAPE, not its numbers: the pass's own
// overhead (P, what a pass costs an order waiting on an occupied line beyond its
// gate), the capacity gate on the order's group (G) and on the overflow (O),
// each measured here by calling dispatch.CheckDropoffCapacity alone. A parked
// order at a group that names no overflow pays P + G + 1: the one read of the
// group's overflow name. With one named and full, P + G + 1 + O: the overflow's
// own gate, and no resolve. An order waiting on any other cause never asks.
// Before the shape (edb48c1e), the parked order paid the group's resolve every
// pass (+8) and, with an overflow named, the overflow's (+12).
func TestOverflow_OnlyAParkedPushPaysPerPass(t *testing.T) {
	t.Parallel()
	type cost struct{ pass, gate, overflow int64 }
	measure := func(t *testing.T, pfx string, withOverflow, line bool) cost {
		t.Helper()
		db, cfg := testdb.OpenWithConfig(t)
		testdb.SetupStandardData(t, db)
		fx := newParkedPush(t, db, pfx, withOverflow)
		dest := fx.group
		if line {
			ln := &nodes.Node{Name: pfx + "-LINE", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(ln), "line")
			_, err := db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, ln.ID, fx.blocker)
			testutil.MustNoErr(t, err, "occupy the line")
			testutil.MustNoErr(t, db.UpdateOrderDeliveryNode(fx.push.ID, ln.Name), "aim at the line")
			dest = ln.Name
		}
		cdb, counter, err := store.OpenCounting(cfg)
		testutil.MustNoErr(t, err, "counting db")
		t.Cleanup(func() { cdb.Close() })
		eng := newUnstartedEngine(t, cdb, testdb.NewTrackingBackend())
		resolver := &dispatch.DefaultResolver{DB: cdb}
		eng.dispatcher = eng.newDispatcher(&dispatchEmitter{bus: eng.Events, engine: eng}, resolver)
		eng.startFulfillment(resolver)
		eng.fulfillment.RunOnce()
		counter.Reset()
		eng.fulfillment.RunOnce()
		var c cost
		c.pass = counter.Count()
		t.Logf("STATEMENTS: %s: %d per pass\n%s", pfx, c.pass, testutil.StatementBreakdown(counter.Statements()))
		gate := func(name string) int64 {
			counter.Reset()
			dispatch.CheckDropoffCapacity(cdb, name, fx.push.ID)
			return counter.Count()
		}
		c.gate = gate(dest)
		if withOverflow {
			c.overflow = gate(fx.overflow)
		}
		return c
	}
	full := measure(t, "OVPC1", true, false)
	none := measure(t, "OVPC2", false, false)
	line := measure(t, "OVPC3", false, true)
	p := line.pass - line.gate
	t.Logf("STATEMENTS: pass overhead P %d; group gate G %d; overflow gate O %d; per pass: overflow full %d, none named %d, occupied line %d",
		p, full.gate, full.overflow, full.pass, none.pass, line.pass)
	if want := p + none.gate + 1; none.pass != want {
		t.Errorf("parked at a group naming no overflow: %d statements per pass, want P+G+1 = %d (the gate and one read)", none.pass, want)
	}
	if want := p + full.gate + 1 + full.overflow; full.pass != want {
		t.Errorf("parked with the overflow full: %d statements per pass, want P+G+1+O = %d (and the overflow's gate, no resolve)", full.pass, want)
	}
}

// morePushes parks n more moves on the parked push's group, each holding an empty
// cart of the fixture's type at a line of its own.
func morePushes(t *testing.T, db *store.DB, pfx string, fx parkedPush, n int) {
	t.Helper()
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM bin_types WHERE code = $1`, pfx+"-T").Scan(&btID), "the fixture's type")
	for i := 0; i < n; i++ {
		src := &nodes.Node{Name: fmt.Sprintf("%s-SRC%d", pfx, i+2), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(src), "a line")
		var cart int64
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
			btID, fmt.Sprintf("%s-CART%d", pfx, i+2), src.ID).Scan(&cart), "a cart")
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("%s-push%d", pfx, i+2)
			o.StationID = "ovp-line"
			o.OrderType = dispatch.OrderTypeMove
			o.SourceNode = src.Name
			o.DeliveryNode = fx.group
		})
		testutil.MustNoErr(t, reservations.Acquire(db.DB, o.ID, o.ID, cart, "test"), "the push holds its cart")
		testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, cart), "the push points at its cart")
	}
}

// laneOverflow names, as the group's overflow, a group of one lane whose mouth a
// carrier stands on and whose back slot is free: the capacity gate counts the
// lane free, and the resolve finds no slot it can reach (the sim's SYN_MARKET, in
// one lane). Returns the overflow group's name.
func laneOverflow(t *testing.T, db *store.DB, pfx string, fx parkedPush) string {
	t.Helper()
	grp, _, slots := laneGroup(t, db, pfx+"-LO", 1, 2)
	binAt(t, db, slots[0][0], "")
	g, err := db.GetNodeByDotName(fx.group)
	testutil.MustNoErr(t, err, "the parked group")
	testutil.MustNoErr(t, db.SetNodeProperty(g.ID, nodes.PropOverflowDestination, grp.Name), "the lane overflow")
	return grp.Name
}

// TestOverflow_OneOverflowAskPerGroupPerPass: N orders parked on one group ask
// its overflow once per scanner pass and share the answer (overflowRefusedThisPass).
// The shape, not the numbers: two more parked orders add the same statements
// whether the group names no overflow or names one the gate admits and the
// resolve refuses (each pays its own gate on the group, and nothing for the
// overflow), and the one ask is there (one parked order at the lane overflow pays
// more than one at a group naming none). Measured as TestOverflow_OnlyAParkedPushPaysPerPass
// measures: store.OpenCounting, an unstarted engine with its dispatcher and scanner,
// the pass after the one that parked them.
//
// RED at 8bdd2304: every parked order asked the overflow, and on a lane group the
// reserve, so each extra order paid the overflow's resolve again.
func TestOverflow_OneOverflowAskPerGroupPerPass(t *testing.T) {
	t.Parallel()
	pass := func(t *testing.T, pfx string, lane bool, extra int) int64 {
		t.Helper()
		db, cfg := testdb.OpenWithConfig(t)
		testdb.SetupStandardData(t, db)
		fx := newParkedPush(t, db, pfx, false)
		if lane {
			overflow := laneOverflow(t, db, pfx, fx)
			if blocked, _ := dispatch.CheckDropoffCapacity(db, overflow, fx.push.ID); blocked {
				t.Fatalf("fixture drift: the gate refuses the lane overflow; the case is the gate admitting what the resolve refuses")
			}
		}
		morePushes(t, db, pfx, fx, extra)
		cdb, counter, err := store.OpenCounting(cfg)
		testutil.MustNoErr(t, err, "counting db")
		t.Cleanup(func() { cdb.Close() })
		eng := newUnstartedEngine(t, cdb, testdb.NewTrackingBackend())
		resolver := &dispatch.DefaultResolver{DB: cdb}
		eng.dispatcher = eng.newDispatcher(&dispatchEmitter{bus: eng.Events, engine: eng}, resolver)
		eng.startFulfillment(resolver)
		eng.fulfillment.RunOnce()
		counter.Reset()
		eng.fulfillment.RunOnce()
		n := counter.Count()
		parked, err := db.GetOrder(fx.push.ID)
		testutil.MustNoErr(t, err, "reload the push")
		if parked.DeliveryNode != fx.group || parked.Status != protocol.StatusQueued {
			t.Fatalf("fixture drift: the push is %s to %s; it must stay parked on its group", parked.Status, parked.DeliveryNode)
		}
		t.Logf("STATEMENTS: %s: %d per pass with %d parked", pfx, n, extra+1)
		return n
	}
	none1, none3 := pass(t, "OVPS1", false, 0), pass(t, "OVPS3", false, 2)
	lane1, lane3 := pass(t, "OVPL1", true, 0), pass(t, "OVPL3", true, 2)
	if lane1 <= none1 {
		t.Errorf("one parked order at the lane overflow pays %d, at a group naming none %d: the overflow was never asked", lane1, none1)
	}
	if lane3-lane1 != none3-none1 {
		t.Errorf("two more parked orders add %d statements at the lane overflow and %d at a group naming none: "+
			"the overflow was asked per order, not once per group per pass", lane3-lane1, none3-none1)
	}
}
