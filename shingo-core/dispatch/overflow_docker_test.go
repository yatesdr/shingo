//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// overflow_docker_test.go — MG6-1. A kanban at its level refuses the
// push; if somebody named an overflow, the carrier goes there instead of
// parking.
//
// CORE-SIDE ONLY. Edge keeps naming the group unconditionally — it has no level
// to read and should not grow one — so this is Core answering "where does this
// actually go" at admission, the same shape as every other placement decision.

// levelledGroup builds a kanban with one position, one declared level
// of `want`, and `held` carriers of that type already standing in it.
func levelledGroup(t *testing.T, db *store.DB, name, code string, want, held, slots int) (int64, int64) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "CreateGroup "+name)
	testdb.MarkGroup(t, db, grpID)

	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`
		INSERT INTO bin_types (code) VALUES ($1)
		ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
		RETURNING id`, code).Scan(&btID), "bin type")

	slotIDs := make([]int64, slots)
	for i := 0; i < slots; i++ {
		n := &nodes.Node{Name: name + "-P" + string(rune('A'+i)), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(n), "create position")
		slotIDs[i] = n.ID
	}
	for i := 0; i < held; i++ {
		_, err := db.Exec(
			`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available')`,
			btID, name+"-BIN-"+string(rune('A'+i)), slotIDs[i])
		testutil.MustNoErr(t, err, "resident carrier")
	}
	if want > 0 {
		testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{
			GroupNodeID: grpID, BinTypeID: btID, Want: want,
		}), "declare level")
	}
	return grpID, btID
}

// pushInto admits an order delivering to a group and reports where it ended up.
func pushInto(t *testing.T, d *Dispatcher, db *store.DB, uuid, group string) string {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeMove,
		Status: StatusPending, Quantity: 1, DeliveryNode: group,
		SourceNode: "OVF-SRC",
	}
	if lerr := d.lifecycle.admit(o, nil, func() error { return d.db.CreateOrder(o) }); lerr != nil {
		t.Fatalf("admit %s: %s", uuid, lerr.Detail)
	}
	got, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "reload order")
	return got.DeliveryNode
}

// AT LEVEL WITH AN OVERFLOW: the carrier goes to the overflow.
func TestOverflow_AtLevelRoutesToTheOverflow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	// A REAL RESOLVER. newTestDispatcher passes nil, and resolveSyntheticDestination
	// returns early on a nil resolver — so every case here would pass by never
	// reaching the code under test.
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})

	src := &nodes.Node{Name: "OVF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "source node")

	// Full: level 1, holding 1, and a spare position so it is not PHYSICALLY
	// full — the level is the only thing refusing.
	grpID, _ := levelledGroup(t, db, "OVF-FULL", "OVF-45x58", 1, 1, 2)
	levelledGroup(t, db, "OVF-SPILL", "OVF-45x58", 0, 0, 2)
	testdb.KanbanOverflow(t, db, grpID, "OVF-SPILL")

	got := pushInto(t, d, db, "ovf-1", "OVF-FULL")
	if got == "OVF-FULL" {
		t.Fatal("the order kept the full group as its destination — the overflow was not tried")
	}
	if got != "OVF-SPILL-PA" && got != "OVF-SPILL-PB" {
		t.Errorf("delivery = %q, want a position inside OVF-SPILL. The group is at its level "+
			"and has an overflow configured; parking is what happens when it does NOT", got)
	}
}

// NO OVERFLOW CONFIGURED: the push parks, holding its bin.
//
// STATED, NOT SOLVED. That is backpressure into whatever was pushing — an
// unloader that cannot put its carrier down stops draining — and it is the
// honest consequence of telling a group to hold exactly N and giving it nowhere
// to send the N+1st.
func TestOverflow_WithoutOneThePushParks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	// A REAL RESOLVER. newTestDispatcher passes nil, and resolveSyntheticDestination
	// returns early on a nil resolver — so every case here would pass by never
	// reaching the code under test.
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})

	src := &nodes.Node{Name: "OVF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "source node")
	levelledGroup(t, db, "OVF-NOSPILL", "OVF-NS-45x58", 1, 1, 2)

	got := pushInto(t, d, db, "ovf-2", "OVF-NOSPILL")
	if got != "OVF-NOSPILL" {
		t.Errorf("delivery = %q, want the group name kept so the order QUEUES against it. "+
			"With no overflow the push parks holding its bin, which is backpressure and is "+
			"the stated residual", got)
	}
}

