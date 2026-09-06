package store

import (
	"testing"
	"time"

	"shingo/protocol"
)

// TestMarkOrderDeparted_StampsOnce is the idempotence pin the review asked for.
//
// Core's rds.Poller keeps its per-block FINISHED states in memory, so a Core
// restart re-fires every already-finished block once. The same BinPickedUp
// therefore arrives twice, and the second arrival can be hours after the robot
// left. A last-write-wins update would move the departure instant forward to a
// time the robot was nowhere near the cell; the changed=false return is also
// what keeps the handler's log line to one.
func TestMarkOrderDeparted_StampsOnce(t *testing.T) {
	db := coverageDB(t)
	id := seedDepartureOrder(t, db, "uuid-departs-once")

	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if o.Departed || o.DepartedAt != nil {
		t.Fatalf("a fresh order must not read as departed: departed=%v at=%v", o.Departed, o.DepartedAt)
	}

	first := time.Date(2026, 9, 3, 14, 30, 0, 0, time.UTC)
	changed, err := db.MarkOrderDeparted(id, first)
	if err != nil {
		t.Fatalf("MarkOrderDeparted: %v", err)
	}
	if !changed {
		t.Fatal("the first stamp must report changed=true — it is what the handler logs on")
	}

	// The replay: same order, a much later instant.
	later := first.Add(3 * time.Hour)
	changed, err = db.MarkOrderDeparted(id, later)
	if err != nil {
		t.Fatalf("MarkOrderDeparted (replay): %v", err)
	}
	if changed {
		t.Error("a replayed pickup must report changed=false — one stamp, one log line")
	}

	o, err = db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder after replay: %v", err)
	}
	if !o.Departed {
		t.Fatal("the order must read as departed after the stamp")
	}
	if o.DepartedAt == nil {
		t.Fatal("Departed is true but DepartedAt is nil — the scan helper set one without the other")
	}
	if !o.DepartedAt.Equal(first) {
		t.Errorf("departed_at = %v, want %v — the replay moved the instant forward to a time the robot was nowhere near the cell",
			o.DepartedAt, first)
	}
}

// TestMarkOrderDeparted_SurvivesTheListReads pins that the column is carried by
// every scan path the guards and the board use, not just the single-row Get.
// The two must agree: the slot check reads GetOrder, the durable-row arm reads
// ListActiveOrdersByProcessNode, and a board tile reads ListActiveByNodeKeys.
func TestMarkOrderDeparted_SurvivesTheListReads(t *testing.T) {
	db := coverageDB(t)
	nodeID := seedDepartureNode(t, db)
	id := seedDepartureOrderAtNode(t, db, "uuid-departs-listed", &nodeID)

	at := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	if _, err := db.MarkOrderDeparted(id, at); err != nil {
		t.Fatalf("MarkOrderDeparted: %v", err)
	}

	listed, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d active orders, want 1 (a departed leg is still non-terminal and still listed)", len(listed))
	}
	if !listed[0].Departed || listed[0].DepartedAt == nil {
		t.Errorf("the list read lost the departure: departed=%v at=%v", listed[0].Departed, listed[0].DepartedAt)
	}
	if protocol.IsTerminal(listed[0].Status) {
		t.Fatal("fixture drift: the order must be non-terminal, or the list read would not return it at all")
	}
}

