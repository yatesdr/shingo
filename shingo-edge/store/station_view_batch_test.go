package store

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// RuntimesForNodes must return, per node, exactly what GetRuntime returns — and
// must simply omit a node that has no row yet, because that omission is the
// signal BuildView uses to fall back to EnsureRuntime (which inserts). If this
// batch invented rows, the "ensure" write would move to a different set of
// nodes than before.
func TestRuntimesForNodes_MatchesPerNodeGet(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	pid, err := db.CreateProcess("P", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	mk := func(name string) int64 {
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: name, Code: name, Name: name, Sequence: 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateProcessNode(%s): %v", name, err)
		}
		return id
	}
	withRow, alsoWithRow, noRow := mk("A"), mk("B"), mk("C")

	for _, id := range []int64{withRow, alsoWithRow} {
		if _, err := db.EnsureProcessNodeRuntime(id); err != nil {
			t.Fatalf("EnsureProcessNodeRuntime(%d): %v", id, err)
		}
	}
	// Give one of them distinguishable state so an all-zero-value bug can't pass.
	active, staged := int64(4242), int64(4243)
	if err := db.UpdateProcessNodeRuntimeOrders(withRow, &active, &staged); err != nil {
		t.Fatalf("UpdateProcessNodeRuntimeOrders: %v", err)
	}

	got, err := processes.RuntimesForNodes(db.DB, []int64{withRow, alsoWithRow, noRow})
	if err != nil {
		t.Fatalf("RuntimesForNodes: %v", err)
	}

	for _, id := range []int64{withRow, alsoWithRow} {
		want, err := processes.GetRuntime(db.DB, id)
		if err != nil {
			t.Fatalf("GetRuntime(%d): %v", id, err)
		}
		if got[id] == nil {
			t.Fatalf("node %d missing from the batch result", id)
		}
		if !reflect.DeepEqual(*got[id], *want) {
			t.Errorf("node %d:\n batched = %+v\n per-node = %+v", id, *got[id], *want)
		}
	}
	if _, present := got[noRow]; present {
		t.Errorf("node %d has no runtime row but appears in the batch result — BuildView would skip the Ensure insert", noRow)
	}
}

// The batched active-order lookup must agree with the per-node call for every
// tile. The cases that matter are the ones a naive IN-list rewrite gets wrong:
// an order that belongs to TWO tiles at once, and a tile with an empty
// CoreNodeName that must not pick up orders with an empty source_node.
func TestListActiveByNodeKeys_MatchesPerNodeCalls(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	pid, err := db.CreateProcess("P", "", "", "", "", false, false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	mk := func(core, code string) int64 {
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: core, Code: code, Name: code, Sequence: 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("CreateProcessNode(%s): %v", code, err)
		}
		return id
	}
	lineNode := mk("ALN_001", "aln-001")   // orders tracked here
	loaderNode := mk("SMN_001", "smn-001") // orders SOURCE from here
	blankNode := mk("", "blank")           // no core node name

	mkOrder := func(uuid string, nodeID *int64, sourceNode string) int64 {
		id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, nodeID, false, 1,
			"DEST", "", sourceNode, "", false, "PAY-1")
		if err != nil {
			t.Fatalf("CreateOrder(%s): %v", uuid, err)
		}
		return id
	}

	// The interesting one: tracked at the line node AND sourcing from the
	// loader node, so BOTH tiles must list it.
	mkOrder("u-shared", &lineNode, "SMN_001")
	// Tracked at the line node, sourcing from somewhere off-board.
	mkOrder("u-line-only", &lineNode, "ELSEWHERE")
	// Tracked at the blank-named node, with an EMPTY source_node. The blank
	// tile must get it via process_node_id; no other tile may pick it up via
	// an empty-string source match.
	mkOrder("u-blank", &blankNode, "")
	// Terminal orders must be excluded by both forms.
	term := mkOrder("u-terminal", &lineNode, "SMN_001")
	if err := db.UpdateOrderStatus(term, "confirmed"); err != nil {
		t.Fatalf("UpdateOrderStatus: %v", err)
	}

	keys := []orders.NodeKey{
		{ProcessNodeID: lineNode, CoreNodeName: "ALN_001"},
		{ProcessNodeID: loaderNode, CoreNodeName: "SMN_001"},
		{ProcessNodeID: blankNode, CoreNodeName: ""},
	}
	got, err := orders.ListActiveByNodeKeys(db.DB, keys)
	if err != nil {
		t.Fatalf("ListActiveByNodeKeys: %v", err)
	}

	for _, k := range keys {
		want, err := db.ListActiveOrdersByProcessNodeOrSource(k.ProcessNodeID, k.CoreNodeName)
		if err != nil {
			t.Fatalf("per-node call for %d: %v", k.ProcessNodeID, err)
		}
		if !equalOrderUUIDs(got[k.ProcessNodeID], want) {
			t.Errorf("node %d (%q):\n batched = %v\n per-node = %v",
				k.ProcessNodeID, k.CoreNodeName, orderUUIDs(got[k.ProcessNodeID]), orderUUIDs(want))
		}
	}

	// Spell the fan-out out explicitly rather than relying on the comparison
	// above, so a regression names itself.
	if n := len(got[lineNode]); n != 2 {
		t.Errorf("line node got %d orders, want 2 (shared + line-only): %v", n, orderUUIDs(got[lineNode]))
	}
	if uuids := orderUUIDs(got[loaderNode]); len(uuids) != 1 || uuids[0] != "u-shared" {
		t.Errorf("loader node got %v, want exactly [u-shared] via source_node", uuids)
	}
	if uuids := orderUUIDs(got[blankNode]); len(uuids) != 1 || uuids[0] != "u-blank" {
		t.Errorf("blank-named node got %v, want exactly [u-blank]", uuids)
	}
	for _, u := range orderUUIDs(got[lineNode]) {
		if u == "u-terminal" {
			t.Error("terminal order leaked into the batched result")
		}
	}
}

func TestListActiveByNodeKeys_EmptyInput(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	got, err := orders.ListActiveByNodeKeys(db.DB, nil)
	if err != nil {
		t.Fatalf("ListActiveByNodeKeys(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func orderUUIDs(os []orders.Order) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.UUID)
	}
	return out
}

// Compare by UUID in order: the full structs are equal too, but a UUID list
// makes a failure readable.
func equalOrderUUIDs(a, b []orders.Order) bool {
	ua, ub := orderUUIDs(a), orderUUIDs(b)
	if len(ua) == 0 && len(ub) == 0 {
		return true
	}
	return reflect.DeepEqual(ua, ub)
}
