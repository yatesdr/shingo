//go:build docker

package service

import (
	"encoding/json"
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// Lane 0 item 5 (BRIEF-unification-D1; RULINGS-C2c-certification §4.1). A
// lifecycle announcement (bumpEpoch's, announceCount's) is addressed by the
// landing's rule for a placement with no order: not a seat, not sent; one
// claiming station, to it; several, to each; none, the last arm (every
// registered Edge). It used to go to every Edge, and on a two-Edge plant the
// Edge that does not model the node logged "process node not found" for each.

// adjustmentDsts returns the destination station of every UOPAdjustment row
// in the outbox for binID ("" is the broadcast address).
func adjustmentDsts(t *testing.T, db *store.DB, binID int64) []string {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+protocol.SubjectUOPAdjustment)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan")
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(raw, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, env.DecodePayload(&data), "decode data")
		var adj protocol.UOPAdjustment
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &adj), "decode adjustment")
		if adj.BinID == binID {
			out = append(out, env.Dst.Station)
		}
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return out
}

// claimSeat records station as claiming node in the claims mirror, as its
// plant.claims report would.
func claimSeat(t *testing.T, db *store.DB, process, station, node string) {
	t.Helper()
	_, err := db.ReplacePlantClaimsFromStation(process, station,
		[]plantclaims.StyleRow{{ProcessID: process, StyleID: process + "-RUN", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: process, StyleID: process + "-RUN", CoreNodeName: node,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSequential, PayloadCode: "PART-A", UOPCapacity: 100}}, nil, 0)
	testutil.MustNoErr(t, err, "mirror: "+station+" claims "+node)
}

// ownedSeat creates a seat node that station claims: where a lifecycle
// announcement for a bin at it goes to that station alone.
func ownedSeat(t *testing.T, db *store.DB, name, station string) *nodes.Node {
	t.Helper()
	n := unclaimedSeat(t, db, name)
	claimSeat(t, db, "PROC-"+name, station, n.Name)
	return n
}

func twoEdges(t *testing.T, db *store.DB, a, b string) {
	t.Helper()
	registerEdgeSchema(t, db, a, protocol.EdgeSchemaBinLanded)
	registerEdgeSchema(t, db, b, protocol.EdgeSchemaBinLanded)
}

// TestLifecycleAnnounce_ALoadAtAnOwnedSeatGoesToItsStationOnly: a load at a
// seat station A claims, on a plant with A and B: one row, to A.
func TestLifecycleAnnounce_ALoadAtAnOwnedSeatGoesToItsStationOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	const a, b = "edge-la-a.line", "edge-la-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "PLK-LA-W1")
	claimSeat(t, db, "LOADER-LA", a, seat.Name)

	bin := createTestBin(t, db, seat.ID, "BIN-LA-LOAD", "", 0)
	_, err := svc.SetForProduction(bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40, protocol.DeclaredByPerson)
	testutil.MustNoErr(t, err, "the load")

	if got := adjustmentDsts(t, db, bin.ID); !slices.Equal(got, []string{a}) {
		t.Fatalf("load announcement rows addressed to %q, want exactly one, to %q (none to %q)", got, a, b)
	}
}

// TestLifecycleAnnounce_AClearAtAnOwnedSeatGoesToItsStationOnly: the clear's
// bump, the same rule.
func TestLifecycleAnnounce_AClearAtAnOwnedSeatGoesToItsStationOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	const a, b = "edge-lc-a.line", "edge-lc-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "FGN-LC-01")
	claimSeat(t, db, "DRAIN-LC", a, seat.Name)

	bin := createTestBin(t, db, seat.ID, "BIN-LC-CLEAR", "PART-A", 30)
	_, err := svc.ClearForReuse(bin.ID, nil, protocol.DeclaredByPerson)
	testutil.MustNoErr(t, err, "the clear")

	if got := adjustmentDsts(t, db, bin.ID); !slices.Equal(got, []string{a}) {
		t.Fatalf("clear announcement rows addressed to %q, want exactly one, to %q", got, a)
	}
}

