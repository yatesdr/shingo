package messaging

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
)

// robot_id_test.go — the Edge keeps the order's robot (orders.robot_id, v50).
// The waybill always carried it and the Edge dropped it; order.update carries
// it too, so a robot that changes after the first waybill reaches the Edge.
//
// An empty robot on the wire is ABSENT, never a clear, on both messages: the
// field is omitempty, so "Core sent none" and "Core sent empty" are the same
// bytes (the queue_reason lesson, queue_reason_clear_test.go). The waybill Core
// sends at dispatch names no robot, and it must not wipe one.

func robotOf(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("read order %d: %v", id, err)
	}
	return o.RobotID
}

func TestHandleOrderWaybill_StoresTheRobotItNames(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)
	id, err := db.CreateOrder("robot-wb", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// The waybill sent at dispatch: a vendor order, no robot yet.
	h.HandleOrderWaybill(nil, &protocol.OrderWaybill{OrderUUID: "robot-wb", WaybillID: "W1"})
	if got := robotOf(t, db, id); got != "" {
		t.Fatalf("after the dispatch waybill: robot %q, want none", got)
	}

	// The waybill sent on the robot's first assignment.
	h.HandleOrderWaybill(nil, &protocol.OrderWaybill{OrderUUID: "robot-wb", WaybillID: "W1", RobotID: "AMR-07"})
	if got := robotOf(t, db, id); got != "AMR-07" {
		t.Fatalf("after the assignment waybill: robot %q, want AMR-07", got)
	}
	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if o.WaybillID == nil || *o.WaybillID != "W1" {
		t.Errorf("waybill_id = %v, want W1 (the robot rides the waybill's own statement)", o.WaybillID)
	}

	// A later waybill that names no robot leaves it.
	h.HandleOrderWaybill(nil, &protocol.OrderWaybill{OrderUUID: "robot-wb", WaybillID: "W2"})
	if got := robotOf(t, db, id); got != "AMR-07" {
		t.Errorf("a waybill naming no robot cleared it: %q, want AMR-07", got)
	}
}

func TestHandleOrderUpdate_StoresTheRobotItNames(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	h := testHandler(t, db)
	id, err := db.CreateOrder("robot-upd", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	h.HandleOrderWaybill(nil, &protocol.OrderWaybill{OrderUUID: "robot-upd", WaybillID: "W1", RobotID: "AMR-07"})

	// Core holds a different robot now and says so on the update.
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: "robot-upd",
		Status: string(protocol.StatusStaged), RobotID: "AMR-11"})
	if got := robotOf(t, db, id); got != "AMR-11" {
		t.Fatalf("after an update naming AMR-11: robot %q", got)
	}

	// An update naming none (an older Core, or a Core that holds none) leaves it.
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: "robot-upd",
		Status: string(protocol.StatusInTransit)})
	if got := robotOf(t, db, id); got != "AMR-11" {
		t.Errorf("an update naming no robot cleared it: %q, want AMR-11", got)
	}
}

// Core puts the robot on every update once it holds one, so most pushes name
// the robot the row already has. That write must not bump updated_at, which
// staleness sweeps read (the reason SetFaultClock's write is guarded too).
func TestSetOrderRobotID_UnchangedRobotWritesNothing(t *testing.T) {
	t.Parallel()
	db := testHandlerDB(t)
	id, err := db.CreateOrder("robot-same", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	const old = "2020-01-01 00:00:00"
	if _, err := db.Exec(`UPDATE orders SET robot_id='AMR-07', updated_at=? WHERE id=?`, old, id); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stamp := func() string {
		var s string
		if err := db.QueryRow(`SELECT updated_at FROM orders WHERE id=?`, id).Scan(&s); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return s
	}

	if err := db.SetOrderRobotID("robot-same", "AMR-07"); err != nil {
		t.Fatalf("set same robot: %v", err)
	}
	if got := stamp(); got != old {
		t.Errorf("the same robot rewrote the row: updated_at %s, want %s", got, old)
	}
	if err := db.SetOrderRobotID("robot-same", "AMR-11"); err != nil {
		t.Fatalf("set new robot: %v", err)
	}
	if got := stamp(); got == old {
		t.Errorf("a new robot did not stamp updated_at")
	}
}
