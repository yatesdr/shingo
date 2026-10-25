//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// toteWantFixture is a press cell fed from behind and an empties group holding
// two tote carts, a 26 landed first and a 40 after it. The part, the type and
// the carts carry no tote numbers until withNumbers sets them.
type toteWantFixture struct {
	db          *store.DB
	short, tall int64
}

func newToteWantFixture(t *testing.T, db *store.DB) toteWantFixture {
	t.Helper()
	_, err := db.Exec(`INSERT INTO payloads (code) VALUES ('TWD-PART')`)
	testutil.MustNoErr(t, err, "payload")
	for _, name := range []string{"TWD-PRESS", "TWD-OUT"} {
		testutil.MustNoErr(t, nodes.Create(db.DB, &nodes.Node{Name: name, Enabled: true}), name)
	}
	_, err = db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node)
		VALUES ('TWD-P', 'S1', 'TWD-PRESS', 'consume', '', 'TWD-PART', 'TWD-OUT')`)
	testutil.MustNoErr(t, err, "claim")
	grpID, err := nodes.CreateGroup(db.DB, "TWD-GRP")
	testutil.MustNoErr(t, err, "group")
	testdb.MarkGroup(t, db, grpID)
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('TWD-TOTE') RETURNING id`).Scan(&btID), "type")
	f := toteWantFixture{db: db}
	for i, id := range []*int64{&f.short, &f.tall} {
		slot := &nodes.Node{Name: fmt.Sprintf("TWD-SLOT-%d", i), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, nodes.Create(db.DB, slot), "slot")
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status)
			VALUES ($1, $2, $3, 'available') RETURNING id`, btID, fmt.Sprintf("TWD-CART-%d", i), slot.ID).Scan(id), "cart")
	}
	return f
}

func (f toteWantFixture) withNumbers(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		`UPDATE payloads SET tote_layers = 5, uop_per_tote = 10 WHERE code = 'TWD-PART'`,
		`UPDATE bin_types SET totes_per_layer = 8 WHERE code = 'TWD-TOTE'`,
		fmt.Sprintf(`UPDATE bins SET totes_on_cart = 26 WHERE id = %d`, f.short),
		fmt.Sprintf(`UPDATE bins SET totes_on_cart = 40 WHERE id = %d`, f.tall),
	} {
		_, err := f.db.Exec(q)
		testutil.MustNoErr(t, err, q)
	}
}

// pick runs the planner's pick for an empty on the counting connection and
// returns the bin, the statements it issued, and the empty finder's statement.
func pickCounted(t *testing.T, cdb *store.DB, counter *store.QueryCounter, need SourceNeed) (int64, []string, string) {
	t.Helper()
	counter.Reset()
	res := NewSourceFinder(cdb, nil, nil).FindSourceForNeed(need)
	stmts := counter.Statements()
	if res.Bin == nil {
		t.Fatalf("found nothing (%v, %s)", res.Outcome, res.QueueCause)
	}
	find := ""
	for _, s := range stmts {
		if strings.Contains(s, "LIMIT 1") && strings.Contains(s, "FROM bins b") {
			find = s
		}
	}
	return res.Bin.ID, stmts, find
}

// TestToteWant_NoNumbersAddNoStatement: an empty for a press cell's outside
// position, as an index refill asks for it (from the empties group, set down at
// the paired node) and as a plain retrieve asks for it. At a plant that sets no
// tote numbers the find is today's statement, byte for byte in its ORDER BY,
// and takes today's cart; the want rides the destination read, so setting the
// numbers moves the cart taken (the 40, a full stack of 5 layers of 8) and not
// the number of statements.
func TestToteWant_NoNumbersAddNoStatement(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	f := newToteWantFixture(t, db)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })

	needs := map[string]SourceNeed{
		"index refill": {SourceNode: "TWD-GRP", PayloadCode: "TWD-PART", Intent: IntentEmpty,
			ProcessNode: "TWD-PRESS", SetDownNode: "TWD-OUT"},
		"plain retrieve": {PayloadCode: "TWD-PART", Intent: IntentEmpty,
			DeliveryNode: "TWD-OUT", SetDownNode: "TWD-OUT"},
	}
	before := map[string]int{}
	for name, need := range needs {
		got, stmts, find := pickCounted(t, cdb, counter, need)
		before[name] = len(stmts)
		t.Logf("%s, no numbers: %d statements", name, len(stmts))
		if got != f.short {
			t.Errorf("%s, no numbers: took %d, want today's first by id %d", name, got, f.short)
		}
		if !strings.HasSuffix(find, bins.AccessibleEmptyOrder) {
			t.Errorf("%s, no numbers: the find does not end in today's ORDER BY:\n%s", name, find)
		}
	}

	f.withNumbers(t)
	for name, need := range needs {
		got, stmts, find := pickCounted(t, cdb, counter, need)
		t.Logf("%s, numbers set: %d statements", name, len(stmts))
		if got != f.tall {
			t.Errorf("%s, numbers set: took %d, want the full stack %d", name, got, f.tall)
		}
		if strings.HasSuffix(find, bins.AccessibleEmptyOrder) {
			t.Errorf("%s, numbers set: the find ranks by no height:\n%s", name, find)
		}
		if len(stmts) != before[name] {
			t.Errorf("%s: %d statements with the numbers set, %d without; the want must ride a read "+
				"the pick already makes", name, len(stmts), before[name])
		}
	}
}

// TestToteWant_ThePoolsStatements counts an empty fetched as the vacant on-deck
// fallback asks for it (set down on the press, a fed core), from three
// sources: a dedicated loader's home whose pool holds a cart, a node no loader
// holds (tier 2 misses: the common case), and a loader home whose pool holds
// nothing. The want is resolved before the pool is asked, and every other empty
// tier already paid for it, so a pool that finds and a source the pool tier
// misses cost what they cost before; a pool holding nothing pays the
// derivation's reads ahead of its own, as a group holding nothing does.
func TestToteWant_ThePoolsStatements(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	newToteWantFixture(t, db)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	home := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, nodes.Create(db.DB, n), name)
		id, err := db.CreateLoader(store.Loader{Name: name + "-LOADER", Role: loaders.RoleProduce,
			Layout: loaders.LayoutDedicatedPositions, Replenishment: "operator"})
		testutil.MustNoErr(t, err, "loader")
		testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: id, PositionNodeID: n.ID,
			PayloadCode: "TWD-PART", Kind: loaders.HomeKindHome}), "home")
		return n
	}
	pool, dry := home("TWD-HOME"), home("TWD-DRY")
	plain := &nodes.Node{Name: "TWD-PLAIN", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, plain), "plain")
	for _, at := range []*nodes.Node{pool, plain} {
		_, err := db.Exec(`INSERT INTO bins (bin_type_id, label, node_id, status)
			SELECT id, $1, $2, 'available' FROM bin_types WHERE code = 'TWD-TOTE'`, at.Name+"-CART", at.ID)
		testutil.MustNoErr(t, err, "cart on "+at.Name)
	}
	for _, c := range []struct {
		name  string
		src   string
		found bool
		want  int
	}{
		{"a pool that finds", pool.Name, true, wantPoolFound},
		{"a node no loader holds", plain.Name, true, wantPoolMissed},
		{"a pool holding nothing", dry.Name, false, wantPoolDry},
	} {
		counter.Reset()
		res := NewSourceFinder(cdb, nil, nil).FindSourceForNeed(SourceNeed{SourceNode: c.src, PayloadCode: "TWD-PART",
			ProcessNode: "TWD-PRESS", SetDownNode: "TWD-PRESS", Intent: IntentEmpty})
		stmts := counter.Statements()
		if (res.Bin != nil) != c.found {
			t.Errorf("%s: found %v (%v, %s), want found=%v", c.name, res.Bin, res.Outcome, res.QueueCause, c.found)
		}
		if len(stmts) != c.want {
			t.Errorf("%s: %d statements, want %d\n%s", c.name, len(stmts), c.want, testutil.StatementBreakdown(stmts))
		}
	}
}

// Measured with the pool asked before the want was derived: found 11, missed 9,
// a pool holding nothing 6. The derivation here is three reads (the set-down
// node with its height, and the two of its effective bin types); a pool that
// finds and a source the pool tier misses paid them after the pool, so they
// are unchanged. A pool holding nothing never reached them and pays them now.
const (
	wantPoolFound  = 11
	wantPoolMissed = 9
	wantPoolDry    = 9
)
