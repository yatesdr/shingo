//go:build docker

package messaging

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
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
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.HandleNodeListRequest(env, &protocol.NodeListRequest{})

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
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.unknown"},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.HandleNodeListRequest(env, &protocol.NodeListRequest{})

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

// TestNodeListResponse_LoaderBuildFailure_SendsNothing verifies that a
// BuildLoaderInfos failure suppresses the node list response entirely.
//
// The Loaders field is omitempty, so a loaders-less NodeListResponse is
// indistinguishable on the Edge from "no loaders configured" — the Edge would
// call ReplaceCoreLoaders(nil) and truncate all five loader cache tables in a
// committed transaction. The handler must send NOTHING and let the Edge keep
// its last-known-good cache until the next tick.
func TestNodeListResponse_LoaderBuildFailure_SendsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Break the loader projection deliberately: hide the table it reads.
	if _, err := db.Exec(`ALTER TABLE bin_loaders RENAME TO bin_loaders_hidden`); err != nil {
		t.Fatalf("hide bin_loaders: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`ALTER TABLE bin_loaders_hidden RENAME TO bin_loaders`); err != nil {
			t.Fatalf("restore bin_loaders: %v", err)
		}
	})

	resp := &captureResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	env := &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.unknown"},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	svc.HandleNodeListRequest(env, &protocol.NodeListRequest{})

	if len(resp.replies) != 0 {
		t.Fatalf("expected 0 replies on loader build failure, got %d", len(resp.replies))
	}
}

func nodeNames(nodes []protocol.NodeInfo) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = fmt.Sprintf("%s(%s)", n.Name, n.NodeType)
	}
	return names
}

// TestHeartbeatAck_CarriesTheConfiguredPlantTimezone is Core's half of the
// zone-inheritance contract: a site is configured once here and every edge
// picks it up from its heartbeat ack, instead of the zone being typed into each
// box's yaml.
//
// THE VALUE SENT IS THE CONFIGURED ONE, NOT THE RESOLVED ONE. Resolving folds
// in Core's own default, so an unconfigured site would broadcast that default
// to the whole fleet with the confidence of an answer — the blank-zone warnings
// on /edges would clear and a value nobody chose would become the plant's
// clock. Empty has to stay empty, which is the second case below.
func TestHeartbeatAck_CarriesTheConfiguredPlantTimezone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	ackTimezone := func(configured string) string {
		t.Helper()
		resp := &captureResponder{}
		svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
		svc.SetPlantTimezone(configured)

		svc.HandleEdgeHeartbeat(feedsPinEnv("stn-tz-test"), &protocol.EdgeHeartbeat{StationID: "stn-tz-test"})

		for _, r := range resp.replies {
			if r.subject != protocol.SubjectEdgeHeartbeatAck {
				continue
			}
			ack, ok := r.payload.(*protocol.EdgeHeartbeatAck)
			if !ok {
				t.Fatalf("ack payload is %T, want *protocol.EdgeHeartbeatAck", r.payload)
			}
			return ack.Timezone
		}
		t.Fatal("no heartbeat ack was sent")
		return ""
	}

	if got := ackTimezone("America/Chicago"); got != "America/Chicago" {
		t.Errorf("ack timezone = %q, want America/Chicago — edges cannot inherit a zone Core "+
			"does not send", got)
	}
	if got := ackTimezone(""); got != "" {
		t.Errorf("ack timezone = %q, want empty — an unconfigured Core must propagate nothing, "+
			"or its own default silently becomes the whole site's clock", got)
	}
}

// TestHeartbeatAck_CarriesTheStationsDisplayName: every ack names the station
// as Core's registry holds it now, so a rename on Core reaches the Edge on its
// next heartbeat; a name cleared on Core is sent as "" (Core owns the name, so
// empty is a value); a station with no row is sent no name at all (nil), so the
// Edge keeps the one it holds.
func TestHeartbeatAck_CarriesTheStationsDisplayName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "stn-name-test"
	_, err := db.EnrollEdge(st, "PLANT / EDGE-2")
	testutil.MustNoErr(t, err, "enroll edge")

	ackName := func(uid string) *string {
		t.Helper()
		resp := &captureResponder{}
		svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
		svc.HandleEdgeHeartbeat(feedsPinEnv(uid), &protocol.EdgeHeartbeat{StationID: uid})
		for _, r := range resp.replies {
			if ack, ok := r.payload.(*protocol.EdgeHeartbeatAck); ok && r.subject == protocol.SubjectEdgeHeartbeatAck {
				return ack.DisplayName
			}
		}
		t.Fatal("no heartbeat ack was sent")
		return nil
	}
	want := func(uid, name string) {
		t.Helper()
		got := ackName(uid)
		if got == nil || *got != name {
			t.Errorf("ack display name for %s = %v, want %q", uid, got, name)
		}
	}

	want(st, "PLANT / EDGE-2")
	_, err = db.RenameEdge(st, "PLANT / EDGE-3")
	testutil.MustNoErr(t, err, "rename")
	want(st, "PLANT / EDGE-3")
	_, err = db.RenameEdge(st, "")
	testutil.MustNoErr(t, err, "clear the name")
	want(st, "")
	if got := ackName("stn-name-never-enrolled"); got != nil {
		t.Errorf("ack display name for a station with no row = %q, want none (nil): Core has no "+
			"answer for it, and the Edge must keep the name it holds", *got)
	}
}

