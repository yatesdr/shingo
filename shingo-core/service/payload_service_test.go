//go:build docker

package service

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// makePayload inserts a payload directly through the service and returns it.
func makePayload(t *testing.T, svc *PayloadService, code, desc string, uop int) *payloads.Payload {
	t.Helper()
	p := &payloads.Payload{Code: code, Description: desc, UOPCapacity: uop}
	if err := svc.Create(p); err != nil {
		t.Fatalf("Create payload %s: %v", code, err)
	}
	return p
}

func TestPayloadService_Create_PersistsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	p := &payloads.Payload{Code: "PL-CR-1", Description: "first", UOPCapacity: 50}
	testutil.MustNoErr(t, svc.Create(p), "Create")
	if p.ID == 0 {
		t.Fatal("expected ID populated")
	}

	got, err := db.GetPayload(p.ID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if got.Code != "PL-CR-1" || got.UOPCapacity != 50 {
		t.Errorf("row = %+v, want PL-CR-1/50", got)
	}
}

func TestPayloadService_GetByCode_ReturnsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-GBC", "by code", 10)

	got, err := svc.GetByCode("PL-GBC")
	if err != nil {
		t.Fatalf("GetByCode: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("GetByCode ID = %d, want %d", got.ID, p.ID)
	}
}

func TestPayloadService_Update_PersistsChanges(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-UPD", "old desc", 10)

	p.Description = "new desc"
	p.UOPCapacity = 25
	testutil.MustNoErr(t, svc.Update(p), "Update")
	got, _ := db.GetPayload(p.ID)
	if got.Description != "new desc" || got.UOPCapacity != 25 {
		t.Errorf("row = %+v, want new desc/25", got)
	}
}

func TestPayloadService_Delete_RemovesRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-DEL", "", 1)

	testutil.MustNoErr(t, svc.Delete(p.ID), "Delete")
	if _, err := db.GetPayload(p.ID); err == nil {
		t.Error("GetPayload after Delete should error")
	}
}

func TestPayloadService_List_ReturnsAll(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	makePayload(t, svc, "PL-L-A", "", 1)
	makePayload(t, svc, "PL-L-B", "", 1)
	makePayload(t, svc, "PL-L-C", "", 1)

	rows, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) < 3 {
		t.Errorf("len(rows) = %d, want >= 3", len(rows))
	}

	// Sanity check against direct db.
	dbRows, _ := db.ListPayloads()
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}

func TestPayloadService_CreateAndListManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-MAN", "manifest", 10)

	item := &payloads.ManifestItem{
		PayloadID:     p.ID,
		PartNumber:    "PART-1",
		PartsPerCycle: 3,
	}
	testutil.MustNoErr(t, svc.CreateManifestItem(item, "", false), "CreateManifestItem")
	if item.ID == 0 {
		t.Fatal("expected manifest item ID populated")
	}

	items, err := svc.ListManifest(p.ID)
	if err != nil {
		t.Fatalf("ListManifest: %v", err)
	}
	if len(items) != 1 || items[0].PartNumber != "PART-1" || items[0].PartsPerCycle != 3 {
		t.Errorf("items = %+v, want one PART-1/3", items)
	}
}

func TestPayloadService_UpdateManifestItem_PersistsChanges(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-UMI", "", 10)

	item := &payloads.ManifestItem{PayloadID: p.ID, PartNumber: "P-OLD", PartsPerCycle: 1}
	testutil.MustNoErr(t, svc.CreateManifestItem(item, "", false), "CreateManifestItem")

	testutil.MustNoErr(t, svc.UpdateManifestItem(item.ID, "P-NEW", "", 9, false), "UpdateManifestItem")
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 1 || rows[0].PartNumber != "P-NEW" || rows[0].PartsPerCycle != 9 {
		t.Errorf("rows = %+v, want one P-NEW/9", rows)
	}
}

func TestPayloadService_DeleteManifestItem_RemovesRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-DMI", "", 10)

	item := &payloads.ManifestItem{PayloadID: p.ID, PartNumber: "P", PartsPerCycle: 1}
	testutil.MustNoErr(t, svc.CreateManifestItem(item, "", false), "CreateManifestItem")
	testutil.MustNoErr(t, svc.DeleteManifestItem(item.ID, false), "DeleteManifestItem")
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want 0 after delete", rows)
	}
}

