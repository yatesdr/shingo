// Lane H's late-lift pin (R4; DECISIONS D11 mechanism (ii)): the ticks counted
// after the lift time come off the old bin and land on the next bin of the
// part, in both books. One rule, judged by each side on the same two numbers:
// a window of the generation that ended at a lift at T is the old bin's if its
// window_start < T, and not if ≥ T (granularity: one flush window).
//
// Enabled at Lane H step 3 (mechanisms (i) and (ii)): the lift-time generation
// (BinPickedUp.Epoch, bins.lifted_at, the after_lift refusal), the Edge reader of
// PickedUpAt (Engine.HandleBinPickedUpMessage) and the landing
// (Engine.HandleBinLanded) exist under the names this file assumed; the
// laneh_pending tag is gone and no assertion changed. The release passes the
// stream fence (w.fence), as an Edge with mechanism (i) sends it.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"

	corebins "shingocore/store/bins"

	edgeengine "shingoedge/engine"
	"shingoedge/orders"
	edgeuop "shingoedge/uop"
)

// shipEdgeDeltas applies every bin_uop_delta in the Edge's outbox at Core, as
// the drainer and Core's ingest would, and acks it.
func (w *twoBooks) shipEdgeDeltas() {
	w.t.Helper()
	msgs, err := w.edge.DB.ListPendingOutbox(1000)
	if err != nil {
		w.t.Fatalf("list edge outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectBinUOPDelta {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			w.t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			w.t.Fatalf("decode data: %v", err)
		}
		var d protocol.BinUOPDelta
		if err := json.Unmarshal(data.Body, &d); err != nil {
			w.t.Fatalf("decode delta: %v", err)
		}
		_ = w.inv.ApplyBinUOPDelta(handoffStation, &d) // a refusal is the observable
		if err := w.edge.DB.AckOutbox(m.ID); err != nil {
			w.t.Fatalf("ack: %v", err)
		}
	}
}

// TestScenario_H_R4_ALateLiftMovesTheTicksAfterTheLiftToTheNextBin — R4.
// Count release at N (no bump, D11 (i)); k ticks, flushed; the robot lifts at
// T; m ticks counted after T, flushed while the Edge has not heard; then the
// pickup reaches the Edge. Old bin: N − k in both books (the m windows are
// refused at Core as after_lift, and the Edge moves them to the pile, then the
// hold, with the bin's part). The next bin of the part lands at M; one tick:
// M − m − 1 in both books.
func TestScenario_H_R4_ALateLiftMovesTheTicksAfterTheLiftToTheNextBin(t *testing.T) {
	w := newTwoBooks(t, "SCN-H-R4", 100)
	acc := edgeuop.New(w.edge.DB, handoffStation, w.edge.DB, w.edge.DB)
	w.edge.Engine.SetInventoryDeltaSink(acc)
	nodeName := w.nodeName
	tickEdge := func() {
		w.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
			Payload: edgeengine.CounterDeltaEvent{ProcessID: w.process, StyleID: w.style, Delta: 1}})
	}

	const n, k, m = 100, 3, 2
	declared := n
	if err := w.manifest.SyncOrClearForReleasedFenced(w.binID, w.order.ID, &declared,
		protocol.DispositionReleasePartial, "operator", w.fence()); err != nil {
		t.Fatalf("release at %d: %v", n, err)
	}
	for i := 0; i < k; i++ {
		tickEdge()
	}
	acc.Flush()
	w.shipEdgeDeltas()

	if err := w.bins.MoveToTransit(w.binID); err != nil {
		t.Fatalf("the lift: %v", err)
	}
	var liftedAt time.Time
	var liftEpoch int64
	if err := w.core.QueryRow(`SELECT lifted_at, delta_epoch FROM bins WHERE id=$1`, w.binID).Scan(&liftedAt, &liftEpoch); err != nil {
		t.Fatalf("read the lift: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the m ticks start strictly after T
	for i := 0; i < m; i++ {
		tickEdge()
	}
	acc.Flush()
	w.shipEdgeDeltas()

	if got, _ := w.coreBin(); got != n-k {
		t.Errorf("Core old bin = %d, want %d: a window starting after the lift was applied to it", got, n-k)
	}
	var refused int
	if err := w.core.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op='after_lift'`, w.binID).Scan(&refused); err != nil {
		t.Fatalf("count after_lift: %v", err)
	}
	if refused == 0 {
		t.Error("no after_lift refusal recorded at Core")
	}
	if got := w.staleDrops(); got != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", got)
	}

	// The pickup reaches the Edge late, carrying T and the new generation.
	if _, err := w.edge.DB.CreateOrder(w.order.EdgeUUID, orders.TypeComplex, &w.nodeID, false, 1,
		"OUT", "", "", false, "", "", ""); err != nil {
		t.Fatalf("edge order: %v", err)
	}
	w.edge.Engine.HandleBinPickedUpMessage(protocol.BinPickedUp{
		OrderUUID: w.order.EdgeUUID, BinID: w.binID, Location: nodeName,
		PickedUpAt: liftedAt, Epoch: liftEpoch,
	})
	if rt := w.edgeRuntime(); rt.ActiveBinID != nil || rt.PendingUOPDelta != m {
		t.Fatalf("Edge after the pickup: bound %v held %d, want nothing bound and %d held", rt.ActiveBinID, rt.PendingUOPDelta, m)
	}

	// The next bin of the part lands at M; one tick.
	const bigM = 80
	next := &corebins.Bin{BinTypeID: w.binType, Label: "SCN-H-R4-NEXT", NodeID: &w.lineNode, Status: "staged"}
	if err := w.core.CreateBin(next); err != nil {
		t.Fatalf("create next bin: %v", err)
	}
	if err := w.core.SetBinManifest(next.ID, `{"items":[]}`, w.payload, bigM); err != nil {
		t.Fatalf("manifest next: %v", err)
	}
	var nextEpoch int64
	if err := w.core.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, next.ID).Scan(&nextEpoch); err != nil {
		t.Fatalf("next epoch: %v", err)
	}
	w.edge.Engine.HandleBinLanded(protocol.BinLanded{
		BinID: next.ID, CoreNodeName: nodeName, Count: bigM, Epoch: nextEpoch,
		PayloadCode: w.payload, PayloadKnown: true, PlacedSeq: 1,
	})
	tickEdge()
	acc.Flush()
	w.shipEdgeDeltas()

	if got := w.edgeRuntime().RemainingUOPCached; got != bigM-m-1 {
		t.Errorf("Edge next bin = %d, want %d", got, bigM-m-1)
	}
	var coreNext int
	if err := w.core.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, next.ID).Scan(&coreNext); err != nil {
		t.Fatalf("read next: %v", err)
	}
	if coreNext != bigM-m-1 {
		t.Errorf("Core next bin = %d, want %d", coreNext, bigM-m-1)
	}
}
