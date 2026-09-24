package store

import (
	"testing"
)

// The one-bucket-key migration (SYNTH-round2 S4, citrine §7): the Edge's bucket
// seq rows move from "<nodeID>|pair|style|payload" to
// "<core_node_name>|pair|style|payload", the key Core's dedup row already has.
//
// Two node ids that share a core_node_name collapse to ONE row. The survivor
// keeps MAX(next_seq), so the merged stream's next seq is above anything Core
// applied from either, and SUM(net) as its net. An old-shape row's net is
// always 0 (no build that writes the old key writes a net), so for the
// collision this is 0: no net has been sent for the scope, every Core row for
// it is NULL-anchored, and the first net-bearing message applies its delta.
func TestRekeyBucketDeltaSeq(t *testing.T) {
	db := testDB(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO processes (id, name) VALUES (1, 'P-A'), (2, 'P-B')`)
	mustExec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name) VALUES
		(34, 1, 'SMN-TEST', 'smn-a', 'SMN-TEST'),
		(45, 2, 'SMN-TEST', 'smn-b', 'SMN-TEST'),
		(7,  1, 'ALN-TEST', 'aln', 'ALN-TEST'),
		(8,  1, '',         'blank', 'BLANK')`)
	mustExec(`INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net) VALUES
		('bucket', '34|L1|U1|100|PART-G', 0, 5, 0),
		('bucket', '45|L1|U1|100|PART-G', 0, 3, 0),
		('bucket', '7|L1|U1|100|PART-H', 0, 9, 0),
		('bucket', '999|L1|U1|100|PART-X', 0, 4, 0),
		('bucket', '8|L1|U1|100|PART-Y', 0, 2, 0),
		('bin', '42', 1, 6, -12)`)

	for run := 1; run <= 2; run++ {
		if err := db.rekeyBucketDeltaSeq(); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		rows, err := db.Query(`SELECT scope_kind, scope_key, epoch, next_seq, net
			FROM inventory_delta_seq ORDER BY scope_kind, scope_key`)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		type row struct {
			kind, key       string
			epoch, seq, net int64
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.kind, &r.key, &r.epoch, &r.seq, &r.net); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, r)
		}
		rows.Close()
		want := []row{
			{"bin", "42", 1, 6, -12},                    // bins untouched
			{"bucket", "8|L1|U1|100|PART-Y", 0, 2, 0},   // blank core name: left as it was
			{"bucket", "999|L1|U1|100|PART-X", 0, 4, 0}, // no such node: left as it was
			{"bucket", "ALN-TEST|L1|U1|100|PART-H", 0, 9, 0},
			{"bucket", "SMN-TEST|L1|U1|100|PART-G", 0, 5, 0}, // the collision: MAX(5, 3)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: rows = %+v, want %+v", run, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("run %d row %d = %+v, want %+v", run, i, got[i], want[i])
			}
		}
	}
}

// A bucket key already on the new shape that ALSO has an old-shape twin merges
// the same way. That happens only if an older build ran after this migration
// (a downgrade) and wrote the old key again. The new-shape row's net is what
// Core anchored applied_net to (the older build's messages carried no net, and
// a message with no net leaves the anchor where it was), so it is kept: SUM(net)
// adds the old row's 0 to it.
func TestRekeyBucketDeltaSeq_MergesWithAnExistingNewKey(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO processes (id, name) VALUES (1, 'P-A')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name)
		VALUES (34, 1, 'SMN-TEST', 'smn-a', 'SMN-TEST')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net) VALUES
		('bucket', '34|L1|U1|100|PART-G', 0, 5, 0),
		('bucket', 'SMN-TEST|L1|U1|100|PART-G', 0, 7, 11)`); err != nil {
		t.Fatal(err)
	}
	if err := db.rekeyBucketDeltaSeq(); err != nil {
		t.Fatal(err)
	}
	var n, seq, net int64
	if err := db.QueryRow(`SELECT COUNT(*), MAX(next_seq), MAX(net) FROM inventory_delta_seq
		WHERE scope_kind='bucket'`).Scan(&n, &seq, &net); err != nil {
		t.Fatal(err)
	}
	if n != 1 || seq != 7 || net != 11 {
		t.Errorf("rows/next_seq/net = %d/%d/%d, want 1/7/11", n, seq, net)
	}
}
