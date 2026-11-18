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
// which it now resolves once.
const costUnkeyedSealedCreate = 20

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
// key is kept only where Core's scene copy lists it at the lift's location. A
// copy that lists the defaults only, no row for the location, and a binTask
// property with no value each send the default, with no error and no log line.
// Not parallel: it reads the package logger.
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
// the fleet calls a keyed plant adds over an unkeyed one: a sealed create and a
// complex create add the scene read (1), an append adds its carts' read and the
// scene read (2), and no shape adds a fleet call. The unkeyed numbers are each
// shape's cost with the builder reading nothing.
func TestCarrierKeys_WhatAKeyCosts(t *testing.T) {
	t.Parallel()
	type cost struct{ stmts, fleet int }
	measure := func(t *testing.T, keyed bool, shape func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant,
		backend *testdb.MockTrackingBackend, counter *store.QueryCounter)) cost {
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
		if keyed {
			p.key(t, d)
		}
		shape(t, d, db, p, backend, counter)
		n := counter.Count()
		calls := len(backend.CreateRequests()) + len(backend.ReleaseCalls())
		t.Logf("COST: keyed=%v %d statements, %d fleet calls: %s", keyed, n, calls,
			testutil.StatementBreakdown(counter.Statements()))
		return cost{stmts: int(n), fleet: calls}
	}
	for _, tc := range []struct {
		name  string
		added int
		shape func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, backend *testdb.MockTrackingBackend,
			counter *store.QueryCounter)
	}{
		{"a sealed create", 1, func(t *testing.T, d *Dispatcher, _ *store.DB, p ckPlant, _ *testdb.MockTrackingBackend,
			counter *store.QueryCounter) {
			src, dst, o := ckHeldRetrieve(t, d, p, "ck-cost-sealed")
			counter.Reset()
			_, err := d.DispatchDirect(o, src, dst)
			testutil.MustNoErr(t, err, "dispatch")
		}},
		{"a complex create", 1, func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, _ *testdb.MockTrackingBackend,
			counter *store.QueryCounter) {
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: "ck-cost-complex",
				PayloadCode: p.payload.Code, Quantity: 1, ProcessNode: p.line.Name,
				Steps: []protocol.ComplexOrderStep{{Action: protocol.ActionPickup, Node: p.storage.Name},
					{Action: protocol.ActionDropoff, Node: p.line.Name}}})
			o := testdb.RequireOrder(t, db, "ck-cost-complex")
			counter.Reset()
			testutil.MustNoErr(t, d.DispatchPreparedComplex(o), "dispatch")
		}},
		{"an append", 2, func(t *testing.T, d *Dispatcher, db *store.DB, p ckPlant, backend *testdb.MockTrackingBackend,
			counter *store.QueryCounter) {
			o := ckAppendOrder(t, db, backend, p, "ck-cost-append")
			counter.Reset()
			ckAppend(t, d, o)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unkeyed, keyed := measure(t, false, tc.shape), measure(t, true, tc.shape)
			if keyed.stmts-unkeyed.stmts != tc.added || keyed.fleet != unkeyed.fleet {
				t.Errorf("a keyed plant: %d statements and %d fleet calls; unkeyed: %d and %d; want +%d statements, +0 calls",
					keyed.stmts, keyed.fleet, unkeyed.stmts, unkeyed.fleet, tc.added)
			}
			t.Logf("COST %s: unkeyed %d statements, keyed %d (+%d); fleet calls %d and %d", tc.name,
				unkeyed.stmts, keyed.stmts, keyed.stmts-unkeyed.stmts, unkeyed.fleet, keyed.fleet)
		})
	}
}
