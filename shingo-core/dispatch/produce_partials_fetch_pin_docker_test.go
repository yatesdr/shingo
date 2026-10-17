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
	"shingocore/store/plantclaims"
)

// WHICH CARRIER A PRODUCE REFILL TAKES, with a partial of the part waiting.
//
// A produce line's refill wants a carrier to fill. A partial of the same part
// (payload X, 0 < count < capacity) is one: finishing it is the point of a
// partial home. Each row stands a partial of X (held an hour) in the source
// group or in a second group, and where the row says a fresh empty in the
// source group; the claim making X names a partial home (the source group, the
// second group) or none; the refill is sent the way its requester sends it,
// and the row names the carrier Core claimed: "partial", "empty" or "waits".
//
// Rows carry today (the tree before produce partials) and after (under it);
// the test asserts after, and a row whose after differs names its ruling.
//
// Not here, already pinned: a dedicated-loader pool as the source ranks a
// partial of X first (binsource.Fill) —
// TestPlanRetrieveEmpty_DedicatedLoaderPool_FillPrefersPartialOverEmpty.

type ppFetchRow struct {
	name string
	// complex: the swap's supply leg (a complex Empty pickup); else the keeper's
	// and a bare line's plain retrieve_empty.
	complex bool
	// concrete: the complex pickup names a concrete slot of the source group
	// (the empty's), not the group.
	concrete bool
	// empty: a fresh empty stands in the source group.
	empty bool
	// home: the claim making X names "source" (the source group), "elsewhere"
	// (the second group, where the partial then stands) or "" (no home).
	home         string
	today, after string
}

// ppFetchCell is a source group and a second group of three slots each, the
// partial of X in the home's group (the source group unless the home is
// elsewhere), a fresh empty in the source group when asked for, and the claim
// mirror naming the home.
type ppFetchCell struct {
	src, other         *nodes.Node
	emptySlot          *nodes.Node
	partialID, emptyID int64
}

func ppFetchGroups(t *testing.T, db *store.DB, prefix string, r ppFetchRow) ppFetchCell {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("NGRP type: %v", err)
	}
	group := func(name string) (*nodes.Node, []*nodes.Node) {
		g := &nodes.Node{Name: name, IsSynthetic: true, NodeTypeID: &ngrp.ID, Enabled: true}
		if err := db.CreateNode(g); err != nil {
			t.Fatalf("group: %v", err)
		}
		var slots []*nodes.Node
		for i := 1; i <= 3; i++ {
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), ParentID: &g.ID, Enabled: true}
			if err := db.CreateNode(s); err != nil {
				t.Fatalf("slot: %v", err)
			}
			slots = append(slots, s)
		}
		return g, slots
	}
	var c ppFetchCell
	var srcSlots, otherSlots []*nodes.Node
	c.src, srcSlots = group(prefix + "-SRC")
	c.other, otherSlots = group(prefix + "-HOME")
	at := srcSlots[0]
	if r.home == "elsewhere" {
		at = otherSlots[0]
	}
	c.partialID = makeLoaderBin(t, db, "PART-X", at.ID, prefix+"-PARTIAL", 5, time.Now().UTC().Add(-time.Hour)).ID
	c.emptySlot = srcSlots[1]
	if r.empty {
		c.emptyID = makeEmptyBin(t, db, srcSlots[1].ID, prefix+"-EMPTY").ID
	}
	home := map[string]string{"source": c.src.Name, "elsewhere": c.other.Name}[r.home]
	if err := db.ReplacePlantClaims(prefix+"-PROC",
		[]plantclaims.StyleRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STY", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STY", CoreNodeName: prefix + "-LINE",
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-X",
			InboundSource: c.src.Name, PartialDestination: home}}, 0); err != nil {
		t.Fatalf("the claim mirror: %v", err)
	}
	return c
}

func ppFetchClaimed(o *orders.Order, partialID, emptyID int64) string {
	switch {
	case o.BinID == nil:
		return "waits"
	case *o.BinID == partialID:
		return "partial"
	case emptyID != 0 && *o.BinID == emptyID:
		return "empty"
	}
	return fmt.Sprintf("bin %d", *o.BinID)
}

