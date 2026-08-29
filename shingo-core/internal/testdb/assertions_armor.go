package testdb

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store"
)

// crashSliverAge is how old a `dispatched` order with no vendor order id has to
// be before it is a finding rather than a race.
//
// It is FIVE MINUTES BECAUSE soakstat's checkArmoredWithNoVendorOrder IS FIVE
// MINUTES. The two are the same assertion, one against a test database and one
// against a running plant, and a bound that differed between them would mean the
// suite and the floor disagreed about what a violation is — the shape where a
// green suite certifies a plant that is reporting the fault.
//
// The window itself is one handover attempt: handoverToFleet CASes the order to
// `dispatched` and THEN asks the fleet to create, deliberately (it is the
// racing-dispatchers lock, and reordering trades a swept transient for an orphan
// vendor order). Inside that window the state is correct and expected. Five
// minutes is many multiples of one RDS request, so nothing healthy reaches it,
// and it is short enough to catch the wedge inside a shift.
const crashSliverAge = 5 * time.Minute

// AssertArmorMatchesStatus is the two-clause dispatched assertion and its
// converse: does what an order HOLDS agree with what its status CLAIMS?
//
// AssertNoPointerWedge asks the same question from the acquiring side and asks
// it about the POINTER. This asks it about the ARMOR, across the seam a fleet
// refusal crosses, and it lands with the demote door because clause 2's
// population is the door's to produce.
//
// ── CLAUSE 1: dispatched ⇒ ARMORED ────────────────────────────────────────
//
// `dispatched` means Core has claimed the order and the call to the fleet is
// made or being made. A robot is committed to a bin, so the bin is spoken for —
// by a hard claim, or by the confirmed reservation that coincides with one. An
// order that is dispatched and holds NEITHER is armor that fell off a live
// order: the bin is available to the next demand while a robot is on its way to
// collect it.
//
// SCOPED TO ORDERS THAT POINT AT A BIN, and the scope is what makes it an
// invariant rather than a style rule. An order with no bin_id has nothing this
// clause can be about — a compound parent carries no cargo, and a fixture row
// conjured at `dispatched` to exercise a status guard is not a claim about
// material. The pointer is what turns "this order is moving something" into a
// checkable fact, which is exactly the scoping AssertNoPointerWedge uses from
// the other side.
//
// AND THE COMPOUND FAMILY COUNTS AS THE HOLDER, which is not a loophole — it is
// what this codebase already does on purpose and says so: "A multi-step
// reshuffle plan INTENTIONALLY overlaps bin claims: a bin that appears in
// several steps ... is claimed once per step, and the last step's write is the
// one that stands" (store/orders.go CreateCompoundChildren), and
// engine/wiring_completion.go relies on it. So a dispatched leg whose bin is
// held by its own parent or by a sibling IS armored: a robot is committed, the
// bin is spoken for, and no demand outside the excavation can take it. Asking
// each leg to hold its own claim would report the design as a defect on every
// dig in the plant — measured, when this clause was first written without the
// clause: four tests, every one a legal re-claim inside one compound.
//
// ── CLAUSE 2: dispatched ∧ NO VENDOR ORDER ⇒ YOUNG ────────────────────────
//
// The fleet's yes is the vendor_order_id, never the status. Between the CAS and
// the create there is a window in which an order is armored and the fleet has
// not answered; a crash inside it leaves an order armored, claimed, holding its
// lane, and invisible to the one loop that would notice — the poller's
// re-registration selects non-empty vendor ids only. Nothing else in the system
// asks about the empty case.
//
// So the state is legal while YOUNG and a finding once old. See crashSliverAge.
//
// ── THE CONVERSE: a hard claim on an ACQUIRING order needs its own paper ──
//
// A hard claim means a robot is committed. An order still acquiring has not
// committed one — so a claim it holds is legal only alongside its OWN live
// reservation on that resource. Without one it is the half-confirmed park: the
// confirm wrote the claim and the reservation went, or the reservation was
// released under a claim that stood. Today that shape has no name and no reader,
// and the tier work's rank comparisons read the reservation book — so a claim
// standing outside it is a resource nobody can see is taken.
//
// SLOT CLAIMS ARE OUT OF THE CONVERSE, stated rather than omitted. Nodes are
// claimed through a sanctioned raw fixture bypass in a hundred tests
// (ClaimSlotForTest, which deliberately writes no reservation). Extending the
// converse to slots is a real question, and it is the narrowing's — the
// resource-keyed refusal in ReleaseAcquiringOrphanClaims is the other half of
// the same one.
func AssertArmorMatchesStatus(t testing.TB, db *store.DB) {
	t.Helper()
	assertDispatchedIsArmored(t, db)
	assertNoOldCrashSliver(t, db)
	assertAcquiringClaimHasItsPaper(t, db)
}

