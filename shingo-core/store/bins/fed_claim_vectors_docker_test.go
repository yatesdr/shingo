//go:build docker

package bins_test

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/fedshape"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// TestFedClaimSQL_TheSharedShapeTable: Core's SQL shape test of a press fed
// from behind answers every case of the table the Edge's Go answers too
// (shared/fedshape). Each case is its own process.
func TestFedClaimSQL_TheSharedShapeTable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	for i, c := range fedshape.MustLoad() {
		process := fmt.Sprintf("FSV-%d", i)
		for _, cl := range c.Claims {
			_, err := db.Exec(`INSERT INTO style_claims
				(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node, second_paired_core_node)
				VALUES ($1, 'S1', $2, $3, '', 'FSV-PART', $4, $5)`, process, cl.Node, cl.Role, cl.Paired, cl.SecondPaired)
			testutil.MustNoErr(t, err, c.Name+": claim "+cl.Node)
		}
		var fed bool
		testutil.MustNoErr(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM style_claims sc
			WHERE sc.process_id = $1 AND sc.core_node_name = $2 AND `+bins.FedClaimSQL("sc")+`)`,
			process, c.Ask).Scan(&fed), c.Name)
		if fed != c.WantFed {
			t.Errorf("%s: FedClaimSQL = %v, want %v\nwhy this case exists: %s", c.Name, fed, c.WantFed, c.Why)
		}
	}
}
