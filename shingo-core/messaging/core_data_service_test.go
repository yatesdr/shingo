//go:build docker

package messaging

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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

		svc.HandleEdgeHeartbeat(feedsPinEnv("stn-tz-test"), &protocol.EdgeHeartbeat{})

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
		svc.HandleEdgeHeartbeat(feedsPinEnv(uid), &protocol.EdgeHeartbeat{})
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

// TestHeartbeatAck_TheOwnedMapPerFamily: a heartbeat that quotes the owned map
// is answered per family from what Core holds of that station, the digest
// always and the members only where it differs from the quote, and never with
// a claims or bin_watch key; a heartbeat with no owned map is answered none.
// A family the heartbeat leaves off is not answered; one
// whose read fails is left off; a failed registry write leaves off the two
// read from the database (claims, cells) and answers the watch list Core holds
// in memory.
func TestHeartbeatAck_TheOwnedMapPerFamily(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st, bare = "stn-owned-held", "stn-owned-bare"
	for _, uid := range []string{st, bare} {
		_, err := db.EnrollEdge(uid, "")
		testutil.MustNoErr(t, err, "enroll "+uid)
	}
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	// What Core holds of st: one process's report row, a watch list and a cell
	// catalog, the last two through their handlers.
	testutil.MustNoErr(t, db.RecordPlantClaimsReport("OWN-P1", st, "claims-d1", time.Now().UTC()), "record a report row")
	svc.HandleBinWatch(feedsPinEnv(st), &protocol.BinWatchList{Digest: "watch-d1", Nodes: []string{"OWN-N1"}})
	svc.HandleEdgeCells(feedsPinEnv(st), &protocol.CellCatalog{Digest: "cells-d1", Cells: []protocol.CellCatalogEntry{
		{CellLabel: "OWN-PLC", Processes: []protocol.CellProcessBinding{{ProcessID: 1, StyleID: 1, PLCName: "OWN-PLC", TagName: "T"}}},
	}})
	heldMembers := map[string]map[string]string{
		protocol.OwnedClaims:   {"OWN-P1": "claims-d1"},
		protocol.OwnedBinWatch: {protocol.SingletonMember: "watch-d1"},
		protocol.OwnedCells:    {protocol.SingletonMember: "cells-d1"},
	}
	digestOf := func(members map[string]string) string {
		t.Helper()
		d, err := protocol.OwnedDigest(members)
		testutil.MustNoErr(t, err, "owned digest")
		return d
	}
	holds := map[string]string{}
	stale := map[string]string{}
	for _, f := range protocol.AllOwned() {
		holds[f] = digestOf(heldMembers[f])
		stale[f] = "stale-" + f
	}
	none := digestOf(nil)

	ack := func(uid string, owned map[string]string) *protocol.EdgeHeartbeatAck {
		t.Helper()
		resp.events = nil
		svc.HandleEdgeHeartbeat(feedsPinEnv(uid), &protocol.EdgeHeartbeat{
			Feeds: map[string]string{protocol.FeedBins: ""}, Owned: owned})
		for _, e := range resp.events {
			if a, ok := e.payload.(*protocol.EdgeHeartbeatAck); ok && e.kind == "reply" {
				return a
			}
		}
		t.Fatalf("no heartbeat ack in %v", resp.trace())
		return nil
	}
	answered := func(a *protocol.EdgeHeartbeatAck) []string {
		out := []string{}
		for f := range a.Owned {
			out = append(out, f)
		}
		sort.Strings(out)
		return out
	}
	// Quoting what Core holds: each family converged, its digest alone.
	a := ack(st, holds)
	for _, f := range protocol.AllOwned() {
		if got := a.Owned[f]; got.Digest != holds[f] || got.Members != nil {
			t.Errorf("converged %s: answered %+v, want digest %s and no members", f, got, holds[f])
		}
	}
	raw, err := json.Marshal(a)
	testutil.MustNoErr(t, err, "marshal ack")
	if s := string(raw); strings.Contains(s, `"claims":null`) || strings.Contains(s, `"bin_watch"`) || strings.Contains(s, `"members"`) {
		t.Errorf("converged: the ack on the wire is %s, want no claims key, no bin_watch and no members", s)
	}

	// Quoting something else: the members Core holds, family by family.
	a = ack(st, stale)
	for _, f := range protocol.AllOwned() {
		if got := a.Owned[f]; got.Digest != holds[f] || !reflect.DeepEqual(got.Members, heldMembers[f]) {
			t.Errorf("differs %s: answered %+v, want digest %s and members %v", f, got, holds[f], heldMembers[f])
		}
	}

	// A station Core holds nothing of: no members, at the digest of none.
	a = ack(bare, stale)
	for _, f := range protocol.AllOwned() {
		if got, ok := a.Owned[f]; !ok || got.Digest != none || len(got.Members) != 0 {
			t.Errorf("holds none %s: answered %+v (present %v), want digest %s and no members", f, got, ok, none)
		}
	}

	// A family the heartbeat leaves off is not answered; none quoted is an
	// empty answer, never nil, which would read as a Core without the map.
	a = ack(st, map[string]string{protocol.OwnedClaims: holds[protocol.OwnedClaims]})
	if got := answered(a); !reflect.DeepEqual(got, []string{protocol.OwnedClaims}) {
		t.Errorf("one family quoted: answered %v, want only %s", got, protocol.OwnedClaims)
	}
	if a = ack(st, map[string]string{}); a.Owned == nil || len(a.Owned) != 0 {
		t.Errorf("an empty owned map: answered %v, want {} (not nil)", a.Owned)
	}

	// No owned map: no answer.
	if a = ack(st, nil); a.Owned != nil {
		t.Errorf("no owned map: answered %v, want nil", a.Owned)
	}

	// A failed read leaves its family off; a failed registry write leaves off
	// both families read from the database.
	hideTable(t, db, "edge_cells")
	if got := answered(ack(st, holds)); !reflect.DeepEqual(got, []string{protocol.OwnedBinWatch, protocol.OwnedClaims}) {
		t.Errorf("cells unreadable: answered %v, want bins.watch and claims", got)
	}
	hideTable(t, db, "edge_registry")
	if got := answered(ack(st, holds)); !reflect.DeepEqual(got, []string{protocol.OwnedBinWatch}) {
		t.Errorf("registry write failed: answered %v, want bins.watch alone", got)
	}
}

// TestHandleEdgeCells_TheCatalogIsTheStationsWholeSet: an edge.cells is the
// sending station's whole catalog. Its cells are the station's, live; a cell it
// no longer names goes stale; an empty catalog leaves none live, which the
// register (non-empty gate) cannot do. The station is the envelope's, and
// another station's cells are not touched; a message with no sender keeps
// nothing.
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

	// No sender: no station to apply it to, so nothing is kept, not even as
	// the cells of station "".
	svc.HandleEdgeCells(from(""), &protocol.CellCatalog{Cells: []protocol.CellCatalogEntry{cell("PLC-0", 3)}})
	var unnamed int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM edge_cells WHERE station_uid = ''`).Scan(&unnamed), "count cells of no station")
	if unnamed != 0 {
		t.Errorf("an edge.cells with no sender left %d cells under station \"\", want none", unnamed)
	}
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
		svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{Hostname: "h", Version: "v"})
		svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{Feeds: map[string]string{}})
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
