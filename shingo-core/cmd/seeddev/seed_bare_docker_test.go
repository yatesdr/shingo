//go:build docker

package main

import (
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/service"
)

// seed_bare_docker_test.go — the fixture key the half-loader sim needs:
// loader_settings (→ a loader's accept_partials and auto_push), seeded through
// the admin doors' own refusals so a fixture cannot set up a state the running
// plant would refuse. A bare marker is not a fixture key: it derives from each
// cart's own type at a stage-1 CLEAR.

func loadSeedFixture(t *testing.T) *plantspec.Plant {
	t.Helper()
	plant, err := plantspec.Load("testdata/seed-fixture.yaml")
	if err != nil {
		t.Fatalf("load seed fixture: %v", err)
	}
	return plant
}

func TestSeedCore_LoaderSettings(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plant := loadSeedFixture(t)
	plant.LoaderSettings = map[string]plantspec.LoaderSettings{
		"FGN_001": {AcceptPartials: true, AutoPush: true},
	}
	if err := plant.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}

	std, err := db.GetBinTypeByCode("STANDARD")
	if err != nil || std.Bare {
		t.Fatalf("STANDARD = %+v, %v; want not bare", std, err)
	}
	l, err := db.GetLoaderByName("FGN_001", "consume")
	if err != nil || l == nil {
		t.Fatalf("FGN_001 loader: %v", err)
	}
	if !l.AcceptPartials {
		t.Errorf("FGN_001 accept_partials = false, want the loader_settings value")
	}
	if !l.AutoPush {
		t.Error("FGN_001 auto_push = false, want the loader_settings value")
	}
	other, err := db.GetLoaderByName("FGN_002", "consume")
	if err != nil || other == nil {
		t.Fatalf("FGN_002 loader: %v", err)
	}
	// FGN_002's CLAIM says auto_push: true; only loader_settings reaches the
	// Core loader (TestPinSeedCore_ClaimAutoPushDoesNotReachTheCoreLoader).
	if other.AcceptPartials || other.AutoPush {
		t.Errorf("FGN_002 has no settings but got accept %v auto_push %v",
			other.AcceptPartials, other.AutoPush)
	}
	// Idempotent re-seed.
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}

// TestSeedCore_RefusesAnIllegalLoaderSettingsFixture: each refusal the admin
// doors make is made at seed time too.
func TestSeedCore_RefusesAnIllegalLoaderSettingsFixture(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		edit    func(p *plantspec.Plant)
		wantErr error // checked with errors.Is when the refusal is the service's
	}{
		{"accept_partials on a produce loader", func(p *plantspec.Plant) {
			p.LoaderSettings = map[string]plantspec.LoaderSettings{"PLK_X1": {AcceptPartials: true}}
		}, service.ErrAcceptPartialsProduce},
		{"auto_push on a produce loader", func(p *plantspec.Plant) {
			p.LoaderSettings = map[string]plantspec.LoaderSettings{"PLK_X1": {AutoPush: true}}
		}, service.ErrAutoPushProduce},
		{"settings naming no loader", func(p *plantspec.Plant) {
			p.LoaderSettings = map[string]plantspec.LoaderSettings{"SYN_SM_FG": {AcceptPartials: true}}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			plant := loadSeedFixture(t)
			tc.edit(plant)
			err := seedCore(db, plant, map[string]int64{})
			if err == nil {
				t.Fatal("seedCore accepted it")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
