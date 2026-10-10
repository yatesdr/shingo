// Lane H's two-book pins (BRIEF-unification-C2c 2026-10-10, "Lane H", R1 and
// R5; DECISIONS D11 mechanism (i)). Each runs Core against Postgres and an
// Edge engine against SQLite and asserts the SAME number in both books:
// Core's bins.uop_remaining and the Edge's remaining_uop_cached.
//
// The wire legs are carried by hand, in the order the r6 race takes them,
// rather than by the Bus: the Edge's ticks are applied at Core as the deltas
// the Edge would flush (stamped with the generation the Edge holds at the
// tick), the release is Core's own SyncOrClearForReleased, and Core's
// uop_adjustment outbox rows are handed to the Edge's handler after the ticks.
// That is the ordering the 18 r6 stale drops took (DESIGN-H §11): ticks
// charged before the release's echo reaches the Edge.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"shingo/protocol"

	"shingocore/dispatch"
	"shingocore/service"
	corestore "shingocore/store"
	corebins "shingocore/store/bins"
	coreorders "shingocore/store/orders"
	coreharness "shingocore/testharness"
	"shingocore/uop"

	edgeengine "shingoedge/engine"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
	edgeuop "shingoedge/uop"
)

// twoBooks is one carrier at a line seat, known to both Core and the Edge.
type twoBooks struct {
	t        *testing.T
	core     *corestore.DB
	manifest *service.BinManifestService
	inv      *uop.InventoryDeltaService
	bins     *service.BinService
	edge     *edgeharness.Edge
	order    *coreorders.Order
	binID    int64
	payload  string
	process  int64
	style    int64
	nodeID   int64
	nodeName string // the seat's Core node name
	binType  int64
	lineNode int64
	seq      map[int64]int64 // next delta seq per generation
	applied  map[int64]bool  // Core outbox rows already handed to the Edge
}

const handoffStation = "edge.handoff"

func newTwoBooks(t *testing.T, label string, count int) *twoBooks {
	t.Helper()
	coreDB := coreharness.OpenDB(t)
	sd := coreharness.SetupStandardData(t, coreDB)
	announce := service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core.test"}
	manifest := service.NewBinManifestService(coreDB, announce)
	inv := uop.NewInventoryDeltaService(coreDB, manifest, announce)

	bin := &corebins.Bin{BinTypeID: sd.BinType.ID, Label: label, NodeID: &sd.LineNode.ID, Status: "staged"}
	if err := coreDB.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	m := `{"items":[{"catid":"` + sd.Payload.Code + `","qty":` + strconv.Itoa(count) + `}]}`
	if err := coreDB.SetBinManifest(bin.ID, m, sd.Payload.Code, count); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	if err := coreDB.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("confirm manifest: %v", err)
	}
	order := &coreorders.Order{
		EdgeUUID: "handoff-" + label, StationID: handoffStation, OrderType: dispatch.OrderTypeComplex,
		Status: dispatch.StatusStaged, PayloadCode: sd.Payload.Code, Quantity: 1,
		SourceNode: sd.LineNode.Name, DeliveryNode: sd.StorageNode.Name,
	}
	if err := coreDB.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	coreharness.ClaimBinForTest(t, coreDB, bin.ID, order.ID)
	var epoch int64
	if err := coreDB.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	edge := edgeharness.NewEdge(t, handoffStation)
	edge.Engine.SetInventoryDeltaSink(edgeuop.New(edge.DB, handoffStation, edge.DB, edge.DB))
	processID, err := edge.DB.CreateProcess("H-PROC-"+label, "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: sd.LineNode.Name, Code: "H1", Name: "H seat", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := edge.DB.CreateStyle("H-STYLE-"+label, "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set style: %v", err)
	}
	claimID, err := upsertClaimLegacySimple(edge.DB, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: sd.LineNode.Name, Role: "consume", SwapMode: "simple",
		PayloadCode: sd.Payload.Code, UOPCapacity: count,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	b := bin.ID
	if err := edge.DB.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &b, epoch, count); err != nil {
		t.Fatalf("bind the carrier: %v", err)
	}

	return &twoBooks{
		t: t, core: coreDB, manifest: manifest, inv: inv, bins: service.NewBinService(coreDB, manifest),
		edge: edge, order: order, binID: bin.ID, payload: sd.Payload.Code,
		process: processID, style: styleID, nodeID: nodeID,
		nodeName: sd.LineNode.Name, binType: sd.BinType.ID, lineNode: sd.LineNode.ID,
		seq: map[int64]int64{}, applied: map[int64]bool{},
	}
}

