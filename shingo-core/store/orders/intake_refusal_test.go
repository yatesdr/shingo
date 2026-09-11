//go:build docker

package orders_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestIntakeRefusal_RecordsAndReplaces pins the record the pair rule reads when
// a partner's row is absent. A refusal round-trips; a uuid Core never refused
// reads as nil and not as an error, because "not received yet" is the common
// case; and a repeat refusal of the same uuid replaces the reason instead of
// failing on the key, so the partner quotes the latest one.
func TestIntakeRefusal_RecordsAndReplaces(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)

	if r, err := orders.GetIntakeRefusal(d.DB, "ir-never"); err != nil || r != nil {
		t.Fatalf("an unrefused uuid read as (%+v, %v), want (nil, nil)", r, err)
	}

	testutil.MustNoErr(t, orders.RecordIntakeRefusal(d.DB, "ir-leg", "ST1", "resolution_failed", "node X not found"), "record")
	r, err := orders.GetIntakeRefusal(d.DB, "ir-leg")
	testutil.MustNoErr(t, err, "read refusal")
	if r == nil || r.ErrorCode != "resolution_failed" || r.Detail != "node X not found" || r.StationID != "ST1" {
		t.Fatalf("refusal read back as %+v", r)
	}

	testutil.MustNoErr(t, orders.RecordIntakeRefusal(d.DB, "ir-leg", "ST1", "invalid_steps", "no steps"), "re-record")
	r, err = orders.GetIntakeRefusal(d.DB, "ir-leg")
	testutil.MustNoErr(t, err, "read refusal after a repeat")
	if r == nil || r.ErrorCode != "invalid_steps" || r.Detail != "no steps" {
		t.Fatalf("a repeat refusal did not replace the first: %+v", r)
	}
}
