//go:build docker

package service

import (
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// The record-count fence, Core half (SYNTH-round2 S7, citrine-kestrel §8 S4).
// RecordCount still writes the counted number absolutely and does not bump the
// epoch. What it adds is a statement of WHERE in the counting station's stream
// that number was taken: Core's applied_net and last_seq for (station, bin,
// epoch), read inside the count's transaction, carried on the UOPAdjustment the
// count enqueues in that same transaction.

func netDelta(binID, epoch, seq int64, delta int, net int64) *protocol.BinUOPDelta {
	d := consumeDelta(binID, epoch, seq, delta)
	d.Net = &net
	return d
}

func countWithFenceFixture(t *testing.T, label string) (*BinService, *InventoryDeltaService, int64, int64, string) {
	t.Helper()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	bins := NewBinService(db, manifest)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})
	// CONVERTED (Lane 0 item 5): the bin stood at STORAGE-A1, a STOR node, where
	// the announcement was broadcast. It is now addressed like the landing and a
	// storage node is not sent, so the bin stands at a seat pinStation claims.
	seat := ownedSeat(t, db, "SEAT-"+label, pinStation)
	bin := createTestBin(t, db, seat.ID, label, "PART-A", 100)
	return bins, deltas, bin.ID, binEpoch(t, db, bin.ID), seat.Name
}

func TestRecordCount_AnnouncesTheCountWithTheStationsCursor(t *testing.T) {
	t.Parallel()
	svc, deltas, binID, epoch, nodeName := countWithFenceFixture(t, "BIN-FENCE-1")
	db := svc.db

	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, netDelta(binID, epoch, 1, -10, -10)), "seq 1")

	fresh, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	res, err := svc.RecordCount(fresh, 50, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")

	adjs := outboxAdjustments(t, db, binID)
	if len(adjs) != 1 {
		t.Fatalf("RecordCount enqueued %d UOPAdjustments, want 1 (in its own transaction)", len(adjs))
	}
	a := adjs[0]
	if a.NewRemaining != 50 || a.Epoch != epoch || a.CoreNodeName != nodeName {
		t.Errorf("adjustment = {remaining %d, epoch %d, node %q}, want {50, %d, %q}",
			a.NewRemaining, a.Epoch, a.CoreNodeName, epoch, nodeName)
	}
	if a.AsOfNet == nil || *a.AsOfNet != -10 || a.AsOfSeq == nil || *a.AsOfSeq != 1 || a.AsOfStation != pinStation {
		t.Errorf("fence = {net %v, seq %v, station %q}, want {-10, 1, %q}: the station can only "+
			"rebase on the point in its own stream Core had reached", a.AsOfNet, a.AsOfSeq, a.AsOfStation, pinStation)
	}
	if protocol.IsLifecycleActor(a.Actor) {
		t.Errorf("actor = %q: a count is a person declaring a number", a.Actor)
	}
	if net, seq, st := res.FenceFor(pinStation); net == nil || *net != -10 || seq == nil || *seq != 1 ||
		st != pinStation || res.Epoch != epoch {
		t.Errorf("CountResult fence for %s = {%v, %v, %q, epoch %d}, want the same fence as the message",
			pinStation, net, seq, st, res.Epoch)
	}
	if net, seq, st := res.FenceFor(""); net != nil || seq != nil || st != "" {
		t.Errorf("CountResult fence for no station = {%v, %v, %q}, want none: a count made at Core "+
			"asks about no station's stream", net, seq, st)
	}

	// The window in flight at the count lands on top of it at Core, and the
	// station, rebasing on AsOfNet, subtracts the same window: both read 45.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, netDelta(binID, epoch, 2, -5, -15)), "seq 2")
	if got := binRemaining(t, db, binID); got != 45 {
		t.Errorf("Core after the in-flight window = %d, want 45", got)
	}
	if got := binEpoch(t, db, binID); got != epoch {
		t.Errorf("RecordCount moved delta_epoch %d -> %d; the fence replaces a bump, it does not add one", epoch, got)
	}
}

