//go:build docker

package reconciliation_test

import (
	"bytes"
	"log"
	"strconv"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
	"shingocore/store/reservations"
)

// acquiring_orphan_claims_docker_test.go — the claim-side backstop's second arm.
//
// ── THE DRIFT IT SWEEPS ───────────────────────────────────────────────────
//
// Ownership of a bin is stored in two books: bins.claimed_by and the
// reservations row. They are coupled by convention rather than by schema, so a
// release that drops one and not the other leaves a bin CLAIMED BY A LIVE ORDER
// with nothing behind the claim. The availability predicates are owner-blind, so
// that bin is then invisible to everybody — including the order whose name is on
// it — and its own demand waits forever on a bin it already owns. It is the
// shape the sim wedged on 2026-08-28 (order 109 / bin 10) and the shape the
// ownership conversion's Stage 1 ends properly.
//
// This sweep is the insurance in the meantime (owner ruling 2026-08-28): a
// minutes-scale self-heal, LOUD, so that a plant quietly running on it is
// visible rather than comfortable. Stage 3 obsoletes it.
//
// ── AND WHAT IT MUST NEVER TOUCH ──────────────────────────────────────────
//
// A claim with any live reservation behind it is a healthy claim — that is the
// normal state of every dispatched order in the plant, and sweeping one would
// take a bin off a robot. orders.bin_id is a different book with a different fix
// and is not this sweep's business either. Terminal orders stay with the
// existing arm.

// orphanFixture builds one live acquiring order holding a bin claim with no
// reservation behind it — the drift, manufactured the way the code produces it:
// the reservation released, the claim left standing.
func orphanFixture(t *testing.T, db *store.DB, label, status string) (*bins.Bin, *orders.Order) {
	t.Helper()
	node := &nodes.Node{Name: label + "-NODE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, "ORPHAN-PART", node.ID, label+"-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = label + "-order"
		o.Status = "queued"
	})
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID) // reserve → claim → confirm
	if status != "queued" {
		testdb.SeedOrderStatus(t, db, order.ID, status, "")
	}
	// THE DRIFT. The owner-blind release deletes the reservation and leaves the
	// claim; this is that outcome, written directly so the test does not depend
	// on which of the two sites produced it today.
	if _, err := db.DB.Exec(
		`DELETE FROM reservations WHERE bin_id=$1 AND resource_kind='bin'`, bin.ID); err != nil {
		t.Fatalf("orphan the claim: %v", err)
	}
	return bin, order
}

func claimedBy(t *testing.T, db *store.DB, binID int64) *int64 {
	t.Helper()
	b, err := db.GetBin(binID)
	if err != nil {
		t.Fatalf("read bin %d: %v", binID, err)
	}
	return b.ClaimedBy
}

// TestAcquiringOrphanClaims_OneSweepFreesTheBinAndTheDemandEscapes is the whole
// point: the bin comes back, and an order can take it again through the guarded
// production path (reserve → claim), which is what "escapes" means.
func TestAcquiringOrphanClaims_OneSweepFreesTheBinAndTheDemandEscapes(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"queued", "sourcing"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			bin, order := orphanFixture(t, db, "ORPH-"+strings.ToUpper(status), status)

			if c := claimedBy(t, db, bin.ID); c == nil || *c != order.ID {
				t.Fatalf("the fixture is wrong: bin %d claimed_by=%v, want order %d", bin.ID, c, order.ID)
			}

			n, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB)
			if err != nil {
				t.Fatalf("ReleaseAcquiringOrphanClaims: %v", err)
			}
			if n != 1 {
				t.Fatalf("the sweep cleared %d claim(s), want 1. Bin %d is claimed by order %d, which is "+
					"%s and holds NO reservation on it — the two books have come apart, and nothing else "+
					"in the system will ever put them back together.", n, bin.ID, order.ID, status)
			}
			if c := claimedBy(t, db, bin.ID); c != nil {
				t.Fatalf("bin %d is still claimed by %d after the sweep", bin.ID, *c)
			}
			// AND THE DEMAND ESCAPES. Freeing the column is not the deliverable —
			// the next order getting the bin through the guarded production path
			// (reserve, then the CAS-guarded claim) is.
			taker := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = "orph-taker-" + status
			})
			testutilAcquire(t, db, taker.ID, bin.ID)
			if err := db.ClaimBin(bin.ID, taker.ID); err != nil {
				t.Fatalf("the orphaned claim is gone but the bin still could not be taken: %v", err)
			}
		})
	}
}

// TestAcquiringOrphanClaims_AHealthyClaimSurvives is the precision rule, and it
// is the one that matters: every dispatched order in the plant is a claim with a
// reservation behind it, and a sweep that took those would strip bins off robots.
func TestAcquiringOrphanClaims_AHealthyClaimSurvives(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "ORPH-OK-NODE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, "ORPHAN-PART-OK", node.ID, "ORPH-OK-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "orph-ok-order"
		o.Status = "sourcing"
	})
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID) // reservation left in place

	n, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB)
	if err != nil {
		t.Fatalf("ReleaseAcquiringOrphanClaims: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweep cleared %d claim(s) that had a live reservation behind them. That is a bin "+
			"taken off an order that legitimately owns it — the failure mode this sweep must never "+
			"have, since a claim WITH a reservation is what every healthy order in the plant looks "+
			"like.", n)
	}
	if c := claimedBy(t, db, bin.ID); c == nil || *c != order.ID {
		t.Fatalf("bin %d claimed_by=%v after the sweep, want order %d untouched", bin.ID, c, order.ID)
	}
}

