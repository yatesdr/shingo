//go:build docker

package orders_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestGetRecovering_FindsTheReturnFromTheCancelledEnd pins the reverse lookup
// the order page uses: from a cancelled order to the return that carried its
// bin back. No return reads as (nil, nil), not an error, because that is every
// cancelled order whose bin never left its node. Two returns for one cancel
// resolve to the newer.
func TestGetRecovering_FindsTheReturnFromTheCancelledEnd(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cancelled := testdb.CreateOrder(t, db)
	unrelated := testdb.CreateOrder(t, db)

	got, err := db.GetOrderRecovering(cancelled.ID)
	if err != nil || got != nil {
		t.Fatalf("no return yet: got (%v, %v), want (nil, nil)", got, err)
	}

	first := testdb.CreateOrder(t, db, func(o *orders.Order) { o.RecoversOrderID = &cancelled.ID })
	second := testdb.CreateOrder(t, db, func(o *orders.Order) { o.RecoversOrderID = &cancelled.ID })

	got, err = db.GetOrderRecovering(cancelled.ID)
	if err != nil {
		t.Fatalf("GetOrderRecovering: %v", err)
	}
	if got == nil || got.ID != second.ID {
		t.Fatalf("GetOrderRecovering(%d) = %v, want the newer return #%d (older is #%d)",
			cancelled.ID, got, second.ID, first.ID)
	}
	if got.RecoversOrderID == nil || *got.RecoversOrderID != cancelled.ID {
		t.Errorf("return #%d reads RecoversOrderID %v, want %d", got.ID, got.RecoversOrderID, cancelled.ID)
	}

	if got, err := db.GetOrderRecovering(unrelated.ID); err != nil || got != nil {
		t.Errorf("order with no return: got (%v, %v), want (nil, nil)", got, err)
	}
}
