//go:build docker

package www

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/plantclaims"
)

// A PARTIAL AT AN UNLOADER, and what the unloader does with it.
//
// The clear: an unloader's CLEAR on a carrier holding a partial count. The
// stage-2 pull: a partial standing in the stage-2 wait group beside a bare
// cart. Each row carries today (the tree before produce partials) and after
// (under it); the test asserts after, and a row whose after differs names its
// ruling.

func TestProducePartialsPin_ClearTakesThePartial(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PP-PARTIAL")
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, sd.Payload.Code, 7), "a partial count")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", map[string]any{"node_name": sd.StorageNode.Name})
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload")
	rows, err := audit.ListBinUOPByBin(db.DB, b.ID, 10, 0)
	testutil.MustNoErr(t, err, "ledger")
	op := "-"
	if len(rows) > 0 {
		op = fmt.Sprintf("%s %d->%d", rows[0].Op, derefInt(rows[0].BeforeUOP), rows[0].AfterUOP)
	}
	seen := fmt.Sprintf("status=%d uop=%d part=%q ledger=%s", rec.Code, got.UOPRemaining, got.PayloadCode, op)
	// Core's clear refuses nothing on count: the partial's count goes to 0 and
	// its part with it, one clear_for_reuse row recording the 7.
	today := `status=200 uop=0 part="" ledger=clear_for_reuse 7->0`
	after := today
	t.Logf("PIN clear/partial-at-a-plain-node %s", seen)
	if seen != after {
		t.Errorf("clear of a partial: got %s, want %s (today %s)", seen, after, today)
	}
}

func derefInt(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func TestProducePartialsPin_StageTwoPullSkipsThePartial(t *testing.T) {
	t.Parallel()
	_, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "PP-WAIT", IsSynthetic: true, NodeTypeID: &ngrp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "the wait group")
	var slots []*nodes.Node
	for i := 1; i <= 2; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("PP-WAIT-%d", i), ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(s), "slot")
		slots = append(slots, s)
	}
	partial := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slots[0].ID, "PP-WAIT-PARTIAL")
	testutil.MustNoErr(t, db.SetBinManifest(partial.ID, `{"items":[]}`, sd.Payload.Code, 7), "a partial")
	bare := testdb.CreateBinAtNode(t, db, sd.Payload.Code, slots[1].ID, "PP-WAIT-BARE")
	marker, err := db.EnsureBareMarker(bare.BinTypeID)
	testutil.MustNoErr(t, err, "bare marker")
	_, err = db.DB.Exec(`UPDATE bins SET bin_type_id=$1, payload_code='', manifest=NULL, uop_remaining=0 WHERE id=$2`,
		marker, bare.ID)
	testutil.MustNoErr(t, err, "a bare cart")

	carts, err := bins.ListBareInGroup(db.DB, grp.ID)
	testutil.MustNoErr(t, err, "the pull's candidates")
	var labels []string
	for _, c := range carts {
		labels = append(labels, c.Label)
	}
	seen := fmt.Sprint(labels)
	// The pull moves bare carts only: a partial in the wait group is never a
	// candidate, and stays where it stands.
	today := "[PP-WAIT-BARE]"
	after := today
	t.Logf("PIN stage-2-pull/partial+bare %s", seen)
	if seen != after {
		t.Errorf("stage-2 pull candidates: got %s, want %s (today %s)", seen, after, today)
	}
}

// ppHome makes node a partial home for a part of capacity 40: a produce claim
// names it, mirrored as the plant claims feed writes it.
func ppHome(t *testing.T, db *store.DB, node *nodes.Node, part string) {
	t.Helper()
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: part, UOPCapacity: 40}), "the part")
	testutil.MustNoErr(t, db.ReplacePlantClaims(part+"-PROC",
		[]plantclaims.StyleRow{{ProcessID: part + "-PROC", StyleID: part + "-STY", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: part + "-PROC", StyleID: part + "-STY", CoreNodeName: part + "-LINE",
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: part,
			PartialDestination: node.Name}}, 0), "a claim keeps its partials there")
}

