//go:build docker

package dispatch

import (
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// carrier_keys_blocks_docker_test.go — the carrier keys on the wire: what the
// fleet is sent for a cart whose family names a key, against Core's copy of
// the RDS scene, and what a plant with no key reads. The two-robot swap's wire
// and the finishes are the engine's (engine/carrier_keys_record_docker_test.go):
// its appends need the fleet's states, which only the engine wires.

// ckSized is the keys a sized location lists: the three defaults and both
// families' pairs.
func ckSized() []testdb.SceneBinTask {
	return append(testdb.DefaultSceneBinTasks(),
		testdb.SceneBinTask{Name: "jackload_4548", Operation: "JackLoad"},
		testdb.SceneBinTask{Name: "wait_4548", Operation: "Wait"},
		testdb.SceneBinTask{Name: "jackload_3230", Operation: "JackLoad"},
		testdb.SceneBinTask{Name: "wait_3230", Operation: "Wait"})
}

// ckType makes a bin type of a family of its own: its id and the family's.
func ckType(t *testing.T, db *store.DB, code string) (typeID, familyID int64) {
	t.Helper()
	familyID = keyFamily(t, db, code)
	bt, err := db.GetBinTypeByCode(code)
	bt = testutil.Must(t, bt, err, "read type "+code)
	return bt.ID, familyID
}

// ckCart stands a cart of a bin type, loaded with payload and confirmed, on a node.
func ckCart(t *testing.T, db *store.DB, node *nodes.Node, typeID int64, payload, label string) *bins.Bin {
	t.Helper()
	b := &bins.Bin{BinTypeID: typeID, Label: label, NodeID: &node.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create "+label)
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, payload, 10), "manifest "+label)
	testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
	got, err := db.GetBin(b.ID)
	return testutil.Must(t, got, err, "reload "+label)
}

// ckPlant is the wires' plant: a storage node and a line, both listing the
// sized keys; the two families, 45x48 and 32x30; a 45x48 cart at storage.
type ckPlant struct {
	db            *store.DB
	storage, line *nodes.Node
	payload       *payloads.Payload
	big, small    int64 // family ids
	cart          *bins.Bin
}

func newCKPlant(t *testing.T, db *store.DB) ckPlant {
	t.Helper()
	storage, line, bp := testdb.SetupTestData(t, db)
	bigType, big := ckType(t, db, "CK-4548")
	_, small := ckType(t, db, "CK-3230")
	return ckPlant{db: db, storage: storage, line: line, payload: bp, big: big, small: small,
		cart: ckCart(t, db, storage, bigType, bp.Code, "CK-CART-4548")}
}

// key keys both families, as the Bins page's save does.
func (p ckPlant) key(t *testing.T, d *Dispatcher) {
	t.Helper()
	testutil.MustNoErr(t, d.SaveCarrierFamilyKeys(p.big, "jackload_4548", "wait_4548"), "key 45x48")
	testutil.MustNoErr(t, d.SaveCarrierFamilyKeys(p.small, "jackload_3230", "wait_3230"), "key 32x30")
}

// retrieve files a retrieve of the plant's part to the line and dispatches it
// as the scanner does; it returns the order, sent.
func (p ckPlant) retrieve(t *testing.T, d *Dispatcher, uuid string) *orders.Order {
	t.Helper()
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve,
		PayloadCode: p.payload.Code, DeliveryNode: p.line.Name, Quantity: 1})
	o := dispatchSimpleViaScanner(t, d, p.db, uuid)
	if o.VendorOrderID == "" {
		t.Fatalf("fixture: retrieve %s was not sent (status %s)", uuid, o.Status)
	}
	return o
}

// simWire is each block the simulator holds for the vendor order, as
// "<block> <location> <binTask>" with the vendor order id cut off.
func simWire(sim *simulator.SimulatorBackend, vid string) []string {
	var out []string
	for _, b := range sim.BlocksForOrder(vid) {
		out = append(out, strings.TrimPrefix(b.BlockID, vid+"-")+" "+b.Location+" "+b.BinTask)
	}
	return out
}

