//go:build docker

package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// block_record_pin_docker_test.go — WHAT EVERY BLOCK SENDS, AND WHAT ITS FINISH
// DOES, PINNED ONE SHAPE AT A TIME.
//
// Each case builds one order shape, sends it through the dispatcher to the
// fleet simulator, and finishes its blocks one at a time as a fleet reports
// them. The transcript records both sides:
//
//   - the wire: each block as it reaches the fleet, on the create or on an
//     append, by its id under the vendor order, its location and its binTask;
//   - the finish: for each finished block, the arm the engine ran on it, read
//     from what it emitted (lift: the cart entered _TRANSIT; store: the cart
//     left _TRANSIT for the block's node; none), where every cart of the case
//     is recorded after it, and the order's steps_done after it.
//
// A transcript is compared whole. It describes what the tree does, not what it
// should do: a diff is a change in what a plant sees, to be explained, never
// re-recorded to make the case pass.

// pinRig drives one case and keeps its transcript.
type pinRig struct {
	t       *testing.T
	db      *store.DB
	sim     *simulator.SimulatorBackend
	eng     *Engine
	transit int64
	orders  map[string]*pinOrder
	tracked []string
	carts   []pinCart
	lines   []string

	mu     sync.Mutex
	lifted map[int64][]int64 // order id -> carts that entered _TRANSIT this step
	stored map[int64][]int64 // order id -> carts that left _TRANSIT this step
}

type pinOrder struct {
	label string
	id    int64
	vid   string
	sent  int // blocks transcribed
	done  int // blocks finished
}

type pinCart struct {
	label string
	id    int64
}

// newPinRig attaches a recorder to an engine already started on sim.
func newPinRig(t *testing.T, db *store.DB, sim *simulator.SimulatorBackend, eng *Engine) *pinRig {
	t.Helper()
	transit, err := db.GetNodeByDotName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	r := &pinRig{t: t, db: db, sim: sim, eng: eng, transit: transit.ID, orders: map[string]*pinOrder{},
		lifted: map[int64][]int64{}, stored: map[int64][]int64{}}
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[EventType, BinEnteredTransitEvent]) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.lifted[evt.Payload.OrderID] = append(r.lifted[evt.Payload.OrderID], evt.Payload.BinID)
	}, EventBinEnteredTransit)
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[EventType, BinUpdatedEvent]) {
		p := evt.Payload
		if p.Action != BinActionMoved || p.FromNodeID != r.transit || p.ToNodeID == r.transit {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stored[p.OrderID] = append(r.stored[p.OrderID], p.BinID)
	}, EventBinUpdated)
	return r
}

// newPinSim is a fresh engine on the simulator with a recorder on it.
func newPinSim(t *testing.T, db *store.DB) *pinRig {
	t.Helper()
	sim := simulator.New()
	return newPinRig(t, db, sim, newTestEngine(t, db, sim))
}

func (r *pinRig) say(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// cart names a cart the transcript reports.
func (r *pinRig) cart(label string, b *bins.Bin) {
	r.carts = append(r.carts, pinCart{label: label, id: b.ID})
}

// track names an order the transcript follows, and transcribes what it has
// sent so far.
func (r *pinRig) track(label string, id int64) {
	r.t.Helper()
	if _, ok := r.orders[label]; ok {
		r.t.Fatalf("pin: order label %s used twice", label)
	}
	r.orders[label] = &pinOrder{label: label, id: id}
	r.tracked = append(r.tracked, label)
	r.sync()
}

func (r *pinRig) trackUUID(label, uuid string) {
	r.t.Helper()
	o, err := r.db.GetOrderByUUID(uuid)
	testutil.MustNoErr(r.t, err, "read order "+uuid)
	if o == nil {
		r.t.Fatalf("pin: no order %s", uuid)
	}
	r.track(label, o.ID)
}

// sync lets the work a step started finish (the after-queue, a fulfillment
// pass), then transcribes every block a tracked order has sent since.
func (r *pinRig) sync() {
	r.t.Helper()
	r.eng.after.settle()
	r.eng.RunFulfillmentScan()
	r.eng.after.settle()
	for _, label := range r.tracked {
		po := r.orders[label]
		o, err := r.db.GetOrder(po.id)
		testutil.MustNoErr(r.t, err, "read order "+label)
		if o.VendorOrderID != po.vid {
			if po.vid != "" {
				r.say("%s vendor order replaced", label)
			}
			po.vid, po.sent, po.done = o.VendorOrderID, 0, 0
		}
		if po.vid == "" {
			continue
		}
		blocks := r.sim.BlocksForOrder(po.vid)
		for i := po.sent; i < len(blocks); i++ {
			kind := "append"
			if po.sent == 0 {
				kind = "create"
			}
			b := blocks[i]
			r.say("%s %s %s %s %s", label, kind, r.blockName(po, b.BlockID), b.Location, b.BinTask)
		}
		if len(blocks) > po.sent {
			po.sent = len(blocks)
		}
	}
}

func (r *pinRig) blockName(po *pinOrder, blockID string) string {
	return strings.TrimPrefix(blockID, po.vid+"-")
}

func (r *pinRig) order(label string) *pinOrder {
	r.t.Helper()
	po, ok := r.orders[label]
	if !ok {
		r.t.Fatalf("pin: no tracked order %s", label)
	}
	return po
}

// finish reports the order's next block FINISHED, as a fleet reports blocks:
// in the order it was sent them.
func (r *pinRig) finish(label string) {
	r.t.Helper()
	po := r.order(label)
	blocks := r.sim.BlocksForOrder(po.vid)
	if po.done >= len(blocks) {
		r.t.Fatalf("pin: %s has no block left to finish (%d sent)", label, len(blocks))
	}
	r.complete(po, blocks[po.done], "finish")
	po.done++
}

// replay reports the order's last finished block FINISHED again, as a
// restarted poller does.
func (r *pinRig) replay(label string) {
	r.t.Helper()
	po := r.order(label)
	if po.done == 0 {
		r.t.Fatalf("pin: %s has finished no block to replay", label)
	}
	r.complete(po, r.sim.BlocksForOrder(po.vid)[po.done-1], "replay")
}

func (r *pinRig) complete(po *pinOrder, b simulator.BlockView, verb string) {
	r.t.Helper()
	r.mu.Lock()
	r.lifted, r.stored = map[int64][]int64{}, map[int64][]int64{}
	r.mu.Unlock()
	emitted := r.sim.CompleteBlock(po.vid, b.BlockID, b.Location, b.BinTask, 0, 0)
	r.mu.Lock()
	arm := r.arm(po.id)
	r.mu.Unlock()
	if !emitted {
		arm = "not emitted"
	}
	r.say("%s %s %s %s %s -> %s | steps_done %d | %s", po.label, verb, r.blockName(po, b.BlockID), b.Location,
		b.BinTask, arm, r.stepsDone(po.id), r.where())
	r.sync()
}

// arm names what the engine did with the block, from what it emitted. Called
// with r.mu held.
func (r *pinRig) arm(orderID int64) string {
	var parts []string
	for _, id := range r.lifted[orderID] {
		parts = append(parts, "lift "+r.cartName(id))
	}
	for _, id := range r.stored[orderID] {
		parts = append(parts, "store "+r.cartName(id))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func (r *pinRig) cartName(id int64) string {
	for _, c := range r.carts {
		if c.id == id {
			return c.label
		}
	}
	return fmt.Sprintf("bin-%d", id)
}

func (r *pinRig) orderName(id int64) string {
	for _, label := range r.tracked {
		if r.orders[label].id == id {
			return label
		}
	}
	return "other"
}

func (r *pinRig) stepsDone(orderID int64) int {
	r.t.Helper()
	o, err := r.db.GetOrder(orderID)
	testutil.MustNoErr(r.t, err, "read steps_done")
	return o.StepsDone
}

func (r *pinRig) status(label string) string {
	r.t.Helper()
	o, err := r.db.GetOrder(r.order(label).id)
	testutil.MustNoErr(r.t, err, "read status")
	return string(o.Status)
}

// where is every cart of the case, where it is recorded and who holds it.
func (r *pinRig) where() string {
	r.t.Helper()
	parts := make([]string, 0, len(r.carts))
	for _, c := range r.carts {
		b, err := r.db.GetBin(c.id)
		testutil.MustNoErr(r.t, err, "read cart "+c.label)
		s := c.label + "@" + b.NodeName
		if b.ClaimedBy != nil {
			s += "[" + r.orderName(*b.ClaimedBy) + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// run reports the order RUNNING.
func (r *pinRig) run(label string) {
	r.t.Helper()
	r.sim.DriveState(r.order(label).vid, "RUNNING")
	r.sync()
}

// stage reports the order RUNNING, then WAITING at its wait.
func (r *pinRig) stage(label string) {
	r.t.Helper()
	po := r.order(label)
	r.sim.DriveState(po.vid, "RUNNING")
	r.sim.DriveState(po.vid, "WAITING")
	r.say("%s waiting -> %s", label, r.status(label))
	r.sync()
}

// release is the station's release of the order's wait.
func (r *pinRig) release(label, uuid string) {
	r.t.Helper()
	r.eng.Dispatcher().HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{OrderUUID: uuid})
	r.say("%s released -> %s", label, r.status(label))
	r.sync()
}

// deliver reports the order RUNNING, then FINISHED.
func (r *pinRig) deliver(label string) {
	r.t.Helper()
	po := r.order(label)
	r.sim.DriveState(po.vid, "RUNNING")
	r.sim.DriveState(po.vid, "FINISHED")
	r.eng.after.settle()
	r.say("%s finished -> %s | steps_done %d | %s", label, r.status(label), r.stepsDone(po.id), r.where())
	r.sync()
}

func (r *pinRig) cancel(label, uuid string) {
	r.t.Helper()
	r.eng.Dispatcher().HandleOrderCancel(testEnvelope(), &protocol.OrderCancel{OrderUUID: uuid, Reason: "pin"})
	r.say("%s cancelled -> %s | %s", label, r.status(label), r.where())
	r.sync()
}

func (r *pinRig) transcript() string {
	return strings.Join(r.lines, "\n")
}

// pinCase is one shape and the transcript it must produce.
type pinCase struct {
	name string
	run  func(t *testing.T) *pinRig
	want string
}

func runPinCases(t *testing.T, cases []pinCase) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.name] {
			t.Fatalf("pin case %q named twice", c.name)
		}
		seen[c.name] = true
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.run(t).transcript()
			if got != c.want {
				t.Errorf("the transcript changed: a block sent or finished does something it did not before.\n"+
					"--- got ---\n%s\n--- want ---\n%s", got, c.want)
			}
		})
	}
}

