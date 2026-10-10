//go:build docker

package main

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestFixtureBalance_AnOverMadePayloadIsFlagged: a payload made twice as fast as
// it is drawn is flagged with its carrier pool, and one drawn twice as fast as
// it is made is flagged the other way; one made within the margin, and one with
// no consume tick at all, are not.
func TestFixtureBalance_AnOverMadePayloadIsFlagged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	over := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "FB-OVER")
	tick := func(binID int64, payload, reason string, units int) {
		_, err := db.Exec(`INSERT INTO bin_uop_ledger (bin_id, before_uop, after_uop, op, payload_code, reason)
			VALUES ($1, 0, $2, 'test', $3, $4)`, binID, units, payload, reason)
		testutil.MustNoErr(t, err, "seed a ledger tick")
	}
	tick(over.ID, sd.Payload.Code, "produce_tick", 4000)
	tick(over.ID, sd.Payload.Code, "consume_tick", 2000)
	tick(over.ID, "FB-EVEN", "produce_tick", 2100)
	tick(over.ID, "FB-EVEN", "consume_tick", 2000)
	tick(over.ID, "FB-DRAINED", "produce_tick", 80)
	tick(over.ID, "FB-SHORT", "produce_tick", 1000)
	tick(over.ID, "FB-SHORT", "consume_tick", 2000)

	bal, err := fixtureBalance(db.DB)
	testutil.MustNoErr(t, err, "read the balance")
	flagged, short := map[string]bool{}, map[string]bool{}
	for _, b := range bal {
		if b.over() {
			flagged[b.payload] = true
		}
		if b.under() {
			short[b.payload] = true
		}
	}
	if !flagged[sd.Payload.Code] || flagged["FB-EVEN"] || flagged["FB-DRAINED"] || flagged["FB-SHORT"] {
		t.Fatalf("over-made %v; want only %s (made 4000, drawn 2000); rows %+v", flagged, sd.Payload.Code, bal)
	}
	if len(short) != 1 || !short["FB-SHORT"] {
		t.Fatalf("under-made %v; want only FB-SHORT (made 1000, drawn 2000); rows %+v", short, bal)
	}
	got := balanceFlags(bal)
	if !strings.Contains(got, "over-makes "+sd.Payload.Code) || !strings.Contains(got, "under-makes FB-SHORT") {
		t.Fatalf("summary %q does not name both flagged payloads", got)
	}
}
