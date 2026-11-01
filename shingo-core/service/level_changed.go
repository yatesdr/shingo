package service

// level_changed.go — the hook a kanban save calls after it commits, when it changed
// what an Edge shown the group's line sees (KanbanSave.changesTheLine), so those
// Edges are pushed it instead of waiting for their next heartbeat.

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
