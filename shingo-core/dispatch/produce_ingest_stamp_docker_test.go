//go:build docker

package dispatch

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// produce_ingest_stamp_docker_test.go — the declared-full stamp on the produce
// ingest (strict pack, lane P0). A station's RELEASE FULL on a bin counted
// below its pack ships the counted number in Quantity and the declaration in
// DeclaredQty; Core books the count and stamps the declaration in one
// transaction, with a release_declared_full row naming who declared it, the
// seat, and the departing order. Every other ingest is today's: the count, and
// no row. Uses s1bIngestFixture (capacity 100, one bin on SYN-PRESS-S1B).

// stampRow is the release_declared_full row for one bin, if any.
type stampRow struct {
	before, after int
	actor         string
	orderID       sql.NullInt64
	detail        string
}

func readStamp(t *testing.T, d *Dispatcher, binID int64) (stampRow, int) {
	t.Helper()
	var n int
	testutil.MustNoErr(t, d.db.QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op='release_declared_full'`,
		binID).Scan(&n), "count stamps")
	var r stampRow
	if n == 1 {
		testutil.MustNoErr(t, d.db.QueryRow(`SELECT before_uop, after_uop, actor, order_id, detail::text
			FROM bin_uop_ledger WHERE bin_id=$1 AND op='release_declared_full'`, binID).
			Scan(&r.before, &r.after, &r.actor, &r.orderID, &r.detail), "read stamp")
	}
	return r, n
}

// A stamped ingest: the bin leaves at the declared 100, one row 97 -> 100 with
// the declarer, the seat and Core's id for the departing order.
func TestIngestStamp_DeclaredFullStampsTheBin(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	order := &orders.Order{EdgeUUID: "edge-order-stamp-1", StationID: "test", OrderType: protocol.OrderTypeComplex,
		Status: "dispatched", Quantity: 1, PayloadCode: bp.Code}
	testutil.MustNoErr(t, d.db.CreateOrder(order), "the departing order")

	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-1", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7", OrderID: order.EdgeUUID,
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 100 || !got.ManifestConfirmed {
		t.Errorf("bin: uop=%d confirmed=%v, want 100 true", got.UOPRemaining, got.ManifestConfirmed)
	}
	r, n := readStamp(t, d, bin.ID)
	if n != 1 {
		t.Fatalf("release_declared_full rows = %d, want 1", n)
	}
	if r.before != 97 || r.after != 100 || r.actor != "operator-7" {
		t.Errorf("stamp = %d -> %d by %q, want 97 -> 100 by operator-7", r.before, r.after, r.actor)
	}
	if !r.orderID.Valid || r.orderID.Int64 != order.ID {
		t.Errorf("stamp order_id = %v, want Core's id %d for edge uuid %s", r.orderID, order.ID, order.EdgeUUID)
	}
	for _, key := range []string{`"node": "SYN-PRESS-S1B"`, `"counted": 97`, `"pack": 100`, `"declared_qty": 100`,
		`"declared_part": "` + bp.Code + `"`} {
		if !strings.Contains(r.detail, key) {
			t.Errorf("stamp detail %s lacks %s", r.detail, key)
		}
	}
	if strings.Contains(r.detail, "order_unresolved") {
		t.Errorf("a resolved order is reported unresolved: %s", r.detail)
	}
}

// An order id Core does not know: the stamp is still written, with no order
// id, and the detail says which uuid did not resolve.
func TestIngestStamp_UnresolvableOrderIsSaidInTheRow(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-2", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7", OrderID: "edge-order-nobody-knows",
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	r, n := readStamp(t, d, bin.ID)
	if n != 1 {
		t.Fatalf("release_declared_full rows = %d, want 1", n)
	}
	if r.orderID.Valid {
		t.Errorf("stamp order_id = %d, want NULL for an unknown order", r.orderID.Int64)
	}
	for _, key := range []string{`"order_unresolved": true`, `"order_uuid": "edge-order-nobody-knows"`} {
		if !strings.Contains(r.detail, key) {
			t.Errorf("stamp detail %s lacks %s", r.detail, key)
		}
	}
}

// An ingest with no declaration (an older Edge, or any release that is not a
// station's FULL) books the count and writes no stamp.
func TestIngestStamp_NoDeclarationBooksTheCount(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-3", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97,
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 97 {
		t.Errorf("uop = %d, want the counted 97", got.UOPRemaining)
	}
	if _, n := readStamp(t, d, bin.ID); n != 0 {
		t.Errorf("release_declared_full rows = %d, want 0", n)
	}
}

// An older Core: the stamped ingest decoded by today's struct, which has no
// declaration fields, books the counted number with no error and no stamp.
func TestIngestStamp_OlderCoreDecodeBooksTheCount(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	wire, err := json.Marshal(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-4", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7", OrderID: "edge-order-x",
	})
	testutil.MustNoErr(t, err, "marshal")
	var old struct {
		OrderUUID   string `json:"order_uuid"`
		PayloadCode string `json:"payload_code"`
		BinLabel    string `json:"bin_label"`
		BinID       int64  `json:"bin_id,omitempty"`
		BinEpoch    int64  `json:"bin_epoch,omitempty"`
		SourceNode  string `json:"source_node"`
		Quantity    int64  `json:"quantity"`
		ProducedAt  string `json:"produced_at,omitempty"`
	}
	testutil.MustNoErr(t, json.Unmarshal(wire, &old), "an older Core decodes it")
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: old.OrderUUID, PayloadCode: old.PayloadCode, BinLabel: old.BinLabel, BinID: old.BinID,
		BinEpoch: old.BinEpoch, SourceNode: old.SourceNode, Quantity: old.Quantity, ProducedAt: old.ProducedAt,
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 97 {
		t.Errorf("uop = %d, want the counted 97", got.UOPRemaining)
	}
	if _, n := readStamp(t, d, bin.ID); n != 0 {
		t.Errorf("release_declared_full rows = %d, want 0", n)
	}
}

// A refused ingest (stale epoch) writes no stamp: the refusal row, the bin
// unchanged.
func TestIngestStamp_RefusedIngestStampsNothing(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-5", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch + 5,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7",
	})
	if lerr == nil || lerr.Code != "stale_epoch" {
		t.Fatalf("ApplyIngestManifest = %+v, want stale_epoch", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("uop = %d, want unchanged %d", got.UOPRemaining, bin.UOPRemaining)
	}
	if _, n := readStamp(t, d, bin.ID); n != 0 {
		t.Errorf("release_declared_full rows = %d, want 0", n)
	}
}

// stampPart is the booked part the release_declared_full row was written
// under (its payload_code column).
func stampPart(t *testing.T, d *Dispatcher, binID int64) string {
	t.Helper()
	var code string
	testutil.MustNoErr(t, d.db.QueryRow(`SELECT payload_code FROM bin_uop_ledger WHERE bin_id=$1 AND op='release_declared_full'`,
		binID).Scan(&code), "stamp part")
	return code
}

// bindAsAnotherPart makes the fixture's bin one counted as another part this
// life (a part of the given pack, bound by its first tick), so the ingest's
// rebind in ApplyIngestManifest books it under that part. Returns the part
// and the bin's epoch after the bind.
func bindAsAnotherPart(t *testing.T, d *Dispatcher, binID int64, code string, pack int) (*payloads.Payload, int64) {
	t.Helper()
	counted := &payloads.Payload{Code: code, UOPCapacity: pack}
	testutil.MustNoErr(t, d.db.CreatePayload(counted), "the counted part")
	testutil.MustNoErr(t, d.db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: counted.ID, PartNumber: "PART-" + code, PartsPerCycle: 1,
	}, ""), "its template line")
	testutil.MustNoErr(t, d.db.SetBinManifest(binID, `{"items":[{"part_number":"PART-`+code+`"}]}`, counted.Code, 3),
		"the first tick binds the counted part")
	bound, err := d.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	return counted, bound.DeltaEpoch
}

// The station's declaration is not the count Core stamps to: a FULL declared
// at 99 on a part whose catalog pack is 100 stamps the bin to 100, and the
// row keeps what was declared (declared_qty, declared_part) beside the pack.
func TestIngestStamp_SamePartStampsToItsOwnPackAndKeepsTheDeclaration(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-7", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 99, CalledBy: "operator-7",
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 100 {
		t.Errorf("bin at %d, want the pack 100 (declared 99)", got.UOPRemaining)
	}
	r, n := readStamp(t, d, bin.ID)
	if n != 1 {
		t.Fatalf("release_declared_full rows = %d, want 1", n)
	}
	if r.before != 97 || r.after != 100 {
		t.Errorf("stamp = %d -> %d, want 97 -> 100", r.before, r.after)
	}
	for _, key := range []string{`"pack": 100`, `"declared_qty": 99`, `"declared_part": "` + bp.Code + `"`} {
		if !strings.Contains(r.detail, key) {
			t.Errorf("stamp detail %s lacks %s", r.detail, key)
		}
	}
}

// A bin counted as another part this life (the rebind in ApplyIngestManifest)
// keeps the counted part and is stamped to that part's own pack (120), not to
// the declaration the station made for the order's part (100); the row is
// written under the booked part and names the declared part and quantity.
func TestIngestStamp_ReboundBinStampsToTheBookedPartsPack(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	counted, epoch := bindAsAnotherPart(t, d, bin.ID, "SYN-STAMP-COUNTED", 120)
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-6", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: epoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7",
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.PayloadCode != counted.Code || got.UOPRemaining != 120 {
		t.Errorf("bin = %s at %d, want %s at its pack 120", got.PayloadCode, got.UOPRemaining, counted.Code)
	}
	r, n := readStamp(t, d, bin.ID)
	if n != 1 {
		t.Fatalf("release_declared_full rows = %d, want 1", n)
	}
	if r.before != 97 || r.after != 120 {
		t.Errorf("stamp = %d -> %d, want 97 -> 120", r.before, r.after)
	}
	if part := stampPart(t, d, bin.ID); part != counted.Code {
		t.Errorf("stamp written under %s, want the booked %s", part, counted.Code)
	}
	for _, key := range []string{`"pack": 120`, `"declared_qty": 100`, `"declared_part": "` + bp.Code + `"`} {
		if !strings.Contains(r.detail, key) {
			t.Errorf("stamp detail %s lacks %s", r.detail, key)
		}
	}
}

// No pack Core can resolve for the booked part (a catalog capacity of 0):
// the count is booked, nothing is stamped, and the declaration is still
// recorded as a release_declared_full row from the count to the count with
// reason no_pack, so it is listed with the stamps on /inventory. The literal
// is the stored value (service.DeclaredFullNoPack); rows already written keep it.
func TestIngestStamp_NoPackBooksTheCountAndSaysSo(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	bp.UOPCapacity = 0
	testutil.MustNoErr(t, d.db.UpdatePayload(bp), "a part with no pack")
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-8", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: bin.DeltaEpoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7",
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.UOPRemaining != 97 || !got.ManifestConfirmed {
		t.Errorf("bin: uop=%d confirmed=%v, want the counted 97, confirmed", got.UOPRemaining, got.ManifestConfirmed)
	}
	r, n := readStamp(t, d, bin.ID)
	if n != 1 {
		t.Fatalf("release_declared_full rows = %d, want 1 (the no_pack row)", n)
	}
	if r.before != 97 || r.after != 97 || r.actor != "operator-7" {
		t.Errorf("row = %d -> %d by %q, want 97 -> 97 by operator-7", r.before, r.after, r.actor)
	}
	for _, key := range []string{`"reason": "no_pack"`, `"declared_qty": 100`, `"declared_part": "` + bp.Code + `"`} {
		if !strings.Contains(r.detail, key) {
			t.Errorf("row detail %s lacks %s", r.detail, key)
		}
	}
	if strings.Contains(r.detail, `"pack"`) {
		t.Errorf("row detail %s names a pack; there is none", r.detail)
	}
}

// A declaration whose booked part's pack (90) is not above the count (97)
// stamps nothing and writes no row: a count is never written down.
func TestIngestStamp_BookedPackNotAboveTheCountStampsNothing(t *testing.T) {
	t.Parallel()
	d, bin, bp := s1bIngestFixture(t)
	counted, epoch := bindAsAnotherPart(t, d, bin.ID, "SYN-STAMP-SMALL", 90)
	if lerr := d.lifecycle.ApplyIngestManifest(&protocol.OrderIngestRequest{
		OrderUUID: "msg-stamp-9", PayloadCode: bp.Code, BinID: bin.ID, BinEpoch: epoch,
		SourceNode: "SYN-PRESS-S1B", Quantity: 97, DeclaredQty: 100, CalledBy: "operator-7",
	}); lerr != nil {
		t.Fatalf("ApplyIngestManifest: %+v", lerr)
	}
	got, err := d.db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if got.PayloadCode != counted.Code || got.UOPRemaining != 97 {
		t.Errorf("bin = %s at %d, want %s at the counted 97", got.PayloadCode, got.UOPRemaining, counted.Code)
	}
	if _, n := readStamp(t, d, bin.ID); n != 0 {
		t.Errorf("release_declared_full rows = %d, want 0", n)
	}
}