// TestMarkOrderLeftCell_IsIndependentOfDeparture pins the v40 column against the
// mistake it exists to prevent being re-made: cell_left_at is NOT departed_at
// under a new name.
//
// The robot leaving the cell's nodes and the leg's placement being recorded are
// two facts. cell_left_at carries the first; departed_at is the conjunction. A
// row that has one and not the other is the whole point — it is a leg that put a
// carrier on the line and cannot yet prove it, and it must still read as working
// the cell.
func TestMarkOrderLeftCell_IsIndependentOfDeparture(t *testing.T) {
	db := coverageDB(t)
	id := seedDepartureOrder(t, db, "uuid-left-cell")

	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if o.CellLeftAt != nil {
		t.Fatalf("a fresh order must not have left the cell: %v", o.CellLeftAt)
	}

	left := time.Date(2026, 9, 6, 9, 34, 9, 0, time.UTC)
	changed, err := db.MarkOrderLeftCell(id, left)
	if err != nil {
		t.Fatalf("MarkOrderLeftCell: %v", err)
	}
	if !changed {
		t.Fatal("the first stamp must report changed=true — it is what the handler logs on")
	}

	o, err = db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder after the stamp: %v", err)
	}
	if o.CellLeftAt == nil || !o.CellLeftAt.Equal(left) {
		t.Fatalf("cell_left_at = %v, want %v", o.CellLeftAt, left)
	}
	if o.Departed || o.DepartedAt != nil {
		t.Fatal("leaving the cell's NODES stamped a DEPARTURE. The two are independent columns: " +
			"the departure also needs the leg's placement to be recorded, and collapsing them " +
			"reintroduces the double-supply race the conjunction closes")
	}

	// The replay: Core restarts and re-fires the same FINISHED block.
	changed, err = db.MarkOrderLeftCell(id, left.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("MarkOrderLeftCell (replay): %v", err)
	}
	if changed {
		t.Error("a replayed pickup must report changed=false — one stamp, one log line")
	}
	o, err = db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder after replay: %v", err)
	}
	if o.CellLeftAt == nil || !o.CellLeftAt.Equal(left) {
		t.Errorf("cell_left_at = %v, want %v — the replay moved the instant forward to a time the robot "+
			"was nowhere near the cell", o.CellLeftAt, left)
	}

	// The departure lands second, from the placement record. Neither stamp clears
	// the other.
	departed := left.Add(30 * time.Second)
	if _, err := db.MarkOrderDeparted(id, departed); err != nil {
		t.Fatalf("MarkOrderDeparted: %v", err)
	}
	o, err = db.GetOrder(id)
	if err != nil {
		t.Fatalf("GetOrder after the departure: %v", err)
	}
	if !o.Departed || o.DepartedAt == nil || !o.DepartedAt.Equal(departed) {
		t.Fatalf("departed_at = %v (departed=%v), want %v", o.DepartedAt, o.Departed, departed)
	}
	if o.CellLeftAt == nil || !o.CellLeftAt.Equal(left) {
		t.Errorf("cell_left_at = %v after the departure, want %v — it records when the robot left and "+
			"nothing may move it", o.CellLeftAt, left)
	}
}

// TestMarkOrderLeftCell_SurvivesTheListReads is the twin of the departed-at list
// pin. settleCellPlacement reads its candidates from ListActiveOrdersByProcessNode
// and skips any row whose CellLeftAt is nil — so a scan path that dropped the
// column would silently make the second trigger a no-op, and every single_robot
// cell would hold until its leg went terminal.
func TestMarkOrderLeftCell_SurvivesTheListReads(t *testing.T) {
	db := coverageDB(t)
	nodeID := seedDepartureNode(t, db)
	id := seedDepartureOrderAtNode(t, db, "uuid-left-cell-listed", &nodeID)

	left := time.Date(2026, 9, 6, 9, 34, 9, 0, time.UTC)
	if _, err := db.MarkOrderLeftCell(id, left); err != nil {
		t.Fatalf("MarkOrderLeftCell: %v", err)
	}

	listed, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d active orders, want 1 (a leg that left the cell is still non-terminal)", len(listed))
	}
	if listed[0].CellLeftAt == nil || !listed[0].CellLeftAt.Equal(left) {
		t.Errorf("the list read lost cell_left_at: %v", listed[0].CellLeftAt)
	}
	if listed[0].Departed {
		t.Error("the list read says departed for a leg that has only left the cell's nodes")
	}
}

// seedDepartureNode creates the process/node scaffolding a node-scoped order
// needs.
func seedDepartureNode(t *testing.T, db *DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO processes (name) VALUES ('DEPARTURE-PROC')`)
	if err != nil {
		t.Fatalf("seed process: %v", err)
	}
	processID, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name, sequence)
		VALUES (?, 'PRESS-A', 'P1', 'Press A', 1)`, processID)
	if err != nil {
		t.Fatalf("seed process node: %v", err)
	}
	nodeID, _ := res.LastInsertId()
	return nodeID
}

func seedDepartureOrder(t *testing.T, db *DB, uuid string) int64 {
	t.Helper()
	return seedDepartureOrderAtNode(t, db, uuid, nil)
}

func seedDepartureOrderAtNode(t *testing.T, db *DB, uuid string, nodeID *int64) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO orders (uuid, order_type, status, process_node_id)
		VALUES (?, ?, ?, ?)`, uuid, protocol.OrderTypeComplex, protocol.StatusInTransit, nodeID)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}
