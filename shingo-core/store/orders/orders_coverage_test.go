//go:build docker

// NOTE: this file uses `package orders_test` (external test package) rather
// than `package orders`. The task rubric said `package orders`, but that
// doesn't compile: `shingocore/store` imports `shingocore/store/orders`,
// and `shingocore/internal/testdb` imports `shingocore/store`. A test file
// with `package orders` importing testdb would create a cycle
// orders -> testdb -> store -> orders. `package orders_test` is the
// standard black-box workaround and lives in the same directory.
package orders_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// newPendingOrder returns a minimal but valid *domain.Order. Tests tweak
// fields as needed after calling this.
func newPendingOrder(uuid string) *domain.Order {
	return &domain.Order{
		EdgeUUID:     uuid,
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: "LINE1-IN",
		SourceNode:   "STORAGE-A1",
	}
}

// -------- Order CRUD lifecycle ---------------------------------------------

func TestOrderCRUD(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-crud")
	o.PayloadDesc = "steel tote"
	o.PayloadCode = "PART-A"
	o.Priority = 5

	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if o.ID == 0 {
		t.Fatal("Create must assign o.ID")
	}

	got, err := orders.Get(db, o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EdgeUUID != "uuid-crud" {
		t.Errorf("EdgeUUID = %q, want %q", got.EdgeUUID, "uuid-crud")
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if got.PayloadDesc != "steel tote" {
		t.Errorf("PayloadDesc = %q, want steel tote", got.PayloadDesc)
	}
	if got.PayloadCode != "PART-A" {
		t.Errorf("PayloadCode = %q, want PART-A", got.PayloadCode)
	}
	if got.Priority != 5 {
		t.Errorf("Priority = %d, want 5", got.Priority)
	}
	if got.DeliveryNode != "LINE1-IN" {
		t.Errorf("DeliveryNode = %q, want LINE1-IN", got.DeliveryNode)
	}

	// GetByUUID
	gotUUID, err := orders.GetByUUID(db, "uuid-crud")
	if err != nil {
		t.Fatalf("GetByUUID: %v", err)
	}
	if gotUUID.ID != o.ID {
		t.Errorf("GetByUUID.ID = %d, want %d", gotUUID.ID, o.ID)
	}

	// UpdateStatus -> dispatched (also writes history)
	if err := orders.UpdateStatus(db, o.ID, "dispatched", "sent to RDS"); err != nil {
		t.Fatalf("UpdateStatus dispatched: %v", err)
	}
	got2, err := orders.Get(db, o.ID)
	if err != nil {
		t.Fatalf("Get after UpdateStatus: %v", err)
	}
	if got2.Status != "dispatched" {
		t.Errorf("Status = %q, want dispatched", got2.Status)
	}
	// Non-terminal transition clears error_detail.
	if got2.ErrorDetail != "" {
		t.Errorf("ErrorDetail = %q, want empty on non-terminal transition", got2.ErrorDetail)
	}

	// UpdateVendor
	if err := orders.UpdateVendor(db, o.ID, "rds-123", "RUNNING", "AMB-01"); err != nil {
		t.Fatalf("UpdateVendor: %v", err)
	}
	got3, _ := orders.Get(db, o.ID)
	if got3.VendorOrderID != "rds-123" {
		t.Errorf("VendorOrderID = %q, want rds-123", got3.VendorOrderID)
	}
	if got3.VendorState != "RUNNING" {
		t.Errorf("VendorState = %q, want RUNNING", got3.VendorState)
	}
	if got3.RobotID != "AMB-01" {
		t.Errorf("RobotID = %q, want AMB-01", got3.RobotID)
	}

	// GetByVendorID
	byVendor, err := orders.GetByVendorID(db, "rds-123")
	if err != nil {
		t.Fatalf("GetByVendorID: %v", err)
	}
	if byVendor.ID != o.ID {
		t.Errorf("GetByVendorID.ID = %d, want %d", byVendor.ID, o.ID)
	}

	// UpdateRobotID (narrow, only touches robot_id)
	if err := orders.UpdateRobotID(db, o.ID, "AMB-99"); err != nil {
		t.Fatalf("UpdateRobotID: %v", err)
	}
	got4, _ := orders.Get(db, o.ID)
	if got4.RobotID != "AMB-99" {
		t.Errorf("RobotID = %q, want AMB-99", got4.RobotID)
	}
	if got4.VendorOrderID != "rds-123" {
		t.Errorf("UpdateRobotID should leave VendorOrderID intact, got %q", got4.VendorOrderID)
	}
	if got4.VendorState != "RUNNING" {
		t.Errorf("UpdateRobotID should leave VendorState intact, got %q", got4.VendorState)
	}

	// Complete sets completed_at (status handled separately).
	if err := orders.Complete(db, o.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got5, _ := orders.Get(db, o.ID)
	if got5.CompletedAt == nil {
		t.Error("CompletedAt should be set after Complete")
	}
}

// -------- Failed/cancelled transitions preserve error_detail --------------

func TestUpdateStatus_PreservesErrorDetailOnTerminal(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-fail")
	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := orders.UpdateStatus(db, o.ID, "failed", "robot crashed"); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}
	got, _ := orders.Get(db, o.ID)
	if got.Status != "failed" {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.ErrorDetail != "robot crashed" {
		t.Errorf("ErrorDetail = %q, want 'robot crashed'", got.ErrorDetail)
	}

	// Cancelled also preserves detail.
	o2 := newPendingOrder("uuid-cancel")
	if err := orders.Create(db, o2); err != nil {
		t.Fatalf("Create o2: %v", err)
	}
	if err := orders.UpdateStatus(db, o2.ID, "cancelled", "operator stop"); err != nil {
		t.Fatalf("UpdateStatus cancelled: %v", err)
	}
	got2, _ := orders.Get(db, o2.ID)
	if got2.ErrorDetail != "operator stop" {
		t.Errorf("ErrorDetail = %q, want 'operator stop'", got2.ErrorDetail)
	}
}

// -------- History: append via UpdateStatus, list oldest-first -------------

func TestHistory_AppendAndList(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-history")
	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create: %v", err)
	}

	events := []struct {
		status string
		detail string
	}{
		{"sourcing", "picking bin"},
		{"dispatched", "sent to RDS"},
		{"in_transit", "robot moving"},
		{"confirmed", "delivered"},
	}
	for _, e := range events {
		if err := orders.UpdateStatus(db, o.ID, e.status, e.detail); err != nil {
			t.Fatalf("UpdateStatus %s: %v", e.status, err)
		}
	}

	hist, err := orders.ListHistory(db, o.ID)
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(hist) != len(events) {
		t.Fatalf("history len = %d, want %d", len(hist), len(events))
	}
	// Assert full ordering (ListHistory returns oldest-first / ascending id)
	// and set-equality of (status, detail) pairs.
	for i, got := range hist {
		if string(got.Status) != events[i].status {
			t.Errorf("history[%d].Status = %q, want %q", i, got.Status, events[i].status)
		}
		if got.Detail != events[i].detail {
			t.Errorf("history[%d].Detail = %q, want %q", i, got.Detail, events[i].detail)
		}
		if got.OrderID != o.ID {
			t.Errorf("history[%d].OrderID = %d, want %d", i, got.OrderID, o.ID)
		}
		if i > 0 && hist[i].ID <= hist[i-1].ID {
			t.Errorf("history[%d].ID = %d not strictly after history[%d].ID = %d",
				i, hist[i].ID, i-1, hist[i-1].ID)
		}
	}
}

// -------- List + status filter --------------------------------------------

func TestList_FilterByStatus(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status string) {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
	}
	mk("p1", "pending")
	mk("p2", "pending")
	mk("p3", "pending")
	mk("c1", "confirmed")
	mk("c2", "confirmed")
	mk("f1", "failed")

	all, err := orders.List(db, "", 100)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 6 {
		t.Errorf("List all len = %d, want 6", len(all))
	}

	pending, err := orders.List(db, "pending", 100)
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if len(pending) != 3 {
		t.Errorf("pending len = %d, want 3", len(pending))
	}
	for _, o := range pending {
		if o.Status != "pending" {
			t.Errorf("found non-pending order %q in pending filter", o.Status)
		}
	}

	confirmed, _ := orders.List(db, "confirmed", 100)
	if len(confirmed) != 2 {
		t.Errorf("confirmed len = %d, want 2", len(confirmed))
	}

	// Limit is honored.
	limited, _ := orders.List(db, "", 2)
	if len(limited) != 2 {
		t.Errorf("limit=2 returned %d rows", len(limited))
	}

	// ListActive: 6 total - 2 confirmed - 1 failed = 3.
	active, err := orders.ListActive(db)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 3 {
		t.Errorf("active len = %d, want 3", len(active))
	}
	for _, o := range active {
		if o.Status == "confirmed" || o.Status == "failed" || o.Status == "cancelled" {
			t.Errorf("ListActive returned terminal status %q", o.Status)
		}
	}
}

// -------- ListFiltered: statuses, station, since, limit, offset -----------

func TestListFiltered(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status, station string) int64 {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		o.StationID = station
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
		return o.ID
	}
	mk("u1", "pending", "line-1")
	mk("u2", "confirmed", "line-1")
	mk("u3", "pending", "line-2")
	mk("u4", "dispatched", "line-1")
	mk("u5", "failed", "line-2")

	// Statuses IN (...)
	got, err := orders.ListFiltered(db, orders.Filter{Statuses: []string{"pending", "dispatched"}})
	if err != nil {
		t.Fatalf("ListFiltered statuses: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("statuses filter len = %d, want 3", len(got))
	}
	for _, o := range got {
		if o.Status != "pending" && o.Status != "dispatched" {
			t.Errorf("unexpected status %q in statuses filter", o.Status)
		}
	}

	// StationID.
	st1, err := orders.ListFiltered(db, orders.Filter{StationID: "line-1"})
	if err != nil {
		t.Fatalf("ListFiltered station: %v", err)
	}
	if len(st1) != 3 {
		t.Errorf("station=line-1 len = %d, want 3", len(st1))
	}
	for _, o := range st1 {
		if o.StationID != "line-1" {
			t.Errorf("StationID = %q, want line-1", o.StationID)
		}
	}

	// Combined.
	combo, _ := orders.ListFiltered(db, orders.Filter{Statuses: []string{"pending"}, StationID: "line-2"})
	if len(combo) != 1 {
		t.Errorf("combined filter len = %d, want 1", len(combo))
	}

	// Pagination: limit + offset, sorted id DESC, no overlap.
	page1, _ := orders.ListFiltered(db, orders.Filter{Limit: 2, Offset: 0})
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}
	page2, _ := orders.ListFiltered(db, orders.Filter{Limit: 2, Offset: 2})
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}
	if len(page1) == 2 && page1[0].ID <= page1[1].ID {
		t.Errorf("page1 not sorted id DESC: %d then %d", page1[0].ID, page1[1].ID)
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Errorf("page1 and page2 overlap on id %d", a.ID)
			}
		}
	}

	// Since in the future: 0 rows.
	future := time.Now().Add(1 * time.Hour)
	none, _ := orders.ListFiltered(db, orders.Filter{Since: &future})
	if len(none) != 0 {
		t.Errorf("future-since len = %d, want 0", len(none))
	}

	// Since in the past: all rows.
	past := time.Now().Add(-24 * time.Hour)
	recent, _ := orders.ListFiltered(db, orders.Filter{Since: &past})
	if len(recent) != 5 {
		t.Errorf("past-since len = %d, want 5", len(recent))
	}
}

