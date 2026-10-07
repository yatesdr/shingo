package service

import (
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/orders"
)

// OrderService exposes the order-query surface used by handlers. The
// full order lifecycle (create, dispatch, complete, abort) lives on
// orders.Manager (`shingoedge/orders`); this service intentionally
// covers ONLY the read paths and the one operator-driven mutation
// (final count confirmation) that handlers reach through the engine.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type OrderService struct {
	db *store.DB
}

// NewOrderService constructs an OrderService wrapping the shared
// *store.DB.
func NewOrderService(db *store.DB) *OrderService {
	return &OrderService{db: db}
}

// Get returns one order by id.
func (s *OrderService) Get(id int64) (*orders.Order, error) {
	return s.db.GetOrder(id)
}

// ListActive returns every order in a non-terminal state across all
// processes.
func (s *OrderService) ListActive() ([]orders.Order, error) {
	return s.db.ListActiveOrders()
}

// ListActiveByProcess returns active orders scoped to one process.
func (s *OrderService) ListActiveByProcess(processID int64) ([]orders.Order, error) {
	return s.db.ListActiveOrdersByProcess(processID)
}

// ListActiveStrict returns only non-terminal orders — Core's "Active"
// tab predicate. Use this for the orders page Active tab; use ListActive
// for the operator HMI which carries 7-day recent history.
func (s *OrderService) ListActiveStrict() ([]orders.Order, error) {
	return s.db.ListActiveOrdersStrict()
}

// ListActiveStrictByProcess mirrors ListActiveStrict, scoped to one process.
func (s *OrderService) ListActiveStrictByProcess(processID int64) ([]orders.Order, error) {
	return s.db.ListActiveOrdersByProcessStrict(processID)
}

// ListAll returns every order across all processes (the "All" tab).
func (s *OrderService) ListAll() ([]orders.Order, error) {
	return s.db.ListOrders()
}

// ListAllByProcess returns every order scoped to one process (the "All"
// tab with a process filter applied).
func (s *OrderService) ListAllByProcess(processID int64) ([]orders.Order, error) {
	return s.db.ListOrdersByProcess(processID)
}

// ListPage returns one page of the orders history (the All tab and the status
// pills), newest first, with the total the filter matches. status "" is every
// status; processID 0 is every process.
func (s *OrderService) ListPage(status string, processID int64, limit, offset int) ([]orders.Order, int, error) {
	return s.db.ListOrdersPage(orders.PageQuery{Status: status, ProcessID: processID, Limit: limit, Offset: offset})
}

// WaitSince answers "how long has this order been waiting" for every order in
// the list that is still ACQUIRING its material, as the RFC3339 instant its
// current wait began. Keyed by order id; absent means no clock, which is the
// honest rendering for an order that is not waiting and for one whose row could
// not be read.
//
// The board already shows WHY a parked order is waiting — Core pushes a reason
// for the whole acquiring set and the Edge keeps one for the same set — and a
// cause with no duration beside it reads the same at forty seconds and at four
// hours. Those are different days on a line.
//
// The instant comes from this Edge's OWN order_history: it appends a row on
// every transition it applies, Core's sourcing pushes included, so nothing has
// to be added to the wire to answer this.
//
// ONE READ PER STATUS PRESENT, and the statuses come from the orders themselves
// rather than from a list — the acquiring set is spelled once, in
// protocol.IsAcquiring, and this is not going to be a second spelling.
func (s *OrderService) WaitSince(list []orders.Order) map[int64]string {
	byStatus := map[protocol.Status][]int64{}
	for _, o := range list {
		if protocol.IsAcquiring(o.Status) {
			byStatus[o.Status] = append(byStatus[o.Status], o.ID)
		}
	}
	if len(byStatus) == 0 {
		return nil
	}
	out := make(map[int64]string)
	for status, ids := range byStatus {
		times, err := s.db.LatestOrderHistoryTimesForStatus(ids, string(status))
		if err != nil {
			// The sentence still renders and simply has no duration under it.
			log.Printf("orders board: wait clock for %d %s order(s): %v", len(ids), status, err)
			continue
		}
		for id, at := range times {
			out[id] = at.UTC().Format(time.RFC3339)
		}
	}
	return out
}

// BinReturnLines answers "what became of the bin this cancelled order left on
// a robot" for every order in the list that has a bin_returns row — one line
// of display text, keyed by order id. Absent means no notice, which renders as
// nothing. One read for the whole list. The notice changes nothing about the
// order (store/bin_returns.go); this is display only.
func (s *OrderService) BinReturnLines(list []orders.Order) map[int64]string {
	if len(list) == 0 {
		return nil
	}
	uuids := make([]string, 0, len(list))
	for _, o := range list {
		uuids = append(uuids, o.UUID)
	}
	returns, err := s.db.BinReturnsForOrders(uuids)
	if err != nil {
		log.Printf("orders board: bin return lines for %d order(s): %v", len(uuids), err)
		return nil
	}
	if len(returns) == 0 {
		return nil
	}
	out := make(map[int64]string, len(returns))
	for _, o := range list {
		if r, ok := returns[o.UUID]; ok {
			if line := BinReturnLine(r); line != "" {
				out[o.ID] = line
			}
		}
	}
	return out
}

// BinReturnLine is the one sentence a bin-return notice prints as.
func BinReturnLine(r store.BinReturn) string {
	bin := "Bin"
	if r.BinLabel != "" {
		bin = "Bin " + r.BinLabel
	}
	switch r.State {
	case protocol.BinReturnReturning:
		if r.Destination != "" {
			return bin + " returning to " + r.Destination
		}
		return bin + " returning to storage"
	case protocol.BinReturnReturned:
		if r.Destination != "" {
			return bin + " returned to " + r.Destination
		}
		return bin + " returned to storage"
	case protocol.BinReturnHeld:
		if r.Reason != "" {
			return bin + " held on the robot: " + r.Reason
		}
		return bin + " held on the robot"
	}
	return ""
}