// TestRecordCount_NoCursorFencesAtTheStart: no station has counted this
// carrier in this generation, so the station addressed is fenced at net 0,
// seq 0 of its own stream: Core has applied none of its windows, and every one
// it has counted lands on top of the number at Core as the station adds it to
// its own. This replaced TestRecordCount_NoCursorMeansNoFence, which pinned the
// count going out unfenced (the info-flow build's C6: every count Core states
// is fenced per addressed station).
func TestRecordCount_NoCursorFencesAtTheStart(t *testing.T) {
	t.Parallel()
	svc, _, binID, _, _ := countWithFenceFixture(t, "BIN-FENCE-2")
	fresh, err := svc.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	res, err := svc.RecordCount(fresh, 7, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")
	adjs := outboxAdjustments(t, svc.db, binID)
	if len(adjs) != 1 {
		t.Fatalf("enqueued %d UOPAdjustments, want 1", len(adjs))
	}
	if a := adjs[0]; a.AsOfNet == nil || *a.AsOfNet != 0 || a.AsOfSeq == nil || *a.AsOfSeq != 0 || a.AsOfStation != pinStation {
		t.Errorf("fence = {%v, %v, %q}, want {0, 0, %q}: no window of the station's is in Core's number",
			a.AsOfNet, a.AsOfSeq, a.AsOfStation, pinStation)
	}
	if net, seq, st := res.FenceFor(pinStation); net == nil || *net != 0 || seq == nil || *seq != 0 || st != pinStation {
		t.Errorf("CountResult fence for %s = {%v, %v, %q}, want the message's {0, 0}", pinStation, net, seq, st)
	}
}

func TestRecordCount_UnanchoredCursorMeansNoFence(t *testing.T) {
	t.Parallel()
	svc, deltas, binID, epoch, _ := countWithFenceFixture(t, "BIN-FENCE-3")
	// An Edge built before the running net: the row exists, applied_net is NULL.
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, consumeDelta(binID, epoch, 1, -4)), "seq 1, no net")
	fresh, err := svc.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	_, err = svc.RecordCount(fresh, 7, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")
	adjs := outboxAdjustments(t, svc.db, binID)
	if len(adjs) != 1 || adjs[0].AsOfNet != nil || adjs[0].AsOfSeq != nil {
		t.Errorf("adjustments = %+v, want one with no fence: without a net there is nothing to rebase on", adjs)
	}
}

// TestRecordCount_TwoCountingStationsEachIsFencedAtItsOwn: two stations have
// counted this carrier in this generation, and the one the count is addressed
// to (the station claiming the seat) is fenced at its own cursor, never the
// other's: one net cannot fence two streams, but each copy measures one. This
// replaced TestRecordCount_TwoCountingStationsMeansNoFence, which pinned the
// count going out unfenced (C6).
func TestRecordCount_TwoCountingStationsEachIsFencedAtItsOwn(t *testing.T) {
	t.Parallel()
	svc, deltas, binID, epoch, _ := countWithFenceFixture(t, "BIN-FENCE-4")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, netDelta(binID, epoch, 1, -3, -3)), "station 1")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta("stn-other", netDelta(binID, epoch, 1, -2, -2)), "station 2")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta("stn-other", netDelta(binID, epoch, 2, -2, -4)), "station 2 again")
	fresh, err := svc.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	res, err := svc.RecordCount(fresh, 7, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")
	adjs := outboxAdjustments(t, svc.db, binID)
	if len(adjs) != 1 {
		t.Fatalf("enqueued %d UOPAdjustments, want 1 (to the station claiming the seat)", len(adjs))
	}
	if a := adjs[0]; a.AsOfNet == nil || *a.AsOfNet != -3 || a.AsOfSeq == nil || *a.AsOfSeq != 1 || a.AsOfStation != pinStation {
		t.Errorf("fence = {%v, %v, %q}, want {-3, 1, %q}: the addressed station's own cursor",
			a.AsOfNet, a.AsOfSeq, a.AsOfStation, pinStation)
	}
	if net, seq, st := res.FenceFor("stn-other"); net == nil || *net != -4 || seq == nil || *seq != 2 || st != "stn-other" {
		t.Errorf("CountResult fence for stn-other = {%v, %v, %q}, want its own {-4, 2}", net, seq, st)
	}
}

// fencedTo is each UOPAdjustment row for binID, by the station it is addressed
// to (one row each).
func fencedTo(t *testing.T, db *store.DB, binID int64) map[string]protocol.UOPAdjustment {
	t.Helper()
	dsts, adjs := adjustmentDsts(t, db, binID), outboxAdjustments(t, db, binID)
	if len(dsts) != len(adjs) {
		t.Fatalf("%d addressed rows, %d decoded", len(dsts), len(adjs))
	}
	out := map[string]protocol.UOPAdjustment{}
	for i, dst := range dsts {
		if _, dup := out[dst]; dup {
			t.Fatalf("two rows for bin %d to %q", binID, dst)
		}
		out[dst] = adjs[i]
	}
	return out
}

