//go:build docker

package dispatch

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/reservations"
)

// source_finder_buried_lookpast_docker_test.go — the caller's filter rides the
// BURIED lookups, not only the accessible scan.
//
// ── WHY THIS ARM IS THE LOAD-BEARING ONE ──────────────────────────────────
//
// The accessible half of this rule was pinned first, against a fixture where
// three FULL carriers stood at three sibling lane mouths and a partial won the
// FIFO comparison in front of them. That witness is gone: the shipped demo plant
// now gives ASSY ONE lane (Lane_08, depth 3), so there are no sibling mouths to
// look past to. What is left on a single deep lane is the BURIED shape — a
// partial at the mouth (SMN_022) with a full behind it (SMN_023) — and that is
// this arm, which shipped with no test at all.
//
// The live drain window is FGN_001 / UNLOADER-A: role: consume, payload: ASSY,
// plants/demo.yaml:1015-1025, the plant's only unloader. requiresFullCarrier
// fires on it, so this is reachable production behaviour and not a hypothetical.
//
// ── WHAT A MISSING FILTER COSTS HERE, AND WHY IT IS WORSE THAN A REFUSAL ──
//
// Filtering only the accessible scan is a HALF-FIX with a bill attached: the
// accessible partials vanish, bestBin goes nil, the buried arm fires on an
// UNFILTERED lookup, and a whole excavation is spent exposing a carrier the
// caller refuses on arrival. A dig is the largest action this system takes. Not
// digging is cheap; digging for nothing is not.
//
// The fixture is synthetic throughout — it builds its own group, lanes and bins
// and never loads a plant spec.

func TestGroupRetrieve_BuriedLookupHonoursTheCallersFilter(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	wall, park, w, p, bp := clearLaneFixture(t, db, "BURIEDFILTER")
	if _, err := db.Exec(`UPDATE payloads SET uop_capacity=30 WHERE code=$1`, bp.Code); err != nil {
		t.Fatalf("give the payload a capacity: %v", err)
	}
	group, err := db.GetNode(*wall.ParentID)
	testutil.MustNoErr(t, err, "reload the group")

	// THE TRAP LANE: mouth partial, and the OLDEST buried bin is a partial too.
	// An unfiltered buried lookup picks this one, because it is the oldest thing
	// buried anywhere in the group.
	agedBinAt(t, db, bp.Code, w[0].ID, "BIN-BF-WALL-MOUTH", 4, 20*time.Minute)
	trap := agedBinAt(t, db, bp.Code, w[1].ID, "BIN-BF-WALL-BURIED-PARTIAL", 5, 90*time.Minute)

	// THE ANSWER LANE: mouth partial, a FULL carrier buried behind it. Younger
	// than the trap, so it can only win by the trap being refused rather than by
	// being older.
	agedBinAt(t, db, bp.Code, p[0].ID, "BIN-BF-PARK-MOUTH", 6, 30*time.Minute)
	full := agedBinAt(t, db, bp.Code, p[1].ID, "BIN-BF-PARK-BURIED-FULL", 30, 60*time.Minute)

	r := &binresolver.DefaultResolver{DB: db}
	fullOnly := func(b *bins.Bin) bool { return b.UOPRemaining >= 30 }

	// (1) NO FILTER — the buried arm is FIFO across lanes and takes the oldest
	// buried bin, which is the trap. This is the behaviour demand must keep.
	res, err := r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, nil)
	var unfiltered *binresolver.BuriedError
	if !errors.As(err, &unfiltered) {
		t.Fatalf("unfiltered resolve = (%v, %v), want a BuriedError — every mouth here is "+
			"occupied, so the only candidates are buried", res, err)
	}
	if unfiltered.Bin.ID != trap.ID {
		t.Errorf("unfiltered buried = bin %d, want the OLDEST buried bin %d — a caller with no "+
			"constraint must still get FIFO", unfiltered.Bin.ID, trap.ID)
	}

	// (2) WITH THE DRAIN WINDOW'S FILTER — the trap is not a candidate, so the
	// buried scan keeps looking and names the FULL carrier instead.
	//
	// THIS IS THE ASSERTION THE MUTATION KILLS. Drop accept.accepts(buried) from
	// checkOldestBuried and this returns the trap: an excavation planned to
	// expose a partial that requiresFullCarrier then declines on arrival.
	res, err = r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, fullOnly)
	var filtered *binresolver.BuriedError
	if !errors.As(err, &filtered) {
		t.Fatalf("filtered resolve = (%v, %v), want a BuriedError naming the full carrier — a full "+
			"carrier is buried at %s and a dig is exactly the right answer", res, err, p[1].Name)
	}
	if filtered.Bin.ID != full.ID {
		t.Errorf("filtered buried = bin %d (uop %d), want the FULL bin %d — the filter must ride the "+
			"buried lookup too, or the dig is spent exposing a bin the caller refuses",
			filtered.Bin.ID, filtered.Bin.UOPRemaining, full.ID)
	}
	if filtered.LaneID != park.ID {
		t.Errorf("filtered buried lane = %d, want the park lane %d — the reshuffle has to be "+
			"planned against the lane the full carrier is actually in", filtered.LaneID, park.ID)
	}
}

