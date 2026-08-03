package engine

import "shingo/protocol"

func (e *Engine) StartupReconcile() error {
	return e.coreSync.StartupReconcile()
}

func (e *Engine) RequestOrderStatusSync() error {
	return e.coreSync.RequestOrderStatusSync()
}

func (e *Engine) HandleOrderStatusSnapshots(items []protocol.OrderStatusSnapshot) {
	e.coreSync.HandleOrderStatusSnapshots(items)
}

func (e *Engine) HandleUnlistedOrders(items []protocol.OrderProjection) {
	e.coreSync.HandleUnlistedOrders(items)
}
