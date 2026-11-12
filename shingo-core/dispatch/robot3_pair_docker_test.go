//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// ROBOT 3 AT CORE. A press whose back a third trip of its own refills
// (the Edge's refill_by own_robot) sends Core an ordinary pair in which
// neither leg fetches, and the back's carrier as a plain retrieve naming the
// press, made when Robot 2 lifts the back. Core needs no third coordinated
// leg: these pin that the pair rule and the coming read already serve it.

// submitRobot3Pair sends the steady pair of a press whose back Robot 3
// refills: R1 lifts the press's carrier to the outbound and stops; R2 indexes
// the on-deck onto the press. Each leg names the other.
func (c vpCell) submitRobot3Pair(prefix string) (r1, r2 string) {
	r1, r2 = prefix+"-r1", prefix+"-r2"
	c.submitLeg(r1, r2, c.origin, prWait(c.press.Name), prPick(c.press.Name), prDrop(c.out.Name))
	c.submitLeg(r2, r1, c.origin, prWait(c.deck.Name), c.pickDeck(), prDrop(c.press.Name))
	return r1, r2
}

// TestRobot3Pair_ThePairWithNoRefillAdmitsInOnePass: with a carrier on deck,
// both legs reach the fleet in one scan pass. Neither leg fetches, so nothing
// is sourced from a market and nothing waits for one.
func TestRobot3Pair_ThePairWithNoRefillAdmitsInOnePass(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "R3A", protocol.ClaimRoleProduce)
	c.carrierAt(c.deck, "R3A-ON-DECK")
	r1, r2 := c.submitRobot3Pair("R3A")
	prScanPass(t, c.d, c.db, r1, r2)

	a, b := prReloadUUID(t, c.db, r1), prReloadUUID(t, c.db, r2)
	if a.VendorOrderID == "" || b.VendorOrderID == "" {
		t.Fatalf("the Robot 3 pair did not go in one pass: R1 %s vendor=%q cause=%q, R2 %s vendor=%q cause=%q",
			a.Status, a.VendorOrderID, a.QueueCause, b.Status, b.VendorOrderID, b.QueueCause)
	}
}

// TestRobot3Pair_ACarrierComingToTheBackHoldsTheNextPair: Robot 3's carrier
// still travelling to a vacant back (a plain retrieve naming the press) holds
// the next pair on waiting_for_outside, both legs, rather than letting Robot 2
// fetch one of its own from the market. Once it is no longer coming, the pair
// goes.
func TestRobot3Pair_ACarrierComingToTheBackHoldsTheNextPair(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "R3B", protocol.ClaimRoleProduce)
	coming := c.liveOrder("R3B-robot3", protocol.OrderTypeRetrieve, protocol.StatusInTransit, c.deck.Name, nil)
	if coming.ProcessNode != c.press.Name {
		t.Fatalf("fixture: the coming carrier names press %q, want %q (Robot 3's names its press)", coming.ProcessNode, c.press.Name)
	}
	r1, r2 := c.submitRobot3Pair("R3B")
	c.scan(r1, r2)
	c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)

	_, err := c.db.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, coming.ID)
	testutil.MustNoErr(t, err, "the carrier is no longer coming")
	c.scan(r1, r2)
	a, b := prReloadUUID(t, c.db, r1), prReloadUUID(t, c.db, r2)
	if a.VendorOrderID == "" || b.VendorOrderID == "" {
		t.Errorf("with nothing coming the pair still holds: R1 %s cause=%q, R2 %s cause=%q",
			a.Status, a.QueueCause, b.Status, b.QueueCause)
	}
}
