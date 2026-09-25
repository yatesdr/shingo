//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// scanner_pass_budget_docker_test.go — what one fulfillment scanner pass costs
// in STATEMENTS, at a plant-shaped config. EVIDENCE, NOT A PIN.
//
// The numbers are logged under a BUDGET: prefix and recorded, with the phase
// they were taken at, in the Stage A BUDGETS.md. Nothing here asserts a count:
// a cost is measured once and written down, and a pin exists only for a
// statement a change ADDS. The unification phases change what a pass does
// (one-step acquire, then a new ladder), so this harness is the before-and-after
// instrument for that and nothing more.
//
// THE FIXTURE is shaped like a small plant rather than a unit case: six storage
// slots (four holding bins of one payload, two empty), seven line nodes, six
// retrieves to six lines and one move of a bin off a line into storage, all
// waiting for the scanner. Three passes are measured. The first finds material
// for some and not the rest, so it is the pass that dispatches; the second
// re-asks and can still dispatch an order that lost a race inside the first; the
// third is the steady state, where what is left is waiting.
//
// RUN ON DEMAND: go test -tags docker -run TestBudget_ScannerPass -v ./engine/

// quiesce waits until the counting store has been silent for a settle window, so
// a measurement starts after the engine's boot work and ends after any async
// tail a pass kicks off (event handlers, edge sends).
func quiesce(t *testing.T, c *store.QueryCounter) {
	t.Helper()
	const settle = 400 * time.Millisecond
	deadline := time.Now().Add(20 * time.Second)
	last, since := c.Count(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if n := c.Count(); n != last {
			last, since = n, time.Now()
			continue
		}
		if time.Since(since) >= settle {
			return
		}
	}
	t.Fatalf("the engine never went quiet (count still moving at %d)", c.Count())
}

func TestBudget_ScannerPass(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)

	// A market: one NGRP with six storage slots of the fixture's STOR type as
	// direct children (the flattened depth-1 shape), bins in the first four. The
	// retrieves name the group, as a plant's do, so the finder chooses the slot.
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	market := &nodes.Node{Name: "BUD-MKT", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(market), "create market group")
	var stor []*nodes.Node
	for i := 1; i <= 6; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("BUD-STOR-%d", i), Zone: "A", Enabled: true,
			NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &market.ID}
		testutil.MustNoErr(t, db.CreateNode(n), "create storage slot")
		stor = append(stor, n)
	}
	for i := 0; i < 4; i++ {
		testdb.CreateBinAtNode(t, db, sd.Payload.Code, stor[i].ID, fmt.Sprintf("BUD-BIN-%d", i+1))
	}
	var lines []*nodes.Node
	for i := 1; i <= 7; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("BUD-LINE-%d", i), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "create line node")
		lines = append(lines, n)
	}
	moveBin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, lines[6].ID, "BUD-BIN-MOVE")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })

	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, cdb, backend)
	quiesce(t, counter)

	counter.Reset()
	time.Sleep(time.Second)
	t.Logf("BUDGET: background noise over 1s of an idle started engine: %d statements", counter.Count())

	// The orders go in AFTER Start, through the plain handle and with no event,
	// so Start's own boot scan cannot take the dispatching pass, and the next
	// scan the engine would run by itself is the 60s sweep.
	for i := 0; i < 6; i++ {
		testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("bud-retrieve-%d", i+1)
			o.OrderType = dispatch.OrderTypeRetrieve
			o.Status = protocol.StatusQueued
			o.PayloadCode = sd.Payload.Code
			o.SourceNode = market.Name
			o.DeliveryNode = lines[i].Name
		})
	}
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "bud-move"
		o.OrderType = dispatch.OrderTypeMove
		o.Status = protocol.StatusQueued
		o.PayloadCode = sd.Payload.Code
		o.SourceNode = lines[6].Name
		o.DeliveryNode = stor[4].Name
		o.BinID = &moveBin.ID
	})

	measure := func(name string) {
		t.Helper()
		before := len(backend.CreateRequests())
		counter.Reset()
		eng.RunFulfillmentScan()
		sync := counter.Statements()
		quiesce(t, counter)
		all := counter.Statements()
		t.Logf("BUDGET: %s — %d dispatched · synchronous %s", name,
			len(backend.CreateRequests())-before, testutil.StatementBreakdown(sync))
		t.Logf("BUDGET: %s — with async tail: %d statements", name, len(all))
	}

	measure("scanner pass 1 (dispatching)")
	measure("scanner pass 2 (re-ask; can still dispatch what pass 1 raced for)")
	measure("scanner pass 3 (steady state, remaining orders waiting)")

	rows, err := db.DB.Query(`SELECT edge_uuid, status, COALESCE(queue_cause,'') FROM orders ORDER BY id`)
	testutil.MustNoErr(t, err, "read outcomes")
	defer rows.Close()
	for rows.Next() {
		var uuid, status, cause string
		testutil.MustNoErr(t, rows.Scan(&uuid, &status, &cause), "scan outcome")
		t.Logf("BUDGET: outcome %s %s %s", uuid, status, cause)
	}
}
