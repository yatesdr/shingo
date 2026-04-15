package plc

import "context"

// ReadTag returns the current value of a single PLC tag. Delegates to the
// underlying WarlinkClient; kept as a Manager method so callers don't
// reach through to the client directly. Tag must exist on the PLC or
// WarLink returns 404.
func (m *Manager) ReadTag(ctx context.Context, plcName, tagName string) (interface{}, error) {
	return m.wl.ReadTagValue(ctx, plcName, tagName)
}

// WriteTag writes a value to a PLC tag. Tag must be marked writable: true
// in WarLink config (HTTP 403 otherwise). PLC must be connected (HTTP 503
// otherwise). Integer values auto-convert to the tag's data type.
func (m *Manager) WriteTag(ctx context.Context, plcName, tagName string, value interface{}) error {
	return m.wl.WriteTagValue(ctx, plcName, tagName, value)
}

// EnableTagPublishing tells WarLink to start publishing a tag.
func (m *Manager) EnableTagPublishing(ctx context.Context, plcName, tagName string) error {
	return m.wl.SetTagPublishing(ctx, plcName, tagName, true)
}

// DisableTagPublishing tells WarLink to stop publishing a tag.
func (m *Manager) DisableTagPublishing(ctx context.Context, plcName, tagName string) error {
	return m.wl.SetTagPublishing(ctx, plcName, tagName, false)
}

// FetchAllTags retrieves ALL tags (published and unpublished) from WarLink.
func (m *Manager) FetchAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
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
