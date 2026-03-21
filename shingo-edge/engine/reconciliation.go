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