// pinLines joins a want's lines.
func pinLines(lines ...string) string { return strings.Join(lines, "\n") }

func TestBlockRecordPin_TheWireAndTheFinish(t *testing.T) {
	t.Parallel()
	runPinCases(t, []pinCase{
		{name: "plain retrieve", run: pinPlainRetrieve, want: pinLines(
			"retrieve create b1 STORAGE-A1 JackLoad",
			"retrieve create b2 LINE1-IN JackUnload",
			"retrieve finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve finished -> delivered | steps_done 2 | A@LINE1-IN",
		)},
		{name: "plain retrieve, a replayed block", run: pinPlainRetrieveReplay, want: pinLines(
			"retrieve create b1 STORAGE-A1 JackLoad",
			"retrieve create b2 LINE1-IN JackUnload",
			"retrieve finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve replay b1 STORAGE-A1 JackLoad -> none | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve replay b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve finished -> delivered | steps_done 2 | A@LINE1-IN",
		)},
		{name: "plain retrieve, a block finishing after the order ended", run: pinPlainRetrieveAfterEnd, want: pinLines(
			"retrieve create b1 STORAGE-A1 JackLoad",
			"retrieve create b2 LINE1-IN JackUnload",
			"retrieve cancelled -> cancelled | A@STORAGE-A1",
			"retrieve finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT",
			"retrieve finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT",
		)},
		{name: "empty-carrier retrieve", run: pinEmptyRetrieve, want: pinLines(
			"empty create b1 STORAGE-A1 JackLoad",
			"empty create b2 LINE1-IN JackUnload",
			"empty finish b1 STORAGE-A1 JackLoad -> lift E | steps_done 1 | E@_TRANSIT[empty]",
			"empty finish b2 LINE1-IN JackUnload -> none | steps_done 2 | E@_TRANSIT[empty]",
			"empty finished -> delivered | steps_done 2 | E@LINE1-IN",
		)},
		{name: "staged release [wait X, pickup X, drop]", run: pinStagedRelease, want: pinLines(
			"removal create b1 LINE1-IN Wait",
			"removal finish b1 LINE1-IN Wait -> none | steps_done 1 | OLD@LINE1-IN[removal]",
			"removal waiting -> staged",
			"removal released -> in_transit",
			"removal append b2 LINE1-IN JackLoad",
			"removal append b3 OUTBOUND-DEST JackUnload",
			"removal finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | OLD@_TRANSIT[removal]",
			"removal finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | OLD@_TRANSIT[removal]",
			"removal finished -> delivered | steps_done 3 | OLD@OUTBOUND-DEST",
		)},
		{name: "single-robot swap", run: pinSingleRobotSwap, want: pinLines(
			"swap create b1 STORAGE-A1 JackLoad",
			"swap create b2 INBOUND-STAGING JackUnload",
			"swap create b3 LINE1-IN Wait",
			"swap finish b1 STORAGE-A1 JackLoad -> lift FRESH | steps_done 1 | FRESH@_TRANSIT[swap] OLD@LINE1-IN[swap]",
			"swap finish b2 INBOUND-STAGING JackUnload -> store FRESH | steps_done 2 | FRESH@INBOUND-STAGING[swap] OLD@LINE1-IN[swap]",
			"swap finish b3 LINE1-IN Wait -> none | steps_done 3 | FRESH@INBOUND-STAGING[swap] OLD@LINE1-IN[swap]",
			"swap waiting -> staged",
			"swap released -> in_transit",
			"swap append b4 LINE1-IN JackLoad",
			"swap append b5 OUTBOUND-STAGING JackUnload",
			"swap append b6 INBOUND-STAGING JackLoad",
			"swap append b7 LINE1-IN JackUnload",
			"swap append b8 OUTBOUND-STAGING JackLoad",
			"swap append b9 OUTBOUND-DEST JackUnload",
			"swap finish b4 LINE1-IN JackLoad -> lift OLD | steps_done 4 | FRESH@INBOUND-STAGING[swap] OLD@_TRANSIT[swap]",
			"swap finish b5 OUTBOUND-STAGING JackUnload -> store OLD | steps_done 5 | FRESH@INBOUND-STAGING[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b6 INBOUND-STAGING JackLoad -> lift FRESH | steps_done 6 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b7 LINE1-IN JackUnload -> store FRESH | steps_done 7 | FRESH@LINE1-IN[swap] OLD@OUTBOUND-STAGING[swap]",
			"swap finish b8 OUTBOUND-STAGING JackLoad -> lift OLD | steps_done 8 | FRESH@LINE1-IN[swap] OLD@_TRANSIT[swap]",
			"swap finish b9 OUTBOUND-DEST JackUnload -> none | steps_done 9 | FRESH@LINE1-IN[swap] OLD@_TRANSIT[swap]",
			"swap finished -> delivered | steps_done 9 | FRESH@LINE1-IN[swap] OLD@OUTBOUND-DEST",
		)},
		{name: "carryover round trip, one cart lifted twice", run: pinCarryover, want: pinLines(
			"carry create b1 LINE1-IN JackLoad",
			"carry create b2 INBOUND-STAGING JackUnload",
			"carry create b3 INBOUND-STAGING Wait",
			"carry finish b1 LINE1-IN JackLoad -> lift TOOL | steps_done 1 | TOOL@_TRANSIT[carry]",
			"carry finish b2 INBOUND-STAGING JackUnload -> store TOOL | steps_done 2 | TOOL@INBOUND-STAGING[carry]",
			"carry finish b3 INBOUND-STAGING Wait -> none | steps_done 3 | TOOL@INBOUND-STAGING[carry]",
			"carry waiting -> staged",
			"carry released -> in_transit",
			"carry append b4 INBOUND-STAGING JackLoad",
			"carry append b5 LINE1-IN JackUnload",
			"carry finish b4 INBOUND-STAGING JackLoad -> lift TOOL | steps_done 4 | TOOL@_TRANSIT[carry]",
			"carry finish b5 LINE1-IN JackUnload -> none | steps_done 5 | TOOL@_TRANSIT[carry]",
			"carry finished -> delivered | steps_done 5 | TOOL@LINE1-IN",
		)},
		{name: "advanced load sequence, sealed", run: pinLoadSequenceSealed, want: pinLines(
			"retrieve create b1 STORAGE-A1 Go_AP1",
			"retrieve create b1-1 STORAGE-A1 Spin_90",
			"retrieve create b1-2 STORAGE-A1 load",
			"retrieve create b1-3 STORAGE-A1 Spin_inverse_90",
			"retrieve create b2 LINE1-IN JackUnload",
			"retrieve finish b1 STORAGE-A1 Go_AP1 -> none | steps_done 0 | A@STORAGE-A1[retrieve]",
			"retrieve finish b1-1 STORAGE-A1 Spin_90 -> none | steps_done 0 | A@STORAGE-A1[retrieve]",
			"retrieve finish b1-2 STORAGE-A1 load -> lift A | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve finish b1-3 STORAGE-A1 Spin_inverse_90 -> none | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve finished -> delivered | steps_done 2 | A@LINE1-IN",
		)},
		{name: "advanced load sequence, gated", run: pinLoadSequenceGated, want: pinLines(
			"lane slots PINGQ-S0 PINGQ-S1",
			"store create b1 LINE1-IN Go_AP1",
			"store create b1-1 LINE1-IN Spin_90",
			"store create b1-2 LINE1-IN load",
			"store create b1-3 LINE1-IN Spin_inverse_90",
			"store create b2 PINGQ-GRP-WAIT Wait",
			"store create b3 PINGQ-S1 JackUnload",
			"store finish b1 LINE1-IN Go_AP1 -> none | steps_done 0 | A@LINE1-IN[store]",
			"store finish b1-1 LINE1-IN Spin_90 -> none | steps_done 0 | A@LINE1-IN[store]",
			"store finish b1-2 LINE1-IN load -> lift A | steps_done 1 | A@_TRANSIT[store]",
			"store finish b1-3 LINE1-IN Spin_inverse_90 -> none | steps_done 1 | A@_TRANSIT[store]",
			"store finish b2 PINGQ-GRP-WAIT Wait -> none | steps_done 2 | A@_TRANSIT[store]",
			"store finish b3 PINGQ-S1 JackUnload -> none | steps_done 3 | A@_TRANSIT[store]",
			"store finished -> delivered | steps_done 3 | A@PINGQ-S1",
		)},
		{name: "manual move", run: pinManualMove, want: pinLines(
			"move create b1 STORAGE-A1 JackLoad",
			"move create b2 LINE1-IN JackUnload",
			"move finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT[move]",
			"move finish b2 LINE1-IN JackUnload -> none | steps_done 2 | A@_TRANSIT[move]",
			"move finished -> delivered | steps_done 2 | A@LINE1-IN",
		)},
		{name: "gated store", run: pinGatedStore, want: pinLines(
			"lane slots PINGS-S0 PINGS-S1",
			"store create b1 LINE1-IN JackLoad",
			"store create b2 PINGS-GRP-WAIT Wait",
			"store create b3 PINGS-S1 JackUnload",
			"store finish b1 LINE1-IN JackLoad -> lift A | steps_done 1 | A@_TRANSIT[store]",
			"store finish b2 PINGS-GRP-WAIT Wait -> none | steps_done 2 | A@_TRANSIT[store]",
			"store finish b3 PINGS-S1 JackUnload -> none | steps_done 3 | A@_TRANSIT[store]",
			"store finished -> delivered | steps_done 3 | A@PINGS-S1",
		)},
		{name: "gated retrieve", run: pinGatedRetrieve, want: pinLines(
			"retrieve create b1 PINGR-GRP-WAIT Wait",
			"retrieve create b2 PINGR-S0 JackLoad",
			"retrieve create b3 LINE1-IN JackUnload",
			"retrieve finish b1 PINGR-GRP-WAIT Wait -> none | steps_done 1 | A@PINGR-S0[retrieve]",
			"retrieve finish b2 PINGR-S0 JackLoad -> lift A | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve finish b3 LINE1-IN JackUnload -> none | steps_done 3 | A@_TRANSIT[retrieve]",
			"retrieve finished -> delivered | steps_done 3 | A@LINE1-IN",
		)},
		{name: "two-robot swap, both legs", run: pinTwoRobotSwap, want: pinLines(
			"supply create b1 STORAGE-A1 JackLoad",
			"supply create b2 INBOUND-STAGING JackUnload",
			"supply create b3 INBOUND-STAGING Wait",
			"evac create b1 LINE1-IN Wait",
			"supply finish b1 STORAGE-A1 JackLoad -> lift FRESH | steps_done 1 | FRESH@_TRANSIT[supply] OLD@LINE1-IN[evac]",
			"supply finish b2 INBOUND-STAGING JackUnload -> store FRESH | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply finish b3 INBOUND-STAGING Wait -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply waiting -> staged",
			"evac finish b1 LINE1-IN Wait -> none | steps_done 1 | FRESH@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"evac waiting -> staged",
			"evac released -> in_transit",
			"evac append b2 LINE1-IN JackLoad",
			"evac append b3 OUTBOUND-DEST JackUnload",
			"evac finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finished -> delivered | steps_done 3 | FRESH@INBOUND-STAGING[supply] OLD@OUTBOUND-DEST",
			"supply released -> in_transit",
			"supply append b4 INBOUND-STAGING JackLoad",
			"supply append b5 LINE1-IN JackUnload",
			"supply finish b4 INBOUND-STAGING JackLoad -> lift FRESH | steps_done 4 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finish b5 LINE1-IN JackUnload -> none | steps_done 5 | FRESH@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finished -> delivered | steps_done 5 | FRESH@LINE1-IN OLD@OUTBOUND-DEST",
		)},
		{name: "keep-staged swap", run: pinKeepStagedSwap, want: pinLines(
			"supply create b1 INBOUND-STAGING Wait",
			"evac create b1 LINE1-IN Wait",
			"supply finish b1 INBOUND-STAGING Wait -> none | steps_done 1 | SPARE@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"supply waiting -> staged",
			"evac finish b1 LINE1-IN Wait -> none | steps_done 1 | SPARE@INBOUND-STAGING[supply] OLD@LINE1-IN[evac]",
			"evac waiting -> staged",
			"evac released -> in_transit",
			"evac append b2 LINE1-IN JackLoad",
			"evac append b3 OUTBOUND-DEST JackUnload",
			"evac finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | SPARE@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | SPARE@INBOUND-STAGING[supply] OLD@_TRANSIT[evac]",
			"evac finished -> delivered | steps_done 3 | SPARE@INBOUND-STAGING[supply] OLD@OUTBOUND-DEST",
			"supply released -> in_transit",
			"supply append b2 INBOUND-STAGING JackLoad",
			"supply append b3 LINE1-IN JackUnload",
			"supply finish b2 INBOUND-STAGING JackLoad -> lift SPARE | steps_done 2 | SPARE@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finish b3 LINE1-IN JackUnload -> none | steps_done 3 | SPARE@_TRANSIT[supply] OLD@OUTBOUND-DEST",
			"supply finished -> delivered | steps_done 3 | SPARE@LINE1-IN OLD@OUTBOUND-DEST",
		)},
		{name: "press-index pair", run: pinPressIndexPair, want: pinLines(
			"r1 create b1 LINE1-IN Wait",
			"r2 create b1 PINPI-BACK Wait",
			"r1 finish b1 LINE1-IN Wait -> none | steps_done 1 | FRONT@LINE1-IN[r1] BACK@PINPI-BACK[r2] FRESH@PINPI-IN[r1]",
			"r1 waiting -> staged",
			"r2 finish b1 PINPI-BACK Wait -> none | steps_done 1 | FRONT@LINE1-IN[r1] BACK@PINPI-BACK[r2] FRESH@PINPI-IN[r1]",
			"r2 waiting -> staged",
			"r1 released -> in_transit",
			"r1 append b2 LINE1-IN JackLoad",
			"r1 append b3 PINPI-OUT JackUnload",
			"r1 append b4 PINPI-IN JackLoad",
			"r1 append b5 PINPI-BACK JackUnload",
			"r1 finish b2 LINE1-IN JackLoad -> lift FRONT | steps_done 2 | FRONT@_TRANSIT[r1] BACK@PINPI-BACK[r2] FRESH@PINPI-IN[r1]",
			"r1 finish b3 PINPI-OUT JackUnload -> store FRONT | steps_done 3 | FRONT@PINPI-OUT[r1] BACK@PINPI-BACK[r2] FRESH@PINPI-IN[r1]",
			"r2 released -> in_transit",
			"r2 append b2 PINPI-BACK JackLoad",
			"r2 append b3 LINE1-IN JackUnload",
			"r2 finish b2 PINPI-BACK JackLoad -> lift BACK | steps_done 2 | FRONT@PINPI-OUT[r1] BACK@_TRANSIT[r2] FRESH@PINPI-IN[r1]",
			"r2 finish b3 LINE1-IN JackUnload -> none | steps_done 3 | FRONT@PINPI-OUT[r1] BACK@_TRANSIT[r2] FRESH@PINPI-IN[r1]",
			"r2 finished -> delivered | steps_done 3 | FRONT@PINPI-OUT[r1] BACK@LINE1-IN FRESH@PINPI-IN[r1]",
			"r1 finish b4 PINPI-IN JackLoad -> lift FRESH | steps_done 4 | FRONT@PINPI-OUT[r1] BACK@LINE1-IN FRESH@_TRANSIT[r1]",
			"r1 finish b5 PINPI-BACK JackUnload -> none | steps_done 5 | FRONT@PINPI-OUT[r1] BACK@LINE1-IN FRESH@_TRANSIT[r1]",
			"r1 finished -> delivered | steps_done 5 | FRONT@PINPI-OUT[r1] BACK@LINE1-IN FRESH@PINPI-BACK",
		)},
		{name: "sequential", run: pinSequential, want: pinLines(
			"removal create b1 LINE1-IN Wait",
			"removal finish b1 LINE1-IN Wait -> none | steps_done 1 | OLD@LINE1-IN[removal] FRESH@STORAGE-A1",
			"removal waiting -> staged",
			"removal released -> in_transit",
			"removal append b2 LINE1-IN JackLoad",
			"removal append b3 OUTBOUND-DEST JackUnload",
			"removal finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | OLD@_TRANSIT[removal] FRESH@STORAGE-A1",
			"backfill create b1 STORAGE-A1 JackLoad",
			"backfill create b2 LINE1-IN JackUnload",
			"removal finish b3 OUTBOUND-DEST JackUnload -> none | steps_done 3 | OLD@_TRANSIT[removal] FRESH@STORAGE-A1[backfill]",
			"removal finished -> delivered | steps_done 3 | OLD@OUTBOUND-DEST FRESH@STORAGE-A1[backfill]",
			"backfill finish b1 STORAGE-A1 JackLoad -> lift FRESH | steps_done 1 | OLD@OUTBOUND-DEST FRESH@_TRANSIT[backfill]",
			"backfill finish b2 LINE1-IN JackUnload -> none | steps_done 2 | OLD@OUTBOUND-DEST FRESH@_TRANSIT[backfill]",
			"backfill finished -> delivered | steps_done 2 | OLD@OUTBOUND-DEST FRESH@LINE1-IN",
		)},
		{name: "changeover per-position, two carrier families", run: pinChangeoverTwoFamilies, want: pinLines(
			"position create b1 LINE1-IN Wait",
			"position finish b1 LINE1-IN Wait -> none | steps_done 1 | OLD@LINE1-IN[position] NEW@PINCO-SRC[position]",
			"position waiting -> staged",
			"position released -> in_transit",
			"position append b2 LINE1-IN JackLoad",
			"position append b3 PINCO-OUT JackUnload",
			"position append b4 PINCO-SRC JackLoad",
			"position append b5 LINE1-IN JackUnload",
			"position finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | OLD@_TRANSIT[position] NEW@PINCO-SRC[position]",
			"position finish b3 PINCO-OUT JackUnload -> store OLD | steps_done 3 | OLD@PINCO-OUT[position] NEW@PINCO-SRC[position]",
			"position finish b4 PINCO-SRC JackLoad -> lift NEW | steps_done 4 | OLD@PINCO-OUT[position] NEW@_TRANSIT[position]",
			"position finish b5 LINE1-IN JackUnload -> none | steps_done 5 | OLD@PINCO-OUT[position] NEW@_TRANSIT[position]",
			"position finished -> delivered | steps_done 5 | OLD@PINCO-OUT[position] NEW@LINE1-IN",
		)},
		{name: "table turn, plain order, lift side", run: pinTurnPlainLift, want: pinLines(
			"move create b1 STORAGE-A1 Wait",
			"move finish b1 STORAGE-A1 Wait -> none | steps_done 1 | A@STORAGE-A1[move]",
			"move waiting -> staged | turns 1",
			"move next poll | turns 1",
			"move append b2 STORAGE-A1 JackLoad",
			"move append b3 LINE1-IN JackUnload",
			"move finish b2 STORAGE-A1 JackLoad -> lift A | steps_done 2 | A@_TRANSIT[move]",
			"move finish b3 LINE1-IN JackUnload -> none | steps_done 3 | A@_TRANSIT[move]",
			"move finished -> delivered | steps_done 3 | A@LINE1-IN",
		)},
		{name: "table turn, plain order, set-down side", run: pinTurnPlainSetDown, want: pinLines(
			"move create b1 STORAGE-A1 JackLoad",
			"move create b2 LINE1-IN Wait",
			"move finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT[move]",
			"move finish b2 LINE1-IN Wait -> none | steps_done 2 | A@_TRANSIT[move]",
			"move waiting -> staged | turns 1",
			"move next poll | turns 1",
			"move append b3 LINE1-IN JackUnload",
			"move finish b3 LINE1-IN JackUnload -> none | steps_done 3 | A@_TRANSIT[move]",
			"move finished -> delivered | steps_done 3 | A@LINE1-IN",
		)},
		{name: "table turn, complex order, lift side", run: pinTurnComplexLift, want: pinLines(
			"swap create b1 LINE1-IN Wait",
			"swap finish b1 LINE1-IN Wait -> none | steps_done 1 | FRESH@STORAGE-A1[swap] OLD@LINE1-IN[swap]",
			"swap waiting -> staged | turns 1",
			"swap next poll | turns 1",
			"swap append b2 LINE1-IN JackLoad",
			"swap append b3 OUTBOUND-DEST JackUnload",
			"swap append b4 STORAGE-A1 JackLoad",
			"swap append b5 LINE1-IN JackUnload",
			"swap finish b2 LINE1-IN JackLoad -> lift OLD | steps_done 2 | FRESH@STORAGE-A1[swap] OLD@_TRANSIT[swap]",
			"swap finish b3 OUTBOUND-DEST JackUnload -> store OLD | steps_done 3 | FRESH@STORAGE-A1[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finish b4 STORAGE-A1 JackLoad -> lift FRESH | steps_done 4 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finish b5 LINE1-IN JackUnload -> none | steps_done 5 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finished -> delivered | steps_done 5 | FRESH@LINE1-IN OLD@OUTBOUND-DEST[swap]",
		)},
		{name: "table turn, complex order, set-down side", run: pinTurnComplexSetDown, want: pinLines(
			"swap create b1 LINE1-IN JackLoad",
			"swap create b2 OUTBOUND-DEST JackUnload",
			"swap create b3 STORAGE-A1 JackLoad",
			"swap create b4 LINE1-IN Wait",
			"swap finish b1 LINE1-IN JackLoad -> lift OLD | steps_done 1 | FRESH@STORAGE-A1[swap] OLD@_TRANSIT[swap]",
			"swap finish b2 OUTBOUND-DEST JackUnload -> store OLD | steps_done 2 | FRESH@STORAGE-A1[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finish b3 STORAGE-A1 JackLoad -> lift FRESH | steps_done 3 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finish b4 LINE1-IN Wait -> none | steps_done 4 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-DEST[swap]",
			"swap waiting -> staged | turns 1",
			"swap next poll | turns 1",
			"swap append b5 LINE1-IN JackUnload",
			"swap finish b5 LINE1-IN JackUnload -> none | steps_done 5 | FRESH@_TRANSIT[swap] OLD@OUTBOUND-DEST[swap]",
			"swap finished -> delivered | steps_done 5 | FRESH@LINE1-IN OLD@OUTBOUND-DEST[swap]",
		)},
		{name: "dig: the reshuffle legs and the dwell", run: pinDig, want: pinLines(
			"leg1 create b1 GRP-PINDIG-WAIT Wait",
			"leg1 create b2 GRP-PINDIG-L1-S1 JackLoad",
			"leg1 create b3 GRP-PINDIG-L1-S1 Wait",
			"leg1 finish b1 GRP-PINDIG-WAIT Wait -> none | steps_done 1 | TARGET@GRP-PINDIG-L1-S2 BLOCKER1@GRP-PINDIG-L1-S1[leg1] BLOCKER2@GRP-PINDIG-L1-S3",
			"leg1 finish b2 GRP-PINDIG-L1-S1 JackLoad -> lift BLOCKER1 | steps_done 2 | TARGET@GRP-PINDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-PINDIG-L1-S3",
			"leg1 append b4 GRP-PINDIG-SHUF JackUnload",
			"leg2 create b1 GRP-PINDIG-WAIT Wait",
			"leg2 create b2 GRP-PINDIG-L1-S2 JackLoad",
			"leg2 create b3 LINE-PINDIG JackUnload",
			"leg1 finish b3 GRP-PINDIG-L1-S1 Wait -> none | steps_done 3 | TARGET@GRP-PINDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-PINDIG-L1-S3",
			"leg1 finish b4 GRP-PINDIG-SHUF JackUnload -> none | steps_done 4 | TARGET@GRP-PINDIG-L1-S2[leg2] BLOCKER1@_TRANSIT[leg1] BLOCKER2@GRP-PINDIG-L1-S3",
			"leg1 finished -> confirmed | steps_done 4 | TARGET@GRP-PINDIG-L1-S2[leg2] BLOCKER1@GRP-PINDIG-SHUF BLOCKER2@GRP-PINDIG-L1-S3",
			"leg2 finish b1 GRP-PINDIG-WAIT Wait -> none | steps_done 1 | TARGET@GRP-PINDIG-L1-S2[leg2] BLOCKER1@GRP-PINDIG-SHUF BLOCKER2@GRP-PINDIG-L1-S3",
			"leg2 finish b2 GRP-PINDIG-L1-S2 JackLoad -> lift TARGET | steps_done 2 | TARGET@_TRANSIT[leg2] BLOCKER1@GRP-PINDIG-SHUF BLOCKER2@GRP-PINDIG-L1-S3",
			"leg2 finish b3 LINE-PINDIG JackUnload -> none | steps_done 3 | TARGET@_TRANSIT[leg2] BLOCKER1@GRP-PINDIG-SHUF BLOCKER2@GRP-PINDIG-L1-S3",
			"leg2 finished -> confirmed | steps_done 3 | TARGET@LINE-PINDIG BLOCKER1@GRP-PINDIG-SHUF BLOCKER2@GRP-PINDIG-L1-S3",
		)},
		{name: "home-clear trade", run: pinHomeClearTrade, want: pinLines(
			"tap -> 200 ok",
			"P S->H create b1 PINCH-S JackLoad",
			"E H->S create b1 PINCH-H JackLoad",
			"E H->S finish b1 PINCH-H JackLoad -> lift E | steps_done 1 | E@_TRANSIT[E H->S] P@PINCH-S[P S->H] XB@PINCH-X",
			"P S->H append b2 PINCH-H JackUnload",
			"P S->H finish b1 PINCH-S JackLoad -> lift P | steps_done 1 | E@_TRANSIT[E H->S] P@_TRANSIT[P S->H] XB@PINCH-X",
			"E H->S append b2 PINCH-S JackUnload",
			"E H->S finish b2 PINCH-S JackUnload -> none | steps_done 3 | E@_TRANSIT[E H->S] P@_TRANSIT[P S->H] XB@PINCH-X",
			"E H->S finished -> confirmed | steps_done 3 | E@PINCH-S P@_TRANSIT[P S->H] XB@PINCH-X",
			"P S->H finish b2 PINCH-H JackUnload -> none | steps_done 3 | E@PINCH-S P@_TRANSIT[P S->H] XB@PINCH-X",
			"P S->H finished -> confirmed | steps_done 3 | E@PINCH-S P@PINCH-H XB@PINCH-X",
		)},
		{name: "carried-bin recovery", run: pinCarriedBinRecovery, want: pinLines(
			"recovery create b1 PINRCB-S1 JackUnload",
			"recovery finish b1 PINRCB-S1 JackUnload -> none | steps_done 0 | CARRIED@_ROBOT:AMR-P1[recovery]",
			"recovery finished -> delivered | steps_done 0 | CARRIED@PINRCB-S1",
		)},
		{name: "cancel-return", run: pinCancelReturn, want: pinLines(
			"return create b1 PINCR-S1 JackUnload",
			"return finish b1 PINCR-S1 JackUnload -> none | steps_done 0 | CARRIED@_ROBOT:AMR-PINCR[return]",
			"return finished -> delivered | steps_done 0 | CARRIED@PINCR-S1",
		)},
		{name: "redirect", run: pinRedirect, want: pinLines(
			"retrieve born -> queued",
			"retrieve redirected -> queued",
			"retrieve create b1 STORAGE-A1 JackLoad",
			"retrieve create b2 PIN-LINE2 JackUnload",
			"retrieve finish b1 STORAGE-A1 JackLoad -> lift A | steps_done 1 | A@_TRANSIT[retrieve]",
			"retrieve finish b2 PIN-LINE2 JackUnload -> none | steps_done 2 | A@_TRANSIT[retrieve]",
			"retrieve finished -> delivered | steps_done 2 | A@PIN-LINE2",
		)},
		{name: "orderless door: the cleaning mission", run: pinCleaningDoor, want: pinLines(
			"cleaning create b1 KNIFE-1 binTask=\"\" -> emitted false",
			"cleaning create b2 KNIFE-2 binTask=\"\" -> emitted false",
		)},
	})
}

