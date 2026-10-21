package service

// level_changed.go — the hook a maintained group's level save calls after it
// commits (SetMaintainLevel, SetMaintainLevelAndLayerWants,
// RemoveMaintainLevel), so the Edges shown that group's line are pushed it
// instead of waiting for their next heartbeat.

// SetLevelChangedFunc installs the hook. Set once, through
// Engine.SetLevelChangedFunc. Atomic because the composition root sets it
// after the engine is built, while HTTP handlers may already be writing.
// Unset: nothing is pushed.
func (s *NodeService) SetLevelChangedFunc(fn func(groupNodeID int64)) {
	s.levelChanged.Store(&fn)
}

func (s *NodeService) notifyLevelChanged(groupNodeID int64) {
	if fn := s.levelChanged.Load(); fn != nil && *fn != nil {
		(*fn)(groupNodeID)
	}
}