// TestGroupRetrieve_BuriedFilterOnTheSingleDeepLane is the population the shipped
// demo plant actually has: ASSY is Lane_08 alone, depth 3. A partial at the mouth
// with a full behind it is the whole shape, and the filter must reach the full.
func TestGroupRetrieve_BuriedFilterOnTheSingleDeepLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	wall, _, w, _, bp := clearLaneFixture(t, db, "BURIEDONELANE")
	if _, err := db.Exec(`UPDATE payloads SET uop_capacity=30 WHERE code=$1`, bp.Code); err != nil {
		t.Fatalf("give the payload a capacity: %v", err)
	}
	group, err := db.GetNode(*wall.ParentID)
	testutil.MustNoErr(t, err, "reload the group")

	// SMN_022 / SMN_023, in the fixture's spelling: a partial at the mouth and a
	// full immediately behind it. Nothing else in the group.
	agedBinAt(t, db, bp.Code, w[0].ID, "BIN-B1L-MOUTH-PARTIAL", 3, 90*time.Minute)
	full := agedBinAt(t, db, bp.Code, w[1].ID, "BIN-B1L-BURIED-FULL", 30, 10*time.Minute)

	r := &binresolver.DefaultResolver{DB: db}
	res, err := r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, func(b *bins.Bin) bool { return b.UOPRemaining >= 30 })

	var buried *binresolver.BuriedError
	if !errors.As(err, &buried) {
		t.Fatalf("resolve = (%v, %v), want a BuriedError naming the full carrier. The mouth holds a "+
			"partial the drain window cannot use, so the accessible scan is empty and the full "+
			"behind it is the only answer", res, err)
	}
	if buried.Bin.ID != full.ID {
		t.Errorf("buried = bin %d, want the FULL bin %d", buried.Bin.ID, full.ID)
	}
}

// TestGroupRetrieve_BuriedFilterSkipsTheLaneItRefuses PINS A LIMIT, and is not a
// statement of design intent.
//
// checkOldestBuried asks FindOldestBuriedBin for ONE bin per lane and continues
// past the lane when the filter refuses it — so a refused buried bin skips the
// WHOLE LANE rather than looking deeper in it. On a lane holding
// partial / partial / full, the full at depth 3 is therefore not reached, and
// the caller waits.
//
// That is the safe direction to err — waiting costs nothing, and a dig planned
// against a refused carrier costs the plant's largest action — but it IS a limit,
// and the next person to widen this should know it exists rather than assume the
// scan walks a lane to the bottom. Deepening the lookup means a new query, not a
// new filter.
func TestGroupRetrieve_BuriedFilterSkipsTheLaneItRefuses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	wall, _, w, _, bp := clearLaneFixture(t, db, "BURIEDLIMIT")
	if _, err := db.Exec(`UPDATE payloads SET uop_capacity=30 WHERE code=$1`, bp.Code); err != nil {
		t.Fatalf("give the payload a capacity: %v", err)
	}
	group, err := db.GetNode(*wall.ParentID)
	testutil.MustNoErr(t, err, "reload the group")

	agedBinAt(t, db, bp.Code, w[0].ID, "BIN-BL-MOUTH", 3, 20*time.Minute)
	agedBinAt(t, db, bp.Code, w[1].ID, "BIN-BL-BURIED-PARTIAL", 4, 90*time.Minute)
	agedBinAt(t, db, bp.Code, w[2].ID, "BIN-BL-BURIED-FULL", 30, 60*time.Minute)

	r := &binresolver.DefaultResolver{DB: db}
	res, err := r.Resolve(group, binresolver.ResolveModeRetrieve, bp.Code, nil,
		reservations.DigAsker{}, func(b *bins.Bin) bool { return b.UOPRemaining >= 30 })

	var buried *binresolver.BuriedError
	if errors.As(err, &buried) {
		t.Fatalf("resolve named buried bin %d — if this now reaches the depth-3 full, the lookup "+
			"has been deepened and this limit pin should be replaced by the assertion that it "+
			"finds it", buried.Bin.ID)
	}
	if res != nil && res.Bin != nil {
		t.Fatalf("resolve = bin %d, want no candidate: every accessible bin is a partial the drain "+
			"window refuses", res.Bin.ID)
	}
}
