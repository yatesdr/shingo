package testdb

import (
	"testing"

	"shingocore/store"
	"shingocore/store/orders"
)

// HoldsAtRest renders every violation of orders.HoldsAtRestSQL as one line: an
// order that has not reached `dispatched` holding bins.claimed_by or
// nodes.claimed_by.
func HoldsAtRest(t testing.TB, db *store.DB) []string {
	t.Helper()
	held, err := orders.ListHoldsAtRest(db.DB)
	if err != nil {
		t.Fatalf("list holds at rest: %v", err)
	}
	var out []string
	for _, h := range held {
		out = append(out, h.Line())
	}
	return out
}

// AssertNoHoldsAtRest fails the test if any waiting order holds a hard claim.
func AssertNoHoldsAtRest(t testing.TB, db *store.DB, when string) {
	t.Helper()
	if v := HoldsAtRest(t, db); len(v) != 0 {
		t.Errorf("%s: an order waits holding a hard claim: %v", when, v)
	}
}

// VerifyRed is the committed form of a red pin. The caller computes the
// invariant's violations; while the bug stands they are PREDICTED, so the test
// passes and logs them. The moment they are gone the test fails, telling whoever
// fixed it to replace the call with the invariant's own assertion (for held
// claims, AssertNoHoldsAtRest). It knows nothing of the invariant, so its
// message names none.
//
// This keeps the gate green while the red stays checked on every run: a
// predicted red that quietly went green would otherwise be invisible until
// someone looked, and a fixture that stopped reaching the bug would read as a
// fix.
func VerifyRed(t *testing.T, name string, violations []string) {
	t.Helper()
	if len(violations) == 0 {
		t.Fatalf("VERIFY-RED %s is GREEN: the predicted violation is gone. If the fix landed, "+
			"replace VerifyRed with the invariant's assertion; if not, the fixture no longer "+
			"reaches the bug and has to be rebuilt.", name)
	}
	for _, v := range violations {
		t.Logf("VERIFY-RED %s, as predicted: %s", name, v)
	}
}
