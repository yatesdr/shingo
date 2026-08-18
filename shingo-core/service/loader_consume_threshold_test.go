//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
)

// TestLoaderService_RejectsConsumeThreshold pins the refusal at both write doors.
//
// A consume loader set to threshold replenishment is storable and completely
// inert: Core derives its thresholds and signals against them, the Edge drops
// every signal (its threshold path resolves produce loaders only), and the drain
// that is a consume loader's actual job is skipped for exactly this
// replenishment value. Nothing runs and nothing complains.
//
// Update is checked separately from Create because it takes no role parameter —
// it reads the stored one — so a Create-only guard would leave the door open to
// editing a working unloader into the inert shape.
func TestLoaderService_RejectsConsumeThreshold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	// Create: refused, and refused by name so a caller can tell a bad config from
	// a broken server.
	_, err := svc.Create("CT-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow,
		loaders.ReplenishmentThreshold, "", "", false)
	if err == nil {
		t.Fatal("Create accepted a consume loader in threshold mode; want refusal")
	}
	if !errors.Is(err, ErrConsumeThreshold) {
		t.Errorf("Create error = %v, want ErrConsumeThreshold", err)
	}

	// The produce equivalent is the normal case and must still work — the guard
	// has to be about the PAIR, not about threshold mode.
	produceID, err := svc.Create("CT-LOADER", loaders.RoleProduce, loaders.LayoutSharedWindow,
		loaders.ReplenishmentThreshold, "", "", false)
	testutil.MustNoErr(t, err, "create produce threshold loader")

	// And a consume loader in its real mode is fine.
	consumeID, err := svc.Create("CT-DRAIN", loaders.RoleConsume, loaders.LayoutSharedWindow,
		loaders.ReplenishmentOperator, "", "", false)
	testutil.MustNoErr(t, err, "create consume drain loader")

	// Update: the same pair refused when editing an existing unloader. Role is
	// not a parameter here, which is exactly why this needs its own check.
	err = svc.Update(consumeID, "CT-DRAIN", loaders.LayoutSharedWindow,
		loaders.ReplenishmentThreshold, "", "", false)
	if err == nil {
		t.Fatal("Update edited an unloader into threshold mode; want refusal")
	}
	if !errors.Is(err, ErrConsumeThreshold) {
		t.Errorf("Update error = %v, want ErrConsumeThreshold", err)
	}

	// The refused edit must not have half-landed.
	back, err := db.GetLoader(consumeID)
	testutil.MustNoErr(t, err, "re-read unloader")
	if back == nil || back.Replenishment != loaders.ReplenishmentOperator {
		t.Errorf("unloader replenishment = %v, want operator — the refused update wrote anyway", back)
	}

	// Editing the produce loader's other fields still works with threshold mode
	// intact; the guard must not have caught the whole threshold vocabulary.
	testutil.MustNoErr(t, svc.Update(produceID, "CT-LOADER-2", loaders.LayoutSharedWindow,
		loaders.ReplenishmentThreshold, "", "", false), "update produce threshold loader")
}

// TestLoaderService_ConsumeDefaultsToDrain pins that the role-aware default is
// still what fills a blank replenishment, and that the new guard sits AFTER it.
// Ordering them the other way would refuse every consume loader created from the
// screen, which sends the field blank.
func TestLoaderService_ConsumeDefaultsToDrain(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	id, err := svc.Create("CTD-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create consume loader with blank replenishment")

	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "read back")
	if got == nil || got.Replenishment != loaders.ReplenishmentOperator {
		t.Errorf("blank replenishment on a consume loader = %v, want operator (drain)", got)
	}
}
