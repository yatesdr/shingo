//go:build docker

package service

import (
	"context"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/demands"
	"shingocore/store/nodes"
)

// TestInventoryLoop_ReadsTheRegistry pins that /inventory's rows are the
// registry's: a monitored binding is a row, a binding whose row is gone is not
// one, and a failed read is a failure rather than "nothing is monitored". (The
// pin moved here from the threshold monitor's Snapshot, which the page read
// until 2026-10-29.)
func TestInventoryLoop_ReadsTheRegistry(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	at := &nodes.Node{Name: "LOOP-RT-HOME", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(at), "create node")
	createTestBin(t, db, at.ID, "LOOP-RT-BIN", "LOOP-RT", 40)
	_, err := db.SyncDemandRegistry("ST-LOOP", []demands.RegistryEntry{{
		StationID: "ST-LOOP", CoreNodeName: at.Name, Role: protocol.ClaimRoleConsume, PayloadCode: "LOOP-RT",
		ReplenishUOPThreshold: 50,
	}})
	testutil.MustNoErr(t, err, "seed registry")
	svc := NewInventoryService(db)

	v, err := svc.Loop(context.Background(), time.Now(), 30*time.Minute)
	testutil.MustNoErr(t, err, "loop")
	if len(v.Rows) != 1 || v.Rows[0].PayloadCode != "LOOP-RT" || v.Rows[0].InLoop == nil || *v.Rows[0].InLoop != 40 ||
		v.Rows[0].State != LoopBelow {
		t.Fatalf("rows = %+v, want LOOP-RT at 40 below 50", v.Rows)
	}

	_, err = db.Exec(`DELETE FROM demand_registry WHERE payload_code = 'LOOP-RT'`)
	testutil.MustNoErr(t, err, "delete registry rows")
	v, err = svc.Loop(context.Background(), time.Now(), 30*time.Minute)
	testutil.MustNoErr(t, err, "loop after delete")
	if len(v.Rows) != 0 {
		t.Errorf("rows %+v after the registry rows went — the page would say a pair is monitored that is not", v.Rows)
	}

	_, err = db.Exec(`ALTER TABLE demand_registry RENAME TO demand_registry_hidden`)
	testutil.MustNoErr(t, err, "hide registry")
	if _, err := svc.Loop(context.Background(), time.Now(), 30*time.Minute); err == nil {
		t.Error("a failed registry read returned no error — the page would render it as nothing monitored")
	}
}

