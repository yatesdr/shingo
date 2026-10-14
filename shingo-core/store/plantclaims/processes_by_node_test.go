//go:build docker

package plantclaims_test

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
)

// TestProcessesByNode: a node maps to every process whose setup names it,
// from the claims and from the positions no claim names (a loader window),
// sorted and each once.
func TestProcessesByNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB
	for _, q := range []string{
		`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode) VALUES
			('SNF2', 'S1', 'ALN_001', 'consume', 'two_robot'),
			('SNF2', 'S2', 'ALN_001', 'consume', 'two_robot'),
			('SNF3', 'S3', 'SMN_001', 'consume', 'two_robot'),
			('SNF4', 'S4', 'SMN_001', 'consume', 'two_robot')`,
		`INSERT INTO process_positions (process_id, core_node_name) VALUES ('Bin Loader', 'SMN_015')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := plantclaims.ProcessesByNode(db)
	testutil.MustNoErr(t, err, "ProcessesByNode")
	want := map[string][]string{
		"ALN_001": {"SNF2"},
		"SMN_001": {"SNF3", "SNF4"},
		"SMN_015": {"Bin Loader"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProcessesByNode = %v, want %v", got, want)
	}
}
