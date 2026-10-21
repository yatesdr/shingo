package plc

// curtain_watch.go — the boards' half of the FG light-curtain cache.
//
// A station view stamps each curtained tile's CurtainOK from this manager's
// cache at build time (www stampViewCurtainState). The board does not poll, so
// a curtain that opens or closes must say so: the view registers every curtain
// tag it reads (WatchCurtainTag), and a cache write that changes one of their
// readings emits EmitCurtainChanged, which the engine turns into the boards'
// nudge. Every other tag — the counters move on every poll — is not watched,
// and a repeated reading is not news.
//
// The set only grows: it holds what some view has drawn since the process
// started, a handful of tags. A tag no view reads any more costs a nudge only
// when it changes.

// curtainKey is the watch set's key for one tag.
func curtainKey(plcName, tagName string) string { return plcName + "\x00" + tagName }

// WatchCurtainTag adds one curtain tag to the watch set. Lock-free: called on
// every view build, and a no-op after the first.
func (m *Manager) WatchCurtainTag(plcName, tagName string) {
	key := curtainKey(plcName, tagName)
	for {
		old := m.curtainWatch.Load()
		if old != nil && (*old)[key] {
			return
		}
		next := make(map[string]bool, 1)
		if old != nil {
			for k := range *old {
				next[k] = true
			}
		}
		next[key] = true
		if m.curtainWatch.CompareAndSwap(old, &next) {
			return
		}
	}
}

// curtainReading is what the curtain stamp reads of one cached tag: whether
// ReadTag answers a value, and how CurtainBool reads it.
func curtainReading(tv TagValue, present bool) [3]bool {
	if !present || tv.Error != "" {
		return [3]bool{}
	}
	v, ok := CurtainBool(tv.Value)
	return [3]bool{true, ok, v}
}

// curtainMoved reports whether a watched tag of this PLC reads differently in
// after than in before. Called under the PLC's lock, with both maps.
func (m *Manager) curtainMoved(plcName string, before, after map[string]TagValue) bool {
	watch := m.curtainWatch.Load()
	if watch == nil {
		return false
	}
	prefix := plcName + "\x00"
	for key := range *watch {
		if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		tag := key[len(prefix):]
		b, bok := before[tag]
		a, aok := after[tag]
		if curtainReading(b, bok) != curtainReading(a, aok) {
			return true
		}
	}
	return false
}

// curtainTagMoved is curtainMoved for one tag's write.
func (m *Manager) curtainTagMoved(plcName, tagName string, before TagValue, present bool, after TagValue) bool {
	watch := m.curtainWatch.Load()
	if watch == nil || !(*watch)[curtainKey(plcName, tagName)] {
		return false
	}
	return curtainReading(before, present) != curtainReading(after, true)
}

// curtainChanged emits the change, outside every lock.
func (m *Manager) curtainChanged() {
	if m.emitter != nil {
		m.emitter.EmitCurtainChanged()
	}
}
