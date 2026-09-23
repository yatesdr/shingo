//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// source_finder_loader_pool_docker_test.go — the finder tiers that read a
// loader position's carriers IN GO (tier 2's pool, tier 4's resident list)
// against a real Postgres.
//
// Neither goes through bins.EmptyCarrierWhere: tier 2 lists the pool with
// ListBinsByNodes and ranks it in binsource, tier 4 lists the named node with
// ListBinsByNode. So a rule added to the plant-wide empty predicate cannot
// change what these return, and these pins are what show it.

// TIER 2, Fill: an empty standing on a dedicated loader's own home position is
// sourced from the loader's pool for that loader's retrieve_empty.
func TestFinderPin_LoaderPoolFillTakesAnEmptyOnItsOwnHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	pos, _ := dedicatedLoaderFixture(t, db, "produce")
	empty := makeEmptyBin(t, db, pos.ID, "pool-home-empty")

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		OrderType: OrderTypeRetrieveEmpty, PayloadCode: "PART-X",
		SourceNode: pos.Name, DeliveryNode: lineNode.Name,
		SourceIntent: SourceIntentForType(OrderTypeRetrieveEmpty),
	}, IntentEmpty)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != empty.ID {
		t.Fatalf("outcome=%v cause=%q bin=%v, want the empty %d on the loader's home %s",
			res.Outcome, res.QueueCause, res.Bin, empty.ID, pos.Name)
	}
}

// TIER 4: a payload-less move naming a shared-window loader's window takes the
// empty standing on it.
func TestFinderPin_MoveTakesTheEmptyOnALoaderWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)

	window := &nodes.Node{Name: "POOLPIN-WIN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "create window")
	loaderID, err := db.CreateLoader(store.Loader{
		Name: "POOLPIN-UNLOADER", Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow, Replenishment: "operator",
	})
	testutil.MustNoErr(t, err, "create loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: window.ID}), "window home")
	empty := makeEmptyBin(t, db, window.ID, "window-resident-empty")

	f := NewSourceFinder(db, nil, nil)
	res := f.FindSource(&orders.Order{
		OrderType: OrderTypeMove, SourceNode: window.Name, DeliveryNode: lineNode.Name,
		SourceIntent: SourceIntentForType(OrderTypeMove),
	}, IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != empty.ID {
		t.Fatalf("outcome=%v cause=%q bin=%v, want the empty %d on window %s",
			res.Outcome, res.QueueCause, res.Bin, empty.ID, window.Name)
	}
}
