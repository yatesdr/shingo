//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// tops_and_bottoms_pull_docker_test.go — SEND BACK's pull at Core (an empty
// retrieve stating tops & bottoms): what it takes, from where, and what intake
// refuses.

// tbpCell is a fed press (a produce tote press whose outside position is no
// claim's core node) whose claim draws from a group, the part's tote carrier
// (the part's carrier rule names it) and the group's slots.
type tbpCell struct {
	db                         *store.DB
	part, tote, press, outside string
	src                        string
	srcID                      int64
	pressID                    int64
	toteID                     int64
	slots                      []*nodes.Node
	used                       int
}

func newTBPCell(t *testing.T, db *store.DB, prefix string) *tbpCell {
	t.Helper()
	c := &tbpCell{db: db, part: prefix + "-PART", tote: prefix + "-TOTE", press: prefix + "-PRESS",
		outside: prefix + "-OUT", src: prefix + "-SRC"}
	pl := &payloads.Payload{Code: c.part, UOPCapacity: 40}
	testutil.MustNoErr(t, db.CreatePayload(pl), "the part")
	bt := &bins.BinType{Code: c.tote}
	testutil.MustNoErr(t, db.CreateBinType(bt), "the tote carrier")
	c.toteID = bt.ID
	c.exec(t, `UPDATE payloads SET tote_layers = 5, uop_per_tote = 8 WHERE code = $1`, c.part)
	c.exec(t, `UPDATE bin_types SET totes_per_layer = 8 WHERE id = $1`, bt.ID)
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{bt.ID}), "the part rides the tote carrier")
	press := &nodes.Node{Name: c.press, Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, press), "press")
	c.pressID = press.ID
	testutil.MustNoErr(t, nodes.Create(db.DB, &nodes.Node{Name: c.outside, Enabled: true}), "outside")
	var err error
	c.srcID, err = nodes.CreateGroup(db.DB, c.src)
	testutil.MustNoErr(t, err, "the inbound source")
	testdb.MarkGroup(t, db, c.srcID)
	for i := 0; i < 8; i++ {
		slot := &nodes.Node{Name: fmt.Sprintf("%s-%d", c.src, i), Enabled: true, ParentID: &c.srcID}
		testutil.MustNoErr(t, nodes.Create(db.DB, slot), "slot")
		c.slots = append(c.slots, slot)
	}
	c.exec(t, `INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node, inbound_source)
		VALUES ($1, 'S1', $2, 'produce', 'two_robot_press_index', $3, $4, $5)`,
		prefix+"-P", c.press, c.part, c.outside, c.src)
	return c
}

func (c *tbpCell) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	_, err := c.db.Exec(q, args...)
	testutil.MustNoErr(t, err, q)
}