func TestPayloadService_ReplaceManifest_SwapsList(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-RPL", "", 10)

	// Seed with one item, then replace with two.
	testutil.MustNoErr(t, svc.CreateManifestItem(&payloads.ManifestItem{PayloadID: p.ID, PartNumber: "OLD", PartsPerCycle: 1}, "", false), "seed CreateManifestItem")

	newLines := []payloads.Line{
		{PartNumber: "N1", PartsPerCycle: 4},
		{PartNumber: "N2", PartsPerCycle: 5},
	}
	testutil.MustNoErr(t, svc.ReplaceManifest(p.ID, newLines), "ReplaceManifest")
	rows, _ := db.ListPayloadManifest(p.ID)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	parts := map[string]int64{rows[0].PartNumber: rows[0].PartsPerCycle, rows[1].PartNumber: rows[1].PartsPerCycle}
	if parts["N1"] != 4 || parts["N2"] != 5 {
		t.Errorf("parts = %+v, want N1:4 N2:5", parts)
	}
}

func TestPayloadService_SetAndListBinTypes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-BTY", "", 10)

	bt1 := &bins.BinType{Code: "BT-A", Description: "a"}
	bt2 := &bins.BinType{Code: "BT-B", Description: "b"}
	testutil.MustNoErr(t, db.CreateBinType(bt1), "create bt1")
	testutil.MustNoErr(t, db.CreateBinType(bt2), "create bt2")

	testutil.MustNoErr(t, svc.SetBinTypes(p.ID, []int64{bt1.ID, bt2.ID}), "SetBinTypes")
	got, err := svc.ListBinTypes(p.ID)
	if err != nil {
		t.Fatalf("ListBinTypes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}

	// Replacing with empty list clears.
	testutil.MustNoErr(t, svc.SetBinTypes(p.ID, nil), "SetBinTypes(nil)")
	got, _ = svc.ListBinTypes(p.ID)
	if len(got) != 0 {
		t.Errorf("after clear len(got) = %d, want 0", len(got))
	}
}

func TestPayloadService_CompatibleNodeNamesByPayload_EmptyByDefault(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	p := makePayload(t, svc, "PL-CNODES", "", 10)

	byPayload, err := svc.CompatibleNodeNamesByPayload()
	if err != nil {
		t.Fatalf("CompatibleNodeNamesByPayload: %v", err)
	}
	// Sanity: matches the per-payload store read.
	dbRows, err := db.ListNodesForPayload(p.ID)
	if err != nil {
		t.Fatalf("ListNodesForPayload: %v", err)
	}
	if len(dbRows) != len(byPayload[p.ID]) {
		t.Errorf("db rows = %d, svc names = %d, should match", len(dbRows), len(byPayload[p.ID]))
	}
}

// TestPayloadService_PayloadsChangedHook: every payload write the service
// makes calls the payloads hook once, after it succeeded, so the feeds drop
// their memos for a desktop edit as for a floor create; a refused write calls
// nothing.
func TestPayloadService_PayloadsChangedHook(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)
	calls := 0
	svc.SetPayloadsChangedFunc(func() { calls++ })

	bt := &bins.BinType{Code: "SYN-HOOK-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	bare, err := db.EnsureBareMarker(bt.ID)
	testutil.MustNoErr(t, err, "bare marker")
	p := &payloads.Payload{Code: "SYN-HOOK-1", UOPCapacity: 5}
	item := &payloads.ManifestItem{PartNumber: "SYN-HOOK-PART", PartsPerCycle: 1}

	for _, w := range []struct {
		name string
		do   func() error
	}{
		{"create (the importer's)", func() error { return svc.Create(&payloads.Payload{Code: "SYN-HOOK-0", UOPCapacity: 5}) }},
		{"create with sets", func() error { return svc.CreateWithSets(p, PayloadSets{}) }},
		{"update", func() error { p.Description = "edited"; return svc.Update(p) }},
		{"update with sets", func() error {
			return svc.UpdateWithSets(p, PayloadSets{BinTypeIDs: &[]int64{bt.ID}})
		}},
		{"set bin types", func() error { return svc.SetBinTypes(p.ID, nil) }},
		{"create manifest item", func() error { item.PayloadID = p.ID; return svc.CreateManifestItem(item, "", false) }},
		{"update manifest item", func() error { return svc.UpdateManifestItem(item.ID, "SYN-HOOK-PART", "", 2, false) }},
		{"delete manifest item", func() error { return svc.DeleteManifestItem(item.ID, false) }},
		{"replace manifest", func() error {
			return svc.ReplaceManifest(p.ID, []payloads.Line{{PartNumber: "SYN-HOOK-PART", PartsPerCycle: 1}})
		}},
		{"set a part's cat id", func() error { return svc.SetPartCATID("SYN-HOOK-PART", "SYN-CAT-1") }},
		{"delete", func() error { return svc.Delete(p.ID) }},
	} {
		before := calls
		testutil.MustNoErr(t, w.do(), w.name)
		if calls != before+1 {
			t.Errorf("%s called the hook %d times, want 1", w.name, calls-before)
		}
	}

	q := makePayload(t, svc, "SYN-HOOK-2", "", 5)
	before := calls
	if err := svc.CreateWithSets(&payloads.Payload{Code: "  ", UOPCapacity: 5}, PayloadSets{}); err == nil {
		t.Error("a blank code was created")
	}
	if err := svc.SetBinTypes(q.ID, []int64{bare}); err == nil {
		t.Error("a bare carrier type was set on a payload")
	}
	if calls != before {
		t.Errorf("refused writes called the hook %d times, want none", calls-before)
	}
}

// floorFix is a floor create's world: one carrier type and its bare marker's
// code, and the submit every test varies.
func floorFix(t *testing.T) (*store.DB, *PayloadService, protocol.PartCreateRequest, int64, string) {
	t.Helper()
	db := testDB(t)
	bt := &bins.BinType{Code: "SYN-FL-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create carrier type")
	bare, err := db.EnsureBareMarker(bt.ID)
	testutil.MustNoErr(t, err, "bare marker")
	var bareCode string
	testutil.MustNoErr(t, db.QueryRow(`SELECT code FROM bin_types WHERE id=$1`, bare).Scan(&bareCode), "bare code")
	return db, NewPayloadService(db), protocol.PartCreateRequest{ProcessID: 17, PartNumber: "SYN-FL-1",
		StandardPack: 800, BinTypeCode: "SYN-FL-BT", CATID: "SYN-C1"}, bt.ID, bareCode
}

// rowCounts is how many payloads and audit rows the database holds.
func rowCounts(t *testing.T, db *store.DB) [2]int {
	t.Helper()
	var n [2]int
	testutil.MustNoErr(t, db.QueryRow(`SELECT (SELECT COUNT(*) FROM payloads), (SELECT COUNT(*) FROM audit_log)`).
		Scan(&n[0], &n[1]), "count rows")
	return n
}

// TestPayloadService_FloorCreate_OneCommit: a part made at the floor is the
// payload, its one carrier type, its one manifest line (the part at ratio 1,
// its CATID on the part) and an audit_log row holding the station, the process
// and the fields as Core read them, all from one transaction: with the audit
// table gone, nothing is written. The row is read back by the repeat check: the
// same station and process with the same fields (the number padded, which
// Core trims) get the stored part, marked a repeat, and nothing is written.
func TestPayloadService_FloorCreate_OneCommit(t *testing.T) {
	t.Parallel()
	db, svc, req, btID, _ := floorFix(t)

	p, repeat, err := svc.CreateFromFloor("edge.a", req)
	testutil.MustNoErr(t, err, "floor create")
	if repeat || p.ID == 0 || p.Code != "SYN-FL-1" || p.UOPCapacity != 800 {
		t.Fatalf("created %+v repeat=%v, want SYN-FL-1 at 800, not a repeat", p, repeat)
	}
	bts, err := db.ListBinTypesForPayload(p.ID)
	testutil.MustNoErr(t, err, "carrier types")
	if len(bts) != 1 || bts[0].ID != btID {
		t.Errorf("carrier types = %+v, want SYN-FL-BT alone", bts)
	}
	lines, err := db.ListPayloadManifest(p.ID)
	testutil.MustNoErr(t, err, "manifest")
	if len(lines) != 1 || lines[0].PartNumber != "SYN-FL-1" || lines[0].PartsPerCycle != 1 || lines[0].CATID != "SYN-C1" {
		t.Errorf("manifest = %+v, want one line SYN-FL-1 at ratio 1 with CATID SYN-C1", lines)
	}
	entries, err := db.ListEntityAudit(store.PayloadAuditEntity, p.ID)
	testutil.MustNoErr(t, err, "audit")
	if len(entries) != 1 || entries[0].Action != FloorCreateAction || entries[0].Actor != "edge.a" {
		t.Fatalf("audit rows = %+v, want one %s by edge.a", entries, FloorCreateAction)
	}
	var made map[string]any
	testutil.MustNoErr(t, json.Unmarshal([]byte(entries[0].NewValue), &made), "decode the record")
	want := map[string]any{"station_uid": "edge.a", "process_id": float64(17), "part_number": "SYN-FL-1",
		"standard_pack": float64(800), "bin_type_code": "SYN-FL-BT", "catid": "SYN-C1"}
	if !reflect.DeepEqual(made, want) {
		t.Errorf("recorded submit = %v, want %v", made, want)
	}

	before := rowCounts(t, db)
	again := req
	again.PartNumber = "  SYN-FL-1 "
	got, repeat, err := svc.CreateFromFloor("edge.a", again)
	testutil.MustNoErr(t, err, "repeat")
	if !repeat || got.ID != p.ID {
		t.Errorf("the same submit again = %+v repeat=%v, want part %d marked a repeat", got, repeat, p.ID)
	}
	if after := rowCounts(t, db); after != before {
		t.Errorf("a repeat wrote rows: %v -> %v", before, after)
	}

	_, err = db.Exec(`ALTER TABLE audit_log RENAME TO audit_log_hidden`)
	testutil.MustNoErr(t, err, "hide audit_log")
	second := req
	second.PartNumber, second.CATID = "SYN-FL-NOAUDIT", ""
	if _, _, err := svc.CreateFromFloor("edge.a", second); err == nil {
		t.Error("a floor create with no audit table succeeded")
	}
	if p, err := db.GetPayloadByCode("SYN-FL-NOAUDIT"); err == nil {
		t.Errorf("a floor create whose audit row failed left its payload: %+v", p)
	}
}

// TestPayloadService_FloorCreate_Refusals: everything a desktop create
// refuses, and the floor's own: a number Core has (made on the desktop, or
// made at the floor by another station, for another process or with other
// fields), the number spelled another way, a CATID another part carries or
// that contradicts the part's known one, a carrier type Core does not have or
// a bare one. Each writes nothing.
func TestPayloadService_FloorCreate_Refusals(t *testing.T) {
	t.Parallel()
	db, svc, req, _, bareCode := floorFix(t)
	desk := &payloads.Payload{Code: "SYN-FL-DESK", UOPCapacity: 5}
	testutil.MustNoErr(t, svc.CreateWithSets(desk, PayloadSets{
		Manifest: &[]ManifestLine{{PartNumber: "SYN-FL-DESK", CATID: "SYN-C9", PartsPerCycle: 1}}}), "desktop part")
	testutil.MustNoErr(t, svc.CreateManifestItem(&payloads.ManifestItem{PayloadID: desk.ID,
		PartNumber: "SYN-FL-LOOSE", PartsPerCycle: 1}, "SYN-C7", false), "a part with a CATID and no payload of its own")
	_, _, err := svc.CreateFromFloor("edge.a", req)
	testutil.MustNoErr(t, err, "the floor part")

	with := func(f func(r *protocol.PartCreateRequest)) protocol.PartCreateRequest {
		r := req
		f(&r)
		return r
	}
	zero, three := 0, 3
	isExists := func(code string, near bool) func(error) bool {
		return func(err error) bool {
			var e *PartExistsError
			return errors.As(err, &e) && e.Code == code && e.Near == near
		}
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	for _, c := range []struct {
		name    string
		station string
		req     protocol.PartCreateRequest
		want    func(error) bool
	}{
		{"a blank number", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber = "" }), is(ErrPayloadCode)},
		{"a number of spaces", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber = "  " }), is(ErrPayloadCode)},
		{"a pack of 0", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.StandardPack = "SYN-FL-N1", 0 }), is(ErrPayloadCapacity)},
		{"tote layers of 0", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.ToteLayers = "SYN-FL-N2", &zero }), is(ErrPayloadToteNumber)},
		{"an unknown carrier", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.BinTypeCode = "SYN-FL-N3", "SYN-NO-BT" }), is(ErrUnknownCarrierType)},
		{"a bare carrier", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.BinTypeCode = "SYN-FL-N4", bareCode }), is(ErrBareTypeInPayloadRule)},
		{"a desktop part", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.CATID = "SYN-FL-DESK", "" }), isExists("SYN-FL-DESK", false)},
		{"the floor part from another station", "edge.b", req, isExists("SYN-FL-1", false)},
		{"the floor part for another process", "edge.a", with(func(r *protocol.PartCreateRequest) { r.ProcessID = 18 }), isExists("SYN-FL-1", false)},
		{"the floor part with another pack", "edge.a", with(func(r *protocol.PartCreateRequest) { r.StandardPack = 600 }), isExists("SYN-FL-1", false)},
		{"the floor part with tote layers", "edge.a", with(func(r *protocol.PartCreateRequest) { r.ToteLayers = &three }), isExists("SYN-FL-1", false)},
		{"a near spelling", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.CATID = "syn fl_desk", "" }), isExists("SYN-FL-DESK", true)},
		{"a CATID another part carries", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.CATID = "SYN-FL-N5", "SYN-C9" }),
			func(err error) bool {
				var e *CATIDTakenError
				return errors.As(err, &e) && e.PartNumber == "SYN-FL-DESK"
			}},
		{"a CATID that contradicts the part's own", "edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.CATID = "SYN-FL-LOOSE", "SYN-C8" }),
			func(err error) bool {
				var e *CATIDConflict
				return errors.As(err, &e) && e.Existing == "SYN-C7"
			}},
	} {
		before := rowCounts(t, db)
		p, repeat, err := svc.CreateFromFloor(c.station, c.req)
		if !c.want(err) {
			t.Errorf("%s: %+v repeat=%v err=%v, not the refusal wanted", c.name, p, repeat, err)
		}
		if after := rowCounts(t, db); after != before {
			t.Errorf("%s wrote rows: %v -> %v", c.name, before, after)
		}
	}

	// A known part with no payload of its own, and no CATID given, keeps its
	// own: the floor part is made over it, its CATID SYN-C7.
	p, _, err := svc.CreateFromFloor("edge.a", with(func(r *protocol.PartCreateRequest) { r.PartNumber, r.CATID = "SYN-FL-LOOSE", "" }))
	testutil.MustNoErr(t, err, "a known part with no CATID given")
	if part, err := db.GetPartByNumber(p.Code); err != nil || part.CATID != "SYN-C7" {
		t.Errorf("the known part's CATID after the floor create = %+v (%v), want SYN-C7 kept", part, err)
	}
}

