//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/sourceability"
)

// D2.5 · presence, read in the pool's own statement: a bin counts present free,
// claimed, reserved or riding a robot; not retired, not on a disabled node. The
// same rule the preflight's Absent answers (bins.BinPresentSQL).
func TestBuildInputs_PresenceCountsSpokenForStockAndNotDeadStock(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB
	std := testdb.SetupStandardData(t, sdb)
	mk := func(name string, enabled, synthetic bool) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, sdb.CreateNode(n), name)
		_, err := db.Exec(`UPDATE nodes SET enabled = $2, is_synthetic = $3 WHERE id = $1`, n.ID, enabled, synthetic)
		testutil.MustNoErr(t, err, "node flags")
		return n
	}
	robot, dead := mk("ABS-ROBOT", true, true), mk("ABS-DEAD", false, false)

	testdb.CreateBinAtNode(t, sdb, "P-FREE", std.StorageNode.ID, "abs-free")
	claimed := testdb.CreateBinAtNode(t, sdb, "P-CLAIMED", std.StorageNode.ID, "abs-claimed")
	testdb.CreateBinAtNode(t, sdb, "P-RIDING", robot.ID, "abs-riding")
	retired := testdb.CreateBinAtNode(t, sdb, "P-RETIRED", std.StorageNode.ID, "abs-retired")
	testdb.CreateBinAtNode(t, sdb, "P-DEAD", dead.ID, "abs-dead")
	_, err := db.Exec(`UPDATE bins SET status = 'retired' WHERE id = $1`, retired.ID)
	testutil.MustNoErr(t, err, "retire")
	_, err = db.Exec(`UPDATE bins SET claimed_by = 1 WHERE id = $1`, claimed.ID)
	testutil.MustNoErr(t, err, "claim")

	in, err := sourceability.BuildInputs(db, time.Hour)
	testutil.MustNoErr(t, err, "build inputs")
	for p, want := range map[string]int{"P-FREE": 1, "P-CLAIMED": 1, "P-RIDING": 1, "P-RETIRED": 0, "P-DEAD": 0} {
		if in.Present[p] != want {
			t.Errorf("Present[%s] = %d, want %d", p, in.Present[p], want)
		}
	}
	if in.Pool["P-CLAIMED"] != 0 || in.Pool["P-RIDING"] != 0 || in.Pool["P-FREE"] != 1 {
		t.Errorf("the pool moved: %v; presence is a third aggregate and the pool answers as before", in.Pool)
	}
}
