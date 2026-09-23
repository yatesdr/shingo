//go:build docker

package main

import (
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// TestPinExhaustedCarrierPool_EmptiesOnlyOnUnloaderWindowsAreReported: an
// ORDINARY type whose every carrier is an empty standing on a live unloader's
// window is "NONE sourceable", because a live loader's own position is outside
// the empty population (D5). A bare type is permanently in that state, which is
// why the check skips bare types (TestExhaustedCarrierPool_ABareTypeIsNotAPool)
// rather than report them.
func TestPinExhaustedCarrierPool_EmptiesOnlyOnUnloaderWindowsAreReported(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, _ := poolFixture(t, db, "HALFWIN")

	id, err := db.CreateLoader(loaders.Loader{
		Name: "SOAK-HALF-UNLOADER", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator,
	})
	if err != nil {
		t.Fatalf("create loader: %v", err)
	}
	for i := 1; i <= 2; i++ {
		win := &nodes.Node{Name: fmt.Sprintf("SOAK-HALF-W%d", i), Enabled: true}
		if err := db.CreateNode(win); err != nil {
			t.Fatalf("create window: %v", err)
		}
		if err := db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: win.ID}); err != nil {
			t.Fatalf("window: %v", err)
		}
		seedCarrier(t, db, typeCode, win.ID, fmt.Sprintf("%s-%d", typeCode, i), "")
	}

	got := checkExhaustedCarrierPool(db)
	if !hasLine(got, "carrier type "+typeCode+" has 2 empty carriers and NONE sourceable") {
		t.Errorf("want the spoken-for line for %s, got %v", typeCode, got)
	}
}

// TestExhaustedCarrierPool_ABareTypeIsNotAPool: a bare type's carriers are never
// sourceable as empties, on a plain node or a window, so the check never
// reports the type — it would otherwise fire on every run.
func TestExhaustedCarrierPool_ABareTypeIsNotAPool(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, nodeID := poolFixture(t, db, "BARE")
	if _, err := db.Exec(`UPDATE bin_types SET bare = true WHERE code = $1`, typeCode); err != nil {
		t.Fatalf("flag bare: %v", err)
	}
	for i := 1; i <= 2; i++ {
		seedCarrier(t, db, typeCode, nodeID, fmt.Sprintf("%s-%d", typeCode, i), "")
	}

	if got := checkExhaustedCarrierPool(db); hasLine(got, typeCode) {
		t.Errorf("a bare type was reported as a pool: %v", got)
	}
}
