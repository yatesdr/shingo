//go:build docker

package plantclaims_test

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
)

// TestListProcessNodeOptions_APairedPositionIsAPosition: a two-robot press-index
// claim's paired and second paired nodes are offered beside its core node, once
// each across styles, and a blank leg offers nothing.
func TestListProcessNodeOptions_APairedPositionIsAPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB
	for _, q := range []string{
		`INSERT INTO nodes (name) VALUES ('PLN_A'), ('PLN_B'), ('PLN_C'), ('WLD_A')`,
		`INSERT INTO style_claims (process_id, style_id, core_node_name, paired_core_node, second_paired_core_node, role, swap_mode) VALUES
			('PRESS', 'S1', 'PLN_A', 'PLN_B', 'PLN_C', 'produce', 'two_robot_press_index'),
			('PRESS', 'S2', 'PLN_A', 'PLN_B', '', 'produce', 'two_robot_press_index'),
			('WELD', 'S3', 'WLD_A', '', '', 'produce', 'two_robot')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	opts, err := plantclaims.ListProcessNodeOptions(db)
	testutil.MustNoErr(t, err, "ListProcessNodeOptions")
	got := map[string][]string{}
	for _, o := range opts {
		for _, n := range o.Nodes {
			got[o.ProcessID] = append(got[o.ProcessID], n.Name)
		}
	}
	want := map[string][]string{"PRESS": {"PLN_A", "PLN_B", "PLN_C"}, "WELD": {"WLD_A"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options = %v, want %v", got, want)
	}
}