// TestCarrierKeysWire_TheSealedRetrieve is the first two wires: a 45x48
// retrieve at a plant whose families name keys sends the lift's sized key and a
// default set-down; the same order at a plant that names none sends today's.
func TestCarrierKeysWire_TheSealedRetrieve(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		keyed bool
		want  []string
	}{
		{"keyed", true, []string{"b1 STORAGE-A1 jackload_4548", "b2 LINE1-IN JackUnload"}},
		{"unkeyed", false, []string{"b1 STORAGE-A1 JackLoad", "b2 LINE1-IN JackUnload"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			p := newCKPlant(t, db)
			testdb.SeedSceneBinTasks(t, db, p.storage.Name, ckSized()...)
			testdb.SeedSceneBinTasks(t, db, p.line.Name, ckSized()...)
			sim := simulator.New()
			d, _ := newTestDispatcher(t, db, sim)
			if tc.keyed {
				p.key(t, d)
			}
			o := p.retrieve(t, d, "ck-wire-"+tc.name)
			if got := simWire(sim, o.VendorOrderID); !slices.Equal(got, tc.want) {
				t.Errorf("the fleet was sent %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCarrierKeys_EveryKeyBlankIsToday: families on record with blank keys, and
// a scene that lists sized keys, send today's blocks, and the create reads
// nothing it did not read before: no family, no scene, its carts once (the read
// the robot group always made). A plant with no key does no added work.
func TestCarrierKeys_EveryKeyBlankIsToday(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	p := newCKPlant(t, db)
	testdb.SeedSceneBinTasks(t, db, p.storage.Name, ckSized()...)
	testdb.SeedSceneBinTasks(t, db, p.line.Name, ckSized()...)
	testutil.MustNoErr(t, db.SetCarrierFamilyKeys(p.big, "", ""), "blank keys")
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open the counting handle")
	t.Cleanup(func() { cdb.Close() })
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, cdb, backend)
	if _, err := d.ReloadCarrierKeys(); err != nil || len(d.carrierKeys()) != 0 {
		t.Fatalf("fixture: the keyed set is %v (%v), want empty", d.carrierKeys(), err)
	}

	src, dst, o := ckHeldRetrieve(t, d, p, "ck-blank")
	counter.Reset()
	_, err = d.DispatchDirect(o, src, dst)
	testutil.MustNoErr(t, err, "dispatch")
	stmts := counter.Statements()
	t.Logf("COST: unkeyed sealed create %d statements: %s", len(stmts), testutil.StatementBreakdown(stmts))

	reqs := backend.CreateRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d fleet creates, want 1", len(reqs))
	}
	got := make([]string, 0, len(reqs[0].Blocks))
	for _, b := range reqs[0].Blocks {
		got = append(got, b.Location+" "+b.BinTask)
	}
	if want := []string{"STORAGE-A1 JackLoad", "LINE1-IN JackUnload"}; !slices.Equal(got, want) {
		t.Errorf("the fleet was sent %q, want today's %q", got, want)
	}
	carts := 0
	for _, s := range stmts {
		if strings.Contains(s, "scene_points") || strings.Contains(s, "carrier_families") {
			t.Errorf("a plant with no key read the keys' sources: %s", s)
		}
		if strings.Contains(s, "b.id IN (SELECT ob.bin_id FROM order_bins ob WHERE ob.order_id") {
			carts++
		}
	}
	if carts != 1 {
		t.Errorf("the create read its carts %d times, want once (the robot group's read, shared)", carts)
	}
	if int64(len(stmts)) != costUnkeyedSealedCreate {
		t.Errorf("the unkeyed sealed create took %d statements, pinned at %d", len(stmts), costUnkeyedSealedCreate)
	}
}

// costUnkeyedSealedCreate is DispatchDirect's statements for a held retrieve
// at a plant with no key: 21 before the carrier keys (measured at d5b9fac7),
// less the second payload read the sealed path made for its load sequence,
// which it now resolves once; plus one for the fleet's yes, which writes
// `dispatched` with the job in a transaction of its own (BEGIN, COMMIT) where
// the vendor id was one bare UPDATE (TestKanbanCost_ScannerParkedPass's +1);
// less the node reads its lane questions no longer make (2026-11-25): they ask
// LaneOf of the nodes the create holds, where LaneForNode read each again by id.
const costUnkeyedSealedCreate = 13

// ckHeldRetrieve files a retrieve of the plant's cart and takes it to the
// moment the scanner hands it to DispatchDirect: found, claimed, held, its
// drop reserved. It returns the source, the destination and the order.
func ckHeldRetrieve(t *testing.T, d *Dispatcher, p ckPlant, uuid string) (*nodes.Node, *nodes.Node, *orders.Order) {
	t.Helper()
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid, OrderType: protocol.OrderTypeRetrieve,
		PayloadCode: p.payload.Code, DeliveryNode: p.line.Name, Quantity: 1})
	o := testdb.RequireOrder(t, p.db, uuid)
	testutil.MustNoErr(t, d.binManifest.ClaimForDispatch(p.cart.ID, o.ID, o.RemainingUOP), "claim")
	testdb.HoldCart(t, p.db, o.ID, p.cart.ID)
	testutil.MustNoErr(t, p.db.UpdateOrderSourceNode(o.ID, p.storage.Name), "source")
	o = testdb.RequireOrder(t, p.db, uuid)
	if dest := d.ReserveStorageDropoff(o); dest.Refused() {
		t.Fatalf("fixture: reserve the drop: %v", dest.Err)
	}
	return p.storage, p.line, o
}

// TestCarrierKeys_ALocationTheSceneDoesNotListGetsTheDefault: the lift's sized
// key is kept only where Core's scene copy lists it at the lift's location, in
// every row it holds for it. A copy that lists the defaults only, no row for
// the location, a binTask property with no value, and a location two areas
// hold of which one lacks the key each send the default, with no error and no
// log line. Not parallel: it reads the package logger.
func TestCarrierKeys_ALocationTheSceneDoesNotListGetsTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scene func(t *testing.T, db *store.DB, loc string)
	}{
		{"the copy lists the defaults only", func(t *testing.T, db *store.DB, loc string) {
			testdb.SeedSceneBinTasks(t, db, loc, testdb.DefaultSceneBinTasks()...)
		}},
		{"the copy has no row for the location", func(*testing.T, *store.DB, string) {}},
		{"the binTask property has no value", func(t *testing.T, db *store.DB, loc string) {
			testdb.SeedSceneBinTasks(t, db, loc)
		}},
		{"a second area's row lacks the key", func(t *testing.T, db *store.DB, loc string) {
			testdb.SeedSceneBinTasks(t, db, loc, ckSized()...)
			testdb.SeedSceneBinTasks(t, db, "CK-AREA2", testdb.DefaultSceneBinTasks()...)
			rows, err := db.SceneLocationsNamed([]string{"CK-AREA2"})
			testutil.MustNoErr(t, err, "read the defaults-only row")
			rows[0].AreaName, rows[0].InstanceName = "Area-02", loc
			testutil.MustNoErr(t, db.UpsertScenePoint(rows[0]), "the location in a second area")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			p := newCKPlant(t, db)
			tc.scene(t, db, p.storage.Name)
			testdb.SeedSceneBinTasks(t, db, p.line.Name, ckSized()...)
			sim := simulator.New()
			d, _ := newTestDispatcher(t, db, sim)
			p.key(t, d)
			logs := captureDispatchLog(t)
			o := p.retrieve(t, d, "ck-scene")
			if got, want := simWire(sim, o.VendorOrderID), []string{"b1 STORAGE-A1 JackLoad", "b2 LINE1-IN JackUnload"}; !slices.Equal(got, want) {
				t.Errorf("the fleet was sent %q, want the default lift %q", got, want)
			}
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "binTask") || strings.Contains(line, "carrier key") {
					t.Errorf("a location without the key logged: %s", line)
				}
			}
		})
	}
}

