//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// bare_carrier_pins_docker_test.go — every empty reader that composes
// EmptyCarrierWhere, over a payload-less carrier of a bare bin type (HLPIN-BARE,
// what a half unloader's stage 1 stamps) and a control of the ordinary DEFAULT
// type.
//
// The carrier stands on a plain bank node and on a plain group slot, never on a
// loader position, so the loader-position arm cannot be what hides it: this
// isolates the type as the only variable.

type bareFixture struct {
	grpID              int64
	bankBare, slotBare *bins.Bin
	bankDefault        *bins.Bin
}

func bareCarrierFixture(t *testing.T, db *store.DB) bareFixture {
	t.Helper()
	sdb := db.DB
	mk := func(name string, parent *int64) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, nodes.Create(sdb, n), "create "+name)
		return n
	}
	grpID, err := nodes.CreateGroup(sdb, "HLPIN-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	bank, bank2 := mk("HLPIN-BANK-1", nil), mk("HLPIN-BANK-2", nil)
	slot := mk("HLPIN-GRP-SLOT-1", &grpID)

	// bare is generated from bare_of (v136): the marker names its carrier.
	var bareID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`WITH c AS (INSERT INTO bin_types (code, description) VALUES ('HLPIN', 'half-loader cart') RETURNING id)
		 INSERT INTO bin_types (code, description, bare_of) SELECT 'HLPIN-BARE', 'half-loader stage-1 carrier', id FROM c RETURNING id`,
	).Scan(&bareID), "bin type")
	ofType := func(b *bins.Bin) *bins.Bin {
		_, err := sdb.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, bareID, b.ID)
		testutil.MustNoErr(t, err, "retype "+b.Label)
		got, err := db.GetBin(b.ID)
		testutil.MustNoErr(t, err, "reread "+b.Label)
		return got
	}
	return bareFixture{
		grpID:       grpID,
		bankBare:    ofType(testdb.CreateBinAtNode(t, db, "", bank.ID, "BIN-HLPIN-BANK-BARE")),
		slotBare:    ofType(testdb.CreateBinAtNode(t, db, "", slot.ID, "BIN-HLPIN-SLOT-BARE")),
		bankDefault: testdb.CreateBinAtNode(t, db, "", bank2.ID, "BIN-HLPIN-BANK-DEFAULT"),
	}
}

func foundBin(t *testing.T, b *bins.Bin, err error) int64 {
	t.Helper()
	if errors.Is(err, sql.ErrNoRows) || b == nil {
		return 0
	}
	testutil.MustNoErr(t, err, "empty scan")
	return b.ID
}

// TestEmptyScan_NeverOffersABareCarrier: an empty carrier of a bare type is
// found by no plant-wide or group-scoped empty finder and counted by no keeper
// count, on a plain node as much as on a loader's window; the DEFAULT control
// on a plain node is still offered. (Before the bare flag the same carrier was
// found by all four and counted 1.)
func TestEmptyScan_NeverOffersABareCarrier(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	f := bareCarrierFixture(t, db)

	// Plant-wide, any type: drain it and see which carriers it offers.
	offered := drainEmpties(t, db, func() (*bins.Bin, error) {
		return db.FindEmptyCompatibleBin("", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
	})
	if offered[f.bankBare.ID] || offered[f.slotBare.ID] {
		t.Errorf("FindEmptyCompatible offered %v, which includes a bare carrier (bank %d, slot %d)",
			offered, f.bankBare.ID, f.slotBare.ID)
	}
	if !offered[f.bankDefault.ID] {
		t.Errorf("FindEmptyCompatible offered %v, want the DEFAULT control %d: the exclusion is too broad",
			offered, f.bankDefault.ID)
	}
	if _, err := db.Exec(`UPDATE bins SET locked=false`); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	b, err := db.FindEmptyBinOfType("HLPIN-BARE", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
	if got := foundBin(t, b, err); got != 0 {
		t.Errorf("FindEmptyOfType(HLPIN-BARE) = bin %d, want none", got)
	}
	b, err = db.FindEmptyCompatibleBinInGroup("", f.grpID, 0, reservations.DigAsker{})
	if got := foundBin(t, b, err); got != 0 {
		t.Errorf("FindEmptyCompatibleInGroup = bin %d, want none (its only carrier is bare)", got)
	}
	b, err = db.FindEmptyBinOfTypeInGroup("HLPIN-BARE", f.grpID, 0, reservations.DigAsker{})
	if got := foundBin(t, b, err); got != 0 {
		t.Errorf("FindEmptyOfTypeInGroup(HLPIN-BARE) = bin %d, want none", got)
	}
	n, err := db.CountEmptyBinsOfTypeInGroup("HLPIN-BARE", f.grpID)
	testutil.MustNoErr(t, err, "count")
	if n != 0 {
		t.Errorf("CountEmptyOfTypeInGroup(HLPIN-BARE) = %d, want 0: the count must agree with the finder", n)
	}
}
