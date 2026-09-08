//go:build docker

package engine

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// cms_clear_end_to_end_docker_test.go — the unloader's departure, on the wire.
//
// The clear is the one CMS event with no second half: shingo does not know which
// zone the material went to, so it books only the departure. That makes the
// end-to-end assertion load-bearing rather than a formality — the feature would
// look complete with rows in cms_transactions that no subscriber accepts and no
// posting ever carries, which is exactly how a fact ships without its reader.

// clearBoundaryBin sets up a tagged supermarket holding one partially-drawn bin,
// and returns the bin and the slot it stands on. Both clear tests need the same
// fixture and differ only in whether the boundary is tagged.
func clearBoundaryBin(t *testing.T, db *store.DB, code string, ppc int64, uopLeft int) (*bins.Bin, int64) {
	t.Helper()
	pay := &payloads.Payload{Code: code, UOPCapacity: 24}
	testutil.MustNoErr(t, db.CreatePayload(pay), "create payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pay.ID, PartNumber: code, PartsPerCycle: ppc,
	}, ""), "create template line")

	_, slot := tagBoundary(t, db, "E2E-CLEAR-"+code, "AMR_SUPERMARKET_TEST")
	bin := createTestBinAtNode(t, db, pay.Code, slot.ID, "BIN-CLEAR-"+code)
	m := bins.Manifest{Items: []bins.ManifestEntry{{PartNumber: code}}}
	body, err := json.Marshal(m)
	testutil.MustNoErr(t, err, "marshal manifest")
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, string(body), pay.Code, uopLeft), "set manifest")
	return bin, slot.ID
}