// TestCarrierKeys_AFailedSceneReadSendsTheDefaultOnce: when the scene copy
// cannot be read, every block of the segment sends the default and the
// dispatcher says so in one line. Not parallel: it reads the package logger.
func TestCarrierKeys_AFailedSceneReadSendsTheDefaultOnce(t *testing.T) {
	db := testDB(t)
	p := newCKPlant(t, db)
	testdb.SeedSceneBinTasks(t, db, p.storage.Name, ckSized()...)
	_, err := db.Exec(`ALTER TABLE scene_points RENAME TO scene_points_unreadable`)
	testutil.MustNoErr(t, err, "make the scene copy unreadable")
	sim := simulator.New()
	d, _ := newTestDispatcher(t, db, sim)
	p.key(t, d)
	logs := captureDispatchLog(t)
	o := p.retrieve(t, d, "ck-scene-down")
	if got, want := simWire(sim, o.VendorOrderID), []string{"b1 STORAGE-A1 JackLoad", "b2 LINE1-IN JackUnload"}; !slices.Equal(got, want) {
		t.Errorf("the fleet was sent %q, want the defaults %q", got, want)
	}
	said := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "binTask keys at [STORAGE-A1]") && strings.Contains(line, "send the default keys") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the failed read was said %d times, want once:\n%s", said, logs.String())
	}
}