// TestLifecycleAnnounce_ABumpAtStorageSendsNothing: a storage node is not a
// seat; no Edge models it.
func TestLifecycleAnnounce_ABumpAtStorageSendsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	twoEdges(t, db, "edge-ls-a.line", "edge-ls-b.line")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-LS-CLEAR", "PART-A", 30)
	_, err := svc.ClearForReuse(bin.ID, nil, protocol.DeclaredByLifecycle)
	testutil.MustNoErr(t, err, "the clear")

	if got := adjustmentDsts(t, db, bin.ID); len(got) != 0 {
		t.Fatalf("a bump at storage enqueued %d announcement row(s) (to %q), want none", len(got), got)
	}
}

// TestLifecycleAnnounce_ACountAtAnOwnedSeatGoesToItsStation: announceCount,
// the same rule.
func TestLifecycleAnnounce_ACountAtAnOwnedSeatGoesToItsStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	binSvc := NewBinService(db, announcingService(db))
	const a, b = "edge-lk-a.line", "edge-lk-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "ALN-LK-01")
	claimSeat(t, db, "WELD-LK", a, seat.Name)

	bin := createTestBin(t, db, seat.ID, "BIN-LK-COUNT", "PART-A", 30)
	fresh, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	_, err = binSvc.RecordCount(fresh, 25, "operator-under-test")
	testutil.MustNoErr(t, err, "the count")

	if got := adjustmentDsts(t, db, bin.ID); !slices.Equal(got, []string{a}) {
		t.Fatalf("count announcement rows addressed to %q, want exactly one, to %q", got, a)
	}
}

// TestLifecycleAnnounce_AnUnclaimedSeatTakesTheLastArm: no station claims the
// seat and there is no order: every registered Edge, each addressed.
func TestLifecycleAnnounce_AnUnclaimedSeatTakesTheLastArm(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	const a, b = "edge-lu-a.line", "edge-lu-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "PLK-LU-W1")

	bin := createTestBin(t, db, seat.ID, "BIN-LU-LOAD", "", 0)
	_, err := svc.SetForProduction(bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40, protocol.DeclaredByPerson)
	testutil.MustNoErr(t, err, "the load")

	got := adjustmentDsts(t, db, bin.ID)
	slices.Sort(got)
	if !slices.Equal(got, []string{a, b}) {
		t.Fatalf("load at an unclaimed seat addressed to %q, want one row to each of %q and %q", got, a, b)
	}
}

// TestLifecycleAnnounce_ASeatTwoStationsClaimGoesToBoth: several claiming: a
// correction goes to each (owner ruling 2026-11-22; it went to nobody).
func TestLifecycleAnnounce_ASeatTwoStationsClaimGoesToBoth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	const a, b = "edge-l2-a.line", "edge-l2-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "ALN-L2-01")
	claimSeat(t, db, "WELD-L2A", a, seat.Name)
	claimSeat(t, db, "WELD-L2B", b, seat.Name)

	bin := createTestBin(t, db, seat.ID, "BIN-L2-CLEAR", "PART-A", 30)
	_, err := svc.ClearForReuse(bin.ID, nil, protocol.DeclaredByLifecycle)
	testutil.MustNoErr(t, err, "the clear")

	got := adjustmentDsts(t, db, bin.ID)
	slices.Sort(got)
	if !slices.Equal(got, []string{a, b}) {
		t.Fatalf("a seat two stations claim got announcement rows to %q, want one to each of %q and %q", got, a, b)
	}
}

