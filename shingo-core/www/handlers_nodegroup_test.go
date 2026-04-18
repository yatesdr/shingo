//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/engine"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// Characterization tests for handlers_nodegroup.go — pinned before the Stage 1
// refactor that replaces h.engine.DB() with named query methods. These tests
// exercise the HTTP surface only (postJSON + rec.Code + DB/event observables),
// so they remain green after the refactor without modification.

// --- fixture helpers ---

// nodeGroupFixture is a minimal NGRP → LANE → slots layout for tests.
type nodeGroupFixture struct {
	Grp   *store.Node
	Lane  *store.Node
	Slots []*store.Node
}

// setupNodeGroupFixture builds one NGRP with one LANE under it and slotCount
// physical slots under the lane. prefix disambiguates names across tests run
// in the same database.
func setupNodeGroupFixture(t *testing.T, db *store.DB, prefix string, slotCount int) *nodeGroupFixture {
	t.Helper()

	grpID, err := db.CreateNodeGroup("GRP-" + prefix)
	if err != nil {
		t.Fatalf("create node group: %v", err)
	}
	laneID, err := db.AddLane(grpID, "GRP-"+prefix+"-L1")
	if err != nil {
		t.Fatalf("add lane: %v", err)
	}

	slots := make([]*store.Node, slotCount)
	for i := 0; i < slotCount; i++ {
		depth := i + 1
		slot := &store.Node{
			Name:     fmt.Sprintf("GRP-%s-L1-S%d", prefix, depth),
			ParentID: &laneID,
			Enabled:  true,
			Depth:    &depth,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", depth, err)
		}
		slots[i] = slot
	}

	grp, err := db.GetNode(grpID)
	if err != nil {
		t.Fatalf("get group after create: %v", err)
	}
	lane, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("get lane after create: %v", err)
	}
	return &nodeGroupFixture{Grp: grp, Lane: lane, Slots: slots}
}

// reparentFixture is the layout required by apiReparentNode's block-check
// path: a physical (non-synthetic) node sitting directly under a source NGRP,
// plus a destination NGRP to move it into. Synthetic nodes (NGRP, LANE) are
// rejected by the handler's synthetic guard at handlers_nodegroup.go:85 —
// only physical direct-children-of-NGRP (the "shuffle slot" shape used in
// testdb/compound.go) exercise the order-block guard.
type reparentFixture struct {
	SrcGrp      *store.Node
	DstGrpID    int64
	DirectChild *store.Node
}

// setupReparentFixture builds a src NGRP + dst NGRP + a non-synthetic node
// whose parent is the src NGRP. prefix disambiguates names across tests.
func setupReparentFixture(t *testing.T, db *store.DB, prefix string) *reparentFixture {
	t.Helper()
	srcID, err := db.CreateNodeGroup("GRP-" + prefix + "-SRC")
	if err != nil {
		t.Fatalf("create src group: %v", err)
	}
	dstID, err := db.CreateNodeGroup("GRP-" + prefix + "-DST")
	if err != nil {
		t.Fatalf("create dst group: %v", err)
	}
	child := &store.Node{
		Name:     "NODE-" + prefix + "-CHILD",
		ParentID: &srcID,
		Enabled:  true,
	}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create direct child: %v", err)
	}
	srcGrp, err := db.GetNode(srcID)
	if err != nil {
		t.Fatalf("get src group: %v", err)
	}
	child, err = db.GetNode(child.ID)
	if err != nil {
		t.Fatalf("get direct child: %v", err)
	}
	return &reparentFixture{SrcGrp: srcGrp, DstGrpID: dstID, DirectChild: child}
}

