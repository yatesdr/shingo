//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// The count-from-the-line door. Core could declare a count downward — a clear,
// a load, or a cycle count on the bins page — but the only way a number could
// come UP was riding an order release. An operator standing at the line who saw
// that a carrier's count was wrong had nowhere to say so, and that is the
// number most likely to be right.

func TestApiBinCount_RecordsTheDeclaredCount(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-1")

	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin before")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 7, "actor": "line-operator",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string `json:"status"`
		BinID        int64  `json:"bin_id"`
		Expected     int    `json:"expected"`
		UOPRemaining int    `json:"uop_remaining"`
		DeltaEpoch   int64  `json:"delta_epoch"`
	}
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	if resp.Status != "ok" || resp.BinID != bin.ID || resp.UOPRemaining != 7 {
		t.Errorf("response: got %+v, want the carrier and the declared 7", resp)
	}
	if resp.Expected != before.UOPRemaining {
		t.Errorf("expected = %d, want %d — the reply has to say what it was, or the "+
			"operator cannot see the size of the correction", resp.Expected, before.UOPRemaining)
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin after")
	if got.UOPRemaining != 7 {
		t.Errorf("uop_remaining = %d, want 7 — the ledger has to take the number from the "+
			"line, which is the side standing next to the carrier", got.UOPRemaining)
	}
}

// TestApiBinCount_DoesNotStartANewGeneration is the one thing this door must
// NOT do. A count correction fixes a number inside the carrier's current life;
// a new generation is for a carrier that has been emptied and reloaded. Bumping
// here would retire a life that is still running, and the station's very next
// report would arrive stamped with a generation Core had just ended and be
// thrown away — the exact failure the rest of this work removes, reintroduced
// by the fix for it.
func TestApiBinCount_DoesNotStartANewGeneration(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-2")

	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin before")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 12,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin after")
	if got.DeltaEpoch != before.DeltaEpoch {
		t.Errorf("delta_epoch went %d → %d — a count correction is not a new life for the "+
			"carrier, and retiring the running one makes the station's next report stale",
			before.DeltaEpoch, got.DeltaEpoch)
	}
}

