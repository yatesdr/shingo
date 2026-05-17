//go:build docker

package messaging

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// captureResponder records replyData calls for test assertions.
type captureResponder struct {
	replies []replyEntry
}

type replyEntry struct {
	env     *protocol.Envelope
	subject string
	payload any
}

func (r *captureResponder) dbg(format string, args ...any) {}
func (r *captureResponder) replyData(env *protocol.Envelope, subject string, payload any) {
	r.replies = append(r.replies, replyEntry{env: env, subject: subject, payload: payload})
}
func (r *captureResponder) sendData(subject, stationID string, payload any) {}

// TestNodeListResponse_IncludesNodeGroups verifies that NGRP node groups are
// included in the node list sync response when their children are assigned to
// the requesting station.
//
// Bug: handleNodeListRequest and ListNodesForStation filter out synthetic/NGRP
// nodes, so edge stations never receive node group information. Edge has UI
// support for NGRP (shows "(group)" suffix, expands children), but core never
// sends them. When the operator hits "Sync Nodes" on edge, NGRP nodes are
// missing from the dropdown, even though edge can display them.
func TestNodeListResponse_IncludesNodeGroups(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// NGRP node type is created by migrations — look it up
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Create NGRP node (the group container)
	grpNode := &nodes.Node{
		Name:        "STORAGE-G1",
		IsSynthetic: true,
		Enabled:     true,
		NodeTypeID:  &grpType.ID,
	}
	testutil.MustNoErr(t, db.CreateNode(grpNode), "create NGRP node")

	// Create a physical child node under the NGRP
	childNode := &nodes.Node{
		Name:     "SLOT-1",
		Enabled:  true,
		ParentID: &grpNode.ID,
	}
	testutil.MustNoErr(t, db.CreateNode(childNode), "create child node")

	// Assign the child to a station (this is how station-scoped queries work)
	stationID := "edge.line1"
	testutil.MustNoErr(t, db.AssignNodeToStation(childNode.ID, stationID), "assign child to station")

	// Create the data service and request node list
	resp := &captureResponder{}
	svc := newCoreDataService(db, resp)

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.handleNodeListRequest(env)

	if len(resp.replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(resp.replies))
	}

	var nodeListResp protocol.NodeListResponse
	payloadBytes, _ := json.Marshal(resp.replies[0].payload)
	testutil.MustNoErr(t, json.Unmarshal(payloadBytes, &nodeListResp), "unmarshal response")

	// Build a set of node names for easy lookup
	names := make(map[string]string) // name -> node_type
	for _, n := range nodeListResp.Nodes {
		names[n.Name] = n.NodeType
	}

	// The NGRP node must be present so edge knows about the group
	if _, ok := names["STORAGE-G1"]; !ok {
		t.Errorf("NGRP node STORAGE-G1 missing from node list response (station-scoped path)")
		t.Logf("nodes received: %v", nodeNames(nodeListResp.Nodes))
	}

	// The child should appear with dot notation (parent.child)
	dotName := "STORAGE-G1.SLOT-1"
	if _, ok := names[dotName]; !ok {
		t.Errorf("child node %q missing from node list response (station-scoped path)", dotName)
		t.Logf("nodes received: %v", nodeNames(nodeListResp.Nodes))
	}
}

// TestNodeListResponse_GlobalPath_IncludesNodeGroups verifies the global
// (non-station-scoped) fallback path also includes NGRP nodes.
func TestNodeListResponse_GlobalPath_IncludesNodeGroups(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// NGRP node type is created by migrations — look it up
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}

	// Create NGRP node
	grpNode := &nodes.Node{
		Name:        "WH-SUPER",
		IsSynthetic: true,
		Enabled:     true,
		NodeTypeID:  &grpType.ID,
	}
	testutil.MustNoErr(t, db.CreateNode(grpNode), "create NGRP node")

	// Create a physical child
	childNode := &nodes.Node{
		Name:     "LANE-A1",
		Enabled:  true,
		ParentID: &grpNode.ID,
	}
	testutil.MustNoErr(t, db.CreateNode(childNode), "create child node")

	// No station assignment — triggers global path fallback

	resp := &captureResponder{}
	svc := newCoreDataService(db, resp)

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.unknown"},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.handleNodeListRequest(env)

	var nodeListResp protocol.NodeListResponse
	payloadBytes, _ := json.Marshal(resp.replies[0].payload)
	testutil.MustNoErr(t, json.Unmarshal(payloadBytes, &nodeListResp), "unmarshal response")

	names := make(map[string]string)
	for _, n := range nodeListResp.Nodes {
		names[n.Name] = n.NodeType
	}

	// NGRP node should be present (parent_id IS NULL, included by first condition)
	if _, ok := names["WH-SUPER"]; !ok {
		t.Errorf("NGRP node WH-SUPER missing from global node list response")
		t.Logf("nodes received: %v", nodeNames(nodeListResp.Nodes))
	}

	// Child with dot notation
	dotName := "WH-SUPER.LANE-A1"
	if _, ok := names[dotName]; !ok {
		t.Errorf("child node %q missing from global node list response", dotName)
		t.Logf("nodes received: %v", nodeNames(nodeListResp.Nodes))
	}
}

func nodeNames(nodes []protocol.NodeInfo) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = fmt.Sprintf("%s(%s)", n.Name, n.NodeType)
	}
	return names
}
