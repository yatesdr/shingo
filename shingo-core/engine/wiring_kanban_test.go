//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/store"
)

// wiring_kanban_test.go — coverage for wiring_kanban.go.
//
// Covers handleKanbanDemand (both FROM-storage and TO-storage branches,
// plus non-moved/empty-payload guards), isStorageSlot (LANE-parent happy
// path, non-LANE parent, orphan node, missing node), and sendDemandSignals
// (indirect, via handleKanbanDemand). Every event assertion is made by
// inspecting the outbox — SendDataToEdge enqueues there, giving us a
// durable, DB-backed signal to verify without mocking messaging.

// ── isStorageSlot — direct unit tests ───────────────────────────────

// seedLaneNodeType ensures the LANE node type exists. The store
// migrations already seed it on a fresh testDB, so we just verify
// it's present rather than re-inserting (which would violate the
// unique-code constraint).
func seedLaneNodeType(t *testing.T, db *store.DB) {
	t.Helper()
	types, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("list node types: %v", err)
	}
	for _, nt := range types {
		if nt.Code == "LANE" {
			return
		}
	}
	// Fallback: create it if migrations didn't seed for some reason.
	if err := db.CreateNodeType(&store.NodeType{Code: "LANE", Name: "Lane", IsSynthetic: true}); err != nil {
		t.Fatalf("create LANE node type: %v", err)
	}
}

// TestIsStorageSlot_ChildOfLane returns true: the parent node is a LANE.
func TestIsStorageSlot_ChildOfLane(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	seedLaneNodeType(t, db)

	laneTypeID := mustGetNodeTypeID(t, db, "LANE")
	lane := &store.Node{Name: "LANE-K1", Enabled: true, NodeTypeID: &laneTypeID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	slot := &store.Node{Name: "LANE-K1-SLOT-1", Enabled: true, ParentID: &lane.ID}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}

	if !eng.isStorageSlot(slot.ID) {
		t.Errorf("expected isStorageSlot(%d) true for child of LANE", slot.ID)
	}
}

