//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// kanban_pins_docker_test.go — characterization pins of the store's half of the
// kanban paths, at today's schema.

// TestKanbanPin_APassReadsAGroupsLevelsOnce: GroupPassReads.ListKanbanLevels is
// one statement per group per view, and a fresh view reads again. Its only reader
// today is the lane gate's pass.
func TestKanbanPin_APassReadsAGroupsLevelsOnce(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	a, b := testdb.MarkedGroup(t, db, "KPM-A"), testdb.MarkedGroup(t, db, "KPM-B")
	var bt int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('KPM-T') RETURNING id`).Scan(&bt), "bin type")
	testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: *a, BinTypeID: bt, Want: 2}), "level")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })

	pass := cdb.GroupPassReads(*a)
	counter.Reset()
	for i := 0; i < 3; i++ {
		lv, err := pass.ListKanbanLevels(*a)
		testutil.MustNoErr(t, err, "levels")
		if len(lv) != 1 || lv[0].Want != 2 {
			t.Fatalf("levels = %+v, want the one row at 2", lv)
		}
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("three asks of one group = %d statements, want 1", got)
	}
	// A group with no row is remembered as well: nothing is not a miss.
	counter.Reset()
	for i := 0; i < 2; i++ {
		if _, err := pass.ListKanbanLevels(*b); err != nil {
			t.Fatalf("levels of the other group: %v", err)
		}
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("two asks of a group with no row = %d statements, want 1", got)
	}
	counter.Reset()
	if _, err := cdb.GroupPassReads(*a).ListKanbanLevels(*a); err != nil {
		t.Fatalf("levels through a new view: %v", err)
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("a new view = %d statements, want 1: nothing outlives its pass", got)
	}
}

// TestKanbanPin_TheUpsertKeepsPayloadAndNodeABlankRevisionOmits: a later revision
// of an episode writes the payload_code and core_node_name it names, and a blank
// keeps what an earlier revision said. An older revision writes nothing.
func TestKanbanPin_TheUpsertKeepsPayloadAndNodeABlankRevisionOmits(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	base := store.DemandOrigin{
		OriginID: "aaaaaaaa-0000-0000-0000-00000000c0de", EpisodeKey: "changeover|KP.LINE|42", Kind: protocol.EpisodeKindChangeover,
		StationID: "KP.LINE", ProcessID: "42", OpenedAt: opened,
	}
	apply := func(rev int64, payload, node string) *store.DemandOrigin {
		t.Helper()
		o := base
		o.Revision, o.PayloadCode, o.CoreNodeName = rev, payload, node
		testutil.MustNoErr(t, db.UpsertDemandOrigin(o), "upsert")
		got, err := db.GetDemandOrigin(base.OriginID)
		testutil.MustNoErr(t, err, "read back")
		return got
	}

	if got := apply(1, "", ""); got.PayloadCode != "" || got.CoreNodeName != "" {
		t.Errorf("opened blank, the row holds %q / %q", got.PayloadCode, got.CoreNodeName)
	}
	if got := apply(2, "KP-PART", "KP-NODE"); got.PayloadCode != "KP-PART" || got.CoreNodeName != "KP-NODE" {
		t.Errorf("revision 2 said KP-PART / KP-NODE, the row holds %q / %q", got.PayloadCode, got.CoreNodeName)
	}
	if got := apply(2, "KP-LATE", "KP-LATE"); got.PayloadCode != "KP-PART" || got.CoreNodeName != "KP-NODE" {
		t.Errorf("a redelivered revision wrote %q / %q", got.PayloadCode, got.CoreNodeName)
	}
	if got := apply(3, "", ""); got.PayloadCode != "KP-PART" || got.CoreNodeName != "KP-NODE" {
		t.Errorf("revision 3 said nothing and the row holds %q / %q; want KP-PART / KP-NODE kept", got.PayloadCode, got.CoreNodeName)
	}
	if got := apply(4, "KP-NEXT", ""); got.PayloadCode != "KP-NEXT" || got.CoreNodeName != "KP-NODE" {
		t.Errorf("revision 4 named a payload alone and the row holds %q / %q; want KP-NEXT / KP-NODE", got.PayloadCode, got.CoreNodeName)
	}
}