// -------- Compound orders: children + GetNextChild ------------------------

func TestChildOrders(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	parent := newPendingOrder("parent")
	parent.OrderType = "compound"
	if err := orders.Create(db, parent); err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	children := []*domain.Order{
		{EdgeUUID: "c1", StationID: "line-1", OrderType: "retrieve", Status: "pending", Quantity: 1, ParentOrderID: &parent.ID, Sequence: 1},
		{EdgeUUID: "c2", StationID: "line-1", OrderType: "store", Status: "pending", Quantity: 1, ParentOrderID: &parent.ID, Sequence: 2},
		{EdgeUUID: "c3", StationID: "line-1", OrderType: "move", Status: "pending", Quantity: 1, ParentOrderID: &parent.ID, Sequence: 3},
	}
	for _, c := range children {
		if err := orders.Create(db, c); err != nil {
			t.Fatalf("Create child %s: %v", c.EdgeUUID, err)
		}
	}

	kids, err := orders.ListChildren(db, parent.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 3 {
		t.Fatalf("children len = %d, want 3", len(kids))
	}
	for i, k := range kids {
		if k.Sequence != i+1 {
			t.Errorf("kids[%d].Sequence = %d, want %d", i, k.Sequence, i+1)
		}
		if k.ParentOrderID == nil || *k.ParentOrderID != parent.ID {
			t.Errorf("kids[%d] parent mismatch", i)
		}
	}

	// GetNextChild: first pending.
	next, err := orders.GetNextChild(db, parent.ID)
	if err != nil {
		t.Fatalf("GetNextChild: %v", err)
	}
	if next.ID != children[0].ID {
		t.Errorf("GetNextChild = %d, want %d", next.ID, children[0].ID)
	}

	// Advance: mark c1 confirmed, next pending is c2.
	if err := orders.UpdateStatus(db, children[0].ID, "confirmed", "done"); err != nil {
		t.Fatalf("UpdateStatus c1: %v", err)
	}
	next2, _ := orders.GetNextChild(db, parent.ID)
	if next2.ID != children[1].ID {
		t.Errorf("GetNextChild after c1 done = %d, want %d", next2.ID, children[1].ID)
	}
}

// -------- Narrow field updates --------------------------------------------

func TestNarrowUpdates(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("narrow")
	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := orders.UpdateSourceNode(db, o.ID, "STORAGE-B2"); err != nil {
		t.Fatalf("UpdateSourceNode: %v", err)
	}
	if err := orders.UpdateDeliveryNode(db, o.ID, "LINE2-IN"); err != nil {
		t.Fatalf("UpdateDeliveryNode: %v", err)
	}
	if err := orders.UpdatePriority(db, o.ID, 42); err != nil {
		t.Fatalf("UpdatePriority: %v", err)
	}
	if err := orders.UpdatePayloadCode(db, o.ID, "PART-B"); err != nil {
		t.Fatalf("UpdatePayloadCode: %v", err)
	}
	if err := orders.UpdateWaitIndex(db, o.ID, 3); err != nil {
		t.Fatalf("UpdateWaitIndex: %v", err)
	}

	got, _ := orders.Get(db, o.ID)
	if got.SourceNode != "STORAGE-B2" {
		t.Errorf("SourceNode = %q, want STORAGE-B2", got.SourceNode)
	}
	if got.DeliveryNode != "LINE2-IN" {
		t.Errorf("DeliveryNode = %q, want LINE2-IN", got.DeliveryNode)
	}
	if got.Priority != 42 {
		t.Errorf("Priority = %d, want 42", got.Priority)
	}
	if got.PayloadCode != "PART-B" {
		t.Errorf("PayloadCode = %q, want PART-B", got.PayloadCode)
	}
	if got.WaitIndex != 3 {
		t.Errorf("WaitIndex = %d, want 3", got.WaitIndex)
	}
}

// -------- ListByStation ---------------------------------------------------

func TestListByStation(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, station string) {
		o := newPendingOrder(uuid)
		o.StationID = station
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
	}
	mk("a", "line-1")
	mk("b", "line-1")
	mk("c", "line-2")

	got, err := orders.ListByStation(db, "line-1", 100)
	if err != nil {
		t.Fatalf("ListByStation: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("station=line-1 len = %d, want 2", len(got))
	}
	for _, o := range got {
		if o.StationID != "line-1" {
			t.Errorf("StationID = %q, want line-1", o.StationID)
		}
	}

	// Limit honored.
	limited, _ := orders.ListByStation(db, "line-1", 1)
	if len(limited) != 1 {
		t.Errorf("limit=1 len = %d, want 1", len(limited))
	}
}

// -------- CountActiveByDeliveryNode / CountInFlightByDeliveryNode ---------

func TestCountByDeliveryNode(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status, dest string) int64 {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		o.DeliveryNode = dest
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
		return o.ID
	}
	mk("q", "queued", "LINE1-IN")
	mk("d", "dispatched", "LINE1-IN")
	mk("t", "in_transit", "LINE1-IN")
	mk("ok", "confirmed", "LINE1-IN")
	mk("x", "cancelled", "LINE1-IN")
	mk("other", "dispatched", "LINE2-IN")

	// Active excludes confirmed/failed/cancelled: queued + dispatched + in_transit = 3.
	active, err := orders.CountActiveByDeliveryNode(db, "LINE1-IN")
	if err != nil {
		t.Fatalf("CountActiveByDeliveryNode: %v", err)
	}
	if active != 3 {
		t.Errorf("CountActiveByDeliveryNode(LINE1-IN) = %d, want 3", active)
	}

	// InFlight also excludes queued: dispatched + in_transit = 2.
	inFlight, err := orders.CountInFlightByDeliveryNode(db, "LINE1-IN")
	if err != nil {
		t.Fatalf("CountInFlightByDeliveryNode: %v", err)
	}
	if inFlight != 2 {
		t.Errorf("CountInFlightByDeliveryNode(LINE1-IN) = %d, want 2", inFlight)
	}

	// Unknown node returns 0, no error.
	zero, err := orders.CountActiveByDeliveryNode(db, "DOES-NOT-EXIST")
	if err != nil {
		t.Fatalf("CountActiveByDeliveryNode unknown: %v", err)
	}
	if zero != 0 {
		t.Errorf("unknown node count = %d, want 0", zero)
	}
}

