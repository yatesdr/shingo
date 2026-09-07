//go:build docker

package main

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// ── THE RESIDENCE THIS ASSERTS ON IS ITS OWN ────────────────────────────────
//
// Same isolation rule as the carrier-pool fixtures: these tests share a
// database and run in parallel, and the overlap check is a question about
// EVERY node. Each case seeds its own node name and asserts only on the
// line naming it.

// mustOrder mints a real order whose steps carry its bin to `node`, plus the
// one history row the check reads — the same rows the dispatcher and the
// status pusher write on live data.
func mustOrder(t *testing.T, db *store.DB, label, node string, binID int64, stepsJSON, status string, at time.Time) {
	t.Helper()
	o := &domain.Order{
		EdgeUUID:    "soak-res-" + label,
		StationID:   "soak",
		OrderType:   protocol.OrderTypeComplex,
		Status:      protocol.Status(status),
		Quantity:    1,
		StepsJSON:   stepsJSON,
		BinID:       &binID,
		PayloadCode: "PART-R",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order %s: %v", label, err)
	}
	if _, err := db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at)
		 VALUES ($1, $2, $3, $4)`,
		o.ID, status, "residence fixture", at); err != nil {
		t.Fatalf("history row for %s: %v", label, err)
	}
}

func dropoffStep(node string) string {
	return fmt.Sprintf(`[{"action":"dropoff","node":%q}]`, node)
}

func pickupStep(node string) string {
	return fmt.Sprintf(`[{"action":"pickup","node":%q}]`, node)
}

// seedPair builds two bin lifetimes at one node. The shapes differ by ONE
// timestamp — the departure of the earlier resident:
//
//	overlapping:  bin A arrives, bin B arrives BEFORE A lifts  → violation
//	sequential:   bin A arrives, A lifts, then bin B arrives   → quiet
//
// Everything else — node, bins, steps, statuses — is identical, so the pair
// pins exactly the comparison under test and nothing else.
func seedPair(t *testing.T, db *store.DB, label string, overlap bool) (node string) {
	t.Helper()
	node = fmt.Sprintf("SOAKPOS-%s", label)
	now := time.Now().UTC()
	binA, binB := int64(9101), int64(9102)

	// The bins themselves: orders.bin_id carries an FK to bins, so the two
	// residents must exist. Their own type (the pool fixtures' isolation
	// rule, same reason) and nothing else — the check only reads order rows.
	bt := &bins.BinType{Code: fmt.Sprintf("SOAKRES-%s", label), Description: "residence fixture"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	for _, b := range []int64{binA, binB} {
		if _, err := db.DB.Exec(
			`INSERT INTO bins (id, bin_type_id, label, status) VALUES ($1, $2, $3, 'available')`,
			b, bt.ID, fmt.Sprintf("SOAK-RES-%s-%d", label, b)); err != nil {
			t.Fatalf("seed bin %d: %v", b, err)
		}
	}

	// Bin A arrives at the node; its departure is either after B's arrival
	// (overlap — the 09-07 PLN_001 shape) or before it (sequential).
	aArr := now.Add(-10 * time.Minute)
	bArr := now.Add(-5 * time.Minute)
	aDep := now // still in residence when B lands
	if !overlap {
		aDep = bArr.Add(-time.Minute) // lifted a minute before B arrives
	}

	mustOrder(t, db, label+"-arrA", node, binA, dropoffStep(node), "delivered", aArr)
	mustOrder(t, db, label+"-depA", node, binA, pickupStep(node), "confirmed", aDep)
	mustOrder(t, db, label+"-arrB", node, binB, dropoffStep(node), "delivered", bArr)
	return node
}

// TestBinResidenceOverlap_PlacingOntoAnOccupiedPosition is the shape the check
// exists for, proven real by sim 2026-09-07: the press-index index leg placed
// bin 30 on PLN_001 while bin 17 still stood there. The lane invariants were
// silent — corridors only — and twenty seconds of two bins at one position
// passed every existing assertion.
func TestBinResidenceOverlap_PlacingOntoAnOccupiedPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := seedPair(t, db, "OVR", true)

	got := checkBinResidenceOverlap(db)
	if !hasLine(got, "node "+node) {
		t.Errorf("two overlapping bin residences at %s did not trip the check.\n"+
			"This is the collision the lane checks cannot see: a placing leg that set a bin "+
			"down on an occupied position.\ngot: %v", node, got)
	}
	if !hasLine(got, "9102 arrived") {
		t.Errorf("the violation must name the arriving bin — the reader's next question is "+
			"WHICH robot placed it.\ngot: %v", got)
	}
}

// TestBinResidenceOverlap_QuietWhenSequential keeps the check from firing on a
// healthy plant: bins handed over node-by-node, each lifting before the next
// lands, are the ordinary choreography of every swap ever run.
func TestBinResidenceOverlap_QuietWhenSequential(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := seedPair(t, db, "SEQ", false)

	if got := checkBinResidenceOverlap(db); hasLine(got, "node "+node) {
		t.Errorf("a clean handover at %s was reported as an overlap. A check that fires on "+
			"ordinary swaps teaches the reader to skip its violations.\ngot: %v", node, got)
	}
}
