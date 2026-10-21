//go:build docker

package www

import (
	"database/sql"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// handlers_containment_test.go — the quality-hold door the Edge's station
// action calls, POST /api/telemetry/bin-quality-hold (apiBinQualityHold over
// PayloadService.SetBinHold), pinned arm by arm with the door-pin instruments
// (runDoorArms, handlers_telemetry_test.go's doorErr and snapDoor).

// holdOf is a bin's hold marker as stored.
func holdOf(t *testing.T, f *doorFix, label string) (bool, string, bool) {
	t.Helper()
	var hold bool
	var by string
	var at sql.NullTime
	testutil.MustNoErr(t, f.db.QueryRow(`SELECT quality_hold, hold_by, hold_at FROM bins WHERE label=$1`, label).
		Scan(&hold, &by, &at), "read the hold of "+label)
	return hold, by, at.Valid
}

// TestBinQualityHoldDoor_Arms pins every arm of the hold door: the status, the
// body byte for byte, and what was written. The door takes a bin id and
// nothing else: it does not ask where the bin stands and writes no audit row.
// An id no bin has is refused with a sentence, never answered ok over a write
// that did nothing; it names no node, so the refusal carries no rows. A write
// the database refuses is a fault, 500, not a bad request.
//
// CHANGED (RULINGS D2): an unknown id was answered ok and wrote nothing, no
// bin_id likewise, and a database fault was 400.
func TestBinQualityHoldDoor_Arms(t *testing.T) {
	t.Parallel()
	onStorage := func(t *testing.T, f *doorFix, label string) *bins.Bin {
		return testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, f.sd.StorageNode.ID, label)
	}
	held := func(t *testing.T, f *doorFix, label, by string) *bins.Bin {
		b := onStorage(t, f, label)
		testutil.MustNoErr(t, f.db.SetBinQualityHold(b.ID, true, by), "hold "+label)
		return b
	}
	ok := func(*testing.T, *doorFix) (int, string) { return http.StatusOK, "{\"ok\":true}\n" }
	wroteCols := func(label string, cols ...string) func(*doorFix) (map[int64][]string, map[string]int) {
		return func(f *doorFix) (map[int64][]string, map[string]int) {
			return map[int64][]string{binIDOf(f, label): cols}, map[string]int{}
		}
	}
	wantHold := func(label string, hold bool, by string, at bool) func(*testing.T, *doorFix) {
		return func(t *testing.T, f *doorFix) {
			gotHold, gotBy, gotAt := holdOf(t, f, label)
			if gotHold != hold || gotBy != by || gotAt != at {
				t.Errorf("hold of %s = {%v %q at:%v}, want {%v %q at:%v}", label, gotHold, gotBy, gotAt, hold, by, at)
			}
		}
	}
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiBinQualityHold }, "/api/telemetry/bin-quality-hold", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "unexpected EOF")},
		{name: "a hold sets the marker with its station",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-SET")
				return map[string]any{"bin_id": b.ID, "hold": true, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: ok,
			wrote:  wroteCols("QH-SET", "hold_at", "hold_by", "quality_hold", "updated_at"),
			after:  wantHold("QH-SET", true, "edge.qh", true)},
		{name: "a hold with no station is attributed to unknown",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-ANON")
				return map[string]any{"bin_id": b.ID, "hold": true}, []int64{b.ID}
			},
			answer: ok,
			wrote:  wroteCols("QH-ANON", "hold_at", "hold_by", "quality_hold", "updated_at"),
			after:  wantHold("QH-ANON", true, "unknown", true)},
		{name: "a hold of a held bin takes the new station and time",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := held(t, f, "QH-AGAIN", "edge.first")
				return map[string]any{"bin_id": b.ID, "hold": true, "by": "edge.second"}, []int64{b.ID}
			},
			answer: ok,
			wrote:  wroteCols("QH-AGAIN", "hold_at", "hold_by", "updated_at"),
			after:  wantHold("QH-AGAIN", true, "edge.second", true)},
		{name: "a release clears the marker and its attribution",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := held(t, f, "QH-CLEAR", "edge.qh")
				return map[string]any{"bin_id": b.ID, "hold": false, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: ok,
			wrote:  wroteCols("QH-CLEAR", "hold_at", "hold_by", "quality_hold", "updated_at"),
			after:  wantHold("QH-CLEAR", false, "", false)},
		{name: "a release of an unheld bin touches only its updated_at",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-NOOP")
				return map[string]any{"bin_id": b.ID, "hold": false}, []int64{b.ID}
			},
			answer: ok,
			wrote:  wroteCols("QH-NOOP", "updated_at"),
			after:  wantHold("QH-NOOP", false, "", false)},
		{name: "a hold of an id no bin has is refused and writes nothing",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-BYSTANDER")
				return map[string]any{"bin_id": 987654321, "hold": true, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: refusedWith(http.StatusNotFound, "bin 987654321 is not known to Core — nothing was done")},
		{name: "a release of an id no bin has is refused and writes nothing",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := held(t, f, "QH-BYSTANDER-R", "edge.qh")
				return map[string]any{"bin_id": 987654321, "hold": false, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: refusedWith(http.StatusNotFound, "bin 987654321 is not known to Core — nothing was done"),
			after:  wantHold("QH-BYSTANDER-R", true, "edge.qh", true)},
		{name: "no bin_id is refused and writes nothing",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-NOID")
				return map[string]any{"hold": true, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "bin_id is required")},
		{name: "a write the database refuses is a fault, answered 500",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "QH-FAULT")
				failBinWrites(t, f.db, b.ID, "quality_hold")
				return map[string]any{"bin_id": b.ID, "hold": true, "by": "edge.qh"}, []int64{b.ID}
			},
			answer: refusedWith(http.StatusInternalServerError, "ERROR: door fault (SQLSTATE P0001)")},
	})
}

// TestBinQualityHoldDoor_AcceptHasNoContentType: the accept is written with a
// bare encoder, not jsonOK, so it carries no Content-Type of its own and the
// server sniffs one. The refusals go through jsonError and say JSON.
func TestBinQualityHoldDoor_AcceptHasNoContentType(t *testing.T) {
	t.Parallel()
	f := newDoorFix(t)
	b := testdb.CreateBinAtNode(t, f.db, f.sd.Payload.Code, f.sd.StorageNode.ID, "QH-CT")
	rec := postJSON(t, f.h.apiBinQualityHold, "/api/telemetry/bin-quality-hold",
		map[string]any{"bin_id": b.ID, "hold": true, "by": "edge.qh"})
	if got := rec.Header().Get("Content-Type"); rec.Code != http.StatusOK || got != "text/plain; charset=utf-8" {
		t.Errorf("accept: %d Content-Type %q, want 200 with the sniffed text/plain; charset=utf-8", rec.Code, got)
	}
	rec = postRaw(t, f.h.apiBinQualityHold, "/api/telemetry/bin-quality-hold", []byte("{"))
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("refusal: Content-Type %q, want application/json", got)
	}
}