// ckAppendOrder is a staged order already sent its create (b1, b2) whose
// stored plan stands a bare wait before its segment:
// [pickup SRC, drop STG, wait(bare), pickup STG, drop LINE]. Its one cart, a
// 45x48, is lifted twice (rows at 0 and 3). The segment's first step is plan
// position 3 (segmentStart) and block b3 (blockOffset 2).
func ckAppendOrder(t *testing.T, db *store.DB, backend fleet.Backend, p ckPlant, uuid string) *orders.Order {
	t.Helper()
	plan := []resolvedStep{ckPick(p.storage.Name), ckDrop("CK-STG"), {Action: protocol.ActionWait},
		ckPick("CK-STG"), ckDrop(p.line.Name)}
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status, o.PayloadCode = uuid, OrderTypeComplex, StatusStaged, p.payload.Code
		o.SourceNode, o.DeliveryNode, o.ProcessNode = p.storage.Name, p.line.Name, p.line.Name
		o.StepsJSON = string(mustJSON(t, plan))
	})
	vid := "V-" + uuid
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, vid, "WAITING", ""), "vendor")
	testdb.SeedCartRow(t, db, o.ID, p.cart.ID, 0, 1)
	testdb.SeedCartRow(t, db, o.ID, p.cart.ID, 3, 4)
	_, err := backend.CreateOrder(fleet.CreateOrderRequest{OrderID: vid, Blocks: []fleet.OrderBlock{
		{BlockID: vid + "-b1", Location: p.storage.Name, BinTask: "jackload_4548", Action: protocol.ActionPickup},
		{BlockID: vid + "-b2", Location: "CK-STG", BinTask: "JackUnload", Action: protocol.ActionDropoff},
	}})
	testutil.MustNoErr(t, err, "the create the fleet already holds")
	return prReload(t, db, o.ID)
}

// ckAppend appends the order's segment after its wait, through the one append.
func ckAppend(t *testing.T, d *Dispatcher, o *orders.Order) {
	t.Helper()
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatal("fixture: the plan does not read")
	}
	segment, moreWaits, blockOffset := splitSegment(steps, o.WaitIndex)
	testutil.MustNoErr(t, d.appendSegmentAndAdvance(o, segment, moreWaits, blockOffset, "carrier keys test"), "append")
}

// TestCarrierKeys_AnAppendNamesItsStepByPlanPosition: a bare wait sends no
// block, so the segment after it starts at plan position 3 but block b3. The
// relay's lift is the row at pick step 3: it sends its family's key, on b3.
// Read at the block number (2) the lift would find no row and send JackLoad.
func TestCarrierKeys_AnAppendNamesItsStepByPlanPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	p := newCKPlant(t, db)
	testdb.SeedSceneBinTasks(t, db, "CK-STG", ckSized()...)
	testdb.SeedSceneBinTasks(t, db, p.line.Name, ckSized()...)
	sim := simulator.New()
	d, _ := newTestDispatcher(t, db, sim)
	p.key(t, d)
	o := ckAppendOrder(t, db, sim, p, "ck-append")
	ckAppend(t, d, o)
	want := []string{"b1 STORAGE-A1 jackload_4548", "b2 CK-STG JackUnload", "b3 CK-STG jackload_4548", "b4 LINE1-IN JackUnload"}
	if got := simWire(sim, o.VendorOrderID); !slices.Equal(got, want) {
		t.Errorf("the fleet holds %q, want %q", got, want)
	}
}

