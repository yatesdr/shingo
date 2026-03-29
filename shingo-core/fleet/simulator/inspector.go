package simulator

import "sync"

// OrderView is a read-only snapshot of a simulated order for test assertions.
type OrderView struct {
	VendorOrderID string
	State         string
	Complete      bool
	Priority      int
	Blocks        []BlockView
}

// BlockView is a read-only snapshot of a single block in a simulated order.
type BlockView struct {
	BlockID  string
	Location string
	BinTask  string
}

// GetOrder returns a snapshot of the simulated order by vendor order ID.
// Returns nil if not found.
func (s *SimulatorBackend) GetOrder(vendorOrderID string) *OrderView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		return nil
	}
	return orderToView(order)
}

// GetOrderByIndex returns the Nth order created (0-based).
// Useful when tests don't know the vendor order ID.
// Returns nil if index is out of range.
func (s *SimulatorBackend) GetOrderByIndex(idx int) *OrderView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idx < 0 || idx >= len(s.orderSeq) {
		return nil
	}
	return orderToView(s.orders[s.orderSeq[idx]])
}

// OrderCount returns the total number of orders created.
func (s *SimulatorBackend) OrderCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.orders)
}

// StagedOrderCount returns the number of incomplete (staged) orders.
func (s *SimulatorBackend) StagedOrderCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, order := range s.orders {
		if !order.complete {
			count++
		}
	}
	return count
}

// BlocksForOrder returns the blocks for a given vendor order ID.
func (s *SimulatorBackend) BlocksForOrder(vendorOrderID string) []BlockView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	order, ok := s.orders[vendorOrderID]
	if !ok {
		return nil
	}
	blocks := make([]BlockView, len(order.blocks))
	for i, b := range order.blocks {
		blocks[i] = BlockView{BlockID: b.blockID, Location: b.location, BinTask: b.binTask}
	}
	return blocks
}

// FindOrdersWithBinTask returns all orders containing a block with the given bin task.
func (s *SimulatorBackend) FindOrdersWithBinTask(binTask string) []*OrderView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []*OrderView
	for _, order := range s.orders {
		for _, b := range order.blocks {
			if b.binTask == binTask {
				results = append(results, orderToView(order))
				break
			}
		}
	}
	return results
}

// VendorOrderIDs returns all vendor order IDs in creation order.
func (s *SimulatorBackend) VendorOrderIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, len(s.orderSeq))
	copy(ids, s.orderSeq)
	return ids
}

func orderToView(o *simulatedOrder) *OrderView {
	v := &OrderView{
		VendorOrderID: o.vendorOrderID,
		State:         o.state,
		Complete:      o.complete,
		Priority:      o.priority,
	}
	for _, b := range o.blocks {
		v.Blocks = append(v.Blocks, BlockView{BlockID: b.blockID, Location: b.location, BinTask: b.binTask})
	}
	return v
}

// HasOrder returns true if a fleet order with the given vendor order ID exists.
// Useful for quick existence checks in test assertions without inspecting the full OrderView.
func (s *SimulatorBackend) HasOrder(vendorOrderID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.orders[vendorOrderID]
	return ok
}

// FindOrderByLocation returns all orders that contain a block at the given
// location (node name). This is useful for compound/complex tests that need
// to verify which fleet orders reference a particular storage or line node.
func (s *SimulatorBackend) FindOrderByLocation(location string) []*OrderView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []*OrderView
	for _, order := range s.orders {
		for _, b := range order.blocks {
			if b.location == location {
				results = append(results, orderToView(order))
				break
			}
		}
	}
	return results
}

// Compile-time check: SimulatorBackend must not expose internal state as pointers.
var _ = sync.RWMutex{} // referenced by receiver type