// walk carries an order to its end the way a fleet runs one whose waits Core
// releases itself: every block it has been sent, finished in turn; at a wait,
// the robot reported there, and whatever Core appends finished in its turn.
func (r *pinRig) walk(label string) {
	r.t.Helper()
	po := r.order(label)
	r.run(label)
	for {
		for po.done < po.sent {
			r.finish(label)
		}
		ov := r.sim.GetOrder(po.vid)
		if ov == nil || ov.Complete {
			break
		}
		r.stage(label)
		if po.done == po.sent {
			r.say("%s waits and nothing appended its tail", label)
			return
		}
	}
	r.deliver(label)
}

// confirmedCart stands a carrier of payload with a confirmed count on a node,
// of bin type btID (0: the standard type).
func confirmedCart(t *testing.T, db *store.DB, payload string, nodeID, btID int64, label string, uop int) *bins.Bin {
	t.Helper()
	if btID == 0 {
		return clearBin(t, db, payload, nodeID, label, uop)
	}
	b := &bins.Bin{BinTypeID: btID, Label: label, NodeID: &nodeID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create "+label)
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, payload, uop), "manifest "+label)
	testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload "+label)
	return got
}

func pinNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
	return n
}

func pinLoadSequenceGated(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	mustExec(t, db, `UPDATE payloads SET advanced_load_sequence = 'Child cart interlock' WHERE code = $1`, sd.Payload.Code)
	_, slots := laneWithTwoSlots(t, db, "PINGQ")
	r := newPinSim(t, db)
	r.cart("A", confirmedCart(t, db, sd.Payload.Code, sd.LineNode.ID, 0, "PIN-A", 5))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-gseq", OrderType: protocol.OrderTypeMove, PayloadCode: sd.Payload.Code,
		SourceNode: sd.LineNode.Name, DeliveryNode: "PINGQ-GRP", Quantity: 1,
	})
	r.say("lane slots %s %s", slots[0].Name, slots[1].Name)
	r.trackUUID("store", "pin-gseq")
	r.walk("store")
	return r
}

