//go:build docker

package store

import "testing"

func TestNodePayload_AssignUnassignList(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "NP-NODE-1", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	p1 := &Payload{Code: "NP-P1", UOPCapacity: 10}
	db.CreatePayload(p1)
	p2 := &Payload{Code: "NP-P2", UOPCapacity: 20}
	db.CreatePayload(p2)

	if err := db.AssignPayloadToNode(node.ID, p1.ID); err != nil {
		t.Fatalf("AssignPayloadToNode p1: %v", err)
	}
	if err := db.AssignPayloadToNode(node.ID, p2.ID); err != nil {
		t.Fatalf("AssignPayloadToNode p2: %v", err)
	}

	// ListPayloadsForNode
	ps, err := db.ListPayloadsForNode(node.ID)
	if err != nil {
		t.Fatalf("ListPayloadsForNode: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("payloads len = %d, want 2", len(ps))
	}
	codes := map[string]bool{}
	for _, p := range ps {
		codes[p.Code] = true
	}
	if !codes["NP-P1"] || !codes["NP-P2"] {
		t.Errorf("payload codes = %+v, want NP-P1 and NP-P2", codes)
	}

	// ListNodesForPayload
	nodes, err := db.ListNodesForPayload(p1.ID)
	if err != nil {
		t.Fatalf("ListNodesForPayload: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	if nodes[0].Name != "NP-NODE-1" {
		t.Errorf("nodes[0].Name = %q, want %q", nodes[0].Name, "NP-NODE-1")
	}

	// Unassign p1
	if err := db.UnassignPayloadFromNode(node.ID, p1.ID); err != nil {
		t.Fatalf("UnassignPayloadFromNode: %v", err)
	}
	after, _ := db.ListPayloadsForNode(node.ID)
	if len(after) != 1 {
		t.Fatalf("after unassign len = %d, want 1", len(after))
	}
	if after[0].Code != "NP-P2" {
		t.Errorf("remaining code = %q, want NP-P2", after[0].Code)
	}
}

func TestSetNodePayloads_Replaces(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "NP-SET-1", Enabled: true}
	db.CreateNode(node)

	pA := &Payload{Code: "NP-SET-A"}
	db.CreatePayload(pA)
	pB := &Payload{Code: "NP-SET-B"}
	db.CreatePayload(pB)
	pC := &Payload{Code: "NP-SET-C"}
	db.CreatePayload(pC)

	// [A, B]
	if err := db.SetNodePayloads(node.ID, []int64{pA.ID, pB.ID}); err != nil {
		t.Fatalf("SetNodePayloads [A,B]: %v", err)
	}
	first, _ := db.ListPayloadsForNode(node.ID)
	if len(first) != 2 {
		t.Fatalf("after [A,B] len = %d, want 2", len(first))
	}

	// Replace with [C]
	if err := db.SetNodePayloads(node.ID, []int64{pC.ID}); err != nil {
		t.Fatalf("SetNodePayloads [C]: %v", err)
	}
	second, _ := db.ListPayloadsForNode(node.ID)
	if len(second) != 1 {
		t.Fatalf("after [C] len = %d, want 1", len(second))
	}
	if second[0].Code != "NP-SET-C" {
		t.Errorf("after [C] code = %q, want NP-SET-C", second[0].Code)
	}

	// Clear
	if err := db.SetNodePayloads(node.ID, nil); err != nil {
		t.Fatalf("SetNodePayloads nil: %v", err)
	}
	empty, _ := db.ListPayloadsForNode(node.ID)
	if len(empty) != 0 {
		t.Errorf("after clear len = %d, want 0", len(empty))
	}
}

func TestGetEffectivePayloads_InheritFromParent(t *testing.T) {
	db := testDB(t)

	parent := &Node{Name: "NP-EFF-PARENT", Enabled: true}
	db.CreateNode(parent)
	child := &Node{Name: "NP-EFF-CHILD", Enabled: true, ParentID: &parent.ID}
	db.CreateNode(child)

	p := &Payload{Code: "NP-EFF-P"}
	db.CreatePayload(p)

	// Assign at parent. Child has no direct assignments — should inherit.
	db.AssignPayloadToNode(parent.ID, p.ID)

	got, err := db.GetEffectivePayloads(child.ID)
	if err != nil {
		t.Fatalf("GetEffectivePayloads child: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("inherited len = %d, want 1", len(got))
	}
	if got[0].Code != "NP-EFF-P" {
		t.Errorf("inherited code = %q, want NP-EFF-P", got[0].Code)
	}

	// Same function on the parent itself
	parentGot, err := db.GetEffectivePayloads(parent.ID)
	if err != nil {
		t.Fatalf("GetEffectivePayloads parent: %v", err)
	}
	if len(parentGot) != 1 {
		t.Fatalf("parent direct len = %d, want 1", len(parentGot))
	}
}
