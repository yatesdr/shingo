//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// TestBlockRecordMigration_V179_OldOrdersStayNullAndTheSeedLiftsAtItsLoad:
// v179 adds orders.block_actions over orders written before it, and every one
// of them reads NULL (sent before the record: read by block names for life),
// while an order born after reads '{}'. It adds load_sequences.lift_at and fills
// it by the frozen name rule: the seeded Child cart interlock lifts at "load",
// index 2. The CHECK refuses an index outside task_names. Re-applied through
// the migrate path (the v136 pattern); a second open changes nothing.
func TestBlockRecordMigration_V179_OldOrdersStayNullAndTheSeedLiftsAtItsLoad(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	old := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "v179-old" })
	for _, stmt := range []string{
		`ALTER TABLE orders DROP COLUMN block_actions`,
		`ALTER TABLE load_sequences DROP CONSTRAINT load_sequences_lift_at_in_task_names`,
		`ALTER TABLE load_sequences DROP COLUMN lift_at`,
		`INSERT INTO load_sequences (name, task_names) VALUES
			('V179 no lift name', '["Go_AP1","Spin_90"]'),
			('V179 empty', '[]'),
			('V179 jack keys', '["Spin","JackLoad","JackUnload"]'),
			('V179 a drop first', '["pick_drop","RELEASE","Pick_Up"]')`,
		`DELETE FROM schema_migrations WHERE version = 179`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "back to the pre-v179 shape: "+stmt)
	}

	for pass := 1; pass <= 2; pass++ {
		migrated, err := store.Open(cfg)
		testutil.MustNoErr(t, err, "re-open to apply v179")
		var actions sql.NullString
		testutil.MustNoErr(t, migrated.QueryRow(`SELECT block_actions::text FROM orders WHERE id=$1`, old.ID).Scan(&actions),
			"read the old order")
		if actions.Valid {
			t.Errorf("pass %d: the order born before v179 reads block_actions %q, want NULL", pass, actions.String)
		}
		if got, err := migrated.GetOrder(old.ID); err != nil || got.BlockActions != nil {
			t.Errorf("pass %d: the old order reads BlockActions %v (%v), want nil", pass, got.BlockActions, err)
		}
		for name, want := range map[string]int{"Child cart interlock": 2, "V179 no lift name": 1, "V179 empty": 0,
			"V179 jack keys": 1, "V179 a drop first": 2} {
			var liftAt int
			testutil.MustNoErr(t, migrated.QueryRow(`SELECT lift_at FROM load_sequences WHERE name=$1`, name).Scan(&liftAt),
				"read lift_at of "+name)
			if liftAt != want {
				t.Errorf("pass %d: %s lifts at %d, want %d", pass, name, liftAt, want)
			}
		}
		if failing := migrated.MigrationsFailingTheirPostCondition(); len(failing) != 0 {
			t.Errorf("pass %d: migrations failing their post-condition: %v", pass, failing)
		}
		testutil.MustNoErr(t, migrated.Close(), "close")
	}

	born := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "v179-new" })
	got, err := db.GetOrder(born.ID)
	got = testutil.Must(t, got, err, "read the new order")
	if got.BlockActions == nil || len(got.BlockActions) != 0 {
		t.Errorf("an order born after v179 reads BlockActions %v, want empty and not nil", got.BlockActions)
	}
	if _, err := db.Exec(`UPDATE load_sequences SET lift_at = 4 WHERE name = 'Child cart interlock'`); err == nil {
		t.Error("lift_at 4 on a four-name sequence was accepted, want the CHECK to refuse it")
	}
	if _, err := db.Exec(`UPDATE load_sequences SET lift_at = -1 WHERE name = 'Child cart interlock'`); err == nil {
		t.Error("lift_at -1 was accepted, want the CHECK to refuse it")
	}
}

// TestBlockRecord_TheCreateReplacesAndTheAppendMerges reads both writers back.
// The create's vendor write replaces the record with the blocks it sent; the
// append's claim merges its segment's blocks in, and records nothing when the
// claim refuses. An order sent before the record (NULL) stays NULL through an
// append, so it is read by its block names for its whole life.
func TestBlockRecord_TheCreateReplacesAndTheAppendMerges(t *testing.T) {
	db := testdb.Open(t)
	live := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rec-live"; o.StepsJSON = `[{"action":"wait"}]` })
	old := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "rec-old"; o.StepsJSON = `[{"action":"wait"}]` })
	_, err := db.Exec(`UPDATE orders SET block_actions = NULL WHERE id = $1`, old.ID)
	testutil.MustNoErr(t, err, "the order sent before the record")
	read := func(id int64) map[string]string {
		t.Helper()
		o, err := db.GetOrder(id)
		return testutil.Must(t, o, err, "read the order").BlockActions
	}

	testutil.MustNoErr(t, db.NameFleetOrder(live.ID, "V-live", "CREATED", "", map[string]string{"V-live-b1": "pickup", "V-live-b2": "wait"}),
		"the create's write")
	if got := read(live.ID); len(got) != 2 || got["V-live-b1"] != "pickup" || got["V-live-b2"] != "wait" {
		t.Fatalf("after the create the record is %v, want the two blocks it sent", got)
	}
	owed, err := db.ClaimAppend(live.ID, 1, `[{"action":"wait"}]`, map[string]string{"V-live-b3": "dropoff"})
	testutil.MustNoErr(t, err, "a refused claim")
	if owed || len(read(live.ID)) != 2 {
		t.Errorf("a claim at the wrong wait index answered %v and left %v, want refused and the record unchanged", owed, read(live.ID))
	}
	owed, err = db.ClaimAppend(live.ID, 0, `[{"action":"wait"}]`, map[string]string{"V-live-b3": "dropoff", "V-live-b4": ""})
	testutil.MustNoErr(t, err, "the claim")
	if got := read(live.ID); !owed || len(got) != 4 || got["V-live-b3"] != "dropoff" || got["V-live-b4"] != "" || got["V-live-b1"] != "pickup" {
		t.Errorf("after the append (owed %v) the record is %v, want the create's two and the segment's two", owed, got)
	}

	owed, err = db.ClaimAppend(old.ID, 0, `[{"action":"wait"}]`, map[string]string{"V-old-b3": "dropoff"})
	testutil.MustNoErr(t, err, "the old order's claim")
	var raw sql.NullString
	testutil.MustNoErr(t, db.QueryRow(`SELECT block_actions::text FROM orders WHERE id=$1`, old.ID).Scan(&raw), "read the column")
	if !owed || raw.Valid {
		t.Errorf("the old order's claim answered %v and left block_actions %q, want claimed and NULL", owed, raw.String)
	}

	// An order born before the record and sent after it (it queued across the
	// deploy) is recorded by its create: every block of the new vendor order is
	// sent now, so the create's write replaces the NULL.
	testutil.MustNoErr(t, db.NameFleetOrder(old.ID, "V-old2", "CREATED", "", map[string]string{"V-old2-b1": "pickup"}),
		"the create of an order born before the record")
	if got := read(old.ID); len(got) != 1 || got["V-old2-b1"] != "pickup" {
		t.Errorf("an order sent after the record reads %v, want the one block its create sent", got)
	}
}
