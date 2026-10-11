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
// claiming station, to it; several, to nobody; none, the last arm (every
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

// TestLifecycleAnnounce_ASeatTwoStationsClaimGoesToNobody: several claiming
// and no order station to decide: nobody (a guess binds at the wrong Edge).
func TestLifecycleAnnounce_ASeatTwoStationsClaimGoesToNobody(t *testing.T) {
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

	if got := adjustmentDsts(t, db, bin.ID); len(got) != 0 {
		t.Fatalf("a seat two stations claim got announcement rows to %q, want none", got)
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
