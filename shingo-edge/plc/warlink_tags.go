package plc

import "context"

// All five delegate methods take m.mu.RLock() for the duration of the
// underlying call. ReplaceClient (manager.go:99-103) swaps m.wl under
// m.mu.Lock(); without the read-lock the swap races every concurrent
// caller — the CATID monitor alone reaches EnableTagPublishing on a
// 500ms tick (engine/plc_catid_monitor.go:154). ManagedPLC's own mp.mu
// is not involved here — the swap is on the Manager-level client pointer.

// ReadTagValue returns the current value of a single PLC tag via a live
// WarLink read. Delegates to the underlying WarlinkClient; kept as a
// Manager method so callers don't reach through to the client directly.
// Tag must exist on the PLC or WarLink returns 404.
//
// Named ReadTagValue (not ReadTag) to avoid collision with the cache-based
// ReadTag(plcName, tagName) on this same type.
func (m *Manager) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	wl := m.client()
	if wl == nil {
		return nil, nil
	}
	return wl.ReadTagValue(ctx, plcName, tagName)
}

// WriteTagValue writes a value to a PLC tag via a live WarLink write.
// Tag must be marked writable: true in WarLink config (HTTP 403 otherwise).
// PLC must be connected (HTTP 503 otherwise). Integer values auto-convert
// to the tag's data type.
func (m *Manager) WriteTagValue(ctx context.Context, plcName, tagName string, value any) error {
	wl := m.client()
	if wl == nil {
		return nil
	}
	return wl.WriteTagValue(ctx, plcName, tagName, value)
}

// EnableTagPublishing tells WarLink to start publishing a tag.
//
// The client is read under m.mu and called outside it, as are the two calls
// below: each is an HTTP round trip of up to its context's deadline, and a
// reconcile queued for the write lock behind a held read lock stalls the poll
// and the CATID tick for that long.
func (m *Manager) EnableTagPublishing(ctx context.Context, plcName, tagName string) error {
	wl := m.client()
	if wl == nil {
		return nil
	}
	return wl.SetTagPublishing(ctx, plcName, tagName, true)
}

// client returns the WarLink client under the read lock, to be called outside it.
func (m *Manager) client() WarlinkClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.wl
}

// DisableTagPublishing tells WarLink to stop publishing a tag.
func (m *Manager) DisableTagPublishing(ctx context.Context, plcName, tagName string) error {
	wl := m.client()
	if wl == nil {
		return nil
	}
	return wl.SetTagPublishing(ctx, plcName, tagName, false)
}

// FetchAllTags retrieves ALL tags (published and unpublished) from WarLink.
func (m *Manager) FetchAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
	wl := m.client()
	if wl == nil {
		return nil, nil
	}
	return wl.ListAllTags(ctx, plcName)
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
