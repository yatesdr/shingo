package store

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ResolveSwapPair anchored on two columns of one runtime row — the record
// fourteen call sites used to write absolutely, each able to drop a live
// sibling it did not own. The staged legs themselves are durable: they carry
// their own process_node_id and their own sibling_order_id, and nothing that
// mutates a runtime slot can unlink them.
//
// ListStagedOrdersByProcessNode and idx_orders_process_node_id had both existed
// for this the whole time with no production caller.
func seedStagedPair(t *testing.T, prefix string, link bool) (db *DB, runtime *processes.RuntimeState, a, b int64) {
	t.Helper()
	db = coverageDB(t)
	processID, err := db.CreateProcess(prefix, "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-N", Code: prefix,
		Name: prefix, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	mk := func(uuid string) int64 {
		id, cerr := db.CreateOrder(uuid, protocol.OrderTypeComplex, &nodeID, false, 1,
			prefix+"-N", "", "", "", false, "PART-X")
		if cerr != nil {
			t.Fatalf("create order %s: %v", uuid, cerr)
		}
		if uerr := db.UpdateOrderStatus(id, "staged"); uerr != nil {
			t.Fatalf("stage order %s: %v", uuid, uerr)
		}
		return id
	}
	a, b = mk(prefix+"-a"), mk(prefix+"-b")
	if link {
		if err := db.LinkOrderSiblings(a, b); err != nil {
			t.Fatalf("link siblings: %v", err)
		}
	}
	// Both runtime pointers nil — the state that used to fall straight to the
	// node-task fallback and, without a task, error out.
	runtime, err = db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	return db, runtime, a, b
}

func TestResolveSwapPair_FindsThePairFromTheDurableRecord(t *testing.T) {
	t.Parallel()
	db, runtime, a, b := seedStagedPair(t, "DURABLE", true)

	evacID, supplyID, err := ResolveSwapPair(db, runtime, nil)
	if err != nil {
		t.Fatalf("resolve with both runtime pointers nil: %v — the legs are staged on this node "+
			"and linked to each other; losing the runtime pointers must not lose the pair", err)
	}
	if evacID == nil || *evacID != a {
		t.Errorf("evac = %v, want %d (the first staged leg, matching the positional convention)", evacID, a)
	}
	if supplyID == nil || *supplyID != b {
		t.Errorf("supply = %v, want %d", supplyID, b)
	}
}

// Two staged legs that are not linked to each other are not a pair. Guessing
// would be worse than the error: these callers release both halves together.
func TestResolveSwapPair_UnlinkedStagedOrdersAreNotAPair(t *testing.T) {
	t.Parallel()
	db, runtime, _, _ := seedStagedPair(t, "UNLINKED", false)

	if _, _, err := ResolveSwapPair(db, runtime, nil); err == nil {
		t.Fatal("two unlinked staged orders resolved as a pair. They are two single-leg flows " +
			"that happen to share a node, and releasing them together is not the same operation.")
	}
}

// The runtime pointers still win when they are populated: this rung is for the
// case where they say nothing, not a replacement for them.
func TestResolveSwapPair_RuntimePointersStillLead(t *testing.T) {
	t.Parallel()
	db, runtime, a, b := seedStagedPair(t, "PTRLEAD", true)

	// Deliberately the other way round from the durable ordering.
	runtime.StagedOrderID = &b
	runtime.ActiveOrderID = &a

	evacID, supplyID, err := ResolveSwapPair(db, runtime, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if evacID == nil || *evacID != b || supplyID == nil || *supplyID != a {
		t.Errorf("evac/supply = %v/%v, want %d/%d — populated runtime pointers name the legs; "+
			"the durable rung only answers when they do not.", evacID, supplyID, b, a)
	}
}
