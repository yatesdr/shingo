//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// tops_and_bottoms_pull_docker_test.go — SEND BACK at Core, end to end on the
// engine: the pull (an empty retrieve stating tops & bottoms) admitted, parked
// and replayed by the fulfillment scanner, and the count its move writes on the
// outside cart it lifts.

// tbpPlant is a fed tote press (TBP-PRESS, its outside TBP-OUT) drawing from a
// group, the part's tote carrier (5 layers of 8, 8 parts a tote), and the
// group's slots.
type tbpPlant struct {
	db        *store.DB
	eng       *Engine
	part, src string
	outside   string
	toteID    int64
	slots     []*nodes.Node
	used      int
}

func newTBPPlant(t *testing.T, prefix string) *tbpPlant {
	t.Helper()
	db := testDB(t)
	p := &tbpPlant{db: db, part: prefix + "-PART", src: prefix + "-SRC", outside: prefix + "-OUT"}
	_, err := db.Exec(`INSERT INTO payloads (code, uop_capacity, tote_layers, uop_per_tote) VALUES ($1, 320, 5, 8)`, p.part)
	testutil.MustNoErr(t, err, "the part")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ($1, 8) RETURNING id`,
		prefix+"-TOTE").Scan(&p.toteID), "the tote carrier")
	_, err = db.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id)
		SELECT id, $2 FROM payloads WHERE code = $1`, p.part, p.toteID)
	testutil.MustNoErr(t, err, "the carrier rule")
	for _, name := range []string{prefix + "-PRESS", p.outside} {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: name, Enabled: true}), name)
	}
	srcID, err := nodes.CreateGroup(db.DB, p.src)
	testutil.MustNoErr(t, err, "the inbound source")
	testdb.MarkGroup(t, db, srcID)
	for i := 0; i < 6; i++ {
		slot := &nodes.Node{Name: fmt.Sprintf("%s-%d", p.src, i), Enabled: true, ParentID: &srcID}
		testutil.MustNoErr(t, db.CreateNode(slot), "slot")
		p.slots = append(p.slots, slot)
	}
	_, err = db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node, inbound_source)
		VALUES ($1, 'S1', $2, 'produce', 'two_robot_press_index', $3, $4, $5)`,
		prefix+"-P", prefix+"-PRESS", p.part, p.outside, p.src)
	testutil.MustNoErr(t, err, "the fed claim")
	p.eng = newTestEngine(t, db, simulator.New())
	return p
}

// cart sets a cart of the tote carrier in the source's next slot: totes on
// it, and a part with its count for a full.
func (p *tbpPlant) cart(t *testing.T, label string, totes int, payload string, uop int) int64 {
	t.Helper()
	at := p.slots[p.used]
	p.used++
	var id int64
	testutil.MustNoErr(t, p.db.QueryRow(`INSERT INTO bins
		(bin_type_id, label, node_id, status, totes_on_cart, payload_code, uop_remaining, manifest_confirmed)
		VALUES ($1, $2, $3, 'available', $4, $5, $6, $7) RETURNING id`,
		p.toteID, label, at.ID, totes, payload, uop, payload != "").Scan(&id), "cart "+label)
	return id
}

// pull is SEND BACK's pull, as the Edge sends it.
func (p *tbpPlant) pull(uuid string) {
	p.eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid,
		OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Want: protocol.TopsAndBottomsOnly,
		PayloadCode: p.part, SourceNode: p.src, DeliveryNode: p.outside, Quantity: 1})
}

// TestTopsAndBottomsPull_ADryPullReplayedWaitsForTopsAndBottoms: the pull is
// admitted into an empty source and parks. The scanner replays it as the
// source gains a full of the part, then a stack (3 high), then tops & bottoms:
// it waits through the first two, saying what it waits for, and takes the
// third.
func TestTopsAndBottomsPull_ADryPullReplayedWaitsForTopsAndBottoms(t *testing.T) {
	t.Parallel()
	p := newTBPPlant(t, "TBR")
	p.pull("tbr-pull")
	want := "Waiting for tops & bottoms in " + p.src
	o := testdb.RequireOrder(t, p.db, "tbr-pull")
	if o.Status != protocol.StatusQueued || o.QueueReason != want {
		t.Fatalf("the dry pull is %s under %q, want queued under %q", o.Status, o.QueueReason, want)
	}
	for _, step := range []struct {
		name         string
		totes        int
		payload      string
		uop          int
		takesThisOne bool
	}{
		{"a full of the part", 40, p.part, 320, false},
		{"a stack, 3 high", 24, "", 0, false},
		{"tops & bottoms", 0, "", 0, true},
	} {
		id := p.cart(t, "TBR-"+fmt.Sprint(p.used), step.totes, step.payload, step.uop)
		p.eng.RunFulfillmentScan()
		o = testdb.RequireOrder(t, p.db, "tbr-pull")
		if !step.takesThisOne {
			if o.Status != protocol.StatusQueued || o.QueueReason != want {
				t.Fatalf("with %s in the source the pull is %s under %q, want still waiting", step.name, o.Status, o.QueueReason)
			}
			continue
		}
		if o.Status != protocol.StatusDispatched && o.Status != protocol.StatusInTransit {
			t.Fatalf("with %s in the source the pull is %s under %q, want dispatched", step.name, o.Status, o.QueueReason)
		}
		if o.BinID == nil || *o.BinID != id {
			t.Errorf("the pull took bin %v, want the tops & bottoms %d", o.BinID, id)
		}
	}
}

// TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails: the press stops
// carrying the part while its pull waits. The scanner's replay asks for the
// press again (no column carries it) and fails the order with the sentence
// intake would have refused it with.
func TestTopsAndBottomsPull_AReplayThatNoLongerResolvesFails(t *testing.T) {
	t.Parallel()
	p := newTBPPlant(t, "TBN")
	p.pull("tbn-pull")
	if o := testdb.RequireOrder(t, p.db, "tbn-pull"); o.Status != protocol.StatusQueued {
		t.Fatalf("the dry pull is %s (%q), want queued", o.Status, o.QueueReason)
	}
	_, err := p.db.Exec(`UPDATE style_claims SET payload_code = 'TBN-OTHER' WHERE core_node_name = 'TBN-PRESS'`)
	testutil.MustNoErr(t, err, "the press stops carrying the part")
	p.cart(t, "TBN-TB", 0, "", 0)
	p.eng.RunFulfillmentScan()
	o := testdb.RequireOrder(t, p.db, "tbn-pull")
	want := p.outside + " is not the outside position of a press fed with " + p.part
	if o.Status != protocol.StatusFailed || o.ErrorDetail != want {
		t.Errorf("the replay left the pull %s with %q, want failed with %q", o.Status, o.ErrorDetail, want)
	}
}

// SEND BACK's move lifts the outside cart by name, a plain move with no
// sibling. Its count at the lift is the derivation every lift off a fed
// outside gets (DeriveTotesAtLift), unchanged: what the front's build drew is
// taken off the cart's height. The three cases the crew meets at the
// unloader.
func TestSendBackMove_TheCountAtTheLift(t *testing.T) {
	t.Parallel()
	for _, k := range []struct {
		name, prefix string
		front        func(t *testing.T, c toteCell)
		want         string
	}{
		{"mid-build: H less what the build drew", "SBM", func(*testing.T, toteCell) {}, "26"},
		{"right after a swap, nothing drawn: H", "SBA", func(t *testing.T, c toteCell) {
			_, err := c.db.Exec(`UPDATE bins SET uop_remaining = 0 WHERE id = $1`, c.front)
			testutil.MustNoErr(t, err, "an empty front")
		}, "40"},
		{"no cart on the press: unknown", "SBN", func(t *testing.T, c toteCell) {
			_, err := c.db.Exec(`DELETE FROM bins WHERE id = $1`, c.front)
			testutil.MustNoErr(t, err, "no front")
		}, "unknown"},
	} {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), k.prefix)
			k.front(t, c)
			move := &orders.Order{EdgeUUID: k.prefix + "-sendback", StationID: "line-1",
				OrderType: dispatch.OrderTypeMove, SourceIntent: dispatch.SourceIntentLocal, Status: "in_transit",
				Quantity: 1, SourceNode: c.out.Name, DeliveryNode: k.prefix + "-DEST", BinID: &c.outside,
				NamedBinID: &c.outside}
			testutil.MustNoErr(t, db.CreateOrder(move), "SEND BACK's move")
			_, err := db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, move.ID, c.outside)
			testutil.MustNoErr(t, err, "claim")
			c.lift(move, c.out.Name)
			if got := totesOn(t, db, c.outside); got != k.want {
				t.Errorf("the outside cart has %s totes at its lift, want %s", got, k.want)
			}
		})
	}
}