// TestLifecycleAnnounce_ALoadAtAPublishedLoaderWindowGoesToItsStation: the
// loader window no claim names, published as a position (Lane 0 item 6): the
// load's announcement goes to the publishing station alone, not the last arm.
func TestLifecycleAnnounce_ALoadAtAPublishedLoaderWindowGoesToItsStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)
	const a, b = "edge-lp-a.line", "edge-lp-b.line"
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "PLK-LP-W1")
	_, err := db.ReplacePlantClaimsFromStation("LOADER-LP", a,
		[]plantclaims.StyleRow{{ProcessID: "LOADER-LP", StyleID: "LOADER-LP-RUN", IsActive: true}},
		nil, []string{seat.Name}, 0)
	testutil.MustNoErr(t, err, "mirror: station a publishes the loader window as a position")

	bin := createTestBin(t, db, seat.ID, "BIN-LP-LOAD", "", 0)
	_, err = svc.SetForProduction(bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40, protocol.DeclaredByPerson)
	testutil.MustNoErr(t, err, "the load")

	if got := adjustmentDsts(t, db, bin.ID); !slices.Equal(got, []string{a}) {
		t.Fatalf("load at a published loader window addressed to %q, want exactly one, to %q", got, a)
	}
}

// twoClaimSeat is a seat two stations claim with a bin on it that only a has
// counted: a's cursor in the generation is net -6 at seq 2, and b has none.
func twoClaimSeat(t *testing.T, label, a, b string) (*BinService, int64, int64) {
	t.Helper()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})
	twoEdges(t, db, a, b)
	seat := unclaimedSeat(t, db, "SEAT-"+label)
	claimSeat(t, db, "PROC-A-"+label, a, seat.Name)
	claimSeat(t, db, "PROC-B-"+label, b, seat.Name)
	bin := createTestBin(t, db, seat.ID, label, "PART-A", 100)
	epoch := binEpoch(t, db, bin.ID)
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(a, netDelta(bin.ID, epoch, 1, -4, -4)), "station a counts")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(a, netDelta(bin.ID, epoch, 2, -2, -6)), "station a counts again")
	return NewBinService(db, manifest), bin.ID, epoch
}

// TestLifecycleAnnounce_EveryCorrectionAtATwoClaimSeat: the other three
// corrections of TestLifecycleAnnounce_ASeatTwoStationsClaimGoesToBoth's
// rule, at a seat two stations claim: a person's count, a person's reset and a
// late draw. Each goes to both as Core's own, so it binds neither, and each
// copy is fenced at its own station's stream.
func TestLifecycleAnnounce_EveryCorrectionAtATwoClaimSeat(t *testing.T) {
	t.Parallel()
	const a, b = "edge-tc-a.line", "edge-tc-b.line"
	wantBoth := func(t *testing.T, what string, db *store.DB, binID int64, count int, epoch, aNet, aSeq int64) map[string]protocol.UOPAdjustment {
		t.Helper()
		got := fencedTo(t, db, binID)
		if len(got) != 2 {
			t.Fatalf("%s at a seat two stations claim went to %d stations (%+v), want both", what, len(got), got)
		}
		for st, fence := range map[string][2]int64{a: {aNet, aSeq}, b: {0, 0}} {
			adj := got[st]
			if adj.Actor != protocol.ActorCoreLifecycle || adj.NewRemaining != count || adj.Epoch != epoch {
				t.Errorf("%s to %s says %d at epoch %d by %q, want %d at %d by %q (a person would bind the empty seat)",
					what, st, adj.NewRemaining, adj.Epoch, adj.Actor, count, epoch, protocol.ActorCoreLifecycle)
			}
			assertFence(t, what, st, adj, fence[0], fence[1])
		}
		return got
	}

	t.Run("a person's count", func(t *testing.T) {
		t.Parallel()
		svc, binID, epoch := twoClaimSeat(t, "BIN-TC-COUNT", a, b)
		fresh, err := svc.db.GetBin(binID)
		testutil.MustNoErr(t, err, "read bin")
		_, err = svc.RecordCount(fresh, 50, "operator-under-test")
		testutil.MustNoErr(t, err, "RecordCount")
		wantBoth(t, "the count", svc.db, binID, 50, epoch, -6, 2)
	})

	t.Run("a person's reset", func(t *testing.T) {
		t.Parallel()
		svc, binID, epoch := twoClaimSeat(t, "BIN-TC-RESET", a, b)
		_, err := svc.manifest.ClearForReuse(binID, nil, protocol.DeclaredByPerson)
		testutil.MustNoErr(t, err, "the reset")
		for st, adj := range wantBoth(t, "the reset", svc.db, binID, 0, epoch+1, 0, 0) {
			if !adj.PayloadKnown {
				t.Errorf("the reset to %s names no part: a person's reset says what the carrier holds", st)
			}
		}
	})

	t.Run("a late draw", func(t *testing.T) {
		t.Parallel()
		svc, binID, epoch := twoClaimSeat(t, "BIN-TC-LATE", a, b)
		tx, err := svc.db.Begin()
		testutil.MustNoErr(t, err, "begin")
		defer func() { _ = tx.Rollback() }()
		atSeat, err := svc.manifest.AnnounceLateDrawTx(tx, binID, epoch, 90)
		testutil.MustNoErr(t, err, "the late draw")
		testutil.MustNoErr(t, tx.Commit(), "commit")
		if !atSeat {
			t.Fatal("the late draw found no seat")
		}
		wantBoth(t, "the late draw", svc.db, binID, 90, epoch, -6, 2)
	})
}

