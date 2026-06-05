package plc

import "context"

// All five delegate methods take m.mu.RLock() for the duration of the
// underlying call. ReplaceClient (manager.go:99-103) swaps m.wl under
// m.mu.Lock(); without the read-lock the swap races every other
// per-second countgroup heartbeat tick. ManagedPLC's own mp.mu is not
// involved here — the swap is on the Manager-level client pointer.

// ReadTagValue returns the current value of a single PLC tag via a live
// WarLink read. Delegates to the underlying WarlinkClient; kept as a
// Manager method so callers don't reach through to the client directly.
// Tag must exist on the PLC or WarLink returns 404.
//
// Named ReadTagValue (not ReadTag) to avoid collision with the cache-based
// ReadTag(plcName, tagName) on this same type.
func (m *Manager) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wl == nil {
		return nil, nil
	}
	return m.wl.ReadTagValue(ctx, plcName, tagName)
}

// WriteTagValue writes a value to a PLC tag via a live WarLink write.
// Tag must be marked writable: true in WarLink config (HTTP 403 otherwise).
// PLC must be connected (HTTP 503 otherwise). Integer values auto-convert
// to the tag's data type.
func (m *Manager) WriteTagValue(ctx context.Context, plcName, tagName string, value any) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wl == nil {
		return nil
	}
	return m.wl.WriteTagValue(ctx, plcName, tagName, value)
}

// EnableTagPublishing tells WarLink to start publishing a tag.
func (m *Manager) EnableTagPublishing(ctx context.Context, plcName, tagName string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wl == nil {
		return nil
	}
	return m.wl.SetTagPublishing(ctx, plcName, tagName, true)
}

// DisableTagPublishing tells WarLink to stop publishing a tag.
func (m *Manager) DisableTagPublishing(ctx context.Context, plcName, tagName string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wl == nil {
		return nil
	}
	return m.wl.SetTagPublishing(ctx, plcName, tagName, false)
}

// FetchAllTags retrieves ALL tags (published and unpublished) from WarLink.
func (m *Manager) FetchAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.wl == nil {
		return nil, nil
	}
	return m.wl.ListAllTags(ctx, plcName)
}

// IsTagPublished checks whether a tag is currently in the local WarLink cache
// (i.e. it's already being published and polled).
func (m *Manager) IsTagPublished(plcName, tagName string) bool {
	m.mu.RLock()
	mp, ok := m.plcs[plcName]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	mp.mu.RLock()
	defer mp.mu.RUnlock()
	_, exists := mp.Values[tagName]
	return exists
}
