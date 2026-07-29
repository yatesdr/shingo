package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// TestStage3_IsCoordinated pins the provenance discriminator: it reads the
// order.Coordinated COLUMN, NOT StepsJSON. The decoupling is the whole point of
// the provenance column — the unified-create follow-up persists a step plan onto
// a PLAIN order, and that order must still classify plain. A no-wait coordinated
// leg (changeover release) classifies coordinated because it was STAMPED so at
// intake, not because of its plan shape.
func TestStage3_IsCoordinated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		coordinated bool
		steps       string
		want        bool
	}{
		{"plain single-transport order, unstamped", false, "", false},
		{"coordinated order (stamped at complex intake)", true, `[{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, true},
		{"decoupling: a PLAIN order that persisted a step plan is still plain", false, `[{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, false},
		{"coordinated stamp holds even with no persisted steps (defensive)", true, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := &orders.Order{Coordinated: c.coordinated, StepsJSON: c.steps}
			if got := IsCoordinated(o); got != c.want {
				t.Errorf("IsCoordinated(Coordinated=%v, StepsJSON=%q) = %v, want %v", c.coordinated, c.steps, got, c.want)
			}
		})
	}
}

// TestStage4_SourceIntentForType pins the label→data mapping used at intake. The
// store OrderType constant survives (resolver-direction token) and still falls to
// the default → "" (full), NOT "local": mapping it to SourceIntentLocal would have
// exempted it from the scanner's payload guard. The mapping is unchanged; this
// test guards the default branch against a stray store→local regression.
func TestStage4_SourceIntentForType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  protocol.OrderType
		want string
	}{
		{OrderTypeRetrieve, SourceIntentFull},       // full payload-matched bin
		{OrderTypeRetrieveEmpty, SourceIntentEmpty}, // generic empty carrier
		{OrderTypeMove, SourceIntentLocal},          // bin AT a concrete node
		{OrderTypeStore, SourceIntentFull},          // self-sources — no finder intent (must NOT be local)
		{OrderTypeComplex, SourceIntentFull},        // coordinated — sourced per-leg, not here
	}
	for _, c := range cases {
		if got := SourceIntentForType(c.typ); got != c.want {
			t.Errorf("SourceIntentForType(%s) = %q, want %q", c.typ, got, c.want)
		}
	}
}

// TestStage3_AssertSimpleNotCoordinated_DoesNotPanic exercises the tripwire on both a
// clean simple order and a (bug-state) simple order stamped coordinated — it must
// log loudly without panicking, and must ignore complex-type orders. Post-column
// the bug state is Coordinated=true on a plain type (NOT StepsJSON — the
// unified-create follow-up gives plain orders steps legitimately).
func TestStage3_AssertSimpleNotCoordinated_DoesNotPanic(t *testing.T) {
	t.Parallel()
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeRetrieve})                       // clean simple
	AssertSimpleNotCoordinated(&orders.Order{ID: 7, OrderType: OrderTypeMove, Coordinated: true}) // bug state — logs
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeMove, StepsJSON: `[{"a":1}]`})   // plain w/ steps — clean
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeComplex, Coordinated: true})     // complex — ignored
}

// TestClassifyInboundOrigin is the intake classification matrix. Every row is a
// message an Edge can actually send, including the ones it should not.
//
// The two that carry the design: an UNSTATED origin lands `orphan`, not
// `no_demand`, because Edge ships before Core and the skew window is precisely
// when every threshold order arrives bare — silencing it there would hide real
// losses for the length of a rollout. And a MALFORMED id lands orphan rather
// than failing the order: origin_id is a UUID column, so storing "not-a-uuid"
// would abort the INSERT and lose the transport work over a telemetry field.
func TestClassifyInboundOrigin(t *testing.T) {
	t.Parallel()
	const good = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	cases := []struct {
		name      string
		id, class string
		wantID    string
		wantClass string
	}{
		{"attached: id and class agree", good, protocol.OriginClassAttached, good, protocol.OriginClassAttached},
		{"attached: id wins over a blank class", good, "", good, protocol.OriginClassAttached},
		{"attached: id wins over a contradicting class", good, protocol.OriginClassNoDemand, good, protocol.OriginClassAttached},
		{"no_demand: stamped by Edge at its create site", "", protocol.OriginClassNoDemand, "", protocol.OriginClassNoDemand},
		{"orphan: said nothing at all (an Edge that predates origins)", "", "", "", protocol.OriginClassOrphan},
		{"orphan: claimed attached with nothing to attach to", "", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
		{"orphan: said orphan outright", "", protocol.OriginClassOrphan, "", protocol.OriginClassOrphan},
		{"orphan: class off the enum", "", "sort-of-attached", "", protocol.OriginClassOrphan},
		{"orphan: id that is not a UUID (must not fail the order)", "not-a-uuid", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
		{"orphan: id that is nearly a UUID", good + "x", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotID, gotClass := classifyInboundOrigin(c.id, c.class, "line-1", "uuid-test")
			if gotID != c.wantID {
				t.Errorf("origin_id = %q, want %q", gotID, c.wantID)
			}
			if gotClass != c.wantClass {
				t.Errorf("origin_class = %q, want %q", gotClass, c.wantClass)
			}
		})
	}
}

// TestClassifyInboundOrigin_NeverReturnsAnEmptyClass is the property the enum
// exists for. An empty origin_class is the state the column was added to
// abolish: "origin_id IS NULL AND the class is empty" is the old unanswerable
// question wearing a new column, and a row in it belongs to no bucket on the
// surface — invisible rather than merely uninteresting.
func TestClassifyInboundOrigin_NeverReturnsAnEmptyClass(t *testing.T) {
	t.Parallel()
	ids := []string{"", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "garbage", "  "}
	classes := []string{"", protocol.OriginClassAttached, protocol.OriginClassNoDemand, protocol.OriginClassOrphan, "junk"}
	for _, id := range ids {
		for _, cl := range classes {
			gotID, gotClass := classifyInboundOrigin(id, cl, "line-1", "uuid-test")
			if gotClass == "" {
				t.Errorf("classifyInboundOrigin(%q, %q) returned an EMPTY class — that row lands in no bucket", id, cl)
			}
			if gotID != "" && gotClass != protocol.OriginClassAttached {
				t.Errorf("classifyInboundOrigin(%q, %q) = (%q, %q): an id may only ever come back attached", id, cl, gotID, gotClass)
			}
		}
	}
}
