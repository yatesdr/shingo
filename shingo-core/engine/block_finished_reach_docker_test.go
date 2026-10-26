//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/seerrds"
	"shingocore/internal/testdb"
	"shingocore/rds"
	"shingocore/store"
)

// EVERY BLOCK THE DISPATCHER SENDS REACHES THE BLOCK SUBSCRIBER AS FINISHED.
//
// The production path end to end: the dispatcher's blocks go out through the
// SEER adapter to a vendor that only answers HTTP, the rds poller diffs the
// vendor's block states (diffBlockStates), the bridge puts EventBlockCompleted
// on the bus, and handleBlockCompleted records each block it is handed as a
// BLOCK_FINISHED mission event. The vendor here jumps: it reports a whole
// segment, or a whole order, FINISHED inside one poll gap, the poller never
// having seen those blocks running. The set of blocks with a BLOCK_FINISHED row
// is then the set the vendor was sent, block for block.

// stubVendor is the SEER RDS order surface, and only it: it keeps what /setOrder
// and /addBlocks send and answers /orderDetails with the state the test drives.
type stubVendor struct {
	mu     sync.Mutex
	orders map[string]*rds.OrderDetail
}

func newStubVendor(t *testing.T) (*stubVendor, string) {
	t.Helper()
	v := &stubVendor{orders: map[string]*rds.OrderDetail{}}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	return v, srv.URL
}

func (v *stubVendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	reply := map[string]any{"code": 0, "msg": "ok"}
	switch {
	case r.URL.Path == "/setOrder":
		var req rds.SetOrderRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		o := &rds.OrderDetail{ID: req.ID, Vehicle: "AMR-01", State: rds.StateCreated, Complete: req.Complete}
		o.Blocks = appendStubBlocks(o.Blocks, req.Blocks)
		v.orders[req.ID] = o
	case r.URL.Path == "/addBlocks":
		var req rds.AddBlocksRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if o := v.orders[req.ID]; o != nil {
			o.Blocks = appendStubBlocks(o.Blocks, req.Blocks)
			o.Complete = req.Complete
		}
	case strings.HasPrefix(r.URL.Path, "/orderDetails/"):
		o := v.orders[strings.TrimPrefix(r.URL.Path, "/orderDetails/")]
		if o == nil {
			reply = map[string]any{"code": 60000, "msg": "no such order"}
			break
		}
		raw, err := json.Marshal(o)
		if err == nil {
			err = json.Unmarshal(raw, &reply)
		}
		if err != nil {
			reply = map[string]any{"code": 50000, "msg": err.Error()}
			break
		}
		reply["code"], reply["msg"] = 0, "ok"
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func appendStubBlocks(to []rds.BlockDetail, sent []rds.Block) []rds.BlockDetail {
	for _, b := range sent {
		to = append(to, rds.BlockDetail{BlockID: b.BlockID, Location: b.Location, BinTask: b.BinTask, State: rds.StateCreated})
	}
	return to
}

// drive sets the vendor order's state and reports its first `finished` blocks
// FINISHED, the rest RUNNING.
func (v *stubVendor) drive(id string, state rds.OrderState, finished int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.orders[id]
	o.State = state
	for i := range o.Blocks {
		o.Blocks[i].State = rds.StateRunning
		if i < finished {
			o.Blocks[i].State = rds.StateFinished
		}
	}
}

// sent is every block the vendor was handed for the order, "location binTask"
// by block id.
func (v *stubVendor) sent(id string) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]string{}
	for _, b := range v.orders[id].Blocks {
		out[b.BlockID] = b.Location + " " + b.BinTask
	}
	return out
}

