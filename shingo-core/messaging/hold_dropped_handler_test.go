//go:build docker

package messaging

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// hold_dropped_handler_test.go — what Core writes for an Edge's dropped produce
// hold (strict pack, lane P0): one hold_dropped row on bin_uop_exception per
// envelope, with no bin (a hold has no carrier), the part on the row, the
// dropper as actor, and the seat, quantity, reason and station in detail.

func holdDropEnvelope(id, station string) *protocol.Envelope {
	return &protocol.Envelope{
		ID:   id,
		Type: protocol.TypeData,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: station},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

func TestHoldDropped_WritesOneRowWithNoBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newLinesideService(t, db)
	// Nanoseconds on purpose: Postgres keeps microseconds, and the redelivery
	// key compares the stored value.
	at := time.Date(2026, 10, 20, 8, 15, 0, 123456789, time.UTC)
	drop := &protocol.HoldDropped{
		Node: "ALN_011", PayloadCode: "SYN44-PARTA.06", Qty: 24, Reason: protocol.HoldDropSweep,
		CalledBy: "edge:uop_stranded_monitor", OccurredAt: at, OrderID: "edge-order-held-for",
	}

	svc.HandleHoldDropped(holdDropEnvelope("env-drop-1", "stn-drop"), drop)
	// The same envelope again (a Kafka redelivery, or the outbox re-sending
	// after a publish whose ack was lost) writes nothing more.
	svc.HandleHoldDropped(holdDropEnvelope("env-drop-1", "stn-drop"), drop)

	var (
		n                  int
		bin                sql.NullInt64
		payload, actor, op string
		occurred           time.Time
		before, after      sql.NullInt64
		detailRaw          []byte
		recovered          sql.NullTime
	)
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_exception WHERE kind = 'hold_dropped'`).Scan(&n), "count rows")
	if n != 1 {
		t.Fatalf("hold_dropped rows = %d after one message delivered twice, want 1", n)
	}
	testutil.MustNoErr(t, db.QueryRow(`SELECT bin_id, payload_code, actor, op, occurred_at, before_uop, after_uop, detail, recovered_at
		FROM bin_uop_exception WHERE kind = 'hold_dropped'`).
		Scan(&bin, &payload, &actor, &op, &occurred, &before, &after, &detailRaw, &recovered), "read row")
	if bin.Valid {
		t.Errorf("bin_id = %d, want NULL (a hold has no carrier)", bin.Int64)
	}
	if payload != "SYN44-PARTA.06" || actor != "edge:uop_stranded_monitor" || op != protocol.HoldDropSweep {
		t.Errorf("row = payload %q actor %q op %q, want the part, the dropper and the reason", payload, actor, op)
	}
	if !occurred.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("occurred_at = %v, want the Edge's %v", occurred, at)
	}
	if before.Valid || after.Valid || recovered.Valid {
		t.Errorf("before/after/recovered = %v/%v/%v, want all NULL", before, after, recovered)
	}
	var detail map[string]any
	testutil.MustNoErr(t, json.Unmarshal(detailRaw, &detail), "decode detail")
	for key, want := range map[string]any{
		"node": "ALN_011", "qty": float64(24), "reason": protocol.HoldDropSweep, "station": "stn-drop",
		"order_id": "edge-order-held-for", "msg_id": "env-drop-1",
	} {
		if detail[key] != want {
			t.Errorf("detail[%s] = %v, want %v (detail %s)", key, detail[key], want, detailRaw)
		}
	}

	// A different drop is a different row.
	svc.HandleHoldDropped(holdDropEnvelope("env-drop-2", "stn-drop"), &protocol.HoldDropped{
		Node: "ALN_011", PayloadCode: "SYN44-PARTA.06", Qty: 1, Reason: protocol.HoldDropCutover, OccurredAt: at,
	})
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_exception WHERE kind = 'hold_dropped'`).Scan(&n), "count rows")
	if n != 2 {
		t.Errorf("hold_dropped rows = %d after a second message, want 2", n)
	}
}

// A message with no station on it and none on the envelope, or no node, is
// still the only record of parts leaving the count: it is written, not
// dropped. A missing occurred_at takes Core's clock.
func TestHoldDropped_IncompleteMessageIsStillWritten(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newLinesideService(t, db)
	svc.HandleHoldDropped(holdDropEnvelope("env-drop-bare", ""), &protocol.HoldDropped{Qty: 2, Reason: "no_carrier"})
	var n int
	var occurred time.Time
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*), MAX(occurred_at) FROM bin_uop_exception WHERE kind = 'hold_dropped'`).
		Scan(&n, &occurred), "count rows")
	if n != 1 || occurred.IsZero() {
		t.Errorf("rows = %d occurred_at = %v, want one row stamped with Core's clock", n, occurred)
	}
}
