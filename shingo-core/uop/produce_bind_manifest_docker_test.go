//go:build docker

package uop_test

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/payloads"
	"shingocore/uop"
)

// A produce tick that binds or rebinds a bin's payload leaves payload_code and
// the manifest naming the same thing.
//
// Both produce-tick arms in ApplyBinUOPDelta (the zero-count first-delta bind
// and the rebind-with-inventory) used to write payload_code alone. Every other
// payload_code writer on bins writes the manifest in the same statement, so a
// bound bin read as one payload by its code and as another (or as nothing) by
// its manifest: NULL on a blank carrier, the stale label's parts on a relabel.
// Readers key parts off the manifest (CMS derivation, the inventory page), so
// the split was a silent wrong answer, not a cosmetic one.
//
// THE ORACLE IS THE LOAD PATH, not a literal. A second bin loaded through
// BinManifestService.SetFromTemplate with the same payload is what "the
// manifest for this payload" means everywhere else; the tick-bound bin must
// match it exactly (jsonb equality, so line ORDER is pinned too — the template
// lines are inserted out of alphabetical order on purpose).

// payloadWithTwoLines creates a payload whose code matches none of its lines,
// with two template lines whose insertion order differs from their sort order.
func payloadWithTwoLines(t *testing.T, db *store.DB, code string) {
	t.Helper()
	p := &payloads.Payload{Code: code, UOPCapacity: 500, Description: code + " (test)"}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload "+code)
	for _, part := range []string{code + "-PN-Z", code + "-PN-A"} {
		testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
			PayloadID: p.ID, PartNumber: part, PartsPerCycle: 1,
		}, ""), "create template line "+part)
	}
}

// assertManifestMatchesLoadPath fails unless bin's manifest equals the manifest
// SetFromTemplate writes for payloadCode on a fresh bin.
func assertManifestMatchesLoadPath(t *testing.T, db *store.DB, mani *service.BinManifestService,
	nodeID, binID int64, payloadCode string) {
	t.Helper()
	oracle := createTestBin(t, db, nodeID, fmt.Sprintf("ORACLE-%s-%d", payloadCode, binID), "", 0)
	zero := 0
	_, err := mani.SetFromTemplate(oracle.ID, payloadCode, &zero, protocol.DeclaredByLifecycle)
	testutil.MustNoErr(t, err, "load oracle bin through SetFromTemplate")

	var gotCode, gotManifest, wantManifest string
	var same bool
	testutil.MustNoErr(t, db.QueryRow(`
		SELECT b.payload_code, COALESCE(b.manifest::text, '<NULL>'), o.manifest::text,
		       b.manifest IS NOT DISTINCT FROM o.manifest
		  FROM bins b, bins o WHERE b.id=$1 AND o.id=$2`, binID, oracle.ID).
		Scan(&gotCode, &gotManifest, &wantManifest, &same), "read bound + oracle manifests")
	if gotCode != payloadCode {
		t.Fatalf("payload_code = %q, want %q", gotCode, payloadCode)
	}
	if !same {
		t.Errorf("payload_code says %q but the manifest does not match that payload's:\n got  %s\n want %s "+
			"(the bind wrote payload_code without the manifest)", payloadCode, gotManifest, wantManifest)
	}
}

func TestApplyBinUOPDelta_FirstDeltaBindWritesTheManifest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	mani := service.NewBinManifestService(db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(db, mani, service.EpochAnnounce{})
	payloadWithTwoLines(t, db, "BINDMF")

	// Blank carrier: first tick binds it.
	blank := createTestBin(t, db, sd.StorageNode.ID, "BIN-BIND-MF-BLANK", "", 0)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(blank.ID, "BINDMF", 3, 1, protocol.ReasonProduceTick)), "first tick on a blank carrier")
	assertManifestMatchesLoadPath(t, db, mani, sd.StorageNode.ID, blank.ID, "BINDMF")

	// Stale label at zero: first tick relabels it.
	stale := createTestBin(t, db, sd.StorageNode.ID, "BIN-BIND-MF-STALE", "PART-OLD", 0)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(stale.ID, "BINDMF", 2, 1, protocol.ReasonProduceTick)), "first tick over a stale label")
	assertManifestMatchesLoadPath(t, db, mani, sd.StorageNode.ID, stale.ID, "BINDMF")
}

func TestApplyBinUOPDelta_RebindWithInventoryWritesTheManifest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	mani := service.NewBinManifestService(db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(db, mani, service.EpochAnnounce{})
	payloadWithTwoLines(t, db, "REBINDMF")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-REBIND-MF", "PART-OLD", 480)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation,
		makeBinDelta(bin.ID, "REBINDMF", 3, 1, protocol.ReasonProduceTick)), "produce tick with inventory aboard")
	assertManifestMatchesLoadPath(t, db, mani, sd.StorageNode.ID, bin.ID, "REBINDMF")

	// The rest of the rebind contract rides unchanged: count continues, anomaly flagged.
	var uopLeft int
	var anomaly bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining, anomaly_at IS NOT NULL FROM bins WHERE id=$1`,
		bin.ID).Scan(&uopLeft, &anomaly), "read rebind state")
	if uopLeft != 483 || !anomaly {
		t.Errorf("post-rebind uop=%d anomaly=%v, want 483/true", uopLeft, anomaly)
	}
}