// createActiveOrderRefSource inserts a pending order whose source_node matches
// the given group name, so the reparent/delete guards' call to
// ListActiveOrdersBySourceRef returns it.
func createActiveOrderRefSource(t *testing.T, db *store.DB, uuid, station, sourceName string) *store.Order {
	t.Helper()
	o := &store.Order{
		EdgeUUID:     uuid,
		StationID:    station,
		OrderType:    "retrieve",
		Status:       "pending",
		SourceNode:   sourceName,
		DeliveryNode: "LINE-TGT",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

// postJSON drives an HTTP handler with a JSON body and returns the recorder.
func postJSON(t *testing.T, handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// captureOrderFailed subscribes to EventOrderFailed on the bus and returns a
// snapshot closure that returns all captured events. Subscribers run
// synchronously on Emit (eventbus.Bus) so no cross-goroutine sleeps are
// needed in callers.
func captureOrderFailed(t *testing.T, bus *engine.EventBus) func() []engine.OrderFailedEvent {
	t.Helper()
	var (
		mu   sync.Mutex
		evts []engine.OrderFailedEvent
	)
	bus.SubscribeTypes(func(e engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := e.Payload.(engine.OrderFailedEvent); ok {
			evts = append(evts, p)
		}
	}, engine.EventOrderFailed)
	return func() []engine.OrderFailedEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]engine.OrderFailedEvent, len(evts))
		copy(out, evts)
		return out
	}
}

// captureNodeUpdated mirrors captureOrderFailed for EventNodeUpdated.
func captureNodeUpdated(t *testing.T, bus *engine.EventBus) func() []engine.NodeUpdatedEvent {
	t.Helper()
	var (
		mu   sync.Mutex
		evts []engine.NodeUpdatedEvent
	)
	bus.SubscribeTypes(func(e engine.Event) {
		mu.Lock()
		defer mu.Unlock()
		if p, ok := e.Payload.(engine.NodeUpdatedEvent); ok {
			evts = append(evts, p)
		}
	}, engine.EventNodeUpdated)
	return func() []engine.NodeUpdatedEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]engine.NodeUpdatedEvent, len(evts))
		copy(out, evts)
		return out
	}
}

// requireOutboxSubject asserts the outbox contains at least one pending entry
// with msg_type "data.<subject>" — the format SendDataToEdge uses when
// enqueuing handler-side notifications via EnqueueOutbox.
func requireOutboxSubject(t *testing.T, db *store.DB, subject string) {
	t.Helper()
	target := "data." + subject
	msgs, err := db.ListPendingOutbox(50)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == target {
			return
		}
	}
	types := make([]string, len(msgs))
	for i, m := range msgs {
		types[i] = m.MsgType
	}
	t.Errorf("no outbox entry with msg_type=%q; got [%s]", target, strings.Join(types, ","))
}

// --- apiCreateNodeGroup -----------------------------------------------------

