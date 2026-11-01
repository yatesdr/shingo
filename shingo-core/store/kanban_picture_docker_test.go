//go:build docker

package store_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// kanban_picture_docker_test.go — the picture over the U9 shape: a kanban whose
// supports are a press's front, paired and second positions, types by part.

type pictureRig struct {
	t     *testing.T
	db    *store.DB
	k     int64
	types map[string]int64
}

func newPictureRig(t *testing.T, cover int) *pictureRig {
	t.Helper()
	db := testdb.Open(t)
	k := testdb.MarkedGroup(t, db, "PIC-K")
	for i := 1; i <= 3; i++ {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: fmt.Sprintf("PIC-K-P%d", i), Enabled: true, ParentID: k}), "position")
	}
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "PIC-K-OFF", Enabled: false, ParentID: k}), "a disabled position")
	r := &pictureRig{t: t, db: db, k: *k, types: map[string]int64{}}
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: *k, EnsureRow: true, Cover: &cover}, "test"), "cover")
	var supports []int64
	for _, name := range []string{"PIC-FRONT", "PIC-PAIRED", "PIC-SECOND", "PIC-WELD"} {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "supported position")
		supports = append(supports, n.ID)
	}
	testutil.MustNoErr(t, db.SetMaintainSupports(*k, supports), "supports")
	for _, code := range []string{"PIC-SMALL", "PIC-BIG", "PIC-TOTES"} {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&id), "type")
		r.types[code] = id
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: *k, BinTypeID: id, Want: 1}), "level")
	}
	return r
}

