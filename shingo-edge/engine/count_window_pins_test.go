package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// A PLC tick reads every node's runtime row once (counterDeltaRows) and takes
// countMu only to write. A release's count, a pickup's leave or an operator's
// count can land, each under countMu, between that read and the tick's lock.
// The tick used to write the count it computed from the row it read, which
// overwrote what landed in between and charged the tick to the bin it read.
// Each pin plays one such write into the window: the tick is handed the row as
// it was read before the write, as handleCounterDelta hands it.
//
// RED at b9de92fe (pins/count-window-red-b9de92fe.log): every case but the
// cost, which reads 2 there too.

// windowWorld is fenceFixture's seat (bin 4401 at epoch 3, count 25, the real
// accumulator) and the row a tick read before anything else landed.
func windowWorld(t *testing.T, coreNode string) (*Engine, *processes.Node, *processes.RuntimeState, *uop.Mutator, int64) {
	t.Helper()
	eng, nodeID, acc, binID := fenceFixture(t, coreNode)
	node, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "the node")
	read, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "the row the tick read")
	return eng, node, read, acc, binID
}

var windowClaim = &processes.NodeClaim{Role: protocol.ClaimRoleConsume, PayloadCode: "PART-A"}

// chargedToBin is what the accumulator holds for the bin at epoch 3: the
// ticks charged to it and not yet flushed (a draw is negative).
func chargedToBin(t *testing.T, acc *uop.Mutator, binID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, acc.WithPending(func(p uop.Pending) error { n = p.Bin(binID, 3); return nil }), "pending")
	return n
}

func TestCountWindow_AReleaseInTheWindowStands(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		declared  int
		wantCount int
		wantHold  int64
		wantBin   int
	}{
		// Released empty: the bin takes no more ticks (R3); the tick is held.
		{name: "released empty", declared: 0, wantCount: 0, wantHold: 3, wantBin: 0},
		// A count release keeps charging the bin until the lift (R1), on top
		// of what the operator declared.
		{name: "released with 10", declared: 10, wantCount: 7, wantHold: 0, wantBin: -3},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng, node, read, acc, binID := windowWorld(t, "WIN-REL-"+c.name[:8])
			testutil.MustNoErr(t, eng.db.SetProcessNodeReleaseCount(node.ID, c.declared), "the release's count")

			eng.handleConsumeTick(node, read, windowClaim, 3)

			rt, err := eng.db.GetProcessNodeRuntime(node.ID)
			testutil.MustNoErr(t, err, "runtime")
			if rt.RemainingUOPCached != c.wantCount {
				t.Errorf("count %d, want %d: the release declared %d before the tick's write", rt.RemainingUOPCached, c.wantCount, c.declared)
			}
			if rt.PendingUOPDelta != c.wantHold {
				t.Errorf("hold %d, want %d", rt.PendingUOPDelta, c.wantHold)
			}
			if got := chargedToBin(t, acc, binID); got != c.wantBin {
				t.Errorf("charged to bin %d: %d, want %d", binID, got, c.wantBin)
			}
		})
	}
}

func TestCountWindow_APickupInTheWindowStands(t *testing.T) {
	t.Parallel()
	eng, node, read, acc, binID := windowWorld(t, "WIN-PICKUP")
	testutil.MustNoErr(t, eng.carrierLeftAtPickup(node.ID, node.CoreNodeName, 4, 0, ""), "the pickup's leave")

	eng.handleConsumeTick(node, read, windowClaim, 2)

	rt, err := eng.db.GetProcessNodeRuntime(node.ID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveBinID != nil {
		t.Fatalf("the seat holds bin %d after the pickup", *rt.ActiveBinID)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("count %d on an empty seat, want 0: the leave zeroed it", rt.RemainingUOPCached)
	}
	if rt.PendingUOPDelta != 2 || rt.PendingUOPPayload != "PART-A" {
		t.Errorf("hold %d of %q, want 2 of PART-A for the next carrier", rt.PendingUOPDelta, rt.PendingUOPPayload)
	}
	if got := chargedToBin(t, acc, binID); got != 0 {
		t.Errorf("charged %d to bin %d, which had left; want 0", got, binID)
	}
}

func TestCountWindow_AnOperatorsCountInTheWindowStands(t *testing.T) {
	t.Parallel()
	eng, node, read, acc, binID := windowWorld(t, "WIN-COUNT")
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID: binID, CoreNodeName: node.CoreNodeName, NewRemaining: 12, Epoch: 3, Actor: "operator-under-test",
	})
	if got := runtimeCount(t, eng, node.ID); got != 12 {
		t.Fatalf("control: the operator's count wrote %d, want 12", got)
	}

	eng.handleConsumeTick(node, read, windowClaim, 3)

	if got := runtimeCount(t, eng, node.ID); got != 9 {
		t.Errorf("count %d, want 9: the operator counted 12 before the tick's write, and the tick draws 3 from it", got)
	}
	if got := chargedToBin(t, acc, binID); got != -3 {
		t.Errorf("charged to bin %d: %d, want -3", binID, got)
	}
}

// A tick whose row is still the seat costs what it cost at b9de92fe, 2: the
// pile read and the count write. No re-read (a read per tick is a stop).
func TestCountWindow_AnUnchangedSeatIsOneWrite(t *testing.T) {
	t.Parallel()
	eng, counter := newCountingCoverageEngine(t)
	nodeID, _ := boundNodeFixture(t, eng, "WIN-COST", 4401, 3)
	eng.SetInventoryDeltaSink(uop.New(eng.db, eng.cfg.StationID(), eng.db, eng.db))
	node, err := eng.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "the node")
	read, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "the row the tick read")

	counter.Reset()
	eng.handleConsumeTick(node, read, windowClaim, 1)
	if n := counter.Count(); n != 2 {
		t.Errorf("an unchanged seat's tick ran %d statements, want 2 (the pile read, the count write)", n)
	}
}
