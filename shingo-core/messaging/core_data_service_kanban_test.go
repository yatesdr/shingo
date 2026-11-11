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

// TestNodeListResponse_MarksKanbans: the node list the Edge already
// receives says which groups keep a level of empties, on both the
// station-scoped and the plant-wide path, so the Edge's claim save can refuse
// such a group as a consume line's source the way Core refuses it for an
// unloader. A group with no level, and every other node, carries no flag.
func TestNodeListResponse_MarksKanbans(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	bt := &bins.BinType{Code: "MG-LIST-TOTE", Description: "kanban list fixture"}
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
	testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{
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
			flags[n.Name] = n.Kanban
		}
		if _, ok := flags["MG-EMPTIES"]; !ok || !flags["MG-EMPTIES"] {
			t.Errorf("%s: MG-EMPTIES not marked kanban: %v", station, nodeNames(got.Nodes))
		}
		for name, m := range flags {
			if m && name != "MG-EMPTIES" {
				t.Errorf("%s: %s marked kanban", station, name)
			}
		}
	}
}

// TestNodeListResponse_SaysWhetherItIsPlantWide: a station with nodes of its
// own gets them, and the reply does not say plant-wide; a station with none
// gets the plant-wide list, and the reply says so. The Edge refuses a name its
// list does not hold, where Core picks, only from a plant-wide list.
func TestNodeListResponse_SaysWhetherItIsPlantWide(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	own := &nodes.Node{Name: "PW-OWN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(own), "create the station's node")
	testutil.MustNoErr(t, db.AssignNodeToStation(own.ID, "edge.pw"), "assign it")
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "PW-OTHER", Enabled: true}), "create another node")

	for _, c := range []struct {
		station   string
		plantWide bool
	}{{"edge.pw", false}, {"edge.unscoped", true}} {
		resp := &captureResponder{}
		svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
		svc.HandleNodeListRequest(&protocol.Envelope{
			Src: protocol.Address{Role: protocol.RoleEdge, Station: c.station},
		}, &protocol.NodeListRequest{})
		if len(resp.replies) != 1 {
			t.Fatalf("%s: %d replies, want 1", c.station, len(resp.replies))
		}
		var got protocol.NodeListResponse
		raw, err := json.Marshal(resp.replies[0].payload)
		testutil.MustNoErr(t, err, "marshal reply")
		testutil.MustNoErr(t, json.Unmarshal(raw, &got), "unmarshal")
		if got.PlantWide != c.plantWide {
			t.Errorf("%s: plant_wide = %v, want %v (nodes %v)", c.station, got.PlantWide, c.plantWide, nodeNames(got.Nodes))
		}
	}
}

// TestNodeListResponse_CarriesEachTypesFloor: the list says each type's floor once,
// by type code and orientation-blind, as the walk's floor rule compares them; a
// type with no size has no entry. The digest covers it, so a floor edit reaches an
// Edge holding the old digest.
func TestNodeListResponse_CarriesEachTypesFloor(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	wide := &bins.BinType{Code: "FL-WIDE", WidthIn: 45, LengthIn: 48}
	long := &bins.BinType{Code: "FL-LONG", WidthIn: 48, LengthIn: 45}
	bare := &bins.BinType{Code: "FL-OWN", WidthIn: 45, LengthIn: 48}
	for _, bt := range []*bins.BinType{wide, long, bare} {
		testutil.MustNoErr(t, db.CreateBinType(bt), "create "+bt.Code)
	}
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	got, err := svc.buildNodeList("edge.floor", true)
	testutil.MustNoErr(t, err, "build")
	if got.Family["FL-WIDE"] != "FL-WIDE" || got.Family["FL-OWN"] != "FL-OWN" {
		t.Errorf("families = %v, want each a family of its own: a footprint decides nothing", got.Family)
	}
	digest := got.Digest

	family, err := db.EnsureCarrierFamily("FL-45")
	testutil.MustNoErr(t, err, "the family")
	testutil.MustNoErr(t, db.SetBinTypeFamily(wide.ID, family), "wide")
	testutil.MustNoErr(t, db.SetBinTypeFamily(long.ID, family), "long")
	got, err = svc.buildNodeList("edge.floor", true)
	testutil.MustNoErr(t, err, "build again")
	if got.Family["FL-WIDE"] != "FL-45" || got.Family["FL-LONG"] != "FL-45" {
		t.Errorf("regrouped = %v, want both of FL-45", got.Family)
	}
	if got.Digest == digest {
		t.Error("a regrouping left the digest as it was: an Edge holding it would never hear")
	}
}
