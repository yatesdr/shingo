//go:build docker

package store_test

import (
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/internal/helpers"
	"shingocore/store/orders"
)

// TestStepNodeSQL_AgreesWithStepNode holds the two spellings of "where plan step
// i happens" together: helpers.StepNodeSQL (the carts' SQL readers) and
// domain.StepNode (the Go ones), over stored plans and implied ones, every step
// and the indexes either side of the plan.
func TestStepNodeSQL_AgreesWithStepNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	plans := []struct{ name, steps, source, delivery string }{
		{"implied", "", "SRC", "DST"},
		{"implied, no delivery", "", "SRC", ""},
		{"empty array", "[]", "SRC", "DST"},
		{"a swap", `[{"action":"wait","node":"P"},{"action":"pickup","node":"P"},{"action":"dropoff","node":"W"},` +
			`{"action":"pickup","node":"S"},{"action":"dropoff","node":"P"}]`, "P", "P"},
		{"a group not chosen yet", `[{"action":"pickup","group":"GRP"},{"action":"dropoff","node":"LINE"}]`, "GRP", "LINE"},
		{"a node over its group", `[{"action":"pickup","node":"GRP.S1","group":"GRP"},{"action":"dropoff","node":"LINE"}]`, "GRP.S1", "LINE"},
		{"a bare wait", `[{"action":"wait"},{"action":"pickup","node":"A"},{"action":"dropoff","node":"B"}]`, "A", "B"},
	}
	for _, p := range plans {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StepsJSON, o.SourceNode, o.DeliveryNode = "step-node-"+p.name, p.steps, p.source, p.delivery
		})
		for i := -1; i <= 6; i++ {
			var got string
			if err := db.QueryRow(`SELECT `+helpers.StepNodeSQL("o", "$2::int")+` FROM orders o WHERE o.id = $1`, o.ID, i).Scan(&got); err != nil {
				t.Fatalf("%s step %d: %v", p.name, i, err)
			}
			if want := domain.StepNode(o, i); got != want {
				t.Errorf("%s step %d: StepNodeSQL %q, StepNode %q", p.name, i, got, want)
			}
		}
	}
}
