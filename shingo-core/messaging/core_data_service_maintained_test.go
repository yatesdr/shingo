//go:build docker

package messaging

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestNodeListResponse_MarksMaintainedGroups: the node list the Edge already
// receives says which groups keep a level of empties, on both the
// station-scoped and the plant-wide path, so the Edge's claim save can refuse
// such a group as a consume line's source the way Core refuses it for an
// unloader. A group with no level, and every other node, carries no flag.
func TestNodeListResponse_MarksMaintainedGroups(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	bt := &bins.BinType{Code: "MG-LIST-TOTE", Description: "maintained list fixture"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	group := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &grpType.ID}
		testutil.MustNoErr(t, db.CreateNode(n), "create group "+name)
		child := &nodes.Node{Name: name + "-S1", Enabled: true, ParentID: &n.ID}
		testutil.MustNoErr(t, db.CreateNode(child), "create child of "+name)
		testutil.MustNoErr(t, db.AssignNodeToStation(child.ID, "edge.mg"), "assign child")
		return n
	}
	empties := group("MG-EMPTIES")
	group("MG-FULLS")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
		GroupNodeID: empties.ID, BinTypeID: bt.ID, Want: 2}), "set level")

	for _, station := range []string{"edge.mg", "edge.unscoped"} {
		resp := &captureResponder{}
		svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
		svc.HandleNodeListRequest(&protocol.Envelope{
			Src: protocol.Address{Role: protocol.RoleEdge, Station: station},
		}, &protocol.NodeListRequest{})
		if len(resp.replies) != 1 {
			t.Fatalf("%s: %d replies, want 1", station, len(resp.replies))
		}
		var got protocol.NodeListResponse
		raw, err := json.Marshal(resp.replies[0].payload)
		testutil.MustNoErr(t, err, "marshal reply")
		testutil.MustNoErr(t, json.Unmarshal(raw, &got), "unmarshal")
		flags := map[string]bool{}
		for _, n := range got.Nodes {
			flags[n.Name] = n.Maintained
		}
		if _, ok := flags["MG-EMPTIES"]; !ok || !flags["MG-EMPTIES"] {
			t.Errorf("%s: MG-EMPTIES not marked maintained: %v", station, nodeNames(got.Nodes))
		}
		for name, m := range flags {
			if m && name != "MG-EMPTIES" {
				t.Errorf("%s: %s marked maintained", station, name)
			}
		}
	}
}