// A FULL OVERFLOW PARKS TOO — one hop, not a chain.
//
// A chain is a loop with extra steps the first time two groups name each other,
// and "the carrier went three groups away from where anybody expected" is worse
// than a park an operator can see.
func TestOverflow_AFullOverflowDoesNotChain(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	// A REAL RESOLVER. newTestDispatcher passes nil, and resolveSyntheticDestination
	// returns early on a nil resolver — so every case here would pass by never
	// reaching the code under test.
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})

	src := &nodes.Node{Name: "OVF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "source node")

	first, _ := levelledGroup(t, db, "OVF-A", "OVF-CH-45x58", 1, 1, 2)
	second, _ := levelledGroup(t, db, "OVF-B", "OVF-CH-45x58", 1, 1, 2)
	third, _ := levelledGroup(t, db, "OVF-C", "OVF-CH-45x58", 0, 0, 2)
	testdb.KanbanOverflow(t, db, first, "OVF-B")
	testdb.KanbanOverflow(t, db, second, "OVF-C")
	_ = third

	got := pushInto(t, d, db, "ovf-3", "OVF-A")
	if got != "OVF-A" {
		t.Errorf("delivery = %q, want OVF-A kept (a park). B is also at its level, and one "+
			"hop means B's own overflow is NOT consulted — C has room and must not be "+
			"reached", got)
	}
}

// A MISCONFIGURED OVERFLOW PARKS RATHER THAN FAILING THE ORDER.
//
// The operator's action succeeds either way; the only difference is where the
// carrier ends up. A typo in a config field must not be able to fail an order
// that would otherwise have queued perfectly well.
func TestOverflow_MisconfiguredOverflowStillParks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	// A REAL RESOLVER. newTestDispatcher passes nil, and resolveSyntheticDestination
	// returns early on a nil resolver — so every case here would pass by never
	// reaching the code under test.
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})

	src := &nodes.Node{Name: "OVF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "source node")

	// A kanban's overflow is a foreign key that can name neither, so these are a
	// plain group's: full, with its property as written.
	for _, tc := range []struct {
		name, group, overflow string
	}{
		{"names a node that does not exist", "OVF-BAD", "OVF-NO-SUCH-NODE"},
		{"names itself", "OVF-SELF", "OVF-SELF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grpID, _ := levelledGroup(t, db, tc.group, tc.group+"-T", 0, 2, 2)
			testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropOverflowDestination, tc.overflow), "overflow")

			got := pushInto(t, d, db, "ovf-"+tc.group, tc.group)
			if got != tc.group {
				t.Errorf("delivery = %q, want %q kept (a park)", got, tc.group)
			}
		})
	}
}

// A PLAIN GROUP KEEPS ITS PROPERTY: no level, no kanbans row, and a full group's
// push still takes the overflow_destination it names. A kanban with a stale copy
// of the property reads its row instead: the row decides which is read.
func TestOverflow_APlainGroupKeepsItsProperty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})
	testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: "OVF-SRC", Enabled: true}), "source node")

	plain, _ := levelledGroup(t, db, "OVF-PLAIN", "OVF-PL-T", 0, 2, 2)
	levelledGroup(t, db, "OVF-PLAIN-SPILL", "OVF-PL-T", 0, 0, 2)
	testutil.MustNoErr(t, db.SetNodeProperty(plain, nodes.PropOverflowDestination, "OVF-PLAIN-SPILL"), "overflow")
	if got := pushInto(t, d, db, "ovf-plain", "OVF-PLAIN"); !strings.HasPrefix(got, "OVF-PLAIN-SPILL-") {
		t.Errorf("delivery = %q, want a position inside OVF-PLAIN-SPILL", got)
	}

	row, _ := levelledGroup(t, db, "OVF-ROW", "OVF-PL-T", 1, 1, 2)
	testutil.MustNoErr(t, db.SetNodeProperty(row, nodes.PropOverflowDestination, "OVF-PLAIN-SPILL"), "a stale property")
	if got := pushInto(t, d, db, "ovf-row", "OVF-ROW"); got != "OVF-ROW" {
		t.Errorf("delivery = %q, want OVF-ROW kept (a park): a kanban with no overflow on its row has none", got)
	}
}

// A GROUP BELOW ITS LEVEL NEVER REACHES THE OVERFLOW AT ALL.
func TestOverflow_BelowLevelIsUnaffected(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = testdb.SetupStandardData(t, db)
	// A REAL RESOLVER. newTestDispatcher passes nil, and resolveSyntheticDestination
	// returns early on a nil resolver — so every case here would pass by never
	// reaching the code under test.
	d := NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core",
		"shingo.dispatch", &DefaultResolver{DB: db})

	src := &nodes.Node{Name: "OVF-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "source node")

	grpID, _ := levelledGroup(t, db, "OVF-ROOM", "OVF-RM-45x58", 4, 1, 3)
	levelledGroup(t, db, "OVF-UNUSED", "OVF-RM-45x58", 0, 0, 2)
	testdb.KanbanOverflow(t, db, grpID, "OVF-UNUSED")

	got := pushInto(t, d, db, "ovf-room", "OVF-ROOM")
	if strings.HasPrefix(got, "OVF-UNUSED") {
		t.Errorf("delivery = %q — a group with room took the overflow path", got)
	}
	if !strings.HasPrefix(got, "OVF-ROOM") {
		t.Errorf("delivery = %q, want a position inside OVF-ROOM", got)
	}
}
