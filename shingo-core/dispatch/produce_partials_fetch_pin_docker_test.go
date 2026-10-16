//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// WHICH CARRIER A PRODUCE REFILL TAKES, with a partial of the part waiting.
//
// A produce line's refill wants a carrier to fill. A partial of the same part
// (payload X, 0 < count < capacity) is one: finishing it is the point of a
// partial home. Each row stands a partial of X (held an hour) and, where the
// row says, a fresh empty in a group, sends the refill the way its requester
// sends it, and names the carrier Core claimed: "partial", "empty" or "waits".
//
// Rows carry today (the tree before produce partials) and after (under it);
// the test asserts after, and a row whose after differs names its ruling.
//
// Not here, already pinned: a dedicated-loader pool as the source ranks a
// partial of X first (binsource.Fill, the only reach today) —
// TestPlanRetrieveEmpty_DedicatedLoaderPool_FillPrefersPartialOverEmpty.

type ppFetchRow struct {
	name string
	// complex: the swap's supply leg (a complex Empty pickup at the group);
	// else the keeper's and a bare line's plain retrieve_empty.
	complex bool
	// empty: a fresh empty stands in the group beside the partial.
	empty        bool
	today, after string
}

// ppFetchGroup is an NGRP of three slots: the partial of X in one, a fresh
// empty in another when asked for.
func ppFetchGroup(t *testing.T, db *store.DB, prefix string, empty bool) (grp *nodes.Node, partialID, emptyID int64) {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP type: %v", err)
	}
	grp = &nodes.Node{Name: prefix + "-GRP", IsSynthetic: true, NodeTypeID: &ngrp.ID, Enabled: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("group: %v", err)
	}
	var slots []*nodes.Node
	for i := 1; i <= 3; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", prefix, i), ParentID: &grp.ID, Enabled: true}
		if err := db.CreateNode(s); err != nil {
			t.Fatalf("slot: %v", err)
		}
		slots = append(slots, s)
	}
	partialID = makeLoaderBin(t, db, "PART-X", slots[0].ID, prefix+"-PARTIAL", 5, time.Now().UTC().Add(-time.Hour)).ID
	if empty {
		emptyID = makeEmptyBin(t, db, slots[1].ID, prefix+"-EMPTY").ID
	}
	return grp, partialID, emptyID
}

func ppFetchClaimed(o *orders.Order, partialID, emptyID int64) string {
	switch {
	case o.BinID == nil:
		return "waits"
	case *o.BinID == partialID:
		return "partial"
	case *o.BinID == emptyID:
		return "empty"
	}
	return fmt.Sprintf("bin %d", *o.BinID)
}

func TestProducePartialsPin_Fetch(t *testing.T) {
	rows := []ppFetchRow{
		// The empty-carrier search reads payload '' only: a partial is not a
		// candidate, so a fresh empty is taken and, without one, the refill
		// waits beside a partial it could finish.
		{name: "retrieve_empty/partial+empty", empty: true, today: "empty", after: "empty"},
		{name: "retrieve_empty/partial-only", today: "waits", after: "waits"},
		// The complex Empty pickup at a group resolves through the same
		// search (finder tier 3), and skips the supply widening.
		{name: "complex-empty-pickup/partial+empty", complex: true, empty: true, today: "empty", after: "empty"},
		{name: "complex-empty-pickup/partial-only", complex: true, today: "waits", after: "waits"},
	}
	for i, r := range rows {
		r := r
		prefix := fmt.Sprintf("PPF%d", i)
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			_, lineNode, _ := setupTestData(t, db)
			if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", UOPCapacity: 10}); err != nil {
				t.Fatalf("PART-X payload: %v", err)
			}
			grp, partialID, emptyID := ppFetchGroup(t, db, prefix, r.empty)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			if r.complex {
				// Group steps resolve at intake only with the group resolver wired,
				// as the engine wires it.
				d, _ = newTestDispatcherWithResolver(t, db)
			}
			uuid := prefix + "-ORDER"
			var o *orders.Order
			if r.complex {
				d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
					OrderUUID: uuid, PayloadCode: "PART-X", Quantity: 1, ProcessNode: lineNode.Name,
					Steps: []protocol.ComplexOrderStep{
						{Action: protocol.ActionPickup, Node: grp.Name, Empty: true},
						{Action: protocol.ActionDropoff, Node: lineNode.Name},
					},
				})
				got, err := db.GetOrderByUUID(uuid)
				if err != nil {
					t.Fatalf("complex order not created: %v", err)
				}
				_ = d.DispatchPreparedComplex(got)
				o, err = db.GetOrderByUUID(uuid)
				if err != nil {
					t.Fatalf("reload: %v", err)
				}
			} else {
				d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
					OrderUUID: uuid, OrderType: OrderTypeRetrieveEmpty, PayloadCode: "PART-X",
					DeliveryNode: lineNode.Name, SourceNode: grp.Name, Quantity: 1,
				})
				o = dispatchSimpleViaScanner(t, d, db, uuid)
			}
			got := ppFetchClaimed(o, partialID, emptyID)
			t.Logf("PIN %-40s %s (status %s, cause %q)", r.name, got, o.Status, o.QueueCause)
			if got != r.after {
				t.Errorf("%s: claimed %s, want %s (today %s)", r.name, got, r.after, r.today)
			}
		})
	}
}
