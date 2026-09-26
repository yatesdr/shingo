//go:build docker

package service

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// bin_move_staging_docker_test.go — a bin moved by hand ends up in the state a
// robot delivery to the same place would leave it in.
//
// Springfield 2026-09-25: two full bins released at a line were moved by hand
// onto a dedicated loader's home positions and stayed `staged`, because the
// hand Move asked its own storage check, which never learned that a loader home
// is storage. A staged bin is never sourced, so both sat unusable until an
// admin released them.

// TestMove_StagedBinOntoDedicatedLoaderHomeArrivesAvailable: a loader home is
// parentless, like a line node, so only the loader-home clause tells them
// apart. A robot delivery there lands the bin available; so must a hand Move.
func TestMove_StagedBinOntoDedicatedLoaderHomeArrivesAvailable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	line := &nodes.Node{Name: "MV-LH-LINE", Enabled: true}
	home := &nodes.Node{Name: "MV-LH-HOME", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create line node")
	testutil.MustNoErr(t, db.CreateNode(home), "create home node")
	loaderID, err := db.CreateLoader(loaders.Loader{Name: "MV-LH-LOADER", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "create dedicated loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: loaderID, PositionNodeID: home.ID,
		PayloadCode: "MV-LH-PART", Kind: loaders.HomeKindHome}), "add the home position")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-LH", NodeID: &line.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create staged bin at the line")

	if _, err := newBinSvc(db).MoveByHand(bin, home.ID); err != nil {
		t.Fatalf("MoveByHand: %v", err)
	}
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.Status != "available" || got.StagedAt != nil {
		t.Errorf("status = %q staged_at = %v, want available with no staged time: a loader home is storage, "+
			"and a staged bin there is never sourced", got.Status, got.StagedAt)
	}
}

// TestMove_StagedBinOntoACarrierLeavesStagingAlone: a robot's deck (or
// _TRANSIT) is "nobody knows where it is", not a place, so a move there says
// nothing about staging. The status is decided when the bin is set down.
func TestMove_StagedBinOntoACarrierLeavesStagingAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	line := &nodes.Node{Name: "MV-DECK-LINE", Enabled: true}
	deck := &nodes.Node{Name: "_ROBOT:MV-AMR", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create line node")
	testutil.MustNoErr(t, db.CreateNode(deck), "create carrier node")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-DECK", NodeID: &line.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create staged bin at the line")
	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin before")

	if _, err := newBinSvc(db).Move(bin, deck.ID); err != nil {
		t.Fatalf("Move: %v", err)
	}
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.Status != "staged" || !sameTime(got.StagedAt, before.StagedAt) || !sameTime(got.StagedExpiresAt, before.StagedExpiresAt) {
		t.Errorf("status = %q staged_at = %v expires = %v, want staging untouched on a carrier (was %v / %v)",
			got.Status, got.StagedAt, got.StagedExpiresAt, before.StagedAt, before.StagedExpiresAt)
	}
}

// sameTime compares two optional instants to the microsecond, the resolution
// Postgres stores.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}
