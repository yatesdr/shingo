//go:build docker

package testdb_test

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// spy is a testing.TB stand-in that records what an assertion reported instead
// of failing the test that is exercising it. Every clause below is proved by
// MANUFACTURING the violating state and checking the assertion catches it — an
// assertion nobody has watched fail is a query, not a guard.
type spy struct {
	testing.TB
	errs []string
}

func (s *spy) Errorf(format string, args ...any) {
	s.errs = append(s.errs, fmt.Sprintf(format, args...))
}
func (s *spy) Fatalf(format string, args ...any) {
	s.errs = append(s.errs, fmt.Sprintf(format, args...))
}
func (s *spy) Helper() {}

// caught reports whether any recorded message contains want.
func (s *spy) caught(want string) bool {
	for _, e := range s.errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

// armorFixture builds a bin, a node and an order in one line, for the clauses
// below to break in one specific way each.
func armorFixture(t *testing.T, db *store.DB, label string, status protocol.Status) (*orders.Order, int64) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	node := &nodes.Node{Name: "ARMOR-" + label, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, node.ID, "ARMOR-BIN-"+label)
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "armor-" + label
		o.Status = status
	})
	return o, bin.ID
}

// TestArmorAssertion_CatchesAnUnarmoredDispatch is clause 1's proof: an order
// the fleet is carrying, pointing at a bin it holds in neither book.
func TestArmorAssertion_CatchesAnUnarmoredDispatch(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	o, binID := armorFixture(t, db, "unarmored", protocol.StatusDispatched)
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, binID), "point at the bin")
	_, err := db.DB.Exec(`UPDATE orders SET vendor_order_id='RDS-1' WHERE id=$1`, o.ID)
	testutil.MustNoErr(t, err, "the fleet said yes")

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if !s.caught("UNARMORED DISPATCH") {
		t.Errorf("a dispatched order holding neither book was not reported; the assertion said %v", s.errs)
	}
}

// TestArmorAssertion_ACompoundSiblingsClaimIsArmor is clause 1's SELECTIVITY
// half, and it is the one the clause was wrong about at first.
//
// A multi-step reshuffle deliberately overlaps bin claims — the same bin is
// claimed once per step and the last write stands (CreateCompoundChildren says
// so, and wiring_completion.go relies on it). So a dispatched leg whose bin is
// held by a sibling IS armored: a robot is committed and no demand outside the
// excavation can take that bin. Written without this, the clause reported the
// design as a defect on four real tests.
func TestArmorAssertion_ACompoundSiblingsClaimIsArmor(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "armor-parent" })
	legA, binID := armorFixture(t, db, "sibling-a", protocol.StatusDispatched)
	_, err := db.DB.Exec(`UPDATE orders SET parent_order_id=$1, vendor_order_id='RDS-2' WHERE id=$2`, parent.ID, legA.ID)
	testutil.MustNoErr(t, err, "make leg A a child")
	legB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "armor-sibling-b"
		o.ParentOrderID = &parent.ID
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(legA.ID, binID), "leg A points at the bin")
	// The later step's claim stands — that is the documented behaviour.
	testdb.ClaimBinForTest(t, db, binID, legB.ID)

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if s.caught("UNARMORED DISPATCH") {
		t.Errorf("a dig leg whose bin its own sibling holds was reported as unarmored. A compound "+
			"overlaps claims on purpose; asking each leg to hold its own would report every "+
			"excavation in the plant. Assertion said: %v", s.errs)
	}
}

// TestArmorAssertion_CatchesTheOldCrashSliver is clause 2's proof: dispatched,
// no vendor id, and older than one handover attempt.
func TestArmorAssertion_CatchesTheOldCrashSliver(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	o, binID := armorFixture(t, db, "sliver", protocol.StatusDispatched)
	testdb.ClaimBinForTest(t, db, binID, o.ID) // armored — clause 1 has nothing to say
	_, err := db.DB.Exec(
		`UPDATE orders SET vendor_order_id='', updated_at = NOW() - INTERVAL '30 minutes' WHERE id=$1`, o.ID)
	testutil.MustNoErr(t, err, "age the sliver")

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if !s.caught("CRASH SLIVER") {
		t.Errorf("an order dispatched half an hour ago with no vendor order was not reported; "+
			"the assertion said %v", s.errs)
	}
}

// TestArmorAssertion_AFreshSliverIsARaceNotAFinding is clause 2's selectivity.
// Inside the handover window the state is correct and expected; an assertion
// that fired on every ordinary dispatch would say nothing.
func TestArmorAssertion_AFreshSliverIsARaceNotAFinding(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	o, binID := armorFixture(t, db, "fresh", protocol.StatusDispatched)
	testdb.ClaimBinForTest(t, db, binID, o.ID)

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if s.caught("CRASH SLIVER") {
		t.Errorf("an order dispatched a moment ago was reported as a crash sliver: %v", s.errs)
	}
}

// TestArmorAssertion_CatchesTheHalfConfirmedPark is the converse clause's proof:
// a hard claim held by an order that has committed no robot, with no reservation
// of its own behind it.
func TestArmorAssertion_CatchesTheHalfConfirmedPark(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	o, binID := armorFixture(t, db, "halfpark", protocol.StatusSourcing)
	testdb.ClaimBinForTest(t, db, binID, o.ID)
	// The reservation goes; the claim stands. That is the shape.
	testutil.MustNoErr(t, reservations.Release(db.DB, o.ID, binID), "drop the paper under the claim")

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if !s.caught("HALF-CONFIRMED PARK") {
		t.Errorf("a claim held by an acquiring order with no paper behind it was not reported; "+
			"the assertion said %v", s.errs)
	}
}

// TestArmorAssertion_APaperedAcquiringClaimIsFine is the converse's selectivity:
// an order holding both books is exactly what a confirm produces.
func TestArmorAssertion_APaperedAcquiringClaimIsFine(t *testing.T) {
	t.Parallel()
	// BEFORE Open, and that ordering is load-bearing: cleanups are LIFO, and
	// DisableWedgeSweep registers one that clears the flag. Called after Open, its
	// cleanup runs BEFORE the sweep and the opt-out silently does nothing.
	testdb.DisableWedgeSweep(t, "this test MANUFACTURES the violating states so the assertions have something to catch")
	db := testdb.Open(t)

	o, binID := armorFixture(t, db, "papered", protocol.StatusSourcing)
	testdb.ClaimBinForTest(t, db, binID, o.ID)

	s := &spy{TB: t}
	testdb.AssertArmorMatchesStatus(s, db)
	if s.caught("HALF-CONFIRMED PARK") {
		t.Errorf("an acquiring order holding both books was reported: %v", s.errs)
	}
}