// -------- ListDispatchedVendorOrderIDs ------------------------------------

func TestListDispatchedVendorOrderIDs(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status, vendorID string) int64 {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
		if vendorID != "" {
			if err := orders.UpdateVendor(db, o.ID, vendorID, "RUNNING", ""); err != nil {
				t.Fatalf("UpdateVendor %s: %v", uuid, err)
			}
		}
		return o.ID
	}
	mk("d1", "dispatched", "rds-1")
	mk("d2", "in_transit", "rds-2")
	mk("d3", "staged", "rds-3")
	mk("done", "confirmed", "rds-done")   // excluded: terminal
	mk("pending", "pending", "rds-pend")  // excluded: wrong status
	mk("no-vendor", "dispatched", "")     // excluded: empty vendor_order_id

	ids, err := orders.ListDispatchedVendorOrderIDs(db)
	if err != nil {
		t.Fatalf("ListDispatchedVendorOrderIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("len = %d, want 3; got %v", len(ids), ids)
	}
	want := map[string]bool{"rds-1": false, "rds-2": false, "rds-3": false}
	for _, id := range ids {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected vendor id %q", id)
			continue
		}
		want[id] = true
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("missing vendor id %q", id)
		}
	}
}

// -------- ListActiveBySourceRef -------------------------------------------

