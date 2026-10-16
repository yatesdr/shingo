//go:build docker

package www

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/nodes"
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
