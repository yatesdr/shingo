package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/catalog"
)

// part_create_test.go — a part made new at a station's changeover picker
// (part_create.go): the answer's applier, and the door's yes reaching the
// picker and the mint.

// newPartAnswer is Core's yes to req as Core's door builds it: the part's
// catalog row (id, the part number as name and code, the pack as capacity, a
// one-line manifest at ratio 1) and its one carrier row.
func newPartAnswer(t *testing.T, req protocol.PartCreateRequest, id int64, repeat bool) *protocol.EdgeAnswer {
	t.Helper()
	body, err := json.Marshal(protocol.PartCreateResponse{
		ProcessID: req.ProcessID,
		Payload: protocol.CatalogPayloadInfo{ID: id, Name: req.PartNumber, Code: req.PartNumber,
			UOPCapacity: req.StandardPack, CATID: req.CATID,
			Manifest: []protocol.ManifestItem{{PartNumber: req.PartNumber, PartsPerCycle: 1}}},
		BinTypes: []protocol.PayloadBinTypeInfo{{PayloadCode: req.PartNumber, BinTypeCode: req.BinTypeCode}},
		Repeat:   repeat,
	})
	testutil.MustNoErr(t, err, "encode the answer")
	return &protocol.EdgeAnswer{Kind: protocol.AskPartCreate, Status: protocol.AnswerOK, Body: body}
}