func TestListActiveBySourceRef(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status, src string) int64 {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		o.SourceNode = src
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
		return o.ID
	}
	mk("p", "pending", "STORAGE-A1")
	mk("s", "sourcing", "STORAGE-A1")
	mk("q", "queued", "STORAGE-A1")
	mk("d", "dispatched", "STORAGE-A1") // excluded: past pre-dispatch
	mk("p2", "pending", "STORAGE-B2")
	mk("p3", "pending", "STORAGE-C3") // excluded: source not in filter

	got, err := orders.ListActiveBySourceRef(db, []string{"STORAGE-A1", "STORAGE-B2"})
	if err != nil {
		t.Fatalf("ListActiveBySourceRef: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("len = %d, want 4", len(got))
	}
	for _, o := range got {
		if o.Status != "pending" && o.Status != "sourcing" && o.Status != "queued" {
			t.Errorf("unexpected status %q", o.Status)
		}
		if o.SourceNode != "STORAGE-A1" && o.SourceNode != "STORAGE-B2" {
			t.Errorf("unexpected source %q", o.SourceNode)
		}
	}

	// Empty names short-circuits to nil, nil.
	none, err := orders.ListActiveBySourceRef(db, nil)
	if err != nil {
		t.Fatalf("ListActiveBySourceRef nil: %v", err)
	}
	if none != nil {
		t.Errorf("expected nil slice for empty names, got %d rows", len(none))
	}
}

