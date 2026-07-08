package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// TestStage3_IsCoordinated pins the provenance discriminator, including the case
// that broke the wait-presence idea: a NO-WAIT complex order (a changeover
// release / staged-deliver leg) still classifies as coordinated because it
// carries a step plan. A plain single-transport order (no StepsJSON) does not.
func TestStage3_IsCoordinated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		steps string
		want  bool
	}{
		{"plain single-transport order has no step plan", "", false},
		{"complex order with a wait plan (swap)", `[{"action":"wait","node":"LINE"},{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, true},
		{"NO-WAIT complex order (changeover release) is still coordinated", `[{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, true},
		{"NO-WAIT complex delivering to a line (staged deliver) is still coordinated", `[{"action":"pickup","node":"STAGE"},{"action":"dropoff","node":"LINE"}]`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCoordinated(&orders.Order{StepsJSON: c.steps}); got != c.want {
				t.Errorf("IsCoordinated(StepsJSON=%q) = %v, want %v", c.steps, got, c.want)
			}
		})
	}
}

// TestStage4_SourceIntentForType pins the label→data mapping used at intake. The
// critical, non-obvious case is store → "" (full/default), NOT "local": store
// self-sources in planStore and never consults the finder, and the scanner's
// payload guard historically FIRED for a blank-payload store (OrderType != Move).
// Mapping store to SourceIntentLocal would silently exempt it from that guard —
// a behavior change the re-homing must not make. This test would fail if store
// ever regained a non-default intent.
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

// TestStage3_AssertSimpleHasNoSteps_DoesNotPanic exercises the tripwire on both a
// clean simple order and a (bug-state) simple order carrying steps — it must log
// loudly without panicking, and must ignore complex-type orders.
func TestStage3_AssertSimpleHasNoSteps_DoesNotPanic(t *testing.T) {
	t.Parallel()
	AssertSimpleHasNoSteps(&orders.Order{OrderType: OrderTypeRetrieve})                             // clean simple
	AssertSimpleHasNoSteps(&orders.Order{ID: 7, OrderType: OrderTypeStore, StepsJSON: `[{"a":1}]`}) // bug state — logs
	AssertSimpleHasNoSteps(&orders.Order{OrderType: OrderTypeComplex, StepsJSON: `[{"a":1}]`})      // complex — ignored
}
