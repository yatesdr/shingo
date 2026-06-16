//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// TestLoaderService_SetHome_RejectsSyntheticWindow guards the structural fix for
// the Springfield "lane 14" loader-window incident: a loader window/position
// must be a real physical slot, never a synthetic container (a node group or an
// empty lane). Assigning a container produced a loader that dispatched into a
// location with no slots ("synthetic node has no children").
func TestLoaderService_SetHome_RejectsSyntheticWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	loaderID, err := db.CreateLoader(loaders.Loader{
		Name: "TEST-LOADER", Role: "produce", Layout: "dedicated_positions",
	})
	testutil.MustNoErr(t, err, "create loader")

	// A synthetic container node (lane/group) must be rejected as a window.
	lane := &nodes.Node{Name: "FAKE-LANE", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create synthetic lane")
	if err := svc.SetHome(loaderID, lane.ID, "", 0); err == nil {
		t.Fatal("SetHome accepted a synthetic container node as a loader window; want rejection")
	}

	// A real physical slot node must be accepted.
	slot := &nodes.Node{Name: "REAL-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create physical slot")
	testutil.MustNoErr(t, svc.SetHome(loaderID, slot.ID, "", 0), "SetHome physical slot")
}