// tick is one consume tick on the Edge, and the delta it flushes applied at
// Core stamped with the generation the Edge held at the tick.
func (w *twoBooks) tick() {
	w.t.Helper()
	epoch := w.edgeRuntime().ActiveBinEpoch
	w.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
		Payload: edgeengine.CounterDeltaEvent{ProcessID: w.process, StyleID: w.style, Delta: 1}})
	w.seq[epoch]++
	now := time.Now().UTC()
	// A skip is the observable (the stale-drop ledger row); not a test error.
	_ = w.inv.ApplyBinUOPDelta(handoffStation, &protocol.BinUOPDelta{
		BinID: w.binID, PayloadCode: w.payload, Delta: -1, Reason: protocol.ReasonConsumeTick,
		SequenceID: w.seq[epoch], Epoch: epoch, WindowStart: now.Add(-time.Second), WindowEnd: now,
	})
}

// fence is the release's stream fence (DECISIONS D11 (i); OrderRelease.AsOf)
// as the wire legs here carry it: the generation the Edge holds, the last seq
// the test applied in it, and that scope's net (every tick here is -1), as of
// now. The release envelope's sender is the station.
//
// CONVERTED (Lane H step 3): the pins called SyncOrClearForReleased with no
// fence, which is now an old Edge's release and keeps the bump. Their
// assertions are unchanged.
func (w *twoBooks) fence() *service.ReleaseFence {
	w.t.Helper()
	epoch := w.edgeRuntime().ActiveBinEpoch
	return &service.ReleaseFence{Station: handoffStation, ReleaseFence: protocol.ReleaseFence{
		BinID: w.binID, Epoch: epoch, AsOfSeq: w.seq[epoch], AsOfNet: -w.seq[epoch], AsOfAt: time.Now().UTC(),
	}}
}

// deliverCoreAdjustments hands every uop_adjustment Core has queued for the
// bin, not yet handed over, to the Edge's handler.
func (w *twoBooks) deliverCoreAdjustments() {
	w.t.Helper()
	rows, err := w.core.Query(`SELECT id, payload FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+protocol.SubjectUOPAdjustment)
	if err != nil {
		w.t.Fatalf("read core outbox: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		if w.applied[id] {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			w.t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			w.t.Fatalf("decode data: %v", err)
		}
		var adj protocol.UOPAdjustment
		if err := json.Unmarshal(data.Body, &adj); err != nil {
			w.t.Fatalf("decode adjustment: %v", err)
		}
		w.applied[id] = true
		if adj.BinID == w.binID {
			w.edge.Engine.HandleUOPAdjustment(adj)
		}
	}
}

func (w *twoBooks) edgeRuntime() *processes.RuntimeState {
	w.t.Helper()
	rt, err := w.edge.DB.GetProcessNodeRuntime(w.nodeID)
	if err != nil || rt == nil {
		w.t.Fatalf("read edge runtime: %v", err)
	}
	return rt
}

func (w *twoBooks) coreBin() (uopRemaining int, payload string) {
	w.t.Helper()
	if err := w.core.QueryRow(`SELECT uop_remaining, COALESCE(payload_code,'') FROM bins WHERE id=$1`,
		w.binID).Scan(&uopRemaining, &payload); err != nil {
		w.t.Fatalf("read core bin: %v", err)
	}
	return uopRemaining, payload
}

func (w *twoBooks) staleDrops() int {
	w.t.Helper()
	var n int
	if err := w.core.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op='stale_epoch_dropped'`,
		w.binID).Scan(&n); err != nil {
		w.t.Fatalf("count stale drops: %v", err)
	}
	return n
}

