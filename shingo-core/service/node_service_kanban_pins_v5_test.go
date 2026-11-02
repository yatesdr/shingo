//go:build docker

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// node_service_kanban_pins_v5_test.go — what the save says about a kanban's family: it
// warns on everything a plant may mean, and refuses the one shape the keeper could not
// count. Taken before the save changes. Predictions: PINS.md, "BRIEF v5".

// pinFamilyOf groups types into a family, where the table exists to say so.
func pinFamilyOf(t *testing.T, db *store.DB, family string, ids ...int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO carrier_families (code) VALUES ($1) ON CONFLICT DO NOTHING`, family); err != nil {
		t.Logf("no family table at this tree: %v", err)
		return
	}
	for _, id := range ids {
		_, err := db.Exec(`UPDATE bin_types SET family_id = (SELECT id FROM carrier_families WHERE code = $1) WHERE id = $2`, family, id)
		testutil.MustNoErr(t, err, "family")
	}
}

// pinPool declares a pool, where the table exists to hold it.
func pinPool(t *testing.T, db *store.DB, kanban int64, family string, pool int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO kanban_families (kanban_node_id, family_id, pool)
		SELECT $1, id, $3 FROM carrier_families WHERE code = $2
		ON CONFLICT (kanban_node_id, family_id) DO UPDATE SET pool = EXCLUDED.pool`, kanban, family, pool)
	if err != nil {
		t.Logf("no pool table at this tree: %v", err)
	}
}

type pinSaveRig struct {
	t                 *testing.T
	svc               *NodeService
	db                *store.DB
	k                 int64
	kids              []*nodes.Node
	small, big, totes int64
}