func pinManualMove(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	res, err := r.eng.CreateBinMove(BinMoveRequest{Selection: BinSelectionAuto, SourceNodeID: storage.ID,
		DestNodeID: line.ID, StationID: "test-station", Desc: "pin move"})
	testutil.MustNoErr(t, err, "CreateBinMove")
	r.track("move", res.OrderID)
	r.walk("move")
	return r
}

func pinGatedStore(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	_, slots := laneWithTwoSlots(t, db, "PINGS")
	r := newPinSim(t, db)
	r.cart("A", confirmedCart(t, db, sd.Payload.Code, sd.LineNode.ID, 0, "PIN-A", 5))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-gstore", OrderType: protocol.OrderTypeMove, PayloadCode: sd.Payload.Code,
		SourceNode: sd.LineNode.Name, DeliveryNode: "PINGS-GRP", Quantity: 1,
	})
	r.say("lane slots %s %s", slots[0].Name, slots[1].Name)
	r.trackUUID("store", "pin-gstore")
	r.walk("store")
	return r
}

func pinGatedRetrieve(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	_, slots := laneWithTwoSlots(t, db, "PINGR")
	r := newPinSim(t, db)
	r.cart("A", confirmedCart(t, db, sd.Payload.Code, slots[0].ID, 0, "PIN-A", 5))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-gret", OrderType: protocol.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-gret")
	r.walk("retrieve")
	return r
}

