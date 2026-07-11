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
