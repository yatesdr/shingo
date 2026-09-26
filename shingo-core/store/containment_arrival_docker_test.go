//go:build docker

package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestStampContainmentArrival pins the divert path's arrival stamp: a bin
// landing inside a containment destination for its (still-contained) payload
// carries the hold marker away from the stamp, and everything downstream
// reads it as contained.
//
// THE ASSERTION THAT MATTERS is the FindSourceFIFO pair around the stamp.
// Before this fix a diverted bin stood in the containment area as an
// ordinary available, unclaimed row - and the plant-wide retrieve fallback,
// which scans every node in the plant, would have sourced it straight back
// into production. The marker is the only thing between a contained bin and
// a hungry cell, so the test walks a bin through exactly that transition:
// sourceable, stamped, unsourceable.
func TestStampContainmentArrival(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var btID int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`INSERT INTO bin_types (code, description) VALUES ('QC-TYPE', 'test carrier') RETURNING id`,
	).Scan(&btID), "seed bin type")

	group := &nodes.Node{Name: "QC-GROUP", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(group), "create containment group")
	child := &nodes.Node{Name: "QC-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(child), "create containment slot")
	testutil.MustNoErr(t, db.SetNodeParent(child.ID, group.ID), "slot under group")
	other := &nodes.Node{Name: "QC-UNRELATED", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(other), "create unrelated node")

	// The claim routes PART-A's containment to the GROUP - the expected shape
	// for a containment area, and the shape that makes the subtree match
	// load-bearing: arrivals spread across the group's children, the claim
	// names the parent.
	_, err := db.DB.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code,
		 containment_destination, outbound_destination)
		VALUES ('P1', 'S1', 'PROD-1', 'produce', 'auto', 'PART-A', 'QC-GROUP', 'FGN-1')`)
	testutil.MustNoErr(t, err, "seed containment-routed claim")

	testutil.MustNoErr(t, db.SetPayloadContainment("PART-A", "quality alert", "tester", true),
		"activate containment")

	// CreateBin writes only the carrier (type, label, node, status); the payload,
	// count and confirmation are the manifest's, written through its own doors as
	// testdb.CreateBinAtNode does. Setting them on the struct is silently dropped,
	// and an unconfirmed bin with no payload is never sourceable, so the
	// before-the-stamp read below would find nothing.
	newBin := func(label string, nodeID int64) *bins.Bin {
		b := &bins.Bin{BinTypeID: btID, Label: label, NodeID: &nodeID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "create bin "+label)
		testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, "PART-A", 10), "manifest "+label)
		testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
		return b
	}

	// ── THE BUG THIS CLOSES ────────────────────────────────────────────────
	bin := newBin("QC-BIN-1", child.ID)
	got, err := db.FindSourceBinFIFO("PART-A", 0)
	testutil.MustNoErr(t, err, "tier-5 read before the stamp")
	if got == nil || got.ID != bin.ID {
		t.Fatalf("before the stamp: tier-5 found %+v, want the bin itself (setup wrong)", got)
	}

	stamped, err := db.StampContainmentArrival(bin.ID, child.ID, "PART-A", "containment-divert")
	testutil.MustNoErr(t, err, "stamp on child landing")
	if !stamped {
		t.Fatal("child landing inside the group's destination did not stamp")
	}
	hold, by, err := db.BinQualityHold(bin.ID)
	testutil.MustNoErr(t, err, "read hold back")
	if !hold || by != "containment-divert" {
		t.Fatalf("hold = %v by %q, want true by containment-divert", hold, by)
	}

	// The finder says "nothing" as sql.ErrNoRows (ScanBin's no-match sentinel),
	// which is exactly the answer wanted here.
	after, err := db.FindSourceBinFIFO("PART-A", 0)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tier-5 read after the stamp: %v", err)
	}
	if after != nil {
		t.Fatalf("after the stamp: tier-5 still sources the contained bin %d - containment leaks", after.ID)
	}

	// ── A landing on the group itself stamps too (self at depth 0). ────────
	binGroup := newBin("QC-BIN-2", group.ID)
	stamped, err = db.StampContainmentArrival(binGroup.ID, group.ID, "PART-A", "containment-divert")
	testutil.MustNoErr(t, err, "stamp on group landing")
	if !stamped {
		t.Fatal("landing on the destination group itself did not stamp")
	}

	// ── An unrelated landing stamps nothing. ───────────────────────────────
	binOther := newBin("QC-BIN-3", other.ID)
	stamped, err = db.StampContainmentArrival(binOther.ID, other.ID, "PART-A", "containment-divert")
	testutil.MustNoErr(t, err, "stamp on unrelated landing")
	if stamped {
		t.Fatal("an unrelated node stamped as a containment arrival")
	}
	hold, _, err = db.BinQualityHold(binOther.ID)
	testutil.MustNoErr(t, err, "read hold on unrelated landing")
	if hold {
		t.Fatal("unrelated landing left a hold marker")
	}

	// ── An operator's hold is never overwritten by the mechanism. ──────────
	binOp := newBin("QC-BIN-4", child.ID)
	testutil.MustNoErr(t, db.SetBinQualityHold(binOp.ID, true, "op-1"), "operator holds the bin")
	stamped, err = db.StampContainmentArrival(binOp.ID, child.ID, "PART-A", "containment-divert")
	testutil.MustNoErr(t, err, "stamp over an operator hold")
	if stamped {
		t.Fatal("the mechanism's stamp overwrote an operator hold")
	}
	_, by, err = db.BinQualityHold(binOp.ID)
	testutil.MustNoErr(t, err, "read operator hold")
	if by != "op-1" {
		t.Fatalf("hold_by = %q, want the operator's op-1", by)
	}

	// ── Containment deactivated mid-flight: the landing is ordinary stock. ─
	testutil.MustNoErr(t, db.SetPayloadContainment("PART-A", "alert cleared", "tester", false),
		"deactivate containment")
	binOff := newBin("QC-BIN-5", child.ID)
	stamped, err = db.StampContainmentArrival(binOff.ID, child.ID, "PART-A", "containment-divert")
	testutil.MustNoErr(t, err, "stamp with containment off")
	if stamped {
		t.Fatal("stamped a bin whose payload is no longer contained")
	}

	// ── A flagged payload with no routed claim stamps nothing. ─────────────
	testutil.MustNoErr(t, db.SetPayloadContainment("PART-B", "no route yet", "tester", true),
		"activate containment for PART-B")
	binB := &bins.Bin{
		BinTypeID: btID, Label: "QC-BIN-6", NodeID: &child.ID, Status: "available",
		PayloadCode: "PART-B", UOPRemaining: 10, ManifestConfirmed: true,
	}
	testutil.MustNoErr(t, db.CreateBin(binB), "create PART-B bin")
	stamped, err = db.StampContainmentArrival(binB.ID, child.ID, "PART-B", "containment-divert")
	testutil.MustNoErr(t, err, "stamp for unclaimed payload")
	if stamped {
		t.Fatal("stamped a landing for a payload no claim routes")
	}
}
