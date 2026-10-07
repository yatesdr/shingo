package store

import (
	"fmt"
	"testing"
	"time"

	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// orders_page_pins_test.go — LC2 (R25). The Orders page's All tab and status
// pills read every order the Edge ever made and filtered the pill in Go; the
// Edge never deletes orders, so that grew for the life of the Pi. They now read
// one page (newest 100) with the status filter in the SQL, through ListPage.
// List and ListByProcess are unchanged: engine tests call them.

// pageFixture seeds 250 orders, one second apart (id 1 oldest), across two
// processes: process A gets i%5 != 0 (200 rows), process B the rest (50).
// Status: confirmed when i is even (125), failed when i%4 == 1, queued otherwise.
type pageFixture struct {
	db       *DB
	procA    int64
	procB    int64
	byStatus map[string]int
	byProcA  map[string]int
}

func seedOrdersPage(t *testing.T) *pageFixture {
	t.Helper()
	db := coverageDB(t)
	f := &pageFixture{db: db, byStatus: map[string]int{}, byProcA: map[string]int{}}
	var err error
	f.procA, err = db.CreateProcess("PAGE-A", "", "", "", false)
	if err != nil {
		t.Fatalf("create process A: %v", err)
	}
	f.procB, err = db.CreateProcess("PAGE-B", "", "", "", false)
	if err != nil {
		t.Fatalf("create process B: %v", err)
	}
	nodeA, err := db.CreateProcessNode(processes.NodeInput{ProcessID: f.procA, CoreNodeName: "PG_A", Code: "PA", Name: "PG_A", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node A: %v", err)
	}
	nodeB, err := db.CreateProcessNode(processes.NodeInput{ProcessID: f.procB, CoreNodeName: "PG_B", Code: "PB", Name: "PG_B", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node B: %v", err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 250; i++ {
		status := "queued"
		switch {
		case i%2 == 0:
			status = "confirmed"
		case i%4 == 1:
			status = "failed"
		}
		node := nodeA
		if i%5 == 0 {
			node = nodeB
		} else {
			f.byProcA[status]++
		}
		f.byStatus[status]++
		at := base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`INSERT INTO orders (uuid, order_type, status, process_node_id, created_at, updated_at)
			VALUES (?, 'retrieve', ?, ?, ?, ?)`, fmt.Sprintf("page-%03d", i), status, node, at, at); err != nil {
			t.Fatalf("seed order %d: %v", i, err)
		}
	}
	return f
}

// uuidNum reads the seed index back out of a fixture uuid.
func uuidNum(t *testing.T, o orders.Order) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(o.UUID, "page-%03d", &n); err != nil {
		t.Fatalf("fixture uuid %q: %v", o.UUID, err)
	}
	return n
}

// TestPinOrders_ListReturnsEveryRow pins what does NOT move: List and
// ListByProcess still return every row, newest first.
func TestPinOrders_ListReturnsEveryRow(t *testing.T) {
	f := seedOrdersPage(t)
	all, err := f.db.ListOrders()
	if err != nil {
		t.Fatalf("ListOrders: %v", err)
	}
	if len(all) != 250 {
		t.Fatalf("ListOrders = %d rows, want 250 (every order)", len(all))
	}
	if n := uuidNum(t, all[0]); n != 250 {
		t.Errorf("ListOrders first row = %d, want 250 (newest first)", n)
	}
	byA, err := f.db.ListOrdersByProcess(f.procA)
	if err != nil {
		t.Fatalf("ListOrdersByProcess: %v", err)
	}
	if len(byA) != 200 {
		t.Errorf("ListOrdersByProcess(A) = %d rows, want 200", len(byA))
	}
}

// TestPinOrders_PageNewestHundred: the All tab reads 100 rows a page, newest
// first, and says how many there are.
func TestPinOrders_PageNewestHundred(t *testing.T) {
	f := seedOrdersPage(t)
	for _, tc := range []struct {
		offset, rows, first, last int
	}{
		{0, 100, 250, 151},
		{100, 100, 150, 51},
		{200, 50, 50, 1},
		{300, 0, 0, 0},
	} {
		page, total, err := f.db.ListOrdersPage(orders.PageQuery{Limit: 100, Offset: tc.offset})
		if err != nil {
			t.Fatalf("offset %d: %v", tc.offset, err)
		}
		if total != 250 {
			t.Errorf("offset %d: total = %d, want 250", tc.offset, total)
		}
		if len(page) != tc.rows {
			t.Fatalf("offset %d: %d rows, want %d", tc.offset, len(page), tc.rows)
		}
		if tc.rows == 0 {
			continue
		}
		if a, b := uuidNum(t, page[0]), uuidNum(t, page[len(page)-1]); a != tc.first || b != tc.last {
			t.Errorf("offset %d: rows %d..%d, want %d..%d (newest first)", tc.offset, a, b, tc.first, tc.last)
		}
	}
}

// TestPinOrders_PageStatusFilterInSQL: a status pill reads only its own rows,
// and the process filter applies with it.
func TestPinOrders_PageStatusFilterInSQL(t *testing.T) {
	f := seedOrdersPage(t)
	page, total, err := f.db.ListOrdersPage(orders.PageQuery{Status: "confirmed", Limit: 100})
	if err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if total != f.byStatus["confirmed"] || total != 125 {
		t.Errorf("confirmed total = %d, want %d", total, f.byStatus["confirmed"])
	}
	if len(page) != 100 {
		t.Errorf("confirmed page = %d rows, want 100", len(page))
	}
	for _, o := range page {
		if o.Status != "confirmed" {
			t.Fatalf("confirmed page carries a %s row (%s)", o.Status, o.UUID)
		}
	}
	if n := uuidNum(t, page[0]); n != 250 {
		t.Errorf("confirmed page first row = %d, want 250", n)
	}

	page, total, err = f.db.ListOrdersPage(orders.PageQuery{Status: "failed", ProcessID: f.procA, Limit: 100})
	if err != nil {
		t.Fatalf("failed + process A: %v", err)
	}
	if total != f.byProcA["failed"] || len(page) != total {
		t.Errorf("failed in A: total %d, rows %d, want %d", total, len(page), f.byProcA["failed"])
	}
	for _, o := range page {
		if o.Status != "failed" || o.ProcessName != "PAGE-A" {
			t.Fatalf("failed+A page carries %s / %q (%s)", o.Status, o.ProcessName, o.UUID)
		}
	}
}

// TestPinOrders_PageCount: the total is the filter's whole match, whatever the
// page size or offset.
func TestPinOrders_PageCount(t *testing.T) {
	f := seedOrdersPage(t)
	for _, tc := range []struct {
		q    orders.PageQuery
		want int
	}{
		{orders.PageQuery{Limit: 10, Offset: 240}, 250},
		{orders.PageQuery{Limit: 0}, 250},
		{orders.PageQuery{ProcessID: f.procB, Limit: 100}, 50},
		{orders.PageQuery{Status: "queued", Limit: 5}, f.byStatus["queued"]},
		{orders.PageQuery{Status: "no-such-status", Limit: 100}, 0},
	} {
		page, total, err := f.db.ListOrdersPage(tc.q)
		if err != nil {
			t.Fatalf("%+v: %v", tc.q, err)
		}
		if total != tc.want {
			t.Errorf("%+v: total = %d, want %d", tc.q, total, tc.want)
		}
		if tc.q.Limit == 0 && len(page) != 0 {
			t.Errorf("Limit 0 returned %d rows, want none", len(page))
		}
	}
}