func TestProducePartialsPin_Fetch(t *testing.T) {
	rows := []ppFetchRow{
		// No home: the empty-carrier search reads payload '' only, so a fresh
		// empty is taken and, without one, the refill waits beside a partial it
		// could finish. Unchanged: a blank claim fetches no partial.
		{name: "retrieve_empty/no-home/partial+empty", empty: true, today: "empty", after: "empty"},
		{name: "retrieve_empty/no-home/partial-only", today: "waits", after: "waits"},
		{name: "complex-empty-pickup/no-home/partial+empty", complex: true, empty: true, today: "empty", after: "empty"},
		{name: "complex-empty-pickup/no-home/partial-only", complex: true, today: "waits", after: "waits"},
		// A finish-later claim (ruling: the Fill preference reaches every
		// empty-intent fetch): the oldest-held partial of X in X's home, or in
		// the fetch's own source group, before a fresh empty.
		{name: "retrieve_empty/home=source/partial+empty", empty: true, home: "source", today: "empty", after: "partial"},
		{name: "retrieve_empty/home=source/partial-only", home: "source", today: "waits", after: "partial"},
		{name: "retrieve_empty/home-elsewhere/partial+empty", empty: true, home: "elsewhere", today: "empty", after: "partial"},
		{name: "complex-empty-pickup/home=source/partial+empty", complex: true, empty: true, home: "source",
			today: "empty", after: "partial"},
		{name: "complex-empty-pickup/home-elsewhere/partial+empty", complex: true, empty: true, home: "elsewhere",
			today: "empty", after: "partial"},
		// A concrete source (the step names a slot of the source group): the
		// pickup joins the finder's partial tier, so the home's partial is
		// taken; today the step took the empty standing there.
		{name: "complex-empty-pickup/concrete-source/home-elsewhere", complex: true, concrete: true, empty: true,
			home: "elsewhere", today: "empty", after: "partial"},
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
			c := ppFetchGroups(t, db, prefix, r)
			uuid := prefix + "-ORDER"
			var o *orders.Order
			if r.complex {
				// Group steps resolve at intake only with the group resolver
				// wired, as the engine wires it.
				d, _ := newTestDispatcherWithResolver(t, db)
				from := c.src.Name
				if r.concrete {
					from = c.emptySlot.Name
				}
				d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
					OrderUUID: uuid, PayloadCode: "PART-X", Quantity: 1, ProcessNode: lineNode.Name,
					Steps: []protocol.ComplexOrderStep{
						{Action: protocol.ActionPickup, Node: from, Empty: true},
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
				d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
				d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
					OrderUUID: uuid, OrderType: OrderTypeRetrieveEmpty, PayloadCode: "PART-X",
					DeliveryNode: lineNode.Name, SourceNode: c.src.Name, Quantity: 1,
				})
				o = dispatchSimpleViaScanner(t, d, db, uuid)
			}
			got := ppFetchClaimed(o, c.partialID, c.emptyID)
			t.Logf("PIN %-50s %s (status %s, cause %q)", r.name, got, o.Status, o.QueueCause)
			if got != r.after {
				t.Errorf("%s: claimed %s, want %s (today %s)", r.name, got, r.after, r.today)
			}
		})
	}
}

// TestProducePartialsPin_OldestHeldComesBackFirst: two partials of X in its
// home, the one loaded LATER held LONGER. The refill takes the one held
// longest: the call-back order is when a partial entered its home, not when it
// was first loaded.
func TestProducePartialsPin_OldestHeldComesBackFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db)
	if err := db.CreatePayload(&payloads.Payload{Code: "PART-X", UOPCapacity: 10}); err != nil {
		t.Fatalf("PART-X payload: %v", err)
	}
	c := ppFetchGroups(t, db, "PPH", ppFetchRow{home: "source"})
	slot, err := db.GetNodeByName("PPH-SRC-S3")
	if err != nil {
		t.Fatalf("slot: %v", err)
	}
	// The first partial: loaded an hour ago, back in its home five minutes ago.
	// The second: loaded ten minutes ago, held for half an hour.
	second := makeLoaderBin(t, db, "PART-X", slot.ID, "PPH-PARTIAL-2", 3, time.Now().UTC().Add(-10*time.Minute))
	now := time.Now().UTC()
	for id, held := range map[int64]time.Time{c.partialID: now.Add(-5 * time.Minute), second.ID: now.Add(-30 * time.Minute)} {
		if _, err := db.DB.Exec(`UPDATE bins SET held_since = $1 WHERE id = $2`, held, id); err != nil {
			t.Fatalf("held_since: %v", err)
		}
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "PPH-ORDER", OrderType: OrderTypeRetrieveEmpty, PayloadCode: "PART-X",
		DeliveryNode: lineNode.Name, SourceNode: c.src.Name, Quantity: 1,
	})
	o := dispatchSimpleViaScanner(t, d, db, "PPH-ORDER")
	if o.BinID == nil || *o.BinID != second.ID {
		t.Errorf("the refill claimed %v, want the partial held longest (%d), not the one loaded first (%d)",
			o.BinID, second.ID, c.partialID)
	}
}
