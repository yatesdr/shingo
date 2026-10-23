//go:build docker

package binresolver

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestGroupAdmitsStore_ACartAtZeroOnARealGroup: a group of four keeping two
// tote carts (8 to a layer), holding two stacks and one tops & bottoms. Its
// stack level is met, so a stack is refused; a cart at 0 is refused while the
// type has no tops & bottoms row, and taken once the row wants two. The row is
// one more statement, read for the cart at 0 alone.
func TestGroupAdmitsStore_ACartAtZeroOnARealGroup(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	grpID, err := nodes.CreateGroup(db.DB, "TBL-GRP")
	testutil.MustNoErr(t, err, "group")
	testdb.MarkGroup(t, db, grpID)
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('TBL-TOTE', 8) RETURNING id`).Scan(&btID), "type")
	cart := func(label string, node int64, totes int) *bins.Bin {
		t.Helper()
		b := &bins.Bin{BinTypeID: btID, Label: label, Status: "available", NodeID: &node}
		testutil.MustNoErr(t, db.CreateBin(b), label)
		_, err := db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, totes, b.ID)
		testutil.MustNoErr(t, err, "totes on "+label)
		got, err := db.GetBin(b.ID)
		testutil.MustNoErr(t, err, "read back "+label)
		return got
	}
	for i := 1; i <= 4; i++ {
		slot := &nodes.Node{Name: fmt.Sprintf("TBL-SLOT-%d", i), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, nodes.Create(db.DB, slot), "slot")
		switch i {
		case 1, 2:
			cart(fmt.Sprintf("TBL-STACK-%d", i), slot.ID, 16)
		case 3:
			cart("TBL-TB", slot.ID, 0)
		}
	}
	away := &nodes.Node{Name: "TBL-AWAY", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, away), "away")
	atZero, stack := cart("TBL-COMING-TB", away.ID, 0), cart("TBL-COMING-STACK", away.ID, 16)
	if atZero.CarrierState() != protocol.CarrierTopsAndBottoms {
		t.Fatalf("fixture: the cart at 0 reads %s", atZero.CarrierState())
	}
	level := store.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, Want: 2}
	testutil.MustNoErr(t, db.SetMaintainLevel(level), "level")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	r := &GroupResolver{DB: cdb}
	group, err := cdb.GetNode(grpID)
	testutil.MustNoErr(t, err, "group node")
	admits := func(b *bins.Bin) (bool, int64) {
		counter.Reset()
		err := r.groupAdmitsStore(group, "", KnownCarrier(b))
		if err != nil && !errors.Is(err, ErrAtDeclaredLevel) {
			t.Fatalf("%s: %v", b.Label, err)
		}
		return err == nil, counter.Count()
	}

	if ok, _ := admits(stack); ok {
		t.Error("a stack was admitted to a group whose stack level is met")
	}
	if ok, _ := admits(atZero); ok {
		t.Error("a cart at 0 was admitted to a group that keeps its type and has no tops & bottoms row")
	}
	testutil.MustNoErr(t, db.SetMaintainLevelAndLayerWants(level, []store.MaintainLayerWant{{Layers: 0, Want: 2}}), "the row")
	okStack, stackStmts := admits(stack)
	okZero, zeroStmts := admits(atZero)
	if okStack {
		t.Error("the row let a stack past the stack level")
	}
	if !okZero {
		t.Error("a cart at 0 was refused with the row short (one standing, two wanted)")
	}
	if zeroStmts != stackStmts+1 {
		t.Errorf("statements: a cart at 0 %d, a stack %d; want exactly one more, the row", zeroStmts, stackStmts)
	}
}
