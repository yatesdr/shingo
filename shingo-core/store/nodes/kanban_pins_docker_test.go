//go:build docker

package nodes_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestKanbanPin_AStrictGroupFencesABlankProcess: the disposition side of the
// fence (GroupFencesAsker; the query side is TestFence_StrictGroupHidesFromOutsidersAndNotFromSupported's
// nameless asker): an ask naming no process is an outsider at a strict group,
// supported process or not, and at a group not strict nobody is.
func TestKanbanPin_AStrictGroupFencesABlankProcess(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	strict, err := nodes.CreateGroup(db.DB, "KPF-STRICT")
	testutil.MustNoErr(t, err, "strict group")
	testdb.MarkGroup(t, db, strict)
	testutil.MustNoErr(t, db.SetNodeProperty(strict, nodes.PropStrictSourcing, "on"), "strict on")
	open, err := nodes.CreateGroup(db.DB, "KPF-OPEN")
	testutil.MustNoErr(t, err, "open group")
	testdb.MarkGroup(t, db, open)
	proc := &nodes.Node{Name: "KPF-PROC", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, proc), "process node")
	testutil.MustNoErr(t, nodes.SetMaintainSupports(db.DB, strict, []int64{proc.ID}), "supports")

	for _, tc := range []struct {
		name  string
		group int64
		asker string
		want  bool
	}{
		{"blank at strict", strict, "", true},
		{"outsider at strict", strict, "KPF-ELSE", true},
		{"supported at strict", strict, "KPF-PROC", false},
		{"blank at a group not strict", open, "", false},
	} {
		got, err := nodes.GroupFencesAsker(db.DB, tc.group, tc.asker)
		testutil.MustNoErr(t, err, tc.name)
		if got != tc.want {
			t.Errorf("%s: fenced = %v, want %v", tc.name, got, tc.want)
		}
	}
}
