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

// Lane H, H.2/H.3 (BRIEF-unification-C2c 2026-10-10; DECISIONS D9, D10). A
// final drop at a seat a station claims enqueues exactly one landing, in the
// transaction that moves the bin, addressed to the claiming station; a drop at
// a storage slot enqueues none. The landing carries the part as known (H.1)
// and a placement stamp (D10).
//
// Enabled with the source (it was behind the laneh_pending tag while
// protocol.SubjectBinLanded did not exist). At the base the arrival announced
// nothing on the final drop (wiring_block_completed.go :438 returns;
// order.delivered is sent after the commit, outside the move's tx).
//
// H.7 sends the landing only to a station registered at or above
// protocol.EdgeSchemaBinLanded, so the fixtures register the claiming station
// at that schema (registerEdgeSchema); an unregistered station is an old Edge
// and gets today's wire (TestPinH7_*).

// registerEdgeSchema records station in edge_registry at schema version v, as
// its register message would (edge_registry.schema_version, v137).
func registerEdgeSchema(t *testing.T, db *store.DB, station string, v int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO edge_registry (station_id, schema_version) VALUES ($1, $2)
		ON CONFLICT (station_id) DO UPDATE SET schema_version = EXCLUDED.schema_version`, station, v)
	testutil.MustNoErr(t, err, "register the station's schema")
}

func landingsFor(t *testing.T, db *store.DB, binID int64) []struct {
	Dst string
	L   protocol.BinLanded
} {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+protocol.SubjectBinLanded)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []struct {
		Dst string
		L   protocol.BinLanded
	}
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan")
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(raw, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, env.DecodePayload(&data), "decode data")
		var l protocol.BinLanded
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &l), "decode landing")
		if l.BinID == binID {
			out = append(out, struct {
				Dst string
				L   protocol.BinLanded
			}{env.Dst.Station, l})
		}
	}
	return out
}

// TestPinH2_AFinalDropAtAClaimedSeatEnqueuesOneLandingToTheClaimingStation —
// H.2 (one outbox row in the move's tx) and H.3 (addressed by the claims
// mirror, not every Edge).
func TestPinH2_AFinalDropAtAClaimedSeatEnqueuesOneLandingToTheClaimingStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)

	const station = "edge-h2.line"
	_, err := db.ReplacePlantClaimsFromStation("proc-h2", station,
		[]plantclaims.StyleRow{{ProcessID: "proc-h2", StyleID: "style-h2", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "proc-h2", StyleID: "style-h2", CoreNodeName: sd.LineNode.Name,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: "PART-A", UOPCapacity: 100}}, nil, 0)
	testutil.MustNoErr(t, err, "mirror: the station claims the line seat")
	registerEdgeSchema(t, db, station, protocol.EdgeSchemaBinLanded)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-H2-SEAT", "PART-A", 80)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "the lift")
	_, err = binSvc.ApplyArrival(bin.ID, sd.LineNode.ID, false, nil, order.ID)
	testutil.MustNoErr(t, err, "the final drop")

	got := landingsFor(t, db, bin.ID)
	if len(got) != 1 {
		t.Fatalf("landings = %d, want exactly 1", len(got))
	}
	l := got[0]
	if l.Dst != station {
		t.Errorf("landing addressed to %q, want the claiming station %q", l.Dst, station)
	}
	if l.L.CoreNodeName != sd.LineNode.Name || l.L.Count != 80 {
		t.Errorf("landing = node %q count %d, want %q/80", l.L.CoreNodeName, l.L.Count, sd.LineNode.Name)
	}
	if l.L.PayloadCode != "PART-A" || !l.L.PayloadKnown {
		t.Errorf("landing part = %q known=%v, want PART-A known (H.1)", l.L.PayloadCode, l.L.PayloadKnown)
	}
	if l.L.Epoch != binEpoch(t, db, bin.ID) {
		t.Errorf("landing generation = %d, want the bin's %d", l.L.Epoch, binEpoch(t, db, bin.ID))
	}
	if l.L.PlacedSeq <= 0 || l.L.PlacedAt.IsZero() {
		t.Errorf("landing placement = seq %d at %v, want a stamp (D10)", l.L.PlacedSeq, l.L.PlacedAt)
	}
	if l.L.OrderUUID == "" {
		t.Error("landing carries no order uuid")
	}
}

// TestPinH2_ADropAtAStorageSlotEnqueuesNoLanding — H.3: storage class, not
// sent.
func TestPinH2_ADropAtAStorageSlotEnqueuesNoLanding(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-H2-STOR", "PART-A", 80)
	order := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "the lift")
	_, err := binSvc.ApplyArrival(bin.ID, sd.StorageNode.ID, false, nil, order.ID)
	testutil.MustNoErr(t, err, "the drop at storage")

	if got := landingsFor(t, db, bin.ID); len(got) != 0 {
		t.Errorf("landings at a storage slot = %d, want 0", len(got))
	}
}

// TestPinH2_TwoArrivalsGetIncreasingPlacementStamps — D10: the stamp is a
// Core sequence, monotonic per Core, so a seat refuses an older landing.
func TestPinH2_TwoArrivalsGetIncreasingPlacementStamps(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)
	_, err := db.ReplacePlantClaimsFromStation("proc-h2b", "edge-h2b.line",
		[]plantclaims.StyleRow{{ProcessID: "proc-h2b", StyleID: "s", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "proc-h2b", StyleID: "s", CoreNodeName: sd.LineNode.Name,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: "PART-A"}}, nil, 0)
	testutil.MustNoErr(t, err, "mirror")
	registerEdgeSchema(t, db, "edge-h2b.line", protocol.EdgeSchemaBinLanded)

	var seqs []int64
	for _, label := range []string{"BIN-H2-SEQ-1", "BIN-H2-SEQ-2"} {
		bin := createTestBin(t, db, sd.StorageNode.ID, label, "PART-A", 10)
		order := createTestOrder(t, db, sd.LineNode.ID)
		claimBinForTest(t, db, bin.ID, order.ID)
		testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "lift")
		_, err := binSvc.ApplyArrival(bin.ID, sd.LineNode.ID, false, nil, order.ID)
		testutil.MustNoErr(t, err, "drop")
		got := landingsFor(t, db, bin.ID)
		if len(got) != 1 {
			t.Fatalf("%s: landings = %d, want 1", label, len(got))
		}
		seqs = append(seqs, got[0].L.PlacedSeq)
		testutil.MustNoErr(t, binSvc.MoveToTransit(bin.ID), "clear the seat for the next")
	}
	if seqs[1] <= seqs[0] {
		t.Errorf("placement stamps %v, want increasing", seqs)
	}
}

// boundsFor returns the destination station of every Bound UOPAdjustment
// queued for the bin.
func boundsFor(t *testing.T, db *store.DB, binID int64) []string {
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
		var a protocol.UOPAdjustment
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &a), "decode adjustment")
		if a.BinID == binID && a.Bound {
			out = append(out, env.Dst.Station)
		}
	}
	return out
}

// TestPinH7_AnOldEdgeGetsTodaysWireAddressed — H.7, new Core with an old Edge
// (schema below protocol.EdgeSchemaBinLanded, or never reported): no landing,
// which it could not bind. A mid-order set-down gets the Bound it always got,
// addressed to the claiming station instead of broadcast; a final drop gets
// nothing new (its carrier still rides order.delivered's bind fields).
func TestPinH7_AnOldEdgeGetsTodaysWireAddressed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	binSvc := NewBinService(db, manifest)

	const station = "edge-h7.line"
	_, err := db.ReplacePlantClaimsFromStation("proc-h7", station,
		[]plantclaims.StyleRow{{ProcessID: "proc-h7", StyleID: "s", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "proc-h7", StyleID: "s", CoreNodeName: sd.LineNode.Name,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeSimple, PayloadCode: "PART-A"}}, nil, 0)
	testutil.MustNoErr(t, err, "mirror")
	registerEdgeSchema(t, db, station, protocol.EdgeSchemaBinLanded-1)

	// A mid-order set-down at the seat.
	mid := createTestBin(t, db, sd.StorageNode.ID, "BIN-H7-MID", "PART-A", 30)
	midOrder := createTestOrder(t, db, sd.StorageNode.ID)
	claimBinForTest(t, db, mid.ID, midOrder.ID)
	testutil.MustNoErr(t, binSvc.MoveToTransit(mid.ID), "lift")
	_, err = binSvc.ApplyIntermediateStore(mid.ID, sd.LineNode.ID, true, nil, midOrder.ID)
	testutil.MustNoErr(t, err, "set-down")
	if got := landingsFor(t, db, mid.ID); len(got) != 0 {
		t.Errorf("landings to an old Edge = %d, want 0", len(got))
	}
	if got := boundsFor(t, db, mid.ID); len(got) != 1 || got[0] != station {
		t.Errorf("Bounds = %v, want exactly one, to %q (addressed, not broadcast)", got, station)
	}

	// The seat cleared; a final drop there.
	testutil.MustNoErr(t, binSvc.MoveToTransit(mid.ID), "the set-down bin leaves")
	final := createTestBin(t, db, sd.StorageNode.ID, "BIN-H7-FINAL", "PART-A", 40)
	finalOrder := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, final.ID, finalOrder.ID)
	testutil.MustNoErr(t, binSvc.MoveToTransit(final.ID), "lift")
	_, err = binSvc.ApplyArrival(final.ID, sd.LineNode.ID, true, nil, finalOrder.ID)
	testutil.MustNoErr(t, err, "final drop")
	if got := landingsFor(t, db, final.ID); len(got) != 0 {
		t.Errorf("landings to an old Edge = %d, want 0", len(got))
	}
	if got := boundsFor(t, db, final.ID); len(got) != 0 {
		t.Errorf("Bounds on a final drop = %v, want none (today's wire is order.delivered)", got)
	}
}

// ── THE LAST ARM (RULINGS-C2c-run-through §4; DECISIONS D19's reach gap) ──
//
// A seat that no station's mirror row claims, reached with no order station:
// an admin move onto a loader or FG seat (the Edge's publisher skips
// manual_swap claims, so those seats are not in the mirror). Before the
// landing, the admin Move's broadcast Bound reached it; the landing as built
// reached nobody, and order.delivered no longer binds. The ruling: the plant's
// only Edge, or every Edge when there are several, each under its own version
// rule. "Never broadcast" was the brief's error.

// unclaimedSeat is a parentless, untyped node no mirror row names: a loader or
// FG seat as Core's tables see it (a seat, not storage).
func unclaimedSeat(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create the seat")
	return n
}

// TestPinD19_AnAdminMoveOntoAnUnclaimedSeatReachesThePlantsOnlyEdge — one
// Edge: exactly one landing, addressed to it.
func TestPinD19_AnAdminMoveOntoAnUnclaimedSeatReachesThePlantsOnlyEdge(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	binSvc := NewBinService(db, announcingService(db))
	const edge = "edge-d19a.line"
	registerEdgeSchema(t, db, edge, protocol.EdgeSchemaBinLanded)
	seat := unclaimedSeat(t, db, "LDR-D19A-SEAT")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-D19A", "PART-A", 60)
	_, err := binSvc.MoveByHand(bin, seat.ID)
	testutil.MustNoErr(t, err, "the admin move onto the loader seat")

	got := landingsFor(t, db, bin.ID)
	if len(got) != 1 || got[0].Dst != edge {
		dsts := make([]string, len(got))
		for i, g := range got {
			dsts[i] = g.Dst
		}
		t.Fatalf("landings = %v, want exactly one, to the plant's only Edge %q: a seat no station "+
			"claims, reached with no order station, reached nobody", dsts, edge)
	}
	if got[0].L.CoreNodeName != seat.Name || got[0].L.Count != 60 {
		t.Errorf("landing = node %q count %d, want %q/60", got[0].L.CoreNodeName, got[0].L.Count, seat.Name)
	}
}

// TestPinD19_AnAdminMoveOntoAnUnclaimedSeatReachesEveryEdge — two Edges: a
// landing to each.
func TestPinD19_AnAdminMoveOntoAnUnclaimedSeatReachesEveryEdge(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	binSvc := NewBinService(db, announcingService(db))
	edges := []string{"edge-d19b.line1", "edge-d19b.line2"}
	for _, e := range edges {
		registerEdgeSchema(t, db, e, protocol.EdgeSchemaBinLanded)
	}
	seat := unclaimedSeat(t, db, "FG-D19B-SEAT")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-D19B", "PART-A", 60)
	_, err := binSvc.MoveByHand(bin, seat.ID)
	testutil.MustNoErr(t, err, "the admin move onto the FG seat")

	var dsts []string
	for _, g := range landingsFor(t, db, bin.ID) {
		dsts = append(dsts, g.Dst)
	}
	if len(dsts) != 2 || dsts[0] == dsts[1] || !slices.Contains(dsts, edges[0]) || !slices.Contains(dsts, edges[1]) {
		t.Fatalf("landings to %v, want one to each of %v", dsts, edges)
	}
}

// TestPinD19_AnOldEdgeAmongSeveralGetsItsBoundAddressed — the per-Edge
// version rule holds on the last arm: the new Edge gets the landing, the old
// one the Bound the admin move always sent it, addressed.
func TestPinD19_AnOldEdgeAmongSeveralGetsItsBoundAddressed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	binSvc := NewBinService(db, announcingService(db))
	const newEdge, oldEdge = "edge-d19c.new", "edge-d19c.old"
	registerEdgeSchema(t, db, newEdge, protocol.EdgeSchemaBinLanded)
	registerEdgeSchema(t, db, oldEdge, protocol.EdgeSchemaBinLanded-1)
	seat := unclaimedSeat(t, db, "LDR-D19C-SEAT")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-D19C", "PART-A", 60)
	_, err := binSvc.MoveByHand(bin, seat.ID)
	testutil.MustNoErr(t, err, "the admin move")

	if got := landingsFor(t, db, bin.ID); len(got) != 1 || got[0].Dst != newEdge {
		t.Errorf("landings = %v, want one, to %q", got, newEdge)
	}
	if got := boundsFor(t, db, bin.ID); len(got) != 1 || got[0] != oldEdge {
		t.Errorf("Bounds = %v, want one, to %q", got, oldEdge)
	}
}

// TestPinD19_AnAdminMoveOntoStorageStillReachesNobody — the last arm is for
// seats: a storage slot is still not sent (a guard; green before and after).
func TestPinD19_AnAdminMoveOntoStorageStillReachesNobody(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	binSvc := NewBinService(db, announcingService(db))
	registerEdgeSchema(t, db, "edge-d19d.line", protocol.EdgeSchemaBinLanded)
	registerEdgeSchema(t, db, "edge-d19d.old", protocol.EdgeSchemaBinLanded-1)

	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-D19D", "PART-A", 60)
	_, err := binSvc.MoveByHand(bin, sd.StorageNode.ID)
	testutil.MustNoErr(t, err, "the admin move onto storage")

	if got := landingsFor(t, db, bin.ID); len(got) != 0 {
		t.Errorf("landings at a storage slot = %d, want 0", len(got))
	}
	if got := boundsFor(t, db, bin.ID); len(got) != 0 {
		t.Errorf("Bounds at a storage slot = %v, want none", got)
	}
}