// TestCMSClearEndToEnd_TheDepartureReachesTheMiddleware is the pin the feature
// exists for, and it holds no seam still: a real engine, a real database, the
// event bus, the builder, the subscriber's Postable gate, the queue, the
// translator and the client, asserted on the bytes a middleware receives and on
// the cms_postings row behind them.
//
// WITHOUT THIS ASSERTION THE FEATURE SHIPS LOOKING COMPLETE. cms_transactions
// rows would accumulate with source_type 'clear', every unit test would be green,
// and the subscriber would drop every one of them on the floor for not being a
// movement — a plant whose storeroom still climbs forever, with a health page
// reporting nothing wrong.
func TestCMSClearEndToEnd_TheDepartureReachesTheMiddleware(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-CLEAR-1"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	// Ten cycles left of a template that packs 24 per cycle: 240 parts leave.
	bin, nodeID := clearBoundaryBin(t, db, "E2E-CLEAR-PAYLOAD", 24, 10)

	epoch, err := eng.ClearForReuseAndBookDeparture(bin.ID, nodeID, nil)
	if err != nil {
		t.Fatalf("ClearForReuseAndBookDeparture: %v", err)
	}
	if epoch == 0 {
		t.Errorf("epoch = 0 — the clear must still bump the generation it always did")
	}

	// THE BIN IS ACTUALLY CLEARED. The rows and the clear share one transaction,
	// so a test that only checked the rows could not tell a booked departure from
	// a rolled-back one.
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload bin")
	if after.UOPRemaining != 0 || after.PayloadCode != "" {
		t.Errorf("bin after the clear = payload %q uop %d, want empty and 0",
			after.PayloadCode, after.UOPRemaining)
	}

	// The recorded row carries the clear's own source type.
	txns, err := db.ListAllCMSTransactions(50, 0)
	testutil.MustNoErr(t, err, "list cms transactions")
	if len(txns) != 1 {
		t.Fatalf("recorded %d transactions, want 1 (one line, one boundary — a clear has no "+
			"second half)", len(txns))
	}
	if txns[0].SourceType != "clear" {
		t.Errorf("source type = %q, want clear", txns[0].SourceType)
	}
	if txns[0].Delta != -240 {
		t.Errorf("delta = %d, want -240", txns[0].Delta)
	}

	if eng.cmsPoster == nil {
		t.Fatal("no poster was started for a configured cms: block")
	}
	eng.cmsPoster.DrainOnce(t.Context())

	got := awaitRequests(t, stub, 1)
	var rows []map[string]any
	if err := json.Unmarshal(got[0], &rows); err != nil {
		t.Fatalf("body is not a JSON array: %v (%s)", err, got[0])
	}
	if len(rows) != 1 {
		t.Fatalf("body carries %d rows, want 1 — the clear is deliberately one-sided: %s",
			len(rows), got[0])
	}
	r := rows[0]
	if r["TransactionType"] != "D" {
		t.Errorf("TransactionType = %v, want D — material LEFT the storeroom", r["TransactionType"])
	}
	if r["Quantity"] != float64(240) {
		t.Errorf("Quantity = %v, want 240 (10 cycles x 24 per cycle), unsigned: %s",
			r["Quantity"], got[0])
	}
	if r["StockLocation"] != "AMR_SUPERMARKET_TEST" {
		t.Errorf("StockLocation = %v, want the boundary's code", r["StockLocation"])
	}
	// BLANK, AND THAT IS ACCURATE. No robot cleared this bin — a person did.
	if r["Resource"] != "" {
		t.Errorf("Resource = %v, want blank — no AMR moved this material", r["Resource"])
	}
	// EntryNumber is the row id, so it is the id of the row that was recorded.
	if r["EntryNumber"] != float64(txns[0].ID) {
		t.Errorf("EntryNumber = %v, want the cms_transactions row id %d",
			r["EntryNumber"], txns[0].ID)
	}

	// AND IT BECAME A POSTING. This is the assertion that the subscriber's gate
	// accepts the new source type; a type it rejects leaves the rows above intact
	// and this row absent.
	status, txID, lastErr := awaitPostingSettled(t, db)
	if status != "posted" || txID != "MW-CLEAR-1" {
		t.Errorf("posting = %s / %q (%s), want posted with the middleware's id",
			status, txID, lastErr)
	}
	var postings int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM cms_postings`).Scan(&postings),
		"count postings")
	if postings != 1 {
		t.Errorf("cms_postings holds %d rows, want 1 — a clear-sourced transaction has to "+
			"reach a posting, or the feature is rows in a table nobody sends", postings)
	}
}

// TestCMSClearEndToEnd_AFailedBookingRefusesTheClear is the ordering rule, pinned
// at the door rather than at the store primitive underneath it.
//
// THE FAILURE IT REFUSES CANNOT BE REPAIRED AFTERWARDS. The quantity is derived
// from uop_remaining and the manifest, and the clear destroys both — so a bin
// cleared with no row recording what left it leaves nothing to reconstruct the
// departure from. A clear the unloader presses again is recoverable; that is not.
//
// The row write is made to fail with a CHECK constraint on this test's own
// database, which is the only way to reach that branch from the door: every other
// input that could break the write is caught in the build. Verified red against a
// two-transaction implementation (db.CreateCMSTransactions followed by
// binManifest.ClearForReuse) — the bin comes back empty with the ledger silent,
// which is the exact loss.
func TestCMSClearEndToEnd_AFailedBookingRefusesTheClear(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-NEVER"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	bin, nodeID := clearBoundaryBin(t, db, "E2E-ATOMIC-PAYLOAD", 24, 10)

	// Refuse exactly the row this clear will build. The build succeeds and the
	// WRITE fails, which is the seam under test.
	if _, err := db.Exec(`ALTER TABLE cms_transactions
		ADD CONSTRAINT e2e_refuse_clear CHECK (source_type <> 'clear')`); err != nil {
		t.Fatalf("install the refusing constraint: %v", err)
	}

	if _, err := eng.ClearForReuseAndBookDeparture(bin.ID, nodeID, nil); err == nil {
		t.Fatal("the clear SUCCEEDED while its departure row could not be written — the bin " +
			"is empty and nothing records what left it, and the quantity it would have " +
			"been derived from is gone")
	}

	// THE BIN STILL HOLDS ITS CONTENTS. This is the assertion; the error above only
	// says the door reported a problem.
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload bin")
	if after.PayloadCode != "E2E-ATOMIC-PAYLOAD" || after.UOPRemaining != 10 {
		t.Errorf("bin after the refused clear = payload %q uop %d, want E2E-ATOMIC-PAYLOAD "+
			"and 10 — the clear must roll back with the row it could not book",
			after.PayloadCode, after.UOPRemaining)
	}
	if after.Manifest == nil || *after.Manifest == "" {
		t.Error("the manifest was cleared anyway — the quantity's inputs are gone")
	}

	txns, err := db.ListAllCMSTransactions(50, 0)
	testutil.MustNoErr(t, err, "list cms transactions")
	if len(txns) != 0 {
		t.Errorf("the refused write left %d rows: %+v", len(txns), txns)
	}
	if got := stub.received(); len(got) != 0 {
		t.Errorf("a refused clear posted %d bodies: %s", len(got), got)
	}
}

// TestCMSClearEndToEnd_UntaggedClearPostsNothingAndStillClears. An untagged clear
// is invisible to CMS and that is correct — but the door is the unloader's, and it
// has to keep working for every station the plant has not tagged.
func TestCMSClearEndToEnd_UntaggedClearPostsNothingAndStillClears(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-NEVER"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	pay := &payloads.Payload{Code: "E2E-UNTAGGED-CLEAR", UOPCapacity: 24}
	testutil.MustNoErr(t, db.CreatePayload(pay), "create payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pay.ID, PartNumber: pay.Code, PartsPerCycle: 24,
	}, ""), "create template line")

	// A plain node under no tagged ancestor.
	node := &nodes.Node{Name: "E2E-UNTAGGED-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create untagged node")
	bin := createTestBinAtNode(t, db, pay.Code, node.ID, "BIN-UNTAGGED-CLEAR")
	m := bins.Manifest{Items: []bins.ManifestEntry{{PartNumber: pay.Code}}}
	body, err := json.Marshal(m)
	testutil.MustNoErr(t, err, "marshal manifest")
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, string(body), pay.Code, 10), "set manifest")

	if _, err := eng.ClearForReuseAndBookDeparture(bin.ID, node.ID, nil); err != nil {
		t.Fatalf("an untagged clear FAILED: %v — this door is the unloader's and it has to "+
			"work at every station the plant has not tagged", err)
	}

	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reload bin")
	if after.UOPRemaining != 0 || after.PayloadCode != "" {
		t.Errorf("bin after the clear = payload %q uop %d, want empty and 0",
			after.PayloadCode, after.UOPRemaining)
	}

	txns, err := db.ListAllCMSTransactions(50, 0)
	testutil.MustNoErr(t, err, "list cms transactions")
	if len(txns) != 0 {
		t.Errorf("an untagged clear recorded %d transactions, want none: %+v", len(txns), txns)
	}
	if got := stub.received(); len(got) != 0 {
		t.Errorf("an untagged clear sent %d requests to the middleware, want none: %s",
			len(got), got)
	}
}

// TestCMSClearEndToEnd_TheServiceClearStaysSilent pins the SPLIT between the two
// clear doors, so it is deliberate rather than incidental.
//
// BinManifestService.ClearForReuse is what www/bin_actions.go's binClear calls —
// the UI admin door, which an operator reaches for to REPAIR a wrong record.
// Booking a repair as an inventory movement writes fiction into a ledger, so that
// door books nothing, and the obvious "helpful" refactor is to move the booking
// down into the service where both doors would inherit it. This test is what
// stops that being silent.
func TestCMSClearEndToEnd_TheServiceClearStaysSilent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-NEVER"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	// A TAGGED boundary, so the only thing keeping this quiet is which door was
	// used. Under the engine's door this exact fixture posts 240.
	bin, _ := clearBoundaryBin(t, db, "E2E-UIDOOR-PAYLOAD", 24, 10)

	if _, err := eng.BinManifest().ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("service ClearForReuse: %v", err)
	}

	txns, err := db.ListAllCMSTransactions(50, 0)
	testutil.MustNoErr(t, err, "list cms transactions")
	if len(txns) != 0 {
		t.Errorf("the UI's clear door booked %d inventory transactions: %+v — an operator "+
			"repairing a wrong record is not material leaving a storeroom, and this is the "+
			"deferral recorded in NEXT-STEPS-cms.md", len(txns), txns)
	}
	if got := stub.received(); len(got) != 0 {
		t.Errorf("the UI's clear door posted %d bodies to the middleware: %s", len(got), got)
	}
}