// TestApiBinCount_ClearsTheGoCountThisFlag: the flag means "somebody go count
// this". Counting it has to clear it, or it ratchets — which it did, at both
// plants, until that was fixed on the bins-page path. The door from the line
// goes through the same service, so it inherits the same behaviour, and this
// test is what says so out loud.
func TestApiBinCount_ClearsTheGoCountThisFlag(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-3")

	_, err := db.Exec(`UPDATE bins SET anomaly_at=NOW() WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "flag the carrier")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var flagged *string
	testutil.MustNoErr(t, db.QueryRow(`SELECT anomaly_at::text FROM bins WHERE id=$1`, bin.ID).Scan(&flagged), "read flag")
	if flagged != nil {
		t.Errorf("anomaly_at = %q, want cleared — a flag that survives being answered is a "+
			"ratchet, and the board can never be returned to clean", *flagged)
	}
}

func TestApiBinCount_RefusesAnEmptyNode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": 5,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 for a node holding nothing; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiBinCount_RefusesANegativeCount(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-COUNT-4")

	rec := postJSON(t, h.apiBinCount, "/api/telemetry/bin-count", map[string]any{
		"node_name": sd.StorageNode.Name, "actual_uop": -1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestBinCountDoor_Arms pins every arm of POST /api/telemetry/bin-count
// (apiBinCount over BinService.RecordCount): the status, the body byte for
// byte, and what was written — nothing at all on a refusal. The fence a count
// carries when a station has a cursor is
// TestLineCount_BroadcastsFencedAndRepliesWithTheFence; here no station claims
// the storage node, so the reply's fence is empty and nothing is broadcast.
func TestBinCountDoor_Arms(t *testing.T) {
	t.Parallel()
	storage := func(f *doorFix) string { return f.sd.StorageNode.Name }
	// onStorage is a carrier of payload at STORAGE-A1 holding 100.
	onStorage := func(t *testing.T, f *doorFix, payload, label string) *bins.Bin {
		return testdb.CreateBinAtNode(t, f.db, payload, f.sd.StorageNode.ID, label)
	}
	countBody := func(f *doorFix, fields map[string]any) map[string]any {
		b := map[string]any{"node_name": f.sd.StorageNode.Name, "actual_uop": 7, "actor": "line-operator"}
		for k, v := range fields {
			b[k] = v
		}
		return b
	}
	counted := func(label string, actual int, warning string) func(*testing.T, *doorFix) (int, string) {
		return func(t *testing.T, f *doorFix) (int, string) {
			b := binByLabel(t, f.db, label)
			return http.StatusOK, doorJSON(t, protocol.BinCountResponse{Status: "ok", BinID: b.ID, BinLabel: label,
				Expected: 100, UOPRemaining: actual, Discrepancy: actual != 100, Warning: warning, DeltaEpoch: b.DeltaEpoch})
		}
	}
	// A count writes the number and who took it, clears the go-count-this
	// flag (already clear here, so it does not show), keeps the epoch, and
	// adds its ledger row and its audit row.
	wroteCount := func(label string, ledger int) func(*doorFix) (map[int64][]string, map[string]int) {
		return func(f *doorFix) (map[int64][]string, map[string]int) {
			return map[int64][]string{binIDOf(f, label): {"last_counted_at", "last_counted_by", "uop_remaining", "updated_at"}},
				map[string]int{"bin_uop_ledger": ledger, "audit_log": 1}
		}
	}
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiBinCount }, "/api/telemetry/bin-count", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "unexpected EOF")},
		{name: "no node_name is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-NONAME")
				return countBody(f, map[string]any{"node_name": ""}), []int64{b.ID}
			},
			answer: refusedWith(http.StatusBadRequest, "node_name is required")},
		{name: "an unknown node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				return countBody(f, map[string]any{"node_name": "CT-NO-SUCH-NODE"}), nil
			},
			answer: refusedAt(atNode("CT-NO-SUCH-NODE"), http.StatusNotFound, `node "CT-NO-SUCH-NODE" not found`)},
		{name: "a node holding no bin is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) { return countBody(f, nil), nil },
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusBadRequest, "no bin at node "+storage(f))
			}},
		{name: "a bin_id not at the node is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-FOREIGNID")
				return countBody(f, map[string]any{"bin_id": 987654321}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "bin 987654321 is not at node "+storage(f))
			}},
		{name: "two bins and no bin_id is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, f.sd.Payload.Code, "CT-AMB-1"), onStorage(t, f, f.sd.Payload.Code, "CT-AMB-2")
				return countBody(f, nil), []int64{a.ID, b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusConflict, "node "+storage(f)+" holds 2 bins; specify bin_id to disambiguate")
			}},
		{name: "a bin with no payload is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "", "CT-EMPTY")
				return countBody(f, nil), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				return refusalAt(t, f, storage(f), http.StatusBadRequest, fmt.Sprintf("cannot validate UOP capacity — bin %d has no payload", binIDOf(f, "CT-EMPTY")))
			}},
		{name: "a payload Core has no template for is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, "CT-GHOST", "CT-GHOST-BIN")
				return countBody(f, nil), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, `lookup payload "CT-GHOST": sql: no rows in result set`)},
		{name: "a payload with no capacity is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				testutil.MustNoErr(t, f.db.CreatePayload(&payloads.Payload{Code: "CT-NOCAP"}), "a payload with no capacity")
				b := onStorage(t, f, "CT-NOCAP", "CT-NOCAP-BIN")
				return countBody(f, nil), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, `cannot validate UOP capacity — payload "CT-NOCAP" has no UOP capacity set`)},
		{name: "a negative count is refused",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-NEG")
				return countBody(f, map[string]any{"actual_uop": -1}), []int64{b.ID}
			},
			answer: refusedAt(storage, http.StatusBadRequest, "actual UOP must be 0 or more")},
		{name: "a write the database refuses is answered 400",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-FAULT")
				failBinWrites(t, f.db, b.ID, "uop_remaining")
				return countBody(f, nil), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				id := binIDOf(f, "CT-FAULT")
				return refusalAt(t, f, storage(f), http.StatusBadRequest, fmt.Sprintf("record count bin %d: ERROR: door fault (SQLSTATE P0001)", id))
			}},
		{name: "a count records the number",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-OK")
				return countBody(f, nil), []int64{b.ID}
			},
			answer: counted("CT-OK", 7, ""),
			wrote:  wroteCount("CT-OK", 1)},
		{name: "a count equal to the record is no discrepancy",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-SAME")
				return countBody(f, map[string]any{"actual_uop": 100}), []int64{b.ID}
			},
			answer: counted("CT-SAME", 100, ""),
			wrote: func(f *doorFix) (map[int64][]string, map[string]int) {
				return map[int64][]string{binIDOf(f, "CT-SAME"): {"last_counted_at", "last_counted_by", "updated_at"}},
					map[string]int{"bin_uop_ledger": 1, "audit_log": 1}
			}},
		{name: "a count over capacity records it and warns",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-OVER")
				return countBody(f, map[string]any{"actual_uop": 1200}), []int64{b.ID}
			},
			answer: counted("CT-OVER", 1200, "recorded count 1200 is above the payload's UOP capacity of 1000 — "+
				"the count was accepted; check the payload's capacity setting"),
			wrote: wroteCount("CT-OVER", 2)},
		{name: "a bin_id names one of two bins",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				a, b := onStorage(t, f, f.sd.Payload.Code, "CT-TWO-1"), onStorage(t, f, f.sd.Payload.Code, "CT-TWO-2")
				return countBody(f, map[string]any{"bin_id": b.ID}), []int64{a.ID, b.ID}
			},
			answer: counted("CT-TWO-2", 7, ""),
			wrote:  wroteCount("CT-TWO-2", 1)},
		// The generation the count was taken in (epoch): a count under any other
		// is refused with the carrier's current one, the body a bin epoch refresh
		// carries, and nothing is written.
		{name: "a count under another generation is refused with the current one",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-EPOCH")
				return countBody(f, map[string]any{"epoch": b.DeltaEpoch + 5}), []int64{b.ID}
			},
			answer: func(t *testing.T, f *doorFix) (int, string) {
				b := binByLabel(t, f.db, "CT-EPOCH")
				return http.StatusConflict, doorJSON(t, map[string]any{
					"epoch_refresh": protocol.BinEpochRefresh{BinID: b.ID, CoreNodeName: storage(f), Epoch: b.DeltaEpoch},
					"error":         "bin CT-EPOCH changed since that count was taken — nothing was recorded",
					"rows":          doorRows(t, f, storage(f))})
			}},
		{name: "a count under the current generation records the number",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				b := onStorage(t, f, f.sd.Payload.Code, "CT-EPOCH-OK")
				return countBody(f, map[string]any{"epoch": b.DeltaEpoch}), []int64{b.ID}
			},
			answer: counted("CT-EPOCH-OK", 7, ""),
			wrote:  wroteCount("CT-EPOCH-OK", 1)},
	})
}