// cart sets a cart of bin type typeID down in the source's next slot (or at
// node when it is set): totes nil is an unverified count, payload a part's
// partial.
func (c *tbpCell) cart(t *testing.T, typeID int64, label string, totes *int, payload string, at *nodes.Node) int64 {
	t.Helper()
	if at == nil {
		at = c.slots[c.used]
		c.used++
	}
	var id int64
	uop := 0
	if payload != "" {
		uop = 10
	}
	testutil.MustNoErr(t, c.db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart, payload_code, uop_remaining)
		VALUES ($1, $2, $3, 'available', $4, $5, $6) RETURNING id`, typeID, label, at.ID, totes, payload, uop).Scan(&id), "cart "+label)
	return id
}

func (c *tbpCell) pull(uuid string) *orders.Order {
	return &orders.Order{EdgeUUID: uuid, OrderType: OrderTypeRetrieveEmpty, SourceIntent: SourceIntentEmpty,
		Want: protocol.TopsAndBottomsOnly, PayloadCode: c.part, SourceNode: c.src, DeliveryNode: c.outside, Quantity: 1}
}

func (c *tbpCell) find(t *testing.T, o *orders.Order) SourceResult {
	t.Helper()
	return NewSourceFinder(c.db, nil, nil).FindSource(o, IntentEmpty)
}

// TestTopsAndBottomsPull_TakesExactlyTopsAndBottoms: the pull's source holds a
// stack (3 high), a leftover (3 high + 2), a cart nobody counted, a partial of
// the part and a bare cart, and tops & bottoms stands in another group of the
// plant. The pull takes none of them and waits, saying for what and where;
// tops & bottoms landing in its source is what it takes.
func TestTopsAndBottomsPull_TakesExactlyTopsAndBottoms(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBX")
	var bareID int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, bare_of) VALUES ('TBX-BARE', $1) RETURNING id`,
		c.toteID).Scan(&bareID), "a bare marker of the tote carrier")
	c.cart(t, c.toteID, "TBX-STACK", intPtr(24), "", nil)
	c.cart(t, c.toteID, "TBX-LEFTOVER", intPtr(26), "", nil)
	c.cart(t, c.toteID, "TBX-UNVERIFIED", nil, "", nil)
	c.cart(t, c.toteID, "TBX-PARTIAL", intPtr(12), c.part, nil)
	c.cart(t, bareID, "TBX-BARECART", nil, "", nil)
	elsewhere, err := nodes.CreateGroup(db.DB, "TBX-ELSE")
	testutil.MustNoErr(t, err, "another group")
	testdb.MarkGroup(t, db, elsewhere)
	far := &nodes.Node{Name: "TBX-ELSE-1", Enabled: true, ParentID: &elsewhere}
	testutil.MustNoErr(t, nodes.Create(db.DB, far), "its slot")
	c.cart(t, c.toteID, "TBX-TB-FAR", intPtr(0), "", far)

	res := c.find(t, c.pull("tbx-1"))
	if res.Outcome != OutcomeWait {
		t.Fatalf("the pull found %v (bin %v); want a wait: nothing in its source is tops & bottoms", res.Outcome, res.Bin)
	}
	if res.QueueParams.Kind != "tops_bottoms" || res.QueueParams.Group != c.src {
		t.Errorf("waits with kind %q in %q, want tops_bottoms in %s", res.QueueParams.Kind, res.QueueParams.Group, c.src)
	}
	if got := materialSentence(res.QueueParams); got != "Waiting for tops & bottoms in "+c.src {
		t.Errorf("the sentence is %q", got)
	}

	// NEVER PLANT-WIDE: a source no tier scopes (a synthetic node that is no
	// group or lane) is the one that would reach the plant-wide scan, where
	// tops & bottoms stands; the exact need waits there instead.
	testutil.MustNoErr(t, nodes.Create(db.DB, &nodes.Node{Name: "TBX-SYN", Enabled: true, IsSynthetic: true}), "a synthetic source")
	odd := c.pull("tbx-2")
	odd.SourceNode = "TBX-SYN"
	if res := c.find(t, odd); res.Outcome != OutcomeWait || res.QueueParams.Group != "TBX-SYN" {
		t.Errorf("from a synthetic source the pull found %v (bin %+v) in %q; want a wait there, never the plant",
			res.Outcome, res.Bin, res.QueueParams.Group)
	}

	tb := c.cart(t, c.toteID, "TBX-TB", intPtr(0), "", nil)
	res = c.find(t, c.pull("tbx-1"))
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != tb {
		t.Fatalf("with tops & bottoms in its source the pull found %v (bin %+v), want bin %d", res.Outcome, res.Bin, tb)
	}
}

// TestTopsAndBottomsPull_AStrictGroupThatSupportsThePressServesIt: the pull's
// asker is the fed press (the find sets it as the need's process node), so a
// strict maintained group that supports the press serves it; one that does
// not support it is fenced.
func TestTopsAndBottomsPull_AStrictGroupThatSupportsThePressServesIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBS")
	tb := c.cart(t, c.toteID, "TBS-TB", intPtr(0), "", nil)
	c.exec(t, `INSERT INTO node_properties (node_id, key, value) VALUES ($1, $2, 'on')`, c.srcID, nodes.PropStrictSourcing)

	if res := c.find(t, c.pull("tbs-1")); res.Outcome != OutcomeWait || res.QueueCause != queueCauseOf(plan.CauseFinderGroupFenced) {
		t.Fatalf("a strict group that does not support the press: %v under %q, want fenced", res.Outcome, res.QueueCause)
	}
	c.exec(t, `INSERT INTO node_maintain_supports (group_node_id, process_node_id) VALUES ($1, $2)`, c.srcID, c.pressID)
	if res := c.find(t, c.pull("tbs-1")); res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != tb {
		t.Fatalf("a strict group that supports the press: %v under %q, want bin %d", res.Outcome, res.QueueCause, tb)
	}
}

