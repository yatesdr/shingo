//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// A release that arrives for a leg Core has already released past its last
// station wait declares nothing. The run's shape (order 337): the first release
// pulled the bin's parts lineside and emptied it; a second press, 160 ms later
// and with no wait echo, arrived once the leg was in transit past its only wait
// and set the emptied bin back to 20, stock that does not exist.
func TestHandleOrderRelease_SecondReleasePastTheWaitDeclaresNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	order, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-second-release", "BIN-SECOND-RELEASE")

	empty := 0
	wait := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-second-release",
		RemainingUOP: &empty,
		Disposition:  &protocol.UOPDisposition{Kind: protocol.DispositionReleaseEmpty},
		CalledBy:     "operator",
		StationWait:  &wait,
	})
	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "get order")
	if got.WaitIndex != 1 {
		t.Fatalf("WaitIndex after the first release = %d, want 1", got.WaitIndex)
	}
	// The fleet picks the leg up and it drives on.
	testutil.MustNoErr(t, db.UpdateOrderStatus(order.ID, string(StatusInTransit), "test: robot driving"), "force in_transit")
	first, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if first.UOPRemaining != 0 {
		t.Fatalf("UOPRemaining after the first release = %d, want 0", first.UOPRemaining)
	}

	partial := 20
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-second-release",
		RemainingUOP: &partial,
		Disposition:  &protocol.UOPDisposition{Kind: protocol.DispositionReleasePartial, Count: 20},
		CalledBy:     "operator",
	})

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.UOPRemaining != 0 {
		t.Errorf("UOPRemaining after a second release past the wait = %d, want 0: the release declared a count for a leg Core had already released", b.UOPRemaining)
	}
	msgs, err := db.ListPendingOutbox(20)
	testutil.MustNoErr(t, err, "list outbox")
	for _, m := range msgs {
		if m.MsgType == string(protocol.TypeOrderError) {
			t.Errorf("error reply enqueued for a repeat release of a moving leg: %s", m.Payload)
		}
	}
}