// TestInventoryLoop_LineRows pins a line's row end to end through Loop: its
// time left at the node's own rate over the rate window (sourceability's
// spelling: consume ticks drawn at a consume spot, produce ticks made at a
// produce spot; another node's ticks, a tick outside the window and a
// correction are not this line running), and On the way read from the live
// orders. A consume spot drew 10 in 30 minutes (a 180 s cycle) and holds 50,
// with one full coming; a produce spot made 12 (150 s) and holds 22 of 2400.
func TestInventoryLoop_LineRows(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	binAt := map[string]int64{}
	for name, b := range map[string]struct {
		payload string
		uop     int
	}{"ALN_R1": {"P-RIN", 50}, "PLN_R2": {"P-ROUT", 22}, "ALN_R9": {"P-RIN", 80}} {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "create node")
		binAt[name] = createTestBin(t, db, n.ID, name+"-BIN", b.payload, b.uop).ID
	}
	_, err := db.Exec(`INSERT INTO process_styles (process_id, style_id, is_active) VALUES ('LINE-R', 'S1', true)`)
	testutil.MustNoErr(t, err, "seed style")
	_, err = db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity, reorder_point)
		VALUES ('LINE-R', 'S1', 'ALN_R1', 'consume', 'single_robot', 'P-RIN', 132, 20),
		       ('LINE-R', 'S1', 'PLN_R2', 'produce', 'single_robot', 'P-ROUT', 2400, 0)`)
	testutil.MustNoErr(t, err, "seed claims")
	tick := func(node, payload, reason string, before, after int, minsAgo int) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO bin_uop_ledger (bin_id, node_id, before_uop, after_uop, op, reason, source, payload_code, actor, applied_at)
			VALUES ($1, (SELECT id FROM nodes WHERE name = $2), $3, $4, 'bin_uop_delta', $5, 'edge', $6, 'edge:test',
			        NOW() - make_interval(mins => $7))`, binAt[node], node, before, after, reason, payload, minsAgo)
		testutil.MustNoErr(t, err, "seed tick")
	}
	tick("ALN_R1", "P-RIN", "consume_tick", 60, 54, 5)        // drew 6
	tick("ALN_R1", "P-RIN", "consume_tick", 54, 50, 10)       // drew 4
	tick("ALN_R1", "P-RIN", "consume_tick", 200, 150, 45)     // outside the 30 minutes
	tick("ALN_R1", "P-RIN", "operator_correction", 90, 40, 5) // not the line running
	tick("ALN_R9", "P-RIN", "consume_tick", 100, 80, 5)       // another node
	tick("PLN_R2", "P-ROUT", "produce_tick", 10, 22, 5)       // made 12
	_, err = db.Exec(`INSERT INTO orders (edge_uuid, order_type, status, payload_code, delivery_node)
		VALUES ('rin-coming', 'retrieve', 'in_transit', 'P-RIN', 'ALN_R1')`)
	testutil.MustNoErr(t, err, "seed order")

	v, err := NewInventoryService(db).Loop(context.Background(), time.Now(), 30*time.Minute)
	testutil.MustNoErr(t, err, "loop")
	got := map[string]LoopRow{}
	for _, r := range v.Rows {
		if r.Kind == "line" {
			got[r.Node] = r
		}
	}
	in, out := got["ALN_R1"], got["PLN_R2"]
	t.Logf("PIN inventory-4/line-rows consume cycle=%v left=%v coming=%d; produce cycle=%v left=%v",
		deref(in.CycleSeconds), deref(in.CoverSeconds), in.Coming, deref(out.CycleSeconds), deref(out.CoverSeconds))
	if in.Role != "consume" || in.InBin == nil || *in.InBin != 50 || deref(in.CycleSeconds) != 180.0 ||
		deref(in.CoverSeconds) != 9000.0 || in.Coming != 1 || !in.Look || in.Beyond {
		t.Errorf("consume row = %+v, want 50 in the bin, a 180 s cycle, 9000 s left, 1 coming, shown, not beyond", in)
	}
	if out.Role != "produce" || out.InBin == nil || *out.InBin != 22 || deref(out.CycleSeconds) != 150.0 ||
		deref(out.CoverSeconds) != 2378*150.0 || out.Coming != 0 || out.Look {
		t.Errorf("produce row = %+v, want 22 in the bin, a 150 s cycle, 2378 x 150 s left, nothing coming, folded", out)
	}
	if _, ok := got["ALN_R9"]; ok {
		t.Error("a node no running style claims is a line row")
	}
}

// TestInventoryLoop_LiveOrders pins the live orders a line's On the way reads:
// every non-terminal order not yet delivered. A delivered bin is at its node
// already; a finished order is gone.
func TestInventoryLoop_LiveOrders(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	for _, o := range []struct{ uuid, status string }{
		{"live-queued", "queued"}, {"live-moving", "in_transit"}, {"live-delivered", "delivered"}, {"live-done", "confirmed"},
		{"live-failed", "failed"},
	} {
		_, err := db.Exec(`INSERT INTO orders (edge_uuid, order_type, status, payload_code, delivery_node)
			VALUES ($1, 'retrieve', $2, 'P-LIVE', 'ALN_LIVE')`, o.uuid, o.status)
		testutil.MustNoErr(t, err, "seed order "+o.uuid)
	}
	live, err := db.LoopLiveOrders()
	testutil.MustNoErr(t, err, "loop live orders")
	n := 0
	for _, o := range live {
		if o.DeliveryNode == "ALN_LIVE" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("live orders to ALN_LIVE = %d, want 2 (queued and in transit; not delivered, confirmed or failed)", n)
	}
}