// TestScenario_H_R1_CountReleaseTicksBeforeTheLiftLandInBothBooks — R1: ticks
// are charged to the bin on the seat until the robot lifts it. D11 (i): the
// release writes the declared count with no bump and fences the stream; the
// generation ends at the lift.
//
// Count release at N, k ticks before the lift: N − k in both books, none
// stale-dropped.
//
// At the base: RED. The release bumps the generation in its own transaction;
// the k ticks the Edge stamps with the generation it still holds are
// stale_epoch_dropped at Core (Core stays N), and the release's echo then
// rewrites the Edge's count to N (handler_uop_adjustment.go absolute arm).
//
// Green at Lane H step 3: the release is fenced (no bump), the lift bumps.
func TestScenario_H_R1_CountReleaseTicksBeforeTheLiftLandInBothBooks(t *testing.T) {
	w := newTwoBooks(t, "SCN-H-R1", 100)
	w.tick()
	w.tick()

	const n, k = 98, 3
	declared := n
	if err := w.manifest.SyncOrClearForReleasedFenced(w.binID, w.order.ID, &declared,
		protocol.DispositionReleasePartial, "operator", w.fence()); err != nil {
		t.Fatalf("release at %d: %v", n, err)
	}
	for i := 0; i < k; i++ {
		w.tick()
	}
	w.deliverCoreAdjustments()

	if got := w.edgeRuntime().RemainingUOPCached; got != n-k {
		t.Errorf("Edge count = %d, want %d: the release's echo rewrote the ticks counted before the lift", got, n-k)
	}
	if got, _ := w.coreBin(); got != n-k {
		t.Errorf("Core uop_remaining = %d, want %d", got, n-k)
	}
	if got := w.staleDrops(); got != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0: a tick before the lift is the released bin's (R1)", got)
	}

	if err := w.bins.MoveToTransit(w.binID); err != nil {
		t.Fatalf("the lift: %v", err)
	}
	if got, _ := w.coreBin(); got != n-k {
		t.Errorf("Core uop_remaining after the lift = %d, want %d", got, n-k)
	}
}

// TestScenario_H_R5_ReleaseAtOneThenATickLeavesLabelledAtZero — R5: a consume
// tick alone never clears a label; a bin may leave labelled at 0, and its exit
// is the loader operator's clear. Release at 1, one tick, lift: the bin
// leaves labelled at 0 in both books.
//
// The loader's refusal text ("confirm it is empty, CLEAR it, then load") is
// Lane L's change (D8, TestPin_LoaderRefusesALabelledCarrierAtZero); this pin
// holds the state the refusal reads.
//
// At the base: RED. The tick is stale-dropped (Core leaves at 1) and the echo
// writes 1 back on the Edge.
//
// Green at Lane H step 3: the release is fenced (no bump), the lift bumps.
func TestScenario_H_R5_ReleaseAtOneThenATickLeavesLabelledAtZero(t *testing.T) {
	w := newTwoBooks(t, "SCN-H-R5", 1)

	one := 1
	if err := w.manifest.SyncOrClearForReleasedFenced(w.binID, w.order.ID, &one,
		protocol.DispositionReleasePartial, "operator", w.fence()); err != nil {
		t.Fatalf("release at 1: %v", err)
	}
	w.tick()
	w.deliverCoreAdjustments()
	if got := w.edgeRuntime().RemainingUOPCached; got != 0 {
		t.Errorf("Edge count before the lift = %d, want 0", got)
	}

	if err := w.bins.MoveToTransit(w.binID); err != nil {
		t.Fatalf("the lift: %v", err)
	}
	got, payload := w.coreBin()
	if got != 0 {
		t.Errorf("Core uop_remaining at the lift = %d, want 0 (the tick before the lift is the bin's)", got)
	}
	if payload != w.payload {
		t.Errorf("Core payload_code = %q, want %q: a consume tick never clears a label (R5)", payload, w.payload)
	}
}