// pinSwapLegs submits a coordinated pair through intake, each leg naming the
// other, as a station sends one.
func pinSwapLegs(r *pinRig, payload, process string, a, b string, stepsA, stepsB []protocol.ComplexOrderStep) {
	r.t.Helper()
	d := r.eng.Dispatcher()
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: a, PayloadCode: payload,
		Quantity: 1, ProcessNode: process, SiblingOrderUUID: b, Steps: stepsA})
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: b, PayloadCode: payload,
		Quantity: 1, ProcessNode: process, SiblingOrderUUID: a, Steps: stepsB})
}

func pinStationWait(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: node, WaitKind: protocol.WaitKindStation}
}

func pinPick(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: node}
}

func pinDrop(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: node}
}

func pinTwoRobotSwap(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, inStage, _, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, 0, "PIN-FRESH", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, 0, "PIN-OLD", 2))
	pinSwapLegs(r, bp.Code, line.Name, "pin-supply", "pin-evac",
		[]protocol.ComplexOrderStep{pinPick(storage.Name),
			{Action: protocol.ActionDropoff, Node: inStage.Name, ExclusiveSlot: true},
			pinStationWait(inStage.Name), pinPick(inStage.Name), pinDrop(line.Name)},
		[]protocol.ComplexOrderStep{pinStationWait(line.Name), pinPick(line.Name), pinDrop(outDest.Name)})
	r.trackUUID("supply", "pin-supply")
	r.trackUUID("evac", "pin-evac")
	r.run("supply")
	r.finish("supply")
	r.finish("supply")
	r.finish("supply")
	r.stage("supply")
	r.run("evac")
	r.finish("evac")
	r.stage("evac")
	r.release("evac", "pin-evac")
	r.finish("evac")
	r.finish("evac")
	r.deliver("evac")
	r.release("supply", "pin-supply")
	r.finish("supply")
	r.finish("supply")
	r.deliver("supply")
	return r
}

