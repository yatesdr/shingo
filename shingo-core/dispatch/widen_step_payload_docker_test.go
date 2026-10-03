//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// TestWiden_AsksASupplyPickupForItsOwnPart: a two-robot changeover's supply leg
// carries the OUTGOING part, because its partner's pickup has to find the
// outgoing bin on the line, while its fetch names the INCOMING part on the step.
// With the inbound source a concrete node, the widen asked that node for the
// order's part, found nothing of it there, and parked the pair, although the
// incoming part was standing exactly where the step said.
//
// The widen asks with the step's part when the step names one, the rule
// resolvedStepPayload already spells for the allocator.
func TestWiden_AsksASupplyPickupForItsOwnPart(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	const prefix = "WSP"

	newPart := prefix + "-NEWPART"
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: newPart, UOPCapacity: 100}), "incoming payload")
	src := prNode(t, db, prefix+"-SRC")
	inStage := prNode(t, db, prefix+"-IN-STAGE")
	outDest := prNode(t, db, prefix+"-OUT")
	testdb.CreateBinAtNode(t, db, newPart, src.ID, prefix+"-NEW")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, prefix+"-OLD")

	fetch := prPick(src.Name)
	fetch.PayloadCode = newPart
	supplyUUID, evacUUID := prefix+"-supply", prefix+"-evac"
	prSubmitLeg(d, supplyUUID, evacUUID, sd.Payload.Code, sd.LineNode.Name,
		fetch, prDropExcl(inStage.Name), prWait(""), prPick(inStage.Name), prDrop(sd.LineNode.Name))
	prSubmitLeg(d, evacUUID, supplyUUID, sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(outDest.Name))

	prScanPass(t, d, db, evacUUID, supplyUUID)

	s, e := prReloadUUID(t, db, supplyUUID), prReloadUUID(t, db, evacUUID)
	if s.VendorOrderID == "" || e.VendorOrderID == "" {
		t.Fatalf("the changeover pair did not go with the incoming part standing at its source: "+
			"supply %q/%q (%q), evac %q/%q (%q)",
			s.Status, s.QueueCause, s.QueueReason, e.Status, e.QueueCause, e.QueueReason)
	}
}
