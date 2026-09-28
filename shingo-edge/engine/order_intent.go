// order_intent.go — the future action an order arms when it is dispatched,
// kept on the order row (orders.pending_intent, Edge v4).
//
// Two features arm one:
//   - Pull From Market (operator_window_pullback.go): when the pullback order
//     delivers to the loader window, auto-clear the bin there.
//   - Clear Loader Home (operator_home_consolidation.go): when Order A's robot
//     picks up the empty off the home, fire Order B (buffer partial -> home).
//
// These used to live in two maps on the Engine, and an Edge restart between
// arming and firing lost them: the pulled-back bin arrived with its count still
// on it, and the home was emptied with no partial ever brought in. The order
// row is now the ONLY place an intent lives — one source of truth, no map to
// disagree with it.
//
// NO RESTART RELOAD. The firing sites (wiring_delivered.go, handler_bin_picked_up.go)
// read the row when their event arrives, so after a restart the intent is simply
// still there. There is deliberately no preload pass on Engine start.

package engine

import (
	"encoding/json"
	"fmt"
	"log"
)

// Intent kinds. The strings are persisted; never rename one.
const (
	intentPullback      = "pullback"
	intentConsolidation = "consolidation"
)

// orderIntent is the JSON envelope in orders.pending_intent. NodeID is the
// pullback's loader-window process node; the rest is a consolidation's Order B.
type orderIntent struct {
	Kind              string `json:"kind"`
	NodeID            int64  `json:"node_id,omitempty"`
	BufferCoreName    string `json:"buffer_core_name,omitempty"`
	HomeCoreName      string `json:"home_core_name,omitempty"`
	HomeProcessNodeID int64  `json:"home_process_node_id,omitempty"`
	Payload           string `json:"payload,omitempty"`
}

func (it orderIntent) consolidation() homeConsolidation {
	return homeConsolidation{
		bufferCoreName:    it.BufferCoreName,
		homeCoreName:      it.HomeCoreName,
		homeProcessNodeID: it.HomeProcessNodeID,
		payload:           it.Payload,
	}
}

// decodeOrderIntent is strict: a malformed envelope or a kind this build does
// not know is an error, never a guess.
func decodeOrderIntent(raw string) (orderIntent, error) {
	var it orderIntent
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		return orderIntent{}, fmt.Errorf("malformed intent: %w", err)
	}
	switch it.Kind {
	case intentPullback, intentConsolidation:
		return it, nil
	default:
		return orderIntent{}, fmt.Errorf("unknown intent kind %q", it.Kind)
	}
}

// armOrderIntent persists the intent on the order row. The order is already
// dispatched when this runs, so a failed write is logged, not returned: the
// operator's action did happen, and what is lost is only the follow-up — the
// same outcome a restart used to cause, now named in the log.
func (e *Engine) armOrderIntent(orderID int64, it orderIntent) {
	b, err := json.Marshal(it)
	if err == nil {
		err = e.db.SetOrderPendingIntent(orderID, string(b))
	}
	if err != nil {
		log.Printf("order_intent: arm %s on order %d FAILED, the follow-up will not fire: %v", it.Kind, orderID, err)
	}
}

// takeOrderIntent returns the order's intent iff it is of the given kind and
// THIS call took it off the row (exactly once across replays and racers). An
// intent of another kind is left in place for its own site. An undecodable one
// is logged and dropped — never a crash, never retried on every event.
func (e *Engine) takeOrderIntent(orderID int64, kind string) (orderIntent, bool) {
	raw, err := e.db.GetOrderPendingIntent(orderID)
	if err != nil || raw == "" {
		return orderIntent{}, false
	}
	it, derr := decodeOrderIntent(raw)
	if derr != nil {
		log.Printf("order_intent: order %d: %v — dropped (raw=%q)", orderID, derr, raw)
		if _, err := e.db.TakeOrderPendingIntent(orderID, raw); err != nil {
			log.Printf("order_intent: drop on order %d: %v", orderID, err)
		}
		return orderIntent{}, false
	}
	if it.Kind != kind {
		return orderIntent{}, false
	}
	took, err := e.db.TakeOrderPendingIntent(orderID, raw)
	if err != nil {
		log.Printf("order_intent: take %s on order %d: %v", kind, orderID, err)
		return orderIntent{}, false
	}
	return it, took
}