func pinKeepStagedSwap(t *testing.T) *pinRig {
	db := testDB(t)
	_, line, inStage, _, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("SPARE", confirmedCart(t, db, bp.Code, inStage.ID, 0, "PIN-SPARE", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, 0, "PIN-OLD", 2))
	pinSwapLegs(r, bp.Code, line.Name, "pin-ks-supply", "pin-ks-evac",
		[]protocol.ComplexOrderStep{pinStationWait(inStage.Name),
			{Action: protocol.ActionPickup, Node: inStage.Name, PayloadCode: bp.Code}, pinDrop(line.Name)},
		[]protocol.ComplexOrderStep{pinStationWait(line.Name), pinPick(line.Name), pinDrop(outDest.Name)})
	r.trackUUID("supply", "pin-ks-supply")
	r.trackUUID("evac", "pin-ks-evac")
	r.run("supply")
	r.finish("supply")
	r.stage("supply")
	r.run("evac")
	r.finish("evac")
	r.stage("evac")
	r.release("evac", "pin-ks-evac")
	r.finish("evac")
	r.finish("evac")
	r.deliver("evac")
	r.release("supply", "pin-ks-supply")
	r.finish("supply")
	r.finish("supply")
	r.deliver("supply")
	return r
}

func pinPressIndexPair(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	front := sd.LineNode
	back, out, inb := pinNode(t, db, "PINPI-BACK"), pinNode(t, db, "PINPI-OUT"), pinNode(t, db, "PINPI-IN")
	r := newPinSim(t, db)
	r.cart("FRONT", confirmedCart(t, db, sd.Payload.Code, front.ID, 0, "PIN-FRONT", 9))
	r.cart("BACK", confirmedCart(t, db, sd.Payload.Code, back.ID, 0, "PIN-BACK", 4))
	r.cart("FRESH", confirmedCart(t, db, sd.Payload.Code, inb.ID, 0, "PIN-FRESH", 10))
	pinSwapLegs(r, sd.Payload.Code, front.Name, "pin-r1", "pin-r2",
		[]protocol.ComplexOrderStep{pinStationWait(front.Name), pinPick(front.Name), pinDrop(out.Name),
			pinPick(inb.Name), pinDrop(back.Name)},
		[]protocol.ComplexOrderStep{pinStationWait(back.Name), pinPick(back.Name), pinDrop(front.Name)})
	r.trackUUID("r1", "pin-r1")
	r.trackUUID("r2", "pin-r2")
	r.run("r1")
	r.finish("r1")
	r.stage("r1")
	r.run("r2")
	r.finish("r2")
	r.stage("r2")
	r.release("r1", "pin-r1")
	r.finish("r1")
	r.finish("r1")
	r.release("r2", "pin-r2")
	r.finish("r2")
	r.finish("r2")
	r.deliver("r2")
	r.finish("r1")
	r.finish("r1")
	r.deliver("r1")
	return r
}

