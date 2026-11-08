// A late pre-lift window applied after the cart landed at another seat: Core
// tells that seat's Edge, and both books agree.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"

	corenodes "shingocore/store/nodes"

	edgeengine "shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/store/processes"
	edgeuop "shingoedge/uop"
)

// TestScenario_H_ALateDrawAtTheNextSeatReachesItsEdge — a cart counted at the
// first seat is lifted while its last window is still on the wire, and lands
// at a second seat, a process of its own, at the count Core held then. The
// second seat counts one stroke (flushed, not yet applied). The late window
// then applies at Core, at the generation the lift ended. Core sends the
// second seat's Edge its count at the current generation, fenced, so the Edge
// keeps its in-flight stroke on top: both books read N − k − m − 1, and
// nothing drops. Before, Core's count moved and the second seat's Edge kept
// the landing's count for the cart's whole stay, m above Core's.
func TestScenario_H_ALateDrawAtTheNextSeatReachesItsEdge(t *testing.T) {
	t.Parallel()
	const n, k, m = 100, 2, 3
	w := newTwoBooks(t, "SCN-H-LATE", n)
	acc := edgeuop.New(w.edge.DB, handoffStation, w.edge.DB, w.edge.DB)
	w.edge.Engine.SetInventoryDeltaSink(acc)
	if _, err := w.core.Exec(`INSERT INTO edge_registry (station_id, schema_version) VALUES ($1, $2)`,
		handoffStation, protocol.EdgeSchemaBinLanded); err != nil {
		t.Fatalf("register the station: %v", err)
	}

	// The second seat: a node Core places at, and a process of its own here.
	line2 := &corenodes.Node{Name: "H-LINE-2", Enabled: true}
	if err := w.core.CreateNode(line2); err != nil {
		t.Fatalf("create the second seat: %v", err)
	}
	p2, err := w.edge.DB.CreateProcess("H-PROC-2", "", "", "", false)
	if err != nil {
		t.Fatalf("second process: %v", err)
	}
	seat2, err := w.edge.DB.CreateProcessNode(processes.NodeInput{ProcessID: p2, CoreNodeName: line2.Name, Code: "H2",
		Name: "H seat 2", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("second node: %v", err)
	}
	s2, err := w.edge.DB.CreateStyle("H-STYLE-2", "", p2)
	if err != nil {
		t.Fatalf("second style: %v", err)
	}
	if err := w.edge.DB.SetActiveStyle(p2, &s2); err != nil {
		t.Fatalf("second style active: %v", err)
	}
	claim2, err := upsertClaimLegacySimple(w.edge.DB, processes.NodeClaimInput{StyleID: s2, CoreNodeName: line2.Name,
		Role: "consume", SwapMode: "simple", PayloadCode: w.payload, UOPCapacity: n})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := w.edge.DB.EnsureProcessNodeRuntime(seat2); err != nil {
		t.Fatalf("second runtime: %v", err)
	}
	if err := w.edge.DB.SetProcessNodeRuntime(seat2, &claim2, 0); err != nil {
		t.Fatalf("second seat's claim: %v", err)
	}
	tick := func(process, style int64) {
		w.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
			Payload: edgeengine.CounterDeltaEvent{ProcessID: process, StyleID: style, Delta: 1}})
	}

	// k strokes at the first seat land at Core; m more are flushed and still
	// on the wire when the robot lifts the cart.
	for i := 0; i < k; i++ {
		tick(w.process, w.style)
	}
	acc.Flush()
	w.shipEdgeDeltas()
	for i := 0; i < m; i++ {
		tick(w.process, w.style)
	}
	acc.Flush()
	late := w.takeEdgeDeltas()
	if len(late) == 0 {
		t.Fatal("no window on the wire at the lift")
	}
	if err := w.bins.MoveToTransit(w.binID); err != nil {
		t.Fatalf("the lift: %v", err)
	}
	var liftedAt time.Time
	var liftEpoch int64
	if err := w.core.QueryRow(`SELECT lifted_at, delta_epoch FROM bins WHERE id=$1`, w.binID).Scan(&liftedAt, &liftEpoch); err != nil {
		t.Fatalf("read the lift: %v", err)
	}
	if _, err := w.edge.DB.CreateOrder(w.order.EdgeUUID, orders.TypeComplex, &w.nodeID, false, 1,
		"OUT", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("edge order: %v", err)
	}
	w.edge.Engine.HandleBinPickedUpMessage(protocol.BinPickedUp{
		OrderUUID: w.order.EdgeUUID, BinID: w.binID, Location: w.nodeName, PickedUpAt: liftedAt, Epoch: liftEpoch,
	})

	// The cart lands at the second seat at the count Core holds now.
	if _, err := w.bins.ApplyArrival(w.binID, line2.ID, false, nil, w.order.ID); err != nil {
		t.Fatalf("arrival at the second seat: %v", err)
	}
	landedAt, _ := w.coreBin()
	if landedAt != n-k {
		t.Fatalf("Core at the landing = %d, want %d: the late window is still on the wire", landedAt, n-k)
	}
	w.edge.Engine.HandleBinLanded(protocol.BinLanded{BinID: w.binID, CoreNodeName: line2.Name, Count: landedAt,
		Epoch: liftEpoch, PayloadCode: w.payload, PayloadKnown: true, PlacedSeq: 1})
	tick(p2, s2)
	acc.Flush()
	inFlight := w.takeEdgeDeltas()

	// The late window applies at Core; Core's adjustments reach the Edge.
	for _, d := range late {
		if err := w.inv.ApplyBinUOPDelta(handoffStation, d); err != nil {
			t.Fatalf("the late window: %v", err)
		}
	}
	w.deliverCoreAdjustments()
	for _, d := range inFlight {
		_ = w.inv.ApplyBinUOPDelta(handoffStation, d)
	}

	want := n - k - m - 1
	core, _ := w.coreBin()
	rt, err := w.edge.DB.GetProcessNodeRuntime(seat2)
	if err != nil || rt == nil {
		t.Fatalf("second seat: %v", err)
	}
	if core != want || rt.RemainingUOPCached != want {
		t.Errorf("after the late window: Core %d, the second seat's Edge %d; want both %d", core, rt.RemainingUOPCached, want)
	}
	if got := w.staleDrops(); got != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", got)
	}
}

// takeEdgeDeltas returns every bin_uop_delta in the Edge's outbox, not yet
// applied at Core, and acks them: the windows the test carries by hand.
func (w *twoBooks) takeEdgeDeltas() []*protocol.BinUOPDelta {
	w.t.Helper()
	msgs, err := w.edge.DB.ListPendingOutbox(1000)
	if err != nil {
		w.t.Fatalf("list edge outbox: %v", err)
	}
	var out []*protocol.BinUOPDelta
	for _, msg := range msgs {
		if msg.MsgType != protocol.SubjectBinUOPDelta {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(msg.Payload, &env); err != nil {
			w.t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			w.t.Fatalf("decode data: %v", err)
		}
		d := &protocol.BinUOPDelta{}
		if err := json.Unmarshal(data.Body, d); err != nil {
			w.t.Fatalf("decode delta: %v", err)
		}
		out = append(out, d)
		if err := w.edge.DB.AckOutbox(msg.ID); err != nil {
			w.t.Fatalf("ack: %v", err)
		}
	}
	return out
}