// part makes a payload with a payload_bin_types row per carrier named.
func (r *pictureRig) part(code string, carriers ...string) {
	r.t.Helper()
	var id int64
	testutil.MustNoErr(r.t, r.db.QueryRow(`INSERT INTO payloads (code) VALUES ($1) RETURNING id`, code).Scan(&id), "payload")
	for _, c := range carriers {
		_, err := r.db.DB.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1,$2)`, id, r.types[c])
		testutil.MustNoErr(r.t, err, "payload bin type")
	}
}

// styleOf is one style's produce claims as (node, part), the first with its pair.
type styleOf struct {
	style  string
	active bool
	parts  [][2]string
}

// mirror replaces a process's rows as the plant.claims feed does.
func (r *pictureRig) mirror(process string, styles ...styleOf) {
	r.t.Helper()
	var ss []plantclaims.StyleRow
	var cs []plantclaims.ClaimRow
	for _, st := range styles {
		ss = append(ss, plantclaims.StyleRow{ProcessID: process, StyleID: st.style, ConfigGen: 1, IsActive: st.active})
		for i, p := range st.parts {
			c := plantclaims.ClaimRow{ProcessID: process, StyleID: st.style, CoreNodeName: p[0],
				Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("press_index"), PayloadCode: p[1], Seq: i}
			cs = append(cs, c)
		}
	}
	testutil.MustNoErr(r.t, plantclaims.ReplaceProcess(r.db.DB, process, ss, cs, 0), "mirror "+process)
}

// openChangeover opens a changeover episode at age old, naming its target's claim.
func (r *pictureRig) openChangeover(id, process, node, part string, age time.Duration) {
	r.t.Helper()
	testutil.MustNoErr(r.t, r.db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: id, Revision: 1, EpisodeKey: "co|" + id, Kind: protocol.EpisodeKindChangeover,
		StationID: "edge.1", ProcessID: process, CoreNodeName: node, PayloadCode: part,
		OpenedAt: time.Now().UTC().Add(-age),
	}), "open changeover")
}

// running is the picture's Running per type code; the one row per level is the
// row the reader takes, so it also asserts that each type has one.
func (r *pictureRig) picture() map[string]store.KanbanPictureRow {
	r.t.Helper()
	rows, err := r.db.ListKanbanPicture([]int64{r.k})
	testutil.MustNoErr(r.t, err, "picture")
	out := map[string]store.KanbanPictureRow{}
	for _, row := range rows {
		if _, dup := out[row.BinTypeCode]; dup {
			r.t.Fatalf("two rows for %s", row.BinTypeCode)
		}
		out[row.BinTypeCode] = row
	}
	return out
}

func (r *pictureRig) wantRunning(what string, want map[string]int) {
	r.t.Helper()
	got := r.picture()
	for code := range r.types {
		if got[code].Running != want[code] {
			r.t.Errorf("%s: %s running = %d, want %d", what, code, got[code].Running, want[code])
		}
		if wantWant := got[code].Base + got[code].Cover*want[code]; got[code].Want() != wantWant {
			r.t.Errorf("%s: %s Want() = %d, want %d", what, code, got[code].Want(), wantWant)
		}
	}
}

func TestKanbanPicture_Running(t *testing.T) {
	t.Parallel()

	t.Run("cover 0: running is counted, the want stays the base", func(t *testing.T) {
		r := newPictureRig(t, 0)
		r.part("P-SMALL", "PIC-SMALL")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-SMALL"}}})
		r.wantRunning("cover 0", map[string]int{"PIC-SMALL": 1})
		if row := r.picture()["PIC-SMALL"]; row.Want() != 1 {
			t.Errorf("Want() = %d, want the base 1", row.Want())
		}
	})

	t.Run("two processes running one type", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-SMALL"}}})
		r.mirror("WELD", styleOf{"W", true, [][2]string{{"PIC-WELD", "P-SMALL"}}})
		r.wantRunning("two processes", map[string]int{"PIC-SMALL": 2})
		if row := r.picture()["PIC-SMALL"]; row.Want() != 9 {
			t.Errorf("Want() = %d, want 1 + 4x2", row.Want())
		}
	})

	t.Run("DISTINCT: a process on four positions, two parts on one type, counts once", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-L", "PIC-SMALL")
		r.part("P-R", "PIC-SMALL")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{
			{"PIC-FRONT", "P-L"}, {"PIC-PAIRED", "P-R"}, {"PIC-SECOND", "P-L"}, {"PIC-WELD", "P-R"}}})
		r.wantRunning("distinct", map[string]int{"PIC-SMALL": 1})
	})

	t.Run("the paired and second nodes of a claim connect it", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		outside := &nodes.Node{Name: "PIC-OUTSIDE", Enabled: true}
		testutil.MustNoErr(t, r.db.CreateNode(outside), "outside")
		testutil.MustNoErr(t, plantclaims.ReplaceProcess(r.db.DB, "PRESS",
			[]plantclaims.StyleRow{{ProcessID: "PRESS", StyleID: "S", ConfigGen: 1, IsActive: true}},
			[]plantclaims.ClaimRow{{ProcessID: "PRESS", StyleID: "S", CoreNodeName: "PIC-OUTSIDE",
				Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapMode("press_index"), PayloadCode: "P-SMALL",
				SecondPairedCoreNode: "PIC-SECOND"}}, 0), "mirror")
		r.wantRunning("second paired", map[string]int{"PIC-SMALL": 1})
	})

	t.Run("a process on no supported node does not count", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.mirror("ELSEWHERE", styleOf{"S", true, [][2]string{{"PIC-K-P1", "P-SMALL"}}})
		r.wantRunning("elsewhere", map[string]int{})
	})

	t.Run("a two-part style runs two types", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-L", "PIC-SMALL")
		r.part("P-R", "PIC-BIG")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-L"}, {"PIC-PAIRED", "P-R"}}})
		r.wantRunning("two parts", map[string]int{"PIC-SMALL": 1, "PIC-BIG": 1})
	})

	t.Run("an open changeover moves the term to the target", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.part("P-TOTES", "PIC-TOTES")
		r.mirror("PRESS", styleOf{"SMALL", true, [][2]string{{"PIC-FRONT", "P-SMALL"}}},
			styleOf{"TOTES", false, [][2]string{{"PIC-FRONT", "P-TOTES"}}})
		r.wantRunning("before", map[string]int{"PIC-SMALL": 1})
		// An older open episode at the same process loses to the latest.
		r.openChangeover("11111111-1111-1111-1111-111111111111", "PRESS", "PIC-FRONT", "P-SMALL", 2*time.Hour)
		r.openChangeover("22222222-2222-2222-2222-222222222222", "PRESS", "PIC-FRONT", "P-TOTES", time.Minute)
		r.wantRunning("open", map[string]int{"PIC-TOTES": 1})
		row := r.picture()["PIC-TOTES"]
		if len(row.FallbackProcesses) != 0 {
			t.Errorf("a target that resolves named a fallback: %v", row.FallbackProcesses)
		}
		// Closing it puts the term back on the active style.
		closed := time.Now().UTC()
		for _, id := range []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"} {
			_, err := r.db.CloseDemandOriginByID(id, "resolved", protocol.ClosedByNotification, closed)
			testutil.MustNoErr(t, err, "close")
		}
		r.wantRunning("closed", map[string]int{"PIC-SMALL": 1})
	})

	t.Run("a target shared with the active style is the style that is not active", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.part("P-BIG", "PIC-BIG")
		r.mirror("PRESS", styleOf{"ONE", true, [][2]string{{"PIC-FRONT", "P-SMALL"}}},
			styleOf{"TWO", false, [][2]string{{"PIC-FRONT", "P-SMALL"}, {"PIC-PAIRED", "P-BIG"}}})
		r.openChangeover("33333333-3333-3333-3333-333333333333", "PRESS", "PIC-FRONT", "P-SMALL", time.Minute)
		r.wantRunning("shared claim", map[string]int{"PIC-SMALL": 1, "PIC-BIG": 1})
	})

	t.Run("a part with no row contributes nothing and is named", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.part("P-NONE")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-SMALL"}, {"PIC-PAIRED", "P-NONE"}}})
		r.wantRunning("no row", map[string]int{"PIC-SMALL": 1})
		if got := r.picture()["PIC-BIG"].NothingProcesses; !reflect.DeepEqual(got, []string{"PRESS"}) {
			t.Errorf("nothing processes = %v, want [PRESS] on every row of the kanban", got)
		}
	})

	t.Run("a part mirrored with no payload at all contributes nothing and is named", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-UNKNOWN"}}})
		r.wantRunning("no payload", map[string]int{})
		if got := r.picture()["PIC-SMALL"].NothingProcesses; !reflect.DeepEqual(got, []string{"PRESS"}) {
			t.Errorf("nothing processes = %v, want [PRESS]", got)
		}
	})

	t.Run("a part with two rows contributes nothing and is named", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-TWO", "PIC-SMALL", "PIC-BIG")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-TWO"}}})
		r.wantRunning("two rows", map[string]int{})
		if got := r.picture()["PIC-SMALL"].NothingProcesses; !reflect.DeepEqual(got, []string{"PRESS"}) {
			t.Errorf("nothing processes = %v, want [PRESS]", got)
		}
	})

	t.Run("a target that resolves to nothing falls back to the active types and is named", func(t *testing.T) {
		r := newPictureRig(t, 4)
		r.part("P-SMALL", "PIC-SMALL")
		r.part("P-TWO", "PIC-SMALL", "PIC-BIG")
		r.mirror("PRESS", styleOf{"S", true, [][2]string{{"PIC-FRONT", "P-SMALL"}}},
			styleOf{"T", false, [][2]string{{"PIC-FRONT", "P-TWO"}}})

		// A claim no style has (a blank episode from an Edge that does not stamp).
		r.openChangeover("44444444-4444-4444-4444-444444444444", "PRESS", "", "", time.Hour)
		r.wantRunning("blank episode", map[string]int{"PIC-SMALL": 1})
		if got := r.picture()["PIC-SMALL"].FallbackProcesses; !reflect.DeepEqual(got, []string{"PRESS"}) {
			t.Errorf("fallback processes = %v, want [PRESS]", got)
		}

		// A style whose only part resolves to no type.
		r.openChangeover("55555555-5555-5555-5555-555555555555", "PRESS", "PIC-FRONT", "P-TWO", time.Minute)
		r.wantRunning("target with no type", map[string]int{"PIC-SMALL": 1})
		row := r.picture()["PIC-BIG"]
		if !reflect.DeepEqual(row.FallbackProcesses, []string{"PRESS"}) {
			t.Errorf("fallback processes = %v, want [PRESS]", row.FallbackProcesses)
		}
		if len(row.NothingProcesses) != 0 {
			t.Errorf("nothing processes = %v; the fallback counts on the active style, whose part resolves", row.NothingProcesses)
		}
	})
}

// The rest of the row, on one kanban, and the NULL/ids scope.
func TestKanbanPicture_TheRow(t *testing.T) {
	t.Parallel()
	r := newPictureRig(t, 4)
	db := r.db
	z45, z32 := testdb.MarkedGroup(t, db, "PIC-Z45"), testdb.MarkedGroup(t, db, "PIC-Z32")
	noLevel := testdb.MarkedGroup(t, db, "PIC-ROW-NO-LEVEL")
	testdb.KanbanStrict(t, db, *noLevel, true)
	strict, station := true, "edge.1"
	wants := []store.MaintainLayerWant{{Layers: 0, Want: 2}, {Layers: 3, Want: 1}}
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: r.k, EnsureRow: true, Strict: &strict, Station: &station,
		Overflow: z45,
		Levels: []store.KanbanLevelChange{
			{BinTypeID: r.types["PIC-SMALL"], Overflow: z32, LayerWants: &wants},
		},
		Sources: &[]int64{*z32, *z45},
	}, "test"), "save")

	row := r.picture()["PIC-SMALL"]
	if row.KanbanName != "PIC-K" || row.StationID != "edge.1" || !row.Strict || row.Slots != 3 || row.Base != 1 || row.Cover != 4 {
		t.Errorf("row = %+v", row)
	}
	if row.TopsAndBottomsWant != 2 {
		t.Errorf("tops & bottoms = %d, want the row at 0 layers", row.TopsAndBottomsWant)
	}
	if row.OverflowNode != "PIC-Z32" || row.OverflowNodeID == nil || *row.OverflowNodeID != *z32 ||
		row.LevelOverflowNodeID == nil || *row.LevelOverflowNodeID != *z32 {
		t.Errorf("per-type overflow: %+v", row)
	}
	if other := r.picture()["PIC-BIG"]; other.OverflowNode != "PIC-Z45" || other.LevelOverflowNodeID != nil {
		t.Errorf("default overflow: %+v", other)
	}
	if names := refNames(row.Sources); !reflect.DeepEqual(names, []string{"PIC-Z32", "PIC-Z45"}) {
		t.Errorf("sources = %v, want rank order", names)
	}
	if names := refNames(row.Supports); !reflect.DeepEqual(names, []string{"PIC-FRONT", "PIC-PAIRED", "PIC-SECOND", "PIC-WELD"}) {
		t.Errorf("supports = %v", names)
	}

	all, err := db.ListKanbanPicture(nil)
	testutil.MustNoErr(t, err, "all")
	if len(all) != 3 {
		t.Errorf("nil ids = %d rows, want the three levels: a kanban with no level has no row", len(all))
	}
	none, err := db.ListKanbanPicture([]int64{*z45})
	testutil.MustNoErr(t, err, "another kanban")
	if len(none) != 0 {
		t.Errorf("ids naming a group with no levels = %+v", none)
	}
}

func refNames(refs []store.KanbanRef) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}