// TestInventoryLoop_LineStation pins the Edge a line's cell reorder point is
// set on: the edge's display name for the style's station.
func TestInventoryLoop_LineStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO process_styles (process_id, style_id, is_active, station_id) VALUES ('LINE-E4', 'S1', true, 'stn-e4');
		INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity, reorder_point)
		VALUES ('LINE-E4', 'S1', 'PLN_E4', 'produce', 'single_robot', 'P-E4FG', 30, 4)`)
	testutil.MustNoErr(t, err, "seed claims")
	_, err = db.Exec(`INSERT INTO edge_registry (station_id, display_name) VALUES ('stn-e4', 'Line E4')`)
	testutil.MustNoErr(t, err, "seed edge")
	claims, err := db.LoopClaims()
	testutil.MustNoErr(t, err, "loop claims")
	station := ""
	for _, c := range claims {
		if c.CoreNodeName == "PLN_E4" {
			station = c.Station
		}
	}
	if station != "Line E4" {
		t.Errorf("line station = %q, want the edge's display name", station)
	}
}

// TestInventoryLoop_OnTheWayIsQueuedOrInTransit pins a position's On the way:
// orders to it that are queued or in transit. A bin delivered and waiting for
// its confirm is already there, and a finished order is gone.
func TestInventoryLoop_OnTheWayIsQueuedOrInTransit(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "OTW-HOME", Enabled: true}), "create node")
	_, err := db.SyncDemandRegistry("ST-OTW", []demands.RegistryEntry{
		{StationID: "ST-OTW", CoreNodeName: "OTW-HOME", Role: protocol.ClaimRoleConsume, PayloadCode: "P-OTW", ReplenishUOPThreshold: 50},
	})
	testutil.MustNoErr(t, err, "seed registry")
	for _, o := range []struct{ uuid, status string }{
		{"otw-queued", "queued"}, {"otw-moving", "in_transit"}, {"otw-delivered", "delivered"}, {"otw-done", "confirmed"},
	} {
		_, err := db.Exec(`INSERT INTO orders (edge_uuid, order_type, status, payload_code, delivery_node)
			VALUES ($1, 'retrieve_empty', $2, 'P-OTW', 'OTW-HOME')`, o.uuid, o.status)
		testutil.MustNoErr(t, err, "seed order "+o.uuid)
	}
	bindings, err := db.LoopBindings(time.Now().AddDate(0, 0, -LoopLeadDays), time.Now())
	testutil.MustNoErr(t, err, "loop bindings")
	got := -1
	for _, b := range bindings {
		if b.CoreNodeName == "OTW-HOME" {
			got = b.Coming
		}
	}
	if got != 2 {
		t.Errorf("on the way = %d, want 2 (queued and in transit; not delivered, not confirmed)", got)
	}
}

// TestInventoryLoop_CompletedL1IsPerPosition pins the count behind "no
// suggestion until the home has a completed L1 cycle" (I9): threshold
// retrieve_empty orders delivered to the position and confirmed inside the
// window. One queued and never completed (SPR's SMN_020) does not count, nor
// one delivered to another home, nor one completed before the window.
func TestInventoryLoop_CompletedL1IsPerPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	for _, name := range []string{"L1-HOME-A", "L1-HOME-B"} {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: name, Enabled: true}), "create node")
	}
	_, err := db.SyncDemandRegistry("ST-L1", []demands.RegistryEntry{
		{StationID: "ST-L1", CoreNodeName: "L1-HOME-A", Role: protocol.ClaimRoleConsume, PayloadCode: "P-L1", ReplenishUOPThreshold: 50},
		{StationID: "ST-L1", CoreNodeName: "L1-HOME-B", Role: protocol.ClaimRoleConsume, PayloadCode: "P-L1", ReplenishUOPThreshold: 50},
	})
	testutil.MustNoErr(t, err, "seed registry")
	order := func(uuid, node, kind string, confirmedAgo string) {
		t.Helper()
		var origin string
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO demand_origins (origin_id, episode_key, kind)
			VALUES (gen_random_uuid(), $1, $2) RETURNING origin_id`, uuid, kind).Scan(&origin), "origin")
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO orders (edge_uuid, order_type, status, payload_code, delivery_node, origin_id)
			VALUES ($1, 'retrieve_empty', 'confirmed', 'P-L1', $2, $3) RETURNING id`, uuid, node, origin).Scan(&id), "order")
		if confirmedAgo != "" {
			_, err := db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, 'confirmed', NOW() - $2::interval)`,
				id, confirmedAgo)
			testutil.MustNoErr(t, err, "history")
		}
	}
	order("l1-a-done", "L1-HOME-A", protocol.EpisodeKindThreshold, "1 hour")
	order("l1-a-old", "L1-HOME-A", protocol.EpisodeKindThreshold, "20 days")
	order("l1-a-cell", "L1-HOME-A", protocol.EpisodeKindCell, "1 hour")
	order("l1-b-never", "L1-HOME-B", protocol.EpisodeKindThreshold, "")

	bindings, err := db.LoopBindings(time.Now().AddDate(0, 0, -LoopLeadDays), time.Now())
	testutil.MustNoErr(t, err, "loop bindings")
	got := map[string]int{}
	for _, b := range bindings {
		got[b.CoreNodeName] = b.CompletedL1
	}
	t.Logf("PIN inventory-2/completed-l1 %v", got)
	if got["L1-HOME-A"] != 1 || got["L1-HOME-B"] != 0 {
		t.Errorf("completed L1 cycles = %v, want L1-HOME-A 1 and L1-HOME-B 0", got)
	}
}