// TestProducePartialsPin_ClearRefusesAHeldPartial: a partial standing in a home
// of its part is held. Core's clear refuses it on one tap (today it zeroed it)
// and clears it on the second, UNLOAD, answering with the count it cleared.
func TestProducePartialsPin_ClearRefusesAHeldPartial(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	ppHome(t, db, sd.StorageNode, "PPC-X")
	b := testdb.CreateBinAtNode(t, db, "PPC-X", sd.StorageNode.ID, "PPC-HELD")
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, "PPC-X", 7), "a partial in its home")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", map[string]any{"node_name": sd.StorageNode.Name})
	if rec.Code != http.StatusConflict {
		t.Fatalf("one tap on a held partial: status %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.UOPRemaining != 7 || got.PayloadCode != "PPC-X" {
		t.Fatalf("the refused clear changed the bin: %d of %q", got.UOPRemaining, got.PayloadCode)
	}

	rec = postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", map[string]any{"node_name": sd.StorageNode.Name, "unload": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("UNLOAD: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp protocol.BinClearResponse
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.ClearedUOP != 7 {
		t.Errorf("cleared_uop = %d, want the 7 Core cleared", resp.ClearedUOP)
	}
	got, err = db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.UOPRemaining != 0 || got.PayloadCode != "" {
		t.Errorf("after UNLOAD the bin holds %d of %q, want cleared", got.UOPRemaining, got.PayloadCode)
	}
}

// TestProducePartialsPin_LoadAtAHomeNeedsACount: a LOAD at a window inside a
// partial home of its part must say how many it holds (today the standard pack
// was assumed, and a real partial read as full); with the count it loads, and
// the carrier is held from that moment.
func TestProducePartialsPin_LoadAtAHomeNeedsACount(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	ppHome(t, db, sd.StorageNode, "PPL-X")
	b := testdb.CreateBinAtNode(t, db, "", sd.StorageNode.ID, "PPL-EMPTY")

	rec := postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": "PPL-X", "manifest": []map[string]any{{"part_number": "P1"}}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a LOAD with no count at a partial home: status %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, h.apiBinLoad, "/api/telemetry/bin-load", map[string]any{
		"node_name": sd.StorageNode.Name, "payload_code": "PPL-X", "uop_count": 12,
		"manifest": []map[string]any{{"part_number": "P1"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("a LOAD declaring 12: status %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var held sql.NullTime
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT held_since FROM bins WHERE id = $1`, b.ID).Scan(&held), "held_since")
	if !held.Valid {
		t.Error("a partial declared in its home is not held (held_since NULL)")
	}
}

// TestProducePartialsPin_HeldSinceIsTheArrival: held_since is written as a
// carrier lands — now, for an unfinished one landing in a home of its part —
// and cleared by a landing anywhere else.
func TestProducePartialsPin_HeldSinceIsTheArrival(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	ppHome(t, db, sd.StorageNode, "PPA-X")
	b := testdb.CreateBinAtNode(t, db, "PPA-X", sd.LineNode.ID, "PPA-PARTIAL")
	land := func(nodeID int64, what string) {
		t.Helper()
		_, err := h.engine.BinService().ApplyArrival(b.ID, nodeID, false, nil, 0)
		testutil.MustNoErr(t, err, what)
	}
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, "PPA-X", 9), "a partial at the line")
	heldSince := func() sql.NullTime {
		var v sql.NullTime
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT held_since FROM bins WHERE id = $1`, b.ID).Scan(&v), "held_since")
		return v
	}
	land(sd.StorageNode.ID, "it lands in its home")
	if !heldSince().Valid {
		t.Fatal("a partial landing in its home is not held")
	}
	land(sd.LineNode.ID, "it lands back at the line")
	if heldSince().Valid {
		t.Error("a carrier that left its home is still held")
	}
}
