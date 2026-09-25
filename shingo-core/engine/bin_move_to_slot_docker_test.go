//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestBinMove_ToAConcreteStorageSlotDispatches — red at the base, green with
// the one-step acquire.
//
// At the base, CreateBinMove soft-reserved the BIN and took the lanes, then
// called ConfirmForDispatch — and never soft-reserved the destination SLOT.
// ConfirmForDispatch treated a STOR destination as a storage dropoff and ran
// ConfirmSlotClaim, whose seatbelt requires a pending slot reservation held by
// the order. So the slot leg refused every time, and a bin move to a concrete
// storage slot NEVER dispatched. Every existing bin-move test moved to a line
// node, which has no slot leg, so nothing pinned this. The door now reserves the
// slot (ReserveStorageDropoff) and takes it in the hand with the bin.
//
// The fixture is the smallest one that reaches it: an empty STOR node outside
// any lane, so no lane admission or occupancy can be the refusal.
//
// Flat bug: a door whose destination kind could never succeed.
func TestBinMove_ToAConcreteStorageSlotDispatches(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BMS-BIN")
	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection: BinSelectionByLabel, BinLabel: bin.Label,
		SourceNodeID: sd.LineNode.ID, DestNodeID: sd.StorageNode.ID, StationID: "test",
	})

	if err != nil {
		t.Fatalf("a bin move to an empty storage slot outside any lane was refused: %v", err)
	}
	if res == nil || res.VendorOrderID == "" {
		t.Fatalf("the move was accepted but never reached the fleet (result %+v)", res)
	}
	var slotOwner *int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT claimed_by FROM nodes WHERE id = $1`, sd.StorageNode.ID).Scan(&slotOwner),
		"read the destination's claim")
	if slotOwner == nil || *slotOwner != res.OrderID {
		t.Errorf("the destination slot is claimed by %v, want the dispatched move (%d)", slotOwner, res.OrderID)
	}
}
