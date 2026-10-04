//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
)

// A STAGED BIN STAYS STAGED. A bin delivered to a node that is not a storage
// slot — a line, a staging spot, a kept spare's spot — is staged, and a staged
// bin is not sourceable. Staging used to expire after staging.ttl, at which
// point the bin became available and any line's plant-wide pull could take it
// off the node it was delivered to; for a keep-staged spot that is the spare
// leaving its line. Staging no longer expires, whatever staging.ttl says.
func TestStaging_ABinDeliveredToALinesideNodeNeverExpires(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{0, time.Millisecond} {
		t.Run(fmt.Sprintf("ttl=%s", ttl), func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			storageNode, lineNode, bp := setupTestData(t, db)
			createTestBinAtNode(t, db, bp.Code, storageNode.ID, fmt.Sprintf("BIN-NOEXP-%d", ttl))
			sim := simulator.New()
			eng := newUnstartedEngineWith(t, db, sim, func(c *config.Config) { c.Staging.TTL = ttl })
			eng.Start()
			t.Cleanup(eng.Stop)
			d := eng.Dispatcher()
			env := testEnvelope()

			uuid := fmt.Sprintf("noexp-%d", ttl)
			d.HandleOrderRequest(env, &protocol.OrderRequest{
				OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve, PayloadCode: bp.Code,
				DeliveryNode: lineNode.Name, Quantity: 1,
			})
			order, err := db.GetOrderByUUID(uuid)
			testutil.MustNoErr(t, err, "the retrieve")
			sim.DriveSimpleLifecycle(order.VendorOrderID)
			d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: uuid, ReceiptType: "confirmed", FinalCount: 1})

			time.Sleep(5 * time.Millisecond) // past any expiry a 1ms ttl would have stamped
			bin, err := db.GetBin(*order.BinID)
			testutil.MustNoErr(t, err, "the delivered bin")
			if bin.Status != "staged" {
				t.Fatalf("bin at the line is %q, want staged", bin.Status)
			}
			if bin.StagedExpiresAt != nil {
				t.Errorf("the staged bin carries an expiry %v with staging.ttl=%s: staging never expires",
					*bin.StagedExpiresAt, ttl)
			}
		})
	}
}