// subscriberSaw is every block handleBlockCompleted recorded for the order, by
// block id, with how many times it was recorded.
func subscriberSaw(t *testing.T, db *store.DB, orderID int64) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT blocks_json FROM mission_events WHERE order_id = $1 AND new_state = $2`,
		orderID, BlockLegState)
	testutil.MustNoErr(t, err, "read block legs")
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var raw string
		testutil.MustNoErr(t, rows.Scan(&raw), "scan block leg")
		var legs []blockLeg
		testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &legs), "decode block leg")
		for _, l := range legs {
			out[l.BlockID]++
		}
	}
	return out
}

// untilStatus waits for the poller to carry the order to a status.
func untilStatus(t *testing.T, db *store.DB, uuid string, want protocol.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if o, err := db.GetOrderByUUID(uuid); err == nil && o != nil && o.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	o, err := db.GetOrderByUUID(uuid)
	t.Fatalf("order %s never reached %s through the poller (now %v, %v)", uuid, want, o, err)
}

// assertEverySentBlockSeen: each block the vendor was sent has exactly one
// BLOCK_FINISHED row, and no row names a block it was not sent.
func assertEverySentBlockSeen(t *testing.T, sent map[string]string, saw map[string]int) {
	t.Helper()
	var ids []string
	for id := range sent {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if saw[id] != 1 {
			t.Errorf("block %s (%s): the subscriber recorded it %d times, want once", id, sent[id], saw[id])
		}
	}
	for id := range saw {
		if _, ok := sent[id]; !ok {
			t.Errorf("the subscriber recorded block %s, which the vendor was never sent", id)
		}
	}
	t.Logf("%d blocks sent, %d recorded: %v", len(sent), len(saw), ids)
}

func TestBlockFinished_EveryBlockSentReachesTheSubscriber(t *testing.T) {
	t.Parallel()

	t.Run("a plain order's advanced load sequence, the order finished inside one poll gap", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storageNode, lineNode, bp := setupTestData(t, db)
		createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REACH-SEQ")
		mustExec(t, db, `UPDATE payloads SET advanced_load_sequence = 'Child cart interlock' WHERE code = $1`, bp.Code)
		vendor, url := newStubVendor(t)
		eng := newTestEngine(t, db, seerrds.New(seerrds.Config{BaseURL: url, Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond}))

		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: "reach-seq", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: bp.Code,
			DeliveryNode: lineNode.Name, Quantity: 1,
		})
		order := testdb.RequireOrderStatus(t, db, "reach-seq", dispatch.StatusDispatched)
		sent := vendor.sent(order.VendorOrderID)
		want := []string{"Go_AP1", "Spin_90", "load", "Spin_inverse_90"}
		for i, task := range want {
			id := fmt.Sprintf("%s-b1", order.VendorOrderID)
			if i > 0 {
				id = fmt.Sprintf("%s-%d", id, i)
			}
			if sent[id] != storageNode.Name+" "+task {
				t.Fatalf("fixture: block %s is %q, want the load sequence's %s at %s (sent %v)", id, sent[id], task, storageNode.Name, sent)
			}
		}
		if len(sent) != 5 {
			t.Fatalf("fixture: %d blocks sent, want the four of the load sequence and the drop (%v)", len(sent), sent)
		}

		vendor.drive(order.VendorOrderID, rds.StateFinished, len(sent))
		untilStatus(t, db, "reach-seq", dispatch.StatusDelivered)
		assertEverySentBlockSeen(t, sent, subscriberSaw(t, db, order.ID))
	})

	// A bare wait sends no block; a wait at a node sends one (BinTask Wait).
	// Either way the create stops at the wait and the release sends the rest.
	waits := []struct {
		name        string
		wait        protocol.ComplexOrderStep
		before, all int
	}{
		{"a complex plan with a bare wait, each segment finished inside one poll gap",
			protocol.ComplexOrderStep{Action: protocol.ActionWait}, 2, 4},
		{"a complex plan with a wait at the line, each segment finished inside one poll gap",
			protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: "LINE1-IN"}, 3, 5},
	}
	for i, c := range waits {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			storageNode, lineNode, bp := setupTestData(t, db)
			if c.wait.Node != "" && c.wait.Node != lineNode.Name {
				t.Fatalf("fixture: the line is %s, not %s", lineNode.Name, c.wait.Node)
			}
			createTestBinAtNode(t, db, bp.Code, storageNode.ID, fmt.Sprintf("BIN-REACH-WAIT-%d", i))
			vendor, url := newStubVendor(t)
			eng := newTestEngine(t, db, seerrds.New(seerrds.Config{BaseURL: url, Timeout: 2 * time.Second, PollInterval: 10 * time.Millisecond}))
			d := eng.Dispatcher()
			env := testEnvelope()
			uuid := fmt.Sprintf("reach-wait-%d", i)

			d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: bp.Code, Quantity: 1,
				Steps: []protocol.ComplexOrderStep{
					{Action: protocol.ActionPickup, Node: storageNode.Name},
					{Action: protocol.ActionDropoff, Node: lineNode.Name},
					c.wait,
					{Action: protocol.ActionPickup, Node: lineNode.Name},
					{Action: protocol.ActionDropoff, Node: storageNode.Name},
				},
			})
			order := testdb.RequireOrderStatus(t, db, uuid, dispatch.StatusDispatched)
			if n := len(vendor.sent(order.VendorOrderID)); n != c.before {
				t.Fatalf("fixture: the create sent %d blocks, want %d", n, c.before)
			}

			vendor.drive(order.VendorOrderID, rds.StateRunning, 0)
			untilStatus(t, db, uuid, dispatch.StatusInTransit)
			vendor.drive(order.VendorOrderID, rds.StateWaiting, c.before)
			untilStatus(t, db, uuid, dispatch.StatusStaged)

			d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: uuid})
			sent := vendor.sent(order.VendorOrderID)
			if len(sent) != c.all {
				t.Fatalf("fixture: %d blocks sent after the release, want %d (%v)", len(sent), c.all, sent)
			}
			vendor.drive(order.VendorOrderID, rds.StateFinished, len(sent))
			untilStatus(t, db, uuid, dispatch.StatusDelivered)
			assertEverySentBlockSeen(t, sent, subscriberSaw(t, db, order.ID))
		})
	}
}
