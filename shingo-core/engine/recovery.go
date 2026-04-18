package engine

func (e *Engine) ReapplyOrderCompletion(orderID int64, actor string) error {
	return e.recovery.ReapplyOrderCompletion(orderID, actor)
}

func (e *Engine) ReleaseTerminalBinClaim(binID int64, actor string) error {
	return e.recovery.ReleaseTerminalBinClaim(binID, actor)
}

// Note: the higher-level ReleaseStagedBin(binID, actor) shortcut used to live
// here but had no callers — handlers go through h.engine.Recovery() for the
// recovery flow, and the Stage 1 refactor needed the 1-arg store delegate on
// *Engine (see engine_db_methods.go) to replace db.ReleaseStagedBin(binID) in
// handlers_bins.go. Keeping both under one name isn't possible, so the
// unused shortcut was removed; call e.Recovery().ReleaseStagedBin(binID, actor)
// when you want the full recovery flow.

func (e *Engine) CancelStuckOrder(orderID int64, actor string) error {
	return e.recovery.CancelStuckOrder(orderID, actor)
}