func pinSequential(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, _, _, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, 0, "PIN-OLD", 2))
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, 0, "PIN-FRESH", 10))
	d := r.eng.Dispatcher()
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: "pin-seq-removal",
		PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{pinStationWait(line.Name), pinPick(line.Name), pinDrop(outDest.Name)}})
	r.trackUUID("removal", "pin-seq-removal")
	r.run("removal")
	r.finish("removal")
	r.stage("removal")
	r.release("removal", "pin-seq-removal")
	r.finish("removal")
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{OrderUUID: "pin-seq-backfill",
		PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{pinPick(storage.Name), pinDrop(line.Name)}})
	r.trackUUID("backfill", "pin-seq-backfill")
	r.finish("removal")
	r.deliver("removal")
	r.walk("backfill")
	return r
}

func pinChangeoverTwoFamilies(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	pos, out, src := sd.LineNode, pinNode(t, db, "PINCO-OUT"), pinNode(t, db, "PINCO-SRC")
	mkType := func(code string) *bins.BinType {
		bt := &bins.BinType{Code: code, Description: "pin carrier"}
		testutil.MustNoErr(t, db.CreateBinType(bt), "bin type "+code)
		return bt
	}
	from, to := mkType("PINCO-4548"), mkType("PINCO-3230")
	var famFrom, famTo int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT family_id FROM bin_types WHERE id=$1`, from.ID).Scan(&famFrom), "family")
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT family_id FROM bin_types WHERE id=$1`, to.ID).Scan(&famTo), "family")
	if famFrom == famTo {
		t.Fatalf("fixture: the two carriers share family %d; the case is two families", famFrom)
	}
	partFrom, partTo := sd.Payload.Code, "PINCO-NEW"
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: partTo, Description: "incoming part", UOPCapacity: 10}), "payload")
	newPart, err := db.GetPayloadByCode(partTo)
	testutil.MustNoErr(t, err, "read payload")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(sd.Payload.ID, []int64{from.ID}), "from part's carrier")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(newPart.ID, []int64{to.ID}), "to part's carrier")
	r := newPinSim(t, db)
	r.cart("OLD", confirmedCart(t, db, partFrom, pos.ID, from.ID, "PIN-OLD-4548", 3))
	r.cart("NEW", confirmedCart(t, db, partTo, src.ID, to.ID, "PIN-NEW-3230", 10))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-co", PayloadCode: partFrom, Quantity: 1, ProcessNode: pos.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: pos.Name, WaitKind: protocol.WaitKindStation, Purpose: "ready"},
			pinPick(pos.Name), pinDrop(out.Name),
			{Action: protocol.ActionPickup, Node: src.Name, PayloadCode: partTo},
			pinDrop(pos.Name),
		},
	})
	r.trackUUID("position", "pin-co")
	r.run("position")
	r.finish("position")
	r.stage("position")
	r.release("position", "pin-co")
	for range 4 {
		r.finish("position")
	}
	r.deliver("position")
	return r
}

// pinTurnRig is a pin on a simulator whose table the dispatcher reads straight
// from the simulator (a test has no robot poll), with node turning its table
// on side ("" the lift, dispatch.TableTurnSideSetDown the set-down).
func pinTurnRig(t *testing.T, db *store.DB, node *nodes.Node, side string) *pinRig {
	t.Helper()
	r := newPinSim(t, db)
	d := r.eng.Dispatcher()
	d.SetTableTurner(r.sim, func(v string) (fleet.RobotStatus, bool) { return r.sim.TableStatus(v), true })
	if side != "" {
		testutil.MustNoErr(t, db.SetNodeProperty(node.ID, dispatch.PropTableTurnSide, side), "side")
	}
	testutil.MustNoErr(t, db.SetNodeProperty(node.ID, dispatch.PropTableTurnRad, "1.5707963267948966"), "angle")
	testutil.MustNoErr(t, d.ReloadTableTurns(), "reload turns")
	return r
}

// turnWalk is walk for an order at a turn node: the robot is reported at its
// wait by name (the table turn reads its vehicle), and the next robot poll
// re-drives the turns.
func (r *pinRig) turnWalk(label, robot string) {
	r.t.Helper()
	po := r.order(label)
	r.sim.DriveStateWithRobot(po.vid, "RUNNING", robot)
	r.sync()
	for {
		for po.done < po.sent {
			r.finish(label)
		}
		ov := r.sim.GetOrder(po.vid)
		if ov == nil || ov.Complete {
			break
		}
		r.sim.DriveStateWithRobot(po.vid, "WAITING", robot)
		r.say("%s waiting -> %s | turns %d", label, r.status(label), len(r.sim.TableTurnCalls()))
		r.sync()
		r.eng.Dispatcher().RedriveTableTurns(0)
		r.say("%s next poll | turns %d", label, len(r.sim.TableTurnCalls()))
		r.sync()
		if po.done == po.sent {
			r.say("%s waits and nothing appended its tail", label)
			return
		}
	}
	r.deliver(label)
}

func pinTurnPlainLift(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := pinTurnRig(t, db, storage, "")
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-turn-pl", OrderType: protocol.OrderTypeMove, PayloadCode: bp.Code,
		SourceNode: storage.Name, DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("move", "pin-turn-pl")
	r.turnWalk("move", "AMR-T1")
	return r
}

func pinTurnPlainSetDown(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := pinTurnRig(t, db, line, dispatch.TableTurnSideSetDown)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-turn-ps", OrderType: protocol.OrderTypeMove, PayloadCode: bp.Code,
		SourceNode: storage.Name, DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("move", "pin-turn-ps")
	r.sim.SetRobotJackUp("AMR-T1", true)
	r.turnWalk("move", "AMR-T1")
	return r
}

func pinTurnComplexLift(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, _, _, outDest, bp := setupProductionNodes(t, db)
	r := pinTurnRig(t, db, line, "")
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, 0, "PIN-FRESH", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, 0, "PIN-OLD", 2))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-turn-cl", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{pinPick(line.Name), pinDrop(outDest.Name),
			pinPick(storage.Name), pinDrop(line.Name)},
	})
	r.trackUUID("swap", "pin-turn-cl")
	r.turnWalk("swap", "AMR-T2")
	return r
}

func pinTurnComplexSetDown(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, _, _, outDest, bp := setupProductionNodes(t, db)
	r := pinTurnRig(t, db, line, dispatch.TableTurnSideSetDown)
	r.cart("FRESH", confirmedCart(t, db, bp.Code, storage.ID, 0, "PIN-FRESH", 10))
	r.cart("OLD", confirmedCart(t, db, bp.Code, line.ID, 0, "PIN-OLD", 2))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-turn-cs", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{pinPick(line.Name), pinDrop(outDest.Name),
			pinPick(storage.Name), pinDrop(line.Name)},
	})
	r.trackUUID("swap", "pin-turn-cs")
	r.sim.SetRobotJackUp("AMR-T3", true)
	r.turnWalk("swap", "AMR-T3")
	return r
}

func pinDig(t *testing.T) *pinRig {
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "PINDIG", NumSlots: 3, TargetSlot: 2,
		TargetAge: 2 * time.Hour})
	r := newPinSim(t, db)
	r.cart("TARGET", sc.TargetBin)
	for i, b := range sc.Blockers {
		r.cart(fmt.Sprintf("BLOCKER%d", i+1), b)
	}
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-dig", OrderType: protocol.OrderTypeRetrieve, PayloadCode: sc.Payload.Code,
		SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name, Quantity: 1,
	})
	parent := testdb.RequireOrder(t, db, "pin-dig")
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs")
	for i, c := range children {
		r.track(fmt.Sprintf("leg%d", i+1), c.ID)
	}
	for i := range children {
		r.walk(fmt.Sprintf("leg%d", i+1))
	}
	return r
}

