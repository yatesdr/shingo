//go:build docker

package store_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/comingvectors"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestComingVectors_CoreAgreesOverStoredRows is Core's half of the "coming"
// agreement: every leg the Edge's builders write (shared/comingvectors, kept
// current by the Edge's TestComingVectors_AreTheBuildersShapes), stored as Core
// stores it (a complex leg's steps_json and its derived delivery node, the last
// node it acts at; a plain order's delivery node, with the plan Core persists
// at dispatch when the case carries one), answers CarrierComingTo at every
// node it names exactly as the case says: the statement's prefilter finds it,
// and its decision agrees. A complex case with no steps is skipped: Core's
// intake refuses a complex order with none (dispatch
// handleComplexOrderRequest), so Core never holds the row; it is the Edge's,
// a row Core projected down without its steps.
func TestComingVectors_CoreAgreesOverStoredRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cases, err := comingvectors.Cases()
	testutil.MustNoErr(t, err, "the vectors")
	if len(cases) == 0 {
		t.Fatal("no vectors: the file is empty")
	}
	stranger := &orders.Order{} // no row, no partner: every live row is another's
	for i, c := range cases {
		isComplex := protocol.OrderType(c.OrderType) == protocol.OrderTypeComplex
		if isComplex && len(c.Steps) == 0 {
			continue
		}
		raw, delivery := string(c.Steps), c.DeliveryNode
		if isComplex {
			var steps []protocol.ComplexOrderStep
			testutil.MustNoErr(t, json.Unmarshal(c.Steps, &steps), c.Name)
			for _, s := range steps {
				if s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff {
					delivery = s.Node
				}
			}
		}
		o := &orders.Order{EdgeUUID: fmt.Sprintf("cv-%d", i), StationID: "line-1", OrderType: protocol.OrderType(c.OrderType),
			Status: protocol.StatusDispatched, Quantity: 1, DeliveryNode: delivery, StepsJSON: raw}
		testutil.MustNoErr(t, db.CreateOrder(o), c.Name)
		for node, want := range c.Places {
			got, err := db.CarrierComingTo(node, stranger)
			testutil.MustNoErr(t, err, c.Name+" at "+node)
			if got != want {
				t.Errorf("%s: coming to %s = %v, the vectors say %v", c.Name, node, got, want)
			}
		}
		_, err := db.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, o.ID)
		testutil.MustNoErr(t, err, "retire "+c.Name)
	}
}
