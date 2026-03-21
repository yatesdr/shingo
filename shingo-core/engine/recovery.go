package engine

func (e *Engine) ReapplyOrderCompletion(orderID int64, actor string) error {
	return e.recovery.ReapplyOrderCompletion(orderID, actor)
}

func (e *Engine) ReleaseTerminalBinClaim(binID int64, actor string) error {
	return e.recovery.ReleaseTerminalBinClaim(binID, actor)
}

func (e *Engine) ReleaseStagedBin(binID int64, actor string) error {
	return e.recovery.ReleaseStagedBin(binID, actor)
}

func (e *Engine) CancelStuckOrder(orderID int64, actor string) error {
	return e.recovery.CancelStuckOrder(orderID, actor)
}
