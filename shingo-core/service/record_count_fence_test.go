//go:build docker

package service

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
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
	sd := testdb.SetupStandardData(t, db)
	manifest := announcingService(db)
	bins := NewBinService(db, manifest)
	deltas := NewInventoryDeltaService(db, manifest, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})
	bin := createTestBin(t, db, sd.StorageNode.ID, label, "PART-A", 100)
	return bins, deltas, bin.ID, binEpoch(t, db, bin.ID), sd.StorageNode.Name
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
	if res.AsOfNet == nil || *res.AsOfNet != -10 || res.AsOfSeq == nil || *res.AsOfSeq != 1 ||
		res.AsOfStation != pinStation || res.Epoch != epoch {
		t.Errorf("CountResult fence = {%v, %v, %q, epoch %d}, want the same fence as the message",
			res.AsOfNet, res.AsOfSeq, res.AsOfStation, res.Epoch)
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

func TestRecordCount_NoCursorMeansNoFence(t *testing.T) {
	t.Parallel()
	svc, _, binID, _, _ := countWithFenceFixture(t, "BIN-FENCE-2")
	fresh, err := svc.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	_, err = svc.RecordCount(fresh, 7, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")
	adjs := outboxAdjustments(t, svc.db, binID)
	if len(adjs) != 1 {
		t.Fatalf("enqueued %d UOPAdjustments, want 1", len(adjs))
	}
	if adjs[0].AsOfNet != nil || adjs[0].AsOfSeq != nil || adjs[0].AsOfStation != "" {
		t.Errorf("fence = {%v, %v, %q}, want nil/nil/\"\": no station has counted this carrier "+
			"in this generation", adjs[0].AsOfNet, adjs[0].AsOfSeq, adjs[0].AsOfStation)
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

func TestRecordCount_TwoCountingStationsMeansNoFence(t *testing.T) {
	t.Parallel()
	svc, deltas, binID, epoch, _ := countWithFenceFixture(t, "BIN-FENCE-4")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta(pinStation, netDelta(binID, epoch, 1, -3, -3)), "station 1")
	testutil.MustNoErr(t, deltas.ApplyBinUOPDelta("stn-other", netDelta(binID, epoch, 1, -2, -2)), "station 2")
	fresh, err := svc.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	_, err = svc.RecordCount(fresh, 7, "operator-under-test")
	testutil.MustNoErr(t, err, "RecordCount")
	adjs := outboxAdjustments(t, svc.db, binID)
	if len(adjs) != 1 || adjs[0].AsOfNet != nil || adjs[0].AsOfStation != "" {
		t.Errorf("adjustments = %+v, want one with no fence: two stations counted this carrier, "+
			"and one net cannot fence two streams", adjs)
	}
}
