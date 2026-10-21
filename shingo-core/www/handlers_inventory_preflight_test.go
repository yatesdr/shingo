//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// handlers_inventory_preflight_test.go — POST /api/inventory/preflight
// (apiInventoryPreflight over InventoryService.PreflightAvailability), the read
// the Edge asks before a changeover and before a REQUEST, pinned at the door:
// its two refusals, the empty answer and a mixed one. Which bins count as free
// and which as present is service's TestInventoryPreflight_*. A read writes
// nothing.
func TestPreflightDoor_Arms(t *testing.T) {
	t.Parallel()
	runDoorArms(t, func(h *Handlers) http.HandlerFunc { return h.apiInventoryPreflight }, "/api/inventory/preflight", []doorArm{
		{name: "an undecodable body is refused",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return "{", nil },
			answer:  refusedWith(http.StatusBadRequest, "decode request: unexpected EOF")},
		{name: "an empty payload code is a server failure",
			arrange: func(*testing.T, *doorFix) (any, []int64) {
				return map[string]any{"station": "edge.pf", "payloads": []string{"PART-A", ""}}, nil
			},
			answer: refusedWith(http.StatusInternalServerError, "preflight: preflight: empty payload code in request")},
		{name: "no payloads asked answers empty lists and a null available",
			arrange: func(*testing.T, *doorFix) (any, []int64) { return map[string]any{"station": "edge.pf"}, nil },
			answer: func(*testing.T, *doorFix) (int, string) {
				return http.StatusOK, "{\"missing\":[],\"absent\":[],\"available\":null}\n"
			}},
		{name: "free, held and absent payloads, in the order asked",
			arrange: func(t *testing.T, f *doorFix) (any, []int64) {
				free := testdb.CreateBinAtNode(t, f.db, "PART-A", f.sd.StorageNode.ID, "PF-FREE")
				testutil.MustNoErr(t, f.db.CreatePayload(&payloads.Payload{Code: "PF-HELD", UOPCapacity: 10}), "PF-HELD")
				held := testdb.CreateBinAtNode(t, f.db, "PF-HELD", f.sd.StorageNode.ID, "PF-HELD-BIN")
				o := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
				testdb.ClaimBinForTest(t, f.db, held.ID, o.ID)
				return map[string]any{"station": "edge.pf", "payloads": []string{"PF-ABSENT", "PF-HELD", "PART-A"}},
					[]int64{free.ID, held.ID}
			},
			answer: func(*testing.T, *doorFix) (int, string) {
				return http.StatusOK, `{"missing":["PF-ABSENT","PF-HELD"],"absent":["PF-ABSENT"],"available":[` +
					`{"payload_code":"PF-ABSENT","bin_count":0},{"payload_code":"PF-HELD","bin_count":0},` +
					`{"payload_code":"PART-A","bin_count":1}]}` + "\n"
			}},
	})
}
