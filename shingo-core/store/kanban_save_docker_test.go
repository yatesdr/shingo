//go:build docker

package store_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// kanban_save_docker_test.go — the one write: one audit row per changed field or
// list entry, none for what did not change, and all of it or none of it.

// kanbanAudit is a kanban's audit rows as "action old>new@actor", sorted.
func kanbanAudit(t *testing.T, db *store.DB, id int64) []string {
	t.Helper()
	rows, err := db.ListEntityAudit("node", id)
	testutil.MustNoErr(t, err, "audit")
	out := []string{}
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s %s>%s@%s", r.Action, r.OldValue, r.NewValue, r.Actor))
	}
	sort.Strings(out)
	return out
}

func TestSaveKanban_AuditsEachChangeOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	k := testdb.MarkedGroup(t, db, "SK-K")
	spill, other := testdb.MarkedGroup(t, db, "SK-SPILL"), testdb.MarkedGroup(t, db, "SK-OTHER")
	press := &nodes.Node{Name: "SK-PRESS", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "press")
	var small, big int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('SK-SMALL', 8) RETURNING id`).Scan(&small), "small")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('SK-BIG') RETURNING id`).Scan(&big), "big")

	station, strict, cover, want := "edge.1", true, 4, 2
	wants := []store.MaintainLayerWant{{Layers: 0, Want: 1}, {Layers: 3, Want: 2}}
	first := store.KanbanChange{NodeID: *k, EnsureRow: true, Station: &station, Strict: &strict, Cover: &cover, Overflow: spill,
		Levels: []store.KanbanLevelChange{
			{BinTypeID: small, Want: &want, LayerWants: &wants, Overflow: other},
			{BinTypeID: big, Want: &want},
		},
		Supports: &[]int64{press.ID},
		Sources:  &[]int64{*spill, *other},
	}
	testutil.MustNoErr(t, db.SaveKanban(first, "stephen"), "first save")
	wantRows := []string{
		"kanban:level:SK-BIG >2@stephen",
		"kanban:level:SK-SMALL >2@stephen",
		"kanban:level_overflow:SK-SMALL >SK-OTHER@stephen",
		"kanban:layer_want:SK-SMALL:0 >1@stephen",
		"kanban:layer_want:SK-SMALL:3 >2@stephen",
		"kanban:overflow >SK-SPILL@stephen",
		"kanban:running_cover 0>4@stephen",
		"kanban:source:SK-OTHER >2@stephen",
		"kanban:source:SK-SPILL >1@stephen",
		"kanban:station >edge.1@stephen",
		"kanban:strict off>on@stephen",
		"kanban:support >SK-PRESS@stephen",
	}
	sort.Strings(wantRows)
	if got := kanbanAudit(t, db, *k); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("first save's audit:\n got %v\nwant %v", got, wantRows)
	}

	// The same save again changes nothing and says nothing.
	testutil.MustNoErr(t, db.SaveKanban(first, "stephen"), "same save")
	if got := kanbanAudit(t, db, *k); len(got) != len(wantRows) {
		t.Errorf("a save that changed nothing wrote %d audit rows:\n%v", len(got)-len(wantRows), got)
	}

	// A list entry that moves, one that goes: a row each. The level's removal takes
	// its layer wants and overflow with it, each named.
	before := len(wantRows)
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *k,
		RemoveLevels: []int64{small},
		Supports:     &[]int64{},
		Sources:      &[]int64{*other},
	}, "ann"), "second save")
	got := kanbanAudit(t, db, *k)
	for _, row := range []string{
		"kanban:level:SK-SMALL 2>@ann",
		"kanban:level_overflow:SK-SMALL SK-OTHER>@ann",
		"kanban:layer_want:SK-SMALL:0 1>@ann",
		"kanban:layer_want:SK-SMALL:3 2>@ann",
		"kanban:support SK-PRESS>@ann",
		"kanban:source:SK-SPILL 1>@ann",
		"kanban:source:SK-OTHER 2>1@ann",
	} {
		if !contains(got, row) {
			t.Errorf("no audit row %q in %v", row, got)
		}
	}
	if len(got) != before+7 {
		t.Errorf("second save wrote %d rows, want 7: %v", len(got)-before, got)
	}
	sources, err := db.ListKanbanSources(*k)
	testutil.MustNoErr(t, err, "sources")
	if len(sources) != 1 || sources[0].SourceNodeID != *other || sources[0].Rank != 1 {
		t.Errorf("sources = %+v, want the survivor at rank 1", sources)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A plain group stays plain: its overflow is a property with the property's audit
// row. Its first row takes the property with it, and one that does not resolve is
// gone with the row, since a NULL parks the push as it did.
func TestSaveKanban_APlainGroupAndItsFirstRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	spill := testdb.MarkedGroup(t, db, "SKP-SPILL")
	p := testdb.MarkedGroup(t, db, "SKP-PLAIN")

	name := "SKP-SPILL"
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *p, PlainOverflow: &name}, "stephen"), "plain overflow")
	if k, err := db.GetKanban(*p); err != nil || k != nil {
		t.Fatalf("a plain save made a row: %+v (%v)", k, err)
	}
	if got := db.GetNodeProperty(*p, nodes.PropOverflowDestination); got != name {
		t.Errorf("property = %q", got)
	}
	if got := kanbanAudit(t, db, *p); !reflect.DeepEqual(got, []string{"property:overflow_destination >SKP-SPILL@stephen"}) {
		t.Errorf("audit = %v", got)
	}
	target, err := db.OverflowTargetOf(*p)
	testutil.MustNoErr(t, err, "target")
	if target.NodeID != 0 || target.Name != name {
		t.Errorf("target = %+v, want the property's name", target)
	}

	var bt int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('SKP-T') RETURNING id`).Scan(&bt), "bin type")
	want := 1
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *p, EnsureRow: true,
		Levels: []store.KanbanLevelChange{{BinTypeID: bt, Want: &want}}}, "stephen"), "first level")
	k, err := db.GetKanban(*p)
	testutil.MustNoErr(t, err, "kanban")
	if k == nil || k.OverflowNodeID == nil || *k.OverflowNodeID != *spill {
		t.Errorf("kanban = %+v, want the property's node on the row", k)
	}
	if got := db.GetNodeProperty(*p, nodes.PropOverflowDestination); got != "" {
		t.Errorf("property = %q after the first row; a group has one or the other", got)
	}
	target, err = db.OverflowTargetOf(*p)
	testutil.MustNoErr(t, err, "target")
	if target.NodeID != *spill || target.Name != "SKP-SPILL" {
		t.Errorf("target = %+v, want the row's node", target)
	}

	dangling := testdb.MarkedGroup(t, db, "SKP-DANGLING")
	testutil.MustNoErr(t, db.SetNodeProperty(*dangling, nodes.PropOverflowDestination, "SKP-NOWHERE"), "dangling")
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *dangling, EnsureRow: true}, "stephen"), "row")
	k, err = db.GetKanban(*dangling)
	testutil.MustNoErr(t, err, "kanban")
	if k == nil || k.OverflowNodeID != nil {
		t.Errorf("kanban = %+v, want NULL", k)
	}
	if got := db.GetNodeProperty(*dangling, nodes.PropOverflowDestination); got != "" {
		t.Errorf("property = %q", got)
	}
}

// All of it or none of it: a save that fails part way leaves every row and every
// audit row as they were. The failing part here is a foreign key:
// a source that does not exist.
func TestSaveKanban_OneTransaction(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	k := testdb.MarkedGroup(t, db, "SKT-K")
	station := "edge.1"
	err := db.SaveKanban(store.KanbanChange{NodeID: *k, EnsureRow: true, Station: &station,
		Sources: &[]int64{999999999}}, "stephen")
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("save of a source that does not exist: %v", err)
	}
	if row, err := db.GetKanban(*k); err != nil || row != nil {
		t.Errorf("the failed save left a row: %+v (%v)", row, err)
	}
	if got := kanbanAudit(t, db, *k); len(got) != 0 {
		t.Errorf("the failed save left audit rows: %v", got)
	}
}