// TestPayloadService_FloorCreate_ConcurrentSubmitIsARepeat: a submit whose
// number another submit commits between the exact-number read and the insert
// meets the unique constraint, goes back to the exact-number step, and is
// answered as that step answers: the same station's same submit gets the part,
// marked a repeat; another station's is refused. The other submit is a
// transaction this test holds open until the floor create waits on it.
func TestPayloadService_FloorCreate_ConcurrentSubmitIsARepeat(t *testing.T) {
	t.Parallel()
	db, svc, req, btID, _ := floorFix(t)
	for _, c := range []struct {
		station, code string
		repeat        bool
	}{{"edge.a", "SYN-FL-RACE-1", true}, {"edge.b", "SYN-FL-RACE-2", false}} {
		r := req
		r.PartNumber, r.CATID = c.code, ""
		record, err := json.Marshal(floorSubmit{StationUID: "edge.a", PartCreateRequest: protocol.PartCreateRequest{
			ProcessID: r.ProcessID, PartNumber: c.code, StandardPack: r.StandardPack, BinTypeCode: r.BinTypeCode}})
		testutil.MustNoErr(t, err, "record")
		tx, err := db.Begin()
		testutil.MustNoErr(t, err, "begin the other submit")
		var id int64
		testutil.MustNoErr(t, tx.QueryRow(`INSERT INTO payloads (code, uop_capacity) VALUES ($1, 800) RETURNING id`, c.code).Scan(&id), "insert")
		_, err = tx.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1, $2)`, id, btID)
		testutil.MustNoErr(t, err, "carrier")
		_, err = tx.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, new_value, actor) VALUES ($1, $2, $3, $4, $5)`,
			store.PayloadAuditEntity, id, FloorCreateAction, string(record), "edge.a")
		testutil.MustNoErr(t, err, "audit")

		type result struct {
			p      *payloads.Payload
			repeat bool
			err    error
		}
		done := make(chan result, 1)
		go func() {
			p, repeat, err := svc.CreateFromFloor(c.station, r)
			done <- result{p, repeat, err}
		}()
		deadline := time.Now().Add(15 * time.Second)
		for waiting := 0; waiting == 0; {
			if time.Now().After(deadline) {
				_ = tx.Rollback()
				t.Fatalf("%s: the floor create never waited on the other submit's insert", c.code)
			}
			// pg_locks is the server's; only this test's database counts.
			testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE NOT l.granted AND a.datname = current_database()`).Scan(&waiting), "locks")
		}
		testutil.MustNoErr(t, tx.Commit(), "commit the other submit")
		got := <-done
		var exists *PartExistsError
		switch {
		case c.repeat && (got.err != nil || !got.repeat || got.p.ID != id):
			t.Errorf("%s from the same station: %+v repeat=%v err=%v, want part %d marked a repeat", c.code, got.p, got.repeat, got.err, id)
		case !c.repeat && !errors.As(got.err, &exists):
			t.Errorf("%s from another station: %+v err=%v, want PartExistsError", c.code, got.p, got.err)
		}
	}
}

// TestPayloadService_FloorCreate_PinRepeatIsTheAskingStations pins what
// floorRepeat returns on a retried floor create, and the station the payloads
// page names as the part's maker (FloorMade), both read back from the audit
// row the create wrote (its {"station_uid": ...} record). The station is the
// asking Edge's (www Doors.partCreate passes the ask's station): a retry from
// it is a repeat of the same part and writes nothing, before and after
// another station's identical submit is refused. Floor creates come only from
// an Edge's ask, never from an order, so the five order kinds of the actor
// split do not apply: no Core actor and no Core-born path makes a floor part.
//
// Red when a retry from the maker is refused (the repeat check reading the
// station from a key the record no longer holds), when another station's
// submit counts as a repeat, or when the page names another maker.
func TestPayloadService_FloorCreate_PinRepeatIsTheAskingStations(t *testing.T) {
	t.Parallel()
	db, svc, req, _, _ := floorFix(t)
	const maker, other = "stn-aaaa1111", "stn-bbbb2222"

	p, repeat, err := svc.CreateFromFloor(maker, req)
	testutil.MustNoErr(t, err, "floor create")
	if repeat {
		t.Fatal("the first submit is marked a repeat")
	}
	retry := func(station string) (bool, error) {
		t.Helper()
		before := rowCounts(t, db)
		got, repeat, err := svc.CreateFromFloor(station, req)
		if after := rowCounts(t, db); after != before {
			t.Errorf("a retry from %s wrote rows: %v -> %v", station, before, after)
		}
		if err == nil && got.ID != p.ID {
			t.Errorf("a retry from %s answered part %d, want %d", station, got.ID, p.ID)
		}
		return repeat, err
	}
	if repeat, err := retry(maker); err != nil || !repeat {
		t.Errorf("the maker's retry: repeat=%v err=%v, want the part marked a repeat", repeat, err)
	}
	var exists *PartExistsError
	if _, err := retry(other); !errors.As(err, &exists) || exists.Code != req.PartNumber {
		t.Errorf("another station's identical submit: err=%v, want PartExistsError for %s", err, req.PartNumber)
	}
	if repeat, err := retry(maker); err != nil || !repeat {
		t.Errorf("the maker's retry after the refusal: repeat=%v err=%v, want a repeat", repeat, err)
	}

	second := req
	second.PartNumber, second.CATID = "SYN-FL-PIN-2", ""
	q, _, err := svc.CreateFromFloor(other, second)
	testutil.MustNoErr(t, err, "the other station's own part")
	made, err := svc.FloorMade()
	testutil.MustNoErr(t, err, "floor made")
	got := map[int64]string{}
	for id, m := range made {
		got[id] = m.Station
	}
	if want := map[int64]string{p.ID: maker, q.ID: other}; !reflect.DeepEqual(got, want) {
		t.Errorf("the payloads page's makers = %v, want %v", got, want)
	}
}
