//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestC2bPin_AnAimBuriedByACommittedRobotIsReMadeOnTheNextPass is the condition
// the orc set on "a plan never blocks a committed robot" (2026-10-09): a
// never-sent plain order's aim does not stop a committed robot from storing in
// front of the aimed bin, so the plain order's next pass must find out. Its
// birth pick no longer stands (PlannedPickStands), and it re-picks or digs. It
// never waits without a cause.
//
// The plant: a marked group, a two-slot lane with the aimed bin at the back,
// and the front slot filled after the plan was written: the committed robot's
// bin. "another bin" adds a reachable bin of the same payload outside the
// lane, so a re-pick has somewhere to go; "only the buried bin" has none, so the
// order must dig.
func TestC2bPin_AnAimBuriedByACommittedRobotIsReMadeOnTheNextPass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prefix  string
		another bool
	}{
		{"another bin", "AIMRA", true},
		{"only the buried bin", "AIMRB", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: tc.prefix, NumSlots: 2, NumShuffles: 2})
			eng := newTestEngine(t, db, testdb.NewTrackingBackend())

			var alt *bins.Bin
			if tc.another {
				sd := testdb.SetupStandardData(t, db)
				stor := &nodes.Node{Name: tc.prefix + "-STOR", Zone: "A", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID}
				testutil.MustNoErr(t, db.CreateNode(stor), "create the other storage node")
				alt = testdb.CreateBinAtNode(t, db, sc.Payload.Code, stor.ID, tc.prefix+"-ALT")
			}

			aimed := sc.TargetBin
			back := sc.Slots[1]
			o := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = fmt.Sprintf("aimr-%s", tc.prefix)
				o.OrderType = dispatch.OrderTypeRetrieve
				o.SourceIntent = dispatch.SourceIntentForType(dispatch.OrderTypeRetrieve)
				o.Status = protocol.StatusQueued
				o.PayloadCode = sc.Payload.Code
				o.DeliveryNode = sc.LineNode.Name
				o.Plan = &domain.PlannedPick{BinID: aimed.ID, Node: back.Name, Payload: sc.Payload.Code,
					Status: string(aimed.Status), Confirmed: true}
			})
			// sc.Blockers[0] at the front slot is the committed robot's bin, stored in
			// front of the aim after the plan was written.

			eng.RunFulfillmentScan()
			eng.Dispatcher().EvaluateLaneReleases(sc.Lane.ID)

			after, err := db.GetOrder(o.ID)
			testutil.MustNoErr(t, err, "reload the order")
			legs, err := db.ListChildOrders(o.ID)
			testutil.MustNoErr(t, err, "list the order's dig legs")
			dug := len(legs) > 0 || excavating(t, db, sc.Lane.ID)
			repicked := after.BinID != nil && *after.BinID != aimed.ID &&
				(after.Status == protocol.StatusDispatched || after.Status == protocol.StatusInTransit)

			switch {
			case repicked && tc.another && *after.BinID != alt.ID:
				t.Fatalf("re-picked bin %d, want the reachable bin %d", *after.BinID, alt.ID)
			case repicked || dug:
				// The pass found the burial and acted on it.
			case after.QueueCause == "":
				t.Fatalf("the order waits with no cause (status %s, bin %v, gate-staged %v) after a committed "+
					"robot buried its aimed bin — the next pass must re-pick or dig",
					after.Status, after.BinID, dispatch.IsGateStaged(after))
			default:
				t.Fatalf("the order waits under %q (status %s) after a committed robot buried its aimed bin; "+
					"the next pass must re-pick or dig", after.QueueCause, after.Status)
			}
		})
	}
}
