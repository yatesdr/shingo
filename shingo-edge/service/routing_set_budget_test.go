package service

import (
	"reflect"
	"testing"

	"shingoedge/store/processes"
)

// routing_set_budget_test.go — S0's routing-nodes row.
//
// GET /api/processes/{id}/routing-nodes answers the Routing tab, and the
// Routing tab is a tab: it is opened and re-opened while an engineer works
// through a plant's names, on the one SQLite connection the whole Edge shares.
//
// SIX, NOT FIVE, AND THE BUDGET IS THREE. S0's row reads "5 queries, 3 feed
// nothing | 2". The diagnosis was right and both numbers are one out at this
// tip: measured on the S0 fixture the route took SIX — main's five plus one,
// because this branch moved the per-row style count out of the list's SELECT
// into its own claims read (ListRoutingNodesWithCounts). Three of the six fed
// nothing the other reads could not answer, so the floor is 6 - 3 = 3, not
// 5 - 3 = 2.
//
// The three that stay are four facts from three reads: the process row (name,
// composer flag), the routing rows (the response, plus the backfill count and
// the unknown-name list read off them in Go), and the process's live claims
// (the per-row StyleCount evidence, and their length IS the report's claim
// count — the predicates are identical). Reaching two would mean dropping one
// of those facts from the response, not reading one of them better.
const routingSetQueryBudget = 3

func TestRoutingSet_QueryCount(t *testing.T) {
	t.Parallel()
	fx := seedBudgetFixture(t)
	svc := NewProcessService(fx.db)

	fx.counter.Reset()
	report, rows, err := svc.RoutingSet(fx.processID, func(string) bool { return false })
	if err != nil {
		t.Fatalf("RoutingSet: %v", err)
	}
	got := fx.counter.Count()

	if len(rows) == 0 {
		t.Fatal("the fixture's routing set is empty — this budget would pass vacuously")
	}
	// A BUDGET WITHOUT THIS INVITES MEETING IT BY SENDING LESS. The claims
	// read is a third of the route's cost and it is what fills StyleCount —
	// the "on N styles" evidence the Routing panel puts under a backfilled
	// name before an engineer adopts or parks it. Dropping it would take the
	// route to two and take the evidence with it.
	//
	// Held against the two-pass read rather than against a number: this
	// fixture's routing rows are names no claim happens to use, so every count
	// is legitimately zero and "some row has a count" would pass vacuously or
	// fail honestly depending on the seed. What must not change is that the
	// one-pass answer IS the two-pass answer.
	want, err := fx.db.ListRoutingNodesWithCounts(fx.processID)
	if err != nil {
		t.Fatalf("ListRoutingNodesWithCounts: %v", err)
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("the one-pass read returned different rows from ListRoutingNodesWithCounts.\n"+
			" one pass: %+v\n two pass: %+v", rows, want)
	}
	if report.Claims != len(claimsOf(t, fx)) {
		t.Errorf("the report counts %d live claims and the process has %d — "+
			"the count is read off the claims now, and the two predicates have to be the same one",
			report.Claims, len(claimsOf(t, fx)))
	}
	if got > routingSetQueryBudget {
		t.Errorf("the routing-nodes route issued %d queries for %d rows; the budget is %d.\n"+
			"  A query that feeds nothing into the response is a query on the one SQLite "+
			"connection the whole Edge shares. (report: %q)",
			got, len(rows), routingSetQueryBudget, report.Line())
	}
	t.Logf("routing-nodes: %d queries, %d rows", got, len(rows))
}

// claimsOf is the process's live claims, read the way the route reads them.
func claimsOf(t *testing.T, fx budgetFixture) []processes.NodeClaim {
	t.Helper()
	claims, err := processes.ListLiveClaimsByProcess(fx.db.DB, fx.processID)
	if err != nil {
		t.Fatalf("ListLiveClaimsByProcess: %v", err)
	}
	return claims
}
