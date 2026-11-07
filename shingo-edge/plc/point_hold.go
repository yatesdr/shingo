package plc

// SetPointHold installs what the poll asks of each point it read before it
// writes anything: true leaves the point's count where it stands this pass,
// so a later pass counts the strokes (the engine's re-point catch-up, which
// waits for the new slot's part identity). Set once at the composition root;
// read by the poll goroutine.
func (m *Manager) SetPointHold(fn func(plcName, tagName string) bool) {
	m.pointHold.Store(&fn)
}

func (m *Manager) pointHeld(plcName, tagName string) bool {
	if fn := m.pointHold.Load(); fn != nil {
		return (*fn)(plcName, tagName)
	}
	return false
}