// TestPressFor_TheOutcomes: one statement answers the press a pull of tops &
// bottoms is for: the fed press whose outside the delivery is and whose claim
// carries the part, whatever carriers the part rides (which of them the pull
// brings is the finder's ordinary derivation). A part the press is not fed
// with, or a delivery that is no fed press's outside, is refused.
func TestPressFor_TheOutcomes(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	c := newTBPCell(t, db, "TBC")
	other := &bins.BinType{Code: "TBC-TOTE2"}
	testutil.MustNoErr(t, db.CreateBinType(other), "a second tote carrier")
	c.exec(t, `UPDATE bin_types SET totes_per_layer = 6 WHERE id = $1`, other.ID)
	knock := &bins.BinType{Code: "TBC-KD"}
	testutil.MustNoErr(t, db.CreateBinType(knock), "a knockdown")
	pl, err := db.GetPayloadByCode(c.part)
	testutil.MustNoErr(t, err, "part")
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })

	rule := func(ids ...int64) {
		testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, ids), "the part's carrier rule")
	}
	for _, k := range []struct {
		name    string
		set     func()
		part    string
		at      string
		refusal string // "" for the press
	}{
		{"the rule names one tote carrier", func() { rule(c.toteID) }, c.part, c.outside, ""},
		{"the rule names two", func() { rule(c.toteID, other.ID) }, c.part, c.outside, ""},
		{"a rule with no tote carrier", func() { rule(knock.ID) }, c.part, c.outside, ""},
		{"no rule", func() { rule() }, c.part, c.outside, ""},
		{"a part the press is not fed with", func() { rule(c.toteID) }, "TBC-OTHER", c.outside, "is not the outside position of a press fed with"},
		{"a node that is no outside", func() { rule(c.toteID) }, c.part, c.press, "is not the outside position of a press fed with"},
	} {
		k.set()
		counter.Reset()
		press, refusal, err := pressFor(cdb, k.part, k.at)
		testutil.MustNoErr(t, err, k.name)
		if n := len(counter.Statements()); n != 1 {
			t.Errorf("%s: %d statements, want one", k.name, n)
		}
		if k.refusal == "" && (refusal != "" || press != c.press) {
			t.Errorf("%s: %q refused %q, want %s", k.name, press, refusal, c.press)
		}
		if k.refusal != "" && (press != "" || !strings.Contains(refusal, k.refusal)) {
			t.Errorf("%s: %q refused %q, want it to say %q", k.name, press, refusal, k.refusal)
		}
	}
}

// orderErrorFor is the order.error reply intake sent for uuid, nil for none.
func orderErrorFor(t *testing.T, db *store.DB, uuid string) *protocol.OrderError {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderError {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var p protocol.OrderError
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "order.error")
		if p.OrderUUID == uuid {
			return &p
		}
	}
	return nil
}