// answerPartAsks is a Core that answers every part.create ask reaching e's
// outbox with answer(req), for the test's life: the ask is taken off the
// outbox and its answer handed back matched to it, as Core's reply is.
func answerPartAsks(t *testing.T, e *Engine, answer func(protocol.PartCreateRequest) *protocol.EdgeAnswer) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if e.PendingAsks() == 0 {
				continue
			}
			msgs, err := e.db.ListPendingOutbox(100)
			if err != nil {
				return // the test's db is closing
			}
			for _, m := range msgs {
				if m.MsgType != protocol.SubjectEdgeAsk {
					continue
				}
				var env protocol.Envelope
				var data protocol.Data
				var ask protocol.EdgeAsk
				var req protocol.PartCreateRequest
				if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &data) != nil ||
					json.Unmarshal(data.Body, &ask) != nil || ask.Kind != protocol.AskPartCreate ||
					json.Unmarshal(ask.Body, &req) != nil {
					continue
				}
				if err := e.db.AckOutbox(m.ID); err != nil {
					t.Errorf("take the ask off the outbox: %v", err)
					continue
				}
				e.HandleEdgeAnswer(answerTo(&env), answer(req))
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// partRowsOn is the process's catalog row for code (nil when none) and the
// number of part-set rows naming it.
func partRowsOn(t *testing.T, db *store.DB, processID int64, code string) (*catalog.CatalogEntry, int) {
	t.Helper()
	var entry *catalog.CatalogEntry
	if got, err := db.GetPayloadCatalogByCode(code); err == nil {
		entry = got
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM process_payloads WHERE process_id = ? AND payload_code = ?`,
		processID, code).Scan(&n), "count the part-set rows")
	return entry, n
}

// carrierRows is how many held carrier rows name code on carrier.
func carrierRows(e *Engine, code, carrier string) int {
	n := 0
	for _, r := range e.PayloadBinTypes() {
		if r.PayloadCode == code && r.BinTypeCode == carrier {
			n++
		}
	}
	return n
}

var synNewPart = protocol.PartCreateRequest{PartNumber: "SYN-NEW-1", StandardPack: 800, BinTypeCode: "SYN-CART"}

// TestApplyPartCreate_BothRowsOrNeither: the catalog row and the part-set row
// commit together. A failing statement on either table leaves neither, and
// the carrier rule takes nothing from a yes that did not land.
func TestApplyPartCreate_BothRowsOrNeither(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"process_payloads", "payload_catalog"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			e := testEngine(t, db)
			pid, _ := seedProcessNode(t, db)
			_, err := db.Exec(`CREATE TRIGGER refuse_new_part BEFORE INSERT ON ` + table +
				` BEGIN SELECT RAISE(ABORT, 'a failing statement'); END`)
			testutil.MustNoErr(t, err, "the failing statement")
			req := synNewPart
			req.ProcessID = pid
			e.HandleEdgeAnswer(nil, newPartAnswer(t, req, 501, false))
			entry, n := partRowsOn(t, db, pid, req.PartNumber)
			if entry != nil || n != 0 {
				t.Errorf("a failing %s insert left catalog row %v and %d part-set rows; want neither", table, entry != nil, n)
			}
			if got := carrierRows(e, req.PartNumber, req.BinTypeCode); got != 0 {
				t.Errorf("a yes that did not land added %d carrier rows", got)
			}
		})
	}
}

// TestApplyPartCreate_Idempotent: the same yes applied twice (a replay, or a
// repeat after the first applied) is one row in each table and one carrier
// row, and the second apply re-writes nothing: the rows' stamps stand.
func TestApplyPartCreate_Idempotent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	pid, _ := seedProcessNode(t, db)
	req := synNewPart
	req.ProcessID = pid
	e.HandleEdgeAnswer(nil, newPartAnswer(t, req, 502, false))
	if entry, n := partRowsOn(t, db, pid, req.PartNumber); entry == nil || n != 1 {
		t.Fatalf("first apply: catalog row %v, %d part-set rows; want one each", entry != nil, n)
	}
	// Stamps no write today could produce: a re-write of either row moves them.
	const stamp = "2000-01-01 00:00:00"
	_, err := db.Exec(`UPDATE payload_catalog SET updated_at = ? WHERE code = ?`, stamp, req.PartNumber)
	testutil.MustNoErr(t, err, "stamp the catalog row")
	_, err = db.Exec(`UPDATE process_payloads SET created_at = ? WHERE payload_code = ?`, stamp, req.PartNumber)
	testutil.MustNoErr(t, err, "stamp the part-set row")

	e.HandleEdgeAnswer(nil, newPartAnswer(t, req, 502, true))
	entry, n := partRowsOn(t, db, pid, req.PartNumber)
	if entry == nil || n != 1 {
		t.Fatalf("second apply: catalog row %v, %d part-set rows; want one each", entry != nil, n)
	}
	var updated, created string
	testutil.MustNoErr(t, db.QueryRow(`SELECT updated_at FROM payload_catalog WHERE code = ?`, req.PartNumber).Scan(&updated), "read")
	testutil.MustNoErr(t, db.QueryRow(`SELECT created_at FROM process_payloads WHERE payload_code = ?`, req.PartNumber).Scan(&created), "read")
	if updated != stamp || created != stamp {
		t.Errorf("the second apply re-wrote a row: catalog updated_at %q, part-set created_at %q; want both %q", updated, created, stamp)
	}
	if got := carrierRows(e, req.PartNumber, req.BinTypeCode); got != 1 {
		t.Errorf("carrier rows = %d after two applies, want 1", got)
	}
}

// TestApplyPartCreate_NoWaiter: an answer for an ask this Edge does not hold
// — sent before a restart, or after its tap stopped waiting — is applied by
// its kind all the same: the part is in the catalog as Core built it, on the
// asking process's part set, and in the carrier rule. A process this Edge no
// longer has takes no part-set row.
func TestApplyPartCreate_NoWaiter(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := seedProcessNode(t, db)
	// A fresh engine over the database: nothing pending, as after a restart.
	e := testEngine(t, db)
	req := synNewPart
	req.ProcessID = pid
	req.CATID = "SYN-CAT-1"
	e.HandleEdgeAnswer(&protocol.Envelope{Type: protocol.TypeData, CorID: "sent-before-a-restart"},
		newPartAnswer(t, req, 503, false))
	entry, n := partRowsOn(t, db, pid, req.PartNumber)
	if entry == nil || n != 1 {
		t.Fatalf("catalog row %v, %d part-set rows; want one each", entry != nil, n)
	}
	if entry.ID != 503 || entry.UOPCapacity != 800 || entry.CATID != "SYN-CAT-1" ||
		len(entry.Manifest) != 1 || entry.Manifest[0].PartNumber != req.PartNumber || entry.Manifest[0].PartsPerCycle != 1 {
		t.Errorf("catalog row = %+v, want Core's row as answered", entry)
	}
	if got := e.BinTypeForPayload(req.PartNumber); got != req.BinTypeCode {
		t.Errorf("carrier rule for the part = %q, want %q", got, req.BinTypeCode)
	}

	gone := req
	gone.ProcessID, gone.PartNumber = pid+1000, "SYN-NEW-GONE"
	e.HandleEdgeAnswer(nil, newPartAnswer(t, gone, 504, false))
	if entry, n := partRowsOn(t, db, gone.ProcessID, gone.PartNumber); entry == nil || n != 0 {
		t.Errorf("a process this Edge does not have: catalog row %v, %d part-set rows; want the row and none", entry != nil, n)
	}
}

// TestApplyPartCreate_ARefusalAppliesNothing: Core's no writes nothing here,
// whatever its body carries: the status decides, not the bytes.
func TestApplyPartCreate_ARefusalAppliesNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	pid, _ := seedProcessNode(t, db)
	req := synNewPart
	req.ProcessID = pid
	refused := newPartAnswer(t, req, 509, false)
	refused.Status, refused.Sentence = protocol.AnswerRefused, "Core already has SYN-NEW-1."
	e.HandleEdgeAnswer(nil, refused)
	if entry, n := partRowsOn(t, db, pid, synNewPart.PartNumber); entry != nil || n != 0 {
		t.Errorf("a refusal wrote catalog row %v and %d part-set rows", entry != nil, n)
	}
	if len(e.PayloadBinTypes()) != 0 {
		t.Errorf("a refusal added carrier rows: %v", e.PayloadBinTypes())
	}
}

// TestApplyPartCreate_LeavesTheHeldDigests: the applier holds no feed digest.
// The catalog and node digests the Edge quotes stay the ones that arrived
// with the feeds, and a carrier rule never heard from Core stays "not heard"
// (the press-index changeover's gate reads it).
func TestApplyPartCreate_LeavesTheHeldDigests(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	pid, _ := seedProcessNode(t, db)
	req := synNewPart
	req.ProcessID = pid

	e.HandleEdgeAnswer(nil, newPartAnswer(t, req, 505, false))
	if e.payloadBinTypesKnown() {
		t.Error("a new part's carrier row made Core's carrier rule read as heard")
	}
	if c, n := e.heldDigest(protocol.FeedCatalog), e.heldDigest(protocol.FeedNodes); c != "" || n != "" {
		t.Errorf("before any feed the applier held digests catalog %q, nodes %q", c, n)
	}

	e.ApplyCatalog(&protocol.CatalogPayloadsResponse{Digest: "SYN-D0",
		Payloads: []protocol.CatalogPayloadInfo{{ID: 505, Name: req.PartNumber, Code: req.PartNumber, UOPCapacity: 800}}})
	e.ApplyNodeList(&protocol.NodeListResponse{Digest: "SYN-D0N", Family: map[string]string{"SYN-CART": "SYN-CART"}})
	if c, n := e.heldDigest(protocol.FeedCatalog), e.heldDigest(protocol.FeedNodes); c != "SYN-D0" || n != "SYN-D0N" {
		t.Fatalf("the feeds held catalog %q, nodes %q; want SYN-D0, SYN-D0N", c, n)
	}
	second := req
	second.PartNumber = "SYN-NEW-2"
	e.HandleEdgeAnswer(nil, newPartAnswer(t, second, 506, false))
	if entry, n := partRowsOn(t, db, pid, second.PartNumber); entry == nil || n != 1 {
		t.Fatalf("the second part did not apply: catalog row %v, %d part-set rows", entry != nil, n)
	}
	if c, n := e.heldDigest(protocol.FeedCatalog), e.heldDigest(protocol.FeedNodes); c != "SYN-D0" || n != "SYN-D0N" {
		t.Errorf("after the applier: catalog %q, nodes %q; want the feeds' SYN-D0, SYN-D0N", c, n)
	}
}

// TestCreatePart_ARepeatLandsThePartOnThePicker: Core made the part and its
// first answer never applied here (lost, or an Edge that restarted before it
// arrived). The operator submits again; Core answers the first part as a
// repeat, and the station's part list has it, with its name.
func TestCreatePart_ARepeatLandsThePartOnThePicker(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	station := anyStationOf(t, f)
	f.eng.setAskKinds([]string{protocol.AskPartCreate})
	f.eng.setBinTypeFamilies(map[string]string{"SYN-CART": "SYN-CART"})
	setAskClock(f.eng, waitRunOut{})
	if _, err := f.eng.CreatePart(f.plant.ProcessID, synNewPart); !errors.Is(err, ErrAskPending) {
		t.Fatalf("first submit = %v, want it sent and unanswered", err)
	}
	if rows := partRowsOf(t, f.eng, station); rows[synNewPart.PartNumber] {
		t.Fatal("the part is on the picker before any answer applied")
	}
	// The first ask is dropped: its answer never comes.
	_, _ = askSent(t, f.eng)
	setAskClock(f.eng, nil)
	answerPartAsks(t, f.eng, func(req protocol.PartCreateRequest) *protocol.EdgeAnswer {
		return newPartAnswer(t, req, 507, true)
	})
	res, err := f.eng.CreatePart(f.plant.ProcessID, synNewPart)
	testutil.MustNoErr(t, err, "the repeat")
	if !res.Repeat || res.Payload.Code != synNewPart.PartNumber {
		t.Fatalf("answer = %+v, want Core's first part as a repeat", res)
	}
	if rows := partRowsOf(t, f.eng, station); !rows[synNewPart.PartNumber] {
		t.Errorf("the station's part rows %v do not list %s", rows, synNewPart.PartNumber)
	}
}

// TestCreatePart_TheNewPartMintsAtOnce: the picker needs nothing more than the
// applier wrote. Straight after Core's yes the station's part rows list the
// part by its catalog name, and the mint takes it onto a style with another of
// the cell's parts.
func TestCreatePart_TheNewPartMintsAtOnce(t *testing.T) {
	t.Parallel()
	f := newMintFixture(t)
	station := anyStationOf(t, f)
	f.eng.setAskKinds([]string{protocol.AskPartCreate})
	f.eng.setBinTypeFamilies(map[string]string{"SYN-CART": "SYN-CART"})
	answerPartAsks(t, f.eng, func(req protocol.PartCreateRequest) *protocol.EdgeAnswer {
		return newPartAnswer(t, req, 508, false)
	})
	if _, err := f.eng.CreatePart(f.plant.ProcessID, synNewPart); err != nil {
		t.Fatalf("CreatePart = %v", err)
	}
	if rows := partRowsOf(t, f.eng, station); !rows[synNewPart.PartNumber] {
		t.Fatalf("the station's part rows %v do not list %s", rows, synNewPart.PartNumber)
	}
	other := f.partsByNode(t, f.members[0])
	var running string
	for _, code := range other {
		running = code
		break
	}
	res, err := f.mint(t, PartPick{Code: synNewPart.PartNumber}, PartPick{Code: running})
	testutil.MustNoErr(t, err, "mint the new part")
	if !res.Minted {
		t.Fatalf("mint = %+v, want a new style", res)
	}
	placed := false
	for _, code := range f.partsByNode(t, res.StyleID) {
		placed = placed || code == synNewPart.PartNumber
	}
	if !placed {
		t.Errorf("the minted style's claims %v do not carry %s", f.partsByNode(t, res.StyleID), synNewPart.PartNumber)
	}
}

// anyStationOf is one of the mint fixture's stations on its process.
func anyStationOf(t *testing.T, f mintFixture) int64 {
	t.Helper()
	for _, id := range f.plant.Stations {
		st, err := f.db.GetOperatorStation(id)
		if err == nil && st.ProcessID == f.plant.ProcessID {
			return id
		}
	}
	t.Fatal("the plant has no station on its process")
	return 0
}

// partRowsOf is the station composer read's part rows by code: true where the
// row carries the catalog's name, false where it lists the bare code.
func partRowsOf(t *testing.T, e *Engine, stationID int64) map[string]bool {
	t.Helper()
	read, err := e.stationService.ComposerForStation(stationID)
	testutil.MustNoErr(t, err, "the station's composer read")
	out := map[string]bool{}
	for _, r := range read.PartRows {
		out[r.Code] = r.Name != ""
	}
	return out
}
