//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestBootReport_NamesANodeWithNoName (docket B2): Core names every node with
// no name at start-up, beside the other setups it reports (reportSetups): its
// id, its parent and the bins standing on it, and changes nothing. The store
// refuses to write such a row now (nodes.ErrNameBlank), so the test leaves a
// lane slot with a bin on it nameless by raw SQL, as a row saved before the
// guard would be.
func TestBootReport_NamesANodeWithNoName(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, payload := testdb.SetupTestData(t, db)
	grpID, err := db.CreateNodeGroup("BOOT-GRP")
	testutil.MustNoErr(t, err, "create the group")
	testdb.MarkGroup(t, db, grpID)
	laneID, err := db.AddLane(grpID, "BOOT-LANE")
	testutil.MustNoErr(t, err, "add the lane")
	depth := 1
	slot := &nodes.Node{Name: "BOOT-SLOT", ParentID: &laneID, Enabled: true, Depth: &depth}
	testutil.MustNoErr(t, db.CreateNode(slot), "create the slot")
	testdb.CreateBinAtNode(t, db, payload.Code, slot.ID, "BOOT-BIN")
	_, err = db.Exec(`UPDATE nodes SET name = '' WHERE id = $1`, slot.ID)
	testutil.MustNoErr(t, err, "leave the slot with no name")

	// Unstarted: the test replaces logFn, which no goroutine may be reading.
	eng := newUnstartedEngine(t, db, simulator.New())
	var said []string
	eng.logFn = func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) }
	eng.reportSetups()

	var lines []string
	for _, l := range said {
		if strings.Contains(l, "have no name") {
			lines = append(lines, l)
		}
	}
	want := fmt.Sprintf(`node %d (parent %d "BOOT-LANE"; bins BOOT-BIN)`, slot.ID, laneID)
	if len(lines) != 1 || !strings.Contains(lines[0], "1 node(s) have no name") || !strings.Contains(lines[0], want) {
		t.Fatalf("the boot report said %q about nodes with no name, want one line naming %s", lines, want)
	}

	got, err := db.GetNode(slot.ID)
	testutil.MustNoErr(t, err, "re-read the slot")
	if got.Name != "" || got.ParentID == nil || *got.ParentID != laneID {
		t.Errorf("after the report the slot reads name %q, parent %v; the report changes nothing", got.Name, got.ParentID)
	}
}