// TestTopsAndBottomsPull_IntakeRefuses: what cannot resolve is refused at
// intake, decided before the row exists, with the sentence on the reply and on
// the row it fails; a pull that resolves is admitted, and waits for its cart
// (an empty source).
func TestTopsAndBottomsPull_IntakeRefuses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBI")
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: "TBI-NONE", UOPCapacity: 40}), "a part the press is not fed with")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	ask := func(uuid, part, src string) {
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid,
			OrderType: OrderTypeRetrieve, RetrieveEmpty: true, Want: protocol.TopsAndBottomsOnly,
			PayloadCode: part, SourceNode: src, DeliveryNode: c.outside, Quantity: 1})
	}
	for _, k := range []struct {
		name, uuid, part, src string
		code, detail          string
	}{
		{"a part the press is not fed with", "tbi-none", "TBI-NONE", c.src, string(protocol.TermStructural),
			c.outside + " is not the outside position of a press fed with TBI-NONE"},
		{"a blank inbound source", "tbi-blank", c.part, "", string(protocol.TermMissingSource),
			c.press + " names no inbound source"},
	} {
		ask(k.uuid, k.part, k.src)
		f := orderErrorFor(t, db, k.uuid)
		if f == nil {
			t.Fatalf("%s: not refused", k.name)
		}
		if f.ErrorCode != k.code || f.Detail != k.detail {
			t.Errorf("%s: refused %s %q, want %s %q", k.name, f.ErrorCode, f.Detail, k.code, k.detail)
		}
		// Decided before the row is born, and the row is born failed with the
		// sentence, as every planning refusal is (a move's missing source too).
		if o := testdb.RequireOrder(t, db, k.uuid); o.Status != protocol.StatusFailed || o.ErrorDetail != k.detail {
			t.Errorf("%s: the row is %s with %q, want failed with the sentence", k.name, o.Status, o.ErrorDetail)
		}
	}
	ask("tbi-ok", c.part, c.src)
	if f := orderErrorFor(t, db, "tbi-ok"); f != nil {
		t.Fatalf("a pull that resolves was refused: %+v", f)
	}
	o := testdb.RequireOrder(t, db, "tbi-ok")
	if o.Status != protocol.StatusQueued || o.SourceIntent != SourceIntentEmpty || o.Want != protocol.TopsAndBottomsOnly ||
		o.QueueReason != "Waiting for tops & bottoms in "+c.src {
		t.Errorf("the pull is %s (%s, want %v) under %q, want queued as an empty wanting tops & bottoms, waiting for its cart",
			o.Status, o.SourceIntent, o.Want, o.QueueReason)
	}
}

// TestTopsAndBottomsPull_TheRetiredTypeIsUnknown: the pull's order type is
// gone with no deprecation arm (it never reached a plant), so an Edge older
// than its Core that still sends retrieve_tops_bottoms is refused at intake as
// an unknown type, before a row exists, and the outside stands vacant until the
// next swap fetches for it. That is the cost of Core first; Edge first would
// pull a stack to the outside instead.
func TestTopsAndBottomsPull_TheRetiredTypeIsUnknown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBO")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "tbo-1",
		OrderType: protocol.OrderType("retrieve_tops_bottoms"), RetrieveEmpty: true, PayloadCode: c.part,
		SourceNode: c.src, DeliveryNode: c.outside, Quantity: 1})
	if f := orderErrorFor(t, db, "tbo-1"); f == nil || f.ErrorCode != string(protocol.TermUnknownType) {
		t.Fatalf("intake answered %+v, want one unknown_type refusal", f)
	}
	if o, err := db.GetOrderByUUID("tbo-1"); err == nil && o != nil {
		t.Errorf("an order row exists (%d, %s); the refusal leaves none", o.ID, o.Status)
	}
}

// wirePull is SEND BACK's pull as the Edge puts it on the wire: a plain empty
// retrieve stating its want, decoded the way intake decodes it.
func (c *tbpCell) wirePull(t *testing.T, d *Dispatcher, uuid string) {
	t.Helper()
	raw := fmt.Sprintf(`{"order_uuid":%q,"order_type":"retrieve","retrieve_empty":true,"want":"tops_bottoms",
		"payload_code":%q,"source_node":%q,"delivery_node":%q,"quantity":1}`, uuid, c.part, c.src, c.outside)
	var req protocol.OrderRequest
	testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &req), "the wire request")
	d.HandleOrderRequest(testEnvelope(), &req)
}