func pinHomeClearTrade(t *testing.T) *pinRig {
	rig := newClearRigWith(t, "PINCH", false, clearRigOpts{})
	r := newPinRig(t, rig.db, rig.sim, rig.eng)
	r.cart("E", rig.e)
	r.cart("P", rig.p)
	r.cart("XB", rig.xBin)
	ans := rig.tap(t)
	r.say("tap -> %d %s", ans.Status, ans.Body.Status)
	parent := rig.parent(t)
	if parent == nil {
		t.Fatal("fixture: the tap birthed no clear")
	}
	l1, l2 := rig.tradeLegs(t, parent.ID)
	r.track("P S->H", l1.ID)
	r.track("E H->S", l2.ID)
	r.run("E H->S")
	r.finish("E H->S")
	r.run("P S->H")
	r.finish("P S->H")
	r.walk("E H->S")
	r.walk("P S->H")
	return r
}

func pinCarriedBinRecovery(t *testing.T) *pinRig {
	db := testDB(t)
	grp, _ := storeGroupWithSlot(t, db, "PINRCB")
	seedClaim(t, db, "PROC-PINRCB", "STY", "LINE-PINRCB", "PINRCB-P", grp.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-P1", "PINRCB-P", "LINE-PINRCB")
	r := newPinSim(t, db)
	r.cart("CARRIED", bin)
	cacheRobot(r.eng, dispatchableRobot("AMR-P1"))
	order, _, err := r.eng.RecoverCarriedBin(bin.ID, "operator:pin")
	testutil.MustNoErr(t, err, "recover the carried bin")
	r.track("recovery", order.ID)
	r.walk("recovery")
	return r
}

func pinCancelReturn(t *testing.T) *pinRig {
	db := testDB(t)
	grp, _ := storeGroupWithSlot(t, db, "PINCR")
	seedClaim(t, db, "PROC-PINCR", "STY", "LINE-PINCR", "PINCR-P", grp.Name, true)
	bin, _ := seedCancelledCarry(t, db, "AMR-PINCR", "PINCR-P", "LINE-PINCR")
	r := newPinSim(t, db)
	r.cart("CARRIED", bin)
	cacheRobot(r.eng, loadedDispatchable("AMR-PINCR"))
	r.eng.sweepCarriedBins()
	r.track("return", theReturn(t, db, bin.ID).ID)
	r.walk("return")
	return r
}

// pinRedirect: a move redirected while it waits for its cart, then sent.
func pinRedirect(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	line2 := pinNode(t, db, "PIN-LINE2")
	r := newPinSim(t, db)
	d := r.eng.Dispatcher()
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-redirect", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-redirect")
	r.say("retrieve born -> %s", r.status("retrieve"))
	d.HandleOrderRedirect(testEnvelope(), &protocol.OrderRedirect{OrderUUID: "pin-redirect", NewDeliveryNode: line2.Name})
	r.say("retrieve redirected -> %s", r.status("retrieve"))
	r.cart("A", confirmedCart(t, db, bp.Code, storage.ID, 0, "PIN-A", 5))
	r.sync()
	r.walk("retrieve")
	return r
}

func pinCleaningDoor(t *testing.T) *pinRig {
	db := testDB(t)
	r := newPinSim(t, db)
	testutil.MustNoErr(t, r.eng.dispatchCleaning("AMR-C1", []string{"KNIFE-1", "KNIFE-2"}, "pin", time.Now()),
		"send the cleaning order")
	ids := r.sim.VendorOrderIDs()
	if len(ids) != 1 {
		t.Fatalf("fixture: %d fleet orders, want the cleaning order alone", len(ids))
	}
	vid := ids[0]
	for _, b := range r.sim.BlocksForOrder(vid) {
		name := strings.TrimPrefix(b.BlockID, vid+"-")
		emitted := r.sim.CompleteBlock(vid, b.BlockID, b.Location, b.BinTask, 0, 0)
		r.say("cleaning create %s %s binTask=%q -> emitted %v", name, b.Location, b.BinTask, emitted)
	}
	return r
}

func pinPlainRetrieve(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-retrieve", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-retrieve")
	r.run("retrieve")
	r.finish("retrieve")
	r.finish("retrieve")
	r.deliver("retrieve")
	return r
}

func pinPlainRetrieveReplay(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-replay", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-replay")
	r.run("retrieve")
	r.finish("retrieve")
	r.replay("retrieve")
	r.finish("retrieve")
	r.replay("retrieve")
	r.deliver("retrieve")
	return r
}

func pinPlainRetrieveAfterEnd(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-ended", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-ended")
	r.run("retrieve")
	r.cancel("retrieve", "pin-ended")
	r.finish("retrieve")
	r.finish("retrieve")
	return r
}

func pinEmptyRetrieve(t *testing.T) *pinRig {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	r := newPinSim(t, db)
	r.cart("E", createTestBinAtNode(t, db, "", sd.StorageNode.ID, "PIN-EMPTY"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-empty", OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true,
		DeliveryNode: sd.LineNode.Name, Quantity: 1,
	})
	r.trackUUID("empty", "pin-empty")
	r.run("empty")
	r.finish("empty")
	r.finish("empty")
	r.deliver("empty")
	return r
}

func pinStagedRelease(t *testing.T) *pinRig {
	db := testDB(t)
	_, line, _, _, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("OLD", createTestBinAtNode(t, db, bp.Code, line.ID, "PIN-OLD"))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-staged", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: line.Name},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: outDest.Name},
		},
	})
	r.trackUUID("removal", "pin-staged")
	r.run("removal")
	r.finish("removal")
	r.stage("removal")
	r.release("removal", "pin-staged")
	r.finish("removal")
	r.finish("removal")
	r.deliver("removal")
	return r
}

func pinSingleRobotSwap(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, inStage, outStage, outDest, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("FRESH", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-FRESH"))
	r.cart("OLD", createTestBinAtNode(t, db, bp.Code, line.ID, "PIN-OLD"))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-single", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: storage.Name},
			{Action: protocol.ActionDropoff, Node: inStage.Name},
			{Action: protocol.ActionWait, Node: line.Name},
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: outStage.Name},
			{Action: protocol.ActionPickup, Node: inStage.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
			{Action: protocol.ActionPickup, Node: outStage.Name},
			{Action: protocol.ActionDropoff, Node: outDest.Name},
		},
	})
	r.trackUUID("swap", "pin-single")
	r.run("swap")
	r.finish("swap")
	r.finish("swap")
	r.finish("swap")
	r.stage("swap")
	r.release("swap", "pin-single")
	for range 6 {
		r.finish("swap")
	}
	r.deliver("swap")
	return r
}

func pinCarryover(t *testing.T) *pinRig {
	db := testDB(t)
	_, line, inStage, _, _, bp := setupProductionNodes(t, db)
	r := newPinSim(t, db)
	r.cart("TOOL", createTestBinAtNode(t, db, bp.Code, line.ID, "PIN-TOOL"))
	r.eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "pin-carry", PayloadCode: bp.Code, Quantity: 1, ProcessNode: line.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: inStage.Name},
			{Action: protocol.ActionWait, Node: inStage.Name, WaitKind: protocol.WaitKindStation},
			{Action: protocol.ActionPickup, Node: inStage.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		},
	})
	r.trackUUID("carry", "pin-carry")
	r.run("carry")
	r.finish("carry")
	r.finish("carry")
	r.finish("carry")
	r.stage("carry")
	r.release("carry", "pin-carry")
	r.finish("carry")
	r.finish("carry")
	r.deliver("carry")
	return r
}

func pinLoadSequenceSealed(t *testing.T) *pinRig {
	db := testDB(t)
	storage, line, bp := testdb.SetupTestData(t, db)
	mustExec(t, db, `UPDATE payloads SET advanced_load_sequence = 'Child cart interlock' WHERE code = $1`, bp.Code)
	r := newPinSim(t, db)
	r.cart("A", createTestBinAtNode(t, db, bp.Code, storage.ID, "PIN-A"))
	r.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pin-seq", OrderType: protocol.OrderTypeRetrieve, PayloadCode: bp.Code,
		DeliveryNode: line.Name, Quantity: 1,
	})
	r.trackUUID("retrieve", "pin-seq")
	r.run("retrieve")
	for range 5 {
		r.finish("retrieve")
	}
	r.deliver("retrieve")
	return r
}