// TestIsStorageSlot_ChildOfNonLane — parent exists but is not LANE.
func TestIsStorageSlot_ChildOfNonLane(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	parent := &store.Node{Name: "NOT-LANE", Enabled: true}
	if err := db.CreateNode(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	child := &store.Node{Name: "NOT-LANE-CHILD", Enabled: true, ParentID: &parent.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	if eng.isStorageSlot(child.ID) {
		t.Errorf("isStorageSlot should be false when parent is not LANE")
	}
}

// TestIsStorageSlot_OrphanNode — no parent → not a storage slot.
func TestIsStorageSlot_OrphanNode(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	orphan := &store.Node{Name: "ORPHAN-1", Enabled: true}
	if err := db.CreateNode(orphan); err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	if eng.isStorageSlot(orphan.ID) {
		t.Errorf("isStorageSlot must be false for orphan node")
	}
}

// TestIsStorageSlot_MissingNode — GetNode returns error → false.
func TestIsStorageSlot_MissingNode(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	if eng.isStorageSlot(987654321) {
		t.Errorf("isStorageSlot must be false for missing node")
	}
}

// ── handleKanbanDemand — empty / non-moved guards ───────────────────

// TestHandleKanbanDemand_GuardsEmptyPayload short-circuits before doing
// any DB work when PayloadCode is blank.
func TestHandleKanbanDemand_GuardsEmptyPayload(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	before, _ := db.ListPendingOutbox(10)
	eng.handleKanbanDemand(BinUpdatedEvent{Action: "moved", PayloadCode: ""})
	after, _ := db.ListPendingOutbox(10)

	if len(after) != len(before) {
		t.Errorf("outbox grew from %d to %d — empty PayloadCode must not enqueue", len(before), len(after))
	}
}

// TestHandleKanbanDemand_GuardsNonMovedAction — only Action=="moved" runs.
func TestHandleKanbanDemand_GuardsNonMovedAction(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// Even with a proper registry entry set up, a non-moved action must skip.
	seedDemandEntry(t, db, "station-x", "SLOT-X", "produce", "P-GUARD")
	before, _ := db.ListPendingOutbox(10)

	eng.handleKanbanDemand(BinUpdatedEvent{
		Action:      "added", // not "moved"
		PayloadCode: "P-GUARD",
		FromNodeID:  1,
		ToNodeID:    2,
	})

	after, _ := db.ListPendingOutbox(10)
	if len(after) != len(before) {
		t.Errorf("non-moved action should not enqueue; outbox %d→%d", len(before), len(after))
	}
}

// ── handleKanbanDemand — end-to-end produce/consume signals ─────────

// TestHandleKanbanDemand_FromStorageSignalsProducers — bin leaves a
// storage slot → sendDemandSignals fires with role=produce, and the
// registered producer gets a data.demand_signal enqueued in outbox.
func TestHandleKanbanDemand_FromStorageSignalsProducers(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	seedLaneNodeType(t, db)
	laneTypeID := mustGetNodeTypeID(t, db, "LANE")
	lane := &store.Node{Name: "LANE-FROM", Enabled: true, NodeTypeID: &laneTypeID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	slot := &store.Node{Name: "LANE-FROM-SLOT", Enabled: true, ParentID: &lane.ID}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}

	// Register both a producer (should receive "produce" signals) and
	// a consumer (should NOT receive when we only drain FROM storage).
	seedDemandEntry(t, db, "station-producer", "PROD-NODE", "produce", "P-K1")
	seedDemandEntry(t, db, "station-consumer", "CONS-NODE", "consume", "P-K1")

	eng.handleKanbanDemand(BinUpdatedEvent{
		Action:      "moved",
		PayloadCode: "P-K1",
		BinID:       42,
		FromNodeID:  slot.ID, // leaving storage
		ToNodeID:    0,       // not going anywhere kanban-relevant
	})

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	producerHit := findDemandSignal(t, msgs, "station-producer")
	if producerHit == nil {
		t.Fatalf("producer station missing demand signal; outbox=%+v", outboxSummary(msgs))
	}
	if producerHit.Role != "produce" {
		t.Errorf("producer signal role = %q, want produce", producerHit.Role)
	}
	if producerHit.PayloadCode != "P-K1" {
		t.Errorf("producer signal payload = %q, want P-K1", producerHit.PayloadCode)
	}

	// Consumer must not be signalled on FROM-only move.
	if consumerHit := findDemandSignal(t, msgs, "station-consumer"); consumerHit != nil {
		t.Errorf("consumer should not be signalled on FROM move: %+v", consumerHit)
	}
}

// TestHandleKanbanDemand_ToStorageSignalsConsumers — bin arrives in
// storage → consumers (role=consume) are signalled.
func TestHandleKanbanDemand_ToStorageSignalsConsumers(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	seedLaneNodeType(t, db)
	laneTypeID := mustGetNodeTypeID(t, db, "LANE")
	lane := &store.Node{Name: "LANE-TO", Enabled: true, NodeTypeID: &laneTypeID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	slot := &store.Node{Name: "LANE-TO-SLOT", Enabled: true, ParentID: &lane.ID}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}

	seedDemandEntry(t, db, "station-consumer-2", "CNS-NODE", "consume", "P-K2")
	seedDemandEntry(t, db, "station-producer-2", "PRD-NODE", "produce", "P-K2")

	eng.handleKanbanDemand(BinUpdatedEvent{
		Action:      "moved",
		PayloadCode: "P-K2",
		BinID:       77,
		FromNodeID:  0,
		ToNodeID:    slot.ID, // arriving at storage
	})

	msgs, _ := db.ListPendingOutbox(10)
	consumerHit := findDemandSignal(t, msgs, "station-consumer-2")
	if consumerHit == nil {
		t.Fatalf("consumer station missing demand signal; outbox=%+v", outboxSummary(msgs))
	}
	if consumerHit.Role != "consume" {
		t.Errorf("consumer signal role = %q, want consume", consumerHit.Role)
	}
	if consumerHit.CoreNodeName != "CNS-NODE" {
		t.Errorf("consumer signal CoreNodeName = %q, want CNS-NODE", consumerHit.CoreNodeName)
	}

	if producerHit := findDemandSignal(t, msgs, "station-producer-2"); producerHit != nil {
		t.Errorf("producer should not be signalled on TO-only move: %+v", producerHit)
	}
}

// TestHandleKanbanDemand_NonStorageNodesSkipped — neither From nor To is
// under a LANE → sendDemandSignals is never invoked → no outbox growth.
func TestHandleKanbanDemand_NonStorageNodesSkipped(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	// Two plain (non-LANE-parented) nodes.
	a := &store.Node{Name: "PLAIN-A", Enabled: true}
	b := &store.Node{Name: "PLAIN-B", Enabled: true}
	_ = db.CreateNode(a)
	_ = db.CreateNode(b)

	seedDemandEntry(t, db, "station-skip", "NOPE", "produce", "P-SKIP")
	before, _ := db.ListPendingOutbox(10)

	eng.handleKanbanDemand(BinUpdatedEvent{
		Action:      "moved",
		PayloadCode: "P-SKIP",
		BinID:       1,
		FromNodeID:  a.ID,
		ToNodeID:    b.ID,
	})

	after, _ := db.ListPendingOutbox(10)
	if len(after) != len(before) {
		t.Errorf("non-storage move must not enqueue; outbox %d→%d", len(before), len(after))
	}
}

// ── test helpers ─────────────────────────────────────────────────────

// mustGetNodeTypeID looks up the ID of a node type by its code — used
// because the Node struct takes NodeTypeID (the FK), not the string code.
func mustGetNodeTypeID(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	types, err := db.ListNodeTypes()
	if err != nil {
		t.Fatalf("list node types: %v", err)
	}
	for _, nt := range types {
		if nt.Code == code {
			return nt.ID
		}
	}
	t.Fatalf("no node type with code %q in: %+v", code, types)
	return 0
}

// seedDemandEntry inserts a single demand-registry row so handleKanbanDemand
// has something to fan out to. Uses SyncDemandRegistry under the hood —
// each call replaces prior entries for that station, but different tests
// use different stationIDs so they don't clobber each other.
func seedDemandEntry(t *testing.T, db *store.DB, stationID, coreNodeName, role, payloadCode string) {
	t.Helper()
	if err := db.SyncDemandRegistry(stationID, []store.DemandRegistryEntry{{
		StationID:    stationID,
		CoreNodeName: coreNodeName,
		Role:         role,
		PayloadCode:  payloadCode,
	}}); err != nil {
		t.Fatalf("sync demand registry %s: %v", stationID, err)
	}
}

// findDemandSignal scans outbox rows looking for a data.<SubjectDemandSignal>
// envelope to the given station, and returns the decoded signal or nil.
// Data envelopes wrap their subject-specific body inside a protocol.Data
// value, which is itself the Payload of the outer Envelope.
func findDemandSignal(t *testing.T, msgs []*store.OutboxMessage, stationID string) *protocol.DemandSignal {
	t.Helper()
	wantType := "data." + protocol.SubjectDemandSignal
	for _, m := range msgs {
		if m.MsgType != wantType || m.StationID != stationID {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := json.Unmarshal(env.Payload, &data); err != nil {
			t.Fatalf("decode data wrapper: %v", err)
		}
		var sig protocol.DemandSignal
		if err := json.Unmarshal(data.Body, &sig); err != nil {
			t.Fatalf("decode DemandSignal body: %v", err)
		}
		return &sig
	}
	return nil
}

// outboxSummary returns a compact string for test-failure messages.
func outboxSummary(msgs []*store.OutboxMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.MsgType+"→"+m.StationID)
	}
	return out
}