// TestTopsAndBottomsPull_TheWantOnTheWireIsWhatItWaitsFor: the pull is a plain
// empty retrieve, and the want it states is what it takes. With only a stack
// (3 high) in its source it waits for tops & bottoms there, as the type's pull
// did; a Core that drops the want would take the stack.
func TestTopsAndBottomsPull_TheWantOnTheWireIsWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBW")
	c.cart(t, c.toteID, "TBW-STACK", intPtr(24), "", nil)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	c.wirePull(t, d, "tbw-1")
	o := testdb.RequireOrder(t, db, "tbw-1")
	if o.OrderType != OrderTypeRetrieveEmpty || o.Status != protocol.StatusQueued ||
		o.QueueReason != "Waiting for tops & bottoms in "+c.src {
		t.Errorf("the pull is a %s, %s under %q; want a retrieve_empty queued waiting for tops & bottoms in %s",
			o.OrderType, o.Status, o.QueueReason, c.src)
	}
}

// TestTopsAndBottomsPull_APartOnTwoToteCarriersTakesEither: the part rides two
// tote carriers, and the pull takes tops & bottoms of either: the ordinary
// derivation chooses among the part's carriers (its rule), so the pull is no
// longer refused for naming more than one.
func TestTopsAndBottomsPull_APartOnTwoToteCarriersTakesEither(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TB2")
	second := &bins.BinType{Code: "TB2-TOTE2"}
	testutil.MustNoErr(t, db.CreateBinType(second), "a second tote carrier")
	c.exec(t, `UPDATE bin_types SET totes_per_layer = 6 WHERE id = $1`, second.ID)
	pl, err := db.GetPayloadByCode(c.part)
	testutil.MustNoErr(t, err, "part")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{c.toteID, second.ID}), "the part rides both")
	tb := c.cart(t, second.ID, "TB2-TB", intPtr(0), "", nil)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	c.wirePull(t, d, "tb2-1")
	if f := orderErrorFor(t, db, "tb2-1"); f != nil {
		t.Fatalf("the pull was refused: %s %q", f.ErrorCode, f.Detail)
	}
	o := testdb.RequireOrder(t, db, "tb2-1")
	if o.Plan == nil || o.Plan.BinID != tb {
		t.Errorf("the pull is %s under %q with plan %+v; want it to have picked the tops & bottoms %d",
			o.Status, o.QueueReason, o.Plan, tb)
	}
}

// TestTopsAndBottomsPull_APartWithNoToteCarrierSaysSo: the part's carrier rule
// names only a carrier that is no tote carrier, so no tops & bottoms is one the
// part may ride, though tops & bottoms stands in the pull's source. The pull
// waits, as it did once the intake refusal went, and its sentence names the
// configuration to fix rather than a shortage; a rule that names the tote
// carrier waits for tops & bottoms as before.
func TestTopsAndBottomsPull_APartWithNoToteCarrierSaysSo(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newTBPCell(t, db, "TBN")
	knock := &bins.BinType{Code: "TBN-KD"}
	testutil.MustNoErr(t, db.CreateBinType(knock), "a knockdown")
	pl, err := db.GetPayloadByCode(c.part)
	testutil.MustNoErr(t, err, "part")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{knock.ID}), "the part rides the knockdown only")
	c.cart(t, c.toteID, "TBN-TB", intPtr(0), "", nil)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	c.wirePull(t, d, "tbn-1")
	if f := orderErrorFor(t, db, "tbn-1"); f != nil {
		t.Fatalf("the pull was refused: %s %q; want it to wait", f.ErrorCode, f.Detail)
	}
	o := testdb.RequireOrder(t, db, "tbn-1")
	want := "No tote carrier is configured for " + c.part + " — waiting for its configuration to be fixed"
	if o.Status != protocol.StatusQueued || o.QueueReason != want || o.QueueCause != "finder-no-tote-carrier" {
		t.Errorf("the pull is %s under %q (%s); want queued under %q (%s)",
			o.Status, o.QueueReason, o.QueueCause, want, "finder-no-tote-carrier")
	}

	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{knock.ID, c.toteID}), "the rule names the tote carrier")
	c.exec(t, `DELETE FROM bins WHERE label = 'TBN-TB'`)
	c.wirePull(t, d, "tbn-2")
	o = testdb.RequireOrder(t, db, "tbn-2")
	if o.QueueReason != "Waiting for tops & bottoms in "+c.src {
		t.Errorf("with the tote carrier in the rule the pull waits under %q, want the shortage", o.QueueReason)
	}
}