// TestHandleEdgeCells_TheCatalogIsTheStationsWholeSet: an edge.cells is the
// sending station's whole catalog. Its cells are the station's, live; a cell it
// no longer names goes stale; an empty catalog leaves none live, which the
// register (non-empty gate) cannot do. The station is the envelope's, and
// another station's cells are not touched.
func TestHandleEdgeCells_TheCatalogIsTheStationsWholeSet(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	from := func(st string) *protocol.Envelope {
		return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: st}}
	}
	cell := func(label string, proc int64) protocol.CellCatalogEntry {
		return protocol.CellCatalogEntry{CellLabel: label,
			Processes: []protocol.CellProcessBinding{{ProcessID: proc, StyleID: 1, PLCName: label, TagName: "T"}}}
	}
	live := func(st string) []string {
		t.Helper()
		cells, err := db.ListEdgeCells(st)
		testutil.MustNoErr(t, err, "list edge cells")
		var out []string
		for _, c := range cells {
			if !c.Stale {
				out = append(out, c.CellLabel)
			}
		}
		return out
	}
	want := func(st string, labels ...string) {
		t.Helper()
		if got := live(st); strings.Join(got, ",") != strings.Join(labels, ",") {
			t.Errorf("%s's live cells = %v, want %v", st, got, labels)
		}
	}

	svc.HandleEdgeCells(from("stn-cells-a"), &protocol.CellCatalog{Cells: []protocol.CellCatalogEntry{cell("PLC-1", 1), cell("PLC-2", 2)}})
	svc.HandleEdgeCells(from("stn-cells-b"), &protocol.CellCatalog{Cells: []protocol.CellCatalogEntry{cell("PLC-9", 9)}})
	want("stn-cells-a", "PLC-1", "PLC-2")
	want("stn-cells-b", "PLC-9")

	svc.HandleEdgeCells(from("stn-cells-a"), &protocol.CellCatalog{Cells: []protocol.CellCatalogEntry{cell("PLC-2", 2)}})
	want("stn-cells-a", "PLC-2")
	svc.HandleEdgeCells(from("stn-cells-a"), &protocol.CellCatalog{Cells: []protocol.CellCatalogEntry{}})
	want("stn-cells-a")
	want("stn-cells-b", "PLC-9")
}

// TestRegistrationAndAck_NameOnlyAVerifiedEphemeralTopic pins D2's advertising
// half: the registration reply and every heartbeat ack name the ephemeral
// topic the messaging client verified, read at send time, and name nothing
// when it verified none or none is wired. Nothing named is no key at all on
// the wire, so an ack to an older Edge is the ack it always got.
func TestRegistrationAndAck_NameOnlyAVerifiedEphemeralTopic(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "stn-eph-test"
	_, err := db.EnrollEdge(st, "")
	testutil.MustNoErr(t, err, "enroll edge")

	named := func(wire bool, verified string) (reg *protocol.EdgeRegistered, hb *protocol.EdgeHeartbeatAck) {
		t.Helper()
		resp := &captureResponder{}
		svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
		if wire {
			svc.SetEphemeralTopic(func() string { return verified })
		}
		svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})
		svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{StationID: st, Feeds: map[string]string{}})
		for _, r := range resp.replies {
			switch p := r.payload.(type) {
			case *protocol.EdgeRegistered:
				reg = p
			case *protocol.EdgeHeartbeatAck:
				hb = p
			}
		}
		if reg == nil || hb == nil {
			t.Fatalf("replies %v: want a registration reply and a heartbeat ack", resp.replies)
		}
		return reg, hb
	}

	reg, hb := named(true, "shingo.ephemeral")
	if reg.EphemeralTopic != "shingo.ephemeral" || hb.EphemeralTopic != "shingo.ephemeral" {
		t.Errorf("verified topic: registration names %q, ack %q; want shingo.ephemeral on both", reg.EphemeralTopic, hb.EphemeralTopic)
	}
	for _, c := range []struct {
		name string
		wire bool
	}{{"a client that verified none", true}, {"no client wired", false}} {
		reg, hb := named(c.wire, "")
		if reg.EphemeralTopic != "" || hb.EphemeralTopic != "" {
			t.Errorf("%s: registration names %q, ack %q; want nothing", c.name, reg.EphemeralTopic, hb.EphemeralTopic)
		}
		raw, err := json.Marshal(hb)
		testutil.MustNoErr(t, err, "marshal ack")
		if strings.Contains(string(raw), "ephemeral_topic") {
			t.Errorf("%s: the ack carries the key on the wire: %s", c.name, raw)
		}
	}
}
