//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
)

// TestC2bPin_AGateRebindStampsTheBindMoment is F4's stamp. The swap's lane drop
// names the shallower slot, so the gate's re-bind moves it to the deeper one.
// The burial shadow asks when the selector last looked at the slot a bin was set
// down in; the re-bind is that look and used to leave no stamp, so a placement it
// re-bound was dated by a later, unrelated in_transit row (order 233, read as a
// GUARD BYPASS).
//
// MUTATION: drop stampBound from rebindGatedDropoff; destination_resolved_at
// stays NULL.
func TestC2bPin_AGateRebindStampsTheBindMoment(t *testing.T) {
	t.Parallel()
	shallower := func(db *store.DB, laneD int64, slots []*nodes.Node) *nodes.Node {
		best, err := db.FindStoreSlotInLaneExcluding(laneD, 0)
		testutil.MustNoErr(t, err, "the lane's own pick")
		for _, s := range slots {
			if s.ID != best.ID {
				return s
			}
		}
		t.Fatal("fixture: lane D has one slot")
		return nil
	}
	f := newC2bSwapDroppingAt(t, shallower)
	before := time.Now().Add(-time.Second)
	at, err := f.db.GetOrder(f.swap.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	f.d.HandleOrderRelease(f.d.syntheticEnvelope(at.StationID), &protocol.OrderRelease{OrderUUID: at.EdgeUUID})

	stamped, ok, err := f.db.OrderDestinationResolvedAt(f.swap.ID)
	testutil.MustNoErr(t, err, "read the stamp")
	if !ok || stamped.Before(before) {
		t.Fatalf("the gate re-bound the swap's lane drop and left the bind moment %v (ok %v), want a stamp "+
			"at the re-bind", stamped, ok)
	}
}
