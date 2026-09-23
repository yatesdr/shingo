//go:build docker

package main

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/store/loaders"
)

// seed_push3_pins_docker_test.go — two seeder facts pinned at base for push 3.

// TestPinSeedCore_ClaimAutoPushDoesNotReachTheCoreLoader: the fixture's
// FGN_001 claim says auto_push: true, and that reaches only the Edge's stored
// claim (seed_edge). The Core loader, which is what the Edge now resolves an
// unloader from, carries no such setting, so LoaderInfo.AutoPush is false.
func TestPinSeedCore_ClaimAutoPushDoesNotReachTheCoreLoader(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plant := loadSeedFixture(t)
	var claimSays bool
	for _, c := range plant.Claims {
		if c.CoreNode == "FGN_001" {
			claimSays = c.AutoPush
		}
	}
	if !claimSays {
		t.Fatal("fixture premise: FGN_001's claim sets auto_push")
	}
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}
	l, err := db.GetLoaderByName("FGN_001", "consume")
	if err != nil || l == nil {
		t.Fatalf("FGN_001 loader: %v", err)
	}
	infos, err := db.BuildLoaderInfos()
	if err != nil {
		t.Fatalf("BuildLoaderInfos: %v", err)
	}
	for _, li := range infos {
		if li.LoaderKey == loaders.Key(l.ID) && li.AutoPush {
			t.Error("LoaderInfo.AutoPush = true for FGN_001; at base nothing carries it to the Core loader")
		}
	}
}

// TestPinSeedCore_AZeroPayloadUnloaderSeeds: the seeder itself already
// expresses stage 2 — a zero-payload window claim becomes a consume
// shared_window loader with one window and no payloads. Only Validate refuses
// the spec (TestPinValidate_AZeroPayloadUnloaderIsRefusedByParity).
func TestPinSeedCore_AZeroPayloadUnloaderSeeds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plant := loadSeedFixture(t)
	plant.Stations = append(plant.Stations, plantspec.Station{Name: "S2_W1", Kind: "unloader"})
	plant.Claims = append(plant.Claims, plantspec.Claim{CoreNode: "S2_W1", Style: "UNLOADER-A-RUN",
		Role: "consume", SwapMode: "manual_swap", WindowOf: "S2_UNLOADER", OutboundDestination: "SYN_MT_Return"})
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}
	l, err := db.GetLoaderByName("S2_UNLOADER", "consume")
	if err != nil || l == nil {
		t.Fatalf("S2_UNLOADER loader: %v", err)
	}
	if l.Layout != loaders.LayoutSharedWindow {
		t.Errorf("layout = %s, want shared_window", l.Layout)
	}
	pls, err := db.ListLoaderPayloads(l.ID)
	if err != nil || len(pls) != 0 {
		t.Errorf("payloads = %v (err %v), want none", pls, err)
	}
	homes, err := db.ListLoaderHomes(l.ID)
	if err != nil || len(homes) != 1 {
		t.Errorf("windows = %d (err %v), want 1", len(homes), err)
	}
}