// -------- ListQueued (FIFO, oldest first) --------------------------------

func TestListQueued(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	mk := func(uuid, status string) int64 {
		o := newPendingOrder(uuid)
		o.Status = protocol.Status(status)
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create %s: %v", uuid, err)
		}
		return o.ID
	}
	id1 := mk("q1", "queued")
	time.Sleep(10 * time.Millisecond) // ensure different created_at for FIFO
	id2 := mk("q2", "queued")
	mk("p", "pending")   // not queued
	mk("c", "confirmed") // not queued

	got, err := orders.ListQueued(db)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != id1 || got[1].ID != id2 {
		t.Errorf("FIFO order wrong: got [%d, %d], want [%d, %d]",
			got[0].ID, got[1].ID, id1, id2)
	}
}

// -------- UpdateBinID + ListByBinID --------------------------------------

func TestUpdateBinIDAndListByBinID(t *testing.T) {
	d := testdb.Open(t)
	fx := testdb.SetupStandardData(t, d)
	db := d.DB

	bin := testdb.CreateBinAtNode(t, d, "PART-A", fx.StorageNode.ID, "BIN-X")

	o := newPendingOrder("bin-owner")
	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create order: %v", err)
	}

	if err := orders.UpdateBinID(db, o.ID, bin.ID); err != nil {
		t.Fatalf("UpdateBinID: %v", err)
	}
	got, _ := orders.Get(db, o.ID)
	if got.BinID == nil || *got.BinID != bin.ID {
		t.Errorf("BinID after update = %v, want %d", got.BinID, bin.ID)
	}

	list, err := orders.ListByBinID(db, bin.ID, 10)
	if err != nil {
		t.Fatalf("ListByBinID: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	if list[0].ID != o.ID {
		t.Errorf("ListByBinID[0].ID = %d, want %d", list[0].ID, o.ID)
	}

	// Unknown bin_id returns empty.
	empty, err := orders.ListByBinID(db, 999999, 10)
	if err != nil {
		t.Fatalf("ListByBinID unknown: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("unknown bin len = %d, want 0", len(empty))
	}
}

// -------- Order <-> Bin junction: InsertOrderBin, ListOrderBins, DeleteOrderBins

func TestOrderBinsJunction(t *testing.T) {
	d := testdb.Open(t)
	fx := testdb.SetupStandardData(t, d)
	db := d.DB

	bin1 := testdb.CreateBinAtNode(t, d, "PART-A", fx.StorageNode.ID, "BIN-1")
	bin2 := testdb.CreateBinAtNode(t, d, "PART-A", fx.StorageNode.ID, "BIN-2")

	o := newPendingOrder("junction")
	o.OrderType = "compound"
	if err := orders.Create(db, o); err != nil {
		t.Fatalf("Create order: %v", err)
	}

	// Insert rows out of step_index order; ListOrderBins must sort ascending.
	if err := orders.InsertOrderBin(db, o.ID, bin2.ID, 2, "deliver", "STORAGE-A1", "LINE1-IN"); err != nil {
		t.Fatalf("InsertOrderBin bin2: %v", err)
	}
	if err := orders.InsertOrderBin(db, o.ID, bin1.ID, 1, "pick", "STORAGE-A1", ""); err != nil {
		t.Fatalf("InsertOrderBin bin1: %v", err)
	}

	list, err := orders.ListOrderBins(db, o.ID)
	if err != nil {
		t.Fatalf("ListOrderBins: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	// Sorted by step_index ascending.
	if list[0].StepIndex != 1 || list[1].StepIndex != 2 {
		t.Errorf("step_index order wrong: [%d, %d], want [1, 2]",
			list[0].StepIndex, list[1].StepIndex)
	}
	if list[0].BinID != bin1.ID {
		t.Errorf("list[0].BinID = %d, want %d", list[0].BinID, bin1.ID)
	}
	if list[0].Action != "pick" {
		t.Errorf("list[0].Action = %q, want pick", list[0].Action)
	}
	if list[0].NodeName != "STORAGE-A1" {
		t.Errorf("list[0].NodeName = %q, want STORAGE-A1", list[0].NodeName)
	}
	if list[1].BinID != bin2.ID {
		t.Errorf("list[1].BinID = %d, want %d", list[1].BinID, bin2.ID)
	}
	if list[1].Action != "deliver" {
		t.Errorf("list[1].Action = %q, want deliver", list[1].Action)
	}
	if list[1].DestNode != "LINE1-IN" {
		t.Errorf("list[1].DestNode = %q, want LINE1-IN", list[1].DestNode)
	}
	for _, ob := range list {
		if ob.OrderID != o.ID {
			t.Errorf("OrderID = %d, want %d", ob.OrderID, o.ID)
		}
		if ob.CreatedAt.IsZero() {
			t.Errorf("CreatedAt zero for step %d", ob.StepIndex)
		}
	}

	// Unknown order returns empty.
	none, err := orders.ListOrderBins(db, 999999)
	if err != nil {
		t.Fatalf("ListOrderBins unknown: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown-order len = %d, want 0", len(none))
	}

	// DeleteOrderBins removes all rows for the order.
	orders.DeleteOrderBins(db, o.ID)
	after, err := orders.ListOrderBins(db, o.ID)
	if err != nil {
		t.Fatalf("ListOrderBins post-delete: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("post-delete len = %d, want 0", len(after))
	}
}

// -------- ScanOrder / ScanOrders: raw-row consumption --------------------

func TestScanOrders_DirectSQL(t *testing.T) {
	d := testdb.Open(t)
	db := d.DB

	for i := 1; i <= 3; i++ {
		o := newPendingOrder("scan-" + string(rune('0'+i)))
		if err := orders.Create(db, o); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// Exercise ScanOrder via a single-row QueryRow.
	row := db.QueryRow(`SELECT ` + orders.SelectCols + ` FROM orders ORDER BY id DESC LIMIT 1`)
	o, err := orders.ScanOrder(row)
	if err != nil {
		t.Fatalf("ScanOrder single: %v", err)
	}
	if o.EdgeUUID == "" {
		t.Error("ScanOrder: EdgeUUID should not be empty")
	}

	// Exercise ScanOrders via a multi-row Query.
	rows, err := db.Query(`SELECT ` + orders.SelectCols + ` FROM orders ORDER BY id`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	all, err := orders.ScanOrders(rows)
	if err != nil {
		t.Fatalf("ScanOrders: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ScanOrders len = %d, want 3", len(all))
	}
}
