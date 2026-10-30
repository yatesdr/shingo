package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// THE RECIPE MIGRATION COLLAPSES ONE-NODE ROLES, KEEPS THE REST, AND RUNS TWICE
// THE SAME. A recipe saved before the rule names its destination, and it may name
// one Settings has since moved off: that is the plant's shape, and the case that
// put red text on the card. No row takes a new version.
func TestUpgradeCollapsesARecipesOneNodeRoles(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	testutil.MustNoErr(t, err, "open")
	t.Cleanup(func() { db.Close() })

	pid, err := db.CreateProcess("UPG-PRESS", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "UPG-SRC", Role: domain.RoutingRoleSource, Enabled: true},
		{ProcessID: pid, CoreNodeName: "UPG-DST-1", Role: domain.RoutingRoleDestination, Enabled: true},
		{ProcessID: pid, CoreNodeName: "UPG-DST-2", Role: domain.RoutingRoleDestination, Enabled: true},
	} {
		_, err := db.UpsertRoutingNode(domain.CoreNodeKinds{}, in)
		testutil.MustNoErr(t, err, "routing "+in.CoreNodeName)
	}
	insert := func(name, flow string) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO flow_presets (process_id, name, version, flow_json, created_by) VALUES (?, ?, 1, ?, 'seed')`, pid, name, flow)
		testutil.MustNoErr(t, err, "seed "+name)
		id, err := res.LastInsertId()
		testutil.MustNoErr(t, err, "id")
		return id
	}
	const withGroups = `{"cells":[{"core_node_name":"UPG-P1","role":"produce","swap_mode":"two_robot_press_index","payload_code":"","inbound_source":"UPG-OLD-SRC","outbound_destination":"UPG-DST-2"}],` +
		`"groups":[{"of":"UPG-P1","name":"G","spots":[{"core_node_name":"UPG-P1"}]}]}`
	collapsed := insert("Collapses", withGroups)
	const orphan = `{"cells":[{"core_node_name":"UPG-P1","role":"produce","swap_mode":"two_robot_press_index","payload_code":"","outbound_destination":"UPG-GONE"}]}`
	orphaned := insert("Orphaned", orphan)
	const broken = `not json`
	unparsed := insert("Broken", broken)

	for pass := 1; pass <= 2; pass++ {
		tx, err := db.Begin()
		testutil.MustNoErr(t, err, "begin")
		testutil.MustNoErr(t, collapseRecipeRoutingOnce(tx), "migrate")
		testutil.MustNoErr(t, tx.Commit(), "commit")

		p, err := db.GetFlowPreset(collapsed)
		testutil.MustNoErr(t, err, "read")
		cells, groups, err := domain.ParseRecipe(p.FlowJSON)
		testutil.MustNoErr(t, err, "parse")
		if cells[0].InboundSource != domain.RecipeFollowsSource {
			t.Errorf("pass %d: source = %q, want the role: the set has one, whatever the old name was", pass, cells[0].InboundSource)
		}
		if cells[0].OutboundDestination != "UPG-DST-2" {
			t.Errorf("pass %d: destination = %q, want the name: the set has two", pass, cells[0].OutboundDestination)
		}
		if len(groups) != 1 || groups[0].Name != "G" {
			t.Errorf("pass %d: groups = %+v, want them as they were", pass, groups)
		}
		if p.Version != 1 {
			t.Errorf("pass %d: version = %d, a migration takes no version", pass, p.Version)
		}
		if p, err := db.GetFlowPreset(orphaned); err != nil || p.FlowJSON != orphan {
			t.Errorf("pass %d: the orphan moved: %v %q", pass, err, p.FlowJSON)
		}
		if p, err := db.GetFlowPreset(unparsed); err != nil || p.FlowJSON != broken {
			t.Errorf("pass %d: the row that does not parse moved: %v %q", pass, err, p.FlowJSON)
		}
	}
}
