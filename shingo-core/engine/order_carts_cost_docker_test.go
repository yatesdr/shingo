//go:build docker

package engine

// order_carts_cost_docker_test.go — the order-carts stream's cost pins on the
// fleet path (BRIEF order-carts v1, U0). Each path is measured on an unstarted
// engine opened on a counting handle to the fixture's database, so no
// background loop lands in the count. The constants are main f8f22f19's; each
// moves to its budget in the commit that changes the path, and EVIDENCE.md in
// the design folder records both.

import (
	"strconv"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

const (
	// costLiftResolve: the lift resolve, the order loaded with its carts: one read
	// of which candidate cart stands at the block's node. Main f8f22f19: 3
	// (ListOrderBins, GetBin, GetNodeByDotName; 6 on its bin_id fallback).
	costLiftResolve = 1
	// costSetDownResolve: the set-down resolve: one read of which candidate cart
	// is in hand. Main: 2 (GetNodeByDotName _TRANSIT, ListBinsByClaim).
	costSetDownResolve = 1
	// costFinished: handleOrderDelivered for the two-cart swap with A set down
	// and B in hand. Budget: no more than today.
	costFinished = 24
	// costTerminalize: TerminalizeOrder on a live order that wins (BEGIN, the
	// status, the history row, the anomaly stamp, the claim release, the slot
	// release, the reservations, COMMIT). Main f8f22f19: 9, with the junction
	// DELETE the end no longer makes.
	costTerminalize = 8
)

// countingCartsSwap is the swap on a database with a counting handle beside
// it, and an unstarted engine on that handle.
func countingCartsSwap(t *testing.T, uuid string) (*cartsSwap, *Engine, *store.QueryCounter, *store.DB) {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	c := newCartsSwapOn(t, db, uuid)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open the counting handle")
	t.Cleanup(func() { cdb.Close() })
	return c, newUnstartedEngine(t, cdb, c.sim), counter, cdb
}

func TestOrderCartsCost_LiftResolve(t *testing.T) {
	t.Parallel()
	c, eng, counter, cdb := countingCartsSwap(t, "carts-cost-lift")
	o, err := cdb.GetOrder(c.order.ID)
	testutil.MustNoErr(t, err, "load the swap with its carts")
	counter.Reset()
	bin, _, _, ok := eng.resolvePickupBinRow(o, c.p.Name)
	n := counter.Count()
	if !ok || bin.ID != c.a.ID {
		t.Fatalf("the lift at %s resolved ok=%v bin=%v, want A (%d)", c.p.Name, ok, bin, c.a.ID)
	}
	t.Logf("COST: lift resolve %d statements: %s", n, testutil.StatementBreakdown(counter.Statements()))
	if n != costLiftResolve {
		t.Fatalf("the lift resolve took %d statements, pinned at %d", n, costLiftResolve)
	}
}

func TestOrderCartsCost_SetDownResolve(t *testing.T) {
	t.Parallel()
	c, eng, counter, cdb := countingCartsSwap(t, "carts-cost-setdown")
	c.block(t, c.p.Name, true)
	o, err := cdb.GetOrder(c.order.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	counter.Reset()
	bin, ok := eng.resolveDropoffBin(o, c.w.Name)
	n := counter.Count()
	if !ok || bin != c.a.ID {
		t.Fatalf("the set-down at %s resolved ok=%v bin=%d, want A (%d)", c.w.Name, ok, bin, c.a.ID)
	}
	t.Logf("COST: set-down resolve %d statements: %s", n, testutil.StatementBreakdown(counter.Statements()))
	if n != costSetDownResolve {
		t.Fatalf("the set-down resolve took %d statements, pinned at %d", n, costSetDownResolve)
	}
}

func TestOrderCartsCost_Finished(t *testing.T) {
	t.Parallel()
	c, eng, counter, cdb := countingCartsSwap(t, "carts-cost-finished")
	c.block(t, c.p.Name, true)
	c.block(t, c.w.Name, false)
	c.block(t, c.s.Name, true)
	o, err := cdb.GetOrder(c.order.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	counter.Reset()
	eng.handleOrderDelivered(o)
	quiesce(t, counter) // the landing announces itself after the settle returns
	n := counter.Count()
	if got := binNodeName(t, cdb, c.b.ID); got != c.p.Name {
		t.Fatalf("FINISHED left B at %s, want %s", got, c.p.Name)
	}
	all := counter.Statements()
	t.Logf("COST: FINISHED %d statements: %s", n, testutil.StatementBreakdown(all[:n]))
	if len(all) > int(n) {
		t.Logf("COST: FINISHED tail after the settle went quiet: %q", all[n:])
	}
	if n != costFinished {
		t.Fatalf("FINISHED took %d statements, pinned at %d", n, costFinished)
	}
}

func TestOrderCartsCost_Terminalize(t *testing.T) {
	t.Parallel()
	c, _, counter, cdb := countingCartsSwap(t, "carts-cost-terminalize")
	counter.Reset()
	won, err := cdb.TerminalizeOrder(c.order.ID, protocol.StatusCancelled, "cost pin")
	n := counter.Count()
	if err != nil || !won {
		t.Fatalf("terminalize won=%v err=%v", won, err)
	}
	t.Logf("COST: terminalize %d statements: %s", n, testutil.StatementBreakdown(counter.Statements()))
	if n != costTerminalize {
		t.Fatalf("terminalize took %d statements, pinned at %d", n, costTerminalize)
	}
}

// TestOrderCartsCost_TheSweepsPerCartReadIsIndexed: the stranded sweep asks
// every 2 s, per anomaly cart, for the cart's last order; that read finds the
// cart by an index. RED at main: it filters orders on bin_id, which has no
// index (orders.ListByBinID; store/schema has no index on orders.bin_id).
func TestOrderCartsCost_TheSweepsPerCartReadIsIndexed(t *testing.T) {
	t.Parallel()
	c, eng, counter, cdb := countingCartsSwap(t, "carts-cost-sweep")
	counter.Reset()
	eng.lastClaimingOrder(c.a.ID)
	stmts := counter.Statements()
	if len(stmts) != 1 {
		t.Fatalf("the per-cart read took %d statements, want 1: %s", len(stmts), testutil.StatementBreakdown(stmts))
	}
	tx, err := cdb.Begin()
	testutil.MustNoErr(t, err, "begin")
	defer tx.Rollback() //nolint:errcheck // read-only
	_, err = tx.Exec(`SET LOCAL enable_seqscan = off`)
	testutil.MustNoErr(t, err, "prefer an index")
	stmt := strings.NewReplacer("$1", strconv.FormatInt(c.a.ID, 10), "$2", "1").Replace(stmts[0])
	rows, err := tx.Query(`EXPLAIN ` + stmt)
	testutil.MustNoErr(t, err, "explain the per-cart read")
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		testutil.MustNoErr(t, rows.Scan(&line), "scan the plan")
		plan = append(plan, line)
	}
	text := strings.Join(plan, "\n")
	t.Logf("COST: the per-cart read's plan:\n%s", text)
	if !strings.Contains(text, "Index Cond: (bin_id = ") {
		t.Fatalf("the per-cart read does not find the cart by an index")
	}
}