// assertFence fails unless adj is fenced at {net, seq} for station.
func assertFence(t *testing.T, what, station string, adj protocol.UOPAdjustment, net, seq int64) {
	t.Helper()
	if adj.AsOfNet == nil || *adj.AsOfNet != net || adj.AsOfSeq == nil || *adj.AsOfSeq != seq || adj.AsOfStation != station {
		show := func(p *int64) string {
			if p == nil {
				return "nil"
			}
			return strconv.FormatInt(*p, 10)
		}
		t.Errorf("%s to %s fenced at {%s, %s, %q}, want {%d, %d, %q}: each station is fenced at its own stream",
			what, station, show(adj.AsOfNet), show(adj.AsOfSeq), adj.AsOfStation, net, seq, station)
	}
}

// TestCountsCoreStates_AreFencedPerAddressedStation: on the last arm (a seat
// no station claims, two Edges registered) each count Core states goes to both,
// and each copy is fenced at its own station's stream: a person's count and a
// late draw at that station's cursor in the generation, net 0 at seq 0 for the
// station with none; a reset at net 0, seq 0 of the generation it starts for
// both, whatever they counted in the one it ended. It used to be "exactly one
// cursor, else none": the late draw named the cursor's own station to both,
// so the other declined it, and the count and the reset went out unfenced.
func TestCountsCoreStates_AreFencedPerAddressedStation(t *testing.T) {
	t.Parallel()
	const a, b = "edge-fp-a.line", "edge-fp-b.line"
	rig := func(t *testing.T, label string) (*BinService, *InventoryDeltaService, int64, int64) {
		t.Helper()
		db := testDB(t)
		testdb.SetupStandardData(t, db)
		manifest := announcingService(db)
		deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})
		twoEdges(t, db, a, b)
		seat := unclaimedSeat(t, db, "SEAT-"+label)
		bin := createTestBin(t, db, seat.ID, label, "PART-A", 100)
		epoch := binEpoch(t, db, bin.ID)
		testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(a, netDelta(bin.ID, epoch, 1, -4, -4)), "station a counts")
		testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(a, netDelta(bin.ID, epoch, 2, -2, -6)), "station a counts again")
		return NewBinService(db, manifest), deltas, bin.ID, epoch
	}

	t.Run("a person's count", func(t *testing.T) {
		t.Parallel()
		svc, _, binID, _ := rig(t, "BIN-FP-COUNT")
		fresh, err := svc.db.GetBin(binID)
		testutil.MustNoErr(t, err, "read bin")
		_, err = svc.RecordCount(fresh, 50, "operator-under-test")
		testutil.MustNoErr(t, err, "RecordCount")
		got := fencedTo(t, svc.db, binID)
		if len(got) != 2 {
			t.Fatalf("the count went to %d stations, want both on the last arm", len(got))
		}
		assertFence(t, "the count", a, got[a], -6, 2)
		assertFence(t, "the count", b, got[b], 0, 0)
	})

	t.Run("a reset", func(t *testing.T) {
		t.Parallel()
		svc, _, binID, epoch := rig(t, "BIN-FP-RESET")
		_, err := svc.manifest.ClearForReuse(binID, nil, protocol.DeclaredByLifecycle)
		testutil.MustNoErr(t, err, "the reset")
		got := fencedTo(t, svc.db, binID)
		if len(got) != 2 {
			t.Fatalf("the reset went to %d stations, want both on the last arm", len(got))
		}
		for _, st := range []string{a, b} {
			if got[st].Epoch != epoch+1 {
				t.Errorf("the reset to %s names epoch %d, want the new %d", st, got[st].Epoch, epoch+1)
			}
			assertFence(t, "the reset", st, got[st], 0, 0)
		}
	})

	t.Run("a late draw", func(t *testing.T) {
		t.Parallel()
		svc, _, binID, epoch := rig(t, "BIN-FP-LATE")
		tx, err := svc.db.Begin()
		testutil.MustNoErr(t, err, "begin")
		defer func() { _ = tx.Rollback() }()
		atSeat, err := svc.manifest.AnnounceLateDrawTx(tx, binID, epoch, 90)
		testutil.MustNoErr(t, err, "the late draw's announcement")
		testutil.MustNoErr(t, tx.Commit(), "commit")
		if !atSeat {
			t.Fatal("the late draw found no seat")
		}
		got := fencedTo(t, svc.db, binID)
		if len(got) != 2 {
			t.Fatalf("the late draw went to %d stations, want both on the last arm", len(got))
		}
		assertFence(t, "the late draw", a, got[a], -6, 2)
		assertFence(t, "the late draw", b, got[b], 0, 0)
	})
}
