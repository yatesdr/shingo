//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// produce_ingest_s1b_docker_test.go — the release-time produce ingest (S1b).
// The Edge finalizes a departing produce bin at the operator's RELEASE and
// ships its count with no manifest lines and the bin's epoch; Core resolves
// the payload's template, stamps the Edge's count, and applies the ingest only
// to the life of the bin it counted.

// s1bIngestFixture is a produce node with one bin on it, a payload with a
// template line and a capacity the count must not be confused with.
func s1bIngestFixture(t *testing.T) (*Dispatcher, *bins.Bin, *payloads.Payload) {
	t.Helper()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	bp.UOPCapacity = 100
	testutil.MustNoErr(t, db.UpdatePayload(bp), "payload capacity")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: bp.ID, PartNumber: "PART-X-TEMPLATE", PartsPerCycle: 1,
	}, ""), "template line")
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "default bin type")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(bp.ID, []int64{bt.ID}), "payload bin types")
	node := &nodes.Node{Name: "SYN-PRESS-S1B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "SYN-BIN-S1B", NodeID: &node.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	return d, got, bp
}

// No lines: Core resolves the template and stamps the Edge's count, not the
// payload's capacity (lifecycle_service.go's no-lines branch used to).
func TestS1bIngest_NoLinesStampsTheCount(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID: "uuid-s1b-nolines", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 47,
	})
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 47 {
		t.Errorf("uop_remaining = %d, want 47 (the Edge's count, not capacity %d)", got.UOPRemaining, bp.UOPCapacity)
	}
	if !got.ManifestConfirmed {
		t.Error("the produced bin's manifest is not confirmed")
	}
	m, err := got.ParseManifest()
	testutil.MustNoErr(t, err, "parse manifest")
	if len(m.Items) != 1 || m.Items[0].PartNumber != "PART-X-TEMPLATE" {
		t.Errorf("manifest = %+v, want the template's one line", m.Items)
	}
}

// A stale epoch: the bin was emptied or re-bound after the Edge counted it.
// Refused, the bin unchanged, and an audit row names it.
func TestS1bIngest_StaleEpochIsRefusedWithAnAuditRow(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID: "uuid-s1b-stale", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch + 5,
		SourceNode: "SYN-PRESS-S1B", Quantity: 47,
	})
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != bin.UOPRemaining || got.ManifestConfirmed != bin.ManifestConfirmed || got.DeltaEpoch != bin.DeltaEpoch {
		t.Errorf("a stale ingest changed the bin: before uop=%d confirmed=%v epoch=%d, after uop=%d confirmed=%v epoch=%d",
			bin.UOPRemaining, bin.ManifestConfirmed, bin.DeltaEpoch, got.UOPRemaining, got.ManifestConfirmed, got.DeltaEpoch)
	}
	var n int
	testutil.MustNoErr(t, d.db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op=$2`,
		bin.ID, "ingest_stale_epoch_refused").Scan(&n), "audit rows")
	if n != 1 {
		t.Errorf("ingest_stale_epoch_refused rows = %d, want 1", n)
	}
}

// Absent epoch (the manual HTTP door) is today's behaviour: applied.
func TestS1bIngest_AbsentEpochApplies(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	d.HandleOrderIngest(testEnvelope(), &protocol.OrderIngestRequest{
		OrderUUID: "uuid-s1b-absent", PayloadCode: bp.Code, BinID: bin.ID,
		SourceNode: "SYN-PRESS-S1B", Quantity: 12,
	})
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 12 || !got.ManifestConfirmed {
		t.Errorf("absent-epoch ingest: uop=%d confirmed=%v, want 12 true", got.UOPRemaining, got.ManifestConfirmed)
	}
}