func newPinSaveRig(t *testing.T) *pinSaveRig {
	t.Helper()
	db := testDB(t)
	r := &pinSaveRig{t: t, svc: NewNodeService(db), db: db}
	sized := func(code string, h float64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, width_in, length_in, height_in) VALUES ($1,45,48,$2) RETURNING id`,
			code, h).Scan(&id), "type "+code)
		return id
	}
	r.small, r.big, r.totes = sized("PSV-SMALL", 25), sized("PSV-BIG", 34), sized("PSV-TOTES", 6)
	pinFamilyOf(t, db, "PSV-45", r.small, r.big, r.totes)
	var kids []*nodes.Node
	r.k, kids = mgGroup(t, db, "PSV-K", 4)
	testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: r.k, EnsureRow: true}, "test"), "the row")
	r.kids = kids
	return r
}

func (r *pinSaveRig) says(what string, chk KanbanCheck, words ...string) {
	r.t.Helper()
	all := strings.ToLower(strings.Join(append(chk.Warnings, chk.Refusals...), "; "))
	for _, w := range words {
		if !strings.Contains(all, strings.ToLower(w)) {
			r.t.Errorf("%s: the save says %q, want it to say %q", what, all, w)
		}
	}
}

func (r *pinSaveRig) saveLevel(bt int64, want int) KanbanCheck {
	r.t.Helper()
	chk, err := r.svc.SetKanbanLevel(r.k, bt, want)
	testutil.MustNoErr(r.t, err, "the save")
	return chk
}

// TestKanbanPin_TheSaveWarns: pool past the slots that admit the family, pool under the
// minimum, a slot admitting part of a family the kanban levels.
func TestKanbanPin_TheSaveWarns(t *testing.T) {
	t.Parallel()
	t.Run("room under the pool", func(t *testing.T) {
		t.Parallel()
		r := newPinSaveRig(t)
		pinPool(t, r.db, r.k, "PSV-45", 9)
		r.says("9 carts on 4 slots", r.saveLevel(r.small, 2), "PSV-45", "room")
	})
	t.Run("pool under the minimum", func(t *testing.T) {
		t.Parallel()
		r := newPinSaveRig(t)
		pinPool(t, r.db, r.k, "PSV-45", 1)
		r.says("a pool of 1 under a mix of 2", r.saveLevel(r.small, 2), "minimum")
	})
	t.Run("a slot admitting part of the family", func(t *testing.T) {
		t.Parallel()
		r := newPinSaveRig(t)
		testutil.MustNoErr(t, r.db.SetNodeBinTypes(r.kids[0].ID, []int64{r.small}), "the slot's types")
		testutil.MustNoErr(t, r.db.SetNodeProperty(r.kids[0].ID, "bin_type_mode", "specific"), "the mode")
		r.says("a slot of one type", r.saveLevel(r.small, 2), r.kids[0].Name, "part of")
	})
	t.Run("members disagreeing", func(t *testing.T) {
		t.Parallel()
		r := newPinSaveRig(t)
		_, err := r.db.Exec(`UPDATE bin_types SET width_in = 44 WHERE id = $1`, r.big)
		testutil.MustNoErr(t, err, "a typo")
		_, err = r.db.Exec(`UPDATE bin_types SET required_robot_group = 'HEAVY' WHERE id = $1`, r.big)
		testutil.MustNoErr(t, err, "a heavier robot")
		r.says("a footprint and a robot group apart", r.saveLevel(r.small, 2), "footprint", "robot group")
	})
}

// TestKanbanPin_ThePoolIsWrittenAtTheMinimumWhenAbsent: nobody types a pool, so the save
// writes it at the minimum and says so.
func TestKanbanPin_ThePoolIsWrittenAtTheMinimumWhenAbsent(t *testing.T) {
	t.Parallel()
	r := newPinSaveRig(t)
	r.says("the first save", r.saveLevel(r.small, 2), "pool")
	var pool int
	if err := r.db.QueryRow(`SELECT pool FROM kanban_families WHERE kanban_node_id = $1`, r.k).Scan(&pool); err != nil {
		t.Errorf("no pool written: %v", err)
	} else if pool != 2 {
		t.Errorf("pool = %d, want the minimum, 2", pool)
	}
}

// TestKanbanPin_TwoKanbansOnOneSupportRefused: both would count the same line carts.
func TestKanbanPin_TwoKanbansOnOneSupportRefused(t *testing.T) {
	t.Parallel()
	r := newPinSaveRig(t)
	pos := &nodes.Node{Name: "PSV-POS", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(pos), "position")
	testutil.MustNoErr(t, r.db.SetKanbanSupports(r.k, []int64{pos.ID}), "supports")
	testdb.KanbanStation(t, r.db, r.k, "PSV-EDGE")
	testutil.MustNoErr(t, r.db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: r.k, BinTypeID: r.small, Want: 1}), "level")
	k2, _ := mgGroup(t, r.db, "PSV-K2", 2)
	testdb.KanbanStation(t, r.db, k2, "PSV-EDGE")
	testutil.MustNoErr(t, r.db.SetKanbanSupports(k2, []int64{pos.ID}), "supports")
	chk, err := r.svc.SetKanbanLevel(k2, r.big, 1)
	testutil.MustNoErr(t, err, "the save")
	if !strings.Contains(strings.Join(chk.Refusals, "; "), "PSV-45") {
		t.Errorf("two kanbans levelling one family on one support: refusals %v, want one naming the family", chk.Refusals)
	}
}

// pinRenameSays is what the node save says before a rename.
func pinRenameSays(t *testing.T, svc *NodeService, n *nodes.Node, name string) []string {
	t.Helper()
	said, err := svc.RenameWarnings(n, name)
	testutil.MustNoErr(t, err, "the rename's words")
	return said
}

// TestKanbanPin_ARenameListsWhatJoinsByName: a loader's outbound and a claim's positions are
// joined by node name, so a rename warns and names them.
func TestKanbanPin_ARenameListsWhatJoinsByName(t *testing.T) {
	t.Parallel()
	r := newPinSaveRig(t)
	mgUnloaderSendingTo(t, r.svc, "PSV-UNL", "PSV-K")
	g, err := r.db.GetNode(r.k)
	testutil.MustNoErr(t, err, "K")
	said := strings.Join(pinRenameSays(t, r.svc, g, "PSV-K-NEW"), "; ")
	if !strings.Contains(said, "PSV-UNL") {
		t.Errorf("the rename says %q, want it to name the loader PSV-UNL", said)
	}
}

// TestKanbanPin_TheSaveReadsTheFamilyTable: the one truth table at the save. A kanban levels
// the cart's type; its one source levels the asked type. That source is a lender of the cart
// exactly where the two are one family.
func TestKanbanPin_TheSaveReadsTheFamilyTable(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../kanban/testdata/same_family.json")
	testutil.MustNoErr(t, err, "the table")
	var table struct {
		Types map[string]struct {
			Family    string     `json:"family"`
			Footprint *[]float64 `json:"footprint"`
		} `json:"types"`
		Cases []struct {
			Cart, Asked string
			Same        bool
		} `json:"cases"`
	}
	testutil.MustNoErr(t, json.Unmarshal(raw, &table), "the table")
	db := testDB(t)
	svc := NewNodeService(db)
	ids := map[string]int64{}
	for code, ty := range table.Types {
		w, l := 0.0, 0.0
		if ty.Footprint != nil {
			w, l = (*ty.Footprint)[0], (*ty.Footprint)[1]
		}
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, width_in, length_in) VALUES ($1,$2,$3) RETURNING id`, code, w, l).Scan(&id), "type")
		ids[code] = id
		pinFamilyOf(t, db, ty.Family, id)
	}
	for i, c := range table.Cases {
		k, _ := mgGroup(t, db, fmt.Sprintf("PST-K%d", i), 2)
		src, _ := mgGroup(t, db, fmt.Sprintf("PST-S%d", i), 2)
		testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: src, BinTypeID: ids[c.Asked], Want: 1}), "source level")
		testutil.MustNoErr(t, db.SaveKanban(store.KanbanChange{NodeID: k, EnsureRow: true, Sources: &[]int64{src}}, "test"), "sources")
		mgUnloaderSendingTo(t, svc, fmt.Sprintf("PST-U%d", i), fmt.Sprintf("PST-K%d", i))
		chk, err := svc.SetKanbanLevel(k, ids[c.Cart], 1)
		testutil.MustNoErr(t, err, "the save")
		noLender := strings.Contains(strings.Join(chk.Warnings, "; "), "no source")
		if noLender == c.Same {
			t.Errorf("a %s beside a source of %s: warned no lender %v; the table says one family: %v (%v)", c.Cart, c.Asked, noLender, c.Same, chk.Warnings)
		}
	}
}