// assertDispatchedIsArmored is clause 1.
func assertDispatchedIsArmored(t testing.TB, db *store.DB) {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT o.id, o.bin_id, COALESCE(o.order_type, '')
		  FROM orders o
		 WHERE o.status = $1
		   AND o.bin_id IS NOT NULL
		   -- Held by the order, or by anyone in its compound family: the parent,
		   -- a sibling leg, or (for a parent) one of its own children. See the
		   -- note above for why the family is the right grain.
		   AND NOT EXISTS (
		       SELECT 1 FROM bins b
		        WHERE b.id = o.bin_id
		          AND (b.claimed_by = o.id
		               OR b.claimed_by = o.parent_order_id
		               OR b.claimed_by IN (
		                   SELECT s.id FROM orders s
		                    WHERE s.parent_order_id = COALESCE(o.parent_order_id, o.id)
		               ))
		   )
		   AND NOT EXISTS (
		       SELECT 1 FROM reservations r
		        WHERE r.order_id = o.id AND r.resource_kind = 'bin' AND r.bin_id = o.bin_id
		          AND r.state = 'confirmed'
		   )
		 ORDER BY o.id`, string(protocol.StatusDispatched))
	if err != nil {
		t.Fatalf("AssertArmorMatchesStatus: scan dispatched orders: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var orderID, binID int64
		var orderType string
		if err := rows.Scan(&orderID, &binID, &orderType); err != nil {
			t.Fatalf("AssertArmorMatchesStatus: scan row: %v", err)
		}
		t.Errorf("UNARMORED DISPATCH: order %d (%s) is `dispatched` toward bin %d and holds neither "+
			"the claim nor a confirmed reservation on it.\n"+
			"    A robot is committed to that bin and the bin is available to the next demand. "+
			"Find the release that ran while the order was still moving.",
			orderID, orderType, binID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertArmorMatchesStatus: dispatched orders: %v", err)
	}
}

// assertNoOldCrashSliver is clause 2.
func assertNoOldCrashSliver(t testing.TB, db *store.DB) {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT id, COALESCE(order_type, ''), EXTRACT(EPOCH FROM ($2::timestamptz - updated_at))::bigint
		  FROM orders
		 WHERE status = $1
		   AND COALESCE(vendor_order_id, '') = ''
		   AND updated_at < $2::timestamptz - ($3::int * INTERVAL '1 second')
		 ORDER BY id`,
		string(protocol.StatusDispatched), clock.Now().UTC(), int(crashSliverAge.Seconds()))
	if err != nil {
		t.Fatalf("AssertArmorMatchesStatus: scan the crash sliver: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var orderID, ageSec int64
		var orderType string
		if err := rows.Scan(&orderID, &orderType, &ageSec); err != nil {
			t.Fatalf("AssertArmorMatchesStatus: scan sliver row: %v", err)
		}
		t.Errorf("CRASH SLIVER: order %d (%s) has been `dispatched` with an empty vendor_order_id "+
			"for %s.\n"+
			"    The fleet's yes is the vendor id, never the status. Core wrote the status and the "+
			"create never landed, so this order holds its claims and its lane while nothing polls "+
			"it — the poller's re-registration selects non-empty ids only.",
			orderID, orderType, protocol.FormatDuration(time.Duration(ageSec)*time.Second))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertArmorMatchesStatus: crash sliver: %v", err)
	}
}

// assertAcquiringClaimHasItsPaper is the converse clause.
func assertAcquiringClaimHasItsPaper(t testing.TB, db *store.DB) {
	t.Helper()
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT b.id, b.claimed_by, o.status
		  FROM bins b
		  JOIN orders o ON o.id = b.claimed_by
		 WHERE o.status IN (%s)
		   AND NOT EXISTS (
		       SELECT 1 FROM reservations r
		        WHERE r.order_id = o.id AND r.resource_kind = 'bin' AND r.bin_id = b.id
		   )
		 ORDER BY b.id`, protocol.AcquiringStatusSQLList()))
	if err != nil {
		t.Fatalf("AssertArmorMatchesStatus: scan acquiring claims: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var binID, orderID int64
		var status string
		if err := rows.Scan(&binID, &orderID, &status); err != nil {
			t.Fatalf("AssertArmorMatchesStatus: scan claim row: %v", err)
		}
		t.Errorf("HALF-CONFIRMED PARK: bin %d is hard-claimed by order %d, which is still `%s` and "+
			"holds no reservation of its own on it.\n"+
			"    A hard claim means a robot is committed; an acquiring order has committed none. "+
			"The bin is taken, and the reservation book — which every rank comparison reads — "+
			"cannot see that it is.",
			binID, orderID, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertArmorMatchesStatus: acquiring claims: %v", err)
	}
}