// TestCarrierKeys_WhatAKeyCosts measures, per order shape, the statements and
// the fleet calls of the call that sends it, at a plant with no key and at one
// whose families name keys. Unkeyed, a sealed create, a complex create and an
// append cost what they cost before the carrier keys (measured at d5b9fac7:
// 21, 60 and 6), less the sealed create's second payload read, plus one on a
// create for the fleet's yes's own transaction, less the node re-reads its
// lane questions no longer make (LaneOf on the nodes in hand, 2026-11-25) and,
// for the complex create, its walks over the plan (one read of the plan's
// nodes, attemptNodes) and its replay walk's (one read of its steps' nodes,
// walkNodes: 43 -> 42). Keyed, a create
// adds the scene read (1) and an append its carts' read and the scene read
// (2); a sealed create of a payload with a load sequence adds nothing (the
// sequence names its lift, and the set-down sends the default). No shape adds
// a fleet call.
func TestCarrierKeys_WhatAKeyCosts(t *testing.T) {
	t.Parallel()
	type cost struct{ stmts, fleet int }
	// shape sets an order up and returns the call that sends it.
	type shape func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, backend fleet.Backend) func()
	measure := func(t *testing.T, keyed bool, sh shape) cost {
		db, cfg := testdb.OpenWithConfig(t)
		p := newCKPlant(t, db)
		for _, loc := range []string{p.storage.Name, p.line.Name, "CK-STG"} {
			testdb.SeedSceneBinTasks(t, db, loc, ckSized()...)
		}
		cdb, counter, err := store.OpenCounting(cfg)
		testutil.MustNoErr(t, err, "open the counting handle")
		t.Cleanup(func() { cdb.Close() })
		backend := testdb.NewTrackingBackend()
		d, _ := newTestDispatcher(t, cdb, backend)
		// Loaded as the engine loads it at start: an unkeyed plant holds an
		// empty set, not none.
		_, err = d.ReloadCarrierKeys()
		testutil.MustNoErr(t, err, "load the keyed families")
		if keyed {
			p.key(t, d)
		}
		send := sh(t, d, db, p, backend)
		calls := func() int { return len(backend.CreateRequests()) + len(backend.ReleaseCalls()) }
		before := calls()
		counter.Reset()
		send()
		c := cost{stmts: int(counter.Count()), fleet: calls() - before}
		t.Logf("COST: keyed=%v %d statements, %d fleet calls: %s", keyed, c.stmts, c.fleet,
			testutil.StatementBreakdown(counter.Statements()))
		return c
	}
	sealed := func(uuid string) shape {
		return func(t *testing.T, d *Dispatcher, _ *store.DB, p ckPlant, _ fleet.Backend) func() {
			src, dst, o := ckHeldRetrieve(t, d, p, uuid)
			return func() {
				_, err := d.DispatchDirect(o, src, dst)
				testutil.MustNoErr(t, err, "dispatch")
			}
		}
	}
	for _, tc := range []struct {
		name           string
		unkeyed, added int
		sh             shape
	}{
		{"a sealed create", costUnkeyedSealedCreate, 1, sealed("ck-cost-sealed")},
		{"a sealed create with a load sequence", 17, 0,
			func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, backend fleet.Backend) func() {
				_, err := db.Exec(`UPDATE payloads SET advanced_load_sequence = 'Child cart interlock' WHERE code = $1`, p.payload.Code)
				testutil.MustNoErr(t, err, "the payload's load sequence")
				return sealed("ck-cost-sequence")(t, d, db, p, backend)
			}},
		{"a complex create", 42, 1, func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, _ fleet.Backend) func() {
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: "ck-cost-complex",
				PayloadCode: p.payload.Code, Quantity: 1, ProcessNode: p.line.Name,
				Steps: []protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: p.storage.Name},
					{Action: protocol.ActionDropoff, Node: p.line.Name}}})
			o := testdb.RequireOrder(t, db, "ck-cost-complex")
			return func() { testutil.MustNoErr(t, d.DispatchPreparedComplex(o), "dispatch") }
		}},
		{"an append", 6, 2, func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, backend fleet.Backend) func() {
			o := ckAppendOrder(t, db, backend, p, "ck-cost-append")
			return func() { ckAppend(t, d, o) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unkeyed, keyed := measure(t, false, tc.sh), measure(t, true, tc.sh)
			t.Logf("COST %s: unkeyed %d statements, keyed %d (+%d); fleet calls %d and %d", tc.name,
				unkeyed.stmts, keyed.stmts, keyed.stmts-unkeyed.stmts, unkeyed.fleet, keyed.fleet)
			if unkeyed.stmts != tc.unkeyed {
				t.Errorf("a plant with no key: %d statements, pinned at %d", unkeyed.stmts, tc.unkeyed)
			}
			if keyed.stmts-unkeyed.stmts != tc.added || keyed.fleet != unkeyed.fleet {
				t.Errorf("a keyed plant: %d statements and %d fleet calls; unkeyed: %d and %d; want +%d statements, +0 calls",
					keyed.stmts, keyed.fleet, unkeyed.stmts, unkeyed.fleet, tc.added)
			}
		})
	}
}