// TestAcquiringOrphanClaims_ADisagreementAboutWHOIsNotSwept pins the
// resource-keyed half of the refusal, which is the one that surprises people.
//
// The sweep asks "is there ANY live reservation on this resource", not "does the
// claimant hold one". A bin claimed by order A while order B holds the live
// reservation is also drift — but it is drift about WHO, not about whether the
// claim has anything behind it, and clearing A's claim here would resolve that
// disagreement by deletion, in favour of whichever writer happened to run last.
// It is a different defect with a different fix, and the conservative direction
// is to leave it visible.
func TestAcquiringOrphanClaims_ADisagreementAboutWHOIsNotSwept(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	bin, order := orphanFixture(t, db, "ORPH-WHO", "queued")

	// Somebody else now holds the live reservation on the same bin.
	other := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "orph-who-other" })
	testutilAcquire(t, db, other.ID, bin.ID)

	n, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB)
	if err != nil {
		t.Fatalf("ReleaseAcquiringOrphanClaims: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweep cleared %d claim(s) while a live reservation stood on the resource. That is "+
			"a two-writer disagreement about WHO owns bin %d, and settling it by deleting one side "+
			"picks a winner arbitrarily.", n, bin.ID)
	}
	if c := claimedBy(t, db, bin.ID); c == nil || *c != order.ID {
		t.Fatalf("bin %d claimed_by=%v, want order %d left alone", bin.ID, c, order.ID)
	}
}

// TestAcquiringOrphanClaims_IsLoud — a self-heal nobody can see is a plant
// quietly running on its backstop. The line names the bin, the order, and what
// the sweep believes it is fixing, so that somebody counting these has something
// to count and somewhere to go.
func TestAcquiringOrphanClaims_IsLoud(t *testing.T) {
	// No t.Parallel — stdlib log.SetOutput is global.
	db := testdb.Open(t)
	bin, order := orphanFixture(t, db, "ORPH-LOUD", "queued")

	var buf bytes.Buffer
	prevW, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevFlags) })

	if _, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB); err != nil {
		t.Fatalf("ReleaseAcquiringOrphanClaims: %v", err)
	}
	logged := buf.String()
	for _, want := range []string{"ORPHANED CLAIM", "bin", "order"} {
		if !strings.Contains(logged, want) {
			t.Errorf("sweep log %q does not mention %q", logged, want)
		}
	}
	if !strings.Contains(logged, strconv.FormatInt(bin.ID, 10)) || !strings.Contains(logged, strconv.FormatInt(order.ID, 10)) {
		t.Errorf("sweep log %q names neither bin %d nor order %d — a reader cannot chase it",
			logged, bin.ID, order.ID)
	}
}

// TestAcquiringOrphanClaims_SweepsTheSlotDual — nodes.claimed_by is the other
// half of the same book and drifts the same way.
func TestAcquiringOrphanClaims_SweepsTheSlotDual(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	slot := &nodes.Node{Name: "ORPH-SLOT", Enabled: true}
	if err := nodes.Create(db.DB, slot); err != nil {
		t.Fatalf("create node: %v", err)
	}
	healthy := &nodes.Node{Name: "ORPH-SLOT-OK", Enabled: true}
	if err := nodes.Create(db.DB, healthy); err != nil {
		t.Fatalf("create node: %v", err)
	}
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "orph-slot-order"
		o.Status = "sourcing"
	})
	// Orphan: a slot claim with nothing behind it.
	testdb.ClaimSlotForTest(t, db, slot.ID, order.ID)
	// Healthy: reserved AND claimed, the ordinary destination hold.
	if err := reservations.AcquireSlot(db, order.ID, healthy.ID, "test"); err != nil {
		t.Fatalf("acquire slot: %v", err)
	}
	if err := db.ConfirmSlotClaim(healthy.ID, order.ID); err != nil {
		t.Fatalf("confirm slot claim: %v", err)
	}

	if _, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB); err != nil {
		t.Fatalf("ReleaseAcquiringOrphanClaims: %v", err)
	}
	if n, _ := nodes.Get(db.DB, slot.ID); n.ClaimedBy != nil {
		t.Errorf("orphaned slot claim survived: claimed_by=%d", *n.ClaimedBy)
	}
	if n, _ := nodes.Get(db.DB, healthy.ID); n.ClaimedBy == nil {
		t.Error("the sweep cleared a slot claim that had a live reservation behind it — a destination " +
			"taken from an order that is on its way there")
	}
}

// testutilAcquire is reservations.Acquire with the fixture's error handling.
func testutilAcquire(t *testing.T, db *store.DB, orderID, binID int64) {
	t.Helper()
	if err := reservations.Acquire(db, orderID, binID, "test"); err != nil {
		t.Fatalf("acquire bin %d for order %d: %v", binID, orderID, err)
	}
}
