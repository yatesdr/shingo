//go:build docker

package main

import (
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/service"
)

// seed_bare_docker_test.go — the fixture keys the half-loader sim needs:
// bare_bin_types (→ bin_types.bare) and loader_settings (→ a loader's
// accept_partials and bare_bin_type_id), seeded through the admin doors' own
// refusals so a fixture cannot set up a state the running plant would refuse.

func loadSeedFixture(t *testing.T) *plantspec.Plant {
	t.Helper()
	plant, err := plantspec.Load("testdata/seed-fixture.yaml")
	if err != nil {
		t.Fatalf("load seed fixture: %v", err)
	}
	return plant
}

func TestSeedCore_BareTypeAndLoaderSettings(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plant := loadSeedFixture(t)
	plant.BinTypes = append(plant.BinTypes, "HALF-CART")
	plant.BareBinTypes = []string{"HALF-CART"}
	plant.LoaderSettings = map[string]plantspec.LoaderSettings{
		"FGN_001": {AcceptPartials: true, BareBinType: "HALF-CART", AutoPush: true},
	}
	if err := plant.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}

	bt, err := db.GetBinTypeByCode("HALF-CART")
	if err != nil || !bt.Bare {
		t.Fatalf("HALF-CART = %+v, %v; want a bare type", bt, err)
	}
	std, err := db.GetBinTypeByCode("STANDARD")
	if err != nil || std.Bare {
		t.Fatalf("STANDARD = %+v, %v; want not bare", std, err)
	}
	l, err := db.GetLoaderByName("FGN_001", "consume")
	if err != nil || l == nil {
		t.Fatalf("FGN_001 loader: %v", err)
	}
	if !l.AcceptPartials || l.BareBinTypeID == nil || *l.BareBinTypeID != bt.ID || l.BareBinTypeCode != "HALF-CART" {
		t.Errorf("FGN_001 = accept %v bare %v/%q, want true and HALF-CART", l.AcceptPartials, l.BareBinTypeID, l.BareBinTypeCode)
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
	if other.AcceptPartials || other.BareBinTypeID != nil || other.AutoPush {
		t.Errorf("FGN_002 has no settings but got accept %v bare %v auto_push %v",
			other.AcceptPartials, other.BareBinTypeID, other.AutoPush)
	}
	// Idempotent re-seed.
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}

// TestSeedCore_RefusesAnIllegalBareFixture: each refusal the admin doors make
// is made at seed time too.
func TestSeedCore_RefusesAnIllegalBareFixture(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		edit    func(p *plantspec.Plant)
		wantErr error // checked with errors.Is when the refusal is the service's
	}{
		{"a bare bin type on a produce loader", func(p *plantspec.Plant) {
			p.LoaderSettings = map[string]plantspec.LoaderSettings{"PLK_X1": {BareBinType: "HALF-CART"}}
		}, service.ErrBareTypeProduce},
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
			plant.BinTypes = append(plant.BinTypes, "HALF-CART")
			plant.BareBinTypes = []string{"HALF-CART"}
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

// TestPlantSpec_RefusesABareTypeAsAPayloadCarrier: validation catches the
// payload-rule case before any write; the seeder asks the service's check
// again at the write.
func TestPlantSpec_RefusesABareTypeAsAPayloadCarrier(t *testing.T) {
	t.Parallel()
	plant := loadSeedFixture(t)
	plant.BareBinTypes = []string{plant.Payloads[0].BinType}
	if err := plant.Validate(); err == nil {
		t.Fatal("Validate accepted a payload whose bin_type is bare")
	}

	db := testdb.Open(t)
	err := seedCore(db, plant, map[string]int64{})
	if !errors.Is(err, service.ErrBareTypeInPayloadRule) {
		t.Fatalf("seedCore err = %v, want ErrBareTypeInPayloadRule", err)
	}
}
