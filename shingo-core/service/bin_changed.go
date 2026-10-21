package service

// bin_changed.go — the hook a station door's bin write calls after it
// commits, so the Edges watching the bin are pushed its new row instead of
// waiting for their next heartbeat. The doors are the Edge's LOAD (LoadAtLine),
// count (BinService.RecordCount) and CLEAR (the engine's
// ClearForReuseStampAndBookDeparture, which calls its own copy of the hook).
// None of them emits a bin event on the bus.

// SetBinChangedFunc installs the hook. Set once, through
// Engine.SetBinChangedFunc. Atomic because the composition root sets it after
// the engine is built, while HTTP handlers may already be writing. Unset:
// nothing is pushed and the heartbeat carries the change.
func (s *BinManifestService) SetBinChangedFunc(fn func(binID int64)) {
	s.binChanged.Store(&fn)
}

func (s *BinManifestService) notifyBinChanged(binID int64) {
	if fn := s.binChanged.Load(); fn != nil && *fn != nil {
		(*fn)(binID)
	}
}

// NotifyBinChanged is the hook for a writer outside this service that bumped a
// bin's generation inside its own transaction (the UOP-delta applier's clear
// at zero and its rollback rebase): called after that transaction commits.
func (s *BinManifestService) NotifyBinChanged(binID int64) { s.notifyBinChanged(binID) }
