//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/reservations"
)

// source_finder_lookpast_docker_test.go — the finder looks PAST a candidate the
// destination cannot use, instead of stopping dead on it.
//
// ── THE DEADLOCK ──────────────────────────────────────────────────────────
//
// A drain window can only use a FULL carrier. That rule used to be a VETO
// applied after the group scan had already chosen its single favourite — and a
// veto is not a filter. The scan re-picked the same favourite every pass, the
// veto refused it every pass, and nothing ever looked at the next candidate.
//
// THE FIXTURE BELOW IS SYNTHETIC AND STAYS THAT WAY — it builds its own nodes
// and bins, so nothing here depends on a plant spec. The live drain window this
// rule protects is FGN_001 / UNLOADER-A (`role: consume`, `payload: ASSY`,
// plants/demo.yaml:1015-1025), the plant's only unloader.
//
// MEASURED ON A DELETED FIXTURE — run of 2026-08-30, WALL. The drain was
// FGN_003 on PANEL-B, an unloader that has since been removed for eating the
// line's own feedstock. (cmd/seeddev/testdata/seed-fixture.yaml has a DIFFERENT
// FGN_003 which is correct there — it drains ASSY-C, a finished good nobody
// consumes.) The FIFO-oldest PANEL-B carrier was bin 19, holding 3 of 30, at
// Lane_01's mouth:
//
//	finder: FGN_003 is a drain window and bin 19 is a partial (3 of 30)
//	        — waiting for a full                                    x1,715
//
// Three FULL carriers sat at the mouths of Lane_04, Lane_07 and Lane_15 with
// nothing in front of them and zero holds on those lanes. They were never once
// considered. With no carrier drained, no empty was ever created, the
// STANDARD-SM pool went to zero, and PANEL-B production stopped — then ASSY
// behind it.
//
// ── AND FIFO IS UNTOUCHED, WHICH IS THE OTHER HALF ────────────────────────
//
// Rotation matters when a line is being fed: demand should get the oldest
// material. So the filter changes nothing for a caller that has no constraint —
// asserted here as its own case, because a fix that quietly de-prioritised age
// for everybody would be a worse bug than the one it replaced.

// agedBinAt places a carrier of a given fullness at a slot, with an explicit age
// so the FIFO comparison is decided by the fixture rather than by insert order.
func agedBinAt(t *testing.T, db *store.DB, payload string, nodeID int64, label string,
	uop int, age time.Duration) *bins.Bin {
	t.Helper()
	b := testdb.CreateBinAtNode(t, db, payload, nodeID, label)
	if _, err := db.Exec(`UPDATE bins SET uop_remaining=$1, loaded_at=now() - $2::interval WHERE id=$3`,
		uop, age.String(), b.ID); err != nil {
		t.Fatalf("age bin %s: %v", label, err)
	}
	reloaded, err := db.GetBin(b.ID)
	if err != nil {
		t.Fatalf("reload bin %s: %v", label, err)
	}
	return reloaded
}

func TestGroupRetrieve_LooksPastACandidateTheCallerCannotUse(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	wall, park, w, p, bp := clearLaneFixture(t, db, "LOOKPAST")
	if _, err := db.Exec(`UPDATE payloads SET uop_capacity=30 WHERE code=$1`, bp.Code); err != nil {
		t.Fatalf("give the payload a capacity: %v", err)
	}
	group, err := db.GetNode(*wall.ParentID)
	testutil.MustNoErr(t, err, "reload the group")

	// THE OLDEST CARRIER IS A PARTIAL, at the wall lane's mouth. This is bin 19.
	partial := agedBinAt(t, db, bp.Code, w[0].ID, "BIN-LOOKPAST-PARTIAL", 3, 90*time.Minute)
	// A FULL one, younger, at the sibling lane's mouth — nothing in front of it.
	full := agedBinAt(t, db, bp.Code, p[0].ID, "BIN-LOOKPAST-FULL", 30, 10*time.Minute)
	_ = park

	r := &binresolver.DefaultResolver{DB: db}

	// (1) NO FILTER — FIFO is exactly as it was, and the oldest wins even though
	// it is nearly empty. This is what demand must keep getting.
	unfiltered, err := r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, nil)
	testutil.MustNoErr(t, err, "unfiltered resolve")
	if unfiltered == nil || unfiltered.Bin == nil || unfiltered.Bin.ID != partial.ID {
		t.Fatalf("unfiltered resolve = %v, want the OLDEST bin %d — a caller with no constraint "+
			"must still get FIFO, and rotation is why", unfiltered, partial.ID)
	}

	// (2) WITH THE DRAIN WINDOW'S FILTER — the partial is not a candidate, so the
	// scan keeps looking and finds the full one in the sibling lane.
	filtered, err := r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, func(b *bins.Bin) bool { return b.UOPRemaining >= 30 })
	testutil.MustNoErr(t, err, "filtered resolve")
	if filtered == nil || filtered.Bin == nil {
		t.Fatalf("filtered resolve found nothing, but a full carrier is standing at %s with nothing "+
			"in front of it. Stopping on the partial is the 1,715-refusal deadlock", p[0].Name)
	}
	if filtered.Bin.ID != full.ID {
		t.Errorf("filtered resolve = bin %d (uop %d), want the FULL bin %d — a bin the caller cannot "+
			"use must lose the comparison, not win it and be thrown away afterwards",
			filtered.Bin.ID, filtered.Bin.UOPRemaining, full.ID)
	}
}
