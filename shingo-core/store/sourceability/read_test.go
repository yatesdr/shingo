//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// This proves BuildInputs' SQL against a real DB: the available-bin pool follows
// the FindSourceFIFO predicate (claimed and reserved bins drop out), and
// Compute's verdict flows from it. Pure-computation edge cases live in the
// no-DB fixtures (sourceability_test.go).

func stateFor(t *testing.T, states []sourceability.StyleState, process, style string) sourceability.StyleState {
	t.Helper()
	for _, s := range states {
		if s.ProcessID == process && s.StyleID == style {
			return s
		}
	}
	t.Fatalf("no state for %s/%s in %+v", process, style, states)
	return sourceability.StyleState{}
}

func TestBuildInputsAndCompute_PoolPredicate(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB

	std := testdb.SetupStandardData(t, sdb)
	bin := testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "src-a")

	// Mirror: SNF2 with style A (needs BIN-A, available) and style Z (needs
	// BIN-Z, which no bin holds).
	styles := []plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A"}, {ProcessID: "SNF2", StyleID: "Z"}}
	claims := []plantclaims.ClaimRow{
		{ProcessID: "SNF2", StyleID: "A", CoreNodeName: std.StorageNode.Name, PayloadCode: "BIN-A"},
		{ProcessID: "SNF2", StyleID: "Z", CoreNodeName: std.StorageNode.Name, PayloadCode: "BIN-Z"},
	}
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db, "SNF2", styles, claims, 0), "seed mirror")

	compute := func() []sourceability.StyleState {
		in, err := sourceability.BuildInputs(db, time.Hour)
		testutil.MustNoErr(t, err, "build inputs")
		return sourceability.Compute(in, sourceability.Config{}, time.Now())
	}

	// Available bin present → A green, Z red (missing BIN-Z).
	states := compute()
	if got := stateFor(t, states, "SNF2", "A"); got.Status != sourceability.StatusGreen {
		t.Errorf("A status = %q, want green", got.Status)
	}
	z := stateFor(t, states, "SNF2", "Z")
	if z.Status != sourceability.StatusRed || len(z.Missing) != 1 || z.Missing[0] != "BIN-Z" {
		t.Errorf("Z = %+v, want red missing [BIN-Z]", z)
	}

	// Claim the bin → it leaves the pool → A goes red missing BIN-A.
	if _, err := db.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusRed {
		t.Errorf("A after claim = %q, want red (claimed bin not sourceable)", got.Status)
	}

	// Unclaim, then place a PENDING reservation → still out of the pool.
	if _, err := db.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("unclaim bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusGreen {
		t.Fatalf("A after unclaim = %q, want green again", got.Status)
	}
	var orderID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		 VALUES ('res-1','edge.line1','retrieve','pending',1) RETURNING id`).Scan(&orderID), "create order")
	if _, err := db.Exec(
		`INSERT INTO reservations (order_id, bin_id, state, expires_at)
		 VALUES ($1, $2, 'pending', NOW() + INTERVAL '1 hour')`, orderID, bin.ID); err != nil {
		t.Fatalf("reserve bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusRed {
		t.Errorf("A after pending reservation = %q, want red (reserved bin not sourceable)", got.Status)
	}
}