// TestLandingStations_SeveralClaimants: the landing's rule where several
// stations claim the node. A landing binds, so it needs one addressee: the
// order's station when it is one of them, else nobody (owner ruling
// 2026-11-22: landingStations is unchanged where corrections go to all).
func TestLandingStations_SeveralClaimants(t *testing.T) {
	t.Parallel()
	a, b := plantclaims.AddressedStation{StationID: "a"}, plantclaims.AddressedStation{StationID: "b"}
	both := []plantclaims.AddressedStation{a, b}
	for _, tc := range []struct {
		name  string
		order plantclaims.AddressedStation
		want  []plantclaims.AddressedStation
	}{
		{name: "no order"},
		{name: "an order of one of them", order: b, want: []plantclaims.AddressedStation{b}},
		{name: "an order of a third", order: plantclaims.AddressedStation{StationID: "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := landingStations(plantclaims.LandingAddress{Claiming: both, Order: tc.order}); !slices.Equal(got, tc.want) {
				t.Errorf("landing at a seat two claim goes to %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClaimingStations_AStaleStationsClaimsCount: the claimants are read from
// the mirror with no look at the registry's status, so a station that stopped
// heartbeating still claims what it last reported. It stays one of two
// claimants, and it alone takes what lands at a seat only it claims.
func TestClaimingStations_AStaleStationsClaimsCount(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	const old, live = "edge-st-old.line", "edge-st-live.line"
	twoEdges(t, db, old, live)
	shared, alone := unclaimedSeat(t, db, "ALN-ST-01"), unclaimedSeat(t, db, "ALN-ST-02")
	claimSeat(t, db, "WELD-ST-OLD-1", old, shared.Name)
	claimSeat(t, db, "WELD-ST-OLD-2", old, alone.Name)
	claimSeat(t, db, "WELD-ST-LIVE", live, shared.Name)
	_, err := db.Exec(`UPDATE edge_registry SET status = 'stale' WHERE station_uid = $1`, old)
	testutil.MustNoErr(t, err, "the old station goes stale")

	for node, want := range map[string][]string{shared.Name: {live, old}, alone.Name: {old}} {
		got, err := db.StationsClaimingNode(node)
		testutil.MustNoErr(t, err, "claimants of "+node)
		if !slices.Equal(got, want) {
			t.Errorf("claimants of %s = %q, want %q: a stale station's claims count", node, got, want)
		}
	}
	bin := createTestBin(t, db, alone.ID, "BIN-ST-CLEAR", "PART-A", 30)
	_, err = announcingService(db).ClearForReuse(bin.ID, nil, protocol.DeclaredByLifecycle)
	testutil.MustNoErr(t, err, "the clear")
	if got := adjustmentDsts(t, db, bin.ID); !slices.Equal(got, []string{old}) {
		t.Errorf("a clear at the stale station's seat went to %q, want it alone (%q)", got, old)
	}
}
