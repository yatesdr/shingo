package www

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingoedge/store/processes"
)

// The clear-node-orders button destroys both runtime order pointers, and this
// route is unlogged by construction: there is no request logger anywhere under
// www/, so before this line nothing recorded that it had been pressed, let
// alone what it discarded. A node that lost a live staged leg here looked
// exactly like a node that never had one.
//
// No t.Parallel in this file — stdlib log.SetOutput is global.
func TestClearNodeOrders_NamesTheNodeAndBothPointers(t *testing.T) {
	h, r := newTestHandlers(t)
	r.Post("/api/nodes/{id}/clear-orders", h.apiClearNodeOrders)
	db := h.engine.(*stubEngine).db

	processID, err := db.CreateProcess("CLEARLOG", "clear log", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SMN_029", Code: "CL1",
		Name: "Clear Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	// Two live pointers, which is exactly the state worth a record.
	active, err := db.CreateOrder("uuid-clear-active", "retrieve", &nodeID, false, 1, "SMN_029", "", "", "", false, "PART-A")
	if err != nil {
		t.Fatalf("create active order: %v", err)
	}
	staged, err := db.CreateOrder("uuid-clear-staged", "retrieve", &nodeID, false, 1, "SMN_029", "", "", "", false, "PART-A")
	if err != nil {
		t.Fatalf("create staged order: %v", err)
	}
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &active, &staged); err != nil {
		t.Fatalf("seat both pointers: %v", err)
	}

	var buf bytes.Buffer
	prevW, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevFlags) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nodes/"+itoa(nodeID)+"/clear-orders", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear returned %d: %s", rec.Code, rec.Body.String())
	}

	logged := buf.String()
	// Assert on the LABELLED values, not on bare integers: a bare "3" would
	// match the node id in the same line and the assertion would pass against a
	// log that had dropped the pointers entirely.
	for _, want := range []string{
		"clear node orders",
		"SMN_029",
		"active_order_id=" + itoa(active),
		"staged_order_id=" + itoa(staged),
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the clear line does not mention %q. It must name the node and BOTH pointer "+
				"values it is about to destroy — after the write those values are gone and no "+
				"journal search can recover them.\nGot:\n%s", want, logged)
		}
	}

	// And it really did clear.
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("re-read runtime: %v", err)
	}
	if rt.ActiveOrderID != nil || rt.StagedOrderID != nil {
		t.Errorf("pointers survived the clear: active=%v staged=%v", rt.ActiveOrderID, rt.StagedOrderID)
	}
}

// An empty slot must read as "none", not as a zero — the whole value of the
// line is telling apart a slot that held nothing from a slot that held
// something.
func TestClearNodeOrders_EmptySlotsReadAsNone(t *testing.T) {
	h, r := newTestHandlers(t)
	r.Post("/api/nodes/{id}/clear-orders", h.apiClearNodeOrders)
	db := h.engine.(*stubEngine).db

	processID, err := db.CreateProcess("CLEARLOG2", "clear log 2", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SMN_031", Code: "CL2",
		Name: "Empty Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}

	var buf bytes.Buffer
	prevW, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevFlags) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/nodes/"+itoa(nodeID)+"/clear-orders", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear returned %d: %s", rec.Code, rec.Body.String())
	}
	if logged := buf.String(); !strings.Contains(logged, "active_order_id=none") {
		t.Errorf("an empty slot must render as none, so a reader can tell it apart from a slot "+
			"that held an order.\nGot:\n%s", logged)
	}
}
