package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// Edge v6: the FG light-curtain interlock moves from processes to
// process_nodes. The pattern is TestMigration_DropsDropViaStagingColumn's:
// open at today's schema, put the old shape back by hand (and forget v6 in
// schema_migrations, as an Edge that never ran it), re-open, and read what the
// runner did.

func columnCount(t *testing.T, db *DB, table string, cols ...string) int {
	t.Helper()
	n := 0
	for _, c := range cols {
		has, err := db.tableHasColumn(table, c)
		testutil.MustNoErr(t, err, "probe "+table+"."+c)
		if has {
			n++
		}
	}
	return n
}

func TestMigration_CurtainMovesToNodes(t *testing.T) {
	t.Parallel()

	t.Run("fresh_db_has_node_columns_and_no_process_columns", func(t *testing.T) {
		db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
		if err != nil {
			t.Fatalf("open fresh db: %v", err)
		}
		defer db.Close()
		if n := columnCount(t, db, "processes", processCurtainColumns...); n != 0 {
			t.Errorf("fresh processes carries %d curtain columns, want 0", n)
		}
		if n := columnCount(t, db, "process_nodes", processCurtainColumns...); n != 4 {
			t.Errorf("fresh process_nodes carries %d curtain columns, want 4", n)
		}
	})

	t.Run("hk_shaped_db_gains_the_node_columns_and_nothing_else", func(t *testing.T) {
		// An Edge that never had the per-process columns (HK's build
		// predates them): no process columns and no node columns.
		dbPath := filepath.Join(t.TempDir(), "hk.db")
		db, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		for _, stmt := range []string{
			"ALTER TABLE process_nodes DROP COLUMN curtain_enabled",
			"ALTER TABLE process_nodes DROP COLUMN curtain_plc_name",
			"ALTER TABLE process_nodes DROP COLUMN curtain_tag_name",
			"ALTER TABLE process_nodes DROP COLUMN curtain_safe_value",
			"DELETE FROM schema_migrations WHERE version >= 6",
		} {
			_, err := db.Exec(stmt)
			testutil.MustNoErr(t, err, stmt)
		}
		res, err := db.Exec(`INSERT INTO processes (name) VALUES ('SYN-PRESS-HK')`)
		testutil.MustNoErr(t, err, "insert process")
		procID, err := res.LastInsertId()
		testutil.MustNoErr(t, err, "process id")
		res, err = db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name, sequence)
			VALUES (?, 'SYN-PLN-3', 'SYN-PLN-3', 'SYN-PLN-3', 1)`, procID)
		testutil.MustNoErr(t, err, "insert node")
		nodeID, err := res.LastInsertId()
		testutil.MustNoErr(t, err, "node id")
		testutil.MustNoErr(t, db.Close(), "close")

		for boot := 1; boot <= 2; boot++ {
			db, err = Open(dbPath)
			if err != nil {
				t.Fatalf("boot %d: %v", boot, err)
			}
			if n := columnCount(t, db, "process_nodes", processCurtainColumns...); n != 4 {
				t.Errorf("boot %d: process_nodes carries %d curtain columns, want 4", boot, n)
			}
			if n := columnCount(t, db, "processes", processCurtainColumns...); n != 0 {
				t.Errorf("boot %d: processes carries %d curtain columns, want 0", boot, n)
			}
			n, err := db.GetProcessNode(nodeID)
			testutil.MustNoErr(t, err, "get node")
			if n.CurtainEnabled || n.CurtainPLCName != "" || n.CurtainTagName != "" || n.CurtainSafeValue != nil {
				t.Errorf("boot %d: node carries an interlock nobody configured: %+v", boot, n)
			}
			testutil.MustNoErr(t, db.Close(), "close")
		}
	})

	t.Run("upgraded_db_carries_the_live_interlock_to_its_produce_nodes", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "legacy.db")
		db, err := Open(dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		// Put the old shape back: no node columns, and the four process
		// columns exactly as the retired v40 ALTERs declared them.
		for _, stmt := range []string{
			"ALTER TABLE process_nodes DROP COLUMN curtain_enabled",
			"ALTER TABLE process_nodes DROP COLUMN curtain_plc_name",
			"ALTER TABLE process_nodes DROP COLUMN curtain_tag_name",
			"ALTER TABLE process_nodes DROP COLUMN curtain_safe_value",
			"ALTER TABLE processes ADD COLUMN curtain_enabled INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE processes ADD COLUMN curtain_plc_name TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE processes ADD COLUMN curtain_tag_name TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE processes ADD COLUMN curtain_safe_value INTEGER NOT NULL DEFAULT 1",
			"DELETE FROM schema_migrations WHERE version >= 6",
		} {
			_, err := db.Exec(stmt)
			testutil.MustNoErr(t, err, stmt)
		}

		// seedProcess builds a process with one style and the given
		// (core node, role) claims, a process_node for each, and the old
		// interlock columns as given. Nodes are created through raw SQL
		// because the store's node scan now names the node columns, which
		// this database does not have yet.
		type claimSpec struct {
			node string
			role protocol.ClaimRole
		}
		seedProcess := func(name string, enabled bool, plcName, tagName string, safe int, claims ...claimSpec) (procID int64, nodeIDs map[string]int64) {
			t.Helper()
			res, err := db.Exec(`INSERT INTO processes (name, curtain_enabled, curtain_plc_name, curtain_tag_name, curtain_safe_value)
				VALUES (?, ?, ?, ?, ?)`, name, enabled, plcName, tagName, safe)
			testutil.MustNoErr(t, err, "insert process "+name)
			procID, err = res.LastInsertId()
			testutil.MustNoErr(t, err, "process id")
			styleID, err := db.CreateStyle(name+"-STYLE", "", procID)
			testutil.MustNoErr(t, err, "create style")
			nodeIDs = map[string]int64{}
			for i, c := range claims {
				_, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
					StyleID: styleID, CoreNodeName: c.node, Role: c.role,
					SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-X",
					OutboundDestination: "SYN-FG-OUT", InboundStaging: "SYN-STG-1",
				})
				testutil.MustNoErr(t, err, "seed claim "+c.node)
				res, err := db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name, sequence)
					VALUES (?, ?, ?, ?, ?)`, procID, c.node, c.node, c.node, i+1)
				testutil.MustNoErr(t, err, "insert node "+c.node)
				nodeIDs[c.node], err = res.LastInsertId()
				testutil.MustNoErr(t, err, "node id")
			}
			return procID, nodeIDs
		}
		// SPR-shaped: the interlock ON, polarity FALSE, two FG produce nodes
		// and one consume node.
		_, live := seedProcess("SYN-PRESS-LIVE", true, "SYN-PLC", "SYN_CURTAIN_OK", 0,
			claimSpec{"SYN-PLN-1", protocol.ClaimRoleProduce},
			claimSpec{"SYN-PLN-2", protocol.ClaimRoleProduce},
			claimSpec{"SYN-ALN-1", protocol.ClaimRoleConsume})
		// Configured once and switched off: pointers set, the old default
		// polarity TRUE nobody can be shown to have chosen.
		_, off := seedProcess("SYN-PRESS-OFF", false, "SYN-PLC", "SYN_CURTAIN_OFF", 1,
			claimSpec{"SYN-PLN-9", protocol.ClaimRoleProduce})
		// Never configured: every column at its old default.
		_, bare := seedProcess("SYN-PRESS-BARE", false, "", "", 1,
			claimSpec{"SYN-PLN-7", protocol.ClaimRoleProduce})

		testutil.MustNoErr(t, db.Close(), "close")
		db, err = Open(dbPath)
		if err != nil {
			t.Fatalf("re-open: %v", err)
		}

		if n := columnCount(t, db, "processes", processCurtainColumns...); n != 0 {
			t.Errorf("upgraded processes still carries %d curtain columns", n)
		}
		f := false
		want := map[int64]struct {
			enabled  bool
			plc, tag string
			safe     *bool
		}{
			live["SYN-PLN-1"]: {true, "SYN-PLC", "SYN_CURTAIN_OK", &f},
			live["SYN-PLN-2"]: {true, "SYN-PLC", "SYN_CURTAIN_OK", &f},
			live["SYN-ALN-1"]: {false, "", "", nil},
			off["SYN-PLN-9"]:  {false, "SYN-PLC", "SYN_CURTAIN_OFF", nil},
			bare["SYN-PLN-7"]: {false, "", "", nil},
		}
		for id, w := range want {
			n, err := db.GetProcessNode(id)
			testutil.MustNoErr(t, err, "get node")
			gotSafe, wantSafe := "nil", "nil"
			if n.CurtainSafeValue != nil {
				gotSafe = fmt.Sprint(*n.CurtainSafeValue)
			}
			if w.safe != nil {
				wantSafe = fmt.Sprint(*w.safe)
			}
			if n.CurtainEnabled != w.enabled || n.CurtainPLCName != w.plc || n.CurtainTagName != w.tag || gotSafe != wantSafe {
				t.Errorf("node %s = enabled:%v plc:%q tag:%q safe:%s; want enabled:%v plc:%q tag:%q safe:%s",
					n.Name, n.CurtainEnabled, n.CurtainPLCName, n.CurtainTagName, gotSafe,
					w.enabled, w.plc, w.tag, wantSafe)
			}
		}

		// A second boot changes nothing: the node rows byte-compare.
		snapshot := func() string {
			t.Helper()
			rows, err := db.Query(`SELECT id, curtain_enabled, curtain_plc_name, curtain_tag_name,
				quote(curtain_safe_value), updated_at FROM process_nodes ORDER BY id`)
			testutil.MustNoErr(t, err, "snapshot")
			defer rows.Close()
			var b strings.Builder
			for rows.Next() {
				var id int64
				var en int
				var plc, tag, safe, upd string
				testutil.MustNoErr(t, rows.Scan(&id, &en, &plc, &tag, &safe, &upd), "scan")
				fmt.Fprintf(&b, "%d|%d|%s|%s|%s|%s\n", id, en, plc, tag, safe, upd)
			}
			return b.String()
		}
		before := snapshot()
		testutil.MustNoErr(t, db.Close(), "close")
		db, err = Open(dbPath)
		if err != nil {
			t.Fatalf("second re-open: %v", err)
		}
		defer db.Close()
		if after := snapshot(); after != before {
			t.Errorf("second boot changed the node rows:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if n := columnCount(t, db, "processes", processCurtainColumns...); n != 0 {
			t.Errorf("second boot re-added %d process curtain columns", n)
		}
	})
}
