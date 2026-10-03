//go:build docker

package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// A STAGED SPARE IS NOT A SOURCE. A line that keeps a spare stands it on the
// claim's inbound staging node, and the spare comes from the claim's inbound
// source. A bin left on a robot by a cancelled order goes back where the claim
// says the part is sourced from, never to staging.

// The cancelled order was lifting the spare off its spot (a keep-staged
// refill's or swap supply's pickup). The return goes to the claim's source
// group, not back onto the spot it was lifted from.
func TestCancelReturn_ASpareLiftedOffItsSpotGoesToTheSourceNotTheSpot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	market, slot := storeGroupWithSlot(t, db, "KSS")
	spot := &nodes.Node{Name: "KSS-SPOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spot), "the spot")
	seedClaim(t, db, "PROC-KSS", "STY", "LINE-KSS", "KSS-P", market.Name, true)
	bin, carrier := seedCancelledCarry(t, db, "AMR-KSS", "KSS-P", "LINE-KSS")
	_, err := db.DB.Exec(`UPDATE orders SET source_node=$1 WHERE id=$2`, spot.Name, carrier.ID)
	testutil.MustNoErr(t, err, "the cancelled order lifted the spare off the spot")
	cacheRobot(eng, loadedDispatchable("AMR-KSS"))

	eng.sweepCarriedBins()

	ret := theReturn(t, db, bin.ID)
	if ret.DeliveryNode != slot.Name {
		t.Errorf("the spare's return goes to %s, want %s in the claim's source %s, not the spot %s",
			ret.DeliveryNode, slot.Name, market.Name, spot.Name)
	}
}

// A claim that names a staging node as its inbound source declares a lineside
// position, not a store: the return holds, and the hold sentence names the node.
func TestCancelReturn_AStagingNodeNamedAsTheSourceHolds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	staging := &nodes.Node{Name: "KSH-STAGING", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(staging), "the staging node")
	seedClaim(t, db, "PROC-KSH", "STY", "LINE-KSH", "KSH-P", staging.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-KSH", "KSH-P", "LINE-KSH")
	cacheRobot(eng, loadedDispatchable("AMR-KSH"))
	before := orderCount(t, db)

	eng.sweepCarriedBins()

	assertNoRecoveryOrder(t, db, bin.ID)
	if after := orderCount(t, db); after != before {
		t.Errorf("orders %d -> %d: a hold creates nothing", before, after)
	}
	action, _, detail := lastBinAction(t, db, bin.ID)
	want := "KSH-STAGING: it is a single position, not a store group or a loader position"
	if action != "carried_bin_return_held" || !strings.Contains(detail, want) {
		t.Errorf("last recovery action = %s %q, want carried_bin_return_held naming %q", action, detail, want)
	}
}