func TestApiCreateNodeGroup_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	snap := captureNodeUpdated(t, h.engine.EventBus())

	rec := postJSON(t, h.apiCreateNodeGroup, "/api/node-group/create",
		map[string]any{"name": "GRP-CREATE-OK"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == 0 || resp.Name != "GRP-CREATE-OK" {
		t.Errorf("response: got %+v", resp)
	}

	got, err := db.GetNode(resp.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.NodeTypeCode != "NGRP" {
		t.Errorf("node_type_code: got %q, want NGRP", got.NodeTypeCode)
	}

	var matched bool
	for _, e := range snap() {
		if e.NodeID == resp.ID && e.Action == "created" && e.NodeName == "GRP-CREATE-OK" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(created) for node %d, got %+v", resp.ID, snap())
	}
}

func TestApiCreateNodeGroup_MissingName(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiCreateNodeGroup, "/api/node-group/create",
		map[string]any{"name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiAddLane -------------------------------------------------------------

func TestApiAddLane_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	snap := captureNodeUpdated(t, h.engine.EventBus())

	grpID, err := db.CreateNodeGroup("GRP-ADDLANE")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	rec := postJSON(t, h.apiAddLane, "/api/node-group/add-lane",
		map[string]any{"group_id": grpID, "name": "GRP-ADDLANE-L1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == 0 {
		t.Fatalf("lane ID missing in response: %+v", resp)
	}

	lane, err := db.GetNode(resp.ID)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}
	if lane.NodeTypeCode != "LANE" {
		t.Errorf("lane type: got %q, want LANE", lane.NodeTypeCode)
	}
	if lane.ParentID == nil || *lane.ParentID != grpID {
		t.Errorf("lane parent: got %v, want %d", lane.ParentID, grpID)
	}

	var matched bool
	for _, e := range snap() {
		if e.NodeID == resp.ID && e.Action == "created" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(created) for lane %d, got %+v", resp.ID, snap())
	}
}

func TestApiAddLane_MissingFields(t *testing.T) {
	h, _ := testHandlers(t)
	rec := postJSON(t, h.apiAddLane, "/api/node-group/add-lane",
		map[string]any{"group_id": 0, "name": ""})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiReorderLaneSlots ----------------------------------------------------

func TestApiReorderLaneSlots_HappyPath(t *testing.T) {
	h, db := testHandlers(t)
	fx := setupNodeGroupFixture(t, db, "REORDER", 3)
	snap := captureNodeUpdated(t, h.engine.EventBus())

	reversed := []int64{fx.Slots[2].ID, fx.Slots[1].ID, fx.Slots[0].ID}
	rec := postJSON(t, h.apiReorderLaneSlots, "/api/node-group/reorder-lane-slots",
		map[string]any{"lane_id": fx.Lane.ID, "ordered_ids": reversed})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	slots, err := db.ListLaneSlots(fx.Lane.ID)
	if err != nil {
		t.Fatalf("list lane slots: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("slot count: got %d, want 3", len(slots))
	}
	for i, s := range slots {
		if s.ID != reversed[i] {
			t.Errorf("slot[%d]: got node %d, want %d", i, s.ID, reversed[i])
		}
	}

	var matched bool
	for _, e := range snap() {
		if e.NodeID == fx.Lane.ID && e.Action == "reordered" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(reordered) for lane %d, got %+v", fx.Lane.ID, snap())
	}
}

func TestApiReorderLaneSlots_NotALane(t *testing.T) {
	h, db := testHandlers(t)
	fx := setupNodeGroupFixture(t, db, "NOLANE", 1)

	// Pass the NGRP ID (not a LANE) — handler must reject.
	rec := postJSON(t, h.apiReorderLaneSlots, "/api/node-group/reorder-lane-slots",
		map[string]any{"lane_id": fx.Grp.ID, "ordered_ids": []int64{fx.Slots[0].ID}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- apiReparentNode --------------------------------------------------------

// TestApiReparentNode_NonForceBlocked asserts the reparent guard returns 409
// with blocked order IDs and performs zero writes when force=false. Uses a
// physical direct-child-of-NGRP; synthetic nodes (LANE, NGRP) are rejected
// earlier by the synthetic guard and never reach the block check.
func TestApiReparentNode_NonForceBlocked(t *testing.T) {
	h, db := testHandlers(t)
	fx := setupReparentFixture(t, db, "RP-BLOCK")

	blockingOrder := createActiveOrderRefSource(t, db, "rp-block-1", "line-1", fx.SrcGrp.Name)

	rec := postJSON(t, h.apiReparentNode, "/api/node-group/reparent-node",
		map[string]any{
			"node_id":   fx.DirectChild.ID,
			"parent_id": fx.DstGrpID,
			"force":     false,
		})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Error    string  `json:"error"`
		OrderIDs []int64 `json:"order_ids"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.OrderIDs) != 1 || resp.OrderIDs[0] != blockingOrder.ID {
		t.Errorf("order_ids: got %v, want [%d]", resp.OrderIDs, blockingOrder.ID)
	}

	// Zero writes: child parent unchanged, order still pending.
	child, err := db.GetNode(fx.DirectChild.ID)
	if err != nil {
		t.Fatalf("get direct child: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != fx.SrcGrp.ID {
		t.Errorf("child parent after 409: got %v, want unchanged %d", child.ParentID, fx.SrcGrp.ID)
	}
	order := testdb.RequireOrder(t, db, blockingOrder.EdgeUUID)
	if order.Status != "pending" {
		t.Errorf("order status after 409: got %q, want pending", order.Status)
	}
}

// TestApiReparentNode_ForceFailsBlockedOrdersEmitsEvents pins the force-mode
// contract: each blocked order is moved to "failed" (FailOrderAtomic) AND an
// EventOrderFailed is emitted with EdgeUUID/StationID populated — the fields
// the engine's notification gate requires to route the failure to Edge. The
// reparent then succeeds.
func TestApiReparentNode_ForceFailsBlockedOrdersEmitsEvents(t *testing.T) {
	h, db := testHandlers(t)
	fx := setupReparentFixture(t, db, "RP-FORCE")

	ord1 := createActiveOrderRefSource(t, db, "rp-force-1", "line-a", fx.SrcGrp.Name)
	ord2 := createActiveOrderRefSource(t, db, "rp-force-2", "line-b", fx.SrcGrp.Name)

	failedSnap := captureOrderFailed(t, h.engine.EventBus())

	rec := postJSON(t, h.apiReparentNode, "/api/node-group/reparent-node",
		map[string]any{
			"node_id":   fx.DirectChild.ID,
			"parent_id": fx.DstGrpID,
			"force":     true,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Orders moved to "failed".
	if got := testdb.RequireOrder(t, db, ord1.EdgeUUID); got.Status != "failed" {
		t.Errorf("order1 status: got %q, want failed", got.Status)
	}
	if got := testdb.RequireOrder(t, db, ord2.EdgeUUID); got.Status != "failed" {
		t.Errorf("order2 status: got %q, want failed", got.Status)
	}

	// Child moved to new parent.
	child, err := db.GetNode(fx.DirectChild.ID)
	if err != nil {
		t.Fatalf("get direct child: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != fx.DstGrpID {
		t.Errorf("child parent: got %v, want %d", child.ParentID, fx.DstGrpID)
	}

	// EventOrderFailed emitted per blocked order, with EdgeUUID/StationID
	// populated. Missing fields here silently skip the Edge notification —
	// this is the TC-38/TC-39-adjacent hardening contract the refactor must
	// preserve.
	seen := map[int64]engine.OrderFailedEvent{}
	for _, e := range failedSnap() {
		if e.ErrorCode == "group_restructured" {
			seen[e.OrderID] = e
		}
	}
	for _, o := range []*store.Order{ord1, ord2} {
		e, ok := seen[o.ID]
		if !ok {
			t.Errorf("no EventOrderFailed(group_restructured) for order %d", o.ID)
			continue
		}
		if e.EdgeUUID != o.EdgeUUID {
			t.Errorf("order %d: EdgeUUID = %q, want %q", o.ID, e.EdgeUUID, o.EdgeUUID)
		}
		if e.StationID != o.StationID {
			t.Errorf("order %d: StationID = %q, want %q", o.ID, e.StationID, o.StationID)
		}
	}
}

// TestApiReparentNode_HappyPathEmitsStructureChanged covers the non-blocked
// path: no orders reference the source group, reparent proceeds, and because
// the old parent is an NGRP the handler enqueues a protocol.SubjectNodeStructureChanged
// message via SendDataToEdge → EnqueueOutbox.
func TestApiReparentNode_HappyPathEmitsStructureChanged(t *testing.T) {
	h, db := testHandlers(t)
	fx := setupReparentFixture(t, db, "RP-HAPPY")

	rec := postJSON(t, h.apiReparentNode, "/api/node-group/reparent-node",
		map[string]any{
			"node_id":   fx.DirectChild.ID,
			"parent_id": fx.DstGrpID,
			"force":     false,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	child, err := db.GetNode(fx.DirectChild.ID)
	if err != nil {
		t.Fatalf("get direct child: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != fx.DstGrpID {
		t.Errorf("child parent: got %v, want %d", child.ParentID, fx.DstGrpID)
	}

	requireOutboxSubject(t, db, protocol.SubjectNodeStructureChanged)
}

// --- apiDeleteNodeGroup -----------------------------------------------------

func TestApiDeleteNodeGroup_NonForceBlocked(t *testing.T) {
	h, db := testHandlers(t)
	src := setupNodeGroupFixture(t, db, "DEL-BLOCK", 0)

	blockingOrder := createActiveOrderRefSource(t, db, "del-block-1", "line-1", src.Grp.Name)

	rec := postJSON(t, h.apiDeleteNodeGroup, "/api/node-group/delete",
		map[string]any{"id": src.Grp.ID, "force": false})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Error    string  `json:"error"`
		OrderIDs []int64 `json:"order_ids"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.OrderIDs) != 1 || resp.OrderIDs[0] != blockingOrder.ID {
		t.Errorf("order_ids: got %v, want [%d]", resp.OrderIDs, blockingOrder.ID)
	}

	// Zero writes: group still exists, order still pending.
	if _, err := db.GetNode(src.Grp.ID); err != nil {
		t.Errorf("group was deleted despite 409: %v", err)
	}
	order := testdb.RequireOrder(t, db, blockingOrder.EdgeUUID)
	if order.Status != "pending" {
		t.Errorf("order status after 409: got %q, want pending", order.Status)
	}
}

// TestApiDeleteNodeGroup_ForceFailsBlockedOrdersThenDeletes asserts the
// sequence: FailOrderAtomic runs for every blocked order (each with a
// populated EventOrderFailed) BEFORE DeleteNodeGroup executes, and the NGRP
// deletion emits a SubjectNodeStructureChanged outbox message.
func TestApiDeleteNodeGroup_ForceFailsBlockedOrdersThenDeletes(t *testing.T) {
	h, db := testHandlers(t)
	src := setupNodeGroupFixture(t, db, "DEL-FORCE", 0)

	ord1 := createActiveOrderRefSource(t, db, "del-force-1", "line-a", src.Grp.Name)
	ord2 := createActiveOrderRefSource(t, db, "del-force-2", "line-b", src.Grp.Name)

	failedSnap := captureOrderFailed(t, h.engine.EventBus())

	rec := postJSON(t, h.apiDeleteNodeGroup, "/api/node-group/delete",
		map[string]any{"id": src.Grp.ID, "force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Both orders failed.
	if got := testdb.RequireOrder(t, db, ord1.EdgeUUID); got.Status != "failed" {
		t.Errorf("order1 status: got %q, want failed", got.Status)
	}
	if got := testdb.RequireOrder(t, db, ord2.EdgeUUID); got.Status != "failed" {
		t.Errorf("order2 status: got %q, want failed", got.Status)
	}

	// Group deleted.
	if _, err := db.GetNode(src.Grp.ID); err == nil {
		t.Errorf("group %d still exists after force delete", src.Grp.ID)
	}

	// One EventOrderFailed(group_deleted) per blocked order with
	// EdgeUUID+StationID populated.
	seen := map[int64]engine.OrderFailedEvent{}
	for _, e := range failedSnap() {
		if e.ErrorCode == "group_deleted" {
			seen[e.OrderID] = e
		}
	}
	for _, o := range []*store.Order{ord1, ord2} {
		e, ok := seen[o.ID]
		if !ok {
			t.Errorf("no EventOrderFailed(group_deleted) for order %d", o.ID)
			continue
		}
		if e.EdgeUUID != o.EdgeUUID {
			t.Errorf("order %d: EdgeUUID = %q, want %q", o.ID, e.EdgeUUID, o.EdgeUUID)
		}
		if e.StationID != o.StationID {
			t.Errorf("order %d: StationID = %q, want %q", o.ID, e.StationID, o.StationID)
		}
	}

	// NGRP-only path: structure-changed notification enqueued for Edge.
	requireOutboxSubject(t, db, protocol.SubjectNodeStructureChanged)
}
