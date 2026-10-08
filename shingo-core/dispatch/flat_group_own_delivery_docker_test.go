//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestFlatGroupStore_TheAskersOwnDeliveryIsNotACompetitor: a store resolve into a
// group whose slots stand directly under it counted every live order naming a
// slot as its delivery_node, the asker included. An order that had chosen the
// group's only free slot on an earlier pass, and asked again (asIntent's re-choice
// at acquiring), was told the group had no slot: it refused itself and waited
// under ngrp-resolve with its own slot free. The lane branch already exempted the
// asker (FindStoreSlotInLaneExcluding); the flat branches now do the same, for
// like-kind and depth-first alike.
func TestFlatGroupStore_TheAskersOwnDeliveryIsNotACompetitor(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{binresolver.StoreLKND, binresolver.StoreDPTH} {
		t.Run(algo, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcherWithResolver(t, db)
			ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
			testutil.MustNoErr(t, err, "NGRP type")
			grp := &nodes.Node{Name: "FLATOWN-" + algo, IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
			testutil.MustNoErr(t, db.CreateNode(grp), "create the group")
			testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, binresolver.PropStoreAlgorithm, algo), "set the algorithm")
			full := &nodes.Node{Name: "FLATOWN-" + algo + "-A", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
			free := &nodes.Node{Name: "FLATOWN-" + algo + "-B", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
			testutil.MustNoErr(t, db.CreateNode(full), "create a slot")
			testutil.MustNoErr(t, db.CreateNode(free), "create a slot")
			testdb.CreateBinAtNode(t, db, sd.Payload.Code, full.ID, "FLATOWN-"+algo+"-OCC")

			asker := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = "flatown-" + algo
				o.OrderType = OrderTypeComplex
				o.PayloadCode = sd.Payload.Code
				o.DeliveryNode = free.Name // chosen on an earlier pass
				o.Status = protocol.StatusQueued
			})
			res, err := d.resolver.Resolve(grp, binresolver.ResolveModeStore, sd.Payload.Code,
				binresolver.UnknownBinType("test: no carrier stated"), digAskerFor(asker), nil)
			if err != nil || res == nil || res.Node == nil || res.Node.ID != free.ID {
				t.Fatalf("the asker that already names %s as its delivery was answered %+v (err %v): it counted "+
					"itself as the order headed there and refused its own slot", free.Name, res, err)
			}
		})
	}
}
